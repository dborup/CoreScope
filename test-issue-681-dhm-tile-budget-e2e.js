/**
 * E2E (#681 M3 / #682 review F3): how many DHM hillshade tiles a person
 * can make the Map request in one minute, against the per-client tile
 * budget map.dhm.tileRateLimitPerMin (dhmTileRateLimitDefault in
 * cmd/server/dhm_proxy.go).
 *
 * The overlay is switched on the way test-issue-680-dhm-terrain-e2e.js
 * does it (an intercepted dhm client-config block, a stubbed
 * /api/map/dhm/wms), so nothing reaches Dataforsyningen. The stub answers
 * every tile, so the count is what the browser asks for; the server would
 * see fewer, since its cache hits are not charged.
 *
 * Two sessions, each in a fresh context on a large 1920×1080 window:
 *   - normal: load Denmark at zoom 8 with the overlay on, three zoom steps
 *     in, four pans, two steps out, a few seconds per view;
 *   - aggressive: through the whole zoom range 8→16→8 with eight pans at
 *     each end, each step waiting only for its tiles to be requested.
 * Playwright routing disables the HTTP cache, so zooming back out
 * re-requests tiles a real browser holds for 24 h: both counts are upper
 * bounds.
 *
 * Deterministic on any runner (#798 review F1; tests/dhm-tile-budget.js
 * explains the two random sources this test used to have: Leaflet's
 * drag glide and the #728 N3 retry of its 1x1 stub tile, which made the
 * same session ask 299 to 428 tiles on CI and turned master red at
 * c4de1f20). Pans release without a glide, the stub is a real 256x256
 * tile, every step's count is checked against its geometric ceiling, and
 * the per-minute requirement comes from the normal session's ceiling at
 * a fixed user pace (USER_VIEW_S per view) with HEADROOM, never from the
 * wall clock. The aggressive session's whole ceiling must fit as one
 * burst (the limiter's burst capacity is one minute's budget,
 * ip_rate_limit.go: burst = interval * perMin).
 *
 * Usage:
 *   BASE_URL=http://localhost:13700 node test-issue-681-dhm-tile-budget-e2e.js
 */
'use strict';
const { chromium } = require('playwright');
const B = require('./tests/dhm-tile-budget');

const BASE = process.env.BASE_URL || 'http://localhost:13700';
const DHM_PATH = '/api/map/dhm/wms';
// Must match dhmTileRateLimitDefault in cmd/server/dhm_proxy.go (#734 M2
// raised it for two overlays; test-issue-734-dhm-overlay-budget-e2e.js).
const TILE_BUDGET_PER_MIN = 2400;
const DHM_BLOCK = {
  enabled: true, url: DHM_PATH, layer: 'dhm_terraen_skyggekort',
  minZoom: 7, maxZoom: 19, bounds: [[54.4265, 7.99125], [57.778, 15.5995]],
  attribution: 'Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel, CC BY 4.0',
};

let failures = 0;
function assert(cond, msg) { if (!cond) throw new Error(msg); }

// A person looking around: three zoom steps in, four pans, two out, with
// time to look at each view.
const NORMAL = [...B.times(3, B.zoomIn), ...B.pans(B.DIRECTIONS.slice(0, 4)), ...B.times(2, B.zoomOut)];
// Harder than real use: through the whole zoom range and back, panning in
// eight directions at both ends, as fast as the tiles are requested.
const AGGRESSIVE = [...B.times(8, B.zoomIn), ...B.pans(B.DIRECTIONS), ...B.times(8, B.zoomOut), ...B.pans(B.DIRECTIONS)];
const OVERLAYS = 1;

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #681 DHM tile budget against ${BASE} ===\n`);
  // The overlay's stored preference (public/map-tile-providers.js) turns
  // it on at load.
  const opts = (pauseMs, steps) => ({
    base: BASE, dhmPath: DHM_PATH, dhmBlock: DHM_BLOCK,
    storage: { 'mc-terrain-dhm': '1' },
    hash: '#/map?lat=56.0&lon=10.5&zoom=8',
    readySelector: '.mc-terrain-pane img.leaflet-tile',
    loadSettleMs: 800, pauseMs, steps,
  });
  try {
    const normal = await B.runSession(browser, 'normal', opts(2500, NORMAL));
    const aggressive = await B.runSession(browser, 'aggressive', opts(450, AGGRESSIVE));
    for (const [name, r, steps] of [['normal', normal, NORMAL], ['aggressive', aggressive, AGGRESSIVE]]) {
      console.log(`  ${name}: ${r.count} tile requests of a ceiling of ${B.sessionCeiling(steps, B.VIEWPORT)}; ` +
        `first view ${JSON.stringify(r.firstView)} (${r.elapsedS.toFixed(1)} s wall clock, not used)`);
      assert(Object.keys(r.firstView).length > 0, name + ': overlay requested no tiles at load');
      B.assertWithinCeilings(name, r);
    }
    const need = B.requiredPerMin(NORMAL, B.VIEWPORT, OVERLAYS);
    const burst = B.sessionCeiling(AGGRESSIVE, B.VIEWPORT);
    console.log(`  budget ${TILE_BUDGET_PER_MIN}/min: the normal session's ceiling ${B.sessionCeiling(NORMAL, B.VIEWPORT)} ` +
      `in ${B.nominalSeconds(NORMAL)} s at ${B.USER_VIEW_S} s per view, x${B.HEADROOM}, needs ${need.toFixed(0)}/min; ` +
      `the aggressive ceiling is ${burst} as one burst`);
    assert(need <= TILE_BUDGET_PER_MIN,
      `the normal session needs ${need.toFixed(0)}/min: more than the budget ${TILE_BUDGET_PER_MIN}`);
    assert(burst <= TILE_BUDGET_PER_MIN,
      `the aggressive session's ceiling ${burst} exceeds the budget ${TILE_BUDGET_PER_MIN}`);
    console.log('  ✅ every step stays under its ceiling, and the default budget carries a normal session sustained for a minute with headroom and the aggressive burst');
  } catch (e) {
    failures++;
    console.log('  ❌ ' + e.message);
  } finally {
    await browser.close();
  }
  console.log(`\n${failures ? 'FAILED' : 'PASSED'}`);
  process.exit(failures ? 1 : 0);
})();
