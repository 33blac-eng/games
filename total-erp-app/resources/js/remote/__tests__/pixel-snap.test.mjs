// Запуск: node --import ./__tests__/erp-stubs.mjs __tests__/pixel-snap.test.mjs
// PLAYER-QUALITY P-3: HiDPI — краї відео на цілих фізичних пікселях, «майже
// 1:1» → рівно 1:1, image-rendering, рамка контейнера, тайли тим самим фільтром.
import assert from 'node:assert/strict';
import {
    snapToDeviceBox, snapPx, imageRenderingFor, NEAR_ONE_TO_ONE_DEVICE_PX,
    createOoWebrtcLayer, DISPLAY_1X1, DISPLAY_FIT,
} from '../desktop-oo-webrtc.js';
import { createTileOverlay } from '../oo-text-tiles.js';

const isInt = (v) => Math.abs(v - Math.round(v)) < 1e-9;

// ── snapToDeviceBox ─────────────────────────────────────────────────────────
{
    // 1:1 на dpr 1.25, Mesh-canvas з дробовим зсувом (margin:auto) — краї на сітку,
    // розмір рівно 1920×1080 фізичних, правий/нижній край не виходить за бокс.
    const s = snapToDeviceBox(10.3, 20.1, 1536, 864, 1.25, 1920, 1080);
    assert.equal(s.deviceWidth, 1920);
    assert.equal(s.deviceHeight, 1080);
    assert.ok(s.exact);
    assert.ok(isInt(s.x * 1.25) && isInt(s.y * 1.25));
    assert.ok((s.x + s.width) * 1.25 <= (10.3 + 1536) * 1.25 + 1e-9);
    assert.ok((s.y + s.height) * 1.25 <= (20.1 + 864) * 1.25 + 1e-9);
    assert.ok(Math.abs(s.x - 10.3) * 1.25 <= NEAR_ONE_TO_ONE_DEVICE_PX + 1);
}
{
    // «майже 1:1»: бокс 1535.6 css (1919.5 фізичних) → рівно 1920, добір вліво
    const s = snapToDeviceBox(0, 0, 1535.6, 864, 1.25, 1920, 1080);
    assert.equal(s.deviceWidth, 1920);
    assert.ok(s.x <= 0);                         // від'ємний overflow не прокручується
    assert.ok((s.x + s.width) <= 1535.6 + 1e-9); // правий край — не далі за бокс
    // за межею допуску (3 фізичних) — НЕ підганяємо, лише сітка
    const t = snapToDeviceBox(0, 0, 1533.6, 862.4, 1.25, 1920, 1080);
    assert.equal(t.exact, false);
    assert.equal(t.deviceWidth, 1917);
}
{
    // ціле dpr, цілі координати — без змін
    const s = snapToDeviceBox(5, 7, 960, 540, 2, 1920, 1080);
    assert.deepEqual([s.x, s.y, s.width, s.height], [5, 7, 960, 540]);
    // кадр ще невідомий (0×0) — лише сітка, всередину боксу
    const u = snapToDeviceBox(0.3, 0.3, 100.5, 50.5, 1.5, 0, 0);
    assert.ok(isInt(u.x * 1.5) && isInt(u.width * 1.5));
    assert.ok(u.x >= 0.3 - 1e-9 && u.x + u.width <= 100.8 + 1e-9);
    assert.equal(u.exact, false);
    // вироджений бокс — нуль, не від'ємне
    const z = snapToDeviceBox(10.2, 10.2, 0.1, 0.1, 1, 0, 0);
    assert.equal(z.width, 0);
    assert.equal(z.height, 0);
    // dpr 0/undefined → 1
    assert.equal(snapToDeviceBox(0, 0, 10, 10, 0, 0, 0).width, 10);
}
// snapPx
assert.equal(snapPx(10.3, 1.25), 10.4);
assert.equal(snapPx(10.3, 1), 10);
assert.equal(snapPx(7, undefined), 7);

// ── imageRenderingFor ───────────────────────────────────────────────────────
assert.equal(imageRenderingFor(3840, 2160, 1920, 1080), 'pixelated'); // 1:1 на dpr 2
assert.equal(imageRenderingFor(2560, 1440, 1280, 720), 'pixelated');  // ціле збільшення у fit
assert.equal(imageRenderingFor(1920, 1080, 1920, 1080), 'auto');      // 1:1 — фільтр не потрібен
assert.equal(imageRenderingFor(2400, 1350, 1920, 1080), 'auto');      // 1.25× — драбина з pixelated
assert.equal(imageRenderingFor(1600, 900, 1920, 1080), 'auto');       // зменшення
assert.equal(imageRenderingFor(3840, 2100, 1920, 1080), 'auto');      // осі різні
assert.equal(imageRenderingFor(0, 0, 0, 0), 'auto');

// ── інтеграція: syncGeometry шару на фейковому DOM ─────────────────────────
function makeEnv(o) {
    const dpr = o.dpr;
    const meshRect = { ...o.mesh };
    const children = [];
    let video = null;
    const win = {
        devicePixelRatio: dpr,
        getComputedStyle: () => ({ position: 'relative' }),
        matchMedia: () => ({ addEventListener() {}, removeEventListener() {} }),
    };
    const doc = {
        defaultView: win,
        hidden: false,
        addEventListener() {}, removeEventListener() {},
        createElement(tag) {
            const e = {
                tag, style: {}, parentNode: null,
                setAttribute() {}, addEventListener() {}, removeEventListener() {},
                play: () => Promise.resolve(), pause() {},
                getBoundingClientRect: () => ({ left: 0, top: 0, width: 0, height: 0 }),
            };
            if (tag === 'video') { e.videoWidth = o.video[0]; e.videoHeight = o.video[1]; video = e; }
            return e;
        },
    };
    const mesh = {
        width: o.video[0], height: o.video[1], style: {},
        getContext() { return null; },
        // У режимі 1:1 шар пише CSS-розмір canvas — рект іде за ним.
        getBoundingClientRect() {
            const w = parseFloat(mesh.style.width);
            const h = parseFloat(mesh.style.height);
            return {
                left: meshRect.left, top: meshRect.top,
                width: Number.isFinite(w) ? w : meshRect.width,
                height: Number.isFinite(h) ? h : meshRect.height,
            };
        },
    };
    const container = {
        style: {}, ownerDocument: doc,
        clientLeft: o.border || 0, clientTop: o.border || 0, scrollLeft: 0, scrollTop: 0,
        appendChild(c) { c.parentNode = container; children.push(c); },
        removeChild(c) { children.splice(children.indexOf(c), 1); c.parentNode = null; },
        querySelector: () => mesh,
        getBoundingClientRect: () => ({ left: 0, top: 0, width: 4000, height: 3000 }),
    };
    return { doc, container, mesh, getVideo: () => video };
}

function layerStyles(o, extra) {
    const env = makeEnv(o);
    const layer = createOoWebrtcLayer({
        container: env.container,
        meshDesktop: { canvas: env.mesh },
        config: {
            requestTicket: () => new Promise(() => {}),
            statsOverlay: false, displayToggle: false,
            displayMode: o.mode || DISPLAY_FIT,
            ...(extra || {}),
        },
    });
    const st = { ...env.getVideo().style };
    layer.destroy('test');
    return st;
}
const px = (s) => parseFloat(s);

{
    // 1:1, dpr 1.25, Mesh-canvas на дробовому зсуві
    const st = layerStyles({ dpr: 1.25, video: [1920, 1080], mesh: { left: 10.3, top: 20.1, width: 1536, height: 864 }, mode: DISPLAY_1X1 });
    assert.ok(isInt(px(st.left) * 1.25), 'left на сітці: ' + st.left);
    assert.ok(isInt(px(st.top) * 1.25), 'top на сітці: ' + st.top);
    assert.equal(px(st.width) * 1.25, 1920);
    assert.equal(px(st.height) * 1.25, 1080);
    assert.equal(st.imageRendering, 'auto');
    // відкат: як до P-3 — дробовий зсув лишається
    const old = layerStyles({ dpr: 1.25, video: [1920, 1080], mesh: { left: 10.3, top: 20.1, width: 1536, height: 864 }, mode: DISPLAY_1X1 }, { pixelSnap: false });
    assert.equal(old.left, '10.3px');
    assert.equal(old.top, '20.1px');
}
{
    // 1:1 на dpr 2: CSS 960 = 1920 фізичних = кадр, тобто масштаб 1 — фільтр
    // не потрібен (старе 'pixelated' тут нічого не робило: N×N не буває в 1:1).
    const st = layerStyles({ dpr: 2, video: [1920, 1080], mesh: { left: 0, top: 0, width: 960, height: 540 }, mode: DISPLAY_1X1 });
    assert.equal(st.width, '960px');
    assert.equal(st.imageRendering, 'auto');
}
{
    // fit, dpr 1.5, бокс Mesh на 0.6 фізичного пікселя більший за кадр → рівно 1:1
    const st = layerStyles({ dpr: 1.5, video: [1920, 1080], mesh: { left: 0.3, top: 0.7, width: 1280.4, height: 720.2 } });
    assert.equal(px(st.width) * 1.5, 1920);
    assert.equal(px(st.height) * 1.5, 1080);
    assert.ok(isInt(px(st.left) * 1.5) && isInt(px(st.top) * 1.5));
}
{
    // fit, ціле збільшення 2× — pixelated; дробове — auto
    assert.equal(layerStyles({ dpr: 1, video: [1280, 720], mesh: { left: 0, top: 0, width: 2560, height: 1440 } }).imageRendering, 'pixelated');
    const st = layerStyles({ dpr: 1, video: [1920, 1080], mesh: { left: 0, top: 0, width: 1600, height: 900 } });
    assert.equal(st.imageRendering, 'auto');
    assert.equal(st.width, '1600px');
}
{
    // рамка контейнера 2px: position:absolute рахується від padding-box —
    // відео мусить лягти рівно на Mesh-canvas (у в'юпорті x = 2), а не на 2px правіше.
    const st = layerStyles({ dpr: 1, border: 2, video: [1920, 1080], mesh: { left: 2, top: 2, width: 1920, height: 1080 } });
    assert.equal(st.left, '0px');
    assert.equal(st.top, '0px');
}

// ── тайли: той самий image-rendering, що й відео ───────────────────────────
{
    const el = (tag) => ({ tag, style: {}, parentNode: null });
    const container = { appendChild(c) { c.parentNode = container; }, removeChild() {} };
    const ov = createTileOverlay({ doc: { createElement: el }, container, containBox: (w, h) => ({ x: 0, y: 0, width: w, height: h }) });
    ov.place(0, 0, 960, 540, 'pixelated');
    assert.equal(ov.canvas.style.imageRendering, 'pixelated');
    ov.place(0, 0, 960, 540);              // старий виклик без rendering — не чіпаємо
    assert.equal(ov.canvas.style.imageRendering, 'pixelated');
    ov.place(0, 0, 960, 540, 'auto');
    assert.equal(ov.canvas.style.imageRendering, 'auto');
    ov.destroy();
}

console.log('pixel-snap: ok');
