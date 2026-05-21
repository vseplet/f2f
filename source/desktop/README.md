# f2f-desktop

Lite-mode GUI клиент f2f. Один Wails-бинарь (Mac/Linux/Win), **без
root-прав, без `utun`, без `pf`, без NetworkExtension**. Подключается
к существующему `f2f-camp.fly.dev`, делает hole-punch с другими
peer'ами через UDP и даёт **meet** (WebRTC аудио/видео/screen share/
чат) и **drop** (BitTorrent через uTP) — всё на одном hole-punched
UDP-сокете.

Полнофункциональный mac-engine `source/mac` остаётся как был
(overlay-туннель, DNS, HTTPS, intercepts), а это — облегчённая
альтернатива для «просто созвониться и кинуть файл».

## Сборка и запуск

```sh
make desktop-install-wails   # один раз, ставит wails CLI с master*
make desktop-build           # собирает build/bin/f2f-desktop.app
make desktop-open            # build + open
make desktop-dev             # wails dev с hot-reload
```

\* `v2.12.0` (последний tag) имеет баг в `staticanalysis` —
`go/packages.Load` без `NeedDeps` падает на anacrolix-graph'е.
В master починили; мы залочены на pseudo-version из `go.mod`.

## Архитектура одного клиента

```
                                  camp.fly.dev
                                  ┌──────────┐
            announce  ─UDP/3478─► │          │
            peer list ◄─HTTP/443─ │          │
                                  └──────────┘
                                       │
        ╔══════════════════════════════╪══════════════════════════════╗
        ║                  f2f-desktop (без root)                     ║
        ║                                                             ║
        ║   ┌────────────────────────────────────────────────────┐    ║
        ║   │      ОДИН hole-punched UDP-сокет (lite UDP)        │ ◄──╫── другие peer'ы
        ║   │  recvLoop мультиплексирует по первому байту:        │    ║
        ║   │    0xFF  → hole-punch ping (lastSeen)              │    ║
        ║   │    0xF2  → WebRTC signaling → Wails event 'signal' │    ║
        ║   │    0xF3  → state envelope (files catalog) → cache  │    ║
        ║   │    else  → btProxy.forwardFromPeer (anacrolix uTP) │    ║
        ║   └────┬────────────────────────────────┬──────────────┘    ║
        ║        │                                │                   ║
        ║        ▼                                ▼                   ║
        ║   ┌──────────┐                  ┌────────────────────────┐  ║
        ║   │ Wails    │                  │ btProxy: per-peer       │  ║
        ║   │ runtime  │                  │ loopback forwarder      │  ║
        ║   │ (signal  │                  │ (127.0.0.1:rand)        │  ║
        ║   │  event)  │                  └──────────┬─────────────┘  ║
        ║   └────┬─────┘                             │                ║
        ║        │                                   ▼                ║
        ║        ▼                       ┌─────────────────────────┐  ║
        ║   ┌─────────────┐              │ anacrolix BT client     │  ║
        ║   │ Browser     │              │ (127.0.0.1:btport, uTP) │  ║
        ║   │ (Chromium   │              └─────────────────────────┘  ║
        ║   │  WKWebView) │                                           ║
        ║   │             │                                           ║
        ║   │ WebRTC      │   ───────── direct via Wi-Fi/STUN ───────►║─► peer's browser
        ║   │ PeerConn    │              (host + srflx candidates)    ║
        ║   └─────────────┘                                           ║
        ╚═════════════════════════════════════════════════════════════╝
```

**Ключевая идея:** весь UDP-трафик от peer'ов прилетает на один
сокет (hole-punched через camp announce + holepunch). Первый байт
говорит куда дальше: hole-punch keepalive, WebRTC signaling,
catalog state, или BT-трафик.

WebRTC media — отдельная история, идёт **напрямую** браузер ↔ браузер
по реальным сетевым интерфейсам через STUN. Туннель не задействован.

## Как проксируются пакеты

### Hole-punch (0xFF)

Один байт, отправляется каждые 1с (burst) / 25с (keepalive) на
известные peer'ы. Поддерживает NAT-маппинги. Никуда дальше не идёт —
обновляет `peer.lastSeenMs`.

### WebRTC signaling (0xF2)

SDP offer/answer + ICE candidates + side-channel hints (`screen-share`
on/off, `hangup`, и т.д.) сериализуются в JSON, оборачиваются в
`[0xF2][JSON]` UDP-пакет.

- **Outgoing**: JS зовёт `SendSignal(toTunnelIP, body)` (Wails-binding).
  Go находит UDP-эндпоинт peer'а, шлёт префикс+body на `c.udp`.
- **Incoming**: `recvLoop` видит `0xF2`, дёргает `c.OnSignal(from, body)`.
  Callback в `app.go` эмитит Wails-event `signal` → JS подписан через
  `EventsOn('signal', ...)`.

**Media** же НЕ идёт через нас. Браузерный `RTCPeerConnection` с
`iceServers: [stun.l.google.com:19302]` открывает свои собственные
UDP-сокеты, пробует host + srflx candidates, ICE сходится напрямую.
Если оба за симметричным NAT — без TURN не пробьётся (V1 ограничение).

### Catalog state-broadcast (0xF3)

Каждые ~20с каждый peer шлёт всем известным peer'ам JSON со списком
своих shared-файлов: `{name, files: [{name, size, info_hash, magnet}]}`.

- **Outgoing**: `stateBroadcastLoop` собирает `myFilesForBroadcast()`,
  marshal'ит, оборачивает в `[0xF3][JSON]`, рассылает всем peer'ам.
- **Incoming**: `recvLoop` видит `0xF3`, парсит JSON, кладёт в
  `peer.files`.
- **UI**: `Library()` binding возвращает плоский список (peer_name,
  peer_tunnel, file), drop-tab рендерит «camp library».

UDP-MTU ограничение ~60KB на один envelope. При превышении файлы
truncate'ятся в `broadcastState`.

### BitTorrent (uTP) — самый интересный кусок

Anacrolix не позволяет подсунуть кастомный `net.PacketConn`. Форкать
— тащить maintenance burden. Поэтому мы **userspace-проксируем**.

**Setup:**
1. anacrolix биндится на `127.0.0.1:<random>` (loopback only).
2. После `New()` зовём `tc.LocalPort()` — узнаём реальный port.
3. `btProxy.setAnacrolix("127.0.0.1:<btport>")` — записывает адрес.

**Outgoing — peer A инициирует download у peer'а B:**
1. UI: `StartDownload(magnet, peerB_udp_endpoint)`.
2. Backend `AddDownload`: для каждого peer-endpoint в списке зовёт
   `btProxy.forwarderAddrFor(peerB)`. Это либо возвращает существующий
   forwarder, либо **создаёт новый**:
   - `loopback := net.ListenUDP("127.0.0.1:0")` — свежий loopback-сокет.
   - Goroutine `relayFromAnacrolix()` читает из этого сокета и шлёт
     прочитанное `c.udp.WriteToUDP(pkt, peerB_addr)` (через
     hole-punched lite socket).
3. `forwarderAddrFor` возвращает `"127.0.0.1:<fwd_port>"` —
   адрес loopback-сокета forwarder'а.
4. anacrolix получает этот адрес как «peer для скачивания», шлёт ему
   uTP-handshake (на самом деле — пакет на forwarder'а на loopback).
5. Forwarder читает, релеит на `c.udp` → peerB через интернет
   (hole-punched UDP).

**Incoming — peer A качает уже идущий поток у peer'а B:**
1. peerB's anacrolix отвечает на handshake → шлёт пакет на
   `127.0.0.1:<fwd_port>` (loopback) внутри своего процесса.
2. На B's стороне: forwarder читает → шлёт через c.udp → A.
3. У peer A в `recvLoop`: пакет не префиксный (не 0xFF/0xF2/0xF3) →
   `btProxy.forwardFromPeer(peerB_addr, pkt)`.
4. btProxy находит forwarder для peerB (создал на шаге Outgoing),
   шлёт пакет на `127.0.0.1:<btport>` (anacrolix loopback).
5. Anacrolix получает, видит valid uTP-handshake response, продолжает.

**Когда peer B видит, что peer A инициирует подключение к нему
(сценарий "seeding"):**
- B получает первый uTP-handshake-пакет на `c.udp` от A.
- B's `recvLoop` → не префиксный → `btProxy.forwardFromPeer(peerA, pkt)`.
- btProxy создаёт forwarder для A на лету (если ещё нет), шлёт на
  anacrolix loopback.
- B's anacrolix видит входящий uTP, проверяет info_hash в handshake,
  находит соответствующий seed → принимает соединение.

```
Peer A side                               Peer B side
─────────────                             ─────────────
anacrolix                                 anacrolix
 │ uTP packet to 127.0.0.1:fwdB_port       ↑
 ▼                                         │ packet on 127.0.0.1:btport
btForwarder for B                         btForwarder for A
 │ reads, relays via lite UDP             ↑ reads, relays to btport
 ▼                                         │
c.udp.WriteToUDP(pkt, peerB_publicAddr)   c.udp recvLoop sees non-prefix
       │                                   ↑
       └──── hole-punched UDP ─────────────┘
              (single socket on each side, shared with
               holepunch / signal / state-broadcast)
```

**Что это даёт:**

- uTP едет по уже-hole-punched сокету → никаких отдельных NAT-
  пробивок для BT, симметричные NAT'ы и CGNAT'ы которые блочат
  отдельные BT-порты больше не проблема.
- anacrolix вообще не знает что peer не на 127.0.0.1 — он видит
  только loopback. Внутренняя логика BT не меняется.
- TCP отключен (`cfg.DisableTCP = true`) — оставлен только uTP,
  потому что только UDP проходит через мультиплексор.
- DHT / public-trackers / PEX отключены — discovery через `0xF3`
  state-frame, как и в mac.

## Отличие от `source/mac`

Mac-engine работает поверх kernel-level overlay:

| | mac | desktop |
|---|---|---|
| Транспорт | utun (kernel) + UDP-туннель | один UDP-сокет на peer |
| Root-привилегии | да (utun, pf, /etc/resolver, :80/:443) | нет |
| BT-протокол | TCP через утан | uTP (UDP) через btProxy |
| BT bind | `<tunnel_ip>:6881` | `127.0.0.1:<rand>` (только loopback) |
| WebRTC media | через утан (ICE rewrite на tunnel_ip) | напрямую браузер↔браузер (host/srflx) |
| WebRTC signaling | HTTP-through-tunnel `:2202/api/signal/inbox` | `0xF2` UDP-фрейм на lite-сокете |
| Catalog discovery | HTTP `/api/files` через утан, polling | `0xF3` state-broadcast UDP, periodic |
| Multiplexing | kernel routes IP-пакеты → engine packetloop | userspace по первому байту |
| Зона `<camp>.f2f` + reverse-proxy | да (`:80`/`:443`, local CA) | нет |
| Intercepts (per-host VPN) | да | нет |
| Egress NAT (общий VPN) | да | нет |
| Firewall (pf-anchor) | да (default-deny на utun) | нет (нет утана) |

Концептуально mac — **полноценный overlay** (имеет свою IP-подсеть,
ходит как обычное сетевое устройство), desktop — **application-level
overlay** (peer'ы общаются через user-space мультиплексор поверх
hole-punched UDP, никакой kernel-magic).

## Файлы

```
source/desktop/
├── main.go                       # Wails entry, окно 820x740 fixed
├── app.go                        # Wails-bindings: Start/Stop/Status,
│                                 #   SendSignal, MyFiles/AddMyFile*,
│                                 #   Library, StartDownload, Downloads,
│                                 #   Reveal
├── internal/
│   ├── lite/
│   │   ├── client.go             # rendezvous + hole-punch + recvLoop multiplex
│   │   ├── torrent.go            # BT client wrapper, state-broadcast,
│   │   │                         #   persistence, prune, refeed
│   │   └── btproxy.go            # per-peer loopback forwarders
│   ├── rendezvous/               # camp protocol — copy of source/mac/internal/rendezvous
│   └── torrent/torrent.go        # anacrolix wrapper, DisableTCP=true
└── frontend/
    ├── index.html                # 3 таба: camp / meet / drop
    └── src/
        ├── main.js               # camp tab + tab switching
        ├── meet.js               # WebRTC (call/answer/ICE, mic/cam/share,
        │                         #   chat data channel, dB-meter, volume)
        ├── drop.js               # BT UI (my files dropzone, library,
        │                         #   active downloads, reveal in Finder)
        └── style.css             # копия source/mac audio.css + layout
```

## Что ещё не работает / known gaps

- WebRTC через **симметричный NAT** обоих сторон — без TURN не пройдёт.
- BT-каталог большой (>~60KB JSON) — truncate'ится в state-frame.
  Решение если нужно: chunked state-frames или экспозиция через
  `/api/files`-стиль endpoint, но через signal/proxy.
- Desktop ↔ mac обмен файлами не работает — mac-engine ещё не умеет
  `0xF3` state-broadcast и BT-multiplex. Можно добавить, но пока нет.
- Идентичность peer'ов — по `name` в camp'е, никакой криптографии.
  Тот же gap что и в mac (см. TODO.md → Identity Phase 1).
