'use strict';
/* tapWhenSettled: a real finger tap on a phone E2E control (#789 M2b, M3).
 *
 * A phone suite taps the middle of a control with page.touchscreen.tap, the
 * way a finger would. The tap must not land while the page is still moving
 * the control (a deep link's smooth scroll, a section that re-renders, a
 * line that arrives late), nor on whatever covers it (the fixed top bar or
 * bottom nav). So the control is centred first, where a finger scrolls it,
 * and tapped only once its box has held still between two reads and the
 * element at its middle is the control itself.
 *
 * Until then any read may come back empty: the box is null while the
 * control is momentarily not rendered (a re-render, an animation frame on a
 * loaded machine), and the centring fails while it is detached. Those are
 * "not settled yet" and the next read follows; the bound is opts.timeoutMs.
 * The size checks run on the settled box only, so a control that settles
 * too small or wider than the viewport still fails, with its box.
 *
 * Two taps in a row are spaced like a finger (#898 M2, review PR#908 F1).
 * Centring puts consecutive controls at the same point, and Chromium's
 * touch emulation turns a second tap within ~30 px and ~300 ms of the
 * previous one into a double tap: the new field gets focus and a dblclick,
 * and the text typed next is dropped (no beforeinput/input fires). A finger
 * cannot type into one field and tap the next that fast, so the helper
 * waits out DOUBLE_TAP_MS when the new tap is within DOUBLE_TAP_SLOP_PX of
 * the page's previous one, then reads the box again.
 *
 * Options: minHeight (px, default 44), timeoutMs (default 10000),
 * intervalMs (default 100). Guarded by test-tap-when-settled.js.
 */

// Measured on Playwright's Chromium (375x560, isMobile, hasTouch): a
// dblclick at dx <= 20 px and gaps <= 300 ms; none at 30 px or 400 ms.
// Both bounds carry a margin.
const DOUBLE_TAP_MS = 500;
const DOUBLE_TAP_SLOP_PX = 50;
const lastTap = new WeakMap();

// doubleTapWait is how long a tap at (x, y) must wait so that Chromium does
// not read it as the second half of a double tap with the previous one.
function doubleTapWait(prev, x, y, now) {
  if (!prev) return 0;
  if (Math.hypot(x - prev.x, y - prev.y) > DOUBLE_TAP_SLOP_PX) return 0;
  return Math.max(0, DOUBLE_TAP_MS - (now - prev.t));
}

// probe centres the control and reads where a finger at its middle lands.
// It answers null while the control has no box or cannot be centred.
async function probe(loc) {
  try {
    await loc.evaluate(e => e.scrollIntoView({ block: 'center', inline: 'nearest' }), null, { timeout: 1000 });
    const b = await loc.boundingBox({ timeout: 1000 });
    if (!b || !(b.width > 0) || !(b.height > 0)) return { box: b || null, hit: null, onTarget: false };
    const x = b.x + b.width / 2, y = b.y + b.height / 2;
    const at = await loc.evaluate((e, p) => {
      const t = document.elementFromPoint(p.x, p.y);
      if (!t) return { onTarget: false, hit: null };
      const name = t.tagName.toLowerCase() + (t.id ? '#' + t.id : '') + (typeof t.className === 'string' && t.className ? '.' + t.className.trim().split(/\s+/).join('.') : '');
      return { onTarget: t === e || e.contains(t), hit: name };
    }, { x, y }, { timeout: 1000 });
    return { box: b, hit: at.hit, onTarget: at.onTarget };
  } catch (_) {
    return null;
  }
}

function sameBox(a, b) {
  return !!a && !!b && a.x === b.x && a.y === b.y && a.width === b.width && a.height === b.height;
}

async function tapWhenSettled(page, sel, opts) {
  opts = opts || {};
  const minHeight = opts.minHeight || 44;
  const timeoutMs = opts.timeoutMs || 10000;
  const intervalMs = opts.intervalMs || 100;
  const vw = page.viewportSize().width;
  const loc = page.locator(sel).first();
  const deadline = Date.now() + timeoutMs;
  let prev = null, last = null;
  for (;;) {
    last = await probe(loc);
    const b = last && last.box;
    if (b && last.onTarget && sameBox(prev, b)) {
      if (!(b.height >= minHeight && b.x >= 0 && b.x + b.width <= vw + 0.5)) {
        throw new Error(sel + ' box ' + JSON.stringify(b) + ' (needs a height of at least ' + minHeight + ' px inside the ' + vw + ' px viewport)');
      }
      const x = b.x + b.width / 2, y = b.y + b.height / 2;
      const wait = doubleTapWait(lastTap.get(page), x, y, Date.now());
      if (wait > 0) {
        // Wait out the double-tap window, then read the box again.
        await new Promise(r => setTimeout(r, wait));
        prev = b;
        continue;
      }
      await page.touchscreen.tap(x, y);
      lastTap.set(page, { x, y, t: Date.now() });
      return b;
    }
    prev = b || null;
    if (Date.now() >= deadline) break;
    await new Promise(r => setTimeout(r, intervalMs));
  }
  throw new Error(sel + ' never settled under a tap within ' + timeoutMs + ' ms; last read: ' +
    (last ? JSON.stringify({ box: last.box, hit: last.hit }) : 'no box'));
}

module.exports = { tapWhenSettled, doubleTapWait, DOUBLE_TAP_MS, DOUBLE_TAP_SLOP_PX };
