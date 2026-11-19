/* test-issue-929-route-overlays.js — #929: the Map's route-style views hide
 * and restore the node overlays (markers, clusters, heat) through ONE pair of
 * helpers in public/map.js, and the heat map follows that state.
 *
 * map.js is read as source; the behaviour itself (no error on a node reload,
 * no heat under a route, heat back on close) is the E2E's
 * (test-issue-929-route-heat-layer-e2e.js).
 *
 * Covered:
 *   1. hideRouteViewOverlays / restoreRouteViewOverlays exist once each and
 *      handle all three overlay layers;
 *   2. drawPacketRoute, drawGPSTrail and drawEstimatedNodes call the hide
 *      helper once and the restore helper (their close button) once, and
 *      touch none of the three layers directly (the mutant: one inlined call
 *      site goes red);
 *   3. toggleHeatmap only updates a layer on the map in place and keeps a
 *      new one off the map while a route view is open; the HeatGradient
 *      watcher rebuilds through it without its own detached flag;
 *   4. destroy() clears the route-view state with the map.
 *
 * Runs via: node test-issue-929-route-overlays.js
 */
'use strict';
const fs = require('fs');
const path = require('path');
const assert = require('assert');

const src = fs.readFileSync(path.join(__dirname, 'public', 'map.js'), 'utf8');
let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
// The source of a top-level (2-space indented) function of the map IIFE, up
// to the next one.
function body(name) {
  const re = new RegExp('\\n  (?:async )?function ' + name + '\\(');
  const m = re.exec(src);
  assert(m, name + ' not found');
  const rest = src.slice(m.index + 1);
  const next = rest.slice(1).search(/\n  (?:async )?function \w+\(/);
  return next < 0 ? rest : rest.slice(0, next + 1);
}
const count = (s, needle) => s.split(needle).length - 1;
const LAYERS = ['markerLayer', 'clusterGroup', 'heatLayer'];

console.log('\n=== #929 route-view overlays (map.js source) ===\n');

test('one hide and one restore helper, covering markers, clusters and heat', () => {
  assert.strictEqual(count(src, 'function hideRouteViewOverlays('), 1);
  assert.strictEqual(count(src, 'function restoreRouteViewOverlays('), 1);
  const hide = body('hideRouteViewOverlays'), restore = body('restoreRouteViewOverlays');
  for (const l of LAYERS) {
    assert(hide.includes('map.removeLayer(' + l + ')'), 'hide misses ' + l);
    assert(restore.includes('map.addLayer(' + l + ')'), 'restore misses ' + l);
  }
  assert(/routeViewOpen = true/.test(hide) && /routeViewOpen = false/.test(restore), 'the helpers own the route-view state');
});

for (const fn of ['drawPacketRoute', 'drawGPSTrail', 'drawEstimatedNodes']) {
  test(fn + ' hides and restores through the helpers only', () => {
    const b = body(fn);
    assert.strictEqual(count(b, 'hideRouteViewOverlays()'), 1, 'hide calls');
    assert.strictEqual(count(b, 'restoreRouteViewOverlays()'), 1, 'restore calls');
    for (const l of LAYERS) {
      assert(!new RegExp('map\\.(?:removeLayer|addLayer)\\(' + l + '\\)').test(b), fn + ' touches ' + l + ' directly');
    }
  });
}

test('toggleHeatmap updates in place only on the map, and builds off the map under a route view', () => {
  const b = body('toggleHeatmap');
  assert(/if \(heatLayer && map\.hasLayer\(heatLayer\)\) \{\s*heatLayer\.setLatLngs\(points\);/.test(b), 'setLatLngs is not guarded by map.hasLayer');
  assert.strictEqual(count(b, 'setLatLngs('), 1);
  assert(/if \(!routeViewOpen\) heatLayer\.addTo\(map\);/.test(b), 'addTo is not guarded by routeViewOpen');
  assert(!/detached/.test(src), 'a second, detached flag is back');
});

test('destroy() clears the route-view state', () => {
  assert(/routeViewOpen = false;/.test(body('destroy')), 'routeViewOpen survives destroy');
});

console.log(`\n${passed} passed, ${failed} failed`);
process.exit(failed ? 1 : 0);
