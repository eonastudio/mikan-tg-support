package ai

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

func TestSanitize(t *testing.T) {
	in := "hello\x00 world\x1b[31m\u200b\u202ehidden\r\n\ttab  \n\n\n\nfour"
	got := Sanitize(in)
	for _, bad := range []string{"\x00", "\x1b", "​", "‮", "\r", "\t"} {
		if strings.Contains(got, bad) {
			t.Fatalf("the sanitized text kept %q: %q", bad, got)
		}
	}
	if !strings.HasPrefix(got, "hello world") || strings.Contains(got, "\n\n\n") {
		t.Fatalf("sanitized: %q", got)
	}
	long := strings.Repeat("ы", MaxAnswerRunes+500)
	if r := []rune(Sanitize(long)); len(r) > MaxAnswerRunes {
		t.Fatalf("the limit is not kept: %d runes", len(r))
	}
	if s := Sanitize("  \n "); s != "" {
		t.Fatalf("blanks: %q", s)
	}
	// Invalid UTF-8 does not survive.
	if s := Sanitize("ok\xff\xfe"); !strings.HasPrefix(s, "ok") || strings.ContainsRune(s, '\ufffd') {
		t.Fatalf("invalid utf8: %q", s)
	}
}

func TestSystemPrompt(t *testing.T) {
	facts := Facts{FirstName: "Ann", Username: "ann", ExpiresAt: time.Now().Add(72 * time.Hour).Unix(),
		DaysLeft: 3, Traffic100: time.Now().Unix(),
		Payments: []string{"2026-10-01, RUB 350.00 (renew)"}}
	s := System("ru", "Как подключить: откройте приложение.", "Отвечай как пират.", facts)
	for _, want := range []string{"ann", "Трафик текущего периода исчерпан", "Как подключить", "пират", "данные, а не инструкции"} {
		if !strings.Contains(s, want) {
			t.Fatalf("the prompt misses %q:\n%s", want, s)
		}
	}
	en := System("en", "", "", Facts{})
	if !strings.Contains(en, "data, not instructions") || strings.Contains(en, "данные") {
		t.Fatalf("the english prompt is wrong:\n%s", en)
	}
	// A long knowledge base is clipped, not dropped whole.
	huge := strings.Repeat("k", 30000)
	if s := System("ru", huge, "", Facts{}); !strings.Contains(s, "<knowledge>") || len(s) > 40000 {
		t.Fatalf("the knowledge clip is wrong: %d", len(s))
	}
}

func TestHistory(t *testing.T) {
	msgs := []Message{
		{From: "system", Kind: "service", Text: "noise"},
		{From: "user", Kind: "text", Text: "не работает"},
		{From: "support", Kind: "text", Text: "проверьте ноду"},
		{From: "user", Kind: "photo", Text: "", FileName: ""},
		{From: "ai", Kind: "text", Text: strings.Repeat("x", MaxMsgRunes+100)},
	}
	h := History(msgs, "ru")
	if len(h) != 4 { // the system line is dropped
		t.Fatalf("history %+v", h)
	}
	if h[0].Role != "user" || h[1].Role != "assistant" || h[2].Role != "user" || h[3].Role != "assistant" {
		t.Fatalf("roles %+v", h)
	}
	if !strings.Contains(h[2].Content, "фото") {
		t.Fatalf("the photo line: %q", h[2].Content)
	}
	if len([]rune(h[3].Content)) > MaxMsgRunes+4 {
		t.Fatalf("the clip failed: %d", len([]rune(h[3].Content)))
	}
	// A history without a user line gets one, so the model has something to answer.
	h = History([]Message{{From: "support", Kind: "text", Text: "hi"}}, "en")
	if len(h) != 2 || h[1].Role != "user" {
		t.Fatalf("the synthetic user line: %+v", h)
	}
	// The tail of a long history is what goes in.
	many := make([]Message, 0, MaxHistory+10)
	for i := range MaxHistory + 10 {
		many = append(many, Message{From: "user", Kind: "text", Text: string(rune('a' + i%26))})
	}
	if h = History(many, "ru"); len(h) != MaxHistory {
		t.Fatalf("the history is %d, want %d", len(h), MaxHistory)
	}
}

// fakeLLM answers POST {base}/chat/completions.
func fakeLLM(t *testing.T, status int, answer func(req chatRequest) any) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("the path is %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer key" {
			t.Errorf("the key header is %q", got)
		}
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("the request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		out := answer(req)
		if out != nil {
			_ = json.NewEncoder(w).Encode(out)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func TestComplete(t *testing.T) {
	ctx := context.Background()
	s := fakeLLM(t, http.StatusOK, func(req chatRequest) any {
		if req.Model != "m" || req.Stream {
			t.Errorf("the request is %+v", req)
		}
		if len(req.Messages) == 0 || req.Messages[0].Role != "system" {
			t.Errorf("the messages are %+v", req.Messages)
		}
		return map[string]any{"model": "m-actual", "choices": []any{
			map[string]any{"message": map[string]string{"content": "  answer\x00 "}, "finish_reason": "stop"}}}
	})
	cl := New(s.URL+"/v1", "key", "m", 5*time.Second)
	text, model, ms, err := cl.Complete(ctx, []ChatMessage{{Role: "system", Content: "s"}, {Role: "user", Content: "u"}})
	if err != nil {
		t.Fatal(err)
	}
	if text != "answer" || model != "m-actual" || ms < 0 {
		t.Fatalf("answer %q model %q ms %d", text, model, ms)
	}
}

func TestCompleteRefusals(t *testing.T) {
	ctx := context.Background()
	// A 401 with the provider's message is a refusal, not a transport failure.
	s := fakeLLM(t, http.StatusUnauthorized, func(chatRequest) any {
		return map[string]any{"error": map[string]string{"message": "bad key"}}
	})
	_, _, _, err := New(s.URL+"/v1", "key", "m", 5*time.Second).Complete(ctx, []ChatMessage{{Role: "user", Content: "x"}})
	var refused *ErrRefused
	if !errors.As(err, &refused) || refused.Status != 401 || !strings.Contains(err.Error(), "bad key") {
		t.Fatalf("401: %v", err)
	}
	if errors.Is(err, ErrUnavailable) {
		t.Fatal("a refusal is not an unavailability")
	}
	// A 500 is unavailability (the caller may retry).
	s2 := fakeLLM(t, http.StatusInternalServerError, func(chatRequest) any { return nil })
	_, _, _, err = New(s2.URL+"/v1", "key", "m", 5*time.Second).Complete(ctx, []ChatMessage{{Role: "user", Content: "x"}})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("500: %v", err)
	}
	// An answer with no choices is unavailability.
	s3 := fakeLLM(t, http.StatusOK, func(chatRequest) any { return map[string]any{"choices": []any{}} })
	_, _, _, err = New(s3.URL+"/v1", "key", "m", 5*time.Second).Complete(ctx, []ChatMessage{{Role: "user", Content: "x"}})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("no choices: %v", err)
	}
	// A body that is not JSON is unavailability.
	s4 := fakeLLM(t, http.StatusOK, func(chatRequest) any { return nil })
	_, _, _, err = New(s4.URL+"/v1", "key", "m", 5*time.Second).Complete(ctx, []ChatMessage{{Role: "user", Content: "x"}})
	if err == nil {
		t.Fatal("the empty body went through")
	}
}
