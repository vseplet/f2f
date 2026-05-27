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
11. [Пакет `internal/ca` — local Certificate Authority](#internalca)
12. [Пакет `internal/keychain` — macOS keychain helpers](#internalkeychain)
13. [Пакет `internal/torrent` — BitTorrent через anacrolix](#internaltorrent)
14. [Пакет `internal/sfu` — Pion SFU для групповых звонков](#internalsfu)
15. [Пакет `internal/web` — HTTP UI + reverse-proxy](#internalweb)
16. [Главные сущности (типы)](#главные-сущности)
17. [Сценарии: куда идёт каждый пакет](#сценарии-куда-идёт-каждый-пакет)
18. [Карта горутин](#карта-горутин)
19. [Дизайнерские решения и почему так](#дизайнерские-решения)
19. [TODO: рефакторинг на слоёную архитектуру + QUIC](#todo-рефакторинг-на-слоёную-архитектуру--quic)
20. [TODO: миграция inter-node на QUIC](#todo-миграция-inter-node-коммуникации-на-quic)
21. [TODO: универсальное десктоп-приложение](#todo-универсальное-десктоп-приложение)
22. [TODO: инженерные улучшения](#todo-инженерные-улучшения)

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
    │   ├── engine.go            #   ~1500 строк, основной файл
    │   ├── log.go               #   broadcast-логгер
    │   └── egress_iface_darwin.go  # авто-детект default route
    ├── tunnel/                # обёртка над wireguard/tun (utun)
    ├── route/                 # вызовы /sbin/route для host-маршрутов
    ├── egress/                # pf NAT + persistent state
    ├── packet/                # парсер IPv4-заголовков
    ├── rendezvous/            # клиент camp-сервера
    │   ├── types.go             # wire-протокол
    │   ├── announce.go          # UDP announce
    │   └── peerlist.go          # HTTP-poll
    ├── dns/                   # локальный DNS на 127.0.0.1:5354
    │   ├── dns.go               # сам DNS-сервер
    │   └── resolver.go          # запись /etc/resolver/*
    ├── ca/                    # local Certificate Authority (HTTPS)
    │   └── ca.go                # генерация ECDSA P-256, Name-Constrained
    │                            # на <camp_id>.f2f, leaf-cert per SNI
    ├── keychain/              # обёртки `security` CLI
    │   └── keychain.go          # add-trusted-cert, find-certificate
    │                            # by fingerprint, delete-certificate
    ├── torrent/               # BitTorrent client (anacrolix wrapper)
    │   └── torrent.go           # seed/download, биндится на
    │                            # <tunnel_ip>:6881, без DHT/trackers
    └── web/                   # HTTP UI + API + reverse-proxy
        ├── server.go            # роутер + хендлеры + proxy listener'ы
        └── assets/              # embed'ятся в бинарь
            ├── index.html       # SPA, 6 табов (camp/tunnel/dns/meet/meet2/drop)
            ├── app.js           # tunnel/camp/dns/intercepts/CA/drop
            ├── audio.js         # meet/WebRTC (1:1 звонки)
            ├── meet2.js         # meet/2 (групповые звонки через SFU)
            ├── audio.css        # стили (общие для meet и meet/2)
            └── vendor/          # tailwind, jquery
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

## internal/firewall

~230 строк. Default-deny на inbound утана + явный allow-list.

Без него любой сервис на твоей машине, который слушает на
`0.0.0.0:<port>`, автоматически становится доступным остальным
peer'ам camp'а через `<tunnel_ip>:<port>` — sshd, postgres, dev-
сервера, всё. Это плохо для лагерей где membership не такой строгий.

Что делает при `Open(iface, tunnelIP, allowed []PortRule)`:

1. Кладёт в anchor `com.apple/f2f-mac-fw` ruleset вида:
   ```
   pass out on utun7 keep state                                            # outbound не ограничен
   block in on utun7 from any to 10.99.0.2/32 all                          # default deny — ТОЛЬКО для пакетов в нас
   pass in on utun7 proto icmp from any to 10.99.0.2/32 keep state         # ping к нам разрешён
   pass in on utun7 proto tcp from any to 10.99.0.2/32 port { 2202 80 443 6881 22 } keep state
   pass in on utun7 proto udp from any to 10.99.0.2/32 port { 6881 } keep state
   # пакеты НЕ на 10.99.0.2/32 (другие peer'ы, public IPs для egress) — implicit pass
   ```
2. Сохраняет state (PID, anchor, iface, tunnel_ip, список разрешённых
   правил) JSON-ом в `/var/run/f2f-mac.firewall.json`. На следующем
   запуске читаем этот файл (`sweepLeftover`) и если процесс с PID-ом
   мёртв — flush'аем anchor за него.

Метод `Apply(allowed []PortRule)` — горячая перезагрузка ruleset'а
без рестарта анкора. Engine вызывает его каждый раз когда юзер
меняет open-ports через UI.

**Scope правил — только destined на наш tunnel_ip.** Это критично,
потому что через утан проходят три класса пакетов:

- **К нам** (`dst = our tunnel_ip`) — наши сервисы. Default-deny + явный allow.
- **К другим peer'ам** (`dst = peer's tunnel_ip`) — engine их прочитает
  из утана и форварднёт UDP'ом дальше. Firewall их **не трогает**
  (implicit pass).
- **К публичным IP** (egress forwarding — Bob делает intercept через
  Alice) — Alice'ин engine пишет пакет в утан, kernel роутит через
  egress на en0. Firewall их **не трогает** — иначе egress сломался
  бы для всех портов кроме built-in.

**Порядок rule'ов критичен.** pf использует last-matching-rule
семантику (без `quick`): для пакета на наш tunnel_ip port 443
матчится сначала `block`, потом `pass`, последний (pass) побеждает.

Engine хранит **два** списка портов:

- `builtinFirewallPorts` (в коде, не редактируется через UI) —
  `2202/tcp`, `80/tcp`, `443/tcp`, `6881/tcp`, `6881/udp`. Это те,
  без которых сам engine не работает: HTTP API tunnel-listener'а,
  reverse-proxy на 80/443, BT-клиент. Если их выключить — соседям
  ничего не достучаться.
- `userFirewall` (в `~/Library/Application Support/f2f/firewall.json`)
  — добавляются через UI, поддерживают on/off без удаления и
  description.

Финальный ruleset для pf = built-in + только включённые user
entries.

**Чего firewall НЕ трогает:**

- Loopback (`lo0`). DNS-сервер на `127.0.0.1:5354` им не задет —
  пакеты на loopback не идут через утан, pf-правила `on utun7` их
  не видят.
- Outbound. Мы выпускаем всё, что наша машина инициирует наружу
  (через `pass out on utun7`).
- `127.0.0.1:80/:443/:2202` слушающие части reverse-proxy и UI —
  они тоже на loopback, не на утане.
- Egress NAT-правила. У них свой anchor (`com.apple/f2f-mac`),
  это `nat` rules, а firewall — `block/pass` rules; они не
  конфликтуют.

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

## internal/ca

~240 строк. Local Certificate Authority для зоны `<camp_id>.f2f` —
позволяет engine'у выдавать leaf-сертификаты для HTTPS-прокси без
обращения к публичному CA.

```go
type CA struct {
    Cert    *x509.Certificate
    Key     *ecdsa.PrivateKey
    CertPEM []byte
    KeyPEM  []byte
    leafCache map[string]*tls.Certificate
}

func Generate(campID string) (*CA, error)           // ECDSA P-256 self-signed root
func (ca *CA) IssueLeaf(hostname string) (*tls.Certificate, error)  // подписать leaf
func (ca *CA) Save(dir string) error                 // ca.crt + ca.key на диск
func Load(dir string) (*CA, error)                   // прочитать обратно
func (ca *CA) MatchesZone(campID string) bool        // pinned к нашей зоне?
func (ca *CA) Fingerprint() string                   // короткий hex для UI
func (ca *CA) Fingerprint256Hex() string             // полный SHA-256 для keychain-сравнения
```

**Ключевая деталь — Name Constraints.** При генерации CA проставляется
расширение RFC 5280 `permittedDNSDomains: [".<camp_id>.f2f"]` с критическим
флагом. Браузер при проверке цепочки видит это поле и **отвергает**
любой leaf, чей DNSName не попадает в эту зону. Даже если CA добавлен
в trust store, он математически не может выпустить cert на `bank.com`
или `gmail.com` — только на `*.<camp_id>.f2f`.

**Алгоритм — ECDSA P-256.** Не ed25519, потому что macOS `security`
CLI отказывается импортировать ed25519-CA в keychain (`Unknown format
in import`). P-256 универсально поддерживается keychain'ом, всеми
браузерами, curl/libssl.

**Leaf-сертификаты выдаются по требованию.** При TLS-handshake'е
engine читает SNI, ищет `IssueLeaf(SNI)` в кэше — если нет, генерирует
новый leaf на лету и кэширует. Срок 1 год, в файлы не пишутся (cheap
to regenerate).

---

## internal/keychain

~80 строк. Обёртки над macOS-утилитой `security`.

```go
// Проверка наличия cert'а в keychain по SHA-256 — read-only, без prompt'а.
func IsInstalledByFingerprint(sha256HexUpper string) bool

// Установка cert'а как trusted root для SSL. ВСЕГДА триггерит macOS
// authentication prompt (даже под sudo) — это by design Apple для
// защиты от malware который мог получить root.
func AddTrustedRoot(certPath string) error

// Удаление cert'а по Subject CN. Без prompt'а.
func RemoveByCommonName(cn string) error
```

**Почему по fingerprint'у, а не по имени.** Если в keychain'е валяется
старый cert с тем же CN (например, после неудачного install'а
предыдущей версии f2f-mac) — name-based check сказал бы «уже стоит»,
мы скипнули бы install, и trust бы не установился. Fingerprint-based
проверка надёжно отличает «этот конкретный cert» от «другой cert с
тем же именем».

---

## internal/torrent

~310 строк. Тонкая обёртка над `github.com/anacrolix/torrent` —
канонический Go-нативный BitTorrent client. Используется для
peer-to-peer file sharing внутри camp-а.

```go
type Options struct {
    ListenAddr   string // "<tunnel_ip>:6881"
    SharedDir    string // ~/Library/Application Support/f2f/shared
    DownloadsDir string // ~/Downloads/f2f-drops
}

type Client struct {
    atc      *atorrent.Client
    seeding  map[string]*SeedHandle  // info_hash hex → seed
    loading  map[string]*Download    // info_hash hex → download
}

type Download struct {
    InfoHash, Name, Magnet string
    Size                   int64
    Torrent                *atorrent.Torrent
    StartedAt              time.Time
    Peers                  []string  // re-fed periodically by engine
    LastProgressAt         time.Time // for stall detection
    LastBytes              int64
}

func New(opts Options) (*Client, error)
func (c *Client) AddSeed(path string) (*SeedHandle, error)
func (c *Client) RemoveSeed(infoHashHex string) error
func (c *Client) ListSeeds() []*SeedHandle
func (c *Client) AddDownload(magnetURI string, peerAddrs []string) (*Download, error)
func (c *Client) RemoveDownload(infoHashHex string) bool
func (c *Client) FeedPeers(d *Download)
func (c *Client) DownloadPath(d *Download) string
func (c *Client) ListDownloads() []*Download
```

**Persistence + recovery (живёт в `engine`, не в этом пакете):**

- `~/Library/Application Support/f2f/downloads.json` — список
  `{magnet, info_hash, peers}` всех когда-либо начатых downloads.
  Сохраняется в `engine.AddDownload`, читается в `restoreDownloads`
  при старте engine'а. После рестарта anacrolix re-hash'ит файлы на
  диске → resumes от текущего прогресса (новые piece'ы не качаются
  заново).
- `engine.rescanSharedDir` на старте обходит SharedDir и
  `AddSeed`'ит всё что там лежит — раздачи переживают рестарт.
- `engine.pruneLoop` (раз в 30с): stat path каждого Download/Seed,
  если файл удалён юзером — `RemoveDownload`/`RemoveSeed`.
  Inflight-загрузки **не дропаются** (они пишут в `.part`, финального
  пути ещё нет).
- `engine.refeedActiveDownloads` (тоже в pruneLoop): для каждого
  inflight Download делает `FeedPeers` — anacrolix получает peer'а
  заново. Если **нет прогресса >90с** (LastProgressAt) → дропаем
  torrent и заново `AddDownload(magnet, peers)`. Кейс «source-peer
  рестартанул мид-загрузка»: anacrolix получает свежий цикл
  подключений + сохранённые piece'ы → resume.
- `engine.chownLoop` (раз в 10с): рекурсивно chown'им SharedDir и
  DownloadsDir в SUDO_USER. Engine крутится под root через sudo,
  anacrolix пишет piece'ы как root, без chown юзер не мог бы
  удалить или перенести файлы из Finder без админ-прав.

**Конфигурация anacrolix-клиента:**

- `NoDHT = true` — без распределённой hash-таблицы (отказ от публичного discovery).
- `DisableTrackers = true` — без публичных tracker'ов.
- `DisablePEX = true` — без peer-exchange (соседи не разглашают друг друга в обход нас).
- `DisableIPv6 = true` — utun у нас IPv4-only, попытка слушать tcp6 на `10.99.0.X` падает с `no suitable address found`.
- `Seed = true` — авто-seed скачанного.

**Discovery идёт не через BT-механизмы**, а через наш `/api/files`
endpoint на tunnel-listener'е. Каждый peer публикует свой каталог,
остальные пуллят его раз в минуту, кладут в `peerState.Files`.
Когда юзер кликает download, engine берёт магнет + peer'овский
`<tunnel_ip>:6881` и зовёт `client.AddMagnet` + `t.AddPeers`.

**Создание seed-торрента:**

```go
info := metainfo.Info{
    PieceLength: 1 << 20, // 1 MiB
    Name:        filepath.Base(path),
    Length:      stat.Size(),
}
info.BuildFromFilePath(path) // считает SHA-1 на каждый piece
infoBytes, _ := bencode.Marshal(info)
mi := &metainfo.MetaInfo{InfoBytes: infoBytes}
infoHash := mi.HashInfoBytes()

t, _ := client.AddTorrentOpt(atorrent.AddTorrentOpts{
    InfoHash: infoHash,
    Storage:  storage.NewFile(filepath.Dir(path)), // seed in-place
})
t.SetInfoBytes(mi.InfoBytes)
t.AllowDataUpload()
t.DownloadAll()  // помечает что у нас есть все piece'ы
```

После этого магнет в формате
`magnet:?xt=urn:btih:<info_hash>&dn=<filename>` готов и публикуется
через `/api/files`.

---

## internal/sfu

Встроенный SFU (Selective Forwarding Unit) для групповых звонков.
Использует Pion WebRTC v4 — чистый Go, без CGo.

### Архитектура

SFU запускается на одном из пиров в camp (хост звонка). Все участники
подключаются к нему через один `RTCPeerConnection`. SFU пробрасывает
RTP-пакеты между участниками без транскодинга.

```
Peer A (browser) ──WebRTC──► SFU (на хосте) ──WebRTC──► Peer B (browser)
                              │
                              └──WebRTC──► Peer C (browser)
```

### ICE и интерфейсы

SFU использует **два `webrtc.API`** с разными `SettingEngine`:

- **`localAPI`** — для хоста (участник на той же машине что и SFU).
  Без фильтра интерфейсов — ICE идёт через loopback.
- **`tunnelAPI`** — для удалённых пиров. `SetInterfaceFilter` пропускает
  только utun-интерфейс. Весь WebRTC-трафик идёт строго через tunnel overlay.

STUN/TURN не нужен. Причина разделения: self-addressed пакеты на utun
дропаются ядром (`drop-no-route`), поэтому хост не может подключиться
к своему SFU через tunnel IP.

### Типы

```go
type SFU struct {
    localAPI     *webrtc.API   // для хоста (все интерфейсы)
    tunnelAPI    *webrtc.API   // для удалённых пиров (только utun)
    localIP      string        // tunnel IP хоста
    participants map[string]*Participant
    onSignal     func(to string, msg []byte)
    renegTimers  map[string]*time.Timer  // batched renegotiation
}

type Participant struct {
    TunnelIP string
    Name     string
    PC       *webrtc.PeerConnection
    DC       *webrtc.DataChannel   // chat relay
    tracks   map[string]*publishedTrack
}

type publishedTrack struct {
    local       *webrtc.TrackLocalStaticRTP  // клон для подписчиков
    publisherPC *webrtc.PeerConnection       // PC издателя (для RTCP PLI)
    remoteSSRC  webrtc.SSRC                  // SSRC оригинального трека
}
```

### Как работает

1. **Хост создаёт звонок** → `engine.CreateCall()` → `sfu.New(localIP, utunName, ...)` →
   `AddParticipant(hostIP)` (использует `localAPI`).
2. **Удалённый пир join'ит** → POST на `/api/call/join` хоста → `AddParticipant(peerIP)`
   (использует `tunnelAPI`). Существующие треки других участников добавляются в новый PC.
3. **Браузер шлёт offer** → SFU создаёт answer → ICE устанавливается.
4. **OnTrack** — SFU получает трек от издателя → создаёт `TrackLocalStaticRTP` клон →
   добавляет его во все остальные PC → шлёт `renegotiate` (см. ниже).
5. **RTP forwarding** — горутина: `remote.Read(buf)` → `local.Write(buf[:n])`.
   Без транскодинга. При завершении потока (трек removed) удаляет sender'ы у
   подписчиков и шлёт `renegotiate`.
6. **RTCP forwarding** — горутина `forwardRTCP` для каждого `RTPSender`:
   читает PLI/FIR от подписчика и форвардит на PC издателя.
7. **PLI burst** — при появлении нового трека 5×PLI каждые 2с, чтобы
   гарантировать keyframe после завершения renegotiation.
8. **Chat** — DataChannel relay: `broadcastChat` рассылает всем кроме отправителя.
9. **Leave** — хост → `SFU.Close()`. Пир → `RemoveParticipant()` →
   удаляет треки из других PC + renegotiation.

### Renegotiation (browser-initiated)

SFU **никогда не создаёт offer'ы**. Вместо этого используется
гибридная browser-initiated схема:

1. SFU добавляет/удаляет треки на PC подписчика.
2. SFU шлёт сигнал `{"kind":"renegotiate","tracks":[{"kind":"video"},...]}`
   с описанием своих sender-треков.
3. **Batching**: запросы группируются по 200мс (через `time.AfterFunc`),
   чтобы audio+video от одного пира попали в один offer.
4. Браузер считает дефицит transceivers: сравнивает количество треков
   по kind с количеством своих recv-capable transceivers. При нехватке
   добавляет `recvonly` transceivers.
5. Браузер создаёт offer → POST на SFU → SFU отвечает (answer включает
   все sender-треки).

Почему browser-initiated, а не SFU-initiated:
- SFU-initiated offers (CreateOffer на Pion) нарушали ICE-сессию:
  каждый renegotiation вызывал кратковременный `disconnected` (~5-13с).
- Browser-initiated: ICE credentials не меняются (браузер всегда offerer),
  ICE-сессия не прерывается.

### Сигнализация

- **Хост ↔ свой браузер**: браузер POST на `/api/call/signal` (loopback) →
  engine обрабатывает → ответ в POST response. SFU-сигналы (`renegotiate`,
  `candidate`) доставляются через `engine.OnLocalSFUSignal` → напрямую
  в `callSignals` SSE hub, минуя HTTP (utun self-traffic дропается ядром).
- **Удалённый пир ↔ SFU**: браузер POST на `/api/call/signal` (loopback) →
  `proxyCallSignalToHost` → POST на tunnel listener хоста → SFU обрабатывает.
  SFU-сигналы идут обратным путём через `deliverSFUSignal` → HTTP tunnel → SSE.

### Reconnection

`AddParticipant` поддерживает замену stale-участника (например, при
перезагрузке страницы хостом). Старый PeerConnection закрывается
(callback отключается чтобы не удалить нового), треки убираются из
подписчиков, создаётся новый PC. Фронтенд при загрузке проверяет
`/api/call/state` и автоматически rejoin'ит свой звонок.

### Call discovery

`callPollLoop` (3с) опрашивает `/api/call/state` у всех онлайн-пиров
(включая случай когда хост уже в звонке — чтобы видеть чужие группы
в dropdown). UI показывает все звонки через `GET /api/call/list`.

### API эндпоинты

| Метод | Путь | Где | Что делает |
|-------|------|-----|------------|
| GET | `/api/call/state` | loopback + tunnel | Состояние локального звонка |
| GET | `/api/call/list` | loopback | Все звонки: локальный + обнаруженные |
| POST | `/api/call/create` | loopback | Создать звонок (запускает SFU) |
| POST | `/api/call/join` | loopback + tunnel | Присоединиться (проксируется на хост) |
| POST | `/api/call/leave` | loopback + tunnel | Покинуть (проксируется на хост) |
| POST | `/api/call/signal` | loopback + tunnel | WebRTC signaling (проксируется на хост) |
| GET | `/api/call/signal/stream` | loopback | SSE для SFU-сигналов → браузер |

### Дизайнерские решения

- **Always-SFU** — даже для 1:1 звонков. Один режим, без mesh/SFU переключения.
- **SFU встроен в engine** — single binary, запускается/останавливается с вызовом.
- **Без транскодинга** — браузеры договариваются о кодеках (Opus + VP8/H264),
  SFU пробрасывает RTP as-is.
- **Browser-initiated renegotiation** — SFU не создаёт offer'ы, чтобы не
  нарушать ICE-сессию. Браузер всегда offerer.
- **Два WebRTC API** — раздельные ICE-политики для local (loopback) и
  remote (tunnel-only) участников.
- **Прямая SSE-доставка** — сигналы к локальному браузеру идут через
  `OnLocalSFUSignal` callback, минуя HTTP roundtrip через utun.
- **RTCP forwarding, не периодический PLI** — периодический PLI (2с/5с) вызывает
  видимые фризы при каждом keyframe. Правильный подход: форвардить PLI/FIR
  от подписчиков к издателям, как в нативном browser-to-browser WebRTC.
- **Camp server не участвует** — call signaling полностью peer-to-peer через tunnel.

### Решённые проблемы

Групповые звонки — самая сложная часть codebase. Ниже — проблемы,
которые были обнаружены и решены в процессе разработки.

**1. utun self-traffic drop (`drop-no-route`)**
Ядро macOS дропает пакеты с src=dst на utun (100.72.176.230 → 100.72.176.230).
SFU на хосте не мог доставить renegotiation-сигналы собственному браузеру через
HTTP на tunnel IP. **Решение**: `engine.OnLocalSFUSignal` — callback от Engine к
Server, сигналы к локальному браузеру идут напрямую в SSE hub, минуя HTTP.

**2. ICE disruption при SFU-initiated renegotiation**
Когда SFU создавал offer (`CreateOffer` на Pion), каждая renegotiation вызывала
кратковременный `pc state: disconnected` (~5-13с). Причина: Pion при CreateOffer
менял что-то в ICE/DTLS state, ломая существующую сессию. **Решение**: перешли
на browser-initiated renegotiation — SFU шлёт `"renegotiate"` сигнал, браузер
сам создаёт offer. ICE credentials не меняются, сессия не прерывается.

**3. Answerer не может добавлять m-lines**
В WebRTC answerer не может добавить новые media sections — только offerer.
Когда SFU добавлял треки через `AddTrack`, а браузер создавал offer, SFU's
answer не мог включить треки без соответствующих m-lines в offer. Screen share
от пира не появлялся у подписчика. **Решение**: SFU шлёт список своих
sender-треков (`tracks: [{kind:"video"},...]`). Браузер считает дефицит
recv-capable transceivers и добавляет `recvonly` transceivers перед offer'ом.

**4. Двойная renegotiation (audio + video одновременно)**
Когда пир подключался, audio и video треки приходили почти одновременно.
Первый вызывал renegotiation → `have-local-offer`, второй падал с ошибкой
(`InvalidModificationError`). **Решение**: batched renegotiation через
`time.AfterFunc(200ms)` — оба трека попадают в один цикл renegotiation.

**5. Stale participant при перезагрузке страницы хостом**
Хост перезагружает страницу → фронтенд теряет состояние, но SFU продолжает
работать со старым PeerConnection. `AddParticipant` возвращал старого участника
с мёртвым PC. Новый звонок создать нельзя — `"call already active"`.
**Решение**: `AddParticipant` при дублирующем IP чистит старого (закрывает PC,
убирает треки из подписчиков, отключает `OnConnectionStateChange` чтобы
закрытие старого PC не удалило нового). Фронтенд при загрузке проверяет
`/api/call/state` и auto-rejoin'ит.

**6. `leaveCall` затирал `sfuHost` при переключении групп**
При переключении из одной группы в другую: `joinCall` вызывал `leaveCall`,
который обнулял `sfuHost`. Новый `joinCall` не знал куда подключаться.
**Решение**: `joinCall` сохраняет target перед `leaveCall` и восстанавливает после.

**7. Forwarding loop не чистил subscriber'ов при остановке трека**
Когда пир останавливал screen share, forwarding loop завершался и удалял трек
из `sender.tracks`, но НЕ удалял local track из PC подписчиков. Тайл
оставался висеть. **Решение**: после выхода из forwarding loop — RemoveTrack
у всех подписчиков + renegotiation.

**8. Race condition: `onnegotiationneeded` vs renegotiate handler**
Когда браузер получал `"renegotiate"` от SFU и вызывал `addTransceiver`,
это триггерило `onnegotiationneeded`. Между `addTransceiver` (синхронный)
и `await createOffer()` (async, yield) event loop пускал `onnegotiationneeded`,
который создавал конкурирующий offer. Два одновременных offer'а ломали
renegotiation. Screen share не появлялся. **Решение**: флаг `negotiating` —
renegotiate handler ставит `negotiating = true`, `onnegotiationneeded`
проверяет `if (negotiating) return;`.

### Известные ограничения

- **Firewall и ephemeral UDP-порты**: Pion создаёт случайный UDP-порт для
  каждого PeerConnection. Эти порты не добавляются в firewall allow list.
  Работает через stateful pf (outbound STUN создаёт state entry, ответы
  пира проходят по state). Ненадёжно при race condition — если пир пошлёт
  первым, пакет дропнется. ICE обычно справляется, но это хрупкое место.
  **TODO**: `SetEphemeralUDPPortRange` + явное открытие в firewall при старте звонка.
- **tx/rx на хосте занижен**: хост ↔ свой SFU идёт через loopback, не через
  utun. Tunnel-счётчики видят только трафик SFU ↔ удалённые пиры. Для 2 участников
  хост видит ~50-70% от трафика по сравнению с 1:1 meet.
- **Нет ICE restart при смене сети**: если пир сменил WiFi, ICE-сессия умирает.
  Нужен механизм ICE restart или автоматический rejoin.
- **Phantom tracks**: иногда браузер получает `ontrack` для потоков с
  нераспознаваемым stream ID (без tunnel IP префикса). Тайлы создаются с
  лейблом "peer". Причина не установлена — возможно, Pion не всегда сохраняет
  кастомный stream ID из `NewTrackLocalStaticRTP`.

---

## internal/web

~1600 строк Go + JS/HTML/CSS в `assets/`. HTTP UI и API.

### Структура

```go
type Server struct {
    engine      *engine.Engine
    addr        string           // "127.0.0.1:2202" — основной listener
    srv         *http.Server     // loopback UI
    tunnelSrv   *http.Server     // <tunnel_ip>:2202 — inter-node listener
    proxySrvs   []*http.Server   // HTTP/HTTPS reverse-proxy на :80/:443
    signals     *signalHub       // SSE-broadcaster для WebRTC 1:1 signalling
    callSignals *signalHub       // SSE-broadcaster для SFU signalling (meet/2)
    ...
}
```

### Несколько HTTP-листенеров

Это нетривиальная часть архитектуры — на одной машине поднимается
несколько серверов, каждый со своей ролью:

- **Loopback UI** (`127.0.0.1:2202`) — полный UI, все API-эндпоинты.
  Доступен только локальным приложениям.
- **Tunnel listener** (`<tunnel_ip>:2202`) — узкий mux для inter-node:
  - `POST /api/signal/inbox` — приём WebRTC 1:1 сигналов от peer-ов.
  - `GET /api/domains` — peer-ы пуллят наши доменные имена с health.
  - `GET /api/ca-cert` — peer-ы пуллят наш CA для установки в их keychain.
  - `GET /api/files` — peer-ы пуллят наш торрент-каталог.
  - `GET /api/firewall` — peer-ы пуллят наши открытые порты.
  - `GET /api/call/state` — peer-ы поллят состояние группового звонка.
  - `POST /api/call/signal` — SFU signaling (offer/answer/candidate/renegotiate).
  - `POST /api/call/join` — вход в групповой звонок (от удалённого пира).
  - `POST /api/call/leave` — выход из группового звонка.
- **HTTP reverse-proxy** (`<tunnel_ip>:80` и `127.0.0.1:80`) —
  парсит Host header, ищет домен в `MyDomains`, форвардит на
  `127.0.0.1:<port>` через `httputil.ReverseProxy`.
- **HTTPS reverse-proxy** (`<tunnel_ip>:443` и `127.0.0.1:443`) —
  TLS-terminator. `tls.Config.GetCertificate` динамически зовёт
  `ca.IssueLeaf(SNI)` чтобы получить leaf для конкретного hostname'а.
  После handshake'а форвардит plain HTTP на backend.

Зачем разделение? **Безопасность** + **функциональная изоляция**:

- UI на `0.0.0.0` показывал бы Start/Stop любому в Wi-Fi.
- Tunnel listener доступен только через утун — кто не в нашем camp-е,
  не достучится.
- HTTP/HTTPS proxy биндится на стандартные веб-порты, чтобы
  `https://gitlab.<camp>.f2f` (без указания порта) работал.

`BindTunnel(ip)` / `BindProxies(ip)` / `UnbindTunnel()` / `UnbindProxies()`
зовутся из `OnStarted`/`OnStopped` хуков engine.

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

- **`index.html`** — SPA. 7 табов: camp, tunnel, dns, meet, meet-, drop, diagnostics.
- **`app.js`** — большая часть UI-логики: рендеринг peer-ов, intercepts,
  domains (с health-точкой + ссылками на `https://<fqdn>`), trusted
  peer CAs, кнопки start/stop, refresh-циклы (status каждые 3с,
  camp peers каждые 3с, my-domains каждые 5с, trusted peers каждые 5с).
- **`audio.js`** — 1:1 WebRTC (таб meet). Один `RTCPeerConnection` с видео-треками
  + screen share + data channel.
- **`meet2.js`** — групповые звонки (таб meet-). Работает через SFU (см. ниже).
- **`audio.css`** — все стили (терминальная эстетика).
- **`vendor/`** — jquery, tailwind (минифицированные, чтоб не зависеть
  от CDN). d3 убран после удаления топологии-графа из tunnel-таба.

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
| `ca.CA` | engine.ca | local CA + leaf-cert cache |
| `engine.DomainEntry` | в `myDomains`, `peerState.Domains` | name + port + proto + health |
| `engine.TrustedPeerCA` | в `engine.trustedPeerCAs` | UI-инфа о доверенном CA соседа |
| `torrent.Client` | engine.torrent | BT-клиент для камп-файлов |
| `torrent.SeedHandle` | внутри torrent.Client | один seed'ящийся файл (path, magnet, info_hash) |
| `torrent.Download` | внутри torrent.Client | один in-flight download |
| `engine.PeerFile` | в `peerState.Files` | файл от другого peer'а (name, size, magnet) |
| `web.Server` | в main.go | HTTP UI + tunnel listener + HTTP/HTTPS proxy |

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
13. **Firewall**: pf-anchor на утане проверяет `dst=10.99.0.2:3000`. Port
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
    `10.99.0.2`) на его публичный IP.
15. Пакет уходит в интернет с его IP.
16. `myip.com` отвечает «твой IP такой-то» — а это IP Friend'а, не мой.
17. Обратный путь обратно тем же макаром.

### D. Friend открывает `https://gitlab.beer.f2f/` (БЕЗ порта)

С реверс-прокси + Local CA — то самое «как gmail.com выглядит для
пользователя, без портов и предупреждений».

1. Friend'овский Chrome зовёт DNS на `gitlab.beer.f2f`. Системный
   резолвер видит `/etc/resolver/beer.f2f` → шлёт в `127.0.0.1:5354`
   на его машине.
2. Его DNS отвечает `10.99.0.2` (мой tunnel_ip).
3. Chrome открывает TCP на `10.99.0.2:443`. Пакет уходит в его утун.
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
12. Anacrolix открывает TCP к `<my_tunnel_ip>:6881`. Пакеты идут через **утун-туннель** (это же `10.99.0.X`).
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

При работающем engine запускается ~30 горутин из кода f2f. С учётом
горутин от зависимостей (Pion WebRTC, anacrolix/torrent, HTTP servers)
реальное число может быть 100-150+.

### Фиксированные worker'ы (engine.Start)

Отслеживаются через `e.workers` WaitGroup, завершаются при `ctx.Done()`.

| Горутина | Интервал | Назначение |
|----------|----------|------------|
| `tunToPeerLoop` | непрерывно | IP-пакеты из utun → UDP к пиру |
| `peerToTunLoop` | непрерывно | UDP от пиров → utun |
| `domainRefreshLoop` | 60с | Маршруты для intercept-доменов |
| `announce.Run` | ~20с | UDP announce к camp server |
| `poller.Run` | ~20с | HTTP poll peer list от camp |
| `holePunchLoop` | 25с + burst | UDP hole-punch ко всем пирам |
| `pinger.Run` | 5с | RTT ping/pong к пирам |
| `domainPollLoop` | 10с | Poll `/api/domains` у пиров |
| `domainHealthLoop` | 8с | TCP health-check своих доменов |
| `filesPollLoop` | 60с | Poll `/api/files` у пиров |
| `peerFirewallPollLoop` | 30с | Poll `/api/firewall` у пиров |
| `callPollLoop` | 3с | Poll `/api/call/state` у пиров |
| `peerCAPollLoop` | 30с | Poll `/api/ca-cert` у пиров |

**~13 наших worker'ов.**

### Torrent (fire-and-forget)

| Горутина | Назначение |
|----------|------------|
| `rescanSharedDir` | Скан shared dir при старте |
| `restoreDownloads` | Восстановление незавершённых загрузок |
| `chownLoop` | Периодический chown файлов юзеру |
| `pruneLoop` | Чистка торрент-стейта |

**4 наших + десятки внутренних горутин от anacrolix/torrent.**

### SFU (динамические)

| Горутина | Кол-во | Назначение |
|----------|--------|------------|
| `handleTrack` (RTP forward) | 1 на трек | `remote.Read` → `local.Write` |
| `forwardRTCP` | 1 на sender×subscriber | PLI/FIR подписчик → издатель |
| PLI burst | 1 на трек (временная, 10с) | 5×PLI для keyframe |
| renegotiate timer | 1 на участника (временная) | 200ms batching |

**+ горутины от Pion WebRTC** (ICE agent, DTLS, SCTP, SRTP per PC) —
каждый `PeerConnection` порождает ~20-30 внутренних горутин Pion.

### Web server

| Горутина | Назначение |
|----------|------------|
| tunnel HTTP listener | HTTP на tunnel IP |
| proxy listeners (×2) | HTTP/HTTPS reverse-proxy |
| SSE connections | 1 на открытый SSE stream |

### Почему 135 горутин при 2 участниках

| Источник | ~Кол-во |
|----------|---------|
| Engine workers | 13 |
| Torrent (наши + anacrolix) | 20-40 |
| SFU forwarding (3 трека × 2 направления) | 6-12 |
| Pion WebRTC (2 PC × ~25 горутин) | 40-60 |
| Web server + SSE | 5 |
| Go runtime (GC, finalizer, netpoll) | 5-10 |
| **Итого** | **~90-140** |

Основной вклад — **Pion** (~25 горутин на PeerConnection) и
**anacrolix/torrent** (~20-30 внутренних горутин). Наших собственных
— ~25-30.

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
зеркалится в `<camp_id>.config.json`, но протокол announce / peer-list
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
    peerping/              ← RTT ping/pong
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
Вынести peer state machine, UDP hole-punch, peerping.
Engine делегирует "сеть" в net/ через интерфейсы.

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

### 4. CLI структура (низкий приоритет)

**Проблема**: два режима через позиционный аргумент (`run` / UI).
Нет инструментов для диагностики без браузера.

**Решение**: subcommands:
```
f2f-mac ui [--bind addr]       # текущий UI-режим (default)
f2f-mac run [--camp id]        # headless
f2f-mac health                 # жив ли engine + peers
f2f-mac status                 # JSON snapshot (Status struct)
f2f-mac config show            # текущий конфиг
f2f-mac config set KEY=VALUE   # изменить параметр
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
