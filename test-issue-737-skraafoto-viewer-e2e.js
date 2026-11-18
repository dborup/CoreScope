/**
 * E2E (#737 M2): the Skråfoto (oblique aerial photo) viewer on node detail
 * and in the Map page's Path profile.
 *
 * The CI server has no map.skraafoto config and no token, so the
 * `skraafoto` block of /api/config/client is injected in the shape
 * cmd/server/skraafoto_proxy.go sends (SkraafotoClientConfig), and
 * /api/skraafoto/search and /api/skraafoto/preview/{collection}/{item} are
 * answered by stubs in the shape the M1 proxy answers (checked against the
 * Go handler with its fake upstream). The previews are small JPEGs drawn
 * on a canvas here. Every other host is refused and recorded: no real
 * Skråfoto, CDN, Dataforsyningen or token is ever involved.
 *
 * Asserts:
 *   1. gate: with no skraafoto block there is no action anywhere, a deep
 *      link opens nothing and nothing is asked; a node without a fix has
 *      no action; the node list asks nothing;
 *   2. node page: the action opens the dialog — loading state, then the
 *      newest year facing north, five direction buttons, the year
 *      selector newest first without 2017, the photo date, the privacy
 *      note, the credit with its dataset and CC BY 4.0 links, and the
 *      "Full resolution" link to the official viewer;
 *   3. lazy loading: one preview per shown photo, none prefetched, a
 *      photo seen before is not asked for again; a year change searches
 *      that year;
 *   4. Escape and Close return focus to the opening button; Escape does
 *      not also leave the node page or close the Path profile;
 *   5. deep link: the state rides the hash (skraafoto=lat,lon,dir[,year])
 *      and leaves it on close; a cold load of such a link opens it;
 *   6. errors: 429 (with Retry-After), 503 and other failures, of the
 *      search and of a preview, each in its own words; an empty answer;
 *   7. the side pane offers it too, and Escape there closes only the
 *      viewer;
 *   8. Path profile: A, B and a clicked chart point (the #781 mapping) open
 *      the viewer at those coordinates; the panel's head and chart do not
 *      move when the row is on;
 *   9. phones, 375x560 portrait and 640x360 landscape, each a cold start
 *      from `/` (no gesture hint marked as seen) by real taps: More, Nodes,
 *      the node's row, then the action — or "Terrain profile to…", a tap
 *      on the map for B (#790) and a tap on the chart — the dialog sits
 *      above the bottom nav and inside the viewport, its controls are hit
 *      by themselves, the gesture hints that are up do not cover it
 *      (#818), and the profile panel keeps its layout;
 *  10. a node name is text, never markup;
 *  11. a node outside the block's bounds (Denmark) has no action, while
 *      "Terrain profile to…" is still offered (#818);
 *  12. #737 M3c, the marks: the stubbed search carries M3a's frame,
 *      point, inFrame, approx and, when asked for a path, path (preview
 *      pixels); the stubbed preview answers ?zoom=1&x=&y= with a crop and
 *      M3b's marker and box headers. The crosshair sits on the point (to
 *      1.5 px, by Skraafoto.displayMap on the image box, which letterboxes)
 *      and stays there through a resize and a rotation; it is dashed when
 *      approx, absent out of frame; the zoom button (aria-pressed) fetches
 *      the crop on the first click only and moves the crosshair to the
 *      crop marker; from a profile the search asks for the line, the line
 *      of sight is drawn through the path's points and the first photo is
 *      the side view of A–B; the layer's aria-label says where the point
 *      is; and, on the phones of 9, the crosshair, the line and a tapped
 *      zoom from a cold start;
 *  13. #839: on a cross-border profile A is offered and B, in the
 *      Copernicus fill, is not, and Chart point goes with a point picked
 *      in the fill; a path wholly abroad has no row; an empty answer's
 *      message is centred with no empty Year box; from a profile a dotted
 *      drop joins the crosshair to the line (the stub's beam), overview
 *      and crop, with a note saying the line is at antenna height.
 *
 * The fixture's nodes are in California: the node used here is moved to
 * Aarhus in the stubbed /api/nodes answers (o.abroad keeps it where it is).
 *
 * SHOTS_DIR=dir also writes screenshots for the PR. CPU_THROTTLE=4 slows
 * every page's CPU by that factor (CDP Emulation.setCPUThrottlingRate),
 * to check that no step depends on how fast the page renders.
 *
 * Usage:
 *   BASE_URL=http://localhost:13581 node test-issue-737-skraafoto-viewer-e2e.js
 */
'use strict';
const path = require('path');
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const SHOTS = process.env.SHOTS_DIR || '';
const THROTTLE = Number(process.env.CPU_THROTTLE) || 1;
const PROFILE_PATH = '/api/terrain/profile';
const SEARCH_PATH = '/api/skraafoto/search';
const PREVIEW_PREFIX = '/api/skraafoto/preview/';
const PRIVACY = 'Public aerial photos from Klimadatastyrelsen, shown at reduced size.';
const CREDIT = {
  attribution: 'Klimadatastyrelsen, Skråfotos, CC BY 4.0', owner: 'Klimadatastyrelsen',
  datasetName: 'Skråfotos', datasetUrl: 'https://www.klimadatastyrelsen.dk/kortlaegning/skraafotos',
  licenseName: 'CC BY 4.0', licenseUrl: 'https://creativecommons.org/licenses/by/4.0/',
};
const SF_BLOCK = {
  enabled: true, searchUrl: SEARCH_PATH, previewUrl: '/api/skraafoto/preview/{collection}/{item}',
  maxPx: 1024, note: PRIVACY, credit: CREDIT, bounds: [[54.4265, 7.99125], [57.778, 15.5995]],
};
// Where the node under test is moved to, inside the bounds.
const AARHUS = { lat: 56.15674, lon: 10.21076 };
const DHM_ATTRIBUTION = 'Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel, CC BY 4.0';
const DHM_BLOCK = {
  enabled: true, url: '/api/map/dhm/wms', layer: 'dhm_terraen_skyggekort',
  minZoom: 7, maxZoom: 19, profile: true,
  bounds: [[54.4265, 7.99125], [57.778, 15.5995]],
  attribution: DHM_ATTRIBUTION,
};
const PNG_1X1 = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGM4ceIEAAS0AlkWLoFAAAAAAElFTkSuQmCC',
  'base64');
const DIRS = ['north', 'east', 'south', 'west', 'nadir'];
// #737 M3c: the stubbed photos are 480x320, so is the M3a frame. The
// point, the line through it, and the crop around it (box 120x80 preview
// pixels, cut 4x finer: the marker at the crop's centre).
const FRAME = { width: 480, height: 320 };
const PT = { x: 130.5, y: 210.2 };
const LINE = [{ x: 20, y: 300 }, { x: 130.5, y: 210.2 }, { x: 470, y: 30 }];
const CROP_BOX = '70.5,170.2,190.5,250.2';
// #839: the line's point above the crosshair (the server's beam), on LINE.
const BEAM = { x: 147.5, y: 201.2 };
const CROP_MARKER = '240,160';
const ZOOM = '[data-testid="sf-zoom"]';
const MARKS = '[data-testid="sf-marks"]';
const CROSS = '[data-testid="sf-cross"]';
const LOS = '[data-testid="sf-los"]';
const LOS_NOTE = '[data-testid="sf-los-note"]';
// A north–south profile: its side view looks east (or west).
const P_NS = '56.10000,10.20000,56.20000,10.20000';

const ACTION = '[data-testid="node-skraafoto"]';
const DIALOG = '[data-testid="sf-dialog"]';
const CLOSE = '[data-testid="sf-close"]';
const STATUS = '[data-testid="sf-status"]';
const IMG = '[data-testid="sf-img"]';
const YEAR = '[data-testid="sf-year"]';
const FULL = '[data-testid="sf-full"]';
const DATE = '[data-testid="sf-date"]';
const NOTE = '[data-testid="sf-note"]';
const CREDIT_SEL = '[data-testid="sf-credit"]';
const DIR = (d) => `[data-testid="sf-dir-${d}"]`;
const PANEL = '[data-testid="tp-panel"]';
const PLOT = '[data-testid="tp-chart"] .tp-plot';
const SF_ROW = '[data-testid="tp-sf-row"]';
const SF_A = '[data-testid="tp-sf-a"]';
const SF_B = '[data-testid="tp-sf-b"]';
const SF_POINT = '[data-testid="tp-sf-point"]';

// A diagonal free-point profile near Aarhus: 101 samples over 8.4 km.
const P = '56.17000,10.12000,56.12000,10.22000';
const N = 101, D = 8400;
const VIEW_W = 820, PLOT_X = 46, PLOT_W = 758;
const sampleX = (i) => Math.round((PLOT_X + PLOT_W * i / (N - 1)) * 100) / 100;

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); return true; }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); return false; }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

function profileFor(q) {
  // An end is lat,lon, or a node's key (the node entry's A): the node
  // under test is answered at Aarhus, where it is moved to.
  const end = (side) => {
    const v = q.get(side) || '';
    if (/^[0-9a-f]{64}$/i.test(v)) return { lat: AARHUS.lat, lon: AARHUS.lon, kind: 'node', role: 'repeater', groundM: 30, antennaM: 10, antennaSource: 'role-default', assumed: true };
    const ll = v.split(',').map(Number);
    return { lat: ll[0], lon: ll[1], kind: 'point', groundM: 30, antennaM: 10, antennaSource: 'point-default', assumed: true };
  };
  const samples = [];
  for (let i = 0; i < N; i++) samples.push({ d: D * i / (N - 1), dtm: 30 + 10 * Math.sin(i / 9), dsm: 34 + 10 * Math.sin(i / 9) });
  return {
    a: end('a'), b: end('b'),
    distanceM: D, stepM: 84, sampleCount: N, model: 'both',
    coverage: { state: 'full', pct: 100, firstGapM: null }, samples,
    physics: { freqMHz: 869.618, freqSource: 'firmware', lambdaM: 0.34474, k: 1.3333333333333333, kSource: 'default', f1MaxM: 13.4 },
    verdict: { state: 'clear', model: 'dtm', criterion: 0.6, worstClearanceM: 12.1, worstAtM: 4200, worstF1Fraction: 1.2 },
    attribution: DHM_ATTRIBUTION,
  };
}

/* The M1 search answer: one photo per direction (o.missing drops some), the
 * year asked for or the newest. The year list carries 2017 on purpose: the
 * server never sends it, and the viewer must not offer it if it did. */
function searchAnswer(q, o) {
  const year = q.get('year') ? Number(q.get('year')) : 2025;
  if (o.empty) return { collection: '', year: 0, years: [], photos: [], credit: CREDIT };
  const c = 'skraafotos' + year;
  const photos = DIRS.filter(d => !(o.missing || []).includes(d)).map((d, i) => {
    const id = year + '_83_36_' + (DIRS.indexOf(d) + 1) + '_0020_0000' + (3045 + i);
    const ph = {
      id, collection: c, direction: d, datetime: year + '-05-07T10:21:33Z',
      viewerUrl: 'https://skraafoto.dataforsyningen.dk/?item=' + id,
      previewUrl: PREVIEW_PREFIX + c + '/' + id,
      frame: FRAME, point: PT, inFrame: !o.outOfFrame, approx: !!o.approx,
    };
    if (q.get('path')) { ph.path = LINE; ph.beam = BEAM; } // M3a: only when a path is asked for; #839 beam
    return ph;
  });
  return { collection: c, year, years: [2025, 2023, 2021, 2019, 2017], photos, credit: CREDIT };
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #737 M2 Skråfoto viewer against ${BASE} ===\n`);

  // One 480x320 JPEG per direction, drawn here: no image fixture, no CDN.
  // 'crop' is the zoomed crop (#737 M3c).
  const JPEG = await (async () => {
    const page = await browser.newPage();
    const out = await page.evaluate((dirs) => {
      const res = {};
      dirs.concat(['crop']).forEach((d, i) => {
        const c = document.createElement('canvas');
        c.width = 480; c.height = 320;
        const g = c.getContext('2d');
        g.fillStyle = 'hsl(' + (i * 70) + ',45%,55%)';
        g.fillRect(0, 0, 480, 320);
        g.fillStyle = 'hsl(0,0%,100%)';
        g.font = 'bold 48px sans-serif';
        g.fillText(d.toUpperCase(), 40, 180);
        res[d] = c.toDataURL('image/jpeg', 0.8).split(',')[1];
      });
      return res;
    }, DIRS);
    await page.close();
    const buf = {};
    Object.keys(out).forEach(k => { buf[k] = Buffer.from(out[k], 'base64'); });
    return buf;
  })();

  /* o.sf false: no skraafoto block. o.search: 'ok' | 429 | 503 | 502 |
   * 'empty'. o.previewFail: { north: 503 } fails those directions.
   * o.holdSearch: the first search waits until rec.release() is called.
   * o.nodeName: name rewrites the moved node's name. o.abroad leaves the
   * moved node at its own (Californian) position. o.firstVisit marks no
   * gesture hint as seen. o.profile(body) rewrites the profile answer
   * (#839). */
  async function newCtx(o) {
    o = o || {};
    const ctx = await browser.newContext({
      viewport: o.viewport || { width: 1280, height: 860 },
      hasTouch: !!o.touch, isMobile: !!o.touch,
    });
    const rec = { search: [], preview: [], zoom: [], profile: [], external: [], errors: [], release: null };
    let hold = o.holdSearch ? new Promise(r => { rec.release = r; }) : null;
    await ctx.route(BASE + '/api/config/client', async (route) => {
      const response = await route.fetch();
      const config = await response.json();
      delete config.skraafoto;
      if (o.sf !== false) config.skraafoto = Object.assign({}, SF_BLOCK);
      config.dhm = Object.assign({}, DHM_BLOCK);
      await route.fulfill({ response, json: config });
    });
    await ctx.route(url => new URL(url).pathname === SEARCH_PATH, async (route) => {
      const u = new URL(route.request().url());
      rec.search.push(u);
      if (hold) { const h = hold; hold = null; await h; }
      const mode = o.search || 'ok';
      if (typeof mode === 'number') {
        const headers = mode === 429 || mode === 503 ? { 'Retry-After': '30' } : {};
        await route.fulfill({ status: mode, headers, contentType: 'application/json', body: JSON.stringify({ error: 'x' }) });
        return;
      }
      await route.fulfill({ status: 200, contentType: 'application/json',
        body: JSON.stringify(searchAnswer(u.searchParams, { empty: mode === 'empty', missing: o.missing, approx: o.approx, outOfFrame: o.outOfFrame })) });
    });
    await ctx.route(url => new URL(url).pathname.startsWith(PREVIEW_PREFIX), async (route) => {
      const pu = new URL(route.request().url());
      const p = pu.pathname;
      if (pu.search) {
        // #737 M3b: the crop around a point, with its marker and box.
        rec.zoom.push(pu);
        await route.fulfill({ status: 200, contentType: 'image/jpeg', body: JPEG.crop,
          headers: { 'Cache-Control': 'public, max-age=604800', 'X-CoreScope-Skraafoto-Marker': CROP_MARKER, 'X-CoreScope-Skraafoto-Crop': CROP_BOX } });
        return;
      }
      rec.preview.push(p);
      const n = Number(p.split('_')[3]); // the direction number in the id
      const dir = DIRS[n - 1];
      const fail = o.previewFail && o.previewFail[dir];
      if (fail) {
        await route.fulfill({ status: fail, headers: { 'Retry-After': '5' }, contentType: 'application/json', body: '{"error":"x"}' });
        return;
      }
      await route.fulfill({ status: 200, contentType: 'image/jpeg', headers: { 'Cache-Control': 'public, max-age=604800' }, body: JPEG[dir] });
    });
    await ctx.route(url => new URL(url).pathname === PROFILE_PATH, async (route) => {
      const u = new URL(route.request().url());
      rec.profile.push(u);
      const body = profileFor(u.searchParams);
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(o.profile ? o.profile(body) : body) });
    });
    // The node under test, in every /api/nodes answer (list, detail).
    await ctx.route(url => /^\/api\/nodes(\/|$)/.test(new URL(url).pathname), async (route) => {
      if (!MOVED_PK) { await route.fallback(); return; }
      const response = await route.fetch();
      let body;
      try { body = await response.json(); } catch (_) { await route.fulfill({ response }); return; }
      (function walk(v) {
        if (!v || typeof v !== 'object') return;
        if (Array.isArray(v)) { v.forEach(walk); return; }
        if (typeof v.public_key === 'string' && v.public_key.toLowerCase() === MOVED_PK) {
          if (!o.abroad) { v.lat = AARHUS.lat; v.lon = AARHUS.lon; }
          if (o.nodeName) v.name = o.nodeName;
        }
        Object.keys(v).forEach(k => walk(v[k]));
      })(body);
      await route.fulfill({ response, json: body });
    });
    await ctx.route(url => new URL(url).pathname === '/api/map/dhm/wms', (route) =>
      route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 }));
    // Every other host: refused and recorded. Map tiles get a blank PNG.
    await ctx.route(/^https?:\/\/(?!localhost|127\.0\.0\.1)/i, async (route) => {
      rec.external.push(route.request().url());
      if (route.request().resourceType() === 'image') {
        await route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 });
      } else {
        await route.abort();
      }
    });
    await ctx.addInitScript(([firstVisit, pinOverview]) => {
      // A page load's last theme-refresh comes 300 ms after the theme
      // config lands (app.js, customize-v2.js init); an earlier one, on
      // data-theme, may come first. See themeSettled.
      window.__sfThemeSettled = false;
      window.addEventListener('theme-refresh', () => {
        if (window._customizerV2 && window._customizerV2.initDone) window.__sfThemeSettled = true;
      });
      try {
        localStorage.setItem('meshcore-theme', 'light');
        localStorage.setItem('meshcore-map-clustering', 'false');
        // #864: the built-in zoom is 'zoomed'. Every suite here but the #864
        // one pins the viewer's own stored choice to the overview-first path.
        if (pinOverview) localStorage.setItem('cs-theme-overrides', JSON.stringify({ skraafoto: { zoom: 'overview' } }));
        if (!firstVisit) {
          ['row-swipe', 'tab-swipe', 'edge-drawer', 'pull-refresh']
            .forEach(k => localStorage.setItem('meshcore-gesture-hints-' + k, 'seen'));
        }
      } catch (_) {}
    }, [!!o.firstVisit, !o.builtinZoom]);
    const page = await ctx.newPage();
    if (THROTTLE > 1) await (await ctx.newCDPSession(page)).send('Emulation.setCPUThrottlingRate', { rate: THROTTLE });
    page.on('pageerror', e => rec.errors.push(String(e)));
    return { ctx, page, rec };
  }

  let MOVED_PK = null;
  const nodes = await (async () => {
    const { ctx, page } = await newCtx({ sf: false });
    await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
    const list = await page.evaluate(async () => {
      const r = await fetch('/api/nodes?limit=2000');
      const d = await r.json();
      return (d.nodes || d).map(n => ({ pk: n.public_key, name: n.name, lat: n.lat, lon: n.lon }));
    });
    await ctx.close();
    const pk = (n) => /^[0-9a-f]{64}$/i.test(n.pk);
    const fixed = list.filter(n => pk(n) && n.name && n.lat != null && n.lon != null && !(n.lat === 0 && n.lon === 0));
    const nofix = list.filter(n => pk(n) && (n.lat == null || n.lon == null || (n.lat === 0 && n.lon === 0)));
    // A 0,0 position is the no-lock sentinel, not a fix (TerrainProfile.hasFix).
    const zero = list.filter(n => pk(n) && n.lat === 0 && n.lon === 0);
    return { fixed: fixed[0], nofix: nofix[0], zero: zero[0] };
  })();
  assert(nodes.fixed && nodes.nofix && nodes.zero, 'the fixture needs a node with a fix, one without and one at 0,0');
  const PK = nodes.fixed.pk.toLowerCase();
  MOVED_PK = PK;
  const LAT5 = AARHUS.lat.toFixed(5), LON5 = AARHUS.lon.toFixed(5);

  /* themeSettled waits for the page load's last theme-refresh. The nodes
   * list, the node page and the map's markers render again on it, and an
   * element found before it may be replaced (#823 review, F1). */
  async function themeSettled(page) {
    await page.waitForFunction(() => window.__sfThemeSettled, null, { timeout: 15000 });
  }
  async function gotoNode(page, pk, extra) {
    await page.goto(`${BASE}/#/nodes/${pk}${extra || ''}`, { waitUntil: 'domcontentloaded' });
    await themeSettled(page);
    await page.waitForSelector('.node-full-card', { timeout: 30000 });
    await page.waitForFunction(() => window.MeshConfigReady !== undefined, null, { timeout: 15000 });
    await page.evaluate(() => window.MeshConfigReady);
  }
  async function gotoProfile(page, profile) {
    await page.goto(`${BASE}/#/map?profile=${profile || P}`, { waitUntil: 'domcontentloaded' });
    await themeSettled(page);
    await page.waitForSelector(PANEL + '.is-open', { timeout: 30000 });
    await page.waitForSelector(PLOT, { timeout: 15000 });
  }
  async function waitReady(page) {
    await page.waitForFunction((sel) => {
      const d = document.querySelector(sel);
      return d && d.getAttribute('data-state') === 'ready';
    }, DIALOG, { timeout: 10000 });
    await page.waitForFunction((sel) => {
      const i = document.querySelector(sel);
      return i && !i.hidden && i.complete && i.naturalWidth > 0;
    }, IMG, { timeout: 10000 });
  }
  const active = (page) => page.evaluate(() => document.activeElement && document.activeElement.getAttribute('data-testid'));
  const hashParam = (page) => page.evaluate(() => new URLSearchParams(location.hash.split('?')[1] || '').get('skraafoto'));
  const rect = (page, sel) => page.$eval(sel, (e) => { const r = e.getBoundingClientRect(); return { x: r.x, y: r.y, w: r.width, h: r.height, b: r.bottom, r: r.right }; });
  /* The element under the centre of sel is sel itself or inside it. */
  async function hitsItself(page, sel) {
    return page.$eval(sel, (e) => {
      const r = e.getBoundingClientRect();
      const t = document.elementFromPoint(r.x + r.width / 2, r.y + r.height / 2);
      return !!t && (t === e || e.contains(t));
    });
  }
  async function shot(page, name) {
    if (SHOTS) await page.screenshot({ path: path.join(SHOTS, name + '.png') });
  }
  const sfRequests = (rec) => rec.external.filter(u => /dataforsyningen|klimadatastyrelsen|skraafoto/i.test(u));

  // ── 1. The gate ───────────────────────────────────────────────────────────
  {
    const { ctx, page, rec } = await newCtx({ sf: false });
    await gotoNode(page, PK, '?skraafoto=56.15674%2C10.21076%2Cnorth');
    await page.waitForTimeout(500);
    await step('gate off: no node action, a deep link opens nothing, nothing asked', async () => {
      assert(await page.$(ACTION) === null, 'action shown without the skraafoto block');
      assert(await page.$(DIALOG) === null, 'dialog opened without the skraafoto block');
      assert(rec.search.length === 0 && rec.preview.length === 0, 'requests: ' + rec.search.length + '/' + rec.preview.length);
    });
    await gotoProfile(page);
    await step('gate off: the Path profile has no oblique photo row', async () => {
      assert(await page.$(SF_ROW) === null, 'row shown without the skraafoto block');
    });
    await ctx.close();
  }
  {
    const { ctx, page, rec } = await newCtx({});
    await gotoNode(page, nodes.nofix.pk.toLowerCase());
    await page.waitForTimeout(500);
    await step('a node without a fix has no action', async () => {
      assert(await page.$(ACTION) === null, 'action on a node without a position');
    });
    await gotoNode(page, nodes.zero.pk.toLowerCase());
    await page.waitForTimeout(500);
    await step('a node at 0,0 (no GPS lock) has no action', async () => {
      assert(await page.$(ACTION) === null, 'action on a node at 0,0');
    });
    await page.goto(`${BASE}/#/nodes`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector(`tr[data-key="${nodes.fixed.pk}"]`, { timeout: 30000 });
    await page.waitForTimeout(500);
    await step('the node list asks nothing of the photo service', async () => {
      assert(rec.search.length === 0 && rec.preview.length === 0, 'requests from a list: ' + rec.search.length);
    });
    await ctx.close();
  }

  // ── 2-5. Node page: the viewer ────────────────────────────────────────────
  {
    const { ctx, page, rec } = await newCtx({ holdSearch: true });
    await gotoNode(page, PK);
    await page.waitForSelector(ACTION, { timeout: 10000 });
    await step('node page: the action is a compact chip button (#920)', async () => {
      const b = await rect(page, ACTION);
      assert(b.h < 44, `desktop chip ${b.w}x${b.h}`);
      assert(await page.$eval(ACTION, e => e.tagName) === 'BUTTON', 'a button: it opens a dialog');
      assert(rec.search.length === 0, 'nothing asked before a click');
    });
    await page.click(ACTION);
    await step('loading: the dialog shows it is looking the photos up', async () => {
      await page.waitForSelector(DIALOG, { timeout: 5000 });
      const st = await page.$eval(DIALOG, e => e.getAttribute('data-state'));
      assert(st === 'loading', 'state ' + st);
      assert(/looking up/i.test(await page.textContent(STATUS)), await page.textContent(STATUS));
      assert(await page.$eval(DIALOG, e => e.getAttribute('role') === 'dialog' && e.getAttribute('aria-modal') === 'true'), 'role=dialog aria-modal');
      rec.release();
    });
    await waitReady(page);
    await step('the newest year, facing north, from the node position', async () => {
      assert(rec.search.length === 1, 'searches ' + rec.search.length);
      const q = rec.search[0].searchParams;
      assert(q.get('lat') === LAT5 && q.get('lon') === LON5 && !q.has('year'), rec.search[0].search);
      assert(await page.getAttribute(DIR('north'), 'aria-pressed') === 'true', 'north not selected');
      assert(rec.preview.length === 1 && /skraafotos2025\/2025_83_36_1_/.test(rec.preview[0]), 'preview ' + rec.preview);
    });
    await step('five directions: N, E, S, W and nadir', async () => {
      for (const d of DIRS) assert(await page.$(DIR(d)), 'no ' + d + ' button');
      assert((await page.$$('[data-testid^="sf-dir-"]')).length === 5, 'not five');
    });
    await step('year selector: newest first, 2017 left out', async () => {
      const years = await page.$$eval(YEAR + ' option', os => os.map(o => o.value));
      assert(JSON.stringify(years) === '["2025","2023","2021","2019"]', years.join(','));
      assert(await page.$eval(YEAR, e => e.value) === '2025', 'selected');
    });
    await step('the photo date, the privacy note, the credit and Full resolution', async () => {
      assert(/2025-05-07/.test(await page.textContent(DATE)), await page.textContent(DATE));
      assert((await page.textContent(NOTE)).trim() === PRIVACY, await page.textContent(NOTE));
      assert((await page.textContent(CREDIT_SEL)).trim() === CREDIT.attribution, await page.textContent(CREDIT_SEL));
      const links = await page.$$eval(CREDIT_SEL + ' a', as => as.map(a => [a.textContent, a.href, a.target, a.rel]));
      assert(links.length === 2, JSON.stringify(links));
      assert(links[0][0] === 'Skråfotos' && links[0][1] === CREDIT.datasetUrl && links[0][2] === '_blank' && /noopener/.test(links[0][3]), JSON.stringify(links[0]));
      assert(links[1][0] === 'CC BY 4.0' && links[1][1] === CREDIT.licenseUrl && /license/.test(links[1][3]), JSON.stringify(links[1]));
      const full = await page.$eval(FULL, a => [a.textContent, a.getAttribute('href'), a.target, a.rel]);
      assert(/full resolution/i.test(full[0]) && /^https:\/\/skraafoto\.dataforsyningen\.dk\/\?item=2025_83_36_1_/.test(full[1]), JSON.stringify(full));
      assert(full[2] === '_blank' && /noopener/.test(full[3]), JSON.stringify(full));
    });
    await step('deep link: the state is in the hash', async () => {
      assert(await hashParam(page) === `${LAT5},${LON5},north,2025`, await hashParam(page));
      assert(page.url().includes('#/nodes/' + PK), page.url());
    });
    await shot(page, '737-node-desktop');
    await page.click(DIR('east'));
    await waitReady(page);
    await step('lazy: east is asked for only when chosen; north is not asked again', async () => {
      assert(rec.preview.length === 2 && /_2_/.test(rec.preview[1]), rec.preview.join(' '));
      await page.click(DIR('north'));
      await waitReady(page);
      assert(rec.preview.length === 2, 'north fetched twice: ' + rec.preview.join(' '));
      assert(await hashParam(page) === `${LAT5},${LON5},north,2025`, await hashParam(page));
    });
    await page.click(DIR('west'));
    await waitReady(page);
    await page.selectOption(YEAR, '2021');
    await waitReady(page);
    await step('year 2021: searched once more, the direction kept, the date and the link follow', async () => {
      assert(rec.search.length === 2 && rec.search[1].searchParams.get('year') === '2021', rec.search.map(u => u.search).join(' '));
      assert(await page.getAttribute(DIR('west'), 'aria-pressed') === 'true', 'direction not kept');
      assert(/skraafotos2021\/2021_83_36_4_/.test(rec.preview[rec.preview.length - 1]), rec.preview.join(' '));
      assert(/2021-05-07/.test(await page.textContent(DATE)), await page.textContent(DATE));
      assert(/item=2021_83_36_4_/.test(await page.getAttribute(FULL, 'href')), 'full link');
      assert(await hashParam(page) === `${LAT5},${LON5},west,2021`, await hashParam(page));
    });
    await step('no image prefetch: one preview per photo shown', async () => {
      assert(rec.preview.length === 4, rec.preview.join(' '));
      assert(new Set(rec.preview).size === rec.preview.length, 'a preview asked twice');
    });
    await step('Escape closes, focus goes back to the action, the node page stays', async () => {
      await page.keyboard.press('Escape');
      await page.waitForSelector(DIALOG, { state: 'detached', timeout: 3000 });
      assert(await active(page) === 'node-skraafoto', 'focus on ' + await active(page));
      await page.waitForTimeout(300);
      assert(page.url().includes('#/nodes/' + PK), 'Escape left the node page: ' + page.url());
      assert(await hashParam(page) === null, 'hash keeps ' + await hashParam(page));
    });
    await step('Close closes, focus goes back to the action', async () => {
      await page.click(ACTION);
      await waitReady(page);
      await page.click(CLOSE);
      await page.waitForSelector(DIALOG, { state: 'detached', timeout: 3000 });
      assert(await active(page) === 'node-skraafoto', 'focus on ' + await active(page));
    });
    await step('Tab stays inside the dialog', async () => {
      await page.click(ACTION);
      await waitReady(page);
      for (let i = 0; i < 14; i++) {
        await page.keyboard.press('Tab');
        const inside = await page.evaluate((sel) => document.querySelector(sel).contains(document.activeElement), DIALOG);
        assert(inside, 'focus left the dialog after ' + (i + 1) + ' Tab(s)');
      }
      await page.keyboard.press('Escape');
    });
    await step('nothing outside this server was asked; no page errors', async () => {
      assert(sfRequests(rec).length === 0, sfRequests(rec).join(' '));
      assert(rec.errors.length === 0, rec.errors.join(' | '));
    });
    await ctx.close();
  }

  // ── 5b. A cold deep link ──────────────────────────────────────────────────
  {
    const { ctx, page, rec } = await newCtx({});
    await gotoNode(page, PK, '?skraafoto=56.15674%2C10.21076%2Cwest%2C2023');
    await waitReady(page);
    await step('a cold deep link opens the viewer on that point, direction and year', async () => {
      const q = rec.search[0].searchParams;
      assert(q.get('lat') === '56.15674' && q.get('lon') === '10.21076' && q.get('year') === '2023', rec.search[0].search);
      assert(await page.getAttribute(DIR('west'), 'aria-pressed') === 'true', 'west');
      assert(await page.$eval(YEAR, e => e.value) === '2023', 'year');
      assert(rec.preview.length === 1 && /2023_83_36_4_/.test(rec.preview[0]), rec.preview.join(' '));
    });
    await step('leaving the page closes it', async () => {
      await page.evaluate(() => { location.hash = '#/packets'; });
      await page.waitForSelector(DIALOG, { state: 'detached', timeout: 3000 });
    });
    await ctx.close();
  }

  // ── 6. Errors ─────────────────────────────────────────────────────────────
  for (const [mode, re] of [[429, /too many.*30 s/i], [503, /busy|daily/i], [502, /unavailable/i]]) {
    const { ctx, page, rec } = await newCtx({ search: mode });
    await gotoNode(page, PK);
    await page.waitForSelector(ACTION, { timeout: 10000 });
    await page.click(ACTION);
    await step(`search ${mode}: the error in its own words, nothing else asked`, async () => {
      await page.waitForFunction((sel) => document.querySelector(sel) && document.querySelector(sel).getAttribute('data-state') === 'error', DIALOG, { timeout: 5000 });
      const t = await page.textContent(STATUS);
      assert(re.test(t), t);
      assert(rec.preview.length === 0, 'preview after a failed search');
      assert(await page.isVisible(CLOSE), 'no way out');
    });
    await ctx.close();
  }
  {
    const { ctx, page, rec } = await newCtx({ previewFail: { north: 503 } });
    await gotoNode(page, PK);
    await page.waitForSelector(ACTION, { timeout: 10000 });
    await page.click(ACTION);
    await step('preview 503: the photo area says so, another direction still works', async () => {
      await page.waitForFunction((sel) => document.querySelector(sel) && document.querySelector(sel).getAttribute('data-state') === 'error', DIALOG, { timeout: 5000 });
      assert(/busy|daily/i.test(await page.textContent(STATUS)), await page.textContent(STATUS));
      assert(await page.$eval(IMG, i => i.hidden), 'image shown on error');
      await page.click(DIR('south'));
      await waitReady(page);
      assert(rec.preview.length === 2, rec.preview.join(' '));
    });
    // #833 F2: hidden is an HTML attribute only; the SVG layer must be
    // hidden by the attribute, and lose the last photo's text, on error.
    await step('preview 503 after a shown photo: the marks layer is hidden and says nothing stale', async () => {
      const shown = await page.$eval(MARKS, (e) => ({ attr: e.hasAttribute('hidden'), display: getComputedStyle(e).display, label: e.getAttribute('aria-label') }));
      assert(!shown.attr && shown.display !== 'none' && shown.label, 'south marks not shown ' + JSON.stringify(shown));
      await page.click(DIR('north'));
      await page.waitForFunction((sel) => document.querySelector(sel).getAttribute('data-state') === 'error', DIALOG, { timeout: 5000 });
      const m = await page.$eval(MARKS, (e) => ({ attr: e.hasAttribute('hidden'), display: getComputedStyle(e).display,
        label: e.getAttribute('aria-label'), role: e.getAttribute('role'), kids: e.childElementCount }));
      assert(m.attr && m.display === 'none', 'layer not hidden ' + JSON.stringify(m));
      assert(m.label === null && m.role === null && m.kids === 0, 'stale layer ' + JSON.stringify(m));
    });
    await ctx.close();
  }
  {
    const { ctx, page, rec } = await newCtx({ search: 'empty' });
    await gotoNode(page, PK);
    await page.waitForSelector(ACTION, { timeout: 10000 });
    await page.click(ACTION);
    await step('no photos at the point: said plainly, no preview asked', async () => {
      await page.waitForFunction((sel) => document.querySelector(sel) && document.querySelector(sel).getAttribute('data-state') === 'empty', DIALOG, { timeout: 5000 });
      assert(/no oblique photos/i.test(await page.textContent(STATUS)), await page.textContent(STATUS));
      assert(rec.preview.length === 0, 'preview asked');
    });
    await step('#839 no photos: the message is centred in the stage, with no empty Year box', async () => {
      const g = await page.evaluate(() => {
        const st = document.querySelector('[data-testid="sf-stage"]').getBoundingClientRect();
        const t = document.querySelector('[data-testid="sf-status"]').getBoundingClientRect();
        const y = document.querySelector('[data-testid="sf-year-wrap"]');
        return { off: (t.left + t.right) / 2 - (st.left + st.right) / 2, year: y ? getComputedStyle(y).display : 'absent' };
      });
      assert(Math.abs(g.off) <= 2, 'the message is ' + g.off.toFixed(1) + ' px off the stage centre');
      assert(g.year === 'none', 'the Year field shows with no year: ' + g.year);
    });
    await shot(page, '839-empty');
    await ctx.close();
  }
  {
    const { ctx, page } = await newCtx({ missing: ['south', 'nadir'] });
    await gotoNode(page, PK);
    await page.waitForSelector(ACTION, { timeout: 10000 });
    await page.click(ACTION);
    await waitReady(page);
    await step('a direction without a photo is disabled', async () => {
      assert(await page.$eval(DIR('south'), b => b.disabled), 'south enabled');
      assert(await page.$eval(DIR('nadir'), b => b.disabled), 'nadir enabled');
      assert(!(await page.$eval(DIR('east'), b => b.disabled)), 'east disabled');
    });
    await ctx.close();
  }

  // ── 7. The side pane ──────────────────────────────────────────────────────
  {
    const { ctx, page, rec } = await newCtx({});
    await page.goto(`${BASE}/#/nodes`, { waitUntil: 'domcontentloaded' });
    await themeSettled(page);
    const row = `tr[data-key="${nodes.fixed.pk}"]`;
    await page.waitForSelector(row, { timeout: 30000 });
    await page.click(row + ' td:nth-child(2)');
    const sel = '#nodesRight .node-detail-actions ' + ACTION;
    await page.waitForSelector(sel, { timeout: 10000 });
    await step('side pane: the action opens the viewer; Escape closes only the viewer', async () => {
      await page.click(sel);
      await waitReady(page);
      await page.keyboard.press('Escape');
      await page.waitForSelector(DIALOG, { state: 'detached', timeout: 3000 });
      await page.waitForTimeout(200);
      assert(await page.$(sel), 'the side pane closed too');
      assert(await active(page) === 'node-skraafoto', 'focus on ' + await active(page));
      assert(rec.search.length === 1, 'searches ' + rec.search.length);
    });
    await ctx.close();
  }

  // ── 8. The Path profile ───────────────────────────────────────────────────
  {
    const { ctx, page, rec } = await newCtx({});
    await gotoProfile(page);
    await page.waitForSelector(SF_ROW, { timeout: 5000 });
    await step('profile: A opens the viewer at A; Escape closes it, not the panel', async () => {
      await page.click(SF_A);
      await waitReady(page);
      const q = rec.search[0].searchParams;
      assert(q.get('lat') === '56.17000' && q.get('lon') === '10.12000', rec.search[0].search);
      await page.keyboard.press('Escape');
      await page.waitForSelector(DIALOG, { state: 'detached', timeout: 3000 });
      assert(await page.isVisible(PANEL), 'Escape closed the profile panel');
      assert(await active(page) === 'tp-sf-a', 'focus on ' + await active(page));
      assert(new URLSearchParams((await page.evaluate(() => location.hash)).split('?')[1]).get('profile'), 'profile left the hash');
    });
    await step('profile: B opens the viewer at B; Close returns focus to B', async () => {
      await page.click(SF_B);
      await waitReady(page);
      const q = rec.search[rec.search.length - 1].searchParams;
      assert(q.get('lat') === '56.12000' && q.get('lon') === '10.22000', q.toString());
      await page.click(CLOSE);
      await page.waitForSelector(DIALOG, { state: 'detached', timeout: 3000 });
      assert(await active(page) === 'tp-sf-b', 'focus on ' + await active(page));
    });
    await step('profile: the point button waits for a click on the chart', async () => {
      assert(await page.$eval(SF_POINT, b => b.disabled), 'enabled before a click');
    });
    await step('profile: a clicked chart point is the #781 point on the path', async () => {
      const pr = await rect(page, PLOT + ' svg');
      await page.mouse.click(pr.x + pr.w * sampleX(25) / VIEW_W, pr.y + pr.h / 2);
      await page.waitForFunction((s) => !document.querySelector(s).disabled, SF_POINT, { timeout: 3000 });
      assert(/2\.10 km/.test(await page.textContent(SF_POINT)), await page.textContent(SF_POINT));
      await page.click(SF_POINT);
      await waitReady(page);
      const want = await page.evaluate(() => window.TerrainProfileCursor.latLonAt([56.17, 10.12], [56.12, 10.22], 0.25));
      const q = rec.search[rec.search.length - 1].searchParams;
      assert(q.get('lat') === want[0].toFixed(5) && q.get('lon') === want[1].toFixed(5), q.toString() + ' want ' + want);
      await page.keyboard.press('Escape');
      await page.waitForSelector(DIALOG, { state: 'detached', timeout: 3000 });
      assert(await active(page) === 'tp-sf-point', 'focus on ' + await active(page));
    });
    await step('profile: Enter on a keyboard cursor picks that sample', async () => {
      await page.focus(PLOT);
      for (let i = 0; i < 11; i++) await page.keyboard.press('ArrowRight');
      await page.waitForTimeout(100);
      await page.keyboard.press('Enter');
      await page.waitForFunction((s) => /840 m/.test(document.querySelector(s).textContent), SF_POINT, { timeout: 3000 });
    });
    await shot(page, '737-profile-desktop');
    await ctx.close();
  }
  {
    // The row does not move the panel's head or chart (#784, #790).
    const boxes = {};
    for (const sf of [false, true]) {
      const { ctx, page } = await newCtx({ sf, viewport: { width: 640, height: 360 }, touch: true });
      await gotoProfile(page);
      await page.waitForTimeout(300);
      boxes[sf] = { head: await rect(page, PANEL + ' .tp-head'), chart: await rect(page, '[data-testid="tp-chart"]'), panel: await rect(page, PANEL) };
      await ctx.close();
    }
    await step('640x360: the row leaves the panel head, the chart and the panel box where they were', async () => {
      const same = (a, b) => ['x', 'y', 'w', 'h'].every(k => Math.abs(a[k] - b[k]) < 0.5);
      assert(same(boxes[true].head, boxes[false].head), 'head ' + JSON.stringify(boxes));
      assert(same(boxes[true].chart, boxes[false].chart), 'chart ' + JSON.stringify(boxes));
      assert(Math.abs(boxes[true].panel.y - boxes[false].panel.y) < 0.5, 'panel ' + JSON.stringify(boxes));
    });
  }


  // ── 12. #737 M3c: the marks ───────────────────────────────────────────────
  /* crossAt is where the crosshair is drawn on screen, and where it should
   * be: the image box plus the contain mapping (Skraafoto.displayMap) of
   * the point in the layer's space, the photo's natural size. */
  async function crossAt(page) {
    return page.evaluate((s) => {
      const img = document.querySelector(s.IMG), c = document.querySelector(s.CROSS);
      if (!img || !c) return null;
      const r = img.getBoundingClientRect(), b = c.getBoundingClientRect();
      const m = window.Skraafoto.displayMap({ width: img.naturalWidth, height: img.naturalHeight }, { width: r.width, height: r.height });
      const p = window.Skraafoto.toDisplay({ x: Number(c.getAttribute('data-x')), y: Number(c.getAttribute('data-y')) }, m);
      return { x: b.x + b.width / 2, y: b.y + b.height / 2, wantX: r.x + p.x, wantY: r.y + p.y,
        data: [Number(c.getAttribute('data-x')), Number(c.getAttribute('data-y'))],
        img: { x: r.x, y: r.y, w: r.width, h: r.height }, arm: b.width,
        dash: getComputedStyle(c.querySelector('.sf-cross-line')).strokeDasharray };
    }, { IMG, CROSS });
  }
  async function crossOnPoint(page, want, label) {
    const c = await crossAt(page);
    assert(c, (label || '') + ' no crosshair');
    assert(c.data[0] === want.x && c.data[1] === want.y, (label || '') + ' crosshair at ' + c.data + ', want ' + JSON.stringify(want));
    assert(Math.abs(c.x - c.wantX) <= 1.5 && Math.abs(c.y - c.wantY) <= 1.5, (label || '') + ' drawn at ' + JSON.stringify(c));
    assert(c.x >= c.img.x && c.x <= c.img.x + c.img.w && c.y >= c.img.y && c.y <= c.img.y + c.img.h, (label || '') + ' off the image ' + JSON.stringify(c));
    return c;
  }
  /* losPoints is the drawn line's points on screen and where the stub's
   * path points should be. */
  async function losOnPath(page, path) {
    const r = await page.evaluate((s) => {
      const img = document.querySelector(s.IMG), l = document.querySelector(s.LOS);
      if (!l) return null;
      const box = img.getBoundingClientRect();
      const ctm = l.getScreenCTM();
      const pts = Array.from(l.points).map((p) => ({ x: ctm.a * p.x + ctm.c * p.y + ctm.e, y: ctm.b * p.x + ctm.d * p.y + ctm.f }));
      const m = window.Skraafoto.displayMap({ width: img.naturalWidth, height: img.naturalHeight }, { width: box.width, height: box.height });
      const want = s.path.map((p) => { const q = window.Skraafoto.toDisplay(p, m); return { x: box.x + q.x, y: box.y + q.y }; });
      const layer = document.querySelector(s.MARKS);
      return { pts, want, visible: getComputedStyle(l).visibility !== 'hidden' && !layer.hasAttribute('hidden') && getComputedStyle(layer).display !== 'none' };
    }, { IMG, LOS, MARKS, path });
    assert(r && r.visible, 'no line of sight drawn');
    assert(r.pts.length === path.length, 'points ' + r.pts.length);
    r.pts.forEach((p, i) => assert(Math.abs(p.x - r.want[i].x) <= 1.5 && Math.abs(p.y - r.want[i].y) <= 1.5,
      'line point ' + i + ' at ' + JSON.stringify(p) + ', want ' + JSON.stringify(r.want[i])));
  }
  /* noSpill (#833 F1) proves nothing of the marks is painted outside the
   * photo: the letterbox bars (the image box minus its contain content
   * rect) look the same with the marks shown and with them hidden.
   * needBars: the viewport must letterbox, or the check says nothing. */
  async function noSpill(page, needBars, label) {
    const g = await page.evaluate((s) => {
      const img = document.querySelector(s.IMG), b = img.getBoundingClientRect();
      const m = window.Skraafoto.displayMap({ width: img.naturalWidth, height: img.naturalHeight }, { width: b.width, height: b.height });
      const c = { x: b.x + m.x, y: b.y + m.y, w: img.naturalWidth * m.scale, h: img.naturalHeight * m.scale };
      return { b: { x: b.x, y: b.y, w: b.width, h: b.height }, c };
    }, { IMG });
    const bars = [];
    const left = g.c.x - g.b.x, right = g.b.x + g.b.w - (g.c.x + g.c.w);
    const top = g.c.y - g.b.y, bottom = g.b.y + g.b.h - (g.c.y + g.c.h);
    if (left >= 4) bars.push({ x: g.b.x, y: g.b.y, width: left - 1, height: g.b.h });
    if (right >= 4) bars.push({ x: g.c.x + g.c.w + 1, y: g.b.y, width: right - 1, height: g.b.h });
    if (top >= 4) bars.push({ x: g.b.x, y: g.b.y, width: g.b.w, height: top - 1 });
    if (bottom >= 4) bars.push({ x: g.b.x, y: g.c.y + g.c.h + 1, width: g.b.w, height: bottom - 1 });
    if (needBars) assert(bars.length > 0, (label || '') + ' no letterbox to check ' + JSON.stringify(g));
    for (const clip of bars) {
      const on = await page.screenshot({ clip });
      await page.$eval(MARKS, (e) => { e.style.visibility = 'hidden'; });
      const off = await page.screenshot({ clip });
      await page.$eval(MARKS, (e) => { e.style.visibility = ''; });
      assert(on.equals(off), (label || '') + ' the marks are painted on the letterbox bar ' + JSON.stringify(clip));
    }
    return bars.length;
  }
  const cropPath = (path) => path.map((p) => ({ x: (p.x - 70.5) * 4, y: (p.y - 170.2) * 4 }));
  const markLabel = (page) => page.getAttribute(MARKS, 'aria-label');
  /* dropOn: the #839 drop runs from the crosshair at `from` to the beam
   * at `to` (photo pixels), drawn on screen where the image puts them. */
  async function dropOn(page, from, to) {
    const r = await page.evaluate((s) => {
      const img = document.querySelector(s.IMG), l = document.querySelector('[data-testid="sf-drop"]');
      if (!l) return null;
      const box = img.getBoundingClientRect();
      const ctm = l.getScreenCTM();
      const at = (x, y) => ({ x: ctm.a * x + ctm.c * y + ctm.e, y: ctm.b * x + ctm.d * y + ctm.f });
      const m = window.Skraafoto.displayMap({ width: img.naturalWidth, height: img.naturalHeight }, { width: box.width, height: box.height });
      const want = [s.from, s.to].map((p) => { const q = window.Skraafoto.toDisplay(p, m); return { x: box.x + q.x, y: box.y + q.y }; });
      return { got: [at(l.x1.baseVal.value, l.y1.baseVal.value), at(l.x2.baseVal.value, l.y2.baseVal.value)], want,
        dash: getComputedStyle(l).strokeDasharray };
    }, { IMG, from, to });
    assert(r, 'no drop line');
    r.got.forEach((p, i) => assert(Math.abs(p.x - r.want[i].x) <= 1.5 && Math.abs(p.y - r.want[i].y) <= 1.5,
      'drop end ' + i + ' at ' + JSON.stringify(p) + ', want ' + JSON.stringify(r.want[i])));
    assert(r.dash && r.dash !== 'none', 'the drop is not dotted: ' + r.dash);
  }

  {
    const { ctx, page, rec } = await newCtx({});
    await gotoNode(page, PK);
    await page.waitForSelector(ACTION, { timeout: 10000 });
    await page.click(ACTION);
    await waitReady(page);
    await step('M3c node: a crosshair on the point, no line, and its text equivalent', async () => {
      await crossOnPoint(page, PT);
      assert(await page.$(LOS) === null, 'a line without a profile');
      assert(await page.getAttribute(MARKS, 'role') === 'img', 'the layer is an image');
      assert(await markLabel(page) === 'Point marked at the centre-left.', await markLabel(page));
      const c = await crossAt(page);
      assert(c.dash === 'none', 'solid when not approximate: ' + c.dash);
      assert(rec.search[0].searchParams.get('path') === null, 'a node asks for no line');
      assert(await page.$eval(MARKS, (e) => getComputedStyle(e).pointerEvents) === 'none', 'the layer takes taps');
    });
    await step('M3c node: Zoom in is a real button, nothing fetched before a click', async () => {
      assert(await page.$eval(ZOOM, (e) => e.tagName === 'BUTTON' && e.type === 'button'), 'not a button');
      assert(await page.getAttribute(ZOOM, 'aria-pressed') === 'false', 'pressed');
      assert((await page.textContent(ZOOM)).trim() === 'Zoom in', await page.textContent(ZOOM));
      assert(rec.zoom.length === 0, 'crop fetched before a click');
    });
    await shot(page, '737-m3c-node-cross');
    await step('M3c node: Zoom in fetches the crop once, the crosshair moves to the crop marker', async () => {
      await page.click(ZOOM);
      await waitReady(page);
      assert(rec.zoom.length === 1, 'crops ' + rec.zoom.length);
      const q = rec.zoom[0].searchParams;
      assert(q.toString() === 'zoom=1&x=130.5&y=210.2', q.toString());
      assert(rec.zoom[0].pathname === rec.preview[0], rec.zoom[0].pathname + ' vs ' + rec.preview[0]);
      assert(await page.getAttribute(ZOOM, 'aria-pressed') === 'true', 'not pressed');
      assert((await page.textContent(ZOOM)).trim() === 'Zoom out', await page.textContent(ZOOM));
      await crossOnPoint(page, { x: 240, y: 160 }, 'zoomed');
      assert(/zoomed in/.test(await page.getAttribute(IMG, 'alt')), await page.getAttribute(IMG, 'alt'));
      assert(await markLabel(page) === 'Point marked at the centre.', await markLabel(page));
    });
    await shot(page, '737-m3c-node-zoom');
    await step('M3c node: Zoom out puts the kept preview back; zoom again uses the kept crop', async () => {
      const previews = rec.preview.length;
      await page.click(ZOOM);
      await waitReady(page);
      await crossOnPoint(page, PT, 'zoomed out');
      await page.click(ZOOM);
      await waitReady(page);
      assert(rec.preview.length === previews && rec.zoom.length === 1, 'fetched again ' + rec.preview.length + '/' + rec.zoom.length);
      await crossOnPoint(page, { x: 240, y: 160 }, 'zoomed again');
    });
    await step('M3c node: another direction turns zoom off and fetches no crop', async () => {
      await page.click(DIR('east'));
      await waitReady(page);
      assert(await page.getAttribute(ZOOM, 'aria-pressed') === 'false', 'still pressed');
      assert(rec.zoom.length === 1, 'crops ' + rec.zoom.length);
      await crossOnPoint(page, PT, 'east');
    });
    await step('M3c node: the crosshair stays on the point through a resize and a rotation', async () => {
      for (const vp of [{ width: 900, height: 520 }, { width: 520, height: 900 }, { width: 1280, height: 860 }]) {
        await page.setViewportSize(vp);
        await page.waitForTimeout(150);
        const c = await crossOnPoint(page, PT, vp.width + 'x' + vp.height);
        // Sized in screen pixels: 2 x 14 px arms, whatever the scale.
        assert(Math.abs(c.arm - 28) <= 2, vp.width + 'x' + vp.height + ' arm ' + c.arm);
      }
    });
    await step('M3c node: no page errors, nothing outside this server asked', async () => {
      assert(rec.errors.length === 0, rec.errors.join(' | '));
      assert(sfRequests(rec).length === 0, sfRequests(rec).join(' '));
    });
    await ctx.close();
  }
  {
    const { ctx, page } = await newCtx({ approx: true });
    await gotoNode(page, PK);
    await page.waitForSelector(ACTION, { timeout: 10000 });
    await page.click(ACTION);
    await waitReady(page);
    await step('M3c approx: the crosshair is dashed and the text says approximate', async () => {
      const c = await crossOnPoint(page, PT);
      assert(c.dash !== 'none' && c.dash !== '', 'not dashed: ' + c.dash);
      assert(await markLabel(page) === 'Point marked at the centre-left, position approximate.', await markLabel(page));
    });
    await shot(page, '737-m3c-approx');
    await ctx.close();
  }
  {
    const { ctx, page, rec } = await newCtx({ outOfFrame: true });
    await gotoNode(page, PK);
    await page.waitForSelector(ACTION, { timeout: 10000 });
    await page.click(ACTION);
    await waitReady(page);
    await step('M3c out of frame: no crosshair, zoom disabled, said in words', async () => {
      assert(await page.$(CROSS) === null, 'a crosshair out of frame');
      assert(await page.$eval(ZOOM, (e) => e.disabled), 'zoom enabled');
      assert(await markLabel(page) === 'The point is outside this photo.', await markLabel(page));
      assert(rec.zoom.length === 0, 'crops ' + rec.zoom.length);
    });
    await ctx.close();
  }
  {
    const { ctx, page, rec } = await newCtx({});
    await gotoProfile(page, P_NS);
    await page.waitForSelector(SF_ROW, { timeout: 5000 });
    await step('M3c profile: the search asks for the line, at the profile\'s antenna heights', async () => {
      await page.click(SF_A);
      await waitReady(page);
      const q = rec.search[0].searchParams;
      assert(q.get('path') === P_NS && q.get('ha') === '10' && q.get('hb') === '10', q.toString());
    });
    await step('M3c profile: a north–south line opens on the side view, looking east', async () => {
      assert(await page.getAttribute(DIR('east'), 'aria-pressed') === 'true', 'east not chosen');
      assert(rec.preview.length === 1 && /_2_/.test(rec.preview[0]), rec.preview.join(' '));
      assert((await hashParam(page)).split(',')[2] === 'east', await hashParam(page));
    });
    await step('M3c profile: the line of sight through the path\'s points, the crosshair on the point', async () => {
      await losOnPath(page, LINE);
      await crossOnPoint(page, PT);
      assert(await markLabel(page) === 'Point marked at the centre-left. Line of sight from A to B drawn on the photo, at antenna height; a dotted line drops from it to the point.', await markLabel(page));
    });
    await step('#839 profile: a dotted drop joins the crosshair to the line, and a note says why', async () => {
      await dropOn(page, PT, BEAM);
      assert(await page.isVisible(LOS_NOTE), 'no note');
      assert(/drawn at antenna height/.test(await page.textContent(LOS_NOTE)), await page.textContent(LOS_NOTE));
    });
    await shot(page, '737-m3c-profile-line');
    await step('M3c profile: zoomed, the line is moved into the crop', async () => {
      await page.click(ZOOM);
      await waitReady(page);
      await losOnPath(page, cropPath(LINE));
      await crossOnPoint(page, { x: 240, y: 160 });
      await dropOn(page, { x: 240, y: 160 }, cropPath([BEAM])[0]);
    });
    await step('M3c profile: zoomed in a short window, the line stays on the photo, off the letterbox', async () => {
      const vp = page.viewportSize();
      await page.setViewportSize({ width: 1280, height: 600 });
      await page.waitForTimeout(200);
      await noSpill(page, true, '1280x600');
      await losOnPath(page, cropPath(LINE));
      await page.setViewportSize(vp);
      await page.waitForTimeout(200);
    });
    await shot(page, '737-m3c-profile-zoom');
    await step('M3c profile: another direction is chosen by hand and kept for the next year', async () => {
      await page.click(DIR('north'));
      await waitReady(page);
      await page.selectOption(YEAR, '2023');
      await waitReady(page);
      assert(await page.getAttribute(DIR('north'), 'aria-pressed') === 'true', 'north not kept');
      assert(await page.getAttribute(ZOOM, 'aria-pressed') === 'false', 'zoom kept over a year change');
      await losOnPath(page, LINE);
      assert(rec.errors.length === 0, rec.errors.join(' | '));
    });
    await ctx.close();
  }

  // ── 9. Phones: a cold start from `/` by real taps ─────────────────────────
  /* tapToNode is a first visit's way to a node: `/`, then More, Nodes and
   * the node's row, each a tap; on a phone the row opens the node page.
   * Opened while the page load's last theme-refresh is pending, the nodes
   * list renders a second time and the row found first is replaced, so
   * the taps start once it has run (themeSettled). The row is tapped
   * through a locator, which scrolls by itself and finds it again.
   * o.dismiss taps "Got it" on each gesture hint as it comes up: on `/`
   * and on the list, which between them show every hint a phone has. */
  async function tapToNode(page, pk, o) {
    o = o || {};
    await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
    await themeSettled(page);
    if (o.dismiss) await dismissHints(page);
    await page.tap('[data-bottom-nav-tab="more"]');
    await page.tap('[data-bottom-nav-more-route="nodes"]');
    const cell = page.locator(`tr[data-key="${pk}"] td:visible`).first();
    await cell.waitFor({ timeout: 30000 });
    if (o.dismiss) await dismissHints(page, 'row-swipe');
    await cell.tap();
    await page.waitForSelector('.node-full-card', { timeout: 30000 });
    await page.waitForSelector(ACTION, { timeout: 10000 });
    if (!o.dismiss) await hintsUp(page);
  }
  /* hintsUp waits for the first visit's gesture hints, shown 800 ms after
   * a route change (gesture-hints.js) and up for 8 s. */
  async function hintsUp(page) {
    await page.waitForSelector('.gesture-hint:not(.gesture-hint-fading)', { timeout: 5000 });
  }
  /* dismissHints waits for the route's hints (or the one named) and taps
   * "Got it" on each, the newest first, as it lies on top: it is then
   * marked as seen and not shown again. They are up within a second of
   * the route change, far from their 8 s fade. */
  async function dismissHints(page, id) {
    await page.waitForSelector(id ? `[data-gesture-hint="${id}"]:not(.gesture-hint-fading)` : '.gesture-hint:not(.gesture-hint-fading)', { timeout: 5000 });
    const ids = await page.$$eval('.gesture-hint', els => els.map(e => e.getAttribute('data-gesture-hint')));
    for (const h of ids.reverse()) await page.tap(`[data-gesture-hint="${h}"] [data-gesture-hint-dismiss]`);
    await page.waitForFunction(() => !document.querySelector('.gesture-hint'), null, { timeout: 3000 });
  }
  /* noHintOver: on a first visit the gesture hints are up; none of them
   * may cover the viewer (#818). Each hint that overlaps the dialog is
   * probed at points of the overlap: what is hit there is the viewer. */
  async function noHintOver(page) {
    const r = await page.evaluate((sel) => {
      const d = document.querySelector(sel).getBoundingClientRect();
      const hints = Array.from(document.querySelectorAll('.gesture-hint'));
      const over = [];
      hints.forEach((h) => {
        const b = h.getBoundingClientRect();
        const x0 = Math.max(b.left, d.left), x1 = Math.min(b.right, d.right);
        const y0 = Math.max(b.top, d.top), y1 = Math.min(b.bottom, d.bottom);
        if (x1 - x0 < 1 || y1 - y0 < 1) return;
        const hits = [];
        for (const fx of [0.2, 0.5, 0.8]) for (const fy of [0.2, 0.5, 0.8]) {
          const t = document.elementFromPoint(x0 + (x1 - x0) * fx, y0 + (y1 - y0) * fy);
          hits.push(!!t && !!t.closest('[data-testid="sf-overlay"]'));
        }
        over.push({ id: h.getAttribute('data-gesture-hint'), hits });
      });
      return { shown: hints.map(h => h.getAttribute('data-gesture-hint')), over };
    }, DIALOG);
    assert(r.over.length > 0, 'no gesture hint over the dialog to test against; shown: ' + r.shown.join(','));
    r.over.forEach((o) => assert(o.hits.every(Boolean), 'the gesture hint "' + o.id + '" covers the viewer: ' + JSON.stringify(o.hits)));
    for (const sel of [CLOSE, DIR('north'), DIR('nadir'), YEAR, IMG]) assert(await hitsItself(page, sel), sel + ' is covered');
  }
  async function aboveNav(page) {
    const nav = await page.$eval('.bottom-nav', e => { const r = e.getBoundingClientRect(); const s = getComputedStyle(e); return { top: r.top, shown: s.display !== 'none' && r.height > 0 }; });
    const d = await rect(page, DIALOG);
    const vh = await page.evaluate(() => window.innerHeight);
    const vw = await page.evaluate(() => window.innerWidth);
    assert(nav.shown, 'the bottom nav is not shown at this width');
    // Above it in z order too: the modal's scrim covers the nav, so a tap
    // there lands on the scrim (aria-modal), not on a nav link.
    const onNav = await page.$eval('.bottom-nav', (e) => {
      const r = e.getBoundingClientRect();
      const t = document.elementFromPoint(r.x + r.width / 2, r.y + r.height / 2);
      return t && t.getAttribute('data-testid');
    });
    assert(onNav === 'sf-overlay', 'the bottom nav is above the viewer: ' + onNav);
    assert(d.b <= nav.top + 0.5, `dialog bottom ${d.b} under the nav top ${nav.top}`);
    assert(d.y >= -0.5 && d.x >= -0.5 && d.r <= vw + 0.5, 'dialog outside the viewport ' + JSON.stringify(d) + ' ' + vw + 'x' + vh);
    for (const sel of [CLOSE, DIR('north'), DIR('nadir'), YEAR]) {
      assert(await hitsItself(page, sel), sel + ' is covered');
      const b = await rect(page, sel);
      assert(b.h >= 44 && b.w >= 44, `${sel} target ${b.w}x${b.h}`);
    }
    const img = await rect(page, IMG);
    assert(img.h >= 80 && img.y >= d.y && img.y < nav.top, 'photo not in view ' + JSON.stringify(img));
    // Over whatever the page has there (the profile panel, z-index 1200).
    assert(await hitsItself(page, IMG), 'the photo is covered');
  }
  /* mapSpot is a point on the visible map, for a tap that picks an end:
   * the map is hit there and around it, not the panel, a control or a
   * marker (by 32 px, beyond the 24 px snap), and not node A's marker. */
  async function mapSpot(page) {
    return page.evaluate(() => {
      const box = document.getElementById('leaflet-map').getBoundingClientRect();
      const marks = Array.from(document.querySelectorAll('.leaflet-marker-icon, .leaflet-interactive')).map(el => el.getBoundingClientRect());
      const BLOCK = '.leaflet-control, .map-controls, .map-controls-toggle, .map-profile-toggle, .map-profile-hint, .tp-panel, .leaflet-marker-icon, .leaflet-interactive';
      for (let y = box.y + 40; y < box.bottom - 20; y += 10) {
        for (let x = box.x + 60; x < box.right - 60; x += 14) {
          if (!marks.every(r => x < r.x - 32 || x > r.right + 32 || y < r.y - 32 || y > r.bottom + 32)) continue;
          const ok = [[0, 0], [-20, 0], [20, 0], [0, -20], [0, 20]].every(([dx, dy]) => {
            const hit = document.elementFromPoint(x + dx, y + dy);
            return hit && hit.closest('.leaflet-container') && !hit.closest(BLOCK);
          });
          if (ok) return { x: Math.round(x), y: Math.round(y) };
        }
      }
      return null;
    });
  }

  /* A first visit to the viewer: the hints that are up must not cover it. */
  async function firstVisitViewer(label, viewport) {
    const { ctx, page, rec } = await newCtx({ viewport, touch: true, firstVisit: true });
    const reached = await step(`${label} first visit: from / to the viewer by taps, no gesture hint marked as seen`, async () => {
      await tapToNode(page, nodes.fixed.pk);
      await page.tap(ACTION);
      await waitReady(page);
      assert(page.url().includes('#/nodes/' + PK), page.url());
      const seen = await page.evaluate(() => Object.keys(localStorage).filter(k => /^meshcore-gesture-hints-/.test(k) && localStorage.getItem(k) === 'seen'));
      assert(seen.length === 0, 'marked as seen: ' + seen.join(','));
    });
    if (!reached) { await ctx.close(); return; }
    await step(`${label} first visit: no gesture hint covers the viewer`, async () => {
      await noHintOver(page);
    });
    await shot(page, '818-first-visit-' + viewport.width);
    await step(`${label} node page: the dialog sits above the bottom nav, controls hit by themselves`, async () => {
      await aboveNav(page);
    });
    await step(`${label} M3c node page: the crosshair on the point; a tap on Zoom in zooms on it`, async () => {
      await crossOnPoint(page, PT);
      await page.locator(ZOOM).scrollIntoViewIfNeeded();
      const z = await rect(page, ZOOM);
      assert(z.h >= 44 && z.w >= 44, `zoom target ${z.w}x${z.h}`);
      assert(await hitsItself(page, ZOOM), 'zoom covered');
      await page.tap(ZOOM);
      await waitReady(page);
      assert(rec.zoom.length === 1 && await page.getAttribute(ZOOM, 'aria-pressed') === 'true', 'crops ' + rec.zoom.length);
      await crossOnPoint(page, { x: 240, y: 160 }, 'zoomed');
      await shot(page, '737-m3c-zoom-' + viewport.width);
      await page.tap(ZOOM);
      await waitReady(page);
      await crossOnPoint(page, PT, 'zoomed out');
      // The phone turned and back: the crosshair follows the photo.
      await page.setViewportSize({ width: viewport.height, height: viewport.width });
      await page.waitForTimeout(200);
      await crossOnPoint(page, PT, 'rotated');
      await page.setViewportSize(viewport);
      await page.waitForTimeout(200);
      await crossOnPoint(page, PT, 'rotated back');
    });
    await step(`${label} node page: tapping a direction and Close work`, async () => {
      await page.tap(DIR('east'));
      await waitReady(page);
      assert(await page.getAttribute(DIR('east'), 'aria-pressed') === 'true', 'east not selected');
      for (const sel of [FULL, NOTE]) {
        await page.locator(sel).scrollIntoViewIfNeeded();
        assert(await hitsItself(page, sel), sel + ' covered');
      }
      await page.tap(CLOSE);
      await page.waitForSelector(DIALOG, { state: 'detached', timeout: 3000 });
      assert(await active(page) === 'node-skraafoto', 'focus on ' + await active(page));
      assert(rec.errors.length === 0, rec.errors.join(' | '));
    });
    await shot(page, '737-node-' + viewport.width);
    await ctx.close();
  }

  /* A cold start to the Path profile, the way #790 lets a phone pick B:
   * the node's "Terrain profile to…", a tap on the visible map for B, then
   * a tap on the chart. The hints are dismissed with "Got it" as they come
   * up: one of them lies over the page's own panel until then (#823
   * review, F4), and the page's layering is not this viewer's. */
  async function coldStartProfile(label, viewport) {
    const { ctx, page, rec } = await newCtx({ viewport, touch: true, firstVisit: true });
    let ends = null;
    const reached = await step(`${label} cold start: from / to a profile by taps, B picked on the map`, async () => {
      await tapToNode(page, nodes.fixed.pk, { dismiss: true });
      await page.tap('[data-testid="node-terrain-profile"]');
      await page.waitForSelector(PANEL + '.is-open[data-phase="pick-b"]', { timeout: 30000 });
      await page.waitForSelector('#leaflet-map[data-loaded="true"]', { timeout: 30000 });
      await page.waitForTimeout(500);
      const spot = await mapSpot(page);
      assert(spot, 'no map left to tap for B: the panel covers it');
      await page.touchscreen.tap(spot.x, spot.y);
      await page.waitForSelector(SF_ROW + ':not([hidden])', { timeout: 10000 });
      await page.waitForSelector(PLOT, { timeout: 15000 });
      assert(rec.profile.length === 1, 'B issued ' + rec.profile.length + ' requests');
      const q = rec.profile[0].searchParams;
      assert(q.get('a') === PK, 'a = ' + q.get('a'));
      const b = (q.get('b') || '').split(',').map(Number);
      assert(b.length === 2 && b.every(Number.isFinite), 'b = ' + q.get('b'));
      ends = { a: [AARHUS.lat, AARHUS.lon], b };
      const seen = await page.evaluate(() => Object.keys(localStorage).filter(k => /^meshcore-gesture-hints-/.test(k) && localStorage.getItem(k) === 'seen'));
      assert(seen.length > 0, 'no hint was shown to dismiss: not a first visit');
      assert(await page.$('.gesture-hint') === null, 'a hint is still up');
    });
    if (!reached) { await ctx.close(); return; }
    const pr = await rect(page, PLOT + ' svg');
    await page.tap(PLOT + ' svg', { position: { x: pr.w * sampleX(50) / VIEW_W, y: pr.h / 2 } });
    await page.waitForFunction((s) => !document.querySelector(s).disabled, SF_POINT, { timeout: 3000 });
    await step(`${label} profile: the row's buttons are 44 px targets, hit by themselves`, async () => {
      for (const sel of [SF_A, SF_B, SF_POINT]) {
        await page.locator(sel).scrollIntoViewIfNeeded();
        const b = await rect(page, sel);
        assert(b.h >= 44 && b.w >= 44, `${sel} ${b.w}x${b.h}`);
        assert(await hitsItself(page, sel), sel + ' covered');
      }
    });
    await step(`${label} profile: B opens the viewer at the tapped point`, async () => {
      await page.tap(SF_B);
      await waitReady(page);
      const q = rec.search[rec.search.length - 1].searchParams;
      assert(q.get('lat') === ends.b[0].toFixed(5) && q.get('lon') === ends.b[1].toFixed(5), q.toString() + ' want ' + ends.b);
      await page.tap(CLOSE);
      await page.waitForSelector(DIALOG, { state: 'detached', timeout: 3000 });
      assert(await active(page) === 'tp-sf-b', 'focus on ' + await active(page));
    });
    await page.tap(SF_POINT);
    await waitReady(page);
    await step(`${label} profile: the viewer opens above the profile panel and the bottom nav`, async () => {
      const q = rec.search[rec.search.length - 1].searchParams;
      const want = await page.evaluate((e) => window.TerrainProfileCursor.latLonAt(e.a, e.b, 0.5), ends);
      assert(q.get('lat') === want[0].toFixed(5) && q.get('lon') === want[1].toFixed(5), q.toString() + ' want ' + want);
      await aboveNav(page);
    });
    await step(`${label} M3c profile: the line and the crosshair drawn, on the side view of A–B`, async () => {
      const q = rec.search[rec.search.length - 1].searchParams;
      const path = (q.get('path') || '').split(',').map(Number);
      assert(path.length === 4 && Math.abs(path[0] - ends.a[0]) < 1e-5 && Math.abs(path[2] - ends.b[0]) < 1e-5, 'path ' + q.get('path'));
      const want = await page.evaluate((p) => window.Skraafoto.perpendicularDirection(
        { a: { lat: p[0], lon: p[1] }, b: { lat: p[2], lon: p[3] } },
        ['north', 'east', 'south', 'west', 'nadir'].map((d) => ({ direction: d }))), path);
      assert(await page.getAttribute(DIR(want), 'aria-pressed') === 'true', 'not the side view ' + want);
      await losOnPath(page, LINE);
      await crossOnPoint(page, PT);
      await page.locator(ZOOM).scrollIntoViewIfNeeded();
      await page.tap(ZOOM);
      await waitReady(page);
      await losOnPath(page, cropPath(LINE));
      await crossOnPoint(page, { x: 240, y: 160 }, 'zoomed');
      // #833 F1: 640x360 letterboxes the photo; the zoomed line runs far
      // outside the crop and must be clipped to the photo.
      await noSpill(page, viewport.width > viewport.height, label + ' zoomed');
      await page.locator(CLOSE).scrollIntoViewIfNeeded();
    });
    await shot(page, '737-profile-' + viewport.width);
    await step(`${label} profile: Close taps back to the point button, the panel is intact`, async () => {
      await page.tap(CLOSE);
      await page.waitForSelector(DIALOG, { state: 'detached', timeout: 3000 });
      assert(await active(page) === 'tp-sf-point', 'focus on ' + await active(page));
      assert(await page.isVisible(PANEL), 'panel gone');
      assert(rec.errors.length === 0, rec.errors.join(' | '));
    });
    await ctx.close();
  }

  for (const [label, viewport] of [['375x560 portrait', { width: 375, height: 560 }], ['640x360 landscape', { width: 640, height: 360 }]]) {
    await firstVisitViewer(label, viewport);
    await coldStartProfile(label, viewport);
  }

  // A tablet: the bottom nav is shown (≤768 px) but the phone padding is not,
  // and the dialog is taller than the room above the nav.
  {
    const { ctx, page } = await newCtx({ viewport: { width: 700, height: 700 }, touch: true });
    await gotoNode(page, PK);
    await page.waitForSelector(ACTION, { timeout: 10000 });
    await page.locator(ACTION).scrollIntoViewIfNeeded();
    await page.tap(ACTION);
    await waitReady(page);
    await step('700x700 tablet node page: the dialog sits above the bottom nav', async () => {
      await aboveNav(page);
    });
    await ctx.close();
  }

  // ── 14. #864: the built-in default opens zoomed in around the point ───────
  for (const [label, o] of [
    ['desktop', {}],
    ['375x560 portrait cold start, real taps', { viewport: { width: 375, height: 560 }, touch: true, firstVisit: true }],
  ]) {
    const { ctx, page, rec } = await newCtx(Object.assign({ builtinZoom: true }, o));
    await gotoNode(page, PK);
    await page.waitForSelector(ACTION, { timeout: 10000 });
    if (o.touch) { await page.locator(ACTION).scrollIntoViewIfNeeded(); await page.tap(ACTION); } else { await page.click(ACTION); }
    await waitReady(page);
    await step('#864 ' + label + ': the first image request is the crop around the point, no overview', async () => {
      assert(rec.zoom.length === 1, 'crops ' + rec.zoom.length);
      assert(rec.zoom[0].searchParams.toString() === 'zoom=1&x=130.5&y=210.2', rec.zoom[0].searchParams.toString());
      assert(rec.preview.length === 0, 'an overview was fetched first: ' + rec.preview.join(' '));
      assert(await page.getAttribute(ZOOM, 'aria-pressed') === 'true', 'Zoom out not shown');
      assert(/zoomed in/.test(await page.getAttribute(IMG, 'alt')), await page.getAttribute(IMG, 'alt'));
    });
    await step('#864 ' + label + ': Zoom out shows the whole photo', async () => {
      if (o.touch) await page.tap(ZOOM); else await page.click(ZOOM);
      await waitReady(page);
      assert(await page.getAttribute(ZOOM, 'aria-pressed') === 'false', 'still pressed');
      assert(rec.preview.length === 1 && rec.zoom.length === 1, 'preview/zoom ' + rec.preview.length + '/' + rec.zoom.length);
      await crossOnPoint(page, PT, 'zoomed out');
    });
    await ctx.close();
  }
  {
    // A viewer's stored overview still wins over the built-in default.
    const { ctx, page, rec } = await newCtx({});
    await gotoNode(page, PK);
    await page.waitForSelector(ACTION, { timeout: 10000 });
    await page.click(ACTION);
    await waitReady(page);
    await step('#864: a stored overview opens on the whole photo', async () => {
      assert(rec.zoom.length === 0 && rec.preview.length === 1, 'zoom/preview ' + rec.zoom.length + '/' + rec.preview.length);
      assert(await page.getAttribute(ZOOM, 'aria-pressed') === 'false', 'pressed');
    });
    await ctx.close();
  }

  // ── 10. A node name is text ───────────────────────────────────────────────
  {
    const evil = '<img src=x onerror="window.__sfXss=1">Mast';
    const { ctx, page } = await newCtx({ nodeName: evil });
    await gotoNode(page, PK);
    await page.waitForSelector(ACTION, { timeout: 10000 });
    await page.click(ACTION);
    await waitReady(page);
    await step('the node name is shown as text, never as markup', async () => {
      const where = await page.textContent('[data-testid="sf-where"]');
      assert(where.includes(evil), 'label: ' + where);
      assert(await page.evaluate(() => window.__sfXss === undefined), 'markup ran');
      assert(await page.$(DIALOG + ' img[src="x"]') === null, 'an <img> from the name');
    });
    await ctx.close();
  }

  // ── 13. #839: the profile's photo buttons only where DHM measured ─────────
  {
    // Cross-border: the second half filled from Copernicus (#738).
    const { ctx, page } = await newCtx({ profile: (b) => Object.assign(b, {
      coverage: { state: 'full', pct: 100, firstGapM: null, mixed: true, fill: [{ fromM: D / 2, toM: D, source: 'copdem-glo30', resolutionM: 30 }] },
      attributionFill: 'Copernicus' }) });
    await gotoProfile(page);
    await page.waitForSelector(SF_ROW, { timeout: 5000 });
    await step('#839 cross-border: A is offered, B (in the fill) is not', async () => {
      assert(await page.isVisible(SF_A), 'A not offered in Denmark');
      assert(!(await page.isVisible(SF_B)), 'B offered abroad');
      assert(await page.isVisible(SF_POINT), 'no Chart point on a path partly in Denmark');
    });
    await step('#839 cross-border: a chart point in the fill takes Chart point away, one in Denmark brings it back', async () => {
      const pr = await rect(page, PLOT + ' svg');
      await page.mouse.click(pr.x + pr.w * sampleX(75) / VIEW_W, pr.y + pr.h / 2);
      await page.waitForFunction((s) => document.querySelector(s).hidden, SF_POINT, { timeout: 3000 });
      await page.mouse.click(pr.x + pr.w * sampleX(25) / VIEW_W, pr.y + pr.h / 2);
      await page.waitForFunction((s) => { const b = document.querySelector(s); return !b.hidden && !b.disabled; }, SF_POINT, { timeout: 3000 });
      assert(await page.isVisible(SF_ROW), 'the row went');
    });
    await shot(page, '839-cross-border');
    await ctx.close();
  }
  {
    // Wholly abroad: no DHM sample, so no samples at all (the server's shape).
    const { ctx, page, rec } = await newCtx({ profile: (b) => Object.assign(b, {
      a: Object.assign(b.a, { groundM: null }), b: Object.assign(b.b, { groundM: null }),
      coverage: { state: 'none', pct: 0, firstGapM: 0 }, samples: [],
      verdict: { state: 'unknown', model: 'dtm', criterion: 0.6, worstClearanceM: null, worstAtM: null, worstF1Fraction: null } }) });
    await page.goto(`${BASE}/#/map?profile=${P}`, { waitUntil: 'domcontentloaded' });
    await themeSettled(page);
    await page.waitForSelector(PANEL + '.is-open [data-testid="tp-status"]', { timeout: 30000 });
    await step('#839 wholly abroad: no oblique photo row, nothing asked', async () => {
      assert(!(await page.isVisible(SF_ROW)), 'the row is offered on a path wholly abroad');
      assert(rec.search.length === 0, 'searches ' + rec.search.length);
    });
    await ctx.close();
  }

  // ── 11. Only inside the coverage ──────────────────────────────────────────
  {
    const { ctx, page, rec } = await newCtx({ abroad: true });
    await gotoNode(page, PK);
    await page.waitForSelector('[data-testid="node-terrain-profile"]', { timeout: 10000 });
    await page.waitForTimeout(300);
    await step('a node outside Denmark has no oblique photo action; the terrain profile is still offered', async () => {
      assert(await page.$(ACTION) === null, 'action on a node at ' + nodes.fixed.lat + ',' + nodes.fixed.lon);
      assert(await page.$('[data-testid="node-terrain-profile"]'), 'no terrain profile action');
      assert(rec.search.length === 0 && rec.preview.length === 0, 'requests ' + rec.search.length);
    });
    await ctx.close();
  }

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed\n`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
