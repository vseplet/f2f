# ROADMAP

Планы и статус по крупным направлениям. Источник правды по уже
реализованному — соответствующий код и доки (ссылки ниже), здесь только
сводка.

## Сделано

- **QUIC-шина / inter-node на QUIC** — пир-к-пир общение и стримы сервисов
  идут по единой QUIC-шине (типы стримов `service.action`). См. `mesh/bus`,
  раздел «Миграция пир-к-пир HTTP → QUIC-шина» в [ARCHITECTURE.md](ARCHITECTURE.md).
- **Транспортное шифрование через AmneziaWG** — overlay-трафик шифруется
  AWG-транспортом. См. `mesh/engine` + `mesh/engine/awg`. Известный гэп:
  junk-параметры обфускации (jc/s1/s2) не прокинуты → handshake пока
  фиксированного размера (магические H1..H4 ranges есть).
- **Система уведомлений** — хаб событий: любой источник пушит
  `Notification`, она пишется в per-camp SQLite и раздаётся в UI по SSE.
  См. `services/notify`.
- **Распределённая БД + блоки** — лог-субстрат `db` (SQLite, anti-entropy sync,
  обнаружение scope'ов, инкрементальная свёртка, **гейтинг синка по членству
  канала**) + блок-движок `db/blocks`; на
  нём **каналы, сообщения и заметки со вложенными страницами**. Старый
  `services/messenger` снят, мессенджер целиком на блоках. См. [DB.md](DB.md),
  [BLOCKS.md](BLOCKS.md), [ENTITIES.md](ENTITIES.md).
- **Exit-only intercepts** — резолв domain-спеков через exit-пира
  (`resolve` bus-хендлер в `services/tunnel`), per-target NAT по таблице
  маршрутов пира, пиннинг ответов в локальный DNS (`dns.SetPinned` +
  `/etc/resolver/<domain>`, mac-only). Открыто: коллизии приватных IP
  (синтетические адреса) и политика выпуска.

## Вытеснено отдельными доками

- **Аутентификация, SSO, f2f как OIDC-провайдер** — реализовано и
  задокументировано отдельно: [OIDC.md](OIDC.md) (провайдер, клиенты,
  passkey/WebAuthn), [IDENTITY.md](IDENTITY.md) (модель пир=пользователь,
  профиль/членство на блоках) и [INVITE.md](INVITE.md) (инвайты, допуск). Код:
  `services/oidc`.

## Планы

### Распределённая БД и приложения над ней
Субстрат + блоки + мессенджер-на-блоках уже сделаны (см. «Сделано»). Осталось:
- **ACL**: гейтинг синка по членству — ✅ сделано (`Sync.SetMemberCheck`:
  `channel:`/`message:`/`note:<bid>` отдаются только членам). Осталось: **e2e**
  (group key на канал, эпохи) — защита при релее/утечке фрейма и forward-secrecy
  при исключении; гейтинг web-API/выдачи по членству (локальный UI пока видит всё).
- **Доставка**: frontier'ы (галочки sent/delivered/read), релей шифротекста.
- **Снапшоты/GC** перекрытых версий и тумбстоунов (лог растёт бесконечно).
- **Заметки/страницы**: reparent (перенос блока/страницы в др. родителя — `move`
  несёт `parent` + проверка циклов), правка `title` готовой страницы, каскадное
  удаление страницы с детьми; запрет дублей имён каналов.
- **Tabs/merge UX**: рендер нескольких heads (варианты), явный merge из UI.
- **Пагинация / ленивая загрузка (важно для масштаба).** Сейчас API отдаёт весь
  scope целиком, включая **инлайн-байты картинок (base64) всех страниц** — на
  каждый открытие и каждый фоновый refresh. Жирно по проводу и в DOM. Нужно:
  - **Заметки:** `/api/notes` отдаёт блоки **только текущей страницы** + список
    дочерних страниц (заголовки для ToC), остальное — по drill-in. Этого должно
    хватить.
  - **Байты вложений — не в JSON-списке:** отдельный blob-эндпоинт
    (`/api/notes/blob?channel&bid`, `Content-Type`+`Cache-Control`), рендер через
    `<img src>`; браузер кэширует по URL → заодно уходит мерцание/передекод картинок.
  - **Чат:** пагинация по **Lamport-курсору** (`before=<lamport>:<id>&limit=N`),
    окно + подгрузка старых при скролле вверх; `limit` на сервере уже есть, клиент
    его не шлёт и курсора нет. То же — не возить инлайн-байты вложений.

### Универсальное десктоп-приложение
- Один реактивный фронтенд на все платформы вместо jQuery + двух кодобаз
  (`source/mac/` embed и `source/desktop/` Wails).
- WebSocket/IPC push вместо HTTP-polling; браузерный fallback как SPA на
  loopback.
- Privilege elevation: app (user) поднимает engine (root) через системный
  диалог, общение по localhost (macOS — `osascript`, позже `SMAppService`).

### Сеть / NAT
- **Hairpin / same-NAT и симметричный NAT** — LAN host-кандидаты + релей-фоллбэк,
  когда прямой punch невозможен. См. [NAT.md](NAT.md).
- **Живучесть хоста** — восстановление после сна (скачок часов) и после потери
  маршрута на ходу (network-down errno) теперь оба дёргают `restartOnEphemeralPort`
  ✅. Остаётся: фиксированный listen-порт при рестарте (стабильный reflex) вместо
  ephemeral — чтобы при флаппинге сна пиры восстанавливались быстрее.
  См. [NAT.md](NAT.md) (разбор инцидента).

### Инженерные улучшения
- Structured logging на `log/slog` (уровни, `--verbose`/`LOG_LEVEL`).
- Тесты engine lifecycle, config round-trip, intercept matching, SFU
  signaling.
- Externalize config: `~/.f2f/settings.toml` + env/CLI override.
- Диагностические CLI без браузера (`f2f health|status|config`); pprof на
  loopback + опциональный Prometheus `/metrics`.

### Панель управления сервисами поверх Docker
- Вкладка «Сервисы»: каталог self-hosted-приложений (Nextcloud, Gitea,
  Vaultwarden, Immich, Grafana…), install/start/stop/upgrade/remove по клику.
- Install автоматом: `docker pull`, свободный overlay-порт, domain в
  `MyDomains`, firewall-порт, TLS от локального CA, регистрация
  OIDC-клиента, дефолтная policy «всем в camp'е».
- Каталог = YAML-шаблоны (builtin embed + `~/.f2f/services-catalog/`).
- Открыто: один сервис на camp vs multi-peer, persistence/handover при
  уходе пира, resource limits, upgrade/rollback.

### Рефакторинг на слоёную архитектуру (опционально)
- `engine.go` — god object (~3.3k строк). Возможное разбиение на `core`
  (типы/конфиг/identity/crypto), `net` (tunnel/udp/rendezvous/pair/quic),
  `app` (dns/intercept/egress/firewall/calls/drop). Делать вместе с тестами
  и structured logging.
