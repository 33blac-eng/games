// O2: резервний хаб глядача. Запуск: node resources/js/remote/__tests__/standby.test.mjs
import assert from 'node:assert/strict';
import { resolveStandby, signalCandidates, shouldFailover, postOfferWithFailover, combineAbortSignals } from '../desktop-oo-webrtc.js';

// OFF за замовчуванням: рівно один URL.
assert.deepEqual(signalCandidates('https://a/offer/viewer'), ['https://a/offer/viewer']);
assert.deepEqual(signalCandidates('https://a/offer/viewer', 'x'), ['https://a/offer/viewer']);
// Порядок, без дублікатів і сміття.
assert.deepEqual(
    signalCandidates('https://a/o', ['https://b/o', '', 'https://a/o', 5, 'https://c/o']),
    ['https://a/o', 'https://b/o', 'https://c/o'],
);
// Мережева помилка і таймаут спроби — так; teardown — ні.
assert.equal(shouldFailover(new TypeError('fetch failed'), 0), true);
const ab = new Error('x'); ab.name = 'AbortError';
assert.equal(shouldFailover(ab, 0, false), true);
assert.equal(shouldFailover(ab, 0, true), false);
// 404 (агент на іншому хабі) і 5xx від проксі — так; ticket/ACL, 200, 500 — ні.
for (const s of [404, 502, 503, 504]) assert.equal(shouldFailover(null, s), true);
for (const s of [200, 400, 401, 403, 500]) assert.equal(shouldFailover(null, s), false);

// --- Інтеграція циклу offer-а ---
// Фейковий fetch: url -> поведінка. 'hang' — висить до abort сигналу.
function mkFetch(map, log) {
    return (url, init) => {
        const body = JSON.parse(init.body);
        log.push([url, body.ticket]);
        const b = map[url];
        if (b === 'hang') {
            return new Promise((_, rej) => {
                const fail = () => { const e = new Error('aborted'); e.name = 'AbortError'; rej(e); };
                if (init.signal.aborted) { fail(); return; }
                init.signal.addEventListener('abort', fail);
            });
        }
        if (b === 'neterr') return Promise.reject(new TypeError('fetch failed'));
        return Promise.resolve({ status: b, ok: b >= 200 && b < 300 });
    };
}
let n = 0;
const reqTicket = async () => ({ ticket: 't' + (++n), grant: 'view' });
// Фолбек-шлях combineAbortSignals (без AbortSignal.any) — де раніше таймаут давав AbortError без failover.
const fallbackCombine = (sig, ms) => combineAbortSignals(sig, ms, { any: null, timeout: null });

// Node: таймер AbortSignal.timeout() unref-нутий — тримаємо event loop живим.
const keepAlive = setInterval(() => {}, 1000);

async function run(map, combine, teardown) {
    n = 0;
    const log = []; const armed = [];
    const r = await postOfferWithFailover({
        urls: ['A', 'B'], ticket: 't0', requestTicket: reqTicket,
        makeBody: (t) => JSON.stringify({ sdp: 'x', ticket: t }),
        onTicket: (t) => armed.push(t),
        teardownSignal: teardown, timeoutMs: 30, fetchFn: mkFetch(map, log), combine,
    }).then((resp) => ({ resp }), (err) => ({ err }));
    return { ...r, log, armed };
}

// 1) Blackhole основного: таймаут спроби A, B реально пробується (обидва шляхи abort-а).
for (const combine of [fallbackCombine, undefined]) {
    const r = await run({ A: 'hang', B: 200 }, combine);
    assert.equal(r.resp && r.resp.status, 200, 'blackhole -> B');
    assert.deepEqual(r.log, [['A', 't0'], ['B', 't1']]);
}
// 2) Split-brain: A живий, але агент на B -> 404 -> B зі свіжим ticket-ом.
{
    const r = await run({ A: 404, B: 200 });
    assert.equal(r.resp.status, 200);
    assert.deepEqual(r.log, [['A', 't0'], ['B', 't1']]);
    assert.deepEqual(r.armed, ['t1'], 'input перевʼязаний на новий ticket');
}
// 3) Ticket ніколи не надсилається двічі (мережева помилка/502 після можливого споживання).
for (const a of ['neterr', 502]) {
    const r = await run({ A: a, B: 200 });
    const tickets = r.log.map((x) => x[1]);
    assert.equal(r.resp.status, 200);
    assert.equal(new Set(tickets).size, tickets.length, 'ticket reuse');
}
// 4) 403 (ticket) — без failover.
{
    const r = await run({ A: 403, B: 200 });
    assert.equal(r.resp.status, 403);
    assert.equal(r.log.length, 1);
}
// 5) Teardown — без failover і без запиту нового ticket-а.
{
    const ctl = new AbortController();
    setTimeout(() => ctl.abort(), 5);
    const r = await run({ A: 'hang', B: 200 }, fallbackCombine, ctl.signal);
    assert.ok(r.err);
    assert.equal(r.log.length, 1);
    assert.equal(n, 0);
}
// 6) Один кандидат (OFF): поведінка як до O2.
{
    n = 0; const log = [];
    const resp = await postOfferWithFailover({
        urls: ['A'], ticket: 't0', requestTicket: reqTicket,
        makeBody: (t) => JSON.stringify({ ticket: t }), timeoutMs: 30, fetchFn: mkFetch({ A: 404 }, log),
    });
    assert.equal(resp.status, 404); assert.equal(n, 0);
}
clearInterval(keepAlive);
console.log('standby.test.mjs OK');

// O2: резерв із відповіді requestTicket() має пріоритет над config; типово OFF.
assert.equal(resolveStandby({ ticket: 't' }, {}), undefined);
assert.equal(resolveStandby(null, undefined), undefined);
assert.deepEqual(resolveStandby({ standbySignalUrls: ['https://b/o'] }, { standbySignalUrls: ['https://c/o'] }), ['https://b/o']);
assert.deepEqual(resolveStandby({ ticket: 't' }, { standbySignalUrls: ['https://c/o'] }), ['https://c/o']);
console.log('standby resolve OK');
