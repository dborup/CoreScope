/**
 * E2E (#727 M1): shareable /p links for the Path profile.
 *
 * Same harness as test-issue-721-move-endpoints-e2e.js: the client config
 * gets a `dhm` block (profile: true, and share: true where a case needs
 * it), and /api/terrain/profile is answered by a stub that echoes the
 * request, so no token and no real elevation service is ever involved.
 * /p itself is served by the real server: with sharing off (the CI
 * default) it is the plain site document, and the handoff script in
 * index.html does the same job either way.
 *
 * Asserts:
 *   1. opening /p?profile=…&ha=9&hb=9 lands on #/map with the panel open
 *      and the profile measured once, with exactly those keys (an injected
 *      parameter and v are dropped);
 *   2. one document request, app.js loaded once, every same-origin asset
 *      answered below 400;
 *   3. Back goes where the user came from — /p leaves no history entry;
 *   4. with dhm.share the copy button hands out a /p link, and that link
 *      opens the same profile; without it, the #/map link as before;
 *   5. the raw /p HTML, fetched with a crawler User-Agent, carries the
 *      handoff script and Open Graph tags; /p/ is a 301 to /p;
 *   6. 360x740: the same landing on a phone;
 *   7. no console errors.
 *
 * Usage:
 *   BASE_URL=http://localhost:13700 node test-issue-727-share-link-e2e.js
 * Optional: SCREENSHOT_DIR=<dir> writes the PR screenshots.
 */
'use strict';
const { chromium } = require('playwright');
const path = require('path');

const BASE = process.env.BASE_URL || 'http://localhost:13700';
const SHOT_DIR = process.env.SCREENSHOT_DIR || '';
const PROFILE_PATH = '/api/terrain/profile';
const ATTRIBUTION = 'Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel, CC BY 4.0';
const DHM_BLOCK = {
  enabled: true, url: '/api/map/dhm/wms', layer: 'dhm_terraen_skyggekort',
  minZoom: 7, maxZoom: 19, profile: true,
  bounds: [[54.4265, 7.99125], [57.778, 15.5995]],
  attribution: ATTRIBUTION,
};
const PNG_1X1 = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGM4ceIEAAS0AlkWLoFAAAAAAElFTkSuQmCC',
  'base64');

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

/* The stub answers the shape of a real GET /api/terrain/profile and echoes
 * the heights it was asked for, the way the server does. */
function profileFor(q) {
  const D = 12000;
  const end = (p, h) => {
    const node = /^[0-9a-f]{64}$/.test(p || '');
    const asked = q.get(h);
    return {
      lat: 37.7, lon: -122.2, kind: node ? 'node' : 'point', role: node ? 'repeater' : undefined,
      groundM: 40, antennaM: asked !== null ? Number(asked) : (node ? 10 : 2),
      antennaSource: asked !== null ? 'request' : (node ? 'role-default' : 'point-default'),
      assumed: asked === null,
    };
  };
  const samples = [];
  for (let i = 0; i < 60; i++) {
    const d = Math.round(D * i / 59);
    samples.push({ d, dtm: 30 + 18 * Math.sin(d / 900), dsm: 42 + 18 * Math.sin(d / 900) });
  }
  return {
    a: end(q.get('a'), 'ha'), b: end(q.get('b'), 'hb'),
    distanceM: D, stepM: 11.7, sampleCount: 1025, model: q.get('model') === 'dtm' ? 'dtm' : 'both',
    coverage: { state: 'full', pct: 100, firstGapM: null }, samples,
    physics: { freqMHz: 869.618, freqSource: 'firmware', lambdaM: 0.34474, k: 1.3333333333333333, kSource: 'default', f1MaxM: 32.14 },
    verdict: { state: 'clear', model: 'dtm', criterion: 0.6, worstClearanceM: 41.2, worstAtM: 5000, worstF1Fraction: 1.4 },
    attribution: ATTRIBUTION,
  };
}

// Two free points in the fixture's Bay Area view.
const A0 = '37.80000,-122.35000', B0 = '37.78000,-121.85000';
// v is a key /p accepts and ignores; the handoff drops it.
const SHARE = `/p?profile=${encodeURIComponent(A0 + ',' + B0)}&model=dtm&ha=9&hb=9&v=abc123`;
// An unlisted key: with sharing on the server answers 400 (with the plain
// site document); either way the handoff must keep it out of the hash.
const SHARE_EVIL = SHARE + '&evil=%3Cscript%3Ealert(1)%3C%2Fscript%3E';
const WANT_HASH = '#/map?profile=' + encodeURIComponent(A0 + ',' + B0) + '&model=dtm&ha=9&hb=9';
const PANEL = '[data-testid="tp-panel"]';

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #727 shareable /p links against ${BASE} ===\n`);

  async function newCtx(o) {
    o = o || {};
    const ctx = await browser.newContext({
      viewport: o.viewport || { width: 1440, height: 900 },
      hasTouch: !!o.touch, isMobile: !!o.touch,
    });
    const rec = { profile: [], consoleErrors: [], documents: [], appJs: 0, bad: [] };
    await ctx.route(BASE + '/api/config/client', async (route) => {
      const response = await route.fetch();
      const config = await response.json();
      config.dhm = Object.assign({}, DHM_BLOCK, o.share ? { share: true } : {});
      await route.fulfill({ response, json: config });
    });
    await ctx.route(url => new URL(url).pathname === PROFILE_PATH, async (route) => {
      const u = new URL(route.request().url());
      rec.profile.push(u);
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(profileFor(u.searchParams)) });
    });
    await ctx.route(/^https?:\/\/(?!localhost|127\.0\.0\.1)/i, async (route) => {
      await route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 });
    });
    await ctx.route(url => new URL(url).pathname === '/api/map/dhm/wms', async (route) => {
      await route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 });
    });
    await ctx.addInitScript(() => {
      try {
        localStorage.setItem('meshcore-theme', 'light');
        localStorage.setItem('meshcore-map-clustering', 'false');
        ['row-swipe', 'tab-swipe', 'edge-drawer', 'pull-refresh']
          .forEach(k => localStorage.setItem('meshcore-gesture-hints-' + k, 'seen'));
      } catch (_) {}
      window.__copied = [];
      try {
        Object.defineProperty(navigator, 'clipboard', { configurable: true, value: {
          writeText: (t) => { window.__copied.push(String(t)); return Promise.resolve(); },
        } });
      } catch (_) {}
    });
    const page = await ctx.newPage();
    page.on('console', m => { if (m.type() === 'error') rec.consoleErrors.push(m.text()); });
    page.on('pageerror', e => rec.consoleErrors.push(String(e)));
    page.on('request', r => {
      if (r.resourceType() === 'document') rec.documents.push(r.url());
      if (new URL(r.url()).pathname === '/app.js') rec.appJs++;
    });
    page.on('response', r => {
      if (r.url().startsWith(BASE) && r.status() >= 400) rec.bad.push(r.status() + ' ' + r.url());
    });
    return { ctx, page, rec };
  }

  async function waitForProfile(page, rec) {
    await page.waitForSelector('#leaflet-map[data-loaded="true"]', { timeout: 30000 });
    await page.waitForFunction((sel) => {
      const p = document.querySelector(sel);
      return !!(p && p.classList.contains('is-open'));
    }, PANEL, { timeout: 15000 });
    const t0 = Date.now();
    while (rec.profile.length === 0 && Date.now() - t0 < 10000) await page.waitForTimeout(100);
    await page.waitForTimeout(800); // a second request would show up here
  }

  // ── 1-3. Opening a /p link ─────────────────────────────────────────────
  {
    const { ctx, page, rec } = await newCtx();
    await page.goto(BASE + '/#/home', { waitUntil: 'domcontentloaded' });
    await page.waitForTimeout(500);
    rec.documents.length = 0; rec.appJs = 0;
    await page.goto(BASE + SHARE, { waitUntil: 'domcontentloaded' });
    await waitForProfile(page, rec);

    await step('/p lands on #/map with only the profile keys', async () => {
      const loc = await page.evaluate(() => ({ path: location.pathname, search: location.search, hash: location.hash }));
      assert(loc.path === '/' && loc.search === '', 'still at ' + loc.path + loc.search);
      assert(loc.hash === WANT_HASH, 'hash ' + loc.hash + ', want ' + WANT_HASH);
    });
    await step('the panel is open and the profile measured once, as linked', async () => {
      assert(rec.profile.length === 1, rec.profile.length + ' profile requests');
      const q = rec.profile[0].searchParams;
      assert(q.get('a') === A0 && q.get('b') === B0, 'ends ' + q.get('a') + ' ' + q.get('b'));
      assert(q.get('model') === 'dtm' && q.get('ha') === '9' && q.get('hb') === '9', 'query ' + q.toString());
      assert(!q.has('evil') && !q.has('v'), 'an unlisted key reached the request');
    });
    await step('one document, app.js once, every asset below 400', async () => {
      assert(rec.documents.length === 1 && new URL(rec.documents[0]).pathname === '/p', 'documents ' + rec.documents.join(' '));
      assert(rec.appJs === 1, 'app.js loaded ' + rec.appJs + ' times');
      assert(rec.bad.length === 0, rec.bad.join(', '));
    });
    if (SHOT_DIR) await page.screenshot({ path: path.join(SHOT_DIR, '727-p-landing-1440.png') });
    await step('Back goes where the user came from', async () => {
      await page.goBack({ waitUntil: 'domcontentloaded' });
      await page.waitForTimeout(500);
      const u = new URL(page.url());
      assert(u.pathname === '/' && u.hash === '#/home', 'back went to ' + page.url());
    });
    await step('no console errors', async () => {
      assert(rec.consoleErrors.length === 0, rec.consoleErrors.join(' | '));
    });
    await ctx.close();
  }

  // ── 1b. An injected parameter ──────────────────────────────────────────
  {
    const { ctx, page, rec } = await newCtx();
    const doc = await page.goto(BASE + SHARE_EVIL, { waitUntil: 'domcontentloaded' });
    await waitForProfile(page, rec);
    await step('an injected parameter reaches neither the hash nor the request', async () => {
      // 400 when the server's sharing is on, 200 (plain site) when off.
      assert([200, 400].includes(doc.status()), 'status ' + doc.status());
      const hash = await page.evaluate(() => location.hash);
      assert(hash === WANT_HASH, 'hash ' + hash);
      assert(rec.profile.length === 1 && !rec.profile[0].searchParams.has('evil'), 'request ' + rec.profile.map(String).join(' '));
      assert(!(await page.evaluate(() => document.querySelector('script:not([src])') && document.documentElement.outerHTML.includes('alert(1)'))), 'payload in the DOM');
    });
    await ctx.close();
  }

  // ── 4. The copy button ─────────────────────────────────────────────────
  for (const share of [true, false]) {
    const { ctx, page, rec } = await newCtx({ share });
    await page.goto(`${BASE}/#/map?profile=${A0},${B0}&model=dtm&ha=12&hb=4`, { waitUntil: 'domcontentloaded' });
    await waitForProfile(page, rec);
    await page.click('[data-testid="tp-copy-link"]');
    await page.waitForFunction(() => (document.querySelector('[data-testid="tp-copy-status"]') || {}).textContent === 'Link copied', null, { timeout: 3000 });
    const copied = await page.evaluate(() => window.__copied.slice());
    if (share) {
      await step('with dhm.share the copy button hands out a /p link', async () => {
        assert(copied.length === 1, copied.length + ' copies');
        const u = new URL(copied[0]);
        assert(u.origin === new URL(BASE).origin && u.pathname === '/p' && u.hash === '', 'copied ' + copied[0]);
        assert(u.searchParams.get('profile') === A0 + ',' + B0 && u.searchParams.get('ha') === '12', 'copied ' + copied[0]);
      });
      await step('the copied /p link opens the same profile', async () => {
        rec.profile.length = 0;
        await page.goto(copied[0], { waitUntil: 'domcontentloaded' });
        await waitForProfile(page, rec);
        const q = rec.profile[rec.profile.length - 1].searchParams;
        assert(q.get('a') === A0 && q.get('b') === B0 && q.get('model') === 'dtm' && q.get('ha') === '12' && q.get('hb') === '4', q.toString());
      });
    } else {
      await step('without dhm.share the copy button keeps the #/map link', async () => {
        assert(copied.length === 1 && new URL(copied[0]).pathname === '/' && new URL(copied[0]).hash.startsWith('#/map?profile='), 'copied ' + copied[0]);
      });
    }
    await ctx.close();
  }

  // ── 5. What a crawler gets ─────────────────────────────────────────────
  {
    const ctx = await browser.newContext();
    await step('a crawler gets HTML with the handoff script and the preview tags', async () => {
      const r = await ctx.request.get(BASE + SHARE, { headers: { 'User-Agent': 'Discordbot/2.0 (+https://discordapp.com)' } });
      assert(r.status() === 200, 'status ' + r.status());
      const body = await r.text();
      assert(body.includes('data-share-handoff'), 'no handoff script');
      assert(/<meta property="og:title" content="[^"]+">/.test(body) && /<meta property="og:image" content="[^"]+">/.test(body), 'no og tags');
      assert(!/__OG_|__BUST__/.test(body), 'unfilled placeholder');
      // A server with sharing on adds og:url and noindex for this page.
      const ogUrl = body.match(/<meta property="og:url" content="([^"]+)">/);
      if (ogUrl) {
        assert(ogUrl[1].replace(/&amp;/g, '&').startsWith(BASE + '/p?profile='), 'og:url ' + ogUrl[1]);
        assert(body.includes('<meta name="robots" content="noindex">'), 'og:url without noindex');
      }
      assert(!body.includes('<script>alert'), 'injected markup');
    });
    await step('/p/ is a 301 to /p with the query kept', async () => {
      const r = await ctx.request.get(BASE + '/p/?profile=' + encodeURIComponent(A0 + ',' + B0), { maxRedirects: 0 });
      assert(r.status() === 301, 'status ' + r.status());
      assert(r.headers().location === '/p?profile=' + encodeURIComponent(A0 + ',' + B0), 'location ' + r.headers().location);
    });
    await ctx.close();
  }

  // ── 6. Phone ───────────────────────────────────────────────────────────
  {
    const { ctx, page, rec } = await newCtx({ viewport: { width: 360, height: 740 }, touch: true });
    await page.goto(BASE + SHARE, { waitUntil: 'domcontentloaded' });
    await waitForProfile(page, rec);
    await step('360x740: /p lands on the open panel too', async () => {
      const hash = await page.evaluate(() => location.hash);
      assert(hash === WANT_HASH, 'hash ' + hash);
      assert(rec.profile.length === 1, rec.profile.length + ' profile requests');
      assert(rec.consoleErrors.length === 0, rec.consoleErrors.join(' | '));
    });
    if (SHOT_DIR) await page.screenshot({ path: path.join(SHOT_DIR, '727-p-landing-360.png') });
    await ctx.close();
  }

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed\n`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
