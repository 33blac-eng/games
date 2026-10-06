// Тестовий гачок резолву: прод-плеєр імпортує модулі, що живуть лише в ERP
// (oo-input.js тощо) і в цьому репо відсутні. Якщо файлу справді немає —
// підставляємо порожню заглушку, щоб тести плеєра вантажились. Наявні файли
// не чіпаємо. Так само desktop-oo.js у репо старіший за прод: якщо в ньому
// немає createOoRetry, дописуємо no-op експорт (лише в тестах; справжній
// модуль живе в ERP). Запуск: node --import ./__tests__/erp-stubs.mjs <test>.
import { register } from 'node:module';

register('data:text/javascript,' + encodeURIComponent(`
import { existsSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
const STUBS = {
    'oo-input.js': 'export function createOoInput() { return { close() {}, destroy() {} }; }',
};
export async function resolve(spec, ctx, next) {
    const name = spec.split('/').pop();
    if (STUBS[name] && ctx.parentURL && ctx.parentURL.startsWith('file:')) {
        const url = new URL(spec, ctx.parentURL);
        if (!existsSync(fileURLToPath(url))) {
            return { url: 'data:text/javascript,' + encodeURIComponent(STUBS[name]), shortCircuit: true };
        }
    }
    return next(spec, ctx);
}
const RETRY_STUB = '\\nexport function createOoRetry() { return { cancel() {}, noteLive() {}, noteFallback() {} }; }\\n';
export async function load(url, ctx, next) {
    const r = await next(url, ctx);
    if (url.startsWith('file:') && url.endsWith('/desktop-oo.js')) {
        const src = String(r.source);
        if (!/export\\s+function\\s+createOoRetry\\b/.test(src)) {
            return { ...r, source: src + RETRY_STUB, shortCircuit: true };
        }
    }
    return r;
}
`));
