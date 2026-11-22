/* test-883-version-autoreload.js — kiosk auto-reload on a new commit (#883 M3).
 *
 * Owner decision 5 (#883 M0): on a new server commit, kiosk screens reload
 * by themselves after a random 30–120 s delay (so many screens do not hit a
 * freshly restarted server at once); normal pages keep the #614 banner.
 *
 * Loads the real public/version-notice.js in a vm with a fake health reader
 * (window.CoreHealth), a fake clock (Date.now + setTimeout) and a fake
 * Math.random, and drives decide() through it: the baseline at load, then
 * readings on the WS-open event.
 */
'use strict';
const vm = require('vm');
const fs = require('fs');
const assert = require('assert');

console.log('--- test-883-version-autoreload.js ---');
let passed = 0, failed = 0;
const pending = [];
function test(name, fn) {
  pending.push(async () => {
    try { await fn(); passed++; console.log('  ✅ ' + name); }
    catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
  });
}

const SRC = fs.readFileSync(__dirname + '/public/version-notice.js', 'utf8');
const H = (commit) => ({ version: 'v1', commit, buildTime: 't' });

function load(healthSeq, random) {
  let now = 1000000, seq = 0, reloads = 0;
  const timers = new Map();
  const wl = {}, dl = {};
  function El(tag) { this.tagName = tag; this.children = []; this.attrs = {}; this.parentNode = null; this.listeners = {}; }
  El.prototype = {
    appendChild(c) { c.parentNode = this; this.children.push(c); return c; },
    remove() { if (this.parentNode) this.parentNode.children = this.parentNode.children.filter((x) => x !== this); this.parentNode = null; },
    setAttribute(k, v) { this.attrs[k] = String(v); },
    addEventListener(t, f) { (this.listeners[t] = this.listeners[t] || []).push(f); },
    click() { (this.listeners.click || []).forEach((f) => f({})); },
  };
  const body = new El('body');
  const doc = { body, hidden: false, createElement: (t) => new El(t), addEventListener(t, f) { (dl[t] = dl[t] || []).push(f); } };
  const reads = [];
  const read = () => { const h = healthSeq.length > 1 ? healthSeq.shift() : healthSeq[0]; reads.push(h); return Promise.resolve(h); };
  const win = {
    addEventListener(t, f) { (wl[t] = wl[t] || []).push(f); },
    location: { reload() { reloads++; } },
    CoreHealth: { read, readOnce: read },
  };
  const fakeMath = Object.create(Math);
  fakeMath.random = () => random;
  const ctx = {
    window: win, document: doc, console, Promise, JSON, Object, String, Number, Math: fakeMath,
    Date: { now: () => now },
    setTimeout: (f, ms) => { const id = ++seq; timers.set(id, { at: now + ms, f, ms }); return id; },
    clearTimeout: (id) => { timers.delete(id); },
    setInterval: () => 0, clearInterval() {},
  };
  vm.createContext(ctx);
  vm.runInContext(SRC, ctx);
  return {
    VN: win.VersionNotice,
    reloads: () => reloads,
    timers: () => [...timers.values()],
    banner: () => body.children.find((c) => 'data-version-notice' in c.attrs) || null,
    // Advance the fake clock, firing due timers in order.
    advance(ms) {
      const end = now + ms;
      for (;;) {
        let next = null;
        for (const [id, t] of timers) if (t.at <= end && (!next || t.at < next[1].at)) next = [id, t];
        if (!next) break;
        timers.delete(next[0]); now = next[1].at; next[1].f();
      }
      now = end;
    },
    wsOpen() { (wl['corescope:ws-open'] || []).forEach((f) => f({})); },
  };
}
const flush = async () => { for (let i = 0; i < 6; i++) await new Promise((r) => setImmediate(r)); };
// A reading on the next WS open, past the 30 s check throttle.
async function newReading(b) { b.advance(60000); b.wsOpen(); await flush(); }

test('reloadDelayMs maps random [0, 1) onto 30–120 s', () => {
  const VN = load([H('aaa')], 0).VN;
  assert.strictEqual(VN.RELOAD_MIN_MS, 30000);
  assert.strictEqual(VN.RELOAD_MAX_MS, 120000);
  assert.strictEqual(VN.reloadDelayMs(0), 30000);
  assert.strictEqual(VN.reloadDelayMs(0.5), 75000);
  assert(VN.reloadDelayMs(0.999999) <= 120000 && VN.reloadDelayMs(0.999999) > 119000);
  assert.strictEqual(VN.reloadDelayMs(NaN), 30000, 'a bad random falls back to the floor');
  assert(VN.reloadDelayMs(5) <= 120000, 'never above the ceiling');
});

test('kiosk (autoReload on): a new commit reloads once after the jittered delay, no banner', async () => {
  const b = load([H('aaa'), H('bbb')], 0.5);
  await flush();
  b.VN.setAutoReload(true);
  await newReading(b);
  assert.strictEqual(b.banner(), null, 'kiosk shows no banner');
  assert.strictEqual(b.reloads(), 0, 'not at once');
  b.advance(74999);
  assert.strictEqual(b.reloads(), 0, 'not before 75 s at random 0.5');
  b.advance(1);
  assert.strictEqual(b.reloads(), 1, 'reloaded at 75 s');
  b.advance(600000);
  assert.strictEqual(b.reloads(), 1, 'once');
});

test('kiosk: the delay follows the random draw (0 → 30 s, just below 1 → just below 120 s)', async () => {
  for (const [r, lo, hi] of [[0, 30000, 30000], [0.9999, 119000, 120000]]) {
    const b = load([H('aaa'), H('bbb')], r);
    await flush();
    b.VN.setAutoReload(true);
    await newReading(b);
    b.advance(lo - 1);
    assert.strictEqual(b.reloads(), 0, 'random ' + r + ': early');
    b.advance(hi - lo + 1);
    assert.strictEqual(b.reloads(), 1, 'random ' + r + ': by ' + hi + ' ms');
  }
});

test('kiosk: the same commit (a plain restart) and unknown or empty commits never reload', async () => {
  const b = load([H('aaa'), H('aaa'), H('unknown'), H(''), { version: 'v1' }], 0);
  await flush();
  b.VN.setAutoReload(true);
  for (let i = 0; i < 4; i++) await newReading(b);
  b.advance(600000);
  assert.strictEqual(b.reloads(), 0);
  assert.strictEqual(b.banner(), null);
});

test('normal page (autoReload off): the banner, never a reload', async () => {
  const b = load([H('aaa'), H('bbb')], 0);
  await flush();
  await newReading(b);
  assert(b.banner(), 'banner shown');
  b.advance(600000);
  assert.strictEqual(b.reloads(), 0);
});

test('leaving kiosk before the reload cancels it and shows the banner instead', async () => {
  const b = load([H('aaa'), H('bbb')], 0.5);
  await flush();
  b.VN.setAutoReload(true);
  await newReading(b);
  b.advance(10000);
  b.VN.setAutoReload(false);
  assert(b.banner(), 'banner once kiosk is gone');
  b.advance(600000);
  assert.strictEqual(b.reloads(), 0, 'cancelled');
  assert.strictEqual(b.timers().length, 0, 'no timer left');
});

test('entering kiosk after a new commit was seen schedules the reload (one timer, banner gone)', async () => {
  const b = load([H('aaa'), H('bbb')], 0);
  await flush();
  await newReading(b);
  assert(b.banner());
  b.VN.setAutoReload(true);
  b.VN.setAutoReload(true);
  assert.strictEqual(b.banner(), null, 'banner hidden in kiosk');
  assert.strictEqual(b.timers().length, 1, 'one reload timer');
  b.advance(30000);
  assert.strictEqual(b.reloads(), 1);
});

test('a later commit while a reload is pending keeps the one timer', async () => {
  const b = load([H('aaa'), H('bbb'), H('ccc')], 0.5);
  await flush();
  b.VN.setAutoReload(true);
  await newReading(b);
  await newReading(b);
  assert.strictEqual(b.timers().length, 1);
  b.advance(75000);
  assert.strictEqual(b.reloads(), 1);
});

(async () => {
  for (const t of pending) await t();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
