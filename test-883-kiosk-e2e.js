/**
 * E2E: Live kiosk mode (#883 M1 — shell, tour, start and exit).
 *
 * /api/config/areas/polygons is stubbed with three areas (the fixture
 * config has none), the shared WebSocket is replaced by page.routeWebSocket
 * and the page runs on Playwright's clock with reduced motion, so each tour
 * step is an instant fitBounds at a known time.
 *
 *   1. #/live?kiosk=1&tour=aaa,BBB,nope,ccc&every=45: body.kiosk; the top
 *      nav, bottom nav, header, stats, controls, feed, legend, VCR bar and
 *      the map's zoom/layer/toggle controls are hidden; the map credit is
 *      visible, non-empty and inside the viewport at the bottom edge.
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
 *  10. The router clears a stray body.kiosk on a non-live route (app.js).
 *  11. ?follow= and ?node= are ignored in kiosk; ?moments=1 applies without
 *      writing live-moments.
 *      Without a tour the view is the map defaults, not the saved view.
 *  12. The kiosk page fills the viewport before Live's resize timer runs.
 *  13. 375x560 touch smoke: no horizontal scroll, credit visible, label up.
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
  const h = { ctx, api: [], areaRequests: 0, errors: [], warnings: [] };
  await ctx.route(/\/api\/config\/areas\/polygons(\?|$)/, (route) => { h.areaRequests++; return route.fulfill({ json: AREAS }); });
  await ctx.routeWebSocket(/.*/, () => {});
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
function credit(page) {
  return page.evaluate(() => {
    const a = document.querySelector('#liveMap .leaflet-control-attribution');
    if (!a) return null;
    const r = a.getBoundingClientRect();
    return { text: a.textContent.trim(), l: r.left, t: r.top, r: r.right, b: r.bottom, w: r.width, h: r.height,
      vw: innerWidth, vh: innerHeight, vis: getComputedStyle(a).visibility, fs: parseFloat(getComputedStyle(a).fontSize) };
  });
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
      assert(c.b >= c.vh - 2, 'credit at the bottom edge, not 58px up: ' + JSON.stringify(c));
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
      await m.ctx.route(/\/api\/config\/client(\?|$)/, async (route) => {
        const res = await route.fetch();
        const body = await res.json();
        body.moments = { enabled: true, historySize: 50 };
        await route.fulfill({ response: res, json: body });
      });
      await m.ctx.route(/\/api\/moments(\?|$)/, (route) => route.fulfill({ json: { moments: [] } }));
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
      await m.page.clock.install();
      await m.page.clock.pauseAt(Date.now() + 1000);
      await m.page.goto(BASE + '/?r=' + Date.now() + '#/live?kiosk=1', { waitUntil: 'domcontentloaded' });
      await m.page.waitForSelector('.live-page');
      const r = await m.page.evaluate(() => { const b = document.querySelector('.live-page').getBoundingClientRect();
        return { top: b.top, bottom: b.bottom, vh: innerHeight, inline: document.querySelector('.live-page').style.height }; });
      assert(r.inline === '' && r.top === 0 && Math.abs(r.bottom - r.vh) < 1, JSON.stringify(r));
      await m.ctx.close();
    });

    await step('375x560 touch smoke: no sideways scroll, credit and label visible', async () => {
      const m = await open(browser, { viewport: { width: 375, height: 560 }, touch: true });
      await m.page.goto(BASE + '/?r=' + Date.now() + KIOSK, { waitUntil: 'domcontentloaded' });
      await m.liveReady();
      await m.kioskReady();
      await m.page.waitForTimeout(300);
      const s = await m.page.evaluate(() => ({ sw: document.documentElement.scrollWidth, vw: innerWidth,
        label: (() => { const r = document.querySelector('.live-kiosk-step').getBoundingClientRect(); return [r.left, r.top, r.right, r.bottom]; })() }));
      assert(s.sw <= s.vw, 'horizontal scroll: ' + JSON.stringify(s));
      assert(s.label[0] >= 0 && s.label[2] <= s.vw, 'label inside: ' + JSON.stringify(s));
      const c = await credit(m.page);
      assert(c && c.text && c.w > 0 && c.l >= 0 && c.r <= c.vw + 0.5 && c.b <= c.vh + 0.5, 'credit: ' + JSON.stringify(c));
      const st = await chromeState(m.page, ['.bottom-nav', '.top-nav', '.vcr-bar']);
      assert(!st.some((x) => x.vis), 'chrome: ' + JSON.stringify(st));
      assert(!m.errors.length, m.errors.join('; '));
      await m.ctx.close();
    });
  } finally {
    await browser.close();
  }
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
