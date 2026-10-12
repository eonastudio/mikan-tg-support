package evfeed

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"tg-support/internal/panelapi"
	"tg-support/internal/paneltest"
	"tg-support/internal/store"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// runFor starts the feed and stops it after d (or when cond turns true).
func runFor(t *testing.T, f *Feed, d time.Duration, cond func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	var perm atomic.Bool
	done := make(chan struct{})
	go func() { defer close(done); f.Run(ctx, &perm) }()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond != nil && cond() {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	cancel()
	<-done
}

func TestFeedFoldsAndAcks(t *testing.T) {
	pf := paneltest.New("tok")
	defer pf.Close()
	kv := panelapi.New(pf.URL(), "tok", 5*time.Second)
	st := store.New(kv, time.Now)
	f := New(kv, st, quiet())

	pf.Emit("payment.paid", 42, 555, map[string]any{"payment_id": 7, "kind": "new",
		"provider": "stars", "amount": 10000, "currency": "RUB", "term_days": 30})
	pf.Emit("user.expiring", 42, 555, map[string]any{"days": 3, "expires_at": time.Now().Add(72 * time.Hour).Unix()})
	pf.Emit("traffic.100", 42, 555, map[string]any{"period_from": time.Now().Add(-720 * time.Hour).Unix()})
	pf.Emit("bot.paid", 42, 555, map[string]any{"anything": true}) // a holder-only type: the fake filters nothing, the feed must fold it harmlessly

	runFor(t, f, 3*time.Second, func() bool { return pf.Cursor() >= 3 })

	p, err := st.Profile(context.Background(), 555)
	if err != nil {
		t.Fatal(err)
	}
	if p.UserID != 42 || len(p.Payments) != 1 || p.DaysLeft != 3 || p.Traffic100 == 0 {
		t.Fatalf("profile %+v", p)
	}
	if pf.Cursor() < 3 {
		t.Fatalf("the cursor stayed at %d", pf.Cursor())
	}
	// The dedupe marks are in the KV (a replay will bounce off them).
	if _, ok := pf.Value("ev/1"); !ok {
		t.Fatal("no dedupe mark for the first event")
	}
	if st := f.Status(true); st.Applied != 3 {
		t.Fatalf("applied %d, want 3 (the bot.* line has a tg id too)", st.Applied)
	}
}

func TestFeedDedupesAReplay(t *testing.T) {
	pf := paneltest.New("tok")
	defer pf.Close()
	kv := panelapi.New(pf.URL(), "tok", 5*time.Second)
	st := store.New(kv, time.Now)
	f := New(kv, st, quiet())
	ctx := context.Background()

	pf.Emit("payment.paid", 42, 777, map[string]any{"payment_id": 1, "amount": 500, "currency": "USD"})
	runFor(t, f, 2*time.Second, func() bool { return pf.Cursor() == 1 })
	// Rewind the cursor: the same event arrives again, and the profile must not
	// grow a second payment out of it.
	pf.SetCursor(0)
	runFor(t, f, 2*time.Second, func() bool { return pf.Cursor() == 1 })
	p, err := st.Profile(ctx, 777)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Payments) != 1 {
		t.Fatalf("the replay doubled the payments: %+v", p.Payments)
	}
}

func TestFeedSurvivesNoApproval(t *testing.T) {
	pf := paneltest.New("tok")
	defer pf.Close()
	pf.SetApproved(false) // the install window: every call is 403 not_approved
	kv := panelapi.New(pf.URL(), "tok", 5*time.Second)
	st := store.New(kv, time.Now)
	f := New(kv, st, quiet())
	// The loop must not spin hot or die; two seconds of it is enough to tell.
	runFor(t, f, 2*time.Second, nil)
	if s := f.Status(true); s.LastError == "" {
		t.Fatal("the status hides the missing approval")
	}
}

func TestTypesList(t *testing.T) {
	// The feed must not ask for the bot.* types: they are the holder's alone, and
	// a request for them would just be filtered (or refuse the whole list on an
	// older panel).
	for _, typ := range Types {
		if typ == "bot.paid" || typ == "bot.refunded" || typ == "bot.trial_not_given" {
			t.Fatalf("the feed asks for %q", typ)
		}
	}
}
