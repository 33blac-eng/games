// Запуск: node resources/js/remote/__tests__/video-stats.test.mjs
import assert from 'node:assert/strict';
import { applyLowLatency, computeVideoStats, formatStatsLines, createStatsOverlay } from '../desktop-oo-stats.js';

// ── applyLowLatency: feature-detect ──────────────────────────────────────────
const chrome = { track: { kind: 'video' }, jitterBufferTarget: 500 };
const legacy = { track: { kind: 'video' }, playoutDelayHint: null };
const firefox = { track: { kind: 'video' } };
const audio = { track: { kind: 'audio' }, jitterBufferTarget: 100 };
const throwing = { track: { kind: 'video' }, get jitterBufferTarget() { return 1; }, set jitterBufferTarget(v) { throw new Error('x'); } };
assert.equal(applyLowLatency([chrome, legacy, firefox, audio, throwing, null]), 2);
assert.equal(chrome.jitterBufferTarget, 0);
assert.equal(legacy.playoutDelayHint, 0);
assert.ok(!('jitterBufferTarget' in firefox) && !('playoutDelayHint' in firefox));
assert.equal(audio.jitterBufferTarget, 100);
assert.equal(applyLowLatency(undefined), 0);

// ── computeVideoStats ────────────────────────────────────────────────────────
function report(t, extra) {
    return new Map([
        ['IT1', Object.assign({
            id: 'IT1', type: 'inbound-rtp', kind: 'video', timestamp: t, codecId: 'C1',
            bytesReceived: 0, framesDecoded: 0, packetsReceived: 0, packetsLost: 0,
            nackCount: 3, pliCount: 1, freezeCount: 2, totalFreezesDuration: 0.25,
            jitterBufferDelay: 2, jitterBufferEmittedCount: 100, totalDecodeTime: 0.5,
            frameWidth: 1920, frameHeight: 1080,
        }, extra)],
        ['C1', { id: 'C1', type: 'codec', mimeType: 'video/H264' }],
        ['T1', { id: 'T1', type: 'transport', selectedCandidatePairId: 'P1' }],
        ['P1', { id: 'P1', type: 'candidate-pair', currentRoundTripTime: 0.04 }],
        ['IA', { id: 'IA', type: 'inbound-rtp', kind: 'audio', timestamp: t, bytesReceived: 9e9 }],
    ]);
}
const r1 = computeVideoStats(report(1000, { bytesReceived: 1000, framesDecoded: 100, packetsReceived: 990, packetsLost: 10 }), null);
assert.equal(r1.stats.bitrateKbps, null);
assert.equal(r1.stats.fps, null);
assert.equal(r1.stats.lossPct, 1); // кумулятивно 10/1000
assert.equal(r1.stats.jitterBufferMs, 20);
assert.equal(r1.stats.decodeMs, 5);
assert.equal(r1.stats.rttMs, 40);
assert.equal(r1.stats.codec, 'H264');
assert.equal(r1.stats.totalFreezesDurationMs, 250);
assert.equal(r1.stats.networkBufferMs, 20 + 20 + 5);

const r2 = computeVideoStats(report(3000, { bytesReceived: 251000, framesDecoded: 160, packetsReceived: 1090, packetsLost: 20 }), r1.snap);
assert.equal(r2.stats.bitrateKbps, 1000); // 250000 Б * 8 / 2 с
assert.equal(r2.stats.fps, 30);           // 60 кадрів / 2 с
assert.ok(Math.abs(r2.stats.lossPct - (10 / 110) * 100) < 1e-9);

// framesPerSecond пріоритетніший за дельту
const r3 = computeVideoStats(report(4000, { framesPerSecond: 59, framesDecoded: 200 }), r2.snap);
assert.equal(r3.stats.fps, 59);

// без inbound-video / fallback на candidate-pair.selected (Firefox) / порожній
assert.equal(computeVideoStats(new Map(), null), null);
assert.equal(computeVideoStats(null, null), null);
const ff = computeVideoStats({
    a: { id: 'a', type: 'inbound-rtp', mediaType: 'video', timestamp: 1 },
    p: { id: 'p', type: 'candidate-pair', selected: true, currentRoundTripTime: 0.01 },
}, null);
assert.equal(ff.stats.rttMs, 10);
assert.equal(ff.stats.codec, null);
assert.equal(ff.stats.jitterBufferMs, null);

// ── formatStatsLines ─────────────────────────────────────────────────────────
const lines = formatStatsLines(r2.stats);
assert.ok(lines.includes('FPS: 30.0'));
assert.ok(lines.includes('Бітрейт: 1.00 Мбіт/с'));
assert.ok(lines.includes('Кодек: H264'));
assert.ok(lines.includes('Розмір: 1920×1080'));
assert.ok(lines.some((l) => l.startsWith('Мережа+буфер:')));
assert.deepEqual(formatStatsLines(null), ['немає inbound-video']);
assert.ok(formatStatsLines(ff.stats).includes('FPS: —'));

// ── createStatsOverlay: textContent, hotkey, cleanup ─────────────────────────
function el(tag) {
    const e = {
        tag, style: {}, children: [], parentNode: null, listeners: {},
        appendChild(c) { c.parentNode = e; e.children.push(c); },
        removeChild(c) { e.children.splice(e.children.indexOf(c), 1); c.parentNode = null; },
        addEventListener(t, f) { e.listeners[t] = f; },
        removeEventListener(t, f) { if (e.listeners[t] === f) delete e.listeners[t]; },
    };
    Object.defineProperty(e, 'innerHTML', { set() { throw new Error('innerHTML заборонено'); } });
    return e;
}
const intervals = new Set();
const win = { setInterval: (f) => { const id = {}; intervals.add(id); return id; }, clearInterval: (id) => intervals.delete(id) };
const docListeners = {};
const doc = {
    defaultView: win, createElement: el,
    addEventListener(t, f) { docListeners[t] = f; },
    removeEventListener(t, f) { if (docListeners[t] === f) delete docListeners[t]; },
};
const container = el('div');
const fakePc = { getStats: async () => report(1000, { codecId: 'C1' }) };
const ov = createStatsOverlay({ container, doc, getPc: () => fakePc });
assert.equal(container.children.length, 2);
const panel = container.children[0];
assert.equal(ov.visible(), false);
let prevented = false;
docListeners.keydown({ ctrlKey: true, altKey: true, code: 'KeyS', preventDefault() { prevented = true; }, stopPropagation() {} });
assert.ok(prevented && ov.visible() && intervals.size === 1);
await new Promise((r) => setTimeout(r, 0));
assert.ok(panel.textContent.includes('Кодек: H264'));
docListeners.keydown({ ctrlKey: false, altKey: true, code: 'KeyS', preventDefault() { throw new Error('не наш'); } });
container.children[1].listeners.click({ preventDefault() {}, stopPropagation() {} });
assert.equal(ov.visible(), false);
assert.equal(intervals.size, 0);
ov.show();
ov.destroy();
assert.equal(intervals.size, 0);
assert.equal(container.children.length, 0);
assert.equal(docListeners.keydown, undefined);

console.log('video-stats: ok');
