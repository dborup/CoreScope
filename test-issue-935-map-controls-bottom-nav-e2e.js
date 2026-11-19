/**
 * E2E (#935): the map-control panel on a phone ends above the bottom nav.
 *
 * Every scenario is a fresh browser context (cold start, empty storage) with
 * touch emulation. At phone widths the panel's max-height ignored
 * --bottom-nav-reserve and --warmup-pills-h, so its last section ("Quick
 * Jump") could never be scrolled above the bottom nav.
 *
 * Asserts, at 375x560, 360x640 and 640x360 (landscape), and at 375x560 with
 * the stale-source pill row showing (--warmup-pills-h set):
 *   1. the open panel ends above the bottom nav and inside the map;
 *   2. the last control, brought into view by real touch swipes only (a panel
 *      a finger cannot scroll fails), is not covered by the nav, and a hit
 *      test at its centre lands on it;
 *   3. a real tap on that control reaches it (it receives the click);
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

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

// Geometry of the panel, the bottom nav and the target, in viewport pixels.
function geometry(page) {
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
  }, TARGET);
}

// Real touch swipes on the panel until the target is above the nav and
// inside the panel. Returns the number of swipes.
async function swipeTargetIntoView(page) {
  const cdp = await page.context().newCDPSession(page);
  let swipes = 0, stuck = 0, g = await geometry(page);
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
      g = await geometry(page);
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
    await page.goto(BASE + '/#/map');
    await page.waitForSelector('#mapControlsToggle');
    await page.waitForSelector('.mc-jump-btn', { state: 'attached', timeout: 15000 });
    if (opts.pills) {
      await page.evaluate((h) => document.documentElement.style.setProperty('--warmup-pills-h', h + 'px'), opts.pills);
    }
    if (touch) {
      await page.tap('#mapControlsToggle');
      await page.tap(LAST + ' > legend');
    }
    await page.waitForTimeout(300);
    return { ctx, page, errors };
  }

  const phones = [
    { name: '375x560', viewport: { width: 375, height: 560 } },
    { name: '360x640', viewport: { width: 360, height: 640 } },
    { name: '640x360 landscape', viewport: { width: 640, height: 360 } },
    { name: '375x560 with the stale-source pills (40px)', viewport: { width: 375, height: 560 }, pills: 40 },
  ];

  for (const p of phones) {
    const { ctx, page, errors } = await open(p.viewport, { pills: p.pills });
    await step(`${p.name}: the open panel ends above the bottom nav and inside the map`, async () => {
      const g = await geometry(page);
      assert(g.navTop != null, 'no bottom nav at this width');
      assert(g.panel.bottom <= g.navTop + 1, `panel bottom ${Math.round(g.panel.bottom)} is under the nav top ${Math.round(g.navTop)}`);
      assert(g.panel.bottom <= g.app.bottom + 1, `panel bottom ${Math.round(g.panel.bottom)} is past the map bottom ${Math.round(g.app.bottom)}`);
      assert(g.creditTop != null && g.panel.bottom <= g.creditTop + 1, `panel bottom ${Math.round(g.panel.bottom)} runs under the map credit at ${Math.round(g.creditTop)}`);
      assert(g.panel.top >= g.app.top - 1, `panel top ${Math.round(g.panel.top)} is above the map top ${Math.round(g.app.top)}`);
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
