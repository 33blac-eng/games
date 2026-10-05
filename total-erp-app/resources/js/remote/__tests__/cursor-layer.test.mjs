// Запуск: node resources/js/remote/__tests__/cursor-layer.test.mjs
import assert from 'node:assert/strict';
import {
    decodeCursorMessage, cursorPresentation, resolveCursorRole, shapeDataUrl,
    createCursorLayer, ROLE_CONTROL, ROLE_VIEW, CSS_CURSOR_MAX, CURSOR_CHANNEL_LABEL,
} from '../desktop-oo-cursor.js';
import { mapRemoteToClient, mapClientToRemote, containBox } from '../desktop-oo-webrtc.js';

const hex = (h) => Uint8Array.from(h.match(/../g).map((x) => parseInt(x, 16)));

// ── framing: ті самі golden-байти, що в cursorproto TestGoldenVectors ──────
const pos = decodeCursorMessage(hex('4301010004030201feffffff2c01000080073804').buffer);
assert.deepEqual(pos, { kind: 'pos', visible: true, shapeId: 0x01020304, x: -2, y: 300, frameW: 1920, frameH: 1080 });
const sh = decodeCursorMessage(hex('4302000007000000010001000000000001020304'));
assert.equal(sh.kind, 'shape');
assert.equal(sh.id, 7);
assert.equal(sh.format, 0);
assert.deepEqual([sh.w, sh.h, sh.hotX, sh.hotY], [1, 1, 0, 0]);
assert.deepEqual([...sh.data], [1, 2, 3, 4]);
assert.equal(CURSOR_CHANNEL_LABEL, 'oosc-cursor');

// межі: сміття, обрізане, задовге, id 0, >256, гаряча точка поза формою, RGBA не тієї довжини
assert.equal(decodeCursorMessage(hex('43')), null);
assert.equal(decodeCursorMessage(hex('4301010004030201feffffff2c010000800738')), null);          // 19 байт
assert.equal(decodeCursorMessage(hex('4301010004030201feffffff2c0100008007380400')), null);      // 21 байт
assert.equal(decodeCursorMessage(hex('5801010004030201feffffff2c01000080073804')), null);        // magic
assert.equal(decodeCursorMessage(hex('4309010004030201feffffff2c01000080073804')), null);        // kind
assert.equal(decodeCursorMessage(hex('4302000000000000010001000000000001020304')), null);        // id 0
assert.equal(decodeCursorMessage(hex('4302000007000000010101000000000001020304')), null);        // w=257
assert.equal(decodeCursorMessage(hex('4302000007000000010001000100000001020304')), null);        // hotX=w
assert.equal(decodeCursorMessage(hex('43020000070000000100010000000000010203')), null);          // RGBA 3 байти
assert.equal(decodeCursorMessage(hex('4302070007000000010001000000000001020304')), null);        // формат
assert.equal(decodeCursorMessage(new Uint8Array(65536)), null);
assert.equal(decodeCursorMessage('text'), null);

// PNG -> data URL без canvas
const png = { format: 1, data: Uint8Array.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]) };
assert.equal(shapeDataUrl(png), 'data:image/png;base64,iVBORw0KGgo=');
assert.equal(shapeDataUrl({ format: 0, data: new Uint8Array(4), w: 1, h: 1 }), null); // RGBA без doc

// ── мапінг: віддалений піксель -> клієнт, через той самий containBox ───────
// без лєтербоксу, масштаб 0.5
assert.deepEqual(mapRemoteToClient(100, 50, { left: 0, top: 0, width: 960, height: 540 }, 1920, 1080), { x: 50, y: 25, scale: 0.5 });
// смуги зліва/справа (бокс 1000x540): зсув на 20
assert.deepEqual(mapRemoteToClient(0, 0, { left: 0, top: 0, width: 1000, height: 540 }, 1920, 1080), { x: 20, y: 0, scale: 0.5 });
// зворотність із mapClientToRemote (у межах пікселя)
{
    const rect = { left: 10, top: 20, width: 1000, height: 600 };
    const b = containBox(rect.width, rect.height, 1920, 1080);
    for (const [rx, ry] of [[0, 0], [960, 540], [1919, 1079], [123, 777]]) {
        const m = mapRemoteToClient(rx + 0.5, ry + 0.5, rect, 1920, 1080);
        const back = mapClientToRemote(rect.left + m.x, rect.top + m.y, rect, 1920, 1080);
        assert.deepEqual(back, { x: rx, y: ry }, `round trip ${rx},${ry} (box ${JSON.stringify(b)})`);
    }
}
assert.equal(mapRemoteToClient(1, 1, null, 1920, 1080), null);
assert.equal(mapRemoteToClient(1, 1, { left: 0, top: 0, width: 100, height: 100 }, 0, 1080), null);

// ── хто малює курсор ───────────────────────────────────────────────────────
assert.equal(resolveCursorRole({}), ROLE_CONTROL);
assert.equal(resolveCursorRole({ viewOnly: true }), ROLE_VIEW);
assert.equal(resolveCursorRole({ viewOnly: true, cursorRole: 'control' }), ROLE_CONTROL);
assert.equal(resolveCursorRole({ cursorRole: 'view' }), ROLE_VIEW);

const small = { w: 32, h: 32, hotX: 3, hotY: 4 };
const big = { w: CSS_CURSOR_MAX + 1, h: 16, hotX: 0, hotY: 0 };
// керівник: малий — CSS-курсор з гарячою точкою, без оверлея
assert.deepEqual(cursorPresentation(ROLE_CONTROL, small, 'data:x', true), { css: 'url("data:x") 3 4, auto', overlay: false });
// керівник: рівно 128 — ще CSS
assert.equal(cursorPresentation(ROLE_CONTROL, { w: 128, h: 128, hotX: 0, hotY: 0 }, 'u', true).overlay, false);
// керівник: >128 — локальний ховаємо, оверлей
assert.deepEqual(cursorPresentation(ROLE_CONTROL, big, 'u', true), { css: 'none', overlay: true });
// керівник: віддалений прихований — ховаємо й локальний
assert.deepEqual(cursorPresentation(ROLE_CONTROL, small, 'u', false), { css: 'none', overlay: false });
// керівник: форми ще нема — нічого не чіпаємо
assert.deepEqual(cursorPresentation(ROLE_CONTROL, null, null, true), { css: '', overlay: false });
// глядач: завжди оверлей (коли видно), локальний не чіпаємо
assert.deepEqual(cursorPresentation(ROLE_VIEW, small, 'u', true), { css: '', overlay: true });
assert.deepEqual(cursorPresentation(ROLE_VIEW, small, 'u', false), { css: '', overlay: false });

// ── DOM-частина на фейковому документі ─────────────────────────────────────
function fakeEl(tag) {
    const attrs = {};
    return {
        tag, style: {}, children: [], parentNode: null,
        setAttribute(k, v) { attrs[k] = String(v); }, getAttribute(k) { return k in attrs ? attrs[k] : null; },
        appendChild(c) { c.parentNode = this; this.children.push(c); return c; },
        removeChild(c) { this.children = this.children.filter((x) => x !== c); c.parentNode = null; },
    };
}
const doc = { createElement: fakeEl };
const container = fakeEl('div');
const canvas = fakeEl('canvas');
canvas.style.cursor = 'crosshair';

// PNG-заголовок (сигнатура + IHDR) з розміром ihdrW×ihdrH (типово = w×h).
function pngShapeMsg(id, w, h, hx, hy, ihdrW = w, ihdrH = h) {
    const b = new Uint8Array(16 + 29);
    const dv = new DataView(b.buffer);
    b[0] = 0x43; b[1] = 2; b[2] = 1;
    dv.setUint32(4, id, true); dv.setUint16(8, w, true); dv.setUint16(10, h, true);
    dv.setUint16(12, hx, true); dv.setUint16(14, hy, true);
    b.set([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a], 16);
    dv.setUint32(24, 13); b.set([0x49, 0x48, 0x44, 0x52], 28);
    dv.setUint32(32, ihdrW); dv.setUint32(36, ihdrH);
    return b;
}
const pngUrl = (m) => 'data:image/png;base64,' + Buffer.from(m.subarray(16)).toString('base64');

// PNG, чий IHDR не збігається з заголовком форми (decompression bomb), —
// сміття; сигнатури без IHDR теж замало.
assert.equal(decodeCursorMessage(pngShapeMsg(5, 16, 16, 0, 0, 65535, 65535)), null);
assert.equal(decodeCursorMessage(pngShapeMsg(5, 16, 16, 0, 0).subarray(0, 24)), null);
assert.equal(decodeCursorMessage(pngShapeMsg(5, 16, 16, 0, 0)).kind, 'shape');
function posMsg(id, x, y, visible) {
    const b = new Uint8Array(20);
    const dv = new DataView(b.buffer);
    b[0] = 0x43; b[1] = 1; b[2] = visible ? 1 : 0;
    dv.setUint32(4, id, true); dv.setInt32(8, x, true); dv.setInt32(12, y, true);
    dv.setUint16(16, 100, true); dv.setUint16(18, 100, true);
    return b;
}
const place = (p) => ({ x: p.x * 2, y: p.y * 2, scale: 2 });

{
    const layer = createCursorLayer({ doc, container, targets: () => [canvas], place, role: ROLE_CONTROL });
    layer.onMessage(pngShapeMsg(5, 16, 16, 2, 3));
    layer.onMessage(posMsg(5, 10, 10, true));
    assert.equal(canvas.style.cursor, 'url("' + pngUrl(pngShapeMsg(5, 16, 16, 2, 3)) + '") 2 3, auto');
    assert.equal(container.children.length, 0, 'керівнику з малою формою оверлей не потрібен');
    layer.onMessage(posMsg(5, 10, 10, false));
    assert.equal(canvas.style.cursor, 'none');
    layer.onMessage(pngShapeMsg(6, 200, 200, 0, 0));
    layer.onMessage(posMsg(6, 10, 10, true));
    assert.equal(container.children.length, 1);
    assert.equal(container.children[0].style.display, 'block');
    layer.destroy();
    assert.equal(canvas.style.cursor, 'crosshair', 'destroy повертає курсор Mesh');
    assert.equal(container.children.length, 0);
}
{
    const layer = createCursorLayer({ doc, container, targets: () => [canvas], place, role: ROLE_VIEW });
    layer.onMessage(posMsg(9, 1, 1, true)); // позиція до форми — нічого не малюємо
    assert.equal(container.children.length, 0);
    layer.onMessage(pngShapeMsg(9, 16, 16, 2, 3));
    const img = container.children[0];
    assert.equal(img.style.display, 'block');
    // 1*2 - hot*scale: (2-4, 2-6)
    assert.equal(img.style.transform, 'translate(-2px,-4px) scale(2)');
    assert.equal(canvas.style.cursor, 'crosshair', 'глядачу локальний курсор не чіпаємо');
    layer.onMessage(posMsg(9, 1, 1, false));
    assert.equal(img.style.display, 'none');
    layer.destroy();
}

console.log('cursor-layer: ok');
