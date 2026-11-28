/* test-table-sort.js — Unit tests for TableSort utility */
'use strict';

const vm = require('vm');
const fs = require('fs');
const path = require('path');
const assert = require('assert');

let passed = 0;
let failed = 0;

function test(name, fn) {
  try {
    fn();
    passed++;
    console.log(`  ✓ ${name}`);
  } catch (e) {
    failed++;
    console.log(`  ✗ ${name}`);
    console.log(`    ${e.message}`);
  }
}

// ── Minimal DOM (#189: jsdom is not a dependency) ─────────────────────────
// Just enough of the element API that table-sort.js touches: attributes,
// classList, style, child lists with real moves on appendChild, remove(),
// listeners + click(), cells, textContent and a small selector engine
// (tag, .class, tag[attr], tag[attr="v"]; descendants, document order).
class FakeEl {
  constructor(tag) {
    this.tagName = tag.toUpperCase();
    this.children = [];
    this.parentNode = null;
    this.attrs = {};
    this.style = {};
    this.listeners = {};
    this.text = '';
    const el = this;
    this.classList = {
      add(c) { const s = el._classes(); if (!s.includes(c)) el.className = s.concat(c).join(' '); },
      remove(c) { el.className = el._classes().filter((x) => x !== c).join(' '); },
      contains(c) { return el._classes().includes(c); },
    };
  }
  _classes() { return (this.attrs.class || '').split(/\s+/).filter(Boolean); }
  get className() { return this.attrs.class || ''; }
  set className(v) { this.attrs.class = String(v); }
  getAttribute(k) { return Object.prototype.hasOwnProperty.call(this.attrs, k) ? this.attrs[k] : null; }
  setAttribute(k, v) { this.attrs[k] = String(v); }
  removeAttribute(k) { delete this.attrs[k]; }
  appendChild(child) {
    if (child.parentNode) child.remove();
    child.parentNode = this;
    this.children.push(child);
    return child;
  }
  remove() {
    if (!this.parentNode) return;
    const sibs = this.parentNode.children;
    sibs.splice(sibs.indexOf(this), 1);
    this.parentNode = null;
  }
  // innerHTML is only ever assigned (the sort arrow); keep the markup as text.
  set innerHTML(html) { this.children = []; this.text = String(html); }
  get innerHTML() { return this.text + this.children.map((c) => c.innerHTML).join(''); }
  get textContent() { return this.text + this.children.map((c) => c.textContent).join(''); }
  get cells() { return this.children.filter((c) => c.tagName === 'TD' || c.tagName === 'TH'); }
  addEventListener(type, fn) { (this.listeners[type] = this.listeners[type] || []).push(fn); }
  removeEventListener(type, fn) { this.listeners[type] = (this.listeners[type] || []).filter((f) => f !== fn); }
  click() { (this.listeners.click || []).slice().forEach((fn) => fn({ type: 'click', preventDefault() {} })); }
  _descendants() { return this.children.flatMap((c) => [c, ...c._descendants()]); }
  querySelectorAll(sel) { return this._descendants().filter((el) => matches(el, sel)); }
  querySelector(sel) { return this.querySelectorAll(sel)[0] || null; }
}

function matches(el, sel) {
  const m = /^([a-z]*)(?:\.([\w-]+))?(?:\[([\w-]+)(?:="([^"]*)")?\])?$/.exec(sel);
  if (!m) throw new Error('FakeEl: unsupported selector ' + sel);
  const [, tag, cls, attr, val] = m;
  if (tag && el.tagName !== tag.toUpperCase()) return false;
  if (cls && !el.classList.contains(cls)) return false;
  if (attr && el.getAttribute(attr) == null) return false;
  if (val !== undefined && el.getAttribute(attr) !== val) return false;
  return true;
}

function createDOM(table) {
  const body = new FakeEl('body');
  if (table) body.appendChild(table);
  const store = {};
  const document = {
    body,
    createElement: (tag) => new FakeEl(tag),
    getElementById: (id) => body._descendants().find((el) => el.getAttribute('id') === id) || null,
    querySelector: (sel) => body.querySelector(sel),
    querySelectorAll: (sel) => body.querySelectorAll(sel),
  };
  const window = { document };
  const ctx = {
    window, document,
    localStorage: {
      getItem: (k) => (k in store ? store[k] : null),
      setItem: (k, v) => { store[k] = String(v); },
    },
  };
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'public', 'table-sort.js'), 'utf8'), ctx);
  return { window };
}

function makeTable(headers, rows) {
  // headers: [{key, type?, label}], rows: [[value, ...]]
  const el = (tag, attrs, text) => {
    const e = new FakeEl(tag);
    for (const k in attrs || {}) e.setAttribute(k, attrs[k]);
    if (text != null) e.text = String(text);
    return e;
  };
  const table = el('table', { id: 't' });
  const thead = table.appendChild(el('thead'));
  const headRow = thead.appendChild(el('tr'));
  for (const h of headers) {
    headRow.appendChild(el('th', Object.assign({ 'data-sort-key': h.key }, h.type ? { 'data-type': h.type } : {}), h.label || h.key));
  }
  const tbody = table.appendChild(el('tbody'));
  for (const row of rows) {
    const tr = tbody.appendChild(el('tr'));
    for (const val of row) {
      if (typeof val === 'object' && val !== null) tr.appendChild(el('td', { 'data-value': val.dataValue }, val.text || ''));
      else tr.appendChild(el('td', { 'data-value': val }, val));
    }
  }
  return table;
}

function getColumnValues(dom, colIndex) {
  const rows = dom.window.document.querySelector('tbody').children;
  return rows.map(r => r.cells[colIndex].getAttribute('data-value'));
}

// The arrow table-sort.js renders: since #1648 M2 a Phosphor caret sprite
// (was ▲ / ▼).
const caret = (dir) => '#ph-caret-' + dir + '"';

console.log('\nTableSort — comparators');

test('text comparator: basic alphabetical', () => {
  const cmp = (() => {
    const dom = createDOM(null);
    return dom.window.TableSort.comparators.text;
  })();
  assert.ok(cmp('apple', 'banana') < 0);
  assert.ok(cmp('banana', 'apple') > 0);
  assert.strictEqual(cmp('same', 'same'), 0);
});

test('text comparator: null/undefined handling', () => {
  const dom = createDOM(null);
  const cmp = dom.window.TableSort.comparators.text;
  assert.strictEqual(cmp(null, null), 0);
  assert.strictEqual(cmp(undefined, undefined), 0);
});

test('numeric comparator: basic numbers', () => {
  const dom = createDOM(null);
  const cmp = dom.window.TableSort.comparators.numeric;
  assert.ok(cmp('1', '2') < 0);
  assert.ok(cmp('10', '2') > 0);
  assert.strictEqual(cmp('5', '5'), 0);
});

test('numeric comparator: NaN sorts last', () => {
  const dom = createDOM(null);
  const cmp = dom.window.TableSort.comparators.numeric;
  assert.ok(cmp('abc', '5') > 0);  // NaN > number (sorts last)
  assert.ok(cmp('5', 'abc') < 0);
  assert.strictEqual(cmp('abc', 'xyz'), 0); // both NaN
});

test('numeric comparator: negative numbers', () => {
  const dom = createDOM(null);
  const cmp = dom.window.TableSort.comparators.numeric;
  assert.ok(cmp('-10', '-5') < 0);
  assert.ok(cmp('-5', '-10') > 0);
});

test('date comparator: ISO dates', () => {
  const dom = createDOM(null);
  const cmp = dom.window.TableSort.comparators.date;
  assert.ok(cmp('2024-01-01T00:00:00Z', '2024-06-01T00:00:00Z') < 0);
  assert.ok(cmp('2024-06-01T00:00:00Z', '2024-01-01T00:00:00Z') > 0);
  assert.strictEqual(cmp('2024-01-01', '2024-01-01'), 0);
});

test('date comparator: invalid dates sort last', () => {
  const dom = createDOM(null);
  const cmp = dom.window.TableSort.comparators.date;
  assert.ok(cmp('invalid', '2024-01-01') > 0);
  assert.ok(cmp('2024-01-01', 'invalid') < 0);
});

test('dBm comparator: strips suffix', () => {
  const dom = createDOM(null);
  const cmp = dom.window.TableSort.comparators.dbm;
  assert.ok(cmp('-120 dBm', '-80 dBm') < 0);
  assert.ok(cmp('-80 dBm', '-120 dBm') > 0);
  assert.strictEqual(cmp('-95 dBm', '-95 dBm'), 0);
});

test('dBm comparator: works without suffix', () => {
  const dom = createDOM(null);
  const cmp = dom.window.TableSort.comparators.dbm;
  assert.ok(cmp('-120', '-80') < 0);
});

console.log('\nTableSort — DOM sorting');

test('sort ascending by text column', () => {
  const tableEl = makeTable(
    [{key: 'name'}],
    [['Charlie'], ['Alice'], ['Bob']]
  );
  const dom = createDOM(tableEl);
  const table = dom.window.document.getElementById('t');
  const inst = dom.window.TableSort.init(table, { defaultColumn: 'name', defaultDirection: 'asc' });
  const vals = getColumnValues(dom, 0);
  assert.deepStrictEqual(vals, ['Alice', 'Bob', 'Charlie']);
});

test('sort descending by numeric column', () => {
  const tableEl = makeTable(
    [{key: 'val', type: 'numeric'}],
    [['3'], ['1'], ['2']]
  );
  const dom = createDOM(tableEl);
  const table = dom.window.document.getElementById('t');
  dom.window.TableSort.init(table, { defaultColumn: 'val', defaultDirection: 'desc' });
  const vals = getColumnValues(dom, 0);
  assert.deepStrictEqual(vals, ['3', '2', '1']);
});

test('click toggles direction', () => {
  const tableEl = makeTable(
    [{key: 'name'}],
    [['B'], ['A'], ['C']]
  );
  const dom = createDOM(tableEl);
  const table = dom.window.document.getElementById('t');
  const inst = dom.window.TableSort.init(table, { defaultColumn: 'name', defaultDirection: 'asc' });

  // Initially ascending
  assert.deepStrictEqual(getColumnValues(dom, 0), ['A', 'B', 'C']);

  // Click same header → descending
  const th = dom.window.document.querySelector('th[data-sort-key="name"]');
  th.click();
  assert.deepStrictEqual(getColumnValues(dom, 0), ['C', 'B', 'A']);

  // Click again → ascending
  th.click();
  assert.deepStrictEqual(getColumnValues(dom, 0), ['A', 'B', 'C']);
});

console.log('\nTableSort — aria-sort attributes');

test('aria-sort set correctly on active column', () => {
  const tableEl = makeTable(
    [{key: 'a'}, {key: 'b'}],
    [['1', 'x'], ['2', 'y']]
  );
  const dom = createDOM(tableEl);
  const table = dom.window.document.getElementById('t');
  dom.window.TableSort.init(table, { defaultColumn: 'a', defaultDirection: 'asc' });

  const thA = dom.window.document.querySelector('th[data-sort-key="a"]');
  const thB = dom.window.document.querySelector('th[data-sort-key="b"]');
  assert.strictEqual(thA.getAttribute('aria-sort'), 'ascending');
  assert.strictEqual(thB.getAttribute('aria-sort'), 'none');
});

test('aria-sort updates on direction change', () => {
  const tableEl = makeTable(
    [{key: 'a'}],
    [['1'], ['2']]
  );
  const dom = createDOM(tableEl);
  const table = dom.window.document.getElementById('t');
  dom.window.TableSort.init(table, { defaultColumn: 'a', defaultDirection: 'asc' });

  const th = dom.window.document.querySelector('th[data-sort-key="a"]');
  assert.strictEqual(th.getAttribute('aria-sort'), 'ascending');

  th.click(); // toggle to desc
  assert.strictEqual(th.getAttribute('aria-sort'), 'descending');
});

test('aria-sort updates when switching columns', () => {
  const tableEl = makeTable(
    [{key: 'a'}, {key: 'b'}],
    [['1', 'x'], ['2', 'y']]
  );
  const dom = createDOM(tableEl);
  const table = dom.window.document.getElementById('t');
  dom.window.TableSort.init(table, { defaultColumn: 'a', defaultDirection: 'asc' });

  const thB = dom.window.document.querySelector('th[data-sort-key="b"]');
  thB.click(); // switch to column b

  const thA = dom.window.document.querySelector('th[data-sort-key="a"]');
  assert.strictEqual(thA.getAttribute('aria-sort'), 'none');
  assert.strictEqual(thB.getAttribute('aria-sort'), 'ascending');
});

console.log('\nTableSort — visual indicator');

test('sort arrow shows on active column', () => {
  const tableEl = makeTable(
    [{key: 'a'}],
    [['1'], ['2']]
  );
  const dom = createDOM(tableEl);
  const table = dom.window.document.getElementById('t');
  dom.window.TableSort.init(table, { defaultColumn: 'a', defaultDirection: 'asc' });

  const arrow = dom.window.document.querySelector('.sort-arrow');
  assert.ok(arrow, 'sort arrow should exist');
  assert.ok(arrow.innerHTML.includes(caret('up')), 'ascending should show the up caret');
});

test('sort arrow changes on direction toggle', () => {
  const tableEl = makeTable(
    [{key: 'a'}],
    [['1'], ['2']]
  );
  const dom = createDOM(tableEl);
  const table = dom.window.document.getElementById('t');
  dom.window.TableSort.init(table, { defaultColumn: 'a', defaultDirection: 'asc' });

  const th = dom.window.document.querySelector('th[data-sort-key="a"]');
  th.click(); // desc
  const arrow = dom.window.document.querySelector('.sort-arrow');
  assert.ok(arrow.innerHTML.includes(caret('down')), 'descending should show the down caret');
});

console.log('\nTableSort — onSort callback');

test('onSort fires with column and direction', () => {
  const tableEl = makeTable(
    [{key: 'a'}, {key: 'b'}],
    [['1', 'x'], ['2', 'y']]
  );
  const dom = createDOM(tableEl);
  const table = dom.window.document.getElementById('t');
  let called = null;
  dom.window.TableSort.init(table, {
    domReorder: false,
    onSort: function(col, dir) { called = { col, dir }; }
  });

  const th = dom.window.document.querySelector('th[data-sort-key="a"]');
  th.click();
  assert.ok(called, 'onSort should fire');
  assert.strictEqual(called.col, 'a');
  assert.strictEqual(called.dir, 'asc');
});

console.log('\nTableSort — domReorder: false');

test('domReorder: false skips DOM sorting', () => {
  const tableEl = makeTable(
    [{key: 'name'}],
    [['C'], ['A'], ['B']]
  );
  const dom = createDOM(tableEl);
  const table = dom.window.document.getElementById('t');
  dom.window.TableSort.init(table, { defaultColumn: 'name', defaultDirection: 'asc', domReorder: false });

  // DOM order should NOT change
  const vals = getColumnValues(dom, 0);
  assert.deepStrictEqual(vals, ['C', 'A', 'B']);
});

console.log('\nTableSort — destroy');

test('destroy removes event handlers and cleans up', () => {
  const tableEl = makeTable(
    [{key: 'a'}],
    [['2'], ['1']]
  );
  const dom = createDOM(tableEl);
  const table = dom.window.document.getElementById('t');
  const inst = dom.window.TableSort.init(table, { defaultColumn: 'a', defaultDirection: 'asc' });

  inst.destroy();

  const th = dom.window.document.querySelector('th[data-sort-key="a"]');
  assert.strictEqual(th.getAttribute('aria-sort'), null, 'aria-sort should be removed');
  assert.ok(!th.classList.contains('sort-active'), 'sort-active should be removed');
  assert.strictEqual(th.querySelector('.sort-arrow'), null, 'arrow should be removed');
});

console.log('\nTableSort — custom comparators');

test('custom comparator overrides built-in', () => {
  const tableEl = makeTable(
    [{key: 'val', type: 'numeric'}],
    [['3'], ['1'], ['2']]
  );
  const dom = createDOM(tableEl);
  const table = dom.window.document.getElementById('t');
  // Custom: reverse numeric
  dom.window.TableSort.init(table, {
    defaultColumn: 'val', defaultDirection: 'asc',
    comparators: { val: function(a, b) { return Number(b) - Number(a); } }
  });
  const vals = getColumnValues(dom, 0);
  assert.deepStrictEqual(vals, ['3', '2', '1']); // reversed
});

console.log('\nTableSort — date sort with data-type="date"');

test('date column sorts correctly', () => {
  const tableEl = makeTable(
    [{key: 'ts', type: 'date'}],
    [['2024-06-15T10:00:00Z'], ['2024-01-01T00:00:00Z'], ['2024-12-25T23:59:59Z']]
  );
  const dom = createDOM(tableEl);
  const table = dom.window.document.getElementById('t');
  dom.window.TableSort.init(table, { defaultColumn: 'ts', defaultDirection: 'asc' });
  const vals = getColumnValues(dom, 0);
  assert.deepStrictEqual(vals, ['2024-01-01T00:00:00Z', '2024-06-15T10:00:00Z', '2024-12-25T23:59:59Z']);
});

// Summary
console.log(`\n${passed + failed} tests, ${passed} passed, ${failed} failed\n`);
process.exit(failed > 0 ? 1 : 0);
