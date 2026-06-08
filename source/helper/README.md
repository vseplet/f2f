# f2f-mac

macOS-сторона проекта f2f. Запускает виртуальный сетевой интерфейс `utunN`,
поднимает локальный UDP-тоннель к другим участникам camp-а через
hole-punching, и даёт браузерную веб-морду для всего управления.

Тот же бинарь играет любой конец туннеля. В одном camp-е (виртуальная
overlay-подсеть `100.64.0.0/10`) могут жить несколько peer-ов одновременно.

## Что внутри

- **L3-тоннель** через `utun` + UDP, прозрачно гоняет IP-пакеты.
- **Camp** — рандеву-сервер на fly.io (`source/camp`). Каждый peer
  периодически шлёт UDP-announce, camp видит его public-эндпоинт,
  раздаёт списки. Hole-punching между peer-ами — направленные
  1-байтовые пакеты, держат NAT-мэппинги.
- **Детерминированный overlay-IP** — адрес peer-а в `100.64.0.0/10`
  выводится прямо из его Ed25519 pub (`sha256(pub) → 100.64.X.Y`,
  см. `engine.PubToV4Addr`). Никакого аллокатора и состояния на
  camp-е: один и тот же ключ всегда даёт тот же tunnel_ip и переживает
  рестарты сам по себе.
- **Управление camp'ами — в CLI** (пакет `cli`). `sudo f2f` без
  аргументов показывает интерактивный picker (на `charmbracelet/huh`):
  выбрать известный camp, создать новый или войти в существующий по
  camp_id. Отдельные подкоманды: `f2f camp ls|new|join|use|rm`. Движок к
  camp-менеджменту отношения не имеет — ему отдают готовые config+identity.
- **Per-camp identity** — на каждый camp_id своя Ed25519-пара
  (`/var/lib/f2f/identity/<camp_id>/{priv,pub}.key`). При создании camp'а
  её генерит CLI и зеркалит pub/fingerprint в `<camp_id>/config.json`;
  присоединяясь к чужому camp'у, гость генерит свою пару при первом старте.
- **Per-camp config** — все user-editable настройки (identity с alias и
  публичным ключом, intercepts, my-domains, firewall, trusted-peer
  fingerprints, peer catalog с last-known доменами и open-портами
  каждого пира) лежат в `$HOME/.f2f/<camp_id>/config.json`.
  Глобальный `$HOME/.f2f/state.json` хранит `last_camp_id` и список
  известных camp'ов.
- **Autostart** — `f2f up` (или запуск без TTY: launchd/скрипт) молча
  поднимает `last_camp_id` из `state.json`, без picker'а.
- **Per-intercept маршрутизация** — каждый домен/IP/CIDR в списке
  intercepts привязан к конкретному peer-у. Можно гнать `gmail.com`
  через одного, `youtube.com` через другого.
- **Egress NAT** на принимающей стороне — `pf` anchor +
  `net.inet.ip.forwarding=1`. Авто-определяет default-route iface.
- **Inbound firewall** на utun — default-deny через `pf` anchor, scope'нутый
  на пакеты к **нашему** tunnel_ip (egress-форвардинг и peer-to-peer трафик
  через нас не трогает). F2F-внутренние порты (`2202/tcp`, `80/tcp`,
  `443/tcp`, `6881/tcp+udp`) всегда открыты, остальные доступны только если
  пользователь явно открыл в Tunnel-табе. Любой сервис который слушает на
  `0.0.0.0` (sshd, postgres, dev-серверы) по умолчанию **не достижим** через
  `<tunnel_ip>`.
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
- **WebRTC аудио + видео + screen share** — 1:1 звонки (meet) и
  групповые через встроенный Pion SFU (meet/2), поверх туннеля, без STUN/TURN.
- **Remote terminal (shells)** — mosh-подобный терминал на машину camp-а,
  прямо в браузерной морде (xterm.js). PTY гоняется по QUIC-шине (не HTTP),
  сессия **живёт на хосте** независимо от клиента: reload/сон → реаттач с
  перерисовкой экрана из ring-буфера (без «фарша»). По умолчанию шелл
  стартует через системный `login` (парольная аутентификация ОС).
- **Drop — file sharing через BitTorrent** в пределах camp-а. Каждый
  peer держит свой каталог раздачи (`anacrolix/torrent`, без DHT и
  публичных tracker'ов, чистый peer-wire протокол). Скачивание
  multi-source — если файл seed'ят 3 peer'а, качаешь параллельно от
  всех. Auto-seed скачанного.

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
go build -o f2f ./source/helper
sudo ./f2f                              # picker camp'а, затем UI на 127.0.0.1:2202
sudo ./f2f --bind 127.0.0.1:3333        # UI на другом порту
sudo ./f2f up                           # без picker: поднять последний camp
```

### Управление camp'ами (CLI)

```sh
sudo ./f2f camp ls                          # список известных camp'ов (★ = последний)
sudo ./f2f camp new family --name vasya     # создать camp (без аргументов — спросит интерактивно)
sudo ./f2f camp join <camp_id> --name vasya # войти в существующий camp по camp_id
sudo ./f2f camp use family                  # сделать camp последним
sudo ./f2f camp rm family                   # забыть camp (удаляет ключи + данные)
```

После выбора camp'а открой `http://127.0.0.1:2202` — там всё **прикладное**
управление (домены, файлы, звонки, intercepts). Создание/переключение
camp'ов через UI больше не делается — только CLI.

## UI

Пять вкладок:

### `camp`

Identity + peers — **только для чтения**. Создание, переключение и
остановка camp'ов живут в CLI (`sudo f2f` / `f2f camp …`), не в UI.

**Когда движок остановлен** — подсказка «no camp running» со списком
CLI-команд.

**Когда движок работает** — key:value readout: твой `name`, `camp id`,
`tunnel ip`, наблюдаемый camp-сервером `endpoint` (public_ip:port),
полный `public key` (кликаемый — копирует hex в буфер) + короткий
fingerprint.

Таблица peers показывает соседей с точкой статуса: зелёная — пакеты
ходят, красная — peer online но не отвечает на punch, серая — peer
offline, жёлтая — это ты сам. Peer'ы которых движок когда-либо видел в
camp'е сохраняются между перезапусками (поле `peer_catalog` в config-файле).

### `tunnel`

Intercepts. Добавляешь `gitlab.com` или `1.2.3.4/24` и выбираешь к
**какому peer-у** этот трафик отправлять. Engine ставит host-маршруты
через утун, при попадании пакета в утун — отправляет UDP'ом выбранному
peer-у. У них поднимется egress, и пакет уйдёт в публичный интернет от
их имени.

Под ним — **open ports**: default-deny inbound на утане. F2F-внутренние
порты (`2202/tcp`, `80/tcp`, `443/tcp`, `6881/tcp+udp`) показаны сверху
с заблокированным чекбоксом — built-in, нельзя выключить. Ниже — твои
кастомные правила: добавил `22 tcp ssh` → peer'ы могут `ssh` к тебе.
Чекбокс «вкл/выкл» оставляет запись в списке но временно отзывает
доступ. Сервисы которые ты публикуешь через DNS+443 (Gitea, Gitlab и
т.п.) **здесь открывать не надо** — reverse-proxy на `:443` уже built-in,
он сам форвардит на твой локальный port.

Под ним — **peer open ports**: что **другие** peer'ы в camp'е явно
открыли у себя (без built-in'ов). Дёргается раз в 30с с peer'овского
`/api/firewall` через туннель, кэшируется в `peer_catalog`, переживает
рестарт. Точка статуса tri-state: зелёная — peer online и правило
включено, красная — online но peer держит правило disabled, серая —
peer offline.

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

Поле **host** опционально — default `127.0.0.1`. Можно указать
`localhost` (для Node ≥17 dev-серверов с IPv6-only bind'ом), или
IP/имя другой машины в LAN (`192.168.1.5`, `nas.local`) если хочешь
выставить наружу её сервис под camp-доменом.

Точка слева от каждого домена — health: зелёная = backend отвечает на
TCP-dial `<host>:port`, красная = не отвечает, серая = не проверено.
Этот же статус peer'ы видят у твоих доменов через polling.

Под ним — **known domains** (опубликованное другими peer-ами). Список
кэшируется в `peer_catalog.domains` и переживает рестарт engine'а —
если poll временно фейлится (network blip), последний known-state
остаётся видимым. Точка tri-state: зелёная — peer online + health=ok,
красная — peer online + health=fail (peer сам сообщает что сервис
лежит), серая — peer offline. У каждой строки кнопка `remove` —
сносит запись из локального каталога (если peer ещё публикует это
имя — следующий poll вернёт обратно, удаление полезно для stale-
записей от ушедших навсегда peer'ов).

Ниже — **trusted peer CAs** (CA-серты соседей установленные в твой
системный keychain как trusted roots, кнопка `remove` сносит из PEM
+ keychain + camp config).

### `meet`

1:1 WebRTC прямо peer-to-peer через туннель. Без STUN/TURN. Выбираешь
peer-а из дропдауна, жмёшь call. Поддерживает:

- Голос + видео (`getUserMedia`).
- Screen share (`getDisplayMedia`).
- dB-meter (свой mic слева, peer справа).
- Fullscreen на любой панели.
- Чат через WebRTC data channel.
- Несколько панелей с горизонтальной прокруткой.

### `meet/2`

Групповые звонки через встроенный Pion SFU. Один участник создаёт
звонок (его engine становится SFU-хостом), остальные находят его через
polling и подключаются. Весь медиа-трафик идёт через tunnel overlay.

- **Аудио + видео + screen share** — от каждого участника к каждому.
- **Кнопки mic/cam** — раздельное управление, disabled если устройства нет.
  Зелёная подсветка при активном состоянии.
- **Fullscreen** — кнопка `⛶` на каждом видео-тайле (hover).
- **Screen share** — `getDisplayMedia`, локальный preview-тайл,
  трансляция через SFU всем участникам.
- **Чат** — DataChannel relay через SFU.
- **Dropdown групп** — показывает все активные звонки (свой + чужие),
  можно переключаться. При leave сбрасывается на «— group —».
- **Auto-rejoin** — если хост перезагрузил страницу, автоматически
  возвращается в свой звонок.
- **Leave (host)** — кнопка показывает «(host)» если ты хост звонка.
- **ICE**: хост ↔ свой SFU — через loopback (все интерфейсы),
  удалённые пиры ↔ SFU — строго через utun (tunnel only).

### `drop`

P2P file sharing внутри camp-а через BitTorrent.

- **My shared files**: drag-and-drop файл в зону → файл копируется в
  `~/Library/Application Support/f2f/shared/` и начинает seed'иться
  через BT-клиент на `<tunnel_ip>:6881`. Клик на имени открывает
  файл в Finder. Remove снимает с раздачи (файл на диске остаётся).
- **Camp library**: всё что seed'ят другие peer-ы (раз в минуту
  опрашивается `/api/files` у online-peer'ов). У записи которую
  **уже скачал** — имя кликабельное (Finder) + зелёный pill
  `downloaded`/`seeding`. Если качается — pill `%`. Иначе — кнопка
  download.
- **Active downloads**: только in-progress. Когда файл досскачался —
  уходит в library с pill'ом seeding.

Скачанное лежит в `~/Downloads/f2f-drops/`, владелец — твой юзер
(не root, engine сам chown'ит). Auto-seeding включён — скачанный
файл продолжает раздаваться следующим peer'ам (multi-source).

**Persistence**: список раздач (`shared/`) и список скачанных файлов
(`downloads.json`) переживают рестарт engine'а. После рестарта —
автоматически re-seed + re-pickup, ничего повторно тянуть не надо.

**Recovery**: если peer-source перезапустился во время загрузки или
ушёл в оффлайн и вернулся — engine детектит stall (нет прогресса
>90с) и делает drop+re-add магнита, anacrolix переподключается с
сохранением скачанных piece'ов. Удалил файл руками в Finder —
engine это видит и через ~30с снимает запись из UI.

DHT, публичные tracker'ы и PEX отключены — discovery только через
наш `/api/files` endpoint. IPv6 listen отключен (utun у нас
IPv4-only).

### `shells`

Терминалы на машины camp-а (сервис `services/shell`). В сайдбаре —
секция **shells** со списком пиров, у кого shell-сервер открыт для тебя
(опрашиваются по шине, `shell.status`). Клик → таб с xterm.js-терминалом.

- **Транспорт** — PTY по QUIC-шине (`:2203`), не HTTP. Браузер говорит со
  **своим** локальным f2f по WebSocket, тот мостит в bus-стрим к пиру.
  Файрвол трогать не надо — едет по уже открытой шине.
- **Переживает сон/reload** — `session_id` персистится в браузере, PTY
  живёт на хосте. Переподключился → сервер шлёт `clear` + ring-буфер
  (текущий экран), а не повтор байт-стрима. Окно `⛶` — fullscreen.
- **kill session** — реально гасит PTY (процесс-группу) на хосте. Просто
  уйти со вкладки = detach (сессия остаётся, реаттачишься позже).
- **Авторизация** — по умолчанию `login` (юзер+пароль ОС). Доступ к
  сервису гейтится политикой `shell` в `<camp_id>/config.json`
  (`enabled` + allowlist пабов); сейчас для теста permissive-дефолт.

## Headless / автозапуск

Если интерактивный picker не нужен (сервер без TTY, login-item, скрипт):

```sh
sudo ./f2f up                 # поднять последний camp (last_camp_id) без picker'а
sudo ./f2f up --bind 127.0.0.1:3333
```

Без TTY (launchd, пайп) `sudo ./f2f` и так не показывает picker, а молча
берёт последний camp — `up` явно форсит это поведение. Если последнего
camp'а нет, процесс поднимет только UI и будет ждать; camp создаётся/
выбирается через `f2f camp …`.

Camp-endpoint'ы (`server_url`, `stun_addr`) живут в `<camp_id>/config.json`
и правятся руками; intercepts и доменные имена — через UI.

## Что делает engine в системе

При старте (в camp-режиме):

1. CLI (`cli.SelectCamp` / `f2f camp …`) выбирает camp, читает `$HOME/.f2f/<camp_id>/config.json` (создаёт при `camp new`), грузит identity и **отдаёт движку готовые `config.Camp` + `identity`**. CLI же апсертит `state.json` (`last_camp_id`, `known_camps`). Движок ничего из этого не создаёт и на диск не пишет.
2. Ed25519 keypair под `/var/lib/f2f/identity/<camp_id>/` (приватник 0600, паблик 0644): генерит CLI при `camp new` (или гость — при первом старте), pub+fingerprint зеркалятся в `<camp_id>/config.json`. intercepts, my-domains, firewall, trusted-peer fingerprints движок берёт из переданного config'а.
3. Открывает utun, ставит на него адрес из camp-а (`100.64.0.X`).
4. Биндит UDP-сокет на `:9000`.
5. Шлёт announce на `f2f-camp.fly.dev:3478` UDP'ом.
6. Поднимает HTTP UI на `127.0.0.1:2202` (loopback) + узкий tunnel-listener на `<tunnel_ip>:2202` (`POST /api/signal/inbox` + `GET /api/domains` + `GET /api/ca-cert`).
7. Поднимает локальный DNS на `127.0.0.1:5354` + пишет `/etc/resolver/<camp_id>.f2f`, флашит mDNSResponder-кэш (`dscacheutil -flushcache` + `killall -HUP mDNSResponder`) чтобы старые NXDOMAIN не висели. NXDOMAIN-ответы DNS-сервера несут SOA с `minttl=1` — негативный кэш живёт максимум секунду.
8. **Local CA**: если в `/var/lib/f2f/ca/` нет cert'а для текущего camp'а — генерит ECDSA P-256 root с `permittedDNSDomains: .<camp_id>.f2f` (Name Constraints). Устанавливает в `/Library/Keychains/System.keychain` через `security add-trusted-cert` (**один раз** при первой установке — macOS спросит пароль).
9. **HTTP/HTTPS proxy**: биндит `:80` и `:443` на `<tunnel_ip>` и `127.0.0.1`. HTTPS использует leaf-сертификаты, генерируемые на лету CA per-SNI.
10. Включает egress NAT: `pf` anchor `com.apple/f2f-mac` с правилом `nat on en0 from 100.64.0.0/10 to any -> (en0)`, плюс `sysctl net.inet.ip.forwarding=1`. Старое значение forwarding сохраняется в `/var/run/f2f-mac.egress.json` для отката.
11. Поднимает **inbound firewall** на утане: `pf` anchor `com.apple/f2f-mac-fw` с `block in on utunN to <our_tunnel_ip>/32 all` + `pass`-правилами для built-in портов (`2202/tcp`, `80/tcp`, `443/tcp`, `6881/tcp+udp`) и любых пользовательских из `<camp_id>/config.json` → `firewall`. Scope'нут на наш tunnel_ip — egress-форвардинг и трафик к другим peer'ам не трогает. Сохраняет pf-state в `/var/run/f2f-mac.firewall.json` для отката.
12. **BitTorrent client** (anacrolix): биндит `<tunnel_ip>:6881` (TCP), без DHT/PEX/публичных tracker'ов, с `Seed = true` и `DisableIPv6 = true`. Раздача из `~/Library/Application Support/f2f/shared/`, скачивание в `~/Downloads/f2f-drops/`.
13. Запускает воркеры:
    - **hole-punch** (1Hz burst / 25s keepalive),
    - **camp peer-list poll** (30с),
    - **domain poll** (10с — узнаём что peer'ы публикуют),
    - **peer-CA poll** (30с — pull `/api/ca-cert` соседей, install в keychain если новый),
    - **files poll** (60с — pull `/api/files` соседей, кэшируем their seed catalog),
    - **peer firewall poll** (30с — pull `/api/firewall`, кэшируем user-list соседей в catalog),
    - **domain health check** (8с — TCP-dial своих сервисов),
    - **peer-to-tun**, **tun-to-peer**.

На выходе всё аккуратно откатывается в обратном порядке. Если `kill -9` — следующий запуск увидит state-файл и подберёт хвост. CA-серты и trusted-peer-CA серты на диске **остаются** между запусками — кэш fingerprint'ов гарантирует что second-run не дёргает пароль-prompt лишний раз.

## Где что хранится

| путь | назначение | владелец |
| --- | --- | --- |
| `$HOME/.f2f/state.json` | `last_camp_id` + список известных camp'ов для дропдауна | юзер |
| `$HOME/.f2f/<camp_id>/config.json` | per-camp: identity (alias + pub), intercepts, my-domains, firewall, trusted-peer fingerprints, peer catalog (с domains/firewall каждого пира) | юзер |
| `/var/lib/f2f/identity/<camp_id>/{priv,pub}.key` | Ed25519 keypair per camp (priv 0600) | root |
| `/var/lib/f2f/ca/{cert,key}.pem` | наш local CA (ECDSA P-256, name-constrained на `.<camp_id>.f2f`) | root |
| `/var/lib/f2f/trusted-peers/<peer_name>.crt` | PEM-копии peer CA (метаданные мирорятся в `<camp_id>/config.json`) | root |
| `/Library/Keychains/System.keychain` | trust для нашего CA и peer CA (через `security add-trusted-cert`) | system |
| `/etc/resolver/<camp_id>.f2f` | macOS-резолвер: `nameserver 127.0.0.1` + `port 5354` | root |
| `~/Library/Application Support/f2f/shared/` | файлы которые ты seed'ишь | юзер |
| `~/Downloads/f2f-drops/` | скачанные через camp library | юзер |
| `~/Library/Application Support/f2f/downloads.json` | список магнитов для re-seed после рестарта | юзер |
| `/var/run/f2f-mac.{egress,firewall}.json` | runtime pf-state для отката | root |

## Manual rescue (если что-то совсем пошло не так)

```sh
sudo pfctl -a com.apple/f2f-mac -F all
sudo pfctl -a com.apple/f2f-mac-fw -F all
sudo sysctl -w net.inet.ip.forwarding=0      # если у тебя было 0 до запуска
sudo rm -f /var/run/f2f-mac.egress.json /var/run/f2f-mac.firewall.json
sudo rm -f /etc/resolver/<camp_id>.f2f
# Снести нашу CA и доверенные peer CA из keychain'а (если хочешь начать с нуля):
sudo security delete-certificate -c "f2f Local CA · <camp_id>" /Library/Keychains/System.keychain
sudo rm -rf /var/lib/f2f/ca /var/lib/f2f/trusted-peers
# Снести identity (приватные ключи) и весь user-state (intercepts, my-domains, ...):
sudo rm -rf /var/lib/f2f/identity
rm -rf ~/.f2f
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
            ║  │          │ ◄──────► utun7 (100.64.0.2)    ║
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
netstat -rn | grep 100.64                     # маршруты в overlay-подсеть
sudo pfctl -a com.apple/f2f-mac -s nat       # NAT-правила в нашем anchor
sudo tcpdump -i utun7 -n -vv                 # пакеты на утуне
sudo tcpdump -i en0 -n udp port 9000         # UDP по сети
dig @127.0.0.1 -p 5354 gitlab.<camp>.f2f     # ручная проверка DNS
curl http://127.0.0.1:2202/api/status        # текущий снапшот engine
```

## API-эндпоинты (для интеграций)

Управление жизненным циклом camp'а — в CLI, не по HTTP: эндпоинты
`/api/start`, `/api/stop`, `/api/camps`, `/api/camp/{id}` удалены
(используй `f2f camp …`).

Loopback (`127.0.0.1:2202`):

| метод | путь | назначение |
| --- | --- | --- |
| GET | `/api/status` | снапшот engine (включая `identity_pub`/`identity_fp`) |
| GET | `/api/camp/peers` | список peer-ов в camp-е |
| POST | `/api/peers/active` | выбрать peer для meet-сигналинга |
| POST | `/api/intercepts` | добавить intercept (`{spec, peer}`) |
| DELETE | `/api/intercepts/{id}` | удалить intercept |
| GET | `/api/my-domains` | мои опубликованные домены (с health) |
| DELETE | `/api/peer-domains/{peer}/{name}` | снести запись из peer-catalog (если peer ещё публикует — следующий poll вернёт) |
| PUT | `/api/my-domains` | заменить список (тело — массив) |
| GET | `/api/ca-cert` | мой local-CA в PEM (peer'ы поллят) |
| GET | `/api/trusted-peers` | список доверенных peer-CA |
| DELETE | `/api/trusted-peers/{fp}` | снести peer CA (PEM + keychain + camp config) |
| GET | `/api/firewall` | built-in + user open-ports |
| PUT | `/api/firewall` | заменить user open-ports (тело: `{user:[{port,protocol,description,enabled},...]}`) |
| GET | `/api/files/mine` | мои seed'ящиеся файлы (с локальным path) |
| POST | `/api/files/mine` | добавить файл по path (`{path}`) |
| POST | `/api/files/mine/upload` | multipart upload в shared-каталог + auto-seed |
| DELETE | `/api/files/mine/{hash}` | снять с раздачи (файл на диске остаётся) |
| POST | `/api/files/download` | начать download (`{magnet, peers[]}`) |
| GET | `/api/files/downloads` | прогресс активных/завершённых (поля complete/seeding/path/fetching_metadata) |
| DELETE | `/api/files/downloads/{hash}` | кенселит download + сносит из `downloads.json` |
| POST | `/api/files/reveal` | открыть `<path>` в Finder (только под shared/ или downloads/) |
| GET | `/api/log/stream` | SSE лог engine |
| POST | `/api/signal/{outbox,inbox}` | WebRTC сигналинг |
| GET | `/api/signal/stream` | SSE сигналов для браузера |
| GET | `/api/shell/peers` | пиры, у кого remote-shell открыт нам (опрос по шине) |
| GET | `/api/shell/ws` | WebSocket-мост браузер↔bus-стрим к PTY пира (`?peer=&session=&cols=&rows=`) |

Tunnel listener (`<tunnel_ip>:2202`, поднимается с engine):

| метод | путь | назначение |
| --- | --- | --- |
| POST | `/api/signal/inbox` | приём WebRTC-сигналов от peer-а |
| GET | `/api/domains` | мои домены — peer-ы поллят его |
| GET | `/api/ca-cert` | мой CA — peer-ы поллят чтобы trust'нуть |
| GET | `/api/files` | мои seed'ящиеся файлы (для cross-peer discovery) |
| GET | `/api/firewall` | мои user-открытые порты — peer-ы поллят для секции "peer open ports" |
| GET | `/api/call/state` | состояние локального группового звонка (для discovery) |
| POST | `/api/call/signal` | SFU WebRTC signaling (offer/answer/candidate/renegotiate) |
| POST | `/api/call/join` | присоединение к групповому звонку (от удалённого пира) |
| POST | `/api/call/leave` | выход из группового звонка (от удалённого пира) |

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
- Groupcall — улучшения meet/2 (более 3 участников, UI polish).
- Per-peer Name Constraints в CA (сейчас CA каждого peer'а в camp-е
  может выпустить cert для любого домена в зоне — friends-circle OK,
  но for-strangers — нужно ограничить чтобы CA peer-а A не мог
  подписать сертификат для домена B).
- Drop ACL — каждый shared-файл может ограничиваться списком peer'ов
  которым он виден/доступен (см. Stage 4 в TODO.md).
- Per-peer firewall ACL — сейчас открытый порт открыт для всего camp'а
  одинаково; ограничение типа «SSH только Bob'у» требует identity-layer'а
  (см. TODO.md, Identity Phase 1).
