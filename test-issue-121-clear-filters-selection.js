/* test-issue-121-clear-filters-selection.js — Clear Filters must empty the
 * observer and type selections (#121).
 *
 * The Packets page keeps the observer and type picks in closure-owned Sets
 * (selectedObservers, selectedTypes). This test evaluates the REAL code from
 * public/packets.js — buildPacketsQuery(), updatePacketsUrl() and the whole
 * filter-bar section from the observer multi-select through the Clear
 * handler — in one scope, against a minimal fake DOM whose menus parse the
 * checkbox markup the page writes. It then drives the menus and the Clear
 * button the way a user does.
 */
'use strict';

const vm = require('vm');
const fs = require('fs');
const assert = require('assert');

console.log('--- test-issue-121-clear-filters-selection.js ---');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log(`  ✅ ${name}`); }
  catch (e) { failed++; console.log(`  ❌ ${name}: ${e.message}`); }
}

const SRC = fs.readFileSync(__dirname + '/public/packets.js', 'utf-8');

// Source of the balanced {...} block that starts at the first '{' after `from`.
function blockEnd(src, from) {
  const open = src.indexOf('{', from);
  let depth = 0;
  for (let i = open; i < src.length; i++) {
    if (src[i] === '{') depth++;
    else if (src[i] === '}') { depth--; if (depth === 0) return i + 1; }
  }
  throw new Error('unbalanced block after ' + from);
}
function extractFunction(name) {
  const start = SRC.indexOf('function ' + name + '(');
  assert(start !== -1, name + ' not found in packets.js');
  return SRC.slice(start, blockEnd(SRC, start));
}
// The filter-bar section: observer multi-select .. end of the Clear handler.
function extractFilterSection() {
  const start = SRC.indexOf('// --- Observer multi-select ---');
  assert(start !== -1, 'observer multi-select section not found');
  const clear = SRC.indexOf("if (clearBtn) clearBtn.addEventListener('click', function()", start);
  assert(clear !== -1, 'Clear handler not found');
  const end = SRC.indexOf(');', blockEnd(SRC, clear)) + 2;
  return SRC.slice(start, end);
}
// updateClearFiltersVisibility() is optional here so the URL-preservation tests
// below can also run (and fail) against a revision that lacks it.
const VISIBILITY = SRC.includes('function updateClearFiltersVisibility(')
  ? extractFunction('updateClearFiltersVisibility') + '\n' : '';
const SECTION = extractFunction('buildPacketsQuery') + '\n' + VISIBILITY +
  extractFunction('updatePacketsUrl') + '\n' + extractFilterSection();

// ---- a minimal DOM ---------------------------------------------------------

function makeEl(id) {
  const listeners = {};
  let items = [];   // checkboxes parsed from innerHTML
  let html = '';
  const classes = new Set();
  const el = {
    id, value: '', textContent: '', title: '', style: { display: '' }, children: [],
    classList: {
      add: (...c) => c.forEach(x => classes.add(x)),
      remove: (...c) => c.forEach(x => classes.delete(x)),
      toggle: (c) => { if (classes.has(c)) classes.delete(c); else classes.add(c); },
      contains: (c) => classes.has(c),
    },
    addEventListener: (ev, fn) => { (listeners[ev] = listeners[ev] || []).push(fn); },
    fire: (ev, e) => (listeners[ev] || []).forEach(fn => fn.call(el, e || {})),
    listenerCount: (ev) => (listeners[ev] || []).length,
    appendChild: (c) => { el.children.push(c); return c; },
    contains: () => true,
    querySelector: () => null,
    querySelectorAll: (sel) => (sel === 'input[type=checkbox]' ? items : []),
    get items() { return items; },
    get innerHTML() { return html; },
    set innerHTML(v) {
      html = String(v);
      items = [];
      const re = /<input type="checkbox"([^>]*)>/g;
      let m;
      while ((m = re.exec(html))) {
        const attrs = m[1];
        const id = /data-(?:obs|type)-id="([^"]*)"/.exec(attrs);
        items.push({ id: id ? id[1] : null, checked: /\schecked\b/.test(attrs), disabled: /\sdisabled\b/.test(attrs) });
      }
    },
  };
  return el;
}

// Mount the section once, as init() does. `state` persists across mounts
// (localStorage, location), `filters` is what the module holds at mount time.
function mount(state, filters) {
  const elements = {};
  const document = {
    getElementById: (id) => elements[id] || (elements[id] = makeEl(id)),
    createElement: (tag) => makeEl('_' + tag),
  };
  const calls = { loadPackets: 0, renderTableRows: 0 };
  const sandbox = {
    document,
    location: state.location,
    history: { replaceState: (_s, _t, url) => { state.location.hash = url; } },
    localStorage: {
      getItem: (k) => (k in state.storage ? state.storage[k] : null),
      setItem: (k, v) => { state.storage[k] = String(v); },
      removeItem: (k) => { delete state.storage[k]; },
    },
    window: {},
    RegionFilter: {
      getRegionParam: () => state.region.join(','),
      setSelected: (arr) => { state.region = Array.from(arr); },   // outer-realm array
    },
    api: () => Promise.resolve({ channels: [] }),
    escapeHtml: (s) => String(s).replace(/[&<>"']/g, (c) => '&#' + c.charCodeAt(0) + ';'),
    bindDocumentHandler: () => {},
    debounce: (fn) => fn,
    loadPackets: () => { calls.loadPackets++; },
    renderTableRows: () => { calls.renderTableRows++; },
    filters,
    observers: [{ id: 'obsA', name: 'Alpha' }, { id: 'obsB', name: 'Bravo' }, { id: 'obsC', name: 'Charlie' }],
    SHORT_BY_ID: { 0: 'REQ', 4: 'ADVERT', 5: 'GRP_TXT', 9: 'TRACE' },
    DEFAULT_TIME_WINDOW: 15,
    savedTimeWindowMin: 15,
    _observerFilterSet: null,
    _rebuildObserverMenu: null,
    _packetSortColumn: null,
    _packetSortDirection: 'desc',
  };
  sandbox.observerMap = new Map(sandbox.observers.map((o) => [o.id, o]));
  vm.createContext(sandbox);
  vm.runInContext(SECTION, sandbox);
  // init() calls updatePacketsUrl() right after the Clear handler is wired
  // ("Show clear button if page loaded with active filters").
  vm.runInContext('updatePacketsUrl()', sandbox);

  const $ = (id) => document.getElementById(id);
  function pick(menuId, attr, id) {
    const menu = $(menuId);
    const item = menu.items.find((x) => x.id === id);
    assert(item, `no ${id} row in #${menuId}`);
    item.checked = id === '__all__' ? true : !item.checked;
    menu.fire('change', { target: { dataset: { [attr]: id }, checked: item.checked } });
  }
  return {
    filters, calls, $, sandbox,
    pickObserver: (id) => pick('observerMenu', 'obsId', id),
    pickType: (id) => pick('typeMenu', 'typeId', id),
    clear: () => $('clearFiltersBtn').fire('click'),
    checked: (menuId) => $(menuId).items.filter((x) => x.checked).map((x) => x.id),
  };
}

function newState() {
  return { storage: {}, location: { hash: '#/packets' }, region: [] };
}

// ---- tests -------------------------------------------------------------------

test('observer: select A, Clear, select B -> only B, everywhere', () => {
  const st = newState();
  const p = mount(st, {});
  p.pickObserver('obsA');
  assert.strictEqual(p.filters.observer, 'obsA', 'fixture: A selected');
  p.clear();
  p.pickObserver('obsB');
  assert.strictEqual(p.filters.observer, 'obsB', 'filters.observer');
  assert.strictEqual(st.storage['meshcore-observer-filter'], 'obsB', 'localStorage');
  assert(/[?&]observer=obsB(&|$)/.test(st.location.hash) && !/obsA/.test(st.location.hash), 'URL ' + st.location.hash);
  assert.deepStrictEqual(p.checked('observerMenu'), ['obsB'], 'menu checkboxes');
  assert.strictEqual(p.$('observerTrigger').textContent, 'Bravo ▾', 'trigger label');
});

test('type: select one type, Clear, select another -> only the new type', () => {
  const st = newState();
  const p = mount(st, {});
  p.pickType('4');
  p.clear();
  p.pickType('5');
  assert.strictEqual(p.filters.type, '5', 'filters.type');
  assert.strictEqual(st.storage['meshcore-type-filter'], '5', 'localStorage');
  assert.deepStrictEqual(p.checked('typeMenu'), ['5'], 'menu checkboxes');
  assert.strictEqual(p.$('typeTrigger').textContent, 'GRP_TXT ▾', 'trigger label');
  assert.strictEqual(p.$('typeTrigger').title, 'Selected: GRP_TXT', 'trigger title');
});

test('after Clear: "All Observers" and "All Types" are checked, labels and titles reset', () => {
  const st = newState();
  const p = mount(st, {});
  p.pickObserver('obsA'); p.pickObserver('obsC');
  p.pickType('4'); p.pickType('9');
  p.clear();
  assert.deepStrictEqual(p.checked('observerMenu'), ['__all__'], 'observer menu');
  assert.deepStrictEqual(p.checked('typeMenu'), ['__all__'], 'type menu');
  assert.strictEqual(p.$('observerTrigger').textContent, 'All Observers ▾');
  assert.strictEqual(p.$('typeTrigger').textContent, 'All Types ▾');
  assert.strictEqual(p.$('typeTrigger').title, 'Filter by packet type');
  assert.strictEqual(p.filters.observer, undefined);
  assert.strictEqual(p.filters.type, undefined);
  assert(!('meshcore-observer-filter' in st.storage) && !('meshcore-type-filter' in st.storage), 'localStorage');
  assert(!/observer=/.test(st.location.hash), 'URL ' + st.location.hash);
});

test('observer and type together, repeated Clears, then new picks', () => {
  const st = newState();
  const p = mount(st, {});
  p.pickObserver('obsA'); p.pickType('4');
  p.clear(); p.clear();
  p.pickObserver('obsB'); p.pickType('5');
  assert.strictEqual(p.filters.observer, 'obsB');
  assert.strictEqual(p.filters.type, '5');
  p.clear();
  p.pickObserver('obsC');
  assert.strictEqual(p.filters.observer, 'obsC');
  assert.strictEqual(p.filters.type, undefined);
  assert.deepStrictEqual(p.checked('typeMenu'), ['__all__']);
});

test('Clear reloads packets exactly once and renders nothing extra', () => {
  const st = newState();
  const p = mount(st, {});
  p.pickObserver('obsA'); p.pickType('4');
  const before = { ...p.calls };
  p.clear();
  assert.strictEqual(p.calls.loadPackets - before.loadPackets, 1, 'loadPackets');
  assert.strictEqual(p.calls.renderTableRows - before.renderTableRows, 0, 'renderTableRows');
});

test('Clear is reachable with only a type selected (button shown), hidden again after', () => {
  const st = newState();
  const p = mount(st, {});
  assert.strictEqual(p.$('clearFiltersBtn').style.display, 'none', 'fixture: hidden with no filter');
  p.pickType('4');
  assert.strictEqual(p.$('clearFiltersBtn').style.display, '', 'shown with only a type');
  p.clear();
  assert.strictEqual(p.$('clearFiltersBtn').style.display, 'none', 'hidden after Clear');
});

// Picking a type must not rewrite the URL: type is not a URL parameter, so
// updatePacketsUrl() (which rebuilds the query from filters only) would drop
// ?obs= and ?viewPath= from #/packets/<hash>?... deep links.
for (const q of ['?obs=123', '?obs=123&viewPath=1']) {
  test('type pick keeps ' + q + ' in the URL and shows Clear', () => {
    const st = newState();
    const p = mount(st, {});
    // user is on a packet detail deep link (set after mount, as clicking a row does)
    st.location.hash = '#/packets/abcd1234' + q;
    p.pickType('4');
    assert.strictEqual(st.location.hash, '#/packets/abcd1234' + q, 'URL ' + st.location.hash);
    assert(/obs=123/.test(st.location.hash), 'obs kept');
    if (q.includes('viewPath')) assert(/viewPath=1/.test(st.location.hash), 'viewPath kept');
    assert.strictEqual(p.$('clearFiltersBtn').style.display, '', 'Clear visible');
    p.pickType('4'); // deselect -> no filter -> hidden again, URL still untouched
    assert.strictEqual(st.location.hash, '#/packets/abcd1234' + q, 'URL after deselect');
    assert.strictEqual(p.$('clearFiltersBtn').style.display, 'none', 'Clear hidden again');
  });
}

test('the other filters are still reset by Clear', () => {
  const st = newState();
  st.region = ['CPH'];
  const p = mount(st, { hash: 'abcd', node: 'n1', nodeName: 'n', channel: 'c', _filterExpr: 'x', _packetFilter: () => true, myNodes: true });
  p.sandbox.savedTimeWindowMin = 60;
  st.storage['meshcore-time-window'] = '60';
  p.clear();
  for (const k of ['hash', 'node', 'nodeName', 'channel', '_filterExpr']) assert.strictEqual(p.filters[k], undefined, k);
  assert.strictEqual(p.filters._packetFilter, null);
  assert.strictEqual(p.filters.myNodes, false);
  assert.strictEqual(p.sandbox.savedTimeWindowMin, 15, 'time window');
  assert(!('meshcore-time-window' in st.storage), 'time-window storage');
  assert.deepStrictEqual(st.region, [], 'region');
  assert.strictEqual(st.location.hash, '#/packets', 'URL');
});

test('SPA remount: a new mount starts from an empty selection after Clear', () => {
  const st = newState();
  const p1 = mount(st, {});
  p1.pickObserver('obsA'); p1.pickType('4');
  p1.clear();
  // destroy() resets the module's filters to {}; the page is mounted again
  const p2 = mount(st, {});
  assert.deepStrictEqual(p2.checked('observerMenu'), ['__all__']);
  assert.deepStrictEqual(p2.checked('typeMenu'), ['__all__']);
  p2.pickObserver('obsB'); p2.pickType('5');
  assert.strictEqual(p2.filters.observer, 'obsB');
  assert.strictEqual(p2.filters.type, '5');
  assert.strictEqual(st.storage['meshcore-observer-filter'], 'obsB');
  assert.strictEqual(p2.$('clearFiltersBtn').listenerCount('click'), 1, 'one Clear handler per mount');
  p2.clear();
  p2.pickObserver('obsC');
  assert.strictEqual(p2.filters.observer, 'obsC');
});

console.log(`\n${passed} passed, ${failed} failed`);
if (failed > 0) process.exit(1);
