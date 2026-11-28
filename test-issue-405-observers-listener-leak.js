/**
 * #405: observers.js must not leak its delegated click/keydown handlers onto
 * the shared #app container.
 *
 * public/observers.js init() wires two delegated handlers (a `click` for the
 * header buttons / per-row navigation, and a `keydown` for keyboard row
 * activation). They used to be bound to the shared `#app` element and were
 * never removed, so after one visit to Observers those handlers kept firing on
 * every later page, and each revisit stacked another pair (the same leak class
 * #402 fixed in compare.js).
 *
 * The fix binds them to the observers page's OWN container (`.observers-page`),
 * which the next page's `app.innerHTML = …` discards along with its listeners.
 *
 * This test drives the real observers.js in a vm sandbox whose elements record
 * their listeners and deduplicate on (type, handler) exactly as the DOM does.
 * `app.innerHTML = …` builds a fresh `.observers-page` container (discarding
 * the previous one), mirroring the SPA router. It asserts:
 * - after init(), the shared #app has NO click/keydown listener;
 * - after destroy(), #app still has none (the core #405 invariant);
 * - the handlers land on the page container and still drive navigation;
 * - a click/keydown on #app itself (outside the container) is inert;
 * - visiting Observers three times does not stack listeners on #app.
 *
 * Mutant: rebind either handler to `app` (the pre-#405 leak) and the "#app has
 * no listener" / "no stacking" cases go red.
 *
 * Usage: node test-issue-405-observers-listener-leak.js
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

const noop = () => {};

// A minimal element that records its own listeners, deduplicated on (type, fn)
// exactly as the DOM does (so a stable handler registered twice counts once —
// the test measures real listener count, not add() calls).
function makeEl(tag, id, className) {
  const listeners = []; // { type, fn }
  const classes = new Set((className || '').split(/\s+/).filter(Boolean));
  const el = {
    tagName: (tag || 'div').toUpperCase(),
    id: id || '',
    className: className || '',
    dataset: {},
    style: {},
    _listeners: listeners,
    innerHTML: '',
    textContent: '',
    isConnected: true,
    classList: {
      add: (c) => classes.add(c), remove: (c) => classes.delete(c),
      toggle: (c, on) => (on === undefined ? (classes.has(c) ? classes.delete(c) : classes.add(c)) : (on ? classes.add(c) : classes.delete(c))),
      contains: (c) => classes.has(c),
    },
    addEventListener: (type, fn) => {
      if (listeners.some((l) => l.type === type && l.fn === fn)) return;
      listeners.push({ type, fn });
    },
    removeEventListener: (type, fn) => {
      const i = listeners.findIndex((l) => l.type === type && l.fn === fn);
      if (i >= 0) listeners.splice(i, 1);
    },
    count: (type) => listeners.filter((l) => l.type === type).length,
    // Fire every listener of `type` with `evt`, like the browser would once the
    // event bubbles to this delegation root.
    fire: (type, evt) => {
      for (const l of listeners.filter((x) => x.type === type).slice()) l.fn(evt);
    },
    querySelector: noop,
    querySelectorAll: () => [],
    appendChild: noop,
    setAttribute: noop,
    getAttribute: () => null,
    closest: () => null,
    focus: noop,
    scrollIntoView: noop,
  };
  return el;
}

function load() {
  const hashWrites = [];
  const errors = [];
  const pages = {};
  const stubEls = new Map();
  function stub(id) {
    if (!stubEls.has(id)) stubEls.set(id, makeEl('div', id));
    return stubEls.get(id);
  }

  // The shared #app. Setting innerHTML rebuilds the page subtree: when the
  // markup is the observers page we expose a fresh `.observers-page` container
  // (discarding any previous one, as the real DOM does); otherwise the
  // container reference is cleared (a different page rendered).
  const app = makeEl('div', 'app');
  app._pageEl = null;
  Object.defineProperty(app, 'innerHTML', {
    get() { return app._html || ''; },
    set(v) {
      app._html = v;
      app._pageEl = /class="observers-page"/.test(v) ? makeEl('div', '', 'observers-page') : null;
    },
  });
  app.querySelector = (sel) => (sel === '.observers-page' ? app._pageEl : null);

  const location = {
    _hash: '#/observers',
    get hash() { return this._hash; },
    set hash(v) { hashWrites.push(v); this._hash = v; },
    search: '', pathname: '/', href: 'http://localhost/',
  };

  const ctx = {
    window: {
      addEventListener: noop, removeEventListener: noop, dispatchEvent: noop,
      innerWidth: 1400, innerHeight: 900, location,
      // SlideOver + MqttStatusPanel + TableSort intentionally absent: the
      // handlers fall through to location.hash navigation, and init() skips
      // the panel mount — keeping the harness to the listener-binding path.
    },
    document: {
      readyState: 'complete',
      body: { classList: { add: noop, remove: noop, toggle: noop, contains: () => false } },
      createElement: () => makeEl('div'),
      head: { appendChild: noop },
      getElementById: (id) => (id === 'app' ? app : stub(id)),
      addEventListener: noop, removeEventListener: noop,
      querySelectorAll: () => [], querySelector: () => null,
    },
    console: { log: noop, warn: noop, debug: noop, error: (...a) => { errors.push(a.map(String).join(' ')); } },
    Date, Math, Array, Object, String, Number, JSON, RegExp, Error, TypeError,
    Map, Set, Promise, URLSearchParams, parseInt, parseFloat, isNaN, isFinite,
    encodeURIComponent, decodeURIComponent,
    setTimeout: noop, clearTimeout: noop,
    // A stable, non-zero handle so destroy()'s clearInterval runs its branch.
    setInterval: () => 1, clearInterval: noop,
    // Never resolves: loadObservers() suspends at its first await, so init()
    // finishes synchronously at the listener-binding path without driving the
    // full render() (which needs far more DOM). The binding is what #405 is about.
    fetch: () => new Promise(() => {}),
    performance: { now: () => 0 },
    localStorage: { getItem: () => null, setItem: noop, removeItem: noop },
    location,
    history: { replaceState: noop, pushState: noop },
    CustomEvent: class CustomEvent {},
    requestAnimationFrame: noop,
    timeAgo: () => '', truncate: (s) => s, escapeHtml: (s) => String(s == null ? '' : s),
    registerPage: (name, page) => { pages[name] = page; },
    RegionFilter: {
      init: noop, offChange: noop, getSelected: () => null, getRegionParam: () => '',
      onChange: (fn) => fn,
    },
    debouncedOnWS: () => null, onWS: noop, offWS: noop, debounce: (fn) => fn,
    api: () => new Promise(() => {}),
    CLIENT_TTL: { observers: 1 },
    getHashParams: () => new URLSearchParams(''),
  };
  ctx.window.document = ctx.document;
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync('public/observers.js', 'utf8'), ctx, { filename: 'public/observers.js' });

  return {
    page: pages.observers,
    app,
    container: () => app._pageEl,
    hashWrites,
    errors,
    // A row click/keydown as it bubbles to the delegation root.
    rowClickEvent: (href) => ({
      target: { closest: (sel) => (sel === 'tr[data-action="navigate"]' ? { dataset: { value: href } } : null) },
      preventDefault: noop,
    }),
    rowKeyEvent: (href, key) => ({
      key: key || 'Enter',
      target: { closest: (sel) => (sel === 'tr[data-action="navigate"]' ? { dataset: { value: href } } : null) },
      preventDefault: noop,
    }),
    // An event that matches nothing (as if it hit #app chrome outside the page).
    inertEvent: () => ({ key: 'Enter', target: { closest: () => null }, preventDefault: noop }),
  };
}

console.log('\n=== #405 observers page: no delegated handlers leak onto #app ===');

test('observers.js registers a page with init and destroy', () => {
  const s = load();
  assert(s.page, 'registerPage("observers", …) ran');
  assert.strictEqual(typeof s.page.init, 'function', 'init');
  assert.strictEqual(typeof s.page.destroy, 'function', 'destroy');
});

test('init() binds NO click/keydown listener on the shared #app', () => {
  const s = load();
  s.page.init(s.app);
  assert.deepStrictEqual(s.errors, [], 'init ran without errors');
  assert.strictEqual(s.app.count('click'), 0, '#app click listeners: ' + s.app.count('click'));
  assert.strictEqual(s.app.count('keydown'), 0, '#app keydown listeners: ' + s.app.count('keydown'));
});

test('the handlers land on the observers page container instead', () => {
  const s = load();
  s.page.init(s.app);
  const c = s.container();
  assert(c, '.observers-page container exists');
  assert.strictEqual(c.count('click'), 1, 'container click listeners: ' + c.count('click'));
  assert.strictEqual(c.count('keydown'), 1, 'container keydown listeners: ' + c.count('keydown'));
});

// The core #405 invariant, worded exactly as the issue asks.
test('init -> destroy leaves NO listener on #app', () => {
  const s = load();
  s.page.init(s.app);
  s.page.destroy();
  assert.strictEqual(s.app.count('click'), 0, '#app click after destroy: ' + s.app.count('click'));
  assert.strictEqual(s.app.count('keydown'), 0, '#app keydown after destroy: ' + s.app.count('keydown'));
});

test('a click on #app itself (outside the page container) is inert', () => {
  const s = load();
  s.page.init(s.app);
  s.hashWrites.length = 0;
  s.app.fire('click', s.rowClickEvent('#/observers/should-not-fire'));
  s.app.fire('keydown', s.inertEvent());
  assert.deepStrictEqual(s.hashWrites, [], 'nothing on #app should navigate: ' + JSON.stringify(s.hashWrites));
});

test('the container click handler still navigates a row (behaviour unchanged)', () => {
  const s = load();
  s.page.init(s.app);
  s.hashWrites.length = 0;
  s.container().fire('click', s.rowClickEvent('#/observers/node-a'));
  assert.deepStrictEqual(s.hashWrites, ['#/observers/node-a'], 'row click navigates: ' + JSON.stringify(s.hashWrites));
});

test('the container keydown handler still activates a row on Enter', () => {
  const s = load();
  s.page.init(s.app);
  s.hashWrites.length = 0;
  s.container().fire('keydown', s.rowKeyEvent('#/observers/node-b', 'Enter'));
  assert.deepStrictEqual(s.hashWrites, ['#/observers/node-b'], 'Enter activates row: ' + JSON.stringify(s.hashWrites));
});

test('visiting Observers three times does not stack listeners on #app', () => {
  const s = load();
  for (let i = 0; i < 3; i++) {
    s.page.destroy();            // the router destroys the current page first
    s.page.init(s.app);
  }
  assert.strictEqual(s.app.count('click'), 0, '#app click after 3 visits: ' + s.app.count('click'));
  assert.strictEqual(s.app.count('keydown'), 0, '#app keydown after 3 visits: ' + s.app.count('keydown'));
  // The live container carries exactly one of each — no stacking anywhere.
  assert.strictEqual(s.container().count('click'), 1, 'live container click: ' + s.container().count('click'));
  assert.strictEqual(s.container().count('keydown'), 1, 'live container keydown: ' + s.container().count('keydown'));
});

test('after navigating away, #app carries no observers handler', () => {
  const s = load();
  s.page.init(s.app);
  s.page.destroy();
  s.app.innerHTML = '<div class="nodes-page"></div>'; // next page renders
  assert.strictEqual(s.app._pageEl, null, 'observers container discarded on next render');
  assert.strictEqual(s.app.count('click'), 0, '#app click after leaving: ' + s.app.count('click'));
  assert.strictEqual(s.app.count('keydown'), 0, '#app keydown after leaving: ' + s.app.count('keydown'));
});

runQueue().then(() => {
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed === 0 ? 0 : 1);
});
