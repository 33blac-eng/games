// Запуск: node resources/js/remote/__tests__/codec444.test.mjs
import assert from 'node:assert/strict';
import { probeCodec444, chooseCodec444 } from '../desktop-oo-codec444.js';

const mk = (ok) => ({ VideoDecoder: { isConfigSupported: async (c) => ({ supported: ok.includes(c.codec) }) } });

assert.deepEqual(await probeCodec444({}, mk(['vp09.01.10.08.03'])), [], 'вимкнено за замовчуванням');
assert.deepEqual(await probeCodec444({ enabled: true }, {}), [], 'без WebCodecs');
assert.deepEqual(await probeCodec444({ enabled: true }, mk(['vp09.01.10.08.03'])), ['vp09.01.10.08.03']);
const throws = { VideoDecoder: { isConfigSupported: async () => { throw new Error('no'); } } };
assert.deepEqual(await probeCodec444({ enabled: true }, throws), []);

assert.equal(chooseCodec444(false, ['vp09.01.10.08.03']), 'h264');
assert.equal(chooseCodec444(true, []), 'h264');
assert.equal(chooseCodec444(true, ['vp09.00.10.08']), 'h264');
assert.equal(chooseCodec444(true, ['vp09.01.10.08.03']), 'vp9-p1');
assert.equal(chooseCodec444(true, ['vp09.01.10.08.03', 'av01.1.08M.08.0.000']), 'av1-p1');
assert.equal(chooseCodec444(true, ['av01.1.08M.08.0.000'], ['vp9-p1']), 'h264');

console.log('codec444: OK');
