/**
 * E2E (#741 customizer follow-up, AGENTS rule 8): the Dataforsyningen base
 * maps as the viewer's default base map in the customizer, on a phone
 * (375x560, touch) from a cold start, with real taps.
 *
 * The CI server has no map.tiles config, so /api/config/client is
 * intercepted and given the block an operator would set, with a dummy
 * token. Every api.dataforsyningen.dk request is answered by a stub in the
 * browser and recorded; every other third-party tile is stubbed too.
 * Nothing reaches Dataforsyningen and no real token exists in the run.
 *
 * Asserts:
 *   1. Gate on, the customizer opened before the client config arrives
 *      (a cold start on a slow phone): the Display tab's base-map lists
 *      pick up the Dataforsyningen styles in place when it lands — light:
 *      Skærmkort standard, grå, dæmpet and Natur- og Friluftskort; dark:
 *      Skærmkort grå — with no close/reopen.
 *   2. Picking Skærmkort dæmpet there swaps the Map in place to
 *      dtk_skaermkort_daempet; after a reload the choice is still selected
 *      in the customizer and the Map loads it (round trip).
 *   3. Reset All Customizations is offered with that choice as the only
 *      customization, and clears it: the Map swaps back to the built-in OSM
 *      base map in place and the key is gone.
 *   4. Gate off with a saved Dataforsyningen light and dark choice: the
 *      lists offer no Dataforsyningen style, the built-in defaults are
 *      selected, the Map shows OSM tiles in light and, after a theme switch
 *      from the More sheet, in dark — never a blank map — and nothing is
 *      requested from Dataforsyningen.
 *   5. The config landing while the viewer types in a customizer field
 *      (Branding → Site Name) keeps the focus and the next keystroke: the
 *      base-map lists are filled without rebuilding the panel.
 *   6. The config landing while a base-map select itself has the focus
 *      leaves that select alone; the lists fill once the focus leaves it.
 *
 * Usage:
 *   BASE_URL=http://localhost:13800 node test-issue-741-customizer-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13800';
const TOKEN = 'e2e-741-dummy-token';
const ON = { enabled: true, token: TOKEN };
const LIGHT_DF = ['dataforsyningen-skaermkort', 'dataforsyningen-skaermkort-graa',
  'dataforsyningen-skaermkort-daempet', 'dataforsyningen-natur-friluft'];
const DARK_DF = ['dataforsyningen-skaermkort-graa-dark'];
const DK = '#/map?lat=56.15&lon=10.2&zoom=12';
// A 1x1 grey PNG for every stubbed tile.
const PNG = Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGNoaGgAAAMEAYFL09IQAAAAAElFTkSuQmCC', 'base64');

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }
const dfIn = (ids) => ids.filter((id) => id.indexOf('dataforsyningen-') === 0);

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #741 customizer base maps (phone) against ${BASE} ===\n`);

  // holdConfig: /api/config/client is answered only when release() is called.
  async function newPhone({ tiles, storage = {}, theme = 'light', holdConfig = false }) {
    const ctx = await browser.newContext({ viewport: { width: 375, height: 560 }, hasTouch: true, isMobile: true, deviceScaleFactor: 2 });
    const rec = { wms: [], osm: [], errors: [] };
    let release = () => {};
    const gate = holdConfig ? new Promise((r) => { release = r; }) : Promise.resolve();
    await ctx.route(BASE + '/api/config/client', async (route) => {
      const response = await route.fetch();
      const config = await response.json();
      delete config.tiles;
      config.map = Object.assign({}, config.map, { tiles: tiles || {} });
      await gate;
      await route.fulfill({ response, json: config });
    });
    await ctx.route(/^https?:\/\/(?!localhost|127\.0\.0\.1)/i, async (route) => {
      const url = route.request().url();
      if (url.indexOf('https://api.dataforsyningen.dk/') === 0) rec.wms.push(url);
      else if (/tile\.openstreetmap\.org/.test(url)) rec.osm.push(url);
      if (route.request().resourceType() !== 'image') { await route.abort(); return; }
      await route.fulfill({ status: 200, contentType: 'image/png', body: PNG });
    });
    await ctx.addInitScript(({ t, st }) => {
      try {
        if (!sessionStorage.getItem('__e2e741c')) {
          sessionStorage.setItem('__e2e741c', '1');
          localStorage.setItem('meshcore-theme', t);
          for (const k of Object.keys(st)) localStorage.setItem(k, st[k]);
        }
      } catch (_) {}
    }, { t: theme, st: storage });
    const page = await ctx.newPage();
    page.on('pageerror', (e) => rec.errors.push(String(e)));
    page.on('console', (m) => { if (m.type() === 'error') rec.errors.push(m.text()); });
    page.on('dialog', (d) => d.accept());
    return { ctx, page, rec, release: () => release() };
  }

  async function settle(page) {
    await page.waitForSelector('.leaflet-container', { timeout: 20000 });
    await page.evaluate(() => window.MeshConfigReady).catch(() => {});
    await page.waitForTimeout(900);
  }
  // The viewer's way in on a phone: More, Customize, the Display tab.
  async function openDisplay(page) {
    await page.tap('[data-bottom-nav-tab="more"]');
    await page.waitForSelector('[data-mpa-mirror="customizeToggle"]', { state: 'visible', timeout: 5000 });
    await page.tap('[data-mpa-mirror="customizeToggle"]');
    await page.waitForSelector('.cust-overlay:not(.hidden)', { timeout: 5000 });
    await page.tap('.cust-tab[data-tab="display"]');
    await page.waitForSelector('.cust-panel[data-panel="display"].active', { timeout: 4000 });
  }
  async function closeCustomizer(page) {
    await page.tap('.cust-overlay .cust-close');
    await page.waitForSelector('.cust-overlay.hidden', { state: 'attached', timeout: 3000 });
  }
  const lists = (page) => page.evaluate(() => {
    const read = (id) => {
      const s = document.getElementById(id);
      return s ? { opts: Array.from(s.options).map((o) => o.value), value: s.value } : null;
    };
    return { light: read('cv2-light-tile-provider'), dark: read('cv2-dark-tile-provider') };
  });
  // Tap the select (a real touch on the control), then choose the option.
  async function pick(page, sel, id) {
    await page.locator(sel).scrollIntoViewIfNeeded();
    const box = await page.locator(sel).boundingBox();
    assert(box && box.x >= 0 && box.x + box.width <= 375, sel + ' not on screen: ' + JSON.stringify(box));
    await page.tap(sel);
    await page.selectOption(sel, id);
    await page.waitForTimeout(1000);
  }
  const tiles = (page) => page.evaluate(() => {
    const imgs = Array.from(document.querySelectorAll('.leaflet-tile-pane img.leaflet-tile'));
    const ok = (re) => imgs.filter((i) => re.test(i.src) && i.complete && i.naturalWidth > 0).length;
    return {
      wms: ok(/^https:\/\/api\.dataforsyningen\.dk\//),
      daempet: imgs.filter((i) => /LAYERS=dtk_skaermkort_daempet(&|$)/i.test(i.src)).length,
      osm: ok(/tile\.openstreetmap\.org/),
    };
  });

  // 1-3 — gate on, cold start
  await step('Phone 375x560, gate on, customizer opened before the config lands: the lists fill in place; dæmpet applies, round-trips a reload, and Reset All restores OSM', async () => {
    const { ctx, page, rec, release } = await newPhone({
      tiles: { providers: { dataforsyningen: ON } }, holdConfig: true,
    });
    try {
      await page.goto(`${BASE}/${DK}`, { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('[data-bottom-nav-tab="more"]', { state: 'visible', timeout: 20000 });
      await openDisplay(page);
      let l = await lists(page);
      assert(l.light && dfIn(l.light.opts).length === 0, 'offered before the config arrived: ' + JSON.stringify(l.light));
      release();
      await page.waitForFunction(() => window.MeshConfigReady !== undefined, null, { timeout: 10000 });
      await page.evaluate(() => window.MeshConfigReady).catch(() => {});
      await page.waitForTimeout(600);
      l = await lists(page);
      assert(JSON.stringify(dfIn(l.light.opts).sort()) === JSON.stringify(LIGHT_DF.slice().sort()), 'light list after the config: ' + l.light.opts.join(','));
      assert(JSON.stringify(dfIn(l.dark.opts)) === JSON.stringify(DARK_DF), 'dark list after the config: ' + l.dark.opts.join(','));
      assert(await page.isVisible('.cust-overlay:not(.hidden) .cust-panel[data-panel="display"].active'), 'the Display tab did not stay open');
      assert(rec.wms.length === 0, 'requested before a pick: ' + rec.wms.length);

      await pick(page, '#cv2-light-tile-provider', 'dataforsyningen-skaermkort-daempet');
      assert(await page.evaluate(() => localStorage.getItem('mc-light-tile-provider')) === 'dataforsyningen-skaermkort-daempet', 'not stored');
      await closeCustomizer(page);
      await page.waitForTimeout(800);
      let t = await tiles(page);
      assert(t.daempet > 0 && t.wms > 0, 'the Map did not swap to dæmpet in place: ' + JSON.stringify(t));

      await page.reload({ waitUntil: 'domcontentloaded' });
      await settle(page);
      t = await tiles(page);
      assert(t.daempet > 0 && t.wms > 0, 'dæmpet not loaded after a reload: ' + JSON.stringify(t));
      await openDisplay(page);
      l = await lists(page);
      assert(l.light.value === 'dataforsyningen-skaermkort-daempet', 'not selected after a reload: ' + l.light.value);

      await page.tap('.cust-tab[data-tab="export"]');
      await page.waitForSelector('#cv2ResetAll', { state: 'visible', timeout: 4000 });
      await page.tap('#cv2ResetAll');
      await page.waitForTimeout(800);
      assert(await page.evaluate(() => localStorage.getItem('mc-light-tile-provider')) === null, 'the light choice survived Reset All');
      await closeCustomizer(page);
      await page.waitForTimeout(1000);
      t = await tiles(page);
      assert(t.wms === 0 && t.osm > 0, 'the Map did not swap back to OSM after Reset All: ' + JSON.stringify(t));
      assert(rec.errors.length === 0, 'errors: ' + rec.errors.join(' | '));
    } finally { await ctx.close(); }
  });

  // 4 — gate off, stale saved choice
  for (const [name, dfCfg] of [['off', { enabled: false, token: TOKEN }], ['enabled without a token', { enabled: true }]]) {
    await step(`Phone 375x560, gate ${name}, saved Dataforsyningen light+dark choice: not offered, built-ins selected, OSM in light and dark, nothing requested`, async () => {
      const { ctx, page, rec } = await newPhone({
        tiles: { providers: { dataforsyningen: dfCfg } },
        storage: { 'mc-light-tile-provider': 'dataforsyningen-skaermkort-graa', 'mc-dark-tile-provider': 'dataforsyningen-skaermkort-graa-dark' },
      });
      try {
        await page.goto(`${BASE}/${DK}`, { waitUntil: 'domcontentloaded' });
        await settle(page);
        let t = await tiles(page);
        assert(t.osm > 0 && t.wms === 0, 'light map: ' + JSON.stringify(t));
        await openDisplay(page);
        const l = await lists(page);
        assert(dfIn(l.light.opts).length === 0 && dfIn(l.dark.opts).length === 0, 'offered while off: ' + JSON.stringify(l));
        assert(l.light.value === 'osm-standard' && l.dark.value === 'osm-dark', 'selected: ' + l.light.value + ' / ' + l.dark.value);
        await closeCustomizer(page);

        // Theme switch through the More sheet's dark-mode button.
        await page.tap('[data-bottom-nav-tab="more"]');
        await page.waitForSelector('.bottom-nav-sheet-darkbtn', { state: 'visible', timeout: 5000 });
        await page.tap('.bottom-nav-sheet-darkbtn');
        await page.waitForFunction(() => document.documentElement.getAttribute('data-theme') === 'dark', null, { timeout: 4000 });
        await page.waitForTimeout(1000);
        t = await tiles(page);
        assert(t.osm > 0 && t.wms === 0, 'dark map: ' + JSON.stringify(t));
        assert(rec.wms.length === 0, 'Dataforsyningen requests: ' + rec.wms.length);
        assert(rec.errors.length === 0, 'errors: ' + rec.errors.join(' | '));
      } finally { await ctx.close(); }
    });
  }

  // 5 — the config lands while the viewer types in the panel
  await step('Phone 375x560, gate on, typing in Site Name when the config lands: the focus and the next keystroke are kept, and the lists still fill', async () => {
    const { ctx, page, rec, release } = await newPhone({ tiles: { providers: { dataforsyningen: ON } }, holdConfig: true });
    try {
      await page.goto(`${BASE}/${DK}`, { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('[data-bottom-nav-tab="more"]', { state: 'visible', timeout: 20000 });
      await openDisplay(page);
      await page.tap('.cust-tab[data-tab="branding"]');
      const field = '.cust-panel[data-panel="branding"].active input[data-cv2-field="branding.siteName"]';
      await page.waitForSelector(field, { state: 'visible', timeout: 4000 });
      await page.tap(field);
      await page.evaluate((sel) => { document.querySelector(sel).__e2e741 = true; }, field);
      await page.keyboard.type('Ab');
      release();
      await page.waitForFunction(() => window.MeshConfigReady !== undefined, null, { timeout: 10000 });
      await page.evaluate(() => window.MeshConfigReady).catch(() => {});
      await page.waitForTimeout(600);
      const focus = await page.evaluate(() => {
        const a = document.activeElement;
        return { same: !!(a && a.__e2e741), tag: a && a.tagName };
      });
      assert(focus.same, 'the Site Name input lost the focus when the config landed (active: ' + focus.tag + ')');
      await page.keyboard.type('c');
      const value = await page.evaluate(() => document.activeElement && document.activeElement.value);
      assert(/Abc$/.test(value || ''), 'a keystroke was lost: ' + JSON.stringify(value));
      const l = await lists(page);
      assert(JSON.stringify(dfIn(l.light.opts).sort()) === JSON.stringify(LIGHT_DF.slice().sort()), 'light list after the config: ' + l.light.opts.join(','));
      assert(JSON.stringify(dfIn(l.dark.opts)) === JSON.stringify(DARK_DF), 'dark list after the config: ' + l.dark.opts.join(','));
      assert(rec.errors.length === 0, 'errors: ' + rec.errors.join(' | '));
    } finally { await ctx.close(); }
  });

  // 6 — the config lands while a base-map select has the focus
  await step('Phone 375x560, gate on, the light select focused when the config lands: it keeps the focus, and the lists fill when the focus leaves', async () => {
    const { ctx, page, rec, release } = await newPhone({ tiles: { providers: { dataforsyningen: ON } }, holdConfig: true });
    try {
      await page.goto(`${BASE}/${DK}`, { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('[data-bottom-nav-tab="more"]', { state: 'visible', timeout: 20000 });
      await openDisplay(page);
      await page.locator('#cv2-light-tile-provider').scrollIntoViewIfNeeded();
      await page.focus('#cv2-light-tile-provider');
      await page.evaluate(() => { document.getElementById('cv2-light-tile-provider').__e2e741 = true; });
      release();
      await page.waitForFunction(() => window.MeshConfigReady !== undefined, null, { timeout: 10000 });
      await page.evaluate(() => window.MeshConfigReady).catch(() => {});
      await page.waitForTimeout(600);
      assert(await page.evaluate(() => !!(document.activeElement && document.activeElement.__e2e741)), 'the focused select was replaced under the viewer');
      // The focus leaves the select.
      await page.evaluate(() => document.activeElement.blur());
      await page.waitForTimeout(300);
      const l = await lists(page);
      assert(JSON.stringify(dfIn(l.light.opts).sort()) === JSON.stringify(LIGHT_DF.slice().sort()), 'light list after the focus left: ' + l.light.opts.join(','));
      assert(JSON.stringify(dfIn(l.dark.opts)) === JSON.stringify(DARK_DF), 'dark list after the focus left: ' + l.dark.opts.join(','));
      // The rebuilt selects still work: a pick is stored.
      await pick(page, '#cv2-light-tile-provider', 'dataforsyningen-skaermkort-graa');
      assert(await page.evaluate(() => localStorage.getItem('mc-light-tile-provider')) === 'dataforsyningen-skaermkort-graa', 'a pick on the rebuilt select was not stored');
      assert(rec.errors.length === 0, 'errors: ' + rec.errors.join(' | '));
    } finally { await ctx.close(); }
  });

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
