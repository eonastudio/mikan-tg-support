// Package tickets is the engine between the three sides of the addon: the user in
// Telegram, the agent on the panel's page and the AI. It owns what a message does —
// open a ticket, append, notify, schedule an auto answer — and nothing of how the
// sides talk (that is internal/telegram, internal/ai and internal/srv).
package tickets

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"tg-support/internal/ai"
	"tg-support/internal/store"
)

// Sender sends to Telegram; *telegram.Client satisfies it (tests fake it).
type Sender interface {
	SendText(ctx context.Context, chatID int64, text string) error
	SendChatAction(ctx context.Context, chatID int64)
}

// LLM completes a context; *ai.Client satisfies it.
type LLM interface {
	Complete(ctx context.Context, msgs []ai.ChatMessage) (text, model string, ms int64, err error)
}

// Config is what the engine reads of the live settings.
type Config func(ctx context.Context) (store.Config, error)

// Engine runs the ticket flow. One Engine is safe for concurrent use (the poller and
// the pages call it at the same time).
type Engine struct {
	st  *store.Store
	cfg Config
	log *slog.Logger

	// send builds the Telegram sender for a config; nil cfg tokens give nil.
	send func(store.Config) Sender
	llm  func(store.Config) LLM

	mu     sync.Mutex
	timers map[int64]*time.Timer // scheduled AI auto answers, by ticket
	flood  map[int64]*burst      // per user
	warned map[int64]time.Time   // a flood warning was sent at

	aiMu    sync.Mutex
	aiStats AIStats
}

// AIStats is what the status page shows of the AI (in memory, since the start).
type AIStats struct {
	AutoSent   int    `json:"auto_sent"`
	AutoFailed int    `json:"auto_failed"`
	Drafts     int    `json:"drafts"`
	LastModel  string `json:"last_model,omitempty"`
	LastError  string `json:"last_error,omitempty"`
	LastAt     int64  `json:"last_at,omitempty"`
}

// New is an Engine on the store; send and llm build the clients per call from the
// live config (a token or a model change reaches the next message, no restart).
func New(st *store.Store, cfg Config, send func(store.Config) Sender, llm func(store.Config) LLM, log *slog.Logger) *Engine {
	return &Engine{st: st, cfg: cfg, send: send, llm: llm, log: log,
		timers: map[int64]*time.Timer{}, flood: map[int64]*burst{}, warned: map[int64]time.Time{}}
}

// AIStats is a snapshot for the status page.
func (e *Engine) AIStats() AIStats {
	e.aiMu.Lock()
	defer e.aiMu.Unlock()
	return e.aiStats
}

// Cfg is the live config (the bot's commands read the greeting through it).
func (e *Engine) Cfg(ctx context.Context) (store.Config, error) { return e.cfg(ctx) }

// Store exposes the store for the server layer (the pages read tickets through it).
func (e *Engine) Store() *store.Store { return e.st }

// Inbound is one user message the poller hands over.
type Inbound struct {
	TgID, ChatID, TgMsgID               int64
	Username, FirstName, LastName, Lang string
	Kind                                string // text | photo | document | sticker
	Text                                string
	FileID, FileMime, FileName          string
	FileSize                            int64
}

// flood limits: a burst of 4, then one message per 2 s; a warned user hears about it
// at most once per 10 minutes.
type burst struct {
	tokens float64
	last   time.Time
}

const (
	floodBurst  = 4.0
	floodRate   = 0.5 // tokens per second
	floodWarnAt = 10 * time.Minute
	maxFileSize = 20 << 20
	// maxTicketMessages bounds one conversation: past it the ticket closes and the
	// next message opens a new one (the KV budget and the dialog view stay sane).
	maxTicketMessages = 5000
)

func (e *Engine) allow(tgID int64, now time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.flood) > 8192 { // an old crowd must not grow the maps without end
		for id, b := range e.flood {
			if now.Sub(b.last) > time.Hour {
				delete(e.flood, id)
				delete(e.warned, id)
			}
		}
	}
	b := e.flood[tgID]
	if b == nil {
		b = &burst{tokens: floodBurst, last: now}
		e.flood[tgID] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * floodRate
	b.last = now
	if b.tokens > floodBurst {
		b.tokens = floodBurst
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (e *Engine) warnDue(tgID int64, now time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if last, ok := e.warned[tgID]; ok && now.Sub(last) < floodWarnAt {
		return false
	}
	e.warned[tgID] = now
	return true
}

// humanWords are what a user says to skip the AI (whole words, case-insensitive).
var humanWords = []string{
	"оператор", "человек", "человека", "живой", "живого", "позовите", "переключите",
	"human", "operator", "live person", "real person", "agent please",
}

func asksHuman(text string) bool {
	t := strings.ToLower(text)
	for _, w := range humanWords {
		if strings.Contains(t, w) {
			return true
		}
	}
	return false
}

// Texts are the bot's lines, ru/en by the user's Telegram language: created,
// closedByUser, closedByAgent, reopened, flood, noOpen, tooBig, humanNote.
func Texts(lang string) (created, closedByUser, closedByAgent, reopened, flood, noOpen, tooBig, humanNote string) {
	if strings.HasPrefix(strings.ToLower(lang), "ru") {
		return "🎫 Тикет #%d создан. Команда поддержки ответит здесь же.\n/status — мои тикеты, /close — закрыть тикет.",
			"Тикет #%d закрыт. Если вопрос снова станет актуальным — просто напишите, откроется новый тикет.",
			"✅ Тикет #%d закрыт поддержкой. Спасибо! Напишите нам, если понадобится помощь.",
			"Тикет #%d снова открыт.",
			"Слишком много сообщений подряд — пожалуйста, пишите чуть медленнее.",
			"У вас нет открытых тикетов. Напишите свой вопрос — и тикет создастся автоматически.",
			"Файл больше 20 МБ не принимается; опишите проблему текстом или пришлите файл меньше.",
			"Понял, зову человека — специалист ответит здесь же. До этого момента бот не будет отвечать автоматически."
	}
	return "🎫 Ticket #%d created. The support team will answer right here.\n/status — my tickets, /close — close the ticket.",
		"Ticket #%d closed. If the matter comes up again, just write — a new ticket will open.",
		"✅ Ticket #%d closed by support. Thank you! Write us if you need help again.",
		"Ticket #%d reopened.",
		"Too many messages in a row — please slow down a bit.",
		"You have no open tickets. Write your question and a ticket is created automatically.",
		"Files over 20 MB are not accepted; describe the issue in text or send a smaller file.",
		"Got it, calling a human — a specialist will answer right here. Until then the bot will not answer automatically."
}

// HandleUserMessage is the poller's handover: create or continue the user's ticket.
// The answer is the ticket the message landed in (for the poller's log). Delivery of
// the confirmations is best effort: a failed notice does not fail the message.
func (e *Engine) HandleUserMessage(ctx context.Context, in Inbound) (store.Ticket, error) {
	if in.TgID == 0 || in.ChatID == 0 {
		return store.Ticket{}, errors.New("tickets: a message without a user")
	}
	now := time.Now()
	cfg, err := e.cfg(ctx)
	if err != nil {
		return store.Ticket{}, err
	}
	created, _, _, _, flood, _, tooBig, humanNote := Texts(in.Lang)

	sender := e.send(cfg)
	if !e.allow(in.TgID, now) {
		if sender != nil && e.warnDue(in.TgID, now) {
			_ = sender.SendText(ctx, in.ChatID, flood)
		}
		return store.Ticket{}, nil // dropped, on purpose
	}
	if in.FileSize > maxFileSize {
		if sender != nil {
			_ = sender.SendText(ctx, in.ChatID, tooBig)
		}
		return store.Ticket{}, nil
	}

	t, open, err := e.st.OpenTicketFor(ctx, in.TgID)
	if err != nil {
		return store.Ticket{}, err
	}
	m := store.Message{From: store.FromUser, Kind: in.Kind, Text: in.Text, TgMsgID: in.TgMsgID,
		FileID: in.FileID, FileMime: in.FileMime, FileName: in.FileName, FileSize: in.FileSize, At: now.Unix()}
	if m.Kind == "" {
		m.Kind = store.KindText
	}

	human := asksHuman(in.Text)
	if open && t.MsgCount >= maxTicketMessages {
		// An endless conversation does not grow one ticket without bound: the old
		// one closes and the message opens a fresh one (the KV budget stays sane).
		if _, err := e.st.CloseTicket(ctx, t.ID); err != nil {
			return store.Ticket{}, err
		}
		e.cancelAI(t.ID)
		_ = e.st.AppendMessage(ctx, t.ID, store.Message{From: store.FromSystem, Kind: store.KindService,
			Text: "the ticket reached its message limit; the conversation continues in a new one", At: now.Unix()})
		if sender != nil {
			if strings.HasPrefix(strings.ToLower(in.Lang), "ru") {
				_ = sender.SendText(ctx, t.ChatID, fmt.Sprintf("Тикет #%d достиг лимита сообщений и закрыт — ваше обращение открыто в новом тикете.", t.ID))
			} else {
				_ = sender.SendText(ctx, t.ChatID, fmt.Sprintf("Ticket #%d reached its message limit and was closed — your request continues in a new ticket.", t.ID))
			}
		}
		open = false
	}
	if !open {
		t, err = e.st.CreateTicket(ctx, store.NewTicket{TgID: in.TgID, ChatID: in.ChatID, Username: in.Username,
			FirstName: in.FirstName, LastName: in.LastName, Lang: in.Lang}, m)
		if err != nil {
			return store.Ticket{}, err
		}
		if sender != nil {
			_ = sender.SendText(ctx, in.ChatID, fmt.Sprintf(created, t.ID))
		}
	} else {
		// A user who moved (rare) keeps the ticket at the old chat; the bot answers
		// where the ticket was opened. Update the pointer chat only on a private move.
		if err := e.st.AppendMessage(ctx, t.ID, m); err != nil {
			return store.Ticket{}, err
		}
	}
	if human {
		if _, err := e.st.UpdateTicket(ctx, t.ID, func(t *store.Ticket) error {
			t.NoAI = true
			return nil
		}); err != nil {
			e.log.Warn("tickets: the human flag", "ticket", t.ID, "err", err)
		} else {
			t.NoAI = true
			_ = e.st.AppendMessage(ctx, t.ID, store.Message{From: store.FromSystem, Kind: store.KindService,
				Text: "user asked for a human", At: now.Unix()})
			if sender != nil {
				_ = sender.SendText(ctx, t.ChatID, humanNote)
			}
			e.cancelAI(t.ID)
		}
	}
	e.scheduleAI(ctx, t, cfg)
	return t, nil
}

// scheduleAI arms the auto answer when the settings want one.
func (e *Engine) scheduleAI(ctx context.Context, t store.Ticket, cfg store.Config) {
	if !cfg.AIReady() || cfg.AIMode != store.AIAuto || t.NoAI {
		return
	}
	if e.llm(cfg) == nil {
		return
	}
	delay := cfg.AIDelay()
	e.mu.Lock()
	if _, armed := e.timers[t.ID]; armed {
		e.mu.Unlock()
		return // an answer is already on the way for this ticket
	}
	tm := time.AfterFunc(delay, func() {
		e.mu.Lock()
		delete(e.timers, t.ID)
		e.mu.Unlock()
		e.AutoAnswer(context.WithoutCancel(ctx), t.ID)
	})
	e.timers[t.ID] = tm
	e.mu.Unlock()
}

// cancelAI disarms a scheduled auto answer (an agent replied, the user asked for a
// human, the ticket closed).
func (e *Engine) cancelAI(id int64) {
	e.mu.Lock()
	if tm, ok := e.timers[id]; ok {
		tm.Stop()
		delete(e.timers, id)
	}
	e.mu.Unlock()
}

// Sweep arms auto answers for open tickets whose user message aged past the delay
// with nobody answering — what a restart left behind. Run it at the start and every
// minute; it is cheap (the index) and idempotent (scheduleAI dedupes by ticket).
func (e *Engine) Sweep(ctx context.Context) {
	cfg, err := e.cfg(ctx)
	if err != nil || !cfg.AIReady() || cfg.AIMode != store.AIAuto || e.llm(cfg) == nil {
		return
	}
	heads, err := e.st.ListTickets(ctx, store.StatusOpen)
	if err != nil {
		e.log.Debug("tickets: sweep", "err", err)
		return
	}
	delay := cfg.AIDelay()
	now := time.Now().Unix()
	for _, h := range heads {
		if h.LastFrom != store.FromUser || h.Unread == 0 {
			continue
		}
		t, err := e.st.Ticket(ctx, h.ID)
		if err != nil || t.NoAI || t.Status != store.StatusOpen || t.LastFrom != store.FromUser {
			continue
		}
		if now-t.LastMsgAt < int64(delay.Seconds()) {
			continue
		}
		e.scheduleAI(ctx, t, cfg)
	}
}

// AutoAnswer is the scheduled AI reply: it fires only while the ticket still waits
// (open, the last word the user's, no human asked), generates, re-checks (an agent
// may have answered during the generation) and sends. Every outcome is counted for
// the status page; a failure is silent to the user (a human will come anyway).
func (e *Engine) AutoAnswer(ctx context.Context, id int64) {
	cfg, err := e.cfg(ctx)
	if err != nil || !cfg.AIEnabled || cfg.AIMode != store.AIAuto {
		return
	}
	model := e.llm(cfg)
	sender := e.send(cfg)
	if model == nil || sender == nil {
		return
	}
	if ok := e.autoStillDue(ctx, id); !ok {
		return
	}
	text, modelName, ms, err := e.answer(ctx, cfg, id)
	if err != nil {
		e.log.Info("tickets: the AI answer failed", "ticket", id, "err", err)
		e.aiMu.Lock()
		e.aiStats.AutoFailed++
		e.aiStats.LastError = err.Error()
		e.aiStats.LastAt = time.Now().Unix()
		e.aiMu.Unlock()
		return
	}
	if ok := e.autoStillDue(ctx, id); !ok {
		return // an agent answered while the model was thinking
	}
	t, err := e.st.Ticket(ctx, id)
	if err != nil {
		return
	}
	if err := sender.SendText(ctx, t.ChatID, text); err != nil {
		e.log.Warn("tickets: the AI answer did not send", "ticket", id, "err", err)
		e.aiMu.Lock()
		e.aiStats.AutoFailed++
		e.aiStats.LastError = err.Error()
		e.aiMu.Unlock()
		return
	}
	if err := e.st.AppendMessage(ctx, id, store.Message{From: store.FromAI, Kind: store.KindText,
		Text: text, AIModel: modelName, AIMs: ms, At: time.Now().Unix()}); err != nil {
		e.log.Warn("tickets: the AI answer did not store", "ticket", id, "err", err)
	}
	e.aiMu.Lock()
	e.aiStats.AutoSent++
	e.aiStats.LastModel = modelName
	e.aiStats.LastAt = time.Now().Unix()
	e.aiStats.LastError = ""
	e.aiMu.Unlock()
}

// autoStillDue re-checks that nobody answered while the AI worked.
func (e *Engine) autoStillDue(ctx context.Context, id int64) bool {
	t, err := e.st.Ticket(ctx, id)
	if err != nil {
		return false
	}
	return t.Status == store.StatusOpen && t.LastFrom == store.FromUser && !t.NoAI
}

// answer builds the context of a ticket and asks the model.
func (e *Engine) answer(ctx context.Context, cfg store.Config, id int64) (string, string, int64, error) {
	t, err := e.st.Ticket(ctx, id)
	if err != nil {
		return "", "", 0, err
	}
	msgs, err := e.st.LastMessages(ctx, id, ai.MaxHistory)
	if err != nil {
		return "", "", 0, err
	}
	lang := cfg.AILang
	if lang == "" {
		lang = t.Lang
	}
	// The service's own language is Russian; anything not English becomes it.
	if !strings.HasPrefix(strings.ToLower(lang), "en") {
		lang = "ru"
	}
	history := make([]ai.Message, 0, len(msgs))
	for _, m := range msgs {
		history = append(history, ai.Message{From: m.From, Kind: m.Kind, Text: m.Text, FileName: m.FileName})
	}
	facts := ai.Facts{Username: t.Username, FirstName: t.FirstName}
	if p, err := e.st.Profile(ctx, t.TgID); err == nil {
		facts.ExpiresAt, facts.DaysLeft, facts.Expired = p.ExpiresAt, p.DaysLeft, p.Expired
		facts.Traffic90, facts.Traffic100 = p.Traffic90, p.Traffic100
		for _, pay := range p.Payments {
			line := fmt.Sprintf("%s, %s %.2f", time.Unix(pay.At, 0).UTC().Format("2006-01-02"),
				strings.ToUpper(pay.Currency), float64(pay.Amount)/100)
			if pay.Kind != "" {
				line += " (" + pay.Kind + ")"
			}
			if pay.Refunded {
				line += " — refunded"
			}
			facts.Payments = append(facts.Payments, line)
		}
	}
	ctxMsgs := []ai.ChatMessage{{Role: "system", Content: ai.System(lang, cfg.AIKnowledge, cfg.AISystem, facts)}}
	ctxMsgs = append(ctxMsgs, ai.History(history, lang)...)
	text, model, ms, err := e.llm(cfg).Complete(ctx, ctxMsgs)
	if err != nil {
		return "", "", 0, err
	}
	if strings.TrimSpace(text) == "" {
		return "", "", 0, errors.New("ai: the model answered with nothing")
	}
	return text, model, ms, nil
}

// Draft is the AI answer without sending it (the page's «AI draft» button, mode draft).
func (e *Engine) Draft(ctx context.Context, id int64) (string, error) {
	cfg, err := e.cfg(ctx)
	if err != nil {
		return "", err
	}
	if e.llm(cfg) == nil {
		return "", errors.New("tickets: the AI is not configured")
	}
	text, model, _, err := e.answer(ctx, cfg, id)
	if err != nil {
		e.aiMu.Lock()
		e.aiStats.LastError = err.Error()
		e.aiStats.LastAt = time.Now().Unix()
		e.aiMu.Unlock()
		return "", err
	}
	e.aiMu.Lock()
	e.aiStats.Drafts++
	e.aiStats.LastModel = model
	e.aiStats.LastAt = time.Now().Unix()
	e.aiStats.LastError = ""
	e.aiMu.Unlock()
	return text, nil
}

// AgentReply sends the agent's text to the user and stores it. Telegram goes first:
// a refusal (the user blocked the bot) reaches the page as an error and nothing is
// stored that the user never saw. The ticket reopens when it was closed.
func (e *Engine) AgentReply(ctx context.Context, id int64, text string) (store.Ticket, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return store.Ticket{}, errors.New("tickets: an empty reply")
	}
	if n := len([]rune(text)); n > store.MaxTextRunes {
		return store.Ticket{}, fmt.Errorf("tickets: the reply is %d characters, the limit is %d", n, store.MaxTextRunes)
	}
	cfg, err := e.cfg(ctx)
	if err != nil {
		return store.Ticket{}, err
	}
	sender := e.send(cfg)
	if sender == nil {
		return store.Ticket{}, errors.New("tickets: the bot is not configured")
	}
	t, err := e.st.Ticket(ctx, id)
	if err != nil {
		return store.Ticket{}, err
	}
	e.cancelAI(id)
	if err := sender.SendText(ctx, t.ChatID, text); err != nil {
		return t, err
	}
	if t.Status == store.StatusClosed {
		if t, err = e.st.ReopenTicket(ctx, id); err != nil {
			return t, err
		}
		_, _, _, reopened, _, _, _, _ := Texts(t.Lang)
		_ = sender.SendText(ctx, t.ChatID, fmt.Sprintf(reopened, t.ID))
	}
	m := store.Message{From: store.FromSupport, Kind: store.KindText, Text: text, At: time.Now().Unix()}
	if err := e.st.AppendMessage(ctx, id, m); err != nil {
		return t, err
	}
	if t, err = e.st.Ticket(ctx, id); err != nil {
		return t, err
	}
	return t, nil
}

// AgentSendAI sends a finished AI text to the user as a support answer of kind ai
// (the page's «send the AI answer» in auto/draft mode).
func (e *Engine) AgentSendAI(ctx context.Context, id int64, text string) (store.Ticket, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return store.Ticket{}, errors.New("tickets: an empty answer")
	}
	cfg, err := e.cfg(ctx)
	if err != nil {
		return store.Ticket{}, err
	}
	sender := e.send(cfg)
	if sender == nil {
		return store.Ticket{}, errors.New("tickets: the bot is not configured")
	}
	t, err := e.st.Ticket(ctx, id)
	if err != nil {
		return store.Ticket{}, err
	}
	e.cancelAI(id)
	if err := sender.SendText(ctx, t.ChatID, text); err != nil {
		return t, err
	}
	if t.Status == store.StatusClosed {
		if t, err = e.st.ReopenTicket(ctx, id); err != nil {
			return t, err
		}
	}
	m := store.Message{From: store.FromAI, Kind: store.KindText, Text: text, At: time.Now().Unix()}
	if err := e.st.AppendMessage(ctx, id, m); err != nil {
		return t, err
	}
	return e.st.Ticket(ctx, id)
}

// Close closes a ticket from the page and tells the user.
func (e *Engine) Close(ctx context.Context, id int64) (store.Ticket, error) {
	t, err := e.st.CloseTicket(ctx, id)
	if err != nil {
		return store.Ticket{}, err
	}
	e.cancelAI(id)
	_ = e.st.AppendMessage(ctx, id, store.Message{From: store.FromSystem, Kind: store.KindService,
		Text: "closed by support", At: time.Now().Unix()})
	cfg, cerr := e.cfg(ctx)
	if cerr == nil {
		if sender := e.send(cfg); sender != nil {
			_, _, closedByAgent, _, _, _, _, _ := Texts(t.Lang)
			_ = sender.SendText(ctx, t.ChatID, fmt.Sprintf(closedByAgent, t.ID))
		}
	}
	return e.st.Ticket(ctx, id)
}

// Reopen reopens a ticket from the page and tells the user.
func (e *Engine) Reopen(ctx context.Context, id int64) (store.Ticket, error) {
	t, err := e.st.ReopenTicket(ctx, id)
	if err != nil {
		return store.Ticket{}, err
	}
	_ = e.st.AppendMessage(ctx, id, store.Message{From: store.FromSystem, Kind: store.KindService,
		Text: "reopened by support", At: time.Now().Unix()})
	cfg, cerr := e.cfg(ctx)
	if cerr == nil {
		if sender := e.send(cfg); sender != nil {
			_, _, _, reopened, _, _, _, _ := Texts(t.Lang)
			_ = sender.SendText(ctx, t.ChatID, fmt.Sprintf(reopened, t.ID))
		}
	}
	return e.st.Ticket(ctx, id)
}

// CloseByUser closes the user's own open ticket (the bot's /close).
func (e *Engine) CloseByUser(ctx context.Context, tgID int64) (store.Ticket, bool, error) {
	t, open, err := e.st.OpenTicketFor(ctx, tgID)
	if err != nil || !open {
		return store.Ticket{}, false, err
	}
	if _, err := e.st.CloseTicket(ctx, t.ID); err != nil {
		return store.Ticket{}, false, err
	}
	e.cancelAI(t.ID)
	_ = e.st.AppendMessage(ctx, t.ID, store.Message{From: store.FromSystem, Kind: store.KindService,
		Text: "closed by the user", At: time.Now().Unix()})
	cfg, cerr := e.cfg(ctx)
	if cerr == nil {
		if sender := e.send(cfg); sender != nil {
			_, closedByUser, _, _, _, _, _, _ := Texts(t.Lang)
			_ = sender.SendText(ctx, t.ChatID, fmt.Sprintf(closedByUser, t.ID))
		}
	}
	nt, err := e.st.Ticket(ctx, t.ID)
	return nt, true, err
}

// UserStatus is the bot's /status: the user's open ticket and their last closed ones.
func (e *Engine) UserStatus(ctx context.Context, tgID int64, lang string) (string, error) {
	ru := !strings.HasPrefix(strings.ToLower(lang), "en")
	t, open, err := e.st.OpenTicketFor(ctx, tgID)
	if err != nil {
		return "", err
	}
	if !open {
		_, _, _, _, _, noOpen, _, _ := Texts(lang)
		return noOpen, nil
	}
	var b strings.Builder
	if ru {
		fmt.Fprintf(&b, "Открыт тикет #%d\n", t.ID)
		if t.LastFrom == store.FromUser {
			b.WriteString("Статус: ждём ответа поддержки.\n")
		} else {
			b.WriteString("Статус: поддержка ответила — проверьте сообщения выше.\n")
		}
		fmt.Fprintf(&b, "Сообщений: %d, создан: %s\n", t.MsgCount,
			time.Unix(t.CreatedAt, 0).UTC().Format("2006-01-02 15:04"))
		b.WriteString("/close — закрыть тикет.")
	} else {
		fmt.Fprintf(&b, "Open ticket #%d\n", t.ID)
		if t.LastFrom == store.FromUser {
			b.WriteString("Status: waiting for support.\n")
		} else {
			b.WriteString("Status: support has answered — see the messages above.\n")
		}
		fmt.Fprintf(&b, "Messages: %d, created: %s\n", t.MsgCount,
			time.Unix(t.CreatedAt, 0).UTC().Format("2006-01-02 15:04"))
		b.WriteString("/close — close the ticket.")
	}
	return b.String(), nil
}

// Greeting is the bot's /start answer with a hint when a ticket is open.
func (e *Engine) Greeting(ctx context.Context, cfg store.Config, tgID int64, lang string) (string, error) {
	g := cfg.Greeting
	if g == "" {
		if strings.HasPrefix(strings.ToLower(lang), "ru") {
			g = store.DefaultGreetingRU
		} else {
			g = store.DefaultGreetingEN
		}
	}
	t, open, err := e.st.OpenTicketFor(ctx, tgID)
	if err == nil && open {
		if strings.HasPrefix(strings.ToLower(lang), "ru") {
			g += fmt.Sprintf("\n\nУ вас есть открытый тикет #%d — просто напишите, сообщение попадёт в него.", t.ID)
		} else {
			g += fmt.Sprintf("\n\nYou have an open ticket #%d — just write, and the message goes into it.", t.ID)
		}
	}
	return g, nil
}
