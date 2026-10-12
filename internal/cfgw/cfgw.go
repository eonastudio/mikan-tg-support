// Package cfgw keeps the config in one place in memory: the poller, the AI and the
// pages all read the same copy, a save on the page reaches them at once, and a
// periodic re-read heals a save made anywhere else (a restore, a second hand in KV).
package cfgw

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"tg-support/internal/store"
)

// Watcher caches cfg/main and tells subscribers when it changed.
type Watcher struct {
	st  *store.Store
	log *slog.Logger

	mu      sync.RWMutex
	cfg     store.Config
	loaded  bool
	loadErr error

	subsMu sync.Mutex
	subs   []func(store.Config)
}

// New is a Watcher; nothing is read until Start or Reload runs.
func New(st *store.Store, log *slog.Logger) *Watcher {
	return &Watcher{st: st, log: log}
}

// OnChange registers fn, called (in the watcher's goroutine or the saver's) with every
// config that differs from the one before. The poller restarts its client this way.
func (w *Watcher) OnChange(fn func(store.Config)) {
	w.subsMu.Lock()
	w.subs = append(w.subs, fn)
	w.subsMu.Unlock()
}

// Get is the cached config; before the first successful load it is the zero config
// (all defaults), and Loaded says which it is.
func (w *Watcher) Get() (store.Config, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.cfg, w.loaded
}

// LoadErr is why the last load failed (the status page shows it).
func (w *Watcher) LoadErr() error {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.loadErr
}

// Reload reads the config now and notifies on a change; the settings page calls it
// after a save so the poller does not wait for the next tick. A failing read keeps
// the last good config (a panel restart must not make the bot forget its token);
// only the very first load has nothing to keep.
func (w *Watcher) Reload(ctx context.Context) (store.Config, error) {
	cfg, err := w.st.LoadConfig(ctx)
	w.mu.Lock()
	w.loadErr = err
	if err != nil {
		kept, wasLoaded := w.cfg, w.loaded
		w.mu.Unlock()
		if wasLoaded {
			return kept, nil // the cache stays; the error is visible via LoadErr
		}
		return store.Config{}, err
	}
	changed := !w.loaded || !equal(w.cfg, cfg)
	w.cfg, w.loaded = cfg, true
	w.mu.Unlock()
	if changed {
		w.notify(cfg)
	}
	return cfg, nil
}

func (w *Watcher) notify(cfg store.Config) {
	w.subsMu.Lock()
	subs := make([]func(store.Config), len(w.subs))
	copy(subs, w.subs)
	w.subsMu.Unlock()
	for _, fn := range subs {
		fn(cfg)
	}
}

// Start re-reads the config every d until ctx ends; the first read runs at once, and
// a failing round keeps the last good config (a panel restart is not a reason for the
// bot to forget its token).
func (w *Watcher) Start(ctx context.Context, d time.Duration) {
	if _, err := w.Reload(ctx); err != nil {
		w.log.Warn("config: the first load failed, retrying", "err", err)
	}
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := w.Reload(ctx); err != nil {
				w.log.Debug("config: reload", "err", err)
			}
		}
	}
}

// equal compares the fields that matter at runtime (all of them, plainly).
func equal(a, b store.Config) bool {
	return a.BotToken == b.BotToken && a.TelegramAPI == b.TelegramAPI && a.Greeting == b.Greeting &&
		a.AIEnabled == b.AIEnabled && a.AIBaseURL == b.AIBaseURL && a.AIAPIKey == b.AIAPIKey &&
		a.AIModel == b.AIModel && a.AIMode == b.AIMode && a.AIDelaySec == b.AIDelaySec &&
		a.AILang == b.AILang && a.AISystem == b.AISystem && a.AIKnowledge == b.AIKnowledge &&
		a.RetentionDays == b.RetentionDays
}
