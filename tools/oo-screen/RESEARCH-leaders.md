# oo-screen проти лідерів ринку: 20 параметрів

Дата: 2026-10-03. Порівнюємо з Parsec, RustDesk, Chrome Remote Desktop (CRD), Moonlight/Sunshine, AnyDesk, а також з MS RDP (AVC444).
Позначка **UNVERIFIED** означає, що факт узято з загальних знань, а в джерелі під час цього дослідження його не перевіряли.

Наше головне обмеження: браузерний `<video>` через Chrome WebRTC приймає лише H.264 (Constrained Baseline/Main 4:2:0). Тому 4:4:4 у профілі High 4:4:4 і HEVC для нас недоступні.

## Джерела
- [P1] Parsec, 4:4:4 і HEVC: https://support.parsec.app/hc/en-us/articles/32381785123860-Improve-Stream-Quality-and-Color-Accuracy
- [R1] RustDesk, кодеки: https://rustdesk.com/docs/en ; https://flathub.org/apps/com.rustdesk.RustDesk
- [M1] Sunshine/Moonlight, YUV444: https://github.com/orgs/LizardByte/discussions/220 ; https://docs.lizardbyte.dev/projects/sunshine/latest/structvideo_1_1encoder__t.html
- [MS1] RDP 10, AVC444: https://cloudblogs.microsoft.com/enterprisemobility/2016/01/11/remote-desktop-protocol-rdp-10-avch-264-improvements-in-windows-10-and-windows-server-2016-technical-preview/
- [MS2] MS-RDPEGFX, AVC444 як дві підрамки 4:2:0: https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-rdpegfx/844018a5-d717-4bc9-bddb-8b4d6be5dd3f
- [C1] CRD, код Chromium remoting (WebRTC, VP8/VP9/AV1): https://source.chromium.org/chromium/chromium/src/+/main:remoting/ (UNVERIFIED у межах цієї сесії)

## Таблиця

| # | Параметр | Parsec | RustDesk | CRD | Moonlight/Sunshine | AnyDesk | oo-screen зараз | Чого бракує | Чи можна з H.264-only у Chrome? | Пріоритет |
|---|---|---|---|---|---|---|---|---|---|---|
| 1 | Кольоровість 4:4:4 / чіткий текст | Опційне 4:4:4 на NVIDIA/Intel [P1] | Є опція True color 4:4:4 для VP9/AV1 (UNVERIFIED) | VP9 4:4:4 у режимі lossless color (UNVERIFIED) | Експериментальний YUV444, лише хост на Windows [M1] | DeskRT, 4:4:4 нестиснуто (UNVERIFIED) | 4:2:0 BT.709 limited | Червоні та сині ореоли на тексті | Нативно ні. Можна обійти: AVC444-подібна друга підрамка з chroma (2 треки) [MS2] або lossless-шар тексту в canvas | High |
| 2 | Кодеки | H.264, H.265 [P1] | VP8/VP9/AV1 програмно, H.264/H.265 апаратно [R1] | VP8/VP9 (AV1 експериментально) (UNVERIFIED) | H.264/HEVC/AV1 [M1] | Власний DeskRT (UNVERIFIED) | Лише H.264 Main | VP9/AV1 для слабких ПК без GPU | Частково: Chrome WebRTC підтримує VP8/VP9/AV1. Обмеження H.264 — це вибір, а не правило | Med |
| 3 | Інструменти для screen content (IBC, palette) | Ні (UNVERIFIED) | AV1 має SCC-інструменти (palette/IntraBC) у libaom (UNVERIFIED) | AV1 screen mode (UNVERIFIED) | Ні | DeskRT заточено під екран (UNVERIFIED) | Ні | Немає SCC-інструментів | Лише через AV1 (`aq-mode`/`tune-content=screen`) | Med |
| 4 | Lossless-доуточнення статичних ділянок | Ні (UNVERIFIED) | Ні (UNVERIFIED) | Так, «quality refine» після бездіяльності (UNVERIFIED) | Ні | Так (UNVERIFIED) | Ні: статичний кадр лишається з QP останнього кодування | Текст не «дотягується» до різкості після зупинки руху | Так: після N мс без змін послати IDR/P-кадр з низьким QP (MFT `CODECAPI_AVEncVideoEncodeQP`) | High |
| 5 | Dirty rects / ROI | Так (UNVERIFIED) | Порівняння кадрів, пропуск незмінених (UNVERIFIED) | Так, через DXGI dirty rects (UNVERIFIED) | Ні, кодує повний кадр | Так (UNVERIFIED) | DXGI дає dirty rects, але кодується весь кадр | Не пропускаються порожні кадри, немає ROI | Так: пропуск кадрів без змін + ROI QP map (`CODECAPI_AVEncVideoROIEnabled`, Win11) | High |
| 6 | Адаптивний QP для тексту | UNVERIFIED | UNVERIFIED | UNVERIFIED | Ні | UNVERIFIED | Ні | QP не зменшується на текстових блоках | Так (ROI/delta-QP map, якщо MFT підтримує) | Med |
| 7 | Rate control / congestion control | Власний, BWE (UNVERIFIED) | Власний за затримкою та якістю (UNVERIFIED) | WebRTC GCC/TWCC (UNVERIFIED) | Фіксований бітрейт, задається користувачем | Власний (UNVERIFIED) | Loss/RTT/REMB, нижня межа 500 kbps | Немає TWCC/GCC на основі затримки | Так: Pion interceptor GCC + TWCC | High |
| 8 | FEC | UNVERIFIED | Ні (TCP/KCP) (UNVERIFIED) | ULPFEC/FlexFEC через WebRTC (UNVERIFIED) | Reed-Solomon FEC (UNVERIFIED) | UNVERIFIED | Ні (лише NACK) | Втрати на лінках з високим RTT | Так: ULPFEC/RED у Pion, Chrome декодує | Low |
| 9 | NACK/PLI/intra-refresh | UNVERIFIED | UNVERIFIED | WebRTC NACK/PLI (UNVERIFIED) | Ref-frame invalidation (UNVERIFIED) | UNVERIFIED | NACK-кеш, debounce PLI, кеш хвоста GOP | Немає intra-refresh і reference invalidation, IDR дає сплеск | Так: gradual intra refresh через MFT (`AVEncVideoGradualIntraRefresh` або LTR) | Med |
| 10 | Jitter buffer / затримка | Дуже низька, <30 мс на LAN [P1] | UNVERIFIED | Буфер WebRTC (UNVERIFIED) | Мінімальний | Низька (UNVERIFIED) | 19–20 мс на LAN, буфер Chrome | Не задано `playoutDelayHint`=0 (UNVERIFIED) | Так: `receiver.playoutDelayHint`/`jitterBufferTarget` | Med |
| 11 | Адаптивні FPS і роздільна здатність | Так (UNVERIFIED) | Авто-FPS (UNVERIFIED) | Так, WebRTC degradation (UNVERIFIED) | Ні (вручну) | Так (UNVERIFIED) | 30 fps, даунскейл GPU лише як резервний шлях | Не знижуємо FPS замість роздільності (для тексту краще знижувати FPS) | Так: `maintain-resolution` — знижувати FPS під навантаженням | High |
| 12 | HiDPI / масштабування | Так (UNVERIFIED) | Так (UNVERIFIED) | Так, піксель в піксель (UNVERIFIED) | Так | Так (UNVERIFIED) | `object-fit: fill`, CSS-розмір | Немає режиму 1:1 з урахуванням devicePixelRatio, розтягування розмиває текст | Так: показ 1:1 і скрол | High |
| 13 | Курсор окремим шаром | Так (UNVERIFIED) | Так (UNVERIFIED) | Так (UNVERIFIED) | Ні, курсор у кадрі | Так (UNVERIFIED) | UNVERIFIED (у DXGI курсор окремо) | Якщо курсор у відео, рух миші коштує кадри | Так: форма через DataChannel + CSS cursor | Med |
| 14 | Кілька моніторів | Так | Так [R1] | Так (UNVERIFIED) | Один на сесію | Так | UNVERIFIED | Перемикання та вибір дисплея | Так | Low |
| 15 | Аудіо | Так | Так | Так | Так | Так | Opus | — | Так | — |
| 16 | Буфер обміну / передавання файлів | Буфер обміну; файли (UNVERIFIED) | Так [R1] | Так | Ні | Так | UNVERIFIED (через MeshCentral) | Текстовий буфер обміну у DataChannel | Так | Med |
| 17 | Затримка вводу | Низька | Середня | Середня | Дуже низька | Низька | DataChannel → SendInput | Чи DataChannel unordered/unreliable для руху миші (UNVERIFIED) | Так | Low |
| 18 | Програмний енкодер на ПК без GPU | Ні, потрібен апаратний кодер (UNVERIFIED) | libvpx/libaom [R1] | libvpx (UNVERIFIED) | Програмний x264 є (UNVERIFIED) | Так (UNVERIFIED) | MS SW MFT H.264 (якість низька, CBR) (UNVERIFIED) | Немає якісного SW-шляху: openh264/x264 з tune для екрана | Так: x264 `--tune` screen/zerolatency або openh264 screen mode | High |
| 19 | NAT / P2P чи relay | P2P + relay (UNVERIFIED) | P2P + власний relay [R1] | ICE/TURN (UNVERIFIED) | Прямо або LAN | P2P + relay (UNVERIFIED) | Через Go hub (усе через relay) | Немає P2P-шляху agent↔browser | Так: ICE прямо, hub лише сигналізація або TURN | Low |
| 20 | Шифрування / запис | E2E (UNVERIFIED) | E2E NaCl (UNVERIFIED) | DTLS-SRTP | AES-GCM (UNVERIFIED) | TLS 1.2 + RSA (UNVERIFIED) | DTLS-SRTP до hub, запис сесій | Hub бачить потік (не E2E). Це прийнятно, бо потрібен запис | — | Low |

Довідково, MS RDP: AVC444 кодується як дві H.264-підрамки 4:2:0 (luma і додаткова chroma), а клієнт збирає 4:4:4 [MS1][MS2]. Саме цей прийом можна відтворити у браузері (WebCodecs або два `<video>` і WebGL-шейдер).

## Що нам бракує — топ-10
Порядок: виграш у якості / ризик / зусилля.

1. **Lossless/low-QP refine статичних ділянок** (#4). Найбільший виграш для Excel і коду. Ризик низький, зусилля мале: таймер бездіяльності плюс кадр із низьким QP.
2. **Пропуск незмінених кадрів за DXGI dirty rects** (#5). Економить бітрейт і CPU на слабких ПК. Ризик низький, зусилля мале.
3. **Відображення 1:1 з урахуванням DPI замість `object-fit: fill`** (#12). Прибирає розмиття від масштабування. Ризик низький, зусилля мале.
4. **Під навантаженням знижувати FPS, а не роздільність** (#11). Текст залишається різким. Зусилля мале.
5. **Якісний програмний H.264 (openh264/x264 у режимі для екрана)** (#18). Цільові ПК часто без GPU-кодера. Зусилля середнє, ризик — навантаження на CPU.
6. **GCC/TWCC замість лише loss/REMB** (#7). Краще поводження на офісному інтернеті. Зусилля середнє (Pion interceptor).
7. **Псевдо-4:4:4 як в AVC444** (#1). Прибирає кольорові ореоли. Зусилля велике: друга підрамка, WebGL-збирання. Ризик — синхронізація двох потоків.
8. **Курсор окремим шаром** (#13). Рух миші не коштує кадрів. Зусилля мале–середнє.
9. **Intra-refresh або LTR замість повних IDR** (#9). Прибирає сплески бітрейту після PLI. Залежить від можливостей MFT.
10. **ROI / delta-QP для текстових блоків, або VP9/AV1 screen-content** (#6, #2, #3). Наступний крок після пунктів 1–2. Зусилля середнє–велике.
