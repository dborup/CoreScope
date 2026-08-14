'use strict';
// Isolated, DOM-free probe for the wait condition used by the pubkey-prefix
// narrowing step of test-nodes-export-e2e.js.
//
// Regression covered: the previous wait condition was
//   button.textContent !== originalLabel
// which times out whenever the dataset already had exactly ONE exportable
// contact before searching -- the button reads "Export JSON (1)" both
// before and after narrowing to that same contact's own prefix, so the
// label never changes and the 15s wait always expired, even though the
// outcome (exactly the target contact, alone) was already correct.
//
// The fixed predicate (mirrored here without a live DOM/browser -- Chromium
// is not installed in this environment, see the E2E file's header) waits
// for the actual target STATE instead of a delta from the prior state:
//   1. the search input holds the prefix we typed
//   2. the export button is enabled
//   3. the export button reads exactly "Export JSON (1)"
//
// This is provably correct for both cases test-nodes-export-e2e.js can hit:
// starting from exactly 1 exportable contact (state is already the target
// state -- resolves immediately, no waiting for a change that won't come)
// and starting from N>1 (state only becomes the target state once the
// debounced search + re-render actually narrows the set).
//
// Keep this in sync with the inline predicate inside the
// page.waitForFunction(...) call in test-nodes-export-e2e.js if either
// changes -- see the comment there pointing back here.
const assert = require('assert');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}

// Mirrors the exact three checks inside test-nodes-export-e2e.js's
// page.waitForFunction(function (expectedPrefix) { ... }, prefix, ...)
// callback, rewritten against a plain {inputValue, btnDisabled, btnText}
// state object instead of reading `document` directly, so it can run here
// under plain Node with no browser.
function isNarrowedToOneContact(state, expectedPrefix) {
  if (!state) return false;
  if (state.inputValue !== expectedPrefix) return false;
  if (state.btnDisabled) return false;
  return state.btnText === 'Export JSON (1)';
}

const PREFIX = 'efef7943505052b47f180948';

console.log('=== Regression: dataset already has exactly 1 exportable contact ===');
test('initial count=1, search by that contact\'s own prefix: label stays "Export JSON (1)" unchanged -- predicate is true immediately, no timeout', () => {
  // This is the exact scenario that hung: nothing about the visible state
  // changes across the search, because there was only ever one match.
  const stateBeforeSearch = { inputValue: '', btnDisabled: false, btnText: 'Export JSON (1)' };
  const stateAfterSearchSettles = { inputValue: PREFIX, btnDisabled: false, btnText: 'Export JSON (1)' };

  assert.strictEqual(isNarrowedToOneContact(stateBeforeSearch, PREFIX), false,
    'before the prefix is actually typed, the predicate must not fire on the input value alone');
  assert.strictEqual(isNarrowedToOneContact(stateAfterSearchSettles, PREFIX), true,
    'once the input holds the prefix and the button already reads "Export JSON (1)" (unchanged), the predicate must resolve true -- it must NOT require the label to differ from its prior value');
});

test('a real waitForFunction-style poll loop over an UNCHANGING state resolves on the first tick (would not time out)', () => {
  // Simulates what Playwright's waitForFunction does: poll the predicate
  // repeatedly against the current DOM state until it returns true or the
  // timeout elapses. Here the "DOM" never changes between polls (exactly
  // the N=1 regression scenario), which is exactly what made the OLD
  // (label !== priorLabel) condition loop forever until timeout.
  const constantState = { inputValue: PREFIX, btnDisabled: false, btnText: 'Export JSON (1)' };
  let ticks = 0;
  let resolved = false;
  for (let i = 0; i < 50; i++) { // stand-in for repeated polls up to a 15s timeout
    ticks++;
    if (isNarrowedToOneContact(constantState, PREFIX)) { resolved = true; break; }
  }
  assert.strictEqual(resolved, true, 'predicate must resolve against a state that never changes');
  assert.strictEqual(ticks, 1, 'must resolve on the very first poll -- no need to wait for any state transition at all');
});

console.log('\n=== Predicate correctly still WAITS when the state has not settled yet (N>1 narrowing case) ===');
test('input not yet updated (mid-typing / fill() has not landed) → false', () => {
  assert.strictEqual(isNarrowedToOneContact({ inputValue: '', btnDisabled: false, btnText: 'Export JSON (1)' }, PREFIX), false);
  assert.strictEqual(isNarrowedToOneContact({ inputValue: PREFIX.slice(0, 4), btnDisabled: false, btnText: 'Export JSON (1)' }, PREFIX), false);
});
test('debounced search has not fired yet — button still shows the old, larger count → false', () => {
  assert.strictEqual(isNarrowedToOneContact({ inputValue: PREFIX, btnDisabled: false, btnText: 'Export JSON (5)' }, PREFIX), false);
});
test('button transiently disabled (e.g. mid re-render) → false even if the stale text already reads "(1)"', () => {
  assert.strictEqual(isNarrowedToOneContact({ inputValue: PREFIX, btnDisabled: true, btnText: 'Export JSON (1)' }, PREFIX), false);
});
test('narrowed to zero (no match) → false, never satisfies the exactly-one target', () => {
  assert.strictEqual(isNarrowedToOneContact({ inputValue: PREFIX, btnDisabled: true, btnText: 'Export JSON' }, PREFIX), false);
});
test('missing DOM elements (null state) → false', () => {
  assert.strictEqual(isNarrowedToOneContact(null, PREFIX), false);
  assert.strictEqual(isNarrowedToOneContact(undefined, PREFIX), false);
});

console.log('\n=== N>1 narrowing still resolves once the real target state is reached ===');
test('poll loop that transitions from N=5 to N=1 over several ticks eventually resolves true', () => {
  const states = [
    { inputValue: '', btnDisabled: false, btnText: 'Export JSON (5)' },       // not typed yet
    { inputValue: PREFIX, btnDisabled: false, btnText: 'Export JSON (5)' },   // typed, debounce pending
    { inputValue: PREFIX, btnDisabled: false, btnText: 'Export JSON (5)' },   // still pending
    { inputValue: PREFIX, btnDisabled: false, btnText: 'Export JSON (1)' },   // settled
  ];
  let resolvedAtTick = -1;
  for (let i = 0; i < states.length; i++) {
    if (isNarrowedToOneContact(states[i], PREFIX)) { resolvedAtTick = i; break; }
  }
  assert.strictEqual(resolvedAtTick, 3, 'must resolve exactly once the settled state is reached, not before');
});

console.log('\n' + passed + ' passed, ' + failed + ' failed');
process.exit(failed ? 1 : 0);
