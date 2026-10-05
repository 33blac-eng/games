# oo-screen — передача екрана ПК у консоль ЕРП

> **Прод (05.09.2026):** хаб `hub-webrtc` на VPS 185.166.216.204 як
> `oo-screen-hub-webrtc-t1.service` (:4470 сигналінг за nginx
> `remote.organicoils.com.ua/offer/*`, `/control`, `/nodes`, `/healthz`;
> UDP :4544 — один ICE-сокет на всі ноги). Секрети — в
> `/etc/oo-screen/hub.env` (0600), не в unit-файлі. Агент `oo-agent.exe` на ПК
> у `C:\ProgramData\OrganicOils\ScreenAgent`, задача `oo-screen-pilot`
> (onlogon), транспорт `webrtc`, 30 к/с. Викочування: хаб —
> `deploy/oo_screen_hub_deploy.py --src <дерево>`, агент —
> `deploy/oo_screen_rollout.py --agent <exe>` з `OO_SCREEN_T1_TOKEN` в env.
> Здоровʼя: `deploy/ops/oo_screen_health.py`. Легасі `hub-wt` (:4460 QUIC)
> на проді ВИМКНЕНО. Усе нижче — про локальний T1-стенд, не про прод.

## T1-стенд (історично)

План: `docs/superpowers/plans/2026-08-26-oo-screen-core-plan.md` (єдине джерело).
Тут — лише робочі конвенції T1, ОДНАКОВІ для обох кандидатів.

## Корпус
`bench/corpus/corpus-1080p60.h264` — Annex-B, High 4.2, 1920×1080@60, CBR 8M,
IDR/2с, repeat-headers, без B-кадрів. 600 AU / 10с; плеєр зациклює (frame_seq
монотонний через цикли, PTS = seq × 16667 мкс). Кадр 0 і кожен «SEQ N» — база
для capture→render flash.

## Спільні пакети (НЕ дублювати, імпортувати)
- `internal/envelope` — конверт Додатка D (Marshal/ReadFrame).
- `internal/h264` — SplitAUs / SplitNALs / ParseSPS → CodecString().

## Порти (локальний T1-стенд; на VPS зміняться)
- 4460/udp — hub кандидата B: QUIC ingest від агента (ALPN `oo-screen-agent`).
- 4461/udp — hub кандидата B: WebTransport (HTTP/3) для браузера, шлях `/wt`.
- 4470/tcp — hub кандидата A: HTTP signaling (`POST /offer/agent`, `POST /offer/viewer`).
- 4480/tcp — статика web/ (viewer-сторінки) для обох.

## Авторизація T1
Статичний токен `OO_SCREEN_T1_TOKEN` (env, дефолт `t1-dev-token`) у query
`?token=` (браузер) та в першому control-повідомленні (агент). Повний
ticket-протокол §6.4 — після bake-off.

## Метрики (ОДНА схема для A і B — інакше порівняння недійсне)
Кожен viewer пише в консоль і накопичує JSON-рядки (NDJSON), по одному на кадр:
```json
{"seq":123,"t_arrival_ms":..., "t_decoded_ms":..., "t_rendered_ms":..., "bytes":N, "key":false}
```
`t_*` — performance.now() браузера (ЛОКАЛЬНІ тривалості, крос-машинні годинники
не віднімаємо — R2#19). Кнопка «Download metrics» зливає NDJSON-файл.
Hub логуює per-leg: arrival від агента, відправку глядачу (stdout NDJSON,
`{"leg":"agent"|"viewer","seq":N,"t_ms":...}`).

## Запуск (ціль тижня 1)
```
bench/run_b.sh   # hub-wt + corpus-player-wt + serve web → браузер /viewer-wt.html
bench/run_a.sh   # hub-webrtc + corpus-player-webrtc + serve web → /viewer-webrtc.html
```
Impairment: clumsy (Windows) на UDP hub-ноги; профілі Додатка A — наступний крок.
