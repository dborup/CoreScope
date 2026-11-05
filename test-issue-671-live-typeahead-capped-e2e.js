// test-issue-671-live-typeahead-capped-e2e.js
//
// #671 — the node-filter typeahead (#liveNodeFilterDropdown) in the expanded
// Live controls panel:
//   - In a capped panel (#667) the list opens below the scroll box (740×360)
//     or runs past its bottom (320×568). #667's showOpenedMenu() scrolled only
//     region/area menus into the box, on a trigger click; the typeahead opens
//     after an `input` event and a fetch.
//   - At ≤ 768px the list is clipped to its own .live-toggles row, which is a
//     scroll container there (overflow-y: auto, and overflow-x: auto at
//     ≤ 640px). Hit-testing below that row finds the region quick-pick row
//     that follows it, capped or not.
//
// Region groups are answered in the browser, as in the #667 E2E. Taps are
// real touch taps at the option's centre, so a clipped or out-of-box option
// is not tapped.
//
// Steps:
//   - capped 320×568 (16 groups): the open list is inside the scroll box and
//     every visible option is hit-testable; a tap on the last one applies it
//   - capped 740×360 (16 groups): the first option is inside the box and
//     hit-testable; a tap on it applies it
//   - uncapped 375×812 (4 groups): every visible option is hit-testable (not
//     covered by the quick-picks); the panel is not scrolled; tap applies
//   - guards (green before and after): closing the list restores the row's
//     scroll overflow at 320×568, and a fitting desktop panel (1440×900) is
//     not capped or scrolled while the list is open
//
// Usage: BASE_URL=http://localhost:13581 node test-issue-671-live-typeahead-capped-e2e.js
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const QUERY = 'a';
const groups = (n) => Array.from({ length: n }, (_, i) => ({ name: 'Region group ' + (i + 1), codes: ['SJC'] }));

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

function state(page) {
  return page.evaluate(() => {
    const rect = (el) => { const r = el.getBoundingClientRect(); return [r.left, r.top, r.right, r.bottom].map(Math.round); };
    const body = document.getElementById('liveControlsBody');
    const dd = document.getElementById('liveNodeFilterDropdown');
    const row = dd.closest('.live-toggles');
    const open = !dd.classList.contains('hidden');
    const ddTop = dd.getBoundingClientRect().top + dd.clientTop;
    // Options the list itself shows (it scrolls past 240px).
    const shown = open ? [...dd.querySelectorAll('.live-node-filter-option')].filter((o) => {
      const r = o.getBoundingClientRect();
      return r.top >= ddTop - 1 && r.bottom <= ddTop + dd.clientHeight + 1;
    }) : [];
    const opt = (o) => {
      const r = o.getBoundingClientRect();
      const x = r.left + r.width / 2, y = r.top + r.height / 2;
      const hit = document.elementFromPoint(x, y);
      return { rect: rect(o), centre: [x, y], hitOk: !!hit && o.contains(hit),
        hit: hit ? (hit.id || String(hit.className)) : null,
        key: o.getAttribute('data-key'), name: o.getAttribute('data-name') };
    };
    const bodyTop = body.getBoundingClientRect().top + body.clientTop;
    return {
      capped: document.getElementById('liveControls').classList.contains('is-capped'),
      box: [Math.round(bodyTop), Math.round(bodyTop + body.clientHeight)],
      scrollTop: body.scrollTop,
      open, list: open ? rect(dd) : null, options: shown.map(opt),
      rowOverflow: getComputedStyle(row).overflowY,
      filter: localStorage.getItem('live-node-filter'),
      nodeParam: new URLSearchParams((location.hash.split('?')[1]) || '').get('node'),
    };
  });
}

async function openLive(browser, w, h, touch, nGroups) {
  const ctx = await browser.newContext({ viewport: { width: w, height: h }, hasTouch: touch, isMobile: touch });
  const G = groups(nGroups);
  await ctx.route((u) => u.pathname === '/api/config/regions/groups', (r) =>
    r.fulfill({ contentType: 'application/json', body: JSON.stringify(G) }));
  await ctx.addInitScript(() => {
    try {
      localStorage.removeItem('live-controls-expanded'); localStorage.removeItem('live-fullscreen');
      localStorage.removeItem('live-node-filter');
    } catch (_) {}
  });
  const page = await ctx.newPage();
  page.setDefaultTimeout(15000);
  page.on('pageerror', (e) => console.error('    [pageerror]', e.message));
  await page.goto(BASE + '/#/live', { waitUntil: 'domcontentloaded' });
  await page.waitForFunction((n) =>
    document.querySelectorAll('#liveControlsBody .region-quick-pick').length === n &&
    !document.getElementById('liveNodeFilterInput').disabled &&
    !!document.querySelector('#vcrBar') && document.querySelector('#vcrBar').offsetHeight > 0 &&
    document.querySelector('.live-page').style.getPropertyValue('--vcr-bar-height') !== '' &&
    document.querySelector('.live-page').style.height !== '', nGroups);
  const gear = await page.$('#liveControlsToggle');
  const g = await gear.boundingBox();
  if (touch) await page.touchscreen.tap(g.x + g.width / 2, g.y + g.height / 2);
  else await page.mouse.click(g.x + g.width / 2, g.y + g.height / 2);
  await page.waitForFunction(() => document.getElementById('liveControls').classList.contains('is-expanded'));
  await frames(page, 4);
  return { ctx, page };
}

function frames(page, n) {
  return page.evaluate((n) => new Promise((resolve) => {
    (function tick(i) { if (!i) resolve(); else requestAnimationFrame(() => tick(i - 1)); })(n);
  }), n);
}

// Type into the node filter the way a user does and wait for the list.
async function typeQuery(page, touch) {
  const r = await (await page.$('#liveNodeFilterInput')).boundingBox();
  if (touch) await page.touchscreen.tap(r.x + r.width / 2, r.y + r.height / 2);
  else await page.mouse.click(r.x + r.width / 2, r.y + r.height / 2);
  await page.keyboard.type(QUERY);
  await page.waitForFunction(() => {
    const dd = document.getElementById('liveNodeFilterDropdown');
    return !dd.classList.contains('hidden') && dd.querySelectorAll('.live-node-filter-option').length >= 2;
  });
  await frames(page, 4);
  return state(page);
}

async function tapApplies(page, touch, o) {
  if (touch) await page.touchscreen.tap(o.centre[0], o.centre[1]);
  else await page.mouse.click(o.centre[0], o.centre[1]);
  await page.waitForTimeout(250);
  const s = await state(page);
  assert(!s.open, 'list still open after the tap');
  assert(s.filter === o.key, `filter is "${s.filter}", expected ${o.name} (${o.key})`);
  assert(s.nodeParam === o.key, `#node=${s.nodeParam}, expected ${o.key}`);
}

const inBox = (r, s) => r[1] >= s.box[0] - 1 && r[3] <= s.box[1] + 1;
const misses = (s) => s.options.filter((o) => !o.hitOk).map((o) => `${o.rect[1]}–${o.rect[3]}→${o.hit}`).join(', ');

async function runCapped320(browser) {
  const tag = '[320x568 capped]';
  const { ctx, page } = await openLive(browser, 320, 568, true, 16);
  const s = await typeQuery(page, true);
  await step(`${tag} open list is inside the scroll box`, async () => {
    assert(s.capped, 'panel not capped');
    assert(inBox(s.list, s), `list y ${s.list[1]}–${s.list[3]} not inside box ${s.box[0]}–${s.box[1]}`);
  });
  await step(`${tag} every visible option is hit-testable`, async () => {
    assert(s.options.length >= 2, `${s.options.length} options shown`);
    assert(!misses(s), `covered: ${misses(s)}`);
  });
  await step(`${tag} a tap on the last option applies the filter`, () =>
    tapApplies(page, true, s.options[s.options.length - 1]));
  await step(`${tag} guard: closing the list restores the row's scroll overflow`, async () => {
    const t = await state(page);
    assert(t.rowOverflow === 'auto', `row overflow-y is ${t.rowOverflow}`);
  });
  await ctx.close();
}

async function runCapped740(browser) {
  const tag = '[740x360 capped]';
  const { ctx, page } = await openLive(browser, 740, 360, true, 16);
  const s = await typeQuery(page, true);
  const first = s.options[0];
  await step(`${tag} first option is inside the scroll box and hit-testable`, async () => {
    assert(s.capped, 'panel not capped');
    assert(first && inBox(first.rect, s), `first option y ${first && first.rect[1]}–${first && first.rect[3]} not inside box ${s.box[0]}–${s.box[1]}`);
    assert(first.hitOk, `first option covered by ${first.hit}`);
  });
  await step(`${tag} a tap on the first option applies the filter`, () => tapApplies(page, true, first));
  await ctx.close();
}

async function runUncapped375(browser) {
  const tag = '[375x812 uncapped]';
  const { ctx, page } = await openLive(browser, 375, 812, true, 4);
  const s = await typeQuery(page, true);
  await step(`${tag} options are not covered by the quick-pick row`, async () => {
    assert(!s.capped, 'panel capped');
    assert(s.options.length >= 2, `${s.options.length} options shown`);
    assert(!misses(s), `covered: ${misses(s)}`);
  });
  await step(`${tag} guard: the fitting panel is not scrolled`, async () => {
    assert(s.scrollTop === 0, `body scrollTop ${s.scrollTop}`);
  });
  await step(`${tag} a tap on the last option applies the filter`, () =>
    tapApplies(page, true, s.options[s.options.length - 1]));
  await ctx.close();
}

async function runDesktop(browser) {
  const tag = '[1440x900 desktop]';
  const { ctx, page } = await openLive(browser, 1440, 900, false, 16);
  const s = await typeQuery(page, false);
  await step(`${tag} guard: panel not capped or scrolled, options hit-testable`, async () => {
    assert(!s.capped, 'panel capped');
    assert(s.scrollTop === 0, `body scrollTop ${s.scrollTop}`);
    assert(!misses(s), `covered: ${misses(s)}`);
  });
  await ctx.close();
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #671 live node-filter typeahead in the controls panel E2E against ${BASE} ===`);
  for (const [name, run] of [['320x568', runCapped320], ['740x360', runCapped740],
    ['375x812', runUncapped375], ['1440x900', runDesktop]]) {
    try { await run(browser); }
    catch (e) { failed++; console.error(`  ✗ [${name}] fatal: ` + e.message); }
  }
  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
