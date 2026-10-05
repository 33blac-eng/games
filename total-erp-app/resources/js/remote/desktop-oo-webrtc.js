// desktop-oo-webrtc.js — переможець bake-off (кандидат A): OO-шар поверх Mesh
// на WebRTC замість WebTransport+WebCodecs.
//
// Що ЛИШАЄТЬСЯ спільним із desktop-oo.js (імпортуємо, НЕ дублюємо):
//   • createOoSession — generation token + обидва сторожі (8с на перший кадр,
//     3с frame-age) + атомарний одноразовий фолбек;
//   • meshCall/PAUSE-UNPAUSE-REFRESH — duck-typing по обгортці Mesh;
//   • та сама state-машина connecting/live/fallback/closed.
//
// Що ІНШЕ: замість worker+OffscreenCanvas+WebCodecs — overlay <video> і
// RTCPeerConnection recvonly. Наслідки, які тут враховані:
//   1. Декодує сам браузер, тож preflight WebCodecs не потрібен — але й
//      «декодер не тягне» ми дізнаємось лише за відсутністю кадрів. Тому
//      паузу Mesh ставимо ЛИШЕ ПІСЛЯ успішного SDP-обміну: до відповіді
//      hub-а Mesh лишається живим, і зрив сигналізації не коштує чорного
//      екрана.
//   2. У WebRTC немає «події кадру» — є rVFC. Він і є джерелом для
//      noteFrame(); там, де rVFC недоступний, беремо 'loadeddata' як перший
//      кадр і далі підстраховуємось timeupdate, інакше frame-age watchdog
//      завалив би живу сесію на браузері без rVFC.
//   3. <video> — НЕ input-поверхня. pointer-events:none обов'язковий, як і в
//      canvas-версії: інакше шар з'їдає mousedown і Mesh «зависає».

import {
    createOoSession,
    meshCall,
    OO_STATE_CONNECTING,
    OO_STATE_LIVE,
    OO_STATE_FALLBACK,
    OO_STATE_CLOSED,
    MESH_PAUSE_NAMES,
    MESH_UNPAUSE_NAMES,
    MESH_REFRESH_NAMES,
    findMeshCanvas,
} from './desktop-oo.js';
import { createTileOverlay, TILES_LABEL } from './oo-text-tiles.js';
import { INPUT_CHANNEL_LABEL, inputEnabledFor, createInputSender, attachInputDom } from './desktop-oo-input.js';
import { applyLowLatency, createStatsOverlay } from './desktop-oo-stats.js';
import {
    CURSOR_CHANNEL_LABEL,
    createCursorLayer,
    resolveCursorRole,
} from './desktop-oo-cursor.js';

export { OO_STATE_CONNECTING, OO_STATE_LIVE, OO_STATE_FALLBACK, OO_STATE_CLOSED };

const DEFAULT_GEOMETRY_MS = 10000;
const DEFAULT_OFFER_TIMEOUT_MS = 8000;
// §MAJOR-6: 'disconnected' у WebRTC транзієнтний (перемикання мережі, коротка
// втрата ICE) і часто сам відновлюється. Даємо йому grace, і лише якщо після
// нього все ще погано — фолбек. 'failed' — остаточний, без grace.
const DEFAULT_DISCONNECT_GRACE_MS = 4000;
// §MAJOR-7: у прихованій вкладці rVFC тротлиться, тож поки hidden — «підживлюємо»
// frame-age цим кроком, щоб сторож не завалив живу сесію хибним фолбеком.
const DEFAULT_HIDDEN_KEEPALIVE_MS = 1000;

// ─────────────────────────────────────────────────────────────────────────────
// ЧИСТІ ХЕЛПЕРИ (без DOM/RTC) — рівно їх покриває desktop-oo-webrtc.test.mjs.
// ─────────────────────────────────────────────────────────────────────────────

/**
 * combineAbortSignals — §MAJOR-5: ОДИН signal, що абортиться і з teardown, і з
 * таймауту. Раніше fetch слухав лише AbortSignal.timeout() окремо від teardown,
 * тож при знищенні шару застарілий offer-fetch переживав teardown.
 *
 * Сучасний шлях — AbortSignal.any([teardown, AbortSignal.timeout(ms)]).
 * Фолбек для старих браузерів — власний контролер + ручний setTimeout.
 *
 * @returns {{signal: AbortSignal, cancel: Function}}
 */
export function combineAbortSignals(teardownSignal, timeoutMs, env) {
    const e = env || {};
    // 'any' in e / 'timeout' in e — щоб тест міг ЯВНО вимкнути сучасний шлях
    // (null), а не тільки не задати його (undefined → беремо глобальний).
    const anyFn = ('any' in e) ? e.any : (typeof AbortSignal !== 'undefined' ? AbortSignal.any : null);
    const timeoutFn = ('timeout' in e) ? e.timeout : (typeof AbortSignal !== 'undefined' ? AbortSignal.timeout : null);

    if (typeof anyFn === 'function' && typeof timeoutFn === 'function') {
        const parts = [];
        if (teardownSignal) parts.push(teardownSignal);
        parts.push(timeoutFn.call(null, timeoutMs));
        return { signal: anyFn.call(null, parts), cancel: () => {} };
    }

    // Фолбек: ручний контролер аборту з таймауту АБО з teardown.
    const Ctor = e.Controller || (typeof AbortController !== 'undefined' ? AbortController : null);
    if (!Ctor) return { signal: teardownSignal || undefined, cancel: () => {} };
    const ctrl = new Ctor();
    const setT = e.setTimeout || setTimeout;
    const clrT = e.clearTimeout || clearTimeout;
    const timer = setT(() => { try { ctrl.abort(); } catch (x) { /* ignore */ } }, timeoutMs);
    let onAbort = null;
    if (teardownSignal) {
        if (teardownSignal.aborted) { try { ctrl.abort(); } catch (x) { /* ignore */ } }
        else {
            onAbort = () => { try { ctrl.abort(); } catch (x) { /* ignore */ } };
            teardownSignal.addEventListener('abort', onAbort);
        }
    }
    return {
        signal: ctrl.signal,
        cancel: () => {
            clrT(timer);
            if (teardownSignal && onAbort) teardownSignal.removeEventListener('abort', onAbort);
        },
    };
}

/**
 * createDisconnectGrace — §MAJOR-6: grace-таймер для WebRTC connectionState.
 *   • 'failed'/'closed'      → фолбек негайно (остаточний стан);
 *   • 'disconnected'         → запускаємо grace; фолбек лише коли він добіг,
 *                              тобто відновлення так і не сталося;
 *   • 'connected'/'completed'→ скасовує grace (сесія відновилась мовчки).
 *
 * @returns {{note: Function, cancel: Function, pending: Function}}
 */
export function createDisconnectGrace(o) {
    const opts = o || {};
    const graceMs = opts.graceMs || DEFAULT_DISCONNECT_GRACE_MS;
    const onFallback = opts.onFallback || (() => {});
    const setT = opts.setTimeout || setTimeout;
    const clrT = opts.clearTimeout || clearTimeout;
    let timer = null;

    function clear() { if (timer !== null) { clrT(timer); timer = null; } }

    return {
        note(state) {
            if (state === 'failed' || state === 'closed') { clear(); onFallback('pc-' + state); return; }
            if (state === 'disconnected') {
                if (timer !== null) return; // grace уже йде — не перезапускаємо
                timer = setT(() => { timer = null; onFallback('pc-disconnected-grace'); }, graceMs);
                return;
            }
            if (state === 'connected' || state === 'completed') { clear(); }
        },
        cancel: clear,
        pending: () => timer !== null,
    };
}

/**
 * createHiddenFrameKeepalive — §MAJOR-7: поки вкладка hidden, frame-age watchdog
 * тротлиться (rVFC не викликається у фоні) і завалив би живу сесію. Поки hidden
 * — крокаємо keepFresh() (=session.noteFrame), тож age не старіє; на поверненні
 * у видимість зупиняємось і робимо один свіжий мазок, щоб рахунок пішов від тепер.
 *
 * @returns {{onVisibility: Function, active: Function, stop: Function}}
 */
export function createHiddenFrameKeepalive(o) {
    const opts = o || {};
    const isHidden = opts.isHidden || (() => false);
    const keepFresh = opts.keepFresh || (() => {});
    const setI = opts.setInterval || setInterval;
    const clrI = opts.clearInterval || clearInterval;
    const intervalMs = opts.intervalMs || DEFAULT_HIDDEN_KEEPALIVE_MS;
    let timer = null;

    function stop() { if (timer !== null) { clrI(timer); timer = null; } }

    function onVisibility() {
        if (isHidden()) {
            if (timer === null) { keepFresh(); timer = setI(() => keepFresh(), intervalMs); }
        } else {
            stop();
            keepFresh(); // повернулись у видимість — рахунок frame-age від тепер
        }
    }

    return { onVisibility, active: () => timer !== null, stop };
}

// ─────────────────────────────────────────────────────────────────────────────
// ЧИСТА ЛОГІКА ПЕРЕМИКАЧА (без DOM, без RTC) — рівно це покриває
// desktop-oo-webrtc.test.mjs.
// ─────────────────────────────────────────────────────────────────────────────

export const MODE_MESH = 'mesh';
export const MODE_OO = 'oo';
export const MODE_AUTO = 'auto';

export const MODES = [MODE_MESH, MODE_OO, MODE_AUTO];

export const OO_LOST_BANNER = 'OO втрачено, перемкнено на Mesh';

export function normalizeMode(mode, fallback) {
    return MODES.indexOf(mode) >= 0 ? mode : (fallback || MODE_MESH);
}

/**
 * createModeController — ЧИСТА state-машина перемикача «Mesh / OO / Авто».
 *
 * Різниця між OO і Авто — НЕ в транспорті, а рівно в одному: чи чесно
 * сказати людині, що OO помер. У режимі «OO» власник порівнює наживо, і
 * мовчазний фолбек зробив би порівняння брехливим («дивись, OO працює» — а
 * то вже Mesh). У «Авто» фолбек мовчазний за задумом: це робочий режим.
 *
 * @param {string}   o.initialMode
 * @param {Function} o.startOo()          підняти OO-шар (DOM-сторона)
 * @param {Function} o.stopOo(reason)     знищити OO-шар
 * @param {Function} o.resumeMesh()       зняти Mesh з паузи (ідемпотентно)
 * @param {Function} o.showBanner(text)   чесний банер (лише режим OO)
 * @param {Function} o.hideBanner()
 * @param {Function} o.log(message)       журнал консолі
 * @param {Function} o.persist(mode)      збереження вибору (sessionStorage)
 */
export function createModeController(o) {
    const opts = o || {};
    const startOo = opts.startOo || (() => {});
    const stopOo = opts.stopOo || (() => {});
    const resumeMesh = opts.resumeMesh || (() => {});
    const showBanner = opts.showBanner || (() => {});
    const hideBanner = opts.hideBanner || (() => {});
    const log = opts.log || (() => {});
    const persist = opts.persist || (() => {});
    const onRender = opts.onRender || (() => {});

    // Фолбек саме на Mesh: невідоме значення (сміття в sessionStorage, друкарська
    // помилка в config) НЕ має мовчки вмикати експериментальний транспорт.
    let mode = normalizeMode(opts.initialMode, MODE_MESH);
    // ooWanted — чи МАЄ зараз існувати OO-шар. Відрізняється від mode саме
    // після фолбеку в режимі «Авто»: вибір лишається 'auto' (кнопка
    // підсвічена), але шару вже нема — інакше повторний клік по «Авто» не
    // піднімав би сесію заново.
    let ooWanted = false;
    let destroyed = false;

    function render() {
        try { onRender(mode, ooWanted); } catch (e) { /* журнал не блокер */ }
    }

    function applyMesh(reason) {
        if (ooWanted) { ooWanted = false; try { stopOo(reason); } catch (e) { /* ignore */ } }
        try { resumeMesh(); } catch (e) { /* ignore */ }
    }

    function applyOo() {
        if (ooWanted) return;
        ooWanted = true;
        try { startOo(); } catch (e) { /* ignore */ }
    }

    const api = {
        mode: () => mode,
        ooWanted: () => ooWanted,

        /** setMode — вибір людини АБО автоперемикання (silent:true). */
        setMode(next, o2) {
            if (destroyed) return mode;
            const opt = o2 || {};
            const want = normalizeMode(next, mode);
            const changed = want !== mode;
            mode = want;
            if (opt.persist !== false) persist(mode);
            if (!opt.silent && changed) log(`Режим екрана: ${labelFor(mode)}`);
            if (mode === MODE_MESH) {
                hideBanner();
                applyMesh('mode-mesh');
            } else {
                hideBanner();
                applyOo();
            }
            render();
            return mode;
        },

        /** start — початкове застосування вибраного режиму (після відкриття вкладки). */
        start() {
            if (destroyed) return mode;
            if (mode === MODE_MESH) applyMesh('start-mesh');
            else applyOo();
            render();
            return mode;
        },

        /**
         * noteOoState — стан OO-шару з onStateChange.
         * fallback/closed при ooWanted = OO помер.
         */
        noteOoState(state, reason) {
            if (destroyed) return mode;
            log(`OO: ${state}${reason ? ' (' + reason + ')' : ''}`);
            if (state !== OO_STATE_FALLBACK && state !== OO_STATE_CLOSED) { render(); return mode; }
            if (!ooWanted) { render(); return mode; }
            // Шар уже сам повернув Mesh з паузи (атомарний фолбек у
            // createOoLayer), але resumeMesh ідемпотентний — зайвий виклик
            // дешевший за застиглий екран, якщо шар помер ДО unpause.
            ooWanted = false;
            try { resumeMesh(); } catch (e) { /* ignore */ }
            if (mode === MODE_OO) {
                // ЧЕСНО: власник порівнює наживо, він мусить знати, що дивиться вже Mesh.
                showBanner(OO_LOST_BANNER);
                log(OO_LOST_BANNER + (reason ? ' — ' + reason : ''));
                mode = MODE_MESH;
                persist(mode);
            }
            // MODE_AUTO — мовчки: ні банера, ні зміни підсвітки.
            render();
            return mode;
        },

        destroy() {
            if (destroyed) return;
            destroyed = true;
            if (ooWanted) { ooWanted = false; try { stopOo('destroy'); } catch (e) { /* ignore */ } }
        },
    };
    return api;
}

export function labelFor(mode) {
    if (mode === MODE_MESH) return 'Mesh';
    if (mode === MODE_OO) return 'OO';
    return 'Авто';
}

// ─────────────────────────────────────────────────────────────────────────────
// N5: ICE-конфіг для суворих офісних firewall-ів (лише TCP 443)
// ─────────────────────────────────────────────────────────────────────────────
//
// Хаб сам є медіа-кінцем, тож головний запасний шлях — ICE-TCP на 443 хаба
// (OO_SCREEN_ICE_TCP_ADVERTISE_PORT): для нього браузеру НЕ потрібен жоден
// iceServer, кандидат приходить в answer-і. iceServers із config/ERP
// потрібні для другого рубежу — TURN-TLS (turns:host:443?transport=tcp) там,
// де DPI пропускає на 443 лише справжній TLS.
//
// UDP завжди має перевагу без жодної логіки з нашого боку: ICE дає host/UDP
// найвищий пріоритет, TCP-host нижчий, relay — найнижчий, і пари
// перевіряються паралельно. Тому iceTransportPolicy лишається 'all' —
// 'relay' вимкнув би UDP і в офісі, де він є. Тут лише чистимо вхід:
// невалідний запис (TURN без облікових даних, чужа схема) з ERP не має
// ламати конструктор RTCPeerConnection — той кидає на ВСЬОМУ списку.

const ICE_URL_RE = /^(stun|stuns|turn|turns):[^\s]+$/i;

// opusStereoSdp — F1 (Opus 48 кГц стерео). Chrome за замовчуванням оголошує
// Opus без stereo=1, а декодер приймача створюється з кількістю каналів із
// цього fmtp — тобто без правки стерео від хаба зводиться в моно. Дописуємо
// stereo=1;sprop-stereo=1 у fmtp кожного opus-PT нашого ж offer-а. Інші
// рядки не чіпаємо; повторний виклик нічого не змінює.
export function opusStereoSdp(sdp) {
    if (typeof sdp !== 'string') return sdp;
    const pts = [];
    const re = /^a=rtpmap:(\d+) opus\/48000/gim;
    let m;
    while ((m = re.exec(sdp)) !== null) pts.push(m[1]);
    if (!pts.length) return sdp;
    return sdp.split(/\r\n/).map((line) => {
        const f = /^a=fmtp:(\d+) (.*)$/.exec(line);
        if (!f || !pts.includes(f[1])) return line;
        let params = f[2];
        if (!/(^|;)\s*stereo=/.test(params)) params += ';stereo=1';
        if (!/(^|;)\s*sprop-stereo=/.test(params)) params += ';sprop-stereo=1';
        return 'a=fmtp:' + f[1] + ' ' + params;
    }).join('\r\n');
}

// iceUrlRank — порядок у списку: UDP-варіанти першими, turns (TLS) останнім.
// На пріоритет кандидатів це не впливає (його рахує ICE), але Chrome опитує
// сервери по черзі, і так дешеві запити йдуть раніше.
function iceUrlRank(u) {
    const s = u.toLowerCase();
    if (s.startsWith('stun')) return 0;
    if (s.startsWith('turns:')) return 3;
    if (s.includes('transport=tcp')) return 2;
    return 1;
}

/**
 * normalizeIceServers — RTCIceServer[] з config/ERP, придатний для
 * RTCPeerConnection: лише stun/stuns/turn/turns-URL, TURN — лише з
 * username+credential, сортування UDP → TCP → TLS. Не масив → [].
 */
export function normalizeIceServers(list) {
    if (!Array.isArray(list)) return [];
    const out = [];
    for (const s of list) {
        if (!s || typeof s !== 'object') continue;
        const urls = (Array.isArray(s.urls) ? s.urls : [s.urls])
            .filter((u) => typeof u === 'string' && ICE_URL_RE.test(u.trim()))
            .map((u) => u.trim());
        const turn = urls.filter((u) => /^turns?:/i.test(u));
        const stun = urls.filter((u) => /^stuns?:/i.test(u));
        const hasCreds = typeof s.username === 'string' && s.username !== ''
            && typeof s.credential === 'string' && s.credential !== '';
        const keep = hasCreds ? stun.concat(turn) : stun;
        if (!keep.length) continue;
        keep.sort((a, b) => iceUrlRank(a) - iceUrlRank(b));
        const e = { urls: keep };
        if (hasCreds && turn.length) { e.username = s.username; e.credential = s.credential; }
        out.push(e);
    }
    out.sort((a, b) => iceUrlRank(a.urls[0]) - iceUrlRank(b.urls[0]));
    return out;
}

/**
 * buildRtcConfig — RTCConfiguration глядача. config.iceTransportPolicy
 * 'relay' — лише для діагностики («чи живий TURN»); будь-що інше = 'all'.
 */
export function buildRtcConfig(config) {
    const c = config || {};
    return {
        iceServers: normalizeIceServers(c.iceServers),
        iceTransportPolicy: c.iceTransportPolicy === 'relay' ? 'relay' : 'all',
        bundlePolicy: 'max-bundle',
    };
}

// ─────────────────────────────────────────────────────────────────────────────
// DOM-шар: overlay <video> + RTCPeerConnection recvonly
// ─────────────────────────────────────────────────────────────────────────────

/**
 * createOoWebrtcLayer — публічна точка входу (аналог createOoLayer).
 *
 * @param {Element}  o.container
 * @param {object}   o.meshDesktop
 * @param {object}   o.config  { requestTicket, firstFrameMs?, frameAgeMs?,
 *                               geometryTimeoutMs?, offerTimeoutMs?, iceServers?,
 *                               iceTransportPolicy?,
 *                               disconnectGraceMs?, inputChannel?, inputGrant?,
 *                               inputAllowed? }
 *   inputChannel (F5, типово false) — власний канал вводу oosc-input замість
 *   Mesh, лише для ролі control; inputAllowed() — гачок політики згоди.
 *   requestTicket() → Promise<{ticket, signalUrl, grant?, standbySignalUrls?}> — §6.4 свіжий одноразовий
 *   ticket на цю ноду; offer їде з ticket, НЕ з довгоживучим токеном (BLOCKER-1/3).
 * @param {Function} o.onStateChange(state, reason)
 * @returns {{destroy: Function, state: Function, generation: Function}}
 */
// ── режим показу (TZ 1.1 / P6) ─────────────────────────────────────────────
// 'fit' — підігнати під бокс Mesh-canvas, ЗБЕРІГАЮЧИ пропорції (contain).
// '1:1' — один піксель відео = один фізичний піксель екрана:
//         CSS-розмір = videoWidth/dpr × videoHeight/dpr, контейнер скролиться.
// Раніше було object-fit:fill + CSS-розмір з canvas — браузер масштабував
// кадр вдруге (і без урахування dpr), звідси мило й криві пропорції.
export const DISPLAY_FIT = 'fit';
export const DISPLAY_1X1 = '1:1';
export const DISPLAY_STORAGE_KEY = 'oo-screen-display-mode';

export function normalizeDisplayMode(m) {
    return m === DISPLAY_1X1 ? DISPLAY_1X1 : DISPLAY_FIT;
}

export function loadDisplayMode(storage) {
    try {
        const st = storage !== undefined ? storage : (typeof localStorage !== 'undefined' ? localStorage : null);
        return normalizeDisplayMode(st ? st.getItem(DISPLAY_STORAGE_KEY) : null);
    } catch (e) { return DISPLAY_FIT; }
}

export function saveDisplayMode(mode, storage) {
    try {
        const st = storage !== undefined ? storage : (typeof localStorage !== 'undefined' ? localStorage : null);
        if (st) st.setItem(DISPLAY_STORAGE_KEY, normalizeDisplayMode(mode));
    } catch (e) { /* приватне вікно / заблоковане сховище — живемо без пам'яті */ }
}

// Бокс, у який object-fit:contain реально кладе кадр усередині елемента
// boxW×boxH (з лєтербоксом). Координати — відносно лівого верхнього кута боксу.
export function containBox(boxW, boxH, srcW, srcH) {
    if (!(boxW > 0) || !(boxH > 0) || !(srcW > 0) || !(srcH > 0)) {
        return { x: 0, y: 0, width: Math.max(0, boxW || 0), height: Math.max(0, boxH || 0) };
    }
    const scale = Math.min(boxW / srcW, boxH / srcH);
    const width = srcW * scale;
    const height = srcH * scale;
    return { x: (boxW - width) / 2, y: (boxH - height) / 2, width, height };
}

// CSS-розмір для режиму 1:1. integer=true — dpr цілий (1, 2, 3), тобто кожен
// піксель відео лягає рівно на N×N фізичних: тоді й тільки тоді вмикаємо
// image-rendering:pixelated (на дробовому dpr воно дає «драбину»).
// absorbForeignStyles — 1:1 тримає знімок стилів Mesh, щоб повернути їх при
// виході. Якщо поки діє 1:1 Mesh сам змінив якийсь стиль (значення вже не те,
// що записали ми), це нова «оригінальна» величина — переносимо її в знімок,
// інакше restore відкотив би Mesh до стану на момент входу в 1:1.
// saved — знімок; written — що ми записали востаннє (null = ще нічого);
// current — що є зараз. Мутує й повертає saved.
export function absorbForeignStyles(saved, written, current) {
    if (!saved || !written) return saved;
    for (const k of Object.keys(saved)) {
        if (k in written && current[k] !== written[k]) saved[k] = current[k];
    }
    return saved;
}

export function oneToOneSize(videoW, videoH, dpr) {
    const d = dpr > 0 ? dpr : 1;
    return {
        width: videoW / d,
        height: videoH / d,
        integer: Math.abs(d - Math.round(d)) < 1e-6,
    };
}

// clientX/clientY → піксель віддаленого екрана. rect — getBoundingClientRect()
// елемента з картинкою; srcW×srcH — розмір віддаленого кадру. Враховує
// лєтербокс від contain. null — клік у чорну смугу (нікуди слати).
export function mapClientToRemote(clientX, clientY, rect, srcW, srcH) {
    if (!rect || !(srcW > 0) || !(srcH > 0)) return null;
    const b = containBox(rect.width, rect.height, srcW, srcH);
    if (!(b.width > 0) || !(b.height > 0)) return null;
    const lx = clientX - rect.left - b.x;
    const ly = clientY - rect.top - b.y;
    if (lx < 0 || ly < 0 || lx > b.width || ly > b.height) return null;
    const x = Math.min(srcW - 1, Math.floor(lx * srcW / b.width));
    const y = Math.min(srcH - 1, Math.floor(ly * srcH / b.height));
    return { x, y };
}

// Зворотний мапінг для шару курсора: піксель віддаленого кадру (x,y у
// srcW×srcH) → точка відносно лівого верхнього кута rect (той самий contain-
// бокс, що й mapClientToRemote). scale — CSS-пікселів на піксель кадру
// (оверлей курсора масштабується разом із картинкою). null — нема геометрії.
export function mapRemoteToClient(x, y, rect, srcW, srcH) {
    if (!rect || !(srcW > 0) || !(srcH > 0)) return null;
    const b = containBox(rect.width, rect.height, srcW, srcH);
    if (!(b.width > 0) || !(b.height > 0)) return null;
    const scale = b.width / srcW;
    return { x: b.x + x * scale, y: b.y + y * (b.height / srcH), scale };
}

/** offerBody — тіло /offer/viewer. monitor лише ціле >0 (F6, config.monitor:
 *  потік "<node>#m<i>" на хабі), інакше поля немає — offer як до F6. */
export function offerBody(sdp, ticket, monitor) {
    if (Number.isInteger(monitor) && monitor > 0) return JSON.stringify({ sdp: sdp, ticket, monitor });
    return JSON.stringify({ sdp: sdp, ticket });
}

/**
 * O2: резервний хаб для глядача. Типово ВИМКНЕНО: без config.standbySignalUrls
 * повертає [signalUrl] — рівно одна спроба, як до O2.
 * standbySignalUrls — масив повних URL /offer/viewer резервних хабів (ті самі
 * ticket-секрети, див. tools/oo-screen/deploy/DEPLOY.md «O2»).
 */
export function signalCandidates(signalUrl, standby) {
    const out = [signalUrl];
    if (Array.isArray(standby)) {
        for (const u of standby) {
            if (typeof u === 'string' && u && !out.includes(u)) out.push(u);
        }
    }
    return out;
}

/**
 * Чи пробувати наступний хаб.
 *  - помилка fetch: так, ЯКЩО це не teardown (tornDown). Таймаут спроби
 *    (AbortError у фолбеку / TimeoutError у AbortSignal.timeout) — так:
 *    blackhole/завислий хаб = найчастіший «мертвий хост».
 *  - 502/503/504: проксі перед мертвим хабом.
 *  - 404 «no publisher for node»: агент сидить на ІНШОМУ хабі (після failover
 *    агент на основний сам не повертається — інакше split-brain).
 *  - 400/401/403 (ticket, ACL) — НІ: резервний відповість так само.
 * Наступна спроба завжди йде зі СВІЖИМ ticket-ом (postOfferWithFailover).
 */
/**
 * O2: звідки брати резервні URL глядача. Пріоритет — відповідь requestTicket()
 * (поле standbySignalUrls поруч із signalUrl: той самий серверний канал, що вже
 * несе signalUrl, тож ERP вмикає резерв без зміни коду виклику шару); інакше —
 * config.standbySignalUrls. Нічого нема → undefined (типово OFF, одна спроба).
 */
export function resolveStandby(ticketResp, config) {
    if (ticketResp && Array.isArray(ticketResp.standbySignalUrls)) return ticketResp.standbySignalUrls;
    if (config && Array.isArray(config.standbySignalUrls)) return config.standbySignalUrls;
    return undefined;
}

export function shouldFailover(err, status, tornDown) {
    if (err) return !tornDown;
    return status === 404 || status === 502 || status === 503 || status === 504;
}

/**
 * O2: POST offer по кандидатах. Кожна спроба має ВЛАСНИЙ таймаут (окремий
 * combineAbortSignals), тож таймаут основного не вбиває спробу резервного.
 * Кожна наступна спроба бере НОВИЙ одноразовий ticket через requestTicket():
 * ticket, що міг бути спожитий хабом, який відповів/обірвався, повторно не
 * надсилається (used-ticket облік у кожного хаба свій).
 *
 * o: { urls, ticket, requestTicket, makeBody(ticket), onTicket(ticket, grant),
 *      teardownSignal, timeoutMs, fetchFn, combine }
 * @returns {Promise<Response>}
 */
export async function postOfferWithFailover(o) {
    const combine = o.combine || combineAbortSignals;
    const torn = () => !!(o.teardownSignal && o.teardownSignal.aborted);
    let ticket = o.ticket;
    for (let i = 0; i < o.urls.length; i++) {
        const last = i === o.urls.length - 1;
        if (i > 0) {
            if (torn()) throw new Error('offer/viewer: teardown');
            const t = await o.requestTicket();
            if (!t || !t.ticket) throw new Error('offer/viewer: немає ticket для резервного хаба');
            ticket = t.ticket;
            if (o.onTicket) o.onTicket(t.ticket, t.grant);
        }
        const c = combine(o.teardownSignal, o.timeoutMs);
        let resp;
        try {
            resp = await o.fetchFn(o.urls[i], {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: o.makeBody(ticket),
                signal: c.signal,
            });
        } catch (e) {
            if (last || !shouldFailover(e, 0, torn())) throw e;
            continue;
        } finally {
            c.cancel();
        }
        if (last || !shouldFailover(null, resp.status)) return resp;
    }
    throw new Error('offer/viewer: немає кандидатів');
}

// ─────────────────────────────────────────────────────────────────────────────
// N6: пряма нога агент↔браузер. Типово ВИМКНЕНО: без config.p2p плеєр шле
// offer лише на /offer/viewer, як і раніше. З config.p2p — спершу /p2p/offer
// того самого хаба (хаб споживає ERP-квиток, агент відповідає напряму).
// 409 або ICE, що не зʼєднався, — relay /offer/viewer з одноразовим
// relay_ticket, який видав хаб (ERP-квиток уже спожито, повторно не шлемо).
// ─────────────────────────────────────────────────────────────────────────────

export const PATH_DIRECT = 'direct';
export const PATH_RELAY = 'relay';
const DEFAULT_P2P_CONNECT_MS = 8000;
const DEFAULT_RELAY_RETRIES = 6;
const DEFAULT_RELAY_RETRY_MS = 1500;

/** p2pOfferUrl — config.p2pUrl або …/offer/viewer → …/p2p/offer; null — не вивести. */
export function p2pOfferUrl(signalUrl, override) {
    if (typeof override === 'string' && override) return override;
    if (typeof signalUrl !== 'string') return null;
    const m = /^(.*)\/offer\/viewer\/?(\?.*)?$/.exec(signalUrl);
    return m ? m[1] + '/p2p/offer' : null;
}

async function postJson(fetchFn, url, body, o) {
    const combine = o.combine || combineAbortSignals;
    const c = combine(o.teardownSignal, o.timeoutMs || DEFAULT_OFFER_TIMEOUT_MS);
    try {
        return await fetchFn(url, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body,
            signal: c.signal,
        });
    } finally {
        c.cancel();
    }
}

/**
 * postP2POffer — POST /p2p/offer. Результат:
 *   {kind:'direct', sdp, id, relayTicket}  — 200 (relayTicket дійсний лише після відкату)
 *   {kind:'relay', reason, relayTicket}    — 409 (relayTicket дійсний одразу)
 *   {kind:'relay', reason, ticket:'same'}  — 404/405: маршруту нема (P2P вимкнено
 *                                            на хабі) — ERP-квиток НЕ спожито
 *   {kind:'relay', reason, ticket:'fresh'} — інше: квиток міг згоріти, потрібен новий
 */
export async function postP2POffer(o) {
    let resp;
    try {
        resp = await postJson(o.fetchFn, o.url, JSON.stringify({ ticket: o.ticket, sdp: o.sdp }), o);
    } catch (e) {
        if (o.teardownSignal && o.teardownSignal.aborted) throw e;
        return { kind: PATH_RELAY, reason: 'p2p-network', ticket: 'fresh' };
    }
    if (resp.status === 200) {
        const a = await resp.json();
        if (a && a.sdp) return { kind: PATH_DIRECT, sdp: a.sdp, id: a.id, relayTicket: a.relay_ticket || '' };
        return { kind: PATH_RELAY, reason: 'p2p-empty-answer', ticket: 'fresh' };
    }
    if (resp.status === 409) {
        let a = null;
        try { a = await resp.json(); } catch (e) { /* ignore */ }
        if (a && a.relay_ticket) return { kind: PATH_RELAY, reason: a.reason || 'conflict', relayTicket: a.relay_ticket };
        return { kind: PATH_RELAY, reason: 'p2p-409-no-ticket', ticket: 'fresh' };
    }
    if (resp.status === 404 || resp.status === 405) return { kind: PATH_RELAY, reason: 'p2p-off', ticket: 'same' };
    return { kind: PATH_RELAY, reason: 'p2p-' + resp.status, ticket: 'fresh' };
}

/**
 * redeemRelay — /offer/viewer з relay_ticket. Після ICE-відкату квиток стає
 * дійсним, лише коли агент звітує fallback хабу (його ICE-сторож може
 * спрацювати трохи пізніше за браузерний), тож 403 повторюємо обмежено.
 * Будь-яка інша відповідь — кінець: квиток одноразовий, і хаб його вже спожив.
 */
export async function redeemRelay(o) {
    const retries = o.retries ?? DEFAULT_RELAY_RETRIES;
    const sleep = o.sleep || ((ms) => new Promise((r) => setTimeout(r, ms)));
    for (let i = 0; ; i++) {
        const resp = await postJson(o.fetchFn, o.url, o.makeBody(o.relayTicket), o);
        if (resp.status !== 403 || i >= retries) return resp;
        if (o.teardownSignal && o.teardownSignal.aborted) return resp;
        await sleep(o.retryMs ?? DEFAULT_RELAY_RETRY_MS);
    }
}

/** waitIceOutcome — 'connected' | 'failed' (failed або таймаут). */
export function waitIceOutcome(peer, timeoutMs, timers) {
    const setT = (timers && timers.setTimeout) || setTimeout;
    const clrT = (timers && timers.clearTimeout) || clearTimeout;
    return new Promise((resolve) => {
        let done = false;
        let t = null;
        const finish = (v) => {
            if (done) return;
            done = true;
            if (t !== null) clrT(t);
            peer.oniceconnectionstatechange = null;
            resolve(v);
        };
        const check = () => {
            const s = peer.iceConnectionState;
            if (s === 'connected' || s === 'completed') finish('connected');
            else if (s === 'failed' || s === 'closed') finish('failed');
        };
        t = setT(() => finish('failed'), timeoutMs || DEFAULT_P2P_CONNECT_MS);
        peer.oniceconnectionstatechange = check;
        check();
    });
}

/**
 * negotiateViewer — увесь SDP-обмін глядача: (опційно) пряма нога, інакше
 * relay. Ставить remote description і повертає {peer, path} (null — сесія
 * вже не поточна).
 * Пряма нога повертає ще relayTicket — одноразовий квиток на relay, якщо
 * вже ЗЄДНАНА нога згодом обірветься (див. createDirectRescue).
 * o.relayTicket (порятунок обірваної прямої ноги) — /p2p/offer не чіпаємо,
 * одразу relay з цим квитком.
 * o: { p2p, p2pUrl, signalUrl, standby, ticket, grant, requestTicket, peer, relayTicket,
 *      rebuildPeer() → Promise<peer>, waitIce(peer) → Promise<'connected'|'failed'>,
 *      makeBody(sdp, ticket), onTicket(ticket, grant, peer), onPath(path),
 *      isCurrent(), fetchFn, teardownSignal, timeoutMs, relayRetries, relayRetryMs, sleep }
 */
export async function negotiateViewer(o) {
    let peer = o.peer;
    let grant = o.grant;
    const current = o.isCurrent || (() => true);
    const onPath = o.onPath || (() => {});
    const net = { fetchFn: o.fetchFn, teardownSignal: o.teardownSignal, timeoutMs: o.timeoutMs || DEFAULT_OFFER_TIMEOUT_MS, combine: o.combine };
    let relayTicket = o.relayTicket || null;
    let ticket = o.ticket;
    const p2pUrl = (o.p2p && !relayTicket) ? p2pOfferUrl(o.signalUrl, o.p2pUrl) : null;
    if (p2pUrl) {
        const r = await postP2POffer({ ...net, url: p2pUrl, ticket, sdp: peer.localDescription.sdp });
        if (!current()) return null;
        if (r.kind === PATH_DIRECT) {
            onPath('direct-connecting');
            await peer.setRemoteDescription({ type: 'answer', sdp: r.sdp });
            const out = await o.waitIce(peer);
            if (!current()) return null;
            if (out === 'connected') {
                onPath(PATH_DIRECT);
                return { peer, path: PATH_DIRECT, relayTicket: r.relayTicket || '' };
            }
            // ICE не зʼєднався: нова PeerConnection на relay (стару вже
            // описано answer-ом агента). Агент сам звітує fallback хабу.
            if (!r.relayTicket) throw new Error('p2p: ICE failed і немає relay_ticket');
            relayTicket = r.relayTicket;
            peer = await o.rebuildPeer();
            if (!current()) return null;
        } else if (r.relayTicket) {
            relayTicket = r.relayTicket; // 409: той самий offer іде на relay
        } else if (r.ticket === 'fresh') {
            const t = await o.requestTicket();
            if (!current()) return null;
            if (!t || !t.ticket) throw new Error('offer/viewer: немає ticket');
            ticket = t.ticket;
            grant = t.grant ?? grant;
        }
    }
    let resp;
    if (relayTicket) {
        // Ввід на relay-нозі хаб судить за тікетом ноги — тепер це relay_ticket.
        if (o.onTicket) o.onTicket(relayTicket, grant, peer);
        resp = await redeemRelay({
            ...net, url: o.signalUrl, relayTicket,
            makeBody: (t) => o.makeBody(peer.localDescription.sdp, t),
            retries: o.relayRetries, retryMs: o.relayRetryMs, sleep: o.sleep,
        });
    } else {
        if (o.onTicket && ticket !== o.ticket) o.onTicket(ticket, grant, peer);
        resp = await postOfferWithFailover({
            ...net,
            urls: signalCandidates(o.signalUrl, o.standby),
            ticket,
            requestTicket: o.requestTicket,
            makeBody: (t) => o.makeBody(peer.localDescription.sdp, t),
            onTicket: (t, g) => { if (o.onTicket) o.onTicket(t, g, peer); },
        });
    }
    if (!current()) return null;
    if (!resp.ok) throw new Error('offer/viewer ' + resp.status);
    const answer = await resp.json();
    if (!current()) return null;
    if (!answer || !answer.sdp) throw new Error('offer/viewer: порожній answer');
    await peer.setRemoteDescription({ type: 'answer', sdp: answer.sdp });
    onPath(PATH_RELAY);
    return { peer, path: PATH_RELAY };
}

/**
 * createDirectRescue — N6: обрив УЖЕ ЗЄДНАНОЇ прямої ноги (ICE failed, кадри
 * перестали йти, агент закрив ногу) — це не привід іти в Mesh: хаб видав
 * разом з answer одноразовий relay_ticket, і він стає дійсним, щойно агент
 * звітує fallback (його ICE-сторож). Порятунок — рівно один раз на сесію:
 * arm() після успішної прямої ноги, take() у фолбеку повертає квиток (і
 * гасить себе), якщо поточний шлях — direct. Relay-нога, що теж впала, іде
 * в Mesh як завжди.
 */
export function createDirectRescue() {
    let armed = null;
    return {
        arm(relayTicket, grant, signalUrl) {
            armed = relayTicket ? { relayTicket, grant, signalUrl } : null;
        },
        take(path) {
            if (path !== PATH_DIRECT || !armed) return null;
            const r = armed;
            armed = null;
            return r;
        },
        clear() { armed = null; },
        armed: () => armed !== null,
    };
}

export function createOoWebrtcLayer(o) {
    const opts = o || {};
    const container = opts.container;
    const meshDesktop = opts.meshDesktop;
    const config = opts.config || {};
    const onStateChange = opts.onStateChange || (() => {});

    const doc = (container && container.ownerDocument) || (typeof document !== 'undefined' ? document : null);
    if (!doc) throw new Error('createOoWebrtcLayer: потрібен DOM');
    if (typeof config.requestTicket !== 'function') throw new Error('createOoWebrtcLayer: немає requestTicket');

    const meshCanvas = findMeshCanvas(meshDesktop, container);
    if (!meshCanvas) throw new Error('createOoWebrtcLayer: не знайдено Mesh-canvas');

    let pc = null;
    let video = null;
    let abort = null;
    let resizeObserver = null;
    let attrObserver = null;
    let geometryTimer = null;
    let rvfcHandle = 0;
    let paused = false;
    let destroyed = false;
    let disconnectGrace = null;   // §MAJOR-6
    let hiddenKeepalive = null;   // §MAJOR-7
    let visibilityHandler = null; // §MAJOR-7
    let displayMode = normalizeDisplayMode(config.displayMode || loadDisplayMode());
    let dprQuery = null;          // TZ 1.1: matchMedia на поточний dpr
    let dprHandler = null;
    let toggleBtn = null;
    let meshSaved = null;         // оригінальні стилі Mesh-canvas/контейнера на час 1:1
    let meshWritten = null;       // що ми самі записали в ці стилі востаннє
    let textTiles = null;         // STAGE3-444 B: canvas з lossless тайлами тексту (config.textTiles)
    let tilesChannel = null;
    let cursorLayer = null;       // config.cursorLayer: окремий курсор (desktop-oo-cursor.js)
    let inputChannel = null;      // F5: config.inputChannel — власний канал вводу (desktop-oo-input.js)
    let inputDom = null;
    let statsOverlay = null;      // getStats()-оверлей (Ctrl+Alt+S); config.statsOverlay === false — вимкнено
    let transportPath = null;     // N6: 'direct' | 'relay' (| 'direct-connecting') — у стат-оверлеї
    let p2pTrialPeer = null;      // N6: peer, чий ICE ще пробує пряму ногу — його стани не фолбечать у Mesh
    const directRescue = createDirectRescue(); // N6: обрив зʼєднаної прямої ноги -> relay, не Mesh

    const session = createOoSession({
        firstFrameMs: config.firstFrameMs,
        frameAgeMs: config.frameAgeMs,
        onStateChange,
        onFallback: (gen, reason) => doFallback(gen, reason),
    });

    // ── overlay ──────────────────────────────────────────────────────────────
    function makeOverlay() {
        const v = doc.createElement('video');
        v.className = 'oo-screen-layer oo-screen-layer-webrtc';
        v.autoplay = true;
        v.muted = true;
        v.defaultMuted = true;
        v.playsInline = true;
        v.setAttribute('autoplay', '');
        v.setAttribute('muted', '');
        v.setAttribute('playsinline', '');
        // pointer-events:none — НЕ косметика (див. шапку файла).
        v.style.cssText = 'position:absolute;pointer-events:none;z-index:5;object-fit:contain;background:transparent;';
        const cs = doc.defaultView && doc.defaultView.getComputedStyle(container);
        if (cs && cs.position === 'static') container.style.position = 'relative';
        container.appendChild(v);
        return v;
    }

    // Геометрія — та сама логіка, що в canvas-версії: міряємо від ФАКТИЧНОГО
    // боксу Mesh-canvas, а не від контейнера (Mesh лєтербоксить картинку, і
    // «розтягнути на контейнер» дало б зсув між тим, що видно, і тим, куди
    // клікає користувач).
    //
    // TZ 1.1: input і далі ловить Mesh-canvas і сам мапить клік через СВІЙ
    // rect → canvas.width/height. Тому в режимі 1:1 ми міняємо CSS-розмір
    // саме Mesh-canvas (а відео просто їде за ним): так мапінг Mesh лишається
    // правильним без жодного нашого перерахунку. Відео всередині цього боксу
    // — contain: якщо пропорції кадру хаба й canvas збігаються (норма),
    // лєтербоксу нема і піксель під курсором = піксель, куди піде клік.
    function setStyle(el, prop, val) {
        // Пишемо лише при зміні: інакше MutationObserver(style) → syncGeometry
        // → знову запис → нескінченна петля.
        if (el.style[prop] !== val) el.style[prop] = val;
    }

    function currentDpr() {
        const w = doc.defaultView;
        return (w && w.devicePixelRatio > 0) ? w.devicePixelRatio : 1;
    }

    function applyOneToOne() {
        const vw = video && video.videoWidth | 0;
        const vh = video && video.videoHeight | 0;
        if (!(vw > 0 && vh > 0)) return false;
        const sz = oneToOneSize(vw, vh, currentDpr());
        const readMesh = () => ({
            width: meshCanvas.style.width, height: meshCanvas.style.height,
            maxWidth: meshCanvas.style.maxWidth, maxHeight: meshCanvas.style.maxHeight,
            overflow: container.style.overflow,
        });
        if (!meshSaved) {
            meshSaved = readMesh();
            meshWritten = null;
        } else {
            // Зміни, яких ми не писали, — від Mesh: вони й мають повернутись.
            absorbForeignStyles(meshSaved, meshWritten, readMesh());
        }
        setStyle(meshCanvas, 'maxWidth', 'none');
        setStyle(meshCanvas, 'maxHeight', 'none');
        setStyle(meshCanvas, 'width', sz.width + 'px');
        setStyle(meshCanvas, 'height', sz.height + 'px');
        setStyle(container, 'overflow', 'auto');
        meshWritten = readMesh(); // як браузер нормалізував наші значення
        setStyle(video, 'imageRendering', sz.integer ? 'pixelated' : 'auto');
        return true;
    }

    function restoreMesh() {
        if (!meshSaved) return;
        const s0 = meshSaved;
        meshSaved = null;
        meshWritten = null;
        try {
            meshCanvas.style.width = s0.width;
            meshCanvas.style.height = s0.height;
            meshCanvas.style.maxWidth = s0.maxWidth;
            meshCanvas.style.maxHeight = s0.maxHeight;
            container.style.overflow = s0.overflow;
        } catch (e) { /* ignore */ }
    }

    function syncGeometry() {
        if (!video) return;
        if (displayMode === DISPLAY_1X1) {
            applyOneToOne();
        } else {
            restoreMesh();
            setStyle(video, 'imageRendering', 'auto');
        }
        const mr = meshCanvas.getBoundingClientRect();
        const cr = container.getBoundingClientRect();
        setStyle(video, 'left', (mr.left - cr.left + container.scrollLeft) + 'px');
        setStyle(video, 'top', (mr.top - cr.top + container.scrollTop) + 'px');
        setStyle(video, 'width', mr.width + 'px');
        setStyle(video, 'height', mr.height + 'px');
        // Оверлей тайлів — у тому самому боксі, всередині contain-прямокутника кадру.
        if (textTiles) {
            textTiles.place(mr.left - cr.left + container.scrollLeft, mr.top - cr.top + container.scrollTop,
                mr.width, mr.height);
        }
        if (cursorLayer) cursorLayer.relayout();
    }

    // Зміна dpr (перетяг вікна на інший монітор, Ctrl+/-) ResizeObserver не
    // ловить. matchMedia на ПОТОЧНЕ значення спрацьовує раз — перевішуємо.
    function watchDpr() {
        unwatchDpr();
        const w = doc.defaultView;
        if (!w || typeof w.matchMedia !== 'function') return;
        try {
            dprQuery = w.matchMedia('(resolution: ' + currentDpr() + 'dppx)');
            dprHandler = () => { syncGeometry(); watchDpr(); };
            if (typeof dprQuery.addEventListener === 'function') dprQuery.addEventListener('change', dprHandler);
            else if (typeof dprQuery.addListener === 'function') dprQuery.addListener(dprHandler);
        } catch (e) { dprQuery = null; dprHandler = null; }
    }

    function unwatchDpr() {
        if (dprQuery && dprHandler) {
            try {
                if (typeof dprQuery.removeEventListener === 'function') dprQuery.removeEventListener('change', dprHandler);
                else if (typeof dprQuery.removeListener === 'function') dprQuery.removeListener(dprHandler);
            } catch (e) { /* ignore */ }
        }
        dprQuery = null;
        dprHandler = null;
    }

    function setDisplayMode(m) {
        displayMode = normalizeDisplayMode(m);
        saveDisplayMode(displayMode);
        if (toggleBtn) toggleBtn.textContent = displayMode === DISPLAY_1X1 ? '100 %' : 'Підігнати';
        syncGeometry();
        return displayMode;
    }

    // Кнопка-перемикач. На відміну від <video>, вона СВІДОМО ловить кліки —
    // вона маленька, у куті й не перекриває робочу область Mesh по суті.
    // config.displayToggle === false — без кнопки (UI дає свою через API).
    function makeToggle() {
        if (config.displayToggle === false || toggleBtn) return;
        const b = doc.createElement('button');
        b.type = 'button';
        b.className = 'oo-screen-display-toggle';
        b.title = 'Режим показу: підігнати / 1:1';
        b.style.cssText = 'position:absolute;top:4px;right:4px;z-index:6;font:12px sans-serif;padding:2px 6px;opacity:.7;cursor:pointer;';
        b.textContent = displayMode === DISPLAY_1X1 ? '100 %' : 'Підігнати';
        b.addEventListener('click', (ev) => {
            ev.preventDefault();
            ev.stopPropagation();
            setDisplayMode(displayMode === DISPLAY_1X1 ? DISPLAY_FIT : DISPLAY_1X1);
        });
        container.appendChild(b);
        toggleBtn = b;
    }

    function watchGeometry() {
        if (typeof ResizeObserver === 'function') {
            resizeObserver = new ResizeObserver(() => syncGeometry());
            resizeObserver.observe(meshCanvas);
            resizeObserver.observe(container);
        }
        if (typeof MutationObserver === 'function') {
            attrObserver = new MutationObserver(() => syncGeometry());
            attrObserver.observe(meshCanvas, { attributes: true, attributeFilter: ['width', 'height', 'style', 'class'] });
        }
        if (video) video.addEventListener('resize', () => syncGeometry()); // зміна videoWidth/Height
        watchDpr();
        makeToggle();
        if (config.statsOverlay !== false && !statsOverlay) {
            statsOverlay = createStatsOverlay({ container, doc, getPc: () => pc, getPath: () => transportPath });
        }
    }

    // ── фолбек ───────────────────────────────────────────────────────────────
    function unpauseMesh() {
        if (!paused) return;
        paused = false;
        meshCall(meshDesktop, MESH_UNPAUSE_NAMES);
        meshCall(meshDesktop, MESH_REFRESH_NAMES);
    }

    function teardownOo() {
        if (disconnectGrace) { try { disconnectGrace.cancel(); } catch (e) { /* ignore */ } disconnectGrace = null; }
        if (hiddenKeepalive) { try { hiddenKeepalive.stop(); } catch (e) { /* ignore */ } hiddenKeepalive = null; }
        if (visibilityHandler && doc && typeof doc.removeEventListener === 'function') {
            try { doc.removeEventListener('visibilitychange', visibilityHandler); } catch (e) { /* ignore */ }
        }
        visibilityHandler = null;
        if (geometryTimer !== null) { clearTimeout(geometryTimer); geometryTimer = null; }
        if (resizeObserver) { try { resizeObserver.disconnect(); } catch (e) { /* ignore */ } resizeObserver = null; }
        if (attrObserver) { try { attrObserver.disconnect(); } catch (e) { /* ignore */ } attrObserver = null; }
        unwatchDpr();
        if (statsOverlay) { try { statsOverlay.destroy(); } catch (e) { /* ignore */ } statsOverlay = null; }
        if (toggleBtn) { if (toggleBtn.parentNode) { try { toggleBtn.parentNode.removeChild(toggleBtn); } catch (e) { /* ignore */ } } toggleBtn = null; }
        // Mesh-canvas має повернутись до свого розміру ДО того, як ним знову
        // керує Mesh (фолбек), інакше його мапінг пішов би від нашого 1:1.
        restoreMesh();
        if (cursorLayer) { try { cursorLayer.destroy(); } catch (e) { /* ignore */ } cursorLayer = null; }
        detachInput();
        if (abort) { try { abort.abort(); } catch (e) { /* ignore */ } abort = null; }
        if (video) {
            if (rvfcHandle && typeof video.cancelVideoFrameCallback === 'function') {
                try { video.cancelVideoFrameCallback(rvfcHandle); } catch (e) { /* ignore */ }
            }
            rvfcHandle = 0;
            try { video.pause(); } catch (e) { /* ignore */ }
            try { video.srcObject = null; } catch (e) { /* ignore */ }
            if (video.parentNode) { try { video.parentNode.removeChild(video); } catch (e) { /* ignore */ } }
        }
        video = null;
        if (tilesChannel) {
            try { tilesChannel.onmessage = null; tilesChannel.close(); } catch (e) { /* ignore */ }
            tilesChannel = null;
        }
        if (textTiles) { try { textTiles.destroy(); } catch (e) { /* ignore */ } textTiles = null; }
        if (pc) {
            // Порядок: спершу глушимо колбеки, потім close(). Інакше
            // connectionstatechange='closed' від НАШОГО ж close() прилітає як
            // «сесія померла» і затирає причину справжнього фолбеку.
            try { pc.ontrack = null; pc.onconnectionstatechange = null; pc.oniceconnectionstatechange = null; } catch (e) { /* ignore */ }
            try { pc.close(); } catch (e) { /* ignore */ }
            pc = null;
        }
    }

    function doFallback(gen, reason) {
        // N6: впала вже зʼєднана пряма нога — relay з relay_ticket (новий
        // peer, нова генерація), а не Mesh. Mesh на час переходу розглушуємо,
        // як і при першому підключенні: пауза — лише після SDP-обміну.
        const rescue = destroyed ? null : directRescue.take(transportPath);
        if (rescue) {
            restoreMesh();
            unpauseMesh();
            teardownOo();
            transportPath = null;
            start(rescue);
            return;
        }
        directRescue.clear();
        // Спершу повертаємо Mesh-картинку, потім знімаємо OO-шар — зворотний
        // порядок дає видиму дірку, у яку встигають клікнути. Розмір
        // Mesh-canvas (режим 1:1) повертаємо ще раніше — до того, як Mesh малює.
        restoreMesh();
        unpauseMesh();
        teardownOo();
    }

    // ── старт ────────────────────────────────────────────────────────────────
    function geometryReady() {
        return (meshCanvas.width | 0) > 0 && (meshCanvas.height | 0) > 0;
    }

    function waitGeometry(gen) {
        return new Promise((resolve) => {
            if (geometryReady()) { resolve(true); return; }
            const deadline = Date.now() + (config.geometryTimeoutMs || DEFAULT_GEOMETRY_MS);
            const poll = () => {
                if (!session.isCurrent(gen)) { resolve(false); return; }
                if (geometryReady()) { resolve(true); return; }
                if (Date.now() >= deadline) { resolve(false); return; }
                geometryTimer = setTimeout(poll, 100);
            };
            poll();
        });
    }

    function waitIceGathering(peer) {
        if (peer.iceGatheringState === 'complete') return Promise.resolve();
        return new Promise((resolve) => {
            // Дедлайн обов'язковий: без TURN-серверів gathering зрідка не
            // доходить до 'complete' взагалі, і offer ніколи б не поїхав.
            const t = setTimeout(resolve, 2000);
            peer.onicegatheringstatechange = () => {
                if (peer.iceGatheringState === 'complete') { clearTimeout(t); resolve(); }
            };
        });
    }

    // Перший кадр і frame-age — обидва через rVFC. Це ЄДИНЕ у WebRTC вікно у
    // реальний потік кадрів: readyState/currentTime брешуть (тікають і на
    // застиглому треку), а 'loadeddata' стріляє рівно раз.
    // §MAJOR-7: поки вкладка hidden — «підживлюємо» frame-age, щоб тротлінг rVFC
    // у фоні не завалив живу сесію хибним frame-age-timeout.
    function armHiddenKeepalive(gen) {
        if (hiddenKeepalive || !doc || typeof doc.addEventListener !== 'function') return;
        hiddenKeepalive = createHiddenFrameKeepalive({
            isHidden: () => !!doc.hidden,
            keepFresh: () => { if (session.isCurrent(gen)) session.noteFrame(gen); },
        });
        visibilityHandler = () => hiddenKeepalive && hiddenKeepalive.onVisibility();
        doc.addEventListener('visibilitychange', visibilityHandler);
        // Могли армитись уже у фоновій вкладці — одразу почати підживлення.
        if (doc.hidden) hiddenKeepalive.onVisibility();
    }

    function armFrameSource(gen) {
        if (!video) return;
        armHiddenKeepalive(gen);
        if (typeof video.requestVideoFrameCallback === 'function') {
            const onFrame = (_now, meta) => {
                if (!session.isCurrent(gen) || !video) return;
                session.noteFrame(gen);
                // Кадр іншої геометрії (інший монітор) — тайли до нього не стосуються;
                // кадр без анонсу still-повтору — ховаємо тайли (oo-text-tiles.js TYPE_STILL).
                if (textTiles) {
                    textTiles.onVideoFrame(video.videoWidth | 0, video.videoHeight | 0,
                        meta && typeof meta.presentedFrames === 'number' ? meta.presentedFrames : undefined);
                }
                rvfcHandle = video.requestVideoFrameCallback(onFrame);
            };
            rvfcHandle = video.requestVideoFrameCallback(onFrame);
            return;
        }
        // Браузер без rVFC: 'loadeddata' дає перший кадр, далі 'timeupdate'
        // тримає frame-age живим. Гірша точність, але краще за гарантований
        // хибний фолбек через 3с на живій картинці.
        video.addEventListener('loadeddata', () => {
            if (session.isCurrent(gen)) session.noteFrame(gen);
        });
        video.addEventListener('timeupdate', () => {
            if (session.isCurrent(gen)) session.noteFrame(gen);
        });
    }

    async function start(rescue) {
        const gen = session.begin();
        abort = new AbortController();
        video = makeOverlay();
        syncGeometry();
        watchGeometry();

        const okGeom = await waitGeometry(gen);
        if (!session.isCurrent(gen)) return;
        if (!okGeom) { session.fallback(gen, 'geometry-timeout'); return; }
        syncGeometry();

        if (typeof RTCPeerConnection !== 'function') {
            session.fallback(gen, 'webrtc-unavailable');
            return;
        }

        try {
            await connect(gen, rescue);
        } catch (e) {
            if (session.isCurrent(gen)) session.fallback(gen, 'connect-failed: ' + (e && e.message ? e.message : e));
        }
    }

    function attachCursorChannel(peer, gen) {
        let dc;
        try {
            dc = peer.createDataChannel(CURSOR_CHANNEL_LABEL);
        } catch (e) { return; } // без курсора, але відео живе
        dc.binaryType = 'arraybuffer';
        if (cursorLayer) { try { cursorLayer.destroy(); } catch (e) { /* ignore */ } }
        const layer = createCursorLayer({
            doc,
            container,
            role: resolveCursorRole(config),
            // Ввід ловить Mesh-canvas (відео — pointer-events:none).
            targets: () => [meshCanvas, container],
            place: (p) => {
                if (!video || !p) return null;
                const srcW = p.frameW || video.videoWidth;
                const srcH = p.frameH || video.videoHeight;
                const vr = video.getBoundingClientRect();
                const cr = container.getBoundingClientRect();
                const m = mapRemoteToClient(p.x, p.y, vr, srcW, srcH);
                if (!m) return null;
                return {
                    x: vr.left - cr.left + container.scrollLeft + m.x,
                    y: vr.top - cr.top + container.scrollTop + m.y,
                    scale: m.scale,
                };
            },
        });
        cursorLayer = layer;
        dc.onmessage = (ev) => {
            if (!session.isCurrent(gen) || cursorLayer !== layer) return;
            layer.onMessage(ev.data);
        };
    }

    function detachInput() {
        if (inputDom) { try { inputDom.destroy(); } catch (e) { /* ignore */ } inputDom = null; }
        if (inputChannel) {
            try { inputChannel.onopen = null; inputChannel.onclose = null; inputChannel.close(); } catch (e) { /* ignore */ }
            inputChannel = null;
        }
    }

    // armInput — після квитка: grant не control => канал закриваємо, ввід
    // лишається в Mesh. Інакше на onopen перехоплюємо ввід контейнера.
    function armInput(gen, ticket, granted) {
        const dc = inputChannel;
        if (!dc) return;
        if (!inputEnabledFor(config, resolveCursorRole(config), granted)) { detachInput(); return; }
        const attach = () => {
            if (!session.isCurrent(gen) || inputChannel !== dc || inputDom) return;
            const sender = createInputSender({
                ticket,
                send: (s) => { if (dc.readyState === 'open') dc.send(s); else throw new Error('closed'); },
                allowed: typeof config.inputAllowed === 'function' ? config.inputAllowed : null,
            });
            inputDom = attachInputDom({
                target: container, doc, sender, containBox,
                geometry: () => {
                    if (!video || !(video.videoWidth > 0)) return null;
                    return { rect: video.getBoundingClientRect(), srcW: video.videoWidth, srcH: video.videoHeight };
                },
            });
        };
        dc.onclose = () => {
            if (inputChannel !== dc) return;
            if (inputDom) { try { inputDom.destroy(); } catch (e) { /* ignore */ } inputDom = null; }
        };
        if (dc.readyState === 'open') attach(); else dc.onopen = attach;
    }

    // N6: прибрати peer, який пробував пряму ногу й не зʼєднався (його
    // канали теж), не чіпаючи overlay/сесію.
    function dropTrialPeer(peer) {
        try { peer.ontrack = null; peer.onconnectionstatechange = null; peer.oniceconnectionstatechange = null; } catch (e) { /* ignore */ }
        if (cursorLayer) { try { cursorLayer.destroy(); } catch (e) { /* ignore */ } cursorLayer = null; }
        detachInput();
        if (tilesChannel) { try { tilesChannel.onmessage = null; tilesChannel.close(); } catch (e) { /* ignore */ } tilesChannel = null; }
        if (textTiles) { try { textTiles.destroy(); } catch (e) { /* ignore */ } textTiles = null; }
        if (disconnectGrace) { try { disconnectGrace.cancel(); } catch (e) { /* ignore */ } disconnectGrace = null; }
        try { peer.close(); } catch (e) { /* ignore */ }
    }

    async function preparePeer(gen) {
        const peer = new RTCPeerConnection(buildRtcConfig(config));
        pc = peer;
        peer.addTransceiver('video', { direction: 'recvonly' });
        // Звук — під тим самим прапорцем, що й на хабі (OO_SCREEN_AUDIO), лише з
        // цього боку він приходить через config. Без config.audio offer лишається
        // бітово тим, що прод шле сьогодні: без аудіо-m-рядка хаб не має куди
        // покласти доріжку, навіть якщо його прапорець увімкнено.
        if (config.audio) peer.addTransceiver('audio', { direction: 'recvonly' });
        // Текстові тайли (STAGE3-444 B) — ЛИШЕ під config.textTiles: без нього
        // offer бітово той самий (жодного m=application). Канал створюємо ДО
        // offer-а; хаб (OO_SCREEN_TILES=1) шле ним тайли від агента.
        if (config.textTiles) {
            textTiles = createTileOverlay({ doc, container, containBox });
            syncGeometry();
            tilesChannel = peer.createDataChannel(TILES_LABEL);
            tilesChannel.binaryType = 'arraybuffer';
            tilesChannel.onmessage = (ev) => {
                if (!session.isCurrent(gen) || !textTiles) return;
                textTiles.onMessage(ev.data);
            };
        }
        // Шар курсора — ЛИШЕ під config.cursorLayer (типово вимкнено; агентові
        // потрібен -cursor-layer). Канал відкриває браузер ДО offer-а, хаб ловить
        // його спільним диспетчером OnDataChannel viewer-ноги (як input/tiles).
        // Без прапорця offer бітово той самий, що й раніше.
        if (config.cursorLayer) attachCursorChannel(peer, gen);
        // F5: канал вводу — ЛИШЕ під config.inputChannel і лише для ролі control.
        // Відкриваємо ДО offer-а; слухачі DOM чіпляємо, коли відомий grant
        // квитка і канал відкритий (armInput нижче). Мовчазний канал хаб не
        // карає: judgeInput судить повідомлення, а не сам факт відкриття.
        if (inputEnabledFor(config, resolveCursorRole(config), config.inputGrant ?? 'control')) { // мовчазний канал; DOM лише при явному grant (armInput)
            try { inputChannel = peer.createDataChannel(INPUT_CHANNEL_LABEL, { ordered: true }); } catch (e) { inputChannel = null; }
        }

        peer.ontrack = (ev) => {
            if (!session.isCurrent(gen) || !video) return;
            // Обидві доріжки їдуть ОДНИМ MediaStream (msid "oo-screen-hub"), тож
            // srcObject ставимо рівно раз — на відео. Аудіо приїжджає другим
            // ontrack у той самий stream і потрапляє в той самий <video>, який
            // muted: звук ЙДЕ по трубі й доходить до елемента, але не звучить,
            // поки його не розглушать (setMuted нижче). Ніхто не має несподівано
            // почути чужий кабінет.
            if (ev.track && ev.track.kind !== 'video') return;
            // Low latency: мінімальний jitter-буфер (config.lowLatency, за замовч. true).
            // Feature-detect усередині — Firefox просто пропускає.
            if (config.lowLatency !== false) applyLowLatency([ev.receiver]);
            video.srcObject = (ev.streams && ev.streams[0]) || new MediaStream([ev.track]);
            const p = video.play();
            if (p && typeof p.catch === 'function') p.catch(() => { /* autoplay muted — не має падати */ });
            armFrameSource(gen);
        };

        // §MAJOR-6: 'disconnected' крізь grace-таймер, 'failed'/'closed' — одразу.
        disconnectGrace = createDisconnectGrace({
            graceMs: config.disconnectGraceMs || DEFAULT_DISCONNECT_GRACE_MS,
            onFallback: (reason) => { if (session.isCurrent(gen)) session.fallback(gen, reason); },
        });
        const grace = disconnectGrace;
        peer.onconnectionstatechange = () => {
            if (!session.isCurrent(gen) || peer !== pc || p2pTrialPeer === peer) return;
            grace.note(peer.connectionState);
        };

        const offer = await peer.createOffer();
        // Звук (F1): правка SDP лише коли звук просили — без config.audio
        // offer бітово той самий, що й до Opus.
        await peer.setLocalDescription(config.audio
            ? { type: offer.type, sdp: opusStereoSdp(offer.sdp) }
            : offer);
        await waitIceGathering(peer);
        return peer;
    }

    async function connect(gen, rescue) {
        const peer = await preparePeer(gen);
        if (!session.isCurrent(gen)) return;

        // §6.4 / BLOCKER-1,3: свіжий одноразовий ticket на цю ноду САМЕ перед
        // offer-ом (щоб не згорів по TTL, поки збирався ICE). node_id уже в
        // claims ticket-а — hub звʼяже глядача з publisher-ом цієї ноди.
        // N6-порятунок: ERP-квиток не потрібен — є одноразовий relay_ticket.
        const ticketResp = rescue
            ? { ticket: rescue.relayTicket, signalUrl: rescue.signalUrl, grant: rescue.grant }
            : await config.requestTicket();
        const { ticket, signalUrl, grant: granted } = ticketResp || {};
        if (!session.isCurrent(gen)) return;
        if (!ticket || !signalUrl) throw new Error('offer/viewer: немає ticket або signalUrl');
        armInput(gen, ticket, granted);

        // §MAJOR-5: signal кожної спроби — teardown-abort АБО таймаут.
        // O2: без config.standbySignalUrls — один кандидат, як раніше.
        // offer несе ОДНОРАЗОВИЙ ticket, не довгоживучий токен.
        // F6: monitor>0 — потік додаткового монітора (desktop-oo-multimon.js);
        // 0/відсутнє — поле не шлемо зовсім, offer як до F6.
        // N6: config.p2p (типово вимкнено) — спершу пряма нога (negotiateViewer);
        // без нього — рівно той самий postOfferWithFailover, що й раніше.
        // Додаткові монітори (F6) лишаються на relay.
        const p2pOn = !rescue && config.p2p === true && !(Number.isInteger(config.monitor) && config.monitor > 0);
        if (p2pOn) p2pTrialPeer = peer;
        let res;
        try {
            res = await negotiateViewer({
                p2p: p2pOn,
                p2pUrl: config.p2pUrl,
                relayTicket: rescue ? rescue.relayTicket : null,
                signalUrl,
                standby: resolveStandby(ticketResp, config),
                ticket,
                grant: granted,
                requestTicket: config.requestTicket,
                peer,
                rebuildPeer: async () => {
                    p2pTrialPeer = null;
                    dropTrialPeer(peer);
                    const np = await preparePeer(gen);
                    if (!session.isCurrent(gen)) throw new Error('teardown');
                    return np;
                },
                waitIce: (p) => waitIceOutcome(p, config.p2pConnectTimeoutMs || DEFAULT_P2P_CONNECT_MS),
                makeBody: (sdp, t) => offerBody(sdp, t, config.monitor),
                onTicket: (t, g) => { if (session.isCurrent(gen)) armInput(gen, t, g); },
                onPath: (p) => { if (session.isCurrent(gen)) transportPath = p; },
                isCurrent: () => session.isCurrent(gen),
                teardownSignal: abort && abort.signal,
                timeoutMs: config.offerTimeoutMs || DEFAULT_OFFER_TIMEOUT_MS,
                fetchFn: (u, init) => fetch(u, init),
            });
        } finally {
            p2pTrialPeer = null;
        }
        if (!res || !session.isCurrent(gen)) return;
        if (res.path === PATH_DIRECT) directRescue.arm(res.relayTicket, granted, signalUrl);

        // Аж ТЕПЕР глушимо Mesh і армимо 8с-сторож: SDP-обмін позаду, тож
        // зрив сигналізації не встиг коштувати нікому чорного екрана.
        paused = meshCall(meshDesktop, MESH_PAUSE_NAMES);
        session.arm(gen);
    }

    start();

    return {
        state: () => session.state(),
        // N6: 'direct' | 'relay' | null (ще не вирішено).
        transport: () => transportPath,
        generation: () => session.current(),
        // setMuted — ручка гучності кроку 1: доріжка вже в елементі, лишається
        // її розглушити. UI-кнопки НЕМАЄ навмисно; поки джерело звуку —
        // тестовий тон хаба, кнопка вмикала б людині не кабінет, а пищалку.
        // Стартовий стан завжди muted, тож замовчування = тиша.
        setMuted(m) { if (video) video.muted = m !== false; },
        // TZ 1.1: 'fit' | '1:1'; зберігається в localStorage.
        getDisplayMode: () => displayMode,
        setDisplayMode,
        destroy(reason) {
            if (destroyed) return;
            destroyed = true;
            session.close(session.current(), reason || 'destroy');
            restoreMesh();
            unpauseMesh();
            teardownOo();
        },
    };
}
