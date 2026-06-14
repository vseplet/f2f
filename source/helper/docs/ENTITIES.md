# Сущности и их организация

> **Статус:** карта модели. Часть сущностей есть в коде (✅), часть — дизайн (📐).
> Детали по подсистемам — в соответствующих доках (ссылки внизу).

Единая модель: что за сущности есть в f2f и как они связаны.

## Иерархия владения

```
Camp  (оверлей; владелец = pub в camp_id)                    ✅
├── User  (человек; user_id, per-camp, passkey-якорь)        📐  — член кэмпа
│    └── Device / Peer  (машина; device_pub)                 ✅  — привязан к User, 1..N
├── Channel (= scope; владелец: User, участники: User[] динам.) ✅ (членство-в-логе 📐)
│    └── Resource  (всё гейтится каналом)
│         ├── persistent (логи db): messages·notes·docs·tasks·files(ref)·secrets
│         ├── live (стримы шины):    terminals·desktops(VNC)·calls
│         └── infra (конфиг device): domains·intercepts·firewall·DNS — camp-wide ИЛИ channel-gated
└── Camp-root: CA/pki  (корень доверия на весь кэмп)                   ✅
```

## Слой 1 — Identity (кто)

- **Camp** ✅ — сеть/оверлей. Владелец зашит в `camp_id` (`<owner_pub>_<label>`).
  Контейнер всего. Кэмпы изолированы.
- **User** 📐 — человек. **`user_id` per-camp** (как device-ключи: в каждом
  кэмпе своя личность, без кросс-кэмп линка). Член кэмпа. Якорь — passkey.
  Сейчас в коде user-слоя нет: «личность» = `device_pub`.
- **Device / Peer** ✅ — машина с f2f. `device_pub` (ed25519) — транспорт +
  attestation. Привязан к `user_id` (device-cert), у юзера их 1..N. Сейчас
  «peer» = device без user-надстройки.

Детали: [INVITE.md](INVITE.md) (user/device/инвайты), [OIDC.md](OIDC.md).

## Слой 2 — Channel (где/кому) — сквозной ACL

- **Channel = `scope`** ✅ — единица доступа и шеринга: **владелец (User) +
  участники (User[])**, состав меняется динамически.
- **Членство** = owner-authored записи в логе канала; текущий состав = свёртка
  (single-writer владелец → тотальный порядок). *(сейчас членство таскается
  снапшотом в сообщении — 📐 переезд в лог)*
- DM = вырожденный канал (2 юзера, по форме ключа); `general` — спец-канал.
- **e2e**: group key на канал (эпохи), раздаётся участникам; смена состава =
  новая эпоха.
- **Членство канала = единый ACL для ВСЕХ его ресурсов** (persistent, live и
  channel-scoped infra).

Детали: [DB.md](DB.md) («Scope = канал»), [MESSAGING_DESIGN.md](MESSAGING_DESIGN.md).

## Слой 3 — Resource (что) — три природы

**Persistent** (записи/блоки в логах `db`, scope=канал; реплицируются, сходятся,
дампятся):
- **messages** ✅ · **notes** ✅ · **docs** 📐 · **tasks** 📐 ·
  **files** ✅ (блок-ссылка на blob, байты через drop/torrent) ·
  **secrets** 📐 (**scope = user или channel**: личные — user; сервиса/проекта —
  channel, видны участникам).

**Live** (стримы шины, не хранятся; **доступ гейтит членство канала**, поток
хостит конкретный device):
- **terminals** ✅ (shell) · **desktops/VNC** ✅ · **calls** ✅ (привязаны к
  каналам). События («звонок начался/закончился») могут логироваться блоками;
  сам поток — живой.

**Infra / networking** (конфиг устройства; публикует device, но **доступ/
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

✅ Аутентифицирует User'ов в приложения (passkey, RS256). Авторизация
приложения (кому можно входить) может гейтиться **членством канала**
(per-client allowlist = участники). См. [OIDC.md](OIDC.md).

## Сквозные рёбра

- **db** хранит persistent-ресурсы; `scope = channel`.
- **membership канала** — единый ACL для всех ресурсов канала.
- запись авторствована **`user_id`** (через device); device→user — привязка.
- **e2e group key** на канал; live-потоки — членство + транспортное шифрование
  (AWG/шина).

## Кардинальности

`Camp 1—N User` · `User 1—N Device` · `Camp 1—N Channel` ·
`Channel 1 owner / N members (User)` · `Channel 1—N Resource` ·
`Resource → 1 Channel` · `Live-resource → хостит 1 Device, доступ через Channel`.

## Зафиксированные решения

- **user_id — per-camp** (изоляция кэмпов; без кросс-кэмп линка личности).
- **Live-ресурсы гейтятся членством канала** (единый ACL, не отдельный
  per-device механизм).
- **Секреты — и user, и channel scope** (личные vs сервиса/проекта).
- **Infra (domains/intercepts/firewall/DNS) — camp-wide или channel-scoped**
  (видимость/доступ через членство); реально camp-level только CA.
