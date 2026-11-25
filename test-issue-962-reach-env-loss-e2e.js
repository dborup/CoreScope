/**
 * E2E (#962): the environment loss in the Predicted reach link budget, in
 * a real browser.
 *
 * As in test-issue-941-m2-nodes-in-reach-e2e.js, the reach is switched on
 * by intercepting /api/config/client (the `coverage` block cmd/server
 * sends, now with `environmentLossDb`), and /api/terrain/coverage and its
 * raster are a stub in the server's shape with five repeaters and rooms
 * in reach. Nothing here reaches a real network.
 *
 * Asserts:
 *   1. desktop, light and dark, from a cold link: the "Environment loss"
 *      field starts at the operator's 25 dB with its hint, the budget is
 *      158 − 25 = 133 dB and the chips and the list classes are at 133 dB;
 *      setting the field to 0 recolours the layer's shares and recounts
 *      the chips and the list classes at 158 dB with no new request of
 *      any kind to /api/, and writes renv=0; the reset returns it to the
 *      operator's 25 dB, drops renv, again with no request;
 *   2. an operator value of 0 gives today's budget and classes; a link
 *      without renv uses the operator's value; renv in a link wins;
 *   3. 375×560 with touch, from a cold #/map, real taps only: a node's
 *      marker, its popup's "View Node", the node's "Predicted reach" chip,
 *      then the environment loss slider (to 0) and the reset; each budget
 *      change recounts with no request, and nothing scrolls sideways;
 *   4. no console error, and no request leaves localhost;
 *   5. #968 M3, the viewer's default (Customize → Display → Predicted
 *      reach): on desktop, light and dark, the section shows with an empty
 *      field and the operator's value as placeholder; 40 moves the open
 *      panel (no renv written, no request), a bad value is put back; a cold
 *      link starts at 40, renv=10 wins and the panel's Reset lands on 40;
 *      emptying the field returns to the operator's value. Without the
 *      server's coverage block the section is not there. On a 375×560
 *      phone from a cold #/map, by real taps: More → Customize → Display,
 *      type 40, close, then the node's reach starts at 40 and its Reset
 *      after a slider tap returns to 40.
 *
 * SHOTS_DIR, when set, receives small JPEG screenshots (default 25 dB vs
 * 0 dB, desktop and phone; #968: the customizer section and the panel at
 * the viewer's default).
 *
 * Usage:
 *   BASE_URL=http://localhost:13581 node test-issue-962-reach-env-loss-e2e.js
 */
'use strict';
const path = require('path');
const { chromium } = require('playwright');
const { tapWhenSettled } = require('./tests/e2e-tap');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const SHOTS = process.env.SHOTS_DIR || '';
const COVERAGE_PATH = '/api/terrain/coverage';
const RASTER_PATH = '/api/terrain/coverage/raster';

const RADIO = { txPowerDbm: 22, antennaGainDbi: 3, rxGainDbi: 3, lossDb: 0, sensitivityDbm: -130 };
function coverageBlock(env) {
  const b = {
    enabled: true, maxRadiusKm: 40, radiusKm: 40, resolutionM: 30, rxHeightM: 2,
    pointAntenna: { m: 2, source: 'point-default' }, radio: RADIO, dhm: false,
    model: 'delta', models: ['bullington', 'delta'],
  };
  if (env !== undefined) b.environmentLossDb = env;
  return b;
}
const AARHUS = { lat: 56.1629, lon: 10.2039 };
// Lowest loss first. At 133 dB (25 dB environment loss): margins 18, 8, 2,
// −2, −12 → good, marginal, weak, none, none. At 158 dB (0 dB): 38, 28,
// 22, 18, 8 → good ×4, marginal.
const NODES = [115, 125, 131, 135, 150].map((loss, i) => ({
  pubkey: String(i + 1).repeat(64), name: 'Mast ' + (i + 1), role: 'repeater', distanceKm: 2 + i * 3,
  heightM: 10, heightSource: 'role-default', lossDb: loss,
}));
const REACH = { roles: ['repeater', 'room'], budgetDb: 158, counts: { good: 4, marginal: 1, weak: 0, none: 0 }, nodes: NODES, omitted: 0, noTerrain: 0 };
const AT_133 = { good: '1', marginal: '1', weak: '1', all: '3' };
const AT_158 = { good: '4', marginal: '1', weak: '0', all: '5' };

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
  console.log(`\n=== #962 environment loss against ${BASE} ===\n`);

  // A fixture node with a fix and a name, by the API.
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
    const rec = { consoleErrors: [], all: [], api: [], coverage: [] };
    await ctx.route(BASE + '/api/config/client', async (route) => {
      const response = await route.fetch();
      const config = await response.json();
      if (o.coverage === false) delete config.coverage;
      else config.coverage = o.coverage || coverageBlock(25);
      await route.fulfill({ response, json: config });
    });
    const rasters = new Map();
    await ctx.route(url => new URL(url).pathname === COVERAGE_PATH, async (route) => {
      const u = new URL(route.request().url());
      rec.coverage.push(u.search);
      const at = u.searchParams.get('at');
      const isNode = /^[0-9a-f]{64}$/.test(at);
      const c = isNode ? { lat: node.lat, lon: node.lon } : { lat: Number(at.split(',')[0]), lon: Number(at.split(',')[1]) };
      const r = Number(u.searchParams.get('r') || 40);
      const key = (u.search.length.toString(16) + 'c'.repeat(32)).slice(0, 32);
      const ras = rasterFor(c, r);
      rasters.set(key, ras.bytes);
      const body = {
        key, center: { lat: c.lat, lon: c.lon, kind: isNode ? 'node' : 'point', groundM: 34.2 },
        params: { radiusKm: r, resolutionM: 30, txHeightM: isNode ? 10 : 2, txHeightSource: isNode ? 'role-default' : 'point-default', txAssumed: true,
          rxHeightM: 2, rxHeightSource: 'default', freqMHz: 869.618, freqSource: 'firmware', k: 4 / 3, kSource: 'default', model: 'delta-bullington-p526-v1' },
        raster: { url: RASTER_PATH + '?key=' + key, width: ras.W, height: ras.H, bounds: ras.bounds, encoding: 'loss-u8',
          lossBaseDb: 60, lossStepDb: 0.5, noData: 255, coveredPct: 100, bytes: ras.bytes.length },
        sources: [{ id: 'dhm-dtm', resolutionM: 30, model: 'dtm' }],
        reach: REACH,
      };
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(body) });
    });
    await ctx.route(url => new URL(url).pathname === RASTER_PATH, async (route) => {
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

  /* panelState reads what the viewer sees of the budget: the field, its
   * value, the budget line, the reset, the chip counts, the class of every
   * node in the list (shown or not) and the legend's shares. */
  function panelState(page) {
    return page.evaluate(() => {
      const q = (s) => document.querySelector(s);
      const chips = {};
      ['good', 'marginal', 'weak', 'all'].forEach(c => { chips[c] = q('[data-testid="reach-nir-count-' + c + '"]').textContent; });
      const shares = {};
      ['good', 'marginal', 'weak'].forEach(c => { shares[c] = q('[data-testid="reach-share-' + c + '"]').textContent; });
      return {
        env: q('[data-testid="reach-env"]').value, envOut: q('[data-testid="reach-env-value"]').textContent,
        budget: q('[data-testid="reach-budget"]').textContent, hint: q('[data-testid="reach-env-hint"]').textContent,
        resetDisabled: q('[data-testid="reach-env-reset"]').disabled, chips, shares,
        classes: Array.from(document.querySelectorAll('[data-testid="reach-nir-item"]')).map(li => li.getAttribute('data-class')),
        hash: location.hash,
      };
    });
  }

  /* setEnv moves the environment loss slider like a mouse drag would and
   * waits past the request debounce, so a request it wrongly started
   * would be seen. */
  async function setEnv(page, v) {
    await page.evaluate((val) => {
      const s = document.querySelector('[data-testid="reach-env"]');
      s.value = String(val);
      s.dispatchEvent(new Event('input', { bubbles: true }));
    }, v);
    await page.waitForTimeout(800);
  }

  function pct(s) { return Number(String(s).replace(/[^0-9]/g, '')); }

  function noNetwork(rec) {
    const bad = rec.all.filter(u => !/^https?:\/\/(localhost|127\.0\.0\.1)[:/]/i.test(u) && !u.startsWith('data:') && !u.startsWith('blob:'));
    assert(bad.filter(u => /dataforsyningen|amazonaws|copernicus/i.test(u)).length === 0, 'a real upstream was reached');
    const errs = rec.consoleErrors.filter(e => !/Failed to load resource/.test(e));
    assert(errs.length === 0, 'console errors: ' + errs.join(' | '));
  }

  /* openNodeReachByTaps: from a cold #/map at the node, real taps on its
   * marker, its popup's "View Node" and the node's "Predicted reach" chip;
   * resolves when the panel is ready. */
  async function openNodeReachByTaps(page, rec, opened) {
    if (!opened) {
      await page.goto(`${BASE}/#/map?lat=${node.lat}&lon=${node.lon}&zoom=16`, { waitUntil: 'domcontentloaded' });
    }
    await page.waitForSelector('#leaflet-map[data-loaded="true"]', { timeout: 30000 });
    await page.waitForTimeout(600);
    // The node's marker: the one nearest the map's centre, which is the node.
    const mk = await page.evaluate(() => {
      const m = document.querySelector('#leaflet-map').getBoundingClientRect();
      const cx = m.left + m.width / 2, cy = m.top + m.height / 2;
      let best = null;
      document.querySelectorAll('#leaflet-map .leaflet-marker-icon').forEach(e => {
        const r = e.getBoundingClientRect();
        const x = r.left + r.width / 2, y = r.top + r.height / 2, d = Math.hypot(x - cx, y - cy);
        if (r.width > 0 && (!best || d < best.d)) best = { x, y, d };
      });
      return best;
    });
    assert(mk && mk.d < 30, 'no marker at the node: ' + JSON.stringify(mk));
    await page.touchscreen.tap(mk.x, mk.y);
    const view = page.locator('.leaflet-popup-content a[href="#/nodes/' + node.pk + '"]');
    await view.waitFor({ state: 'visible', timeout: 10000 });
    await view.tap();
    await page.waitForSelector('[data-testid="node-predicted-reach"]', { timeout: 30000 });
    await tapWhenSettled(page, '[data-testid="node-predicted-reach"]', { minHeight: 32 });
    await waitPanel(page);
    assert(new URLSearchParams(rec.coverage[rec.coverage.length - 1]).get('at') === node.pk.toLowerCase(), 'not the node\'s reach: ' + rec.coverage.join(' '));
  }

  /* tapEnvSliderStart taps the environment loss slider's left end (0 dB). */
  async function tapEnvSliderStart(page) {
    await page.evaluate(() => document.querySelector('[data-testid="reach-env"]').scrollIntoView({ block: 'center' }));
    await page.waitForTimeout(300);
    const sl = await page.evaluate(() => {
      const e = document.querySelector('[data-testid="reach-env"]');
      const r = e.getBoundingClientRect();
      const x = r.left + 2, y = r.top + r.height / 2, top = document.elementFromPoint(x, y);
      return { x, y, onTop: top === e, h: r.height };
    });
    assert(sl.onTop && sl.h >= 32, 'the slider is covered or small: ' + JSON.stringify(sl));
    await page.touchscreen.tap(sl.x, sl.y);
    await page.waitForTimeout(800);
  }

  async function shot(page, name, el) {
    if (!SHOTS) return;
    const target = el ? await page.$(el) : page;
    await target.screenshot({ path: path.join(SHOTS, name), type: 'jpeg', quality: 60 });
  }

  // ── 1: desktop ─────────────────────────────────────────────────────────
  for (const theme of ['light', 'dark']) {
    await step(`${theme}: 25 dB by default; 0 dB recolours and recounts with no request; the reset returns to 25 dB`, async () => {
      const { ctx, page, rec } = await newCtx({ theme });
      try {
        await page.goto(`${BASE}/#/map?reach=${AARHUS.lat},${AARHUS.lon}&rkm=25`, { waitUntil: 'domcontentloaded' });
        await waitPanel(page);
        await page.click('[data-testid="reach-nir-all"]');
        let s = await panelState(page);
        assert(s.env === '25' && s.envOut === '25 dB' && s.budget === 'Maximum path loss 133 dB', 'default ' + JSON.stringify(s));
        assert(/buildings, trees and other clutter the terrain model does not include/i.test(s.hint), 'hint ' + s.hint);
        assert(s.resetDisabled, 'the reset is offered at the default');
        assert(JSON.stringify(s.chips) === JSON.stringify(AT_133), 'chips at 133 dB ' + JSON.stringify(s.chips));
        assert(JSON.stringify(s.classes) === JSON.stringify(['good', 'marginal', 'weak', 'none', 'none']), 'classes at 133 dB ' + s.classes);
        assert(!/renv=/.test(s.hash), 'renv written for the default: ' + s.hash);
        const at25 = s.shares;
        await shot(page, `962-desktop-${theme}-25db.jpg`);
        await shot(page, `962-panel-${theme}-25db.jpg`, '[data-testid="reach-panel"]');
        await page.evaluate(() => document.querySelector('[data-testid="reach-env"]').scrollIntoView({ block: 'center' }));
        await shot(page, `962-budget-${theme}-25db.jpg`, 'fieldset:has([data-testid="reach-env"])');

        const api = rec.api.length, cov = rec.coverage.length;
        await setEnv(page, 0);
        s = await panelState(page);
        assert(s.env === '0' && s.envOut === '0 dB' && s.budget === 'Maximum path loss 158 dB', 'at 0 ' + JSON.stringify(s));
        assert(JSON.stringify(s.chips) === JSON.stringify(AT_158), 'chips at 158 dB ' + JSON.stringify(s.chips));
        assert(JSON.stringify(s.classes) === JSON.stringify(['good', 'good', 'good', 'good', 'marginal']), 'classes at 158 dB ' + s.classes);
        assert(pct(s.shares.good) > pct(at25.good), 'the shares did not move: ' + JSON.stringify([at25, s.shares]));
        assert(/[?&]renv=0(&|$)/.test(s.hash), 'renv=0 not in the link: ' + s.hash);
        assert(!s.resetDisabled, 'no reset away from the default');
        assert(rec.api.length === api && rec.coverage.length === cov, 'a budget change asked the server: ' + rec.api.slice(api).join(' '));
        await shot(page, `962-desktop-${theme}-0db.jpg`);
        await shot(page, `962-panel-${theme}-0db.jpg`, '[data-testid="reach-panel"]');
        await page.evaluate(() => document.querySelector('[data-testid="reach-env"]').scrollIntoView({ block: 'center' }));
        await shot(page, `962-budget-${theme}-0db.jpg`, 'fieldset:has([data-testid="reach-env"])');

        await page.click('[data-testid="reach-env-reset"]');
        await page.waitForTimeout(800);
        s = await panelState(page);
        assert(s.env === '25' && s.budget === 'Maximum path loss 133 dB' && s.resetDisabled, 'after reset ' + JSON.stringify(s));
        assert(JSON.stringify(s.chips) === JSON.stringify(AT_133) && JSON.stringify(s.shares) === JSON.stringify(at25), 'not back at 133 dB ' + JSON.stringify(s));
        assert(!/renv=/.test(s.hash), 'renv left in the link after the reset: ' + s.hash);
        assert(rec.api.length === api && rec.coverage.length === cov, 'the reset asked the server');
        noNetwork(rec);
      } finally { await ctx.close(); }
    });
  }

  // ── 2: operator values and links ───────────────────────────────────────
  for (const c of [
    { name: 'an operator 0 dB is today\'s budget and classes', env: 0, link: '', budget: 158, chips: AT_158, field: '0' },
    { name: 'a link without renv uses the operator\'s 35 dB', env: 35, link: '', budget: 123, chips: { good: '0', marginal: '1', weak: '0', all: '1' }, field: '35' },
    { name: 'renv in a link wins over the operator\'s 35 dB', env: 35, link: '&renv=5', budget: 153, chips: { good: '4', marginal: '1', weak: '0', all: '5' }, field: '5' },
    { name: 'a server without the key: 25 dB', env: undefined, link: '', budget: 133, chips: AT_133, field: '25' },
  ]) {
    await step(c.name, async () => {
      const { ctx, page, rec } = await newCtx({ coverage: coverageBlock(c.env) });
      try {
        await page.goto(`${BASE}/#/map?reach=${AARHUS.lat},${AARHUS.lon}&rkm=25${c.link}`, { waitUntil: 'domcontentloaded' });
        await waitPanel(page);
        const s = await panelState(page);
        assert(s.budget === 'Maximum path loss ' + c.budget + ' dB' && s.env === c.field, JSON.stringify(s));
        assert(JSON.stringify(s.chips) === JSON.stringify(c.chips), 'chips ' + JSON.stringify(s.chips));
        assert(!/renv/.test(rec.coverage.join(' ')), 'the environment loss reached the server: ' + rec.coverage.join(' '));
        noNetwork(rec);
      } finally { await ctx.close(); }
    });
  }

  // ── 3: phone, cold #/map, real taps ────────────────────────────────────
  for (const theme of ['light', 'dark']) {
    await step(`375×560 ${theme}, cold #/map: taps open a node's reach; the slider to 0 dB and the reset recount with no request`, async () => {
      const { ctx, page, rec } = await newCtx({ theme, viewport: { width: 375, height: 560 }, touch: true });
      try {
        await openNodeReachByTaps(page, rec);
        let s = await panelState(page);
        assert(s.budget === 'Maximum path loss 133 dB' && JSON.stringify(s.chips) === JSON.stringify(AT_133), 'phone default ' + JSON.stringify(s));
        await page.evaluate(() => document.querySelector('[data-testid="reach-env"]').scrollIntoView({ block: 'center' }));
        await page.waitForTimeout(300);
        await shot(page, `962-phone-${theme}-375x560-25db.jpg`);

        // A tap at the slider's left end puts it at 0 dB.
        const api = rec.api.length, cov = rec.coverage.length;
        await tapEnvSliderStart(page);
        s = await panelState(page);
        assert(s.env === '0' && s.budget === 'Maximum path loss 158 dB' && JSON.stringify(s.chips) === JSON.stringify(AT_158), 'phone at 0 ' + JSON.stringify(s));
        assert(/[?&]renv=0(&|$)/.test(s.hash), 'renv=0 not in the link: ' + s.hash);
        await shot(page, `962-phone-${theme}-375x560-0db.jpg`);

        await tapWhenSettled(page, '[data-testid="reach-env-reset"]');
        await page.waitForTimeout(800);
        s = await panelState(page);
        assert(s.env === '25' && s.budget === 'Maximum path loss 133 dB' && JSON.stringify(s.chips) === JSON.stringify(AT_133) && !/renv=/.test(s.hash),
          'phone after reset ' + JSON.stringify(s));
        assert(rec.api.length === api && rec.coverage.length === cov, 'a budget change asked the server: ' + rec.api.slice(api).join(' '));
        const sideways = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
        assert(sideways <= 0, 'horizontal scroll ' + sideways + ' px');
        noNetwork(rec);
      } finally { await ctx.close(); }
    });
  }

  // ── 5: #968 M3, the viewer's default ──────────────────────────────────
  const REACH_INPUT = '[data-cv2-reach="environmentLossDb"]';

  /* openDisplayTab: the palette button (desktop), or More → Customize on a
   * phone, then the Display tab, scrolled to the Predicted reach field. */
  async function openDisplayTab(page, phone) {
    await page.waitForFunction(() => window._customizerV2 && window._customizerV2.initDone, null, { timeout: 15000 });
    if (phone) {
      await tapWhenSettled(page, '[data-bottom-nav-tab="more"]');
      await page.waitForSelector('[data-mpa-mirror="customizeToggle"]', { state: 'visible', timeout: 5000 });
      await tapWhenSettled(page, '[data-mpa-mirror="customizeToggle"]');
    } else await page.click('#customizeToggle');
    await page.waitForSelector('.cust-overlay:not(.hidden)', { timeout: 5000 });
    if (phone) await tapWhenSettled(page, '.cust-tab[data-tab="display"]', { minHeight: 32 });
    else await page.click('.cust-tab[data-tab="display"]');
    await page.waitForSelector('.cust-panel[data-panel="display"].active', { timeout: 4000 });
  }

  function customizerReach(page) {
    return page.evaluate((sel) => {
      const i = document.querySelector(sel);
      const title = [...document.querySelectorAll('.cust-panel[data-panel="display"] .cust-section-title')].some(p => p.textContent === 'Predicted reach');
      if (!i) return { title, input: false };
      const row = i.closest('.cust-terrain-row');
      return { title, input: true, value: i.value, placeholder: i.placeholder, min: i.min, max: i.max, hint: row ? row.textContent : '',
        h: i.getBoundingClientRect().height,
        stored: (() => { try { return (JSON.parse(localStorage.getItem('cs-theme-overrides') || '{}').reach) || null; } catch (_) { return 'bad'; } })() };
    }, REACH_INPUT);
  }

  async function setReachField(page, v) {
    await page.fill(REACH_INPUT, String(v));
    await page.dispatchEvent(REACH_INPUT, 'change');
    await page.waitForTimeout(700); // the customizer's debounced write, then theme-refresh
  }

  function resetLabel(page) {
    return page.evaluate(() => document.querySelector('[data-testid="reach-env-reset"]').getAttribute('aria-label'));
  }

  for (const theme of ['light', 'dark']) {
    await step(`#968 ${theme}: the Display tab sets the viewer's default; renv wins; the panel's Reset lands on it`, async () => {
      const { ctx, page, rec } = await newCtx({ theme });
      try {
        await page.goto(`${BASE}/#/map?reach=${AARHUS.lat},${AARHUS.lon}&rkm=25`, { waitUntil: 'domcontentloaded' });
        await waitPanel(page);
        await openDisplayTab(page);
        await page.locator(REACH_INPUT).scrollIntoViewIfNeeded();
        let c = await customizerReach(page);
        assert(c.title && c.input && c.value === '' && c.placeholder === '25' && c.min === '0' && c.max === '60' && c.stored === null, 'section ' + JSON.stringify(c));
        assert(/Empty: the server\u2019s 25 dB/.test(c.hint) && /renv/.test(c.hint), 'hint ' + c.hint);
        await shot(page, `968-customizer-display-${theme}.jpg`, '.cust-overlay');

        // 40 dB moves the open panel at once, with no request and no renv.
        const cov = rec.coverage.length;
        await setReachField(page, 40);
        c = await customizerReach(page);
        assert(JSON.stringify(c.stored) === '{"environmentLossDb":40}', 'stored ' + JSON.stringify(c.stored));
        let s = await panelState(page);
        assert(s.env === '40' && s.budget === 'Maximum path loss 118 dB' && s.resetDisabled && !/renv=/.test(s.hash), 'live ' + JSON.stringify(s));
        assert(/your default, 40 dB/.test(await resetLabel(page)), 'reset label ' + await resetLabel(page));
        // A bad value is put back, never stored.
        await setReachField(page, 70);
        c = await customizerReach(page);
        assert(c.value === '40' && JSON.stringify(c.stored) === '{"environmentLossDb":40}', 'a bad value stuck ' + JSON.stringify(c));
        assert(rec.coverage.length === cov, 'the viewer default asked the server again');

        // A cold link: 40 dB; renv=10 wins; Reset lands on 40.
        await page.goto(`${BASE}/?r=1#/map?reach=${AARHUS.lat},${AARHUS.lon}&rkm=25`, { waitUntil: 'domcontentloaded' });
        await waitPanel(page);
        s = await panelState(page);
        assert(s.env === '40' && s.budget === 'Maximum path loss 118 dB' && !/renv=/.test(s.hash), 'cold ' + JSON.stringify(s));
        await page.goto(`${BASE}/?r=2#/map?reach=${AARHUS.lat},${AARHUS.lon}&rkm=25&renv=10`, { waitUntil: 'domcontentloaded' });
        await waitPanel(page);
        s = await panelState(page);
        assert(s.env === '10' && s.budget === 'Maximum path loss 148 dB' && !s.resetDisabled, 'renv ' + JSON.stringify(s));
        await page.click('[data-testid="reach-env-reset"]');
        await page.waitForTimeout(800);
        s = await panelState(page);
        assert(s.env === '40' && s.budget === 'Maximum path loss 118 dB' && s.resetDisabled && !/renv=/.test(s.hash), 'reset ' + JSON.stringify(s));
        await page.click('[data-testid="reach-nir-all"]');
        await page.waitForTimeout(200);
        await shot(page, `968-desktop-${theme}-viewer-40db.jpg`);
        await shot(page, `968-panel-${theme}-viewer-40db.jpg`, '[data-testid="reach-panel"]');

        // Emptying the field: the operator's value again, nothing stored.
        await openDisplayTab(page);
        await setReachField(page, '');
        c = await customizerReach(page);
        s = await panelState(page);
        assert(c.stored === null && s.env === '25' && s.budget === 'Maximum path loss 133 dB', 'cleared ' + JSON.stringify([c, s]));
        assert(/server default, 25 dB/.test(await resetLabel(page)), 'reset label ' + await resetLabel(page));
        assert(!/renv|env/.test(rec.coverage.join(' ')), 'the environment loss reached the server: ' + rec.coverage.join(' '));
        noNetwork(rec);
      } finally { await ctx.close(); }
    });
  }

  await step('#968: without the server\'s coverage block the Display tab has no Predicted reach section', async () => {
    const { ctx, page, rec } = await newCtx({ coverage: false });
    try {
      await page.goto(`${BASE}/#/map`, { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('#leaflet-map[data-loaded="true"]', { timeout: 30000 });
      await openDisplayTab(page);
      const c = await customizerReach(page);
      assert(!c.title && !c.input, 'shown without coverage ' + JSON.stringify(c));
      assert(!(await page.$('[data-testid="map-reach-toggle"]')), 'the map offers the reach without coverage');
      await shot(page, '968-customizer-display-no-coverage.jpg', '.cust-overlay');
      noNetwork(rec);
    } finally { await ctx.close(); }
  });

  await step('#968 375×560, cold #/map, real taps: More → Customize → Display, 40 dB; the node\'s reach starts there and Reset returns to it', async () => {
    const { ctx, page, rec } = await newCtx({ theme: 'dark', viewport: { width: 375, height: 560 }, touch: true });
    try {
      await page.goto(`${BASE}/#/map?lat=${node.lat}&lon=${node.lon}&zoom=16`, { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('#leaflet-map[data-loaded="true"]', { timeout: 30000 });
      await openDisplayTab(page, true);
      await page.locator(REACH_INPUT).scrollIntoViewIfNeeded();
      await page.waitForTimeout(300);
      let c = await customizerReach(page);
      assert(c.title && c.input && c.value === '' && c.h >= 32, 'phone section ' + JSON.stringify(c));
      await tapWhenSettled(page, REACH_INPUT, { minHeight: 32 });
      await page.keyboard.type('40');
      await page.keyboard.press('Enter');
      // Leaving the field commits it, as on a phone keyboard's "done".
      await page.evaluate((sel) => document.querySelector(sel).blur(), REACH_INPUT);
      await page.waitForTimeout(700);
      c = await customizerReach(page);
      assert(JSON.stringify(c.stored) === '{"environmentLossDb":40}' && c.value === '40', 'phone stored ' + JSON.stringify(c));
      // The customizer re-renders its panel on every write (scrolling it to
      // the top); bring the field back for the picture.
      await page.locator(REACH_INPUT).scrollIntoViewIfNeeded();
      await page.waitForTimeout(200);
      await shot(page, '968-phone-dark-375x560-customizer.jpg');
      const sideways1 = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
      assert(sideways1 <= 0, 'horizontal scroll in the customizer ' + sideways1 + ' px');
      await tapWhenSettled(page, '.cust-close', { minHeight: 24 });
      await page.waitForSelector('.cust-overlay.hidden', { state: 'attached', timeout: 5000 });

      await openNodeReachByTaps(page, rec, true);
      let s = await panelState(page);
      assert(s.env === '40' && s.budget === 'Maximum path loss 118 dB' && s.resetDisabled && !/renv=/.test(s.hash), 'phone default ' + JSON.stringify(s));
      await page.evaluate(() => document.querySelector('[data-testid="reach-env"]').scrollIntoView({ block: 'center' }));
      await page.waitForTimeout(300);
      await shot(page, '968-phone-dark-375x560-viewer-40db.jpg');
      const cov = rec.coverage.length;
      await tapEnvSliderStart(page);
      s = await panelState(page);
      assert(s.env === '0' && /[?&]renv=0(&|$)/.test(s.hash), 'phone at 0 ' + JSON.stringify(s));
      await tapWhenSettled(page, '[data-testid="reach-env-reset"]');
      await page.waitForTimeout(800);
      s = await panelState(page);
      assert(s.env === '40' && s.budget === 'Maximum path loss 118 dB' && !/renv=/.test(s.hash), 'phone reset ' + JSON.stringify(s));
      assert(rec.coverage.length === cov, 'a budget change asked the server');
      const sideways = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
      assert(sideways <= 0, 'horizontal scroll ' + sideways + ' px');
      noNetwork(rec);
    } finally { await ctx.close(); }
  });

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed\n`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
