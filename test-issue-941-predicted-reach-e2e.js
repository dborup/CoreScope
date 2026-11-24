/**
 * E2E (#941 M1): the Predicted reach tool on the Map page, in a real
 * browser.
 *
 * The CI server has no DHM token and no foreign fill, so the reach is
 * switched on the way an operator's config would: /api/config/client is
 * intercepted and given the `coverage` block cmd/server sends
 * (CoverageClientConfig) and the `dhm` block for the credit, and
 * /api/terrain/coverage and its raster are a stub in the server's shape
 * (CoverageResponse; a synthetic loss raster: free-space loss growing with
 * distance, plus a 20 dB shadow east of the centre). Nothing here reaches
 * a real network.
 *
 * Asserts:
 *   1. arm with the toggle, click the map: one request for that point,
 *      the canvas layer, the legend with three classes and the DHM credit
 *      with its links; the address bar carries reach=;
 *   2. moving the power slider recolours the layer and the legend with no
 *      new request to /api/terrain/coverage or its raster;
 *   3. a #/map?reach=…&rkm=…&rpwr=… link restores the layer without a
 *      click, sends r but never the power, and shows the linked power;
 *   4. node detail's "Predicted reach" chip opens the node's reach;
 *   5. 375×560 with touch: no horizontal scroll, the panel's close button
 *      and the toggle are on screen and on top; a recolour of the
 *      40 km-sized raster under 4× CPU throttling stays within 50 ms;
 *      from a cold #/map, taps open the panel and, scrolled down to the
 *      link budget, the sticky head's × is still on top and closes it;
 *   6. dark and light: the layer's colours are the theme's CSS variables;
 *   7. a refusal shows the panel's words, not the server's text;
 *   8. no console error, and no request leaves localhost.
 *
 * SHOTS_DIR, when set, receives small JPEG screenshots.
 *
 * Usage:
 *   BASE_URL=http://localhost:13581 node test-issue-941-predicted-reach-e2e.js
 */
'use strict';
const path = require('path');
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const SHOTS = process.env.SHOTS_DIR || '';
const COVERAGE_PATH = '/api/terrain/coverage';
const RASTER_PATH = '/api/terrain/coverage/raster';

// internal/dhm/dhm.go, as cmd/server sends it in the dhm block.
const DHM_ATTRIBUTION = 'Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel, CC BY 4.0';
const DHM_DERIVED = DHM_ATTRIBUTION + ' — data bearbejdet';
const DHM_BLOCK = {
  enabled: true, url: '/api/map/dhm/wms', layer: 'dhm_terraen_skyggekort', minZoom: 7, maxZoom: 19, profile: false,
  bounds: [[54.4265, 7.99125], [57.778, 15.5995]], attribution: DHM_ATTRIBUTION,
  datasetName: 'Danmarks Højdemodel', datasetUrl: 'https://dataforsyningen.dk/data/930',
  licenseName: 'CC BY 4.0', licenseUrl: 'https://creativecommons.org/licenses/by/4.0/',
  attributionDerived: DHM_DERIVED, overlays: [],
};
const RADIO = { txPowerDbm: 22, antennaGainDbi: 3, rxGainDbi: 3, lossDb: 0, sensitivityDbm: -130 };
const COVERAGE_BLOCK = {
  enabled: true, maxRadiusKm: 40, radiusKm: 40, resolutionM: 30, rxHeightM: 2,
  pointAntenna: { m: 2, source: 'point-default' }, radio: RADIO, dhm: true,
};
const AARHUS = { lat: 56.1629, lon: 10.2039 };

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + (e && e.stack || e)); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

// ── The stub: the server's shape, a synthetic raster ──────────────────────

function mercY(lat) { return Math.log(Math.tan(Math.PI / 4 + lat * Math.PI / 360)); }
function latOfMercY(y) { return (2 * Math.atan(Math.exp(y)) - Math.PI / 2) * 180 / Math.PI; }

/* rasterFor is a W×H loss raster around c out to radiusKm, Web Mercator
 * aligned: 91 dB at 1 km + 20·log10(d km) (+20 dB east of the centre). */
function rasterFor(c, radiusKm) {
  const W = 400, H = 400;
  const dLat = radiusKm / 111.2 * 1.05, dLon = dLat / Math.cos(c.lat * Math.PI / 180);
  const s = c.lat - dLat, n = c.lat + dLat, w = c.lon - dLon, e = c.lon + dLon;
  const y0 = mercY(n), y1 = mercY(s);
  const bytes = Buffer.alloc(W * H);
  for (let row = 0; row < H; row++) {
    const lat = latOfMercY(y0 + (y1 - y0) * (row + 0.5) / H);
    for (let col = 0; col < W; col++) {
      const lon = w + (e - w) * (col + 0.5) / W;
      const dy = (lat - c.lat) * 111.2, dx = (lon - c.lon) * 111.2 * Math.cos(c.lat * Math.PI / 180);
      const d = Math.hypot(dx, dy);
      let v = 255;
      if (d <= radiusKm) {
        const loss = 91 + 20 * Math.log10(Math.max(d, 0.05)) + (dx > 0 ? 20 : 0) + 25;
        v = Math.max(0, Math.min(254, Math.round((loss - 60) / 0.5)));
      }
      bytes[row * W + col] = v;
    }
  }
  return { bytes, W, H, bounds: [[s, w], [n, e]] };
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #941 M1 Predicted reach against ${BASE} ===\n`);

  // Fixture nodes, by the API: one with a real fix.
  const node = await (async () => {
    const ctx = await browser.newContext();
    const page = await ctx.newPage();
    await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
    const list = await page.evaluate(async () => {
      const r = await fetch('/api/nodes?limit=2000');
      const d = await r.json();
      return (d.nodes || d).map(n => ({ pk: n.public_key, name: n.name, lat: n.lat, lon: n.lon }));
    });
    await ctx.close();
    return list.find(n => n.lat != null && n.lon != null && !(n.lat === 0 && n.lon === 0) && /^[0-9a-f]{64}$/i.test(n.pk) && n.name);
  })();
  assert(node, 'the fixture needs a node with a fix');

  async function newCtx(o) {
    o = o || {};
    const ctx = await browser.newContext({
      viewport: o.viewport || { width: 1280, height: 900 },
      hasTouch: !!o.touch, isMobile: !!o.touch,
    });
    const rec = { consoleErrors: [], all: [], coverage: [], raster: 0 };
    await ctx.route(BASE + '/api/config/client', async (route) => {
      const response = await route.fetch();
      const config = await response.json();
      config.dhm = DHM_BLOCK;
      config.coverage = COVERAGE_BLOCK;
      await route.fulfill({ response, json: config });
    });
    const rasters = new Map();
    await ctx.route(url => new URL(url).pathname === COVERAGE_PATH, async (route) => {
      const u = new URL(route.request().url());
      rec.coverage.push(u.search);
      if (o.refuse) {
        await route.fulfill({ status: o.refuse.status, contentType: 'application/json', body: JSON.stringify(o.refuse.body), headers: o.refuse.headers || {} });
        return;
      }
      const at = u.searchParams.get('at');
      const isNode = /^[0-9a-f]{64}$/.test(at);
      const c = isNode ? { lat: node.lat, lon: node.lon } : { lat: Number(at.split(',')[0]), lon: Number(at.split(',')[1]) };
      const r = Number(u.searchParams.get('r') || 40);
      const key = (u.search.length.toString(16) + 'a'.repeat(32)).slice(0, 32);
      const ras = rasterFor(c, r);
      rasters.set(key, ras.bytes);
      const body = {
        key, center: { lat: c.lat, lon: c.lon, kind: isNode ? 'node' : 'point', role: isNode ? 'repeater' : undefined, groundM: 34.2 },
        params: { radiusKm: r, resolutionM: 30, txHeightM: isNode ? 10 : 2, txHeightSource: isNode ? 'role-default' : 'point-default', txAssumed: true,
          rxHeightM: 2, rxHeightSource: 'default', freqMHz: 869.618, freqSource: 'firmware', k: 4 / 3, kSource: 'default', model: 'bullington-p526-v1' },
        raster: { url: RASTER_PATH + '?key=' + key, width: ras.W, height: ras.H, bounds: ras.bounds, encoding: 'loss-u8',
          lossBaseDb: 60, lossStepDb: 0.5, noData: 255, coveredPct: 100, bytes: ras.bytes.length },
        sources: [{ id: 'dhm-dtm', resolutionM: 30, model: 'dtm' }],
        attribution: DHM_DERIVED,
      };
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(body) });
    });
    await ctx.route(url => new URL(url).pathname === RASTER_PATH, async (route) => {
      rec.raster++;
      const key = new URL(route.request().url()).searchParams.get('key');
      const bytes = rasters.get(key);
      if (!bytes) { await route.fulfill({ status: 404, contentType: 'application/json', body: '{"error":"not found"}' }); return; }
      await route.fulfill({ status: 200, contentType: 'application/octet-stream', body: bytes });
    });
    // Every third-party request is stubbed; nothing may reach a real network.
    await ctx.route(/^https?:\/\/(?!localhost|127\.0\.0\.1)/i, async (route) => {
      await route.fulfill({ status: 200, contentType: 'image/png', body: Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGM4ceIEAAS0AlkWLoFAAAAAAElFTkSuQmCC', 'base64') });
    });
    await ctx.addInitScript(({ theme }) => {
      try {
        localStorage.setItem('meshcore-theme', theme);
        localStorage.setItem('meshcore-map-clustering', 'false');
        ['row-swipe', 'tab-swipe', 'edge-drawer', 'pull-refresh']
          .forEach(k => localStorage.setItem('meshcore-gesture-hints-' + k, 'seen'));
      } catch (_) {}
      const apply = () => document.documentElement && document.documentElement.setAttribute('data-theme', theme);
      apply();
      document.addEventListener('DOMContentLoaded', apply);
    }, { theme: o.theme || 'light' });
    const page = await ctx.newPage();
    page.on('console', m => { if (m.type() === 'error') rec.consoleErrors.push(m.text()); });
    page.on('pageerror', e => rec.consoleErrors.push(String(e)));
    page.on('request', r => rec.all.push(r.url()));
    return { ctx, page, rec };
  }

  async function waitLayer(page) {
    await page.waitForSelector('[data-testid="reach-panel"]:not([hidden])', { timeout: 20000 });
    await page.waitForFunction(() => {
      const c = document.querySelector('canvas.reach-layer');
      return c && c.width > 0 && c.isConnected && !document.querySelector('[data-testid="reach-panel"].is-busy');
    }, null, { timeout: 15000 });
    await page.waitForTimeout(200);
  }

  /* pixelAt reads the layer's RGBA at a geographic point. */
  function pixelAt(page, lat, lon) {
    return page.evaluate(({ lat, lon }) => {
      const c = document.querySelector('canvas.reach-layer');
      const r = c.getBoundingClientRect();
      const map = document.querySelector('#leaflet-map');
      const lm = window.__reachTestMap;
      if (lm) {
        const p = lm.latLngToContainerPoint([lat, lon]);
        const mr = map.getBoundingClientRect();
        const x = Math.floor((mr.left + p.x - r.left) / r.width * c.width);
        const y = Math.floor((mr.top + p.y - r.top) / r.height * c.height);
        return Array.from(c.getContext('2d').getImageData(x, y, 1, 1).data);
      }
      return null;
    }, { lat, lon });
  }

  function cssColor(page, name) {
    return page.evaluate((n) => {
      const v = getComputedStyle(document.documentElement).getPropertyValue(n).trim();
      return window.PredictedReach.parseColor(v);
    }, name);
  }

  function noNetwork(rec) {
    const bad = rec.all.filter(u => !/^https?:\/\/(localhost|127\.0\.0\.1)[:/]/i.test(u) && !u.startsWith('data:') && !u.startsWith('blob:'));
    const real = bad.filter(u => /dataforsyningen|amazonaws|copernicus/i.test(u));
    assert(real.length === 0, 'a request reached a real upstream: ' + real.join(', '));
    const errs = rec.consoleErrors.filter(e => !/Failed to load resource/.test(e));
    assert(errs.length === 0, 'console errors: ' + errs.join(' | '));
  }

  async function shot(page, name, el) {
    if (!SHOTS) return;
    const target = el ? await page.$(el) : page;
    await target.screenshot({ path: path.join(SHOTS, name), type: 'jpeg', quality: 60 });
  }

  /* hookMap lets pixelAt project: the page keeps its map private, so the
   * first image overlay added (the reach layer) hands it over. */
  async function hookMap(page) {
    await page.addInitScript(() => {
      const t = setInterval(() => {
        if (window.L && window.L.ImageOverlay && !window.L.ImageOverlay.__reachHooked) {
          window.L.ImageOverlay.__reachHooked = true;
          const onAdd = window.L.ImageOverlay.prototype.onAdd;
          window.L.ImageOverlay.prototype.onAdd = function (map) { window.__reachTestMap = map; return onAdd.call(this, map); };
          clearInterval(t);
        }
      }, 5);
    });
  }

  // ── 1: arm, click ───────────────────────────────────────────────────────
  for (const theme of ['light', 'dark']) {
    await step(`${theme}: arm with the toggle, click the map: layer, legend, credit, link`, async () => {
      const { ctx, page, rec } = await newCtx({ theme });
      try {
        await hookMap(page);
        await page.goto(`${BASE}/#/map?lat=${AARHUS.lat}&lon=${AARHUS.lon}&zoom=10`, { waitUntil: 'domcontentloaded' });
        await page.waitForSelector('#leaflet-map[data-loaded="true"]', { timeout: 30000 });
        await page.waitForSelector('[data-testid="map-reach-toggle"]', { timeout: 15000 });
        assert(rec.coverage.length === 0, 'a request before the tool was used');
        await page.click('[data-testid="map-reach-toggle"]');
        assert(await page.isVisible('[data-testid="reach-hint"]'), 'no hint after arming');
        assert(await page.getAttribute('[data-testid="map-reach-toggle"]', 'aria-pressed') === 'true', 'toggle not pressed');
        const box = await page.locator('#leaflet-map').boundingBox();
        await page.mouse.click(box.x + box.width / 2, box.y + box.height / 2);
        await waitLayer(page);
        assert(rec.coverage.length === 1 && rec.raster === 1, `requests: ${rec.coverage.length} json, ${rec.raster} raster`);
        const q = new URLSearchParams(rec.coverage[0]);
        const [la, lo] = q.get('at').split(',').map(Number);
        assert(Math.abs(la - AARHUS.lat) < 0.02 && Math.abs(lo - AARHUS.lon) < 0.03, 'clicked point ' + q.get('at'));
        for (const k of q.keys()) assert(['at'].includes(k), 'unexpected request key ' + k);
        const legend = await page.$$eval('[data-testid="reach-legend"] .reach-legend-row', rows => rows.map(r => r.getAttribute('data-class') + ':' + r.textContent));
        assert(legend.length === 3 && /^good:Good/.test(legend[0]) && /^marginal/.test(legend[1]) && /^weak/.test(legend[2]), 'legend ' + legend);
        const credit = await page.evaluate(() => {
          const p = document.querySelector('[data-testid="reach-credit-dhm"]');
          return p && { text: p.textContent, links: Array.from(p.querySelectorAll('a')).map(a => [a.textContent, a.getAttribute('href'), a.getAttribute('target')]) };
        });
        assert(credit && credit.text === DHM_DERIVED, 'credit ' + JSON.stringify(credit));
        assert(credit.links.length === 2 && credit.links[0][1] === DHM_BLOCK.datasetUrl && credit.links[1][1] === DHM_BLOCK.licenseUrl && credit.links[0][2] === '_blank', 'credit links ' + JSON.stringify(credit.links));
        // The address bar is written debounced (250 ms) after the click.
        await page.waitForFunction(() => /[?&]reach=/.test(location.hash), null, { timeout: 3000 })
          .catch(() => { throw new Error('no reach= in the address bar: ' + page.url()); });
        // The colours are the theme's: north of the centre (no shadow) at
        // 3 km is good, east at 15 km (shadowed) weak or none.
        const good = await cssColor(page, '--reach-good');
        const px = await pixelAt(page, AARHUS.lat + 3 / 111.2, AARHUS.lon);
        assert(px && px[3] === 255 && px[0] === good[0] && px[1] === good[1] && px[2] === good[2], `pixel ${px} vs --reach-good ${good}`);
        await shot(page, `941-click-${theme}-1280.jpg`);
        noNetwork(rec);
      } finally { await ctx.close(); }
    });
  }

  // ── 2: the power slider recolours without a request ─────────────────────
  await step('the power slider recolours the layer and legend with no new request', async () => {
    const { ctx, page, rec } = await newCtx({});
    try {
      await hookMap(page);
      await page.goto(`${BASE}/#/map?reach=${AARHUS.lat},${AARHUS.lon}&rkm=20`, { waitUntil: 'domcontentloaded' });
      await waitLayer(page);
      const before = { json: rec.coverage.length, raster: rec.raster };
      // West of the centre at 8 km: 91 + 18 + 25 = 134 dB → margin 24: good.
      // East at 8 km: 154 dB → margin 4: marginal at 22 dBm, weak at 20,
      // none at 17.
      const east = [AARHUS.lat, AARHUS.lon + 8 / (111.2 * Math.cos(AARHUS.lat * Math.PI / 180))];
      const colours = { marginal: await cssColor(page, '--reach-marginal'), weak: await cssColor(page, '--reach-weak') };
      const at22 = await pixelAt(page, east[0], east[1]);
      assert(at22[3] === 255 && at22.slice(0, 3).join() === colours.marginal.join(), 'east at 22 dBm: ' + at22);
      const pct22 = await page.textContent('[data-testid="reach-share-good"]');
      await page.evaluate(() => {
        const s = document.querySelector('[data-testid="reach-power"]');
        s.value = '20';
        s.dispatchEvent(new Event('input', { bubbles: true }));
      });
      await page.waitForTimeout(150);
      const at20 = await pixelAt(page, east[0], east[1]);
      assert(at20.slice(0, 3).join() === colours.weak.join(), 'east at 20 dBm: ' + at20);
      await page.evaluate(() => {
        const s = document.querySelector('[data-testid="reach-power"]');
        s.value = '10';
        s.dispatchEvent(new Event('input', { bubbles: true }));
      });
      await page.waitForTimeout(150);
      const at10 = await pixelAt(page, east[0], east[1]);
      assert(at10[3] === 0, 'east at 10 dBm should be uncoloured: ' + at10);
      const pct10 = await page.textContent('[data-testid="reach-share-good"]');
      assert(pct10 !== pct22, `legend share did not move: ${pct22} → ${pct10}`);
      assert(await page.textContent('[data-testid="reach-budget"]') === 'Maximum path loss 146 dB', 'budget text');
      await page.waitForTimeout(400);
      assert(rec.coverage.length === before.json && rec.raster === before.raster,
        `the slider made requests: ${rec.coverage.length - before.json} json, ${rec.raster - before.raster} raster`);
      assert(/[?&]rpwr=10(&|$)/.test(await page.evaluate(() => location.hash)), 'power not in the link');
      noNetwork(rec);
    } finally { await ctx.close(); }
  });

  // ── 2b (#960): the frequency is shown, not edited ───────────────────────
  await step('#960 the frequency is read-only with its source, rmhz is ignored and no freq is sent', async () => {
    const { ctx, page, rec } = await newCtx({});
    try {
      await page.goto(`${BASE}/#/map?reach=${AARHUS.lat},${AARHUS.lon}&rkm=20&rmhz=915`, { waitUntil: 'domcontentloaded' });
      await waitLayer(page);
      assert(await page.locator('[data-testid="reach-freq"]').count() === 0, 'the frequency field is still rendered');
      assert(await page.locator('.reach-panel input[type="number"]').count() === 3, 'geometry inputs: expected 3');
      assert(!/freq/.test(rec.coverage[0]), 'freq was sent: ' + rec.coverage[0]);
      const used = (await page.textContent('[data-testid="reach-freq-used"]')).trim();
      assert(/869\.618 MHz/.test(used) && /firmware/.test(used) && !/915/.test(used), 'frequency used: ' + used);
      assert(await page.locator('[data-testid="reach-freq-used"] input').count() === 0, 'the frequency used is editable');
      await shot(page, '960-desktop.jpg', '[data-testid="reach-panel"]');
      await page.waitForTimeout(400);
      noNetwork(rec);
    } finally { await ctx.close(); }
  });

  await step('#960 375×560 touch, cold #/map: taps open the panel, the frequency is text, not a field', async () => {
    const { ctx, page, rec } = await newCtx({ viewport: { width: 375, height: 560 }, touch: true });
    try {
      await page.goto(`${BASE}/#/map?lat=${AARHUS.lat}&lon=${AARHUS.lon}&zoom=10`, { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('#leaflet-map[data-loaded="true"]', { timeout: 30000 });
      await page.locator('[data-testid="map-reach-toggle"]').tap();
      const m = await page.locator('#leaflet-map').boundingBox();
      await page.touchscreen.tap(m.x + m.width / 2, m.y + 60);
      await waitLayer(page);
      assert(await page.locator('[data-testid="reach-freq"]').count() === 0, 'the frequency field is rendered');
      assert(/869\.618 MHz \(firmware\)/.test(await page.textContent('[data-testid="reach-freq-used"]')), 'frequency used');
      assert(!/freq/.test(rec.coverage[0]), 'freq was sent: ' + rec.coverage[0]);
      await page.evaluate(() => document.querySelector('[data-testid="reach-freq-used"]').scrollIntoView({ block: 'center' }));
      await shot(page, '960-phone.jpg');
    } finally { await ctx.close(); }
  });

  // ── 3: a link restores the layer ────────────────────────────────────────
  await step('a #/map?reach= link restores the layer, sends r but never the power', async () => {
    const { ctx, page, rec } = await newCtx({});
    try {
      await page.goto(`${BASE}/#/map?reach=${AARHUS.lat},${AARHUS.lon}&rkm=25&rtx=12&rpwr=14&rsens=-125`, { waitUntil: 'domcontentloaded' });
      await waitLayer(page);
      assert(rec.coverage.length === 1, 'requests ' + rec.coverage.length);
      const q = new URLSearchParams(rec.coverage[0]);
      assert(q.get('at') === `${AARHUS.lat},${AARHUS.lon}` && q.get('r') === '25' && q.get('ha') === '12', 'request ' + rec.coverage[0]);
      assert(!/pwr|sens|14|125/.test(rec.coverage[0]), 'the budget reached the server: ' + rec.coverage[0]);
      assert(await page.inputValue('[data-testid="reach-power"]') === '14', 'linked power not shown');
      assert(await page.textContent('[data-testid="reach-budget"]') === 'Maximum path loss 145 dB', 'linked budget');
      const centre = await page.textContent('[data-testid="reach-centre"]');
      assert(/56\.1629, 10\.2039/.test(centre) && /ground 34 m/.test(centre), 'centre ' + centre);
      // The view was fitted to the disk.
      const view = await page.evaluate(() => {
        const c = document.querySelector('canvas.reach-layer').getBoundingClientRect();
        const m = document.querySelector('#leaflet-map').getBoundingClientRect();
        return { c: [c.left, c.top, c.right, c.bottom], m: [m.left, m.top, m.right, m.bottom] };
      });
      assert(view.c[0] >= view.m[0] - 30 && view.c[2] <= view.m[2] + 30, 'the disk is not in view: ' + JSON.stringify(view));
      // The open panel leaves the map's zoom buttons uncovered.
      for (const sel of ['.leaflet-control-zoom-in', '.leaflet-control-zoom-out']) {
        const onTop = await page.evaluate((s) => {
          const e = document.querySelector(s), r = e.getBoundingClientRect();
          const top = document.elementFromPoint(r.left + r.width / 2, r.top + r.height - 3);
          return !!top && (top === e || e.contains(top));
        }, sel);
        assert(onTop, sel + ' is covered by the panel');
      }
      // Closing takes the layer and the link away.
      await page.click('[data-testid="reach-close"]');
      await page.waitForTimeout(400);
      assert(!(await page.$('canvas.reach-layer')) || !(await page.isVisible('canvas.reach-layer')), 'layer left after close');
      assert(!/reach=|rpwr|rkm/.test(await page.evaluate(() => location.hash)), 'link left after close');
      noNetwork(rec);
    } finally { await ctx.close(); }
  });

  // ── 4: node detail chip ─────────────────────────────────────────────────
  await step('node detail offers "Predicted reach", and it opens the node\'s reach', async () => {
    const { ctx, page, rec } = await newCtx({});
    try {
      const pk = node.pk.toLowerCase();
      await page.goto(`${BASE}/#/nodes/${pk}`, { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('.node-full-card', { timeout: 30000 });
      await page.waitForSelector('[data-testid="node-predicted-reach"]', { timeout: 15000 });
      assert(await page.getAttribute('[data-testid="node-predicted-reach"]', 'href') === '#/map?reach=' + pk, 'chip href');
      await page.click('[data-testid="node-predicted-reach"]');
      await waitLayer(page);
      assert(new URLSearchParams(rec.coverage[0]).get('at') === pk, 'request ' + rec.coverage[0]);
      const centre = await page.textContent('[data-testid="reach-centre"]');
      assert(centre.includes(node.name) && /antenna 10 m \(assumed\)/.test(centre), 'centre ' + centre);
      await shot(page, '941-node-light-1280.jpg');
      noNetwork(rec);
    } finally { await ctx.close(); }
  });

  // ── 5: phone ────────────────────────────────────────────────────────────
  for (const theme of ['light', 'dark']) {
    await step(`${theme} 375×560 touch: no horizontal scroll, panel and toggle reachable, recolour ≤ 50 ms at 4× CPU`, async () => {
      const { ctx, page, rec } = await newCtx({ viewport: { width: 375, height: 560 }, touch: true, theme });
      try {
        await page.goto(`${BASE}/#/map?reach=${AARHUS.lat},${AARHUS.lon}&rkm=20`, { waitUntil: 'domcontentloaded' });
        await waitLayer(page);
        const m = await page.evaluate(() => ({ sw: document.documentElement.scrollWidth, bw: document.body.scrollWidth, iw: window.innerWidth }));
        assert(m.sw <= m.iw && m.bw <= m.iw, 'horizontal scroll: ' + JSON.stringify(m));
        for (const sel of ['[data-testid="reach-close"]', '[data-testid="map-reach-toggle"]', '[data-testid="reach-power"]', '.leaflet-control-zoom-out']) {
          await page.locator(sel).scrollIntoViewIfNeeded();
          const ok = await page.evaluate((s) => {
            const e = document.querySelector(s), r = e.getBoundingClientRect();
            const x = r.left + r.width / 2, y = r.top + r.height / 2;
            const top = document.elementFromPoint(x, y);
            return r.width > 0 && r.left >= 0 && r.right <= window.innerWidth && r.top >= 0 && r.bottom <= window.innerHeight
              && !!top && (top === e || e.contains(top));
          }, sel);
          assert(ok, sel + ' is not on screen and on top');
        }
        const panel = await page.evaluate(() => {
          const r = document.querySelector('[data-testid="reach-panel"]').getBoundingClientRect();
          return { left: r.left, right: r.right, bottom: r.bottom, h: r.height };
        });
        assert(panel.left >= 0 && panel.right <= 375 && panel.bottom <= 560 && panel.h <= 560 * 0.5, 'panel ' + JSON.stringify(panel));
        await shot(page, `941-phone-${theme}-375.jpg`);
        // A phone's recolour: a 40 km raster (816×816) through the table
        // and onto a canvas, with the CPU throttled 4×.
        const cdp = await ctx.newCDPSession(page);
        await cdp.send('Emulation.setCPUThrottlingRate', { rate: 4 });
        const ms = await page.evaluate(() => {
          const PR = window.PredictedReach, n = 816;
          const raster = new Uint8Array(n * n);
          for (let i = 0; i < raster.length; i++) raster[i] = (i * 2654435761 >>> 24);
          const c = document.createElement('canvas');
          c.width = n; c.height = n;
          const ctx2 = c.getContext('2d');
          const meta = { lossBaseDb: 60, lossStepDb: 0.5, noData: 255 };
          const colors = PR.themeColors();
          const run = (b) => {
            const img = ctx2.createImageData(n, n);
            PR.paint(img.data, raster, PR.buildLut(b, meta, colors));
            ctx2.putImageData(img, 0, 0);
          };
          run(158);
          const times = [];
          for (let k = 0; k < 5; k++) { const t = performance.now(); run(150 + k); times.push(performance.now() - t); }
          times.sort((a, b) => a - b);
          return times[2];
        });
        await cdp.send('Emulation.setCPUThrottlingRate', { rate: 1 });
        console.log(`     recolour 816×816 at 4× CPU throttling: ${ms.toFixed(1)} ms (median of 5)`);
        assert(ms <= 50, `recolour ${ms} ms > 50 ms`);
        noNetwork(rec);
      } finally { await ctx.close(); }
    });
  }

  // ── 5b: phone, cold #/map: the head stays on top of a scrolled panel ──
  await step('375×560 touch, cold #/map: taps open the panel; scrolled to the budget, × is on top and closes it', async () => {
    const { ctx, page, rec } = await newCtx({ viewport: { width: 375, height: 560 }, touch: true });
    try {
      await page.goto(`${BASE}/#/map?lat=${AARHUS.lat}&lon=${AARHUS.lon}&zoom=10`, { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('#leaflet-map[data-loaded="true"]', { timeout: 30000 });
      await page.locator('[data-testid="map-reach-toggle"]').tap();
      const m = await page.locator('#leaflet-map').boundingBox();
      await page.touchscreen.tap(m.x + m.width / 2, m.y + 60);
      await waitLayer(page);
      // Down to the link budget, as a swipe inside the panel would: its
      // last line is in view below the head, not under it.
      const scrolled = await page.evaluate(() => {
        const p = document.querySelector('[data-testid="reach-panel"]');
        const b = document.querySelector('[data-testid="reach-budget"]');
        b.scrollIntoView({ block: 'end' });
        const pr = p.getBoundingClientRect(), br = b.getBoundingClientRect();
        const hr = p.querySelector('.reach-head').getBoundingClientRect();
        const top = document.elementFromPoint(br.left + 4, br.top + br.height / 2);
        return { top: p.scrollTop, belowHead: br.top >= hr.bottom - 1, inPanel: br.bottom <= pr.bottom + 1,
          budgetOnTop: !!top && (top === b || b.contains(top)) };
      });
      assert(scrolled.top > 100 && scrolled.belowHead && scrolled.inPanel && scrolled.budgetOnTop,
        'the budget is not in view: ' + JSON.stringify(scrolled));
      await page.waitForTimeout(100);
      const close = await page.evaluate(() => {
        const e = document.querySelector('[data-testid="reach-close"]');
        const p = document.querySelector('[data-testid="reach-panel"]').getBoundingClientRect();
        const r = e.getBoundingClientRect();
        const x = r.left + r.width / 2, y = r.top + r.height / 2;
        const top = document.elementFromPoint(x, y);
        return { x, y, onTop: !!top && (top === e || e.contains(top)), inPanel: r.top >= p.top - 1 && r.bottom <= p.bottom + 1 };
      });
      assert(close.onTop && close.inPanel, '× scrolled away with the panel: ' + JSON.stringify(close));
      await shot(page, '941-phone-sticky-head-light-375x560.jpg');
      // A tap where × is, with no scrolling first.
      await page.touchscreen.tap(close.x, close.y);
      await page.waitForSelector('[data-testid="reach-panel"]', { state: 'hidden', timeout: 5000 });
      await page.waitForFunction(() => !/[?&]reach=/.test(location.hash), null, { timeout: 5000 })
        .catch(() => { throw new Error('reach= stayed in the hash'); });
      noNetwork(rec);
    } finally { await ctx.close(); }
  });

  // ── 7: refusals ─────────────────────────────────────────────────────────
  await step('a refusal shows the panel\'s words, never the server\'s text', async () => {
    for (const r of [
      { status: 400, body: { error: 'point not covered' }, want: /No terrain data there/ },
      { status: 400, body: { error: 'radius too large', maxRadiusKm: 25 }, want: /limited to 25 km/ },
      { status: 503, body: { error: 'predicted reach busy' }, headers: { 'Retry-After': '5' }, want: /busy.*in 5 s/ },
      { status: 502, body: { error: '<img src=x onerror=alert(1)>' }, want: /unavailable/ },
    ]) {
      const { ctx, page, rec } = await newCtx({ refuse: r });
      try {
        await page.goto(`${BASE}/#/map?reach=${AARHUS.lat},${AARHUS.lon}`, { waitUntil: 'domcontentloaded' });
        await page.waitForSelector('[data-testid="reach-status"].is-error', { timeout: 15000 });
        const text = await page.textContent('[data-testid="reach-status"]');
        assert(r.want.test(text), `${r.status}: ${text}`);
        assert(!(await page.$('[data-testid="reach-panel"] img')), 'markup from the server reached the panel');
        assert(rec.raster === 0, 'a raster was asked for after a refusal');
      } finally { await ctx.close(); }
    }
  });

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed\n`);
  process.exit(failed ? 1 : 0);
})().catch(e => { console.error(e); process.exit(1); });
