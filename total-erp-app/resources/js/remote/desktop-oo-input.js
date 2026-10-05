// F5: власний канал вводу ERP-плеєра (браузер -> хаб -> агент), без MeshCentral.
//
// 🔴 ТИПОВО ВИМКНЕНО. Шар підключається лише під config.inputChannel === true
// (і має сенс лише якщо хаб запущено з OO_SCREEN_INPUT=1, а агент з -input).
// Без прапорця offer бітово той самий, що й раніше, і ввід далі ловить Mesh.
//
// РОЛЬ. Канал відкривається ЛИШЕ для ролі 'control' (той самий resolve, що для
// шару курсора: config.cursorRole / config.viewOnly) І лише коли ERP-квиток не
// заявляє іншого grant (requestTicket() може повернути grant). Справжня засувка
// — на хабі: judgeInput рве ВСЮ сесію глядача за квиток без control, тож
// глядач-«перегляд» каналу не відкриває взагалі, інакше втратив би й картинку.
//
// ЗГОДА. Окремої політики згоди (S3) у хабі/агенті поки немає. Якщо ERP її
// матиме, config.inputAllowed() -> bool перевіряється на КОЖНІЙ події: false =
// подія не йде, а все натиснуте відпускається.
//
// ПОДВІЙНИЙ ВВІД. Поки канал відкритий, події ловляться на контейнері у
// CAPTURE-фазі і зупиняються (stopPropagation), тож Mesh-canvas їх не бачить:
// інакше кожен клік пішов би на ПК двічі. Канал закрився — слухачі зняті, ввід
// знову в Mesh.
//
// Формат події = agent/input.Event (v=1, type, x/y 0..1, button, down,
// wheel_x/wheel_y, scancode/extended, unicode), загорнутий у {ticket, event}
// (hub/cmd/hub-webrtc/input.go viewerInputMsg).

export const INPUT_CHANNEL_LABEL = 'oosc-input';
export const INPUT_VERSION = 1;
// Клієнтська стеля нижча за хабову (200/с, burst 40): чесний глядач ніколи не
// має впертися в хабову, бо там перебір = мовчки відкинута подія.
export const CLIENT_RATE_PER_SEC = 150;
export const CLIENT_BURST = 30;
export const MOVE_COALESCE_MS = 12;

// KeyboardEvent.code -> PS/2 set 1 make code; 0xE0xx = extended.
export const SCANCODES = Object.freeze({
    Escape: 0x01, Digit1: 0x02, Digit2: 0x03, Digit3: 0x04, Digit4: 0x05, Digit5: 0x06,
    Digit6: 0x07, Digit7: 0x08, Digit8: 0x09, Digit9: 0x0a, Digit0: 0x0b, Minus: 0x0c,
    Equal: 0x0d, Backspace: 0x0e, Tab: 0x0f, KeyQ: 0x10, KeyW: 0x11, KeyE: 0x12, KeyR: 0x13,
    KeyT: 0x14, KeyY: 0x15, KeyU: 0x16, KeyI: 0x17, KeyO: 0x18, KeyP: 0x19, BracketLeft: 0x1a,
    BracketRight: 0x1b, Enter: 0x1c, ControlLeft: 0x1d, KeyA: 0x1e, KeyS: 0x1f, KeyD: 0x20,
    KeyF: 0x21, KeyG: 0x22, KeyH: 0x23, KeyJ: 0x24, KeyK: 0x25, KeyL: 0x26, Semicolon: 0x27,
    Quote: 0x28, Backquote: 0x29, ShiftLeft: 0x2a, Backslash: 0x2b, KeyZ: 0x2c, KeyX: 0x2d,
    KeyC: 0x2e, KeyV: 0x2f, KeyB: 0x30, KeyN: 0x31, KeyM: 0x32, Comma: 0x33, Period: 0x34,
    Slash: 0x35, ShiftRight: 0x36, NumpadMultiply: 0x37, AltLeft: 0x38, Space: 0x39,
    CapsLock: 0x3a, F1: 0x3b, F2: 0x3c, F3: 0x3d, F4: 0x3e, F5: 0x3f, F6: 0x40, F7: 0x41,
    F8: 0x42, F9: 0x43, F10: 0x44, NumLock: 0x45, ScrollLock: 0x46, Numpad7: 0x47,
    Numpad8: 0x48, Numpad9: 0x49, NumpadSubtract: 0x4a, Numpad4: 0x4b, Numpad5: 0x4c,
    Numpad6: 0x4d, NumpadAdd: 0x4e, Numpad1: 0x4f, Numpad2: 0x50, Numpad3: 0x51,
    Numpad0: 0x52, NumpadDecimal: 0x53, IntlBackslash: 0x56, F11: 0x57, F12: 0x58,
    NumpadEnter: 0xe01c, ControlRight: 0xe01d, NumpadDivide: 0xe035, AltRight: 0xe038,
    Home: 0xe047, ArrowUp: 0xe048, PageUp: 0xe049, ArrowLeft: 0xe04b, ArrowRight: 0xe04d,
    End: 0xe04f, ArrowDown: 0xe050, PageDown: 0xe051, Insert: 0xe052, Delete: 0xe053,
    MetaLeft: 0xe05b, MetaRight: 0xe05c, ContextMenu: 0xe05d,
});

const BUTTONS = ['left', 'middle', 'right', 'x1', 'x2'];

export function buttonName(b) { return BUTTONS[b] || null; }

// keyEvent — KeyboardEvent -> подія агента або null (нема що слати).
export function keyEvent(code, key, down) {
    const sc = SCANCODES[code];
    if (sc) {
        const ev = { v: INPUT_VERSION, type: 'key', down: !!down, scancode: sc & 0xff };
        if (sc > 0xff) ev.extended = true;
        return ev;
    }
    // Невідома фізична клавіша, але є символ — шлемо Unicode (лише одна
    // кодова точка: 'Dead', 'Unidentified' тощо — не символи).
    if (typeof key === 'string' && [...key].length === 1) {
        return { v: INPUT_VERSION, type: 'key', down: !!down, unicode: key.codePointAt(0) };
    }
    return null;
}

// normPoint — клієнтська точка -> 0..1 над кадром (contain-бокс), або null.
export function normPoint(clientX, clientY, rect, srcW, srcH, containBox) {
    if (!rect || !(srcW > 0) || !(srcH > 0)) return null;
    const b = containBox(rect.width, rect.height, srcW, srcH);
    if (!(b.width > 0) || !(b.height > 0)) return null;
    const lx = (clientX - rect.left - b.x) / b.width;
    const ly = (clientY - rect.top - b.y) / b.height;
    if (!(lx >= 0 && ly >= 0 && lx <= 1 && ly <= 1)) return null;
    return { x: lx, y: ly };
}

// wheelNotches — WheelEvent.delta* -> «зубці» агента (+ = вправо / від себе).
// deltaMode 0=px (~100px/зубець у Chrome), 1=рядки (3/зубець), 2=сторінки.
export function wheelNotches(dx, dy, mode) {
    const div = mode === 1 ? 3 : mode === 2 ? 1 : 100;
    const clamp = (v) => Math.max(-10, Math.min(10, v));
    // DOM deltaY>0 = донизу (до себе) -> агент wheel_y<0.
    return { wheel_x: clamp(dx / div), wheel_y: clamp(-dy / div) };
}

// createTokenBucket — той самий закон, що rate.Limiter на хабі.
export function createTokenBucket(ratePerSec, burst, now) {
    let tokens = burst;
    let last = now();
    return {
        allow() {
            const t = now();
            tokens = Math.min(burst, tokens + (t - last) * ratePerSec / 1000);
            last = t;
            if (tokens >= 1) { tokens -= 1; return true; }
            return false;
        },
    };
}

// createInputSender — чиста логіка без DOM: конверт, коалесинг рухів, стеля,
// облік натиснутого (щоб відпустити все при втраті фокусу / закритті).
//   o.send(string), o.ticket, o.allowed() -> bool, o.now(), o.setTimer/clearTimer
export function createInputSender(o) {
    const now = o.now || (() => Date.now());
    const setT = o.setTimer || ((f, ms) => setTimeout(f, ms));
    const clrT = o.clearTimer || ((h) => clearTimeout(h));
    const bucket = createTokenBucket(o.ratePerSec || CLIENT_RATE_PER_SEC, o.burst || CLIENT_BURST, now);
    const heldKeys = new Map();   // code -> key-подія (down)
    const heldButtons = new Set();
    let pendingMove = null;
    let moveTimer = null;
    const stats = { sent: 0, coalesced: 0, limited: 0, denied: 0 };

    function raw(ev, force) {
        // force — відпускання: стеля не сміє лишити клавішу затиснутою на ПК.
        if (!force && !bucket.allow()) { stats.limited++; return false; }
        try { o.send(JSON.stringify({ ticket: o.ticket, event: ev })); } catch (e) { return false; }
        stats.sent++;
        return true;
    }
    function allowed() {
        if (typeof o.allowed === 'function' && !o.allowed()) { stats.denied++; releaseAll(); return false; }
        return true;
    }
    function flushMove() {
        if (moveTimer !== null) { clrT(moveTimer); moveTimer = null; }
        if (pendingMove) { const m = pendingMove; pendingMove = null; raw(m); }
    }
    function move(x, y) {
        if (!allowed()) return;
        if (pendingMove) stats.coalesced++;
        pendingMove = { v: INPUT_VERSION, type: 'mouse_move', x, y };
        if (moveTimer === null) moveTimer = setT(() => { moveTimer = null; flushMove(); }, o.coalesceMs || MOVE_COALESCE_MS);
    }
    function button(name, down, pt) {
        if (!name) return;
        if (down && !allowed()) return;
        const ev = { v: INPUT_VERSION, type: 'mouse_button', button: name, down: !!down };
        if (pt) { ev.x = pt.x; ev.y = pt.y; }
        // Рух перед кліком мусить дійти першим, інакше клік прилетить у старе місце.
        flushMove();
        if (down) { if (raw(ev)) heldButtons.add(name); return; }
        if (heldButtons.delete(name)) raw(ev, true);
    }
    function wheel(w, pt) {
        if (!allowed() || (!w.wheel_x && !w.wheel_y)) return;
        const ev = { v: INPUT_VERSION, type: 'mouse_wheel' };
        if (w.wheel_x) ev.wheel_x = w.wheel_x;
        if (w.wheel_y) ev.wheel_y = w.wheel_y;
        if (pt) { ev.x = pt.x; ev.y = pt.y; }
        flushMove();
        raw(ev);
    }
    function key(code, keyName, down) {
        if (down) {
            if (!allowed()) return;
            const ev = keyEvent(code, keyName, true);
            if (ev && raw(ev)) heldKeys.set(code, ev);
            return;
        }
        // Відпускаємо лише те, що САМІ натиснули (інакше keyup без keydown).
        const held = heldKeys.get(code);
        if (!held) return;
        heldKeys.delete(code);
        raw({ ...held, down: false }, true);
    }
    function releaseAll() {
        for (const ev of heldKeys.values()) raw({ ...ev, down: false }, true);
        heldKeys.clear();
        for (const b of heldButtons) raw({ v: INPUT_VERSION, type: 'mouse_button', button: b, down: false }, true);
        heldButtons.clear();
    }
    function destroy() {
        if (moveTimer !== null) { clrT(moveTimer); moveTimer = null; }
        pendingMove = null;
        releaseAll();
    }
    return { move, button, wheel, key, releaseAll, destroy, stats, held: () => heldKeys.size + heldButtons.size };
}

// inputEnabledFor — чи відкривати канал узагалі.
export function inputEnabledFor(config, role, grant) {
    const c = config || {};
    if (c.inputChannel !== true) return false;
    if (role !== 'control') return false;
    // grant не повернули — вирішує хаб; повернули і він не control — не пробуємо
    // (хаб інакше порвав би всю сесію глядача).
    if (grant !== undefined && grant !== null && grant !== 'control') return false;
    return true;
}

// attachInputDom — DOM-частина: слухачі на контейнері (capture), мапінг через
// o.geometry() -> {rect, srcW, srcH} | null. Повертає { destroy }.
export function attachInputDom(o) {
    const t = o.target;
    const win = (o.doc && o.doc.defaultView) || null;
    const sender = o.sender;
    const pt = (e) => {
        const g = o.geometry();
        return g ? normPoint(e.clientX, e.clientY, g.rect, g.srcW, g.srcH, o.containBox) : null;
    };
    const stop = (e) => { e.stopPropagation(); if (e.cancelable) e.preventDefault(); };
    const h = {
        pointermove(e) { const p = pt(e); if (p) sender.move(p.x, p.y); e.stopPropagation(); },
        pointerdown(e) {
            const p = pt(e);
            if (!p) return;
            stop(e);
            if (typeof t.focus === 'function') { try { t.focus({ preventScroll: true }); } catch (x) { /* ignore */ } }
            sender.button(buttonName(e.button), true, p);
        },
        pointerup(e) { stop(e); sender.button(buttonName(e.button), false, pt(e)); },
        mousemove(e) { e.stopPropagation(); },
        mousedown: stop, mouseup: stop, click: stop, dblclick: stop, contextmenu: stop,
        wheel(e) { const p = pt(e); if (!p) return; stop(e); sender.wheel(wheelNotches(e.deltaX, e.deltaY, e.deltaMode), p); },
        keydown(e) { stop(e); sender.key(e.code, e.key, true); },
        keyup(e) { stop(e); sender.key(e.code, e.key, false); },
    };
    if (!(t.tabIndex >= 0)) { try { t.tabIndex = 0; } catch (x) { /* ignore */ } }
    for (const k of Object.keys(h)) t.addEventListener(k, h[k], { capture: true, passive: false });
    const onBlur = () => sender.releaseAll();
    t.addEventListener('blur', onBlur, true);
    if (win) win.addEventListener('blur', onBlur);
    return {
        destroy() {
            for (const k of Object.keys(h)) t.removeEventListener(k, h[k], { capture: true });
            t.removeEventListener('blur', onBlur, true);
            if (win) win.removeEventListener('blur', onBlur);
            sender.destroy();
        },
    };
}
