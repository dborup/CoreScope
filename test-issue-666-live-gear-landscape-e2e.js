// test-issue-666-live-gear-landscape-e2e.js
//
// #666 — in landscape on a phone (812×375, 667×375) the Live gear toggle
// (#liveControlsToggle) was not hit-testable at all, even with the panel
// collapsed. The Leaflet top-right stack (zoom, layers, fullscreen, gear) is
// one column from y = 74 to y = 356, so in a 375px-tall viewport the gear
// (y 304–356) sat under the VCR bar (812) or below the clip of the 317px
// tall .live-page (667, where the bottom-nav took the tap instead).
//
// Region groups and areas are answered in the browser, as in the #664 E2E,
// so the expanded panel is as tall as it gets on a real deployment.
//
// Landscape (812×375 and 667×375 touch, plus 640×360 and 568×320 where the
// ≤640px header stretches edge to edge):
//   - the gear centre is hit-testable while collapsed and while expanded,
//     the gear is ≥ 44×44 and does not intersect the panel
//   - open → close twice with real taps on the gear centre
//   - the expanded panel does not intersect any top-right control
//   - zoom, layers and fullscreen stay hit-testable too
//   - in fullscreen (#1572) the header keeps its pre-#666 max-width
// Portrait 375×812 and desktop 1440×900: the control stack keeps its
// pre-#666 position (gear at right − 60, top 306).
//
// Usage: BASE_URL=http://localhost:13581 node test-issue-666-live-gear-landscape-e2e.js
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
    // The other top-right controls must not be traded away for the gear.
    const others = {};
    for (const [k, sel] of [['zoomIn', '.leaflet-control-zoom-in'], ['layers', '.leaflet-control-layers'],
      ['fullscreen', '#liveFullscreenToggle']]) {
      const el = document.querySelector('#liveMap ' + sel);
      const r = el && el.getBoundingClientRect();
      const o = r && document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2);
      others[k] = !!o && el.contains(o) ? 'ok' : (o ? (o.id || o.getAttribute('class') || o.tagName) : 'none');
    }
    return {
      cx, cy, gw: g.width, gh: g.height,
      gear: [g.left, g.top, g.right, g.bottom].map(Math.round),
      panel: [h.left, h.top, h.right, h.bottom].map(Math.round),
      hitOk: !!hit && gear.contains(hit),
      hitId: hit ? (hit.id || hit.getAttribute('class') || hit.tagName) : null,
      overlap: g.left < h.right && h.left < g.right && g.top < h.bottom && h.top < g.bottom,
      // Every control in the top-right stack, not just the gear.
      stackOverlap: [...document.querySelectorAll('#liveMap .leaflet-top.leaflet-right > .leaflet-control')]
        .map((c) => c.getBoundingClientRect())
        .some((r) => r.left < h.right && h.left < r.right && r.top < h.bottom && h.top < r.bottom),
      others,
      expanded: root.classList.contains('is-expanded'),
      collapsed: root.classList.contains('is-collapsed') && body.hasAttribute('hidden'),
      aria: gear.getAttribute('aria-expanded'),
      chips: body.querySelectorAll('.region-quick-pick').length,
      vw: window.innerWidth,
    };
  });
}

async function openLive(browser, w, h, touch) {
  const ctx = await browser.newContext({ viewport: { width: w, height: h }, hasTouch: touch, isMobile: touch });
  await ctx.route((u) => u.pathname === '/api/config/regions/groups', (r) =>
    r.fulfill({ contentType: 'application/json', body: JSON.stringify(GROUPS) }));
  await ctx.route((u) => u.pathname === '/api/config/areas', (r) =>
    r.fulfill({ contentType: 'application/json', body: JSON.stringify(AREAS) }));
  await ctx.addInitScript(() => { try { localStorage.removeItem('live-controls-expanded'); } catch (_) {} });
  const page = await ctx.newPage();
  page.setDefaultTimeout(15000);
  page.on('pageerror', (e) => console.error('    [pageerror]', e.message));
  await page.goto(BASE + '/#/live', { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#liveControlsToggle');
  await page.waitForFunction(() =>
    document.querySelectorAll('#liveControlsBody .region-quick-pick').length > 0 &&
    !!document.querySelector('#liveAreaFilter .region-dropdown-trigger') &&
    !!document.querySelector('#vcrBar') && document.querySelector('#vcrBar').offsetHeight > 0);
  return { ctx, page };
}

async function runLandscape(browser, w, h) {
  const tag = `[${w}x${h}]`;
  const { ctx, page } = await openLive(browser, w, h, true);
  let s = await gearState(page);
  await step(`${tag} collapsed: gear centre is hit-testable and ≥ 44px`, async () => {
    assert(s.collapsed && s.aria === 'false', JSON.stringify(s));
    assert(s.gw >= 44 && s.gh >= 44, `gear touch target ${s.gw}x${s.gh} < 44`);
    assert(s.hitOk, `elementFromPoint at gear centre is ${s.hitId}; gear=${s.gear}`);
  });
  await step(`${tag} collapsed: zoom, layers and fullscreen stay hit-testable`, async () => {
    for (const [k, v] of Object.entries(s.others)) assert(v === 'ok', `${k} covered by ${v}`);
  });
  for (let round = 1; round <= 2; round++) {
    await page.touchscreen.tap(s.cx, s.cy);
    await page.waitForTimeout(250);
    s = await gearState(page);
    await step(`${tag} round ${round}: a tap on the gear opens the panel`, async () => {
      assert(s.expanded && s.aria === 'true', JSON.stringify(s));
      assert(s.chips === GROUPS.length, `chips=${s.chips}`);
    });
    await step(`${tag} round ${round}: expanded panel does not cover the gear`, async () => {
      assert(s.hitOk, `elementFromPoint at gear centre is ${s.hitId}; gear=${s.gear} panel=${s.panel}`);
      assert(!s.overlap, `panel ${s.panel} intersects gear ${s.gear}`);
      assert(!s.stackOverlap, `panel ${s.panel} intersects the zoom/layers/fullscreen controls`);
      assert(s.panel[0] >= 0 && s.panel[2] <= s.vw, `panel ${s.panel} outside viewport ${s.vw}`);
    });
    await page.touchscreen.tap(s.cx, s.cy);
    await page.waitForTimeout(250);
    s = await gearState(page);
    await step(`${tag} round ${round}: a tap on the gear closes the panel`, async () => {
      assert(s.collapsed && !s.expanded && s.aria === 'false', JSON.stringify(s));
    });
    if (!s.collapsed) break; // a second round would only repeat the failure
  }
  // Fullscreen (#1572) hides the gear and the controls body; the #666 rail
  // must not narrow the header there even if the controls stay expanded.
  await page.touchscreen.tap(s.cx, s.cy);
  await page.waitForTimeout(250);
  await page.click('#liveFullscreenToggle');
  await page.waitForFunction(() => document.body.classList.contains('live-fullscreen'));
  await step(`${tag} fullscreen: expanded controls leave the header width as before #666`, async () => {
    const mw = await page.$eval('#liveHeader', (el) => getComputedStyle(el).maxWidth);
    const expected = w - (w <= 640 ? 16 : 24);
    assert(mw === expected + 'px', `max-width ${mw}, expected ${expected}px`);
  });
  await ctx.close();
}

async function runUnchanged(browser, w, h, touch) {
  const tag = `[${w}x${h}]`;
  const { ctx, page } = await openLive(browser, w, h, touch);
  const s = await gearState(page);
  await step(`${tag} control stack keeps its pre-#666 position`, async () => {
    assert(s.gear[0] === w - 60 && s.gear[1] === 306, `gear at ${s.gear}, expected (${w - 60}, 306)`);
    assert(s.hitOk, `elementFromPoint at gear centre is ${s.hitId}`);
  });
  await ctx.close();
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #666 live gear tappable in landscape E2E against ${BASE} ===`);
  for (const [w, h] of [[812, 375], [667, 375], [640, 360], [568, 320]]) {
    try { await runLandscape(browser, w, h); }
    catch (e) { failed++; console.error(`  ✗ [${w}x${h}] fatal: ` + e.message); }
  }
  for (const [w, h, touch] of [[375, 812, true], [1440, 900, false]]) {
    try { await runUnchanged(browser, w, h, touch); }
    catch (e) { failed++; console.error(`  ✗ [${w}x${h}] fatal: ` + e.message); }
  }
  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
