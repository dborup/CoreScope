// test-issue-664-live-gear-mobile-e2e.js
//
// #664 — on a phone the expanded Live controls panel (#liveHeader) grew over
// the gear toggle (#liveControlsToggle) in the Leaflet top-right stack, so a
// tap on the gear landed on the panel and the panel could not be closed.
//
// The panel only gets tall enough when region groups and areas are rendered,
// so both endpoints are answered in the browser (same approach as
// test-issue-416-region-quick-picks-e2e.js); the fixture stays untouched.
//
// Per viewport (375×812 and 320×568 touch, 768×900 touch, 1440×900 desktop):
//   - open the panel via a real tap/click on the gear centre
//   - the gear centre is hit-testable (elementFromPoint is the gear or inside)
//     and the gear rect does not intersect the expanded panel rect
//   - close it with a real tap/click at the same point; the panel is
//     collapsed and aria-expanded="false"
//   - open → close twice
// Desktop also asserts the expanded header keeps its pre-#664 geometry.
//
// Usage: BASE_URL=http://localhost:13581 node test-issue-664-live-gear-mobile-e2e.js
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const GROUPS = [
  { name: 'Bay Area', codes: ['SJC', 'SFO'] },
  { name: 'Peninsula', codes: ['SFO'] },
  { name: 'East Bay', codes: ['OAK'] },
  { name: 'Monterey', codes: ['MRY'] },
];
const AREAS = [{ key: 'north', label: 'North' }, { key: 'south', label: 'South' }];

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

function gearState(page) {
  return page.evaluate(() => {
    const gear = document.getElementById('liveControlsToggle');
    const header = document.getElementById('liveHeader');
    const root = document.getElementById('liveControls');
    const body = document.getElementById('liveControlsBody');
    const g = gear.getBoundingClientRect();
    const h = header.getBoundingClientRect();
    const cx = g.left + g.width / 2, cy = g.top + g.height / 2;
    const hit = document.elementFromPoint(cx, cy);
    const overlap = g.left < h.right && h.left < g.right && g.top < h.bottom && h.top < g.bottom;
    return {
      cx, cy, gw: g.width, gh: g.height,
      gear: [g.left, g.top, g.right, g.bottom].map(Math.round),
      panel: [h.left, h.top, h.right, h.bottom].map(Math.round),
      hitOk: !!hit && gear.contains(hit),
      hitId: hit ? (hit.id || hit.tagName) : null,
      overlap,
      expanded: root.classList.contains('is-expanded'),
      collapsed: root.classList.contains('is-collapsed') && body.hasAttribute('hidden'),
      aria: gear.getAttribute('aria-expanded'),
      chips: body.querySelectorAll('.region-quick-pick').length,
      area: !!body.querySelector('#liveAreaFilter .region-dropdown-trigger'),
      vw: window.innerWidth,
      maxWidth: getComputedStyle(header).maxWidth,
    };
  });
}

async function pressGear(page, touch, s) {
  if (touch) await page.touchscreen.tap(s.cx, s.cy);
  else await page.mouse.click(s.cx, s.cy);
  await page.waitForTimeout(250);
}

async function runViewport(browser, w, h, touch) {
  const tag = `[${w}x${h}]`;
  const ctx = await browser.newContext({ viewport: { width: w, height: h }, hasTouch: touch, isMobile: touch });
  await ctx.route((u) => u.pathname === '/api/config/regions/groups', (r) =>
    r.fulfill({ contentType: 'application/json', body: JSON.stringify(GROUPS) }));
  await ctx.route((u) => u.pathname === '/api/config/areas', (r) =>
    r.fulfill({ contentType: 'application/json', body: JSON.stringify(AREAS) }));
  // Start collapsed regardless of a persisted #1532 pin state.
  await ctx.addInitScript(() => { try { localStorage.removeItem('live-controls-expanded'); } catch (_) {} });
  const page = await ctx.newPage();
  page.setDefaultTimeout(15000);
  page.on('pageerror', (e) => console.error('    [pageerror]', e.message));

  await page.goto(BASE + '/#/live', { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#liveControlsToggle');
  await page.waitForFunction(() =>
    document.querySelectorAll('#liveControlsBody .region-quick-pick').length > 0 &&
    !!document.querySelector('#liveAreaFilter .region-dropdown-trigger'));

  let stuck = false;
  for (let round = 1; round <= 2; round++) {
    let s = await gearState(page);
    await step(`${tag} round ${round}: starts collapsed`, async () => {
      assert(s.collapsed && s.aria === 'false', JSON.stringify(s));
      assert(s.gw >= 44 && s.gh >= 44, `gear touch target ${s.gw}x${s.gh} < 44`);
    });
    await pressGear(page, touch, s);
    s = await gearState(page);
    await step(`${tag} round ${round}: gear opens the panel with region groups + area rendered`, async () => {
      assert(s.expanded && s.aria === 'true', JSON.stringify(s));
      assert(s.chips === GROUPS.length && s.area, `chips=${s.chips} area=${s.area}`);
    });
    await step(`${tag} round ${round}: expanded panel does not cover the gear`, async () => {
      assert(s.hitOk, `elementFromPoint at gear centre is #${s.hitId}; gear=${s.gear} panel=${s.panel}`);
      assert(!s.overlap, `panel ${s.panel} intersects gear ${s.gear}`);
      assert(s.panel[0] >= 0 && s.panel[2] <= s.vw, `panel ${s.panel} outside viewport ${s.vw}`);
    });
    if (!touch) {
      await step(`${tag} round ${round}: desktop header geometry unchanged`, async () => {
        assert(s.maxWidth === (s.vw - 24) + 'px', `max-width ${s.maxWidth}, expected ${s.vw - 24}px`);
        assert(s.panel[0] === 12 && s.panel[1] === 64, `panel origin ${s.panel}`);
      });
    }
    await pressGear(page, touch, s);
    s = await gearState(page);
    await step(`${tag} round ${round}: a ${touch ? 'tap' : 'click'} on the gear closes the panel`, async () => {
      assert(s.collapsed && !s.expanded && s.aria === 'false', JSON.stringify(s));
    });
    if (!s.collapsed) { stuck = true; break; } // a second round would only repeat the failure
  }
  if (stuck) { await ctx.close(); return; }
  if (process.env.SHOT_DIR && w === 375) {
    await pressGear(page, touch, await gearState(page));
    await page.screenshot({ path: `${process.env.SHOT_DIR}/664-${w}.png` });
    await pressGear(page, touch, await gearState(page));
  }
  if (touch && w <= 640) {
    // Fullscreen (#1572) hides the gear and the controls body; the #664 rail
    // must not narrow the header there even if the controls stay expanded.
    await pressGear(page, touch, await gearState(page));
    await page.click('#liveFullscreenToggle');
    await page.waitForFunction(() => document.body.classList.contains('live-fullscreen'));
    await step(`${tag} fullscreen: expanded controls leave the header width as before #664`, async () => {
      const mw = await page.$eval('#liveHeader', (el) => getComputedStyle(el).maxWidth);
      assert(mw === (w - 16) + 'px', `max-width ${mw}, expected ${w - 16}px`);
    });
  }
  await ctx.close();
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #664 live gear reachable on mobile E2E against ${BASE} ===`);
  for (const [w, h, touch] of [[375, 812, true], [320, 568, true], [768, 900, true], [1440, 900, false]]) {
    try { await runViewport(browser, w, h, touch); }
    catch (e) { failed++; console.error(`  ✗ [${w}x${h}] fatal: ` + e.message); }
  }
  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
