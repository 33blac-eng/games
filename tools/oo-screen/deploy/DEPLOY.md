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
| `OO_SCREEN_ICE_TCP_ADVERTISE_PORT` **нова** | `0` (= ICE_TCP_PORT) | порт у TCP-кандидатах answer-а, коли 443 тримає nginx `stream` (§9) |
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

## 9. N5: глядач за суворим firewall-ом (назовні лише TCP 443)

Офісний firewall ріже весь UDP і всі порти, крім TCP 443. Шляхи, які глядач
пробує сам (ICE перевіряє пари паралельно, пріоритет рахує браузер — вручну
нічого вибирати не треба):

1. **UDP host** `хаб:4544/udp` — як і досі, найкращий, якщо UDP є.
2. **ICE-TCP passive на 443** — хаб сам слухає ICE-TCP (RFC 6544, кадри
   RFC 4571), браузер підключається активно. Жодного relay і жодного
   iceServer у браузера: кандидат приходить в answer-і хаба. **Основний
   запасний шлях** (реалізовано; тест `TestN5TCPOnlyViewerPlays`).
3. **TURN-TLS на 443** (coturn) — лише для firewall-ів із DPI, що на 443
   пропускають тільки справжній TLS (ICE-TCP — не TLS). Зайвий hop і TLS
   поверх TCP; вмикається через iceServers з ERP.
4. WebSocket-транспорт медіа — **не робимо**: TURN-TLS закриває той самий
   випадок стандартно, а свій транспорт означав би свій jitter-буфер і NACK
   у плеєрі.

pion/turn усередині хаба теж не вбудовуємо: хаб і так кінцева точка медіа,
тож TURN на тому самому хості не дає нічого понад ICE-TCP, крім TLS-обгортки,
— а її дає coturn без нового коду в хабі.

### 9.1 Хаб (`/etc/oo-screen/hub.env`)

```ini
OO_SCREEN_PUBLIC_IP=203.0.113.10        # публічна IP (host-кандидати, NAT 1:1)
OO_SCREEN_ICE_PORT=4544                 # UDP, як і було
OO_SCREEN_ICE_TCP_PORT=4443             # внутрішній ICE-TCP (назовні ЗАКРИТИЙ)
OO_SCREEN_ICE_TCP_ADVERTISE_PORT=443    # що оголошувати глядачу: порт nginx stream
```

Якщо 443 на сервері більше нікому не потрібен — простіше
`OO_SCREEN_ICE_TCP_PORT=443` без `_ADVERTISE_PORT` і без nginx (юніту тоді
потрібен `AmbientCapabilities=CAP_NET_BIND_SERVICE`).

### 9.2 nginx: 443 спільний для HTTPS і ICE-TCP (`stream` + `ssl_preread`)

TLS-клієнт першим шле ClientHello, ICE-TCP — STUN Binding (2 байти довжини +
STUN). Для не-TLS потоку `$ssl_preread_protocol` порожній — за цим і ділимо.
TLS ділиться ще й за SNI: `turn.example.com` -> coturn (TURN-TLS), решта ->
HTTPS.

```nginx
# /etc/nginx/nginx.conf — на верхньому рівні, ПОРУЧ із http {}
stream {
    map $ssl_preread_protocol $oo_443_kind {
        ""      ice;      # не TLS -> ICE-TCP (RFC 4571)
        default tls;
    }
    map "$oo_443_kind:$ssl_preread_server_name" $oo_443_upstream {
        ~^ice:                  oo_ice;
        "tls:turn.example.com"  oo_turns;
        default                 oo_https;
    }

    # ВАЖЛИВО: IP інтерфейсу хаба, НЕ 127.0.0.1. pion шукає ICE-сесію за
    # (ufrag, ЛОКАЛЬНА IP з'єднання), а кандидат оголошено з IP інтерфейсу;
    # з'єднання на loopback сесії не знаходить ("Failed to ping without
    # candidate pairs", ICE вічно checking). Відтворено тестом: через
    # 127.0.0.1 ICE не встає, через IP інтерфейсу — встає.
    upstream oo_ice   { server 10.0.0.5:4443; }    # приватна IP eth0 хаба
    upstream oo_turns { server 127.0.0.1:5349; }   # coturn tls-listening-port
    upstream oo_https { server 127.0.0.1:8443; }   # http{} server нижче

    server {
        listen 443;
        listen [::]:443;
        ssl_preread on;
        preread_timeout 5s;
        proxy_connect_timeout 5s;
        proxy_timeout 10m;     # ICE consent кожні ~5 с, тиші не буває
        proxy_pass $oo_443_upstream;
        # proxy_protocol тут НЕ вмикати: ні pion, ні coturn його не чекають.
    }
}

http {
    server {
        listen 127.0.0.1:8443 ssl;   # колишній listen 443 ssl
        http2 on;
        server_name erp.example.com;
        # ... ssl_certificate ..., location /oo-hub/ { proxy_pass http://127.0.0.1:4470; }
    }
}
```

Наслідок переїзду HTTPS за `stream`: http-сервер бачить клієнта як 127.0.0.1.
Якщо потрібна справжня IP (rate-limit `/offer/*`, логи) — окремий
`stream server` для HTTPS із `proxy_protocol on;` і в http
`listen 127.0.0.1:8443 ssl proxy_protocol; set_real_ip_from 127.0.0.1;
real_ip_header proxy_protocol;`; `OO_SCREEN_TRUSTED_PROXIES` лишити на
127.0.0.1. Для `oo_ice` proxy_protocol ламає STUN.

### 9.3 Firewall сервера

```bash
ufw allow 443/tcp            # nginx stream: HTTPS + ICE-TCP + TURN-TLS
ufw allow 4544/udp           # ICE UDP (основний шлях)
ufw allow 3478/udp           # coturn STUN/TURN-UDP (якщо coturn є)
ufw allow 49160:49200/udp    # coturn relay-порти (min-port/max-port нижче)
ufw deny  4443/tcp           # ICE-TCP хаба — лише через nginx
ufw deny  5349/tcp           # coturn TLS — лише через nginx
```

Офісному firewall-у клієнта нічого відкривати не треба: потрібен лише
вихідний TCP 443.

### 9.4 coturn (TURN-TLS для DPI-firewall-ів)

```ini
# /etc/turnserver.conf
listening-ip=10.0.0.5
listening-port=3478
tls-listening-port=5349          # TLS приходить від nginx stream (SNI turn.example.com)
external-ip=203.0.113.10/10.0.0.5
relay-ip=10.0.0.5
min-port=49160
max-port=49200
fingerprint
use-auth-secret
static-auth-secret=<секрет, той самий у ERP>   # REST-креди з TTL, не статичний пароль
realm=turn.example.com
cert=/etc/letsencrypt/live/turn.example.com/fullchain.pem
pkey=/etc/letsencrypt/live/turn.example.com/privkey.pem
no-tlsv1
no-tlsv1_1
no-cli
no-multicast-peers
denied-peer-ip=0.0.0.0-255.255.255.255
allowed-peer-ip=10.0.0.5          # relay ЛИШЕ до хаба
allowed-peer-ip=203.0.113.10
```

`allowed-peer-ip` обмежує relay самим хабом — інакше відкритий TURN стає
проксі в чужу мережу. `turn.example.com` має резолвитись у ту саму публічну
IP (nginx ділить за SNI). Якщо coturn слухає TLS на 127.0.0.1, додайте
`listening-ip=127.0.0.1` і `upstream oo_turns` на 127.0.0.1:5349 (як вище).

### 9.5 ERP / плеєр

Плеєр (`total-erp-app/resources/js/remote/desktop-oo-webrtc.js`) бере
`config.iceServers` через `buildRtcConfig`/`normalizeIceServers`: лише
stun/turn/turns, TURN — тільки з обліковими даними (інакше конструктор
RTCPeerConnection відкинув би весь список), порядок UDP -> TCP -> TLS,
`iceTransportPolicy: 'all'` — тож UDP завжди виграє, де він є, а TCP/TLS
підхоплюються автоматично. Для ICE-TCP хаба iceServers **не потрібні**.
Для TURN-TLS ERP віддає короткоживучі REST-креди coturn
(`username = "<unix_expiry>:<user>"`, `credential = base64(HMAC-SHA1(secret,
username))`):

```js
iceServers: [
  { urls: 'stun:turn.example.com:3478' },
  { urls: ['turn:turn.example.com:3478', 'turns:turn.example.com:443?transport=tcp'],
    username: '1791200000:viewer42', credential: '<hmac>' },
]
```

`iceTransportPolicy: 'relay'` — лише для діагностики «чи живий TURN».

### 9.6 Заміри (in-process, `hub/cmd/hub-webrtc/n5_tcp443_test.go`)

Глядач — pion з `SetNetworkTypes(TCP4)` (жодного UDP-кандидата — як браузер
за таким firewall-ом); між ним і хабом — TCP-фронт замість nginx stream; хаб
оголошує порт фронту (`_ADVERTISE_PORT`), тест перевіряє, що внутрішній порт
в answer не потрапляє і що медіа йде через фронт. Агент шле синтетичний
H.264 60 к/с, ~4 Мбіт/с, IDR раз на 2 с (+ на keyframe_request/PLI); кадр
«декодовний» = побайтно цілий і ланцюг від IDR не рвався. Лінк: однобічна
затримка 40 мс хаб -> глядач. UDP-втрата — дроп пакета + NACK pion (вікно
SRTP 1024, як у браузера). TCP-втрату ядро на loopback не відтворює, тому
вона змодельована: втрачений сегмент зупиняє ВЕСЬ потік за ним на
1×RTT+10 мс (fast retransmit) або на 200 мс (RTO — оцінка зверху, коли
кожна втрата хвостова). 30 с на сценарій:
`OO_SCREEN_N5_BENCH=1 OO_SCREEN_N5_DUR=30s go test -run N5TCPvsUDP -v ./hub/cmd/hub-webrtc/`.

| сценарій | шлях | TTFF | декодовні | лат. p50 / p95 / p99 | макс. розрив |
|---|---|---|---|---|---|
| чисто | UDP | 242 мс | 1787/1787 (100 %) | 58 / 61 / 75 мс | 109 мс |
| чисто | TCP 443 | 239 мс | 1793/1793 (100 %) | 58 / 62 / 75 мс | 55 мс |
| 1 % втрат | UDP + NACK | 342 мс | 1792/1792 (100 %) | 59 / 143 / 175 мс | 152 мс |
| 1 % втрат | TCP, fast-retx | 281 мс | 1803/1803 (100 %) | 58 / 94 / 141 мс | 151 мс |
| 1 % втрат | TCP, RTO 200 мс | 425 мс | 1762/1762 (100 %) | 292 / 1028 / 1328 мс | 601 мс |

(58 мс = 40 мс лінку + ~18 мс хаба/депакетизації; TTFF — від offer до першого
декодовного IDR, включно з ICE+DTLS через 40 мс лінк.)

Висновок: TCP 443 дає ту саму картинку, що UDP, поки втрат немає; при 1 %
втрат ціна HOL — це хвіст затримки, а не биті кадри (TCP нічого не губить).
З fast retransmit хвіст навіть коротший за UDP+NACK (NACK pion опитує раз
на 100 мс); у гіршому випадку RTO затримка накопичується до ~1 с p95 — тому
TCP лишається **запасним** шляхом, а UDP має пріоритет.

**UNVERIFIED** (потрібні справжні nginx, coturn і браузер): розбір
`ssl_preread` ICE-TCP від Chrome/Edge/Firefox, TURN-TLS через coturn за nginx
SNI, корпоративні DPI і явні HTTP-проксі (через CONNECT ICE-TCP не пройде;
TURN-TLS — лише якщо браузер сам піде через проксі), справжня динаміка TCP
(cwnd, RTO) замість моделі.
