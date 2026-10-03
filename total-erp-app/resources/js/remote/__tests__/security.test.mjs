// Безпековий аудит JS-плеєра (tools/oo-screen/SECURITY-AUDIT.md, SecJxx).
// Запуск: node resources/js/remote/__tests__/security.test.mjs
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import { loadDisplayMode, saveDisplayMode, DISPLAY_FIT } from '../desktop-oo-webrtc.js';

const here = dirname(fileURLToPath(import.meta.url));
const files = ['desktop-oo-webrtc.js', 'desktop-oo.js', 'desktop.js'];
const src = Object.fromEntries(files.map((f) => [f, readFileSync(join(here, '..', f), 'utf8')]));

// SecJ01: жодних HTML-синків для віддалених даних (XSS).
for (const [f, s] of Object.entries(src)) {
    for (const sink of ['innerHTML', 'outerHTML', 'insertAdjacentHTML', 'document.write', 'eval(', 'new Function']) {
        assert.ok(!s.includes(sink), `${f}: знайдено ${sink}`);
    }
}

// SecJ02: квиток/токен не пишуться в localStorage/sessionStorage і не логуються.
for (const [f, s] of Object.entries(src)) {
    for (const line of s.split('\n')) {
        if (/(localStorage|sessionStorage|\bst)\.setItem/.test(line)) {
            assert.ok(!/ticket|token/i.test(line), `${f}: секрет у сховищі: ${line.trim()}`);
        }
        if (/console\.(log|info|warn|error|debug)/.test(line)) {
            assert.ok(!/\bticket\b|\btoken\b/.test(line), `${f}: секрет у консолі: ${line.trim()}`);
        }
    }
}

// SecJ03: сміття у сховищі не ламає режим (normalize) — без довіри до localStorage.
const evil = { getItem: () => '<img src=x onerror=alert(1)>', setItem: () => {} };
assert.equal(loadDisplayMode(evil), DISPLAY_FIT);
const stored = [];
saveDisplayMode('"><script>', { setItem: (k, v) => stored.push(v) });
assert.deepEqual(stored, [DISPLAY_FIT]);

// SecJ04: offer несе одноразовий ticket, а не довгоживучий token.
assert.match(src['desktop-oo-webrtc.js'], /JSON\.stringify\(\{\s*sdp:[^}]*ticket\s*\}\)/);
assert.ok(!/JSON\.stringify\(\{[^}]*\btoken\b/.test(src['desktop-oo-webrtc.js']), 'offer несе token');

console.log('security.test.mjs: OK');
