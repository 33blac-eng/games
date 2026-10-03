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
| 17 | Один спільний агентський токен на весь парк; node обирає сам агент → будь-хто з токеном реєструє/витісняє чужу ноду й отримує ввід її глядачів; невдалий offer лишає ноду в реєстрі (ріст памʼяті) | main.go:848 | **FAIL** (високий) | SecH11 KnownFAIL. Рекомендація: токен на ноду (видає ERP, HMAC(node_id)), створювати ноду лише після успішного SetRemoteDescription |
| 18 | /nodes лише з X-OO-Hub-Key; порожній ключ = закрито | nodes.go:57 | PASS | SecH12 |
| 19 | session_id ренегоціації/visibility — 128 біт crypto/rand, constant-time | main.go:1430 | PASS | SecH13 |
| 20 | Стеля глядачів на ноду (DoS памʼяттю/горутинами) | main.go:817 | **FIXED** | SecH14; коміт `5b6bcb6` (OO_SCREEN_MAX_VIEWERS, дефолт 16, 429) |
| 21 | Глобальні ліміти: кількість нод, rate-limit /offer/viewer по IP (кожен запит = виклик ERP) | main.go:763 | **FAIL** (середній) | Немає жодного глобального ліміту чи per-IP throttle; рекомендація: rate.Limiter на IP + стеля нод |
| 22 | Кеші NACK/GOP, черги глядачів обмежені; стеля тривалості сесії 120 хв | nack.go, gop.go, fanout.go:139-170 | PASS | наявні nack_test.go, sessioncap_test.go |
| 23 | Права файлів записів (кадри чужих екранів) | record.go:369-378 | **FIXED** | SecH15; коміт `66f9961` (0700/0600, O_EXCL замість O_TRUNC) |
| 24 | Path traversal в імені запису з node_id агента | record.go:441 | PASS | SecH16 |
| 25 | Ретенція записів (вік/обсяг) | recordprune.go:87 | PASS | наявний recordprune_test.go |
| 26 | /control (перемикання монітора) приймає квиток grant=view | outputs.go:106 | **FAIL** (низький) | SecH18 KnownFAIL; view-глядач змінює картинку всіх глядачів ноди. Не виправлено: ERP-консоль може слати view-квитки на /control — потрібне погодження |
| 27 | Відповіді з помилкою не містять нутрощів і квитка | main.go:984, 742 | PASS | SecH19 |
| 28 | Тіло помилки ERP повністю йде в journald | hub/ticket.go:83, main.go:744 | **FAIL** (низький) | SecT05 KnownFAIL; рекомендація: обрізати до ~200 байт і не логувати тіло 2xx-«bad json» |
| 29 | Таймаути HTTP-сервера, заборона дефолтного токена на старті | main.go:2047, 2090 | PASS | SecH20 |
| 30 | pprof лише за явним env на окремому слухачі | main.go:2057 | PASS | рев'ю коду; адресу треба ставити 127.0.0.1 |
| 31 | TLS сигналінгу: хаб слухає plain HTTP `:4470` на всіх інтерфейсах (TLS — у nginx) | main.go:52 | UNVERIFIED | Безпечно лише якщо :4470 закритий firewall-ом; рекомендація: дефолт 127.0.0.1:4470 |
| 32 | Легасі WebTransport: InsecureSkipVerify в агенті | agent/cmd/oo-agent/main.go:196 | **FAIL** (легасі) | MITM на транспорті wt; прод — webrtc. Рекомендація: certhash-пінінг або видалити wt |
| 33 | Агентський токен у командному рядку schtask (`-token`) — видно будь-якому локальному користувачу ПК | agent/cmd/oo-agent/main.go:1273 | **FAIL** (середній) | рев'ю; разом із #17 дає захоплення будь-якої ноди. Рекомендація: файл з ACL SYSTEM/Administrators |
| 34 | Локальний HTTP-перемикач агента (-switch-addr) / CSRF з браузера | agent/cmd/oo-agent/main.go:1257 | PASS | Слухача більше немає (керування — через control DataChannel); `grep Listen` в агенті порожній |
| 35 | Валідація подій вводу на агенті: версія, NaN/Inf, межі 0..1, невідомі типи | agent/input/input.go:153 | PASS | SecI01, SecI02 |
| 36 | Координати затиснуті в межі поверхні захоплення | agent/input/input.go:225 | PASS | SecI03 |
| 37 | Комбінації клавіш (Win+R тощо) без фільтра; немає автовідпускання затиснутих клавіш при обриві | agent/input/input.go:183 | **FAIL** (низький, by design) | SecI04 KnownFAIL; Ctrl+Alt+Del SendInput і так не інʼєктує. Рекомендація: на закритті каналу слати key-up усім натиснутим |
| 38 | XSS у плеєрі (innerHTML/eval з віддаленими даними) | resources/js/remote/*.js | PASS | SecJ01 (лише textContent) |
| 39 | localStorage/sessionStorage: лише режим відображення, без квитків; сміття нормалізується | desktop-oo-webrtc.js:341-352, desktop.js:51 | PASS | SecJ02, SecJ03 |
| 40 | Offer плеєра несе одноразовий ticket, а не довгоживучий token | desktop-oo-webrtc.js:797 | PASS | SecJ04 |
| 41 | Захардкоджені секрети/ключі в репо | весь tools/oo-screen | PASS | `git grep` по PRIVATE KEY/AKIA/ghp_/sk-/xox/…=; знайдено лише dev-дефолт `t1-dev-token` (main.go:91, hub відмовляється стартувати з ним; агент має той самий фолбек), *.pem/*.key/.env немає |
| 42 | Залежності з відомими CVE | go.mod | PASS (з зауваженням) | `govulncheck ./hub/... ./internal/... ./bench/... ./agent/input/...`: 0 досяжних; 4 у модулі golang.org/x/crypto v0.54.0 (ssh, openpgp) — код їх не викликає; рекомендовано підняти до ≥ v0.56.0. Агент (windows/cgo) просканувати з цього Linux не вдалось — UNVERIFIED. govulncheck сканував stdlib go1.26.8; прод, зібраний go1.26.0, може мати stdlib-CVE — зібрати свіжим тулчейном |

## Підсумок

- FIXED: #20 (стеля глядачів на ноду), #23 (права/O_EXCL записів).
- FAIL (не виправлено, потребує дизайн-рішень): #17 спільний агентський токен + вибір node агентом (найвищий ризик),
  #21 відсутні глобальні/IP-ліміти, #33 токен у командному рядку, #32 InsecureSkipVerify (легасі wt),
  #26 /control з view-квитком, #28 тіло ERP у журналі, #37 комбінації клавіш/застряглі клавіші.
- UNVERIFIED: властивості квитка на боці ERP (#6, #7), TLS/експозиція :4470 (#31), CVE агента під Windows.
