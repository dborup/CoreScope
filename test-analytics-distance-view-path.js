/**
 * Source-grep tests for the "View Path" button added to the Analytics
 * Distance tab's two leaderboards (public/analytics.js, renderDistanceTab):
 * "Top 20 Longest Hops" and "Top 10 Longest Multi-Hop Paths".
 *
 * Both tables already had a small packet-hash link and a "View on map"
 * icon button (dist-map-hop / dist-map-path, which just drop pins on the
 * main map via sessionStorage + #/map?route=1). View Path adds the richer
 * in-place packet-path-map.js modal (elapsed time, area shading, branch
 * legend) as a second icon button in the same actions cell.
 */
'use strict';
const fs = require('fs');
const assert = require('assert');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

const src = fs.readFileSync('public/analytics.js', 'utf8');

console.log('\n=== analytics.js: Distance tab "View Path" buttons ===');

test('Top 20 Longest Hops row builds a View Path button gated on h.hash', () => {
  assert.ok(src.includes('const viewPathBtn = h.hash ? `<button class="btn-icon dist-view-path" data-view-path="${esc(h.hash)}"'),
    'the per-hop leaderboard row must build a dist-view-path button carrying the full hash');
});

test('Top 10 Longest Multi-Hop Paths row builds a View Path button gated on p.hash', () => {
  assert.ok(src.includes('const viewPathBtn = p.hash ? `<button class="btn-icon dist-view-path" data-view-path="${esc(p.hash)}"'),
    'the per-path leaderboard row must build a dist-view-path button carrying the full hash');
});

test('both leaderboards append viewPathBtn into the same actions cell as the existing map button', () => {
  assert.ok(src.includes('<td>${mapBtn}${viewPathBtn}</td></tr>`;'),
    'View Path must render alongside View on map, not replace it or need its own column');
  // Both rows use the identical closing shape; confirm it appears twice (hops row + paths row).
  const count = (src.match(/<td>\$\{mapBtn\}\$\{viewPathBtn\}<\/td><\/tr>`;/g) || []).length;
  assert.strictEqual(count, 2, 'expected exactly 2 rows (hops leaderboard + paths leaderboard) to use this cell shape, got ' + count);
});

test('dist-view-path buttons are wired via window.PacketPathMap.open, not a navigation', () => {
  assert.ok(src.includes("el.querySelectorAll('.dist-view-path').forEach(btn => {"),
    'must wire dist-view-path buttons the same per-button addEventListener pattern as dist-map-hop/dist-map-path');
  const wireIdx = src.indexOf("el.querySelectorAll('.dist-view-path').forEach(btn => {");
  const snippet = src.slice(wireIdx, wireIdx + 200);
  assert.ok(snippet.includes('window.PacketPathMap') && snippet.includes('btn.dataset.viewPath'),
    'the click handler must call window.PacketPathMap.open(btn.dataset.viewPath)');
});

test('the View Path wiring block is registered after the innerHTML assignment, like the map-button wiring', () => {
  const innerHtmlIdx = src.indexOf('el.innerHTML = html;');
  const wireIdx = src.indexOf("el.querySelectorAll('.dist-view-path').forEach(btn => {");
  assert.ok(innerHtmlIdx > -1 && wireIdx > -1 && wireIdx > innerHtmlIdx,
    'button listeners must attach after the HTML they target has been inserted into the DOM');
});

// ===== SUMMARY =====
console.log(`\n${'='.repeat(40)}`);
console.log(`analytics.js Distance tab View Path tests: ${passed} passed, ${failed} failed`);
console.log(`${'='.repeat(40)}\n`);
if (failed > 0) process.exit(1);
