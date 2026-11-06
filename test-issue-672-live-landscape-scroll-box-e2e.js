// test-issue-672-live-landscape-scroll-box-e2e.js
//
// #672 — in short landscape between 641 and 768px wide (height ≤ 500px) the
// capped Live controls panel (#667) got a scroll box of only 43–83px. The
// top-nav (52px), the 48px chart toggle in the header row and the 60px VCR
// LCD took the height; at ≤ 640px the #1234 header layout hides the first
// two, and the mobile VCR shrinks the LCD.
//
// Region groups are answered in the browser, as in the #667 E2E. Taps are
// real touch taps at the element's centre.
//
// Steps:
//   - band 653×320, 700×360, 740×360, 700×480 (16 groups): the band layout
//     (no top-nav, no chart toggle, mobile LCD); the capped scroll box is
//     ≥ 150px; scrolled to the end, a tap on the last control (Area
//     trigger) opens its menu; no horizontal page scroll. 700×480 is the
//     tallest case and pins the max-height: 500px ceiling (#678 review N1)
//   - band 653×320: the gear stays ≥ 44px and hit-testable in the #666
//     landscape row (same top as the zoom control, below the 56px offset),
//     the bottom-nav is shown, and the VCR buttons keep the sizes they have
//     at 769px
//   - boundaries: 640×320 keeps the ≤ 640px layout and the capped header
//     offset, reserve and box it has on master (#678 review N3); 769×360
//     keeps the top-nav, the MESH LIVE title and the full LCD
//   - guards: a 768×1024 portrait tablet (chart toggle) and a 1280×500
//     desktop window (title) keep the top-nav, the header and the full LCD
//
// Usage: BASE_URL=http://localhost:13581 node test-issue-672-live-landscape-scroll-box-e2e.js
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const MIN_BOX = 150;
const MOBILE_LCD = 74;
// N3 (#678 review): 640×320 is outside #672's change, so its capped panel
// must match master, not just clear MIN_BOX (152 vs 150 left 2px). The box
// is innerHeight − --live-ctrl-top − --live-ctrl-reserve − 12px gutter. The
// CSS inputs are pinned exactly: the #1234 header at top 8px with 4px
// padding, and a 112px reserve (58px bottom-nav + 54px VCR bar). Only the
// header row above the body depends on font metrics: 36px on macOS, 35px on
// Linux CI (box 152 vs 153), so that offset gets that 1px range.
const AT_640 = { headerTop: 8, padTop: '4px', ctrlReserve: '112px', rowMin: 35, rowMax: 36, gutter: 12 };
const groups = (n) => Array.from({ length: n }, (_, i) => ({ name: 'Region group ' + (i + 1), codes: ['SJC'] }));
const AREAS = [{ key: 'north', label: 'North' }, { key: 'south', label: 'South' }];

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

function state(page) {
  return page.evaluate(() => {
    const shown = (el) => !!el && el.getClientRects().length > 0 && getComputedStyle(el).visibility !== 'hidden';
    const rect = (el) => { const r = el.getBoundingClientRect(); return [r.left, r.top, r.right, r.bottom].map(Math.round); };
    const centre = (el) => { const r = el.getBoundingClientRect(); return [r.left + r.width / 2, r.top + r.height / 2]; };
    const hitOk = (el) => {
      if (!el) return false;
      const [x, y] = centre(el);
      const hit = document.elementFromPoint(x, y);
      return !!hit && el.contains(hit);
    };
    const body = document.getElementById('liveControlsBody');
    const gear = document.getElementById('liveControlsToggle');
    const zoom = document.querySelector('.live-page .leaflet-control-zoom');
    const area = document.querySelector('#liveAreaFilter .region-dropdown-trigger');
    const menu = document.querySelector('#liveAreaFilter .area-dropdown-menu');
    const canvas = document.querySelector('.vcr-lcd-canvas');
    return {
      capped: document.getElementById('liveControls').classList.contains('is-capped'),
      expanded: document.getElementById('liveControls').classList.contains('is-expanded'),
      box: body.clientHeight, scrollTop: body.scrollTop, scrollRange: body.scrollHeight - body.clientHeight,
      bodyRect: rect(body),
      topNav: shown(document.querySelector('.top-nav')),
      bottomNav: shown(document.querySelector('.bottom-nav')),
      chartToggle: shown(document.getElementById('liveHeaderToggle')),
      title: shown(document.querySelector('#liveHeader .live-title')),
      lcdWidth: canvas ? Math.round(canvas.getBoundingClientRect().width) : null,
      vcrBtns: [...document.querySelectorAll('#vcrBar button')].filter(shown).map((b) => b.id + ':' + b.offsetWidth + 'x' + b.offsetHeight),
      gear: rect(gear), gearCentre: centre(gear), gearHit: hitOk(gear), gearSize: [gear.offsetWidth, gear.offsetHeight],
      zoomTop: zoom ? Math.round(zoom.getBoundingClientRect().top) : null,
      area: area && rect(area), areaCentre: area && centre(area), areaHit: hitOk(area),
      menuOpen: !!menu && !menu.hidden,
      hScroll: document.documentElement.scrollWidth > document.documentElement.clientWidth,
      ctrlTop: document.querySelector('.live-page').style.getPropertyValue('--live-ctrl-top'),
      vh: window.innerHeight,
      headerTop: Math.round(document.getElementById('liveHeader').getBoundingClientRect().top),
      headerPadTop: getComputedStyle(document.getElementById('liveHeader')).paddingTop,
      ctrlReserve: document.querySelector('.live-page').style.getPropertyValue('--live-ctrl-reserve'),
    };
  });
}

function frames(page, n) {
  return page.evaluate((n) => new Promise((resolve) => {
    (function tick(i) { if (!i) resolve(); else requestAnimationFrame(() => tick(i - 1)); })(n);
  }), n);
}

async function openLive(browser, w, h, touch) {
  const ctx = await browser.newContext({ viewport: { width: w, height: h }, hasTouch: touch, isMobile: touch });
  const G = groups(16);
  await ctx.route((u) => u.pathname === '/api/config/regions/groups', (r) =>
    r.fulfill({ contentType: 'application/json', body: JSON.stringify(G) }));
  await ctx.route((u) => u.pathname === '/api/config/areas', (r) =>
    r.fulfill({ contentType: 'application/json', body: JSON.stringify(AREAS) }));
  await ctx.addInitScript(() => {
    try { localStorage.removeItem('live-controls-expanded'); localStorage.removeItem('live-fullscreen'); } catch (_) {}
  });
  const page = await ctx.newPage();
  page.setDefaultTimeout(15000);
  page.on('pageerror', (e) => console.error('    [pageerror]', e.message));
  await page.goto(BASE + '/#/live', { waitUntil: 'domcontentloaded' });
  // Settled layout (see the #666 E2E): live.js has measured the VCR bar and
  // set the page height to innerHeight − bottom-nav.
  await page.waitForFunction(() =>
    document.querySelectorAll('#liveControlsBody .region-quick-pick').length === 16 &&
    !!document.querySelector('#liveAreaFilter .region-dropdown-trigger') &&
    !!document.querySelector('#vcrBar') && document.querySelector('#vcrBar').offsetHeight > 0 &&
    document.querySelector('.live-page').style.getPropertyValue('--vcr-bar-height') !== '' &&
    document.querySelector('.live-page').style.height !== '');
  return { ctx, page };
}

async function tap(page, touch, [x, y]) {
  if (touch) await page.touchscreen.tap(x, y);
  else await page.mouse.click(x, y);
  await page.waitForTimeout(250);
  await frames(page, 4);
}

async function openPanel(page, touch) {
  await tap(page, touch, (await state(page)).gearCentre);
  await page.waitForFunction(() => document.getElementById('liveControls').classList.contains('is-expanded'));
  await frames(page, 4);
  return state(page);
}

// Scroll the capped body to its end with the wheel at the box centre (the
// #667 E2E covers touch drags), then wait for the position to hold.
async function scrollToEnd(page, s) {
  const x = (s.bodyRect[0] + s.bodyRect[2]) / 2, y = (s.bodyRect[1] + s.bodyRect[3]) / 2;
  await page.mouse.move(x, y);
  for (let i = 0; i < 20; i++) {
    s = await state(page);
    if (s.scrollTop >= s.scrollRange - 1) break;
    await page.mouse.wheel(0, 200);
    await page.waitForTimeout(80);
  }
  await frames(page, 6);
  return state(page);
}

let vcrAt769 = null;

async function runBand(browser, w, h) {
  const tag = `[${w}x${h}]`;
  const { ctx, page } = await openLive(browser, w, h, true);
  let s = await openPanel(page, true);
  // N1 (#678 review): pins the band's height ceiling, which the box check
  // alone does not — out of the band 700×480 still gets a ≥ 150px box.
  await step(`${tag} in the band: no top-nav, no chart toggle, mobile LCD`, async () => {
    assert(!s.topNav && !s.chartToggle, `topNav ${s.topNav}, chartToggle ${s.chartToggle}`);
    assert(s.lcdWidth === MOBILE_LCD, `LCD canvas ${s.lcdWidth}px wide, expected ${MOBILE_LCD}px`);
  });
  await step(`${tag} capped panel: scroll box is ≥ ${MIN_BOX}px`, async () => {
    assert(s.capped, 'panel not capped (16 groups should not fit)');
    assert(s.box >= MIN_BOX, `scroll box ${s.box}px < ${MIN_BOX}px`);
  });
  await step(`${tag} scrolled to the end, a tap on the last control (Area trigger) opens its menu`, async () => {
    s = await scrollToEnd(page, s);
    assert(s.scrollTop >= s.scrollRange - 1, `scrollTop ${s.scrollTop} of ${s.scrollRange}`);
    assert(s.area[1] >= s.bodyRect[1] - 1 && s.area[3] <= s.bodyRect[3] + 1, `Area trigger ${s.area} outside box ${s.bodyRect}`);
    assert(s.areaHit, 'Area trigger not hit-testable');
    await tap(page, true, s.areaCentre);
    s = await state(page);
    assert(s.menuOpen, 'Area menu did not open');
  });
  await step(`${tag} no horizontal page scroll`, async () => assert(!s.hScroll, 'page scrolls sideways'));
  if (w === 653) {
    await step(`${tag} gear ≥ 44px and hit-testable in the #666 landscape row`, async () => {
      assert(s.gearSize[0] >= 44 && s.gearSize[1] >= 44, `gear ${s.gearSize}`);
      assert(s.gearHit, 'gear not hit-testable');
      assert(Math.abs(s.zoomTop - s.gear[1]) <= 4, `gear top ${s.gear[1]} not in the zoom row (${s.zoomTop})`);
      assert(s.gear[1] >= 56, `gear top ${s.gear[1]} moved above the 56px control offset`);
    });
    await step(`${tag} navigation stays reachable through the bottom-nav`, async () =>
      assert(s.bottomNav, 'bottom-nav hidden'));
    await step(`${tag} VCR buttons keep their 769px sizes`, async () => {
      assert(vcrAt769, 'no 769px reference');
      assert(s.vcrBtns.join() === vcrAt769.join(), `${s.vcrBtns} vs ${vcrAt769}`);
    });
  }
  await ctx.close();
}

async function runBoundary640(browser) {
  const { ctx, page } = await openLive(browser, 640, 320, true);
  const closed = await state(page);
  await step('[640x320] keeps the ≤ 640px layout: no top-nav, no chart toggle', async () =>
    assert(!closed.topNav && !closed.chartToggle, `topNav ${closed.topNav}, chartToggle ${closed.chartToggle}`));
  const s = await openPanel(page, true);
  await step('[640x320] capped panel unchanged from master: header at 8px, 112px reserve, box from the cap formula', async () => {
    assert(s.capped, 'panel not capped');
    const top = parseInt(s.ctrlTop, 10), reserve = parseInt(s.ctrlReserve, 10);
    const row = top - s.headerTop;
    const got = { headerTop: s.headerTop, padTop: s.headerPadTop, ctrlTop: s.ctrlTop, ctrlReserve: s.ctrlReserve, box: s.box };
    assert(s.headerTop === AT_640.headerTop && s.headerPadTop === AT_640.padTop, `header ${JSON.stringify(got)}`);
    assert(s.ctrlReserve === AT_640.ctrlReserve, `reserve ${JSON.stringify(got)}`);
    assert(row >= AT_640.rowMin && row <= AT_640.rowMax, `header row ${row}px, master ${AT_640.rowMin}–${AT_640.rowMax}px: ${JSON.stringify(got)}`);
    assert(s.box === s.vh - top - reserve - AT_640.gutter, `box ${s.box}px ≠ ${s.vh} − ${top} − ${reserve} − ${AT_640.gutter}: ${JSON.stringify(got)}`);
  });
  await ctx.close();
}

// Outside the band: the top-nav, the header (chart toggle at ≤ 768px, the
// MESH LIVE title above) and the 130px LCD stay.
async function runUnchanged(browser, w, h, touch) {
  const tag = `[${w}x${h}]`;
  const { ctx, page } = await openLive(browser, w, h, touch);
  const s = await state(page);
  if (w === 769) vcrAt769 = s.vcrBtns;
  const head = w <= 768 ? 'chart toggle' : 'MESH LIVE title';
  await step(`${tag} unchanged: top-nav, ${head} and the full VCR LCD are shown`, async () => {
    assert(s.topNav, 'top-nav hidden');
    assert(w <= 768 ? s.chartToggle : s.title, `${head} hidden`);
    assert(s.lcdWidth === 130, `LCD canvas ${s.lcdWidth}px wide, expected 130px`);
  });
  await ctx.close();
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #672 Live capped panel scroll box in short landscape E2E against ${BASE} ===`);
  const runs = [
    ['769', () => runUnchanged(browser, 769, 360, true)],
    ['768 portrait', () => runUnchanged(browser, 768, 1024, true)],
    ['desktop', () => runUnchanged(browser, 1280, 500, false)],
    ...[[653, 320], [700, 360], [740, 360], [700, 480]].map(([w, h]) => [`${w}x${h}`, () => runBand(browser, w, h)]),
    ['640', () => runBoundary640(browser)],
  ];
  for (const [name, run] of runs) {
    try { await run(); }
    catch (e) { failed++; console.error(`  ✗ [${name}] fatal: ` + e.message); }
  }
  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
