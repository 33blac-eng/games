// N5: ICE-конфіг глядача для суворих firewall-ів (лише TCP 443).
// Запуск: node resources/js/remote/__tests__/ice-config.test.mjs
import assert from 'node:assert/strict';
import { normalizeIceServers, buildRtcConfig } from '../desktop-oo-webrtc.js';

// Порожній/відсутній конфіг = як досі: жодних серверів, політика 'all'.
assert.deepEqual(normalizeIceServers(undefined), []);
assert.deepEqual(normalizeIceServers('stun:x'), []);
assert.deepEqual(buildRtcConfig({}), { iceServers: [], iceTransportPolicy: 'all', bundlePolicy: 'max-bundle' });
assert.deepEqual(buildRtcConfig(undefined).iceServers, []);

// Типовий конфіг із ERP: TURN-TLS 443, TURN UDP/TCP, STUN — у будь-якому порядку.
const erp = [
    { urls: 'turns:turn.example.com:443?transport=tcp', username: 'u', credential: 'p' },
    { urls: ['turn:turn.example.com:3478?transport=tcp', 'turn:turn.example.com:3478'], username: 'u', credential: 'p' },
    { urls: 'stun:turn.example.com:3478' },
];
const n = normalizeIceServers(erp);
assert.deepEqual(n.map((s) => s.urls), [
    ['stun:turn.example.com:3478'],
    ['turn:turn.example.com:3478', 'turn:turn.example.com:3478?transport=tcp'],
    ['turns:turn.example.com:443?transport=tcp'],
], 'UDP першим, TCP далі, TLS останнім');
assert.equal(n[0].username, undefined, 'STUN без облікових даних');
assert.equal(n[2].credential, 'p');

// TURN без облікових даних конструктор RTCPeerConnection відкинув би РАЗОМ з
// усім списком — викидаємо лише його; STUN у тому ж записі лишається.
assert.deepEqual(normalizeIceServers([{ urls: ['turn:x:3478', 'stun:x:3478'] }]), [{ urls: ['stun:x:3478'] }]);
assert.deepEqual(normalizeIceServers([{ urls: 'turns:x:443', username: 'u' }]), []);
// Чужі схеми, сміття, пробіли.
assert.deepEqual(normalizeIceServers([null, 7, { urls: 'http://x' }, { urls: [' stun:y:3478 ', 5] }]), [{ urls: ['stun:y:3478'] }]);

// UDP НЕ вимикаємо: 'relay' лише явним прапорцем діагностики.
assert.equal(buildRtcConfig({ iceTransportPolicy: 'tcp' }).iceTransportPolicy, 'all');
assert.equal(buildRtcConfig({ iceTransportPolicy: 'relay' }).iceTransportPolicy, 'relay');

console.log('ice-config: OK');
