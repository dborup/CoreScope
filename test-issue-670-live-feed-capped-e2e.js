// test-issue-670-live-feed-capped-e2e.js
//
// #670 — while the Live controls panel is capped (#667), the feed card drops
// to z-index 999 so the panel wins where the two overlap. The rule has no
// media query, so it applies from 641px (the feed is not rendered at
// ≤ 640px) up to any width where the panel caps. The feed at its default
// bottom-left corner lies entirely inside the capped panel's box at every
// size measured, so the whole feed card is under the panel until the gear
// closes it. This suite pins that behaviour across 641–1024px, including the
// 769–1024px half that had no test.
//
// Region groups are answered in the browser, as in the #667/#672 E2Es. 16
// groups cap the panel up to 800×400; 1024×500 needs 40. Taps are real
// touch taps (mouse clicks on the 1024×500 desktop window) at the centre.
//
// Steps per capped size (641×360, 653×320, 740×360, 768×360, 800×400 touch,
// 1024×500 mouse):
//   - the panel is capped and the feed (shown, bottom-left) overlaps it
//   - #feedHideBtn, the feed's corner button and #liveFeed .panel-content are
//     not hit-testable at their centres: the topmost element is in
//     #liveHeader
//   - a panel control inside the overlap is hit-testable, and a tap on it
//     works (a region quick-pick turns pressed, an input takes focus)
//   - a tap on the gear closes the panel; the three feed targets are
//     hit-testable again and a tap on #feedHideBtn hides the feed
// Guard: a panel that fits (1024×500, 16 groups) is not capped, and the feed
// keeps z-index 1000 with its three targets hit-testable.
//
// Usage: BASE_URL=http://localhost:13581 node test-issue-670-live-feed-capped-e2e.js
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const groups = (n) => Array.from({ length: n }, (_, i) => ({ name: 'Region group ' + (i + 1), codes: ['SJC'] }));
const AREAS = [{ key: 'north', label: 'North' }, { key: 'south', label: 'South' }];
const FEED_TARGETS = { hide: '#feedHideBtn', corner: '#liveFeed .panel-corner-btn', content: '#liveFeed .panel-content' };

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

function state(page) {
  return page.evaluate((targets) => {
    const rect = (el) => { const r = el.getBoundingClientRect(); return [r.left, r.top, r.right, r.bottom].map(Math.round); };
    const centre = (el) => { const r = el.getBoundingClientRect(); return [r.left + r.width / 2, r.top + r.height / 2]; };
    const label = (el) => !el ? 'none' : (el.id ? '#' + el.id : el.tagName.toLowerCase() + (el.classList[0] ? '.' + el.classList[0] : ''));
    const header = document.getElementById('liveHeader');
    const feed = document.getElementById('liveFeed');
    const gear = document.getElementById('liveControlsToggle');
    // For each feed target: who is topmost at its centre, and whether that is
    // the target itself (`ok`) or part of the panel (`panel`).
    const hits = {};
    for (const [k, sel] of Object.entries(targets)) {
      const el = document.querySelector(sel);
      if (!el) { hits[k] = { missing: true }; continue; }
      const [x, y] = centre(el);
      const top = document.elementFromPoint(x, y);
      hits[k] = { ok: !!top && el.contains(top), panel: !!top && header.contains(top), top: label(top), at: [x, y].map(Math.round) };
    }
    const fr = feed.getBoundingClientRect();
    // A visible panel control whose centre is inside the feed card and the
    // panel's scroll box: a region quick-pick where there is one, else an
    // input (at 653×320 the node filter sits there).
    const box = document.getElementById('liveControlsBody').getBoundingClientRect();
    const inside = (el) => {
      const [x, y] = centre(el);
      return el.getClientRects().length > 0 && x > fr.left && x < fr.right && y > fr.top && y < fr.bottom &&
        y > box.top && y < box.bottom;
    };
    const cands = [...document.querySelectorAll('#liveControlsBody .region-quick-pick, #liveControlsBody input[id]')].filter(inside);
    const pick = cands.find((el) => el.matches('.region-quick-pick')) || cands[0];
    const under = pick ? {
      sel: pick.matches('.region-quick-pick')
        ? `#liveControlsBody .region-quick-pick[data-region-group="${pick.getAttribute('data-region-group')}"]`
        : '#' + pick.id,
      centre: centre(pick),
      hit: (() => { const [x, y] = centre(pick); const t = document.elementFromPoint(x, y); return !!t && pick.contains(t); })(),
    } : null;
    const root = document.getElementById('liveControls');
    return {
      capped: root.classList.contains('is-capped'),
      expanded: root.classList.contains('is-expanded'),
      header: rect(header), feed: rect(feed),
      feedShown: feed.getClientRects().length > 0 && getComputedStyle(feed).visibility !== 'hidden' && !feed.classList.contains('hidden'),
      feedPos: feed.getAttribute('data-position'),
      feedZ: getComputedStyle(feed).zIndex,
      hits,
      under,
      gearCentre: centre(gear), gearHit: (() => { const [x, y] = centre(gear); const t = document.elementFromPoint(x, y); return !!t && gear.contains(t); })(),
    };
  }, FEED_TARGETS);
}

function overlaps(a, b) {
  return Math.min(a[2], b[2]) > Math.max(a[0], b[0]) && Math.min(a[3], b[3]) > Math.max(a[1], b[1]);
}

function frames(page, n) {
  return page.evaluate((n) => new Promise((resolve) => {
    (function tick(i) { if (!i) resolve(); else requestAnimationFrame(() => tick(i - 1)); })(n);
  }), n);
}

async function openLive(browser, w, h, touch, nGroups) {
  const ctx = await browser.newContext({ viewport: { width: w, height: h }, hasTouch: touch, isMobile: touch });
  const G = groups(nGroups);
  await ctx.route((u) => u.pathname === '/api/config/regions/groups', (r) =>
    r.fulfill({ contentType: 'application/json', body: JSON.stringify(G) }));
  await ctx.route((u) => u.pathname === '/api/config/areas', (r) =>
    r.fulfill({ contentType: 'application/json', body: JSON.stringify(AREAS) }));
  // Default panel corners and a shown feed (see PANEL_DEFAULTS in live.js).
  await ctx.addInitScript(() => {
    try {
      ['live-controls-expanded', 'live-fullscreen', 'live-feed-hidden', 'panel-corner-liveFeed',
        'panel-corner-liveLegend', 'panel-corner-liveNodeDetail'].forEach((k) => localStorage.removeItem(k));
    } catch (_) {}
  });
  const page = await ctx.newPage();
  page.setDefaultTimeout(15000);
  page.on('pageerror', (e) => console.error('    [pageerror]', e.message));
  await page.goto(BASE + '/#/live', { waitUntil: 'domcontentloaded' });
  await page.waitForFunction((n) =>
    document.querySelectorAll('#liveControlsBody .region-quick-pick').length === n &&
    !!document.querySelector('#feedHideBtn') &&
    !!document.querySelector('#vcrBar') && document.querySelector('#vcrBar').offsetHeight > 0 &&
    document.querySelector('.live-page').style.getPropertyValue('--vcr-bar-height') !== '' &&
    document.querySelector('.live-page').style.height !== '', nGroups);
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
  await page.waitForTimeout(350); // feed/header transitions
  await frames(page, 4);
  return state(page);
}

const fmt = (hits) => Object.entries(hits).map(([k, v]) => `${k}:${v.missing ? 'missing' : v.ok ? 'feed' : v.top}`).join(', ');

async function runCapped(browser, w, h, touch, nGroups) {
  const tag = `[${w}x${h}]`;
  const { ctx, page } = await openLive(browser, w, h, touch, nGroups);
  let s = await openPanel(page, touch);
  await step(`${tag} panel is capped and the bottom-left feed overlaps it`, async () => {
    assert(s.expanded && s.capped, `expanded ${s.expanded}, capped ${s.capped}`);
    assert(s.feedShown && s.feedPos === 'bl', `feed shown ${s.feedShown}, position ${s.feedPos}`);
    assert(overlaps(s.header, s.feed), `feed ${s.feed} does not overlap the panel ${s.header}`);
  });
  await step(`${tag} capped: the panel wins — #feedHideBtn, the corner button and the feed content are under it`, async () => {
    const bad = Object.entries(s.hits).filter(([, v]) => v.missing || v.ok || !v.panel);
    assert(!bad.length, `topmost: ${fmt(s.hits)} (feed z-index ${s.feedZ})`);
  });
  await step(`${tag} capped: a panel control inside the overlap is hit-testable and a tap on it works`, async () => {
    assert(s.under, `no panel control centre inside the feed ${s.feed}`);
    assert(s.under.hit, `${s.under.sel} at ${s.under.centre.map(Math.round)} is covered (feed z-index ${s.feedZ})`);
    await tap(page, touch, s.under.centre);
    // A quick-pick turns pressed; an input takes focus.
    const r = await page.evaluate((sel) => {
      const el = document.querySelector(sel);
      return el.matches('.region-quick-pick') ? el.getAttribute('aria-pressed') === 'true' : document.activeElement === el;
    }, s.under.sel);
    assert(r, `the tap on ${s.under.sel} did not reach it`);
  });
  s = await state(page);
  await step(`${tag} a tap on the gear closes the panel and the feed is usable again`, async () => {
    assert(s.gearHit, 'gear not hit-testable');
    await tap(page, touch, s.gearCentre);
    await page.waitForFunction(() => !document.getElementById('liveControls').classList.contains('is-expanded'));
    await page.waitForTimeout(350);
    const after = await state(page);
    assert(!after.capped, 'panel still capped after closing');
    const bad = Object.entries(after.hits).filter(([, v]) => !v.ok);
    assert(!bad.length, `after closing, topmost: ${fmt(after.hits)}`);
    await tap(page, touch, after.hits.hide.at);
    const hidden = await page.evaluate(() => document.getElementById('liveFeed').classList.contains('hidden'));
    assert(hidden, 'a tap on #feedHideBtn did not hide the feed');
  });
  await ctx.close();
}

// The rule only applies while capped: a panel that fits keeps the pre-#667
// stacking, with the feed at z-index 1000 on top.
async function runFits(browser) {
  const tag = '[1024x500, 16 groups]';
  const { ctx, page } = await openLive(browser, 1024, 500, false, 16);
  const s = await openPanel(page, false);
  await step(`${tag} panel fits: not capped, the feed keeps z-index 1000 and its targets`, async () => {
    assert(s.expanded && !s.capped, `expanded ${s.expanded}, capped ${s.capped}`);
    assert(s.feedZ === '1000', `feed z-index ${s.feedZ}`);
    const bad = Object.entries(s.hits).filter(([, v]) => !v.ok);
    assert(!bad.length, `topmost: ${fmt(s.hits)}`);
  });
  await ctx.close();
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #670 Live feed under a capped controls panel E2E against ${BASE} ===`);
  const runs = [
    ...[[641, 360], [653, 320], [740, 360], [768, 360], [800, 400]].map(([w, h]) =>
      [`${w}x${h}`, () => runCapped(browser, w, h, true, 16)]),
    ['1024x500', () => runCapped(browser, 1024, 500, false, 40)],
    ['fits', () => runFits(browser)],
  ];
  for (const [name, run] of runs) {
    try { await run(); }
    catch (e) { failed++; console.error(`  ✗ [${name}] fatal: ` + e.message); }
  }
  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
