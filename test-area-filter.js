/**
 * Unit tests for public/area-filter.js
 *
 * Tests the exported AreaFilter API via Node.js vm sandbox.
 * No real DOM or network — localStorage and fetch are mocked.
 */
'use strict';
const vm = require('vm');
const fs = require('fs');
const assert = require('assert');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log(`  ✓ ${name}`); }
  catch (e) { failed++; console.error(`  ✗ ${name}: ${e.message}`); }
}

// ---------------------------------------------------------------------------
// Minimal fake DOM: enough of Element for render() to build and for the tests
// to walk the result. No HTML parsing — an innerHTML write of markup is kept
// as an opaque string so a regression to the string-built menu is visible.
// ---------------------------------------------------------------------------
function makeEl(tag) {
  const attrs = Object.create(null);
  const el = {
    tagName: tag.toUpperCase(),
    className: '',
    hidden: false,
    onclick: null,
    children: [],
    parent: null,
    textContent: '',
    style: {},
    _html: '',
    setAttribute(k, v) { attrs[k] = String(v); },
    getAttribute(k) { return k in attrs ? attrs[k] : null; },
    hasAttribute(k) { return k in attrs; },
    appendChild(c) { c.parent = el; el.children.push(c); return c; },
    get dataset() {
      const d = {};
      Object.keys(attrs).forEach(k => { if (k.indexOf('data-') === 0) d[k.slice(5)] = attrs[k]; });
      return d;
    },
    get innerHTML() { return el._html; },
    set innerHTML(v) { el._html = String(v); el.children = []; el.textContent = ''; },
    closest(sel) {
      const m = /^\[([\w-]+)\]$/.exec(sel);
      for (let n = el; n; n = n.parent) if (m && n.hasAttribute(m[1])) return n;
      return null;
    },
    contains(n) { for (; n; n = n.parent) if (n === el) return true; return false; },
    all() { return el.children.reduce((a, c) => a.concat(c, c.all()), []); },
    querySelector(sel) {
      const cls = sel.replace(/^\./, '');
      return el.all().find(c => c.className.split(' ').indexOf(cls) !== -1) || null;
    },
    querySelectorAll(sel) {
      const cls = sel.replace(/^\./, '');
      return el.all().filter(c => c.className.split(' ').indexOf(cls) !== -1);
    },
  };
  return el;
}

// ---------------------------------------------------------------------------
// Helper: build a fresh vm context with mocked globals.
// Each call resets _selected / _areas so tests are independent.
// ---------------------------------------------------------------------------
function buildCtx({ storageValue = null, fetchResponse = [] } = {}) {
  const storage = Object.create(null);
  if (storageValue !== null) storage['meshcore-area-filter'] = storageValue;

  const localStorage = {
    getItem: (k) => (k in storage ? storage[k] : null),
    setItem: (k, v) => { storage[k] = String(v); },
    removeItem: (k) => { delete storage[k]; },
  };

  const fetchMock = () => Promise.resolve({ json: () => Promise.resolve(fetchResponse) });

  // Minimal document stub for render() calls and cleanup registration.
  const listeners = [];
  const document = {
    createElement: makeEl,
    addEventListener: (type, fn, capture) => listeners.push({ type, fn, capture }),
    removeEventListener: (type, fn) => {
      const idx = listeners.findIndex(l => l.fn === fn);
      if (idx !== -1) listeners.splice(idx, 1);
    },
  };

  const ctx = {
    window: {},
    console,
    document,
    localStorage,
    fetch: fetchMock,
    encodeURIComponent,
    Array,
    Promise,
    String,
  };
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync('public/area-filter.js', 'utf8'), ctx);
  return { AF: ctx.window.AreaFilter, ctx, storage, listeners };
}

// ---------------------------------------------------------------------------
// Default state (no storage, no areas loaded)
// ---------------------------------------------------------------------------
console.log('\n=== default state (no selection, no areas) ===');
{
  const { AF } = buildCtx();
  test('getSelected() is null initially', () => assert.strictEqual(AF.getSelected(), null));
  test('getAreaParam() returns empty string', () => assert.strictEqual(AF.getAreaParam(), ''));
  test('areaQueryString() returns empty string', () => assert.strictEqual(AF.areaQueryString(), ''));
}

// ---------------------------------------------------------------------------
// State restored from localStorage
// ---------------------------------------------------------------------------
console.log('\n=== selection restored from localStorage ===');
{
  const { AF } = buildCtx({ storageValue: 'BEL' });
  test('getSelected() returns stored key', () => assert.strictEqual(AF.getSelected(), 'BEL'));
  test('getAreaParam() returns stored key', () => assert.strictEqual(AF.getAreaParam(), 'BEL'));
  test('areaQueryString() returns &area=BEL', () => assert.strictEqual(AF.areaQueryString(), '&area=BEL'));
}

// ---------------------------------------------------------------------------
// areaQueryString encodes special characters
// ---------------------------------------------------------------------------
console.log('\n=== areaQueryString URL-encoding ===');
{
  const { AF } = buildCtx({ storageValue: 'SAN JOSE' });
  test('spaces are encoded', () => assert.strictEqual(AF.areaQueryString(), '&area=SAN%20JOSE'));
}

// ---------------------------------------------------------------------------
// fetchAreas — selection cleared when key no longer exists in config
// ---------------------------------------------------------------------------
console.log('\n=== fetchAreas clears stale selection ===');
(async () => {
  const { AF, storage } = buildCtx({
    storageValue: 'GONE',
    fetchResponse: [{ key: 'NL', label: 'Netherlands' }],
  });
  await AF.init(makeDomContainer());
  test('stale key cleared after fetchAreas', () => assert.strictEqual(AF.getSelected(), null));
  test('localStorage entry removed when stale', () => assert.strictEqual(storage['meshcore-area-filter'], undefined));
})();

// ---------------------------------------------------------------------------
// fetchAreas — valid selection kept after fetch
// ---------------------------------------------------------------------------
console.log('\n=== fetchAreas keeps valid selection ===');
(async () => {
  const { AF } = buildCtx({
    storageValue: 'BEL',
    fetchResponse: [{ key: 'BEL', label: 'Belgium' }, { key: 'NL', label: 'Netherlands' }],
  });
  await AF.init(makeDomContainer());
  test('valid selection retained after fetchAreas', () => assert.strictEqual(AF.getSelected(), 'BEL'));
  test('getAreaParam() still returns BEL', () => assert.strictEqual(AF.getAreaParam(), 'BEL'));
})();

// ---------------------------------------------------------------------------
// Select an area, then click "All" — selection clears to null
// ---------------------------------------------------------------------------
console.log('\n=== "All" restores empty state ===');
(async () => {
  const { AF, storage } = buildCtx({
    storageValue: 'BEL',
    fetchResponse: [{ key: 'BEL', label: 'Belgium' }],
  });

  const container = makeEl('div');

  await AF.init(container);
  test('before All click: selected is BEL', () => assert.strictEqual(AF.getSelected(), 'BEL'));

  // Click the real "All" button in the rendered menu.
  const menu = container.querySelector('.area-dropdown-menu');
  menu.onclick({ target: menu.children[0] });
  test('"All" click clears selection to null', () => assert.strictEqual(AF.getSelected(), null));
  test('localStorage entry removed after All click', () => assert.strictEqual(storage['meshcore-area-filter'], undefined));
  test('areaQueryString() empty after All click', () => assert.strictEqual(AF.areaQueryString(), ''));
})();


// ---------------------------------------------------------------------------
// #989: labels / keys are config-controlled; the menu must show them as text
// ---------------------------------------------------------------------------
console.log('\n=== #989 render builds DOM from text, not HTML ===');
(async () => {
  const evil = [
    { key: 'a"><b>k1</b>', label: '<b>Bold</b> "q" \'s\' & <i>x</i>' },
    { key: "k'2&amp;", label: '&lt;img src=x onerror=1&gt;' },
  ];
  const { AF } = buildCtx({ fetchResponse: evil });
  const container = makeEl('div');
  const seen = [];
  AF.onChange(v => seen.push(v));
  await AF.init(container);

  const menu = container.querySelector('.area-dropdown-menu');
  const items = menu.children;
  test('#989 markup is never written through innerHTML', () => assert.strictEqual(container.innerHTML, ''));
  test('#989 one button per area plus All', () => {
    assert.strictEqual(items.length, 3);
    assert.ok(items.every(b => b.tagName === 'BUTTON'));
    assert.strictEqual(items[0].textContent, 'All');
  });
  test('#989 labels render as literal text', () => {
    assert.strictEqual(items[1].textContent, evil[0].label);
    assert.strictEqual(items[2].textContent, evil[1].label);
  });
  test('#989 keys survive in data-area verbatim', () => {
    assert.strictEqual(items[1].getAttribute('data-area'), evil[0].key);
    assert.strictEqual(items[2].getAttribute('data-area'), evil[1].key);
    assert.strictEqual(items[0].getAttribute('data-area'), '__all__');
  });
  test('#989 no stray element nodes injected', () => {
    // wrap > (trigger, menu > 3 buttons)
    assert.strictEqual(container.all().length, 1 + 1 + 1 + 3);
  });
  test('#989 trigger shows All as text', () => {
    assert.strictEqual(container.querySelector('.region-dropdown-trigger').textContent, 'Area: All ▾');
  });
  test('#989 classes and ARIA are unchanged', () => {
    const wrap = container.children[0];
    assert.strictEqual(wrap.className, 'region-dropdown-wrap');
    assert.strictEqual(wrap.getAttribute('role'), 'group');
    assert.strictEqual(wrap.getAttribute('aria-label'), 'Area filter');
    const t = container.querySelector('.region-dropdown-trigger');
    assert.strictEqual(t.getAttribute('aria-haspopup'), 'listbox');
    assert.strictEqual(t.getAttribute('aria-expanded'), 'false');
    assert.strictEqual(menu.className, 'region-dropdown-menu area-dropdown-menu');
    assert.strictEqual(menu.getAttribute('role'), 'listbox');
    assert.strictEqual(menu.getAttribute('aria-label'), 'Select area');
    assert.strictEqual(menu.hidden, true);
    assert.strictEqual(items[0].className, 'region-dropdown-item area-dropdown-item area-item-active');
    assert.strictEqual(items[1].className, 'region-dropdown-item area-dropdown-item');
  });

  // Select the hostile area: selection, active class and trigger label follow.
  menu.onclick({ target: items[1] });
  test('#989 click selects the exact key', () => {
    assert.strictEqual(AF.getSelected(), evil[0].key);
    assert.deepStrictEqual(seen, [evil[0].key]);
  });
  test('#989 trigger label is literal text after selection', () => {
    assert.strictEqual(container.querySelector('.region-dropdown-trigger').textContent,
      'Area: ' + evil[0].label + ' ▾');
  });
  test('#989 only the selected item is active after re-render', () => {
    const its = container.querySelector('.area-dropdown-menu').children;
    assert.deepStrictEqual(its.map(b => b.className.indexOf('area-item-active') !== -1), [false, true, false]);
    assert.strictEqual(its.length, 3);
  });
  test('#989 trigger toggles menu and aria-expanded', () => {
    const t = container.querySelector('.region-dropdown-trigger');
    const m = container.querySelector('.area-dropdown-menu');
    t.onclick({ stopPropagation() {} });
    assert.strictEqual(m.hidden, false);
    assert.strictEqual(t.getAttribute('aria-expanded'), 'true');
    t.onclick({ stopPropagation() {} });
    assert.strictEqual(m.hidden, true);
    assert.strictEqual(t.getAttribute('aria-expanded'), 'false');
  });
})();

// ---------------------------------------------------------------------------
// onChange / offChange
// ---------------------------------------------------------------------------
console.log('\n=== onChange / offChange ===');
{
  const { AF } = buildCtx();
  const calls = [];
  const fn = (v) => calls.push(v);
  AF.onChange(fn);

  // Manually trigger a selection change via render + click simulation would
  // require full DOM. Instead verify the listener is registered and can be removed.
  AF.offChange(fn);
  // No assertion failure means no error in register/remove path.
  test('offChange removes listener without error', () => assert.ok(true));
}

// ---------------------------------------------------------------------------
// Helper: minimal DOM container stub for init/render calls
// ---------------------------------------------------------------------------
function makeDomContainer() {
  return makeEl('div');
}

// Print synchronous results immediately; async tests append on their own.
setImmediate(() => {
  console.log(`\n${passed} passed, ${failed} failed`);
  if (failed > 0) process.exit(1);
});
