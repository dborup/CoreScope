/**
 * #258: makeColumnsResizable() (public/app.js) sizes a table's columns from its
 * header and its first body rows.
 *
 * 1. Rows that do not have one cell per column -- a full-width colspan cell (the
 *    packets vscroll spacers, "No packets found") or a different cell count --
 *    are not measured. They used to be credited to column 0 by index.
 * 2. Widths are not locked from an (almost) empty body. With fewer than
 *    COL_MEASURE_MIN_ROWS usable rows the table gets provisional widths and the
 *    columns are measured again ONCE, when enough rows arrive -- unless the user
 *    has saved widths by then. What governs the one-shot MutationObserver is the
 *    usable-row count of the FIRST render, not which page the table belongs to:
 *    a table whose first body already has >= COL_MEASURE_MIN_ROWS usable rows
 *    gets no observer (no per-render work), but any table -- analytics included
 *    -- whose first render has fewer than that (e.g. an analytics tab with < 5
 *    data rows) does create the one-shot observer. (#282 (5): an earlier claim
 *    that analytics tables "never create an observer" was wrong on that count.)
 * 3. Saved widths (localStorage) are applied as before, without measuring.
 *
 * Runs the real app.js in a vm sandbox against a minimal fake table DOM. A
 * cell's scrollWidth is its content width; like a real auto-layout table, a
 * header whose th carries a width reports at least that width, and a shown
 * resize handle adds the 4px it sticks out. So a re-measure that keeps the
 * provisional widths, drops a width the page's markup set on a th, or counts
 * the handles that the first measure did not have, is caught.
 *
 * Usage: node test-issue-258-column-widths.js
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

// --- sandbox -----------------------------------------------------------------

const observers = [];
class FakeMutationObserver {
  constructor(cb) { this.cb = cb; this.target = null; this.options = null; this.connected = false; observers.push(this); }
  observe(target, options) { this.target = target; this.options = options; this.connected = true; }
  disconnect() { this.connected = false; }
  // A real observer only calls back while it observes.
  fire() { if (this.connected) this.cb([], this); }
}

const tables = {};
const store = {};
const ctx = {
  window: { addEventListener: () => {}, dispatchEvent: () => {} },
  document: {
    readyState: 'complete',
    createElement: () => ({ className: '', classList: { add() {}, remove() {} }, addEventListener() {}, style: {} }),
    head: { appendChild: () => {} },
    body: { style: {} },
    getElementById: () => null,
    addEventListener: () => {},
    removeEventListener: () => {},
    querySelectorAll: () => [],
    querySelector: (sel) => tables[sel] || null,
  },
  console, Date, Infinity, Math, Array, Object, String, Number, JSON, RegExp, Error, TypeError,
  parseInt, parseFloat, isNaN, isFinite, encodeURIComponent, decodeURIComponent,
  setTimeout: () => {}, clearTimeout: () => {}, setInterval: () => {}, clearInterval: () => {},
  fetch: () => Promise.resolve({ json: () => Promise.resolve({}) }),
  performance: { now: () => Date.now() },
  localStorage: {
    getItem: (k) => (k in store ? store[k] : null),
    setItem: (k, v) => { store[k] = String(v); },
    removeItem: (k) => { delete store[k]; },
  },
  location: { hash: '' },
  CustomEvent: class CustomEvent {},
  Map, Set, Promise, URLSearchParams,
  addEventListener: () => {}, dispatchEvent: () => {},
  requestAnimationFrame: () => {},
  MutationObserver: FakeMutationObserver,
};
ctx.getHashParams = () => new URLSearchParams('');
vm.createContext(ctx);
vm.runInContext(fs.readFileSync('public/payload-labels.js', 'utf8'), ctx);
vm.runInContext(fs.readFileSync('public/app.js', 'utf8'), ctx, { filename: 'public/app.js' });
const makeColumnsResizable = ctx.makeColumnsResizable;

// --- fake table ----------------------------------------------------------------

const CONTAINER_W = 1000;
const pct = (s) => parseFloat(s);

function cell(w, colSpan) { return { scrollWidth: w, colSpan: colSpan || 1, style: {}, dataset: {} }; }
// A cell TableResponsive hid (class col-hidden, display:none): it has no width
// unless its hiding is lifted (TableResponsive.unhidden) while measuring.
function hiddenCell(w) {
  return { colSpan: 1, style: {}, dataset: {}, table: null,
    get scrollWidth() { return this.table && this.table.__unhidden ? w : 0; } };
}
// Stand-in for packets.js's TableResponsive.unhidden(table, fn); the real one
// is tested in test-packets.js.
const unhiddenCalls = [];
ctx.window.TableResponsive = {
  unhidden(table, fn) {
    unhiddenCalls.push(table);
    table.__unhidden = true;
    try { return fn(); } finally { table.__unhidden = false; }
  },
};
function row(widths) { return { children: widths.map((w) => cell(w)) }; }
function spanRow(w, span) { return { children: [cell(w, span)] }; }

// headers: content width of each header cell; authored: inline th widths the
// page's own markup sets (e.g. observers' style="width:32px"); hidden: header
// cells TableResponsive hid.
function makeTable(id, headers, rows, authored, hidden) {
  const ths = headers.map((w, i) => {
    const th = {
      style: { width: (authored && authored[i]) || '' }, dataset: {}, handles: [],
      appendChild(h) { this.handles.push(h); },
      get scrollWidth() {
        if (hidden && hidden[i] && !table.__unhidden) return 0;
        // An auto-layout cell is at least as wide as the width it was given.
        const sw = this.style.width || '';
        const given = /%$/.test(sw) ? pct(sw) / 100 * CONTAINER_W : /px$/.test(sw) ? parseFloat(sw) : 0;
        // A shown resize handle sticks out 4px past the th (right: -4px).
        const handleOut = this.handles.some((h) => h.style.display !== 'none') ? 4 : 0;
        return Math.max(w, given) + handleOut;
      },
      get offsetWidth() { return pct(this.style.width || '0') / 100 * CONTAINER_W; },
    };
    return th;
  });
  const tbody = { rows: rows.slice(), querySelectorAll(sel) { return sel === 'tr' ? this.rows : []; } };
  Object.defineProperty(tbody, 'children', { get() { return this.rows; } });
  const thead = { querySelectorAll: (sel) => (sel === 'tr:first-child th' ? ths : []) };
  const table = {
    dataset: {}, style: {}, isConnected: true,
    parentElement: { clientWidth: CONTAINER_W },
    get offsetWidth() { return CONTAINER_W; },
    querySelector: (sel) => (sel === 'thead' ? thead : sel === 'tbody' ? tbody : null),
    querySelectorAll: (sel) => (sel === 'td, th'
      ? ths.concat(...tbody.rows.map((r) => r.children))
      : sel === '.col-resize-handle' ? [].concat(...ths.map((th) => th.handles)) : []),
  };
  rows.forEach((r) => r.children.forEach((c) => { if ('table' in c) c.table = table; }));
  tables['#' + id] = table;
  return { table, ths, tbody, widths: () => ths.map((th) => pct(th.style.width)) };
}

// Columns: expand, time, path, details.
const HEADERS = [10, 30, 30, 50];
const DATA = [20, 80, 100, 300];
const dataRows = (n, w) => Array.from({ length: n }, () => row(w || DATA));
const observersOf = (t) => observers.filter((o) => o.target === t.tbody);
const close = (a, b) => a.length === b.length && a.every((x, i) => Math.abs(x - b[i]) < 1e-6);

// Reference: a table measured from 10 plain data rows.
const ref = makeTable('ref', HEADERS, dataRows(10));
makeColumnsResizable('#ref', 'k-ref');
const REF = ref.widths();
// Header-only widths (what an empty body yields).
const hdr = makeTable('hdr', HEADERS, []);
makeColumnsResizable('#hdr', 'k-hdr');
const HDR = hdr.widths();

console.log('\n=== #258 makeColumnsResizable: rows that span columns ===');

test('reference widths come from the data rows (Details is the widest column)', () => {
  assert.strictEqual(typeof makeColumnsResizable, 'function');
  assert(REF[3] > REF[0] * 5, 'Details must be far wider than expand: ' + JSON.stringify(REF));
  assert(Math.abs(REF.reduce((s, w) => s + w, 0) - 100) < 1e-6, 'percentages sum to 100');
});

test('colspan spacer / empty-state rows are not credited to column 0', () => {
  const t = makeTable('span', HEADERS, [spanRow(1200, 4)].concat(dataRows(10), [spanRow(1200, 4)]));
  makeColumnsResizable('#span', 'k-span');
  assert(close(t.widths(), REF), 'widths with spacer rows ' + JSON.stringify(t.widths()) + ' vs ' + JSON.stringify(REF));
  assert(t.widths()[0] < 5, 'expand stays narrow: ' + t.widths()[0] + '%');
});

test('a row whose cell count differs from the header is not measured', () => {
  const short = { children: [cell(900), cell(900, 3)] };
  const fewer = { children: [cell(900), cell(900)] };
  const t = makeTable('short', HEADERS, [short, fewer].concat(dataRows(10)));
  makeColumnsResizable('#short', 'k-short');
  assert(close(t.widths(), REF), JSON.stringify(t.widths()) + ' vs ' + JSON.stringify(REF));
});

test('a row with a colspan cell is not measured even when its cell count matches the header', () => {
  const odd = { children: [cell(900, 2), cell(900), cell(900), cell(900)] };
  const t = makeTable('oddspan', HEADERS, [odd].concat(dataRows(10)));
  makeColumnsResizable('#oddspan', 'k-oddspan');
  assert(close(t.widths(), REF), JSON.stringify(t.widths()) + ' vs ' + JSON.stringify(REF));
});

test('a full first body is measured once and gets no observer (no per-render work)', () => {
  assert.strictEqual(observersOf(ref).length, 0, 'no MutationObserver for a table measured from enough rows');
  const t = makeTable('full', HEADERS, [spanRow(1200, 4)].concat(dataRows(5)));
  makeColumnsResizable('#full', 'k-full');
  assert.strictEqual(observersOf(t).length, 0, '5 usable rows are enough');
});

console.log('\n=== #258 makeColumnsResizable: empty or near-empty first body ===');

test('an empty first body (only "No packets found") gets provisional header widths and one childList observer', () => {
  const t = makeTable('empty', HEADERS, [spanRow(1200, 4)]);
  makeColumnsResizable('#empty', 'k-empty');
  assert(close(t.widths(), HDR), 'provisional = header widths: ' + JSON.stringify(t.widths()));
  const obs = observersOf(t);
  assert.strictEqual(obs.length, 1, 'one observer on the tbody');
  assert(obs[0].connected && obs[0].options && obs[0].options.childList === true, JSON.stringify(obs[0].options));
  assert(!obs[0].options.subtree && !obs[0].options.attributes && !obs[0].options.characterData,
    'childList only: ' + JSON.stringify(obs[0].options));
  assert.strictEqual(t.ths.slice(0, -1).every((th) => th.handles.length === 1), true, 'resize handles added right away');
});

test('when real rows arrive the columns are measured again, once, and the observer disconnects', () => {
  const t = makeTable('later', HEADERS, [spanRow(1200, 4)]);
  makeColumnsResizable('#later', 'k-later');
  const [mo] = observersOf(t);
  t.tbody.rows = [spanRow(0, 4)].concat(dataRows(20), [spanRow(0, 4)]);
  mo.fire();
  assert(close(t.widths(), REF), 're-measured from the rows: ' + JSON.stringify(t.widths()) + ' vs ' + JSON.stringify(REF));
  assert.strictEqual(mo.connected, false, 'observer disconnected after the re-measure');
  assert(t.ths.slice(0, -1).every((th) => th.handles.length === 1), 'no second set of handles');
  // Later renders do not move the widths.
  t.tbody.rows = dataRows(20, [20, 80, 400, 30]);
  mo.fire();
  assert(close(t.widths(), REF), 'widths stay after later renders: ' + JSON.stringify(t.widths()));
  assert.strictEqual(observersOf(t).length, 1, 'no new observer');
});

test('a near-empty first body (2 rows) is provisional; 4 rows are not enough, 6 rows re-measure', () => {
  const t = makeTable('few', HEADERS, dataRows(2, [20, 80, 100, 60]));
  makeColumnsResizable('#few', 'k-few');
  const provisional = t.widths();
  const [mo] = observersOf(t);
  assert(mo && mo.connected, 'near-empty body is observed');
  t.tbody.rows = dataRows(4);
  mo.fire();
  assert(close(t.widths(), provisional), 'below the threshold nothing is measured');
  assert(mo.connected, 'still waiting');
  t.tbody.rows = [spanRow(1200, 4), spanRow(1200, 4), spanRow(1200, 4), spanRow(1200, 4), spanRow(1200, 4), spanRow(1200, 4)];
  mo.fire();
  assert(close(t.widths(), provisional), 'six colspan rows are not "real rows"');
  assert(mo.connected, 'still waiting');
  t.tbody.rows = dataRows(6);
  mo.fire();
  assert(close(t.widths(), REF), 're-measured: ' + JSON.stringify(t.widths()));
  assert.strictEqual(mo.connected, false);
});

test('a re-measure is not fed by the provisional widths', () => {
  // Provisional widths from a header-only measure give "time" ~22%; the rows
  // need less. A re-measure that keeps the old th widths would keep ~22%.
  const t = makeTable('feedback', [10, 200, 30, 50], [spanRow(1200, 4)]);
  makeColumnsResizable('#feedback', 'k-feedback');
  const before = t.widths();
  const [mo] = observersOf(t);
  t.tbody.rows = dataRows(10, [20, 80, 100, 600]);
  mo.fire();
  const after = t.widths();
  const r = makeTable('feedback-ref', [10, 200, 30, 50], dataRows(10, [20, 80, 100, 600]));
  makeColumnsResizable('#feedback-ref', 'k-feedback-ref');
  assert(close(after, r.widths()), 'same as a first measure of those rows: ' + JSON.stringify(after) + ' vs ' + JSON.stringify(r.widths()) + ' (provisional ' + JSON.stringify(before) + ')');
});

test('a th width set by the page markup counts in the first measure and again in the re-measure', () => {
  const authored = ['60px', '', '', ''];
  const r = makeTable('authored-ref', HEADERS, dataRows(10), authored);
  makeColumnsResizable('#authored-ref', 'k-authored-ref');
  assert(r.widths()[0] > REF[0] + 2, 'the 60px header widens column 0 as before: ' + JSON.stringify(r.widths()));
  const t = makeTable('authored', HEADERS, [spanRow(1200, 4)], authored);
  makeColumnsResizable('#authored', 'k-authored');
  const [mo] = observersOf(t);
  t.tbody.rows = dataRows(10);
  mo.fire();
  assert(close(t.widths(), r.widths()), 're-measure with the authored width: ' + JSON.stringify(t.widths()) + ' vs ' + JSON.stringify(r.widths()));
});

test('columns TableResponsive hid are measured as if shown, as at the first measure', () => {
  // A re-measure runs before TableResponsive.register()'s own observer has
  // marked the new cells; the measure lifts the hiding so header and rows agree.
  const rows = Array.from({ length: 10 }, () => ({ children: [cell(20), hiddenCell(80), cell(100), cell(300)] }));
  const t = makeTable('responsive', HEADERS, rows, null, [false, true, false, false]);
  makeColumnsResizable('#responsive', 'k-responsive');
  assert(close(t.widths(), REF), 'hidden column measured like a shown one: ' + JSON.stringify(t.widths()) + ' vs ' + JSON.stringify(REF));
  assert(unhiddenCalls.includes(t.table), 'measured inside TableResponsive.unhidden');
  // The re-measure too.
  const e = makeTable('responsive-later', HEADERS, [spanRow(1200, 4)], null, [false, true, false, false]);
  makeColumnsResizable('#responsive-later', 'k-responsive-later');
  const [mo] = observersOf(e);
  e.tbody.rows = Array.from({ length: 10 }, () => ({ children: [cell(20), hiddenCell(80), cell(100), cell(300)] }));
  e.tbody.rows.forEach((r) => { r.children[1].table = e.table; });
  mo.fire();
  assert(close(e.widths(), REF), 're-measured with the hiding lifted: ' + JSON.stringify(e.widths()));
});

console.log('\n=== #258 makeColumnsResizable: saved widths ===');

test('valid saved widths are applied as is: fixed layout, no measure, no observer', () => {
  store['k-saved'] = JSON.stringify([5, 20, 25, 50]);
  const t = makeTable('saved', HEADERS, [spanRow(1200, 4)]);
  makeColumnsResizable('#saved', 'k-saved');
  assert.deepStrictEqual(t.widths(), [5, 20, 25, 50]);
  assert.strictEqual(t.table.style.tableLayout, 'fixed');
  assert.strictEqual(observersOf(t).length, 0, 'saved widths need no re-measure');
  assert(t.ths.slice(0, -1).every((th) => th.handles.length === 1), 'handles still added');
});

test('widths saved while the table is provisional (user dragged a handle) are not re-measured', () => {
  const t = makeTable('dragged', HEADERS, [spanRow(1200, 4)]);
  makeColumnsResizable('#dragged', 'k-dragged');
  const [mo] = observersOf(t);
  t.ths[0].style.width = '7%'; t.ths[1].style.width = '23%'; t.ths[2].style.width = '30%'; t.ths[3].style.width = '40%';
  store['k-dragged'] = JSON.stringify([7, 23, 30, 40]);
  t.tbody.rows = dataRows(20);
  mo.fire();
  assert.deepStrictEqual(t.widths(), [7, 23, 30, 40], 'the user\'s widths stay');
  assert.strictEqual(mo.connected, false, 'nothing left to wait for');
});

console.log('\n=== #258 makeColumnsResizable: lifecycle ===');

test('calling again on the same table is a no-op (no second measure, no second observer)', () => {
  const t = makeTable('again', HEADERS, [spanRow(1200, 4)]);
  makeColumnsResizable('#again', 'k-again');
  makeColumnsResizable('#again', 'k-again');
  assert.strictEqual(observersOf(t).length, 1);
  assert(t.ths.slice(0, -1).every((th) => th.handles.length === 1));
});

test('a table removed from the page stops waiting without measuring', () => {
  const t = makeTable('gone', HEADERS, [spanRow(1200, 4)]);
  makeColumnsResizable('#gone', 'k-gone');
  const provisional = t.widths();
  const [mo] = observersOf(t);
  t.table.isConnected = false;
  t.tbody.rows = dataRows(20);
  mo.fire();
  assert(close(t.widths(), provisional), 'not measured');
  assert.strictEqual(mo.connected, false, 'disconnected');
});

console.log(`\n${passed} passed, ${failed} failed`);
process.exit(failed === 0 ? 0 : 1);
