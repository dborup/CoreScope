/**
 * E2E (#972): the Live map's clickable-path layer stays bounded on traffic.
 *
 * Every animated route leaves an invisible, clickable polyline in
 * `clickablePathsLayer` (an L.layerGroup), kept for 30 s and capped at 50.
 * Eviction used to call `poly.remove()`, which takes the polyline off the
 * map but leaves it in the group's `_layers`, so the group kept every route
 * ever drawn: about 5,000 polylines (their L.LatLng / L.Point arrays and the
 * click closure) per hour on real traffic, the ~30 MB/h heap growth of #972.
 *
 * The test loads /#/live, feeds packets through the real WS handler
 * (window._liveWSHandler(), the function app.js calls per frame) and, once
 * the animations have drained, checks through public Leaflet API only:
 *   1. the cap was reached, so eviction actually ran;
 *   2. no layer group on the map holds a polyline that is off the map;
 *   3. the clickable polylines held by layer groups are at most the cap,
 *      after a first and after a second batch (bounded, not growing).
 *
 * Usage: BASE_URL=http://localhost:13581 node test-issue-972-live-clickable-paths-bounded-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const CAP = 50;          // CLICKABLE_PATH_MAX in live.js
const BATCH = 120;       // > CAP, so every batch evicts
const GAP_MS = 60;       // ~16 packets/s; one route animates ~1.3 s, under the 20-anim cap

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

// Feeds `n` two-position ADVERT routes (sender GPS anchor + one resolved hop
// between fixture nodes with GPS) and waits until every animation is done.
async function feed(page, n, offset) {
  await page.evaluate(async ({ n, offset, gap }) => {
    const nd = window._liveNodeData();
    const keys = Object.keys(nd).filter((k) => nd[k].lat && nd[k].lon);
    const handler = window._liveWSHandler();
    for (let i = 0; i < n; i++) {
      const j = offset + i;
      const a = keys[j % keys.length], c = keys[(j + 1) % keys.length];
      handler({ type: 'packet', data: {
        id: 972000000 + j,
        hash: (0x972000000000 + j).toString(16).padStart(16, '0'),
        timestamp: new Date().toISOString(),
        observer_id: 'E2E972', observer_name: 'e2e-972',
        path_json: JSON.stringify([c.slice(0, 2)]),
        resolved_path: JSON.stringify([c]),
        decoded: { header: { payloadTypeName: 'ADVERT' },
          payload: { type: 'ADVERT', pubKey: a, name: nd[a].name, lat: nd[a].lat, lon: nd[a].lon } },
      } });
      await new Promise((r) => setTimeout(r, gap));
    }
  }, { n, offset, gap: GAP_MS });
  await page.waitForFunction(() => window._liveTestSeams.getAnimCount() === 0 &&
    document.getElementById('liveAnimCount').textContent === '0', null, { timeout: 30000 });
}

// What the layer groups on the map hold, read through public Leaflet API.
function groupCensus(page) {
  return page.evaluate(() => {
    const map = window._liveGetMap();
    let clickable = 0, offMap = 0;
    map.eachLayer((g) => {
      if (!(g instanceof L.LayerGroup)) return;
      g.getLayers().forEach((l) => {
        if (!(l instanceof L.Polyline)) return;
        if (!map.hasLayer(l)) offMap++;
        if (l.options.interactive && l.options.opacity === 0) clickable++;
      });
    });
    return { clickable, offMap };
  });
}

(async () => {
  const browser = await chromium.launch({ headless: true });
  const page = await browser.newPage({ viewport: { width: 1400, height: 900 } });
  page.setDefaultTimeout(20000);
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  try {
    await page.goto(BASE + '/#/live', { waitUntil: 'domcontentloaded' });
    await page.waitForFunction(() => typeof window._liveWSHandler === 'function' && !!window._liveWSHandler() &&
      !!window._liveGetMap() && !!window._liveTestSeams && window.HopResolver && HopResolver.ready() &&
      Object.keys(window._liveNodeData()).length >= 2);
    await page.evaluate(() => { if (window._liveVcrSetMode) window._liveVcrSetMode('LIVE'); });

    await feed(page, BATCH, 0);
    const first = await groupCensus(page);
    console.log('  after ' + BATCH + ' packets: ' + JSON.stringify(first));
    await step('the cap is reached, so eviction ran', async () => {
      assert(first.clickable >= CAP || first.offMap > 0,
        'only ' + first.clickable + ' clickable paths after ' + BATCH + ' packets; the feed did not draw routes');
    });
    await step('no layer group keeps a polyline that is off the map', async () => {
      assert(first.offMap === 0, first.offMap + ' evicted polylines are still held by a layer group');
    });
    await step('clickable polylines in layer groups <= ' + CAP, async () => {
      assert(first.clickable <= CAP, first.clickable + ' clickable polylines held, cap is ' + CAP);
    });

    await feed(page, BATCH, BATCH);
    const second = await groupCensus(page);
    console.log('  after ' + 2 * BATCH + ' packets: ' + JSON.stringify(second));
    await step('a second batch does not grow the groups', async () => {
      assert(second.offMap === 0 && second.clickable <= CAP,
        'after ' + 2 * BATCH + ' packets: ' + JSON.stringify(second));
    });
    await step('no page errors', async () => {
      assert(errors.length === 0, errors.join(' | '));
    });
  } catch (e) {
    failed++; console.error('  ✗ setup: ' + e.message);
  } finally {
    await browser.close();
  }
  console.log('\n' + passed + ' passed, ' + failed + ' failed');
  process.exit(failed ? 1 : 0);
})();
