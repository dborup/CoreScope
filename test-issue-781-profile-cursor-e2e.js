/**
 * E2E (#781): hovering the Path profile chart shows the same point on the
 * Map page's profile line.
 *
 * Like the other DHM suites, the CI server has no map.dhm config and no
 * token, so /api/config/client is intercepted and given the `dhm` block
 * the Go server sends when the profile is served, and /api/terrain/profile
 * is answered with a synthetic profile. No real DHM service and no
 * third-party tile server is ever contacted.
 *
 * Asserts:
 *   1. hovering the chart shows the dashed line at the sample under the
 *      pointer, the readout for that sample, and one red dot on the map at
 *      that fraction of the drawn line from A to B (within 1.5 px);
 *   2. moving along the chart moves the same dot and line, no new layers;
 *   3. leaving the chart hides the line, the readout and the dot;
 *   4. ←/→ step one sample and Shift+→ ten, with the readout polite;
 *   5. a touch drag at 360x740 moves the dot, which stays after the lift;
 *   6. perf: 500 synthetic moves in one task cause one update and no chart
 *      rebuild; 500 spread over frames never more than one per frame;
 *   7. Close removes the dot and the line, and after several Map visits
 *      the cursor's listeners are back to zero.
 *
 * SHOTS_DIR=dir also writes the light and dark screenshots of the PR.
 *
 * Usage:
 *   BASE_URL=http://localhost:13581 node test-issue-781-profile-cursor-e2e.js
 */
'use strict';
const path = require('path');
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const SHOTS = process.env.SHOTS_DIR || '';
const PROFILE_PATH = '/api/terrain/profile';
const ATTRIBUTION = 'Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel, CC BY 4.0';
const DHM_BLOCK = {
  enabled: true, url: '/api/map/dhm/wms', layer: 'dhm_terraen_skyggekort',
  minZoom: 7, maxZoom: 19, profile: true,
  bounds: [[54.4265, 7.99125], [57.778, 15.5995]],
  attribution: ATTRIBUTION,
};
const PNG_1X1 = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGM4ceIEAAS0AlkWLoFAAAAAAElFTkSuQmCC',
  'base64');
const PANEL = '[data-testid="tp-panel"]';
const PLOT = '[data-testid="tp-chart"] .tp-plot';
const LINE = '[data-testid="tp-cursor-line"]';
const READOUT = '[data-testid="tp-cursor-readout"]';
const DOT = '.leaflet-overlay-pane .tp-cursor-dot';

// A diagonal free-point path near Aarhus, long enough that the dot moves
// visibly; far from the fixture's own nodes.
const P = '56.17000,10.12000,56.12000,10.22000';
const N = 101, D = 8400;
// The chart's fixed viewBox and plot box (public/terrain-profile.js).
const VIEW_W = 820, PLOT_X = 46, PLOT_W = 758;
const sampleX = (i) => Math.round((PLOT_X + PLOT_W * i / (N - 1)) * 100) / 100;

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

/* A hill in the middle rises through the line of sight, so the chart has
 * intrusion marks and the readout a negative clearance there. */
function terrainAt(i) { return 30 + 45 * Math.exp(-Math.pow((i - 50) / 9, 2)); }
function profileFor(q) {
  const end = (side) => {
    const ll = (q.get(side) || '').split(',').map(Number);
    return { lat: ll[0], lon: ll[1], kind: 'point', groundM: 30, antennaM: 10, antennaSource: 'point-default', assumed: true };
  };
  const samples = [];
  for (let i = 0; i < N; i++) {
    const t = Math.round(terrainAt(i) * 100) / 100;
    samples.push({ d: D * i / (N - 1), dtm: t, dsm: Math.round((t + (i % 7 === 0 ? 9 : 2)) * 100) / 100 });
  }
  return {
    a: end('a'), b: end('b'),
    distanceM: D, stepM: 84, sampleCount: N, model: 'both',
    coverage: { state: 'full', pct: 100, firstGapM: null }, samples,
    physics: { freqMHz: 869.618, freqSource: 'firmware', lambdaM: 0.34474, k: 1.3333333333333333, kSource: 'default', f1MaxM: 13.4 },
    verdict: { state: 'blocked', model: 'dtm', criterion: 0.6, worstClearanceM: -36.1, worstAtM: 4200, worstF1Fraction: -2.7,
      surface: { state: 'blocked', worstClearanceM: -38.1, worstAtM: 4200, worstF1Fraction: -2.8 } },
    attribution: ATTRIBUTION,
  };
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #781 Path profile hover cursor against ${BASE} ===\n`);

  async function newCtx(o) {
    o = o || {};
    const ctx = await browser.newContext({
      viewport: o.viewport || { width: 1280, height: 860 },
      hasTouch: !!o.touch, isMobile: !!o.touch,
    });
    const rec = { profile: [], consoleErrors: [] };
    await ctx.route(BASE + '/api/config/client', async (route) => {
      const response = await route.fetch();
      const config = await response.json();
      config.dhm = Object.assign({}, DHM_BLOCK);
      await route.fulfill({ response, json: config });
    });
    await ctx.route(url => new URL(url).pathname === PROFILE_PATH, async (route) => {
      const u = new URL(route.request().url());
      rec.profile.push(u);
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(profileFor(u.searchParams)) });
    });
    await ctx.route(/^https?:\/\/(?!localhost|127\.0\.0\.1)/i, async (route) => {
      await route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 });
    });
    await ctx.route(url => new URL(url).pathname === '/api/map/dhm/wms', async (route) => {
      await route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 });
    });
    await ctx.addInitScript((theme) => {
      try {
        localStorage.setItem('meshcore-theme', theme);
        localStorage.setItem('meshcore-map-clustering', 'false');
        ['row-swipe', 'tab-swipe', 'edge-drawer', 'pull-refresh']
          .forEach(k => localStorage.setItem('meshcore-gesture-hints-' + k, 'seen'));
      } catch (_) {}
      /* Net listeners (added − removed) on the chart's plot, the element
       * the cursor wires, per (type, function), so a repeated add of the
       * same pair is not double-counted. */
      const net = window.__cursorNet = { listeners: 0 };
      const live = new WeakMap();
      const add = EventTarget.prototype.addEventListener;
      const remove = EventTarget.prototype.removeEventListener;
      const onPlot = (t) => t && t.classList && t.classList.contains('tp-plot');
      const setFor = (el, type) => {
        let m = live.get(el); if (!m) { m = new Map(); live.set(el, m); }
        let s = m.get(type); if (!s) { s = new Set(); m.set(type, s); }
        return s;
      };
      EventTarget.prototype.addEventListener = function (type, fn, opt) {
        if (onPlot(this) && fn) {
          const s = setFor(this, type);
          if (!s.has(fn)) { s.add(fn); net.listeners++; }
        }
        return add.call(this, type, fn, opt);
      };
      EventTarget.prototype.removeEventListener = function (type, fn, opt) {
        if (onPlot(this) && fn && setFor(this, type).delete(fn)) net.listeners--;
        return remove.call(this, type, fn, opt);
      };
    }, o.theme || 'light');
    const page = await ctx.newPage();
    page.on('console', m => { if (m.type() === 'error') rec.consoleErrors.push(m.text()); });
    page.on('pageerror', e => rec.consoleErrors.push(String(e)));
    return { ctx, page, rec };
  }

  async function openProfile(page) {
    await page.goto(`${BASE}/#/map?profile=${P}`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#leaflet-map[data-loaded="true"]', { timeout: 30000 });
    await page.waitForFunction((sel) => {
      const p = document.querySelector(sel);
      return !!(p && p.classList.contains('is-open'));
    }, PANEL, { timeout: 15000 });
    await page.waitForSelector(PLOT + '[tabindex="0"]', { timeout: 15000 });
    await page.waitForSelector('[data-testid="tp-end-marker-b"]', { timeout: 10000 });
    await page.waitForTimeout(300);
  }

  /* cursorState reads what the cursor shows: the line's sample index, the
   * readout, the dot and where the dot should be — at the line's fraction
   * of the way from A's marker to B's, in screen space. A Leaflet line is
   * straight on screen, so that is exactly on it. */
  async function cursorState(page) {
    return page.evaluate(([LINE, READOUT, DOT, PLOT_X, PLOT_W]) => {
      const line = document.querySelector(LINE);
      const ro = document.querySelector(READOUT);
      const dots = document.querySelectorAll(DOT);
      const c = (el) => { const r = el.getBoundingClientRect(); return { x: r.left + r.width / 2, y: r.top + r.height / 2 }; };
      const a = document.querySelector('[data-testid="tp-end-marker-a"]');
      const b = document.querySelector('[data-testid="tp-end-marker-b"]');
      const x = line ? Number(line.getAttribute('x1')) : null;
      const t = x === null ? null : (x - PLOT_X) / PLOT_W;
      const out = {
        lineVisible: !!line && line.getAttribute('visibility') === 'visible',
        lineDash: line && line.getAttribute('stroke-dasharray'),
        lineStroke: line && getComputedStyle(line).stroke,
        x: x, t: t,
        readout: ro && !ro.hidden ? Array.from(ro.children).filter(r => !r.hidden).map(r => r.textContent) : null,
        live: ro && ro.getAttribute('aria-live'),
        dots: dots.length,
        dotFill: dots.length ? getComputedStyle(dots[0]).fill : null,
      };
      if (dots.length && a && b && t !== null) {
        const pa = c(a), pb = c(b), pd = c(dots[0]);
        const want = { x: pa.x + (pb.x - pa.x) * t, y: pa.y + (pb.y - pa.y) * t };
        out.dotOff = Math.hypot(pd.x - want.x, pd.y - want.y);
      }
      return out;
    }, [LINE, READOUT, DOT, PLOT_X, PLOT_W]);
  }

  /* clientXOf is the viewport x of sample i on the stretched chart. */
  async function clientXOf(page, i) {
    const box = await page.locator(PLOT + ' svg').boundingBox();
    return { x: box.x + sampleX(i) / VIEW_W * box.width, y: box.y + box.height / 2 };
  }

  // ── 1–3. mouse ─────────────────────────────────────────────────────────
  {
    const { ctx, page, rec } = await newCtx();
    await step('hover shows the dashed line, the readout and one dot on the drawn line', async () => {
      await openProfile(page);
      let s = await cursorState(page);
      assert(!s.lineVisible && s.readout === null && s.dots === 0, 'shown before any hover: ' + JSON.stringify(s));
      const at = await clientXOf(page, 25);
      await page.mouse.move(at.x, at.y);
      await page.waitForTimeout(100);
      s = await cursorState(page);
      assert(s.lineVisible, 'line hidden');
      assert(s.lineDash, 'line is not dashed');
      assert(s.x === sampleX(25), 'line at x ' + s.x + ', sample 25 is at ' + sampleX(25));
      assert(s.dots === 1, 'dots on the map: ' + s.dots);
      assert(s.dotOff <= 1.5, 'dot is ' + s.dotOff + ' px off the drawn line at t=' + s.t);
      assert(/rgb\(220, 38, 38\)/.test(s.dotFill), 'dot fill ' + s.dotFill);
      const want = '2.10 km from A';
      assert(s.readout && s.readout[0] === want, 'readout ' + JSON.stringify(s.readout));
      assert(s.readout[1] === 'Terrain ' + (Math.round(terrainAt(25) * 100) / 100).toFixed(1) + ' m', 'terrain row ' + s.readout[1]);
      assert(/^Surface \d+\.\d m$/.test(s.readout[2]), 'surface row ' + s.readout[2]);
      assert(/^Clearance −?\d+\.\d m \(surface −?\d+\.\d m\)$/.test(s.readout[3]), 'clearance row ' + s.readout[3]);
      if (SHOTS) {
        const mid = await clientXOf(page, 47);
        await page.mouse.move(mid.x, mid.y);
        await page.waitForTimeout(150);
        await page.screenshot({ path: path.join(SHOTS, '781-hover-light.jpg'), type: 'jpeg', quality: 70 });
      }
    });

    await step('moving along the chart moves the same dot and line', async () => {
      const handle = await page.$(DOT);
      for (const i of [50, 75, 100, 0]) {
        const at = await clientXOf(page, i);
        await page.mouse.move(at.x, at.y);
        await page.waitForTimeout(80);
        const s = await cursorState(page);
        assert(s.x === sampleX(i), 'sample ' + i + ': line at ' + s.x);
        assert(s.dots === 1 && s.dotOff <= 1.5, 'sample ' + i + ': ' + JSON.stringify(s));
      }
      const same = await page.evaluate(([h, sel]) => h === document.querySelector(sel), [handle, DOT]);
      assert(same, 'the dot was replaced by a new layer');
      const s = await cursorState(page);
      assert(s.readout[0] === '0 m from A', 'readout at A ' + s.readout[0]);
      assert(/Clearance 10\.0 m/.test(s.readout[3]), 'clearance at A: the antenna height, ' + s.readout[3]);
      const hill = await clientXOf(page, 50);
      await page.mouse.move(hill.x, hill.y);
      await page.waitForTimeout(80);
      const h = await cursorState(page);
      assert(/^Clearance −/.test(h.readout[3]), 'the hill reads as below the line of sight: ' + h.readout[3]);
    });

    await step('leaving the chart hides the line, the readout and the dot', async () => {
      const on = await clientXOf(page, 60);
      await page.mouse.move(on.x, on.y);
      await page.waitForTimeout(80);
      const shown = await cursorState(page);
      assert(shown.lineVisible && shown.dots === 1, 'not shown before leaving: ' + JSON.stringify(shown));
      const box = await page.locator(PANEL).boundingBox();
      await page.mouse.move(box.x + box.width / 2, Math.max(5, box.y - 60));
      await page.waitForTimeout(100);
      const s = await cursorState(page);
      assert(!s.lineVisible && s.readout === null && s.dots === 0, 'still shown: ' + JSON.stringify(s));
      assert(rec.consoleErrors.length === 0, 'console errors: ' + rec.consoleErrors.join(' | '));
    });

    // ── 4. keyboard ──────────────────────────────────────────────────────
    await step('←/→ step one sample, Shift+→ ten, announced politely', async () => {
      await page.focus(PLOT);
      await page.keyboard.press('ArrowRight');
      await page.waitForTimeout(60);
      let s = await cursorState(page);
      assert(s.x === sampleX(0) && s.dots === 1, 'first → is A: ' + JSON.stringify(s));
      await page.keyboard.press('ArrowRight');
      await page.waitForTimeout(60);
      assert((await cursorState(page)).x === sampleX(1), '→ is one sample');
      await page.keyboard.press('Shift+ArrowRight');
      await page.waitForTimeout(60);
      assert((await cursorState(page)).x === sampleX(11), 'Shift+→ is ten');
      await page.keyboard.press('ArrowLeft');
      await page.waitForTimeout(60);
      s = await cursorState(page);
      assert(s.x === sampleX(10), '← is one back: ' + s.x);
      assert(s.dotOff <= 1.5, 'dot off the line by ' + s.dotOff);
      assert(s.live === 'polite', 'aria-live ' + s.live);
      assert(s.readout && s.readout[0] === '840 m from A', 'readout ' + JSON.stringify(s.readout));
      const scrolled = await page.evaluate(() => window.scrollX);
      assert(scrolled === 0, 'the arrow keys scrolled the page');
    });

    // ── 6. perf ──────────────────────────────────────────────────────────
    await step('perf: 500 moves in one task → one update, no rebuild, one dot', async () => {
      const r = await page.evaluate(async ([PLOT, LINE, DOT]) => {
        const plot = document.querySelector(PLOT);
        const svg = plot.querySelector('svg');
        const line = document.querySelector(LINE);
        const chart = plot.parentNode;
        let x1 = 0, childList = 0, dotMoves = 0;
        const mo = new MutationObserver((ms) => ms.forEach((m) => {
          if (m.type === 'attributes' && m.target === line && m.attributeName === 'x1') x1++;
          // The readout's own rows change their text each update; anything
          // else added or removed under the chart would be a rebuild.
          if (m.type === 'childList' && !m.target.closest('[data-testid="tp-cursor-readout"]')) childList++;
        }));
        mo.observe(chart, { subtree: true, childList: true, attributes: true });
        const dot = document.querySelector(DOT);
        const dmo = new MutationObserver((ms) => { dotMoves += ms.filter(m => m.attributeName === 'd').length; });
        if (dot) dmo.observe(dot, { attributes: true });
        const box = svg.getBoundingClientRect();
        const t0 = performance.now();
        for (let i = 0; i < 500; i++) {
          plot.dispatchEvent(new PointerEvent('pointermove', { bubbles: true, pointerType: 'mouse', clientX: box.left + box.width * (0.1 + 0.8 * i / 499), clientY: box.top + 20 }));
        }
        const dispatchMs = performance.now() - t0;
        await new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r)));
        const one = { x1, childList, dotMoves };
        // Then 500 more, 50 per frame over 10 frames.
        x1 = 0; dotMoves = 0;
        let frames = 0;
        for (let f = 0; f < 10; f++) {
          for (let i = 0; i < 50; i++) {
            plot.dispatchEvent(new PointerEvent('pointermove', { bubbles: true, pointerType: 'mouse', clientX: box.left + box.width * (0.1 + 0.08 * f + 0.001 * i), clientY: box.top + 20 }));
          }
          await new Promise(r => requestAnimationFrame(r));
          frames++;
        }
        await new Promise(r => requestAnimationFrame(r));
        frames++;
        mo.disconnect(); dmo.disconnect();
        return { one, spread: { x1, dotMoves, frames, childList }, dispatchMs, dots: document.querySelectorAll(DOT).length,
          sameSvg: plot.querySelector('svg') === svg };
      }, [PLOT, LINE, DOT]);
      assert(r.one.x1 === 1, 'line updates for one burst: ' + r.one.x1);
      assert(r.one.dotMoves <= 1, 'dot updates for one burst: ' + r.one.dotMoves);
      assert(r.one.childList === 0 && r.spread.childList === 0, 'the chart DOM was rebuilt: ' + JSON.stringify(r));
      assert(r.spread.x1 <= r.spread.frames && r.spread.x1 >= 5, 'line updates ' + r.spread.x1 + ' over ' + r.spread.frames + ' frames');
      assert(r.spread.dotMoves <= r.spread.frames, 'dot updates ' + r.spread.dotMoves + ' over ' + r.spread.frames + ' frames');
      assert(r.dots === 1 && r.sameSvg, JSON.stringify(r));
      console.log('    (500 synthetic moves dispatched in ' + r.dispatchMs.toFixed(1) + ' ms)');
    });

    // ── 7. teardown ──────────────────────────────────────────────────────
    await step('Close removes the dot and the line and every cursor listener', async () => {
      const before = await page.evaluate(() => window.__cursorNet.listeners);
      assert(before === 7, 'cursor listeners while open: ' + before);
      await page.click('[data-testid="tp-close"]');
      await page.waitForTimeout(100);
      const s = await page.evaluate(([LINE, READOUT, DOT]) => ({
        dots: document.querySelectorAll(DOT).length, lines: document.querySelectorAll(LINE).length,
        readouts: document.querySelectorAll(READOUT).length, net: window.__cursorNet.listeners,
      }), [LINE, READOUT, DOT]);
      assert(s.dots === 0 && s.lines === 0 && s.readouts === 0 && s.net === 0, JSON.stringify(s));
    });
    await ctx.close();
  }

  {
    const { ctx, page, rec } = await newCtx();
    await step('several Map visits leave no cursor listener, dot or line behind', async () => {
      const seen = [];
      for (let v = 0; v < 3; v++) {
        await openProfile(page);
        const at = await clientXOf(page, 30 + v * 10);
        await page.mouse.move(at.x, at.y);
        await page.waitForTimeout(80);
        const open = await page.evaluate((DOT) => ({ net: window.__cursorNet.listeners, dots: document.querySelectorAll(DOT).length }), DOT);
        assert(open.net === 7 && open.dots === 1, 'visit ' + v + ' while hovering: ' + JSON.stringify(open));
        await page.evaluate(() => { location.hash = '#/nodes'; });
        await page.waitForFunction(() => !document.querySelector('#leaflet-map'), null, { timeout: 15000 });
        await page.waitForTimeout(200);
        seen.push(await page.evaluate(([LINE, DOT]) => window.__cursorNet.listeners + '/' + document.querySelectorAll(LINE).length + '/' + document.querySelectorAll(DOT).length, [LINE, DOT]));
      }
      assert(seen.every(x => x === '0/0/0'), 'listeners/lines/dots after each visit: ' + seen.join(', '));
      assert(rec.consoleErrors.length === 0, 'console errors: ' + rec.consoleErrors.join(' | '));
    });
    await ctx.close();
  }

  // ── 5. touch at 360x740 ────────────────────────────────────────────────
  {
    const { ctx, page, rec } = await newCtx({ viewport: { width: 360, height: 740 }, touch: true });
    await step('touch drag at 360x740 moves the dot, which stays after the lift', async () => {
      await openProfile(page);
      await page.locator(PLOT).scrollIntoViewIfNeeded();
      const cdp = await ctx.newCDPSession(page);
      const from = await clientXOf(page, 20), to = await clientXOf(page, 70);
      const touch = (type, p) => cdp.send('Input.dispatchTouchEvent', { type, touchPoints: p ? [{ x: p.x, y: p.y }] : [] });
      await touch('touchStart', from);
      await page.waitForTimeout(60);
      let s = await cursorState(page);
      assert(s.x === sampleX(20) && s.dots === 1, 'touch down: ' + JSON.stringify(s));
      for (let k = 1; k <= 10; k++) {
        await touch('touchMove', { x: from.x + (to.x - from.x) * k / 10, y: from.y });
        await page.waitForTimeout(20);
      }
      await page.waitForTimeout(60);
      s = await cursorState(page);
      assert(s.x === sampleX(70), 'after the drag the line is at ' + s.x + ', want ' + sampleX(70));
      await touch('touchEnd', null);
      await page.waitForTimeout(150);
      s = await cursorState(page);
      assert(s.lineVisible && s.x === sampleX(70) && s.dots === 1, 'gone after the lift: ' + JSON.stringify(s));
      assert(s.dotOff <= 1.5, 'dot off the line by ' + s.dotOff);
      assert(s.readout && s.readout[0] === '5.88 km from A', 'readout ' + JSON.stringify(s.readout));
      assert(rec.consoleErrors.length === 0, 'console errors: ' + rec.consoleErrors.join(' | '));
    });
    await ctx.close();
  }

  if (SHOTS) {
    const { ctx, page } = await newCtx({ theme: 'dark' });
    await openProfile(page);
    const at = await clientXOf(page, 47);
    await page.mouse.move(at.x, at.y);
    await page.waitForTimeout(150);
    await page.screenshot({ path: path.join(SHOTS, '781-hover-dark.jpg'), type: 'jpeg', quality: 70 });
    await ctx.close();
  }

  await browser.close();
  console.log(`\n#781 E2E: ${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
