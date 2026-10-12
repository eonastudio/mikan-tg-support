package srv

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"tg-support/internal/ai"
	"tg-support/internal/bot"
	"tg-support/internal/cfgw"
	"tg-support/internal/evfeed"
	"tg-support/internal/meta"
	"tg-support/internal/store"
	"tg-support/internal/telegram"
	"tg-support/internal/tickets"
)

// PanelInfo is the cached answer of GET /panel/v2/self (the status page shows whose
// grant the addon runs under).
type PanelInfo struct {
	ID          string   `json:"id,omitempty"`
	Version     string   `json:"version,omitempty"`
	Permissions []string `json:"permissions,omitempty"`
	LastAt      int64    `json:"last_at,omitempty"`
	LastError   string   `json:"last_error,omitempty"`
}

// RPC dispatches the calls of the addon's admin pages. Every answer is JSON; the
// HTTP status is the frame's status (the panel passes both through as they are).
//
// Trust model: a call arrives only through the panel, only from an admin's session
// (the panel checks both), and the addon checks its own Bearer token in front. What
// the addon must still not do is leak its secrets back to the frame: the config
// answers with flags where a token or a key would be, and the audit log of the panel
// sees methods and paths alone (no bodies), so no secret goes into a path either.
type RPC struct {
	st     *store.Store
	eng    *tickets.Engine
	watch  *cfgw.Watcher
	poller *bot.Poller
	feed   *evfeed.Feed
	panel  func() PanelInfo
	permEv *atomic.Bool
	log    *slog.Logger
	start  time.Time

	permEvents atomic.Bool
}

// RPCDeps wires the dispatcher.
type RPCDeps struct {
	Store  *store.Store
	Engine *tickets.Engine
	Watch  *cfgw.Watcher
	Poller *bot.Poller
	Feed   *evfeed.Feed
	Panel  func() PanelInfo
	Log    *slog.Logger
}

// NewRPC is the dispatcher.
func NewRPC(d RPCDeps) *RPC {
	r := &RPC{st: d.Store, eng: d.Engine, watch: d.Watch, poller: d.Poller, feed: d.Feed,
		panel: d.Panel, log: d.Log, start: time.Now()}
	r.permEv = &r.permEvents
	return r
}

// EventsPermitted is the flag the feed keeps (the status page shows it).
func (r *RPC) EventsPermitted() *atomic.Bool { return r.permEv }

// Handle is one call of the pages: (status, JSON body).
func (r *RPC) Handle(ctx context.Context, method, rawPath string, body json.RawMessage, lang string) (int, any) {
	u, err := url.Parse(rawPath)
	if err != nil {
		return errJSON(http.StatusBadRequest, "bad_request", "the path is not parseable")
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	q := u.Query()
	switch {
	case method == http.MethodGet && u.Path == "/bootstrap":
		return r.bootstrap(ctx, lang)
	case method == http.MethodGet && u.Path == "/settings":
		return r.settings(ctx)
	case method == http.MethodPut && u.Path == "/settings":
		return r.saveSettings(ctx, body)
	case method == http.MethodGet && u.Path == "/status":
		return r.status(ctx)
	case method == http.MethodGet && u.Path == "/tickets":
		return r.listTickets(ctx, q.Get("status"), q.Get("q"))
	case method == http.MethodPost && u.Path == "/test/bot":
		return r.testBot(ctx, body)
	case method == http.MethodPost && u.Path == "/test/ai":
		return r.testAI(ctx, body)
	case method == http.MethodPost && u.Path == "/maintenance/reindex":
		return r.reindex(ctx)
	case method == http.MethodPost && u.Path == "/maintenance/prune":
		return r.prune(ctx)
	}
	// /tickets/{id}[/…]
	if len(segs) == 2 && segs[0] == "tickets" {
		id, ok := ticketID(segs[1])
		if !ok {
			return errJSON(http.StatusBadRequest, "bad_request", "the ticket id is a number")
		}
		switch method {
		case http.MethodGet:
			return r.ticket(ctx, id)
		case http.MethodDelete:
			return r.deleteTicket(ctx, id)
		}
		return errJSON(http.StatusMethodNotAllowed, "method_not_allowed", "use GET or DELETE")
	}
	if len(segs) == 3 && segs[0] == "tickets" {
		id, ok := ticketID(segs[1])
		if !ok {
			return errJSON(http.StatusBadRequest, "bad_request", "the ticket id is a number")
		}
		switch segs[2] {
		case "reply":
			if method != http.MethodPost {
				return errJSON(http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
			}
			return r.reply(ctx, id, body)
		case "close":
			if method != http.MethodPost {
				return errJSON(http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
			}
			return r.close(ctx, id)
		case "reopen":
			if method != http.MethodPost {
				return errJSON(http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
			}
			return r.reopen(ctx, id)
		case "draft":
			if method != http.MethodPost {
				return errJSON(http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
			}
			return r.draft(ctx, id)
		case "ai-send":
			if method != http.MethodPost {
				return errJSON(http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
			}
			return r.aiSend(ctx, id, body)
		case "media":
			if method != http.MethodGet {
				return errJSON(http.StatusMethodNotAllowed, "method_not_allowed", "use GET")
			}
			return r.media(ctx, id, q.Get("seq"))
		}
	}
	return errJSON(http.StatusNotFound, "not_found", "no such call")
}

func ticketID(s string) (int64, bool) {
	if s == "" || len(s) > 19 || strings.Trim(s, "0123456789") != "" {
		return 0, false
	}
	id, err := strconv.ParseInt(s, 10, 64)
	return id, err == nil && id > 0
}

func errJSON(status int, code, msg string) (int, any) {
	return status, apiError{Code: code, Message: msg}
}

// storeErr maps a store/panel error to the frame's status.
func storeErr(err error) (int, any) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return errJSON(http.StatusNotFound, "not_found", "no such ticket")
	case errors.Is(err, context.DeadlineExceeded):
		return errJSON(http.StatusGatewayTimeout, "timeout", "the call took too long")
	}
	return errJSON(http.StatusBadGateway, "store_error", safeErr(err))
}

// safeErr is an error's text for the page: no URLs, no tokens (a transport error
// carries the address it failed on).
func safeErr(err error) string {
	s := err.Error()
	if len(s) > 300 {
		s = s[:300]
	}
	// Drop anything that looks like a bearer token or a bot token in a URL.
	if i := strings.Index(s, "/bot"); i >= 0 {
		if j := strings.Index(s[i+4:], "/"); j >= 0 {
			s = s[:i+4] + "<token>" + s[i+4+j:]
		}
	}
	return s
}

// cfgOf is the live config or the frame's error.
func (r *RPC) cfgOf(ctx context.Context) (store.Config, bool, int, any) {
	cfg, ok := r.watch.Get()
	if !ok {
		c2, err := r.watch.Reload(ctx)
		if err != nil {
			st, body := errJSON(http.StatusServiceUnavailable, "panel_unavailable",
				"the addon cannot read its settings from the panel yet")
			return store.Config{}, false, st, body
		}
		cfg = c2
	}
	return cfg, true, 0, nil
}

// bootstrap is the frame's first call: who the addon is, what is configured, how the
// bot and the feed are doing — everything the page draws around the ticket list.
func (r *RPC) bootstrap(ctx context.Context, lang string) (int, any) {
	cfg, ok, st, body := r.cfgOf(ctx)
	if !ok {
		return st, body
	}
	heads, err := r.st.ListTickets(ctx, "")
	if err != nil {
		return storeErr(err)
	}
	open, closed, unread := 0, 0, 0
	for _, h := range heads {
		if h.Status == store.StatusOpen {
			open++
			unread += h.Unread
		} else {
			closed++
		}
	}
	if lang != "ru" && lang != "en" {
		lang = "ru"
	}
	return http.StatusOK, map[string]any{
		"version": meta.Version,
		"lang":    lang,
		"config":  cfg.View(),
		"bot":     r.poller.Status(cfg),
		"feed":    r.feed.Status(r.permEvents.Load()),
		"ai":      r.eng.AIStats(),
		"panel":   r.panelInfo(),
		"counts":  map[string]int{"open": open, "closed": closed, "unread": unread, "indexed": len(heads)},
	}
}

func (r *RPC) panelInfo() PanelInfo {
	if r.panel == nil {
		return PanelInfo{}
	}
	return r.panel()
}

// listTickets is the index, filtered and searched.
func (r *RPC) listTickets(ctx context.Context, status, q string) (int, any) {
	switch status {
	case "", store.StatusOpen, store.StatusClosed:
	default:
		return errJSON(http.StatusBadRequest, "bad_request", "status is open, closed or empty")
	}
	heads, err := r.st.ListTickets(ctx, status)
	if err != nil {
		return storeErr(err)
	}
	if s := strings.ToLower(strings.TrimSpace(q)); s != "" {
		out := heads[:0]
		for _, h := range heads {
			if strings.Contains(strings.ToLower(h.Subject), s) ||
				strings.Contains(strings.ToLower(h.Username), s) ||
				strings.Contains(strings.ToLower(h.FirstName), s) ||
				strings.Contains(strconv.FormatInt(h.TgID, 10), s) {
				out = append(out, h)
			}
		}
		heads = out
	}
	return http.StatusOK, map[string]any{"tickets": heads}
}

// maxDialogMessages is what one dialog view carries: the panel passes at most 1 MiB
// to the frame, and a very long ticket must not break that (the tail is what the
// agent reads anyway; the rest stays in the KV).
const maxDialogMessages = 400

// ticket is one dialog with the user's profile beside it.
func (r *RPC) ticket(ctx context.Context, id int64) (int, any) {
	t, err := r.st.Ticket(ctx, id)
	if err != nil {
		return storeErr(err)
	}
	msgs, err := r.st.LastMessages(ctx, id, maxDialogMessages)
	if err != nil {
		return storeErr(err)
	}
	var profile *store.Profile
	if p, err := r.st.Profile(ctx, t.TgID); err == nil {
		profile = &p
	}
	// The agent has looked at it: the unread count goes (a CAS round on the ticket).
	if t.Unread > 0 {
		if t2, err := r.st.UpdateTicket(ctx, id, func(t *store.Ticket) error { t.Unread = 0; return nil }); err == nil {
			t = t2
		}
	}
	return http.StatusOK, map[string]any{"ticket": t, "messages": msgs, "profile": profile,
		"truncated": t.MsgCount > len(msgs)}
}

// reply sends the agent's text to the user.
func (r *RPC) reply(ctx context.Context, id int64, body json.RawMessage) (int, any) {
	var in struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return errJSON(http.StatusBadRequest, "bad_request", "the body is {text}")
	}
	if strings.TrimSpace(in.Text) == "" {
		return errJSON(http.StatusBadRequest, "empty_text", "the reply is empty")
	}
	if utf8.RuneCountInString(in.Text) > store.MaxTextRunes {
		return errJSON(http.StatusBadRequest, "too_long", "the reply is longer than 4096 characters")
	}
	t, err := r.eng.AgentReply(ctx, id, in.Text)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return storeErr(err)
		}
		return errJSON(http.StatusBadGateway, "send_failed", safeErr(err))
	}
	return http.StatusOK, map[string]any{"ticket": t}
}

func (r *RPC) close(ctx context.Context, id int64) (int, any) {
	t, err := r.eng.Close(ctx, id)
	if err != nil {
		return storeErr(err)
	}
	return http.StatusOK, map[string]any{"ticket": t}
}

func (r *RPC) reopen(ctx context.Context, id int64) (int, any) {
	t, err := r.eng.Reopen(ctx, id)
	if err != nil {
		return storeErr(err)
	}
	return http.StatusOK, map[string]any{"ticket": t}
}

func (r *RPC) deleteTicket(ctx context.Context, id int64) (int, any) {
	if _, err := r.st.Ticket(ctx, id); err != nil {
		return storeErr(err)
	}
	if err := r.st.DeleteTicket(ctx, id); err != nil {
		return storeErr(err)
	}
	return http.StatusOK, map[string]any{"ok": true}
}

// draft is the AI answer for the agent to edit or send (mode draft).
func (r *RPC) draft(ctx context.Context, id int64) (int, any) {
	if _, err := r.st.Ticket(ctx, id); err != nil {
		return storeErr(err)
	}
	text, err := r.eng.Draft(ctx, id)
	if err != nil {
		return errJSON(http.StatusBadGateway, "ai_failed", safeErr(err))
	}
	return http.StatusOK, map[string]any{"text": text}
}

// aiSend sends an AI answer to the user: the text the agent edited, or a fresh draft
// when the body carries none.
func (r *RPC) aiSend(ctx context.Context, id int64, body json.RawMessage) (int, any) {
	var in struct {
		Text string `json:"text"`
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &in); err != nil {
			return errJSON(http.StatusBadRequest, "bad_request", "the body is {text?}")
		}
	}
	text := strings.TrimSpace(in.Text)
	if text == "" {
		var err error
		if text, err = r.eng.Draft(ctx, id); err != nil {
			return errJSON(http.StatusBadGateway, "ai_failed", safeErr(err))
		}
	}
	t, err := r.eng.AgentSendAI(ctx, id, text)
	if err != nil {
		return errJSON(http.StatusBadGateway, "send_failed", safeErr(err))
	}
	return http.StatusOK, map[string]any{"ticket": t, "text": text}
}

// maxPreviewBytes is what a photo may weigh to be shown in the frame: the RPC answer
// goes through the panel's 1 MiB limit, and base64 grows it by a third.
const maxPreviewBytes = 700_000

// media is one photo of the dialog as a data URL (the frame may draw data: images).
func (r *RPC) media(ctx context.Context, id int64, seqStr string) (int, any) {
	cfg, ok, st, body := r.cfgOf(ctx)
	if !ok {
		return st, body
	}
	if cfg.BotToken == "" {
		return errJSON(http.StatusConflict, "bot_not_configured", "the bot token is not set")
	}
	seq, err := strconv.ParseInt(seqStr, 10, 64)
	if err != nil || seq <= 0 {
		return errJSON(http.StatusBadRequest, "bad_request", "seq is a message number")
	}
	msgs, err := r.st.Messages(ctx, id, seq-1, 1)
	if err != nil {
		return storeErr(err)
	}
	if len(msgs) == 0 || msgs[0].Seq != seq {
		return errJSON(http.StatusNotFound, "not_found", "no such message")
	}
	m := msgs[0]
	if m.FileID == "" {
		return errJSON(http.StatusNotFound, "not_found", "the message has no file")
	}
	mimeT := strings.ToLower(m.FileMime)
	if !strings.HasPrefix(mimeT, "image/") {
		return errJSON(http.StatusUnsupportedMediaType, "not_previewable", "only images are previewable")
	}
	switch mimeT {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
	default:
		mimeT = "image/jpeg"
	}
	if m.FileSize > maxPreviewBytes {
		return errJSON(http.StatusRequestEntityTooLarge, "too_large",
			fmt.Sprintf("the file is %d bytes; the preview limit is %d", m.FileSize, maxPreviewBytes))
	}
	cl := telegram.New(cfg.APIBase(), cfg.BotToken)
	fctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	f, err := cl.GetFile(fctx, m.FileID)
	if err != nil {
		return errJSON(http.StatusBadGateway, "telegram_failed", safeErr(err))
	}
	if f.FileSize > maxPreviewBytes {
		return errJSON(http.StatusRequestEntityTooLarge, "too_large", "the file is too large for a preview")
	}
	data, err := cl.DownloadFile(fctx, f.FilePath, f.FileSize)
	if err != nil {
		return errJSON(http.StatusBadGateway, "telegram_failed", safeErr(err))
	}
	return http.StatusOK, map[string]any{
		"data_url": "data:" + mimeT + ";base64," + base64.StdEncoding.EncodeToString(data),
		"mime":     mimeT,
		"size":     len(data),
	}
}

// settings is the config without its secrets.
func (r *RPC) settings(ctx context.Context) (int, any) {
	cfg, ok, st, body := r.cfgOf(ctx)
	if !ok {
		return st, body
	}
	return http.StatusOK, cfg.View()
}

// settingsIn is one PUT /settings: a nil field stays as it was, "" in a secret keeps
// the old value (the frame shows a filled secret as a placeholder, never its text).
type settingsIn struct {
	BotToken      *string `json:"bot_token"`
	TelegramAPI   *string `json:"telegram_api"`
	Greeting      *string `json:"greeting"`
	AIEnabled     *bool   `json:"ai_enabled"`
	AIBaseURL     *string `json:"ai_base_url"`
	AIAPIKey      *string `json:"ai_api_key"`
	AIModel       *string `json:"ai_model"`
	AIMode        *string `json:"ai_mode"`
	AIDelaySec    *int    `json:"ai_delay_sec"`
	AILang        *string `json:"ai_lang"`
	AISystem      *string `json:"ai_system"`
	AIKnowledge   *string `json:"ai_knowledge"`
	RetentionDays *int    `json:"retention_days"`
	ClearSecret   *string `json:"clear_secret"`
}

// botTokenShape is Telegram's own: <bot id>:<secret>. A loose check catches a paste
// of the wrong thing (a username, a URL) before it reaches Telegram.
func botTokenShape(s string) bool {
	i := strings.IndexByte(s, ':')
	if i <= 0 || i == len(s)-1 {
		return false
	}
	if strings.Trim(s[:i], "0123456789") != "" {
		return false
	}
	rest := s[i+1:]
	if len(rest) < 20 || len(rest) > 128 {
		return false
	}
	return strings.Trim(rest, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-") == ""
}

func (r *RPC) saveSettings(ctx context.Context, body json.RawMessage) (int, any) {
	var in settingsIn
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return errJSON(http.StatusBadRequest, "bad_request", "the body is not a valid settings object")
	}
	if in.BotToken != nil && *in.BotToken != "" && !botTokenShape(strings.TrimSpace(*in.BotToken)) {
		return errJSON(http.StatusBadRequest, "bad_token", "a bot token looks like 123456789:AA…; this does not")
	}
	if in.ClearSecret != nil {
		switch *in.ClearSecret {
		case "bot_token", "ai_api_key":
		default:
			return errJSON(http.StatusBadRequest, "bad_request", "clear_secret is bot_token or ai_api_key")
		}
	}
	saved, err := r.st.SaveConfig(ctx, func(c *store.Config) error {
		if in.BotToken != nil {
			c.SetBotToken(*in.BotToken)
		}
		if in.TelegramAPI != nil {
			c.TelegramAPI = strings.TrimSuffix(strings.TrimSpace(*in.TelegramAPI), "/")
		}
		if in.Greeting != nil {
			c.Greeting = strings.TrimSpace(*in.Greeting)
		}
		if in.AIEnabled != nil {
			c.AIEnabled = *in.AIEnabled
		}
		if in.AIBaseURL != nil {
			c.AIBaseURL = strings.TrimSuffix(strings.TrimSpace(*in.AIBaseURL), "/")
		}
		if in.AIAPIKey != nil {
			c.SetAIKey(*in.AIAPIKey)
		}
		if in.AIModel != nil {
			c.AIModel = strings.TrimSpace(*in.AIModel)
		}
		if in.AIMode != nil {
			c.AIMode = strings.TrimSpace(*in.AIMode)
		}
		if in.AIDelaySec != nil {
			c.AIDelaySec = *in.AIDelaySec
		}
		if in.AILang != nil {
			c.AILang = strings.TrimSpace(*in.AILang)
		}
		if in.AISystem != nil {
			c.AISystem = *in.AISystem
		}
		if in.AIKnowledge != nil {
			c.AIKnowledge = *in.AIKnowledge
		}
		if in.RetentionDays != nil {
			c.RetentionDays = *in.RetentionDays
		}
		if in.ClearSecret != nil {
			return c.ClearSecret(*in.ClearSecret)
		}
		return nil
	})
	if err != nil {
		return errJSON(http.StatusBadRequest, "bad_settings", safeErr(err))
	}
	// The watcher re-reads and tells the poller at once: a new token starts polling
	// without waiting for the next tick.
	if _, err := r.watch.Reload(ctx); err != nil {
		r.log.Warn("settings: the reload after a save failed", "err", err)
	}
	return http.StatusOK, saved.View()
}

// testBot checks a token (the saved one by default) with getMe, without saving it.
func (r *RPC) testBot(ctx context.Context, body json.RawMessage) (int, any) {
	var in struct {
		Token string `json:"token"`
		API   string `json:"api"`
	}
	if len(body) > 0 && json.Unmarshal(body, &in) != nil {
		return errJSON(http.StatusBadRequest, "bad_request", "the body is {token?, api?}")
	}
	cfg, ok, st, bd := r.cfgOf(ctx)
	if !ok {
		return st, bd
	}
	token := strings.TrimSpace(in.Token)
	if token == "" {
		token = cfg.BotToken
	}
	api := strings.TrimSpace(in.API)
	if api == "" {
		api = cfg.APIBase()
	}
	if token == "" {
		return http.StatusOK, map[string]any{"ok": false, "error": "no token to test"}
	}
	if !botTokenShape(token) {
		return http.StatusOK, map[string]any{"ok": false, "error": "the token has the wrong shape"}
	}
	tctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	me, err := telegram.New(api, token).GetMe(tctx)
	if err != nil {
		return http.StatusOK, map[string]any{"ok": false, "error": safeErr(err)}
	}
	return http.StatusOK, map[string]any{"ok": true, "username": "@" + me.Username, "name": me.FirstName, "id": me.ID}
}

// testAI asks the configured (or the typed) model for one word.
func (r *RPC) testAI(ctx context.Context, body json.RawMessage) (int, any) {
	var in struct {
		BaseURL string `json:"base_url"`
		APIKey  string `json:"api_key"`
		Model   string `json:"model"`
	}
	if len(body) > 0 && json.Unmarshal(body, &in) != nil {
		return errJSON(http.StatusBadRequest, "bad_request", "the body is {base_url?, api_key?, model?}")
	}
	cfg, ok, st, bd := r.cfgOf(ctx)
	if !ok {
		return st, bd
	}
	base, key, model := strings.TrimSpace(in.BaseURL), strings.TrimSpace(in.APIKey), strings.TrimSpace(in.Model)
	if base == "" {
		base = cfg.AIBaseURL
	}
	if key == "" {
		key = cfg.AIAPIKey
	}
	if model == "" {
		model = cfg.AIModel
	}
	if base == "" || key == "" || model == "" {
		return http.StatusOK, map[string]any{"ok": false, "error": "base_url, api_key and model are all needed"}
	}
	tctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	text, used, ms, err := ai.New(base, key, model, 25*time.Second).Complete(tctx, []ai.ChatMessage{
		{Role: "system", Content: "You are a connectivity test. Answer with the single word: OK"},
		{Role: "user", Content: "ping"},
	})
	if err != nil {
		return http.StatusOK, map[string]any{"ok": false, "error": safeErr(err)}
	}
	return http.StatusOK, map[string]any{"ok": true, "reply": text, "model": used, "ms": ms}
}

func (r *RPC) reindex(ctx context.Context) (int, any) {
	n, err := r.st.RebuildIndex(ctx)
	if err != nil {
		return storeErr(err)
	}
	return http.StatusOK, map[string]any{"heads": n}
}

func (r *RPC) prune(ctx context.Context) (int, any) {
	cfg, ok, st, bd := r.cfgOf(ctx)
	if !ok {
		return st, bd
	}
	n, err := r.st.Prune(ctx, cfg.Retention())
	if err != nil {
		return storeErr(err)
	}
	return http.StatusOK, map[string]any{"removed": n}
}

// status is the health page: the bot, the feed, the AI, the panel's grant, the KV.
func (r *RPC) status(ctx context.Context) (int, any) {
	cfg, ok, st, bd := r.cfgOf(ctx)
	if !ok {
		return st, bd
	}
	ticketsN, err1 := r.st.CountKeys(ctx, "t/", 5000)
	usersN, err2 := r.st.CountKeys(ctx, "u/", 5000)
	kvErr := ""
	if err1 != nil || err2 != nil {
		kvErr = safeErr(errors.Join(err1, err2))
	}
	return http.StatusOK, map[string]any{
		"version":  meta.Version,
		"uptime_s": int64(time.Since(r.start).Seconds()),
		"bot":      r.poller.Status(cfg),
		"feed":     r.feed.Status(r.permEvents.Load()),
		"ai":       r.eng.AIStats(),
		"panel":    r.panelInfo(),
		"kv":       map[string]any{"tickets": ticketsN, "profiles": usersN, "error": kvErr},
	}
}
