// Package store is the addon's state on top of the panel's KV (permission kv). The
// container keeps no files, so everything lives here, backed up with the panel:
//
//	cfg/main            the settings (the bot token and the AI key among them)
//	seq/tickets         the ticket counter, decimal
//	t/<id8>             one ticket (id zero-padded to 8)
//	m/<id8>/<chunk4>    messages of a ticket, in chunks of at most chunkFlush bytes
//	idx/tickets         heads of the most recently updated tickets, newest first
//	ut/<tg_id>          the open ticket of a Telegram user, decimal
//	u/<tg_id>           what the panel's events told of a user (for the AI and the agent)
//	ev/<event_id>       an event handled (dedupe; the feed is at least once), with a TTL
//	upd/<update_id>     a Telegram update handled (dedupe across an offset replay), TTL
//	off/main            the acknowledged Telegram update offset, decimal
//
// Writes that race (the poller and the admin pages touch the same ticket) go through
// CAS: read with the version, write If-Match, repeat on conflict.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"tg-support/internal/panelapi"
)

// ErrNotFound is a missing ticket, profile or key, whichever the caller asked for.
var ErrNotFound = errors.New("store: not found")

// Store reads and writes the addon's KV. One Store is safe for concurrent use.
type Store struct {
	kv  *panelapi.Client
	now func() time.Time
}

// New is a Store on the panel's KV.
func New(kv *panelapi.Client, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{kv: kv, now: now}
}

// Now is the store's clock (tests freeze it).
func (s *Store) Now() time.Time { return s.now() }

// KV exposes the raw client (the status page counts keys with it).
func (s *Store) KV() *panelapi.Client { return s.kv }

// ticketKey is t/<id8>.
func ticketKey(id int64) string { return fmt.Sprintf("t/%08d", id) }

// msgPrefix is the prefix of a ticket's chunks.
func msgPrefix(id int64) string { return fmt.Sprintf("m/%08d/", id) }

// chunkKey is m/<id8>/<chunk4>.
func chunkKey(id int64, chunk int) string { return fmt.Sprintf("m/%08d/%04d", id, chunk) }

// userKey is ut/<tg_id>.
func userTicketKey(tgID int64) string { return "ut/" + strconv.FormatInt(tgID, 10) }

// profileKey is u/<tg_id>.
func profileKey(tgID int64) string { return "u/" + strconv.FormatInt(tgID, 10) }

// getJSON reads a key and decodes it; a missing key is ErrNotFound.
func getJSON[T any](ctx context.Context, s *Store, key string, out *T) (int64, error) {
	data, ver, err := s.kv.KVGet(ctx, key)
	if errors.Is(err, panelapi.ErrNotFound) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return 0, fmt.Errorf("store: %s: %w", key, err)
	}
	return ver, nil
}

// putJSON encodes and writes a value, plain (no condition).
func putJSON(ctx context.Context, s *Store, key string, v any, ttl time.Duration) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("store: %s: %w", key, err)
	}
	_, err = s.kv.KVPut(ctx, key, data, panelapi.PutOpts{TTL: ttl})
	return err
}

// casJSON is a CAS round over a JSON value: fn sees the decoded value (nil when the
// key is missing) and mutates or replaces it; a nil answer skips the write.
func casJSON[T any](ctx context.Context, s *Store, key string, fn func(cur *T) (*T, error)) error {
	return casJSONTTL(ctx, s, key, 0, fn)
}

// casJSONTTL is casJSON with a TTL on every write (0: no expiry).
func casJSONTTL[T any](ctx context.Context, s *Store, key string, ttl time.Duration, fn func(cur *T) (*T, error)) error {
	return s.kv.CAS(ctx, key, 8, func(data []byte, ver int64) (panelapi.Update, error) {
		var cur *T
		if data != nil {
			var v T
			if err := json.Unmarshal(data, &v); err != nil {
				return panelapi.Update{}, fmt.Errorf("store: %s: %w", key, err)
			}
			cur = &v
		}
		next, err := fn(cur)
		if err != nil || next == nil {
			return panelapi.Update{}, err
		}
		out, err := json.Marshal(next)
		if err != nil {
			return panelapi.Update{}, fmt.Errorf("store: %s: %w", key, err)
		}
		return panelapi.Update{Val: out, TTL: ttl}, nil
	})
}

// MarkOnce marks key as handled and says whether this call was the first: the dedupe
// of at-least-once feeds (panel events, replayed Telegram updates).
func (s *Store) MarkOnce(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	_, err := s.kv.KVPut(ctx, key, []byte("1"), panelapi.PutOpts{TTL: ttl, IfAbsent: true})
	if errors.Is(err, panelapi.ErrConflict) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// Offset is the acknowledged Telegram update offset (0: none yet).
func (s *Store) Offset(ctx context.Context) (int64, error) {
	data, _, err := s.kv.KVGet(ctx, "off/main")
	if errors.Is(err, panelapi.ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n, _ := strconv.ParseInt(string(data), 10, 64)
	return n, nil
}

// SaveOffset acknowledges every Telegram update below next.
func (s *Store) SaveOffset(ctx context.Context, next int64) error {
	_, err := s.kv.KVPut(ctx, "off/main", []byte(strconv.FormatInt(next, 10)), panelapi.PutOpts{})
	return err
}

// CountKeys counts the addon's live keys with a prefix, up to max (a status readout).
func (s *Store) CountKeys(ctx context.Context, prefix string, max int) (int, error) {
	n, after := 0, ""
	for {
		keys, next, err := s.kv.KVList(ctx, prefix, after, 1000)
		if err != nil {
			return n, err
		}
		n += len(keys)
		if next == "" || n >= max {
			return n, nil
		}
		after = next
	}
}
