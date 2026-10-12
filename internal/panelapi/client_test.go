package panelapi

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"tg-support/internal/paneltest"
)

func newClient(t *testing.T) (*Client, *paneltest.Fake) {
	t.Helper()
	f := paneltest.New("tok")
	t.Cleanup(f.Close)
	return New(f.URL(), "tok", 5*time.Second), f
}

func TestSelfAndAuthErrors(t *testing.T) {
	c, f := newClient(t)
	ctx := context.Background()
	s, err := c.Self(ctx)
	if err != nil || s.ID != "tg-support" || len(s.Permissions) != 3 {
		t.Fatalf("self: %+v (%v)", s, err)
	}
	// A wrong token is unauthorized, not a transport failure.
	bad := New(f.URL(), "wrong", 5*time.Second)
	_, err = bad.Self(ctx)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("the wrong token: %v", err)
	}
	// No approval yet: not_approved, distinguishable from a missing permission.
	f.SetApproved(false)
	_, err = c.Self(ctx)
	if !errors.Is(err, ErrNotApproved) {
		t.Fatalf("the missing approval: %v", err)
	}
	f.SetApproved(true)
}

func TestKVRoundtrip(t *testing.T) {
	c, _ := newClient(t)
	ctx := context.Background()
	if _, _, err := c.KVGet(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a missing key: %v", err)
	}
	v, err := c.KVPut(ctx, "a/b", []byte("one"), PutOpts{})
	if err != nil || v != 1 {
		t.Fatalf("the first put: %d (%v)", v, err)
	}
	data, ver, err := c.KVGet(ctx, "a/b")
	if err != nil || string(data) != "one" || ver != 1 {
		t.Fatalf("get: %s %d (%v)", data, ver, err)
	}
	// If-Match over a stale version is a conflict; over the live one it writes.
	if _, err := c.KVPut(ctx, "a/b", []byte("x"), PutOpts{IfMatch: 99}); !errors.Is(err, ErrConflict) {
		t.Fatalf("the stale write: %v", err)
	}
	if v, err = c.KVPut(ctx, "a/b", []byte("two"), PutOpts{IfMatch: 1}); err != nil || v != 2 {
		t.Fatalf("the CAS write: %d (%v)", v, err)
	}
	// if_absent refuses where a live value is.
	if _, err := c.KVPut(ctx, "a/b", []byte("three"), PutOpts{IfAbsent: true}); !errors.Is(err, ErrConflict) {
		t.Fatalf("the absent write: %v", err)
	}
	// TTL lands as an expiry the panel reports.
	if _, err := c.KVPut(ctx, "temp", []byte("t"), PutOpts{TTL: 60 * time.Second}); err != nil {
		t.Fatal(err)
	}
	keys, next, err := c.KVList(ctx, "", "", 100)
	if err != nil || next != "" || len(keys) != 2 {
		t.Fatalf("list: %+v %q (%v)", keys, next, err)
	}
	var sawExpiry bool
	for _, k := range keys {
		if k.Key == "temp" && k.ExpiresAt != nil {
			sawExpiry = true
		}
	}
	if !sawExpiry {
		t.Fatal("the ttl key has no expiry")
	}
	// Delete with a wrong version is a conflict; without one it deletes.
	if err := c.KVDelete(ctx, "a/b", 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("the stale delete: %v", err)
	}
	if err := c.KVDelete(ctx, "a/b", 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.KVGet(ctx, "a/b"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the key survived: %v", err)
	}
	// Keys outside the grammar never reach the wire.
	if _, _, err := c.KVGet(ctx, "bad key!"); err == nil {
		t.Fatal("the bad key went through")
	}
	// A value over 64 KiB is refused on this side already.
	if _, err := c.KVPut(ctx, "big", make([]byte, MaxValue+1), PutOpts{}); err == nil {
		t.Fatal("the oversize went through")
	}
}

func TestCASConcurrentCounter(t *testing.T) {
	c, _ := newClient(t)
	ctx := context.Background()
	const writers, each = 6, 10
	var wg sync.WaitGroup
	errs := make(chan error, writers*each)
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				err := c.CAS(ctx, "counter", 16, func(val []byte, ver int64) (Update, error) {
					n := 0
					for _, ch := range val {
						n = n*10 + int(ch-'0')
					}
					return Update{Val: []byte(itoa(n + 1))}, nil
				})
				if err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	val, _, err := c.KVGet(ctx, "counter")
	if err != nil || string(val) != itoa(writers*each) {
		t.Fatalf("the counter is %s (%v), want %d", val, err, writers*each)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestEventsAndAck(t *testing.T) {
	c, f := newClient(t)
	ctx := context.Background()
	f.Emit("payment.paid", 1, 2, map[string]int{"amount": 100})
	f.Emit("bot.paid", 1, 2, map[string]int{"x": 1})
	// The types filter is the server's job; the client passes it.
	events, err := c.Events(ctx, []string{"payment.paid"}, 0, 10, 0)
	if err != nil || len(events) != 1 || events[0].Type != "payment.paid" {
		t.Fatalf("events: %+v (%v)", events, err)
	}
	if events[0].UserID != 1 || events[0].TgID != 2 {
		t.Fatalf("the ids: %+v", events[0])
	}
	if err := c.EventsAck(ctx, events[0].ID); err != nil {
		t.Fatal(err)
	}
	if f.Cursor() != 1 {
		t.Fatalf("the cursor is %d", f.Cursor())
	}
	// After the ack the feed is empty without ?after.
	if events, err = c.Events(ctx, nil, 0, 10, 0); err != nil || len(events) != 1 {
		t.Fatalf("the rest: %+v (%v)", events, err)
	}
	// A malformed ack is refused.
	if err := c.EventsAck(ctx, 0); err == nil {
		t.Fatal("the ack of nothing went through")
	}
}

func TestErrorShape(t *testing.T) {
	c, f := newClient(t)
	// A route the panel does not serve: a 404 with the panel's code.
	_, _, _, err := c.do(context.Background(), "GET", "/nope", nil, nil, nil, 1<<10)
	if err == nil || !strings.Contains(err.Error(), "not_found") {
		t.Fatalf("the unknown route: %v", err)
	}
	_ = f
}
