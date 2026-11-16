/**
 * E2E (#839 finding 2): the Nodes list must be reachable in phone landscape.
 *
 * At 640x360 (and other phones at most 640 px wide and 500 px tall) the
 * stacked Nodes top bar (search, count pills, Export) and the region chips
 * filled all of #app above the bottom nav. The table's scroll box
 * .panel-left was left 12 px tall, the first row sat under the bottom nav,
 * a swipe moved nothing and no row could be tapped.
 *
 * Flow, from a cold start with touch at each size: Home → "I know what I'm
 * doing" → bottom nav More → Nodes → dismiss every gesture tip → swipe up
 * on the page with one finger until the first row is in view (at most 8
 * swipes) → the row is fully above the bottom nav, uncovered at its centre
 * → tap it → #/nodes/{pubkey} opens the node.
 *
 * 375x560 portrait is a guard: it worked before and must keep working.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-issue-839-nodes-landscape-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:3000';

let passed = 0, failed = 0;
async function test(name, fn) {
  try { await fn(); passed++; console.log('  ✅ ' + name); }
  catch (err) { failed++; console.log('  ❌ ' + name + ': ' + err.message); }
}
function assert(cond, msg) { if (!cond) throw new Error(msg || 'assertion failed'); }

let browser;

async function dismissTips(page) {
  for (let i = 0; i < 6; i++) {
    const btn = await page.$('[data-gesture-hint-dismiss]:visible');
    if (!btn) return;
    await btn.tap().catch(() => {});
    await page.waitForTimeout(250);
  }
}

// Geometry of the first row, the bottom nav and the scroll positions of
// everything that may scroll it.
function rowState(page) {
  return page.evaluate(() => {
    const row = document.querySelector('#nodesTable tbody tr');
    const nav = document.querySelector('.bottom-nav');
    const r = row && row.getBoundingClientRect();
    const navTop = nav && getComputedStyle(nav).display !== 'none' ? nav.getBoundingClientRect().top : innerHeight;
    let hit = false;
    if (r) {
      const el = document.elementFromPoint(r.left + Math.min(40, r.width / 2), (r.top + r.bottom) / 2);
      hit = !!(el && row.contains(el));
    }
    const scrolls = [...document.querySelectorAll('.nodes-page, .nodes-page .split-layout, #nodesLeft')].map((e) => e.scrollTop);
    // Where a viewer swipes: on the list while at least a finger's height
    // of it shows above the bottom nav, else on whatever the page shows.
    const vis = (el) => {
      const b = el && el.getBoundingClientRect();
      return b ? { top: Math.max(b.top, 52), bottom: Math.min(b.bottom, navTop) } : null;
    };
    let zone = vis(document.getElementById('nodesLeft'));
    if (!zone || zone.bottom - zone.top < 48) zone = vis(document.querySelector('.nodes-page'));
    return {
      row: r && { top: r.top, bottom: r.bottom }, navTop, hit, scrolls: scrolls.join(','),
      zone, vw: innerWidth, docW: document.documentElement.scrollWidth,
    };
  });
}

async function swipeUp(page, cdp, s) {
  const x = Math.round(s.vw / 2);
  const y = Math.round(s.zone.bottom - 8);
  // No further than needed to bring the row's bottom above the bottom nav.
  const need = s.row ? Math.round(s.row.bottom - s.navTop + 24) : 9999;
  const dist = Math.max(40, Math.min(need, Math.round((s.zone.bottom - s.zone.top - 16) * 0.8)));
  const before = s.scrolls;
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [{ x, y }] });
  for (let i = 1; i <= 10; i++) {
    await cdp.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [{ x, y: y - (dist * i) / 10 }] });
  }
  // Hold the finger still before lifting it, so the drag ends without a fling.
  await page.waitForTimeout(150);
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [{ x, y: y - dist }] });
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
  await page.waitForTimeout(350);
  if ((await rowState(page)).scrolls !== before) return;
  // Some headless builds do not turn synthetic touch drags into scrolls. A
  // wheel at the same point takes the same user scroll chain, which a box
  // too small to scroll the row into view cannot satisfy either.
  await page.mouse.move(x, y);
  await page.mouse.wheel(0, dist);
  await page.waitForTimeout(350);
}

const fullyInView = (s) => !!s.row && s.row.top >= 52 && s.row.bottom <= s.navTop && s.hit;

async function run(w, h) {
  const ctx = await browser.newContext({ viewport: { width: w, height: h }, hasTouch: true, isMobile: true });
  const page = await ctx.newPage();
  const errors = [];
  page.on('pageerror', (e) => errors.push(String(e)));
  try {
    await page.goto(`${BASE}/`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#chooseExp', { timeout: 15000 });
    await page.tap('#chooseExp');
    await page.tap('[data-bottom-nav-tab="more"]');
    await page.waitForSelector('[data-bottom-nav-more-route="nodes"]', { state: 'visible', timeout: 5000 });
    await page.tap('[data-bottom-nav-more-route="nodes"]');
    await page.waitForSelector('#nodesTable tbody tr[data-key], #nodesTable tbody tr', { timeout: 15000 });
    await page.waitForTimeout(1200); // gesture tips appear ~800 ms after settle
    await dismissTips(page);

    const cdp = await ctx.newCDPSession(page);
    let s = await rowState(page);
    for (let i = 0; i < 8 && !fullyInView(s); i++) {
      await swipeUp(page, cdp, s);
      s = await rowState(page);
    }
    assert(fullyInView(s), `first row never fully above the bottom nav and tappable: row ${JSON.stringify(s.row)}, bottom nav top ${s.navTop}, hit ${s.hit}`);
    assert(s.docW <= s.vw, `horizontal page overflow: ${s.docW} > ${s.vw}`);

    const pk = await page.evaluate(() => {
      const row = document.querySelector('#nodesTable tbody tr');
      return row.getAttribute('data-key') || row.getAttribute('data-pubkey') || '';
    });
    await page.tap('#nodesTable tbody tr:first-child td:first-child');
    await page.waitForFunction(() => /^#\/nodes\/[0-9a-f]+/i.test(location.hash), null, { timeout: 8000 });
    const hash = await page.evaluate(() => location.hash);
    if (pk) assert(hash.indexOf(pk) >= 0, `opened ${hash}, expected the tapped node ${pk}`);
    assert(errors.length === 0, 'page errors: ' + errors[0]);
  } finally {
    await ctx.close();
  }
}

(async () => {
  browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #839 Nodes in phone landscape E2E against ${BASE} ===`);

  await test('640x360 landscape: cold start → More → Nodes → swipe → tap a row opens the node', () => run(640, 360));
  await test('568x320 landscape (smallest phone): the same flow', () => run(568, 320));
  await test('375x560 portrait guard: the same flow still works', () => run(375, 560));

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed\n`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
