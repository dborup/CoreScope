'use strict';
/* touchSwipe: a real finger swipe that scrolls by a known distance (#967).
 *
 * The phone suites scroll a sheet or a list the way a viewer does, with
 * touches dispatched through CDP. Several things made the distance a swipe
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
 *   - On a page that has not drawn a frame since it loaded, the first
 *     swipe scrolled 1 to 3 px more than the next ones (24 px instead of
 *     21 in 6 of 8 fresh pages); after a few animation frames, none did.
 *   - On a loaded host the first moves of a swipe sometimes scroll nothing,
 *     even with the slop added: 30 px swipes scrolled 16 to 18 px, 53 px
 *     ones 37 to 40 px (#973 review, macOS under load). A page that cancels
 *     the first three touchmoves of a touch does the same every time
 *     (30 px → 18), which is how test-touch-swipe-e2e.js reproduces it.
 *
 * So touchSwipe is closed-loop. It lets the given scroll containers hold
 * still for two animation frames before the finger lands, then moves it the
 * distance to scroll plus TOUCH_SLOP_PX. It holds the finger still for
 * HOLD_MS (no fling at the lift), waits for the containers and measures how
 * far they scrolled. While that is short it moves the same finger on by the
 * shortfall and measures again, up to MAX_CORRECTIONS times. Only then
 * does it lift. Correcting inside the touch costs the suite no swipe, and
 * the moves after the first ones do scroll on a loaded host. A further swipe
 * would land again and could lose its first moves again. After the lift it
 * measures once more. Only if the scroll is still short, and the caller
 * allows more than one swipe (opts.maxSwipes, counted against the suite's own
 * budget), does it finish the remainder with further real swipes. It never
 * swipes back to undo an overshoot.
 * Guarded by test-touch-swipe-helper.js (the path, the order and the loop,
 * against a model page) and test-touch-swipe-e2e.js (the distance, in a
 * real browser).
 */

const TOUCH_SLOP_PX = 15;
const HOLD_MS = 150;
const MAX_CORRECTIONS = 3;

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

// scrolled is how far the containers have scrolled down together since
// `from` (their scrollTops then), and the viewport's height.
async function scrolled(page, selectors, from) {
  const now = await page.evaluate((sels) => ({
    tops: sels.map((s) => { const e = document.querySelector(s); return e ? e.scrollTop : 0; }),
    h: innerHeight,
  }), selectors);
  return { px: from ? now.tops.reduce((sum, t, i) => sum + t - from[i], 0) : 0, tops: now.tops, h: now.h };
}

async function move(page, cdp, points) {
  for (const p of points) {
    await cdp.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [p] });
    await page.waitForTimeout(16);
  }
}

// oneSwipe is a single touch from `at` that scrolls the containers by
// scrollPx, correcting a shortfall before the lift. How far they scrolled.
async function oneSwipe(page, cdp, at, scrollPx, scrollers, steps) {
  await scrollSettled(page, scrollers);
  const from = (await scrolled(page, scrollers)).tops;
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [{ x: at.x, y: at.y }] });
  const path = swipePath(at, scrollPx, steps);
  await move(page, cdp, path);
  let finger = path[path.length - 1], got = 0;
  for (let k = 0; ; k++) {
    await page.waitForTimeout(HOLD_MS);
    await scrollSettled(page, scrollers);
    const was = got, now = await scrolled(page, scrollers, from);
    got = now.px;
    const short = Math.sign(scrollPx) * (scrollPx - got);
    // Done; out of corrections; or the last correction scrolled nothing
    // (the content's end).
    if (short < 1 || k === MAX_CORRECTIONS || (k > 0 && got === was)) break;
    const y = Math.max(0, Math.min(now.h - 1, finger.y - Math.sign(scrollPx) * short));
    if (y === finger.y) break; // the finger is at the screen's edge
    const n = Math.ceil(Math.abs(y - finger.y) / 4), from0 = finger.y;
    const more = [];
    for (let i = 1; i <= n; i++) more.push({ x: at.x, y: Math.round(from0 + (y - from0) * i / n) });
    await move(page, cdp, more);
    finger = more[more.length - 1];
  }
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
  await scrollSettled(page, scrollers);
  return (await scrolled(page, scrollers, from)).px;
}

// touchSwipe swipes from `at` so that the content under it scrolls by
// scrollPx (see swipePath) and returns the number of swipes it took (1
// unless opts.maxSwipes allows more and one fell short).
// opts.scrollers (selectors, required): the containers that must hold
// still before and after, and whose scroll is measured. opts.steps: the
// number of moves (default 8). opts.maxSwipes: how many swipes it may use
// (default 1).
async function touchSwipe(page, cdp, at, scrollPx, opts) {
  opts = opts || {};
  if (!opts.scrollers || !opts.scrollers.length) throw new Error('touchSwipe needs opts.scrollers');
  const max = Math.max(1, opts.maxSwipes || 1);
  let swipes = 0, done = 0, last;
  do {
    last = await oneSwipe(page, cdp, at, scrollPx - done, opts.scrollers, opts.steps || 8);
    done += last;
    swipes++;
  } while (swipes < max && last !== 0 && Math.sign(scrollPx) * (scrollPx - done) >= 1);
  return swipes;
}

module.exports = { touchSwipe, swipePath, scrollSettled, TOUCH_SLOP_PX, HOLD_MS, MAX_CORRECTIONS };
