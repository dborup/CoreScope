/* test-issue-180-packets-detail-close.js — #180
 *
 * closeDetailPanel() in public/packets.js closes the packet detail. Clear
 * Filters calls it (#180 item 2: Clear leaves the detail), so with no row
 * selected it must not re-render the rows: Clear reloads them itself.
 * Escape calls it too, and at ≤640 px it must close the mobile bottom sheet
 * (#180 item 6: it only reset the desktop pane).
 *
 * Runs the REAL closeDetailPanel() from public/packets.js inside a
 * vm.createContext sandbox with a small fake DOM.
 */
'use strict';

const vm = require('vm');
const fs = require('fs');
const path = require('path');
const assert = require('assert');

console.log('--- test-issue-180-packets-detail-close.js ---');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log(`  ✅ ${name}`); }
  catch (e) { failed++; console.log(`  ❌ ${name}: ${e.message}`); }
}

const SRC = fs.readFileSync(path.join(__dirname, 'public/packets.js'), 'utf8');

// Source of `function <name>(…) {…}` through its matching '}'.
function extractFunction(name) {
  const start = SRC.indexOf('function ' + name + '(');
  assert(start !== -1, name + ' not found in public/packets.js');
  const open = SRC.indexOf('{', start);
  let depth = 0;
  for (let i = open; i < SRC.length; i++) {
    if (SRC[i] === '{') depth++;
    else if (SRC[i] === '}' && --depth === 0) return SRC.slice(start, i + 1);
  }
  throw new Error('unbalanced braces after ' + name);
}

function makeEl(id, classes) {
  const set = new Set(classes || []);
  return {
    id, innerHTML: '',
    classList: {
      add: (c) => set.add(c), remove: (c) => set.delete(c), contains: (c) => set.has(c),
    },
    closest: () => null,
  };
}

// opts.pane: the desktop pane is open (not .empty); opts.selectedId;
// opts.sheet: the mobile sheet exists ('open' or 'closed').
function makeSandbox(opts) {
  opts = opts || {};
  const els = { pktRight: makeEl('pktRight', opts.pane ? [] : ['empty']) };
  if (opts.sheet) els.mobileDetailSheet = makeEl('mobileDetailSheet', ['mobile-detail-sheet'].concat(opts.sheet === 'open' ? ['open'] : []));
  const ctx = {
    document: { getElementById: (id) => els[id] || null },
    PANEL_CLOSE_HTML: '<button class="panel-close-btn"></button>',
    __renders: 0,
  };
  vm.createContext(ctx);
  vm.runInContext(
    'var selectedId = ' + JSON.stringify(opts.selectedId == null ? null : opts.selectedId) + ';\n' +
    'function renderTableRows() { __renders++; }\n' + extractFunction('closeDetailPanel'),
    ctx, { filename: 'packets.js (#180 extract)' });
  return {
    ctx, els,
    close() { vm.runInContext('closeDetailPanel()', ctx); },
    selectedId() { return vm.runInContext('selectedId', ctx); },
  };
}

test('desktop pane open with a selected row: closes the pane, clears the selection, re-renders once', () => {
  const s = makeSandbox({ pane: true, selectedId: 42 });
  s.close();
  assert(s.els.pktRight.classList.contains('empty'), 'pane not emptied');
  assert.strictEqual(s.selectedId(), null);
  assert.strictEqual(s.ctx.__renders, 1);
});

test('no row selected (e.g. Clear Filters on the list): no extra render', () => {
  const s = makeSandbox({ pane: false, selectedId: null });
  s.close();
  assert.strictEqual(s.ctx.__renders, 0, 'rows re-rendered with nothing selected');
});

test('≤640 px: closes the open mobile detail sheet and clears the selection (#180 item 6)', () => {
  const s = makeSandbox({ pane: false, selectedId: 42, sheet: 'open' });
  s.close();
  assert(!s.els.mobileDetailSheet.classList.contains('open'), 'mobile sheet still open');
  assert.strictEqual(s.selectedId(), null);
  assert.strictEqual(s.ctx.__renders, 1, 'selection highlight not re-rendered');
});

test('no mobile sheet in the DOM (desktop, or never opened): no error', () => {
  const s = makeSandbox({ pane: true, selectedId: 1 });
  s.close();
  assert(s.els.pktRight.classList.contains('empty'));
});

console.log(`\n${passed} passed, ${failed} failed`);
if (failed > 0) process.exit(1);
console.log('All tests passed ✅');
