/**
 * E2E (#982): Map popup follow-ups from #981's reviews, in a real browser.
 *
 * #979 keeps an open node popup through background marker rebuilds. With
 * clustering on, markercluster adds a rebuild's markers in chunks; a
 * rebuild asked for meanwhile waits for the add to end. This suite pins
 * what #981's reviews left open:
 *
 *   1. a throw inside a deferred chunk of that add (forced by patching the
 *      cluster group in the page; there is no hook in the app) no longer
 *      stalls the map: the rebuild queued behind the dead add runs, the
 *      kept marker has its remove handler again, and later refreshes still
 *      rebuild; with no rebuild queued, the popup is settled (it closes, its
 *      marker is not on the map) and the next refresh rebuilds;
 *   2. two adds in flight at once (clustering turned off and on between
 *      two chunks): only the current add settles the popup, so it stays
 *      open with one remove handler (endClusterAdd's stale-add guard);
 *   3. a popup on a spiderfied cluster child (co-located nodes, several
 *      repeaters on one mast) survives two refreshes: the spider is restored
 *      around it and the popup stays on its marker, at 1440×900 (click) and
 *      375×560 (tap); a kept popup whose marker a refresh puts in a new
 *      cluster still closes;
 *   4. closing a popup on a dense phone map, at 360×640 and 375×560 touch,
 *      light and dark, with real taps: the map buttons step aside while a
 *      popup is open, so neither the × nor the popup is under one, and they
 *      come back when it closes; a finger tap on empty map beside a marker,
 *      which Chromium's touch adjustment moves onto that marker, closes the
 *      open popup instead of opening the neighbour's, while a tap on a
 *      marker still opens it, with or without a popup open; View Node and
 *      Show Neighbors are at least 44 px tall (compact at 1440×900);
 *   5. no page error but the injected one, and no request leaves localhost.
 *
 * SHOTS_DIR, when set, receives JPEG screenshots (≤ 960 px, quality 60).
 *
 * Usage:
 *   BASE_URL=http://localhost:13581 node test-issue-982-map-popup-followups-e2e.js
 */
'use strict';
const path = require('path');
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
if (!['localhost', '127.0.0.1', '[::1]'].includes(new URL(BASE).hostname)) throw new Error('local server only');
const SHOTS = process.env.SHOTS_DIR || '';
const INJECTED = '__982 injected chunk failure';

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
const ACTION = '.leaflet-popup .map-popup-tools [data-testid]';
const ADVERT = JSON.stringify({ type: 'packet', data: {
  id: 982001, hash: '982a000000000001', observer_id: 'obs982', observer_name: 'E2E-982', snr: 5, rssi: -90,
  timestamp: new Date().toISOString(), decoded: { header: { payloadTypeName: 'ADVERT' }, payload: {}, path: { hops: [] } },
} });
// The buttons over the map: zoom, layers, and the settings, ruler and reach
// toggles.
const MAP_BUTTONS = '#map-wrap .leaflet-top .leaflet-control, #map-wrap .map-controls-toggle, ' +
  '#map-wrap .map-profile-toggle, #map-wrap .map-reach-toggle';

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
  console.log(`\n=== #982 Map popup follow-ups against ${BASE} ===\n`);

  /* newCtx: every /api/nodes page is cut from one list, the server's
   * nodes plus o.clones - n clones of them (0.02°–0.08° from their node, so
   * a node keeps its own marker at zoom 15); o.nodes(list) may rewrite that
   * list per answer. */
  async function newCtx(o) {
    const ctx = await browser.newContext({
      viewport: o.viewport, hasTouch: !!o.touch, isMobile: !!o.touch,
      colorScheme: o.theme === 'dark' ? 'dark' : 'light',
    });
    const rec = { errors: [], all: [], nodes: 0, sockets: [], list: null };
    await ctx.routeWebSocket(/.*/, (ws) => { rec.sockets.push(ws); });
    await ctx.route(BASE + '/api/config/client', async (route) => {
      const response = await route.fetch();
      const config = await response.json();
      config.dhm = DHM_BLOCK; config.coverage = COVERAGE_BLOCK;
      await route.fulfill({ response, json: config });
    });
    await ctx.route(url => new URL(url).pathname === '/api/nodes', async (route) => {
      rec.nodes++;
      const u = new URL(route.request().url());
      const limit = Number(u.searchParams.get('limit')) || 500;
      const offset = Number(u.searchParams.get('offset')) || 0;
      if (!rec.list) {
        const all = new URL(u); all.searchParams.set('limit', '5000'); all.searchParams.set('offset', '0');
        const base = ((await (await route.fetch({ url: all.toString() })).json()).nodes) || [];
        const placed = base.filter(n => n.lat && n.lon);
        const list = base.slice();
        for (let i = 0; list.length < (o.clones || 0); i++) {
          const n = placed[i % placed.length];
          const a = i * 2.399963, d = 0.02 + 0.06 * ((i * 7919) % 1000) / 1000;
          list.push(Object.assign({}, n, {
            public_key: String(n.public_key).slice(0, 56) + (0x98200000 + i).toString(16),
            name: (n.name || 'Node') + ' ~' + i, lat: n.lat + d * Math.sin(a), lon: n.lon + d * Math.cos(a),
          }));
        }
        rec.list = list;
      }
      const list = o.nodes ? o.nodes(rec.list) : rec.list;
      return route.fulfill({ json: { nodes: list.slice(offset, offset + limit), total: list.length, counts: {} } });
    });
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
      // Where each touch really started, and what it was dispatched to.
      window.__downs = [];
      document.addEventListener('pointerdown', (e) => {
        const icon = e.target.closest && e.target.closest('.leaflet-marker-icon');
        window.__downs.push({ x: e.clientX, y: e.clientY, type: e.pointerType, onIcon: !!icon });
      }, true);
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
    assert(rec.sockets.length > 0, 'the page opened no WebSocket');
    await page.waitForTimeout(800);
  }

  /* settledBox waits until sel holds still for two reads and a finger at
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

  /* fingerTap is a touch with a finger's contact area (radius px), as a
   * phone reports it; Chromium's touch adjustment then moves a tap that
   * lands near a tappable element onto it. Two taps are spaced so that
   * Chromium does not read them as a double tap. */
  async function fingerTap(page, cdp, x, y, radius) {
    await page.waitForTimeout(600);
    await page.evaluate(() => { window.__downs = []; });
    await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [{ x, y, radiusX: radius, radiusY: radius, force: 1 }] });
    await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
  }

  async function tap(page, sel, touch) {
    const b = await settledBox(page, sel);
    if (touch) await page.touchscreen.tap(b.x, b.y); else await page.mouse.click(b.x, b.y);
  }

  /* pickMarker marks a visible repeater marker a finger can hit, clear of
   * the map's edges, and answers its key. o.keys: only these keys. */
  async function pickMarker(page, o) {
    const r = await page.evaluate((o) => {
      const map = window.__maps[window.__maps.length - 1];
      const size = map.getSize();
      const box = map.getContainer().getBoundingClientRect();
      const found = [];
      map.eachLayer((l) => {
        if (!l._icon || !l._nodeKey || l._role !== 'repeater') return;
        if (o.keys && o.keys.indexOf(String(l._nodeKey).toLowerCase()) < 0) return;
        const r = l._icon.getBoundingClientRect();
        const x = r.left + r.width / 2, y = r.top + r.height / 2;
        if (x < box.left + 70 || x > box.left + size.x - 70 || y < box.top + 60 || y > box.top + size.y - 40) return;
        const hit = document.elementFromPoint(x, y);
        if (!hit || !(hit === l._icon || l._icon.contains(hit))) return;
        found.push({ l, d: Math.abs(x - box.left - size.x / 2) + Math.abs(y - box.top - size.y * 0.6) });
      });
      found.sort((a, b) => a.d - b.d);
      if (!found.length) return null;
      document.querySelectorAll('[data-test-marker]').forEach(e => e.removeAttribute('data-test-marker'));
      found[0].l._icon.setAttribute('data-test-marker', '1');
      return { pk: String(found[0].l._nodeKey).toLowerCase() };
    }, o || {});
    assert(r, 'no repeater marker to tap');
    return r;
  }

  /* openPopup taps the picked marker, waits for the popup and its actions
   * to settle, tags the popup element and counts every popupclose. */
  async function openPopup(page, touch) {
    await tap(page, '[data-test-marker]', touch);
    await popupSettled(page);
    await page.evaluate(() => {
      const map = window.__maps[window.__maps.length - 1];
      document.querySelector('.leaflet-popup').setAttribute('data-test-popup', '982');
      window.__closes = 0;
      if (!window.__closeHooked) { map.on('popupclose', () => { window.__closes++; }); window.__closeHooked = true; }
    });
  }

  /* popupSettled waits for an opening popup's actions and for the pan
   * that brings it into view. */
  async function popupSettled(page) {
    await page.waitForSelector(POPUP, { timeout: 5000 });
    await page.waitForSelector(ACTION, { timeout: 5000 });
    await page.waitForFunction(() => {
      const m = window.__maps[window.__maps.length - 1];
      return !(m._panAnim && m._panAnim._inProgress);
    }, null, { timeout: 5000 });
    await page.waitForTimeout(400);
  }

  /* popupState: whether the tagged popup is still the open one, and on
   * its marker; the marker's remove → closePopup handlers. */
  function popupState(page) {
    return page.evaluate(() => {
      const map = window.__maps[window.__maps.length - 1];
      const el = document.querySelector('.leaflet-popup');
      const p = map._popup;
      const src = p && p.isOpen() && p._source;
      return {
        open: !!el, same: !!el && el.getAttribute('data-test-popup') === '982',
        key: src ? String(src._nodeKey).toLowerCase() : null, onMap: !!src && map.hasLayer(src),
        atMarker: !!src && p.getLatLng().equals(src.getLatLng()),
        spider: !!src && !!src._preSpiderfyLatlng && !!src._spiderLeg && map.hasLayer(src._spiderLeg),
        removeHandlers: src ? ((src._events && src._events.remove) || []).filter(l => l.fn === src.closePopup).length : null,
        closes: window.__closes,
      };
    });
  }

  function assertKept(s, pk, what) {
    assert(s.open && s.same, what + ': the popup closed or was rebuilt: ' + JSON.stringify(s));
    assert(s.key === pk && s.onMap && s.atMarker, what + ': the popup lost its marker: ' + JSON.stringify(s));
    assert(s.closes === 0, what + ': popupclose fired: ' + JSON.stringify(s));
    assert(s.removeHandlers === 1, what + ': the kept marker has ' + s.removeHandlers + ' remove handlers');
  }

  /* advertRefresh sends one ADVERT batch and waits until loadNodes() has
   * rebuilt the markers: a marker of another node, held from before, is no
   * longer on the map (with clustering on, until the add has ended too).
   * o.fetch: drop the client's /nodes cache first, so /api/nodes is asked
   * again. */
  async function advertRefresh(page, rec, o) {
    o = o || {};
    await page.evaluate((pk) => {
      const map = window.__maps[window.__maps.length - 1];
      window.__other = null;
      map.eachLayer((l) => { if (!window.__other && l._nodeKey && String(l._nodeKey).toLowerCase() !== pk) window.__other = l; });
    }, o.pk || '');
    assert(await page.evaluate(() => !!window.__other), 'no other marker on the map to watch the rebuild with');
    if (o.fetch) await page.evaluate(() => invalidateApiCache('/nodes'));
    const n0 = rec.nodes;
    rec.sockets[rec.sockets.length - 1].send(ADVERT);
    await page.waitForFunction(() => !window.__maps[window.__maps.length - 1].hasLayer(window.__other), null, { timeout: 10000 });
    if (o.fetch) assert(rec.nodes > n0, 'the refresh did not ask /api/nodes again');
    await page.waitForFunction(() => !window.__adds || window.__adds.every(a => a.done || a.threw), null, { timeout: 15000 });
    await page.waitForTimeout(300);
  }

  /* watchClusterAdds wraps the cluster group's addLayers: each add is noted
   * with its marker count, its chunks and whether its last chunk ran.
   * window.__onChunked, when set, runs once from a timeout after the first
   * chunk of an add that goes on in more chunks. window.__throwAt = n makes
   * the n-th marker of the next add throw (the test's stand-in for a bug in
   * marker or icon code); a.threw says whether that was in the first chunk
   * or a deferred one. */
  async function watchClusterAdds(page, o) {
    await page.evaluate((o) => {
      const map = window.__maps[window.__maps.length - 1];
      let g = null;
      map.eachLayer((l) => { if (l instanceof L.MarkerClusterGroup) g = l; });
      if (!g) throw new Error('no cluster group on the map');
      window.__group = g;
      if (o.chunkInterval != null) g.options.chunkInterval = o.chunkInterval;
      window.__adds = [];
      if (g.__watched) return;
      g.__watched = true;
      const addLayers = g.addLayers;
      g.addLayers = function (arr, skip) {
        const a = { total: arr.length, chunks: 0, done: false, returned: false, threw: null, n: 0 };
        window.__adds.push(a);
        window.__curAdd = a;
        const progress = this.options.chunkProgress;
        this.options.chunkProgress = function (p, t) {
          a.chunks++;
          if (p === t) a.done = true;
          if (progress) return progress.apply(this, arguments);
        };
        const r = addLayers.call(this, arr, skip);
        a.returned = true;
        if (!a.done && window.__onChunked) { const f = window.__onChunked; window.__onChunked = null; setTimeout(f, 0); }
        return r;
      };
      const addOne = g._addLayer;
      g._addLayer = function () {
        const a = window.__curAdd;
        if (window.__throwAt && a && ++a.n === window.__throwAt) {
          window.__throwAt = 0;
          a.threw = a.returned ? 'deferred' : 'first';
          throw new Error('__982 injected chunk failure');
        }
        return addOne.apply(this, arguments);
      };
    }, o || {});
  }

  /* clusterState: the adds noted, each node's marker count in the group
   * and how many of them are companions. */
  function clusterState(page) {
    return page.evaluate(() => {
      const keys = new Map();
      let companions = 0;
      for (const l of window.__group.getLayers()) {
        if (!l._nodeKey) continue;
        keys.set(l._nodeKey, (keys.get(l._nodeKey) || 0) + 1);
        if (l._role === 'companion') companions++;
      }
      return {
        adds: window.__adds.map(a => ({ total: a.total, chunks: a.chunks, done: a.done, threw: a.threw })),
        markers: keys.size, doubled: Array.from(keys.values()).filter(n => n > 1).length, companions,
      };
    });
  }

  /* zoomToRepeater centres on a fixture repeater at zoom 15, where its
   * clones are clear of it. */
  async function zoomToRepeater(page) {
    await page.evaluate(() => {
      const map = window.__maps[window.__maps.length - 1];
      let pick = null;
      window.__group.eachLayer((l) => { if (!pick && l._role === 'repeater' && !/9820[0-9a-f]{4}$/.test(l._nodeKey)) pick = l; });
      map.setView(pick.getLatLng(), 15, { animate: false });
    });
    await page.waitForTimeout(600);
  }

  /* chunkedRefresh: an ADVERT batch with the CPU slowed 4×, so the
   * rebuild's add goes on in chunks. It answers once the adds after it
   * have ended, or once o.until holds. */
  async function chunkedRefresh(page, rec, cdp, o) {
    await cdp.send('Emulation.setCPUThrottlingRate', { rate: 4 });
    try {
      const a0 = await page.evaluate(() => window.__adds.length);
      if (o.fetch) await page.evaluate(() => invalidateApiCache('/nodes'));
      rec.sockets[rec.sockets.length - 1].send(ADVERT);
      await page.waitForFunction((a0) => window.__adds.length > a0, a0, { timeout: 15000 });
      return await page.waitForFunction(o.until || ((a0) => window.__adds.slice(a0).every(a => a.done)), a0, { timeout: o.timeout || 20000 })
        .then(() => true, () => false);
    } finally { await cdp.send('Emulation.setCPUThrottlingRate', { rate: 1 }); }
  }

  /* shot saves a JPEG at most 960 px wide (quality 60). */
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

  function clean(rec, allowed) {
    const out = rec.all.filter(u => !/^(https?:\/\/(localhost|127\.0\.0\.1)[:/]|data:|blob:)/i.test(u));
    const real = out.filter(u => /dataforsyningen|amazonaws|copernicus/i.test(u));
    assert(real.length === 0, 'a request reached a real upstream: ' + real.join(', '));
    const errors = rec.errors.filter(e => !(allowed && e.indexOf(INJECTED) >= 0));
    assert(errors.length === 0, 'page errors: ' + errors.join(' | '));
  }

  // ── 1: a throw in a deferred chunk does not stall the rebuilds ───────────
  for (const o of [
    { label: 'a rebuild queued behind it runs, and the popup stays', queued: true },
    { label: 'with no rebuild queued, the popup is settled and the next refresh rebuilds', queued: false },
  ]) {
    await step(`desktop 1440×900, clustering on, 1,000 nodes: a throw in a deferred cluster chunk: ${o.label}`, async () => {
      const { ctx, page, rec } = await newCtx({ viewport: { width: 1440, height: 900 }, clustering: true, clones: 1000 });
      const cdp = await ctx.newCDPSession(page);
      try {
        await coldMap(page, rec);
        await watchClusterAdds(page, {});
        await zoomToRepeater(page);
        const node = await pickMarker(page);
        await openPopup(page, false);
        await page.evaluate(() => { window.__keep = window.__maps[window.__maps.length - 1]._popup._source; });
        await watchClusterAdds(page, { chunkInterval: 0 });
        await page.evaluate((queued) => {
          window.__throwAt = 600;
          // A filter change during the add: renderMarkers is asked again.
          if (queued) window.__onChunked = () => document.getElementById('mcRole_companion').click();
        }, o.queued);
        const ran = await chunkedRefresh(page, rec, cdp, {
          until: (a0) => window.__adds.length > a0 + 1 && window.__adds[window.__adds.length - 1].done,
          timeout: o.queued ? 8000 : 1500,
        });
        const c = await clusterState(page);
        assert(c.adds[0].threw === 'deferred' && !c.adds[0].done && c.adds[0].chunks > 1,
          'the throw was not in a deferred chunk: ' + JSON.stringify(c.adds));
        if (o.queued) {
          assert(ran, 'the rebuild queued behind the dead add never ran: ' + JSON.stringify(c.adds));
          assert(c.adds.length === 2 && c.adds[1].done, 'adds after the dead one: ' + JSON.stringify(c.adds));
          assert(c.companions === 0 && c.doubled === 0 && c.markers === c.adds[1].total,
            'the queued filter change is not on the map: ' + JSON.stringify(c));
          assertKept(await popupState(page), node.pk, 'after the queued rebuild');
        } else {
          // Nothing was asked meanwhile: the add is let go, the popup is
          // settled (its marker is not on the map, so it closes) and
          // Leaflet's close is back on its marker.
          await page.waitForFunction(() => !document.querySelector('.leaflet-popup'), null, { timeout: 3000 })
            .catch(() => { throw new Error('the dead add was never let go: ' + JSON.stringify(c.adds)); });
          const k = await page.evaluate(() => ((window.__keep._events && window.__keep._events.remove) || [])
            .filter(l => l.fn === window.__keep.closePopup).length);
          assert(k === 1, 'the kept marker has ' + k + ' remove handlers');
        }
        // Later refreshes still rebuild.
        const ok = await chunkedRefresh(page, rec, cdp, { fetch: true });
        const c2 = await clusterState(page);
        assert(ok && c2.adds.length >= (o.queued ? 3 : 2) && c2.adds.slice(-1)[0].done,
          'a later refresh did not rebuild: ' + JSON.stringify(c2.adds));
        assert(c2.doubled === 0 && c2.markers === c2.adds.slice(-1)[0].total, 'markers twice or missing: ' + JSON.stringify(c2));
        if (o.queued) assertKept(await popupState(page), node.pk, 'after a later refresh');
        assert(rec.errors.filter(e => e.indexOf(INJECTED) >= 0).length === 1, 'the injected throw: ' + rec.errors.join(' | '));
        clean(rec, true);
      } finally { await ctx.close(); }
    });
  }

  // ── 2: only the current add settles the popup ────────────────────────────
  await step('desktop 1440×900, clustering on, 1,000 nodes: clustering off and on between two chunks — the dropped add ends after the new one started, and the popup stays with one remove handler', async () => {
    const { ctx, page, rec } = await newCtx({ viewport: { width: 1440, height: 900 }, clustering: true, clones: 1000 });
    const cdp = await ctx.newCDPSession(page);
    try {
      await coldMap(page, rec);
      await watchClusterAdds(page, {});
      await zoomToRepeater(page);
      const node = await pickMarker(page);
      await openPopup(page, false);
      await watchClusterAdds(page, { chunkInterval: 0 });
      // One task: off (the add in flight is dropped, the markers go on the
      // plain layer), then on (a new add starts). The dropped add goes on
      // in its timeouts and ends while the new one is current.
      await page.evaluate(() => {
        window.__onChunked = () => { const cb = document.getElementById('mcClusters'); cb.click(); cb.click(); };
      });
      const ok = await chunkedRefresh(page, rec, cdp, { until: (a0) => window.__adds.length >= a0 + 2 && window.__adds.every(a => a.done) });
      // The group's own onAdd adds its (empty) backlog in between.
      const c = await clusterState(page);
      const big = c.adds.filter(a => a.total > 0);
      assert(ok && big.length === 2 && big.every(a => a.done && a.chunks > 1), 'two overlapping chunked adds: ' + JSON.stringify(c.adds));
      await page.waitForTimeout(300);
      assertKept(await popupState(page), node.pk, 'after two adds in flight');
      // The current add is still the one that settles: a refresh rebuilds.
      // (Its group may show few markers until then: markercluster's dropped
      // add went on into the group re-added under it, as on master.)
      assert(await chunkedRefresh(page, rec, cdp, { fetch: true }), 'the next refresh did not rebuild');
      await page.waitForTimeout(300);
      assertKept(await popupState(page), node.pk, 'after the next refresh');
      const c2 = await clusterState(page);
      assert(c2.doubled === 0 && c2.markers === c2.adds.slice(-1)[0].total, 'markers twice or missing after the next refresh: ' + JSON.stringify(c2));
      clean(rec);
    } finally { await ctx.close(); }
  });

  // ── 3: a popup on a spiderfied cluster child ─────────────────────────────
  /* mast: one fixture repeater and three more nodes at its exact place (a
   * mast), so they cluster at every zoom and spiderfy when tapped. */
  function mastNodes(state) {
    return (list) => {
      if (!state.base) {
        state.base = list.find(n => n.role === 'repeater' && n.lat && n.lon);
        state.keys = [String(state.base.public_key).toLowerCase()];
        state.extra = [1, 2, 3].map(i => {
          const pk = String(state.base.public_key).slice(0, 56) + (0x98230000 + i).toString(16);
          state.keys.push(pk.toLowerCase());
          return Object.assign({}, state.base, { public_key: pk, name: 'Mast ' + i });
        });
      }
      return list.concat(state.extra);
    };
  }

  for (const o of [
    { label: 'desktop 1440×900 dark', viewport: { width: 1440, height: 900 }, theme: 'dark', shot: 'desktop-dark' },
    { label: 'phone 375×560 light', viewport: { width: 375, height: 560 }, touch: true, theme: 'light', shot: 'phone-light' },
  ]) {
    await step(`${o.label}, clustering on: a popup on a spiderfied child of co-located nodes stays through two refreshes, on its leg`, async () => {
      const mast = {};
      const { ctx, page, rec } = await newCtx(Object.assign({ clustering: true, nodes: mastNodes(mast) }, o));
      try {
        await coldMap(page, rec);
        await watchClusterAdds(page, {});
        await page.evaluate((pk) => {
          const map = window.__maps[window.__maps.length - 1];
          let mk = null;
          window.__group.eachLayer((l) => { if (String(l._nodeKey).toLowerCase() === pk) mk = l; });
          map.setView(mk.getLatLng(), 15, { animate: false });
        }, mast.keys[0]);
        await page.waitForTimeout(600);
        // Tap the mast's cluster: at the cluster group's last zoom it
        // spiderfies.
        await page.evaluate((pk) => {
          let mk = null;
          window.__group.eachLayer((l) => { if (String(l._nodeKey).toLowerCase() === pk) mk = l; });
          window.__group.getVisibleParent(mk)._icon.setAttribute('data-test-cluster', '1');
        }, mast.keys[0]);
        await tap(page, '[data-test-cluster]', o.touch);
        await page.waitForFunction((keys) => {
          const map = window.__maps[window.__maps.length - 1];
          let n = 0;
          map.eachLayer((l) => { if (l._nodeKey && keys.indexOf(String(l._nodeKey).toLowerCase()) >= 0 && l._spiderLeg) n++; });
          return n === keys.length;
        }, mast.keys, { timeout: 5000 });
        await page.waitForTimeout(500);
        const node = await pickMarker(page, { keys: mast.keys });
        await openPopup(page, o.touch);
        let s = await popupState(page);
        assert(s.spider && s.atMarker, 'the popup is not on a spiderfied child: ' + JSON.stringify(s));
        await advertRefresh(page, rec, { pk: node.pk });
        assertKept(await popupState(page), node.pk, 'after a cached refresh');
        await advertRefresh(page, rec, { pk: node.pk, fetch: true });
        s = await popupState(page);
        assertKept(s, node.pk, 'after a refetch');
        assert(s.spider, 'the spider was not restored around the popup: ' + JSON.stringify(s));
        const legs = await page.evaluate((keys) => {
          const map = window.__maps[window.__maps.length - 1];
          let n = 0, legs = 0;
          map.eachLayer((l) => {
            if (l._nodeKey && keys.indexOf(String(l._nodeKey).toLowerCase()) >= 0) n++;
            if (l instanceof L.Polyline && l._path && l._path.classList.contains('leaflet-cluster-spider-leg')) legs++;
          });
          return { n, legs };
        }, mast.keys);
        assert(legs.n === mast.keys.length, 'the mast\'s markers on the map: ' + JSON.stringify(legs));
        const c = await clusterState(page);
        assert(c.doubled === 0, 'markers twice: ' + JSON.stringify(c));
        await shot(page, `982-spider-popup-${o.shot}.jpg`);
        clean(rec);
      } finally { await ctx.close(); }
    });
  }

  await step('desktop 1440×900, clustering on: a kept popup whose marker a refresh puts in a new cluster still closes', async () => {
    const st = { pk: null, extra: null };
    const o = { viewport: { width: 1440, height: 900 }, clustering: true,
      nodes: list => (st.extra ? list.concat(st.extra) : list) };
    const { ctx, page, rec } = await newCtx(o);
    try {
      await coldMap(page, rec);
      await watchClusterAdds(page, {});
      await zoomToRepeater(page);
      const node = await pickMarker(page);
      await openPopup(page, false);
      // The next answer has a new node at the very same place.
      st.extra = await page.evaluate((pk) => {
        const src = window.__maps[window.__maps.length - 1]._popup._source;
        const ll = src.getLatLng();
        return [{ public_key: pk.slice(0, 56) + '98240001', name: 'Newcomer', role: 'repeater', lat: ll.lat, lon: ll.lng,
          last_seen: new Date().toISOString(), last_heard: new Date().toISOString() }];
      }, node.pk);
      await advertRefresh(page, rec, { pk: node.pk, fetch: true });
      const s = await popupState(page);
      assert(!s.open && s.closes === 1, 'a kept popup whose marker joined a cluster: ' + JSON.stringify(s));
      clean(rec);
    } finally { await ctx.close(); }
  });

  // ── 4: closing a popup on a dense phone map ──────────────────────────────
  /* buttonsOver: the map buttons that are visible and overlap the open
   * popup, and whether a finger at the middle of its × hits the ×. */
  function popupVsButtons(page, sel) {
    return page.evaluate((sel) => {
      const pop = document.querySelector('.leaflet-popup');
      const x = document.querySelector('.leaflet-popup-close-button');
      if (!pop || !x) return null;
      const p = pop.querySelector('.leaflet-popup-content-wrapper').getBoundingClientRect();
      const xr = x.getBoundingClientRect();
      const hit = document.elementFromPoint(xr.left + xr.width / 2, xr.top + xr.height / 2);
      const over = [];
      document.querySelectorAll(sel).forEach((b) => {
        const r = b.getBoundingClientRect(), cs = getComputedStyle(b);
        if (!r.width || cs.visibility === 'hidden' || cs.display === 'none') return;
        if (r.left < p.right && r.right > p.left && r.top < p.bottom && r.bottom > p.top) over.push(b.className.toString().slice(0, 40));
      });
      return { over, xOnTop: !!hit && (hit === x || x.contains(hit)), x: { w: xr.width, h: xr.height } };
    }, sel);
  }

  /* placeMarker moves the map so that a visible repeater singleton sits at
   * (fx, fy) of the map's size, and marks it. */
  async function placeMarker(page, fx, fy) {
    const ok = await page.evaluate(({ fx, fy }) => {
      const map = window.__maps[window.__maps.length - 1];
      const size = map.getSize();
      let best = null;
      map.eachLayer((l) => {
        if (!l._icon || !l._nodeKey || l._role !== 'repeater') return;
        const p = map.latLngToContainerPoint(l.getLatLng());
        const d = Math.abs(p.x - size.x / 2) + Math.abs(p.y - size.y / 2);
        if (!best || d < best.d) best = { l, d };
      });
      if (!best) return false;
      map.closePopup();
      const p = map.latLngToContainerPoint(best.l.getLatLng());
      map.panBy([p.x - size.x * fx, p.y - size.y * fy], { animate: false });
      window.__placed = best.l;
      return true;
    }, { fx, fy });
    assert(ok, 'no repeater marker to place');
    await page.waitForTimeout(500);
    const marked = await page.evaluate(() => {
      const l = window.__placed;
      document.querySelectorAll('[data-test-marker]').forEach(e => e.removeAttribute('data-test-marker'));
      if (!l._icon) return false;
      l._icon.setAttribute('data-test-marker', '1');
      return true;
    });
    assert(marked, 'the placed marker is not on the map');
  }

  /* nearMiss finds a point of bare map 5–7 px beside a marker icon, with no
   * other marker within 18 px and clear of the popup: a finger aimed at the
   * map that a touch-adjusting browser moves onto that marker. */
  function nearMissPoints(page) {
    return page.evaluate(() => {
      const bare = (x, y) => { const h = document.elementFromPoint(x, y); return !!h && (h.classList.contains('leaflet-container') || !!h.closest('.leaflet-tile-pane')); };
      const m = document.getElementById('leaflet-map').getBoundingClientRect();
      const pop = document.querySelector('.leaflet-popup');
      const pb = pop ? pop.getBoundingClientRect() : null;
      const out = [];
      document.querySelectorAll('#leaflet-map .leaflet-marker-pane .leaflet-marker-icon').forEach((ic) => {
        const b = ic.getBoundingClientRect();
        for (const d of [5, 7]) {
          for (const [x, y] of [[b.right + d, b.top + b.height / 2], [b.left - d, b.top + b.height / 2], [b.left + b.width / 2, b.bottom + d]]) {
            if (x < m.left + 24 || x > m.right - 24 || y < m.top + 24 || y > m.bottom - 24) continue;
            if (pb && x > pb.left - 30 && x < pb.right + 30 && y > pb.top - 30 && y < pb.bottom + 40) continue;
            let ok = bare(x, y);
            for (let a = 0; ok && a < 16; a++) {
              const h = document.elementFromPoint(x + 18 * Math.cos(a * Math.PI / 8), y + 18 * Math.sin(a * Math.PI / 8));
              const other = h && h.closest('.leaflet-marker-icon');
              if (other && other !== ic) ok = false;
            }
            if (ok) out.push({ x, y, node: !ic.classList.contains('mc-cluster-wrap') });
          }
        }
      });
      // A node's marker first: on a cluster the tap would zoom in.
      return out.sort((a, b) => (b.node ? 1 : 0) - (a.node ? 1 : 0));
    });
  }

  // Both viewports in both themes, clustering on and off (off is the
  // denser map: every node its own marker).
  for (const o of [
    { label: 'phone 360×640 light, clustering off', viewport: { width: 360, height: 640 }, theme: 'light', shot: '360-light' },
    { label: 'phone 360×640 dark, clustering on', viewport: { width: 360, height: 640 }, theme: 'dark', clustering: true, shot: '360-dark' },
    { label: 'phone 375×560 light, clustering on', viewport: { width: 375, height: 560 }, theme: 'light', clustering: true, shot: '375-light' },
    { label: 'phone 375×560 dark, clustering off', viewport: { width: 375, height: 560 }, theme: 'dark', shot: '375-dark' },
  ]) {
    const { ctx, page, rec } = await newCtx(Object.assign({ touch: true }, o));
    const cdp = await ctx.newCDPSession(page);
    try {
      await coldMap(page, rec);

      await step(`${o.label}: a popup near a top corner is clear of the map buttons, its × is on top, and the buttons come back when it closes`, async () => {
        for (const [fx, side] of [[0.78, 'right'], [0.25, 'left']]) {
          await placeMarker(page, fx, 0.16);
          await openPopup(page, true);
          const r = await popupVsButtons(page, MAP_BUTTONS);
          assert(r && r.over.length === 0, `${side}: the popup is under map buttons: ` + JSON.stringify(r));
          assert(r.xOnTop, `${side}: the popup's × is covered: ` + JSON.stringify(r));
          assert(r.x.w >= 44 && r.x.h >= 44, `${side}: the × is ${r.x.w}×${r.x.h}`);
          if (side === 'right') await shot(page, `982-popup-corner-${o.shot}.jpg`);
          await tap(page, '.leaflet-popup-close-button', true);
          await page.waitForFunction(() => !document.querySelector('.leaflet-popup'), null, { timeout: 5000 });
          const back = await settledBox(page, '#map-wrap .leaflet-control-zoom-in');
          assert(back.onTarget, `${side}: the zoom button did not come back`);
          await page.waitForTimeout(600);
        }
      });

      await step(`${o.label}: View Node and Show Neighbors are at least 44 px tall and on top`, async () => {
        await placeMarker(page, 0.5, 0.7);
        await openPopup(page, true);
        for (const sel of ['.leaflet-popup .map-popup a[href^="#/nodes/"]', '.leaflet-popup [data-show-neighbors]']) {
          const b = await settledBox(page, sel);
          assert(b.h >= 44 && b.w >= 44, sel + ' is ' + b.w + '×' + b.h);
        }
        await shot(page, `982-popup-links-${o.shot}.jpg`);
      });

      await step(`${o.label}: a finger on bare map beside a marker closes the open popup instead of opening that marker's; a tap on a marker still opens it`, async () => {
        // A popup near the top leaves the map below it to tap beside.
        await placeMarker(page, 0.5, 0.15);
        await openPopup(page, true);
        let res = null;
        const cands = await nearMissPoints(page);
        for (const p of cands.slice(0, 12)) {
          const before = await popupState(page);
          if (!before.open) break;
          const z0 = await page.evaluate(() => window.__maps[window.__maps.length - 1].getZoom());
          await fingerTap(page, cdp, p.x, p.y, 12);
          await page.waitForTimeout(500);
          const downs = await page.evaluate(() => window.__downs);
          const after = Object.assign(await popupState(page), { node: p.node,
            zoomed: (await page.evaluate(() => window.__maps[window.__maps.length - 1].getZoom())) !== z0 });
          // Only a touch the browser moved onto the marker is the case.
          if (!(downs.length && downs[0].onIcon)) {
            assert(!after.open, 'a tap on bare map left the popup open: ' + JSON.stringify(after));
            await placeMarker(page, 0.5, 0.15);
            await openPopup(page, true);
            continue;
          }
          res = { p, after, key0: before.key };
          break;
        }
        assert(res, 'no tap was moved onto a marker by touch adjustment (' + cands.length + ' candidates)');
        assert(!res.after.open && res.after.closes === 1 && !res.after.zoomed,
          'a tap beside a marker was taken as a tap on it instead of closing the open popup: ' + JSON.stringify(res));
        await shot(page, `982-near-miss-closed-${o.shot}.jpg`);
        // With no popup open, a tap on a marker opens it ...
        await placeMarker(page, 0.5, 0.15);
        const a = await settledBox(page, '[data-test-marker]');
        await fingerTap(page, cdp, a.x, a.y, 12);
        await popupSettled(page);
        const k1 = (await popupState(page)).key;
        // ... and with one open, a tap on another marker is that marker's:
        // a node's opens its popup, a cluster's zooms in or spiderfies.
        const other = await page.evaluate((k1) => {
          const map = window.__maps[window.__maps.length - 1];
          const m = map.getContainer().getBoundingClientRect();
          const pb = document.querySelector('.leaflet-popup').getBoundingClientRect();
          const picks = [];
          document.querySelectorAll('#leaflet-map .leaflet-marker-pane .leaflet-marker-icon').forEach((ic) => {
            const r = ic.getBoundingClientRect();
            const x = r.left + r.width / 2, y = r.top + r.height / 2;
            if (x < m.left + 24 || x > m.right - 24 || y < m.top + 24 || y > m.bottom - 24) return;
            if (x > pb.left - 20 && x < pb.right + 20 && y > pb.top - 20 && y < pb.bottom + 30) return;
            const h = document.elementFromPoint(x, y);
            if (!h || !(h === ic || ic.contains(h))) return;
            let layer = null;
            map.eachLayer((l) => { if (l._icon === ic) layer = l; });
            if (!layer || String(layer._nodeKey).toLowerCase() === k1) return;
            picks.push({ x, y, k: layer._nodeKey ? String(layer._nodeKey).toLowerCase() : null, z: map.getZoom() });
          });
          picks.sort((a, b) => (a.k ? 0 : 1) - (b.k ? 0 : 1));
          return picks[0] || null;
        }, k1);
        assert(other, 'no other marker clear of the popup');
        await fingerTap(page, cdp, other.x, other.y, 12);
        await page.waitForFunction((o) => {
          const map = window.__maps[window.__maps.length - 1];
          const s = map._popup && map._popup.isOpen() && map._popup._source;
          if (o.k) return !!s && String(s._nodeKey).toLowerCase() === o.k;
          return map.getZoom() > o.z || Object.values(map._layers).some(l => l._spiderLeg);
        }, other, { timeout: 5000 }).catch(() => { throw new Error('a tap on another marker was not that marker\'s: ' + JSON.stringify(other)); });
        clean(rec);
      });
    } finally { await ctx.close(); }
  }

  await step('desktop 1440×900: the popup links stay compact, and the map buttons stay while a popup is open', async () => {
    const { ctx, page, rec } = await newCtx({ viewport: { width: 1440, height: 900 }, theme: 'dark' });
    try {
      await coldMap(page, rec);
      await pickMarker(page);
      await openPopup(page, false);
      for (const sel of ['.leaflet-popup .map-popup a[href^="#/nodes/"]', '.leaflet-popup [data-show-neighbors]']) {
        const b = await settledBox(page, sel);
        assert(b.h < 30, sel + ' grew on a desktop: ' + b.h);
      }
      const z = await settledBox(page, '#map-wrap .leaflet-control-zoom-in');
      assert(z.onTarget, 'the zoom button is gone on a desktop');
      await shot(page, '982-popup-desktop-dark.jpg');
      clean(rec);
    } finally { await ctx.close(); }
  });

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
