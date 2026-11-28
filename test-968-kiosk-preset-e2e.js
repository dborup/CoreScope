/**
 * E2E: the operator kiosk preset (#968 M1): config.json "kiosk" →
 * /api/config/client → #/live?kiosk=1.
 *
 * /api/config/client is intercepted and given a `kiosk` block (the fixture
 * config has none); /api/config/areas/polygons, /api/nodes/network-status
 * and /api/analytics/areas are stubbed and counted; the app socket is
 * replaced by page.routeWebSocket; reduced motion.
 *
 *   1. A bare #/live?kiosk=1 at 1920x1080 takes the preset: its two-area
 *      tour ("1 / 2", every 30 s), the light theme over a dark viewer, only
 *      the rate and records tiles, no network-status/analytics-areas
 *      request, and a 2 h remount. Nothing is saved.
 *   2. The same link with &tour=ccc&every=90&theme=dark: the link wins per
 *      key; the tiles stay the preset's.
 *   3. An empty &tour= in the link is a static view (no tour).
 *   4. The preset never turns kiosk on: plain #/live is normal Live.
 *   5. A cold kiosk link whose /api/config/client answers late (after the
 *      first paint): the theme switches to the preset's once, the tour is
 *      the preset's, and leaving kiosk brings the viewer's theme back.
 *   6. Without a kiosk block nothing changes: the built-in dark theme and
 *      all three tiles.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-968-kiosk-preset-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
if (!['localhost', '127.0.0.1', '[::1]'].includes(new URL(BASE).hostname)) throw new Error('local server only');

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + (e && e.stack || e)); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

// The fixture's nodes are around the Bay Area.
const AREAS = [
  { key: 'CCC', label: 'Kiosk Area C', latMin: 36.9, latMax: 37.1, lonMin: -122.15, lonMax: -121.9 },
  { key: 'AAA', label: 'Kiosk Area A', latMin: 37.2, latMax: 37.45, lonMin: -122.05, lonMax: -121.75 },
  { key: 'BBB', label: 'Kiosk Area B', polygon: [[37.70, -122.52], [37.83, -122.52], [37.83, -122.35], [37.70, -122.35]] },
];
// As the server serves it (resolved: every member present).
const PRESET = { tour: ['AAA', 'bbb'], everySeconds: 30, theme: 'light', tiles: ['rate', 'records'], remountHours: 2 };
const H = 3600000;

async function open(browser, o) {
  o = o || {};
  const ctx = await browser.newContext({ viewport: o.viewport || { width: 1920, height: 1080 }, reducedMotion: 'reduce' });
  const h = { ctx, errors: [], status: 0, areaStats: 0, clientCfg: 0 };
  await ctx.route(/\/api\/config\/areas\/polygons(\?|$)/, (r) => r.fulfill({ json: AREAS }));
  await ctx.route(/\/api\/nodes\/network-status(\?|$)/, (r) => { h.status++;
    return r.fulfill({ json: { total: 400, active: 318, degraded: 2, silent: 80, roleCounts: {} } }); });
  await ctx.route(/\/api\/analytics\/areas(\?|$)/, (r) => { h.areaStats++; return r.fulfill({ json: { density: [] } }); });
  await ctx.route(/\/api\/config\/client(\?|$)/, async (route) => {
    const res = await route.fetch();
    const body = await res.json();
    h.clientCfg++;
    // Moments on, so the records tile has its source.
    body.moments = { enabled: true, historySize: 50 };
    if (o.preset !== null) body.kiosk = o.preset || PRESET;
    if (o.delayMs) await new Promise((r) => setTimeout(r, o.delayMs));
    await route.fulfill({ response: res, json: body });
  });
  await ctx.route(/\/api\/moments(\?|$)/, (r) => r.fulfill({ json: { moments: [] } }));
  await ctx.routeWebSocket(/.*/, () => {});
  const page = await ctx.newPage();
  h.page = page;
  page.on('pageerror', (e) => h.errors.push(String(e.stack || e.message).split('\n').slice(0, 3).join(' | ')));
  h.kioskUp = () => page.waitForFunction(() => window._liveKiosk && window._liveKiosk(), null, { timeout: 20000 });
  h.liveReady = () => page.waitForFunction(() => window._liveGetMap && window._liveGetMap(), null, { timeout: 20000 });
  h.stats = () => page.evaluate(() => window._liveKiosk() && window._liveKiosk().stats());
  h.state = () => page.evaluate(() => {
    const l = document.querySelector('.live-kiosk-step-label'), c = document.querySelector('.live-kiosk-step-count');
    return {
      kiosk: document.body.classList.contains('kiosk'),
      theme: document.documentElement.getAttribute('data-theme'),
      saved: localStorage.getItem('meshcore-theme'),
      label: (l ? l.textContent : '') + '|' + (c ? c.textContent : ''),
      tiles: [...document.querySelectorAll('.live-kiosk-tile')].map((t) => t.getAttribute('data-tile')),
    };
  });
  // The viewer's own theme is dark, so a light preset is visible.
  h.seed = async () => {
    await page.goto(BASE + '/?r=' + Date.now() + '#/packets', { waitUntil: 'domcontentloaded' });
    await page.evaluate(() => { localStorage.setItem('meshcore-theme', 'dark'); localStorage.setItem('live-moments', 'false'); });
    await page.reload({ waitUntil: 'domcontentloaded' });
    await page.evaluate(() => window.MeshConfigReady);
  };
  h.go = async (hash) => {
    await page.evaluate((hash) => { location.hash = hash; }, hash);
    await h.liveReady();
  };
  h.leave = async () => {
    await page.evaluate(() => { location.hash = '#/packets'; });
    await page.waitForFunction(() => !document.body.classList.contains('kiosk') && !(window._liveKiosk && window._liveKiosk()), null, { timeout: 20000 });
  };
  return h;
}

(async () => {
  const browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_PATH || undefined });
  try {
    console.log('\n=== #968 M1 the operator kiosk preset E2E ===');
    {
      const h = await open(browser);
      const p = h.page;
      await h.seed();

      await step('a bare kiosk=1 link takes the preset: its tour, every, theme, tiles and remount interval', async () => {
        await h.go('#/live?kiosk=1');
        await h.kioskUp();
        await p.waitForFunction(() => window._liveKiosk().stats().steps === 2, null, { timeout: 20000 });
        const s = await h.stats();
        assert(s.every === 30, 'every: ' + s.every);
        assert(JSON.stringify(s.tiles) === '["rate","records"]', 'stats tiles: ' + JSON.stringify(s.tiles));
        assert(s.remountInMs <= 2 * H && s.remountInMs > 2 * H - 60000, 'remount in ' + s.remountInMs);
        const st = await h.state();
        assert(st.kiosk, 'body.kiosk');
        assert(st.label === 'Kiosk Area A|1 / 2', 'label: ' + st.label);
        assert(st.theme === 'light' && st.saved === 'dark', 'preset theme, nothing saved: ' + JSON.stringify(st));
        assert(JSON.stringify(st.tiles) === '["rate","records"]', 'tiles in the DOM: ' + JSON.stringify(st.tiles));
        assert(!(await p.$('.live-kiosk-tile-area')), 'no area part');
      });

      await step('no active tile: no network-status or analytics/areas request', async () => {
        await p.waitForTimeout(1500);
        assert(h.status === 0 && h.areaStats === 0, 'bulk reads: ' + h.status + ' + ' + h.areaStats);
        assert((await h.stats()).polls === 0, 'polls: ' + (await h.stats()).polls);
      });

      await step('the link wins per key: &tour=ccc&every=90&theme=dark; the tiles stay the preset\'s', async () => {
        await h.leave();
        await h.go('#/live?kiosk=1&tour=ccc&every=90&theme=dark');
        await h.kioskUp();
        await p.waitForFunction(() => window._liveKiosk().stats().steps === 1, null, { timeout: 20000 });
        const s = await h.stats();
        assert(s.every === 90, 'every: ' + s.every);
        const st = await h.state();
        assert(st.label === 'Kiosk Area C|', 'label: ' + st.label);
        assert(st.theme === 'dark', 'theme: ' + st.theme);
        assert(JSON.stringify(st.tiles) === '["rate","records"]', 'tiles: ' + JSON.stringify(st.tiles));
      });

      await step('an empty &tour= in the link is a static view, not the preset tour', async () => {
        await h.leave();
        await h.go('#/live?kiosk=1&tour=');
        await h.kioskUp();
        await p.waitForTimeout(500);
        const s = await h.stats();
        assert(s.steps === 0 && s.fetches === 0, 'no tour: ' + JSON.stringify({ steps: s.steps, fetches: s.fetches }));
        assert((await h.state()).theme === 'light', 'the preset theme still applies');
      });

      await step('the preset never turns kiosk on: plain #/live is normal Live with the viewer\'s theme', async () => {
        await h.leave();
        await h.go('#/live');
        await p.waitForTimeout(500);
        const st = await h.state();
        assert(!st.kiosk && st.theme === 'dark' && st.tiles.length === 0, JSON.stringify(st));
        assert(!(await p.evaluate(() => window._liveKiosk && window._liveKiosk())), 'no controller');
      });

      await step('no page errors', async () => { assert(!h.errors.length, h.errors.join('; ')); });
      await h.ctx.close();
    }

    await step('a cold link with a late /api/config/client: the preset theme once, its tour, and the viewer\'s theme back on exit', async () => {
      const h = await open(browser, { delayMs: 2500 });
      const p = h.page;
      await p.goto(BASE + '/?r=' + Date.now() + '#/packets', { waitUntil: 'domcontentloaded' });
      await p.evaluate(() => { localStorage.setItem('meshcore-theme', 'dark'); localStorage.setItem('live-moments', 'false'); });
      // Record each data-theme value from the first script on (an init
      // script runs before <html> exists, so it watches the document).
      await p.addInitScript(() => {
        window.__themes = [];
        new MutationObserver((list) => list.forEach((m) => {
          if (m.target === document.documentElement) window.__themes.push(m.target.getAttribute('data-theme'));
        })).observe(document, { attributes: true, subtree: true, attributeFilter: ['data-theme'] });
      });
      await p.goto(BASE + '/?r=' + Date.now() + '#/live?kiosk=1', { waitUntil: 'domcontentloaded' });
      await h.kioskUp();
      await p.waitForFunction(() => window._liveKiosk().stats().steps === 2, null, { timeout: 20000 });
      const st = await h.state();
      assert(st.theme === 'light' && st.saved === 'dark', JSON.stringify(st));
      assert((await h.stats()).every === 30, 'every');
      const seen = await p.evaluate(() => window.__themes.slice());
      assert(seen[seen.length - 1] === 'light' && seen.filter((t) => t === 'light').length === 1, 'one switch to light: ' + JSON.stringify(seen));
      await h.leave();
      const back = await h.state();
      assert(back.theme === 'dark' && back.saved === 'dark', 'viewer theme back: ' + JSON.stringify(back));
      assert(!h.errors.length, h.errors.join('; '));
      await h.ctx.close();
    });

    await step('without a kiosk block nothing changes: the dark theme and all three tiles', async () => {
      const h = await open(browser, { preset: null });
      const p = h.page;
      await h.seed();
      await p.evaluate(() => localStorage.setItem('meshcore-theme', 'light'));
      await p.reload({ waitUntil: 'domcontentloaded' });
      await h.go('#/live?kiosk=1&tour=aaa');
      await h.kioskUp();
      const st = await h.state();
      assert(st.theme === 'dark', 'theme: ' + st.theme);
      assert(JSON.stringify(st.tiles) === '["rate","active","records"]', 'tiles: ' + JSON.stringify(st.tiles));
      const s = await h.stats();
      assert(s.every === 60 && s.remountInMs > 5.9 * H, JSON.stringify({ every: s.every, r: s.remountInMs }));
      await p.waitForFunction(() => window._liveKiosk().stats().polls >= 1, null, { timeout: 10000 });
      assert(!h.errors.length, h.errors.join('; '));
      await h.ctx.close();
    });
  } finally {
    await browser.close();
  }
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
