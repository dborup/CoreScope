/**
 * E2E (#959): "Predicted reach" and "Terrain profile" in the Map page's
 * node popup, in a real browser.
 *
 * The CI server has no DHM token and no coverage config, so both features
 * are switched on the way an operator's config would: /api/config/client
 * is given the `dhm` block (profile: true) and the `coverage` block that
 * cmd/server sends, and /api/terrain/coverage and its raster are a stub in
 * the server's shape. Nothing reaches a real network.
 *
 * Asserts, on a 960×720 desktop and a 375×560 touch phone, from a cold
 * #/map with real clicks and taps:
 *   1. rendering the markers builds no action (no per-marker work); one
 *      popup open builds each action once and sends no request, and the
 *      handler costs well under 1 ms per open, a full lookup included;
 *   2. the popup shows View Node, Show Neighbors, Terrain profile and
 *      Predicted reach for that node, wholly on the map (it is panned again
 *      once the actions are in); on the phone each chip is a 44 px target
 *      that a finger at its middle hits;
 *   3. Predicted reach opens the reach for that node in place: one request
 *      for that key, the panel names the node, reach=<key> in the address
 *      bar, no hashchange, the same map (no reload) and the same view;
 *   4. Terrain profile from a reopened popup starts the tool with that node
 *      as A and the picker armed for B: no hashchange, the same map,
 *      profile=<key> in the address bar, no profile request yet, and the
 *      reach put away;
 *   5. with both features off the popup has no action; with only the reach
 *      on, only Predicted reach;
 *   6. no page error, and no request leaves localhost.
 *
 * SHOTS_DIR, when set, receives JPEG screenshots (≤ 960 px, quality 60).
 *
 * Usage:
 *   BASE_URL=http://localhost:13581 node test-issue-959-popup-actions-e2e.js
 */
'use strict';
const path = require('path');
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const SHOTS = process.env.SHOTS_DIR || '';
const COVERAGE_PATH = '/api/terrain/coverage';
const RASTER_PATH = '/api/terrain/coverage/raster';
const PROFILE_PATH = '/api/terrain/profile';

const DHM_ATTRIBUTION = 'Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel, CC BY 4.0';
const DHM_BLOCK = {
  enabled: true, url: '/api/map/dhm/wms', layer: 'dhm_terraen_skyggekort', minZoom: 7, maxZoom: 19, profile: true,
  bounds: [[54.4265, 7.99125], [57.778, 15.5995]], attribution: DHM_ATTRIBUTION,
  datasetName: 'Danmarks Højdemodel', datasetUrl: 'https://dataforsyningen.dk/data/930',
  licenseName: 'CC BY 4.0', licenseUrl: 'https://creativecommons.org/licenses/by/4.0/',
  attributionDerived: DHM_ATTRIBUTION + ' — data bearbejdet', overlays: [],
};
const COVERAGE_BLOCK = {
  enabled: true, maxRadiusKm: 40, radiusKm: 40, resolutionM: 30, rxHeightM: 2,
  pointAntenna: { m: 2, source: 'point-default' },
  radio: { txPowerDbm: 22, antennaGainDbi: 3, rxGainDbi: 3, lossDb: 0, sensitivityDbm: -130 }, dhm: true,
};
const PNG_1X1 = Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGM4ceIEAAS0AlkWLoFAAAAAAElFTkSuQmCC', 'base64');
const REACH = '.leaflet-popup [data-testid="node-predicted-reach"]';
const PROFILE = '.leaflet-popup [data-testid="node-terrain-profile"]';

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
  console.log(`\n=== #959 Map popup: Predicted reach and Terrain profile against ${BASE} ===\n`);

  // Every node with a position, by the API (the stub centres the reach on it).
  const byKey = await (async () => {
    const ctx = await browser.newContext();
    const page = await ctx.newPage();
    await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
    const list = await page.evaluate(async () => {
      const r = await fetch('/api/nodes?limit=2000');
      const d = await r.json();
      return (d.nodes || d).map(n => ({ pk: String(n.public_key || '').toLowerCase(), name: n.name, lat: n.lat, lon: n.lon }));
    });
    await ctx.close();
    return new Map(list.filter(n => n.lat && n.lon).map(n => [n.pk, n]));
  })();
  assert(byKey.size > 0, 'the fixture needs nodes with a position');

  async function newCtx(o) {
    o = o || {};
    const ctx = await browser.newContext({
      viewport: o.viewport || { width: 960, height: 720 },
      hasTouch: !!o.touch, isMobile: !!o.touch,
    });
    const rec = { errors: [], all: [], api: [], coverage: [], profile: [] };
    await ctx.route(BASE + '/api/config/client', async (route) => {
      const response = await route.fetch();
      const config = await response.json();
      delete config.dhm; delete config.coverage;
      if (o.dhm !== false) config.dhm = DHM_BLOCK;
      if (o.coverage !== false) config.coverage = COVERAGE_BLOCK;
      await route.fulfill({ response, json: config });
    });
    await ctx.route(url => new URL(url).pathname === COVERAGE_PATH, async (route) => {
      const u = new URL(route.request().url());
      rec.coverage.push(u.searchParams.get('at'));
      const n = byKey.get(u.searchParams.get('at')) || { lat: 37.8, lon: -122.3 };
      const d = 0.3;
      const body = {
        key: 'f'.repeat(32), center: { lat: n.lat, lon: n.lon, kind: 'node', role: 'repeater', groundM: 34 },
        params: { radiusKm: 40, resolutionM: 30, txHeightM: 10, txHeightSource: 'role-default', txAssumed: true,
          rxHeightM: 2, rxHeightSource: 'default', freqMHz: 869.618, freqSource: 'firmware', k: 4 / 3, kSource: 'default', model: 'bullington-p526-v1' },
        raster: { url: RASTER_PATH + '?key=' + 'f'.repeat(32), width: 32, height: 32, bounds: [[n.lat - d, n.lon - d], [n.lat + d, n.lon + d]],
          encoding: 'loss-u8', lossBaseDb: 60, lossStepDb: 0.5, noData: 255, coveredPct: 100, bytes: 1024 },
        sources: [{ id: 'dhm-dtm', resolutionM: 30, model: 'dtm' }],
        attribution: DHM_BLOCK.attributionDerived,
      };
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(body) });
    });
    await ctx.route(url => new URL(url).pathname === RASTER_PATH, (route) =>
      route.fulfill({ status: 200, contentType: 'application/octet-stream', body: Buffer.alloc(1024, 120) }));
    await ctx.route(url => new URL(url).pathname === PROFILE_PATH, (route) => {
      rec.profile.push(route.request().url());
      return route.fulfill({ status: 500, contentType: 'application/json', body: '{"error":"not in this test"}' });
    });
    await ctx.route(url => new URL(url).pathname === '/api/map/dhm/wms', (route) =>
      route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 }));
    await ctx.route(/^https?:\/\/(?!localhost|127\.0\.0\.1)/i, (route) =>
      route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 }));
    await ctx.addInitScript(() => {
      try {
        localStorage.setItem('meshcore-theme', 'light');
        localStorage.setItem('meshcore-map-clustering', 'false');
        ['row-swipe', 'tab-swipe', 'edge-drawer', 'pull-refresh']
          .forEach(k => localStorage.setItem('meshcore-gesture-hints-' + k, 'seen'));
      } catch (_) {}
      // Every hashchange: from #/map it would rebuild the page (app.js navigate()).
      window.__hashchanges = 0;
      window.addEventListener('hashchange', () => { window.__hashchanges++; });
      // Every Leaflet map made, so a reload shows as a second one.
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
      // Every nodeEntryAction call: rendering markers must make none.
      window.__builds = { 'node-terrain-profile': 0, 'node-predicted-reach': 0 };
      const spy = (name, testid) => {
        let real;
        Object.defineProperty(window, name, {
          configurable: true,
          get() { return real; },
          set(v) {
            real = v;
            if (v && typeof v.nodeEntryAction === 'function') {
              const f = v.nodeEntryAction;
              v.nodeEntryAction = function () { window.__builds[testid]++; return f.apply(this, arguments); };
            }
          },
        });
      };
      spy('TerrainProfile', 'node-terrain-profile');
      spy('PredictedReach', 'node-predicted-reach');
    });
    const page = await ctx.newPage();
    page.on('pageerror', e => rec.errors.push(String(e)));
    page.on('request', r => {
      rec.all.push(r.url());
      const u = new URL(r.url());
      if (/^(localhost|127\.0\.0\.1)$/.test(u.hostname) && u.pathname.startsWith('/api/')) rec.api.push(u.pathname + u.search);
    });
    return { ctx, page, rec };
  }

  async function coldMap(page) {
    await page.goto(`${BASE}/#/map`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#leaflet-map[data-loaded="true"]', { timeout: 30000 });
    await page.evaluate(() => window.MeshConfigReady);
    await page.waitForSelector('.leaflet-marker-icon', { timeout: 15000 });
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
        return { x, y, w: r.width, h: r.height, left: r.left, right: r.right, top: r.top, bottom: r.bottom,
          onTarget: !!hit && (hit === e || e.contains(hit)) };
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
    return b;
  }

  /* pickMarker marks a node marker a finger can hit, clear of the map's
   * buttons and with room for its popup above it, and answers its key. */
  async function pickMarker(page) {
    const key = await page.evaluate(() => {
      const map = window.__maps[window.__maps.length - 1];
      const size = map.getSize();
      const box = map.getContainer().getBoundingClientRect();
      const found = [];
      map.eachLayer((l) => {
        if (!l._icon || !l._nodeKey || !l.getPopup || !l.getPopup()) return;
        const html = String(l.getPopup().getContent() || '');
        // A node's popup (View Node), not an observer-only marker's.
        if (html.indexOf('href="#/nodes/') < 0) return;
        const r = l._icon.getBoundingClientRect();
        const x = r.left + r.width / 2, y = r.top + r.height / 2;
        if (x < box.left + 60 || x > box.left + size.x - 60 || y < box.top + size.y * 0.45 || y > box.top + size.y - 40) return;
        const hit = document.elementFromPoint(x, y);
        if (!hit || !(hit === l._icon || l._icon.contains(hit))) return;
        found.push({ l, d: Math.abs(x - box.left - size.x / 2) });
      });
      found.sort((a, b) => a.d - b.d);
      if (!found.length) return null;
      document.querySelectorAll('[data-test-marker]').forEach(e => e.removeAttribute('data-test-marker'));
      found[0].l._icon.setAttribute('data-test-marker', '1');
      return String(found[0].l._nodeKey).toLowerCase();
    });
    assert(key && byKey.has(key), 'no node marker to tap: ' + key);
    return byKey.get(key);
  }

  async function openPopup(page, touch) {
    await tap(page, '[data-test-marker]', touch);
    await page.waitForSelector('.leaflet-popup .map-popup', { timeout: 5000 });
    // Leaflet pans the map to fit the popup it opens.
    await page.waitForFunction(() => {
      const m = window.__maps[window.__maps.length - 1];
      return !(m._panAnim && m._panAnim._inProgress);
    }, null, { timeout: 5000 });
    await page.waitForTimeout(400);
  }

  function view(page) {
    return page.evaluate(() => {
      const m = window.__maps[window.__maps.length - 1];
      const c = m.getCenter();
      return { lat: c.lat, lng: c.lng, zoom: m.getZoom(), maps: window.__maps.length, hashchanges: window.__hashchanges,
        keep: document.getElementById('leaflet-map').getAttribute('data-test-keep') };
    });
  }

  function popupLinks(page) {
    return page.$$eval('.leaflet-popup .map-popup a', as => as.map(a => (a.getAttribute('data-testid') || '') + '|' + a.textContent.trim() + '|' + a.getAttribute('href')));
  }

  async function shot(page, name) {
    if (!SHOTS) return;
    await page.screenshot({ path: path.join(SHOTS, name), type: 'jpeg', quality: 60 });
  }

  function clean(rec) {
    const out = rec.all.filter(u => !/^(https?:\/\/(localhost|127\.0\.0\.1)[:/]|data:|blob:)/i.test(u));
    const real = out.filter(u => /dataforsyningen|amazonaws|copernicus/i.test(u));
    assert(real.length === 0, 'a request reached a real upstream: ' + real.join(', '));
    assert(rec.errors.length === 0, 'page errors: ' + rec.errors.join(' | '));
  }

  // ── 1–4: the round trip, desktop and phone ────────────────────────────────
  for (const o of [
    { label: 'desktop 960×720', viewport: { width: 960, height: 720 }, touch: false, shot: 'desktop' },
    { label: 'phone 375×560', viewport: { width: 375, height: 560 }, touch: true, shot: 'phone' },
  ]) {
    const { ctx, page, rec } = await newCtx(o);
    try {
      await coldMap(page);
      await page.waitForSelector('[data-testid="map-reach-toggle"]', { timeout: 15000 });
      await page.waitForSelector('[data-testid="map-profile-toggle"]', { timeout: 15000 });
      let node = null;

      await step(`${o.label}: rendering the markers builds no popup action`, async () => {
        const n = await page.evaluate(() => ({ markers: document.querySelectorAll('.leaflet-marker-icon').length, builds: window.__builds }));
        assert(n.markers > 10, 'markers: ' + n.markers);
        assert(n.builds['node-terrain-profile'] === 0 && n.builds['node-predicted-reach'] === 0, 'built at render: ' + JSON.stringify(n.builds));
      });

      if (!o.touch) {
        await step(`${o.label}: one popup open costs well under a millisecond, a full node lookup included`, async () => {
          // The real handler, through the real map: for the last node marker,
          // and for a key not in the list (the lookup walks every node). The
          // fake popup's box is detached, so the shared mount appends nothing
          // and nothing pans.
          const r = await page.evaluate(() => {
            const map = window.__maps[window.__maps.length - 1];
            let last = null;
            map.eachLayer((l) => { if (l._nodeKey && l.getPopup && String(l.getPopup().getContent() || '').indexOf('href="#/nodes/') >= 0) last = l._nodeKey; });
            const N = 500;
            const boxes = [];
            for (let i = 0; i < N; i++) {
              const el = document.createElement('div');
              el.innerHTML = '<div class="map-popup" data-node-popup></div>';
              boxes.push({ getElement: () => el, _source: { _nodeKey: last }, isOpen: () => false });
            }
            // A key no node has: the lookup walks the whole list and gives up.
            const miss = { getElement: () => boxes[0].getElement(), _source: { _nodeKey: 'f'.repeat(64) }, isOpen: () => false };
            const before = Object.assign({}, window.__builds);
            let t0 = performance.now();
            for (let i = 0; i < N; i++) map.fire('popupopen', { popup: boxes[i] });
            const ms = (performance.now() - t0) / N;
            t0 = performance.now();
            for (let i = 0; i < N; i++) map.fire('popupopen', { popup: miss });
            const missMs = (performance.now() - t0) / N;
            const rows = boxes.filter(b => b.getElement().querySelectorAll('.map-popup-tools').length === 1).length;
            return { ms, missMs, rows, N, markers: document.querySelectorAll('.leaflet-marker-icon').length, before };
          });
          console.log(`    popupopen handler: ${r.ms.toFixed(4)} ms per open, ${r.missMs.toFixed(4)} ms for a key not in the list (${r.N} opens each, ${r.markers} markers)`);
          assert(r.rows === r.N, 'the handler did not run once for every open: ' + r.rows);
          assert(r.ms < 1 && r.missMs < 1, 'one popup open took ' + r.ms + ' / ' + r.missMs + ' ms');
          await page.waitForTimeout(100);
          const after = await page.evaluate(() => window.__builds);
          assert(JSON.stringify(after) === JSON.stringify(r.before), 'a detached popup built an action: ' + JSON.stringify(after));
        });
      }

      await step(`${o.label}: the popup shows View Node, Show Neighbors, Terrain profile and Predicted reach, with no request`, async () => {
        node = await pickMarker(page);
        const before = rec.api.length;
        await openPopup(page, o.touch);
        await page.waitForSelector(REACH, { timeout: 5000 });
        await page.waitForSelector(PROFILE, { timeout: 5000 });
        const links = await popupLinks(page);
        assert(links.length === 4, 'links: ' + JSON.stringify(links));
        assert(links[0] === '|View Node →|#/nodes/' + node.pk, 'View Node: ' + links[0]);
        assert(/^\|Show Neighbors\|/.test(links[1]), 'Show Neighbors: ' + links[1]);
        assert(links[2] === 'node-terrain-profile|Terrain profile|#/map?profile=' + node.pk, 'Terrain profile: ' + links[2]);
        assert(links[3] === 'node-predicted-reach|Predicted reach|#/map?reach=' + node.pk, 'Predicted reach: ' + links[3]);
        const builds = await page.evaluate(() => window.__builds);
        assert(builds['node-terrain-profile'] === 1 && builds['node-predicted-reach'] === 1, 'one build per action for one open: ' + JSON.stringify(builds));
        const sent = rec.api.slice(before);
        assert(sent.length === 0, 'requests on popup open: ' + sent.join(', '));
        if (o.touch) {
          for (const sel of [REACH, PROFILE]) {
            const b = await settledBox(page, sel);
            assert(b.w >= 44 && b.h >= 44, `${sel} target ${b.w}x${b.h}`);
            assert(b.left >= 0 && b.right <= o.viewport.width, `${sel} off screen: ${b.left}..${b.right}`);
          }
        } else {
          const b = await settledBox(page, REACH);
          assert(b.h < 44, 'desktop chip is compact: ' + b.h);
        }
        // The actions grew the popup after Leaflet had panned it into view:
        // it is panned again, so the whole popup is on the map, not under
        // the top bar.
        await page.waitForTimeout(500);
        const fit = await page.evaluate(() => {
          const p = document.querySelector('.leaflet-popup').getBoundingClientRect();
          const m = document.getElementById('leaflet-map').getBoundingClientRect();
          const top = document.elementFromPoint(p.left + p.width / 2, p.top + 4);
          return { p: [p.left, p.top, p.right, p.bottom], m: [m.left, m.top, m.right, m.bottom],
            topHit: !!top && !!top.closest('.leaflet-popup') };
        });
        assert(fit.p[1] >= fit.m[1] && fit.p[3] <= fit.m[3] && fit.p[0] >= fit.m[0] && fit.p[2] <= fit.m[2] && fit.topHit,
          'the popup is not wholly on the map: ' + JSON.stringify(fit));
        // The chip text is the theme's, not Leaflet's popup link colour.
        const c = await page.$eval(REACH, e => [getComputedStyle(e).color, getComputedStyle(e).backgroundColor]);
        assert(c[0] !== c[1] && c[0] !== 'rgb(0, 120, 168)', 'chip colours: ' + c);
        await shot(page, `959-popup-${o.shot}.jpg`);
      });

      await step(`${o.label}: Predicted reach opens that node's reach in place, keeping the map and its view`, async () => {
        await page.evaluate(() => document.getElementById('leaflet-map').setAttribute('data-test-keep', '1'));
        const v0 = await view(page);
        await tap(page, REACH, o.touch);
        await page.waitForSelector('[data-testid="reach-panel"]:not([hidden])', { timeout: 10000 });
        await page.waitForFunction(() => {
          const c = document.querySelector('canvas.reach-layer');
          return c && c.width > 0 && !document.querySelector('[data-testid="reach-panel"].is-busy');
        }, null, { timeout: 10000 });
        await page.waitForFunction((pk) => new RegExp('[?&]reach=' + pk).test(location.hash), node.pk, { timeout: 3000 })
          .catch(() => { throw new Error('no reach=<key> in the address bar: ' + page.url()); });
        assert(rec.coverage.length === 1 && rec.coverage[0] === node.pk, 'coverage requests: ' + JSON.stringify(rec.coverage));
        const centre = await page.textContent('[data-testid="reach-centre"]');
        assert(centre.indexOf('From ' + node.name) === 0, 'panel centre: ' + centre);
        const v1 = await view(page);
        assert(v1.hashchanges === v0.hashchanges, 'a hashchange: the page was rebuilt');
        assert(v1.maps === 1 && v1.keep === '1', 'the map was rebuilt: ' + JSON.stringify(v1));
        assert(v1.zoom === v0.zoom && Math.abs(v1.lat - v0.lat) < 1e-9 && Math.abs(v1.lng - v0.lng) < 1e-9,
          'the view moved: ' + JSON.stringify([v0, v1]));
        assert(!(await page.$('.leaflet-popup')), 'the popup stayed open over the reach');
        await shot(page, `959-reach-${o.shot}.jpg`);
      });

      await step(`${o.label}: Terrain profile starts the tool with that node as A, in place`, async () => {
        // The phone's reach panel covers the lower map; close it first, as
        // a user would, so the marker is in reach of a finger again.
        if (o.touch) await tap(page, '[data-testid="reach-close"]', true);
        await page.waitForTimeout(300);
        await openPopup(page, o.touch);
        await page.waitForSelector(PROFILE, { timeout: 5000 });
        const v0 = await view(page);
        await tap(page, PROFILE, o.touch);
        await page.waitForSelector('[data-testid="tp-panel"].is-open', { timeout: 10000 });
        await page.waitForTimeout(500);
        const s = await page.evaluate(() => {
          const q = (x) => document.querySelector(x);
          const hint = q('[data-testid="map-profile-hint"]');
          return {
            nameA: (q('[data-testid="tp-name-a"]') || {}).textContent || '',
            nameB: (q('[data-testid="tp-name-b"]') || {}).textContent || '',
            pressed: q('[data-testid="map-profile-toggle"]').getAttribute('aria-pressed'),
            picking: !!q('.leaflet-container.tp-picking'),
            hint: hint && !hint.hidden ? hint.textContent : '',
            reachOpen: !!q('[data-testid="reach-panel"]:not([hidden])'),
            hash: location.hash,
          };
        });
        assert(s.nameA === node.name, `A = "${s.nameA}", want "${node.name}"`);
        assert(s.nameB === 'not picked', 'B = ' + s.nameB);
        assert(s.pressed === 'true' && s.picking, 'the picker is not armed for B: ' + JSON.stringify(s));
        assert(/other end/.test(s.hint), 'hint: ' + s.hint);
        assert(!s.reachOpen, 'the reach stayed open beside the profile');
        assert(new RegExp('[?&]profile=' + node.pk + '(&|$)').test(s.hash), 'address bar: ' + s.hash);
        assert(rec.profile.length === 0, 'a profile request before B');
        const v1 = await view(page);
        assert(v1.hashchanges === v0.hashchanges, 'a hashchange: the page was rebuilt');
        assert(v1.maps === 1 && v1.keep === '1', 'the map was rebuilt: ' + JSON.stringify(v1));
        assert(v1.zoom === v0.zoom, 'the zoom changed: ' + JSON.stringify([v0, v1]));
        assert(!(await page.$('.leaflet-popup')), 'the popup stayed open');
        await shot(page, `959-profile-${o.shot}.jpg`);
        clean(rec);
      });
    } finally { await ctx.close(); }
  }

  // ── 5: features off ──────────────────────────────────────────────────────
  for (const o of [
    { label: 'both features off', dhm: false, coverage: false, want: [] },
    { label: 'only the reach on', dhm: false, coverage: true, want: ['node-predicted-reach'] },
    { label: 'only the profile on, phone', dhm: true, coverage: false, want: ['node-terrain-profile'], viewport: { width: 375, height: 560 }, touch: true },
  ]) {
    await step(`${o.label}: the popup offers ${o.want.length ? o.want.join(', ') : 'no action'}`, async () => {
      const { ctx, page, rec } = await newCtx(o);
      try {
        await coldMap(page);
        await pickMarker(page);
        await openPopup(page, o.touch);
        await page.waitForTimeout(600);
        const ids = await page.$$eval('.leaflet-popup .map-popup [data-testid]', es => es.map(e => e.getAttribute('data-testid')));
        assert(JSON.stringify(ids) === JSON.stringify(o.want), 'actions: ' + JSON.stringify(ids));
        const links = await popupLinks(page);
        assert(links.length === 2 + o.want.length && /View Node/.test(links[0]) && /Show Neighbors/.test(links[1]), 'links: ' + JSON.stringify(links));
        if (!o.want.length) {
          const shown = await page.evaluate(() => {
            const e = document.querySelector('.leaflet-popup .map-popup-tools');
            return e ? getComputedStyle(e).display : 'none';
          });
          assert(shown === 'none', 'an empty action row takes room: ' + shown);
          await shot(page, '959-popup-off-desktop.jpg');
        }
        clean(rec);
      } finally { await ctx.close(); }
    });
  }

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
