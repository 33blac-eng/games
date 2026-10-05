// desktop-oo-multimon.js — F6: кілька моніторів одночасно, бік плеєра.
//
// Хаб (OO_SCREEN_MULTIMON=1) у відповіді POST /control віддає `streams` —
// індекси моніторів, що публікуються ОДНОЧАСНО (0 = основний потік ноди).
// Кожен потік відкривається окремою viewer-ногою: той самий /offer/viewer,
// свіжий одноразовий квиток на ту саму ноду і поле `monitor` у тілі offer-а
// (createOoWebrtcLayer: config.monitor). Нода береться хабом лише з квитка —
// `monitor` її тільки звужує.
//
// Тут — чиста логіка (розбір відповіді, підписи, розкладка side-by-side) і
// маленький DOM-перемикач. Жодного стану сесії: його тримає шар на кожен потік.
// Без `streams` (старий хаб або фіча вимкнена) перемикач не показується —
// поведінка плеєра як до F6.

export const VIEW_ALL = 'all';

/** streamsFrom — індекси потоків з відповіді /control: цілі 0..15, без
 *  повторів, за зростанням. Порожньо, якщо поля немає або там лише 0. */
export function streamsFrom(resp) {
    const raw = resp && Array.isArray(resp.streams) ? resp.streams : [];
    const set = new Set();
    for (const v of raw) {
        if (Number.isInteger(v) && v >= 0 && v <= 15) set.add(v);
    }
    const out = [...set].sort((a, b) => a - b);
    return out.length > 1 ? out : [];
}

/** monitorLabel — «Монітор 2 (2560×1440, основний)» з outputs хаба. */
export function monitorLabel(outputs, idx) {
    const o = Array.isArray(outputs) ? outputs.find((x) => x && x.index === idx) : null;
    let s = 'Монітор ' + (idx + 1);
    if (o && o.width > 0 && o.height > 0) {
        s += ' (' + o.width + '×' + o.height + (o.primary ? ', основний' : '') + ')';
    }
    return s;
}

/** sideBySide — розкладка N кадрів в один ряд у боксі boxW×boxH: спільна
 *  висота, ширини пропорційні кадрам, ціле зменшено до вписування (contain).
 *  sizes: [{w,h}]; невідомий розмір (0) вважається 16:9. Повертає
 *  [{x,y,width,height}] у пікселях боксу, по центру. */
export function sideBySide(boxW, boxH, sizes) {
    if (!(boxW > 0) || !(boxH > 0) || !sizes || sizes.length === 0) return [];
    const ar = sizes.map((s) => (s && s.w > 0 && s.h > 0 ? s.w / s.h : 16 / 9));
    const total = ar.reduce((a, b) => a + b, 0);
    const h = Math.min(boxH, boxW / total);
    const used = h * total;
    let x = (boxW - used) / 2;
    const y = (boxH - h) / 2;
    return ar.map((a) => {
        const r = { x: Math.round(x), y: Math.round(y), width: Math.round(a * h), height: Math.round(h) };
        x += a * h;
        return r;
    });
}

/** normalizeView — VIEW_ALL або індекс із streams; інше -> перший потік. */
export function normalizeView(v, streams) {
    if (v === VIEW_ALL && streams.length > 1) return VIEW_ALL;
    const n = typeof v === 'string' && v !== '' ? Number(v) : v;
    return streams.includes(n) ? n : (streams.length ? streams[0] : 0);
}

/** viewStreams — які потоки відкривати для вибору view. */
export function viewStreams(view, streams) {
    return view === VIEW_ALL ? streams.slice() : [view];
}

/**
 * createMonitorPicker — <select> «Монітор N / Усі поруч». Не робить нічого,
 * якщо streams порожній (повертає null): один монітор — плеєр без змін.
 * onPick(view) кличеться з VIEW_ALL або індексом.
 */
export function createMonitorPicker({ doc, container, streams, outputs, value, onPick }) {
    if (!doc || !container || !streams || streams.length < 2) return null;
    const sel = doc.createElement('select');
    sel.className = 'oo-monitor-picker';
    sel.setAttribute('aria-label', 'Монітор');
    const add = (val, text) => {
        const o = doc.createElement('option');
        o.value = String(val);
        o.textContent = text;
        sel.appendChild(o);
    };
    for (const i of streams) add(i, monitorLabel(outputs, i));
    add(VIEW_ALL, 'Усі поруч (' + streams.length + ')');
    sel.value = String(normalizeView(value, streams));
    sel.addEventListener('change', () => onPick && onPick(normalizeView(sel.value, streams)));
    container.appendChild(sel);
    return { el: sel, destroy() { sel.remove(); } };
}
