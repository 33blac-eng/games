// Запуск: node resources/js/remote/__tests__/display-mapping.test.mjs
import assert from 'node:assert/strict';
import {
    containBox, oneToOneSize, mapClientToRemote, normalizeDisplayMode,
    loadDisplayMode, saveDisplayMode, DISPLAY_FIT, DISPLAY_1X1,
} from '../desktop-oo-webrtc.js';

// contain: пропорції збігаються — без лєтербоксу
assert.deepEqual(containBox(960, 540, 1920, 1080), { x: 0, y: 0, width: 960, height: 540 });
// ширший бокс — смуги зліва/справа
assert.deepEqual(containBox(1000, 540, 1920, 1080), { x: 20, y: 0, width: 960, height: 540 });
// вищий бокс — смуги зверху/знизу
assert.deepEqual(containBox(960, 600, 1920, 1080), { x: 0, y: 30, width: 960, height: 540 });

// 1:1
assert.deepEqual(oneToOneSize(1920, 1080, 2), { width: 960, height: 540, integer: true });
assert.deepEqual(oneToOneSize(1920, 1080, 1), { width: 1920, height: 1080, integer: true });
assert.equal(oneToOneSize(1920, 1080, 1.25).integer, false);
assert.equal(oneToOneSize(1920, 1080, 1.25).width, 1536);
assert.equal(oneToOneSize(100, 100, 0).width, 100);

// мапінг
const rect = { left: 100, top: 50, width: 1000, height: 540 }; // смуги по 20px з боків
assert.equal(mapClientToRemote(110, 100, rect, 1920, 1080), null);           // у смузі
assert.deepEqual(mapClientToRemote(120, 50, rect, 1920, 1080), { x: 0, y: 0 });
assert.deepEqual(mapClientToRemote(120 + 480, 50 + 270, rect, 1920, 1080), { x: 960, y: 540 });
assert.deepEqual(mapClientToRemote(120 + 960, 50 + 540, rect, 1920, 1080), { x: 1919, y: 1079 });
assert.equal(mapClientToRemote(1081, 100, rect, 1920, 1080), null);
assert.equal(mapClientToRemote(0, 0, rect, 0, 0), null);
// 1:1 при dpr 2: CSS 960×540 ↔ 1920×1080
assert.deepEqual(mapClientToRemote(0.5, 0.5, { left: 0, top: 0, width: 960, height: 540 }, 1920, 1080), { x: 1, y: 1 });

// режим + сховище
assert.equal(normalizeDisplayMode('x'), DISPLAY_FIT);
assert.equal(normalizeDisplayMode('1:1'), DISPLAY_1X1);
const mem = new Map();
const st = { getItem: (k) => mem.get(k) ?? null, setItem: (k, v) => mem.set(k, v) };
assert.equal(loadDisplayMode(st), DISPLAY_FIT);
saveDisplayMode('1:1', st);
assert.equal(loadDisplayMode(st), DISPLAY_1X1);
const broken = { getItem() { throw new Error('denied'); }, setItem() { throw new Error('denied'); } };
assert.equal(loadDisplayMode(broken), DISPLAY_FIT);
saveDisplayMode('1:1', broken); // не кидає
assert.equal(loadDisplayMode(null), DISPLAY_FIT);

console.log('display-mapping: ok');
