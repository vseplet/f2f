# f2f-mac — гид по коду

Этот документ — подробное описание того, **как устроен** mac-клиент
f2f. Если ты не Go-программист и хочешь сесть и разобраться в codebase
— читай отсюда.

[GUIDE.md](GUIDE.md) — про **что делает** и **как пользоваться**; этот файл —
про **где что лежит и почему так**; [README.md](README.md) — карта всей
документации.

## Оглавление

1. [Краткий Go-ликбез для тех кто не пишет на Go](#краткий-go-ликбез)
2. [Структура папки](#структура-папки)
3. [Точка входа: main.go](#точка-входа-maingo)
4. [Слои и сервисы](#слои-и-сервисы)
5. [Сценарии: куда идёт каждый пакет](#сценарии-куда-идёт-каждый-пакет)
6. [Карта горутин](#карта-горутин)
7. [Дизайнерские решения и почему так](#дизайнерские-решения)
8. [Планы и нереализованное](#планы-и-нереализованное) → [ROADMAP.md](ROADMAP.md)

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
├── db/                        # РАСПРЕДЕЛЁННАЯ БД (над mesh, под сервисами) — см. DB.md
│   ├── frame.go                 # Frame: подписанная иммутабельная запись per-(author,scope)
│   ├── store.go                 # Store интерфейс + MemStore
│   ├── sqlitestore.go           # SQLiteStore: один файл db.sqlite per-camp (таблица frames)
│   ├── service.go               # Service: Commit/Apply/Since/Vector/Frames/Query/Dump + OnCommit/OnApply
│   ├── sync.go                  # Sync поверх mesh/bus: push + pull + db.scopes (обнаружение) + гейтинг по членству (SetMemberCheck)
│   └── blocks/                  # БЛОК-ДВИЖОК поверх db — см. BLOCKS.md
│       ├── blocks.go              # Manager: Create/Update/Move/Delete/Merge/Upsert + инкрем. свёртка Blocks(scope)
│       ├── channels/              # block.channel {name,members} в scope "channels"; general/DM; IsMember
│       └── message/               # block.message {body,file,reply,thread} в scope "message:<channelBid>"
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
│   ├── notify/                 # хаб уведомлений (in-memory ring + SSE), слушает шину
│   │   └── notify.go            # Service: Push/Recent/Subscribe, FromBus
│   ├── shell/                  # remote-terminal (mosh-подобный PTY по шине)
│   │   └── shell.go             # Service: HandleStream("shell.open") — PTY + detached-сессии (session_id) + ring-buffer reattach + kill; login/drop-to-user
│   └── vnc/                    # remote-desktop (тонкий TCP-прокси к VNC-серверу хоста)
│       └── vnc.go               # Service: HandleStream("vnc.open") — bus-стрим ⟷ localhost:5900; vnc.status (dial-тест)
│
└── ui/web/                    # HTTP UI + reverse-proxy
    ├── server.go                # роутер, statusView мерджит engine.Status + service-данные
    ├── notes.go                 # /api/notes — generic block CRUD (заметки/доки) над db/blocks
    ├── api_channels.go          # /api/channels — каналы (block.channel)
    ├── api_messages.go          # /api/messages (+/share,/clear) — сообщения; /api/db/query (SQL-консоль)
    ├── messenger_bridge.go      # мост db↔браузер: /api/events (push), OnFrameApplied, inbound-тосты
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
реагировать на свой lifecycle (`OnStarted`/`OnStopped`).

Пир-к-пир обмена по HTTP больше нет: сервисы общаются с соседями по
QUIC-шине (`mesh/bus`), адресуясь по pub. Engine отдаёт лишь снимок
online-пиров (`OnlinePeersForCAPoll`, поле `Pub`), а сам HTTP-порт
пира и `SetTunnelHTTPPort` удалены.

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
OnlinePeersForCAPoll() []OnlinePeerHTTPInfo  // снимок online peers (Pub/Name/Host) для bus-poll
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
| **dns** | DNS-сервер `127.0.0.1:0` для `<camp>.f2f` зоны, имплементит `Resolver` через `LookupHost`. Catalog: `MyDomains` (наши) + per-peer `peerDoms` (poll по шине `domains` каждые 10с). Плюс `pinned` — intercept-домены, отвечаемые IP-ами с exit-пира (+ `/etc/resolver/<domain>`). TCP health-check каждые 8с. |
| **drop** | Anacrolix BT-клиент на overlay v4 + fallback на ephemeral port если 6881 занят. `rescanSharedDir` re-seed'ит при старте. `pruneLoop` снимает с раздачи удалённые файлы. Poll списка файлов у пиров (`files` по шине) раз в минуту. Persist в `downloads.json`. |
| **firewall** | pf-anchor lifecycle (Open/Apply/Close) с recovery state файлом. Built-in порты (`2203/udp` шина, `80`/`443`, `6881`) + user CRUD из UI. Peer-firewall poll каждые 30с (`firewall` по шине) — складывает в `peerPorts[pub]`. |
| **pki** | My CA: load-or-generate per camp, install в trust store (один раз — пользователь даёт пароль). Peer CAs: каждые 30с poll `ca-cert` по шине, новые сохраняются на диск. Install в keychain — только по клику в UI. |
| **calls** | Pion SFU + group calls. Hosting peer запускает SFU `sfu.New(...)`, остальные join. Сигналинг и state по шине (`call.signal`/`call.state`/`call.join`/`call.leave`). Remote-call poll каждые 3с. |
| **tunnel** | Application-уровень routing. **Intercepts**: Add(spec, peer) — resolve spec в prefixes, routes на utun, persist в `c.Intercepts`. DNS-спеки резолвятся **на exit-пире** по шине (`resolve`), ответ пинится в dns. `RefreshDomainRoutes` каждые 60с re-resolve'ит. **Egress**: NAT для overlay subnet на default iface + per-target NAT для целей, уходящих через другой интерфейс (split-tunnel VPN). |
| **bus** | **QUIC data bus** на `overlay-IP:2203` — единый пир-к-пир транспорт (заменяет HTTP-over-tunnel). Авто-меш: пинг всех достижимых пиров раз в 30с, tie-break (младший pub дозванивается → одно соединение на пару), кэш коннектов (вход/исход переиспользуются — стримы двунаправленные). API: `Request`/`Notify`/`Handle(type, fn)`. TLS — self-signed + skip-verify: **аутентичность/шифрование уже даёт оверлей** (overlay-IP ≡ pub, WireGuard), идентичность пира = overlay-IP входящего коннекта. `Events`-хук отдаёт пинги в notify. |
| **~~messenger~~** | Снят. Чат/каналы/заметки теперь на **блоках** (`db/blocks/message`, `db/blocks/channels`) поверх `db`-субстрата (один файл `db.sqlite`, anti-entropy sync). См. [DB.md](DB.md), [BLOCKS.md](BLOCKS.md). |
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

## Миграция пир-к-пир HTTP → QUIC-шина (сделано, 2026-06)

Раньше пиры общались по **HTTP поверх туннеля**: каждый держал
HTTP-листенер на `overlay-IP:2202` (`tunnelSrv`), соседи стучались по
утану. Теперь весь пир-фейсинг переехал на **шину** (`mesh/bus`,
QUIC/2203), а HTTP-листенер и порт 2202 на оверлее **удалены**.

Почему это безопасно без своей аутентификации: оверлей (WireGuard) уже
шифрует и подтверждает источник (overlay-IP детерминирован из pub,
чужой ключ → дроп). Поэтому шина — с self-signed + skip-verify;
идентичность пира = overlay-IP коннекта (резолвится в pub через roster).

### Что переехало (каждый сервис регистрирует свой bus-тип через `Register()`)

| Bus-тип | Что делает | Сервис |
|---|---|---|
| `signal` | WebRTC p2p сигналинг (offer/answer/candidate) | ui/web |
| `call.signal` | SFU-сигналинг (group calls) | calls |
| `call.state` | опрос состояния звонка пира | calls |
| `call.join` / `call.leave` | join/leave удалённого SFU | calls |
| `domains` | опубликованные домены пира (dns-poll 10с) | dns |
| `ca-cert` | CA-серт пира (pki-poll 30с) | pki |
| `files` | список расшаренных файлов (drop-poll 60с) | drop |
| `firewall` | список открытых портов пира (30с) | firewall |
| `resolve` | резолв имени на exit-пире (intercepts) | tunnel |
| `notify` | уведомления | notify |

Каждый запрос: `busSvc.Request(pub, type, payload)` на инициаторе,
`busSvc.Handle(type, fn)` на приёмнике. Опросы (domains/ca-cert/files/
firewall/call.state) пока остаются опросами, просто по шине — следующий
шаг переделать их из poll в push (владелец `Notify`-ит при изменении).

### Что закрылось после миграции

- **`tunnelSrv` целиком** (`BindTunnel`/`UnbindTunnel`) — удалён.
- **`TunnelHTTPPort`/`SetTunnelHTTPPort`** — убраны из engine.
- **`2202/tcp` из `firewall.BuiltinRules`** — порт на оверлее закрыт
  (loopback-UI на `127.0.0.1:2202` остаётся — он не под pf оверлея).
- bus чистит коннекты к ушедшим из roster пирам (`evictStale`) и
  пингует только online-пиров.

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
| **dns** | `PollPeers` | 10с (5с warmup) | `domains` по шине у online peers → peerDoms + store catalog |
| **dns** | `HealthCheck` | 8с | TCP-dial своих portов → стампим health/checkedAt |
| **firewall** | `PollPeers` | 30с (9с warmup) | `firewall` по шине у peers → peerPorts + catalog |
| **pki** | `PollPeers` | 30с (5с warmup) | `ca-cert` по шине → discover (запись PEM на диск, install только по UI-клику) |
| **drop** | `PollPeers` | 60с (7с warmup) | `files` по шине у peers → peerFiles |
| **drop** | `rescanSharedDir`, `restoreDownloads` | разовое | seed уже лежащих файлов; resume сохранённых download'ов |
| **drop** | `chownLoop` | 10с | chown shared/downloads-каталогов на SUDO_USER |
| **drop** | `pruneLoop` | 30с | прибрать seed'ы файлов которые удалены из FS; re-feed stalled |
| **calls** | `PollPeers` | 3с | `call.state` по шине у peers → AllCalls |
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

Engine не импортирует web (нет circular dep). Но сервисам нужно знать
когда engine стартанул/остановился (поднять/снять DNS, egress, bus и
т.п.). Решение: engine выставляет два nil-функции `OnStarted/OnStopped`,
main.go их подписывает.

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
каждого peer'а в roster'е появится верифицируемый `peer_pub`, и можно
будет добавить matching по source-IP с привязкой к pubkey.

### Built-in порты — read-only, не редактируются юзером

`2203/udp` (QUIC-шина), `80/tcp`, `443/tcp`, `6881/tcp+udp` — это то,
без чего сам engine ломается (пиры не достучатся по шине, HTTPS
proxy, BT). Дать юзеру их выключить — слишком легко
«случайно» отрезать функциональность и потом не понимать что
сломалось. UI показывает их с disabled-чекбоксом + pill'ом
«built-in».

---

## Планы и нереализованное

TODO-планы вынесены в [ROADMAP.md](ROADMAP.md). Проектные документы по
крупным подсистемам — отдельно: [DB.md](DB.md) (распределённая БД),
[BLOCKS.md](BLOCKS.md) (блок-модель), [MESSAGING_DESIGN.md](MESSAGING_DESIGN.md)
(чат), [OIDC.md](OIDC.md), [INVITE.md](INVITE.md) (identity/инвайты),
[SECRETS.md](SECRETS.md). Полная карта — [README.md](README.md).


## Что почитать дальше

- **[ROADMAP.md](ROADMAP.md)** — планы и сделанное по крупным направлениям;
  проектные доки подсистем — см. [README.md](README.md).
- **`source/camp/`** — сторона camp-сервера (Go, импортирует wire-типы из
  rendezvous). Маленькая. Если читал mac — поймёшь camp за час.
- **Git log** — `git log --oneline` показывает порядок добавления фич;
  по конкретному коммиту можно понять как одно решение пришло на смену
  другому.

Если что-то непонятно по конкретной функции — открой её в IDE с
go-плагином, скакни на её колл-сайты (Cmd+B / F12), почитай контекст
вокруг. Go очень explicit — там обычно нет «магии», всё рядом.
