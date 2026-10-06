# Світове порівняння 2026: різкий текст і низька затримка (TASK.md, кроки 2–3)

Дата: 06.10.2026 · гілка `w8-research` (база `b408e40`) · код **не змінювався**, це лише дослідження і план.

Документ доповнює, а не повторює наявні таблиці:
`COMPARISON.md` (32 параметри транспорту), `parsec-moonlight.md`, `rustdesk-anydesk.md`, `crd-rdp-teamviewer.md`, `../RESEARCH-leaders.md` (20 параметрів, 03.10).
Тут тільки те, що стосується **різкості тексту і затримки**: 4:4:4, інструменти для екранного вмісту, dirty rects, дошліфування статики, адаптивний QP, курсор, керування бітрейтом. Для кожної техніки є три відповіді: чи є вона в цьому репозиторії, чи можна її застосувати в Chrome і скільки це коштує.

**Позначки.**
- ✅ — перевірено за першоджерелом, відкритим у цій сесії (сирці або документація; посилання поруч).
- (s) — бачив лише уривок у пошуку: сторінку заблокував проксі або вона не відкрилась.
- **UNVERIFIED** — висновок або оцінка, яку тут не перевірено ні кодом, ні заміром.
- «Симуляція» — цифри з `bench/quality` (x264, libvpx, libaom на Linux). Це не реальний ПК.

Доступ із сесії. Відкривались `raw.githubusercontent.com` (дзеркала Chromium, libwebrtc `webrtc-sdk/webrtc`, Sunshine, Moonlight, RustDesk, x264). Проксі заблокував `webrtc.googlesource.com`, `chromium.googlesource.com`, `learn.microsoft.com`, `support.parsec.app`, `support.anydesk.com`, `developer.nvidia.com`, `devblogs.microsoft.com` і `web.archive.org`. Тому все про Parsec і AnyDesk тут або з попередніх таблиць, або (s).

---

## 0. Коротко

1. **Обмеження Chrome формулювали неточно.** `TASK.md` каже: «Chrome приймає через WebRTC лише H.264 Constrained Baseline/Main». За сирцями libwebrtc і Chromium Chrome **на прийомі** оголошує й декодує ще три 4:4:4-формати. Усі три декодуються програмно.
   - **H.264 High 4:4:4 Predictive** (`profile-level-id=f4001f`): FFmpeg приймає `YUV444P`.
   - **VP9 profile 1**: libvpx.
   - **AV1 profile 1**: dav1d.

   Те саме видно в нашому ж коді: у коментарі `hub/cmd/hub-webrtc/main.go:56-62` записано, що живий Chrome 05.09 оголосив `42001f/42e01f/4d001f/f4001f`. Отже, **4:4:4 у звичайному `<video>` через `RTCPeerConnection` можливий без WebCodecs**. Справжнє обмеження інше: Media Foundation MFT, наш енкодер на агенті, 4:4:4 не кодує. High (`64xx`) і справді не гарантовано.
2. **Для нерухомого тексту oo-screen уже має не менше прийомів, ніж будь-хто з лідерів.** Є пропуск незмінених кадрів, дошліфування QP, lossless-тайли, 15 к/с у режимі «Текст», шар курсора, FEC, детектор затримки, pacer. Частина з них вимкнена прапорцями. Відставання в іншому:
   - (а) колір тексту **в русі** (4:2:0);
   - (б) енкодер не отримує жодної інформації про регіони: ні active map, ні ROI, ні dirty rects;
   - (в) дошліфування — 2 фіксовані кадри, а не збіжність, як top-off у CRD;
   - (г) керування бітрейтом налаштоване на якість, а не на затримку: HRD 0,5 с проти 1 кадру в Sunshine;
   - (д) немає intra-refresh, LTR і RFI, а IDR іде періодично раз на 10 с;
   - (е) не використовується RTP-розширення `playout-delay`, яке CRD ставить на кожен кадр.
3. **Топ-10 плану** (розділ 5). Спершу дешеві кроки без ризику: телеметрія можливостей браузера й енкодера, top-off, `playout-delay`. Потім перевірка H.264 4:4:4 (`f4001f`), бо це найбільший виграш у кольорі при найменших змінах у хабі. Після неї — VBV під затримку. AV1 p1 і шлях WebCodecs свідомо відкладено.

---

## 1. Що насправді вміє Chrome (перевірено в сирцях)

### 1.1 Прийом через WebRTC (`RTCPeerConnection` → `<video>`)

Як Chrome складає список форматів: `DecoderAdapter::GetSupportedFormats()` об'єднує програмну `webrtc::InternalDecoderFactory` з апаратною фабрикою GPU ✅ [C1]. Тому все, що вміє програмна фабрика libwebrtc, Chrome оголошує завжди, незалежно від GPU.

| Формат | SDP | Хто декодує | Висновок | Джерела |
|---|---|---|---|---|
| H.264 Constrained Baseline / Baseline / Main | `42e01f`, `42001f`, `4d001f` | Апаратно, якщо є; інакше FFmpeg | ✅ Так. Цим ми користуємося зараз | [L1] |
| H.264 High / Constrained High | `64001f` / `640c1f` | Лише якщо апаратна фабрика GPU оголосить High | На нашому парку **ні**: Chrome 05.09 не оголосив жодного `64xx` (`main.go:56-62`). На інших ПК — UNVERIFIED | [L1], код репо |
| **H.264 High 4:4:4 Predictive** | **`f4001f`** (рівень 3.1, `packetization-mode` 0/1) | Лише FFmpeg, **один потік** (`thread_count = 1`) | ✅ **Так.** `SupportedH264DecoderCodecs()` додає `kProfilePredictiveHigh444` до списку CBP/Main. `H264DecoderImpl` приймає `AV_PIX_FMT_YUV444P` і `YUVJ444P` (а також 4:2:2 і 10-біт) і віддає `I444Buffer`. На нашому парку `f4001f` уже приходить в offer | [L1], [L2], `main.go:56-62` |
| VP9 profile 0 | `profile-id=0` | Апаратно або libvpx | ✅ Так | [L3] |
| **VP9 profile 1** (8-біт 4:4:4) | `profile-id=1` | Лише libvpx | ✅ **Так.** У сирцях: «The WebRTC internal decoder supports VP9 profile 1 and 3». `STAGE3-444.md` вважав, що «profile 1 не оголошується» (UNVERIFIED) — це **спростовано** | [L3] |
| AV1 Main (profile 0) | `profile=0` | dav1d (+ апаратно, якщо є) | ✅ Так | [L5], [L6] |
| **AV1 High (profile 1)** 4:4:4 | `profile=1` | Лише dav1d | ✅ **Так.** `InternalDecoderFactory` оголошує `AV1Profile1()`, `Dav1dDecoder` обробляє `DAV1D_PIXEL_LAYOUT_I444`. CRD через профіль у SDP вмикає «lossless color» | [L4], [L5], [CRD3] |
| HEVC | `H265` | Лише апаратно | (s) У WebRTC за замовчуванням з Chrome 136. 4:4:4 RExt і інструменти SCC — UNVERIFIED. **Не рекомендую:** лише апаратне декодування, а частина парку на Win7 і старих GPU | [H1] |

Застереження.

- **`f4001f` уже прибирали з libwebrtc.** Є коміт «Revert "Added support for H264 YUV444 (I444) decoding."» (s) [L7]. У гілках дзеркала `m104`, `m114`, `m125` і `m137` підтримка присутня ✅. Отже, її повернули, і щонайменше з M104 (2022) вона на місці. Може зникнути знову — це ризик, хоч і малий.
- `rtc_use_h264` у збірці Chromium дорівнює `media_use_openh264` [L6]. У фірмовому Chrome і Edge на десктопі H.264 через WebRTC працює, це видно на проді. У «голому» Chromium без пропрієтарних кодеків H.264 немає зовсім.
- **Ціна на боці глядача — UNVERIFIED.** Програмне декодування 4:4:4 йде в одному потоці FFmpeg і обробляє вдвічі більше вибірок, ніж 4:2:0. Для 1440p30 з прокруткою на слабкому ноутбуці глядача це потрібно заміряти. Firefox і Safari `f4001f`, найімовірніше, не оголошують (UNVERIFIED), тому запасний 4:2:0 потрібен обов'язково.
- **Хаб.** `h264ProfileCompatible` (`main.go:2179`) вимагає однакового `profile_idc`. Агент у `f4` і глядач без `f4001f` означають 415. Отже, потрібен переговорний механізм на рівні ноди: шлемо 4:4:4 лише тоді, коли **всі** глядачі ноди оголосили `f4001f`. Так само, як дозвіл шару курсора в `cursor.go`. Розбір SPS (`internal/h264/annexb.go:203`) уже розуміє `profile_idc = 244` і `chroma_format_idc = 3`, MKV пише `V_MPEG4/ISO/AVC`, FEC від кодека не залежить.

### 1.2 WebCodecs (DataChannel або WebTransport → `VideoDecoder` → canvas)

- ✅ `IsDefaultDecoderSupportedVideoType()` повертає `true` для **будь-якого** профілю H.264 (у фірмових збірках декодує FFmpeg), а також для VP9 profile 0 і 1 та AV1 Main і High [C2].
- Що це дає: жодних обмежень SDP. Можна High, 4:4:4, дві підрамки AVC444, власний формат.
- Ціна: доведеться самим робити jitter-буфер, NACK, FEC, оцінку смуги, синхронізацію звуку з відео і рендер. Усе це зараз дає WebRTC.
- У репо вже є старий шлях WebTransport + WebCodecs: `desktop-oo.js` (кодек `avc1.64002A`), `hub/cmd/hub-wt`, `hub/wt_relay.go`. Його свідомо замінили на WebRTC.
- **Висновок:** після знахідки `f4001f` шлях WebCodecs потрібен лише для того, чого WebRTC не дає взагалі, наприклад AVC444 з двох апаратних 4:2:0-енкодерів. Це L-зусилля з високим ризиком; пріоритет низький.

### 1.3 Помилка в прототипі `codec444`

`desktop-oo-codec444.js` перевіряє можливості через `VideoDecoder.isConfigSupported` (WebCodecs). Медіа ж іде через `RTCPeerConnection`, тож перевіряти треба `RTCRtpReceiver.getCapabilities('video')`: рядки `sdpFmtpLine` з `profile-level-id=f4001f`, `profile-id=1` і `profile=1`. Ці дві відповіді збігаються лише випадково. До того ж у прототипі немає кандидата H.264 4:4:4.

---

## 2. Техніки лідерів: що вони роблять для тексту й затримки

Позначення: P — Parsec, RD — RustDesk, CRD — Chrome Remote Desktop, S/M — Sunshine/Moonlight, AD — AnyDesk.

| # | Техніка | Лідери (джерело) | oo-screen зараз (файл; стан) | У Chrome застосовно? | Ціна / ризик |
|---|---|---|---|---|---|
| T1 | **4:4:4 для тексту** | CRD: VP9 p1 / AV1 p1, коли клієнт просить I444 профілем у SDP ✅ [CRD1][CRD2][CRD3]. S/M: H.264 High 4:4:4, HEVC RExt 4:4:4, AV1 High 4:4:4 ✅ [M1]; у Sunshine через NVENC, QuickSync і програмний libx264, але **не** AMF і не MediaFoundation ✅ [S2]; NVENC перевіряє `NV_ENC_CAPS_SUPPORT_YUV444_ENCODE` ✅ [S1]. RD: прапор `i444` → VP9 p1 / AV1 p1, лише якщо **всі** глядачі хочуть і вміють ✅ [RD1][RD2][RD3]. P: «Prefer 4:4:4 Color» через H.265, хост з NVIDIA або Intel (s) [P1]. AD: DeskRT (s) | Лише 4:2:0 (MFT не вміє 4:4:4). На нерухомому тексті колір виправляють lossless-тайли (`internal/tiles`, `oo-text-tiles.js`; **вимк.**). Переговори `internal/codec444` + `desktop-oo-codec444.js` — прототип, **вимк.**, медіа — H.264 | **Так, трьома шляхами:** `f4001f` (H.264, найменше змін у хабі), VP9 p1, AV1 p1. Див. §1.1 | Енкодер: NVENC напряму (лише NVIDIA), x264 (GPL), libaom AV1 p1 (BSD, повільний). Глядач: програмне декодування. Ризик середній |
| T2 | **Інструменти для екранного вмісту** (palette, IntraBC, tune=screen) | CRD AV1: `AOM_CONTENT_SCREEN`, palette **увімк.**, IntraBC **вимк.** (у реальному часі дорого), CDEF увімк., `aq-mode=3` ✅ [CRD2]. CRD VP9: `VP9E_CONTENT_SCREEN`, cyclic refresh `aq-mode=3` ✅ [CRD1]. RD AV1: screen + palette, IntraBC вимк. ✅ [RD2]; RD VP9 tune-content не ставить ✅ [RD1]. AD: «exploits GUI properties» (маркетинг, s) [AD1] | Немає. MFT має лише `AVEncVideoContentType` (там нема значення «screen»); OpenH264 `SCREEN_CONTENT_REAL_TIME` описано в `bench/SOFTWARE-ENCODER.md`, у коді немає | Лише з AV1 (palette). У H.264 таких інструментів нема, найближче — OpenH264 у режимі екрана | AV1: L. OpenH264: M |
| T3 | **Dirty rects / active map** (кодувати лише змінені блоки) | CRD: active map у VP8/VP9/AV1; під час top-off мапа **не очищається**, тож дошліфовується лише змінене ✅ [CRD1][CRD3]. CRD шле порожні кадри не частіше за keep-alive 2 с ✅ [CRD3]. RD: кодує лише коли є новий кадр ✅ (`rustdesk-anydesk.md`). S: `minimum_fps_target`, дублікати кадрів для статики ✅ [S3] | DXGI dirty/move rects читаються (`agent/capture/dxgi.c:790-830`), але йдуть лише в пропуск кадру, детектор режиму «Текст» і `contentmode`. **В енкодер не передаються** | Так, це суто агентна сторона. MFT має GUID `CODECAPI_AVEncVideoDirtyRectEnabled` (є в mingw `codecapi.h:703`), але семантика й підтримка конкретними MFT — UNVERIFIED. D3D12 Video Encode має dirty regions і QP map з Agility SDK 1.716 (s) [D1]. NVENC — QP delta map (UNVERIFIED) | M, ризик середній: підтримку в драйверах треба виміряти |
| T4 | **Дошліфування статики / top-off / lossless** | CRD: після руху кодує ще кадри, **доки QP не дійде до `kMinQuantizer` = 10** (діапазон 10–50). Таймер — інтервал кадру × 1,1. Для «великих» кадрів (> 300 000 змінених пікселів і прогноз за `kEstimatedBytesPerMegapixel`) min QP піднімається до max, щоб кадр прийшов швидше ✅ [CRD3]. CRD: стратегія «lossless encode» в VP9 (швидкість 5) ✅ [CRD1]. RDP: progressive codec (s, `crd-rdp-teamviewer.md`) | `internal/refine`: через 200 мс тиші **2 кадри, QP 22 → 18** (MaxQP + `MFSampleExtension_VideoEncodeQP`, `mft.c:886-892`, `:1508-1520`); **увімк.** лише для апаратного енкодера. Тайли без втрат (PNG) — **вимк.** Чи слухає реальний MFT ці QP — UNVERIFIED (TZ 0.3 №3) | Так, повністю на боці агента | S–M, ризик низький |
| T5 | **Адаптивний QP для тексту** (AQ, ROI, QP map) | S: NVENC spatial AQ (опційно, «higher QP to flat regions»), `minQP` ✅ [S1][S3]. CRD і RD: `aq-mode=3` (cyclic refresh) ✅. x264 вміє QP-зсув на кожен макроблок (`quant_offsets`) ✅ [X2]. P: `encoder_min_qp=5` (`parsec-moonlight.md`) | Немає. QP — один на кадр (лише в refine) | Так, на агенті. MFT: `CODECAPI_AVEncVideoROIEnabled` (GUID є), підтримка — UNVERIFIED. Маска тексту вже рахується в `internal/tiles/select.go` | M, ризик середній |
| T6 | **Курсор окремим шаром** | CRD: `CursorShapeInfo` ✅. RD: `CursorData`/`CursorPosition` ✅. AD: режими курсора ✅ (`COMPARISON.md`) | **Є**, `internal/cursorproto`, `hub/cmd/hub-webrtc/cursor.go`, `desktop-oo-cursor.js`, ~120 Гц; **вимк.** (`-cursor-layer`) до перевірки на ПК | — | Потрібна лише перевірка на ПК |
| T7 | **Керування бітрейтом під затримку** | S: CBR, **VBV = 1 кадр** (`bitrate / fps`), опційно до +400 %; `NV_ENC_TUNING_INFO_ULTRA_LOW_LATENCY`; без lookahead ✅ [S1][S3]. CRD: CBR, undershoot 100 %, overshoot 15 %, правило «великого кадру», `EncoderBitrateFilter` ✅ [CRD1][CRD3]. RD: керування за затримкою, поріг 150 мс, раз на 3 с ✅ [RD4] | `mft.c:484-495`: PeakConstrainedVBR, Max = 1,5 × mean, **HRD = 0,5 с** (≈ 15 кадрів при 30 к/с). Хаб: втрати + RTT + REMB, детектор затримки TWCC (`internal/bwe`, `delaybwe.go`; **вимк.**), проба (**вимк.**), pacer (**вимк.**) | Так, на агенті й у хабі | S, ризик середній: на прокрутці при малому бітрейті якість може впасти |
| T8 | **GOP і відновлення після втрат** | S: **нескінченний GOP**, IDR лише на запит, **reference frame invalidation** (`nvEncInvalidateRefFrames`), опційний intra-refresh (період 300) ✅ [S1]. CRD: `kf_max_dist = 10000` ✅ [CRD1]. RD: keyframe вимкнено, якщо інтервал не задано ✅ [RD1] | IDR **раз на 10 с** (`-gop-seconds`), на запит і для нового глядача (GOP-кеш хаба `gop.go`). Немає intra-refresh, LTR і RFI | Так. У MFT є GUID-и `AVEncVideoGradualIntraRefresh`, `AVEncVideoLTRBufferControl`, `MarkLTRFrame`, `UseLTRFrame`; підтримка — UNVERIFIED | M, ризик середній: GOP-кеш розрахований на періодичний IDR |
| T9 | **FEC** | S/M: Reed-Solomon, 20 % за замовч. ✅ (`parsec-moonlight.md`) | **Є**: ULPFEC + RED, 2D (`internal/ulpfec`, `fec.go`); **вимк.** На стенді RTT 200 мс: 24 → 0,8 с/хв завмирань | — | Перевірити в Chrome; врахувати накладні FEC у бітрейті (відома межа, `fec.go:19`) |
| T10 | **Рендер із мінімальною затримкою** | CRD: на **кожному** кадрі `SetPlayoutDelay(VideoPlayoutDelay::Minimal())` (RTP-розширення playout-delay 0/0) і `VideoContentType::SCREENSHARE` ✅ [CRD3] | Плеєр: `jitterBufferTarget = 0` / `playoutDelayHint` + адаптивна ціль (`desktop-oo-webrtc.js:151-167`, `:1617`). Хаб розширень playout-delay і video-content-type **не ставить** | Так, на хабі (пакети ретранслюються, розширення додати можна). Чи оголошує Chrome на нашому парку розширення playout-delay — UNVERIFIED | S, ризик низький |
| T11 | **FPS під вміст** | RD: FPS 1–120, старт 15, керування за затримкою ✅ [RD4]. CRD: 15–120 Гц ✅ [CRD3]. S: мін. FPS для статики ✅ | **Є**: режим «Текст» → 15 к/с (**увімк.**), режим «Відео» (`videoboost.go`, **вимк.**), `-lowmotion-cap` (**вимк.**), `internal/swlimit` | — | — |
| T12 | **Енкодери** | S: NVENC/QSV/AMF/VA-API/VT/Vulkan/libx264 ✅. CRD: H.264 **лише апаратно** (`H264PROFILE_MAIN`, мін. 1800 кбіт/с на Мп), VP8/VP9/AV1 програмно ✅ [CRD4]. RD: апаратні + libvpx як запасний ✅ | MFT (апаратний, інакше MS SW MFT), прапор `-force-software`. NvEncodeAPI напряму і OpenH264 — лише в планах (`PLAN-waves.md`, `bench/SOFTWARE-ENCODER.md`) | — | — |
| T13 | **Lossless окремим каналом чи в потоці** | RDP: progressive/ClearCodec у потоці (s). У H.264 lossless — лише High 4:4:4 + `qpprime_y_zero_transform_bypass`; x264 вмикає це **тільки** в CQP 0 ✅ [X1]. Отже, «lossless лише на текстових МБ» в одному потоці x264 не дає; є майже lossless через `quant_offsets` [X2] | PNG-тайли окремим каналом поверх `<video>` (**вимк.**) | Тайли працюють у будь-якому браузері. Майже lossless у потоці можливий лише з 4:4:4-шляхом | — |

### 2.1 Факти за продуктами (лише нове відносно попередніх таблиць)

**Chrome Remote Desktop** (сирці Chromium `remoting/`):
- **4:4:4 вмикається через SDP.** `WebrtcVideoEncoderWrapper` бере `profile-id` / `profile` із погодженого формату. Profile 1 означає `SetLosslessColor(true)` → I444 [CRD3]. Отже, і браузер-клієнт CRD отримує VP9/AV1 4:4:4 стандартним `RTCPeerConnection`. Це незалежне підтвердження §1.1.
- **Top-off.**
  - `kMinQuantizer = 10`, `kMaxQuantizer = 50`.
  - `top_off_active_ = (frame->quantizer > kMinQuantizer)`.
  - Додаткові кадри йдуть з інтервалом `max(1,1 × інтервал, інтервал + 2 мс)`, тож їх витісняє справжній кадр із захоплення.
  - Поки top-off активний, active map не очищається (`clear_active_map = !top_off_active_`).
  - Великий кадр (> 300 000 пікселів, оцінка 100 000 байт/Мп) кодується з min QP = max QP (50), щоб дійти швидко. Якість потім підтягує той самий top-off [CRD3].
- **VP9:** CBR, `kf_max_dist = 10000`, `rc_dropframe_thresh = 0`, undershoot 100 %, overshoot 15 %, `VP9E_CONTENT_SCREEN`, `AQ_MODE = 3` (cyclic refresh), швидкість 6, для lossless 5 [CRD1].
- **AV1:** `AOM_CONTENT_SCREEN`, palette = 1, IntraBC = 0, TPL/OBMC/warped/global motion/CFL/smooth intra вимкнено, CDEF = 1, `AQ_MODE = 3`, row-mt і тайли за кількістю потоків [CRD2].
- **H.264** лише через апаратний `VideoEncodeAccelerator`, профіль Main [CRD4]. Тобто CRD теж не має 4:4:4 у H.264.
- **Порожні кадри** — не частіше за keep-alive 2 с. Кожен кадр позначено playout delay `Minimal()` і `SCREENSHARE` [CRD3].

**Sunshine / Moonlight**:
- **NVENC** [S1]:
  - нескінченний GOP, `frameIntervalP = 1`, CBR, `zeroReorderDelay`, без lookahead;
  - **VBV = bitrate / framerate**, тобто один кадр;
  - `ULTRA_LOW_LATENCY`;
  - RFI, якщо є `NV_ENC_CAPS_SUPPORT_REF_PIC_INVALIDATION`;
  - intra-refresh за запитом клієнта (період 300, `singleSliceIntraRefresh`);
  - 4:4:4: `NV_ENC_H264_PROFILE_HIGH_444_GUID` з перевіркою можливостей.
- **Кодеки з 4:4:4.** Прапор `YUV444_SUPPORT` мають `nvenc`, `quicksync` і `software` (libx264); `amdvce` і `mediafoundation` його не мають [S2].
- **Moonlight** оголошує `VIDEO_FORMAT_H264_HIGH8_444`, `H265_REXT8_444` і `AV1_HIGH8_444`, а клієнт moonlight-qt має перемикач `enableYUV444` [M1].
- **Налаштування** [S3]: `minimum_fps_target` (дублікати кадрів для статики), `nvenc_spatial_aq` (типово вимк.), `nvenc_vbv_increase` (0–400 %, типово 0), `fec_percentage`.

**RustDesk**:
- **4:4:4.** `use_i444()` вмикає I444, лише коли **всі** декодери глядачів мають `prefer_chroma = I444` і вміють 4:4:4 для цього кодека [RD3]. Це той самий принцип «найменшого спільного знаменника», що потрібен нам для `f4001f`.
- **VP9:** profile 1 при i444, CBR, undershoot 95, `kf_mode = DISABLED`, якщо інтервал не задано, cpu-used 7, row-mt. **Без** tune-content screen [RD1].
- **AV1:** профіль 1 при i444, CBR, screen + palette, IntraBC вимк., `aq-mode = 3`, deltaq 0 [RD2].
- **Якість:** коефіцієнти бітрейту Best 1,5 / Balanced 0,67 / Speed 0,5 [RD3]. QoS за затримкою з порогом 150 мс, FPS 1–120 [RD4].

**Parsec** — сторінки підтримки заблоковано, тому лише (s): 4:4:4 вмикається через H.265 («Prefer 4:4:4 Color»), на хості NVIDIA або Intel; AMD лише 4:2:0 [P1]. Решта — у `parsec-moonlight.md`: BUD/UDP, «+7 мс», `encoder_min_qp`.

**AnyDesk** — лише маркетинг (s): DeskRT «exploits the special properties of GUI image data (large areas of the same colour, high contrasts, sharp edges, repeating patterns…, linear translation)», 60 к/с [AD1]. Технічних деталей немає. Усе про AnyDesk — **UNVERIFIED**.

---

## 3. Що з цього вже є в oo-screen

| Техніка | Де в коді | Типово | Чого бракує до рівня лідерів |
|---|---|---|---|
| Пропуск незмінених кадрів | `agent/capture/dxgi.c:879-929`, `agent/cmd/oo-agent/main.go:1907` | увімк. | — (на рівні CRD і RD) |
| Дошліфування статики | `internal/refine/refine.go` (`DefaultQPs = {22, 18}`, `Idle` 200 мс), `agent/encode/mft.c:886`, `:1508` | увімк. (лише апаратний) | Збіжність за фактичним QP (top-off), обмеження регіоном, захист від великих кадрів |
| Lossless-тайли тексту | `internal/tiles/*`, `total-erp-app/resources/js/remote/oo-text-tiles.js` | вимк. | Перевірка на ПК (TZ F10) |
| Режим «Текст», 15 к/с | `internal/textmode`, `agent/cmd/oo-agent/textfps.go` | увімк. | — |
| Режими «Відео» і «малорухомий» | `internal/contentmode`, `hub/cmd/hub-webrtc/videoboost.go` | вимк. | — |
| Шар курсора | `internal/cursorproto`, `hub/.../cursor.go`, `desktop-oo-cursor.js` | вимк. | Перевірка на ПК |
| Переговори 4:4:4 | `internal/codec444/codec444.go`, `desktop-oo-codec444.js` | вимк. (прототип) | Правильна перевірка можливостей (§1.3), кандидат `f4001f`, медіашлях |
| FEC (ULPFEC/RED, 2D) | `internal/ulpfec`, `hub/.../fec.go` | вимк. | Chrome, FEC у бюджеті бітрейту |
| Детектор затримки (TWCC) і проба | `internal/bwe`, `hub/.../delaybwe.go`, `probe.go` | вимк. | Chrome (TWCC), поле |
| Pacer | `internal/pacer` | вимк. | — |
| BT.709 у VUI | `internal/h264/sps_vui.go` | увімк. | — |
| Мінімальний jitter-буфер, HiDPI 1:1 | `desktop-oo-webrtc.js:151-167`, `:836`, `:1345` | увімк. | RTP-розширення playout-delay з боку хаба |
| Налаштування енкодера | `agent/encode/mft.c:484-531`: PeakConstrainedVBR, 1,5×, HRD 0,5 с, B = 0, 2 опорні кадри, LowLatency, QualityVsSpeed 80, CABAC | — | VBV під затримку; AQ/ROI; intra-refresh/LTR; телеметрія `IsSupported` |
| Обмеження енкодера за CPU | `internal/swlimit` | увімк. (програмний) | OpenH264 у режимі екрана |

**Чого немає зовсім:**
- 4:4:4 у медіашляху;
- AQ, ROI і QP map;
- dirty rects в енкодер;
- intra-refresh, LTR і RFI;
- нескінченний GOP;
- RTP-розширення playout-delay і video-content-type;
- інструменти для екранного вмісту.

---

## 4. Застосовність у Chrome: ціна й ризик кожного варіанта

### 4.1 Колір тексту (4:4:4)

| Варіант | Як | Chrome | Енкодер на агенті | Зміни в хабі й плеєрі | Очікуваний виграш | Ризик |
|---|---|---|---|---|---|---|
| **A. H.264 High 4:4:4 (`f4001f`) у `<video>`** | Агент кодує `profile_idc = 244`, `chroma_format_idc = 3`; хаб пропускає, лише якщо всі глядачі ноди оголосили `f4001f`, інакше Main | ✅ FFmpeg, програмно (§1.1) | NVENC напряму (лише NVIDIA, з перевіркою caps, як у Sunshine) **або** x264 4:4:4 (GPL v2 або комерційна ліцензія; CPU). MS MFT 4:4:4 **не вміє**. OpenH264 кодує лише CB | Хаб: переговори на рівні ноди (за зразком `cursor.go`), MediaEngine з `4d001f` і `f4001f`, скидання GOP-кешу при перемиканні. Плеєр: `setCodecPreferences` з `f4001f`. SPS, MKV і FEC уже сумісні | Статика, симуляція x264 high444 @8M: chroma PSNR **53,5 проти 40,0 дБ**, RGB PSNR 46,8 проти 34,2 (`bench/quality/RESULTS.md`). Русі x264 high444 не міряли | Середній: CPU агента без NVIDIA; програмне декодування в одному потоці у глядача (UNVERIFIED); ліцензія x264 |
| A′. Гібрид «4:4:4 лише в режимі Текст» | Рух — апаратний Main 4:2:0, набір і читання — x264 4:4:4 (CPU низький, бо змін мало). Перемикання через IDR | Той самий ✅; на одному PT `f4001f` декодер FFmpeg, імовірно, прийме і SPS Main після IDR — **UNVERIFIED** | Апаратний MFT + x264 | Як у A + IDR на кожному перемиканні | Колір під час набору як в оригіналі; тайли не потрібні | Середній: частота IDR, межа між режимами |
| B. AV1 profile 1 (libaom, screen, palette) | Як у CRD [CRD2] | ✅ dav1d | Лише програмно: апаратні AV1 у Windows — 4:2:0, SVT-AV1 4:4:4 не вміє (UNVERIFIED). Симуляція libaom: ~20 мс на кадр 1080p на Xeon | Хаб має H.264-специфічні GOP-кеш, перезапис SPS і MKV — потрібен шар, незалежний від кодека. Pion уміє AV1 (`rtp@v1.10.5/codecs/av1_packet.go`) | Рух, симуляція: chroma **+12,7…+14,0 дБ** при рівному бітрейті, але luma нижчий за H.264 (`RESULTS-codec444-motion.md`) | Високий: CPU слабких ПК, L-обсяг |
| C. VP9 profile 1 | libvpx, як у CRD і RD | ✅ libvpx | Програмно | Як у B | Симуляція: chroma +4…+6 дБ, але бітрейт перевищує ціль у 1,8–2,5 раза | Не рекомендовано (так і в `RESULTS-codec444-motion.md`) |
| D. AVC444 (дві підрамки 4:2:0) через WebCodecs | MS-RDPEGFX | ✅ WebCodecs | Два апаратні MFT 4:2:0 | Власний транспорт, WebGL-збирання (`STAGE3-444.md`) | Симуляція: chroma +6…+12 дБ, luma −3,4 дБ @4M | Високий, L. **Після A не має сенсу**, крім ПК, де є лише апаратний 4:2:0 і зовсім немає CPU |
| E. Lossless-тайли (є) | PNG окремим каналом | ✅ | — | Є | Chroma 68–72 дБ на статиці (`STAGE3-444.md`) | Низький; лише статика |

### 4.2 Різкість статики й адаптивний QP

- **Top-off до збіжності.** Повністю агентна зміна. QP кадру можна читати з бітстріму: `pic_init_qp` + `slice_qp_delta` розбираються в `internal/h264`. Тоді не треба вірити, що MFT послухав `MFSampleExtension_VideoEncodeQP`. Ризик низький.
- **ROI / QP map для текстових МБ** — лише там, де їх підтримує енкодер:
  - MFT `AVEncVideoROIEnabled` — UNVERIFIED;
  - NVENC QP delta map — UNVERIFIED;
  - x264 `quant_offsets` ✅ [X2].

  Маска тексту вже є (`internal/tiles/select.go`). Без підтримки енкодера лишається покадровий MaxQP.
- **Dirty rects в енкодер.** У MFT — UNVERIFIED. У D3D12 Video Encode — лише Windows 11 + свіжі драйвери (s) [D1]. Для офісного парку, де є Win7 і слабкі ПК, це **поки не варте зусиль**. Досить використати dirty rects для **рішень** (обсяг top-off, правило великого кадру), а не для самого кодування.

### 4.3 Затримка

- **RTP-розширення playout-delay 0/0 + video-content-type=screenshare на виході хаба.** Так робить CRD. Зусилля S, без змін в агенті. Ефект на нашому парку — UNVERIFIED. `jitterBufferTarget = 0` у плеєрі вже частково дає те саме.
- **VBV / HRD.** Зараз 0,5 с означає, що один IDR або кадр прокрутки може зайняти до 0,5 с каналу. Sunshine тримає 1 кадр, CRD скидає якість великого кадру. Пропоную HRD залежно від режиму: «Відео» і рух — 2–3 кадри, «Текст» і refine — як зараз. Перевірка — `bench/impair/cmd/netbench` (з pacer і без).
- **Нескінченний GOP + intra-refresh або LTR.** Це прибере періодичні піки IDR. Але GOP-кеш хаба (B1, перший кадр 0,21 с) тримається на періодичному IDR, тож новий глядач тоді чекатиме IDR на запит. Обидва варіанти треба заміряти за метрикою «час до першого кадру».

---

## 5. Пріоритетний план (крок 3)

Шкала:
- **Виграш:** 1 — малий … 5 — найбільший, для тексту або затримки офісного користувача.
- **Ризик:** Н / С / В.
- **Обсяг:** S (дні), M (1–3 тижні), L (місяць+).
- **Пріоритет** = виграш з поправкою на ризик і обсяг.

Усе, що змінює поведінку, вмикається прапорцем і за замовчуванням вимкнене (TZ §6).

**Передумова (P0, не з цього дослідження).** Програма замірів на реальних ПК з TZ §7 (M1–M5) і рішення про вмикання вже зробленого: шар курсора, тайли, FEC, детектор затримки, pacer. Найдешевший виграш — це увімкнути те, що вже написано. Пункти нижче не повторюють цей список.

| # | Що | Виграш | Ризик | Обсяг | Файли | Як перевірити |
|---|---|---|---|---|---|---|
| **1** | **Телеметрія можливостей браузера.** Плеєр бере `RTCRtpReceiver.getCapabilities('video')` і повідомляє хабу: `f4001f`, VP9 `profile-id=1`, AV1 `profile=1`, `64xx`, розширення playout-delay. Хаб пише частки в `/metrics`. Заодно виправити перевірку в `codec444` (§1.3) і додати кандидата `h264-444` | 2 (сам по собі), але без нього не можна вирішити п. 4 і 7 | Н | S | `total-erp-app/resources/js/remote/desktop-oo-codec444.js`, `desktop-oo-webrtc.js`; `internal/codec444/codec444.go`; `hub/cmd/hub-webrtc/main.go` (розбір offer), `metrics.go` | JS-тест на фейкових `getCapabilities`; Go-тест `Negotiate`; на проді — частка `f4001f` по глядачах |
| **2** | **Телеметрія можливостей MFT.** На старті `ICodecAPI_IsSupported` / `IsModifiable` для `ROIEnabled`, `DirtyRectEnabled`, `GradualIntraRefresh`, `LTRBufferControl`, `MinQP`, `MaxQP`, `EncodeQP`, `EncodeFrameTypeQP`, `ContentType` → `cfg_report` → лог і `/metrics` хаба. Плюс фактичний QP кадру з бітстріму | 2 (основа для п. 3, 8, 9) | Н | S | `agent/encode/mft.c` (`configure_codecapi`, `cfg_report`), `encode_windows.go`, `agent/cmd/oo-agent/main.go`; `internal/h264` (новий розбір `slice_qp_delta`) | Юніт-тест розбору QP на кадрах корпусу; на ПК — рядок логу (M2) |
| **3** | **Top-off до збіжності** (як у CRD) замість «2 кадри, QP 22 → 18». Повторювати refine-кадр, доки фактичний QP (п. 2) > цілі (≈ 12–16), з бюджетом байтів на епізод і переривати рухом. **Правило великого кадру:** коли змінено > N пікселів і бюджет HRD малий, кадр кодується з високим min QP, а якість потім добирає top-off | 4 (нерухомий текст на БУДЬ-ЯКОМУ енкодері, зокрема там, де MFT ігнорує QP семпла) | Н | S–M | `internal/refine/refine.go` (новий режим `Converge`), `agent/cmd/oo-agent/main.go`, `agent/encode/mft.c` (`MinQP`) | Go-тести автомата (фейковий годинник, фейковий QP); `bench/quality` — edge-SSIM до і після; на ПК — M3 |
| **4** | **Перевірка H.264 4:4:4 (`f4001f`), фаза 1.** (а) Додати x264 `high444` у `bench/quality/codec444_motion.py` і корпус тексту при рівному бітрейті. (б) Через `agent/cmd/corpus-player-webrtc` віддати в живий Chrome файл High 4:4:4 і заміряти декодування, CPU глядача і правильність кольору (`bench/quality/compare_image.py`). (в) Перевірити, що на одному PT `f4001f` декодер приймає і SPS Main після IDR (варіант A′) | 3 (знімає головний UNVERIFIED) | Н (лише стенд) | S–M | `bench/quality/codec444_motion.py`, `bench/quality/RESULTS-codec444-motion.md`, `agent/cmd/corpus-player-webrtc/` | Цифри chroma PSNR і edge-SSIM у русі; `getStats` `framesDecoded`, `totalDecodeTime` у Chrome |
| **5** | **RTP-розширення playout-delay (min = max = 0) і video-content-type = screenshare** на виході хаба до глядача, якщо браузер їх оголосив (як CRD для кожного кадру) | 2 (затримка відмальовки; ефект UNVERIFIED) | Н | S | `hub/cmd/hub-webrtc/main.go` (MediaEngine `RegisterHeaderExtension`), `fanout.go` / `egress.go` (запис розширення), `interceptors.go` | Go-тест: розширення на дроті, і лише коли погоджене; у Chrome — `getStats` `jitterBufferDelay` і оверлей «i» |
| **6** | **VBV під затримку за режимом вмісту.** Рух і «Відео» — HRD ≈ 2–3 кадри і `MaxBitRate` ≈ 1,2× (Sunshine — 1 кадр); «Текст» і refine — як зараз (0,5 с, 1,5×). Перемикання без IDR через `AVEncCommonBufferSize` / `MeanBitRate` (уже змінюються наживо в `mft.c:1495-1500`) | 3 (менше хвостів затримки й завмирань на вузькому каналі; може частково замінити pacer) | С (якість прокрутки при малому бітрейті) | S | `agent/encode/mft.c` (`hrd_bits`, `peak_bps`, `oos_enc_set_bitrate`), `internal/contentmode`, `agent/cmd/oo-agent/main.go` | `bench/impair/cmd/netbench` при обмеженні 2/4/8M: p95 доставки кадру і с/хв завмирань до/після; `bench/quality/workloads.py` — edge-SSIM прокрутки |
| **7** | **H.264 4:4:4 у прод-шляху, фаза 2–3** (якщо п. 4 позитивний). Хаб: переговори на рівні ноди «4:4:4 дозволено, лише коли ВСІ глядачі мають `f4001f`», інакше Main; новий глядач без `f4001f` → IDR Main (за зразком переговорів курсора, RD `use_i444`). Агент: NVENC 4:4:4 напряму, де є NVIDIA (`NV_ENC_CAPS_SUPPORT_YUV444_ENCODE`), і/або x264 4:4:4 у режимі «Текст» (A′), коли `internal/swlimit` каже, що CPU вистачає. Плеєр: `setCodecPreferences` з `f4001f` | **5** (колір тексту в русі й під час набору; закриває Q2 і частково Q3 у TZ) | С | M–L | `hub/cmd/hub-webrtc/main.go` (`h264ProfileCompatible`, MediaEngine, вибір профілю ноди), `gop.go` (скидання при зміні профілю), `cursor.go` як зразок; `agent/encode/` (новий `nvenc_windows.go` / `x264_windows.go` через `LoadLibrary`), `agent/cmd/oo-agent/main.go`; `desktop-oo-webrtc.js` | Go-тести переговорів (2 глядачі: `f4` + `4d` → Main; обидва `f4` → 4:4:4; вихід «слабкого» → перемикання з IDR); M3/M5 на ПК; рішення щодо ліцензії x264 — **за власником** |
| **8** | **ROI / QP map для текстових МБ** під час refine (і в 4:4:4-шляху). MFT `ROIEnabled`, якщо п. 2 покаже підтримку; NVENC QP delta map; x264 `quant_offsets`. Маска — з `internal/tiles/select.go`. Top-off обмежити регіоном, накопиченим з DXGI dirty rects після останньої збіжності (аналог active map у CRD) | 3 (текст гостріший при тому ж бюджеті; менше байтів на фото й градієнти) | С (підтримка в драйверах) | M | `agent/encode/mft.c`, `internal/tiles/select.go`, `internal/refine`, `agent/capture/meta.go` | `bench/quality`: edge-SSIM і байти на епізод при ROI і без; на ПК — лише там, де п. 2 показав `ROIEnabled` |
| **9** | **Відновлення без повних IDR.** Gradual intra-refresh або LTR (`MarkLTRFrame` / `UseLTRFrame`) на PLI замість IDR; періодичний IDR рідше (30–60 с) або вимкнений, якщо GOP-кеш хаба віддає IDR на запит без втрати L3 | 3 (без піків IDR: менше завмирань і різкості, що «пливе» після втрат) | С (підтримка в MFT; GOP-кеш) | M | `agent/encode/mft.c`, `agent/cmd/oo-agent/main.go` (`-gop-seconds`), `hub/cmd/hub-webrtc/gop.go`, `nack.go` (PLI) | netbench 1–5 % втрат: с/хв завмирань, байти після PLI; `hubbench` — час до першого кадру ≤ 500 мс (L3) |
| **10** | **OpenH264 у режимі екрана для ПК без GPU** (`SCREEN_CONTENT_REAL_TIME`, LTR, frame skip; дизайн уже є в `bench/SOFTWARE-ENCODER.md`): завантаження DLL Cisco з перевіркою підпису, ланцюг «апаратний MFT → OpenH264 → MS SW MFT» | 3 (слабкі ПК без GPU: CPU і текст; профіль CB, Chrome приймає) | С (ліцензія бінарника Cisco, ABI між версіями) | M | `agent/encode/` (новий `openh264_windows.go`), `internal/swlimit`, `agent/cmd/oo-agent/main.go` | CPU на слабкому ПК (M2), `bench/quality` |

**Свідомо відкладено:**
- **AV1 profile 1** (§4.1-B): L, CPU слабких ПК, і хаб довелося б робити незалежним від кодека. Повернутись, лише якщо п. 4 покаже, що H.264 4:4:4 у Chrome непридатний.
- **AVC444 / WebCodecs** (§4.1-D, §1.2): L і високий ризик; після `f4001f` має сенс лише для ПК без CPU, де є тільки апаратний 4:2:0.
- **HEVC:** у Chrome лише апаратне декодування; Win7 у парку.
- **Dirty rects у D3D12 Video Encode:** лише Windows 11 і нові драйвери.
- **IntraBC:** CRD і RustDesk самі вимикають його в реальному часі.
- **FEC у бюджеті бітрейту** (`fec.go:19`): S–M; йде разом із N2/N3 у TZ, а не з якістю тексту.

**Залежності:**
- п. 1 → п. 4 → п. 7;
- п. 2 → п. 3, п. 8, п. 9;
- п. 3 корисний і без решти;
- п. 5 і п. 6 незалежні.

---

## 6. Що лишилось UNVERIFIED (і як це закрити)

| Що | Як закрити |
|---|---|
| Chrome і Edge на всіх ПК парку оголошують `f4001f`; Firefox і Safari — ні | п. 1 (`/metrics`), M5 |
| CPU глядача на програмному декодуванні H.264 4:4:4 у WebRTC (FFmpeg в одному потоці) | п. 4(б) |
| Декодер на PT `f4001f` приймає й SPS Main після IDR (варіант A′) | п. 4(в) |
| Що з `ROIEnabled`, `DirtyRectEnabled`, `GradualIntraRefresh`, LTR підтримують MFT NVIDIA, Intel, AMD і MS SW | п. 2 на парку (M2) |
| Чи слухає MFT `MFSampleExtension_VideoEncodeQP` у PeakConstrainedVBR | п. 2 (фактичний QP з бітстріму) |
| Підтримка NVENC 4:4:4 і QP map на GPU парку | п. 7: перевірка caps на старті, як у Sunshine |
| Ефект playout-delay на затримку відмальовки в Chrome | п. 5 + M1 |
| Parsec і AnyDesk: деталі їхніх прийомів | Первинні джерела заблоковані; лишається (s) |

---

## Джерела

Усі ✅ відкрито в цій сесії через `raw.githubusercontent.com`. Канонічні адреси `*.googlesource.com` з цієї сесії недоступні, тому дано дзеркала.

**libwebrtc** (дзеркало `webrtc-sdk/webrtc`, гілка `m137_release`):
- [L1] https://github.com/webrtc-sdk/webrtc/blob/m137_release/modules/video_coding/codecs/h264/h264.cc — `SupportedH264Codecs()` (CB/Baseline/Main) і `SupportedH264DecoderCodecs()` (+ `kProfilePredictiveHigh444`). Також перевірено у гілках `m104_release`, `m114_release`, `m125_release`.
- [L2] https://github.com/webrtc-sdk/webrtc/blob/m137_release/modules/video_coding/codecs/h264/h264_decoder_impl.cc — `kPixelFormatsSupported` (`YUV444P`…), `CreateI444Buffer`, `thread_count = 1`.
- [L3] https://github.com/webrtc-sdk/webrtc/blob/m137_release/modules/video_coding/codecs/vp9/vp9.cc — `SupportedVP9DecoderCodecs()`: profile 1 і 3.
- [L4] https://github.com/webrtc-sdk/webrtc/blob/m137_release/modules/video_coding/codecs/av1/dav1d_decoder.cc — I420 / I444.
- [L5] https://github.com/webrtc-sdk/webrtc/blob/m137_release/media/engine/internal_decoder_factory.cc — `AV1Profile0()`, `AV1Profile1()`, H.264 і VP9.
- [L6] https://github.com/webrtc-sdk/webrtc/blob/m137_release/webrtc.gni — `rtc_use_h264 = media_use_openh264` у Chromium; `rtc_include_dav1d_in_internal_decoder_factory = true`.
- [L7] (s) https://webrtc.googlesource.com/src/+/3f42fdf19ff691f3eae23cab33e33a39f809b835 — Revert «Added support for H264 YUV444 (I444) decoding».

**Chromium** (дзеркало `chromium/chromium`, гілка `main`):
- [C1] https://github.com/chromium/chromium/blob/main/third_party/blink/renderer/platform/peerconnection/video_codec_factory.cc — `DecoderAdapter`: програмні формати `InternalDecoderFactory` ∪ формати GPU.
- [C2] https://github.com/chromium/chromium/blob/main/media/base/supported_types.cc — `IsDefaultDecoderSupportedVideoType` (H.264 → `true`), VP9 profile 0/1, AV1.

**Chrome Remote Desktop:**
- [CRD1] https://github.com/chromium/chromium/blob/main/remoting/codec/webrtc_video_encoder_vpx.cc
- [CRD2] https://github.com/chromium/chromium/blob/main/remoting/codec/webrtc_video_encoder_av1.cc
- [CRD3] https://github.com/chromium/chromium/blob/main/remoting/protocol/webrtc_video_encoder_wrapper.cc — top-off, `kMinQuantizer` / `kMaxQuantizer`, правило великого кадру, keep-alive 2 с, профіль із SDP → I444, `SetPlayoutDelay(Minimal())`, `SCREENSHARE`.
- [CRD4] https://github.com/chromium/chromium/blob/main/remoting/codec/webrtc_video_encoder_gpu.cc — H.264 Main, мінімум 1800 кбіт/с на Мп.
- Додатково: `remoting/protocol/webrtc_video_encoder_factory.cc` і `remoting/codec/video_encoder_active_map.cc` (там само).

**Sunshine / Moonlight:**
- [S1] https://github.com/LizardByte/Sunshine/blob/master/src/nvenc/nvenc_base.cpp
- [S2] https://github.com/LizardByte/Sunshine/blob/master/src/video.cpp — `YUV444_SUPPORT` у nvenc, quicksync, software; немає в amdvce і mediafoundation.
- [S3] https://github.com/LizardByte/Sunshine/blob/master/docs/configuration.md — `minimum_fps_target`, `nvenc_spatial_aq`, `nvenc_vbv_increase`, `fec_percentage`.
- [M1] https://github.com/moonlight-stream/moonlight-common-c/blob/master/src/Limelight.h — `VIDEO_FORMAT_*_444`; https://github.com/moonlight-stream/moonlight-qt/blob/master/app/gui/SettingsView.qml — `enableYUV444`.

**RustDesk** (гілка `master`):
- [RD1] https://github.com/rustdesk/rustdesk/blob/master/libs/scrap/src/common/vpxcodec.rs
- [RD2] https://github.com/rustdesk/rustdesk/blob/master/libs/scrap/src/common/aom.rs
- [RD3] https://github.com/rustdesk/rustdesk/blob/master/libs/scrap/src/common/codec.rs — `use_i444`, коефіцієнти Best / Balanced / Speed.
- [RD4] https://github.com/rustdesk/rustdesk/blob/master/src/server/video_qos.rs

**x264:**
- [X1] https://github.com/mirror/x264/blob/master/encoder/set.c — `b_qpprime_y_zero_transform_bypass = CQP && qp == 0`.
- [X2] https://github.com/mirror/x264/blob/master/x264.h — `X264_CSP_I444`, `quant_offsets`.

**Лише уривки з пошуку:**
- [P1] (s) https://support.parsec.app/hc/en-us/articles/32381785123860-Improve-Stream-Quality-and-Color-Accuracy
- [AD1] (s) https://www.neowin.net/software/anydesk-363/ — опис DeskRT.
- [H1] (s) https://learn.microsoft.com/en-ca/answers/questions/5880331/h-265-(hevc)-not-published-sent-via-webrtc-chrome ; https://vdo.ninja/h265 — HEVC у WebRTC Chrome 136+, лише з апаратним декодуванням.
- [D1] (s) https://devblogs.microsoft.com/directx/agility-sdk-1-716-0-new-d3d12-video-encode-features/ — dirty regions і QP map у D3D12 Video Encode.

**MFT CodecAPI:** набір GUID-ів (`AVEncVideoROIEnabled`, `AVEncVideoDirtyRectEnabled`, `AVEncVideoGradualIntraRefresh`, `AVEncVideoLTRBufferControl`, `MarkLTRFrame`, `UseLTRFrame`, `MinQP`, `EncodeQP`…) перевірено лише в mingw-w64 `codecapi.h`, локально. Документація Microsoft з сесії недоступна, тож чи підтримують їх конкретні MFT — UNVERIFIED.

**Код цього репозиторію:**
- `hub/cmd/hub-webrtc/main.go:56-62` — offer живого Chrome 05.09.
- `hub/cmd/hub-webrtc/main.go:2179` — `h264ProfileCompatible`.
- `agent/encode/mft.c:484-531`, `:886`, `:1508`.
- `internal/refine/refine.go`, `internal/h264/annexb.go:203`.
- `bench/quality/RESULTS.md`, `RESULTS-codec444-motion.md`, `STAGE3-444.md`.
