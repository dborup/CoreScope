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
    dashedEdges: document.querySelectorAll('path.mc-rt-edge[stroke-dasharray]').length,
    // Every dashed marker belongs to a row with a chip, and every chip row
    // that has a marker on the map has a dashed one.
    markerMismatch: (() => {
      const chipIdx = new Set([...document.querySelectorAll('.mc-rt-sidebar .mc-rt-row')]
        .filter(r => r.querySelector('.mc-rt-status-unsure')).map(r => r.getAttribute('data-hop-idx')));
      const bad = [];
      document.querySelectorAll('.mc-rt-marker[data-hop-idx]').forEach(m => {
        const idx = m.getAttribute('data-hop-idx');
        if (m.classList.contains('mc-rt-marker--unsure') !== chipIdx.has(idx)) bad.push(idx);
      });
      return bad;
    })(),
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
      assert(s.unsureMarkers > 0, 'no dashed marker');
      assert(s.markerMismatch.length === 0, 'markers and chips disagree at hop idx ' + s.markerMismatch.join(','));
      const chip = await page.$eval('.mc-rt-status-unsure', el => ({ title: el.title, visible: el.querySelector('[aria-hidden="true"]').textContent, sr: el.querySelector('.sr-only').textContent }));
      assert(/^\? 1 of \d+$/.test(chip.visible), 'chip text: ' + chip.visible);
      assert(/Likeliest: .+\(\d+%\)/.test(chip.title) && chip.sr === chip.title, 'tooltip and screen-reader text: ' + JSON.stringify(chip));
      assert(errors.length === 0, 'page errors: ' + errors.join(' | '));
    } finally { await ctx.close(); }
  });

  await step('customizer select switches live: Show (no markings) → Hide (no 1-byte relays) → Mark', async () => {
    const { ctx, page, errors } = await open(ONE_BYTE);
    try {
      await waitRelays(page, RELAYS);
      const mark = await state(page);
      assert(mark.chips > 0, 'baseline: no chips in Mark');
      await nextSidebar(page, () => pickInCustomizer(page, 'show'));
      let s = await state(page);
      assert(s.mode === 'show' && s.relays === RELAYS && !s.notice && s.chips === 0 && s.unsureMarkers === 0,
        'show: ' + JSON.stringify(s));
      assert(mark.dashedEdges > s.dashedEdges,
        `Mark must dash the segments of uncertain hops (mark ${mark.dashedEdges}, show ${s.dashedEdges})`);
      // Hide: the sidebar is redrawn with only payload endpoints, or removed
      // when none survives — either way not the old one.
      await page.evaluate(() => { const x = document.querySelector('.mc-rt-sidebar'); if (x) x.setAttribute('data-e2e-old', '1'); });
      await pickInCustomizer(page, 'hide');
      await page.waitForFunction(() => { const x = document.querySelector('.mc-rt-sidebar'); return !x || !x.hasAttribute('data-e2e-old'); });
      s = await state(page);
      assert(s.mode === 'hide' && s.relays === 0, 'hide: ' + JSON.stringify(s));
      await pickInCustomizer(page, 'mark');
      await waitRelays(page, RELAYS);
      s = await state(page);
      assert(s.mode === 'mark' && s.chips === mark.chips, 'back to mark: ' + JSON.stringify(s));
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

  await step('isolating one observer\'s path keeps the markings and recounts the notice for that path', async () => {
    const { ctx, page, errors } = await open(ONE_BYTE);
    await ctx.route(url => url.pathname === '/api/packets/57224a4a01c9c682', async route => {
      const res = await route.fetch();
      const body = await res.json();
      const first = body.observations.find(o => String(o.id) === '147') || body.observations[0];
      body.observations = [first, Object.assign({}, first, {
        id: 9990147, observer_id: 'E2E674SECONDOBSERVER', observer_name: 'E2E674 second observer',
        path_json: JSON.stringify(JSON.parse(first.path_json).slice(0, 10)), resolved_path: null,
      })];
      body.observation_count = 2;
      await route.fulfill({ response: res, json: body });
    });
    try {
      await page.reload({ waitUntil: 'load' });
      await page.waitForFunction(() => window.__mc_lastRouteDraw && window.__mc_lastRouteDraw.kind === 'multi' &&
        document.querySelectorAll('.mc-rt-path-row').length >= 2);
      await page.click('.mc-rt-path-row .mc-rt-path-obs[title*="E2E674 second observer"]');
      await page.waitForFunction(() => document.querySelectorAll('.mc-rt-row-iso').length > 0);
      const iso = await page.evaluate(() => ({
        rows: document.querySelectorAll('.mc-rt-row-iso').length,
        chips: document.querySelectorAll('.mc-rt-row-iso .mc-rt-status-unsure').length,
        dashedIso: document.querySelectorAll('path.mc-rt-edge-iso[stroke-dasharray]').length,
        notice: (document.querySelector('.mc-rt-onebyte-summary') || {}).textContent || '',
      }));
      assert(iso.chips > 0, 'isolated rows lost their chips: ' + JSON.stringify(iso));
      assert(iso.dashedIso > 0, 'isolated path has no dashed segment: ' + JSON.stringify(iso));
      const m = iso.notice.match(/(\d+) of (\d+) hops are uncertain/);
      assert(m && Number(m[1]) === iso.chips && Number(m[2]) <= 10, 'notice must describe the isolated path: ' + JSON.stringify(iso));
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
      const summary = await page.$eval('.mc-rt-mobile-summary', el => el.textContent);
      assert(/\d+ uncertain/.test(summary), 'the collapsed mobile summary must carry the uncertain count: ' + summary);
      assert(errors.length === 0, 'page errors: ' + errors.join(' | '));
    } finally { await ctx.close(); }
  });

  await browser.close();
  console.log(`\n#674 E2E: ${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch(e => { console.error('  ✗ #674 E2E:', e.stack || e); process.exit(1); });
