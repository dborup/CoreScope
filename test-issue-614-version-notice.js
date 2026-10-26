/* test-issue-614-version-notice.js — "new version available" notice (#614).
 *
 * Loads the real public/version-notice.js (and public/nav-drawer.js for the
 * shared /api/health reader) in vm contexts. Checks the pure decision
 * function (first load, same commit, new commit once, dismiss, failed or
 * commit-less health), the check throttle, and the wiring: baseline read at
 * load, checks on visibilitychange / WS open / interval, never an
 * auto-reload, accessible banner built without HTML strings.
 */
'use strict';
const vm = require('vm');
const fs = require('fs');
const assert = require('assert');

console.log('--- test-issue-614-version-notice.js ---');
let passed = 0, failed = 0;
const pending = [];
function test(name, fn) {
  pending.push(async () => {
    try { await fn(); passed++; console.log('  ✅ ' + name); }
    catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
  });
}

const SRC = fs.readFileSync(__dirname + '/public/version-notice.js', 'utf8');
const DRAWER = fs.readFileSync(__dirname + '/public/nav-drawer.js', 'utf8');

// ── pure logic ────────────────────────────────────────────────────────────
function pure() {
  const ctx = { window: {}, console, Date, Math, Object, String };
  vm.createContext(ctx);
  vm.runInContext(SRC, ctx);
  return ctx.window.VersionNotice;
}
const VN = pure();
const H = (commit) => ({ version: 'v1', commit, buildTime: 't' });
const fresh = () => VN.initialState();

test('first load records the baseline and does not notify', () => {
  const r = VN.decide(fresh(), H('aaa'));
  assert.strictEqual(r.notify, false);
  assert.strictEqual(r.state.baseline, 'aaa');
});
test('the same commit does not notify', () => {
  const s = VN.decide(fresh(), H('aaa')).state;
  assert.strictEqual(VN.decide(s, H('aaa')).notify, false);
});
test('a new commit notifies, and only once per commit', () => {
  let s = VN.decide(fresh(), H('aaa')).state;
  const r1 = VN.decide(s, H('bbb'));
  assert.strictEqual(r1.notify, true);
  assert.strictEqual(r1.commit, 'bbb');
  const r2 = VN.decide(r1.state, H('bbb'));
  assert.strictEqual(r2.notify, false);
});
test('after a notice the same commit stays quiet but a later commit notifies again', () => {
  let s = VN.decide(fresh(), H('aaa')).state;
  s = VN.decide(s, H('bbb')).state;
  assert.strictEqual(VN.decide(s, H('bbb')).notify, false);
  assert.strictEqual(VN.decide(s, H('ccc')).notify, true);
});
for (const [what, h] of Object.entries({
  'null (failed or 5xx health)': null,
  'undefined': undefined,
  'an empty object': {},
  'an unknown commit': { commit: 'unknown' },
  'a blank commit': { commit: '  ' },
  'a non-string commit': { commit: 42 },
})) {
  test('health ' + what + ' never notifies and keeps the baseline', () => {
    const s = VN.decide(fresh(), H('aaa')).state;
    const r = VN.decide(s, h);
    assert.strictEqual(r.notify, false);
    assert.strictEqual(r.state.baseline, 'aaa');
  });
  test('health ' + what + ' as the first reading leaves the baseline unset', () => {
    const r = VN.decide(fresh(), h);
    assert.strictEqual(r.notify, false);
    assert.strictEqual(r.state.baseline, null);
  });
}
test('a restart window (failed health) between two readings still yields one notice', () => {
  let s = VN.decide(fresh(), H('aaa')).state;
  s = VN.decide(s, null).state;
  const r = VN.decide(s, H('bbb'));
  assert.strictEqual(r.notify, true);
});
test('shouldCheck throttles within the minimum gap and allows after it', () => {
  assert.strictEqual(VN.shouldCheck(1000, 0, 30000), true, 'never checked');
  assert.strictEqual(VN.shouldCheck(10000, 1000, 30000), false);
  assert.strictEqual(VN.shouldCheck(31000, 1000, 30000), true);
  assert.strictEqual(VN.shouldCheck(5, NaN, 30000), true);
});

// ── wiring ────────────────────────────────────────────────────────────────
function makeDoc() {
  const listeners = {};
  function El(tag) {
    this.tagName = String(tag).toUpperCase();
    this.children = []; this.attrs = {}; this.parentNode = null;
    this.className = ''; this.textContent = ''; this.hidden = false;
    this.listeners = {};
  }
  El.prototype = {
    appendChild(c) { c.parentNode = this; this.children.push(c); return c; },
    remove() { if (this.parentNode) this.parentNode.children = this.parentNode.children.filter((x) => x !== this); this.parentNode = null; },
    setAttribute(k, v) { this.attrs[k] = String(v); },
    getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; },
    addEventListener(t, f) { (this.listeners[t] = this.listeners[t] || []).push(f); },
    click() { (this.listeners.click || []).forEach((f) => f({})); },
    querySelector(sel) {
      const m = sel.match(/^\[([\w-]+)\]$/);
      const walk = (n) => { for (const c of n.children) { if (m && m[1] in c.attrs) return c; const r = walk(c); if (r) return r; } return null; };
      return walk(this);
    },
  };
  let htmlWrites = 0;
  Object.defineProperty(El.prototype, 'innerHTML', { set() { htmlWrites++; }, get() { return ''; } });
  const body = new El('body');
  const doc = {
    body, hidden: false,
    createElement: (t) => new El(t),
    addEventListener(t, f) { (listeners[t] = listeners[t] || []).push(f); },
    querySelector: (s) => body.querySelector(s),
    fire(t) { (listeners[t] || []).forEach((f) => f({})); },
  };
  return { doc, htmlWrites: () => htmlWrites };
}

function load(healthSeq, extra) {
  extra = extra || {};
  const { doc, htmlWrites } = makeDoc();
  const calls = [];
  const wl = {};
  let now = 1000000;
  const timers = [];
  let reloads = 0;
  const win = {
    addEventListener(t, f) { (wl[t] = wl[t] || []).push(f); },
    location: { reload() { reloads++; } },
  };
  const ctx = {
    window: win, document: doc, console, Date: { now: () => now }, Promise, JSON, Object, String, Math, Number,
    setInterval: (f, ms) => { timers.push({ f, ms }); return timers.length; },
    clearInterval() {},
    fetch: (url) => {
      calls.push(url);
      const h = healthSeq.length > 1 ? healthSeq.shift() : healthSeq[0];
      if (h === 'reject') return Promise.reject(new Error('offline'));
      if (h === '5xx') return Promise.resolve({ ok: false, status: 503, json: () => Promise.resolve({}) });
      return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(h) });
    },
  };
  win.fetch = ctx.fetch;
  vm.createContext(ctx);
  // The drawer's DOM wiring needs a fuller DOM than this fake provides; only
  // its health reader (assigned before that wiring) is under test here, so a
  // later wiring error is expected and ignored.
  ctx.requestAnimationFrame = (f) => { f(); return 1; };
  ctx.performance = { now: () => 0 };
  win.matchMedia = () => ({ matches: false, addEventListener() {} });
  win.innerWidth = 1280;
  try { vm.runInContext(DRAWER, ctx); } catch (e) { /* drawer UI wiring, not under test */ }
  vm.runInContext(SRC, ctx);
  return {
    calls, doc, htmlWrites, timers, win,
    reloads: () => reloads,
    advance(ms) { now += ms; },
    fireWin(t) { (wl[t] || []).forEach((f) => f({})); },
    banner: () => doc.body.querySelector('[data-version-notice]'),
  };
}
const flush = async () => { for (let i = 0; i < 6; i++) await new Promise((r) => setTimeout(r, 0)); };

test('nav-drawer.js exposes the shared health reader and the notice uses it', async () => {
  const b = load([H('aaa')]);
  assert.strictEqual(typeof b.win.CoreHealth.read, 'function');
  await flush();
  assert.deepStrictEqual(b.calls.slice(), ['/api/health'], 'baseline read at load');
});
test('page load reads the baseline once and shows nothing', async () => {
  const b = load([H('aaa')]);
  await flush();
  assert.strictEqual(b.calls.length, 1);
  assert.strictEqual(b.banner(), null);
});
test('visibilitychange to visible with a new commit shows an accessible banner', async () => {
  const b = load([H('aaa'), H('bbb')]);
  await flush();
  const writesBefore = b.htmlWrites();
  b.advance(60000);
  b.doc.hidden = false;
  b.doc.fire('visibilitychange');
  await flush();
  const el = b.banner();
  assert(el, 'no banner');
  assert.strictEqual(el.getAttribute('role'), 'status');
  assert(/new version/i.test(el.textContent + el.children.map((c) => c.textContent).join(' ')));
  assert.strictEqual(b.reloads(), 0, 'must never auto-reload');
  assert.strictEqual(b.htmlWrites(), writesBefore, 'no HTML strings');
});
test('visibilitychange while hidden does not fetch', async () => {
  const b = load([H('aaa'), H('bbb')]);
  await flush();
  b.advance(60000);
  b.doc.hidden = true;
  b.doc.fire('visibilitychange');
  await flush();
  assert.strictEqual(b.calls.length, 1);
});
test('a WebSocket open event triggers a check; checks inside the throttle gap are skipped', async () => {
  const b = load([H('aaa'), H('bbb')]);
  await flush();
  b.fireWin('corescope:ws-open');
  await flush();
  assert.strictEqual(b.calls.length, 1, 'initial socket open right after load is throttled');
  b.advance(60000);
  b.fireWin('corescope:ws-open');
  await flush();
  assert.strictEqual(b.calls.length, 2);
  assert(b.banner());
});
test('the periodic check runs at a multi-minute interval and skips hidden tabs', async () => {
  const b = load([H('aaa'), H('bbb')]);
  await flush();
  assert.strictEqual(b.timers.length, 1);
  assert(b.timers[0].ms >= 120000, 'interval too short: ' + b.timers[0].ms);
  b.advance(b.timers[0].ms);
  b.doc.hidden = true;
  b.timers[0].f();
  await flush();
  assert.strictEqual(b.calls.length, 1);
  b.doc.hidden = false;
  b.timers[0].f();
  await flush();
  assert.strictEqual(b.calls.length, 2);
});
test('failed and 5xx health during a restart do not notify', async () => {
  const b = load([H('aaa'), 'reject', '5xx']);
  await flush();
  for (let i = 0; i < 2; i++) { b.advance(60000); b.fireWin('corescope:ws-open'); await flush(); }
  assert.strictEqual(b.calls.length, 3);
  assert.strictEqual(b.banner(), null);
});
test('the same new commit across several checks shows one banner', async () => {
  const b = load([H('aaa'), H('bbb')]);
  await flush();
  for (let i = 0; i < 3; i++) { b.advance(60000); b.fireWin('corescope:ws-open'); await flush(); }
  assert.strictEqual(b.doc.body.children.filter((c) => 'data-version-notice' in c.attrs).length, 1);
});
test('a second new commit while the banner is open does not stack a second banner', async () => {
  const b = load([H('aaa'), H('bbb'), H('ccc')]);
  await flush();
  for (let i = 0; i < 2; i++) { b.advance(60000); b.fireWin('corescope:ws-open'); await flush(); }
  assert.strictEqual(b.calls.length, 3);
  assert.strictEqual(b.doc.body.children.filter((c) => 'data-version-notice' in c.attrs).length, 1);
});
test('Reload reloads the page; the notice does not reload by itself', async () => {
  const b = load([H('aaa'), H('bbb')]);
  await flush();
  b.advance(60000); b.fireWin('corescope:ws-open'); await flush();
  assert.strictEqual(b.reloads(), 0);
  b.banner().querySelector('[data-version-notice-reload]').click();
  assert.strictEqual(b.reloads(), 1);
});
test('Dismiss removes the banner and it does not return for the same commit', async () => {
  const b = load([H('aaa'), H('bbb')]);
  await flush();
  b.advance(60000); b.fireWin('corescope:ws-open'); await flush();
  b.banner().querySelector('[data-version-notice-dismiss]').click();
  assert.strictEqual(b.banner(), null);
  b.advance(60000); b.fireWin('corescope:ws-open'); await flush();
  assert.strictEqual(b.banner(), null);
  assert.strictEqual(b.reloads(), 0);
});
test('the drawer version footer still works and shares the one reader', async () => {
  const src = fs.readFileSync(__dirname + '/public/nav-drawer.js', 'utf8');
  assert.strictEqual((src.match(/fetch\(\s*'\/api\/health'/g) || []).length, 1, 'one fetch site in nav-drawer.js');
  assert.strictEqual((SRC.match(/fetch\(/g) || []).length, 0, 'version-notice.js must not fetch itself');
});

(async () => {
  for (const t of pending) await t();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
