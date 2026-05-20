# f2f-mac

macOS-сторона проекта f2f. Запускает виртуальный сетевой интерфейс `utunN`,
поднимает локальный UDP-тоннель к другим участникам camp-а через
hole-punching, и даёт браузерную веб-морду для всего управления.

Тот же бинарь играет любой конец туннеля. В одном camp-е (виртуальная
overlay-подсеть `10.99.0.0/24`) могут жить несколько peer-ов одновременно.

## Что внутри

- **L3-тоннель** через `utun` + UDP, прозрачно гоняет IP-пакеты.
- **Camp** — рандеву-сервер на fly.io (`source/camp`). Каждый peer
  периодически шлёт UDP-announce, camp видит его public-эндпоинт,
  раздаёт списки. Hole-punching между peer-ами — направленные
  1-байтовые пакеты, держат NAT-мэппинги.
- **Sticky tunnel_ip** — `(camp_id, name)` → конкретный октет в
  `10.99.0.0/24`, хранится в Turso. Зашёл в кэмп под одним именем
  — всегда получишь тот же `10.99.0.X`.
- **Per-intercept маршрутизация** — каждый домен/IP/CIDR в списке
  intercepts привязан к конкретному peer-у. Можно гнать `gmail.com`
  через одного, `youtube.com` через другого.
- **Egress NAT** на принимающей стороне — `pf` anchor +
  `net.inet.ip.forwarding=1`. Авто-определяет default-route iface.
- **Локальный DNS-резолвер** для зоны `<camp_id>.f2f` — каждый peer
  публикует свои домены, остальные peer-ы их видят и резолвят на
  tunnel_ip владельца.
- **HTTP/HTTPS reverse-proxy** на стандартных портах `:80`/`:443`
  — открываешь `https://gitlab.<camp>.f2f` (без порта), engine
  парсит Host header / SNI и форвардит на нужный локальный порт.
- **Local CA + auto-trust** — каждый peer держит свой
  name-constrained CA для зоны `<camp_id>.f2f` (ECDSA P-256), CA-серты
  пирами обмениваются автоматически через `/api/ca-cert` и
  устанавливаются в системный keychain → **зелёный замок** в браузере.
- **Health-check** — engine TCP-dial'ит свои сервисы каждые 8с,
  статус (зелёная/красная точка) показывается в UI и шарится peer-ам.
- **WebRTC аудио + видео + screen share** между peer-ами поверх
  туннеля, без STUN/TURN.

## Зависимости

- macOS (тестировалось на Apple Silicon)
- Go 1.22+
- `sudo` для запуска (нужен root для utun, pf, /etc/resolver)

## Сборка и запуск

Через Makefile в корне репо:

```sh
make build       # собирает бинарь ./f2f-mac
make run         # sudo go run, UI на 127.0.0.1:2202
make kill        # прибить процессы f2f-mac
```

Или напрямую:

```sh
go build -o f2f-mac ./source/mac
sudo ./f2f-mac                          # UI на 127.0.0.1:2202 по умолчанию
sudo ./f2f-mac --bind 127.0.0.1:3333    # на другом порту
```

Открой `http://127.0.0.1:2202` — это все управление.

## UI

Пять вкладок:

### `camp`

Identity + peers. Пишешь свой **name** и **camp id**, жмёшь **Start** в
правом углу — engine стартует, регистрируется на `f2f-camp.fly.dev`,
получает свой `tunnel_ip`, начинает hole-punching ко всем остальным
peer-ам в этом camp-е. Таблица peers показывает их с точкой статуса:
зелёная — пакеты ходят, красная — peer online но не отвечает на punch,
серая — peer offline, жёлтая — это ты сам.

### `tunnel`

Intercepts. Добавляешь `gitlab.com` или `1.2.3.4/24` и выбираешь к
**какому peer-у** этот трафик отправлять. Engine ставит host-маршруты
через утун, при попадании пакета в утун — отправляет UDP'ом выбранному
peer-у. У них поднимется egress, и пакет уйдёт в публичный интернет от
их имени.

Под ним — счётчики `tx packets / tx bytes / rx packets / rx bytes`
(в одну строку) и `log`.

### `dns`

Публикуешь свои сервисы как **локальные имена** в зоне
`<camp_id>.f2f`. Добавил `gitlab` + port `3000` — у всех peer-ов в
твоём camp-е резолвится `gitlab.<camp_id>.f2f`:
- **`http://gitlab.<camp_id>.f2f`** (без порта) — попадает на engine'ов
  reverse-proxy на `:80`, форвардится на твой локальный `:3000`.
- **`https://gitlab.<camp_id>.f2f`** — engine на `:443` терминирует TLS
  leaf-сертом подписанным local-CA (зелёный замок), форвардит plain
  HTTP на `:3000`.
- **`http://gitlab.<camp_id>.f2f:3000`** — прямой проход в утун,
  proxy не задействован.

Точка слева от каждого домена — health: зелёная = backend отвечает на
TCP-dial 127.0.0.1:port, красная = не отвечает, серая = не проверено.
Этот же статус peer'ы видят у твоих доменов через polling.

Под ним — known domains (опубликованное другими peer-ами) и trusted
peer CAs (список CA-сертов соседей которые установлены в твой
системный keychain как trusted roots).

### `meet`

WebRTC прямо peer-to-peer через туннель. Без STUN/TURN. Выбираешь
peer-а из дропдауна, жмёшь call. Поддерживает:

- Голос + видео (`getUserMedia`).
- Screen share (`getDisplayMedia`).
- dB-meter (свой mic слева, peer справа).
- Fullscreen на любой панели.
- Чат через WebRTC data channel.
- Несколько панелей с горизонтальной прокруткой.

### `drop`

Пока заглушка.

## Headless-режим `run`

Если UI не нужен (сервер без графики, скрипт):

```sh
sudo ./f2f-mac run --listen :9000 \
  --camp-url wss://f2f-camp.fly.dev/ws \
  --name vasya --id beer
```

| флаг | описание |
| --- | --- |
| `--name` | твоё имя в camp-е |
| `--id` | shared camp id |
| `--camp-url` | URL camp-сервера, по умолчанию `wss://f2f-camp.fly.dev/ws` |
| `--camp-stun` | host:port для STUN-наблюдения, дефолт `f2f-camp.fly.dev:3478` |
| `--listen` | UDP-порт для туннеля (`:9000`) |
| `--egress-iface` | физический интерфейс для NAT'а (пусто = авто-детект default route) |
| `--local-ip` / `--peer-ip` | placeholder'ы, camp всё равно их перепишет |

Intercepts и доменные имена в headless'е недоступны — добавляются только через UI.

## Что делает engine в системе

При старте (в camp-режиме):

1. Открывает utun, ставит на него адрес из camp-а (`10.99.0.X`).
2. Биндит UDP-сокет на `:9000`.
3. Шлёт announce на `f2f-camp.fly.dev:3478` UDP'ом.
4. Поднимает HTTP UI на `127.0.0.1:2202` (loopback) + узкий tunnel-listener на `<tunnel_ip>:2202` (`POST /api/signal/inbox` + `GET /api/domains` + `GET /api/ca-cert`).
5. Поднимает локальный DNS на `127.0.0.1:5354` + пишет `/etc/resolver/<camp_id>.f2f`.
6. **Local CA**: если в `/var/lib/f2f/ca/` нет cert'а для текущего camp'а — генерит ECDSA P-256 root с `permittedDNSDomains: .<camp_id>.f2f` (Name Constraints). Устанавливает в `/Library/Keychains/System.keychain` через `security add-trusted-cert` (**один раз** при первой установке — macOS спросит пароль).
7. **HTTP/HTTPS proxy**: биндит `:80` и `:443` на `<tunnel_ip>` и `127.0.0.1`. HTTPS использует leaf-сертификаты, генерируемые на лету CA per-SNI.
8. Включает egress NAT: `pf` anchor `com.apple/f2f-mac` с правилом `nat on en0 from 10.99.0.0/24 to any -> (en0)`, плюс `sysctl net.inet.ip.forwarding=1`. Старое значение forwarding сохраняется в `/var/run/f2f-mac.egress.json` для отката.
9. Запускает воркеры:
   - **hole-punch** (1Hz burst / 25s keepalive),
   - **camp peer-list poll** (30с),
   - **domain poll** (10с — узнаём что peer'ы публикуют),
   - **peer-CA poll** (30с — pull `/api/ca-cert` соседей, install в keychain если новый),
   - **domain health check** (8с — TCP-dial своих сервисов),
   - **peer-to-tun**, **tun-to-peer**.

На выходе всё аккуратно откатывается в обратном порядке. Если `kill -9` — следующий запуск увидит state-файл и подберёт хвост. CA-серты и trusted-peer-CA серты на диске **остаются** между запусками — кэш fingerprint'ов гарантирует что second-run не дёргает пароль-prompt лишний раз.

## Manual rescue (если что-то совсем пошло не так)

```sh
sudo pfctl -a com.apple/f2f-mac -F all
sudo sysctl -w net.inet.ip.forwarding=0      # если у тебя было 0 до запуска
sudo rm -f /var/run/f2f-mac.egress.json
sudo rm -f /etc/resolver/<camp_id>.f2f
# Снести нашу CA и доверенные peer CA из keychain'а (если хочешь начать с нуля):
sudo security delete-certificate -c "f2f Local CA · <camp_id>" /Library/Keychains/System.keychain
sudo rm -rf /var/lib/f2f/ca /var/lib/f2f/trusted-peers
```

`pfctl -E` reference-counted token можно проверить через `sudo pfctl -s References`.

## Архитектура (один peer)

```
                              camp.fly.dev
                              ┌──────────┐
        announce  ─UDP/3478─► │          │
        peer list ◄─HTTP/443─ │          │
                              └──────────┘
                                   │
            ╔══════════════════════╪══════════════════════╗
            ║       f2f-mac на этой машине                ║
            ║                                             ║
            ║  ┌──────────┐         ┌──────────────────┐  ║
            ║  │ engine   │ ◄──────►│ UDP :9000        │ ║─── public internet
            ║  │          │         └──────────────────┘  ║    (hole-punched
            ║  │          │                               ║     к другим peer-ам)
            ║  │          │ ◄──────► utun7 (10.99.0.2)    ║
            ║  │          │                               ║
            ║  │          │ ◄──────► dns :5354 (127.0.0.1)║
            ║  │          │                               ║
            ║  │          │ ◄──────► HTTP  :80            ║
            ║  │          │           HTTPS :443          ║
            ║  │          │           (reverse-proxy →    ║
            ║  │          │            127.0.0.1:port)    ║
            ║  └──────────┘                               ║
            ║       ▲                                     ║
            ║       │ HTTP                                ║
            ║       ▼                                     ║
            ║  ┌──────────┐                               ║
            ║  │ web UI   │ :2202 (127.0.0.1 + tunnel_ip) ║
            ║  └──────────┘                               ║
            ╚═════════════════════════════════════════════╝
```

## Полезные команды

```sh
ifconfig | grep -A2 utun                     # утуны и их адреса
netstat -rn | grep 10.99                     # маршруты в overlay-подсеть
sudo pfctl -a com.apple/f2f-mac -s nat       # NAT-правила в нашем anchor
sudo tcpdump -i utun7 -n -vv                 # пакеты на утуне
sudo tcpdump -i en0 -n udp port 9000         # UDP по сети
dig @127.0.0.1 -p 5354 gitlab.<camp>.f2f     # ручная проверка DNS
curl http://127.0.0.1:2202/api/status        # текущий снапшот engine
```

## API-эндпоинты (для интеграций)

Loopback (`127.0.0.1:2202`):

| метод | путь | назначение |
| --- | --- | --- |
| GET | `/api/status` | снапшот engine |
| POST | `/api/start` | старт (тело: `{camp_name, camp_id}`) |
| POST | `/api/stop` | остановка |
| GET | `/api/camp/peers` | список peer-ов в camp-е |
| POST | `/api/peers/active` | выбрать peer для meet-сигналинга |
| POST | `/api/intercepts` | добавить intercept (`{spec, peer}`) |
| DELETE | `/api/intercepts/{id}` | удалить intercept |
| GET | `/api/my-domains` | мои опубликованные домены (с health) |
| PUT | `/api/my-domains` | заменить список (тело — массив) |
| GET | `/api/ca-cert` | мой local-CA в PEM (peer'ы поллят) |
| GET | `/api/trusted-peers` | список доверенных peer-CA |
| GET | `/api/log/stream` | SSE лог engine |
| POST | `/api/signal/{outbox,inbox}` | WebRTC сигналинг |
| GET | `/api/signal/stream` | SSE сигналов для браузера |

Tunnel listener (`<tunnel_ip>:2202`, поднимается с engine):

| метод | путь | назначение |
| --- | --- | --- |
| POST | `/api/signal/inbox` | приём WebRTC-сигналов от peer-а |
| GET | `/api/domains` | мои домены — peer-ы поллят его |
| GET | `/api/ca-cert` | мой CA — peer-ы поллят чтобы trust'нуть |

HTTP/HTTPS reverse-proxy (на `<tunnel_ip>:80/:443` и `127.0.0.1:80/:443`):

любой `http(s)://<name>.<camp_id>.f2f/...` запрос парсится по Host/SNI,
ищется в `MyDomains`, форвардится на `127.0.0.1:<configured_port>`.

## Тесты

```sh
go test ./...
```

## Что осталось

- IPv6 на utun + IPv6 NAT (сейчас IPv6 утекает мимо туннеля).
- Sleep/wake recovery (macOS-уход в сон порой ломает NAT-state, см. TODO.md).
- Линукс/винда стороны.
- Groupcall (mesh → Pion SFU, см. TODO.md).
- Per-peer Name Constraints в CA (сейчас CA каждого peer'а в camp-е
  может выпустить cert для любого домена в зоне — friends-circle OK,
  но for-strangers — нужно ограничить чтобы CA peer-а A не мог
  подписать сертификат для домена B).
