// Запуск: node resources/js/remote/__tests__/display-mapping.test.mjs
import assert from 'node:assert/strict';
import {
    containBox, oneToOneSize, mapClientToRemote, normalizeDisplayMode,
    loadDisplayMode, saveDisplayMode, DISPLAY_FIT, DISPLAY_1X1,
    absorbForeignStyles, oneToOnePlacement,
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

// 1:1: зміни стилів, яких ми не писали (Mesh), потрапляють у знімок
{
    const saved = { width: '800px', height: '', maxWidth: '100%', maxHeight: '', overflow: '' };
    const written = { width: '960px', height: '540px', maxWidth: 'none', maxHeight: 'none', overflow: 'auto' };
    // Mesh змінив maxHeight (ми потім перепишемо його на 'none')
    const current = { ...written, maxHeight: '90vh' };
    absorbForeignStyles(saved, written, current);
    assert.equal(saved.maxHeight, '90vh');
    assert.equal(saved.width, '800px');      // наше — знімок не чіпаємо
    assert.equal(saved.maxWidth, '100%');
    // ще нічого не записано — знімок без змін
    const s2 = { width: 'a' };
    assert.equal(absorbForeignStyles(s2, null, { width: 'b' }).width, 'a');
}

// Q-14: 1:1 — початок картинки на сітці ФІЗИЧНИХ пікселів, розмір точний.
{
    const near = (a, b) => Math.abs(a - b) < 1e-9;
    // dpr 1.25: 250 CSS px = 312.5 фізичних -> 312 (250 - 0.4) або 313
    const p = oneToOnePlacement(250, 100, 1366, 768, 1.25);
    const physL = (250 + p.dx) * 1.25;
    const physT = (100 + p.dy) * 1.25;
    assert.ok(near(physL, Math.round(physL)), 'left на сітці: ' + physL);
    assert.ok(near(physT, Math.round(physT)), 'top на сітці: ' + physT);
    assert.ok(Math.abs(p.dx * 1.25) <= 0.5 + 1e-9);
    // розмір — точно videoW/dpr: 1366 відео-пікселів = 1366 фізичних
    assert.ok(near(p.width * 1.25, 1366));
    assert.ok(near(p.height * 1.25, 768));
    // dpr 1.5, дробовий відступ із layout (1/64 CSS px)
    const q = oneToOnePlacement(100.015625, 33.3, 1920, 1080, 1.5);
    assert.ok(near((100.015625 + q.dx) * 1.5, Math.round((100.015625 + q.dx) * 1.5)));
    assert.ok(near((33.3 + q.dy) * 1.5, Math.round((33.3 + q.dy) * 1.5)));
    // уже на сітці — поправки нема
    assert.deepEqual(oneToOnePlacement(40, 8, 1920, 1080, 1.25), { dx: 0, dy: 0, width: 1536, height: 864 });
    // dpr 1, цілий відступ — нуль; дробовий — до найближчого пікселя
    assert.equal(oneToOnePlacement(17, 3, 800, 600, 1).dx, 0);
    assert.ok(near(oneToOnePlacement(17.4, 3, 800, 600, 1).dx, -0.4));
    // зламаний dpr / NaN — не NaN у стилях
    const z = oneToOnePlacement(NaN, undefined, 800, 600, 0);
    assert.deepEqual(z, { dx: 0, dy: 0, width: 800, height: 600 });
}

console.log('display-mapping: ok');
