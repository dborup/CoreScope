/**
 * E2E (#1521, ported for #189): the Live map's node-pulse highlight ring
 * never drops below a 2px stroke while it is visible (a11y minimum).
 *
 * Since #1521 the pulse rings are drawn on the canvas animation engine:
 * pulseNode() queues a pulse with hl_weight 3, and stepPulse() thins it to 2
 * once hl_op falls to 0.4 or below, until the ring fades out (hl_op <= 0).
 *
 * The test loads /#/live, waits for init and an idle engine in LIVE mode,
 * fires one pulse through window._liveTestSeams.triggerPulse, and then
 * follows that pulse frame by frame (awaiting rAF inside the page, as in
 * test-pr-1490-live-map-gpu-animations-e2e.js) until the engine drops it.
 * It checks that the ring was seen visible in both phases (so the thin
 * phase was really observed) and that its weight was >= 2 in every visible
 * frame.
 *
 * Previously test-marker-outline-weight.js, an @playwright/test spec;
 * that runner is not a dependency, so the spec never ran. This port uses the
 * plain `playwright` API like the other *-e2e.js files.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-live-pulse-ring-weight-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const MIN_WEIGHT = 2;
// The ring fades out in ~0.7s at 1x; 5s is ample headroom for slow CI.
const LIFETIME_TIMEOUT_MS = 5000;

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

async function run(browser) {
  const ctx = await browser.newContext({ viewport: { width: 1400, height: 900 } });
  const page = await ctx.newPage();
  page.setDefaultTimeout(15000);
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  try {
    await page.goto(BASE + '/#/live', { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#liveMap', { state: 'visible' });
    await page.waitForFunction(() => typeof window._liveWSHandler === 'function' && !!window._liveWSHandler() &&
      !!window._liveTestSeams && typeof window._liveTestSeams.triggerPulse === 'function');
    await page.evaluate(() => { if (window._liveVcrSetMode) window._liveVcrSetMode('LIVE'); });

    const seen = await page.evaluate(async (timeoutMs) => {
      const seams = window._liveTestSeams;
      // stepPulse runs for every queued pulse, on screen or not.
      const before = seams.getPulses().length;
      seams.triggerPulse('test-189-pulse-ring', [37.45, -122.05], 'ADVERT');
      const pulses = seams.getPulses();
      if (pulses.length !== before + 1) return { queued: false };
      const pulse = pulses[pulses.length - 1];
      const out = { queued: true, initialWeight: pulse.hl_weight, frames: 0, thick: 0, thin: 0,
        minWeight: Infinity, minWeightOp: null, done: false };
      const t0 = performance.now();
      while (performance.now() - t0 < timeoutMs) {
        await new Promise((r) => requestAnimationFrame(r));
        if (!seams.getPulses().includes(pulse)) { out.done = true; break; }
        if (!(pulse.hl_op > 0)) continue;
        out.frames++;
        if (pulse.hl_op > 0.4) out.thick++; else out.thin++;
        if (pulse.hl_weight < out.minWeight) { out.minWeight = pulse.hl_weight; out.minWeightOp = pulse.hl_op; }
      }
      out.ms = Math.round(performance.now() - t0);
      return out;
    }, LIFETIME_TIMEOUT_MS);

    await step('the pulse is queued with a ' + seen.initialWeight + 'px ring', async () => {
      assert(seen.queued, 'triggerPulse did not add a pulse to activePulses');
      assert(seen.initialWeight >= MIN_WEIGHT, 'initial hl_weight ' + seen.initialWeight + ' < ' + MIN_WEIGHT);
    });

    await step('the ring is followed through its visible life (' + seen.frames + ' frames, ' +
      seen.thick + ' thick + ' + seen.thin + ' thin, ' + seen.ms + 'ms)', async () => {
      assert(seen.done, 'the pulse was not removed within ' + LIFETIME_TIMEOUT_MS + 'ms');
      assert(seen.thick > 0, 'no visible frame with hl_op > 0.4 was observed');
      assert(seen.thin > 0, 'no visible frame with 0 < hl_op <= 0.4 was observed (the thin phase)');
    });

    await step('the visible ring is never thinner than ' + MIN_WEIGHT + 'px (min ' + seen.minWeight + ')', async () => {
      assert(seen.minWeight >= MIN_WEIGHT, 'hl_weight dropped to ' + seen.minWeight + ' at hl_op ' + seen.minWeightOp);
    });

    await step('no page errors', async () => {
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
      console.error('test-live-pulse-ring-weight-e2e.js: FAIL — Chromium required but unavailable: ' + err.message);
      process.exit(1);
    }
    console.log('test-live-pulse-ring-weight-e2e.js: SKIP (Chromium unavailable: ' + err.message.split('\n')[0] + ')');
    process.exit(0);
  }
  console.log('=== #1521 live pulse ring weight E2E — ' + BASE + ' ===');
  try {
    try { await run(browser); }
    catch (e) { failed++; console.error('  ✗ ' + e.message); }
  } finally {
    await browser.close();
  }
  console.log('\ntest-live-pulse-ring-weight-e2e.js: ' + passed + ' passed, ' + failed + ' failed');
  process.exit(failed ? 1 : 0);
})();
