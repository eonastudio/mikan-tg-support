// Package panelapi is the client of the mikan panel's API for addons (protocol 2,
// PROTOCOL-v2.md): the addon's own KV store, the feed of events and /self. The panel
// serves it on a loopback (or bridge) listener; the addon proves itself with the token
// from MIKAN_PANEL_TOKEN and may call only what its approved permissions hold.
package panelapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// MaxValue is the largest KV value the panel accepts (and the largest body it reads).
const MaxValue = 64 << 10

// validKey mirrors the panel's key grammar: 1-256 of A-Z a-z 0-9 . _ : / -.
var validKey = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,256}$`)

// ValidKey says whether key can be used in the KV API.
func ValidKey(k string) bool { return validKey.MatchString(k) }

// Errors the panel answers with; codes as in PROTOCOL-v2.md.
var (
	ErrUnauthorized = errors.New("panelapi: unauthorized")      // 401: a bad or not yet known token
	ErrNotApproved  = errors.New("panelapi: not approved")      // 403 not_approved: no grant yet
	ErrForbidden    = errors.New("panelapi: forbidden")         // 403 forbidden: the permission is missing
	ErrNotFound     = errors.New("panelapi: not found")         // 404
	ErrConflict     = errors.New("panelapi: conflict")          // 409: a CAS condition did not hold
	ErrQuota        = errors.New("panelapi: quota exceeded")    // 507: the addon's store is full
	ErrBadRequest   = errors.New("panelapi: bad request")       // 400
	ErrUnavailable  = errors.New("panelapi: panel unavailable") // the way failed, or the panel erred
)

// Error is the panel's refusal with its own message.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("panelapi: %d %s: %s", e.Status, e.Code, e.Message)
}

// Client calls the Panel API. One Client is safe for concurrent use.
type Client struct {
	base  string // MIKAN_PANEL_URL, e.g. http://127.0.0.1:41990/panel/v2
	token string
	hc    *http.Client
}

// New is a Client for the panel at base (MIKAN_PANEL_URL) with the addon's token.
func New(base, token string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 60 * time.Second // room for a 50 s long poll
	}
	return &Client{
		base:  strings.TrimSuffix(base, "/"),
		token: token,
		hc: &http.Client{
			Timeout: timeout,
			// The panel must not be walked away from: a redirect is an answer, not a way.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// do sends one request and reads the answer, up to limit bytes. A status the panel
// documents becomes a typed error (ErrConflict and friends), anything else an *Error.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body []byte, hdr http.Header, limit int64) (int, http.Header, []byte, error) {
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("panelapi: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return 0, nil, nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if int64(len(data)) > limit {
		return resp.StatusCode, resp.Header, nil, fmt.Errorf("panelapi: the answer is larger than %d bytes", limit)
	}
	if resp.StatusCode/100 == 2 {
		return resp.StatusCode, resp.Header, data, nil
	}
	// What the panel refused: its {"code","message"} first, then the documented kinds.
	e := &Error{Status: resp.StatusCode}
	if json.Unmarshal(data, e) != nil || e.Code == "" {
		e.Code = "panel_error"
		e.Message = http.StatusText(resp.StatusCode)
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return resp.StatusCode, resp.Header, data, errors.Join(ErrUnauthorized, e)
	case resp.StatusCode == http.StatusForbidden && e.Code == "not_approved":
		return resp.StatusCode, resp.Header, data, errors.Join(ErrNotApproved, e)
	case resp.StatusCode == http.StatusForbidden:
		return resp.StatusCode, resp.Header, data, errors.Join(ErrForbidden, e)
	case resp.StatusCode == http.StatusNotFound:
		return resp.StatusCode, resp.Header, data, errors.Join(ErrNotFound, e)
	case resp.StatusCode == http.StatusConflict:
		return resp.StatusCode, resp.Header, data, errors.Join(ErrConflict, e)
	case resp.StatusCode == http.StatusInsufficientStorage:
		return resp.StatusCode, resp.Header, data, errors.Join(ErrQuota, e)
	case resp.StatusCode == http.StatusBadRequest:
		return resp.StatusCode, resp.Header, data, errors.Join(ErrBadRequest, e)
	}
	if resp.StatusCode >= 500 {
		return resp.StatusCode, resp.Header, data, fmt.Errorf("%w: %w", ErrUnavailable, e)
	}
	return resp.StatusCode, resp.Header, data, e
}

// Self is who the panel takes the addon for.
type Self struct {
	ID          string   `json:"id"`
	Version     string   `json:"version"`
	Permissions []string `json:"permissions"`
}

// Self asks GET /self: a liveness and identity check, and the permissions as approved.
func (c *Client) Self(ctx context.Context) (Self, error) {
	var s Self
	_, _, data, err := c.do(ctx, http.MethodGet, "/self", nil, nil, nil, MaxValue)
	if err != nil {
		return s, err
	}
	err = json.Unmarshal(data, &s)
	return s, err
}

// etagVersion parses the panel's ETag: a decimal version in quotes.
func etagVersion(h http.Header) int64 {
	v, err := strconv.ParseInt(strings.Trim(h.Get("ETag"), `"`), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// KVGet reads a key: its value and the version (the ETag a CAS write conditions on).
// A key that is not there (or has expired) is ErrNotFound.
func (c *Client) KVGet(ctx context.Context, key string) ([]byte, int64, error) {
	if !ValidKey(key) {
		return nil, 0, fmt.Errorf("%w: key %q", ErrBadRequest, key)
	}
	status, hdr, data, err := c.do(ctx, http.MethodGet, "/kv/"+key, nil, nil, nil, MaxValue)
	if err != nil {
		return nil, 0, err
	}
	if status != http.StatusOK {
		return nil, 0, &Error{Status: status, Code: "unexpected", Message: "kv get"}
	}
	return data, etagVersion(hdr), nil
}

// PutOpts conditions a KV write.
type PutOpts struct {
	TTL      time.Duration // 0: the value does not expire on its own
	IfAbsent bool          // write only where no live value is
	IfMatch  int64         // >0: write only over exactly this version (compare-and-set)
}

func (o PutOpts) query() url.Values {
	q := url.Values{}
	if o.TTL > 0 {
		q.Set("ttl", strconv.FormatInt(int64(o.TTL.Seconds()), 10))
	}
	if o.IfAbsent {
		q.Set("if_absent", "1")
	}
	return q
}

func (o PutOpts) header() http.Header {
	if o.IfMatch > 0 {
		h := http.Header{}
		h.Set("If-Match", strconv.Quote(strconv.FormatInt(o.IfMatch, 10)))
		return h
	}
	return nil
}

// KVPut writes a value (at most 64 KiB) and returns its new version. A condition that
// does not hold is ErrConflict; a full store is ErrQuota.
func (c *Client) KVPut(ctx context.Context, key string, value []byte, o PutOpts) (int64, error) {
	if !ValidKey(key) {
		return 0, fmt.Errorf("%w: key %q", ErrBadRequest, key)
	}
	if len(value) > MaxValue {
		return 0, fmt.Errorf("%w: the value of %q is %d bytes, the limit is %d", ErrBadRequest, key, len(value), MaxValue)
	}
	_, _, data, err := c.do(ctx, http.MethodPut, "/kv/"+key, o.query(), value, o.header(), MaxValue)
	if err != nil {
		return 0, err
	}
	var out struct {
		Version int64 `json:"version"`
	}
	if json.Unmarshal(data, &out) != nil {
		return 0, errors.New("panelapi: kv put: the answer has no version")
	}
	return out.Version, nil
}

// KVDelete removes a key; ifMatch >0 conditions it on a version. A key that is not
// there deletes fine (the panel answers 204 either way).
func (c *Client) KVDelete(ctx context.Context, key string, ifMatch int64) error {
	if !ValidKey(key) {
		return fmt.Errorf("%w: key %q", ErrBadRequest, key)
	}
	var h http.Header
	if ifMatch > 0 {
		h = http.Header{}
		h.Set("If-Match", strconv.Quote(strconv.FormatInt(ifMatch, 10)))
	}
	_, _, _, err := c.do(ctx, http.MethodDelete, "/kv/"+key, nil, nil, h, MaxValue)
	return err
}

// KeyInfo is one key of a listing.
type KeyInfo struct {
	Key       string `json:"key"`
	Version   int64  `json:"version"`
	ExpiresAt *int64 `json:"expires_at,omitempty"`
}

// KVList lists live keys in order: a prefix, then pages by after (the last key of the
// page before); limit is 1-1000. next is "" at the end.
func (c *Client) KVList(ctx context.Context, prefix, after string, limit int) ([]KeyInfo, string, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	q := url.Values{}
	if prefix != "" {
		q.Set("prefix", prefix)
	}
	if after != "" {
		q.Set("after", after)
	}
	q.Set("limit", strconv.Itoa(limit))
	_, _, data, err := c.do(ctx, http.MethodGet, "/kv", q, nil, nil, 1<<20)
	if err != nil {
		return nil, "", err
	}
	var out struct {
		Keys []KeyInfo `json:"keys"`
		Next string    `json:"next"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, "", fmt.Errorf("panelapi: kv list: %w", err)
	}
	return out.Keys, out.Next, nil
}

// Event is one entry of the panel's feed.
type Event struct {
	ID     int64           `json:"id"`
	Type   string          `json:"type"`
	At     time.Time       `json:"at"`
	UserID int64           `json:"user_id"`
	TgID   int64           `json:"tg_id"`
	Data   json.RawMessage `json:"data"`
}

// Events reads the feed after a cursor: 0 is the addon's own cursor the panel keeps.
// wait is the long poll in seconds (0-50), limit 1-500. Delivery is at least once:
// a consumer dedupes by ID.
func (c *Client) Events(ctx context.Context, types []string, after int64, limit, wait int) ([]Event, error) {
	q := url.Values{}
	if len(types) > 0 {
		q.Set("types", strings.Join(types, ","))
	}
	if after > 0 {
		q.Set("after", strconv.FormatInt(after, 10))
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q.Set("limit", strconv.Itoa(limit))
	if wait > 0 {
		q.Set("wait", strconv.Itoa(min(wait, 50)))
	}
	_, _, data, err := c.do(ctx, http.MethodGet, "/events", q, nil, nil, 1<<20)
	if err != nil {
		return nil, err
	}
	var out struct {
		Events []Event `json:"events"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("panelapi: events: %w", err)
	}
	return out.Events, nil
}

// EventsAck moves the addon's cursor to id (the last event handled); the cursor only
// goes forward.
func (c *Client) EventsAck(ctx context.Context, id int64) error {
	body, err := json.Marshal(map[string]int64{"id": id})
	if err != nil {
		return err
	}
	_, _, _, err = c.do(ctx, http.MethodPost, "/events/ack", nil, body, nil, MaxValue)
	return err
}

// Update is one KV write of a compare-and-set round: val is what to write over the
// version ver (0 with no live value); nil val skips the write.
type Update struct {
	Val []byte
	TTL time.Duration
}

// CAS reads a key, builds the new value with fn and writes it If-Match, repeating while
// the value changes under it (a concurrent writer) at most tries times. A missing key
// reaches fn as (nil, 0) and is written with if_absent semantics through If-Match: 0
// means "only where nothing live is" is not expressible, so the first write races a
// concurrent create: the loser retries and fn sees the winner's value.
//
// The retries back off: contenders that hammer the key without a pause keep colliding
// and exhaust the budget even when each of them would win eventually. fn must be
// idempotent-safe (it runs again per retry).
func (c *Client) CAS(ctx context.Context, key string, tries int, fn func(val []byte, ver int64) (Update, error)) error {
	if tries <= 0 {
		tries = 8
	}
	for i := range tries {
		val, ver, err := c.KVGet(ctx, key)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if errors.Is(err, ErrNotFound) {
			val, ver = nil, 0
		}
		u, err := fn(val, ver)
		if err != nil {
			return err
		}
		if u.Val == nil {
			return nil // fn decided not to write
		}
		opts := PutOpts{TTL: u.TTL}
		if ver > 0 {
			opts.IfMatch = ver
		} else {
			opts.IfAbsent = true
		}
		_, err = c.KVPut(ctx, key, u.Val, opts)
		if errors.Is(err, ErrConflict) && i < tries-1 {
			d := time.Duration(1<<min(i, 5)) * time.Millisecond
			t := time.NewTimer(d)
			select {
			case <-ctx.Done():
				t.Stop()
				return ctx.Err()
			case <-t.C:
			}
			continue
		}
		return err
	}
	return ErrConflict
}
