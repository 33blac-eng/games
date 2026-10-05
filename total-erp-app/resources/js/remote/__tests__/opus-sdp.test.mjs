// Запуск: node resources/js/remote/__tests__/opus-sdp.test.mjs
import assert from 'node:assert/strict';
import { opusStereoSdp } from '../desktop-oo-webrtc.js';

// Типовий offer Chrome: opus без stereo, плюс red і PCMU.
const chromeOffer = [
    'v=0',
    'm=audio 9 UDP/TLS/RTP/SAVPF 111 63 0',
    'a=rtpmap:111 opus/48000/2',
    'a=fmtp:111 minptime=10;useinbandfec=1',
    'a=rtpmap:63 red/48000/2',
    'a=fmtp:63 111/111',
    'a=rtpmap:0 PCMU/8000',
    '',
].join('\r\n');

const out = opusStereoSdp(chromeOffer);
assert.ok(out.includes('a=fmtp:111 minptime=10;useinbandfec=1;stereo=1;sprop-stereo=1\r\n'));
assert.ok(out.includes('a=fmtp:63 111/111\r\n'), 'red не чіпаємо');
assert.equal(opusStereoSdp(out), out, 'ідемпотентно');

// Уже стерео — без дублів.
const already = 'a=rtpmap:111 opus/48000/2\r\na=fmtp:111 stereo=1;sprop-stereo=0\r\n';
assert.equal(opusStereoSdp(already), already);

// Без opus — без змін; не рядок — як є.
const video = 'm=video 9 UDP/TLS/RTP/SAVPF 102\r\na=fmtp:102 packetization-mode=1\r\n';
assert.equal(opusStereoSdp(video), video);
assert.equal(opusStereoSdp(undefined), undefined);

console.log('opus-sdp: OK');
