/**
 * Shared by the DHM tile-budget E2Es (test-issue-681-dhm-tile-budget-e2e.js,
 * test-issue-734-dhm-overlay-budget-e2e.js; #798 review F1).
 *
 * Both used to extrapolate a session's tile count to a per-minute rate
 * from the session's wall-clock time, and both went red on CI on the same
 * scripted session (#681 on master c4de1f20: 1036/min; #734 on PR #798:
 * 2518/min). The wall clock was not the cause: CI took 23-25 s every
 * time. The count was. Two things made it random:
 *   - Leaflet's drag inertia. On release, Leaflet 1.9.4 takes the speed of
 *     the last 50 ms of drag samples (one per animation frame) with no
 *     speed cap (inertiaMaxSpeed Infinity) and glides speed²/1360 px. The
 *     same scripted 500 px drag moved the map 500 px to 4600 px, depending
 *     only on how the runner's frames fell, and a long glide requests tile
 *     after tile while the session waits.
 *   - a 1x1 stub tile (#681): the page treats it as a placeholder and
 *     retries it once after 3-5 s with Math.random() jitter (#728 N3), and
 *     whether the retry fires depends on when the next step moves away.
 *
 * So here:
 *   - every stub tile is a real 256x256 PNG (no placeholder retries);
 *   - a pan rests the pointer for PAN_HOLD_MS before release, longer than
 *     Leaflet's 50 ms speed window, so it releases without a glide and
 *     moves the map by exactly the dragged distance on every machine;
 *   - every step settles on map state, not on a fixed pause (#798 delta
 *     review N1): a fixed 450 ms was shorter than Leaflet's zoom
 *     transition on a 4-vCPU runner, so a drag that started during the
 *     transition was dropped (Draggable._onDown ignores a press while the
 *     pane has leaflet-zoom-anim), a zoom click could be lost or deferred,
 *     and one step's tiles were charged to the next. Now the next step
 *     starts only when the zoom animation and any pan are over, a zoom
 *     step has changed the zoom by exactly one, every tile layer reports
 *     !isLoading(), and no DHM request has arrived for QUIET_MS. Each
 *     step's tiles are then exactly the requests made since the last
 *     settle, whatever the runner's speed;
 *   - the count is checked per step against a geometric ceiling, and the
 *     per-minute requirement is derived from that ceiling at a fixed user
 *     pace (USER_VIEW_S per view), never from the wall clock.
 *
 * Ceilings, per overlay, for a viewport W x H and 256 px tiles:
 *   - one view:  (ceil(W/256) + 1) x (ceil(H/256) + 1), the most tile
 *     columns x rows any alignment can touch (1920x1080: 9 x 6 = 54);
 *   - a zoom step: one view (a new zoom level shares no tiles);
 *   - a pan by (dx, dy) without glide: ceil(|dx|/256) new columns of at
 *     most ROWS tiles plus ceil(|dy|/256) new rows of at most COLS tiles
 *     (an edge that moves d px crosses at most ceil(d/256) tile borders);
 *   - a session: one view at load plus the sum of its steps.
 * Flick glides are real use the scripted sessions leave out; the HEADROOM
 * factor on top of the ceiling is what covers them.
 */
'use strict';
const zlib = require('zlib');

const TILE_PX = 256;
const PAN_HOLD_MS = 100;  // > Leaflet's 50 ms drag-speed window
const USER_VIEW_S = 2.5;  // the pace of a person looking at each view; #681's
                          // measured normal session ran ~25 s for 10 views
const HEADROOM = 1.3;
const QUIET_MS = 300;     // no new DHM request for this long = settled
const SETTLE_TIMEOUT_MS = 20000;

// A real 256x256 transparent PNG: what the page keeps as a loaded tile.
const TILE_PNG = (() => {
  const crcT = new Int32Array(256);
  for (let n = 0; n < 256; n++) { let c = n; for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1; crcT[n] = c; }
  const crc = (b) => { let c = -1; for (const x of b) c = crcT[(c ^ x) & 0xff] ^ (c >>> 8); return (c ^ -1) >>> 0; };
  const chunk = (t, d) => { const l = Buffer.alloc(4); l.writeUInt32BE(d.length); const td = Buffer.concat([Buffer.from(t), d]); const c = Buffer.alloc(4); c.writeUInt32BE(crc(td)); return Buffer.concat([l, td, c]); };
  const ihdr = Buffer.alloc(13); ihdr.writeUInt32BE(256, 0); ihdr.writeUInt32BE(256, 4); ihdr[8] = 8; ihdr[9] = 6;
  return Buffer.concat([Buffer.from('\x89PNG\r\n\x1a\n', 'binary'), chunk('IHDR', ihdr), chunk('IDAT', zlib.deflateSync(Buffer.alloc((256 * 4 + 1) * 256))), chunk('IEND', Buffer.alloc(0))]);
})();

const VIEWPORT = { width: 1920, height: 1080 };
const DIRECTIONS = [[500, 0], [0, 400], [-500, 0], [0, -400], [400, 300], [-400, 300], [-400, -300], [400, -300]];
const zoomIn = { op: 'zoomIn' };
const zoomOut = { op: 'zoomOut' };
const pans = (dirs) => dirs.map(([dx, dy]) => ({ op: 'pan', dx, dy }));
const times = (n, step) => Array.from({ length: n }, () => step);

function gridOf(vp) {
  return { cols: Math.ceil(vp.width / TILE_PX) + 1, rows: Math.ceil(vp.height / TILE_PX) + 1 };
}
function viewCeiling(vp) { const g = gridOf(vp); return g.cols * g.rows; }
function stepCeiling(step, vp) {
  if (step.op !== 'pan') return viewCeiling(vp);
  const g = gridOf(vp);
  return Math.min(viewCeiling(vp),
    Math.ceil(Math.abs(step.dx) / TILE_PX) * g.rows + Math.ceil(Math.abs(step.dy) / TILE_PX) * g.cols);
}
function sessionCeiling(steps, vp) {
  return steps.reduce((n, s) => n + stepCeiling(s, vp), viewCeiling(vp));
}
// The session at a person's pace: USER_VIEW_S on the first view and on
// each step's view.
function nominalSeconds(steps) { return (1 + steps.length) * USER_VIEW_S; }
// The per-minute rate the budget must carry for this session sustained:
// the ceiling (all overlays) at the nominal pace, times HEADROOM.
function requiredPerMin(steps, vp, overlays) {
  return HEADROOM * overlays * sessionCeiling(steps, vp) * 60 / nominalSeconds(steps);
}

/**
 * Runs one scripted session in a fresh context and returns what the page
 * asked the stubbed DHM proxy for, per step and per layer.
 *   opts: { base, dhmPath, dhmBlock, storage, hash, readySelector,
 *           pauseMs, steps }
 * pauseMs is only a minimum dwell per step (the scripted pace); the count
 * never depends on it, because every step first settles (see settle()).
 */
async function runSession(browser, name, opts) {
  const ctx = await browser.newContext({ viewport: VIEWPORT });
  const tiles = [];
  let lastTileAt = Date.now();
  try {
    await ctx.route(opts.base + '/api/config/client', async (route) => {
      const response = await route.fetch();
      const config = await response.json();
      config.dhm = opts.dhmBlock;
      await route.fulfill({ response, json: config });
    });
    await ctx.route(url => new URL(url).pathname === opts.dhmPath, async (route) => {
      tiles.push(new URL(route.request().url()).searchParams.get('layers'));
      lastTileAt = Date.now();
      await route.fulfill({ status: 200, contentType: 'image/png', body: TILE_PNG });
      lastTileAt = Date.now();
    });
    // Base-map tiles never leave the machine.
    await ctx.route(/^https?:\/\/(?!localhost|127\.0\.0\.1)/i, async (route) => {
      if (route.request().resourceType() !== 'image') { await route.continue(); return; }
      await route.fulfill({ status: 200, contentType: 'image/png', body: TILE_PNG });
    });
    await ctx.addInitScript((storage) => {
      try { for (const k of Object.keys(storage)) localStorage.setItem(k, storage[k]); } catch (_) {}
    }, opts.storage);
    const page = await ctx.newPage();
    // BUDGET_CPU_THROTTLE=4 runs the session on a 4x slower CPU (CDP), the
    // check that the counts do not depend on the runner's speed.
    const throttle = Number(process.env.BUDGET_CPU_THROTTLE || 0);
    if (throttle > 1) {
      const cdp = await ctx.newCDPSession(page);
      await cdp.send('Emulation.setCPUThrottlingRate', { rate: throttle });
    }
    const view = () => page.evaluate(() => {
      const m = window.__mc_map;
      const p = m.project(m.getCenter(), m.getZoom());
      return { z: m.getZoom(), x: Math.round(p.x), y: Math.round(p.y) };
    });
    const center = () => page.$eval('.leaflet-container', el => {
      const r = el.getBoundingClientRect();
      return { x: r.x + r.width / 2, y: r.y + r.height / 2 };
    });
    // The map is at rest: no zoom animation, no pan animation, no drag in
    // progress, every tile layer done loading, and (for a zoom step) the
    // zoom is the expected one.
    const atRest = (wantZ) => page.evaluate((want) => {
      const m = window.__mc_map;
      if (!m || m._animatingZoom) return false;
      if (m._panAnim && m._panAnim._inProgress) return false;
      const d = m.dragging && m.dragging._draggable;
      if (d && d._moving) return false;
      if (m.getContainer().querySelector('.leaflet-zoom-anim')) return false;
      if (want != null && m.getZoom() !== want) return false;
      let loading = false;
      m.eachLayer(l => { if (typeof l.isLoading === 'function' && l.isLoading()) loading = true; });
      return !loading;
    }, wantZ);
    // Settled = at rest and no DHM request for QUIET_MS. A step that never
    // settles (a zoom that never reaches wantZ) throws with what it saw.
    const settle = async (label, wantZ) => {
      const deadline = Date.now() + SETTLE_TIMEOUT_MS;
      for (;;) {
        if (await atRest(wantZ) && Date.now() - lastTileAt >= QUIET_MS) {
          // Re-check after the quiet window so a request that lands in
          // the same tick as the check is not missed.
          await page.waitForTimeout(50);
          if (await atRest(wantZ) && Date.now() - lastTileAt >= QUIET_MS) return;
        }
        if (Date.now() > deadline) {
          const v = await view();
          throw new Error(`${name}: ${label} did not settle in ${SETTLE_TIMEOUT_MS} ms (now z${v.z}` +
            (wantZ != null ? `, expected z${wantZ}` : '') + ')');
        }
        await page.waitForTimeout(50);
      }
    };
    const t0 = Date.now();
    await page.goto(`${opts.base}/${opts.hash}`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('.leaflet-control-layers', { timeout: 20000 });
    await page.waitForFunction(sel => document.querySelectorAll(sel).length > 0 && !!window.__mc_map,
      opts.readySelector, { timeout: 15000 });
    await settle('first view', null);

    const countBy = (list) => list.reduce((m, id) => { m[id] = (m[id] || 0) + 1; return m; }, {});
    const firstView = countBy(tiles);
    const steps = [];
    let seen = tiles.length;
    let before = await view();
    for (const step of opts.steps) {
      const label = step.op === 'pan' ? `pan(${step.dx},${step.dy})` : step.op;
      const stepStart = Date.now();
      let wantZ = null;
      if (step.op === 'pan') {
        const c = await center();
        await page.mouse.move(c.x, c.y);
        await page.mouse.down();
        await page.mouse.move(c.x + step.dx, c.y + step.dy, { steps: 6 });
        await page.waitForTimeout(PAN_HOLD_MS);
        await page.mouse.up();
      } else {
        wantZ = before.z + (step.op === 'zoomIn' ? 1 : -1);
        await page.click(step.op === 'zoomIn' ? '.leaflet-control-zoom-in' : '.leaflet-control-zoom-out');
      }
      await settle(label, wantZ);
      const dwell = opts.pauseMs - (Date.now() - stepStart);
      if (dwell > 0) await page.waitForTimeout(dwell);
      const after = await view();
      const got = countBy(tiles.slice(seen));
      const moved = after.z === before.z ? ` moved ${before.x - after.x},${before.y - after.y} px` : '';
      console.log(`    ${name} ${label}: ${JSON.stringify(got)} (ceiling ${stepCeiling(step, VIEWPORT)} per layer) → z${after.z}${moved}`);
      steps.push({ step, label, got, before, after });
      seen = tiles.length;
      before = after;
    }
    return { count: tiles.length, byLayer: countBy(tiles), firstView, steps, elapsedS: (Date.now() - t0) / 1000 };
  } finally {
    await ctx.close();
  }
}

/**
 * The deterministic checks every budget session gets: the first view and
 * every step stay under their geometric ceilings, per layer, a zoom step
 * changed the zoom by exactly one, and a pan moved the map by exactly what
 * was dragged (not dropped, no glide). Throws on the
 * first breach with the step that caused it.
 */
function assertWithinCeilings(name, result) {
  const vc = viewCeiling(VIEWPORT);
  for (const [layer, n] of Object.entries(result.firstView)) {
    if (n > vc) throw new Error(`${name}: first view asked ${n} ${layer} tiles, over the one-view ceiling ${vc}`);
  }
  for (const s of result.steps) {
    const cap = stepCeiling(s.step, VIEWPORT);
    for (const [layer, n] of Object.entries(s.got)) {
      if (n > cap) throw new Error(`${name}: ${s.label} asked ${n} ${layer} tiles, over its ceiling ${cap}`);
    }
    if (s.step.op !== 'pan') {
      const want = s.before.z + (s.step.op === 'zoomIn' ? 1 : -1);
      if (s.after.z !== want) {
        throw new Error(`${name}: ${s.label} went z${s.before.z} → z${s.after.z}, not z${want}: the session is not the scripted one`);
      }
      continue;
    }
    if (s.after.z !== s.before.z) {
      throw new Error(`${name}: ${s.label} changed the zoom z${s.before.z} → z${s.after.z}`);
    }
    {
      const mx = s.before.x - s.after.x, my = s.before.y - s.after.y;
      if (mx === 0 && my === 0) {
        throw new Error(`${name}: ${s.label} did not move the map: the drag was dropped`);
      }
      if (Math.abs(mx - s.step.dx) > 2 || Math.abs(my - s.step.dy) > 2) {
        throw new Error(`${name}: ${s.label} moved the map ${mx},${my} px: the drag glided, so the count is not deterministic`);
      }
    }
  }
}

module.exports = {
  TILE_PX, PAN_HOLD_MS, USER_VIEW_S, QUIET_MS, HEADROOM, TILE_PNG, VIEWPORT, DIRECTIONS,
  zoomIn, zoomOut, pans, times,
  viewCeiling, stepCeiling, sessionCeiling, nominalSeconds, requiredPerMin,
  runSession, assertWithinCeilings,
};
