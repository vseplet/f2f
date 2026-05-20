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
4. [Пакет `internal/engine` — мозг всей программы](#internalengine)
5. [Пакет `internal/tunnel` — utun-интерфейс](#internaltunnel)
6. [Пакет `internal/route` — маршруты ядра macOS](#internalroute)
7. [Пакет `internal/egress` — pf NAT](#internalegress)
8. [Пакет `internal/packet` — IP-парсер](#internalpacket)
9. [Пакет `internal/rendezvous` — общение с camp](#internalrendezvous)
10. [Пакет `internal/dns` — локальный DNS-резолвер](#internaldns)
11. [Пакет `internal/web` — HTTP UI](#internalweb)
12. [Главные сущности (типы)](#главные-сущности)
13. [Сценарии: куда идёт каждый пакет](#сценарии-куда-идёт-каждый-пакет)
14. [Дизайнерские решения и почему так](#дизайнерские-решения)

---

## Краткий Go-ликбез

Если ты пишешь на JS/Python/Ruby — несколько штук, которые
встречаются в коде на каждом шагу:

**Пакеты.** Папка с `*.go`-файлами = пакет. Имя пакета указывается в
первой строке файла (`package engine`). Импортируется по пути от
корня модуля: `import "github.com/vseplet/f2f/source/mac/internal/engine"`.
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
`internal/dns` принимает `Resolver` интерфейс — кто угодно с методом
`PeerDomains() map[string][]DomainEntry` подходит. Engine этим
интерфейсом владеет, но dns его не импортирует — нет circular dep.

**`io.Reader`/`io.Writer`.** Универсальные интерфейсы потоков
байтов. Файл, сокет, HTTP-body — всё это `io.Reader`.

**Билд-теги (`//go:build darwin`).** Метка над файлом «компилировать
только под macOS». Тут весь код помечен — мы macOS-only.

---

## Структура папки

```
source/mac/
├── main.go                    # точка входа CLI
├── go.mod / go.sum            # зависимости модуля
├── README.md                  # пользовательская доку
├── ARCHITECTURE.md            # ← этот файл
└── internal/                  # внутренние пакеты, не для импорта из других модулей
    ├── engine/                # мозг: runtime тоннеля
    │   ├── engine.go           #   ~1400 строк, основной файл
    │   ├── log.go              #   broadcast-логгер
    │   └── egress_iface_darwin.go  # авто-детект default route
    ├── tunnel/                # обёртка над wireguard/tun (utun)
    ├── route/                 # вызовы /sbin/route для host-маршрутов
    ├── egress/                # pf NAT + persistent state
    ├── packet/                # парсер IPv4-заголовков
    ├── rendezvous/            # клиент camp-сервера
    │   ├── types.go            # wire-протокол
    │   ├── announce.go         # UDP announce
    │   └── peerlist.go         # HTTP-poll
    ├── dns/                   # локальный DNS на 127.0.0.1:5354
    │   ├── dns.go              # сам DNS-сервер
    │   └── resolver.go         # запись /etc/resolver/*
    └── web/                   # HTTP UI + API
        ├── server.go           # роутер + хендлеры
        └── assets/             # embed'ятся в бинарь
            ├── index.html      # SPA
            ├── app.js          # tunnel/camp/dns/intercepts
            ├── audio.js        # meet/WebRTC
            ├── audio.css       # стили
            └── vendor/         # tailwind, jquery, d3
```

**`internal/`** — особая папка в Go. Только пакеты внутри **этого
модуля** могут импортировать содержимое `internal/`. Никто извне не
утащит наш `engine` в свой проект. Это инкапсуляция уровня модуля.

---

## Точка входа: main.go

~190 строк. Разбирает CLI-аргументы, выбирает режим, создаёт `Engine`,
поднимает либо UI, либо headless `run`-цикл.

**Логика:**

```go
func main() {
    // Парсим первый позиционный аргумент.
    // По умолчанию (без аргумента) → "ui".
    // Если "run" → headless режим.
    // Если "-h"/"--help"/"help" → usage.
    ...
}
```

Две функции:

- `runCmd(args)` — голый CLI: парсит флаги (`--name`, `--id`, `--listen`,
  `--egress-iface`, и т.д.), создаёт `engine.Config`, зовёт `eng.Start(cfg)`,
  ждёт SIGINT/SIGTERM, потом `eng.Stop()`.

- `uiCmd(args)` — UI-режим: создаёт `engine.New()`, `web.New(eng, bind)`,
  настраивает хуки (`eng.OnStarted` → `srv.BindTunnel`, `eng.OnStopped` →
  `srv.UnbindTunnel`), запускает HTTP-сервер в goroutine, ждёт сигнал
  или ошибку, корректно гасит.

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

## internal/engine

**~1400 строк, главный файл проекта.** Здесь живёт runtime: utun,
UDP, маршруты, NAT, hole-punching, intercepts, peer-list, DNS-старт,
hook'и.

### Тип `Engine`

```go
type Engine struct {
    mu sync.Mutex            // защищает shared state ниже

    running bool             // флаг "поднят ли engine"
    cfg     Config           // снапшот конфига с момента Start

    tun      *tunnel.Tunnel  // утун (см. internal/tunnel)
    udp      *net.UDPConn    // UDP-сокет туннеля
    routes   *route.Manager  // ядерные маршруты (см. internal/route)
    egr      *egress.Egress  // pf NAT (см. internal/egress)
    dnsSrv   *internaldns.Server // локальный DNS
    announce *rendezvous.AnnounceClient // UDP announce → camp

    // Атомарные указатели — читаются часто, lock-free.
    campAddr   atomic.Pointer[net.UDPAddr]
    campReflex atomic.Pointer[string]
    campPeers  atomic.Pointer[[]rendezvous.PeerInfo]
    myDomains  atomic.Pointer[[]DomainEntry]

    // Карта peer-ов (см. peerState ниже), защищена mu.
    peers          map[string]*peerState  // tunnel_ip → peer
    activeTunnelIP atomic.Pointer[string]

    // Intercepts: spec → routes, защищено mu.
    intercepts map[string]*InterceptInfo
    nextItemID uint64

    // Static-peer mode (legacy, без camp).
    staticPeer       atomic.Pointer[net.UDPAddr]
    lastStaticPingMs atomic.Int64

    cancel  context.CancelFunc  // отменяет все воркеры на Stop
    workers sync.WaitGroup      // ждём пока воркеры разъедутся
    started time.Time

    // Счётчики, атомарные:
    txBytes, rxBytes     atomic.Uint64
    txPackets, rxPackets atomic.Uint64

    tap *logTap  // broadcast-pipe в лог-стрим SSE

    // Хуки в main:
    OnStarted func(localIP string)
    OnStopped func()

    tunnelHTTPPort string  // порт для domain-poll
}
```

**Что важно понять:**

- Engine — **долгоживущий объект**. `New()` создаёт пустой, `Start(cfg)`
  поднимает всё, `Stop()` гасит, потом можно снова `Start(cfg)`.

- Все воркеры (горутины) запускаются в `Start` и привязаны к
  `context.WithCancel(...)`. Когда `Stop` вызывает `cancel()` — все они
  получают `<-ctx.Done()` и выходят.

- `peers map` — это **наш локальный view** на peer-ов. Каждый peer
  имеет `peerState` (см. ниже). Карта индексируется по tunnel_ip
  (`10.99.0.3`).

### Тип `peerState`

```go
type peerState struct {
    Name        string
    TunnelIP    string
    PublicIP    string
    UDPPort     int
    UDPEndpoint string
    JoinedAt    int64
    Online      bool          // camp видел announce недавно
    LastSeenAt  int64
    Domains     []DomainEntry // приехало через domain-poll

    UDPAddr      *net.UDPAddr  // куда слать UDP (берётся из камп-репорта)
    LastSeenMs   atomic.Int64  // когда последний раз мы получили от него
                               //   ЛЮБОЙ пакет (punch или real IP).
                               //   Из этого берётся "reachable" дот.
    LastPingMs   atomic.Int64  // когда мы последний раз пунчили
}
```

### Тип `InterceptInfo`

```go
type InterceptInfo struct {
    ID       string   // "i7" — генерится engine'ом
    Spec     string   // что добавил пользователь ("ya.ru", "1.2.3.4/24")
    Peer     string   // имя peer-а — обязательное поле
    Prefixes []string // что resolveSpec реально вытащил из DNS
}
```

`Spec` хранит юзерскую строку, `Prefixes` — IPv4/IPv6 CIDR-ы, которые
получились после резолва. На каждое из `Prefixes` engine ставит
host-маршрут через utun.

### Воркеры (goroutines), запускаемые в Start

| Имя | Что делает | Каденс |
|---|---|---|
| `tunToPeerLoop` | Читает IP-пакеты из утуна, решает кому слать (routeFor), пушит UDP'ом peer-у | event-driven (на каждый пакет) |
| `peerToTunLoop` | Читает UDP с сокета, идентифицирует peer-а, обновляет LastSeen, пишет IP-пакет в утун | event-driven |
| `holePunchLoop` | Шлёт 1-байтовые UDP-пакеты всем peer-ам чтобы NAT-мэппинги не закрылись | 1Hz если peer не reachable, 25s если reachable |
| `domainRefreshLoop` | Резолвит домены из intercepts (для случая когда IP изменился, e.g. CDN) | 1 раз в 60 сек |
| `announce.Run` | Шлёт UDP-announce на camp-сервер | 20 секунд |
| `PeerListPoller.Run` | HTTP-GET'ит `/api/id/<camp>` с camp-сервера, обновляет peer-list | 30 секунд |
| `domainPollLoop` | Обходит online-peer-ов, GET'ит их `/api/domains` через туннель | 10 секунд |

Каждая помечается `e.workers.Add(1)` перед запуском и `defer e.workers.Done()` внутри.
В `Stop` после `cancel()` мы делаем `e.workers.Wait()` — ждём пока все
goroutines корректно разъедутся.

### `routeFor(pkt)` — куда отправить пакет

Это сердце multi-peer маршрутизации:

```go
func (e *Engine) routeFor(pkt []byte) *net.UDPAddr {
    dst := packet.ExtractDst(pkt)

    // 1. Если dst — tunnel_ip какого-то peer-а → отправляем напрямую ему.
    //    Это для peer-to-peer трафика (meet, прямые соединения).
    if p, ok := e.peers[dst.String()]; ok && p.UDPAddr != nil {
        return p.UDPAddr
    }

    // 2. Если dst попал в префикс какого-то intercept'а → отправляем
    //    к peer-у, к которому intercept привязан.
    target := e.interceptPeerForLocked(dst)
    if target != "" {
        for _, p := range e.peers {
            if p.Name == target && p.UDPAddr != nil {
                return p.UDPAddr
            }
        }
    }

    // 3. Иначе — drop.
    return nil
}
```

В логах будут строки вида `[utun7] ipv4 src=10.99.0.2 dst=8.8.8.8 [→peer]`
(нашли куда слать) или `[drop-no-route]` (некуда).

### `peerToTunLoop` — приём

Когда UDP-пакет прилетает, мы должны понять кто его прислал:

```go
// 1. Это camp-сервер? (announce-reply)
if sameUDPAddr(campAddr, from) {
    announce.HandlePacket(pkt) // обновляет наш PeerInfo
    continue
}

// 2. Источник совпадает с известным peer.UDPAddr → это он.
//    (даже для 1-байтовых punch-пакетов).

// 3. Если пакет похож на IPv4 (n>=20, version=4) — глянем
//    src tunnel_ip в IP-заголовке. Может быть peer чей NAT
//    сменил порт (срабатывает редко, но защита).

// 4. Если опознали → обновляем p.LastSeenMs, может обновим UDPAddr.

// 5. Если пакет короче 20 байт (punch) — здесь и завершаем,
//    в утун не пишем.

// 6. Иначе — пишем сырой IP-пакет в утун, ядро его разрулит.
```

### Hooks

```go
OnStarted func(localIP string)
OnStopped func()
```

Engine не знает про `web.Server` (это плохая зависимость — циклическая).
Вместо этого main.go подписывает callback'и, которые web.Server использует
для запуска tunnel-listener'а в нужный момент.

---

## internal/tunnel

~170 строк. Тонкая обёртка над пакетом
`golang.zx2c4.com/wireguard/tun/...` — это библиотека WireGuard
которая умеет создавать утун-устройства на macOS.

**Что предоставляет:**

```go
type Tunnel struct { ... }

func Open(localIP, peerIP string) (*Tunnel, error)           // point-to-point
func OpenSubnet(localIP string, prefixLen int) (*Tunnel, error)  // /24 overlay

func (t *Tunnel) Name() string                  // "utun7"
func (t *Tunnel) Read() ([]byte, error)         // прочитать пакет
func (t *Tunnel) Write(pkt []byte) error        // отправить пакет в утун
func (t *Tunnel) Close() error                  // тейкдаун
```

Внутри: создаёт `utunN` (ядро само выберет номер), вызывает
`ifconfig utunN inet <localIP> <peerIP> netmask <...> up`, делает то же
для `/24` варианта.

**Ключевая деталь:** в camp-режиме мы зовём `OpenSubnet(localIP, 24)`,
что задаёт утуну весь `10.99.0.0/24` как «локальная подсеть» — ядро
теперь умеет роутить пакеты на любой `10.99.0.X` через утун.

---

## internal/route

~160 строк. Управляет host-маршрутами через утун.

```go
type Manager struct { ifname string; entries []netip.Prefix }

func New(ifname string) *Manager
func (m *Manager) Add(p netip.Prefix) error       // route add -net p.String() -interface ifname
func (m *Manager) AddReject(p netip.Prefix) error // route add -net p -reject
func (m *Manager) Remove(p netip.Prefix) error
func (m *Manager) Cleanup() []error               // снести всё что добавили
```

Под капотом — вызовы `/sbin/route` через `os/exec`.

**Зачем AddReject?** Для IPv6-адресов intercept'а. Утун у нас IPv4-only,
если пакет IPv6 туда попадёт — он уйдёт в утун, увязнет, никто не
получит. Лучше **reject** — `connect()` сразу падает с ECONNREFUSED, и
браузер по Happy Eyeballs быстро переключится на IPv4-альтернативу.

---

## internal/egress

~260 строк. Включает NAT на egress-стороне.

Что делает при `Open(iface, subnet)`:

1. Сохраняет `sysctl net.inet.ip.forwarding` (читает текущее значение).
2. `sysctl -w net.inet.ip.forwarding=1` (включает IP-форвардинг).
3. `pfctl -E` — включает pf, получает «reference-counted token».
4. Загружает в anchor `com.apple/f2f-mac` правило:
   ```
   nat on en0 from 10.99.0.0/24 to any -> (en0)
   ```

State (PID, token, старое значение forwarding, anchor-имя) пишется
JSON-ом в `/var/run/f2f-mac.egress.json`. На следующем запуске мы
читаем этот файл (`sweepLeftover`) и если процесс с тем PID-ом уже
мёртв — откатываем за него. Это спасает от `kill -9`.

**Anchor `com.apple/f2f-mac`** попадает под уже существующий wildcard
`nat-anchor "com.apple/*"` в `/etc/pf.conf` — мы **не трогаем**
основной ruleset. Apple сделала эту дыру для своих VPN-сервисов; мы
ей пользуемся.

---

## internal/packet

~85 строк. Маленький парсер IPv4-заголовков.

```go
func ExtractDst(pkt []byte) netip.Addr   // байты 16..20 → dst IP
func Summary(pkt []byte) string          // "ipv4 src=10.99.0.2 dst=8.8.8.8 TCP len=64"
```

`Summary` — для красивых строчек в логе. Парсит первый байт
(`version<<4 | IHL`), достаёт src/dst, протокол, длину; для TCP/UDP
вытаскивает порты.

Используется в engine для логирования.

---

## internal/rendezvous

Клиент camp-сервера.

### `types.go`

Wire-протокол. Должен совпадать с `source/camp/src/types.ts`:

```go
type PeerInfo struct {
    Name        string
    PublicIP    string
    UDPPort     int
    UDPEndpoint string
    TunnelIP    string
    JoinedAt    int64
    Online      bool
    LastSeenAt  int64
}

type announceReq struct {  // отправляем
    T      string  // "announce"
    Name   string
    CampID string
}
```

### `announce.go`

UDP-клиент. Делит сокет с тоннелем (тот же `:9000`):

- `NewAnnounceClient(conn, campAddr, name, campID)` — конструктор.
- `AnnounceOnce(timeout)` — синхронно шлёт + ждёт ответ. Используется
  на старте, до того как `peerToTunLoop` стартует.
- `Run(ctx, every)` — фоновый цикл, шлёт announce каждые `every`
  (обычно 20 сек). Ответы он **не читает** — за это отвечает главный
  read-loop в engine (`peerToTunLoop`), который видит пакет от
  `campAddr` и зовёт `HandlePacket`.
- `HandlePacket(pkt)` — парсит ответ, обновляет `self atomic.Pointer[PeerInfo]`.

**Хитрость**: announce шлётся на тот же UDP-сокет, что и tunnel-данные.
Зачем? Потому что **NAT-мэппинг это per-сокет** на исходящей стороне.
Чтобы camp видел тот же external endpoint, что и peer-ы будут
использовать для hole punching, надо обращаться к camp с того же
сокета.

### `peerlist.go`

HTTP-poller. Каждые 30 секунд GET'ит `https://f2f-camp.fly.dev/api/id/<camp>`,
парсит JSON, зовёт `onUpdate(peers)` (это в engine'е `applyPeerList`).

---

## internal/dns

~210 строк суммарно. Локальный DNS-сервер для зоны `<camp_id>.f2f`.

### `dns.go`

Использует библиотеку `github.com/miekg/dns` — стандарт для написания
DNS-серверов в Go.

```go
type Resolver interface {
    PeerDomains() map[string][]DomainEntry  // tunnel_ip → []entry
}

type Server struct { ... }

func Open(bindAddr, campID string, res Resolver) (*Server, error)
func (s *Server) Close() error
```

`Open` биндится на UDP `127.0.0.1:5354` (или другой указанный адрес),
регистрирует обработчик через `dns.NewServeMux().HandleFunc("<zone>", s.handle)`.

`handle(w, req)`:

1. Берёт первый вопрос (`req.Question[0]`).
2. Если qname не оканчивается на `.<camp_id>.f2f.` → REFUSED.
3. Если qtype = A или ANY → ищет в `res.PeerDomains()` по короткому label'у.
4. Если найдено → A-запись с IP владельца.
5. Если AAAA → NOERROR без ответов (наша оверлей-сеть только IPv4).

### `resolver.go`

Управляет файлом `/etc/resolver/<camp_id>.f2f`:

```
nameserver 127.0.0.1
port 5354
search_order 1
```

macOS-резолвер автоматически подхватывает любые файлы в
`/etc/resolver/<zone>` и направляет запросы для `<zone>` на указанный
nameserver/port.

`WriteResolver(campID, bindAddr)` — пишет файл (нужно root).
`RemoveResolver(campID)` — удаляет на Stop.

---

## internal/web

~640 строк Go + JS/HTML/CSS в `assets/`. HTTP UI и API.

### Структура

```go
type Server struct {
    engine    *engine.Engine
    addr      string         // "127.0.0.1:2202" — основной listener
    srv       *http.Server   // loopback UI
    tunnelSrv *http.Server   // <tunnel_ip>:2202 — узкий listener
    signals   *signalHub     // SSE-broadcaster для WebRTC signalling
    ...
}
```

### Два HTTP-листенера

Это нетривиальная часть архитектуры:

- **Loopback** (`127.0.0.1:2202`) — полный UI, все API-эндпоинты. Только
  для приложений на этой же машине.
- **Tunnel** (`<tunnel_ip>:2202`) — узкий, только два пути:
  - `POST /api/signal/inbox` — приём WebRTC-сигналов от peer-ов.
  - `GET /api/domains` — peer-ы пуллят наши доменные имена.

Зачем два? Чтобы UI **не торчал в локалку**. Если бы мы биндились
`0.0.0.0:2202` — любой в Wi-Fi мог бы зайти в UI. Loopback изолирован.
Tunnel listener поднимается **только пока engine жив** и физически
доступен только через утун (никакого пути из LAN в `10.99.0.X` без
утун-маршрута).

`BindTunnel(ip)` / `UnbindTunnel()` зовутся из `OnStarted`/`OnStopped`
хуков engine.

### Embed assets

```go
//go:embed assets
var assetsFS embed.FS
```

— это директива Go-компилятора: «возьми всё содержимое `assets/`
папки и встрой в бинарь как `embed.FS`». В рантайме мы делаем
`fs.Sub(assetsFS, "assets")` и сервим через `http.FileServer`. Один
бинарь без внешних файлов.

### Signal hub

Маленький broadcast в памяти для WebRTC SSE:

```go
type signalHub struct {
    mu     sync.Mutex
    subs   map[chan []byte]struct{}
}

func (h *signalHub) subscribe() (chan []byte, func())  // вернёт канал + unsubscribe
func (h *signalHub) broadcast(msg []byte)              // отправит во все подписки
```

Каждый браузер, подписанный на `GET /api/signal/stream`, получает
свой канал. Когда `POST /api/signal/inbox` принял сообщение от peer-а
— оно фанаутится через broadcast всем подписчикам.

### Frontend (`assets/`)

- **`index.html`** — SPA. 5 табов: camp, tunnel, dns, meet, drop.
- **`app.js`** — большая часть UI-логики: рендеринг peer-ов, intercepts,
  domains, кнопки start/stop, refresh-циклы (status каждые 3с,
  topology каждые 2с, camp peers каждые 3с, my-domains каждые 5с).
- **`audio.js`** — WebRTC. Один `RTCPeerConnection` с обоими видео-треками
  + screen share + data channel.
- **`audio.css`** — все стили (терминальная эстетика).
- **`vendor/`** — jquery, d3, tailwind (всё минифицированное, тут чтобы
  не зависеть от CDN).

UI не делает sophisticated framework-things — простой jquery +
ручной DOM-манипуляции. Удобно читать.

---

## Главные сущности

| Тип | Где живёт | Что хранит |
|---|---|---|
| `engine.Engine` | runtime | вся state — utun, UDP, peers, intercepts, domains |
| `engine.Config` | передаётся в `Start` | LocalIP, Listen, Camp{Name,ID,URL,StunAddr}, EgressIface |
| `engine.peerState` | внутри `Engine.peers` | вид одного peer-а — name, tunnel_ip, UDPAddr, lastSeen |
| `engine.InterceptInfo` | внутри `Engine.intercepts` | один intercept — spec, peer, resolved prefixes |
| `engine.DomainEntry` | в `myDomains` и `peerState.Domains` | name + port + proto |
| `engine.Status` | возвращается из `Status()` | снапшот для UI: running, peers, intercepts, counters |
| `engine.PeerStatusInfo` | внутри `Status.Peers` | per-peer строка для UI: имя, ip, online, reachable, active |
| `rendezvous.PeerInfo` | из camp-сервера | wire-формат — name, public_ip, udp_endpoint, tunnel_ip |
| `rendezvous.AnnounceClient` | в engine.announce | UDP-клиент camp-сервера |
| `rendezvous.PeerListPoller` | goroutine | HTTP-poller camp-сервера |
| `tunnel.Tunnel` | engine.tun | обёртка над утуном |
| `route.Manager` | engine.routes | host-маршруты через утун |
| `egress.Egress` | engine.egr | pf NAT |
| `dns.Server` | engine.dnsSrv | локальный DNS |
| `web.Server` | в main.go | HTTP UI |

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

Почему мы не маппим свои домены в свой tunnel_ip (`10.99.0.2`)? Потому что
ядро macOS маршрутизирует пакеты на `10.99.0.2` **через утун** (`route -n
get 10.99.0.2` показывает `interface: utun7`). А engine при чтении из утуна
видит свой собственный `dst=10.99.0.2`, в peers-map его не находит (это же
**мы**), интерсепт тоже не матчится → drop. Локальный сервер недостижим.
Loopback `127.0.0.1` обходит эту проблему.

### B. Friend открывает `gitlab.beer.f2f:3000`

1. Его macOS делает DNS — у него `/etc/resolver/beer.f2f` тоже стоит,
   запрос идёт в его локальный `127.0.0.1:5354`.
2. **Его** engine.PeerDomains возвращает: для tunnel_ip `10.99.0.2`
   есть домен `gitlab` (он узнал об этом из polling-цикла моего
   `/api/domains`).
3. DNS отвечает `gitlab.beer.f2f → 10.99.0.2`.
4. Его Chrome открывает TCP к `10.99.0.2:3000`.
5. Его kernel роутит на утун (10.99.0.0/24 → utun).
6. Его `tunToPeerLoop` читает IP-пакет.
7. `routeFor(pkt)`: dst = `10.99.0.2` → находит в его `peers` map моего
   `peerState`, возвращает `myUDPAddr` (`171.97.230.138:7851`).
8. Пакет уходит UDP'ом на 171.97.230.138:7851.
9. Через интернет (либо hairpin NAT если на одной сети) долетает до меня.
10. Мой kernel доставляет UDP в `:9000` (мой engine).
11. Мой `peerToTunLoop` читает, проверяет источник — это friend, обновляет
    LastSeen.
12. Пакет — IPv4 (n>=20), записываем в утун.
13. Мой kernel получает пакет из утуна, видит dst = `10.99.0.2` (это **я**) —
    локально достижим, доставляет в TCP-стэк.
14. Мой Python HTTP-сервер на `*:3000` (а `*` включает в себя `10.99.0.2`) —
    получает запрос, отвечает.
15. Обратный путь зеркальный.

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
12. У него kernel роутит: dst=1.2.3.4 — это публичный IP, не локальный.
    Идёт через egress.
13. **pf NAT** на его стороне меняет src с `1.2.3.4` (нет, src был **моим**
    `10.99.0.2`) на его публичный IP.
14. Пакет уходит в интернет с его IP.
15. `myip.com` отвечает «твой IP такой-то» — а это IP Friend'а, не мой.
16. Обратный путь обратно тем же макаром.

### D. Hole-punching

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

### Sticky tunnel_ip через Turso

Раньше каждый restart peer-а давал новый октет. Это ломало intercept-binding
(`gmail.com via Vsevolod` где Vsevolod был `10.99.0.5`, рестарт — он стал
`10.99.0.8`). Теперь camp хранит `(camp_id, name) → octet` в Turso, и
один и тот же name всегда получает один и тот же tunnel_ip.

### Polling вместо push для domains

Альтернатива была — каждый peer пушит свой список доменов на остальных
при изменении. Но это требует знать кто online, обрабатывать ошибки
доставки, заводить retry-логику. Polling проще: каждые 10 секунд —
GET у каждого online peer-а, либо ответ есть, либо нет. Idempotent.
Для нашего масштаба (3-10 peer-ов в camp-е) — overhead копеечный.

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
