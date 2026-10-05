/**
 * #259 (2): the node page's Escape-to-go-back listener must not leak.
 *
 * public/nodes.js init() wires a document-level `keydown` handler (`nodesEsc`)
 * for the full-screen node view. It used to be a fresh closure per init() and
 * was only ever removed when Escape actually fired, so navigating node A -> B
 * -> C left three live listeners on `document` and one Escape then wrote the
 * same hash three times.
 *
 * The nodes *list* view has a sibling of the same class: `nodesPanelEsc` in
 * renderLeft() (Escape closes the detail panel). renderLeft() runs on every
 * load of the list -- a visit, a region change, a filter change -- so that one
 * leaked one listener per render too. Both are covered here.
 *
 * This test drives the real nodes.js in a vm sandbox whose
 * document.addEventListener / removeEventListener record the live listeners and
 * deduplicate on (type, handler) exactly as the DOM does. It runs the router's
 * destroy/init cycle for three node pages and asserts:
 * - at most one document `keydown` listener survives;
 * - Escape writes location.hash exactly once;
 * - destroy() leaves no document `keydown` listener behind;
 * - the listener still works on a page opened after a destroy;
 * - the list view's own Escape listener (`nodesPanelEsc`) does not stack
 *   across renders or visits, and destroy() removes it.
 *
 * The list-view cases await the asynchronous loadNodes() before counting: the
 * listener is only registered once renderLeft() has run, so a synchronous
 * assertion would pass no matter what the page does.
 *
 * Usage: node test-issue-259-nodes-esc-listener.js
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

// nodes.js's list view renders from the async loadNodes(); the sandbox's api()
// resolves immediately, so draining the microtask queue is enough to get the
// real renderLeft() to run. Nothing here depends on timers (the sandbox's are
// no-ops on purpose, so nothing fires behind the assertions).
async function settle() {
  for (let i = 0; i < 50; i++) await Promise.resolve();
}

// A sandbox that records every document-level listener and the hash writes.
function loadNodes() {
  const noop = () => {};
  const docListeners = [];   // { type, fn }
  const hashWrites = [];
  const replaceStateWrites = [];
  const errors = [];         // anything nodes.js's catch blocks log
  let regionChange = null;   // the list view's RegionFilter.onChange handler
  const pages = {};
  const els = new Map();
  function el(id) {
    if (!els.has(id)) {
      // A real Set-backed classList, so nodesPanelEsc's "only act while a
      // detail panel is open" guard behaves as it does in the browser.
      const classes = new Set();
      els.set(id, {
        id, innerHTML: '', textContent: '', style: {}, isConnected: true,
        dataset: {}, value: '',
        classList: {
          add: (c) => classes.add(c), remove: (c) => classes.delete(c),
          toggle: (c, on) => (on ? classes.add(c) : classes.delete(c)),
          contains: (c) => classes.has(c),
        },
        addEventListener: noop, removeEventListener: noop,
        querySelector: () => null, querySelectorAll: () => [],
        appendChild: noop, setAttribute: noop, getAttribute: () => null,
        closest: () => null, focus: noop, scrollIntoView: noop,
      });
    }
    return els.get(id);
  }
  const location = {
    _hash: '#/nodes/aa',
    get hash() { return this._hash; },
    set hash(v) { hashWrites.push(v); this._hash = v; },
    search: '', pathname: '/', href: 'http://localhost/',
  };
  const ctx = {
    window: {
      addEventListener: noop, removeEventListener: noop, dispatchEvent: noop,
      innerWidth: 1400, innerHeight: 900, location,
    },
    document: {
      readyState: 'complete',
      body: { classList: { add: noop, remove: noop, toggle: noop, contains: () => false } },
      createElement: () => el('created'),
      head: { appendChild: noop },
      getElementById: (id) => el(id),
      // Deduplicated on (type, fn) exactly as the DOM does, so a stable
      // handler reference registered twice counts once — the test measures
      // real listener count, not add() calls.
      addEventListener: (type, fn) => {
        if (docListeners.some((l) => l.type === type && l.fn === fn)) return;
        docListeners.push({ type, fn });
      },
      removeEventListener: (type, fn) => {
        const i = docListeners.findIndex((l) => l.type === type && l.fn === fn);
        if (i >= 0) docListeners.splice(i, 1);
      },
      querySelectorAll: () => [],
      querySelector: () => null,
    },
    console: { log: noop, warn: noop, debug: noop,
      // Surfaced by the list-view cases: loadNodes() swallows its own
      // exceptions, so a missing stub would otherwise read as "no listener".
      error: (...a) => { errors.push(a.map(String).join(' ')); } },
    Date, Math, Array, Object, String, Number, JSON, RegExp, Error, TypeError,
    Map, Set, Promise, URLSearchParams, parseInt, parseFloat, isNaN, isFinite,
    encodeURIComponent, decodeURIComponent,
    setTimeout: noop, clearTimeout: noop, setInterval: noop, clearInterval: noop,
    fetch: () => Promise.resolve({ ok: true, json: () => Promise.resolve({}) }),
    performance: { now: () => Date.now() },
    localStorage: { getItem: () => null, setItem: noop, removeItem: noop },
    location,
    history: {
      replaceState: (_s, _t, url) => { replaceStateWrites.push(url); },
      pushState: noop,
    },
    CustomEvent: class CustomEvent {},
    requestAnimationFrame: noop,
    ROLE_COLORS: {}, ROLE_STYLE: {}, TYPE_COLORS: {},
    getNodeStatus: () => 'active',
    getHealthThresholds: () => ({ staleMs: 1, degradedMs: 2, silentMs: 3 }),
    timeAgo: () => '', truncate: (s) => s, escapeHtml: (s) => String(s == null ? '' : s),
    payloadTypeName: () => '', payloadTypeColor: () => '',
    registerPage: (name, page) => { pages[name] = page; },
    RegionFilter: {
      init: noop, offChange: noop, getRegionParam: () => '',
      onChange: (fn) => { regionChange = fn; return fn; },
    },
    debouncedOnWS: () => null, onWS: noop, offWS: noop, debounce: (fn) => fn,
    api: () => Promise.resolve({}), invalidateApiCache: noop,
    CLIENT_TTL: { nodeList: 1, nodeDetail: 1, nodeHealth: 1 },
    initTabBar: noop, getFavorites: () => [], favStar: () => '', bindFavStars: noop,
    makeColumnsResizable: noop,
    getHashParams: () => new URLSearchParams(''),
    AreaFilter: {
      init: noop, onChange: noop, offChange: noop, getParam: () => '',
      getAreaParam: () => '', getSelected: () => null, get: () => null,
    },
    nodePassesGeoFilter: () => true,
  };
  ctx.window.document = ctx.document;
  // The real markup renders the right panel as class="panel-right empty", and
  // nodesPanelEsc only acts while it is not .empty. Seed that, so the guard is
  // the browser's guard and the cases have to open the panel to exercise it.
  el('nodesRight').classList.add('empty');
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync('public/nodes.js', 'utf8'), ctx, { filename: 'public/nodes.js' });
  return {
    page: pages.nodes,
    app: el('app'),
    panel: el('nodesRight'),
    keydownListeners: () => docListeners.filter((l) => l.type === 'keydown'),
    hashWrites,
    replaceStateWrites,
    errors,
    // Re-runs the list view's load the way a region change does: one more
    // renderLeft() inside the same visit.
    regionChange: () => regionChange && regionChange(),
    pressEscape: () => {
      for (const l of docListeners.filter((x) => x.type === 'keydown').slice()) {
        l.fn({ key: 'Escape', preventDefault: () => {}, stopPropagation: () => {} });
      }
    },
  };
}

console.log('\n=== #259 node page: one Escape listener, not one per visit ===');

test('nodes.js registers a page with init and destroy', () => {
  const s = loadNodes();
  assert(s.page, 'registerPage("nodes", …) ran');
  assert.strictEqual(typeof s.page.init, 'function', 'init');
  assert.strictEqual(typeof s.page.destroy, 'function', 'destroy');
});

test('a single node page leaves exactly one document keydown listener', () => {
  const s = loadNodes();
  s.page.init(s.app, 'aa');
  assert.strictEqual(s.keydownListeners().length, 1, 'got ' + s.keydownListeners().length);
});

test('navigating A -> B -> C does not stack listeners', () => {
  const s = loadNodes();
  for (const pk of ['aa', 'bb', 'cc']) {
    s.page.destroy();          // the router destroys the current page first
    s.page.init(s.app, pk);
  }
  const n = s.keydownListeners().length;
  assert(n <= 1, 'expected at most one keydown listener after three visits, got ' + n);
});

test('after A -> B -> C one Escape writes the hash exactly once', () => {
  const s = loadNodes();
  for (const pk of ['aa', 'bb', 'cc']) {
    s.page.destroy();
    s.page.init(s.app, pk);
  }
  s.hashWrites.length = 0;
  s.pressEscape();
  assert.deepStrictEqual(s.hashWrites, ['#/nodes'], 'hash writes: ' + JSON.stringify(s.hashWrites));
});

test('destroy() removes the Escape listener', () => {
  const s = loadNodes();
  s.page.init(s.app, 'aa');
  s.page.destroy();
  assert.strictEqual(s.keydownListeners().length, 0, 'got ' + s.keydownListeners().length);
  s.hashWrites.length = 0;
  s.pressEscape();
  assert.deepStrictEqual(s.hashWrites, [], 'a destroyed page does not navigate: ' + JSON.stringify(s.hashWrites));
});

test('Escape still goes back on a page opened after a destroy', () => {
  const s = loadNodes();
  s.page.init(s.app, 'aa');
  s.page.destroy();
  s.page.init(s.app, 'bb');
  s.hashWrites.length = 0;
  s.pressEscape();
  assert.deepStrictEqual(s.hashWrites, ['#/nodes'], 'hash writes: ' + JSON.stringify(s.hashWrites));
});

// --- the list view's own Escape listener (nodesPanelEsc) --------------------
//
// The previous version of this block asserted that the list view adds no
// Escape listener at all. That passed for the wrong reason: it counted before
// the asynchronous loadNodes() -> renderLeft() had run. The list view does add
// one, and these cases await the render and hold it to one.

test('the list view registers exactly one Escape listener once its load resolves', async () => {
  const s = loadNodes();
  s.page.init(s.app, null);
  await settle();
  assert.deepStrictEqual(s.errors, [], 'the list view rendered without errors');
  assert.strictEqual(s.keydownListeners().length, 1,
    'one list-view Escape listener, got ' + s.keydownListeners().length);
});

test('a second render inside the same visit does not add a second listener', async () => {
  const s = loadNodes();
  s.page.init(s.app, null);
  await settle();
  s.regionChange();          // a region change re-runs loadNodes() -> renderLeft()
  await settle();
  assert.deepStrictEqual(s.errors, [], 'both renders ran without errors');
  const n = s.keydownListeners().length;
  assert.strictEqual(n, 1, 'still one listener after two renders, got ' + n);
});

test('list -> node -> list -> node -> list does not stack listeners', async () => {
  const s = loadNodes();
  for (const pk of [null, 'aa', null, 'bb', null]) {
    s.page.destroy();        // the router destroys the current page first
    s.page.init(s.app, pk);
    await settle();
  }
  assert.deepStrictEqual(s.errors, [], 'every render ran without errors');
  const n = s.keydownListeners().length;
  assert.strictEqual(n, 1, 'the list view ends with one listener, got ' + n);
});

test('one Escape closes the list view\'s detail panel exactly once', async () => {
  const s = loadNodes();
  for (const pk of [null, 'aa', null]) {
    s.page.destroy();
    s.page.init(s.app, pk);
    await settle();
  }
  // nodesPanelEsc only acts while the right panel is not .empty, so open it.
  s.panel.classList.remove('empty');
  s.replaceStateWrites.length = 0;
  s.pressEscape();
  assert.deepStrictEqual(s.replaceStateWrites, ['#/nodes'],
    'one panel close, got ' + JSON.stringify(s.replaceStateWrites));
});

test('destroy() removes the list view\'s Escape listener', async () => {
  const s = loadNodes();
  s.page.init(s.app, null);
  await settle();
  s.page.destroy();
  assert.strictEqual(s.keydownListeners().length, 0,
    'got ' + s.keydownListeners().length);
  s.panel.classList.remove('empty');
  s.replaceStateWrites.length = 0;
  s.pressEscape();
  assert.deepStrictEqual(s.replaceStateWrites, [],
    'a destroyed list view does not close anything: ' + JSON.stringify(s.replaceStateWrites));
});

runQueue().then(() => {
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed === 0 ? 0 : 1);
});
