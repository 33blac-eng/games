// desktop-oo-cursor.js — шар курсора OO-плеєра (config.cursorLayer, ТИПОВО
// ВИМКНЕНО). Агент із -cursor-layer не вмальовує вказівник у відео, а шле
// форму й позицію окремим DataChannel-ом 'oosc-cursor' (дзеркало
// tools/oo-screen/internal/cursorproto). Тут — розбір повідомлень і показ.
//
// ХТО МАЛЮЄ КУРСОР (визначення):
//   • КЕРІВНИК (role 'control' — локальна людина водить мишею по цьому
//     екрану): її власний вказівник уже стоїть там, куди поїде віддалений, і
//     без затримки. Тому окремий віддалений курсор НЕ малюємо — лише міняємо
//     ФОРМУ локального на віддалену (CSS cursor:url(...) з гарячою точкою),
//     щоб I-beam/resize/hand були видні одразу. Форма > 128x128 (стеля
//     браузерів для CSS-курсора) → локальний ховаємо (cursor:none) і малюємо
//     віддалену форму оверлеєм у віддаленій позиції. Віддалений курсор
//     прихований (visible=false, напр. під час набору тексту) → cursor:none.
//   • ГЛЯДАЧ (role 'view' — дивиться, не керує): локальний вказівник не має
//     стосунку до віддаленого, тож віддалений курсор малюємо ЗАВЖДИ оверлеєм
//     у віддаленій позиції, а локальний не чіпаємо.

export const CURSOR_CHANNEL_LABEL = 'oosc-cursor';
export const CSS_CURSOR_MAX = 128;     // стеля розміру CSS-курсора в браузерах
export const CURSOR_MAX_DIM = 256;     // cursorproto.MaxDim
export const CURSOR_MAX_MESSAGE = 65535;

const MAGIC = 0x43; // 'C'
const KIND_POS = 1;
const KIND_SHAPE = 2;
const FORMAT_RGBA = 0;
const FORMAT_PNG = 1;
const POS_SIZE = 20;
const SHAPE_HEADER = 16;

export const ROLE_CONTROL = 'control';
export const ROLE_VIEW = 'view';

function asBytes(data) {
    if (data instanceof Uint8Array) return data;
    if (data instanceof ArrayBuffer) return new Uint8Array(data);
    if (ArrayBuffer.isView(data)) return new Uint8Array(data.buffer, data.byteOffset, data.byteLength);
    return null;
}

// decodeCursorMessage — дзеркало cursorproto.DecodePos/DecodeShape з тими
// самими межами. null — сміття (ігноруємо, канал не рвемо).
export function decodeCursorMessage(data) {
    const b = asBytes(data);
    if (!b || b.length < 2 || b.length > CURSOR_MAX_MESSAGE || b[0] !== MAGIC) return null;
    const dv = new DataView(b.buffer, b.byteOffset, b.byteLength);
    if (b[1] === KIND_POS) {
        if (b.length !== POS_SIZE) return null;
        return {
            kind: 'pos',
            visible: (b[2] & 1) !== 0,
            shapeId: dv.getUint32(4, true),
            x: dv.getInt32(8, true),
            y: dv.getInt32(12, true),
            frameW: dv.getUint16(16, true),
            frameH: dv.getUint16(18, true),
        };
    }
    if (b[1] === KIND_SHAPE) {
        if (b.length < SHAPE_HEADER) return null;
        const s = {
            kind: 'shape',
            format: b[2],
            id: dv.getUint32(4, true),
            w: dv.getUint16(8, true),
            h: dv.getUint16(10, true),
            hotX: dv.getUint16(12, true),
            hotY: dv.getUint16(14, true),
            data: b.subarray(SHAPE_HEADER),
        };
        if (s.id === 0) return null;
        if (s.w < 1 || s.h < 1 || s.w > CURSOR_MAX_DIM || s.h > CURSOR_MAX_DIM) return null;
        if (s.hotX >= s.w || s.hotY >= s.h) return null;
        if (s.format === FORMAT_RGBA) { if (s.data.length !== s.w * s.h * 4) return null; }
        else if (s.format === FORMAT_PNG) { if (!pngHeaderMatches(s.data, s.w, s.h)) return null; }
        else return null;
        return s;
    }
    return null;
}

// pngHeaderMatches — дзеркало cursorproto.checkPNGHeader: сигнатура PNG і
// IHDR з тими самими w×h, що й заголовок форми. Інакше 30-байтний PNG міг би
// заявити 65535×65535 — і декодер браузера (img / CSS cursor) виділяв би
// пам'ять під IHDR, а не під задекларовані ≤256×256 (decompression bomb).
const PNG_SIG = [0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a];
export function pngHeaderMatches(d, w, h) {
    if (!d || d.length < 29) return false;
    for (let i = 0; i < 8; i++) if (d[i] !== PNG_SIG[i]) return false;
    const dv = new DataView(d.buffer, d.byteOffset, d.byteLength);
    if (dv.getUint32(8) !== 13 || d[12] !== 0x49 || d[13] !== 0x48 || d[14] !== 0x44 || d[15] !== 0x52) return false;
    return dv.getUint32(16) === w && dv.getUint32(20) === h;
}

function base64(bytes) {
    if (typeof Buffer !== 'undefined') return Buffer.from(bytes).toString('base64');
    let s = '';
    for (let i = 0; i < bytes.length; i += 0x8000) {
        s += String.fromCharCode.apply(null, bytes.subarray(i, i + 0x8000));
    }
    return btoa(s);
}

// shapeDataUrl — PNG прямо в data:-URL; сирий RGBA — через canvas (doc
// потрібен лише для цього формату). null — не вийшло.
export function shapeDataUrl(shape, doc) {
    if (!shape) return null;
    if (shape.format === FORMAT_PNG) return 'data:image/png;base64,' + base64(shape.data);
    if (shape.format !== FORMAT_RGBA || !doc) return null;
    try {
        const c = doc.createElement('canvas');
        c.width = shape.w; c.height = shape.h;
        const ctx = c.getContext('2d');
        const img = ctx.createImageData(shape.w, shape.h);
        img.data.set(shape.data);
        ctx.putImageData(img, 0, 0);
        return c.toDataURL('image/png');
    } catch (e) { return null; }
}

// cursorPresentation — ЧИСТЕ рішення «що показати» (див. шапку).
//   css: значення style.cursor для input-поверхні ('' = не чіпати)
//   overlay: малювати віддалену форму елементом у віддаленій позиції
export function cursorPresentation(role, shape, url, visible) {
    const haveShape = !!(shape && url);
    if (role !== ROLE_CONTROL) {
        return { css: '', overlay: haveShape && !!visible };
    }
    if (!visible) return { css: 'none', overlay: false };
    if (!haveShape) return { css: '', overlay: false };
    if (shape.w <= CSS_CURSOR_MAX && shape.h <= CSS_CURSOR_MAX) {
        return { css: 'url("' + url + '") ' + shape.hotX + ' ' + shape.hotY + ', auto', overlay: false };
    }
    return { css: 'none', overlay: true };
}

// resolveCursorRole — config.cursorRole явно ('control'|'view'), інакше
// config.viewOnly=true -> глядач, а за замовчуванням — керівник (Mesh-canvas
// під шаром ловить мишу саме цієї людини).
export function resolveCursorRole(config) {
    const c = config || {};
    if (c.cursorRole === ROLE_CONTROL || c.cursorRole === ROLE_VIEW) return c.cursorRole;
    return c.viewOnly ? ROLE_VIEW : ROLE_CONTROL;
}

// createCursorLayer — DOM-частина.
//   o.doc, o.container — куди класти оверлей;
//   o.targets()        — елементи, на яких ставимо style.cursor (input-поверхня);
//   o.place(pos)       — {x,y,scale} у координатах контейнера або null
//                        (мапінг робить desktop-oo-webrtc.js через containBox);
//   o.role             — ROLE_CONTROL | ROLE_VIEW.
export function createCursorLayer(o) {
    const doc = o.doc;
    const role = o.role === ROLE_VIEW ? ROLE_VIEW : ROLE_CONTROL;
    const shapes = new Map(); // id -> {shape, url}
    const savedCursor = new Map(); // el -> original style.cursor
    let pos = null;
    let img = null;
    let lastCss = null;
    const MAX_CACHE = 64;

    function overlay() {
        if (img) return img;
        img = doc.createElement('img');
        img.className = 'oo-screen-remote-cursor';
        img.alt = '';
        img.style.cssText = 'position:absolute;pointer-events:none;z-index:6;display:none;left:0;top:0;transform-origin:0 0;image-rendering:auto;';
        o.container.appendChild(img);
        return img;
    }

    function setCss(css) {
        if (css === lastCss) return;
        lastCss = css;
        for (const el of (o.targets() || [])) {
            if (!el || !el.style) continue;
            if (!savedCursor.has(el)) savedCursor.set(el, el.style.cursor || '');
            el.style.cursor = css === '' ? savedCursor.get(el) : css;
        }
    }

    function render() {
        const entry = pos ? shapes.get(pos.shapeId) : null;
        const pres = cursorPresentation(role, entry && entry.shape, entry && entry.url, pos && pos.visible);
        setCss(pres.css);
        if (!pres.overlay) { if (img) img.style.display = 'none'; return; }
        const at = o.place(pos);
        if (!at) { if (img) img.style.display = 'none'; return; }
        const el = overlay();
        if (el.getAttribute('src') !== entry.url) el.setAttribute('src', entry.url);
        const s = at.scale > 0 ? at.scale : 1;
        el.style.width = entry.shape.w + 'px';
        el.style.height = entry.shape.h + 'px';
        el.style.transform = 'translate(' + (at.x - entry.shape.hotX * s) + 'px,' + (at.y - entry.shape.hotY * s) + 'px) scale(' + s + ')';
        el.style.display = 'block';
    }

    return {
        role,
        onMessage(data) {
            const m = decodeCursorMessage(data);
            if (!m) return;
            if (m.kind === 'shape') {
                const url = shapeDataUrl(m, doc);
                if (!url) return;
                if (shapes.size >= MAX_CACHE && !shapes.has(m.id)) shapes.delete(shapes.keys().next().value);
                shapes.set(m.id, { shape: { w: m.w, h: m.h, hotX: m.hotX, hotY: m.hotY, format: m.format }, url });
            } else {
                pos = m;
            }
            render();
        },
        // relayout — геометрія відео змінилась (resize, 1:1).
        relayout() { render(); },
        destroy() {
            for (const [el, c] of savedCursor) { try { el.style.cursor = c; } catch (e) { /* ignore */ } }
            savedCursor.clear();
            if (img && img.parentNode) { try { img.parentNode.removeChild(img); } catch (e) { /* ignore */ } }
            img = null;
            shapes.clear();
            pos = null;
        },
    };
}
