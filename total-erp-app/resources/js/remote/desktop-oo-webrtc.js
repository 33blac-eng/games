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
// DOM-шар: overlay <video> + RTCPeerConnection recvonly
// ─────────────────────────────────────────────────────────────────────────────

/**
 * createOoWebrtcLayer — публічна точка входу (аналог createOoLayer).
 *
 * @param {Element}  o.container
 * @param {object}   o.meshDesktop
 * @param {object}   o.config  { requestTicket, firstFrameMs?, frameAgeMs?,
 *                               geometryTimeoutMs?, offerTimeoutMs?, iceServers?,
 *                               disconnectGraceMs? }
 *   requestTicket() → Promise<{ticket, signalUrl}> — §6.4 свіжий одноразовий
 *   ticket на цю ноду; offer їде з ticket, НЕ з довгоживучим токеном (BLOCKER-1/3).
 * @param {Function} o.onStateChange(state, reason)
 * @returns {{destroy: Function, state: Function, generation: Function}}
 */
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
        v.style.cssText = 'position:absolute;pointer-events:none;z-index:5;object-fit:fill;background:transparent;';
        const cs = doc.defaultView && doc.defaultView.getComputedStyle(container);
        if (cs && cs.position === 'static') container.style.position = 'relative';
        container.appendChild(v);
        return v;
    }

    // Геометрія — та сама логіка, що в canvas-версії: міряємо від ФАКТИЧНОГО
    // боксу Mesh-canvas, а не від контейнера (Mesh лєтербоксить картинку, і
    // «розтягнути на контейнер» дало б зсув між тим, що видно, і тим, куди
    // клікає користувач).
    function syncGeometry() {
        if (!video) return;
        const mr = meshCanvas.getBoundingClientRect();
        const cr = container.getBoundingClientRect();
        video.style.left = (mr.left - cr.left + container.scrollLeft) + 'px';
        video.style.top = (mr.top - cr.top + container.scrollTop) + 'px';
        video.style.width = mr.width + 'px';
        video.style.height = mr.height + 'px';
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
        if (pc) {
            // Порядок: спершу глушимо колбеки, потім close(). Інакше
            // connectionstatechange='closed' від НАШОГО ж close() прилітає як
            // «сесія померла» і затирає причину справжнього фолбеку.
            try { pc.ontrack = null; pc.onconnectionstatechange = null; pc.oniceconnectionstatechange = null; } catch (e) { /* ignore */ }
            try { pc.close(); } catch (e) { /* ignore */ }
            pc = null;
        }
    }

    function doFallback() {
        // Спершу повертаємо Mesh-картинку, потім знімаємо OO-шар — зворотний
        // порядок дає видиму дірку, у яку встигають клікнути.
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
            const onFrame = () => {
                if (!session.isCurrent(gen) || !video) return;
                session.noteFrame(gen);
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

    async function start() {
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
            await connect(gen);
        } catch (e) {
            if (session.isCurrent(gen)) session.fallback(gen, 'connect-failed: ' + (e && e.message ? e.message : e));
        }
    }

    async function connect(gen) {
        const peer = new RTCPeerConnection({ iceServers: config.iceServers || [] });
        pc = peer;
        peer.addTransceiver('video', { direction: 'recvonly' });
        // Звук — під тим самим прапорцем, що й на хабі (OO_SCREEN_AUDIO), лише з
        // цього боку він приходить через config. Без config.audio offer лишається
        // бітово тим, що прод шле сьогодні: без аудіо-m-рядка хаб не має куди
        // покласти доріжку, навіть якщо його прапорець увімкнено.
        if (config.audio) peer.addTransceiver('audio', { direction: 'recvonly' });

        peer.ontrack = (ev) => {
            if (!session.isCurrent(gen) || !video) return;
            // Обидві доріжки їдуть ОДНИМ MediaStream (msid "oo-screen-hub"), тож
            // srcObject ставимо рівно раз — на відео. Аудіо приїжджає другим
            // ontrack у той самий stream і потрапляє в той самий <video>, який
            // muted: звук ЙДЕ по трубі й доходить до елемента, але не звучить,
            // поки його не розглушать (setMuted нижче). Ніхто не має несподівано
            // почути чужий кабінет.
            if (ev.track && ev.track.kind !== 'video') return;
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
        peer.onconnectionstatechange = () => {
            if (!session.isCurrent(gen)) return;
            disconnectGrace.note(peer.connectionState);
        };

        const offer = await peer.createOffer();
        await peer.setLocalDescription(offer);
        await waitIceGathering(peer);
        if (!session.isCurrent(gen)) return;

        // §6.4 / BLOCKER-1,3: свіжий одноразовий ticket на цю ноду САМЕ перед
        // offer-ом (щоб не згорів по TTL, поки збирався ICE). node_id уже в
        // claims ticket-а — hub звʼяже глядача з publisher-ом цієї ноди.
        const { ticket, signalUrl } = await config.requestTicket();
        if (!session.isCurrent(gen)) return;
        if (!ticket || !signalUrl) throw new Error('offer/viewer: немає ticket або signalUrl');

        // §MAJOR-5: ОДИН signal — teardown-abort АБО таймаут. Раніше fetch слухав
        // лише AbortSignal.timeout() окремо від teardown, тож знищення шару не
        // рвало застарілий offer-fetch.
        const combined = combineAbortSignals(abort && abort.signal, config.offerTimeoutMs || DEFAULT_OFFER_TIMEOUT_MS);
        let resp;
        try {
            resp = await fetch(signalUrl, {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                // offer несе ОДНОРАЗОВИЙ ticket, не довгоживучий токен.
                body: JSON.stringify({ sdp: peer.localDescription.sdp, ticket }),
                signal: combined.signal,
            });
        } finally {
            combined.cancel();
        }
        if (!session.isCurrent(gen)) return;
        if (!resp.ok) throw new Error('offer/viewer ' + resp.status);
        const answer = await resp.json();
        if (!session.isCurrent(gen)) return;
        if (!answer || !answer.sdp) throw new Error('offer/viewer: порожній answer');

        await peer.setRemoteDescription({ type: 'answer', sdp: answer.sdp });
        if (!session.isCurrent(gen)) return;

        // Аж ТЕПЕР глушимо Mesh і армимо 8с-сторож: SDP-обмін позаду, тож
        // зрив сигналізації не встиг коштувати нікому чорного екрана.
        paused = meshCall(meshDesktop, MESH_PAUSE_NAMES);
        session.arm(gen);
    }

    start();

    return {
        state: () => session.state(),
        generation: () => session.current(),
        // setMuted — ручка гучності кроку 1: доріжка вже в елементі, лишається
        // її розглушити. UI-кнопки НЕМАЄ навмисно; поки джерело звуку —
        // тестовий тон хаба, кнопка вмикала б людині не кабінет, а пищалку.
        // Стартовий стан завжди muted, тож замовчування = тиша.
        setMuted(m) { if (video) video.muted = m !== false; },
        destroy(reason) {
            if (destroyed) return;
            destroyed = true;
            session.close(session.current(), reason || 'destroy');
            unpauseMesh();
            teardownOo();
        },
    };
}
