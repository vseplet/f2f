# f2f-mac — гид по коду

Этот документ — подробное описание того, **как устроен** mac-клиент
f2f. Если ты не Go-программист и хочешь сесть и разобраться в codebase
— читай отсюда.

README в этой же папке — про **что делает** и **как пользоваться**;
этот файл — про **где что лежит и почему так**.

## Оглавление

1. [Краткий Go-ликбез для тех кто не пишет на Go](#краткий-go-ликбез)
2. [Структура папки](#структура-папки)
3. [Точка входа: main.go](#точка-входа-maingo)
4. [Слои и сервисы](#слои-и-сервисы)
5. [Сценарии: куда идёт каждый пакет](#сценарии-куда-идёт-каждый-пакет)
6. [Карта горутин](#карта-горутин)
7. [Дизайнерские решения и почему так](#дизайнерские-решения)
19. [TODO: рефакторинг на слоёную архитектуру + QUIC](#todo-рефакторинг-на-слоёную-архитектуру--quic)
20. [TODO: миграция inter-node на QUIC](#todo-миграция-inter-node-коммуникации-на-quic)
21. [TODO: транспортное шифрование через AmneziaWG](#todo-транспортное-шифрование-через-amneziawg)
22. [TODO: универсальное десктоп-приложение](#todo-универсальное-десктоп-приложение)
23. [TODO: инженерные улучшения](#todo-инженерные-улучшения)
24. [TODO: система уведомлений](#todo-система-уведомлений)
25. [TODO: аутентификация, SSO и OIDC](#todo-аутентификация-sso-и-f2f-как-oidc-провайдер)
26. [TODO: панель управления сервисами поверх Docker](#todo-панель-управления-сервисами-поверх-docker)

---

## Краткий Go-ликбез

Если ты пишешь на JS/Python/Ruby — несколько штук, которые
встречаются в коде на каждом шагу:

**Пакеты.** Папка с `*.go`-файлами = пакет. Имя пакета указывается в
первой строке файла (`package engine`). Импортируется по пути от
корня модуля: `import "github.com/vseplet/f2f/source/helper/engine"`.
Внутри одного пакета все файлы видят друг друга без явных импортов.

**Видимость.** Имя с **большой буквы** (`Engine`, `Status`) — экспортируется
наружу пакета. Имя с маленькой (`engine`, `peerState`) — приватное,
видно только внутри пакета.

**Goroutines (`go func() {...}()`).** Лёгкий поток. Запустить — `go f()`. Они
выполняются параллельно. В этом коде их **много**: каждый цикл
обработки пакетов, каждый poll-loop — отдельная goroutine.

**Каналы (`chan T`).** Безопасный обмен между goroutines. `ch <- value`
— положить, `<-ch` — забрать. В этом коде каналы почти не используются
напрямую — мы вместо них применяем `context.Context` для отмены и
`sync.Mutex` для shared state.

**Context.** Объект, который умеет «отмениться». Передаётся в долгие
функции. Когда ты делаешь `cancel()` — все воркеры, получившие этот
context, узнают через `<-ctx.Done()` и должны корректно завершиться.

**Mutex (`sync.Mutex`).** Замок для shared-state. `e.mu.Lock()` /
`defer e.mu.Unlock()` — стандартный паттерн «не дать двум goroutines
одновременно читать-писать map».

**`atomic.Pointer[T]`.** Указатель, который можно атомарно `Store/Load`
без mutex'а. Используется для горячих read-path'ов (например, текущий
активный peer-адрес — читается из всех воркеров, пишется иногда).

**`defer`.** Откладывает вызов до выхода из функции. `defer cleanup()`
— гарантия что cleanup выполнится при любом return / panic. Часто
используется для разблокировки mutex'ов и закрытия ресурсов.

**Интерфейсы.** Утиная типизация на стероидах. Например,
`engine/dns` (теперь `services/dns/server.go`) принимает `Resolver`
интерфейс — кто угодно с методом `LookupHost(label)` подходит. В
текущей раскладке его имплементит `services/dns.Service`.

**`io.Reader`/`io.Writer`.** Универсальные интерфейсы потоков
байтов. Файл, сокет, HTTP-body — всё это `io.Reader`.

**Билд-теги (`//go:build darwin`).** Метка над файлом «компилировать
только под macOS». Тут весь код помечен — мы macOS-only.

---

## Структура папки

```
source/helper/
├── main.go                    # точка входа: создаёт Store, Engine, сервисы; orchestrate lifecycle
├── go.mod / go.sum            # зависимости модуля
├── README.md                  # пользовательская доку
├── ARCHITECTURE.md            # ← этот файл
│
├── platform/                  # OS-level примитивы (Darwin/Linux/Windows)
│   ├── firewall_*.go            # pfctl (darwin) / nft (linux) wrappers
│   ├── tun_*.go / iface_*.go    # CreateTUN, ifconfig, multicast/offload toggles
│   ├── route_*.go               # /sbin/route на mac, ip route на linux
│   ├── egress_*.go              # NAT install/remove + ip-forwarding sysctl
│   ├── dns_*.go                 # InstallZoneResolver (/etc/resolver/<zone>)
│   ├── cert_*.go                # TrustStoreAdd/Remove/Contains
│   ├── paths_*.go               # AppSupportDir и пр.
│   └── reveal_*.go              # «открыть в Finder»
│
├── config/                    # singletone-Store над $HOME/.f2f
│   └── config.go                # Camp/Peer/Domain/Firewall/TrustedPeer структуры
│                                # + Store{Snapshot/UpdateCamp/Load/Save} с in-memory кэшем
│
├── identity/                  # Ed25519 ключ peer'а + X25519 derive + fingerprint (GenerateInvite — заготовка, пока не подключена)
│
├── cli/                       # ЖИЗНЕННЫЙ ЦИКЛ CAMP'ОВ (create/list/use/join/rm)
│   ├── manager.go               # Manager над config.Store: provisioning, state.json, LoadForStart
│   ├── wizard.go                # интерактивный picker на charmbracelet/huh (SelectCamp)
│   └── cli.go                   # разбор `f2f camp …` подкоманд
│
├── mesh/                      # ФАБРИКА ПИРОВ — всё про связь узлов (L3 → L7), без app logic
│   ├── engine/                  # TRANSPORT SUBSTRATE (L3): оверлей, только сеть
│   │   ├── engine.go              # orchestrator: lifecycle, peers map, callbacks. Start(Camp,Identity); RosterEntry — нейтральный intake (~2000 LOC)
│   │   ├── peers_seed.go          # read-only seed peers из переданного config'а (hydrate/prune); движок каталог НЕ пишет
│   │   ├── helpers.go             # PubToV4Addr/V4Subnet + extractDst/packetSummary
│   │   ├── log.go                 # broadcast-логгер для UI
│   │   ├── awg/                   # AmneziaWG Device + conn.Bind + UAPI builder
│   │   ├── obfenv/                # ChaCha20-Poly1305 envelope, magic-headers H1..H8 из camp_id
│   │   ├── pair/                  # signed pair_req/pair_res handshake
│   │   ├── route/                 # Manager поверх platform.Route* — track + Cleanup
│   │   └── utun/                  # lifecycle одного utun-устройства (Open/Read/Write/Close)
│   ├── camp/                    # RENDEZVOUS/DISCOVERY: формирует меш (не app поверх него)
│   │   ├── camp.go                # Service: announce-клиент, ростер → eng.ApplyCampRoster, персист PeerCatalog
│   │   └── rendezvous/            # camp UDP announce (ростер в ответе; PeerInfo wire-формат)
│   ├── bus/                     # ТРАНСПОРТ (L7): QUIC overlay:2203 — типизир. обмен по pub
│   │   └── bus.go                 # Service: listener+dial, auto-mesh, tie-break, Request/Notify/Handle + OpenStream/HandleStream (raw bidi stream под PTY/файлы). Recovery: keepalive 5s / idle 20s, ping 15s, вотчер сразу выкидывает мёртвый конект из кэша (conn.Context().Done) — без зомби
│   └── gossip/                  # репликация NodeState (platform + peer-view) по шине
│       └── gossip.go              # Service: Announce/Peer/All/OnChange; generic для app, типизир. для fabric
│
├── services/                  # APPLICATION-LEVEL сервисы поверх engine
│   ├── dns/                     # DNS-сервер + MyDomains catalog + peer-poll + health
│   │   ├── dns.go                 # Service: lifecycle, LookupHost (Resolver impl), CRUD
│   │   └── server.go              # DNS протокол поверх miekg/dns
│   ├── drop/                    # BT-клиент + filesPollLoop + chown/prune
│   │   ├── drop.go                # Service: torrent client lifecycle, downloads.json
│   │   └── torrent/               # anacrolix wrapper (бывший helper/torrent)
│   ├── firewall/                # pf-anchor + user rules + peer-rules poll
│   │   ├── firewall.go            # Service: CRUD, builtin rules, PollPeers
│   │   └── anchor.go              # pf-anchor lifecycle + state-file recovery
│   ├── pki/                     # my CA (HTTPS termination) + peer CAs (poll/install)
│   │   ├── pki.go                 # Service: lifecycle, ListPeerCAs, Install/Remove
│   │   ├── ca_core.go             # Generate/Load/Save CA, IssueLeaf (бывший helper/ca)
│   │   └── ca_trust.go            # EnsureSystemTrust поверх platform
│   ├── calls/                   # SFU + group calls + remote-call poll
│   │   ├── calls.go               # Service: Create/Join/Leave/End, PollPeers
│   │   └── sfu/                   # Pion-based SFU (бывший helper/sfu)
│   ├── tunnel/                  # APPLICATION-уровень routing: intercepts + egress
│   │   ├── tunnel.go              # Service: AddIntercept/RemoveIntercept, RefreshDomainRoutes
│   │   └── egress.go              # NAT install + ip-forwarding (бывший engine/egress)
│   ├── messenger/              # пер-камповая SQLite (~/.f2f/<camp_id>/messenger.db)
│   │   └── messenger.go          # Store: messages/channels, modernc sqlite (no cgo)
│   ├── notify/                 # хаб уведомлений (in-memory ring + SSE), слушает шину
│   │   └── notify.go            # Service: Push/Recent/Subscribe, FromBus
│   ├── shell/                  # remote-terminal (mosh-подобный PTY по шине)
│   │   └── shell.go             # Service: HandleStream("shell.open") — PTY + detached-сессии (session_id) + ring-buffer reattach + kill; login/drop-to-user
│   └── vnc/                    # remote-desktop (тонкий TCP-прокси к VNC-серверу хоста)
│       └── vnc.go               # Service: HandleStream("vnc.open") — bus-стрим ⟷ localhost:5900; vnc.status (dial-тест)
│
└── ui/web/                    # HTTP UI + reverse-proxy
    ├── server.go                # роутер, statusView мерджит engine.Status + service-данные
    ├── shell.go                 # /api/shell/peers (discovery по шине) + /api/shell/ws (мост браузер↔bus-стрим к PTY)
    ├── vnc.go                   # /api/vnc/peers + /api/vnc/ws (мост браузер↔bus-стрим к VNC-серверу пира)
    └── assets/                  # SPA (embed'ятся; vendor/xterm — терминал; noVNC завендорен)
```

**Тиры.** `mesh/` — **фабрика пиров**: всё про связь узлов, без app logic.
Внутри по уровням: `engine` (L3 — оверлей: utun/UDP/AWG/pair/hole-punch),
`camp` (rendezvous/discovery — кормит движок ростером, без неё нет пиров),
`bus` (L7 — типизированный QUIC-обмен по pub), `gossip` (репликация
fabric-стейта NodeState). `services/` — пользовательские сервисы поверх
фабрики: каждый держит своё состояние, пишет в `config.Store`, читает
живых peer'ов через `engine.*` и обменивается с пирами через `mesh/bus`.
`cli/` — жизненный цикл camp'ов (create/list/use/join/rm + picker);
отдаёт движку готовые `config.Camp` + `identity`. main.go соединяет их.

**Зачем такое разделение.** Фабрика — это "как узлы связаны", её
поведение не зависит от того что мы публикуем (домены / файлы / звонки).
Сервис — это "что ты можешь сделать поверх фабрики". Можно убрать сервис
без поломки фабрики, добавить новый без её правок. `camp` лежит в `mesh/`,
а не в `services/`, именно потому что это формирование меша, а не
приложение поверх него. `identity/config/platform` — фундамент под всеми
тирами, `cli` — оркестратор camp-стейта над `config.Store`.

---

## Точка входа: main.go

Разбирает CLI-аргументы, выбирает режим, создаёт `Engine`, поднимает
сервисы + UI и решает какой camp поднять.

**Логика:**

```go
func main() {
    // args[0] == "camp" → cli.RunCamp(store, args[1:]); exit
    //                     (управление camp'ами: ls/new/join/use/rm)
    // args[0] == "up"   → autostart=true (без интерактивного picker'а)
    // иначе             → run(bind, console, autostart)
}
```

`run(...)` создаёт `engine.New()`, конструирует все сервисы, вешает хуки
(`eng.OnStarted` → старт сервисов + `srv.BindTunnel`, `eng.OnStopped` →
обратный teardown), и **выбирает camp до старта воркеров/UI**:

```go
mgr := cli.NewManager(store)
interactive := !autostart && cli.Interactive()  // есть TTY и не `up`
camp, idt, _ := mgr.SelectCamp(interactive)      // picker | последний camp
// … затем поднимаются воркеры + UI, и:
eng.Start(engine.Config{Camp: camp, Identity: idt, Listen: ":9000"})
```

Picker (`huh`) запускается **первым**, на чистом терминале — иначе баннер
UI и логи воркеров рвут его перерисовку. Движок получает уже готовые
`config.Camp` + `identity`; ничего про создание/выбор camp'а не знает.

**Ключевое:**

```go
const defaultBind = "127.0.0.1:2202"
```

Это дефолт для UI. Поменять в одном месте — сменишь и для пользователя.

```go
eng.OnStarted = func(localIP string) {
    srv.BindTunnel(localIP)
}
```

«Хуки» — функции, которые engine зовёт сам в нужные моменты. Так
engine не знает про web (нет обратной зависимости), но позволяет web
реагировать на свой lifecycle.

```go
eng.SetTunnelHTTPPort(port)
```

`port` извлекается из `--bind`-флага. Engine при опросе чужих
`/api/domains` будет ходить именно на этот порт (мы предполагаем что
у всех peer-ов f2f-mac слушает на одном и том же порту — обычно 2202).

---

## Слои и сервисы

Архитектурно делится на **четыре слоя**, снизу вверх:

```
┌─────────────────────────────────────────────────────────┐
│  ui/web              HTTP UI + reverse-proxy + statusView│
├─────────────────────────────────────────────────────────┤
│  services/  ←  application logic, держат своё состояние │
│   dns  drop  firewall  pki  calls  tunnel               │
├─────────────────────────────────────────────────────────┤
│  engine/             transport substrate                 │
│   utun + UDP + AWG + pair + punch + camp announce       │
│   sub: awg/ obfenv/ pair/ rendezvous/ route/ utun/      │
├─────────────────────────────────────────────────────────┤
│  config/  identity/  platform/                          │
│   Store      Ed25519/X25519     pfctl/nft/...           │
└─────────────────────────────────────────────────────────┘
```

### engine/ — транспортный субстрат

Только то что трогает сеть и железо. **Не** знает что мы публикуем
(домены, файлы, звонки) — для него все peer'ы одинаковы. ~2000 LOC.

| Что | Зачем |
|---|---|
| `engine.go` | orchestrator: lifecycle (`Start(Camp,Identity)`/Stop), peers map (`peerState`), OnStarted/OnStopped callbacks. `RosterEntry` — нейтральный intake-тип (`ApplyCampRoster`), движок не знает wire-формат camp'а |
| `peers_seed.go` | read-only seed `e.peers` из переданного `config.Camp.PeerCatalog` (hydrate) + чистка self (prune). Движок каталог на диск **не пишет** — это делает `mesh/camp` |
| `helpers.go` | `PubToV4Addr` (overlay 100.64.X.Y из pub), `extractDst`/`packetSummary` (IP-парсер для логов) |
| `log.go` | log-tap для UI broadcast |
| `awg/` | AmneziaWG `Device` + `conn.Bind` поверх общего UDP-сокета + UAPI builder с обфускацией |
| `obfenv/` | ChaCha20-Poly1305 envelope для pair_req/res, magic-headers H1..H8 deterministic из camp_id |
| `pair/` | `BuildReq/BuildRes/ParseReq/ParseRes` — Ed25519-подписанный handshake с canonical bytes |
| `route/` | `Manager` поверх `platform.RouteAdd/Remove`, track + Cleanup при Stop |
| `utun/` | lifecycle одного utun-интерфейса: Open / Read / Write / Close через `wgtun.Device` |

**`mesh/camp` — rendezvous/discovery** (формирует меш, не app поверх него):

| Что | Зачем |
|---|---|
| `camp.go` | announce-клиент на UDP-сокете движка; ростер из ответа → `eng.ApplyCampRoster` (маппит `rendezvous.PeerInfo` → `engine.RosterEntry`); персистит `PeerCatalog` в `config.Store` |
| `rendezvous/` | wire-протокол camp-сервера: UDP announce + `PeerInfo`. Импортируется и клиентом, и самим camp-сервером (`source/camp`) |

**Поверхность для сервисов** (engine.go экспортит):

```go
Routes() *route.Manager                  // borrow route primitive
UtunName() string                         // живое имя utun
HasPeerName(name string) bool             // peer есть в catalog?
TunnelHTTPPort() string                   // порт для peer-poll over utun
OnlinePeersForCAPoll() []OnlinePeerHTTPInfo  // снимок paired peers (Pub/Name/Host)
SyncAWG()                                 // триггер awgSyncPeers
SetAWGAllowedCIDRsHook(fn)                // services/tunnel инжектит intercept CIDRs
```

### services/ — пользовательские сервисы

Каждый сервис:
- держит **своё** состояние (in-memory + персист через `config.Store.UpdateCamp(fn)`),
- получает `*config.Store` и `*engine.Engine` в конструкторе,
- управляется из main.go (Start на `eng.OnStarted`, Stop на `eng.OnStopped`),
- спавнит свои poll-loops под root `ctx` процесса.

| Сервис | Что внутри |
|---|---|
| **dns** | DNS-сервер `127.0.0.1:0` для `<camp>.f2f` зоны, имплементит `Resolver` через `LookupHost`. Catalog: `MyDomains` (наши) + per-peer `peerDoms` (polled из `/api/domains` каждые 10с). TCP health-check каждые 8с (стучится на 127.0.0.1:port для опубликованных портов). |
| **drop** | Anacrolix BT-клиент на overlay v4 + fallback на ephemeral port если 6881 занят. `rescanSharedDir` re-seed'ит при старте. `pruneLoop` снимает с раздачи удалённые файлы. `filesPollLoop` опрашивает peer'ов раз в минуту. Persist в `downloads.json`. |
| **firewall** | pf-anchor lifecycle (Open/Apply/Close) с recovery state файлом. Built-in порты (`2202/tcp`, `2203/udp` шина, `80`/`443`, `6881`) + user CRUD из UI. Peer-firewall poll каждые 30с — складывает в `peerPorts[pub]`, статус показывается в UI. |
| **pki** | My CA: load-or-generate per camp, install в trust store (один раз — пользователь даёт пароль). Peer CAs: каждые 30с poll `/api/ca-cert`, новые сохраняются на диск. Install в keychain — только по клику в UI. |
| **calls** | Pion SFU + group calls. Hosting peer запускает SFU `sfu.New(...)`, остальные join. Signal через `/api/call/signal` (HTTP через utun). Remote-call poll каждые 3с. |
| **tunnel** | Application-уровень routing. **Intercepts**: AddIntercept(spec, peer) — resolve spec в prefixes, добавить routes на utun, persist в `c.Intercepts`. `RefreshDomainRoutes` каждые 60с re-resolve'ит DNS-spec'и. **Egress**: автоматически открывает NAT для overlay subnet на default route iface (`platform.InstallNAT` + ip-forwarding). |
| **bus** | **QUIC data bus** на `overlay-IP:2203` — единый пир-к-пир транспорт (заменяет HTTP-over-tunnel). Авто-меш: пинг всех достижимых пиров раз в 30с, tie-break (младший pub дозванивается → одно соединение на пару), кэш коннектов (вход/исход переиспользуются — стримы двунаправленные). API: `Request`/`Notify`/`Handle(type, fn)`. TLS — self-signed + skip-verify: **аутентичность/шифрование уже даёт оверлей** (overlay-IP ≡ pub, WireGuard), идентичность пира = overlay-IP входящего коннекта. `Events`-хук отдаёт пинги в notify. |
| **messenger** | Пер-камповая SQLite `~/.f2f/<camp_id>/messenger.db` (драйвер `modernc.org/sqlite`, без cgo), ленивое открытие + кэш хендлов. Таблицы `messages` (dm/channel) и `channels`. Локальное хранилище чата; обмен — поверх шины. |
| **notify** | Хаб уведомлений: in-memory кольцо (200) + fan-out в UI по SSE. Источники: шина (`Handle("notify")` — пир прислал) и bus-события (пинги). Отдаёт `/api/notifications` + `/api/notifications/stream`. Транспорт-агностичен (локальные сервисы зовут `Push` напрямую). |
| **shell** | Remote-terminal по шине. `HandleStream("shell.open")` — спавнит PTY (по умолчанию системный `login`; fallback-шелл дропается до `SUDO_USER`), держит **detached-сессии** по `session_id` с ring-буфером (reattach перерисовывает экран — переживает сон/reload). Протокол на стриме: сервер→клиент сырой вывод PTY, клиент→сервер фреймы `d`/`r`/`k` (data/resize/kill). `shell.status` (Request) — discovery (UI-список **sticky**: пир держится ~35с после последнего ответа, чтобы флап шины не выкидывал его). Доступ гейтит `config.Camp.Shell` (enabled + allowlist пабов). Web-слой мостит браузерный xterm.js (WS) в этот стрим. |
| **vnc** | Remote-desktop по шине — **тонкий TCP-прокси**, не свой захват. `HandleStream("vnc.open")` — переливает RFB между bus-стримом и локальным VNC-сервером ОС (`127.0.0.1:5900`: macOS Screen Sharing / x11vnc / wayvnc). Захват/кодирование/аутентификацию делает сам сервер. `vnc.status` (Request) — dial-тест `:5900` (показываем в списке только машины с живым десктопом). Доступ гейтит `config.Camp.Vnc`. Web-слой мостит браузерный noVNC (WS) в этот стрим; качество (`qualityLevel`/`compressionLevel`) и auth (VNC-пароль / Apple ARD) — на стороне noVNC (завендорен под `/vendor/novnc`). |

### config/ + identity/ + platform/

| Пакет | Что |
|---|---|
| `config.Store` | thread-safe wrapper над `~/.f2f/<camp_id>/config.json`. `Snapshot(id)` отдаёт deep-copy, `UpdateCamp(id, fn)` атомарно читает→мутирует→пишет. In-memory cache синхронизирован с диском. Источник правды для всех сервисов и для `cli`. |
| `identity` | derive Ed25519 keypair из persistent seed, fingerprint = первые 8 байт SHA-256(pub). `X25519()` через HKDF — для AWG transport keys. `CampLabel(id)` — короткий display-label из camp_id. `DirFor(id)` — путь к keypair'у. `GenerateInvite` — заготовка под invite-токены (механизм ещё не реализован: не вызывается, в CLI минтинга/парсинга нет). |
| `cli` | жизненный цикл camp'ов над `config.Store`: `Manager` (create/list/use/join-по-camp_id/rm, `state.json`, `LoadForStart`), `SelectCamp` (picker `huh` или последний camp), разбор `f2f camp …`. Единственный, кто генерит identity и пишет camp-config при создании. |
| `platform` | OS-specific примитивы. Build-tag'ы `_darwin.go`/`_linux.go`/`_windows.go`. Pfctl/nftables, ifconfig/ip-link, /sbin/route/ip-route, NAT install, trust-store add/remove, /etc/resolver write, AppSupportDir, "reveal in finder". |

### Кто кого знает (импорты)

```
ui/web         → mesh, services/*, cli        (нужен всё)
cli            → config, identity, mesh/engine, huh
services/*     → mesh, config, identity, platform   (но НЕ друг друга)
mesh/camp      → mesh/engine, config           (драйвер субстрата: ростер → engine)
mesh/gossip    → mesh/bus                     (транспорт)
mesh/bus       → (свой Resolver-интерфейс)    (engine-адаптер инъектит main)
mesh/engine    → config, identity, platform   (НЕ импортирует camp/rendezvous — intake через RosterEntry)
config         → platform                     (paths/chown)
identity       → -                            (stdlib only — кросс-каттинг примитив)
platform       → -                            (stdlib + OS calls)
```

Стрелка между движком и camp **односторонняя**: `mesh/camp → mesh/engine`
(camp заносит ростер). Движок про wire-формат camp-сервера не знает —
`ApplyCampRoster` принимает нейтральный `engine.RosterEntry`, а camp сам
перекладывает в него `rendezvous.PeerInfo`.

Сервисы **не импортируют друг друга**. Если двум нужны общие данные
— через `config.Store` (catalog + per-peer secondary data) или через
engine getter (live peer state). Исключение по проводке: `bus` и `notify`
связаны в main.go через колбэки/хендлеры (`busSvc.Handle("notify", …)`,
`busSvc.Events = …`), а не прямыми импортами.

---

## Миграция пир-к-пир HTTP → QUIC-шина

Сейчас пиры общаются друг с другом по **HTTP поверх туннеля**: каждый
держит `tunnelSrv` (HTTP-листенер на `overlay-IP:2202`, см. `BindTunnel`),
и соседи делают к нему запросы по утану. Это работает, но плодит
ad-hoc эндпоинты, требует открытого 2202 на оверлее и не даёт единой
типизированной модели. Цель — перевести всё это на **шину** (`services/bus`,
QUIC/2203) и **закрыть** лишний порт + листенер.

Почему это безопасно без своей аутентификации: оверлей (WireGuard) уже
шифрует и подтверждает источник (overlay-IP детерминирован из pub,
чужой ключ → дроп). Поэтому и текущий HTTP — без TLS, и шина — с
self-signed + skip-verify; идентичность пира = overlay-IP коннекта.

### Что переезжает (всё пир-фейсинг на `tunnelSrv`)

| HTTP-эндпоинт (overlay:2202) | Что делает | Тип на шине | Статус |
|---|---|---|---|
| `POST /api/signal/inbox` | WebRTC p2p сигналинг (offer/answer/candidate) | `signal` | TODO |
| `POST /api/call/signal` | SFU-сигналинг (group calls) | `call-signal` | TODO |
| `GET /api/call/state` | опрос состояния звонка пира | `call-state` | TODO |
| `POST /api/call/join` / `leave` | join/leave удалённого SFU | `call-join` / `call-leave` | TODO |
| `GET /api/domains` | опубликованные домены пира (dns-poll каждые 10с) | `domains` | TODO |
| `GET /api/ca-cert` | CA-серт пира (pki-poll каждые 30с) | `ca-cert` | TODO |
| `GET /api/files` | список расшаренных файлов (drop) | `files` | TODO |
| `GET /api/firewall` | список открытых портов пира | `firewall` | TODO |
| — | сообщения/каналы (messenger) | `message` | TODO |
| — | уведомления | `notify` | ✅ есть |

Шаблон миграции (на каждый эндпоинт): поднять `Push`/poll-сторону на
`busSvc.Notify/Request`, а приёмную — на `busSvc.Handle(type, fn)`;
HTTP-роут на `tunnelSrv` удалить. Опросы (domains/ca-cert/files/firewall)
можно переделать из «каждый сам опрашивает соседей» в «владелец
`Notify`-ит изменения по шине» — меньше трафика, мгновенные апдейты.

### Что закрывается после полной миграции

- **`tunnelSrv` целиком** (`BindTunnel`/`UnbindTunnel`) — пир-фейсинг
  HTTP на оверлее больше не нужен.
- **`2202/tcp` из `firewall.BuiltinRules`** — порт на оверлее закрывается
  (loopback-UI на `127.0.0.1:2202` остаётся, он не под pf-правилом оверлея).

### Что остаётся (не транспорт-шина)

- **`80`/`443` (proxy)** — отдаёт пользовательские `.f2f`-домены (gitea и
  т.п.) реальным браузерам; это не «данные f2f», а проксирование сервисов.
- **`6881` (BitTorrent)** — фактическая передача файлов в drop (свой
  протокол). По шине поедет только *листинг* файлов; перенос самой
  передачи на QUIC — отдельная большая история.
- **`2203/udp` (bus)** и WG-handshake на физическом UDP-порту.

Итог: целевой пир-фейсинг surface на оверлее = `2203/udp` (шина) +
`80/443` (proxy) + `6881` (BT). HTTP-листенер `tunnelSrv` уходит.

---

## Сценарии: куда идёт каждый пакет

### A. Я открыл `gitlab.beer.f2f:3000` в Chrome на своей машине

(Здесь я опубликовал gitlab, я же его читаю — крайний случай, петля).

1. Chrome зовёт системный DNS на `gitlab.beer.f2f`.
2. macOS видит `/etc/resolver/beer.f2f` — направляет запрос на `127.0.0.1:5354`.
3. Наш `internal/dns` Server получает запрос, делает `engine.PeerDomains()`.
4. PeerDomains возвращает в том числе `"127.0.0.1" → [{Name:"gitlab"}]` (мы наши
   собственные домены маппим в loopback, см. ниже почему).
5. DNS возвращает `gitlab.beer.f2f → 127.0.0.1` Chrome'у.
6. Chrome открывает TCP к `127.0.0.1:3000` — попадает прямо на наш Python
   HTTP-сервер. Утун в этой петле не участвует.

Почему мы не маппим свои домены в свой tunnel_ip (`100.64.0.2`)? Потому что
ядро macOS маршрутизирует пакеты на `100.64.0.2` **через утун** (`route -n
get 100.64.0.2` показывает `interface: utun7`). А engine при чтении из утуна
видит свой собственный `dst=100.64.0.2`, в peers-map его не находит (это же
**мы**), интерсепт тоже не матчится → drop. Локальный сервер недостижим.
Loopback `127.0.0.1` обходит эту проблему.

### B. Friend открывает `gitlab.beer.f2f:3000`

1. Его macOS делает DNS — у него `/etc/resolver/beer.f2f` тоже стоит,
   запрос идёт в его локальный `127.0.0.1:5354`.
2. **Его** engine.PeerDomains возвращает: для tunnel_ip `100.64.0.2`
   есть домен `gitlab` (он узнал об этом из polling-цикла моего
   `/api/domains`).
3. DNS отвечает `gitlab.beer.f2f → 100.64.0.2`.
4. Его Chrome открывает TCP к `100.64.0.2:3000`.
5. Его kernel роутит на утун (100.64.0.0/10 → utun).
6. Его `tunToPeerLoop` читает IP-пакет.
7. `routeFor(pkt)`: dst = `100.64.0.2` → находит в его `peers` map моего
   `peerState`, возвращает `myUDPAddr` (`171.97.230.138:7851`).
8. Пакет уходит UDP'ом на 171.97.230.138:7851.
9. Через интернет (либо hairpin NAT если на одной сети) долетает до меня.
10. Мой kernel доставляет UDP в `:9000` (мой engine).
11. Мой `peerToTunLoop` читает, проверяет источник — это friend, обновляет
    LastSeen.
12. Пакет — IPv4 (n>=20), записываем в утун.
13. **Firewall**: pf-anchor на утане проверяет `dst=100.64.0.2:3000`. Port
    3000 НЕ в built-in списке и НЕ в user-list'е → packet **dropped**.
    Friend получит TCP timeout. Чтобы это работало — нужно либо явно
    открыть `3000/tcp` через Tunnel-таб UI, либо опубликовать сервис
    через DNS (`http://gitlab.beer.f2f/`) и ходить через reverse-proxy на
    :80/:443, который уже built-in.

### C. Я нажал `intercepts` → `myip.com via Friend`

1. UI POST'ит на `/api/intercepts` `{spec:"myip.com", peer:"Friend"}`.
2. `engine.AddIntercept`:
   - резолвит `myip.com` → `1.2.3.4` и `1.2.3.5` (примеры).
   - ставит host-маршруты `1.2.3.4/32`, `1.2.3.5/32` через утун.
   - сохраняет в `e.intercepts["i7"] = {Spec, Peer:"Friend", Prefixes:[...]}`.
3. Я открываю `https://myip.com` в браузере.
4. ОС резолвит `myip.com` своим резолвером (не нашим — это нормальный
   домен, не `.f2f`).
5. Получает `1.2.3.4`.
6. Открывает TCP к `1.2.3.4:443`.
7. Kernel смотрит маршрут: `1.2.3.4/32` → utun.
8. Мой engine читает из утуна.
9. `routeFor`: dst=1.2.3.4 — не tunnel_ip → ищем по intercepts →
   найден `Friend` → берём его `UDPAddr`.
10. Пакет UDP'ом к Friend'у.
11. У него engine получает, пишет в утун.
12. **Firewall**: pf-anchor у Friend'а смотрит на dst — это `1.2.3.4`,
    НЕ его tunnel_ip. Anchor его не трогает (implicit pass).
13. У него kernel роутит: dst=1.2.3.4 — это публичный IP, не локальный.
    Идёт через egress.
14. **pf NAT** на его стороне меняет src с `1.2.3.4` (нет, src был **моим**
    `100.64.0.2`) на его публичный IP.
15. Пакет уходит в интернет с его IP.
16. `myip.com` отвечает «твой IP такой-то» — а это IP Friend'а, не мой.
17. Обратный путь обратно тем же макаром.

### D. Friend открывает `https://gitlab.beer.f2f/` (БЕЗ порта)

С реверс-прокси + Local CA — то самое «как gmail.com выглядит для
пользователя, без портов и предупреждений».

1. Friend'овский Chrome зовёт DNS на `gitlab.beer.f2f`. Системный
   резолвер видит `/etc/resolver/beer.f2f` → шлёт в `127.0.0.1:5354`
   на его машине.
2. Его DNS отвечает `100.64.0.2` (мой tunnel_ip).
3. Chrome открывает TCP на `100.64.0.2:443`. Пакет уходит в его утун.
4. UDP через туннель доходит до меня, kernel записывает пакет в утун.
   **Firewall**: `443/tcp` — built-in allow, пропускается. Доставляется
   на мой `<tunnel_ip>:443` — там слушает наш HTTPS proxy.
5. **TLS-handshake**: ClientHello прилетает с SNI=`gitlab.beer.f2f`.
   `tls.Config.GetCertificate` зовёт `engine.ca.IssueLeaf("gitlab.beer.f2f")`
   — генерируется leaf-cert (если ещё нет в кэше), подписанный моим
   local-CA. Engine отдаёт chain `[leaf, CA]`.
6. **Браузер Friend'а валидирует**: leaf подписан моим CA. Мой CA
   уже в его системном keychain'е (он его pulled через
   `peerCAPollLoop` и установил через `security add-trusted-cert`).
   Цепочка валидна → **зелёный замок**.
7. После handshake'а engine форвардит дешифрованный HTTP-запрос на
   `127.0.0.1:3000` (порт gitlab'а из `MyDomains["gitlab"]`).
8. Gitlab отвечает, engine упаковывает в TLS, шлёт обратно Friend'у.

Без шага 6 (peer-CA exchange + auto-trust) — браузер бы ругался «not
secure», пользователь жал бы «proceed anyway». С шагом — всё прозрачно.

### E. Friend качает мой файл через drop-таб

1. Я drag-and-drop'нул `video.mp4` в UI «my shared files».
2. UI: `POST /api/files/mine/upload` (multipart с файлом).
3. Engine копирует в `~/Library/Application Support/f2f/shared/video.mp4`.
4. `torrent.AddSeed(path)`:
   - Считает SHA-1 каждого piece (1 MiB).
   - Создаёт `metainfo.Info` → `bencode.Marshal` → получаем `info_hash`.
   - Зовёт `atorrent.AddTorrentOpt(...)` со storage на parent-dir.
   - Anacrolix начинает seed'ить на `<my_tunnel_ip>:6881`.
5. `/api/files` теперь возвращает запись с этим magnet'ом.

Через 0-60 секунд:

6. Engine у Friend'а тикает `filesPollLoop` → `GET http://<my_tunnel_ip>:2202/api/files`.
7. Получает `[{name:"video.mp4", info_hash:"abc...", magnet:"magnet:?xt=urn:btih:abc..."}]`.
8. Кладёт в `peer.Files[]`, что летит в его UI через `/api/status`.
9. Friend в UI видит запись в «camp library», нажимает download.
10. UI: `POST /api/files/download` с `{magnet, peers:["<my_tunnel_ip>:6881"]}`.
11. `torrent.AddDownload`:
    - `client.AddMagnet(magnet)` — добавляем торрент.
    - `t.AddPeers([...])` — даём анакроликсу мой адрес.
    - `<-t.GotInfo()` — ждём metainfo (приходит из BT-handshake'а через peer wire).
    - `t.DownloadAll()` — начинаем тянуть piece'ы.
12. Anacrolix открывает TCP к `<my_tunnel_ip>:6881`. Пакеты идут через **утун-туннель** (это же `100.64.0.X`).
13. Мой engine на `:6881` принимает соединение, отдаёт piece'ы.
14. Friend сохраняет в `~/Downloads/f2f-drops/video.mp4`.
15. **Auto-seed**: завершив download, Friend сам становится seed'ом — третий peer уже может качать у него (или у меня — параллельно). Multi-source активирован.

### F. Hole-punching

1. Мой engine на старте делает `AnnounceOnce` к camp — UDP-пакет на
   `f2f-camp.fly.dev:3478` от моего `:9000`.
2. Camp видит мой src (`Vpub:VportToCamp`), отвечает JSON-ом с моим
   tunnel_ip.
3. У camp в peer-list я появляюсь с public_ip `Vpub:VportToCamp`.
4. Friend периодически polls camp HTTP — узнаёт мой публик-эндпоинт.
5. Его `applyPeerList` добавляет меня в его `e.peers` с моим UDPAddr.
6. Его `holePunchLoop` начинает слать мне 1-байтовые UDP на `Vpub:VportToCamp`.
7. Симметрично — я ему. Оба NAT-а открывают мэппинги для друг друга.
8. Punch-пакеты доходят — `peerToTunLoop` каждого обновляет
   `LastSeenMs` соответствующего peer'а.
9. UI показывает зелёный дот.

Если в какой-то момент пакеты прекратят приходить (NAT-мэппинг
закрылся) — `LastSeenMs` устареет, hole-punch loop переключается с
keepalive-каденса (25с) обратно в burst (1Hz), пытается восстановить.

---

## Карта горутин

После рефакторинга горутины **распределены** между engine и сервисами.
Engine спавнит транспортные loops, сервисы — свои poll/refresh loops.

### Engine (через `e.workers` WaitGroup, выход на `ctx.Done()`)

| Горутина | Интервал | Назначение |
|---|---|---|
| `tunToPeerLoop` | непрерывно | (только static --peer mode; в camp mode AWG Device владеет utun) |
| `peerToTunLoop` | непрерывно | UDP читает, диспетчит: camp announce reply → handlePacket; envelope-magic → pair / AWG Bind; всё остальное → utun.Write |
| `holePunchLoop` | 1с burst / 25с keepalive | pair_req ко всем peer'ам, NAT-keepalive + identity attestation |
| `announce.Run` | 20с | UDP announce к camp серверу |
| `poller.Run` | 20с | HTTP poll peer list от camp |

### Сервисы (под root `ctx` из main.go, spawn в горутинах main)

| Сервис | Горутина | Интервал | Что делает |
|---|---|---|---|
| **dns** | `PollPeers` | 10с (5с warmup) | GET `/api/domains` у online peers → peerDoms + store catalog |
| **dns** | `HealthCheck` | 8с | TCP-dial своих portов → стампим health/checkedAt |
| **firewall** | `PollPeers` | 30с (9с warmup) | GET `/api/firewall` у peers → peerPorts + catalog |
| **pki** | `PollPeers` | 30с (5с warmup) | GET `/api/ca-cert` → discover (запись PEM на диск, install только по UI-клику) |
| **drop** | `PollPeers` | 60с (7с warmup) | GET `/api/files` у peers → peerFiles |
| **drop** | `rescanSharedDir`, `restoreDownloads` | разовое | seed уже лежащих файлов; resume сохранённых download'ов |
| **drop** | `chownLoop` | 10с | chown shared/downloads-каталогов на SUDO_USER |
| **drop** | `pruneLoop` | 30с | прибрать seed'ы файлов которые удалены из FS; re-feed stalled |
| **calls** | `PollPeers` | 3с | GET `/api/call/state` у peers → AllCalls |
| **tunnel** | `RefreshDomainRoutes` | 60с | re-resolve DNS-spec intercept'ов и обновить routes |

### Pion / anacrolix / AWG внутренние

- **AmneziaWG Device** (camp mode): ~25-30 worker'ов сразу при `Device.Up()` — encryption workers (8) × decryption workers (8) × handshake workers (8) + TUN reader / event worker / receive worker.
- **anacrolix BT client**: ~30-50 даже без активных download/seed — peer-conn handlers, pieces, tracker stubs (DHT отключён, но scaffolding есть).
- **Pion WebRTC**: каждый `PeerConnection` порождает ~20-30 при handshake (ICE agent, DTLS, SCTP, SRTP, RTP read/write per track). До первого звонка их **нет**.

### Сколько в реальности

Сразу после старта **без звонков** — около **150 горутин**:

| Источник | ~Кол-во |
|---|---|
| AWG Device workers (encryption/decryption/handshake × 8 + сервисные) | ~30 |
| Anacrolix BT client (даже idle) | ~40-50 |
| Engine workers (peerToTun, punchLoop, announce, poller) | ~5 |
| Service polls (dns × 2, firewall, pki, drop, calls, tunnel) | ~7 |
| HTTP servers (UI + tunnel + 2 proxies) + SSE accept | ~10 |
| Pion WebRTC media engine init, MDNS stub, NetGather | ~10-15 |
| Go runtime (GC, finalizer, netpoll, traceback) | ~5-10 |
| **Итого без звонков** | **~120-150** |

Под live-звонок из 2-3 участников добавляется ~50-100 (1 Pion
PeerConnection × N peers + SFU forwarding). Не баг — нормально для
стека с AWG + Anacrolix + Pion на борту.

### SFU (динамические)

| Горутина | Кол-во | Назначение |
|----------|--------|------------|
| `handleTrack` (RTP forward) | 1 на трек | `remote.Read` → `local.Write` |
| `forwardRTCP` | 1 на sender×subscriber | PLI/FIR подписчик → издатель |
| PLI burst | 1 на трек (временная, 10с) | 5×PLI для keyframe |
| renegotiate timer | 1 на участника (временная) | 200ms batching |

**+ горутины от Pion WebRTC** (ICE agent, DTLS, SCTP, SRTP per PC) —
каждый `PeerConnection` порождает ~20-30 внутренних горутин Pion.

---

## Дизайнерские решения

Несколько важных «почему так» — чтобы при чтении кода не недоумевать.

### Один UDP-сокет на туннель И announce

Можно было бы держать отдельный сокет для announce-протокола. Но
тогда NAT-мэппинг для announce-сокета (к camp-у) и для туннеля (к
peer-ам) был бы **разный**. Camp бы видел один external port, peer-ы
бы получали попытки от другого. С symmetric NAT это бы убило
hole-punching.

Решение: shared socket. `peerToTunLoop` — единственный read-loop. Он
смотрит на src пакета и решает — это camp (`sameUDPAddr(campAddr,
from)`) или peer.

### `e.mu sync.Mutex` для `peers` и `intercepts`

Из всех воркеров эти карты читают и пишут. Простой `map` в Go
**небезопасен** для concurrent access — поэтому мьютекс. `atomic.Pointer`
не подходит потому что нужны частичные изменения (Add/Remove одного peer-а).

### Атомарные счётчики

`txBytes atomic.Uint64` etc — обновляются на каждый пакет. Под mutex'ом
это была бы серьёзная contention. Atomic - lock-free.

### Hooks вместо обратной зависимости

Engine не импортирует web (нет circular dep). Но web нужно знать когда
engine стартанул чтоб поднять tunnel-listener. Решение: engine выставляет
два nil-функции `OnStarted/OnStopped`, main.go их подписывает.

### `internaldns` псевдоним

```go
internaldns "github.com/vseplet/f2f/source/mac/internal/dns"
```

— чтобы не конфликтовать с пакетом `dns` из библиотеки `miekg/dns`.

### Embed UI в бинарь

Один бинарь, никаких внешних файлов, ничего не «потеряется» при
deploy'е. Цена: UI меняешь → пересобираешь бинарь. Для dev-flow норм:
`go run` всё равно компилирует каждый раз.

### `<camp_id>.f2f` как TLD

`.f2f` — не зарегистрированный TLD. Никогда не будет коллидировать
с настоящими доменами. `<camp_id>.f2f` даёт изоляцию: разные кэмпы =
разные TLD-подзоны = разные `/etc/resolver/<file>`.

### `127.0.0.1:5354` для DNS

Не `:53` (привилегированный, потенциальный конфликт), не `:5353`
(занимает Bonjour/mDNS — у нас был баг про это, см. git history).

### Детерминированный overlay-IP из pub

Раньше camp раздавал октеты из пула и каждый restart peer-а мог дать
новый адрес. Это ломало intercept-binding (`gmail.com via Vsevolod`
где Vsevolod был `100.64.0.5`, рестарт — он стал `100.64.0.8`). Теперь
адрес выводится детерминированно из Ed25519 pub (`sha256(pub) →
100.64.X.Y`, см. `engine.PubToV4Addr`): один и тот же ключ всегда даёт
один и тот же tunnel_ip, без состояния на camp-е и без внешней БД.

### Polling вместо push для domains

Альтернатива была — каждый peer пушит свой список доменов на остальных
при изменении. Но это требует знать кто online, обрабатывать ошибки
доставки, заводить retry-логику. Polling проще: каждые 10 секунд —
GET у каждого online peer-а, либо ответ есть, либо нет. Idempotent.
Для нашего масштаба (3-10 peer-ов в camp-е) — overhead копеечный.

### Name-Constrained CA на зону, не на пер-домен

CA генерируется один раз для camp-а с `permittedDNSDomains:
[".<camp_id>.f2f"]` — это **wildcard** на всю зону. При публикации/удалении
домена CA **не перевыпускается** (cert тот же), просто меняется список
в `MyDomains`. Плюс: keychain trust сохраняется. Минус: внутри camp-а
любой peer's CA технически может выпустить cert для любого имени в
зоне — для friends-circle норм, для большого camp-а нужны per-domain
constraints (см. TODO).

### TLS-termination, не passthrough

Engine на `:443` расшифровывает TLS у себя — backend (gitlab/etc) общается
с engine по plain HTTP на loopback'е. Цена — engine видит plaintext.
Альтернатива (TLS-passthrough по SNI) требовала бы чтобы у backend'а был
TLS-cert на `gitlab.<camp>.f2f`, что снимает с нас половину магии CA.
Termination — стандарт для reverse-proxy (nginx/caddy/traefik делают
так же).

### ECDSA P-256 для CA, не ed25519

ed25519 быстрее и компактнее, но `security` CLI на macOS отказывается
импортировать ed25519-CA («Unknown format in import»). P-256
универсально поддерживается keychain'ом и всеми TLS-клиентами.

### Fingerprint-based check на «уже в keychain'е»

`security find-certificate -c <CN>` (по имени) находит cert даже
если он там лежит как «not trusted» (например, от прошлой сборки
которая упала на trust-добавлении). Тогда мы решили бы «уже стоит»
и не вызвали бы `add-trusted-cert` — trust бы не было. Поэтому
проверяем по SHA-256: «cert именно с этим fingerprint'ом в keychain'е».

### BitTorrent без DHT, trackers, PEX

Стандартный BT работает в публичной экосистеме: DHT для discovery,
tracker'ы для координации, PEX для распространения peer-list'а. Нам
это всё **не нужно** — peer'ы в camp-е и так друг о друге знают
через camp poll, и наш `/api/files` отдаёт магнеты.

`NoDHT/DisableTrackers/DisablePEX = true` гарантируют что наши
файлы **не сольются в публичный swarm** случайно. Info-hash знаком
только тем, кому мы сами раздали список через `/api/files`.

### Seed'ы запускаются в goroutine, не блокируют Start

`anacrolix.NewClient` иногда отрабатывает не мгновенно (allocations,
listener bind, internal init). Если `engine.Start` дождётся его
синхронно — UI замёрзнет в «loading…» на несколько секунд. Решение
— стартуем torrent в goroutine с `defer recover()` — если паникнет,
видим в логе но engine продолжает работать без BT.

### Firewall default-deny на утане, не на peer'е

Альтернатива была — фильтровать на per-peer basis (Alice разрешает
SSH только Bob'у, но не Carol'у). Это требует identity-layer'а
(на каком основании отличать Bob'а от Carol'ы криптографически —
сейчас только по tunnel_ip который доверять нельзя). Поэтому V1 —
firewall **на интерфейсе целиком**, без per-peer rules. Каждый
открытый порт открыт для **всех** членов camp'а одинаково.

Per-peer ACL отложен до Identity Phase 1 (см. TODO.md). Сами keypair'ы
уже генерятся per-camp под `/var/lib/f2f/identity/<camp_id>/` и pub
зеркалится в `<camp_id>/config.json`, но протокол announce / peer-list
их пока не использует. Когда подвяжем подпись announce-пакетов — у
каждого peer'а в roster'е появится верифицируемый `user_pub`, и можно
будет добавить matching по source-IP с привязкой к pubkey.

### Built-in порты — read-only, не редактируются юзером

`2202/tcp`, `80/tcp`, `443/tcp`, `6881/tcp+udp` — это то, без чего
сам engine ломается (peer'ы не достучатся до tunnel-listener'а,
HTTPS proxy, BT). Дать юзеру их выключить — слишком легко
«случайно» отрезать функциональность и потом не понимать что
сломалось. UI показывает их с disabled-чекбоксом + pill'ом
«built-in».

---

## TODO: рефакторинг на слоёную архитектуру + QUIC

### Проблема

`engine.go` — god object: 3354 строки, ~90 методов. В одном файле
перемешаны: управление пирами, hole-punching, 5 polling loops, torrent,
DNS, intercepts, маршрутизация пакетов, firewall, диагностика, SFU.
`web/server.go` — 1600 строк HTTP-хендлеров, знает обо всей бизнес-логике.

### Целевая слоёная архитектура

```
internal/
  core/                    ← фундамент: типы, конфиг, identity, crypto
    types.go               ← PeerInfo, CampConfig, Status и пр.
    config/                ← config store (camp configs, state.json)
    identity/              ← Ed25519 keypairs, camp identity
    overlay/               ← PubToV4Addr, overlay math
    ca/                    ← Certificate Authority
    keychain/              ← macOS keychain helpers

  net/                     ← сетевой уровень: подключение и поддержка связи
    tunnel/                ← utun device (read/write raw IP)
    udp/                   ← UDP socket, hole-punching, NAT traversal
    rendezvous/            ← camp announce, peer list polling
    pair/                  ← pair_req/pair_res — NAT-keepalive + identity-attestation + RTT
    obfenv/                ← control-envelope (camp_key, magic-header ranges)
    peers/                 ← peer state machine (online/offline, endpoints)
    quic/                  ← QUIC connection manager (NEW)

  app/                     ← прикладной уровень: бизнес-логика + свои API
    dns/                   ← local DNS resolver + domain publishing
      service.go           ← бизнес-логика
      api.go               ← RegisterRoutes(mux), RegisterStreams(quic)
    intercept/             ← per-domain routing, route management
      service.go
      api.go
    egress/                ← pf NAT на принимающей стороне
    firewall/              ← default-deny utun + user allow list
      service.go
      api.go
    meet/                  ← WebRTC 1:1 (signaling)
      service.go
      api.go
    meet2/                 ← SFU групповые звонки
      service.go
      api.go
      sfu/                 ← Pion SFU
    drop/                  ← BitTorrent file sharing
      service.go
      api.go
    packet/                ← IP packet parsing + forwarding loops

  web/                     ← тонкий HTTP wiring (только browser ↔ localhost)
    server.go              ← собирает RegisterRoutes() от всех app/ модулей
    assets/

  engine.go                ← тонкий orchestrator: Start/Stop, wires layers
```

### Принципы

- **core/** — нет зависимостей от net/ или app/. Чистые типы и утилиты.
- **net/** — знает о core/, не знает о app/. "Есть ли связь с пиром
  и как до него дотянуться."
- **app/** — знает о core/ и net/. Каждый модуль — отдельный пакет
  со своим `Service` + `api.go` (HTTP routes и QUIC stream handlers).
- **web/** — НЕ содержит бизнес-логики. Только wiring: вызывает
  `mod.RegisterRoutes(mux)` у каждого app-модуля. Цель: < 200 строк.
- **engine.go** — тонкий orchestrator: создаёт слои, Start/Stop.
  Цель: < 500 строк.

### Паттерн: модуль владеет своим API

Каждый app-модуль сам регистрирует свои HTTP-хендлеры и QUIC-стримы:

```go
// app/dns/api.go
func (s *Service) RegisterRoutes(mux *http.ServeMux) {
    mux.HandleFunc("GET /api/domains", s.handleList)
    mux.HandleFunc("PUT /api/domains", s.handleSet)
}

func (s *Service) RegisterStreams(q *quic.PeerConn) {
    q.Handle("domains", s.handleDomainStream)  // push при изменениях
}
```

```go
// web/server.go — только wiring
func (s *Server) routes(mux *http.ServeMux) {
    s.dns.RegisterRoutes(mux)
    s.drop.RegisterRoutes(mux)
    s.meet.RegisterRoutes(mux)
    s.meet2.RegisterRoutes(mux)
    s.firewall.RegisterRoutes(mux)
    // ...
}
```

### Что выносится из engine.go

| Блок | ~Строк | Куда |
|------|--------|------|
| Типы (Status, PeerInfo, InterceptInfo...) | 200 | `core/types.go` |
| peerState + applyPeerList | 120 | `net/peers/` |
| holePunchLoop + restartOnEphemeralPort | 100 | `net/udp/` |
| domainPollLoop + health check + MyDomains | 250 | `app/dns/` |
| filesPollLoop + torrent management | 350 | `app/drop/` |
| peerCAPollLoop + installPeerCA | 180 | `core/ca/` |
| peerFirewallPollLoop | 70 | `app/firewall/` |
| callPollLoop + SFU call management | 180 | `app/meet2/` |
| tunToPeerLoop + peerToTunLoop + routeFor | 200 | `app/packet/` |
| Intercepts + domainRefreshLoop | 250 | `app/intercept/` |
| Diagnostics + Status | 200 | engine (остаётся) |

### Текущий снимок engine.go (для R3-фазы)

После внедрения pair + AWG (2026-06-02) `engine.go` — это **3897 строк,
105 функций, 15 типов**. Реальная карта по строкам:

```
1-77       — packetLog, awgDebug, package doc                       (engine)
83-244     — Config, Status, CampHealth, Diagnostics,               (engine)
             PeerStatusInfo, InterceptInfo, DomainEntry — wire-shapes
254-403    — peerState + IsOnline/IsPaired/IsHalfPaired             (engine)
406-602    — Engine struct, New, LogTap, Subscribe                  (engine)

605-1030   — Start() — 425 строк, главный конструктор всего         (engine)
1033-1071  — ensureCA + CA() accessor                               🟡 core/ca/

1076-1497  — Torrent: startTorrent, prune/refeed, chown, rescan,    🔴 helper/torrent/
             save/load, AddDownload, RemoveDownload, restore        (целый менеджер, ~420 строк)
1521-1546  — currentReflex, trustedPeersDir                         (engine helpers)

1567-1736  — Firewall config: 12 функций про user-rules,            🟡 helper/firewall/
             cleanUserFirewall, merge, persist                       config-side, ~170 строк

1739-1976  — Trusted peer CAs: load, peerCAPollLoop, discover,      🔴 helper/ca/peer_trust.go
             InstallPeerCA, persistTrustedPeerToCamp ~240 строк

1987-2127  — applyPeerList (camp peer-list → e.peers reconcile)     (engine)
2129-2447  — Polling-loops: files/firewall/domains/health           🟡 helper/{files,dns,...}/
             ~320 строк 4 одинаковых паттерна

2454-2545  — SetTunnelHTTPPort, SetDefaultListen, awgSyncPeers,     (engine)
             pairReqPacket
2546-2664  — handlePairReq, handlePairRes                           (engine)
2666-2789  — holePunchLoop, restartOnEphemeralPort                  (engine)
2791-2833  — diagnosticsLocked, campHealthLocked                    (engine)

2836-2961  — Stop()                                                 (engine)
2964-3147  — Status() + peersStatusLocked + helpers                 (engine)

3179-3320  — SetActivePeer, MyDomains/SetMyDomains, LookupHost      🟡 mix: DNS — в dns/,
             (DNS resolver)                                          active-peer/MyDomains — в app/dns

3325-3477  — AddIntercept, RemoveIntercept, addInterceptLocked      🟡 engine/intercept/
             ~200 строк (нужен engine state для AllowedIPs sync)

3479-3593  — domainRefreshLoop, refreshDomainRoutes, resolveSpec    🟡 engine/intercept/

3595-3892  — tunToPeerLoop, routeFor, interceptPeerForLocked,       (engine — core data path)
             peerToTunLoop, ipv4Src, rollbackPartial, sameUDPAddr
```

#### Чистое engine ≈ 1500 строк

Если вытащить только **транспортный substrate**:
- Engine struct + Start/Stop + rollbackPartial
- peerState + IsOnline/IsPaired/IsHalfPaired
- applyPeerList (camp roster → e.peers)
- handlePairReq/Res + awgSyncPeers + pairReqPacket
- holePunchLoop + restartOnEphemeralPort
- tunToPeerLoop + peerToTunLoop + multiplex
- Status builder

Это ~1500 строк чистой «как поднять и поддерживать AWG-туннель».
Остальные ~2400 — features которые engine хостит и которые **должны
делегироваться через интерфейсы**.

#### Приоритизированный план extraction'а

| # | Кандидат | Текущий объём | Целевое место | Сложность |
|---|---|---|---|---|
| 1 | **Torrent (BT manager)** | ~420 строк | `helper/torrent/manager.go` | средняя — нужно вынести state + lifecycle |
| 2 | **Trusted peer CAs** | ~240 строк | `helper/ca/peer_trust.go` | низкая — мало engine-state'а |
| 3 | **Calls (engine/call.go)** | ~280 строк | `helper/call/` (или `sfu/`) | средняя — нужны hooks от engine |
| 4 | **Firewall config** | ~170 строк | `helper/firewall/` (расширить) | низкая |
| 5 | **Polling loops generic** | ~320 строк × 4 | `helper/{dns,files,firewall}/poller.go` | средняя — можно сделать общий паттерн |
| 6 | **Intercepts** | ~200 строк | `engine/intercept/` (всё-таки в engine — нужен awgSyncPeers) | низкая |
| 7 | **DNS LookupHost** | ~70 строк | `helper/dns/resolver.go` через интерфейс | низкая |
| 8 | **Start() декомпозиция** | ~425 строк | разнести по соответствующим пакетам | сложная — много кросс-завязок |

После всех этих extraction'ов `engine.go` ужмётся до ~1500 строк
чистого transport-substrate'а. Каждый extraction — отдельный коммит,
behavior-preserving, проверяется тем что всё работает после рестарта.

**Принцип**: features импортируют engine, engine не импортирует
features. Engine выставляет наружу:
- `Peers() []PeerStatusInfo` (read-only snapshot)
- `OverlayIP() netip.Addr` (own overlay)
- `Subscribe(eventKind)` для событий (peer joined/left, paired/unpaired)
- `SendToPeer(pub, payload []byte)` для in-tunnel HTTP (когда мигрируем
  на QUIC)
- `Identity()` — read-only access

Features используют это для своих нужд. UI/web слой обращается к
features напрямую через их API, не через Engine.

### Что выносится из web/server.go

| Блок | Куда |
|------|------|
| handleListDomains, handleSetDomains | `app/dns/api.go` |
| handleListMyFiles, handleAddMyFile, ... | `app/drop/api.go` |
| handleCallCreate, handleCallJoin, ... | `app/meet2/api.go` |
| handleSignalOutbox, handleSignalInbox | `app/meet/api.go` |
| handleListFirewall, handleSetFirewall | `app/firewall/api.go` |
| handleListIntercepts, handleAddIntercept | `app/intercept/api.go` |
| SSE streams (signalHub, callSignals) | в соответствующий app/ модуль |

### Порядок рефакторинга

**Фаза R1 — core/**
Перенести типы, config, identity, overlay, ca, keychain.
Engine начинает импортировать `core/`. Без изменения поведения.

**Фаза R2 — net/**
Вынести peer state machine, UDP hole-punch (через pair/obfenv),
pair-handshake loop. Engine делегирует "сеть" в net/ через интерфейсы.

**Фаза R3 — app/ модули (по одному)**
Каждый модуль: выделить Service + api.go из engine.go и web/server.go.
Порядок: dns → firewall → intercept → drop → meet → meet2.
Engine становится тонким orchestrator.

**Фаза R4 — QUIC**
`net/quic/` connection manager. app/ модули добавляют `RegisterStreams()`.
Push вместо polling. Удаление tunnel HTTP listener.

---

## TODO: миграция inter-node коммуникации на QUIC

### Зачем

Сейчас ноды общаются по HTTP API поверх tunnel overlay — 5 polling loops
(3–60с интервалы), on-demand POST'ы для сигналинга. Это:
- **Лишний overhead** — HTTP/1.1 headers, TCP handshake на каждый запрос.
- **Polling вместо push** — впустую гоняем запросы когда данные не менялись.
- **Хрупко при обрывах** — TCP ломается при смене сети, sleep/wake.
- **Нет мультиплексинга** — каждый poll-loop независимый, дублирует DNS/connect.

QUIC даёт: connection migration (смена WiFi → соединение живёт), 0-RTT
reconnect, встроенный multiplexing без head-of-line blocking, push-модель
через bidirectional streams.

### Текущие inter-node HTTP endpoints

**Polling loops (engine → peers):**

| Endpoint | Интервал | Timeout | Данные |
|----------|----------|---------|--------|
| `GET /api/domains` | 10с | 3с | Список опубликованных доменов + health |
| `GET /api/ca-cert` | 30с | 5с | CA-сертификат (PEM) |
| `GET /api/files` | 60с | 5с | Торрент-каталог (name, size, magnet) |
| `GET /api/firewall` | 30с | 5с | Открытые порты пира |
| `GET /api/call/state` | 3с | 3с | Состояние группового звонка |

**On-demand (real-time):**

| Endpoint | Когда | Данные |
|----------|-------|--------|
| `POST /api/signal/inbox` | WebRTC 1:1 сигналинг | Offer/answer/candidate JSON |
| `POST /api/call/signal` | SFU сигналинг | Offer/answer/candidate/renegotiate |
| `POST /api/call/join` | Вход в групповой звонок | Name, tunnel_ip |
| `POST /api/call/leave` | Выход из группового звонка | — |

### Целевая архитектура

```
Peer A ──QUIC──► Peer B
  │                │
  ├─ stream: domains (push)
  ├─ stream: files (push)
  ├─ stream: firewall (push)
  ├─ stream: ca-cert (one-shot)
  ├─ stream: call-state (push)
  ├─ stream: webrtc-signal (bidirectional)
  ├─ stream: sfu-signal (bidirectional)
  └─ stream: sfu-control (join/leave)
```

- **Один QUIC connection на пару пиров** поверх tunnel overlay IP
  (100.64.x.y). Устанавливается после hole-punch, когда пир online.
- **Мультиплексированные стримы** по типу данных. Каждый стрим —
  отдельный канал, без head-of-line blocking.
- **Push вместо polling** — нода шлёт обновления только когда данные
  менялись (domains changed → push по стриму). Подписчик получает
  мгновенно, без задержки до следующего poll-тика.
- **Bidirectional streams** для сигналинга — request/response на одном
  стриме, без HTTP overhead.
- **0-RTT reconnect** — после sleep/wake или обрыва восстановление
  без полного хендшейка. Connection ID не привязан к IP:port.
- **HTTP остаётся только для browser ↔ localhost** (UI API на loopback).

### План миграции

**Фаза 0 — Пакет `internal/quic`**
- QUIC connection manager: dial/accept, reconnect, health monitoring.
- Привязка к `engine.peers` — открывать соединение когда пир online,
  закрывать когда offline.
- TLS на основе существующего per-camp Ed25519 identity (или
  самоподписанный cert из `internal/ca`).
- Listener на tunnel IP, порт по конвенции (напр. 2203/udp).

**Фаза 1 — Push-модель для polling loops**
- Заменить `domainPollLoop` → push stream: при изменении `MyDomains`
  нода шлёт обновление всем подключённым пирам.
- Аналогично `filesPollLoop`, `peerFirewallPollLoop`, `peerCAPollLoop`.
- `callPollLoop` → push stream для call state.
- На время миграции: fallback на HTTP polling если QUIC-соединение
  не установлено.

**Фаза 2 — Сигналинг через QUIC**
- WebRTC 1:1 сигналинг (`/api/signal/inbox`) → bidirectional stream.
- SFU сигналинг (`/api/call/signal`) → bidirectional stream.
  Заменяет `deliverSFUSignal` HTTP roundtrip.
- SFU control (`/api/call/join`, `/api/call/leave`) → request/response
  на control stream.

**Фаза 3 — Убрать tunnel HTTP listener**
- Удалить `BindTunnel` / `tunnelSrv`.
- Удалить все `handleXxxRemote` handlers.
- HTTP mux остаётся только на loopback для browser UI.
- Firewall: закрыть 2202/tcp на utun, открыть 2203/udp (QUIC).

### Что НЕ мигрирует

- **Browser ↔ localhost HTTP API** — остаётся как есть (loopback mux).
- **Camp server HTTP polling** (`rendezvous/peerlist.go`) — это внешний
  сервер на fly.io, не inter-node.
- **UDP hole-punch / announce** — низкоуровневый UDP, не HTTP.
- **WebRTC media (RTP/RTCP)** — идёт через Pion ICE, отдельный transport.

### Открытые вопросы

- **Библиотека**: `quic-go` (зрелая, MIT) vs `go` stdlib (Go 1.24+
  экспериментальный `net/quic`)?
- **Аутентификация**: mutual TLS на Ed25519 identity или
  pre-shared key из camp_id?
- **Порт**: фиксированный (2203) или discovery через camp server?
- **Обратная совместимость**: нужен ли переходный период когда ноды
  поддерживают и HTTP и QUIC (mixed fleet)?

---

## TODO: транспортное шифрование через AmneziaWG

### Зачем

Сейчас весь трафик через туннель идёт **plaintext UDP** — IP-пакеты с
utun заворачиваются в UDP-датаграмму и летят между пирами без
шифрования. Что это значит:

- **Содержимое читается** провайдером и любым на пути. HTTPS внутри
  туннеля защищён сам по себе, но DNS-запросы (мы держим свой
  резолвер на `<camp>.f2f`, но запросы peer→peer идут открытыми),
  HTTP без TLS, RDP, SMB и прочее — в открытом виде.
- **Сам факт VPN-туннеля заметен**: регулярный UDP-флоу между двумя
  хостами с характерным паттерном (1-байт keepalive каждые 25с,
  периодический STUN-обмен с camp-сервером) — DPI учится это
  опознавать. WireGuard уже режется по части провайдеров в РФ;
  любой узнаваемый VPN — следующий кандидат.

Целевое состояние: **encrypted-by-default transport** на базе AmneziaWG
с DPI-обфускацией. AmneziaWG — форк WireGuard (Noise IK +
ChaCha20-Poly1305) с дополнительными полями (`Jc`, `H1..H4`, `I1..I5`,
`S1..S4`), которые делают трафик неотличимым от произвольного потока
байтов или замаскированным под известный протокол.

### Что AmneziaWG приносит поверх WireGuard

- **`Jc`/`Jmin`/`Jmax`** — N мусорных UDP-пакетов случайной длины
  перед каждым handshake'ом. DPI видит шум, не WG handshake-сигнатуру.
- **`S1`..`S4`** — рандомный padding для handshake init/response/cookie
  и transport-пакетов. Длина перестаёт быть отличительным признаком
  (у ванильного WG handshake-пакеты фиксированных размеров).
- **`H1`..`H4`** — кастомные magic-байты вместо WG-овских `0x01..0x04`
  для типов init/response/cookie/transport. Главный fingerprint
  WG ломается. Значения задаются диапазонами (`1000-2000` →
  каждый раз случайное из диапазона), чтобы DPI не зацепился даже
  за фиксированное число.
- **`I1`..`I5`** — кастомные signature-пакеты по DSL. Самая интересная
  фича. Каждый описывается строкой типа `<b 0x474554><rc 12><r 100><t>`:
  статичные байты "GET", 12 случайных букв, 100 случайных байт,
  4-байт timestamp. Тэги:
  - `<b 0x..>` — статичные байты
  - `<r N>` — N случайных байт
  - `<rc N>` — N случайных букв `[a-zA-Z]`
  - `<rd N>` — N случайных цифр `[0-9]`
  - `<t>` — 4-байт unix timestamp

  Через I1..I5 можно слать пакеты, которые выглядят как фрагмент
  HTTP-запроса, mDNS, Steam-handshake — что угодно. DPI смотрит
  первые байты, видит "GET ", решает "это HTTP, не моя забота".

### Принципы интеграции

1. **Camp не трогаем вообще**. Camp-сервер остаётся как есть, ноль
   строк правок. Транспортные ключи (X25519) обмениваются **напрямую
   между пирами** через расширенный hole-punch (см. "Pair-handshake"
   ниже), без camp как посредника. Camp по-прежнему знает только
   Ed25519 pub + UDP endpoint каждого peer'а — этого ему достаточно
   для rendezvous'а и overlay-адресации, и **недостаточно** для MITM:
   даже скомпрометированный camp не сможет подменить транспортные
   ключи и расшифровать трафик.

2. **`amneziawg-go` как библиотека, не приложение**. У них публичный
   API: `device.NewDevice(tdev tun.Device, bind conn.Bind, logger *device.Logger)`
   + `device.IpcSet(uapiBlob string)`. `Bind` и `tun.Device` —
   интерфейсы, мы реализуем свои поверх существующего UDP-сокета
   и utun. Никакого UAPI unix-сокета, никакого демона, никакого
   `wg-quick` поверх.

3. **Существующие фичи engine не теряем**: NAT-traversal (теперь
   через pair_req, а не 1-byte `0x00`), camp announce, RTT-measurement
   (поглощено pair_res с echo_ms — отдельный pinger удалён),
   intercepts, route table, firewall, DNS, BT, CA — всё остаётся.
   Меняется байтовый transport (зашифрован) и control-plane
   (обфусцирован), не функциональность.

4. **Identity один на пир**. Не два keypair'а для разных целей —
   X25519 для AWG derive'ится из существующего Ed25519 seed, источник
   истины один (`priv.key`). См. ниже.

5. **Backwards-compatible rollout**. Plaintext-fallback пока хотя бы
   один пир в паре на старой версии. Никакого "big-bang обновите
   всех одновременно".

### Идентификация: Ed25519 ↔ X25519

#### Текущее состояние

Per-camp Ed25519 keypair в `/var/lib/f2f/identity/<camp_id>/{priv,pub}.key`
— это `identity.Identity` (см. `identity/identity.go`). Pub в hex
транзитом через camp в `PeerInfo.Pub` (`rendezvous/types.go:19`).
Из pub deriv'ится overlay-адрес `100.64.X.Y` (`overlay.PubToV4Addr`).

Сейчас используется для: подписи invite-токенов (`identity.GenerateInvite`),
identifier'а на camp-сервере (rendezvous), derivation
overlay-IP.

#### Целевое: X25519 derive'ится из Ed25519 seed

WG/AWG handshake — это **Noise IK на Curve25519**. Нужны X25519
priv/pub у обоих пиров, при этом pub второго заранее известен.

Два логичных варианта:

| Вариант | Плюс | Минус |
|---|---|---|
| **A**: отдельный X25519 keypair рядом с ed25519 на диске | Полная криптографическая независимость | Два источника истины, два файла, два места ротации, два бекапа |
| **B**: X25519 deriv'ится из Ed25519 seed через HKDF | Один источник истины, derive каждый старт, бекап один | Утрата одного → утрата другого (один и тот же threat model) |

**Выбираем B**. Логика:

- `ed25519.PrivateKey` в Go = 64 байта (32 seed + 32 pub). Seed — то,
  из чего derive'ится весь keypair.
- `x25519_scalar = HKDF-SHA256(ed25519_seed, salt=nil, info="f2f-wg-static-v1")`,
  затем `x25519_pub = curve25519.ScalarBaseMult(x25519_scalar)`.
- Тег `info` критически важен: даёт **криптографически независимый**
  ключ из той же seed (стандартный паттерн домейн-сепарации,
  используется в Signal, libsignal, Wire).
- Версия в info (`-v1`) — заранее зарезервированный slot на ротацию
  алгоритма деривации без смены seed'а на диске.

**Почему не "Ed25519 priv напрямую в X25519 priv" (через birational
equivalence из RFC 7748)?** Технически возможно. Но результат
**статистически коррелирован** с исходным ключом — теоретическая
утечка одного даёт частичные данные о другом, плюс некоторые атаки
на сторонние каналы становятся переносимыми между алгоритмами.
HKDF с тегом этого избегает.

**На диск НЕ сохраняем.** Derive'ится за миллисекунды, занимает
считанные байты RAM. Меньше state = меньше путей утечки. Бекап
`/var/lib/f2f/identity/` автоматически бекапит и WG identity (как
производное).

#### Обмен WGPub через pair-handshake (НЕ через camp)

Camp **не** видит и не передаёт WGPub. Это закрывает вектор MITM от
скомпрометированного camp-сервера: если camp подменит endpoint
peer'а на свой адрес, без подменённого WGPub он не сможет завершить
WG-handshake (не знает соответствующий X25519 priv). А подменить
WGPub он не может, потому что WGPub через него вообще не ходит.

Вместо этого WGPub обменивается **напрямую между пирами** через
расширенный hole-punch — два сообщения, `pair_req` и `pair_res`,
которые одновременно (a) открывают NAT, (b) подтверждают identity,
(c) меряют RTT. Они **полностью заменили** два предыдущих механизма:
- старый 1-byte `0x00` hole-punch (просто пробивал NAT)
- старый pinger ping/pong (мерял RTT в plaintext JSON)

Pinger как отдельный loop больше не существует — `engine/peerping/`
удалён, RTT приходит из `pair_res` через `echo_ms`.

##### Pair packets (req + res)

Два типа JSON-payload'а, оба заворачиваются в один и тот же
control-envelope (`H5` magic):

```json
// pair_req — шлётся по schedule из holePunchLoop
{
  "t": "pair_req",
  "name": "alice",
  "pub": "<ed25519_pub_hex>",
  "wg_pub": "<x25519_pub_hex>",
  "sent_ms": 1735000000123,
  "sig": "<ed25519_sig_hex>"
}

// pair_res — шлётся fire-on-receive в ответ на каждый valid pair_req
{
  "t": "pair_res",
  "name": "alice",
  "pub": "<ed25519_pub_hex>",
  "wg_pub": "<x25519_pub_hex>",
  "sent_ms": 1735000000456,
  "echo_ms": 1735000000123,
  "sig": "<ed25519_sig_hex>"
}
```

Подпись каждого варианта покрывает разный canonical message с
**разными domain-тегами**:

```
sig_req = ed25519.Sign(ed_priv, "f2f-pair-req-v1|" + name + "|" + pub + "|" + wg_pub + "|" + sent_ms)
sig_res = ed25519.Sign(ed_priv, "f2f-pair-res-v1|" + name + "|" + pub + "|" + wg_pub + "|" + sent_ms + "|" + echo_ms)
```

Разные теги критичны: без них подпись от `pair_req` была бы валидна
как подпись `pair_res` (поля идентичны кроме echo_ms), и атакующий с
camp_key мог бы переклеивать тип.

`echo_ms` в pair_res — это копия `sent_ms` из triggering pair_req.
Receiver of pair_res computes RTT = now - echo_ms, при условии что
`echo_ms == LastSentReqMs` (защита от stale echoes после ротации
sent_ms).

##### Verify flow

При получении любого pair-пакета (req или res):

1. Расшифровать envelope: `obfenv.Open(packet, camp_key)`. Bad tag → drop.
2. Тип определяется по полю `t` в JSON. Диспатчим в `pair.ParseReq` или
   `pair.ParseRes`.
3. Найти peer по `pub` (Ed25519) в `e.peers`. Не-член camp'а → drop.
4. Проверить подпись (домен-тег учитывается ParseReq/ParseRes
   автоматически).
5. Подпись валидна → атомарно сохранить `peer.WGPub = wg_pub`,
   обновить `LastValidReqMs` / `LastValidResMs`.
6. Для pair_res дополнительно: если `echo_ms == p.LastSentReqMs` →
   `RTT = now - echo_ms`, сохранить в `LastRTTMs`. Если не совпало —
   stale echo, RTT не считаем.
7. Для pair_req дополнительно: **немедленно** ответить pair_res'ом с
   нашим `sent_ms` и `echo_ms = req.sent_ms`.

##### Lifecycle

`pair_req` шлётся **из holePunchLoop** на схеме 1Hz burst → 25с
keepalive (порог зависит от `LastValidResMs` свежести — двусторонняя
проверка).

`pair_res` — **fire-on-receive**, отдельной cadence нет. Каждый
valid pair_req → синхронный response.

Следствия:
- **Continuous identity-attestation** — атакующий, который влез после
  установления, должен подделывать ed25519-подпись на каждом keepalive
  (невозможно без `priv.key`).
- **RTT обновляется каждые 25с в steady state** — нам нашего pair_req
  ответили res'ом, RTT измерился.
- **Потеря пакета не критична** — следующий tick принесёт всё заново.
- **Ротация identity** (rotated `priv.key`) → новый pub → не находится
  в `e.peers` → drop. Ротация требует обновления camp roster через
  announce.

##### Что НЕ кладём в pair

- `overlay_v4` — derive'ится из `pub` детерминистически
  (`overlay.PubToV4Addr`), передавать не нужно.
- `domains`, `files`, `intercepts` — polling-данные, меняются в
  runtime, остаются в `/api/*` endpoint'ах поверх tunnel.
- `udp_endpoint` — сам факт получения pair-пакета даёт `from` адрес.

##### Что ЕЩЁ можно положить (опционально)

- `awg_profile` (Jc/H1..H4/I1..I5) — если решим per-pair профили
  обфускации (см. "Параметры обфускации" ниже). Подписаны той же
  `sig`, поэтому camp не может их подменить.
- `version` (proto version) — для будущей feature-negotiation.

Эти поля добавляются аддитивно — старые клиенты их игнорируют.

##### Обфускация: control-envelope для pair-handshake

Pair-пакеты в plaintext-виде — это JSON с `pub`, `wg_pub`, `sig`,
`sent_ms` (и `echo_ms` у res) — для DPI очень узнаваемая сигнатура:
те же поля каждый tick, та же структура, hex-строки фиксированной
длины. Если DPI настроится конкретно на f2f, такие пакеты будут
палиться мгновенно.

Поэтому **оба** типа (req и res) заворачиваются в один и тот же
**control-envelope** того же духа, что AWG-обёртка трафика:

```
[magic_h5..h8 (4 bytes, random in negotiated range)]
[nonce (12 bytes, random per packet)]
[ChaCha20-Poly1305(camp_key, json_payload)]
[poly1305 tag (16 bytes)]
```

`camp_key` — симметричный 32-байт ключ, **общий для всех членов camp'а**:

```
camp_key = HKDF-SHA256(
    camp_id_bytes,         // IKM: camp_id уже у всех есть из invite
    salt = nil,
    info = "f2f-control-v1"
)
```

Ключевые свойства:

- `camp_key` **не криптографически secret** — любой, у кого есть invite,
  его может вывести. Это нормально, потому что цель ключа — **только
  обфускация от DPI**, не аутентификация. Аутентификацию даёт
  ed25519-подпись внутри расшифрованного JSON.
- Поэтому атакующий с invite видит наши pair-пакеты, но **атакующий без
  invite видит шум** — те же `[magic | random bytes | tag]`, что и AWG.
- Camp-сервер `camp_key` тоже может вывести (он знает `camp_id` из
  каждого announce'а) — это пригодится, если решим обфусцировать
  camp announce тоже (см. ниже).

##### Magic headers `H5..H8`

Помимо AWG'шных `H1..H4` (для четырёх типов AWG-пакетов:
init/response/cookie/transport) выделяем **четыре дополнительных
диапазона `H5..H8`** для control-envelope:

| Header | Что внутри |
|---|---|
| `H5` | pair-handshake (`pair_req` и `pair_res` — один slot, тип определяется по полю `t` внутри расшифрованного JSON) |
| `H6` | резерв (будущие control-типы — например, AWG-profile-handoff, key-rotation request) |
| `H7` | резерв |
| `H8` | резерв |

Все восемь диапазонов **derive'ятся детерминистически из camp_id**
через HKDF с разными info-тэгами (`f2f-magic-h1-v1`..`f2f-magic-h8-v1`).
Два пира в одном camp'е получают одинаковые ranges без какого-либо
дополнительного обмена — camp_id уже у обоих из invite. Они **не
пересекаются** между собой и не пересекаются со старыми
дискриминаторами (`0x40..0x4F` IPv4, `0x60..0x6F` IPv6, `{` JSON
для legacy plaintext-fallback).

##### Multiplex с envelope'ом

`peerToTunLoop` после введения envelope'а становится:

```go
firstU32 := binary.LittleEndian.Uint32(pkt[:4])
switch {
case withinRange(firstU32, H1..H4):
    awgBind.Recv(pkt)                       // зашифрованный AWG
case withinRange(firstU32, H5..H8):
    plain := decryptEnvelope(pkt, camp_key) // расшифровать control
    handleControl(plain)                    // диспатч по "t" в JSON
case isIPv4or6(pkt[0]):
    // plaintext-fallback пиры — старая логика
default:
    drop()
}
```

DPI снаружи: видит UDP с разными magic, все остальное — псевдослучайные
байты, индистинктно от шифрованного трафика.

##### Pinger удалён — pair его поглотил

В первой версии плана предполагался отдельный pinger-loop в plaintext
JSON, который мерял RTT, а после внедрения AWG он "переезжал" внутрь
зашифрованного туннеля. Этот шаг устарел: после введения
pair_req/pair_res с echo_ms — RTT уже измеряется в самом
pair-handshake'е, отдельный pinger не нужен.

Пакет `engine/peerping/` удалён целиком. RTT и двусторонняя
достижимость теперь читаются из `peerState.LastRTTMs` /
`LastValidResMs` — оба заполняются в `handlePairRes` после успешной
верификации подписи и матча `echo_ms ↔ LastSentReqMs`.

### Транспортный слой: вживление amneziawg-go

#### Точка интеграции

`amneziawg-go` собирается через `device.NewDevice(tdev, bind, log)`,
где:
- `tdev` — наша обёртка над utun. Сейчас `tunnel.Tunnel` использует
  `golang.zx2c4.com/wireguard/tun`; amnezia форкнул его в
  `github.com/amnezia-vpn/amneziawg-go/tun`. Интерфейсы совместимы
  (форк), нужна или замена импорта, или тонкий адаптер.
- `bind` — наш `engine/awg.Bind`, реализует `conn.Bind` поверх
  существующего `e.udp`.
- `log` — `device.Logger` с прокидыванием в `engine.tap`.

Конфигурация через `device.IpcSet("private_key=...\npublic_key=...\nendpoint=...\n...")`
— простой текстовый протокол WG UAPI. Никаких unix-сокетов, никакого
демона, IpcSet — обычный Go-метод, принимает `io.Reader`.

#### Мультиплекс на одном UDP-сокете

До AWG, после pair-handshake внедрения (текущее состояние)
`peerToTunLoop` (`engine.go:3335`) дискриминирует:

| Первый uint32 | Что | Дальше |
|---|---|---|
| `H5` | pair-handshake envelope | decrypt camp_key → JSON → `pair_req` / `pair_res` диспатч |
| `H6..H8` | reserved control slots | drop (handler'ов пока нет) |
| `0x40..0x4F` | IPv4 packet | `tun.Write` (plaintext-fallback пиры) |
| `0x60..0x6F` | IPv6 packet | `tun.Write` (plaintext-fallback) |
| `{` (`0x7B`) | camp announce reply | существующий `announce.HandlePacket` |
| прочее | drop |  |

После внедрения AWG (шаги #4-#7) добавится:

| Первый uint32 | Что | Дальше |
|---|---|---|
| `H1..H4` | AWG transport/handshake | `awgBind.Recv` → Device расшифровывает → пишет в utun |

**1-byte hole-punch исчез** (commit `7757470`) — его роль (открывать
NAT) полностью поглощена pair_req в envelope'е.

`Bind.Open()` возвращает `[]ReceiveFunc`; каждый получает AWG-пакеты
через канал, в который пушит `peerToTunLoop` после первого matchа.
Device читает из ReceiveFunc, расшифровывает по WG-протоколу,
проверяет `allowed_ips`, и сам пишет в utun. Engine про этот путь
дальше не знает.

`H1..H8` выбираются при создании camp'а так, чтобы **не пересекаться**
между собой и со старыми дискриминаторами (`0x40..0x4F`, `0x60..0x6F`,
первый байт `0x7B`). Например, все восемь диапазонов в `0x80..0xBF`
с делением на 8 равных подзон.

#### Что переезжает в Device, что остаётся в engine

| Было в engine | Стало |
|---|---|
| `tunToPeerLoop` — utun → UDP | `Device.RoutineReadFromTUN` (внутри AWG) |
| `peerToTunLoop` — UDP → utun (часть после мультиплекса) | `Device.RoutineDecryption` + Device сам пишет в utun |
| `routeFor(pkt)` — find peer by dst IP | `Device.allowedips` (radix-trie матч в AWG) |
| `interceptPeerForLocked` — match CIDR → peer | `allowed_ip = CIDR` на нужном peer'е через IpcSet |
| `peer.UDPAddr` | `endpoint=host:port` peer'а через IpcSet |
| 1-byte hole-punch (`0x00`) и `engine/peerping/` | **Уже удалены** (commit `7757470`) — pair_req делает NAT-keepalive, pair_res несёт RTT через echo_ms |
| `pair_req` (JSON, signed) + `pair_res` в control-envelope (`H5`) | **Уже добавлены** (commits `1aec38d`+`7757470`) — открывают NAT + identity-attestation + RTT |
| Camp announce, peer poll | **Остаётся в engine** (camp по-прежнему plaintext, см. open question) |
| Мультиплекс на recv | **Уже добавлен** — `pair.Type(plain)` диспатчит между req/res |

То есть engine **отдаёт** криптографию + само routing-решение, но
**сохраняет** контроль над всем остальным.

#### Маппинг f2f абстракций → WG UAPI

| f2f сейчас | WG UAPI ключ |
|---|---|
| `e.identity.X25519Priv` (derived) | `private_key=<hex>` |
| `peer.WGPub` (из verified pair_req/pair_res) | `public_key=<hex>` |
| `peer.UDPAddr` | `endpoint=<host:port>` |
| `overlay.PubToV4Addr(peer.Pub)` | `allowed_ip=100.64.X.Y/32` |
| `InterceptInfo.Prefixes` (bound to peer) | `allowed_ip=<CIDR>` (peer'у-владельцу) |
| hole-punch keepalive (наш, для NAT) | `persistent_keepalive_interval=25` |

Это **точное** покрытие 1:1, никаких "потерянных" фич — даже
intercept'ы выражаются естественно через `allowed_ips`.

### Параметры обфускации: где живут

`Jc`, `Jmin`, `Jmax`, `H1`..`H4`, `S1`..`S4`, `I1`..`I5` — должны
совпадать у обоих пиров (иначе handshake не пройдёт). Три варианта
хранения:

1. **Hardcoded в коде** — одинаковые для всех camp'ов. Простейшее.
   Минус: один fingerprint на весь fleet; DPI, обученный против
   одного camp'а, режет всех.
2. **В `<camp_id>/config.json`**, генерится при создании camp'а
   на клиенте-создателе и распространяется через invite-токен.
   Per-camp профиль. Camp-сервер не знает параметров.
3. **Передаётся через pair-handshake** между пирами (как WGPub).
   Per-pair профиль обфускации без участия camp'а — два пира в одном
   camp'е могут даже использовать разные профили между собой.

**Рекомендация — вариант 2.** Аргументы:

- Per-camp профиль = два разных camp'а с одинаковыми участниками
  выглядят для DPI по-разному. Каждый camp — отдельная цель для
  обучения.
- Camp-сервер не вовлечён вообще (как и для WGPub) → утечка camp'а
  ничего не даёт DPI про обфускационные параметры.
- При создании camp'а у нас уже есть RNG в `identity.Generate()` —
  параметры обфускации генерим там же, кладём в `Camp.AWGProfile`
  в `<camp_id>/config.json`.
- Invite-токен расширяется `AWGProfile`-блоком — новый участник
  видит профиль сразу при вступлении, до первого announce'а.

### Pair-handshake, NAT и AWG handshake — порядок

Pair_req и hole-punch — это **один и тот же пакет** в новом дизайне.
Pair_req open'ит NAT (полезный UDP-пакет с peer-to-peer endpoint'а на
peer-to-peer endpoint), и заодно несёт identity + WGPub + sent_ms в
обёртке control-envelope. Pair_res идёт в ответ — несёт echo_ms для
RTT-измерения. Никаких отдельных 1-byte пакетов, никакого pinger'а.
Порядок:

1. **Camp poll** — engine узнаёт, что у peer X появился endpoint
   `1.2.3.4:5678` и его Ed25519 `pub`. WGPub camp **не знает**.
2. **Engine pair_req (через holePunchLoop)** — шлёт pair_req-пакет в
   control-envelope (`H5` magic + ChaCha20-Poly1305) на endpoint.
   Это и есть hole-punch — пакет одновременно открывает NAT и
   несёт подписанный identity + наш WGPub. Если peer X тоже шлёт
   встречный pair_req — NAT с обеих сторон открыт, мы получили его
   WGPub.
3. **Engine fire-on-receive pair_res** — на каждый valid входящий
   pair_req engine мгновенно строит и отсылает pair_res с нашим
   sent_ms и echo_ms = req.sent_ms. Это даёт оппоненту чистую RTT.
4. **Engine verify pair_res** — на каждый входящий pair_res
   расшифровывает envelope, парсит JSON, проверяет подпись. Если
   `echo_ms == LastSentReqMs` → `RTT = now - echo_ms`, сохраняется в
   `peer.LastRTTMs`. Невалидная подпись → drop, peer остаётся в
   plaintext fallback.
5. **Engine кормит AWG endpoint'ом + WGPub'ом** —
   `device.IpcSet("public_key=<verified_wg_pub>\nendpoint=1.2.3.4:5678\nallowed_ip=...")`.
   Если valid pair-handshake ещё не сложился — IpcSet не вызывается.
6. **AWG keepalive** — Device отправляет первый handshake init через
   уже открытый NAT (наш pair_req его пробил, идёт по той же
   траектории).
7. **Handshake завершается** — обмен симметричными ключами, AllowedIPs
   активны, трафик через этого peer'а шифруется.

Важно: **AWG keepalive ≠ engine pair_req**. AWG keepalive шлёт
зашифрованные WG-transport-пакеты раз в N секунд для поддержания
WG session state'а (`H1..H4` magic); pair_req — это control-envelope
пакет (`H5` magic) с подписанным identity'ом + sent_ms. Они уживаются
параллельно: pair_req продолжает continuous attestation + open NAT
+ RTT-обновления (через pair_res от peer'а), AWG keepalive
поддерживает WG state.

### Status model: paired / half-paired / unreachable / offline

После внедрения pair-handshake'а есть два **независимых** crypto-сигнала,
которые UI комбинирует в человеко-читаемый статус:

- `LastValidReqMs` — последний валидный `pair_req` **от** этого peer'а
- `LastValidResMs` — последний валидный `pair_res` от этого peer'а **на наш** `pair_req`

Оба нужны, чтобы понять направление связи:

| Color | Status | Условие | Что значит |
|---|---|---|---|
| 🟢 paired | bidirectional crypto-attested | `LastValidReqMs < 30s` **И** `LastValidResMs < 30s` | Их pair_req приходит И наш pair_req получает ответ — связь подтверждена в обе стороны, RTT измеряется |
| 🟡 half-paired | one-way | `LastValidReqMs < 30s` **И** `LastValidResMs >= 30s` | Они активно пингуют нас (их pair_req свежий), но наш pair_req не получает свежий pair_res. Либо наш send-path до них сломан, либо их ответ теряется по дороге |
| 🔴 unreachable | в roster, их pair_req стух | `InCamp=true`, `LastValidReqMs >= 30s` (стало неважно что с res) | Их keepalive прекратился — они либо ушли, либо на старой версии без pair. Состояние "наш res свежий, их req стух" тоже сюда — это значит они перестали пинговать, остатки данных стухнут сами |
| ⚪ offline | не в roster | `InCamp=false` | — |

> **Почему half-paired асимметричен.** Если их `pair_req` перестал
> приходить — peer фактически "исчез" с нашей стороны, остатки
> `LastValidResMs` от прошлых раундов уже бессмысленны. А если их
> `pair_req` идёт, но `pair_res` на наш `pair_req` не возвращается —
> peer **активно живёт и пытается общаться**, просто связь
> однонаправленная. Это качественно разные ситуации, поэтому только
> вторую помечаем оранжевым; первая попадает в красный.

Заметь: **legacy/старая версия НЕ имеет своего цвета**. После полного
раската hardening'а (см. шаг #10) старые ноды без pair не появятся в
этой таблице как "reachable" вообще — они автоматически попадают в
🔴 unreachable. Это **фича, не баг** — сигнал к обновлению.

Поле `Verified` в `PeerStatusInfo` сейчас = `LastValidResMs < 30s` для
обратной совместимости с UI, который ещё знает только бинарный
verified/not. После доработки UI можно ввести явное поле `Status`
(или пара `Paired bool` + `HalfPaired bool`) и переключить
цветовую логику.

### Backward compatibility

| Сценарий | Поведение |
|---|---|
| Оба пира — новая версия, обмен pair_req+pair_res успешен | Шифрованный transport через AmneziaWG (когда AWG будет включён) |
| Один пир на старой версии (шлёт `0x00` hole-punch вместо pair_req) | Plaintext fallback с этим peer'ом (новый ждёт pair_req, не получает) |
| Pair-пакет получен, но подпись невалидна (camp подменил? битый пакет?) | Drop, plaintext fallback, warning в логи |
| Pair-handshake валиден, но AWG handshake не прошёл (network / отказ peer'а) | Logging warning, plaintext fallback по timeout'у |

Это даёт **пошаговое раскатывание**: один пир обновляется → его
трафик с не-обновлёнными остаётся plaintext, с обновлёнными —
шифрован. Никакого big-bang. **Без координации с camp-deploy'ом
вообще** — пары пиров договариваются напрямую.

Когда раскатывание полное — добавляется флаг `RequireAWG bool` в
camp config; при `true` engine отказывается общаться с peer'ами,
от которых не пришёл валидный pair_req + pair_res + WGPub. Это уже
политическое решение, не техническое.

### План внедрения

Каждый пункт — отдельный коммит, можно посмотреть/откатить
независимо. Завершённые помечены ✅, текущая граница — после ✅ всё
working, на проводе уже непрозрачные envelope'ы с pair-handshake'ом.

✅ **#1 `identity.X25519()`** — derive X25519 keypair из Ed25519 seed
   через HKDF (`info="f2f-wg-static-v1"`). Unit-тест на детерминизм
   (одна seed → один и тот же x25519). Pub публикуется методом
   `X25519PubHex()`. Commit `8b98e7c`.

✅ **#2 Control-envelope (`engine/obfenv/`)** — `Seal`/`Open` функции
   обёртки ChaCha20-Poly1305 + magic header. `camp_key` derive'ится
   из `camp_id` через HKDF (`info="f2f-control-v1"`). 8 magic-header
   диапазонов (`H1..H8`) тоже derive'ятся из camp_id —
   `<camp_id>/config.json` **не нужен**, всё детерминистически из
   camp_id. Unit-тесты на roundtrip + reject невалидного tag'а.
   Commit `59fe31e`.

✅ **#3 Pair-handshake (`engine/pair/`)** — pair_req/pair_res с
   sent_ms/echo_ms. Canonical messages (`f2f-pair-req-v1|...`,
   `f2f-pair-res-v1|...`) с domain-separation. Sign/verify через
   `identity.Sign` + raw `ed25519.Verify`. Pair_req шлётся из
   `holePunchLoop` в envelope (`H5` magic), pair_res fire-on-receive.
   `peerToTunLoop` мультиплексирует, верифицирует, обновляет WGPub +
   LastValidReqMs/ResMs + LastRTTMs. **Полностью заменил** 1-byte
   `0x00` hole-punch И весь `engine/peerping/` (RTT теперь из
   pair_res). **Camp не трогается вообще.** Commits `1aec38d` +
   `7757470`.

✅ **#4 `engine/awg/bind.go`** — реализация `conn.Bind` поверх `e.udp`.
   `Send`/`Receive`/`ParseEndpoint`/`BatchSize`. Inbox-канал на 64
   пакета, ReceiveFunc дрейнит. `Deliver()` принимает классифицированные
   engine'ом AWG-пакеты. Commit `3a0b305`.

✅ **#5 Discriminator AWG → bind** — `peerToTunLoop` мультиплексирует:
   при `obfenv.SlotFor(firstU32) ∈ {SlotAWGInit..SlotAWGTransport}`
   вызывает `awgBind.Deliver(pkt, from.AddrPort())`. Commit `5909e4e`.

✅ **#6 + #7 `engine/awg/device.go` + Peer sync** — `device.NewDevice`
   создаётся на Engine.Start (после tun.Open и UDP-сокета). `IpcSet`
   с base config (private_key, listen_port=0, h1..h4 derive'нутые из
   camp_id через `obfenv`). `awgSyncPeers` собирает verified peers и
   пушит peer-блоки. Triggers: `handlePairReq`/`handlePairRes` (на
   firstReq/firstRes) + каждый `applyPeerList` (camp poll). Commit
   `0903b5f`.

   **Замечание про amneziawg-go v1.0.4 UAPI**: парсер `h1..h4` ждёт
   **одно uint32** (не range, как в master). Мы шлём `start` slot'а —
   приёмная сторона матчит по своему `obfenv.SlotFor(start)` (start
   в начале slot range, попадает в [start, end)).

✅ **#8 Intercept sync** — `awgSyncPeers` собирает для каждого peer'а
   `cidrs = [overlay/32] + [info.Prefixes for info in e.intercepts if
   info.Peer == p.Name]` и пушит через UAPI как несколько `allowed_ip=`
   строк. `AddIntercept`/`RemoveIntercept` дёргают `go e.awgSyncPeers()`
   асинхронно после успешной операции. `PeerSyncInfo.OverlayCIDR string`
   → `AllowedCIDRs []string`. Commit `570fd1a`.

✅ **#9 (частично) — Tunnel routing вынос** — `tunToPeerLoop` запускается
   ТОЛЬКО если `awgDevice == nil`. В `peerToTunLoop` plaintext
   `tun.Write` гейтнут `awgDevice == nil`. То есть в camp+AWG режиме
   Device единолично владеет utun. `routeFor` и `interceptPeerForLocked`
   физически ещё в коде (для static --peer fallback), но не вызываются
   когда AWG активен.

⏳ **#10 Hardening: drop plaintext IPv4/IPv6 на UDP-сокете** —
   `peerToTunLoop` принимает **только** envelope'ы (H1..H8 диапазоны).
   Любой IPv4/IPv6 пакет, пришедший без AWG-обёртки — drop. Закрывает
   спуфинг-вектор "пиши IP-пакет на наш UDP endpoint, попадай в утун
   без аутентификации". Сейчас в `peerToTunLoop` всё ещё есть ветка
   `version == 4 || 6 → tun.Write` гейтнутая `awgDevice == nil` —
   она работает только в static-mode и при отключенном AWG. Полное
   удаление этой ветки — отдельный шаг.

   Опциональный временный флаг `AllowLegacyPlaintext` в camp config
   возвращает старое поведение на время раската; убирается из кода
   после полного перехода всех участников на новые версии.

После #7 шифрование заработало. После #8 intercept-egress снова рабочий
(был broken между #7 и #8 — AWG'шный allowedips trie не знал prefix'ов).
После #10 plaintext запрещён полностью.

> **Шаги "Pinger move into tunnel" и "Tunnel routing вынос"** из первой
> редакции плана **слились с #6**: pair_req/pair_res поглотили RTT
> (commit `7757470` удалил `engine/peerping/`), а Device забрал utun
> в собственное владение в момент `awg.Start` (commit `0903b5f`).

### Критический фикс пост-внедрения

**Bind reopen bug** (commit `93d21fc`). `conn.Bind` ОТКРЫВАЕТСЯ И
ЗАКРЫВАЕТСЯ amneziawg-go несколько раз во время `IpcSet` +
`device.Up()` (transitions через listen_port + state changes).
Изначальная имплементация создавала `closed` channel **один раз** в
`New()`, а `Close()` закрывал его навсегда. После первого Close любой
последующий Open возвращал `ReceiveFunc` который немедленно отдавал
`net.ErrClosed` — receive-goroutine моментально завершалась, и AWG
Device фактически не принимал входящие пакеты вообще.

Симптом: handshake initiation шёл, в ответ peer слал handshake
response, наш engine.peerToTunLoop его получал и классифицировал
(`rx udp 92 bytes ... magic=h2 slot=1`), `bind.Deliver` помещал в
inbox, но ReceiveFunc стоял мёртвым — пакет никогда не достигал
Device. AWG handshake retry'ил вечно, data plane не работал ни для
одной пары peer'ов.

Лечится созданием fresh `b.closed = make(chan struct{})` на каждом
Open. Open поддерживает Close→Open циклы корректно.

Уроки:
- **conn.Bind** в amneziawg-go это не "открыл один раз и забыл" — он
  жизнеcycle'ит с device. Все ресурсы Bind должны переживать
  Close→Open циклы.
- **F2F_AWG_DEBUG=1** + наш discriminator-лог быстро локализовали
  бы это сразу — было видно что AWG-magic packets классифицируются
  и Deliver'ятся, но Device ничего не "Received". Логи спасли.

### Критический фикс №2: Race в SyncPeers IpcSet (commit `e5bea28`)

**Симптом**: после внедрения incremental UAPI (`33f4cd1`) HTTP-запросы
через overlay (`ca-poll`, `domains-poll`, и т.д.) начали timeout'иться
у ВСЕХ пиров одновременно. При этом:
- pair-handshake (наш control plane через H5 envelope) работал — UI
  показывал peer'ов зелёными
- AWG handshake завершался — в логах `awg: peer(X) - Received
  handshake response`, `Receiving keepalive packet`
- НО реальный TCP-трафик через overlay не доходил

То есть control plane живой, AWG-keepalive'ы летят, но data
plane мёртв. Странное расхождение.

**Корень проблемы** — гонка между **concurrent goroutine'ами**,
запускающими `awgSyncPeers`.

#### Почему вообще несколько goroutine'ов?

`awgSyncPeers` запускается **через `go ...` (fire-and-forget)** из
пяти точек:

```
handlePairReq    → on firstReq=true  → go awgSyncPeers
handlePairRes    → on firstRes=true  → go awgSyncPeers
applyPeerList    → каждый camp poll  → go awgSyncPeers
AddIntercept     → после успеха      → go awgSyncPeers
RemoveIntercept  → после успеха      → go awgSyncPeers
```

Зачем `go`? Чтобы **не блокировать** место вызова — handlePairReq
должен быстро ответить pair_res'ом, applyPeerList не должен блочить
camp poll. Все они на горячих путях.

Но эти goroutine'ы между собой **никак не координируются**. При
первом контакте с новым пиром три триггера летят в окне ~100ms:
сначала applyPeerList сообщает что peer есть → потом приходит
pair_req → потом pair_res. Три параллельных `awgSyncPeers`. Каждый
строит свой snapshot и идёт в `dev.SyncPeers` → внутри в `IpcSet`.

#### Что было в первой версии incremental кода

```go
func (d *Device) SyncPeers(peers []PeerSyncInfo) error {
    normalized := NormalizePeers(peers)
    d.mu.Lock()
    blob := BuildIncrementalBlock(d.lastPeers, normalized, ...)
    if blob == "" {
        d.mu.Unlock()
        return nil
    }
    d.lastPeers = normalized   // ← обновили БЕЗ IpcSet'а
    d.mu.Unlock()              // ← отпустили lock ДО IpcSet'а
    return d.dev.IpcSet(blob)  // ← IpcSet runs unsynchronized
}
```

Lock защищает только diff и обновление `lastPeers`, но **не сам
IpcSet**. После Unlock следующий вызывающий тут же лезет в lock,
видит обновлённый `lastPeers`, считает свой diff против него,
обновляет дальше, выходит, идёт в IpcSet.

В итоге **N IpcSet'ов** запускаются параллельно, **порядок их
выполнения не определён**.

#### Что ломается из-за неупорядоченности

Концретный сценарий с двумя goroutine'ами A и B при первом контакте:

```
T=0      A (firstReq):    diff против prev=nil → CREATE X с E1
                          обновил lastPeers=[X с E1], отпустил lock
                          ВНЕ lock'а: вызывает IpcSet(CREATE X с E1)

T=0.001  B (firstRes):    в это время X уже в lastPeers, но не в
                          Device. B видит lastPeers=[X с E1], считает
                          diff против curr=[X с E2 — pair_res пришёл
                          с другого endpoint'а] → update_only+endpoint=E2
                          обновил lastPeers=[X с E2], отпустил lock
                          ВНЕ lock'а: вызывает IpcSet(update_only X E2)

T=0.002  Какой из IpcSet'ов выполнится первым в Device?
```

Если первым отработает A's IpcSet — peer X создан с E1. Потом B's
IpcSet: `public_key=X` найдёт существующего → `update_only=true` no-op
→ `endpoint=E2` обновит. **Финал: X с E2 ✓**.

Если первым отработает B's IpcSet (это валидно — порядок неопределён):
- `public_key=X` → LookupPeer(X) **не найдёт** (A ещё не создал) →
  Device автоматом NewPeer(X) → `peer.created=true`
- `update_only=true` → раз `peer.created=true`, Device **отменяет**
  только что созданного → RemovePeer + dummy
- `endpoint=E2` → применяется к dummy → **no-op**

Потом A's IpcSet: создаёт X с E1. **Финал: X с E1 ✗**. E2 потеряно.

**В нашем `lastPeers` записано E2, в Device по факту E1.** Они
рассинхронизировались. AWG продолжает слать пакеты на E1 (старый
endpoint после NAT-rebind). HTTP-пакеты идут "в никуда" → timeout.

#### Почему control plane не пострадал

Pair-handshake идёт через `e.udp.WriteToUDP(packet, peer.UDPAddr)` —
**мы сами** управляем endpoint'ом каждого пакета. peer.UDPAddr в
`peerState` мы апдейтим в handlePairReq/Res, и оно правильное.

AWG же шлёт на endpoint **который ему запушили через UAPI** — а там
из-за race застряло E1. Поэтому pair шёл, AWG handshake шёл (потому
что mac-mini посылал нам, мы отвечали, всё бидиректное), но data
plane (наш HTTP через утун → AWG → wire) умирал.

Плюс был сопутствующий баг: `d.lastPeers = normalized` **до** вызова
IpcSet. Если IpcSet возвращал ошибку, состояние Device и наша
память расходятся ещё сильнее, retry не сработает (следующий diff
покажет что всё ок).

#### Фикс

Lock держится **на всё время** SyncPeers, включая IpcSet:

```go
func (d *Device) SyncPeers(peers []PeerSyncInfo) error {
    normalized := NormalizePeers(peers)
    d.mu.Lock()
    defer d.mu.Unlock()
    blob := BuildIncrementalBlock(d.lastPeers, normalized, ...)
    if blob == "" {
        return nil
    }
    if err := d.dev.IpcSet(blob); err != nil {
        return err  // не обновляем lastPeers — следующий sync ретрайнет
    }
    d.lastPeers = normalized
    return nil
}
```

Эффекты:
- IpcSet'ы **строго сериализованы** через `d.mu`. Конкурентные
  goroutine'ы выстраиваются в очередь.
- `lastPeers` обновляется **только после успеха IpcSet** — память и
  Device консистентны.
- На ошибке IpcSet `lastPeers` не меняется, следующий sync вычислит
  тот же diff и попробует снова.

#### Лучшая архитектура на будущее (channel-coalescing)

Lock работает, но плодит лишние IpcSet'ы — если 3 события прилетели
в окне 100ms, делаются 3 sync'а (вторые два, скорее всего, no-op
если первый уже отработал). Идеально — **один выделенный goroutine**
+ канал coalescing:

```go
type Engine struct {
    ...
    awgSyncCh chan struct{}  // buffered=1
}

func (e *Engine) requestAwgSync() {
    select {
    case e.awgSyncCh <- struct{}{}:
    default: // уже есть pending — пропускаем, текущий обработает свежее состояние
    }
}

// В Engine.Start:
go e.awgSyncWorker(ctx)

func (e *Engine) awgSyncWorker(ctx) {
    for {
        select {
        case <-ctx.Done(): return
        case <-e.awgSyncCh:
            e.awgSyncPeers()  // строит свежий snapshot из e.peers, синхронно
        }
    }
}
```

Триггеры (`handlePairReq`/`handlePairRes`/`applyPeerList`/intercept ops)
вместо `go e.awgSyncPeers()` вызывают `e.requestAwgSync()`. Если 5
триггеров случились за миллисекунду — в канале лежит максимум **один**
запрос, worker'ом выполнится один sync, который увидит финальное
состояние.

Это в backlog'е как ещё одна оптимизация, не critical.

#### Уроки

- **Fire-and-forget `go` goroutine'ы на shared state — почти всегда
  bug**. Если несколько мест запускают `go workerThatMutatesState()`,
  нужно явно сериализовать через lock или single-goroutine pattern.
- **Lock должен покрывать всё что должно быть атомарным**, не только
  чтение и подготовку. В нашем случае IpcSet — изменяющая Device
  операция — должен был быть под lock'ом.
- **Если состояние нашей памяти ≠ состоянию внешней системы (Device)
  — это потенциальный disaster** при возобновлении операций. Update
  lastPeers ТОЛЬКО после подтверждения что Device применил блоб.
- **Unit-тесты не поймали race** потому что они однопоточные — diff
  для последовательных snapshot'ов проверялся, но не concurrent IpcSet
  вызовы. Hard to test race conditions; нужны явные stress-тесты с
  goroutine'ами или fuzz.
- **Симптом был тонкий**: control plane живой, AWG handshake'и идут,
  keepalive'ы летят, но HTTP не работает — потому что данные шли
  на STARED endpoint, который не доходит до перерегистрированного
  пира. Без понимания semantic'и `update_only=true` в WG UAPI вообще
  не было бы зацепки.

### Открытые вопросы

1. **Friend's Windows side**: пишет UDP-обёртку с нуля или использует
   `amneziawg-windows` (нативный)? Если первое — можно отойти от
   wg-совместимого формата, ослабив зависимость от amneziawg-go.
   Если второе — wire-формат фиксирован их кодом, нужно match'ить
   byte-in-byte.

2. **Парс параметров обфускации в invite-токене**. Сейчас invite —
   это `{camp_id, invite_id, expires_at}` + подпись. Если включаем
   `Jc/H1..H4`/`I1..I5` в токен — новый участник сразу знает профиль
   и не выкачивает его отдельным запросом. Минус: токен раздувается
   (`I1..I5` могут быть длинными строками).

3. **Per-peer профили обфускации vs camp-wide**. Если каждая пара
   имеет свой профиль — DPI сложнее обучить, но **state на квадрат**
   количества пиров. Camp-wide — дешевле, но единый fingerprint.

4. **AWG обновления**. `amneziawg-go` развивается (добавляют новые
   UAPI ключи, новые маскировки). Стратегия отслеживания: pin на
   конкретную ревизию в `go.mod` + периодический ребейс с ручной
   проверкой совместимости wire-формата.

5. ~~Существующие intercept'ы при первом запуске AWG.~~ **Закрыто
   в commit `570fd1a`**: `restoreInterceptsFromCamp` восстанавливает
   их в `e.intercepts` на Start, потом `awgSyncPeers` на первом camp
   poll'е (через 30с после старта) пушит их в UAPI. Окно ~30с после
   старта когда intercepts ещё не активны в AWG — fixed by triggering
   awgSyncPeers from camp poll's applyPeerList.

6. **Ротация X25519 keypair'а вслед за Ed25519**. Если ed25519
   ротируется (пользователь удалил `/var/lib/f2f/identity/<camp_id>/`
   и перерегистрировался) — x25519 derive'нется новый. Это
   **поведение по умолчанию**: ротация identity = ротация
   transport keypair. Это нужно явно зафиксировать в дизайне и
   в UI (предупреждение при сбросе identity).

7. **Обфускация camp announce**. Сейчас в плане camp ↔ engine остаётся
   plaintext JSON. Camp endpoint (`f2f-camp.fly.dev:3478`) и так
   публично известен — скрывать факт «обращения к camp'у» бесполезно.
   Но содержимое announce'а (имя, pub, camp_id) — лишний signal для
   DPI. `camp_key` для envelope camp может вывести из `camp_id` сам
   (~10 строк TS). **Решение когда делать**: после стабилизации p2p
   шифрования, отдельным деплоем — это редактирование camp'а, которое
   мы отложили.

8. **`camp_key` rotation при компрометации**. `camp_key` derive'ится из
   `camp_id`. Если camp_id утёк (invite перехвачен) — атакующий может
   расшифровывать наш control-envelope (видеть pair_req/pair_res, но
   не подделывать — подпись держит). Ротация требует смены camp_id, что
   = новый camp + миграция всех участников. Возможный компромисс:
   добавить `camp_key_epoch` в config; engine derive'ит
   `camp_key = HKDF(camp_id, "f2f-control-v1", epoch)`, ротация
   `epoch` без смены `camp_id`. Сложность: распространение нового
   epoch'а (через invite-token? через signed control packet?).

9. **Timing шага #10 (drop plaintext)**. Hardening нельзя включить
   преждевременно — пока AWG не подключён (шаги #4-#7), plaintext —
   единственный способ передавать data-traffic между обновлёнными
   пирами. Правильный порядок:
   - #4-#7 AWG, шифр включён → новые пары больше не нуждаются в
     plaintext.
   - #8-#9 routing вынос → engine больше не пишет plaintext в утун.
   - **Только потом** #10 hardening: drop plaintext на recv.

   Если #10 случится раньше — обновлённые ноды перестанут общаться
   между собой до того, как AWG возьмёт на себя транспорт. Open
   вопрос: добавить ли guard в коде (отказ включать hardening пока
   AWG не активен на peer'е) или полагаться на ручную последовательность
   деплоя.

### Backlog после внедрения AWG (приоритизированный)

Шаги #1-#9 закрыты и работают в production'е. То что ниже — известные
неоптимальности и фичи второй очереди, обнаруженные при доводке.

**🟡 Replace_peers thrash**. `awgSyncPeers` использует
`replace_peers=true` для атомарной замены peer-set'а в UAPI. Каждый
вызов рвёт все живые WG-сессии, даже если состав peer'ов не менялся.
applyPeerList дёргает SyncPeers на каждом camp poll'е (раз в 30с) →
сессии переустанавливаются каждые 30с. Handshake re-completes за
~25ms на LAN, TCP retransmits это переживают, но:
- расход CPU на handshake'и
- лишние пакеты на проводе
- HTTP-запросы попадающие точно в момент replace_peers могут проседать

Два пути починки:
- **A) Skip-if-unchanged**: считать hash от serialised peer-list-blob'а,
  ранее-выйти если совпадает с last-pushed. Закрывает 90% thrash'а.
  ~10-15 строк кода (поле `lastSyncedHash` в Engine, FNV-64 от блоба).
- **B) Incremental UAPI updates**: вместо `replace_peers=true` —
  per-peer `public_key=... \n update_only=true \n endpoint=...` /
  `remove=true`. Не рушит сессии на routine updates. Сложнее,
  требует diff'а.

Сначала (A) — простой и закроет большую часть проблемы.

**🟡 Polling-loops к unreachable пирам**. `ca-poll`, `domainPollLoop`,
`filesPollLoop`, `peerFirewallPollLoop`, `callPollLoop` пытаются HTTP
peer'у через overlay-IP. Если peer не paired (`IsPaired() == false`)
— AWG'шный allowed_ips trie его не знает, пакеты дропаются. HTTP
получает `context deadline exceeded`, спам в логах. Лечится: гейтить
все эти loops по `peer.IsPaired()`, skip'ить unreachable.

**🟡 Полная DPI-обфускация**. Сейчас мы выставляем только `h1..h4`
(magic headers). У AmneziaWG есть ещё:
- `Jc/Jmin/Jmax` — junk-пакеты перед handshake
- `S1..S4` — random padding handshake/transport-пакетов
- `I1..I5` — custom signature packets (HTTP-mimicry DSL)

Без них AWG-handshake **по размерам** ванильный WG. DPI обученный
против WG может зацепиться за фиксированные 148/92-байтовые init/response.
Лечится: derive Jc/Jmin/Jmax/S1..S4 из camp_id в obfenv, передавать
в `BuildSelfConfig`. `I1..I5` (более тяжёлая фича, DSL для имитации
HTTP/STUN/etc) — отдельной итерацией.

**🟢 #10 Hardening — drop plaintext IPv4/IPv6 на recv**. Описан в Плане
выше. После #10 единственное что принимается на UDP-сокете —
envelope'ы (`H1..H8`). Закрывает spoof-инжекшн в utun.

**🟢 Camp announce обфускация**. Сейчас camp ↔ engine в plaintext JSON.
Camp endpoint и так публично известен, но содержимое announce'а
(имя, pub, camp_id) — лишний DPI-signal. `camp_key` для envelope camp
может вывести из `camp_id` сам (~10 строк TS на camp-стороне).

**🟢 Чистка debug-логов**. `F2F_AWG_DEBUG=1` логи + `awg: h1..h4
slot ... configured magic=N` startup-снапшот — оставлены пока
устаканивается. Когда AWG-интеграция стабилизируется (нет регрессий
на двух-трёх рестартах) — убрать или понизить verbosity.

**🟢 Tests для Engine-уровня AWG flow**. Сейчас есть unit-тесты для
`engine/awg/`, `engine/obfenv/`, `engine/pair/` по отдельности. End-to-end
теста "Start engine A + Start engine B → они пайrятся → HTTPS работает"
нет. Это требовало бы in-process Engine pair'а на ephemeral портах —
делается, но не приоритет.

**🟢 Sticky peer state survives Stop**. При `Stop`/`Start` reset
AWG-сессии — нормально. Но `LastValidReqMs/LastValidResMs/LastRTTMs`
сбрасываются тоже. На рестарте UI на ~30с показывает все peer'ы как
🔴 unreachable пока не накопятся новые pair-handshake'и. Не баг, но
UX-неприятно. Можно snapshot'ить timestamps в `<camp_id>/config.json`,
загружать при Start. Не критично.

### Взаимодействие с другими TODO

- **Identity Phase 1** (подпись announce-пакетов ed25519-ключом —
  см. секцию "Firewall default-deny на утане, не на peer'е" выше):
  параллельная работа, не блокирует. Ed25519 продолжает использоваться
  для подписи, X25519 — для transport encrypt. Один источник
  истины (seed), две производные.
- **QUIC миграция** (выше): QUIC начнёт ходить **поверх**
  зашифрованного AWG-туннеля (через overlay tunnel IP). Это
  упрощает QUIC TLS — транспорт уже аутентифицирован X25519
  pubkey'ом peer'а, поверх можно использовать self-signed без
  предварительного PKI. Имеет смысл сделать AWG до QUIC.
- **OIDC/SSO**: identity для OIDC = тот же ed25519 pub. X25519 —
  внутренний transport-only ключ, не светится в OIDC-токенах.

---

## TODO: универсальное десктоп-приложение

### Проблема

Текущий UI — jQuery + vanilla JS, встроен в Go-бинарь (`web/assets/`).
Обновления данных перерисовывают целые секции через `innerHTML`, что:
- Убивает scroll position, selection state, focus.
- Мерцание при каждом poll-тике (3-10с).
- Невозможно сделать плавные анимации и transitions.
- Два отдельных UI: `source/mac/` (встроенный) и `source/desktop/` (Wails).
  Фичи дублируются или отстают друг от друга.

### Целевая архитектура

```
┌──────────────────────────────────┐
│     Desktop App (универсальный)  │
│  Wails / Tauri / Electron        │
│  ┌────────────────────────────┐  │
│  │   Frontend (Svelte/Vue)    │  │
│  │   реактивный, один на все  │  │
│  │   платформы                │  │
│  └─────────┬──────────────────┘  │
│            │ WebSocket / IPC     │
│  ┌─────────▼──────────────────┐  │
│  │   Backend (Go)             │  │
│  │   engine + net + app       │  │
│  └────────────────────────────┘  │
└──────────────────────────────────┘
```

- **Один UI** для Mac и Windows (и Linux в будущем).
- **Реактивный фреймворк** (Svelte, Vue или React) вместо jQuery.
  Virtual DOM / fine-grained reactivity — обновляется только то что
  изменилось, без перерисовки всей страницы.
- **WebSocket / IPC** вместо HTTP polling для обновлений UI.
  Backend пушит изменения → UI мгновенно реагирует.
- **Браузерный fallback** — тот же фронтенд доступен как SPA на
  `localhost:2202` для тех кто не хочет десктоп-приложение.

### Текущие проблемы UI

| Проблема | Причина | Решение |
|----------|---------|---------|
| Мерцание при обновлении | `innerHTML` перерисовка | Реактивный фреймворк |
| Потеря scroll/focus | DOM пересоздаётся | Virtual DOM / fine-grained updates |
| Два кодобазы UI | mac embed + desktop Wails | Один универсальный фронтенд |
| HTTP polling для UI | `setInterval` + `$.getJSON` | WebSocket push от backend |
| Нет оффлайн-состояния | Каждый poll может упасть | Reactive store + reconnect |

### План миграции

**Фаза U1 — Выбор стека**
Определиться: Wails (уже есть в `source/desktop/`, Go-native)
vs Tauri (Rust, более легковесный) vs Electron (тяжёлый, но экосистема).
Фреймворк: Svelte (простой, быстрый) vs Vue vs React.

**Фаза U2 — WebSocket API**
Добавить WebSocket endpoint на loopback сервер. Backend пушит обновления
(status, peers, domains, calls, files) через WS. Фронтенд подписывается
на нужные каналы. HTTP API остаётся для мутаций (POST/PUT/DELETE).

**Фаза U3 — Новый фронтенд**
Переписать UI на выбранном фреймворке. Один компонент на таб
(camp, tunnel, dns, meet, meet2, drop). Reactive store для состояния.

**Фаза U4 — Обёртка в десктоп-приложение**
Wails/Tauri build → нативный `.app` (Mac) и `.exe` (Windows).
Системный трей, автозапуск, нотификации.

**Фаза U5 — Убрать старый UI**
Удалить `web/assets/`, jQuery, inline HTML. `web/server.go` обслуживает
только API + WebSocket + статику нового фронтенда.

### Privilege elevation: UI (user) → engine (root)

Engine требует root (utun, pf, routes). Desktop-app запускается от
обычного пользователя. Решение: app при старте поднимает engine через
системный диалог с паролем, затем общается по localhost.

```
Desktop App (user)
    │
    ├─ запускает engine с elevation (системный диалог с паролем)
    │
    └─ общается с engine по localhost:2202 (API + WebSocket)

Engine (root)
    └─ utun, pf, routes, UDP hole-punch, QUIC, SFU
```

**По платформам:**

| ОС | Механизм | Диалог | Подпись нужна? |
|----|----------|--------|----------------|
| **macOS** | `osascript -e 'do shell script "..." with administrator privileges'` | Стандартный macOS диалог с паролем | Нет |
| **macOS** (polish) | `SMAppService` — helper в launchd, пароль один раз при установке | Touch ID / пароль | Да (Apple Developer) |
| **Linux** | `pkexec /usr/local/bin/f2f` (PolicyKit) | Системный диалог с паролем | Нет |
| **Windows** | `ShellExecute` с verb `"runas"` → UAC prompt | "Разрешить изменения?" | Нет (но без подписи — жёлтый warning) |

Во всех ОС один паттерн: desktop app (user) → системный диалог →
engine (root) → общение по localhost. Платформо-специфичная только
строчка запуска.

Для начала — `osascript` на macOS (просто, без подписи, пароль при
каждом запуске). `SMAppService` (пароль один раз) — позже, когда
будет Apple Developer account.

### Открытые вопросы

- **Wails vs Tauri**: Wails уже есть (`source/desktop/`), Go-native.
  Tauri легковеснее (Rust), но второй язык в проекте.
- **Meet WebRTC**: Wails/Tauri используют системный WebView →
  `getUserMedia` доступен, отдельный libwebrtc не нужен.

---

## TODO: инженерные улучшения

### 1. Structured logging (высокий приоритет)

**Проблема**: `log.Printf` с "WARN:" в тексте, 156+ мест. Нет уровней,
нет фильтрации, нет structured fields. Терминал — стена текста.

**Решение**:
- Leveled logger: Error, Warn, Info, Debug.
- Structured fields: `log.Info("peer connected", "ip", ip, "name", name)`.
- Флаг `--verbose` / `--quiet` (или `LOG_LEVEL` env var).
- Группировка: "Starting DNS..." → "DNS ready (50ms)" вместо 5 отдельных строк.
- Библиотека: `log/slog` (stdlib Go 1.21+) — zero-dependency, structured, leveled.

**Частично сделано**: per-packet логи (`[utun]`/`[udp]` на каждый пакет)
вынесены за флаг `F2F_PACKET_LOG=1` (по умолчанию off) — они топили SFU-логи
и всё остальное. См. `packetLog()` в engine.go. Это первый шаг к управляемой
verbosity; полноценный leveled logger ещё впереди.

### 2. Тесты (высокий приоритет)

**Проблема**: покрытие ~5%. Тестируются только `route`, `overlay`, `packet`.
Нет тестов на engine lifecycle, config persistence, intercepts, DNS, SFU.

**Решение**:
- Engine Start/Stop lifecycle с mock'ами подсистем.
- Config load/save round-trip тесты.
- Intercept matching (domain → peer → route).
- SFU signaling: offer → answer → renegotiate cycle.
- Integration test skeleton (может потребовать Docker/VM для utun/pf).

### 3. Externalize config (средний приоритет)

**Проблема**: hardcoded значения разбросаны по коду.

| Что | Где | Значение |
|-----|-----|----------|
| DNS порт | `dns/server.go` | `5354` |
| Peer online window | `engine.go` | `30000ms` |
| Keepalive interval | `engine.go` | `25000ms` |
| Hole-punch burst | `engine.go` | `1000ms` |
| Domain poll | `engine.go` | `10s` |
| Files poll | `engine.go` | `60s` |
| Firewall poll | `engine.go` | `30s` |
| CA poll | `engine.go` | `30s` |
| Call poll | `call.go` | `3s` |
| Builtin firewall ports | `engine.go` | `2202,80,443,6881` |
| State dir | `engine.go` | `/var/lib/f2f/` |

**Решение**: `~/.f2f/settings.toml` с override'ами. Env var fallback.
CLI флаги для основных (`--dns-port`, `--bind`, `--log-level`).

### 4. CLI структура (частично сделано)

**Сделано**: управление camp'ами вынесено в подкоманды (пакет `cli`):
```
f2f [--bind addr]              # picker camp'а + UI (default)
f2f up [--bind addr]           # headless: последний camp без picker'а
f2f camp ls|new|join|use|rm
```

**Осталось** — диагностические подкоманды без браузера:
```
f2f health                     # жив ли engine + peers
f2f status                     # JSON snapshot (Status struct)
f2f config show|set KEY=VALUE  # параметры
```

### 5. Observability (средний приоритет)

**Проблема**: нет pprof, нет метрик. Утечки горутин и памяти
диагностируются только вручную.

**Решение**:
- `--pprof` флаг → `/debug/pprof` на loopback (не на tunnel!).
- Optional Prometheus `/metrics`: peers_online, dns_queries_total,
  tunnel_tx_bytes, tunnel_rx_bytes, sfu_active_tracks, sfu_participants.
- В `Diagnostics`: добавить memory usage, open FDs, goroutine count.

### 6. Worker / poller boilerplate (низкий приоритет)

**Проблема**: паттерн повторяется 15+ раз:
```go
e.workers.Add(1)
go func() {
    defer e.workers.Done()
    ticker := time.NewTicker(interval)
    defer ticker.Stop()
    for {
        select {
        case <-ctx.Done(): return
        case <-ticker.C:
        }
        doWork()
    }
}()
```

7 polling loops с идентичной структурой.

**Решение**: helper:
```go
func (e *Engine) runPoller(ctx context.Context, name string, interval time.Duration, fn func(context.Context)) {
    e.workers.Add(1)
    go func() {
        defer e.workers.Done()
        ticker := time.NewTicker(interval)
        defer ticker.Stop()
        for {
            select {
            case <-ctx.Done(): return
            case <-ticker.C:
            }
            fn(ctx)
        }
    }()
}

// Использование:
e.runPoller(ctx, "domains", 10*time.Second, e.pollAllPeerDomains)
e.runPoller(ctx, "files", 60*time.Second, e.pollAllPeerFiles)
e.runPoller(ctx, "firewall", 30*time.Second, e.pollAllPeerFirewall)
```

### Приоритеты

| # | Что | Усилие | Влияние | Когда |
|---|-----|--------|---------|-------|
| 1 | Structured logging (`slog`) | Низкое | Высокое | Перед рефакторингом |
| 2 | Тесты engine lifecycle | Среднее | Высокое | Перед рефакторингом |
| 3 | Externalize config | Среднее | Среднее | Во время рефакторинга |
| 4 | CLI subcommands | Низкое | Низкое | После десктоп-приложения |
| 5 | pprof + metrics | Низкое | Среднее | При необходимости |
| 6 | Worker helper | Низкое | Низкое | Во время рефакторинга |

---

## TODO: система уведомлений

### Зачем

Сейчас пользователь узнаёт о событиях только если смотрит в UI.
Нет способа понять что произошло пока тебя не было: кто появился
в сети, кто создал звонок, кто расшарил файл.

### События для уведомлений

| Событие | Источник | Приоритет |
|---------|----------|-----------|
| Пир появился в сети | `applyPeerList` (peer online) | Низкий |
| Пир ушёл из сети | `applyPeerList` (peer offline) | Низкий |
| Создан групповой звонок | `callPollLoop` (новый remote call) | Высокий |
| Пир присоединился к звонку | SFU `AddParticipant` | Средний |
| Пир покинул звонок | SFU `RemoveParticipant` | Средний |
| Новый файл в camp library | `filesPollLoop` (новый файл у пира) | Средний |
| Файл расшарен конкретно мне | Нужен новый механизм (targeted share) | Высокий |
| Загрузка файла завершена | torrent download complete | Средний |
| Пир опубликовал новый домен | `domainPollLoop` (новый домен) | Низкий |

### Архитектура

```
Engine events → NotificationService → хранилище + доставка
                                        │
                                        ├─ UI list (история уведомлений)
                                        ├─ push → браузер/desktop app (SSE / IPC)
                                        └─ OS notifications (macOS/Win/Linux)
```

- **NotificationService** (`app/notifications/`) — принимает события от
  модулей, дедуплицирует, хранит историю, рассылает подписчикам.
- **Хранилище** — `~/.f2f/notifications.json` или SQLite. Максимум
  N записей (ring buffer). Фильтрация по типу, пиру, времени.
- **UI** — список уведомлений с badge-счётчиком непрочитанных.
  При клике — переход к контексту (звонок, файл, пир).
- **OS-нотификации** — macOS: `NSUserNotification` / `UNUserNotification`.
  Показывать для высокоприоритетных событий (звонок, targeted file share).
- **Targeted share** — расширение протокола: при расшаривании файла
  указать список tunnel_ip получателей. При push через QUIC — уведомление
  только этим пирам.

### Интеграция с TODO

- **QUIC**: уведомления о событиях пиров приходят через push-стримы
  (вместо обнаружения через polling).
- **Desktop app**: нативные OS-нотификации из Wails/Tauri.
- **Рефакторинг**: `app/notifications/service.go` + `api.go`
  (RegisterRoutes для UI).

---

## TODO: аутентификация, SSO и f2f как OIDC-провайдер

### Зачем

У camp'а уже десяток сервисов (gitea, grafana, vault, nextcloud,
supabase, coder...), у каждого свой логин. И сам UI f2f (`:2202`)
сейчас **без авторизации** — кто на машине, тот и рулит движком.
Идея: f2f становится **identity provider'ом всего camp'а**. Личность
уже есть (per-camp Ed25519 pubkey, verified во всём camp), фактор входа —
**passkey/Touch ID**. Один вход — доступ ко всему, без паролей на сервис.

### Контекст: транспорт сейчас не аутентифицирован

Data-plane туннеля — **голый UDP**. Source tunnel IP подделывается изнутри
camp'а, TLS у прокси server-only (клиент не аутентифицируется). То есть
сетевой слой **не удостоверяет** кто подключился. Значит личность надо
доказывать на прикладном уровне — подписью camp-ключа.

### f2f как OIDC-провайдер

Большинство сервисов (gitea, grafana, nextcloud, ...) умеют **OIDC** из
коробки. f2f поднимает стандартный OIDC-endpoint (напр. `auth.xyz.f2f`):
- Сервисы регистрируются как OIDC-клиенты, редиректят логин на f2f.
- f2f аутентифицирует пользователя (см. фазы ниже) и выдаёт ID-token.
- `sub` токена = camp pubkey; claims = имя пира, camp_id.
- Стандартный протокол → нулевая кастомная интеграция на стороне сервисов.

WebAuthn/passkey тут естественен: у f2f настоящий HTTPS (local CA) и
стабильный домен — готовый relying party.

### Мимикрия под self-hosted OIDC-провайдеры

В homelab/self-hosted сценах уже сложилась экосистема легковесных
passkey-first OIDC-провайдеров: **PocketID**, **Authelia**, **Authentik**,
Keycloak (потяжелее). Сервисы вроде Nextcloud, Gitea, Vaultwarden,
Immich часто сконфигурированы поднимать логин через эти IDP'ы.

Идея: f2f выставляет endpoint'ы в форматах **этих конкретных
провайдеров**, чтобы существующая конфигурация сервиса (`oidc_issuer:
https://pocket-id.example/...`) **просто работала** при направлении на
f2f-endpoint — без правок настроек сервиса, без изучения generic OIDC.

Что меняется в конфиге сервиса:
- Только base URL: `pocket-id.example` → `auth.xyz.f2f`
- Возможно client_id/secret (выдаются нашим OIDC при регистрации)

Что **НЕ меняется**:
- Discovery URL path, claims shape, scope-семантика — всё как у
  имитируемого провайдера.

В коде это под-профили мимикрии:

```
app/auth/profiles/
  pocketid.go    — формат PocketID (passkey-first, минималистичный OIDC)
  authelia.go    — Authelia API
  authentik.go   — Authentik API
  generic.go     — стандартный OIDC (для тех кто умеет custom provider)
```

Какой профиль активен — выбирается по virtual-host
(`pocketid.xyz.f2f`, `authelia.xyz.f2f`) или по client_id-префиксу.
Под капотом identity та же — camp Ed25519 + passkey gate.

Зачем именно self-hosted, а не Google/GitHub-style мимикрия: эти IDP'ы
**уже рассчитаны** на homelab, у них стандартизованные интерфейсы, не
завязка на конкретные имена доменов или legal-имена брендов. Мимикрия
под Google могла бы (теоретически) дать «Sign in with Google» в любом
SaaS-приложении, но это:
- сильно сложнее (Google'ский OIDC расширен specific-claims)
- упирается в legal/branding (использование чужого имени)
- наш use case — закрытая camp-сеть с self-hosted сервисами, где
  PocketID-формат закрывает 90% случаев.

Под-вопросы:
- **Какие профили приоритетны**: PocketID + Authelia + generic — этого
  должно хватить под текущие сервисы в camp.
- **Discovery & JWKS**: каждый профиль выставляет свой
  `.well-known/openid-configuration` + `jwks_uri` с **нашим**
  подписывающим ключом. Сервис фетчит discovery → видит наш JWKS → ок.
- **Token claims**: маппинг camp identity → claims конкретного
  профиля. `sub` = pub-fingerprint, `preferred_username` = peer-name,
  `email` = `<peer>@<camp>.f2f` и т.д., с поправкой на специфику
  профиля.

### Фаза 1 (сейчас, голый UDP): «Sign in with f2f» + passkey

Challenge-response, личность доказывается подписью, не сетью:
1. Сервис (OIDC-клиент) редиректит на `auth.xyz.f2f` с challenge.
2. Страница зовёт **локальный агент** пользователя (`127.0.0.1:2202/api/sign`).
3. Агент подписывает challenge Ed25519-ключом; **passkey/Touch ID гейтит подпись**.
4. `auth.xyz.f2f` проверяет подпись против известного pubkey → выдаёт OIDC-токен.
5. Cookie-сессия, чтобы не тачить на каждый запрос.

Подделать нельзя — нужен приватный ключ, а не IP.

### Фаза 2 (после QUIC): zero-click

Когда транспорт станет QUIC (встроенный TLS, каждый пир аутентифицирован),
source identity криптографически надёжна → прокси инжектит
`X-F2F-Peer` на бэкенд → **вход без кликов**. Заодно закрывается дыра
голого UDP (спуфинг/прослушка не-TLS трафика). SSO и QUIC — синергия.

### Защита самого f2f-приложения

UI/API f2f (`:2202`, управление движком) тоже за passkey:
- Открыл UI → Touch ID → доступ к Start/Stop, настройкам, intercepts.
- Критично для desktop-app (см. privilege elevation): UI запускает root-движок,
  нельзя чтобы любой с доступом к машине молча им рулил.

### Per-service ACL (ложится сверху)

Раз f2f знает кто вошёл (pubkey в токене) — авторизация по личности,
а не по портам: «gitea всем в camp, vault только мне». Прокси проверяет
claim перед форвардом.

### Группы и политики доступа

Поверх identity-by-pubkey строится **policy layer** для управления
доступом ко всему ресурсу camp'а:
- **services** (HTTPS-домены через наш прокси)
- **ports** (open ports на peer'ах через firewall)
- **tunneling** (кто может egress'ить через кого через intercept'ы)
- **DNS** (кому какие записи в camp-зоне видны)

Базовые сущности:
- **Группа** — именованный набор pub-фингерпринтов (`@admins`, `@devs`,
  `@guests`). Один peer может быть в нескольких группах. Группы
  определены на уровне camp (хранятся в camp config или
  распространяются signed-сообщениями между peer'ами).
- **Политика** — декларативное правило `<group> <verb> <resource>`:
  - `@admins allow vault.xyz.f2f`
  - `@devs allow gitea.xyz.f2f, grafana.xyz.f2f`
  - `@guests deny intercept`
  - `@everyone allow dns.read camp`

Точки проверки политик:
- **Edge proxy (`:443`)** — перед форвардом на upstream сервис
  проверяет policy: peer (из OIDC-токена) ∈ группа разрешающая этот
  service. Без матча — 403.
- **Tunnel firewall (`utun`)** — open ports peer'а гейтятся policy.
  Сейчас firewall знает только «есть/нет порт открытый для всего
  camp'а»; с группами добавляется «открытый только для @admins».
- **Intercept gateway** — peer X хочет интерсептить через Alice'у
  трафик к `8.8.8.8` — Alice проверяет, разрешено ли X использовать
  её как egress (policy `@trusted allow intercept`).
- **DNS resolver** — `xyz.f2f` зоны частично скрываются: гости не
  видят `vault.xyz.f2f` в peer-list.

Где хранится:
- Camp config содержит группы и политики. Их редактирование требует
  подписи **camp owner'а** (или делегирования через специальную
  политику `@admins allow manage.policy`).
- Распространение: при изменении — camp-сервер рассылает обновлённый
  config всем peer'ам (или peer'ы pull'ят на каждом poll'е, сейчас и
  так так делают для intercept'ов / firewall).

UI:
- Вкладка «Доступ» в helper UI: список групп, добавление/удаление
  peer'ов, редактирование политик. Только видна тем, кто в группе
  `@admins` (рекурсивная политика).

Этапы:
1. Группы как простое поле peer'а в camp config (`groups: ["@devs"]`).
2. Политики — отдельный YAML/JSON-блок в camp config.
3. Reverse-proxy читает policy, выставляет 403 на disallow.
4. Firewall расширяется per-group ACL.
5. Intercept gateway гейт по группе.

---

### Где в коде

- `app/auth/` (после рефакторинга): OIDC-провайдер, WebAuthn, challenge-response.
- Прокси (`handleProxy`): forward-auth — проверка сессии/токена перед форвардом
  (рядом с уже добавленными `X-Forwarded-*`).
- Агент-подпись: новый `POST /api/sign` на loopback, гейт через passkey.

---

## TODO: панель управления сервисами поверх Docker

### Зачем

Camp'у нужны сервисы — Nextcloud для файлов, Gitea для кода,
Vaultwarden для паролей, Immich для фотографий, Grafana для метрик,
Authentik/PocketID для legacy auth и т.д. Сейчас если ты хочешь любой
из них поднять — это **полноценная инфраструктурная работа**: пишешь
docker-compose, привязываешь к домену через nginx/Caddy, настраиваешь
OIDC, генеришь TLS, добавляешь DNS, открываешь порты в фаерволе.
Каждый сервис — отдельная история на час-два минимум.

Идея: в helper UI добавить вкладку «Сервисы» — каталог известных
self-hosted-приложений, и **по одному клику** оно поднимается с
полной интеграцией в camp.

### Как должно работать

Пользователь заходит в UI → вкладка «Сервисы» → список карточек
(Nextcloud, Gitea, Vaultwarden, Immich, Authentik, Grafana, ...).
Каждая карточка показывает: статус (запущен/нет), версию, URL внутри
camp'а, кнопки `Start` / `Stop` / `Upgrade` / `Logs` / `Remove`.

Кнопка `Install` на карточке Nextcloud за **5 секунд** делает:

1. `docker pull` нужного image'а (или из локального cache'а если уже
   лежит).
2. Генерирует уникальное имя `nextcloud-<peer>` для контейнера.
3. Резолвит свободный TCP-порт на overlay-адресе peer'а (например
   `100.X.Y.Z:8081`).
4. Стартует контейнер с шаблоном переменных:
   - `NEXTCLOUD_TRUSTED_DOMAINS=nextcloud.xyz.f2f`
   - `OIDC_PROVIDER_URL=https://auth.xyz.f2f`
   - `OIDC_CLIENT_ID=...`, `OIDC_CLIENT_SECRET=...` (автоматически
     зарегистрированы как новый OIDC-клиент в нашем `app/auth/`)
   - Tom-of-the volumes для persistence (`/var/lib/f2f/services/nextcloud/data`)
5. Регистрирует domain `nextcloud` → `<my-overlay>:<port>` в
   `MyDomains` (та же штука что user может сделать руками в UI).
6. Открывает firewall-port'у через `UserFirewallPorts` (если нужен).
7. Применяет дефолтную policy: «всем в camp'е разрешено».
8. Готово — `https://nextcloud.xyz.f2f` доступен у всех paired peer'ов
   и работает SSO через passkey.

### Каталог сервисов

Каталог = набор YAML-шаблонов, каждый описывает:

```yaml
# catalog/nextcloud.yaml
name: Nextcloud
description: Self-hosted file sync & collaboration
icon: nextcloud.svg
image: nextcloud:latest
port: 80
env:
  NEXTCLOUD_TRUSTED_DOMAINS: "{{ .Domain }}"
  OIDC_PROVIDER_URL: "https://auth.{{ .Camp }}.f2f"
  OIDC_CLIENT_ID: "{{ .OIDCClientID }}"
  OIDC_CLIENT_SECRET: "{{ .OIDCClientSecret }}"
volumes:
  - "{{ .DataDir }}/nextcloud:/var/www/html"
oidc:
  enabled: true
  profile: generic
  redirect: "/apps/user_oidc/code"
healthcheck:
  endpoint: /status.php
  expect: 200
```

Подстановки выполняет helper при создании сервиса.

Каталог хранится:
- **Builtin** (вшит в бинарь) — десяток известных сервисов, обновляется
  с релизом f2f.
- **User-defined** (в `~/.f2f/services-catalog/`) — пользователь может
  добавить свои шаблоны для не-builtin приложений.

### Что под капотом

- `app/services/`:
  - `catalog.go` — парсинг шаблонов + builtin embed
  - `docker.go` — обёртка над `docker-cli` или `containerd` API (или
    `dockerd` через unix-сокет)
  - `lifecycle.go` — start / stop / upgrade / remove, синхронизация
    с persisted state
  - `template.go` — рендеринг env/volumes из шаблона + переменных camp'а
  - `api.go` — HTTP-API для UI

- Зависимость от **Docker daemon** на хосте. Альтернатива — Podman
  (rootless preferred). На macOS — OrbStack / Docker Desktop. UI
  обнаруживает и сообщает «Docker не найден» если нет.

### Интеграции

- **OIDC**: автоматически регистрирует сервис как OIDC-клиент в
  `app/auth/`, выдаёт client_id/secret, инжектит в env.
- **Domains**: автоматически добавляет в `MyDomains`, DNS-резолвер
  отдаёт overlay-IP.
- **TLS**: наш local CA выписывает сертификат на `<service>.xyz.f2f`,
  reverse-proxy использует.
- **Firewall**: открывает порт через `UserFirewallPorts` (с правильной
  группой если policy layer уже есть).
- **Policy/Groups**: дефолтная политика «всем в camp'е» при install,
  пользователь меняет в UI.
- **Backups**: volumes лежат в `/var/lib/f2f/services/<name>/data`, BT
  client может seed'ить их (опционально) для cross-peer replication
  (см. drop-фичу).

### Открытые вопросы

1. **Multi-peer service**: один сервис на одной машине или поднимать
   везде? Для большинства (Nextcloud, Gitea) — одна инстанция на camp.
   Если несколько пиров хотят поднять — конфликт по domain. UI должен
   показать «уже поднят у Alice'ы».
2. **Persistence на peer'е**: если Alice ушла из camp'а — что с её
   Nextcloud-data? Опция: миграция (BT-репликация на оставшихся +
   handover).
3. **Resource limits**: container'у выставлять memory/CPU limits?
   Default из шаблона + override в UI.
4. **Upgrade path**: docker pull нового image'а + restart. Бекапы
   automatic? Откат при поломке?
5. **Network namespace**: сервисы должны быть видны только через
   overlay (не torch'нуть порты на public en0). Docker-сеть в bind на
   overlay-IP, либо `--network=host` + bind в коде.

---

## TODO: intercepts на домены, видимые только из сети exit-пира

### Зачем

Кейс: `work-vpn.ru` доступен пользователю **только когда он сидит в
корпоративном VPN**. Хочется, чтобы любой пир camp'а мог ходить на такой
домен через того пира, который в этом VPN — прозрачно, по настоящему
имени, с настоящим сертификатом end-to-end.

### Что уже есть (почти всё)

`services/tunnel` (intercepts) — это полноценный пакетный роутинг: пара
`Intercept{Spec, Peer}`, где Spec = CIDR / IP / **DNS-имя**.
`resolveSpec` резолвит имя в IP, ставит host-роуты (/32) на utun и через
`allowedCIDRsForPeer` добавляет эти /32 в `allowed_ips` выбранного пира.
Пакеты к этим IP идут по оверлею на пир, а пир их выпускает наружу через
`startEgress` (NAT overlay-subnet → `DefaultEgressInterface` +
ip-forwarding). Для **публичного** домена `Add("example.com", peerX)`
работает уже сейчас: клиент резолвит публичный IP, роутит через peerX,
peerX выпускает наружу своим внешним адресом.

### Гэп

1. **Резолв на неправильной стороне.** `resolveSpec` зовёт
   `net.LookupIP` на **origin-пире** (том, откуда трафик), а должен — на
   **exit-пире** (том, через кого выходит и кто сидит в work VPN).
   У origin'а `work-vpn.ru` либо не резолвится, либо даёт не тот адрес
   (приватный IP корпсети валиден только изнутри неё).
2. **Egress в правильный интерфейс.** `startEgress` NAT'ит на
   `DefaultEgressInterface` (обычно en0). Если корп-IP достижим через
   **tun самого work-VPN**, masquerade на en0 не применится → нужен NAT
   на интерфейсе, которым пир реально достаёт цель. Ключевая мысль:
   exit-пир «резолвит» не только `имя → IP`, но и `IP → исходящий
   интерфейс` — это знает его собственная таблица маршрутов (для корп-IP
   она уже указывает на utun work-VPN). То есть egress становится
   **per-target**: для каждой разрезолвленной цели пир смотрит свой route
   table, находит iface (work-VPN'овский utun) и ставит masquerade именно
   на него, а не на фиксированный default. Опционально bus-`resolve`
   может вернуть `{ips, iface}` вместе, но NAT пир настраивает локально —
   клиенту iface не нужен.

### Дизайн (минимально поверх intercepts)

1. **bus-хендлер `resolve` на каждом пире** — принимает имя, делает
   `net.LookupIP` со своей точки (внутри своего VPN), возвращает IP.
   Крошечный, как `ping`; под egress-политику (allowlist кто/какие
   имена). См. `mesh/bus`.
2. **`tunnel.go`**: для DNS-спека резолвить через
   `bus.Request(exitPeer, "resolve", name)` вместо локального
   `net.LookupIP`. Дальше всё как сейчас — вернувшиеся IP идут в
   host-роуты + `allowed_ips`. `RefreshDomainRoutes` (периодический
   re-resolve, 60с) тоже ходит через пир. Сейчас `tunnel` держит только
   `*engine.Engine` — надо прокинуть `bus` (или инжектнуть резолвер-функцию).
3. **DNS на клиенте**: intercept-домен должен у клиента отвечать **теми
   же** IP, что вернул пир (иначе приложение пойдёт не туда). Значит для
   таких имён ставим точечную запись в локальном резолвере → peer-resolved
   IP (обобщить нынешний zone-резолвер на полное имя). См. `services/dns`.
4. **egress (гэп 2)**: per-target NAT. Для разрезолвленного IP пир смотрит
   свою таблицу маршрутов → находит исходящий iface (work-VPN'овский utun)
   → ставит masquerade на него, а не на фиксированный
   `DefaultEgressInterface`. Правится отдельно от (1)–(3).

Итог: имя резолвится в корп-IP на exit-пире → этот IP роутится через
него → он выпускает трафик в work VPN. Полный TCP/UDP, настоящий
сертификат, без MITM. SNI-прокси/termination не нужны.

### Открытые вопросы

1. Конфликт DNS-ответа: если у клиента имя и так публично резолвится —
   приоритет intercept-записи. Чья запись «главнее» при нескольких
   intercept'ах одного имени на разные пиры?
2. Приватные IP-коллизии: два разных work-VPN могут оба отдавать
   `10.0.0.5`. Роуты по /32 на разные пиры конфликтуют — нужна
   диспетчеризация, недостаточно одного IP (упрётся в magic-IP диапазон,
   как quad-100 у Tailscale).
3. Политика exit-пира: кто и какие имена/подсети может выпускать
   (как `Shell`/`Vnc` policy).

---

## Что почитать дальше

- **`TODO.md`** (в корне) — список планируемых задач, с расшифровкой
  технических подходов (sleep/wake recovery, route-change reaction).
- **`source/camp/`** — TypeScript сторона. Маленькая. Если читал mac —
  поймёшь camp за час.
- **Git log** — `git log --oneline` показывает порядок добавления фич;
  по конкретному коммиту можно понять как одно решение пришло на смену
  другому.

Если что-то непонятно по конкретной функции — открой её в IDE с
go-плагином, скакни на её колл-сайты (Cmd+B / F12), почитай контекст
вокруг. Go очень explicit — там обычно нет «магии», всё рядом.
