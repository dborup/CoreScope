/**
 * Source-grep tests for the "View Path" button added to the observer
 * detail page's "Recent Packets" table (public/observer-detail.js,
 * renderRecentPackets).
 *
 * The whole table row is already a click-to-navigate target
 * (#/packets/<hash>, see PR #1539 / issue #209). Adding a nested button
 * for the packet-path-map.js modal inside that same row is a real
 * correctness risk if the click/keydown handlers don't special-case it
 * first: a click on the button would both open the modal AND navigate
 * the row away out from under it. This mirrors test-xss-escape-sinks.js's
 * existing style of testing renderRecentPackets via source inspection
 * rather than full DOM execution (the file's own established pattern for
 * this function).
 */
'use strict';
const fs = require('fs');
const assert = require('assert');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

const src = fs.readFileSync('public/observer-detail.js', 'utf8');

console.log('\n=== observer-detail.js: Recent Packets "View Path" button ===');

test('renderRecentPackets emits a View Path button carrying the packet hash', () => {
  assert.ok(src.includes('data-view-path="${escapeHtml(p.hash)}"'),
    'each row with a hash must render a [data-view-path] button');
});

test('the View Path button is gated on p.hash, matching the row itself', () => {
  const idx = src.indexOf('const viewPathBtn = p.hash ?');
  assert.ok(idx > -1, 'viewPathBtn must be built conditionally on p.hash');
});

test('the click handler checks [data-view-path] BEFORE the row-navigate fallthrough', () => {
  const clickHandlerStart = src.indexOf("el.addEventListener('click', function (e) {");
  assert.ok(clickHandlerStart > -1, 'delegated click handler not found');
  const snippet = src.slice(clickHandlerStart, clickHandlerStart + 500);
  const viewPathIdx = snippet.indexOf("closest('[data-view-path]')");
  const rowIdx = snippet.indexOf("closest('tr[data-action=\"navigate\"]')");
  assert.ok(viewPathIdx > -1 && rowIdx > -1, 'both checks must be present in the click handler');
  assert.ok(viewPathIdx < rowIdx, 'the [data-view-path] check must come first, before the row-navigate fallthrough');
});

test('clicking the View Path button stops propagation so it cannot also trigger row navigation', () => {
  const clickHandlerStart = src.indexOf("el.addEventListener('click', function (e) {");
  const snippet = src.slice(clickHandlerStart, clickHandlerStart + 500);
  const viewPathBlock = snippet.slice(snippet.indexOf("closest('[data-view-path]')"), snippet.indexOf('return;') + 'return;'.length);
  assert.ok(viewPathBlock.includes('e.stopPropagation()'),
    'the View Path branch must call e.stopPropagation() -- without it, a click also bubbles to whatever assumes the row itself was clicked');
  assert.ok(viewPathBlock.includes('window.PacketPathMap'),
    'the View Path branch must call window.PacketPathMap.open(...)');
});

test('the keydown handler skips row-navigation when focus is on the View Path button', () => {
  const keydownStart = src.indexOf("el.addEventListener('keydown', function (e) {");
  assert.ok(keydownStart > -1, 'keydown handler not found');
  const snippet = src.slice(keydownStart, keydownStart + 400);
  const guardIdx = snippet.indexOf("e.target.closest('[data-view-path]')");
  const rowIdx = snippet.indexOf("closest('tr[data-action=\"navigate\"]')");
  assert.ok(guardIdx > -1 && rowIdx > -1, 'both checks must be present in the keydown handler');
  assert.ok(guardIdx < rowIdx,
    'the View Path guard must run before the row-navigate check -- otherwise pressing Enter/Space while ' +
    'focused on the button ALSO navigates the row away (double-activation), on top of the button\'s own ' +
    'native Enter/Space-triggers-click behavior');
});

// ===== SUMMARY =====
console.log(`\n${'='.repeat(40)}`);
console.log(`observer-detail.js View Path tests: ${passed} passed, ${failed} failed`);
console.log(`${'='.repeat(40)}\n`);
if (failed > 0) process.exit(1);
