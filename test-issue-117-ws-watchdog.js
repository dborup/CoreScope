/* test-issue-117-ws-watchdog.js — shared WebSocket liveness (#117).
 *
 * Loads the real public/app.js in a vm with a fake clock, fake timers and a
 * fake WebSocket, boots it through its DOMContentLoaded listeners, and checks:
 *  - a silent OPEN socket and a handshake that never opens are replaced
 *    once after WS_STALE_MS, measured from socket creation;
 *  - any frame (heartbeat or traffic) keeps the socket;
 *  - heartbeats are consumed before the logo pulse, cache invalidation and
 *    onWS listeners (so before every pause buffer); packets are unchanged;
 *  - wall-clock steps, online and visible-tab resume;
 *  - ordinary close keeps the configured reconnect delay;
 *  - onclose, watchdog, resume and pull-to-reconnect never leave more than
 *    one live socket or more than one pending reconnect, and a replaced
 *    socket's late events are detached.
 */
'use strict';
const vm = require('vm');
const fs = require('fs');
const assert = require('assert');

console.log('--- test-issue-117-ws-watchdog.js ---');
let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

const APP = fs.readFileSync(__dirname + '/public/app.js', 'utf8');
const STALE_MATCH = APP.match(/const WS_STALE_MS = (\d+);/);
const STALE = STALE_MATCH ? Number(STALE_MATCH[1]) : 75000; // master has none: use the documented value
const HEARTBEAT = '{"type":"heartbeat"}';
const PACKET = JSON.stringify({ type: 'packet', data: { packet: { hash: 'abc' } } });

function makeBox(opts) {
  opts = opts || {};
  // Fake clock: `mono` drives timers, `wall` is what Date.now() returns.
  // They move together unless a test steps the wall clock.
  const clock = { mono: 0, wall: 1790000000000 };
  let seq = 0;
  const timers = new Map();
  function setTimeout_(fn, ms) {
    const id = ++seq;
    timers.set(id, { fn, at: clock.mono + Math.max(0, Number(ms) || 0), id });
    return id;
  }
  function clearTimeout_(id) { timers.delete(id); }
  function advance(ms) {
    const end = clock.mono + ms;
    for (;;) {
      let next = null;
      for (const t of timers.values()) if (t.at <= end && (!next || t.at < next.at || (t.at === next.at && t.id < next.id))) next = t;
      if (!next) break;
      clock.wall += next.at - clock.mono;
      clock.mono = next.at;
      timers.delete(next.id);
      next.fn();
    }
    clock.wall += end - clock.mono;
    clock.mono = end;
  }
  class FakeDate extends Date {
    constructor(...a) { if (a.length) super(...a); else super(clock.wall); }
    static now() { return clock.wall; }
  }

  const sockets = [];
  function FakeWS(url) {
    this.url = url;
    this.readyState = 0;
    this.closeCalled = false;
    this.onopen = this.onclose = this.onerror = this.onmessage = null;
    sockets.push(this);
  }
  FakeWS.prototype.close = function () {
    if (this.closeCalled) return;
    this.closeCalled = true;
    this.readyState = 2;
    // The close event arrives later (after the closing handshake; much
    // later on a half-open connection).
    const self = this;
    setTimeout_(function () { self.readyState = 3; if (self.onclose) self.onclose({}); }, opts.closeEventMs == null ? 50 : opts.closeEventMs);
  };
  FakeWS.prototype.send = function () {};
  FakeWS.prototype.open = function () { this.readyState = 1; if (this.onopen) this.onopen({}); };
  FakeWS.prototype.recv = function (data) { if (this.onmessage) this.onmessage({ data }); };
  FakeWS.prototype.serverClose = function () { this.readyState = 3; if (this.onclose) this.onclose({}); };

  const docListeners = {}, winListeners = {};
  function el(id) {
    return {
      id, style: {}, dataset: {}, textContent: '', innerHTML: '', value: '',
      classList: { add() {}, remove() {}, toggle() {}, contains() { return false; } },
      addEventListener() {}, removeEventListener() {}, setAttribute() {}, getAttribute() { return null; },
      appendChild(c) { return c; }, remove() {}, querySelector() { return null; }, querySelectorAll() { return []; },
      contains() { return false; },
    };
  }
  const doc = {
    hidden: false, readyState: 'loading',
    documentElement: Object.assign(el('html'), { scrollTop: 0, style: { setProperty() {} } }),
    body: el('body'), head: el('head'),
    createElement: (t) => el(t), getElementById: () => null,
    querySelector: () => null, querySelectorAll: () => [],
    addEventListener(ev, fn) { (docListeners[ev] = docListeners[ev] || []).push(fn); },
    removeEventListener() {},
  };
  const win = {
    addEventListener(ev, fn) { (winListeners[ev] = winListeners[ev] || []).push(fn); },
    removeEventListener() {}, dispatchEvent() { return true; },
    matchMedia: () => ({ matches: false, addEventListener() {}, addListener() {} }),
  };
  if (opts.reconnectMs) win.WS_RECONNECT_MS = opts.reconnectMs;
  const ctx = {
    console: { log() {}, warn() {}, error() {}, info() {}, debug() {} },
    setTimeout: setTimeout_, clearTimeout: clearTimeout_,
    setInterval: () => 0, clearInterval() {},
    Date: FakeDate, Math, JSON, Object, Array, String, Number, Boolean, Error, RegExp, Map, Set, WeakMap, Symbol, Promise,
    requestAnimationFrame: () => 0, cancelAnimationFrame() {},
    performance: { now: () => clock.mono },
    location: { protocol: 'http:', host: 'localhost', hash: '', pathname: '/', search: '' },
    navigator: { userAgent: 'test', maxTouchPoints: 0 },
    WebSocket: FakeWS,
    fetch: () => new Promise(() => {}),
    localStorage: { getItem: () => null, setItem() {}, removeItem() {} },
    sessionStorage: { getItem: () => null, setItem() {}, removeItem() {} },
    document: doc, window: win,
    CustomEvent: function (type, init) { this.type = type; this.detail = (init || {}).detail; },
    Event: function (type) { this.type = type; },
    MutationObserver: function () { this.observe = function () {}; this.disconnect = function () {}; },
    ResizeObserver: function () { this.observe = function () {}; this.disconnect = function () {}; },
  };
  Object.assign(win, { location: ctx.location, localStorage: ctx.localStorage, document: doc, navigator: ctx.navigator });
  ctx.self = win; ctx.globalThis = ctx;
  vm.createContext(ctx);
  vm.runInContext(APP, ctx);
  vm.runInContext('window.connectWS = connectWS; window.onWS = onWS; window.offWS = offWS; window.pullReconnect = pullReconnect; window.__api = api;', ctx);
  // Boot through the page's own startup listeners; a stubbed-DOM failure
  // after the socket wiring is irrelevant here.
  for (const fn of docListeners.DOMContentLoaded || []) { try { fn({}); } catch (_) {} }
  for (const fn of winListeners.DOMContentLoaded || []) { try { fn({}); } catch (_) {} }

  const box = {
    ctx, clock, timers, sockets, advance,
    live() { return sockets.filter((s) => !s.closeCalled && s.readyState !== 3); },
    current() { return sockets[sockets.length - 1]; },
    stepWall(ms) { clock.wall += ms; },
    setHidden(h) { doc.hidden = h; for (const fn of docListeners.visibilitychange || []) fn({}); },
    online() { for (const fn of winListeners.online || []) fn({}); },
    // pending timers whose callback is connectWS itself (the reconnect)
    pendingReconnects() {
      let n = 0;
      for (const t of timers.values()) if (t.fn === ctx.connectWS) n++;
      return n;
    },
  };
  return box;
}

function booted(opts) {
  const b = makeBox(opts);
  assert.strictEqual(b.sockets.length, 1, 'startup should open exactly one socket');
  return b;
}

console.log('\n=== silent sockets are replaced once ===');

test('a silent OPEN socket is replaced exactly once after WS_STALE_MS', () => {
  const b = booted();
  const s = b.current(); s.open();
  b.advance(10000); s.recv(PACKET);
  b.advance(STALE - 1);
  assert.strictEqual(b.sockets.length, 1, 'replaced before the threshold');
  b.advance(2);
  assert.strictEqual(b.sockets.length, 2, 'a socket silent for WS_STALE_MS was not replaced');
  assert(s.closeCalled, 'the stale socket was not closed');
  assert.strictEqual(s.onmessage, null, 'the stale socket still has its handlers');
  assert.strictEqual(b.live().length, 1);
  b.advance(1000); // the old socket's close event arrives: nothing more
  assert.strictEqual(b.sockets.length, 2, 'the replaced socket\'s close event scheduled another connection');
  assert.strictEqual(b.pendingReconnects(), 0);
});

test('a handshake that never opens is replaced after WS_STALE_MS from creation', () => {
  const b = booted();
  b.advance(STALE - 1);
  assert.strictEqual(b.sockets.length, 1);
  b.advance(2);
  assert.strictEqual(b.sockets.length, 2, 'a stuck handshake was never replaced');
  assert.strictEqual(b.live().length, 1);
});

test('heartbeats keep a quiet socket for 10 minutes', () => {
  const b = booted();
  const s = b.current(); s.open();
  for (let t = 0; t < 600000; t += 30000) { b.advance(30000); s.recv(HEARTBEAT); }
  assert.strictEqual(b.sockets.length, 1, 'a socket with regular heartbeats was replaced');
});

test('packet traffic alone keeps the socket', () => {
  const b = booted();
  const s = b.current(); s.open();
  for (let t = 0; t < 600000; t += STALE / 2) { b.advance(STALE / 2); s.recv(PACKET); }
  assert.strictEqual(b.sockets.length, 1);
});

console.log('\n=== heartbeats are consumed first ===');

test('a heartbeat reaches no onWS listener, no logo pulse and no cache invalidation', () => {
  const b = booted();
  const s = b.current(); s.open();
  const got = [];
  b.ctx.window.onWS((m) => got.push(m));
  const logo = b.ctx.window.__corescopeLogo;
  const before = logo.stats.triggered + logo.stats.dropped;
  s.recv(HEARTBEAT);
  assert.strictEqual(got.length, 0, 'the heartbeat was dispatched to a listener (and so to pause buffers)');
  assert.strictEqual(logo.stats.triggered + logo.stats.dropped, before, 'the heartbeat pulsed the logo');
  assert(!b.ctx.window.__api._invalidateTimer, 'the heartbeat scheduled cache invalidation');
});

test('packet messages are dispatched unchanged', () => {
  const b = booted();
  const s = b.current(); s.open();
  const got = [];
  b.ctx.window.onWS((m) => got.push(m));
  s.recv(PACKET);
  assert.strictEqual(got.length, 1);
  assert.deepStrictEqual(JSON.parse(JSON.stringify(got[0])), JSON.parse(PACKET));
});

console.log('\n=== clock steps and resume ===');

test('the wall clock stepping back does not postpone detection by the size of the step', () => {
  const b = booted();
  const s = b.current(); s.open();
  b.advance(1000); s.recv(PACKET);
  b.stepWall(-3600000); // one hour back
  b.advance(STALE + 1);
  assert.strictEqual(b.sockets.length, 2, 'after a backward clock step the silent socket was not replaced within WS_STALE_MS');
});

test('a forward step (sleep) is caught on visible-tab resume at once', () => {
  const b = booted();
  const s = b.current(); s.open(); s.recv(PACKET);
  b.setHidden(true);
  b.stepWall(10 * 60000); // asleep: timers did not run
  assert.strictEqual(b.sockets.length, 1, 'hiding the tab must not trigger a check');
  b.setHidden(false);
  assert.strictEqual(b.sockets.length, 2, 'resume after a long silence did not replace the socket');
  assert.strictEqual(b.live().length, 1);
});

test('online after a long silence reconnects at once; with recent traffic it does not', () => {
  const b = booted();
  const s = b.current(); s.open(); s.recv(PACKET);
  b.advance(5000);
  b.online();
  assert.strictEqual(b.sockets.length, 1, 'online with recent traffic replaced a healthy socket');
  b.stepWall(STALE);
  b.online();
  assert.strictEqual(b.sockets.length, 2, 'online after silence did not reconnect');
});

test('repeated resume events open one socket', () => {
  const b = booted();
  const s = b.current(); s.open();
  b.stepWall(STALE * 3);
  b.setHidden(false); b.online(); b.setHidden(false); b.online();
  assert.strictEqual(b.sockets.length, 2, b.sockets.length + ' sockets after four resume events');
  b.advance(STALE - 1);
  assert.strictEqual(b.sockets.length, 2);
  assert.strictEqual(b.live().length, 1);
});

console.log('\n=== close, pull and races ===');

test('an ordinary close keeps the configured reconnect delay, and schedules one reconnect', () => {
  const b = booted({ reconnectMs: 5000 });
  const s = b.current(); s.open(); s.recv(PACKET);
  s.serverClose();
  assert.strictEqual(b.pendingReconnects(), 1, 'onclose must leave exactly one pending reconnect');
  b.advance(4999);
  assert.strictEqual(b.sockets.length, 1, 'reconnected before the configured delay');
  b.advance(1);
  assert.strictEqual(b.sockets.length, 2);
  b.advance(STALE * 2); // no watchdog on the closed socket
  assert.strictEqual(b.live().length, 1);
});

test('resume and the watchdog during a pending reconnect do not add a socket', () => {
  const b = booted({ reconnectMs: 5000 });
  const s = b.current(); s.open();
  b.advance(STALE - 1000);
  s.serverClose();
  b.stepWall(STALE);
  b.setHidden(false); b.online();
  b.advance(4999);
  assert.strictEqual(b.sockets.length, 1, 'a resume check bypassed the pending reconnect');
  b.advance(1);
  assert.strictEqual(b.sockets.length, 2);
  assert.strictEqual(b.live().length, 1);
  assert.strictEqual(b.pendingReconnects(), 0);
});

test('a pull during the reconnect delay cancels the pending reconnect', () => {
  const b = booted({ reconnectMs: 5000 });
  const s = b.current(); s.open();
  s.serverClose();
  b.advance(1000);
  b.ctx.window.pullReconnect();
  assert.strictEqual(b.pendingReconnects(), 0, 'the reconnect scheduled by onclose is still pending');
  b.current().open();
  b.advance(10000);
  assert.strictEqual(b.sockets.length, 2, b.sockets.length + ' sockets: the old reconnect replaced the pulled socket');
  assert.strictEqual(b.live().length, 1);
});

test('pull-to-reconnect on a socket that is not open leaves one socket', () => {
  const b = booted();
  const s = b.current(); // still CONNECTING
  b.ctx.window.pullReconnect();
  b.advance(10000);
  assert.strictEqual(b.live().length, 1, b.live().length + ' live sockets after pull-to-reconnect');
  assert(s.closeCalled);
  assert.strictEqual(b.sockets.length, 2, b.sockets.length + ' sockets constructed');
});

test('pull-to-reconnect on an OPEN (possibly half-open) socket replaces it at once', () => {
  const b = booted({ closeEventMs: 60000 });
  const s = b.current(); s.open();
  b.ctx.window.pullReconnect();
  assert.strictEqual(b.sockets.length, 2, 'pull waited for the old socket\'s close event');
  b.advance(120000);
  assert.strictEqual(b.live().length, 1);
});

test('pull, watchdog and close racing each other end with one socket and no pending reconnect', () => {
  const b = booted({ reconnectMs: 3000 });
  let s = b.current(); s.open();
  b.ctx.window.pullReconnect();
  b.ctx.window.pullReconnect();
  s = b.current(); s.serverClose();
  b.ctx.window.pullReconnect();
  b.advance(STALE + 10); // watchdog of a never-opened socket
  b.setHidden(false); b.online();
  b.advance(20000);
  assert.strictEqual(b.live().length, 1, b.live().length + ' live sockets');
  assert(b.pendingReconnects() <= 1);
});

console.log(`\n${passed} passed, ${failed} failed`);
if (failed > 0) process.exit(1);
