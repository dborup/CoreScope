'use strict';
/* Pins the packets page's time window for an E2E browser context.
 *
 * CI freshens the fixture once per Playwright shard, and #/packets shows the
 * last 15 minutes by default, so a suite that starts more than 15 minutes into
 * its shard sees an empty table (master went red that way after #777). 180
 * minutes is the largest window packets.js keeps on a mobile viewport (#340)
 * and a listed option, and far more than a shard runs.
 *
 * packets.js reads meshcore-time-window once, when the script is evaluated,
 * so the seed is an init script: it is in place before every page load of the
 * context. Guarded by test-packets-window-pinned.js.
 */
const PINNED_WINDOW_MIN = 180;

async function pinPacketsTimeWindow(ctx, minutes) {
  const min = minutes === undefined ? PINNED_WINDOW_MIN : minutes;
  await ctx.addInitScript(m => {
    try { localStorage.setItem('meshcore-time-window', String(m)); } catch (_) {}
  }, min);
  return ctx;
}

// A non-default window is part of the packets URL (?timeWindow=N, the deep
// link). A suite that asserts an exact hash compares it without the window it
// pinned itself; every other parameter, and any other window, stays.
function withoutPinnedWindow(hash, minutes) {
  const min = String(minutes === undefined ? PINNED_WINDOW_MIN : minutes);
  const s = String(hash);
  const q = s.indexOf('?');
  if (q < 0) return s;
  const kept = s.slice(q + 1).split('&').filter(p => p !== 'timeWindow=' + min);
  return kept.length ? s.slice(0, q) + '?' + kept.join('&') : s.slice(0, q);
}

module.exports = { PINNED_WINDOW_MIN, pinPacketsTimeWindow, withoutPinnedWindow };
