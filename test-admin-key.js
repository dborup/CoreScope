#!/usr/bin/env node
/* #789 M3 — unit tests for public/admin-key.js, the admin-key session the
 * admin tools share (channel proposals, antenna claims). The real file runs
 * in a vm context with a recording fetch; the two pages' own suites
 * (test-channel-proposals.js, test-antenna-claims.js and their E2Es) show
 * they still behave as before on top of it. */
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const SRC_PATH = path.join(__dirname, 'public', 'admin-key.js');
const SRC = fs.readFileSync(SRC_PATH, 'utf8');

function load(answer) {
  const calls = [];
  const ctx = { console, Promise, JSON, Object, String, Error };
  ctx.window = ctx;
  ctx.fetch = (url, opts) => {
    calls.push({ url, opts });
    const a = answer || { status: 200, text: '{"ok":true}' };
    return Promise.resolve({ ok: a.status >= 200 && a.status < 300, status: a.status, text: () => Promise.resolve(a.text) });
  };
  vm.createContext(ctx);
  vm.runInContext(SRC, ctx, { filename: SRC_PATH });
  return { AK: ctx.MCAdminKey, calls, ctx };
}

const tests = [];
function test(name, fn) { tests.push({ name, fn }); }

test('a session starts locked and sends no key', async () => {
  const { AK, calls } = load();
  const s = AK.create();
  assert.strictEqual(s.has(), false);
  await s.request('GET', '/admin/x', { admin: true });
  assert.strictEqual(calls[0].url, '/api/admin/x');
  assert.strictEqual(calls[0].opts.headers['X-API-Key'], undefined);
  assert.strictEqual(calls[0].opts.cache, 'no-store');
});

test('the key goes only on admin requests, and Lock forgets it', async () => {
  const { AK, calls } = load();
  const s = AK.create();
  s.set('secret-key-secret-key');
  assert.strictEqual(s.has(), true);
  await s.request('GET', '/admin/x', { admin: true });
  await s.request('GET', '/public/y');
  assert.strictEqual(calls[0].opts.headers['X-API-Key'], 'secret-key-secret-key');
  assert.strictEqual(calls[1].opts.headers['X-API-Key'], undefined, 'a public request carried the key');
  s.clear();
  assert.strictEqual(s.has(), false);
  await s.request('GET', '/admin/x', { admin: true });
  assert.strictEqual(calls[2].opts.headers['X-API-Key'], undefined, 'the key survived Lock');
});

test('an empty key is no key', () => {
  const { AK } = load();
  const s = AK.create();
  s.set('');
  assert.strictEqual(s.has(), false);
  s.set(null);
  assert.strictEqual(s.has(), false);
});

test('each page has its own session: unlocking one does not unlock the other', async () => {
  const { AK, calls } = load();
  const a = AK.create(), b = AK.create();
  a.set('key-for-page-a-only');
  assert.strictEqual(b.has(), false);
  await b.request('GET', '/admin/x', { admin: true });
  assert.strictEqual(calls[0].opts.headers['X-API-Key'], undefined);
});

test('the key is never stored in the page: no storage, no global', () => {
  const code = SRC.replace(/\/\*[\s\S]*?\*\//g, '').replace(/\/\/.*$/gm, '');
  assert.ok(!/localStorage|sessionStorage|indexedDB|document\.cookie/.test(code), 'admin-key.js touches browser storage');
  const { AK, ctx } = load();
  AK.create().set('secret-key-secret-key');
  assert.ok(!JSON.stringify(Object.keys(ctx)).includes('secret'), 'the key leaked into a global');
});

test('bodies are JSON, errors carry the status and the server message', async () => {
  const { AK, calls } = load({ status: 409, text: '{"error":"claim is approved"}' });
  const s = AK.create();
  let err = null;
  try { await s.request('POST', '/admin/x/approve', { admin: true, body: { a: 1 } }); } catch (e) { err = e; }
  assert.strictEqual(calls[0].opts.method, 'POST');
  assert.strictEqual(calls[0].opts.body, '{"a":1}');
  assert.strictEqual(calls[0].opts.headers['Content-Type'], 'application/json');
  assert.ok(err && err.status === 409 && err.message === 'claim is approved', String(err && err.message));
  const bad = load({ status: 500, text: 'not json' });
  let e2 = null;
  try { await bad.AK.create().request('GET', '/x'); } catch (e) { e2 = e; }
  assert.ok(e2 && e2.status === 500 && /500/.test(e2.message), String(e2 && e2.message));
});

test('a signal is passed through for aborts', async () => {
  const { AK, calls } = load();
  const signal = { aborted: false };
  await AK.create().request('GET', '/x', { signal });
  assert.strictEqual(calls[0].opts.signal, signal);
});

test('both admin pages use the shared session, not their own key variable', () => {
  ['channel-proposals.js', 'antenna-claims.js'].forEach((f) => {
    const src = fs.readFileSync(path.join(__dirname, 'public', f), 'utf8');
    assert.ok(/MCAdminKey\.create\(\)/.test(src), f + ' does not use MCAdminKey');
    assert.ok(!/var adminKey\b/.test(src) && !/X-API-Key'\] =/.test(src), f + ' still keeps its own key or header');
  });
  const html = fs.readFileSync(path.join(__dirname, 'public', 'index.html'), 'utf8');
  const at = (s) => html.indexOf('src="' + s);
  assert.ok(at('admin-key.js') > 0, 'index.html does not load admin-key.js');
  assert.ok(at('admin-key.js') < at('channel-proposals.js') && at('admin-key.js') < at('antenna-claims.js'),
    'admin-key.js must load before the pages that use it');
});

(async () => {
  let passed = 0;
  for (const t of tests) {
    try { await t.fn(); passed++; console.log('  ✓ ' + t.name); }
    catch (e) { console.error('  ✗ ' + t.name + ': ' + e.message); }
  }
  console.log(`test-admin-key.js: ${passed} passed, ${tests.length - passed} failed`);
  process.exit(passed === tests.length ? 0 : 1);
})();
