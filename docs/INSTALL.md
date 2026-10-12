# Установка аддона tg-support

Три способа: **A** — кастомный образ из реестра (рекомендуется), **B** — офлайн
(архив образа, без доступа сервера к реестру), **C** — внешний процесс без Docker.
Все три регистрируют аддон в одном месте панели: **Аддоны → Маркетплейс → свой аддон**.

## 0. Требования и подготовка

1. **Версия панели.** Протокол 2 аддонов есть начиная с `0.5.1.0` (включая `-rc`).
   Проверьте версию в админке (или `mikan --version` на сервере). На более старых
   версиях раздел «Маркетплейс» не покажет установку своего аддона, а хост ответит
   `host_outdated` — сначала обновите панель и хост (`sudo mikan update`).
2. **Второй бот.** В Telegram у [@BotFather](https://t.me/BotFather):
   `/newbot` → имя → username (например `acme_support_bot`) → **сохраните токен**.
   Токен вставляется позже в настройках аддона (в админке панели), а не в файлах сервера.
   Рекомендуется сразу задать боту описание и аватар (`/setdescription`, `/setuserpic`).
3. **ID аддона — ровно `tg-support`.** После установки хост вызывает `GET /v2/info`
   аддона и сверяет `id` с тем, под которым аддон зарегистрировали; при несовпадении
   установка откатывается. Переопределить id можно только у внешнего процесса
   (переменная `MIKAN_ADDON_ID`), контейнеру хост дополнительных переменных не передаёт.

## A. Кастомный образ из реестра (рекомендуется)

На машине с Docker (не обязательно на сервере панели):

```sh
cd tg-support
docker buildx build --platform linux/amd64 --build-arg VERSION=0.1.0 \
  -t <ваш-реестр>/mikan-tg-support:0.1.0 --push .
# сервер ARM — собирайте обе архитектуры:
docker buildx build --platform linux/amd64,linux/arm64 --build-arg VERSION=0.1.0 \
  -t <ваш-реестр>/mikan-tg-support:0.1.0 --push .

# дайджест сборки (именно он вставляется в панель, не тег):
docker buildx imagetools inspect <ваш-реестр>/mikan-tg-support:0.1.0
# → Name:      <ваш-реестр>/mikan-tg-support:0.1.0
#   Digest:    sha256:0123456789abcdef…
```

Подойдёт любой реестр, до которого дотянется сервер панели: GHCR (`ghcr.io/<org>/…`),
Docker Hub, свой Harbor/registry. Для приватного реестра на сервере должен быть
`docker login` (хост тянет образ обычным `docker pull`).

В админке панели: **Аддоны → Маркетплейс → «Установить свой аддон»**:

| Поле | Значение |
|---|---|
| ID | `tg-support` |
| Источник | свой образ (custom) |
| Image | `<ваш-реестр>/mikan-tg-support` (без тега) |
| Digest | `sha256:0123456789abcdef…` |
| Kind | `ui` |
| Mode | `http` |
| Права | `ui`, `kv`, `events` |
| Память | 128 МБ (или 0 — значение по умолчанию) |
| Доверие | подтвердить |

То же самое одним запросом (сессия администратора, не API-ключ):

```sh
curl -X POST https://<панель>/api/v1/marketplace/custom \
  -H 'Cookie: <сессия админа>' -H 'Content-Type: application/json' \
  -d '{"id":"tg-support","source":"custom",
       "image":"<ваш-реестр>/mikan-tg-support",
       "digest":"sha256:0123456789abcdef…",
       "kind":"ui","mode":"http",
       "permissions":["ui","kv","events"],
       "mem_mb":128,"confirm":true}'
```

Хост поставит контейнер в сеть-мост `mikan-addons` (порт наружу только на loopback,
контейнер: read-only, без привилегий, пользователь 65532, перезапуск `unless-stopped`)
и в течение ~30 с дождётся `/v2/info`. Дальше — раздел «Первый запуск».

## B. Офлайн: архив образа (сервер без доступа к реестру)

1. Соберите образ на своей машине (команды из способа A, но `--load` вместо `--push`):

   ```sh
   docker buildx build --platform linux/amd64 --build-arg VERSION=0.1.0 \
     -t mikan-tg-support:0.1.0 --load .
   docker image inspect mikan-tg-support:0.1.0 --format '{{index .RepoDigests 0}}'
   ```

   Если `RepoDigests` пуст (образ не пушился), зафиксируйте дайджест пересборкой
   в локальный реестр или используйте `docker save` с тегом и укажите в панели
   digest из `docker buildx imagetools`/`skopeo inspect docker-archive:…`.
   Хост принимает образ строго по `image@digest`.

2. Сохраните образ **по дайджесту** и передайте архив на сервер:

   ```sh
   docker save <image>@sha256:… -o tg-support-0.1.0.tar
   scp tg-support-0.1.0.tar root@<сервер>:/tmp/
   ```

3. На сервере загрузите архив в движок и зарегистрируйте аддон в панели
   (как в способе A, те же image и digest):

   ```sh
   sudo mikan addon load /tmp/tg-support-0.1.0.tar --id tg-support
   ```

   `mikan addon load` помнит загруженную сборку, и последующая установка через панель
   не идёт в реестр (`pull_policy: never` + локальный образ). Порядок: сначала `load`,
   затем установка в панели.

## C. Внешний процесс (без Docker)

Аддон — статический бинарник; панель умеет подключать «внешний» аддон на
`http://127.0.0.1:<порт>`.

1. Соберите бинарник для сервера (Linux amd64; для ARM — `GOARCH=arm64`):

   ```sh
   CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
     -ldflags "-s -w -X tg-support/internal/meta.Version=0.1.0" -o tg-support .
   scp tg-support root@<сервер>:/opt/tg-support/tg-support
   ```

2. В панели: **Маркетплейс → свой аддон**: id `tg-support`, источник **external**,
   URL `http://127.0.0.1:41555` (свой свободный порт ≥1024), kind `ui`, mode `http`,
   права `ui`, `kv`, `events`, подтвердить. Внешний процесс не запускается хостом —
   панель лишь запомнит его адрес и **сгенерирует токены**.

3. На сервере возьмите токены из состояния хоста (файл читается только root):

   ```sh
   sudo cat /opt/mikan/addons/state.json
   # adapters → "tg-support" → token, panel_token; и panel_api (адрес API панели)
   ```

4. Systemd-юнit `/etc/systemd/system/tg-support.service`:

   ```ini
   [Unit]
   Description=mikan tg-support addon
   After=network-online.target mikan-panel.service
   Wants=network-online.target

   [Service]
   ExecStart=/opt/tg-support/tg-support
   Environment=MIKAN_ADAPTER_LISTEN=127.0.0.1:41555
   Environment=MIKAN_ADAPTER_TOKEN=<token из state.json>
   Environment=MIKAN_PANEL_URL=http://<panel_api из state.json>/panel/v2
   Environment=MIKAN_PANEL_TOKEN=<panel_token из state.json>
   Restart=always
   RestartSec=3
   User=tg-support
   NoNewPrivileges=true
   ProtectSystem=strict
   ProtectHome=true
   PrivateTmp=true
   ReadOnlyPaths=/

   [Install]
   WantedBy=multi-user.target
   ```

   ```sh
   sudo useradd -r -s /usr/sbin/nologin tg-support
   sudo systemctl daemon-reload && sudo systemctl enable --now tg-support
   ```

   Токены не меняются при обновлениях того же аддона; после переустановки
   (remove + install) панель выдаст новые — обновите unit.

## Первый запуск и проверка

1. Админка панели → **Аддоны** → в списке инструментов появился `tg-support`
   (статус running) → откройте его **страницу** (permission `ui`).
2. Вкладка **Настройки**: вставьте токен бота → **Проверить бота** (должен показать
   @username) → **Сохранить**. Поллинг стартует сразу.
3. Вкладка **Статус**: `bot: running`, `feed: permission yes` (если одобрили `events`).
   Первые секунды после установки панель может ещё не знать токены аддона —
   `panel_unavailable` в этот момент нормален и проходит сам (см. «Как это работает»
   в ARCHITECTURE.md).
4. Напишите боту в Telegram «тест» → тикет появился в списке → ответьте из админки →
   ответ пришёл в чат. Готово.

## Обновление

- **Способ A/B:** соберите образ новой версии, возьмите новый digest, в панели —
  «Установить свой аддон» с тем же id `tg-support` и новым дайджестом. Порты и токены
  сохраняются, состояние (KV) не трогается, простой — секунды на перезапуск контейнера.
- **Способ C:** замените бинарник и `systemctl restart tg-support`.
- Права при обновлении остаются прежними; если вы добавляете permission, панель
  попросит новое одобрение.

## Удаление

Админка → Аддоны → `tg-support` → удалить. Панель **сразу** отзывает grant и стирает
KV-данные аддона и его курсор событий: тикеты и настройки удаляются безвозвратно
(в бэкапах панели они остаются до истечения срока бэкапов). Контейнер останавливает
хост. Официальный бот mikan не затрагивается.

## Диагностика

| Симптом | Причина и лечение |
|---|---|
| Установка откатилась: «the addon answers as …» | id в панели не `tg-support`. Переставьте с верным id. |
| `host_outdated` при установке | Хост сервера старше 0.5.1.0: `sudo mikan update`, затем повторите. |
| Страница аддона: 502 `addon_unreachable` | Контейнер не отвечает: `docker logs mikan-addons-tg-support-1`, проверьте `docker ps`. |
| Статус: `bot error: refused by Telegram (401/403)` | Неверный/отозванный токен — перевыпустите у BotFather и сохраните новый. |
| Статус: `another poller … (409)` | Тем же токеном поллит кто-то ещё (второй контейнер, сторонний скрипт, токен влит в другую программу). Устраните дубль. |
| Статус: `feed: no permission` | При установке не одобрили `events` — переустановите аддон с правом `events` (карточка клиента останется пустой без него). |
| Telegram недоступен с сервера (таймауты getUpdates) | Настройте прокси/зеркало Bot API и укажите его в Настройках → «API Telegram» (например `http://127.0.0.1:8081` к локальному telegram-api-proxy). |
| `quota_exceeded` в логах | KV-квота аддона исчерпана (по умолчанию 256 МБ). Уменьшите срок хранения закрытых тикетов (Настройки) и нажмите «Очистить старые закрытые» на вкладке Статус; либо поднимите лимит настройкой панели `addons.kv_limit_mib`. |

Логи контейнера: `docker logs -f mikan-addons-tg-support-1` — структурированный JSON,
по умолчанию уровень info (предупреждения и ошибки видны всегда). Хост передаёт
кастомным контейнерам только фиксированные переменные `MIKAN_*`, поэтому уровень
логирования в рантайме не переключается; для внешнего процесса (способ C) доступна
переменная `MIKAN_LOG=debug`.
