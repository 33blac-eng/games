# Безпековий аудит oo-screen (віддалений екран і ввід)

Дата: 03.10.2026. Обсяг: `hub/` (ticket.go, revoke.go), `hub/cmd/hub-webrtc/`,
`agent/input/`, `agent/cmd/oo-agent/` (точково), JS-плеєр
`total-erp-app/resources/js/remote/`. Бекенд ERP (видача/підпис/consume квитка)
у цьому репо відсутній, тому властивості квитка на боці ERP позначено UNVERIFIED.

Тести: `hub/security_test.go` (SecT), `hub/cmd/hub-webrtc/security_test.go` (SecH),
`agent/input/security_test.go` (SecI), `total-erp-app/resources/js/remote/__tests__/security.test.mjs` (SecJ).
Тест із суфіксом `KnownFAIL` фіксує наявну ваду: коли її виправлять, він впаде, і його треба перевернути.

Статуси: **PASS** — властивість виконується; **FAIL** — вада підтверджена, не виправлена (задокументовано);
**FIXED** — виправлено в цій гілці; **UNVERIFIED** — перевірити з цього репо неможливо.

| # | Властивість | file:line | Результат | Доказ |
|---|---|---|---|---|
| 1 | Глядач без квитка в ticket-режимі відхиляється, ERP не викликається | hub/cmd/hub-webrtc/main.go:739 | PASS | SecH01 |
| 2 | Статичний агентський токен не відчиняє viewer-ногу в проді | main.go:739 | PASS | SecH02 |
| 3 | Глядач ноди A не досягає ноди B (node лише з claims, не з тіла) | main.go:750 | PASS | SecH03 |
| 4 | Consume fail-closed: 4xx/5xx/битий JSON/без claims/ERP недосяжний | hub/ticket.go:49-98, main.go:742 | PASS | SecT02, SecH04 |
| 5 | Квиток передається в тілі POST (не в URL), з X-OO-Hub-Key | hub/ticket.go:57-68 | PASS | SecT01 |
| 6 | Replay квитка: hub не кешує claims, single-use делеговано ERP | hub/ticket.go:49 | PASS (hub) / UNVERIFIED (ERP) | SecT03; реалізація single-use в ERP поза репо |
| 7 | Підпис, expiry, clock skew, constant-time порівняння квитка | ERP ScreenEngineController (поза репо) | UNVERIFIED | hub JWT не валідує взагалі, лише consume в ERP |
| 8 | Тіло відповіді ERP обмежене (1 МБ) | hub/ticket.go:80 | PASS | SecT04 |
| 9 | Runtime-відкликання node/user, курсор, fail-soft, ключ у заголовку | hub/revoke.go:261 | PASS | SecT06, SecH17 |
| 10 | Stale-рубильник: ERP мовчить > 90 с → рвуться всі сесії; env не вимикає | hub/revoke.go:185 | PASS | SecT07 |
| 11 | View-only глядач не може інʼєктувати ввід; чужий квиток → kill сесії | hub/cmd/hub-webrtc/input.go:102 | PASS | SecH06 |
| 12 | Ліміти каналу вводу: 1 КБ/повідомлення, 200/с, burst 40 | input.go:57-71 | PASS | SecH06 |
| 13 | Ліміт тіла /offer/*, /control, /viewer/visibility (256 КБ, отже й SDP) | main.go:775, outputs.go:99, visibility.go:131 | PASS | SecH07 |
| 14 | Лише POST на /offer/* | main.go:770 | PASS | SecH08 |
| 15 | CORS: у ticket-режимі ACAO = лише origin ERP | main.go:695 | PASS | SecH09 (у T1-режимі `*`, режим не для проду) |
| 16 | Агентський токен: constant-time, невірний → 401 без створення ноди | main.go:1029 | PASS | SecH10 |
| 17 | Один спільний агентський токен на весь парк; node обирає сам агент → будь-хто з токеном реєструє/витісняє чужу ноду й отримує ввід її глядачів; невдалий offer лишає ноду в реєстрі (ріст памʼяті) | main.go:848 | **FIXED** (ef60492) | Токен ноди hex(HMAC-SHA256(master, node_id)), constant-time (hub/agenttoken.go, hub/cmd/hub-webrtc/agentauth.go); випуск — hub/cmd/oo-node-token. Легасі спільний токен за замовчуванням ще приймається з WARNING раз на ноду; OO_SCREEN_AGENT_AUTH=strict його вимикає (OO_SCREEN_LEGACY_AGENT_TOKEN=1 — аварійний відкат). Невдалий agent-offer прибирає створену ним ноду. SecH11 перевернуто (TestSecH11AgentTokenBoundToNode). Залишок до strict: випустити токени всім нодам |
| 18 | /nodes лише з X-OO-Hub-Key; порожній ключ = закрито | nodes.go:57 | PASS | SecH12 |
| 19 | session_id ренегоціації/visibility — 128 біт crypto/rand, constant-time | main.go:1430 | PASS | SecH13 |
| 20 | Стеля глядачів на ноду (DoS памʼяттю/горутинами) | main.go:817 | **FIXED** | SecH14; коміт `5b6bcb6` (OO_SCREEN_MAX_VIEWERS, дефолт 16, 429) |
| 21 | Глобальні ліміти: кількість нод, rate-limit /offer/viewer по IP (кожен запит = виклик ERP) | main.go:763 | **FIXED** (a649a3a) | OO_SCREEN_MAX_NODES (дефолт 500) — нова нода понад стелю 503; per-IP token bucket на /offer/viewer і /offer/agent (OO_SCREEN_OFFER_RATE=1/с, OO_SCREEN_OFFER_BURST=10, RATE=0 вимикає); X-Forwarded-For лише від OO_SCREEN_TRUSTED_PROXIES (hub/cmd/hub-webrtc/ratelimit.go). TestSecRateLimitPerIP, TestSecMaxNodes |
| 22 | Кеші NACK/GOP, черги глядачів обмежені; стеля тривалості сесії 120 хв | nack.go, gop.go, fanout.go:139-170 | PASS | наявні nack_test.go, sessioncap_test.go |
| 23 | Права файлів записів (кадри чужих екранів) | record.go:369-378 | **FIXED** | SecH15; коміт `66f9961` (0700/0600, O_EXCL замість O_TRUNC) |
| 24 | Path traversal в імені запису з node_id агента | record.go:441 | PASS | SecH16 |
| 25 | Ретенція записів (вік/обсяг) | recordprune.go:87 | PASS | наявний recordprune_test.go |
| 26 | /control (перемикання монітора) приймає квиток grant=view | outputs.go:106 | **FIXED (opt-in)** (028c18a) | Дефолт не змінено (ERP-консоль шле view-квитки); OO_SCREEN_CONTROL_REQUIRES_INPUT=1 вимагає grant=control для /control, інакше 403. SecH18 перевернуто (TestSecH18ControlGrantOptIn) |
| 27 | Відповіді з помилкою не містять нутрощів і квитка | main.go:984, 742 | PASS | SecH19 |
| 28 | Тіло помилки ERP повністю йде в journald | hub/ticket.go:83, main.go:744 | **FIXED** (f42e7b4) | Тіло помилки ERP обрізане до 200 символів, квиток (jti) і JWT/довгі токеноподібні рядки вирізаються; тіло 2xx «bad json/no claims» не логується (лише довжина). SecT05 перевернуто (TestSecT05ERPErrorBodyRedacted) |
| 29 | Таймаути HTTP-сервера, заборона дефолтного токена на старті | main.go:2047, 2090 | PASS | SecH20 |
| 30 | pprof лише за явним env на окремому слухачі | main.go:2057 | PASS | рев'ю коду; адресу треба ставити 127.0.0.1 |
| 31 | TLS сигналінгу: хаб слухає plain HTTP `:4470` на всіх інтерфейсах (TLS — у nginx) | main.go:52 | UNVERIFIED | Безпечно лише якщо :4470 закритий firewall-ом; рекомендація: дефолт 127.0.0.1:4470 |
| 32 | Легасі WebTransport: InsecureSkipVerify в агенті | agent/cmd/oo-agent/main.go:196 | **FIXED** (e430e3c) | Легасі wt: перевірка сертифіката за замовчуванням; самопідписаний hub-wt — пінінг -wt-cert-sha256 (CERT_HASH= хаба, base64/hex); -wt-insecure лише явно (internal/agentcred/wttls.go, TestWTTLSConfig) |
| 33 | Агентський токен у командному рядку schtask (`-token`) — видно будь-якому локальному користувачу ПК | agent/cmd/oo-agent/main.go:1273 | **FIXED** (df67966) | Агент читає токен з -token-file (ACL SYSTEM/Administrators) або env OO_AGENT_TOKEN; -token лишено для сумісності з WARNING (internal/agentcred, TestResolveTokenOrder; agent/cmd/oo-agent/README.md). Задачі планувальника на ПК треба перевести на -token-file |
| 34 | Локальний HTTP-перемикач агента (-switch-addr) / CSRF з браузера | agent/cmd/oo-agent/main.go:1257 | PASS | Слухача більше немає (керування — через control DataChannel); `grep Listen` в агенті порожній |
| 35 | Валідація подій вводу на агенті: версія, NaN/Inf, межі 0..1, невідомі типи | agent/input/input.go:153 | PASS | SecI01, SecI02 |
| 36 | Координати затиснуті в межі поверхні захоплення | agent/input/input.go:225 | PASS | SecI03 |
| 37 | Комбінації клавіш (Win+R тощо) без фільтра; немає автовідпускання затиснутих клавіш при обриві | agent/input/input.go:183 | **FIXED** (6b160a4) | Комбінації лишаються pass-through (by design), але: Injector відстежує затиснуті клавіші/кнопки й ReleaseAll шле key-up/button-up при закритті каналу вводу та на паузі (жодного видимого глядача); опційний блок-лист OO_AGENT_INPUT_BLOCK_KEYS (напр. 0xE05B,0xE05C) (agent/input/held.go). SecI04 перевернуто (TestSecI04HeldKeysReleasedAndBlocklist) |
| 38 | XSS у плеєрі (innerHTML/eval з віддаленими даними) | resources/js/remote/*.js | PASS | SecJ01 (лише textContent) |
| 39 | localStorage/sessionStorage: лише режим відображення, без квитків; сміття нормалізується | desktop-oo-webrtc.js:341-352, desktop.js:51 | PASS | SecJ02, SecJ03 |
| 40 | Offer плеєра несе одноразовий ticket, а не довгоживучий token | desktop-oo-webrtc.js:797 | PASS | SecJ04 |
| 41 | Захардкоджені секрети/ключі в репо | весь tools/oo-screen | PASS | `git grep` по PRIVATE KEY/AKIA/ghp_/sk-/xox/…=; знайдено лише dev-дефолт `t1-dev-token` (main.go:91, hub відмовляється стартувати з ним; агент має той самий фолбек), *.pem/*.key/.env немає |
| 42 | Залежності з відомими CVE | go.mod | PASS | 6929df3: golang.org/x/crypto піднято до v0.57.0 (x/net v0.58.0, x/sys v0.48.0, x/text v0.42.0). Повторний `govulncheck ./hub/... ./internal/... ./bench/... ./agent/input/...`: 0 досяжних; у модулях лишився 1 — GO-2026-5932 (x/crypto/openpgp «unmaintained», виправлення немає, код його не імпортує). Агент (windows/cgo) — UNVERIFIED. Прод збирати свіжим тулчейном (stdlib-CVE go1.26.0) |
| 43 | Дубль-канали `oosc-tiles` однієї viewer-ноги: кожен запускав tilesPump і перезаписував vl.tilesOut — витік горутин (до ~65k) | hub/cmd/hub-webrtc/tiles.go viewerTilesHandler | **FIXED** (15ddd26) | Один канал тайлів на ногу: зайві закриваються, поки vl.tilesOut != nil; помпа виходить і на закриття свого каналу (черга від'єднується). TestTilesDuplicateViewerChannels (40 каналів → 1 помпа, 0 після закриття) |
| 44 | PNG-бомба в тайлах: IHDR не звірявся з w/h заголовка, srcW/srcH без стелі (canvas 65535×65535) | internal/tiles/proto.go validate; oo-text-tiles.js parseTileMessage | **FIXED** (5df238e) | Сигнатура PNG + IHDR == w×h (дзеркало cursorproto.checkPNGHeader; у плеєрі — pngHeaderMatches з desktop-oo-cursor.js), srcW/srcH ≤ 16384 (tiles.MaxSrcSide), тайл у межах джерела, w,h ≤ 256. TestDecodeRejectsPNGBomb (45-байтна бомба), text-tiles.test.mjs |
| 45 | Застарілий курсор пізньому глядачу: липкі форма/позиція скидались лише на новий агентський канал | hub/cmd/hub-webrtc/cursor.go | **FIXED** (51f5519) | Ретранслятор скидає липкий стан на OnClose агентського каналу (крім запізнілого закриття старого) і при Failed/Closed агентського PC (agentRelaysGone). TestCursorRelayClearsOnAgentClose |

## Підсумок

- FIXED: #20 (стеля глядачів на ноду), #23 (права/O_EXCL записів), #17 (токен на ноду; strict — env),
  #21 (стеля нод + per-IP rate-limit), #26 (opt-in env), #28 (редагування тіла ERP), #32 (wt: перевірка/пінінг),
  #33 (-token-file / OO_AGENT_TOKEN), #37 (автовідпускання + блок-лист), #42 (x/crypto v0.57.0).
- Потребує дій на розгортанні: випустити токени нод і перевести агентів на -token-file, після чого
  OO_SCREEN_AGENT_AUTH=strict; за потреби OO_SCREEN_CONTROL_REQUIRES_INPUT=1, OO_SCREEN_TRUSTED_PROXIES.
- UNVERIFIED: властивості квитка на боці ERP (#6, #7), TLS/експозиція :4470 (#31), CVE агента під Windows.

## Повторний аудит

Незалежна друга перевірка HIGH/MEDIUM рядків тестами (`*_sec2_test.go`,
`__tests__/security2.test.mjs`, `deploy/test/hub-deploy-sec2-test.sh`).

| # | Що перевірено | Результат | Доказ |
|---|---|---|---|
| R1 (#17) | strict без окремого `OO_SCREEN_AGENT_SECRET`: master = T1-токен, який є на кожному ПК → будь-який ПК виковує HMAC(token, чужа нода) | **FIXED** (HIGH) | TestSec2StrictRequiresSeparateMaster; strict тепер відхиляє токени нод, якщо master == T1 (ERROR у журнал); DEPLOY.md §6 |
| R2 (#17) | Канонікалізація node_id: токен `pc1` для `PC1`, `pc1 `, кирилична `с`, fullwidth, `\x00`, порожній; constant-time | PASS | TestSec2NodeTokenNoCanonicalisation (HMAC по точних байтах, subtle.ConstantTimeCompare) |
| R3 (#17) | node_id з керівними символами/>256 байт (ін'єкція в журнал `[node=%s]`, роздування реєстру) | **FIXED** (LOW) | TestSec2AgentNodeIDValidation; порожній id лишено (T1/одновузловий режим) |
| R4 (#17) | Легасі-токен за замовчуванням приймається | PASS (як задокументовано) | дефолт сумісний; strict вимикає; аварійний відкат env |
| R5 (#21) | XFF: недовірений пір, кілька заголовків, ліве підроблене значення, сміття праворуч, IPv6-проксі, биті CIDR у списку | PASS | TestSec2XFFParsing |
| R6 (#21) | IPv6: кошик на /128 → ротація адрес у своєму /64 обходить ліміт і роздуває мапу; v4-mapped ≠ v4 | **FIXED** (MEDIUM) | TestSec2IPv6RotationSharesBucket; rateKey: IPv6 → /64, ::ffff:a.b.c.d → IPv4 |
| R7 (#20/#21) | Стелі глядачів/нод без гонок | PASS | addViewerLimit і getOrCreateNew перевіряють стелю під тим самим локом, що й вставка |
| R8 (#33) | Права файлу токена агента | PASS з зауваженням | ResolveToken не перевіряє ACL файла (Windows); ACL — відповідальність розкочування (agent-deploy.ps1 файла не створює) |
| R9 (#32) | wt cert pinning | PASS | лише leaf SHA-256, TLS1.3, без ClientSessionCache (resumption не оминає VerifyPeerCertificate); порівняння не секретне |
| R10 (#37) | Блок-лист: 0xE05B == 0x5B+extended, биті записи — помилка | PASS | TestSec2BlocklistForms |
| R11 (#43/#45) | Нові канали tiles/cursor: глядач не пише агенту | PASS (рев'ю) | на viewer-каналах немає OnMessage; лише агентський канал публікує; per-viewer черги/буфери обмежені |
| R12 (#44) | PNG-бомби в плеєрі (тайли й курсор), межі srcW/x+w, len | PASS | security2.test.mjs |
| R13 | hub-deploy.sh: журнал smoke у фіксованому `/tmp/oo-hub-smoke.log` (симлінк-атака іншого користувача VPS); `--health-url` вставлявся в `'...'` віддаленої команди | **FIXED** (LOW) | hub-deploy-sec2-test.sh; mktemp + прибирання; валідація host/URL. Host з «-» уже відсікався парсером прапорців. Відкат перевірено наявним hub-deploy-test.sh |
| R14 | CI | PASS з зауваженням | лише push/pull_request (без pull_request_target), `permissions: contents: read`, без `${{ }}` з недовірених полів у run. Зауваження: actions за тегами, не SHA; `govulncheck@latest`; hub-deploy-sec2-test.sh у CI не підключено |
| R15 | Секрети в репо | PASS | grep secret/token/password/key, PEM/AKIA/ghp_/sk-/xox: лише dev-дефолт `t1-dev-token` (хаб із ним не стартує) і тестові значення; .env/.pem/.key не трекаються |
| R16 | agent-deploy.ps1 | зауваження (LOW) | SHA256 $NewExe перевіряється до копіювання (TOCTOU, якщо каталог доступний на запис не-адміну) |
