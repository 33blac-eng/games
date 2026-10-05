// Запуск: node resources/js/remote/__tests__/multimon.test.mjs
import assert from 'node:assert/strict';
import {
    streamsFrom, monitorLabel, sideBySide, normalizeView, viewStreams,
    createMonitorPicker, VIEW_ALL,
} from '../desktop-oo-multimon.js';
import { offerBody } from '../desktop-oo-webrtc.js';

// offer: без monitor — тіло побайтно як до F6
assert.equal(offerBody('s', 't'), '{"sdp":"s","ticket":"t"}');
assert.equal(offerBody('s', 't', 0), '{"sdp":"s","ticket":"t"}');
assert.equal(offerBody('s', 't', 2), '{"sdp":"s","ticket":"t","monitor":2}');
assert.equal(offerBody('s', 't', '2'), '{"sdp":"s","ticket":"t"}');

// старий хаб / фіча вимкнена / один монітор — перемикача немає
assert.deepEqual(streamsFrom({}), []);
assert.deepEqual(streamsFrom(null), []);
assert.deepEqual(streamsFrom({ streams: [0] }), []);
assert.deepEqual(streamsFrom({ streams: [2, 0, 1, 1, -1, 16, 1.5, '3'] }), [0, 1, 2]);

assert.equal(monitorLabel([{ index: 1, width: 2560, height: 1440 }], 1), 'Монітор 2 (2560×1440)');
assert.equal(monitorLabel([{ index: 0, width: 1920, height: 1080, primary: true }], 0), 'Монітор 1 (1920×1080, основний)');
assert.equal(monitorLabel(null, 3), 'Монітор 4');

// два 16:9 у боксі 1920×540 — рівно впритул
assert.deepEqual(sideBySide(1920, 540, [{ w: 1920, h: 1080 }, { w: 1920, h: 1080 }]),
    [{ x: 0, y: 0, width: 960, height: 540 }, { x: 960, y: 0, width: 960, height: 540 }]);
// вузький бокс — обмежує ширина, вертикальне центрування
const r = sideBySide(1000, 1000, [{ w: 1600, h: 900 }, { w: 1080, h: 1920 }]);
assert.equal(r.length, 2);
assert.ok(r[0].x >= 0 && r[1].x + r[1].width <= 1001);
assert.equal(r[0].height, r[1].height);
assert.ok(r[0].y > 0);
assert.ok(r[0].width > r[1].width); // landscape ширший за portrait
// невідомий розмір = 16:9
assert.deepEqual(sideBySide(1600, 900, [{ w: 0, h: 0 }]), [{ x: 0, y: 0, width: 1600, height: 900 }]);
assert.deepEqual(sideBySide(0, 100, [{ w: 1, h: 1 }]), []);

assert.equal(normalizeView(VIEW_ALL, [0, 1]), VIEW_ALL);
assert.equal(normalizeView('1', [0, 1]), 1);
assert.equal(normalizeView(7, [0, 1]), 0);
assert.equal(normalizeView(VIEW_ALL, [0]), 0);
assert.deepEqual(viewStreams(VIEW_ALL, [0, 2]), [0, 2]);
assert.deepEqual(viewStreams(2, [0, 2]), [2]);

// мінімальний DOM
function el(tag) {
    const e = {
        tag, children: [], value: '', listeners: {}, attrs: {},
        appendChild(c) { this.children.push(c); c.parent = this; return c; },
        setAttribute(k, v) { this.attrs[k] = v; },
        addEventListener(k, f) { this.listeners[k] = f; },
        remove() { const p = this.parent; if (p) p.children = p.children.filter((x) => x !== this); },
    };
    return e;
}
const doc = { createElement: el };
const box = el('div');
assert.equal(createMonitorPicker({ doc, container: box, streams: [] }), null);
assert.equal(box.children.length, 0);
let picked = null;
const p = createMonitorPicker({ doc, container: box, streams: [0, 1], outputs: [], value: 1, onPick: (v) => { picked = v; } });
assert.equal(p.el.children.length, 3);
assert.equal(p.el.value, '1');
p.el.value = VIEW_ALL;
p.el.listeners.change();
assert.equal(picked, VIEW_ALL);
p.destroy();
assert.equal(box.children.length, 0);

console.log('multimon: ok');
