/**
 * E2E (#721): move A or B of an open Path profile without restarting it —
 * drag the endpoint markers, or use Move A / Move B / Swap in the panel.
 *
 * Switched on the way test-issue-681-profile-tool-e2e.js does it: the
 * client config gets the `dhm` block with profile: true, and
 * /api/terrain/profile is answered by a stub that echoes the request, so
 * no token and no real service is ever involved.
 *
 * Asserts:
 *   1. the endpoints are draggable 48px markers, and the panel offers
 *      Move A, Move B and Swap;
 *   2. dragging B issues no request while the drag is in progress and
 *      exactly one after the drop; the panel stays open, A, both heights
 *      and the model are kept, and the hash follows;
 *   3. a drop on a node marker snaps to that node (a pubkey endpoint);
 *   4. Escape during a drag puts the marker back, with no request;
 *   5. the Move A flow: the button arms the picker for A only, a click on
 *      the map replaces A in one request; Escape cancels with none;
 *   6. Swap exchanges the ends and their heights in one request;
 *   7. a touch drag at 360px, and the Move B flow by touch;
 *   8. 360x740 and 740x360 keep Move A, Move B and Swap reachable at 48px;
 *   9. axe is clean on the panel and the markers in light and dark;
 *  10. no console errors.
 *
 * Usage:
 *   BASE_URL=http://localhost:13700 node test-issue-721-move-endpoints-e2e.js
 * Optional: SCREENSHOT_DIR=<dir> writes the PR screenshots.
 */
'use strict';
const { chromium } = require('playwright');
const path = require('path');

const BASE = process.env.BASE_URL || 'http://localhost:13700';
const SHOT_DIR = process.env.SCREENSHOT_DIR || '';
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

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

/* The stub answers the shape of a real GET /api/terrain/profile and echoes
 * the heights it was asked for, the way the server does. */
function profileFor(q) {
  const D = 12000;
  const end = (p, h) => {
    const node = /^[0-9a-f]{64}$/.test(p || '');
    const asked = q.get(h);
    return {
      lat: 37.7, lon: -122.2, kind: node ? 'node' : 'point', role: node ? 'repeater' : undefined,
      groundM: 40, antennaM: asked !== null ? Number(asked) : (node ? 10 : 2),
      antennaSource: asked !== null ? 'request' : (node ? 'role-default' : 'point-default'),
      assumed: asked === null,
    };
  };
  const samples = [];
  for (let i = 0; i < 60; i++) {
    const d = Math.round(D * i / 59);
    samples.push({ d, dtm: 30 + 18 * Math.sin(d / 900), dsm: 42 + 18 * Math.sin(d / 900) });
  }
  return {
    a: end(q.get('a'), 'ha'), b: end(q.get('b'), 'hb'),
    distanceM: D, stepM: 11.7, sampleCount: 1025, model: q.get('model') === 'dtm' ? 'dtm' : 'both',
    coverage: { state: 'full', pct: 100, firstGapM: null }, samples,
    physics: { freqMHz: 869.618, freqSource: 'firmware', lambdaM: 0.34474, k: 1.3333333333333333, kSource: 'default', f1MaxM: 32.14 },
    verdict: { state: 'clear', model: 'dtm', criterion: 0.6, worstClearanceM: 41.2, worstAtM: 5000, worstF1Fraction: 1.4 },
    attribution: ATTRIBUTION,
  };
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #721 move A/B of a Path profile against ${BASE} ===\n`);

  async function newCtx(o) {
    o = o || {};
    const ctx = await browser.newContext({
      viewport: o.viewport || { width: 1280, height: 860 },
      hasTouch: !!o.touch, isMobile: !!o.touch,
    });
    const rec = { profile: [], consoleErrors: [], all: [] };
    await ctx.route(BASE + '/api/config/client', async (route) => {
      const response = await route.fetch();
      const config = await response.json();
      config.dhm = DHM_BLOCK;
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
    await ctx.addInitScript(({ theme }) => {
      try {
        localStorage.setItem('meshcore-theme', theme);
        localStorage.setItem('meshcore-map-clustering', 'false');
        ['row-swipe', 'tab-swipe', 'edge-drawer', 'pull-refresh']
          .forEach(k => localStorage.setItem('meshcore-gesture-hints-' + k, 'seen'));
      } catch (_) {}
      const apply = () => document.documentElement && document.documentElement.setAttribute('data-theme', theme);
      apply();
      document.addEventListener('DOMContentLoaded', apply);
    }, { theme: o.theme || 'light' });
    const page = await ctx.newPage();
    page.on('console', m => { if (m.type() === 'error') rec.consoleErrors.push(m.text()); });
    page.on('pageerror', e => rec.consoleErrors.push(String(e)));
    page.on('request', r => rec.all.push(r.url()));
    return { ctx, page, rec };
  }

  // The fixture's nodes are in the Bay Area. Both ends are free points in
  // the upper part of the view, clear of the bottom-sheet panel.
  const VIEW = { lat: 37.45, lon: -122.1, zoom: 10 };
  const A0 = '37.80000,-122.35000', B0 = '37.78000,-121.85000';
  const LINK = `profile=${A0},${B0}&model=dtm&ha=12&hb=4`;
  async function gotoMap(page, extra, view) {
    const v = view || VIEW;
    const qs = `lat=${v.lat}&lon=${v.lon}&zoom=${v.zoom}` + (extra ? '&' + extra : '');
    await page.goto(`${BASE}/#/map?${qs}`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#leaflet-map[data-loaded="true"]', { timeout: 30000 });
    await page.waitForFunction(() => window.MeshConfigReady !== undefined, null, { timeout: 15000 });
    await page.waitForSelector('[data-testid="tp-end-marker-b"]', { timeout: 15000 });
    await page.waitForTimeout(700);
  }

  // A phone shows a strip of map above the panel; the fixture's labels fill
  // the Bay, so the phone cases measure over the open sea west of it.
  const PHONE_VIEW = { lat: 37.35, lon: -122.75, zoom: 10 };
  const PA0 = '37.62000,-122.85000', PB0 = '37.58000,-122.62000';
  const PHONE_LINK = `profile=${PA0},${PB0}&model=dtm&ha=12&hb=4`;
  const PANEL = '[data-testid="tp-panel"]';
  const MK = (s) => `[data-testid="tp-end-marker-${s}"]`;

  async function centre(page, sel) {
    return page.evaluate((s) => {
      const el = document.querySelector(s);
      if (!el) return null;
      const r = el.getBoundingClientRect();
      return { x: Math.round(r.x + r.width / 2), y: Math.round(r.y + r.height / 2), w: r.width, h: r.height };
    }, sel);
  }

  async function hashParams(page) {
    return page.evaluate(() => Object.fromEntries(new URLSearchParams(location.hash.split('?')[1] || '')));
  }

  async function panelOpen(page) {
    return page.evaluate(() => {
      const p = document.querySelector('[data-testid="tp-panel"]');
      return !!(p && p.classList.contains('is-open'));
    });
  }

  /* A spot on the map above the panel, clear of every marker (by 32px,
   * beyond the 24px snap) and every control, so a click or a drop there is
   * a free point. */
  async function freeSpot(page, nearX) {
    return page.evaluate((nx) => {
      const box = document.getElementById('leaflet-map').getBoundingClientRect();
      const marks = Array.from(document.querySelectorAll('.leaflet-marker-icon')).map(el => el.getBoundingClientRect());
      const BLOCK = '.leaflet-control, .map-controls, .map-controls-toggle, .map-profile-toggle, .map-profile-hint, .tp-panel, .leaflet-marker-icon';
      const xs = [];
      // Clear of the control columns on both sides, by more than a finger.
      for (let x = box.x + 90; x < box.right - 90; x += 16) xs.push(x);
      if (nx) xs.sort((p, q) => Math.abs(p - nx) - Math.abs(q - nx));
      for (const x0 of xs) {
        // Below where the picking hint appears (it is hidden until a drag or
        // a Move starts), down to the panel: elementFromPoint refuses
        // anything the panel covers.
        for (let y0 = box.y + 140; y0 < box.bottom - 20; y0 += 12) {
          const x = Math.round(x0), y = Math.round(y0);
          if (!marks.every(r => x < r.x - 32 || x > r.right + 32 || y < r.y - 32 || y > r.bottom + 32)) continue;
          const ok = [[0, 0], [-24, 0], [24, 0], [0, -24], [0, 24]].every(([dx, dy]) => {
            const hit = document.elementFromPoint(x + dx, y + dy);
            return hit && hit.closest('.leaflet-container') && !hit.closest(BLOCK);
          });
          if (ok) return { x, y };
        }
      }
      return null;
    }, nearX || 0);
  }

  /* A node marker on screen and away from the panel, with its key. */
  async function nodeMarker(page) {
    return page.evaluate(() => {
      const box = document.getElementById('leaflet-map').getBoundingClientRect();
      const keys = new Set((window.__mc_nodes || []).map(n => String(n.public_key || '').toLowerCase()));
      const out = [];
      window.__mc_map.eachLayer((l) => {
        if (!(l instanceof window.L.Marker) || !l._icon || !l._nodeKey) return;
        const k = String(l._nodeKey).toLowerCase();
        if (!keys.has(k)) return;
        const r = l._icon.getBoundingClientRect();
        const x = Math.round(r.x + r.width / 2), y = Math.round(r.y + r.height / 2);
        if (x < box.x + 40 || x > box.right - 40 || y < box.y + 70 || y > box.y + box.height * 0.45) return;
        const hit = document.elementFromPoint(x, y);
        if (hit && hit.closest('.tp-end-marker')) return;
        out.push({ x, y, key: k });
      });
      return out[0] || null;
    });
  }

  async function mouseDrag(page, from, to, opts) {
    opts = opts || {};
    await page.mouse.move(from.x, from.y);
    await page.mouse.down();
    const steps = 12;
    for (let i = 1; i <= steps; i++) {
      await page.mouse.move(from.x + (to.x - from.x) * i / steps, from.y + (to.y - from.y) * i / steps);
      await page.waitForTimeout(25);
    }
    if (opts.during) await opts.during();
    await page.mouse.up();
  }

  // ── 1–4. drag, desktop ──────────────────────────────────────────────────
  {
    const { ctx, page, rec } = await newCtx({});
    await gotoMap(page, LINK);

    await step('the ends are draggable 48px markers, and the panel offers Move A, Move B and Swap', async () => {
      assert(await panelOpen(page), 'the link did not open the panel');
      assert(rec.profile.length === 1, 'the link issued ' + rec.profile.length + ' requests');
      for (const s of ['a', 'b']) {
        const c = await centre(page, MK(s));
        assert(c, 'no endpoint marker ' + s);
        assert(c.w >= 48 && c.h >= 48, `marker ${s} is ${c.w}x${c.h}`);
        assert(await page.getAttribute(MK(s), 'role') === 'button', 'marker ' + s + ' is not a focusable control');
      }
      for (const id of ['tp-move-a', 'tp-move-b', 'tp-swap']) {
        const b = await page.$(`[data-testid="${id}"]`);
        assert(b, 'missing ' + id);
        assert(!(await b.isDisabled()), id + ' is disabled with both ends set');
      }
    });

    await step('dragging B: no request during the drag, exactly one after the drop, panel open', async () => {
      const from = await centre(page, MK('b'));
      const to = await freeSpot(page, from.x - 120);
      assert(to, 'no free spot to drop on');
      const before = rec.profile.length;
      const hashBefore = await hashParams(page);
      let during = -1;
      await mouseDrag(page, from, to, {
        during: async () => {
          await page.waitForTimeout(500);   // longer than the debounce
          during = rec.profile.length - before;
          if (SHOT_DIR) await page.screenshot({ path: path.join(SHOT_DIR, '721-drag-desktop.png') });
        },
      });
      assert(during === 0, during + ' requests went out while B was being dragged');
      await page.waitForTimeout(900);
      assert(rec.profile.length - before === 1, 'the drop issued ' + (rec.profile.length - before) + ' requests');
      assert(await panelOpen(page), 'the panel closed on the drop');
      const q = rec.profile[rec.profile.length - 1].searchParams;
      assert(q.get('a') === A0, 'A moved: ' + q.get('a'));
      assert(q.get('b') !== B0 && /^-?\d+\.\d{5},-?\d+\.\d{5}$/.test(q.get('b')), 'B is not a new free point: ' + q.get('b'));
      assert(q.get('ha') === '12' && q.get('hb') === '4', 'heights not kept: ' + q.toString());
      assert(q.get('model') === 'dtm', 'model not kept: ' + q.get('model'));
      const h = await hashParams(page);
      assert(h.profile === A0 + ',' + q.get('b'), 'hash does not follow the drop: ' + h.profile);
      assert(h.lat === hashBefore.lat && h.ha === '12' && h.model === 'dtm', 'hash lost keys: ' + JSON.stringify(h));
      const nameB = await page.textContent('[data-testid="tp-name-b"]');
      assert(/\d+\.\d{4}, -?\d+\.\d{4}/.test(nameB), 'B is not named as the new point: ' + nameB);
    });

    await step('a drop on a node marker snaps B to that node', async () => {
      const target = await nodeMarker(page);
      assert(target, 'no node marker on screen to drop on');
      const from = await centre(page, MK('b'));
      const before = rec.profile.length;
      await mouseDrag(page, from, { x: target.x + 6, y: target.y + 4 });
      await page.waitForTimeout(900);
      assert(rec.profile.length - before === 1, 'the snap issued ' + (rec.profile.length - before) + ' requests');
      const q = rec.profile[rec.profile.length - 1].searchParams;
      assert(q.get('b') === target.key, 'B did not snap to the node: ' + q.get('b') + ' (want ' + target.key + ')');
      assert(q.get('a') === A0, 'A moved');
      assert((await hashParams(page)).profile === A0 + ',' + target.key, 'hash not on the node');
    });

    await step('Escape during a drag puts B back, with no request', async () => {
      const from = await centre(page, MK('b'));
      const to = await freeSpot(page);
      const before = rec.profile.length;
      const hashBefore = (await hashParams(page)).profile;
      await mouseDrag(page, from, to, { during: () => page.keyboard.press('Escape') });
      await page.waitForTimeout(800);
      assert(rec.profile.length === before, 'a cancelled drag issued ' + (rec.profile.length - before) + ' requests');
      const back = await centre(page, MK('b'));
      assert(Math.abs(back.x - from.x) <= 2 && Math.abs(back.y - from.y) <= 2,
        `B was left at ${back.x},${back.y}, not back at ${from.x},${from.y}`);
      assert((await hashParams(page)).profile === hashBefore, 'a cancelled drag changed the hash');
      assert(await panelOpen(page), 'Escape during a drag closed the panel');
    });

    await step('the Move A button arms the picker for A only; a map click replaces A in one request', async () => {
      const bBefore = (await hashParams(page)).profile.split(',').slice(-1)[0];
      await page.click('[data-testid="tp-move-a"]');
      await page.waitForTimeout(200);
      assert(await page.getAttribute('[data-testid="tp-move-a"]', 'aria-pressed') === 'true', 'Move A is not pressed');
      const hint = await page.textContent('[data-testid="map-profile-hint"]');
      assert(/new position for A/.test(hint), 'hint = ' + hint);
      if (SHOT_DIR) await page.screenshot({ path: path.join(SHOT_DIR, '721-move-a-desktop.png') });
      const spot = await freeSpot(page);
      const before = rec.profile.length;
      await page.mouse.click(spot.x, spot.y);
      await page.waitForTimeout(900);
      assert(rec.profile.length - before === 1, 'Move A issued ' + (rec.profile.length - before) + ' requests');
      const q = rec.profile[rec.profile.length - 1].searchParams;
      assert(q.get('a') !== A0 && /^-?\d+\.\d{5},-?\d+\.\d{5}$/.test(q.get('a')), 'A is not the clicked point: ' + q.get('a'));
      assert(q.get('b') === bBefore, 'B changed on Move A: ' + q.get('b'));
      assert(q.get('ha') === '12', 'A lost its height');
      assert(await page.getAttribute('[data-testid="tp-move-a"]', 'aria-pressed') === 'false', 'Move A stayed pressed');
      assert(!(await page.$('[data-testid="map-profile-hint"]:not([hidden])')), 'hint still up');
      assert(await panelOpen(page), 'the panel closed');
    });

    await step('Escape during Move A keeps A where it was, with no request', async () => {
      const hashBefore = (await hashParams(page)).profile;
      await page.click('[data-testid="tp-move-a"]');
      await page.waitForTimeout(150);
      const before = rec.profile.length;
      await page.keyboard.press('Escape');
      await page.waitForTimeout(500);
      assert(rec.profile.length === before, 'Escape issued a request');
      assert((await hashParams(page)).profile === hashBefore, 'Escape moved A');
      assert(await page.getAttribute('[data-testid="tp-move-a"]', 'aria-pressed') === 'false', 'Move A stayed pressed');
      assert(await page.getAttribute('[data-testid="map-profile-toggle"]', 'aria-pressed') === 'false', 'picker still armed');
      assert(await panelOpen(page), 'the first Escape closed the panel');
    });

    await step('Swap exchanges the ends and their heights in one request', async () => {
      const prof = (await hashParams(page)).profile;
      const before = rec.profile.length;
      await page.click('[data-testid="tp-swap"]');
      await page.waitForTimeout(900);
      assert(rec.profile.length - before === 1, 'swap issued ' + (rec.profile.length - before) + ' requests');
      const q = rec.profile[rec.profile.length - 1].searchParams;
      assert(q.get('a') + ',' + q.get('b') !== prof, 'nothing was swapped');
      assert(q.get('ha') === '4' && q.get('hb') === '12', 'heights did not swap: ' + q.toString());
      assert(await page.inputValue('[data-testid="tp-h-a"]') === '4', 'the A input still shows 12');
    });

    await step('no console errors through the flow', async () => {
      assert(rec.consoleErrors.length === 0, rec.consoleErrors.join(' | '));
      const tok = rec.all.filter(u => /[?&](token|ticket|api[_-]?key)=/i.test(u) || /dataforsyningen\.dk/i.test(u));
      assert(tok.length === 0, 'credential or upstream request: ' + tok.join(', '));
    });
    await ctx.close();
  }

  // ── 7. touch at 360px ───────────────────────────────────────────────────
  {
    const { ctx, page, rec } = await newCtx({ viewport: { width: 360, height: 740 }, touch: true });
    await gotoMap(page, PHONE_LINK, PHONE_VIEW);
    const cdp = await ctx.newCDPSession(page);
    async function touchDrag(from, to, during) {
      await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [{ x: from.x, y: from.y }] });
      for (let i = 1; i <= 12; i++) {
        await cdp.send('Input.dispatchTouchEvent', { type: 'touchMove',
          touchPoints: [{ x: from.x + (to.x - from.x) * i / 12, y: from.y + (to.y - from.y) * i / 12 }] });
        await page.waitForTimeout(25);
      }
      if (during) await during();
      await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
    }

    await step('360px touch: dragging B issues nothing during the drag and one request after', async () => {
      assert(await panelOpen(page), 'panel not open');
      const from = await centre(page, MK('b'));
      assert(from, 'no B marker');
      const to = await freeSpot(page, from.x - 60);
      assert(to, 'no free spot at 360px');
      const before = rec.profile.length;
      let during = -1;
      await touchDrag(from, to, async () => {
        await page.waitForTimeout(500);
        during = rec.profile.length - before;
        if (SHOT_DIR) await page.screenshot({ path: path.join(SHOT_DIR, '721-drag-mobile.png') });
      });
      assert(during === 0, during + ' requests during the touch drag');
      await page.waitForTimeout(900);
      assert(rec.profile.length - before === 1, 'the touch drop issued ' + (rec.profile.length - before) + ' requests');
      const q = rec.profile[rec.profile.length - 1].searchParams;
      assert(q.get('a') === PA0 && q.get('b') !== PB0, 'wrong ends after the touch drag: ' + q.toString());
      assert(q.get('hb') === '4', 'B lost its height');
      assert(await panelOpen(page), 'panel closed');
    });

    await step('360px touch: Move B by tap replaces B only', async () => {
      await page.tap('[data-testid="tp-move-b"]');
      await page.waitForTimeout(200);
      assert(await page.getAttribute('[data-testid="tp-move-b"]', 'aria-pressed') === 'true', 'Move B not pressed');
      if (SHOT_DIR) await page.screenshot({ path: path.join(SHOT_DIR, '721-move-b-mobile.png') });
      const spot = await freeSpot(page);
      assert(spot, 'no free spot to tap');
      const before = rec.profile.length;
      await page.touchscreen.tap(spot.x, spot.y);
      await page.waitForTimeout(900);
      assert(rec.profile.length - before === 1, 'Move B issued ' + (rec.profile.length - before) + ' requests');
      const q = rec.profile[rec.profile.length - 1].searchParams;
      assert(q.get('a') === PA0, 'A changed on Move B');
      assert(rec.consoleErrors.length === 0, rec.consoleErrors.join(' | '));
    });
    await ctx.close();
  }

  // ── 8. small viewports ──────────────────────────────────────────────────
  for (const vp of [{ name: '360x740 portrait', w: 360, h: 740 }, { name: '740x360 landscape', w: 740, h: 360 }]) {
    const { ctx, page, rec } = await newCtx({ viewport: { width: vp.w, height: vp.h }, touch: true });
    await gotoMap(page, PHONE_LINK, PHONE_VIEW);
    await step(vp.name + ': Move A, Move B and Swap are reachable and at least 48px', async () => {
      const report = await page.evaluate(() => {
        const out = [];
        for (const id of ['tp-move-a', 'tp-move-b', 'tp-swap', 'tp-close']) {
          const el = document.querySelector('[data-testid="' + id + '"]');
          if (!el) { out.push({ id, missing: true }); continue; }
          el.scrollIntoView({ block: 'center' });
          const r = el.getBoundingClientRect();
          const hit = document.elementFromPoint(Math.round(r.x + r.width / 2), Math.round(r.y + r.height / 2));
          out.push({ id, w: Math.round(r.width), h: Math.round(r.height),
            inView: r.top >= 0 && r.bottom <= window.innerHeight + 1 && r.left >= 0 && r.right <= window.innerWidth + 1,
            hittable: !!hit && (el === hit || el.contains(hit)) });
        }
        return { controls: out, overflowX: document.documentElement.scrollWidth > window.innerWidth + 1 };
      });
      assert(!report.overflowX, 'the page scrolls sideways');
      for (const c of report.controls) {
        assert(!c.missing, 'missing ' + c.id);
        assert(c.w >= 48 && c.h >= 48, `${c.id} is ${c.w}x${c.h}`);
        assert(c.inView, c.id + ' cannot be scrolled into view');
        assert(c.hittable, c.id + ' is covered');
      }
      assert(rec.consoleErrors.length === 0, rec.consoleErrors.join(' | '));
    });
    await ctx.close();
  }

  // ── 9. axe ──────────────────────────────────────────────────────────────
  const AXE_RULES = ['color-contrast', 'label', 'aria-required-attr', 'aria-valid-attr', 'aria-valid-attr-value',
    'button-name', 'aria-command-name', 'link-name', 'duplicate-id', 'nested-interactive', 'aria-allowed-attr'];
  for (const theme of ['light', 'dark']) {
    const { ctx, page } = await newCtx({ theme });
    await gotoMap(page, LINK);
    await step('axe is clean on the panel and the A/B markers in ' + theme, async () => {
      const { AxeBuilder } = require('@axe-core/playwright');
      await page.click('[data-testid="tp-move-b"]');     // the pressed state too
      await page.waitForTimeout(200);
      const res = await new AxeBuilder({ page }).include(PANEL).include(MK('a')).include(MK('b'))
        .withRules(AXE_RULES).analyze();
      const v = res.violations.map(x => x.id + ' x' + x.nodes.length);
      assert(res.violations.length === 0, 'axe violations: ' + v.join(', '));
    });
    /* axe files the A/B discs under "incomplete" — small and rounded — so
     * the tag text is measured directly, as test-issue-681-profile-tool-e2e
     * does for the panel's own A/B tags. */
    await step('the A/B tag on the map markers is at least 4.5:1 in ' + theme, async () => {
      const rows = await page.evaluate(() => Array.from(document.querySelectorAll('.tp-end-dot')).map((el) => {
        const rgb = (c) => (c.match(/[\d.]+/g) || []).slice(0, 3).map(Number);
        const lum = (c) => {
          const [r, g, b] = rgb(c).map(v => { v /= 255; return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4); });
          return 0.2126 * r + 0.7152 * g + 0.0722 * b;
        };
        const cs = getComputedStyle(el);
        const l1 = lum(cs.color), l2 = lum(cs.backgroundColor);
        return { text: el.textContent, fg: cs.color, bg: cs.backgroundColor,
          ratio: Math.round((Math.max(l1, l2) + 0.05) / (Math.min(l1, l2) + 0.05) * 100) / 100 };
      }));
      assert(rows.length === 2, 'found ' + rows.length + ' A/B discs');
      for (const r of rows) assert(r.ratio >= 4.5, `${r.text} is ${r.ratio}:1 (${r.fg} on ${r.bg})`);
    });
    await ctx.close();
  }

  await browser.close();
  console.log(`\n#721 E2E: ${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
