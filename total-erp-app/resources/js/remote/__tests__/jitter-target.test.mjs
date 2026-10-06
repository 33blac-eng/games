// Запуск: node --import ./__tests__/erp-stubs.mjs __tests__/jitter-target.test.mjs
// PLAYER-QUALITY P-2: ціль jitter-буфера з гістерезисом + одиниці F-13.
import assert from 'node:assert/strict';
import {
    adaptiveJitterTargetMs, createJitterTargetController, applyLowLatencyReceiver,
} from '../desktop-oo-webrtc.js';

// ── стара чиста функція — без змін ─────────────────────────────────────────
assert.equal(adaptiveJitterTargetMs(0, 0), 0);
assert.equal(adaptiveJitterTargetMs(0.01, 0), 60);
assert.equal(adaptiveJitterTargetMs(0.02, 100), 100);

// ── гістерезис: угору одразу, вниз — після 3 чистих поспіль ────────────────
{
    const c = createJitterTargetController({});
    assert.equal(c.current(), 0);
    assert.equal(c.next(0), 0);
    assert.equal(c.next(0.012), 60);   // втрати — одразу 60
    assert.equal(c.next(0.008), 60);   // 1-й чистий — тримаємо
    assert.equal(c.next(0.011), 60);   // знову втрати — лічильник чистих скинуто
    assert.equal(c.next(0), 60);       // 1
    assert.equal(c.next(0), 60);       // 2
    assert.equal(c.next(0), 0);        // 3 поспіль — назад до бази
    assert.equal(c.next(0), 0);
}
// Стара поведінка на тій самій послідовності «пилкою» — для порівняння:
{
    const seq = [0.012, 0.008, 0.011, 0.009, 0.013, 0.007];
    const old = seq.map((l) => adaptiveJitterTargetMs(l, 0));
    assert.deepEqual(old, [60, 0, 60, 0, 60, 0]); // 5 змін цілі за 30 с
    const c = createJitterTargetController({});
    const now = seq.map((l) => c.next(l));
    assert.deepEqual(now, [60, 60, 60, 60, 60, 60]); // жодної зміни після першої
}
// база з playoutDelaySeconds; holdSamples налаштовується
{
    const c = createJitterTargetController({ baseMs: 100, holdSamples: 1 });
    assert.equal(c.next(0), 100);
    assert.equal(c.next(0.5), 100);    // 100 > 60 — база перемагає
    const d = createJitterTargetController({ baseMs: 20, holdSamples: 1 });
    assert.equal(d.next(0.02), 60);
    assert.equal(d.next(0), 20);       // hold 1 — одразу вниз
}

// ── F-13: одиниці — playoutDelayHint у секундах, jitterBufferTarget у мс ───
{
    const r = { playoutDelayHint: null, jitterBufferTarget: null };
    assert.deepEqual(applyLowLatencyReceiver(r, 'video', 0.05), ['playoutDelayHint', 'jitterBufferTarget']);
    assert.equal(r.playoutDelayHint, 0.05);
    assert.equal(r.jitterBufferTarget, 50);
    const a = { jitterBufferTarget: null };
    assert.deepEqual(applyLowLatencyReceiver(a, 'audio', 0), []); // звук не чіпаємо
    assert.equal(a.jitterBufferTarget, null);
    const z = { jitterBufferTarget: 300 };
    applyLowLatencyReceiver(z, 'video');
    assert.equal(z.jitterBufferTarget, 0);
}

console.log('jitter-target: ok');
