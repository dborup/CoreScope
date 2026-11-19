/**
 * E2E (#935): the map-control panel on a phone ends above the bottom nav.
 *
 * Every scenario is a fresh browser context (cold start, empty storage) with
 * touch emulation. At phone widths the panel's max-height ignored
 * --bottom-nav-reserve and --warmup-pills-h, so its last section ("Quick
 * Jump") could never be scrolled above the bottom nav.
 *
 * Asserts, at 375x560, 360x640 and 640x360 (landscape), and at 375x560 with
 * the real stale-source pill row (/api/healthz stubbed with a stale
 * ingest_liveness source, as test-issue-894-pill-no-overlay-e2e.js does):
 *   1. the open panel ends above the bottom nav and inside the map;
 *   2. the collapsed Quick Jump legend, brought into view by real touch swipes
 *      only (a panel a finger cannot scroll fails), is entirely above the nav,
 *      and a tap 4px above its bottom edge lands on it;
 *   3. the last control, after expanding the section and swiping again, is not
 *      covered by the nav, a hit test at its centre lands on it, and a real
 *      tap on it reaches it (it receives the click);
 * at 375x560 in embed mode (#/map?embed=1, no header, no bottom nav) the
 * panel is no shorter than the master rule gave (100vh - 80px), stays inside
 * the map, and its last control is reachable by swipes;
 * and at 1440x900 the panel is unchanged: inside the map, no bottom nav.
 *
 * Swipes are stepped touchStart/touchMove/touchEnd events sent through CDP:
 * Input.synthesizeScrollGesture does not scroll on the Linux runner (#841).
 *
 * Usage:
 *   BASE_URL=http://localhost:13935 node test-issue-935-map-controls-bottom-nav-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13935';
const LAST = '#mapControls fieldset.mc-section:last-of-type';
const TARGET = LAST + ' .mc-jump-btn:last-child';
const LEGEND = LAST + ' > legend';

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

// Geometry of the panel, the bottom nav and the target, in viewport pixels.
function geometry(page, sel) {
  return page.evaluate((target) => {
    const r = (el) => { const b = el.getBoundingClientRect(); return { top: b.top, bottom: b.bottom, left: b.left, right: b.right }; };
    const panel = document.getElementById('mapControls');
    const nav = document.querySelector('.bottom-nav');
    const app = document.getElementById('app');
    const t = document.querySelector(target);
    const credit = document.querySelector('.leaflet-control-attribution');
    const navVisible = !!nav && getComputedStyle(nav).display !== 'none';
    return {
      panel: r(panel), app: r(app), target: r(t), creditTop: credit ? credit.getBoundingClientRect().top : null,
      navTop: navVisible ? nav.getBoundingClientRect().top : null,
      scrollTop: panel.scrollTop, scrollHeight: panel.scrollHeight, clientHeight: panel.clientHeight,
    };
  }, sel || TARGET);
}

// Real touch swipes on the panel until the target is above the nav and
// inside the panel. Returns the number of swipes.
async function swipeTargetIntoView(page, sel) {
  const cdp = await page.context().newCDPSession(page);
  let swipes = 0, stuck = 0, g = await geometry(page, sel);
  const visible = (x) => x.target.bottom <= Math.min(x.panel.bottom, x.navTop == null ? Infinity : x.navTop) + 1;
  try {
    while (!visible(g) && swipes < 40) {
      const x = Math.round((g.panel.left + g.panel.right) / 2);
      const dist = Math.max(30, Math.round((g.panel.bottom - g.panel.top) / 3));
      const y0 = Math.round((g.panel.top + g.panel.bottom) / 2 + dist / 2);
      await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [{ x, y: y0 }] });
      for (let k = 1; k <= 10; k++) {
        await cdp.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [{ x, y: y0 - Math.round(dist * k / 10) }] });
      }
      await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
      swipes++;
      await page.waitForTimeout(150);
      const before = g.scrollTop;
      g = await geometry(page, sel);
      stuck = g.scrollTop === before ? stuck + 1 : 0;
      if (stuck >= 2) break;
    }
  } finally { await cdp.detach(); }
  return { swipes, g };
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #935 map controls vs bottom nav against ${BASE} ===\n`);

  async function open(viewport, opts) {
    opts = opts || {};
    const touch = viewport.width < 1000;
    const ctx = await browser.newContext({ viewport, hasTouch: touch, isMobile: touch });
    const page = await ctx.newPage();
    const errors = [];
    page.on('pageerror', (e) => errors.push(e.message));
    if (opts.pills) {
      await ctx.route('**/api/healthz', (route) => route.fulfill({
        status: 200, contentType: 'application/json', headers: { 'X-Corescope-Load-Status': 'ready' },
        body: JSON.stringify({
          ready: true, from_pubkey_backfill: { done: true, processed: 1, total: 1 },
          ingest_liveness: { 'mqtt-eu': { lastReceiptUnix: Date.now() / 1000 - 3 * 3600 } },
        }),
      }));
    }
    await page.goto(BASE + '/#/map' + (opts.embed ? '?embed=1' : ''));
    await page.waitForSelector('#mapControlsToggle');
    await page.waitForSelector('.mc-jump-btn', { state: 'attached', timeout: 15000 });
    if (opts.pills) {
      await page.waitForFunction(() => document.querySelectorAll('#warmup-stale-pills .warmup-pill').length === 1, null, { timeout: 10000 });
    }
    let collapsed = null;
    if (touch) {
      await page.tap('#mapControlsToggle');
      await page.waitForTimeout(300);
      // The Quick Jump section starts collapsed: bring its legend into view with
      // real swipes before tapping it, so no programmatic scroll is involved.
      collapsed = await swipeTargetIntoView(page, LEGEND);
      collapsed.hit = await page.evaluate((sel) => {
        const el = document.querySelector(sel), b = el.getBoundingClientRect();
        const top = document.elementFromPoint(b.left + b.width / 2, b.bottom - 4);
        return { bottom: b.bottom, ok: !!top && el.contains(top) };
      }, LEGEND);
      await page.tap(LEGEND);
    }
    await page.waitForTimeout(300);
    return { ctx, page, errors, collapsed };
  }

  const phones = [
    { name: '375x560', viewport: { width: 375, height: 560 } },
    { name: '360x640', viewport: { width: 360, height: 640 } },
    { name: '640x360 landscape', viewport: { width: 640, height: 360 } },
    { name: '375x560 with the real stale-source pill row', viewport: { width: 375, height: 560 }, pills: true },
  ];

  for (const p of phones) {
    const { ctx, page, errors, collapsed } = await open(p.viewport, { pills: p.pills });
    await step(`${p.name}: the open panel ends above the bottom nav and inside the map`, async () => {
      const g = await geometry(page);
      assert(g.navTop != null, 'no bottom nav at this width');
      assert(g.panel.bottom <= g.navTop + 1, `panel bottom ${Math.round(g.panel.bottom)} is under the nav top ${Math.round(g.navTop)}`);
      assert(g.panel.bottom <= g.app.bottom + 1, `panel bottom ${Math.round(g.panel.bottom)} is past the map bottom ${Math.round(g.app.bottom)}`);
      assert(g.creditTop != null && g.panel.bottom <= g.creditTop + 1, `panel bottom ${Math.round(g.panel.bottom)} runs under the map credit at ${Math.round(g.creditTop)}`);
      assert(g.panel.top >= g.app.top - 1, `panel top ${Math.round(g.panel.top)} is above the map top ${Math.round(g.app.top)}`);
    });
    await step(`${p.name}: the collapsed Quick Jump legend is entirely above the nav after swipes, and a tap near its bottom edge lands on it`, async () => {
      const g = collapsed.g;
      assert(g.target.bottom <= g.navTop + 1, `after ${collapsed.swipes} swipe(s) the legend ends at ${Math.round(g.target.bottom)}, under the nav top ${Math.round(g.navTop)}`);
      assert(collapsed.hit.ok, `a hit test 4px above the legend's bottom edge (${Math.round(collapsed.hit.bottom)}) lands on something else (the nav covers it)`);
    });
    await step(`${p.name}: the last control is brought above the nav by real touch swipes and is not covered`, async () => {
      const { swipes, g } = await swipeTargetIntoView(page);
      assert(g.target.bottom <= g.navTop + 1, `after ${swipes} swipe(s) the last control ends at ${Math.round(g.target.bottom)}, under the nav top ${Math.round(g.navTop)}`);
      assert(g.target.bottom <= g.panel.bottom + 1, `after ${swipes} swipe(s) the last control is below the panel`);
      const hit = await page.evaluate((t) => {
        const el = document.querySelector(t), b = el.getBoundingClientRect();
        const top = document.elementFromPoint(b.left + b.width / 2, b.top + b.height / 2);
        return !!top && el.contains(top);
      }, TARGET);
      assert(hit, 'a hit test at the centre of the last control lands on something else');
    });
    await step(`${p.name}: a real tap on the last control, in place, reaches it`, async () => {
      await page.evaluate((t) => { const el = document.querySelector(t); el.__tapped = 0; el.addEventListener('click', () => { el.__tapped++; }); }, TARGET);
      // At the centre of the control, with no scrolling into view first.
      const b = await page.evaluate((t) => { const r = document.querySelector(t).getBoundingClientRect(); return { x: r.left + r.width / 2, y: r.top + r.height / 2 }; }, TARGET);
      await page.touchscreen.tap(b.x, b.y);
      const n = await page.evaluate((t) => document.querySelector(t).__tapped, TARGET);
      assert(n === 1, `the last control got ${n} click(s) from a tap`);
    });
    await step(`${p.name}: no page errors`, async () => { assert(errors.length === 0, errors.join('; ')); });
    await ctx.close();
  }

  {
    const vp = { width: 375, height: 560 };
    const { ctx, page, errors } = await open(vp, { embed: true });
    await step('375x560 embed: the panel is no shorter than the master rule (100vh - 80px) and stays inside the map', async () => {
      const g = await geometry(page);
      assert(g.navTop == null, 'bottom nav shows in embed mode');
      // max-height computes to a percentage; the panel's content exceeds the cap here, so its height is the cap.
      assert(g.panel.bottom - g.panel.top >= vp.height - 80 - 1, `panel height ${Math.round(g.panel.bottom - g.panel.top)}px is under master's ${vp.height - 80}px`);
      assert(g.panel.bottom <= g.app.bottom + 1, `panel bottom ${Math.round(g.panel.bottom)} is past the map bottom ${Math.round(g.app.bottom)}`);
      assert(g.creditTop != null && g.panel.bottom <= g.creditTop + 1, `panel bottom ${Math.round(g.panel.bottom)} runs under the map credit at ${Math.round(g.creditTop)}`);
    });
    await step('375x560 embed: the last control is reachable by real touch swipes', async () => {
      const { swipes, g } = await swipeTargetIntoView(page);
      assert(g.target.bottom <= g.panel.bottom + 1, `after ${swipes} swipe(s) the last control is below the panel`);
      assert(g.target.bottom <= g.app.bottom + 1, `after ${swipes} swipe(s) the last control is past the map bottom`);
    });
    await step('375x560 embed: no page errors', async () => { assert(errors.length === 0, errors.join('; ')); });
    await ctx.close();
  }

  {
    const { ctx, page } = await open({ width: 1440, height: 900 });
    await step('1440x900: panel unchanged (top 12px, inside the map, no bottom nav)', async () => {
      const g = await geometry(page);
      assert(g.navTop == null, 'bottom nav shows on desktop');
      assert(Math.round(g.panel.top - g.app.top) === 12, `panel top offset ${Math.round(g.panel.top - g.app.top)}`);
      assert(g.panel.bottom <= g.app.bottom - 12 + 1, `panel bottom ${Math.round(g.panel.bottom)} is past the map bottom ${Math.round(g.app.bottom)}`);
    });
    await ctx.close();
  }

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed\n`);
  process.exit(failed ? 1 : 0);
})();
