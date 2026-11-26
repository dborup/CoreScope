#!/usr/bin/env node
/* E2E guard for tests/e2e-touch-swipe.js (#967): in a real Chromium with
 * touch, touchSwipe scrolls a box by the distance asked for, give or take
 * BAND_PX, in one swipe.
 *
 * The phone suites test-issue-839-map-phone-e2e.js and
 * test-issue-941-m2-nodes-in-reach-e2e.js size their swipe budgets by that
 * distance. Before #967 a swipe scrolled 15 px less than the finger moved
 * (Chromium's touch slop), plus a fling of varying length when the finger
 * lifted while moving; on a loaded host its first moves sometimes scrolled
 * nothing as well. The suites ran out of swipes now and then.
 *
 * BAND_PX is not pixel equality, which a loaded host does not keep (a
 * swipe can land a few px past). It is still far narrower than what the old
 * swipes did: 15 px short (the slop), 12 px short (the lost first moves) or
 * hundreds of px long (a fling). Step 6 shows that both old behaviours fall
 * outside it.
 *
 * Asserts, at 375x560 with touch, on a static page (no server needed):
 *   1. swipes of 1, 21, 36 and 65 px, up and down, from 10 or 8 moves,
 *      each scroll the box by that ± BAND_PX, in one swipe although three
 *      are allowed (so a further swipe cannot hide a missing correction);
 *   2. the same holds while the main thread is busy for 30 ms in every
 *      frame (a loaded runner);
 *   3. the same holds on a page that cancels the first three touchmoves of
 *      every touch (what a loaded host did to the first moves), whereas the
 *      same finger path without touchSwipe's correction falls short by more
 *      than BAND_PX;
 *   4. the first swipe on a page that has drawn no frame yet is in the band
 *      too (it went 1 to 3 px further when the finger landed at once), in
 *      eight fresh pages;
 *   5. the box is still when touchSwipe returns: two animation frames
 *      later it has not moved;
 *   6. the old swipes fall outside the band: a finger that travels only the
 *      distance asked for falls short by about TOUCH_SLOP_PX, and the
 *      helper's own path lifted while it moves (event timestamps 8 ms
 *      apart, so the speed does not depend on the host) flings far past it.
 *
 * Usage: node test-touch-swipe-e2e.js
 */
'use strict';
const { chromium } = require('playwright');
const { touchSwipe, swipePath, scrollSettled, TOUCH_SLOP_PX, HOLD_MS } = require('./tests/e2e-touch-swipe');

const BAND_PX = 5;
const PAGE = `<!doctype html><meta name="viewport" content="width=device-width, initial-scale=1">
<style>
  html, body { margin: 0; height: 100%; overflow: hidden; }
  #box { position: absolute; left: 16px; right: 16px; top: 100px; height: 300px; overflow-y: auto; overscroll-behavior: contain; }
  #box div { height: 40px; border-bottom: 1px solid; }
</style>
<div id="box">${Array.from({ length: 80 }, (_, i) => `<div>row ${i}</div>`).join('')}</div>`;
// The first three touchmoves of each touch scroll nothing.
const EAT_FIRST_MOVES = `(() => {
  let n = 0;
  const box = document.getElementById('box');
  box.addEventListener('touchstart', () => { n = 0; }, { passive: true });
  box.addEventListener('touchmove', (e) => { if (n++ < 3) e.preventDefault(); }, { passive: false });
})();`;

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + (e && e.stack || e)); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log('\n=== #967 touchSwipe in a real browser ===\n');

  async function open(script) {
    const ctx = await browser.newContext({ viewport: { width: 375, height: 560 }, hasTouch: true, isMobile: true });
    const page = await ctx.newPage();
    await page.setContent(PAGE);
    if (script) await page.evaluate(script);
    const cdp = await ctx.newCDPSession(page);
    return { ctx, page, cdp };
  }
  const AT = { x: 187, y: 250 };
  const top = page => page.evaluate(() => document.getElementById('box').scrollTop);
  const setTop = (page, v) => page.evaluate((t) => { document.getElementById('box').scrollTop = t; }, v);
  const inBand = (moved, px) => Math.abs(moved - px) <= BAND_PX;

  async function bandSwipes(page, cdp) {
    const got = [];
    for (const steps of [10, 8]) {
      for (const px of [1, 21, 36, 65]) {
        for (const sign of [1, -1]) {
          await setTop(page, 1000);
          // Three swipes allowed, one expected: the shortfall is made up
          // inside the touch, not by the fallback's further swipes.
          const swipes = await touchSwipe(page, cdp, AT, sign * px, { steps, scrollers: ['#box'], maxSwipes: 3 });
          const moved = (await top(page)) - 1000;
          if (!inBand(moved, sign * px) || swipes !== 1) got.push(`${steps} moves, ${sign * px} px: moved ${moved} in ${swipes} swipe(s)`);
        }
      }
    }
    assert(got.length === 0, `swipes off their distance by more than ${BAND_PX} px: ` + got.join('; '));
  }

  await step(`swipes of 1, 21, 36 and 65 px, up and down, scroll the box by that ± ${BAND_PX} px`, async () => {
    const { ctx, page, cdp } = await open();
    try { await bandSwipes(page, cdp); } finally { await ctx.close(); }
  });

  await step('the same with the main thread busy for 30 ms in every frame', async () => {
    const { ctx, page, cdp } = await open(() => {
      const busy = () => { const end = performance.now() + 30; while (performance.now() < end) { /* a loaded runner */ } requestAnimationFrame(busy); };
      requestAnimationFrame(busy);
    });
    try { await bandSwipes(page, cdp); } finally { await ctx.close(); }
  });

  await step('the same when the first three moves of a touch scroll nothing; without the correction they fall short', async () => {
    const { ctx, page, cdp } = await open(EAT_FIRST_MOVES);
    try {
      await bandSwipes(page, cdp);
      // touchSwipe's first path alone, held and lifted: what it would
      // scroll if it did not move on.
      await setTop(page, 1000);
      await scrollSettled(page, ['#box']);
      await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [AT] });
      for (const p of swipePath(AT, 30, 10)) {
        await cdp.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [p] });
        await page.waitForTimeout(16);
      }
      await page.waitForTimeout(HOLD_MS);
      await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
      await scrollSettled(page, ['#box']);
      const moved = (await top(page)) - 1000;
      assert(moved < 30 - BAND_PX, `the uncorrected path scrolled ${moved} px of 30, inside the band: the page no longer loses the first moves`);
    } finally { await ctx.close(); }
  });

  await step(`the first swipe on a page that has drawn no frame yet scrolls 21 ± ${BAND_PX} px, in eight fresh pages`, async () => {
    const got = [];
    for (let i = 0; i < 8; i++) {
      const { ctx, page, cdp } = await open();
      try {
        await setTop(page, 1000);
        await touchSwipe(page, cdp, AT, 21, { scrollers: ['#box'] });
        got.push((await top(page)) - 1000);
      } finally { await ctx.close(); }
    }
    assert(got.every(v => inBand(v, 21)), 'first swipes scrolled ' + got.join(', '));
  });

  await step('the box is still when touchSwipe returns', async () => {
    const { ctx, page, cdp } = await open();
    try {
      await setTop(page, 1000);
      await touchSwipe(page, cdp, AT, 65, { scrollers: ['#box'] });
      const at = await top(page);
      await page.evaluate(() => { window.__frames = 0; const tick = () => { if (++window.__frames < 2) requestAnimationFrame(tick); }; requestAnimationFrame(tick); });
      await page.waitForFunction(() => window.__frames >= 2);
      assert(await top(page) === at, 'the box moved after touchSwipe returned: ' + at + ' → ' + await top(page));
    } finally { await ctx.close(); }
  });

  await step('the old swipes fall outside the band: the bare distance falls short, a lift while moving flings', async () => {
    const { ctx, page, cdp } = await open();
    try {
      // A finger moving only the 36 px asked for in 8 moves, held, lifted.
      await setTop(page, 1000);
      await scrollSettled(page, ['#box']);
      await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [AT] });
      for (let k = 1; k <= 8; k++) {
        await cdp.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [{ x: AT.x, y: AT.y - 36 * k / 8 }] });
        await page.waitForTimeout(16);
      }
      await page.waitForTimeout(HOLD_MS);
      await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
      await scrollSettled(page, ['#box']);
      const short = (await top(page)) - 1000;
      assert(short < 36 - BAND_PX, `36 px of finger travel scrolled ${short} px, inside the band (about ${36 - TOUCH_SLOP_PX} expected)`);

      // touchSwipe's path for 65 px, lifted while it moves.
      await setTop(page, 1000);
      await scrollSettled(page, ['#box']);
      const t0 = Date.now() / 1000;
      await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [AT], timestamp: t0 });
      const path = swipePath(AT, 65, 8);
      for (let k = 0; k < path.length; k++) {
        await cdp.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [path[k]], timestamp: t0 + (k + 1) * 0.008 });
      }
      await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [], timestamp: t0 + path.length * 0.008 + 0.001 });
      await scrollSettled(page, ['#box']);
      const flung = (await top(page)) - 1000;
      assert(flung > 65 + BAND_PX, `a lift while moving scrolled ${flung} px for 65, inside the band: no fling`);
    } finally { await ctx.close(); }
  });

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed\n`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
