/**
 * E2E (#332): no page may request a tile from basemaps.cartocdn.com.
 *
 * Several maps used to build their tile URL from a hard-coded CARTO
 * template instead of the operator's configured dark/light provider.
 * Keyless CARTO raster tiles have been stamped "API KEY REQUIRED" since
 * August 2026, so those maps rendered watermarked tiles even on an
 * instance configured for Esri/OpenTopoMap/OSM.
 *
 * This test drives a real browser over every surface named in the issue —
 * node detail, the main map, Live, Areas and both customizer geo-filter
 * previews — in BOTH themes, and asserts:
 *
 *   1. zero network requests to basemaps.cartocdn.com, and
 *   2. at least one tile request that DOES go to the expected provider
 *      host (so "no CARTO" can't pass by the map simply never loading).
 *
 * Third-party tile IMAGE requests are stubbed with a 1x1 PNG at the route
 * level: the point is which URL the page asks for, not what comes back,
 * and CI has no business reaching a tile CDN. Everything else off-origin
 * (the Leaflet/Chart.js bundles the pages load from unpkg) is allowed
 * through unchanged — stubbing those breaks the maps outright.
 *
 * Usage:
 *   BASE_URL=http://localhost:13800 node test-issue-332-no-carto-tiles-e2e.js
 *
 * Optional: EXPECT_TILE_HOST_DARK / EXPECT_TILE_HOST_LIGHT override the
 * host each theme's maps are expected to hit. Both default to
 * 'tile.openstreetmap.org', the keyless built-in fallback a server with no
 * map.tiles config resolves to (what CI runs). Point them at a configured
 * provider's host to verify a specific operator configuration, e.g.
 *   EXPECT_TILE_HOST_DARK=server.arcgisonline.com \
 *   EXPECT_TILE_HOST_LIGHT=tile.opentopomap.org node <this file>
 * against a server whose config.json sets darkDefault/lightDefault to
 * 'esri-darkgray-labels' / 'opentopomap'.
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13800';
const CARTO_HOST = 'basemaps.cartocdn.com';
// The keyless built-in fallback (#332) — what a server with no map.tiles
// config resolves to, in both themes.
const OSM_HOST = 'tile.openstreetmap.org';
const EXPECT = {
  dark:  process.env.EXPECT_TILE_HOST_DARK  || OSM_HOST,
  light: process.env.EXPECT_TILE_HOST_LIGHT || OSM_HOST,
};
// A node with lat/lon in test-fixtures/e2e-fixture.db, so the detail page
// actually renders its inset map.
const NODE_KEY = process.env.NODE_KEY ||
  '988396af0ff369c2178a8c2a303661bf26193257136ed4452b6eadd9547011e9';

// 1x1 transparent PNG — stands in for every third-party tile.
const PNG_1X1 = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg==',
  'base64');

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });

  console.log(`\n=== #332 no-CARTO tile requests against ${BASE} ===`);
  console.log(`    expected tile host — dark: ${EXPECT.dark}  light: ${EXPECT.light}\n`);

  // One fresh context per surface so the recorded request list is scoped,
  // and so a tile cached by an earlier page can't mask a later regression.
  async function withPage(theme, fn) {
    const ctx = await browser.newContext();
    const tiles = [];
    // Record + stub off-origin IMAGE requests only (tiles). Scripts and
    // stylesheets — Leaflet, Chart.js — must load for real or no map exists.
    await ctx.route(/^https?:\/\/(?!localhost|127\.0\.0\.1)/i, async (route) => {
      if (route.request().resourceType() !== 'image') { await route.continue(); return; }
      tiles.push(route.request().url());
      await route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 });
    });
    // Pin the theme before any app script runs, so the very first tile
    // attach already uses the theme under test. addInitScript can run before
    // <html> exists, so set it again on DOMContentLoaded.
    // 'meshcore-theme' is the key index.html's inline bootstrap and
    // app.js applyTheme() both read; setting data-theme alone is undone by
    // them on load.
    await ctx.addInitScript((t) => {
      try { localStorage.setItem('meshcore-theme', t); } catch (_) {}
      const apply = () => {
        try { document.documentElement.setAttribute('data-theme', t); } catch (_) {}
      };
      apply();
      document.addEventListener('DOMContentLoaded', apply);
    }, theme);
    const page = await ctx.newPage();
    page.setDefaultTimeout(15000);
    page.on('pageerror', (e) => console.error('    [pageerror]', e.message));
    try { await fn(page, tiles); }
    finally { await ctx.close(); }
  }

  function check(label, tiles, { requireHit = true, expectHost = null } = {}) {
    const carto = tiles.filter((u) => u.indexOf(CARTO_HOST) !== -1);
    assert(carto.length === 0,
      `${label}: ${carto.length} request(s) to ${CARTO_HOST} — first: ${carto[0]}`);
    if (requireHit) {
      const host = expectHost || OSM_HOST;
      const hits = tiles.filter((u) => u.indexOf(host) !== -1);
      assert(hits.length > 0,
        `${label}: no tile request to ${host} — the map did not load, ` +
        `so "0 CARTO requests" proves nothing. Saw: ${JSON.stringify(tiles.slice(0, 5))}`);
    }
  }

  for (const theme of ['dark', 'light']) {
    console.log(`── theme: ${theme} ──`);

    await step(`[${theme}] /#/map requests no CARTO tiles`, async () => {
      await withPage(theme, async (page, tiles) => {
        await page.goto(BASE + '/#/map', { waitUntil: 'domcontentloaded' });
        await page.waitForSelector('#leaflet-map .leaflet-tile-pane', { state: 'attached', timeout: 15000 });
        await page.waitForTimeout(1500);
        check('map', tiles, { expectHost: EXPECT[theme] });
      });
    });

    await step(`[${theme}] /#/live requests no CARTO tiles`, async () => {
      await withPage(theme, async (page, tiles) => {
        await page.goto(BASE + '/#/live', { waitUntil: 'domcontentloaded' });
        await page.waitForSelector('.leaflet-tile-pane', { state: 'attached', timeout: 15000 });
        await page.waitForTimeout(1500);
        check('live', tiles, { expectHost: EXPECT[theme] });
      });
    });

    await step(`[${theme}] node detail inset map requests no CARTO tiles`, async () => {
      await withPage(theme, async (page, tiles) => {
        await page.goto(BASE + '/#/nodes/' + NODE_KEY, { waitUntil: 'domcontentloaded' });
        await page.waitForSelector('.leaflet-tile-pane', { state: 'attached', timeout: 15000 });
        await page.waitForTimeout(1500);
        check('node detail', tiles, { expectHost: EXPECT[theme] });
      });
    });

    await step(`[${theme}] Areas tab requests no CARTO tiles`, async () => {
      await withPage(theme, async (page, tiles) => {
        await page.goto(BASE + '/#/analytics', { waitUntil: 'domcontentloaded' });
        const tab = page.locator('.tab-btn[data-tab="areas"]');
        await tab.waitFor({ timeout: 15000 });
        await tab.click();
        await page.waitForTimeout(2500);
        // The Areas tab only mounts a map when the fixture has areas, so a
        // tile hit is not guaranteed here — the CARTO assertion is.
        check('areas', tiles, { requireHit: false });
      });
    });

    await step(`[${theme}] customizer geo-filter previews request no CARTO tiles`, async () => {
      await withPage(theme, async (page, tiles) => {
        await page.goto(BASE + '/#/map', { waitUntil: 'domcontentloaded' });
        await page.waitForSelector('#customizeToggle', { timeout: 15000 });
        // Drop the map's own tiles so the next requests come from the
        // customizer previews alone.
        await page.waitForTimeout(1200);
        tiles.length = 0;
        await page.click('#customizeToggle');
        const gfTab = page.locator('.cust-tab[data-tab="geofilter"]');
        await gfTab.waitFor({ timeout: 15000 });
        await gfTab.click();
        await page.waitForSelector('#cv2-gf-map .leaflet-tile-pane', { state: 'attached', timeout: 15000 });
        await page.waitForTimeout(1500);
        // Both previews are deliberately LIGHT-styled whatever the app theme
        // is, so they resolve the configured light provider in both passes.
        check('customizer geo-filter tab', tiles, { expectHost: EXPECT.light });

        // Clicking the inset opens the full-screen modal preview — the
        // second site that used to hard-code CARTO.
        tiles.length = 0;
        await page.click('#cv2-gf-map');
        await page.waitForSelector('#cv2-gf-modal-map .leaflet-tile-pane', { state: 'attached', timeout: 15000 });
        await page.waitForTimeout(1500);
        check('customizer geo-filter modal', tiles, { expectHost: EXPECT.light });
      });
    });
  }

  // The standalone geo-filter builder has no provider registry at all; it
  // used to hard-code the CARTO dark template.
  await step('standalone /geofilter-builder.html requests no CARTO tiles', async () => {
    await withPage('dark', async (page, tiles) => {
      await page.goto(BASE + '/geofilter-builder.html', { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('#map .leaflet-tile-pane', { state: 'attached', timeout: 15000 });
      await page.waitForTimeout(1500);
      // The builder loads no registry, so it always uses the OSM baseline
      // regardless of how the server is configured.
      check('geofilter-builder', tiles, { expectHost: OSM_HOST });
    });
  });

  await browser.close();
  console.log(`\n#332 no-CARTO E2E: ${passed} passed, ${failed} failed`);
  process.exit(failed === 0 ? 0 : 1);
})().catch((e) => { console.error(e); process.exit(1); });
