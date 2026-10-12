package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"tg-support/internal/panelapi"
)

// Ticket statuses.
const (
	StatusOpen   = "open"
	StatusClosed = "closed"
)

// Who wrote a message.
const (
	FromUser    = "user"
	FromSupport = "support"
	FromAI      = "ai"
	FromSystem  = "system"
)

// Message kinds.
const (
	KindText     = "text"
	KindPhoto    = "photo"
	KindDocument = "document"
	KindSticker  = "sticker"
	KindService  = "service"
)

// Ticket is one support conversation with one Telegram user.
type Ticket struct {
	ID        int64  `json:"id"`
	TgID      int64  `json:"tg_id"`
	ChatID    int64  `json:"chat_id"` // where the bot answers (a private chat: the user's id)
	Username  string `json:"username,omitempty"`
	FirstName string `json:"first_name,omitempty"`
	LastName  string `json:"last_name,omitempty"`
	Lang      string `json:"lang,omitempty"` // the user's Telegram language
	Subject   string `json:"subject"`
	Status    string `json:"status"`          // open | closed
	NoAI      bool   `json:"no_ai,omitempty"` // the user asked for a human
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
	LastMsgAt int64  `json:"last_msg_at"`
	LastFrom  string `json:"last_from,omitempty"`
	MsgCount  int    `json:"msg_count"`
	Unread    int    `json:"unread,omitempty"` // user messages no agent has seen
	Seq       int64  `json:"seq"`              // the seq of the last message
	LastChunk int    `json:"last_chunk"`       // where the next message goes
}

// Message is one line of a ticket's history. TgMsgID makes a replayed Telegram
// update (the poller crashed before it marked the update) an idempotent no-op.
type Message struct {
	Seq      int64  `json:"seq"`
	At       int64  `json:"at"`
	From     string `json:"from"` // user | support | ai | system
	Kind     string `json:"kind"` // text | photo | document | sticker | service
	Text     string `json:"text,omitempty"`
	TgMsgID  int64  `json:"tg_msg_id,omitempty"`
	FileID   string `json:"file_id,omitempty"`
	FileMime string `json:"file_mime,omitempty"`
	FileSize int64  `json:"file_size,omitempty"`
	FileName string `json:"file_name,omitempty"`
	AIModel  string `json:"ai_model,omitempty"`
	AIMs     int64  `json:"ai_ms,omitempty"`
}

// TicketHead is a ticket as the index and the list page keep it: enough to show a
// line, small enough that indexCap of them fit one KV value.
type TicketHead struct {
	ID        int64  `json:"id"`
	Status    string `json:"status"`
	TgID      int64  `json:"tg_id"`
	Username  string `json:"username,omitempty"`
	FirstName string `json:"first_name,omitempty"`
	Subject   string `json:"subject"`
	UpdatedAt int64  `json:"updated_at"`
	LastFrom  string `json:"last_from,omitempty"`
	Unread    int    `json:"unread,omitempty"`
	MsgCount  int    `json:"msg_count"`
}

// index is idx/tickets: heads sorted by UpdatedAt desc, capped.
type index struct {
	Heads []TicketHead `json:"heads"`
}

const (
	indexKey   = "idx/tickets"
	indexCap   = 300      // heads in the index
	subjectCap = 120      // runes in a head's subject
	chunkFlush = 48 << 10 // a chunk rolls over near this size (the KV limit is 64 KiB)
	chunkCap   = 200      // and at this many messages
	msgTextCap = MaxTextRunes
)

// MaxTextRunes is Telegram's own message limit; longer input is clipped.
const MaxTextRunes = 4096

// clipText trims a stored text to msgTextCap runes (a byte cut could land mid-rune).
func clipText(s string) string {
	s = strings.ToValidUTF8(s, "")
	r := []rune(s)
	if len(r) > msgTextCap {
		r = r[:msgTextCap]
	}
	return string(r)
}

// clipName bounds an identity or file field the store keeps: Telegram has its own
// limits, but the store does not trust another service's limits with its KV budget.
func clipName(s string, n int) string {
	s = strings.ToValidUTF8(s, "")
	r := []rune(s)
	if len(r) > n {
		r = r[:n]
	}
	return string(r)
}

// clipSubject is the first line of a first message, short.
func clipSubject(s string) string {
	s = strings.TrimSpace(strings.SplitN(s, "\n", 2)[0])
	r := []rune(s)
	if len(r) > subjectCap {
		r = r[:subjectCap]
	}
	if s = strings.TrimSpace(string(r)); s == "" {
		return "…"
	}
	return s
}

func head(t *Ticket) TicketHead {
	return TicketHead{ID: t.ID, Status: t.Status, TgID: t.TgID, Username: t.Username, FirstName: t.FirstName,
		Subject: clipSubject(t.Subject), UpdatedAt: t.UpdatedAt, LastFrom: t.LastFrom, Unread: t.Unread, MsgCount: t.MsgCount}
}

// nextTicketID bumps seq/tickets under CAS.
func (s *Store) nextTicketID(ctx context.Context) (int64, error) {
	var id int64
	err := s.kv.CAS(ctx, "seq/tickets", 8, func(val []byte, ver int64) (panelapi.Update, error) {
		cur, _ := strconv.ParseInt(string(val), 10, 64)
		id = cur + 1
		return panelapi.Update{Val: []byte(strconv.FormatInt(id, 10))}, nil
	})
	return id, err
}

// NewTicket is who a ticket is created for.
type NewTicket struct {
	TgID, ChatID        int64
	Username, FirstName string
	LastName, Lang      string
}

// CreateTicket registers a new ticket with its first user message. The message's text
// (or its kind for media) becomes the subject.
func (s *Store) CreateTicket(ctx context.Context, nt NewTicket, first Message) (Ticket, error) {
	id, err := s.nextTicketID(ctx)
	if err != nil {
		return Ticket{}, err
	}
	now := s.now().Unix()
	first.From = FromUser
	if first.At == 0 {
		first.At = now
	}
	first.Seq = 1
	first.Text = clipText(first.Text)
	first.FileName = clipName(first.FileName, 256)
	first.FileMime = clipName(first.FileMime, 64)
	first.FileID = clipName(first.FileID, 256)
	subject := first.Text
	if subject == "" {
		subject = kindName(first.Kind)
	}
	t := Ticket{ID: id, TgID: nt.TgID, ChatID: nt.ChatID,
		Username: clipName(nt.Username, 64), FirstName: clipName(nt.FirstName, 128),
		LastName: clipName(nt.LastName, 128), Lang: clipName(nt.Lang, 16),
		Subject: clipSubject(subject), Status: StatusOpen,
		CreatedAt: now, UpdatedAt: now, LastMsgAt: first.At, LastFrom: FromUser, MsgCount: 1, Unread: 1,
		Seq: 1, LastChunk: 0}
	if err := putJSON(ctx, s, ticketKey(id), &t, 0); err != nil {
		return Ticket{}, err
	}
	if err := putJSON(ctx, s, chunkKey(id, 0), &chunk{Msgs: []Message{first}}, 0); err != nil {
		return Ticket{}, err
	}
	if _, err := s.kv.KVPut(ctx, userTicketKey(nt.TgID), []byte(strconv.FormatInt(id, 10)), panelapi.PutOpts{}); err != nil {
		return Ticket{}, err
	}
	if err := s.touchIndex(ctx, &t); err != nil {
		return Ticket{}, err
	}
	return t, nil
}

func kindName(kind string) string {
	switch kind {
	case KindPhoto:
		return "📷 фото"
	case KindDocument:
		return "📄 файл"
	case KindSticker:
		return "🎭 стикер"
	}
	return "сообщение"
}

// Ticket reads one ticket.
func (s *Store) Ticket(ctx context.Context, id int64) (Ticket, error) {
	var t Ticket
	if _, err := getJSON(ctx, s, ticketKey(id), &t); err != nil {
		return Ticket{}, err
	}
	return t, nil
}

// UpdateTicket mutates a ticket under CAS and refreshes its index head. fn returns an
// error to abort (the ticket stays as it was).
func (s *Store) UpdateTicket(ctx context.Context, id int64, fn func(t *Ticket) error) (Ticket, error) {
	var out Ticket
	err := casJSON(ctx, s, ticketKey(id), func(cur *Ticket) (*Ticket, error) {
		if cur == nil {
			return nil, ErrNotFound
		}
		t := *cur
		if err := fn(&t); err != nil {
			return nil, err
		}
		t.UpdatedAt = s.now().Unix()
		out = t
		return &t, nil
	})
	if err != nil {
		return Ticket{}, err
	}
	if err := s.touchIndex(ctx, &out); err != nil {
		return out, err
	}
	return out, nil
}

// OpenTicketFor is the user's open ticket, if they have one.
func (s *Store) OpenTicketFor(ctx context.Context, tgID int64) (Ticket, bool, error) {
	data, _, err := s.kv.KVGet(ctx, userTicketKey(tgID))
	if errors.Is(err, panelapi.ErrNotFound) {
		return Ticket{}, false, nil
	}
	if err != nil {
		return Ticket{}, false, err
	}
	id, _ := strconv.ParseInt(string(data), 10, 64)
	if id == 0 {
		return Ticket{}, false, nil
	}
	t, err := s.Ticket(ctx, id)
	if errors.Is(err, ErrNotFound) { // the pointer outlived the ticket (a prune): clean it
		_ = s.kv.KVDelete(ctx, userTicketKey(tgID), 0)
		return Ticket{}, false, nil
	}
	if err != nil {
		return Ticket{}, false, err
	}
	if t.Status != StatusOpen { // a close raced the pointer
		return Ticket{}, false, nil
	}
	return t, true, nil
}

// PointUserAt sets (id>0) or clears (id==0) the user's open-ticket pointer.
func (s *Store) PointUserAt(ctx context.Context, tgID, id int64) error {
	if id == 0 {
		return s.kv.KVDelete(ctx, userTicketKey(tgID), 0)
	}
	_, err := s.kv.KVPut(ctx, userTicketKey(tgID), []byte(strconv.FormatInt(id, 10)), panelapi.PutOpts{})
	return err
}

// chunk is m/<id8>/<chunk4>.
type chunk struct {
	Msgs []Message `json:"msgs"`
}

// AppendMessage adds a user, agent, AI or system line to a ticket: the ticket's
// counters move under CAS, then the message goes into the last chunk (a new one when
// it is full). m.Seq and m.At are assigned here. A message carrying a TgMsgID that is
// already stored is a no-op (a replayed Telegram update).
func (s *Store) AppendMessage(ctx context.Context, id int64, m Message) error {
	m.Text = clipText(m.Text)
	m.FileName = clipName(m.FileName, 256)
	m.FileMime = clipName(m.FileMime, 64)
	m.FileID = clipName(m.FileID, 256)
	if m.At == 0 {
		m.At = s.now().Unix()
	}
	if m.TgMsgID != 0 {
		dup, err := s.hasTgMessage(ctx, id, m.TgMsgID)
		if err != nil {
			return err
		}
		if dup {
			return nil
		}
	}
	var assigned Message
	if _, err := s.UpdateTicket(ctx, id, func(t *Ticket) error {
		t.Seq++
		m.Seq = t.Seq
		t.MsgCount++
		t.LastMsgAt = m.At
		t.LastFrom = m.From
		if m.From == FromUser {
			t.Unread++
		} else {
			t.Unread = 0
		}
		assigned = m
		return nil
	}); err != nil {
		return err
	}
	return s.appendChunk(ctx, id, assigned)
}

// hasTgMessage scans a ticket's chunks for a Telegram message id (a replay check; the
// scan is over at most a few chunks in the common case since replays are recent).
func (s *Store) hasTgMessage(ctx context.Context, id, tgMsgID int64) (bool, error) {
	msgs, err := s.Messages(ctx, id, 0, 0)
	if err != nil {
		return false, err
	}
	for _, m := range msgs {
		if m.TgMsgID == tgMsgID && m.From == FromUser {
			return true, nil
		}
	}
	return false, nil
}

// appendChunk writes one message into the ticket's last chunk under CAS. A chunk that
// would grow past the flush size stays as it is and the message opens the next one;
// the rollover itself is CAS and idempotent, so two racing appenders (the poller and
// an agent reply) lose nothing.
func (s *Store) appendChunk(ctx context.Context, id int64, m Message) error {
	for range 4 {
		t, err := s.Ticket(ctx, id)
		if err != nil {
			return err
		}
		n := t.LastChunk
		err = s.kv.CAS(ctx, chunkKey(id, n), 8, func(data []byte, ver int64) (panelapi.Update, error) {
			var c chunk
			if data != nil {
				if err := json.Unmarshal(data, &c); err != nil {
					return panelapi.Update{}, fmt.Errorf("store: chunk %d of %d: %w", n, id, err)
				}
			}
			for _, x := range c.Msgs {
				if x.Seq == m.Seq {
					return panelapi.Update{}, nil // a crashed round wrote it already
				}
			}
			next := append(append([]Message(nil), c.Msgs...), m)
			out, err := json.Marshal(&chunk{Msgs: next})
			if err != nil {
				return panelapi.Update{}, err
			}
			if len(out) <= chunkFlush && len(next) <= chunkCap {
				return panelapi.Update{Val: out}, nil
			}
			// Rollover: the message opens chunk n+1. Both steps are idempotent, so a
			// retry of this CAS (or a crash between the steps) lands the same way.
			if err := s.casAppendChunk(ctx, id, n+1, m); err != nil {
				return panelapi.Update{}, err
			}
			if _, err := s.UpdateTicketMeta(ctx, id, func(t *Ticket) {
				if t.LastChunk < n+1 {
					t.LastChunk = n + 1
				}
			}); err != nil {
				return panelapi.Update{}, err
			}
			return panelapi.Update{}, nil
		})
		if err == nil {
			return nil
		}
		if !errors.Is(err, panelapi.ErrConflict) {
			return err
		}
	}
	return panelapi.ErrConflict
}

// casAppendChunk appends m to a fixed chunk under CAS (creating it), skipping a
// message whose seq is there already.
func (s *Store) casAppendChunk(ctx context.Context, id int64, n int, m Message) error {
	key := chunkKey(id, n)
	return s.kv.CAS(ctx, key, 8, func(data []byte, ver int64) (panelapi.Update, error) {
		var c chunk
		if data != nil {
			if err := json.Unmarshal(data, &c); err != nil {
				return panelapi.Update{}, fmt.Errorf("store: %s: %w", key, err)
			}
		}
		for _, x := range c.Msgs {
			if x.Seq == m.Seq {
				return panelapi.Update{}, nil
			}
		}
		out, err := json.Marshal(&chunk{Msgs: append(c.Msgs, m)})
		if err != nil {
			return panelapi.Update{}, err
		}
		return panelapi.Update{Val: out}, nil
	})
}

// UpdateTicketMeta mutates a ticket without moving UpdatedAt and without touching the
// index (bookkeeping like LastChunk).
func (s *Store) UpdateTicketMeta(ctx context.Context, id int64, fn func(t *Ticket)) (Ticket, error) {
	var out Ticket
	err := casJSON(ctx, s, ticketKey(id), func(cur *Ticket) (*Ticket, error) {
		if cur == nil {
			return nil, ErrNotFound
		}
		t := *cur
		fn(&t)
		out = t
		return &t, nil
	})
	return out, err
}

// Messages reads a ticket's history from seq afterSeq (0: all), at most limit (0: all),
// oldest first. A chunk that vanished (a prune mid-read) is skipped, not failed.
func (s *Store) Messages(ctx context.Context, id int64, afterSeq int64, limit int) ([]Message, error) {
	keys, _, err := s.kv.KVList(ctx, msgPrefix(id), "", 1000)
	if err != nil {
		return nil, err
	}
	out := make([]Message, 0, 32)
	for _, k := range keys {
		var c chunk
		data, _, err := s.kv.KVGet(ctx, k.Key)
		if errors.Is(err, panelapi.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &c); err != nil {
			return nil, fmt.Errorf("store: %s: %w", k.Key, err)
		}
		for _, m := range c.Msgs {
			if m.Seq <= afterSeq {
				continue
			}
			out = append(out, m)
			if limit > 0 && len(out) >= limit {
				return out, nil
			}
		}
	}
	return out, nil
}

// LastMessages is the tail of a ticket's history (the AI context, the dialog view).
func (s *Store) LastMessages(ctx context.Context, id int64, limit int) ([]Message, error) {
	all, err := s.Messages(ctx, id, 0, 0)
	if err != nil {
		return nil, err
	}
	if limit > 0 && len(all) > limit {
		all = all[len(all)-limit:]
	}
	return all, nil
}

// touchIndex puts a ticket's head into idx/tickets, newest first, capped.
func (s *Store) touchIndex(ctx context.Context, t *Ticket) error {
	h := head(t)
	return casJSON(ctx, s, indexKey, func(cur *index) (*index, error) {
		ix := index{}
		if cur != nil {
			ix = *cur
		}
		out := make([]TicketHead, 0, len(ix.Heads)+1)
		out = append(out, h)
		for _, x := range ix.Heads {
			if x.ID != h.ID {
				out = append(out, x)
			}
		}
		sortHeads(out)
		if len(out) > indexCap {
			out = out[:indexCap]
		}
		ix.Heads = out
		return &ix, nil
	})
}

func sortHeads(h []TicketHead) {
	for i := 1; i < len(h); i++ {
		for j := i; j > 0 && (h[j].UpdatedAt > h[j-1].UpdatedAt || h[j].UpdatedAt == h[j-1].UpdatedAt && h[j].ID > h[j-1].ID); j-- {
			h[j], h[j-1] = h[j-1], h[j]
		}
	}
}

// ListTickets is the index: status "" is everything, else open or closed. The index
// holds the newest indexCap; older closed tickets live on under t/ until the prune
// takes them.
func (s *Store) ListTickets(ctx context.Context, status string) ([]TicketHead, error) {
	var ix index
	if _, err := getJSON(ctx, s, indexKey, &ix); err != nil {
		if errors.Is(err, ErrNotFound) {
			return []TicketHead{}, nil
		}
		return nil, err
	}
	out := make([]TicketHead, 0, len(ix.Heads))
	for _, h := range ix.Heads {
		if status == "" || h.Status == status {
			out = append(out, h)
		}
	}
	return out, nil
}

// RebuildIndex reads every ticket and rewrites the index (a repair the status page
// offers; nothing calls it by itself).
func (s *Store) RebuildIndex(ctx context.Context) (int, error) {
	keys, err := s.listAllKeys(ctx, "t/")
	if err != nil {
		return 0, err
	}
	heads := make([]TicketHead, 0, len(keys))
	for _, k := range keys {
		var t Ticket
		if _, err := getJSON(ctx, s, k, &t); err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return 0, err
		}
		heads = append(heads, head(&t))
	}
	sortHeads(heads)
	if len(heads) > indexCap {
		heads = heads[:indexCap]
	}
	return len(heads), putJSON(ctx, s, indexKey, &index{Heads: heads}, 0)
}

func (s *Store) listAllKeys(ctx context.Context, prefix string) ([]string, error) {
	var out []string
	after := ""
	for {
		keys, next, err := s.kv.KVList(ctx, prefix, after, 1000)
		if err != nil {
			return nil, err
		}
		for _, k := range keys {
			out = append(out, k.Key)
		}
		if next == "" {
			return out, nil
		}
		after = next
	}
}

// CloseTicket closes a ticket and frees the user's pointer.
func (s *Store) CloseTicket(ctx context.Context, id int64) (Ticket, error) {
	t, err := s.UpdateTicket(ctx, id, func(t *Ticket) error {
		t.Status = StatusClosed
		t.Unread = 0
		return nil
	})
	if err != nil {
		return Ticket{}, err
	}
	if err := s.PointUserAt(ctx, t.TgID, 0); err != nil {
		return t, err
	}
	return t, nil
}

// ReopenTicket reopens a ticket and points the user at it again.
func (s *Store) ReopenTicket(ctx context.Context, id int64) (Ticket, error) {
	t, err := s.UpdateTicket(ctx, id, func(t *Ticket) error {
		t.Status = StatusOpen
		return nil
	})
	if err != nil {
		return Ticket{}, err
	}
	if err := s.PointUserAt(ctx, t.TgID, t.ID); err != nil {
		return t, err
	}
	return t, nil
}

// Prune removes closed tickets older than the retention: their key, their chunks and
// their index head. Open tickets are kept whatever their age.
func (s *Store) Prune(ctx context.Context, retention time.Duration) (int, error) {
	cutoff := s.now().Add(-retention).Unix()
	keys, err := s.listAllKeys(ctx, "t/")
	if err != nil {
		return 0, err
	}
	removed := 0
	dropped := map[int64]bool{}
	for _, k := range keys {
		var t Ticket
		if _, err := getJSON(ctx, s, k, &t); err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return removed, err
		}
		if t.Status != StatusClosed || t.UpdatedAt > cutoff {
			continue
		}
		chunks, err := s.listAllKeys(ctx, msgPrefix(t.ID))
		if err != nil {
			return removed, err
		}
		for _, ck := range chunks {
			if err := s.kv.KVDelete(ctx, ck, 0); err != nil && !errors.Is(err, panelapi.ErrNotFound) {
				return removed, err
			}
		}
		if err := s.kv.KVDelete(ctx, k, 0); err != nil && !errors.Is(err, panelapi.ErrNotFound) {
			return removed, err
		}
		dropped[t.ID] = true
		removed++
	}
	if len(dropped) > 0 {
		err = casJSON(ctx, s, indexKey, func(cur *index) (*index, error) {
			if cur == nil {
				return nil, nil
			}
			out := make([]TicketHead, 0, len(cur.Heads))
			for _, h := range cur.Heads {
				if !dropped[h.ID] {
					out = append(out, h)
				}
			}
			cur.Heads = out
			return cur, nil
		})
	}
	return removed, err
}

// DeleteTicket removes one ticket now: its key, its chunks, its head and the user's
// pointer when it was the open one.
func (s *Store) DeleteTicket(ctx context.Context, id int64) error {
	t, err := s.Ticket(ctx, id)
	if err != nil {
		return err
	}
	chunks, err := s.listAllKeys(ctx, msgPrefix(id))
	if err != nil {
		return err
	}
	for _, ck := range chunks {
		if err := s.kv.KVDelete(ctx, ck, 0); err != nil && !errors.Is(err, panelapi.ErrNotFound) {
			return err
		}
	}
	if err := s.kv.KVDelete(ctx, ticketKey(id), 0); err != nil && !errors.Is(err, panelapi.ErrNotFound) {
		return err
	}
	if cur, ok, err := s.OpenTicketFor(ctx, t.TgID); err == nil && ok && cur.ID == id {
		if err := s.PointUserAt(ctx, t.TgID, 0); err != nil {
			return err
		}
	}
	return casJSON(ctx, s, indexKey, func(cur *index) (*index, error) {
		if cur == nil {
			return nil, nil
		}
		out := make([]TicketHead, 0, len(cur.Heads))
		for _, h := range cur.Heads {
			if h.ID != id {
				out = append(out, h)
			}
		}
		cur.Heads = out
		return cur, nil
	})
}
