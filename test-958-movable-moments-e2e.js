/**
 * E2E: #958 — the Live Moments window moves like the Feed panel.
 *
 * The moments block of /api/config/client is added by interception,
 * GET /api/moments is stubbed (two moments, so the bar shows from the
 * start) and the app's WebSocket is replaced by page.routeWebSocket, as in
 * test-881-live-moments-e2e.js.
 *
 *   1. 1440x900 from a cold #/live: #liveMoments is a drag panel at its
 *      default anchor (bc, bottom centre) with a visible corner button.
 *   2. A real mouse drag by the bar's count moves it; the position is saved
 *      and survives a reload; a window resize keeps it inside the viewport.
 *   3. Bar buttons: a press on Pause that then moves 60px is no drag, and
 *      clicks on Pause and on the drawer button still work.
 *   4. The corner button cycles bc → tl → tr → br → bl → bc; each stop is
 *      on its anchor and a right-hand corner covers no Leaflet control; a
 *      click after a drag clears the drag.
 *   5. Escape mid-drag reverts; reset (resetPanelPositions) puts it back at
 *      bc and forgets the drag and the corner.
 *   6. The drag gate off and on again (1440 → 700 → 1440): the window at
 *      tr leaves its corner while narrow and gets it back when wide.
 *   7. Lifecycle: Live → Packets → Live three times, and Moments off/on
 *      three times, leave the live DragManager with the three panels plus
 *      this one, all in the document.
 *   8. Kiosk at 1440x900: a saved drag and corner do not move the kiosk
 *      stack; it has no drag hooks and is not registered.
 *   9. Touch tablet 1024x768 (gate off): the bar shows, its corner button
 *      is in the DOM but not visible.
 *  10. Order: with live.js served without initMoments' MeshConfigReady
 *      await, Moments start before init() makes the DragManager; on a first
 *      mount and on a remount the window still joins that mount's manager
 *      (not none, not the last mount's) and drags.
 *  11. Phone 375x560 with touch, cold from Home and the bottom-nav Live tab:
 *      a saved drag and corner change nothing, the corner button is not
 *      visible, a touch drag on the bar does not move it and the drawer
 *      opens.
 *
 * Every rect a press or an assertion depends on is read after
 * tests/e2e-live-settle.js says the layout is final: right after a mount
 * the window drops 52px when live.js sets the page height (review PR#961).
 *
 * Usage: BASE_URL=http://localhost:13581 node test-958-movable-moments-e2e.js
 */
'use strict';
const { chromium } = require('playwright');
const { settleLive } = require('./tests/e2e-live-settle');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
if (!['localhost', '127.0.0.1', '[::1]'].includes(new URL(BASE).hostname)) throw new Error('local server only');

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + (e && e.stack || e)); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

const PK = (c) => c.repeat(64);
const VIEW = 'lat=55.3&lon=9.6&zoom=9';
const at = (agoS) => new Date(Date.now() - agoS * 1000).toISOString();
const REST = [
  { id: 'rest-2', kind: 'back_online', at: at(600), silentHours: 37, node: { publicKey: PK('b'), name: 'E2E-958 back', role: 'repeater', lat: 55.1, lon: 9.1 } },
  { id: 'rest-1', kind: 'new_node', at: at(1200), node: { publicKey: PK('a'), name: 'E2E-958 new', role: 'repeater', lat: 55.2, lon: 9.2 } },
];
const SAVED = { 'panel-drag-liveMoments': JSON.stringify({ xPct: 0.1, yPct: 0.4 }), 'panel-corner-liveMoments': 'tl' };

// open: a context with the moments block, the stubbed history and a mocked
// socket. opts.seed puts localStorage keys in place before the app runs.
async function open(browser, opts) {
  opts = opts || {};
  const ctx = await browser.newContext(Object.assign({ viewport: opts.viewport || { width: 1440, height: 900 } },
    opts.touch ? { hasTouch: true, isMobile: true } : {}));
  if (opts.seed) {
    await ctx.addInitScript((seed) => {
      if (sessionStorage.getItem('e2e-958-seeded')) return;
      sessionStorage.setItem('e2e-958-seeded', '1');
      for (const k in seed) localStorage.setItem(k, seed[k]);
    }, opts.seed);
  }
  await ctx.route(/\/api\/config\/client(\?|$)/, async (route) => {
    const res = await route.fetch();
    const body = await res.json();
    body.moments = { enabled: true, historySize: 50 };
    await route.fulfill({ response: res, json: body });
  });
  await ctx.route(/\/api\/moments(\?|$)/, (route) => route.fulfill({ json: { moments: REST } }));
  await ctx.routeWebSocket(/.*/, () => {});
  const page = await ctx.newPage();
  const h = { ctx, page, errors: [] };
  page.on('pageerror', (e) => h.errors.push(String(e.stack || e.message).split('\n').slice(0, 4).join(' | ')));
  h.ready = async () => {
    await page.waitForFunction(() => window._liveGetMap && window._liveGetMap() && window._liveMoments && window._liveMoments(), null, { timeout: 20000 });
    await page.waitForSelector('.live-moments-bar', { state: 'visible', timeout: 10000 });
    await h.settle();
  };
  // the rect read next is the one a press lands on (tests/e2e-live-settle.js)
  h.settle = () => settleLive(page, '.live-moments');
  h.goto = async (hash) => {
    await page.goto(BASE + '/?r=' + Date.now() + '#/live?' + (hash || VIEW), { waitUntil: 'domcontentloaded' });
    await h.ready();
  };
  return h;
}

// state: the Moments window's position and drag state.
function state(page) {
  return page.evaluate(() => {
    const w = document.querySelector('.live-moments');
    const r = w.getBoundingClientRect();
    const btn = w.querySelector('.panel-corner-btn');
    return {
      id: w.id, pos: w.getAttribute('data-position'), dragged: w.getAttribute('data-dragged'),
      left: r.left, top: r.top, right: r.right, bottom: r.bottom, width: r.width,
      vw: innerWidth, vh: innerHeight,
      header: !!w.querySelector('.live-moments-bar.panel-header'),
      cornerBtn: !!btn,
      cornerShown: !!(btn && btn.getClientRects().length && getComputedStyle(btn).visibility !== 'hidden'),
      drag: localStorage.getItem('panel-drag-liveMoments'),
      corner: localStorage.getItem('panel-corner-liveMoments'),
      inline: w.getAttribute('style') || '',
    };
  });
}

// mouseDrag: a real mouse press on (x, y), moved by (dx, dy) in steps.
async function mouseDrag(page, x, y, dx, dy, beforeUp) {
  await page.mouse.move(x, y);
  await page.mouse.down();
  await page.mouse.move(x + dx, y + dy, { steps: 12 });
  if (beforeUp) await beforeUp();
  await page.mouse.up();
}
async function centre(page, sel) {
  const r = await page.locator(sel).boundingBox();
  assert(r, sel + ' has a box');
  return { x: r.x + r.width / 2, y: r.y + r.height / 2 };
}

// anchorOf: where live.css puts each position; null keys are not checked.
async function anchors(page) {
  return page.evaluate(() => {
    const w = document.querySelector('.live-moments');
    const cs = getComputedStyle(w);
    const vcr = parseFloat(cs.getPropertyValue('--vcr-bar-height')) || 58;
    const credit = parseFloat(cs.getPropertyValue('--live-credit-height')) || 0;
    const page = document.querySelector('.live-page').getBoundingClientRect();
    return { bottomGap: innerHeight - page.bottom + vcr + credit + 10, pageTop: page.top, pageCx: page.left + page.width / 2 };
  });
}

// leafletCovered: Leaflet controls whose centre the Moments window takes.
function leafletCovered(page) {
  return page.evaluate(() => {
    const bad = [];
    for (const el of document.querySelectorAll('.leaflet-control a, .leaflet-control button')) {
      const r = el.getBoundingClientRect();
      if (!r.width || !r.height) continue;
      const hit = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2);
      if (hit && hit.closest('.live-moments')) bad.push(el.className || el.title || el.tagName);
    }
    return bad;
  });
}

function near(a, b, tol) { return Math.abs(a - b) <= (tol || 2); }

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log('\n=== #958 movable Moments window E2E against ' + BASE + ' ===');
  try {
    const d = await open(browser);
    await step('1440x900 cold #/live: a drag panel at its default anchor', async () => {
      await d.goto();
      const s = await state(d.page);
      const a = await anchors(d.page);
      assert(s.id === 'liveMoments', 'id: ' + s.id);
      assert(s.header, 'the bar is the .panel-header');
      assert(s.pos === 'bc' && !s.dragged, JSON.stringify(s));
      assert(s.cornerShown, 'corner button visible');
      assert(near((s.left + s.right) / 2, a.pageCx, 3), 'centred: ' + JSON.stringify(s));
      assert(near(s.vh - s.bottom, a.bottomGap, 3), 'above the VCR bar: ' + JSON.stringify([s, a]));
    });

    let dragged = null;
    await step('a real mouse drag by the bar moves it, saved; it survives a reload', async () => {
      const s0 = await state(d.page);
      const c = await centre(d.page, '.live-moments-bar .live-moments-count');
      await mouseDrag(d.page, c.x, c.y, -400, -300);
      const s = await state(d.page);
      assert(s.dragged === 'true' && !s.pos, 'dragged: ' + JSON.stringify(s));
      assert(near(s.left, s0.left - 400, 3) && near(s.top, s0.top - 300, 3), 'moved by the drag: ' + JSON.stringify([s0, s]));
      assert(s.drag, 'saved as panel-drag-liveMoments');
      dragged = s;
      await d.page.reload({ waitUntil: 'domcontentloaded' });
      await d.ready();
      const r = await state(d.page);
      assert(r.dragged === 'true' && near(r.left, dragged.left, 2) && near(r.top, dragged.top, 2), 'restored: ' + JSON.stringify([dragged, r]));
    });

    await step('a window resize keeps a dragged window inside the viewport', async () => {
      const c = await centre(d.page, '.live-moments-bar .live-moments-count');
      await mouseDrag(d.page, c.x, c.y, 1400, 0); // past the right edge: snaps to it
      let s = await state(d.page);
      assert(s.right <= s.vw && s.vw - s.right <= 13, 'snapped to the right edge: ' + JSON.stringify(s));
      await d.page.setViewportSize({ width: 1100, height: 900 });
      await d.page.waitForFunction(() => document.querySelector('.live-moments').getBoundingClientRect().right <= innerWidth, null, { timeout: 3000 });
      s = await state(d.page);
      assert(s.right <= 1100 && s.left >= 0, 'clamped: ' + JSON.stringify(s));
      await d.page.setViewportSize({ width: 1440, height: 900 });
      await d.page.evaluate(() => window._panelCorner.resetPanelPositions());
      await d.settle();
    });

    await step('bar buttons: a press on Pause that moves is no drag; Pause and the drawer still click', async () => {
      const s0 = await state(d.page);
      assert(s0.pos === 'bc' && !s0.dragged, 'reset first: ' + JSON.stringify(s0));
      const p = await centre(d.page, '.live-moments-bar .live-moments-pause');
      await mouseDrag(d.page, p.x, p.y, -60, -60);
      let s = await state(d.page);
      assert(!s.dragged && !s.drag && s.pos === 'bc' && near(s.left, s0.left, 1) && near(s.top, s0.top, 3), 'not moved: ' + JSON.stringify([s0, s]));
      await d.page.click('.live-moments-bar .live-moments-pause');
      assert(await d.page.getAttribute('.live-moments-bar .live-moments-pause', 'aria-pressed') === 'true', 'paused');
      await d.page.click('.live-moments-bar .live-moments-pause');
      assert(await d.page.getAttribute('.live-moments-bar .live-moments-pause', 'aria-pressed') === 'false', 'resumed');
      await d.page.click('.live-moments-bar .live-moments-open');
      await d.page.waitForSelector('.live-moments-drawer', { state: 'visible' });
      await d.page.keyboard.press('Escape');
      await d.page.waitForSelector('.live-moments-drawer', { state: 'hidden' });
      s = await state(d.page);
      assert(!s.dragged && !s.drag && near(s.left, s0.left, 1), 'clicks are no drag: ' + JSON.stringify(s));
    });

    await step('the corner button cycles bc → tl → tr → br → bl → bc, each on its anchor', async () => {
      const a = await anchors(d.page);
      const seen = [];
      for (let i = 0; i < 5; i++) {
        await d.page.click('#liveMoments .panel-corner-btn');
        const s = await state(d.page);
        seen.push(s.pos);
        assert(s.corner === s.pos, 'saved: ' + JSON.stringify(s));
        if (s.pos === 'tl' || s.pos === 'bl') assert(near(s.left, 12, 2), s.pos + ' left: ' + JSON.stringify(s));
        if (s.pos === 'tl' || s.pos === 'tr') assert(near(s.top, a.pageTop + 64, 2), s.pos + ' top: ' + JSON.stringify([s, a]));
        if (s.pos === 'bl' || s.pos === 'br') assert(near(s.vh - s.bottom, a.bottomGap, 3), s.pos + ' bottom: ' + JSON.stringify([s, a]));
        if (s.pos === 'tr' || s.pos === 'br') {
          assert(s.vw - s.right >= 12, s.pos + ' right: ' + JSON.stringify(s));
          const bad = await leafletCovered(d.page);
          assert(!bad.length, s.pos + ' covers Leaflet controls: ' + bad.join(', '));
        }
        if (s.pos === 'bc') assert(near((s.left + s.right) / 2, a.pageCx, 3), 'bc centred: ' + JSON.stringify(s));
        // the button stays on top of a panel or the header in that corner
        const top = await d.page.evaluate(() => {
          const b = document.querySelector('#liveMoments .panel-corner-btn').getBoundingClientRect();
          const hit = document.elementFromPoint(b.left + b.width / 2, b.top + b.height / 2);
          return hit && hit.closest('.panel-corner-btn') ? 'self' : (hit && (hit.id || hit.className));
        });
        assert(top === 'self', s.pos + ': corner button covered by ' + top);
      }
      assert(seen.join(',') === 'tl,tr,br,bl,bc', 'cycle: ' + seen.join(','));
      // after a drag, the button clears it and goes on from the saved corner
      const c = await centre(d.page, '.live-moments-bar .live-moments-count');
      await mouseDrag(d.page, c.x, c.y, -300, -200);
      assert((await state(d.page)).dragged === 'true', 'dragged');
      await d.page.click('#liveMoments .panel-corner-btn');
      const s = await state(d.page);
      assert(s.pos === 'tl' && !s.dragged && !s.drag && !/top|left/.test(s.inline) && near(s.left, 12, 2), 'drag cleared: ' + JSON.stringify(s));
    });

    await step('Escape mid-drag reverts; reset puts it back at bc and forgets drag and corner', async () => {
      const s0 = await state(d.page);
      const c = await centre(d.page, '.live-moments-bar .live-moments-count');
      await mouseDrag(d.page, c.x, c.y, 300, 200, () => d.page.keyboard.press('Escape'));
      let s = await state(d.page);
      assert(!s.drag && s.pos === 'tl' && near(s.left, s0.left, 1) && near(s.top, s0.top, 3), 'reverted: ' + JSON.stringify([s0, s]));
      await mouseDrag(d.page, c.x, c.y, 300, 200);
      assert((await state(d.page)).drag, 'dragged again');
      await d.page.evaluate(() => window._panelCorner.resetPanelPositions());
      s = await state(d.page);
      const a = await anchors(d.page);
      assert(s.pos === 'bc' && !s.dragged && !s.drag && !s.corner && s.inline === '', 'reset: ' + JSON.stringify(s));
      assert(near((s.left + s.right) / 2, a.pageCx, 3) && near(s.vh - s.bottom, a.bottomGap, 3), 'back at the anchor: ' + JSON.stringify(s));
    });

    await step('drag gate off and on again (narrow, then wide): the window gets its corner back', async () => {
      await d.page.click('#liveMoments .panel-corner-btn'); // bc → tl
      await d.page.click('#liveMoments .panel-corner-btn'); // tl → tr
      const s0 = await state(d.page);
      assert(s0.pos === 'tr' && s0.corner === 'tr', 'at tr: ' + JSON.stringify(s0));
      // onDragMediaChange disables / enables the DragManager in the same
      // call that takes / gives back the corner
      await d.page.setViewportSize({ width: 700, height: 900 });
      await d.page.waitForFunction(() => window._liveDragMgr.enabled === false, null, { timeout: 5000 });
      await d.settle();
      let s = await state(d.page);
      assert(!s.pos && s.corner === 'tr', 'gate off: its own anchor, the corner kept: ' + JSON.stringify(s));
      await d.page.setViewportSize({ width: 1440, height: 900 });
      await d.page.waitForFunction(() => window._liveDragMgr.enabled === true, null, { timeout: 5000 });
      await d.settle();
      s = await state(d.page);
      const a = await anchors(d.page);
      assert(s.pos === 'tr' && s.corner === 'tr' && !s.dragged, 'tr again: ' + JSON.stringify(s));
      assert(near(s.top, a.pageTop + 64, 2) && near(s.left, s0.left, 2) && near(s.right, s0.right, 2),
        'on the tr anchor as before: ' + JSON.stringify([s0, s, a]));
      await d.page.evaluate(() => window._panelCorner.resetPanelPositions());
    });

    await step('lifecycle: three remounts and three Moments off/on leave one registration', async () => {
      for (let i = 0; i < 3; i++) {
        await d.page.evaluate(() => { location.hash = '#/packets'; });
        await d.page.waitForFunction(() => !document.querySelector('.live-page'));
        await d.page.evaluate((v) => { location.hash = '#/live?' + v; }, VIEW);
        await d.ready();
      }
      for (let i = 0; i < 3; i++) {
        await d.page.evaluate(() => { const t = document.getElementById('liveMomentsToggle'); t.checked = false; t.dispatchEvent(new Event('change')); });
        await d.page.waitForFunction(() => !document.querySelector('.live-moments'));
        await d.page.evaluate(() => { const t = document.getElementById('liveMomentsToggle'); t.checked = true; t.dispatchEvent(new Event('change')); });
        await d.page.waitForSelector('.live-moments-bar', { state: 'visible' });
      }
      await d.settle();
      const r = await d.page.evaluate(() => {
        const m = window._liveDragMgr;
        return { n: m._panels.length, ids: m._panels.map((p) => p.id).sort().join(','),
          connected: m._panels.every((p) => p.isConnected), roots: document.querySelectorAll('.live-moments').length };
      });
      assert(r.n === 4 && r.ids === 'liveFeed,liveLegend,liveMoments,liveNodeDetail' && r.connected && r.roots === 1, JSON.stringify(r));
      // the re-made window still drags
      const c = await centre(d.page, '.live-moments-bar .live-moments-count');
      await mouseDrag(d.page, c.x, c.y, -200, -100);
      assert((await state(d.page)).dragged === 'true', 'drags after toggling');
      await d.page.evaluate(() => window._panelCorner.resetPanelPositions());
      assert(!d.errors.length, d.errors.join('; '));
    });
    await d.ctx.close();

    await step('kiosk 1440x900: a saved drag and corner do not move the stack; no drag hooks', async () => {
      const KIOSK = 'kiosk=1&' + VIEW;
      const plain = await open(browser);
      await plain.goto(KIOSK);
      const base = await state(plain.page);
      await plain.ctx.close();
      const k = await open(browser, { seed: SAVED });
      await k.goto(KIOSK);
      const s = await state(k.page);
      assert(!s.id && !s.pos && !s.dragged && !s.header && s.inline === '', 'no drag hooks: ' + JSON.stringify(s));
      assert(s.drag && s.corner, 'the saved keys are there: ' + JSON.stringify(s));
      assert(near(s.left, base.left, 1) && near(s.top, base.top, 3) && near(s.right, base.right, 1), 'not moved: ' + JSON.stringify([base, s]));
      const n = await k.page.evaluate(() => window._liveDragMgr._panels.map((p) => p.id).join(','));
      assert(!/liveMoments|^,|,,/.test(n) && n.split(',').length === 3, 'not registered: ' + n);
      assert(!k.errors.length, k.errors.join('; '));
      await k.ctx.close();
    });

    await step('touch tablet 1024x768 (drag gate off): the bar shows, its corner button does not', async () => {
      const t = await open(browser, { viewport: { width: 1024, height: 768 }, touch: true });
      await t.goto();
      const env = await t.page.evaluate(() => ({ vw: innerWidth, gate: matchMedia('(pointer: fine) and (min-width: 768px)').matches }));
      assert(env.vw === 1024 && !env.gate, 'a 1024px window with the drag gate off: ' + JSON.stringify(env));
      const s = await state(t.page);
      assert(s.id === 'liveMoments' && s.cornerBtn, 'a panel with its corner button in the DOM: ' + JSON.stringify(s));
      assert(!s.cornerShown, 'corner button not visible: ' + JSON.stringify(s));
      assert(!s.pos && !s.dragged, 'on its own anchor: ' + JSON.stringify(s));
      assert(await t.page.isVisible('.live-moments-bar .live-moments-pause'), 'the bar\'s Pause button shows (not the phone bar)');
      assert(!t.errors.length, t.errors.join('; '));
      await t.ctx.close();
    });

    await step('Moments started before the DragManager exists still join this mount\'s manager', async () => {
      // live.js served without initMoments' MeshConfigReady await, so
      // startMoments() runs inside init(), before its DragManager block. It
      // records the manager it saw (none, or the last mount's).
      const AWAIT = 'try { if (window.MeshConfigReady) await window.MeshConfigReady; } catch (_) {}';
      const START = '  function startMoments() {\n';
      const o = await open(browser);
      let patched = 0;
      await o.ctx.route(/\/live\.js(\?|$)/, async (route) => {
        const res = await route.fetch();
        const src = await res.text();
        if (src.includes(AWAIT) && src.includes(START)) patched++;
        await route.fulfill({ response: res, body: src.replace(AWAIT, '')
          .replace(START, START + '    (window.__e2eSeenMgr = window.__e2eSeenMgr || []).push(window._liveDragMgr || null);\n') });
      });
      await o.page.goto(BASE + '/?r=' + Date.now() + '#/packets', { waitUntil: 'domcontentloaded' });
      await o.page.waitForFunction(() => window.MC_MOMENTS_CFG, null, { timeout: 20000 });
      assert(patched === 1, 'live.js patched: ' + patched);
      for (let mount = 1; mount <= 2; mount++) {
        await o.page.evaluate((v) => { location.hash = '#/live?' + v; }, VIEW);
        await o.ready();
        const r = await o.page.evaluate(() => {
          const m = window._liveDragMgr, seen = window.__e2eSeenMgr;
          return { starts: seen.length, before: seen[seen.length - 1] !== m,
            ids: m._panels.map((p) => p.id).sort().join(','), connected: m._panels.every((p) => p.isConnected),
            prev: seen.length > 1 && seen[seen.length - 1] ? seen[seen.length - 1]._panels.length : 0 };
        });
        assert(r.starts === mount && r.before, 'mount ' + mount + ': Moments started before this mount\'s manager: ' + JSON.stringify(r));
        assert(r.ids === 'liveFeed,liveLegend,liveMoments,liveNodeDetail' && r.connected && r.prev <= 3, 'mount ' + mount + ': ' + JSON.stringify(r));
        const s0 = await state(o.page);
        const c = await centre(o.page, '.live-moments-bar .live-moments-count');
        await mouseDrag(o.page, c.x, c.y, -200, -100);
        const s = await state(o.page);
        assert(s.dragged === 'true' && near(s.left, s0.left - 200, 3) && near(s.top, s0.top - 100, 3), 'mount ' + mount + ' drags: ' + JSON.stringify([s0, s]));
        await o.page.evaluate(() => window._panelCorner.resetPanelPositions());
        await o.page.evaluate(() => { location.hash = '#/packets'; });
        await o.page.waitForFunction(() => !document.querySelector('.live-page'));
      }
      assert(!o.errors.length, o.errors.join('; '));
      await o.ctx.close();
    });

    await step('phone 375x560 touch, cold from Home: saved keys change nothing; no corner button; a touch drag does not move it', async () => {
      const coldLive = async (h) => {
        await h.page.goto(BASE + '/?r=' + Date.now() + '#/', { waitUntil: 'domcontentloaded' });
        await h.page.waitForSelector('[data-bottom-nav-tab="live"]', { state: 'visible', timeout: 20000 });
        await h.page.tap('[data-bottom-nav-tab="live"]');
        await h.ready(); // settled: the VCR bar, the fonts and the page height
      };
      const vp = { width: 375, height: 560 };
      const plain = await open(browser, { viewport: vp, touch: true });
      await coldLive(plain);
      const base = await state(plain.page);
      await plain.ctx.close();
      const p = await open(browser, { viewport: vp, touch: true, seed: SAVED });
      await coldLive(p);
      let s = await state(p.page);
      assert(s.cornerBtn && !s.cornerShown, 'corner button in the DOM but not visible: ' + JSON.stringify(s));
      assert(!s.pos && !s.dragged && s.inline === '', 'no corner, no position: ' + JSON.stringify(s));
      assert(s.drag && s.corner, 'the saved keys are there: ' + JSON.stringify(s));
      assert(near(s.left, base.left, 1) && near(s.top, base.top, 3) && near(s.right, base.right, 1) && near(s.left, 8, 1),
        'same place as without them: ' + JSON.stringify([base, s]));
      assert(near(s.right, s.vw - 68, 1), 'right edge at the control rail, as #881 placed it: ' + JSON.stringify(s));
      // a touch drag that starts on the bar
      const b = await p.page.locator('.live-moments-bar').boundingBox();
      const cdp = await p.ctx.newCDPSession(p.page);
      const pt = (x, y) => [{ x, y, id: 1, radiusX: 4, radiusY: 4, force: 1 }];
      await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: pt(b.x + 1, b.y + b.height / 2) });
      for (let i = 1; i <= 8; i++) await cdp.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: pt(b.x + 1 - i * 20, b.y + b.height / 2 - i * 25) });
      await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
      s = await state(p.page);
      assert(!s.dragged && near(s.left, base.left, 1) && near(s.top, base.top, 3), 'not moved by touch: ' + JSON.stringify(s));
      await p.page.tap('.live-moments-bar .live-moments-open');
      await p.page.waitForSelector('.live-moments-drawer', { state: 'visible' });
      await p.page.tap('.live-moments-drawer .live-moments-close');
      await p.page.waitForSelector('.live-moments-drawer', { state: 'hidden' });
      assert(!p.errors.length, p.errors.join('; '));
      await p.ctx.close();
    });
  } finally {
    await browser.close();
  }
  console.log('\n' + passed + ' passed, ' + failed + ' failed');
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
