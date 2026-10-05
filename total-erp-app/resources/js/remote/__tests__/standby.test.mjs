// O2: резервний хаб глядача. Запуск: node resources/js/remote/__tests__/standby.test.mjs
import assert from 'node:assert/strict';
import { signalCandidates, shouldFailover } from '../desktop-oo-webrtc.js';

// OFF за замовчуванням: рівно один URL.
assert.deepEqual(signalCandidates('https://a/offer/viewer'), ['https://a/offer/viewer']);
assert.deepEqual(signalCandidates('https://a/offer/viewer', 'x'), ['https://a/offer/viewer']);
// Порядок, без дублікатів і сміття.
assert.deepEqual(
    signalCandidates('https://a/o', ['https://b/o', '', 'https://a/o', 5, 'https://c/o']),
    ['https://a/o', 'https://b/o', 'https://c/o'],
);
// Мережева помилка — так; abort (teardown/таймаут) — ні.
assert.equal(shouldFailover(new TypeError('fetch failed'), 0), true);
const ab = new Error('x'); ab.name = 'AbortError';
assert.equal(shouldFailover(ab, 0), false);
// 5xx від проксі — так; 4xx (ticket) і 200, 500 — ні.
for (const s of [502, 503, 504]) assert.equal(shouldFailover(null, s), true);
for (const s of [200, 400, 401, 403, 404, 500]) assert.equal(shouldFailover(null, s), false);
console.log('standby.test.mjs OK');
