/**
 * E2E (#727 M4): share previews in the customizer's Terrain Profile section,
 * and what the viewer's switch does to the Path profile's Copy link.
 *
 * The CI server has no map.dhm config and no Dataforsyningen token, so the
 * profile and sharing are switched on the way an operator's config would:
 * /api/config/client is intercepted and given the `dhm` block the Go server
 * sends with the profile and map.dhm.profile.share on (share: true, and the
 * card's look as dhm.card), and /api/terrain/profile is answered with a
 * synthetic profile. Nothing reaches the elevation service.
 *
 * Asserts:
 *   1. the Display tab's Terrain Profile section shows Share previews: the
 *      switch (on), the operator's card theme and names, and the one line
 *      on the operator's saved palette; light/hidden from dhm.card too;
 *   2. switching it off is stored as terrain.shareLinks false, survives a
 *      reload, and makes Copy link hand out the #/map link; on again, the
 *      /p link, without a reload; resetAll puts it back on;
 *   3. without dhm.share: the note, no switch; with the Display tab
 *      disabled by the operator (customizer.disabledTabs): no tab, no
 *      reachable switch;
 *   4. the phone flow at 375x560 (touch): the section is reachable by
 *      scrolling the customizer, the switch row is at least 48px tall, a tap
 *      toggles it, and nothing overflows sideways;
 *   5. axe (contrast, labels) on the Display panel in light and dark.
 *
 * Usage:
 *   BASE_URL=http://localhost:13800 node test-issue-727-m4-share-customizer-e2e.js
 * Optional: SCREENSHOT_DIR=<dir> writes the PR screenshots (small JPEGs).
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
  minZoom: 7, maxZoom: 19, profile: true, share: true,
  card: { theme: 'dark', names: true },
  bounds: [[54.4265, 7.99125], [57.778, 15.5995]],
  attribution: ATTRIBUTION,
};
const PNG_1X1 = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGM4ceIEAAS0AlkWLoFAAAAAAElFTkSuQmCC',
  'base64');
const PALETTE_LINE = 'The card is drawn on the server in the operator’s saved palette, not with the colour overrides in this browser.';
const SWITCH = '#cv2-terrain-share';

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

/* The shape of a real GET /api/terrain/profile, enough to open the panel. */
function profileFor(q) {
  const D = 12000;
  const end = (h) => {
    const asked = q.get(h);
    return { lat: 37.7, lon: -122.2, kind: 'point', groundM: 40,
      antennaM: asked !== null ? Number(asked) : 2,
      antennaSource: asked !== null ? 'request' : 'point-default', assumed: asked === null };
  };
  const samples = [];
  for (let i = 0; i < 60; i++) {
    const d = Math.round(D * i / 59);
    samples.push({ d, dtm: 30 + 18 * Math.sin(d / 900), dsm: 42 + 18 * Math.sin(d / 900) });
  }
  return {
    a: end('ha'), b: end('hb'),
    distanceM: D, stepM: 11.7, sampleCount: 1025, model: q.get('model') === 'dtm' ? 'dtm' : 'both',
    coverage: { state: 'full', pct: 100, firstGapM: null }, samples,
    physics: { freqMHz: 869.618, freqSource: 'firmware', lambdaM: 0.34474, k: 1.3333333333333333, kSource: 'default', f1MaxM: 32.14 },
    verdict: { state: 'clear', model: 'dtm', criterion: 0.6, worstClearanceM: 41.2, worstAtM: 5000, worstF1Fraction: 1.4 },
    attribution: ATTRIBUTION,
  };
}

const A0 = '37.80000,-122.35000', B0 = '37.78000,-121.85000';
const PROFILE_LINK = `#/map?profile=${A0},${B0}&model=dtm&ha=12&hb=4`;

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #727 M4 customizer share previews against ${BASE} ===\n`);

  async function newCtx(o) {
    o = o || {};
    const ctx = await browser.newContext({
      viewport: o.viewport || { width: 1440, height: 900 },
      hasTouch: !!o.touch, isMobile: !!o.touch,
    });
    const rec = { profile: [], consoleErrors: [] };
    await ctx.route(BASE + '/api/config/client', async (route) => {
      const response = await route.fetch();
      const config = await response.json();
      config.dhm = o.dhm !== undefined ? o.dhm : DHM_BLOCK;
      if (o.disabledTabs) config.customizer = Object.assign({}, config.customizer, { disabledTabs: o.disabledTabs });
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
        // Seeded once per context: a reload must keep what the page stored.
        if (!sessionStorage.getItem('e2e-seeded')) {
          sessionStorage.setItem('e2e-seeded', '1');
          localStorage.setItem('meshcore-theme', theme);
          localStorage.setItem('meshcore-map-clustering', 'false');
          ['row-swipe', 'tab-swipe', 'edge-drawer', 'pull-refresh']
            .forEach(k => localStorage.setItem('meshcore-gesture-hints-' + k, 'seen'));
        }
      } catch (_) {}
      window.__copied = [];
      try {
        Object.defineProperty(navigator, 'clipboard', { configurable: true, value: {
          writeText: (t) => { window.__copied.push(String(t)); return Promise.resolve(); },
        } });
      } catch (_) {}
    }, { theme: o.theme || 'light' });
    const page = await ctx.newPage();
    page.on('console', m => { if (m.type() === 'error') rec.consoleErrors.push(m.text()); });
    page.on('pageerror', e => rec.consoleErrors.push(String(e)));
    return { ctx, page, rec };
  }

  async function gotoMap(page, hash) {
    await page.goto(`${BASE}/${hash || '#/map?lat=37.62&lon=-122.1&zoom=10'}`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#leaflet-map[data-loaded="true"]', { timeout: 30000 });
    await page.waitForFunction(() => window._customizerV2 && window._customizerV2.initDone, null, { timeout: 15000 });
    await page.waitForTimeout(600);
  }

  /* Opens the customizer on its Display tab the way a viewer does: the
   * palette button, or on a phone (no palette button in the top bar) the
   * bottom nav's More sheet, then Customize. */
  async function openDisplay(page, phone) {
    if (phone) {
      await page.tap('[data-bottom-nav-tab="more"]');
      await page.waitForSelector('[data-mpa-mirror="customizeToggle"]', { state: 'visible', timeout: 5000 });
      await page.tap('[data-mpa-mirror="customizeToggle"]');
    } else {
      await page.click('#customizeToggle');
    }
    await page.waitForSelector('.cust-overlay:not(.hidden)', { timeout: 5000 });
    if (phone) await page.tap('.cust-tab[data-tab="display"]');
    else await page.click('.cust-tab[data-tab="display"]');
    await page.waitForSelector('.cust-panel[data-panel="display"].active', { timeout: 4000 });
  }

  async function closeCustomizer(page) {
    await page.click('#customizeToggle');
    await page.waitForTimeout(300);
  }

  /* An open Path profile panel sits above the customizer's lower part
   * (z-index 1200 over 1050, as before #727), and the Display panel ends
   * with this section, so with a profile open the switch is reached by
   * keyboard: Tab to it and Space. Mouse and tap are covered below with no
   * profile open. */
  async function toggleByKeyboard(page, want) {
    await page.focus(SWITCH);
    if ((await page.isChecked(SWITCH)) !== want) await page.keyboard.press('Space');
    await page.waitForTimeout(400);
    assert((await page.isChecked(SWITCH)) === want, 'the switch did not follow Space');
  }

  const stored = (page) => page.evaluate(() => {
    try { return JSON.parse(localStorage.getItem('cs-theme-overrides') || '{}'); } catch (_) { return null; }
  });

  async function copyLink(page) {
    await page.evaluate(() => { window.__copied.length = 0; });
    await page.click('[data-testid="tp-copy-link"]');
    await page.waitForFunction(() => window.__copied.length === 1, null, { timeout: 3000 });
    return page.evaluate(() => window.__copied[0]);
  }

  async function waitForProfile(page, rec) {
    await page.waitForFunction(() => {
      const p = document.querySelector('[data-testid="tp-panel"]');
      return !!(p && p.classList.contains('is-open'));
    }, null, { timeout: 15000 });
    const t0 = Date.now();
    while (rec.profile.length === 0 && Date.now() - t0 < 10000) await page.waitForTimeout(100);
    await page.waitForTimeout(400);
  }

  // ── 1. The section ──────────────────────────────────────────────────────
  {
    const { ctx, page, rec } = await newCtx();
    await gotoMap(page);
    await openDisplay(page);
    await step('Share previews: the switch, on, with the operator’s card look', async () => {
      await page.locator(SWITCH).scrollIntoViewIfNeeded();
      assert(await page.isVisible(SWITCH), 'switch not visible');
      assert(await page.isChecked(SWITCH), 'switch is off by default');
      assert((await page.textContent('#cv2-card-theme')).trim() === 'Dark', 'theme');
      assert((await page.textContent('#cv2-card-names')).trim() === 'Shown', 'names');
      const text = await page.textContent('.cust-panel[data-panel="display"]');
      assert(text.split(PALETTE_LINE).length === 2, 'the palette line is missing or repeated');
      assert(rec.consoleErrors.length === 0, rec.consoleErrors.join(' | '));
    });
    await ctx.close();
  }
  {
    const { ctx, page } = await newCtx({ dhm: Object.assign({}, DHM_BLOCK, { card: { theme: 'light', names: false } }) });
    await gotoMap(page);
    await openDisplay(page);
    await step('a light card without names reads Light and Hidden', async () => {
      assert((await page.textContent('#cv2-card-theme')).trim() === 'Light', 'theme');
      assert((await page.textContent('#cv2-card-names')).trim() === 'Hidden', 'names');
    });
    await ctx.close();
  }

  // ── 2. What the switch does ─────────────────────────────────────────────
  {
    const { ctx, page, rec } = await newCtx();
    await gotoMap(page, PROFILE_LINK);
    await waitForProfile(page, rec);
    await step('on (the default): Copy link hands out the /p link', async () => {
      const u = new URL(await copyLink(page));
      assert(u.pathname === '/p' && u.hash === '', 'copied ' + u);
    });
    const profilesBefore = rec.profile.length;
    await openDisplay(page);
    await toggleByKeyboard(page, false);
    await step('off is stored as terrain.shareLinks false', async () => {
      const s = await stored(page);
      assert(s && s.terrain && s.terrain.shareLinks === false, JSON.stringify(s));
    });
    await closeCustomizer(page);
    await step('off: Copy link hands out the #/map link, without a reload', async () => {
      const u = new URL(await copyLink(page));
      assert(u.pathname === '/' && u.hash.indexOf('#/map?profile=') === 0, 'copied ' + u);
    });
    await step('the switch did not re-measure the open profile', async () => {
      assert(rec.profile.length === profilesBefore, (rec.profile.length - profilesBefore) + ' extra profile requests');
    });
    await page.reload({ waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#leaflet-map[data-loaded="true"]', { timeout: 30000 });
    await page.waitForFunction(() => window._customizerV2 && window._customizerV2.initDone, null, { timeout: 15000 });
    await waitForProfile(page, rec);
    await step('off survives a reload, in the switch and the copied link', async () => {
      const u = new URL(await copyLink(page));
      assert(u.pathname === '/' && u.hash.indexOf('#/map?profile=') === 0, 'copied ' + u);
      await openDisplay(page);
      assert(!(await page.isChecked(SWITCH)), 'the switch shows on after a reload');
    });
    await toggleByKeyboard(page, true);
    await closeCustomizer(page);
    await step('on again: the override is cleared and the /p link is back', async () => {
      const s = await stored(page);
      assert(!(s && s.terrain && 'shareLinks' in s.terrain), JSON.stringify(s));
      const u = new URL(await copyLink(page));
      assert(u.pathname === '/p', 'copied ' + u);
    });
    await openDisplay(page);
    await toggleByKeyboard(page, false);
    await page.evaluate(() => window._customizerV2.resetAll());
    await page.reload({ waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#leaflet-map[data-loaded="true"]', { timeout: 30000 });
    await page.waitForFunction(() => window._customizerV2 && window._customizerV2.initDone, null, { timeout: 15000 });
    await waitForProfile(page, rec);
    await step('resetAll strips the switch: on, and the /p link', async () => {
      const s = await stored(page);
      assert(!(s && s.terrain && 'shareLinks' in s.terrain), JSON.stringify(s));
      const u = new URL(await copyLink(page));
      assert(u.pathname === '/p', 'copied ' + u);
      assert(rec.consoleErrors.length === 0, rec.consoleErrors.join(' | '));
    });
    await ctx.close();
  }

  // ── 3. Gates ────────────────────────────────────────────────────────────
  {
    const { ctx, page } = await newCtx({ dhm: Object.assign({}, DHM_BLOCK, { share: false, card: undefined }) });
    await gotoMap(page);
    await openDisplay(page);
    await step('without dhm.share: the note, and no switch', async () => {
      const text = await page.textContent('.cust-panel[data-panel="display"]');
      assert(/Link previews are off on this server/.test(text), 'no note');
      assert(text.indexOf(PALETTE_LINE) === -1, 'palette line without a card');
      assert(await page.locator(SWITCH).count() === 0, 'switch present');
    });
    await ctx.close();
  }
  {
    const { ctx, page } = await newCtx({ disabledTabs: ['display'] });
    await gotoMap(page);
    await page.click('#customizeToggle');
    await page.waitForSelector('.cust-overlay:not(.hidden)', { timeout: 5000 });
    await step('a disabled Display tab hides the share switch', async () => {
      assert(await page.locator('.cust-tab[data-tab="display"]').count() === 0, 'Display tab still offered');
      const n = await page.locator(SWITCH).count();
      assert(n === 0 || !(await page.locator(SWITCH).isVisible()), 'the switch is visible');
    });
    await ctx.close();
  }

  // ── 4. The phone flow, 375x560 ─────────────────────────────────────────
  {
    const { ctx, page } = await newCtx({ viewport: { width: 375, height: 560 }, touch: true });
    await gotoMap(page);
    await openDisplay(page, true);
    await page.locator(SWITCH).scrollIntoViewIfNeeded();
    await page.waitForTimeout(300);
    await step('375x560: the switch row is reachable, 48px, inside the screen', async () => {
      const box = await page.locator('label[for="cv2-terrain-share"]').boundingBox();
      assert(box, 'no switch row');
      assert(box.height >= 48, 'row ' + box.height + 'px tall');
      assert(box.x >= 0 && box.x + box.width <= 375 && box.y >= 0 && box.y + box.height <= 560, 'row off screen ' + JSON.stringify(box));
      const over = await page.evaluate(() => {
        const o = document.querySelector('.cust-overlay');
        return { page: document.documentElement.scrollWidth - document.documentElement.clientWidth,
          overlay: o ? o.scrollWidth - o.clientWidth : 0 };
      });
      assert(over.page <= 0 && over.overlay <= 0, 'horizontal overflow ' + JSON.stringify(over));
    });
    await step('375x560: a tap on the row turns it off and stores it', async () => {
      await page.tap('label[for="cv2-terrain-share"]');
      await page.waitForTimeout(400);
      assert(!(await page.isChecked(SWITCH)), 'still on after a tap');
      const s = await stored(page);
      assert(s && s.terrain && s.terrain.shareLinks === false, JSON.stringify(s));
    });
    await step('375x560: the card rows and the palette line are readable in the panel', async () => {
      const line = page.locator('.cust-panel[data-panel="display"] p', { hasText: PALETTE_LINE });
      for (const [name, loc] of [['theme', page.locator('#cv2-card-theme')], ['names', page.locator('#cv2-card-names')], ['palette line', line]]) {
        assert(await loc.count() === 1, name + ': ' + (await loc.count()) + ' elements');
        await loc.scrollIntoViewIfNeeded();
        const b = await loc.boundingBox();
        assert(b && b.x >= 0 && b.x + b.width <= 375 && b.y >= 0 && b.y + b.height <= 560, name + ' ' + JSON.stringify(b));
      }
    });
    if (SHOT_DIR) {
      await page.tap('label[for="cv2-terrain-share"]');
      await page.locator(SWITCH).scrollIntoViewIfNeeded();
      await page.waitForTimeout(300);
      await page.screenshot({ path: path.join(SHOT_DIR, '727-m4-share-customizer-375x560.jpg'), type: 'jpeg', quality: 70 });
    }
    await ctx.close();
  }

  // ── 5. axe ──────────────────────────────────────────────────────────────
  const AXE_RULES = ['color-contrast', 'label', 'aria-required-attr', 'aria-valid-attr', 'button-name', 'link-name'];
  for (const theme of ['light', 'dark']) {
    const { ctx, page } = await newCtx({ theme });
    await gotoMap(page);
    await openDisplay(page);
    await page.locator(SWITCH).scrollIntoViewIfNeeded();
    await step('axe is clean on the Display panel in ' + theme, async () => {
      const { AxeBuilder } = require('@axe-core/playwright');
      const res = await new AxeBuilder({ page }).include('.cust-panel[data-panel="display"]').withRules(AXE_RULES).analyze();
      const v = res.violations.map(x => x.id + ' (' + x.nodes.length + ': ' + x.nodes.slice(0, 2).map(n => n.target.join(' ')).join(', ') + ')');
      assert(res.violations.length === 0, 'axe violations: ' + v.join(', '));
    });
    await ctx.close();
  }

  // ── Screenshots: the section, light and dark, 1440 and 360 ──────────────
  if (SHOT_DIR) {
    for (const theme of ['light', 'dark']) {
      for (const width of [1440, 360]) {
        const { ctx, page } = await newCtx({ theme, viewport: { width, height: width === 1440 ? 900 : 740 }, touch: width === 360 });
        await gotoMap(page);
        await openDisplay(page, width === 360);
        await page.evaluate(() => {
          const el = document.querySelector('#cv2-terrain-share');
          if (el) el.closest('.cust-field').previousElementSibling.scrollIntoView({ block: 'start' });
        });
        await page.waitForTimeout(400);
        const file = path.join(SHOT_DIR, `727-m4-share-customizer-${theme}-${width}.jpg`);
        if (width === 1440) await page.locator('.cust-overlay').screenshot({ path: file, type: 'jpeg', quality: 70 });
        else await page.screenshot({ path: file, type: 'jpeg', quality: 70 });
        await ctx.close();
      }
    }
    console.log('  screenshots → ' + SHOT_DIR);
  }

  await browser.close();
  console.log(`\n#727 M4 E2E: ${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
