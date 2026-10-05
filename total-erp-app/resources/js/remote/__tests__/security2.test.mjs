// Повторний аудит oo-screen (#44, #45): PNG-бомби й межі в плеєрі.
// Запуск: node resources/js/remote/__tests__/security2.test.mjs
import assert from 'node:assert/strict';
import { parseTileMessage, HEADER_SIZE, TYPE_TILE, FORMAT_PNG } from '../oo-text-tiles.js';
import { decodeCursorMessage, pngHeaderMatches } from '../desktop-oo-cursor.js';

function png(w, h) {
    const b = new Uint8Array(33);
    b.set([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 13, 0x49, 0x48, 0x44, 0x52]);
    const dv = new DataView(b.buffer);
    dv.setUint32(16, w); dv.setUint32(20, h);
    return b;
}

function tile({ x = 0, y = 0, w = 64, h = 64, srcW = 1920, srcH = 1080, payload = png(w, h) } = {}) {
    const b = new Uint8Array(HEADER_SIZE + payload.length);
    const dv = new DataView(b.buffer);
    b[0] = 0x4F; b[1] = 0x54; b[2] = 1; b[3] = TYPE_TILE;
    dv.setUint16(12, x, true); dv.setUint16(14, y, true);
    dv.setUint16(16, w, true); dv.setUint16(18, h, true);
    dv.setUint16(20, srcW, true); dv.setUint16(22, srcH, true);
    b[24] = FORMAT_PNG;
    dv.setUint32(28, payload.length, true);
    b.set(payload, HEADER_SIZE);
    return b;
}

assert.ok(parseTileMessage(tile()), 'валідний тайл');
assert.equal(parseTileMessage(tile({ payload: png(65535, 65535) })), null, 'IHDR-бомба');
assert.equal(parseTileMessage(tile({ payload: png(64, 65) })), null, 'IHDR на 1 більший');
assert.equal(parseTileMessage(tile({ payload: png(64, 64).subarray(0, 28) })), null, 'обрізаний IHDR');
assert.equal(parseTileMessage(tile({ x: 65535 - 10, srcW: 65535 })), null, 'srcW > MAX_SRC_SIDE');
assert.equal(parseTileMessage(tile({ x: 1900 })), null, 'тайл за межами джерела');
assert.equal(parseTileMessage(tile({ x: 65500, srcW: 16384 })), null, 'x+w переповнення');
const pl = tile(); new DataView(pl.buffer).setUint32(28, 1 << 30, true);
assert.equal(parseTileMessage(pl), null, 'len не збігається');

function shape(w, h, payload) {
    const b = new Uint8Array(16 + payload.length);
    const dv = new DataView(b.buffer);
    b[0] = 0x43; b[1] = 2; b[2] = 1; // magic, KIND_SHAPE, FORMAT_PNG
    dv.setUint32(4, 7, true); dv.setUint16(8, w, true); dv.setUint16(10, h, true);
    b.set(payload, 16);
    return b;
}
// Курсор (cursorproto).
const goodShape = shape(32, 32, png(32, 32));
assert.ok(decodeCursorMessage(goodShape), "валідна форма курсора");
{
    assert.equal(decodeCursorMessage(shape(32, 32, png(65535, 65535))), null, 'курсор: IHDR-бомба');
    assert.equal(decodeCursorMessage(shape(300, 300, png(300, 300))), null, 'курсор > 256');
}
assert.equal(pngHeaderMatches(png(10, 10), 10, 10), true);
assert.equal(pngHeaderMatches(png(10, 10), 10, 11), false);
assert.equal(pngHeaderMatches(new Uint8Array(4), 0, 0), false);
console.log('security2: ok');
