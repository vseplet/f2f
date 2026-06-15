# f2f как OIDC-провайдер

> **Статус:** реализовано в `services/oidc` — работает с реальным Gitea.

f2f превращает overlay-личность пира в **стандартные OIDC-токены**, так что
self-hosted приложения камп'а (Gitea, Affine, Grafana, …) логинятся «через
f2f» без своих паролей. Фактор входа — **passkey (Touch ID)**, личность —
camp-pub пира. Реализовано в `services/oidc/`.

Это **generic OIDC** (а не мимикрия под конкретные провайдеры): приложения
тянут наш `/.well-known/openid-configuration` и работают, как с любым
spec-совместимым IdP. Pocket ID/Authelia/Authentik — референсы, не цель.

## Главная идея: co-located IdP, по одному на пира

`zone` (`identity.CampLabel(campID)`) — **общая на весь камп**, поэтому один
лейбл (`auth`, `id`) не может принадлежать каждому пиру. Решение — **каждый
пир сам себе IdP**, а его токенам доверяют только приложения, настроенные на
его issuer (то есть его собственные приложения, которые он и хостит).

Адресуется IdP пира двумя способами (оба ведут в локальный OIDC-сервис на
`127.0.0.1:2203`, прокси проставляет attested-заголовок):

- **Выделенный домен `auth-<fp8>.<zone>.f2f`** (issuer в корне). `<fp8>` —
  первые 8 hex отпечатка pub, уникален на пира → коллизий в общей зоне нет.
  Это канонический issuer, его показывает админка.
- **Co-located `/oidc` на любом домене пира** (`https://gitea.<zone>.f2f/oidc`).
  Домен приложения и так маршрутизируется в этого пира; прокси перехватывает
  `/oidc/*`, срезает префикс и проставляет `X-Forwarded-Prefix`.

Маршрутизацию делает `services/proxy` (см. `handleProxy`): сверяет лейбл с
`dns.OIDCLabel()` и путь с `/oidc`. DNS резолвит `auth-<fp8>` локально через
`LocalRoutes`.

## Аутентификация: attestation + passkey

Личность доказывается **не сетью-как-таковой, а двумя слоями**:

1. **Overlay-attestation.** AmneziaWG/WireGuard аутентифицирует пира на L3.
   Прокси резолвит overlay-IP вызывающего → pub и инжектит заголовок
   **`X-F2F-Peer`** (всегда срезая входящую копию — анти-спуф). OIDC берёт
   `sub` = этот pub. Если заголовка нет (loopback/сам себе) — fallback на
   локальную личность.
2. **Passkey (WebAuthn).** На экране `/authorize` вместо кнопки «разрешить» —
   WebAuthn-церемония: первый раз энролл (`navigator.credentials.create`),
   дальше вход (`...get`). Touch ID = «живой человек присутствует и
   согласен». Защищает от attested-но-беспризорного устройства.

**`rpId = <zone>.f2f`** (не хост): браузер позволяет rpId быть
registrable-родителем origin'а, а `.f2f` — неизвестный TLD, поэтому
`<zone>.f2f` — registrable-домен. Значит **один passkey на камп** работает у
IdP любого пира. Энролл — один раз.

Credential'ы хранятся в `passkeys.json` (`pub → []credential`), **локально**.
Камп-вайд репликация публичной части credential'ов (чтобы IdP любого пира мог
проверить ассерцию удалённого гостя) — **пока не сделана**; на одной машине
self/loopback-кейс работает.

## Подпись токенов: RS256, не EdDSA

Изначально хотели подписывать camp-ключом ed25519 (EdDSA, kid=pub). Но
реальные OIDC-клиенты (Gitea на `coreos/go-oidc` и многие другие) **не
принимают EdDSA** — их список алгоритмов это RS256/ES256/PS256. Поэтому:

- Подписываем **RS256** отдельным **per-camp RSA-2048** ключом.
- Ключ генерится при первом использовании и лежит в `oidc_rsa.pem` (камп-дир,
  `0600`). `kid` выводится из pub-ключа → стабилен между рестартами (JWKS не
  протухает у relying party).
- `iss`/issuer-хост по-прежнему привязывает токен к пиру; `sub` = pub.

Каждый пир генерит **свой** RSA-ключ. Репликация не нужна: issuer per-peer,
приложение тянет JWKS с того же хоста.

## Клиенты (приложения)

Реестр персистентный — `clients.json` (камп-дир), переживает рестарты.

- **Confidential** (серверные: Gitea/Affine/…): есть `client_secret`
  (хранится в открытом виде, чтобы показывать в админке; файл `0600` за
  оверлеем). Аутентифицируются на `/token` через `client_secret_basic`/`_post`.
- **Public** (SPA/native): без секрета, обязателен PKCE.
- **PKCE опционально** для confidential (многие серверные приложения PKCE не
  шлют), обязателен для public.
- **Wildcard** в redirect/logout URI (`https://*.app.<zone>.f2f/*`).
- **Динамическая регистрация** (RFC 7591, `/register`) создаёт public-клиента;
  confidential заводятся через админку.
- redirect_uri валидируется: только `https` в зоне `.f2f` (loopback `http`
  разрешён для локальной демки).

## Claims

`sub` = pub пира. У f2f нет email/username, поэтому синтезируем по scope:

- `profile` → `name`, `preferred_username` = имя пира.
- `email` → `email = <fp8>@<zone>.f2f` (стабильный идентификатор), плюс
  `email_verified: false` (это идентификатор, а не доставляемый адрес).

## Эндпоинты

`/.well-known/openid-configuration`, `/authorize`, `/token`, `/userinfo`,
`/jwks`, `/register` (DCR), `/end_session`, и для passkey-церемонии
`/authorize/passkey/begin|finish`, `/authorize/cancel`. Flow —
authorization code + (опционально) PKCE S256.

## Logout

`/end_session` (RP-initiated logout). У IdP **нет постоянной сессии** (passkey
спрашивается каждый вход), поэтому чистить нечего — мы валидируем
`post_logout_redirect_uri` и редиректим назад. Валидация: совпадение с
зарегистрированным logout-URI **или** совпадение origin'а с одним из
redirect_uri клиента (чтобы «вернуться в приложение» работало без отдельной
регистрации).

## Сессии и время жизни

Три разных слоя, не путать:

- **id/access-токен** — `exp` 60 минут. Используются только в момент логина
  (приложение меняет код → читает claims → заводит свою сессию).
- **SSO-сессия у IdP** — **отсутствует**. Каждый поход на `/authorize` = новый
  passkey. Можно добавить (подписанная кука на N часов) — тогда один Touch ID
  на окно, и `end_session` станет настоящим single-logout.
- **Сессия приложения** (напр. кука Gitea) — управляется самим приложением.
  Именно она определяет, **как часто спрашивается passkey**: пока сессия
  приложения жива, к нашему IdP оно не обращается.

## Модель угроз

IdP технически может вписать любой `sub` в токены, **которые сам подписывает**
— то есть соврать **только своим приложениям** (а он их и так хостит, эскалации
нет). Подделать токен для **чужого** пира нельзя: у того другой issuer и
другой RSA-ключ в JWKS. Именно per-peer-модель это и сдерживает — единый
центральный IdP позволил бы одному пиру прикидываться кем угодно везде.

End-to-end (чтобы даже приложение не верило хосту на слово) потребовал бы
со-подписи гостем — обычные OIDC-приложения такое не едят, поэтому вне рамок.

## Файлы на диске (камп-дир)

- `oidc_rsa.pem` — RS256 signing key.
- `clients.json` — реестр приложений (+ секреты).
- `passkeys.json` — passkey-credential'ы по pub.

## Где в коде

- `services/oidc/` — `oidc.go` (эндпоинты/flow), `jwt.go` (RS256+JWKS),
  `signkey.go` (RSA-ключ), `webauthn.go` (passkey-обвязка), `credstore.go`,
  `clientstore.go`, `demo/` (игрушечный RP для проверки).
- `services/proxy/proxy.go` — роут `auth-<fp>`/`/oidc`, инжект `X-F2F-Peer`.
- `services/dns/dns.go` — `OIDCLabel()` + запись в `LocalRoutes`.
- `mesh/engine/engine.go` — `Identity()` (доступ к ключу для in-camp gate).
- `main.go` — конструирование сервиса + loopback-листенер `:2203`.
- `ui/web/oidc.go` + assets — API `/api/oidc[/clients]` и вкладка OIDC.

## Пример: Gitea

1. Gitea на `127.0.0.1:3000`, `ROOT_URL=https://gitea-test.<zone>.f2f/`.
2. Опубликовать домен `gitea-test.<zone>.f2f → 127.0.0.1:3000`.
3. В портале (вкладка OIDC) создать confidential-клиента, callback
   `https://gitea-test.<zone>.f2f/user/oauth2/f2f/callback`, PKCE off.
4. В Gitea добавить OAuth2-источник (`gitea admin auth add-oauth
   --provider openidConnect --auto-discover-url
   https://auth-<fp8>.<zone>.f2f/.well-known/openid-configuration` + id/secret,
   scopes `openid profile email`), `ENABLE_AUTO_REGISTRATION`.

## Не сделано (TODO)

- **SSO-сессия** на IdP (меньше Touch ID, single-logout).
- **Per-client access control** (allowlist pub'ов/групп) + **журнал входов**
  (user→client) — сейчас любой член камп'а может войти в любой клиент.
- **Камп-вайд репликация** passkey-credential'ов (вход удалённого гостя у
  чужого IdP) — через membership-лог из `MESSAGING_DESIGN.md`.
- **Edit клиента** (сейчас только create/delete).
- **Forward-auth гейт** для сервисов без OIDC (n8n, Supabase): passkey-логин
  на прокси перед апстримом + инжект identity-заголовков.
- **Защита самого портала** (`:2202`) за passkey.
- **Группы и политики** доступа (см. ARCHITECTURE.md).
