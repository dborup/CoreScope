#!/usr/bin/env node
/* Issue #740 M3b — the Danish areas (DAGI) filter on Packets, and the
 * selected area's outline on the Map.
 *
 * Against the real Go server (its real /api/config/dagi, /api/packets,
 * /api/nodes and /api/config/dagi/polygons). The fixture has no node in
 * Denmark, so filtered lists are empty; the live WebSocket rule is covered
 * with mocked frames by test-issue-740-m3b-packets-live-dagi-e2e.js.
 *
 *  1. Phone 375x560, light and dark, cold start, real taps, Packets: Filters,
 *     the DAGI trigger, a bottom sheet nothing covers, search "aar", tap
 *     Aarhus: kommune=0751 in the URL and on /api/packets, one member-set
 *     request, the ADVERT note; ✕ clears the param, the note and the filter.
 *  2. Phone 375x560, light and dark, cold start, real taps, Map: the controls,
 *     the trigger, a sheet above the bottom nav, search, tap Aarhus: one
 *     outline, one polygon request, the map fitted to Aarhus; a zoom asks for
 *     nothing; ✕ removes the outline and its credit.
 *  3. 1440x900, light and dark: a cold link with its own view draws the
 *     outline without moving the map; a keyboard pick in the popover (not
 *     clipped by the controls panel) replaces it and fits; a pan or zoom asks
 *     for nothing; ✕ removes it.
 *  4. Leaving and re-entering Map and Packets leaks no outline, control or
 *     window/document listener.
 *  5. Packets REST: a postnr link sends it; an unknown code is never sent;
 *     a same-tab link into Packets applies; the URL writer keeps the param.
 *
 * Screenshots for the PR's browser validation go to DAGI_SHOTS_DIR when set.
 * Defaults to localhost:13581 — NEVER point at prod (AGENTS.md). CI sets BASE_URL.
 * Run: BASE_URL=http://localhost:13581 node test-issue-740-m3b-dagi-packets-map-e2e.js
 */
'use strict';
const path = require('path');
const { chromium } = require('playwright');
const { tapWhenSettled } = require('./tests/e2e-tap');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const SHOTS = process.env.DAGI_SHOTS_DIR || '';
const DAGI = ['kommune', 'landsdel', 'dkregion', 'postnr'];
// Bounding boxes of the areas the steps pick, with a margin (from the
// polygon endpoint): the fitted map centre must fall inside.
const AARHUS = { s: 56.0, n: 56.4, w: 9.9, e: 10.5 };
const HJORRING = { s: 57.2, n: 57.65, w: 9.6, e: 10.5 };

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
  console.log(`\n=== #740 M3b DAGI on Packets and the Map outline against ${BASE} ===\n`);
  const errors = [];

  async function cold(viewport, touch, theme) {
    const ctx = await browser.newContext({ viewport, hasTouch: touch, isMobile: touch });
    await ctx.addInitScript((t) => {
      try {
        if (!sessionStorage.getItem('e2e-init')) {
          sessionStorage.setItem('e2e-init', '1');
          localStorage.setItem('meshcore-theme', t);
          ['row-swipe', 'tab-swipe', 'edge-drawer', 'pull-refresh']
            .forEach((k) => localStorage.setItem('meshcore-gesture-hints-' + k, 'seen'));
        }
      } catch (_) {}
    }, theme || 'light');
    const page = await ctx.newPage();
    page.setDefaultTimeout(15000);
    page.on('console', (m) => { if (m.type() === 'error' && !/tile|net::ERR|Failed to load resource/i.test(m.text())) errors.push(m.text()); });
    page.on('pageerror', (e) => errors.push(String(e)));
    const reqs = { packets: [], members: [], polygons: [], dagi: [], mapNodes: [] };
    page.on('request', (r) => {
      const u = new URL(r.url());
      const d = dagiOf(u);
      if (d) reqs.dagi.push(u.pathname + '?' + d);
      if (u.pathname === '/api/packets') reqs.packets.push(u);
      if (u.pathname === '/api/nodes' && d && u.searchParams.get('geoFilter') === '0') reqs.members.push(u);
      if (u.pathname === '/api/config/dagi/polygons') reqs.polygons.push(u.search.slice(1));
      if (u.pathname === '/api/nodes' && u.searchParams.has('lastHeard')) reqs.mapNodes.push(u);
    });
    return { ctx, page, reqs };
  }
  const dagiOf = (u) => DAGI.filter((p) => u.searchParams.has(p)).map((p) => p + '=' + u.searchParams.get(p)).join('&');
  async function shot(page, name) { if (SHOTS) await page.screenshot({ path: path.join(SHOTS, 'm3b-' + name + '.png') }); }
  const label = (page, sel) => page.evaluate((s) => document.querySelector(s + ' .dagi-trigger-label').textContent, sel);
  const outlines = (page) => page.locator('path.dagi-outline').count();
  const credit = (page) => page.evaluate(() => document.querySelector('.leaflet-control-attribution').textContent);
  const mapView = (page) => page.evaluate(() => JSON.parse(localStorage.getItem('map-view') || 'null'));
  const inBox = (v, b) => v && v.lat > b.s && v.lat < b.n && v.lng > b.w && v.lng < b.e;
  async function packetsLoaded(page) {
    await page.waitForFunction(() => { const l = document.getElementById('pktLeft'); return l && l.getAttribute('data-loaded') === 'true'; });
  }
  async function mapLoaded(page) {
    await page.waitForFunction(() => { const l = document.getElementById('leaflet-map'); return l && l.getAttribute('data-loaded') === 'true'; });
  }
  // The outline's box on screen against the map's: how much of the map it
  // spans, 0 when it is off the visible map.
  const outlineCover = (page) => page.evaluate(() => {
    const p = document.querySelector('path.dagi-outline').getBoundingClientRect();
    const m = document.getElementById('leaflet-map').getBoundingClientRect();
    const w = Math.max(0, Math.min(p.right, m.right) - Math.max(p.left, m.left));
    const h = Math.max(0, Math.min(p.bottom, m.bottom) - Math.max(p.top, m.top));
    return { w: w / m.width, h: h / m.height };
  });
  // #740 M3b follow-up (review F1): what is on top of the fitted outline.
  // A 9x9 grid over the outline's box on the visible map; each point must
  // hit the map, never the Map Controls panel or its toggle.
  const outlineHidden = (page) => page.evaluate(() => {
    const p = document.querySelector('path.dagi-outline').getBoundingClientRect();
    const m = document.getElementById('leaflet-map').getBoundingClientRect();
    const l = Math.max(p.left, m.left), r = Math.min(p.right, m.right);
    const t = Math.max(p.top, m.top), b = Math.min(p.bottom, m.bottom);
    const out = [];
    for (let i = 1; i <= 9; i++) for (let j = 1; j <= 9; j++) {
      const x = l + (r - l) * i / 10, y = t + (b - t) * j / 10;
      const e = document.elementFromPoint(x, y);
      if (!e || !e.closest('#leaflet-map') || e.closest('#mapControls, .map-controls-toggle')) out.push(Math.round(x) + ',' + Math.round(y));
    }
    return out;
  });
  // A bottom sheet inside the viewport that nothing covers: the element at
  // its corners and centre is the sheet itself.
  async function sheetOnTop(page, sel, vp) {
    vp = vp || { width: 375, height: 560 };
    const r = await page.evaluate((s) => {
      const p = document.querySelector(s + ' .dagi-panel');
      const b = p.getBoundingClientRect();
      const pts = [[b.left + 4, b.top + 4], [b.right - 4, b.top + 4], [b.left + 4, b.bottom - 4], [b.right - 4, b.bottom - 4], [b.left + b.width / 2, b.top + b.height / 2]];
      return { box: [b.left, b.top, b.right, b.bottom], pos: getComputedStyle(p).position, sw: document.documentElement.scrollWidth,
        covered: pts.filter(([x, y]) => { const e = document.elementFromPoint(x, y); return !e || !p.contains(e); }).map((x) => x.map(Math.round).join(',')) };
    }, sel);
    assert(r.pos === 'fixed' && r.box[0] === 0 && r.box[2] === vp.width && Math.round(r.box[3]) === vp.height && r.box[1] > 0, 'not a bottom sheet: ' + JSON.stringify(r));
    assert(!r.covered.length, 'the sheet is covered at ' + r.covered.join(' '));
    assert(r.sw <= vp.width, 'sideways scroll: ' + r.sw);
  }
  // Tap the visible row of a search hit (the hit is marked for the tap).
  async function tapHit(page, sel, text) {
    const h = await page.evaluateHandle(({ s, t }) => Array.from(document.querySelectorAll(s + ' .dagi-option'))
      .find((o) => !o.hidden && o.offsetParent && o.textContent === t), { s: sel, t: text });
    await h.evaluate((o) => o.setAttribute('data-e2e', 'hit'));
    await tapWhenSettled(page, '[data-e2e="hit"]');
    await page.evaluate(() => { const e = document.querySelector('[data-e2e="hit"]'); if (e) e.removeAttribute('data-e2e'); });
  }
  async function searchOnPhone(page, sel, text) {
    await tapWhenSettled(page, sel + ' .dagi-search');
    await page.keyboard.type(text, { delay: 30 });
    await page.waitForFunction((s) => Array.from(document.querySelectorAll(s + ' .dagi-option')).some((o) => o.offsetParent && o.textContent === 'Aarhus'), sel);
  }

  for (const theme of ['light', 'dark']) {
    await step(`Packets, 375x560 ${theme}, cold start, real taps: pick Aarhus, then clear it`, async () => {
      const { ctx, page, reqs } = await cold({ width: 375, height: 560 }, true, theme);
      await page.goto(BASE + '/#/packets', { waitUntil: 'domcontentloaded' });
      await packetsLoaded(page);
      await page.waitForSelector('#packetsDagiFilter .dagi-trigger', { state: 'attached' });
      if (!(await page.locator('#packetsDagiFilter .dagi-trigger').isVisible())) {
        // At 375 px the Filters button is a mirror in the page header.
        await page.locator('#filterToggleBtn, .filter-toggle-btn-mirror').filter({ visible: true }).first()
          .evaluate((b) => b.setAttribute('data-e2e', 'filters'));
        await tapWhenSettled(page, '[data-e2e="filters"]');
      }
      assert((await label(page, '#packetsDagiFilter')) === 'Denmark: All', 'trigger: ' + await label(page, '#packetsDagiFilter'));
      // The dropdown row gained a control: it wraps, so Types and Channels
      // stay on the screen.
      const offscreen = await page.evaluate(() => Array.from(document.querySelectorAll('.filter-group-dropdowns > *'))
        .filter((c) => c.offsetParent && c.getBoundingClientRect().right > 375).map((c) => c.id || c.className));
      assert(!offscreen.length, 'off the screen: ' + offscreen.join(', '));
      await shot(page, `packets-phone-${theme}-1`);
      await tapWhenSettled(page, '#packetsDagiFilter .dagi-trigger');
      await page.waitForSelector('#packetsDagiFilter .dagi-panel', { state: 'visible' });
      await sheetOnTop(page, '#packetsDagiFilter');
      const search = await page.evaluate(() => document.querySelector('#packetsDagiFilter .dagi-search').getBoundingClientRect().right);
      assert(search <= 375, 'the search box runs off the sheet: right ' + search);
      await searchOnPhone(page, '#packetsDagiFilter', 'aar');
      await shot(page, `packets-phone-${theme}-2-search`);
      reqs.packets.length = 0;
      await tapHit(page, '#packetsDagiFilter', 'Aarhus');
      await page.waitForFunction(() => /[?&]kommune=0751(&|$)/.test(location.hash));
      await packetsLoaded(page);
      await page.waitForFunction(() => !document.getElementById('packetsDagiNotice').hidden);
      assert(reqs.packets.some((u) => dagiOf(u) === 'kommune=0751'), 'no /api/packets with kommune=0751: ' + reqs.packets.map(String).join(' '));
      assert(reqs.members.length === 1 && dagiOf(reqs.members[0]) === 'kommune=0751', 'member requests: ' + reqs.members.map(String).join(' '));
      assert((await label(page, '#packetsDagiFilter')) === 'Kommune: Aarhus', 'trigger: ' + await label(page, '#packetsDagiFilter'));
      const note = await page.evaluate(() => document.getElementById('packetsDagiNotice').textContent);
      assert(/only adverts are shown/.test(note) && /nodes have no position/.test(note), 'note: ' + note);
      assert(await page.evaluate(() => document.documentElement.scrollWidth <= 375), 'sideways scroll');
      await shot(page, `packets-phone-${theme}-3-filtered`);

      reqs.packets.length = 0;
      await tapWhenSettled(page, '#packetsDagiFilter .dagi-clear');
      await page.waitForFunction(() => !/kommune=/.test(location.hash));
      await packetsLoaded(page);
      assert(reqs.packets.length > 0 && reqs.packets.every((u) => !dagiOf(u)), 'after ✕: ' + reqs.packets.map(String).join(' '));
      assert(await page.evaluate(() => document.getElementById('packetsDagiNotice').hidden), 'note left after ✕');
      assert((await label(page, '#packetsDagiFilter')) === 'Denmark: All');
      assert(reqs.members.length === 1, 'a clear fetched a member set');
      await ctx.close();
    });

    for (const vp of [{ width: 375, height: 560 }, { width: 360, height: 640 }]) {
    await step(`Map, ${vp.width}x${vp.height} ${theme}, cold start, real taps: pick Aarhus (outline, fit, not under the controls), zoom, then clear it`, async () => {
      const { ctx, page, reqs } = await cold(vp, true, theme);
      await page.goto(BASE + '/#/map', { waitUntil: 'domcontentloaded' });
      await mapLoaded(page);
      await page.waitForSelector('#mapDagiFilter .dagi-trigger', { state: 'attached' });
      if (!(await page.locator('#mapDagiFilter .dagi-trigger').isVisible())) await tapWhenSettled(page, '#mapControlsToggle', { minHeight: 36 });
      assert((await outlines(page)) === 0, 'an outline with nothing selected');
      await tapWhenSettled(page, '#mapDagiFilter .dagi-trigger');
      await page.waitForSelector('#mapDagiFilter .dagi-panel', { state: 'visible' });
      await sheetOnTop(page, '#mapDagiFilter', vp);
      await shot(page, `map-phone-${vp.width}-${theme}-1-sheet`);
      await searchOnPhone(page, '#mapDagiFilter', 'aar');
      const t0 = Date.now();
      await tapHit(page, '#mapDagiFilter', 'Aarhus');
      await page.waitForFunction(() => document.querySelectorAll('path.dagi-outline').length === 1);
      const drawnMs = Date.now() - t0;
      await page.waitForFunction(() => /[?&]kommune=0751(&|$)/.test(location.hash));
      await page.waitForTimeout(600); // the fit's animation and moveend
      const view = await mapView(page);
      assert(inBox(view, AARHUS), 'not fitted to Aarhus: ' + JSON.stringify(view));
      const cover = await outlineCover(page);
      assert(cover.w > 0.3 || cover.h > 0.3, 'the outline is not in view: ' + JSON.stringify(cover));
      assert(reqs.polygons.length === 1 && reqs.polygons[0] === 'layer=kommune&code=0751', 'polygon requests: ' + reqs.polygons.join(' '));
      assert(/Klimadatastyrelsen, DAGI/.test(await credit(page)), 'no DAGI credit: ' + await credit(page));
      console.log(`    (${vp.width}x${vp.height} ${theme}: tap → outline drawn in ${drawnMs} ms)`);
      // The pick closed the controls panel: the fitted outline is in sight,
      // and the gear says so.
      assert(!(await page.locator('#mapControls').isVisible()), 'the Map Controls panel stayed open over the outline');
      assert(await page.getAttribute('#mapControlsToggle', 'aria-expanded') === 'false', 'the gear still says expanded');
      const under = await outlineHidden(page);
      assert(!under.length, under.length + ' of 81 points over the outline are covered: ' + under.slice(0, 5).join(' '));
      await shot(page, `map-phone-${vp.width}-${theme}-2-outline`);

      // A zoom asks for nothing; the outline stays one.
      await tapWhenSettled(page, '.leaflet-control-zoom-in', { minHeight: 30 });
      await page.waitForTimeout(600);
      assert(reqs.polygons.length === 1, 'a zoom fetched the polygon again: ' + reqs.polygons.join(' '));
      assert((await outlines(page)) === 1);

      if (!(await page.locator('#mapDagiFilter .dagi-clear').isVisible())) await tapWhenSettled(page, '#mapControlsToggle', { minHeight: 36 });
      await tapWhenSettled(page, '#mapDagiFilter .dagi-clear');
      await page.waitForFunction(() => document.querySelectorAll('path.dagi-outline').length === 0);
      assert(!/DAGI/.test(await credit(page)), 'the credit stayed after ✕: ' + await credit(page));
      assert(!/kommune=/.test(await page.evaluate(() => location.hash)), 'param left after ✕');
      assert(await page.evaluate((w) => document.documentElement.scrollWidth <= w, vp.width), 'sideways scroll');
      // A clear is no pick: the panel stays open.
      assert(await page.locator('#mapControls').isVisible(), 'a clear closed the controls panel');
      await ctx.close();
    });
    }
  }

  for (const theme of ['light', 'dark']) {
    await step(`Map, 1440x900 ${theme}: a cold link draws without a fit; a pick replaces and fits; pan/zoom ask for nothing; ✕ clears`, async () => {
      const { ctx, page, reqs } = await cold({ width: 1440, height: 900 }, false, theme);
      await page.goto(BASE + '/#/map?lat=40&lon=-100&zoom=5&dkregion=1083', { waitUntil: 'domcontentloaded' });
      await mapLoaded(page);
      await page.waitForFunction(() => document.querySelectorAll('path.dagi-outline').length === 1);
      await page.waitForTimeout(600);
      const cover = await outlineCover(page);
      assert(cover.w === 0 || cover.h === 0, 'a cold link moved the map to the area: ' + JSON.stringify(cover));
      const v0 = await mapView(page);
      assert(!v0 || (Math.abs(v0.lat - 40) < 1 && Math.abs(v0.lng + 100) < 1), 'the link\'s view was not kept: ' + JSON.stringify(v0));
      assert(reqs.polygons.join(' ') === 'layer=region&code=1083', 'polygon requests: ' + reqs.polygons.join(' '));
      assert(reqs.mapNodes.length > 0 && reqs.mapNodes.every((u) => dagiOf(u) === 'dkregion=1083'), 'map nodes: ' + reqs.mapNodes.map(String).join(' '));
      assert(/Klimadatastyrelsen, DAGI/.test(await credit(page)), 'no DAGI credit: ' + await credit(page));
      assert((await label(page, '#mapDagiFilter')) === 'Region: Region Syddanmark');
      // The longest label still leaves the clear button inside the panel.
      const fit = await page.evaluate(() => ({ clear: document.querySelector('#mapDagiFilter .dagi-clear').getBoundingClientRect().right,
        panel: document.getElementById('mapControls').getBoundingClientRect().right }));
      assert(fit.clear <= fit.panel, 'the clear button runs out of the controls panel: ' + JSON.stringify(fit));

      await page.click('#mapDagiFilter .dagi-trigger');
      await page.waitForSelector('#mapDagiFilter .dagi-panel', { state: 'visible' });
      const pop = await page.evaluate(() => {
        const p = document.querySelector('#mapDagiFilter .dagi-panel').getBoundingClientRect();
        const c = document.getElementById('mapControls').getBoundingClientRect();
        const hit = document.elementFromPoint(p.left + p.width / 2, p.top + p.height / 2);
        return { left: p.left, right: p.right, top: p.top, bottom: p.bottom, controlsLeft: c.left, onTop: !!(hit && hit.closest('.dagi-panel')) };
      });
      assert(pop.onTop && pop.right <= pop.controlsLeft && pop.left >= 0 && pop.top >= 0 && pop.bottom <= 900, 'popover clipped or off screen: ' + JSON.stringify(pop));
      await page.keyboard.type('hjør');
      await page.keyboard.press('ArrowDown');
      await shot(page, `map-desktop-${theme}-popover`);
      await page.keyboard.press('Enter');
      await page.waitForFunction(() => /[?&]kommune=0860(&|$)/.test(location.hash));
      await page.waitForFunction(() => {
        const p = document.querySelector('path.dagi-outline');
        const m = document.getElementById('leaflet-map').getBoundingClientRect();
        if (!p) return false;
        const b = p.getBoundingClientRect();
        return b.left >= m.left - 1 && b.right <= m.right + 1 && b.width > m.width * 0.2;
      });
      await page.waitForTimeout(600);
      assert((await outlines(page)) === 1, 'more than one outline');
      assert(inBox(await mapView(page), HJORRING), 'not fitted to Hjørring: ' + JSON.stringify(await mapView(page)));
      assert(reqs.polygons.join(' ') === 'layer=region&code=1083 layer=kommune&code=0860', 'polygon requests: ' + reqs.polygons.join(' '));
      assert(dagiOf(reqs.mapNodes[reqs.mapNodes.length - 1]) === 'kommune=0860', 'the pick did not reload the nodes with its param');
      await shot(page, `map-desktop-${theme}-fitted`);

      const box = await page.locator('#leaflet-map').boundingBox();
      await page.mouse.move(box.x + 300, box.y + 300);
      await page.mouse.down();
      await page.mouse.move(box.x + 500, box.y + 450, { steps: 8 });
      await page.mouse.up();
      await page.mouse.wheel(0, -400);
      await page.waitForTimeout(800);
      assert(reqs.polygons.length === 2, 'a pan or zoom fetched a polygon: ' + reqs.polygons.join(' '));

      await page.click('#mapDagiFilter .dagi-clear');
      await page.waitForFunction(() => document.querySelectorAll('path.dagi-outline').length === 0);
      assert(!/DAGI/.test(await credit(page)), 'the credit stayed after ✕');
      await ctx.close();
    });
  }

  await step('leaving and re-entering Map and Packets leaks no outline, control or listener', async () => {
    const { ctx, page } = await cold({ width: 1440, height: 900 }, false);
    await page.goto(BASE + '/#/map?kommune=0751', { waitUntil: 'domcontentloaded' });
    await mapLoaded(page);
    await page.waitForFunction(() => document.querySelectorAll('path.dagi-outline').length === 1);
    const cdp = await ctx.newCDPSession(page);
    const scripts = new Map();
    cdp.on('Debugger.scriptParsed', (e) => scripts.set(e.scriptId, e.url));
    await cdp.send('Debugger.enable');
    // window and document listeners by script file. filter-ux.js is left
    // out: it adds a document click listener per Packets visit, before this
    // change too (the #740 M3a review's P2), and is not this change's code.
    async function listeners() {
      const out = { window: 0, document: 0, dagi: 0 };
      for (const expr of ['window', 'document']) {
        const { result } = await cdp.send('Runtime.evaluate', { expression: expr });
        const { listeners: l } = await cdp.send('DOMDebugger.getEventListeners', { objectId: result.objectId });
        l.forEach((x) => {
          const file = (scripts.get(x.scriptId) || '').replace(/[?#].*$/, '').split('/').pop();
          if (file === 'filter-ux.js') return;
          out[expr]++;
          if (/^dagi-/.test(file)) out.dagi++;
        });
      }
      return out;
    }
    let first = null;
    for (let i = 0; i < 4; i++) {
      await page.evaluate(() => { location.hash = '#/packets'; });
      await packetsLoaded(page);
      await page.waitForSelector('#packetsDagiFilter .dagi-trigger', { state: 'attached' });
      assert((await outlines(page)) === 0, 'an outline outlived the Map');
      assert(await page.locator('.dagi-filter-wrap').count() === 1, 'controls: ' + await page.locator('.dagi-filter-wrap').count());
      await page.evaluate(() => { location.hash = '#/map'; });
      await mapLoaded(page);
      await page.waitForFunction(() => document.querySelectorAll('path.dagi-outline').length === 1);
      assert(await page.locator('.dagi-filter-wrap').count() === 1, 'controls: ' + await page.locator('.dagi-filter-wrap').count());
      const now = await listeners();
      if (i === 0) first = now;
      else assert(now.window === first.window && now.document === first.document, `listeners grew: ${JSON.stringify(first)} → ${JSON.stringify(now)}`);
      assert(now.dagi === 0, 'a closed DAGI picker left ' + now.dagi + ' window/document listeners');
    }
    // A return to the Map is no pick: the map stays where the user left it.
    const cover = await outlineCover(page);
    assert(cover.w === 0 || cover.h === 0, 'a return to the Map fitted the area: ' + JSON.stringify(cover));
    await ctx.close();
  });

  await step('Packets REST: a postnr link is sent, with its member set; an unknown code is never sent and leaves the URL', async () => {
    const { ctx, page, reqs } = await cold({ width: 1440, height: 900 }, false);
    await page.goto(BASE + '/#/packets?postnr=8000', { waitUntil: 'domcontentloaded' });
    await packetsLoaded(page);
    assert(reqs.packets.length > 0 && reqs.packets.every((u) => dagiOf(u) === 'postnr=8000'), 'sent: ' + reqs.packets.map(String).join(' '));
    assert(reqs.members.length === 1 && dagiOf(reqs.members[0]) === 'postnr=8000', 'member requests: ' + reqs.members.map(String).join(' '));
    await ctx.close();
    const b = await cold({ width: 1440, height: 900 }, false);
    await b.page.goto(BASE + '/#/packets?timeWindow=60&kommune=9999', { waitUntil: 'domcontentloaded' });
    await packetsLoaded(b.page);
    assert(b.reqs.dagi.length === 0, 'an unknown code was sent: ' + b.reqs.dagi.join(' '));
    assert(await b.page.evaluate(() => location.hash) === '#/packets?timeWindow=60', 'hash: ' + await b.page.evaluate(() => location.hash));
    assert(await b.page.evaluate(() => document.getElementById('packetsDagiNotice').hidden), 'a note for an unknown code');
    await b.ctx.close();
  });

  await step('Packets: a same-tab link applies, and the URL writer keeps the param', async () => {
    const { ctx, page, reqs } = await cold({ width: 1440, height: 900 }, false);
    await page.goto(BASE + '/#/packets', { waitUntil: 'domcontentloaded' });
    await packetsLoaded(page);
    await page.evaluate(() => { location.hash = '#/map'; });
    await mapLoaded(page);
    reqs.packets.length = 0;
    await page.evaluate(() => { location.hash = '#/packets?kommune=0860'; });
    await packetsLoaded(page);
    await page.waitForFunction(() => !document.getElementById('packetsDagiNotice').hidden);
    assert(reqs.packets.some((u) => dagiOf(u) === 'kommune=0860'), 'sent: ' + reqs.packets.map(String).join(' '));
    await page.selectOption('#fTimeWindow', '60');
    await page.waitForFunction(() => /timeWindow=60/.test(location.hash));
    assert(/[?&]kommune=0860(&|$)/.test(await page.evaluate(() => location.hash)), 'the time window dropped the param: ' + await page.evaluate(() => location.hash));
    // A pinned hash bypasses every list filter, DAGI too: no note, no param.
    const pin = (await (await page.request.get(BASE + '/api/packets?limit=1')).json()).packets[0].hash;
    reqs.packets.length = 0;
    await page.evaluate((h) => { location.hash = '#/packets?hash=' + h + '&kommune=0860'; }, pin);
    await packetsLoaded(page);
    assert(reqs.packets.length > 0 && reqs.packets.every((u) => !dagiOf(u)), 'a pinned hash sent the DAGI param: ' + reqs.packets.map(String).join(' '));
    assert(await page.evaluate(() => document.getElementById('packetsDagiNotice').hidden), 'the note shows for a pinned hash');
    await ctx.close();
  });

  await step('no console errors', async () => { assert(!errors.length, errors.slice(0, 3).join(' | ')); });

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
