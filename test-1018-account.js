/**
 * #1018 M1 — unit tests for public/my-nodes.js (the one owner of My nodes)
 * and the pure helpers of public/account.js, loaded into a vm context.
 *
 *   1. signed out, MyNodes is the old browser list (meshcore-my-nodes);
 *   2. signed in, the account's list is the source of truth, writes go to
 *      the registered writer, a full list refuses an add, and the browser's
 *      own list is never deleted;
 *   3. the migration: the browser's entries the account lacks, merged,
 *      deduplicated by lower-case key and capped; "Not now" sticks per
 *      account; signing out returns to the browser list;
 *   4. the Home grid page: 24 cards, then "Show more";
 *   5. no public/*.js file other than my-nodes.js touches the storage key
 *      (the six direct readers moved to the module);
 *   6. account.js: the name budget check (31/23 bytes, a code cut off, UTF-8
 *      boundaries), status lines, pasted keys, companions only, the server
 *      node shape, the stored sign-in's lifetime.
 *
 * Usage: node test-1018-account.js
 */
'use strict';
const fs = require('fs');
const path = require('path');
const vm = require('vm');
const assert = require('assert');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + (e && e.stack || e)); }
}

function memStorage() {
  const m = new Map();
  return {
    getItem: (k) => (m.has(k) ? m.get(k) : null),
    setItem: (k, v) => m.set(k, String(v)),
    removeItem: (k) => m.delete(k),
    _map: m,
  };
}

function load(files) {
  const ctx = { localStorage: memStorage(), sessionStorage: memStorage(), console, Date, JSON, Math, encodeURIComponent, isFinite };
  ctx.window = ctx;
  vm.createContext(ctx);
  for (const f of files) vm.runInContext(fs.readFileSync(path.join(__dirname, 'public', f), 'utf8'), ctx, { filename: f });
  return ctx;
}

const K = (c) => c.repeat(64);
// Values from the vm context have its own Array prototype: compare as JSON.
const j = (v) => JSON.parse(JSON.stringify(v));

console.log('\n=== #1018 M1 MyNodes and account helpers ===\n');

test('signed out: the old browser list, read and written in its old shape', () => {
  const ctx = load(['my-nodes.js']);
  const M = ctx.MyNodes;
  ctx.localStorage.setItem('meshcore-my-nodes', JSON.stringify([{ pubkey: K('a'), name: 'Roof', addedAt: '2026-01-01T00:00:00.000Z' }]));
  assert.strictEqual(M.signedIn(), false);
  assert.deepStrictEqual(j(M.keys()), [K('a')]);
  assert.ok(M.has(K('a').toUpperCase()), 'has() ignores case');
  M.add(K('b'), 'Hill');
  const stored = JSON.parse(ctx.localStorage.getItem('meshcore-my-nodes'));
  assert.deepStrictEqual(stored.map((n) => n.pubkey), [K('a'), K('b')]);
  assert.strictEqual(stored[1].name, 'Hill');
  M.add(K('b'), 'Again');
  assert.strictEqual(M.list().length, 2, 'no duplicate');
  M.remove(K('a'));
  assert.deepStrictEqual(j(M.keys()), [K('b')]);
  ctx.localStorage.setItem('meshcore-my-nodes', '{bad');
  assert.deepStrictEqual(j(M.list()), [], 'unreadable storage is an empty list');
});

test('signed in: the account list, the writer, the cap, the browser list kept', () => {
  const ctx = load(['my-nodes.js']);
  const M = ctx.MyNodes;
  ctx.localStorage.setItem('meshcore-my-nodes', JSON.stringify([{ pubkey: K('a'), name: 'Local' }]));
  const calls = [];
  M.setWriter({ add: (e) => calls.push(['add', e.pubkey]), remove: (pk) => calls.push(['remove', pk]) });
  let changes = 0;
  M.onChange(() => changes++);
  M.setAccount('abcd1234', [{ pubkey: K('c'), name: 'Synced' }], 2);
  assert.strictEqual(M.signedIn(), true);
  assert.deepStrictEqual(j(M.keys()), [K('c')]);
  assert.ok(!M.has(K('a')), 'the browser entry is not on the account');
  assert.strictEqual(M.add(K('d'), 'New'), true);
  assert.strictEqual(M.add(K('e'), 'Over'), false, 'a full list refuses');
  M.remove(K('c'));
  assert.deepStrictEqual(calls, [['add', K('d')], ['remove', K('c')]]);
  assert.deepStrictEqual(j(JSON.parse(ctx.localStorage.getItem('meshcore-my-nodes')).map((n) => n.pubkey)), [K('a')],
    'the browser list is never touched while signed in');
  assert.ok(changes >= 3, 'every change is announced');
  M.clearAccount();
  assert.strictEqual(M.signedIn(), false);
  assert.deepStrictEqual(j(M.keys()), [K('a')], 'signed out: the browser list again');
});

test('migration: candidates, merge with dedupe and cap, Not now per account', () => {
  const ctx = load(['my-nodes.js']);
  const M = ctx.MyNodes;
  ctx.localStorage.setItem('meshcore-my-nodes', JSON.stringify([
    { pubkey: K('a').toUpperCase(), name: 'Dup' }, { pubkey: K('b'), name: 'B' }, { pubkey: K('c'), name: 'C' }, { pubkey: K('b'), name: 'B2' }]));
  assert.deepStrictEqual(j(M.migrationCandidates()), [], 'signed out: nothing to migrate');
  M.setAccount('abcd1234', [{ pubkey: K('a'), name: 'A' }], 200);
  assert.deepStrictEqual(j(M.migrationCandidates().map((n) => n.pubkey)), [K('b'), K('c')]);
  assert.deepStrictEqual(j(M.mergedForMigration(0).map((n) => n.pubkey)), [K('a'), K('b'), K('c')]);
  assert.deepStrictEqual(j(M.mergedForMigration(2).map((n) => n.pubkey)), [K('a'), K('b')], 'capped at maxMyNodes');
  assert.strictEqual(M.migrationDismissed(), false);
  M.dismissMigration();
  assert.strictEqual(M.migrationDismissed(), true);
  M.setAccount('ffff0000', [], 200);
  assert.strictEqual(M.migrationDismissed(), false, '"Not now" belongs to one account');
});

test('Home grid: 24 cards, then Show more adds 24', () => {
  const ctx = load(['my-nodes.js']);
  const M = ctx.MyNodes;
  const list = Array.from({ length: 200 }, (_, i) => ({ pubkey: String(i) }));
  let p = M.gridPage(list, M.GRID_PAGE);
  assert.strictEqual(M.GRID_PAGE, 24);
  assert.strictEqual(p.visible.length, 24);
  assert.strictEqual(p.more, 176);
  p = M.gridPage(list, 48);
  assert.strictEqual(p.visible.length, 48);
  p = M.gridPage(list.slice(0, 10), 0);
  assert.strictEqual(p.visible.length, 10);
  assert.strictEqual(p.more, 0);
});

test('settings export/import carries the browser list only', () => {
  const ctx = load(['my-nodes.js']);
  const M = ctx.MyNodes;
  M.importLocal([{ pubkey: K('a'), name: 'A' }]);
  M.setAccount('abcd1234', [{ pubkey: K('b') }], 200);
  assert.deepStrictEqual(j(M.exportLocal().map((n) => n.pubkey)), [K('a')]);
});

test('no public file other than my-nodes.js touches meshcore-my-nodes', () => {
  const dir = path.join(__dirname, 'public');
  const offenders = fs.readdirSync(dir).filter((f) => f.endsWith('.js') && f !== 'my-nodes.js')
    .filter((f) => fs.readFileSync(path.join(dir, f), 'utf8').includes('meshcore-my-nodes'));
  assert.deepStrictEqual(offenders, []);
  const html = fs.readFileSync(path.join(dir, 'index.html'), 'utf8');
  const at = (s) => html.indexOf('<script src="' + s);
  assert.ok(at('my-nodes.js') > 0 && at('my-nodes.js') < at('customize-v2.js') && at('my-nodes.js') < at('home.js'),
    'my-nodes.js loads before every reader');
  assert.ok(at('account.js') > at('antenna-claims.js'), 'account.js reuses the claim poller');
});

const A = load(['my-nodes.js', 'account.js']).Account;

test('name budget: 31 bytes without a position, 23 with, on UTF-8 boundaries', () => {
  const code = '3HQ7ZB';
  const n31 = 'x'.repeat(24) + ' ' + code;
  assert.strictEqual(A.utf8Bytes(n31), 31);
  assert.ok(A.nameFits(n31, code, 31).ok);
  assert.strictEqual(A.nameFits(n31, code, 23).reason, 'cut');
  assert.strictEqual(A.nameFits('Anna x' + code, code, 31).reason, 'missing');
  assert.strictEqual(A.utf8Bytes('Øre'), 4);
  assert.strictEqual(A.utf8Prefix('ØØØ', 3), 'Ø', 'never half a character');
  assert.strictEqual(A.utf8Prefix('🛰🛰', 5), '🛰');
  assert.ok(/31 bytes: the code fits/.test(A.nameFitsText(A.nameFits(n31, code, 31))));
});

test('status lines: proven redeems, the end states stop polling', () => {
  assert.strictEqual(A.statusLine({ status: 'pending' }).done, false);
  const proven = A.statusLine({ status: 'proven' });
  assert.ok(proven.done && proven.redeem);
  for (const s of ['expired', 'error', 'gone']) assert.ok(A.statusLine({ status: s }).done, s);
  assert.strictEqual(A.statusLine({ status: 'error', error: 'Too many' }).text, 'Too many');
  assert.ok(/heard here/.test(A.loginError({ status: 404 })));
  assert.ok(/few minutes/.test(A.loginError({ status: 429 })));
});

test('pasted keys, companions only, server node shape, stored sign-in lifetime', () => {
  assert.strictEqual(A.normalizeKey('  ' + K('A') + ' '), K('a'));
  assert.strictEqual(A.normalizeKey(K('a').slice(1)), '');
  assert.strictEqual(A.normalizeKey(K('g')), '');
  const list = j(A.companions([{ public_key: K('a'), role: 'companion' }, { public_key: K('b'), role: 'repeater' }, { role: 'companion' }]));
  assert.deepStrictEqual(list.map((n) => n.public_key), [K('a')]);
  assert.deepStrictEqual(JSON.parse(JSON.stringify(A.toServerNode({ pubkey: K('A'), name: 'R', addedAt: '2026-01-01T00:00:00.000Z' }))),
    { pubkey: K('a'), name: 'R', addedAt: Date.parse('2026-01-01T00:00:00.000Z') });
  assert.deepStrictEqual(JSON.parse(JSON.stringify(A.toServerNode({ pubkey: K('a'), addedAt: 'nope' }))), { pubkey: K('a') });
  const now = 1_800_000_000_000;
  const p = { requestId: 'e000000000000001', redeemSecret: 's', expiresAt: now };
  assert.ok(A.pendingUsable(p, now + 9 * 60000), 'usable within the redeem window after the code');
  assert.ok(!A.pendingUsable(p, now + 11 * 60000));
  assert.ok(!A.pendingUsable(null, now));
});

console.log(`\n${passed} passed, ${failed} failed\n`);
process.exit(failed ? 1 : 0);
