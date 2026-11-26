/**
 * E2E (#941 M2): nodes in reach and the model choice in the Predicted
 * reach panel, in a real browser.
 *
 * As in test-issue-941-predicted-reach-e2e.js, the reach is switched on by
 * intercepting /api/config/client (the `coverage` block cmd/server sends,
 * now with `model` and `models`), and /api/terrain/coverage and its raster
 * are a stub in the server's shape (CoverageResponse), here with `reach`:
 * five repeaters and rooms with their path loss, one of them named with
 * markup, and one without terrain. Hidden nodes are in no count (the
 * server leaves them out). Nothing here reaches a real network.
 *
 * Asserts, light and dark:
 *   1. desktop: the class chips count the nodes at the operator's budget
 *      (good 2, marginal 1, weak 1, all 4), the note says one is below
 *      0 dB and one has no terrain, and says nothing of hidden nodes;
 *      the Good chip opens the list (rlist=good in the address bar) with
 *      node-page links, the marked-up name as text; All lists every node
 *      in reach in loss order;
 *   2. moving the power slider recounts and recolours the chips and the
 *      list with no new request of any kind to /api/;
 *   3. the model select asks again once, with model=delta, puts
 *      rmodel=delta in the address bar and names the model; a deep link
 *      with rmodel and rlist restores both;
 *   4. 375×560, 360×640, 320×568 and, landscape, 640×360 and 568×320 with touch, from a cold #/map, with 66 nodes
 *      in Good: real taps on the toggle and the map open the panel, a tap
 *      on a chip opens the list, and with the list open the power slider
 *      is reached by touch swipes on the sheet alone (it sits at most
 *      700 px below the chip) and touch-dragged; that recounts the list
 *      with no request; there is no horizontal scroll; the chip and the
 *      node rows are on screen and on top, a row is one tap target of at
 *      least 44 px, a row given keyboard focus is not left under the
 *      sheet's sticky head, and a tap on a row's margin opens the node;
 *      landscape, with the list open the sheet stands clear of the zoom,
 *      layers and reach buttons, the map-controls gear, the attribution and
 *      the bottom nav (a tap at each one's centre lands on it), and the
 *      list shows at least one full row;
 *      portrait sizes, a 640×480 touch window and a mouse at 640×360 keep
 *      the portrait rule (8 px in, 46% of the map);
 *   5. no console error, and no request leaves localhost.
 *
 * SHOTS_DIR, when set, receives small JPEG screenshots.
 *
 * Usage:
 *   BASE_URL=http://localhost:13581 node test-issue-941-m2-nodes-in-reach-e2e.js
 */
'use strict';
const path = require('path');
const { chromium } = require('playwright');
const { touchSwipe, TOUCH_SLOP_PX } = require('./tests/e2e-touch-swipe');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const SHOTS = process.env.SHOTS_DIR || '';
const COVERAGE_PATH = '/api/terrain/coverage';
const RASTER_PATH = '/api/terrain/coverage/raster';

const RADIO = { txPowerDbm: 22, antennaGainDbi: 3, rxGainDbi: 3, lossDb: 0, sensitivityDbm: -130 };
const COVERAGE_BLOCK = {
  enabled: true, maxRadiusKm: 40, radiusKm: 40, resolutionM: 30, rxHeightM: 2,
  pointAntenna: { m: 2, source: 'point-default' }, radio: RADIO, dhm: false,
  // #962: no environment loss, so these numbers stay the 158 dB budget's.
  environmentLossDb: 0,
  model: 'bullington', models: ['bullington', 'delta'],
};
const AARHUS = { lat: 56.1629, lon: 10.2039 };
const XSS_NAME = '<img src=x onerror="window.__xss=1">Roof';
// Lowest loss first, as cmd/server sorts them. At 158 dB: margins 38, 18,
// 8, 2, −3 → good, good, marginal, weak, none.
const NODES = [
  { pubkey: '1'.repeat(64), name: 'Near mast', role: 'repeater', distanceKm: 2.1, heightM: 10, heightSource: 'role-default', lossDb: 120 },
  { pubkey: 'ab'.repeat(32), name: XSS_NAME, role: 'room', distanceKm: 5.4, heightM: 5, heightSource: 'role-default', lossDb: 140 },
  { pubkey: '3'.repeat(64), name: 'Hill top', role: 'repeater', distanceKm: 9.8, heightM: 31.5, heightSource: 'owner', lossDb: 150 },
  { pubkey: '4'.repeat(64), name: 'Far valley', role: 'repeater', distanceKm: 17.2, heightM: 10, heightSource: 'role-default', lossDb: 156 },
  { pubkey: '5'.repeat(64), name: 'Behind the ridge', role: 'repeater', distanceKm: 23, heightM: 10, heightSource: 'role-default', lossDb: 161 },
];
const REACH = { roles: ['repeater', 'room'], budgetDb: 158, counts: { good: 2, marginal: 1, weak: 1, none: 1 }, nodes: NODES, omitted: 0, noTerrain: 1 };
// The phone's reach: 64 more repeaters between Near mast and the room, at
// 121–133.6 dB, so Good lists 66 nodes at 158 dB, as a busy area does. At
// −10 dBm (126 dB) Good is empty and 27 nodes are in reach, Near mast first.
const PHONE_NODES = NODES.slice(0, 1).concat(Array.from({ length: 64 }, (_, i) => ({
  pubkey: ('e' + i.toString(16).padStart(3, '0')).repeat(16), name: 'Mast ' + (i + 1), role: 'repeater',
  distanceKm: 2.5 + i * 0.1, heightM: 10, heightSource: 'role-default', lossDb: Math.round((121 + i * 0.2) * 10) / 10,
})), NODES.slice(1));
const PHONE_REACH = { roles: ['repeater', 'room'], budgetDb: 158, counts: { good: 66, marginal: 1, weak: 1, none: 1 }, nodes: PHONE_NODES, omitted: 0, noTerrain: 1 };
// The furthest the power slider may sit below the Good chip with the list
// open: about three sheets' worth of swiping at 375×560.
const PHONE_SLIDER_MAX_PX = 700;

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + (e && e.stack || e)); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

function mercY(lat) { return Math.log(Math.tan(Math.PI / 4 + lat * Math.PI / 360)); }
function latOfMercY(y) { return (2 * Math.atan(Math.exp(y)) - Math.PI / 2) * 180 / Math.PI; }

/* A small loss raster: free-space-like growth with distance. */
function rasterFor(c, radiusKm) {
  const W = 200, H = 200;
  const dLat = radiusKm / 111.2 * 1.05, dLon = dLat / Math.cos(c.lat * Math.PI / 180);
  const s = c.lat - dLat, n = c.lat + dLat, w = c.lon - dLon, e = c.lon + dLon;
  const y0 = mercY(n), y1 = mercY(s);
  const bytes = Buffer.alloc(W * H);
  for (let row = 0; row < H; row++) {
    const lat = latOfMercY(y0 + (y1 - y0) * (row + 0.5) / H);
    for (let col = 0; col < W; col++) {
      const lon = w + (e - w) * (col + 0.5) / W;
      const d = Math.hypot((lat - c.lat) * 111.2, (lon - c.lon) * 111.2 * Math.cos(c.lat * Math.PI / 180));
      bytes[row * W + col] = d <= radiusKm ? Math.max(0, Math.min(254, Math.round((116 + 20 * Math.log10(Math.max(d, 0.05)) - 60) / 0.5))) : 255;
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
  console.log(`\n=== #941 M2 nodes in reach against ${BASE} ===\n`);

  async function newCtx(o) {
    o = o || {};
    const ctx = await browser.newContext({
      viewport: o.viewport || { width: 1280, height: 900 },
      hasTouch: !!o.touch, isMobile: !!o.touch,
    });
    const rec = { consoleErrors: [], all: [], api: [], coverage: [], raster: 0 };
    await ctx.route(BASE + '/api/config/client', async (route) => {
      const response = await route.fetch();
      const config = await response.json();
      config.coverage = o.coverage || COVERAGE_BLOCK;
      await route.fulfill({ response, json: config });
    });
    const rasters = new Map();
    await ctx.route(url => new URL(url).pathname === COVERAGE_PATH, async (route) => {
      const u = new URL(route.request().url());
      rec.coverage.push(u.search);
      const at = u.searchParams.get('at');
      const c = { lat: Number(at.split(',')[0]), lon: Number(at.split(',')[1]) };
      const r = Number(u.searchParams.get('r') || 40);
      const model = u.searchParams.get('model') === 'delta' ? 'delta-bullington-p526-v1' : 'bullington-p526-v1';
      const key = (u.search.length.toString(16) + 'b'.repeat(32)).slice(0, 32);
      const ras = rasterFor(c, r);
      rasters.set(key, ras.bytes);
      const body = {
        key, center: { lat: c.lat, lon: c.lon, kind: 'point', groundM: 34.2 },
        params: { radiusKm: r, resolutionM: 30, txHeightM: 2, txHeightSource: 'point-default', txAssumed: true,
          rxHeightM: 2, rxHeightSource: 'default', freqMHz: 869.618, freqSource: 'firmware', k: 4 / 3, kSource: 'default', model },
        raster: { url: RASTER_PATH + '?key=' + key, width: ras.W, height: ras.H, bounds: ras.bounds, encoding: 'loss-u8',
          lossBaseDb: 60, lossStepDb: 0.5, noData: 255, coveredPct: 100, bytes: ras.bytes.length },
        sources: [{ id: 'dhm-dtm', resolutionM: 30, model: 'dtm' }],
        reach: o.reach || REACH,
      };
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(body) });
    });
    await ctx.route(url => new URL(url).pathname === RASTER_PATH, async (route) => {
      rec.raster++;
      const bytes = rasters.get(new URL(route.request().url()).searchParams.get('key'));
      if (!bytes) { await route.fulfill({ status: 404, contentType: 'application/json', body: '{"error":"not found"}' }); return; }
      await route.fulfill({ status: 200, contentType: 'application/octet-stream', body: bytes });
    });
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
    page.on('request', r => {
      rec.all.push(r.url());
      if (/^https?:\/\/(localhost|127\.0\.0\.1)[:/][^/]*\/api\//i.test(r.url())) rec.api.push(r.url());
    });
    return { ctx, page, rec };
  }

  async function waitPanel(page) {
    await page.waitForSelector('[data-testid="reach-panel"]:not([hidden])', { timeout: 20000 });
    await page.waitForFunction(() => {
      const c = document.querySelector('canvas.reach-layer');
      const nir = document.querySelector('[data-testid="reach-nir"]');
      return c && c.width > 0 && nir && !nir.hidden && !document.querySelector('[data-testid="reach-panel"].is-busy');
    }, null, { timeout: 15000 });
    await page.waitForTimeout(150);
  }

  function chips(page) {
    return page.evaluate(() => {
      const out = {};
      ['good', 'marginal', 'weak', 'all'].forEach(c => {
        const b = document.querySelector('[data-testid="reach-nir-' + c + '"]');
        out[c] = b && { n: b.querySelector('.reach-nir-count').textContent, pressed: b.getAttribute('aria-pressed'), hidden: b.hidden };
      });
      return out;
    });
  }

  function listed(page) {
    return page.$$eval('[data-testid="reach-nir-item"]', lis => lis.filter(li => !li.hidden && li.offsetParent !== null).map(li => {
      const a = li.querySelector('a');
      return { cls: li.getAttribute('data-class'), name: a.textContent, href: a.getAttribute('href'),
        margin: li.querySelector('.reach-nir-margin').textContent, swatch: li.querySelector('.reach-swatch').className };
    }));
  }

  /* setPower moves the power slider and waits past the panel's request
   * debounce (500 ms), so a request it wrongly started would be seen. */
  async function setPower(page, v) {
    await page.evaluate((val) => {
      const s = document.querySelector('[data-testid="reach-power"]');
      s.value = String(val);
      s.dispatchEvent(new Event('input', { bubbles: true }));
    }, v);
    await page.waitForTimeout(800);
  }

  function noNetwork(rec) {
    const bad = rec.all.filter(u => !/^https?:\/\/(localhost|127\.0\.0\.1)[:/]/i.test(u) && !u.startsWith('data:') && !u.startsWith('blob:'));
    assert(bad.filter(u => /dataforsyningen|amazonaws|copernicus/i.test(u)).length === 0, 'a real upstream was reached');
    const errs = rec.consoleErrors.filter(e => !/Failed to load resource/.test(e));
    assert(errs.length === 0, 'console errors: ' + errs.join(' | '));
  }

  async function shot(page, name, el) {
    if (!SHOTS) return;
    const target = el ? await page.$(el) : page;
    await target.screenshot({ path: path.join(SHOTS, name), type: 'jpeg', quality: 60 });
  }

  // ── 1–3: desktop ───────────────────────────────────────────────────────
  for (const theme of ['light', 'dark']) {
    await step(`${theme}: counts, the Good list, All, a budget change with no request, the model`, async () => {
      const { ctx, page, rec } = await newCtx({ theme });
      try {
        await page.goto(`${BASE}/#/map?reach=${AARHUS.lat},${AARHUS.lon}&rkm=25`, { waitUntil: 'domcontentloaded' });
        await waitPanel(page);
        let c = await chips(page);
        assert(c.good.n === '2' && c.marginal.n === '1' && c.weak.n === '1' && c.all.n === '4', 'chips ' + JSON.stringify(c));
        assert(['good', 'marginal', 'weak', 'all'].every(k => c[k].pressed === 'false'), 'a chip pressed before a click');
        const note = await page.textContent('[data-testid="reach-nir-note"]');
        assert(/4 of 5 repeaters and rooms/.test(note) && /1 below 0 dB/.test(note) && /1 without terrain data/.test(note) && !/hidden/.test(note), 'note ' + note);
        assert(await page.isHidden('[data-testid="reach-nir-list"]'), 'the list is open before a chip is clicked');

        await page.click('[data-testid="reach-nir-good"]');
        let l = await listed(page);
        assert(l.length === 2 && l[0].name === 'Near mast' && l[1].name === XSS_NAME, 'good list ' + JSON.stringify(l));
        assert(l[0].href === '#/nodes/' + '1'.repeat(64) && l[1].href === '#/nodes/' + 'ab'.repeat(32), 'hrefs');
        assert(l[0].margin === '+38 dB' && l[0].swatch === 'reach-swatch reach-swatch-good', 'item ' + JSON.stringify(l[0]));
        assert(await page.evaluate(() => !document.querySelector('[data-testid="reach-nir-list"] img') && !window.__xss), 'markup in a name became DOM');
        assert((await chips(page)).good.pressed === 'true', 'Good not pressed');
        await page.waitForFunction(() => /[?&]rlist=good/.test(location.hash), null, { timeout: 3000 });
        await page.click('[data-testid="reach-nir-all"]');
        l = await listed(page);
        assert(l.map(x => x.cls).join() === 'good,good,marginal,weak', 'all list ' + JSON.stringify(l.map(x => x.cls)));
        await shot(page, `941-m2-nodes-desktop-${theme}.jpg`);
        await shot(page, `941-m2-panel-${theme}.jpg`, '[data-testid="reach-panel"]');

        // 2: the budget recounts and recolours in place, no request.
        const before = { api: rec.api.length, coverage: rec.coverage.length, raster: rec.raster };
        await setPower(page, 10); // −12 dB: margins 26, 6, −4, −10, −15
        c = await chips(page);
        assert(c.good.n === '1' && c.marginal.n === '1' && c.weak.n === '0' && c.all.n === '2', 'chips at 10 dBm ' + JSON.stringify(c));
        l = await listed(page);
        assert(l.map(x => x.name + ':' + x.cls + ':' + x.margin).join('|') === `Near mast:good:+26 dB|${XSS_NAME}:marginal:+6 dB`, 'list at 10 dBm ' + JSON.stringify(l));
        await setPower(page, 30); // +8 dB: 46, 26, 16, 10, 5
        c = await chips(page);
        assert(c.good.n === '4' && c.weak.n === '0' && c.all.n === '5', 'chips at 30 dBm ' + JSON.stringify(c));
        assert(rec.api.length === before.api && rec.coverage.length === before.coverage && rec.raster === before.raster,
          `a budget change asked the server: ${rec.api.slice(before.api).join(', ')}`);
        assert(/5 of 5/.test(await page.textContent('[data-testid="reach-nir-note"]')), 'note after the budget change');
        await shot(page, `941-m2-nodes-30dbm-${theme}.jpg`, '[data-testid="reach-panel"]');

        // 3: the model asks again, once, and is in the link.
        await page.selectOption('[data-testid="reach-model-select"]', 'delta');
        for (let i = 0; i < 60 && rec.coverage.length === before.coverage; i++) await page.waitForTimeout(50);
        await page.waitForFunction(() => /delta-Bullington/.test(document.querySelector('[data-testid="reach-model"]').textContent), null, { timeout: 5000 });
        assert(rec.coverage.length === before.coverage + 1 && new URLSearchParams(rec.coverage[rec.coverage.length - 1]).get('model') === 'delta',
          'model request ' + rec.coverage.slice(before.coverage).join(' '));
        assert(!/model|rpwr|rlist/.test(rec.coverage[rec.coverage.length - 1].replace('model=delta', '')), 'the request carries the budget or the list');
        await page.waitForFunction(() => /[?&]rmodel=delta/.test(location.hash), null, { timeout: 3000 });
        assert((await chips(page)).all.pressed === 'true', 'the list filter was lost on the new model');
        noNetwork(rec);
      } finally { await ctx.close(); }
    });
  }

  await step('a link with rmodel and rlist restores the model and the open list', async () => {
    const { ctx, page, rec } = await newCtx({});
    try {
      await page.goto(`${BASE}/#/map?reach=${AARHUS.lat},${AARHUS.lon}&rkm=25&rmodel=delta&rlist=marginal`, { waitUntil: 'domcontentloaded' });
      await waitPanel(page);
      assert(rec.coverage.length === 1 && new URLSearchParams(rec.coverage[0]).get('model') === 'delta', 'request ' + rec.coverage);
      assert(await page.inputValue('[data-testid="reach-model-select"]') === 'delta', 'select');
      const l = await listed(page);
      assert(l.length === 1 && l[0].name === 'Hill top' && l[0].cls === 'marginal', 'list ' + JSON.stringify(l));
      noNetwork(rec);
    } finally { await ctx.close(); }
  });

  await step('a config without a model offers delta as the default and sends no model', async () => {
    const noModel = Object.assign({}, COVERAGE_BLOCK);
    delete noModel.model;
    const { ctx, page, rec } = await newCtx({ coverage: noModel });
    try {
      await page.goto(`${BASE}/#/map?reach=${AARHUS.lat},${AARHUS.lon}&rkm=25`, { waitUntil: 'domcontentloaded' });
      await waitPanel(page);
      assert(await page.inputValue('[data-testid="reach-model-select"]') === 'delta', 'select');
      assert(rec.coverage.length === 1 && !new URLSearchParams(rec.coverage[0]).has('model'), 'request ' + rec.coverage);
      noNetwork(rec);
    } finally { await ctx.close(); }
  });

  // ── 4: phone, touch, from a cold #/map ──────────────────────────────────
  /* sheetGrip is where a thumb would push the sheet: the lowest spot of
   * its visible body, below the sticky head, off the node list (a swipe
   * there scrolls the list) and a thumb's width (±16 px) clear of every
   * slider (the browser snaps a touch near one onto it, and a swipe then
   * moves it). The list itself when no such spot shows. */
  function sheetGrip(page) {
    return page.evaluate(() => {
      const sheet = document.querySelector('[data-testid="reach-panel"]');
      const list = document.querySelector('[data-testid="reach-nir-list"]');
      const s = sheet.getBoundingClientRect();
      const head = sheet.querySelector('.reach-head').getBoundingClientRect();
      const hitAt = (px, py) => document.elementFromPoint(px, py);
      const offSliders = (px, py) => { const h = hitAt(px, py); return !h || !h.closest('input[type="range"]'); };
      // Down the middle first, then the label column at the left and the
      // edge at the right, where the fields leave room between them.
      for (const x of [s.left + s.width / 2, s.left + 24, s.right - 12]) {
        for (let y = s.bottom - 18; y > head.bottom + 18; y -= 4) {
          const hit = hitAt(x, y);
          if (!hit || !sheet.contains(hit) || list.contains(hit) || hit.closest('input, select')) continue;
          if ([-16, 0, 16].every(dy => [-16, 0, 16].every(dx => offSliders(x + dx, y + dy)))) return { x, y, onList: false };
        }
      }
      return { x: s.left + s.width / 2, y: (head.bottom + s.bottom) / 2, onList: true };
    });
  }

  /* inView: the element is wholly on screen in the sheet, below its head,
   * and on top at its centre. */
  function inView(page, selector) {
    return page.evaluate((sel) => {
      const el = document.querySelector(sel);
      const sheet = document.querySelector('[data-testid="reach-panel"]');
      const r = el.getBoundingClientRect(), s = sheet.getBoundingClientRect();
      const head = sheet.querySelector('.reach-head').getBoundingClientRect();
      const hit = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2);
      return r.height > 0 && r.top >= head.bottom && r.bottom <= Math.min(s.bottom, innerHeight) && !!hit && (hit === el || el.contains(hit));
    }, selector);
  }

  /* geometry: where the sheet, its head, selector and the list are, for a
   * failure message. */
  function geometry(page, selector) {
    return page.evaluate((sel) => {
      const q = (x) => { const e = typeof x === 'string' ? document.querySelector(x) : x; if (!e) return null; const r = e.getBoundingClientRect(); return [Math.round(r.top), Math.round(r.bottom)]; };
      const sheet = document.querySelector('[data-testid="reach-panel"]');
      return JSON.stringify({ sheet: q(sheet), head: q('.reach-head'), el: q(sel), list: q('[data-testid="reach-nir-list"]'), scrollTop: sheet.scrollTop, scrollH: sheet.scrollHeight });
    }, selector);
  }

  /* touchScrollTo swipes the sheet, by touch only, until selector is in
   * view: up while it is below the head, down while above. The number of
   * swipes. */
  async function touchScrollTo(page, cdp, selector, max) {
    for (let n = 0; n <= max; n++) {
      if (await inView(page, selector)) return n;
      const below = await page.evaluate((sel) => {
        const r = document.querySelector(sel).getBoundingClientRect();
        const head = document.querySelector('[data-testid="reach-panel"] .reach-head').getBoundingClientRect();
        return r.top >= head.bottom;
      }, selector);
      const at = await sheetGrip(page);
      // A swipe on the node list scrolls the list, not the sheet: the trap.
      if (at.onList) throw new Error(`no spot on the sheet off the node list and the sliders to swipe from on the way to ${selector} (the list covers its body) ${await geometry(page, selector)}`);
      const tops = () => page.evaluate(() => ['reach-panel', 'reach-nir-list'].map(id => document.querySelector(`[data-testid="${id}"]`).scrollTop).join());
      const was = await tops();
      // How far the sheet moves: short enough that a body barely taller
      // than the element is not overshot back and forth, and small enough
      // that the first path is at most a thumb's 80 px of finger travel:
      // that distance plus Chromium's touch slop (#967: a 36 px swipe moved
      // the sheet 21 px, and on a slightly taller layout the list ran out of
      // swipes). A correction inside the touch moves the same finger further,
      // by the shortfall, so the whole touch can travel more than 80 px.
      const room = await page.evaluate((sel) => {
        const head = document.querySelector('[data-testid="reach-panel"] .reach-head').getBoundingClientRect();
        const s = document.querySelector('[data-testid="reach-panel"]').getBoundingClientRect();
        return s.bottom - head.bottom - document.querySelector(sel).getBoundingClientRect().height;
      }, selector);
      const dy = Math.max(1, Math.min(80 - TOUCH_SLOP_PX, Math.floor(room - 8)));
      // One swipe a turn (no opts.maxSwipes): touchSwipe makes up a
      // shortfall inside the touch, and a further swipe must land on a grip
      // found again, which the next turn does.
      await touchSwipe(page, cdp, at, below ? dy : -dy, { scrollers: ['[data-testid="reach-panel"]', '[data-testid="reach-nir-list"]'] });
      if (await tops() === was) throw new Error(`a swipe at ${JSON.stringify(at)} scrolled nothing on the way to ${selector}`);
    }
    const geo = await geometry(page, selector);
    throw new Error(`${selector} not in view after ${max} swipes ${geo}`);
  }

  /* dragSlider touch-drags a range input's thumb to value v. */
  async function dragSlider(page, cdp, selector, v) {
    const g = await page.evaluate((sel) => {
      const s = document.querySelector(sel);
      const r = s.getBoundingClientRect();
      return { left: r.left, width: r.width, y: r.top + r.height / 2, min: Number(s.min), max: Number(s.max), value: Number(s.value) };
    }, selector);
    const thumb = 16; // Chromium's thumb, about
    const xOf = val => g.left + thumb / 2 + (g.width - thumb) * (val - g.min) / (g.max - g.min);
    const from = { x: xOf(g.value), y: g.y };
    await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [from] });
    for (let k = 1; k <= 10; k++) {
      await cdp.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [{ x: from.x + (xOf(v) - from.x) * k / 10, y: g.y }] });
      await page.waitForTimeout(16);
    }
    await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
    await page.waitForTimeout(800); // past the request debounce
  }

  /* controlsClear lists what the open sheet gets in the way of: each map
   * control and the bottom nav whose centre does not hit itself, or whose
   * box meets the sheet's; and a sheet that reaches the top edge of the map
   * (it keeps 8 px of map above it). Empty when it is clear of all. */
  function controlsClear(page) {
    return page.evaluate(() => {
      const sheet = document.querySelector('[data-testid="reach-panel"]').getBoundingClientRect();
      const map = document.querySelector('#leaflet-map').getBoundingClientRect();
      const bad = [];
      if (sheet.top < map.top + 8) bad.push('sheet top ' + Math.round(sheet.top) + ' < map top ' + Math.round(map.top) + ' + 8');
      const SEL = ['.leaflet-control-zoom-in', '.leaflet-control-zoom-out', '.leaflet-control-layers-toggle',
        '[data-testid="map-reach-toggle"]', '.map-controls-toggle', '.leaflet-control-attribution a', '.bottom-nav'];
      SEL.forEach((sel) => {
        const e = document.querySelector(sel);
        if (!e) { bad.push(sel + ' missing'); return; }
        const r = e.getBoundingClientRect();
        const hit = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2);
        if (!hit || !(hit === e || e.contains(hit))) bad.push(sel + ' centre hits ' + (hit && (hit.getAttribute('data-testid') || hit.className || hit.tagName)));
        if (r.left < sheet.right && r.right > sheet.left && r.top < sheet.bottom && r.bottom > sheet.top) bad.push(sel + ' overlaps the sheet');
      });
      return bad;
    });
  }

  const PHONES = [
    { theme: 'light', width: 375, height: 560 }, { theme: 'dark', width: 375, height: 560 },
    { theme: 'light', width: 360, height: 640 }, { theme: 'dark', width: 360, height: 640 },
    { theme: 'light', width: 320, height: 568 },
    // Landscape: the sheet's body is short, the list must leave room to swipe it.
    { theme: 'light', width: 640, height: 360 }, { theme: 'dark', width: 640, height: 360 },
    { theme: 'light', width: 568, height: 320 },
  ];
  for (const { theme, width, height } of PHONES) {
    await step(`${theme}: ${width}×${height} touch from a cold #/map: a chip opens the list, the budget is reached by touch and recounts it, a row opens the node`, async () => {
      const { ctx, page, rec } = await newCtx({ theme, viewport: { width, height }, touch: true, reach: PHONE_REACH });
      try {
        await page.goto(`${BASE}/#/map?lat=${AARHUS.lat}&lon=${AARHUS.lon}&zoom=10`, { waitUntil: 'domcontentloaded' });
        await page.waitForSelector('#leaflet-map[data-loaded="true"]', { timeout: 30000 });
        await page.waitForSelector('[data-testid="map-reach-toggle"]', { timeout: 15000 });
        await page.tap('[data-testid="map-reach-toggle"]');
        const box = await page.locator('#leaflet-map').boundingBox();
        await page.touchscreen.tap(box.x + box.width / 2, box.y + box.height / 3);
        await waitPanel(page);
        assert(rec.coverage.length === 1, 'requests ' + rec.coverage.length);
        const cdp = await ctx.newCDPSession(page);
        const GOOD = '[data-testid="reach-nir-good"]';
        await touchScrollTo(page, cdp, GOOD, 8);
        const onTop = await page.evaluate((sel) => {
          const b = document.querySelector(sel);
          const r = b.getBoundingClientRect();
          return { top: r.top, bottom: r.bottom, h: r.height };
        }, GOOD);
        assert(onTop.h >= 44, 'Good chip ' + JSON.stringify(onTop));
        await page.tap(GOOD);
        let l = await listed(page);
        assert(l.length === 66, 'phone list ' + l.length);
        const ROW = '[data-testid="reach-nir-item"]:not([hidden])';
        // The list is a short window of its own, wholly on screen in the sheet.
        await touchScrollTo(page, cdp, '[data-testid="reach-nir-list"]', 8);
        await shot(page, `941-m2-nodes-phone-list-${theme}-${width}x${height}.jpg`);
        // One full row is the least the list shows: 44 px of content, not
        // counting its 1 px top border.
        const listBox = await page.evaluate(() => { const l = document.querySelector('[data-testid="reach-nir-list"]'); return { client: l.clientHeight, scroll: l.scrollHeight }; });
        assert(listBox.client >= 44, 'the list shows less than one row ' + JSON.stringify(listBox));
        // Landscape: the sheet stands clear of every map control and the
        // nav, and a tap at each one's centre lands on it.
        if (width > height) {
          const clear = await controlsClear(page);
          assert(clear.length === 0, 'with the list open, the sheet covers ' + JSON.stringify(clear));
        }

        // With the list open, the power slider is reached by touch on the
        // sheet, and it is not pushed far down by the list.
        const POWER = '[data-testid="reach-power"]';
        const gap = await page.evaluate(([chip, slider]) =>
          Math.round(document.querySelector(slider).getBoundingClientRect().top - document.querySelector(chip).getBoundingClientRect().top), [GOOD, POWER]);
        assert(gap <= PHONE_SLIDER_MAX_PX, `with the list open the power slider is ${gap} px below the Good chip; at most ${PHONE_SLIDER_MAX_PX}`);
        const swipes = await touchScrollTo(page, cdp, POWER, 12);
        console.log(`    ${width}×${height}: the power slider ${gap} px below the chip, reached in ${swipes} swipes`);
        await shot(page, `941-m2-nodes-phone-slider-${theme}-${width}x${height}.jpg`);
        const before = rec.api.length;
        await dragSlider(page, cdp, POWER, -10);
        assert(await page.inputValue(POWER) === '-10', 'power after the drag ' + await page.inputValue(POWER));
        const c = await chips(page);
        assert(c.good.n === '0' && c.marginal.n === '12' && c.weak.n === '15' && c.all.n === '27', 'chips at −10 dBm ' + JSON.stringify(c));
        assert(rec.api.length === before, 'a budget change asked the server: ' + rec.api.slice(before).join(', '));
        const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
        assert(overflow <= 0, 'horizontal scroll ' + overflow);

        // Back up to the chips by touch; All lists the 27, Near mast first.
        const ALL = '[data-testid="reach-nir-all"]';
        await touchScrollTo(page, cdp, ALL, 12);
        await page.tap(ALL);
        l = await listed(page);
        assert(l.length === 27 && l[0].name === 'Near mast' && l[0].cls === 'marginal', 'All at −10 dBm ' + JSON.stringify(l.slice(0, 2)));
        await touchScrollTo(page, cdp, ROW, 8);
        // The whole row is the link's tap target: the swatch, the meta line
        // and the margin hit the link, and the row is at least 44 px tall.
        const row = await page.evaluate((sel) => {
          const li = document.querySelector(sel);
          const a = li.querySelector('a');
          const r = li.getBoundingClientRect();
          const at = (x, y) => { const h = document.elementFromPoint(x, y); return !!h && (h === a || a.contains(h)); };
          return { h: r.height, swatch: at(r.left + 4, r.top + r.height / 2), meta: at(r.left + r.width / 2, r.bottom - 4),
            margin: at(r.right - 4, r.top + r.height / 2) };
        }, ROW);
        assert(row.h >= 44 && row.swatch && row.meta && row.margin, 'row target ' + JSON.stringify(row));
        await shot(page, `941-m2-nodes-phone-${theme}-${width}x${height}.jpg`);
        // Keyboard focus on a row under the sticky head brings it out from
        // under the head (the sheet's scroll-padding-top).
        const focused = await page.evaluate((sel) => {
          const sheet = document.querySelector('[data-testid="reach-panel"]');
          const head = sheet.querySelector('.reach-head').getBoundingClientRect();
          const li = document.querySelector(sel);
          sheet.scrollTop += li.getBoundingClientRect().top - (head.top + 12);
          const under = li.getBoundingClientRect().top < head.bottom;
          li.querySelector('a').focus();
          return { under, top: li.getBoundingClientRect().top, headBottom: sheet.querySelector('.reach-head').getBoundingClientRect().bottom };
        }, ROW);
        assert(focused.under && focused.top >= focused.headBottom, 'a focused row stays under the head ' + JSON.stringify(focused));
        await touchScrollTo(page, cdp, ROW, 8);
        const tapAt = await page.evaluate((sel) => { const r = document.querySelector(sel).getBoundingClientRect(); return { x: r.right - 4, y: r.bottom - 4 }; }, ROW);
        await page.touchscreen.tap(tapAt.x, tapAt.y);
        await page.waitForFunction(() => location.hash.indexOf('#/nodes/' + '1'.repeat(64)) === 0, null, { timeout: 5000 });
        noNetwork(rec);
      } finally { await ctx.close(); }
    });
  }

  // The landscape-phone rule is for touch landscape phones under 480 px
  // high only: portrait phones keep the sheet along the bottom (8 px in from
  // each side, 46% of the map), and so do a 640x480 touch window (the bound
  // is 479) and a mouse at 640x360.
  const KEEP_PORTRAIT = [
    { width: 375, height: 560, touch: true }, { width: 360, height: 640, touch: true }, { width: 320, height: 568, touch: true },
    { width: 640, height: 480, touch: true }, { width: 640, height: 360, touch: false },
  ];
  for (const { width, height, touch } of KEEP_PORTRAIT) {
    await step(`${width}×${height}${touch ? ' touch' : ' mouse'}: the sheet keeps the portrait rule (8 px in, 46% of the map)`, async () => {
      const { ctx, page } = await newCtx({ viewport: { width, height }, touch, reach: PHONE_REACH });
      try {
        await page.goto(`${BASE}/#/map?lat=${AARHUS.lat}&lon=${AARHUS.lon}&zoom=10`, { waitUntil: 'domcontentloaded' });
        await page.waitForSelector('#leaflet-map[data-loaded="true"]', { timeout: 30000 });
        await page.waitForSelector('[data-testid="map-reach-toggle"]', { timeout: 15000 });
        if (touch) await page.tap('[data-testid="map-reach-toggle"]'); else await page.click('[data-testid="map-reach-toggle"]');
        const box = await page.locator('#leaflet-map').boundingBox();
        if (touch) await page.touchscreen.tap(box.x + box.width / 2, box.y + box.height / 3);
        else await page.mouse.click(box.x + box.width / 2, box.y + box.height / 3);
        await waitPanel(page);
        const g = await page.evaluate(() => {
          const sheet = document.querySelector('[data-testid="reach-panel"]');
          const r = sheet.getBoundingClientRect();
          const cs = getComputedStyle(sheet);
          return { left: r.left, right: innerWidth - r.right, maxHeight: cs.maxHeight, list: getComputedStyle(document.querySelector('[data-testid="reach-nir-list"]')).maxHeight };
        });
        assert(g.left === 8 && g.right === 8 && g.maxHeight === '46%', 'not the portrait rule ' + JSON.stringify(g));
        assert(!/^max\(45px/.test(g.list) && g.list !== 'none', 'the list took the landscape window ' + g.list);
      } finally { await ctx.close(); }
    });
  }

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed\n`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
