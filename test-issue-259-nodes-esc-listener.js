/**
 * #259 (2): the node page's Escape-to-go-back listener must not leak.
 *
 * public/nodes.js init() wires a document-level `keydown` handler (`nodesEsc`)
 * for the full-screen node view. It used to be a fresh closure per init() and
 * was only ever removed when Escape actually fired, so navigating node A -> B
 * -> C left three live listeners on `document` and one Escape then wrote the
 * same hash three times.
 *
 * This test drives the real nodes.js in a vm sandbox with a counting
 * document.addEventListener / removeEventListener, runs the router's
 * destroy/init cycle for three node pages and asserts:
 * - at most one document `keydown` listener survives;
 * - Escape writes location.hash exactly once;
 * - destroy() leaves no document `keydown` listener behind;
 * - the listener still works on a page opened after a destroy.
 *
 * Usage: node test-issue-259-nodes-esc-listener.js
 */
'use strict';
const vm = require('vm');
const fs = require('fs');
const assert = require('assert');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

// A sandbox that records every document-level listener and the hash writes.
function loadNodes() {
  const noop = () => {};
  const docListeners = [];   // { type, fn }
  const hashWrites = [];
  const pages = {};
  const els = new Map();
  function el(id) {
    if (!els.has(id)) {
      els.set(id, {
        id, innerHTML: '', textContent: '', style: {}, isConnected: true,
        classList: { add: noop, remove: noop, toggle: noop, contains: () => false },
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
      addEventListener: (type, fn) => { docListeners.push({ type, fn }); },
      removeEventListener: (type, fn) => {
        const i = docListeners.findIndex((l) => l.type === type && l.fn === fn);
        if (i >= 0) docListeners.splice(i, 1);
      },
      querySelectorAll: () => [],
      querySelector: () => null,
    },
    console: { log: noop, warn: noop, error: noop, debug: noop },
    Date, Math, Array, Object, String, Number, JSON, RegExp, Error, TypeError,
    Map, Set, Promise, URLSearchParams, parseInt, parseFloat, isNaN, isFinite,
    encodeURIComponent, decodeURIComponent,
    setTimeout: noop, clearTimeout: noop, setInterval: noop, clearInterval: noop,
    fetch: () => Promise.resolve({ ok: true, json: () => Promise.resolve({}) }),
    performance: { now: () => Date.now() },
    localStorage: { getItem: () => null, setItem: noop, removeItem: noop },
    location,
    history: { replaceState: noop, pushState: noop },
    CustomEvent: class CustomEvent {},
    requestAnimationFrame: noop,
    ROLE_COLORS: {}, ROLE_STYLE: {}, TYPE_COLORS: {},
    getNodeStatus: () => 'active',
    getHealthThresholds: () => ({ staleMs: 1, degradedMs: 2, silentMs: 3 }),
    timeAgo: () => '', truncate: (s) => s, escapeHtml: (s) => String(s == null ? '' : s),
    payloadTypeName: () => '', payloadTypeColor: () => '',
    registerPage: (name, page) => { pages[name] = page; },
    RegionFilter: { init: noop, onChange: () => noop, offChange: noop, getRegionParam: () => '' },
    debouncedOnWS: () => null, onWS: noop, offWS: noop, debounce: (fn) => fn,
    api: () => Promise.resolve({}), invalidateApiCache: noop,
    CLIENT_TTL: { nodeList: 1, nodeDetail: 1, nodeHealth: 1 },
    initTabBar: noop, getFavorites: () => [], favStar: () => '', bindFavStars: noop,
    makeColumnsResizable: noop,
    getHashParams: () => new URLSearchParams(''),
    AreaFilter: { init: noop, onChange: () => noop, offChange: noop, getParam: () => '', get: () => null },
  };
  ctx.window.document = ctx.document;
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync('public/nodes.js', 'utf8'), ctx, { filename: 'public/nodes.js' });
  return {
    page: pages.nodes,
    app: el('app'),
    keydownListeners: () => docListeners.filter((l) => l.type === 'keydown'),
    hashWrites,
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

test('the nodes list view adds no Escape listener', () => {
  const s = loadNodes();
  s.page.init(s.app, null);
  assert.strictEqual(s.keydownListeners().length, 0, 'the list view has no Escape handler');
});

console.log(`\n${passed} passed, ${failed} failed`);
process.exit(failed === 0 ? 0 : 1);
