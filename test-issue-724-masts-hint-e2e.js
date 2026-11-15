/**
 * E2E (#724 M4): the interference hint in the Map page's Path profile.
 *
 * Like the other DHM and masts suites, the CI server has neither map.dhm
 * nor map.masts, so /api/config/client is intercepted and given both
 * blocks, /api/terrain/profile is answered with a synthetic profile and
 * /api/masts with fixed synthetic sites. Neither the proxy's upstream nor
 * Mastedatabasen nor any DHM service is ever contacted.
 *
 * Asserts:
 *   1. an end 320 m from an 800 MHz site: the chip "Cell transmitter
 *      ~320 m" in the title row with the other chips, the note under the
 *      detail line ("especially within 100 m"), hostile stub text inert;
 *      with both overlays off, only the cells around the two ends are
 *      asked for, each once, as /api/masts?cell=E,N;
 *   2. an end 40 m from a 900 MHz site: the strong tier;
 *   3. ends near only an 1800 MHz site, an 800 MHz site 520 m away and a
 *      band-less GSM-R site: no chip, no note;
 *   4. without the masts block: no chip and no /api/masts request;
 *   5. at 375 px the chip stays inside the panel (no horizontal overflow);
 *   6. leaving the Map page with a path open, several times, leaves no
 *      theme-refresh listener and no data-theme observer behind (#768
 *      review 1: clear() after map.remove() built a new masts controller);
 *   7. no console errors.
 *
 * Usage:
 *   BASE_URL=http://localhost:13581 node test-issue-724-masts-hint-e2e.js
 * Optional: SCREENSHOT_DIR=<dir> writes small JPG screenshots of the
 * profile with and without the hint.
 */
'use strict';
const { chromium } = require('playwright');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const SHOT_DIR = process.env.SCREENSHOT_DIR || '';
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
  enabled: true, url: MASTS_PATH, minZoom: 13, cellSizeM: 2000,
  attribution: 'Antenna positions: Mastedatabasen, Digitaliseringsstyrelsen (CC0)',
  sourceUrl: 'https://mastedatabasen.dk/', cellExtent: [219, 3017, 462, 3209],
};
const PNG_1X1 = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGM4ceIEAAS0AlkWLoFAAAAAAElFTkSuQmCC',
  'base64');
const PANEL = '[data-testid="tp-panel"]';
const CHIP = '[data-testid="tp-mast-hint"]';
const NOTE = '[data-testid="tp-mast-note"]';
const EVIL = '<img src=x onerror="window.__xss=1">';

// The page's own code, to place the stub sites and list the end cells.
const ctxJS = (() => {
  const ctx = { window: {}, Math, Map, Set, Promise, Object, Array, Number, String, isFinite };
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'public', 'masts-layer.js'), 'utf8'), ctx);
  ctx.MastsLayer = ctx.window.MastsLayer;
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'public', 'masts-hint.js'), 'utf8'), ctx);
  return ctx.window;
})();
const ML = ctxJS.MastsLayer, MH = ctxJS.MastsHint;

const R = 6371000;
const north = (ll, m) => [+(ll[0] + m / R * 180 / Math.PI).toFixed(7), ll[1]];
const fmt = (ll) => ll[0].toFixed(5) + ',' + ll[1].toFixed(5);
const cellOf = (ll) => { const p = ML.toUTM32(ll[0], ll[1]); return Math.floor(p.e / 2000) + ',' + Math.floor(p.n / 2000); };

// Viborg heath, far from the fixture's nodes.
const A1 = [56.43, 9.40], B1 = [56.44, 9.43];   // 1: A 320 m from an 800 MHz site
const A2 = [56.46, 9.50], B2 = [56.47, 9.52];   // 2: B 40 m from a 900 MHz site
const A3 = [56.50, 9.60], B3 = [56.51, 9.62];   // 3: nothing relevant within 500 m
const SITES = [
  { ll: north(A1, 320), entries: [{ technology: 'LTE' + EVIL, bandMHz: 800, interference: true }], interference: true, street: 'Hedevej' + EVIL },
  { ll: north(B2, 40), entries: [{ technology: 'GSM', bandMHz: 900, interference: true }], interference: true },
  { ll: north(A3, 30), entries: [{ technology: 'LTE', bandMHz: 1800, interference: false }], interference: false },
  { ll: north(A3, 520), entries: [{ technology: 'LTE', bandMHz: 800, interference: true }], interference: true },
  { ll: north(B3, 25), entries: [{ technology: 'GSM-R', interference: true }], interference: true },
];
const BY_CELL = {};
SITES.forEach((s, i) => {
  const key = cellOf(s.ll);
  (BY_CELL[key] = BY_CELL[key] || []).push({
    lat: s.ll[0], lon: s.ll[1], street: s.street, city: 'Viborg', stations: ['S' + i + EVIL],
    entries: s.entries, interference: s.interference, likelyMast: false,
  });
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
  console.log(`\n=== #724 M4 interference hint against ${BASE} ===\n`);

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
      rec.masts.push(u);
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

  async function openProfile(page, a, b) {
    await page.goto(`${BASE}/#/map?profile=${fmt(a)},${fmt(b)}`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#leaflet-map[data-loaded="true"]', { timeout: 30000 });
    await page.waitForFunction(() => {
      const c = document.querySelector('[data-testid="tp-verdict"]');
      return !!(c && c.getAttribute('data-state') && c.getAttribute('data-state') !== 'pending');
    }, null, { timeout: 15000 });
  }

  async function hintState(page) {
    return page.evaluate(([chipSel, noteSel]) => {
      const chip = document.querySelector(chipSel), note = document.querySelector(noteSel);
      const vis = (el) => !!(el && !el.hidden && el.getClientRects().length);
      return {
        shown: vis(chip), noteShown: vis(note),
        text: chip ? chip.textContent : null, tier: chip ? chip.getAttribute('data-tier') : null,
        inHead: !!(chip && chip.parentElement && chip.parentElement.classList.contains('tp-head')),
        beforeCopy: !!(chip && chip.nextElementSibling && chip.nextElementSibling.classList.contains('tp-copy')),
        note: note ? note.textContent : null,
        imgs: document.querySelectorAll('.tp-panel img').length,
        xss: window.__xss === 1,
      };
    }, [CHIP, NOTE]);
  }

  async function waitHint(page, want) {
    await page.waitForFunction(([sel, w]) => {
      const c = document.querySelector(sel);
      return w ? !!(c && !c.hidden && c.textContent === w) : true;
    }, [CHIP, want], { timeout: 10000 });
  }

  async function shot(page, name) {
    if (!SHOT_DIR) return;
    fs.mkdirSync(SHOT_DIR, { recursive: true });
    await page.screenshot({ path: path.join(SHOT_DIR, name), type: 'jpeg', quality: 60 });
  }

  // ── 1. Near an 800 MHz site, both overlays off ─────────────────────────
  {
    const { ctx, page, rec } = await newCtx({ storage: { 'mc-masts': '0', 'mc-masts-int': '0' } });
    await step('an end 320 m from an 800 MHz site shows the chip and its note', async () => {
      await openProfile(page, A1, B1);
      await waitHint(page, 'Cell transmitter ~320 m');
      const s = await hintState(page);
      assert(s.shown && s.tier === 'near', 'chip ' + JSON.stringify(s));
      assert(s.inHead && s.beforeCopy, 'chip not in the title row before the copy button');
      assert(s.noteShown && /^A: an 800 MHz cellular transmitter about 320 m away\./.test(s.note), 'note ' + s.note);
      assert(/especially within 100 m/.test(s.note) && !/most within/.test(s.note), 'wording ' + s.note);
      assert(!s.xss && s.imgs === 0, 'hostile stub text became markup');
      await shot(page, 'm4-hint-near-light.jpg');
    });
    await step('with both overlays off only the end cells are asked for, each once', async () => {
      await page.waitForTimeout(600);
      const want = new Set(MH.cellsNear(A1[0], A1[1], MASTS).concat(MH.cellsNear(B1[0], B1[1], MASTS)));
      const asked = rec.masts.map((u) => { assert(/^cell=\d+%2C\d+$/.test(u.search.slice(1)), 'query ' + u.search); return u.searchParams.get('cell'); });
      assert(asked.length === new Set(asked).size, 'a cell twice: ' + asked);
      assert(asked.every((k) => want.has(k)), 'asked beyond the end cells: ' + asked + ' vs ' + Array.from(want));
      assert(asked.length === want.size, 'asked ' + asked.length + ' of ' + want.size);
      const markers = await page.evaluate(() => document.querySelectorAll('.leaflet-overlay-pane canvas').length);
      assert(rec.consoleErrors.length === 0, 'console errors: ' + rec.consoleErrors.join(' | ') + ' (canvases ' + markers + ')');
    });
    await step('dark theme: the chip reads in the theme colour', async () => {
      await page.evaluate(() => document.documentElement.setAttribute('data-theme', 'dark'));
      const c = await page.evaluate((sel) => {
        const chip = document.querySelector(sel);
        const want = getComputedStyle(document.documentElement).getPropertyValue('--mast-interference').trim();
        return { color: getComputedStyle(chip).color, border: getComputedStyle(chip).borderTopColor, want };
      }, CHIP);
      assert(c.want && c.color === c.border, 'chip colours ' + JSON.stringify(c));
      await shot(page, 'm4-hint-near-dark.jpg');
    });
    await ctx.close();
  }

  // ── 2. Within 100 m: the strong tier, layer on ─────────────────────────
  {
    const { ctx, page, rec } = await newCtx({ storage: { 'mc-masts-int': '1' } });
    await step('an end 40 m from a 900 MHz site is the strong tier', async () => {
      await openProfile(page, A2, B2);
      await waitHint(page, 'Cell transmitter ~40 m');
      const s = await hintState(page);
      assert(s.tier === 'strong', 'tier ' + s.tier);
      assert(/B: a 900 MHz cellular transmitter about 40 m away, possibly on the same building or mast\./.test(s.note), 'note ' + s.note);
      const asked = rec.masts.map((u) => u.searchParams.get('cell'));
      assert(asked.length === new Set(asked).size, 'with the layer on a cell was asked twice: ' + asked);
      assert(rec.consoleErrors.length === 0, 'console errors: ' + rec.consoleErrors.join(' | '));
      await shot(page, 'm4-hint-strong-light.jpg');
    });
    await ctx.close();
  }

  // ── 3. Nothing relevant: no chip ───────────────────────────────────────
  {
    const { ctx, page, rec } = await newCtx({ storage: { 'mc-masts': '0' } });
    await step('1800 MHz at 30 m, 800 MHz at 520 m and a band-less GSM-R site: no hint', async () => {
      await openProfile(page, A3, B3);
      const want = new Set(MH.cellsNear(A3[0], A3[1], MASTS).concat(MH.cellsNear(B3[0], B3[1], MASTS)));
      const t0 = Date.now();
      while (new Set(rec.masts.map((u) => u.searchParams.get('cell'))).size < want.size && Date.now() - t0 < 10000) await page.waitForTimeout(100);
      await page.waitForTimeout(500);
      const s = await hintState(page);
      assert(!s.shown && !s.noteShown, 'hint shown: ' + JSON.stringify(s));
      assert(rec.consoleErrors.length === 0, 'console errors: ' + rec.consoleErrors.join(' | '));
      await shot(page, 'm4-no-hint-light.jpg');
    });
    await ctx.close();
  }

  // ── 4. Not offered ─────────────────────────────────────────────────────
  {
    const { ctx, page, rec } = await newCtx({ masts: false });
    await step('without the masts block: no chip and no /api/masts request', async () => {
      await openProfile(page, A1, B1);
      await page.waitForTimeout(800);
      const s = await hintState(page);
      assert(!s.shown, 'chip shown without the masts block');
      assert(rec.masts.length === 0, 'requests: ' + rec.masts.length);
      assert(rec.consoleErrors.length === 0, 'console errors: ' + rec.consoleErrors.join(' | '));
    });
    await ctx.close();
  }

  // ── 5. Phone ───────────────────────────────────────────────────────────
  {
    const { ctx, page, rec } = await newCtx({ viewport: { width: 375, height: 812 }, storage: { 'mc-masts': '0' } });
    await step('375 px: the chip stays inside the panel and the page does not scroll sideways', async () => {
      await openProfile(page, A1, B1);
      await waitHint(page, 'Cell transmitter ~320 m');
      const g = await page.evaluate(([chipSel, panelSel]) => {
        const c = document.querySelector(chipSel).getBoundingClientRect(), p = document.querySelector(panelSel).getBoundingClientRect();
        return { cl: c.left, cr: c.right, pl: p.left, pr: p.right, sw: document.documentElement.scrollWidth, vw: window.innerWidth };
      }, [CHIP, PANEL]);
      assert(g.cl >= g.pl - 0.5 && g.cr <= g.pr + 0.5, 'chip outside the panel: ' + JSON.stringify(g));
      assert(g.sw <= g.vw, 'horizontal overflow: ' + JSON.stringify(g));
      assert(rec.consoleErrors.length === 0, 'console errors: ' + rec.consoleErrors.join(' | '));
      await shot(page, 'm4-hint-phone-light.jpg');
    });
    await ctx.close();
  }

  // ── 6. No leak when the Map page is left with a path open ─────────────
  {
    const { ctx, page, rec } = await newCtx({ storage: { 'mc-masts': '0', 'mc-masts-int': '0' } });
    // Net (added − removed) theme-refresh listeners and data-theme
    // observers on <html>, counted from the first script on.
    await page.addInitScript(() => {
      const net = window.__themeNet = { listeners: 0, observers: 0 };
      const add = window.addEventListener, remove = window.removeEventListener;
      const fns = new Set();
      window.addEventListener = function (t, fn, o) {
        if (t === 'theme-refresh' && !fns.has(fn)) { fns.add(fn); net.listeners++; }
        return add.call(this, t, fn, o);
      };
      window.removeEventListener = function (t, fn, o) {
        if (t === 'theme-refresh' && fns.delete(fn)) net.listeners--;
        return remove.call(this, t, fn, o);
      };
      const MO = window.MutationObserver;
      window.MutationObserver = class extends MO {
        observe(target, opts) {
          if (!this.__theme && target === document.documentElement && opts && (opts.attributeFilter || []).includes('data-theme')) {
            this.__theme = true; net.observers++;
          }
          return super.observe(target, opts);
        }
        disconnect() {
          if (this.__theme) { this.__theme = false; net.observers--; }
          return super.disconnect();
        }
      };
    });
    await step('leaving the Map page with a path open leaks no theme listener or observer', async () => {
      await page.goto(`${BASE}/#/nodes`, { waitUntil: 'domcontentloaded' });
      await page.waitForTimeout(800);
      const base = await page.evaluate(() => Object.assign({}, window.__themeNet));
      const seen = [];
      for (let i = 0; i < 3; i++) {
        await page.evaluate((h) => { location.hash = h; }, `#/map?profile=${fmt(A1)},${fmt(B1)}`);
        await page.waitForSelector('#leaflet-map[data-loaded="true"]', { timeout: 30000 });
        await waitHint(page, 'Cell transmitter ~320 m');
        await page.evaluate(() => { location.hash = '#/nodes'; });
        await page.waitForFunction(() => !document.querySelector('#leaflet-map'), null, { timeout: 10000 });
        await page.waitForTimeout(300);
        const n = await page.evaluate(() => Object.assign({}, window.__themeNet));
        seen.push((n.listeners - base.listeners) + '/' + (n.observers - base.observers));
      }
      assert(seen.every((x) => x === '0/0'), 'net theme-refresh listeners / data-theme observers after each visit: ' + seen.join(', '));
      assert(rec.consoleErrors.length === 0, 'console errors: ' + rec.consoleErrors.join(' | '));
    });
    await ctx.close();
  }

  await browser.close();
  console.log(`\n${passed}/${passed + failed} tests passed`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
