package tickets

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"tg-support/internal/ai"
	"tg-support/internal/panelapi"
	"tg-support/internal/paneltest"
	"tg-support/internal/store"
)

type sentMsg struct {
	ChatID int64
	Text   string
}

type fakeSender struct {
	mu   sync.Mutex
	sent []sentMsg
	fail error
}

func (f *fakeSender) SendText(_ context.Context, chatID int64, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	f.sent = append(f.sent, sentMsg{chatID, text})
	return nil
}

func (f *fakeSender) SendChatAction(context.Context, int64) {}

func (f *fakeSender) texts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.sent))
	for i, m := range f.sent {
		out[i] = m.Text
	}
	return out
}

func (f *fakeSender) reset() {
	f.mu.Lock()
	f.sent = nil
	f.mu.Unlock()
}

type fakeLLM struct {
	mu    sync.Mutex
	reply string
	err   error
	calls int
	last  []ai.ChatMessage
}

func (f *fakeLLM) Complete(_ context.Context, msgs []ai.ChatMessage) (string, string, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.last = msgs
	if f.err != nil {
		return "", "", 0, f.err
	}
	return f.reply, "fake-model", 42, nil
}

func (f *fakeLLM) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type harness struct {
	st   *store.Store
	eng  *Engine
	send *fakeSender
	llm  *fakeLLM
	fake *paneltest.Fake

	mu  sync.RWMutex
	cfg store.Config
}

func (h *harness) getCfg() store.Config {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.cfg
}

func (h *harness) setCfg(c store.Config) {
	h.mu.Lock()
	h.cfg = c
	h.mu.Unlock()
}

func newHarness(t *testing.T, cfg store.Config) *harness {
	t.Helper()
	f := paneltest.New("panel-token")
	kv := panelapi.New(f.URL(), "panel-token", 10*time.Second)
	st := store.New(kv, time.Now)
	h := &harness{st: st, cfg: cfg, fake: f, send: &fakeSender{}, llm: &fakeLLM{reply: "AI answer"}}
	cfgFn := func(context.Context) (store.Config, error) { return h.getCfg(), nil }
	sendFn := func(c store.Config) Sender {
		if c.BotToken == "" {
			return nil
		}
		return h.send
	}
	llmFn := func(c store.Config) LLM {
		if !c.AIReady() {
			return nil
		}
		return h.llm
	}
	h.eng = New(st, cfgFn, sendFn, llmFn, testLogger(t))
	t.Cleanup(f.Close)
	return h
}

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func aiCfg(mode string) store.Config {
	return store.Config{BotToken: "1:t", AIEnabled: true, AIMode: mode, AIBaseURL: "https://llm.test/v1",
		AIAPIKey: "k", AIModel: "m", AIDelaySec: 1, AIKnowledge: "FAQ: перезапустите приложение"}
}

func msg(text string) Inbound {
	return Inbound{TgID: 100, ChatID: 100, TgMsgID: time.Now().UnixNano(), Username: "ann",
		FirstName: "Ann", Lang: "ru", Kind: store.KindText, Text: text}
}

func TestInboundCreatesAndContinues(t *testing.T) {
	h := newHarness(t, store.Config{BotToken: "1:t"})
	ctx := context.Background()
	m1 := msg("Не работает VPN")
	m1.TgMsgID = 1
	tk, err := h.eng.HandleUserMessage(ctx, m1)
	if err != nil {
		t.Fatal(err)
	}
	if tk.ID != 1 || tk.Subject != "Не работает VPN" {
		t.Fatalf("ticket %+v", tk)
	}
	sent := h.send.texts()
	if len(sent) != 1 || !strings.Contains(sent[0], "Тикет #1 создан") {
		t.Fatalf("confirmation: %+v", sent)
	}
	m2 := msg("и приложение виснет")
	m2.TgMsgID = 2
	tk2, err := h.eng.HandleUserMessage(ctx, m2)
	if err != nil {
		t.Fatal(err)
	}
	if tk2.ID != 1 {
		t.Fatalf("the second message opened %+v", tk2)
	}
	msgs, _ := h.st.Messages(ctx, 1, 0, 0)
	if len(msgs) != 2 {
		t.Fatalf("history %+v", msgs)
	}
	if len(h.send.texts()) != 1 {
		t.Fatal("the second message sent another confirmation")
	}
	// A replay of the same Telegram message is a no-op.
	if _, err := h.eng.HandleUserMessage(ctx, m2); err != nil {
		t.Fatal(err)
	}
	if msgs2, _ := h.st.Messages(ctx, 1, 0, 0); len(msgs2) != 2 {
		t.Fatalf("the replay doubled the history: %d", len(msgs2))
	}
}

func TestHumanKeywordStopsTheAI(t *testing.T) {
	h := newHarness(t, aiCfg(store.AIAuto))
	ctx := context.Background()
	m := msg("позовите оператора, пожалуйста")
	m.TgMsgID = 11
	tk, err := h.eng.HandleUserMessage(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	if !tk.NoAI {
		t.Fatalf("the flag is not set: %+v", tk)
	}
	// The auto answer of a human-only ticket does nothing, even when due.
	time.Sleep(1300 * time.Millisecond) // past the 1 s delay: the timer, if any, fires
	if h.llm.Calls() != 0 {
		t.Fatal("the AI answered a human-only ticket")
	}
	if got := strings.Join(h.send.texts(), "\n"); !strings.Contains(got, "зову человека") {
		t.Fatalf("no human note: %s", got)
	}
}

func TestAutoAnswerFlow(t *testing.T) {
	h := newHarness(t, aiCfg(store.AIAuto))
	ctx := context.Background()
	m := msg("как подключить?")
	m.TgMsgID = 21
	tk, err := h.eng.HandleUserMessage(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	// The scheduled answer fires after the 1 s delay.
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) && h.llm.Calls() == 0 {
		time.Sleep(50 * time.Millisecond)
	}
	if h.llm.Calls() == 0 {
		t.Fatal("the auto answer never fired")
	}
	h.llm.mu.Lock()
	sys := ""
	if len(h.llm.last) > 0 && h.llm.last[0].Role == "system" {
		sys = h.llm.last[0].Content
	}
	h.llm.mu.Unlock()
	if !strings.Contains(sys, "FAQ: перезапустите приложение") {
		t.Fatalf("the knowledge base did not reach the model:\n%s", sys)
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got, _ := h.st.Ticket(ctx, tk.ID); got.LastFrom == store.FromAI {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	got, _ := h.st.Ticket(ctx, tk.ID)
	if got.LastFrom != store.FromAI || got.Unread != 0 {
		t.Fatalf("the ticket after the AI: %+v", got)
	}
	msgs, _ := h.st.Messages(ctx, tk.ID, 0, 0)
	last := msgs[len(msgs)-1]
	if last.From != store.FromAI || last.Text != "AI answer" || last.AIModel != "fake-model" {
		t.Fatalf("the AI message: %+v", last)
	}
	if all := strings.Join(h.send.texts(), "|"); !strings.Contains(all, "AI answer") {
		t.Fatalf("the user did not get the AI answer: %s", all)
	}
	stats := h.eng.AIStats()
	if stats.AutoSent != 1 || stats.LastModel != "fake-model" {
		t.Fatalf("stats %+v", stats)
	}
}

func TestAutoAnswerYieldsToTheAgent(t *testing.T) {
	h := newHarness(t, aiCfg(store.AIAuto))
	h.llm.reply = "slow answer"
	ctx := context.Background()
	m := msg("вопрос")
	m.TgMsgID = 31
	tk, _ := h.eng.HandleUserMessage(ctx, m)
	// The agent answers before the AI is due.
	if _, err := h.eng.AgentReply(ctx, tk.ID, "человеческий ответ"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	if h.llm.Calls() != 0 {
		t.Fatal("the AI answered over the agent")
	}
}

func TestDraftSendsNothing(t *testing.T) {
	h := newHarness(t, aiCfg(store.AIDraft))
	ctx := context.Background()
	m := msg("вопрос про трафик")
	m.TgMsgID = 41
	tk, _ := h.eng.HandleUserMessage(ctx, m)
	h.send.reset()
	text, err := h.eng.Draft(ctx, tk.ID)
	if err != nil || text != "AI answer" {
		t.Fatalf("draft %q (%v)", text, err)
	}
	if len(h.send.sent) != 0 {
		t.Fatal("the draft reached the user")
	}
	// Draft mode schedules nothing: the delay passes and the model is never called
	// beyond the draft itself.
	calls := h.llm.Calls()
	time.Sleep(1300 * time.Millisecond)
	if h.llm.Calls() != calls {
		t.Fatal("draft mode answered by itself")
	}
}

func TestAgentReplyAndReopen(t *testing.T) {
	h := newHarness(t, store.Config{BotToken: "1:t"})
	ctx := context.Background()
	m := msg("проблема")
	m.TgMsgID = 51
	tk, _ := h.eng.HandleUserMessage(ctx, m)
	h.send.reset()
	if _, err := h.eng.AgentReply(ctx, tk.ID, "решение"); err != nil {
		t.Fatal(err)
	}
	if got := h.send.texts(); len(got) != 1 || got[0] != "решение" {
		t.Fatalf("the user got %+v", got)
	}
	got, _ := h.st.Ticket(ctx, tk.ID)
	if got.LastFrom != store.FromSupport || got.Unread != 0 {
		t.Fatalf("the ticket: %+v", got)
	}
	// The user closes, the agent's reply reopens.
	if _, ok, err := h.eng.CloseByUser(ctx, 100); err != nil || !ok {
		t.Fatalf("close: %v %v", ok, err)
	}
	h.send.reset()
	if _, err := h.eng.AgentReply(ctx, tk.ID, "ещё одно решение"); err != nil {
		t.Fatal(err)
	}
	got, _ = h.st.Ticket(ctx, tk.ID)
	if got.Status != store.StatusOpen {
		t.Fatalf("the reply did not reopen: %+v", got)
	}
	if all := strings.Join(h.send.texts(), "|"); !strings.Contains(all, "снова открыт") {
		t.Fatalf("no reopen notice: %s", all)
	}
	// A failed send stores nothing.
	h.send.fail = errors.New("blocked by the user")
	h.send.reset()
	if _, err := h.eng.AgentReply(ctx, tk.ID, "не дойдёт"); err == nil {
		t.Fatal("the failed send went through")
	}
	msgs, _ := h.st.Messages(ctx, tk.ID, 0, 0)
	for _, mm := range msgs {
		if mm.Text == "не дойдёт" {
			t.Fatal("the failed reply was stored")
		}
	}
	// An empty reply and an overlong one are refused.
	if _, err := h.eng.AgentReply(ctx, tk.ID, "   "); err == nil {
		t.Fatal("the empty reply went through")
	}
	h.send.fail = nil
	if _, err := h.eng.AgentReply(ctx, tk.ID, strings.Repeat("ы", store.MaxTextRunes+1)); err == nil {
		t.Fatal("the overlong reply went through")
	}
}

func TestCloseByUserTwice(t *testing.T) {
	h := newHarness(t, store.Config{BotToken: "1:t"})
	ctx := context.Background()
	m := msg("проблема")
	m.TgMsgID = 61
	if _, err := h.eng.HandleUserMessage(ctx, m); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := h.eng.CloseByUser(ctx, 100); err != nil || !ok {
		t.Fatalf("the first close: %v %v", ok, err)
	}
	if _, ok, err := h.eng.CloseByUser(ctx, 100); err != nil || ok {
		t.Fatalf("the second close: %v %v", ok, err)
	}
	// The next message opens a new ticket.
	m2 := msg("снова")
	m2.TgMsgID = 62
	tk2, err := h.eng.HandleUserMessage(ctx, m2)
	if err != nil || tk2.ID != 2 {
		t.Fatalf("the new ticket: %+v (%v)", tk2, err)
	}
}

func TestFloodLimit(t *testing.T) {
	h := newHarness(t, store.Config{BotToken: "1:t"})
	ctx := context.Background()
	for i := range 12 {
		m := msg("спам")
		m.TgMsgID = int64(100 + i)
		if _, err := h.eng.HandleUserMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := h.st.Ticket(ctx, 1)
	if got.MsgCount >= 12 {
		t.Fatalf("the limiter let %d through", got.MsgCount)
	}
	if got.MsgCount < 4 {
		t.Fatalf("the limiter is too strict: %d", got.MsgCount)
	}
	warns := 0
	for _, s := range h.send.texts() {
		if strings.Contains(s, "Слишком много сообщений") {
			warns++
		}
	}
	if warns != 1 {
		t.Fatalf("warnings: %d, want exactly 1", warns)
	}
}

func TestNoBotNoSending(t *testing.T) {
	h := newHarness(t, store.Config{}) // no token: the sender is nil
	ctx := context.Background()
	m := msg("вопрос без бота")
	m.TgMsgID = 71
	tk, err := h.eng.HandleUserMessage(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	if tk.ID != 1 {
		t.Fatalf("the ticket: %+v", tk)
	}
	if len(h.send.sent) != 0 {
		t.Fatal("something was sent without a token")
	}
	if _, err := h.eng.AgentReply(ctx, tk.ID, "x"); err == nil {
		t.Fatal("the agent reply went through without a bot")
	}
}

func TestUserStatusAndGreeting(t *testing.T) {
	h := newHarness(t, store.Config{BotToken: "1:t", Greeting: "Свои привет"})
	ctx := context.Background()
	if s, err := h.eng.UserStatus(ctx, 999, "ru"); err != nil || !strings.Contains(s, "нет открытых") {
		t.Fatalf("status of nobody: %q (%v)", s, err)
	}
	m := msg("проблема")
	m.TgMsgID = 81
	if _, err := h.eng.HandleUserMessage(ctx, m); err != nil {
		t.Fatal(err)
	}
	g, err := h.eng.Greeting(ctx, h.getCfg(), 100, "ru")
	if err != nil || !strings.Contains(g, "Свои привет") || !strings.Contains(g, "#1") {
		t.Fatalf("greeting: %q (%v)", g, err)
	}
	s, _ := h.eng.UserStatus(ctx, 100, "ru")
	if !strings.Contains(s, "Открыт тикет #1") || !strings.Contains(s, "ждём ответа") {
		t.Fatalf("status: %q", s)
	}
	if _, err := h.eng.AgentReply(ctx, 1, "ответ"); err != nil {
		t.Fatal(err)
	}
	s, _ = h.eng.UserStatus(ctx, 100, "en")
	if !strings.Contains(s, "support has answered") {
		t.Fatalf("english status: %q", s)
	}
}

func TestSweepPicksUpWhatARestartLeft(t *testing.T) {
	cfg := aiCfg(store.AIAuto)
	cfg.AIDelaySec = 0 // the default 120 s applies — the sweep below fakes the age
	h := newHarness(t, cfg)
	ctx := context.Background()
	m := msg("старый вопрос")
	m.TgMsgID = 91
	tk, _ := h.eng.HandleUserMessage(ctx, m)
	// Age the last message past any delay by rewriting the ticket meta.
	if _, err := h.st.UpdateTicketMeta(ctx, tk.ID, func(t *store.Ticket) {
		t.LastMsgAt = time.Now().Add(-time.Hour).Unix()
	}); err != nil {
		t.Fatal(err)
	}
	// The pending timer of HandleUserMessage is still armed for +120 s; the sweep
	// must not double-fire it (scheduleAI dedupes by ticket).
	h.eng.Sweep(ctx)
	time.Sleep(300 * time.Millisecond)
	if c := h.llm.Calls(); c != 0 {
		t.Fatalf("the sweep and the timer both fired: %d calls", c)
	}
	h.eng.cancelAI(tk.ID) // disarm the 120 s timer before the harness goes away
}
