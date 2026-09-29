/**
 * E2E (#125): every persisted Live view toggle is restored and wired before
 * Live init awaits anything.
 *
 * live.js renders the controls panel, then awaits /api/config/map and
 * loadNodes(). #88 moved only "Multibyte only" ahead of those awaits; the
 * other persisted toggles (Heat, Inferred/ghost hops, Realistic, Color by
 * hash, Favorites, Foreign, Matrix, Rain) were restored and wired after
 * them, so during init they showed their default state and a click made then
 * was not saved and was reverted when init caught up.
 *
 * Same hold technique as test-live-multibyte-only-e2e.js: /api/config/map is
 * intercepted, which freezes init at its first await right after the panel is
 * rendered; window._liveWSHandler() marks a finished init.
 *
 * Checks, per toggle: the saved (non-default) value is shown while init is
 * held; a click then saves at once, exactly once, and survives init. Then
 * the effects that need the map: a Heat click during init decides whether
 * the heat layer exists after init; Matrix saved or clicked ON disables Heat
 * from the first paint and applies the theme after init; Rain clicked ON
 * shows its canvas. Finally three SPA round trips leave one listener per
 * toggle (one click writes once).
 *
 * Usage: BASE_URL=http://localhost:13581 node test-issue-125-live-toggles-early-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const ORIGIN = new URL(BASE).origin;
let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

// id, localStorage key, value shown when nothing is saved
const TOGGLES = [
  { id: 'liveHeatToggle', key: 'meshcore-live-heatmap', dflt: true },
  { id: 'liveGhostToggle', key: 'live-ghost-hops', dflt: true },
  { id: 'liveRealisticToggle', key: 'live-realistic-propagation', dflt: false },
  { id: 'liveColorHashToggle', key: 'meshcore-color-packets-by-hash', dflt: true },
  { id: 'liveFavoritesToggle', key: 'live-favorites-only', dflt: false },
  { id: 'liveForeignToggle', key: 'live-highlight-foreign', dflt: false },
  { id: 'liveMatrixToggle', key: 'live-matrix-mode', dflt: false },
  { id: 'liveMatrixRainToggle', key: 'live-matrix-rain', dflt: false },
];
const KEYS = TOGGLES.map((t) => t.key);

// A context whose first page load starts from `seed` ({key: 'true'|'false'};
// other toggle keys removed), counting writes per key and recording errors.
async function newLivePage(browser, errors, seed) {
  const ctx = await browser.newContext({ viewport: { width: 1400, height: 900 } });
  const page = await ctx.newPage();
  page.setDefaultTimeout(15000);
  page.on('pageerror', (e) => errors.push('pageerror: ' + e.message));
  page.on('console', (m) => {
    if (m.type() !== 'error') return;
    const url = (m.location() && m.location().url) || '';
    if (url && !url.startsWith(ORIGIN)) return; // external tile/CDN noise
    errors.push('console.error: ' + m.text());
  });
  await page.addInitScript(([keys, seedObj]) => {
    if (!sessionStorage.getItem('t125-seeded')) {
      sessionStorage.setItem('t125-seeded', '1');
      for (const k of keys) {
        if (Object.prototype.hasOwnProperty.call(seedObj, k)) localStorage.setItem(k, seedObj[k]);
        else localStorage.removeItem(k);
      }
    }
    window.__t125Writes = {};
    const setItem = Storage.prototype.setItem;
    Storage.prototype.setItem = function (k, v) {
      if (this === window.localStorage && keys.includes(k)) window.__t125Writes[k] = (window.__t125Writes[k] || 0) + 1;
      return setItem.call(this, k, v);
    };
    window.addEventListener('unhandledrejection', (e) => {
      console.error('unhandledrejection: ' + (e.reason && e.reason.message || e.reason));
    });
  }, [KEYS, seed || {}]);
  return { ctx, page };
}

async function holdLiveInit(page) {
  let release, hit;
  const released = new Promise((r) => { release = r; });
  const intercepted = new Promise((r) => { hit = r; });
  await page.route('**/api/config/map', async (route) => {
    hit();
    await released;
    await route.continue().catch((e) => { if (!page.isClosed()) throw e; });
  }, { times: 1 });
  return {
    release,
    async reached() {
      let timer;
      const timeout = new Promise((_, rej) => { timer = setTimeout(() => rej(new Error(
        'Live init never requested /api/config/map within 15s; the hold point this test relies on has moved')), 15000); });
      try { await Promise.race([intercepted, timeout]); } finally { clearTimeout(timer); }
    },
  };
}

async function readState(page) {
  return page.evaluate((toggles) => {
    const out = { liveReady: !!window._liveWSHandler(), writes: Object.assign({}, window.__t125Writes), t: {} };
    for (const t of toggles) {
      const cb = document.getElementById(t.id);
      out.t[t.id] = { exists: !!cb, checked: cb ? cb.checked : null, disabled: cb ? cb.disabled : null, stored: localStorage.getItem(t.key) };
    }
    const mapEl = document.getElementById('liveMap');
    out.matrixTheme = !!(mapEl && mapEl.classList.contains('matrix-theme'));
    out.heatLayer = !!document.querySelector('#liveMap .leaflet-heatmap-layer');
    out.rainCanvas = !!document.getElementById('matrixRainCanvas');
    return out;
  }, TOGGLES);
}

// load() with init held, inspect(page, state) while held, then release and
// wait for init to finish (even when a check failed).
async function withInitHeld(page, load, inspect) {
  const hold = await holdLiveInit(page);
  let failure = null;
  try {
    await load();
    await hold.reached();
    await page.locator('#liveHeatToggle').waitFor({ state: 'attached' });
    const st = await readState(page);
    assert(!st.liveReady, 'Live finished init while it should be held; the readiness signal moved');
    await inspect(page, st);
  } catch (e) {
    failure = e;
  } finally {
    hold.release();
  }
  const ready = await page.waitForFunction(() => !!window._liveWSHandler(), null, { timeout: 15000 }).then(() => true, () => false);
  if (failure) throw failure;
  if (!ready) throw new Error('Live init did not finish after it was released');
}

// A user click while init is held: the controls body may be collapsed, so
// click the input programmatically the same way a label click would.
async function clickDuringInit(page, id) {
  await page.evaluate((i) => document.getElementById(i).click(), id);
}

const loadLive = (page) => () => page.goto(BASE + '/#/live', { waitUntil: 'domcontentloaded' });

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  const errors = [];
  console.log('\n=== #125 live toggles wired before init awaits, against ' + BASE + ' ===');

  await step('with nothing saved, every toggle shows its default while init is held', async () => {
    const { ctx, page } = await newLivePage(browser, errors, {});
    try {
      await withInitHeld(page, loadLive(page), async (p, st) => {
        for (const t of TOGGLES) {
          assert(st.t[t.id].exists, t.id + ' missing');
          assert(st.t[t.id].checked === t.dflt, t.id + ' shows ' + st.t[t.id].checked + ' during init, default is ' + t.dflt);
        }
      });
      const st = await readState(page);
      for (const t of TOGGLES) assert(!st.writes[t.key], 'init wrote ' + t.key + ' on its own');
    } finally { await ctx.close(); }
  });

  for (const t of TOGGLES) {
    await step(t.id + ': the saved value is shown during init, and a click then is saved once and kept', async () => {
      const saved = String(!t.dflt);
      const { ctx, page } = await newLivePage(browser, errors, { [t.key]: saved });
      const want = t.dflt; // the click flips the saved (non-default) value back
      try {
        await withInitHeld(page, loadLive(page), async (p, st) => {
          assert(st.t[t.id].checked === !t.dflt, t.id + ' shows ' + st.t[t.id].checked + ' during init, saved ' + saved);
          await clickDuringInit(p, t.id);
          const after = await readState(p);
          assert(after.t[t.id].checked === want, 'the click did not flip ' + t.id);
          assert(after.t[t.id].stored === String(want), 'a click during init must save at once (stored=' + after.t[t.id].stored + ')');
          assert(after.writes[t.key] === 1, 'one click must write ' + t.key + ' once (writes=' + after.writes[t.key] + ')');
        });
        const st = await readState(page);
        assert(st.t[t.id].checked === want, 'init reverted the click on ' + t.id);
        assert(st.t[t.id].stored === String(want), 'setting changed after init (stored=' + st.t[t.id].stored + ')');
      } finally { await ctx.close(); }
    });
  }

  await step('Heat clicked OFF during init: no heat layer after init; clicked back ON: layer shown', async () => {
    const { ctx, page } = await newLivePage(browser, errors, { 'meshcore-live-heatmap': 'true' });
    try {
      await withInitHeld(page, loadLive(page), async (p) => { await clickDuringInit(p, 'liveHeatToggle'); });
      let st = await readState(page);
      assert(!st.heatLayer, 'heat layer present after init although Heat was switched off during init');
      await page.evaluate(() => document.getElementById('liveHeatToggle').click());
      st = await readState(page);
      assert(st.heatLayer, 'heat layer missing after switching Heat on after init');
    } finally { await ctx.close(); }
  });

  await step('Matrix saved ON: Heat unchecked and disabled from the first paint; theme applied after init', async () => {
    const { ctx, page } = await newLivePage(browser, errors, { 'live-matrix-mode': 'true', 'meshcore-live-heatmap': 'true' });
    try {
      await withInitHeld(page, loadLive(page), async (p, st) => {
        assert(st.t.liveHeatToggle.checked === false && st.t.liveHeatToggle.disabled === true,
          'with Matrix saved ON, Heat must be unchecked and disabled during init (checked=' + st.t.liveHeatToggle.checked + ', disabled=' + st.t.liveHeatToggle.disabled + ')');
      });
      const st = await readState(page);
      assert(st.matrixTheme, 'matrix theme not applied after init');
      assert(!st.heatLayer, 'heat layer shown while Matrix is on');
      assert(st.t.liveHeatToggle.disabled, 'Heat enabled while Matrix is on');
    } finally { await ctx.close(); }
  });

  await step('Matrix and Rain clicked ON during init: interlock at once, theme and rain after init; Matrix OFF re-enables Heat', async () => {
    const { ctx, page } = await newLivePage(browser, errors, {});
    try {
      await withInitHeld(page, loadLive(page), async (p) => {
        await clickDuringInit(p, 'liveMatrixToggle');
        await clickDuringInit(p, 'liveMatrixRainToggle');
        const st = await readState(p);
        assert(st.t.liveHeatToggle.disabled && !st.t.liveHeatToggle.checked, 'Matrix clicked ON did not disable Heat during init');
      });
      let st = await readState(page);
      assert(st.matrixTheme, 'matrix theme not applied after init');
      assert(st.rainCanvas, 'rain canvas missing after init');
      assert(!st.heatLayer, 'heat layer shown while Matrix is on');
      await page.evaluate(() => document.getElementById('liveMatrixToggle').click());
      st = await readState(page);
      assert(!st.matrixTheme, 'matrix theme still applied after switching Matrix off');
      assert(!st.t.liveHeatToggle.disabled, 'Heat still disabled after switching Matrix off');
    } finally { await ctx.close(); }
  });

  await step('three SPA round trips keep one listener per toggle', async () => {
    const { ctx, page } = await newLivePage(browser, errors, {});
    try {
      await page.goto(BASE + '/#/live', { waitUntil: 'domcontentloaded' });
      await page.waitForFunction(() => !!window._liveWSHandler(), null, { timeout: 15000 });
      for (let i = 0; i < 3; i++) {
        await page.evaluate(() => { location.hash = '#/packets'; });
        await page.waitForSelector('#pktTable', { state: 'attached' });
        await page.evaluate(() => { location.hash = '#/live'; });
        await page.waitForFunction(() => !!window._liveWSHandler(), null, { timeout: 15000 });
      }
      for (const t of TOGGLES) {
        if (t.id === 'liveHeatToggle') continue; // disabled while Matrix is on; covered above
        const before = (await readState(page)).writes[t.key] || 0;
        await page.evaluate((id) => document.getElementById(id).click(), t.id);
        const after = (await readState(page)).writes[t.key] || 0;
        assert(after - before === 1, 'one click on ' + t.id + ' after 3 round trips wrote ' + (after - before) + ' times');
      }
    } finally { await ctx.close(); }
  });

  await step('no page errors, console errors or unhandled rejections', async () => {
    assert(errors.length === 0, errors.slice(0, 5).join(' | '));
  });

  await browser.close();
  console.log('\n--- ' + passed + ' passed, ' + failed + ' failed ---');
  process.exit(failed ? 1 : 0);
})();
