package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"tg-support/internal/panelapi"
	"tg-support/internal/paneltest"
	"tg-support/internal/store"
	"tg-support/internal/telegram"
	"tg-support/internal/tickets"
)

// fakeTG is a Bot API stand-in with an updates queue and a log of what was sent.
type fakeTG struct {
	*httptest.Server
	mu      sync.Mutex
	updates []map[string]any
	sent    []sentTo
	refuse  int // a status to answer everything with (0: normal)
}

type sentTo struct {
	ChatID int64
	Text   string
}

func newFakeTG(t *testing.T) *fakeTG {
	t.Helper()
	f := &fakeTG{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /bot<token>/<method>: the method is the last path segment, whatever the token.
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
		method := ""
		if len(parts) == 2 {
			method = parts[1]
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		refuse := f.refuse
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if refuse != 0 {
			w.WriteHeader(refuse)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": refuse, "description": "refused"})
			return
		}
		switch method {
		case "getMe":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{
				"id": 123, "is_bot": true, "first_name": "Поддержка", "username": "support_bot"}})
		case "getUpdates":
			off, _ := body["offset"].(float64)
			f.mu.Lock()
			var out []map[string]any
			for _, u := range f.updates {
				id, _ := u["update_id"].(int64) // the queue holds native ints
				if float64(id) >= off || off < 0 {
					out = append(out, u)
				}
			}
			f.mu.Unlock()
			if out == nil {
				time.Sleep(20 * time.Millisecond) // do not let the poller spin hot
				out = []map[string]any{}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": out})
		case "sendMessage":
			chat, _ := body["chat_id"].(float64)
			text, _ := body["text"].(string)
			f.mu.Lock()
			f.sent = append(f.sent, sentTo{int64(chat), text})
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": 500}})
		case "setMyCommands":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeTG) push(updateID int64, fromID, chatID int64, chatType, text string) {
	f.mu.Lock()
	f.updates = append(f.updates, map[string]any{
		"update_id": updateID,
		"message": map[string]any{
			"message_id": updateID * 10,
			"from":       map[string]any{"id": fromID, "is_bot": false, "first_name": "Ann", "username": "ann", "language_code": "ru"},
			"chat":       map[string]any{"id": chatID, "type": chatType},
			"date":       time.Now().Unix(),
			"text":       text,
		},
	})
	f.mu.Unlock()
}

func (f *fakeTG) texts() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var b []string
	for _, s := range f.sent {
		b = append(b, fmt.Sprintf("%d:%s", s.ChatID, s.Text))
	}
	return strings.Join(b, "|")
}

type botStack struct {
	fake   *fakeTG
	pf     *paneltest.Fake
	st     *store.Store
	poller *Poller
	eng    *tickets.Engine
	cancel context.CancelFunc
}

func newBotStack(t *testing.T) *botStack {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fakeTG := newFakeTG(t)
	pf := paneltest.New("panel-token")
	kv := panelapi.New(pf.URL(), "panel-token", 10*time.Second)
	st := store.New(kv, time.Now)
	cfg := store.Config{BotToken: "123:secret", TelegramAPI: fakeTG.URL}
	if _, err := st.SaveConfig(context.Background(), func(c *store.Config) error {
		c.BotToken = cfg.BotToken
		c.TelegramAPI = cfg.TelegramAPI
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	eng := tickets.New(st, func(context.Context) (store.Config, error) { return cfg, nil },
		func(c store.Config) tickets.Sender {
			if c.BotToken == "" {
				return nil
			}
			return telegram.New(c.APIBase(), c.BotToken)
		},
		func(store.Config) tickets.LLM { return nil }, log)
	p := New(st, eng, log)
	ctx, cancel := context.WithCancel(context.Background())
	p.ApplyConfig(ctx, cfg)
	t.Cleanup(func() { cancel(); p.Stop(); pf.Close() })
	return &botStack{fake: fakeTG, pf: pf, st: st, poller: p, eng: eng, cancel: cancel}
}

// waitFor polls cond until it holds or the timeout passes.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestPollerOpensTickets(t *testing.T) {
	s := newBotStack(t)
	ctx := context.Background()
	s.fake.push(1, 555, 555, "private", "не работает VPN")
	waitFor(t, 5*time.Second, "the ticket", func() bool {
		tk, ok, _ := s.st.OpenTicketFor(ctx, 555)
		return ok && tk.ID == 1
	})
	if got := s.fake.texts(); !strings.Contains(got, "Тикет #1 создан") {
		t.Fatalf("the user got %q", got)
	}
	// The status shows a running poller that learned the bot.
	st := s.poller.Status(store.Config{BotToken: "123:secret", TelegramAPI: s.fake.URL})
	if !st.Running || st.Username != "@support_bot" {
		t.Fatalf("status %+v", st)
	}
	// A second message continues the ticket.
	s.fake.push(2, 555, 555, "private", "и приложение виснет")
	waitFor(t, 5*time.Second, "the second message", func() bool {
		msgs, _ := s.st.Messages(ctx, 1, 0, 0)
		return len(msgs) == 2
	})
	// A group message is ignored: no ticket for the group.
	s.fake.push(3, 777, -100777, "supergroup", "hello")
	time.Sleep(400 * time.Millisecond)
	if _, ok, _ := s.st.OpenTicketFor(ctx, 777); ok {
		t.Fatal("a group message opened a ticket")
	}
	// /start answers with the greeting.
	before := len(strings.Split(s.fake.texts(), "|"))
	s.fake.push(4, 555, 555, "private", "/start")
	waitFor(t, 5*time.Second, "the greeting", func() bool {
		return len(strings.Split(s.fake.texts(), "|")) > before
	})
	if !strings.Contains(s.fake.texts(), "бот технической поддержки") {
		t.Fatalf("the greeting: %q", s.fake.texts())
	}
	// /close closes the ticket and says so.
	s.fake.push(5, 555, 555, "private", "/close")
	waitFor(t, 5*time.Second, "the close", func() bool {
		_, ok, _ := s.st.OpenTicketFor(ctx, 555)
		return !ok
	})
	if !strings.Contains(s.fake.texts(), "закрыт") {
		t.Fatalf("the close notice: %q", s.fake.texts())
	}
}

func TestPollerDedupesReplays(t *testing.T) {
	s := newBotStack(t)
	ctx := context.Background()
	s.fake.push(1, 555, 555, "private", "вопрос")
	waitFor(t, 5*time.Second, "the ticket", func() bool {
		_, ok, _ := s.st.OpenTicketFor(ctx, 555)
		return ok
	})
	msgsBefore, _ := s.st.Messages(ctx, 1, 0, 0)
	// Simulate a crash before the offset save: rewind the offset and restart the
	// poller — the same update must not create a second message.
	if err := s.st.SaveOffset(ctx, 1); err != nil {
		t.Fatal(err)
	}
	s.poller.Stop()
	p2 := New(s.st, s.eng, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	p2.ApplyConfig(ctx2, store.Config{BotToken: "123:secret", TelegramAPI: s.fake.URL})
	defer p2.Stop()
	time.Sleep(700 * time.Millisecond)
	msgsAfter, _ := s.st.Messages(ctx, 1, 0, 0)
	if len(msgsAfter) != len(msgsBefore) {
		t.Fatalf("the replay doubled the history: %d → %d", len(msgsBefore), len(msgsAfter))
	}
	if n := strings.Count(s.fake.texts(), "Тикет #1 создан"); n != 1 {
		t.Fatalf("the confirmation was sent %d times", n)
	}
}

func TestPollerStopsOnABadToken(t *testing.T) {
	s := newBotStack(t)
	// Everything is refused with a 401 from now on.
	s.fake.mu.Lock()
	s.fake.refuse = http.StatusUnauthorized
	s.fake.mu.Unlock()
	cfg := store.Config{BotToken: "123:secret", TelegramAPI: s.fake.URL}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := New(s.st, s.eng, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer p.Stop()
	p.ApplyConfig(ctx, cfg)
	waitFor(t, 5*time.Second, "the refusal", func() bool {
		st := p.Status(cfg)
		return !st.Running && strings.Contains(st.LastError, "refused")
	})
	// Without a token the poller is stopped and says it is not configured.
	p.ApplyConfig(ctx, store.Config{})
	if st := p.Status(store.Config{}); st.Running || st.Configured {
		t.Fatalf("status %+v", st)
	}
}

func TestPollerRestartsOnATokenChange(t *testing.T) {
	s := newBotStack(t)
	// The same config: nothing restarts and the poller keeps running.
	cfg := store.Config{BotToken: "123:secret", TelegramAPI: s.fake.URL}
	s.poller.ApplyConfig(context.Background(), cfg)
	waitFor(t, 3*time.Second, "the poller", func() bool {
		return s.poller.Status(cfg).Running
	})
	// A new token: the old loop stops and a fresh one runs (the fake answers any
	// token; what matters is that the poller stays up with the new key).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg2 := store.Config{BotToken: "999:secret-two", TelegramAPI: s.fake.URL}
	s.poller.ApplyConfig(ctx, cfg2)
	waitFor(t, 3*time.Second, "the restarted poller", func() bool {
		st := s.poller.Status(cfg2)
		return st.Running && st.Username == "@support_bot"
	})
	// An empty token stops it for good.
	s.poller.ApplyConfig(ctx, store.Config{TelegramAPI: s.fake.URL})
	if st := s.poller.Status(store.Config{}); st.Running || st.Configured {
		t.Fatalf("the poller survived the token removal: %+v", st)
	}
}
