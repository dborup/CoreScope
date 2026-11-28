#!/usr/bin/env node
/* Issue #740 M3b follow-up — one "selected boundary" layer on the Map.
 *
 * Against the real Go server (its real /api/config/dagi, /api/nodes and
 * /api/config/dagi/polygons). The fixture has no Area polygons configured,
 * so the two Area endpoints (/api/config/areas and /api/config/areas/polygons)
 * are fulfilled here with one Area near Aarhus; nothing else is mocked.
 *
 *  1. 1440x900, light and dark (plan §4.6, review F2): with an Area and a
 *     DAGI code both set, only the Area outline shows and no polygon is
 *     asked for; Area "All" brings the DAGI outline; the one "Selected area
 *     outline" checkbox and its storage key hide and show whichever outline
 *     is chosen, with no new request; the Area again replaces the DAGI one.
 *     Both outlines are in --geo-filter-color, and the DAGI credit is shown
 *     as text, never as HTML (review F3, mutants O3 and O4).
 *  2. 1440x600 (review F6): the popover beside the Map Controls panel stays
 *     level with its trigger when the panel scrolls.
 *  3. A reload of #/map?dkregion=1083 draws the outline again from the
 *     browser cache or a 304 (plan M3b, review F7): no second 200 download.
 *     This step routes nothing (routing turns the HTTP cache off).
 *
 * Defaults to localhost:13581 — NEVER point at prod (AGENTS.md). CI sets BASE_URL.
 * Run: BASE_URL=http://localhost:13581 node test-issue-740-m3b-boundary-layer-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const AREA = { key: 'e2e-aarhus-bay', label: 'E2E Aarhus Bay' };
const AREA_POLYGON = [[56.10, 10.15], [56.10, 10.30], [56.20, 10.30], [56.20, 10.15]];
const HOSTILE = '<b id="e2e-credit-html">bold</b>';

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
  console.log(`\n=== #740 M3b follow-up: one selected-boundary layer on the Map against ${BASE} ===\n`);
  const errors = [];

  async function cold(viewport, theme, init) {
    const ctx = await browser.newContext({ viewport });
    await ctx.addInitScript(({ t, s }) => {
      try {
        if (!sessionStorage.getItem('e2e-init')) {
          sessionStorage.setItem('e2e-init', '1');
          localStorage.setItem('meshcore-theme', t);
          Object.keys(s).forEach((k) => localStorage.setItem(k, s[k]));
        }
      } catch (_) {}
    }, { t: theme || 'light', s: init || {} });
    const page = await ctx.newPage();
    page.setDefaultTimeout(15000);
    page.on('console', (m) => { if (m.type() === 'error' && !/tile|net::ERR|Failed to load resource/i.test(m.text())) errors.push(m.text()); });
    page.on('pageerror', (e) => errors.push(String(e)));
    const polygons = [];
    page.on('request', (r) => {
      const u = new URL(r.url());
      if (u.pathname === '/api/config/dagi/polygons') polygons.push(u.search.slice(1));
    });
    return { ctx, page, polygons };
  }
  async function mapLoaded(page) {
    await page.waitForFunction(() => { const l = document.getElementById('leaflet-map'); return l && l.getAttribute('data-loaded') === 'true'; });
  }
  const count = (page) => page.evaluate(() => ({
    area: document.querySelectorAll('path.area-outline').length,
    dagi: document.querySelectorAll('path.dagi-outline').length,
  }));
  const waitCount = (page, area, dagi) => page.waitForFunction(({ a, d }) =>
    document.querySelectorAll('path.area-outline').length === a && document.querySelectorAll('path.dagi-outline').length === d,
  { a: area, d: dagi });
  const credit = (page) => page.evaluate(() => document.querySelector('.leaflet-control-attribution').textContent);
  // The colour the theme gives the boundary, and the stroke a path has.
  const strokes = (page) => page.evaluate(() => ({
    want: getComputedStyle(document.documentElement).getPropertyValue('--geo-filter-color').trim(),
    area: (document.querySelector('path.area-outline') || { getAttribute: () => null }).getAttribute('stroke'),
    dagi: (document.querySelector('path.dagi-outline') || { getAttribute: () => null }).getAttribute('stroke'),
  }));
  async function pickArea(page, key) {
    await page.click('#mapAreaFilter .region-dropdown-trigger');
    await page.click(`#mapAreaFilter [data-area="${key}"]`);
  }

  for (const theme of ['light', 'dark']) {
    await step(`1440x900 ${theme}: Area and DAGI share one outline layer, one checkbox and one colour; the DAGI credit is text`, async () => {
      const { ctx, page, polygons } = await cold({ width: 1440, height: 900 }, theme, { 'meshcore-area-filter': AREA.key });
      await page.route('**/api/config/areas', (r) => r.fulfill({ json: [AREA] }));
      await page.route('**/api/config/areas/polygons', (r) => r.fulfill({ json: [{ key: AREA.key, label: AREA.label, polygon: AREA_POLYGON }] }));
      // The real polygon answer, with markup in its credit.
      await page.route('**/api/config/dagi/polygons?*', async (r) => {
        const res = await r.fetch();
        const body = await res.json();
        body.credit = HOSTILE + ' ' + body.credit;
        await r.fulfill({ response: res, json: body });
      });
      await page.goto(BASE + '/#/map?kommune=0751', { waitUntil: 'domcontentloaded' });
      await mapLoaded(page);
      await waitCount(page, 1, 0);
      await page.waitForFunction(() => window.DagiFilter && DagiFilter.polygonQuery() === 'layer=kommune&code=0751');
      await page.waitForTimeout(500);
      let c = await count(page);
      assert(c.area === 1 && c.dagi === 0, 'Area and DAGI set: ' + JSON.stringify(c));
      assert(polygons.length === 0, 'the Area wins, yet a DAGI polygon was fetched: ' + polygons.join(' '));
      assert(await page.isVisible('#mcSelectedAreaLabel'), 'no outline checkbox');
      assert(await page.isChecked('#mcSelectedArea'), 'the checkbox is not checked by default');
      let s = await strokes(page);
      assert(s.want && s.area === s.want, 'the Area outline is not --geo-filter-color: ' + JSON.stringify(s));
      assert(!/DAGI/.test(await credit(page)), 'a DAGI credit with no DAGI outline');

      // Area "All": the DAGI outline takes the layer.
      await pickArea(page, '__all__');
      await waitCount(page, 0, 1);
      assert(polygons.join(' ') === 'layer=kommune&code=0751', 'polygon requests: ' + polygons.join(' '));
      assert(await page.isVisible('#mcSelectedAreaLabel'), 'the checkbox went away with the Area');
      s = await strokes(page);
      assert(s.dagi === s.want, 'the DAGI outline is not --geo-filter-color: ' + JSON.stringify(s));
      const text = await credit(page);
      assert(text.includes(HOSTILE) && /Klimadatastyrelsen, DAGI/.test(text), 'the credit is not shown as text: ' + text);
      assert(await page.locator('#e2e-credit-html').count() === 0, 'the credit\'s markup was rendered as HTML');

      // The one checkbox and storage key hide and show the chosen outline.
      await page.uncheck('#mcSelectedArea');
      await waitCount(page, 0, 0);
      assert(await page.evaluate(() => localStorage.getItem('meshcore-map-selected-area-outline')) === 'false', 'not stored');
      assert(!/DAGI/.test(await credit(page)), 'the credit stayed with the outline hidden');
      await page.check('#mcSelectedArea');
      await waitCount(page, 0, 1);
      assert(polygons.length === 1, 'showing the outline again fetched it: ' + polygons.join(' '));
      await page.uncheck('#mcSelectedArea');
      await waitCount(page, 0, 0);

      // Hidden, the Area comes back hidden too; checked, it replaces the DAGI outline.
      await pickArea(page, AREA.key);
      await page.waitForTimeout(400);
      c = await count(page);
      assert(c.area === 0 && c.dagi === 0, 'an outline with the checkbox off: ' + JSON.stringify(c));
      await page.check('#mcSelectedArea');
      await waitCount(page, 1, 0);
      assert(!/DAGI/.test(await credit(page)), 'the DAGI credit stayed under the Area outline');
      assert(polygons.length === 1, 'polygon requests: ' + polygons.join(' '));
      await ctx.close();
    });
  }

  await step('1440x600: the popover beside the Map Controls panel follows its trigger when the panel scrolls', async () => {
    const { ctx, page } = await cold({ width: 1440, height: 600 }, 'light');
    await page.goto(BASE + '/#/map', { waitUntil: 'domcontentloaded' });
    await mapLoaded(page);
    await page.waitForSelector('#mapDagiFilter .dagi-trigger', { state: 'visible' });
    // Scroll the trigger to the bottom of the panel, so the panel can scroll it up.
    await page.evaluate(() => document.querySelector('#mapDagiFilter .dagi-trigger').scrollIntoView({ block: 'end' }));
    await page.click('#mapDagiFilter .dagi-trigger');
    await page.waitForSelector('#mapDagiFilter .dagi-panel', { state: 'visible' });
    const where = () => page.evaluate(() => ({
      trigger: document.querySelector('#mapDagiFilter .dagi-trigger').getBoundingClientRect().top,
      panel: document.querySelector('#mapDagiFilter .dagi-panel').getBoundingClientRect().top,
      height: document.querySelector('#mapDagiFilter .dagi-panel').getBoundingClientRect().height,
      open: !document.querySelector('#mapDagiFilter .dagi-panel').hidden,
    }));
    const before = await where();
    const box = await page.locator('#mapControls').boundingBox();
    await page.mouse.move(box.x + box.width / 2, box.y + 40);
    await page.mouse.wheel(0, 300);
    await page.waitForFunction((t) => Math.abs(document.querySelector('#mapDagiFilter .dagi-trigger').getBoundingClientRect().top - t) > 50, before.trigger);
    await page.waitForTimeout(200);
    const after = await where();
    assert(after.open, 'the popover closed on a scroll');
    assert(after.trigger < before.trigger - 50, 'the panel did not scroll: ' + JSON.stringify({ before, after }));
    // Level with the trigger, unless the viewport clamps it (8 px margins).
    const want = Math.max(8, Math.min(after.trigger, 600 - 8 - after.height));
    assert(Math.abs(after.panel - want) <= 1, 'the popover did not follow its trigger: ' + JSON.stringify({ before, after, want }));
    await ctx.close();
  });

  await step('a reload draws the outline again from the browser cache or a 304, not a second download', async () => {
    const { ctx, page } = await cold({ width: 1440, height: 900 }, 'light');
    const cdp = await ctx.newCDPSession(page);
    await cdp.send('Network.enable');
    const loads = [];
    const ids = new Map();
    cdp.on('Network.requestWillBeSent', (e) => { if (/\/api\/config\/dagi\/polygons\?/.test(e.request.url)) ids.set(e.requestId, { cached: false, status: 0 }); });
    cdp.on('Network.requestServedFromCache', (e) => { if (ids.has(e.requestId)) ids.get(e.requestId).cached = true; });
    cdp.on('Network.responseReceived', (e) => {
      const r = ids.get(e.requestId);
      if (!r) return;
      r.status = e.response.status;
      if (e.response.fromDiskCache || e.response.fromMemoryCache) r.cached = true;
      loads.push(r);
    });
    await page.goto(BASE + '/#/map?dkregion=1083', { waitUntil: 'domcontentloaded' });
    await mapLoaded(page);
    await page.waitForFunction(() => document.querySelectorAll('path.dagi-outline').length === 1);
    await page.waitForTimeout(300);
    assert(loads.length === 1 && loads[0].status === 200 && !loads[0].cached, 'first visit: ' + JSON.stringify(loads));
    await page.reload({ waitUntil: 'domcontentloaded' });
    await mapLoaded(page);
    await page.waitForFunction(() => document.querySelectorAll('path.dagi-outline').length === 1);
    await page.waitForTimeout(300);
    assert(loads.length === 2, 'loads: ' + JSON.stringify(loads));
    assert(loads[1].cached || loads[1].status === 304, 'the reload downloaded the polygon again: ' + JSON.stringify(loads[1]));
    console.log('    (reload: ' + (loads[1].cached ? 'served from the browser cache' : 'revalidated, 304') + ')');
    await ctx.close();
  });

  await step('no console errors', async () => { assert(!errors.length, errors.slice(0, 3).join(' | ')); });

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
