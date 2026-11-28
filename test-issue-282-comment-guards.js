/**
 * #282 comment/text fixes (points 2, 4, 5). These are documentation corrections
 * with no runtime surface, so the regression guard is on the source text itself:
 * each assertion is red against the pre-#282 wording and green once corrected,
 * and re-introducing the old wording (the mutant) turns it red again. The
 * behaviours the comments describe are exercised elsewhere:
 *   - the #866 deep-link form (point 2) by test-issue-147-packets-url-*;
 *   - the Time-handle resize (point 4) by test-issue-258-column-widths-e2e.js;
 *   - a sparse first body creating one MutationObserver (point 5) by
 *     test-issue-258-column-widths.js ("empty"/"near-empty first body" cases).
 *
 * Usage: node test-issue-282-comment-guards.js
 */
'use strict';
const fs = require('fs');
const assert = require('assert');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

const read = (f) => fs.readFileSync(f, 'utf8');

console.log('\n=== #282 comment/text fixes ===');

// --- Point 2: the #866 deep-link form in packets.js ------------------------
test('point 2: packets.js names the #866 deep link as #/packets/<hash>?obs=<id>', () => {
  const src = read('public/packets.js');
  assert(!src.includes('#/packets/<hash>/<obs>'),
    'the stale #/packets/<hash>/<obs> form is still in packets.js');
  assert(src.includes('#/packets/<hash>?obs=<id>'),
    'the corrected #/packets/<hash>?obs=<id> form is missing from packets.js');
});

// --- Point 4: the resize-handle header in the #258 column-widths E2E --------
test('point 4: the #258 E2E header describes dragging the Time handle right, not the Path handle left', () => {
  const src = read('test-issue-258-column-widths-e2e.js');
  const header = src.slice(0, src.indexOf('*/'));
  assert(!/Path handle is dragged left/.test(header),
    'the wrong "Path handle is dragged left" description is still in the header');
  assert(/Time handle is dragged right/.test(header),
    'the header should describe dragging the Time handle right (what the test does)');
});

// --- Point 5: the "never create an observer" claim in the #258 unit test ----
test('point 5: the #258 column-widths header does not claim analytics tables never create an observer', () => {
  const src = read('test-issue-258-column-widths.js');
  const header = src.slice(0, src.indexOf('*/'));
  // The correction makes the trigger the usable-row count of the first render,
  // and spells out that analytics tables are not exempt.
  assert(/analytics included/.test(header),
    'the header should state the row-count rule applies to any table, analytics included');
  assert(/does create the one-shot observer/.test(header),
    'the header should state that a sparse first render does create the observer');
});

console.log(`\n${passed} passed, ${failed} failed`);
process.exit(failed === 0 ? 0 : 1);
