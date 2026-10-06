// Запуск: node resources/js/remote/__tests__/codec444.test.mjs
import assert from 'node:assert/strict';
import { probeCodec444, chooseCodec444, rtpVideoCaps } from '../desktop-oo-codec444.js';
import { offerBody } from '../desktop-oo-webrtc.js';

// C1: можливості — з RTCRtpReceiver.getCapabilities('video'), не з WebCodecs.
const chrome = { codecs: [
    { mimeType: 'video/VP8' },
    { mimeType: 'video/rtx', sdpFmtpLine: 'apt=96' },
    { mimeType: 'video/H264', sdpFmtpLine: 'level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f' },
    { mimeType: 'video/H264', sdpFmtpLine: 'level-asymmetry-allowed=1;packetization-mode=0;profile-level-id=42e01f' },
    { mimeType: 'video/H264', sdpFmtpLine: 'level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=F4001F' },
    { mimeType: 'video/VP9', sdpFmtpLine: 'profile-id=0' },
    { mimeType: 'video/VP9', sdpFmtpLine: 'profile-id=1' },
    { mimeType: 'video/AV1', sdpFmtpLine: 'level-idx=5;profile=1;tier=0' },
    { mimeType: 'video/AV1' },
] };
const rtp = (caps) => ({ RTCRtpReceiver: { getCapabilities: (k) => (k === 'video' ? caps : null) } });

assert.deepEqual(rtpVideoCaps(rtp(chrome)), { h264: ['42e01f', 'f4001f'], vp9: [0, 1], av1: [1, 0] });
assert.equal(rtpVideoCaps({}), null, 'без API');
assert.equal(rtpVideoCaps({ RTCRtpReceiver: { getCapabilities: () => { throw new Error('x'); } } }), null);

assert.deepEqual(await probeCodec444({}, rtp(chrome)), [], 'вимкнено за замовчуванням');
assert.deepEqual(await probeCodec444({ enabled: true }, {}), [], 'без getCapabilities');
assert.deepEqual(await probeCodec444({ enabled: true }, rtp(chrome)),
    ['av01.1.08M.08.0.000', 'vp09.01.10.08.03', 'avc1.f4001f']);
// WebCodecs каже «так», а WebRTC — ні: довіряємо WebRTC.
const wcOnly = { VideoDecoder: { isConfigSupported: async () => ({ supported: true }) },
    ...rtp({ codecs: [{ mimeType: 'video/VP9', sdpFmtpLine: 'profile-id=0' }] }) };
assert.deepEqual(await probeCodec444({ enabled: true }, wcOnly), []);

assert.equal(chooseCodec444(false, ['vp09.01.10.08.03']), 'h264');
assert.equal(chooseCodec444(true, []), 'h264');
assert.equal(chooseCodec444(true, ['vp09.00.10.08']), 'h264');
assert.equal(chooseCodec444(true, ['vp09.01.10.08.03']), 'vp9-p1');
assert.equal(chooseCodec444(true, ['vp09.01.10.08.03', 'av01.1.08M.08.0.000']), 'av1-p1');
assert.equal(chooseCodec444(true, ['av01.1.08M.08.0.000'], ['vp9-p1']), 'h264');
assert.equal(chooseCodec444(true, ['avc1.f4001f']), 'h264', 'h264-444 лише за явним prefer');
assert.equal(chooseCodec444(true, ['avc1.f4001f'], ['h264-444']), 'h264-444');

// Звіт хабу: поле caps лише коли передано (config.reportCaps).
assert.equal(offerBody('s', 't', 0, null), '{"sdp":"s","ticket":"t"}');
assert.equal(offerBody('s', 't', 0, { h264: ['f4001f'], vp9: [], av1: [] }),
    '{"sdp":"s","ticket":"t","caps":{"h264":["f4001f"],"vp9":[],"av1":[]}}');
assert.equal(offerBody('s', 't', 2, { h264: [], vp9: [1], av1: [] }),
    '{"sdp":"s","ticket":"t","monitor":2,"caps":{"h264":[],"vp9":[1],"av1":[]}}');

console.log('codec444: OK');
