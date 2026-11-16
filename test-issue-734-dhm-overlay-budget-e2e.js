/**
 * E2E (#734 M2): the DHM tile budget with two overlays on at once, a
 * hillshade and the contour lines, from z15 where both draw.
 *
 * #681's test-issue-681-dhm-tile-budget-e2e.js measures one overlay from
 * z8; below z14 the contours draw nothing, so a session that starts there
 * never shows the second stream. This one starts at z15 in Aarhus with
 * terrain and contours on (stored preferences), on a 1920x1080 window with
 * the HTTP cache off (Playwright routing), so the counts are upper bounds.
 * Every overlay request is the same per-client budget on the server
 * (map.dhm.tileRateLimitPerMin, one budget for every overlay) and takes
 * the same cold-tile admission slots (dhmTileMaxInFlight), so the counts
 * are per overlay and in total.
 *
 * Nothing leaves the machine: the dhm client-config block is intercepted,
 * /api/map/dhm/wms is a stub, third-party tiles are stubs.
 *
 * Deterministic on any runner (#798 review F1; tests/dhm-tile-budget.js
 * explains why the wall-clock extrapolation it replaced was not): pans
 * release without a glide, every step's count is checked against its
 * geometric ceiling, and the per-minute requirement comes from the
 * ceiling at a fixed user pace, not from how fast the runner was.
 *
 * Asserts, against the shipped defaults in cmd/server/dhm_proxy.go:
 *   - the cold first view of both overlays fits the admission bound, so no
 *     tile of a person's first view is refused as `busy`;
 *   - each session's first view and steps stay under their ceilings;
 *   - the default budget carries the normal session's ceiling, sustained
 *     at USER_VIEW_S per view, with HEADROOM, and the aggressive session's
 *     whole ceiling as one burst (the limiter's burst is one minute's
 *     budget).
 * The measured numbers are printed per step for the PR description
 * (AGENTS rule 0).
 *
 * Usage:
 *   BASE_URL=http://localhost:13910 node test-issue-734-dhm-overlay-budget-e2e.js
 */
'use strict';
const { chromium } = require('playwright');
const B = require('./tests/dhm-tile-budget');

const BASE = process.env.BASE_URL || 'http://localhost:13910';
const DHM_PATH = '/api/map/dhm/wms';
// Must match cmd/server/dhm_proxy.go.
const TILE_BUDGET_PER_MIN = 2400; // dhmTileRateLimitDefault
const TILE_MAX_IN_FLIGHT = 96;    // dhmTileMaxInFlight
const ATTRIBUTION = 'Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel, CC BY 4.0';
const DHM_BLOCK = {
  enabled: true, url: DHM_PATH, layer: 'dhm_terraen_skyggekort',
  minZoom: 7, maxZoom: 19, bounds: [[54.4265, 7.99125], [57.778, 15.5995]],
  attribution: ATTRIBUTION,
  datasetName: 'Danmarks Højdemodel', datasetUrl: 'https://dataforsyningen.dk/data/930',
  licenseName: 'CC BY 4.0', licenseUrl: 'https://creativecommons.org/licenses/by/4.0/',
  overlays: [
    { id: 'terrain', minZoom: 7, maxZoom: 19, datasetName: 'Danmarks Højdemodel', datasetUrl: 'https://dataforsyningen.dk/data/930' },
    { id: 'surface', minZoom: 7, maxZoom: 19, datasetName: 'Danmarks Højdemodel', datasetUrl: 'https://dataforsyningen.dk/data/928' },
    { id: 'contours', minZoom: 14, maxZoom: 19, datasetName: 'Danmarks Højdemodel', datasetUrl: 'https://dataforsyningen.dk/data/926' },
  ],
};

let failures = 0;
function assert(cond, msg) { if (!cond) throw new Error(msg); }

// A person looking around at contour zooms: two steps in, four pans,
// two steps out, with time to look at each view.
const NORMAL = [...B.times(2, B.zoomIn), ...B.pans(B.DIRECTIONS.slice(0, 4)), ...B.times(2, B.zoomOut)];
// Harder than real use: z15 → z19 → z15 with eight pans at both ends,
// as fast as the tiles are requested.
const AGGRESSIVE = [...B.times(4, B.zoomIn), ...B.pans(B.DIRECTIONS), ...B.times(4, B.zoomOut), ...B.pans(B.DIRECTIONS)];
const OVERLAYS = 2; // terrain + contours

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #734 DHM tile budget, hillshade + contours from z15, against ${BASE} ===\n`);
  const opts = (pauseMs, steps) => ({
    base: BASE, dhmPath: DHM_PATH, dhmBlock: DHM_BLOCK,
    storage: { 'mc-terrain-dhm': '1', 'mc-contours-dhm': '1' },
    hash: '#/map?lat=56.16&lon=10.2&zoom=15',
    readySelector: '.mc-contour-pane img.leaflet-tile',
    pauseMs, steps,
  });
  try {
    const normal = await B.runSession(browser, 'normal', opts(2500, NORMAL));
    const aggressive = await B.runSession(browser, 'aggressive', opts(0, AGGRESSIVE));
    for (const [name, r, steps] of [['normal', normal, NORMAL], ['aggressive', aggressive, AGGRESSIVE]]) {
      console.log(`  ${name}: ${r.count} tile requests ${JSON.stringify(r.byLayer)} of a ceiling of ` +
        `${OVERLAYS * B.sessionCeiling(steps, B.VIEWPORT)}; first view ${JSON.stringify(r.firstView)} (${r.elapsedS.toFixed(1)} s wall clock, not used)`);
      assert((r.firstView.terrain || 0) > 0 && (r.firstView.contours || 0) > 0, name + ': both overlays must request tiles at load');
      assert(!r.byLayer.surface, name + ': surface requested although terrain is the choice');
      assert(r.byLayer.terrain === r.byLayer.contours, `${name}: terrain and contours share every view, but asked ${r.byLayer.terrain} vs ${r.byLayer.contours}`);
      B.assertWithinCeilings(name, r);
    }
    const firstView = Math.max(...[normal, aggressive].map(r => Object.values(r.firstView).reduce((a, b) => a + b, 0)));
    const need = B.requiredPerMin(NORMAL, B.VIEWPORT, OVERLAYS);
    const burst = OVERLAYS * B.sessionCeiling(AGGRESSIVE, B.VIEWPORT);
    console.log(`  budget ${TILE_BUDGET_PER_MIN}/min: the normal session's ceiling ${OVERLAYS * B.sessionCeiling(NORMAL, B.VIEWPORT)} ` +
      `in ${B.nominalSeconds(NORMAL)} s at ${B.USER_VIEW_S} s per view, x${B.HEADROOM}, needs ${need.toFixed(0)}/min; ` +
      `the aggressive ceiling is ${burst} as one burst`);
    console.log(`  admission ${TILE_MAX_IN_FLIGHT}: a cold first view of both overlays is ${firstView} tiles`);
    assert(firstView <= TILE_MAX_IN_FLIGHT,
      `a cold first view is ${firstView} tiles: more than the ${TILE_MAX_IN_FLIGHT} admission slots`);
    assert(need <= TILE_BUDGET_PER_MIN,
      `the normal session needs ${need.toFixed(0)}/min: more than the budget ${TILE_BUDGET_PER_MIN}`);
    assert(burst <= TILE_BUDGET_PER_MIN,
      `the aggressive session's ceiling ${burst} exceeds the budget ${TILE_BUDGET_PER_MIN}`);
    console.log('  ✅ with two DHM overlays on, a cold view fits the admission bound, every step stays under its ceiling, and the default budget carries a normal session sustained for a minute with headroom and the aggressive burst');
  } catch (e) {
    failures++;
    console.log('  ❌ ' + e.message);
  } finally {
    await browser.close();
  }
  console.log(`\n${failures ? 'FAILED' : 'PASSED'}`);
  process.exit(failures ? 1 : 0);
})();
