# agent/capture — DXGI Desktop Duplication → NV12 (Ф0, план §4)

Джерело кадрів для агента: `IDXGIOutputDuplication` + **персистентний D3D11
конвеєр BGRA→NV12 на GPU**. Гілка паралельна до транспортних спайків —
підключення до енкодера/транспорту буде пізніше.

## Файли

| файл | що це |
|---|---|
| `dxgi.h` / `dxgi.c` | C-шар: COM, duplication, курсор, VideoProcessor |
| `capture_windows.go` | cgo-обгортка: `Capturer`, типізовані помилки, state-machine перестворення |
| `capture_stub.go` | `!windows` заглушка — `go build ./...` не ламається на Linux |
| `../cmd/capture-probe` | CLI-доказ що конвеєр живий |

> ⚠️ `dxgi.h` називається саме так, як системний заголовок, тому він **затіняє**
> `<dxgi.h>` для всіх, хто його тягне (а `d3d11.h` тягне). Тому перший рядок —
> `#include_next <dxgi.h>`: спершу підтягуємо справжній, потім оголошуємо своє.
> Без цього збірка падає на `unknown type name 'DXGI_FORMAT'`.

## Конвеєр одного кадру

```
AcquireNextFrame(timeout = кадровий бюджет)
  └─ CopyResource → власна BGRA-текстура     (acquired-текстура живе лише до ReleaseFrame)
  └─ ReleaseFrame                            (тримати захоплений кадр = гальмувати композитор)
  └─ композит курсора В BGRA-текстуру
  └─ ID3D11VideoContext::VideoProcessorBlt   BGRA → NV12, на GPU
  └─ CopyResource → NV12 staging + Map       ← Ф0-only readback
```

Конвеєр (device, обидві текстури, VideoProcessor, input/output views)
створюється **один раз** у `oos_open` і переживає всі кадри. Перестворюється
лише разом із duplication.

### Обробка статусів AcquireNextFrame

* `DXGI_ERROR_WAIT_TIMEOUT` — **не кадр і не помилка**. `NextFrame` просто йде
  на нове коло. Наслідок: на абсолютно статичному екрані `NextFrame` блокується
  до `ctx`-дедлайну — давай контекст із таймаутом, якщо треба «новин нема».
* `ACCESS_LOST` / `DEVICE_REMOVED` / `DEVICE_RESET` / `INVALID_CALL` /
  `E_ACCESSDENIED` — сигнал перестворення. `Capturer.reinit` рве все і будує
  наново з експоненційним backoff (10мс → 500мс, до 12 спроб). Це покриває
  rotation, зміну роздільної здатності, hotplug, lock/unlock, GPU reset.
  `ErrAccessLost` виходить назовні **лише** коли перестворення провалилось увесь
  бюджет спроб.
* `E_ACCESSDENIED` **на етапі відкриття** (`DuplicateOutput`) — це не транзієнт,
  а «ми в RDP-сесії / на secure desktop». Окрема помилка `ErrNotAvailable`;
  ретраї не допоможуть, поки сесія не зміниться.

### Курсор

DXGI ніколи не малює вказівник у десктоп-текстуру — віддає окремо
(`PointerPosition` + `PointerShapeBuffer`). Ми композитимо його **ДО**
конвертації, бо інакше рух миші не породжує зміненого кадру і «кодуй лише
зміну» губить курсор (Codex R2#14).

Реалізовано **всі три** типи shape:

* `MONOCHROME` — 1bpp, `Height` подвійна: AND-маска зверху, XOR-маска знизу.
* `COLOR` — BGRA, source-over по власній альфі.
* `MASKED_COLOR` — альфа=0 → замінити піксель, альфа=0xFF → XOR з десктопом.

Спосіб: `CopySubresourceRegion` прямокутника курсора в маленьку staging-текстуру
→ CPU-блендинг → `UpdateSubresource` назад у BGRA-текстуру. Це кілька КБ на
кадр, і воно коректне для XOR/AND типів, яким потрібні пікселі підкладки.

DXGI шле shape лише коли він змінився — тому shape кешується в `oos_cap`.

## Гейт (виконано на цій машині, 2560×1440)

```
$ CGO_ENABLED=1 go build ./agent/cmd/capture-probe     # exit 0
$ ./capture-probe.exe -n 120 -timeout 45s
outputs=1 using=0
opened output=0 size=2560x1440 budget=33ms
frame=60  ... cursor=true/color@1040,2 composited=true y64k_sha256=2aca38dddf913049..CHANGED p50=16.7ms  p95=17.54ms
frame=120 ... cursor=true/color@546,12 composited=true y64k_sha256=e2a5dfe17eb56d0d..CHANGED p50=16.55ms p95=17.62ms

frames=120 real=3 mouse_only=117 distinct_y64k_hashes=118 elapsed=1.83919s effective_fps=65.2
acquire+convert p50=16.55ms p95=17.62ms min=3.36ms max=28.7ms
CAPTURE_OK frames=120
```

118 різних SHA-256 на 120 кадрів — і при цьому 117 із них `mouse_only`
(десктоп не мінявся). Тобто **композит курсора реально доїжджає до Y-плейну
NV12**. Це найсильніший доказ, який тут можна отримати: змінюється саме те, що
ми домальовуємо.

`real=3` — це справжні презенти десктопу. На статичному екрані їх мало; щоб
кадри взагалі йшли, курсор рухався через
`[System.Windows.Forms.Cursor]::Position`.

## Числа

* **p50 = 16.6 мс, p95 = 17.6 мс** (min 3.4 / max 28.7) на 2560×1440.
* Ефективні 65 fps при кадровому бюджеті 33 мс.
* ⚠️ Ці числа **включають Ф0-readback** (CopyResource в staging + Map + два
  memcpy ~5.5 МБ на кадр). Це і є домінанта p50 — не сам DXGI і не Blt.
  Zero-copy шлях до MFT прибирає майже все це.

## TODO

* **Zero-copy до енкодера** — головне. Зараз `NV12Frame.Y/UV` — це CPU-копії,
  бо енкодера ще нема і кадр мусить дійти в Go байтами. Правильний шлях:
  віддавати `ID3D11Texture2D` (NV12) прямо в hardware-MFT через
  `MF_SA_D3D11_AWARE` + `IMFDXGIDeviceManager` + `MFCreateDXGISurfaceBuffer`.
  Тоді `Y`/`UV` стають `nil`, а поруч з'являється handle текстури.
* **Композит курсора в compute-shader** замість CPU-блендингу. Поточний варіант
  коректний і дешевий (кілька КБ), але робить два дрібні GPU↔CPU переходи на
  кадр; на 240 Гц це помітно.
* **Rotation** — читаємо `DXGI_OUTDUPL_DESC.ModeDesc` (орієнтація текстури,
  не поверненого десктопу). При 90°/270° кадр приїде «лежачи»; поворот у
  VideoProcessor (`VideoProcessorSetStreamRotation`) ще не виставляється.
* **Move/dirty rects** — свідомо НЕ реалізовані (план §4: оптимізація після Ф0).
  Завжди повний кадр.
* **Мультимонітор** — `New(outputIdx)` бере один вихід адаптера 0.
  Мультиадаптерний enum і склейка кількох виходів — поза Ф0.
* **Secure desktop / екран логіна** — DXGI там не працює взагалі (`E_ACCESSDENIED`).
  GDI-фолбек — Ф3. Зараз це чесно повертається як `ErrNotAvailable`.
* **HDR / scRGB виходи** — зараз жорстко `B8G8R8A8_UNORM`. На HDR-дисплеї
  duplication віддасть `R16G16B16A16_FLOAT` і `CopyResource` впаде на
  невідповідності форматів. Треба читати формат із `DXGI_OUTDUPL_DESC` і
  вибирати tone-mapping.
* **Колірний простір** зашитий BT.709 studio-range на виході. Для 4K/HDR
  доведеться параметризувати.
* **`Capturer` не потокобезпечний** — уся D3D11-робота на горутині, що кличе
  `NextFrame`. Буфери `Y`/`UV` перевикористовуються між кадрами, тож кадр треба
  спожити до наступного `NextFrame`.
