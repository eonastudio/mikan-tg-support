/* The addon's admin page. It runs in the panel's sandboxed frame: no cookies, no
   storage, no network of its own — every call goes to the parent window over
   postMessage and comes back as {type:"rpc-result"} (the panel forwards it to the
   addon's /v2/ui/rpc). All dynamic text is rendered with textContent/createElement:
   a ticket carries what users wrote, and nothing of it may come alive as HTML. */
"use strict";

/* ---------------- rpc bridge ---------------- */

let rpcSeq = 0;
const pending = new Map();

function rpc(method, path, body) {
  return new Promise((resolve, reject) => {
    const id = ++rpcSeq;
    const timer = setTimeout(() => {
      if (pending.has(id)) {
        pending.delete(id);
        reject({ status: 0, body: { code: "timeout", message: "the call took too long" } });
      }
    }, 40000);
    pending.set(id, { resolve, reject, timer });
    window.parent.postMessage({ type: "rpc", id, method, path, body: body ?? null }, "*");
  });
}

window.addEventListener("message", (ev) => {
  const d = ev.data;
  if (!d || d.type !== "rpc-result") return;
  const p = pending.get(d.id);
  if (!p) return;
  pending.delete(d.id);
  clearTimeout(p.timer);
  if (d.status >= 200 && d.status < 300) p.resolve(d.body);
  else p.reject({ status: d.status, body: d.body });
});

/* ---------------- i18n ---------------- */

const I18N = {
  ru: {
    "app.title": "Тикеты поддержки",
    "tab.tickets": "Тикеты", "tab.settings": "Настройки", "tab.status": "Статус",
    "filter.open": "Открытые", "filter.closed": "Закрытые", "filter.all": "Все",
    "search.ph": "Поиск: текст, @username, tg id…",
    "list.empty": "Тикетов нет",
    "list.count": (n) => `${n} шт.`,
    "t.reply.ph": "Ответ поддержки… (Enter — отправить, Shift+Enter — перенос)",
    "t.send": "Отправить", "t.aiSend": "✦ Отправить ИИ", "t.aiDraft": "✦ Черновик ИИ",
    "t.close": "Закрыть", "t.reopen": "Открыть снова", "t.delete": "Удалить",
    "t.confirmDelete": "Удалить тикет безвозвратно?",
    "t.you": "вы", "t.ai": "ИИ", "t.system": "система",
    "profile.title": "Клиент (данные панели)",
    "profile.none": "Нет данных: пользователь не связан с панелью или событий ещё не было.",
    "profile.sub": "Подписка", "profile.until": "до", "profile.expired": "истекла",
    "profile.traffic": "Трафик", "profile.t90": "90% периода", "profile.t100": "100% — исчерпан",
    "profile.payments": "Платежи", "profile.refunded": "возврат",
    "profile.updated": "обновлено",
    "set.bot": "Telegram-бот поддержки",
    "set.token": "Токен бота (от @BotFather)",
    "set.tokenSet": "Токен сохранён — оставьте пустым, чтобы не менять",
    "set.tokenNot": "Токен не задан: бот не запущен",
    "set.api": "API Telegram (если нужен зеркалирующий прокси)",
    "set.apiHelp": "Пусто — https://api.telegram.org",
    "set.greeting": "Приветствие (/start); пусто — встроенный текст",
    "set.testBot": "Проверить бота",
    "set.ai": "ИИ-поддержка (OpenAI-совместимый API)",
    "set.aiOn": "Включить ИИ",
    "set.aiMode": "Режим",
    "set.aiModeHelp": "off — выключен; draft — черновики для агента; auto — ИИ отвечает сам после задержки, если агент не ответил (агента можно позвать словом «оператор»)",
    "set.aiBase": "Base URL (…/v1)",
    "set.aiKey": "API-ключ",
    "set.aiKeySet": "Ключ сохранён — оставьте пустым, чтобы не менять",
    "set.aiKeyNot": "Ключ не задан",
    "set.aiModel": "Модель",
    "set.aiDelay": "Задержка автоответа, сек",
    "set.aiDelayHelp": "Сколько ИИ ждёт ответа агента в режиме auto",
    "set.aiLang": "Язык ответов ИИ",
    "set.aiLangAuto": "язык пользователя",
    "set.aiSystem": "Дополнительные правила для ИИ",
    "set.aiKnowledge": "База знаний (FAQ) для ИИ",
    "set.aiKnowledgeHelp": "Текст от лица администратора: тарифы, инструкции, частые вопросы. ИИ опирается на него и не выдумывает факты.",
    "set.testAI": "Проверить ИИ",
    "set.other": "Прочее",
    "set.retention": "Хранить закрытые тикеты, дней",
    "set.save": "Сохранить",
    "set.saved": "Сохранено",
    "set.clearToken": "Удалить токен бота", "set.clearKey": "Удалить ключ ИИ",
    "set.cleared": "Удалено",
    "st.bot": "Бот", "st.feed": "События панели", "st.ai": "ИИ", "st.panel": "Панель", "st.kv": "Хранилище (KV)", "st.app": "Аддон",
    "st.reindex": "Перестроить индекс", "st.prune": "Очистить старые закрытые",
    "st.yes": "да", "st.no": "нет",
    "bot.on": "бот на связи", "bot.off": "бот не запущен", "bot.err": "ошибка бота", "bot.none": "бот не настроен",
    "err.generic": (c, m) => `${c || "error"}: ${m || ""}`,
  },
  en: {
    "app.title": "Support tickets",
    "tab.tickets": "Tickets", "tab.settings": "Settings", "tab.status": "Status",
    "filter.open": "Open", "filter.closed": "Closed", "filter.all": "All",
    "search.ph": "Search: text, @username, tg id…",
    "list.empty": "No tickets",
    "list.count": (n) => `${n}`,
    "t.reply.ph": "Support reply… (Enter to send, Shift+Enter for a new line)",
    "t.send": "Send", "t.aiSend": "✦ Send AI", "t.aiDraft": "✦ AI draft",
    "t.close": "Close", "t.reopen": "Reopen", "t.delete": "Delete",
    "t.confirmDelete": "Delete the ticket for good?",
    "t.you": "you", "t.ai": "AI", "t.system": "system",
    "profile.title": "Client (panel data)",
    "profile.none": "No data: the user is not linked to the panel or there were no events yet.",
    "profile.sub": "Subscription", "profile.until": "until", "profile.expired": "expired",
    "profile.traffic": "Traffic", "profile.t90": "90% of the period", "profile.t100": "100% — exhausted",
    "profile.payments": "Payments", "profile.refunded": "refunded",
    "profile.updated": "updated",
    "set.bot": "The support Telegram bot",
    "set.token": "Bot token (from @BotFather)",
    "set.tokenSet": "A token is saved — leave empty to keep it",
    "set.tokenNot": "No token: the bot is not running",
    "set.api": "Telegram API (when a mirroring proxy is needed)",
    "set.apiHelp": "Empty — https://api.telegram.org",
    "set.greeting": "The /start greeting; empty — the built-in text",
    "set.testBot": "Test the bot",
    "set.ai": "AI support (an OpenAI-compatible API)",
    "set.aiOn": "Enable the AI",
    "set.aiMode": "Mode",
    "set.aiModeHelp": "off — disabled; draft — drafts for the agent; auto — the AI answers by itself after the delay when no agent did (a user can call a human with «operator»)",
    "set.aiBase": "Base URL (…/v1)",
    "set.aiKey": "API key",
    "set.aiKeySet": "A key is saved — leave empty to keep it",
    "set.aiKeyNot": "No key",
    "set.aiModel": "Model",
    "set.aiDelay": "Auto answer delay, s",
    "set.aiDelayHelp": "How long the AI waits for an agent in auto mode",
    "set.aiLang": "The AI's answer language",
    "set.aiLangAuto": "the user's language",
    "set.aiSystem": "Extra rules for the AI",
    "set.aiKnowledge": "Knowledge base (FAQ) for the AI",
    "set.aiKnowledgeHelp": "The admin's own text: plans, how-tos, frequent questions. The AI leans on it and invents nothing.",
    "set.testAI": "Test the AI",
    "set.other": "Other",
    "set.retention": "Keep closed tickets, days",
    "set.save": "Save",
    "set.saved": "Saved",
    "set.clearToken": "Remove the bot token", "set.clearKey": "Remove the AI key",
    "set.cleared": "Removed",
    "st.bot": "Bot", "st.feed": "Panel events", "st.ai": "AI", "st.panel": "Panel", "st.kv": "Storage (KV)", "st.app": "Addon",
    "st.reindex": "Rebuild the index", "st.prune": "Prune old closed",
    "st.yes": "yes", "st.no": "no",
    "bot.on": "bot online", "bot.off": "bot stopped", "bot.err": "bot error", "bot.none": "bot not configured",
    "err.generic": (c, m) => `${c || "error"}: ${m || ""}`,
  },
};

let lang = "ru";
const t = (k) => (I18N[lang] && I18N[lang][k]) || (I18N.ru[k]) || k;

/* ---------------- dom helpers ---------------- */

const $ = (sel) => document.querySelector(sel);
const $$ = (sel) => Array.from(document.querySelectorAll(sel));

function el(tag, cls, text) {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text !== undefined && text !== null) n.textContent = String(text);
  return n;
}

function toast(msg, isErr) {
  const n = $("#toast");
  n.textContent = msg;
  n.className = "toast" + (isErr ? " err" : "");
  clearTimeout(toast._t);
  toast._t = setTimeout(() => n.classList.add("hidden"), 3200);
}

function fail(e) {
  const b = e && e.body ? e.body : {};
  toast(t("err.generic")(b.code, b.message) || "error", true);
}

function ago(unix) {
  if (!unix) return "";
  const s = Math.max(0, Math.floor(Date.now() / 1000 - unix));
  if (s < 60) return s + "s";
  if (s < 3600) return Math.floor(s / 60) + "m";
  if (s < 86400) return Math.floor(s / 3600) + "h";
  return Math.floor(s / 86400) + "d";
}

function when(unix) {
  if (!unix) return "—";
  return new Date(unix * 1000).toLocaleString(lang === "ru" ? "ru-RU" : "en-US", {
    day: "2-digit", month: "2-digit", hour: "2-digit", minute: "2-digit",
  });
}

/* ---------------- state ---------------- */

const state = {
  view: "tickets",
  status: "open",
  search: "",
  bootstrap: null,
  tickets: [],
  ticket: null,      // the open dialog's ticket
  messages: [],
  profile: null,
  timers: [],
};

function setView(v) {
  state.view = v;
  $$(".view").forEach((n) => n.classList.add("hidden"));
  $$(".tab").forEach((n) => n.classList.toggle("active", n.dataset.view === v));
  if (v === "tickets") { $("#view-tickets").classList.remove("hidden"); loadTickets(); }
  if (v === "ticket") { $("#view-ticket").classList.remove("hidden"); }
  if (v === "settings") { $("#view-settings").classList.remove("hidden"); loadSettings(); }
  if (v === "status") { $("#view-status").classList.remove("hidden"); loadStatus(); }
  resetTimers();
}

function resetTimers() {
  state.timers.forEach(clearInterval);
  state.timers = [];
  if (state.view === "tickets") state.timers.push(setInterval(loadTicketsSilently, 10000));
  if (state.view === "ticket") state.timers.push(setInterval(refreshDialog, 5000));
  if (state.view === "status") state.timers.push(setInterval(loadStatus, 10000));
  // The badge follows the bot wherever the admin is.
  state.timers.push(setInterval(refreshBadge, 15000));
}

/* ---------------- badge & bootstrap ---------------- */

function badgeOf(bot) {
  const n = $("#bot-badge");
  if (!bot.configured) { n.textContent = t("bot.none"); n.className = "badge off"; return; }
  if (bot.last_error) { n.textContent = t("bot.err"); n.className = "badge err"; n.title = bot.last_error; return; }
  if (bot.running) {
    n.textContent = (bot.username || t("bot.on")) + (bot.last_update_at ? " · " + ago(bot.last_update_at) : "");
    n.className = "badge ok"; n.title = bot.bot_name || "";
    return;
  }
  n.textContent = t("bot.off"); n.className = "badge off";
}

async function refreshBadge() {
  try {
    const b = await rpc("GET", "/bootstrap");
    state.bootstrap = b;
    badgeOf(b.bot);
  } catch (e) { /* the panel may be mid-save; the next round heals it */ }
}

async function boot() {
  try {
    const b = await rpc("GET", "/bootstrap");
    state.bootstrap = b;
    if (b.lang === "en" || b.lang === "ru") lang = b.lang;
  } catch (e) {
    fail(e);
  }
  translateStatic();
  badgeOf((state.bootstrap && state.bootstrap.bot) || { configured: false });
  setView("tickets");
}

function translateStatic() {
  document.documentElement.lang = lang;
  $("#app-title").textContent = t("app.title");
  document.title = t("app.title");
  const tabs = { tickets: "tab.tickets", settings: "tab.settings", status: "tab.status" };
  $$(".tab").forEach((n) => { n.textContent = t(tabs[n.dataset.view]); });
  $$(".chip").forEach((n) => { n.textContent = t("filter." + (n.dataset.status || "all")); });
  $("#search").placeholder = t("search.ph");
  $("#refresh").title = t("tab.tickets");
  $("#reply").placeholder = t("t.reply.ph");
  $("#t-send").textContent = t("t.send");
  $("#t-ai-send").textContent = t("t.aiSend");
  $("#t-ai-draft").textContent = t("t.aiDraft");
  $("#t-close").textContent = t("t.close");
  $("#t-reopen").textContent = t("t.reopen");
  $("#t-delete").textContent = t("t.delete");
  $$("[data-i18n]").forEach((n) => { n.textContent = t(n.dataset.i18n); });
  $("#s-token-state").textContent = "";
  $("#test-bot").textContent = t("set.testBot");
  $("#test-ai").textContent = t("set.testAI");
  $("#save").textContent = t("set.save");
  $("#clear-token").textContent = t("set.clearToken");
  $("#clear-key").textContent = t("set.clearKey");
  $("#reindex").textContent = t("st.reindex");
  $("#prune").textContent = t("st.prune");
  $("#back").textContent = "←";
}

/* ---------------- tickets list ---------------- */

async function loadTickets() {
  try {
    const r = await rpc("GET", "/tickets?status=" + encodeURIComponent(state.status));
    state.tickets = r.tickets || [];
    renderList();
  } catch (e) { fail(e); }
}

async function loadTicketsSilently() {
  try {
    const r = await rpc("GET", "/tickets?status=" + encodeURIComponent(state.status));
    state.tickets = r.tickets || [];
    renderList();
  } catch (e) { /* silent */ }
}

function matchesSearch(h) {
  const q = state.search.trim().toLowerCase();
  if (!q) return true;
  return (h.subject || "").toLowerCase().includes(q)
    || (h.username || "").toLowerCase().includes(q)
    || (h.first_name || "").toLowerCase().includes(q)
    || String(h.tg_id || "").includes(q);
}

function renderList() {
  const list = $("#ticket-list");
  list.textContent = "";
  const rows = state.tickets.filter(matchesSearch);
  $("#list-empty").classList.toggle("hidden", rows.length > 0);
  $("#list-empty").textContent = t("list.empty");
  const cnt = t("list.count");
  $("#list-count").textContent = typeof cnt === "function" ? cnt(rows.length) : rows.length;
  for (const h of rows) {
    const row = el("div", "ticket-row");
    row.setAttribute("role", "listitem");
    row.tabIndex = 0;
    row.appendChild(el("span", "tid", "#" + h.id));
    const mid = el("div");
    mid.appendChild(el("div", "tsubject", h.subject || "…"));
    const who = h.username ? "@" + h.username : (h.first_name || "");
    mid.appendChild(el("div", "twho", `${who || "—" } · tg ${h.tg_id} · ${h.msg_count} ✉`));
    row.appendChild(mid);
    const right = el("div", "tright");
    if (h.unread > 0) right.appendChild(el("span", "pill unread", "+" + h.unread));
    if (h.last_from === "ai") right.appendChild(el("span", "pill ai", t("t.ai")));
    right.appendChild(el("span", "pill " + h.status, h.status === "open" ? t("filter.open") : t("filter.closed")));
    right.appendChild(el("span", "twhen", ago(h.updated_at)));
    row.appendChild(right);
    const open = () => openTicket(h.id);
    row.addEventListener("click", open);
    row.addEventListener("keydown", (ev) => { if (ev.key === "Enter") open(); });
    list.appendChild(row);
  }
}

/* ---------------- dialog ---------------- */

async function openTicket(id) {
  try {
    const r = await rpc("GET", "/tickets/" + id);
    state.ticket = r.ticket; state.messages = r.messages || []; state.profile = r.profile;
    renderDialog();
    $("#view-tickets").classList.add("hidden");
    setView("ticket");
    $("#reply").focus();
  } catch (e) { fail(e); }
}

async function refreshDialog() {
  if (!state.ticket) return;
  try {
    const r = await rpc("GET", "/tickets/" + state.ticket.id);
    const changed = r.ticket.seq !== state.ticket.seq || r.ticket.status !== state.ticket.status
      || (r.messages || []).length !== state.messages.length;
    state.ticket = r.ticket; state.messages = r.messages || []; state.profile = r.profile;
    if (changed) renderDialog(true);
  } catch (e) { /* silent */ }
}

function renderDialog(keepScroll) {
  const tk = state.ticket;
  const msgs = $("#messages");
  const atBottom = keepScroll && msgs.scrollTop + msgs.clientHeight >= msgs.scrollHeight - 60;
  const head = $("#t-head");
  head.textContent = "";
  head.appendChild(el("span", "tid", "#" + tk.id));
  head.appendChild(el("span", "tsubject", tk.subject || "…"));
  const who = tk.username ? "@" + tk.username : [tk.first_name, tk.last_name].filter(Boolean).join(" ");
  head.appendChild(el("span", "muted", `${who || "—"} · tg ${tk.tg_id} · ${when(tk.created_at)}`));
  head.appendChild(el("span", "pill " + tk.status, tk.status === "open" ? t("filter.open") : t("filter.closed")));
  if (tk.no_ai) head.appendChild(el("span", "pill ai", "human only"));

  $("#t-close").classList.toggle("hidden", tk.status !== "open");
  $("#t-reopen").classList.toggle("hidden", tk.status !== "closed");
  $("#t-delete").classList.toggle("hidden", false);
  const b = state.bootstrap || {};
  const aiOn = !!(b.config && b.config.ai_enabled && b.config.ai_ready);
  $("#t-ai-draft").classList.toggle("hidden", !aiOn);
  $("#t-ai-send").classList.toggle("hidden", !aiOn);
  $("#t-send").disabled = false; // a reply to a closed ticket reopens it, so the composer stays live

  msgs.textContent = "";
  for (const m of state.messages) msgs.appendChild(renderMsg(m));
  if (atBottom || !keepScroll) msgs.scrollTop = msgs.scrollHeight;

  renderProfile();
}

function renderMsg(m) {
  const wrap = el("div", "msg " + m.from);
  const bubble = el("div", "bubble");
  if (m.text) bubble.appendChild(el("span", null, m.text));
  // The ticket this message belongs to, captured now: an agent may open another
  // dialog while the preview below is still loading.
  const tid = state.ticket ? state.ticket.id : 0;
  if (m.kind === "photo" && m.file_id) {
    const img = el("img", "thumb");
    img.alt = "photo";
    img.loading = "lazy";
    // The preview is fetched through the addon (a data URL); a file too large for
    // the frame's 1 MiB answer shows as a note instead.
    let tried = false;
    const load = async () => {
      if (tried) return;
      tried = true;
      try {
        const r = await rpc("GET", `/tickets/${tid}/media?seq=${m.seq}`);
        img.src = r.data_url;
        img.addEventListener("click", () => { $("#lightbox-img").src = r.data_url; $("#lightbox").classList.remove("hidden"); });
      } catch (e) {
        img.replaceWith(el("span", "file-note", "📷 " + ((e.body && e.body.message) || "photo")));
      }
    };
    img.src = "";
    bubble.appendChild(img);
    load();
  } else if (m.kind === "document") {
    bubble.appendChild(el("div", "file-note", `📄 ${m.file_name || "file"} · ${Math.round((m.file_size || 0) / 1024)} KB`));
  } else if (m.kind === "sticker") {
    bubble.appendChild(el("div", "file-note", "🎭 " + (m.text || "sticker")));
  }
  wrap.appendChild(bubble);
  const meta = el("div", "meta");
  if (m.from === "support") meta.appendChild(el("span", null, t("t.you")));
  if (m.from === "ai") meta.appendChild(el("span", null, t("t.ai") + (m.ai_model ? " · " + m.ai_model : "") + (m.ai_ms ? " · " + (m.ai_ms / 1000).toFixed(1) + "s" : "")));
  if (m.from === "system") meta.appendChild(el("span", null, t("t.system")));
  meta.appendChild(el("span", null, when(m.at)));
  wrap.appendChild(meta);
  return wrap;
}

function renderProfile() {
  const box = $("#profile");
  box.textContent = "";
  const p = state.profile;
  box.classList.remove("hidden");
  box.appendChild(el("h3", null, t("profile.title")));
  if (!p) { box.appendChild(el("p", "muted", t("profile.none"))); return; }
  const dl = el("dl");
  const row = (k, v, cls) => {
    dl.appendChild(el("dt", null, k));
    dl.appendChild(el("dd", cls || null, v));
  };
  if (p.expired) row(t("profile.sub"), t("profile.expired"), "bad");
  else if (p.expires_at) row(t("profile.sub"), t("profile.until") + " " + new Date(p.expires_at * 1000).toLocaleDateString(lang === "ru" ? "ru-RU" : "en-US") + (p.days_left ? ` (${p.days_left}d!)` : ""), p.days_left ? "warn" : null);
  else row(t("profile.sub"), "—");
  if (p.traffic_100) row(t("profile.traffic"), t("profile.t100"), "bad");
  else if (p.traffic_90) row(t("profile.traffic"), t("profile.t90"), "warn");
  if (p.user_id) row("user id", String(p.user_id));
  if (p.payments && p.payments.length) {
    const wrap = el("div", "pay");
    wrap.appendChild(el("h3", null, t("profile.payments")));
    for (const pay of p.payments.slice(0, 6)) {
      const line = `${new Date(pay.at * 1000).toLocaleDateString(lang === "ru" ? "ru-RU" : "en-US")} · ${(pay.amount / 100).toFixed(2)} ${pay.currency || ""} · ${pay.kind || ""}${pay.refunded ? " (" + t("profile.refunded") + ")" : ""}`;
      wrap.appendChild(el("div", pay.refunded ? "bad" : null, line.trim()));
    }
    box.appendChild(dl);
    box.appendChild(wrap);
  } else {
    box.appendChild(dl);
  }
  if (p.updated_at) box.appendChild(el("p", "muted", t("profile.updated") + ": " + ago(p.updated_at) + " ago"));
}

/* ---------------- dialog actions ---------------- */

async function sendReply() {
  const ta = $("#reply");
  const text = ta.value.trim();
  if (!text || !state.ticket) return;
  ta.value = "";
  $("#t-send").disabled = true;
  try {
    const r = await rpc("POST", `/tickets/${state.ticket.id}/reply`, { text });
    state.ticket = r.ticket;
    await refreshDialog();
  } catch (e) {
    ta.value = text; // the agent does not lose what they typed
    fail(e);
  } finally {
    $("#t-send").disabled = false;
  }
}

async function aiDraft() {
  if (!state.ticket) return;
  $("#t-ai-draft").disabled = true;
  try {
    const r = await rpc("POST", `/tickets/${state.ticket.id}/draft`);
    const ta = $("#reply");
    ta.value = ta.value ? ta.value + "\n" + r.text : r.text;
    ta.focus();
  } catch (e) { fail(e); } finally { $("#t-ai-draft").disabled = false; }
}

async function aiSend() {
  if (!state.ticket) return;
  $("#t-ai-send").disabled = true;
  try {
    await rpc("POST", `/tickets/${state.ticket.id}/ai-send`, {});
    await refreshDialog();
  } catch (e) { fail(e); } finally { $("#t-ai-send").disabled = false; }
}

async function closeTicket() {
  if (!state.ticket) return;
  try { await rpc("POST", `/tickets/${state.ticket.id}/close`); await refreshDialog(); } catch (e) { fail(e); }
}

async function reopenTicket() {
  if (!state.ticket) return;
  try { await rpc("POST", `/tickets/${state.ticket.id}/reopen`); await refreshDialog(); } catch (e) { fail(e); }
}

async function deleteTicket() {
  if (!state.ticket || !window.confirm(t("t.confirmDelete"))) return;
  try {
    await rpc("DELETE", "/tickets/" + state.ticket.id);
    state.ticket = null;
    setView("tickets");
  } catch (e) { fail(e); }
}

/* ---------------- settings ---------------- */

function fillSettings(cfg) {
  $("#s-token").value = "";
  $("#s-token").placeholder = cfg.has_bot_token ? "••••••••••" : "123456789:AA…";
  $("#s-token-state").textContent = cfg.has_bot_token ? t("set.tokenSet") : t("set.tokenNot");
  $("#s-api").value = cfg.telegram_api || "";
  $("#s-greeting").value = cfg.greeting || "";
  $("#s-ai-on").checked = !!cfg.ai_enabled;
  $("#s-ai-mode").value = cfg.ai_mode || "off";
  $("#s-ai-base").value = cfg.ai_base_url || "";
  $("#s-ai-key").value = "";
  $("#s-ai-key").placeholder = cfg.has_ai_key ? "••••••••••" : "sk-…";
  $("#s-ai-key-state").textContent = cfg.has_ai_key ? t("set.aiKeySet") : t("set.aiKeyNot");
  $("#s-ai-model").value = cfg.ai_model || "";
  $("#s-ai-delay").value = cfg.ai_delay_sec || 0;
  $("#s-ai-lang").value = cfg.ai_lang || "";
  $("#s-ai-system").value = cfg.ai_system || "";
  $("#s-ai-knowledge").value = cfg.ai_knowledge || "";
  $("#s-retention").value = cfg.retention_days || 60;
}

async function loadSettings() {
  try { fillSettings(await rpc("GET", "/settings")); } catch (e) { fail(e); }
}

async function saveSettings() {
  const body = {
    telegram_api: $("#s-api").value.trim(),
    greeting: $("#s-greeting").value,
    ai_enabled: $("#s-ai-on").checked,
    ai_mode: $("#s-ai-mode").value,
    ai_base_url: $("#s-ai-base").value.trim(),
    ai_model: $("#s-ai-model").value.trim(),
    ai_delay_sec: parseInt($("#s-ai-delay").value || "0", 10) || 0,
    ai_lang: $("#s-ai-lang").value,
    ai_system: $("#s-ai-system").value,
    ai_knowledge: $("#s-ai-knowledge").value,
    retention_days: parseInt($("#s-retention").value || "60", 10) || 60,
  };
  const tok = $("#s-token").value.trim();
  if (tok) body.bot_token = tok;
  const key = $("#s-ai-key").value.trim();
  if (key) body.ai_api_key = key;
  $("#save").disabled = true;
  try {
    const cfg = await rpc("PUT", "/settings", body);
    fillSettings(cfg);
    $("#save-out").textContent = t("set.saved");
    setTimeout(() => { $("#save-out").textContent = ""; }, 2500);
    refreshBadge();
  } catch (e) { fail(e); } finally { $("#save").disabled = false; }
}

async function clearSecret(which) {
  try {
    const cfg = await rpc("PUT", "/settings", { clear_secret: which });
    fillSettings(cfg);
    toast(t("set.cleared"));
    refreshBadge();
  } catch (e) { fail(e); }
}

async function testBot() {
  $("#test-bot").disabled = true;
  $("#test-bot-out").textContent = "…";
  try {
    const body = {};
    const tok = $("#s-token").value.trim();
    if (tok) body.token = tok;
    const api = $("#s-api").value.trim();
    if (api) body.api = api;
    const r = await rpc("POST", "/test/bot", body);
    $("#test-bot-out").textContent = r.ok ? `✓ ${r.username} (${r.name})` : `✗ ${r.error}`;
  } catch (e) { $("#test-bot-out").textContent = "✗ " + ((e.body && e.body.message) || e); }
  finally { $("#test-bot").disabled = false; }
}

async function testAI() {
  $("#test-ai").disabled = true;
  $("#test-ai-out").textContent = "…";
  try {
    const body = {};
    const b = $("#s-ai-base").value.trim(); if (b) body.base_url = b;
    const k = $("#s-ai-key").value.trim(); if (k) body.api_key = k;
    const m = $("#s-ai-model").value.trim(); if (m) body.model = m;
    const r = await rpc("POST", "/test/ai", body);
    $("#test-ai-out").textContent = r.ok ? `✓ ${r.model}: ${r.reply} (${r.ms} ms)` : `✗ ${r.error}`;
  } catch (e) { $("#test-ai-out").textContent = "✗ " + ((e.body && e.body.message) || e); }
  finally { $("#test-ai").disabled = false; }
}

/* ---------------- status ---------------- */

function dl(pairs) {
  const d = el("dl");
  for (const [k, v, cls] of pairs) {
    d.appendChild(el("dt", null, k));
    d.appendChild(el("dd", cls || null, v === "" || v === null || v === undefined ? "—" : String(v)));
  }
  return d;
}

function card(title, pairs) {
  const c = el("div", "card");
  c.appendChild(el("h3", null, title));
  c.appendChild(dl(pairs));
  return c;
}

async function loadStatus() {
  try {
    const s = await rpc("GET", "/status");
    const box = $("#status-body");
    box.textContent = "";
    const yn = (b) => (b ? t("st.yes") : t("st.no"));
    box.appendChild(card(t("st.app"), [
      ["version", s.version],
      ["uptime", Math.floor(s.uptime_s / 60) + "m"],
    ]));
    box.appendChild(card(t("st.bot"), [
      ["running", yn(s.bot.running), s.bot.running ? null : "bad"],
      ["configured", yn(s.bot.configured)],
      ["bot", s.bot.username],
      ["api", s.bot.api_base],
      ["offset", s.bot.offset],
      ["processed", s.bot.processed],
      ["last update", s.bot.last_update_at ? ago(s.bot.last_update_at) + " ago" : "—"],
      ["error", s.bot.last_error, s.bot.last_error ? "bad" : null],
    ]));
    box.appendChild(card(t("st.feed"), [
      ["permission", yn(s.feed.enabled), s.feed.enabled ? null : "bad"],
      ["applied", s.feed.applied],
      ["last event", s.feed.last_event_at ? ago(s.feed.last_event_at) + " ago" : "—"],
      ["error", s.feed.last_error, s.feed.last_error ? "bad" : null],
    ]));
    box.appendChild(card(t("st.ai"), [
      ["auto sent", s.ai.auto_sent],
      ["auto failed", s.ai.auto_failed, s.ai.auto_failed ? "bad" : null],
      ["drafts", s.ai.drafts],
      ["last model", s.ai.last_model],
      ["error", s.ai.last_error, s.ai.last_error ? "bad" : null],
    ]));
    box.appendChild(card(t("st.panel"), [
      ["id", s.panel.id],
      ["version", s.panel.version],
      ["permissions", (s.panel.permissions || []).join(", ")],
      ["checked", s.panel.last_at ? ago(s.panel.last_at) + " ago" : "—"],
      ["error", s.panel.last_error, s.panel.last_error ? "bad" : null],
    ]));
    box.appendChild(card(t("st.kv"), [
      ["tickets", s.kv.tickets],
      ["profiles", s.kv.profiles],
      ["error", s.kv.error, s.kv.error ? "bad" : null],
    ]));
  } catch (e) { fail(e); }
}

/* ---------------- wiring ---------------- */

function wire() {
  $$(".tab").forEach((n) => n.addEventListener("click", () => setView(n.dataset.view)));
  $$(".chip").forEach((n) => n.addEventListener("click", () => {
    $$(".chip").forEach((c) => c.classList.remove("active"));
    n.classList.add("active");
    state.status = n.dataset.status;
    loadTickets();
  }));
  let searchT = null;
  $("#search").addEventListener("input", (ev) => {
    state.search = ev.target.value;
    clearTimeout(searchT);
    searchT = setTimeout(renderList, 150);
  });
  $("#refresh").addEventListener("click", loadTickets);
  $("#back").addEventListener("click", () => { state.ticket = null; setView("tickets"); });
  $("#t-send").addEventListener("click", sendReply);
  $("#t-ai-draft").addEventListener("click", aiDraft);
  $("#t-ai-send").addEventListener("click", aiSend);
  $("#t-close").addEventListener("click", closeTicket);
  $("#t-reopen").addEventListener("click", reopenTicket);
  $("#t-delete").addEventListener("click", deleteTicket);
  $("#reply").addEventListener("keydown", (ev) => {
    if (ev.key === "Enter" && !ev.shiftKey) { ev.preventDefault(); sendReply(); }
  });
  $("#save").addEventListener("click", saveSettings);
  $("#test-bot").addEventListener("click", testBot);
  $("#test-ai").addEventListener("click", testAI);
  $("#clear-token").addEventListener("click", () => clearSecret("bot_token"));
  $("#clear-key").addEventListener("click", () => clearSecret("ai_api_key"));
  $("#reindex").addEventListener("click", async () => {
    try { const r = await rpc("POST", "/maintenance/reindex"); $("#maint-out").textContent = "✓ " + r.heads; } catch (e) { fail(e); }
  });
  $("#prune").addEventListener("click", async () => {
    try { const r = await rpc("POST", "/maintenance/prune"); $("#maint-out").textContent = "✓ −" + r.removed; } catch (e) { fail(e); }
  });
  $("#lightbox").addEventListener("click", () => {
    $("#lightbox").classList.add("hidden");
    $("#lightbox-img").src = "";
  });
  document.addEventListener("keydown", (ev) => {
    if (ev.key === "Escape") {
      $("#lightbox").classList.add("hidden");
      $("#lightbox-img").src = "";
    }
  });
}

wire();
boot();
