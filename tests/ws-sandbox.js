/* tests/ws-sandbox.js — shared vm sandbox for the WebSocket unit tests
 * (test-issue-117-ws-watchdog.js, test-issue-937-ws-backoff.js).
 *
 * Loads the real public/app.js in a vm with a fake clock, fake timers and a
 * fake WebSocket, and boots it through its DOMContentLoaded listeners.
 * Math.random is pinned (opts.random, default 0.5 = no reconnect jitter) so
 * reconnect delays are deterministic; console.error calls are recorded in
 * box.errors.
 */
'use strict';
const vm = require('vm');
const fs = require('fs');
const assert = require('assert');

const APP = fs.readFileSync(__dirname + '/../public/app.js', 'utf8');
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

  const errors = [];
  const sandboxMath = Object.create(Math);
  sandboxMath.random = opts.random || (() => 0.5);

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
    console: { log() {}, warn() {}, error(...a) { errors.push(a); }, info() {}, debug() {} },
    setTimeout: setTimeout_, clearTimeout: clearTimeout_,
    setInterval: () => 0, clearInterval() {},
    Date: FakeDate, Math: sandboxMath, JSON, Object, Array, String, Number, Boolean, Error, RegExp, Map, Set, WeakMap, Symbol, Promise,
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
    ctx, clock, timers, sockets, advance, errors, math: sandboxMath,
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

module.exports = { APP, STALE, HEARTBEAT, PACKET, makeBox, booted };
