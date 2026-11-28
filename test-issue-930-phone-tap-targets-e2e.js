#!/usr/bin/env node
/* Issue #930 — phone tap targets for the map-control checkboxes and the
 * More sheet's "Customize" item.
 *
 * At 375x560 and 360x640 with touch, every step a real touchscreen tap from a
 * cold start (fresh context, no saved state, opened on #/home):
 *
 *  1. Bottom-nav Map → gear → each map-control section in turn. Every visible
 *     checkbox's hit area (its <label> row, which toggles it) measures at
 *     least 44x44 with getBoundingClientRect, and the row owns its own top
 *     and bottom edge (elementFromPoint), so no neighbour row covers it.
 *  2. A real tap on the Heat map row's edge, 18 px above and then below the
 *     checkbox centre and near the row's right end (well outside the 13 px
 *     box), switches the heat layer on and then off again.
 *  3. Bottom-nav More → "Customize": the item measures at least 44x44, and a
 *     real tap near its corner (outside the 70x16 label) opens the customizer
 *     and closes the sheet.
 *  4. Desktop 1280x800 without touch keeps the old compact rows (2 px padding,
 *     no min-height, a one-line row under 30 px), so the change is phone-only.
 *
 * Defaults to localhost:13581 — NEVER point at prod (AGENTS.md). CI sets BASE_URL.
 * Run: BASE_URL=http://localhost:13581 node test-issue-930-phone-tap-targets-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const MIN = 44;
const PHONES = [{ width: 375, height: 560 }, { width: 360, height: 640 }];

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
  console.log(`\n=== #930 phone tap targets against ${BASE} ===\n`);
  const errors = [];

  async function cold(viewport, touch) {
    const ctx = await browser.newContext({ viewport, hasTouch: touch, isMobile: touch });
    const page = await ctx.newPage();
    page.setDefaultTimeout(15000);
    page.on('console', (m) => { if (m.type() === 'error' && !/tile|net::ERR|Failed to load resource/i.test(m.text())) errors.push(m.text()); });
    page.on('pageerror', (e) => errors.push(String(e)));
    await page.goto(BASE + '/#/home', { waitUntil: 'domcontentloaded' });
    return { ctx, page };
  }

  // Cold #/home → bottom-nav Map → gear: the map controls are open.
  async function openControls(page) {
    await tap(page, '[data-bottom-nav-tab="map"]');
    await page.waitForSelector('#mapControlsToggle', { state: 'visible' });
    await tap(page, '#mapControlsToggle');
    await page.waitForFunction(() => !document.getElementById('mapControls').classList.contains('collapsed'));
  }
  // Opens a section of the accordion by a real tap on its legend.
  async function openSection(page, idx) {
    const sel = `#mapControls fieldset.mc-section:nth-of-type(${idx + 1}) > legend.mc-label`;
    if (await page.evaluate((s) => document.querySelector(s).getAttribute('aria-expanded') !== 'true', sel)) {
      await tap(page, sel);
      await page.waitForFunction((s) => document.querySelector(s).getAttribute('aria-expanded') === 'true', sel);
    }
  }
  // Every visible checkbox in the controls: its hit area (the label that
  // toggles it, or the box itself) and which rows own the area's edges.
  const ROWS = () => Array.from(document.querySelectorAll('#mapControls input[type="checkbox"]'))
    .filter((cb) => cb.getClientRects().length && cb.getBoundingClientRect().width > 0)
    .map((cb) => {
      const hit = cb.closest('label') || (cb.labels && cb.labels[0]) || cb;
      hit.scrollIntoView({ block: 'center' });
      const r = hit.getBoundingClientRect();
      const owns = (x, y) => { const t = document.elementFromPoint(x, y); return !!t && (t === cb || hit.contains(t)); };
      const x = r.right - 8;
      return { id: cb.id, w: r.width, h: r.height, top: owns(x, r.top + 2), bottom: owns(x, r.bottom - 2) };
    });

  for (const vp of PHONES) {
    const tag = `${vp.width}x${vp.height}`;

    await step(`${tag} touch: every map-control checkbox row is at least ${MIN}x${MIN} and owns its edges`, async () => {
      const { ctx, page } = await cold(vp, true);
      await openControls(page);
      const n = await page.evaluate(() => document.querySelectorAll('#mapControls fieldset.mc-section').length);
      const seen = [];
      for (let i = 0; i < n; i++) {
        await openSection(page, i);
        for (const r of await page.evaluate(ROWS)) {
          if (seen.some((s) => s.id === r.id)) continue;
          seen.push(r);
          assert(r.h >= MIN && r.w >= MIN, `#${r.id} hit area ${r.w.toFixed(0)}x${r.h.toFixed(0)}, want >= ${MIN}x${MIN}`);
          assert(r.top && r.bottom, `#${r.id} row edges are covered (top ${r.top}, bottom ${r.bottom})`);
        }
      }
      ['mcHeatmap', 'mcClusters', 'mcHashLabels', 'mcMultiByte', 'mcNeighbors', 'mcTopRoutes', 'mcRole_repeater']
        .forEach((id) => assert(seen.some((s) => s.id === id), `#${id} was never measured (saw ${seen.map((s) => s.id).join(',')})`));
      await ctx.close();
    });

    await step(`${tag} touch: real taps on the Heat map row edge, outside the box, switch the layer on and off`, async () => {
      const { ctx, page } = await cold(vp, true);
      await openControls(page);
      await openSection(page, await page.evaluate(() => Array.from(document.querySelectorAll('#mapControls fieldset.mc-section')).findIndex((f) => f.contains(document.getElementById('mcHeatmap')))));
      assert(await page.evaluate(() => !document.getElementById('mcHeatmap').checked && !document.querySelector('canvas.leaflet-heatmap-layer')), 'heat map on before any tap');
      await tapRowEdge(page, -18);
      await page.waitForFunction(() => document.getElementById('mcHeatmap').checked && !!document.querySelector('canvas.leaflet-heatmap-layer'), null, { timeout: 5000 });
      await tapRowEdge(page, +18);
      await page.waitForFunction(() => !document.getElementById('mcHeatmap').checked && !document.querySelector('canvas.leaflet-heatmap-layer'), null, { timeout: 5000 });
      await ctx.close();
    });

    await step(`${tag} touch: More → Customize is at least ${MIN}x${MIN} and a real tap on its corner opens the customizer`, async () => {
      const { ctx, page } = await cold(vp, true);
      await tap(page, '[data-bottom-nav-tab="more"]');
      const item = '[data-bottom-nav-sheet] [data-mpa-mirror="customizeToggle"]';
      await page.waitForSelector(item, { state: 'visible' });
      const r = await page.evaluate((s) => { const b = document.querySelector(s).getBoundingClientRect(); return { x: b.x, y: b.y, w: b.width, h: b.height }; }, item);
      assert(r.h >= MIN && r.w >= MIN, `Customize hit area ${r.w.toFixed(0)}x${r.h.toFixed(0)}, want >= ${MIN}x${MIN}`);
      assert(!(await page.evaluate(() => !!document.querySelector('.cust-overlay'))), 'customizer open before the tap');
      // 6 px in from the item's top-left corner: outside the label text.
      await tapAt(page, item, Math.round(r.x + 6), Math.round(r.y + 6));
      await page.waitForFunction(() => { const o = document.querySelector('.cust-overlay'); return !!o && o.getBoundingClientRect().height > 100; }, null, { timeout: 5000 });
      assert(await page.evaluate(() => document.querySelector('[data-bottom-nav-sheet]').hidden), 'More sheet still open after Customize');
      await ctx.close();
    });
  }

  await step('1280x800 desktop: the map-control rows keep their compact size', async () => {
    const { ctx, page } = await cold({ width: 1280, height: 800 }, false);
    await page.goto(BASE + '/#/map', { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#mcRole_repeater', { state: 'visible' });
    // The desktop rows keep the base `.mc-section label` box: 2 px padding,
    // no min-height. A one-line row (Heat map) stays 20 px tall.
    const rows = await page.evaluate(() => Array.from(document.querySelectorAll('#mapControls input[type="checkbox"]'))
      .filter((cb) => cb.getClientRects().length)
      .map((cb) => { const l = cb.closest('label'), cs = getComputedStyle(l); return { id: cb.id, h: l.getBoundingClientRect().height, pad: cs.paddingTop + ' ' + cs.paddingBottom, min: cs.minHeight }; }));
    assert(rows.length >= 8, 'only ' + rows.length + ' rows visible');
    rows.forEach((r) => assert(r.pad === '2px 2px' && (r.min === '0px' || r.min === 'auto'), `#${r.id} desktop row has padding ${r.pad}, min-height ${r.min}; want 2px 2px and none`));
    const heat = rows.find((r) => r.id === 'mcHeatmap');
    assert(heat && heat.h < 30, 'Heat map row is ' + (heat && heat.h.toFixed(0)) + ' px tall on desktop, want the compact < 30');
    await ctx.close();
  });

  await step('no console errors', async () => { assert(!errors.length, errors.slice(0, 3).join(' | ')); });

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();

// A real touch tap at (x, y), after checking that `sel` (or its label) is the
// topmost element there, so nothing covers it.
async function tapAt(page, sel, x, y) {
  const hit = await page.locator(sel).first().evaluate((el, p) => {
    const top = document.elementFromPoint(p.x, p.y);
    return !!top && (el === top || el.contains(top) || (el.labels && Array.from(el.labels).some((l) => l.contains(top))));
  }, { x, y });
  if (!hit) throw new Error(sel + ' is not the target at ' + x + ',' + y);
  await page.touchscreen.tap(x, y);
}
async function tap(page, sel) {
  const loc = page.locator(sel).first();
  await loc.scrollIntoViewIfNeeded();
  const b = await loc.boundingBox();
  if (!b) throw new Error(sel + ' has no box');
  await tapAt(page, sel, Math.round(b.x + b.width / 2), Math.round(b.y + b.height / 2));
}
// Taps the Heat map row near its right end, dy px from the checkbox centre:
// at |dy| = 18 the point is far outside the 13 px box.
async function tapRowEdge(page, dy) {
  await page.locator('#mcHeatmap').scrollIntoViewIfNeeded();
  const p = await page.evaluate((d) => {
    const cb = document.getElementById('mcHeatmap').getBoundingClientRect();
    const row = document.getElementById('mcHeatmap').closest('label').getBoundingClientRect();
    return { x: Math.round(row.right - 8), y: Math.round(cb.top + cb.height / 2 + d) };
  }, dy);
  await tapAt(page, '#mcHeatmap', p.x, p.y);
}
