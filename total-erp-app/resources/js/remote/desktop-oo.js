// desktop-oo.js — композитна desktop-сесія (§7 плану oo-screen, Ф0).
//
// Ідея: Mesh-canvas ЛИШАЄТЬСЯ input-поверхнею (весь ввід, фокус, клавіші —
// його), а зверху кладемо власний OO-canvas з pointer-events:none, у який
// малюємо H.264 з hub-а через WebTransport + WebCodecs. Тобто ми НЕ підміняємо
// desktop.js, а домальовуємо поверх нього. desktop.js/control.js сьогодні
// тримають рівно один активний модуль і прибивають ввід до свого canvas —
// саме тому композит, а не заміна.
//
// Самодостатність: цей файл НЕ редагує і НЕ імпортує control.js/desktop.js.
// Інтеграційний виклик createOoLayer() робиться наступною хвилею (після
// bake-off A vs B), тому все спілкування з Mesh — через duck-typing по
// переданому meshDesktop (див. meshCall нижче).
//
// Три речі, на яких така композиція ламається тихо, і що з ними тут зроблено:
//  1. Розʼїзд геометрії. Клік по Mesh-canvas масштабується від його ВНУТРІШНІХ
//     width/height. Якщо OO-шар має інші розміри або зміщення — картинка
//     бреше, і людина клікає не туди, куди дивиться. Тому синхронізуємо і
//     внутрішні атрибути, і CSS-бокс (ResizeObserver + MutationObserver на
//     width/height/style Mesh-canvas).
//  2. Пережитки старої сесії. Reconnect/таймер/decoder-output зі СТАРОЇ сесії,
//     який доїхав після старту нової, малює чужий кадр або гасить живий шар.
//     Тому generation token: кожен колбек звіряє свій gen, старий = no-op.
//  3. Мовчазне «чорне вікно». Якщо OO-шар не поїхав, а Mesh уже на паузі —
//     користувач бачить застиглий екран і жодної помилки. Тому фолбек ЗАВЖДИ
//     атомарний: unpause + refresh Mesh і зняття OO-шару однією операцією,
//     і не лише на старті (8с), а безперервно (frame-age > 3с).

export const OO_STATE_CONNECTING = 'connecting';
export const OO_STATE_LIVE = 'live';
export const OO_STATE_FALLBACK = 'fallback';
export const OO_STATE_CLOSED = 'closed';

const DEFAULT_FIRST_FRAME_MS = 8000;   // §7: перший OO-кадр за 8с — інакше Mesh
const DEFAULT_FRAME_AGE_MS = 3000;     // §7: безперервний watchdog, не лише старт
const DEFAULT_TICK_MS = 500;           // крок перевірки frame-age
const DEFAULT_GEOMETRY_MS = 10000;     // скільки чекаємо геометрію від Mesh
const DEFAULT_CODEC = 'avc1.64002A';

// ─────────────────────────────────────────────────────────────────────────────
// ЧИСТА ЛОГІКА (без DOM, без WebTransport) — рівно цю частину покриває
// desktop-oo.test.mjs. Усе, що стосується часу, приходить через clock, щоб
// тест ганяв фейковий годинник, а не спав реальні 8 секунд.
// ─────────────────────────────────────────────────────────────────────────────

export function realClock() {
    return {
        now: () => (typeof performance !== 'undefined' ? performance.now() : Date.now()),
        setTimeout: (fn, ms) => setTimeout(fn, ms),
        clearTimeout: (id) => clearTimeout(id),
    };
}

/**
 * createOoSession — стан композитної сесії + обидва сторожі.
 *
 * Ключове: КОЖЕН публічний метод приймає gen і мовчки ігнорує виклик зі
 * старої генерації. Це не «на всяк випадок»: WebTransport-reader, decoder
 * output і reconnect-таймер живуть довше за сесію, яка їх породила, і
 * прилітають ПІСЛЯ того, як користувач уже перевідкрив екран.
 *
 * onFallback(gen, reason) — сюди DOM-шар вішає атомарне unpause+refresh+зняти
 * canvas. Викликається РІВНО один раз на генерацію.
 */
export function createOoSession(options) {
    const opts = options || {};
    const clock = opts.clock || realClock();
    const firstFrameMs = opts.firstFrameMs || DEFAULT_FIRST_FRAME_MS;
    const frameAgeMs = opts.frameAgeMs || DEFAULT_FRAME_AGE_MS;
    const tickMs = opts.tickMs || DEFAULT_TICK_MS;
    const onStateChange = opts.onStateChange || (() => {});
    const onFallback = opts.onFallback || (() => {});

    let generation = 0;
    let state = OO_STATE_CLOSED;
    let armedAt = -1;        // момент, з якого рахуємо 8с на перший кадр
    let lastFrameAt = -1;    // момент останнього кадру (frame-age watchdog)
    let sawFrame = false;
    let tickTimer = null;
    let finished = true;     // fallback/close уже відпрацював для цієї генерації

    function emit(next, reason) {
        state = next;
        try { onStateChange(next, reason); } catch (e) { /* журнал не блокер сесії */ }
    }

    function stopTicking() {
        if (tickTimer !== null) { clock.clearTimeout(tickTimer); tickTimer = null; }
    }

    function scheduleTick() {
        stopTicking();
        tickTimer = clock.setTimeout(() => { tickTimer = null; tick(); }, tickMs);
    }

    // tick — ОДИН сторож на дві умови. Розділяти їх на два таймери спокусливо,
    // але тоді при переході «перший кадр приїхав» треба гасити один і піднімати
    // інший, і рівно в цю щілину сесія лишається без сторожа взагалі.
    function tick() {
        if (finished || armedAt < 0) return;
        const t = clock.now();
        if (!sawFrame) {
            if (t - armedAt >= firstFrameMs) { fallback(generation, 'first-frame-timeout'); return; }
        } else if (t - lastFrameAt >= frameAgeMs) {
            fallback(generation, 'frame-age-timeout');
            return;
        }
        scheduleTick();
    }

    return {
        current: () => generation,
        state: () => state,
        isCurrent: (gen) => gen === generation && !finished,

        /** begin — нова генерація. Усе старе автоматично стає no-op. */
        begin() {
            stopTicking();
            generation += 1;
            armedAt = -1;
            lastFrameAt = -1;
            sawFrame = false;
            finished = false;
            emit(OO_STATE_CONNECTING, 'begin');
            return generation;
        },

        /**
         * arm — Mesh віддав геометрію, Mesh-картинку поставлено на паузу,
         * відлік 8с пішов. Раніше за паузу арміти НЕ можна: інакше 8с течуть,
         * поки ми ще чекаємо геометрію, і фолбек спрацює на живій сесії.
         */
        arm(gen) {
            if (gen !== generation || finished) return false;
            armedAt = clock.now();
            scheduleTick();
            return true;
        },

        /** noteFrame — приїхав OO-кадр. Перший переводить у live. */
        noteFrame(gen) {
            if (gen !== generation || finished) return false;
            lastFrameAt = clock.now();
            if (!sawFrame) {
                sawFrame = true;
                if (armedAt < 0) armedAt = lastFrameAt;
                emit(OO_STATE_LIVE, 'first-frame');
                scheduleTick();
            }
            return true;
        },

        fallback(gen, reason) { return fallback(gen, reason); },

        close(gen, reason) {
            if (gen !== undefined && gen !== generation) return false;
            if (finished) return false;
            finished = true;
            stopTicking();
            emit(OO_STATE_CLOSED, reason || 'closed');
            return true;
        },
    };

    function fallback(gen, reason) {
        if (gen !== generation || finished) return false;
        finished = true;
        stopTicking();
        emit(OO_STATE_FALLBACK, reason || 'fallback');
        try { onFallback(gen, reason); } catch (e) { /* фолбек мусить добігти */ }
        return true;
    }
}

/**
 * meshCall — виклик методу Mesh-об'єкта по списку можливих імен.
 *
 * Навіщо: control.js віддає СВОЮ обгортку (stop/refresh/setDisplay…), а сирий
 * agent-desktop-0.0.2.js — свої SendPause/SendUnPause/SendRefresh. Модуль має
 * працювати з обома, не редагуючи жоден з них. Повертає true, якщо щось таки
 * викликали — це важливо: якщо паузи НЕМА, ми не маємо права глушити Mesh і
 * лишаємось у чесному режимі «OO поверх живого Mesh».
 */
export function meshCall(target, names, args) {
    if (!target) return false;
    for (const name of names) {
        const fn = target[name];
        if (typeof fn === 'function') {
            try { fn.apply(target, args || []); return true; } catch (e) { return false; }
        }
    }
    return false;
}

const PAUSE_NAMES = ['SendPause', 'sendPause', 'pause'];
const UNPAUSE_NAMES = ['SendUnPause', 'SendUnpause', 'sendUnPause', 'unpause', 'resume'];
const REFRESH_NAMES = ['SendRefresh', 'refresh'];

// Реекспорт для інших OO-транспортів (desktop-oo-webrtc.js): списки імен і
// пошук Mesh-canvas — спільна частина композиції, а не деталь WebTransport.
// Внутрішні імена НЕ чіпаємо, щоб не рухати решту файла.
export const MESH_PAUSE_NAMES = PAUSE_NAMES;
export const MESH_UNPAUSE_NAMES = UNPAUSE_NAMES;
export const MESH_REFRESH_NAMES = REFRESH_NAMES;

// ─────────────────────────────────────────────────────────────────────────────
// DOM-шар
// ─────────────────────────────────────────────────────────────────────────────

export function findMeshCanvas(meshDesktop, container) {
    if (meshDesktop) {
        for (const key of ['canvas', 'Canvas', 'CanvasId', 'canvasElement']) {
            const c = meshDesktop[key];
            if (c && typeof c.getContext === 'function') return c;
        }
    }
    if (container && typeof container.querySelector === 'function') {
        return container.querySelector('canvas');
    }
    return null;
}

/**
 * createOoLayer — публічна точка входу.
 *
 * @param {object}      o.container    елемент, у якому живе Mesh-canvas (position буде
 *                                     виставлено в relative, якщо він static)
 * @param {object}      o.meshDesktop  об'єкт Mesh-сесії (обгортка з desktop.js АБО сирий
 *                                     agent-desktop). З нього беремо canvas і pause/refresh.
 * @param {object}      o.config       { url, ticket, certhash?, codec?, firstFrameMs?,
 *                                       frameAgeMs?, geometryTimeoutMs? }
 * @param {Function}    o.onStateChange(state, reason)
 * @returns {{destroy: Function, state: Function, generation: Function}}
 */
export function createOoLayer(o) {
    const opts = o || {};
    const container = opts.container;
    const meshDesktop = opts.meshDesktop;
    const config = opts.config || {};
    const onStateChange = opts.onStateChange || (() => {});

    const doc = (container && container.ownerDocument) || (typeof document !== 'undefined' ? document : null);
    if (!doc) throw new Error('createOoLayer: потрібен DOM');

    const meshCanvas = findMeshCanvas(meshDesktop, container);
    if (!meshCanvas) throw new Error('createOoLayer: не знайдено Mesh-canvas');

    let abort = null;
    let worker = null;
    let transport = null;
    let overlay = null;
    let resizeObserver = null;
    let attrObserver = null;
    let paused = false;
    let geometryTimer = null;
    let destroyed = false;

    const session = createOoSession({
        firstFrameMs: config.firstFrameMs,
        frameAgeMs: config.frameAgeMs,
        onStateChange,
        onFallback: (gen, reason) => doFallback(gen, reason),
    });

    // ── overlay + синхронізація геометрії ────────────────────────────────────
    function makeOverlay() {
        const c = doc.createElement('canvas');
        c.className = 'oo-screen-layer';
        // pointer-events:none — НЕ косметика: без цього шар зʼїдає mousedown і
        // Mesh перестає отримувати ввід, а виглядає це як «завис віддалений ПК».
        c.style.cssText = 'position:absolute;pointer-events:none;z-index:5;';
        const cs = doc.defaultView && doc.defaultView.getComputedStyle(container);
        if (cs && cs.position === 'static') container.style.position = 'relative';
        container.appendChild(c);
        return c;
    }

    function syncGeometry() {
        if (!overlay) return;
        const w = meshCanvas.width | 0;
        const h = meshCanvas.height | 0;
        if (w > 0 && h > 0 && (overlay.width !== w || overlay.height !== h)) {
            overlay.width = w;
            overlay.height = h;
            if (worker) worker.postMessage({ type: 'resize', width: w, height: h, gen: session.current() });
        }
        // CSS-бокс міряємо від фактичного положення Mesh-canvas усередині
        // контейнера, а не від самого контейнера: Mesh-canvas зазвичай
        // відцентрований/лєтербоксований, і «розтягнути на контейнер» дало б
        // рівно той зсув, через який клік і картинка розʼїжджаються.
        const mr = meshCanvas.getBoundingClientRect();
        const cr = container.getBoundingClientRect();
        overlay.style.left = (mr.left - cr.left + container.scrollLeft) + 'px';
        overlay.style.top = (mr.top - cr.top + container.scrollTop) + 'px';
        overlay.style.width = mr.width + 'px';
        overlay.style.height = mr.height + 'px';
    }

    function watchGeometry() {
        if (typeof ResizeObserver === 'function') {
            resizeObserver = new ResizeObserver(() => syncGeometry());
            resizeObserver.observe(meshCanvas);
            resizeObserver.observe(container);
        }
        if (typeof MutationObserver === 'function') {
            // ResizeObserver бачить CSS-бокс, але НЕ бачить зміну внутрішніх
            // width/height — а Mesh міняє саме їх, коли агент рапортує новий
            // розмір екрана. Без цього спостерігача OO-шар лишається в старій
            // роздільності й картинка мило їде.
            attrObserver = new MutationObserver(() => syncGeometry());
            attrObserver.observe(meshCanvas, { attributes: true, attributeFilter: ['width', 'height', 'style', 'class'] });
        }
    }

    // ── фолбек: атомарно повертаємо Mesh і знімаємо OO ───────────────────────
    function unpauseMesh() {
        if (!paused) return;
        paused = false;
        meshCall(meshDesktop, UNPAUSE_NAMES);
        meshCall(meshDesktop, REFRESH_NAMES); // без refresh Mesh лишається на старому кадрі
    }

    function teardownOo() {
        if (geometryTimer !== null) { clearTimeout(geometryTimer); geometryTimer = null; }
        if (resizeObserver) { try { resizeObserver.disconnect(); } catch (e) { /* ignore */ } resizeObserver = null; }
        if (attrObserver) { try { attrObserver.disconnect(); } catch (e) { /* ignore */ } attrObserver = null; }
        if (worker) { try { worker.terminate(); } catch (e) { /* ignore */ } worker = null; }
        if (transport) { try { transport.close(); } catch (e) { /* ignore */ } transport = null; }
        if (abort) { try { abort.abort(); } catch (e) { /* ignore */ } abort = null; }
        if (overlay && overlay.parentNode) { try { overlay.parentNode.removeChild(overlay); } catch (e) { /* ignore */ } }
        overlay = null;
    }

    function doFallback(gen, reason) {
        // Порядок навмисний: спершу повертаємо Mesh-картинку, потім знімаємо
        // OO-шар. Зворотний порядок дає видиму дірку (порожній контейнер), у яку
        // користувач встигає клікнути.
        unpauseMesh();
        teardownOo();
    }

    // ── старт ────────────────────────────────────────────────────────────────
    function geometryReady() {
        return (meshCanvas.width | 0) > 0 && (meshCanvas.height | 0) > 0;
    }

    async function start() {
        const gen = session.begin();
        abort = new AbortController();
        overlay = makeOverlay();
        syncGeometry();
        watchGeometry();

        // 1) чекаємо ГЕОМЕТРІЮ від Mesh. Ставити паузу раніше не можна:
        //    без розміру ми не знаємо ні конфіг декодера, ні розмір шару.
        const okGeom = await waitGeometry(gen);
        if (!session.isCurrent(gen)) return;
        if (!okGeom) { session.fallback(gen, 'geometry-timeout'); return; }
        syncGeometry();

        // 2) preflight ДО відкриття OO-сесії (§10). Якщо декодер не тягне
        //    точний конфіг — падаємо в Mesh, НЕ поставивши його на паузу і не
        //    витративши на це 8 секунд чорного екрана.
        let pre;
        try {
            pre = await startWorkerAndPreflight(gen);
        } catch (e) {
            if (session.isCurrent(gen)) session.fallback(gen, 'worker-failed: ' + (e && e.message ? e.message : e));
            return;
        }
        if (!session.isCurrent(gen)) return;
        if (!pre || !pre.supported) {
            session.fallback(gen, 'decoder-unsupported: ' + ((pre && pre.reason) || 'unknown'));
            return;
        }

        // 3) глушимо Mesh-картинку і аж тепер армимо 8с-сторож.
        paused = meshCall(meshDesktop, PAUSE_NAMES);
        session.arm(gen);

        // 4) конект. З цього моменту будь-яка помилка — це фолбек, а не throw:
        //    сесія вже на паузі, кидати виняток нагору = лишити чорний екран.
        try {
            await connect(gen);
        } catch (e) {
            if (session.isCurrent(gen)) session.fallback(gen, 'connect-failed: ' + (e && e.message ? e.message : e));
        }
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

    function startWorkerAndPreflight(gen) {
        return new Promise((resolve, reject) => {
            worker = new Worker(new URL('./desktop-oo-worker.js', import.meta.url), { type: 'module' });
            worker.onerror = (e) => {
                if (!session.isCurrent(gen)) return;
                session.fallback(gen, 'worker-error: ' + ((e && e.message) || 'unknown'));
            };
            worker.onmessage = (ev) => {
                const m = ev.data || {};
                // Кожне повідомлення воркера звіряє генерацію: воркер старої
                // сесії ще дихає до terminate(), і його 'frame' інакше
                // «оживив» би вже впалу нову сесію.
                if (m.gen !== undefined && m.gen !== gen) return;
                if (!session.isCurrent(gen)) return;
                switch (m.type) {
                    case 'preflight': resolve({ supported: !!m.supported, reason: m.reason }); break;
                    case 'frame': session.noteFrame(gen); break;
                    case 'fatal': session.fallback(gen, 'decoder: ' + (m.message || 'fatal')); break;
                    default: break;
                }
            };
            worker.postMessage({
                type: 'preflight',
                gen,
                codec: config.codec || DEFAULT_CODEC,
                width: meshCanvas.width | 0,
                height: meshCanvas.height | 0,
            });
            setTimeout(() => reject(new Error('preflight timeout')), 4000);
        });
    }

    async function connect(gen) {
        if (typeof WebTransport !== 'function') throw new Error('WebTransport недоступний');
        const wtOpts = {};
        if (config.certhash) {
            // dev-параметр: самопідписаний серт hub-а. У проді certhash не
            // передається взагалі — там звичайний ланцюжок довіри.
            // Канон — base64 (як друкує hub-wt і як його читають viewer-wt.html
            // та bench/capture.py), НЕ hex.
            wtOpts.serverCertificateHashes = [{ algorithm: 'sha-256', value: base64ToBytes(config.certhash) }];
        }
        const wt = new WebTransport(config.url, wtOpts);
        transport = wt;
        await wt.ready;
        if (!session.isCurrent(gen)) { try { wt.close(); } catch (e) { /* ignore */ } return; }

        const control = await wt.createBidirectionalStream();
        if (!session.isCurrent(gen)) { try { wt.close(); } catch (e) { /* ignore */ } return; }

        const off = overlay.transferControlToOffscreen();
        worker.postMessage({
            type: 'init',
            gen,
            canvas: off,
            writable: control.writable,
            readable: wt.incomingUnidirectionalStreams,
            ticket: config.ticket || null,
            codec: config.codec || DEFAULT_CODEC,
        }, [off, control.writable, wt.incomingUnidirectionalStreams]);

        wt.closed.then(() => {
            if (session.isCurrent(gen)) session.fallback(gen, 'transport-closed');
        }).catch(() => {
            if (session.isCurrent(gen)) session.fallback(gen, 'transport-error');
        });
    }

    start();

    return {
        state: () => session.state(),
        generation: () => session.current(),
        destroy(reason) {
            if (destroyed) return;
            destroyed = true;
            const gen = session.current();
            session.close(gen, reason || 'destroy');
            unpauseMesh();
            teardownOo();
        },
    };
}

export function hexToBytes(hex) {
    const clean = String(hex).replace(/[^0-9a-fA-F]/g, '');
    const out = new Uint8Array(clean.length >> 1);
    for (let i = 0; i < out.length; i++) out[i] = parseInt(clean.substr(i * 2, 2), 16);
    return out;
}

// base64ToBytes — certhash від hub-wt (CERT_HASH= у stdout) друкується
// base64.StdEncoding. Якщо значення проїхало через query/атрибут, '+'
// могло перетворитись на пробіл — та сама пастка, що ловили у
// tools/oo-screen/web/viewer-wt.html; відновлюємо перед декодуванням.
export function base64ToBytes(b64) {
    const clean = String(b64).trim().replace(/ /g, '+');
    const bin = atob(clean);
    const out = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
    return out;
}
