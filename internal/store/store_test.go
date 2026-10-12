package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"tg-support/internal/panelapi"
	"tg-support/internal/paneltest"
)

func newStore(t *testing.T) (*Store, *paneltest.Fake) {
	t.Helper()
	f := paneltest.New("panel-token")
	t.Cleanup(f.Close)
	kv := panelapi.New(f.URL(), "panel-token", 10*time.Second)
	return New(kv, time.Now), f
}

func TestCreateTicketLayout(t *testing.T) {
	s, f := newStore(t)
	ctx := context.Background()
	tk, err := s.CreateTicket(ctx, NewTicket{TgID: 111, ChatID: 111, Username: "ann", FirstName: "Ann", Lang: "ru"},
		Message{Kind: KindText, Text: "Не работает VPN", TgMsgID: 5})
	if err != nil {
		t.Fatal(err)
	}
	if tk.ID != 1 || tk.Status != StatusOpen || tk.Seq != 1 || tk.Unread != 1 || tk.LastFrom != FromUser {
		t.Fatalf("the ticket is %+v", tk)
	}
	for _, k := range []string{"seq/tickets", "t/00000001", "m/00000001/0000", "ut/111", "idx/tickets"} {
		if _, ok := f.Value(k); !ok {
			t.Errorf("no %s in the KV: %v", k, f.Keys())
		}
	}
	got, ok, err := s.OpenTicketFor(ctx, 111)
	if err != nil || !ok || got.ID != 1 {
		t.Fatalf("OpenTicketFor: %v %v %+v", ok, err, got)
	}
	msgs, err := s.Messages(ctx, 1, 0, 0)
	if err != nil || len(msgs) != 1 || msgs[0].Text != "Не работает VPN" || msgs[0].TgMsgID != 5 {
		t.Fatalf("Messages: %v %+v", err, msgs)
	}
	// The second ticket of another user gets the next id.
	t2, err := s.CreateTicket(ctx, NewTicket{TgID: 222, ChatID: 222}, Message{Kind: KindText, Text: "hi"})
	if err != nil || t2.ID != 2 {
		t.Fatalf("the second ticket: %v %+v", err, t2)
	}
}

func TestAppendMessageAndDedupe(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	tk, _ := s.CreateTicket(ctx, NewTicket{TgID: 1, ChatID: 1}, Message{Kind: KindText, Text: "first", TgMsgID: 10})
	if err := s.AppendMessage(ctx, tk.ID, Message{From: FromUser, Kind: KindText, Text: "second", TgMsgID: 11}); err != nil {
		t.Fatal(err)
	}
	// A replay of the same Telegram message changes nothing.
	if err := s.AppendMessage(ctx, tk.ID, Message{From: FromUser, Kind: KindText, Text: "second", TgMsgID: 11}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendMessage(ctx, tk.ID, Message{From: FromSupport, Kind: KindText, Text: "answer"}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Ticket(ctx, tk.ID)
	if got.Seq != 3 || got.MsgCount != 3 {
		t.Fatalf("seq %d count %d, want 3 and 3", got.Seq, got.MsgCount)
	}
	if got.Unread != 0 || got.LastFrom != FromSupport {
		t.Fatalf("unread %d last %q", got.Unread, got.LastFrom)
	}
	msgs, _ := s.Messages(ctx, tk.ID, 0, 0)
	if len(msgs) != 3 || msgs[1].Text != "second" || msgs[2].From != FromSupport {
		t.Fatalf("history %+v", msgs)
	}
	if msgs[0].Seq != 1 || msgs[1].Seq != 2 || msgs[2].Seq != 3 {
		t.Fatalf("seqs %+v", msgs)
	}
}

func TestAppendConcurrent(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	tk, _ := s.CreateTicket(ctx, NewTicket{TgID: 1, ChatID: 1}, Message{Kind: KindText, Text: "start"})
	const writers, each = 8, 6
	var wg sync.WaitGroup
	errs := make(chan error, writers*each)
	for w := range writers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range each {
				m := Message{From: FromSupport, Kind: KindText, Text: fmt.Sprintf("w%d-m%d", w, i)}
				if err := s.AppendMessage(ctx, tk.ID, m); err != nil {
					errs <- err
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	got, _ := s.Ticket(ctx, tk.ID)
	if got.MsgCount != 1+writers*each || got.Seq != int64(1+writers*each) {
		t.Fatalf("count %d seq %d, want %d", got.MsgCount, got.Seq, 1+writers*each)
	}
	msgs, _ := s.Messages(ctx, tk.ID, 0, 0)
	if len(msgs) != 1+writers*each {
		t.Fatalf("the history holds %d, want %d", len(msgs), 1+writers*each)
	}
	seen := map[int64]bool{}
	for _, m := range msgs {
		if seen[m.Seq] {
			t.Fatalf("seq %d twice", m.Seq)
		}
		seen[m.Seq] = true
	}
}

func TestChunkRollover(t *testing.T) {
	s, f := newStore(t)
	ctx := context.Background()
	tk, _ := s.CreateTicket(ctx, NewTicket{TgID: 1, ChatID: 1}, Message{Kind: KindText, Text: "start"})
	big := strings.Repeat("x", 3000) // ~3 KB per message: 48 KB flush at ~16
	for i := range 40 {
		if err := s.AppendMessage(ctx, tk.ID, Message{From: FromUser, Kind: KindText, Text: fmt.Sprintf("%02d %s", i, big)}); err != nil {
			t.Fatal(err)
		}
	}
	msgs, err := s.Messages(ctx, tk.ID, 0, 0)
	if err != nil || len(msgs) != 41 {
		t.Fatalf("history %d (%v), want 41", len(msgs), err)
	}
	for i, m := range msgs {
		if m.Seq != int64(i+1) {
			t.Fatalf("message %d has seq %d", i, m.Seq)
		}
	}
	chunks := 0
	for _, k := range f.Keys() {
		if strings.HasPrefix(k, "m/00000001/") {
			chunks++
		}
	}
	if chunks < 2 {
		t.Fatalf("no rollover: %d chunks", chunks)
	}
	got, _ := s.Ticket(ctx, tk.ID)
	if got.LastChunk != chunks-1 {
		t.Fatalf("LastChunk %d, want %d", got.LastChunk, chunks-1)
	}
	// The tail read (the AI context) works across the chunks.
	last, err := s.LastMessages(ctx, tk.ID, 5)
	if err != nil || len(last) != 5 || last[4].Seq != 41 {
		t.Fatalf("tail %+v (%v)", last, err)
	}
}

func TestCloseReopenDelete(t *testing.T) {
	s, f := newStore(t)
	ctx := context.Background()
	tk, _ := s.CreateTicket(ctx, NewTicket{TgID: 7, ChatID: 7}, Message{Kind: KindText, Text: "q"})
	if _, err := s.CloseTicket(ctx, tk.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.OpenTicketFor(ctx, 7); ok {
		t.Fatal("the pointer survived the close")
	}
	heads, _ := s.ListTickets(ctx, StatusClosed)
	if len(heads) != 1 || heads[0].Status != StatusClosed {
		t.Fatalf("closed heads %+v", heads)
	}
	if _, err := s.ReopenTicket(ctx, tk.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.OpenTicketFor(ctx, 7); !ok {
		t.Fatal("the reopen did not point the user back")
	}
	if err := s.DeleteTicket(ctx, tk.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Ticket(ctx, tk.ID); err == nil {
		t.Fatal("the ticket survived its delete")
	}
	for _, k := range f.Keys() {
		if strings.HasPrefix(k, "m/00000001/") || k == "t/00000001" || k == "ut/7" {
			t.Fatalf("%s survived the delete", k)
		}
	}
	heads, _ = s.ListTickets(ctx, "")
	if len(heads) != 0 {
		t.Fatalf("the index kept %+v", heads)
	}
}

func TestPruneKeepsOpen(t *testing.T) {
	base := time.Now().Add(-90 * 24 * time.Hour)
	s, _ := newStore(t)
	s.now = func() time.Time { return base }
	ctx := context.Background()
	old, _ := s.CreateTicket(ctx, NewTicket{TgID: 1, ChatID: 1}, Message{Kind: KindText, Text: "old"})
	fresh, _ := s.CreateTicket(ctx, NewTicket{TgID: 2, ChatID: 2}, Message{Kind: KindText, Text: "fresh"})
	if _, err := s.CloseTicket(ctx, old.ID); err != nil {
		t.Fatal(err)
	}
	s.now = time.Now // back to the present: the old ticket is way past the retention
	n, err := s.Prune(ctx, 60*24*time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("prune removed %d (%v), want 1", n, err)
	}
	if _, err := s.Ticket(ctx, old.ID); err == nil {
		t.Fatal("the old closed ticket survived")
	}
	if _, err := s.Ticket(ctx, fresh.ID); err != nil {
		t.Fatal("the open ticket was pruned:", err)
	}
}

func TestProfileFromEvents(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	at := time.Now()
	paid, _ := json.Marshal(map[string]any{"payment_id": 9, "kind": "renew", "provider": "stars",
		"amount": 35000, "currency": "RUB", "term_days": 30})
	if err := s.ApplyEvent(ctx, 555, 42, "payment.paid", at, paid); err != nil {
		t.Fatal(err)
	}
	exp, _ := json.Marshal(map[string]any{"days": 3, "expires_at": at.Add(72 * time.Hour).Unix()})
	if err := s.ApplyEvent(ctx, 555, 42, "user.expiring", at, exp); err != nil {
		t.Fatal(err)
	}
	ref, _ := json.Marshal(map[string]any{"payment_id": 9})
	if err := s.ApplyEvent(ctx, 555, 42, "payment.refunded", at, ref); err != nil {
		t.Fatal(err)
	}
	p, err := s.Profile(ctx, 555)
	if err != nil {
		t.Fatal(err)
	}
	if p.UserID != 42 || p.DaysLeft != 3 || p.ExpiresAt == 0 {
		t.Fatalf("profile %+v", p)
	}
	if len(p.Payments) != 1 || !p.Payments[0].Refunded || p.Payments[0].Amount != 35000 {
		t.Fatalf("payments %+v", p.Payments)
	}
	// No tg id: nothing to fold into, and no error.
	if err := s.ApplyEvent(ctx, 0, 42, "payment.paid", at, paid); err != nil {
		t.Fatal(err)
	}
	// A malformed payload keeps the profile.
	if err := s.ApplyEvent(ctx, 555, 42, "user.expiring", at, json.RawMessage(`{`)); err != nil {
		t.Fatal(err)
	}
	if p2, _ := s.Profile(ctx, 555); p2.DaysLeft != 3 {
		t.Fatalf("the bad event hurt the profile: %+v", p2)
	}
}

func TestConfigSecrets(t *testing.T) {
	s, f := newStore(t)
	ctx := context.Background()
	if _, err := s.SaveConfig(ctx, func(c *Config) error {
		c.SetBotToken("123:abc")
		c.AIModel = "gpt"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// "" keeps the saved secret.
	if _, err := s.SaveConfig(ctx, func(c *Config) error {
		c.SetBotToken("")
		c.AIModel = "gpt-2"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := s.LoadConfig(ctx)
	if err != nil || cfg.BotToken != "123:abc" || cfg.AIModel != "gpt-2" {
		t.Fatalf("config %+v (%v)", cfg, err)
	}
	if v := cfg.View(); !v.HasBotToken {
		t.Fatal("the view does not say the token is set")
	}
	if raw, err := json.Marshal(cfg.View()); err != nil || strings.Contains(string(raw), "123:abc") {
		t.Fatalf("the view leaks the token: %s (%v)", raw, err)
	}
	if _, ok := f.Value("cfg/main"); !ok {
		t.Fatal("no cfg/main")
	}
	// A refusal of fn changes nothing.
	if _, err := s.SaveConfig(ctx, func(c *Config) error { return fmt.Errorf("no") }); err == nil {
		t.Fatal("the save accepted a refusal")
	}
	if cfg2, _ := s.LoadConfig(ctx); cfg2.AIModel != "gpt-2" {
		t.Fatalf("the refused save changed the config: %+v", cfg2)
	}
	// Validation: a bad mode does not save.
	if _, err := s.SaveConfig(ctx, func(c *Config) error { c.AIMode = "wild"; return nil }); err == nil {
		t.Fatal("a bad ai_mode saved")
	}
	if err := cfg.ClearSecret("bot_token"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.ClearSecret("what"); err == nil {
		t.Fatal("an unknown secret cleared")
	}
}

func TestMarkOnceAndOffset(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	first, err := s.MarkOnce(ctx, "ev/1", time.Hour)
	if err != nil || !first {
		t.Fatalf("the first mark: %v %v", first, err)
	}
	again, err := s.MarkOnce(ctx, "ev/1", time.Hour)
	if err != nil || again {
		t.Fatalf("the second mark: %v %v", again, err)
	}
	if off, _ := s.Offset(ctx); off != 0 {
		t.Fatalf("offset %d, want 0", off)
	}
	if err := s.SaveOffset(ctx, 42); err != nil {
		t.Fatal(err)
	}
	if off, _ := s.Offset(ctx); off != 42 {
		t.Fatalf("offset %d, want 42", off)
	}
}

func TestRebuildIndex(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	for i := range 5 {
		if _, err := s.CreateTicket(ctx, NewTicket{TgID: int64(i + 1), ChatID: int64(i + 1)},
			Message{Kind: KindText, Text: fmt.Sprintf("t%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	// Lose the index, then rebuild it from the tickets.
	if err := s.kv.KVDelete(ctx, indexKey, 0); err != nil {
		t.Fatal(err)
	}
	if heads, _ := s.ListTickets(ctx, ""); len(heads) != 0 {
		t.Fatalf("the lost index still lists %d", len(heads))
	}
	n, err := s.RebuildIndex(ctx)
	if err != nil || n != 5 {
		t.Fatalf("rebuild %d (%v), want 5", n, err)
	}
	heads, _ := s.ListTickets(ctx, "")
	if len(heads) != 5 {
		t.Fatalf("the rebuilt index holds %d", len(heads))
	}
	if heads[0].ID != 5 {
		t.Fatalf("the rebuilt index is not sorted newest first: %+v", heads)
	}
}
