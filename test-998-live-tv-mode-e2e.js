/**
 * E2E: Live's "TV mode" button and dialog (#998), which build and start the
 * kiosk link (#883, operator preset #968).
 *
 * /api/config/areas and /api/config/areas/polygons are stubbed with three
 * areas (the fixture config has none) and counted; /api/config/client gets
 * a Moments block and, per context, a `kiosk` preset; the kiosk's bulk reads
 * are stubbed; the app socket is replaced by page.routeWebSocket; reduced
 * motion.
 *
 *   1. Desktop 1440x900: the button is in the Live controls (behind the
 *      gear) and at least 44 px; it opens a labelled modal dialog with the
 *      configured areas (sorted by label) and the preset's tour, seconds
 *      and theme; opening it makes no request (the areas config is the
 *      Area filter's cached one).
 *   2. Ticking builds the link in tick order with the seconds (pulled into
 *      the kiosk's bounds), the theme and Moments off.
 *   3. "Copy link" puts exactly that URL on the clipboard.
 *   4. The QR code is drawn on demand, inside the viewport.
 *   5. Escape closes it and gives the focus back to the button.
 *   6. "Start here" lands in kiosk with the chosen tour: body.kiosk, two
 *      steps in that order, every 45 s, the dark theme, no Moments; the
 *      button is not shown in kiosk.
 *   7. Without a preset the dialog starts at the kiosk's defaults.
 *   8. 375x560 phone with touch, light and dark, real taps: gear → TV mode
 *      → two areas → Start here lands in kiosk with that tour; the dialog
 *      fits the viewport, every target is at least 44 px, no sideways
 *      scroll, the panel uses the theme's card colour.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-998-live-tv-mode-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
if (!['localhost', '127.0.0.1', '[::1]'].includes(new URL(BASE).hostname)) throw new Error('local server only');
const SHOT = (name) => (process.env.E2E_SCREENSHOT_DIR ? require('path').join(process.env.E2E_SCREENSHOT_DIR, '998-' + name + '.png') : null);

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + (e && e.stack || e)); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

// The fixture's nodes are around the Bay Area. Served in a random order,
// as the server's map is.
const AREAS = [
  { key: 'CCC', label: 'Kiosk Area C', latMin: 36.9, latMax: 37.1, lonMin: -122.15, lonMax: -121.9 },
  { key: 'AAA', label: 'Kiosk Area A', latMin: 37.2, latMax: 37.45, lonMin: -122.05, lonMax: -121.75 },
  { key: 'BBB', label: 'Kiosk Area B', polygon: [[37.70, -122.52], [37.83, -122.52], [37.83, -122.35], [37.70, -122.35]] },
];
const PRESET = { tour: ['bbb', 'AAA'], everySeconds: 30, theme: 'light', tiles: ['rate', 'active', 'records'], remountHours: 6 };

async function open(browser, o) {
  o = o || {};
  const ctx = await browser.newContext(Object.assign({ viewport: o.viewport || { width: 1440, height: 900 }, reducedMotion: 'reduce' },
    o.touch ? { hasTouch: true, isMobile: true, deviceScaleFactor: 2 } : {}, o.colorScheme ? { colorScheme: o.colorScheme } : {}));
  const h = { ctx, errors: [], areas: 0, polygons: 0, other: [] };
  await ctx.route(/\/api\/config\/areas(\?|$)/, (r) => { h.areas++; return r.fulfill({ json: AREAS.map((a) => ({ key: a.key, label: a.label })) }); });
  await ctx.route(/\/api\/config\/areas\/polygons(\?|$)/, (r) => { h.polygons++; return r.fulfill({ json: AREAS }); });
  await ctx.route(/\/api\/nodes\/network-status(\?|$)/, (r) => r.fulfill({ json: { total: 400, active: 318, degraded: 2, silent: 80, roleCounts: {} } }));
  await ctx.route(/\/api\/analytics\/areas(\?|$)/, (r) => r.fulfill({ json: { density: [] } }));
  await ctx.route(/\/api\/config\/client(\?|$)/, async (route) => {
    const res = await route.fetch();
    const body = await res.json();
    body.moments = { enabled: true, historySize: 50 };
    if (o.preset) body.kiosk = o.preset;
    await route.fulfill({ response: res, json: body });
  });
  await ctx.route(/\/api\/moments(\?|$)/, (r) => r.fulfill({ json: { moments: [] } }));
  await ctx.routeWebSocket(/.*/, () => {});
  const page = await ctx.newPage();
  h.page = page;
  page.on('pageerror', (e) => h.errors.push(String(e.stack || e.message).split('\n').slice(0, 3).join(' | ')));
  // Every request after a mark, to show the dialog makes none.
  page.on('request', (r) => { if (h.watch && /\/api\/config\//.test(r.url())) h.other.push(r.url().replace(BASE, '')); });
  await page.addInitScript((o) => {
    if (sessionStorage.getItem('998-seeded')) return;
    sessionStorage.setItem('998-seeded', '1');
    localStorage.setItem('meshcore-theme', o.theme || 'dark');
    if (!o.touch) localStorage.setItem('live-nav-pinned', 'true');
  }, { theme: o.colorScheme, touch: !!o.touch });
  h.liveReady = async () => {
    await page.waitForFunction(() => window._liveGetMap && window._liveGetMap(), null, { timeout: 20000 });
    await page.evaluate(() => window.MeshConfigReady);
    // The Area filter has its (cached) config.
    await page.waitForFunction(() => document.querySelector('#liveAreaFilter .region-dropdown-trigger'), null, { timeout: 20000 });
  };
  h.gotoLive = async () => {
    await page.goto(BASE + '/?r=' + Date.now() + '#/live', { waitUntil: 'domcontentloaded' });
    await h.liveReady();
  };
  h.dialog = () => page.evaluate(() => {
    const d = document.querySelector('[data-live-tv-dialog]');
    if (!d) return null;
    const url = d.querySelector('.live-tv-url');
    return {
      role: d.getAttribute('role'), modal: d.getAttribute('aria-modal'),
      title: (document.getElementById(d.getAttribute('aria-labelledby')) || {}).textContent,
      areas: [...d.querySelectorAll('.live-tv-area')].map((li) => li.querySelector('.live-tv-area-label').textContent + ':' + li.querySelector('.live-tv-order').textContent),
      every: d.querySelector('.live-tv-every').value,
      theme: d.querySelector('.live-tv-theme').value,
      moments: d.querySelector('.live-tv-moments') ? d.querySelector('.live-tv-moments').checked : null,
      url: url ? url.textContent : '',
    };
  });
  h.kiosk = async () => {
    await page.waitForFunction(() => window._liveKiosk && window._liveKiosk(), null, { timeout: 20000 });
    return page.evaluate(() => {
      const l = document.querySelector('.live-kiosk-step-label'), c = document.querySelector('.live-kiosk-step-count');
      return { kiosk: document.body.classList.contains('kiosk'), hash: location.hash, stats: window._liveKiosk().stats(),
        label: (l ? l.textContent : '') + '|' + (c ? c.textContent : ''), theme: document.documentElement.getAttribute('data-theme'),
        dialog: !!document.querySelector('[data-live-tv-dialog]') };
    });
  };
  return h;
}

async function openControls(page) {
  if (!(await page.isVisible('#liveTvModeBtn'))) await page.click('#liveControlsToggle');
  await page.waitForSelector('#liveTvModeBtn', { state: 'visible', timeout: 5000 });
}

(async () => {
  const browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_PATH || undefined });
  try {
    console.log('\n=== #998 Live TV mode E2E ===');
    {
      const h = await open(browser, { preset: PRESET });
      const p = h.page;
      await h.ctx.grantPermissions(['clipboard-read', 'clipboard-write'], { origin: BASE });
      await step('Live loads with the Area filter\'s areas', async () => { await h.gotoLive(); });
      const expected = '#/live?kiosk=1&tour=CCC,AAA&every=45&theme=dark&moments=0';

      await step('the TV mode button is in the Live controls, behind the gear, at least 44 px', async () => {
        assert(await p.evaluate(() => !!document.querySelector('#liveControlsBody #liveTvModeBtn')), 'not in #liveControlsBody');
        assert(!(await p.isVisible('#liveTvModeBtn')), 'shown while the controls are collapsed');
        await openControls(p);
        const b = await p.locator('#liveTvModeBtn').boundingBox();
        assert(b && b.height >= 44 && b.width >= 44, 'button ' + JSON.stringify(b));
        assert((await p.textContent('#liveTvModeBtn')).trim() === 'TV mode');
      });

      await step('it opens a labelled modal with the areas sorted by label and the preset\'s tour, seconds and theme; no request', async () => {
        const before = h.areas;
        h.watch = true; h.other = [];
        await p.click('#liveTvModeBtn');
        await p.waitForSelector('[data-live-tv-dialog] .live-tv-panel', { state: 'visible', timeout: 5000 });
        const d = await h.dialog();
        assert(d.role === 'dialog' && d.modal === 'true' && d.title === 'TV mode', JSON.stringify(d));
        assert(JSON.stringify(d.areas) === '["Kiosk Area A:2","Kiosk Area B:1","Kiosk Area C:"]', 'areas ' + JSON.stringify(d.areas));
        assert(d.every === '30' && d.theme === 'light' && d.moments === true, JSON.stringify(d));
        assert(d.url === BASE + '/#/live?kiosk=1&tour=BBB,AAA&every=30&theme=light', 'url ' + d.url);
        await p.keyboard.press('Escape');
        await p.click('#liveTvModeBtn');
        await p.waitForSelector('[data-live-tv-dialog]', { timeout: 5000 });
        await p.waitForTimeout(300);
        h.watch = false;
        assert(h.areas === before && h.areas <= 1, 'areas config requests: ' + before + ' -> ' + h.areas);
        assert(!h.other.length, 'requests while opening: ' + h.other.join(', '));
      });

      await step('ticking builds the link in tick order, with bounded seconds, the theme and Moments off', async () => {
        await p.click('.live-tv-area-input[data-area="BBB"]');
        await p.click('.live-tv-area-input[data-area="AAA"]');
        await p.click('.live-tv-area-input[data-area="CCC"]');
        await p.click('.live-tv-area-input[data-area="AAA"]');
        await p.fill('.live-tv-every', '5');
        await p.press('.live-tv-every', 'Tab');
        assert((await h.dialog()).every === '15', 'every not pulled into the bounds');
        await p.fill('.live-tv-every', '45');
        await p.press('.live-tv-every', 'Tab');
        await p.selectOption('.live-tv-theme', 'dark');
        await p.click('.live-tv-moments');
        const d = await h.dialog();
        assert(JSON.stringify(d.areas) === '["Kiosk Area A:2","Kiosk Area B:","Kiosk Area C:1"]', 'areas ' + JSON.stringify(d.areas));
        assert(d.url === BASE + '/' + expected, 'url ' + d.url);
      });

      await step('Copy link puts exactly that URL on the clipboard', async () => {
        await p.evaluate(() => navigator.clipboard.writeText('-'));
        await p.click('.live-tv-copy');
        await p.waitForFunction(() => /copied/i.test(document.querySelector('.live-tv-status').textContent), null, { timeout: 5000 });
        const clip = await p.evaluate(() => navigator.clipboard.readText());
        assert(clip === BASE + '/' + expected, 'clipboard ' + clip);
      });

      await step('the QR code is drawn on demand, inside the viewport', async () => {
        assert(!(await p.isVisible('.live-tv-qr')), 'shown before asked');
        await p.click('.live-tv-qr-btn');
        const q = await p.evaluate(() => {
          const svg = document.querySelector('.live-tv-qr svg');
          const path = svg && svg.querySelector('path');
          svg.scrollIntoView({ block: 'nearest' });
          const r = svg.getBoundingClientRect();
          return { d: path ? path.getAttribute('d').length : 0, w: r.width, top: r.top, bottom: r.bottom, left: r.left, right: r.right, vw: innerWidth, vh: innerHeight };
        });
        assert(q.d > 100 && q.w >= 150 && q.left >= 0 && q.right <= q.vw && q.top >= 0 && q.bottom <= q.vh, JSON.stringify(q));
        await p.screenshot({ path: SHOT('desktop-dialog') || undefined }).catch(() => {});
      });

      await step('Escape closes it and the focus goes back to the button', async () => {
        await p.keyboard.press('Escape');
        assert(!(await h.dialog()), 'still open');
        assert(await p.evaluate(() => document.activeElement && document.activeElement.id === 'liveTvModeBtn'), 'focus');
      });

      await step('Start here lands in kiosk with the chosen tour, seconds and theme, and no Moments', async () => {
        await p.click('#liveTvModeBtn');
        await p.waitForSelector('[data-live-tv-dialog]', { timeout: 5000 });
        // A new dialog starts from the preset again.
        assert((await h.dialog()).url === BASE + '/#/live?kiosk=1&tour=BBB,AAA&every=30&theme=light', 'reopened from the preset');
        await p.click('.live-tv-area-input[data-area="BBB"]');
        await p.click('.live-tv-area-input[data-area="AAA"]');
        await p.click('.live-tv-area-input[data-area="CCC"]');
        await p.click('.live-tv-area-input[data-area="AAA"]');
        await p.fill('.live-tv-every', '45');
        await p.selectOption('.live-tv-theme', 'dark');
        await p.click('.live-tv-moments');
        await p.click('.live-tv-start');
        await p.waitForFunction(() => window._liveKiosk && window._liveKiosk() && window._liveKiosk().stats().steps === 2, null, { timeout: 20000 });
        const k = await h.kiosk();
        assert(k.hash === expected, 'hash ' + k.hash);
        assert(k.kiosk && !k.dialog, JSON.stringify(k));
        assert(k.stats.every === 45, 'every ' + k.stats.every);
        assert(k.label === 'Kiosk Area C|1 / 2', 'label ' + k.label);
        assert(k.theme === 'dark', 'theme ' + k.theme);
        await p.waitForTimeout(500);
        assert(!(await p.evaluate(() => window._liveMoments && window._liveMoments())), 'Moments running');
        assert(!(await p.isVisible('#liveTvModeBtn')), 'the button shows in kiosk');
      });

      await step('no page errors', async () => { assert(!h.errors.length, h.errors.join('; ')); });
      await h.ctx.close();
    }

    await step('without a preset the dialog starts at the kiosk defaults: no tour, 60 s, dark, Moments on', async () => {
      const h = await open(browser);
      await h.gotoLive();
      await openControls(h.page);
      await h.page.click('#liveTvModeBtn');
      await h.page.waitForSelector('[data-live-tv-dialog]', { timeout: 5000 });
      const d = await h.dialog();
      assert(JSON.stringify(d.areas) === '["Kiosk Area A:","Kiosk Area B:","Kiosk Area C:"]', JSON.stringify(d.areas));
      assert(d.every === '60' && d.theme === 'dark' && d.moments === true, JSON.stringify(d));
      assert(d.url === BASE + '/#/live?kiosk=1&tour=&every=60&theme=dark', d.url);
      assert(!h.errors.length, h.errors.join('; '));
      await h.ctx.close();
    });

    for (const scheme of ['light', 'dark']) {
      await step('375x560 phone, ' + scheme + ', real taps: gear → TV mode → two areas → Start here lands in kiosk', async () => {
        const h = await open(browser, { viewport: { width: 375, height: 560 }, touch: true, colorScheme: scheme });
        const p = h.page;
        await h.gotoLive();
        assert(await p.evaluate(() => document.documentElement.getAttribute('data-theme')) === scheme, 'theme');
        const tap = async (sel) => {
          const loc = p.locator(sel);
          await loc.waitFor({ state: 'visible', timeout: 10000 });
          await loc.scrollIntoViewIfNeeded();
          const b = await loc.boundingBox();
          assert(b && b.x >= 0 && b.x + b.width <= 375 && b.y >= 0 && b.y + b.height <= 560, sel + ' off screen ' + JSON.stringify(b));
          assert(b.height >= 44, sel + ' ' + b.height + ' px tall');
          await p.touchscreen.tap(b.x + b.width / 2, b.y + b.height / 2);
          await p.waitForTimeout(550); // two taps a finger apart (tests/e2e-tap.js)
          return b;
        };
        await tap('#liveControlsToggle');
        await tap('#liveTvModeBtn');
        await p.waitForSelector('[data-live-tv-dialog] .live-tv-panel', { state: 'visible', timeout: 5000 });
        const fit = await p.evaluate(() => {
          const panel = document.querySelector('.live-tv-panel');
          const r = panel.getBoundingClientRect();
          const probe = document.createElement('div');
          probe.style.background = 'var(--card-bg)';
          document.body.appendChild(probe);
          const card = getComputedStyle(probe).backgroundColor;
          probe.remove();
          const small = [...panel.querySelectorAll('button, select, input[type="number"], .live-tv-area-row, .live-tv-check')]
            .map((e) => [e.className || e.tagName, e.getBoundingClientRect().height]).filter((x) => x[1] < 44);
          return { r: { l: r.left, t: r.top, r: r.right, b: r.bottom }, scroll: panel.scrollHeight > panel.clientHeight,
            sw: document.scrollingElement.scrollWidth, iw: innerWidth, bg: getComputedStyle(panel).backgroundColor, card, small };
        });
        assert(fit.r.l >= 0 && fit.r.t >= 0 && fit.r.r <= 375 && fit.r.b <= 560, 'panel ' + JSON.stringify(fit.r));
        assert(fit.sw <= fit.iw, 'sideways scroll ' + fit.sw);
        assert(fit.bg === fit.card, 'panel ' + fit.bg + ' card ' + fit.card);
        assert(!fit.small.length, 'under 44 px: ' + JSON.stringify(fit.small));
        await p.screenshot({ path: SHOT('phone-' + scheme) || undefined }).catch(() => {});
        await tap('.live-tv-area:has([data-area="BBB"]) .live-tv-area-row');
        await tap('.live-tv-area:has([data-area="CCC"]) .live-tv-area-row');
        const d = await h.dialog();
        assert(d.url === BASE + '/#/live?kiosk=1&tour=BBB,CCC&every=60&theme=dark', 'url ' + d.url);
        await tap('.live-tv-start');
        await p.waitForFunction(() => window._liveKiosk && window._liveKiosk() && window._liveKiosk().stats().steps === 2, null, { timeout: 20000 });
        const k = await h.kiosk();
        assert(k.kiosk && !k.dialog && k.hash === '#/live?kiosk=1&tour=BBB,CCC&every=60&theme=dark', JSON.stringify(k));
        assert(k.label === 'Kiosk Area B|1 / 2', 'label ' + k.label);
        assert(!h.errors.length, h.errors.join('; '));
        await h.ctx.close();
      });
    }
  } finally {
    await browser.close();
  }
  console.log('\n' + passed + ' passed, ' + failed + ' failed');
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
