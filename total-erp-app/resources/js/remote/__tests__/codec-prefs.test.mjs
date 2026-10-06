// Запуск: node --import ./__tests__/erp-stubs.mjs __tests__/codec-prefs.test.mjs
// PLAYER-QUALITY P-1: порядок H.264-профілів у offer-і глядача.
import assert from 'node:assert/strict';
import {
    h264ProfileRank, orderH264Profiles, orderCodecs, applyCodecPreferences,
} from '../desktop-oo-webrtc.js';

// Типовий набір RTCRtpReceiver.getCapabilities('video') Chrome (порядок браузера;
// точний список залежить від версії й платформи — тут лише форма).
const H = (fmtp) => ({ mimeType: 'video/H264', clockRate: 90000, sdpFmtpLine: fmtp });
const chromeCaps = [
    { mimeType: 'video/VP8', clockRate: 90000 },
    { mimeType: 'video/rtx', clockRate: 90000 },
    { mimeType: 'video/VP9', clockRate: 90000, sdpFmtpLine: 'profile-id=0' },
    H('level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f'),
    H('level-asymmetry-allowed=1;packetization-mode=0;profile-level-id=42001f'),
    H('level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f'),
    H('level-asymmetry-allowed=1;packetization-mode=0;profile-level-id=42e01f'),
    H('level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=4d001f'),
    H('level-asymmetry-allowed=1;packetization-mode=0;profile-level-id=4d001f'),
    H('level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=f4001f'),
    H('level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=64001f'),
    H('level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=640c1f'),
    { mimeType: 'video/AV1', clockRate: 90000 },
    { mimeType: 'video/red', clockRate: 90000 },
    { mimeType: 'video/ulpfec', clockRate: 90000 },
];
const plid = (c) => (/profile-level-id=([0-9a-f]+)/.exec(c.sdpFmtpLine || '') || [])[1];
const pm = (c) => (/packetization-mode=(\d)/.exec(c.sdpFmtpLine || '') || [])[1];

// ── ранги ────────────────────────────────────────────────────────────────────
assert.deepEqual(h264ProfileRank('packetization-mode=1;profile-level-id=4d001f'), [0, 0]);
assert.deepEqual(h264ProfileRank('profile-level-id=4D401F;packetization-mode=1'), [0, 0]); // регістр/порядок
assert.deepEqual(h264ProfileRank('packetization-mode=1;profile-level-id=42e01f'), [0, 1]);
assert.deepEqual(h264ProfileRank('packetization-mode=1;profile-level-id=42401f'), [0, 1]); // constraint_set1
assert.deepEqual(h264ProfileRank('packetization-mode=1;profile-level-id=42001f'), [0, 2]);
assert.deepEqual(h264ProfileRank('packetization-mode=1;profile-level-id=f4001f'), [0, 4]);
assert.deepEqual(h264ProfileRank('packetization-mode=1;profile-level-id=64001f'), [0, 5]);
assert.deepEqual(h264ProfileRank('packetization-mode=1;profile-level-id=640c1f'), [0, 5]);
assert.deepEqual(h264ProfileRank('packetization-mode=0;profile-level-id=4d001f'), [1, 0]);
assert.deepEqual(h264ProfileRank(''), [1, 3]);                 // pm відсутній = 0 за RFC 6184
assert.deepEqual(h264ProfileRank(undefined), [1, 3]);
assert.deepEqual(h264ProfileRank('packetization-mode=1;profile-level-id=zz'), [0, 3]);

// ── повний ланцюг applyCodecPreferences ─────────────────────────────────────
let applied = null;
const tx = { setCodecPreferences(list) { applied = list; } };
const R = { getCapabilities: (k) => (k === 'video' ? { codecs: chromeCaps } : null) };
assert.equal(applyCodecPreferences(tx, R, ['H264']), true);
// нічого не загублено і не задвоєно
assert.equal(applied.length, chromeCaps.length);
assert.deepEqual(new Set(applied), new Set(chromeCaps));
const h264 = applied.filter((c) => c.mimeType === 'video/H264');
// H.264 — першим блоком (F-13), усередині: Main pm1 першим
assert.ok(applied.slice(0, h264.length).every((c) => c.mimeType === 'video/H264'));
assert.equal(plid(applied[0]), '4d001f');
assert.equal(pm(applied[0]), '1');
// усі pm=1 раніше за всі pm=0
const lastPm1 = h264.map(pm).lastIndexOf('1');
const firstPm0 = h264.map(pm).indexOf('0');
assert.ok(lastPm1 < firstPm0);
// pm=1: Main → CB → Baseline → f4 → High (High ніколи не попереду Main/Baseline)
assert.deepEqual(h264.filter((c) => pm(c) === '1').map(plid), ['4d001f', '42e01f', '42001f', 'f4001f', '64001f', '640c1f']);
// High лишився в offer-і (старий агент 64002a домовляється точним збігом)
assert.ok(h264.some((c) => plid(c) === '64001f'));
// решта кодеків — у порядку браузера після H.264
assert.deepEqual(applied.slice(h264.length).map((c) => c.mimeType),
    ['video/VP8', 'video/rtx', 'video/VP9', 'video/AV1', 'video/red', 'video/ulpfec']);

// відкат: h264Profiles:false — порядок профілів браузера (як до P-1)
applyCodecPreferences(tx, R, ['H264'], { h264Profiles: false });
assert.equal(plid(applied[0]), '42001f');
assert.deepEqual(applied, orderCodecs(chromeCaps, ['H264']));

// orderH264Profiles не рухає не-H.264 слоти (prefer VP8 — H.264 лишається своїм блоком)
const vp8First = orderH264Profiles(orderCodecs(chromeCaps, ['VP8']));
assert.equal(vp8First[0].mimeType, 'video/VP8');
assert.equal(plid(vp8First.find((c) => c.mimeType === 'video/H264')), '4d001f');
// записи без fmtp (старі тести/браузери) — порядок збережено
const bare = [{ mimeType: 'video/H264', id: 1 }, { mimeType: 'video/H264', id: 2 }, { mimeType: 'video/VP8' }];
assert.deepEqual(orderH264Profiles(bare).map((c) => c.id), [1, 2, undefined]);
assert.deepEqual(orderH264Profiles(null), []);
// вхід не мутується
const copy = chromeCaps.slice();
orderH264Profiles(chromeCaps);
assert.deepEqual(chromeCaps, copy);

// setCodecPreferences кидає — сесія живе (false, без винятку)
assert.equal(applyCodecPreferences({ setCodecPreferences() { throw new Error('x'); } }, R, ['H264']), false);
assert.equal(applyCodecPreferences({}, R, ['H264']), false);

console.log('codec-prefs: ok');
