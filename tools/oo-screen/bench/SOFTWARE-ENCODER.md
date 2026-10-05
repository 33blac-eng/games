# Софтверний H.264 на ПК без GPU-енкодера: MS MFT проти OpenH264

ТЗ P8, RESEARCH-leaders.md (ПК без апаратного енкодера). Статус: дизайн-нотатка, **без замірів на парку**. Усі цифри CPU нижче — з публічних джерел і позначені як такі. Бінарі не вендоримо.

## 1. Що є зараз (Microsoft H264 Video Encoder MFT)

Шлях: `agent/encode/mft.c` (`force_software` / `OOS_ENC_NOHW`), у Go — `openEncoder` у `agent/cmd/oo-agent/main.go`: повторне відкриття в рідній роздільності, бо `submit_cpu` обрізає кадр, а не масштабує.

Налаштування софт-MFT після коміту P8:

| Параметр | Значення | Примітка |
|---|---|---|
| Профіль | Main (High лише як резерв) | Chrome на парку не оголошує High |
| Rate control | PeakConstrainedVBR, Max = 1,5×mean, HRD = 0,5 с | спільне з hw |
| LowLatency | `MF_LOW_LATENCY` + `CODECAPI_AVLowLatencyMode` | |
| B-кадри | 0 | |
| GOP | 2×fps (`MF_MT_MAX_KEYFRAME_SPACING` + `AVEncMPVGOPSize`) | |
| Refs | 2 | |
| QualityVsSpeed | 80 | заміряно лише на NVENC; для софту — UNVERIFIED |
| **NumWorkerThreads** | ядра − 1 при ≥4 ядрах | **нове**: інакше MFT займає всі ядра і гальмує Excel/1С |
| **CABAC** | увімкнено | **нове**: дефолт софт-MFT не задокументований; на тексті CABAC економить потік |
| **Порядок** | CodecAPI ставиться ДО `SetOutputType` (і повторно після) | **нове**: MSDN «H.264 Video Encoder» вимагає частину властивостей до узгодження типу |
| Refine (низький QP на статиці) | вимкнено для софту | свідомо: зайвий кадр коштує CPU |

Втрати на софт-шляху, які лишаються:

* Копія NV12 у `MFCreateMemoryBuffer` на кожен кадр (`oos_enc_submit_cpu`) плюс readback з GPU у capture: на 2560×1440 це ~5,5 МБ memcpy на кадр. Без переходу на `IMF2DBuffer` з пулом це не прибрати; виграш невеликий порівняно з самим кодуванням.
* Роздільність не знижується: `submit_cpu` обрізає, а не масштабує. Через це крок «роздільність» політики `internal/swlimit` поки лише записується в лог.
* Режиму screen content у MS MFT немає: ні palette, ні intra-block-copy, ні окремого rate control для статики.

## 2. Автоліміти (вже в коді)

`internal/swlimit`: EWMA стінного часу `Encode` порівнюється з бюджетом, який залежить від кількості ядер: 0,5 інтервалу кадру на 1–2 ядрах, 0,7 на 3–4 ядрах, 0,85 на більшій кількості. Поступки йдуть так: спершу FPS (30→24→20→15→12→10), потім роздільність (3/4, 2/3, поки лише як порада). Відновлення — у зворотному порядку. Гістерезис: 2 с перевантаження, щоб поступитися, і 10 с прогнозованого запасу на наступному кроці, щоб повернути його. Політика працює поверх статичної апріорної стелі `softwareFPSCap`.

## 3. OpenH264 (Cisco) як альтернатива

### Ліцензія і патенти

* Вихідний код OpenH264 поширюється за BSD-2-Clause.
* Патентне покриття MPEG LA (тепер Via LA) Cisco оплачує **лише для бінарів, які Cisco сама збирає і роздає** з `ciscobinary.openh264.org`. Ліцензія цих бінарів (openh264.org/BINARY_LICENSE.txt) прямо вимагає, щоб бінар **не постачався разом із продуктом**: кінцевий користувач або інсталятор завантажує його з сервера Cisco. Так роблять Firefox і GStreamer у дистрибутивах.
* Звідси вимоги до нас: інсталятор або агент завантажує `openh264-<ver>-win64.dll.bz2` з Cisco, перевіряє SHA-256 і підпис, розпаковує поруч з агентом, показує користувачу інформацію про ліцензію і дає змогу вимкнути завантаження. Власна збірка з вихідного коду патентного покриття не має.
* Для порівняння: MS MFT уже ліцензований разом з Windows. На N-редакціях без Media Feature Pack його немає, і там OpenH264 — єдиний варіант без GPU.

### Інтеграція

* `LoadLibraryW` (за повним шляхом, `LOAD_LIBRARY_SEARCH_*`, щоб уникнути DLL hijacking) + `GetProcAddress("WelsCreateSVCEncoder")`, `WelsDestroySVCEncoder`, `WelsGetCodecVersionEx`. Перевіряємо мажорну версію ABI: vtable `ISVCEncoder` змінюється між релізами (наприклад, 2.x).
* Мінімальні виклики: `InitializeExt(SEncParamExt)` → `EncodeFrame(SSourcePicture I420)` → `SFrameBSInfo` (Annex-B NAL) → `ForceIntraFrame` / `SetOption(ENCODER_OPTION_BITRATE)`. Цей набір мапиться на наш інтерфейс `encode.Encoder`: Encode, ForceIDR, SetBitrate.
* Ключові параметри:
  * `iUsageType = SCREEN_CONTENT_REAL_TIME`;
  * `iRCMode = RC_BITRATE_MODE` (або `RC_QUALITY_MODE`);
  * `bEnableFrameSkip`;
  * `iMultipleThreadIdc = cores-1`;
  * `iEntropyCodingModeFlag = 1` (CABAC — лише від версії 2.x; у 1.x профіль Baseline/CAVLC);
  * `uiIntraPeriod = 2*fps`;
  * `iNumRefFrame`;
  * `bEnableLongTermReference` (для screen content).
* Вхід I420, а не NV12: потрібна деінтерлівація UV, дешевий цикл на CPU.
* **Профіль**: OpenH264 історично кодує Constrained Baseline (42e01f). На парку Chrome оголошує 42e01f, тож SDP сумісний. Але без CABAC і 8×8 transform потік на тому самому тексті більший.
* Збірка: cgo-обгортка в окремому файлі з build-тегом `windows`, без лінкування — лише `LoadLibrary`. Фолбек-ланцюг такий: hw MFT → OpenH264 (якщо DLL є і пройшла перевірку) → MS MFT.

### CPU: що кажуть джерела

* Cisco/WebRTC: OpenH264 спроєктований для реального часу в WebRTC. Його використовують Firefox (WebRTC H.264) і libwebrtc як софтверний H.264 encoder. У режимі `SCREEN_CONTENT_REAL_TIME` він вмикає детекцію статичних і скрольованих ділянок (scroll detection, background detection) і пропускає кодування незмінених макроблоків. На робочому столі саме це основне джерело економії CPU. Джерела: github.com/cisco/openh264 (README, `codec/api/wels/codec_app_def.h`, `EUsageType`), `codec/encoder/core/src/wels_preprocess.cpp` (ScrollDetection, BackgroundDetection).
* Порівняльні заміри OpenH264 і x264 (наприклад, обговорення в issues cisco/openh264 і тести в Chromium bug tracker щодо софтверного H.264 у WebRTC): OpenH264 у режимі real-time швидший за x264 `medium`, але гірший за якістю при тому самому бітрейті. Для 720p30 це порядку одного ядра. **UNVERIFIED** для наших 1080p/1440p: точних цифр для робочого столу з першоджерела в цій нотатці немає.
* MS MFT: на цьому проєкті заміряно 3,31 ядра на 2560×1440 при 30 к/с (див. `softwareMeasuredCores` в `agent/cmd/oo-agent/output.go`). Пряме порівняння з OpenH264 на тому самому корпусі ще не робили.

Очікування (гіпотеза, яку треба заміряти): на статичному або малорухомому робочому столі OpenH264 SCREEN_CONTENT дасть суттєво нижчий CPU за рахунок пропуску незмінених MB. На скролі й відео різниця менша. Наш DXGI-пропуск no-op кадрів частину цієї переваги вже забирає.

### Ризики

* Завантаження з інтернету під час інсталяції: на закритих офісних мережах його може не бути. Тоді лишається MS MFT, тобто фолбек обов'язковий.
* ABI-дрейф між версіями DLL. Треба пінити версію і хеш.
* Baseline/CAVLC (1.x) на тексті програє Main/CABAC MS MFT за бітрейтом.
* Додаткова поверхня атаки: стороння DLL у процесі агента. Обов'язкові перевірка підпису і хешу та завантаження лише за повним шляхом.

## 4. Рекомендація

1. **Зараз**: лишити MS MFT як софтверний шлях за замовчуванням. Новий тюнінг (потоки, CABAC, порядок CodecAPI) разом з адаптивною FPS-політикою `internal/swlimit` закриває головний ризик — гальмування ПК користувача.
2. **Далі**: зробити прототип OpenH264 за прапорцем `-sw-encoder=openh264` (DLL завантажує оператор вручну) і заміряти на корпусі `bench/corpus` проти MS MFT: CPU (ядра), якість тексту (`bench/quality`), бітрейт. Рішення приймати за цифрами. Перемикати за замовчуванням варто лише за умови: CPU ≥30% нижчий при не гіршій якості тексту.
3. Якщо OpenH264 виграє, завантаження з Cisco CDN робить інсталятор (не агент під час роботи) з пінованим хешем і зрозумілим повідомленням про ліцензію. Бінар у репозиторій і в пакет не кладемо.
4. Окремо: масштабування на софт-шляху (CPU-ресемплер або GPU VideoProcessor перед readback), щоб крок «роздільність» зі `swlimit` перестав бути лише порадою.

## Джерела

* Microsoft Learn — «H.264 Video Encoder» (Media Foundation): підтримувані ICodecAPI, вимоги до порядку налаштування.
* Microsoft Learn — `CODECAPI_AVEncNumWorkerThreads`, `CODECAPI_AVEncH264CABACEnable`, `CODECAPI_AVLowLatencyMode`.
* github.com/cisco/openh264 — README, `codec/api/wels/codec_app_def.h` (`EUsageType::SCREEN_CONTENT_REAL_TIME`), `codec_api.h` (`WelsCreateSVCEncoder`).
* openh264.org — «Binary License» і FAQ про патентне покриття та заборону вбудовувати бінар у дистрибутив.
* Mozilla — інтеграція OpenH264 GMP у Firefox (завантаження плагіна з Cisco під час роботи).
