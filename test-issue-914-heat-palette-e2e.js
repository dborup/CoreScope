/**
 * E2E (#914): the node heat map's own violet -> magenta palette, from theme
 * tokens, on Map and Live.
 *
 * Asserts, against the real pages and the real leaflet.heat layer:
 *   1. Map (light and dark): the layer's gradient is the --heat-N tokens and
 *      the rendered palette's top pixel is --heat-5 (so the canvas really
 *      uses it, not just the option); every stop is violet-magenta;
 *   2. Live: the same, through the same helper;
 *   3. a customizer change of --heat-5 rebuilds the layer without a reload
 *      (a window marker survives) and the new palette is drawn; clearing it
 *      brings the default back; Map and Live;
 *   4. the achromatopsia preset rebuilds it with its light ramp, the dark
 *      toggle switches it to its dark ramp, clearing restores the theme's;
 *      a customizer colour beats the preset and clearing it brings the
 *      preset back (the #1446 rule); a preset without a heat ramp (deut)
 *      leaves the layer alone; Map and Live;
 *   5. the dark-mode toggle rebuilds it with the dark tokens;
 *   6. after every rebuild above the page holds exactly one heat canvas (an
 *      old layer left under the new one goes red; #917 review finding 3);
 *   7. the Theme tab lists the five Heat map colour rows;
 *   8. at 375x560 (touch), from a map with the heat map off, real taps open
 *      the Map controls, the Display section and the Heat map checkbox; each
 *      target is the topmost element at its tap point; the layer comes with
 *      the tokens and goes again;
 *   9. a theme change with the heat map switched off leaves it off, on Map
 *      and Live (#917 delta review finding 4);
 *  10. Map: with a route view open, which hides the heat layer, a dark
 *      toggle rebuilds the layer with the dark tokens but leaves it off the
 *      map (#917 delta review finding 2);
 *  11. no console errors.
 *
 * Usage:  BASE_URL=http://localhost:13700 node test-issue-914-heat-palette-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
// A fixture packet with a resolvable path: #/map?packet= opens its route view.
const ROUTE_PACKET = 'e26ad9dfa1ac3f7e';
let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }
// The canvas gradient is 8-bit and premultiplied: the palette is within a few
// steps of the token, never a different colour.
const near = (a, b) => { const x = parseInt(a.slice(1), 16), y = parseInt(b.slice(1), 16); return [16, 8, 0].every((sh) => Math.abs(((x >> sh) & 255) - ((y >> sh) & 255)) <= 3); };
const hue = (h) => {
  const n = parseInt(h.slice(1), 16);
  const r = (n >> 16) / 255, g = ((n >> 8) & 255) / 255, b = (n & 255) / 255;
  const mx = Math.max(r, g, b), d = mx - Math.min(r, g, b);
  if (!d) return -1;
  let x = mx === r ? ((g - b) / d) % 6 : mx === g ? (b - r) / d + 2 : (r - g) / d + 4;
  x *= 60; return x < 0 ? x + 360 : x;
};

// Reads the live layer for a page: gradient option, palette ends, identity.
const READ = (global) => `(() => {
  const l = window.${global};
  if (!l || !l._heat || !l._heat._grad) return null;
  const g = l._heat._grad, cs = getComputedStyle(document.documentElement);
  const tok = [1, 2, 3, 4, 5].map((n) => cs.getPropertyValue('--heat-stop-' + n).trim());
  const px = (i) => '#' + [g[i], g[i + 1], g[i + 2]].map((v) => v.toString(16).padStart(2, '0')).join('');
  return { grad: l.options.gradient, tok, top: px(255 * 4), low: px(51 * 4), id: l._leaflet_id,
    canvases: document.querySelectorAll('canvas.leaflet-heatmap-layer').length };
})()`;

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #914 heat map palette against ${BASE} ===\n`);
  const errors = [];

  async function open(route, o) {
    o = o || {};
    const ctx = await browser.newContext({
      viewport: o.viewport || { width: 1280, height: 800 },
      colorScheme: o.dark ? 'dark' : 'light',
      hasTouch: !!o.touch, isMobile: !!o.touch,
    });
    await ctx.addInitScript((d) => {
      try {
        localStorage.setItem('meshcore-theme', d.dark ? 'dark' : 'light');
        localStorage.setItem('meshcore-map-heatmap', d.heat ? 'true' : 'false');
        localStorage.setItem('meshcore-live-heatmap', d.heat ? 'true' : 'false');
        localStorage.removeItem('meshcore-cb-preset');
      } catch (e) {}
    }, { dark: !!o.dark, heat: o.heat !== false });
    const page = await ctx.newPage();
    page.on('console', (m) => { if (m.type() === 'error' && !/tile|net::ERR|Failed to load resource/i.test(m.text())) errors.push(m.text()); });
    page.on('pageerror', (e) => errors.push(String(e)));
    await page.goto(BASE + '/#/' + route, { waitUntil: 'domcontentloaded' });
    return { ctx, page };
  }
  const waitLayer = (page, global) => page.waitForFunction(`!!(window.${global} && window.${global}._heat && window.${global}._heat._grad)`, null, { timeout: 15000 });
  const read = (page, global) => page.evaluate(READ(global));

  function checkTokens(r, label) {
    assert(r, label + ': no heat layer');
    const keys = Object.keys(r.grad).sort((a, b) => a - b);
    assert(keys.join() === '0.2,0.4,0.6,0.8,1', label + ': stops ' + keys);
    keys.forEach((k, i) => {
      assert(r.grad[k].toLowerCase() === r.tok[i].toLowerCase(), label + ': stop ' + k + ' ' + r.grad[k] + ' != token ' + r.tok[i]);
      const h = hue(r.tok[i]);
      assert(h >= 255 && h <= 325, label + ': token ' + r.tok[i] + ' hue ' + h.toFixed(0) + ' is not violet-magenta');
    });
    assert(near(r.top, r.tok[4]), label + ': drawn top colour ' + r.top + ' != --heat-5 ' + r.tok[4]);
    assert(r.canvases === 1, label + ': ' + r.canvases + ' heat canvases, want 1');
  }
  // A preset ramp: the layer and the drawn palette follow the --heat-stop-N
  // tokens, which must be the preset's ramp for the mode.
  function checkRamp(r, ramp, label) {
    assert(r, label + ': no heat layer');
    ['0.2', '0.4', '0.6', '0.8', '1'].forEach((k, i) => {
      assert(r.tok[i].toLowerCase() === ramp[i].toLowerCase(), label + ': token ' + (i + 1) + ' ' + r.tok[i] + ' != ' + ramp[i]);
      assert(r.grad[k].toLowerCase() === ramp[i].toLowerCase(), label + ': stop ' + k + ' ' + r.grad[k] + ' != ' + ramp[i]);
    });
    assert(near(r.top, ramp[4]), label + ': drawn top colour ' + r.top + ' != ' + ramp[4]);
    assert(r.canvases === 1, label + ': ' + r.canvases + ' heat canvases, want 1');
  }
  const darkToggle = (page) => page.evaluate(() => { (document.getElementById('darkModeToggle') || document.getElementById('darkModeCheckbox')).click(); });

  let lightTop;
  for (const dark of [false, true]) {
    await step('Map ' + (dark ? 'dark' : 'light') + ': layer gradient and drawn palette are the tokens', async () => {
      const { ctx, page } = await open('map', { dark });
      await waitLayer(page, '_meshcoreHeatLayer');
      const r = await read(page, '_meshcoreHeatLayer');
      checkTokens(r, 'Map');
      if (!dark) lightTop = r.top; else assert(!near(r.top, lightTop), 'dark tokens equal light');
      await ctx.close();
    });
  }

  await step('Live: layer uses the same tokens through the same helper', async () => {
    const { ctx, page } = await open('live');
    await waitLayer(page, '_meshcoreLiveHeatLayer');
    checkTokens(await read(page, '_meshcoreLiveHeatLayer'), 'Live');
    await ctx.close();
  });

  for (const [route, global] of [['map', '_meshcoreHeatLayer'], ['live', '_meshcoreLiveHeatLayer']]) {
    await step(route + ': a customizer change rebuilds the layer without a reload', async () => {
      const { ctx, page } = await open(route);
      await waitLayer(page, global);
      await page.evaluate(() => { window.__noReload = 1; });
      const before = await read(page, global);
      await page.evaluate(() => window._customizerV2.setOverride('theme', 'heat5', '#00aa11'));
      await page.waitForFunction(`window.${global} && window.${global}._leaflet_id !== ${before.id} && window.${global}.options.gradient['1'] === '#00aa11'`, null, { timeout: 8000 });
      const after = await read(page, global);
      assert(near(after.top, '#00aa11'), 'drawn top colour ' + after.top);
      assert(after.canvases === 1, after.canvases + ' heat canvases after the change, want 1');
      assert(await page.evaluate(() => window.__noReload === 1), 'the page reloaded');
      await page.evaluate(() => window._customizerV2.clearOverride('theme', 'heat5'));
      await page.waitForFunction(`window.${global}.options.gradient['1'] !== '#00aa11'`, null, { timeout: 8000 });
      checkTokens(await read(page, global), route + ' after clear');
      await ctx.close();
    });
  }

  for (const [route, global] of [['map', '_meshcoreHeatLayer'], ['live', '_meshcoreLiveHeatLayer']]) {
    await step(route + ': the achromatopsia preset rebuilds the layer per mode; deut leaves it; clearing restores the theme', async () => {
      const { ctx, page } = await open(route);
      await waitLayer(page, global);
      const theme = await read(page, global);
      const ramp = await page.evaluate(() => window.MeshCorePresets.list.find((x) => x.id === 'achromat').heatRamp);
      await page.evaluate(() => window.MeshCorePresets.applyPreset('achromat'));
      await page.waitForFunction(`window.${global}._leaflet_id !== ${theme.id}`, null, { timeout: 8000 });
      const light = await read(page, global);
      checkRamp(light, ramp.light, route + ' achromat light');
      await darkToggle(page);
      await page.waitForFunction(`window.${global}._leaflet_id !== ${light.id}`, null, { timeout: 8000 });
      checkRamp(await read(page, global), ramp.dark, route + ' achromat dark');
      await darkToggle(page);
      await page.waitForFunction(`window.${global}.options.gradient['1'].toLowerCase() === '${ramp.light[4]}'`, null, { timeout: 8000 });
      // #1446: a user's colour beats the active preset; clearing it brings the preset back.
      await page.evaluate(() => window._customizerV2.setOverride('theme', 'heat5', '#ff0000'));
      await page.waitForFunction(`window.${global}.options.gradient['1'].toLowerCase() === '#ff0000'`, null, { timeout: 8000 });
      const user = await read(page, global);
      assert(user.tok[4].toLowerCase() === '#ff0000' && near(user.top, '#ff0000'), route + ' user override under the preset: ' + user.tok[4] + ' drawn ' + user.top);
      assert(user.tok[3].toLowerCase() === ramp.light[3], route + ' the other stops stay the preset\'s');
      assert(user.canvases === 1, user.canvases + ' heat canvases, want 1');
      await page.evaluate(() => window._customizerV2.clearOverride('theme', 'heat5'));
      await page.waitForFunction(`window.${global}.options.gradient['1'].toLowerCase() === '${ramp.light[4]}'`, null, { timeout: 8000 });
      checkRamp(await read(page, global), ramp.light, route + ' achromat after the override is cleared');
      await page.evaluate(() => window.MeshCorePresets.clearPreset());
      await page.waitForFunction(`window.${global}.options.gradient['1'].toLowerCase() === '${theme.grad['1'].toLowerCase()}'`, null, { timeout: 8000 });
      const back = await read(page, global);
      checkTokens(back, route + ' after clear');
      await page.evaluate(() => window.MeshCorePresets.applyPreset('deut'));
      await page.waitForTimeout(300);
      const deut = await read(page, global);
      assert(deut.id === back.id, 'deut (no heat ramp) rebuilt the layer');
      checkTokens(deut, route + ' deut');
      await page.evaluate(() => window.MeshCorePresets.clearPreset());
      await ctx.close();
    });
  }

  await step('Map: the dark-mode toggle rebuilds the layer with the dark tokens', async () => {
    const { ctx, page } = await open('map');
    await waitLayer(page, '_meshcoreHeatLayer');
    const before = await read(page, '_meshcoreHeatLayer');
    await darkToggle(page);
    await page.waitForFunction(`window._meshcoreHeatLayer._leaflet_id !== ${before.id}`, null, { timeout: 8000 });
    const after = await read(page, '_meshcoreHeatLayer');
    assert(after.grad['1'].toLowerCase() !== before.grad['1'].toLowerCase(), 'gradient did not change');
    checkTokens(after, 'Map after toggle');
    await ctx.close();
  });

  for (const [route, global, box] of [['map', '_meshcoreHeatLayer', 'mcHeatmap'], ['live', '_meshcoreLiveHeatLayer', 'liveHeatToggle']]) {
    await step(route + ': with the heat map switched off, a theme change leaves it off', async () => {
      const { ctx, page } = await open(route);
      await waitLayer(page, global); // nodes are in: a stray rebuild would have points to draw
      await page.evaluate((id) => { const el = document.getElementById(id); el.checked = false; el.dispatchEvent(new Event('change', { bubbles: true })); }, box);
      await page.waitForFunction(() => !document.querySelector('canvas.leaflet-heatmap-layer'), null, { timeout: 5000 });
      await page.evaluate(() => window._customizerV2.setOverride('theme', 'heat5', '#00aa11'));
      await page.waitForFunction(() => getComputedStyle(document.documentElement).getPropertyValue('--heat-stop-5').trim() === '#00aa11', null, { timeout: 8000 });
      await darkToggle(page);
      await page.waitForTimeout(500); // past the watcher's 30 ms coalescing
      const n = await page.evaluate(() => document.querySelectorAll('canvas.leaflet-heatmap-layer').length);
      assert(n === 0, n + ' heat canvases after a theme change with the heat map off');
      await page.evaluate(() => window._customizerV2.clearOverride('theme', 'heat5'));
      await ctx.close();
    });
  }

  await step('Map: a dark toggle while a route view hides the heat layer rebuilds it off the map', async () => {
    const { ctx, page } = await open('map?packet=' + ROUTE_PACKET);
    await page.waitForFunction(() => !!window._meshcoreHeatLayer && !!document.querySelector('a[title="Close route"]') &&
      !document.querySelector('canvas.leaflet-heatmap-layer'), null, { timeout: 15000 });
    const before = await page.evaluate(() => ({ id: window._meshcoreHeatLayer._leaflet_id, top: window._meshcoreHeatLayer.options.gradient['1'] }));
    await darkToggle(page);
    await page.waitForFunction(`window._meshcoreHeatLayer && window._meshcoreHeatLayer._leaflet_id !== ${before.id}`, null, { timeout: 8000 });
    await page.waitForTimeout(300);
    const after = await page.evaluate(() => ({
      top: window._meshcoreHeatLayer.options.gradient['1'],
      tok: getComputedStyle(document.documentElement).getPropertyValue('--heat-stop-5').trim(),
      canvases: document.querySelectorAll('canvas.leaflet-heatmap-layer').length,
      route: !!document.querySelector('a[title="Close route"]'),
    }));
    assert(after.route, 'the route view closed');
    assert(after.canvases === 0, after.canvases + ' heat canvases under the route view, want 0');
    assert(after.top.toLowerCase() === after.tok.toLowerCase() && after.top !== before.top, 'the hidden layer kept ' + after.top + ', token ' + after.tok);
    await ctx.close();
  });

  await step('Customizer Theme tab lists the five Heat map colours', async () => {
    const { ctx, page } = await open('map');
    await waitLayer(page, '_meshcoreHeatLayer');
    await page.evaluate(() => { const b = document.getElementById('customizeToggle'); if (b) b.click(); });
    await page.waitForSelector('.cust-heat-colors', { state: 'attached', timeout: 8000 });
    const n = await page.$$eval('[data-panel="theme"] .cust-heat-colors input[type=color], [data-panel="theme"] .cust-heat-colors [data-cv2-field]', (e) => e.length);
    assert(n >= 5, 'only ' + n + ' heat inputs');
    await ctx.close();
  });

  await step('375x560 touch: real taps on the controls, the Display section and Heat map bring the layer with the tokens and take it away', async () => {
    const { ctx, page } = await open('map', { viewport: { width: 375, height: 560 }, touch: true, heat: false });
    await page.waitForSelector('#mapControlsToggle', { state: 'visible', timeout: 15000 });
    assert(await page.evaluate(() => !window._meshcoreHeatLayer && !document.querySelector('canvas.leaflet-heatmap-layer')), 'heat map on before any tap');
    await tap(page, '#mapControlsToggle');
    await page.waitForFunction(() => !document.getElementById('mapControls').classList.contains('collapsed'));
    // The Heat map checkbox sits in a section that may start collapsed on phones.
    if (await page.evaluate(() => { const fs = document.getElementById('mcHeatmap').closest('fieldset.mc-section'); return !!(fs && fs.classList.contains('mc-collapsed')); })) {
      await tap(page, 'fieldset.mc-section:has(#mcHeatmap) > legend.mc-label');
    }
    await page.waitForFunction(() => { const r = document.getElementById('mcHeatmap').getBoundingClientRect(); return r.width > 0 && r.height > 0; });
    await tap(page, '#mcHeatmap');
    await waitLayer(page, '_meshcoreHeatLayer');
    checkTokens(await read(page, '_meshcoreHeatLayer'), 'Map 375x560');
    await tap(page, '#mcHeatmap');
    await page.waitForFunction(() => !window._meshcoreHeatLayer && !document.querySelector('canvas.leaflet-heatmap-layer'), null, { timeout: 5000 });
    await ctx.close();
  });

  await step('no console errors', async () => { assert(!errors.length, errors.slice(0, 3).join(' | ')); });

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
  if (!hit) throw new Error(sel + ' is covered at ' + x + ',' + y);
  await page.touchscreen.tap(x, y);
}
