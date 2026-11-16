/**
 * E2E (#723 M4): the ancient monuments settings in the customizer, and their
 * effect on the Map page's monuments overlay and Path profile warning.
 *
 * The CI server has neither map.dhm nor map.heritage, so /api/config/client
 * is intercepted and given both blocks, /api/terrain/profile is answered
 * with a synthetic profile, /api/terrain/heritage with fixed synthetic
 * answers per end (A inside a zone, B 42 m from one), and every outside host
 * (the agency's WMS among them) with a 1x1 PNG. Neither the agency nor any
 * DHM service is ever contacted.
 *
 * Asserts:
 *   1. the Display tab has an "Ancient Monuments" section with the four
 *      settings at their defaults (overlay off, 85 %, warning on, distance
 *      empty), bounded like the rule (10-100 %, 0-250 m), 48 px rows;
 *   2. the overlay's opacity follows the setting at once (0.85 → 0.4);
 *   3. an open profile follows the warning distance and the warning switch
 *      without a reload and without a single new lookup; the verdict chip
 *      never changes;
 *   4. out-of-range values are put back and never stored; a value back at
 *      its default is a cleared override;
 *   5. round trip: the settings survive a reload and reach the export, with
 *      no server-only map.heritage key in it;
 *   6. Reset All brings the defaults back, live;
 *   7. "on by default": the next map starts with the overlay on, turning it
 *      off writes heritage=0 into the link, and heritage=0 in a link wins;
 *   8. a phone, cold start at 375x560 with touch: More → Customize →
 *      Display, each of the four settings changed by real taps (and typing),
 *      rows inside the screen; the overlay then starts on, at the new
 *      opacity;
 *   9. without the heritage block the section is absent;
 *  10. no console errors.
 *
 * Usage:
 *   BASE_URL=http://localhost:13581 node test-issue-723-m4-heritage-customizer-e2e.js
 * Optional: SCREENSHOT_DIR=<dir> writes small JPG screenshots.
 */
'use strict';
const { chromium } = require('playwright');
const path = require('path');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const SHOT_DIR = process.env.SCREENSHOT_DIR || '';
const PROFILE_PATH = '/api/terrain/profile';
const HERITAGE_PATH = '/api/terrain/heritage';
const ATTRIBUTION = 'Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel, CC BY 4.0';
const DHM_BLOCK = {
  enabled: true, url: '/api/map/dhm/wms', layer: 'dhm_terraen_skyggekort', minZoom: 7, maxZoom: 19, profile: true,
  bounds: [[54.4265, 7.99125], [57.778, 15.5995]], attribution: ATTRIBUTION,
};
const HERITAGE_BLOCK = {
  enabled: true, url: 'https://www.kulturarv.dk/ffgeoserver/public/wms',
  layers: 'fundogfortidsminder_areal_beskyttelse,fundogfortidsminder_punkt_fredet',
  minZoom: 12, maxZoom: 19, bounds: [[54.4646, 8.0899], [57.7289, 15.5689]],
  attribution: '© Slots- og Kulturstyrelsen',
};
const INSIDE = {
  radiusM: 100, count: 1, truncated: false, attribution: '© Slots- og Kulturstyrelsen',
  nearest: { type: 'Rundhøj', dating: 'Oldtid', status: 'Fredet', distanceM: 0, insideZone: true },
};
const NEAR = {
  radiusM: 100, count: 1, truncated: false, attribution: '© Slots- og Kulturstyrelsen',
  nearest: { type: 'Vandfyldt grav', dating: 'Middelalder', status: 'Fredet', distanceM: 42, insideZone: false },
};
const PNG_1X1 = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg==', 'base64');
const STORE = 'cs-theme-overrides';
const LAYER_LABEL = 'Protected ancient monuments';
const CHIP = '[data-testid="tp-heritage-chip"]';
const NOTE = '[data-testid="tp-heritage-note"]';
const VERDICT = '[data-testid="tp-verdict"]';

// Viborg heath: A and B a profile apart, both in Denmark.
const A = [56.43, 9.40], B = [56.44, 9.43];
const fmt = (ll) => ll[0].toFixed(5) + ',' + ll[1].toFixed(5);
const MAP_Z13 = `#/map?lat=${A[0]}&lon=${A[1]}&zoom=13`;

function profileFor(q) {
  const D = 1800;
  const end = (side) => {
    const ll = (q.get(side) || '').split(',').map(Number);
    return { lat: ll[0], lon: ll[1], kind: 'point', groundM: 40, antennaM: 2, antennaSource: 'point-default', assumed: true };
  };
  const samples = [];
  for (let i = 0; i < 40; i++) {
    const d = Math.round(D * i / 39);
    samples.push({ d, dtm: 30 + 8 * Math.sin(d / 200) });
  }
  return {
    a: end('a'), b: end('b'), distanceM: D, stepM: 1.8, sampleCount: 1001, model: 'dtm',
    coverage: { state: 'full', pct: 100, firstGapM: null }, samples,
    physics: { freqMHz: 869.618, freqSource: 'firmware', lambdaM: 0.34474, k: 1.3333333333333333, kSource: 'default', f1MaxM: 12.5 },
    verdict: { state: 'clear', model: 'dtm', criterion: 0.6, worstClearanceM: 21.2, worstAtM: 600, worstF1Fraction: 2.1 },
    attribution: ATTRIBUTION,
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
  console.log(`\n=== #723 M4 customizer ancient monuments settings against ${BASE} ===\n`);

  async function newCtx(o) {
    o = o || {};
    const ctx = await browser.newContext(Object.assign(
      { viewport: o.viewport || { width: 1280, height: 800 } },
      o.phone ? { hasTouch: true, isMobile: true, deviceScaleFactor: 2 } : {}));
    const rec = { heritage: [], profile: [], consoleErrors: [] };
    await ctx.route(BASE + '/api/config/client', async (route) => {
      const response = await route.fetch();
      const config = await response.json();
      config.dhm = DHM_BLOCK;
      if (o.heritage !== false) config.heritage = HERITAGE_BLOCK;
      await route.fulfill({ response, json: config });
    });
    await ctx.route(url => new URL(url).pathname === PROFILE_PATH, async (route) => {
      const u = new URL(route.request().url());
      rec.profile.push(u.search);
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(profileFor(u.searchParams)) });
    });
    await ctx.route(url => new URL(url).pathname === HERITAGE_PATH, async (route) => {
      const u = new URL(route.request().url());
      rec.heritage.push(u.search);
      const lat = Number(u.searchParams.get('lat'));
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(Math.abs(lat - A[0]) < 0.005 ? INSIDE : NEAR) });
    });
    await ctx.route(/^https?:\/\/(?!localhost|127\.0\.0\.1)/i, async (route) => {
      await route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 });
    });
    await ctx.route(url => new URL(url).pathname === '/api/map/dhm/wms', async (route) => {
      await route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 });
    });
    await ctx.addInitScript((init) => {
      try {
        if (sessionStorage.getItem('__m4init')) return;   // once per context, not on every reload
        sessionStorage.setItem('__m4init', '1');
        localStorage.setItem('meshcore-theme', 'light');
        localStorage.setItem('meshcore-map-clustering', 'false');
        ['row-swipe', 'tab-swipe', 'edge-drawer', 'pull-refresh']
          .forEach(k => localStorage.setItem('meshcore-gesture-hints-' + k, 'seen'));
        Object.keys(init || {}).forEach(k => localStorage.setItem(k, init[k]));
      } catch (_) {}
    }, o.storage || {});
    const page = await ctx.newPage();
    page.on('console', m => { if (m.type() === 'error') rec.consoleErrors.push(m.text()); });
    page.on('pageerror', e => rec.consoleErrors.push(String(e)));
    return { ctx, page, rec };
  }

  async function ready(page) {
    await page.waitForSelector('#leaflet-map[data-loaded="true"]', { timeout: 30000 });
    await page.waitForFunction(() => window._customizerV2 && window._customizerV2.initDone, null, { timeout: 15000 });
  }

  async function gotoMap(page, hash) {
    await page.goto(`${BASE}/${hash || MAP_Z13}`, { waitUntil: 'domcontentloaded' });
    await ready(page);
  }

  async function openProfile(page) {
    await gotoMap(page, `#/map?profile=${fmt(A)},${fmt(B)}`);
    await page.waitForFunction((sel) => {
      const c = document.querySelector(sel);
      return !!(c && c.getAttribute('data-state') && c.getAttribute('data-state') !== 'pending');
    }, VERDICT, { timeout: 15000 });
  }

  // The palette button, or on a phone the bottom nav's More sheet, then
  // Customize, then Display: what a viewer taps.
  async function openDisplay(page, phone) {
    if (!(await page.$('.cust-overlay:not(.hidden)'))) {
      if (phone) {
        await page.tap('[data-bottom-nav-tab="more"]');
        await page.waitForSelector('[data-mpa-mirror="customizeToggle"]', { state: 'visible', timeout: 5000 });
        await page.tap('[data-mpa-mirror="customizeToggle"]');
      } else {
        await page.click('#customizeToggle');
      }
      await page.waitForSelector('.cust-overlay:not(.hidden)', { timeout: 5000 });
    }
    if (phone) await page.tap('.cust-tab[data-tab="display"]');
    else await page.click('.cust-tab[data-tab="display"]');
    await page.waitForSelector('.cust-panel[data-panel="display"].active', { timeout: 4000 });
  }

  const sel = (key) => `[data-cv2-heritage="${key}"]`;

  async function setNum(page, key, value) {
    await page.fill(sel(key), String(value));
    await page.dispatchEvent(sel(key), 'change');
    await page.waitForTimeout(600); // the customizer's debounced write
  }

  async function clickCheck(page, key) {
    await page.click(`label[for="cv2-heritage-${key}"]`);
    await page.waitForTimeout(600);
  }

  /* An open Path profile panel sits above the customizer's lower part
   * (z-index 1200 over 1050, see the #727 M4 E2E), so with a profile open
   * the switch is reached the way a keyboard user does: Tab to it, Space.
   * Mouse and tap are covered below with no profile open. */
  async function toggleByKeyboard(page, key, want) {
    await page.focus(sel(key));
    if ((await page.isChecked(sel(key))) !== want) await page.keyboard.press('Space');
    await page.waitForTimeout(600);
    assert((await page.isChecked(sel(key))) === want, key + ' did not follow Space');
  }

  const storedHeritage = (page) => page.evaluate((k) => {
    try { return (JSON.parse(localStorage.getItem(k) || '{}').heritage) || null; } catch (_) { return 'unparsable'; }
  }, STORE);

  const overlay = (page) => page.evaluate((label) => {
    const lab = Array.from(document.querySelectorAll('.leaflet-control-layers-overlays label')).find((l) => l.textContent.indexOf(label) >= 0);
    const box = lab && lab.querySelector('input');
    const layer = document.querySelector('.mc-heritage-pane .leaflet-layer');
    return { offered: !!lab, on: box ? box.checked : null, opacity: layer ? Number(getComputedStyle(layer).opacity) : null };
  }, LAYER_LABEL);

  async function waitOpacity(page, want) {
    await page.waitForFunction((w) => {
      const l = document.querySelector('.mc-heritage-pane .leaflet-layer');
      return !!l && Math.abs(Number(getComputedStyle(l).opacity) - w) < 0.001;
    }, want, { timeout: 8000 });
  }

  const panel = (page) => page.evaluate(([c, n, v]) => {
    const chip = document.querySelector(c), note = document.querySelector(n), verdict = document.querySelector(v);
    return {
      chip: chip && !chip.hidden ? chip.textContent : null,
      title: chip ? chip.title : '',
      note: note && !note.hidden ? note.textContent : null,
      verdict: verdict ? verdict.textContent + '|' + verdict.className + '|' + verdict.getAttribute('data-state') : null,
    };
  }, [CHIP, NOTE, VERDICT]);

  // ── 1-6. Desktop: the section, live effects, bounds, round trip, reset ──
  {
    const { ctx, page, rec } = await newCtx({ storage: { 'mc-heritage': '1' } });

    await step('the Display tab has the four monuments settings at their defaults', async () => {
      await openProfile(page);
      await openDisplay(page);
      const f = await page.evaluate(() => {
        const one = (k) => {
          const el = document.querySelector('[data-cv2-heritage="' + k + '"]');
          if (!el) return null;
          const lab = document.querySelector('label[for="' + el.id + '"]');
          return { type: el.type, value: el.value, checked: el.checked, min: el.min, max: el.max, placeholder: el.placeholder,
            h: el.getBoundingClientRect().height, labelH: lab ? lab.getBoundingClientRect().height : 0 };
        };
        const title = Array.from(document.querySelectorAll('.cust-section-title')).some((p) => p.textContent === 'Ancient Monuments');
        return { title, enabled: one('enabled'), opacity: one('opacityPct'), warn: one('profileWarning'), dist: one('warnDistanceM') };
      });
      assert(f.title, 'no Ancient Monuments section');
      assert(f.enabled && f.enabled.type === 'checkbox' && !f.enabled.checked && f.enabled.labelH >= 48, 'enabled ' + JSON.stringify(f.enabled));
      assert(f.opacity && f.opacity.value === '85' && f.opacity.min === '10' && f.opacity.max === '100' && f.opacity.h >= 48, 'opacity ' + JSON.stringify(f.opacity));
      assert(f.warn && f.warn.type === 'checkbox' && f.warn.checked && f.warn.labelH >= 48, 'warning ' + JSON.stringify(f.warn));
      assert(f.dist && f.dist.value === '' && f.dist.min === '0' && f.dist.max === '250' && f.dist.placeholder && f.dist.h >= 48, 'distance ' + JSON.stringify(f.dist));
    });

    await step('an open profile follows the warning distance and switch, with no new lookup', async () => {
      await page.waitForFunction((n) => { const e = document.querySelector(n); return !!(e && !e.hidden && /B: Vandfyldt grav/.test(e.textContent)); }, NOTE, { timeout: 10000 });
      const before = await panel(page);
      assert(before.chip === 'In monument zone', 'chip ' + before.chip);
      const lookups = rec.heritage.length, profiles = rec.profile.length;
      assert(lookups === 2, 'lookups ' + rec.heritage);
      await setNum(page, 'warnDistanceM', 30);
      await page.waitForFunction((n) => { const e = document.querySelector(n); return !!(e && !e.hidden && !/B:/.test(e.textContent) && /A: Rundhøj/.test(e.textContent)); }, NOTE, { timeout: 8000 });
      await toggleByKeyboard(page, 'profileWarning', false);
      await page.waitForFunction(([c, n]) => { const a = document.querySelector(c), b = document.querySelector(n); return a.hidden && b.hidden; }, [CHIP, NOTE], { timeout: 8000 });
      await toggleByKeyboard(page, 'profileWarning', true);
      await page.waitForFunction((c) => !document.querySelector(c).hidden, CHIP, { timeout: 8000 });
      await setNum(page, 'warnDistanceM', 42);
      await page.waitForFunction((n) => /B: Vandfyldt grav, about 42 m/.test(document.querySelector(n).textContent), NOTE, { timeout: 8000 });
      assert(rec.heritage.length === lookups, 'asked again: ' + rec.heritage.slice(lookups));
      assert(rec.profile.length === profiles, 're-measured: ' + rec.profile.slice(profiles));
      assert((await panel(page)).verdict === before.verdict, 'the verdict chip changed');
      const s = await storedHeritage(page);
      assert(s && s.warnDistanceM === 42 && !('profileWarning' in s), 'stored ' + JSON.stringify(s));
    });

    await step('the overlay opacity follows the setting at once', async () => {
      await gotoMap(page);
      let o = await overlay(page);
      assert(o.offered && o.on === true, 'overlay ' + JSON.stringify(o));
      await waitOpacity(page, 0.85);
      await openDisplay(page);
      await setNum(page, 'opacityPct', 40);
      await waitOpacity(page, 0.4);
      const s = await storedHeritage(page);
      assert(s && s.opacityPct === 40, 'stored ' + JSON.stringify(s));
    });

    await step('out-of-range values are put back and never stored; a default is a cleared override', async () => {
      await setNum(page, 'opacityPct', 5);
      assert(await page.inputValue(sel('opacityPct')) === '40', 'opacity kept 5');
      await setNum(page, 'warnDistanceM', 300);
      assert(await page.inputValue(sel('warnDistanceM')) === '42', 'distance kept 300');
      await setNum(page, 'opacityPct', '4.5');
      let s = await storedHeritage(page);
      assert(s && s.opacityPct === 40 && s.warnDistanceM === 42, 'stored ' + JSON.stringify(s));
      await setNum(page, 'warnDistanceM', '');
      s = await storedHeritage(page);
      assert(s && !('warnDistanceM' in s), 'an empty distance stored: ' + JSON.stringify(s));
      await setNum(page, 'warnDistanceM', 42);
    });

    await step('round trip: the settings survive a reload and reach the export', async () => {
      await page.reload({ waitUntil: 'domcontentloaded' });
      await ready(page);
      await waitOpacity(page, 0.4);
      await openDisplay(page);
      assert(await page.inputValue(sel('opacityPct')) === '40');
      assert(await page.inputValue(sel('warnDistanceM')) === '42');
      await page.click('.cust-tab[data-tab="export"]');
      await page.waitForSelector('.cust-panel[data-panel="export"].active', { timeout: 4000 });
      const json = JSON.parse(await page.inputValue('#cv2ExportJson'));
      assert(json.heritage && json.heritage.opacityPct === 40 && json.heritage.warnDistanceM === 42, 'export ' + JSON.stringify(json.heritage));
      for (const k of ['radiusM', 'rateLimitPerMin', 'cacheEntries', 'maxInFlight']) assert(!JSON.stringify(json).includes(k), k + ' in the export');
    });

    await step('Reset All brings the defaults back, live', async () => {
      page.once('dialog', (d) => d.accept());
      await page.click('.cust-tab[data-tab="export"]');
      await page.waitForSelector('.cust-panel[data-panel="export"].active', { timeout: 4000 });
      await page.click('#cv2ResetAll');
      await waitOpacity(page, 0.85);
      assert(await storedHeritage(page) === null, 'heritage kept after reset');
      await openDisplay(page);
      assert(await page.inputValue(sel('opacityPct')) === '85');
      assert(await page.inputValue(sel('warnDistanceM')) === '');
      assert(rec.consoleErrors.length === 0, 'console errors: ' + rec.consoleErrors.join(' | '));
    });
    await ctx.close();
  }

  // ── 7. On by default ──────────────────────────────────────────────────────
  {
    // The viewer once turned the overlay off on the map (mc-heritage 0).
    const { ctx, page, rec } = await newCtx({ storage: { 'mc-heritage': '0' } });
    await step('"on by default": the next map starts with it; off is =0 in the link; =0 in a link wins', async () => {
      await gotoMap(page);
      assert((await overlay(page)).on === false, 'the stored off was lost');
      await openDisplay(page);
      await clickCheck(page, 'enabled');
      const s = await storedHeritage(page);
      assert(s && s.enabled === true, 'stored ' + JSON.stringify(s));
      // A new default forgets the old remembered choice, so it applies.
      assert(await page.evaluate(() => localStorage.getItem('mc-heritage')) === null, 'the remembered off was kept');
      await page.reload({ waitUntil: 'domcontentloaded' });
      await ready(page);
      assert((await overlay(page)).on === true, 'the overlay started off');
      await page.evaluate((label) => {
        const lab = Array.from(document.querySelectorAll('.leaflet-control-layers-overlays label')).find((l) => l.textContent.indexOf(label) >= 0);
        lab.querySelector('input').click();
      }, LAYER_LABEL);
      await page.waitForTimeout(400);
      assert(/heritage=0/.test(await page.evaluate(() => location.hash)), 'off not in the link: ' + await page.evaluate(() => location.hash));
      await page.goto(`${BASE}/${MAP_Z13}&heritage=0`, { waitUntil: 'domcontentloaded' });
      await page.reload({ waitUntil: 'domcontentloaded' });
      await ready(page);
      assert((await overlay(page)).on === false, 'heritage=0 in the link lost');
      assert(rec.consoleErrors.length === 0, 'console errors: ' + rec.consoleErrors.join(' | '));
    });
    await ctx.close();
  }

  // ── 8. A phone, cold start, real taps ────────────────────────────────────
  {
    const { ctx, page, rec } = await newCtx({ viewport: { width: 375, height: 560 }, phone: true });
    const inScreen = async (selector) => {
      const loc = page.locator(selector);
      await loc.scrollIntoViewIfNeeded();
      const b = await loc.boundingBox();
      assert(b && b.x >= 0 && b.x + b.width <= 375 && b.y >= 0 && b.y + b.height <= 560, selector + ' off screen ' + JSON.stringify(b));
      return b;
    };
    async function tapCheck(key, want) {
      const label = `label[for="cv2-heritage-${key}"]`;
      const b = await inScreen(label);
      assert(b.height >= 48, key + ' row ' + b.height + ' px');
      await page.tap(label);
      await page.waitForTimeout(700);
      assert((await page.isChecked(sel(key))) === want, key + ' did not follow the tap');
    }
    async function tapType(key, value) {
      const b = await inScreen(sel(key));
      assert(b.height >= 48, key + ' input ' + b.height + ' px');
      await page.tap(sel(key));
      await page.keyboard.press('ControlOrMeta+A');
      await page.keyboard.press('Backspace');
      if (value !== '') await page.keyboard.type(String(value));
      // Leave the field with a tap on the section title: that commits it.
      const t = page.locator('.cust-section-title', { hasText: 'Ancient Monuments' });
      await t.scrollIntoViewIfNeeded();
      await t.tap();
      await page.waitForTimeout(700);
    }
    await step('375x560 phone: every monuments setting changes with real taps, inside the screen', async () => {
      await gotoMap(page);
      await openDisplay(page, true);
      await tapCheck('enabled', true);
      await tapType('opacityPct', 55);
      await tapCheck('profileWarning', false);
      await tapType('warnDistanceM', 20);
      const s = await storedHeritage(page);
      assert(s && s.enabled === true && s.opacityPct === 55 && s.profileWarning === false && s.warnDistanceM === 20, 'stored ' + JSON.stringify(s));
      const over = await page.evaluate(() => {
        const o = document.querySelector('.cust-overlay');
        return { page: document.documentElement.scrollWidth - document.documentElement.clientWidth, overlay: o ? o.scrollWidth - o.clientWidth : 0 };
      });
      assert(over.page <= 0 && over.overlay <= 0, 'horizontal overflow ' + JSON.stringify(over));
      if (SHOT_DIR) {
        await page.locator('.cust-section-title', { hasText: 'Ancient Monuments' }).scrollIntoViewIfNeeded();
        await page.screenshot({ path: path.join(SHOT_DIR, '723-m4-customizer-375x560.jpg'), type: 'jpeg', quality: 70 });
      }
    });
    await step('375x560 phone: the next map starts with the overlay on, at the new opacity', async () => {
      await page.reload({ waitUntil: 'domcontentloaded' });
      await ready(page);
      const o = await overlay(page);
      assert(o.on === true, 'overlay ' + JSON.stringify(o));
      await waitOpacity(page, 0.55);
      assert(rec.consoleErrors.length === 0, 'console errors: ' + rec.consoleErrors.join(' | '));
    });
    await ctx.close();
  }

  // ── 9. Not offered ──────────────────────────────────────────────────────
  {
    const { ctx, page, rec } = await newCtx({ heritage: false });
    await step('without the heritage block the section is absent', async () => {
      await gotoMap(page);
      await openDisplay(page);
      assert(await page.$('[data-cv2-heritage]') === null, 'heritage inputs without the overlay');
      const title = await page.evaluate(() => Array.from(document.querySelectorAll('.cust-section-title')).some((p) => p.textContent === 'Ancient Monuments'));
      assert(!title, 'Ancient Monuments title without the overlay');
      assert(rec.consoleErrors.length === 0, 'console errors: ' + rec.consoleErrors.join(' | '));
    });
    await ctx.close();
  }

  await browser.close();
  console.log(`\n${passed}/${passed + failed} tests passed`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
