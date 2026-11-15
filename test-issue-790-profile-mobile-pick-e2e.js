/**
 * E2E (#790): on a phone the Path profile panel must leave the map free to
 * tap while an end is still to be picked. After A, and while a Move A /
 * Move B is pending, the panel is a compact bar — the title, the next
 * step, Cancel and Close — and a tap on the map above it picks the end.
 * With both ends it expands with the chart, as before.
 *
 * Switched on the way test-issue-721-move-endpoints-e2e.js does it: the
 * client config gets the `dhm` block with profile: true and
 * /api/terrain/profile is answered by a stub, so no token and no real
 * service is involved.
 *
 * At 375x667, 390x844 (iPhone), 360x740 (Android) and 667x375 (landscape),
 * with touch:
 *   1. the toggle, then a tap on the map picks A; the panel is the compact
 *      bar (status visible, Cancel and Close at 48px, no body, no chip
 *      pill) and leaves most of the map uncovered;
 *   2. a tap on the visible map above the bar sets B: one request;
 *   3. the panel expands with the chart;
 *   4. Move A is compact while pending, a tap moves A, it expands again;
 *      Cancel during Move B keeps B, with no request;
 *   5. Cancel with only A resets the profile and stays on the Map page;
 *   6. (F4) the ▾/▴ toggle folds the finished profile to its head (title,
 *      verdict chips, the verdict as status), the map above it can be
 *      dragged, and unfolding brings the panel back — no request, same
 *      hash;
 * at 375x560 and 360x640 too (the reviewer's small phones);
 * plus, once: the #781 hover cursor still puts its dot on the map after
 * the panel has been compact and expanded again, and after fold/unfold;
 * axe is clean on the bar and on the folded head; at 1280x800 (desktop,
 * keyboard) the panel is compact while picking (F3), and focus goes
 * Move → Cancel → back to Move after Escape and after Cancel (F1/F2);
 * no console errors.
 *
 * Usage:
 *   BASE_URL=http://localhost:13800 node test-issue-790-profile-mobile-pick-e2e.js
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

function profileFor(q) {
  const D = 12000;
  const end = (p, h) => {
    const node = /^[0-9a-f]{64}$/.test(p || '');
    const asked = q.get(h);
    return {
      lat: 37.6, lon: -122.7, kind: node ? 'node' : 'point', role: node ? 'repeater' : undefined,
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

const PANEL = '[data-testid="tp-panel"]';
const TOGGLE = '[data-testid="map-profile-toggle"]';
const CANCEL = '[data-testid="tp-cancel"]';
const DOT = '.leaflet-overlay-pane .tp-cursor-dot';
// Open sea west of the fixture's Bay Area nodes: no markers to snap to.
const PHONE_VIEW = { lat: 37.5, lon: -122.85, zoom: 10 };

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #790 Path profile picking on phones against ${BASE} ===\n`);

  async function newCtx(o) {
    const touch = o.touch !== false;
    const ctx = await browser.newContext({ viewport: o.viewport, hasTouch: touch, isMobile: touch });
    const rec = { profile: [], consoleErrors: [] };
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
    await ctx.route(/^https?:\/\/(?!localhost|127\.0\.0\.1)/i, route => route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 }));
    await ctx.route(url => new URL(url).pathname === '/api/map/dhm/wms', route => route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 }));
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
    return { ctx, page, rec };
  }

  async function gotoMap(page) {
    const v = PHONE_VIEW;
    await page.goto(`${BASE}/#/map?lat=${v.lat}&lon=${v.lon}&zoom=${v.zoom}`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#leaflet-map[data-loaded="true"]', { timeout: 30000 });
    await page.waitForFunction(() => window.MeshConfigReady !== undefined, null, { timeout: 15000 });
    await page.waitForSelector(TOGGLE, { timeout: 15000 });
    await page.waitForTimeout(500);
  }

  /* A spot on the visible map — not under the panel, a control, the hint
   * or a marker (by 32px, beyond the 24px snap) — at or below minY, and
   * away from `avoid` so A and B are not the same point. */
  async function freeSpot(page, o) {
    o = o || {};
    return page.evaluate(({ minFrac, avoid }) => {
      const box = document.getElementById('leaflet-map').getBoundingClientRect();
      const marks = Array.from(document.querySelectorAll('.leaflet-marker-icon')).map(el => el.getBoundingClientRect());
      const BLOCK = '.leaflet-control, .map-controls, .map-controls-toggle, .map-profile-toggle, .map-profile-hint, .tp-panel, .leaflet-marker-icon';
      const y0 = box.y + Math.max(40, box.height * (minFrac || 0));
      for (let y = y0; y < box.bottom - 20; y += 10) {
        for (let x = box.x + 60; x < box.right - 60; x += 14) {
          const px = Math.round(x), py = Math.round(y);
          if (avoid && Math.hypot(px - avoid.x, py - avoid.y) < 60) continue;
          if (!marks.every(r => px < r.x - 32 || px > r.right + 32 || py < r.y - 32 || py > r.bottom + 32)) continue;
          const ok = [[0, 0], [-20, 0], [20, 0], [0, -20], [0, 20]].every(([dx, dy]) => {
            const hit = document.elementFromPoint(px + dx, py + dy);
            return hit && hit.closest('.leaflet-container') && !hit.closest(BLOCK);
          });
          if (ok) return { x: px, y: py };
        }
      }
      return null;
    }, { minFrac: o.minFrac || 0, avoid: o.avoid || null });
  }

  /* The panel as a viewer sees it. */
  async function panelView(page) {
    return page.evaluate(() => {
      const q = (s) => document.querySelector(s);
      const shown = (el) => !!(el && el.getClientRects().length && getComputedStyle(el).visibility !== 'hidden');
      const box = (el) => { const r = el.getBoundingClientRect(); return { x: r.x, y: r.y, w: r.width, h: r.height, bottom: r.bottom }; };
      const panel = q('[data-testid="tp-panel"]');
      const map = document.getElementById('leaflet-map').getBoundingClientRect();
      const chips = Array.from(panel ? panel.querySelectorAll('.tp-chip') : []).filter(shown);
      const status = q('[data-testid="tp-pick-status"]');
      const fold = q('[data-testid="tp-fold"]');
      const cancel = q('[data-testid="tp-cancel"]');
      const close = q('[data-testid="tp-close"]');
      return {
        open: !!(panel && panel.classList.contains('is-open')),
        phase: panel && panel.getAttribute('data-phase'),
        panel: panel ? box(panel) : null,
        map: { y: map.y, h: map.height, bottom: map.bottom },
        status: shown(status) ? status.textContent : null,
        statusBox: shown(status) ? box(status) : null,
        cancel: shown(cancel) ? box(cancel) : null,
        close: shown(close) ? box(close) : null,
        copy: shown(q('[data-testid="tp-copy-link"]')),
        titleText: panel ? panel.querySelector('.tp-title').innerText.replace(/\s+/g, '') .replace('Pathprofile', 'Path profile') : null,
        fold: shown(fold) ? Object.assign(box(fold), { tag: fold.tagName, expanded: fold.getAttribute('aria-expanded'), glyph: fold.querySelector('[aria-hidden]').textContent, name: fold.textContent.replace(/[▾▴]/g, '').trim(), inTitle: !!fold.closest('h3.tp-title') }) : null,
        body: shown(q('[data-testid="tp-body"]')),
        chart: shown(q('[data-testid="tp-chart"] svg.tp-svg')),
        chips: chips.map(c => c.textContent),
        emptyChips: chips.filter(c => !c.textContent.trim()).length,
        hash: location.hash,
        armed: (q('[data-testid="map-profile-toggle"]') || {}).getAttribute
          ? q('[data-testid="map-profile-toggle"]').getAttribute('aria-pressed') : null,
      };
    });
  }

  function assertCompact(v, phase, statusRe, where) {
    assert(v.open, where + ': the panel is not open');
    // The bar takes no more than a third of the map area (a phone's map is
    // already short); the rest stays tappable.
    const covered = v.panel.h / v.map.h;
    assert(covered <= 0.34, where + ': the panel covers ' + Math.round(covered * 100) + '% of the map (' +
      Math.round(v.panel.h) + ' of ' + Math.round(v.map.h) + ' px)');
    assert(v.status && statusRe.test(v.status), where + ': status ' + JSON.stringify(v.status));
    assert(!v.body, where + ': the body (credit, A/B rows, steppers) is shown in the compact bar');
    assert(v.emptyChips === 0 && v.chips.length === 0, where + ': chips in the bar: ' + JSON.stringify(v.chips));
    assert(v.cancel && v.cancel.h >= 48 && v.cancel.w >= 48, where + ': Cancel missing or under 48px: ' + JSON.stringify(v.cancel));
    assert(v.close && v.close.h >= 48 && v.close.w >= 48, where + ': Close missing or under 48px');
    assert(!v.copy, where + ': the copy-link button is in the compact bar');
    // The status is inside the viewport and the panel, never cut off.
    assert(v.statusBox.bottom <= v.panel.bottom + 0.5 && v.statusBox.y >= v.panel.y - 0.5, where + ': status outside the bar');
    assert(v.phase === phase, where + ': phase ' + v.phase + ', want ' + phase);
  }

  async function tap(page, spot) {
    await page.touchscreen.tap(spot.x, spot.y);
    await page.waitForTimeout(700);
  }

  /* The map pane's offset: it changes when the map is dragged. */
  async function paneOffset(page) {
    return page.evaluate(() => {
      const r = document.querySelector('.leaflet-map-pane').getBoundingClientRect();
      return { x: Math.round(r.x), y: Math.round(r.y) };
    });
  }

  /* A one-finger drag on the map, by raw touch events (Playwright has
   * only tap). */
  async function touchDrag(page, from, dx, dy) {
    const cdp = await page.context().newCDPSession(page);
    const pt = (x, y) => [{ x: Math.round(x), y: Math.round(y), id: 1 }];
    await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: pt(from.x, from.y) });
    for (let i = 1; i <= 8; i++) {
      await cdp.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: pt(from.x + dx * i / 8, from.y + dy * i / 8) });
      await page.waitForTimeout(16);
    }
    await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
    await cdp.detach();
    await page.waitForTimeout(500);
  }

  const VIEWPORTS = [
    { name: '375x560', viewport: { width: 375, height: 560 } },
    { name: '360x640', viewport: { width: 360, height: 640 } },
    { name: '375x667', viewport: { width: 375, height: 667 } },
    { name: '390x844', viewport: { width: 390, height: 844 } },
    { name: '360x740', viewport: { width: 360, height: 740 } },
    { name: '667x375 landscape', viewport: { width: 667, height: 375 } },
  ];

  for (const vp of VIEWPORTS) {
    const { ctx, page, rec } = await newCtx({ viewport: vp.viewport });
    await gotoMap(page);
    let spotA = null;

    await step(vp.name + ': after A the panel is a compact bar and the map above it is free', async () => {
      await page.tap(TOGGLE);
      await page.waitForTimeout(200);
      spotA = await freeSpot(page);
      assert(spotA, 'no free spot for A');
      await tap(page, spotA);
      const v = await panelView(page);
      assertCompact(v, 'pick-b', /^Pick the second point on the map\.$/, 'after A');
      assert(rec.profile.length === 0, 'A alone issued a request');
      if (SHOT_DIR) await page.screenshot({ path: path.join(SHOT_DIR, '790-pick-b-' + vp.name.split(' ')[0] + '.png') });
    });

    await step(vp.name + ': a tap on the visible map sets B, and the panel expands with the chart', async () => {
      const spotB = await freeSpot(page, { avoid: spotA });
      assert(spotB, 'no map left to tap for B: the panel covers it');
      await tap(page, spotB);
      const t0 = Date.now();
      while (rec.profile.length === 0 && Date.now() - t0 < 5000) await page.waitForTimeout(100);
      await page.waitForTimeout(400);
      assert(rec.profile.length === 1, 'B issued ' + rec.profile.length + ' requests');
      const v = await panelView(page);
      assert(v.body && v.chart, 'the panel did not expand with the chart: ' + JSON.stringify({ body: v.body, chart: v.chart }));
      assert(v.status === null && v.cancel === null, 'the compact status/Cancel stayed in the full panel');
      assert(v.emptyChips === 0, 'an empty chip pill is shown');
      assert(v.phase === 'ready', 'phase ' + v.phase);
      const prof = (new URLSearchParams(v.hash.split('?')[1]).get('profile') || '').split(',');
      assert(prof.length === 4, 'the hash does not hold both ends: ' + v.hash);
      if (SHOT_DIR) await page.screenshot({ path: path.join(SHOT_DIR, '790-ready-' + vp.name.split(' ')[0] + '.png') });
    });

    await step(vp.name + ': Move A is compact while pending; a tap moves A and it expands again', async () => {
      const before = rec.profile.length;
      const prof = new URLSearchParams((await panelView(page)).hash.split('?')[1]).get('profile').split(',');
      await page.locator('[data-testid="tp-move-a"]').scrollIntoViewIfNeeded();
      await page.tap('[data-testid="tp-move-a"]');
      await page.waitForTimeout(250);
      assertCompact(await panelView(page), 'move-a', /^Pick the new position for A on the map\.$/, 'Move A');
      const spot = await freeSpot(page, { avoid: spotA });
      assert(spot, 'no map left to tap for the new A');
      await tap(page, spot);
      const t0 = Date.now();
      while (rec.profile.length === before && Date.now() - t0 < 5000) await page.waitForTimeout(100);
      await page.waitForTimeout(400);
      assert(rec.profile.length === before + 1, 'Move A issued ' + (rec.profile.length - before) + ' requests');
      const q = rec.profile[rec.profile.length - 1].searchParams;
      assert(q.get('a') !== prof.slice(0, 2).join(','), 'A did not move');
      assert(q.get('b') === prof.slice(2).join(','), 'B changed on Move A');
      const v = await panelView(page);
      assert(v.phase === 'ready' && v.body && v.chart, 'not expanded after Move A: ' + JSON.stringify({ phase: v.phase, body: v.body }));
    });

    await step(vp.name + ': Cancel during Move B keeps B, with no request, and expands', async () => {
      const before = rec.profile.length;
      const hashBefore = (await panelView(page)).hash;
      await page.locator('[data-testid="tp-move-b"]').scrollIntoViewIfNeeded();
      await page.tap('[data-testid="tp-move-b"]');
      await page.waitForTimeout(250);
      assertCompact(await panelView(page), 'move-b', /^Pick the new position for B on the map\.$/, 'Move B');
      await page.tap(CANCEL);
      await page.waitForTimeout(600);
      const v = await panelView(page);
      assert(rec.profile.length === before, 'Cancel issued a request');
      assert(v.hash === hashBefore, 'Cancel moved an end: ' + v.hash);
      assert(v.open && v.phase === 'ready' && v.body, 'not the full panel after Cancel: ' + JSON.stringify({ open: v.open, phase: v.phase }));
      assert(v.armed === 'false', 'the picker is still armed after Cancel');
    });

    await step(vp.name + ': ▾ folds the profile to its head, the map above it drags, ▴ unfolds it (F4)', async () => {
      const before = rec.profile.length;
      let v = await panelView(page);
      const hashBefore = v.hash;
      const full = v.panel.h;
      assert(v.fold, 'no fold toggle on the finished profile');
      assert(v.fold.tag === 'BUTTON', 'the toggle is a ' + v.fold.tag + ', not a button');
      assert(v.fold.w >= 48 && v.fold.h >= 48, 'the toggle is under 48px: ' + JSON.stringify(v.fold));
      assert(v.fold.expanded === 'true' && v.fold.glyph === '▾', 'expanded toggle: ' + JSON.stringify(v.fold));
      assert(v.fold.inTitle && v.fold.name === 'Path profile', 'the toggle is not the title: ' + JSON.stringify(v.fold));
      assert(v.titleText === 'Path profile▾', 'the title row reads ' + JSON.stringify(v.titleText));
      await page.tap('[data-testid="tp-fold"]');
      await page.waitForTimeout(250);
      v = await panelView(page);
      assert(v.fold && v.fold.expanded === 'false' && v.fold.glyph === '▴', 'folded toggle: ' + JSON.stringify(v.fold));
      assert(!v.body, 'the body is still shown when folded');
      assert(v.chips.length >= 1 && v.emptyChips === 0 && /Clear$/.test(v.chips[0]), 'folded chips: ' + JSON.stringify(v.chips));
      assert(v.status === 'The whole first Fresnel zone is clear of terrain.', 'folded status: ' + JSON.stringify(v.status));
      assert(v.statusBox.bottom <= v.panel.bottom + 0.5, 'the status is cut off');
      assert(v.close, 'Close left the folded head');
      const covered = v.panel.h / v.map.h;
      assert(covered <= 0.34, 'folded, the panel covers ' + Math.round(covered * 100) + '% of the map (' +
        Math.round(v.panel.h) + ' px, ' + Math.round(full) + ' px unfolded)');
      if (SHOT_DIR) await page.screenshot({ path: path.join(SHOT_DIR, '790-folded-' + vp.name.split(' ')[0] + '.png') });
      // The map the folded panel gave back can be dragged.
      const spot = await freeSpot(page);
      assert(spot, 'no free map to drag while folded');
      const pane0 = await paneOffset(page);
      await touchDrag(page, spot, 0, 60);
      const pane1 = await paneOffset(page);
      assert(Math.abs(pane1.y - pane0.y) >= 30, 'the map did not move: ' + JSON.stringify([pane0, pane1]));
      v = await panelView(page);
      assert(v.fold && v.fold.expanded === 'false', 'dragging the map unfolded the panel');
      await page.tap('[data-testid="tp-fold"]');
      await page.waitForTimeout(250);
      v = await panelView(page);
      assert(v.fold && v.fold.expanded === 'true' && v.fold.glyph === '▾', 'unfolded toggle: ' + JSON.stringify(v.fold));
      assert(v.body && v.chart && v.phase === 'ready', 'unfold did not bring the chart back');
      assert(v.status === null, 'the folded status stayed');
      assert(Math.abs(v.panel.h - full) <= 1, 'unfolded height ' + v.panel.h + ', was ' + full);
      assert(rec.profile.length === before, 'fold/unfold issued a request');
      const prof = (h) => new URLSearchParams(h.split('?')[1]).get('profile');
      assert(prof(v.hash) === prof(hashBefore), 'the profile changed: ' + v.hash);
    });

    await step(vp.name + ': Cancel with only A resets the profile and stays on the Map', async () => {
      await page.tap('[data-testid="tp-close"]');
      await page.waitForTimeout(300);
      await page.tap(TOGGLE);
      await page.waitForTimeout(200);
      const spot = await freeSpot(page);
      await tap(page, spot);
      assert((await panelView(page)).phase === 'pick-b', 'not picking B');
      await page.tap(CANCEL);
      await page.waitForTimeout(400);
      const v = await panelView(page);
      assert(!v.open, 'the panel is still open after Cancel');
      assert(/^#\/map\?/.test(v.hash), 'Cancel left the Map page: ' + v.hash);
      assert(!/profile=/.test(v.hash), 'the profile is still in the hash: ' + v.hash);
      assert(v.armed === 'false', 'the picker is still armed');
      assert(await page.locator('.tp-path-line, .tp-path-ghost').count() === 0, 'a path line was left on the map');
      assert(rec.consoleErrors.length === 0, 'console errors: ' + rec.consoleErrors.join(' | '));
    });
    await ctx.close();
  }

  // ── #781 hover sync and axe, once ─────────────────────────────────────
  for (const theme of ['light', 'dark']) {
    const { ctx, page, rec } = await newCtx({ viewport: { width: 390, height: 844 }, theme });
    await gotoMap(page);
    await step(theme + ': axe is clean on the compact bar', async () => {
      await page.tap(TOGGLE);
      await page.waitForTimeout(200);
      const a = await freeSpot(page);
      await tap(page, a);
      const { AxeBuilder } = require('@axe-core/playwright');
      const res = await new AxeBuilder({ page }).include(PANEL)
        .withRules(['color-contrast', 'button-name', 'aria-allowed-attr', 'aria-valid-attr-value', 'nested-interactive', 'target-size'])
        .analyze();
      assert(res.violations.length === 0, 'axe: ' + res.violations.map(x => x.id + ' x' + x.nodes.length).join(', '));
      const b = await freeSpot(page, { avoid: a });
      await tap(page, b);
      await page.waitForSelector('[data-testid="tp-chart"] svg.tp-svg', { timeout: 5000 });
    });
    await ctx.close();
  }

  /* #781: the hover cursor is a mouse affordance, so this one runs without
   * touch, at the same phone width, through a compact Move B and back. */
  {
    const { ctx, page, rec } = await newCtx({ viewport: { width: 390, height: 844 }, touch: false });
    await gotoMap(page);
    await step('the #781 hover cursor still puts one dot on the map after compact → expanded', async () => {
      await page.click(TOGGLE);
      await page.waitForTimeout(200);
      const a = await freeSpot(page);
      await page.mouse.click(a.x, a.y);
      await page.waitForTimeout(500);
      assert((await panelView(page)).phase === 'pick-b', 'not compact after A');
      const b = await freeSpot(page, { avoid: a });
      await page.mouse.click(b.x, b.y);
      await page.waitForSelector('[data-testid="tp-chart"] svg.tp-svg', { timeout: 5000 });
      await page.click('[data-testid="tp-move-b"]');
      await page.waitForTimeout(200);
      assert((await panelView(page)).phase === 'move-b', 'not compact during Move B');
      await page.click(CANCEL);
      await page.waitForTimeout(300);
      // Clicking Move B scrolled the panel body down to it; back to the chart.
      await page.locator('[data-testid="tp-chart"] .tp-plot svg').scrollIntoViewIfNeeded();
      const svg = await page.locator('[data-testid="tp-chart"] .tp-plot svg').boundingBox();
      assert(svg, 'no chart to hover');
      await page.mouse.move(svg.x + svg.width * 0.4, svg.y + svg.height / 2);
      await page.waitForTimeout(150);
      const dots = await page.locator(DOT).count();
      assert(dots === 1, 'dots on the map while hovering: ' + dots);
      assert(rec.consoleErrors.length === 0, 'console errors: ' + rec.consoleErrors.join(' | '));
    });
    await step('the #781 hover cursor still works after ▾ fold and ▴ unfold (F4)', async () => {
      await page.mouse.move(5, 5);
      await page.waitForTimeout(150);
      assert(await page.locator(DOT).count() === 0, 'the dot stayed after mouse-leave');
      await page.click('[data-testid="tp-fold"]');
      await page.waitForTimeout(200);
      assert(!(await panelView(page)).body, 'not folded');
      await page.click('[data-testid="tp-fold"]');
      await page.waitForTimeout(200);
      await page.locator('[data-testid="tp-chart"] .tp-plot svg').scrollIntoViewIfNeeded();
      const svg = await page.locator('[data-testid="tp-chart"] .tp-plot svg').boundingBox();
      assert(svg && svg.width > 0, 'no chart to hover after unfold');
      await page.mouse.move(svg.x + svg.width * 0.6, svg.y + svg.height / 2);
      await page.waitForTimeout(150);
      const dots = await page.locator(DOT).count();
      assert(dots === 1, 'dots on the map while hovering after unfold: ' + dots);
      assert(rec.consoleErrors.length === 0, 'console errors: ' + rec.consoleErrors.join(' | '));
    });
    await ctx.close();
  }

  /* F3: desktop collapses too — the compact bar has no viewport condition,
   * by decision, because it keeps the map free on every screen. And F1/F2:
   * a keyboard Move puts focus on Cancel, and Escape or Cancel bring it
   * back to the Move button, never to <body>. */
  {
    const { ctx, page, rec } = await newCtx({ viewport: { width: 1280, height: 800 }, touch: false });
    await gotoMap(page);
    const active = () => page.evaluate(() => {
      const a = document.activeElement;
      return a ? (a.getAttribute('data-testid') || a.tagName) : null;
    });
    await step('1280x800: the panel is the compact bar while B is picked, and during a Move (F3)', async () => {
      await page.click(TOGGLE);
      await page.waitForTimeout(200);
      const a = await freeSpot(page);
      await page.mouse.click(a.x, a.y);
      await page.waitForTimeout(500);
      let v = await panelView(page);
      assert(v.phase === 'pick-b' && !v.body && v.cancel && v.status === 'Pick the second point on the map.',
        'not compact after A on desktop: ' + JSON.stringify({ phase: v.phase, body: v.body, status: v.status }));
      assert(v.panel.h <= 120, 'the desktop bar is ' + Math.round(v.panel.h) + ' px tall');
      const b = await freeSpot(page, { avoid: a });
      await page.mouse.click(b.x, b.y);
      await page.waitForSelector('[data-testid="tp-chart"] svg.tp-svg', { timeout: 5000 });
      await page.click('[data-testid="tp-move-a"]');
      await page.waitForTimeout(200);
      v = await panelView(page);
      assert(v.phase === 'move-a' && !v.body && v.panel.h <= 120, 'not compact during Move A on desktop: ' + JSON.stringify({ phase: v.phase, h: v.panel.h }));
      await page.click(CANCEL);
      await page.waitForTimeout(200);
      assert((await panelView(page)).phase === 'ready', 'not ready after Cancel');
    });
    for (const [side, how] of [['a', 'Escape'], ['a', 'Cancel'], ['b', 'Escape'], ['b', 'Cancel']]) {
      await step('1280x800 keyboard: Move ' + side.toUpperCase() + ' → focus on Cancel → ' + how + ' → focus back on Move ' + side.toUpperCase() + ' (F1/F2)', async () => {
        const before = rec.profile.length;
        await page.focus('[data-testid="tp-move-' + side + '"]');
        await page.keyboard.press('Enter');
        await page.waitForTimeout(200);
        assert((await panelView(page)).phase === 'move-' + side, 'Enter did not start Move ' + side.toUpperCase());
        assert(await active() === 'tp-cancel', 'focus during the move: ' + await active());
        // On Cancel, a keyboard press (Enter) — the button has focus.
        await page.keyboard.press(how === 'Escape' ? 'Escape' : 'Enter');
        await page.waitForTimeout(250);
        const v = await panelView(page);
        assert(v.open && v.phase === 'ready', how + ' did not end the move: ' + JSON.stringify({ open: v.open, phase: v.phase }));
        assert(await active() === 'tp-move-' + side, 'focus after ' + how + ': ' + await active());
        assert(rec.profile.length === before, how + ' issued a request');
      });
    }
    await step('1280x800: the folded head passes axe', async () => {
      await page.click('[data-testid="tp-fold"]');
      await page.waitForTimeout(200);
      const { AxeBuilder } = require('@axe-core/playwright');
      const res = await new AxeBuilder({ page }).include(PANEL)
        .withRules(['color-contrast', 'button-name', 'aria-allowed-attr', 'aria-valid-attr-value', 'nested-interactive', 'target-size'])
        .analyze();
      assert(res.violations.length === 0, 'axe: ' + res.violations.map(x => x.id + ' x' + x.nodes.length).join(', '));
      assert(rec.consoleErrors.length === 0, 'console errors: ' + rec.consoleErrors.join(' | '));
    });
    await ctx.close();
  }

  await browser.close();
  console.log(`\n#790 E2E: ${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
