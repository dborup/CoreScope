/**
 * E2E (#912 M2) — the operator chooses the default 1-byte hop mode.
 *
 * The server's own /api/config/client carries customizer.oneByteHopsDefault
 * (mark when config.json has no key). The other steps answer that request
 * with the server's real body plus oneByteHopsDefault "hide", and check on
 * the 22-hop 1-byte fixture route (packet 57224a4a01c9c682, observation 147,
 * as #674's suite uses) that:
 *   - a fresh visitor gets Hide: no 1-byte relays on the route, nothing
 *     stored, and the customizer selects Hide and labels it the server default;
 *   - a visitor's own choice (Show, picked in the customizer) wins over the
 *     default and survives a reload;
 *   - an explicit Mark survives the Hide default across a reload.
 * Also: the customizer's 1-byte hop setting on a 375x560 phone, light and
 * dark, does not scroll sideways and fits the screen.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-issue-912-m2-1byte-operator-default-e2e.js
 * Set E2E_SHOTS=<dir> to keep the phone screenshots.
 */
'use strict';
const path = require('path');
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const ONE_BYTE = '#/map?packet=57224a4a01c9c682&obs=147';
const RELAYS = 22;
const MODE_KEY = 'meshcore-1byte-hops-mode';

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

  // A fresh browser profile per call. operatorDefault, when set, replaces
  // customizer.oneByteHopsDefault in the server's real client config.
  async function open(route, { operatorDefault, viewport, theme } = {}) {
    const ctx = await browser.newContext(viewport
      ? { viewport, isMobile: true, hasTouch: true }
      : { viewport: { width: 1280, height: 900 } });
    if (theme) {
      await ctx.addInitScript((t) => { try { localStorage.setItem('meshcore-theme', t); } catch (_e) { /* storage unavailable */ } }, theme);
    }
    if (operatorDefault) {
      await ctx.route(url => url.pathname === '/api/config/client', async r => {
        const res = await r.fetch();
        const body = await res.json();
        body.customizer = Object.assign({}, body.customizer, { oneByteHopsDefault: operatorDefault });
        await r.fulfill({ response: res, json: body });
      });
    }
    const page = await ctx.newPage();
    page.setDefaultTimeout(30000);
    const errors = [];
    page.on('pageerror', e => errors.push(String(e.message)));
    await page.goto(BASE + '/' + route, { waitUntil: 'load' });
    await page.evaluate(() => window.MeshConfigReady);
    return { ctx, page, errors };
  }

  const relays = (page) => page.evaluate(() =>
    [...document.querySelectorAll('.mc-rt-sidebar .mc-rt-row')].filter(r => !/from payload/.test(r.textContent)).length);
  const waitRelays = (page, n) => page.waitForFunction((n) =>
    [...document.querySelectorAll('.mc-rt-sidebar .mc-rt-row')].filter(r => !/from payload/.test(r.textContent)).length === n, n);
  const waitRouteDrawn = (page) => page.waitForFunction(() => !!window.__mc_lastRouteDraw);
  const stored = (page) => page.evaluate((k) => localStorage.getItem(k), MODE_KEY);
  const mode = (page) => page.evaluate(() => window.MC_get1ByteHopsMode());

  // A phone reaches the customizer by taps, through the bottom nav's More.
  async function openCustomizerDisplay(page, { touch = false } = {}) {
    if (touch) {
      await page.tap('[data-bottom-nav-tab="more"]');
      await page.waitForSelector('[data-mpa-mirror="customizeToggle"]', { state: 'visible' });
      await page.tap('[data-mpa-mirror="customizeToggle"]');
    } else await page.click('#customizeToggle');
    await page.waitForSelector('.cust-overlay:not(.hidden)');
    if (touch) await page.tap('.cust-tab[data-tab="display"]'); else await page.click('.cust-tab[data-tab="display"]');
    await page.waitForSelector('#cv2-hide-1byte-hops');
  }
  const selectState = (page) => page.$eval('#cv2-hide-1byte-hops', sel => ({
    value: sel.value,
    defaults: [...sel.options].filter(o => /\(server default\)/.test(o.textContent)).map(o => o.value),
  }));

  console.log(`\n#912 M2 E2E against ${BASE}\n`);

  await step('the server serves customizer.oneByteHopsDefault (mark when config.json has none)', async () => {
    const { ctx, page } = await open('#/home');
    try {
      const cust = await page.evaluate(async () => (await (await fetch('/api/config/client')).json()).customizer);
      assert(cust && ['show', 'mark', 'hide'].includes(cust.oneByteHopsDefault), JSON.stringify(cust));
      assert(Array.isArray(cust.disabledTabs), 'disabledTabs must stay: ' + JSON.stringify(cust));
    } finally { await ctx.close(); }
  });

  await step('a fresh visitor gets the operator default (Hide): no 1-byte relays, nothing stored, customizer says so', async () => {
    const { ctx, page, errors } = await open(ONE_BYTE, { operatorDefault: 'hide' });
    try {
      await waitRouteDrawn(page);
      await page.waitForFunction(() => window.MC_get1ByteHopsMode() === 'hide');
      await page.waitForFunction(() => [...document.querySelectorAll('.mc-rt-sidebar .mc-rt-row')]
        .filter(r => !/from payload/.test(r.textContent)).length === 0);
      assert(await stored(page) === null, 'following the default must store nothing');
      await openCustomizerDisplay(page);
      const s = await selectState(page);
      assert(s.value === 'hide' && JSON.stringify(s.defaults) === '["hide"]', JSON.stringify(s));
      assert(errors.length === 0, 'page errors: ' + errors.join(' | '));
    } finally { await ctx.close(); }
  });

  await step('control: without an operator default the same visitor gets Mark (all 22 relays)', async () => {
    const { ctx, page } = await open(ONE_BYTE);
    try {
      await waitRelays(page, RELAYS);
      assert(await mode(page) === 'mark', 'mode ' + await mode(page));
    } finally { await ctx.close(); }
  });

  await step('the visitor\'s own choice (Show) wins over the Hide default and survives a reload', async () => {
    const { ctx, page, errors } = await open(ONE_BYTE, { operatorDefault: 'hide' });
    try {
      await waitRouteDrawn(page);
      await page.waitForFunction(() => window.MC_get1ByteHopsMode() === 'hide');
      await openCustomizerDisplay(page);
      await page.selectOption('#cv2-hide-1byte-hops', 'show');
      await page.click('#customizeToggle');
      await waitRelays(page, RELAYS);
      assert(await stored(page) === 'show', 'stored ' + await stored(page));
      await page.reload({ waitUntil: 'load' });
      await page.evaluate(() => window.MeshConfigReady);
      await waitRelays(page, RELAYS);
      assert(await mode(page) === 'show', 'after reload: ' + await mode(page));
      await openCustomizerDisplay(page);
      const s = await selectState(page);
      assert(s.value === 'show' && JSON.stringify(s.defaults) === '["hide"]', JSON.stringify(s));
      assert(errors.length === 0, 'page errors: ' + errors.join(' | '));
    } finally { await ctx.close(); }
  });

  await step('an explicit Mark is stored and survives the Hide default across a reload', async () => {
    const { ctx, page } = await open(ONE_BYTE, { operatorDefault: 'hide' });
    try {
      await waitRouteDrawn(page);
      await openCustomizerDisplay(page);
      await page.selectOption('#cv2-hide-1byte-hops', 'mark');
      await page.click('#customizeToggle');
      await waitRelays(page, RELAYS);
      assert(await stored(page) === 'mark', 'stored ' + await stored(page));
      await page.reload({ waitUntil: 'load' });
      await page.evaluate(() => window.MeshConfigReady);
      await waitRelays(page, RELAYS);
      assert(await mode(page) === 'mark', 'after reload: ' + await mode(page) + ', relays ' + await relays(page));
    } finally { await ctx.close(); }
  });

  for (const theme of ['light', 'dark']) {
    await step(`375x560 phone, ${theme}: the 1-byte hop setting fits and the page does not scroll sideways`, async () => {
      const { ctx, page, errors } = await open('#/home', { operatorDefault: 'hide', viewport: { width: 375, height: 560 }, theme });
      try {
        await openCustomizerDisplay(page, { touch: true });
        await page.$eval('#cv2-hide-1byte-hops', el => el.scrollIntoView({ block: 'center' }));
        const g = await page.evaluate(() => {
          const sel = document.getElementById('cv2-hide-1byte-hops');
          const r = sel.getBoundingClientRect();
          const label = document.querySelector('label[for="cv2-hide-1byte-hops"]').getBoundingClientRect();
          const cs = getComputedStyle(sel);
          return {
            theme: document.documentElement.getAttribute('data-theme'),
            left: r.left, right: r.right, top: r.top, bottom: r.bottom, h: r.height,
            labelRight: label.right, vw: window.innerWidth, vh: window.innerHeight,
            scroll: document.documentElement.scrollWidth, client: document.documentElement.clientWidth,
            bg: cs.backgroundColor, fg: cs.color,
            text: sel.options[sel.selectedIndex].textContent,
          };
        });
        assert(g.theme === theme, 'theme ' + g.theme);
        assert(g.scroll <= g.client + 1, 'horizontal overflow: ' + JSON.stringify(g));
        assert(g.left >= 0 && g.right <= g.vw && g.labelRight <= g.vw, 'select or label leaves the screen: ' + JSON.stringify(g));
        assert(g.top >= 0 && g.bottom <= g.vh, 'select not reachable on screen: ' + JSON.stringify(g));
        assert(g.h >= 24, 'select too small to tap: ' + g.h);
        assert(g.bg !== g.fg, 'select text has no contrast: ' + JSON.stringify(g));
        assert(/Hide \(server default\)/.test(g.text), 'selected option text: ' + g.text);
        if (process.env.E2E_SHOTS) {
          await page.screenshot({ path: path.join(process.env.E2E_SHOTS, `912-m2-phone-${theme}.png`) });
        }
        assert(errors.length === 0, 'page errors: ' + errors.join(' | '));
      } finally { await ctx.close(); }
    });
  }

  await browser.close();
  console.log(`\n#912 M2 E2E: ${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch(e => { console.error('  ✗ #912 M2 E2E:', e.stack || e); process.exit(1); });
