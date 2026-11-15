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
 *   9. phones, real taps from a cold start: 375x560 portrait and 640x360
 *      landscape — the dialog sits above the bottom nav and inside the
 *      viewport, its controls are hit by themselves, and the profile panel
 *      keeps its layout;
 *  10. a node name is text, never markup.
 *
 * SHOTS_DIR=dir also writes screenshots for the PR.
 *
 * Usage:
 *   BASE_URL=http://localhost:13581 node test-issue-737-skraafoto-viewer-e2e.js
 */
'use strict';
const path = require('path');
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const SHOTS = process.env.SHOTS_DIR || '';
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
  maxPx: 1024, note: PRIVACY, credit: CREDIT,
};
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
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

function profileFor(q) {
  const end = (side) => {
    const ll = (q.get(side) || '').split(',').map(Number);
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
    return {
      id, collection: c, direction: d, datetime: year + '-05-07T10:21:33Z',
      viewerUrl: 'https://skraafoto.dataforsyningen.dk/?item=' + id,
      previewUrl: PREVIEW_PREFIX + c + '/' + id,
    };
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
  const JPEG = await (async () => {
    const page = await browser.newPage();
    const out = await page.evaluate((dirs) => {
      const res = {};
      dirs.forEach((d, i) => {
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
   * o.nodeName: { pk, name } rewrites that node's name. */
  async function newCtx(o) {
    o = o || {};
    const ctx = await browser.newContext({
      viewport: o.viewport || { width: 1280, height: 860 },
      hasTouch: !!o.touch, isMobile: !!o.touch,
    });
    const rec = { search: [], preview: [], external: [], errors: [], release: null };
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
        body: JSON.stringify(searchAnswer(u.searchParams, { empty: mode === 'empty', missing: o.missing })) });
    });
    await ctx.route(url => new URL(url).pathname.startsWith(PREVIEW_PREFIX), async (route) => {
      const p = new URL(route.request().url()).pathname;
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
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(profileFor(u.searchParams)) });
    });
    if (o.nodeName) {
      await ctx.route(url => new URL(url).pathname === '/api/nodes/' + o.nodeName.pk, async (route) => {
        const response = await route.fetch();
        const body = await response.json();
        if (body && body.node) body.node.name = o.nodeName.name; else if (body) body.name = o.nodeName.name;
        await route.fulfill({ response, json: body });
      });
    }
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
    await ctx.addInitScript(() => {
      try {
        localStorage.setItem('meshcore-theme', 'light');
        localStorage.setItem('meshcore-map-clustering', 'false');
        ['row-swipe', 'tab-swipe', 'edge-drawer', 'pull-refresh']
          .forEach(k => localStorage.setItem('meshcore-gesture-hints-' + k, 'seen'));
      } catch (_) {}
    });
    const page = await ctx.newPage();
    page.on('pageerror', e => rec.errors.push(String(e)));
    return { ctx, page, rec };
  }

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
  const LAT5 = Number(nodes.fixed.lat).toFixed(5), LON5 = Number(nodes.fixed.lon).toFixed(5);

  async function gotoNode(page, pk, extra) {
    await page.goto(`${BASE}/#/nodes/${pk}${extra || ''}`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('.node-full-card', { timeout: 30000 });
    await page.waitForFunction(() => window.MeshConfigReady !== undefined, null, { timeout: 15000 });
    await page.evaluate(() => window.MeshConfigReady);
  }
  async function gotoProfile(page) {
    await page.goto(`${BASE}/#/map?profile=${P}`, { waitUntil: 'domcontentloaded' });
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
    await step('node page: the action is a 48 px button', async () => {
      const b = await rect(page, ACTION);
      assert(b.h >= 48 && b.w >= 48, `target ${b.w}x${b.h}`);
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

  // ── 9. Phones: real taps from a cold start ────────────────────────────────
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
  for (const [label, viewport] of [['375x560 portrait', { width: 375, height: 560 }], ['640x360 landscape', { width: 640, height: 360 }]]) {
    {
      const { ctx, page, rec } = await newCtx({ viewport, touch: true });
      await gotoNode(page, PK);
      await page.waitForSelector(ACTION, { timeout: 10000 });
      await page.locator(ACTION).scrollIntoViewIfNeeded();
      await page.tap(ACTION);
      await waitReady(page);
      await step(`${label} node page: the dialog sits above the bottom nav, controls hit by themselves`, async () => {
        await aboveNav(page);
      });
      await step(`${label} node page: tapping a direction and Close work`, async () => {
        await page.tap(DIR('east'));
        await waitReady(page);
        assert(await page.getAttribute(DIR('east'), 'aria-pressed') === 'true', 'east not selected');
        await page.locator(FULL).scrollIntoViewIfNeeded();
        assert(await hitsItself(page, FULL), 'Full resolution covered');
        await page.tap(CLOSE);
        await page.waitForSelector(DIALOG, { state: 'detached', timeout: 3000 });
        assert(await active(page) === 'node-skraafoto', 'focus on ' + await active(page));
        assert(rec.errors.length === 0, rec.errors.join(' | '));
      });
      await shot(page, '737-node-' + viewport.width);
      await ctx.close();
    }
    {
      const { ctx, page, rec } = await newCtx({ viewport, touch: true });
      await gotoProfile(page);
      await page.waitForSelector(SF_ROW, { timeout: 5000 });
      const pr = await rect(page, PLOT + ' svg');
      await page.tap(PLOT + ' svg', { position: { x: pr.w * sampleX(50) / VIEW_W, y: pr.h / 2 } });
      await page.waitForFunction((s) => !document.querySelector(s).disabled, SF_POINT, { timeout: 3000 });
      await page.locator(SF_POINT).scrollIntoViewIfNeeded();
      await step(`${label} profile: the row's buttons are 44 px targets, hit by themselves`, async () => {
        for (const sel of [SF_A, SF_B, SF_POINT]) {
          await page.locator(sel).scrollIntoViewIfNeeded();
          const b = await rect(page, sel);
          assert(b.h >= 44 && b.w >= 44, `${sel} ${b.w}x${b.h}`);
          assert(await hitsItself(page, sel), sel + ' covered');
        }
      });
      await page.tap(SF_POINT);
      await waitReady(page);
      await step(`${label} profile: the viewer opens above the profile panel and the bottom nav`, async () => {
        const q = rec.search[0].searchParams;
        const want = await page.evaluate(() => window.TerrainProfileCursor.latLonAt([56.17, 10.12], [56.12, 10.22], 0.5));
        assert(q.get('lat') === want[0].toFixed(5) && q.get('lon') === want[1].toFixed(5), q.toString());
        await aboveNav(page);
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

  // ── 10. A node name is text ───────────────────────────────────────────────
  {
    const evil = '<img src=x onerror="window.__sfXss=1">Mast';
    const { ctx, page } = await newCtx({ nodeName: { pk: PK, name: evil } });
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

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed\n`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
