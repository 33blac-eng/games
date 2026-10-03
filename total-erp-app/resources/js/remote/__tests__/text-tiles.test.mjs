// Запуск: node resources/js/remote/__tests__/text-tiles.test.mjs
import assert from 'node:assert/strict';
import {
    parseTileMessage, createTileState, createTileOverlay,
    HEADER_SIZE, MAX_MESSAGE, TYPE_TILE, TYPE_INVALIDATE, TYPE_STILL, TYPE_KEEP, FORMAT_PNG,
} from '../oo-text-tiles.js';
import { containBox } from '../desktop-oo-webrtc.js';

// png — сигнатура PNG + IHDR(w,h) (+ tag-байт у кінці, якщо заданий).
function png(w, h, tag) {
    const b = new Uint8Array(33 + (tag === undefined ? 0 : 1));
    b.set([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 13, 0x49, 0x48, 0x44, 0x52]);
    const dv = new DataView(b.buffer);
    dv.setUint32(16, w); dv.setUint32(20, h);
    b.set([8, 6, 0, 0, 0], 24);
    if (tag !== undefined) b[33] = tag;
    return [...b];
}

// Будує повідомлення так само, як tools/oo-screen/internal/tiles/proto.go Encode.
function msg({ type = TYPE_TILE, epoch = 1, frame = 2, x = 64, y = 0, w = 64, h = 64,
    srcW = 1920, srcH = 1080, format = FORMAT_PNG, payload = png(w, h) } = {}) {
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

// Еталон із Go: tiles.Encode{Tile, epoch 7, frame 99, 64,128 64x32, 1920x1080, PNG, fakePNG(64,32,33)}.
const golden = Uint8Array.from([0x4f, 0x54, 0x01, 0x01, 0x07, 0, 0, 0, 0x63, 0, 0, 0, 0x40, 0, 0x80, 0,
    0x40, 0, 0x20, 0, 0x80, 0x07, 0x38, 0x04, 0x01, 0, 0, 0, 0x21, 0, 0, 0, ...png(64, 32)]);
{
    const m = parseTileMessage(golden.buffer);
    assert.equal(m.type, TYPE_TILE);
    assert.deepEqual([m.epoch, m.frame, m.x, m.y, m.w, m.h, m.srcW, m.srcH], [7, 99, 64, 128, 64, 32, 1920, 1080]);
    assert.deepEqual([...m.payload], png(64, 32));
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
    // PNG-бомба: 45 байт (32 заголовок + 13), IHDR обрізаний/65535×65535
    bomb45: msg({ payload: png(65535, 65535).slice(0, 13) }),
    ihdrMismatch: msg({ payload: png(65535, 65535) }),
    notPng: msg({ payload: new Array(33).fill(0) }),
    hugeSrc: msg({ srcW: 65535, srcH: 65535 }),
    srcOver: msg({ srcW: 16385 }),
    invWithPayload: msg({ type: TYPE_INVALIDATE, format: 0 }),
    tooBig: new Uint8Array(MAX_MESSAGE + 1),
    notBinary: 'OT',
};
assert.equal(msg({ payload: png(65535, 65535).slice(0, 13) }).length, 45);
assert.ok(parseTileMessage(msg({ srcW: 16384, srcH: 16384 })), 'max src');
for (const [k, v] of Object.entries(bad)) assert.equal(parseTileMessage(v), null, k);

// стан епох
{
    const s = createTileState();
    let a = s.accept(parseTileMessage(msg({ epoch: 1 })));
    assert.deepEqual([a.clear, a.draw], [true, true]);
    a = s.accept(parseTileMessage(msg({ epoch: 1, x: 128 })));
    assert.deepEqual([a.clear, a.draw], [false, true]);
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
        decode: async (p) => { const t = p[p.length - 1]; if (t === 9) await gate; return 'bmp' + t; },
    });
    ov.place(10, 20, 1000, 540);
    await ov.onMessage(msg({ epoch: 1, x: 64, y: 64, payload: png(64, 64, 1) }).buffer);
    assert.equal(canvas.width, 1920);
    assert.equal(canvas.style.left, '30px'); // (1000-960)/2 + 10
    assert.equal(canvas.style.width, '960px');
    assert.ok(ops.includes('draw:bmp1@64,64'));
    // тайл, що декодується, поки приходить invalidate, — не малюється
    const p = ov.onMessage(msg({ epoch: 1, payload: png(64, 64, 9) }).buffer);
    await ov.onMessage(inval(2).buffer);
    release();
    await p;
    assert.ok(!ops.includes('draw:bmp9@64,0'));
    assert.equal(ops[ops.length - 1], 'clear');
    // сміття ігнорується
    await ov.onMessage(new Uint8Array(5).buffer);
    // кадр відео іншої пропорції стирає тайли
    await ov.onMessage(msg({ epoch: 2, payload: png(64, 64, 1) }).buffer);
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
// TYPE_KEEP: еталон із Go (internal/tiles/keep_test.go keepGolden):
// tiles.Keep(9, 4, 1920, 1080, [{64,128,64,32}]).
const keepGolden = Uint8Array.from([0x4f, 0x54, 0x01, 0x04, 0x09, 0, 0, 0, 0x04, 0, 0, 0,
    0, 0, 0, 0, 0, 0, 0, 0, 0x80, 0x07, 0x38, 0x04, 0, 0, 0, 0, 0x08, 0, 0, 0,
    0x40, 0, 0x80, 0, 0x40, 0, 0x20, 0]);
{
    const m = parseTileMessage(keepGolden);
    assert.deepEqual([m.type, m.epoch, m.frame, m.srcW, m.srcH], [TYPE_KEEP, 9, 4, 1920, 1080]);
    assert.deepEqual(m.rects, [{ x: 64, y: 128, w: 64, h: 32 }]);
    const mutK = (i, v) => { const b = keepGolden.slice(); b[i] = v; return b; };
    for (const [k, v] of Object.entries({
        outside: (() => { const b = mutK(32, 0xF0); b[33] = 0x07; return b; })(),
        zeroW: mutK(36, 0),
        format: mutK(24, FORMAT_PNG),
        hdrX: mutK(12, 1),
        noSrc: (() => { const b = mutK(20, 0); b[21] = 0; return b; })(),
        partial: (() => { const b = new Uint8Array(keepGolden.length + 1); b.set(keepGolden); b[28] = 9; return b; })(),
    })) assert.equal(parseTileMessage(v), null, 'keep ' + k);
}
const keep = (epoch, rects, srcW = 1920, srcH = 1080) => {
    const p = [];
    for (const r of rects) p.push(r.x & 255, r.x >> 8, r.y & 255, r.y >> 8, r.w & 255, r.w >> 8, r.h & 255, r.h >> 8);
    return msg({ type: TYPE_KEEP, epoch, x: 0, y: 0, w: 0, h: 0, srcW, srcH, format: 0, payload: p });
};
// стан: тайли переживають invalidate; keep повертає перелічені, решту викидає
{
    const s = createTileState();
    s.accept(parseTileMessage(inval(1)));
    s.accept(parseTileMessage(keep(1, [])));
    s.accept(parseTileMessage(msg({ epoch: 1, x: 0 })));
    s.accept(parseTileMessage(msg({ epoch: 1, x: 64 })));
    assert.equal(s.held(), 2);
    s.accept(parseTileMessage(inval(2)));
    assert.equal(s.held(), 2);
    const a = s.accept(parseTileMessage(keep(2, [{ x: 64, y: 0, w: 64, h: 64 }])));
    assert.deepEqual(a.kept.map((e) => e.x), [64]);
    assert.deepEqual(a.dropped.map((e) => e.x), [0]);
    assert.equal(s.count(), 1);
    // інша геометрія: keep не повертає нічого
    s.accept(parseTileMessage(inval(3)));
    const b = s.accept(parseTileMessage(keep(3, [{ x: 64, y: 0, w: 64, h: 64 }], 1280, 1024)));
    assert.equal(b.kept.length, 0);
    assert.equal(s.held(), 0);
    // старий агент (без keep): тайл нової епохи викидає старі
    s.accept(parseTileMessage(inval(4)));
    s.accept(parseTileMessage(keep(4, [], 1920, 1080)));
    s.accept(parseTileMessage(msg({ epoch: 4, x: 0 })));
    s.accept(parseTileMessage(inval(5)));
    s.accept(parseTileMessage(msg({ epoch: 5, x: 128 })));
    assert.equal(s.held(), 1);
}
// оверлей: епізод 2 малює утриманий bmp без повторного декоду
{
    const ops = [];
    const closed = [];
    const canvas = {
        style: {}, width: 0, height: 0,
        getContext: () => ({
            clearRect: () => ops.push('clear'),
            drawImage: (b, x, y) => ops.push('draw:' + b.id + '@' + x + ',' + y),
        }),
    };
    let decodes = 0;
    const ov = createTileOverlay({
        doc: { createElement: () => canvas }, container: { appendChild() {} }, containBox,
        decode: async (p) => { decodes++; const id = 'bmp' + p[p.length - 1]; return { id, close: () => closed.push(id) }; },
    });
    await ov.onMessage(inval(1).buffer);
    await ov.onMessage(keep(1, []).buffer);
    await ov.onMessage(msg({ epoch: 1, x: 0, payload: png(64, 64, 1) }).buffer);
    await ov.onMessage(msg({ epoch: 1, x: 64, payload: png(64, 64, 2) }).buffer);
    assert.equal(decodes, 2);
    await ov.onMessage(inval(2).buffer);
    ops.length = 0;
    await ov.onMessage(keep(2, [{ x: 0, y: 0, w: 64, h: 64 }]).buffer);
    assert.deepEqual(ops, ['draw:bmp1@0,0']);
    assert.deepEqual(closed, ['bmp2']);
    await ov.onMessage(msg({ epoch: 2, x: 64, payload: png(64, 64, 3) }).buffer);
    assert.equal(decodes, 3);
    assert.deepEqual(ops, ['draw:bmp1@0,0', 'draw:bmp3@64,0']);
    // тайл, що декодується під час invalidate, утримується і вертається keep-ом
    let release;
    const gate = new Promise((r) => { release = r; });
    const ov2 = createTileOverlay({
        doc: { createElement: () => canvas }, container: { appendChild() {} }, containBox,
        decode: async () => { await gate; return { id: 'late', close() {} }; },
    });
    const p = ov2.onMessage(msg({ epoch: 1, x: 0 }).buffer);
    await ov2.onMessage(inval(2).buffer);
    release();
    await p;
    ops.length = 0;
    await ov2.onMessage(keep(2, [{ x: 0, y: 0, w: 64, h: 64 }]).buffer);
    assert.deepEqual(ops, ['draw:late@0,0']);
}
console.log('text-tiles: ok');
