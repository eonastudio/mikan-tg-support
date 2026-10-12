// Package paneltest is a fake of the panel's API for addons (protocol 2) on
// httptest: the KV with versions, TTLs and conditions, the event feed with the
// per-addon cursor and ack, and /self. The tests run the addon against it as they
// would against the panel, over real HTTP.
package paneltest

import (
	"encoding/json"

	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Fake is the panel's API; New starts it.
type Fake struct {
	*httptest.Server

	mu       sync.Mutex
	values   map[string]*entry
	events   []eventRow
	cursor   int64
	token    string
	selfID   string
	perms    []string
	approved bool
	// Calls counts the requests by "METHOD path" (a test may assert on traffic).
	Calls map[string]int
}

type entry struct {
	value     []byte
	version   int64
	expiresAt int64 // 0: no expiry
}

type eventRow struct {
	ID     int64           `json:"id"`
	Type   string          `json:"type"`
	At     time.Time       `json:"at"`
	UserID int64           `json:"user_id"`
	TgID   int64           `json:"tg_id"`
	Data   json.RawMessage `json:"data"`
}

// New is a running fake; token is what the addon must send as its Bearer.
func New(token string) *Fake {
	f := &Fake{
		values:   map[string]*entry{},
		token:    token,
		selfID:   "tg-support",
		perms:    []string{"ui", "kv", "events"},
		approved: true,
		Calls:    map[string]int{},
	}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

// URL is the base for the addon's MIKAN_PANEL_URL (…/panel/v2).
func (f *Fake) URL() string { return f.Server.URL + "/panel/v2" }

// SetSelf changes the /self answer (an install under another id, a grant).
func (f *Fake) SetSelf(id string, perms []string) {
	f.mu.Lock()
	f.selfID, f.perms = id, perms
	f.mu.Unlock()
}

// SetApproved mimics the 403 not_approved of a fresh install.
func (f *Fake) SetApproved(ok bool) {
	f.mu.Lock()
	f.approved = ok
	f.mu.Unlock()
}

// Emit adds an event to the feed and wakes nothing (the fake does not long poll;
// it answers at once, which the consumer treats as an empty round).
func (f *Fake) Emit(typ string, userID, tgID int64, data any) int64 {
	raw, _ := json.Marshal(data)
	f.mu.Lock()
	defer f.mu.Unlock()
	id := int64(len(f.events)) + 1
	f.events = append(f.events, eventRow{ID: id, Type: typ, At: time.Now().UTC(), UserID: userID, TgID: tgID, Data: raw})
	return id
}

// Value is a raw KV read for assertions ("", false when there is none or it expired).
func (f *Fake) Value(key string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.values[key]
	if !ok || (e.expiresAt > 0 && time.Now().Unix() >= e.expiresAt) {
		return "", false
	}
	return string(e.value), true
}

// Keys is every live key, sorted (assertions on the layout).
func (f *Fake) Keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	now := time.Now().Unix()
	for k, e := range f.values {
		if e.expiresAt == 0 || e.expiresAt > now {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Cursor is the addon's event cursor (what the last ack moved it to).
func (f *Fake) Cursor() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cursor
}

// SetCursor rewinds the cursor (a test of the replay: the feed sees old events again
// and must dedupe them by their ids).
func (f *Fake) SetCursor(n int64) {
	f.mu.Lock()
	f.cursor = n
	f.mu.Unlock()
}

func (f *Fake) fail(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "message": msg})
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.Calls[r.Method+" "+r.URL.Path]++
	f.mu.Unlock()

	if r.Header.Get("Origin") != "" {
		f.fail(w, http.StatusForbidden, "forbidden", "not for browsers")
		return
	}
	if got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); got != f.token {
		f.fail(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
		return
	}
	f.mu.Lock()
	approved := f.approved
	f.mu.Unlock()
	if !approved {
		f.fail(w, http.StatusForbidden, "not_approved", "not_approved")
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/panel/v2")
	switch {
	case path == "/self" && r.Method == http.MethodGet:
		f.mu.Lock()
		id, perms := f.selfID, f.perms
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "version": "0.5.1.0-test", "permissions": perms})

	case path == "/events" && r.Method == http.MethodGet:
		f.eventsList(w, r)
	case path == "/events/ack" && r.Method == http.MethodPost:
		f.eventsAck(w, r)

	case path == "/kv" && r.Method == http.MethodGet:
		f.kvList(w, r)
	case strings.HasPrefix(path, "/kv/") && r.Method == http.MethodGet:
		f.kvGet(w, strings.TrimPrefix(path, "/kv/"))
	case strings.HasPrefix(path, "/kv/") && r.Method == http.MethodPut:
		f.kvPut(w, r, strings.TrimPrefix(path, "/kv/"))
	case strings.HasPrefix(path, "/kv/") && r.Method == http.MethodDelete:
		f.kvDelete(w, r, strings.TrimPrefix(path, "/kv/"))

	default:
		f.fail(w, http.StatusNotFound, "not_found", "no such method")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *Fake) eventsList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var types map[string]bool
	if t := q.Get("types"); t != "" {
		types = map[string]bool{}
		for _, x := range strings.Split(t, ",") {
			types[x] = true
		}
	}
	f.mu.Lock()
	after := f.cursor
	if a := q.Get("after"); a != "" {
		after, _ = strconv.ParseInt(a, 10, 64)
	}
	var out []eventRow
	for _, e := range f.events {
		if e.ID <= after {
			continue
		}
		if types != nil && !types[e.Type] {
			continue
		}
		out = append(out, e)
		if len(out) >= limit {
			break
		}
	}
	f.mu.Unlock()
	if out == nil {
		out = []eventRow{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out})
}

func (f *Fake) eventsAck(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID int64 `json:"id"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil || in.ID < 1 {
		f.fail(w, http.StatusBadRequest, "bad_request", "the body is {id}")
		return
	}
	f.mu.Lock()
	if in.ID > f.cursor {
		f.cursor = in.ID
	}
	f.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (f *Fake) kvGet(w http.ResponseWriter, key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.values[key]
	if !ok || (e.expiresAt > 0 && time.Now().Unix() >= e.expiresAt) {
		f.fail(w, http.StatusNotFound, "not_found", "no such key")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(e.version, 10)))
	if e.expiresAt > 0 {
		w.Header().Set("X-Expires-At", strconv.FormatInt(e.expiresAt, 10))
	}
	_, _ = w.Write(e.value)
}

func (f *Fake) kvPut(w http.ResponseWriter, r *http.Request, key string) {
	value := make([]byte, 0, 1024)
	buf := make([]byte, 1024)
	for {
		n, err := r.Body.Read(buf)
		value = append(value, buf[:n]...)
		if err != nil {
			break
		}
		if len(value) > 64<<10 {
			f.fail(w, http.StatusRequestEntityTooLarge, "too_large", "a value is at most 64 KiB")
			return
		}
	}
	q := r.URL.Query()
	var ttl int64
	if t := q.Get("ttl"); t != "" {
		ttl, _ = strconv.ParseInt(t, 10, 64)
		if ttl < 1 || ttl > 366*24*3600 {
			f.fail(w, http.StatusBadRequest, "bad_request", "ttl")
			return
		}
	}
	absent := q.Get("if_absent") == "1" || q.Get("if_absent") == "true"
	want, conditional, bad := ifMatch(r)
	if bad {
		f.fail(w, http.StatusBadRequest, "bad_request", "If-Match is a version")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now().Unix()
	old, exists := f.values[key]
	live := exists && (old.expiresAt == 0 || old.expiresAt > now)
	if absent && live || conditional && (!live || old.version != want) {
		f.fail(w, http.StatusConflict, "conflict", "the value is not what the condition expects")
		return
	}
	version := int64(1)
	if exists {
		version = old.version + 1
	}
	e := &entry{value: append([]byte(nil), value...), version: version}
	if ttl > 0 {
		e.expiresAt = now + ttl
	}
	f.values[key] = e
	w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(version, 10)))
	writeJSON(w, http.StatusOK, map[string]int64{"version": version})
}

func (f *Fake) kvDelete(w http.ResponseWriter, r *http.Request, key string) {
	want, conditional, bad := ifMatch(r)
	if bad {
		f.fail(w, http.StatusBadRequest, "bad_request", "If-Match is a version")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now().Unix()
	if conditional {
		old, ok := f.values[key]
		if !ok || old.version != want || (old.expiresAt > 0 && old.expiresAt <= now) {
			f.fail(w, http.StatusConflict, "conflict", "the value is not what the condition expects")
			return
		}
	}
	delete(f.values, key)
	w.WriteHeader(http.StatusNoContent)
}

func (f *Fake) kvList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	prefix, after := q.Get("prefix"), q.Get("after")
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	f.mu.Lock()
	now := time.Now().Unix()
	var keys []string
	for k, e := range f.values {
		if strings.HasPrefix(k, prefix) && (e.expiresAt == 0 || e.expiresAt > now) && (after == "" || k > after) {
			keys = append(keys, k)
		}
	}
	f.mu.Unlock()
	sort.Strings(keys)
	type kvKey struct {
		Key       string `json:"key"`
		Version   int64  `json:"version"`
		ExpiresAt *int64 `json:"expires_at,omitempty"`
	}
	out := struct {
		Keys []kvKey `json:"keys"`
		Next string  `json:"next"`
	}{Keys: []kvKey{}}
	f.mu.Lock()
	for i, k := range keys {
		if i >= limit {
			out.Next = out.Keys[limit-1].Key
			break
		}
		e := f.values[k]
		kk := kvKey{Key: k, Version: e.version}
		if e.expiresAt > 0 {
			x := e.expiresAt
			kk.ExpiresAt = &x
		}
		out.Keys = append(out.Keys, kk)
	}
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

func ifMatch(r *http.Request) (int64, bool, bool) {
	h := strings.TrimSpace(r.Header.Get("If-Match"))
	if h == "" {
		return 0, false, false
	}
	v, err := strconv.ParseInt(strings.Trim(h, `"`), 10, 64)
	return v, true, err != nil
}

// CallsOf is how many requests of a kind the fake saw.
func (f *Fake) CallsOf(methodPath string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Calls[methodPath]
}
