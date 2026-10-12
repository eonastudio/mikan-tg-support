package cfgw

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"tg-support/internal/panelapi"
	"tg-support/internal/paneltest"
	"tg-support/internal/store"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestReloadNotifyAndKeepLastGood(t *testing.T) {
	f := paneltest.New("tok")
	defer f.Close()
	kv := panelapi.New(f.URL(), "tok", 5*time.Second)
	st := store.New(kv, time.Now)
	w := New(st, quiet())
	ctx := context.Background()

	var seen []store.Config
	w.OnChange(func(c store.Config) { seen = append(seen, c) })

	// Before any load there is no config.
	if _, ok := w.Get(); ok {
		t.Fatal("the watcher is loaded before the first read")
	}
	// The first successful load notifies (the poller must start).
	if _, err := st.SaveConfig(ctx, func(c *store.Config) error { c.SetBotToken("1:tok"); return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0].BotToken != "1:tok" {
		t.Fatalf("the first change: %+v", seen)
	}
	// The same config again: no notification.
	if _, err := w.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 {
		t.Fatalf("a no-change reload notified: %+v", seen)
	}
	// A real change notifies again.
	if _, err := st.SaveConfig(ctx, func(c *store.Config) error { c.SetBotToken("2:tok"); return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[1].BotToken != "2:tok" {
		t.Fatalf("the second change: %+v", seen)
	}
}

func TestFailedReloadKeepsTheCache(t *testing.T) {
	f := paneltest.New("tok")
	kv := panelapi.New(f.URL(), "tok", 5*time.Second)
	st := store.New(kv, time.Now)
	w := New(st, quiet())
	ctx := context.Background()
	if _, err := st.SaveConfig(ctx, func(c *store.Config) error { c.Greeting = "hi"; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	// The panel goes away (as during its restart): the cache survives, and Get
	// keeps answering — the bot must not forget its settings.
	f.Close()
	cfg, err := w.Reload(ctx)
	if err != nil {
		t.Fatalf("the failed reload surfaced an error despite the cache: %v", err)
	}
	if cfg.Greeting != "hi" {
		t.Fatalf("the cache was lost: %+v", cfg)
	}
	if got, ok := w.Get(); !ok || got.Greeting != "hi" {
		t.Fatalf("Get after the failure: %+v %v", got, ok)
	}
	if w.LoadErr() == nil {
		t.Fatal("the failure is invisible")
	}
}
