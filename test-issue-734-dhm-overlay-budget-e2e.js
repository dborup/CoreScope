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
 * Asserts, against the shipped defaults in cmd/server/dhm_proxy.go:
 *   - the cold first view of both overlays fits the admission bound, so no
 *     tile of a person's first view is refused as `busy`;
 *   - the default budget leaves 1.3x headroom over the normal session
 *     extrapolated to a full minute (as #681 does), and covers the
 *     aggressive session's raw burst.
 * The measured numbers are printed for the PR description (AGENTS rule 0).
 *
 * Usage:
 *   BASE_URL=http://localhost:13910 node test-issue-734-dhm-overlay-budget-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

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
// A real 256x256 tile is what the page keeps; a 1x1 one would be retried
// as a placeholder (#728 N3) and inflate the count.
const zlib = require('zlib');
const TILE = (() => {
  const crcT = new Int32Array(256);
  for (let n = 0; n < 256; n++) { let c = n; for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1; crcT[n] = c; }
  const crc = (b) => { let c = -1; for (const x of b) c = crcT[(c ^ x) & 0xff] ^ (c >>> 8); return (c ^ -1) >>> 0; };
  const chunk = (t, d) => { const l = Buffer.alloc(4); l.writeUInt32BE(d.length); const td = Buffer.concat([Buffer.from(t), d]); const c = Buffer.alloc(4); c.writeUInt32BE(crc(td)); return Buffer.concat([l, td, c]); };
  const ihdr = Buffer.alloc(13); ihdr.writeUInt32BE(256, 0); ihdr.writeUInt32BE(256, 4); ihdr[8] = 8; ihdr[9] = 6;
  return Buffer.concat([Buffer.from('\x89PNG\r\n\x1a\n', 'binary'), chunk('IHDR', ihdr), chunk('IDAT', zlib.deflateSync(Buffer.alloc((256 * 4 + 1) * 256))), chunk('IEND', Buffer.alloc(0))]);
})();
const BASE_TILE = TILE;

let failures = 0;
function assert(cond, msg) { if (!cond) throw new Error(msg); }

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #734 DHM tile budget, hillshade + contours from z15, against ${BASE} ===\n`);
  async function session(name, script) {
    const ctx = await browser.newContext({ viewport: { width: 1920, height: 1080 } });
    const tiles = [];
    try {
      await ctx.route(BASE + '/api/config/client', async (route) => {
        const response = await route.fetch();
        const config = await response.json();
        config.dhm = DHM_BLOCK;
        await route.fulfill({ response, json: config });
      });
      await ctx.route(url => new URL(url).pathname === DHM_PATH, async (route) => {
        tiles.push({ t: Date.now(), id: new URL(route.request().url()).searchParams.get('layers') });
        await route.fulfill({ status: 200, contentType: 'image/png', body: TILE });
      });
      await ctx.route(/^https?:\/\/(?!localhost|127\.0\.0\.1)/i, async (route) => {
        if (route.request().resourceType() !== 'image') { await route.continue(); return; }
        await route.fulfill({ status: 200, contentType: 'image/png', body: BASE_TILE });
      });
      await ctx.addInitScript(() => {
        try { localStorage.setItem('mc-terrain-dhm', '1'); localStorage.setItem('mc-contours-dhm', '1'); } catch (_) {}
      });
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
      await page.goto(`${BASE}/#/map?lat=56.16&lon=10.2&zoom=15`, { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('.leaflet-control-layers', { timeout: 20000 });
      await page.waitForFunction(() => document.querySelectorAll('.mc-contour-pane img.leaflet-tile').length > 0,
        null, { timeout: 15000 });
      await settle(1500);
      const start = tiles[0].t;
      const atLoad = { all: tiles.length, terrain: tiles.filter(x => x.id === 'terrain').length, contours: tiles.filter(x => x.id === 'contours').length };
      await script(ops);
      const elapsedS = (Date.now() - start) / 1000;
      const by = (id) => tiles.filter(x => x.id === id).length;
      console.log(`  ${name}: ${tiles.length} tile requests in ${elapsedS.toFixed(1)} s ` +
        `(terrain ${by('terrain')}, contours ${by('contours')}); first view ${atLoad.all} (terrain ${atLoad.terrain}, contours ${atLoad.contours})`);
      assert(atLoad.terrain > 0 && atLoad.contours > 0, name + ': both overlays must request tiles at load');
      assert(by('surface') === 0, name + ': surface requested although terrain is the choice');
      assert(elapsedS <= 60, `${name}: took ${elapsedS.toFixed(1)} s, longer than the one-minute window`);
      return { count: tiles.length, elapsedS, atLoad };
    } finally {
      await ctx.close();
    }
  }

  const directions = [[500, 0], [0, 400], [-500, 0], [0, -400], [400, 300], [-400, 300], [-400, -300], [400, -300]];
  try {
    // A person looking around at contour zooms: two steps in, four pans,
    // two steps out, with time to look at each view.
    const normal = await session('normal', async (op) => {
      for (let i = 0; i < 2; i++) await op.zoomIn(2500);
      for (const [dx, dy] of directions.slice(0, 4)) await op.pan(dx, dy, 2500);
      for (let i = 0; i < 2; i++) await op.zoomOut(2500);
    });
    // Harder than real use: z15 → z19 → z15 with eight pans at both ends,
    // as fast as the tiles are requested.
    const aggressive = await session('aggressive', async (op) => {
      for (let i = 0; i < 4; i++) await op.zoomIn(450);
      for (const [dx, dy] of directions) await op.pan(dx, dy, 450);
      for (let i = 0; i < 4; i++) await op.zoomOut(450);
      for (const [dx, dy] of directions) await op.pan(dx, dy, 450);
    });
    const normalPerMin = normal.count * 60 / normal.elapsedS;
    const firstView = Math.max(normal.atLoad.all, aggressive.atLoad.all);
    console.log(`  budget ${TILE_BUDGET_PER_MIN}/min: normal extrapolates to ${normalPerMin.toFixed(0)}/min ` +
      `(${(TILE_BUDGET_PER_MIN / normalPerMin).toFixed(2)}x headroom), aggressive used ${aggressive.count} of it in ` +
      `${aggressive.elapsedS.toFixed(1)} s (${(TILE_BUDGET_PER_MIN / aggressive.count).toFixed(2)}x headroom)`);
    console.log(`  admission ${TILE_MAX_IN_FLIGHT}: a cold first view of both overlays is ${firstView} tiles`);
    assert(firstView <= TILE_MAX_IN_FLIGHT,
      `a cold first view is ${firstView} tiles: more than the ${TILE_MAX_IN_FLIGHT} admission slots`);
    assert(normalPerMin * 1.3 <= TILE_BUDGET_PER_MIN,
      `normal session extrapolates to ${normalPerMin.toFixed(0)}/min: budget ${TILE_BUDGET_PER_MIN} is under 1.3x that`);
    assert(aggressive.count <= TILE_BUDGET_PER_MIN,
      `aggressive session ${aggressive.count} tiles exceeds the budget ${TILE_BUDGET_PER_MIN}`);
    console.log('  ✅ with two DHM overlays on, a cold view fits the admission bound, a normal session sustained for a minute leaves headroom in the default budget, and the aggressive burst fits in it');
  } catch (e) {
    failures++;
    console.log('  ❌ ' + e.message);
  } finally {
    await browser.close();
  }
  console.log(`\n${failures ? 'FAILED' : 'PASSED'}`);
  process.exit(failures ? 1 : 0);
})();
