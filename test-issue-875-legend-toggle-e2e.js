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

  // Review finding 3: the x (and corner button) must be on screen and hit by
  // itself at every common height; the legend scrolls inside its own box.
  const inView = (page, sel) => page.evaluate((q) => {
    const el = document.querySelector(q), r = el.getBoundingClientRect();
    const nav = document.querySelector('.top-nav, nav.top-nav');
    const navB = nav ? nav.getBoundingClientRect().bottom : 0;
    return { top: r.top, navB, bottom: r.bottom, vh: window.innerHeight };
  }, sel);
  const CLOSE_VPS = [[1280, 500], [1024, 600], [1024, 700], [1366, 650], [812, 375], [1024, 768]];
  for (const [w, h] of CLOSE_VPS) {
    await page.setViewportSize({ width: w, height: h });
    await step(`[${w}x${h}] legend x and corner button reachable in the default corner; legend inside the viewport`, async () => {
      await setup(page, 'br', false);
      const bad = await hitProbe(page, ['#legendCloseBtn', '#liveLegend .panel-corner-btn']);
      assert(bad.length === 0, bad.join('; '));
      const g = await page.$eval('#liveLegend', (e) => { const r = e.getBoundingClientRect(); return [r.top, r.bottom, window.innerHeight]; });
      assert(g[0] >= 52 && g[1] <= g[2], `legend ${g[0]}..${g[1]} outside 52..${g[2]}`);
    });
  }

  // Review finding 4: in every corner the open legend leaves zoom, Fullscreen,
  // Settings and the legend toggle alone, and its own x stays reachable.
  const SHORT_VPS = [[812, 375], [667, 375], [1024, 480], [1280, 500], [1024, 600], [1366, 650]];
  for (const [w, h] of SHORT_VPS) {
    await page.setViewportSize({ width: w, height: h });
    for (const corner of ['tl', 'tr', 'br', 'bl']) {
      await step(`[${w}x${h}] legend open in ${corner}: stack clear and x reachable`, async () => {
        await setup(page, corner, false);
        const bad = await hitProbe(page, STACK.concat(['#legendCloseBtn']));
        assert(bad.length === 0, bad.join('; '));
        const g = await page.$eval('#liveLegend', (e) => { const r = e.getBoundingClientRect(); return [r.top, r.bottom, window.innerHeight]; });
        assert(g[0] >= 52 && g[1] <= g[2], `legend ${g[0]}..${g[1]} outside 52..${g[2]}`);
      });
    }
  }
  // The scroll box really scrolls when the room is short.
  await page.setViewportSize({ width: 812, height: 375 });
  await step('[812x375] legend content scrolls inside the panel, header stays put', async () => {
    await setup(page, 'br', false);
    const r = await page.$eval('#liveLegend .panel-content', (e) => ({ sh: e.scrollHeight, ch: e.clientHeight, oy: getComputedStyle(e).overflowY }));
    assert(r.sh > r.ch && r.oy !== 'visible', JSON.stringify(r));
    const before = await page.$eval('#legendCloseBtn', (e) => e.getBoundingClientRect().top);
    await page.$eval('#liveLegend .panel-content', (e) => { e.scrollTop = e.scrollHeight; });
    const after = await page.$eval('#legendCloseBtn', (e) => e.getBoundingClientRect().top);
    assert(before === after, `x moved from ${before} to ${after}`);
  });

  // Review findings 5 and 6: aria-expanded, aria-controls, title and label on
  // load, close, reopen. aria-expanded (not aria-pressed) is deliberate.
  await page.setViewportSize({ width: 1440, height: 900 });
  await step('toggle aria-expanded, aria-controls, label and title follow the state', async () => {
    const read = () => page.$eval('#legendToggleBtn', (e) => [e.getAttribute('aria-expanded'), e.getAttribute('aria-label'), e.title, e.getAttribute('aria-controls')].join('|'));
    await setup(page, 'br', false);
    assert(await read() === 'true|Hide legend|Hide legend|liveLegend', 'load open: ' + await read());
    await page.click('#legendCloseBtn');
    assert(await read() === 'false|Show legend|Show legend|liveLegend', 'after x: ' + await read());
    assert(await page.evaluate(() => document.activeElement && document.activeElement.id) === 'legendToggleBtn', 'focus not moved to the toggle');
    await page.click('#legendToggleBtn');
    assert(await read() === 'true|Hide legend|Hide legend|liveLegend', 'reopened: ' + await read());
    await page.click('#legendToggleBtn');
    await page.reload({ waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#legendToggleBtn');
    await page.waitForTimeout(300);
    assert(await read() === 'false|Show legend|Show legend|liveLegend', 'load closed: ' + await read());
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
