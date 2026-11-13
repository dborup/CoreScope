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
 * Two sessions, each in a fresh context on a large 1920×1080 window,
 * each inside one minute:
 *   - normal: load Denmark at zoom 8 with the overlay on, three zoom steps
 *     in, four pans, two steps out, a few seconds per view;
 *   - aggressive: through the whole zoom range 8→16→8 with eight pans at
 *     each end, each step waiting only for its tiles to be requested.
 * Playwright routing disables the HTTP cache, so zooming back out
 * re-requests tiles a real browser holds for 24 h: both counts are upper
 * bounds.
 *
 * Asserts: the default budget is at least 3x the normal session and
 * covers the aggressive one.
 *
 * Usage:
 *   BASE_URL=http://localhost:13700 node test-issue-681-dhm-tile-budget-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13700';
const DHM_PATH = '/api/map/dhm/wms';
// Must match dhmTileRateLimitDefault in cmd/server/dhm_proxy.go.
const TILE_BUDGET_PER_MIN = 1200;
const DHM_BLOCK = {
  enabled: true, url: DHM_PATH, layer: 'dhm_terraen_skyggekort',
  minZoom: 7, maxZoom: 19, bounds: [[54.4265, 7.99125], [57.778, 15.5995]],
  attribution: 'Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel, CC BY 4.0',
};
const PNG_1X1 = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg==',
  'base64');

let failures = 0;
function assert(cond, msg) { if (!cond) throw new Error(msg); }

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #681 DHM tile budget against ${BASE} ===\n`);
  // One fresh browser context per session: nothing cached between them.
  async function session(name, script) {
    const ctx = await browser.newContext({ viewport: { width: 1920, height: 1080 } });
    const terrain = [];
    try {
      await ctx.route(BASE + '/api/config/client', async (route) => {
        const response = await route.fetch();
        const config = await response.json();
        config.dhm = DHM_BLOCK;
        await route.fulfill({ response, json: config });
      });
      await ctx.route(url => new URL(url).pathname === DHM_PATH, async (route) => {
        terrain.push(Date.now());
        await route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 });
      });
      // Base-map tiles never leave the machine.
      await ctx.route(/^https?:\/\/(?!localhost|127\.0\.0\.1)/i, async (route) => {
        if (route.request().resourceType() !== 'image') { await route.continue(); return; }
        await route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 });
      });
      // The overlay's stored preference (public/map-tile-providers.js)
      // turns it on at load.
      await ctx.addInitScript(() => { try { localStorage.setItem('mc-terrain-dhm', '1'); } catch (_) {} });
      const page = await ctx.newPage();
      const settle = (ms) => page.waitForTimeout(ms);
      const ops = {
        async pan(dx, dy, ms) {
          const box = await page.$eval('.leaflet-container', el => {
            const r = el.getBoundingClientRect();
            return { x: r.x + r.width / 2, y: r.y + r.height / 2 };
          });
          await page.mouse.move(box.x, box.y);
          await page.mouse.down();
          await page.mouse.move(box.x + dx, box.y + dy, { steps: 6 });
          await page.mouse.up();
          await settle(ms);
        },
        async zoomIn(ms) { await page.click('.leaflet-control-zoom-in'); await settle(ms); },
        async zoomOut(ms) { await page.click('.leaflet-control-zoom-out'); await settle(ms); },
      };
      await page.goto(`${BASE}/#/map?lat=56.0&lon=10.5&zoom=8`, { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('.leaflet-control-layers', { timeout: 20000 });
      await page.waitForFunction(() => document.querySelectorAll('.mc-terrain-pane img.leaflet-tile').length > 0,
        null, { timeout: 15000 });
      await settle(800);
      const start = terrain[0];
      const atLoad = terrain.length;
      await script(ops);
      const elapsedS = (Date.now() - start) / 1000;
      console.log(`  ${name}: ${terrain.length} tile requests in ${elapsedS.toFixed(1)} s (${atLoad} at load)`);
      assert(atLoad > 0, name + ': overlay requested no tiles at load');
      assert(elapsedS <= 60, `${name}: took ${elapsedS.toFixed(1)} s, longer than the one-minute window`);
      return terrain.length;
    } finally {
      await ctx.close();
    }
  }

  const directions = [[500, 0], [0, 400], [-500, 0], [0, -400], [400, 300], [-400, 300], [-400, -300], [400, -300]];
  try {
    // A person looking around: three zoom steps in, four pans, two out,
    // with time to look at each view.
    const normal = await session('normal', async (op) => {
      for (let i = 0; i < 3; i++) await op.zoomIn(2500);
      for (const [dx, dy] of directions.slice(0, 4)) await op.pan(dx, dy, 2500);
      for (let i = 0; i < 2; i++) await op.zoomOut(2500);
    });
    // Harder than real use: through the whole zoom range and back, panning
    // in eight directions at both ends, as fast as the tiles are requested.
    const aggressive = await session('aggressive', async (op) => {
      for (let i = 0; i < 8; i++) await op.zoomIn(450);
      for (const [dx, dy] of directions) await op.pan(dx, dy, 450);
      for (let i = 0; i < 8; i++) await op.zoomOut(450);
      for (const [dx, dy] of directions) await op.pan(dx, dy, 450);
    });
    console.log(`  budget ${TILE_BUDGET_PER_MIN}/min: ${(TILE_BUDGET_PER_MIN / normal).toFixed(1)}x a normal session, ` +
      `${(TILE_BUDGET_PER_MIN / aggressive).toFixed(1)}x the aggressive one`);
    assert(normal * 3 <= TILE_BUDGET_PER_MIN, `normal session ${normal} tiles: budget ${TILE_BUDGET_PER_MIN} is under 3x that`);
    assert(aggressive <= TILE_BUDGET_PER_MIN, `aggressive session ${aggressive} tiles exceeds the budget ${TILE_BUDGET_PER_MIN}`);
    console.log('  ✅ a normal session uses under a third of the default tile budget, the aggressive one fits in it');
  } catch (e) {
    failures++;
    console.log('  ❌ ' + e.message);
  } finally {
    await browser.close();
  }
  console.log(`\n${failures ? 'FAILED' : 'PASSED'}`);
  process.exit(failures ? 1 : 0);
})();
