// Package meta is the addon's identity in one place: the id the panel knows it by
// (the admin must register the custom addon under exactly this id — the host checks
// /v2/info against the registration and rolls the install back on a mismatch), the
// version and the catalog texts.
package meta

// ID is the addon's id. The registration in the panel must name it exactly.
const ID = "tg-support"

// Version is the addon's own version (settable with -ldflags "-X
// tg-support/internal/meta.Version=…").
var Version = "0.1.0"

// Protocol is the addon protocol this speaks.
const Protocol = 2

// Kind and Mode as the catalog writes them.
const (
	Kind = "ui"
	Mode = "http"
)

// MinPanel is the oldest panel that runs it (protocol 2 appeared here).
const MinPanel = "0.5.1.0-rc.6"

// Permissions the addon needs; the admin approves exactly these at install.
var Permissions = []string{"ui", "kv", "events"}

// Name of the addon per language.
var Name = map[string]string{
	"ru": "Бот техподдержки (тикеты)",
	"en": "Support bot (tickets)",
}

// Description per language.
var Description = map[string]string{
	"ru": "Второй Telegram-бот технической поддержки: пользователи пишут боту — тикеты появляются в панели, агенты отвечают из панели. Заготовка ИИ-ответов с персонализацией по данным панели.",
	"en": "A second Telegram support bot: users write to the bot — tickets appear in the panel, agents answer from the panel. AI answers scaffold with personalization from the panel's data.",
}

// Help per language (the marketplace card).
var Help = map[string]string{
	"ru": "Создайте второго бота у @BotFather, вставьте его токен на странице аддона (Настройки). Страница аддона: Тикеты — диалоги с пользователями; Настройки — бот и ИИ; Статус — здоровье. Данные хранятся в KV панели.",
	"en": "Create a second bot with @BotFather and paste its token on the addon's page (Settings). The page: Tickets — the conversations; Settings — the bot and the AI; Status — health. State lives in the panel's KV.",
}

// Title of the admin page per language.
var Title = map[string]string{
	"ru": "Тикеты поддержки",
	"en": "Support tickets",
}
