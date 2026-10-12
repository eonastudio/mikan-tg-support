// Package bot runs the addon's own Telegram bot: the poller of api.telegram.org (or
// the mirror the admin set), the commands and the handover of user messages to the
// ticket engine. It is not the panel's bot — that slot belongs to the official bot;
// this one is a bot of its own, with the token the admin pasted on the addon's page.
package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"tg-support/internal/store"
	"tg-support/internal/telegram"
	"tg-support/internal/tickets"
)

// Status is what the admin page shows of the bot.
type Status struct {
	Running     bool   `json:"running"`
	Configured  bool   `json:"configured"`
	Username    string `json:"username,omitempty"`
	BotName     string `json:"bot_name,omitempty"`
	LastUpdate  int64  `json:"last_update_at,omitempty"`
	LastError   string `json:"last_error,omitempty"`
	LastErrorAt int64  `json:"last_error_at,omitempty"`
	Processed   int64  `json:"processed"`
	Offset      int64  `json:"offset"`
	APIBase     string `json:"api_base,omitempty"`
}

// Poller keeps one getUpdates loop per config (token+API); ApplyConfig restarts it
// when either changes and stops it when the token goes.
type Poller struct {
	st  *store.Store
	eng *tickets.Engine
	log *slog.Logger

	mu     sync.Mutex
	cancel context.CancelFunc // of the running loop
	key    string             // token+"\x00"+api the loop runs with
	gen    uint64             // the loop's generation: an old loop must not clear the new one's status
	fails  map[int64]int      // per update, for the poison-update skip

	offset    atomic.Int64
	running   atomic.Bool
	username  atomic.Value // string
	botName   atomic.Value // string
	lastUpd   atomic.Int64
	lastErr   atomic.Value // string
	lastErrAt atomic.Int64
	processed atomic.Int64
	stopped   chan struct{} // closed with the main ctx
	stopOnce  sync.Once
}

// New is a Poller; call ApplyConfig whenever the config may have changed.
func New(st *store.Store, eng *tickets.Engine, log *slog.Logger) *Poller {
	p := &Poller{st: st, eng: eng, log: log, stopped: make(chan struct{}), fails: map[int64]int{}}
	p.username.Store("")
	p.botName.Store("")
	p.lastErr.Store("")
	return p
}

// ApplyConfig (re)starts the loop for the config's token, or stops it without one.
// Called by the config watcher on every change and once at the start.
func (p *Poller) ApplyConfig(ctx context.Context, cfg store.Config) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := cfg.BotToken + "\x00" + cfg.APIBase()
	if cfg.BotToken == "" {
		p.stopLocked()
		return
	}
	if p.cancel != nil && p.key == key {
		return // the same bot keeps polling
	}
	p.stopLocked()
	select {
	case <-p.stopped:
		return // shutting down; no new loops
	default:
	}
	loopCtx, cancel := context.WithCancel(ctx)
	p.cancel, p.key = cancel, key
	p.gen++
	gen := p.gen
	p.running.Store(true)
	go p.loop(loopCtx, gen, cfg.BotToken, cfg.APIBase())
}

func (p *Poller) stopLocked() {
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
		p.key = ""
	}
	p.running.Store(false)
}

// Stop ends the poller for good (the process is going down); a second Stop is a
// no-op (the shutdown path and a test cleanup may both call it).
func (p *Poller) Stop() {
	p.stopOnce.Do(func() { close(p.stopped) })
	p.mu.Lock()
	p.stopLocked()
	p.mu.Unlock()
}

// Status is the readout for the admin page.
func (p *Poller) Status(cfg store.Config) Status {
	s := Status{
		Running:    p.running.Load(),
		Configured: cfg.BotToken != "",
		Username:   p.username.Load().(string),
		BotName:    p.botName.Load().(string),
		LastUpdate: p.lastUpd.Load(),
		LastError:  p.lastErr.Load().(string),
		Offset:     p.offset.Load(),
		Processed:  p.processed.Load(),
	}
	if s.LastError != "" {
		s.LastErrorAt = p.lastErrAt.Load()
	}
	if cfg.BotToken != "" {
		s.APIBase = cfg.APIBase()
	}
	return s
}

func (p *Poller) fail(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	p.lastErr.Store(msg)
	p.lastErrAt.Store(time.Now().Unix())
	p.log.Warn("bot: " + msg)
}

func (p *Poller) clearErr() { p.lastErr.Store("") }

// loop is the getUpdates round: it recovers the offset from the KV, learns the bot
// with getMe, then polls until its context ends. Transport failures back off; a 409
// means another poller holds the token (a second container or a token pasted into
// another program) — it says so loudly and waits.
func (p *Poller) loop(ctx context.Context, gen uint64, token, api string) {
	cl := telegram.New(api, token)
	defer func() {
		// Only the loop of the current generation clears the status: a replaced loop
		// must not report the new one as stopped while it winds down.
		p.mu.Lock()
		current := p.gen == gen
		p.mu.Unlock()
		if current {
			p.running.Store(false)
		}
		if off := p.offset.Load(); off > 0 {
			sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			_ = p.st.SaveOffset(sctx, off)
			cancel()
		}
	}()

	// The offset: where the last run stopped. Without one (a fresh install) the
	// backlog is acknowledged away: the bot answers what is written to it from now on,
	// not what piled up in the queue before the token was pasted.
	off, err := p.st.Offset(ctx)
	if err != nil {
		p.log.Warn("bot: the offset did not load, treating the bot as fresh", "err", err)
	}
	p.offset.Store(off)
	if off == 0 {
		if _, err := cl.GetUpdates(ctx, -1, 0, nil); err != nil && !errors.Is(err, context.Canceled) {
			p.log.Info("bot: the backlog acknowledge failed", "err", err)
		}
	}

	// getMe: who the bot is (the page shows it); a bad token ends the loop quietly —
	// ApplyConfig restarts it when the admin pastes a new one.
	me, err := cl.GetMe(ctx)
	if err != nil {
		var te *telegram.Error
		if errors.As(err, &te) && te.IsUnauthorized() {
			p.fail("the bot token was refused by Telegram (401/403) — check it on the settings page")
			return
		}
		// A transport failure or a broken answer is not a reason to stop: getUpdates
		// below retries on its own schedule, and the name stays unknown meanwhile.
		p.fail("getMe: %v", err)
	} else {
		p.username.Store("@" + me.Username)
		p.botName.Store(me.FirstName)
		p.clearErr()
		if err := cl.SetMyCommands(ctx); err != nil {
			p.log.Info("bot: setMyCommands", "err", err) // cosmetic
		}
	}

	backoff := time.Duration(0)
	for ctx.Err() == nil {
		updates, err := cl.GetUpdates(ctx, p.offset.Load(), 50, []string{"message"})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			backoff = nextBackoff(backoff)
			var te *telegram.Error
			switch {
			case errors.As(err, &te) && te.IsConflict():
				p.fail("another poller runs with this bot token (409) — a second container or the token is used elsewhere; waiting")
				sleep(ctx, 30*time.Second)
			case errors.As(err, &te) && te.IsUnauthorized():
				p.fail("the bot token was refused by Telegram (401/403)")
				return // waits for a new token through ApplyConfig
			case errors.As(err, &te) && te.RetryAfter() > 0:
				sleep(ctx, te.RetryAfter()+time.Second)
			default:
				p.fail("getUpdates: %v (retrying in %s)", err, backoff)
				sleep(ctx, backoff)
			}
			continue
		}
		backoff = 0
		p.clearErr()
		fresh := false
		for _, u := range updates {
			if ctx.Err() != nil {
				return
			}
			if err := p.handle(ctx, cl, u); err != nil {
				// The update stays unacknowledged: the next getUpdates brings the
				// batch again, and the per-update dedupe keeps the handled ones quiet.
				p.log.Warn("bot: the batch waits on a failing update", "update", u.UpdateID, "err", err)
				sleep(ctx, 3*time.Second)
				break
			}
			if u.UpdateID+1 > p.offset.Load() {
				p.offset.Store(u.UpdateID + 1)
			}
			fresh = true
		}
		if fresh {
			if err := p.st.SaveOffset(ctx, p.offset.Load()); err != nil {
				p.log.Warn("bot: the offset did not save", "err", err)
			}
			p.lastUpd.Store(time.Now().Unix())
		}
	}
}

func nextBackoff(cur time.Duration) time.Duration {
	if cur <= 0 {
		return 2 * time.Second
	}
	return min(cur*2, time.Minute)
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// maxFails is how many rounds one update may fail before it is skipped: a poison
// update must not stop the queue forever.
const maxFails = 3

// handle is one update: dispatch, and only a success is marked handled — a crash
// between the two replays the update, and the engine's own dedupes (by the Telegram
// message id) keep the replay from doubling what the user sees.
func (p *Poller) handle(ctx context.Context, cl *telegram.Client, u telegram.Update) error {
	mark := fmt.Sprintf("upd/%d", u.UpdateID)
	first, err := p.st.MarkOnce(ctx, mark, 48*time.Hour)
	if err != nil {
		return err // the KV is down: nothing works anyway, the batch waits
	}
	if !first {
		p.processed.Add(1)
		return nil // handled before a restart
	}
	if err := p.dispatch(ctx, cl, u); err != nil {
		// Give the next round a fresh chance; the fail counter ends a poison update.
		_ = p.st.KV().KVDelete(ctx, mark, 0)
		p.mu.Lock()
		p.fails[u.UpdateID]++
		n := p.fails[u.UpdateID]
		p.mu.Unlock()
		if n >= maxFails {
			p.log.Error("bot: the update is skipped after repeated failures", "update", u.UpdateID, "err", err)
			_, _ = p.st.MarkOnce(ctx, mark, 48*time.Hour) // skip it for good
			p.mu.Lock()
			delete(p.fails, u.UpdateID)
			p.mu.Unlock()
			p.processed.Add(1)
			return nil
		}
		return err
	}
	p.mu.Lock()
	delete(p.fails, u.UpdateID) // a success ends the failure streak
	p.mu.Unlock()
	p.processed.Add(1)
	return nil
}

// dispatch routes one update: private chats only, commands first, then the engine.
func (p *Poller) dispatch(ctx context.Context, cl *telegram.Client, u telegram.Update) error {
	m := u.Message
	if m == nil || m.From == nil {
		return nil // a kind we do not subscribe to; ack
	}
	if m.From.IsBot || m.Chat.Type != "private" {
		return nil // bots and groups are not support conversations
	}
	in := tickets.Inbound{TgID: m.From.ID, ChatID: m.Chat.ID, TgMsgID: m.MessageID,
		Username: m.From.Username, FirstName: m.From.FirstName, LastName: m.From.LastName,
		Lang: m.From.LanguageCode}

	text := strings.TrimSpace(m.Text)
	if text != "" && strings.HasPrefix(text, "/") {
		return p.command(ctx, cl, text, in)
	}
	switch {
	case len(m.Photo) > 0:
		ph := m.Photo[len(m.Photo)-1] // the largest
		in.Kind, in.FileID, in.FileSize, in.Text = store.KindPhoto, ph.FileID, ph.FileSize, m.Caption
		in.FileMime = "image/jpeg"
	case m.Document != nil:
		in.Kind, in.FileID, in.FileSize = store.KindDocument, m.Document.FileID, m.Document.FileSize
		in.FileName, in.FileMime, in.Text = m.Document.FileName, m.Document.MimeType, m.Caption
	case m.Sticker != nil:
		in.Kind, in.FileID, in.Text = store.KindSticker, m.Sticker.FileID, m.Sticker.Emoji
	case text != "":
		in.Kind, in.Text = store.KindText, text
	default:
		// A kind the bot does not read yet (audio, video, voice): tell the user once
		// what to do instead of staying silent, then ack.
		ru := strings.HasPrefix(strings.ToLower(in.Lang), "ru")
		if ru {
			_ = cl.SendText(ctx, in.ChatID, "Такие сообщения бот пока не принимает — опишите проблему текстом или пришлите фото/файл.")
		} else {
			_ = cl.SendText(ctx, in.ChatID, "The bot cannot take this kind of message yet — describe the issue in text or send a photo/file.")
		}
		return nil
	}
	_, err := p.eng.HandleUserMessage(ctx, in)
	return err
}

// command is /start, /status, /close and the help for anything else. The @botname
// suffix of a group-style command is accepted and stripped.
func (p *Poller) command(ctx context.Context, cl *telegram.Client, text string, in tickets.Inbound) error {
	cmd, _, _ := strings.Cut(text, "\n")
	cmd, _, _ = strings.Cut(strings.TrimSpace(cmd), " ")
	cmd = strings.ToLower(cmd)
	if i := strings.Index(cmd, "@"); i > 0 {
		cmd = cmd[:i]
	}
	lang := in.Lang
	cfg, err := p.eng.Cfg(ctx)
	if err != nil {
		return err
	}
	switch cmd {
	case "/start", "/help":
		g, err := p.eng.Greeting(ctx, cfg, in.TgID, lang)
		if err != nil {
			return err
		}
		return cl.SendText(ctx, in.ChatID, g)
	case "/status":
		s, err := p.eng.UserStatus(ctx, in.TgID, lang)
		if err != nil {
			return err
		}
		return cl.SendText(ctx, in.ChatID, s)
	case "/close":
		_, closed, err := p.eng.CloseByUser(ctx, in.TgID)
		if err != nil {
			return err
		}
		if !closed {
			_, _, _, _, _, noOpen, _, _ := tickets.Texts(lang)
			return cl.SendText(ctx, in.ChatID, noOpen)
		}
		return nil // CloseByUser told the user already
	default:
		if strings.HasPrefix(strings.ToLower(lang), "ru") {
			return cl.SendText(ctx, in.ChatID, "Напишите свой вопрос текстом — создастся тикет, и команда поддержки ответит здесь.\nКоманды: /start, /status, /close")
		}
		return cl.SendText(ctx, in.ChatID, "Write your question as a message — a ticket is created and support answers here.\nCommands: /start, /status, /close")
	}
}
