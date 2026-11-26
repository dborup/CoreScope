/**
 * E2E (#979 item 1): an open Map node popup survives a background node
 * refresh, in a real browser.
 *
 * The Map page reloads its nodes for every WebSocket batch that holds an
 * ADVERT (loadNodes → renderMarkers). Before #979 that rebuilt every marker
 * through clearLayers(), which removed the marker that owned the open popup
 * and so closed it, its #959 actions with it. The app's WebSocket is
 * replaced with page.routeWebSocket and fed ADVERT batches; /api/config/client
 * gets the dhm and coverage blocks so the popup carries Terrain profile and
 * Predicted reach, and the coverage is a stub in the server's shape.
 *
 * Asserts, from a cold #/map:
 *   1. at 1440×900 (clustering off and on) and 375×560 touch, light and
 *      dark: an open popup stays open through two ADVERT refreshes (one from
 *      the client cache, one that refetches /api/nodes), the same popup
 *      element with its actions, no popupclose and no flicker of
 *      #map-wrap.has-map-popup, while every other marker was rebuilt; its
 *      Predicted reach is then hit by a finger at its middle and opens the
 *      reach for that node; then (the issue's phone flow) the next node's
 *      popup makes the phone's reach sheet step aside through two more
 *      refreshes, and a tap on the map closes it and the sheet comes back;
 *   1b. with clustering on and 2,000 nodes (the fixture's and clones of
 *      them), at 1440×900 and 375×560 touch, CPU 4× and the cluster group's
 *      chunkInterval lowered to 20 ms so markercluster adds them in chunks
 *      on any machine: the popup stays open through two refreshes, with one
 *      remove handler on its marker; a filter change during a chunked add
 *      (two renderMarkers calls) is run once after it, and no node has two
 *      markers in the group;
 *   2. a popup whose node is filtered out (its role unchecked) closes, one
 *      whose node is gone from the next /api/nodes answer closes, and with
 *      clustering on, one kept through a refresh still closes when a zoom
 *      out puts its marker in a cluster (Leaflet's own close is back on);
 *   3. a refresh that renames, moves and ages the popup's node leaves the
 *      open popup as it is, its marker takes the node's new place and stale
 *      icon (where a marker built afresh is), and the popup opened next
 *      shows the new data; Show Neighbors in a popup closes it (1440×900
 *      click, 375×560 tap);
 *   4. a #/map?node= link centres on its node and opens its popup once: a
 *      later reload neither moves the view nor takes over the popup the
 *      user opened since;
 *   5. no page error, and no request leaves localhost.
 *
 * SHOTS_DIR, when set, receives JPEG screenshots (≤ 960 px, quality 60).
 *
 * Usage:
 *   BASE_URL=http://localhost:13581 node test-issue-979-map-popup-advert-e2e.js
 */
'use strict';
const path = require('path');
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
if (!['localhost', '127.0.0.1', '[::1]'].includes(new URL(BASE).hostname)) throw new Error('local server only');
const SHOTS = process.env.SHOTS_DIR || '';
const COVERAGE_PATH = '/api/terrain/coverage';
const RASTER_PATH = '/api/terrain/coverage/raster';

const DHM_BLOCK = {
  enabled: true, url: '/api/map/dhm/wms', layer: 'dhm_terraen_skyggekort', minZoom: 7, maxZoom: 19, profile: true,
  bounds: [[54.4265, 7.99125], [57.778, 15.5995]], attribution: 'DHM, CC BY 4.0',
  datasetName: 'Danmarks Højdemodel', datasetUrl: 'https://dataforsyningen.dk/data/930',
  licenseName: 'CC BY 4.0', licenseUrl: 'https://creativecommons.org/licenses/by/4.0/',
  attributionDerived: 'DHM, CC BY 4.0 — data bearbejdet', overlays: [],
};
const COVERAGE_BLOCK = {
  enabled: true, maxRadiusKm: 40, radiusKm: 40, resolutionM: 30, rxHeightM: 2,
  pointAntenna: { m: 2, source: 'point-default' },
  radio: { txPowerDbm: 22, antennaGainDbi: 3, rxGainDbi: 3, lossDb: 0, sensitivityDbm: -130 }, dhm: true,
};
const PNG_1X1 = Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGM4ceIEAAS0AlkWLoFAAAAAAElFTkSuQmCC', 'base64');
const POPUP = '.leaflet-popup .map-popup[data-node-popup]';
const REACH = '.leaflet-popup [data-testid="node-predicted-reach"]';
const PROFILE = '.leaflet-popup [data-testid="node-terrain-profile"]';
const ADVERT = JSON.stringify({ type: 'packet', data: {
  id: 979001, hash: '979a000000000001', observer_id: 'obs979', observer_name: 'E2E-979', snr: 5, rssi: -90,
  timestamp: new Date().toISOString(), decoded: { header: { payloadTypeName: 'ADVERT' }, payload: {}, path: { hops: [] } },
} });

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + (e && e.stack || e)); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #979 Map popup through ADVERT refreshes against ${BASE} ===\n`);

  /* clonedPage answers one page of /api/nodes (fetchAllNodes asks 500 at a
   * time) from the fixture's nodes and clones of them up to o.clones nodes
   * in all. A clone sits 0.02°–0.08° from its node, so a node keeps its own
   * marker at zoom 15. o.nodes(list) may rewrite the list as below. */
  async function clonedPage(route, o) {
    const u = new URL(route.request().url());
    const limit = Number(u.searchParams.get('limit')) || 500;
    const offset = Number(u.searchParams.get('offset')) || 0;
    if (!o.cloneList) {
      const all = new URL(u); all.searchParams.set('limit', '5000'); all.searchParams.set('offset', '0');
      const base = ((await (await route.fetch({ url: all.toString() })).json()).nodes) || [];
      const placed = base.filter(n => n.lat && n.lon);
      const list = base.slice();
      for (let i = 0; list.length < o.clones; i++) {
        const n = placed[i % placed.length];
        const a = i * 2.399963, d = 0.02 + 0.06 * ((i * 7919) % 1000) / 1000;
        list.push(Object.assign({}, n, {
          public_key: String(n.public_key).slice(0, 56) + (0x97900000 + i).toString(16),
          name: (n.name || 'Node') + ' ~' + i, lat: n.lat + d * Math.sin(a), lon: n.lon + d * Math.cos(a),
        }));
      }
      o.cloneList = list;
    }
    const list = o.nodes ? o.nodes(o.cloneList) : o.cloneList;
    return { nodes: list.slice(offset, offset + limit), total: list.length, counts: {} };
  }

  /* newCtx: o.nodes(list) may rewrite each /api/nodes answer's node list;
   * o.clones serves that many nodes (clonedPage). */
  async function newCtx(o) {
    const ctx = await browser.newContext({
      viewport: o.viewport, hasTouch: !!o.touch, isMobile: !!o.touch,
      colorScheme: o.theme === 'dark' ? 'dark' : 'light',
    });
    const rec = { errors: [], all: [], nodes: 0, coverage: [], sockets: [] };
    await ctx.routeWebSocket(/.*/, (ws) => { rec.sockets.push(ws); });
    await ctx.route(BASE + '/api/config/client', async (route) => {
      const response = await route.fetch();
      const config = await response.json();
      config.dhm = DHM_BLOCK; config.coverage = COVERAGE_BLOCK;
      await route.fulfill({ response, json: config });
    });
    await ctx.route(url => new URL(url).pathname === '/api/nodes', async (route) => {
      rec.nodes++;
      if (o.clones) return route.fulfill({ json: await clonedPage(route, o) });
      const response = await route.fetch();
      if (!o.nodes) return route.fulfill({ response });
      const body = await response.json();
      body.nodes = o.nodes(body.nodes || []);
      await route.fulfill({ response, json: body });
    });
    await ctx.route(url => new URL(url).pathname === COVERAGE_PATH, (route) => {
      const u = new URL(route.request().url());
      rec.coverage.push(u.searchParams.get('at'));
      const c = rec.center || { lat: 37.8, lon: -122.3 }, d = 0.3;
      const body = {
        key: 'f'.repeat(32), center: { lat: c.lat, lon: c.lon, kind: 'node', role: 'repeater', groundM: 34 },
        params: { radiusKm: 40, resolutionM: 30, txHeightM: 10, txHeightSource: 'role-default', txAssumed: true,
          rxHeightM: 2, rxHeightSource: 'default', freqMHz: 869.618, freqSource: 'firmware', k: 4 / 3, kSource: 'default', model: 'bullington-p526-v1' },
        raster: { url: RASTER_PATH + '?key=' + 'f'.repeat(32), width: 32, height: 32, bounds: [[c.lat - d, c.lon - d], [c.lat + d, c.lon + d]],
          encoding: 'loss-u8', lossBaseDb: 60, lossStepDb: 0.5, noData: 255, coveredPct: 100, bytes: 1024 },
        sources: [{ id: 'dhm-dtm', resolutionM: 30, model: 'dtm' }],
        attribution: DHM_BLOCK.attributionDerived,
      };
      return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(body) });
    });
    await ctx.route(url => new URL(url).pathname === RASTER_PATH, (route) =>
      route.fulfill({ status: 200, contentType: 'application/octet-stream', body: Buffer.alloc(1024, 120) }));
    await ctx.route(url => new URL(url).pathname === '/api/map/dhm/wms', (route) =>
      route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 }));
    await ctx.route(/^https?:\/\/(?!localhost|127\.0\.0\.1)/i, (route) =>
      route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 }));
    await ctx.addInitScript((o) => {
      try {
        localStorage.setItem('meshcore-theme', o.theme);
        localStorage.setItem('meshcore-map-clustering', o.clustering ? 'true' : 'false');
        ['row-swipe', 'tab-swipe', 'edge-drawer', 'pull-refresh']
          .forEach(k => localStorage.setItem('meshcore-gesture-hints-' + k, 'seen'));
      } catch (_) {}
      window.__maps = [];
      let realL;
      Object.defineProperty(window, 'L', {
        configurable: true,
        get() { return realL; },
        set(v) {
          realL = v;
          if (v && v.Map && v.Map.addInitHook) v.Map.addInitHook(function () { window.__maps.push(this); });
        },
      });
    }, { theme: o.theme || 'light', clustering: !!o.clustering });
    const page = await ctx.newPage();
    page.on('pageerror', e => rec.errors.push(String(e)));
    page.on('request', r => rec.all.push(r.url()));
    return { ctx, page, rec };
  }

  async function coldMap(page, rec) {
    await page.goto(`${BASE}/#/map`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#leaflet-map[data-loaded="true"]', { timeout: 30000 });
    await page.evaluate(() => window.MeshConfigReady);
    await page.waitForSelector('.leaflet-marker-icon', { timeout: 15000 });
    await page.waitForSelector('[data-testid="map-reach-toggle"]', { timeout: 15000 });
    assert(rec.sockets.length > 0, 'the page opened no WebSocket');
    await page.waitForTimeout(800);
  }

  /* settledBox waits until sel holds still for two frames and a finger at
   * its middle lands on it, then answers its box and that point. */
  async function settledBox(page, sel, timeoutMs) {
    const t0 = Date.now();
    let prev = null;
    for (;;) {
      const b = await page.evaluate((s) => {
        const e = document.querySelector(s);
        if (!e) return null;
        const r = e.getBoundingClientRect();
        const x = r.left + r.width / 2, y = r.top + r.height / 2;
        const hit = document.elementFromPoint(x, y);
        return { x, y, w: r.width, h: r.height, onTarget: !!hit && (hit === e || e.contains(hit)) };
      }, sel);
      if (b && b.onTarget && prev && Math.abs(prev.x - b.x) < 0.5 && Math.abs(prev.y - b.y) < 0.5) return b;
      if (Date.now() - t0 > (timeoutMs || 8000)) throw new Error(`${sel} never settled under a finger: ${JSON.stringify(b)}`);
      prev = b;
      await page.waitForTimeout(120);
    }
  }

  async function tap(page, sel, touch) {
    const b = await settledBox(page, sel);
    if (touch) await page.touchscreen.tap(b.x, b.y); else await page.mouse.click(b.x, b.y);
  }

  /* pickMarker marks a visible repeater marker a finger can hit, clear of
   * the map's buttons and with room for its popup above it, and answers its
   * key and position. o.exclude: a key not to pick. o.anyHeight: anywhere a
   * finger hits it (with the phone's reach sheet open, only the map above
   * it is free). */
  async function pickMarker(page, o) {
    const r = await page.evaluate((o) => {
      const map = window.__maps[window.__maps.length - 1];
      const size = map.getSize();
      const box = map.getContainer().getBoundingClientRect();
      const found = [];
      map.eachLayer((l) => {
        if (!l._icon || !l._nodeKey || l._role !== 'repeater') return;
        const r = l._icon.getBoundingClientRect();
        const x = r.left + r.width / 2, y = r.top + r.height / 2;
        if (x < box.left + 60 || x > box.left + size.x - 60 || y > box.top + size.y - 40) return;
        if (y < box.top + (o.anyHeight ? 40 : size.y * 0.45)) return;
        if (o.exclude && String(l._nodeKey).toLowerCase() === o.exclude) return;
        const hit = document.elementFromPoint(x, y);
        if (!hit || !(hit === l._icon || l._icon.contains(hit))) return;
        found.push({ l, d: Math.abs(x - box.left - size.x / 2) });
      });
      found.sort((a, b) => a.d - b.d);
      if (!found.length) return null;
      document.querySelectorAll('[data-test-marker]').forEach(e => e.removeAttribute('data-test-marker'));
      found[0].l._icon.setAttribute('data-test-marker', '1');
      const ll = found[0].l.getLatLng();
      return { pk: String(found[0].l._nodeKey).toLowerCase(), lat: ll.lat, lon: ll.lng };
    }, o || {});
    assert(r, 'no repeater marker to tap');
    return r;
  }

  /* openPopup taps the picked marker and arms the watchers: the popup
   * element is tagged, and every popupclose and every removal of
   * #map-wrap.has-map-popup is counted from here on. */
  async function openPopup(page, touch) {
    await tap(page, '[data-test-marker]', touch);
    await page.waitForSelector(POPUP, { timeout: 5000 });
    await page.waitForSelector(REACH, { timeout: 5000 });
    await page.waitForFunction(() => {
      const m = window.__maps[window.__maps.length - 1];
      return !(m._panAnim && m._panAnim._inProgress);
    }, null, { timeout: 5000 });
    await page.waitForTimeout(400);
    await page.evaluate(() => {
      const map = window.__maps[window.__maps.length - 1];
      document.querySelector('.leaflet-popup').setAttribute('data-test-popup', '979');
      window.__closes = 0;
      if (!window.__closeHooked) { map.on('popupclose', () => { window.__closes++; }); window.__closeHooked = true; }
      window.__sheetDrops = 0;
      if (window.__sheetObs) window.__sheetObs.disconnect();
      const wrap = document.getElementById('map-wrap');
      window.__sheetObs = new MutationObserver(() => { if (!wrap.classList.contains('has-map-popup')) window.__sheetDrops++; });
      window.__sheetObs.observe(wrap, { attributes: true, attributeFilter: ['class'] });
    });
  }

  /* advertRefresh sends one ADVERT batch and waits until loadNodes() has
   * rebuilt the markers: a marker of another node, held from before, is no
   * longer on the map. o.fetch: drop the client's /nodes cache first (as
   * app.js does 5 s after any message), so /api/nodes is asked again. */
  async function advertRefresh(page, rec, o) {
    o = o || {};
    await page.evaluate((pk) => {
      const map = window.__maps[window.__maps.length - 1];
      window.__other = null;
      map.eachLayer((l) => { if (!window.__other && l._nodeKey && String(l._nodeKey).toLowerCase() !== pk) window.__other = l; });
    }, o.pk || '');
    const held = await page.evaluate(() => !!window.__other);
    assert(held, 'no other marker on the map to watch the rebuild with');
    if (o.fetch) await page.evaluate(() => invalidateApiCache('/nodes'));
    const n0 = rec.nodes;
    rec.sockets[rec.sockets.length - 1].send(ADVERT);
    await page.waitForFunction(() => !window.__maps[window.__maps.length - 1].hasLayer(window.__other), null, { timeout: 10000 });
    if (o.fetch) assert(rec.nodes > n0, 'the refresh did not ask /api/nodes again');
    await page.waitForTimeout(300);
  }

  /* popupState: whether the tagged popup is still the open one, with its
   * actions, and what the watchers counted. */
  function popupState(page) {
    return page.evaluate(() => {
      const map = window.__maps[window.__maps.length - 1];
      const el = document.querySelector('.leaflet-popup');
      const src = map._popup && map._popup.isOpen() && map._popup._source;
      return {
        open: !!el, same: !!el && el.getAttribute('data-test-popup') === '979',
        actions: el ? el.querySelectorAll('.map-popup-tools [data-testid]').length : 0,
        name: el && el.querySelector('h3') ? el.querySelector('h3').textContent : null,
        key: src ? String(src._nodeKey).toLowerCase() : null, onMap: !!src && map.hasLayer(src),
        closes: window.__closes, sheetDrops: window.__sheetDrops,
        wrapFlag: document.getElementById('map-wrap').classList.contains('has-map-popup'),
      };
    });
  }

  const sheet = (page) => page.evaluate(() => {
    const p = document.querySelector('[data-testid="reach-panel"]');
    return { open: !!p && !p.hidden, shown: !!p && getComputedStyle(p).display !== 'none' };
  });

  /* shot saves a JPEG at most 960 px wide (quality 60): a wider page is
   * scaled down on a canvas in a blank page. */
  async function shot(page, name) {
    if (!SHOTS) return;
    const vw = page.viewportSize().width;
    if (vw <= 960) return page.screenshot({ path: path.join(SHOTS, name), type: 'jpeg', quality: 60 });
    const png = (await page.screenshot({ type: 'png' })).toString('base64');
    const scratch = await page.context().newPage();
    try {
      const b64 = await scratch.evaluate(async (src) => {
        const img = new Image();
        img.src = 'data:image/png;base64,' + src;
        await img.decode();
        const c = document.createElement('canvas');
        c.width = 960; c.height = Math.round(img.height * 960 / img.width);
        c.getContext('2d').drawImage(img, 0, 0, c.width, c.height);
        return c.toDataURL('image/jpeg', 0.6).split(',')[1];
      }, png);
      require('fs').writeFileSync(path.join(SHOTS, name), Buffer.from(b64, 'base64'));
    } finally { await scratch.close(); }
  }

  function clean(rec) {
    const out = rec.all.filter(u => !/^(https?:\/\/(localhost|127\.0\.0\.1)[:/]|data:|blob:)/i.test(u));
    const real = out.filter(u => /dataforsyningen|amazonaws|copernicus/i.test(u));
    assert(real.length === 0, 'a request reached a real upstream: ' + real.join(', '));
    assert(rec.errors.length === 0, 'page errors: ' + rec.errors.join(' | '));
  }

  function assertKept(s, node, what) {
    assert(s.open && s.same, what + ': the popup closed or was rebuilt: ' + JSON.stringify(s));
    assert(s.key === node.pk && s.onMap, what + ': the popup lost its marker: ' + JSON.stringify(s));
    assert(s.actions === 2, what + ': the popup lost its actions: ' + JSON.stringify(s));
    assert(s.closes === 0 && s.sheetDrops === 0 && s.wrapFlag, what + ': popupclose or a sheet flicker: ' + JSON.stringify(s));
  }

  // ── 1: the popup stays open through ADVERT refreshes ─────────────────────
  for (const o of [
    { label: 'desktop 1440×900 light', viewport: { width: 1440, height: 900 }, theme: 'light', shot: 'desktop-light' },
    { label: 'desktop 1440×900 dark, clustering on', viewport: { width: 1440, height: 900 }, theme: 'dark', clustering: true, shot: 'desktop-dark' },
    { label: 'phone 375×560 light', viewport: { width: 375, height: 560 }, touch: true, theme: 'light', shot: 'phone-light' },
    { label: 'phone 375×560 dark', viewport: { width: 375, height: 560 }, touch: true, theme: 'dark', shot: 'phone-dark' },
  ]) {
    const { ctx, page, rec } = await newCtx(o);
    try {
      await coldMap(page, rec);
      let node = null;

      await step(`${o.label}: an open popup and its actions stay through two ADVERT refreshes`, async () => {
        node = await pickMarker(page);
        rec.center = node;
        await openPopup(page, o.touch);
        await advertRefresh(page, rec, { pk: node.pk });
        assertKept(await popupState(page), node, 'after a cached refresh');
        await advertRefresh(page, rec, { pk: node.pk, fetch: true });
        assertKept(await popupState(page), node, 'after a refetch');
        await shot(page, `979-popup-after-adverts-${o.shot}.jpg`);
      });

      await step(`${o.label}: its Predicted reach is under a finger and opens the reach for that node`, async () => {
        const b = await settledBox(page, REACH);
        if (o.touch) assert(b.w >= 44 && b.h >= 44, 'chip target ' + b.w + 'x' + b.h);
        const c0 = rec.coverage.length;
        await tap(page, REACH, o.touch);
        await page.waitForSelector('[data-testid="reach-panel"]:not([hidden])', { timeout: 10000 });
        await page.waitForFunction(() => !document.querySelector('.leaflet-popup'), null, { timeout: 5000 });
        assert(rec.coverage.slice(c0).indexOf(node.pk) >= 0, 'no reach request for ' + node.pk + ': ' + rec.coverage.slice(c0));
        const s = await sheet(page);
        assert(s.open && s.shown, 'the reach is not shown: ' + JSON.stringify(s));
        await page.waitForTimeout(500);
        await shot(page, `979-reach-${o.shot}.jpg`);
      });

      await step(`${o.label}: with the reach open, the next popup keeps the sheet aside through two refreshes, and a map tap brings it back`, async () => {
        node = await pickMarker(page, { exclude: node.pk, anyHeight: true });
        await openPopup(page, o.touch);
        const s0 = await sheet(page);
        assert(s0.open && s0.shown === !o.touch, 'the sheet with a popup open: ' + JSON.stringify(s0));
        await advertRefresh(page, rec, { pk: node.pk });
        await advertRefresh(page, rec, { pk: node.pk, fetch: true });
        assertKept(await popupState(page), node, 'over the reach');
        const s1 = await sheet(page);
        assert(s1.open && s1.shown === !o.touch, 'the sheet came back during a refresh: ' + JSON.stringify(s1));
        await shot(page, `979-popup-over-reach-${o.shot}.jpg`);
        // A tap on open map, clear of the popup, the sheet and the controls.
        const pt = await page.evaluate(() => {
          const m = document.getElementById('leaflet-map').getBoundingClientRect();
          const p = document.querySelector('.leaflet-popup').getBoundingClientRect();
          const cands = [];
          for (let fy = 0.12; fy < 0.95; fy += 0.02) for (let fx = 0.12; fx < 0.89; fx += 0.03) {
            const x = m.left + m.width * fx, y = m.top + m.height * fy;
            if (x > p.left - 16 && x < p.right + 16 && y > p.top - 16 && y < p.bottom + 32) continue;
            // No marker or control under the finger or 16 px around it (a
            // tile, the bare map, or a line or the reach in the overlay pane).
            let free = true;
            for (const [dx, dy] of [[0, 0], [-16, 0], [16, 0], [0, -16], [0, 16], [-11, -11], [11, -11], [-11, 11], [11, 11]]) {
              const hit = document.elementFromPoint(x + dx, y + dy);
              if (!hit || !(hit.classList.contains('leaflet-container') || hit.closest('.leaflet-tile-pane') ||
                hit.closest('.leaflet-overlay-pane'))) { free = false; break; }
            }
            if (free) cands.push({ x, y });
          }
          return cands[0] || null;
        });
        assert(pt, 'no free map point to tap');
        if (o.touch) await page.touchscreen.tap(pt.x, pt.y); else await page.mouse.click(pt.x, pt.y);
        await page.waitForFunction(() => !document.querySelector('.leaflet-popup'), null, { timeout: 5000 });
        const s2 = await sheet(page);
        assert(s2.open && s2.shown, 'the reach sheet did not come back: ' + JSON.stringify(s2));
        await page.waitForTimeout(300);
        await shot(page, `979-sheet-back-${o.shot}.jpg`);
        clean(rec);
      });
    } finally { await ctx.close(); }
  }

  // ── 1b: clustering on, a rebuild that markercluster adds in chunks ───────
  /* watchClusterAdds wraps the cluster group's addLayers: each add is noted
   * with its marker count, how many it held when the call returned (fewer
   * than all: chunked) and when its last chunk ran. chunkInterval, when
   * given, is lowered so the add is chunked on a fast machine too.
   * o.midChunk: once, a role filter is turned off and on again from a
   * timeout between the first two chunks (two renderMarkers calls). */
  async function watchClusterAdds(page, o) {
    await page.evaluate((o) => {
      const map = window.__maps[window.__maps.length - 1];
      let g = null;
      map.eachLayer((l) => { if (l instanceof L.MarkerClusterGroup) g = l; });
      if (!g) throw new Error('no cluster group on the map');
      window.__group = g;
      if (o.chunkInterval) g.options.chunkInterval = o.chunkInterval;
      window.__adds = [];
      window.__midChunk = !!o.midChunk;
      if (g.__watched) return;
      g.__watched = true;
      const addLayers = g.addLayers;
      g.addLayers = function (arr, skip) {
        const a = { total: arr.length, first: -1, chunks: 0, done: false };
        window.__adds.push(a);
        const progress = this.options.chunkProgress;
        this.options.chunkProgress = function (p, t) {
          a.chunks++;
          if (p === t) a.done = true;
          if (progress) return progress.apply(this, arguments);
        };
        const r = addLayers.call(this, arr, skip);
        a.first = this.getLayers().length;
        if (!a.done && window.__midChunk) {
          window.__midChunk = false;
          setTimeout(() => { const cb = document.getElementById('mcRole_companion'); cb.click(); cb.click(); }, 0);
        }
        return r;
      };
    }, o || {});
  }

  /* clusterState: the adds noted, every node's marker count in the group,
   * and the open popup's marker and its remove handlers. */
  function clusterState(page) {
    return page.evaluate(() => {
      const map = window.__maps[window.__maps.length - 1];
      const keys = new Map();
      for (const l of window.__group.getLayers()) if (l._nodeKey) keys.set(l._nodeKey, (keys.get(l._nodeKey) || 0) + 1);
      const src = map._popup && map._popup.isOpen() && map._popup._source;
      return {
        adds: window.__adds.map(a => ({ total: a.total, first: a.first, chunks: a.chunks, done: a.done })),
        markers: keys.size, doubled: Array.from(keys.values()).filter(n => n > 1).length,
        removeHandlers: src ? ((src._events && src._events.remove) || []).filter(l => l.fn === src.closePopup).length : null,
      };
    });
  }

  /* chunkedRefresh: advertRefresh's ADVERT batch, with the CPU slowed
   * rate×, until the rebuild's add (watchClusterAdds) and any add after it
   * have had their last chunk. o.fetch as advertRefresh. */
  async function chunkedRefresh(page, rec, cdp, rate, o) {
    await cdp.send('Emulation.setCPUThrottlingRate', { rate });
    try {
      const a0 = await page.evaluate(() => window.__adds.length);
      if (o.fetch) await page.evaluate(() => invalidateApiCache('/nodes'));
      const n0 = rec.nodes;
      rec.sockets[rec.sockets.length - 1].send(ADVERT);
      await page.waitForFunction((a0) => window.__adds.length > a0 && window.__adds.every(a => a.done), a0, { timeout: 30000 });
      if (o.fetch) assert(rec.nodes > n0, 'the refresh did not ask /api/nodes again');
      await page.waitForTimeout(300);
    } finally { await cdp.send('Emulation.setCPUThrottlingRate', { rate: 1 }); }
  }

  /* zoomToRepeater centres on a fixture repeater at zoom 15, where its
   * clones (clonedPage) are clear of it. */
  async function zoomToRepeater(page) {
    await page.evaluate(() => {
      const map = window.__maps[window.__maps.length - 1];
      let pick = null;
      map.eachLayer((l) => { if (!pick && l._nodeKey && l._role === 'repeater' && !/9790[0-9a-f]{4}$/.test(l._nodeKey)) pick = l; });
      if (!pick) window.__group.eachLayer((l) => { if (!pick && l._role === 'repeater' && !/9790[0-9a-f]{4}$/.test(l._nodeKey)) pick = l; });
      map.setView(pick.getLatLng(), 15, { animate: false });
    });
    await page.waitForTimeout(600);
  }

  for (const o of [
    { label: 'desktop 1440×900, clustering on, 2,000 nodes', viewport: { width: 1440, height: 900 }, theme: 'dark' },
    { label: 'phone 375×560, clustering on, 2,000 nodes', viewport: { width: 375, height: 560 }, touch: true, theme: 'light' },
  ]) {
    const { ctx, page, rec } = await newCtx(Object.assign({ clustering: true, clones: 2000 }, o));
    const cdp = await ctx.newCDPSession(page);
    try {
      await coldMap(page, rec);
      await watchClusterAdds(page, {});
      await zoomToRepeater(page);

      await step(`${o.label}: a popup stays open through refreshes markercluster adds in chunks (CPU 4×)`, async () => {
        const node = await pickMarker(page, { anyHeight: !!o.touch });
        await openPopup(page, o.touch);
        await watchClusterAdds(page, { chunkInterval: 20 });
        await chunkedRefresh(page, rec, cdp, 4, {});
        await chunkedRefresh(page, rec, cdp, 4, { fetch: true });
        const c = await clusterState(page);
        assert(c.adds.length === 2 && c.adds.every(a => a.total > 1900 && a.first < a.total && a.chunks > 1),
          'the refreshes were not added in chunks: ' + JSON.stringify(c.adds));
        assertKept(await popupState(page), node, 'after chunked refreshes');
        assert(c.removeHandlers === 1, 'the kept marker has ' + c.removeHandlers + ' remove handlers');
        await shot(page, `979-popup-chunked-${o.touch ? 'phone' : 'desktop'}.jpg`);
      });

      await step(`${o.label}: a rebuild asked for during a chunked add waits for it: no marker twice, one remove handler`, async () => {
        const node = await popupState(page);
        await watchClusterAdds(page, { chunkInterval: 20, midChunk: true });
        await chunkedRefresh(page, rec, cdp, 4, { fetch: true });
        // The filter's two rebuilds ran as one, after the add.
        await page.waitForFunction(() => window.__adds.length >= 2 && window.__adds.every(a => a.done), null, { timeout: 30000 });
        await page.waitForTimeout(300);
        const c = await clusterState(page);
        assert(c.adds.length === 2, 'the rebuilds during the add were not run as one after it: ' + JSON.stringify(c.adds));
        assert(c.doubled === 0 && c.markers === c.adds[1].total, 'markers twice or missing: ' + JSON.stringify(c));
        assertKept(await popupState(page), { pk: node.key }, 'after a rebuild during a chunked add');
        assert(c.removeHandlers === 1, 'the kept marker has ' + c.removeHandlers + ' remove handlers');
        clean(rec);
      });
    } finally { await ctx.close(); }
  }

  // ── 2: a popup whose node is filtered out or gone closes ─────────────────
  await step('desktop: a popup whose node is filtered out closes, and the flag goes', async () => {
    const { ctx, page, rec } = await newCtx({ viewport: { width: 1440, height: 900 } });
    try {
      await coldMap(page, rec);
      await pickMarker(page);
      await openPopup(page, false);
      await page.evaluate(() => document.getElementById('mcRole_repeater').click());
      await page.waitForFunction(() => !document.querySelector('.leaflet-popup'), null, { timeout: 5000 });
      const s = await popupState(page);
      assert(s.closes === 1 && !s.wrapFlag, 'filtered out: ' + JSON.stringify(s));
      // Its role back on, it is not opened again by itself.
      await page.evaluate(() => document.getElementById('mcRole_repeater').click());
      await page.waitForTimeout(300);
      assert(!(await popupState(page)).open, 'the popup came back by itself');
      clean(rec);
    } finally { await ctx.close(); }
  });

  await step('desktop: a popup whose node is gone from the next /api/nodes closes', async () => {
    let gone = null;
    const o = { viewport: { width: 1440, height: 900 }, nodes: list => list.filter(n => String(n.public_key).toLowerCase() !== gone) };
    const { ctx, page, rec } = await newCtx(o);
    try {
      await coldMap(page, rec);
      const node = await pickMarker(page);
      await openPopup(page, false);
      gone = node.pk;
      await advertRefresh(page, rec, { pk: node.pk, fetch: true });
      const s = await popupState(page);
      assert(!s.open && s.closes === 1 && !s.wrapFlag, 'gone: ' + JSON.stringify(s));
      const left = await page.evaluate((pk) => {
        let n = 0;
        window.__maps[window.__maps.length - 1].eachLayer((l) => { if (l._nodeKey && String(l._nodeKey).toLowerCase() === pk) n++; });
        return n;
      }, node.pk);
      assert(left === 0, 'the gone node still has a marker: ' + left);
      clean(rec);
    } finally { await ctx.close(); }
  });

  await step('desktop, clustering on: after a refresh, zooming out until its marker joins a cluster still closes the popup', async () => {
    // Leaflet's own close on marker removal is back on the kept marker.
    const { ctx, page, rec } = await newCtx({ viewport: { width: 1440, height: 900 }, clustering: true });
    try {
      await coldMap(page, rec);
      const node = await pickMarker(page);
      await openPopup(page, false);
      await advertRefresh(page, rec, { pk: node.pk, fetch: true });
      assertKept(await popupState(page), node, 'before the zoom');
      const r = await page.evaluate(async () => {
        const map = window.__maps[window.__maps.length - 1];
        const mk = map._popup._source;
        for (let z = map.getZoom() - 1; z >= 2 && map.hasLayer(mk); z--) {
          map.setZoom(z, { animate: false });
          await new Promise(res => setTimeout(res, 200));
        }
        return { clustered: !map.hasLayer(mk), open: map._popup.isOpen(), closes: window.__closes };
      });
      assert(r.clustered && !r.open && r.closes === 1, 'zoomed out: ' + JSON.stringify(r));
      clean(rec);
    } finally { await ctx.close(); }
  });

  // ── 3: the refreshed data shows on the next open ─────────────────────────
  /* keptMarker: where the marker of node pk is and how it looks, and
   * whether it is the open popup's. */
  const keptMarker = (page, pk) => page.evaluate((pk) => {
    const map = window.__maps[window.__maps.length - 1];
    let mk = null;
    map.eachLayer((l) => { if (!mk && l._icon && l._nodeKey && String(l._nodeKey).toLowerCase() === pk) mk = l; });
    if (!mk) return null;
    const ll = mk.getLatLng();
    return { lat: ll.lat, lon: ll.lng, stale: mk._icon.classList.contains('marker-stale'),
      iconStale: / marker-stale/.test(mk.options.icon.options.className || ''),
      open: !!(map._popup && map._popup.isOpen() && map._popup._source === mk) };
  }, pk);

  await step('desktop: a refresh that renames, moves and ages the open popup\'s node leaves the popup as it is, moves and restyles its marker, and the next open shows the new data', async () => {
    let changed = null;
    const STALE = new Date(Date.now() - 40 * 86400000).toISOString();
    const o = { viewport: { width: 1440, height: 900 },
      nodes: list => list.map(n => (String(n.public_key).toLowerCase() === changed
        ? Object.assign({}, n, { name: 'Renamed 979', lat: n.lat + 0.004, lon: n.lon - 0.006, last_seen: STALE, last_heard: STALE }) : n)) };
    const { ctx, page, rec } = await newCtx(o);
    try {
      await coldMap(page, rec);
      const node = await pickMarker(page);
      await openPopup(page, false);
      const s0 = await popupState(page);
      const m0 = await keptMarker(page, node.pk);
      assert(m0 && m0.open && !m0.stale, 'before: ' + JSON.stringify(m0));
      changed = node.pk;
      await advertRefresh(page, rec, { pk: node.pk, fetch: true });
      const s1 = await popupState(page);
      assertKept(s1, node, 'changed');
      assert(s1.name === s0.name, 'the open popup was re-rendered: ' + s0.name + ' → ' + s1.name);
      const m1 = await keptMarker(page, node.pk);
      // The kept marker took the node's new place and its stale look.
      assert(m1 && m1.open, 'the open popup\'s marker: ' + JSON.stringify(m1));
      assert(Math.abs(m1.lat - m0.lat) > 0.002 || Math.abs(m1.lon - m0.lon) > 0.002, 'the kept marker did not move: ' + JSON.stringify([m0, m1]));
      assert(m1.stale && m1.iconStale, 'the kept marker kept its old icon: ' + JSON.stringify(m1));
      await page.evaluate(() => window.__maps[window.__maps.length - 1].closePopup());
      await page.waitForTimeout(100);
      // A rebuild with no popup open makes a new marker: the kept one was
      // where and as that one is.
      await advertRefresh(page, rec, { pk: node.pk });
      const m2 = await keptMarker(page, node.pk);
      assert(m2 && Math.abs(m2.lat - m1.lat) < 1e-9 && Math.abs(m2.lon - m1.lon) < 1e-9 && m2.stale,
        'the kept marker was not where a new one is: ' + JSON.stringify([m1, m2]));
      await page.evaluate((pk) => window.__maps[window.__maps.length - 1].eachLayer((l) => {
        if (l._icon && l._nodeKey && String(l._nodeKey).toLowerCase() === pk) l._icon.setAttribute('data-test-marker', '1');
      }), node.pk);
      await tap(page, '[data-test-marker]', false);
      await page.waitForSelector(POPUP, { timeout: 5000 });
      const s2 = await popupState(page);
      assert(s2.name === 'Renamed 979', 'the next open shows old data: ' + s2.name);
      assert(s2.actions === 2 || (await page.waitForSelector(REACH, { timeout: 5000 })), 'no actions on reopen');
      clean(rec);
    } finally { await ctx.close(); }
  });

  // ── 3b: Show Neighbors from a popup closes it ────────────────────────────
  for (const o of [
    { label: 'desktop 1440×900', viewport: { width: 1440, height: 900 } },
    { label: 'phone 375×560', viewport: { width: 375, height: 560 }, touch: true },
  ]) {
    await step(`${o.label}: Show Neighbors in a popup closes it and shows that node's neighbours`, async () => {
      const { ctx, page, rec } = await newCtx(o);
      try {
        await coldMap(page, rec);
        await pickMarker(page);
        await openPopup(page, o.touch);
        await tap(page, '.leaflet-popup [data-show-neighbors]', o.touch);
        await page.waitForFunction(() => !document.querySelector('.leaflet-popup'), null, { timeout: 5000 });
        const s = await popupState(page);
        assert(s.closes === 1 && !s.wrapFlag, 'Show Neighbors: ' + JSON.stringify(s));
        const r = await page.evaluate(() => ({ on: document.getElementById('mcNeighbors').checked }));
        assert(r.on, 'the neighbours filter is not on');
        await page.waitForTimeout(500);
        assert(!(await popupState(page)).open, 'the popup came back');
        await shot(page, `979-show-neighbors-${o.touch ? 'phone' : 'desktop'}.jpg`);
        clean(rec);
      } finally { await ctx.close(); }
    });
  }

  // ── 4: a #/map?node= link opens its node's popup once, not per reload ──
  await step('desktop: a #/map?node= link centres and opens once; a reload leaves the view and the user\'s popup alone', async () => {
    const { ctx, page, rec } = await newCtx({ viewport: { width: 1440, height: 900 } });
    try {
      await page.goto(`${BASE}/`, { waitUntil: 'domcontentloaded' });
      const target = await page.evaluate(async () => {
        const d = await (await fetch('/api/nodes?limit=2000')).json();
        const n = (d.nodes || []).find(n => n.role === 'repeater' && n.lat && n.lon);
        return n && String(n.public_key).toLowerCase();
      });
      assert(target, 'no repeater with a position');
      await page.goto(`${BASE}/#/map?node=${target}`, { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('#leaflet-map[data-loaded="true"]', { timeout: 30000 });
      await page.waitForSelector(POPUP, { timeout: 10000 });
      assert((await popupState(page)).key === target, 'the link did not open its node');
      await page.evaluate(() => window.__maps[window.__maps.length - 1].closePopup());
      await page.evaluate(() => window.__maps[window.__maps.length - 1].setZoom(12, { animate: false }));
      await page.waitForTimeout(400);
      const node = await pickMarker(page, { exclude: target, anyHeight: true });
      await openPopup(page, false);
      const v0 = await page.evaluate(() => { const m = window.__maps[window.__maps.length - 1]; const c = m.getCenter(); return [c.lat, c.lng, m.getZoom()]; });
      await advertRefresh(page, rec, { pk: node.pk, fetch: true });
      await page.waitForTimeout(800); // the link's popup opens 500 ms after a load
      assertKept(await popupState(page), node, 'after a reload with ?node=');
      const v1 = await page.evaluate(() => { const m = window.__maps[window.__maps.length - 1]; const c = m.getCenter(); return [c.lat, c.lng, m.getZoom()]; });
      assert(JSON.stringify(v0) === JSON.stringify(v1), 'the reload moved the view: ' + JSON.stringify(v0) + ' → ' + JSON.stringify(v1));
      clean(rec);
    } finally { await ctx.close(); }
  });

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
