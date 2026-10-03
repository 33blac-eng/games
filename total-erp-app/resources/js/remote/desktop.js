// resources/js/remote/desktop.js
// Обгортка ЇХНЬОГО декодера (CreateAgentRemoteDesktop + CreateAgentRedirect з
// public/scripts/agent-desktop-0.0.2.js та agent-redir-ws-0.1.1.js, той самий
// домен total.organicoils.com.ua/remote/scripts/) у НАШ canvas. Протокол і
// декодер — не наші, ми лише монтуємо офіційне API до наших grant-токенів і
// DOM. Складання рівно як в офіційному UI (views/default3.handlebars:11021):
//   CreateAgentRedirect(meshServer, CreateAgentRemoteDesktop(canvasEl), '', auth, rauth, meshBase)
//
// WebRTC P2P (пілот 26.08): після state 3 їхній redir сам пробує підняти
// datachannel до агента і перевести кадри повз реле — див. блок «P2P» нижче.
// Сигналінг їде тим самим relay-вебсокетом (sendCtrlMsg), окремого каналу не
// треба. Не вийшло — лишається websocket, фолбек їхній вбудований.

import { meshServer, grant, closeGrant, reportOpened, reportFirstFrame, loadMeshLibs, meshRelayUrlName, reportError } from './control.js';

// ponytail: конкретний список для цієї вкладки — без xterm.js (він для терміналу,
// вантажити його тут нема сенсу). Якщо data-scripts на #oo-remote-root недоступний
// (розмітка ще не встигла змонтуватись), падаємо на цей самий набір імен.
const DESKTOP_LIBS = ['scripts/common-0.0.1.js', 'scripts/agent-redir-ws-0.1.1.js', 'scripts/agent-desktop-0.0.2.js'];

function libNamesFromRoot() {
    const root = document.getElementById('oo-remote-root');
    if (!root || !root.dataset.scripts) return DESKTOP_LIBS;
    try {
        const all = JSON.parse(root.dataset.scripts);
        const kvm = Array.isArray(all) ? all.filter((n) => !String(n).includes('xterm')) : [];
        return kvm.length ? kvm : DESKTOP_LIBS;
    } catch (e) {
        return DESKTOP_LIBS;
    }
}

function meshBase() {
    const root = document.getElementById('oo-remote-root');
    return (root && root.dataset.meshBase) || '/';
}

// Прямий хост реле (в обхід Cloudflare) з config/remote_access.php →
// data-relay-direct. Порожньо = стара поведінка (реле через origin сторінки).
// Заміряно 26.08: WS-handshake meshrelay через CF 274мс, напряму 129мс.
function relayDirectBase() {
    const root = document.getElementById('oo-remote-root');
    const v = root && root.dataset.relayDirect;
    return v ? String(v).replace(/\/$/, '') : '';
}

// Запобіжник: якщо прямий хост із мережі користувача не відкрився, наступна
// спроба піде старим шляхом через origin. Ключ чиститься на кожному вдалому
// прямому підключенні.
const DIRECT_BROKEN_KEY = 'oo-remote-relay-direct-broken';
function directMarkedBroken() { try { return localStorage.getItem(DIRECT_BROKEN_KEY) === '1'; } catch (e) { return false; } }
function markDirectBroken(on) { try { on ? localStorage.setItem(DIRECT_BROKEN_KEY, '1') : localStorage.removeItem(DIRECT_BROKEN_KEY); } catch (e) { /* private mode */ } }

/**
 * openDesktop({ nodeId, canvas, onState, onMessage })
 * -> Promise<{ stop(), ctrlAltDel(), setDisplay(i), state() }>
 *
 * onState(state, reason) отримує: 'connecting' | 'connected' | 'disconnected' | 'error'
 * reason — людський текст причини (для disconnected/error), інакше не переданий.
 */
export async function openDesktop({ nodeId, canvas, onState, onMessage, onDisplays }) {
    if (!nodeId) throw new Error('openDesktop: nodeId required');
    if (!canvas || typeof canvas.getContext !== 'function') throw new Error('openDesktop: canvas element required');

    const notifyState = (state, reason) => { if (onState) { try { onState(state, reason); } catch (e) { /* caller's bug, not ours */ } } };

    // ponytail: agent-desktop-0.0.2.js (SendKeyMsg) і agent-redir-ws-0.1.1.js (Start)
    // читають бare-глобал `urlargs` (їхня власна сторінка ставить його через
    // parseUriArgs() з common-0.0.1.js — код, який ми свідомо не вантажимо). Без
    // цього перший Start()/натискання клавіші валиться ReferenceError і сесія тихо
    // гине. Дешево і безпечно продекларувати завчасно, навіть якщо хтось інший
    // теж це зробить.
    if (typeof window.urlargs === 'undefined') window.urlargs = {};

    notifyState('connecting');

    await loadMeshLibs(libNamesFromRoot());

    if (typeof window.CreateAgentRemoteDesktop !== 'function' || typeof window.CreateAgentRedirect !== 'function') {
        const msg = 'MeshCentral оновився — онови mesh_scripts у config/remote_access.php';
        reportError('desktop:missing-globals', msg);
        notifyState('error', msg);
        throw new Error(msg);
    }

    let g;
    try {
        g = await grant(nodeId, 'desktop');
    } catch (e) {
        const msg = String((e && e.message) || e);
        notifyState('error', msg);
        throw e;
    }

    const desktop = window.CreateAgentRemoteDesktop(canvas, canvas.parentElement);
    const redir = window.CreateAgentRedirect(meshServer, desktop, '', g.auth, g.rauth, meshBase());
    // Спайк 13.08: наш control-канал живе не під /remote/, тому лічений відносний
    // шлях з '../' сегментами (a la '../../remote/meshrelay.ashx') рахує control.js —
    // саме він знає глибину поточного роуту й меш-базу.
    redir.urlname = meshRelayUrlName();

    // ── P2P (WebRTC) ──────────────────────────────────────────────────────────
    // За замовчуванням їхній redir має attemptWebRTC=false (agent-redir-ws-0.1.1.js:24)
    // і читає прапорець із features&128 лише на ЇХНІЙ сторінці default3.handlebars.
    // Ми features не читаємо — вмикаємо самі. Серверний ключ settings.webrtc для
    // цього НЕ потрібен: агент приймає offer безумовно (meshcore.js:4710 — жодної
    // перевірки прапорця), біт 128 годує тільки їхній власний UI.
    // 🚨 А от desktopmultiplex на сервері P2P вбиває: мультиплексор
    // (meshdesktopmultiplex.js) не пропускає webrtc-контрол-трафік, тож offer до
    // агента не доходить. Тому в meshcentral-data/config.json його вимкнено.
    // Конфіг іде прямо в RTCPeerConnection (там же, рядок 163), тому формат —
    // рідний браузерний; iceServers регістрозалежний. STUN нашого coturn досить:
    // не склеїлось — лишаємось на websocket через реле.
    // 📏 ЗАМІРЯНО 26.08 (жива сесія на «Organic»). Сигналінг працює наскрізь:
    // агент відповів answer, наш браузер зібрав host+srflx (STUN coturn живий).
    // Але агент віддав ЛИШЕ host-кандидати 192.168.x — і ICE став checking →
    // disconnected, транспорт лишився websocket. Причина не в конфізі: слова
    // 'stun' у meshcore.js НЕМАЄ ВЗАГАЛІ, агент ніколи не отримує STUN-сервер і
    // вміє тільки свої LAN-адреси. Тобто P2P тут вмикається рівно тоді, коли
    // браузер і агент в одній мережі (офісні місця), а віддалений адмін завжди
    // піде через реле. Прапорець лишаємо ввімкненим: коштує нуль, фолбек
    // штатний, а в офісі дає реальний P2P. Щоб підняти P2P і через інтернет,
    // потрібен TURN з обох боків — а агент його не підтримує без патча.
    redir.attemptWebRTC = true;
    redir.webrtcconfig = { iceServers: [{ urls: ['stun:185.166.216.204:3478'] }] };

    let stopped = false;
    let grabbed = false;
    let connected = false; // state 3 вже відпрацьовано — див. «повтор на P2P» нижче
    let connectTimer = null; // R07: сторож «не вийшли на state 3 за 20с»
    const keyHandlers = {};

    // Налаштування картинки. WebP (type 4) замість дефолтного JPEG: на реальних
    // скрінах ERP-екранів WebP q50 = 42% байтів JPEG q50 (замір 26.08, 4 скріни
    // 1600×950). frameTimer 50мс проти дефолтних 100мс декодера = стеля кадрів
    // 10 → 20 к/с; разом із WebP трафік ≈ як був (0.42×2), а плавність удвічі.
    let imgType = 4;      // 1 = JPEG, 4 = WebP
    let imgQuality = 60;  // рівень стиснення (значення селектора «Якість»)
    let frameTimer = 50;  // мс між кадрами (значення селектора «Швидкість»)
    function applyImageSettings() {
        try { desktop.SendCompressionLevel(imgType, imgQuality, 1024, frameTimer); } catch (e) { reportError('desktop.imgset', e); }
    }
    const useDirect = !!relayDirectBase() && !directMarkedBroken();

    // Телеметрія транспорту. redir.webRtcActive стає true лише коли datachannel
    // реально відкрився І агент підтвердив перемикання (їхній performWebRtcSwitch,
    // agent-redir-ws-0.1.1.js:141) — до того й після провалу ICE це websocket.
    // Другий маркер того самого: redir.latency.current стає -1 (RTT на P2P не
    // рахується). Пишемо в консоль і в журнал ЛИШЕ на зміні, щоб не сміттярити.
    let transportSeen = '';
    const currentTransport = () => (redir.webRtcActive === true ? 'webrtc' : 'websocket');
    function noteTransport() {
        const t = currentTransport();
        try { window.__ooDesktopTransport = t; } catch (e) { /* ignore */ }
        if (t === transportSeen) return;
        transportSeen = t;
        try { console.info('[oo-remote] transport=' + t); } catch (e) { /* ignore */ }
        if (t === 'webrtc') reportError('desktop:webrtc-active', 'P2P увімкнувся — кадри пішли повз реле');
    }
    // Живий зонд для ручної/автоматичної перевірки з консолі сторінки: читає стан
    // ПРЯМО з redir, тому не залежить від того, чи долетів подієвий колбек (у
    // прихованій вкладці таймери тротляться). Знімається в teardown, щоб не
    // тримати посилання на декодер після сесії.
    try {
        window.__ooDesktopProbe = () => ({
            transport: currentTransport(),
            state: desktop.State,
            webRtcActive: redir.webRtcActive === true,
            ice: (redir.webrtc && redir.webrtc.iceConnectionState) || null,
            latency: (redir.latency && redir.latency.current),
        });
    } catch (e) { /* ignore */ }

    // ponytail: НЕ desktop.GrabKeyInput() (вішає document.onkeydown/keyup/keypress —
    // затирає обробники Filament, задокументована пастка проєкту). І НЕ
    // desktop.handleKeyDown/handleKeyUp/handleKeys — їхні власні тіла звіряють
    // бare-глобал `desktop.State` (не obj.State, буквально ім'я змінної зі
    // сторінки-оригіналу), якого в нас немає. Викликаємо xxKeyDown/xxKeyUp/xxKeyPress
    // напряму — вони коректно перевіряють обʼєктний obj.State і саме так поводяться
    // офіційні document.onkeydown-обробники, яких ми відтворюємо на канвасі.
    // 🚨 ЧОМУ ТЕКСТ НЕ ЙШОВ. MeshCentral шле друкований символ через подію
    // KEYPRESS (xxKeyPress -> SendKeyUnicode), а xxKeyDown для друкованих СВІДОМО
    // не робить нічого. keypress — застаріла подія, на <canvas> вона часто взагалі
    // не спрацьовує, тож символи нікуди не йшли (службові клавіші через keydown —
    // йшли). Тому НЕ покладаємось на keypress: друкований символ шлемо Unicode
    // самі на keydown, службові — через рідний xxKeyDown; UP у всіх випадках через
    // xxKeyUp (він сам обирає Unicode чи скан-код).
    function sendKeyDown(e) {
        if (desktop.State !== 3 || e.key === 'Dead') return;
        const printable = typeof e.key === 'string' && e.key.length === 1
            && !e.ctrlKey && !e.altKey && !e.metaKey && desktop.remoteKeyMap === false;
        if (printable) {
            desktop.SendKeyUnicode(desktop.KeyAction.DOWN, e.key.charCodeAt(0));
            if (e.preventDefault) e.preventDefault();
        } else {
            desktop.xxKeyDown(e); // Enter/Backspace/стрілки/скан-коди/комбо
        }
    }

    function attachCanvasKeys() {
        keyHandlers.keydown = (e) => sendKeyDown(e);
        keyHandlers.keyup = (e) => desktop.xxKeyUp(e);
        // Канвас вішаємо на КАНВАС, а не на document (GrabKeyInput затирає Filament —
        // задокументована пастка), тому канвас МУСИТЬ мати фокус. tabindex робить
        // його фокусованим, mousedown повертає фокус на кожен клік у екран.
        if (!canvas.hasAttribute('tabindex')) canvas.tabIndex = 0;
        keyHandlers.focusOnClick = () => { try { canvas.focus(); } catch (e) { /* ignore */ } };
        // D1: GrabMouseInput вішає колесо лише на c.DOMMouseScroll (гілка обрана
        // за /mozilla/i у UA, а він є В УСІХ браузерах, включно з Chrome) — цю подію
        // Chrome не шле НІКОЛИ, тож прокрутка в екрані була мертва. Транслюємо
        // стандартний wheel у формат, який чекає SendMouseMsg (event.wheelDelta),
        // і глушимо прокрутку сторінки під канвою.
        keyHandlers.wheel = (e) => {
            if (desktop.State !== 3) return;
            e.preventDefault();
            desktop.SendMouseMsg(desktop.KeyAction.SCROLL, { pageX: e.pageX, pageY: e.pageY, wheelDelta: -e.deltaY });
        };
        canvas.addEventListener('wheel', keyHandlers.wheel, { passive: false });
        // D3: канва втратила фокус (Alt+Tab, клік у Filament) із затиснутим модифікатором —
        // keyup уже не долетить, і Shift/Ctrl/Alt/Win «залипають» на віддаленому ПК.
        // handleReleaseKeys відпускає САМЕ реально натиснуті клавіші (наш keydown-шлях
        // наповнює obj.pressedKeys через SendKeyMsgKC), точніше за ручний перелік кодів.
        keyHandlers.blur = () => { if (desktop.State === 3 && typeof desktop.handleReleaseKeys === 'function') desktop.handleReleaseKeys(); };
        canvas.addEventListener('blur', keyHandlers.blur);
        canvas.addEventListener('mousedown', keyHandlers.focusOnClick);
        canvas.addEventListener('keydown', keyHandlers.keydown);
        canvas.addEventListener('keyup', keyHandlers.keyup);
        keyHandlers.focusOnClick();
    }

    function detachCanvasKeys() {
        if (keyHandlers.wheel) canvas.removeEventListener('wheel', keyHandlers.wheel);
        if (keyHandlers.blur) canvas.removeEventListener('blur', keyHandlers.blur);
        if (keyHandlers.focusOnClick) canvas.removeEventListener('mousedown', keyHandlers.focusOnClick);
        if (keyHandlers.keydown) canvas.removeEventListener('keydown', keyHandlers.keydown);
        if (keyHandlers.keyup) canvas.removeEventListener('keyup', keyHandlers.keyup);
        keyHandlers.wheel = keyHandlers.blur = keyHandlers.keydown = keyHandlers.keyup = keyHandlers.focusOnClick = null;
    }

    redir.onStateChanged = (sender, state) => {
        // 0 = роз'єднано, 1 = ws-конект, 2 = під'єднано до релею, 3 = наскрізь готово
        if (state === 3 && !grabbed) {
            grabbed = true;
            desktop.GrabMouseInput();
            attachCanvasKeys();
        }
        if (state !== 3 && grabbed) {
            grabbed = false;
            detachCanvasKeys();
        }
        if (state === 3) {
            if (connectTimer) { clearTimeout(connectTimer); connectTimer = null; } // R07: підключились — сторож зайвий
            if (useDirect) markDirectBroken(false); // прямий маршрут живий — запобіжник знято
            notifyState('connected');
            noteTransport();
            // 🚨 ПОВТОР НА P2P — не помилка. Після вдалого перемикання на WebRTC їхній
            // performWebRtcSwitch() сам смикає onStateChanged(obj, obj.State), а State
            // лишається 3 — тобто цей блок заходить ВДРУГЕ. Усе, що має статись рівно
            // раз (запис «відкрито» в журнал грантів і сторож WebP), — під прапорцем,
            // інакше на кожній P2P-сесії був би дубль у remote_access_grants.
            if (connected) return;
            connected = true;
            applyImageSettings(); // WebP + 20 к/с замість дефолтних JPEG50 + 10 к/с
            // Сторож WebP: старий агент може мовчки не вміти type 4 — тоді кадри
            // просто не приходять. Немає ПЕРШОГО кадру за 4с → повертаємось на JPEG.
            setTimeout(() => {
                if (!framed && !stopped && imgType === 4) {
                    imgType = 1;
                    applyImageSettings();
                    reportError('desktop:webp-fallback', 'агент не віддав жодного кадру на WebP — повернувся на JPEG');
                }
            }, 4000);
            reportOpened(g.grant_id).catch(() => { /* best effort, журнал не блокер */ });
        }
        else if (state === 1 || state === 2) { notifyState('connecting'); }
        else {
            // D9: 'disconnected' лише на НЕОЧІКУВАНИЙ обрив. Наш власний stop() ставить
            // stopped=true ДО redir.Stop(), тож перемикання вкладки/ПК не підніме
            // фальшивий банер «звʼязок втрачено».
            if (!stopped) {
                // Обрив ДО першого підключення на прямому маршруті = мережа
                // користувача не пускає на net.… — наступний клік піде через origin.
                if (useDirect && !grabbed) markDirectBroken(true);
                notifyState('disconnected'); teardown('closed'); // сервер/агент розірвав сам
            }
        }
    };

    if (onMessage) desktop.onMessage = (msg) => onMessage(msg);

    // ПЕРШИЙ КАДР. Досі «підключився» і «підключився й бачить» були в наших
    // записах нерозрізненні — саме тому чорний екран у людини не лишав сліду
    // ніде, і про нього дізнавались, тільки коли вона напише. onPreDrawImage
    // кличе декодер перед кожним малюванням; нам потрібен лише перший.
    let framed = false;
    desktop.onPreDrawImage = () => {
        if (framed) return;
        framed = true;
        reportFirstFrame(g.grant_id).catch(() => { /* мітка якості, не блокер */ });
    };

    // Список моніторів агент шле сам, окремим кадром (agent-desktop-0.0.2.js:264):
    // onDisplayinfo(obj, displays, selectedDisplay), де displays — мапа {номер: підпис}.
    // Тобто до підключення ми не знаємо ні скільки їх, ні який активний — саме тому
    // перемикач моніторів наповнюється звідси, а не з даних ЕРП.
    if (onDisplays) {
        desktop.onDisplayinfo = (sender, displays, selected) => {
            try { onDisplays(displays || {}, selected); } catch (e) { /* caller's bug, not ours */ }
        };
    }

    async function teardown(outcome) {
        if (stopped) return;
        stopped = true;
        if (connectTimer) { clearTimeout(connectTimer); connectTimer = null; }
        detachCanvasKeys();
        try { delete window.__ooDesktopProbe; window.__ooDesktopTransport = ''; } catch (e) { /* ignore */ }
        try { redir.Stop(); } catch (e) { /* ignore */ }
        try { desktop.Stop(); } catch (e) { /* ignore */ }
        try { await closeGrant(g.grant_id, outcome); } catch (e) { /* best effort, журнал не блокер */ }
    }

    // Прямий маршрут реле: Start() їхнього agent-redir жорстко будує URL від
    // window.location.host (рядок 60 agent-redir-ws-0.1.1.js) — хост звідти не
    // вийняти параметром. Тому на час СИНХРОННОГО Start() підмінюємо
    // window.WebSocket обгорткою, що переписує лише хост meshrelay-адреси на
    // net.… (сірий DNS, без Cloudflare). Після Start() глобал повертається.
    function startTunnel() {
        if (!useDirect) { redir.Start(nodeId); return; }
        const base = relayDirectBase(); // https://net.organicoils.com.ua/remote
        const NativeWS = window.WebSocket;
        window.WebSocket = function (url, protos) {
            let u = url;
            try {
                const parsed = new URL(url, window.location.href);
                if (parsed.pathname.endsWith('/meshrelay.ashx')) {
                    u = base.replace(/^http/, 'ws') + '/meshrelay.ashx' + parsed.search;
                }
            } catch (e) { /* незрозумілий URL — лишаємо як є */ }
            return protos === undefined ? new NativeWS(u) : new NativeWS(u, protos);
        };
        try { redir.Start(nodeId); } finally { window.WebSocket = NativeWS; }
    }

    try {
        startTunnel();
        // R07: якщо тунель не дійшов до state 3 за 20с — не лишаємо вічне
        // «підключення…». Гасимо й повідомляємо, щоб користувач міг спробувати знову.
        connectTimer = setTimeout(() => {
            if (stopped || grabbed) return;
            if (useDirect) markDirectBroken(true); // наступний клік — через origin
            notifyState('error', 'Не вдалося підключитися до екрана за 20 секунд. Спробуйте ще раз.');
            teardown('timeout');
        }, 20000);
    } catch (e) {
        const msg = String((e && e.message) || e);
        await teardown('failed');
        notifyState('error', msg);
        throw e;
    }

    // Функціонал старого екранного режиму MeshCentral, відновлений поверх того ж
    // decoder-API (agent-desktop-0.0.2.js). Усі виклики — реальні методи об'єкта,
    // жодної «мовчазної кнопки»: клавіша Win/Esc, оновлення кадру, блок локального
    // вводу людини за ПК і рівень якості/стиснення картинки.
    return {
        stop: () => teardown('closed'),
        ctrlAltDel: () => desktop.SendCtrlAltDelMsg(),
        setDisplay: (i) => desktop.SetDisplay(i),
        state: () => desktop.State,
        // 'webrtc' = кадри йдуть P2P повз реле, 'websocket' = штатний фолбек.
        transport: () => currentTransport(),
        winKey: () => { try { desktop.SendStartMsg(); } catch (e) { reportError('desktop.winkey', e); } },
        escKey: () => { try { desktop.SendEscKey(); } catch (e) { reportError('desktop.esc', e); } },
        refresh: () => { try { desktop.SendRefresh(); } catch (e) { reportError('desktop.refresh', e); } },
        // code: 1 = заблокувати клавіатуру/мишу людини за ПК, 0 = розблокувати.
        setInputLock: (on) => { try { desktop.SendRemoteInputLock(on ? 1 : 0); } catch (e) { reportError('desktop.inputlock', e); } },
        // Рівно два регулятори картинки (рішення власника 26.08):
        // Якість = рівень стиснення (тип лишається наш поточний — WebP, або JPEG
        // після сторожа-фолбека); Швидкість = FrameRateTimer агента в мс.
        setQuality: (level) => { const v = parseInt(level, 10); if (v > 0) { imgQuality = v; applyImageSettings(); } },
        setSpeed: (ms) => { const v = parseInt(ms, 10); if (v > 0) { frameTimer = v; applyImageSettings(); } },
    };
}
