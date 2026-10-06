// OO-відео: low-latency налаштування приймача + оверлей статистики.
//
// Чисті функції (applyLowLatency, computeVideoStats, formatStatsLines) — без
// DOM, тестуються в node. createStatsOverlay — DOM-шар: кнопка + Ctrl+Alt+S,
// прихований за замовчуванням, опитує getStats() раз на секунду.
// Віддалені дані (codec тощо) пишемо ЛИШЕ через textContent, ніколи innerHTML.

export const STATS_INTERVAL_MS = 1000;

/**
 * Мінімальна затримка буфера приймача. Feature-detect: jitterBufferTarget
 * (стандарт, Chrome 114+/Safari), playoutDelayHint (старий Chrome). Firefox
 * не має жодного — просто нічого не робимо.
 * @returns {number} скільки приймачів реально налаштовано
 */
export function applyLowLatency(receivers) {
    let n = 0;
    for (const r of receivers || []) {
        if (!r || (r.track && r.track.kind && r.track.kind !== 'video')) continue;
        let ok = false;
        try { if ('jitterBufferTarget' in r) { r.jitterBufferTarget = 0; ok = true; } } catch (e) { /* ignore */ }
        try { if ('playoutDelayHint' in r) { r.playoutDelayHint = 0; ok = true; } } catch (e) { /* ignore */ }
        if (ok) n++;
    }
    return n;
}

function toMap(report) {
    const m = new Map();
    if (!report) return m;
    if (typeof report.forEach === 'function') report.forEach((v, k) => m.set((v && v.id) || k, v));
    else for (const k of Object.keys(report)) m.set(report[k].id || k, report[k]);
    return m;
}

const num = (x) => (typeof x === 'number' && Number.isFinite(x) ? x : null);

/**
 * Рахує метрики з двох знімків getStats(). prev може бути null (перший тік) —
 * тоді дельти null, але абсолютні метрики вже є.
 * @returns {{snap: object, stats: object}|null} snap — передати як prev наступного разу
 */
export function computeVideoStats(report, prev) {
    const m = toMap(report);
    let inb = null;
    let pair = null;
    let transport = null;
    for (const s of m.values()) {
        if (!s) continue;
        if (s.type === 'inbound-rtp' && (s.kind === 'video' || s.mediaType === 'video')) inb = s;
        else if (s.type === 'transport') transport = s;
    }
    if (transport && transport.selectedCandidatePairId) pair = m.get(transport.selectedCandidatePairId) || null;
    if (!pair) {
        for (const s of m.values()) {
            if (s && s.type === 'candidate-pair' && (s.selected || (s.nominated && s.state === 'succeeded'))) { pair = s; break; }
        }
    }
    if (!inb) return null;

    const ts = num(inb.timestamp);
    const snap = {
        ts,
        bytesReceived: num(inb.bytesReceived),
        framesDecoded: num(inb.framesDecoded),
        packetsReceived: num(inb.packetsReceived),
        packetsLost: num(inb.packetsLost),
        // P-4: для інтервальних середніх буфера/декоду.
        jitterBufferDelay: num(inb.jitterBufferDelay),
        jitterBufferTargetDelay: num(inb.jitterBufferTargetDelay),
        jitterBufferEmittedCount: num(inb.jitterBufferEmittedCount),
        totalDecodeTime: num(inb.totalDecodeTime),
    };
    const dt = prev && ts !== null && prev.ts !== null && ts > prev.ts ? (ts - prev.ts) / 1000 : null;
    const delta = (k) => (dt && snap[k] !== null && prev[k] !== null ? snap[k] - prev[k] : null);

    let fps = num(inb.framesPerSecond);
    const dFrames = delta('framesDecoded');
    if (fps === null && dFrames !== null) fps = dFrames / dt;
    const dBytes = delta('bytesReceived');
    const bitrateKbps = dBytes !== null ? (dBytes * 8) / dt / 1000 : null;

    // Втрати: за інтервал, якщо є prev, інакше кумулятивно.
    let lossPct = null;
    const dLost = delta('packetsLost');
    const dRecv = delta('packetsReceived');
    if (dLost !== null && dRecv !== null) {
        const lost = Math.max(0, dLost);
        lossPct = lost + dRecv > 0 ? (lost / (lost + dRecv)) * 100 : 0;
    } else if (snap.packetsLost !== null && snap.packetsReceived !== null) {
        const lost = Math.max(0, snap.packetsLost);
        lossPct = lost + snap.packetsReceived > 0 ? (lost / (lost + snap.packetsReceived)) * 100 : 0;
    }

    // PLAYER-QUALITY P-4: буфер і декод — середні ЗА ІНТЕРВАЛ (з дельт), а не
    // за всю сесію. Кумулятивне середнє через годину роботи майже не рухається:
    // сплеск буфера до 200 мс на втратах в оверлеї не видно взагалі. Перший
    // тік (prev нема) або інтервал без кадрів — кумулятивне, як раніше.
    const jbd = snap.jitterBufferDelay;
    const jbc = snap.jitterBufferEmittedCount;
    const dJbc = delta('jitterBufferEmittedCount');
    const dJbd = delta('jitterBufferDelay');
    let jitterBufferMs = jbd !== null && jbc ? (jbd / jbc) * 1000 : null;
    if (dJbc > 0 && dJbd !== null && dJbd >= 0) jitterBufferMs = (dJbd / dJbc) * 1000;
    // Ціль буфера, яку браузер реально тримає (jitterBufferTargetDelay, Chrome
    // 113+): видно, чи послухався він jitterBufferTarget/playoutDelayHint.
    let jitterTargetMs = null;
    const jbt = snap.jitterBufferTargetDelay;
    const dJbt = delta('jitterBufferTargetDelay');
    if (dJbc > 0 && dJbt !== null && dJbt >= 0) jitterTargetMs = (dJbt / dJbc) * 1000;
    else if (jbt !== null && jbc) jitterTargetMs = (jbt / jbc) * 1000;
    const tdt = snap.totalDecodeTime;
    let decodeMs = tdt !== null && snap.framesDecoded ? (tdt / snap.framesDecoded) * 1000 : null;
    const dTdt = delta('totalDecodeTime');
    if (dFrames > 0 && dTdt !== null && dTdt >= 0) decodeMs = (dTdt / dFrames) * 1000;
    const rttSec = pair ? num(pair.currentRoundTripTime) : null;
    const rttMs = rttSec !== null ? rttSec * 1000 : null;

    let codec = null;
    const c = inb.codecId ? m.get(inb.codecId) : null;
    if (c && typeof c.mimeType === 'string') codec = c.mimeType.replace(/^video\//i, '');

    const tfd = num(inb.totalFreezesDuration);
    const stats = {
        fps,
        bitrateKbps,
        lossPct,
        nackCount: num(inb.nackCount),
        pliCount: num(inb.pliCount),
        freezeCount: num(inb.freezeCount),
        totalFreezesDurationMs: tfd !== null ? tfd * 1000 : null,
        jitterBufferMs,
        jitterTargetMs,
        decodeMs,
        width: num(inb.frameWidth),
        height: num(inb.frameHeight),
        codec,
        rttMs,
        // НЕ glass-to-glass: лише мережа (RTT/2) + jitter-буфер + декод.
        networkBufferMs: rttMs !== null || jitterBufferMs !== null
            ? (rttMs !== null ? rttMs / 2 : 0) + (jitterBufferMs || 0) + (decodeMs || 0)
            : null,
    };
    return { snap, stats };
}

function fmt(x, digits, unit) {
    return x === null || x === undefined ? '—' : x.toFixed(digits) + (unit || '');
}
const orDash = (x) => (x === null || x === undefined ? '—' : String(x));

/** Рядки для оверлею (чистий текст). */
export function formatStatsLines(s) {
    if (!s) return ['немає inbound-video'];
    return [
        'FPS: ' + fmt(s.fps, 1),
        'Бітрейт: ' + fmt(s.bitrateKbps === null ? null : s.bitrateKbps / 1000, 2, ' Мбіт/с'),
        'Втрати: ' + fmt(s.lossPct, 2, ' %'),
        'NACK/PLI: ' + orDash(s.nackCount) + ' / ' + orDash(s.pliCount),
        'Фризи: ' + orDash(s.freezeCount) + ' (' + fmt(s.totalFreezesDurationMs, 0, ' мс') + ')',
        'Jitter-буфер: ' + fmt(s.jitterBufferMs, 1, ' мс')
            + (s.jitterTargetMs !== null && s.jitterTargetMs !== undefined ? ' (ціль ' + fmt(s.jitterTargetMs, 0, ' мс') + ')' : ''),
        'Декод: ' + fmt(s.decodeMs, 1, ' мс'),
        'Розмір: ' + (s.width && s.height ? s.width + '×' + s.height : '—'),
        'Кодек: ' + (s.codec || '—'),
        'RTT: ' + fmt(s.rttMs, 1, ' мс'),
        'Мережа+буфер: ' + fmt(s.networkBufferMs, 1, ' мс'),
    ];
}

// ── P-4: таймінги кадрів з requestVideoFrameCallback ───────────────────────
// getStats дає СЕРЕДНІ за інтервал; rVFC — кожен показаний кадр:
//   receiveTime          — коли прийшов останній пакет кадру (WebRTC-джерело);
//   expectedDisplayTime  — коли композитор покаже кадр;
//   processingDuration   — декод (с).
// Різниця expectedDisplayTime − receiveTime = jitter-буфер + декод + черга
// рендера — та частина затримки, що живе в БРАУЗЕРІ і яку ми крутимо
// jitterBufferTarget. Обидва часи — одна шкала (performance.now()), тож без
// синхронізації годинників. Мережу й агента це НЕ включає (див. M1 «скло-до-скла»).
export const FRAME_TIMING_WINDOW = 120;

function percentile(sorted, p) {
    if (!sorted.length) return null;
    const i = Math.min(sorted.length - 1, Math.max(0, Math.ceil((p / 100) * sorted.length) - 1));
    return sorted[i];
}

/**
 * createFrameTimingMeter — кільцевий буфер останніх `size` кадрів.
 * onFrame(now, meta) — прямо з колбека rVFC; snapshot() → null (даних нема,
 * напр. браузер без receiveTime) або { frames, recvToDisplayP50, recvToDisplayP95,
 * processingP50 } у мс.
 */
export function createFrameTimingMeter(o) {
    const size = (o && o.size > 0) ? o.size : FRAME_TIMING_WINDOW;
    const lat = [];
    const proc = [];
    let frames = 0;
    const push = (arr, v) => { arr.push(v); if (arr.length > size) arr.shift(); };
    return {
        onFrame(_now, meta) {
            if (!meta) return;
            frames += 1;
            const r = num(meta.receiveTime);
            const e = num(meta.expectedDisplayTime);
            // receiveTime = 0 / відсутній — не WebRTC-кадр або старий браузер.
            if (r !== null && r > 0 && e !== null && e >= r) push(lat, e - r);
            const p = num(meta.processingDuration);
            if (p !== null && p >= 0) push(proc, p * 1000);
        },
        snapshot() {
            if (!lat.length && !proc.length) return null;
            const ls = lat.slice().sort((a, b) => a - b);
            const ps = proc.slice().sort((a, b) => a - b);
            return {
                frames,
                recvToDisplayP50: percentile(ls, 50),
                recvToDisplayP95: percentile(ls, 95),
                processingP50: percentile(ps, 50),
            };
        },
        reset() { lat.length = 0; proc.length = 0; frames = 0; },
    };
}

/** Рядок оверлею для rVFC-таймінгів; null — нема даних. */
export function formatFrameTimingLine(t) {
    if (!t || (t.recvToDisplayP50 === null && t.processingP50 === null)) return null;
    return 'Прийом→показ: ' + fmt(t.recvToDisplayP50, 0) + ' / ' + fmt(t.recvToDisplayP95, 0, ' мс')
        + ' (p50/p95), декод ' + fmt(t.processingP50, 1, ' мс');
}

/**
 * P-4/P-3: рядок масштабу — головна підказка, чому текст «милий»: кадр
 * videoW×videoH показано на deviceW×deviceH ФІЗИЧНИХ пікселях. 1:1 — без
 * ресемплу; <1 — зменшення (тонкі лінії гублять); >1 — збільшення.
 * r: { videoW, videoH, cssW, cssH, dpr, rendering }.
 */
export function formatScaleLine(r) {
    if (!r || !(r.videoW > 0) || !(r.cssW > 0) || !(r.dpr > 0)) return null;
    const dw = Math.round(r.cssW * r.dpr);
    const dh = Math.round(r.cssH * r.dpr);
    const k = dw / r.videoW;
    const exact = dw === r.videoW && dh === r.videoH;
    return 'Показ: ' + dw + '×' + dh + ' фіз. @' + Number(r.dpr.toFixed(3)) + ' → '
        + (exact ? '1:1' : k.toFixed(3) + '×') + (r.rendering === 'pixelated' ? ' (pixelated)' : '');
}

/** N6: рядок шляху медіа — direct (агент↔браузер напряму) чи relay (через хаб). */
export function formatPathLine(path) {
    if (path === 'direct') return 'Шлях: direct (P2P)';
    if (path === 'relay') return 'Шлях: relay (хаб)';
    if (path === 'direct-connecting') return 'Шлях: direct… (ICE)';
    return 'Шлях: —';
}

/**
 * DOM-оверлей статистики. getPc() — актуальний RTCPeerConnection або null.
 * @returns {{show: Function, hide: Function, toggle: Function, visible: Function, destroy: Function}}
 */
export function createStatsOverlay(o) {
    const { container, getPc } = o;
    const doc = o.doc || container.ownerDocument;
    const win = doc.defaultView || globalThis;
    let timer = null;
    let prev = null;
    let destroyed = false;
    let busy = false;

    const panel = doc.createElement('div');
    panel.className = 'oo-stats-overlay';
    panel.style.cssText = 'position:absolute;top:28px;right:4px;z-index:7;display:none;pointer-events:none;'
        + 'font:11px/1.35 monospace;white-space:pre;color:#0f0;background:rgba(0,0,0,.7);padding:4px 6px;border-radius:3px;';
    container.appendChild(panel);

    let btn = null;
    if (o.button !== false) {
        btn = doc.createElement('button');
        btn.type = 'button';
        btn.className = 'oo-stats-toggle';
        btn.title = 'Статистика відео (Ctrl+Alt+S)';
        btn.textContent = 'i';
        btn.style.cssText = 'position:absolute;top:4px;right:84px;z-index:6;font:12px sans-serif;padding:2px 6px;opacity:.7;cursor:pointer;';
        btn.addEventListener('click', onClick);
        container.appendChild(btn);
    }
    doc.addEventListener('keydown', onKey, true);

    function onClick(ev) { ev.preventDefault(); ev.stopPropagation(); toggle(); }
    function onKey(ev) {
        if (ev.ctrlKey && ev.altKey && !ev.shiftKey && !ev.metaKey && ev.code === 'KeyS') {
            ev.preventDefault();
            ev.stopPropagation();
            toggle();
        }
    }

    async function tick() {
        const pc = getPc();
        if (busy || !pc || typeof pc.getStats !== 'function') return;
        busy = true;
        try {
            const report = await pc.getStats();
            if (destroyed || timer === null) return;
            const r = computeVideoStats(report, prev);
            prev = r ? r.snap : null;
            const lines = formatStatsLines(r && r.stats);
            if (typeof o.getPath === 'function') lines.unshift(formatPathLine(o.getPath()));
            // P-4: rVFC-таймінги й масштаб показу — лише коли шар їх дає.
            const ft = typeof o.getFrameTiming === 'function' ? formatFrameTimingLine(o.getFrameTiming()) : null;
            if (ft) lines.push(ft);
            const sc = typeof o.getRender === 'function' ? formatScaleLine(o.getRender()) : null;
            if (sc) lines.push(sc);
            panel.textContent = lines.join('\n');
        } catch (e) { /* ignore */ } finally { busy = false; }
    }

    function show() {
        if (destroyed || timer !== null) return;
        panel.style.display = 'block';
        panel.textContent = '…';
        prev = null;
        timer = win.setInterval(tick, STATS_INTERVAL_MS);
        tick();
    }
    function hide() {
        if (timer !== null) { win.clearInterval(timer); timer = null; }
        panel.style.display = 'none';
    }
    function toggle() { if (timer !== null) hide(); else show(); }

    return {
        show, hide, toggle,
        visible: () => timer !== null,
        destroy() {
            if (destroyed) return;
            destroyed = true;
            hide();
            doc.removeEventListener('keydown', onKey, true);
            if (btn) { btn.removeEventListener('click', onClick); if (btn.parentNode) btn.parentNode.removeChild(btn); btn = null; }
            if (panel.parentNode) panel.parentNode.removeChild(panel);
        },
    };
}
