// Мікробенч гарячих шляхів плеєра (браузер, але чисті функції — міряємо в node).
// Запуск: node tools/oo-screen/bench/js-micro.bench.mjs  [--count 5]
// Для кожного кейсу — count прогонів, у кожному ~0.5 с циклу; друкує ns/op.
import { parseTileMessage } from '../../../total-erp-app/resources/js/remote/oo-text-tiles.js';
import { mapClientToRemote } from '../../../total-erp-app/resources/js/remote/desktop-oo-webrtc.js';
import { decodeCursorMessage } from '../../../total-erp-app/resources/js/remote/desktop-oo-cursor.js';

const countArg = process.argv.indexOf('--count');
const COUNT = countArg > 0 ? Number(process.argv[countArg + 1]) : 5;

const hex = (h) => Uint8Array.from(h.match(/../g).map((x) => parseInt(x, 16)));

// Тайл 64×64 PNG у 1080p кадрі: 32-байтний заголовок + ~2.5 КБ payload
// (середній розмір тайла з корпусу); IHDR збігається з w×h.
function tileMsg() {
    const payload = new Uint8Array(2500);
    payload.set([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 13, 0x49, 0x48, 0x44, 0x52,
        0, 0, 0, 64, 0, 0, 0, 64, 8, 2, 0, 0, 0]);
    const b = new Uint8Array(32 + payload.length);
    const dv = new DataView(b.buffer);
    b.set([0x4f, 0x54, 1, 1]);
    dv.setUint32(4, 7, true); dv.setUint32(8, 99, true);
    dv.setUint16(12, 640, true); dv.setUint16(14, 320, true);
    dv.setUint16(16, 64, true); dv.setUint16(18, 64, true);
    dv.setUint16(20, 1920, true); dv.setUint16(22, 1080, true);
    b[24] = 1;
    dv.setUint32(28, payload.length, true);
    b.set(payload, 32);
    return b.buffer;
}

const tile = tileMsg();
const posMsg = hex('4301010004030201feffffff2c01000080073804').buffer;
const rect = { left: 10, top: 50, width: 1600, height: 900 };

const cases = {
    parseTileMessage: () => parseTileMessage(tile),
    mapClientToRemote: (i) => mapClientToRemote(10 + (i % 1600), 50 + (i % 900), rect, 1920, 1080),
    decodeCursorPos: () => decodeCursorMessage(posMsg),
};

let sink;
function run(fn) {
    // Калібрування: підбираємо N на ~0.5 с.
    let n = 1000;
    for (;;) {
        const t0 = process.hrtime.bigint();
        for (let i = 0; i < n; i++) sink = fn(i);
        const dt = Number(process.hrtime.bigint() - t0);
        if (dt > 5e8) return dt / n;
        n *= dt < 5e7 ? 10 : 2;
    }
}

for (const [name, fn] of Object.entries(cases)) {
    if (!fn(1)) throw new Error(`${name}: returned null — fixture broken`);
    run(fn); // warm-up (JIT)
    for (let c = 0; c < COUNT; c++) {
        // Формат рядка benchstat-сумісний.
        console.log(`BenchmarkJS_${name}\t1\t${run(fn).toFixed(2)} ns/op`);
    }
}
if (sink === undefined) console.log('');
