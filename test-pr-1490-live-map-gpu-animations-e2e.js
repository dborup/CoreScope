/**
 * E2E (PR #1490 / #1514 / #1520, ported for #170): the Live map's canvas
 * animation engine queues, drains and goes back to sleep.
 *
 * live.js draws in-flight packets on a <canvas> inside a dedicated Leaflet
 * pane (`animationsPane`, z=625) and runs a requestAnimationFrame loop only
 * while there is something to animate. When an animation finishes it hands
 * off to a fading Leaflet polyline on the same pane and records it in
 * `recentPaths`, which is capped at 5.
 *
 * Per viewport (desktop and mobile) the test loads /#/live, waits until init
 * has finished (window._liveWSHandler() returns a handler) and the engine is
 * idle, then fires a burst of 20 synthetic lines through the real
 * window._liveDrawAnimatedLine seam and checks, via window._liveTestSeams:
 *   1. the animation canvas lives on the animations pane;
 *   2. all 20 animations are queued and the engine wakes;
 *   3. they drain to 0, and in the frame they drain recentPaths is <= 5
 *      (20 > 5, so the cap actually runs);
 *   4. the engine goes back to sleep (isAnimating false);
 *   5. the fading polylines are drawn on the animations pane: with
 *      preferCanvas:true Leaflet adds one renderer canvas per pane, so the
 *      burst may add a renderer canvas to the animations pane and to no
 *      other pane (a fade line without `pane:` lands on overlayPane).
 *
 * Headless Chromium may throttle requestAnimationFrame, so the drain is
 * driven by awaiting rAF from inside the page (as in
 * test-issue-1599-replay-freeze-e2e.js), which also lets step 3 read the
 * path count in the same frame the queue empties.
 *
 * Originally written against @playwright/test, which is not a dependency;
 * this port uses the plain `playwright` API like the other *-e2e.js files.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-pr-1490-live-map-gpu-animations-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const BURST = 20;          // > the recentPaths cap of 5
const PATH_CAP = 5;
// One animation takes ~660ms at 1x; the burst runs in parallel, so the queue
// drains in ~0.7s locally and in CI. 2.5s leaves headroom for the instrumented
// frontend (10/10 local runs well below it) and still catches a stalled or
// serialised engine. The measured time is logged.
const DRAIN_TIMEOUT_MS = 2500;
const VIEWPORTS = [
  { name: 'desktop', viewport: { width: 1400, height: 900 } },
  { name: 'mobile', viewport: { width: 375, height: 812 }, isMobile: true, hasTouch: true },
];

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

async function runViewport(browser, vp) {
  console.log('\n--- ' + vp.name + ' ' + vp.viewport.width + 'x' + vp.viewport.height + ' ---');
  const ctx = await browser.newContext({ viewport: vp.viewport, isMobile: !!vp.isMobile, hasTouch: !!vp.hasTouch });
  const page = await ctx.newPage();
  page.setDefaultTimeout(15000);
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  try {
    await page.goto(BASE + '/#/live', { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#liveMap', { state: 'visible' });
    await page.waitForFunction(() => typeof window._liveWSHandler === 'function' && !!window._liveWSHandler() &&
      !!window._liveTestSeams && typeof window._liveDrawAnimatedLine === 'function');
    // Start from an idle engine in LIVE mode at 1x.
    await page.evaluate(() => { if (window._liveVcrSetMode) window._liveVcrSetMode('LIVE'); });
    await page.waitForFunction(() => window._liveTestSeams.getAnimCount() === 0 && !window._liveTestSeams.isAnimating(),
      null, { polling: 'raf' });

    await step(vp.name + ': the animation canvas is on the animations pane', async () => {
      const n = await page.locator('#liveMap .leaflet-pane.leaflet-animations-pane > canvas').count();
      assert(n >= 1, 'no <canvas> on .leaflet-animations-pane (got ' + n + ')');
      const z = await page.evaluate(() => getComputedStyle(document.querySelector('#liveMap .leaflet-animations-pane')).zIndex);
      assert(z === '625', 'animations pane z-index is ' + z + ', want 625');
    });

    // Leaflet canvas renderers per pane (the engine's own canvas has no class).
    const rendererCanvases = () => page.evaluate(() => {
      const out = {};
      document.querySelectorAll('#liveMap .leaflet-pane > canvas.leaflet-zoom-animated:not(.leaflet-heatmap-layer)').forEach((c) => {
        const pane = (c.parentElement.className.match(/leaflet-([\w-]+)-pane/) || [])[1] || '?';
        out[pane] = (out[pane] || 0) + 1;
      });
      return out;
    });
    const renderersBefore = await rendererCanvases();

    const queued = await page.evaluate((count) => {
      for (let i = 0; i < count; i++) {
        window._liveDrawAnimatedLine([37.4, -122.0], [37.5, -122.1], '#00ff00', null, null, '00AA', 'test-1490-' + i);
      }
      return { count: window._liveTestSeams.getAnimCount(), awake: window._liveTestSeams.isAnimating() };
    }, BURST);

    await step(vp.name + ': a burst of ' + BURST + ' animations is queued and wakes the engine', async () => {
      assert(queued.count === BURST, 'queued ' + queued.count + ', want ' + BURST);
      assert(queued.awake === true, 'engine did not wake (isAnimating=' + queued.awake + ')');
    });

    const drained = await page.evaluate(async (timeoutMs) => {
      const seam = window._liveTestSeams;
      const t0 = performance.now();
      while (seam.getAnimCount() > 0 && performance.now() - t0 < timeoutMs) {
        await new Promise((r) => requestAnimationFrame(r));
      }
      // Read the path count in the frame the queue emptied, before the
      // fades (~0.4s) remove the paths on their own.
      return { count: seam.getAnimCount(), paths: seam.getPathCount(), ms: Math.round(performance.now() - t0) };
    }, DRAIN_TIMEOUT_MS);

    await step(vp.name + ': the animations drain to 0 (' + drained.ms + 'ms)', async () => {
      assert(drained.count === 0, 'activeAnimations did not drain within ' + DRAIN_TIMEOUT_MS + 'ms (count=' + drained.count + ')');
    });

    await step(vp.name + ': recentPaths is capped at ' + PATH_CAP + ' when the burst lands', async () => {
      assert(drained.paths >= 1, 'no fading paths were recorded (got ' + drained.paths + ')');
      assert(drained.paths <= PATH_CAP, 'recentPaths grew to ' + drained.paths + ', cap is ' + PATH_CAP);
    });

    await step(vp.name + ': the engine goes back to sleep', async () => {
      // One rAF tick separates the queue emptying from renderAnimations
      // flipping isAnimating off.
      const asleep = await page.evaluate(async () => {
        for (let i = 0; i < 10 && window._liveTestSeams.isAnimating(); i++) {
          await new Promise((r) => requestAnimationFrame(r));
        }
        return !window._liveTestSeams.isAnimating();
      });
      assert(asleep, 'isAnimating stayed true after the queue drained');
    });

    await step(vp.name + ': the fading polylines render on the animations pane only', async () => {
      const after = await rendererCanvases();
      const seen = 'before ' + JSON.stringify(renderersBefore) + ', after ' + JSON.stringify(after);
      assert((after.animations || 0) === 1, 'want exactly one Leaflet renderer canvas on .leaflet-animations-pane — ' + seen);
      const grew = Object.keys(after).filter((pane) => pane !== 'animations' && after[pane] > (renderersBefore[pane] || 0));
      assert(grew.length === 0, 'the burst added a renderer canvas to ' + grew.join(', ') + ' — a fade line left the animations pane; ' + seen);
    });

    await step(vp.name + ': no page errors', async () => {
      assert(errors.length === 0, errors.join(' | '));
    });
  } finally {
    await ctx.close();
  }
}

(async () => {
  const requireChromium = process.env.CHROMIUM_REQUIRE === '1';
  let browser;
  try {
    browser = await chromium.launch({
      headless: true,
      executablePath: process.env.CHROMIUM_PATH || undefined,
      args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
    });
  } catch (err) {
    if (requireChromium) {
      console.error('test-pr-1490-live-map-gpu-animations-e2e.js: FAIL — Chromium required but unavailable: ' + err.message);
      process.exit(1);
    }
    console.log('test-pr-1490-live-map-gpu-animations-e2e.js: SKIP (Chromium unavailable: ' + err.message.split('\n')[0] + ')');
    process.exit(0);
  }
  console.log('=== PR #1490 live map canvas animation engine E2E — ' + BASE + ' ===');
  try {
    for (const vp of VIEWPORTS) {
      try { await runViewport(browser, vp); }
      catch (e) { failed++; console.error('  ✗ ' + vp.name + ': ' + e.message); }
    }
  } finally {
    await browser.close();
  }
  console.log('\ntest-pr-1490-live-map-gpu-animations-e2e.js: ' + passed + ' passed, ' + failed + ' failed');
  process.exit(failed ? 1 : 0);
})();
