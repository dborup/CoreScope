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
 *      after a first and after a second batch (bounded, not growing);
 *   4. on a 375x560 touch phone from a cold start, after more routes than
 *      the cap, a tap on the newest route still opens its popup (what the
 *      viewer sees and can click is unchanged).
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

async function openLive(page) {
  await page.goto(BASE + '/#/live', { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() => typeof window._liveWSHandler === 'function' && !!window._liveWSHandler() &&
    !!window._liveGetMap() && !!window._liveTestSeams && window.HopResolver && HopResolver.ready() &&
    Object.keys(window._liveNodeData()).length >= 2);
  await page.evaluate(() => { if (window._liveVcrSetMode) window._liveVcrSetMode('LIVE'); });
}

// Resolves once the map's centre and zoom are unchanged over 400 ms.
async function waitForStillMap(page) {
  await page.waitForFunction(() => new Promise((resolve) => {
    const map = window._liveGetMap();
    const v = () => map.getCenter().toString() + map.getZoom();
    const before = v();
    setTimeout(() => resolve(v() === before), 400);
  }), null, { timeout: 15000 });
}

// A point on the newest clickable route that a tap reaches (the overlay
// canvas is topmost there, not a marker, panel or control).
function newestRoutePoint(page) {
  return page.evaluate(() => {
    const map = window._liveGetMap();
    const rect = map.getContainer().getBoundingClientRect();
    const polys = [];
    map.eachLayer((g) => {
      if (!(g instanceof L.LayerGroup)) return;
      g.getLayers().forEach((l) => {
        if (l instanceof L.Polyline && l.options.interactive && l.options.opacity === 0 && map.hasLayer(l)) polys.push(l);
      });
    });
    for (const l of polys.reverse()) {
      const ll = l.getLatLngs();
      const a = map.latLngToContainerPoint(ll[0]), b = map.latLngToContainerPoint(ll[ll.length - 1]);
      for (const t of [0.5, 0.4, 0.6, 0.3, 0.7]) {
        const x = rect.left + a.x + (b.x - a.x) * t, y = rect.top + a.y + (b.y - a.y) * t;
        const el = document.elementFromPoint(x, y);
        if (el && el.tagName === 'CANVAS' && el.closest('.leaflet-overlay-pane')) return { x, y };
      }
    }
    return null;
  });
}

(async () => {
  const browser = await chromium.launch({ headless: true });
  const page = await browser.newPage({ viewport: { width: 1400, height: 900 } });
  page.setDefaultTimeout(20000);
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  try {
    await openLive(page);

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

    console.log('\n--- phone 375x560, touch, cold start ---');
    const phone = await browser.newContext({ viewport: { width: 375, height: 560 }, isMobile: true, hasTouch: true });
    const pp = await phone.newPage();
    pp.setDefaultTimeout(20000);
    pp.on('pageerror', (e) => errors.push(e.message));
    await openLive(pp);
    await feed(pp, CAP + 20, 2 * BATCH);
    await step('phone: groups stay at the cap', async () => {
      const c = await groupCensus(pp);
      assert(c.offMap === 0 && c.clickable <= CAP, JSON.stringify(c));
    });
    await step('phone: a tap on the newest route opens its popup', async () => {
      // A cold start may still be settling the view; tap once it holds
      // still, and re-aim if the map moved under the tap.
      let opened = false, last = null;
      for (let attempt = 0; attempt < 3 && !opened; attempt++) {
        await waitForStillMap(pp);
        last = await newestRoutePoint(pp);
        assert(last, 'no tappable point on any clickable route');
        await pp.touchscreen.tap(last.x, last.y);
        opened = await pp.locator('.path-info-popup').waitFor({ state: 'visible', timeout: 2500 })
          .then(() => true, () => false);
      }
      assert(opened, 'no popup after 3 taps, last at ' + JSON.stringify(last));
    });
    await phone.close();

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
