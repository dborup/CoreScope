/**
 * #282 (1): the packets page's Escape-to-close-the-detail-panel listener must
 * not leak.
 *
 * public/packets.js registered a fresh `pktEsc` closure on `document` inside
 * renderLeft(), and renderLeft() runs on every visit to #/packets (and on every
 * filter/region change within a visit). The closure was never removed, so each
 * entry stacked one more live `keydown` listener on `document` -- the same leak
 * class as nodes.js' #259 nodesEsc/nodesPanelEsc. destroy() never took it off.
 *
 * The fix makes `_pktEsc` a stable module-level reference: a repeat
 * addEventListener of the same function is a DOM no-op (so at most one listener
 * survives), and destroy() removes it.
 *
 * This drives the real packets.js in a vm sandbox whose document
 * addEventListener/removeEventListener record the live listeners and
 * deduplicate on (type, handler) exactly as the DOM does, then runs the
 * router's init/destroy cycle for several #/packets visits and asserts:
 *  - a single visit leaves exactly one document keydown listener;
 *  - entering and leaving #/packets several times does not stack listeners;
 *  - a second render inside the same visit does not add a second listener;
 *  - destroy() leaves no document keydown listener behind;
 *  - the listener still works on a page opened after a destroy.
 *
 * init() renders the list from the asynchronous loadPackets(); the sandbox's
 * api() resolves immediately, so draining the microtask queue gets renderLeft()
 * -- where the listener is registered -- to run before the assertions.
 *
 * Usage: node test-issue-282-pktesc-listener.js
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

// packets.js' list view renders from the async loadPackets(); the sandbox's
// api() resolves immediately, so draining the microtask queue is enough to get
// renderLeft() to run. Nothing here depends on timers (the sandbox's are
// no-ops, so nothing fires behind the assertions).
async function settle() {
  for (let i = 0; i < 80; i++) await Promise.resolve();
}

const noop = () => {};

// A sandbox that records every document-level listener and the panel closes.
function loadPackets() {
  const docListeners = [];   // { type, fn }
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
    api: () => Promise.resolve({ packets: [], observations: [], messages: [], observers: [], total: 0 }),
    invalidateApiCache: noop,
    onWS: noop, offWS: noop, debouncedOnWS: () => null, debounce: (fn) => fn,
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
    errors,
    regionChange: () => regionChange && regionChange(),
    pressEscape: () => {
      for (const l of docListeners.filter((x) => x.type === 'keydown').slice()) {
        l.fn({ key: 'Escape', preventDefault: noop, stopPropagation: noop });
      }
    },
  };
}

console.log('\n=== #282 packets page: one Escape listener, not one per visit ===');

test('packets.js registers a page with init and destroy', () => {
  const s = loadPackets();
  assert(s.page, 'registerPage("packets", …) ran');
  assert.strictEqual(typeof s.page.init, 'function', 'init');
  assert.strictEqual(typeof s.page.destroy, 'function', 'destroy');
});

test('a single #/packets visit leaves exactly one document keydown listener', async () => {
  const s = loadPackets();
  s.page.init(s.app, null);
  await settle();
  assert.deepStrictEqual(s.errors, [], 'the list view rendered without errors');
  assert.strictEqual(s.keydownListeners().length, 1,
    'one Escape listener, got ' + s.keydownListeners().length);
});

test('entering and leaving #/packets several times does not stack listeners', async () => {
  const s = loadPackets();
  for (let i = 0; i < 4; i++) {
    s.page.destroy();          // the router destroys the current page first
    s.page.init(s.app, null);
    await settle();
  }
  assert.deepStrictEqual(s.errors, [], 'every render ran without errors');
  const n = s.keydownListeners().length;
  assert.strictEqual(n, 1, 'at most one listener after four visits, got ' + n);
});

test('a second render inside the same visit does not add a second listener', async () => {
  const s = loadPackets();
  s.page.init(s.app, null);
  await settle();
  s.regionChange();            // a region change re-runs loadPackets() -> renderLeft()
  await settle();
  assert.deepStrictEqual(s.errors, [], 'both renders ran without errors');
  const n = s.keydownListeners().length;
  assert.strictEqual(n, 1, 'still one listener after two renders, got ' + n);
});

test('destroy() removes the Escape listener', async () => {
  const s = loadPackets();
  s.page.init(s.app, null);
  await settle();
  s.page.destroy();
  assert.strictEqual(s.keydownListeners().length, 0,
    'got ' + s.keydownListeners().length);
});

test('the Escape listener still works on a page opened after a destroy', async () => {
  const s = loadPackets();
  s.page.init(s.app, null);
  await settle();
  s.page.destroy();
  s.page.init(s.app, null);
  await settle();
  assert.strictEqual(s.keydownListeners().length, 1,
    'one listener on the reopened page, got ' + s.keydownListeners().length);
});

test('the one listener is the functioning Escape handler: it closes an open panel', async () => {
  const s = loadPackets();
  s.page.init(s.app, null);
  await settle();
  // closeDetailPanel() marks #pktRight .empty and resets its body. Simulate an
  // open detail panel, then fire Escape through the registered listener.
  const panel = s.el('pktRight');
  panel.classList.remove('empty');
  panel.innerHTML = '<div>a selected packet</div>';
  s.pressEscape();
  assert.strictEqual(panel.classList.contains('empty'), true,
    'Escape should close (mark empty) the open detail panel');
  assert.ok(/Select a packet to view details/.test(panel.innerHTML),
    'Escape should reset the panel body, got: ' + panel.innerHTML);
});

runQueue().then(() => {
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed === 0 ? 0 : 1);
});
