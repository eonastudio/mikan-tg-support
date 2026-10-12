// Package evfeed consumes the panel's feed of events (permission events) and folds
// it into the per-user profiles the AI and the ticket page read: payments, the
// subscription's end and the traffic notices. The feed is at least once, so every
// event is deduped by its id (a KV mark with a TTL); the cursor itself lives in the
// panel, moved by /events/ack.
package evfeed

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"tg-support/internal/panelapi"
	"tg-support/internal/store"
)

// Types the addon subscribes to: the payment events and the notices. (The bot.*
// events carry whole subscriptions but belong to the holder of the panel's bot slot
// alone; a second bot never sees them.)
var Types = []string{
	"payment.paid", "payment.refunded",
	"user.expiring", "user.expired",
	"traffic.90", "traffic.100",
}

// Status is the readout for the admin page.
type Status struct {
	Enabled   bool   `json:"enabled"`
	LastError string `json:"last_error,omitempty"`
	LastErrAt int64  `json:"last_error_at,omitempty"`
	LastEvent int64  `json:"last_event_at,omitempty"`
	Applied   int64  `json:"applied"`
	LastID    int64  `json:"last_id,omitempty"`
}

// Feed is the consumer; Run keeps it going until the context ends.
type Feed struct {
	kv  *panelapi.Client
	st  *store.Store
	log *slog.Logger

	applied atomic.Int64
	lastID  atomic.Int64
	lastEv  atomic.Int64
	lastErr atomic.Value // string
	lastEAt atomic.Int64
}

// New is a Feed on the panel's API and the store.
func New(kv *panelapi.Client, st *store.Store, log *slog.Logger) *Feed {
	f := &Feed{kv: kv, st: st, log: log}
	f.lastErr.Store("")
	return f
}

// Status is the readout for the admin page; enabled says whether the grant has the
// events permission (a 403 forbidden turns it off here).
func (f *Feed) Status(enabled bool) Status {
	s := Status{Enabled: enabled, Applied: f.applied.Load(), LastID: f.lastID.Load(), LastEvent: f.lastEv.Load()}
	if e := f.lastErr.Load().(string); e != "" {
		s.LastError, s.LastErrAt = e, f.lastEAt.Load()
	}
	return s
}

// Run is the loop: a 30 s long poll, fold what came, ack the last. The panel keeps
// the cursor, so a restart continues where the last ack was; the KV marks keep the
// replays between two acks from double-folding a payment.
func (f *Feed) Run(ctx context.Context, permitted *atomic.Bool) {
	backoff := time.Duration(0)
	for ctx.Err() == nil {
		events, err := f.kv.Events(ctx, Types, 0, 100, 30)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			switch {
			case errors.Is(err, panelapi.ErrNotApproved):
				// The grant is not there yet (a fresh install window): wait it out.
				f.setErr("the panel has no approval for the addon yet")
				sleep(ctx, 15*time.Second)
			case errors.Is(err, panelapi.ErrForbidden):
				// No events permission: the admin did not grant it. Quiet, slow polls
				// keep the page's status honest without load.
				if permitted != nil {
					permitted.Store(false)
				}
				f.setErr("no permission for the panel's events")
				sleep(ctx, 5*time.Minute)
			case errors.Is(err, panelapi.ErrUnauthorized):
				f.setErr("the panel does not know the addon's token yet")
				sleep(ctx, 10*time.Second)
			default:
				backoff = nextBackoff(backoff)
				f.setErr(err.Error())
				sleep(ctx, backoff)
			}
			continue
		}
		backoff = 0
		if permitted != nil {
			permitted.Store(true)
		}
		f.clearErr()
		var last int64
		for _, e := range events {
			last = e.ID
			if err := f.apply(ctx, e); err != nil {
				f.log.Warn("events: an event did not fold", "id", e.ID, "type", e.Type, "err", err)
				// The ack below still moves past it: a profile misses one line, while
				// a poisoned event would otherwise replay forever.
			}
			if e.TgID != 0 {
				f.applied.Add(1)
			}
			f.lastEv.Store(e.At.Unix())
		}
		if last > 0 {
			if err := f.kv.EventsAck(ctx, last); err != nil {
				f.log.Warn("events: the ack failed", "err", err)
				continue // the batch comes again; the marks dedupe it
			}
			f.lastID.Store(last)
		}
	}
}

// apply folds one event: dedupe by id first (a payment folded twice would show twice
// in the profile), then into the profile of its tg_id.
func (f *Feed) apply(ctx context.Context, e panelapi.Event) error {
	mark := "ev/" + itoa(e.ID)
	first, err := f.st.MarkOnce(ctx, mark, 8*24*time.Hour)
	if err != nil {
		return err
	}
	if !first {
		return nil
	}
	return f.st.ApplyEvent(ctx, e.TgID, e.UserID, e.Type, e.At, e.Data)
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func (f *Feed) setErr(s string) {
	if len(s) > 300 {
		s = s[:300]
	}
	f.lastErr.Store(s)
	f.lastEAt.Store(time.Now().Unix())
}

func (f *Feed) clearErr() { f.lastErr.Store("") }

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
