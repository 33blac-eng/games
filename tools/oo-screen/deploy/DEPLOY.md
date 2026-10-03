# Викочування oo-screen на прод (хаб + агенти)

Комплект готувався БЕЗ доступу до прода. Нічого з цього ще не запускалось
проти справжнього VPS — лише локальний тест із заглушками
(`deploy/test/hub-deploy-test.sh`). Перший прогін — з `--dry-run`.

## 0. Головне правило: ПАРА, хаб перший

З `PLAN-waves.md`:

- агент і хаб збираються з **одного коміту** і їдуть **разом**, навіть якщо
  здається, що зміна «тільки в агенті» (випадок 31.08: новий агент проти
  старого хаба — `publisher lost` кожні ~25 с);
- **ХАБ ПЕРШИЙ**: новий агент проти старого хаба флапає кожні ~10 с (сторож
  живості агента бʼється від ctl-повідомлень, а старий хаб після першого
  `sendGate` мовчить); старий агент проти нового хаба — допустимий
  перехідний стан на час розкочування;
- відкат — у **зворотному** порядку: спершу агенти, потім хаб;
- md5/sha256-дисципліна: хеш бінаря фіксується до і після кожного кроку.

`build.sh` пише обидва бінарі + `VERSION` + `SHA256SUMS` з одного коміту,
`agent-deploy.ps1` відмовляється ставити агента, якщо `-ExpectedHubVersion`
(те, що зараз на хабі) не дорівнює `VERSION` агента.

## 1. Збірка

```bash
cd tools/oo-screen
deploy/build.sh            # брудне дерево = відмова
cat deploy/dist/SHA256SUMS
```

Потрібні Go і `x86_64-w64-mingw32-gcc` (агент — cgo). Результат у `deploy/dist/`:
`hub-linux-amd64`, `oo-node-token-linux-amd64`, `oo-agent-windows-amd64.exe`,
`VERSION`, `SHA256SUMS`.

## 2. Одноразова підготовка сервера

`hub-deploy.sh` очікує, що юніт запускає **симлінк**:

```ini
# /etc/systemd/system/oo-hub.service (фрагмент)
[Service]
WorkingDirectory=/opt/oo-screen
ExecStart=/opt/oo-screen/hub
EnvironmentFile=/etc/oo-screen/hub.env   # chmod 600, тут секрети
Restart=on-failure
```

Перший раз: покладіть поточний робочий бінар як `/opt/oo-screen/hub.<старий-sha>`
і `ln -sfn hub.<старий-sha> /opt/oo-screen/hub` — тоді скрипту буде на що
відкочуватись. Ssh-користувачу потрібен `sudo systemctl restart <unit>` без
пароля (або ssh під root і `OO_DEPLOY_SUDO=""`), а також `journalctl -u <unit>`.

Каталог записів (якщо `OO_SCREEN_RECORD=1`): хаб сам звузить права наявного
каталогу до 0700 на першому записі; файли — 0600.

## 3. Хаб

```bash
deploy/hub-deploy.sh --dry-run my-vps oo-hub.service   # подивитись
deploy/hub-deploy.sh my-vps oo-hub.service             # викотити
```

Що робить:
1. звіряє `hub-linux-amd64` з `SHA256SUMS`;
2. кладе `hub.<sha>` поруч зі старими (пропускає, якщо вже є з тим самим SHA256);
3. **preflight**: у хаба немає `-version`, тому новий бінар стартує пробно на
   `127.0.0.1:14470` (ICE на 14471, запис вимкнено, одноразовий `OO_SCREEN_T1_TOKEN` — з дефолтним хаб не стартує) і має відповісти `/healthz`
   за 10 с — інакше прод не чіпаємо;
4. атомарно перемикає симлінк (`ln -sfn` + `mv -T`) і робить `systemctl restart`;
5. до `--timeout` (60 с) чекає: юніт `active`, `GET /healthz` = 200, у журналі
   юніта з моменту рестарту нема `panic:`/`fatal error:` (і ще 3 с після цього);
6. інакше — **автоматичний відкат** симлінка на попередній `hub.<sha>` і рестарт.

Повторний запуск тієї ж версії — лише перевірка здоровʼя, без рестарту.
Опції: `--remote-dir`, `--health-url`, `--timeout`, `--smoke-port`, `--dist`.

Після хаба вручну: `curl -s http://127.0.0.1:4470/healthz` і `/nodes` з VPS,
переконатися, що старі агенти підʼєднались (старий агент + новий хаб — норма
на час розкочування).

## 4. Агенти (лише після здорового хаба тієї ж версії)

На кожному ПК, адміністратором:

```powershell
.\agent-deploy.ps1 -NewExe .\oo-agent-windows-amd64.exe -VersionFile .\VERSION `
    -ExpectedHubVersion <sha з hub.<sha> на VPS> -Sha256 <з SHA256SUMS> `
    -InstallPath 'C:\Program Files\oo-screen\oo-agent.exe' -TaskName oo-agent `
    -LogPath 'C:\ProgramData\oo-screen\oo-agent.log'
```

`-WhatIf` — сухий прогін. Скрипт: зупиняє задачу, робить `oo-agent.exe.bak-<час>`,
підміняє бінар, стартує задачу, до 60 с чекає процес і в лозі рядок
`oo-agent: webrtc PC state: connected` (без `panic:`); інакше повертає бекап.
**Це чернетка**: шляхи, імʼя задачі та `-log` звірте з тим, як агент реально
встановлений на ПК (агент має запускатись із `-log`, інакше перевіряти нічого).
Спершу 1 ПК, 5 хв спостереження, потім решта.

## 5. Токени нод (SEC #17)

Кожна нода отримує власний токен `hex(HMAC-SHA256(master, node_id))`:

```bash
# на VPS, master — той самий, що OO_SCREEN_AGENT_SECRET у hub.env
sudo sh -c 'umask 077; grep ^OO_SCREEN_AGENT_SECRET= /etc/oo-screen/hub.env | cut -d= -f2- > /root/agent.secret'
./oo-node-token-linux-amd64 -node PC-042 -secret-file /root/agent.secret > PC-042.token
```

Секрет не передається в командному рядку. На ПК токен кладеться у файл з ACL
лише SYSTEM/Administrators і передається агенту `-token-file <шлях>`
(або env `OO_AGENT_TOKEN`). `-token` у командному рядку застарілий.
`-node` агента мусить точно збігатися з `-node` при випуску токена.

## 6. Перехід на OO_SCREEN_AGENT_AUTH=strict

За замовчуванням хаб приймає і токен ноди, і спільний легасі-токен
(`OO_SCREEN_T1_TOKEN`), попереджаючи в журналі раз на ноду.

1. Задати `OO_SCREEN_AGENT_SECRET` (новий випадковий, ≥32 байти) у `hub.env`,
   викотити хаб. Поки режим не strict — старі агенти працюють на легасі-токені.
2. Випустити токени для всіх нод (крок 5), розкласти на ПК, викотити агентів
   з `-token-file`.
3. Чекати, поки з журналу зникнуть попередження про легасі-токен для всіх
   нод: `journalctl -u oo-hub --since -1h | grep "легасі-токені"`.
4. Додати `OO_SCREEN_AGENT_AUTH=strict`, рестарт хаба
   (`systemctl restart oo-hub`). Перевірити `/nodes` — усі ноди на місці.
5. Аварійний вихід без відкату бінаря: `OO_SCREEN_LEGACY_AGENT_TOKEN=1`
   знову дозволяє легасі-токен навіть у strict.

Увага: якщо `OO_SCREEN_AGENT_SECRET` порожній, master = `OO_SCREEN_T1_TOKEN`,
тобто зміна T1-токена інвалідує всі токени нод. Гірше: T1-токен є на КОЖНОМУ ПК,
тож будь-який ПК сам обчислює токен чужої ноди. Тому в strict хаб без окремого
`OO_SCREEN_AGENT_SECRET` (або з ним == `OO_SCREEN_T1_TOKEN`) токени нод НЕ приймає
(ERROR у журналі) — крок 1 обовʼязковий.

## 7. Відкат

- **Хаб, автоматично**: `hub-deploy.sh` сам повертає симлінк, якщо нова версія
  не здорова.
- **Хаб, вручну** (виявилось пізніше):
  ```bash
  ssh my-vps 'ls -l /opt/oo-screen/'          # які hub.<sha> є
  deploy/hub-deploy.sh --dist <dist попередньої збірки> my-vps oo-hub.service
  # або напряму:
  ssh my-vps 'cd /opt/oo-screen && ln -sfn hub.<старий> hub.next && mv -Tf hub.next hub && sudo systemctl restart oo-hub'
  ```
- **Пара**: спершу агенти на попередню версію (бекап `.bak-<час>` поруч з exe:
  зупинити задачу, скопіювати бекап на місце, стартувати), потім хаб.
  Новий хаб + старі агенти — допустимо; старий хаб + нові агенти — ні.
- **strict**: прибрати `OO_SCREEN_AGENT_AUTH=strict` (або `OO_SCREEN_LEGACY_AGENT_TOKEN=1`) і рестарт.
- Старі `hub.<sha>` не видаляються автоматично; прибирати вручну, лишаючи ≥2 останні.

## 8. Змінні середовища

Позначка **нова** — зʼявилась у цій гілці (SEC-аудит та хвилі).
Порожнє значення = дефолт.

### Хаб (`hub/cmd/hub-webrtc`, `hub/`)

| Змінна | Дефолт | Сенс |
|---|---|---|
| `OO_SCREEN_HUB_ADDR` | `:4470` | адреса HTTP-сигналінгу |
| `OO_SCREEN_T1_TOKEN` | — (дефолт `t1-dev-token` хаб відхиляє і не стартує) | спільний (легасі) токен агента і T1-токен глядача; обовʼязковий |
| `OO_SCREEN_AGENT_SECRET` **нова** | = `OO_SCREEN_T1_TOKEN` | master для токенів нод (HMAC) |
| `OO_SCREEN_AGENT_AUTH` **нова** | (не strict) | `strict` — лише токени нод, легасі-токен відхиляється |
| `OO_SCREEN_LEGACY_AGENT_TOKEN` **нова** | вимк. | `1` — дозволити легасі-токен навіть при strict (аварійно) |
| `OO_SCREEN_AGENT_NODE_ID` | порожньо | node для старого агента без `-node` |
| `OO_SCREEN_ERP_BASE` | порожньо | URL ERP; непорожній вмикає ticket-режим для глядачів |
| `OO_SCREEN_HUB_KEY` | порожньо | ключ хаба для ERP |
| `OO_SCREEN_REVOKE_STALE_AFTER` | `90s` | скільки терпіти недоступність ERP, перш ніж рвати сесії (fail-closed) |
| `OO_SCREEN_REVOKE_TIMEOUT` | `8s` | таймаут одного запиту відкликань до ERP |
| `OO_SCREEN_ICE_PORT` | = `OO_SCREEN_UDP_PORT_MIN` або `4544` | єдиний UDP-порт ICE-mux |
| `OO_SCREEN_UDP_PORT_MIN` | `4544` | легасі, дефолт для ICE_PORT |
| `OO_SCREEN_ICE_TCP_PORT` | `0` (вимк.) | порт ICE-TCP |
| `OO_SCREEN_PUBLIC_IP` | авто | публічний IP для host-кандидатів (NAT 1:1) |
| `OO_SCREEN_STUN_URLS` | порожньо | STUN через кому |
| `OO_SCREEN_TURN_URL` / `_USER` / `_PASS` | порожньо | TURN; лише всі три разом |
| `OO_SCREEN_MAX_NODES` **нова** | `500` | стеля нод у реєстрі (понад — 503) |
| `OO_SCREEN_MAX_VIEWERS` **нова** | `16` | стеля глядачів на ноду |
| `OO_SCREEN_OFFER_RATE` **нова** | `1` | per-IP запитів/с на `/offer/*`; `0` вимикає |
| `OO_SCREEN_OFFER_BURST` **нова** | `10` | сплеск для rate-limit |
| `OO_SCREEN_TRUSTED_PROXIES` **нова** | `127.0.0.0/8,::1` (не задано); порожнє = без довіри | IP/CIDR через кому, чиєму `X-Forwarded-For` віримо |
| `OO_SCREEN_CONTROL_REQUIRES_INPUT` **нова** | вимк. | `1` — `/control` лише з grant=control у тікеті |
| `OO_SCREEN_SESSION_CAP` | `120m` | максимальна тривалість сесії глядача |
| `OO_SCREEN_GOP_SPAN` | `12s` (3s…30s) | глибина GOP-кешу; більше за `-gop-seconds` агента |
| `OO_SCREEN_GOP_REPLAY_SPAN` | `2s` | бюджет віддачі кешу новому глядачеві: бітрейт × span × 1,25; більший хвіст → keyframe-запит |
| `OO_SCREEN_GOP_REPLAY_MAX_BYTES` | `3145728` | жорстка стеля віддачі кешу (≤ 12 МБ); `0` — кеш не віддавати, завжди IDR |
| `OO_SCREEN_START_BITRATE` | `8000000` | стартовий бітрейт, біт/с |
| `OO_SCREEN_BITRATE_FASTUP` | вимк. | `1` — швидке відновлення бітрейту |
| `OO_SCREEN_STRICT_CODEC` | вимк. | `1`/`true` — рвати сесію при неузгодженому профілі H.264 |
| `OO_SCREEN_AUDIO` | вимк. | `1` — аудіо-доріжка (і на агенті) |
| `OO_SCREEN_INPUT` | вимк. | `1` — канал вводу (і на агенті) |
| `OO_SCREEN_RECORD` | вимк. | `1` — запис сесій у MKV |
| `OO_SCREEN_RECORD_DIR` | `recordings` | каталог записів (0700, файли 0600; 14 днів / 8 ГіБ) |
| `OO_SCREEN_PPROF_ADDR` | порожньо | адреса pprof (окремий слухач); ставити лише 127.0.0.1:порт |
| `OO_SCREEN_METRICS_ADDR` | порожньо (вимк.) | адреса окремого слухача Prometheus `/metrics` (мітка `node`: ноди, глядачі, ingress bps/fps, keyframes/хв, ціль бітрейту + причина, loss/RTT max/avg, NACK/PLI, кеш GOP, TTFF, черги egress/глядачів, процес); ставити лише 127.0.0.1:порт |

### Агент (`agent/`, `internal/agentcred`)

| Змінна | Дефолт | Сенс |
|---|---|---|
| `OO_AGENT_TOKEN` **нова** | порожньо | токен ноди (нижче за `-token-file`, вище за `-token`) |
| `OO_SCREEN_T1_TOKEN` | `t1-dev-token` | легасі-фолбек токена агента |
| `OO_AGENT_INPUT_BLOCK_KEYS` **нова** | порожньо | скан-коди через кому (`0xE05B,0xE05C`), які агент не вводить; помилка в списку = відмова |
| `OO_SCREEN_PACER` **нова** | вимк. | `1` = рівномірна відправка RTP (~2× цілі бітрейту, черга ≤ 75 мс); допомагає під стелею каналу ~8 Мбіт/с, додає до 75 мс затримки |
| `OO_SCREEN_AUDIO` | вимк. | `1` = `-audio` |
| `OO_SCREEN_INPUT` | вимк. | `1` = `-input` |

Стендові: `OO_SCREEN_HUB_URL` (corpus-player), `OO_SCREEN_T1_TOKEN` у `hub-wt`/`loadgen`.
