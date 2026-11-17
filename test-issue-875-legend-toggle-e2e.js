#!/usr/bin/env node
/**
 * #875 E2E: legend toggle sits in the top-right Leaflet control stack and is
 * never covered by the legend; the legend has a close button.
 * Run: BASE_URL=http://localhost:13581 node test-issue-875-legend-toggle-e2e.js
 */
'use strict';
const { chromium } = require('playwright');
const BASE = process.env.BASE_URL || 'http://localhost:13581';
let failed = 0;
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }
async function step(name, fn) {
  try { await fn(); console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message.split('\n')[0]); }
}

async function setup(page, corner, hidden) {
  await page.goto(BASE + '/#/live', { waitUntil: 'domcontentloaded' });
  await page.evaluate(([c, h]) => {
    localStorage.setItem('panel-corner-liveLegend', c);
    localStorage.setItem('live-legend-hidden', String(h));
  }, [corner, hidden]);
  await page.reload({ waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#legendToggleBtn');
  await page.waitForSelector('#liveLegend');
  await page.waitForFunction(() => {
    const v = getComputedStyle(document.querySelector('.live-page')).getPropertyValue('--vcr-bar-height');
    return v && parseFloat(v) > 0;
  });
  await page.waitForTimeout(400);
}

// Hit-test the centre of every control in the stack; returns offenders.
function hitProbe(page, ids) {
  return page.evaluate((list) => list.map((sel) => {
    const el = document.querySelector(sel);
    if (!el) return sel + ': missing';
    const r = el.getBoundingClientRect();
    if (!r.width || !r.height) return sel + ': not visible';
    const hit = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2);
    return (hit === el || el.contains(hit)) ? null : sel + ': covered by ' + (hit ? hit.tagName + '#' + hit.id + '.' + hit.className : 'none');
  }).filter(Boolean), ids);
}

const STACK = ['.leaflet-control-zoom-in', '#liveFullscreenToggle', '#liveControlsToggle', '#legendToggleBtn'];

(async () => {
  let browser;
  try {
    browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_PATH || undefined,
      args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'] });
  } catch (e) {
    if (process.env.CHROMIUM_REQUIRE === '1') throw e;
    console.log('SKIP: Chromium unavailable'); process.exit(0);
  }
  const ctx = await browser.newContext();
  const page = await ctx.newPage();
  page.setDefaultTimeout(10000);

  for (const vp of [{ width: 1024, height: 768 }, { width: 1440, height: 900 }]) {
    await page.setViewportSize(vp);
    for (const corner of ['tl', 'tr', 'br', 'bl']) {
      await step(`[${vp.width}x${vp.height}] legend open in ${corner}: control stack not covered`, async () => {
        await setup(page, corner, false);
        const open = await page.$eval('#liveLegend', (e) => !e.classList.contains('hidden'));
        assert(open, 'legend should be open');
        const bad = await hitProbe(page, STACK);
        assert(bad.length === 0, bad.join('; '));
        const toggleInStack = await page.$eval('#legendToggleBtn', (e) => !!e.closest('.leaflet-top.leaflet-right .live-leaflet-toggle'));
        assert(toggleInStack, 'legend toggle not inside topright live-leaflet-toggle');
        const order = await page.evaluate(() => {
          const t = (s) => document.querySelector(s).getBoundingClientRect().top;
          return t('#liveFullscreenToggle') < t('#liveControlsToggle') && t('#liveControlsToggle') < t('#legendToggleBtn');
        });
        assert(order, 'legend toggle is not below Fullscreen and Settings');
        const closeBad = await hitProbe(page, ['#legendCloseBtn', '#liveLegend .panel-corner-btn']);
        assert(closeBad.length === 0, closeBad.join('; '));
      });
    }
  }

  await page.setViewportSize({ width: 1440, height: 900 });
  await step('close button hides legend, toggle reopens, state survives reload', async () => {
    await setup(page, 'br', false);
    await page.click('#legendCloseBtn');
    assert(await page.$eval('#liveLegend', (e) => e.classList.contains('hidden')), 'close did not hide');
    assert(await page.evaluate(() => localStorage.getItem('live-legend-hidden')) === 'true', 'hidden not persisted');
    await page.reload({ waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#legendToggleBtn');
    await page.waitForTimeout(300);
    assert(await page.$eval('#liveLegend', (e) => e.classList.contains('hidden')), 'hidden state lost on reload');
    await page.click('#legendToggleBtn');
    assert(!(await page.$eval('#liveLegend', (e) => e.classList.contains('hidden'))), 'toggle did not reopen');
    assert(await page.evaluate(() => localStorage.getItem('live-legend-hidden')) === 'false', 'open not persisted');
    await page.click('#legendToggleBtn');
    assert(await page.$eval('#liveLegend', (e) => e.classList.contains('hidden')), 'toggle did not close');
    assert(await page.$eval('#legendToggleBtn', (e) => e.getAttribute('aria-label')) === 'Show legend', 'label not Show legend');
  });

  await step('corner cycling still works on the legend', async () => {
    await setup(page, 'br', false);
    await page.click('#liveLegend .panel-corner-btn');
    const pos = await page.$eval('#liveLegend', (e) => e.getAttribute('data-position'));
    assert(pos === 'bl' || pos === 'tl', 'corner did not change, got ' + pos);
    assert((await hitProbe(page, STACK)).length === 0, 'stack covered after cycling');
  });
  await ctx.close();

  // Phone: cold start (fresh context), real taps.
  const phone = await browser.newContext({ viewport: { width: 375, height: 560 }, hasTouch: true, isMobile: true });
  const pp = await phone.newPage();
  pp.setDefaultTimeout(10000);
  await step('[375x560] cold start: settings and fullscreen tappable, legend toggle absent', async () => {
    await pp.goto(BASE + '/#/live', { waitUntil: 'domcontentloaded' });
    await pp.waitForSelector('#liveControlsToggle');
    await pp.waitForTimeout(400);
    const vis = await pp.$eval('#legendToggleBtn', (e) => { const r = e.getBoundingClientRect(); return r.width > 0 && r.height > 0; });
    assert(!vis, 'legend toggle should be hidden on phone (legend is hidden there)');
    assert((await hitProbe(pp, ['#liveFullscreenToggle', '#liveControlsToggle'])).length === 0, 'phone controls covered');
    await pp.tap('#liveControlsToggle');
    assert(await pp.$eval('#liveControlsToggle', (e) => e.getAttribute('aria-expanded')) === 'true', 'tap on settings did not expand');
    await pp.tap('#liveControlsToggle');
    await pp.tap('#liveFullscreenToggle');
    assert(await pp.evaluate(() => document.body.classList.contains('live-fullscreen')), 'tap on fullscreen did nothing');
  });

  await browser.close();
  console.log(failed ? '\n' + failed + ' failed' : '\nall passed');
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error('FAIL: ' + e.stack); process.exit(1); });
