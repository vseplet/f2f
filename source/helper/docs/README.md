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

- **Канал (scope) = единица доступа** для ВСЕХ приложений (чат, доки, таски,
  OIDC, файлы): владелец + динамические участники; членство = owner-authored
  записи в логе. См. [DB.md](DB.md) и [MESSAGING_DESIGN.md](MESSAGING_DESIGN.md).
- **Identity / пользователь**: `user_id` поверх device-ключей; passkey-якорь.
  См. [INVITE.md](INVITE.md), [OIDC.md](OIDC.md).
- **Субстрат `db`**: иммутабельные подписанные логи + репликация; чат/доки/таски
  — приложения над ним. См. [DB.md](DB.md), [BLOCKS.md](BLOCKS.md).

## Подсистемы

| док | про что | статус |
|---|---|---|
| [OIDC.md](OIDC.md) | f2f как OIDC-провайдер (passkey, RS256, клиенты) | ✅ |
| [DB.md](DB.md) | распределённая БД: лог, хранение, sync, доставка | 🟡 ядро |
| [BLOCKS.md](BLOCKS.md) | блок-модель: чат/доки/таски как один движок | 📐 |
| [MESSAGING_DESIGN.md](MESSAGING_DESIGN.md) | чат: членство, group keys, e2e, история | 📐 |
| [INVITE.md](INVITE.md) | user-identity, инвайты, допуск пиров | 📐 |
| [SECRETS.md](SECRETS.md) | хранилище секретов (два уровня, unlock) | 📐 |
