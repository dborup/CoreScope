/* Unit tests for compare.js reference-to-many helpers — #122 */
'use strict';
const vm = require('vm');
const fs = require('fs');
const assert = require('assert');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log(`  ✅ ${name}`); }
  catch (e) { failed++; console.log(`  ❌ ${name}: ${e.message}`); }
}

function makeSandbox() {
  const ctx = {
    window: { addEventListener: () => {}, dispatchEvent: () => {} },
    document: {
      readyState: 'complete',
      createElement: () => ({ id: '', textContent: '', innerHTML: '', addEventListener: () => {} }),
      head: { appendChild: () => {} },
      getElementById: () => null,
      querySelectorAll: () => [],
      addEventListener: () => {},
    },
    console,
    setTimeout, clearTimeout,
    location: { hash: '#/compare', href: '' },
    history: { replaceState: () => {} },
    URLSearchParams,
    Map, Set, Date, Promise,
    escapeHtml: (s) => s,
    api: () => Promise.resolve({ observers: [] }),
    CLIENT_TTL: { observers: 0, observerComparison: 0 },
    registerPage: () => {},
    timeAgo: () => '',
    payloadTypeColor: () => '',
  };
  ctx.self = ctx.window;
  return ctx;
}

const ctx = makeSandbox();
const sandbox = vm.createContext(ctx);
const compareSrc = fs.readFileSync(__dirname + '/public/compare.js', 'utf8');
vm.runInContext(compareSrc, sandbox);

console.log('\ncompare.js reference-to-many helpers (#122):');

test('formatCoveragePct, buildOtherObserverIds, sortComparisonRows are exposed on window', () => {
  assert.strictEqual(typeof sandbox.window.formatCoveragePct, 'function');
  assert.strictEqual(typeof sandbox.window.buildOtherObserverIds, 'function');
  assert.strictEqual(typeof sandbox.window.sortComparisonRows, 'function');
});

// ---- formatCoveragePct ------------------------------------------------

test('formatCoveragePct renders null as unavailable, never 0%', () => {
  const out = sandbox.window.formatCoveragePct(null);
  assert.strictEqual(out, '—');
  assert.notStrictEqual(out, '0.0%', 'a zero-denominator percentage must not read as 0%');
});

test('formatCoveragePct renders undefined the same as null', () => {
  assert.strictEqual(sandbox.window.formatCoveragePct(undefined), '—');
});

test('formatCoveragePct renders a real zero as "0.0%", distinct from unavailable', () => {
  assert.strictEqual(sandbox.window.formatCoveragePct(0), '0.0%');
});

test('formatCoveragePct formats to one decimal place', () => {
  assert.strictEqual(sandbox.window.formatCoveragePct(60), '60.0%');
  assert.strictEqual(sandbox.window.formatCoveragePct(66.666), '66.7%');
});

// ---- buildOtherObserverIds --------------------------------------------

test('buildOtherObserverIds excludes only the reference id', () => {
  const observers = [{ id: 'a' }, { id: 'ref' }, { id: 'b' }];
  const ids = sandbox.window.buildOtherObserverIds(observers, 'ref');
  assert.deepStrictEqual(ids, ['a', 'b']);
});

test('buildOtherObserverIds preserves input order (caller controls sort)', () => {
  const observers = [{ id: 'zed' }, { id: 'alpha' }, { id: 'ref' }];
  const ids = sandbox.window.buildOtherObserverIds(observers, 'ref');
  assert.deepStrictEqual(ids, ['zed', 'alpha']);
});

test('buildOtherObserverIds does not mutate its input', () => {
  const observers = [{ id: 'a' }, { id: 'ref' }];
  const copy = observers.slice();
  sandbox.window.buildOtherObserverIds(observers, 'ref');
  assert.deepStrictEqual(observers, copy);
});

test('buildOtherObserverIds returns everything when the reference is absent', () => {
  const observers = [{ id: 'a' }, { id: 'b' }];
  assert.deepStrictEqual(sandbox.window.buildOtherObserverIds(observers, 'nope'), ['a', 'b']);
});

// ---- sortComparisonRows ------------------------------------------------

function row(id, name, total, shared, referenceOnly, otherOnly, refPct, otherPct) {
  return {
    observerId: id, observerName: name, total: total, shared: shared,
    referenceOnly: referenceOnly, otherOnly: otherOnly,
    referenceCoveragePct: refPct, otherCoveragePct: otherPct,
  };
}

test('sortComparisonRows sorts by name, case-insensitive, ascending', () => {
  const rows = [row('z', 'Zed', 1, 1, 0, 0, 100, 100), row('a', 'alpha', 1, 1, 0, 0, 100, 100), row('b', 'Beta', 1, 1, 0, 0, 100, 100)];
  const sorted = sandbox.window.sortComparisonRows(rows, 'name', 'asc');
  assert.deepStrictEqual(sorted.map(r => r.observerId), ['a', 'b', 'z']);
});

test('sortComparisonRows reverses with direction=desc', () => {
  const rows = [row('a', 'Alpha', 1, 1, 0, 0, 100, 100), row('z', 'Zed', 1, 1, 0, 0, 100, 100)];
  const sorted = sandbox.window.sortComparisonRows(rows, 'name', 'desc');
  assert.deepStrictEqual(sorted.map(r => r.observerId), ['z', 'a']);
});

test('sortComparisonRows sorts numerically on a count column', () => {
  const rows = [row('a', 'A', 50, 10, 40, 5, 20, 20), row('b', 'B', 5, 1, 4, 0, 20, 20), row('c', 'C', 20, 3, 17, 1, 15, 15)];
  const sorted = sandbox.window.sortComparisonRows(rows, 'total', 'asc');
  assert.deepStrictEqual(sorted.map(r => r.observerId), ['b', 'c', 'a']);
});

test('sortComparisonRows does not mutate the input array', () => {
  const rows = [row('z', 'Zed', 1, 1, 0, 0, 100, 100), row('a', 'Alpha', 1, 1, 0, 0, 100, 100)];
  const original = rows.slice();
  sandbox.window.sortComparisonRows(rows, 'name', 'asc');
  assert.deepStrictEqual(rows, original, 'sortComparisonRows must return a copy, not sort in place');
});

// This is the mutant-catching case for the #122 "unavailable, not 0%"
// requirement as it propagates into sorting: a null percentage (zero
// denominator) must sort as the WORST/last value in both directions, never
// silently compare as if it were numeric 0 (which would place it ahead of
// real small-but-nonzero percentages on an ascending sort).
test('sortComparisonRows sorts a null (unavailable) percentage last in both directions', () => {
  const rows = [
    row('has-data', 'HasData', 10, 1, 9, 0, 10, 10),
    row('unavailable', 'Unavailable', 0, 0, 0, 0, null, null),
  ];
  const asc = sandbox.window.sortComparisonRows(rows, 'otherCoveragePct', 'asc');
  assert.strictEqual(asc[0].observerId, 'has-data', 'null must not sort before a real 10% on ascending');
  assert.strictEqual(asc[1].observerId, 'unavailable');

  const desc = sandbox.window.sortComparisonRows(rows, 'otherCoveragePct', 'desc');
  assert.strictEqual(desc[0].observerId, 'has-data', 'null must still sort last on descending');
  assert.strictEqual(desc[1].observerId, 'unavailable');
});

test('sortComparisonRows breaks ties by observerId for a deterministic order', () => {
  const rows = [row('b', 'Same', 1, 1, 0, 0, 100, 100), row('a', 'Same', 1, 1, 0, 0, 100, 100)];
  const sorted = sandbox.window.sortComparisonRows(rows, 'name', 'asc');
  assert.deepStrictEqual(sorted.map(r => r.observerId), ['a', 'b']);
});

test('resolveRememberedReference drops a stored id that is no longer an eligible observer', () => {
  const obs = [{ id: 'a' }, { id: 'b' }];
  const f = sandbox.window.resolveRememberedReference;
  assert.strictEqual(f('gone', 'a', obs), 'gone', 'an explicit URL ref is kept even if unknown (server reports 404)');
  assert.strictEqual(f(null, 'a', obs), 'a', 'a remembered id that still exists is kept');
  assert.strictEqual(f(null, 'gone', obs), null, 'a remembered id for a vanished observer is dropped');
  assert.strictEqual(f(null, null, obs), null);
});

console.log(`\n  ${passed} passed, ${failed} failed`);
process.exit(failed > 0 ? 1 : 0);
