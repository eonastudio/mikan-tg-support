package srv

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"tg-support/internal/ai"
	"tg-support/internal/bot"
	"tg-support/internal/cfgw"
	"tg-support/internal/evfeed"
	"tg-support/internal/meta"
	"tg-support/internal/panelapi"
	"tg-support/internal/paneltest"
	"tg-support/internal/store"
	"tg-support/internal/tickets"
)

const adapterToken = "adapter-token"

type recSender struct {
	mu   sync.Mutex
	sent []string
}

func (r *recSender) SendText(_ context.Context, _ int64, text string) error {
	r.mu.Lock()
	r.sent = append(r.sent, text)
	r.mu.Unlock()
	return nil
}
func (r *recSender) SendChatAction(context.Context, int64) {}
func (r *recSender) all() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.sent, "|")
}

type stubLLM struct{}

func (stubLLM) Complete(context.Context, []ai.ChatMessage) (string, string, int64, error) {
	return "draft text", "stub", 5, nil
}

type stack struct {
	srv    *httptest.Server
	st     *store.Store
	eng    *tickets.Engine
	send   *recSender
	watch  *cfgw.Watcher
	fake   *paneltest.Fake
	poller *bot.Poller
}

func newStack(t *testing.T) *stack {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fake := paneltest.New("panel-token")
	t.Cleanup(fake.Close)
	kv := panelapi.New(fake.URL(), "panel-token", 10*time.Second)
	st := store.New(kv, time.Now)
	watch := cfgw.New(st, log)
	if _, err := watch.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	send := &recSender{}
	eng := tickets.New(st, func(context.Context) (store.Config, error) {
		c, ok := watch.Get()
		if !ok {
			return watch.Reload(context.Background())
		}
		return c, nil
	},
		func(c store.Config) tickets.Sender {
			if c.BotToken == "" {
				return nil
			}
			return send
		},
		func(c store.Config) tickets.LLM {
			if !c.AIReady() {
				return nil
			}
			return stubLLM{}
		}, log)
	poller := bot.New(st, eng, log)
	feed := evfeed.New(kv, st, log)
	rpc := NewRPC(RPCDeps{Store: st, Engine: eng, Watch: watch, Poller: poller, Feed: feed,
		Panel: func() PanelInfo { return PanelInfo{ID: meta.ID, Permissions: meta.Permissions} }, Log: log})
	s, err := New(Deps{
		Info: Info{ID: meta.ID, Protocol: meta.Protocol, Version: meta.Version, Kind: meta.Kind, Mode: meta.Mode,
			Name: meta.Name, Description: meta.Description, Permissions: meta.Permissions,
			UI: UI{Entry: "/ui/index.html", Title: meta.Title}, Settings: []any{}, Help: meta.Help},
		Pages: testPages(), RPC: rpc, Log: log, Token: adapterToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(s.Handler())
	t.Cleanup(hs.Close)
	return &stack{srv: hs, st: st, eng: eng, send: send, watch: watch, fake: fake, poller: poller}
}

// testPages is a tiny stand-in of the embedded web/: the same file names.
func testPages() fstest.MapFS {
	return fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte(`<!DOCTYPE html><html><body>tickets</body></html>`)},
		"app.js":     &fstest.MapFile{Data: []byte(`"use strict";console.log(1);`)},
		"style.css":  &fstest.MapFile{Data: []byte(`body{margin:0}`)},
	}
}

func (s *stack) do(method, path, bearer string, body []byte) (*http.Response, []byte) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, s.srv.URL+path, rd)
	if err != nil {
		panic(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := s.srv.Client().Do(req)
	if err != nil {
		panic(err)
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	resp.Body.Close()
	return resp, data
}

// rpcCall forwards one call of the pages as the panel does: the addon's own HTTP
// status is the frame's status, and its JSON body is the frame's body.
func (s *stack) rpcCall(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var raw json.RawMessage
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		raw = b
	}
	call, _ := json.Marshal(map[string]any{"method": method, "path": path, "body": raw,
		"admin": map[string]string{"lang": "ru"}})
	resp, data := s.do(http.MethodPost, "/v2/ui/rpc", adapterToken, call)
	var out map[string]any
	if len(data) > 0 {
		_ = json.Unmarshal(data, &out)
	}
	return resp.StatusCode, out
}

// num is a JSON number field or 0 when the field is absent (omitempty drops zeros).
func num(m map[string]any, k string) float64 {
	if v, ok := m[k].(float64); ok {
		return v
	}
	return 0
}

func TestAuthCoversEverything(t *testing.T) {
	s := newStack(t)
	for _, p := range []string{"/v2/info", "/ui/index.html", "/v2/health", "/ui/app.js"} {
		resp, _ := s.do(http.MethodGet, p, "", nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s without a token: %d", p, resp.StatusCode)
		}
		resp, _ = s.do(http.MethodGet, p, "wrong-token", nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s with a wrong token: %d", p, resp.StatusCode)
		}
	}
	call := []byte(`{"method":"GET","path":"/bootstrap"}`)
	resp, _ := s.do(http.MethodPost, "/v2/ui/rpc", "", call)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("rpc without a token: %d", resp.StatusCode)
	}
}

func TestInfoAndHealth(t *testing.T) {
	s := newStack(t)
	resp, data := s.do(http.MethodGet, "/v2/info", adapterToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("info: %d", resp.StatusCode)
	}
	var info Info
	if err := json.Unmarshal(data, &info); err != nil {
		t.Fatalf("info: %v (%s)", err, data)
	}
	if info.ID != "tg-support" || info.Protocol != 2 || info.Kind != "ui" || info.Mode != "http" {
		t.Fatalf("info: %+v", info)
	}
	if info.UI.Entry != "/ui/index.html" || info.UI.Title["ru"] == "" || info.UI.Title["en"] == "" {
		t.Fatalf("ui: %+v", info.UI)
	}
	resp, _ = s.do(http.MethodGet, "/v2/health", adapterToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health: %d", resp.StatusCode)
	}
}

func TestUIFiles(t *testing.T) {
	s := newStack(t)
	resp, data := s.do(http.MethodGet, "/ui/index.html", adapterToken, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(data), "tickets") {
		t.Fatalf("index: %d %s", resp.StatusCode, data)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("the content type is %q", ct)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag")
	}
	req, _ := http.NewRequest(http.MethodGet, s.srv.URL+"/ui/index.html", nil)
	req.Header.Set("Authorization", "Bearer "+adapterToken)
	req.Header.Set("If-None-Match", etag)
	r2, err := s.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != http.StatusNotModified {
		t.Fatalf("the revalidation answered %d", r2.StatusCode)
	}
	resp, _ = s.do(http.MethodGet, "/ui/app.js", adapterToken, nil)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/javascript") {
		t.Fatalf("app.js: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	resp, _ = s.do(http.MethodGet, "/ui/none.txt", adapterToken, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a missing file: %d", resp.StatusCode)
	}
	// Traversal shapes end in a 404 (or a redirect the client follows to one), never
	// in a file from outside the pages.
	resp, body := s.do(http.MethodGet, "/ui/%2e%2e%2fmain.go", adapterToken, nil)
	if resp.StatusCode == http.StatusOK || strings.Contains(string(body), "package main") {
		t.Fatalf("the traversal got through: %d", resp.StatusCode)
	}
}

func TestRPCBootstrapAndSecrets(t *testing.T) {
	s := newStack(t)
	// A config with secrets, saved through the store like the page would.
	if _, err := s.st.SaveConfig(context.Background(), func(c *store.Config) error {
		c.SetBotToken("123456789:AAFakeTokenForTestsOnly_xxxxxxxxxxx")
		c.SetAIKey("ai-secret-key")
		c.AIEnabled = true
		c.AIMode = store.AIDraft
		c.AIBaseURL = "https://llm.test/v1"
		c.AIModel = "m"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.watch.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, body := s.rpcCall(t, http.MethodGet, "/bootstrap", nil)
	if status != http.StatusOK {
		t.Fatalf("bootstrap: %d %+v", status, body)
	}
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), "123456789:AAFakeTokenForTestsOnly_xxxxxxxxxxx") || strings.Contains(string(raw), "ai-secret-key") {
		t.Fatalf("the bootstrap leaks secrets: %s", raw)
	}
	cfgView := body["config"].(map[string]any)
	if cfgView["has_bot_token"] != true || cfgView["has_ai_key"] != true {
		t.Fatalf("the flags: %+v", cfgView)
	}
	if body["lang"] != "ru" || body["version"] == nil {
		t.Fatalf("bootstrap: %+v", body)
	}
}

func TestRPCTicketFlow(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	if _, err := s.st.SaveConfig(ctx, func(c *store.Config) error {
		c.SetBotToken("123456789:AAFakeTokenForTestsOnly_xxxxxxxxxxx")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.watch.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	// A user writes; the engine opens the ticket.
	if _, err := s.eng.HandleUserMessage(ctx, tickets.Inbound{TgID: 5, ChatID: 5, TgMsgID: 1,
		Username: "ann", Lang: "ru", Kind: store.KindText, Text: "не работает"}); err != nil {
		t.Fatal(err)
	}
	status, body := s.rpcCall(t, http.MethodGet, "/tickets?status=open", nil)
	if status != http.StatusOK {
		t.Fatalf("list: %d %+v", status, body)
	}
	list := body["tickets"].([]any)
	if len(list) != 1 {
		t.Fatalf("the list holds %d", len(list))
	}
	head := list[0].(map[string]any)
	if head["subject"] != "не работает" || num(head, "unread") != 1 {
		t.Fatalf("head: %+v", head)
	}
	// The dialog: messages and the (absent) profile.
	status, body = s.rpcCall(t, http.MethodGet, "/tickets/1", nil)
	if status != http.StatusOK || body["profile"] != nil {
		t.Fatalf("dialog: %d %+v", status, body)
	}
	msgs := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages: %+v", msgs)
	}
	// Opening it clears the unread count.
	status, body = s.rpcCall(t, http.MethodGet, "/tickets?status=open", nil)
	if status != http.StatusOK || num(body["tickets"].([]any)[0].(map[string]any), "unread") != 0 {
		t.Fatalf("the unread count survived the view: %+v", body)
	}
	// The agent replies; the user's side receives it.
	status, _ = s.rpcCall(t, http.MethodPost, "/tickets/1/reply", map[string]string{"text": "проверьте ноду"})
	if status != http.StatusOK {
		t.Fatalf("reply: %d", status)
	}
	if !strings.Contains(s.send.all(), "проверьте ноду") {
		t.Fatalf("the user got %q", s.send.all())
	}
	// An empty reply is refused.
	if status, _ = s.rpcCall(t, http.MethodPost, "/tickets/1/reply", map[string]string{"text": "  "}); status != http.StatusBadRequest {
		t.Fatalf("the empty reply: %d", status)
	}
	// Close, then the list of closed has it.
	if status, _ = s.rpcCall(t, http.MethodPost, "/tickets/1/close", nil); status != http.StatusOK {
		t.Fatalf("close: %d", status)
	}
	status, body = s.rpcCall(t, http.MethodGet, "/tickets?status=closed", nil)
	if status != http.StatusOK || len(body["tickets"].([]any)) != 1 {
		t.Fatalf("closed list: %d %+v", status, body)
	}
	// Delete removes it for good.
	if status, _ = s.rpcCall(t, http.MethodDelete, "/tickets/1", nil); status != http.StatusOK {
		t.Fatalf("delete: %d", status)
	}
	if status, _ = s.rpcCall(t, http.MethodGet, "/tickets/1", nil); status != http.StatusNotFound {
		t.Fatalf("the deleted ticket answers %d", status)
	}
}

func TestRPCDraftFlow(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	if _, err := s.st.SaveConfig(ctx, func(c *store.Config) error {
		c.SetBotToken("123456789:AAFakeTokenForTestsOnly_xxxxxxxxxxx")
		c.AIEnabled = true
		c.AIMode = store.AIDraft
		c.AIBaseURL = "https://llm.test/v1"
		c.SetAIKey("k")
		c.AIModel = "m"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.watch.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.eng.HandleUserMessage(ctx, tickets.Inbound{TgID: 6, ChatID: 6, TgMsgID: 1,
		Lang: "ru", Kind: store.KindText, Text: "вопрос"}); err != nil {
		t.Fatal(err)
	}
	status, body := s.rpcCall(t, http.MethodPost, "/tickets/1/draft", nil)
	if status != http.StatusOK || body["text"] != "draft text" {
		t.Fatalf("draft: %d %+v", status, body)
	}
	if strings.Contains(s.send.all(), "draft text") {
		t.Fatal("the draft reached the user")
	}
	// ai-send with an edited text sends exactly it.
	status, _ = s.rpcCall(t, http.MethodPost, "/tickets/1/ai-send", map[string]string{"text": "draft text (edited)"})
	if status != http.StatusOK {
		t.Fatalf("ai-send: %d", status)
	}
	if !strings.Contains(s.send.all(), "draft text (edited)") {
		t.Fatalf("the user got %q", s.send.all())
	}
}

func TestRPCSettingsGuards(t *testing.T) {
	s := newStack(t)
	// A wrong-shaped token is refused.
	status, body := s.rpcCall(t, http.MethodPut, "/settings", map[string]any{"bot_token": "just words"})
	if status != http.StatusBadRequest {
		t.Fatalf("the bad token: %d %+v", status, body)
	}
	// A right-shaped one saves; the answer masks it.
	status, body = s.rpcCall(t, http.MethodPut, "/settings", map[string]any{"bot_token": "123456789:AAFakeTokenForTestsOnly_xxxxxxxxxxx"})
	if status != http.StatusOK {
		t.Fatalf("the save: %d %+v", status, body)
	}
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), "123456789:AAFakeTokenForTestsOnly_xxxxxxxxxxx") {
		t.Fatalf("the answer leaks the token: %s", raw)
	}
	if body["has_bot_token"] != true {
		t.Fatalf("the flag: %+v", body)
	}
	// An empty token keeps the old one.
	status, _ = s.rpcCall(t, http.MethodPut, "/settings", map[string]any{"bot_token": ""})
	cfg, _ := s.st.LoadConfig(context.Background())
	if status != http.StatusOK || cfg.BotToken != "123456789:AAFakeTokenForTestsOnly_xxxxxxxxxxx" {
		t.Fatalf("the empty save: %d %q", status, cfg.BotToken)
	}
	// clear_secret drops it.
	status, _ = s.rpcCall(t, http.MethodPut, "/settings", map[string]any{"clear_secret": "bot_token"})
	cfg, _ = s.st.LoadConfig(context.Background())
	if status != http.StatusOK || cfg.BotToken != "" {
		t.Fatalf("the clear: %d %q", status, cfg.BotToken)
	}
	// A bad mode and a bad delay are refused.
	if status, _ = s.rpcCall(t, http.MethodPut, "/settings", map[string]any{"ai_mode": "wild"}); status != http.StatusBadRequest {
		t.Fatalf("the bad mode: %d", status)
	}
	if status, _ = s.rpcCall(t, http.MethodPut, "/settings", map[string]any{"ai_delay_sec": -5}); status != http.StatusBadRequest {
		t.Fatalf("the bad delay: %d", status)
	}
	// An unknown field is refused (a typo must not pass silently).
	if status, _ = s.rpcCall(t, http.MethodPut, "/settings", map[string]any{"what": 1}); status != http.StatusBadRequest {
		t.Fatalf("the unknown field: %d", status)
	}
	// A broken body is refused.
	resp, _ := s.do(http.MethodPost, "/v2/ui/rpc", adapterToken,
		[]byte(`{"method":"PUT","path":"/settings","body":{`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("the broken body: %d", resp.StatusCode)
	}
}

func TestRPCValidation(t *testing.T) {
	s := newStack(t)
	if status, _ := s.rpcCall(t, http.MethodGet, "/tickets/abc", nil); status != http.StatusBadRequest {
		t.Fatalf("a word for an id: %d", status)
	}
	if status, _ := s.rpcCall(t, http.MethodGet, "/tickets/-3", nil); status != http.StatusBadRequest {
		t.Fatalf("a negative id: %d", status)
	}
	if status, _ := s.rpcCall(t, http.MethodGet, "/nowhere", nil); status != http.StatusNotFound {
		t.Fatalf("an unknown path: %d", status)
	}
	if status, _ := s.rpcCall(t, http.MethodPost, "/tickets", nil); status != http.StatusNotFound {
		t.Fatalf("a collection POST: %d", status)
	}
	// The forwarding layer itself refuses what the panel would refuse.
	call := []byte(`{"method":"PATCH","path":"/bootstrap"}`)
	resp, data := s.do(http.MethodPost, "/v2/ui/rpc", adapterToken, call)
	var env struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(data, &env)
	if resp.StatusCode != http.StatusBadRequest || env.Code != "bad_request" {
		t.Fatalf("PATCH: %d %s", resp.StatusCode, data)
	}
	call = []byte(`{"method":"GET","path":"no-slash"}`)
	resp, _ = s.do(http.MethodPost, "/v2/ui/rpc", adapterToken, call)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a path without a slash: %d", resp.StatusCode)
	}
}

func TestRPCMediaGuards(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	if _, err := s.st.SaveConfig(ctx, func(c *store.Config) error {
		c.SetBotToken("123456789:AAFakeTokenForTestsOnly_xxxxxxxxxxx")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.watch.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	// A ticket with an oversized photo and a document.
	if _, err := s.st.CreateTicket(ctx, store.NewTicket{TgID: 9, ChatID: 9},
		store.Message{Kind: store.KindPhoto, FileID: "f1", FileMime: "image/jpeg", FileSize: 5 << 20}); err != nil {
		t.Fatal(err)
	}
	if err := s.st.AppendMessage(ctx, 1, store.Message{From: store.FromUser, Kind: store.KindDocument,
		FileID: "f2", FileName: "log.txt", FileMime: "text/plain"}); err != nil {
		t.Fatal(err)
	}
	if status, _ := s.rpcCall(t, http.MethodGet, "/tickets/1/media?seq=1", nil); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("the oversize: %d", status)
	}
	if status, _ := s.rpcCall(t, http.MethodGet, "/tickets/1/media?seq=2", nil); status != http.StatusUnsupportedMediaType {
		t.Fatalf("the document: %d", status)
	}
	if status, _ := s.rpcCall(t, http.MethodGet, "/tickets/1/media?seq=99", nil); status != http.StatusNotFound {
		t.Fatalf("a missing seq: %d", status)
	}
	if status, _ := s.rpcCall(t, http.MethodGet, "/tickets/1/media?seq=x", nil); status != http.StatusBadRequest {
		t.Fatalf("a word for a seq: %d", status)
	}
}

func TestRPCMaintenance(t *testing.T) {
	s := newStack(t)
	status, body := s.rpcCall(t, http.MethodPost, "/maintenance/reindex", nil)
	if status != http.StatusOK || body["heads"].(float64) != 0 {
		t.Fatalf("reindex: %d %+v", status, body)
	}
	status, body = s.rpcCall(t, http.MethodPost, "/maintenance/prune", nil)
	if status != http.StatusOK || body["removed"].(float64) != 0 {
		t.Fatalf("prune: %d %+v", status, body)
	}
	status, body = s.rpcCall(t, http.MethodGet, "/status", nil)
	if status != http.StatusOK || body["version"] == nil || body["bot"] == nil || body["kv"] == nil {
		t.Fatalf("status: %d %+v", status, body)
	}
}

func TestTestBotWithoutToken(t *testing.T) {
	s := newStack(t)
	// No saved token, none in the body: an honest "no token", not a crash.
	status, body := s.rpcCall(t, http.MethodPost, "/test/bot", map[string]any{})
	if status != http.StatusOK || body["ok"] != false {
		t.Fatalf("test/bot: %d %+v", status, body)
	}
	// A wrong-shaped token never reaches the wire.
	status, body = s.rpcCall(t, http.MethodPost, "/test/bot", map[string]any{"token": "words"})
	if status != http.StatusOK || body["ok"] != false || !strings.Contains(body["error"].(string), "shape") {
		t.Fatalf("the shaped check: %d %+v", status, body)
	}
}
