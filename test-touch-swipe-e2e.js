#!/usr/bin/env node
/* E2E guard for tests/e2e-touch-swipe.js (#967): in a real Chromium with
 * touch, touchSwipe scrolls a box by exactly the distance asked for.
 *
 * The phone suites test-issue-839-map-phone-e2e.js and
 * test-issue-941-m2-nodes-in-reach-e2e.js size their swipe budgets by that
 * distance. Before #967 a swipe scrolled 15 px less than the finger moved
 * (Chromium's touch slop), plus a fling of varying length when the finger
 * lifted while moving; both suites ran out of swipes now and then.
 *
 * Asserts, at 375x560 with touch, on a static page (no server needed):
 *   1. swipes of 1, 21, 36 and 65 px, up and down, each scroll the box by
 *      exactly that, from 10 fast or 8 paced moves;
 *   2. the same holds while the main thread is busy for 30 ms in every
 *      frame (a loaded runner);
 *   3. the first swipe on a page that has drawn no frame yet is exact too
 *      (it went 1 to 3 px further when the finger landed at once), in
 *      eight fresh pages;
 *   4. the box is still when touchSwipe returns: two animation frames
 *      later it has not moved;
 *   5. a finger that travels only the distance asked for (the old swipes)
 *      falls short by TOUCH_SLOP_PX, so the slop is still what the helper
 *      adds.
 *
 * Usage: node test-touch-swipe-e2e.js
 */
'use strict';
const { chromium } = require('playwright');
const { touchSwipe, scrollSettled, TOUCH_SLOP_PX } = require('./tests/e2e-touch-swipe');

const PAGE = `<!doctype html><meta name="viewport" content="width=device-width, initial-scale=1">
<style>
  html, body { margin: 0; height: 100%; overflow: hidden; }
  #box { position: absolute; left: 16px; right: 16px; top: 100px; height: 300px; overflow-y: auto; overscroll-behavior: contain; }
  #box div { height: 40px; border-bottom: 1px solid; }
</style>
<div id="box">${Array.from({ length: 80 }, (_, i) => `<div>row ${i}</div>`).join('')}</div>`;

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

  async function open() {
    const ctx = await browser.newContext({ viewport: { width: 375, height: 560 }, hasTouch: true, isMobile: true });
    const page = await ctx.newPage();
    await page.setContent(PAGE);
    const cdp = await ctx.newCDPSession(page);
    return { ctx, page, cdp };
  }
  const AT = { x: 187, y: 250 };
  const top = page => page.evaluate(() => document.getElementById('box').scrollTop);
  const setTop = (page, v) => page.evaluate((t) => { document.getElementById('box').scrollTop = t; }, v);

  async function exactSwipes(page, cdp) {
    const got = [];
    for (const steps of [10, 8]) {
      for (const px of [1, 21, 36, 65]) {
        for (const sign of [1, -1]) {
          await setTop(page, 1000);
          await touchSwipe(page, cdp, AT, sign * px, { steps, scrollers: ['#box'] });
          const moved = (await top(page)) - 1000;
          if (moved !== sign * px) got.push(`${steps} moves, ${sign * px} px: moved ${moved}`);
        }
      }
    }
    assert(got.length === 0, 'swipes off their distance: ' + got.join('; '));
  }

  await step('swipes of 1, 21, 36 and 65 px, up and down, scroll the box by exactly that', async () => {
    const { ctx, page, cdp } = await open();
    try { await exactSwipes(page, cdp); } finally { await ctx.close(); }
  });

  await step('the same with the main thread busy for 30 ms in every frame', async () => {
    const { ctx, page, cdp } = await open();
    try {
      await page.evaluate(() => {
        const busy = () => { const end = performance.now() + 30; while (performance.now() < end) { /* a loaded runner */ } requestAnimationFrame(busy); };
        requestAnimationFrame(busy);
      });
      await exactSwipes(page, cdp);
    } finally { await ctx.close(); }
  });

  await step('the first swipe on a page that has drawn no frame yet scrolls exactly 21 px, in eight fresh pages', async () => {
    const got = [];
    for (let i = 0; i < 8; i++) {
      const { ctx, page, cdp } = await open();
      try {
        await setTop(page, 1000);
        await touchSwipe(page, cdp, AT, 21, { scrollers: ['#box'] });
        got.push((await top(page)) - 1000);
      } finally { await ctx.close(); }
    }
    assert(got.every(v => v === 21), 'first swipes scrolled ' + got.join(', '));
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

  await step('a finger that travels only the distance asked for falls short by TOUCH_SLOP_PX', async () => {
    const { ctx, page, cdp } = await open();
    try {
      await setTop(page, 1000);
      await scrollSettled(page, ['#box']);
      // The old swipes: a finger moving 36 px in 8 moves, held, lifted.
      await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [AT] });
      for (let k = 1; k <= 8; k++) {
        await cdp.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [{ x: AT.x, y: AT.y - 36 * k / 8 }] });
        await page.waitForTimeout(16);
      }
      await page.waitForTimeout(150);
      await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
      await scrollSettled(page, ['#box']);
      const moved = (await top(page)) - 1000;
      assert(moved === 36 - TOUCH_SLOP_PX, `36 px of finger travel scrolled ${moved} px, not ${36 - TOUCH_SLOP_PX}`);
    } finally { await ctx.close(); }
  });

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed\n`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
