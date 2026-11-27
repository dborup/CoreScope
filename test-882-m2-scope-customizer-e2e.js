/**
 * E2E (#882 M2): the Live scope palette and the scope legend window in the
 * customizer, and what they do to the Live page.
 *
 *   1. Defaults unchanged: a fresh page draws with the stylesheet palette;
 *      picking a theme preset (Ocean) leaves every --scope-* as it was.
 *   2. Light: Theme -> Live scope colours, a new colour for the slot a scope
 *      holds recolours its legend swatch at once and the next line drawn;
 *      the pick is saved per browser.
 *   3. Dark: the same tab edits the dark palette; the dark pick draws in
 *      dark, the light pick again after switching back.
 *   4. A colourblind preset recolours the slots nobody picked; a pick wins.
 *   5. Display -> Live scope legend: 5 min and 3 rows retitle the legend and
 *      cap it ("+N more"); out-of-range input is put back; reload keeps it.
 *   6. Export / import round trip through the Export tab's JSON.
 *   7. Phone 375x560, cold start, touch: More -> Customize, Display and
 *      Theme sections reached by real taps inside the screen, in light and
 *      dark; then Live with "Color by scope" (real tap) draws the new colour.
 *
 * /api/nodes is served by request interception so the two route endpoints
 * are known nodes at the viewport centre (as in test-live-scope-color-e2e.js).
 *
 * Usage: BASE_URL=http://localhost:13581 node test-882-m2-scope-customizer-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
if (!['localhost', '127.0.0.1', '[::1]'].includes(new URL(BASE).hostname)) throw new Error('local server only');
const SHOTS = process.env.E2E_SHOT_DIR || '';

let passed = 0, failed = 0;
let curPage = null; // for a screenshot of a failed step (E2E_SHOT_DIR)
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) {
    failed++; console.error('  ✗ ' + name + ': ' + (e && e.stack || e));
    if (SHOTS && curPage) await curPage.screenshot({ path: SHOTS + '/fail-' + failed + '.png' }).catch(() => {});
  }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

const A = 'e2e882m2' + 'a'.repeat(56);
const B = 'e2e882m2' + 'b'.repeat(56);
const VIEW = 'lat=50.5&lon=10.5&zoom=11';
const KEY = (slot) => 'scope' + (slot + 1);

function nodesBody() {
  const now = new Date().toISOString();
  const node = (pk, name, lat, lon) => ({ public_key: pk, name, role: 'repeater', lat, lon, last_seen: now, last_heard: now, first_seen: now, advert_count: 3 });
  const nodes = [node(A, 'E2E-882M2 A', 50.5, 10.5), node(B, 'E2E-882M2 B', 50.51, 10.52)];
  return JSON.stringify({ nodes, total: nodes.length, counts: {} });
}

async function open(browser, opts) {
  opts = opts || {};
  const ctx = await browser.newContext(Object.assign({ viewport: opts.viewport || { width: 1400, height: 900 } }, opts.touch ? { hasTouch: true, isMobile: true } : {}));
  await ctx.addInitScript((theme) => {
    try {
      if (!sessionStorage.getItem('e2e882m2')) {
        localStorage.removeItem('live-legend-hidden');
        // Live auto-hides the top nav after 4 s (#62); pinned, the palette
        // button stays where a viewer would reach for it.
        localStorage.setItem('live-nav-pinned', 'true');
        localStorage.setItem('meshcore-theme', theme);
        sessionStorage.setItem('e2e882m2', '1');
      }
    } catch (_) { /* storage off */ }
  }, opts.theme || 'light');
  const page = await ctx.newPage();
  curPage = page;
  const h = { ctx, page, errors: [] };
  page.on('pageerror', (e) => h.errors.push(String(e.stack || e.message).split('\n').slice(0, 3).join(' | ')));
  await page.route(/\/api\/nodes\?/, (route) => route.fulfill({ status: 200, contentType: 'application/json', body: nodesBody() }));
  h.ready = async () => {
    await page.waitForFunction((k) => {
      const m = window._liveNodeMarkers && window._liveNodeMarkers();
      return !!(window._liveWSHandler && window._liveWSHandler()) && m && m[k];
    }, A, { timeout: 20000 });
    await page.waitForFunction(() => window._customizerV2 && window._customizerV2.initDone, null, { timeout: 15000 });
  };
  h.goto = async (query) => {
    await page.goto(BASE + '/?r=' + Date.now() + '#/live?' + (query ? query + '&' : '') + VIEW, { waitUntil: 'domcontentloaded' });
    await h.ready();
  };
  let seq = 0;
  h.send = async (scope) => {
    const hash = 'e2e882m2' + (++seq) + 'x' + Date.now().toString(16);
    await page.evaluate((a) => window._liveWSHandler()({ type: 'packet', data: {
      id: 882200 + a.seq, hash: a.hash, raw_hex: '1500', route_type: 0, payload_type: 5,
      scope_name: a.scope, path_json: '["E2","E2"]', resolved_path: [a.a, a.b],
      observer_id: 'e2e-882m2', observer_name: 'e2e-882m2', timestamp: new Date().toISOString(), snr: 5, rssi: -90,
      decoded: { header: { payloadTypeName: 'GRP_TXT' }, payload: { text: 'x' }, path: { hops: [] } },
    } }), { seq, hash, scope, a: A, b: B });
    return hash;
  };
  h.routeColor = async (hash) => {
    await page.waitForFunction((x) => window._liveClickablePaths.some((e) => e.hash === x), hash, { timeout: 8000 });
    return page.evaluate((x) => window._liveClickablePaths.find((e) => e.hash === x).color, hash);
  };
  h.cssVar = (n) => page.evaluate((v) => getComputedStyle(document.documentElement).getPropertyValue(v).trim(), n);
  h.slot = (name) => page.evaluate((n) => window._liveScopeRegistry().slotOf(n), name);
  h.legend = async () => {
    await page.waitForTimeout(1300); // the legend re-render is coalesced to <= 1/s
    return page.evaluate(() => ({
      title: document.querySelector('#scopeLegendSection .legend-title').textContent,
      rows: [...document.querySelectorAll('#scopeLegendList li')].map((li) => {
        const sw = li.querySelector('.live-scope-swatch');
        return { text: li.textContent.trim(), sw: sw ? getComputedStyle(sw).backgroundColor : null };
      }),
    }));
  };
  // The palette button, or on a phone the More sheet, then Customize, then a tab.
  h.openTab = async (tab, phone) => {
    if (!(await page.$('.cust-overlay:not(.hidden)'))) {
      if (phone) {
        await page.tap('[data-bottom-nav-tab="more"]');
        await page.waitForSelector('[data-mpa-mirror="customizeToggle"]', { state: 'visible', timeout: 5000 });
        await page.tap('[data-mpa-mirror="customizeToggle"]');
      } else await page.click('#customizeToggle');
      await page.waitForSelector('.cust-overlay:not(.hidden)', { timeout: 5000 });
    }
    if (phone) await page.tap('.cust-tab[data-tab="' + tab + '"]'); else await page.click('.cust-tab[data-tab="' + tab + '"]');
    await page.waitForSelector('.cust-panel[data-panel="' + tab + '"].active', { timeout: 4000 });
  };
  h.openScopeColours = async (phone) => {
    await h.openTab('theme', phone);
    const open = await page.$eval('.cust-scope-colors', (d) => d.open);
    if (!open) {
      const sum = page.locator('.cust-scope-colors > summary');
      await sum.scrollIntoViewIfNeeded();
      if (phone) await sum.tap(); else await sum.click();
    }
  };
  // A native colour picker cannot be driven, so the value is filled; the
  // change event is the one the picker fires when it closes.
  h.pickColour = async (section, key, hex) => {
    const sel = '[data-cv2-field="' + section + '.' + key + '"]';
    await page.locator(sel).scrollIntoViewIfNeeded();
    await page.fill(sel, hex);
    await page.dispatchEvent(sel, 'change');
    await page.waitForTimeout(700); // the debounced write
  };
  h.setNum = async (key, v) => {
    const sel = '[data-cv2-live-scope="' + key + '"]';
    await page.fill(sel, String(v));
    await page.dispatchEvent(sel, 'change');
    await page.waitForTimeout(700);
  };
  h.closePanel = async (phone) => {
    if (!(await page.$('.cust-overlay:not(.hidden)'))) return;
    if (phone) await page.tap('.cust-close'); else await page.click('.cust-close');
    await page.waitForTimeout(200);
  };
  h.scopeOn = async () => {
    if (!(await page.$eval('#liveColorScopeToggle', (e) => e.checked))) throw new Error('scope mode not on');
  };
  h.overrides = () => page.evaluate(() => JSON.parse(localStorage.getItem('cs-theme-overrides') || '{}'));
  return h;
}

function rgb(hex) {
  const n = parseInt(hex.slice(1), 16);
  return 'rgb(' + (n >> 16) + ', ' + ((n >> 8) & 255) + ', ' + (n & 255) + ')';
}

const M1_LIGHT_1 = '#0072b2', M1_DARK_1 = '#3d8bfd';

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log('\n=== #882 M2 scope palette + legend window in the customizer, against ' + BASE + ' ===');
  try {
    const h = await open(browser);
    const st = {};

    await step('defaults unchanged: stylesheet palette; a theme preset (Ocean) leaves --scope-* alone', async () => {
      await h.goto('color=scope');
      await h.scopeOn();
      const before = {};
      for (const v of ['--scope-1', '--scope-7', '--scope-none', '--scope-unknown', '--scope-other']) before[v] = await h.cssVar(v);
      assert(before['--scope-1'] === M1_LIGHT_1, 'default --scope-1 ' + before['--scope-1']);
      const title = (await h.legend()).title;
      assert(title === 'SCOPES (last 15 min)', 'default title ' + title);
      await h.openTab('theme');
      await h.page.click('.cust-preset-btn[data-preset="ocean"]');
      await h.page.waitForTimeout(400);
      for (const v of Object.keys(before)) assert(await h.cssVar(v) === before[v], v + ' moved with the Ocean preset');
      assert(await h.page.evaluate(() => !document.documentElement.style.getPropertyValue('--scope-1')), 'Ocean stamped --scope-1 inline');
      assert(await h.page.evaluate(() => window._customizerV2.detectActivePreset()) === 'ocean', 'Ocean not detected');
      // Back to Default (clears every override).
      await h.page.click('.cust-preset-btn[data-preset="default"]');
      await h.page.waitForTimeout(400);
      await h.closePanel();
    });

    await step('light: a new colour for a scope\'s slot recolours its swatch at once and the next line', async () => {
      const first = await h.send('#dk');
      st.slot = await h.slot('#dk');
      st.var = '--scope-' + (st.slot + 1);
      const old = await h.routeColor(first);
      assert(old === await h.cssVar(st.var), 'line before ' + old);
      await h.openScopeColours();
      const rows = await h.page.$$eval('.cust-scope-colors [data-cv2-field]', (els) => els.map((e) => e.getAttribute('data-cv2-field')));
      assert(rows.length === 15 && rows.every((r) => r.startsWith('theme.')), 'rows ' + JSON.stringify(rows));
      await h.pickColour('theme', KEY(st.slot), '#123456');
      assert(await h.cssVar(st.var) === '#123456', st.var + ' = ' + await h.cssVar(st.var));
      await h.closePanel();
      const L = await h.legend();
      const dk = L.rows.find((r) => r.text === '#dk');
      assert(dk && dk.sw === rgb('#123456'), 'swatch ' + JSON.stringify(L.rows));
      const next = await h.routeColor(await h.send('#dk'));
      assert(next === '#123456', 'next line ' + next);
      assert(await h.routeColor(first) === old, 'a drawn line was recoloured (not expected: lines keep their colour)');
      const ov = await h.overrides();
      assert(ov.theme && ov.theme[KEY(st.slot)] === '#123456', 'not saved: ' + JSON.stringify(ov));
    });

    await step('dark: the tab edits the dark palette; each theme draws its own pick', async () => {
      await h.page.evaluate(() => document.documentElement.setAttribute('data-theme', 'dark'));
      await h.page.waitForTimeout(300);
      // A light pick with no dark one carries over, as for every theme key.
      assert(await h.cssVar(st.var) === '#123456', 'dark before its own pick: ' + await h.cssVar(st.var));
      assert(await h.cssVar('--scope-unknown') === '#c4b5a5', 'dark default unknown ' + await h.cssVar('--scope-unknown'));
      await h.openScopeColours();
      assert(await h.page.$('.cust-scope-colors [data-cv2-field="themeDark.' + KEY(st.slot) + '"]'), 'dark mode does not edit themeDark');
      await h.pickColour('themeDark', KEY(st.slot), '#abcdef');
      await h.closePanel();
      assert(await h.routeColor(await h.send('#dk')) === '#abcdef', 'dark line');
      const L = await h.legend();
      assert(L.rows.find((r) => r.text === '#dk').sw === rgb('#abcdef'), 'dark swatch');
      if (SHOTS) await h.page.screenshot({ path: SHOTS + '/desktop-dark-legend.png' });
      await h.page.evaluate(() => document.documentElement.setAttribute('data-theme', 'light'));
      await h.page.waitForTimeout(300);
      assert(await h.routeColor(await h.send('#dk')) === '#123456', 'light line after switching back');
      const ov = await h.overrides();
      assert(ov.themeDark && ov.themeDark[KEY(st.slot)] === '#abcdef', 'dark pick not saved');
      if (SHOTS) await h.page.screenshot({ path: SHOTS + '/desktop-light-legend.png' });
    });

    await step('a colourblind preset recolours the slots nobody picked; a pick still wins', async () => {
      const other = st.slot === 0 ? 1 : 0;
      const before = await h.cssVar('--scope-' + (other + 1));
      await h.page.evaluate(() => window.MeshCorePresets.applyPreset('deut', { skipPersist: true }));
      await h.page.waitForTimeout(300);
      const deut = await h.page.evaluate((i) => window.MeshCorePresets.list.find((p) => p.id === 'deut').scopePalette.light[i], other);
      assert((await h.cssVar('--scope-' + (other + 1))).toLowerCase() === deut.toLowerCase(), 'deut slot ' + (other + 1) + ': ' + await h.cssVar('--scope-' + (other + 1)) + ' want ' + deut + ' (was ' + before + ')');
      assert(await h.cssVar(st.var) === '#123456', 'the pick lost to the preset');
      await h.page.evaluate(() => window.MeshCorePresets.clearPreset());
      await h.page.waitForTimeout(300);
      assert(await h.cssVar('--scope-' + (other + 1)) === before, 'clearPreset did not restore');
    });

    await step('Display -> Live scope legend: 5 min and 3 rows retitle and cap the legend; bad input is put back', async () => {
      for (let i = 0; i < 6; i++) await h.send('#w' + i);
      await h.openTab('display');
      const sec = h.page.locator('[data-cv2-live-scope-section]');
      assert(await sec.count() === 1, 'no Live scope legend section');
      assert(await h.page.inputValue('[data-cv2-live-scope="windowMinutes"]') === '15', 'window default');
      assert(await h.page.inputValue('[data-cv2-live-scope="legendRows"]') === '12', 'rows default');
      await h.setNum('windowMinutes', 5);
      await h.setNum('legendRows', 3);
      await h.setNum('legendRows', 40);
      assert(await h.page.inputValue('[data-cv2-live-scope="legendRows"]') === '3', 'an out-of-range value stayed');
      const ov = await h.overrides();
      assert(ov.liveScope && ov.liveScope.windowMinutes === 5 && ov.liveScope.legendRows === 3, 'stored ' + JSON.stringify(ov.liveScope));
      await h.closePanel();
      const L = await h.legend();
      assert(L.title === 'SCOPES (last 5 min)', 'title ' + L.title);
      const named = L.rows.filter((r) => r.text.startsWith('#'));
      assert(named.length === 3, 'rows ' + JSON.stringify(L.rows));
      assert(L.rows.some((r) => /^\+\d+ more$/.test(r.text)), 'no +N more: ' + JSON.stringify(L.rows));
    });

    await step('reload keeps the palette and the window', async () => {
      await h.goto('color=scope');
      assert(await h.cssVar(st.var) === '#123456', 'pick lost on reload');
      await h.send('#dk');
      const L = await h.legend();
      assert(L.title === 'SCOPES (last 5 min)', 'window lost on reload: ' + L.title);
      await h.openTab('display');
      assert(await h.page.inputValue('[data-cv2-live-scope="windowMinutes"]') === '5', 'input after reload');
      assert(await h.page.$$eval('[data-cv2-live-scope-section] .cv2-override-dot', (d) => d.length) === 2, 'an override dot per changed key');
      await h.closePanel();
    });

    await step('export / import round trip (Export tab JSON)', async () => {
      const exported = await h.page.evaluate(() => JSON.parse(JSON.stringify(window._customizerV2.readOverrides())));
      assert(exported.liveScope && exported.theme[KEY(st.slot)] === '#123456', 'export ' + JSON.stringify(exported));
      const h2 = await open(browser);
      await h2.goto('color=scope');
      const res = await h2.page.evaluate((d) => window._customizerV2.validateShape(d), exported);
      assert(res.valid, 'import rejected: ' + JSON.stringify(res.errors));
      await h2.page.evaluate((d) => window._customizerV2.writeOverrides(d), exported);
      await h2.goto('color=scope');
      assert(await h2.cssVar(st.var) === '#123456', 'imported colour');
      await h2.send('#dk');
      assert((await h2.legend()).title === 'SCOPES (last 5 min)', 'imported window');
      assert(!h2.errors.length, h2.errors.join('; '));
      await h2.ctx.close();
    });
    assert(!h.errors.length, h.errors.join('; '));
    await h.ctx.close();

    for (const theme of ['light', 'dark']) {
      await step('phone 375x560 (' + theme + '), cold start, real taps: Display and Theme sections, then Live by scope', async () => {
        const p = await open(browser, { viewport: { width: 375, height: 560 }, touch: true, theme });
        const pg = p.page;
        await pg.goto(BASE + '/?r=' + Date.now() + '#/', { waitUntil: 'domcontentloaded' });
        await pg.waitForSelector('[data-bottom-nav-tab="live"]', { state: 'visible', timeout: 20000 });
        await pg.evaluate(() => localStorage.setItem('live-map-view', JSON.stringify({ lat: 50.5, lng: 10.5, zoom: 11 })));
        await pg.tap('[data-bottom-nav-tab="live"]');
        await p.ready();
        assert(await pg.evaluate(() => document.documentElement.getAttribute('data-theme')) === theme, 'theme');
        const inScreen = async (selector) => {
          const loc = pg.locator(selector).first();
          await loc.scrollIntoViewIfNeeded();
          const b = await loc.boundingBox();
          assert(b && b.x >= 0 && b.x + b.width <= 375 && b.y >= 0 && b.y + b.height <= 560, selector + ' off screen ' + JSON.stringify(b));
          return b;
        };
        await p.openTab('display', true);
        const tapType = async (key, value) => {
          const sel = '[data-cv2-live-scope="' + key + '"]';
          const b = await inScreen(sel);
          assert(b.height >= 40, key + ' input ' + b.height + ' px');
          await pg.tap(sel);
          await pg.keyboard.press('ControlOrMeta+A');
          await pg.keyboard.press('Backspace');
          await pg.keyboard.type(String(value));
          const t = pg.locator('[data-cv2-live-scope-section] .cust-section-title');
          await t.scrollIntoViewIfNeeded();
          await t.tap();
          await pg.waitForTimeout(700);
          assert(await pg.inputValue(sel) === String(value), key + ' did not take ' + value);
        };
        await tapType('windowMinutes', 10);
        await tapType('legendRows', 4);
        // The section survives the panel's re-render after the write.
        await inScreen('[data-cv2-live-scope-section] .cust-section-title');
        assert(await pg.inputValue('[data-cv2-live-scope="legendRows"]') === '4', 'rows after re-render');
        if (SHOTS) await pg.screenshot({ path: SHOTS + '/phone-' + theme + '-display.png' });
        const ov = await p.overrides();
        assert(ov.liveScope && ov.liveScope.windowMinutes === 10 && ov.liveScope.legendRows === 4, 'stored ' + JSON.stringify(ov));
        assert(await pg.evaluate(() => document.documentElement.scrollWidth - innerWidth) <= 0, 'sideways scroll (display)');
        // Theme tab: the group opens by a real tap; every row inside the screen.
        await p.openScopeColours(true);
        const sec = theme === 'dark' ? 'themeDark' : 'theme';
        for (const k of ['scope1', 'scope12', 'scopeOther']) await inScreen('[data-cv2-field="' + sec + '.' + k + '"]');
        await pg.tap('[data-cv2-field="' + sec + '.scope1"]'); // a real tap opens the native picker
        await pg.keyboard.press('Escape');
        await p.pickColour(sec, 'scope1', '#cc0077');
        if (SHOTS) await pg.screenshot({ path: SHOTS + '/phone-' + theme + '-theme.png' });
        assert(await pg.evaluate(() => document.documentElement.scrollWidth - innerWidth) <= 0, 'sideways scroll (theme)');
        await p.closePanel(true);
        // Live: Color by scope by a real tap, then a scope on slot 1.
        if (await pg.$eval('#liveControlsToggle', (e) => e.getAttribute('aria-expanded')) !== 'true') await pg.tap('#liveControlsToggle');
        await pg.waitForSelector('#liveColorScopeToggle', { state: 'visible' });
        await pg.$eval('#liveColorScopeToggle', (e) => e.scrollIntoView({ block: 'nearest' }));
        await pg.tap('#liveColorScopeToggle');
        await p.scopeOn();
        // A name whose preferred slot is 0, found by the module's own hash.
        const name = await pg.evaluate(() => { for (let i = 0; i < 1000; i++) { const n = '#p' + i; if (window.LiveScopeColor.preferredSlot(n) === 0) return n; } return null; });
        const hash = await p.send(name);
        assert(await p.slot(name) === 0, 'slot');
        assert(await p.routeColor(hash) === '#cc0077', 'phone line ' + await p.routeColor(hash));
        await pg.waitForTimeout(1300);
        const menu = await pg.evaluate(() => ({ title: document.querySelector('#scopeLegendMenu .legend-title').textContent,
          sw: [...document.querySelectorAll('#scopeLegendMenu li')].map((li) => { const s = li.querySelector('.live-scope-swatch'); return li.textContent.trim() + '=' + (s ? getComputedStyle(s).backgroundColor : ''); }) }));
        assert(menu.title === 'SCOPES (last 10 min)', 'phone title ' + menu.title);
        assert(menu.sw.includes(name + '=' + rgb('#cc0077')), 'phone swatch ' + JSON.stringify(menu.sw));
        await pg.$eval('#scopeLegendMenu', (e) => e.scrollIntoView({ block: 'nearest' }));
        if (SHOTS) await pg.screenshot({ path: SHOTS + '/phone-' + theme + '-live.png' });
        assert(!p.errors.length, p.errors.join('; '));
        await p.ctx.close();
      });
    }
  } finally {
    await browser.close();
  }
  console.log('\n' + passed + ' passed, ' + failed + ' failed');
  process.exit(failed ? 1 : 0);
})();
