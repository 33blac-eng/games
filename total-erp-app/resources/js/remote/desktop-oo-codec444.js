// desktop-oo-codec444.js — Q2/F8 прототип: плеєр збирає, які 4:4:4-кодеки
// він може декодувати, і повідомляє хабу. Вибір робить хаб
// (internal/codec444.Negotiate), фолбек — H.264. Вимкнено за замовчуванням:
// без opts.enabled повертається [].
//
// C1 (WORLD-COMPARISON-2026 §1.3): медіа йде через RTCPeerConnection, тож
// можливості беремо з RTCRtpReceiver.getCapabilities('video'), а НЕ з WebCodecs
// VideoDecoder.isConfigSupported — це різні декодери, і їхні відповіді
// збігаються лише випадково. Підтримка у реальному Chrome НЕ ПЕРЕВІРЕНА.

export const CODEC444_CANDIDATES = [
    'av01.1.08M.08.0.000', // AV1 High (profile 1), 8 біт 4:4:4
    'vp09.01.10.08.03',    // VP9 profile 1, 8 біт 4:4:4
    'avc1.f4001f',         // H.264 High 4:4:4 Predictive (SDP profile-level-id=f4001f)
];

function fmtpParam(line, key) {
    const m = new RegExp('(?:^|;)\\s*' + key + '=([^;\\s]+)', 'i').exec(String(line || ''));
    return m ? m[1] : null;
}

// rtpVideoCaps — що браузер оголошує для ПРИЙОМУ відео через WebRTC:
// { h264: ['42e01f', 'f4001f', ...], vp9: [0, 1], av1: [0, 1] } (без повторів).
// null — API немає або він кинув виняток. env інжектується в тестах.
export function rtpVideoCaps(env = globalThis) {
    const R = env && env.RTCRtpReceiver;
    if (!R || typeof R.getCapabilities !== 'function') return null;
    let caps;
    try { caps = R.getCapabilities('video'); } catch (_) { return null; }
    if (!caps || !Array.isArray(caps.codecs)) return null;
    const out = { h264: [], vp9: [], av1: [] };
    const add = (arr, v) => { if (!arr.includes(v)) arr.push(v); };
    for (const c of caps.codecs) {
        const mt = String((c && c.mimeType) || '').toLowerCase();
        const f = c && c.sdpFmtpLine;
        if (mt === 'video/h264') {
            const p = fmtpParam(f, 'profile-level-id');
            if (p && /^[0-9a-f]{6}$/i.test(p)) add(out.h264, p.toLowerCase());
        } else if (mt === 'video/vp9') {
            const p = fmtpParam(f, 'profile-id');
            add(out.vp9, p == null ? 0 : Number(p) | 0); // без параметра — profile 0
        } else if (mt === 'video/av1') {
            const p = fmtpParam(f, 'profile');
            add(out.av1, p == null ? 0 : Number(p) | 0);
        }
    }
    return out;
}

// probeCodec444 — повертає масив підтримуваних рядків кодеків (у форматі
// CODEC444_CANDIDATES, який розуміє Go codec444). Async — заради сумісності
// з колишнім WebCodecs-API.
export async function probeCodec444(opts = {}, env = globalThis) {
    if (!opts.enabled) return [];
    const caps = rtpVideoCaps(env);
    if (!caps) return [];
    const out = [];
    if (caps.av1.includes(1)) out.push(CODEC444_CANDIDATES[0]);
    if (caps.vp9.includes(1)) out.push(CODEC444_CANDIDATES[1]);
    if (caps.h264.some((p) => p.startsWith('f4'))) out.push(CODEC444_CANDIDATES[2]);
    return out;
}

// chooseCodec444 — дзеркало Go Negotiate для відображення/тестів.
// 'h264-444' лише якщо явно є в prefer (типовий порядок його не містить).
export function chooseCodec444(enabled, codecs, prefer = ['av1-p1', 'vp9-p1']) {
    if (!enabled) return 'h264';
    const has = new Set();
    for (const c of codecs || []) {
        const s = String(c).trim().toLowerCase();
        if (s.startsWith('vp09.01.')) has.add('vp9-p1');
        else if (s.startsWith('av01.1.')) has.add('av1-p1');
        else if (s.startsWith('avc1.f4')) has.add('h264-444');
    }
    for (const p of prefer) if (has.has(p)) return p;
    return 'h264';
}
