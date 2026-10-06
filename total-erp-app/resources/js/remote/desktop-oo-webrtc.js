// desktop-oo-webrtc.js — переможець bake-off (кандидат A): OO-шар поверх Mesh
// на WebRTC замість WebTransport+WebCodecs.
//
// Що ЛИШАЄТЬСЯ спільним із desktop-oo.js (імпортуємо, НЕ дублюємо):
//   • createOoSession — generation token + обидва сторожі (8с на перший кадр,
//     6с frame-age) + атомарний одноразовий фолбек;
//   • meshCall/PAUSE-UNPAUSE-REFRESH — duck-typing по обгортці Mesh;
//   • та сама state-машина connecting/live/fallback/closed.
//
// Що ІНШЕ: замість worker+OffscreenCanvas+WebCodecs — overlay <video> і
// RTCPeerConnection recvonly. Наслідки, які тут враховані:
//   1. Декодує сам браузер, тож preflight WebCodecs не потрібен — але й
//      «декодер не тягне» ми дізнаємось лише за відсутністю кадрів. Тому
//      паузу Mesh ставимо ЛИШЕ НА ПЕРШОМУ OO-КАДРІ: до нього Mesh лишається
//      живим, і ні зрив сигналізації, ні повільний перший кадр не коштують
//      чорного чи застиглого екрана.
//   2. У WebRTC немає «події кадру» — є rVFC. Він і є джерелом для
//      noteFrame(); там, де rVFC недоступний, беремо 'loadeddata' як перший
//      кадр, а далі — ріст getVideoPlaybackQuality().totalVideoFrames і
//      framesDecoded із getStats. НЕ currentTime/timeupdate: вони тікають і
//      на застиглому треку, і frame-age не спрацював би ніколи.
//   3. <video> — НЕ input-поверхня. pointer-events:none обов'язковий, як і в
//      canvas-версії: інакше шар з'їдає mousedown і Mesh «зависає».

import {
    createOoSession,
    createOoRetry,
    meshCall,
    OO_STATE_CONNECTING,
    OO_STATE_LIVE,
    OO_STATE_FALLBACK,
    OO_STATE_CLOSED,
    MESH_PAUSE_NAMES,
    MESH_UNPAUSE_NAMES,
    MESH_REFRESH_NAMES,
    findMeshCanvas,
} from './desktop-oo.js';
import { createTileOverlay, TILES_LABEL } from './oo-text-tiles.js';
import { INPUT_CHANNEL_LABEL, inputEnabledFor, createInputSender, attachInputDom } from './desktop-oo-input.js';
import { createStatsOverlay } from './desktop-oo-stats.js';
import {
    CURSOR_CHANNEL_LABEL,
    createCursorLayer,
    resolveCursorRole,
} from './desktop-oo-cursor.js';

import { createOoInput } from './oo-input.js';

export { OO_STATE_CONNECTING, OO_STATE_LIVE, OO_STATE_FALLBACK, OO_STATE_CLOSED };

const DEFAULT_GEOMETRY_MS = 10000;
const DEFAULT_OFFER_TIMEOUT_MS = 8000;
// §MAJOR-6: 'disconnected' у WebRTC транзієнтний (перемикання мережі, коротка
// втрата ICE) і часто сам відновлюється. Даємо йому grace, і лише якщо після
// нього все ще погано — фолбек. 'failed' — остаточний, без grace.
// 10с: Chrome часто повертається з 'disconnected' за 5–15с (перемикання Wi-Fi);
// 4с спалювали спробу reconnect на мережі, що й так відновилась би.
const DEFAULT_DISCONNECT_GRACE_MS = 10000;
// C3: 'disconnected' довше за 3с (або 'failed') — спершу ICE restart на тому ж PC;
// не відновився за 5с — повний tryReconnect.
const DEFAULT_ICE_RESTART_AFTER_MS = 3000;
const DEFAULT_ICE_RESTART_TIMEOUT_MS = 5000;
// Дедлайн збирання ICE (картинка, ICE restart і звук — одне число): без TURN
// gathering зрідка не доходить до 'complete' взагалі, і offer ніколи б не поїхав.
export const ICE_GATHER_DEADLINE_MS = 2000;
// №14: поріг «помітних» втрат і jitter-буфер для них.
const LOSSY_LOSS_PCT = 0.01;
const LOSSY_JITTER_TARGET_MS = 60;
// Після стількох мс у live бюджет reconnect поповнюється: яма Wi-Fi о 10:00 не
// має забирати в людини OO о 15:00. Мерехтливий канал (live < 60с) — не поповнює.
const DEFAULT_RECONNECT_REFILL_MS = 60000;
// Після повернення вкладки агентові треба resume + IDR; frame-age рахуємо не
// одразу, а з цим запасом (як для першого кадру).
const DEFAULT_VISIBLE_GRACE_MS = 5000;
// §MAJOR-7: у прихованій вкладці rVFC тротлиться, тож поки hidden — «підживлюємо»
// frame-age цим кроком, щоб сторож не завалив живу сесію хибним фолбеком.
const DEFAULT_HIDDEN_KEEPALIVE_MS = 1000;
// Живий моніторинг якості: оператор має БАЧИТИ fps/rtt/loss на живій сесії, а не
// дізнаватись про деградацію зі скарги. Пороги й крок — як у проді (відновлено
// 29.08.2026 з бандла desktop-oo-webrtc-CRqTtW6Y.js).
const DEFAULT_STATS_INTERVAL_MS = 5000;
const DEFAULT_QUALITY_DEGRADE_MS = 30000;  // 30с вікно: транзієнтна яма не смикає картинку
const DEFAULT_QUALITY_MIN_FPS = 8;
const DEFAULT_QUALITY_MAX_RTT_MS = 600;
const DEFAULT_QUALITY_MAX_LOSS = 0.15;
// F-13: віддалений СТІЛ — це не відео з YouTube. Браузер за замовчуванням
// тримає jitter-буфер під плавність відтворення (десятки-сотні мілісекунд), і
// на керуванні мишею це відчувається як «курсор їде за рукою». Просимо нуль:
// краще випадкове смикання, ніж стала затримка на порожньому місці.
const DEFAULT_PLAYOUT_DELAY_S = 0;

// ─────────────────────────────────────────────────────────────────────────────
// ЧИСТІ ХЕЛПЕРИ (без DOM/RTC) — гейт tests/js/oo-webrtc-resilience.test.mjs.
// ─────────────────────────────────────────────────────────────────────────────

/**
 * orderCodecs — F-13: ставить кодеки з `prefer` попереду решти, зберігаючи
 * ВСІ інші (їх викидати не можна: якщо агент раптом віддасть інший кодек,
 * порожній перетин = чорний екран замість «трохи гіршого» кодека).
 *
 * Порівняння за суфіксом mimeType після '/', регістронезалежно: 'video/H264'.
 *
 * @param {Array<{mimeType?:string}>} codecs  capabilities.codecs
 * @param {Array<string>} prefer              ['H264','VP8'] — у порядку бажаності
 * @returns {Array} той самий масив об'єктів, переупорядкований
 */
export function orderCodecs(codecs, prefer) {
    const list = Array.isArray(codecs) ? codecs.slice() : [];
    const want = (Array.isArray(prefer) ? prefer : []).map((s) => String(s).toLowerCase());
    if (!list.length || !want.length) return list;
    const rank = (c) => {
        const mime = String((c && c.mimeType) || '');
        const name = mime.slice(mime.indexOf('/') + 1).toLowerCase();
        const i = want.indexOf(name);
        return i === -1 ? want.length : i;
    };
    // Стабільне сортування: усередині однакового рангу порядок браузера
    // лишається як був (там уже відсортовано за профілями/payload-type).
    return list
        .map((c, i) => ({ c, i, r: rank(c) }))
        .sort((a, b) => (a.r - b.r) || (a.i - b.i))
        .map((x) => x.c);
}

/**
 * h264ProfileRank — PLAYER-QUALITY P-1: місце H.264-варіанта в offer-і.
 * Повертає [packetizationRank, profileRank] — менше = раніше.
 *
 * ЧОМУ порядок усередині H.264 взагалі важливий. Хаб і агент (pion) шукають
 * точний збіг fmtp: packetization-mode + profile_idc + profile_iop. Новий
 * агент кодує Main 4d40xx (constraint_set1), а Chrome оголошує Main як 4d001f
 * (iop 0x00) — точного збігу НЕМАЄ, і pion бере «частковий»: ПЕРШИЙ H.264-PT
 * з offer-а браузера (rtpcodec.go codecParametersFuzzySearch, TrackLocal.Bind).
 * У дефолтному порядку Chrome першим іде 42001f (Baseline), тож Main-потік
 * їде під міткою Baseline: декодер браузера налаштовується під профіль із SDP
 * і лише потім бачить справжній SPS. Ставимо Main першим — мітка збігається з
 * потоком. Далі Constrained Baseline 42e0xx (сумісний з усіма декодерами),
 * Baseline, невідомі, High 4:4:4 (f4) і High (64) — ОСТАННІМИ: Chrome НЕ
 * приймає High від нашого хаба надійно (TASK.md), але й викидати 64xx не
 * можна — старий агент 64002a домовляється саме точним збігом з 64001f.
 * packetization-mode=1 — завжди перед 0: агент шле FU-A, а pm=0 їх не
 * передбачає.
 */
export function h264ProfileRank(fmtp) {
    const s = String(fmtp || '').toLowerCase();
    const pm = /(?:^|;)\s*packetization-mode\s*=\s*(\d+)/.exec(s);
    const pmRank = pm && pm[1] === '1' ? 0 : 1;
    const m = /(?:^|;)\s*profile-level-id\s*=\s*([0-9a-f]{6})/.exec(s);
    if (!m) return [pmRank, 3];
    const idc = parseInt(m[1].slice(0, 2), 16);
    const iop = parseInt(m[1].slice(2, 4), 16);
    if (idc === 0x4d) return [pmRank, 0];                 // Main
    if (idc === 0x42 && (iop & 0x40)) return [pmRank, 1]; // Constrained Baseline
    if (idc === 0x42) return [pmRank, 2];                 // Baseline
    if (idc === 0xf4) return [pmRank, 4];                 // High 4:4:4 Predictive
    if (idc === 0x64) return [pmRank, 5];                 // High — лише для старого агента
    return [pmRank, 3];
}

/**
 * orderH264Profiles — P-1: переставляє ЛИШЕ H.264-записи між собою (за
 * h264ProfileRank, стабільно), лишаючи кожен не-H.264 кодек на його місці.
 * Нічого не викидає (див. orderCodecs). Записи без sdpFmtpLine мають
 * однаковий ранг — їхній порядок не змінюється.
 */
export function orderH264Profiles(codecs) {
    const list = Array.isArray(codecs) ? codecs.slice() : [];
    const isH264 = (c) => /\/h264$/i.test(String((c && c.mimeType) || ''));
    const slots = [];
    const items = [];
    list.forEach((c, i) => { if (isH264(c)) { slots.push(i); items.push({ c, i, r: h264ProfileRank(c.sdpFmtpLine) }); } });
    if (items.length < 2) return list;
    items.sort((a, b) => (a.r[0] - b.r[0]) || (a.r[1] - b.r[1]) || (a.i - b.i));
    slots.forEach((slot, k) => { list[slot] = items[k].c; });
    return list;
}

/**
 * applyCodecPreferences — F-13: просить браузер ставити H264 першим у offer.
 * Агент кодує саме H264 (NVENC/MFT), і якщо offer починається з VP8, хаб
 * зобов'язаний або перекодовувати, або домовлятись довше.
 * P-1: усередині H264 — Main першим (orderH264Profiles); opts.h264Profiles
 * === false — порядок профілів як у браузера (відкат).
 *
 * Усе всередині try: setCodecPreferences кидає на непідтримуваних наборах, а
 * помилка тут не сміє коштувати сесії — без преференцій просто трохи гірше.
 *
 * @returns {boolean} чи справді застосовано
 */
export function applyCodecPreferences(transceiver, RTCRtpReceiverCtor, prefer, opts) {
    if (!transceiver || typeof transceiver.setCodecPreferences !== 'function') return false;
    const R = RTCRtpReceiverCtor;
    if (!R || typeof R.getCapabilities !== 'function') return false;
    try {
        const caps = R.getCapabilities('video');
        if (!caps || !Array.isArray(caps.codecs) || !caps.codecs.length) return false;
        let list = orderCodecs(caps.codecs, prefer || ['H264']);
        if (!opts || opts.h264Profiles !== false) list = orderH264Profiles(list);
        transceiver.setCodecPreferences(list);
        return true;
    } catch (e) {
        return false;
    }
}

/**
 * applyLowLatencyReceiver — F-13: знімає з ВІДЕО-приймача буфер плавності.
 *   playoutDelayHint (Chromium) — секунди;
 *   jitterBufferTarget (стандарт, Chrome 114+) — мілісекунди.
 * Обидва — саме hint: браузер має право не послухатись, і це нормально.
 *
 * Аудіо тут НЕ чіпаємо свідомо: на звуці нульовий буфер дає клацання.
 *
 * @returns {Array<string>} які ручки реально виставились (для тесту й логів)
 */
export function applyLowLatencyReceiver(receiver, kind, delaySeconds) {
    const applied = [];
    if (!receiver || kind !== 'video') return applied;
    const sec = typeof delaySeconds === 'number' ? delaySeconds : DEFAULT_PLAYOUT_DELAY_S;
    try {
        if ('playoutDelayHint' in receiver) { receiver.playoutDelayHint = sec; applied.push('playoutDelayHint'); }
    } catch (e) { /* hint — не обов'язок браузера */ }
    try {
        if ('jitterBufferTarget' in receiver) { receiver.jitterBufferTarget = sec * 1000; applied.push('jitterBufferTarget'); }
    } catch (e) { /* те саме */ }
    return applied;
}

/**
 * fitRect — F-29: вписує картинку srcW×srcH у коробку boxW×boxH БЕЗ спотворення.
 *
 * ЧОМУ не object-fit:contain: 0..1 для oo-input рахується по прямокутнику
 * САМОГО <video> (surface: () => video.getBoundingClientRect()). З 'contain'
 * усередину елемента входять чорні поля лєтербоксу, і кожен клік поїхав би на
 * їхню товщину. Тому лєтербокс рахуємо самі й даємо елементові вже правильні
 * розміри — прямокутник елемента ДОРІВНЮЄ прямокутнику картинки.
 *
 * Розміри джерела невідомі (videoWidth ще 0) — віддаємо коробку як є: краще
 * один кадр розтягнутим, ніж нульовий розмір.
 *
 * @returns {{left:number, top:number, width:number, height:number}} зсув від кута коробки
 */
export function fitRect(boxW, boxH, srcW, srcH) {
    const bw = Math.max(0, boxW || 0);
    const bh = Math.max(0, boxH || 0);
    if (!(srcW > 0) || !(srcH > 0) || !bw || !bh) return { left: 0, top: 0, width: bw, height: bh };
    const scale = Math.min(bw / srcW, bh / srcH);
    // Math.min/Math.max — не косметика: на збігу пропорцій арифметика з
    // плаваючою комою дає w трохи БІЛЬШЕ за bw, і зсув виходить -0 замість 0
    // (від'ємний відступ = картинка на пікселі виїхала за Mesh-canvas).
    const w = Math.min(bw, srcW * scale);
    const h = Math.min(bh, srcH * scale);
    return { left: Math.max(0, (bw - w) / 2), top: Math.max(0, (bh - h) / 2), width: w, height: h };
}

/**
 * combineAbortSignals — §MAJOR-5: ОДИН signal, що абортиться і з teardown, і з
 * таймауту. Раніше fetch слухав лише AbortSignal.timeout() окремо від teardown,
 * тож при знищенні шару застарілий offer-fetch переживав teardown.
 *
 * Сучасний шлях — AbortSignal.any([teardown, AbortSignal.timeout(ms)]).
 * Фолбек для старих браузерів — власний контролер + ручний setTimeout.
 *
 * @returns {{signal: AbortSignal, cancel: Function}}
 */
export function combineAbortSignals(teardownSignal, timeoutMs, env) {
    const e = env || {};
    // 'any' in e / 'timeout' in e — щоб тест міг ЯВНО вимкнути сучасний шлях
    // (null), а не тільки не задати його (undefined → беремо глобальний).
    const anyFn = ('any' in e) ? e.any : (typeof AbortSignal !== 'undefined' ? AbortSignal.any : null);
    const timeoutFn = ('timeout' in e) ? e.timeout : (typeof AbortSignal !== 'undefined' ? AbortSignal.timeout : null);

    if (typeof anyFn === 'function' && typeof timeoutFn === 'function') {
        const parts = [];
        if (teardownSignal) parts.push(teardownSignal);
        parts.push(timeoutFn.call(null, timeoutMs));
        return { signal: anyFn.call(null, parts), cancel: () => {} };
    }

    // Фолбек: ручний контролер аборту з таймауту АБО з teardown.
    const Ctor = e.Controller || (typeof AbortController !== 'undefined' ? AbortController : null);
    if (!Ctor) return { signal: teardownSignal || undefined, cancel: () => {} };
    const ctrl = new Ctor();
    const setT = e.setTimeout || setTimeout;
    const clrT = e.clearTimeout || clearTimeout;
    const timer = setT(() => { try { ctrl.abort(); } catch (x) { /* ignore */ } }, timeoutMs);
    let onAbort = null;
    if (teardownSignal) {
        if (teardownSignal.aborted) { try { ctrl.abort(); } catch (x) { /* ignore */ } }
        else {
            onAbort = () => { try { ctrl.abort(); } catch (x) { /* ignore */ } };
            teardownSignal.addEventListener('abort', onAbort);
        }
    }
    return {
        signal: ctrl.signal,
        cancel: () => {
            clrT(timer);
            if (teardownSignal && onAbort) teardownSignal.removeEventListener('abort', onAbort);
        },
    };
}

/**
 * createDisconnectGrace — §MAJOR-6: grace-таймер для WebRTC connectionState.
 *   • 'closed'               → фолбек негайно (остаточний стан);
 *   • 'failed'               → C3: ICE restart (onRestart), без нього — фолбек негайно;
 *   • 'disconnected'         → через restartMs — ICE restart; без нього чекаємо
 *                              повний grace і лише тоді фолбек. Свідомо, для
 *                              старого хаба без leg: 'disconnected' часто сам
 *                              повертається за 5–15с, а повний reconnect на 3-й
 *                              секунді рвав би сесію, що відновилась би сама
 *                              (див. DEFAULT_DISCONNECT_GRACE_MS). Хаб без leg →
 *                              одразу tryReconnect лише на 'failed';
 *   • 'connected'/'completed'→ скасовує все (сесія відновилась).
 *
 * C3: onRestart() → true = restart пішов; тоді на відновлення restartTimeoutMs,
 * інакше фолбек 'ice-restart-timeout'. Провал самого restart — restartFailed(reason).
 * Поки restart триває, 'failed'/'disconnected' ігноруються: після restartIce
 * Chrome проходить через них по дорозі до 'connected'.
 *
 * @returns {{note: Function, restartFailed: Function, cancel: Function, pending: Function}}
 */
export function createDisconnectGrace(o) {
    const opts = o || {};
    const graceMs = opts.graceMs || DEFAULT_DISCONNECT_GRACE_MS;
    const restartMs = opts.restartMs || DEFAULT_ICE_RESTART_AFTER_MS;
    const restartTimeoutMs = opts.restartTimeoutMs || DEFAULT_ICE_RESTART_TIMEOUT_MS;
    const onFallback = opts.onFallback || (() => {});
    const onRestart = opts.onRestart || (() => false);
    const setT = opts.setTimeout || setTimeout;
    const clrT = opts.clearTimeout || clearTimeout;
    let timer = null;         // повний grace 'disconnected'
    let restartTimer = null;  // 3с до ICE restart
    let restartDeadline = null;

    function clear() {
        if (timer !== null) { clrT(timer); timer = null; }
        if (restartTimer !== null) { clrT(restartTimer); restartTimer = null; }
        if (restartDeadline !== null) { clrT(restartDeadline); restartDeadline = null; }
    }

    function fail(reason) { clear(); onFallback(reason); }

    // true = restart пішов (або вже йде).
    function restart() {
        if (restartDeadline !== null) return true;
        let started = false;
        try { started = onRestart() === true; } catch (e) { started = false; }
        if (!started) return false;
        if (timer !== null) { clrT(timer); timer = null; }
        restartDeadline = setT(() => { restartDeadline = null; fail('ice-restart-timeout'); }, restartTimeoutMs);
        return true;
    }

    return {
        note(state) {
            if (state === 'closed') { fail('pc-closed'); return; }
            if (state === 'failed') {
                if (restartTimer !== null) { clrT(restartTimer); restartTimer = null; }
                if (!restart()) fail('pc-failed');
                return;
            }
            if (state === 'disconnected') {
                if (timer !== null || restartDeadline !== null) return; // уже чекаємо — не перезапускаємо
                timer = setT(() => { timer = null; fail('pc-disconnected-grace'); }, graceMs);
                restartTimer = setT(() => { restartTimer = null; restart(); }, restartMs);
                return;
            }
            if (state === 'connected' || state === 'completed') { clear(); }
        },
        restartFailed(reason) { if (restartDeadline !== null) fail(reason || 'ice-restart-failed'); },
        cancel: clear,
        pending: () => timer !== null || restartTimer !== null || restartDeadline !== null,
    };
}

/**
 * waitIceGathering — хаб не trickle, тож кандидати мусять бути в SDP: чекаємо
 * 'complete' або ICE_GATHER_DEADLINE_MS. Слухач і таймер знімаються, щойно
 * чекання скінчилось (№13). skipIfComplete:false — для ICE restart: чекання
 * ставимо ДО setLocalDescription, і там 'complete' ще від старого збирання.
 */
export function waitIceGathering(peer, opts) {
    const o = opts || {};
    if (o.skipIfComplete !== false && peer.iceGatheringState === 'complete') return Promise.resolve();
    return new Promise((resolve) => {
        let t = null;
        const onGather = () => { if (peer.iceGatheringState === 'complete') done(); };
        const done = () => { clearTimeout(t); peer.removeEventListener('icegatheringstatechange', onGather); resolve(); };
        peer.addEventListener('icegatheringstatechange', onGather);
        t = setTimeout(done, ICE_GATHER_DEADLINE_MS);
    });
}

/** C3: `<signal_url>/restart` — /offer/viewer → /offer/viewer/restart. */
export function restartUrlFrom(signalUrl) {
    try {
        const u = new URL(signalUrl);
        u.pathname = u.pathname.replace(/\/+$/, '') + '/restart';
        return u.toString();
    } catch (e) {
        return null;
    }
}

/**
 * adaptiveJitterTargetMs — звіт 01 №14: нульовий буфер на чистому каналі (курсор
 * не їде за рукою), ~60 мс коли втрати помітні (≥1%): там нуль дає ривки.
 * baseMs — нижня межа з config.playoutDelaySeconds.
 */
export function adaptiveJitterTargetMs(lossPct, baseMs) {
    const base = baseMs > 0 ? baseMs : 0;
    return lossPct >= LOSSY_LOSS_PCT ? Math.max(base, LOSSY_JITTER_TARGET_MS) : base;
}

// P-2: скільки ПОСПІЛЬ чистих семплів (крок startQualityMonitor, 5 с) треба,
// щоб зняти буфер втрат назад до бази. Вгору — одразу.
const JITTER_CLEAN_HOLD_SAMPLES = 3;

/**
 * createJitterTargetController — PLAYER-QUALITY P-2: adaptiveJitterTargetMs з
 * гістерезисом. Раніше ціль перераховувалась з ОДНОГО 5-секундного семпла:
 * втрати 1.2 % → 0.8 % → 1.1 % давали 60 → 0 → 60 мс кожні 5 с, а кожна
 * зміна мінімальної затримки змушує jitter-буфер Chrome перерахувати час
 * показу (видно як прискорення/пригальмовування картинки). Тепер угору —
 * на першому ж семплі з втратами (ривки від NACK на нулі гірші за +60 мс),
 * униз — лише після holdSamples чистих поспіль.
 *
 * @returns {{next: (lossPct:number) => number, current: () => number}}
 */
export function createJitterTargetController(o) {
    const opts = o || {};
    const base = opts.baseMs > 0 ? opts.baseMs : 0;
    const hold = opts.holdSamples > 0 ? opts.holdSamples : JITTER_CLEAN_HOLD_SAMPLES;
    let target = base;
    let clean = 0;
    return {
        next(lossPct) {
            const want = adaptiveJitterTargetMs(lossPct, base);
            if (want >= target) { target = want; clean = 0; return target; }
            clean += 1;
            if (clean >= hold) { target = want; clean = 0; }
            return target;
        },
        current: () => target,
    };
}

/**
 * createHiddenFrameKeepalive — §MAJOR-7: поки вкладка hidden, frame-age watchdog
 * тротлиться (rVFC не викликається у фоні) і завалив би живу сесію. Поки hidden
 * — крокаємо keepFresh() (=session.keepAlive: сторожі не старіють, але сесія НЕ
 * стає live без справжнього кадру); на поверненні у видимість ще visibleGraceMs
 * підживлюємо, щоб агент встиг прокинутись і дати IDR.
 *
 * @returns {{onVisibility: Function, active: Function, stop: Function}}
 */
export function createHiddenFrameKeepalive(o) {
    const opts = o || {};
    const isHidden = opts.isHidden || (() => false);
    const keepFresh = opts.keepFresh || (() => {});
    const setI = opts.setInterval || setInterval;
    const clrI = opts.clearInterval || clearInterval;
    const intervalMs = opts.intervalMs || DEFAULT_HIDDEN_KEEPALIVE_MS;
    let timer = null;

    function stop() { if (timer !== null) { clrI(timer); timer = null; } }

    function onVisibility() {
        if (isHidden()) {
            if (timer === null) { keepFresh(); timer = setI(() => keepFresh(), intervalMs); }
        } else {
            // Повернулись у видимість: ще visibleGraceMs підживлюємо frame-age,
            // поки агент прокидається і шле IDR, — далі рахують справжні кадри.
            stop();
            keepFresh();
            timer = setI(() => keepFresh(), intervalMs);
            const t = timer;
            (opts.setTimeout || setTimeout)(() => { if (timer === t && !isHidden()) stop(); }, opts.visibleGraceMs || DEFAULT_VISIBLE_GRACE_MS);
        }
    }

    return { onVisibility, active: () => timer !== null, stop };
}

// ─────────────────────────────────────────────────────────────────────────────
// ЧИСТА ЛОГІКА ПЕРЕМИКАЧА (без DOM, без RTC) — гейт
// tests/js/oo-mode-controller.test.mjs.
// ─────────────────────────────────────────────────────────────────────────────

export const MODE_MESH = 'mesh';
export const MODE_OO = 'oo';
export const MODE_AUTO = 'auto';

export const MODES = [MODE_MESH, MODE_OO, MODE_AUTO];

export const OO_LOST_BANNER = 'OO втрачено, перемкнено на Mesh';

export function normalizeMode(mode, fallback) {
    return MODES.indexOf(mode) >= 0 ? mode : (fallback || MODE_MESH);
}

/**
 * createModeController — ЧИСТА state-машина перемикача «Mesh / OO / Авто».
 *
 * Різниця між OO і Авто — НЕ в транспорті, а рівно в одному: чи чесно
 * сказати людині, що OO помер. У режимі «OO» власник порівнює наживо, і
 * мовчазний фолбек зробив би порівняння брехливим («дивись, OO працює» — а
 * то вже Mesh). У «Авто» фолбек мовчазний за задумом: це робочий режим.
 *
 * @param {string}   o.initialMode
 * @param {Function} o.startOo()          підняти OO-шар (DOM-сторона)
 * @param {Function} o.stopOo(reason)     знищити OO-шар
 * @param {Function} o.resumeMesh()       зняти Mesh з паузи (ідемпотентно)
 * @param {Function} o.showBanner(text)   чесний банер (лише режим OO)
 * @param {Function} o.hideBanner()
 * @param {Function} o.log(message)       журнал консолі
 * @param {Function} o.persist(mode)      збереження вибору (sessionStorage)
 */
export function createModeController(o) {
    const opts = o || {};
    const startOo = opts.startOo || (() => {});
    const stopOo = opts.stopOo || (() => {});
    const resumeMesh = opts.resumeMesh || (() => {});
    const showBanner = opts.showBanner || (() => {});
    const hideBanner = opts.hideBanner || (() => {});
    const log = opts.log || (() => {});
    const persist = opts.persist || (() => {});
    const onRender = opts.onRender || (() => {});

    // Повернення в OO після фолбеку. Причин втратити кадри більше, ніж здається:
    // екран UAC (DXGI його не бачить), коротка мережева яма, зміна адаптера — і
    // досі одна така секунда коштувала весь сеанс. Працює ЛИШЕ в «Авто»: у
    // режимі OO людина обрала його свідомо і має бачити чесний провал.
    const retry = createOoRetry({
        onRetry: () => { if (!destroyed) applyOo(); },
        setTimeout: opts.setTimeout,
        clearTimeout: opts.clearTimeout,
        now: opts.now,
        delayMs: opts.retryDelayMs,
        stableMs: opts.retryStableMs,
        maxDelayMs: opts.retryMaxDelayMs,
    });

    // Фолбек саме на Mesh: невідоме значення (сміття в sessionStorage, друкарська
    // помилка в config) НЕ має мовчки вмикати експериментальний транспорт.
    let mode = normalizeMode(opts.initialMode, MODE_MESH);
    // ooWanted — чи МАЄ зараз існувати OO-шар. Відрізняється від mode саме
    // після фолбеку в режимі «Авто»: вибір лишається 'auto' (кнопка
    // підсвічена), але шару вже нема — інакше повторний клік по «Авто» не
    // піднімав би сесію заново.
    let ooWanted = false;
    let destroyed = false;

    function render() {
        try { onRender(mode, ooWanted); } catch (e) { /* журнал не блокер */ }
    }

    function applyMesh(reason) {
        if (ooWanted) { ooWanted = false; try { stopOo(reason); } catch (e) { /* ignore */ } }
        try { resumeMesh(); } catch (e) { /* ignore */ }
    }

    function applyOo() {
        if (ooWanted) return;
        ooWanted = true;
        try { startOo(); } catch (e) { /* ignore */ }
    }

    const api = {
        mode: () => mode,
        ooWanted: () => ooWanted,

        /** setMode — вибір людини АБО автоперемикання (silent:true). */
        setMode(next, o2) {
            if (destroyed) return mode;
            const opt = o2 || {};
            const want = normalizeMode(next, mode);
            const changed = want !== mode;
            mode = want;
            // Людина щойно вибрала режим руками — стара черга повернення більше
            // не її справа: вона підняла б OO поверх свіжого вибору.
            retry.cancel();
            if (opt.persist !== false) persist(mode);
            if (!opt.silent && changed) log(`Режим екрана: ${labelFor(mode)}`);
            if (mode === MODE_MESH) {
                hideBanner();
                applyMesh('mode-mesh');
            } else {
                hideBanner();
                applyOo();
            }
            render();
            return mode;
        },

        /** start — початкове застосування вибраного режиму (після відкриття вкладки). */
        start() {
            if (destroyed) return mode;
            if (mode === MODE_MESH) applyMesh('start-mesh');
            else applyOo();
            render();
            return mode;
        },

        /**
         * noteOoState — стан OO-шару з onStateChange.
         * fallback/closed при ooWanted = OO помер.
         */
        noteOoState(state, reason) {
            if (destroyed) return mode;
            log(`OO: ${state}${reason ? ' (' + reason + ')' : ''}`);
            if (state !== OO_STATE_FALLBACK && state !== OO_STATE_CLOSED) {
                // Звідси міряється «сесія прожила довго»: після довгої живої
                // сесії лічильник спроб скидається, інакше дві невдачі за весь
                // робочий день замкнули б повернення назавжди.
                if (state === OO_STATE_LIVE) retry.noteLive();
                render();
                return mode;
            }
            if (!ooWanted) { render(); return mode; }
            // Шар уже сам повернув Mesh з паузи (атомарний doFallback у
            // createOoWebrtcLayer), але resumeMesh ідемпотентний — зайвий виклик
            // дешевший за застиглий екран, якщо шар помер ДО unpause.
            ooWanted = false;
            try { resumeMesh(); } catch (e) { /* ignore */ }
            if (mode === MODE_OO) {
                // ЧЕСНО: власник порівнює наживо, він мусить знати, що дивиться вже Mesh.
                showBanner(OO_LOST_BANNER);
                log(OO_LOST_BANNER + (reason ? ' — ' + reason : ''));
                mode = MODE_MESH;
                persist(mode);
            }
            // MODE_AUTO — мовчки: ні банера, ні зміни підсвітки. Але саме тут
            // ставимо в чергу повернення: mode на цей момент уже MODE_MESH,
            // якщо людина була в режимі OO, тож retry сам промовчить.
            retry.noteFallback(mode);
            render();
            return mode;
        },

        destroy() {
            if (destroyed) return;
            destroyed = true;
            retry.cancel();
            if (ooWanted) { ooWanted = false; try { stopOo('destroy'); } catch (e) { /* ignore */ } }
        },
    };
    return api;
}

// Чиста математика живого моніторингу якості: із двох послідовних getStats()
// рахує fps, втрати і RTT та каже, чи сесія погана. Без DOM; гейт — через
// сам шар: tests/js/oo-webrtc-live-wiring.test.mjs і oo-webrtc-resilience.test.mjs.
//
// 🔴 Відновлено 29.08.2026 з ПРОДового бандла desktop-oo-webrtc-CRqTtW6Y.js.
// Коміт 550222ee (27.08) обіцяв цю функцію в повідомленні, але сам файл у нього
// не потрапив: залетіли лише тест і індикатор. Тому JS-гейт стояв червоним із
// 27.08, а єдина копія коду жила в мініфікованому бандлі на проді.
export function evaluateQualitySample(prev, inbound, candidatePair, limits) {
    const ts = inbound.timestamp || 0;
    let fps = typeof inbound.framesPerSecond === 'number' ? inbound.framesPerSecond : null;
    let lossPct = 0;
    let framesDelta = null;
    let bytesDelta = null;

    let freezes = 0;
    let framesDropped = 0;
    let jitterBufferMs = null;
    let bitrateKbps = null;

    if (prev) {
        const dtSec = Math.max(0.001, (ts - prev.ts) / 1000);
        framesDelta = (inbound.framesDecoded || 0) - prev.framesDecoded;
        bytesDelta = (inbound.bytesReceived || 0) - (prev.bytesReceived || 0);
        // F-14: freezeCount — ЄДИНИЙ показник у getStats, що прямо каже «людина
        // побачила ривок». fps/loss/rtt його не замінюють: короткий стоп на
        // 300 мс не зсуне середній fps за 5с і не дасть жодної втрати, а очима
        // видно саме його. framesDropped — те саме з боку декодера.
        freezes = Math.max(0, (inbound.freezeCount || 0) - (prev.freezeCount || 0));
        framesDropped = Math.max(0, (inbound.framesDropped || 0) - (prev.framesDropped || 0));
        // jitterBufferDelay — СУМА секунд очікування по всіх виданих кадрах;
        // сама по собі росте завжди, тож ділимо на приріст лічильника кадрів і
        // дістаємо середню затримку буфера НА КАДР за цей інтервал (мс).
        const jbDelta = (inbound.jitterBufferDelay || 0) - (prev.jitterBufferDelay || 0);
        const jbFrames = (inbound.jitterBufferEmittedCount || 0) - (prev.jitterBufferEmittedCount || 0);
        if (jbDelta > 0 && jbFrames > 0) jitterBufferMs = Math.round((jbDelta / jbFrames) * 1000);
        // C4: бітрейт з дельти байтів; скинутий лічильник — не від'ємний бітрейт.
        bitrateKbps = bytesDelta >= 0 ? Math.round((bytesDelta * 8) / dtSec / 1000) : null;
        if (fps == null) {
            fps = framesDelta >= 0 ? framesDelta / dtSec : null;
        }
        const lost = (inbound.packetsLost || 0) - prev.packetsLost;
        const received = (inbound.packetsReceived || 0) - prev.packetsReceived;
        const total = lost + received;
        // Лічильник міг зменшитись (рестарт статистики) — від'ємних втрат не буває.
        lossPct = total > 0 ? Math.max(0, lost) / total : 0;
    }

    const rttMs = candidatePair && typeof candidatePair.currentRoundTripTime === 'number'
        ? Math.round(candidatePair.currentRoundTripTime * 1000)
        : null;

    // F-31/Q-01: НЕРУХОМИЙ ЕКРАН — НЕ ДЕГРАДАЦІЯ.
    //
    // Агент кодує лише те, що змінилось: людина читає текст — і fps чесно
    // падає до 2-5. Старий сторож бачив «fps 5 < 8» і оголошував провал:
    // 85% усіх семплів прода приїжджали з bad=true при RTT 19 мс і втратах
    // 0.01%, а «Авто» після 30 с такого спокою мовчки зносило OO на Mesh —
    // рівно тоді, коли все працювало ідеально.
    //
    // Відрізняємо двоє:
    //   idle    — НОВІ кадри є (framesDecoded росте) і транспорт здоровий:
    //             картинка просто не змінюється. Низький fps не карається.
    //   stalled — за інтервал НІ кадру, НІ байта: потік справді завмер.
    //             Це гірше за низький fps, тож карається завжди.
    // F-14: freezeCount входить і в «здоровий транспорт», і в bad — ривок,
    // якого людина не могла не помітити, не сміє рахуватись за спокій.
    const maxFreezes = limits.maxFreezes != null ? limits.maxFreezes : 0;
    const transportOk = lossPct <= limits.maxLoss
        && (rttMs == null || rttMs <= limits.maxRttMs)
        && freezes <= maxFreezes;
    const stalled = prev != null && framesDelta <= 0 && bytesDelta <= 0;
    const idle = prev != null && !stalled && framesDelta > 0 && transportOk;
    const fpsBad = !idle && fps != null && fps < limits.minFps;
    const bad = stalled
        || fpsBad
        || freezes > maxFreezes
        || (rttMs != null && rttMs > limits.maxRttMs)
        || lossPct > limits.maxLoss;

    return {
        fps,
        lossPct,
        rttMs,
        freezes,
        framesDropped,
        jitterBufferMs,
        // C4: телеметрія — jitter мережі (не буфера), роздільність потоку.
        bitrateKbps,
        jitterMs: typeof inbound.jitter === 'number' ? Math.round(inbound.jitter * 1000) : null,
        width: typeof inbound.frameWidth === 'number' ? inbound.frameWidth : null,
        height: typeof inbound.frameHeight === 'number' ? inbound.frameHeight : null,
        idle,
        stalled,
        bad,
        nextPrev: {
            ts,
            framesDecoded: inbound.framesDecoded || 0,
            bytesReceived: inbound.bytesReceived || 0,
            packetsLost: inbound.packetsLost || 0,
            packetsReceived: inbound.packetsReceived || 0,
            // F-14: без цих трьох кожен інтервал рахував би дельту від нуля —
            // тобто «щойно був ривок» на кожному тіку.
            freezeCount: inbound.freezeCount || 0,
            framesDropped: inbound.framesDropped || 0,
            jitterBufferDelay: inbound.jitterBufferDelay || 0,
            jitterBufferEmittedCount: inbound.jitterBufferEmittedCount || 0,
        },
    };
}

export function labelFor(mode) {
    if (mode === MODE_MESH) return 'Mesh';
    if (mode === MODE_OO) return 'OO';
    return 'Авто';
}

// ─────────────────────────────────────────────────────────────────────────────
// N5: ICE-конфіг для суворих офісних firewall-ів (лише TCP 443)
// ─────────────────────────────────────────────────────────────────────────────
//
// Хаб сам є медіа-кінцем, тож головний запасний шлях — ICE-TCP на 443 хаба
// (OO_SCREEN_ICE_TCP_ADVERTISE_PORT): для нього браузеру НЕ потрібен жоден
// iceServer, кандидат приходить в answer-і. iceServers із config/ERP
// потрібні для другого рубежу — TURN-TLS (turns:host:443?transport=tcp) там,
// де DPI пропускає на 443 лише справжній TLS.
//
// UDP завжди має перевагу без жодної логіки з нашого боку: ICE дає host/UDP
// найвищий пріоритет, TCP-host нижчий, relay — найнижчий, і пари
// перевіряються паралельно. Тому iceTransportPolicy лишається 'all' —
// 'relay' вимкнув би UDP і в офісі, де він є. Тут лише чистимо вхід:
// невалідний запис (TURN без облікових даних, чужа схема) з ERP не має
// ламати конструктор RTCPeerConnection — той кидає на ВСЬОМУ списку.

const ICE_URL_RE = /^(stun|stuns|turn|turns):[^\s]+$/i;

// opusStereoSdp — F1 (Opus 48 кГц стерео). Chrome за замовчуванням оголошує
// Opus без stereo=1, а декодер приймача створюється з кількістю каналів із
// цього fmtp — тобто без правки стерео від хаба зводиться в моно. Дописуємо
// stereo=1;sprop-stereo=1 у fmtp кожного opus-PT нашого ж offer-а. Інші
// рядки не чіпаємо; повторний виклик нічого не змінює.
export function opusStereoSdp(sdp) {
    if (typeof sdp !== 'string') return sdp;
    const pts = [];
    const re = /^a=rtpmap:(\d+) opus\/48000/gim;
    let m;
    while ((m = re.exec(sdp)) !== null) pts.push(m[1]);
    if (!pts.length) return sdp;
    return sdp.split(/\r\n/).map((line) => {
        const f = /^a=fmtp:(\d+) (.*)$/.exec(line);
        if (!f || !pts.includes(f[1])) return line;
        let params = f[2];
        if (!/(^|;)\s*stereo=/.test(params)) params += ';stereo=1';
        if (!/(^|;)\s*sprop-stereo=/.test(params)) params += ';sprop-stereo=1';
        return 'a=fmtp:' + f[1] + ' ' + params;
    }).join('\r\n');
}

// iceUrlRank — порядок у списку: UDP-варіанти першими, turns (TLS) останнім.
// На пріоритет кандидатів це не впливає (його рахує ICE), але Chrome опитує
// сервери по черзі, і так дешеві запити йдуть раніше.
function iceUrlRank(u) {
    const s = u.toLowerCase();
    if (s.startsWith('stun')) return 0;
    if (s.startsWith('turns:')) return 3;
    if (s.includes('transport=tcp')) return 2;
    return 1;
}

/**
 * normalizeIceServers — RTCIceServer[] з config/ERP, придатний для
 * RTCPeerConnection: лише stun/stuns/turn/turns-URL, TURN — лише з
 * username+credential, сортування UDP → TCP → TLS. Не масив → [].
 */
export function normalizeIceServers(list) {
    if (!Array.isArray(list)) return [];
    const out = [];
    for (const s of list) {
        if (!s || typeof s !== 'object') continue;
        const urls = (Array.isArray(s.urls) ? s.urls : [s.urls])
            .filter((u) => typeof u === 'string' && ICE_URL_RE.test(u.trim()))
            .map((u) => u.trim());
        const turn = urls.filter((u) => /^turns?:/i.test(u));
        const stun = urls.filter((u) => /^stuns?:/i.test(u));
        const hasCreds = typeof s.username === 'string' && s.username !== ''
            && typeof s.credential === 'string' && s.credential !== '';
        const keep = hasCreds ? stun.concat(turn) : stun;
        if (!keep.length) continue;
        keep.sort((a, b) => iceUrlRank(a) - iceUrlRank(b));
        const e = { urls: keep };
        if (hasCreds && turn.length) { e.username = s.username; e.credential = s.credential; }
        out.push(e);
    }
    out.sort((a, b) => iceUrlRank(a.urls[0]) - iceUrlRank(b.urls[0]));
    return out;
}

/**
 * buildRtcConfig — RTCConfiguration глядача. config.iceTransportPolicy
 * 'relay' — лише для діагностики («чи живий TURN»); будь-що інше = 'all'.
 */
export function buildRtcConfig(config) {
    const c = config || {};
    return {
        iceServers: normalizeIceServers(c.iceServers),
        iceTransportPolicy: c.iceTransportPolicy === 'relay' ? 'relay' : 'all',
        bundlePolicy: 'max-bundle',
    };
}

// ─────────────────────────────────────────────────────────────────────────────
// DOM-шар: overlay <video> + RTCPeerConnection recvonly
// ─────────────────────────────────────────────────────────────────────────────

/**
 * createOoWebrtcLayer — публічна точка входу OO-шару (її імпортує control-oo-screen.js).
 *
 * @param {Element}  o.container
 * @param {object}   o.meshDesktop
 * @param {object}   o.config  { requestTicket, firstFrameMs?, frameAgeMs?,
 *                               geometryTimeoutMs?, offerTimeoutMs?, iceServers?,
 *                               iceTransportPolicy?,
 *                               disconnectGraceMs?, inputChannel?, inputGrant?,
 *                               inputAllowed? }
 *   inputChannel (F5, типово false) — власний канал вводу oosc-input замість
 *   Mesh, лише для ролі control; inputAllowed() — гачок політики згоди.
 *   requestTicket() → Promise<{ticket, signalUrl, grant?, standbySignalUrls?}> — §6.4 свіжий одноразовий
 *   ticket на цю ноду; offer їде з ticket, НЕ з довгоживучим токеном (BLOCKER-1/3).
 * @param {Function} o.onStateChange(state, reason)
 * @returns {{destroy: Function, state: Function, generation: Function}}
 */
// ── режим показу (TZ 1.1 / P6) ─────────────────────────────────────────────
// 'fit' — підігнати під бокс Mesh-canvas, ЗБЕРІГАЮЧИ пропорції (contain).
// '1:1' — один піксель відео = один фізичний піксель екрана:
//         CSS-розмір = videoWidth/dpr × videoHeight/dpr, контейнер скролиться.
// Раніше було object-fit:fill + CSS-розмір з canvas — браузер масштабував
// кадр вдруге (і без урахування dpr), звідси мило й криві пропорції.
export const DISPLAY_FIT = 'fit';
export const DISPLAY_1X1 = '1:1';
export const DISPLAY_STORAGE_KEY = 'oo-screen-display-mode';

export function normalizeDisplayMode(m) {
    return m === DISPLAY_1X1 ? DISPLAY_1X1 : DISPLAY_FIT;
}

export function loadDisplayMode(storage) {
    try {
        const st = storage !== undefined ? storage : (typeof localStorage !== 'undefined' ? localStorage : null);
        return normalizeDisplayMode(st ? st.getItem(DISPLAY_STORAGE_KEY) : null);
    } catch (e) { return DISPLAY_FIT; }
}

export function saveDisplayMode(mode, storage) {
    try {
        const st = storage !== undefined ? storage : (typeof localStorage !== 'undefined' ? localStorage : null);
        if (st) st.setItem(DISPLAY_STORAGE_KEY, normalizeDisplayMode(mode));
    } catch (e) { /* приватне вікно / заблоковане сховище — живемо без пам'яті */ }
}

// Бокс, у який object-fit:contain реально кладе кадр усередині елемента
// boxW×boxH (з лєтербоксом). Координати — відносно лівого верхнього кута боксу.
export function containBox(boxW, boxH, srcW, srcH) {
    if (!(boxW > 0) || !(boxH > 0) || !(srcW > 0) || !(srcH > 0)) {
        return { x: 0, y: 0, width: Math.max(0, boxW || 0), height: Math.max(0, boxH || 0) };
    }
    const scale = Math.min(boxW / srcW, boxH / srcH);
    const width = srcW * scale;
    const height = srcH * scale;
    return { x: (boxW - width) / 2, y: (boxH - height) / 2, width, height };
}

// CSS-розмір для режиму 1:1. integer=true — dpr цілий (1, 2, 3). Увага
// (PLAYER-QUALITY P-3): у 1:1 піксель відео = ОДИН фізичний піксель за будь-
// якого dpr (CSS = videoW/dpr), тож N×N тут не буває; image-rendering тепер
// обирає imageRenderingFor за фактичним масштабом, integer лишився для відкату.
// absorbForeignStyles — 1:1 тримає знімок стилів Mesh, щоб повернути їх при
// виході. Якщо поки діє 1:1 Mesh сам змінив якийсь стиль (значення вже не те,
// що записали ми), це нова «оригінальна» величина — переносимо її в знімок,
// інакше restore відкотив би Mesh до стану на момент входу в 1:1.
// saved — знімок; written — що ми записали востаннє (null = ще нічого);
// current — що є зараз. Мутує й повертає saved.
export function absorbForeignStyles(saved, written, current) {
    if (!saved || !written) return saved;
    for (const k of Object.keys(saved)) {
        if (k in written && current[k] !== written[k]) saved[k] = current[k];
    }
    return saved;
}

export function oneToOneSize(videoW, videoH, dpr) {
    const d = dpr > 0 ? dpr : 1;
    return {
        width: videoW / d,
        height: videoH / d,
        integer: Math.abs(d - Math.round(d)) < 1e-6,
    };
}

// ── P-3: HiDPI — прив'язка до сітки фізичних пікселів ─────────────────────
// Навіть коли CSS-розмір відео дає рівно videoWidth фізичних пікселів (1:1 на
// dpr 1.25: 1536 css = 1920 px), ДРОБОВИЙ зсув (left 10.4px → 13 px на dpr
// 1.25, або Mesh-canvas, відцентрований margin:auto) змушує композитор
// семплювати кадр зі зсувом на пів пікселя — білінійне «мило» на всьому
// тексті. Тому краї боксу кладемо на цілі фізичні пікселі.
// Допуск «майже 1:1»: бокс Mesh-canvas, що на ≤1.5 фізичного пікселя
// відрізняється від розміру кадру (округлення layout, 1535.6 css замість 1536),
// стає РІВНО videoW×videoH: інакше масштаб 0.9998 ресемплить увесь кадр
// заради пів пікселя.
// ПРАВИЙ/НИЖНІЙ край ніколи не виходить за край Mesh-canvas (floor): вихід
// навіть на долю пікселя за контейнер з overflow:auto дає смугу прокрутки,
// та міняє розмір контейнера → ResizeObserver → syncGeometry → петля. Тож
// «майже 1:1» добирає пікселі ВЛІВО/ВГОРУ (від'ємний overflow не прокручується),
// зсув відносно Mesh-canvas ≤ допуск + 1 фізичний піксель.
export const NEAR_ONE_TO_ONE_DEVICE_PX = 1.5;
const SNAP_EPS = 1e-3; // LayoutUnit Chrome = 1/64 px: 1365.9999 — це 1366

/**
 * snapToDeviceBox — бокс у координатах в'юпорту (CSS px) → бокс з краями на
 * цілих фізичних пікселях, не ширший за вихідний праворуч/знизу.
 * srcW×srcH — розмір кадру (0 = ще невідомо).
 * @returns {{x, y, width, height, deviceWidth, deviceHeight, exact}}
 *   exact — один піксель кадру = один фізичний піксель екрана.
 */
export function snapToDeviceBox(x, y, w, h, dpr, srcW, srcH, tolPx) {
    const d = dpr > 0 ? dpr : 1;
    const tol = typeof tolPx === 'number' ? tolPx : NEAR_ONE_TO_ONE_DEVICE_PX;
    const bx = (x || 0) * d;
    const by = (y || 0) * d;
    const bw = Math.max(0, w || 0) * d;
    const bh = Math.max(0, h || 0) * d;
    const x1 = Math.floor(bx + bw + SNAP_EPS);
    const y1 = Math.floor(by + bh + SNAP_EPS);
    let x0 = Math.ceil(bx - SNAP_EPS);
    let y0 = Math.ceil(by - SNAP_EPS);
    if (srcW > 0 && srcH > 0 && Math.abs(bw - srcW) <= tol && Math.abs(bh - srcH) <= tol) {
        x0 = x1 - srcW;
        y0 = y1 - srcH;
    }
    const dw = Math.max(0, x1 - x0);
    const dh = Math.max(0, y1 - y0);
    if (!dw) x0 = x1;
    if (!dh) y0 = y1;
    return {
        x: x0 / d, y: y0 / d, width: dw / d, height: dh / d,
        deviceWidth: dw, deviceHeight: dh,
        exact: srcW > 0 && dw === srcW && dh === srcH,
    };
}

/** snapPx — одна координата в'юпорту (CSS px) на найближчий фізичний піксель. */
export function snapPx(v, dpr) {
    const d = dpr > 0 ? dpr : 1;
    return Math.round(v * d) / d;
}

/**
 * imageRenderingFor — P-3: 'pixelated' ЛИШЕ коли кадр збільшено в ЦІЛЕ число
 * разів ≥2 по обох осях (1:1 на dpr 2, кадр 1280×720 у боксі 2560×1440): тоді
 * кожен піксель кадру — рівний квадрат N×N, текст різкий, як на моніторі з
 * меншим DPI. Дробове збільшення з pixelated дає «драбину» (стовпці різної
 * ширини), зменшення — алiасинг, тож там 'auto' (білінійно).
 */
export function imageRenderingFor(deviceW, deviceH, srcW, srcH) {
    if (!(srcW > 0) || !(srcH > 0) || !(deviceW > 0) || !(deviceH > 0)) return 'auto';
    const sx = deviceW / srcW;
    const sy = deviceH / srcH;
    const k = Math.round(sx);
    if (k >= 2 && Math.abs(sx - k) < 1e-6 && Math.abs(sy - k) < 1e-6) return 'pixelated';
    return 'auto';
}

// clientX/clientY → піксель віддаленого екрана. rect — getBoundingClientRect()
// елемента з картинкою; srcW×srcH — розмір віддаленого кадру. Враховує
// лєтербокс від contain. null — клік у чорну смугу (нікуди слати).
export function mapClientToRemote(clientX, clientY, rect, srcW, srcH) {
    if (!rect || !(srcW > 0) || !(srcH > 0)) return null;
    const b = containBox(rect.width, rect.height, srcW, srcH);
    if (!(b.width > 0) || !(b.height > 0)) return null;
    const lx = clientX - rect.left - b.x;
    const ly = clientY - rect.top - b.y;
    if (lx < 0 || ly < 0 || lx > b.width || ly > b.height) return null;
    const x = Math.min(srcW - 1, Math.floor(lx * srcW / b.width));
    const y = Math.min(srcH - 1, Math.floor(ly * srcH / b.height));
    return { x, y };
}

// Зворотний мапінг для шару курсора: піксель віддаленого кадру (x,y у
// srcW×srcH) → точка відносно лівого верхнього кута rect (той самий contain-
// бокс, що й mapClientToRemote). scale — CSS-пікселів на піксель кадру
// (оверлей курсора масштабується разом із картинкою). null — нема геометрії.
export function mapRemoteToClient(x, y, rect, srcW, srcH) {
    if (!rect || !(srcW > 0) || !(srcH > 0)) return null;
    const b = containBox(rect.width, rect.height, srcW, srcH);
    if (!(b.width > 0) || !(b.height > 0)) return null;
    const scale = b.width / srcW;
    return { x: b.x + x * scale, y: b.y + y * (b.height / srcH), scale };
}

/** offerBody — тіло /offer/viewer. monitor лише ціле >0 (F6, config.monitor:
 *  потік "<node>#m<i>" на хабі), інакше поля немає — offer як до F6. */
export function offerBody(sdp, ticket, monitor) {
    if (Number.isInteger(monitor) && monitor > 0) return JSON.stringify({ sdp: sdp, ticket, monitor });
    return JSON.stringify({ sdp: sdp, ticket });
}

/**
 * O2: резервний хаб для глядача. Типово ВИМКНЕНО: без config.standbySignalUrls
 * повертає [signalUrl] — рівно одна спроба, як до O2.
 * standbySignalUrls — масив повних URL /offer/viewer резервних хабів (ті самі
 * ticket-секрети, див. tools/oo-screen/deploy/DEPLOY.md «O2»).
 */
export function signalCandidates(signalUrl, standby) {
    const out = [signalUrl];
    if (Array.isArray(standby)) {
        for (const u of standby) {
            if (typeof u === 'string' && u && !out.includes(u)) out.push(u);
        }
    }
    return out;
}

/**
 * Чи пробувати наступний хаб.
 *  - помилка fetch: так, ЯКЩО це не teardown (tornDown). Таймаут спроби
 *    (AbortError у фолбеку / TimeoutError у AbortSignal.timeout) — так:
 *    blackhole/завислий хаб = найчастіший «мертвий хост».
 *  - 502/503/504: проксі перед мертвим хабом.
 *  - 404 «no publisher for node»: агент сидить на ІНШОМУ хабі (після failover
 *    агент на основний сам не повертається — інакше split-brain).
 *  - 400/401/403 (ticket, ACL) — НІ: резервний відповість так само.
 * Наступна спроба завжди йде зі СВІЖИМ ticket-ом (postOfferWithFailover).
 */
/**
 * O2: звідки брати резервні URL глядача. Пріоритет — відповідь requestTicket()
 * (поле standbySignalUrls поруч із signalUrl: той самий серверний канал, що вже
 * несе signalUrl, тож ERP вмикає резерв без зміни коду виклику шару); інакше —
 * config.standbySignalUrls. Нічого нема → undefined (типово OFF, одна спроба).
 */
export function resolveStandby(ticketResp, config) {
    if (ticketResp && Array.isArray(ticketResp.standbySignalUrls)) return ticketResp.standbySignalUrls;
    if (config && Array.isArray(config.standbySignalUrls)) return config.standbySignalUrls;
    return undefined;
}

/**
 * N6: звідки брати прапор прямої ноги. Як і resolveStandby: пріоритет —
 * відповідь requestTicket() (поля p2p / p2pUrl поруч із signalUrl; ERP ставить
 * їх із env OO_SCREEN_P2P / OO_SCREEN_P2P_URL), інакше — config.p2p / config.p2pUrl.
 * Лише строге true вмикає; нічого нема → { p2p: false } (типово OFF).
 */
export function resolveP2P(ticketResp, config) {
    const src = (ticketResp && typeof ticketResp.p2p === 'boolean') ? ticketResp : (config || {});
    const p2pUrl = (ticketResp && typeof ticketResp.p2pUrl === 'string' && ticketResp.p2pUrl)
        || (config && typeof config.p2pUrl === 'string' && config.p2pUrl) || undefined;
    return { p2p: src.p2p === true, p2pUrl };
}

export function shouldFailover(err, status, tornDown) {
    if (err) return !tornDown;
    return status === 404 || status === 502 || status === 503 || status === 504;
}

/**
 * O2: POST offer по кандидатах. Кожна спроба має ВЛАСНИЙ таймаут (окремий
 * combineAbortSignals), тож таймаут основного не вбиває спробу резервного.
 * Кожна наступна спроба бере НОВИЙ одноразовий ticket через requestTicket():
 * ticket, що міг бути спожитий хабом, який відповів/обірвався, повторно не
 * надсилається (used-ticket облік у кожного хаба свій).
 *
 * o: { urls, ticket, requestTicket, makeBody(ticket), onTicket(ticket, grant),
 *      teardownSignal, timeoutMs, fetchFn, combine }
 * @returns {Promise<Response>}
 */
export async function postOfferWithFailover(o) {
    const combine = o.combine || combineAbortSignals;
    const torn = () => !!(o.teardownSignal && o.teardownSignal.aborted);
    let ticket = o.ticket;
    for (let i = 0; i < o.urls.length; i++) {
        const last = i === o.urls.length - 1;
        if (i > 0) {
            if (torn()) throw new Error('offer/viewer: teardown');
            const t = await o.requestTicket();
            if (!t || !t.ticket) throw new Error('offer/viewer: немає ticket для резервного хаба');
            ticket = t.ticket;
            if (o.onTicket) o.onTicket(t.ticket, t.grant);
        }
        const c = combine(o.teardownSignal, o.timeoutMs);
        let resp;
        try {
            resp = await o.fetchFn(o.urls[i], {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: o.makeBody(ticket),
                signal: c.signal,
            });
        } catch (e) {
            if (last || !shouldFailover(e, 0, torn())) throw e;
            continue;
        } finally {
            c.cancel();
        }
        if (last || !shouldFailover(null, resp.status)) return resp;
    }
    throw new Error('offer/viewer: немає кандидатів');
}

// ─────────────────────────────────────────────────────────────────────────────
// N6: пряма нога агент↔браузер. Типово ВИМКНЕНО: без config.p2p плеєр шле
// offer лише на /offer/viewer, як і раніше. З config.p2p — спершу /p2p/offer
// того самого хаба (хаб споживає ERP-квиток, агент відповідає напряму).
// 409 або ICE, що не зʼєднався, — relay /offer/viewer з одноразовим
// relay_ticket, який видав хаб (ERP-квиток уже спожито, повторно не шлемо).
// ─────────────────────────────────────────────────────────────────────────────

export const PATH_DIRECT = 'direct';
export const PATH_RELAY = 'relay';
const DEFAULT_P2P_CONNECT_MS = 8000;
const DEFAULT_RELAY_RETRIES = 6;
const DEFAULT_RELAY_RETRY_MS = 1500;

/** p2pOfferUrl — config.p2pUrl або …/offer/viewer → …/p2p/offer; null — не вивести. */
export function p2pOfferUrl(signalUrl, override) {
    if (typeof override === 'string' && override) return override;
    if (typeof signalUrl !== 'string') return null;
    const m = /^(.*)\/offer\/viewer\/?(\?.*)?$/.exec(signalUrl);
    return m ? m[1] + '/p2p/offer' : null;
}

// 🔴 F-53 (прод): тіло відповіді читаємо ПІД signal-ом (таймаут + teardown).
// postJson знімає свій signal одразу після fetch, і завислий resp.json()
// інакше не переривався б нічим.
async function readJsonBounded(resp, o) {
    const combine = o.combine || combineAbortSignals;
    const c = combine(o.teardownSignal, o.timeoutMs || DEFAULT_OFFER_TIMEOUT_MS);
    try {
        const sig = c.signal;
        if (!sig) return await resp.json();
        return await new Promise((resolve, reject) => {
            if (sig.aborted) { reject(new Error('offer/viewer: abort')); return; }
            const onAbort = () => reject(new Error('offer/viewer: body timeout/teardown'));
            sig.addEventListener('abort', onAbort, { once: true });
            Promise.resolve(resp.json()).then(
                (v) => { sig.removeEventListener('abort', onAbort); resolve(v); },
                (e) => { sig.removeEventListener('abort', onAbort); reject(e); },
            );
        });
    } finally {
        c.cancel();
    }
}

async function postJson(fetchFn, url, body, o) {
    const combine = o.combine || combineAbortSignals;
    const c = combine(o.teardownSignal, o.timeoutMs || DEFAULT_OFFER_TIMEOUT_MS);
    try {
        return await fetchFn(url, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body,
            signal: c.signal,
        });
    } finally {
        c.cancel();
    }
}

/**
 * postP2POffer — POST /p2p/offer. Результат:
 *   {kind:'direct', sdp, id, relayTicket}  — 200 (relayTicket дійсний лише після відкату)
 *   {kind:'relay', reason, relayTicket}    — 409 (relayTicket дійсний одразу)
 *   {kind:'relay', reason, ticket:'same'}  — 404/405: маршруту нема (P2P вимкнено
 *                                            на хабі) — ERP-квиток НЕ спожито
 *   {kind:'relay', reason, ticket:'fresh'} — інше: квиток міг згоріти, потрібен новий
 */
export async function postP2POffer(o) {
    let resp;
    try {
        resp = await postJson(o.fetchFn, o.url, JSON.stringify({ ticket: o.ticket, sdp: o.sdp }), o);
    } catch (e) {
        if (o.teardownSignal && o.teardownSignal.aborted) throw e;
        return { kind: PATH_RELAY, reason: 'p2p-network', ticket: 'fresh' };
    }
    if (resp.status === 200) {
        const a = await readJsonBounded(resp, o);
        if (a && a.sdp) return { kind: PATH_DIRECT, sdp: a.sdp, id: a.id, relayTicket: a.relay_ticket || '' };
        return { kind: PATH_RELAY, reason: 'p2p-empty-answer', ticket: 'fresh' };
    }
    if (resp.status === 409) {
        let a = null;
        try { a = await resp.json(); } catch (e) { /* ignore */ }
        if (a && a.relay_ticket) return { kind: PATH_RELAY, reason: a.reason || 'conflict', relayTicket: a.relay_ticket };
        return { kind: PATH_RELAY, reason: 'p2p-409-no-ticket', ticket: 'fresh' };
    }
    if (resp.status === 404 || resp.status === 405) return { kind: PATH_RELAY, reason: 'p2p-off', ticket: 'same' };
    return { kind: PATH_RELAY, reason: 'p2p-' + resp.status, ticket: 'fresh' };
}

/**
 * redeemRelay — /offer/viewer з relay_ticket. Після ICE-відкату квиток стає
 * дійсним, лише коли агент звітує fallback хабу (його ICE-сторож може
 * спрацювати трохи пізніше за браузерний), тож 403 повторюємо обмежено.
 * Будь-яка інша відповідь — кінець: квиток одноразовий, і хаб його вже спожив.
 */
export async function redeemRelay(o) {
    const retries = o.retries ?? DEFAULT_RELAY_RETRIES;
    const sleep = o.sleep || ((ms) => new Promise((r) => setTimeout(r, ms)));
    for (let i = 0; ; i++) {
        const resp = await postJson(o.fetchFn, o.url, o.makeBody(o.relayTicket), o);
        if (resp.status !== 403 || i >= retries) return resp;
        if (o.teardownSignal && o.teardownSignal.aborted) return resp;
        await sleep(o.retryMs ?? DEFAULT_RELAY_RETRY_MS);
    }
}

/** waitIceOutcome — 'connected' | 'failed' (failed або таймаут). */
export function waitIceOutcome(peer, timeoutMs, timers) {
    const setT = (timers && timers.setTimeout) || setTimeout;
    const clrT = (timers && timers.clearTimeout) || clearTimeout;
    return new Promise((resolve) => {
        let done = false;
        let t = null;
        const finish = (v) => {
            if (done) return;
            done = true;
            if (t !== null) clrT(t);
            peer.oniceconnectionstatechange = null;
            resolve(v);
        };
        const check = () => {
            const s = peer.iceConnectionState;
            if (s === 'connected' || s === 'completed') finish('connected');
            else if (s === 'failed' || s === 'closed') finish('failed');
        };
        t = setT(() => finish('failed'), timeoutMs || DEFAULT_P2P_CONNECT_MS);
        peer.oniceconnectionstatechange = check;
        check();
    });
}

/**
 * negotiateViewer — увесь SDP-обмін глядача: (опційно) пряма нога, інакше
 * relay. Ставить remote description і повертає {peer, path} (null — сесія
 * вже не поточна).
 * Пряма нога повертає ще relayTicket — одноразовий квиток на relay, якщо
 * вже ЗЄДНАНА нога згодом обірветься (див. createDirectRescue).
 * o.relayTicket (порятунок обірваної прямої ноги) — /p2p/offer не чіпаємо,
 * одразу relay з цим квитком.
 * o: { p2p, p2pUrl, signalUrl, standby, ticket, grant, requestTicket, peer, relayTicket,
 *      rebuildPeer() → Promise<peer>, waitIce(peer) → Promise<'connected'|'failed'>,
 *      makeBody(sdp, ticket), onTicket(ticket, grant, peer), onPath(path),
 *      isCurrent(), fetchFn, teardownSignal, timeoutMs, relayRetries, relayRetryMs, sleep }
 */
export async function negotiateViewer(o) {
    let peer = o.peer;
    let grant = o.grant;
    const current = o.isCurrent || (() => true);
    const onPath = o.onPath || (() => {});
    const net = { fetchFn: o.fetchFn, teardownSignal: o.teardownSignal, timeoutMs: o.timeoutMs || DEFAULT_OFFER_TIMEOUT_MS, combine: o.combine };
    let relayTicket = o.relayTicket || null;
    let ticket = o.ticket;
    const p2pUrl = (o.p2p && !relayTicket) ? p2pOfferUrl(o.signalUrl, o.p2pUrl) : null;
    if (p2pUrl) {
        const r = await postP2POffer({ ...net, url: p2pUrl, ticket, sdp: peer.localDescription.sdp });
        if (!current()) return null;
        if (r.kind === PATH_DIRECT) {
            onPath('direct-connecting');
            await peer.setRemoteDescription({ type: 'answer', sdp: r.sdp });
            const out = await o.waitIce(peer);
            if (!current()) return null;
            if (out === 'connected') {
                onPath(PATH_DIRECT);
                return { peer, path: PATH_DIRECT, relayTicket: r.relayTicket || '' };
            }
            // ICE не зʼєднався: нова PeerConnection на relay (стару вже
            // описано answer-ом агента). Агент сам звітує fallback хабу.
            if (!r.relayTicket) throw new Error('p2p: ICE failed і немає relay_ticket');
            relayTicket = r.relayTicket;
            peer = await o.rebuildPeer();
            if (!current()) return null;
        } else if (r.relayTicket) {
            relayTicket = r.relayTicket; // 409: той самий offer іде на relay
        } else if (r.ticket === 'fresh') {
            const t = await o.requestTicket();
            if (!current()) return null;
            if (!t || !t.ticket) throw new Error('offer/viewer: немає ticket');
            ticket = t.ticket;
            grant = t.grant ?? grant;
        }
    }
    let resp;
    if (relayTicket) {
        // Ввід на relay-нозі хаб судить за тікетом ноги — тепер це relay_ticket.
        if (o.onTicket) o.onTicket(relayTicket, grant, peer);
        resp = await redeemRelay({
            ...net, url: o.signalUrl, relayTicket,
            makeBody: (t) => o.makeBody(peer.localDescription.sdp, t),
            retries: o.relayRetries, retryMs: o.relayRetryMs, sleep: o.sleep,
        });
    } else {
        if (o.onTicket && ticket !== o.ticket) o.onTicket(ticket, grant, peer);
        resp = await postOfferWithFailover({
            ...net,
            urls: signalCandidates(o.signalUrl, o.standby),
            ticket,
            requestTicket: o.requestTicket,
            makeBody: (t) => o.makeBody(peer.localDescription.sdp, t),
            onTicket: (t, g) => { if (o.onTicket) o.onTicket(t, g, peer); },
        });
    }
    if (!current()) return null;
    if (!resp.ok) throw new Error('offer/viewer ' + resp.status);
    const answer = await readJsonBounded(resp, net);
    if (!current()) return null;
    if (!answer || !answer.sdp) throw new Error('offer/viewer: порожній answer');
    await peer.setRemoteDescription({ type: 'answer', sdp: answer.sdp });
    onPath(PATH_RELAY);
    return { peer, path: PATH_RELAY, answer };
}

/**
 * createDirectRescue — N6: обрив УЖЕ ЗЄДНАНОЇ прямої ноги (ICE failed, кадри
 * перестали йти, агент закрив ногу) — це не привід іти в Mesh: хаб видав
 * разом з answer одноразовий relay_ticket, і він стає дійсним, щойно агент
 * звітує fallback (його ICE-сторож). Порятунок — рівно один раз на сесію:
 * arm() після успішної прямої ноги, take() у фолбеку повертає квиток (і
 * гасить себе), якщо поточний шлях — direct. Relay-нога, що теж впала, іде
 * в Mesh як завжди.
 */
export function createDirectRescue() {
    let armed = null;
    return {
        arm(relayTicket, grant, signalUrl) {
            armed = relayTicket ? { relayTicket, grant, signalUrl } : null;
        },
        take(path) {
            if (path !== PATH_DIRECT || !armed) return null;
            const r = armed;
            armed = null;
            return r;
        },
        clear() { armed = null; },
        armed: () => armed !== null,
    };
}

export function createOoWebrtcLayer(o) {
    const opts = o || {};
    const container = opts.container;
    // F-20: НЕ const. Mesh-транспорт вміє перепідключитись ПІД живим шаром
    // (rebindMesh нижче) — тоді сюди лягає НОВИЙ Mesh-обʼєкт, а шар не вмирає.
    let meshDesktop = opts.meshDesktop;
    const config = opts.config || {};
    const onStateChange = opts.onStateChange || (() => {});

    const doc = (container && container.ownerDocument) || (typeof document !== 'undefined' ? document : null);
    if (!doc) throw new Error('createOoWebrtcLayer: потрібен DOM');
    if (typeof config.requestTicket !== 'function') throw new Error('createOoWebrtcLayer: немає requestTicket');

    let meshCanvas = findMeshCanvas(meshDesktop, container);
    if (!meshCanvas) throw new Error('createOoWebrtcLayer: не знайдено Mesh-canvas');

    let pc = null;
    let video = null;
    let abort = null;
    let resizeObserver = null;
    let attrObserver = null;
    let geometryTimer = null;
    let rvfcHandle = 0;
    let paused = false;
    let destroyed = false;
    let disconnectGrace = null;   // §MAJOR-6
    let hiddenKeepalive = null;   // §MAJOR-7
    let visibilityHandler = null; // §MAJOR-7
    let statsTimer = null;        // живий моніторинг якості
    let prevSample = null;        // попередній знімок getStats
    let badSinceTs = 0;           // відколи якість погана (для авто-фолбеку)
    let ooInput = null;           // власний канал вводу (oo-input.js), лише під config.input
    let ooTicket = '';            // квиток ЦІЄЇ сесії — ним підписана кожна подія вводу
    let frameSourceArmedGen = -1; // яку генерацію вже озброєно (idempotency для повторного ontrack)
    let meshPausedGen = -1;       // №11: у якій генерації Mesh уже глушили (на першому кадрі)
    let videoReceiver = null;     // №14: приймач відео — ручка адаптивного jitterBufferTarget
    let ooLeg = null;             // C3: id viewer-ноги з answer — без нього ICE restart неможливий
    let ooSignalUrl = null;       // C3: /offer/viewer цієї ноги
    // F-11: скільки повних перепідключень OO ще дозволено до чесного Mesh.
    const reconnectTries = config.reconnectTries != null ? config.reconnectTries : 2;
    let reconnectsLeft = reconnectTries;
    let liveAt = 0;               // коли шар востаннє став live (поповнення бюджету)
    let displayMode = normalizeDisplayMode(config.displayMode || loadDisplayMode());
    let dprQuery = null;          // TZ 1.1: matchMedia на поточний dpr
    let dprHandler = null;
    let toggleBtn = null;
    let meshSaved = null;         // оригінальні стилі Mesh-canvas/контейнера на час 1:1
    let meshWritten = null;       // що ми самі записали в ці стилі востаннє
    let textTiles = null;         // STAGE3-444 B: canvas з lossless тайлами тексту (config.textTiles)
    let tilesChannel = null;
    let cursorLayer = null;       // config.cursorLayer: окремий курсор (desktop-oo-cursor.js)
    let inputChannel = null;      // F5: config.inputChannel — власний канал вводу (desktop-oo-input.js)
    let inputDom = null;
    let statsOverlay = null;      // getStats()-оверлей (Ctrl+Alt+S); config.statsOverlay === false — вимкнено
    let transportPath = null;     // N6: 'direct' | 'relay' (| 'direct-connecting') — у стат-оверлеї
    let p2pTrialPeer = null;      // N6: peer, чий ICE ще пробує пряму ногу — його стани не фолбечать у Mesh
    const directRescue = createDirectRescue(); // N6: обрив зʼєднаної прямої ноги -> relay, не Mesh

    const session = createOoSession({
        firstFrameMs: config.firstFrameMs,
        frameAgeMs: config.frameAgeMs,
        onStateChange: (state, reason) => {
            if (state === OO_STATE_LIVE) liveAt = Date.now();
            onStateChange(state, reason);
        },
        onFallback: (gen, reason) => doFallback(gen, reason),
        onTimeout: (gen, reason) => tryReconnect(gen, reason),
    });

    // ── overlay ──────────────────────────────────────────────────────────────
    function makeOverlay() {
        const v = doc.createElement('video');
        v.className = 'oo-screen-layer oo-screen-layer-webrtc';
        v.autoplay = true;
        v.muted = true;
        v.defaultMuted = true;
        v.playsInline = true;
        v.setAttribute('autoplay', '');
        v.setAttribute('muted', '');
        v.setAttribute('playsinline', '');
        // pointer-events:none — НЕ косметика (див. шапку файла).
        // object-fit:fill безпечний ЛИШЕ тому, що syncGeometry() дає елементові
        // рівно ті пропорції, що й у потоку (F-29). Раніше елемент розтягувався
        // на весь Mesh-canvas, і потік з іншим співвідношенням сторін (інший
        // монітор, зміна роздільної здатності) виглядав приплюснутим.
        v.style.cssText = 'position:absolute;pointer-events:none;z-index:5;object-fit:fill;background:transparent;';
        const cs = doc.defaultView && doc.defaultView.getComputedStyle(container);
        if (cs && cs.position === 'static') container.style.position = 'relative';
        container.appendChild(v);
        return v;
    }

    // Геометрія — та сама логіка, що в canvas-версії: міряємо від ФАКТИЧНОГО
    // боксу Mesh-canvas, а не від контейнера (Mesh лєтербоксить картинку, і
    // «розтягнути на контейнер» дало б зсув між тим, що видно, і тим, куди
    // клікає користувач).
    //
    // TZ 1.1: input і далі ловить Mesh-canvas і сам мапить клік через СВІЙ
    // rect → canvas.width/height. Тому в режимі 1:1 ми міняємо CSS-розмір
    // саме Mesh-canvas (а відео просто їде за ним): так мапінг Mesh лишається
    // правильним без жодного нашого перерахунку. Відео всередині цього боксу
    // — contain: якщо пропорції кадру хаба й canvas збігаються (норма),
    // лєтербоксу нема і піксель під курсором = піксель, куди піде клік.
    function setStyle(el, prop, val) {
        // Пишемо лише при зміні: інакше MutationObserver(style) → syncGeometry
        // → знову запис → нескінченна петля.
        if (el.style[prop] !== val) el.style[prop] = val;
    }

    function currentDpr() {
        const w = doc.defaultView;
        return (w && w.devicePixelRatio > 0) ? w.devicePixelRatio : 1;
    }

    function applyOneToOne() {
        const vw = video && video.videoWidth | 0;
        const vh = video && video.videoHeight | 0;
        if (!(vw > 0 && vh > 0)) return false;
        const sz = oneToOneSize(vw, vh, currentDpr());
        const readMesh = () => ({
            width: meshCanvas.style.width, height: meshCanvas.style.height,
            maxWidth: meshCanvas.style.maxWidth, maxHeight: meshCanvas.style.maxHeight,
            overflow: container.style.overflow,
        });
        if (!meshSaved) {
            meshSaved = readMesh();
            meshWritten = null;
        } else {
            // Зміни, яких ми не писали, — від Mesh: вони й мають повернутись.
            absorbForeignStyles(meshSaved, meshWritten, readMesh());
        }
        setStyle(meshCanvas, 'maxWidth', 'none');
        setStyle(meshCanvas, 'maxHeight', 'none');
        setStyle(meshCanvas, 'width', sz.width + 'px');
        setStyle(meshCanvas, 'height', sz.height + 'px');
        setStyle(container, 'overflow', 'auto');
        meshWritten = readMesh(); // як браузер нормалізував наші значення
        return true;
    }

    // Старе правило image-rendering (до P-3) — для config.pixelSnap === false.
    function legacyImageRendering() {
        if (displayMode !== DISPLAY_1X1) return 'auto';
        const vw = video.videoWidth | 0;
        const vh = video.videoHeight | 0;
        if (!(vw > 0 && vh > 0)) return 'auto';
        return oneToOneSize(vw, vh, currentDpr()).integer ? 'pixelated' : 'auto';
    }

    // P-3: зсув блоку, від якого рахується position:absolute (padding-box
    // контейнера), у координатах в'юпорту.
    function containerOrigin(cr) {
        return {
            x: cr.left + (container.clientLeft || 0) - (container.scrollLeft || 0),
            y: cr.top + (container.clientTop || 0) - (container.scrollTop || 0),
        };
    }

    function restoreMesh() {
        if (!meshSaved) return;
        const s0 = meshSaved;
        meshSaved = null;
        meshWritten = null;
        try {
            meshCanvas.style.width = s0.width;
            meshCanvas.style.height = s0.height;
            meshCanvas.style.maxWidth = s0.maxWidth;
            meshCanvas.style.maxHeight = s0.maxHeight;
            container.style.overflow = s0.overflow;
        } catch (e) { /* ignore */ }
    }

    function syncGeometry() {
        if (!video) return;
        if (displayMode === DISPLAY_1X1) {
            applyOneToOne();
        } else {
            restoreMesh();
        }
        // Mesh ще не намалював жодного кадру (canvas лишився дефолтним 300×150
        // за HTML-специфікацією) — тоді бокс canvas, а з ним і OO-відео, крихітні.
        // Даємо canvas розміри OO-потоку: це той самий екран, і Mesh, коли
        // прокинеться, поставить ті самі числа.
        if (meshCanvas.width === 300 && meshCanvas.height === 150 && video.videoWidth > 0 && video.videoHeight > 0) {
            meshCanvas.width = video.videoWidth;
            meshCanvas.height = video.videoHeight;
        }
        const mr = meshCanvas.getBoundingClientRect();
        const cr = container.getBoundingClientRect();
        // F-29: пропорції беремо з ПОТОКУ, а не з Mesh-canvas. Лєтербокс
        // рахуємо самі (див. fitRect) — прямокутник елемента дорівнює
        // прямокутнику картинки, тож 0..1 в oo-input лишається точним.
        // 🚨 КУРСОР НЕ ТАМ (16.09.2026). Без власного вводу (config.input=false) мишу
        // веде Mesh і рахує координати по ВСЬОМУ Mesh-canvas. Вписане за
        // пропорціями потоку відео тоді зсунуте відносно тієї сітки — клік їде
        // повз. Тож лєтербокс лише з власним вводом; інакше відео = бокс canvas.
        // PR-оверлеї (тайли, курсор-шар, канал вводу) теж рахують по прямокутнику
        // картинки (containBox), тож лєтербокс потрібен і їм, не лише oo-input.
        const fit = (config.input || config.inputChannel || config.textTiles || config.cursorLayer)
            ? fitRect(mr.width, mr.height, video.videoWidth, video.videoHeight)
            : { left: 0, top: 0, width: mr.width, height: mr.height };
        let box;
        let rendering;
        if (config.pixelSnap === false) {
            // Відкат P-3: геометрія й image-rendering як до прив'язки.
            box = {
                left: mr.left - cr.left + container.scrollLeft + fit.left,
                top: mr.top - cr.top + container.scrollTop + fit.top,
                width: fit.width,
                height: fit.height,
            };
            rendering = legacyImageRendering();
        } else {
            // P-3: краї — на цілих фізичних пікселях; «майже 1:1» — рівно 1:1.
            // Зсув рахуємо від padding-box контейнера (clientLeft — рамка;
            // раніше її товщина зсувала відео відносно Mesh-canvas).
            const dpr = currentDpr();
            const vw = video.videoWidth | 0;
            const vh = video.videoHeight | 0;
            const s = snapToDeviceBox(mr.left + fit.left, mr.top + fit.top, fit.width, fit.height, dpr, vw, vh);
            const org = containerOrigin(cr);
            box = { left: s.x - org.x, top: s.y - org.y, width: s.width, height: s.height };
            rendering = imageRenderingFor(s.deviceWidth, s.deviceHeight, vw, vh);
        }
        setStyle(video, 'left', box.left + 'px');
        setStyle(video, 'top', box.top + 'px');
        setStyle(video, 'width', box.width + 'px');
        setStyle(video, 'height', box.height + 'px');
        setStyle(video, 'imageRendering', rendering);
        // Оверлей тайлів — у тому самому боксі й з тим самим image-rendering,
        // що й картинка: інакше lossless-тайл і відео під ним масштабуються
        // різними фільтрами й «двоять» на краях літер.
        if (textTiles) textTiles.place(box.left, box.top, box.width, box.height, rendering);
        if (cursorLayer) cursorLayer.relayout();
    }

    // Зміна dpr (перетяг вікна на інший монітор, Ctrl+/-) ResizeObserver не
    // ловить. matchMedia на ПОТОЧНЕ значення спрацьовує раз — перевішуємо.
    function watchDpr() {
        unwatchDpr();
        const w = doc.defaultView;
        if (!w || typeof w.matchMedia !== 'function') return;
        try {
            dprQuery = w.matchMedia('(resolution: ' + currentDpr() + 'dppx)');
            dprHandler = () => { syncGeometry(); watchDpr(); };
            if (typeof dprQuery.addEventListener === 'function') dprQuery.addEventListener('change', dprHandler);
            else if (typeof dprQuery.addListener === 'function') dprQuery.addListener(dprHandler);
        } catch (e) { dprQuery = null; dprHandler = null; }
    }

    function unwatchDpr() {
        if (dprQuery && dprHandler) {
            try {
                if (typeof dprQuery.removeEventListener === 'function') dprQuery.removeEventListener('change', dprHandler);
                else if (typeof dprQuery.removeListener === 'function') dprQuery.removeListener(dprHandler);
            } catch (e) { /* ignore */ }
        }
        dprQuery = null;
        dprHandler = null;
    }

    function setDisplayMode(m) {
        displayMode = normalizeDisplayMode(m);
        saveDisplayMode(displayMode);
        if (toggleBtn) toggleBtn.textContent = displayMode === DISPLAY_1X1 ? '100 %' : 'Підігнати';
        syncGeometry();
        return displayMode;
    }

    // Кнопка-перемикач. На відміну від <video>, вона СВІДОМО ловить кліки —
    // вона маленька, у куті й не перекриває робочу область Mesh по суті.
    // config.displayToggle === false — без кнопки (UI дає свою через API).
    function makeToggle() {
        if (config.displayToggle === false || toggleBtn) return;
        const b = doc.createElement('button');
        b.type = 'button';
        b.className = 'oo-screen-display-toggle';
        b.title = 'Режим показу: підігнати / 1:1';
        b.style.cssText = 'position:absolute;top:4px;right:4px;z-index:6;font:12px sans-serif;padding:2px 6px;opacity:.7;cursor:pointer;';
        b.textContent = displayMode === DISPLAY_1X1 ? '100 %' : 'Підігнати';
        b.addEventListener('click', (ev) => {
            ev.preventDefault();
            ev.stopPropagation();
            setDisplayMode(displayMode === DISPLAY_1X1 ? DISPLAY_FIT : DISPLAY_1X1);
        });
        container.appendChild(b);
        toggleBtn = b;
    }

    function watchGeometry() {
        if (typeof ResizeObserver === 'function') {
            resizeObserver = new ResizeObserver(() => syncGeometry());
            resizeObserver.observe(meshCanvas);
            resizeObserver.observe(container);
        }
        if (typeof MutationObserver === 'function') {
            attrObserver = new MutationObserver(() => syncGeometry());
            attrObserver.observe(meshCanvas, { attributes: true, attributeFilter: ['width', 'height', 'style', 'class'] });
        }
        if (video) video.addEventListener('resize', () => syncGeometry()); // зміна videoWidth/Height
        watchDpr();
        makeToggle();
        if (config.statsOverlay !== false && !statsOverlay) {
            statsOverlay = createStatsOverlay({ container, doc, getPc: () => pc, getPath: () => transportPath });
        }
    }

    /**
     * rebindMesh — F-20: Mesh-транспорт перепідключився ПІД ЖИВИМ OO-шаром.
     *
     * ЧОМУ ВОНО ПОТРІБНЕ. Mesh desktop-relay рветься на цілком живому агенті
     * (просів Wi-Fi ноутбука). Раніше кожен такий обрив ішов через
     * teardownActiveModule() -> teardownOoScreen(), тобто збій ОДНОГО транспорту
     * зносив картинку ІНШОГО, яка в ту секунду працювала бездоганно.
     *
     * Шар тримає Mesh-обʼєкт заради трьох речей, і всі три треба перевести на
     * новий обʼєкт, а не створювати шар заново:
     *   1. pause — поки OO дає кадри, Mesh мусить мовчати. НОВА Mesh-сесія
     *      народжується НЕ на паузі, тож глушимо її тут же (і лише якщо стара
     *      справді була заглушена: інакше поставили б паузу там, де шар її не
     *      ставив, і після destroy() ніхто б її не зняв).
     *   2. unpause+refresh на destroy() — після rebind вони поїдуть у ЖИВИЙ
     *      обʼєкт, а не в закритий (саме через це шар і не можна лишати
     *      привʼязаним до мертвої сесії).
     *   3. Mesh-canvas — джерело геометрії overlay. Зазвичай це той самий
     *      елемент DOM (id oo-remote-canvas переживає перепідключення), але
     *      якщо модуль підсунув інший — переармовуємо спостерігачів.
     *
     * @returns {boolean} true = шар тепер дивиться на новий Mesh-обʼєкт
     */
    function rebindMesh(next) {
        if (destroyed || !next || next === meshDesktop) return false;
        meshDesktop = next;
        if (paused) paused = meshCall(meshDesktop, MESH_PAUSE_NAMES);
        const canvas = findMeshCanvas(meshDesktop, container);
        if (canvas && canvas !== meshCanvas) {
            const wasCovered = meshCanvas && meshCanvas.style && meshCanvas.style.opacity === '0';
            coverMesh(false);
            meshCanvas = canvas;
            if (wasCovered) coverMesh(true);
            if (resizeObserver) { try { resizeObserver.disconnect(); } catch (e) { /* ignore */ } resizeObserver = null; }
            if (attrObserver) { try { attrObserver.disconnect(); } catch (e) { /* ignore */ } attrObserver = null; }
            watchGeometry();
        }
        syncGeometry();
        return true;
    }

    // ── фолбек ───────────────────────────────────────────────────────────────
    // 🚨 ЗАДВОЄННЯ КАРТИНКИ (16.09.2026). Відео вписується в Mesh-canvas за
    // пропорціями ПОТОКУ (fitRect), а Mesh на паузі лишається видимим зі своїм
    // останнім кадром. Пропорції розійшлись — у смугах лєтербоксу видно старий
    // Mesh-кадр іншого масштабу: «два потоки». Поки OO дає кадри, Mesh-canvas
    // прозорий; opacity, а не visibility — події миші мусять і далі доходити.
    function coverMesh(on) {
        try { if (meshCanvas && meshCanvas.style) meshCanvas.style.opacity = on ? '0' : ''; } catch (e) { /* ignore */ }
    }

    function unpauseMesh() {
        coverMesh(false);
        if (!paused) return;
        paused = false;
        meshCall(meshDesktop, MESH_UNPAUSE_NAMES);
        meshCall(meshDesktop, MESH_REFRESH_NAMES);
    }

    // Раз на statsIntervalMs знімає getStats, зводить його через
    // evaluateQualitySample і віддає назовні через config.onQuality (індикатор
    // біля перемикача Mesh/OO/Авто). Якщо ввімкнено config.qualityFallback —
    // на СТІЙКІЙ деградації (не коротшій за qualityDegradeMs) кладе сесію на
    // Mesh тим самим шляхом, що й сторож frame-age.
    function startQualityMonitor(gen) {
        const intervalMs = config.statsIntervalMs || DEFAULT_STATS_INTERVAL_MS;
        const degradeMs = config.qualityDegradeMs || DEFAULT_QUALITY_DEGRADE_MS;
        const limits = {
            minFps: config.qualityMinFps != null ? config.qualityMinFps : DEFAULT_QUALITY_MIN_FPS,
            maxRttMs: config.qualityMaxRttMs != null ? config.qualityMaxRttMs : DEFAULT_QUALITY_MAX_RTT_MS,
            maxLoss: config.qualityMaxLoss != null ? config.qualityMaxLoss : DEFAULT_QUALITY_MAX_LOSS,
        };
        const onQuality = typeof config.onQuality === 'function' ? config.onQuality : null;
        prevSample = null;
        badSinceTs = 0;
        // P-2: config.lowLatency === false — буфер браузера НЕ чіпаємо зовсім
        // (раніше адаптивна петля за 5 с усе одно ставила 0 і зводила прапорець
        // нанівець). Інакше — ціль із гістерезисом, база з playoutDelaySeconds.
        const jitterCtl = config.lowLatency === false ? null : createJitterTargetController({
            baseMs: typeof config.playoutDelaySeconds === 'number' ? config.playoutDelaySeconds * 1000 : 0,
        });

        const setI = config.setInterval || (typeof setInterval === 'function' ? setInterval : null);
        if (!setI || !pc || typeof pc.getStats !== 'function') return;

        statsTimer = setI(() => {
            if (!session.isCurrent(gen) || !pc) return;
            pc.getStats().then((report) => {
                if (!session.isCurrent(gen)) return;
                let inbound = null;
                let pair = null;
                let selectedPairId = null;
                const pairs = {};
                report.forEach((s) => {
                    if (s.type === 'inbound-rtp' && (s.kind === 'video' || s.mediaType === 'video')) {
                        inbound = s;
                    } else if (s.type === 'transport' && s.selectedCandidatePairId) {
                        selectedPairId = s.selectedCandidatePairId;
                    } else if (s.type === 'candidate-pair') {
                        pairs[s.id] = s;
                        // F-14: NOMINATED — це та пара, якою реально йде трафік.
                        // Раніше бралась ПРОСТО ОСТАННЯ з підхожих, тож будь-яка
                        // залишкова 'succeeded' пара затирала обрану, і RTT
                        // показувався від маршруту, яким нічого не передається.
                        if (s.nominated) pair = s;
                        else if (!pair && s.state === 'succeeded') pair = s;
                    }
                });
                // №9: nominated-пар буває кілька; справжню називає transport.
                if (selectedPairId && pairs[selectedPairId]) pair = pairs[selectedPairId];
                if (!inbound) return;

                const m = evaluateQualitySample(prevSample, inbound, pair, limits);
                const decodedBefore = prevSample ? prevSample.framesDecoded : null;
                prevSample = m.nextPrev;
                // План 0.3: живість за транспортом. rVFC тротлиться (вікно
                // перекрите, фонове), а декодер кадри рахує — отже потік живий.
                // Лише framesDecoded: байти без кадрів = декодер завмер, і це
                // мусить ловити frame-age, як і раніше.
                if (decodedBefore !== null && m.nextPrev.framesDecoded > decodedBefore) frameArrived(gen);
                // №14: адаптивний jitter-буфер — нуль на чистому каналі, ~60 мс на втратах.
                // P-2: з гістерезисом і лише коли lowLatency не вимкнено.
                if (jitterCtl && videoReceiver && 'jitterBufferTarget' in videoReceiver) {
                    const want = jitterCtl.next(m.lossPct);
                    try { if (videoReceiver.jitterBufferTarget !== want) videoReceiver.jitterBufferTarget = want; } catch (e) { /* hint */ }
                }
                // №5: прихована вкладка — хаб не шле відео (F-39), кожен семпл
                // був би «stalled/bad». Не оцінюємо й не накопичуємо badSince.
                if ((doc && doc.hidden) || pageHidden()) { badSinceTs = 0; return; }
                if (onQuality) {
                    try {
                        onQuality({
                            fps: m.fps != null ? Math.round(m.fps) : null,
                            lossPct: m.lossPct,
                            rttMs: m.rttMs,
                            // F-31/Q-01: індикатор і метрика мусять бачити
                            // РІЗНИЦЮ між «екран не змінюється» і «потік завмер».
                            idle: m.idle,
                            stalled: m.stalled,
                            bad: m.bad,
                            // F-14: ривки й буфер — те, що людина відчуває, а
                            // середній fps за 5с приховує.
                            freezes: m.freezes,
                            jitterBufferMs: m.jitterBufferMs,
                            // C4: телеметрія семпла (control.js мапить у bitrate_kbps…).
                            bitrateKbps: m.bitrateKbps,
                            jitterMs: m.jitterMs,
                            width: m.width,
                            height: m.height,
                        });
                    } catch (e) { /* індикатор не має валити сесію */ }
                }
                if (!config.qualityFallback) return;
                if (!m.bad) { badSinceTs = 0; return; }
                // Перша погана вибірка лише зводить годинник; фолбек — коли
                // погано ТРИМАЄТЬСЯ, інакше мережева яма смикала б картинку.
                if (!badSinceTs) { badSinceTs = m.nextPrev.ts; return; }
                if (m.nextPrev.ts - badSinceTs >= degradeMs && session.isCurrent(gen)) {
                    session.fallback(gen, (m.stalled ? 'quality-stalled: ' : 'quality-degraded: ')
                        + 'fps=' + (m.fps != null ? Math.round(m.fps) : '?')
                        + ' rtt=' + (m.rttMs != null ? m.rttMs : '?')
                        + 'ms loss=' + Math.round(m.lossPct * 100) + '%');
                }
            }).catch(() => { /* getStats у мертвій сесії — не подія */ });
        }, intervalMs);
    }

    function stopQualityMonitor() {
        if (statsTimer === null) return;
        const clrI = config.clearInterval || (typeof clearInterval === 'function' ? clearInterval : null);
        if (clrI) { try { clrI(statsTimer); } catch (e) { /* ignore */ } }
        statsTimer = null;
        prevSample = null;
        badSinceTs = 0;
    }

    function teardownOo() {
        coverMesh(false);
        stopQualityMonitor();
        // Ввід знімаємо ПЕРШИМ і до pc.close(): destroy() відпускає реально
        // затиснуті клавіші, а зробити це можна лише поки канал ще живий.
        if (ooInput) { try { ooInput.destroy(); } catch (e) { /* ignore */ } ooInput = null; }
        ooTicket = '';
        // №12: мертва нога — не адресат visibility; C3 — і не restart.
        ooSessionId = null;
        ooLeg = null;
        ooSignalUrl = null;
        videoReceiver = null;
        if (disconnectGrace) { try { disconnectGrace.cancel(); } catch (e) { /* ignore */ } disconnectGrace = null; }
        if (hiddenKeepalive) { try { hiddenKeepalive.stop(); } catch (e) { /* ignore */ } hiddenKeepalive = null; }
        if (visibilityHandler && doc && typeof doc.removeEventListener === 'function') {
            try { doc.removeEventListener('visibilitychange', visibilityHandler); } catch (e) { /* ignore */ }
        }
        visibilityHandler = null;
        if (geometryTimer !== null) { clearTimeout(geometryTimer); geometryTimer = null; }
        if (resizeObserver) { try { resizeObserver.disconnect(); } catch (e) { /* ignore */ } resizeObserver = null; }
        if (attrObserver) { try { attrObserver.disconnect(); } catch (e) { /* ignore */ } attrObserver = null; }
        unwatchDpr();
        if (statsOverlay) { try { statsOverlay.destroy(); } catch (e) { /* ignore */ } statsOverlay = null; }
        if (toggleBtn) { if (toggleBtn.parentNode) { try { toggleBtn.parentNode.removeChild(toggleBtn); } catch (e) { /* ignore */ } } toggleBtn = null; }
        // Mesh-canvas має повернутись до свого розміру ДО того, як ним знову
        // керує Mesh (фолбек), інакше його мапінг пішов би від нашого 1:1.
        restoreMesh();
        if (cursorLayer) { try { cursorLayer.destroy(); } catch (e) { /* ignore */ } cursorLayer = null; }
        detachInput();
        if (abort) { try { abort.abort(); } catch (e) { /* ignore */ } abort = null; }
        if (video) {
            if (rvfcHandle && typeof video.cancelVideoFrameCallback === 'function') {
                try { video.cancelVideoFrameCallback(rvfcHandle); } catch (e) { /* ignore */ }
            }
            rvfcHandle = 0;
            try { video.pause(); } catch (e) { /* ignore */ }
            try { video.srcObject = null; } catch (e) { /* ignore */ }
            if (video.parentNode) { try { video.parentNode.removeChild(video); } catch (e) { /* ignore */ } }
        }
        video = null;
        if (tilesChannel) {
            try { tilesChannel.onmessage = null; tilesChannel.close(); } catch (e) { /* ignore */ }
            tilesChannel = null;
        }
        if (textTiles) { try { textTiles.destroy(); } catch (e) { /* ignore */ } textTiles = null; }
        if (pc) {
            // Порядок: спершу глушимо колбеки, потім close(). Інакше
            // connectionstatechange='closed' від НАШОГО ж close() прилітає як
            // «сесія померла» і затирає причину справжнього фолбеку.
            try { pc.ontrack = null; pc.onconnectionstatechange = null; pc.oniceconnectionstatechange = null; } catch (e) { /* ignore */ }
            try { pc.close(); } catch (e) { /* ignore */ }
            pc = null;
        }
    }

    /**
     * F-11: одна спроба ПІДНЯТИ OO НАНОВО перед тим, як здатись на Mesh.
     *
     * Це ДРУГА сходинка. Першою йде ICE restart на тому ж PC (C3, iceRestart
     * через /offer/viewer/restart за leg); сюди потрапляємо, коли його нема
     * (старий хаб без leg) або він не підняв зʼєднання за 5с. Тут «restart» =
     * повний перезапуск шару: новий PC, новий квиток, нова нога.
     *
     * Бюджет скінченний (reconnectTries, за замовчуванням 2) і поповнюється
     * лише після DEFAULT_RECONNECT_REFILL_MS у live: ПК, що впав назовсім або
     * мерехтить, не крутить нескінченний цикл замість чесного Mesh.
     *
     * Поки перепідключаємось — Mesh на екрані: замерзлий останній OO-кадр
     * виглядає як жива картинка, і людина клікає в неї, а клік нікуди не йде.
     */
    function tryReconnect(gen, reason) {
        if (destroyed || !session.isCurrent(gen)) return false;
        // N6: обрив зʼєднаної прямої ноги — спершу relay-порятунок (doFallback), не повний reconnect.
        if (transportPath === PATH_DIRECT && directRescue.armed()) return false;
        if (liveAt && Date.now() - liveAt >= (config.reconnectRefillMs || DEFAULT_RECONNECT_REFILL_MS)) {
            reconnectsLeft = reconnectTries;
        }
        liveAt = 0;
        if (reconnectsLeft <= 0) return false;
        reconnectsLeft -= 1;
        unpauseMesh();
        teardownOo();
        // session.fallback() НЕ кличемо свідомо: він емітить OO_STATE_FALLBACK,
        // а control.js на нього перемикає режим на Mesh назавжди — спроби б не
        // лишилось. start() сам почне нову генерацію через session.begin().
        try { onStateChange(OO_STATE_CONNECTING, 'reconnect: ' + reason); } catch (e) { /* журнал не блокер */ }
        start();
        return true;
    }

    function doFallback(gen, reason) {
        // N6: впала вже зʼєднана пряма нога — relay з relay_ticket (новий
        // peer, нова генерація), а не Mesh. Mesh на час переходу розглушуємо,
        // як і при першому підключенні: пауза — лише після SDP-обміну.
        const rescue = destroyed ? null : directRescue.take(transportPath);
        if (rescue) {
            restoreMesh();
            unpauseMesh();
            teardownOo();
            transportPath = null;
            start(rescue);
            return;
        }
        directRescue.clear();
        // Спершу повертаємо Mesh-картинку, потім знімаємо OO-шар — зворотний
        // порядок дає видиму дірку, у яку встигають клікнути. Розмір
        // Mesh-canvas (режим 1:1) повертаємо ще раніше — до того, як Mesh малює.
        restoreMesh();
        unpauseMesh();
        teardownOo();
    }

    // ── старт ────────────────────────────────────────────────────────────────
    function geometryReady() {
        return (meshCanvas.width | 0) > 0 && (meshCanvas.height | 0) > 0;
    }

    function waitGeometry(gen) {
        return new Promise((resolve) => {
            if (geometryReady()) { resolve(true); return; }
            const deadline = Date.now() + (config.geometryTimeoutMs || DEFAULT_GEOMETRY_MS);
            const poll = () => {
                if (!session.isCurrent(gen)) { resolve(false); return; }
                if (geometryReady()) { resolve(true); return; }
                if (Date.now() >= deadline) { resolve(false); return; }
                geometryTimer = setTimeout(poll, 100);
            };
            poll();
        });
    }

    // Джерела кадрів (див. шапку): rVFC; без нього — 'loadeddata' + ріст
    // totalVideoFrames; плюс ріст framesDecoded зі getStats у монітора якості.
    // readyState/currentTime брешуть (тікають і на застиглому треку) — їм не віримо.
    // §MAJOR-7: поки вкладка hidden — «підживлюємо» frame-age, щоб тротлінг rVFC
    // у фоні не завалив живу сесію хибним frame-age-timeout.
    function armHiddenKeepalive(gen) {
        if (hiddenKeepalive || !doc || typeof doc.addEventListener !== 'function') return;
        hiddenKeepalive = createHiddenFrameKeepalive({
            isHidden: () => !!doc.hidden,
            // keepAlive, НЕ noteFrame: у прихованій вкладці до першого кадру
            // noteFrame оголосив би live без жодного кадру (first_frame в ЕРП,
            // сторож першого кадру вимкнений).
            keepFresh: () => { if (session.isCurrent(gen)) session.keepAlive(gen); },
        });
        visibilityHandler = () => hiddenKeepalive && hiddenKeepalive.onVisibility();
        doc.addEventListener('visibilitychange', visibilityHandler);
        // Могли армитись уже у фоновій вкладці — одразу почати підживлення.
        if (doc.hidden) hiddenKeepalive.onVisibility();
    }

    // Справжній кадр (rVFC, loadeddata або ріст framesDecoded). №11: Mesh глушимо
    // лише на ПЕРШОМУ кадрі генерації, не одразу після answer — інакше до 8с
    // людина дивилась на застиглий Mesh-кадр, поки OO ще не дав жодного.
    function frameArrived(gen) {
        if (!session.isCurrent(gen)) return;
        session.noteFrame(gen);
        if (meshPausedGen !== gen) {
            meshPausedGen = gen;
            paused = meshCall(meshDesktop, MESH_PAUSE_NAMES);
        }
        if (paused) coverMesh(true);
    }

    function armFrameSource(gen) {
        if (!video) return;
        // ontrack може прилетіти повторно (кілька треків, ренегоціація) —
        // без цього кожен повтор навішував би ще один rVFC-цикл і ще одну
        // пару loadeddata/timeupdate слухачів на той самий <video>.
        if (frameSourceArmedGen === gen) return;
        frameSourceArmedGen = gen;
        // F-29: до першого кадру videoWidth === 0, тож перший syncGeometry()
        // рахував без пропорцій. 'resize' у <video> стріляє і на першому кадрі,
        // і на зміні роздільної здатності віддаленого екрана. №7: під guard-ом,
        // інакше кожен повторний ontrack навішував дубль.
        if (typeof video.addEventListener === 'function') {
            video.addEventListener('loadedmetadata', syncGeometry);
            video.addEventListener('resize', syncGeometry);
        }
        armHiddenKeepalive(gen);
        if (typeof video.requestVideoFrameCallback === 'function') {
            const onFrame = (_now, meta) => {
                if (!session.isCurrent(gen) || !video) return;
                frameArrived(gen);
                // Кадр іншої геометрії (інший монітор) — тайли до нього не стосуються;
                // кадр без анонсу still-повтору — ховаємо тайли (oo-text-tiles.js TYPE_STILL).
                if (textTiles) {
                    textTiles.onVideoFrame(video.videoWidth | 0, video.videoHeight | 0,
                        meta && typeof meta.presentedFrames === 'number' ? meta.presentedFrames : undefined);
                }
                rvfcHandle = video.requestVideoFrameCallback(onFrame);
            };
            rvfcHandle = video.requestVideoFrameCallback(onFrame);
            return;
        }
        // Браузер без rVFC: 'loadeddata' дає перший кадр, далі живість — лише
        // за ростом лічильника декодованих кадрів (тут і framesDecoded у
        // startQualityMonitor). timeupdate — лише привід глянути на лічильник:
        // сам він тікає і на застиглому треку.
        let seenFrames = 0;
        video.addEventListener('loadeddata', () => frameArrived(gen));
        video.addEventListener('timeupdate', () => {
            const q = video && typeof video.getVideoPlaybackQuality === 'function' ? video.getVideoPlaybackQuality() : null;
            if (q && q.totalVideoFrames > seenFrames) { seenFrames = q.totalVideoFrames; frameArrived(gen); }
        });
    }

    async function start(rescue) {
        const gen = session.begin();
        abort = new AbortController();
        video = makeOverlay();
        syncGeometry();
        watchGeometry();

        const okGeom = await waitGeometry(gen);
        if (!session.isCurrent(gen)) return;
        if (!okGeom) { session.fallback(gen, 'geometry-timeout'); return; }
        syncGeometry();

        if (typeof RTCPeerConnection !== 'function') {
            session.fallback(gen, 'webrtc-unavailable');
            return;
        }

        try {
            await connect(gen, rescue);
        } catch (e) {
            if (!session.isCurrent(gen)) return;
            const reason = 'connect-failed: ' + (e && e.message ? e.message : e);
            // F-11: сигналізація зривається і від однієї загубленої відповіді —
            // дати другий шанс дешевше, ніж відібрати в людини OO-картинку.
            if (tryReconnect(gen, reason)) return;
            session.fallback(gen, reason);
        }
    }

    function attachCursorChannel(peer, gen) {
        let dc;
        try {
            dc = peer.createDataChannel(CURSOR_CHANNEL_LABEL);
        } catch (e) { return; } // без курсора, але відео живе
        dc.binaryType = 'arraybuffer';
        if (cursorLayer) { try { cursorLayer.destroy(); } catch (e) { /* ignore */ } }
        const layer = createCursorLayer({
            doc,
            container,
            role: resolveCursorRole(config),
            // Ввід ловить Mesh-canvas (відео — pointer-events:none).
            targets: () => [meshCanvas, container],
            place: (p) => {
                if (!video || !p) return null;
                const srcW = p.frameW || video.videoWidth;
                const srcH = p.frameH || video.videoHeight;
                const vr = video.getBoundingClientRect();
                const cr = container.getBoundingClientRect();
                const m = mapRemoteToClient(p.x, p.y, vr, srcW, srcH);
                if (!m) return null;
                if (config.pixelSnap !== false) {
                    // P-3: гаряча точка — на цілому фізичному пікселі (як і відео):
                    // у 1:1 hotX·scale·dpr ціле, тож уся форма курсора лягає
                    // на сітку без білінійного розмиття; рамка контейнера — clientLeft.
                    const dpr = currentDpr();
                    const org = containerOrigin(cr);
                    return {
                        x: snapPx(vr.left + m.x, dpr) - org.x,
                        y: snapPx(vr.top + m.y, dpr) - org.y,
                        scale: m.scale,
                    };
                }
                return {
                    x: vr.left - cr.left + container.scrollLeft + m.x,
                    y: vr.top - cr.top + container.scrollTop + m.y,
                    scale: m.scale,
                };
            },
        });
        cursorLayer = layer;
        dc.onmessage = (ev) => {
            if (!session.isCurrent(gen) || cursorLayer !== layer) return;
            layer.onMessage(ev.data);
        };
    }

    function detachInput() {
        if (inputDom) { try { inputDom.destroy(); } catch (e) { /* ignore */ } inputDom = null; }
        if (inputChannel) {
            try { inputChannel.onopen = null; inputChannel.onclose = null; inputChannel.close(); } catch (e) { /* ignore */ }
            inputChannel = null;
        }
    }

    // armInput — після квитка: grant не control => канал закриваємо, ввід
    // лишається в Mesh. Інакше на onopen перехоплюємо ввід контейнера.
    function armInput(gen, ticket, granted) {
        const dc = inputChannel;
        if (!dc) return;
        if (!inputEnabledFor(config, resolveCursorRole(config), granted)) { detachInput(); return; }
        const attach = () => {
            if (!session.isCurrent(gen) || inputChannel !== dc || inputDom) return;
            const sender = createInputSender({
                ticket,
                send: (s) => { if (dc.readyState === 'open') dc.send(s); else throw new Error('closed'); },
                allowed: typeof config.inputAllowed === 'function' ? config.inputAllowed : null,
            });
            inputDom = attachInputDom({
                target: container, doc, sender, containBox,
                geometry: () => {
                    if (!video || !(video.videoWidth > 0)) return null;
                    return { rect: video.getBoundingClientRect(), srcW: video.videoWidth, srcH: video.videoHeight };
                },
            });
        };
        dc.onclose = () => {
            if (inputChannel !== dc) return;
            if (inputDom) { try { inputDom.destroy(); } catch (e) { /* ignore */ } inputDom = null; }
        };
        if (dc.readyState === 'open') attach(); else dc.onopen = attach;
    }

    // N6: прибрати peer, який пробував пряму ногу й не зʼєднався (його
    // канали теж), не чіпаючи overlay/сесію.
    function dropTrialPeer(peer) {
        try { peer.ontrack = null; peer.onconnectionstatechange = null; peer.oniceconnectionstatechange = null; } catch (e) { /* ignore */ }
        if (cursorLayer) { try { cursorLayer.destroy(); } catch (e) { /* ignore */ } cursorLayer = null; }
        detachInput();
        if (ooInput) { try { ooInput.destroy(); } catch (e) { /* ignore */ } ooInput = null; }
        if (tilesChannel) { try { tilesChannel.onmessage = null; tilesChannel.close(); } catch (e) { /* ignore */ } tilesChannel = null; }
        if (textTiles) { try { textTiles.destroy(); } catch (e) { /* ignore */ } textTiles = null; }
        if (disconnectGrace) { try { disconnectGrace.cancel(); } catch (e) { /* ignore */ } disconnectGrace = null; }
        try { peer.close(); } catch (e) { /* ignore */ }
    }

    async function preparePeer(gen) {
        const peer = new RTCPeerConnection(buildRtcConfig(config));
        pc = peer;
        const videoTx = peer.addTransceiver('video', { direction: 'recvonly' });
        // F-13: H264 першим — саме його кодує агент; далі знімаємо буфер
        // плавності з приймача (див. applyLowLatencyReceiver у ontrack).
        applyCodecPreferences(
            videoTx,
            config.RTCRtpReceiver || (typeof RTCRtpReceiver !== 'undefined' ? RTCRtpReceiver : null),
            config.preferCodecs || ['H264'],
            { h264Profiles: config.h264ProfileOrder !== false },
        );
        // Звук просимо ЛИШЕ коли сервер його справді віддає. Зайва звукова
        // доріжка в offer змусила б хаб домовлятись про те, чого він не
        // публікує, — а мовчазна невдача домовляння коштує всієї картинки.
        if (config.audio) peer.addTransceiver('audio', { direction: 'recvonly' });
        // Текстові тайли (STAGE3-444 B) — ЛИШЕ під config.textTiles: без нього
        // offer бітово той самий (жодного m=application). Канал створюємо ДО
        // offer-а; хаб (OO_SCREEN_TILES=1) шле ним тайли від агента.
        if (config.textTiles) {
            textTiles = createTileOverlay({ doc, container, containBox });
            syncGeometry();
            tilesChannel = peer.createDataChannel(TILES_LABEL);
            tilesChannel.binaryType = 'arraybuffer';
            tilesChannel.onmessage = (ev) => {
                if (!session.isCurrent(gen) || !textTiles) return;
                textTiles.onMessage(ev.data);
            };
        }
        // Шар курсора — ЛИШЕ під config.cursorLayer (типово вимкнено; агентові
        // потрібен -cursor-layer). Канал відкриває браузер ДО offer-а, хаб ловить
        // його спільним диспетчером OnDataChannel viewer-ноги (як input/tiles).
        // Без прапорця offer бітово той самий, що й раніше.
        if (config.cursorLayer) attachCursorChannel(peer, gen);
        // F5: канал вводу — ЛИШЕ під config.inputChannel і лише для ролі control.
        // Відкриваємо ДО offer-а; слухачі DOM чіпляємо, коли відомий grant
        // квитка і канал відкритий (armInput нижче). Мовчазний канал хаб не
        // карає: judgeInput судить повідомлення, а не сам факт відкриття.
        if (inputEnabledFor(config, resolveCursorRole(config), config.inputGrant ?? 'control')) { // мовчазний канал; DOM лише при явному grant (armInput)
            try { inputChannel = peer.createDataChannel(INPUT_CHANNEL_LABEL, { ordered: true }); } catch (e) { inputChannel = null; }
        }

        // Власний ввід (oo-input.js) — ЛИШЕ під прапорцем. Створюємо ДО
        // createOffer(), бо DataChannel мусить потрапити в те саме SDP, що й
        // медіа; квиток підставляємо пізніше через геттер — його беруть аж
        // перед відправкою offer-а, щоб не згорів по TTL, поки збирався ICE.
        // Прапорця немає -> createDataChannel не кличеться взагалі, у SDP
        // нічого не змінюється, і ввід їде MeshCentral-ом як їхав.
        ooTicket = '';
        if (config.input) {
            ooInput = createOoInput({
                pc: peer,
                ticket: () => ooTicket,
                target: container,
                // Поверхня — саме <video>, а не контейнер: 0..1 мусить бути по
                // КАРТИНЦІ, інакше клік поїде повз на будь-якому лєтербоксі.
                surface: () => video && video.getBoundingClientRect(),
                // Фокус повертаємо на Mesh-канву: tabindex на ній уже є
                // (desktop.js), і саме туди приходять клавіші.
                focusTarget: meshCanvas,
                coalesceMs: config.inputCoalesceMs,
                wheelPixelsPerNotch: config.wheelPixelsPerNotch,
            });
        }

        peer.ontrack = (ev) => {
            if (!session.isCurrent(gen) || !video) return;
            // F-13: буфер плавності знімаємо саме тут — receiver існує лише
            // після ontrack. Тільки відео; звук лишаємо як є.
            // config.lowLatency === false — вимкнути (PR); типово буфер знімаємо.
            if (config.lowLatency !== false) {
                applyLowLatencyReceiver(
                    ev.receiver,
                    ev.track && ev.track.kind,
                    config.playoutDelaySeconds,
                );
            }
            // Звук іде окремим шаром (createOoAudioLayer), а <video> тут німий.
            // Аудіо-трек без streams раніше ПІДМІНЯВ srcObject потоком лише зі
            // звуком — відео зникало, і за кілька секунд падав frame-age.
            if (ev.track && ev.track.kind === 'audio') return;
            videoReceiver = ev.receiver || null;
            video.srcObject = (ev.streams && ev.streams[0]) || new MediaStream([ev.track]);
            const p = video.play();
            if (p && typeof p.catch === 'function') p.catch(() => { /* autoplay muted — не має падати */ });
            armFrameSource(gen);
        };

        // §MAJOR-6: 'disconnected' крізь grace-таймер, 'failed'/'closed' — одразу.
        disconnectGrace = createDisconnectGrace({
            graceMs: config.disconnectGraceMs || DEFAULT_DISCONNECT_GRACE_MS,
            // F-11: провал ICE — ще не привід здаватись. Спершу одна повна
            // спроба підняти OO наново (tryReconnect), і лише коли бюджет
            // вичерпано — Mesh.
            onFallback: (reason) => {
                if (!session.isCurrent(gen)) return;
                if (tryReconnect(gen, reason)) return;
                session.fallback(gen, reason);
            },
            // C3: спершу ICE restart на тому ж PC (канал вводу й декодер живуть);
            // старий хаб без leg → false, і далі звичайний шлях.
            restartMs: config.iceRestartAfterMs,
            restartTimeoutMs: config.iceRestartTimeoutMs,
            onRestart: () => {
                if (!session.isCurrent(gen) || pc !== peer || !ooLeg || !ooSignalUrl) return false;
                iceRestart(gen, peer).catch((e) => {
                    if (session.isCurrent(gen) && disconnectGrace) {
                        disconnectGrace.restartFailed('ice-restart-failed: ' + (e && e.message ? e.message : e));
                    }
                });
                return true;
            },
        });
        const grace = disconnectGrace;
        peer.onconnectionstatechange = () => {
            if (!session.isCurrent(gen) || peer !== pc || p2pTrialPeer === peer) return;
            grace.note(peer.connectionState);
        };

        const offer = await peer.createOffer();
        // Звук (F1): правка SDP лише коли звук просили — без config.audio
        // offer бітово той самий, що й до Opus.
        await peer.setLocalDescription(config.audio
            ? { type: offer.type, sdp: opusStereoSdp(offer.sdp) }
            : offer);
        await waitIceGathering(peer);
        return peer;
    }

    async function connect(gen, rescue) {
        // F-12: квиток просимо ПАРАЛЕЛЬНО зі збиранням ICE (preparePeer), а не
        // після нього: платимо max(gathering, ticket) замість суми.
        // §6.4 / BLOCKER-1,3: node_id уже в claims ticket-а — hub звʼяже глядача
        // з publisher-ом цієї ноди. N6-порятунок: ERP-квиток не потрібен —
        // є одноразовий relay_ticket.
        const ticketPromise = rescue
            ? Promise.resolve({ ticket: rescue.relayTicket, signalUrl: rescue.signalUrl, grant: rescue.grant })
            : config.requestTicket();
        // Гілка-глушник: якщо gathering вийде РАНІШЕ за відмову квитка,
        // необроблена rejection лягла б у window.onunhandledrejection.
        ticketPromise.catch(() => { /* справжню помилку віддасть await нижче */ });
        const peer = await preparePeer(gen);
        if (!session.isCurrent(gen)) return;

        const ticketResp = await ticketPromise;
        const { ticket, signalUrl, grant: granted } = ticketResp || {};
        if (!session.isCurrent(gen)) return;
        if (!ticket || !signalUrl) throw new Error('offer/viewer: немає ticket або signalUrl');
        // Той самий квиток, що й у медіа: хаб звіряє його з кожним
        // повідомленням каналу вводу і рве сесію, якщо він не збігся.
        ooTicket = ticket;
        armInput(gen, ticket, granted);

        // §MAJOR-5: signal кожної спроби — teardown-abort АБО таймаут.
        // O2: без config.standbySignalUrls — один кандидат, як раніше.
        // F6: monitor>0 — потік додаткового монітора (desktop-oo-multimon.js).
        // N6: config.p2p (типово вимкнено) — спершу пряма нога (negotiateViewer);
        // без нього — той самий postOfferWithFailover. Додаткові монітори — relay.
        const p2pCfg = resolveP2P(ticketResp, config);
        const p2pOn = !rescue && p2pCfg.p2p && !(Number.isInteger(config.monitor) && config.monitor > 0);
        if (p2pOn) p2pTrialPeer = peer;
        let res;
        try {
            res = await negotiateViewer({
                p2p: p2pOn,
                p2pUrl: p2pCfg.p2pUrl,
                relayTicket: rescue ? rescue.relayTicket : null,
                signalUrl,
                standby: resolveStandby(ticketResp, config),
                ticket,
                grant: granted,
                requestTicket: config.requestTicket,
                peer,
                rebuildPeer: async () => {
                    p2pTrialPeer = null;
                    dropTrialPeer(peer);
                    const np = await preparePeer(gen);
                    if (!session.isCurrent(gen)) throw new Error('teardown');
                    return np;
                },
                waitIce: (p) => waitIceOutcome(p, config.p2pConnectTimeoutMs || DEFAULT_P2P_CONNECT_MS),
                makeBody: (sdp, t) => offerBody(sdp, t, config.monitor),
                onTicket: (t, g) => { if (session.isCurrent(gen)) { ooTicket = t; armInput(gen, t, g); } },
                onPath: (p) => { if (session.isCurrent(gen)) transportPath = p; },
                isCurrent: () => session.isCurrent(gen),
                teardownSignal: abort && abort.signal,
                timeoutMs: config.offerTimeoutMs || DEFAULT_OFFER_TIMEOUT_MS,
                fetchFn: (u, init) => fetch(u, init),
            });
        } finally {
            p2pTrialPeer = null;
        }
        if (!res || !session.isCurrent(gen)) return;
        if (res.path === PATH_DIRECT) directRescue.arm(res.relayTicket, granted, signalUrl);
        // Пряма нога відповідає на /p2p/offer без session_id/leg: visibility і
        // ICE restart для неї недоступні (хаб за замовчуванням слатиме далі).
        const answer = res.answer || {};

        // F-39: хаб адресує viewer-ногу за session_id з answer. Запамʼятовуємо
        // і одразу кажемо поточну видимість: вкладка могла бути прихованою вже
        // на момент підключення, а хаб сам про це не дізнається.
        ooSessionId = answer.session_id || null;
        ooVisibilityUrl = visibilityUrlFrom(signalUrl);
        // C3: нова нога хаба віддає leg; старий хаб — ні, тоді restart не пробуємо.
        ooLeg = typeof answer.leg === 'string' && answer.leg ? answer.leg : null;
        ooSignalUrl = signalUrl;
        sendVisibility(pageHidden());

        // Аж ТЕПЕР армимо 8с-сторож: SDP-обмін позаду. Mesh глушить перший
        // кадр (frameArrived, №11), а не answer.
        session.arm(gen);
        startQualityMonitor(gen);
    }

    /**
     * C3: ICE restart на ТОМУ Ж PeerConnection — хаб (pion) робить
     * SetRemoteDescription + CreateAnswer на своїй нозі за leg. Хаб не trickle,
     * тож кандидати мусять бути в SDP: чекаємо gathering (слухача ставимо ДО
     * setLocalDescription, щоб не проґавити 'complete').
     */
    async function iceRestart(gen, peer) {
        const url = restartUrlFrom(ooSignalUrl);
        const leg = ooLeg;
        if (!url || !leg) throw new Error('немає leg/url');
        if (typeof peer.restartIce === 'function') peer.restartIce();
        const gathered = waitIceGathering(peer, { skipIfComplete: false });
        await peer.setLocalDescription(await peer.createOffer({ iceRestart: true }));
        await gathered;
        if (!session.isCurrent(gen) || pc !== peer) return;
        const combined = combineAbortSignals(abort && abort.signal, config.iceRestartTimeoutMs || DEFAULT_ICE_RESTART_TIMEOUT_MS);
        let answer;
        try {
            const resp = await fetch(url, {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ leg, sdp: peer.localDescription.sdp, type: 'offer' }),
                signal: combined.signal,
            });
            if (!resp.ok) throw new Error('offer/viewer/restart ' + resp.status);
            answer = await resp.json();
        } finally {
            combined.cancel();
        }
        if (!session.isCurrent(gen) || pc !== peer) return;
        if (!answer || !answer.sdp) throw new Error('offer/viewer/restart: порожній answer');
        await peer.setRemoteDescription({ type: 'answer', sdp: answer.sdp });
    }

    // ── F-39: прихована вкладка ────────────────────────────────────────────
    //
    // Поки вкладка прихована, браузер декодує потік на повній швидкості —
    // марний трафік і CPU в людини. Ні track.enabled=false, ні
    // direction='inactive' цього не спиняють (друге ще й вимагає
    // ренегоціації), тож рішення на боці хаба: він перестає слати цій нозі
    // відео, а коли приховані ВСІ глядачі ноди — ставить агента на паузу.
    //
    // Помилка тут не критична за задумом: хаб за замовчуванням продовжує
    // слати. Тому мовчазний catch — це не проковтнута помилка, а обраний
    // безпечний бік.
    let ooSessionId = null;
    let ooVisibilityUrl = null;

    function visibilityUrlFrom(signal) {
        try {
            return new URL('/viewer/visibility', signal).toString();
        } catch (e) {
            return null;
        }
    }

    function sendVisibility(hidden, retry) {
        if (!ooSessionId || !ooVisibilityUrl) return;
        fetch(ooVisibilityUrl, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ session_id: ooSessionId, hidden: !!hidden }),
            keepalive: true, // visibilitychange може не встигнути до згортання
            credentials: 'omit',
        }).catch(() => {
            // Ретрай лише в бік «видимий»: не зекономити — не біда, а от
            // лишитись без картинки після повернення — біда.
            if (!hidden && !retry) setTimeout(() => sendVisibility(false, true), 1000);
        });
    }

    // hasDom — модуль вантажиться і в чистому Node (JS-гейти), де document
    // не існує зовсім. Без цієї перевірки сам конструктор шару падав
    // ReferenceError, і гейт червонів на 4 файлах одразу.
    const hasDom = typeof document !== 'undefined' && typeof window !== 'undefined';
    const pageHidden = () => hasDom && document.visibilityState === 'hidden';
    const onVisibilityChange = () => sendVisibility(pageHidden());
    // pageshow — повернення з bfcache, де visibilitychange може не спрацювати.
    const onPageShow = () => sendVisibility(false);
    if (hasDom) {
        document.addEventListener('visibilitychange', onVisibilityChange);
        window.addEventListener('pageshow', onPageShow);
    }
    // Свідомо НЕ слухаємо blur/focus: вікно без фокуса, але видиме — це той,
    // хто дивиться.

    start();

    return {
        state: () => session.state(),
        // N6: 'direct' | 'relay' | null (ще не вирішено).
        transport: () => transportPath,
        generation: () => session.current(),
        // F-20: перевʼязка на нову Mesh-сесію без смерті шару (див. rebindMesh).
        rebindMesh,
        /**
         * setMuted — єдина ручка звуку шару. Типово шар НІМИЙ (makeOverlay глушить
         * <video> одразу при створенні), і це не косметика: несподівано почути
         * чужий кабінет гірше, ніж не почути свій.
         *
         * Знімаючи глушник, ще раз кличемо play(): поки елемент був німим, браузер
         * міг лишити його на паузі, і сама по собі зміна muted його не зрушить.
         *
         * Повертає ФАКТИЧНИЙ стан, а не бажаний. Відтворення зі звуком браузер
         * дозволяє лише після дії людини; клік по регулятору такою дією і є, але
         * якщо відмова все ж прийшла — повертаємо шар у німий стан і кажемо про це
         * викликачу. Інакше кнопка малювала б «звук увімкнено» над тишею.
         *
         * @returns {Promise<boolean>} true = зараз німо
         */
        setMuted(m) {
            const mute = m !== false;
            if (!video) return Promise.resolve(true);
            const v = video;
            v.muted = mute;
            v.defaultMuted = mute;
            if (mute) return Promise.resolve(true);
            const p = v.play();
            if (!p || typeof p.then !== 'function') return Promise.resolve(false);
            return p.then(() => false, () => { v.muted = true; v.defaultMuted = true; return true; });
        },
        // TZ 1.1: 'fit' | '1:1'; зберігається в localStorage.
        getDisplayMode: () => displayMode,
        setDisplayMode,
        destroy(reason) {
            // F-39: слухачі видимості живуть стільки ж, скільки шар.
            if (hasDom) {
                try { document.removeEventListener('visibilitychange', onVisibilityChange); } catch (e) { /* ignore */ }
                try { window.removeEventListener('pageshow', onPageShow); } catch (e) { /* ignore */ }
            }
            ooSessionId = null;
            if (destroyed) return;
            destroyed = true;
            session.close(session.current(), reason || 'destroy');
            restoreMesh();
            unpauseMesh();
            teardownOo();
        },
    };
}

// ─────────────────────────────────────────────────────────────────────────────
// Звук без картинки (16.09.2026): Mesh дає зображення, OO — лише звук.
// ─────────────────────────────────────────────────────────────────────────────

/**
 * createOoAudioLayer — окреме OO-зʼєднання ЛИШЕ заради звуку ПК.
 *
 * ЧОМУ. MeshCentral звуку не передає зовсім, а якісніша картинка часто саме в
 * нього. Раніше звук був тільки разом з OO-зображенням, тобто вибір «Mesh»
 * означав тишу.
 *
 * ЯК. Звичайна viewer-нога хаба, одразу після answer —
 * POST /viewer/visibility {hidden:true, audio:true}: хаб не шле цій нозі відео,
 * але пересилає звук і тримає агента в "resume" (hub visibility.go, wantAudio).
 * Старий хаб поле audio не знає — тоді звуку не буде, а відео теж не піде:
 * безпечний бік. Відеотрансивер лишається в offer-і, бо хаб будує answer під
 * нього; без нього нога може не зібратись.
 *
 * Жодної паузи Mesh, жодного вводу, жодного сторожа кадрів: цей шар не
 * відповідає за зображення і не сміє на нього впливати.
 *
 * @param {object} o.config  { requestTicket, iceServers, offerTimeoutMs? }
 * @param {Function} [o.onClosed] (reason) — зʼєднання померло саме
 * @returns {{ ready: Promise<boolean>, destroy: Function }} ready: true = звук грає
 */
export function createOoAudioLayer(o) {
    const opts = o || {};
    const config = opts.config || {};
    const onClosed = typeof opts.onClosed === 'function' ? opts.onClosed : () => {};
    let destroyed = false;
    let pc = null;
    const audio = document.createElement('audio');
    audio.autoplay = true;
    audio.hidden = true;
    document.body.appendChild(audio);
    const abort = new AbortController();

    // №13: 'disconnected' теж смерть звуку — але крізь grace, як у картинки;
    // раніше звук зникав тихо, і кнопка далі малювала «увімкнено».
    const grace = createDisconnectGrace({
        graceMs: config.disconnectGraceMs,
        onFallback: (reason) => { if (!destroyed) { destroy(reason); onClosed(reason); } },
    });

    function destroy(reason) {
        if (destroyed) return;
        destroyed = true;
        grace.cancel();
        try { abort.abort(); } catch (e) { /* ignore */ }
        try { audio.pause(); audio.srcObject = null; audio.remove(); } catch (e) { /* ignore */ }
        if (pc) { try { pc.close(); } catch (e) { /* ignore */ } pc = null; }
        return reason;
    }

    async function connect() {
        const peer = new RTCPeerConnection({ iceServers: config.iceServers || [] });
        pc = peer;
        peer.addTransceiver('video', { direction: 'recvonly' });
        peer.addTransceiver('audio', { direction: 'recvonly' });
        peer.ontrack = (ev) => {
            if (destroyed || !ev.track || ev.track.kind !== 'audio') return;
            audio.srcObject = new MediaStream([ev.track]);
        };
        peer.onconnectionstatechange = () => {
            if (!destroyed) grace.note(peer.connectionState);
        };
        const ticketPromise = config.requestTicket();
        ticketPromise.catch(() => {});
        await peer.setLocalDescription(await peer.createOffer());
        await waitIceGathering(peer);
        const { ticket, signalUrl } = await ticketPromise;
        if (destroyed) return false;
        const combined = combineAbortSignals(abort.signal, config.offerTimeoutMs || DEFAULT_OFFER_TIMEOUT_MS);
        let answer;
        try {
            const resp = await fetch(signalUrl, {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ sdp: peer.localDescription.sdp, ticket }),
                signal: combined.signal,
            });
            if (!resp.ok) throw new Error('offer/viewer ' + resp.status);
            answer = await resp.json();
        } finally {
            combined.cancel();
        }
        if (destroyed) return false;
        if (!answer || !answer.sdp) throw new Error('offer/viewer: порожній answer');
        await peer.setRemoteDescription({ type: 'answer', sdp: answer.sdp });
        // Той самий таймаут + teardown, що й в offer: хаб, який прийняв SDP і
        // завис на visibility, інакше лишав би ready невирішеним назавжди.
        const visCombined = combineAbortSignals(abort.signal, config.offerTimeoutMs || DEFAULT_OFFER_TIMEOUT_MS);
        let vis;
        try {
            vis = await fetch(new URL('/viewer/visibility', signalUrl).toString(), {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ session_id: answer.session_id, hidden: true, audio: true }),
                credentials: 'omit',
                signal: visCombined.signal,
            });
        } finally {
            visCombined.cancel();
        }
        if (!vis.ok) throw new Error('viewer/visibility ' + vis.status);
        if (destroyed) return false;
        // Звук зі звуком браузер дозволяє лише після дії людини; клік по кнопці
        // нею і є. Відмова — чесне false, кнопка не малюватиме «увімкнено».
        try { await audio.play(); } catch (e) { return false; }
        return true;
    }

    const ready = connect().catch((e) => {
        if (!destroyed) { destroy('connect-failed'); onClosed('connect-failed: ' + (e && e.message ? e.message : e)); }
        return false;
    });
    return { ready, destroy };
}
