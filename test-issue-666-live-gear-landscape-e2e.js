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
      // Height of each top-right control box, in DOM order (zoom, layers,
      // fullscreen, gear). In the landscape row only zoom may be ~100px.
      ctrlHeights: [...document.querySelectorAll('#liveMap .leaflet-top.leaflet-right > .leaflet-control')]
        .map((c) => Math.round(c.getBoundingClientRect().height)),
      hScroll: document.documentElement.scrollWidth > document.documentElement.clientWidth,
      expanded: root.classList.contains('is-expanded'),
      collapsed: root.classList.contains('is-collapsed') && body.hasAttribute('hidden'),
      aria: gear.getAttribute('aria-expanded'),
      chips: body.querySelectorAll('.region-quick-pick').length,
      vw: window.innerWidth,
      maxWidth: getComputedStyle(header).maxWidth,
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
    !!document.querySelector('#vcrBar') && document.querySelector('#vcrBar').offsetHeight > 0 &&
    // Settled layout: live.js has measured the VCR bar (#1206) and set the
    // page height to innerHeight − bottom-nav. Before that the page briefly
    // keeps the CSS 100dvh − 52px height and every bottom-anchored overlay
    // (VCR bar, legend toggle) sits higher than it ends up.
    document.querySelector('.live-page').style.getPropertyValue('--vcr-bar-height') !== '' &&
    document.querySelector('.live-page').style.height !== '');
  return { ctx, page };
}

async function press(page, touch, x, y) {
  if (touch) await page.touchscreen.tap(x, y);
  else await page.mouse.click(x, y);
  await page.waitForTimeout(250);
}

async function runLandscape(browser, w, h, touch = true) {
  const tag = `[${w}x${h}${touch ? '' : ' desktop'}]`;
  const { ctx, page } = await openLive(browser, w, h, touch);
  let s = await gearState(page);
  await step(`${tag} collapsed: gear centre is hit-testable and ≥ 44px`, async () => {
    assert(s.collapsed && s.aria === 'false', JSON.stringify(s));
    assert(s.gw >= 44 && s.gh >= 44, `gear touch target ${s.gw}x${s.gh} < 44`);
    assert(s.hitOk, `elementFromPoint at gear centre is ${s.hitId}; gear=${s.gear}`);
  });
  await step(`${tag} collapsed: zoom, layers and fullscreen stay hit-testable`, async () => {
    for (const [k, v] of Object.entries(s.others)) assert(v === 'ok', `${k} covered by ${v}`);
  });
  await step(`${tag} collapsed: controls form one row with only zoom taller than a toggle`, async () => {
    const [zoom, ...rest] = s.ctrlHeights;
    assert(s.ctrlHeights.length === 4, `controls ${s.ctrlHeights}`);
    assert(s.gear[1] < 100, `gear top ${s.gear[1]}, expected the row at the top`);
    assert(zoom >= 90 && rest.every((x) => x <= 56), `control heights ${s.ctrlHeights} (layers/fullscreen/gear stretched?)`);
  });
  await step(`${tag} collapsed: no horizontal page scroll`, async () => {
    assert(!s.hScroll, 'documentElement scrollWidth > clientWidth');
  });
  const verb = touch ? 'tap' : 'click';
  for (let round = 1; round <= 2; round++) {
    await press(page, touch, s.cx, s.cy);
    s = await gearState(page);
    await step(`${tag} round ${round}: a ${verb} on the gear opens the panel`, async () => {
      assert(s.expanded && s.aria === 'true', JSON.stringify(s));
      assert(s.chips === GROUPS.length, `chips=${s.chips}`);
    });
    await step(`${tag} round ${round}: expanded panel does not cover the gear`, async () => {
      assert(s.hitOk, `elementFromPoint at gear centre is ${s.hitId}; gear=${s.gear} panel=${s.panel}`);
      assert(!s.overlap, `panel ${s.panel} intersects gear ${s.gear}`);
      assert(!s.stackOverlap, `panel ${s.panel} intersects the zoom/layers/fullscreen controls`);
      assert(s.panel[0] >= 0 && s.panel[2] <= s.vw, `panel ${s.panel} outside viewport ${s.vw}`);
      assert(!s.hScroll, 'documentElement scrollWidth > clientWidth');
    });
    // Only a panel that really opened can prove the close; otherwise the
    // collapsed end state below would pass vacuously.
    const wasOpen = s.expanded && s.aria === 'true';
    const before = JSON.stringify({ expanded: s.expanded, aria: s.aria });
    await press(page, touch, s.cx, s.cy);
    s = await gearState(page);
    await step(`${tag} round ${round}: a ${verb} on the gear closes the open panel`, async () => {
      assert(wasOpen, `panel was not open before the ${verb}: ${before}`);
      assert(s.collapsed && !s.expanded && s.aria === 'false', JSON.stringify(s));
    });
    if (!s.collapsed) break; // a second round would only repeat the failure
  }
  // Fullscreen (#1572) hides the gear and the controls body; the #666 rail
  // must not narrow the header there even if the controls stay expanded.
  await press(page, touch, s.cx, s.cy);
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

// #666 F5: at 720×300 the legend toggle reaches 9px into the row's right
// end; both must still answer at their own centres.
async function runLegendToggle(browser, w, h) {
  const tag = `[${w}x${h} desktop]`;
  const { ctx, page } = await openLive(browser, w, h, false);
  const r = await page.evaluate(() => {
    const centreHit = (el) => {
      const b = el.getBoundingClientRect();
      const hit = document.elementFromPoint(b.left + b.width / 2, b.top + b.height / 2);
      return { ok: !!hit && el.contains(hit), id: hit ? ((hit.closest('[id]') || {}).id + ' > ' + (hit.getAttribute('class') || hit.tagName)) : null,
        rect: [b.left, b.top, b.right, b.bottom].map(Math.round) };
    };
    return { gear: centreHit(document.getElementById('liveControlsToggle')),
      legend: centreHit(document.getElementById('legendToggleBtn')) };
  });
  await step(`${tag} gear and legend toggle are both hit-testable at their centres`, async () => {
    assert(r.gear.ok, `gear ${r.gear.rect} centre hits ${r.gear.id}; legend toggle ${r.legend.rect}`);
    assert(r.legend.ok, `legend toggle ${r.legend.rect} centre hits ${r.legend.id}`);
  });
  await ctx.close();
}

// #666 F3: a short portrait phone (height ≤ 500px) is outside the landscape
// query. It keeps the column, and the expanded header keeps the #664 rail
// (100vw − 12px − 68px) instead of collapsing to the landscape row rail.
async function runShortPortrait(browser, w, h) {
  const tag = `[${w}x${h}]`;
  const { ctx, page } = await openLive(browser, w, h, true);
  let s = await gearState(page);
  await step(`${tag} portrait keeps the control column`, async () => {
    assert(s.gear[0] === w - 60 && s.gear[1] === 306, `gear at ${s.gear}, expected (${w - 60}, 306)`);
  });
  // The gear may sit under the VCR bar at this height (pre-existing, not
  // #666's landscape case), so open the panel through the element itself.
  await page.$eval('#liveControlsToggle', (el) => el.click());
  await page.waitForTimeout(250);
  s = await gearState(page);
  await step(`${tag} expanded header keeps the #664 width and stays inside the viewport`, async () => {
    assert(s.expanded, JSON.stringify(s));
    assert(s.maxWidth === (w - 80) + 'px', `max-width ${s.maxWidth}, expected ${w - 80}px`);
    assert(s.panel[0] >= 0 && s.panel[2] <= w && s.panel[3] <= h, `panel ${s.panel} outside ${w}x${h}`);
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
  // Phones (touch), then short desktop windows (mouse): 1024×480 is a low
  // desktop window, 1280×500 the upper bound of the max-height query.
  for (const [w, h, touch] of [[812, 375, true], [667, 375, true], [640, 360, true], [568, 320, true],
    [1024, 480, false], [1280, 500, false]]) {
    try { await runLandscape(browser, w, h, touch); }
    catch (e) { failed++; console.error(`  ✗ [${w}x${h}] fatal: ` + e.message); }
  }
  try { await runLegendToggle(browser, 720, 300); }
  catch (e) { failed++; console.error('  ✗ [720x300] fatal: ' + e.message); }
  try { await runShortPortrait(browser, 320, 480); }
  catch (e) { failed++; console.error('  ✗ [320x480] fatal: ' + e.message); }
  // One pixel above the query (1280×501) keeps the column, like portrait and
  // a full-height desktop.
  for (const [w, h, touch] of [[375, 812, true], [1440, 900, false], [1280, 501, false]]) {
    try { await runUnchanged(browser, w, h, touch); }
    catch (e) { failed++; console.error(`  ✗ [${w}x${h}] fatal: ` + e.message); }
  }
  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
