/**
 * #881 F2: leaving #/packets before its init() finishes must not leak the
 * page's WS handler (or its document click delegation) into the next page.
 *
 * packets.js init() awaits loadObservers()+loadPackets() and only then
 * registers `debouncedOnWS(...)` and the document `data-action` click handler.
 * The router calls destroy() synchronously on navigation, so a quick
 * Packets -> Live hop ran destroy() while init() was still awaiting; init()
 * then resumed and registered both listeners on a page that was already gone.
 * Nothing ever removed them: the leaked WS handler kept feeding the module's
 * pauseBuffer and every later page saw one extra wsListener per hop.
 *
 * The fix: destroy() bumps initGeneration, and init() returns after each await
 * when its generation is stale.
 *
 * This drives the real packets.js in the same vm sandbox as
 * test-issue-282-pktesc-listener.js, with api() held behind a gate so the
 * destroy() lands mid-init deterministically.
 *
 * Usage: node test-881-packets-init-race.js
 */
'use strict';
const vm = require('vm');
const fs = require('fs');
const assert = require('assert');

let passed = 0, failed = 0;
const queue = [];
function test(name, fn) { queue.push([name, fn]); }
async function runQueue() {
  for (const [name, fn] of queue) {
    try { await fn(); passed++; console.log('  ✅ ' + name); }
    catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
  }
}

async function settle() {
  for (let i = 0; i < 80; i++) await Promise.resolve();
}

const noop = () => {};

// A sandbox that records every document-level listener and the panel closes.
function loadPackets() {
  const docListeners = [];   // { type, fn }
  const wsListeners = [];    // handlers debouncedOnWS() registered and offWS() did not remove
  let openGate;
  const gate = new Promise((r) => { openGate = r; });
  // api() answers only when the test opens the gate, so a destroy() can land
  // while init() is still awaiting its first fetch -- the race under test.
  const gatedApi = () => gate.then(() => ({ packets: [], observations: [], messages: [], observers: [], total: 0 }));
  const offWS = (fn) => { const i = wsListeners.indexOf(fn); if (i >= 0) wsListeners.splice(i, 1); };
  const debouncedOnWS = () => { const h = function () {}; wsListeners.push(h); return h; };
  const errors = [];
  let regionChange = null;   // the list view's RegionFilter.onChange handler
  const pages = {};
  const els = new Map();
  function makeEl(id) {
    const classes = new Set();
    const el = {
      id, tagName: 'DIV', innerHTML: '', textContent: '', value: '', checked: false,
      style: {}, dataset: {}, isConnected: true, children: [], rows: [],
      classList: {
        add: (c) => classes.add(c), remove: (c) => classes.delete(c),
        toggle: (c, on) => (on === undefined ? (classes.has(c) ? classes.delete(c) : classes.add(c)) : (on ? classes.add(c) : classes.delete(c))),
        contains: (c) => classes.has(c),
      },
      addEventListener: noop, removeEventListener: noop,
      querySelector: () => null, querySelectorAll: () => [],
      appendChild: (c) => { el.children.push(c); return c; },
      insertBefore: (c) => { el.children.push(c); return c; },
      removeChild: noop, remove: noop, replaceChildren: noop,
      setAttribute: (k, v) => { el.dataset[k] = v; }, getAttribute: () => null,
      removeAttribute: noop, hasAttribute: () => false,
      closest: () => null, focus: noop, blur: noop, scrollIntoView: noop,
      getBoundingClientRect: () => ({ width: 0, height: 0, top: 0, left: 0, right: 0, bottom: 0 }),
      offsetWidth: 0, offsetHeight: 0, scrollWidth: 0, clientWidth: 0,
    };
    return el;
  }
  function el(id) {
    if (!els.has(id)) els.set(id, makeEl(id));
    return els.get(id);
  }
  const location = {
    _hash: '#/packets',
    get hash() { return this._hash; },
    set hash(v) { this._hash = v; },
    search: '', pathname: '/', href: 'http://localhost/', origin: 'http://localhost',
  };
  const stubComponent = {
    init: noop, register: noop, setSelected: noop, offChange: noop,
    getRegionParam: () => '', getAreaParam: () => '', getParam: () => '',
    getSelected: () => [], get: () => null,
    onChange: (fn) => { regionChange = fn; return fn; },
    unhidden: (_t, fn) => { if (typeof fn === 'function') fn(); },
  };
  const ctx = {
    window: {
      addEventListener: noop, removeEventListener: noop, dispatchEvent: noop,
      innerWidth: 1200, innerHeight: 900, location,
      // Component singletons packets.js guards on; keep them present but inert.
      HopDisplay: { renderHop: () => '', renderPathSymbolsLegend: () => '', _showFromBtn: noop },
      // PacketFilter/FilterUX/ChannelColorPicker/URLState/PacketPathMap stay
      // undefined on purpose -- each call site is guarded, so the render path
      // that registers _pktEsc runs without them.
    },
    document: {
      readyState: 'complete',
      body: { classList: { add: noop, remove: noop, toggle: noop, contains: () => false }, appendChild: noop },
      createElement: () => makeEl('created'),
      head: { appendChild: noop },
      getElementById: (id) => el(id),
      // Deduplicated on (type, fn) exactly as the DOM does, so a stable handler
      // reference registered twice counts once -- this measures real listener
      // count, not addEventListener calls.
      addEventListener: (type, fn) => {
        if (docListeners.some((l) => l.type === type && l.fn === fn)) return;
        docListeners.push({ type, fn });
      },
      removeEventListener: (type, fn) => {
        const i = docListeners.findIndex((l) => l.type === type && l.fn === fn);
        if (i >= 0) docListeners.splice(i, 1);
      },
      querySelector: () => null,
      querySelectorAll: () => [],
    },
    console: { log: noop, warn: noop, debug: noop, info: noop,
      error: (...a) => { errors.push(a.map(String).join(' ')); } },
    Date, Math, Array, Object, String, Number, JSON, RegExp, Error, TypeError, RangeError,
    Infinity, NaN, Map, Set, Promise, URLSearchParams, parseInt, parseFloat, isNaN, isFinite,
    encodeURIComponent, decodeURIComponent,
    setTimeout: noop, clearTimeout: noop, setInterval: noop, clearInterval: noop,
    requestAnimationFrame: noop, cancelAnimationFrame: noop,
    fetch: () => Promise.resolve({ ok: true, json: () => Promise.resolve({}) }),
    performance: { now: () => Date.now() },
    localStorage: { getItem: () => null, setItem: noop, removeItem: noop },
    location,
    history: { replaceState: noop, pushState: noop },
    CustomEvent: class CustomEvent {},
    // api() resolves immediately with empty-but-shaped data so loadObservers()
    // and loadPackets() complete and renderLeft() runs.
    api: gatedApi,
    invalidateApiCache: noop,
    onWS: noop, offWS, debouncedOnWS,
    debounce: (fn) => fn,
    registerPage: (name, page) => { pages[name] = page; },
    RegionFilter: stubComponent,
    AreaFilter: stubComponent,
    TableSort: { init: () => ({ destroy: noop }) },
    TableResponsive: { register: noop, unhidden: (_t, fn) => { if (typeof fn === 'function') fn(); } },
    makeColumnsResizable: noop,
    CLIENT_TTL: {},
  };
  ctx.window.document = ctx.document;
  vm.createContext(ctx);
  // Load the real dependency surface packets.js needs, then packets.js itself.
  for (const f of ['public/payload-labels.js', 'public/roles.js', 'public/app.js', 'public/packet-helpers.js']) {
    vm.runInContext(fs.readFileSync(f, 'utf8'), ctx, { filename: f });
    for (const k of Object.keys(ctx.window)) ctx[k] = ctx.window[k];
  }
  // Keep the heavy layout/sort helpers inert even though app.js defined real
  // ones: the listener under test is registered before them, and they want a
  // live table DOM this sandbox does not build.
  ctx.makeColumnsResizable = noop;
  // app.js also installed its real api() and WS helpers; put the gated and
  // recording ones back.
  ctx.api = ctx.window.api = gatedApi;
  ctx.offWS = offWS;
  ctx.debouncedOnWS = debouncedOnWS;
  // app.js defines the real registerPage() (into its own lexical `pages`), which
  // the dep load above installed over the stub. Restore a capturing registerPage
  // so packets.js' registerPage('packets', …) lands in this sandbox's `pages`.
  ctx.registerPage = (name, page) => { pages[name] = page; };
  vm.runInContext(fs.readFileSync('public/packets.js', 'utf8'), ctx, { filename: 'public/packets.js' });

  return {
    page: pages.packets,
    app: el('app'),
    el,
    keydownListeners: () => docListeners.filter((l) => l.type === 'keydown'),
    clickListeners: () => docListeners.filter((l) => l.type === 'click'),
    wsListeners: () => wsListeners.length,
    openGate: () => openGate(),
    errors,
    regionChange: () => regionChange && regionChange(),
    pressEscape: () => {
      for (const l of docListeners.filter((x) => x.type === 'keydown').slice()) {
        l.fn({ key: 'Escape', preventDefault: noop, stopPropagation: noop });
      }
    },
  };
}

console.log('\n=== #881 F2 packets: no listener leak when init is overtaken ===');

test('a completed visit registers exactly one WS handler and destroy() removes it', async () => {
  const s = loadPackets();
  s.page.init(s.app, null);
  s.openGate();
  await settle();
  assert.deepStrictEqual(s.errors, [], 'init ran without errors');
  assert.strictEqual(s.wsListeners(), 1, 'one WS handler after init');
  s.page.destroy();
  assert.strictEqual(s.wsListeners(), 0, 'none after destroy');
});

test('destroy() while init awaits its first fetch leaves no WS handler behind', async () => {
  const s = loadPackets();
  s.page.init(s.app, null);
  await settle();               // init is parked on the gated fetch
  s.page.destroy();             // the user already left #/packets
  s.openGate();                 // the fetch answers afterwards
  await settle();
  assert.strictEqual(s.wsListeners(), 0, 'leaked ' + s.wsListeners() + ' WS handler(s)');
});

test('destroy() while init awaits leaves no document click delegation behind', async () => {
  const s = loadPackets();
  const before = s.clickListeners().length;
  s.page.init(s.app, null);
  await settle();
  s.page.destroy();
  s.openGate();
  await settle();
  assert.strictEqual(s.clickListeners().length, before,
    'click listeners ' + before + ' -> ' + s.clickListeners().length);
});

test('three quick Packets visits that are each left mid-init leak nothing', async () => {
  const s = loadPackets();
  for (let i = 0; i < 3; i++) {
    s.page.init(s.app, null);
    await settle();
    s.page.destroy();
  }
  s.openGate();
  await settle();
  assert.strictEqual(s.wsListeners(), 0, 'leaked ' + s.wsListeners());
});

test('an overtaken init does not block the visit that replaced it', async () => {
  const s = loadPackets();
  s.page.init(s.app, null);     // first visit, left mid-init
  await settle();
  s.page.destroy();
  s.page.init(s.app, null);     // second visit stays
  s.openGate();
  await settle();
  assert.deepStrictEqual(s.errors, [], 'no errors');
  assert.strictEqual(s.wsListeners(), 1, 'exactly the current visit\'s handler, got ' + s.wsListeners());
});

runQueue().then(() => {
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed === 0 ? 0 : 1);
});
