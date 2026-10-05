// desktop-oo-codec444.js — Q2/F8 прототип: плеєр збирає, які 4:4:4-кодеки
// він може декодувати (WebCodecs VideoDecoder.isConfigSupported), і
// повідомляє хабу. Вибір робить хаб (internal/codec444.Negotiate), фолбек —
// H.264. Вимкнено за замовчуванням: без opts.enabled повертається [].
// Підтримка у реальному Chrome НЕ ПЕРЕВІРЕНА.

export const CODEC444_CANDIDATES = [
    'av01.1.08M.08.0.000', // AV1 High (profile 1), 8 біт 4:4:4
    'vp09.01.10.08.03',    // VP9 profile 1, 8 біт 4:4:4
];

// probeCodec444 — повертає масив підтримуваних рядків кодеків.
// env.VideoDecoder інжектується в тестах; помилки → кодек не підтримано.
export async function probeCodec444(opts = {}, env = globalThis) {
    if (!opts.enabled) return [];
    const VD = env && env.VideoDecoder;
    if (!VD || typeof VD.isConfigSupported !== 'function') return [];
    const out = [];
    for (const codec of CODEC444_CANDIDATES) {
        try {
            const r = await VD.isConfigSupported({ codec, codedWidth: 1920, codedHeight: 1080 });
            if (r && r.supported === true) out.push(codec);
        } catch (_) { /* не підтримано */ }
    }
    return out;
}

// chooseCodec444 — дзеркало Go Negotiate для відображення/тестів.
export function chooseCodec444(enabled, codecs, prefer = ['av1-p1', 'vp9-p1']) {
    if (!enabled) return 'h264';
    const has = new Set();
    for (const c of codecs || []) {
        const s = String(c).trim().toLowerCase();
        if (s.startsWith('vp09.01.')) has.add('vp9-p1');
        else if (s.startsWith('av01.1.')) has.add('av1-p1');
    }
    for (const p of prefer) if (has.has(p)) return p;
    return 'h264';
}
