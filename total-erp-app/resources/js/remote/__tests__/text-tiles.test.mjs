// Запуск: node resources/js/remote/__tests__/text-tiles.test.mjs
import assert from 'node:assert/strict';
import {
    parseTileMessage, createTileState, createTileOverlay,
    HEADER_SIZE, MAX_MESSAGE, TYPE_TILE, TYPE_INVALIDATE, TYPE_STILL, FORMAT_PNG,
} from '../oo-text-tiles.js';
import { containBox } from '../desktop-oo-webrtc.js';

// Будує повідомлення так само, як tools/oo-screen/internal/tiles/proto.go Encode.
function msg({ type = TYPE_TILE, epoch = 1, frame = 2, x = 64, y = 0, w = 64, h = 64,
    srcW = 1920, srcH = 1080, format = FORMAT_PNG, payload = [1, 2, 3] } = {}) {
    const b = new Uint8Array(HEADER_SIZE + payload.length);
    const dv = new DataView(b.buffer);
    b[0] = 0x4F; b[1] = 0x54; b[2] = 1; b[3] = type;
    dv.setUint32(4, epoch, true); dv.setUint32(8, frame, true);
    dv.setUint16(12, x, true); dv.setUint16(14, y, true);
    dv.setUint16(16, w, true); dv.setUint16(18, h, true);
    dv.setUint16(20, srcW, true); dv.setUint16(22, srcH, true);
    b[24] = format;
    dv.setUint32(28, payload.length, true);
    b.set(payload, HEADER_SIZE);
    return b;
}
const still = (epoch) => msg({ type: TYPE_STILL, epoch, w: 0, h: 0, x: 0, format: 0, payload: [] });
const inval = (epoch) => msg({ type: TYPE_INVALIDATE, epoch, w: 0, h: 0, x: 0, format: 0, payload: [] });

// Еталон із Go: tiles.Encode{Tile, epoch 7, frame 99, 64,128 64x32, 1920x1080, PNG, [1 2 3]}.
const golden = Uint8Array.from([0x4f, 0x54, 0x01, 0x01, 0x07, 0, 0, 0, 0x63, 0, 0, 0, 0x40, 0, 0x80, 0,
    0x40, 0, 0x20, 0, 0x80, 0x07, 0x38, 0x04, 0x01, 0, 0, 0, 0x03, 0, 0, 0, 1, 2, 3]);
{
    const m = parseTileMessage(golden.buffer);
    assert.equal(m.type, TYPE_TILE);
    assert.deepEqual([m.epoch, m.frame, m.x, m.y, m.w, m.h, m.srcW, m.srcH], [7, 99, 64, 128, 64, 32, 1920, 1080]);
    assert.deepEqual([...m.payload], [1, 2, 3]);
}
// Uint8Array-вид зі зсувом теж парситься
{
    const outer = new Uint8Array(golden.length + 5);
    outer.set(golden, 5);
    assert.equal(parseTileMessage(outer.subarray(5)).epoch, 7);
}
assert.equal(parseTileMessage(inval(4)).type, TYPE_INVALIDATE);

// відмови
const mut = (i, v) => { const b = golden.slice(); b[i] = v; return b; };
const bad = {
    short: golden.subarray(0, 10),
    magic: mut(0, 0),
    version: mut(2, 2),
    len: mut(28, 9),
    reserved: mut(25, 1),
    type: msg({ type: 9 }),
    format: msg({ format: 3 }),
    outside: msg({ x: 1900 }),
    zeroW: msg({ w: 0 }),
    huge: msg({ w: 300, srcW: 4000 }),
    invWithPayload: msg({ type: TYPE_INVALIDATE, format: 0 }),
    tooBig: new Uint8Array(MAX_MESSAGE + 1),
    notBinary: 'OT',
};
for (const [k, v] of Object.entries(bad)) assert.equal(parseTileMessage(v), null, k);

// стан епох
{
    const s = createTileState();
    let a = s.accept(parseTileMessage(msg({ epoch: 1 })));
    assert.deepEqual(a, { clear: true, draw: true });
    a = s.accept(parseTileMessage(msg({ epoch: 1, x: 128 })));
    assert.deepEqual(a, { clear: false, draw: true });
    assert.equal(s.count(), 2);
    a = s.accept(parseTileMessage(inval(2)));
    assert.deepEqual(a, { clear: true, draw: false });
    assert.ok(!s.isCurrent(1) && s.isCurrent(2));
    a = s.accept(parseTileMessage(msg({ epoch: 3 }))); // нова епоха без invalidate
    assert.equal(a.clear, true);
    assert.equal(s.onVideoSize(1280, 720), false); // та сама пропорція (скейл енкодера)
    assert.equal(s.onVideoSize(1280, 1024), true); // інший монітор
    assert.equal(s.count(), 0);
}

// оверлей на фейковому DOM: розміщення через containBox і порядок епох
{
    const ops = [];
    const canvas = {
        style: {}, width: 0, height: 0,
        getContext: () => ({
            clearRect: () => ops.push('clear'),
            drawImage: (b, x, y) => ops.push('draw:' + b + '@' + x + ',' + y),
        }),
    };
    const doc = { createElement: () => canvas };
    const container = { appendChild() {} };
    let release;
    const gate = new Promise((r) => { release = r; });
    const ov = createTileOverlay({
        doc, container, containBox,
        decode: async (p) => { if (p[0] === 9) await gate; return 'bmp' + p[0]; },
    });
    ov.place(10, 20, 1000, 540);
    await ov.onMessage(msg({ epoch: 1, x: 64, y: 64, payload: [1] }).buffer);
    assert.equal(canvas.width, 1920);
    assert.equal(canvas.style.left, '30px'); // (1000-960)/2 + 10
    assert.equal(canvas.style.width, '960px');
    assert.ok(ops.includes('draw:bmp1@64,64'));
    // тайл, що декодується, поки приходить invalidate, — не малюється
    const p = ov.onMessage(msg({ epoch: 1, payload: [9] }).buffer);
    await ov.onMessage(inval(2).buffer);
    release();
    await p;
    assert.ok(!ops.includes('draw:bmp9@64,0'));
    assert.equal(ops[ops.length - 1], 'clear');
    // сміття ігнорується
    await ov.onMessage(new Uint8Array(5).buffer);
    // кадр відео іншої пропорції стирає тайли
    await ov.onMessage(msg({ epoch: 2, payload: [1] }).buffer);
    ops.length = 0;
    ov.onVideoFrame(1280, 1024);
    assert.deepEqual(ops, ['clear']);
}
// TYPE_STILL: розбір і відмови
{
    // Еталон із Go: tiles.Still(9, 4).
    const g = Uint8Array.from([0x4f, 0x54, 0x01, 0x03, 0x09, 0, 0, 0, 0x04, 0, 0, 0,
        0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0]);
    const m = parseTileMessage(g);
    assert.deepEqual([m.type, m.epoch, m.frame], [TYPE_STILL, 9, 4]);
    assert.equal(parseTileMessage(msg({ type: TYPE_STILL, format: 0 })), null); // з payload
}

// кредити still: без анонсів у сесії — кадри тайли не ховають (старий агент)
{
    const s = createTileState();
    s.accept(parseTileMessage(msg({ epoch: 1 })));
    assert.equal(s.onFrames(5), null);
}
// з анонсами: keepalive з кредитом — тайли лишаються; кадр без кредиту — сховати;
// запізнілий анонс повертає; анонс чужої епохи — ігнор
{
    const s = createTileState();
    s.accept(parseTileMessage(inval(1)));
    assert.deepEqual(s.accept(parseTileMessage(still(1))), { clear: false, draw: false, show: true });
    // тайлів ще нема: кадр readback-у їсть кредит, але не в мінус
    assert.equal(s.onFrames(2), null);
    assert.equal(s.credits(), 0);
    s.accept(parseTileMessage(msg({ epoch: 1 })));
    s.accept(parseTileMessage(still(1)));
    assert.equal(s.onFrames(1), null);          // keepalive з анонсом
    assert.equal(s.onFrames(1), 'hide');        // змінений кадр раніше за invalidate
    assert.equal(s.accept(parseTileMessage(still(1))).show, true); // ні, це був запізнілий анонс
    assert.equal(s.accept(parseTileMessage(still(7))).show, undefined);
    assert.equal(s.onFrames(0), null);
    assert.deepEqual(s.accept(parseTileMessage(inval(2))), { clear: true, draw: false });
    assert.equal(s.credits(), 0);
}
// оверлей: rVFC presentedFrames → visibility
{
    const canvas = { style: {}, width: 0, height: 0, getContext: () => ({ clearRect() {}, drawImage() {} }) };
    const ov = createTileOverlay({
        doc: { createElement: () => canvas }, container: { appendChild() {} }, containBox,
        decode: async () => 'bmp',
    });
    await ov.onMessage(inval(1).buffer);
    await ov.onMessage(still(1).buffer);
    ov.onVideoFrame(1920, 1080, 100);           // кадр readback-у
    await ov.onMessage(msg({ epoch: 1 }).buffer);
    await ov.onMessage(still(1).buffer);
    ov.onVideoFrame(1920, 1080, 101);           // keepalive
    assert.equal(ov.isHidden(), false);
    ov.onVideoFrame(1920, 1080, 101);           // той самий presentedFrames — не кадр
    assert.equal(ov.isHidden(), false);
    ov.onVideoFrame(1920, 1080, 102);           // кадр без анонсу
    assert.equal(ov.isHidden(), true);
    assert.equal(canvas.style.visibility, 'hidden');
    await ov.onMessage(inval(2).buffer);         // invalidate: стерто й знову видимо
    assert.equal(ov.isHidden(), false);
    assert.equal(canvas.style.visibility, '');
}
console.log('text-tiles: ok');
