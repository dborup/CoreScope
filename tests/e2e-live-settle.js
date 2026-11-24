'use strict';
/* settleLive: wait until the Live page's layout is final (#958, review
 * PR#961 F1 and F5).
 *
 * A Live E2E that reads a panel's rect and then presses on it must read the
 * rect the press lands on. Right after a mount (and after a resize) it moves:
 * live.js initResizeHandler sets .live-page's height in a 50 ms debounced
 * handler, and init() makes the top nav fixed, so a bottom-anchored panel
 * (the Moments window, the Feed) drops by the nav's 52 px (848 -> 900 at
 * 1440x900) a few frames after it first shows. The VCR bar and map credit
 * heights follow by ResizeObserver. A rect read before that is stale: the
 * press lands above the bar, or a drag is measured from the wrong place.
 *
 * Settled means: the page has the height that handler gives the current
 * viewport (innerHeight less the bottom nav and the stale-source pills),
 * the fonts are in, and the element's rect is the same in two polls in a
 * row. waitForFunction polls once per animation frame ('raf'), so that is
 * one frame apart.
 *
 * livePageSettled runs in the page, so it is self-contained. It must stay
 * synchronous: waitForFunction takes a returned Promise as truthy at once.
 * Guarded by test-live-settle.js.
 */

// livePageSettled is the in-page predicate; selector is the element whose
// rect must hold still. The last rect is kept on window.__e2eSettleLast.
function livePageSettled(selector) {
  const pg = document.querySelector('.live-page');
  const el = document.querySelector(selector);
  if (!pg || !el || document.fonts.status !== 'loaded') return false;
  // as live.js initResizeHandler computes it
  const bn = document.querySelector('.bottom-nav');
  let reserve = bn && getComputedStyle(bn).display !== 'none' ? bn.getBoundingClientRect().height : 0;
  if (!reserve) {
    const probe = document.createElement('div');
    probe.style.cssText = 'position:absolute;visibility:hidden;height:var(--bottom-nav-reserve,0px);pointer-events:none;';
    document.body.appendChild(probe);
    reserve = probe.getBoundingClientRect().height || 0;
    probe.remove();
  }
  const pills = parseFloat(getComputedStyle(document.documentElement).getPropertyValue('--warmup-pills-h')) || 0;
  // an empty style.height (the handler has not run) is NaN: not settled
  if (!(Math.abs(parseFloat(pg.style.height) - Math.max(0, innerHeight - reserve - pills)) <= 0.5)) return false;
  const r = el.getBoundingClientRect();
  const key = [r.left, r.top, r.width, r.height].join();
  const last = window.__e2eSettleLast;
  window.__e2eSettleLast = key;
  return last === key;
}

async function settleLive(page, selector, opts) {
  await page.evaluate(() => { window.__e2eSettleLast = null; });
  await page.waitForFunction(livePageSettled, selector, { polling: 'raf', timeout: (opts && opts.timeoutMs) || 10000 });
}

module.exports = { settleLive, livePageSettled };
