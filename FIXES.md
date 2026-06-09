# Требуемые правки (по итогам ревью 2026-06)

Список отсортирован по приоритету. Пометка **[→QUIC]** — правка
меняет форму или вовсе отпадает после миграции межсервисной
коммуникации с HTTP-поверх-туннеля на QUIC bus (см. последний раздел).

## 1. Безопасность — критично

- [ ] **Shell: default-deny.** Сейчас permissive-by-default — любой пир
  кэмпа может открыть терминал (`services/shell/shell.go:88-94`,
  `config.go` Shell.Allowed пуст = разрешено всем). Сделать
  `enabled=false` по умолчанию, явный allow-список pub-ключей в UI.
- [ ] **VNC: default-deny.** То же самое (`services/vnc/vnc.go:49-52`).
- [ ] **Подписывать UDP announce на camp.** Сервер принимает любой pub
  без подписи (`source/camp/udp.go`) — зная camp_id, можно объявиться
  с чужим ключом и подменить endpoint в roster (DoS конкретного пира).
  Подписать announce тем же Ed25519, сервер проверяет подпись по pub
  из пакета. Раскатка по правилу add-new → wait → remove-old.
- [ ] **Инвайты вместо «camp_id = членство».** Сейчас знание camp_id
  достаточно для входа. (В работе, ветка `invite`.)
- [ ] **Audit-лог для shell/vnc.** Логировать pub пира, время
  открытия/закрытия сессии, успех/провал (`shell.go`, `vnc.go` —
  сейчас только `session %s started`).
- [ ] **Rate-limit на camp-сервере.** Нет ограничений на число camps /
  peers-per-camp / частоту announce — спам пожирает память
  (`source/camp/hub.go`).

## 2. Надёжность

- [x] **bus: чистка мёртвых QUIC-коннектов.** Сделано: pingLoop теперь
  сносит кэшированные коннекты (и linkUp-записи) пиров, ушедших из
  roster (`mesh/bus/bus.go`, evictStale). Падение ping и так дропало.
- [ ] **engine: восстановление после ошибки чтения UDP.**
  `peerToTunLoop` при ошибке сокета просто выходит — пакеты теряются
  до полного Stop/Start (`mesh/engine/engine.go:~1671`). Пересоздавать
  сокет или эскалировать в управляемый рестарт.
- [ ] **holePunchLoop: гарантированный рестарт после rebind.**
  `restartOnEphemeralPort()` уходит в отдельную горутину и при ошибке
  Start может оставить движок без hole-punch (`engine.go:~1211`).
- [ ] **Retry/backoff в poll-петлях.** [→QUIC] Все опросы пиров (dns,
  pki, firewall, calls) — `if err != nil { continue }` без повтора;
  потерянный пакет = пропуск пира на весь тик.
- [ ] **tunnel: атомарность intercept.** Add ставит маршрут, потом
  пишет config; при ошибке persist маршрут и конфиг расходятся
  (`services/tunnel/tunnel.go:152-221`). Откатывать маршрут при
  ошибке записи.
- [ ] **awg bind: считать и логировать drop при переполнении inbox.**
  Буфер 64, переполнение — молчаливый drop (`mesh/engine/awg/bind.go:~195`).
  Достаточно atomic-счётчика + строки в Status.

## 3. Масштабирование (по мере роста кэмпа)

- [ ] **routeFor: O(1) вместо перебора.** Каждый исходящий пакет —
  линейный проход по peers (`engine.go:~1641`). Держать обратную map
  `overlayIP → UDPAddr`, перестраивать на roster-апдейте.
- [ ] **PeerCatalog: eviction.** Каталог только растёт, мёртвые пиры
  остаются навсегда (`mesh/camp/camp.go:~325`). TTL или максимум.
- [ ] **gossip store: eviction.** Аналогично (`mesh/gossip/gossip.go:~182`).

## 4. Качество кода

- [ ] **Общий Poller для poll-петель.** [→QUIC] dns (10s), pki (30s),
  firewall (30s), calls (3s) — четыре копии одного паттерна
  тикер+опрос. Вынести в общий хелпер с интервалом и backoff.
  После перехода на QUIC опросы в идеале заменяются push-уведомлениями
  по bus — тогда хелпер нужен только как fallback.
- [x] **Один http.Client на сервис.** Сделано попутно с миграцией на
  QUIC: клиент создаётся один раз в New и используется только
  HTTP-фоллбеком.
- [ ] **Синхронизировать lifecycle воркеров с Stop сервиса.**
  PollPeers/HealthCheck живут на process-ctx и переживают Stop своего
  сервиса (`main.go:~199-346`) — могут дёрнуть остановленный engine.
- [ ] **calls: mutex вокруг SFU.** `callCtx` в atomic.Value, но
  `AddParticipant` не защищён — гонка при одновременном Join
  (`services/calls/calls.go:~207`).
- [ ] **Вынести тайминги движка в конфиг.** keepalive 25s, burst 1s,
  fresh/wake 30s захардкожены (`engine.go`) — для отладки плохих сетей
  нужна крутилка без пересборки.
- [ ] **obfenv: не паниковать при сбое crypto/rand.** `randomMagic`
  паникует вместо возврата ошибки (`mesh/engine/obfenv/obfenv.go:~199`).
- [ ] **Ротация логов.** `~/.f2f/f2f.log` растёт бесконечно и содержит
  IP/имена пиров.

## 5. Тесты и документация

- [ ] **Тесты на services/.** Один тест на ~6000 строк (messenger).
  Минимум: dns-резолв по каталогу, pki выпуск/констрейнты leaf-cert,
  firewall сборка правил, tunnel add/remove intercept. Для этого
  сервисам нужны интерфейсы вместо прямого `*engine.Engine` —
  решить до того, как слой подрастёт.
- [ ] **Integration-тест engine Start/Stop** (два движка на loopback:
  пара, обмен пакетом, рестарт).
- [ ] **README в корне.** Сейчас две строки; хотя бы что это, ссылки
  на `source/helper/ARCHITECTURE.md` и quick start.

## Миграция сервисов HTTP → QUIC

**Статус (2026-06): фаза add-new сделана.** Все peer↔peer endpoints
получили bus-двойники, клиенты ходят bus-first с HTTP-фоллбеком:

| HTTP (tunnel listener)    | bus type      | где                          |
|---------------------------|---------------|------------------------------|
| GET /api/domains          | `domains`     | services/dns                 |
| GET /api/ca-cert          | `ca-cert`     | services/pki                 |
| GET /api/firewall         | `firewall`    | services/firewall            |
| GET /api/files            | `files`       | services/drop                |
| GET /api/call/state       | `call.state`  | services/calls               |
| POST /api/call/join       | `call.join`   | services/calls               |
| POST /api/call/leave      | `call.leave`  | services/calls               |
| POST /api/call/signal     | `call.signal` | services/calls               |
| POST /api/signal/inbox    | `signal`      | ui/web (RegisterBus)         |

**Фаза remove-old тоже сделана** (после проверки кэмпа на новой
сборке): HTTP-фоллбеки `fetch*` и фоллбек-ветки в ui/web удалены,
tunnel-listener (BindTunnel) выпилен, 2202 убран из builtin-портов
firewall, `TunnelHTTPPort` ушёл из engine. 80/443 остаются: это
reverse-proxy для опубликованных доменов (браузер пира ходит на
tunnelIP:443 напрямую), а не межсервисный RPC. Попутно bus перестал
пинговать офлайн-пиров (busResolver.Peers фильтрует по Online).

План был: вся межсервисная коммуникация пир↔пир (HTTP поверх
туннеля) переезжает на QUIC bus (`mesh/bus`), как уже сделано для
shell/vnc/notify.

Что это даёт сверх унификации:

- один транспорт = одна точка аутентификации пира (pub из
  QUIC-хендшейка) вместо доверия overlay-IP;
- push вместо poll: roster/domains/ca/firewall рассылаются по событию,
  poll-петли остаются только как редкий fallback;
- из firewall builtin-портов уходят 80/443/2202 для туннеля — наружу
  остаётся только QUIC bus (2203) и торрент;
- tunnel-facing HTTP-listener (`ui/web/server.go:133-168`) удаляется
  целиком — меньше поверхности атаки.

Порядок: сначала закрыть пункты раздела 1 (default-deny, подпись
announce) — они не зависят от транспорта; затем переносить endpoints
на bus по одному, по правилу совместимости add-new → wait → remove-old
(старый HTTP-handler живёт, пока все пиры не обновятся).

Перед началом миграции закрыть «bus: чистка мёртвых коннектов» из
раздела 2 — после переезда bus станет единственным транспортом и
утечка коннектов начнёт бить по всем сервисам сразу. (Сделано.)
