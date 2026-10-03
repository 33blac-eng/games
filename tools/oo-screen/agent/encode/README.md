# agent/encode — hardware Media Foundation H.264 encoder (Ф0)

Реалізує §5.1 плану (енкодер — ОДИН, hardware-MFT first) і канонічний контракт
§5.2 (Annex-B, повний AU на кадр, SPS/PPS з кожним IDR, запінені профіль/рівень,
монотонний PTS). Скидання — §5.5.

```
capture (DXGI dupl -> GPU BGRA->NV12)
        │  ID3D11Texture2D (NV12), той самий ID3D11Device
        ▼
encode  MFTEnumEx(VIDEO_ENCODER, HARDWARE) -> "NVIDIA H.264 Encoder MFT"
        MF_TRANSFORM_ASYNC_UNLOCK, MF_LOW_LATENCY
        IMFDXGIDeviceManager(ResetDevice) -> MFT_MESSAGE_SET_D3D_MANAGER
        [VideoProcessorBlt NV12->NV12, якщо десктоп більший за кадр]
        MFCreateDXGISurfaceBuffer(pool slot) -> ProcessInput
        METransformNeedInput / METransformHaveOutput -> ProcessOutput
        ▼
        []AU{Data (Annex-B), Keyframe, PTS}
```

## Що зроблено

| Вимога | Стан |
|---|---|
| MFTEnumEx, MFT_CATEGORY_VIDEO_ENCODER, MFT_ENUM_FLAG_HARDWARE, перший H.264 | ✔ |
| MF_LOW_LATENCY=TRUE | ✔ |
| CODECAPI_AVLowLatencyMode | ✔ |
| CODECAPI_AVEncCommonRateControlMode = CBR + AVEncCommonMeanBitRate | ✔ |
| CODECAPI_AVEncMPVGOPSize = 2с×fps | ✔ (конфігурований через `Config.GOP`) |
| CODECAPI_AVEncMPVDefaultBPictureCount = 0 | ✔ (ffprobe: `I P P P …`, жодного B) |
| Профіль High, рівень 4.2 | ✔ (`profile_idc=100 level_idc=42`, SPS звірений `internal/h264`) |
| SPS/PPS з кожним IDR | ✔ (NVIDIA MFT повторює inband; кеш із `MF_MT_MPEG_SEQUENCE_HEADER` як запасний префікс) |
| Async MFT через IMFMediaEventGenerator | ✔ |
| MF_E_NOTACCEPTING | ✔ (кредит повертається, вихід дренується, `OOS_ENC_AGAIN`) |
| ForceIDR (IDR на запит hub-а) | ✔ `Encoder.ForceIDR()` |
| Flush+Restart (§5.5) | ✔ `Encoder.Flush()`: DRAIN → викид буферів → COMMAND_FLUSH → BEGIN_STREAMING → ForceIDR |
| Вхід або []byte, або texture handle | ✔ `encode.Frame{Y,UV,…}` або `encode.Frame{Texture}` |

## Zero-copy: **ДОВЕДЕНИЙ**

Шлях реальний, не заглушка:

1. `agent/capture` експортує `Device()` (ID3D11Device*) і `NV12Texture()`
   (ціль VideoProcessorBlt) — `oos_device` / `oos_nv12_texture` у `dxgi.c`.
2. `SetCPUReadback(false)` вимикає `CopyResource`+`Map` у `oos_next` цілком.
   Лічильник `oos_cpu_maps` рахує кожен CPU-Map NV12; у гейті він **0**.
3. Енкодер бере той самий девайс, ставить `ID3D10Multithread` protection,
   `MFCreateDXGIDeviceManager`→`ResetDevice`→`MFT_MESSAGE_SET_D3D_MANAGER`,
   і подає кадр як `MFCreateDXGISurfaceBuffer(ID3D11Texture2D)`.

Доказ у виводі гейта:

```
ENCODE_GATE zero-copy   PASS  DXGI surface path=true, NV12 CPU maps by capture=0 (want 0)
```

Тобто NV12 не мапиться на CPU **ніде** в пайплайні: ані в capture (readback
вимкнено), ані в енкодері (`MFCreateDXGISurfaceBuffer`, не `MFCreateMemoryBuffer`).

### Чесне уточнення про «zero» копій

Zero-copy тут означає «жодного CPU-map / readback», як і формулює Додаток C.
На GPU лишається один `CopyResource` (або `VideoProcessorBlt`, якщо треба
масштабувати) з постійної NV12-текстури capture у слот пулу з 6 текстур. Це
навмисно: capture перезаписує свою єдину ціль щокадру, а async-MFT тримає буфер
довше за `ProcessInput`. Прибрати і цю копію можна лише зробивши сам пул цілями
Blt у capture — це TODO, не потрібне для гейта.

## Числовий гейт Додатка C

Прогін: `agent/cmd/encode-probe`, 60с, 1080p, 8 Мбіт/с CBR, zero-copy, машина з
**GeForce GTX 650** (Kepler, NVENC 1-го покоління) — **не** «Editor» із плану.

| Пункт Додатка C | Результат |
|---|---|
| zero-copy ДОВЕДЕНИЙ | **PASS** — 0 CPU-мапів |
| encode p99 ≤ 8мс/кадр | **PASS** — ProcessInput p99 = 0.48мс, p50 = 0.14мс |
| keyframe ≤ 2× кадрового бюджету | **FAIL** — 34.05мс проти 33.3мс ліміту (60fps) |
| CPU процесу ≤ 10% | **PASS** — 1.03% |
| GPU-encode util ≤ 60% | **UNKNOWN** — без профайлера не міряємо і не вигадуємо |
| візуальний 10px-текст | **UNKNOWN** — ручна перевірка дампа |

### Чому keyframe-рядок FAIL і що це насправді означає

Метрика в пробі — `submit → AU` (наскрізна затримка), і вона включає глибину
черги async-MFT. Порівняння в тому ж прогоні:

```
worst keyframe 34.05ms vs median delta 29.48ms => сам keyframe коштує +4.57ms
```

Тобто **сам ключовий кадр укладається** в 2× бюджет із запасом; поза лімітом
його виносить постійні ~29мс конвеєра (≈2 кадри при 60fps). Гейт залишено
суворим навмисно — послаблювати критерій, щоб він позеленів, було б підміною.
Правильні наступні кроки, у порядку дешевизни:

1. Перевиміряти на «Editor» (гейт писався під нього; тут Kepler-NVENC).
2. Якщо конвеєр і там ~2 кадри — це властивість async-MFT, і критерій треба
   переписати на маргінальну вартість keyframe, а не на наскрізну затримку.
3. Якщо ні одне, ні друге — NVENC напряму, за тим самим гейтом (§5.1).

### ffprobe дампа (перші 5с)

```
codec_name=h264   profile=High   level=42
width=1920  height=1080  pix_fmt=yuv420p  field_order=progressive
pict_type: I P P P P P P P P P …   (жодного B-кадру)
```

## Відомі межі

- **NVIDIA MFT вимагає D3D-менеджер навіть для CPU-входу.** Без
  `MFT_MESSAGE_SET_D3D_MANAGER` `SetOutputType` падає з
  `MF_E_UNSUPPORTED_D3D_TYPE (0xC00D6D76)` — перевірено окремим C-пробником.
  Тому `New` створює власний ID3D11Device, коли `Config.D3DDevice` порожній;
  CPU-шлях (`Frame.Y/UV`) від цього працює, але це не zero-copy.
- **Цей GPU не кодує вище 1080p.** 2560×1440 відкидається тим самим
  `MF_E_UNSUPPORTED_D3D_TYPE`. Тому в енкодері є GPU-масштабатор
  (`Config.SrcWidth/SrcHeight` → `VideoProcessorBlt` NV12→NV12), і проба типово
  кодує 1920×1080 з 1440p-десктопа. Масштабування — на GPU, CPU-мапів не додає.
- **Рівень 4.2 не універсальний.** Вище 1080p він нелегальний, тож
  `set_output_type` йде драбиною 42 → 50 → 51 → 52 → «вибери сам»;
  `Encoder.Level()` каже, що прижилось. При 1080p це завжди 42.
- **Capture віддає одну NV12-текстуру**, яку перезаписує щокадру. Споживач
  зобов'язаний скопіювати її на GPU до наступного `NextFrame` — енкодер це
  робить, будь-який інший споживач мусить теж.
- **Пул на 6 слотів** без явного fence: при конвеєрі глибше 6 кадрів слот міг би
  бути перезаписаний до того, як MFT його дочитав. На вимірюваній глибині (~2)
  запас 3×; для Ф1 сюди треба справжній fence або `ID3D11Query`.
- **`Encode` блокує**, доки MFT не попросить вхід (`METransformNeedInput`). Це і
  є back-pressure: у гейті p99 = 14.8мс при 60fps. Admission-політика §5.5
  (дроп ДО кодування) живе вище і поки не реалізована.
- **GPU-util не міряємо.** Потрібен Nsight/GPUView; проба друкує `UNKNOWN`.
- **Курсор у пробі рухає сам процес** (`SetCursorPos` через user32) — фонова
  job не має інтерактивної віконної станції і реальний курсор не зрушить.

## TODO

- [ ] Прогнати гейт на «Editor» (Додаток C писався під цю машину).
- [ ] Fence/Query на слоти пулу замість round-robin на 6.
- [ ] Прибрати останню GPU-копію: зробити пул енкодера цілями Blt у capture.
- [ ] Візуальний 10px-текст-тест автоматизувати (зараз ручний).
- [ ] GPU-encode util — інструментувати або зафіксувати як «міряємо профайлером».

## Використання

```go
cap_, _ := capture.New(0)
cap_.SetCPUReadback(false)                  // zero-copy
w, h := cap_.Size()

enc, _ := encode.New(encode.Config{
    Width: 1920, Height: 1080, FPS: 60, BitrateBps: 8_000_000,
    D3DDevice: cap_.Device(),
    SrcWidth:  w, SrcHeight: h,             // GPU-масштаб, якщо десктоп більший
})
defer enc.Close()

f, _ := cap_.NextFrame(ctx)
aus, _ := enc.Encode(encode.Frame{Texture: f.Texture, PTS: pts})
for _, au := range aus { /* au.Data — повний Annex-B AU */ }

enc.ForceIDR()   // keyframe-request від hub-а (§5.4)
enc.Flush()      // закриття відео-epoch (§5.5): drain + flush + новий IDR
```

`Encode` **не** «один кадр → один AU»: hardware-MFT конвеєрний, тож поверненi AU
зазвичай належать раннішим кадрам. Орієнтуватись треба на `au.PTS`.

## Перевірка

```
CGO_ENABLED=1 go build ./agent/...              # Windows
GOOS=linux CGO_ENABLED=0 go build ./agent/...   # stub
go run ./agent/cmd/encode-probe -seconds 60 -fps 60 -bitrate 8000000
ffprobe "$TEMP/oo-screen-encode-probe.h264"
```
