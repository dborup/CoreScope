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
 * Options: minHeight (px, default 44), timeoutMs (default 10000),
 * intervalMs (default 100). Guarded by test-tap-when-settled.js.
 */

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
      await page.touchscreen.tap(b.x + b.width / 2, b.y + b.height / 2);
      return b;
    }
    prev = b || null;
    if (Date.now() >= deadline) break;
    await new Promise(r => setTimeout(r, intervalMs));
  }
  throw new Error(sel + ' never settled under a tap within ' + timeoutMs + ' ms; last read: ' +
    (last ? JSON.stringify({ box: last.box, hit: last.hit }) : 'no box'));
}

module.exports = { tapWhenSettled };
