/**
 * #314 (1): the channel message scroll listener must not throw after the page
 * is gone, and must not outlive it.
 *
 * public/channels.js registered an anonymous `scroll` listener on #chMessages
 * inside init() whose body did
 *
 *     document.getElementById('chScrollBtn').classList.toggle('hidden', atBottom)
 *
 * with no guard. The router (public/app.js) destroys the current page and then
 * lets the next page's init() rebuild #app, which detaches #chMessages. A
 * `scroll` event can still reach that detached element -- resetting the scroll
 * position of a removed overflow box queues one -- and by then #chScrollBtn is
 * no longer in the document, so getElementById() returns null and the listener
 * threw:
 *
 *     Uncaught TypeError: Cannot read properties of null (reading 'classList')
 *         at HTMLDivElement.<anonymous> (channels.js:1668)
 *
 * The fix optional-chains the lookup, the way every other #chScrollBtn call
 * site in the file already does, and makes the handler a stable module-level
 * function so destroy() can take it off the element it was added to again --
 * the same leak class as #259/#282, scoped to the element instead of
 * `document`.
 *
 * The harness drives the real channels.js in a vm sandbox (modelled on
 * test-channels-client-state-152.js) whose elements record listeners and
 * deduplicate on (type, handler) exactly as the DOM does, so the counts below
 * are real listener counts and not addEventListener call counts.
 *
 * The harness models element identity the way the browser does: assigning
 * #app's innerHTML detaches every element the previous page created, so a
 * second init() gets a *new* #chMessages. That is what makes the leak
 * measurable here -- one detached #chMessages per visit, each pinned by a
 * live handler -- instead of collapsing onto a single reused element.
 *
 * Usage: node test-issue-314-chscroll-listener.js
 */
'use strict';

const vm = require('vm');
const fs = require('fs');
const path = require('path');
const assert = require('assert');
const { webcrypto } = require('crypto');

let passed = 0, failed = 0;
const queue = [];
function test(name, fn) { queue.push([name, fn]); }
async function runQueue() {
  for (const [name, fn] of queue) {
    try { await fn(); passed++; console.log('  ✅ ' + name); }
    catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
  }
}

async function flush(n) {
  for (let i = 0; i < (n || 20); i++) await new Promise((r) => setImmediate(r));
}

const noop = () => {};

function makeHarness() {
  const storage = {};
  let elements = {};
  // Every element the harness has ever handed out, so a test can ask what the
  // *detached* ones are still holding.
  const everyEl = [];
  // The harness cannot parse HTML, so getElementById() fabricates whatever id
  // the page asks for -- which models "the markup init() just wrote is there".
  // Off while no channels markup is in the document (before the first init,
  // and after the router has handed #app to another page).
  let markupPresent = false;

  function makeFakeEl(id) {
    const classes = new Set();
    const listeners = {};
    const el = {
      id: id || '', textContent: '', value: '', checked: false,
      scrollTop: 0, scrollHeight: 0, clientHeight: 0,
      style: {}, dataset: {},
      isConnected: true,
      classList: {
        add: (c) => { classes.add(c); },
        remove: (c) => { classes.delete(c); },
        toggle: (c, on) => {
          if (on === undefined) { if (classes.has(c)) classes.delete(c); else classes.add(c); return; }
          if (on) classes.add(c); else classes.delete(c);
        },
        contains: (c) => classes.has(c),
      },
      // Deduplicated on (type, handler) exactly as the DOM does: a stable
      // handler registered twice counts once.
      addEventListener(type, fn) {
        const l = (listeners[type] = listeners[type] || []);
        if (l.indexOf(fn) === -1) l.push(fn);
      },
      removeEventListener(type, fn) {
        const l = listeners[type];
        if (!l) return;
        const i = l.indexOf(fn);
        if (i >= 0) l.splice(i, 1);
      },
      listenerCount(type) { return (listeners[type] || []).length; },
      dispatch(type, extra) {
        const ev = Object.assign({ type, target: el, currentTarget: el, preventDefault: noop, stopPropagation: noop }, extra || {});
        for (const fn of (listeners[type] || []).slice()) fn(ev);
      },
      querySelector() { this._q = this._q || {}; return this._q.one || (this._q.one = makeFakeEl()); },
      querySelectorAll() { return []; },
      getAttribute() { return null; }, setAttribute: noop, removeAttribute: noop,
      hasAttribute: () => false, closest: () => null,
      getBoundingClientRect: () => ({ width: 240, height: 0, top: 0, left: 0, right: 0, bottom: 0 }),
      appendChild: noop, removeChild: noop, remove: noop, insertBefore: noop,
      focus: noop, blur: noop, scrollIntoView: noop,
    };
    // Writing #app's innerHTML is what the router/page does on every visit:
    // the previous page's markup leaves the document, and the new markup is
    // there to be looked up. Exactly the moment the old #chMessages detaches.
    let html = '';
    Object.defineProperty(el, 'innerHTML', {
      get: () => html,
      set: (v) => {
        html = String(v);
        if (el.id === 'app') { replaceAppMarkup(); markupPresent = true; }
      },
    });
    everyEl.push(el);
    return el;
  }
  // Detach everything the previous page's markup held and forget it, so the
  // next getElementById() builds a fresh element.
  function replaceAppMarkup() {
    for (const id of Object.keys(elements)) {
      if (id === 'app') continue;
      elements[id].isConnected = false;
      delete elements[id];
    }
  }
  function el(id) {
    if (!elements[id]) elements[id] = makeFakeEl(id);
    return elements[id];
  }

  const windowListeners = {};
  const errors = [];
  const h = { elements };

  const ctx = {
    window: {
      addEventListener(type, fn) { (windowListeners[type] = windowListeners[type] || []).push(fn); },
      removeEventListener: noop,
      matchMedia: () => ({ matches: false, addEventListener: noop, removeEventListener: noop }),
    },
    document: {
      readyState: 'complete',
      documentElement: { getAttribute: () => null, setAttribute: noop, classList: { add: noop, remove: noop, toggle: noop, contains: () => false } },
      createElement: () => makeFakeEl(),
      head: { appendChild: noop },
      body: { appendChild: noop, removeChild: noop, contains: () => false },
      // Once the router has handed #app to another page, the channels markup --
      // #chMessages, #chScrollBtn -- is no longer in the document.
      getElementById: (id) => (id === 'app' || markupPresent ? el(id) : null),
      addEventListener: noop, removeEventListener: noop,
      querySelector: () => null,
      querySelectorAll: () => [],
    },
    console: Object.assign({}, console, { error: (...a) => { errors.push(a.map(String).join(' ')); } }),
    Date, Math, Array, Object, String, Number, JSON, RegExp, Error, TypeError, RangeError,
    Infinity, NaN, Set, Map, Promise, parseInt, parseFloat, isNaN, isFinite,
    encodeURIComponent, decodeURIComponent,
    setTimeout: (fn) => { Promise.resolve().then(fn); return 0; },
    clearTimeout: noop, setInterval: () => 0, clearInterval: noop,
    requestAnimationFrame: (cb) => { Promise.resolve().then(cb); return 0; },
    cancelAnimationFrame: noop,
    fetch: () => Promise.resolve({ ok: true, json: () => Promise.resolve({}) }),
    performance: { now: () => Date.now() },
    localStorage: {
      getItem: (k) => Object.prototype.hasOwnProperty.call(storage, k) ? storage[k] : null,
      setItem: (k, v) => { storage[k] = String(v); },
      removeItem: (k) => { delete storage[k]; },
    },
    location: { hash: '#/channels', search: '', pathname: '/' },
    history: { replaceState: noop, pushState: noop },
    crypto: webcrypto, TextEncoder, TextDecoder,
    Uint8Array, Uint16Array, Uint32Array, Int8Array, Int16Array, Int32Array, ArrayBuffer,
    URLSearchParams,
    CustomEvent: class CustomEvent {},
    MutationObserver: class MutationObserver { observe() {} disconnect() {} },
    matchMedia: () => ({ matches: false, addEventListener: noop, removeEventListener: noop }),
    addEventListener: noop, dispatchEvent: noop,
    getHashParams: () => new URLSearchParams(),
    btoa: (s) => Buffer.from(String(s), 'binary').toString('base64'),
    atob: (s) => Buffer.from(String(s), 'base64').toString('binary'),
    CSS: { escape: (s) => String(s) },
  };
  ctx.self = ctx;
  ctx.globalThis = ctx;

  ctx.onWS = noop;
  ctx.offWS = noop;
  ctx.debouncedOnWS = (fn) => fn;
  ctx.debounce = (fn) => fn;
  ctx.api = () => Promise.resolve({ channels: [], messages: [], packets: [], observers: [] });
  ctx.invalidateApiCache = noop;
  ctx.CLIENT_TTL = { channels: 15000, observers: 120000, channelMessages: 10000, nodeDetail: 10000 };
  ctx.escapeHtml = (s) => String(s == null ? '' : s);
  ctx.truncate = (s) => String(s || '');
  ctx.formatHashHex = (x) => String(x);
  ctx.formatSecondsAgo = () => '';
  ctx.timeAgo = () => '';
  ctx.payloadTypeName = () => 'GRP_TXT';
  ctx.ROLE_EMOJI = {};
  ctx.ROLE_LABELS = {};
  ctx.RegionFilter = {
    init: noop, onChange: (fn) => fn, offChange: noop,
    getRegionParam: () => '', getSelected: () => null,
  };
  ctx.ChannelColors = { get: () => null, remove: noop };
  ctx.ChannelColorPicker = { open: noop, show: noop };

  let pageMod = null;
  ctx.registerPage = (name, mod) => { if (name === 'channels') pageMod = mod; };

  vm.createContext(ctx);
  function load(file) {
    vm.runInContext(fs.readFileSync(path.join(__dirname, file), 'utf8'), ctx, { filename: file });
    for (const k of Object.keys(ctx.window)) ctx[k] = ctx.window[k];
  }
  load('public/vendor/aes-ecb.js');
  load('public/channel-decrypt.js');
  load('public/channel-proposals.js');
  ctx.window.ChannelProposals.mount = noop;
  ctx.window.ChannelProposals.unmount = noop;
  load('public/channels.js');

  h.page = pageMod;
  h.errors = errors;
  h.msgEl = () => el('chMessages');
  h.scrollBtn = () => el('chScrollBtn');
  // Scroll listeners on the #chMessages that is currently in the document.
  h.scrollListeners = () => el('chMessages').listenerCount('scroll');
  // Scroll listeners on every #chMessages ever built, detached ones included.
  // This is the leak: a detached element with a live handler stays reachable.
  h.allScrollListeners = () => everyEl.reduce((n, e) => n + (e.id === 'chMessages' ? e.listenerCount('scroll') : 0), 0);
  h.init = async () => { await pageMod.init(el('app'), null); await flush(); };
  // What the router does on the way out: destroy the page, then hand #app to
  // the next page's init(). The old #chMessages is detached and nothing from
  // the channels markup is findable any more.
  h.navigateAway = () => { replaceAppMarkup(); markupPresent = false; };
  return h;
}

console.log('\n=== #314 channels: the message scroll listener survives teardown ===');

test('channels.js registers a page with init and destroy', () => {
  const h = makeHarness();
  assert(h.page, 'registerPage("channels", …) ran');
  assert.strictEqual(typeof h.page.init, 'function', 'init');
  assert.strictEqual(typeof h.page.destroy, 'function', 'destroy');
});

test('a single #/channels visit leaves exactly one scroll listener on #chMessages', async () => {
  const h = makeHarness();
  await h.init();
  assert.deepStrictEqual(h.errors, [], 'init rendered without errors');
  assert.strictEqual(h.scrollListeners(), 1, 'got ' + h.scrollListeners());
});

test('the scroll listener toggles #chScrollBtn while the page is live', async () => {
  const h = makeHarness();
  await h.init();
  const msgEl = h.msgEl();
  // Scrolled well away from the bottom: the "new messages" button shows.
  msgEl.scrollHeight = 2000; msgEl.clientHeight = 400; msgEl.scrollTop = 0;
  msgEl.dispatch('scroll');
  assert.strictEqual(h.scrollBtn().classList.contains('hidden'), false,
    'away from the bottom the button must be visible');
  // Back at the bottom: it hides again.
  msgEl.scrollTop = 1600;
  msgEl.dispatch('scroll');
  assert.strictEqual(h.scrollBtn().classList.contains('hidden'), true,
    'at the bottom the button must be hidden');
});

// The acceptance criterion from the issue: #/channels -> #/live, with a
// `scroll` still in flight on the message pane that just left the document.
test('a scroll event on a detached #chMessages does not throw', async () => {
  const h = makeHarness();
  await h.init();
  const msgEl = h.msgEl();
  msgEl.scrollHeight = 2000; msgEl.clientHeight = 400; msgEl.scrollTop = 0;
  h.navigateAway();          // the router handed #app to the next page
  assert.strictEqual(msgEl.isConnected, false, 'the pane really is detached');
  assert.doesNotThrow(() => msgEl.dispatch('scroll'),
    'the listener must tolerate #chScrollBtn being gone');
});

test('destroy() removes the scroll listener', async () => {
  const h = makeHarness();
  await h.init();
  h.page.destroy();
  assert.strictEqual(h.scrollListeners(), 0, 'got ' + h.scrollListeners());
});

test('entering and leaving #/channels several times leaves one live listener', async () => {
  const h = makeHarness();
  for (let i = 0; i < 4; i++) {
    h.page.destroy();        // the router destroys the current page first
    await h.init();          // ... then the next init() rebuilds #app
  }
  assert.deepStrictEqual(h.errors, [], 'every visit rendered without errors');
  assert.strictEqual(h.scrollListeners(), 1,
    'one listener on the live pane, got ' + h.scrollListeners());
  // The four panes from the earlier visits are detached; none may still hold
  // a handler, or each visit pins its old subtree.
  assert.strictEqual(h.allScrollListeners(), 1,
    'one scroll listener across every #chMessages ever built, got ' + h.allScrollListeners());
});

// The router always destroys before it re-inits (public/app.js), so this is
// the belt-and-braces half of the guarantee: init() drops the pane it held
// before it takes the new one, so even a re-init without a destroy cannot
// leave a handler on the detached one.
test('a re-init without a destroy leaves no handler on the old pane', async () => {
  const h = makeHarness();
  await h.init();
  const first = h.msgEl();
  await h.init();
  assert.notStrictEqual(h.msgEl(), first, 'the second init() built a new pane');
  assert.strictEqual(first.listenerCount('scroll'), 0,
    'the detached pane must be clean, got ' + first.listenerCount('scroll'));
  assert.strictEqual(h.allScrollListeners(), 1, 'got ' + h.allScrollListeners());
});

test('the listener still works on a page opened after a destroy', async () => {
  const h = makeHarness();
  await h.init();
  h.page.destroy();
  await h.init();
  assert.strictEqual(h.scrollListeners(), 1, 'got ' + h.scrollListeners());
  const msgEl = h.msgEl();
  msgEl.scrollHeight = 2000; msgEl.clientHeight = 400; msgEl.scrollTop = 0;
  msgEl.dispatch('scroll');
  assert.strictEqual(h.scrollBtn().classList.contains('hidden'), false,
    'the reopened page still toggles the button');
});

runQueue().then(() => {
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed === 0 ? 0 : 1);
});
