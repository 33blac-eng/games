// Запуск: node --import ./__tests__/erp-stubs.mjs __tests__/stats-quality.test.mjs
// PLAYER-QUALITY P-4: інтервальні буфер/декод, ціль буфера, rVFC-таймінги, масштаб.
import assert from 'node:assert/strict';
import {
    computeVideoStats, formatStatsLines, createFrameTimingMeter, formatFrameTimingLine,
    formatScaleLine, createStatsOverlay, FRAME_TIMING_WINDOW,
} from '../desktop-oo-stats.js';

const inbound = (t, x) => new Map([['I', Object.assign({ id: 'I', type: 'inbound-rtp', kind: 'video', timestamp: t }, x)]]);

// ── буфер і декод — за інтервал, не за сесію ───────────────────────────────
{
    // година сесії: 108000 кадрів по 10 мс буфера, 4 мс декоду
    const a = computeVideoStats(inbound(0, {
        framesDecoded: 108000, totalDecodeTime: 432, jitterBufferDelay: 1080,
        jitterBufferEmittedCount: 108000, jitterBufferTargetDelay: 0,
    }), null);
    assert.equal(a.stats.jitterBufferMs, 10);    // перший тік — кумулятивне, як раніше
    assert.equal(a.stats.decodeMs, 4);
    assert.equal(a.stats.jitterTargetMs, 0);
    // наступна секунда: 30 кадрів, кожен чекав 200 мс (втрати + NACK), декод 8 мс,
    // браузер підняв ціль до 60 мс
    const b = computeVideoStats(inbound(1000, {
        framesDecoded: 108030, totalDecodeTime: 432 + 0.24, jitterBufferDelay: 1080 + 6,
        jitterBufferEmittedCount: 108030, jitterBufferTargetDelay: 1.8,
    }), a.snap);
    assert.ok(Math.abs(b.stats.jitterBufferMs - 200) < 1e-6, 'інтервал: ' + b.stats.jitterBufferMs);
    assert.ok(Math.abs(b.stats.decodeMs - 8) < 1e-6);
    assert.ok(Math.abs(b.stats.jitterTargetMs - 60) < 1e-6);
    // кумулятивне середнє цей сплеск майже не помітило б
    assert.ok((1086 / 108030) * 1000 < 10.1);
    assert.ok(formatStatsLines(b.stats).some((l) => l.startsWith('Jitter-буфер: 200.0 мс (ціль 60 мс)')));
    // інтервал без кадрів — назад на кумулятивне, не ділимо на нуль
    const c = computeVideoStats(inbound(2000, {
        framesDecoded: 108030, totalDecodeTime: 432.24, jitterBufferDelay: 1086, jitterBufferEmittedCount: 108030,
    }), b.snap);
    assert.ok(Number.isFinite(c.stats.jitterBufferMs) && Number.isFinite(c.stats.decodeMs));
    // браузер без jitterBufferTargetDelay (Firefox) — рядок без «ціль»
    assert.equal(c.stats.jitterTargetMs, null);
    assert.ok(formatStatsLines(c.stats).some((l) => /^Jitter-буфер: [\d.]+ мс$/.test(l)));
}

// ── rVFC: прийом→показ і декод ──────────────────────────────────────────────
{
    const m = createFrameTimingMeter({ size: 4 });
    assert.equal(m.snapshot(), null);
    assert.equal(formatFrameTimingLine(null), null);
    m.onFrame(0, null);                                     // без meta — ігнор
    m.onFrame(0, { receiveTime: 0, expectedDisplayTime: 10 }); // receiveTime 0 — не WebRTC-кадр
    assert.equal(m.snapshot(), null);
    for (const [r, e, p] of [[100, 120, 0.004], [200, 230, 0.005], [300, 310, 0.003], [400, 440, 0.006], [500, 515, 0.004]]) {
        m.onFrame(e, { receiveTime: r, expectedDisplayTime: e, processingDuration: p });
    }
    m.onFrame(0, { receiveTime: 600, expectedDisplayTime: 590 }); // показ раніше прийому — сміття
    const s = m.snapshot();
    // вікно 4: [30, 10, 40, 15] → відсортовано [10, 15, 30, 40]
    assert.equal(s.recvToDisplayP50, 15);
    assert.equal(s.recvToDisplayP95, 40);
    assert.ok(Math.abs(s.processingP50 - 4) < 1e-9);       // [5,3,6,4] → p50 = 4
    assert.equal(s.frames, 7);
    assert.equal(formatFrameTimingLine(s), 'Прийом→показ: 15 / 40 мс (p50/p95), декод 4.0 мс');
    m.reset();
    assert.equal(m.snapshot(), null);
    assert.equal(FRAME_TIMING_WINDOW, 120);
}

// ── масштаб показу ──────────────────────────────────────────────────────────
assert.equal(formatScaleLine({ videoW: 1920, videoH: 1080, cssW: 1536, cssH: 864, dpr: 1.25, rendering: 'auto' }),
    'Показ: 1920×1080 фіз. @1.25 → 1:1');
assert.equal(formatScaleLine({ videoW: 1920, videoH: 1080, cssW: 1600, cssH: 900, dpr: 1, rendering: 'auto' }),
    'Показ: 1600×900 фіз. @1 → 0.833×');
assert.equal(formatScaleLine({ videoW: 1280, videoH: 720, cssW: 2560, cssH: 1440, dpr: 1, rendering: 'pixelated' }),
    'Показ: 2560×1440 фіз. @1 → 2.000× (pixelated)');
assert.equal(formatScaleLine(null), null);
assert.equal(formatScaleLine({ videoW: 0, cssW: 10, dpr: 1 }), null);

// ── оверлей: нові рядки зʼявляються лише коли шар дає геттери ───────────────
async function overlayText(extra) {
    const created = [];
    const el = (tag) => {
        const e = { tag, style: {}, parentNode: null, textContent: '',
            appendChild(c) { c.parentNode = e; }, removeChild(c) { c.parentNode = null; },
            addEventListener() {}, removeEventListener() {} };
        created.push(e);
        return e;
    };
    const win = { setInterval: () => 1, clearInterval() {} };
    const doc = { defaultView: win, createElement: el, addEventListener() {}, removeEventListener() {} };
    const pc = { getStats: async () => inbound(1, { framesDecoded: 1 }) };
    const ov = createStatsOverlay({ container: el('div'), doc, getPc: () => pc, button: false, ...extra });
    ov.show();
    await new Promise((r) => setTimeout(r, 0));
    const panel = created.find((e) => e.className === 'oo-stats-overlay');
    const text = panel.textContent;
    ov.destroy();
    return text.split('\n');
}
{
    const meter = createFrameTimingMeter();
    meter.onFrame(0, { receiveTime: 10, expectedDisplayTime: 35, processingDuration: 0.002 });
    const lines = await overlayText({
        getFrameTiming: () => meter.snapshot(),
        getRender: () => ({ videoW: 1920, videoH: 1080, cssW: 1536, cssH: 864, dpr: 1.25 }),
    });
    assert.ok(lines.includes('Прийом→показ: 25 / 25 мс (p50/p95), декод 2.0 мс'), lines.join(' | '));
    assert.ok(lines.includes('Показ: 1920×1080 фіз. @1.25 → 1:1'));
    // без геттерів (старий виклик) — рядків нема, решта як була
    const plain = await overlayText({});
    assert.ok(!plain.some((l) => l.startsWith('Прийом→показ') || l.startsWith('Показ:')));
    assert.ok(plain.some((l) => l.startsWith('FPS:')));
    // rVFC ще не дав даних — рядка теж нема
    const empty = await overlayText({ getFrameTiming: () => createFrameTimingMeter().snapshot(), getRender: () => null });
    assert.ok(!empty.some((l) => l.startsWith('Прийом→показ') || l.startsWith('Показ:')));
}

console.log('stats-quality: ok');
