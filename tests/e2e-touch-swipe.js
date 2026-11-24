'use strict';
/* touchSwipe: a real finger swipe that scrolls by a known distance (#967).
 *
 * The phone suites scroll a sheet or a list the way a viewer does, with
 * touches dispatched through CDP. Two things made the distance a swipe
 * scrolls differ from the distance the finger moves:
 *
 *   - Chromium starts a scroll only once the finger has moved TOUCH_SLOP_PX;
 *     the scroll then follows the finger for the rest of the way. Measured
 *     on Playwright's Chromium (isMobile, hasTouch): a swipe of 36 px
 *     scrolls 21 px, one of 80 px scrolls 65 px. A suite that sized its
 *     swipes by the distance it wanted to scroll fell short by 15 px a
 *     swipe and ran out of swipes on a layout a little taller than its own
 *     (test-issue-941-m2-nodes-in-reach-e2e.js with another sans-serif
 *     font).
 *   - A finger lifted while it moves hands the rest to a fling, and the
 *     fling's length follows the speed of the last moves, which on a loaded
 *     runner is different on each run (test-issue-839-map-phone-e2e.js:
 *     30 px swipes scrolled 15 to 41 px).
 *
 *   - On a page that has not drawn a frame since it loaded, the first
 *     swipe scrolled 1 to 3 px more than the next ones (24 px instead of
 *     21 in 6 of 8 fresh pages); after a few animation frames, none did.
 *
 * So touchSwipe lets the given scroll containers hold still for two
 * animation frames before the finger lands, moves it the distance to scroll
 * plus TOUCH_SLOP_PX, holds it still for HOLD_MS before it lifts (no fling),
 * and returns once the containers have held still again.
 * Guarded by test-touch-swipe-helper.js (the path) and
 * test-touch-swipe-e2e.js (the distance, in a real browser).
 */

const TOUCH_SLOP_PX = 15;
const HOLD_MS = 150;

// swipePath is the finger's moves for a swipe from `at` that scrolls the
// content by scrollPx: positive moves the finger up the screen (the content
// scrolls down, to what is below), negative moves it down.
function swipePath(at, scrollPx, steps) {
  const travel = Math.sign(scrollPx) * (Math.abs(scrollPx) + TOUCH_SLOP_PX);
  const points = [];
  for (let k = 1; k <= steps; k++) points.push({ x: at.x, y: Math.round(at.y - travel * k / steps) });
  return points;
}

// scrollSettled waits until every element in `selectors` has kept its
// scroll position across two animation frames.
async function scrollSettled(page, selectors) {
  await page.evaluate(() => { window.__touchSwipeLast = null; });
  await page.waitForFunction((sels) => {
    const now = sels.map((s) => { const e = document.querySelector(s); return e ? e.scrollTop + ',' + e.scrollLeft : '-'; }).join('|');
    const same = window.__touchSwipeLast === now;
    window.__touchSwipeLast = now;
    return same;
  }, selectors, { polling: 'raf', timeout: 10000 });
}

// touchSwipe swipes from `at` so that the content under it scrolls by
// scrollPx (see swipePath). opts.scrollers (selectors, required): the
// containers that must hold still before and after. opts.steps: the number
// of moves (default 8).
async function touchSwipe(page, cdp, at, scrollPx, opts) {
  opts = opts || {};
  if (!opts.scrollers || !opts.scrollers.length) throw new Error('touchSwipe needs opts.scrollers');
  await scrollSettled(page, opts.scrollers);
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [{ x: at.x, y: at.y }] });
  for (const p of swipePath(at, scrollPx, opts.steps || 8)) {
    await cdp.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [p] });
    await page.waitForTimeout(16);
  }
  await page.waitForTimeout(HOLD_MS);
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
  await scrollSettled(page, opts.scrollers);
}

module.exports = { touchSwipe, swipePath, scrollSettled, TOUCH_SLOP_PX, HOLD_MS };
