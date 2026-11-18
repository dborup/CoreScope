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
const ADMIN_KEY_PATH = path.join(__dirname, 'public', 'admin-key.js');

function load() {
  const ctx = { console, Promise, Math, JSON, Object, Array, String, Number, encodeURIComponent, URLSearchParams };
  ctx.window = ctx;
  ctx.setTimeout = setTimeout;
  ctx.clearTimeout = clearTimeout;
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync(HEIGHT_SRC_PATH, 'utf8'), ctx, { filename: HEIGHT_SRC_PATH });
  vm.runInContext(fs.readFileSync(ADMIN_KEY_PATH, 'utf8'), ctx, { filename: ADMIN_KEY_PATH });
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
  // A claim stored before #898 has {clear} for its one height part.
  assert.ok(/removed/.test(AC.statusInfo({ status: 'approved' }, { clear: true }).text));
  assert.ok(/use your antenna height/.test(AC.statusInfo({ status: 'approved' }, { clear: false, heightM: 18.5 }).text));
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
  assert.strictEqual(AC.DEFAULT_SETTINGS.pollMaxAttempts, 60);
  for (let i = 0; i < AC.DEFAULT_SETTINGS.pollMaxAttempts; i++) total += AC.pollDelay(i);
  assert.ok(total > 20 * 60 * 1000 && total <= 40 * 60 * 1000, 'polling runs ' + total + ' ms before giving up');
});

// ── #789 M3: the operator's settings from GET /api/antenna-claims/config ──
test('clientSettings: the M2 constants are the defaults, a config answer replaces them', () => {
  eq(AC.clientSettings(null), { pollInitialMs: 2000, pollBackoff: 1.5, pollMaxMs: 30000, pollMaxAttempts: 60, adminDefaultFilter: 'proven' });
  eq(AC.clientSettings({ enabled: true, pollInitialMs: 1000, pollBackoff: 2, pollMaxMs: 8000, pollMaxAttempts: 5, adminDefaultFilter: 'pending' }),
    { pollInitialMs: 1000, pollBackoff: 2, pollMaxMs: 8000, pollMaxAttempts: 5, adminDefaultFilter: 'pending' });
  // Junk keeps the default, field by field.
  eq(AC.clientSettings({ pollInitialMs: '1000', pollBackoff: 0.5, pollMaxMs: -1, pollMaxAttempts: 2.5, adminDefaultFilter: '<b>' }),
    AC.clientSettings(null));
});

test('pollDelay follows the operator settings', () => {
  const s = { pollInitialMs: 1000, pollBackoff: 2, pollMaxMs: 5000, pollMaxAttempts: 5 };
  eq([0, 1, 2, 3, 4].map((i) => AC.pollDelay(i, s)), [1000, 2000, 4000, 5000, 5000]);
});

test('the poller uses the settings for delays and for when to give up', async () => {
  const T = fakeTimers();
  let gaveUp = 0;
  const p = AC.createStatusPoller({
    setTimeout: T.setTimeout, clearTimeout: T.clearTimeout,
    settings: { pollInitialMs: 1000, pollBackoff: 2, pollMaxMs: 5000, pollMaxAttempts: 3 },
    fetchStatus: () => Promise.resolve({ status: 'pending' }),
    onUpdate: () => {}, onGiveUp: () => gaveUp++,
  });
  p.start('abcdef0123456789');
  const delays = [];
  for (let i = 0; i < 3; i++) { delays.push(T.peek()); await T.fire(); }
  eq([delays, gaveUp, T.pending()], [[1000, 2000, 4000], 1, 0]);
});

test('currentHeight: the profile badge and wording for each source', () => {
  const TP = { heightBadge: (s) => ({ owner: { key: 'owner', label: 'owner' }, 'dsm-estimate': { key: 'estimate', label: 'estimate' } })[s] || null,
    sourceLabel: (s) => ({ owner: 'set by the node owner', 'dsm-estimate': 'estimated from surface model' })[s] || '' };
  const o = AC.currentHeight({ antennaM: 18.5, antennaSource: 'owner', assumed: false }, TP);
  eq([o.height, o.label, o.badge.key], ['18.5 m', 'set by the node owner', 'owner']);
  assert.ok(o.text.startsWith('This site\u2019s height: 18.5 m, set by the node owner. Your own path profiles may use another height'), o.text);
  const e = AC.currentHeight({ antennaM: 22, antennaSource: 'dsm-estimate', assumed: true }, TP);
  eq([e.height, e.badge.key], ['22 m', 'estimate']);
  const p = AC.currentHeight({ antennaM: 10, antennaSource: 'role-default', assumed: true, estimatePending: true }, null);
  assert.ok(/^This site\u2019s height: 10 m, role-default\. A path profile may still raise it/.test(p.text), p.text);
  for (const bad of [null, {}, { antennaM: '18', antennaSource: 'owner' }, { antennaM: 18 }, { antennaM: NaN, antennaSource: 'owner' }]) {
    assert.strictEqual(AC.currentHeight(bad, TP), null, JSON.stringify(bad));
  }
});

// Review PR#893 F7: the line is the site's height; where the viewer's own
// role heights or "Prefer owner heights" can replace it in their profiles,
// a note says so. Operator heights are not theirs to replace.
test('currentHeight: a viewer note for owner, estimate and default heights only', () => {
  const at = (source) => AC.currentHeight({ antennaM: 12, antennaSource: source }, null).viewer;
  for (const s of ['owner', 'dsm-estimate', 'role-default']) {
    const v = at(s);
    assert.ok(/role heights/.test(v) && /Prefer owner heights/.test(v) && /Customize/.test(v), s + ': ' + v);
  }
  for (const s of ['operator-node', 'operator-role']) assert.strictEqual(at(s), '', s);
});

function fakeTimers() {
  const q = [];
  return {
    setTimeout: (fn, ms) => { const t = { fn, ms }; q.push(t); return t; },
    clearTimeout: (t) => { const i = q.indexOf(t); if (i >= 0) q.splice(i, 1); },
    pending: () => q.length,
    peek: () => (q[0] ? q[0].ms : null),
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

test('the admin filter default comes from the settings and is left out of the link', () => {
  assert.strictEqual(AC.parseFilter(undefined, 'pending'), 'pending');
  assert.strictEqual(AC.parseFilter('nonsense', 'approved'), 'approved');
  assert.strictEqual(AC.parseFilter('proven', 'pending'), 'proven');
  assert.strictEqual(AC.adminHash('pending', 'pending'), '#/tools/antenna-claims');
  assert.strictEqual(AC.adminHash('proven', 'pending'), '#/tools/antenna-claims?filter=proven');
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
  assert.ok(/newer approved claim already sets this node’s height/.test(msg), msg);
  assert.ok(/Reject this claim instead/.test(msg), msg);
  const other = AC.decisionError({ status: 409, message: 'claim is approved' });
  assert.ok(/claim is approved/.test(other) && !/Reject this claim instead/.test(other), other);
  // The ingestor's own refusal arrives as a result error with the same text.
  assert.strictEqual(AC.decisionError({ message: 'a newer claim for this node is already approved' }), msg);
});

// ── #898 M2: station details and the combined claim ───────────────────────
test('parseGain: -5 to 20 dBi on the 0.1 grid, comma and minus sign accepted, empty is "not given"', () => {
  const ok = { '': null, '  ': null, '6': 6, '2,5': 2.5, '-5': -5, '−2.5': -2.5, '20': 20, '20.0': 20, '0': 0, '-0': 0, '15.1': 15.1 };
  for (const [raw, want] of Object.entries(ok)) {
    const r = AC.parseGain(raw);
    assert.strictEqual(r.error, undefined, raw + ': ' + r.error);
    assert.ok(Object.is(r.gainDbi, want), JSON.stringify(raw) + ' → ' + r.gainDbi);
  }
  for (const raw of ['-5.1', '20.1', '0.05', '6.25', 'abc', '1e1', '6.', '.5', '100', '--1', '+3']) {
    const r = AC.parseGain(raw);
    assert.ok(r.error && r.gainDbi === undefined, JSON.stringify(raw) + ' accepted as ' + r.gainDbi);
  }
  assert.ok(/0\.1 dBi/.test(AC.parseGain('0.05').error), 'a finer step must say why');
});

test('powerList: ticked sources in the server\'s canonical order, null for none', () => {
  eq(AC.powerList({ battery: true, mains: true }), ['mains', 'battery']);
  eq(AC.powerList({ solar: true, battery: true, mains: true }), ['mains', 'solar', 'battery']);
  assert.strictEqual(AC.powerList({}), null);
  assert.strictEqual(AC.powerList({ wind: true }), null);
  assert.strictEqual(AC.powerList(null), null);
});

test('checkContact mirrors antennaclaim.ValidContact (refused, never rewritten)', () => {
  const ok = ['Jens', 'jens@example.dk', 'OZ1ABC', 'Ærø Radio', '+45 1234 5678', 'a.b_c-d#e/f', 'é' + 'x'.repeat(31), 'Ελλάδα', 'x'.repeat(32)];
  for (const v of ok) eq(AC.checkContact(v), { value: v }, v);
  eq(AC.checkContact(''), { value: '' });
  const bad = ['x'.repeat(33), 'https://evil', 'a:b', 'a‮b', 'a‍b', '<img>', 'a&b', 'a"b', "a'b", 'a\nb', 'a\tb',
    ' lead', 'trail ', 'two  spaces', 'a b', 'emoji 😀', 'ø'.repeat(33), '😀'.repeat(17)];
  for (const v of bad) assert.ok(AC.checkContact(v).error, JSON.stringify(v) + ' was accepted');
  // 32 runes but over 64 bytes (three-byte characters).
  assert.ok(AC.checkContact('中'.repeat(22)).error, '66 bytes accepted');
  assert.strictEqual(AC.checkContact('中'.repeat(21)).value, '中'.repeat(21));
});

const KEEP = { op: 'keep' };
const DET = (o) => Object.assign({ op: 'set', antennaType: '', gainRaw: '', power: {}, installation: '', contactPublic: '', contactAdmin: '' }, o);
test('buildClaimBody: every Keep/Set/Remove combination gives one member per part, never {}', () => {
  const heights = { keep: KEEP, set: { op: 'set', raw: '12,5' }, clear: { op: 'clear', raw: '99' } };
  const details = { keep: KEEP, set: DET({ antennaType: 'omni' }), clear: { op: 'clear', antennaType: 'yagi', contactAdmin: 'x' } };
  for (const h of Object.keys(heights)) {
    for (const d of Object.keys(details)) {
      const r = AC.buildClaimBody({ height: heights[h], details: details[d] });
      if (h === 'keep' && d === 'keep') { assert.ok(r.error && !r.body, 'Keep + Keep built a body'); continue; }
      const keys = Object.keys(r.body).sort();
      const want = [h === 'set' ? 'heightM' : h === 'clear' ? 'clear' : null, d === 'set' ? 'details' : d === 'clear' ? 'clearDetails' : null].filter(Boolean).sort();
      eq(keys, want, h + '/' + d);
      if (h === 'set') assert.strictEqual(r.body.heightM, 12.5);
      if (h === 'clear') assert.strictEqual(r.body.clear, true);
      if (d === 'clear') assert.strictEqual(r.body.clearDetails, true);
    }
  }
  assert.ok(AC.buildClaimBody({}).error);
  assert.ok(AC.buildClaimBody(null).error);
});

test('buildClaimBody: a details set sends only the given fields, both contacts included', () => {
  const r = AC.buildClaimBody({ height: KEEP, details: DET({ antennaType: 'yagi', gainRaw: '6,5', power: { battery: true, solar: true },
    installation: 'mast', contactPublic: 'OZ1ABC', contactAdmin: 'jens@example.dk' }) });
  eq(r.body, { details: { antennaType: 'yagi', gainDbi: 6.5, power: ['solar', 'battery'], installation: 'mast', contactPublic: 'OZ1ABC', contactAdmin: 'jens@example.dk' } });
  // Any non-empty subset of the six fields (N2), the admin contact alone too.
  eq(AC.buildClaimBody({ details: DET({ contactAdmin: 'Jens' }) }).body, { details: { contactAdmin: 'Jens' } });
  eq(AC.buildClaimBody({ details: DET({ gainRaw: '0' }) }).body, { details: { gainDbi: 0 } });
  // Nothing given is not a clear: it is refused (the #898 §1.4 trap).
  const empty = AC.buildClaimBody({ height: KEEP, details: DET({}) });
  assert.ok(empty.error && !empty.body && empty.field === 'details', JSON.stringify(empty));
  // Unknown enum values are not sent.
  eq(AC.buildClaimBody({ details: DET({ antennaType: 'dish', installation: 'tree', power: { wind: true }, gainRaw: '3' }) }).body, { details: { gainDbi: 3 } });
});

test('buildClaimBody: a bad value is refused with the field to focus, nothing sent', () => {
  const cases = [
    [{ height: { op: 'set', raw: '18.55' } }, 'height'],
    [{ details: DET({ gainRaw: '21' }) }, 'gain'],
    [{ details: DET({ antennaType: 'omni', contactPublic: 'https://x' }) }, 'contactPublic'],
    [{ details: DET({ antennaType: 'omni', contactAdmin: 'a‮b' }) }, 'contactAdmin'],
    [{ height: { op: 'set', raw: '10' }, details: DET({ contactAdmin: ' x' }) }, 'contactAdmin'],
  ];
  for (const [c, f] of cases) {
    const r = AC.buildClaimBody(c);
    assert.ok(r.error && !r.body && r.field === f, JSON.stringify(c) + ' → ' + JSON.stringify(r));
  }
  assert.ok(/Public contact/.test(AC.buildClaimBody(cases[2][0]).error));
  assert.ok(/Administrator contact/.test(AC.buildClaimBody(cases[3][0]).error));
});

test('partsOf reads the ops only; a details-only claim is never a height removal (U1)', () => {
  eq(AC.partsOf({ heightOp: '', detailsOp: 'set', heightM: null }), { height: '', details: 'set' });
  eq(AC.partsOf({ detailsOp: 'clear', heightM: null }), { height: '', details: 'clear' });
  eq(AC.partsOf({ heightOp: 'clear', heightM: null }), { height: 'clear', details: '' });
  eq(AC.partsOf({ details: { antennaType: 'omni' } }), { height: '', details: 'set' });
  eq(AC.partsOf({ heightM: 3, clearDetails: true }), { height: 'set', details: 'clear' });
  eq(AC.partsOf({ parts: { height: 'set', details: 'clear' } }), { height: 'set', details: 'clear' });
  // A claim stored before #898.
  eq(AC.partsOf({ clear: true, heightM: null }), { height: 'clear', details: '' });
  eq(AC.partsOf({ clear: false, heightM: 7 }), { height: 'set', details: '' });
});

test('describeClaim and the approved text name each carried part', () => {
  assert.strictEqual(AC.describeClaim({ height: 'set', details: 'set' }, 12.5), 'Setting the antenna height to 12.5 m and replacing the station details of this node.');
  assert.strictEqual(AC.describeClaim({ height: '', details: 'set' }, null), 'Replacing the station details of this node.');
  assert.strictEqual(AC.describeClaim({ height: 'clear', details: 'clear' }, null), 'Removing the owner antenna height and removing the station details of this node.');
  const t = (p) => AC.statusInfo({ status: 'approved' }, { parts: p }).text;
  assert.ok(/^Approved: .*station details are shown/.test(t({ height: '', details: 'set' })) && !/height/.test(t({ height: '', details: 'set' })), t({ height: '', details: 'set' }));
  assert.ok(/^Done: your station details are removed\.$/.test(t({ height: '', details: 'clear' })), t({ height: '', details: 'clear' }));
  assert.ok(/height is removed and your station details are removed/.test(t({ height: 'clear', details: 'clear' })));
  assert.ok(/^Approved: your height is removed|^Approved: your antenna height is removed and your station details are shown/.test(t({ height: 'clear', details: 'set' })), t({ height: 'clear', details: 'set' }));
});

test('stationView: public values only, in order; nothing for null, stale or empty details', () => {
  const v = AC.stationView({ details: { antennaType: 'omni', gainDbi: 6, power: ['solar', 'battery'], installation: 'mast', contactPublic: 'OZ1ABC' }, approvedAt: 1700000000000 });
  eq(v.rows, [['Antenna', 'Omni'], ['Gain', '6.0 dBi'], ['Power', 'Solar + Battery'], ['Installation', 'Mast'], ['Owner contact', 'OZ1ABC']]);
  assert.strictEqual(v.approvedAt, 1700000000000);
  assert.strictEqual(AC.stationView({ details: null, stale: true }), null);
  // A stale answer shows nothing even if it carries details (F4).
  assert.strictEqual(AC.stationView({ details: { antennaType: 'omni', contactPublic: 'OZ1ABC' }, stale: true, approvedAt: 1 }), null);
  assert.notStrictEqual(AC.stationView({ details: { antennaType: 'omni' }, stale: false, approvedAt: 1 }), null);
  assert.strictEqual(AC.stationView({ details: null }), null);
  assert.strictEqual(AC.stationView(null), null);
  assert.strictEqual(AC.stationView({ details: {} }), null);
  assert.strictEqual(AC.stationView({ details: { antennaType: 'dish' } }), null, 'an unknown value is not shown');
  eq(AC.stationView({ details: { gainDbi: -2.5 } }).rows, [['Gain', '-2.5 dBi']]);
});

test('stationView and stationPrefill never carry contactAdmin, even if a response leaked it', () => {
  const leaked = { details: { antennaType: 'omni', contactAdmin: 'ZZADMIN9', contactPublic: 'ZZPUBLIC7' }, approvedAt: 1 };
  const v = JSON.stringify(AC.stationView(leaked));
  assert.ok(!/ZZADMIN9/.test(v) && /ZZPUBLIC7/.test(v), v);
  const pf = AC.stationPrefill(leaked);
  assert.ok(!/ZZADMIN9/.test(JSON.stringify(pf)) && !('contactAdmin' in pf), JSON.stringify(pf));
  eq(AC.stationPrefill(leaked), { antennaType: 'omni', gainRaw: '', power: {}, installation: '', contactPublic: 'ZZPUBLIC7' });
  // The public contact empty or missing: no `contactPublic || contactAdmin`
  // fallback, in the block or in the pre-fill (which would then publish the
  // admin contact). Review PR#908 F2.
  for (const pub of [{ contactPublic: '' }, {}]) {
    const r = { details: Object.assign({ antennaType: 'omni', contactAdmin: 'ZZADMIN9' }, pub), approvedAt: 1 };
    const sv = AC.stationView(r);
    eq(sv.rows, [['Antenna', 'Omni']]);
    assert.ok(!/ZZADMIN9/.test(JSON.stringify(sv)), JSON.stringify(sv));
    eq(AC.stationPrefill(r), { antennaType: 'omni', gainRaw: '', power: {}, installation: '', contactPublic: '' });
    // Only the admin contact: nothing to show at all.
    assert.strictEqual(AC.stationView({ details: Object.assign({ contactAdmin: 'ZZADMIN9' }, pub) }), null);
  }
  eq(AC.stationPrefill({ details: { gainDbi: 6, power: ['mains', 'wind'] } }).gainRaw, '6.0');
  eq(AC.stationPrefill({ details: { power: ['mains', 'wind'] } }).power, { mains: true });
  eq(AC.stationPrefill(null), { antennaType: '', gainRaw: '', power: {}, installation: '', contactPublic: '' });
});

test('storedInfo keeps the code, parts and height only: no detail, no contact (U4)', () => {
  const info = AC.storedInfo({ code: CODE, suggestedName: 'Roof ' + CODE, expiresAt: 5, budget: 23, heightM: 12.5, originalName: 'Roof',
    parts: { height: 'set', details: 'set' }, details: { contactAdmin: 'ZZADMIN9', contactPublic: 'ZZPUBLIC7', antennaType: 'omni' },
    contactAdmin: 'ZZADMIN9', clear: false });
  eq(info, { code: CODE, suggestedName: 'Roof ' + CODE, expiresAt: 5, budget: 23, heightM: 12.5, parts: { height: 'set', details: 'set' }, originalName: 'Roof' });
  assert.strictEqual(AC.storedInfo({ code: CODE, heightM: 3, parts: { height: '', details: 'set' } }).heightM, null, 'a height kept for a claim without one');
});

test('adminParts: per-part lines from the ops, the replaced marker, contactAdmin from proven on', () => {
  const base = { heightOp: 'set', heightM: 12.5, detailsOp: 'set', contactAdmin: 'ZZADMIN9',
    details: { antennaType: 'omni', gainDbi: 6, power: ['solar', 'battery'], installation: 'mast', contactPublic: 'OZ1ABC' } };
  const p = AC.adminParts(Object.assign({ status: 'proven' }, base));
  eq(p.lines.map((l) => [l.part, l.label, l.text, l.replaced]), [
    ['height', 'Height', '12.5 m', false],
    ['details', 'Station details', 'Omni · 6.0 dBi · Solar + Battery · Mast · public contact OZ1ABC', false]]);
  assert.strictEqual(p.contactAdmin, 'ZZADMIN9');
  assert.strictEqual(AC.adminParts(Object.assign({ status: 'approved', heightInForce: true, detailsInForce: true }, base)).contactAdmin, 'ZZADMIN9');
  for (const st of ['pending', 'rejected', 'revoked', 'expired', 'superseded', undefined]) {
    assert.strictEqual(AC.adminParts(Object.assign({ status: st }, base)).contactAdmin, '', 'contactAdmin listed for ' + st);
  }
  // Partly replaced: the height is still in force, the details are not.
  const part = AC.adminParts(Object.assign({ status: 'approved', heightInForce: true, detailsInForce: false }, base));
  eq(part.lines.map((l) => l.replaced), [false, true]);
  // A details-only claim has no height line at all, though heightM is null.
  eq(AC.adminParts({ status: 'proven', heightM: null, detailsOp: 'set', details: { contactPublic: 'x' } }).lines.map((l) => l.part), ['details']);
  eq(AC.adminParts({ status: 'proven', heightM: null, detailsOp: 'clear' }).lines.map((l) => [l.label, l.text]), [['Station details', 'remove']]);
  eq(AC.adminParts({ status: 'approved', heightM: null, heightOp: 'clear', heightInForce: true }).lines.map((l) => [l.label, l.text]), [['Height', 'remove']]);
  eq(AC.adminParts({ status: 'proven', detailsOp: 'set', details: {} }).lines[0].text, 'administrator contact only');
  // No op, no line: nothing is inferred from heightM.
  eq(AC.adminParts({ status: 'proven', heightM: null }).lines, []);
});

test('reviewReasons: the one needsReview flag explained by height or gain', () => {
  eq(AC.reviewReasons({ needsReview: false, heightOp: 'set' }), []);
  eq(AC.reviewReasons({ needsReview: true, heightOp: 'set' }), ['Check: far from the surface estimate']);
  // Both reasons for a combined claim with an unusual gain: the one flag may
  // also come from the height (review PR#908 F3).
  eq(AC.reviewReasons({ needsReview: true, heightOp: 'set', detailsOp: 'set', details: { gainDbi: 18 } }),
    ['Check: gain outside the usual 0–15 dBi', 'Check: the height may also be far from the surface estimate']);
  eq(AC.reviewReasons({ needsReview: true, heightOp: 'clear', detailsOp: 'set', details: { gainDbi: 18 } }), ['Check: gain outside the usual 0–15 dBi']);
  eq(AC.reviewReasons({ needsReview: true, detailsOp: 'set', details: { gainDbi: -1 } }), ['Check: gain outside the usual 0–15 dBi']);
  eq(AC.reviewReasons({ needsReview: true, detailsOp: 'set', details: { gainDbi: 6 } }), ['Check: flagged for review']);
});

test('the 409 names the overlapping part of the claim', () => {
  const err = { status: 409, message: 'a newer claim for this node is already approved' };
  assert.ok(/sets this node’s station details/.test(AC.decisionError(err, { detailsOp: 'set' })));
  assert.ok(/sets this node’s height or station details/.test(AC.decisionError(err, { heightOp: 'set', detailsOp: 'clear' })));
  assert.ok(/sets this node’s height \(/.test(AC.decisionError(err, { heightOp: 'clear', heightM: null })));
});

// renderAdminRow in a minimal DOM whose innerHTML throws: every string of a
// claim reaches the row as text (U2), and contactAdmin only from proven on.
function fakeDom() {
  function Node(tag) {
    this.tagName = tag.toUpperCase(); this.children = []; this.attrs = {}; this._text = ''; this.className = ''; this.hidden = false;
    this.classList = { add() {}, toggle() {} };
  }
  Node.prototype.appendChild = function (c) { this.children.push(c); return c; };
  Node.prototype.setAttribute = function (k, v) { this.attrs[k] = String(v); };
  Node.prototype.getAttribute = function (k) { return k in this.attrs ? this.attrs[k] : null; };
  Node.prototype.addEventListener = function () {};
  Object.defineProperty(Node.prototype, 'textContent', {
    get() { return this.children.length ? this.children.map((c) => c.textContent).join('') : this._text; },
    set(v) { this.children = []; this._text = String(v); },
  });
  Object.defineProperty(Node.prototype, 'innerHTML', { get() { throw new Error('innerHTML read'); }, set() { throw new Error('innerHTML set'); } });
  return {
    createElement: (t) => new Node(t),
    createTextNode: (t) => { const n = new Node('#text'); n._text = String(t); return n; },
  };
}
function loadWithDom() {
  const ctx = { console, Promise, Math, JSON, Object, Array, String, Number, encodeURIComponent, URLSearchParams, document: fakeDom() };
  ctx.window = ctx; ctx.setTimeout = setTimeout; ctx.clearTimeout = clearTimeout;
  vm.createContext(ctx);
  for (const f of [HEIGHT_SRC_PATH, ADMIN_KEY_PATH, SRC_PATH]) vm.runInContext(fs.readFileSync(f, 'utf8'), ctx, { filename: f });
  return ctx.AntennaClaims;
}
test('renderAdminRow: markup in both contacts stays text; contactAdmin only for proven and approved', () => {
  const ACD = loadWithDom();
  const XSS = '<img src=x onerror=alert(1)>';
  const claim = (status) => ({ id: '1111111111111111', pubkey: 'a'.repeat(64), name: XSS, role: 'repeater', status, createdAt: 1,
    heightOp: 'set', heightM: 10, detailsOp: 'set', details: { antennaType: 'omni', contactPublic: XSS + 'P' }, contactAdmin: XSS + 'A' });
  const text = (li) => li.textContent;
  const proven = text(ACD.renderAdminRow(claim('proven')));
  assert.ok(proven.includes('Contact (admin only): ' + XSS + 'A'), proven);
  assert.ok(proven.includes('public contact ' + XSS + 'P'), proven);
  assert.ok(proven.includes('Height: 10 m') && proven.includes('Station details: Omni'), proven);
  assert.ok(!text(ACD.renderAdminRow(claim('pending'))).includes(XSS + 'A'), 'contactAdmin shown for a pending claim');
  assert.ok(text(ACD.renderAdminRow(claim('approved'))).includes(XSS + 'A'), 'contactAdmin missing for an approved claim');
  const detailsOnly = text(ACD.renderAdminRow({ id: '2222222222222222', pubkey: 'a'.repeat(64), name: 'x', status: 'proven', heightM: null, detailsOp: 'set', details: { installation: 'roof' } }));
  assert.ok(!/Height/.test(detailsOnly) && /Station details: Roof/.test(detailsOnly), detailsOnly);
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
