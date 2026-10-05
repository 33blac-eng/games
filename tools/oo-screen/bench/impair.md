# Impairment на Windows — clumsy для T1 bake-off

Дзеркалить траси Додатка A (план `docs/superpowers/plans/2026-08-26-oo-screen-core-plan.md`,
рядки 285-321). Мета: чесно показати, що clumsy МОЖЕ і що НЕ МОЖЕ з цих 8 трас,
і що робити з рештою.

## Де взяти

- clumsy 0.3: https://github.com/jagt/clumsy/releases (Windows-only WinDivert-based
  impairment tool, portable .exe + WinDivert.dll/.sys поруч, без інсталятора).
- Розпакувати у постійне місце (наприклад `C:\tools\clumsy\`), запускати з
  **правами адміністратора** (WinDivert потребує driver-рівня доступу).
- Порти T1-стенду фіксовані в README.md: `4460/udp` (QUIC ingest агент->hub B),
  `4461/udp` (WebTransport/HTTP-3 браузер->hub B). Кандидат A (WebRTC/HTTP
  signaling) також зрештою ганяє медіа по UDP (ICE/SRTP) — саме ці потоки й
  ловить clumsy filter нижче, не порт 4470 (то TCP-сигналінг).

## Базовий WinDivert-фільтр для T1-стенду

```
udp and (udp.DstPort == 4460 or udp.DstPort == 4461 or udp.SrcPort == 4460 or udp.SrcPort == 4461)
```

Для кандидата A (WebRTC UDP-медіа) порт наперед невідомий (ICE обирає
динамічно) — або звузити filter до конкретного локального порту, який
призначить агент/браузер (подивитись у `chrome://webrtc-internals` /
netstat під час запуску), або (простіше і надійніше для T1) фільтрувати по
IP-парі hub<->машина-viewer:

```
udp and ip.DstAddr == <hub_ip> and ip.SrcAddr == <viewer_or_agent_ip>
```

## Мапінг профілів S2-S7 на прапорці clumsy

| Траса | Що вимагає План | clumsy-модуль | Прапорці |
|---|---|---|---|
| S2 loss-low | 1% loss, RTT 20мс | Drop + Lag | `--drop chance=1` (uniform), `--lag chance=100 time=10` (RTT/2 на кожному боці ≈20мс round-trip) |
| S3 loss-mid | 3% loss, RTT 60мс | Drop + Lag | `--drop chance=3`, `--lag chance=100 time=30` |
| S4 loss-burst | 5% burst (Gilbert-Elliott p=.05,r=.4), RTT 60мс | **Наближення через Drop, НЕ точна GE-модель** | `--drop chance=5` (uniform!) + `--lag time=30`; див. "Чого НЕ вміє" нижче |
| S5 reorder | 2% reorder gap5, RTT 60мс | Out of Order | `--out-of-order chance=2 time=30` (clumsy не має параметра "gap 5" — див. нижче), `--lag time=30` |
| S6 far | RTT 120мс, 0.5% loss | Lag + Drop | `--lag chance=100 time=60`, `--drop chance=0.5` |
| S7 bw-collapse | 12→2 Мбіт/с сходинкою на 60с, RTT 60мс | Throttle (частково) | `--throttle chance=100 [профіль нижче]`, `--lag time=30` |
| S8 udp-blocked | UDP dropped повністю | Drop | `--drop chance=100` (простий і точний кейс — тут clumsy достатньо) |
| S1 clean | 0% loss, RTT 20мс | (нічого) | без clumsy, baseline |

Загальна нотатка по `--lag`: clumsy `time=N` додає затримку N мс **на кожному
пійманому пакеті в одному напрямку**; щоб отримати симетричний RTT, фільтр
має ловити пакети в ОБИДВА боки (наш filter вище це робить SrcPort/DstPort
парою), тоді додана затримка на пакет ≈ RTT/2.

## Чого clumsy НЕ вміє чесно (і як апроксимувати)

1. **Gilbert-Elliott burst-loss модель (S4)**. clumsy `--drop chance=N` —
   uniform Bernoulli drop, НЕ двостанова марковська модель з параметрами
   p=.05 (перехід good->bad), r=.4 (перехід bad->good). Наслідок: втрати
   рівномірно розкидані, а не пачками — trace НЕ відтворює характерний
   для burst-loss ефект (кілька підряд втрачених кадрів -> довший freeze,
   ніж дає той самий % при uniform drop). Апроксимація для T1: підняти
   `--drop chance` вище номінальних 5%, щоб емулювати гіршу картину, і
   явно позначити результат як "верхня межа, не точна GE-трасса". Чесно
   ганяти GE-модель — потрібен `netem` (Linux tc) з `loss gemodel`, або
   власний скрипт поверх WinDivert, що керує краплями пакетів за станом
   марковського ланцюга (можна написати, поза обсягом T1).

2. **Точний "reorder gap5" (S5)**. clumsy `--out-of-order` переставляє
   пакети в межах вікна `time=N`, але не має параметра "затримати кожен
   5-й пакет" (gap 5), як у Linux `tc netem reorder ... gap 5`. Наслідок:
   розподіл переставлених пакетів у clumsy не той самий, що дає netem
   `gap`, хоч частка (2%) можна виставити наближено через `chance`.
   Апроксимація: прийняти `--out-of-order chance=2 time=30` як proxy,
   позначити в звіті, що точний `gap5`-патерн не відтворено.

3. **Bandwidth-сходинка 12->2 Мбіт/с (S7)**. clumsy `--throttle` затримує
   пакети понад заданий ліміт, але керується вручну (пороги в GUI/командах
   мають бути виставлені й ЗМІНЕНІ ВРУЧНУ рівно на 60-й секунді прогону —
   немає вбудованого профілю "сходинка за розкладом"). Апроксимація для
   T1: два послідовні запуски clumsy-процесу через `--duration` (якщо
   версія підтримує) або зовнішній таймер-скрипт (PowerShell `Start-Job`
   + `Start-Sleep 60` + перезапуск clumsy з іншим throttle-лімітом),
   що перезапускає clumsy з новим лімітом рівно на позначці 60с. Різкість
   переходу (drop-to-2mbit vs step) не гарантована так само чисто, як
   `tc qdisc` change events на Linux.

4. **Bandwidth-ліміт взагалі (throttle точність)**. clumsy `--throttle`
   реалізований як crude token-bucket поверх WinDivert; для реалістичного
   TBF/HTB shaping (як `tc qdisc add ... tbf`) точність гірша, особливо на
   малих інтервалах (перші секунди після зміни ліміту).

**Підсумок — які траси на Windows-clumsy НЕ відтворюються чесно:**
S4 (Gilbert-Elliott burst), S5 (точний reorder gap5), S7 (bandwidth-сходинка
з чистим переходом). Ці три варто ганяти на Linux hub з `netem` (наступний
крок після T1, поза Windows-стендом), або трактувати clumsy-результат як
верхню/наближену межу з явною приміткою в score.py/звіті. S1, S2, S3, S6, S8
clumsy відтворює прийнятно чесно (uniform loss + симетричний lag —
стандартний випадок, який WinDivert-based інструменти роблять добре).

## Перевірка, що clumsy бачить UDP 4460/4461

1. Запустити hub кандидата B (`bench/run_b.sh`) і почати передачу
   (агент або corpus-player шле UDP на 4460, viewer підключається на 4461).
2. Запустити clumsy (адмін-права) з filter:
   ```
   udp and (udp.DstPort == 4460 or udp.DstPort == 4461 or udp.SrcPort == 4460 or udp.SrcPort == 4461)
   ```
   і увімкнути лише `--drop chance=100` тимчасово (найпростіший detector:
   якщо потік миттєво зупиняється — clumsy бачить трафік).
3. Перевірка без clumsy (базова видимість трафіку, не залежить від clumsy):
   ```powershell
   netstat -ano -p UDP | findstr ":4460 :4461"
   ```
   очікується рядок з локальною адресою на 4460 і 4461 у стані активного
   сокета (UDP не має "ESTABLISHED", але рядок має з'явитись, поки hub
   слухає).
4. Альтернативна перевірка захоплення пакетів (Wireshark/`netsh trace`,
   опційно): фільтр `udp.port == 4460 or udp.port == 4461` має показувати
   вхідні/вихідні пакети під час активного прогону — якщо порожньо, або
   WinDivert-фільтр clumsy не збігається з реальними адресами
   (SrcAddr/DstAddr localhost vs LAN IP — WinDivert по-різному бачить
   loopback залежно від версії Windows/WinDivert, для T1-стенду на одній
   машині варто явно перевірити цей край-кейс), або firewall/адмін-права
   блокують WinDivert driver.
5. Якщо hub і viewer на РІЗНИХ машинах: WinDivert ловить лише трафік, що
   реально проходить через мережевий стек машини, де запущено clumsy —
   для симетричного impairment (обидва боки RTT) clumsy треба ставити на
   hub-машину (де сходяться обидва UDP-порти), а не на клієнта.
