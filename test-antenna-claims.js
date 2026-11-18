#!/usr/bin/env node
/* #789 M2b — unit tests for public/antenna-claims.js, the real production
 * file loaded into a vm context (no copies of the code under test). The
 * owner flow and the admin queue in a browser are in
 * test-antenna-claims-e2e.js. */
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const SRC_PATH = path.join(__dirname, 'public', 'antenna-claims.js');
const SRC = fs.readFileSync(SRC_PATH, 'utf8');
const HEIGHT_SRC_PATH = path.join(__dirname, 'public', 'antenna-height.js');

function load() {
  const ctx = { console, Promise, Math, JSON, Object, Array, String, Number, encodeURIComponent, URLSearchParams };
  ctx.window = ctx;
  ctx.setTimeout = setTimeout;
  ctx.clearTimeout = clearTimeout;
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync(HEIGHT_SRC_PATH, 'utf8'), ctx, { filename: HEIGHT_SRC_PATH });
  vm.runInContext(SRC, ctx, { filename: SRC_PATH });
  return ctx.AntennaClaims;
}

const AC = load();
let passed = 0;
const tests = [];
function test(name, fn) { tests.push({ name, fn }); }
// Values from the vm context are from another realm: compare as JSON.
function eq(a, b, m) { assert.strictEqual(JSON.stringify(a), JSON.stringify(b), m); }

// ── Height: the server's 0–100 m, 0.1 m grid ─────────────────────────────
test('parseHeight accepts the server grid, with a decimal point or comma', () => {
  const ok = { '0': 0, '18.5': 18.5, '18,5': 18.5, ' 7 ': 7, '100': 100, '99.9': 99.9, '18.50': 18.5, '0.1': 0.1 };
  for (const [raw, want] of Object.entries(ok)) {
    const r = AC.parseHeight(raw);
    assert.strictEqual(r.error, undefined, raw + ': ' + r.error);
    assert.strictEqual(r.heightM, want, raw);
  }
});

test('parseHeight refuses what the server would refuse, never rounding it', () => {
  for (const raw of ['', '  ', 'abc', '-1', '100.1', '101', '18.55', '0.05', '1e2', '0x10', '18.', '.5', '1,000.5', null]) {
    const r = AC.parseHeight(raw);
    assert.ok(r.error, JSON.stringify(raw) + ' was accepted as ' + r.heightM);
    assert.strictEqual(r.heightM, undefined, JSON.stringify(raw));
  }
  assert.ok(/0\.1 m/.test(AC.parseHeight('18.55').error), 'a finer step must say why');
});

// ── Name: mirrors antennaclaim.ContainsCode ───────────────────────────────
const CODE = '7KX9QM';
test('checkName: the suggested name fits', () => {
  const r = AC.checkName('Roof Repeater ' + CODE, CODE, 23);
  eq([r.ok, r.bytes, r.budget, r.reason], [true, 20, 23, '']);
  assert.strictEqual(AC.checkName(CODE, CODE, 23).ok, true);
});

test('checkName: the code must be a whole space-separated word', () => {
  for (const name of ['Roof' + CODE, 'Roof ' + CODE + 'x', 'Roof-' + CODE, 'Roof ' + CODE.toLowerCase(), 'Roof']) {
    const r = AC.checkName(name, CODE, 23);
    eq([r.ok, r.reason], [false, 'missing'], name);
  }
});

test('checkName: a code past the 23-byte budget is cut off on air', () => {
  const r = AC.checkName('Kirkebakken Repeater ' + CODE, CODE, 23); // 27 bytes
  eq([r.ok, r.reason, r.bytes], [false, 'cut', 27]);
  // Exactly at the budget still fits.
  assert.strictEqual(AC.checkName('Kirkebakken Rep ' + CODE, CODE, 23).ok, true);
});

test('checkName counts UTF-8 bytes, not characters', () => {
  // "Ærøskøbing" is 10 characters but 13 bytes: 13 + 1 + 6 = 20.
  assert.strictEqual(AC.checkName('Ærøskøbing ' + CODE, CODE, 23).bytes, 20);
  // 8 × "ø" = 16 bytes + 1 + 6 = 23 fits; one more "ø" pushes the code out.
  assert.strictEqual(AC.checkName('ø'.repeat(8) + ' ' + CODE, CODE, 23).ok, true);
  assert.strictEqual(AC.checkName('ø'.repeat(9) + ' ' + CODE, CODE, 23).reason, 'cut');
  // An emoji is 4 bytes.
  assert.strictEqual(AC.checkName('\u{1F4E1} ' + CODE, CODE, 23).bytes, 11);
});

test('checkName: characters the CLI refuses are pointed out', () => {
  for (const c of ['[', ']', '\\', ':', ',', '?', '*']) {
    assert.strictEqual(AC.checkName('Roof' + c + ' ' + CODE, CODE, 23).reason, 'chars', c);
  }
});

test('nameCheckText: bytes against the budget, and over it only when the code is still on air', () => {
  assert.strictEqual(AC.nameCheckText(AC.checkName('Roof Repeater ' + CODE, CODE, 23)), '20 / 23 bytes — the code fits.');
  // Words after the code run past the budget, but the code is in the first 23 bytes.
  const over = AC.checkName('Roof Repeater ' + CODE + ' and more words', CODE, 23);
  assert.strictEqual(over.ok, true);
  assert.strictEqual(AC.nameCheckText(over), '35 bytes; only the first 23 go on air — the code is inside them.');
  const cut = AC.nameCheckText(AC.checkName('Kirkebakken Repeater ' + CODE, CODE, 23));
  assert.ok(/^27 \/ 23 bytes — The name is too long/.test(cut), cut);
  assert.ok(/^5 \/ 23 bytes — The code must appear/.test(AC.nameCheckText(AC.checkName('Roof!', CODE, 23))));
});

// ── Status lines ──────────────────────────────────────────────────────────
test('statusInfo: queued, pending and proven keep polling; the rest are final', () => {
  const live = ['queued', 'pending', 'proven'];
  const final = ['approved', 'rejected', 'revoked', 'expired', 'superseded', 'gone', 'error', undefined];
  live.forEach((s) => assert.strictEqual(AC.statusInfo({ status: s }).done, false, s));
  final.forEach((s) => assert.strictEqual(AC.statusInfo({ status: s }).done, true, String(s)));
  assert.ok(/review/.test(AC.statusInfo({ status: 'proven' }).text));
  assert.ok(/removed/.test(AC.statusInfo({ status: 'approved' }, { clear: true }).text));
  assert.ok(/use your antenna height/.test(AC.statusInfo({ status: 'approved' }, { clear: false }).text));
  assert.strictEqual(AC.statusInfo({ status: 'error', error: 'claim refused' }).text, 'claim refused');
});

test('giveUpText fits the last status the owner saw', () => {
  // Proven: the advert is done, an administrator decides next (#868 review N1).
  assert.strictEqual(AC.giveUpText({ status: 'proven' }), 'Proven; waiting for review. Check again later.');
  for (const st of [{ status: 'pending' }, { status: 'queued' }, null, undefined]) {
    assert.ok(/check again after sending the advert/.test(AC.giveUpText(st)), JSON.stringify(st));
  }
});

// ── Bounded polling ───────────────────────────────────────────────────────
test('pollDelay backs off and is capped', () => {
  assert.strictEqual(AC.pollDelay(0), 2000);
  assert.strictEqual(AC.pollDelay(1), 3000);
  for (let i = 0; i < 100; i++) assert.ok(AC.pollDelay(i) <= 30000 && AC.pollDelay(i) >= 2000, 'attempt ' + i);
  let total = 0;
  for (let i = 0; i < AC.MAX_POLL_ATTEMPTS; i++) total += AC.pollDelay(i);
  assert.ok(total <= 40 * 60 * 1000, 'polling runs ' + total + ' ms before giving up');
});

function fakeTimers() {
  const q = [];
  return {
    setTimeout: (fn, ms) => { const t = { fn, ms }; q.push(t); return t; },
    clearTimeout: (t) => { const i = q.indexOf(t); if (i >= 0) q.splice(i, 1); },
    pending: () => q.length,
    async fire() { const t = q.shift(); assert.ok(t, 'no timer pending'); t.fn(); await new Promise((r) => setImmediate(r)); },
  };
}

test('the poller follows pending → proven → approved and then stops', async () => {
  const T = fakeTimers();
  const answers = ['queued', 'pending', 'pending', 'proven', 'approved'];
  const seen = [];
  const p = AC.createStatusPoller({
    setTimeout: T.setTimeout, clearTimeout: T.clearTimeout,
    fetchStatus: (id) => { assert.strictEqual(id, 'abcdef0123456789'); return Promise.resolve({ status: answers[seen.length] }); },
    onUpdate: (st) => seen.push(st.status),
  });
  p.start('abcdef0123456789');
  for (let i = 0; i < answers.length; i++) await T.fire();
  eq(seen, answers);
  assert.strictEqual(T.pending(), 0, 'kept polling after approval');
  assert.strictEqual(p.isActive(), false);
});

test('the poller gives up after its attempts and can be started again', async () => {
  const T = fakeTimers();
  let calls = 0, gaveUp = 0;
  const p = AC.createStatusPoller({
    setTimeout: T.setTimeout, clearTimeout: T.clearTimeout, maxAttempts: 4,
    fetchStatus: () => { calls++; return Promise.resolve({ status: 'pending' }); },
    onUpdate: () => {}, onGiveUp: () => gaveUp++,
  });
  p.start('abcdef0123456789');
  for (let i = 0; i < 4; i++) await T.fire();
  eq([calls, gaveUp, T.pending()], [4, 1, 0]);
  p.start('abcdef0123456789');
  assert.strictEqual(T.pending(), 1, '"Check again" did not restart polling');
});

test('the poller: 404 means gone, a network error backs off, a dead view stops it', async () => {
  const T = fakeTimers();
  const seen = [];
  let fail = { status: 500 };
  const p = AC.createStatusPoller({
    setTimeout: T.setTimeout, clearTimeout: T.clearTimeout,
    fetchStatus: () => Promise.reject(fail),
    onUpdate: (st) => seen.push(st.status),
  });
  p.start('abcdef0123456789');
  await T.fire();
  eq([seen.length, T.pending()], [0, 1], 'a 500 must retry quietly');
  fail = { status: 404 };
  await T.fire();
  eq([seen, T.pending()], [['gone'], 0]);

  let alive = true, calls = 0;
  const q = AC.createStatusPoller({
    setTimeout: T.setTimeout, clearTimeout: T.clearTimeout, isAlive: () => alive,
    fetchStatus: () => { calls++; return Promise.resolve({ status: 'pending' }); }, onUpdate: () => {},
  });
  q.start('abcdef0123456789');
  await T.fire();
  alive = false;
  await T.fire();
  eq([calls, T.pending()], [1, 0], 'polling went on after the view was gone');
});

test('a cancelled or restarted poller ignores answers still in flight', async () => {
  const T = fakeTimers();
  let resolve;
  const seen = [];
  const p = AC.createStatusPoller({
    setTimeout: T.setTimeout, clearTimeout: T.clearTimeout,
    fetchStatus: () => new Promise((r) => { resolve = r; }), onUpdate: (st) => seen.push(st.status),
  });
  p.start('abcdef0123456789');
  await T.fire();
  p.cancel();
  resolve({ status: 'approved' });
  await new Promise((r) => setImmediate(r));
  eq(seen, []);
});

test('an admin poller only waits while the decision is queued', async () => {
  const T = fakeTimers();
  const seen = [];
  const answers = ['queued', 'error'];
  const p = AC.createStatusPoller({
    setTimeout: T.setTimeout, clearTimeout: T.clearTimeout,
    keepGoing: (st) => st.status === 'queued',
    fetchStatus: () => Promise.resolve({ status: answers[seen.length] }), onUpdate: (st) => seen.push(st.status),
  });
  p.start('abcdef0123456789');
  await T.fire();
  await T.fire();
  eq([seen, T.pending()], [['queued', 'error'], 0]);
});

// ── Deep links ────────────────────────────────────────────────────────────
const PK = 'ab'.repeat(32);
test('the node section deep link carries only the request id', () => {
  assert.strictEqual(AC.claimHash(PK, null), '#/nodes/' + PK + '?section=node-antenna');
  const h = AC.claimHash(PK, 'abcdef0123456789');
  assert.strictEqual(h, '#/nodes/' + PK + '?section=node-antenna&claim=abcdef0123456789');
  assert.strictEqual(AC.claimParam(h), 'abcdef0123456789');
  for (const bad of ['?claim=ABCDEF0123456789', '?claim=abc', '?claim=<x>', '?claim=' + 'a'.repeat(65), '', '#/nodes/x']) {
    assert.strictEqual(AC.claimParam('#/nodes/x' + bad), null, bad);
  }
  assert.ok(!/code|7KX9QM/i.test(h), 'the code must never be in the link');
});

test('the admin filter round-trips through the link; proven is the default', () => {
  assert.strictEqual(AC.parseFilter(undefined), 'proven');
  assert.strictEqual(AC.parseFilter('nonsense'), 'proven');
  AC.FILTERS.forEach(([f]) => assert.strictEqual(AC.parseFilter(f), f));
  assert.strictEqual(AC.adminHash('proven'), '#/tools/antenna-claims');
  assert.strictEqual(AC.adminHash('approved'), '#/tools/antenna-claims?filter=approved');
  const statuses = AC.FILTERS.map(([f]) => f).sort();
  eq(statuses, ['approved', 'expired', 'pending', 'proven', 'rejected', 'revoked', 'superseded']);
});

// ── Admin decisions ───────────────────────────────────────────────────────
test('decisionsFor offers only what the server accepts', () => {
  eq(AC.decisionsFor('proven'), ['approve', 'reject']);
  eq(AC.decisionsFor('pending'), ['reject']);
  eq(AC.decisionsFor('approved'), ['revoke']);
  ['rejected', 'revoked', 'expired', 'superseded', '', undefined].forEach((s) =>
    eq(AC.decisionsFor(s), [], String(s)));
});

test('the 409 for a newer approved claim says what to do instead', () => {
  const msg = AC.decisionError({ status: 409, message: 'a newer claim for this node is already approved' });
  assert.ok(/newer claim for this node is already approved/.test(msg), msg);
  assert.ok(/Reject this claim instead/.test(msg), msg);
  const other = AC.decisionError({ status: 409, message: 'claim is approved' });
  assert.ok(/claim is approved/.test(other) && !/Reject this claim instead/.test(other), other);
  // The ingestor's own refusal arrives as a result error with the same text.
  assert.strictEqual(AC.decisionError({ message: 'a newer claim for this node is already approved' }), msg);
});

(async () => {
  for (const t of tests) {
    try {
      await t.fn();
      passed++;
      console.log('  ✓ ' + t.name);
    } catch (e) {
      console.error('  ✗ ' + t.name + '\n    ' + ((e && e.stack) || e));
      process.exit(1);
    }
  }
  console.log('\ntest-antenna-claims.js: ' + passed + ' passed');
})();
