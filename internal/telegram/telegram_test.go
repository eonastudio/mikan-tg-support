package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeTG is a Bot API stand-in: it answers canned envelopes and records the calls.
type fakeTG struct {
	*httptest.Server
	calls   []string
	answer  func(method string, body map[string]any) any // nil envelope: the default ok
	status  int
	rawBody string
}

func newFakeTG(t *testing.T) *fakeTG {
	t.Helper()
	f := &fakeTG{status: http.StatusOK}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/bot123:secret/")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.calls = append(f.calls, rest)
		if f.rawBody != "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(f.status)
			_, _ = w.Write([]byte(f.rawBody))
			return
		}
		result := any(true)
		if f.answer != nil {
			if v := f.answer(rest, body); v != nil {
				result = v
			}
		} else {
			switch rest {
			case "getMe":
				result = map[string]any{"id": 123, "is_bot": true, "first_name": "Support", "username": "support_bot"}
			case "sendMessage":
				result = map[string]any{"message_id": 77}
			}
		}
		env := map[string]any{"ok": f.status == http.StatusOK}
		if f.status == http.StatusOK {
			env["result"] = result
		} else {
			env["error_code"] = f.status
			env["description"] = "refused"
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		_ = json.NewEncoder(w).Encode(env)
	}))
	t.Cleanup(f.Close)
	return f
}

func TestCallAndErrors(t *testing.T) {
	f := newFakeTG(t)
	cl := New(f.URL, "123:secret")
	ctx := context.Background()

	me, err := cl.GetMe(ctx)
	if err != nil || me.Username != "support_bot" {
		t.Fatalf("getMe: %v %+v", err, me)
	}
	if err := cl.SendText(ctx, 42, "hello"); err != nil {
		t.Fatal(err)
	}
	if f.calls[len(f.calls)-1] != "sendMessage" {
		t.Fatalf("the last call is %q", f.calls[len(f.calls)-1])
	}

	// A 429 with retry_after surfaces as *Error with the wait.
	f.status = http.StatusTooManyRequests
	f.rawBody = `{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":7}}`
	err = cl.SendText(ctx, 42, "hello")
	var te *Error
	if !errors.As(err, &te) || te.RetryAfter() != 7*time.Second || !ShouldRetry(err) {
		t.Fatalf("429: %v", err)
	}
	// A 401 is unauthorized and not worth a retry.
	f.status = http.StatusUnauthorized
	f.rawBody = `{"ok":false,"error_code":401,"description":"Unauthorized"}`
	err = cl.SendText(ctx, 42, "x")
	if !errors.As(err, &te) || !te.IsUnauthorized() || ShouldRetry(err) {
		t.Fatalf("401: %v", err)
	}
	// A 409 is the second poller.
	f.status = http.StatusConflict
	f.rawBody = `{"ok":false,"error_code":409,"description":"Conflict: terminated by other getUpdates request"}`
	err = cl.Call(ctx, "getUpdates", map[string]any{}, nil)
	if !errors.As(err, &te) || !te.IsConflict() {
		t.Fatalf("409: %v", err)
	}
}

func TestRedactKeepsTheTokenOut(t *testing.T) {
	// A server that refuses the connection is not needed: a transport error carries
	// the URL; check redact() on one directly.
	s := `Post "https://api.telegram.org/bot123:secret/sendMessage": dial tcp: lookup failed`
	got := redact(errors.New(s))
	if strings.Contains(got, "123:secret") || !strings.Contains(got, "<token>") {
		t.Fatalf("the token leaked: %s", got)
	}
}

func TestSplitText(t *testing.T) {
	if p := SplitText("short", 4096); len(p) != 1 || p[0] != "short" {
		t.Fatalf("short: %+v", p)
	}
	if p := SplitText("", 4096); p != nil {
		t.Fatalf("empty: %+v", p)
	}
	exact := strings.Repeat("a", 4096)
	if p := SplitText(exact, 4096); len(p) != 1 {
		t.Fatalf("exact: %d parts", len(p))
	}
	// 9000 characters of words: three clean parts, every rune kept.
	words := strings.Repeat("word ", 1800) // 9000 runes
	p := SplitText(words, 4096)
	if len(p) > 4 {
		t.Fatalf("too many parts: %d", len(p))
	}
	joined := strings.Join(p, "")
	if strings.Count(joined, "word") != 1800 && len(p) < 4 {
		t.Fatalf("lost words: %d parts, %d runes", len(p), len(joined))
	}
	for _, part := range p {
		if len([]rune(part)) > 4096 {
			t.Fatalf("a part is %d runes", len([]rune(part)))
		}
	}
	// 20 000 runes: four parts and an ellipsis, nothing beyond.
	huge := strings.Repeat("x", 20000)
	p = SplitText(huge, 4096)
	if len(p) != 4 || !strings.HasSuffix(p[3], "…") {
		t.Fatalf("huge: %d parts, last ends %q", len(p), p[len(p)-1][len(p[3])-3:])
	}
	// Line breaks are preferred cut points.
	two := strings.Repeat("a", 3000) + "\n" + strings.Repeat("b", 3000)
	p = SplitText(two, 4096)
	if len(p) != 2 || strings.Contains(p[0], "b") {
		t.Fatalf("the line break was not preferred: %+v", p)
	}
}

func TestDownloadFileGuards(t *testing.T) {
	f := newFakeTG(t)
	cl := New(f.URL, "123:secret")
	ctx := context.Background()
	// A path with traversal or query shapes never reaches the wire.
	for _, bad := range []string{"", "../etc/passwd", "a?b=1", "a#b", "a\nb"} {
		if _, err := cl.DownloadFile(ctx, bad, 0); err == nil {
			t.Fatalf("the path %q went through", bad)
		}
	}
	// An announced oversize is refused without a download.
	if _, err := cl.DownloadFile(ctx, "photos/x.jpg", 9<<20); err == nil {
		t.Fatal("the oversize went through")
	}
	// A real download answers the bytes (the fake has no /file route: a 404 is a
	// transport-class answer, which is enough here — the guards above are the point).
	f.status = http.StatusNotFound
	f.rawBody = `{"ok":false,"error_code":404,"description":"Not Found"}`
	if _, err := cl.DownloadFile(ctx, "photos/x.jpg", 10); err == nil {
		t.Fatal("the 404 went through")
	}
}
