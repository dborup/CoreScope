/**
 * E2E (#929): the Map's route-style views and the node heat map.
 *
 * The packet route (drawPacketRoute), the GPS trail (drawGPSTrail) and the
 * estimated nodes view (drawEstimatedNodes) hide the node overlays while
 * they are open. Asserts, for each of the three views with the heat map on:
 *   1. a node reload under the view raises no console error and leaves no
 *      heat canvas under it (before the fix: "Map load error: TypeError"
 *      from leaflet.heat's redraw on the removed layer);
 *   2. switching the heat map off and on under the view leaves no heat
 *      canvas under it (before the fix: the new layer was drawn over it);
 *   3. closing the view brings the heat map back, exactly one canvas
 *      (before the fix: none);
 *   4. with the heat map switched off under the view, closing it leaves the
 *      heat map off;
 *   5. leaving the Map with a route view open and coming back draws the
 *      heat map (the route-view state does not outlive the mount);
 *   6. at 375x560 (touch), real taps open the controls and switch the heat
 *      map off and on under the packet route (still no canvas); closing the
 *      route brings it back (one canvas);
 *   7. no console errors.
 *
 * Usage:  BASE_URL=http://localhost:13700 node test-issue-929-route-heat-layer-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
// A fixture packet with a resolvable path: #/map?packet= opens its route view.
const ROUTE_PACKET = 'e26ad9dfa1ac3f7e';
const GPS_TRAIL = { sender: 'e2e-929', points: [{ lat: 56.15, lon: 10.2 }, { lat: 56.17, lon: 10.22 }, { lat: 56.2, lon: 10.25 }] };
const VIEWS = [
  { name: 'packet route', route: 'map?packet=' + ROUTE_PACKET, close: 'Close route' },
  { name: 'GPS trail', route: 'map', close: 'Close path', trail: GPS_TRAIL },
  { name: 'estimated nodes', route: 'map?estimatedNodes=1', close: 'Close view' },
];

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

const STATE = () => {
  const l = window._meshcoreHeatLayer;
  return {
    layer: !!l,
    onMap: !!(l && l._map),
    canvases: document.querySelectorAll('canvas.leaflet-heatmap-layer').length,
    closeBtns: document.querySelectorAll('a[title^="Close "]').length,
    checked: document.getElementById('mcHeatmap').checked,
  };
};

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #929 route views and the heat map against ${BASE} ===\n`);
  const allErrors = [];

  async function open(view, o) {
    o = o || {};
    const ctx = await browser.newContext({
      viewport: o.viewport || { width: 1280, height: 800 },
      hasTouch: !!o.touch, isMobile: !!o.touch,
    });
    await ctx.addInitScript((d) => {
      try {
        localStorage.setItem('meshcore-map-heatmap', 'true');
        if (d.trail && !window.__trail929) {
          window.__trail929 = true;
          sessionStorage.setItem('map-gps-trail', JSON.stringify(d.trail));
        }
      } catch (e) {}
    }, { trail: view.trail || null });
    const page = await ctx.newPage();
    const errors = [];
    const err = (t) => { errors.push(t); allErrors.push(t); };
    page.on('console', (m) => { if (m.type() === 'error' && !/tile|net::ERR|Failed to load resource/i.test(m.text())) err(m.text()); });
    page.on('pageerror', (e) => err(String(e)));
    await page.goto(BASE + '/#/' + view.route, { waitUntil: 'domcontentloaded' });
    // The view is open over a built, hidden heat layer.
    await page.waitForFunction((t) => !!window._meshcoreHeatLayer && !!document.querySelector('a[title="' + t + '"]') &&
      !document.querySelector('canvas.leaflet-heatmap-layer'), view.close, { timeout: 15000 });
    return { page, errors, done: () => ctx.close() };
  }
  const state = (page) => page.evaluate(STATE);
  // A node reload, as an ADVERT over the websocket or a filter change does:
  // data-loaded is set again when the reload has run toggleHeatmap.
  async function reloadNodes(page) {
    await page.evaluate(() => {
      document.getElementById('leaflet-map').removeAttribute('data-loaded');
      document.getElementById('mcLastHeard').dispatchEvent(new Event('change'));
    });
    await page.waitForFunction(() => document.getElementById('leaflet-map').getAttribute('data-loaded') === 'true', null, { timeout: 15000 });
    await page.waitForTimeout(200);
  }
  const clickHeat = (page) => page.evaluate(() => document.getElementById('mcHeatmap').click());
  const clickClose = (page, t) => page.evaluate((x) => document.querySelector('a[title="' + x + '"]').click(), t);

  for (const view of VIEWS) {
    await step(view.name + ': a node reload under the view raises no error and draws no heat', async () => {
      const { page, errors, done } = await open(view);
      await reloadNodes(page);
      const s = await state(page);
      assert(!errors.length, 'console errors: ' + errors.slice(0, 2).join(' | '));
      assert(s.closeBtns === 1, 'the view closed');
      assert(s.layer && !s.onMap, 'heat layer ' + (s.layer ? 'on the map' : 'gone') + ' under the view');
      assert(s.canvases === 0, s.canvases + ' heat canvases under the view, want 0');
      // The reloaded layer is the one the close brings back.
      await clickClose(page, view.close);
      await page.waitForTimeout(200);
      const c = await state(page);
      assert(c.onMap && c.canvases === 1, 'after close: onMap ' + c.onMap + ', ' + c.canvases + ' canvases, want 1');
      assert(!errors.length, 'console errors: ' + errors.slice(0, 2).join(' | '));
      await done();
    });

    await step(view.name + ': switching the heat map off and on under the view draws no heat', async () => {
      const { page, errors, done } = await open(view);
      await clickHeat(page);
      await page.waitForTimeout(100);
      await clickHeat(page);
      await page.waitForTimeout(200);
      const s = await state(page);
      assert(s.checked, 'checkbox off');
      assert(s.layer && !s.onMap, 'heat layer ' + (s.layer ? 'on the map' : 'gone') + ' under the view');
      assert(s.canvases === 0, s.canvases + ' heat canvases under the view, want 0');
      assert(!errors.length, 'console errors: ' + errors.slice(0, 2).join(' | '));
      await done();
    });

    await step(view.name + ': closing the view brings the heat map back', async () => {
      const { page, errors, done } = await open(view);
      await clickClose(page, view.close);
      await page.waitForTimeout(200);
      const s = await state(page);
      assert(s.closeBtns === 0, 'the view is still open');
      assert(s.onMap, 'heat layer not on the map after close');
      assert(s.canvases === 1, s.canvases + ' heat canvases after close, want 1');
      assert(!errors.length, 'console errors: ' + errors.slice(0, 2).join(' | '));
      await done();
    });

    await step(view.name + ': with the heat map switched off under the view, closing it leaves heat off', async () => {
      const { page, errors, done } = await open(view);
      await clickHeat(page);
      await page.waitForTimeout(100);
      await clickClose(page, view.close);
      await page.waitForTimeout(200);
      const s = await state(page);
      assert(!s.checked && !s.layer, 'heat layer present with the checkbox off');
      assert(s.canvases === 0, s.canvases + ' heat canvases with the heat map off, want 0');
      assert(!errors.length, 'console errors: ' + errors.slice(0, 2).join(' | '));
      await done();
    });
  }

  await step('leaving the Map with a route view open and coming back draws the heat map', async () => {
    const { page, errors, done } = await open(VIEWS[0]);
    await page.evaluate(() => { location.hash = '#/nodes'; });
    await page.waitForFunction(() => !document.getElementById('leaflet-map'), null, { timeout: 10000 });
    await page.evaluate(() => { location.hash = '#/map'; });
    await page.waitForFunction(() => !!window._meshcoreHeatLayer && !!window._meshcoreHeatLayer._map &&
      document.querySelectorAll('canvas.leaflet-heatmap-layer').length === 1, null, { timeout: 15000 })
      .catch(() => { throw new Error('no heat map on the fresh Map'); });
    assert(!errors.length, 'console errors: ' + errors.slice(0, 2).join(' | '));
    await done();
  });

  await step('375x560 touch: real taps switch heat off and on under the packet route (no canvas); closing it brings heat back', async () => {
    const { page, errors, done } = await open(VIEWS[0], { viewport: { width: 375, height: 560 }, touch: true });
    if (await page.evaluate(() => document.getElementById('mapControls').classList.contains('collapsed'))) {
      await tap(page, '#mapControlsToggle');
      await page.waitForFunction(() => !document.getElementById('mapControls').classList.contains('collapsed'));
    }
    if (await page.evaluate(() => { const fs = document.getElementById('mcHeatmap').closest('fieldset.mc-section'); return !!(fs && fs.classList.contains('mc-collapsed')); })) {
      await tap(page, 'fieldset.mc-section:has(#mcHeatmap) > legend.mc-label');
    }
    await page.waitForFunction(() => { const r = document.getElementById('mcHeatmap').getBoundingClientRect(); return r.width > 0 && r.height > 0; });
    await tap(page, '#mcHeatmap');
    await page.waitForFunction(() => !document.getElementById('mcHeatmap').checked);
    await tap(page, '#mcHeatmap');
    await page.waitForFunction(() => document.getElementById('mcHeatmap').checked);
    await page.waitForTimeout(200);
    let s = await state(page);
    assert(s.layer && !s.onMap && s.canvases === 0, 'under the route: onMap ' + s.onMap + ', ' + s.canvases + ' canvases, want 0');
    // At this width the Map controls toggle sits over Close route (both are
    // top-right; it predates #929), so the close is a DOM click.
    await clickClose(page, 'Close route');
    await page.waitForFunction(() => !document.querySelector('a[title="Close route"]'), null, { timeout: 5000 });
    await page.waitForTimeout(200);
    s = await state(page);
    assert(s.onMap && s.canvases === 1, 'after Close route: onMap ' + s.onMap + ', ' + s.canvases + ' canvases, want 1');
    assert(!errors.length, 'console errors: ' + errors.slice(0, 2).join(' | '));
    await done();
  });

  await step('no console errors', async () => { assert(!allErrors.length, allErrors.slice(0, 3).join(' | ')); });

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();

// A real touch tap at the target's centre, after checking that the target
// (or its label) is the topmost element there, so nothing covers it.
async function tap(page, sel) {
  const loc = page.locator(sel).first();
  await loc.scrollIntoViewIfNeeded();
  const b = await loc.boundingBox();
  if (!b) throw new Error(sel + ' has no box');
  const x = Math.round(b.x + b.width / 2), y = Math.round(b.y + b.height / 2);
  const hit = await loc.evaluate((el, p) => {
    const top = document.elementFromPoint(p.x, p.y);
    return !!top && (el === top || el.contains(top) || (el.labels && Array.from(el.labels).some((l) => l.contains(top))));
  }, { x, y });
  if (!hit) throw new Error(sel + ' is covered at its tap point');
  await page.touchscreen.tap(x, y);
}
