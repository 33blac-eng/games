// F5: канал вводу ERP-плеєра. Запуск: node resources/js/remote/__tests__/input-channel.test.mjs
import assert from 'node:assert/strict';
import {
    INPUT_CHANNEL_LABEL, keyEvent, normPoint, wheelNotches, buttonName, createTokenBucket,
    createInputSender, inputEnabledFor, attachInputDom, CLIENT_RATE_PER_SEC,
} from '../desktop-oo-input.js';
import { containBox } from '../desktop-oo-webrtc.js';

assert.equal(INPUT_CHANNEL_LABEL, 'oosc-input'); // == hub inputChannelLabel

// ── роль / grant / прапорець ───────────────────────────────────────────────
assert.equal(inputEnabledFor({}, 'control'), false, 'типово вимкнено');
assert.equal(inputEnabledFor({ inputChannel: true }, 'control'), true);
assert.equal(inputEnabledFor({ inputChannel: true }, 'view'), false, 'перегляд не відкриває канал');
assert.equal(inputEnabledFor({ inputChannel: true }, 'control', 'view'), false, 'grant=view');
assert.equal(inputEnabledFor({ inputChannel: true }, 'control', 'control'), true);

// ── клавіші: формат agent/input.Event ──────────────────────────────────────
assert.deepEqual(keyEvent('KeyA', 'a', true), { v: 1, type: 'key', down: true, scancode: 0x1e });
assert.deepEqual(keyEvent('ArrowLeft', 'ArrowLeft', false), { v: 1, type: 'key', down: false, scancode: 0x4b, extended: true });
assert.deepEqual(keyEvent('Unknown', 'ї', true), { v: 1, type: 'key', down: true, unicode: 'ї'.codePointAt(0) });
assert.equal(keyEvent('Unknown', 'Dead', true), null);
assert.equal(buttonName(0), 'left'); assert.equal(buttonName(2), 'right'); assert.equal(buttonName(9), null);

// ── мапінг: contain-бокс, letterbox поза кадром = null ─────────────────────
const rect = { left: 10, top: 20, width: 200, height: 200 };
const p = normPoint(110, 120, rect, 1920, 1080, containBox);
assert.ok(Math.abs(p.x - 0.5) < 1e-9 && Math.abs(p.y - 0.5) < 1e-9);
assert.equal(normPoint(110, 25, rect, 1920, 1080, containBox), null, 'letterbox');
assert.equal(normPoint(110, 120, rect, 0, 0, containBox), null);
assert.deepEqual(wheelNotches(0, 100, 0), { wheel_x: 0, wheel_y: -1 });
assert.deepEqual(wheelNotches(0, -3, 1), { wheel_x: 0, wheel_y: 1 });
assert.equal(wheelNotches(0, 1e6, 0).wheel_y, -10);

// ── sender: конверт, коалесинг, стеля, відпускання ─────────────────────────
function rig(extra) {
    let t = 0; const timers = []; const out = [];
    const s = createInputSender({
        ticket: 'T', send: (m) => out.push(JSON.parse(m)), now: () => t,
        setTimer: (f) => { timers.push(f); return timers.length; }, clearTimer: () => { timers.length = 0; },
        ...extra,
    });
    return { s, out, tick(ms, fire = true) { t += ms; if (!fire) return; const f = timers.splice(0); f.forEach((x) => x()); } };
}
{
    const r = rig();
    for (let i = 0; i < 50; i++) r.s.move(i / 100, 0.5);
    r.tick(12);
    assert.equal(r.out.length, 1, '50 рухів за 12 мс -> 1 подія');
    assert.equal(r.out[0].ticket, 'T');
    assert.deepEqual(r.out[0].event, { v: 1, type: 'mouse_move', x: 0.49, y: 0.5 });
    assert.equal(r.s.stats.coalesced, 49);
    // рух перед кліком доходить першим
    r.s.move(0.1, 0.1); r.s.button('left', true, { x: 0.1, y: 0.1 });
    assert.deepEqual(r.out.slice(1).map((m) => m.event.type), ['mouse_move', 'mouse_button']);
    r.s.key('KeyA', 'a', true);
    assert.equal(r.s.held(), 2);
    r.s.releaseAll();
    assert.equal(r.s.held(), 0);
    const ups = r.out.slice(-2).map((m) => m.event);
    assert.ok(ups.every((e) => e.down === false));
    // keyup без власного keydown не шлеться
    const n = r.out.length; r.s.key('KeyB', 'b', false); assert.equal(r.out.length, n);
}
{
    // стеля: 1000 кліків-down за одну мить -> рівно burst
    const r = rig({ burst: 30 });
    for (let i = 0; i < 1000; i++) r.s.wheel({ wheel_y: 1 }, null);
    assert.equal(r.out.length, 30);
    assert.equal(r.s.stats.limited, 970);
}
{
    // згода: allowed()=false -> нічого не йде, натиснуте відпускається
    let ok = true;
    const r = rig({ allowed: () => ok });
    r.s.key('ShiftLeft', 'Shift', true);
    ok = false;
    r.s.key('KeyA', 'a', true);
    r.s.move(0.2, 0.2); r.tick(12);
    const evs = r.out.map((m) => m.event);
    assert.deepEqual(evs, [
        { v: 1, type: 'key', down: true, scancode: 0x2a },
        { v: 1, type: 'key', down: false, scancode: 0x2a },
    ]);
    assert.ok(r.s.stats.denied >= 1);
}

// ── симуляція: 10 с чесної роботи (миша 1000 Гц + друк 10 симв/с) ──────────
// Ціль: жодна подія не впирається в стелю хаба (200/с, burst 40) і клієнта.
{
    const r = rig();
    let hubT = 0;
    const hub = createTokenBucket(200, 40, () => hubT);
    let hubDrops = 0; let sent = 0;
    for (let ms = 0; ms < 10000; ms++) {
        r.s.move((ms % 1000) / 1000, 0.5);
        if (ms % 100 === 0) r.s.key('KeyA', 'a', true);
        if (ms % 100 === 50) r.s.key('KeyA', 'a', false);
        r.tick(1, ms % 12 === 11);
        while (sent < r.out.length) { hubT = ms; if (!hub.allow()) hubDrops++; sent++; }
    }
    const perSec = r.out.length / 10;
    console.log(`sim: ${r.out.length} подій / 10 с = ${perSec.toFixed(1)}/с; клієнтська стеля ${CLIENT_RATE_PER_SEC}/с; limited=${r.s.stats.limited}; hubDrops=${hubDrops}; coalesced=${r.s.stats.coalesced}`);
    assert.equal(hubDrops, 0);
    assert.equal(r.s.stats.limited, 0);
}

// ── DOM: capture-фаза глушить Mesh, destroy знімає слухачів ────────────────
{
    const listeners = {};
    const target = {
        tabIndex: -1,
        addEventListener(k, f) { (listeners[k] = listeners[k] || []).push(f); },
        removeEventListener(k, f) { listeners[k] = (listeners[k] || []).filter((x) => x !== f); },
        focus() {},
    };
    const out = [];
    const sender = createInputSender({ ticket: 'T', send: (m) => out.push(JSON.parse(m).event) });
    const dom = attachInputDom({
        target, doc: null, sender, containBox,
        geometry: () => ({ rect: { left: 0, top: 0, width: 100, height: 100 }, srcW: 100, srcH: 100 }),
    });
    assert.equal(target.tabIndex, 0);
    let stopped = 0; let prevented = 0;
    const ev = (o) => ({ cancelable: true, stopPropagation: () => stopped++, preventDefault: () => prevented++, ...o });
    listeners.pointerdown[0](ev({ clientX: 50, clientY: 50, button: 0 }));
    listeners.pointerup[0](ev({ clientX: 50, clientY: 50, button: 0 }));
    listeners.keydown[0](ev({ code: 'Enter', key: 'Enter' }));
    listeners.keyup[0](ev({ code: 'Enter', key: 'Enter' }));
    assert.equal(stopped, 4);
    assert.deepEqual(out.map((e) => [e.type, e.down]), [['mouse_button', true], ['mouse_button', false], ['key', true], ['key', false]]);
    dom.destroy();
    assert.ok(Object.values(listeners).every((l) => l.length === 0));
}
console.log('input-channel: OK');
