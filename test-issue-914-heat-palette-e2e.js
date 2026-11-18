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
 *      brings the default back;
 *   4. a colour-blind preset rebuilds it too and the default preset / clear
 *      restores the theme's;
 *   5. the dark-mode toggle rebuilds it with the dark tokens;
 *   6. the Display tab lists the five Heat map colour rows;
 *   7. at 375x560 (touch, real taps) the Heat map checkbox is tapped off
 *      and on in the Map controls and the layer returns with the tokens;
 *   8. no console errors.
 *
 * Usage:  BASE_URL=http://localhost:13700 node test-issue-914-heat-palette-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
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
  const tok = [1, 2, 3, 4, 5].map((n) => (cs.getPropertyValue('--mc-heat-' + n).trim() || cs.getPropertyValue('--heat-' + n).trim()));
  const px = (i) => '#' + [g[i], g[i + 1], g[i + 2]].map((v) => v.toString(16).padStart(2, '0')).join('');
  return { grad: l.options.gradient, tok, top: px(255 * 4), low: px(51 * 4), id: l._leaflet_id };
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
        localStorage.setItem('meshcore-theme', d ? 'dark' : 'light');
        localStorage.setItem('meshcore-map-heatmap', 'true');
        localStorage.setItem('meshcore-live-heatmap', 'true');
        localStorage.removeItem('meshcore-cb-preset');
      } catch (e) {}
    }, !!o.dark);
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
  }

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
      assert(await page.evaluate(() => window.__noReload === 1), 'the page reloaded');
      await page.evaluate(() => window._customizerV2.clearOverride('theme', 'heat5'));
      await page.waitForFunction(`window.${global}.options.gradient['1'] !== '#00aa11'`, null, { timeout: 8000 });
      checkTokens(await read(page, global), route + ' after clear');
      await ctx.close();
    });
  }

  await step('Map: a colour-blind preset rebuilds the layer; clearing restores the theme palette', async () => {
    const { ctx, page } = await open('map');
    await waitLayer(page, '_meshcoreHeatLayer');
    const before = await read(page, '_meshcoreHeatLayer');
    await page.evaluate(() => window.MeshCorePresets.applyPreset('deut'));
    await page.waitForFunction(`window._meshcoreHeatLayer._leaflet_id !== ${before.id}`, null, { timeout: 8000 });
    const p = await read(page, '_meshcoreHeatLayer');
    const deut = await page.evaluate(() => window.MeshCorePresets.list.find((x) => x.id === 'deut').heatRamp);
    assert(p.grad['1'].toLowerCase() === deut[4].toLowerCase(), 'preset top ' + p.grad['1']);
    await page.evaluate(() => window.MeshCorePresets.clearPreset());
    await page.waitForFunction(`window._meshcoreHeatLayer.options.gradient['1'].toLowerCase() === '${before.grad['1'].toLowerCase()}'`, null, { timeout: 8000 });
    await ctx.close();
  });

  await step('Map: the dark-mode toggle rebuilds the layer with the dark tokens', async () => {
    const { ctx, page } = await open('map');
    await waitLayer(page, '_meshcoreHeatLayer');
    const before = await read(page, '_meshcoreHeatLayer');
    await page.evaluate(() => { document.getElementById('darkModeToggle') ? document.getElementById('darkModeToggle').click() : document.getElementById('darkModeCheckbox').click(); });
    await page.waitForFunction(`window._meshcoreHeatLayer._leaflet_id !== ${before.id}`, null, { timeout: 8000 });
    const after = await read(page, '_meshcoreHeatLayer');
    assert(after.grad['1'].toLowerCase() !== before.grad['1'].toLowerCase(), 'gradient did not change');
    checkTokens(after, 'Map after toggle');
    await ctx.close();
  });

  await step('Customizer Display tab lists the five Heat map colours', async () => {
    const { ctx, page } = await open('map');
    await waitLayer(page, '_meshcoreHeatLayer');
    await page.evaluate(() => { const b = document.getElementById('customizeToggle'); if (b) b.click(); });
    await page.waitForSelector('.cust-heat-colors', { state: 'attached', timeout: 8000 });
    const n = await page.$$eval('.cust-heat-colors input[type=color], .cust-heat-colors [data-cv2-field]', (e) => e.length);
    assert(n >= 5, 'only ' + n + ' heat inputs');
    await ctx.close();
  });

  await step('375x560 touch: real taps switch Heat map off and on; the layer returns with the tokens', async () => {
    const { ctx, page } = await open('map', { viewport: { width: 375, height: 560 }, touch: true });
    await waitLayer(page, '_meshcoreHeatLayer');
    await page.touchscreen.tap(...(await centre(page, '#mapControlsToggle')));
    await page.waitForFunction(() => !document.getElementById('mapControls').classList.contains('collapsed'));
    // The Heat map checkbox sits in a section that may be collapsed on phones.
    await page.evaluate(() => {
      const cb = document.getElementById('mcHeatmap');
      const fs = cb.closest('fieldset.mc-section');
      const lg = fs && fs.querySelector('legend.mc-label');
      if (fs && fs.classList.contains('mc-collapsed') && lg) lg.click();
    });
    await page.waitForFunction(() => { const r = document.getElementById('mcHeatmap').getBoundingClientRect(); return r.width > 0 && r.height > 0; });
    await page.touchscreen.tap(...(await centre(page, '#mcHeatmap')));
    await page.waitForFunction(() => !window._meshcoreHeatLayer, null, { timeout: 5000 });
    await page.touchscreen.tap(...(await centre(page, '#mcHeatmap')));
    await waitLayer(page, '_meshcoreHeatLayer');
    checkTokens(await read(page, '_meshcoreHeatLayer'), 'Map 375x560');
    await ctx.close();
  });

  await step('no console errors', async () => { assert(!errors.length, errors.slice(0, 3).join(' | ')); });

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();

async function centre(page, sel) {
  const b = await page.locator(sel).boundingBox();
  if (!b) throw new Error(sel + ' has no box');
  return [Math.round(b.x + b.width / 2), Math.round(b.y + b.height / 2)];
}
