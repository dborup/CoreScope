/**
 * Source-grep tests for the "View Path" button on the Analytics Distance
 * tab's two leaderboards (public/analytics.js, renderDistanceTab): "Top 20
 * Longest Hops" and "Top 10 Longest Multi-Hop Paths".
 *
 * History: both tables originally had a "View on map" icon button
 * (dist-map-hop / dist-map-path, dropped pins on the main map via
 * sessionStorage + #/map?route=1). A first pass added View Path as a
 * SECOND icon button in the same cell, which made two 48x48 touch-target
 * buttons (the AGENTS glove-operability floor -- can't shrink below that)
 * unreadable in a dense 20-row leaderboard regardless of scroll/sticky
 * tricks. Since View Path is a strict superset of what View on map showed
 * (same route, plus elapsed time, area shading, branch legend), the map
 * button was dropped -- View Path is now the sole action per row.
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

console.log('\n=== analytics.js: Distance tab "View Path" button ===');

test('Top 20 Longest Hops row builds a View Path button gated on h.hash', () => {
  assert.ok(src.includes('const viewPathBtn = h.hash ? `<button class="btn-icon dist-view-path" data-view-path="${esc(h.hash)}"'),
    'the per-hop leaderboard row must build a dist-view-path button carrying the full hash');
});

test('Top 10 Longest Multi-Hop Paths row builds a View Path button gated on p.hash', () => {
  assert.ok(src.includes('const viewPathBtn = p.hash ? `<button class="btn-icon dist-view-path" data-view-path="${esc(p.hash)}"'),
    'the per-path leaderboard row must build a dist-view-path button carrying the full hash');
});

test('View Path is the only action rendered in the trailing cell of both leaderboards', () => {
  const count = (src.match(/<td>\$\{viewPathBtn\}<\/td><\/tr>`;/g) || []).length;
  assert.strictEqual(count, 2, 'expected exactly 2 rows (hops leaderboard + paths leaderboard) to render only viewPathBtn, got ' + count);
});

test('the old "View on map" button (dist-map-hop / dist-map-path) is gone from the Distance tab', () => {
  // A historical mention in a code comment is fine; what must be gone is
  // the actual markup/wiring -- a rendered button class or a querySelectorAll
  // that would try to wire up a button no longer emitted.
  assert.ok(!src.includes('class="btn-icon dist-map-hop"') && !src.includes('class="btn-icon dist-map-path"'),
    'no row should render a dist-map-hop/dist-map-path button anymore');
  assert.ok(!src.includes("querySelectorAll('.dist-map-hop')") && !src.includes("querySelectorAll('.dist-map-path')"),
    'no wiring block should still be querying for a button that is never rendered');
});

test('dist-view-path buttons are wired via window.PacketPathMap.open, not a navigation', () => {
  assert.ok(src.includes("el.querySelectorAll('.dist-view-path').forEach(btn => {"),
    'must delegate-wire dist-view-path buttons after render');
  const wireIdx = src.indexOf("el.querySelectorAll('.dist-view-path').forEach(btn => {");
  const snippet = src.slice(wireIdx, wireIdx + 200);
  assert.ok(snippet.includes('window.PacketPathMap') && snippet.includes('btn.dataset.viewPath'),
    'the click handler must call window.PacketPathMap.open(btn.dataset.viewPath)');
});

test('the View Path wiring block is registered after the innerHTML assignment', () => {
  const innerHtmlIdx = src.indexOf('el.innerHTML = html;');
  const wireIdx = src.indexOf("el.querySelectorAll('.dist-view-path').forEach(btn => {");
  assert.ok(innerHtmlIdx > -1 && wireIdx > -1 && wireIdx > innerHtmlIdx,
    'button listeners must attach after the HTML they target has been inserted into the DOM');
});

test('neither leaderboard needs the analytics-table-scroll wrapper anymore', () => {
  // A single 48x48 action column plus 9-10 compact text columns fits
  // without a forced horizontal scroll container -- confirms the row
  // template really did drop back to one action, not just hide a second.
  const wrapCount = (src.match(/<div class="analytics-table-scroll">/g) || []).length;
  assert.strictEqual(wrapCount, 0, 'renderDistanceTab should not need analytics-table-scroll, got ' + wrapCount + ' wrapper(s)');
});

console.log('\n=== style.css: no leftover .col-actions sticky rule ===');
{
  const css = fs.readFileSync('public/style.css', 'utf8');
  test('the sticky .col-actions rule was removed along with its now-unused class', () => {
    assert.ok(!css.includes('.col-actions'),
      'renderDistanceTab no longer emits class="col-actions" anywhere -- this CSS would be dead weight');
  });
}

// ===== SUMMARY =====
console.log(`\n${'='.repeat(40)}`);
console.log(`analytics.js Distance tab View Path tests: ${passed} passed, ${failed} failed`);
console.log(`${'='.repeat(40)}\n`);
if (failed > 0) process.exit(1);
