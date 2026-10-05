// N6: пряма нога в плеєрі. Запуск: node resources/js/remote/__tests__/p2p.test.mjs
import assert from 'node:assert/strict';
import {
    p2pOfferUrl, postP2POffer, redeemRelay, waitIceOutcome, negotiateViewer, offerBody,
    createDirectRescue, PATH_DIRECT, PATH_RELAY, resolveP2P,
} from '../desktop-oo-webrtc.js';
import { formatPathLine } from '../desktop-oo-stats.js';

const SIG = 'https://hub/offer/viewer';
const P2P = 'https://hub/p2p/offer';

assert.equal(p2pOfferUrl(SIG), P2P);
assert.equal(p2pOfferUrl('https://hub/x/offer/viewer/'), 'https://hub/x/p2p/offer');
assert.equal(p2pOfferUrl('https://hub/other'), null);
assert.equal(p2pOfferUrl(SIG, 'https://o/p2p'), 'https://o/p2p');
// resolveP2P: типово OFF; відповідь ERP (ticket) має пріоритет над config.
assert.deepEqual(resolveP2P(null, {}), { p2p: false, p2pUrl: undefined });
assert.deepEqual(resolveP2P({}, { p2p: true }), { p2p: true, p2pUrl: undefined });
assert.deepEqual(resolveP2P({ p2p: false }, { p2p: true, p2pUrl: 'https://c' }), { p2p: false, p2pUrl: 'https://c' });
assert.deepEqual(resolveP2P({ p2p: true, p2pUrl: 'https://t' }, { p2pUrl: 'https://c' }), { p2p: true, p2pUrl: 'https://t' });
assert.deepEqual(resolveP2P({}, { p2p: 'true' }), { p2p: false, p2pUrl: undefined });
assert.equal(formatPathLine('direct'), 'Шлях: direct (P2P)');
assert.equal(formatPathLine('relay'), 'Шлях: relay (хаб)');
assert.equal(formatPathLine(null), 'Шлях: —');

function res(status, body) {
    return { status, ok: status >= 200 && status < 300, json: async () => body };
}
// Фейковий fetch: черга відповідей на кожен URL; log — [url, ticket].
function mkFetch(routes, log) {
    return async (url, init) => {
        const body = JSON.parse(init.body);
        log.push([url, body.ticket]);
        const q = routes[url];
        assert.ok(q && q.length, 'неочікуваний запит ' + url);
        const r = q.shift();
        if (r instanceof Error) throw r;
        return r;
    };
}
let peerSeq = 0;
function mkPeer() {
    const p = { n: ++peerSeq, localDescription: { sdp: 'offer-' + peerSeq }, remote: null };
    p.setRemoteDescription = async (d) => { p.remote = d; };
    return p;
}
const noSleep = async () => {};
function base(extra) {
    const tickets = [];
    const paths = [];
    const o = {
        p2p: true, signalUrl: SIG, ticket: 'erp-1', grant: 'control',
        requestTicket: async () => { throw new Error('свіжий квиток не мав знадобитись'); },
        peer: mkPeer(),
        rebuildPeer: async () => mkPeer(),
        waitIce: async () => 'connected',
        makeBody: (sdp, t) => offerBody(sdp, t),
        onTicket: (t, g) => tickets.push([t, g]),
        onPath: (p) => paths.push(p),
        sleep: noSleep,
        ...extra,
    };
    return { o, tickets, paths };
}

// 1) Пряма нога: один запит на /p2p/offer, /offer/viewer не чіпаємо.
{
    const log = [];
    const { o, paths, tickets } = base({ fetchFn: mkFetch({ [P2P]: [res(200, { id: 'a', sdp: 'ans-direct', relay_ticket: 'p2pfb.x' })] }, log) });
    const peer0 = o.peer;
    const r = await negotiateViewer(o);
    assert.equal(r.path, PATH_DIRECT);
    assert.equal(r.peer, peer0);
    assert.equal(peer0.remote.sdp, 'ans-direct');
    assert.deepEqual(log, [[P2P, 'erp-1']]);
    assert.deepEqual(paths, ['direct-connecting', PATH_DIRECT]);
    assert.deepEqual(tickets, []); // ввід лишається з ERP-квитком (агент знімає конверт сам)
    assert.equal(r.relayTicket, 'p2pfb.x'); // на випадок обриву вже зʼєднаної ноги
}

// 2) 409 → relay з relay_ticket тим самим offer-ом; ERP-квиток удруге не шлемо.
{
    const log = [];
    const { o, paths, tickets } = base({
        fetchFn: mkFetch({
            [P2P]: [res(409, { fallback: 'relay', reason: 'multi-viewer', relay_ticket: 'p2pfb.409' })],
            [SIG]: [res(200, { sdp: 'ans-relay' })],
        }, log),
        rebuildPeer: async () => { throw new Error('на 409 peer не перебудовується'); },
    });
    const peer0 = o.peer;
    const r = await negotiateViewer(o);
    assert.equal(r.path, PATH_RELAY);
    assert.equal(r.peer, peer0);
    assert.equal(peer0.remote.sdp, 'ans-relay');
    assert.deepEqual(log, [[P2P, 'erp-1'], [SIG, 'p2pfb.409']]);
    assert.deepEqual(paths, [PATH_RELAY]);
    // Ввід (роль control) переозброєно на тікет relay-ноги — його судить хаб.
    assert.deepEqual(tickets, [['p2pfb.409', 'control']]);
}

// 3) 200, але ICE не зʼєднався → новий peer, relay з relay_ticket. Хаб ще не
//    відпустив квиток (агент не встиг звітувати fallback) → 403, повтор, 200.
{
    const log = [];
    let rebuilt = null;
    const { o, paths, tickets } = base({
        grant: 'view',
        fetchFn: mkFetch({
            [P2P]: [res(200, { id: 'b', sdp: 'ans-direct', relay_ticket: 'p2pfb.ice' })],
            [SIG]: [res(403, null), res(200, { sdp: 'ans-relay' })],
        }, log),
        waitIce: async () => 'failed',
        rebuildPeer: async () => { rebuilt = mkPeer(); return rebuilt; },
    });
    const peer0 = o.peer;
    const r = await negotiateViewer(o);
    assert.equal(r.path, PATH_RELAY);
    assert.equal(r.peer, rebuilt);
    assert.notEqual(r.peer, peer0);
    assert.equal(rebuilt.remote.sdp, 'ans-relay');
    // relay-offer несе SDP НОВОГО peer-а.
    assert.deepEqual(log, [[P2P, 'erp-1'], [SIG, 'p2pfb.ice'], [SIG, 'p2pfb.ice']]);
    assert.deepEqual(paths, ['direct-connecting', PATH_RELAY]);
    // Роль view: grant передається як був — armInput сам не відкриє ввід.
    assert.deepEqual(tickets, [['p2pfb.ice', 'view']]);
}

// 4) Одноразовість: після будь-якої не-403 відповіді relay_ticket більше не шлемо;
//    403 повторюється обмежено; ERP-квиток іде рівно один раз.
{
    const log = [];
    const resp = await redeemRelay({
        url: SIG, relayTicket: 'p2pfb.once', makeBody: (t) => offerBody('s', t), sleep: noSleep,
        fetchFn: mkFetch({ [SIG]: [res(403, null), res(403, null), res(403, null)] }, log), retries: 2,
    });
    assert.equal(resp.status, 403);
    assert.equal(log.length, 3);
    const log2 = [];
    const resp2 = await redeemRelay({
        url: SIG, relayTicket: 'p2pfb.once', makeBody: (t) => offerBody('s', t), sleep: noSleep,
        fetchFn: mkFetch({ [SIG]: [res(500, null)] }, log2),
    });
    assert.equal(resp2.status, 500);
    assert.equal(log2.length, 1);

    // Relay відмовив назовсім → помилка (плеєр іде в Mesh), без другого ERP-запиту.
    const log3 = [];
    const { o } = base({
        fetchFn: mkFetch({
            [P2P]: [res(409, { reason: 'consent', relay_ticket: 'p2pfb.c' })],
            [SIG]: [res(410, null)],
        }, log3),
    });
    await assert.rejects(negotiateViewer(o), /offer\/viewer 410/);
    assert.deepEqual(log3, [[P2P, 'erp-1'], [SIG, 'p2pfb.c']]);
    assert.equal(log3.filter(([, t]) => t === 'erp-1').length, 1);
}

// 5) P2P вимкнено на хабі (404 — маршруту нема): квиток не спожито, той самий
//    ERP-квиток іде на /offer/viewer. Інша помилка — лише зі СВІЖИМ квитком.
{
    const log = [];
    const { o } = base({ fetchFn: mkFetch({ [P2P]: [res(404, null)], [SIG]: [res(200, { sdp: 'r' })] }, log) });
    assert.equal((await negotiateViewer(o)).path, PATH_RELAY);
    assert.deepEqual(log, [[P2P, 'erp-1'], [SIG, 'erp-1']]);

    const log2 = [];
    const b = base({
        fetchFn: mkFetch({ [P2P]: [res(403, null)], [SIG]: [res(200, { sdp: 'r' })] }, log2),
        requestTicket: async () => ({ ticket: 'erp-2', grant: 'control' }),
    });
    assert.equal((await negotiateViewer(b.o)).path, PATH_RELAY);
    assert.deepEqual(log2, [[P2P, 'erp-1'], [SIG, 'erp-2']]);
    assert.deepEqual(b.tickets, [['erp-2', 'control']]);
}

// 6) Прапорець вимкнено (дефолт): /p2p/offer не викликається взагалі.
{
    const log = [];
    const { o } = base({ p2p: false, fetchFn: mkFetch({ [SIG]: [res(200, { sdp: 'r' })] }, log) });
    assert.equal((await negotiateViewer(o)).path, PATH_RELAY);
    assert.deepEqual(log, [[SIG, 'erp-1']]);
}

// 7) postP2POffer: мережева помилка — relay зі свіжим квитком.
{
    const r = await postP2POffer({ url: P2P, ticket: 't', sdp: 's', fetchFn: async () => { throw new TypeError('x'); } });
    assert.deepEqual(r, { kind: PATH_RELAY, reason: 'p2p-network', ticket: 'fresh' });
}

// 8) waitIceOutcome: connected / failed / таймаут.
{
    const p = { iceConnectionState: 'checking' };
    const w = waitIceOutcome(p, 1000);
    p.iceConnectionState = 'connected'; p.oniceconnectionstatechange();
    assert.equal(await w, 'connected');
    const p2 = { iceConnectionState: 'checking' };
    const w2 = waitIceOutcome(p2, 1000);
    p2.iceConnectionState = 'failed'; p2.oniceconnectionstatechange();
    assert.equal(await w2, 'failed');
    let fire;
    const w3 = waitIceOutcome({ iceConnectionState: 'new' }, 5, { setTimeout: (f) => { fire = f; return 1; }, clearTimeout: () => {} });
    fire();
    assert.equal(await w3, 'failed');
}

// 9) Обрив УЖЕ ЗЄДНАНОЇ прямої ноги: порятунок через hub relay з relay_ticket
//    (не Mesh). /p2p/offer і ERP-квиток не чіпаються; новий peer; 403, поки
//    агент не звітував fallback, — обмежений повтор; ввід — на relay-квиток.
{
    const rescue = createDirectRescue();
    assert.equal(rescue.take(PATH_DIRECT), null); // не озброєно — Mesh
    // Сесія 1: пряма нога зʼєдналась -> озброюємо.
    const log = [];
    const first = base({ fetchFn: mkFetch({ [P2P]: [res(200, { id: 'd', sdp: 'ans-direct', relay_ticket: 'p2pfb.drop' })] }, log) });
    const r1 = await negotiateViewer(first.o);
    assert.equal(r1.path, PATH_DIRECT);
    rescue.arm(r1.relayTicket, 'control', SIG);
    assert.equal(rescue.take(PATH_RELAY), null); // впала relay-нога — це Mesh
    assert.ok(rescue.armed());
    // Нога впала: фолбек бере квиток рівно раз.
    const got = rescue.take(PATH_DIRECT);
    assert.deepEqual(got, { relayTicket: 'p2pfb.drop', grant: 'control', signalUrl: SIG });
    assert.equal(rescue.take(PATH_DIRECT), null);
    // Сесія 2 (нова генерація) — relay з цим квитком.
    const log2 = [];
    const second = base({
        ticket: got.relayTicket, grant: got.grant, relayTicket: got.relayTicket,
        fetchFn: mkFetch({ [SIG]: [res(403, null), res(200, { sdp: 'ans-relay' })] }, log2),
        rebuildPeer: async () => { throw new Error('peer уже новий'); },
    });
    const peer2 = second.o.peer;
    const r2 = await negotiateViewer(second.o);
    assert.equal(r2.path, PATH_RELAY);
    assert.equal(r2.peer, peer2);
    assert.equal(peer2.remote.sdp, 'ans-relay');
    assert.deepEqual(log2, [[SIG, 'p2pfb.drop'], [SIG, 'p2pfb.drop']]);
    assert.deepEqual(second.paths, [PATH_RELAY]);
    assert.deepEqual(second.tickets, [['p2pfb.drop', 'control']]);
    // Relay відмовив назовсім (квиток знищено відкликанням S2) — помилка -> Mesh.
    const log3 = [];
    const third = base({ relayTicket: 'p2pfb.revoked', fetchFn: mkFetch({ [SIG]: [res(403, null), res(403, null)] }, log3), relayRetries: 1 });
    await assert.rejects(negotiateViewer(third.o), /offer\/viewer 403/);
    assert.deepEqual(log3.map(([u]) => u), [SIG, SIG]);
    // Без relay_ticket у 200 порятунок не озброюється.
    rescue.arm('', 'control', SIG);
    assert.equal(rescue.take(PATH_DIRECT), null);
}

console.log('p2p.test.mjs: ok');
