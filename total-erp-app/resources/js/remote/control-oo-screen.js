// resources/js/remote/control-oo-screen.js
// OO-екран (WebRTC): перемикач «Mesh / OO / Авто», звук, якість, монітори.
// Винесено з control.js дослівно (хвиля 2 аудиту віддаленого доступу, 24.09).
import { ctl } from './control-state.js';
import { hideBanner, showBanner } from './control-banner.js';
import { ooLayerLive } from './control-desktop-reconnect.js';
import { devices } from './control-devices-store.js';
import { abortAfter, fetchJSON, ooCsrf, reportError } from './control-http.js';
import { clearDisplays, renderOoDisplays } from './control.js';

let ooLayerGen = 0;          // покоління шару: колбек мертвого шару не чіпає живий

// ───────────── OO-екран: перемикач «Mesh / OO / Авто» ───────────────────────
//
// Композиція, а не заміна: Mesh-canvas ЛИШАЄТЬСЯ input-поверхнею (весь ввід
// його), OO кладе поверх <video> з pointer-events:none. Тому desktop.js тут
// не чіпається взагалі — ми лише домальовуємо шар над уже відкритим модулем.

const OO_MODE_STORE_PREFIX = 'oo-screen-mode:';

/**
 * Конфіг приїжджає ЛИШЕ з сервера (data-oo-screen).
 * ЖОДНОГО токена (BLOCKER-1): у DOM його немає. Замість token — ticketEndpoint,
 * з якого на кожну OO-сесію тягнеться свіжий одноразовий ticket (§6.4).
 */
function readOoScreenConfig(el) {
    const raw = el && el.dataset ? el.dataset.ooScreen : null;
    if (!raw) return null;
    let cfg;
    try { cfg = JSON.parse(raw); } catch (e) { reportError('oo-screen-config', e); return null; }
    if (!cfg || !cfg.enabled || !cfg.ticketEndpoint) return null;
    return {
        signalUrl: String(cfg.signalUrl || ''),
        ticketEndpoint: String(cfg.ticketEndpoint),
        // Адреси журналу НЕОБОВ’ЯЗКОВІ: на проді блейд їх ще не віддає, і це не
        // поломка — без адреси просто нічого не шлемо. Тому саме тернарник, а не
        // String(cfg.x || ''): порожній рядок ще падав би у гілку «є адреса»,
        // а String(undefined) взагалі дав би адресу "undefined".
        qualityEndpoint: cfg.qualityEndpoint ? String(cfg.qualityEndpoint) : null,
        sessionEventEndpoint: cfg.sessionEventEndpoint ? String(cfg.sessionEventEndpoint) : null,
        // controlUrl — /control на хабі (список моніторів + «перемкни на N»).
        // Теж НЕОБОВ’ЯЗКОВИЙ і з тієї ж причини: сторінка, залита до появи цього
        // ключа, мусить працювати як раніше — просто без перемикача монітора.
        controlUrl: cfg.controlUrl ? String(cfg.controlUrl) : null,
        // readinessEndpoint — «хто з ПК зараз готовий віддавати власний потік».
        // НЕОБОВ’ЯЗКОВИЙ з тієї ж причини, що й попередні: сторінка, залита до
        // появи ключа, працює як раніше — просто нічого не гасить наперед.
        readinessEndpoint: cfg.readinessEndpoint ? String(cfg.readinessEndpoint) : null,
        // audio — чи сервер передачі екрана взагалі віддає звук. Ключ
        // НЕОБОВ’ЯЗКОВИЙ з тієї ж причини, що й адреси вище: сторінка, залита
        // до його появи, працює як раніше — просто регулятор звуку погашений
        // і сам пояснює, чому. Тому саме !!cfg.audio, а не String(...).
        audio: !!cfg.audio,
        // input — чи вести клавіатуру й мишу ВЛАСНИМ каналом замість
        // MeshCentral. Ключ НЕОБОВ’ЯЗКОВИЙ і типово вимкнений з тієї ж
        // причини, що й audio, тільки ціна помилки вища: це канал повного
        // керування чужим ПК. Немає ключа -> ввід їде Mesh-ом рівно як їхав.
        // Вмикається ТРЬОМА узгодженими прапорцями: цей ключ у блейді плюс
        // OO_SCREEN_INPUT=1 на хабі й на агенті.
        input: !!cfg.input,
        mode: String(cfg.mode || 'mesh'),
    };
}

/**
 * ooControlUrl — адреса /control на хабі.
 *
 * Явний controlUrl із блейда головніший. Без нього виводимо із signalUrl, який
 * уже є: обидва — той самий хаб, і другий ключ у конфігу лише множив би шанс, що
 * вони роз’їдуться. Не схоже на адресу сигналінгу — повертаємо null, і перемикач
 * просто не з’явиться (краще без органа керування, ніж із тим, що б’є в нікуди).
 */
export function ooControlUrl(cfg) {
    if (!cfg) return null;
    if (cfg.controlUrl) return String(cfg.controlUrl);
    const signal = String(cfg.signalUrl || '');
    if (!/\/offer\/viewer\/?$/.test(signal)) return null;
    return signal.replace(/\/offer\/viewer\/?$/, '/control');
}

/**
 * ooDisplayOptions — монітори з хаба -> опції перемикача.
 *
 * Один монітор (або жодного) = порожньо, як і в Mesh-гілці: орган керування, що
 * нічого не перемикає, лише плутає. Підпис іде від ОДИНИЦІ («Монітор 1»), бо
 * індекси DXGI нульові, а людина рахує монітори з першого; сам value лишається
 * індексом, який розуміє агент.
 */
export function ooDisplayOptions(outputs) {
    const list = Array.isArray(outputs) ? outputs.filter((o) => o && Number.isInteger(o.index)) : [];
    if (list.length < 2) return [];
    return list.map((o) => {
        const size = (o.width && o.height) ? ` ${o.width}×${o.height}` : '';
        return {
            value: String(o.index),
            label: `Монітор ${o.index + 1}${size}${o.primary ? ' (основний)' : ''}`,
        };
    });
}

/**
 * fetchOoTicket — свіжий одноразовий ticket §6.4 на КОНКРЕТНУ ноду.
 * node_id несе саме ту машину (BLOCKER-3): hub зв’яже глядача з publisher-ом
 * цієї ноди по node_id у claims. Auth-сесія + CSRF — гейт як у консолі.
 * Повертає { ticket, signalUrl } (signalUrl із відповіді, або з data-атрибута).
 */
async function fetchOoTicket(nodeId, fallbackSignalUrl, isNewSession) {
    if (!ctl.ooScreenConfig || !ctl.ooScreenConfig.ticketEndpoint) throw new Error('oo-screen: немає ticketEndpoint');
    const res = await fetchJSON(ctl.ooScreenConfig.ticketEndpoint, {
        method: 'POST',
        // 🔴 Квиток для /control не сміє відкривати сеанс. Сервер
        // (ScreenEngineController::ticket) читає відсутній `purpose` як
        // 'session', іде в openSession() і МОВЧКИ закриває поточну живу
        // сесію; нова кадру вже не отримає. Замір 05.09.2026: 40 зі 130
        // «сесій без кадру» за тиждень — саме ці примари.
        // Форма взята з гілки task/prod-remote-audit (паралельна сесія
        // полагодила це першою й охайніше — без зайвого параметра).
        body: JSON.stringify({ node_id: nodeId, purpose: isNewSession ? 'session' : 'control' }),
    });
    const ticket = res && res.ticket ? String(res.ticket) : '';
    if (!ticket) throw new Error('oo-screen: порожній ticket');
    const signalUrl = String((res && res.signal_url) || fallbackSignalUrl || ctl.ooScreenConfig.signalUrl || '');
    if (!signalUrl) throw new Error('oo-screen: немає signalUrl');
    // Сервер заводить сесію разом із квитком і повертає її correlation_id.
    // Без нього метрики якості лягали б у БД без прив’язки до сесії —
    // рядки є, а до чого вони, невідомо.
    // F1A-10: ooControlCall бере квиток і на ПЕРЕМИКАННЯ монітора всередині
    // тієї самої сесії, а не лише на її старт. Раніше це затирало
    // ooQualityCorrelation щоразу — накопичений буфер летів під новий id
    // (або взагалі губився). Міняємо correlation ЛИШЕ для нової сесії; перед
    // тим зливаємо те, що встигло назбиратись під старим id.
    if (isNewSession) {
        flushOoQuality();
        // Без цього перший же семпл у queueOoQualitySample бачив «чужу» ноду
        // і стирав correlation — метрики й beacon закриття лягали без сесії.
        ooQualityNode = nodeId;
        ooQualityCorrelation = (res && res.correlation_id) ? String(res.correlation_id) : null;
    } else if (!ooQualityCorrelation && res && res.correlation_id) {
        ooQualityCorrelation = String(res.correlation_id);
    }
    return { ticket, signalUrl };
}

// F-16: стеля на один запит до хаба. 10с — свідомо менше за 15с fetchJSON:
// /control має відповісти миттєво (агент поруч із хабом), а людина тримає
// натиснутим перемикач і чекає.
const OO_CONTROL_TIMEOUT_MS = 10000;


/**
 * C1: селекти тулбара -> стелі OO-потоку для hub /control.
 *
 * «Якість» — рівень 1..5 за контрактом {1:1.5M, 2:3M, 3:5M, 4:8M, 5:null (стеля
 * агента)}. Селект у блейді несе відсоток JPEG для Mesh (40/60/80); рішення
 * оркестратора 24.09: 40 -> 3 Мбіт/с, 60 -> 5 Мбіт/с, 80 («висока», типова) ->
 * рівень 5, тобто без стелі. Інакше типова «висока» різала б OO до 8 Мбіт/с,
 * а зміна самої «Швидкості» тягнула б цю стелю за собою.
 * «Швидкість» — мс між кадрами -> fps = round(1000/ms), кламп 1..60.
 * Нечитабельне значення -> null (хаб лишає свою стелю).
 */
const OO_BITRATE_BY_LEVEL = { 1: 1500000, 2: 3000000, 3: 5000000, 4: 8000000, 5: null };
export function ooToolbarLimits(quality, speedMs) {
    const q = parseInt(quality, 10);
    const ms = parseInt(speedMs, 10);
    let level = null;
    if (q >= 1 && q <= 5) level = q;
    else if (q > 5) level = q >= 80 ? 5 : Math.max(1, Math.round(q / 20));
    return {
        max_bitrate_bps: level ? OO_BITRATE_BY_LEVEL[level] : null,
        max_fps: ms > 0 ? Math.min(60, Math.max(1, Math.round(1000 / ms))) : null,
    };
}

/** Людина змінила селект відносно розмітки (option з атрибутом selected — типовий). */
function ooSelectChanged(sel) {
    const opt = sel && sel.options ? sel.options[sel.selectedIndex] : null;
    return !!opt && !opt.defaultSelected;
}

/**
 * C1: живий OO-шар малює сам — Mesh під ним на паузі, тож стелі йдуть у хаб.
 * onlyIfChanged — виклик на 'live' (перший кадр, зокрема після перепідключення):
 * хаб тримає стелю лише до відключення глядача, нова нога стоїть на своїй.
 * Тож шлемо, якщо людина відійшла від типових значень, — у тому числі коли
 * обрала їх ще в Mesh-режимі чи до першого кадру (тоді ooLayerLive() був false).
 */
function applyOoToolbarLimits(onlyIfChanged) {
    if (!ooLayerLive() || !ctl.activeNodeId) return;
    const q = document.querySelector('[data-action="quality"]');
    const sp = document.querySelector('[data-action="speed"]');
    if (onlyIfChanged && !ooSelectChanged(q) && !ooSelectChanged(sp)) return;
    const limits = ooToolbarLimits(q && q.value, sp && sp.value);
    // Типова «Швидкість» (50 мс = 20 fps — ритм Mesh) для OO — не вибір людини.
    // Інакше зміна самої «Якості» ставила б стелю 20 fps, якої ніхто не обирав:
    // хаб без стелі лишає агента на його власному -fps. null — зняти стелю, якщо
    // людина раніше обрала іншу швидкість і повернулась до типової.
    if (!ooSelectChanged(sp)) limits.max_fps = null;
    ooControlCall(ctl.activeNodeId, null, limits).catch((e) => {
        reportError('oo-limits', e);
        showBanner(e.human || 'Не вдалося змінити якість OO-екрана.');
    });
}

/**
 * ooControlCall — один запит на hub /control: {ticket, output?} -> {outputs, active}.
 *
 * 🔑 АВТОРИЗАЦІЯ: свіжий ОДНОРАЗОВИЙ квиток §6.4 на цю ноду — рівно той самий
 * механізм, що й /offer/viewer, і видає його той самий ERP-ендпоінт із тим самим
 * гейтом доступу до ПК. Ніякого «постійного» ключа перемикача не існує: хто не
 * має права дивитись цей ПК, квитка не отримає, а вжитий квиток не спрацює вдруге.
 *
 * output === null -> лише читання списку (наповнити перемикач), інакше — команда.
 */
async function ooControlCall(nodeId, output, extra) {
    const url = ooControlUrl(ctl.ooScreenConfig);
    if (!url) throw new Error('oo-screen: немає адреси /control');
    const { ticket } = await fetchOoTicket(nodeId, ctl.ooScreenConfig.signalUrl);
    const body = { ticket };
    if (output !== null && output !== undefined) body.output = Number(output);
    // C1: стелі тулбара (max_bitrate_bps / max_fps) — той самий квиток і ендпоінт.
    if (extra) Object.assign(body, extra);
    // Голий fetch, а не fetchJSON: хаб — чужий origin, куки й CSRF ERP туди не йдуть
    // і не потрібні (авторизує квиток), той самий шлях, що й offer у шарі OO.
    // F-16: без таймауту мертвий хаб лишав цей промис невирішеним НАЗАВЖДИ, а
    // разом із ним — sel.disabled=true у onDisplayChange (його .finally() ніколи
    // не наставав). Перемикач моніторів залипав до перезавантаження сторінки.
    const to = abortAfter(OO_CONTROL_TIMEOUT_MS);
    let res;
    try {
        res = await fetch(url, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(body),
            signal: to.signal,
        });
    } catch (e) {
        if (e && (e.name === 'TimeoutError' || e.name === 'AbortError')) {
            const te = new Error('/control -> timeout');
            te.human = 'Хаб не відповів за 10 секунд. Спробуйте ще раз.';
            throw te;
        }
        throw e;
    } finally {
        to.cancel();
    }
    if (!res.ok) throw new Error('/control ' + res.status);
    return res.json();
}

/** Наповнити перемикач моніторами з хаба. Тихо нічого не робить, поки їх < 2. */
async function loadOoDisplays(nodeId) {
    if (!ctl.ooScreenConfig || !ooControlUrl(ctl.ooScreenConfig)) return;
    let data;
    try {
        data = await ooControlCall(nodeId, null);
    } catch (e) {
        reportError('oo-displays', e);
        return;
    }
    if (ctl.ooDisplaysNode !== nodeId) return; // поки ходили — перемкнули ПК/вкладку
    renderOoDisplays(data && data.outputs, data && data.active);
}

// Вибір режиму — ПО ВУЗЛУ: власник порівнює конкретний ПК, і однакова
// глобальна «липка» кнопка перенесла б висновок з однієї машини на іншу.
function ooStoredMode(nodeId) {
    try { return sessionStorage.getItem(OO_MODE_STORE_PREFIX + nodeId); } catch (e) { return null; }
}
function ooStoreMode(nodeId, mode) {
    try { sessionStorage.setItem(OO_MODE_STORE_PREFIX + nodeId, mode); } catch (e) { /* приватний режим — не блокер */ }
}

/** Журнал консолі: у браузер — завжди, на сервер — лише смерть OO (щоб не спамити). */
function ooLog(message) {
    if (typeof console !== 'undefined') console.info('[remote-access] oo-screen:', message);
}

// Готовність вузлів до ВЛАСНОЇ передачі екрана: що сказав хаб через ERP.
//
// ooReadyKnown — чи хаб узагалі відповів. Поки false, стан КОЖНОГО вузла —
// «невідомо», і це НЕ «не готовий»: гасити кнопку через власний зламаний
// опитувач гірше, ніж пустити клік, який може не вдатись.
let ooReadyKnown = false;
const ooReadySet = new Set();   // вузли з живим публікатором (лише коли known)

/** true / false / null («невідомо»). */
export function ooReadyState(nodeId) {
    if (!ooReadyKnown || !nodeId) return null;
    return ooReadySet.has(nodeId);
}

/**
 * Питає ERP, хто зараз на зв’язку, і перемальовує перемикач.
 *
 * Будь-яка невдача (мережа, 429, хаб мовчить -> known:false) повертає стан у
 * «невідомо», а не в «не готовий»: застаріле false тримало б кнопку погашеною
 * рівно тоді, коли перевірити нічим.
 */
// Причина, яку назвав сам агент (хаб /nodes unavailable), напр. 'session-locked'.
const ooUnavailableWhy = new Map();

async function refreshOoReady() {
    const url = ctl.ooScreenConfig && ctl.ooScreenConfig.readinessEndpoint;
    if (!url) return;
    let data = null;
    try { data = await fetchJSON(url); } catch (e) { /* невідомо — не помилка сторінки */ }
    ooReadySet.clear();
    ooUnavailableWhy.clear();
    ooReadyKnown = !!(data && data.known);
    if (ooReadyKnown && Array.isArray(data.ready)) data.ready.forEach((n) => ooReadySet.add(String(n)));
    if (ooReadyKnown && data.unavailable && typeof data.unavailable === 'object') {
        Object.entries(data.unavailable).forEach(([n, why]) => ooUnavailableWhy.set(String(n), String(why)));
    }
    try { if (ctl.ooModeCtl) renderOoSegment(ctl.ooModeCtl.mode()); } catch (e) { /* сегмент уже знято */ }
}

// Чи може цей ПК ЗАРАЗ віддавати OO-потік — і якщо ні, то чому саме.
//
// Причин дві, і людині вони кажуть різне.
//
// 1. XP/Vista — назавжди: агента для них нема. Windows 7 з 06.10.2026
//    ПРАЦЮЄ (агент збирається Win7-сумісним Go і захоплює екран через GDI, бо
//    DXGI Desktop Duplication там нема), тож її більше не гасимо: чи готовий
//    конкретний ПК — каже хаб через ready, як і для решти.
// 2. Агент на ПК зараз не на зв’язку з хабом — тимчасово, і саме це трапляється
//    значно частіше. Стан приходить ззовні (ready), бо знає його лише хаб.
//
// Показувати живий перемикач там, де OO неможливий, — обіцянка, якої консоль не
// може дотримати: людина тисне OO, з’єднуватись нема з ким, і вона отримує банер
// «OO втрачено» замість пояснення. Тому гасимо НАПЕРЕД і кажемо причину.
//
// ready: true (готовий) | false (хаб ЯВНО сказав «нема») | null/undefined
// (невідомо). Гасимо лише на явному false — «невідомо» ≠ «не готовий».
export function ooUnsupportedReason(dev, ready, why) {
    if (dev?.access_assignment_only || dev?.screen_engine === 'mesh') {
        return 'Персональний доступ працює через Mesh, щоб сервер перевіряв дозволені канали та строк дії.';
    }
    const os = (dev && dev.os) ? String(dev.os) : '';
    // ОС невідома — не вгадуємо (але про зв’язок нижче можемо знати напевно).
    if (os && /windows\s*(xp|vista)/i.test(os)) {
        return 'Windows XP/Vista: програми передачі екрана для цієї ОС нема. '
             + 'Цей ПК працює через Mesh — так і задумано.';
    }
    // why — причина від самого агента (17.09.2026). Заблокований ПК — найчастіший
    // випадок: екран входу бачить лише Mesh, і людині варто знати саме це.
    if (why === 'session-locked') {
        return 'ПК заблоковано: екран входу показує лише Mesh. '
             + 'Щойно людина розблокує ПК, кнопка ввімкнеться сама.';
    }
    if (ready === false) {
        return 'Програма передачі екрана на цьому ПК зараз не відповідає, тож показуємо через Mesh. '
             + 'Коли ПК буде на зв’язку, кнопка ввімкнеться сама.';
    }
    return null;
}

function renderOoSegment(mode) {
    const seg = document.getElementById('oo-remote-oo-mode');
    if (!seg) return;
    const why = ooUnsupportedReason(
        ctl.activeNodeId ? devices.get(ctl.activeNodeId) : null,
        ooReadyState(ctl.activeNodeId),
        ctl.activeNodeId ? ooUnavailableWhy.get(ctl.activeNodeId) : undefined,
    );
    seg.querySelectorAll('[data-oo-mode]').forEach((b) => {
        b.setAttribute('aria-selected', b.dataset.ooMode === mode ? 'true' : 'false');
        const blocked = !!why && b.dataset.ooMode !== 'mesh';
        b.disabled = blocked;
        b.setAttribute('aria-disabled', blocked ? 'true' : 'false');
        if (blocked) {
            if (!b.dataset.ooTitleWas) b.dataset.ooTitleWas = b.getAttribute('title') || '';
            b.setAttribute('title', why);
            b.style.opacity = '.45';
            b.style.cursor = 'not-allowed';
        } else if (b.dataset.ooTitleWas !== undefined) {
            if (b.dataset.ooTitleWas) b.setAttribute('title', b.dataset.ooTitleWas);
            b.style.opacity = '';
            b.style.cursor = '';
        }
    });
    // Звук залежить від того ж стану, що й транспорт (Mesh -> звучати нема чому).
    // Перемальовуємо тим самим тактом, інакше кнопки на одній смузі розійдуться.
    renderOoSound();
}

// ─────────────────────────────────────────────────────────────────────────────
// Звук чужого ПК: регулятор у смузі консолі
//
// Типово БЕЗЗВУЧНО, і це рішення, а не значення за замовчуванням: людина
// відкриває консоль, щоб подивитись, і не має несподівано почути чужий кабінет
// (а поруч із нею — своїх колег). Звук вмикається одним кліком і тим самим
// кліком вимикається.
//
// Пам’ять — по ВУЗЛУ і на сесію вкладки, рівно як ooStoredMode: вибір «слухаю
// цей ПК» стосується конкретної машини, і переносити його на наступну було б
// тією ж помилкою, що й глобальна липка кнопка транспорту.
// ─────────────────────────────────────────────────────────────────────────────

const OO_SOUND_STORE_PREFIX = 'oo-screen-sound:';

let ooSoundOn = false;      // бажання людини (типово — тиша)
let ooSoundNode = null;     // чий вибір зараз у пам’яті

function ooStoredSound(nodeId) {
    try { return sessionStorage.getItem(OO_SOUND_STORE_PREFIX + nodeId) === 'on'; } catch (e) { return false; }
}
function ooStoreSound(nodeId, on) {
    try { sessionStorage.setItem(OO_SOUND_STORE_PREFIX + nodeId, on ? 'on' : 'off'); } catch (e) { /* приватний режим — не блокер */ }
}

/**
 * ooSoundReason — чому звуку зараз НЕМА, людською мовою (або null, якщо все гаразд).
 *
 * Причин дві, і людині вони кажуть різне:
 *   1. сервер передачі екрана звук не віддає взагалі — це надовго і не лікується
 *      кліком у консолі;
 *   2. зображення зараз дає Mesh, а не власний потік ПК — лікується перемиканням
 *      транспорту тут же, поруч.
 *
 * Живий регулятор там, де звучати нема чому, — обіцянка, якої консоль не може
 * дотримати: людина тисне, нічого не чути, і винною виглядає гучність системи.
 * Тому гасимо НАПЕРЕД і кажемо причину — той самий підхід, що в ooUnsupportedReason.
 *
 * @param {object|null} cfg   конфіг OO-екрана (readOoScreenConfig)
 * @param {boolean} live      чи справді йде власний потік ПК зараз
 */
export function ooSoundReason(cfg, live, audioOnly) {
    if (!cfg || !cfg.audio) {
        return 'Цей ПК передає лише зображення — звук вимкнено на сервері передачі екрана.';
    }
    // audioOnly (16.09.2026): звук можна взяти окремим OO-зʼєднанням і під
    // картинкою Mesh — тож Mesh більше не причина тиші. Причина лише одна:
    // програма передачі екрана на цьому ПК недоступна.
    if (!live && !audioOnly) {
        return 'Звук дає програма передачі екрана на ПК, а вона зараз недоступна (ПК не на зв’язку).';
    }
    return null;
}

/**
 * ooSoundButtonState — увесь вигляд кнопки однією чистою функцією.
 *
 * Погашений вигляд ТОЧНО той самий, що в renderOoSegment (disabled +
 * opacity .45 + cursor not-allowed + причина в підказці): два різні способи
 * сказати «сюди не можна» на одній смузі читались би як два різні стани.
 *
 * Підпис несе стан СЛОВАМИ, а не лише іконкою: наводити мишу, щоб дізнатись, чи
 * тебе зараз чути, — не варіант.
 *
 * Чиста вона тому, що саме тут і живе вся логіка, яку варто пиняти гейтом;
 * renderOoSound нижче — лише перенесення цього в DOM.
 *
 * @param {object|null} cfg     конфіг OO-екрана
 * @param {boolean} live        чи йде власний потік ПК
 * @param {boolean} wanted      чи людина просила звук
 */
export function ooSoundButtonState(cfg, live, wanted, audioOnly) {
    const why = ooSoundReason(cfg, live, audioOnly);
    const on = !!wanted && !why;
    return {
        on,
        disabled: !!why,
        label: on ? '🔊 Звук увімкнено' : '🔇 Звук вимкнено',
        title: why || (on ? 'Вимкнути звук цього ПК' : 'Увімкнути звук цього ПК'),
        opacity: why ? '.45' : '',
        cursor: why ? 'not-allowed' : '',
    };
}

/** renderOoSound — перенести ooSoundButtonState на живу кнопку. Логіки тут нема. */
function renderOoSound() {
    const btn = document.getElementById('oo-remote-sound');
    if (!btn) return;
    const v = ooSoundButtonState(ctl.ooScreenConfig, ooLayerLive(), ooSoundOn, ooCanAudioOnly());
    btn.setAttribute('aria-pressed', v.on ? 'true' : 'false');
    btn.textContent = v.label;
    btn.disabled = v.disabled;
    btn.setAttribute('aria-disabled', v.disabled ? 'true' : 'false');
    btn.setAttribute('title', v.title);
    btn.style.opacity = v.opacity;
    btn.style.cursor = v.cursor;
}

/** Донести бажання людини до живого шару. Шар віддає ФАКТИЧНИЙ стан — на ньому й малюємо. */
function applyOoSound() {
    if (!ctl.ooLayer || typeof ctl.ooLayer.setMuted !== 'function') return;
    let res;
    try { res = ctl.ooLayer.setMuted(!ooSoundOn); } catch (e) { reportError('oo-sound', e); return; }
    if (!res || typeof res.then !== 'function') return;
    const myGen = ooLayerGen;
    res.then((muted) => {
        if (myGen !== ooLayerGen) return;   // шар уже інший
        // Браузер не дав звук — не малюємо «увімкнено» над тишею.
        if (muted && ooSoundOn) {
            ooSoundOn = false;
            if (ooSoundNode) ooStoreSound(ooSoundNode, false);
            renderOoSound();
        }
    }, (e) => reportError('oo-sound', e));
}

function onOoSoundClick() {
    // Страховка: disabled на кнопці можна обійти (розширення, клавіатура з
    // чужого фокуса), а обіцяти звук там, де його нема, не можна ніде.
    if (ooSoundReason(ctl.ooScreenConfig, ooLayerLive(), ooCanAudioOnly())) return;
    ooSoundOn = !ooSoundOn;
    if (ooSoundNode) ooStoreSound(ooSoundNode, ooSoundOn);
    syncOoSound();
    renderOoSound();
}

// ── Звук під картинкою Mesh (16.09.2026) ────────────────────────────────────
// Картинку дає OO — звук іде з того ж шару (applyOoSound). Картинку дає Mesh —
// звук бере окреме OO-зʼєднання без відео (createOoAudioLayer). Одночасно
// двох джерел звуку не буває: syncOoSound — єдине місце, що між ними обирає.
let ooModRef = null;        // модуль desktop-oo-webrtc.js, коли вже завантажений
let ooAudioLayer = null;
let ooAudioGen = 0;

function ooCanAudioOnly() {
    if (!ctl.ooScreenConfig || !ctl.ooScreenConfig.audio || !ooSoundNode || !ooModRef) return false;
    return !ooUnsupportedReason(devices.get(ooSoundNode), ooReadyState(ooSoundNode), ooUnavailableWhy.get(ooSoundNode));
}

function stopOoAudioOnly() {
    ooAudioGen += 1;
    if (ooAudioLayer) { try { ooAudioLayer.destroy('stop'); } catch (e) { /* ignore */ } ooAudioLayer = null; }
}

function syncOoSound() {
    if (!ooSoundOn || !ooSoundNode) { stopOoAudioOnly(); applyOoSound(); return; }
    if (ooLayerLive()) { stopOoAudioOnly(); applyOoSound(); return; }
    if (ooAudioLayer || !ooCanAudioOnly()) return;
    const nodeId = ooSoundNode;
    const myGen = ++ooAudioGen;
    const onFail = (reason) => {
        if (myGen !== ooAudioGen) return;
        ooAudioLayer = null;
        ooSoundOn = false;
        ooStoreSound(nodeId, false);
        renderOoSound();
        if (reason) reportError('oo-audio-only', new Error(String(reason)));
    };
    ooAudioLayer = ooModRef.createOoAudioLayer({
        config: {
            ...ctl.ooScreenConfig,
            // Нова сесія — лише коли відео OO нема зовсім. Поки шар OO живий або
            // ще підключається, квиток 'session' закрив би ЙОГО сесію на сервері.
            requestTicket: () => fetchOoTicket(nodeId, ctl.ooScreenConfig.signalUrl, !ctl.ooLayer),
        },
        onClosed: onFail,
    });
    ooAudioLayer.ready.then((ok) => { if (!ok) { stopOoAudioOnly(); onFail(null); } });
}

// Пороги «погано» — ті самі, що в індикаторі і в агентському моніторі.
const OO_Q_MIN_FPS = 8;
const OO_Q_MAX_RTT = 600;
const OO_Q_MAX_LOSS = 0.15;
// Сервер приймає пачку до 120; шлемо по 12 — це рівно хвилина при кроці 5с,
// тобто один запит на хвилину замість дванадцяти.
const OO_Q_BATCH = 12;

let ooQualityBuf = [];
let ooQualityNode = null;
let ooQualityCorrelation = null;   // id сесії з відповіді на квиток

/**
 * Q-01: «мало кадрів» ≠ «погано».
 *
 * Замір прода 05.09.2026: 85% семплів приїжджали з bad=true — при RTT 19 мс і
 * втратах 0.01%. Це не деградація, це людина, що читає документ: агент кодує
 * лише зміни, тож на нерухомому екрані fps чесно падає до 2-5. Метрика через це
 * брехала, і за нею неможливо було відрізнити справжню проблему від спокою.
 *
 * Монітор (evaluateQualitySample) тепер сам розрізняє idle (нові кадри є,
 * транспорт здоровий) і stalled (ні кадру, ні байта) — беремо його вердикт,
 * коли він є. Порогова гілка лишається для старих/чужих викликів без прапорців.
 */
function ooQualityIsBad(m) {
    if (m.stalled) return true;
    if (typeof m.bad === 'boolean') return m.bad;
    if (m.idle) {
        // Нерухомий екран: fps не карає, транспорт — карає.
        return (m.rttMs != null && m.rttMs > OO_Q_MAX_RTT) || (m.lossPct > OO_Q_MAX_LOSS);
    }
    return (m.fps != null && m.fps < OO_Q_MIN_FPS)
        || (m.rttMs != null && m.rttMs > OO_Q_MAX_RTT)
        || (m.lossPct > OO_Q_MAX_LOSS);
}

/**
 * Один семпл пачки /remote-access/screen/quality (C4). Чиста — гейт тримає мапу
 * полів шару -> колонки screen_quality_samples; чого шар не дав — null.
 */
export function ooQualitySample(metrics, now) {
    const n = (v) => (v != null && isFinite(v) ? v : null);
    // Нові колонки — цілі без знаку (валідація integer|min:0): дріб шару
    // (jitter 3.7 мс) інакше валив би всю пачку 422.
    const u = (v) => (n(v) == null ? null : Math.max(0, Math.round(v)));
    return {
        fps: n(metrics.fps),
        rtt_ms: n(metrics.rttMs),
        // Сервер зберігає ЧАСТКУ 0..1, як її рахує монітор, а не відсотки.
        loss_pct: metrics.lossPct,
        bad: ooQualityIsBad(metrics),
        bitrate_kbps: u(metrics.bitrateKbps),
        jitter_ms: u(metrics.jitterMs),
        width: u(metrics.width),
        height: u(metrics.height),
        freeze_count: u(metrics.freezes),
        is_static: typeof metrics.idle === 'boolean' ? metrics.idle : null,
        sampled_at: now,
    };
}

/** Зливає накопичені семпли на сервер. Мовчазний: метрики не мають права
 *  зіпсувати живу сесію, тому будь-яка помилка тут — просто втрачена пачка. */
function flushOoQuality() {
    const url = ctl.ooScreenConfig && ctl.ooScreenConfig.qualityEndpoint;
    const node = ooQualityNode;
    const samples = ooQualityBuf;
    ooQualityBuf = [];
    if (!url || !node || !samples.length) return;
    const body = { node_id: node, samples };
    if (ooQualityCorrelation) body.correlation_id = ooQualityCorrelation;
    fetchJSON(url, {
        method: 'POST',
        body: JSON.stringify(body),
    }).catch(() => { /* пачку втрачено — і нехай, сесія важливіша */ });
}

/** Кладе один семпл у буфер; шле пачкою, коли назбиралось. */
function queueOoQualitySample(nodeId, metrics) {
    if (!metrics) return;
    if (ooQualityNode !== nodeId) { ooQualityBuf = []; ooQualityNode = nodeId; ooQualityCorrelation = null; }
    ooQualityBuf.push(ooQualitySample(metrics, Date.now()));
    if (ooQualityBuf.length >= OO_Q_BATCH) flushOoQuality();
}

/**
 * §8: подія стану сесії на сервер (перший кадр / фолбек). Знає це саме браузер,
 * серверного шляху для таких переходів немає.
 *
 * Мовчазна, як і flushOoQuality: немає адреси (прод ще без маршруту) або немає
 * correlation_id — просто нічого не шлемо; помилка запиту — втрачений рядок
 * журналу. Журнал не має права зіпсувати живу сесію.
 */
function sendOoSessionEvent(nodeId, event, reason) {
    const url = ctl.ooScreenConfig && ctl.ooScreenConfig.sessionEventEndpoint;
    if (!url || !nodeId || !ooQualityCorrelation) return;
    fetchJSON(url, {
        method: 'POST',
        body: JSON.stringify({
            node_id: nodeId,
            correlation_id: ooQualityCorrelation,
            event,
            reason: reason || null,
        }),
    }).catch(() => { /* рядок журналу втрачено — і нехай, сесія важливіша */ });
}

/**
 * Покинута вкладка: hub про закриття ERP не звітує, тому сесія висіла б
 * «відкритою», доки та сама людина не візьме новий квиток або адмін не обірве.
 *
 * Саме pagehide, а не visibilitychange: pagehide спрацьовує на закритті вкладки,
 * переході й перезавантаженні і НЕ спрацьовує на перемиканні вкладок, тоді як
 * visibilitychange закривав би живу сесію на кожен alt-tab — а після цього
 * модель відкидала б справжні переходи як недозволені з closed. (На мобільних
 * надійніший був би саме visibilitychange, бо там вкладку вбиває ОС і pagehide
 * може не встигнути, але консоль віддаленого доступу — десктопна.)
 *
 * fetch на вивантаженні сторінки браузер обриває, тому sendBeacon; немає його
 * або він відмовив — тихо пропускаємо. CSRF окремим полем _token: заголовків
 * beacon не носить, а Laravel читає _token і з JSON-тіла.
 */
function beaconOoSessionClosed() {
    const url = ctl.ooScreenConfig && ctl.ooScreenConfig.sessionEventEndpoint;
    if (!url || !ooQualityNode || !ooQualityCorrelation) return;
    if (typeof navigator === 'undefined' || typeof navigator.sendBeacon !== 'function') return;
    const body = JSON.stringify({
        node_id: ooQualityNode,
        correlation_id: ooQualityCorrelation,
        event: 'closed',
        reason: 'pagehide',
        _token: ooCsrf(),
    });
    try {
        navigator.sendBeacon(url, new Blob([body], { type: 'application/json' }));
    } catch (e) { /* beacon відмовив — сесію дозакриє наступний квиток */ }
}

// Живий індикатор якості OO-сесії: оператор має БАЧИТИ fps/rtt/loss на живій
// сесії, а не дізнаватись про деградацію зі скарги. Компактний рядок біля
// перемикача; порожній текст ховає його (не-live стани). Червоний, коли погано.
// Відновлено 29.08.2026 з коміту 550222ee — пізніша хвиля зрізала ці рядки, і
// в проді індикатор жив, а у вихідниках його не було.
function updateOoQualityIndicator(metrics) {
    const seg = document.getElementById('oo-remote-oo-mode');
    if (!seg) return;
    let el = document.getElementById('oo-quality-indicator');
    if (!metrics) { if (el) el.textContent = ''; return; }
    if (!el) {
        el = document.createElement('span');
        el.id = 'oo-quality-indicator';
        el.style.cssText = 'margin-left:.5rem;font-size:.72rem;opacity:.75;white-space:nowrap;';
        seg.appendChild(el);
    }
    const parts = [];
    if (metrics.fps != null) parts.push(metrics.fps + 'fps');
    if (metrics.rttMs != null) parts.push(metrics.rttMs + 'ms');
    if (metrics.lossPct > 0.01) parts.push(Math.round(metrics.lossPct * 100) + '% втр');
    // Y2: той самий вердикт, що йде в телеметрію (idle/stalled ураховано).
    // Власні пороги тут фарбували червоним нерухомий екран («3fps» на читанні).
    const bad = ooQualityIsBad(metrics);
    el.style.color = bad ? '#dc2626' : '';
    el.textContent = parts.length ? 'OO ' + parts.join(' · ') : '';
}

/** destroyOoLayer — знищення шару. Ідемпотентне; викликається ПЕРЕД m.stop(). */
function destroyOoLayer(reason) {
    const layer = ctl.ooLayer;
    ctl.ooLayer = null;
    ooLayerGen += 1; // усі колбеки старого шару стають no-op
    if (layer && typeof layer.destroy === 'function') {
        try { layer.destroy(reason || 'teardown'); } catch (e) { reportError('oo-layer-destroy', e); }
    }
}

/** Повний демонтаж OO при зміні вкладки/ноди. */
function teardownOoScreen(reason) {
    // «Відключитись» / інший ПК / інша вкладка: сесію закриваємо на сервері
    // ТУТ, а не чекаємо наступного квитка (superseded) чи pagehide. Раніше вона
    // висіла відкритою хвилинами, і тривалість у телеметрії брехала. Спершу
    // недобрана пачка якості — інакше останні секунди метрик губились.
    if (ooQualityNode && ooQualityCorrelation) {
        flushOoQuality();
        sendOoSessionEvent(ooQualityNode, 'closed', reason || 'teardown');
        ooQualityCorrelation = null;
    }
    if (ctl.ooModeCtl) { try { ctl.ooModeCtl.destroy(); } catch (e) { /* ignore */ } ctl.ooModeCtl = null; }
    stopOoAudioOnly();
    if (ctl.ooDisplaysNode) { ctl.ooDisplaysNode = null; clearDisplays(); }
    destroyOoLayer(reason);
    ctl.ooMeshDesktop = null;
}

/**
 * setupOoScreen — піднімає контролер перемикача над уже відкритим Mesh-модулем.
 * Викликається РІВНО після activeModule = m у гілці kind==='desktop'.
 */
async function setupOoScreen(nodeId, meshDesktop) {
    if (!ctl.ooScreenConfig) return;
    const container = document.getElementById('oo-remote-view-desktop');
    const meshCanvas = document.getElementById('oo-remote-canvas');
    if (!container || !meshCanvas) return;

    // Динамічний імпорт — ЛИШЕ тут. Vite ріже його у власний lazy-chunk; у
    // входи файл НЕ додається (пастка 61-байтових заглушок).
    let ooMod;
    try {
        ooMod = await import('./desktop-oo-webrtc.js');
        ooModRef = ooMod;
    } catch (e) {
        reportError('oo-screen-import', e);
        return;
    }
    // Поки вантажився чанк, людина могла перемкнути ПК/вкладку.
    if (ctl.activeModule !== meshDesktop || ctl.activeKind !== 'desktop') return;

    // «Липкий» вибір зберігається ПО ВУЗЛУ, але ПК міг бути переставлений на
    // Windows 7, або вибір лишився з часів, коли ми ще не знали ОС. Непридатна
    // машина стартує з Mesh незалежно від збереженого — інакше консоль сама
    // пішла б у завідомо приречену спробу.
    // Готовність питаємо ДО створення контролера й саме з await: інакше
    // «липкий» oo встиг би стартувати на ПК, з яким нема з ким з’єднуватись, і
    // людина побачила б рівно той банер, заради якого все це й робиться.
    await refreshOoReady();
    if (ctl.activeModule !== meshDesktop || ctl.activeKind !== 'desktop') return;

    const dev = devices.get(nodeId);
    const unsupported = ooUnsupportedReason(dev, ooReadyState(nodeId), ooUnavailableWhy.get(nodeId));
    const initial = unsupported ? 'mesh' : (ooStoredMode(nodeId) || ctl.ooScreenConfig.mode);

    // Вибір звуку — теж по вузлу. Піднімаємо ДО створення контролера, бо його
    // onRender одразу перемалює смугу, і кнопка має вже знати, чий вибір показує.
    ooSoundNode = nodeId;
    ooSoundOn = ooStoredSound(nodeId);

    // F-20: далі замикання беруть Mesh-об’єкт ЗВІДСИ, а не з параметра. Mesh
    // може перепідключитись під живим шаром (reconnectMeshKeepingOo), і тоді
    // resumeMesh/startOo мусять бачити НОВУ сесію, а не закриту.
    ctl.ooMeshDesktop = meshDesktop;

    ctl.ooModeCtl = ooMod.createModeController({
        initialMode: initial,
        persist: (m) => ooStoreMode(nodeId, m),
        log: ooLog,
        showBanner,
        hideBanner,
        onRender: (m) => renderOoSegment(m),
        resumeMesh: () => {
            // Ідемпотентно: сам шар уже робить atomic unpause+refresh, але
            // якщо він помер ДО паузи — тут страховка від застиглого кадру.
            const mesh = ctl.ooMeshDesktop;
            if (mesh && typeof mesh.refresh === 'function') {
                try { mesh.refresh(); } catch (e) { /* ignore */ }
            }
        },
        stopOo: (reason) => destroyOoLayer(reason),
        startOo: () => {
            destroyOoLayer('restart');
            const myGen = ooLayerGen;
            try {
                ctl.ooLayer = ooMod.createOoWebrtcLayer({
                    container,
                    meshDesktop: ctl.ooMeshDesktop || meshDesktop,
                    // Per-node config: requestTicket несе саме цю ноду в §6.4-ticket
                    // (BLOCKER-3), і offer поїде з одноразовим ticket, не з токеном.
                    config: {
                        ...ctl.ooScreenConfig,
                        nodeId,
                        requestTicket: () => fetchOoTicket(nodeId, ctl.ooScreenConfig.signalUrl, true),
                        // Живі метрики якості (getStats кожні 5с) → індикатор біля
                        // перемикача. Авто-фолбек за деградацією лишається opt-in
                        // (config.qualityFallback з ooScreenConfig), тут лише показ.
                        onQuality: (metrics) => {
                            if (myGen !== ooLayerGen) return;
                            updateOoQualityIndicator(metrics);
                            queueOoQualitySample(nodeId, metrics);
                        },
                    },
                    onStateChange: (state, reason) => {
                        // Гейт покоління: колбек шару, знятого мілісекунду тому,
                        // інакше показав би банер «OO втрачено» вже на новому шарі.
                        if (myGen !== ooLayerGen || !ctl.ooModeCtl) return;
                        // §8: 'live' шар віддає рівно на ПЕРШОМУ кадрі
                        // (emit(OO_STATE_LIVE, 'first-frame') за прапорцем sawFrame).
                        if (state === 'live') {
                            sendOoSessionEvent(nodeId, 'first_frame');
                            // Шар народжується німим завжди. Якщо людина в цій сесії
                            // вже просила звук саме для цього ПК — вмикаємо тут, бо
                            // до першого кадру вмикати не було чого.
                            syncOoSound(); // звук переходить із Mesh-зʼєднання в цей шар
                            renderOoSound();
                            // Монітори питаємо аж тут: список має сенс лише коли
                            // картинку реально дає агент, і жоден квиток не горить
                            // дарма на сесії, що так і не піднялась.
                            ctl.ooDisplaysNode = nodeId;
                            loadOoDisplays(nodeId);
                            // C1: нова нога хаба — без стелі; повертаємо вибір тулбара.
                            applyOoToolbarLimits(true);
                        }
                        if (state === 'fallback' || state === 'closed') {
                            reportError('oo-screen', new Error(`${state}: ${reason || 'unknown'}`));
                            updateOoQualityIndicator(null); // сховати індикатор, коли OO не живе
                            syncOoSound();                  // картинка знову в Mesh — звук окремим зʼєднанням
                            renderOoSound();
                            // Картинку знову дає Mesh — перемикач моніторів агента
                            // мусить зникнути разом із нею, інакше клік пішов би в
                            // hub по ноду, якої вже ніхто не дивиться через OO.
                            ctl.ooDisplaysNode = null;
                            clearDisplays();
                            flushOoQuality();               // недобрану пачку не втрачаємо
                            // Фолбек — перехід у межах ТІЄЇ Ж сесії (video_engine
                            // oo -> mesh), тож 'closed' сюди не йде: шар гасне і
                            // при звичайному перемиканні на Mesh, а сесія жива.
                            if (state === 'fallback') sendOoSessionEvent(nodeId, 'fallback', reason);
                        }
                        ctl.ooModeCtl.noteOoState(state, reason);
                    },
                });
            } catch (e) {
                reportError('oo-layer-start', e);
                if (ctl.ooModeCtl && myGen === ooLayerGen) ctl.ooModeCtl.noteOoState('fallback', 'start-failed');
            }
        },
    });

    renderOoSegment(ctl.ooModeCtl.mode());
    ctl.ooModeCtl.start();
    if (initial === 'mesh') syncOoSound(); // збережене «звук увімкнено» під Mesh
}

function onOoSegmentClick(ev) {
    const btn = ev.target.closest ? ev.target.closest('[data-oo-mode]') : null;
    if (!btn || !ctl.ooModeCtl) return;
    ctl.ooModeCtl.setMode(btn.dataset.ooMode);
}

export {
    applyOoToolbarLimits, beaconOoSessionClosed, onOoSegmentClick, onOoSoundClick, ooControlCall, ooLog,
    readOoScreenConfig, refreshOoReady, renderOoSound, setupOoScreen, teardownOoScreen,
};
