/**
 * #322 comment fixes (points 3 and 4), follow-ups to #313 (#282). These are
 * documentation corrections with no runtime surface, so the regression guard is
 * on the source text itself: each assertion is red against the wrong wording and
 * green once corrected, and re-introducing the old wording (the mutant) turns it
 * red again. The behaviours the comments describe are exercised elsewhere:
 *   - the pktEsc once-per-visit leak (point 3) by test-issue-282-pktesc-listener.js;
 *   - normalizeObservedPathHashSizes()'s real callers (point 4) by
 *     test-channels-observed-path-hash-size.js (normalize/union) and the
 *     dedup/merge paths in channels.js.
 *
 * Usage: node test-issue-322-comment-guards.js
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
// Collapse whitespace after dropping per-line comment margins (JSDoc `*` and
// `//`) so a phrase that wraps across comment lines matches as one string.
const squash = (s) => s.replace(/^\s*(?:\/\/|\*)\s?/gm, '').replace(/\s+/g, ' ').trim();

console.log('\n=== #322 comment fixes ===');

// --- Point 3: the pktEsc leak header -----------------------------------------
// renderLeft() also runs on filter/region changes, but those later calls return
// at the `filtersBuilt` guard before the addEventListener, so the closure was
// added once per VISIT, not per filter/region change. The old header claimed
// otherwise.
test('point 3: the pktEsc header does not claim the leak stacked on every filter/region change', () => {
  const header = read('test-issue-282-pktesc-listener.js');
  const h = squash(header.slice(0, header.indexOf('*/')));
  assert(!h.includes('runs on every visit to #/packets (and on every filter/region change within a visit)'),
    'the header still claims renderLeft stacks a listener on every filter/region change within a visit');
  assert(h.includes('filtersBuilt'),
    'the header should explain that later renders return at the filtersBuilt guard before the addEventListener');
});

// --- Point 4: normalizeObservedPathHashSizes()'s real callers ----------------
// renderSenderPathHashBadge() reads message.senderPathHashSize directly; it does
// NOT call normalizeObservedPathHashSizes(). The old comment said the sender
// badge used it.
test('point 4: channels.js does not say the sender badge uses normalizeObservedPathHashSizes()', () => {
  const src = squash(read('public/channels.js'));
  assert(!src.includes('the union/merge path and the sender badge still use it'),
    'the comment still claims the sender badge uses normalizeObservedPathHashSizes()');
  assert(src.includes('renderSenderPathHashBadge does not'),
    'the corrected comment should state renderSenderPathHashBadge does not call it');
});

console.log(`\n${passed} passed, ${failed} failed`);
process.exit(failed === 0 ? 0 : 1);
