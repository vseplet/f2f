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
- **Exit-only intercepts** — резолв domain-спеков через exit-пира
  (`resolve` bus-хендлер в `services/tunnel`), per-target NAT по таблице
  маршрутов пира, пиннинг ответов в локальный DNS (`dns.SetPinned` +
  `/etc/resolver/<domain>`, mac-only). Открыто: коллизии приватных IP
  (синтетические адреса) и политика выпуска.

## Вытеснено отдельными доками

- **Аутентификация, SSO, f2f как OIDC-провайдер** — реализовано и
  задокументировано отдельно: [OIDC.md](OIDC.md) (провайдер, клиенты,
  passkey/WebAuthn) и [INVITE.md](INVITE.md) (identity и инвайты). Код:
  `services/oidc`.

## Планы

### Распределённая БД и приложения над ней
- Субстрат [DB.md](DB.md): SQLite-бэкенд, anti-entropy sync поверх шины,
  доставка по frontier'ам, snapshots/GC.
- [BLOCKS.md](BLOCKS.md): блок-модель (чат/доки/таски как один движок).
- Перевести messenger на `db`.

### Универсальное десктоп-приложение
- Один реактивный фронтенд на все платформы вместо jQuery + двух кодобаз
  (`source/mac/` embed и `source/desktop/` Wails).
- WebSocket/IPC push вместо HTTP-polling; браузерный fallback как SPA на
  loopback.
- Privilege elevation: app (user) поднимает engine (root) через системный
  диалог, общение по localhost (macOS — `osascript`, позже `SMAppService`).

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
