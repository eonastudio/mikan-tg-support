// Package telegram is the client of the Bot API for the addon's own bot. The panel's
// Bot API proxy serves the panel's bot alone (one addon holds it), so the support bot
// is a bot of its own: the admin creates it with @BotFather and pastes the token on
// the addon's page; the addon polls api.telegram.org (or an admin-set mirror) itself.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"tg-support/internal/netguard"
)

// Limits of what the addon reads and sends.
const (
	maxAnswer   = 1 << 20 // a Bot API answer (updates batch included)
	maxFileSize = 8 << 20 // a file downloaded for the admin page (the UI takes ~700 KiB of it)
	MaxText     = 4096    // Telegram's message limit
)

// Error is a Bot API refusal: {"ok":false,"error_code":N,"description":…}.
type Error struct {
	Code        int
	Description string
	Parameters  *ResponseParameters `json:"-"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("telegram: %d %s", e.Code, e.Description)
}

// RetryAfter is the flood wait the error asks for (0 when it asks none).
func (e *Error) RetryAfter() time.Duration {
	if e.Parameters == nil || e.Parameters.RetryAfter <= 0 {
		return 0
	}
	return time.Duration(e.Parameters.RetryAfter) * time.Second
}

// ResponseParameters carries the flood wait and migration hints.
type ResponseParameters struct {
	RetryAfter    int   `json:"retry_after"`
	MigrateToChat int64 `json:"migrate_to_chat_id"`
}

// IsUnauthorized: the token is wrong or revoked.
func (e *Error) IsUnauthorized() bool {
	return e.Code == http.StatusUnauthorized || e.Code == http.StatusForbidden
}

// IsConflict: another poller runs with the same token (a second container, a leftover
// process, the token pasted elsewhere).
func (e *Error) IsConflict() bool { return e.Code == http.StatusConflict }

// ErrTransport: the way to Telegram failed.
var ErrTransport = errors.New("telegram: transport")

// Client calls one bot's methods. Safe for concurrent use.
type Client struct {
	base  string // e.g. https://api.telegram.org
	token string
	hc    *http.Client
}

// New is a Client for the bot token at the API base ("" is https://api.telegram.org).
func New(base, token string) *Client {
	if base == "" {
		base = "https://api.telegram.org"
	}
	return &Client{
		base:  strings.TrimSuffix(base, "/"),
		token: token,
		// getUpdates with timeout=50 answers at ~50 s; the client outlives it.
		hc: &http.Client{Timeout: 75 * time.Second, Transport: netguard.Transport(10 * time.Second),
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}
}

// envelope is the Bot API answer.
type envelope struct {
	OK          bool                `json:"ok"`
	Result      json.RawMessage     `json:"result"`
	ErrorCode   int                 `json:"error_code"`
	Description string              `json:"description"`
	Parameters  *ResponseParameters `json:"parameters"`
}

// Call runs one method with JSON args and decodes the result. A 429 is the *Error with
// its RetryAfter; a 5xx or a transport failure is ErrTransport (wrapped), so a caller
// can retry those and nothing else.
func (c *Client) Call(ctx context.Context, method string, args, out any) error {
	body, err := json.Marshal(args)
	if err != nil {
		return err
	}
	url := c.base + "/bot" + c.token + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrTransport, redact(err))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer+1))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrTransport, redact(err))
	}
	if int64(len(data)) > maxAnswer {
		return fmt.Errorf("%w: the answer is larger than %d bytes", ErrTransport, maxAnswer)
	}
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		if resp.StatusCode >= 500 {
			return fmt.Errorf("%w: HTTP %d", ErrTransport, resp.StatusCode)
		}
		return fmt.Errorf("telegram: %s: the answer is not JSON", method)
	}
	if !env.OK {
		e := &Error{Code: env.ErrorCode, Description: env.Description, Parameters: env.Parameters}
		if e.Code == 0 {
			e.Code = resp.StatusCode
		}
		return e
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		return fmt.Errorf("telegram: %s: result: %w", method, err)
	}
	return nil
}

// redact keeps a transport error from leaking the token in the URL.
func redact(err error) string {
	s := err.Error()
	// The only secret in a URL of ours is the token between /bot and the next /.
	if i := strings.Index(s, "/bot"); i >= 0 {
		if j := strings.Index(s[i+4:], "/"); j >= 0 {
			s = s[:i+4] + "<token>" + s[i+4+j:]
		}
	}
	return s
}

// User is a Telegram account.
type User struct {
	ID           int64  `json:"id"`
	IsBot        bool   `json:"is_bot"`
	FirstName    string `json:"first_name"`
	LastName     string `json:"last_name"`
	Username     string `json:"username"`
	LanguageCode string `json:"language_code"`
}

// Chat is where a message lives.
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"` // private | group | supergroup | channel
}

// PhotoSize is one resolution of a photo; the bot takes the largest it gets.
type PhotoSize struct {
	FileID   string `json:"file_id"`
	FileSize int64  `json:"file_size"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
}

// Document is a sent file.
type Document struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	MimeType string `json:"mime_type"`
	FileSize int64  `json:"file_size"`
}

// Sticker is a sticker.
type Sticker struct {
	FileID string `json:"file_id"`
	Emoji  string `json:"emoji"`
}

// Message is an incoming message (the fields the support bot reads).
type Message struct {
	MessageID int64       `json:"message_id"`
	From      *User       `json:"from"`
	Chat      Chat        `json:"chat"`
	Date      int64       `json:"date"`
	Text      string      `json:"text"`
	Caption   string      `json:"caption"`
	Photo     []PhotoSize `json:"photo"`
	Document  *Document   `json:"document"`
	Sticker   *Sticker    `json:"sticker"`
}

// Update is one getUpdates entry.
type Update struct {
	UpdateID int64    `json:"update_id"`
	Message  *Message `json:"message"`
}

// GetMe is the bot's own account.
func (c *Client) GetMe(ctx context.Context) (User, error) {
	var u User
	err := c.Call(ctx, "getMe", struct{}{}, &u)
	return u, err
}

// GetUpdates long-polls: offset acknowledges what is below it, timeout is Telegram's
// own (seconds), allowed narrows the update kinds.
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeout int, allowed []string) ([]Update, error) {
	args := map[string]any{"timeout": timeout}
	if offset != 0 {
		// A negative offset is Telegram's own "acknowledge the backlog" call; 0 or
		// absent acknowledges nothing.
		args["offset"] = offset
	}
	if len(allowed) > 0 {
		args["allowed_updates"] = allowed
	}
	var out []Update
	err := c.Call(ctx, "getUpdates", args, &out)
	return out, err
}

// SentMessage is what sendMessage answers with.
type SentMessage struct {
	MessageID int64 `json:"message_id"`
}

// SendText sends one message as plain text (no parse mode: what users and agents
// write is shown as it is, and nothing in a ticket can inject formatting). A text
// longer than Telegram's limit is split on line breaks into at most 4 parts.
func (c *Client) SendText(ctx context.Context, chatID int64, text string) error {
	for _, part := range SplitText(text, MaxText) {
		args := map[string]any{"chat_id": chatID, "text": part, "disable_web_page_preview": true}
		var sent SentMessage
		if err := c.Call(ctx, "sendMessage", args, &sent); err != nil {
			return err
		}
	}
	return nil
}

// SplitText cuts a text into parts of at most max runes, preferring line and word
// breaks; at most 4 parts, the rest is dropped with an ellipsis (an agent reply of
// 16 000 characters is a document, not a chat message).
func SplitText(s string, max int) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	r := []rune(s)
	if len(r) <= max {
		return []string{s}
	}
	const maxParts = 4
	var parts []string
	for len(r) > 0 && len(parts) < maxParts {
		if len(r) <= max {
			parts = append(parts, string(r))
			r = nil // the tail is in; the ellipsis below must not fire
			break
		}
		cut := max
		// Prefer the last line break, then the last space, in the window's tail.
		tail := r[:max]
		if i := lastIndex(tail, '\n'); i > max/2 {
			cut = i + 1
		} else if i := lastIndex(tail, ' '); i > max/2 {
			cut = i + 1
		}
		parts = append(parts, string(r[:cut]))
		r = r[cut:]
	}
	if len(r) > 0 { // something is left after the fourth part
		last := parts[len(parts)-1]
		lr := []rune(last)
		if len(lr) > 1 {
			parts[len(parts)-1] = string(lr[:len(lr)-1]) + "…"
		}
	}
	return parts
}

func lastIndex(r []rune, c rune) int {
	for i := len(r) - 1; i >= 0; i-- {
		if r[i] == c {
			return i
		}
	}
	return -1
}

// SendChatAction shows «typing…» while an answer is composed.
func (c *Client) SendChatAction(ctx context.Context, chatID int64) {
	_ = c.Call(ctx, "sendChatAction", map[string]any{"chat_id": chatID, "action": "typing"}, nil)
}

// SetMyCommands puts the bot's menu in place (called at every start; a failure is
// cosmetic and only logged).
func (c *Client) SetMyCommands(ctx context.Context) error {
	ru := []map[string]string{
		{"command": "start", "description": "Начать заново"},
		{"command": "status", "description": "Мои тикеты"},
		{"command": "close", "description": "Закрыть мой открытый тикет"},
	}
	en := []map[string]string{
		{"command": "start", "description": "Start over"},
		{"command": "status", "description": "My tickets"},
		{"command": "close", "description": "Close my open ticket"},
	}
	args := map[string]any{
		"commands": []map[string]string{
			{"command": "start", "description": "Начать заново / Start over"},
			{"command": "status", "description": "Мои тикеты / My tickets"},
			{"command": "close", "description": "Закрыть тикет / Close my ticket"},
		},
	}
	if err := c.Call(ctx, "setMyCommands", args, nil); err != nil {
		return err
	}
	// Per-language menus on top of the default; failures here stay cosmetic.
	_ = c.Call(ctx, "setMyCommands", map[string]any{"commands": ru,
		"scope": map[string]string{"type": "default"}, "language_code": "ru"}, nil)
	_ = c.Call(ctx, "setMyCommands", map[string]any{"commands": en,
		"scope": map[string]string{"type": "default"}, "language_code": "en"}, nil)
	return nil
}

// FileInfo is getFile's answer.
type FileInfo struct {
	FileID   string `json:"file_id"`
	FileSize int64  `json:"file_size"`
	FilePath string `json:"file_path"`
}

// GetFile asks where a file lives.
func (c *Client) GetFile(ctx context.Context, fileID string) (FileInfo, error) {
	var f FileInfo
	err := c.Call(ctx, "getFile", map[string]string{"file_id": fileID}, &f)
	return f, err
}

// DownloadFile reads a file of the bot (for the admin page's preview). size>0 refuses
// anything bigger without reading it; the read is capped at maxFileSize either way.
func (c *Client) DownloadFile(ctx context.Context, filePath string, size int64) ([]byte, error) {
	if size > maxFileSize {
		return nil, fmt.Errorf("telegram: the file is %d bytes, the limit is %d", size, maxFileSize)
	}
	// filePath comes from getFile and is a plain "photos/x.jpg"-style path; keep it that way.
	if filePath == "" || strings.ContainsAny(filePath, "?#\r\n") || strings.Contains(filePath, "..") {
		return nil, errors.New("telegram: not a file path")
	}
	u := c.base + "/file/bot" + c.token + "/" + filePath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTransport, redact(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: HTTP %d", ErrTransport, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTransport, redact(err))
	}
	if int64(len(data)) > maxFileSize {
		return nil, fmt.Errorf("telegram: the file is larger than %d bytes", maxFileSize)
	}
	return data, nil
}

// RetryAfterOf is the flood wait an error asks for, 0 when it asks none.
func RetryAfterOf(err error) time.Duration {
	var e *Error
	if errors.As(err, &e) {
		return e.RetryAfter()
	}
	return 0
}

// ShouldRetry: a transport failure or a 5xx worth another try.
func ShouldRetry(err error) bool {
	if errors.Is(err, ErrTransport) {
		return true
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Code == 429 || e.Code >= 500
	}
	return false
}

// ChatID is a chat id as the Bot API takes it.
func ChatID(id int64) string { return strconv.FormatInt(id, 10) }
