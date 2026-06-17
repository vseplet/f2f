# Документация f2f (helper)

Карта документов. Статусы: ✅ реализовано · 🟡 частично · 📐 дизайн.

## С чего начать

- **[GUIDE.md](GUIDE.md)** — *что делает и как пользоваться*: сборка, запуск,
  UI, где что хранится, rescue. ✅
- **[ARCHITECTURE.md](ARCHITECTURE.md)** — *где что лежит и почему*: гид по
  коду (структура, main, слои/сервисы, сценарии пакетов, горутины,
  дизайн-решения). ✅
- **[ROADMAP.md](ROADMAP.md)** — сделанное / планы по крупным направлениям.
- **[ENTITIES.md](ENTITIES.md)** — карта сущностей: camp · user · device · channel ·
  ресурсы (persistent/live/infra) и как всё связано.

## Сквозные понятия (общие для всего)

- **Канал = блок** (`block.channel`, scope `channels`): владелец + участники в
  его content; единица доступа для всех приложений. ACL-гейтинг пока не включён.
  См. [BLOCKS.md](BLOCKS.md), [DB.md](DB.md), [ENTITIES.md](ENTITIES.md).
- **Identity**: пир = пользователь, идентичность = `peer_pub`; профиль
  (`block.profile`) + passkey-якорь. См. [IDENTITY.md](IDENTITY.md),
  [INVITE.md](INVITE.md), [OIDC.md](OIDC.md).
- **Субстрат `db` + блоки**: иммутабельные подписанные логи (`Frame`) +
  репликация; чат/каналы/заметки-со-страницами — **на блоках** (`db/blocks`).
  См. [DB.md](DB.md), [BLOCKS.md](BLOCKS.md).

## Подсистемы

| док | про что | статус |
|---|---|---|
| [NAT.md](NAT.md) | hole-punch, авто-репанч и дыра в самовосстановлении туннеля | 🟡 |
| [OIDC.md](OIDC.md) | f2f как OIDC-провайдер (passkey, RS256, клиенты) | ✅ |
| [DB.md](DB.md) | распределённая БД: лог, SQLite, sync; релей/доставка/GC — дизайн | 🟡 |
| [BLOCKS.md](BLOCKS.md) | блок-движок; каналы/сообщения/заметки-страницы на нём | 🟡 |
| [MESSAGING_DESIGN.md](MESSAGING_DESIGN.md) | чат на блоках ✅; e2e/group keys/доставка/история — дизайн | 🟡 |
| [IDENTITY.md](IDENTITY.md) | личность/членство кэмпа (owner-only)/OIDC-профиль — на блоках | 📐 |
| [INVITE.md](INVITE.md) | инвайты и допуск пиров (пир=пользователь) | 📐 |
| [SECRETS.md](SECRETS.md) | хранилище секретов (два уровня, unlock) | 📐 |
