/**
 * E2E (#968 M2): Customize → Display → Live kiosk, the screen's text and
 * tile size (kiosk.scale) and its edge margin (kiosk.safeInsetVmin), and
 * what they do to a #/live?kiosk=1 screen.
 *
 * /api/config/areas/polygons, /api/nodes/network-status and
 * /api/analytics/areas are stubbed; the app socket is replaced by
 * page.routeWebSocket; /api/config/client is intercepted for the operator
 * preset steps only (the fixture config has no kiosk block).
 *
 *   1. Desktop 1440x900: the Display tab has the Live kiosk section with
 *      auto / 3, a 48 px select and input; setting 2 and 6 stores them.
 *   2. The kiosk link at 1920x1080: computed --kiosk-scale 2, the step
 *      label 6 % of the short side from the page's corner (65 px), the big
 *      number at twice its base size.
 *   3. Leaving kiosk takes the inline values off body.
 *   4. A customizer change while kiosk runs (theme-changed) applies at once.
 *   5. Reset All: auto (1.5 by the media query at 1920x1080) and 3vmin.
 *   6. The operator preset (scale 1.25, inset 5): kiosk takes it with no
 *      viewer value; the Display tab shows it; a viewer value wins; setting
 *      the operator's value back stores nothing.
 *   7. A cold kiosk link whose /api/config/client answers 2.5 s late with
 *      the preset 1.25 / 5: kiosk mounts on auto / 3vmin, then ends on
 *      1.25 / 5vmin once the preset arrives (initKiosk applies it again).
 *   8. 375x560 touch phone, cold from Home: More → Customize → Display,
 *      both controls changed by real taps, both >= 44 px, no horizontal
 *      scroll.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-968-kiosk-customizer-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
if (!['localhost', '127.0.0.1', '[::1]'].includes(new URL(BASE).hostname)) throw new Error('local server only');

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + (e && e.stack || e)); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

const AREAS = [{ key: 'AAA', label: 'Kiosk Area A', latMin: 37.2, latMax: 37.45, lonMin: -122.05, lonMax: -121.75 }];
const PRESET = { tour: [], everySeconds: 60, theme: 'dark', tiles: ['rate', 'active', 'records'], remountHours: 6, scale: 1.25, safeInsetVmin: 5 };

async function open(browser, o) {
  o = o || {};
  const ctx = await browser.newContext(Object.assign({ viewport: o.viewport || { width: 1440, height: 900 }, reducedMotion: 'reduce' },
    o.touch ? { hasTouch: true, isMobile: true } : {}));
  const h = { ctx, errors: [] };
  await ctx.route(/\/api\/config\/areas\/polygons(\?|$)/, (r) => r.fulfill({ json: AREAS }));
  await ctx.route(/\/api\/nodes\/network-status(\?|$)/, (r) => r.fulfill({ json: { total: 400, active: 318, degraded: 2, silent: 80, roleCounts: {} } }));
  await ctx.route(/\/api\/analytics\/areas(\?|$)/, (r) => r.fulfill({ json: { density: [] } }));
  if (o.preset) {
    await ctx.route(/\/api\/config\/client(\?|$)/, async (route) => {
      const res = await route.fetch();
      const body = await res.json();
      body.kiosk = o.preset;
      if (o.delayMs) await new Promise((r) => setTimeout(r, o.delayMs));
      await route.fulfill({ response: res, json: body });
    });
  }
  await ctx.routeWebSocket(/.*/, () => {});
  const page = await ctx.newPage();
  h.page = page;
  page.on('pageerror', (e) => h.errors.push(String(e.stack || e.message).split('\n').slice(0, 3).join(' | ')));
  page.on('dialog', (d) => d.accept()); // Reset All's confirm
  h.cold = async (hash) => {
    await page.goto(BASE + '/?r=' + Date.now() + (hash || '#/packets'), { waitUntil: 'domcontentloaded' });
    await page.evaluate(() => window.MeshConfigReady);
    await page.waitForFunction(() => window._customizerV2 && window._customizerV2.initDone, null, { timeout: 15000 });
  };
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
  h.closeCustomizer = async () => {
    if (await page.$('.cust-overlay:not(.hidden)')) {
      await page.keyboard.press('Escape');
      await page.waitForSelector('.cust-overlay:not(.hidden)', { state: 'detached', timeout: 3000 }).catch(() => {});
    }
  };
  h.setScale = async (v) => { await page.selectOption('[data-cv2-kiosk="scale"]', String(v)); await page.waitForTimeout(600); };
  h.setInset = async (v) => {
    await page.fill('[data-cv2-kiosk="safeInsetVmin"]', String(v));
    await page.dispatchEvent('[data-cv2-kiosk="safeInsetVmin"]', 'change');
    await page.waitForTimeout(600); // the debounced write
  };
  h.stored = () => page.evaluate(() => { try { return (JSON.parse(localStorage.getItem('cs-theme-overrides') || '{}').kiosk) || null; } catch (_) { return 'bad'; } });
  h.kiosk = async () => {
    await page.evaluate(() => { location.hash = '#/live?kiosk=1&tour=AAA'; });
    await page.waitForFunction(() => window._liveKiosk && window._liveKiosk(), null, { timeout: 20000 });
    await page.waitForSelector('.live-kiosk-step', { state: 'attached', timeout: 10000 });
  };
  h.leave = async () => {
    await page.evaluate(() => { location.hash = '#/packets'; });
    await page.waitForFunction(() => !document.body.classList.contains('kiosk'), null, { timeout: 20000 });
  };
  h.look = () => page.evaluate(() => {
    const b = document.body, cs = getComputedStyle(b);
    const live = document.querySelector('.live-page'), st = document.querySelector('.live-kiosk-step');
    const big = document.querySelector('.live-kiosk-tile-value');
    const pr = live && live.getBoundingClientRect(), sr = st && st.getBoundingClientRect();
    return {
      scale: cs.getPropertyValue('--kiosk-scale').trim(),
      inset: cs.getPropertyValue('--kiosk-safe-inset').trim(),
      inlineScale: b.style.getPropertyValue('--kiosk-scale'),
      inlineInset: b.style.getPropertyValue('--kiosk-safe-inset'),
      stepLeft: sr && pr ? Math.round(sr.left - pr.left) : null,
      stepTop: sr && pr ? Math.round(sr.top - pr.top) : null,
      bigPx: big ? parseFloat(getComputedStyle(big).fontSize) : null,
      short: Math.min(innerWidth, innerHeight),
    };
  });
  return h;
}

(async () => {
  const browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_PATH || undefined });
  try {
    console.log('\n=== #968 M2 Live kiosk customizer E2E against ' + BASE + ' ===');
    const h = await open(browser);
    const p = h.page;

    await step('Display tab: the Live kiosk section with auto / 3, 48 px controls; 2 and 6 are stored', async () => {
      await h.cold();
      await h.openTab('display');
      const s = await p.evaluate(() => {
        const sel = document.querySelector('[data-cv2-kiosk="scale"]'), inp = document.querySelector('[data-cv2-kiosk="safeInsetVmin"]');
        return { title: [...document.querySelectorAll('.cust-section-title')].some((e) => e.textContent === 'Live kiosk'),
          scale: sel && sel.value, inset: inp && inp.value,
          h: [sel, inp].map((e) => e && e.getBoundingClientRect().height) };
      });
      assert(s.title && s.scale === 'auto' && s.inset === '3', JSON.stringify(s));
      assert(s.h.every((x) => x >= 44), 'heights ' + JSON.stringify(s.h));
      await h.setScale(2);
      await h.setInset(6);
      const st = await h.stored();
      assert(JSON.stringify(st) === '{"scale":2,"safeInsetVmin":6}', 'stored: ' + JSON.stringify(st));
      await h.setInset(12); // out of range: put back
      assert(await p.inputValue('[data-cv2-kiosk="safeInsetVmin"]') === '6', 'a bad value stayed in the field');
      assert(JSON.stringify(await h.stored()) === '{"scale":2,"safeInsetVmin":6}', 'a bad value was stored');
      await h.closeCustomizer();
    });

    await step('kiosk at 1920x1080 uses them: scale 2, a 6 % inset, the big number at twice its size', async () => {
      await p.setViewportSize({ width: 1920, height: 1080 });
      await h.kiosk();
      const l = await h.look();
      assert(l.scale === '2' && l.inset === '6vmin', JSON.stringify(l));
      const want = Math.round(0.06 * l.short); // 65
      assert(Math.abs(l.stepLeft - want) <= 1 && Math.abs(l.stepTop - want) <= 1, 'step label at ' + l.stepLeft + ',' + l.stepTop + ', want ' + want);
      assert(Math.abs(l.bigPx - 112) < 0.5, 'big number ' + l.bigPx + ' px, want 56 x 2');
    });

    await step('leaving kiosk takes the inline values off body', async () => {
      await h.leave();
      const l = await h.look();
      assert(l.inlineScale === '' && l.inlineInset === '', JSON.stringify(l));
    });

    await step('a customizer change while kiosk runs applies at once, with no remount', async () => {
      await h.kiosk();
      const before = await p.evaluate(() => window.LiveKiosk.remounts());
      const ctl = await p.evaluate(() => { window.__k = window._liveKiosk(); return true; });
      assert(ctl);
      await p.evaluate(() => window._customizerV2.setKioskSetting('scale', 1));
      await p.waitForFunction(() => getComputedStyle(document.body).getPropertyValue('--kiosk-scale').trim() === '1', null, { timeout: 3000 });
      assert(await p.evaluate(() => window._liveKiosk() === window.__k), 'the controller was replaced');
      assert(await p.evaluate(() => window.LiveKiosk.remounts()) === before, 'remounted');
      await h.leave();
    });

    await step('Reset All: auto (1.5 at 1920x1080) and 3vmin again', async () => {
      await h.openTab('export');
      await p.click('#cv2ResetAll');
      await p.waitForFunction(() => !localStorage.getItem('cs-theme-overrides'), null, { timeout: 3000 });
      await h.openTab('display');
      assert(await p.inputValue('[data-cv2-kiosk="scale"]') === 'auto', 'the select did not follow');
      await h.closeCustomizer();
      await h.kiosk();
      const l = await h.look();
      assert(l.inlineScale === '' && l.inlineInset === '' && l.scale === '1.5' && l.inset === '3vmin', JSON.stringify(l));
      await h.leave();
      assert(!h.errors.length, h.errors.join('; '));
    });
    await h.ctx.close();

    await step('operator preset 1.25 / 5: kiosk takes it, the tab shows it, a viewer value wins, its own value stores nothing', async () => {
      const o = await open(browser, { preset: PRESET, viewport: { width: 1920, height: 1080 } });
      const q = o.page;
      await o.cold();
      await o.kiosk();
      let l = await o.look();
      assert(l.scale === '1.25' && l.inset === '5vmin', 'preset: ' + JSON.stringify(l));
      await o.leave();
      await o.openTab('display');
      assert(await q.inputValue('[data-cv2-kiosk="scale"]') === '1.25' && await q.inputValue('[data-cv2-kiosk="safeInsetVmin"]') === '5', 'the tab shows the operator values');
      await o.setScale(2);
      await o.setInset(5); // the operator's: not stored
      assert(JSON.stringify(await o.stored()) === '{"scale":2}', 'stored: ' + JSON.stringify(await o.stored()));
      await o.closeCustomizer();
      await o.kiosk();
      l = await o.look();
      assert(l.scale === '2' && l.inset === '5vmin', 'viewer over preset: ' + JSON.stringify(l));
      await o.leave();
      assert(!o.errors.length, o.errors.join('; '));
      await o.ctx.close();
    });

    await step('a late /api/config/client (2.5 s): kiosk mounts on auto / 3vmin, then ends on the preset 1.25 / 5vmin', async () => {
      const o = await open(browser, { preset: PRESET, delayMs: 2500, viewport: { width: 1920, height: 1080 } });
      const q = o.page;
      await q.goto(BASE + '/?r=' + Date.now() + '#/live?kiosk=1&tour=AAA', { waitUntil: 'domcontentloaded' });
      await q.waitForFunction(() => document.body.classList.contains('kiosk'), null, { timeout: 20000 });
      const early = await q.evaluate(() => ({ cfg: window.MC_KIOSK_CFG === undefined,
        scale: getComputedStyle(document.body).getPropertyValue('--kiosk-scale').trim(),
        inset: getComputedStyle(document.body).getPropertyValue('--kiosk-safe-inset').trim() }));
      // The preset must not be there yet, or this step tests nothing.
      assert(early.cfg, 'the client config arrived before kiosk mounted');
      assert(early.scale === '1.5' && early.inset === '3vmin', 'before the preset: ' + JSON.stringify(early));
      await q.waitForFunction(() => window._liveKiosk && window._liveKiosk(), null, { timeout: 20000 });
      await q.waitForSelector('.live-kiosk-step', { state: 'attached', timeout: 10000 });
      const l = await o.look();
      assert(l.scale === '1.25' && l.inset === '5vmin', 'after the late preset: ' + JSON.stringify(l));
      const want = Math.round(0.05 * l.short); // 54
      assert(Math.abs(l.stepLeft - want) <= 1 && Math.abs(l.stepTop - want) <= 1, 'step label at ' + l.stepLeft + ',' + l.stepTop + ', want ' + want);
      assert(!o.errors.length, o.errors.join('; '));
      await o.ctx.close();
    });

    await step('375x560 touch phone, cold from Home: both controls by real taps, >= 44 px, no horizontal scroll', async () => {
      const m = await open(browser, { viewport: { width: 375, height: 560 }, touch: true });
      const q = m.page;
      await m.cold('#/home');
      await m.openTab('display', true);
      await q.locator('[data-cv2-kiosk="scale"]').scrollIntoViewIfNeeded();
      const geo = await q.evaluate(() => {
        const r = (s) => document.querySelector(s).getBoundingClientRect();
        const a = r('[data-cv2-kiosk="scale"]'), b = r('[data-cv2-kiosk="safeInsetVmin"]');
        const panel = document.querySelector('.cust-panel[data-panel="display"]');
        return { a: [a.left, a.right, a.height], b: [b.left, b.right, b.height],
          docScroll: document.documentElement.scrollWidth - document.documentElement.clientWidth,
          panelScroll: panel ? panel.scrollWidth - panel.clientWidth : 0, w: innerWidth };
      });
      assert(geo.a[2] >= 44 && geo.b[2] >= 44, 'heights ' + JSON.stringify(geo));
      assert(geo.a[0] >= 0 && geo.a[1] <= geo.w && geo.b[0] >= 0 && geo.b[1] <= geo.w, 'outside the screen ' + JSON.stringify(geo));
      assert(geo.docScroll <= 0 && geo.panelScroll <= 0, 'horizontal scroll ' + JSON.stringify(geo));
      await q.tap('[data-cv2-kiosk="scale"]');
      await q.selectOption('[data-cv2-kiosk="scale"]', '1.5');
      await q.waitForTimeout(600);
      await q.locator('[data-cv2-kiosk="safeInsetVmin"]').scrollIntoViewIfNeeded();
      await q.tap('[data-cv2-kiosk="safeInsetVmin"]');
      for (let i = 0; i < 4; i++) { await q.keyboard.press('Backspace'); await q.keyboard.press('Delete'); }
      assert(await q.inputValue('[data-cv2-kiosk="safeInsetVmin"]') === '', 'the field did not clear');
      await q.keyboard.type('4.5');
      await q.tap('.cust-panel[data-panel="display"] .cust-section-title >> text=Live kiosk');
      await q.waitForTimeout(700);
      const st = await m.stored();
      assert(JSON.stringify(st) === '{"scale":1.5,"safeInsetVmin":4.5}', 'stored: ' + JSON.stringify(st));
      assert(!m.errors.length, m.errors.join('; '));
      await m.ctx.close();
    });
  } finally {
    await browser.close();
  }
  console.log('\n' + passed + ' passed, ' + failed + ' failed');
  process.exit(failed ? 1 : 0);
})();
