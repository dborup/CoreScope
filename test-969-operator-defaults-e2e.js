/**
 * E2E (#969): the operator's display defaults reach the browser.
 *
 * config.json `timestamps` and `moments.display` are served on
 * /api/config/client; the CI server has neither, so this suite adds them
 * to the real response by interception (GET /api/moments is stubbed and
 * the app socket replaced, as in the #881 suites).
 *
 *   1. Packets, 1440x900, cold start: the Time column prints the operator's
 *      format (absolute, UTC, ISO with milliseconds); SITE_CONFIG carries
 *      it; the Display tab shows it with no override dot.
 *   2. A viewer's customizer value still wins over the operator's; a
 *      client config that lands after the theme is still waited for, and a
 *      viewer value unlike the built-in but unlike the operator's too is
 *      not dropped as a phantom override.
 *   3. Live: the operator's one card at a time shows one card for three
 *      moments; the Display tab shows the operator's 1 and 20 s; a viewer
 *      value beats them.
 *   4. Live: the operator's Moments-off default starts Live without them;
 *      a ?moments=1 link wins.
 *   3b. Live at 1440x900: under the operator's visibleCards 1 and
 *      showBusiest off, a viewer who types 3 and ticks Busiest repeater in
 *      the Display tab keeps both across a reload (review #975).
 *   5. 375x560 touch phone from a cold start: the same timestamp format on
 *      Packets; Live shows one card without sideways overflow. A phone
 *      always shows one (VISIBLE_PHONE), so the operator's visibleCards is
 *      proven by step 3 on desktop, not here.
 *   6. Without the operator blocks: the built-ins, as before.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-969-operator-defaults-e2e.js
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

const OP_TS = { defaultMode: 'absolute', timezone: 'utc', formatPreset: 'iso-seconds', customFormat: '', allowCustomFormat: false };
const ISO_MS = /^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\.\d{3}$/;
const VIEW = 'lat=55.3&lon=9.6&zoom=9';
const PK = (c) => c.repeat(64);
const at = (agoS) => new Date(Date.now() - agoS * 1000).toISOString();
const nodeMoment = (id, i) => ({ id, kind: 'new_node', at: at(2), node: { publicKey: PK('a'), name: 'E2E-969 ' + id, role: 'repeater', lat: 55.2 + i * 0.01, lon: 9.4 } });

// opts.display: moments.display (undefined: none); opts.timestamps: false
// leaves the server's own block; opts.overrides: the viewer's customizer
// overrides before the first load.
async function open(browser, opts) {
  opts = opts || {};
  const ctx = await browser.newContext(Object.assign({ viewport: opts.viewport || { width: 1440, height: 900 } },
    opts.touch ? { hasTouch: true, isMobile: true, deviceScaleFactor: 2 } : {}));
  const h = { ctx, sockets: [], errors: [], momentsRequests: 0 };
  await ctx.route(/\/api\/config\/client(\?|$)/, async (route) => {
    const res = await route.fetch();
    // opts.lateMs: the client config lands well after the theme.
    if (opts.lateMs) await new Promise((r) => setTimeout(r, opts.lateMs));
    const body = await res.json();
    if (opts.timestamps !== false) body.timestamps = OP_TS;
    body.moments = { enabled: true, historySize: 50 };
    if (opts.display) body.moments.display = opts.display;
    await route.fulfill({ response: res, json: body });
  });
  await ctx.route(/\/api\/moments(\?|$)/, (route) => { h.momentsRequests++; return route.fulfill({ json: { moments: [] } }); });
  await ctx.routeWebSocket(/.*/, (ws) => { h.sockets.push(ws); });
  if (opts.overrides) {
    await ctx.addInitScript((ov) => {
      try {
        if (!sessionStorage.getItem('__e2e969')) {
          sessionStorage.setItem('__e2e969', '1');
          localStorage.setItem('cs-theme-overrides', JSON.stringify(ov));
        }
      } catch (_) {}
    }, opts.overrides);
  }
  const page = await ctx.newPage();
  page.on('pageerror', (e) => h.errors.push(String(e.stack || e.message).split('\n').slice(0, 4).join(' | ')));
  h.page = page;
  h.inited = () => page.waitForFunction(() => window._customizerV2 && window._customizerV2.initDone, null, { timeout: 15000 });
  h.send = async (frames) => {
    const ws = h.sockets[h.sockets.length - 1];
    assert(ws, 'the app socket connected');
    for (const f of [].concat(frames)) ws.send(JSON.stringify({ type: 'moment', data: f }));
  };
  // Cold start on Packets; resolves with the first rendered Time cell
  // once it is in its final form for this config. A 3 h window (the
  // longest a phone allows) keeps the fixture's packets in view however
  // long ago it was freshened.
  h.packets = async (re) => {
    await page.goto(BASE + '/?r=' + Date.now() + '#/packets?timeWindow=180', { waitUntil: 'domcontentloaded' });
    await h.inited();
    await page.waitForFunction((src) => {
      const el = document.querySelector('td.col-time .timestamp-text');
      return el && new RegExp(src).test(el.textContent.trim());
    }, re.source, { timeout: 20000 });
    return page.evaluate(() => document.querySelector('td.col-time .timestamp-text').textContent.trim());
  };
  h.live = async (query, expectOn) => {
    await page.goto(BASE + '/?r=' + Date.now() + '#/live?' + VIEW + (query || ''), { waitUntil: 'domcontentloaded' });
    await page.waitForFunction(() => window._liveWSHandler && window._liveWSHandler() && window._liveGetMap && window._liveGetMap(), null, { timeout: 20000 });
    await h.inited();
    if (expectOn) await page.waitForFunction(() => window._liveMoments && window._liveMoments(), null, { timeout: 10000 });
    else await page.waitForFunction(() => getComputedStyle(document.getElementById('liveMomentsLabel')).display !== 'none', null, { timeout: 10000 });
  };
  h.openDisplay = async () => {
    if (opts.touch) {
      await page.tap('[data-bottom-nav-tab="more"]');
      await page.waitForSelector('[data-mpa-mirror="customizeToggle"]', { state: 'visible', timeout: 5000 });
      await page.tap('[data-mpa-mirror="customizeToggle"]');
    } else await page.click('#customizeToggle');
    await page.waitForSelector('.cust-overlay:not(.hidden)', { timeout: 5000 });
    if (opts.touch) await page.tap('.cust-tab[data-tab="display"]'); else await page.click('.cust-tab[data-tab="display"]');
    await page.waitForSelector('.cust-panel[data-panel="display"].active', { timeout: 4000 });
  };
  h.cards = () => page.evaluate(() => document.querySelectorAll('.live-moment-card:not(.is-leaving)').length);
  return h;
}

(async () => {
  const browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_PATH || undefined });
  try {
    console.log('\n=== #969 operator display defaults E2E against ' + BASE + ' ===');

    await step('Packets: the operator timestamp format from a cold start, shown as the default in the Display tab', async () => {
      const h = await open(browser);
      const text = await h.packets(ISO_MS);
      assert(ISO_MS.test(text), 'Time cell ' + JSON.stringify(text));
      const s = await h.page.evaluate(() => window.SITE_CONFIG.timestamps);
      assert(JSON.stringify(s) === JSON.stringify(OP_TS), 'SITE_CONFIG.timestamps ' + JSON.stringify(s));
      await h.openDisplay();
      const d = await h.page.evaluate(() => ({
        mode: document.querySelector('[data-cv2-select="timestamps.defaultMode"]').value,
        tz: document.querySelector('[data-cv2-select="timestamps.timezone"]').value,
        fmt: document.querySelector('[data-cv2-select="timestamps.formatPreset"]').value,
        dots: document.querySelectorAll('.cv2-override-dot[data-reset-s="timestamps"]').length,
      }));
      assert(d.mode === 'absolute' && d.tz === 'utc' && d.fmt === 'iso-seconds' && d.dots === 0, JSON.stringify(d));
      assert(!h.errors.length, h.errors.join('; '));
      await h.ctx.close();
    });

    await step('Packets: a viewer customizer value wins over the operator default', async () => {
      const h = await open(browser, { overrides: { timestamps: { defaultMode: 'ago' } } });
      const text = await h.packets(/ago$|^just now$/);
      assert(!ISO_MS.test(text), 'the operator beat the viewer: ' + text);
      const s = await h.page.evaluate(() => window.SITE_CONFIG.timestamps);
      assert(s.defaultMode === 'ago' && s.timezone === 'utc', JSON.stringify(s));
      await h.ctx.close();
    });

    await step('Packets: a client config that lands 2 s after the theme still sets the format; a viewer value is kept', async () => {
      const h = await open(browser, { lateMs: 2000, overrides: { timestamps: { timezone: 'local' } } });
      const text = await h.packets(ISO_MS);
      assert(ISO_MS.test(text), 'Time cell ' + JSON.stringify(text));
      const s = await h.page.evaluate(() => ({ ts: window.SITE_CONFIG.timestamps, ov: JSON.parse(localStorage.getItem('cs-theme-overrides') || '{}') }));
      assert(s.ts.defaultMode === 'absolute' && s.ts.timezone === 'local', JSON.stringify(s));
      assert(s.ov.timestamps && s.ov.timestamps.timezone === 'local', 'the viewer value was dropped: ' + JSON.stringify(s.ov));
      await h.ctx.close();
    });

    await step('Live: the operator one-card default, shown in the Display tab; a viewer value wins', async () => {
      const h = await open(browser, { display: { visibleCards: 1, dwellSeconds: 20 } });
      await h.live('', true);
      await h.send([0, 1, 2].map((i) => nodeMoment('c' + i, i)));
      await h.page.waitForFunction(() => document.querySelectorAll('.live-moment-card').length >= 1, null, { timeout: 5000 });
      await h.page.waitForTimeout(800);
      assert(await h.cards() === 1, 'cards ' + await h.cards());
      await h.openDisplay();
      const d = await h.page.evaluate(() => ({
        cards: document.querySelector('[data-cv2-moments="visibleCards"]').value,
        dwell: document.querySelector('[data-cv2-moments="dwellSeconds"]').value,
        dots: document.querySelectorAll('.cv2-override-dot[data-reset-s="moments"]').length,
      }));
      assert(d.cards === '1' && d.dwell === '20' && d.dots === 0, JSON.stringify(d));
      await h.ctx.close();

      const v = await open(browser, { display: { visibleCards: 1 }, overrides: { moments: { visibleCards: 3 } } });
      await v.live('', true);
      await v.send([0, 1, 2].map((i) => nodeMoment('v' + i, i)));
      await v.page.waitForFunction(() => document.querySelectorAll('.live-moment-card:not(.is-leaving)').length === 3, null, { timeout: 5000 });
      await v.ctx.close();
    });

    // Review #975, finding 1: a viewer who picks the built-in value under a
    // different operator default keeps it across a reload.
    await step('Live, 1440x900: under the operator visibleCards 1 and showBusiest off, the viewer types 3 and ticks Busiest repeater; both survive a reload', async () => {
      const h = await open(browser, { display: { visibleCards: 1, showBusiest: false } });
      await h.live('', true);
      await h.openDisplay();
      const cards = h.page.locator('[data-cv2-moments="visibleCards"]');
      await cards.fill('3');
      await cards.press('Tab');
      await h.page.click('[data-cv2-moments="showBusiest"]');
      const saved = () => h.page.evaluate(() => (JSON.parse(localStorage.getItem('cs-theme-overrides') || '{}').moments || null));
      await h.page.waitForFunction(() => {
        const m = JSON.parse(localStorage.getItem('cs-theme-overrides') || '{}').moments || {};
        return m.visibleCards === 3 && m.showBusiest === true;
      }, null, { timeout: 5000 }).catch(async () => { throw new Error('viewer values not saved: ' + JSON.stringify(await saved())); });
      await h.page.reload({ waitUntil: 'domcontentloaded' });
      await h.page.waitForFunction(() => window._liveMoments && window._liveMoments(), null, { timeout: 20000 });
      await h.inited();
      const eff = await h.page.evaluate(() => window._customizerV2.momentsSettings(window.SITE_CONFIG));
      assert(eff.visibleCards === 3 && eff.showBusiest === true, 'after reload ' + JSON.stringify(eff));
      await h.send([
        { id: 'b0', kind: 'busiest_repeater', at: at(2), relays: 42, windowMinutes: 10, node: { publicKey: PK('b'), name: 'E2E-969 busy', role: 'repeater', lat: 55.25, lon: 9.45 } },
        nodeMoment('r1', 1), nodeMoment('r2', 2),
      ]);
      await h.page.waitForFunction(() => document.querySelectorAll('.live-moment-card:not(.is-leaving)').length === 3, null, { timeout: 5000 })
        .catch(async () => { throw new Error('cards ' + await h.cards()); });
      assert(await h.page.evaluate(() => !!document.querySelector('.live-moment-card[data-kind="busiest_repeater"]')), 'no busiest card');
      await h.openDisplay();
      const d = await h.page.evaluate(() => ({
        cards: document.querySelector('[data-cv2-moments="visibleCards"]').value,
        busiest: document.querySelector('[data-cv2-moments="showBusiest"]').checked,
      }));
      assert(d.cards === '3' && d.busiest === true, 'Display tab after reload ' + JSON.stringify(d));
      assert(!h.errors.length, h.errors.join('; '));
      await h.ctx.close();
    });

    await step('Live: the operator Moments-off default; a ?moments=1 link wins', async () => {
      const h = await open(browser, { display: { enabled: false } });
      await h.live('', false);
      const off = await h.page.evaluate(() => ({ ctrl: !!window._liveMoments(), checked: document.getElementById('liveMomentsToggle').checked, dom: document.querySelectorAll('.live-moments').length }));
      assert(!off.ctrl && !off.checked && off.dom === 0, 'operator default off ignored: ' + JSON.stringify(off));
      assert(h.momentsRequests === 0, 'history requested while off');
      await h.live('&moments=1', true);
      assert(await h.page.evaluate(() => document.getElementById('liveMomentsToggle').checked), 'the link lost to the operator default');
      await h.ctx.close();
    });

    await step('375x560 phone, cold start: the operator timestamp format; Live shows one card, as a phone always does, without sideways overflow', async () => {
      const h = await open(browser, { viewport: { width: 375, height: 560 }, touch: true, display: { visibleCards: 1 } });
      const text = await h.packets(ISO_MS);
      assert(ISO_MS.test(text), 'Time cell ' + JSON.stringify(text));
      await h.live('', true);
      await h.send([0, 1].map((i) => nodeMoment('p' + i, i)));
      await h.page.waitForFunction(() => document.querySelectorAll('.live-moment-card').length >= 1, null, { timeout: 5000 });
      await h.page.waitForTimeout(800);
      assert(await h.cards() === 1, 'cards ' + await h.cards());
      const overflow = await h.page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
      assert(overflow <= 0, 'sideways overflow ' + overflow);
      assert(!h.errors.length, h.errors.join('; '));
      await h.ctx.close();
    });

    await step('without the operator blocks: the built-ins', async () => {
      const h = await open(browser, { timestamps: false });
      await h.packets(/ago$|^just now$/);
      const s = await h.page.evaluate(() => ({ ts: window.SITE_CONFIG.timestamps, m: window.SITE_CONFIG.moments || null }));
      assert(s.ts.defaultMode === 'ago' && s.ts.timezone === 'local' && s.ts.formatPreset === 'iso' && s.m === null, JSON.stringify(s));
      await h.ctx.close();
    });
  } finally {
    await browser.close();
  }
  console.log('\n#969 operator display defaults E2E: ' + passed + ' passed, ' + failed + ' failed');
  process.exit(failed ? 1 : 0);
})();
