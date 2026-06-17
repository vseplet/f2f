# Сущности и их организация

> **Статус:** карта модели. Часть сущностей есть в коде (✅), часть — дизайн (📐).
> Детали по подсистемам — в соответствующих доках (ссылки внизу).

Единая модель: что за сущности есть в f2f и как они связаны.

> **Решение:** пир = пользователь. Отдельного слоя `User`/`user_id` нет —
> идентичность это **`peer_pub`** (ed25519 машины). Один человек на одной машине =
> один пир = одна личность. Второе устройство = отдельный пир (отдельный
> camp-invite), а не «привязка к тому же юзеру». Сделано «для простоты».

## Иерархия владения

```
Camp  (оверлей; владелец = pub в camp_id)                            ✅
├── Peer  (машина = человек = личность; peer_pub ed25519)            ✅ (профиль 📐) — член кэмпа
│    └── Profile (block.profile в well-known scope profiles)        📐  — self-authored
├── Channel (= блок `block.channel` в scope channel:<bid>; владелец+участники в content) ✅ (синк гейтится членством ✅; e2e 📐)
│    └── Resource  (всё гейтится каналом)
│         ├── persistent (логи db): messages·notes·docs·tasks·files(ref)·secrets
│         ├── live (стримы шины):    terminals·desktops(VNC)·calls
│         └── infra (конфиг device): domains·intercepts·firewall·DNS — camp-wide ИЛИ channel-gated
└── Camp-root: CA/pki  (корень доверия на весь кэмп)                   ✅
```

## Слой 1 — Identity (кто)

- **Camp** ✅ — сеть/оверлей. Владелец зашит в `camp_id` (`<owner_pub>_<label>`).
  Контейнер всего. Кэмпы изолированы.
- **Peer** ✅ — машина с f2f, она же личность. **`peer_pub` (ed25519)** —
  и транспорт/attestation, и идентичность одновременно. В каждом кэмпе своя
  (без кросс-кэмп линка). Член кэмпа.
- **Profile** 📐 — отображаемая часть пира: `name` (username), first/last,
  email, публичные passkey-креды. Живёт **блоком** (`block.profile`),
  self-authored, синкается всем членам кэмпа. Якорь входа — passkey на
  `<zone>.f2f`. См. [IDENTITY.md](IDENTITY.md).

Детали: [INVITE.md](INVITE.md) (допуск/инвайты), [OIDC.md](OIDC.md).

## Слой 2 — Channel (где/кому) — сквозной ACL

- **Channel = блок `block.channel`** ✅ — в **своём** scope `channel:<bid>`,
  `content = {name, members}`. Владелец = автор первой версии; **участники прямо
  во фрейме канала** (множество `peer_pub`), состав меняет владелец новой
  версией (single-writer → без конфликтов). Ресурсы канала — в scope'ах по bid
  (`note:<bid>`, `message:<bid>`).
- **bid** ✅: `general` (well-known), `dm-<hash(pubs)>` (личка, детерминир.),
  `<fp16>-<rand>` (обычный). `general` зарезервирован (нельзя пересоздать/вложить).
- **Членство канала = единый ACL** — **синк гейтится по нему ✅**:
  `Sync.SetMemberCheck` отдаёт `channel:`/`message:`/`note:<bid>` пиру только если
  он член (фильтрует `db.scopes`/`db.pull`/`db.push` по bus-attested pub).
  Не-член не получает ни мету канала, ни сообщения/заметки.
- **e2e** 📐: group key на канал (эпохи) участникам; смена состава = новая эпоха.
  Нужен для защиты при релее/утечке фрейма и forward-secrecy при исключении —
  одного гейтинга синка для этого мало. См. [ROADMAP.md](ROADMAP.md).

Детали: [DB.md](DB.md) («Scope»), [BLOCKS.md](BLOCKS.md) (`block.channel`).

## Слой 3 — Resource (что) — три природы

**Persistent** (записи/блоки в логах `db`, scope=канал; реплицируются, сходятся,
дампятся):
- **messages** ✅ · **notes** ✅ · **docs** 📐 · **tasks** 📐 ·
  **files** ✅ (блок-ссылка на blob, байты через drop/torrent) ·
  **secrets** 📐 (**scope = peer или channel**: личные — peer; сервиса/проекта —
  channel, видны участникам).

**Live** (стримы шины, не хранятся; **доступ гейтит членство канала**, поток
хостит конкретный пир):
- **terminals** ✅ (shell) · **desktops/VNC** ✅ · **calls** ✅ (привязаны к
  каналам). События («звонок начался/закончился») могут логироваться блоками;
  сам поток — живой.

**Infra / networking** (конфиг устройства; публикует пир, но **доступ/
видимость можно гейтить каналом** — camp-wide по умолчанию, channel-scoped как
ACL-уточнение):
- **domains** ✅ (домен/приложение видно/доступно только участникам канала) ·
  **intercepts/egress** ✅ (egress доступен каналу) · **firewall** ✅ (порт
  открыт только членам канала) · **DNS** ✅ (записи зоны скрыты от не-членов).

Детали: [BLOCKS.md](BLOCKS.md) (persistent как блоки), [SECRETS.md](SECRETS.md).

## Camp-root: CA / pki

Единственное реально camp-level — **CA** ✅ (корень доверия на весь кэмп,
name-constrained на `.<zone>.f2f`). Leaf/trusted-peer-серты следуют за domains.

## OIDC — в identity-слое

✅ Аутентифицирует пиров в приложения (passkey, RS256). `sub` = `peer_pub`,
email/имя — из `block.profile`. Авторизация приложения (кому можно входить)
может гейтиться **членством канала** (per-client allowlist = участники).
См. [OIDC.md](OIDC.md).

## Сквозные рёбра

- **db** хранит persistent-ресурсы; `scope = channel`.
- **membership канала** — единый ACL для всех ресурсов канала.
- запись авторствована **`peer_pub`** напрямую (без прослойки user_id).
- **e2e group key** на канал; live-потоки — членство + транспортное шифрование
  (AWG/шина).

## Кардинальности

`Camp 1—N Peer` · `Peer 1—1 Profile` · `Camp 1—N Channel` ·
`Channel 1 owner / N members (Peer)` · `Channel 1—N Resource` ·
`Resource → 1 Channel` · `Live-resource → хостит 1 Peer, доступ через Channel`.

## Зафиксированные решения

- **Пир = пользователь** — слой `user_id` убран; идентичность = `peer_pub`,
  per-camp (изоляция кэмпов). Несколько устройств = несколько пиров.
- **Профиль — блок** (`block.profile`) в well-known scope `profiles`,
  self-authored, синкается всем членам кэмпа.
- **Live-ресурсы гейтятся членством канала** (единый ACL, не отдельный
  per-device механизм).
- **Секреты — и peer, и channel scope** (личные vs сервиса/проекта).
- **Infra (domains/intercepts/firewall/DNS) — camp-wide или channel-scoped**
  (видимость/доступ через членство); реально camp-level только CA.
