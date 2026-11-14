/**
 * E2E (#724 M3): the Mastedatabasen antenna sites layer on Map and Live.
 *
 * The CI server has no map.masts config, so the layer is switched on the
 * way an operator's config would: /api/config/client is intercepted and
 * given the `masts` block the Go server sends while the proxy is on, and
 * /api/masts is answered by a stub that generates synthetic sites inside
 * the requested cell. Neither the real proxy nor Mastedatabasen is ever
 * contacted; the proxy itself is covered by cmd/server/masts_proxy_*_test.go
 * against a fake WFS.
 *
 * Asserts:
 *   1. not offered without the block: no toggles, no /api/masts request;
 *   2. offered on Map and Live: the two toggles, the sites on and the
 *      interference layer off by default, the credit shown;
 *   3. zoom gate: below zoom 13 no request and the status line says so;
 *   4. at zoom 14: every request is one `cell=E,N` inside the extent, at
 *      most 4 in flight at once, markers drawn on a canvas;
 *   5. the interference toggle: rings drawn, ?mastsInt=1; the sites
 *      toggle off writes ?masts=0; a deep link restores both;
 *   6. clicking a site opens a popup with the source's fields only, the
 *      hostile stub fields as text (no element, no script run);
 *   7. a pan away and back makes no request for a cell already loaded;
 *   8. no console errors.
 *
 * Usage:
 *   BASE_URL=http://localhost:13724 node test-issue-724-masts-layer-e2e.js
 * Optional: SCREENSHOT_DIR=<dir> writes small light/dark screenshots; those
 * load the real base-map tiles (only /api/masts stays a stub). Elsewhere
 * base tiles are stubbed unless REAL_TILES=1.
 */
'use strict';
const { chromium } = require('playwright');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const BASE = process.env.BASE_URL || 'http://localhost:13724';
const SHOT_DIR = process.env.SCREENSHOT_DIR || '';
const REAL_TILES = process.env.REAL_TILES === '1';
const MASTS_PATH = '/api/masts';
const MASTS = {
  enabled: true, url: MASTS_PATH, minZoom: 13, cellSizeM: 2000,
  attribution: 'Antenna positions: Mastedatabasen, Digitaliseringsstyrelsen (CC0)',
  sourceUrl: 'https://mastedatabasen.dk/', cellExtent: [219, 3017, 462, 3209],
};
const SITE_SEL = '.leaflet-control-layers-overlays label[data-overlay-id="masts-sites"]';
const INT_SEL = '.leaflet-control-layers-overlays label[data-overlay-id="masts-interference"]';
// Viborg heath: no fixture nodes in Denmark, so a click hits a site.
const VIEW = { lat: 56.43, lon: 9.40, zoom: 14 };
const PNG_1X1 = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg==', 'base64');
const EVIL = '<img src=x onerror="window.__xss=1">';

// The projection the page uses, to put stub sites inside their cell.
const ML = (() => {
  const ctx = { window: {}, Math, Map, Set, Promise, Object, Array, Number, String, isFinite };
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'public', 'masts-layer.js'), 'utf8'), ctx);
  return ctx.window.MastsLayer;
})();
function fromUTM32(e, n) {
  let lat = 56, lon = 9 + (e - 500000) / 62000;
  for (let i = 0; i < 30; i++) {
    const p = ML.toUTM32(lat, lon), d = 1e-6;
    const pe = ML.toUTM32(lat, lon + d), pn = ML.toUTM32(lat + d, lon);
    const a = (pe.e - p.e) / d, b = (pn.e - p.e) / d, c = (pe.n - p.n) / d, dd = (pn.n - p.n) / d;
    const det = a * dd - b * c, re = e - p.e, rn = n - p.n;
    lon += (dd * re - b * rn) / det;
    lat += (a * rn - c * re) / det;
  }
  return [lat, lon];
}
function stubCell(E, N) {
  const sites = [];
  for (const [fe, fn, interference] of [[0.3, 0.4, true], [0.7, 0.65, false]]) {
    const [lat, lon] = fromUTM32((E + fe) * 2000, (N + fn) * 2000);
    sites.push({
      lat, lon, street: 'Hedevej' + EVIL, houseNumber: String(E % 90 + 1), postcode: '8800', city: 'Viborg',
      stations: ['S' + E + 'N' + N + (interference ? 'A' : 'B')],
      entries: interference
        ? [{ technology: 'LTE', bandMHz: 800, service: 'Mobiltelefoni', commissioned: '2016-04-01', interference: true },
           { technology: '5G-NR', bandMHz: 3500, service: 'Mobiltelefoni' + EVIL, commissioned: '2021-09-01', interference: false }]
        : [{ technology: '5G-NR', bandMHz: 3500, service: 'Mobiltelefoni', commissioned: '2022-01-01', interference: false }],
      interference, likelyMast: false,
    });
  }
  return { cell: { e: E, n: N }, truncated: false, sites };
}

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
  console.log(`\n=== #724 M3 antenna sites layer against ${BASE} ===\n`);

  async function newCtx({ masts = true, theme = 'light', viewport = { width: 1280, height: 800 }, delayMs = 0, realTiles = REAL_TILES } = {}) {
    const ctx = await browser.newContext({ viewport });
    const rec = { masts: [], open: 0, maxOpen: 0, consoleErrors: [] };
    await ctx.route(BASE + '/api/config/client', async (route) => {
      const response = await route.fetch();
      const config = await response.json();
      delete config.masts;
      if (masts) config.masts = MASTS;
      await route.fulfill({ response, json: config });
    });
    await ctx.route((url) => new URL(url).pathname === MASTS_PATH, async (route) => {
      const u = new URL(route.request().url());
      rec.masts.push(u);
      rec.open++; rec.maxOpen = Math.max(rec.maxOpen, rec.open);
      if (delayMs) await new Promise((r) => setTimeout(r, delayMs));
      rec.open--;
      const m = /^(\d+),(\d+)$/.exec(u.searchParams.get('cell') || '');
      if (!m) { await route.fulfill({ status: 400, json: { error: 'invalid masts request' } }); return; }
      await route.fulfill({ status: 200, json: stubCell(Number(m[1]), Number(m[2])) });
    });
    await ctx.route(/^https?:\/\/(?!localhost|127\.0\.0\.1)/i, async (route) => {
      if (realTiles || route.request().resourceType() !== 'image') { await route.continue(); return; }
      await route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 });
    });
    await ctx.addInitScript(({ t }) => {
      try { localStorage.setItem('meshcore-theme', t); } catch (_) {}
      const apply = () => document.documentElement && document.documentElement.setAttribute('data-theme', t);
      apply();
      document.addEventListener('DOMContentLoaded', apply);
      // Record every Leaflet map, so the test can find marker positions.
      let _L;
      Object.defineProperty(window, 'L', {
        configurable: true,
        get() { return _L; },
        set(v) {
          _L = v;
          try { v.Map.addInitHook(function () { (window.__mcMaps = window.__mcMaps || []).push(this); }); } catch (_) {}
        },
      });
    }, { t: theme });
    const page = await ctx.newPage();
    page.on('console', (m) => { if (m.type() === 'error') rec.consoleErrors.push(m.text()); });
    page.on('pageerror', (e) => rec.consoleErrors.push(String(e)));
    return { ctx, page, rec };
  }

  async function gotoMap(page, hash, view) {
    const v = view || VIEW;
    const sep = hash.includes('?') ? '&' : '?';
    await page.goto(`${BASE}/${hash}${sep}lat=${v.lat}&lon=${v.lon}&zoom=${v.zoom}`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('.leaflet-control-layers', { timeout: 20000 });
    await page.waitForFunction(() => window.MeshConfigReady !== undefined, null, { timeout: 10000 });
    await page.evaluate(() => window.MeshConfigReady).catch(() => {});
    await page.waitForTimeout(1200);
  }
  async function checked(page, sel) {
    return page.evaluate((s) => { const i = document.querySelector(s + ' input'); return i ? i.checked : null; }, sel);
  }
  async function setOverlay(page, sel, on) {
    await page.evaluate(({ s, want }) => {
      const i = document.querySelector(s + ' input');
      if (!i) throw new Error('no toggle ' + s);
      if (i.checked !== want) i.click();
    }, { s: sel, want: on });
    await page.waitForTimeout(1000);
  }
  // The map that carries the masts controller (Map or Live).
  async function stats(page) {
    return page.evaluate(() => {
      const m = (window.__mcMaps || []).filter((x) => x._mcMasts).pop();
      if (!m) return null;
      const s = m._mcMasts.stats();
      s.sitesOn = m.hasLayer(m._mcMasts.groups.sites);
      s.intOn = m.hasLayer(m._mcMasts.groups.interference);
      let markers = 0, rings = 0;
      // Drawn markers only: a group keeps its cells while it is off.
      m._mcMasts.groups.sites.eachLayer((g) => g.eachLayer((mk) => { if (m.hasLayer(mk)) markers++; }));
      m._mcMasts.groups.interference.eachLayer((g) => g.eachLayer((mk) => { if (m.hasLayer(mk)) rings++; }));
      s.markers = markers; s.rings = rings;
      s.canvas = !!document.querySelector('.leaflet-overlay-pane canvas');
      return s;
    });
  }
  const hashParams = (page) => page.evaluate(() => {
    const h = location.hash, i = h.indexOf('?');
    return Object.fromEntries(new URLSearchParams(i >= 0 ? h.slice(i + 1) : ''));
  });
  const settle = async (page) => {
    await page.waitForTimeout(400); // past the 250 ms moveend debounce
    for (let i = 0; i < 40; i++) {
      const s = await stats(page);
      if (s && s.pending === 0 && s.inFlight === 0) return s;
      await page.waitForTimeout(150);
    }
    return stats(page);
  };

  for (const pg of [{ name: 'Map', hash: '#/map' }, { name: 'Live', hash: '#/live' }]) {
    await step(`${pg.name}: not offered without the masts block, and no request`, async () => {
      const { ctx, page, rec } = await newCtx({ masts: false });
      await gotoMap(page, pg.hash);
      assert(!(await page.$(SITE_SEL)) && !(await page.$(INT_SEL)), 'toggles shown');
      await page.waitForTimeout(800);
      assert(rec.masts.length === 0, rec.masts.length + ' masts requests');
      await ctx.close();
    });

    await step(`${pg.name}: two toggles, sites on, interference off by default, credit shown`, async () => {
      const { ctx, page, rec } = await newCtx();
      await gotoMap(page, pg.hash);
      const labels = await page.$$eval('.leaflet-control-layers-overlays label', (ls) => ls.map((l) => [l.getAttribute('data-overlay-id'), l.textContent.trim()]));
      const ids = labels.map((l) => l[0]).filter((x) => x && x.startsWith('masts'));
      assert(JSON.stringify(ids) === '["masts-sites","masts-interference"]', 'toggles ' + JSON.stringify(labels));
      assert(labels.some((l) => l[1] === 'Antenna sites (mast, tower or rooftop)'), 'site label');
      assert(labels.some((l) => l[1] === 'Nearby transmitters (interference)'), 'interference label');
      assert((await checked(page, SITE_SEL)) === true, 'sites not on');
      assert((await checked(page, INT_SEL)) === false, 'interference on by default');
      const s = await settle(page);
      assert(s.markers > 0 && s.rings === 0 && s.canvas, 'markers ' + JSON.stringify(s));
      const credit = await page.$$eval('.leaflet-control-attribution', (els) => els.map((e) => e.textContent).join('|'));
      assert(credit.includes(MASTS.attribution), 'credit ' + credit);
      assert(rec.masts.length > 0);
      assert(rec.consoleErrors.length === 0, rec.consoleErrors.join('\n'));
      await ctx.close();
    });
  }

  await step('zoom gate: no request below zoom 13, status line says so', async () => {
    const { ctx, page, rec } = await newCtx();
    await gotoMap(page, '#/map', Object.assign({}, VIEW, { zoom: 12 }));
    await page.waitForTimeout(800);
    assert(rec.masts.length === 0, rec.masts.length + ' requests at zoom 12');
    await page.hover('.leaflet-control-layers');
    const st = await page.$eval('.mc-masts-status', (el) => ({ text: el.textContent, hidden: el.hidden, visible: el.offsetParent !== null }));
    assert(st.text === 'Zoom in to 13+ to see antenna positions' && !st.hidden && st.visible, JSON.stringify(st));
    await ctx.close();
  });

  await step('requests: one valid cell each, at most 4 in flight, every cell once', async () => {
    const { ctx, page, rec } = await newCtx({ delayMs: 150, viewport: { width: 1920, height: 1080 } });
    await gotoMap(page, '#/map', Object.assign({}, VIEW, { zoom: 13 }));
    const s = await settle(page);
    assert(rec.masts.length >= 50, 'only ' + rec.masts.length + ' cells for a z13 1920×1080 view');
    for (const u of rec.masts) {
      assert([...u.searchParams.keys()].join() === 'cell', 'query ' + u.search);
      const [e, n] = u.searchParams.get('cell').split(',').map(Number);
      assert(e >= 219 && e <= 462 && n >= 3017 && n <= 3209, 'cell outside the extent ' + u.search);
    }
    const cells = rec.masts.map((u) => u.searchParams.get('cell'));
    assert(new Set(cells).size === cells.length, 'a cell was requested twice');
    assert(rec.maxOpen <= 4, rec.maxOpen + ' requests in flight at once');
    console.log(`     z13 1920×1080: ${cells.length} cells, max ${rec.maxOpen} in flight, ${s.markers} sites`);
    await ctx.close();
  });

  await step('interference toggle, sites toggle, hash, and the deep link', async () => {
    const { ctx, page } = await newCtx();
    await gotoMap(page, '#/map');
    await settle(page);
    await setOverlay(page, INT_SEL, true);
    let s = await settle(page);
    assert(s.rings > 0 && s.intOn, 'no rings ' + JSON.stringify(s));
    assert((await hashParams(page)).mastsInt === '1', 'no mastsInt=1');
    await setOverlay(page, SITE_SEL, false);
    s = await stats(page);
    assert(!s.sitesOn && s.intOn);
    assert((await hashParams(page)).masts === '0', 'no masts=0');
    const { ctx: c2, page: p2 } = await newCtx();
    await gotoMap(p2, '#/map?masts=0&mastsInt=1');
    assert((await checked(p2, SITE_SEL)) === false && (await checked(p2, INT_SEL)) === true, 'deep link not restored');
    await c2.close();
    await ctx.close();
  });

  async function clickNearestSite(page) {
    const pt = await page.evaluate(() => {
      const m = (window.__mcMaps || []).filter((x) => x._mcMasts).pop();
      const c = m.getCenter();
      let best = null;
      m._mcMasts.groups.sites.eachLayer((g) => g.eachLayer((mk) => {
        const d = m.distance(c, mk.getLatLng());
        if (!best || d < best.d) best = { d, ll: mk.getLatLng() };
      }));
      const p = m.latLngToContainerPoint(best.ll);
      const r = m.getContainer().getBoundingClientRect();
      return { x: r.left + p.x, y: r.top + p.y };
    });
    await page.mouse.click(pt.x, pt.y);
    await page.waitForSelector('.mc-masts-popup .mc-masts-popup-body', { timeout: 5000 });
    await page.waitForTimeout(400); // popup autopan
  }
  async function popupShot(page, file) {
    const box = await page.$eval('.mc-masts-popup', (el) => { const r = el.getBoundingClientRect(); return { x: r.left, y: r.top, width: r.width, height: r.height }; });
    await page.screenshot({ path: file, clip: { x: Math.max(0, box.x - 20), y: Math.max(0, box.y - 20), width: box.width + 40, height: box.height + 50 } });
  }

  await step('popup: source fields only, hostile text inert', async () => {
    const { ctx, page, rec } = await newCtx();
    await gotoMap(page, '#/map');
    await settle(page);
    await clickNearestSite(page);
    const p = await page.$eval('.mc-masts-popup .mc-masts-popup-body', (el) => ({
      text: el.textContent, imgs: el.querySelectorAll('img').length,
      links: Array.from(el.querySelectorAll('a')).map((a) => [a.href, a.target, a.rel]),
      heads: Array.from(el.querySelectorAll('th')).map((t) => t.textContent),
    }));
    assert(p.imgs === 0, 'markup from a field became an element');
    assert(await page.evaluate(() => window.__xss === undefined), 'stub script ran');
    assert(p.text.includes('Hedevej<img src=x onerror="window.__xss=1">'), 'hostile street not shown as text');
    assert(/Station: S\d+N\d+[AB]/.test(p.text), 'station');
    assert(JSON.stringify(p.heads) === '["Technology","Band (MHz)","Service","Commissioned"]', 'columns ' + p.heads);
    assert(p.text.includes('mast, a tower or a rooftop') && p.text.includes('CC0'), 'caveat');
    assert(!/operator|height above|structure type:/i.test(p.text), 'unpublished field');
    assert(p.links.length === 1 && p.links[0][0] === 'https://mastedatabasen.dk/' && p.links[0][1] === '_blank' && /noopener/.test(p.links[0][2]), JSON.stringify(p.links));
    assert(rec.consoleErrors.length === 0, rec.consoleErrors.join('\n'));
    await ctx.close();
  });

  await step('pan away and back: no request for a cell already loaded; markers detached off-view', async () => {
    const { ctx, page, rec } = await newCtx();
    await gotoMap(page, '#/map');
    const s0 = await settle(page);
    const first = rec.masts.length;
    await page.evaluate(() => { const m = window.__mcMaps.filter((x) => x._mcMasts).pop(); m.panBy([3000, 0], { animate: false }); });
    const s1 = await settle(page);
    assert(rec.masts.length > first, 'no request after the pan');
    await page.evaluate(() => { const m = window.__mcMaps.filter((x) => x._mcMasts).pop(); m.panBy([-3000, 0], { animate: false }); });
    const s2 = await settle(page);
    const cells = rec.masts.map((u) => u.searchParams.get('cell'));
    assert(new Set(cells).size === cells.length, 'a loaded cell was requested again');
    assert(s2.attached === s0.attached && s1.built >= s0.built, `attached ${s0.attached} → ${s2.attached}`);
    assert(s2.kept <= 320, 'kept ' + s2.kept);
    await ctx.close();
  });

  if (SHOT_DIR) {
    for (const theme of ['light', 'dark']) {
      await step(`screenshots (${theme}): both toggles on, a popup, the zoom gate`, async () => {
        const { ctx, page } = await newCtx({ theme, viewport: { width: 900, height: 560 }, realTiles: true });
        await gotoMap(page, '#/map?mastsInt=1');
        await settle(page);
        await page.waitForTimeout(1500); // base tiles
        await page.hover('.leaflet-control-layers');
        await page.waitForTimeout(400);
        await page.screenshot({ path: path.join(SHOT_DIR, `masts-layer-${theme}.png`), clip: { x: 0, y: 52, width: 640, height: 500 } });
        await page.mouse.move(450, 450);
        await clickNearestSite(page);
        await popupShot(page, path.join(SHOT_DIR, `masts-popup-${theme}.png`));
        await gotoMap(page, '#/map?mastsInt=1', Object.assign({}, VIEW, { zoom: 12 }));
        await page.hover('.leaflet-control-layers');
        await page.waitForTimeout(400);
        const box = await page.$eval('.leaflet-control-layers', (el) => { const r = el.getBoundingClientRect(); return { x: r.left, y: r.top, width: r.width, height: r.height }; });
        await page.screenshot({ path: path.join(SHOT_DIR, `masts-zoom-gate-${theme}.png`), clip: { x: box.x - 8, y: box.y + box.height - 110, width: box.width + 16, height: 118 } });
        await ctx.close();
      });
    }
  }

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
