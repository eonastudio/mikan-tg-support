package store

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Config is cfg/main: what the admin set on the addon's page. The panel of this
// release draws no settings form for tools, so the addon keeps its settings itself,
// in the panel's KV. Secrets (the bot token, the AI key) live only here: the UI is
// told whether they are set, never what they are.
type Config struct {
	BotToken    string `json:"bot_token,omitempty"`
	TelegramAPI string `json:"telegram_api,omitempty"` // default https://api.telegram.org
	Greeting    string `json:"greeting,omitempty"`     // the bot's /start answer; "" is the built-in text

	AIEnabled   bool   `json:"ai_enabled,omitempty"`
	AIBaseURL   string `json:"ai_base_url,omitempty"` // an OpenAI-compatible API, …/v1
	AIAPIKey    string `json:"ai_api_key,omitempty"`
	AIModel     string `json:"ai_model,omitempty"`
	AIMode      string `json:"ai_mode,omitempty"`      // off | draft | auto
	AIDelaySec  int    `json:"ai_delay_sec,omitempty"` // auto: how long a user waits for a human first
	AILang      string `json:"ai_lang,omitempty"`      // ru | en | "" (the user's language)
	AISystem    string `json:"ai_system,omitempty"`    // extra rules for the model
	AIKnowledge string `json:"ai_knowledge,omitempty"` // the service's FAQ, plain text

	RetentionDays int `json:"retention_days,omitempty"` // prune closed tickets after; default 60
}

// AI modes.
const (
	AIOff   = "off"
	AIDraft = "draft" // the AI writes a draft, the agent sends it
	AIAuto  = "auto"  // the AI answers by itself until a human is asked for
)

const configKey = "cfg/main"

// Defaults.
const (
	DefaultTelegramAPI = "https://api.telegram.org"
	DefaultRetention   = 60
	DefaultAIDelaySec  = 120
	DefaultGreetingRU  = "Здравствуйте! Это бот технической поддержки.\nОпишите проблему — сообщение попадёт к команде, ответ придёт сюда же.\n\n/status — мои тикеты, /close — закрыть мой открытый тикет."
	DefaultGreetingEN  = "Hello! This is the technical support bot.\nDescribe your issue — it goes to the team, and the answer comes back here.\n\n/status — my tickets, /close — close my open ticket."
)

// APIBase is where the bot polls; the default when unset.
func (c Config) APIBase() string {
	if c.TelegramAPI == "" {
		return DefaultTelegramAPI
	}
	return strings.TrimSuffix(c.TelegramAPI, "/")
}

// Greet is the /start answer in lang, the built-in text when the admin set none.
func (c Config) Greet(lang string) string {
	if c.Greeting != "" {
		return c.Greeting
	}
	if strings.HasPrefix(strings.ToLower(lang), "ru") {
		return DefaultGreetingRU
	}
	return DefaultGreetingEN
}

// Retention is how long a closed ticket is kept.
func (c Config) Retention() time.Duration {
	d := c.RetentionDays
	if d <= 0 {
		d = DefaultRetention
	}
	return time.Duration(d) * 24 * time.Hour
}

// AIDelay is how long auto mode waits for a human before the AI answers.
func (c Config) AIDelay() time.Duration {
	d := c.AIDelaySec
	if d <= 0 {
		d = DefaultAIDelaySec
	}
	return time.Duration(d) * time.Second
}

// AIReady says whether the AI could answer at all.
func (c Config) AIReady() bool {
	return c.AIEnabled && c.AIMode != "" && c.AIMode != AIOff &&
		c.AIBaseURL != "" && c.AIAPIKey != "" && c.AIModel != ""
}

// webURL: an absolute http(s) URL with a host, or "".
func webURL(s string) bool {
	if s == "" {
		return true
	}
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != ""
}

// Check validates a config as the UI saved it: what is wrong is named by its field.
func (c Config) Check() error {
	switch {
	case c.TelegramAPI != "" && !webURL(c.TelegramAPI):
		return errors.New("telegram_api must be an http(s) URL")
	case len(c.Greeting) > 4096:
		return errors.New("greeting is at most 4096 characters")
	case c.AIBaseURL != "" && !webURL(c.AIBaseURL):
		return errors.New("ai_base_url must be an http(s) URL")
	case c.AIModel != "" && len(c.AIModel) > 256:
		return errors.New("ai_model is too long")
	case c.AIMode != "" && c.AIMode != AIOff && c.AIMode != AIDraft && c.AIMode != AIAuto:
		return errors.New("ai_mode is off, draft or auto")
	case c.AIDelaySec < 0 || c.AIDelaySec > 24*60*60:
		return errors.New("ai_delay_sec is 0 to 86400")
	case c.AILang != "" && c.AILang != "ru" && c.AILang != "en":
		return errors.New("ai_lang is ru or en")
	case len(c.AISystem) > 8000:
		return errors.New("ai_system is at most 8000 characters")
	case len(c.AIKnowledge) > 24000:
		return errors.New("ai_knowledge is at most 24000 characters")
	case c.RetentionDays < 0 || c.RetentionDays > 3650:
		return errors.New("retention_days is 0 to 3650")
	}
	return nil
}

// LoadConfig reads cfg/main; a fresh addon (no key) gets the zero config, which is
// all defaults.
func (s *Store) LoadConfig(ctx context.Context) (Config, error) {
	var c Config
	if _, err := getJSON(ctx, s, configKey, &c); err != nil && !errors.Is(err, ErrNotFound) {
		return Config{}, err
	}
	return c, nil
}

// SaveConfig applies fn to the stored config under CAS and writes what it returns.
// fn mutates the draft; the saved config is returned. What fn refuses (an error) is
// left as it was. Secrets keep their old value where the draft has "" and keep is set
// for them by the caller (the UI sends "" for an unchanged secret).
func (s *Store) SaveConfig(ctx context.Context, fn func(cur *Config) error) (Config, error) {
	var saved Config
	err := casJSON(ctx, s, configKey, func(cur *Config) (*Config, error) {
		base := Config{}
		if cur != nil {
			base = *cur
		}
		next := base
		if err := fn(&next); err != nil {
			return nil, err
		}
		if err := next.Check(); err != nil {
			return nil, err
		}
		saved = next
		return &next, nil
	})
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Config{}, err
	}
	if errors.Is(err, ErrNotFound) { // cannot happen: casJSON treats a miss as nil, not an error
		return Config{}, err
	}
	return saved, nil
}

// ConfigView is the config as the UI may see it: secrets become flags.
type ConfigView struct {
	HasBotToken   bool   `json:"has_bot_token"`
	TelegramAPI   string `json:"telegram_api,omitempty"`
	Greeting      string `json:"greeting,omitempty"`
	AIEnabled     bool   `json:"ai_enabled"`
	AIBaseURL     string `json:"ai_base_url,omitempty"`
	HasAIKey      bool   `json:"has_ai_key"`
	AIModel       string `json:"ai_model,omitempty"`
	AIMode        string `json:"ai_mode,omitempty"`
	AIDelaySec    int    `json:"ai_delay_sec,omitempty"`
	AILang        string `json:"ai_lang,omitempty"`
	AISystem      string `json:"ai_system,omitempty"`
	AIKnowledge   string `json:"ai_knowledge,omitempty"`
	RetentionDays int    `json:"retention_days,omitempty"`
	AIReady       bool   `json:"ai_ready"`
}

// View is the config without its secrets.
func (c Config) View() ConfigView {
	return ConfigView{
		HasBotToken: c.BotToken != "", TelegramAPI: c.TelegramAPI, Greeting: c.Greeting,
		AIEnabled: c.AIEnabled, AIBaseURL: c.AIBaseURL, HasAIKey: c.AIAPIKey != "",
		AIModel: c.AIModel, AIMode: c.AIMode, AIDelaySec: c.AIDelaySec, AILang: c.AILang,
		AISystem: c.AISystem, AIKnowledge: c.AIKnowledge, RetentionDays: c.RetentionDays,
		AIReady: c.AIReady(),
	}
}

// BotTokenSet replaces the token unless the new one is empty ("" keeps the old).
func (c *Config) SetBotToken(tok string) {
	if tok != "" {
		c.BotToken = strings.TrimSpace(tok)
	}
}

// SetAIKey replaces the key unless the new one is empty.
func (c *Config) SetAIKey(k string) {
	if k != "" {
		c.AIAPIKey = strings.TrimSpace(k)
	}
}

// ClearSecret drops one secret by name (the UI's «remove» buttons).
func (c *Config) ClearSecret(name string) error {
	switch name {
	case "bot_token":
		c.BotToken = ""
	case "ai_api_key":
		c.AIAPIKey = ""
	default:
		return fmt.Errorf("store: no secret %q", name)
	}
	return nil
}
