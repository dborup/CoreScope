/**
 * E2E: Live kiosk mode (#883 M1 — shell, tour, start and exit; #883 M2 —
 * TV layout, big numbers, large Moments).
 *
 * /api/config/areas/polygons is stubbed with three areas (the fixture
 * config has none), the shared WebSocket is replaced by page.routeWebSocket
 * and the page runs on Playwright's clock with reduced motion, so each tour
 * step is an instant fitBounds at a known time.
 *
 *   1. #/live?kiosk=1&tour=aaa,BBB,nope,ccc&every=45: body.kiosk; the top
 *      nav, bottom nav, header, stats, controls, feed, legend, VCR bar and
 *      the map's zoom/layer/toggle controls are hidden; the map credit is
 *      visible, non-empty and inside the 3 % safe inset (M2).
 *   2. The start overlay is up and focused; Enter starts it (and is not an
 *      interaction that pauses the tour).
 *   3. Step 1 is area AAA (exact key matched case-insensitively) with
 *      "1 / 3"; "nope" is skipped with an on-screen note.
 *   4. The cursor is none after 3 s without a move; a move brings it back.
 *   5. After `every` s the map centre lies in area BBB and the label reads
 *      it; the step makes 0 API requests and the areas were fetched once.
 *   6. pointerdown on the map pauses: no step at `every`; the tour resumes
 *      after resumeAfter (90 s at every=45) at the next step.
 *   7. Escape (two-step when fullscreen) exits to #/live: chrome back,
 *      LiveKiosk.liveCount() === 0, live-map-view / live-fullscreen /
 *      meshcore-theme / live-moments byte-identical to before.
 *   8. Enter and leave kiosk 10 times: one controller at most,
 *      liveCount() === 0, and kiosk adds no window/document listener (CDP
 *      counts, against plain Live round trips: Live itself leaks a few per
 *      mount today, fixed in #883 M3).
 *   9. VCR out of LIVE pauses the tour; LIVE again resumes at the next step.
 *      Follow (LIVE VCR) pauses it too; stopping Follow resumes it.
 *  10. The router clears a stray body.kiosk on a non-live route (app.js).
 *  11. ?follow= and ?node= are ignored in kiosk; ?moments=1 applies without
 *      writing live-moments, and its pause and drawer buttons are hidden.
 *      Without a tour the view is the map defaults, not the saved view.
 *  12. The kiosk page fills the viewport before Live's resize timer runs,
 *      and the map credit sits at the bottom edge before Live measures the
 *      (hidden) VCR bar.
 *  13. 375x560 touch smoke: no horizontal scroll, credit visible, label up,
 *      no Moments bar (on a phone it is only the drawer button).
 *  14. 375x560 and 360x640 touch, cold link, real taps: tap a node, tap the
 *      panel's close button. Kiosk stays, the panel closes ("Exit kiosk"
 *      is hidden while the panel is open and covers no part of it).
 *
 * M2 (stubbed /api/nodes/network-status, /api/analytics/areas, /api/moments
 * and `moment` frames on the stubbed socket):
 *  15. The tiles: packets/min, active nodes with the tour area's count,
 *      the rolling records; a newer record_link frame updates the tile, an
 *      older one does not; a step changes the area part with no request.
 *  16. Request budget: at most 2 bulk calls per 60 s, 0 while the tab is
 *      hidden (visibilitychange), one round on show; the tiles' DOM is not
 *      written while nothing changes.
 *  17. Large Moments: two cards at most (visibleCards 3), scaled sizes,
 *      times refreshed on a held card; on without &moments= and not saved.
 *  18. The kiosk theme: dark by default over a light viewer, never saved,
 *      the viewer's back on exit; &theme=auto keeps it; &theme=light.
 *  19. 1920x1080: --kiosk-scale 1.5, the credit inside the safe inset at
 *      ≥ 14px, no nav-drawer shadow at the left edge.
 *  20. Outside kiosk the Moments sizes are the old px (token defaults).
 *  21. 375x560: the tiles are one row at the top, inside the viewport.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-883-kiosk-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
if (!['localhost', '127.0.0.1', '[::1]'].includes(new URL(BASE).hostname)) throw new Error('local server only');

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + (e && e.stack || e)); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

// The fixture's nodes are around the Bay Area; the response order is not
// the tour order (the server ranges over a Go map).
const AREAS = [
  { key: 'CCC', label: 'Kiosk Area C', latMin: 36.9, latMax: 37.1, lonMin: -122.15, lonMax: -121.9 },
  { key: 'AAA', label: 'Kiosk Area A', latMin: 37.2, latMax: 37.45, lonMin: -122.05, lonMax: -121.75 },
  { key: 'BBB', label: 'Kiosk Area B', polygon: [[37.70, -122.52], [37.83, -122.52], [37.83, -122.35], [37.70, -122.35]] },
];
const BOUNDS = { AAA: [[37.2, -122.05], [37.45, -121.75]], BBB: [[37.70, -122.52], [37.83, -122.35]], CCC: [[36.9, -122.15], [37.1, -121.9]] };
const EVERY = 45;
const KIOSK = '#/live?kiosk=1&tour=aaa,BBB,nope,ccc&every=' + EVERY + '&region=';
const SEEDED = {
  'live-map-view': '{"lat":10.5,"lng":20.25,"zoom":5}',
  'live-fullscreen': 'false',
  'meshcore-theme': 'dark',
  'live-moments': 'false',
};
const HIDDEN = ['.top-nav', '.bottom-nav', '.live-header-body', '.live-header-critical', '.live-stats-row',
  '.live-controls-body', '.vcr-bar', '.live-feed', '.live-legend', '.leaflet-control-zoom', '.leaflet-control-layers',
  '#liveFullscreenToggle', '#liveControlsToggle', '#legendToggleBtn'];

async function open(browser, opts) {
  opts = opts || {};
  const ctx = await browser.newContext(Object.assign({ viewport: opts.viewport || { width: 1280, height: 720 }, reducedMotion: 'reduce' },
    opts.touch ? { hasTouch: true, isMobile: true } : {}));
  const h = { ctx, api: [], areaRequests: 0, errors: [], warnings: [], sockets: [] };
  await ctx.route(/\/api\/config\/areas\/polygons(\?|$)/, (route) => { h.areaRequests++; return route.fulfill({ json: AREAS }); });
  await ctx.routeWebSocket(/.*/, (ws) => { h.sockets.push(ws); });
  // A frame on the app's (stubbed) socket.
  h.send = (frame) => {
    const ws = h.sockets[h.sockets.length - 1];
    assert(ws, 'the app socket connected');
    ws.send(JSON.stringify(frame));
  };
  const page = await ctx.newPage();
  h.page = page;
  page.on('pageerror', (e) => h.errors.push(String(e.stack || e.message).split('\n').slice(0, 4).join(' | ')));
  page.on('console', (m) => { if (m.type() === 'warning') h.warnings.push(m.text()); });
  page.on('request', (r) => { if (new URL(r.url()).pathname.startsWith('/api/')) h.api.push(new URL(r.url()).pathname); });
  if (opts.clock) await page.clock.install();
  h.kioskReady = () => page.waitForFunction(() => window._liveKiosk && window._liveKiosk() &&
    window._liveKiosk().stats().steps > 0, null, { timeout: 20000 });
  h.liveReady = () => page.waitForFunction(() => window._liveWSHandler && window._liveWSHandler() &&
    window._liveGetMap && window._liveGetMap(), null, { timeout: 20000 });
  h.stats = () => page.evaluate(() => window._liveKiosk() && window._liveKiosk().stats());
  h.label = () => page.evaluate(() => {
    const l = document.querySelector('.live-kiosk-step-label'), c = document.querySelector('.live-kiosk-step-count');
    return (l ? l.textContent : '') + '|' + (c ? c.textContent : '');
  });
  h.centreIn = (key) => page.evaluate((b) => {
    const c = window._liveGetMap().getCenter();
    return c.lat > b[0][0] && c.lat < b[1][0] && c.lng > b[0][1] && c.lng < b[1][1];
  }, BOUNDS[key]);
  return h;
}

// Visible = laid out with a size and not visibility:hidden (display:none
// anywhere up the tree gives an empty rect).
function chromeState(page, sels) {
  return page.evaluate((sels) => sels.map((s) => {
    const els = [...document.querySelectorAll(s)];
    const vis = els.some((e) => { const r = e.getBoundingClientRect(); return r.width > 0 && r.height > 0 && getComputedStyle(e).visibility !== 'hidden'; });
    return { s, n: els.length, vis };
  }), sels);
}
// The 3 % safe inset (--kiosk-safe-inset: 3vmin) around the credit's corner.
function inSafeCorner(c) {
  const inset = 0.03 * Math.min(c.vw, c.vh);
  return c.b <= c.vh - inset + 0.5 && c.b >= c.vh - inset - 2 && c.r <= c.vw - inset + 0.5 && c.r >= c.vw - inset - 2;
}
function credit(page) {
  return page.evaluate(() => {
    const a = document.querySelector('#liveMap .leaflet-control-attribution');
    if (!a) return null;
    const r = a.getBoundingClientRect();
    return { text: a.textContent.trim(), l: r.left, t: r.top, r: r.right, b: r.bottom, w: r.width, h: r.height,
      vw: innerWidth, vh: innerHeight, vis: getComputedStyle(a).visibility, fs: parseFloat(getComputedStyle(a).fontSize) };
  });
}
// Moments on (the fixture config has no moments block) with one moment in
// the history, so the Moments bar shows.
const MOMENTS = [{ id: 'kiosk-1', kind: 'back_online', at: new Date(Date.now() - 600000).toISOString(), silentHours: 37,
  node: { publicKey: 'b'.repeat(64), name: 'Kiosk moment', role: 'repeater', lat: 37.3, lon: -121.9 } }];
async function momentsOn(ctx, history) {
  await ctx.route(/\/api\/config\/client(\?|$)/, async (route) => {
    const res = await route.fetch();
    const body = await res.json();
    body.moments = { enabled: true, historySize: 50 };
    await route.fulfill({ response: res, json: body });
  });
  await ctx.route(/\/api\/moments(\?|$)/, (route) => route.fulfill({ json: { moments: history || MOMENTS } }));
}
const MOMENT_BUTTONS = ['.live-moments-pause', '.live-moments-open'];
// The middle of a node marker that a finger there would hit, clear of the
// screen edges, or null.
function tappableNode(page) {
  return page.evaluate(() => {
    for (const m of document.querySelectorAll('.live-node-marker')) {
      const r = m.getBoundingClientRect();
      const x = r.left + r.width / 2, y = r.top + r.height / 2;
      if (x < 40 || y < 120 || x > innerWidth - 40 || y > innerHeight - 40) continue;
      const t = document.elementFromPoint(x, y);
      if (t && (t === m || m.contains(t))) return { x, y };
    }
    return null;
  });
}
// A box that held still between two reads (a panel's slide has ended).
async function settledBox(page, sel) {
  let prev = null;
  for (let i = 0; i < 40; i++) {
    const b = await page.evaluate((sel) => { const e = document.querySelector(sel); if (!e) return null;
      const r = e.getBoundingClientRect(); return { l: r.left, t: r.top, w: r.width, h: r.height }; }, sel);
    if (b && prev && b.w > 0 && JSON.stringify(b) === JSON.stringify(prev)) return { x: b.l + b.w / 2, y: b.t + b.h / 2, box: b };
    prev = b;
    await page.waitForTimeout(100);
  }
  throw new Error('no settled box for ' + sel + ': ' + JSON.stringify(prev));
}

function readStorage(page) {
  return page.evaluate((keys) => Object.fromEntries(keys.map((k) => [k, localStorage.getItem(k)])), Object.keys(SEEDED));
}

(async () => {
  const browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_PATH || undefined });
  try {
    console.log('\n=== #883 M1 Live kiosk E2E ===');
    const h = await open(browser, { clock: true });
    const page = h.page;
    // Seed the viewer's own state on another page, then enter kiosk the way
    // a link would.
    await page.goto(BASE + '/?r=' + Date.now() + '#/packets', { waitUntil: 'domcontentloaded' });
    await page.evaluate((s) => { for (const [k, v] of Object.entries(s)) localStorage.setItem(k, v); }, SEEDED);
    let mounted = false;
    await step('a kiosk link mounts Live with the kiosk controller', async () => {
      await page.evaluate((hash) => { location.hash = hash; }, KIOSK);
      await h.liveReady();
      await h.kioskReady();
      mounted = true;
    });
    if (!mounted) throw new Error('no kiosk controller: the rest cannot run');

    await step('chrome hidden; body.kiosk; the map credit stays visible and inside the viewport', async () => {
      assert(await page.evaluate(() => document.body.classList.contains('kiosk')), 'body.kiosk');
      const st = await chromeState(page, HIDDEN);
      const shown = st.filter((x) => x.vis).map((x) => x.s);
      assert(!shown.length, 'still visible: ' + shown.join(', '));
      assert(st.find((x) => x.s === '.top-nav').n === 1 && st.find((x) => x.s === '.vcr-bar').n === 1, 'checked real nodes');
      const c = await credit(page);
      assert(c && c.text.length > 0, 'credit text: ' + JSON.stringify(c));
      assert(c.w > 0 && c.h > 0 && c.vis !== 'hidden', 'credit visible: ' + JSON.stringify(c));
      assert(c.l >= 0 && c.t >= 0 && c.r <= c.vw + 0.5 && c.b <= c.vh + 0.5, 'credit inside the viewport: ' + JSON.stringify(c));
      // M2: inside the 3 % safe inset, at its edge (not 58px up).
      assert(inSafeCorner(c), 'credit at the safe inset\'s bottom-right corner: ' + JSON.stringify(c));
      assert(c.fs >= 11, 'credit font ≥ 11px: ' + c.fs);
      const vcr = await page.evaluate(() => getComputedStyle(document.querySelector('.live-page')).getPropertyValue('--vcr-bar-height').trim());
      assert(vcr === '0px', '--vcr-bar-height: ' + vcr);
    });

    await step('step 1 is area AAA (case-insensitive key), "1 / 3"; "nope" skipped with a note and a warning', async () => {
      assert(await h.label() === 'Kiosk Area A|1 / 3', await h.label());
      assert(await h.centreIn('AAA'), 'centre in AAA');
      const note = await page.evaluate(() => { const n = document.querySelector('.live-kiosk-note'); return n && n.textContent; });
      assert(note === 'Skipped: nope', 'note: ' + note);
      assert(h.warnings.some((w) => /skipped \(unknown\): nope/.test(w)), 'warning: ' + h.warnings.join('; '));
    });

    await step('start overlay: focused; Enter closes it without pausing the tour', async () => {
      const o = await page.evaluate(() => ({ btn: !!document.querySelector('.live-kiosk-start'),
        focused: document.activeElement && document.activeElement.classList.contains('live-kiosk-start') }));
      assert(o.btn && o.focused, JSON.stringify(o));
      await page.keyboard.press('Enter');
      await page.waitForFunction(() => !document.querySelector('.live-kiosk-overlay'));
      const s = await h.stats();
      assert(!s.paused && !s.overlay, JSON.stringify(s));
    });

    await step('idle cursor: none after 3 s without a move, back on a move', async () => {
      await page.mouse.move(400, 300);
      const cur = () => page.evaluate(() => ({ idle: document.body.classList.contains('kiosk-idle'),
        map: getComputedStyle(document.querySelector('#liveMap')).cursor,
        exit: getComputedStyle(document.querySelector('.live-kiosk-exit')).visibility }));
      const moving = await cur();
      assert(!moving.idle && moving.map !== 'none' && moving.exit === 'visible', 'after a move: ' + JSON.stringify(moving));
      await page.clock.runFor(3500);
      const idle = await cur();
      assert(idle.idle && idle.map === 'none' && idle.exit === 'hidden', 'idle: ' + JSON.stringify(idle));
      assert(!(await h.stats()).paused, 'a move does not pause');
    });

    await step('after `every` s: area BBB, "2 / 3"; 0 API requests for the step; areas fetched once', async () => {
      const s0 = await h.stats();
      assert(s0.step === 0, JSON.stringify(s0));
      // The clock moves in 1 s slices. The slice with the step must hold no
      // request that the quiet slices do not also show: the app's own
      // pollers (e.g. /api/stats) tick on their own schedule, a request
      // caused by the step would not.
      let stepApi = null;
      const quiet = new Set();
      for (let i = 0; i < EVERY + 5 && stepApi === null; i++) {
        const before = h.api.length;
        await page.clock.runFor(1000);
        await page.waitForTimeout(30); // let request events land
        const slice = h.api.slice(before);
        if ((await h.stats()).step === 1) stepApi = slice;
        else slice.forEach((u) => quiet.add(u));
      }
      assert(stepApi !== null, 'the tour reached step 2');
      const caused = stepApi.filter((u) => !quiet.has(u) || /^\/api\/(config\/areas|nodes|analytics\/areas)/.test(u));
      assert(caused.length === 0, 'API requests caused by the step: ' + caused.join(', ') + ' (background: ' + [...quiet].join(', ') + ')');
      console.log('    step second: ' + stepApi.length + ' request(s), all background pollers [' + stepApi.join(', ') + ']');
      assert(await h.label() === 'Kiosk Area B|2 / 3', await h.label());
      assert(await h.centreIn('BBB'), 'centre in BBB');
      assert(h.areaRequests === 1 && (await h.stats()).fetches === 1, 'areas once: ' + h.areaRequests);
    });

    await step('pointerdown pauses; no step at `every`; resumes after 90 s at the next step', async () => {
      await page.mouse.move(640, 400);
      await page.mouse.down();
      await page.mouse.up();
      const s = await h.stats();
      assert(s.paused && s.reasons.includes('interaction'), JSON.stringify(s));
      await page.clock.runFor((EVERY + 5) * 1000);
      assert((await h.stats()).step === 1, 'no step while paused');
      await page.clock.runFor((90 - EVERY - 5) * 1000 + 1000);
      const r = await h.stats();
      assert(!r.paused && r.step === 2, 'resumed at the next step: ' + JSON.stringify(r));
      assert(await h.label() === 'Kiosk Area C|3 / 3', await h.label());
      assert(await h.centreIn('CCC'), 'centre in CCC');
    });

    await step('VCR out of LIVE pauses the tour (mode); back to LIVE resumes at the next step', async () => {
      // The VCR bar is hidden in kiosk; its buttons are driven directly.
      await page.evaluate(() => document.getElementById('vcrPauseBtn').click());
      const s = await h.stats();
      assert(s.paused && s.reasons.includes('mode'), JSON.stringify(s));
      await page.clock.runFor((EVERY + 5) * 1000);
      assert((await h.stats()).step === 2, 'no step while the VCR is paused');
      await page.evaluate(() => document.getElementById('vcrLiveBtn').click());
      const r = await h.stats();
      assert(!r.paused && r.step === 0, 'resumed at the next step: ' + JSON.stringify(r));
      assert(await h.label() === 'Kiosk Area A|1 / 3', await h.label());
    });

    await step('Follow pauses the tour (mode) with the VCR still LIVE; stopping it resumes at the next step', async () => {
      // Follow starts in kiosk from a path popup or a feed card; the seam
      // starts it the same way, and the pause is read in the same task.
      const s = await page.evaluate(() => { window._liveStartFollow('abcdef0123456789');
        return { st: window._liveKiosk().stats(), vcr: window._liveVCR().mode, following: !!window._liveFollowState() }; });
      assert(s.following && s.vcr === 'LIVE', JSON.stringify(s));
      assert(s.st.paused && s.st.reasons.includes('mode'), JSON.stringify(s));
      const r = await page.evaluate(() => { window._liveStopFollow(); return window._liveKiosk().stats(); });
      assert(!r.paused && r.step === 1, 'resumed at the next step: ' + JSON.stringify(r));
    });

    await step('the viewer\'s storage is untouched while kiosk runs', async () => {
      const now = await readStorage(page);
      assert(JSON.stringify(now) === JSON.stringify(SEEDED), JSON.stringify(now));
    });

    await step('Escape exits to #/live: chrome back, liveCount() === 0, storage byte-identical', async () => {
      // Two steps when the Enter above put the browser in fullscreen: the
      // first press leaves fullscreen and keeps kiosk.
      if (await page.evaluate(() => !!document.fullscreenElement)) {
        await page.keyboard.press('Escape');
        await page.waitForFunction(() => !document.fullscreenElement);
        assert(await page.evaluate(() => document.body.classList.contains('kiosk')), 'still kiosk after leaving fullscreen');
      }
      await page.keyboard.press('Escape');
      await page.waitForFunction(() => !document.body.classList.contains('kiosk') && window._liveKiosk && !window._liveKiosk() &&
        window._liveGetMap && window._liveGetMap(), null, { timeout: 20000 });
      const hash = await page.evaluate(() => location.hash);
      assert(!/kiosk|tour|every/.test(hash) && /region=/.test(hash), 'kiosk params stripped, others kept: ' + hash);
      const st = await chromeState(page, ['.top-nav', '.vcr-bar', '.live-header-body', '.leaflet-control-zoom', '#liveFullscreenToggle']);
      const hidden = st.filter((x) => !x.vis).map((x) => x.s);
      assert(!hidden.length, 'still hidden: ' + hidden.join(', '));
      const k = await page.evaluate(() => ({ live: window.LiveKiosk.liveCount(), ui: document.querySelectorAll('[class^="live-kiosk"]').length,
        idle: document.body.classList.contains('kiosk-idle') }));
      assert(k.live === 0 && k.ui === 0 && !k.idle, JSON.stringify(k));
      const now = await readStorage(page);
      assert(JSON.stringify(now) === JSON.stringify(SEEDED), 'storage: ' + JSON.stringify(now));
    });

    // The targets that outlive a mount are window and document; their
    // listeners are counted exactly (CDP DOMDebugger.getEventListeners).
    // Live itself leaks on every remount today, kiosk or not: one window
    // resize, one window orientationchange and one document click listener
    // per mount, plus ~100 listeners and ~1300 nodes in detached map
    // containers (#883 M3 fixes Live's leaks). So the baseline is a plain
    // Live round trip: 10 kiosk round trips must add exactly what 10 plain
    // ones add on window and document, i.e. kiosk itself adds nothing. The
    // page-wide JSEventListeners metric is logged for the record.
    await step('enter and leave 10 times: kiosk adds no window/document listener, liveCount() === 0', async () => {
      const cdp = await page.context().newCDPSession(page);
      await cdp.send('Performance.enable');
      const on = async (expression) => {
        const { result } = await cdp.send('Runtime.evaluate', { expression });
        const { listeners } = await cdp.send('DOMDebugger.getEventListeners', { objectId: result.objectId });
        await cdp.send('Runtime.releaseObject', { objectId: result.objectId });
        return listeners.length;
      };
      const listeners = async () => {
        await cdp.send('HeapProfiler.collectGarbage');
        const m = await cdp.send('Performance.getMetrics');
        return { page: m.metrics.find((x) => x.name === 'JSEventListeners').value, win: await on('window'), doc: await on('document') };
      };
      const enter = async () => {
        await page.evaluate((hash) => { location.hash = hash; }, KIOSK);
        await h.kioskReady();
      };
      const live = async (hash) => {
        await page.evaluate((hash) => { location.hash = hash; }, hash);
        await page.waitForFunction(() => !document.body.classList.contains('kiosk') && window._liveGetMap && window._liveGetMap() &&
          window._liveWSHandler && window._liveWSHandler(), null, { timeout: 20000 });
      };
      const leave = () => live('#/live');
      await enter(); await leave(); // warm-up: lazy singletons bind once
      const base = await listeners();
      for (let i = 0; i < 10; i++) { await live('#/live?plain=1'); await leave(); }
      const plain = await listeners();
      for (let i = 0; i < 10; i++) {
        await enter();
        assert(await page.evaluate(() => window.LiveKiosk.liveCount()) === 1, 'one controller at a time');
        await leave();
      }
      const kiosk = await listeners();
      assert(await page.evaluate(() => window.LiveKiosk.liveCount()) === 0, 'liveCount 0');
      console.log('    window/document listeners: base ' + base.win + '/' + base.doc + ', +10 plain ' + plain.win + '/' + plain.doc +
        ', +10 kiosk ' + kiosk.win + '/' + kiosk.doc);
      console.log('    JSEventListeners (page): base ' + base.page + ', +10 plain ' + plain.page + ' (Δ' + (plain.page - base.page) +
        '), +10 kiosk ' + kiosk.page + ' (Δ' + (kiosk.page - plain.page) + ')');
      assert(kiosk.win - plain.win === plain.win - base.win && kiosk.doc - plain.doc === plain.doc - base.doc,
        'window/document: 10 kiosk round trips add ' + (kiosk.win - plain.win) + '/' + (kiosk.doc - plain.doc) +
        ', 10 plain ones ' + (plain.win - base.win) + '/' + (plain.doc - base.doc));
    });

    await step('the router clears a stray body.kiosk on a non-live route', async () => {
      await page.evaluate(() => { location.hash = '#/packets'; });
      await page.waitForFunction(() => !document.querySelector('.live-page'));
      await page.evaluate(() => document.body.classList.add('kiosk', 'kiosk-idle'));
      await page.evaluate(() => { location.hash = '#/nodes'; });
      await page.waitForFunction(() => location.hash === '#/nodes');
      await page.waitForTimeout(200);
      const cls = await page.evaluate(() => document.body.className);
      assert(!/\bkiosk\b|kiosk-idle/.test(cls), 'body: ' + cls);
    });

    await step('no page errors', async () => {
      assert(!h.errors.length, h.errors.join('; '));
    });
    await h.ctx.close();

    await step('kiosk ignores ?follow= and ?node=, and applies ?moments=1 without saving it', async () => {
      const m = await open(browser);
      await momentsOn(m.ctx);
      await m.page.goto(BASE + '/?r=' + Date.now() + '#/packets', { waitUntil: 'domcontentloaded' });
      await m.page.evaluate(() => { localStorage.setItem('live-moments', 'false'); localStorage.removeItem('live-node-filter'); });
      await m.page.evaluate((hash) => { location.hash = hash; }, KIOSK + '&moments=1&follow=abcdef0123456789&node=zzz-kiosk');
      await m.liveReady();
      await m.kioskReady();
      await m.page.waitForFunction(() => window._liveMoments && window._liveMoments(), null, { timeout: 10000 });
      const s = await m.page.evaluate(() => ({ follow: !!window._liveFollowState(), keys: window._liveGetNodeFilterKeys().slice(),
        moments: localStorage.getItem('live-moments'), hash: location.hash }));
      assert(!s.follow, 'no follow in kiosk');
      assert(!s.keys.length, 'no node filter in kiosk: ' + JSON.stringify(s.keys));
      assert(s.moments === 'false', 'live-moments not written: ' + s.moments);
      // §4.3: the Moments pause and drawer buttons are chrome too; the bar
      // keeps its count.
      await m.page.waitForFunction(() => { const b = document.querySelector('.live-moments-bar'); return b && !b.hidden; });
      const mb = await chromeState(m.page, ['.live-moments-bar', '.live-moments-count'].concat(MOMENT_BUTTONS));
      assert(mb.every((x) => x.n > 0), 'Moments bar and buttons rendered: ' + JSON.stringify(mb));
      assert(mb[0].vis && mb[1].vis, 'the Moments bar and count show: ' + JSON.stringify(mb));
      assert(!mb.some((x) => MOMENT_BUTTONS.includes(x.s) && x.vis), 'Moments buttons hidden in kiosk: ' + JSON.stringify(mb));
      // No tour: a static view from the map defaults, not the viewer's saved view.
      await m.page.evaluate(() => localStorage.setItem('live-map-view', '{"lat":10.5,"lng":20.25,"zoom":5}'));
      await m.page.evaluate(() => { location.hash = '#/packets'; });
      await m.page.waitForFunction(() => !document.querySelector('.live-page'));
      await m.page.evaluate(() => { location.hash = '#/live?kiosk=1'; });
      await m.liveReady();
      await m.page.waitForFunction(() => window._liveKiosk && window._liveKiosk());
      const v = await m.page.evaluate(() => { const c = window._liveGetMap().getCenter(); return { lat: c.lat, lng: c.lng,
        steps: window._liveKiosk().stats().steps, saved: localStorage.getItem('live-map-view') }; });
      const defaults = await (await m.page.request.get(BASE + '/api/config/map')).json();
      assert(Math.abs(v.lat - defaults.center[0]) < 0.01 && Math.abs(v.lng - defaults.center[1]) < 0.01,
        'static view at the map defaults ' + JSON.stringify(defaults.center) + ', got ' + JSON.stringify(v));
      assert(v.steps === 0 && v.saved === '{"lat":10.5,"lng":20.25,"zoom":5}', JSON.stringify(v));
      assert(!m.errors.length, m.errors.join('; '));
      await m.ctx.close();
    });

    await step('the kiosk page fills the viewport from the first paint, before Live measures it', async () => {
      const m = await open(browser);
      // A paused clock holds Live's 50 ms resize timer, so only CSS sizes it.
      // The nodes request is held: Live awaits it after it builds the map and
      // before it measures the VCR bar, so the map credit is laid out with
      // CSS alone (no inline --vcr-bar-height yet; the base fallback is 58px).
      let release = null;
      await m.ctx.route(/\/api\/nodes(\?|$)/, (route) => new Promise((res) => {
        release = () => res(route.continue().catch(() => {}));
      }));
      await m.page.clock.install();
      await m.page.clock.pauseAt(Date.now() + 1000);
      await m.page.goto(BASE + '/?r=' + Date.now() + '#/live?kiosk=1', { waitUntil: 'domcontentloaded' });
      await m.page.waitForSelector('.live-page');
      const r = await m.page.evaluate(() => { const b = document.querySelector('.live-page').getBoundingClientRect();
        return { top: b.top, bottom: b.bottom, vh: innerHeight, inline: document.querySelector('.live-page').style.height }; });
      assert(r.inline === '' && r.top === 0 && Math.abs(r.bottom - r.vh) < 1, JSON.stringify(r));
      await m.page.waitForSelector('#liveMap .leaflet-control-attribution');
      for (let i = 0; i < 100 && !release; i++) await m.page.waitForTimeout(50);
      const c = await m.page.evaluate(() => {
        const a = document.querySelector('#liveMap .leaflet-control-attribution').getBoundingClientRect();
        return { inline: document.querySelector('.live-page').style.getPropertyValue('--vcr-bar-height'), b: a.bottom, h: a.height, vw: innerWidth, vh: innerHeight };
      });
      assert(release, 'the nodes request is held');
      assert(c.inline === '', 'Live has not measured the VCR bar yet: ' + JSON.stringify(c));
      const inset = 0.03 * Math.min(c.vw, c.vh);
      assert(c.h > 0 && c.b >= c.vh - inset - 2 && c.b <= c.vh - inset + 0.5, 'credit at the safe inset before the measure: ' + JSON.stringify(c));
      if (release) release();
      await m.ctx.close();
    });

    await step('375x560 touch smoke: no sideways scroll, credit and label visible, no Moments bar', async () => {
      const m = await open(browser, { viewport: { width: 375, height: 560 }, touch: true });
      await momentsOn(m.ctx);
      await m.page.goto(BASE + '/?r=' + Date.now() + KIOSK + '&moments=1', { waitUntil: 'domcontentloaded' });
      await m.liveReady();
      await m.kioskReady();
      // The bar has a moment to show (not [hidden]); on a phone it holds only
      // the drawer button, so kiosk hides all of it.
      await m.page.waitForFunction(() => { const b = document.querySelector('.live-moments-bar'); return b && !b.hidden; },
        null, { timeout: 10000 });
      await m.page.waitForTimeout(300);
      const s = await m.page.evaluate(() => ({ sw: document.documentElement.scrollWidth, vw: innerWidth,
        label: (() => { const r = document.querySelector('.live-kiosk-step').getBoundingClientRect(); return [r.left, r.top, r.right, r.bottom]; })() }));
      assert(s.sw <= s.vw, 'horizontal scroll: ' + JSON.stringify(s));
      assert(s.label[0] >= 0 && s.label[2] <= s.vw, 'label inside: ' + JSON.stringify(s));
      const c = await credit(m.page);
      assert(c && c.text && c.w > 0 && c.l >= 0 && c.r <= c.vw + 0.5 && c.b <= c.vh + 0.5, 'credit: ' + JSON.stringify(c));
      const st = await chromeState(m.page, ['.bottom-nav', '.top-nav', '.vcr-bar', '.live-moments-bar'].concat(MOMENT_BUTTONS));
      assert(!st.some((x) => x.vis), 'chrome: ' + JSON.stringify(st));
      assert(!m.errors.length, m.errors.join('; '));
      await m.ctx.close();
    });

    // review PR#939 F1: on a phone "Exit kiosk" sat over the node panel's
    // close button, and the tap that opens a node wakes it for 3 s.
    for (const viewport of [{ width: 375, height: 560 }, { width: 360, height: 640 }]) {
      await step(viewport.width + 'x' + viewport.height + ' touch, cold link: tap a node, tap the panel\'s close button; kiosk stays', async () => {
        const m = await open(browser, { viewport, touch: true });
        const p = m.page;
        await p.goto(BASE + '/?r=' + Date.now() + '#/live?kiosk=1', { waitUntil: 'domcontentloaded' });
        await m.liveReady();
        await p.waitForFunction(() => window._liveKiosk && window._liveKiosk() && document.querySelector('.live-node-marker'),
          null, { timeout: 20000 });
        // Finger taps, spaced past Chromium's double-tap window.
        const tap = async (pt) => { await p.touchscreen.tap(pt.x, pt.y); await p.waitForTimeout(600); };
        if (await p.$('.live-kiosk-start')) {
          await tap(await settledBox(p, '.live-kiosk-start'));
          await p.waitForFunction(() => !document.querySelector('.live-kiosk-overlay'));
        }
        const node = await tappableNode(p);
        assert(node, 'a node marker to tap');
        await tap(node);
        await p.waitForFunction(() => !document.getElementById('liveNodeDetail').classList.contains('hidden'));
        const x = await settledBox(p, '#nodeDetailClose');
        const open1 = await p.evaluate((pt) => {
          const t = document.elementFromPoint(pt.x, pt.y), e = document.querySelector('.live-kiosk-exit');
          return { idle: document.body.classList.contains('kiosk-idle'), exitShown: e.getBoundingClientRect().width > 0,
            onClose: !!t && !!t.closest('#nodeDetailClose'), hit: t && (t.id || String(t.className && t.className.baseVal != null ? t.className.baseVal : t.className)) };
        }, x);
        assert(!open1.idle, 'the node tap woke kiosk: ' + JSON.stringify(open1));
        assert(!open1.exitShown && open1.onClose, 'close button not covered: ' + JSON.stringify(open1));
        await tap(x);
        const after = await p.evaluate(() => ({ kiosk: document.body.classList.contains('kiosk'), hash: location.hash,
          open: !document.getElementById('liveNodeDetail').classList.contains('hidden'), live: window.LiveKiosk.liveCount(),
          exitShown: document.querySelector('.live-kiosk-exit').getBoundingClientRect().width > 0 }));
        assert(after.kiosk && /kiosk=1/.test(after.hash) && after.live === 1, 'still kiosk: ' + JSON.stringify(after));
        assert(!after.open, 'the panel closed: ' + JSON.stringify(after));
        assert(after.exitShown, 'Exit kiosk back once the panel is closed: ' + JSON.stringify(after));
        assert(!m.errors.length, m.errors.join('; '));
        await m.ctx.close();
      });
    }

    // ── #883 M2: TV layout, big numbers, large Moments ────────────────────
    const PK = (c) => c.repeat(64);
    const ago = (ms) => new Date(Date.now() - ms).toISOString();
    const DENSITY = [{ areaKey: 'AAA', label: 'Kiosk Area A', total: 60, active: 41 },
      { areaKey: 'BBB', label: 'Kiosk Area B', total: 30, active: 17 }];
    const recLink = (id, km, agoMs) => ({ id, kind: 'record_link', at: ago(agoMs), previousKm: 30,
      link: { a: { publicKey: PK('c'), name: 'Ridge-Rpt', role: 'repeater', lat: 37.3, lon: -121.9 },
              b: { publicKey: PK('d'), name: 'Hill-Rpt', role: 'repeater', lat: 37.34, lon: -121.64 }, distanceKm: km } });
    // at: the page's own (possibly fake) clock, in ms.
    const newNode = (id, i, at) => ({ id, kind: 'new_node', at: new Date(at).toISOString(),
      node: { publicKey: PK('e'), name: 'Kiosk node ' + i, role: 'repeater', lat: 37.3 + i / 100, lon: -121.9 } });
    const HISTORY = [
      { id: 'h-busy', kind: 'busiest_repeater', at: ago(300000), relays: 212, windowMinutes: 10,
        node: { publicKey: PK('f'), name: 'Rpt-3', role: 'repeater', lat: 37.4, lon: -122 } },
      recLink('h-link', 38.24, 3600000),
    ];
    const KIOSK_M2 = '#/live?kiosk=1&tour=aaa,BBB&every=' + EVERY;
    async function openM2(o) {
      o = o || {};
      const m = await open(browser, { clock: o.clock, viewport: o.viewport, touch: o.touch });
      m.status = 0; m.areaStats = 0;
      await m.ctx.route(/\/api\/nodes\/network-status(\?|$)/, (r) => { m.status++;
        return r.fulfill({ json: { total: 400, active: 318, degraded: 2, silent: 80, roleCounts: {} } }); });
      await m.ctx.route(/\/api\/analytics\/areas(\?|$)/, (r) => { m.areaStats++; return r.fulfill({ json: { density: DENSITY } }); });
      if (o.moments !== false) await momentsOn(m.ctx, HISTORY);
      m.texts = (sel) => m.page.evaluate((sel) => [...document.querySelectorAll(sel)].map((e) => e.textContent), sel);
      m.bulk = () => m.status + m.areaStats;
      m.now = () => m.page.evaluate(() => Date.now());
      return m;
    }
    async function enterKiosk(m, hash, seed) {
      await m.page.goto(BASE + '/?r=' + Date.now() + '#/packets', { waitUntil: 'domcontentloaded' });
      if (seed) await m.page.evaluate((s) => { for (const [k, v] of Object.entries(s)) localStorage.setItem(k, v); }, seed);
      await m.page.evaluate((hash) => { location.hash = hash; }, hash);
      await m.liveReady();
      await m.kioskReady();
    }
    const fontPx = (page, sel) => page.evaluate((sel) => { const e = document.querySelector(sel); return e ? parseFloat(getComputedStyle(e).fontSize) : null; }, sel);

    {
      const m = await openM2({ clock: true });
      const p = m.page;
      let up = false;
      await step('M2: the tiles show packets/min, active nodes (whole mesh · tour area) and the rolling records', async () => {
        await enterKiosk(m, KIOSK_M2, { 'live-moments': 'false' });
        await p.waitForFunction(() => window._liveMoments && window._liveMoments(), null, { timeout: 10000 });
        await p.clock.runFor(2100); // Live's 2 s rate tick
        await p.waitForFunction(() => { const v = document.querySelectorAll('.live-kiosk-record-value');
          return v.length === 2 && v[1].textContent === 'Rpt-3' && document.querySelectorAll('.live-kiosk-tile-value')[1].textContent === '318'; },
        null, { timeout: 10000 });
        const v = await m.texts('.live-kiosk-tile-value');
        assert(/^\d[\d,]*$/.test(v[0]), 'packets/min: ' + v[0]);
        assert(v[1] === '318', 'active nodes from network-status: ' + v[1]);
        assert((await m.texts('.live-kiosk-tile-area'))[0] === 'Kiosk Area A 41', 'area: ' + (await m.texts('.live-kiosk-tile-area')));
        assert(JSON.stringify(await m.texts('.live-kiosk-tile-label')) === '["packets/min","active nodes"]', await m.texts('.live-kiosk-tile-label'));
        assert(JSON.stringify(await m.texts('.live-kiosk-record-label')) === '["Longest link (24 h)","Busiest (10 min)"]', await m.texts('.live-kiosk-record-label'));
        assert(JSON.stringify(await m.texts('.live-kiosk-record-value')) === '["38.2 km","Rpt-3"]', await m.texts('.live-kiosk-record-value'));
        assert(JSON.stringify(await m.texts('.live-kiosk-record-detail')) === '["Ridge-Rpt ↔ Hill-Rpt","212 relays"]', await m.texts('.live-kiosk-record-detail'));
        // The tiles sit above the credit, inside the safe inset.
        const g = await p.evaluate(() => { const t = document.querySelector('.live-kiosk-tiles').getBoundingClientRect();
          const a = document.querySelector('#liveMap .leaflet-control-attribution').getBoundingClientRect();
          return { tb: t.bottom, tl: t.left, at: a.top, vw: innerWidth, vh: innerHeight }; });
        assert(g.tb <= g.at && g.tl >= 0.03 * Math.min(g.vw, g.vh) - 0.5, 'tiles above the credit, inside the inset: ' + JSON.stringify(g));
        up = true;
      });

      await step('M2: a newer record_link frame updates the tile; an older one does not; Moments on without &moments= and not saved', async () => {
        assert(up, 'tiles up');
        m.send({ type: 'moment', data: recLink('w-link', 61.2, 0) });
        await p.waitForFunction(() => document.querySelector('.live-kiosk-record-value').textContent === '61.2 km', null, { timeout: 5000 });
        m.send({ type: 'moment', data: recLink('w-old', 99, 7200000) });
        await p.clock.runFor(2100);
        assert((await m.texts('.live-kiosk-record-value'))[0] === '61.2 km', 'an older record does not replace the shown one');
        assert(await p.evaluate(() => localStorage.getItem('live-moments')) === 'false', 'live-moments not written');
      });

      await step('M2: a tour step changes the area part with no request', async () => {
        const before = m.bulk();
        for (let i = 0; i < EVERY + 5; i++) {
          await p.clock.runFor(1000);
          if ((await m.texts('.live-kiosk-step-label'))[0] === 'Kiosk Area B') break;
        }
        assert((await m.texts('.live-kiosk-step-label'))[0] === 'Kiosk Area B', 'step 2');
        assert((await m.texts('.live-kiosk-tile-area'))[0] === 'Kiosk Area B 17', 'area part: ' + (await m.texts('.live-kiosk-tile-area')));
        assert(m.bulk() === before, 'no bulk request for the step: ' + (m.bulk() - before));
      });

      // Between two steps nothing on the tiles changes: twenty 2 s rate ticks
      // and one poll with the same answers must not touch their DOM.
      await step('M2: the tiles write nothing while no value changes (rate ticks and a poll with the same answers)', async () => {
        await p.evaluate(() => {
          window.__tileMuts = 0;
          new MutationObserver((l) => { window.__tileMuts += l.length; })
            .observe(document.querySelector('.live-kiosk-tiles'), { subtree: true, childList: true, characterData: true, attributes: true });
        });
        const w0 = (await m.stats()).tileWrites, b0 = m.bulk();
        await p.clock.runFor(EVERY * 1000 - 5000);
        await p.waitForTimeout(50);
        assert((await m.stats()).step === 1, 'still step 2 (no area change in the window)');
        assert(m.bulk() - b0 === 2, 'a poll ran in the window: ' + (m.bulk() - b0));
        const muts = await p.evaluate(() => window.__tileMuts);
        assert(muts === 0, 'tile DOM mutations: ' + muts);
        assert((await m.stats()).tileWrites === w0, 'tileWrites: ' + w0 + ' → ' + (await m.stats()).tileWrites);
      });

      await step('M2: request budget: ≤ 2 bulk calls per 60 s, one network-status and one analytics/areas', async () => {
        const per = [];
        for (let i = 0; i < 3; i++) {
          const b = m.bulk();
          await p.clock.runFor(60000);
          await p.waitForTimeout(50);
          per.push(m.bulk() - b);
        }
        assert(per.every((n) => n <= 2) && per.every((n) => n === 2), 'bulk calls per 60 s: ' + per.join(', '));
        assert(m.status === m.areaStats, 'one of each per round: ' + m.status + '/' + m.areaStats);
        console.log('    bulk calls per 60 s: ' + per.join(', ') + ' (network-status ' + m.status + ', analytics/areas ' + m.areaStats + ' in total)');
      });

      await step('M2: hidden tab → 0 bulk calls; shown again → one round at once', async () => {
        const vis = (hidden) => p.evaluate((hidden) => {
          Object.defineProperty(document, 'hidden', { configurable: true, get: () => hidden });
          Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => (hidden ? 'hidden' : 'visible') });
          document.dispatchEvent(new Event('visibilitychange'));
        }, hidden);
        await vis(true);
        const b = m.bulk();
        await p.clock.runFor(180000);
        await p.waitForTimeout(50);
        assert(m.bulk() === b, 'bulk calls while hidden: ' + (m.bulk() - b));
        assert((await m.stats()).pollTimer === false, 'no poll timer while hidden');
        await vis(false);
        await p.waitForTimeout(100);
        assert(m.bulk() - b === 2, 'one round on show: ' + (m.bulk() - b));
      });

      await step('M2: large Moments: two cards at most (visibleCards 3), kiosk sizes, a held card\'s time refreshes', async () => {
        const t0 = await m.now();
        for (let i = 0; i < 4; i++) m.send({ type: 'moment', data: newNode('w-n' + i, i, t0) });
        await p.waitForFunction(() => document.querySelectorAll('.live-moment-card').length >= 2, null, { timeout: 5000 });
        await p.clock.runFor(50);
        const n = await p.evaluate(() => document.querySelectorAll('.live-moment-card:not(.is-leaving)').length);
        assert(n === 2, 'cards: ' + n);
        assert(await fontPx(p, '.live-moment-title') === 18 && await fontPx(p, '.live-moment-detail') === 16, 'kiosk sizes at scale 1: ' +
          (await fontPx(p, '.live-moment-title')) + '/' + (await fontPx(p, '.live-moment-detail')));
        const icon = await p.evaluate(() => document.querySelector('.live-moment-main .ph-icon').getBoundingClientRect().width);
        assert(icon === 24, 'icon: ' + icon);
        const box = await p.evaluate(() => { const r = document.querySelector('.live-moments').getBoundingClientRect(); return { t: r.top, r: r.right, vw: innerWidth }; });
        assert(box.r <= box.vw - 0.03 * 720 + 0.5 && box.t < 200, 'top-right inside the inset: ' + JSON.stringify(box));
        // Hold the first card with the mouse; its time follows the clock.
        const c = await p.evaluate(() => { const r = document.querySelector('.live-moment-card').getBoundingClientRect(); return { x: r.left + 20, y: r.top + 10 }; });
        await p.mouse.move(c.x, c.y);
        assert((await m.texts('.live-moment-card .live-moment-time'))[0] === 'now', 'built as now');
        await p.clock.runFor(30000);
        const t = (await m.texts('.live-moment-card .live-moment-time'))[0];
        assert(t === '30 s', 'refreshed after 30 s: ' + t);
        await p.mouse.move(5, 5);
      });

      await step('M2: no page errors', async () => { assert(!m.errors.length, m.errors.join('; ')); });
      await m.ctx.close();
    }

    await step('M2: the kiosk theme is dark over a light viewer, never saved, and the viewer\'s comes back on exit', async () => {
      const m = await openM2({ moments: false });
      const p = m.page;
      await p.goto(BASE + '/?r=' + Date.now() + '#/packets', { waitUntil: 'domcontentloaded' });
      await p.evaluate(() => localStorage.setItem('meshcore-theme', 'light'));
      await p.reload({ waitUntil: 'domcontentloaded' });
      const th = () => p.evaluate(() => ({ t: document.documentElement.getAttribute('data-theme'), s: localStorage.getItem('meshcore-theme'),
        cb: (document.getElementById('darkModeCheckbox') || {}).checked }));
      assert((await th()).t === 'light', 'viewer light');
      const visit = async (hash) => { await p.evaluate((hash) => { location.hash = hash; }, hash); await m.liveReady();
        await p.waitForFunction(() => window._liveKiosk && window._liveKiosk()); };
      const leave = async () => { await p.keyboard.press('Escape');
        await p.waitForFunction(() => !document.body.classList.contains('kiosk') && window._liveGetMap && window._liveGetMap(), null, { timeout: 20000 }); };
      await visit('#/live?kiosk=1');
      let s = await th();
      assert(s.t === 'dark' && s.s === 'light', 'kiosk dark, storage light: ' + JSON.stringify(s));
      // app.js's whole theme apply ran (the dark checkbox follows), not only data-theme.
      assert(s.cb === true, 'the dark-mode checkbox follows the kiosk theme: ' + JSON.stringify(s));
      await leave();
      s = await th();
      assert(s.t === 'light' && s.s === 'light' && s.cb === false, 'viewer back: ' + JSON.stringify(s));
      await visit('#/live?kiosk=1&theme=auto');
      assert((await th()).t === 'light', 'auto keeps the viewer\'s theme');
      await leave();
      await p.evaluate(() => localStorage.setItem('meshcore-theme', 'dark'));
      await p.reload({ waitUntil: 'domcontentloaded' });
      await m.liveReady();
      await visit('#/live?kiosk=1&theme=light');
      s = await th();
      assert(s.t === 'light' && s.s === 'dark', 'theme=light over a dark viewer: ' + JSON.stringify(s));
      await leave();
      s = await th();
      assert(s.t === 'dark' && s.s === 'dark', 'dark again: ' + JSON.stringify(s));
      assert(!m.errors.length, m.errors.join('; '));
      await m.ctx.close();
    });

    await step('M2: 1920x1080: scale 1.5, big type, the credit inside the safe inset at ≥ 14px, no nav-drawer shadow', async () => {
      const m = await openM2({ viewport: { width: 1920, height: 1080 } });
      await enterKiosk(m, KIOSK_M2);
      const p = m.page;
      await p.waitForFunction(() => document.querySelectorAll('.live-kiosk-tile-value')[1].textContent === '318', null, { timeout: 10000 });
      const sc = await p.evaluate(() => getComputedStyle(document.body).getPropertyValue('--kiosk-scale').trim());
      assert(sc === '1.5', '--kiosk-scale: ' + sc);
      assert(await fontPx(p, '.live-kiosk-tile-value') === 84, 'numbers 56px × 1.5: ' + (await fontPx(p, '.live-kiosk-tile-value')));
      assert(await fontPx(p, '.live-kiosk-tile-label') === 27, 'labels 18px × 1.5');
      assert(await fontPx(p, '.live-kiosk-step-label') === 42, 'step label 28px × 1.5');
      const c = await credit(p);
      assert(c && c.text && inSafeCorner(c), 'credit in the safe corner: ' + JSON.stringify(c));
      assert(c.fs >= 14, 'credit font ≥ 14px at 1080p: ' + c.fs);
      const sh = await p.evaluate(() => { const d = document.querySelector('.nav-drawer'); return d ? getComputedStyle(d).boxShadow : 'none'; });
      assert(sh === 'none', 'closed nav drawer shadow in kiosk: ' + sh);
      m.send({ type: 'moment', data: newNode('big-1', 1, await m.now()) });
      await p.waitForSelector('.live-moment-card');
      assert(await fontPx(p, '.live-moment-title') === 27, 'Moments title 18px × 1.5: ' + (await fontPx(p, '.live-moment-title')));
      assert(!m.errors.length, m.errors.join('; '));
      await m.ctx.close();
    });

    await step('M2: outside kiosk the Moments sizes are the old px (the token defaults)', async () => {
      const m = await openM2({ viewport: { width: 1280, height: 720 } });
      const p = m.page;
      await p.goto(BASE + '/?r=' + Date.now() + '#/live?moments=1', { waitUntil: 'domcontentloaded' });
      await m.liveReady();
      await p.waitForFunction(() => window._liveMoments && window._liveMoments(), null, { timeout: 10000 });
      m.send({ type: 'moment', data: newNode('plain-1', 1, await m.now()) });
      await p.waitForSelector('.live-moment-card');
      const st = await p.evaluate(() => {
        const cs = (s) => getComputedStyle(document.querySelector(s));
        return { kiosk: document.body.classList.contains('kiosk'), title: cs('.live-moment-title').fontSize, detail: cs('.live-moment-detail').fontSize,
          time: cs('.live-moment-time').fontSize, pad: cs('.live-moment-main').padding,
          icon: document.querySelector('.live-moment-main .ph-icon').getBoundingClientRect().width,
          width: document.querySelector('.live-moments').getBoundingClientRect().width };
      });
      assert(!st.kiosk, 'plain Live');
      assert(JSON.stringify(st) === JSON.stringify({ kiosk: false, title: '12px', detail: '12px', time: '12px', pad: '6px 8px', icon: 16, width: 320 }),
        'old px: ' + JSON.stringify(st));
      assert(!m.errors.length, m.errors.join('; '));
      await m.ctx.close();
    });

    await step('M2: 375x560: the tiles are one row at the top under the step label, inside the viewport', async () => {
      const m = await openM2({ viewport: { width: 375, height: 560 }, touch: true });
      const p = m.page;
      await p.goto(BASE + '/?r=' + Date.now() + KIOSK_M2, { waitUntil: 'domcontentloaded' });
      await m.liveReady();
      await m.kioskReady();
      await p.waitForFunction(() => document.querySelectorAll('.live-kiosk-tile-value')[1].textContent === '318', null, { timeout: 10000 });
      const g = await p.evaluate(() => {
        const r = (s) => { const b = document.querySelector(s).getBoundingClientRect(); return { l: b.left, t: b.top, r: b.right, b: b.bottom }; };
        const tiles = [...document.querySelectorAll('.live-kiosk-tile:not([hidden])')].map((e) => e.getBoundingClientRect().top);
        return { step: r('.live-kiosk-step'), tiles: r('.live-kiosk-tiles'), tops: tiles, sw: document.documentElement.scrollWidth, vw: innerWidth };
      });
      assert(g.sw <= g.vw, 'no sideways scroll: ' + JSON.stringify(g));
      assert(g.tiles.l >= 0 && g.tiles.r <= g.vw && g.tiles.t >= g.step.b && g.tiles.b < 200, 'one row at the top under the label: ' + JSON.stringify(g));
      assert(g.tops.length === 3 && g.tops.every((t) => Math.abs(t - g.tops[0]) < 1), 'one row: ' + JSON.stringify(g.tops));
      const c = await credit(p);
      assert(c && c.text && inSafeCorner(c), 'credit: ' + JSON.stringify(c));
      // The phone's one Moments card stays above the credit, which kiosk
      // lifted into the safe inset.
      m.send({ type: 'moment', data: newNode('phone-1', 1, await m.now()) });
      await p.waitForSelector('.live-moment-card');
      await p.waitForTimeout(300);
      const card = await p.evaluate(() => document.querySelector('.live-moment-card').getBoundingClientRect().bottom);
      assert(card <= c.t, 'the card ends above the credit: card bottom ' + card + ', credit top ' + c.t);
      assert(!m.errors.length, m.errors.join('; '));
      await m.ctx.close();
    });
  } finally {
    await browser.close();
  }
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
