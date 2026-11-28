/**
 * E2E (#723 M3): the protected ancient monument warning in the Map page's
 * Path profile.
 *
 * The CI server has neither map.dhm nor map.heritage, so
 * /api/config/client is intercepted and given both blocks,
 * /api/terrain/profile is answered with a synthetic profile and
 * /api/terrain/heritage with fixed synthetic answers per end. The agency's
 * WMS/WFS, DHM and every other outside host are stubbed or refused here:
 * no request leaves the machine.
 *
 * Asserts:
 *   1. A inside a zone, B 42 m from one, and both at once: the warn chip
 *      and the note under the detail line, never in the title row, with the neutral
 *      wording, each end's type and distance and the register's credit;
 *      one lat/lon-only lookup per end after the profile; the verdict chip
 *      exactly as it reads without the heritage block;
 *   2. nothing near, and failed lookups (502, 429): no chip, no note, and
 *      nothing in the panel that reads as an all-clear;
 *   3. Close takes the note away, and the next profile does not show the
 *      old one; a height edit re-measures without a new lookup;
 *   4. without the heritage block: no lookup at all;
 *   5. hostile dataset text is shown as text and never runs;
 *   6. 740x360, 568x320 and 844x390 landscape (touch): the note does not
 *      move the chart, which stays on screen, and the 48 px close button
 *      survives;
 *   6b. 640x360, 667x375, 667x340, 667x330 and 667x320 landscape (touch)
 *      with every chip showing (the surface chip, "assumed heights" and
 *      the monument chip): the title row keeps its height, Copy and Close
 *      stay on it, the chart stays where it was and fully on screen, and
 *      the #781 hover cursor still follows a touch on the chart;
 *   6c. 568x320, 640x360, 640x340, 640x320 and 667x320 landscape (touch) with the
 *      widest chips ("Terrain: Marginal", "Buildings/trees: Blocked",
 *      "assumed heights"): Copy and Close share the title's top and take
 *      a tap, no chip runs in under them, so the title row never breaks,
 *      and the chart ends above the bottom navigation. Geometry only, so it holds for any font (#796 F5);
 *   7. light and dark: the chip and the note border use --warning;
 *   8. no console errors, no request to the agency or any outside host.
 *
 * Usage:
 *   BASE_URL=http://localhost:13581 node test-issue-723-profile-warning-e2e.js
 * Optional: SCREENSHOT_DIR=<dir> writes small JPG screenshots.
 */
'use strict';
const { chromium } = require('playwright');
const fs = require('fs');
const path = require('path');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const SHOT_DIR = process.env.SCREENSHOT_DIR || '';
const PROFILE_PATH = '/api/terrain/profile';
const HERITAGE_PATH = '/api/terrain/heritage';
const HERITAGE_BLOCK = {
  enabled: true, url: 'https://www.kulturarv.dk/ffgeoserver/public/wms',
  layers: 'fundogfortidsminder_areal_beskyttelse,fundogfortidsminder_punkt_fredet',
  minZoom: 12, maxZoom: 19, bounds: [[54.4646, 8.0899], [57.7289, 15.5689]],
  attribution: '© Slots- og Kulturstyrelsen',
};
const DHM_BLOCK = {
  enabled: true, url: '/api/map/dhm/wms', layer: 'dhm_terraen_skyggekort', minZoom: 7, maxZoom: 19, profile: true,
  bounds: [[54.4265, 7.99125], [57.778, 15.5995]],
  attribution: 'Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel, CC BY 4.0',
};
const PNG_1X1 = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg==', 'base64');
const WARNING = 'Protected ancient monument / protection zone nearby — check before building';
const EVIL = '<img src=x onerror="window.__heritageXss=1">';
const CREDIT = '© Slots- og Kulturstyrelsen';
const PANEL = '[data-testid="tp-panel"]';
const CHIP = '[data-testid="tp-heritage-chip"]';
const NOTE = '[data-testid="tp-heritage-note"]';
const VERDICT = '[data-testid="tp-verdict"]';

// Viborg heath, away from the fixture's nodes. IN is inside a zone, NEAR
// 42 m from one; FAR1 and FAR2 have nothing near.
const IN = [56.43, 9.40], NEAR = [56.44, 9.43], FAR1 = [56.50, 9.60], FAR2 = [56.51, 9.62];
const fmt = (ll) => ll[0].toFixed(5) + ',' + ll[1].toFixed(5);
const answerFor = {
  [fmt(IN)]: { radiusM: 100, count: 1, truncated: false, attribution: CREDIT,
    nearest: { type: 'Rundhøj', dating: 'Oldtid', status: 'Fredet', protectionNumber: '3624:15', systemNumber: '190001', distanceM: 0, insideZone: true } },
  [fmt(NEAR)]: { radiusM: 100, count: 1, truncated: false, attribution: CREDIT,
    nearest: { type: 'Vandfyldt grav', dating: 'Middelalder', status: 'Fredet', distanceM: 42, insideZone: false } },
};
const NONE = { radiusM: 100, count: 0, truncated: false, attribution: CREDIT, nearest: null };

/* surface: the terrain is clear and only the surface is marginal, so the
 * title row also holds the "Buildings/trees" chip (#729). surface 'wide':
 * the widest pair, "Terrain: Marginal" beside "Buildings/trees: Blocked"
 * (#796 F5). */
function profileFor(q, surface) {
  const D = 1800;
  const end = (side, h) => {
    const ll = (q.get(side) || '').split(',').map(Number);
    return { lat: ll[0], lon: ll[1], kind: 'point', groundM: 40,
      antennaM: q.get(h) !== null ? Number(q.get(h)) : 2,
      antennaSource: q.get(h) !== null ? 'request' : 'point-default', assumed: q.get(h) === null };
  };
  const samples = [];
  for (let i = 0; i < 40; i++) {
    const d = Math.round(D * i / 39);
    samples.push({ d, dtm: 30 + 8 * Math.sin(d / 200), dsm: 34 + 8 * Math.sin(d / 200) });
  }
  return {
    a: end('a', 'ha'), b: end('b', 'hb'),
    distanceM: D, stepM: 1.8, sampleCount: 1001, model: 'both',
    coverage: { state: 'full', pct: 100, firstGapM: null }, samples,
    physics: { freqMHz: 869.618, freqSource: 'firmware', lambdaM: 0.34474, k: 1.3333333333333333, kSource: 'default', f1MaxM: 12.5 },
    verdict: { state: surface && surface !== 'wide' ? 'clear' : 'fresnel60', model: 'dtm', criterion: 0.6, worstClearanceM: 3.2, worstAtM: 600, worstF1Fraction: 0.4,
      surface: { state: surface === 'wide' ? 'blocked' : 'fresnel60', worstClearanceM: surface === 'wide' ? -1.4 : 2.1, worstAtM: 600, worstF1Fraction: 0.3 } },
    attribution: DHM_BLOCK.attribution,
  };
}

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #723 M3 profile monument warning against ${BASE} ===\n`);

  /* o: viewport, touch, theme, heritage (false: no block), surface (see
   * profileFor), answer(key)
   * (a body, a number = that HTTP status, or 'hold' to answer when the test
   * calls rec.release()), evil (hostile type text). */
  async function newCtx(o) {
    o = o || {};
    const ctx = await browser.newContext({ viewport: o.viewport || { width: 1280, height: 800 }, hasTouch: !!o.touch, isMobile: !!o.touch });
    const rec = { heritage: [], profile: [], outside: [], agency: 0, consoleErrors: [], held: [] };
    rec.release = () => rec.held.splice(0).forEach(r => r());
    await ctx.route(BASE + '/api/config/client', async (route) => {
      const response = await route.fetch();
      const config = await response.json();
      config.dhm = DHM_BLOCK;
      if (o.heritage === false) delete config.heritage; else config.heritage = HERITAGE_BLOCK;
      delete config.masts;
      await route.fulfill({ response, json: config });
    });
    await ctx.route(url => new URL(url).pathname === PROFILE_PATH, async (route) => {
      const u = new URL(route.request().url());
      rec.profile.push(u);
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(profileFor(u.searchParams, o.surface)) });
    });
    await ctx.route(url => new URL(url).pathname === HERITAGE_PATH, async (route) => {
      const u = new URL(route.request().url());
      rec.heritage.push(u);
      const key = u.searchParams.get('lat') + ',' + u.searchParams.get('lon');
      let a = o.answer ? o.answer(key) : (answerFor[key] || NONE);
      if (a === 'hold') { await new Promise(r => rec.held.push(r)); a = answerFor[key] || NONE; }
      if (typeof a === 'number') {
        await route.fulfill({ status: a, contentType: 'application/json', headers: a === 429 ? { 'Retry-After': '30' } : {},
          body: JSON.stringify({ error: 'heritage data unavailable' }) });
        return;
      }
      if (o.evil && a.nearest) a = Object.assign({}, a, { nearest: Object.assign({}, a.nearest, { type: a.nearest.type + EVIL }) });
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(a) }).catch(() => {});
    });
    await ctx.route(url => new URL(url).pathname === DHM_BLOCK.url, route =>
      route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 }));
    // Every outside host: base-map tiles get a 1x1 PNG, anything else is
    // recorded and refused. Nothing leaves the machine.
    await ctx.route(/^https?:\/\/(?!localhost|127\.0\.0\.1)/i, async (route) => {
      const req = route.request();
      if (req.resourceType() === 'image') { await route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 }); return; }
      rec.outside.push(req.url());
      await route.abort();
    });
    // Registered last, so it runs first: the agency's host never sees a request.
    await ctx.route(/^https?:\/\/www\.kulturarv\.dk\//i, async (route) => {
      rec.agency++;
      await route.fulfill({ status: 200, contentType: 'image/png', headers: { 'Access-Control-Allow-Origin': '*' }, body: PNG_1X1 });
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
    return { ctx, page, rec };
  }

  async function openProfile(page, a, b) {
    await page.goto(`${BASE}/#/map?profile=${fmt(a)},${fmt(b)}`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#leaflet-map[data-loaded="true"]', { timeout: 30000 });
    await waitVerdict(page);
  }
  async function waitVerdict(page) {
    await page.waitForFunction((sel) => {
      const c = document.querySelector(sel);
      return !!(c && c.getAttribute('data-state') && c.getAttribute('data-state') !== 'pending');
    }, VERDICT, { timeout: 15000 });
  }
  async function waitLookups(rec, n) {
    const t0 = Date.now();
    while (rec.heritage.length < n && Date.now() - t0 < 10000) await new Promise(r => setTimeout(r, 50));
    await new Promise(r => setTimeout(r, 300));
  }
  async function state(page) {
    return page.evaluate(([chipSel, noteSel, verdictSel, panelSel]) => {
      const chip = document.querySelector(chipSel), note = document.querySelector(noteSel), v = document.querySelector(verdictSel);
      const panel = document.querySelector(panelSel);
      const vis = (el) => !!(el && !el.hidden && el.getClientRects().length);
      return {
        chip: vis(chip), note: vis(note),
        chipHidden: chip ? chip.hidden : null, noteHidden: note ? note.hidden : null,
        label: chip ? chip.textContent : null, title: chip ? chip.title : null,
        noteText: note ? note.textContent : null, noteHTML: note ? note.innerHTML : null,
        inHead: !!(chip && chip.closest('.tp-head')),
        // The chip and then its note, both under the detail line.
        afterDetail: (() => {
          const d = document.querySelector('[data-testid="tp-detail"]');
          const after = (x, y) => !!(x && y && (x.compareDocumentPosition(y) & Node.DOCUMENT_POSITION_FOLLOWING));
          return after(d, chip) && after(chip, note);
        })(),
        chipClass: chip ? chip.className : '',
        verdict: v ? { text: v.textContent, state: v.getAttribute('data-state'), cls: v.className, title: v.title } : null,
        panelText: panel ? Array.from(panel.querySelectorAll('*')).filter(e => !e.hidden && e.getClientRects().length && !e.children.length).map(e => e.textContent).join(' | ') : '',
        imgs: panel ? panel.querySelectorAll('img').length : -1,
        xss: window.__heritageXss === 1,
      };
    }, [CHIP, NOTE, VERDICT, PANEL]);
  }
  async function waitChip(page, want) {
    await page.waitForFunction(([sel, w]) => {
      const c = document.querySelector(sel);
      return !!(c && !c.hidden && c.textContent === w);
    }, [CHIP, want], { timeout: 10000 });
  }
  async function shot(page, name) {
    if (!SHOT_DIR) return;
    fs.mkdirSync(SHOT_DIR, { recursive: true });
    await page.screenshot({ path: path.join(SHOT_DIR, name), type: 'jpeg', quality: 60 });
  }
  function checkClean(rec) {
    assert(rec.agency === 0, 'requests to the agency: ' + rec.agency);
    assert(rec.outside.length === 0, 'requests to outside hosts: ' + rec.outside.join(' '));
    assert(rec.consoleErrors.length === 0, 'console errors: ' + rec.consoleErrors.join(' | '));
  }
  function checkLookups(rec, ends) {
    const got = rec.heritage.map(u => {
      assert(/^\?lat=-?\d{1,3}\.\d{5}&lon=-?\d{1,3}\.\d{5}$/.test(u.search), 'lookup query ' + u.search);
      return u.searchParams.get('lat') + ',' + u.searchParams.get('lon');
    });
    assert(JSON.stringify(got) === JSON.stringify(ends.map(fmt)), 'lookups ' + got.join(' ; ') + ' want ' + ends.map(fmt).join(' ; '));
  }

  // The verdict as it reads with no heritage block: what the note must
  // leave exactly as it is.
  const baseline = {};
  {
    const { ctx, page, rec } = await newCtx({ heritage: false });
    await step('without the heritage block: no lookup, no chip, no note', async () => {
      for (const [a, b] of [[IN, FAR1], [FAR1, NEAR], [IN, NEAR]]) {
        await openProfile(page, a, b);
        await page.waitForTimeout(500);
        const s = await state(page);
        baseline[fmt(a) + ';' + fmt(b)] = s.verdict;
        assert(!s.chip && !s.note, 'shown without the block');
      }
      assert(rec.heritage.length === 0, 'lookups without the block: ' + rec.heritage.length);
      checkClean(rec);
    });
    await ctx.close();
  }

  // ── 1. A, B, both ──────────────────────────────────────────────────────
  for (const c of [
    { name: 'A inside a zone', a: IN, b: FAR1, label: 'In monument zone', lines: ['A: Rundhøj, inside its protection zone.'], not: /B:/ },
    { name: 'B 42 m from a zone', a: FAR1, b: NEAR, label: 'Monument zone ~42 m', lines: ['B: Vandfyldt grav, about 42 m from its protection zone.'], not: /A:/ },
    { name: 'both ends', a: IN, b: NEAR, label: 'In monument zone',
      lines: ['A: Rundhøj, inside its protection zone.', 'B: Vandfyldt grav, about 42 m from its protection zone.'], not: null },
  ]) {
    const { ctx, page, rec } = await newCtx();
    await step(`${c.name}: the warn chip and the note with type and distance under the detail; the verdict unchanged`, async () => {
      await openProfile(page, c.a, c.b);
      await waitChip(page, c.label);
      const s = await state(page);
      assert(s.chip && s.note, 'chip ' + s.chip + ' note ' + s.note);
      assert(!s.inHead && s.afterDetail, 'placement: inHead ' + s.inHead + ' afterDetail ' + s.afterDetail);
      assert(/tp-chip-warn/.test(s.chipClass), 'chip class ' + s.chipClass);
      assert(s.noteText.indexOf(WARNING) === 0, 'note does not lead with the warning: ' + s.noteText);
      c.lines.forEach(l => assert(s.noteText.indexOf(l) > 0 && s.title.indexOf(l) > 0, 'missing "' + l + '" in ' + s.noteText));
      if (c.not) assert(!c.not.test(s.noteText), 'note names the other end: ' + s.noteText);
      assert(s.noteText.indexOf(CREDIT) > 0, 'no credit: ' + s.noteText);
      assert(!/may build|allowed|permitted|dispensation/i.test(s.panelText), 'permission or a legal claim: ' + s.panelText);
      const want = baseline[fmt(c.a) + ';' + fmt(c.b)];
      assert(want && JSON.stringify(s.verdict) === JSON.stringify(want), 'verdict ' + JSON.stringify(s.verdict) + ' vs ' + JSON.stringify(want));
      assert(rec.profile.length === 1, 'profile requests ' + rec.profile.length);
      checkLookups(rec, [c.a, c.b]);
      checkClean(rec);
      await shot(page, 'm3-' + c.name.replace(/\W+/g, '-') + '.jpg');
    });
    if (c.name === 'both ends') {
      await step('both ends: a height edit re-measures without a new lookup; Close takes the note away', async () => {
        await page.click('[data-testid="tp-h-a-plus"]');
        const t0 = Date.now();
        while (rec.profile.length < 2 && Date.now() - t0 < 5000) await page.waitForTimeout(50);
        await waitVerdict(page);
        await waitChip(page, 'In monument zone');
        assert(rec.profile.length === 2, 'profile requests ' + rec.profile.length);
        assert(rec.heritage.length === 2, 'a height edit looked up again: ' + rec.heritage.length);
        await page.click('[data-testid="tp-close"]');
        await page.waitForTimeout(300);
        const s = await state(page);
        assert(s.chipHidden === true && s.noteHidden === true && !s.label && !s.noteHTML, 'note left after Close: ' + JSON.stringify(s));
        // The next profile, with nothing near, shows nothing old.
        await page.evaluate((h) => { location.hash = h; }, `#/map?profile=${fmt(FAR1)},${fmt(FAR2)}`);
        await waitLookups(rec, 4);
        await waitVerdict(page);
        await page.waitForTimeout(300);
        const t = await state(page);
        assert(!t.chip && !t.note && !t.label, 'the old note came back: ' + JSON.stringify(t));
        checkClean(rec);
      });
    }
    await ctx.close();
  }

  // ── 2. Silence ─────────────────────────────────────────────────────────
  for (const c of [
    { name: 'nothing near either end', answer: () => NONE },
    { name: 'the lookups fail (502)', answer: () => 502 },
    { name: 'the lookups are refused (429)', answer: () => 429 },
  ]) {
    const { ctx, page, rec } = await newCtx({ answer: c.answer });
    await step(`${c.name}: no chip, no note, no all-clear; the verdict unchanged`, async () => {
      await openProfile(page, IN, NEAR);
      await waitLookups(rec, 2);
      const s = await state(page);
      assert(!s.chip && !s.note && !s.label && !s.noteHTML, 'shown: ' + JSON.stringify(s));
      assert(!/monument|heritage|protection zone/i.test(s.panelText), 'panel mentions monuments: ' + s.panelText);
      assert(JSON.stringify(s.verdict) === JSON.stringify(baseline[fmt(IN) + ';' + fmt(NEAR)]), 'verdict ' + JSON.stringify(s.verdict));
      checkLookups(rec, [IN, NEAR]);
      // A refused lookup is a failed request, which the page logs; nothing
      // else may be.
      rec.consoleErrors = rec.consoleErrors.filter(e => !/status of (502|429)/.test(e));
      checkClean(rec);
    });
    await ctx.close();
  }

  // ── 5. Hostile dataset text ────────────────────────────────────────────
  {
    const { ctx, page, rec } = await newCtx({ evil: true });
    await step('hostile dataset text is shown as text and never runs', async () => {
      await openProfile(page, IN, NEAR);
      await page.waitForFunction((sel) => { const c = document.querySelector(sel); return !!(c && !c.hidden); }, CHIP, { timeout: 10000 });
      await page.waitForTimeout(300);
      const s = await state(page);
      assert(!s.xss && s.imgs === 0, 'hostile text became markup');
      assert(s.noteText.indexOf('Rundhøj' + EVIL) > 0, 'hostile text not shown as text: ' + s.noteText);
      checkClean(rec);
    });
    await ctx.close();
  }

  // ── 6. Short landscape ─────────────────────────────────────────────────
  for (const vp of [{ width: 740, height: 360 }, { width: 568, height: 320 }, { width: 844, height: 390 }]) {
    const { ctx, page, rec } = await newCtx({ viewport: vp, touch: true, answer: (k) => (answerFor[k] ? 'hold' : NONE) });
    await step(`${vp.width}x${vp.height} landscape: the note does not move the chart, which stays on screen`, async () => {
      await openProfile(page, IN, NEAR);
      await waitLookups(rec, 2);
      const box = () => page.evaluate(([chartSel, bodySel, closeSel]) => {
        const c = document.querySelector(chartSel).getBoundingClientRect();
        const body = document.querySelector(bodySel);
        const b = body.getBoundingClientRect();
        const x = document.querySelector(closeSel).getBoundingClientRect();
        // On screen means hit-testable: nothing (the title row, the bottom
        // navigation) covers the chart's top or bottom edge.
        const hits = (y) => { const t = document.elementFromPoint(c.left + c.width / 2, y); return !!(t && t.closest(chartSel)); };
        return { top: Math.round(c.top), bottom: Math.round(c.bottom), h: Math.round(c.height),
          visible: c.height > 0 && hits(c.top + 2) && hits(c.bottom - 2),
          bodyTop: Math.round(b.top), bodyBottom: Math.round(b.bottom), scrollTop: body.scrollTop,
          vh: window.innerHeight, vw: window.innerWidth, sw: document.documentElement.scrollWidth,
          closeW: Math.round(x.width), closeH: Math.round(x.height) };
      }, ['[data-testid="tp-chart"]', '[data-testid="tp-body"]', '[data-testid="tp-close"]']);
      const before = await box();
      assert((await state(page)).chip === false, 'note shown before its lookups answered');
      rec.release();
      await page.waitForFunction((sel) => { const c = document.querySelector(sel); return !!(c && !c.hidden); }, CHIP, { timeout: 10000 });
      await page.waitForTimeout(200);
      const after = await box();
      assert(Math.abs(after.top - before.top) <= 1 && Math.abs(after.bottom - before.bottom) <= 1,
        'the chart moved: ' + JSON.stringify({ before, after }));
      assert(after.scrollTop === 0, 'the panel body scrolled: ' + after.scrollTop);
      assert(before.visible, 'the chart was not fully on screen even before the note: ' + JSON.stringify(before));
      assert(after.visible && after.top >= after.bodyTop - 1 && after.bottom <= after.bodyBottom + 1 && after.bottom <= after.vh + 1,
        'the chart is not fully on screen: ' + JSON.stringify(after));
      await shot(page, `m3-landscape-${vp.width}x${vp.height}.jpg`);
      assert(after.closeW >= 48 && after.closeH >= 48, 'close is ' + after.closeW + 'x' + after.closeH);
      assert(after.sw <= after.vw, 'horizontal overflow: ' + JSON.stringify(after));
      // The note itself can be scrolled to.
      const noteInView = await page.evaluate((sel) => {
        const e = document.querySelector(sel); e.scrollIntoView({ block: 'center' });
        const r = e.getBoundingClientRect(); return r.height > 0 && r.top >= 0 && r.bottom <= window.innerHeight + 1;
      }, NOTE);
      assert(noteInView, 'the note cannot be brought on screen');
      checkClean(rec);
    });
    await ctx.close();
  }

  // ── 6b. Short landscape, every chip showing ───────────────────────────
  /* The review of #784 found that with the surface chip and "assumed
   * heights" also showing, a monument chip in the title row wrapped Close
   * (and at 667 wide Copy too) onto a second row, which pushed the chart
   * under the bottom navigation at 667x320-340. Here the title row, the
   * buttons on it and the chart must stay exactly where they were. */
  for (const vp of [{ width: 640, height: 360 }, { width: 667, height: 375 }, { width: 667, height: 340 },
    { width: 667, height: 330 }, { width: 667, height: 320 }]) {
    const { ctx, page, rec } = await newCtx({ viewport: vp, touch: true, surface: true, answer: (k) => (answerFor[k] ? 'hold' : NONE) });
    await step(`${vp.width}x${vp.height} landscape, every chip showing: the title row keeps its height and buttons, the chart stays on screen, the hover cursor still works`, async () => {
      await openProfile(page, IN, NEAR);
      await waitLookups(rec, 2);
      const geo = () => page.evaluate(([chartSel, bodySel, panelSel, chipSel]) => {
        const r = (el) => { const b = el.getBoundingClientRect(); return { top: Math.round(b.top), bottom: Math.round(b.bottom), h: Math.round(b.height) }; };
        const panel = document.querySelector(panelSel);
        const head = panel.querySelector('.tp-head'), title = head.querySelector('.tp-title');
        const chart = document.querySelector(chartSel), c = chart.getBoundingClientRect();
        const body = document.querySelector(bodySel);
        const shown = (sel) => { const e = document.querySelector(sel); return !!(e && !e.hidden && e.getClientRects().length); };
        // On the title row: the button overlaps the title vertically.
        const onTitleRow = (sel) => { const b = document.querySelector(sel).getBoundingClientRect(), t = title.getBoundingClientRect();
          return b.top < t.bottom && b.bottom > t.top; };
        const hits = (y) => { const t = document.elementFromPoint(c.left + c.width / 2, y); return !!(t && t.closest(chartSel)); };
        return {
          head: r(head), chart: r(chart), body: r(body), scrollTop: body.scrollTop,
          visible: c.height > 0 && hits(c.top + 2) && hits(c.bottom - 2),
          chips: { verdict: shown('[data-testid="tp-verdict"]'), surface: shown('[data-testid="tp-surface-chip"]'),
            assumed: shown('[data-testid="tp-assumed-chip"]'), heritage: shown(chipSel) },
          copyOnRow: onTitleRow('[data-testid="tp-copy-link"]'), closeOnRow: onTitleRow('[data-testid="tp-close"]'),
          vh: window.innerHeight, vw: window.innerWidth, sw: document.documentElement.scrollWidth,
        };
      }, ['[data-testid="tp-chart"]', '[data-testid="tp-body"]', PANEL, CHIP]);
      const before = await geo();
      assert(before.chips.verdict && before.chips.surface && before.chips.assumed && !before.chips.heritage,
        'chips before the answers: ' + JSON.stringify(before.chips));
      assert(before.visible && before.copyOnRow && before.closeOnRow, 'not a clean start: ' + JSON.stringify(before));
      rec.release();
      await waitChip(page, 'In monument zone');
      await page.waitForTimeout(200);
      const after = await geo();
      const s = await state(page);
      assert(s.note, 'no note');
      assert(after.chips.verdict && after.chips.surface && after.chips.assumed && after.chips.heritage,
        'not every chip shows: ' + JSON.stringify(after.chips));
      assert(Math.abs(after.head.h - before.head.h) <= 1, 'the title row grew ' + before.head.h + ' → ' + after.head.h + ' px');
      assert(after.copyOnRow && after.closeOnRow, 'Copy/Close left the title row: ' + JSON.stringify(after));
      assert(Math.abs(after.chart.top - before.chart.top) <= 1 && Math.abs(after.chart.bottom - before.chart.bottom) <= 1,
        'the chart moved: ' + JSON.stringify({ before: before.chart, after: after.chart }));
      assert(after.scrollTop === 0, 'the panel body scrolled: ' + after.scrollTop);
      assert(after.visible && after.chart.top >= after.body.top - 1 && after.chart.bottom <= after.body.bottom + 1 && after.chart.bottom <= after.vh + 1,
        'the chart is not fully on screen: ' + JSON.stringify(after));
      assert(after.sw <= after.vw, 'horizontal overflow: ' + JSON.stringify(after));
      await shot(page, `m3-landscape-chips-${vp.width}x${vp.height}.jpg`);
      // #781: a touch on the chart still draws the cursor line and puts one
      // dot on the map's profile line.
      const box = await page.locator('[data-testid="tp-chart"] .tp-plot svg').boundingBox();
      const cdp = await ctx.newCDPSession(page);
      const at = { x: box.x + box.width * 0.4, y: box.y + box.height / 2 };
      await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [at] });
      await cdp.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [{ x: at.x + box.width * 0.2, y: at.y }] });
      await page.waitForTimeout(80);
      await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
      await page.waitForTimeout(150);
      const cur = await page.evaluate(() => {
        const line = document.querySelector('[data-testid="tp-cursor-line"]');
        return { line: !!line && line.getAttribute('visibility') === 'visible',
          dots: document.querySelectorAll('.leaflet-overlay-pane .tp-cursor-dot').length };
      });
      assert(cur.line && cur.dots === 1, 'hover cursor after a touch: ' + JSON.stringify(cur));
      checkClean(rec);
    });
    await ctx.close();
  }

  // ── 6c. Short landscape, the widest chips ─────────────────────────────
  /* #796 F5: with "Terrain: Marginal", "Buildings/trees: Blocked" and
   * "assumed heights" the title row broke at 640 px and Close went onto a
   * line of its own, which put the chart under the bottom navigation at
   * 640x340 and 640x320. The checks are geometric (same top, chart bottom
   * above the nav top), so a wider or narrower font cannot hide a break. */
  /* 568x320 is where the chips cannot share one line in any font, so the
   * chip row has to wrap there. */
  for (const vp of [{ width: 568, height: 320 }, { width: 640, height: 360 }, { width: 640, height: 340 }, { width: 640, height: 320 }, { width: 667, height: 320 }]) {
    const { ctx, page, rec } = await newCtx({ viewport: vp, touch: true, surface: 'wide', heritage: false });
    await step(`${vp.width}x${vp.height} landscape, the widest chips: Copy and Close stay on the title row, the chart ends above the bottom nav`, async () => {
      await openProfile(page, FAR1, FAR2);
      await page.waitForSelector('[data-testid="tp-assumed-chip"]:not([hidden])', { timeout: 5000 });
      await page.waitForTimeout(200);
      const g = await page.evaluate(() => {
        const q = (s) => document.querySelector(s);
        const top = (e) => Math.round(e.getBoundingClientRect().top);
        const title = q('.tp-title'), chart = q('[data-testid="tp-chart"]'), nav = q('.bottom-nav');
        const c = chart.getBoundingClientRect();
        const hits = (y) => { const t = document.elementFromPoint(c.left + c.width / 2, y); return !!(t && t.closest('[data-testid="tp-chart"]')); };
        return {
          verdict: q('[data-testid="tp-verdict"]').textContent, surface: q('[data-testid="tp-surface-chip"]').textContent,
          assumed: !q('[data-testid="tp-assumed-chip"]').hidden,
          titleTop: top(title), copyTop: top(q('[data-testid="tp-copy-link"]')), closeTop: top(q('[data-testid="tp-close"]')),
          // Copy and Close are on screen and take a tap at their centre.
          buttonsTappable: ['[data-testid="tp-copy-link"]', '[data-testid="tp-close"]'].every((sel) => {
            const e = q(sel), b = e.getBoundingClientRect();
            const hit = document.elementFromPoint(b.left + b.width / 2, b.top + b.height / 2);
            return b.left >= 0 && b.right <= window.innerWidth && !!(hit && e.contains(hit));
          }),
          // Every chip ends before Copy begins and after the title ends: none
          // runs in under the buttons or the title.
          chipsClear: (() => {
            const copyL = q('[data-testid="tp-copy-link"]').getBoundingClientRect().left, titleR = title.getBoundingClientRect().right;
            return Array.from(document.querySelectorAll('.tp-head .tp-chip')).filter(e => e.getClientRects().length)
              .every(e => { const b = e.getBoundingClientRect(); return b.right <= copyL + 0.5 && b.left >= titleR - 0.5; });
          })(),
          chartBottom: Math.round(c.bottom), navTop: nav && nav.getClientRects().length ? Math.round(nav.getBoundingClientRect().top) : null,
          visible: c.height > 0 && hits(c.top + 2) && hits(c.bottom - 2),
          vw: window.innerWidth, sw: document.documentElement.scrollWidth,
        };
      });
      assert(/Marginal/.test(g.verdict) && /Blocked/.test(g.surface) && g.assumed, 'not the widest chips: ' + JSON.stringify(g));
      assert(g.navTop !== null, 'no bottom navigation at ' + vp.width + 'x' + vp.height);
      assert(Math.abs(g.copyTop - g.titleTop) <= 1 && Math.abs(g.closeTop - g.titleTop) <= 1,
        'the title row broke (title/copy/close top ' + g.titleTop + '/' + g.copyTop + '/' + g.closeTop + ')');
      assert(g.buttonsTappable, 'Copy or Close is off screen or covered: ' + JSON.stringify(g));
      assert(g.chipsClear, 'a chip runs in under the title or Copy: ' + JSON.stringify(g));
      assert(g.chartBottom <= g.navTop && g.visible, 'the chart ends under the bottom nav: ' + JSON.stringify(g));
      assert(g.sw <= g.vw, 'horizontal overflow: ' + JSON.stringify(g));
      await shot(page, `m3-landscape-wide-${vp.width}x${vp.height}.jpg`);
      checkClean(rec);
    });
    await ctx.close();
  }

  // ── 7. Theme ───────────────────────────────────────────────────────────
  for (const theme of ['light', 'dark']) {
    const { ctx, page, rec } = await newCtx({ theme });
    await step(`${theme}: the chip and the note border are the theme's --warning`, async () => {
      await openProfile(page, IN, NEAR);
      await waitChip(page, 'In monument zone');
      const c = await page.evaluate(([chipSel, noteSel]) => {
        const probe = document.createElement('span');
        probe.style.color = 'var(--warning)';
        document.body.appendChild(probe);
        const want = getComputedStyle(probe).color;
        probe.remove();
        const chip = getComputedStyle(document.querySelector(chipSel)), note = getComputedStyle(document.querySelector(noteSel));
        return { want, color: chip.color, border: chip.borderTopColor, noteBorder: note.borderLeftColor };
      }, [CHIP, NOTE]);
      assert(c.color === c.want && c.border === c.want && c.noteBorder === c.want, 'colours ' + JSON.stringify(c));
      checkClean(rec);
      await shot(page, `m3-${theme}.jpg`);
    });
    await ctx.close();
  }

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
