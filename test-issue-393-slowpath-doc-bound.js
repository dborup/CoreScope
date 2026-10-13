/**
 * test-issue-393-slowpath-doc-bound.js
 *
 * #393 N1 — docs/api-spec.md must document the slow-path bound that the Go
 * code actually enforces. `sanitizeSlowPathComponent` caps each half at
 * `slowPathRawCap` CONTENT runes and then appends one "…" ellipsis when it
 * truncates; `normalizeSlowPath` joins two halves with a single space. So:
 *
 *   per half  = slowPathRawCap content runes + 1 ellipsis
 *   total     = 2*slowPathRawCap + 2 ellipses + 1 join space  = 2*cap + 3
 *
 * With cap = 128 that is 259, not the 257 the spec documented (and the
 * "≤128-rune-per-half" phrasing ignored the ellipsis entirely). This pins the
 * doc against the real constant so the drift cannot silently return.
 */
'use strict';
const fs = require('fs');
const assert = require('assert');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

const routes = fs.readFileSync('cmd/server/routes.go', 'utf8');
const capM = routes.match(/const slowPathRawCap = (\d+)/);
const cap = capM ? Number(capM[1]) : null;
const total = cap != null ? 2 * cap + 3 : null; // 2*cap + 2 ellipses + 1 space
const spec = fs.readFileSync('docs/api-spec.md', 'utf8');

test('#393 N1: slowPathRawCap is readable from cmd/server/routes.go', () => {
  assert.ok(cap != null, 'could not read `const slowPathRawCap = N` from routes.go');
});

test('#393 N1: api-spec documents the correct total bound (≤' + total + ' runes)', () => {
  assert.ok(spec.includes('≤' + total + ' runes'),
    'api-spec.md must document the total slow-path bound as ≤' + total + ' runes');
  assert.ok(!spec.includes('≤257 runes'),
    'api-spec.md still documents the stale ≤257-rune total (off by two)');
});

test('#393 N1: api-spec per-half wording accounts for the truncation ellipsis', () => {
  // The per-half copy is `cap` content runes PLUS the "…" marker, so the old
  // "≤128-rune-per-half" phrasing was off by one and must be gone.
  assert.ok(!spec.includes('≤128-rune-per-half'),
    'api-spec.md still says "≤128-rune-per-half" (ignores the ellipsis → off by one)');
});

console.log('\n#393 slow-path doc bound: ' + passed + ' passed, ' + failed + ' failed');
if (failed > 0) process.exit(1);
