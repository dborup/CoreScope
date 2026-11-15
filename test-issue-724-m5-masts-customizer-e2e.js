/**
 * E2E (#724 M5): the antenna sites settings in the customizer, and their
 * effect on the Map page.
 *
 * The CI server has neither map.dhm nor map.masts, so /api/config/client is
 * intercepted and given both blocks (the masts block with operator values
 * minZoom 14, hintDistanceM 500, hintStrongM 100), /api/terrain/profile is
 * answered with a synthetic profile and /api/masts with fixed synthetic
 * sites. Neither Mastedatabasen nor any DHM service is ever contacted.
 *
 * Asserts:
 *   1. the Display tab has an "Antenna Sites" section with the four
 *      settings, starting at the operator's values and bounded like the
 *      server (zoom 14-18, distance 100-990, strong 0-distance), and the
 *      inputs are 48 px tall;
 *   2. changing the hint distance N in the customizer updates an open
 *      profile's hint without a reload: a site 700 m from A shows no chip
 *      at 500 m and "Cell transmitter ~700 m" at 800 m, and the chip goes
 *      again at 600 m; the strong tier follows too;
 *   3. a value out of bounds (995, 13 below the operator's gate, a strong
 *      tier past the distance) is put back and never stored;
 *   4. round trip: the stored section survives a reload, shows in the
 *      inputs again and in the export JSON;
 *   5. a raised zoom gate (16) at zoom 15 asks for no cell and says
 *      "Zoom in to 16+"; lowering it again loads the view without a reload;
 *   6. "Show antenna sites by default" off: the next Map page starts with
 *      the sites overlay off, and a link with masts=1 still turns it on;
 *   7. without the masts block the section is absent;
 *   8. no console errors.
 *
 * Usage:
 *   BASE_URL=http://localhost:13581 node test-issue-724-m5-masts-customizer-e2e.js
 */
'use strict';
const { chromium } = require('playwright');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const PROFILE_PATH = '/api/terrain/profile';
const MASTS_PATH = '/api/masts';
const ATTRIBUTION = 'Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel, CC BY 4.0';
const DHM_BLOCK = {
  enabled: true, url: '/api/map/dhm/wms', layer: 'dhm_terraen_skyggekort',
  minZoom: 7, maxZoom: 19, profile: true,
  bounds: [[54.4265, 7.99125], [57.778, 15.5995]],
  attribution: ATTRIBUTION,
};
const MASTS = {
  enabled: true, url: MASTS_PATH, minZoom: 14, cellSizeM: 2000,
  attribution: 'Antenna positions: Mastedatabasen, Digitaliseringsstyrelsen (CC0)',
  sourceUrl: 'https://mastedatabasen.dk/', cellExtent: [219, 3017, 462, 3209],
  hintDistanceM: 500, hintStrongM: 100,
};
const PNG_1X1 = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGM4ceIEAAS0AlkWLoFAAAAAAElFTkSuQmCC',
  'base64');
const CHIP = '[data-testid="tp-mast-hint"]';
const STORE = 'cs-theme-overrides';

const ML = (() => {
  const ctx = { window: {}, Math, Map, Set, Promise, Object, Array, Number, String, isFinite };
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'public', 'masts-layer.js'), 'utf8'), ctx);
  return ctx.window.MastsLayer;
})();

const R = 6371000;
const north = (ll, m) => [+(ll[0] + m / R * 180 / Math.PI).toFixed(7), ll[1]];
const fmt = (ll) => ll[0].toFixed(5) + ',' + ll[1].toFixed(5);
const cellOf = (ll) => { const p = ML.toUTM32(ll[0], ll[1]); return Math.floor(p.e / 2000) + ',' + Math.floor(p.n / 2000); };

// Viborg heath, far from the fixture's nodes. A is 700 m from an 800 MHz
// site and B 60 m from a 900 MHz one.
const A = [56.43, 9.40], B = [56.44, 9.43];
const SITES = [
  { ll: north(A, 700), entries: [{ technology: 'LTE', bandMHz: 800, interference: true }] },
  { ll: north(B, 60), entries: [{ technology: 'GSM', bandMHz: 900, interference: true }] },
];
const BY_CELL = {};
SITES.forEach((s, i) => {
  const key = cellOf(s.ll);
  (BY_CELL[key] = BY_CELL[key] || []).push({ lat: s.ll[0], lon: s.ll[1], city: 'Viborg', stations: ['S' + i], entries: s.entries, interference: true, likelyMast: false });
});

function profileFor(q) {
  const D = 1800;
  const end = (side) => {
    const ll = (q.get(side) || '').split(',').map(Number);
    return { lat: ll[0], lon: ll[1], kind: 'point', groundM: 40, antennaM: 2, antennaSource: 'point-default', assumed: true };
  };
  const samples = [];
  for (let i = 0; i < 40; i++) {
    const d = Math.round(D * i / 39);
    samples.push({ d, dtm: 30 + 8 * Math.sin(d / 200), dsm: 34 + 8 * Math.sin(d / 200) });
  }
  return {
    a: end('a'), b: end('b'),
    distanceM: D, stepM: 1.8, sampleCount: 1001, model: 'both',
    coverage: { state: 'full', pct: 100, firstGapM: null }, samples,
    physics: { freqMHz: 869.618, freqSource: 'firmware', lambdaM: 0.34474, k: 1.3333333333333333, kSource: 'default', f1MaxM: 12.5 },
    verdict: { state: 'clear', model: 'dtm', criterion: 0.6, worstClearanceM: 21.2, worstAtM: 600, worstF1Fraction: 2.1,
      surface: { state: 'clear', worstClearanceM: 15.1, worstAtM: 600, worstF1Fraction: 1.5 } },
    attribution: ATTRIBUTION,
  };
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
  console.log(`\n=== #724 M5 customizer antenna sites settings against ${BASE} ===\n`);

  async function newCtx(o) {
    o = o || {};
    const ctx = await browser.newContext({ viewport: o.viewport || { width: 1280, height: 800 } });
    const rec = { masts: [], consoleErrors: [] };
    await ctx.route(BASE + '/api/config/client', async (route) => {
      const response = await route.fetch();
      const config = await response.json();
      config.dhm = DHM_BLOCK;
      if (o.masts !== false) config.masts = MASTS;
      await route.fulfill({ response, json: config });
    });
    await ctx.route(url => new URL(url).pathname === PROFILE_PATH, async (route) => {
      const u = new URL(route.request().url());
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(profileFor(u.searchParams)) });
    });
    await ctx.route(url => new URL(url).pathname === MASTS_PATH, async (route) => {
      const u = new URL(route.request().url());
      rec.masts.push(u.searchParams.get('cell'));
      const key = u.searchParams.get('cell');
      await route.fulfill({ status: 200, contentType: 'application/json',
        body: JSON.stringify({ cell: { e: +key.split(',')[0], n: +key.split(',')[1] }, truncated: false, sites: BY_CELL[key] || [] }) });
    });
    await ctx.route(/^https?:\/\/(?!localhost|127\.0\.0\.1)/i, async (route) => {
      await route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 });
    });
    await ctx.route(url => new URL(url).pathname === '/api/map/dhm/wms', async (route) => {
      await route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 });
    });
    await ctx.addInitScript((init) => {
      try {
        if (sessionStorage.getItem('__m5init')) return;   // once per context, not on every reload
        sessionStorage.setItem('__m5init', '1');
        localStorage.setItem('meshcore-theme', 'light');
        localStorage.setItem('meshcore-map-clustering', 'false');
        ['row-swipe', 'tab-swipe', 'edge-drawer', 'pull-refresh']
          .forEach(k => localStorage.setItem('meshcore-gesture-hints-' + k, 'seen'));
        Object.keys(init || {}).forEach(k => localStorage.setItem(k, init[k]));
      } catch (_) {}
    }, o.storage || {});
    const page = await ctx.newPage();
    page.on('console', m => { if (m.type() === 'error') rec.consoleErrors.push(m.text()); });
    page.on('pageerror', e => rec.consoleErrors.push(String(e)));
    return { ctx, page, rec };
  }

  async function ready(page) {
    await page.waitForSelector('#leaflet-map[data-loaded="true"]', { timeout: 30000 });
    await page.waitForFunction(() => window._customizerV2 && window._customizerV2.initDone, null, { timeout: 15000 });
  }

  async function openProfile(page) {
    await page.goto(`${BASE}/#/map?profile=${fmt(A)},${fmt(B)}`, { waitUntil: 'domcontentloaded' });
    await ready(page);
    await page.waitForFunction(() => {
      const c = document.querySelector('[data-testid="tp-verdict"]');
      return !!(c && c.getAttribute('data-state') && c.getAttribute('data-state') !== 'pending');
    }, null, { timeout: 15000 });
  }

  async function openDisplay(page) {
    if (!(await page.$('.cust-overlay:not(.hidden)'))) {
      await page.click('#customizeToggle');
      await page.waitForSelector('.cust-overlay:not(.hidden)', { timeout: 5000 });
    }
    await page.click('.cust-tab[data-tab="display"]');
    await page.waitForSelector('.cust-panel[data-panel="display"].active', { timeout: 4000 });
  }

  async function setNum(page, key, value) {
    const sel = `[data-cv2-masts="${key}"]`;
    await page.fill(sel, String(value));
    await page.dispatchEvent(sel, 'change');
    await page.waitForTimeout(600); // the customizer's debounced write
  }

  const storedMasts = (page) => page.evaluate((k) => {
    try { return (JSON.parse(localStorage.getItem(k) || '{}').masts) || null; } catch (_) { return 'unparsable'; }
  }, STORE);

  const chip = (page) => page.evaluate((sel) => {
    const c = document.querySelector(sel);
    return c && !c.hidden ? { text: c.textContent, tier: c.getAttribute('data-tier') } : null;
  }, CHIP);

  async function waitChip(page, want, tier) {
    await page.waitForFunction(([sel, w, t]) => {
      const c = document.querySelector(sel);
      if (w === null) return !c || c.hidden;
      return !!(c && !c.hidden && c.textContent === w && (!t || c.getAttribute('data-tier') === t));
    }, [CHIP, want, tier || null], { timeout: 10000 });
  }

  // ── 1-4. The section, the live hint, bounds, round trip ─────────────────
  {
    const { ctx, page, rec } = await newCtx({ storage: { 'mc-masts': '0', 'mc-masts-int': '0' } });

    await step('the Display tab has the four antenna sites settings at the operator values', async () => {
      await openProfile(page);
      await openDisplay(page);
      const f = await page.evaluate(() => {
        const one = (k) => {
          const el = document.querySelector('[data-cv2-masts="' + k + '"]');
          if (!el) return null;
          const lab = document.querySelector('label[for="' + el.id + '"]');
          return { type: el.type, value: el.value, checked: el.checked, min: el.min, max: el.max, h: el.getBoundingClientRect().height,
            label: lab ? lab.textContent : '', labelH: lab ? lab.getBoundingClientRect().height : 0 };
        };
        const title = Array.from(document.querySelectorAll('.cust-section-title')).some((p) => p.textContent === 'Antenna Sites');
        return { title, enabled: one('enabled'), minZoom: one('minZoom'), far: one('hintDistanceM'), strong: one('hintStrongM') };
      });
      assert(f.title, 'no Antenna Sites section');
      assert(f.enabled && f.enabled.type === 'checkbox' && f.enabled.checked && f.enabled.labelH >= 44, 'enabled ' + JSON.stringify(f.enabled));
      assert(f.minZoom && f.minZoom.value === '14' && f.minZoom.min === '14' && f.minZoom.max === '18', 'minZoom ' + JSON.stringify(f.minZoom));
      assert(f.far && f.far.value === '500' && f.far.min === '100' && f.far.max === '990', 'distance ' + JSON.stringify(f.far));
      assert(f.strong && f.strong.value === '100' && f.strong.min === '0' && f.strong.max === '500', 'strong ' + JSON.stringify(f.strong));
      for (const k of ['minZoom', 'far', 'strong']) assert(f[k].h >= 48, k + ' input ' + f[k].h + ' px');
    });

    await step('changing N in the customizer updates the open profile hint', async () => {
      // B is 60 m from a 900 MHz site: strong at the operator's 100 m.
      await waitChip(page, 'Cell transmitter ~60 m', 'strong');
      await setNum(page, 'hintDistanceM', 800);
      await waitChip(page, 'Cell transmitter ~60 m', 'strong');
      // Strong tier below 60 m: B is now only "near"; A at 700 m is in range.
      await setNum(page, 'hintStrongM', 50);
      await waitChip(page, 'Cell transmitter ~60 m', 'near');
      const note = await page.evaluate(() => document.querySelector('[data-testid="tp-mast-note"]').textContent);
      assert(/^A: an 800 MHz cellular transmitter about 700 m away\./.test(note), 'A at 800 m: ' + note);
      assert(/Within about 800 m, especially within 50 m/.test(note), note);
      assert(rec.masts.includes(cellOf(SITES[0].ll)), 'the 700 m site cell was never fetched: ' + rec.masts);
      await setNum(page, 'hintDistanceM', 600);
      await page.waitForFunction(() => !/^A:/.test(document.querySelector('[data-testid="tp-mast-note"]').textContent), null, { timeout: 10000 });
      assert((await chip(page)).text === 'Cell transmitter ~60 m');
      const s = await storedMasts(page);
      assert(s && s.hintDistanceM === 600 && s.hintStrongM === 50 && !('minZoom' in s), 'stored ' + JSON.stringify(s));
    });

    await step('out-of-bounds values are put back and never stored', async () => {
      await setNum(page, 'hintDistanceM', 995);
      assert(await page.inputValue('[data-cv2-masts="hintDistanceM"]') === '600', 'distance kept 995');
      await setNum(page, 'minZoom', 13);
      assert(await page.inputValue('[data-cv2-masts="minZoom"]') === '14', 'gate went below the operator');
      await setNum(page, 'hintStrongM', 650);
      assert(await page.inputValue('[data-cv2-masts="hintStrongM"]') === '50', 'strong tier past the distance kept');
      await setNum(page, 'hintDistanceM', -5);
      const s = await storedMasts(page);
      assert(s && s.hintDistanceM === 600 && s.hintStrongM === 50 && !('minZoom' in s), 'stored ' + JSON.stringify(s));
    });

    await step('back at the operator value is a cleared override', async () => {
      await setNum(page, 'hintStrongM', 100);
      const s = await storedMasts(page);
      assert(s && !('hintStrongM' in s) && s.hintDistanceM === 600, 'stored ' + JSON.stringify(s));
      await setNum(page, 'hintStrongM', 50);
    });

    await step('round trip: the settings survive a reload and reach the export', async () => {
      await page.reload({ waitUntil: 'domcontentloaded' });
      await ready(page);
      await waitChip(page, 'Cell transmitter ~60 m', 'near');
      await openDisplay(page);
      assert(await page.inputValue('[data-cv2-masts="hintDistanceM"]') === '600');
      assert(await page.inputValue('[data-cv2-masts="hintStrongM"]') === '50');
      await page.click('.cust-tab[data-tab="export"]');
      await page.waitForSelector('.cust-panel[data-panel="export"].active', { timeout: 4000 });
      const json = JSON.parse(await page.inputValue('#cv2ExportJson'));
      assert(json.masts && json.masts.hintDistanceM === 600 && json.masts.hintStrongM === 50, 'export ' + JSON.stringify(json.masts));
      assert(!JSON.stringify(json).includes('cellRateLimitPerMin'), 'server config in the export');
      assert(rec.consoleErrors.length === 0, 'console errors: ' + rec.consoleErrors.join(' | '));
    });
    await ctx.close();
  }

  // ── 5. The zoom gate ────────────────────────────────────────────────────
  {
    const { ctx, page, rec } = await newCtx({ storage: { 'mc-masts': '1', [STORE]: JSON.stringify({ masts: { minZoom: 16 } }) } });
    await step('a raised zoom gate asks for no cell below it and says so; lowering it applies at once', async () => {
      await page.goto(`${BASE}/#/map?lat=56.43&lon=9.40&zoom=15`, { waitUntil: 'domcontentloaded' });
      await ready(page);
      await page.waitForTimeout(1200);
      const status = await page.evaluate(() => { const s = document.querySelector('.mc-masts-status'); return s && !s.hidden ? s.textContent : ''; });
      assert(status === 'Zoom in to 16+ to see antenna positions', 'status ' + JSON.stringify(status));
      assert(rec.masts.length === 0, 'cells asked below the gate: ' + rec.masts);
      await openDisplay(page);
      assert(await page.inputValue('[data-cv2-masts="minZoom"]') === '16');
      await setNum(page, 'minZoom', 15);
      await page.waitForFunction(() => { const s = document.querySelector('.mc-masts-status'); return !s || s.hidden || !/Zoom in/.test(s.textContent); }, null, { timeout: 10000 });
      const t0 = Date.now();
      while (!rec.masts.length && Date.now() - t0 < 10000) await page.waitForTimeout(100);
      assert(rec.masts.length > 0, 'the lowered gate fetched nothing');
      const s = await storedMasts(page);
      assert(s && s.minZoom === 15, 'stored ' + JSON.stringify(s));
      assert(rec.consoleErrors.length === 0, 'console errors: ' + rec.consoleErrors.join(' | '));
    });
    await ctx.close();
  }

  // ── 6. The sites overlay default ────────────────────────────────────────
  {
    const { ctx, page, rec } = await newCtx({ storage: { 'mc-masts': '1' } });
    const sitesOn = () => page.evaluate((label) => {
      const lab = Array.from(document.querySelectorAll('.leaflet-control-layers-overlays label')).find((l) => l.textContent.indexOf(label) >= 0);
      const box = lab && lab.querySelector('input');
      return box ? box.checked : null;
    }, ML.SITE_LABEL);
    await step('"Show antenna sites by default" off starts the next map without them; a link still wins', async () => {
      await page.goto(`${BASE}/#/map?lat=56.43&lon=9.40&zoom=15`, { waitUntil: 'domcontentloaded' });
      await ready(page);
      assert(await sitesOn() === true, 'the stored on was lost');
      await openDisplay(page);
      await page.click('label[for="cv2-masts-enabled"]');
      await page.waitForTimeout(600);
      const s = await storedMasts(page);
      assert(s && s.enabled === false, 'stored ' + JSON.stringify(s));
      assert(await page.evaluate(() => localStorage.getItem('mc-masts')) === null, 'the remembered choice was kept');
      await page.reload({ waitUntil: 'domcontentloaded' });
      await ready(page);
      assert(await sitesOn() === false, 'the sites overlay started on');
      await page.goto(`${BASE}/#/map?lat=56.43&lon=9.40&zoom=15&masts=1`, { waitUntil: 'domcontentloaded' });
      await page.reload({ waitUntil: 'domcontentloaded' });
      await ready(page);
      assert(await sitesOn() === true, 'masts=1 in the link lost');
      assert(rec.consoleErrors.length === 0, 'console errors: ' + rec.consoleErrors.join(' | '));
    });
    await ctx.close();
  }

  // ── 7. Not offered ──────────────────────────────────────────────────────
  {
    const { ctx, page, rec } = await newCtx({ masts: false });
    await step('without the masts block the section is absent', async () => {
      await page.goto(`${BASE}/#/map`, { waitUntil: 'domcontentloaded' });
      await ready(page);
      await openDisplay(page);
      assert(await page.$('[data-cv2-masts]') === null, 'masts inputs without the layer');
      const title = await page.evaluate(() => Array.from(document.querySelectorAll('.cust-section-title')).some((p) => p.textContent === 'Antenna Sites'));
      assert(!title, 'Antenna Sites title without the layer');
      assert(rec.consoleErrors.length === 0, 'console errors: ' + rec.consoleErrors.join(' | '));
    });
    await ctx.close();
  }

  await browser.close();
  console.log(`\n${passed}/${passed + failed} tests passed`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
