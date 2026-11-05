/**
 * E2E (#674) — 1-byte path hops: Show / Mark / Hide, with Mark as the default.
 *
 * Fixture packet 57224a4a01c9c682, observation 147 (the #598 fixture): 22
 * hops, all 1-byte. In Mark mode the map route shows the packet's
 * "Sent with: 1-byte" badge, "N of 22 hops are uncertain", a "? 1 of K" chip
 * on each uncertain guess and a dashed marker for it. Show is the pre-#674
 * view (no markings); Hide leaves the 1-byte hops out (#1633/#673). The mode
 * is switched through the customizer's own select, live, and the route
 * redraws in place. Packet 73658eb54a439ea7 (observation 343, 2-byte hops) is
 * the control: no 1-byte badge. Also checked: the #1633 key migrates to Hide,
 * the notice follows the theme in light and dark, and a 375px phone does not
 * scroll sideways.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-issue-674-1byte-hop-modes-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const ONE_BYTE = '#/map?packet=57224a4a01c9c682&obs=147';
const TWO_BYTE = '#/map?packet=73658eb54a439ea7&obs=343';
const RELAYS = 22;

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

  async function open(route, { storage = {}, mobile = false } = {}) {
    const ctx = await browser.newContext(mobile
      ? { viewport: { width: 375, height: 812 }, isMobile: true, hasTouch: true }
      : { viewport: { width: 1280, height: 900 } });
    await ctx.addInitScript((s) => {
      try {
        if (sessionStorage.getItem('__e2e674_seeded')) return; // seed once per tab
        sessionStorage.setItem('__e2e674_seeded', '1');
        localStorage.removeItem('meshcore-1byte-hops-mode');
        localStorage.removeItem('meshcore-hide-1byte-hops');
        for (const k of Object.keys(s)) localStorage.setItem(k, s[k]);
      } catch (_e) { /* storage unavailable */ }
    }, storage);
    const page = await ctx.newPage();
    page.setDefaultTimeout(30000);
    const errors = [];
    page.on('pageerror', e => errors.push(String(e.message)));
    await page.goto(BASE + '/' + route, { waitUntil: 'load' });
    return { ctx, page, errors };
  }

  const state = (page) => page.evaluate(() => ({
    mode: window.MC_get1ByteHopsMode(),
    relays: [...document.querySelectorAll('.mc-rt-sidebar .mc-rt-row')].filter(r => !/from payload/.test(r.textContent)).length,
    notice: (document.querySelector('.mc-rt-onebyte') || {}).textContent || '',
    chips: document.querySelectorAll('.mc-rt-sidebar .mc-rt-status-unsure').length,
    unsureMarkers: document.querySelectorAll('.mc-rt-marker--unsure').length,
    sidebar: !!document.querySelector('.mc-rt-sidebar'),
  }));
  const waitRelays = (page, n) => page.waitForFunction((n) =>
    [...document.querySelectorAll('.mc-rt-sidebar .mc-rt-row')].filter(r => !/from payload/.test(r.textContent)).length === n, n);
  // Wait for the next sidebar: mark the current one, then wait for a fresh one.
  async function nextSidebar(page, action) {
    await page.evaluate(() => { const s = document.querySelector('.mc-rt-sidebar'); if (s) s.setAttribute('data-e2e-old', '1'); });
    await action();
    await page.waitForFunction(() => { const s = document.querySelector('.mc-rt-sidebar'); return s && !s.hasAttribute('data-e2e-old'); });
  }
  async function pickInCustomizer(page, mode) {
    await page.click('#customizeToggle');
    await page.waitForSelector('.cust-overlay:not(.hidden)');
    await page.click('.cust-tab[data-tab="display"]');
    await page.waitForSelector('#cv2-hide-1byte-hops');
    await page.selectOption('#cv2-hide-1byte-hops', mode);
    await page.click('#customizeToggle'); // close it again
  }

  console.log(`\n#674 E2E against ${BASE}\n`);

  let markChips = 0;
  await step('default is Mark: 1-byte badge, "N of 22 hops are uncertain", one chip and dashed marker per guess', async () => {
    const { ctx, page, errors } = await open(ONE_BYTE);
    try {
      await waitRelays(page, RELAYS);
      const s = await state(page);
      assert(s.mode === 'mark', 'mode ' + s.mode);
      assert(/Sent with: 1-byte/.test(s.notice), 'badge missing: ' + s.notice);
      const m = s.notice.match(/(\d+) of (\d+) hops are uncertain/);
      assert(m && Number(m[2]) === RELAYS, 'summary missing: ' + s.notice);
      assert(Number(m[1]) === s.chips && s.chips > 0, `summary says ${m[1]}, chips ${s.chips}`);
      assert(s.unsureMarkers > 0 && s.unsureMarkers <= s.chips, 'dashed markers: ' + s.unsureMarkers);
      const chipText = await page.$eval('.mc-rt-status-unsure', el => el.textContent + ' | ' + el.title);
      assert(/\? 1 of \d+/.test(chipText) && /Likeliest: .+\(\d+%\)/.test(chipText), chipText);
      markChips = s.chips;
      assert(errors.length === 0, 'page errors: ' + errors.join(' | '));
    } finally { await ctx.close(); }
  });

  await step('customizer select switches live: Show (no markings) → Hide (no 1-byte relays) → Mark', async () => {
    const { ctx, page, errors } = await open(ONE_BYTE);
    try {
      await waitRelays(page, RELAYS);
      await nextSidebar(page, () => pickInCustomizer(page, 'show'));
      let s = await state(page);
      assert(s.mode === 'show' && s.relays === RELAYS && !s.notice && s.chips === 0 && s.unsureMarkers === 0,
        'show: ' + JSON.stringify(s));
      await pickInCustomizer(page, 'hide');
      await page.waitForFunction(() => !document.querySelector('.mc-rt-sidebar'));
      s = await state(page);
      assert(s.mode === 'hide' && s.relays === 0, 'hide: ' + JSON.stringify(s));
      await pickInCustomizer(page, 'mark');
      await waitRelays(page, RELAYS);
      s = await state(page);
      assert(s.mode === 'mark' && s.chips === markChips, 'back to mark: ' + JSON.stringify(s));
      const stored = await page.evaluate(() => localStorage.getItem('meshcore-1byte-hops-mode'));
      assert(stored === null, 'the default is stored as no key, got ' + stored);
      assert(errors.length === 0, 'page errors: ' + errors.join(' | '));
    } finally { await ctx.close(); }
  });

  await step('the #1633 "hide" key migrates to Hide on load', async () => {
    const { ctx, page } = await open(ONE_BYTE, { storage: { 'meshcore-hide-1byte-hops': 'true' } });
    try {
      await page.waitForFunction(() => !!window.__mc_lastRouteDraw);
      const s = await page.evaluate(() => ({
        mode: window.MC_get1ByteHopsMode(),
        newKey: localStorage.getItem('meshcore-1byte-hops-mode'),
        oldKey: localStorage.getItem('meshcore-hide-1byte-hops'),
      }));
      assert(s.mode === 'hide' && s.newKey === 'hide' && s.oldKey === null, JSON.stringify(s));
    } finally { await ctx.close(); }
  });

  await step('control: a 2-byte route in Mark mode gets no 1-byte badge', async () => {
    const { ctx, page, errors } = await open(TWO_BYTE);
    try {
      await page.waitForFunction(() => document.querySelectorAll('.mc-rt-sidebar .mc-rt-row').length > 0);
      const s = await state(page);
      assert(!/Sent with: 1-byte/.test(s.notice), 'unexpected 1-byte badge: ' + s.notice);
      assert(errors.length === 0, 'page errors: ' + errors.join(' | '));
    } finally { await ctx.close(); }
  });

  await step('the notice follows the theme (light and dark) and its accent is the warn token', async () => {
    const { ctx, page } = await open(ONE_BYTE);
    try {
      await waitRelays(page, RELAYS);
      const look = {};
      for (const theme of ['light', 'dark']) {
        look[theme] = await page.evaluate(async (theme) => {
          const st = document.createElement('style');
          st.textContent = '*{transition:none!important}';
          document.head.appendChild(st);
          document.documentElement.setAttribute('data-theme', theme);
          await new Promise(r => requestAnimationFrame(() => r()));
          const n = document.querySelector('.mc-rt-onebyte');
          const probe = document.createElement('span');
          probe.style.color = 'var(--path-hash-warn)';
          document.body.appendChild(probe);
          const warn = getComputedStyle(probe).color;
          probe.remove();
          const c = getComputedStyle(n);
          return { bg: c.backgroundColor, accent: c.borderLeftColor, warn };
        }, theme);
      }
      for (const t of ['light', 'dark']) assert(look[t].accent === look[t].warn, t + ': ' + JSON.stringify(look[t]));
      assert(look.light.bg !== look.dark.bg, 'background does not follow the theme: ' + JSON.stringify(look));
    } finally { await ctx.close(); }
  });

  await step('375px phone: the route with markings does not scroll sideways', async () => {
    const { ctx, page, errors } = await open(ONE_BYTE, { mobile: true });
    try {
      await page.waitForFunction(() => !!document.querySelector('.mc-rt-sidebar .mc-rt-onebyte'));
      const w = await page.evaluate(() => ({ scroll: document.documentElement.scrollWidth, client: document.documentElement.clientWidth }));
      assert(w.scroll <= w.client + 1, 'horizontal overflow: ' + JSON.stringify(w));
      assert(errors.length === 0, 'page errors: ' + errors.join(' | '));
    } finally { await ctx.close(); }
  });

  await browser.close();
  console.log(`\n#674 E2E: ${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch(e => { console.error('  ✗ #674 E2E:', e.stack || e); process.exit(1); });
