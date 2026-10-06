// Текстові тайли поверх WebRTC-відео (oo-screen, STAGE3-444 варіант B).
//
// Агент на нерухомому екрані шле lossless PNG-тайли кольорового тексту
// каналом "oosc-tiles"; плеєр малює їх на canvas точно над вмістом <video>.
// Будь-яка зміна екрана — invalidate (нова епоха) → canvas чиститься.
//
// Формат повідомлення — дзеркало tools/oo-screen/internal/tiles/proto.go
// (little-endian, 32 байти заголовка + payload). Під прапорцем
// config.textTiles; без нього цей модуль не імпортується в дію.

import { pngHeaderMatches } from './desktop-oo-cursor.js';

export const TILES_LABEL = 'oosc-tiles';
export const HEADER_SIZE = 32;
export const VERSION = 1;
export const TYPE_TILE = 1;
export const TYPE_INVALIDATE = 2;
// TYPE_STILL — агент анонсує, що НАСТУПНИЙ відеокадр — still-повтор
// (keepalive) картинки епізоду. Кожен анонс — кредит на один кадр; кадр без
// кредиту при намальованих тайлах = змінена картинка, що обігнала свій
// invalidate (DataChannel і RTP не впорядковані між собою) → тайли
// ховаємо, доки не прийде запізнілий анонс або invalidate.
// Чому не RTP-мітка: агент пише семпли через pion (випадкова початкова
// мітка), хаб перебазовує мітки — жодна сторона не знає мітки кадру, яку
// бачить браузер; а keepalive щоразу дістає НОВУ мітку.
export const TYPE_STILL = 3;
// TYPE_KEEP — дедуп між епізодами (той самий VERSION 1: старий плеєр невідомий
// тип просто ігнорує). Перше повідомлення епізоду після invalidate:
// payload = N × (u16 x, u16 y, u16 w, u16 h) LE — тайли попередніх епізодів
// (тієї ж srcW×srcH), чиї пікселі не змінились. Плеєр тримає декодовані тайли
// між епохами (store), invalidate лише стирає canvas; keep повертає на canvas
// перелічені й викидає решту, нові/змінені тайли приходять як TYPE_TILE.
export const TYPE_KEEP = 4;
export const KEEP_RECT_SIZE = 8;
export const FORMAT_NONE = 0;
export const FORMAT_PNG = 1;
export const MAX_MESSAGE = 64 * 1024;
export const MAX_TILE_SIDE = 256;
// MAX_SRC_SIDE — tiles.MaxSrcSide: canvas оверлея має розмір srcW×srcH, тож
// без стелі одне повідомлення просило б canvas 65535×65535.
export const MAX_SRC_SIDE = 16384;

// parseTileMessage — ArrayBuffer | Uint8Array → повідомлення або null
// (будь-яке порушення формату = null, нічого не малюємо).
export function parseTileMessage(data) {
    let u8;
    if (data instanceof ArrayBuffer) u8 = new Uint8Array(data);
    else if (ArrayBuffer.isView(data)) u8 = new Uint8Array(data.buffer, data.byteOffset, data.byteLength);
    else return null;
    if (u8.length < HEADER_SIZE || u8.length > MAX_MESSAGE) return null;
    if (u8[0] !== 0x4F || u8[1] !== 0x54 || u8[2] !== VERSION) return null;
    if (u8[25] || u8[26] || u8[27]) return null;
    const dv = new DataView(u8.buffer, u8.byteOffset, u8.byteLength);
    const len = dv.getUint32(28, true);
    if (len !== u8.length - HEADER_SIZE) return null;
    const m = {
        type: u8[3],
        epoch: dv.getUint32(4, true),
        frame: dv.getUint32(8, true),
        x: dv.getUint16(12, true),
        y: dv.getUint16(14, true),
        w: dv.getUint16(16, true),
        h: dv.getUint16(18, true),
        srcW: dv.getUint16(20, true),
        srcH: dv.getUint16(22, true),
        format: u8[24],
        payload: u8.subarray(HEADER_SIZE),
    };
    if (m.type === TYPE_INVALIDATE || m.type === TYPE_STILL) {
        return (len === 0 && m.format === FORMAT_NONE) ? m : null;
    }
    if (m.type === TYPE_KEEP) {
        if (m.format !== FORMAT_NONE || m.x || m.y || m.w || m.h || len % KEEP_RECT_SIZE) return null;
        if (!m.srcW || !m.srcH || m.srcW > MAX_SRC_SIDE || m.srcH > MAX_SRC_SIDE) return null;
        const rects = [];
        for (let o = HEADER_SIZE; o < u8.length; o += KEEP_RECT_SIZE) {
            const r = { x: dv.getUint16(o, true), y: dv.getUint16(o + 2, true),
                w: dv.getUint16(o + 4, true), h: dv.getUint16(o + 6, true) };
            if (!r.w || !r.h || r.w > MAX_TILE_SIDE || r.h > MAX_TILE_SIDE) return null;
            if (r.x + r.w > m.srcW || r.y + r.h > m.srcH) return null;
            rects.push(r);
        }
        m.rects = rects;
        return m;
    }
    if (m.type !== TYPE_TILE || m.format !== FORMAT_PNG || len === 0) return null;
    if (!m.w || !m.h || m.w > MAX_TILE_SIDE || m.h > MAX_TILE_SIDE) return null;
    if (!m.srcW || !m.srcH || m.srcW > MAX_SRC_SIDE || m.srcH > MAX_SRC_SIDE) return null;
    if (m.x + m.w > m.srcW || m.y + m.h > m.srcH) return null;
    // IHDR PNG мусить збігатися з w×h заголовка (як у курсора): інакше 45-байтне
    // повідомлення змусило б createImageBitmap виділяти під 65535×65535.
    if (!pngHeaderMatches(m.payload, m.w, m.h)) return null;
    return m;
}

const tileKey = (r) => r.x + ',' + r.y + ',' + r.w + ',' + r.h;

// createTileState — чиста логіка епох (без DOM), тестується в node.
//   accept(msg) → { clear, draw, entry?, kept?, dropped? }
//     entry — запис сховища для нового тайла (overlay кладе туди bmp);
//     kept — записи, що їх keep повернув у поточну епоху (малювати);
//     dropped — записи, викинуті зі сховища (закрити bmp).
//   onVideoSize(w, h) → true, якщо тайли треба стерти (інші пропорції кадру =
//   інший монітор / інша картинка, тайли до неї не стосуються).
//   onFrames(n) → 'hide' | null — n нових відеокадрів (rVFC presentedFrames);
//   accept(still) → { show: true }, коли кредитів знову вистачає.
// Кредити діють лише з першого TYPE_STILL сесії: старий агент/хаб анонсів
// не шле, і тоді поведінка та сама, що до них (лише invalidate).
// Сховище (store) — тайли, що переживають епохи: запис чинний (малюється),
// коли entry.epoch === поточна епоха. Епізод без keep (старий агент/хаб)
// викидає все старіше за свою епоху — поведінка як до дедупу.
export function createTileState() {
    let epoch = null;
    let srcW = 0;
    let srcH = 0;
    let count = 0;
    let stillSeen = false;
    let credits = 0;
    let keepEpoch = null;
    const store = new Map();
    function dropWhere(f) {
        const out = [];
        for (const [k, e] of store) {
            if (f(e)) { store.delete(k); out.push(e); }
        }
        return out;
    }
    return {
        epoch: () => epoch,
        count: () => count,
        credits: () => credits,
        held: () => store.size,
        source: () => ({ width: srcW, height: srcH }),
        inStore(e) { return store.get(e.key) === e; },
        isLive(e) { return store.get(e.key) === e && e.epoch === epoch; },
        accept(m) {
            if (!m) return { clear: false, draw: false };
            if (m.type === TYPE_STILL) {
                if (m.epoch !== epoch) return { clear: false, draw: false };
                stillSeen = true;
                credits++;
                return { clear: false, draw: false, show: credits >= 0 };
            }
            if (m.type === TYPE_INVALIDATE) {
                epoch = m.epoch;
                count = 0;
                credits = 0;
                return { clear: true, draw: false };
            }
            if (m.type === TYPE_KEEP) {
                const clear = epoch !== m.epoch || count > 0;
                if (epoch !== m.epoch) credits = 0;
                epoch = m.epoch;
                keepEpoch = m.epoch;
                const same = srcW === m.srcW && srcH === m.srcH;
                const want = new Set(m.rects.map(tileKey));
                const dropped = dropWhere((e) => !same || !want.has(e.key));
                srcW = m.srcW;
                srcH = m.srcH;
                const kept = [];
                for (const r of m.rects) {
                    const e = store.get(tileKey(r));
                    if (e && e.epoch !== m.epoch) { e.epoch = m.epoch; kept.push(e); }
                }
                count = kept.length;
                return { clear, draw: false, kept, dropped };
            }
            let clear = false;
            let dropped = [];
            if (epoch !== m.epoch || srcW !== m.srcW || srcH !== m.srcH) {
                clear = count > 0 || epoch !== m.epoch;
                if (srcW !== m.srcW || srcH !== m.srcH) dropped = dropWhere(() => true);
                epoch = m.epoch;
                srcW = m.srcW;
                srcH = m.srcH;
                count = 0;
                credits = 0;
            }
            if (keepEpoch !== m.epoch) {
                dropped = dropped.concat(dropWhere((e) => e.epoch !== m.epoch));
            }
            const key = tileKey(m);
            const old = store.get(key);
            if (old) dropped.push(old);
            const entry = { key, x: m.x, y: m.y, epoch: m.epoch, bmp: null };
            store.set(key, entry);
            count++;
            return { clear, draw: true, entry, dropped };
        },
        isCurrent(e) { return e === epoch; },
        onFrames(n) {
            if (!stillSeen || !(n > 0)) return null;
            if (!count) {
                // Тайлів ще нема: кадри до них (зокрема той, з якого агент
                // робив readback) кредитів не їдять у мінус.
                credits = Math.max(credits - n, 0);
                return null;
            }
            credits -= n;
            return credits < 0 ? 'hide' : null;
        },
        // dropAll — інша картинка: сховище більше ні до чого.
        dropAll() { return dropWhere(() => true); },
        onVideoSize(w, h) {
            if (!count || !(w > 0) || !(h > 0) || !srcW || !srcH) return false;
            // Енкодер може масштабувати — порівнюємо пропорції, не пікселі.
            if (Math.abs(w / h - srcW / srcH) > 0.01) {
                count = 0;
                epoch = null;
                return true;
            }
            return false;
        },
    };
}

// createTileOverlay — canvas над <video>. Внутрішня роздільність canvas =
// роздільність ДЖЕРЕЛА (захопленого екрана), CSS-розмір = containBox
// всередині боксу відео, тож тайл (x,y) лягає рівно на свій піксель.
export function createTileOverlay(o) {
    const doc = o.doc;
    const container = o.container;
    const containBox = o.containBox;
    const decode = o.decode || ((bytes) => createImageBitmap(new Blob([bytes], { type: 'image/png' })));
    const state = createTileState();
    const canvas = doc.createElement('canvas');
    canvas.className = 'oo-screen-text-tiles';
    canvas.style.cssText = 'position:absolute;pointer-events:none;z-index:5;left:0;top:0;width:0;height:0;';
    canvas.width = 1;
    canvas.height = 1;
    container.appendChild(canvas);
    let box = null; // { left, top, width, height } — CSS-бокс <video>
    let destroyed = false;
    let hidden = false;
    let lastPresented = null;

    function setHidden(h) {
        if (hidden === h) return;
        hidden = h;
        canvas.style.visibility = h ? 'hidden' : '';
    }

    function ctx() { return canvas.getContext && canvas.getContext('2d'); }

    function clear() {
        const c = ctx();
        if (c) c.clearRect(0, 0, canvas.width, canvas.height);
        setHidden(false); // порожній canvas ховати нічого
    }

    function release(list) {
        if (!list) return;
        for (const e of list) {
            if (e.bmp && e.bmp.close) e.bmp.close();
            e.bmp = null;
        }
    }

    function layout() {
        const src = state.source();
        if (!box || !src.width || !src.height) return;
        const b = containBox(box.width, box.height, src.width, src.height);
        canvas.style.left = (box.left + b.x) + 'px';
        canvas.style.top = (box.top + b.y) + 'px';
        canvas.style.width = b.width + 'px';
        canvas.style.height = b.height + 'px';
    }

    return {
        canvas,
        state,
        // place — той самий бокс, що syncGeometry ставить <video>.
        // rendering (PLAYER-QUALITY P-3) — image-rendering відео: тайли мусять
        // масштабуватись тим самим фільтром, що й кадр під ними.
        place(left, top, width, height, rendering) {
            box = { left, top, width, height };
            if (typeof rendering === 'string' && canvas.style.imageRendering !== rendering) {
                canvas.style.imageRendering = rendering;
            }
            layout();
        },
        // presentedFrames — з метаданих requestVideoFrameCallback (може
        // стрибати на кілька кадрів, якщо колбек пропустив); без нього — 1.
        onVideoFrame(videoW, videoH, presentedFrames) {
            if (state.onVideoSize(videoW, videoH)) { release(state.dropAll()); clear(); return; }
            let n = 1;
            if (typeof presentedFrames === 'number') {
                n = lastPresented === null ? 1 : presentedFrames - lastPresented;
                lastPresented = presentedFrames;
                if (!(n > 0)) return;
            }
            if (state.onFrames(n) === 'hide') setHidden(true);
        },
        isHidden: () => hidden,
        async onMessage(data) {
            if (destroyed) return;
            const m = parseTileMessage(data);
            if (!m) return;
            const act = state.accept(m);
            release(act.dropped);
            if (act.show) setHidden(false);
            if ((m.type === TYPE_TILE || m.type === TYPE_KEEP) &&
                (canvas.width !== m.srcW || canvas.height !== m.srcH)) {
                canvas.width = m.srcW; // скидає й вміст
                canvas.height = m.srcH;
                layout();
            } else if (act.clear) {
                clear();
            }
            if (act.kept) {
                const c = ctx();
                for (const e of act.kept) {
                    if (c && e.bmp) c.drawImage(e.bmp, e.x, e.y);
                }
            }
            if (!act.draw) return;
            const entry = act.entry;
            let bmp;
            try {
                bmp = await decode(m.payload);
            } catch (e) {
                return;
            }
            // Поки декодували, запис могли витіснити — тоді тайл уже чужий.
            if (destroyed || !entry || !state.inStore(entry)) {
                if (bmp && bmp.close) bmp.close();
                return;
            }
            entry.bmp = bmp;
            // Запис може бути в сховищі, але з минулої епохи (invalidate під
            // час декоду): тримаємо для keep, не малюємо.
            if (!state.isLive(entry)) return;
            const c = ctx();
            if (c) c.drawImage(bmp, m.x, m.y);
        },
        clear,
        destroy() {
            destroyed = true;
            release(state.dropAll());
            if (canvas.parentNode) canvas.parentNode.removeChild(canvas);
        },
    };
}
