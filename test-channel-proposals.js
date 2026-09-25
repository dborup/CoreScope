#!/usr/bin/env node
/* Unit tests for public/channel-proposals.js — the real production file,
 * loaded into a vm context (no copies of the code under test). */
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const SRC_PATH = path.join(__dirname, 'public', 'channel-proposals.js');
const SRC = fs.readFileSync(SRC_PATH, 'utf8');

function load() {
  const ctx = { console, Promise, Map, Set, Math, JSON, Object, Array, String, encodeURIComponent };
  ctx.window = ctx;
  ctx.setTimeout = setTimeout;
  ctx.clearTimeout = clearTimeout;
  vm.createContext(ctx);
  vm.runInContext(SRC, ctx, { filename: SRC_PATH });
  return ctx.ChannelProposals;
}

const CP = load();
let passed = 0;
const tests = [];
function test(name, fn) { tests.push({ name, fn }); }

// ── normalizeName mirrors internal/channelregistry.NormalizeName ─────────
test('normalizeName accepts and preserves case', () => {
  const ok = {
    '#MeshCore': '#MeshCore',
    'MeshCore': '#MeshCore',
    '  #wardriving  ': '#wardriving',
    '#Test': '#Test',
    '#test': '#test',
    '#København': '#København',
    '#my channel': '#my channel',
    '##double': '##double',
    '#\u{1F468}\u200D\u{1F469}\u200D\u{1F467}': '#\u{1F468}\u200D\u{1F469}\u200D\u{1F467}',
  };
  ok['#' + 'a'.repeat(30)] = '#' + 'a'.repeat(30); // exactly 31 bytes
  for (const [input, want] of Object.entries(ok)) {
    const r = CP.normalizeName(input);
    assert.strictEqual(r.error, undefined, `${JSON.stringify(input)}: ${r.error}`);
    assert.strictEqual(r.name, want, JSON.stringify(input));
  }
});

test('normalizeName rejects what the firmware cannot hold or would mislead', () => {
  const bad = [
    '', '   ', '#',
    '#' + 'a'.repeat(31),            // 32 bytes
    '#' + 'æ'.repeat(16),       // 16 characters but 33 bytes
    '#0123456789abcdef0123456789abcdef', // a pasted PSK
    '#bad\u0000name', '#tab\tname', '#rtl\u202Eevil', '#iso\u2066late',
    '# leading', '#\u00A0leading-nbsp',
    '#lone\uD800surrogate',
  ];
  for (const input of bad) {
    const r = CP.normalizeName(input);
    assert.ok(r.error && !r.name, `${JSON.stringify(input)} should be rejected, got ${JSON.stringify(r)}`);
  }
});

// ── mergeApprovedChannels ────────────────────────────────────────────────
function deepFreeze(o) {
  Object.freeze(o);
  for (const v of Object.values(o)) if (v && typeof v === 'object' && !Object.isFrozen(v)) deepFreeze(v);
  return o;
}

test('merge marks existing channels shared, appends quiet ones, never mutates input', () => {
  const channels = deepFreeze([
    { hash: '#busy', name: '#busy', messageCount: 5, lastActivityMs: 10 },
    { hash: '#other', name: '#other', messageCount: 1, lastActivityMs: 5 },
  ]);
  const approved = deepFreeze([
    { name: '#busy', hash: '#busy' },
    { name: '#Quiet', hash: '#Quiet' },
    { name: '#Quiet', hash: '#Quiet' }, // duplicate from the server is ignored
  ]);
  const before = JSON.stringify({ channels, approved });
  const out = CP.mergeApprovedChannels(channels, approved);
  assert.strictEqual(JSON.stringify({ channels, approved }), before, 'inputs changed');
  assert.notStrictEqual(out, channels, 'must return a new array');
  assert.strictEqual(out.length, 3);
  assert.strictEqual(out[0].shared, true);
  assert.strictEqual(out[0].messageCount, 5, 'existing data kept');
  assert.notStrictEqual(out[0], channels[0], 'shared entry is a copy');
  assert.strictEqual(out[1], channels[1], 'untouched entries are reused');
  assert.strictEqual(out[1].shared, undefined);
  assert.deepStrictEqual(JSON.parse(JSON.stringify(out[2])), {
    hash: '#Quiet', name: '#Quiet', messageCount: 0, lastActivity: null,
    lastActivityMs: 0, lastMessage: null, lastSender: null, shared: true,
  });
});

test('merge is byte-exact: #Test and #test are different channels', () => {
  const out = CP.mergeApprovedChannels([{ hash: '#test', name: '#test' }], [{ name: '#Test', hash: '#Test' }]);
  assert.strictEqual(out.length, 2);
  assert.strictEqual(out[0].shared, undefined);
  assert.strictEqual(out[1].hash, '#Test');
});

test('merge handles missing or empty approvedChannels', () => {
  const channels = [{ hash: '#a', name: '#a' }];
  for (const approved of [undefined, null, [], {}, [null, { name: 'x' }]]) {
    const out = CP.mergeApprovedChannels(channels, approved);
    assert.strictEqual(out.length, 1);
    assert.strictEqual(out[0], channels[0]);
  }
  const empty = CP.mergeApprovedChannels(undefined, undefined);
  assert.ok(Array.isArray(empty) && empty.length === 0, 'no input yields an empty list');
});

test('merge is O(n+m): 30K channels x 128 approved in well under 50ms', () => {
  const channels = [];
  for (let i = 0; i < 30000; i++) channels.push({ hash: '#c' + i, name: '#c' + i, messageCount: i });
  const approved = [];
  for (let i = 0; i < 128; i++) approved.push({ name: '#c' + (i * 200), hash: '#c' + (i * 200) });
  for (let i = 0; i < 128; i++) approved.push({ name: '#new' + i, hash: '#new' + i });
  CP.mergeApprovedChannels(channels, approved); // warm up
  const t0 = process.hrtime.bigint();
  const out = CP.mergeApprovedChannels(channels, approved);
  const ms = Number(process.hrtime.bigint() - t0) / 1e6;
  assert.strictEqual(out.length, 30128);
  assert.strictEqual(out.filter((c) => c.shared).length, 256);
  assert.ok(ms < 50, `merge took ${ms.toFixed(1)}ms`);
  console.log(`    merge 30000 + 256 approved: ${ms.toFixed(2)}ms`);
});

// ── polling ──────────────────────────────────────────────────────────────
test('pollDelay backs off and is capped at 10s', () => {
  const seq = [0, 1, 2, 3, 4, 5, 6, 7, 20].map(CP.pollDelay);
  assert.deepStrictEqual(seq, [1000, 1500, 2250, 3375, 5063, 7594, 10000, 10000, 10000]);
});

function fakeTimers() {
  const pending = new Map();
  let next = 1;
  return {
    setTimeout(fn, ms) { const id = next++; pending.set(id, { fn, ms }); return id; },
    clearTimeout(id) { pending.delete(id); },
    delays: [],
    async fireNext() {
      const [id, t] = pending.entries().next().value || [];
      if (!id) return false;
      pending.delete(id);
      this.delays.push(t.ms);
      t.fn();
      await new Promise((r) => setImmediate(r));
      return true;
    },
    size() { return pending.size; },
  };
}

test('poller polls only while queued, then stops', async () => {
  const timers = fakeTimers();
  const answers = [{ status: 'queued' }, { status: 'queued' }, { status: 'pending', proposal: { name: '#x' } }];
  const updates = [];
  const p = CP.createPoller({
    fetchStatus: (id) => { assert.strictEqual(id, 'req1'); return Promise.resolve(answers.shift()); },
    onUpdate: (st) => updates.push(st.status),
    setTimeout: timers.setTimeout, clearTimeout: timers.clearTimeout,
  });
  p.start('req1');
  while (await timers.fireNext()) { /* drain */ }
  assert.deepStrictEqual(updates, ['queued', 'queued', 'pending']);
  assert.deepStrictEqual(timers.delays, [1000, 1500, 2250]);
  assert.strictEqual(p.isActive(), false);
});

test('starting a new request cancels the old one; cancel stops polling', async () => {
  const timers = fakeTimers();
  const seen = [];
  const p = CP.createPoller({
    fetchStatus: (id) => Promise.resolve({ status: 'queued', id }),
    onUpdate: (st) => seen.push(st.id),
    setTimeout: timers.setTimeout, clearTimeout: timers.clearTimeout,
  });
  p.start('old');
  p.start('new');
  assert.strictEqual(timers.size(), 1, 'only the active request is scheduled');
  await timers.fireNext();
  assert.deepStrictEqual(seen, ['new']);
  p.cancel();
  assert.strictEqual(timers.size(), 0);
  assert.strictEqual(p.isActive(), false);
});

test('a result arriving after cancel is ignored', async () => {
  const timers = fakeTimers();
  let resolveFetch;
  const seen = [];
  const p = CP.createPoller({
    fetchStatus: () => new Promise((r) => { resolveFetch = r; }),
    onUpdate: (st) => seen.push(st.status),
    setTimeout: timers.setTimeout, clearTimeout: timers.clearTimeout,
  });
  p.start('r');
  await timers.fireNext();
  p.cancel(); // e.g. the user navigated away
  resolveFetch({ status: 'pending' });
  await new Promise((r) => setImmediate(r));
  assert.deepStrictEqual(seen, []);
});

test('404 ends polling with an error; transient errors back off; attempts are bounded', async () => {
  let timers = fakeTimers();
  let seen = [];
  const notFound = Object.assign(new Error('gone'), { status: 404 });
  let p = CP.createPoller({
    fetchStatus: () => Promise.reject(notFound),
    onUpdate: (st) => seen.push(st.status),
    setTimeout: timers.setTimeout, clearTimeout: timers.clearTimeout,
  });
  p.start('r');
  while (await timers.fireNext()) { /* drain */ }
  assert.deepStrictEqual(seen, ['error']);

  timers = fakeTimers();
  seen = [];
  let gaveUp = false;
  p = CP.createPoller({
    fetchStatus: () => Promise.reject(new Error('offline')),
    onUpdate: (st) => seen.push(st.status),
    onGiveUp: () => { gaveUp = true; },
    maxAttempts: 4,
    setTimeout: timers.setTimeout, clearTimeout: timers.clearTimeout,
  });
  p.start('r');
  while (await timers.fireNext()) { /* drain */ }
  assert.deepStrictEqual(seen, []);
  assert.deepStrictEqual(timers.delays, [1000, 1500, 2250, 3375]);
  assert.strictEqual(gaveUp, true);
});

// ── security properties of the source ────────────────────────────────────
test('the admin key is never persisted or put in a URL', () => {
  const code = SRC.replace(/\/\*[\s\S]*?\*\//g, '').replace(/\/\/.*$/gm, '');
  assert.ok(!/localStorage|sessionStorage|indexedDB|document\.cookie/.test(code), 'storage API used');
  assert.ok(/'X-API-Key'\] = adminKey/.test(code), 'key must travel in the X-API-Key header');
  assert.ok(!/[?&](key|apiKey|api_key)=/.test(code), 'key-like query parameter found');
});

(async () => {
  for (const t of tests) {
    try {
      await t.fn();
      passed++;
      console.log('  ✓ ' + t.name);
    } catch (e) {
      console.error('  ✗ ' + t.name + '\n    ' + (e && e.stack || e));
      process.exitCode = 1;
    }
  }
  console.log(`test-channel-proposals.js: ${passed} passed, ${tests.length - passed} failed`);
})();
