/* test-issue-125-live-toggles-wiring.js — structural guard for #125.
 *
 * The persisted Live view toggles must be restored and wired before init()
 * awaits anything (see test-issue-125-live-toggles-early-e2e.js for the
 * behaviour). This fails if the wiring call moves behind an await, if a
 * toggle is dropped from the wiring table, or if a second change listener
 * for one of these toggles is added elsewhere in live.js.
 */
'use strict';
const fs = require('fs');
const assert = require('assert');

console.log('--- test-issue-125-live-toggles-wiring.js ---');
let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

const SRC = fs.readFileSync(__dirname + '/public/live.js', 'utf8');
const IDS = ['liveHeatToggle', 'liveGhostToggle', 'liveRealisticToggle', 'liveColorHashToggle',
  'liveFavoritesToggle', 'liveForeignToggle', 'liveMultibyteToggle', 'liveMatrixToggle', 'liveMatrixRainToggle'];

function blockAt(src, start) {
  const open = src.indexOf('{', start);
  let depth = 0;
  for (let i = open; i < src.length; i++) {
    if (src[i] === '{') depth++;
    else if (src[i] === '}') { depth--; if (depth === 0) return src.slice(start, i + 1); }
  }
  throw new Error('unbalanced block');
}
function fn(name) {
  const i = SRC.search(new RegExp('function ' + name + '\\s*\\('));
  assert(i !== -1, name + '() not found in live.js');
  return blockAt(SRC, i);
}

test('init() calls wireLiveControls() before its first await', () => {
  const init = fn('init');
  const wire = init.indexOf('wireLiveControls();');
  const firstAwait = init.search(/\bawait\b/);
  assert(wire !== -1, 'init() does not call wireLiveControls()');
  assert(firstAwait !== -1, 'init() has no await any more; update this test');
  assert(wire < firstAwait, 'wireLiveControls() runs after the first await in init()');
  assert(init.indexOf('app.innerHTML') < wire, 'wireLiveControls() must run after the markup exists');
});

test('wireLiveControls() restores and wires every persisted view toggle', () => {
  const wire = fn('wireLiveControls');
  for (const id of IDS) assert(wire.includes("'" + id + "'"), id + ' is not in wireLiveControls()');
});

test('no toggle gets a change listener outside wireLiveControls()', () => {
  const wire = fn('wireLiveControls');
  const rest = SRC.replace(wire, '');
  for (const id of IDS) {
    // Outside the wiring the id may only be looked up without a listener
    // (e.g. the Heat/Matrix interlock).
    const re = new RegExp("getElementById\\('" + id + "'\\)\\s*\\.addEventListener|const (\\w+) = document\\.getElementById\\('" + id + "'\\);[\\s\\S]{0,400}?\\1\\.addEventListener\\('change'", 'g');
    assert(!re.test(rest), id + ' gets a change listener outside wireLiveControls()');
  }
});

test('the heat layer is built after init only when Heat is enabled', () => {
  const init = fn('init');
  assert(!/await loadNodes\(\);\s*showHeatMap\(\);/.test(init), 'init() builds the heat layer unconditionally after loadNodes()');
});

console.log(`\n${passed} passed, ${failed} failed`);
if (failed > 0) process.exit(1);
