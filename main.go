// Command tg-support is the mikan addon of protocol 2: a second Telegram bot for
// technical support with a ticket system in the panel and an AI answer scaffold.
//
// It serves the panel (mode http): /v2/info, the admin pages under /ui/ and their
// calls in /v2/ui/rpc; it polls Telegram itself (the bot is its own, not the panel's
// — that slot stays with the official bot); it reads the panel's events for the
// users' profiles; its whole state lives in the panel's KV. The container keeps no
// files and needs only the four MIKAN_* variables the host gives it.
package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"tg-support/internal/ai"
	"tg-support/internal/bot"
	"tg-support/internal/cfgw"
	"tg-support/internal/evfeed"
	"tg-support/internal/meta"
	"tg-support/internal/panelapi"
	"tg-support/internal/srv"
	"tg-support/internal/store"
	"tg-support/internal/telegram"
	"tg-support/internal/tickets"
)

//go:embed web
var webFS embed.FS

// addonIDPattern mirrors the panel's id grammar.
var addonIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "tg-support:", err)
		os.Exit(1)
	}
}

func run() error {
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel()}))

	listen := os.Getenv("MIKAN_ADAPTER_LISTEN")
	adapterToken := os.Getenv("MIKAN_ADAPTER_TOKEN")
	panelURL := os.Getenv("MIKAN_PANEL_URL")
	panelToken := os.Getenv("MIKAN_PANEL_TOKEN")
	if err := srv.Listen(listen); err != nil {
		return err
	}
	if adapterToken == "" {
		return errors.New("MIKAN_ADAPTER_TOKEN is required")
	}
	if panelURL == "" || panelToken == "" {
		return errors.New("MIKAN_PANEL_URL and MIKAN_PANEL_TOKEN are required")
	}
	id := meta.ID
	if v := strings.TrimSpace(os.Getenv("MIKAN_ADDON_ID")); v != "" {
		if !addonIDPattern.MatchString(v) {
			return fmt.Errorf("MIKAN_ADDON_ID %q: want a-z, 0-9 and dashes, at most 32", v)
		}
		id = v
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	kv := panelapi.New(panelURL, panelToken, 60*time.Second)
	st := store.New(kv, time.Now)
	watch := cfgw.New(st, log)

	// send builds (and caches) the Telegram client of the current config; llm the AI
	// client. A nil answer means "not configured", and the engine stays quiet.
	senders := &clientCache{}
	send := func(cfg store.Config) tickets.Sender {
		if cfg.BotToken == "" {
			return nil
		}
		return senders.telegram(cfg.APIBase(), cfg.BotToken)
	}
	llm := func(cfg store.Config) tickets.LLM {
		if !cfg.AIReady() {
			return nil
		}
		return senders.ai(cfg.AIBaseURL, cfg.AIAPIKey, cfg.AIModel)
	}

	engCfg := func(ctx context.Context) (store.Config, error) {
		if cfg, ok := watch.Get(); ok {
			return cfg, nil
		}
		return watch.Reload(ctx)
	}
	eng := tickets.New(st, engCfg, send, llm, log)
	poller := bot.New(st, eng, log)
	feed := evfeed.New(kv, st, log)

	rpc := srv.NewRPC(srv.RPCDeps{Store: st, Engine: eng, Watch: watch, Poller: poller, Feed: feed,
		Panel: selfCache.read, Log: log})

	pages, err := fs.Sub(webFS, "web")
	if err != nil {
		return err
	}
	server, err := srv.New(srv.Deps{
		Info: srv.Info{
			ID: id, Protocol: meta.Protocol, Version: meta.Version, Kind: meta.Kind, Mode: meta.Mode,
			Name: meta.Name, Description: meta.Description, Permissions: meta.Permissions,
			UI:       srv.UI{Entry: "/ui/index.html", Title: meta.Title},
			Settings: []any{},
			Help:     meta.Help,
		},
		Pages: pages, RPC: rpc, Log: log, Token: adapterToken,
	})
	if err != nil {
		return err
	}

	// The config watcher restarts the poller when the token or the API moves.
	watch.OnChange(func(cfg store.Config) { poller.ApplyConfig(ctx, cfg) })

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); watch.Start(ctx, 15*time.Second) }()
	wg.Add(1)
	go func() { defer wg.Done(); feed.Run(ctx, rpc.EventsPermitted()) }()
	wg.Add(1)
	go func() { defer wg.Done(); keepSelf(ctx, kv, id, log) }()
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				eng.Sweep(ctx)
			}
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(6 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				cfg, ok := watch.Get()
				if !ok {
					continue
				}
				if n, err := st.Prune(ctx, cfg.Retention()); err != nil {
					log.Warn("prune", "err", err)
				} else if n > 0 {
					log.Info("prune", "removed", n)
				}
			}
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := srv.Run(ctx, listen, server.Handler(), log); err != nil {
			log.Error("the server stopped", "err", err)
			stop()
		}
	}()

	log.Info("started", "id", id, "version", meta.Version, "listen", listen, "panel", panelURL)
	<-ctx.Done()
	log.Info("stopping")
	poller.Stop() // saves the offset on its way out
	shutdownWait(&wg, 15*time.Second, log)
	return nil
}

func shutdownWait(wg *sync.WaitGroup, d time.Duration, log *slog.Logger) {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		log.Warn("the shutdown timed out")
	}
}

func logLevel() slog.Level {
	switch strings.ToLower(os.Getenv("MIKAN_LOG")) {
	case "debug":
		return slog.LevelDebug
	case "warn", "error":
		return slog.LevelWarn
	}
	return slog.LevelInfo
}

// selfState keeps the panel's view of the addon (GET /panel/v2/self) for the status
// page, and says loudly when the registration's id and the answer disagree — the
// sign of an install under a different id, which the host refuses anyway.
type selfState struct {
	mu   sync.RWMutex
	info srv.PanelInfo
}

func (c *selfState) read() srv.PanelInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.info
}

var selfCache selfState

func keepSelf(ctx context.Context, kv *panelapi.Client, id string, log *slog.Logger) {
	once := true
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		s, err := kv.Self(ctx)
		selfCache.mu.Lock()
		if err != nil {
			selfCache.info.LastError = err.Error()
			selfCache.info.LastAt = time.Now().Unix()
		} else {
			selfCache.info = srv.PanelInfo{ID: s.ID, Version: s.Version, Permissions: s.Permissions,
				LastAt: time.Now().Unix()}
			if once && s.ID != id {
				log.Error("the panel knows the addon under another id — reinstall it with id "+id,
					"panel_says", s.ID, "addon_says", id)
			}
		}
		selfCache.mu.Unlock()
		once = false
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// clientCache keeps one Telegram client and one AI client per set of credentials:
// building an http.Transport per message would burn file descriptors for nothing.
type clientCache struct {
	mu    sync.Mutex
	tgKey string
	tg    *telegram.Client
	aiKey string
	aiCl  *ai.Client
}

func (c *clientCache) telegram(api, token string) *telegram.Client {
	key := api + "\x00" + token
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tg == nil || c.tgKey != key {
		c.tg, c.tgKey = telegram.New(api, token), key
	}
	return c.tg
}

func (c *clientCache) ai(base, key, model string) *ai.Client {
	k := base + "\x00" + key + "\x00" + model
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.aiCl == nil || c.aiKey != k {
		c.aiCl, c.aiKey = ai.New(base, key, model, 30*time.Second), k
	}
	return c.aiCl
}
