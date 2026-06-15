# Распределённая БД (пакет `db`)

> **Статус:** реализовано (`db/`): лог, SQLite-бэкенд, sync поверх bus (push +
> pull + обнаружение scope'ов), инкрементальная свёртка. Релей, e2e, frontier'ы
> доставки, снапшоты/GC — дизайн.

f2f's distributed database — **над mesh** (реплицируется через `mesh/bus`),
**под всеми сервисами** (один общий стор, который они переиспользуют),
дампится и шерится целиком. Двигает и хранит **opaque подписанные entries**;
смысл записи (сообщение, блок документа, задача) и сведение конкурентных
версий — в приложениях НАД пакетом (см. [BLOCKS.md](BLOCKS.md),
[MESSAGING_DESIGN.md](MESSAGING_DESIGN.md)).

## Статус

- **Реализовано** (`db/`): `Frame` (sign/verify/chain), `Store` интерфейс +
  `MemStore` + **`SQLiteStore`** (один файл `db.sqlite` per-camp), `Service`
  (`Commit`/`Apply`/`Since`/`Vector`/`Frames`/`Scopes`/`MaxLamport`/`Query`/
  `Dump`/`Import`, хуки `OnCommit`/`OnApply`), **`Sync`** поверх `mesh/bus`
  (eager push + anti-entropy pull + `db.scopes`-обнаружение незнакомых scope'ов).
  Lamport переживает рестарт (reseed из стора). Первый потребитель — **`blocks`**
  (чат/каналы/заметки уже на нём, см. [BLOCKS.md](BLOCKS.md)).
- **Не реализовано**: релей шифротекста, e2e group key, frontier'ы доставки,
  снапшоты/GC.

## Модель: `Frame`

Иммутабельная подписанная запись в per-(author, scope) логе. **Ничего не
редактируем и не удаляем** — «edit»/«delete» это новые записи; состояние =
детерминированная свёртка над логом.

**Метадата открытым текстом** (db индексирует, релей носит вслепую),
**payload запечатан** group-key'ем scope'а:

```
scope, author, seq, prev, lamport, type, ts   ← cleartext
payload                                        ← sealed (зашифрованный BlockOp)
sig, id                                        ← подпись автора + content-hash
```

> Тип записи раньше назывался `Entry`; переименован в **`Frame`** (слово «лог»
> перегружено — есть console-log, `/api/log/stream`). Wire-формат не изменился
> (JSON-теги те же); SQLite-таблица мигрирует `entries`→`frames` на старте.

Два РАЗНЫХ «предка», не путать:
- `Frame.prev` — предыдущая запись **этого автора** в логе (per-author chain) →
  целостность репликации (дырки/форки).
- `op.parents` (app-уровень, в payload) — версии **одного блока**
  (cross-author DAG) → heads/табы/merge. См. BLOCKS.md.

## Scope = «тип:канал»

`scope` — строковый ключ, по которому распилен лог: `<тип данных>:<канал>`.
Реальные: `channel:<bid>` (мета канала — по scope на канал), `message:<bid>`
(сообщения), `note:<conv>` (блоки заметки). Канал — единица доступа.

- **Канал — это блок** (`block.channel` в **своём** scope `channel:<bid>`, см.
  BLOCKS.md) — отдельный scope на канал нужен, чтобы синк гейтил его по членству.
  `content = {name, members}` — **участники прямо во фрейме канала**, владелец =
  автор первой версии. Состав меняет владелец новой версией блока (single-writer
  → без конфликтов). Проверка — `channels.IsMember(bid, pub)`.
- **bid канала**: `general` (well-known), `dm-<hash(sort(pubs))>` (детерминир.
  для лички), `<fp16>-<rand>` (обычный, namespace по автору-создателю).
- **e2e на канал** (дизайн): эпоха + group key на scope участникам; смена
  состава = новая эпоха (см. [MESSAGING_DESIGN.md](MESSAGING_DESIGN.md)).

> **ACL: гейтинг синка по членству — РЕАЛИЗОВАН.** `Sync.SetMemberCheck`
> (в `main` через `channelsMgr.IsMember`) отдаёт scope (`channel:`/`message:`/
> `note:<bid>`) пиру только если он член канала — фильтрует `db.scopes`, `db.pull`
> и `db.push` (по bus-attested pub запрашивающего). Не-член больше не получает ни
> метаданные канала, ни сообщения/заметки. **Не закрыто (📐):** e2e group key
> (релей/утечка фрейма не защищены без шифрования) и forward-secrecy при
> исключении (старое уже скачанное у бывшего члена остаётся).

## Хранение (SQLite-бэкенд) ✅

**Лог не дробим по таблицам** — иначе ломается единый sync. Источник правды —
**одна таблица `frames`**; «партиционирование» делаем **индексами**, не
таблицами. Файл per-camp (`db.sqlite`) → дамп = копия файла. (`frontiers`/`meta`
ниже — пока дизайн.)

```sql
CREATE TABLE frames (
  id TEXT PRIMARY KEY, scope TEXT, author TEXT, seq INTEGER, prev TEXT,
  lamport INTEGER, type TEXT, ts INTEGER, payload BLOB, sig TEXT,
  UNIQUE(scope, author, seq)           -- индекс под per-author дельту (Since)
);
CREATE INDEX ix_scope_lamport ON frames(scope, lamport);  -- фолд/порядок
CREATE INDEX ix_scope_type    ON frames(scope, type);     -- «все медиа/таблицы в scope»

-- проект:
CREATE TABLE frontiers ( scope TEXT, peer TEXT, author TEXT, seq INTEGER,
  PRIMARY KEY(scope, peer, author) );    -- статус доставки (version-вектор пира)
CREATE TABLE meta ( k TEXT PRIMARY KEY, v BLOB );  -- версия схемы, snapshot-чекпойнты
```

**Проекции — уровень приложения, отдельные таблицы**: лог = truth (payload
зашифрован), а каждое приложение держит свои **материализованные свёртки над
расшифрованным логом**: messenger → `messages`+FTS, docs → `blocks_current`
(heads + порядок), tasks → индекс по полям. Субстрат про них не знает.

**Проекция = чистая функция `fold(log) → state`, одноразовый кэш.** Её можно
**пересобрать с нуля в любой момент** проигрыванием лога — лог это
единственный источник правды. Отсюда:
- **миграция схемы** проекции = поменять код + пересобрать (nuke+replay); сам
  **лог никогда не мигрирует**;
- **восстановление** при повреждении/баге = дропнуть таблицу и отстроить;
- **новое устройство/версия приложения** строит проекцию локально из
  реплицированного лога.
Нормально проекция инкрементальная (хранит **watermark** — до какого lamport
досчитана), но всегда сносима и пересобираема. Это event-sourcing / CQRS
read-model.

## Sync (anti-entropy) ✅ (релей — проект)

- Состояние scope у реплики = **version vector** `{author → max seq}` (дешёвый
  `Vector(scope)`, без чтения payload'ов).
- **Push** (`db.push`): на каждый локальный commit (`OnCommit`) запись летит
  всем подключённым пирам; получатель `Apply`, на дырку — pull у отправителя.
- **Pull** (`db.pull`): по тику `PullAll` пир спрашивает `Since(have)` у каждого.
  `Since` читается **per-author по индексу** (дельта, без скана всего scope).
- **Обнаружение scope'ов** (`db.scopes`): пир спрашивает у других их список
  scope'ов и тянет незнакомые — иначе поздно подключившийся не узнал бы про
  канал, созданный пока он был офлайн.
- **Релей** (проект): любой пир, у кого есть запись, отдаёт её → store-and-forward.
  Не-член релеит **шифротекст вслепую** (двухслойный конверт: внутренний
  подписанный+зашифрованный `Frame`, внешний мутабельный `Relay`+TTL).
- **Снапшоты под GC**: append-only растёт → перекрытые версии дропаем после
  merge+полной репликации; догоняющему льём **снапшот (подписанный checkpoint
  свёртки) + свежий хвост**, а не весь лог с начала.

## Доставка / степень синхронизации

**Не храним пер-запись-пер-пир галочки** — статус выводится из frontier'ов:
запись `(author A, seq N)` доставлена пиру X ⟺ `vv_X[scope][A] ≥ N`.

- **Получатели** = membership scope'а (DM → один пир, канал → все).
- **delivered all** = frontier всех членов покрывает запись.
- **Степень синхронизации** пира = отставание его frontier'а от текущего.
- Галочки: **sent** (в логе) → **delivered(X)** → **delivered all**.
- Два уровня уверенности:
  - **мягкий**: frontier, который пир анонсировал (anti-entropy/gossip) — для
    галочек, без подписи;
  - **жёсткий**: получатель коммитит **подписанную `cursor.ack`/`cursor.read`**
    запись (= «получил/прочитал до (A,N)»), она реплицируется обратно →
    криптопруф + read-receipt; едет через релеи транзитивно.
- **Заменяет** старый outbox/ACK-ретрай мессенджера.

## Мои записи vs чужие

Это **не тип блока** (тип = `Entry.type`: text/media/…), а **происхождение** —
`Entry.author`. Различать обязательно, но **выводится из `author == мой
user_id`** (cleartext, индексируется); отдельный `is_mine` флаг не нужен
(денормализация, рассинхрон — это буквально `author==me`).

Разница — в роли sync-движка, не в хранении:
- **мои** — активно **пушить** онлайн-членам + **трекать доставку** (frontier'ы
  получателей должны догнать мой seq) + **ретрансмит** до delivered-all. По сути
  **outbox**.
- **чужие** — **pull/relay/converge**, доставку не гоню.
- проекцию кормят и те, и другие.

Хранить полезно **не per-block флаг, а watermark на scope** — «до какого моего
seq доставлено всем» (выводится из frontier'ов; кэш для «что досылать» + галочек).

Нюанс: пока `Author` = device-pub, «мои» = только с этого устройства; с вводом
`user_id` `author == мой user_id` охватит **все мои устройства**.

## Типы и поиск

- `Entry.type` (`block.text`/`block.media`/`block.table`/`chat.msg`…) —
  **cleartext, индексируется** → метадата-фильтр «все медиа в scope» без
  расшифровки.
- **Полнотекст по контенту** — app-уровень **после decrypt** (FTS в проекции),
  db в payload не лезет.
- **Медиа по ссылке**: payload = content-address (hash/magnet), сами байты —
  через файловый слой (drop/torrent / Blob Storage). Лог = мелкие записи.

## Dump / Import

`Dump()` → вся БД (все scope'ы) в JSON; записи самоверифицируются подписями.
`Import()` вливает дамп (применяет в порядке цепочек, идемпотентно). Для
бэкапа / шеринга целиком / нового устройства.

## API (текущее)

`Commit(signer, scope, type, payload)` (локальная запись — сам ставит
seq/prev/lamport/подпись, reseed Lamport из стора), `Apply(frame)` (приём
реплики), `Since(scope, have)`, `Vector(scope)`, `Frames(scope)`, `Scopes()`,
`MaxLamport()`, `Query(sql)` (read-only SQL-консоль), `Dump()`, `Import(blob)`,
хуки `OnCommit(fn)` (→ push) и `OnApply(fn)` (→ live-обновление UI).

## Сделано / дальше

- ✅ SQLite-бэкенд, sync over `mesh/bus` (push+pull+scopes), инкрементальная
  свёртка, Lamport-reseed, первый потребитель (`blocks`: чат/каналы/заметки;
  старый `services/messenger` снят).
- ⬜ Frontier'ы доставки (галочки sent/delivered/read), снапшоты/GC, релей
  шифротекста, e2e group key + ACL-гейтинг синка по членству.
