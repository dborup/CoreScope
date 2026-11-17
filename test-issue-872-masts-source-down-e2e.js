/**
 * E2E (#872): the antenna sites layer while Mastedatabasen is down.
 *
 * The CI server has no map.masts config, so /api/config/client is
 * intercepted and given the masts block, and /api/masts is a stub that
 * answers like the server's open circuit breaker (cmd/server/masts_breaker.go):
 * 503, X-CoreScope-Masts-Source: down, Retry-After 300, for every cell but
 * one "cached" cell, which answers 200 with a site. Neither the proxy nor
 * Mastedatabasen is ever contacted; the breaker itself is covered by
 * cmd/server/masts_breaker_872_test.go against a fake WFS.
 *
 * Asserts:
 *   1. desktop 1280x800, zoom 14: the status line says the source is not
 *      responding (not "Loading antenna sites…"), every cell is asked
 *      exactly once over several seconds (no retry storm), and the cached
 *      cell's site is still drawn;
 *   2. phone 375x560 (light and dark), real taps (touch emulation): a tap
 *      on the layers button opens the list, and the status line is on
 *      screen, inside the list, not covered, and readable (its text colour
 *      differs from its background);
 *   3. zoom 12 still shows the zoom hint, not the source-down text;
 *   4. no console errors.
 *
 * Usage:
 *   BASE_URL=http://localhost:13872 node test-issue-872-masts-source-down-e2e.js
 * Optional: SCREENSHOT_DIR=<dir> writes the phone status line.
 */
'use strict';
const { chromium } = require('playwright');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const BASE = process.env.BASE_URL || 'http://localhost:13872';
const SHOT_DIR = process.env.SCREENSHOT_DIR || '';
const MASTS = {
  enabled: true, url: '/api/masts', minZoom: 13, cellSizeM: 2000,
  attribution: 'Antenna positions: Mastedatabasen, Digitaliseringsstyrelsen (CC0)',
  sourceUrl: 'https://mastedatabasen.dk/', cellExtent: [219, 3017, 462, 3209],
};
const DOWN_TEXT = 'Antenna data source (Mastedatabasen) is not responding — try again later';
const VIEW = { lat: 56.43, lon: 9.40 };
const LIST = '.leaflet-control-layers-list';
const STATUS = '.mc-masts-status';
const PNG_1X1 = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg==', 'base64');

const ML = (() => {
  const ctx = { window: {}, Math, Map, Set, Promise, Object, Array, Number, String, isFinite };
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'public', 'masts-layer.js'), 'utf8'), ctx);
  return ctx.window.MastsLayer;
})();
// The "cached" cell: the one under the view's centre.
const CACHED = (() => { const p = ML.toUTM32(VIEW.lat, VIEW.lon); return [Math.floor(p.e / 2000), Math.floor(p.n / 2000)]; })();
const SITE = { lat: VIEW.lat, lon: VIEW.lon, street: 'Hedevej', postcode: '8800', city: 'Viborg', stations: ['S1'],
  entries: [{ technology: 'LTE', bandMHz: 800, service: 'Mobiltelefoni', interference: true }], interference: true, likelyMast: false };

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
  console.log(`\n=== #872 antenna sites while the source is down, against ${BASE} ===\n`);

  async function newCtx({ viewport, touch = false, theme = 'light' }) {
    const ctx = await browser.newContext({ viewport, hasTouch: touch, isMobile: touch, colorScheme: theme });
    const rec = { masts: [], errors: [] };
    await ctx.route(BASE + '/api/config/client', async (route) => {
      const response = await route.fetch();
      const config = await response.json();
      config.masts = MASTS;
      await route.fulfill({ response, json: config });
    });
    await ctx.route((u) => new URL(u).pathname === MASTS.url, async (route) => {
      const cell = new URL(route.request().url()).searchParams.get('cell') || '';
      rec.masts.push(cell);
      const [e, n] = cell.split(',').map(Number);
      if (e === CACHED[0] && n === CACHED[1]) {
        await route.fulfill({ status: 200, json: { cell: { e, n }, truncated: false, sites: [SITE] } });
        return;
      }
      await route.fulfill({
        status: 503,
        headers: { 'Retry-After': '300', 'X-CoreScope-Masts-Source': 'down', 'Cache-Control': 'no-store' },
        json: { error: 'antenna data source unavailable' },
      });
    });
    await ctx.route(/^https?:\/\/(?!localhost|127\.0\.0\.1)/i, async (route) => {
      if (route.request().resourceType() === 'image') {
        await route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 });
        return;
      }
      await route.abort();
    });
    await ctx.addInitScript((t) => {
      try {
        localStorage.setItem('meshcore-theme', t);
        ['row-swipe', 'tab-swipe', 'edge-drawer', 'pull-refresh']
          .forEach((k) => localStorage.setItem('meshcore-gesture-hints-' + k, 'seen'));
      } catch (_) {}
      const apply = () => document.documentElement && document.documentElement.setAttribute('data-theme', t);
      apply();
      document.addEventListener('DOMContentLoaded', apply);
      let _L;
      Object.defineProperty(window, 'L', {
        configurable: true,
        get() { return _L; },
        set(v) {
          _L = v;
          try { v.Map.addInitHook(function () { (window.__mcMaps = window.__mcMaps || []).push(this); }); } catch (_) {}
        },
      });
    }, theme);
    const page = await ctx.newPage();
    page.on('console', (m) => {
      // The stubbed 503s are expected network errors, not page errors.
      if (m.type() === 'error' && !/status of 503/.test(m.text())) rec.errors.push(m.text());
    });
    page.on('pageerror', (e) => rec.errors.push(String(e)));
    return { ctx, page, rec };
  }

  async function gotoMap(page, zoom) {
    await page.goto(`${BASE}/#/map?lat=${VIEW.lat}&lon=${VIEW.lon}&zoom=${zoom}`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('.leaflet-control-layers-toggle', { timeout: 20000 });
    await page.evaluate(() => window.MeshConfigReady).catch(() => {});
    await page.waitForSelector(STATUS, { state: 'attached', timeout: 10000 });
    await page.waitForTimeout(1500);
  }
  const statusText = (page) => page.$eval(STATUS, (el) => (el.hidden ? '' : el.textContent));
  const drawn = (page) => page.evaluate(() => {
    const m = (window.__mcMaps || []).filter((x) => x._mcMasts).pop();
    let n = 0;
    m._mcMasts.groups.sites.eachLayer((g) => g.eachLayer((mk) => { if (m.hasLayer(mk)) n++; }));
    return n;
  });

  await step('desktop z14: source-down text, every cell asked once, the cached cell still drawn', async () => {
    const { ctx, page, rec } = await newCtx({ viewport: { width: 1280, height: 800 } });
    try {
      await gotoMap(page, 14);
      await page.waitForFunction((t) => {
        const el = document.querySelector('.mc-masts-status');
        return el && !el.hidden && el.textContent === t;
      }, DOWN_TEXT, { timeout: 10000 });
      const first = rec.masts.length;
      assert(first > 4, 'only ' + first + ' cells asked');
      await page.waitForTimeout(4000);
      assert(rec.masts.length === first, `${rec.masts.length - first} more requests while the source is down (retry storm)`);
      assert(new Set(rec.masts).size === rec.masts.length, 'a cell was asked twice');
      assert((await statusText(page)) === DOWN_TEXT, 'status ' + (await statusText(page)));
      assert((await drawn(page)) === 1, 'the cached cell\'s site is not drawn');
      assert(rec.errors.length === 0, 'console errors: ' + rec.errors.join(' | '));
    } finally { await ctx.close(); }
  });

  for (const theme of ['light', 'dark']) {
    await step(`phone 375x560 ${theme}: a real tap opens the layers, the source-down line is on screen and readable`, async () => {
      const { ctx, page, rec } = await newCtx({ viewport: { width: 375, height: 560 }, touch: true, theme });
      try {
        await gotoMap(page, 14);
        await page.waitForFunction((t) => {
          const el = document.querySelector('.mc-masts-status');
          return el && el.textContent === t;
        }, DOWN_TEXT, { timeout: 10000 });
        await page.tap('.leaflet-control-layers-toggle');
        await page.waitForSelector('.leaflet-control-layers-expanded', { timeout: 3000 });
        await page.waitForTimeout(400);
        // The line under the antenna rows: scrolled into view inside the
        // list as a finger would find it (the list is the scroll box).
        await page.$eval(STATUS, (el) => el.scrollIntoView({ block: 'nearest' }));
        const r = await page.evaluate(({ s, l }) => {
          const el = document.querySelector(s);
          const b = el.getBoundingClientRect();
          const box = document.querySelector(l).getBoundingClientRect();
          const top = document.elementFromPoint(b.left + Math.min(20, b.width / 2), b.top + b.height / 2);
          const cs = getComputedStyle(el);
          let bg = 'rgba(0, 0, 0, 0)';
          for (let p = el; p && /rgba\(0, 0, 0, 0\)|transparent/.test(bg); p = p.parentElement) bg = getComputedStyle(p).backgroundColor;
          return {
            text: el.textContent, hidden: el.hidden,
            left: b.left, right: b.right, top: b.top, bottom: b.bottom, width: b.width,
            inList: b.top >= box.top - 1 && b.bottom <= box.bottom + 1,
            uncovered: !!top && (top === el || el.contains(top)), on: top ? top.className : null,
            color: cs.color, bg, vw: innerWidth, vh: innerHeight,
          };
        }, { s: STATUS, l: LIST });
        assert(r.text === DOWN_TEXT && !r.hidden, 'status ' + JSON.stringify(r.text));
        assert(r.left >= 0 && r.right <= r.vw && r.top >= 0 && r.bottom <= r.vh, 'off screen ' + JSON.stringify(r));
        assert(r.inList, 'outside the visible list ' + JSON.stringify(r));
        assert(r.uncovered, 'covered by ' + r.on);
        assert(r.color !== r.bg, `text colour ${r.color} equals its background`);
        if (SHOT_DIR) {
          await page.screenshot({ path: path.join(SHOT_DIR, `masts-source-down-375x560-${theme}.png`) });
        }
        console.log(`      line ${Math.round(r.width)} px wide at y=${Math.round(r.top)}, ${r.color} on ${r.bg}`);
        assert(rec.errors.length === 0, 'console errors: ' + rec.errors.join(' | '));
      } finally { await ctx.close(); }
    });
  }

  await step('zoom 12: the zoom hint, not the source-down text, and no request', async () => {
    const { ctx, page, rec } = await newCtx({ viewport: { width: 1280, height: 800 } });
    try {
      await gotoMap(page, 12);
      assert(rec.masts.length === 0, rec.masts.length + ' requests at zoom 12');
      assert((await statusText(page)) === 'Zoom in to 13+ to see antenna positions', 'status ' + (await statusText(page)));
    } finally { await ctx.close(); }
  });

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
