// test-issue-667-live-panel-overflow-e2e.js
//
// #667 — on small screens the expanded Live controls panel (#liveHeader with
// #liveControlsBody) grew taller than the viewport. With 16 region groups at
// 320×568 its bottom was at y ≈ 1200. The header is an overlay inside the
// `overflow: hidden` .live-page, so neither the panel nor the page scrolled and
// the last controls (Show all nodes, the Area trigger) could not be reached.
// After #666 the landscape rail made the panel wrap into more rows, so
// 568×320 and 740×360 overflowed as well.
//
// Region groups and areas are answered in the browser, as in the #664/#666
// E2Es. Scrolling uses user input, not scrollIntoView(): a finger drag (CDP
// Input.dispatchTouchEvent), with a mouse wheel at the same point as the
// fallback where headless Chromium does not scroll on synthetic touch.
// .live-page is `overflow: hidden`, which scripts can still scroll but user
// input cannot, so scrollIntoView() would pass on master.
//
// Overflowing panels (320×568, 375×812, 568×320, 740×360 touch, 16 groups):
//   - the panel bottom is inside the viewport and above the VCR bar
//   - a touch swipe on the panel brings the last control (the Area trigger)
//     into view; a tap on its centre opens the menu, and the last menu item
//     can be swiped to and tapped too; the page itself never scrolls
//   - the gear is still hit-testable and a tap on it closes the panel
//   - there is no horizontal page scroll
// Fitting panels keep the pre-#667 layout (no cap, overflow visible):
// 375×812 with 4 groups and 1440×900 desktop with 16 groups.
// Rotation 320×568 → 568×320 while expanded re-fits the cap.
// Review round 2 (#669):
//   - overlays that used to be above the panel stay there while it is capped:
//     the VCR "More" scope menu (320×568, 375×812, 568×320) and node detail
//     (740×360, 800×400), checked by hit-testing where they overlap
//   - an opened Area menu scrolls into the 43–83px box of 653×320 and 740×360
//   - a scrolled, capped panel stays capped after a viewport change
//   - leaving Live disconnects the tracker's observer from the old Live DOM
//
// Usage: BASE_URL=http://localhost:13581 node test-issue-667-live-panel-overflow-e2e.js
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const groups = (n) => Array.from({ length: n }, (_, i) => ({ name: 'Region group ' + (i + 1), codes: ['SJC'] }));
const AREAS = [{ key: 'north', label: 'North' }, { key: 'south', label: 'South' },
  // The last label is long enough that an overlay menu would be wider than the
  // capped body at 320px and be clipped by its scroll box.
  { key: 'west', label: 'West coast, the inland valleys and the islands' }];

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

function state(page) {
  return page.evaluate(() => {
    const rect = (el) => { const r = el.getBoundingClientRect(); return [r.left, r.top, r.right, r.bottom].map(Math.round); };
    const hitOk = (el) => {
      if (!el) return false;
      const r = el.getBoundingClientRect();
      const hit = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2);
      return !!hit && el.contains(hit);
    };
    const gear = document.getElementById('liveControlsToggle');
    const header = document.getElementById('liveHeader');
    const root = document.getElementById('liveControls');
    const body = document.getElementById('liveControlsBody');
    const page = document.querySelector('.live-page');
    const bar = document.getElementById('vcrBar');
    const area = document.querySelector('#liveAreaFilter .region-dropdown-trigger');
    const menu = document.querySelector('#liveAreaFilter .area-dropdown-menu');
    const items = menu ? [...menu.querySelectorAll('[data-area]')] : [];
    const lastItem = items[items.length - 1] || null;
    const bodyW = body.clientWidth;
    const cs = getComputedStyle(body);
    const se = document.scrollingElement || document.documentElement;
    return {
      vw: window.innerWidth, vh: window.innerHeight,
      header: rect(header), body: rect(body), gear: rect(gear),
      gearCentre: (() => { const r = gear.getBoundingClientRect(); return [r.left + r.width / 2, r.top + r.height / 2]; })(),
      gearHit: hitOk(gear), gearSize: [gear.offsetWidth, gear.offsetHeight],
      vcrTop: Math.round(bar.getBoundingClientRect().top),
      area: area && rect(area), areaHit: hitOk(area),
      menuOpen: !!menu && !menu.hidden, menu: menu && !menu.hidden ? rect(menu) : null,
      lastItem: lastItem && rect(lastItem), lastItemHit: hitOk(lastItem),
      // Visible width of the last item's label vs. what it needs, capped at
      // the panel body width.
      lastItemShown: lastItem ? [lastItem.clientWidth, Math.min(lastItem.scrollWidth, bodyW - 4)] : null,
      areaLabel: area && area.textContent.trim(),
      overflowY: cs.overflowY, maxHeight: cs.maxHeight,
      scrollTop: body.scrollTop, scrollRange: body.scrollHeight - body.clientHeight,
      pageScroll: [page.scrollTop, page.scrollLeft, se.scrollTop, se.scrollLeft],
      hScroll: document.documentElement.scrollWidth > document.documentElement.clientWidth,
      expanded: root.classList.contains('is-expanded'),
      collapsed: root.classList.contains('is-collapsed') && body.hasAttribute('hidden'),
      aria: gear.getAttribute('aria-expanded'),
      chips: body.querySelectorAll('.region-quick-pick').length,
    };
  });
}

async function openLive(browser, w, h, touch, nGroups, init) {
  const ctx = await browser.newContext({ viewport: { width: w, height: h }, hasTouch: touch, isMobile: touch });
  if (init) await ctx.addInitScript(init);
  const G = groups(nGroups);
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
  await page.waitForSelector('#liveControlsToggle');
  await page.waitForFunction((n) =>
    document.querySelectorAll('#liveControlsBody .region-quick-pick').length === n &&
    !!document.querySelector('#liveAreaFilter .region-dropdown-trigger') &&
    !!document.querySelector('#vcrBar') && document.querySelector('#vcrBar').offsetHeight > 0 &&
    // Settled layout (see the #666 E2E): live.js has measured the VCR bar
    // and set the page height to innerHeight − bottom-nav.
    document.querySelector('.live-page').style.getPropertyValue('--vcr-bar-height') !== '' &&
    document.querySelector('.live-page').style.height !== '', nGroups);
  return { ctx, page };
}

async function tap(page, touch, x, y) {
  if (touch) await page.touchscreen.tap(x, y);
  else await page.mouse.click(x, y);
  await page.waitForTimeout(250);
}

// One real finger swipe up on the panel (content moves up), starting inside
// the visible part of the controls body.
async function swipeUp(page, cdp, s) {
  const bottom = Math.min(s.body[3], s.vcrTop, s.vh) - 12;
  const top = Math.max(s.body[1], 0) + 12;
  const x = Math.round((s.body[0] + s.body[2]) / 2);
  const y = Math.round(Math.max(top, bottom));
  const dist = Math.max(40, Math.round((bottom - top) * 0.8));
  const before = s.scrollTop;
  // Finger drag: touchStart, ten touchMoves upwards, touchEnd.
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [{ x, y }] });
  for (let i = 1; i <= 10; i++) {
    await cdp.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [{ x, y: y - (dist * i) / 10 }] });
  }
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
  await settle(page);
  if ((await state(page)).scrollTop !== before) { scrollBy.touch++; return; }
  // Some headless builds do not turn synthetic touch drags into scrolls. A
  // wheel at the same point takes the same user scroll chain, which an
  // `overflow: hidden` ancestor cannot join either, so master stays red.
  await page.mouse.move(x, y);
  await page.mouse.wheel(0, dist);
  await settle(page);
  if ((await state(page)).scrollTop !== before) scrollBy.wheel++;
}
const scrollBy = { touch: 0, wheel: 0 };

// A touch swipe ends in a fling that keeps the body moving for about 0.5s
// (measured 450–500ms at 320×568). Positions read before it stops are stale,
// and Chromium turns a tap during a fling into a fling stop without a click,
// so the last Area item was sometimes "tapped" without being selected. Wait
// until scrollTop has not changed for 6 frames (2s at most).
function settle(page) {
  return page.evaluate(() => new Promise((resolve) => {
    const b = document.getElementById('liveControlsBody');
    const t0 = performance.now();
    let last = b.scrollTop, same = 0;
    (function tick() {
      requestAnimationFrame(() => {
        if (b.scrollTop === last) same++; else { same = 0; last = b.scrollTop; }
        if (same >= 6 || performance.now() - t0 > 2000) resolve(); else tick();
      });
    })();
  }));
}

// Swipe until `visible(s)` holds or the panel stops moving.
async function swipeUntil(page, cdp, visible) {
  let s = await state(page);
  for (let i = 0; i < 25 && !visible(s); i++) {
    const before = s.scrollTop;
    await swipeUp(page, cdp, s);
    s = await state(page);
    if (s.scrollTop === before && i > 1) break;
  }
  return s;
}

const inView = (r, s) => !!r && r[1] >= 0 && r[3] <= Math.min(s.vh, s.vcrTop);

async function runOverflow(browser, w, h) {
  const tag = `[${w}x${h}]`;
  const { ctx, page } = await openLive(browser, w, h, true, 16);
  const cdp = await ctx.newCDPSession(page);
  let s = await state(page);
  await tap(page, true, s.gearCentre[0], s.gearCentre[1]);
  s = await state(page);
  await step(`${tag} a tap on the gear opens the panel with 16 region groups`, async () => {
    assert(s.expanded && s.aria === 'true', JSON.stringify(s));
    assert(s.chips === 16, `chips=${s.chips}`);
  });
  await step(`${tag} expanded: panel bottom is inside the viewport and above the VCR bar`, async () => {
    assert(s.header[1] >= 0 && s.header[3] <= s.vh, `panel ${s.header} outside viewport height ${s.vh}`);
    assert(s.header[3] <= s.vcrTop, `panel bottom ${s.header[3]} under the VCR bar (top ${s.vcrTop})`);
  });
  await step(`${tag} expanded: no horizontal page scroll`, async () => {
    assert(!s.hScroll, 'documentElement scrollWidth > clientWidth');
    assert(s.header[0] >= 0 && s.header[2] <= s.vw, `panel ${s.header} outside viewport width ${s.vw}`);
  });

  s = await swipeUntil(page, cdp, (x) => inView(x.area, x) && x.areaHit);
  await step(`${tag} a touch swipe brings the last control (Area trigger) into view`, async () => {
    assert(inView(s.area, s), `Area trigger ${s.area} not in view (vh ${s.vh}, VCR top ${s.vcrTop}); body scrollTop ${s.scrollTop}/${s.scrollRange}`);
    assert(s.areaHit, `Area trigger ${s.area} centre is covered`);
    assert(s.pageScroll.every((v) => v === 0), `page scrolled instead of the panel: ${s.pageScroll}`);
  });
  await tap(page, true, (s.area[0] + s.area[2]) / 2, (s.area[1] + s.area[3]) / 2);
  s = await state(page);
  await step(`${tag} a tap on the Area trigger opens its menu`, async () => {
    assert(s.menuOpen, 'area menu did not open');
  });
  s = await swipeUntil(page, cdp, (x) => inView(x.lastItem, x) && x.lastItemHit);
  await step(`${tag} the last Area menu item can be swiped to and tapped`, async () => {
    assert(inView(s.lastItem, s) && s.lastItemHit, `last item ${s.lastItem} not reachable; menu ${s.menu}`);
    assert(s.menu[0] >= s.body[0] && s.menu[2] <= s.body[2], `menu ${s.menu} clipped by the panel body ${s.body}`);
    assert(s.lastItemShown[0] >= s.lastItemShown[1], `last item label shown ${s.lastItemShown[0]}px of ${s.lastItemShown[1]}px`);
    await tap(page, true, (s.lastItem[0] + s.lastItem[2]) / 2, (s.lastItem[1] + s.lastItem[3]) / 2);
    const after = await state(page);
    assert(/West/.test(after.areaLabel), `Area trigger reads "${after.areaLabel}" after tapping the last item`);
    assert(!after.hScroll, 'documentElement scrollWidth > clientWidth');
  });

  s = await state(page);
  await step(`${tag} the gear stays ≥ 44px, hit-testable, and a tap closes the panel`, async () => {
    assert(s.expanded && s.aria === 'true', `panel was not open before the tap: ${JSON.stringify(s)}`);
    assert(s.gearSize[0] >= 44 && s.gearSize[1] >= 44, `gear ${s.gearSize}`);
    assert(s.gearHit, `gear ${s.gear} centre is covered; panel ${s.header}`);
    await tap(page, true, s.gearCentre[0], s.gearCentre[1]);
    const after = await state(page);
    assert(after.collapsed && !after.expanded && after.aria === 'false', JSON.stringify(after));
    assert(!after.hScroll, 'documentElement scrollWidth > clientWidth');
  });
  await ctx.close();
}

// A panel that fits keeps the pre-#667 layout: no cap, no scroll container,
// so the region/area menus still open as overlays.
async function runFits(browser, w, h, touch, nGroups) {
  const tag = `[${w}x${h}${touch ? '' : ' desktop'}, ${nGroups} groups]`;
  const { ctx, page } = await openLive(browser, w, h, touch, nGroups);
  let s = await state(page);
  await tap(page, touch, s.gearCentre[0], s.gearCentre[1]);
  s = await state(page);
  await step(`${tag} fitting panel is not capped and does not scroll`, async () => {
    assert(s.expanded, JSON.stringify(s));
    assert(s.header[3] <= Math.min(s.vh, s.vcrTop), `panel ${s.header} does not fit; pick a smaller case`);
    assert(s.overflowY === 'visible' && s.maxHeight === 'none', `overflow-y ${s.overflowY}, max-height ${s.maxHeight}`);
  });
  await tap(page, touch, (s.area[0] + s.area[2]) / 2, (s.area[1] + s.area[3]) / 2);
  s = await state(page);
  await step(`${tag} fitting panel: Area menu still opens as an overlay below the trigger`, async () => {
    assert(s.menuOpen && s.menu[1] >= s.area[3] - 1, `menu ${s.menu}, trigger ${s.area}`);
    assert(s.menu[3] > s.body[3], `menu ${s.menu} is laid out inside the body ${s.body}, not over the map`);
  });
  await ctx.close();
}

// Rotating while expanded must re-fit the cap to the new viewport.
async function runRotate(browser) {
  const tag = '[320x568 → 568x320]';
  const { ctx, page } = await openLive(browser, 320, 568, true, 16);
  let s = await state(page);
  await tap(page, true, s.gearCentre[0], s.gearCentre[1]);
  await page.setViewportSize({ width: 568, height: 320 });
  await page.waitForTimeout(600);
  s = await state(page);
  await step(`${tag} after rotating, the expanded panel still ends inside the viewport`, async () => {
    assert(s.expanded, JSON.stringify(s));
    assert(s.header[3] <= Math.min(s.vh, s.vcrTop), `panel ${s.header}, vh ${s.vh}, VCR top ${s.vcrTop}`);
    assert(!s.hScroll, 'documentElement scrollWidth > clientWidth');
  });
  await ctx.close();
}

// Where `sel`'s parts overlap the panel, the topmost element at the centre of
// each overlap must belong to `sel` (#669 review F1/F2: the capped panel must
// not cover overlays that used to be on top of it).
function overlap(page, sel, partsSel) {
  return page.evaluate(([sel, partsSel]) => {
    const el = document.querySelector(sel);
    const h = document.getElementById('liveHeader').getBoundingClientRect();
    const parts = el ? [...el.querySelectorAll(partsSel)] : [];
    if (el && el.matches(partsSel)) parts.unshift(el);
    const points = [];
    for (const p of parts) {
      const r = p.getBoundingClientRect();
      const l = Math.max(r.left, h.left), t = Math.max(r.top, h.top);
      const rr = Math.min(r.right, h.right), b = Math.min(r.bottom, h.bottom);
      if (rr - l < 2 || b - t < 2) continue;
      const x = (l + rr) / 2, y = (t + b) / 2;
      const hit = document.elementFromPoint(x, y);
      points.push({ part: p.id || p.textContent.trim(), x: Math.round(x), y: Math.round(y),
        ok: !!hit && el.contains(hit), hit: hit ? (hit.id || hit.className || hit.tagName) : null });
    }
    return {
      capped: document.getElementById('liveControls').classList.contains('is-capped'),
      shown: !!el && el.getClientRects().length > 0 && !el.hidden && !el.classList.contains('hidden'),
      header: [h.left, h.top, h.right, h.bottom].map(Math.round), points,
    };
  }, [sel, partsSel]);
}

const centre = (page, sel) => page.$eval(sel, (e) => { const r = e.getBoundingClientRect(); return [r.left + r.width / 2, r.top + r.height / 2]; });

// F1: the mobile VCR "More" scope menu opens upwards out of the VCR bar, into
// the area of a capped panel.
async function runVcrMenu(browser, w, h) {
  const tag = `[${w}x${h}]`;
  const { ctx, page } = await openLive(browser, w, h, true, 16);
  let s = await state(page);
  await tap(page, true, s.gearCentre[0], s.gearCentre[1]);
  const more = await centre(page, '[data-vcr-scope-more]');
  await tap(page, true, more[0], more[1]);
  const o = await overlap(page, '#vcrScopeMoreMenu', '.vcr-scope-more-item');
  await step(`${tag} capped panel + open VCR scope menu: the menu is on top where they overlap`, async () => {
    assert(o.capped, 'panel is not capped');
    assert(o.shown, 'VCR scope menu did not open');
    assert(o.points.length, `no VCR menu item overlaps the panel ${o.header}`);
    const bad = o.points.filter((p) => !p.ok);
    assert(!bad.length, 'covered by the panel: ' + JSON.stringify(bad));
  });
  await step(`${tag} a tap on a VCR scope item where it overlaps the panel selects it`, async () => {
    const p = o.points[o.points.length - 1];
    assert(p, 'no overlap point');
    await tap(page, true, p.x, p.y);
    const r = await page.evaluate(() => ({
      label: document.querySelector('[data-vcr-scope-more]').textContent.trim(),
      hidden: document.getElementById('vcrScopeMoreMenu').hidden,
      expanded: document.getElementById('liveControls').classList.contains('is-expanded'),
    }));
    assert(r.label === p.part + ' ▾' && r.hidden, `tapped "${p.part}", More reads "${r.label}", menu hidden ${r.hidden}`);
    assert(r.expanded, 'the tap reached the panel instead');
  });
  await ctx.close();
}

// F2: from 641px up node detail is a 1000 overlay later in the DOM than the
// header and used to win where they overlap.
async function runNodeDetail(browser, w, h) {
  const tag = `[${w}x${h}]`;
  const { ctx, page } = await openLive(browser, w, h, true, 16);
  let s = await state(page);
  await tap(page, true, s.gearCentre[0], s.gearCentre[1]);
  await page.waitForFunction(() => window._liveNodeMarkers && Object.keys(window._liveNodeMarkers()).length > 0);
  // A marker click is how a user opens node detail; the marker itself may be
  // under the panel, so fire Leaflet's click on it.
  await page.evaluate(() => { const m = window._liveNodeMarkers(); m[Object.keys(m)[0]].fire('click'); });
  await page.waitForTimeout(400);
  const o = await overlap(page, '#liveNodeDetail', '#liveNodeDetail, #nodeDetailClose');
  await step(`${tag} capped panel + open node detail: node detail is on top where they overlap`, async () => {
    assert(o.capped, 'panel is not capped');
    assert(o.shown, 'node detail did not open');
    assert(o.points.length, `node detail does not overlap the panel ${o.header}`);
    const bad = o.points.filter((p) => !p.ok);
    assert(!bad.length, 'covered by the panel: ' + JSON.stringify(bad));
  });
  await step(`${tag} a tap on the node detail close button closes it`, async () => {
    const c = await centre(page, '#nodeDetailClose');
    await tap(page, true, c[0], c[1]);
    const closed = await page.evaluate(() => document.getElementById('liveNodeDetail').classList.contains('hidden'));
    assert(closed, 'node detail is still open');
  });
  await ctx.close();
}

// F3: in the 641–768px landscape band the scroll box is only 43–83px tall, and
// an opened menu used to render below it, out of sight.
async function runMenuInBox(browser, w, h) {
  const tag = `[${w}x${h}]`;
  const { ctx, page } = await openLive(browser, w, h, true, 16);
  const cdp = await ctx.newCDPSession(page);
  let s = await state(page);
  await tap(page, true, s.gearCentre[0], s.gearCentre[1]);
  s = await swipeUntil(page, cdp, (x) => inView(x.area, x) && x.areaHit);
  await tap(page, true, (s.area[0] + s.area[2]) / 2, (s.area[1] + s.area[3]) / 2);
  await settle(page);
  const r = await page.evaluate(() => {
    const body = document.getElementById('liveControlsBody');
    const b = body.getBoundingClientRect();
    const boxBottom = b.top + body.clientTop + body.clientHeight;
    const first = document.querySelector('#liveAreaFilter .area-dropdown-menu [data-area]');
    const f = first.getBoundingClientRect();
    const hit = document.elementFromPoint(f.left + f.width / 2, f.top + f.height / 2);
    return { capped: document.getElementById('liveControls').classList.contains('is-capped'),
      box: [Math.round(b.top), Math.round(boxBottom)], first: [Math.round(f.top), Math.round(f.bottom)],
      inBox: f.top >= b.top - 1 && f.bottom <= boxBottom + 1, hit: !!hit && first.contains(hit),
      page: document.querySelector('.live-page').scrollTop };
  });
  await step(`${tag} a tapped Area trigger brings its menu into the scroll box without another swipe`, async () => {
    assert(r.capped, 'panel is not capped');
    assert(r.inBox && r.hit, `first menu item y ${r.first} outside the scroll box y ${r.box}`);
    assert(r.page === 0, `page scrolled: ${r.page}`);
  });
  await ctx.close();
}

// F4: after the body is scrolled, a viewport change re-measures the content.
// That must count what is scrolled out of view, or the cap drops and the
// panel springs back over the VCR bar.
async function runScrolledResize(browser) {
  const tag = '[320x568 scrolled → 320x600]';
  const { ctx, page } = await openLive(browser, 320, 568, true, 16);
  const cdp = await ctx.newCDPSession(page);
  let s = await state(page);
  await tap(page, true, s.gearCentre[0], s.gearCentre[1]);
  s = await swipeUntil(page, cdp, (x) => inView(x.area, x) && x.areaHit);
  const scrolled = s.scrollTop;
  await page.setViewportSize({ width: 320, height: 600 });
  await page.waitForTimeout(600);
  s = await state(page);
  await step(`${tag} a viewport change on a scrolled panel keeps it capped and inside the viewport`, async () => {
    assert(scrolled > 0, `body was not scrolled (${scrolled})`);
    assert(s.overflowY === 'auto', `cap dropped: overflow-y ${s.overflowY}, max-height ${s.maxHeight}`);
    assert(s.header[3] <= Math.min(s.vh, s.vcrTop), `panel ${s.header}, vh ${s.vh}, VCR top ${s.vcrTop}`);
  });
  await ctx.close();
}

// F5: leaving Live must tear the tracker down: no observer keeps the detached
// Live DOM, and nothing re-fits it afterwards.
function trackObservers() {
  const RO = window.ResizeObserver;
  if (!RO) return;
  const live = new Set();
  window.ResizeObserver = class extends RO {
    constructor(cb) { super(cb); this.targets = new Set(); }
    observe(t, o) { this.targets.add(t); live.add(this); return super.observe(t, o); }
    unobserve(t) { this.targets.delete(t); return super.unobserve(t); }
    disconnect() { this.targets.clear(); live.delete(this); return super.disconnect(); }
  };
  window.__observing = (root) => [...live].reduce((n, ro) => n + [...ro.targets].filter((t) => root.contains(t)).length, 0);
  window.__fits = 0;
  const set = CSSStyleDeclaration.prototype.setProperty;
  CSSStyleDeclaration.prototype.setProperty = function (name) {
    if (name === '--live-ctrl-top') window.__fits++;
    return set.apply(this, arguments);
  };
}

async function runTeardown(browser) {
  const tag = '[320x568 Live → Packets]';
  const { ctx, page } = await openLive(browser, 320, 568, true, 16, trackObservers);
  let s = await state(page);
  await tap(page, true, s.gearCentre[0], s.gearCentre[1]);
  const on = await page.evaluate(() => {
    window.__livePage = document.querySelector('.live-page');
    window.__liveControls = document.getElementById('liveControls');
    return { observed: window.__observing(window.__livePage), capped: window.__liveControls.classList.contains('is-capped') };
  });
  await page.evaluate(() => { location.hash = '#/packets'; });
  await page.waitForFunction(() => !document.querySelector('.live-page'));
  await page.waitForTimeout(300);
  const fits = await page.evaluate(() => window.__fits);
  await page.setViewportSize({ width: 568, height: 320 });
  await page.waitForTimeout(400);
  await page.setViewportSize({ width: 320, height: 568 });
  await page.waitForTimeout(400);
  const off = await page.evaluate(() => ({
    observed: window.__observing(window.__livePage), fits: window.__fits,
    capped: window.__liveControls.classList.contains('is-capped'), connected: window.__livePage.isConnected,
  }));
  await step(`${tag} after leaving Live no observer holds the old Live DOM and nothing re-fits it`, async () => {
    assert(on.capped && on.observed > 0, `tracker not active on Live: ${JSON.stringify(on)}`);
    assert(!off.connected, 'Live DOM still attached');
    assert(off.observed === 0, `${off.observed} detached Live element(s) still observed`);
    assert(off.fits === fits, `fit ran ${off.fits - fits}× after leaving Live`);
    assert(!off.capped, '.is-capped left on the detached panel');
  });
  await ctx.close();
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #667 live controls panel fits the viewport and scrolls E2E against ${BASE} ===`);
  for (const [w, h] of [[320, 568], [375, 812], [568, 320], [740, 360]]) {
    try { await runOverflow(browser, w, h); }
    catch (e) { failed++; console.error(`  ✗ [${w}x${h}] fatal: ` + e.message); }
  }
  for (const [w, h, touch, n] of [[375, 812, true, 4], [1440, 900, false, 16]]) {
    try { await runFits(browser, w, h, touch, n); }
    catch (e) { failed++; console.error(`  ✗ [${w}x${h}] fatal: ` + e.message); }
  }
  try { await runRotate(browser); }
  catch (e) { failed++; console.error('  ✗ [rotate] fatal: ' + e.message); }
  const runs = [
    ...[[320, 568], [375, 812], [568, 320]].map(([w, h]) => ['vcr menu', () => runVcrMenu(browser, w, h)]),
    ...[[740, 360], [800, 400]].map(([w, h]) => ['node detail', () => runNodeDetail(browser, w, h)]),
    ...[[653, 320], [740, 360]].map(([w, h]) => ['menu in box', () => runMenuInBox(browser, w, h)]),
    ['scrolled resize', () => runScrolledResize(browser)],
    ['teardown', () => runTeardown(browser)],
  ];
  for (const [name, run] of runs) {
    try { await run(); }
    catch (e) { failed++; console.error(`  ✗ [${name}] fatal: ` + e.message); }
  }
  await browser.close();
  console.log(`  panel scrolled by touch drag ${scrollBy.touch}×, by wheel fallback ${scrollBy.wheel}×`);
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
