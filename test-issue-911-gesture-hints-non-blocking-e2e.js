/**
 * E2E (#911): a first-visit gesture hint never swallows a tap meant for a
 * page control.
 *
 * Cold start on a phone (nothing pre-seeded in localStorage, real touch
 * taps), at 375x560 and 360x640:
 *
 *   1. Home: with the tab-swipe and edge-swipe hints up, no point of a hint
 *      outside its "Got it" button takes a tap, no control's middle is
 *      covered, and the first tap on "I know what I'm doing" (on a point the
 *      tab-swipe pill covers) reaches the button and dismisses the hints;
 *   2. Nodes: the first tap on the search field, on a point the edge-swipe
 *      pill covers, focuses it;
 *   3. the real flow Home > More > Nodes: the first tap on a node row, on a
 *      point the row-swipe pill covers, opens the node; no hint follows onto
 *      the node page; Back to nodes takes the first tap there;
 *   4. the node page from a cold deep link shows no hint, and real taps on
 *      the claim controls work: height field, Get code, Start over;
 *   5. "Got it" works on the first tap while the hint fades in;
 *   6. the probes themselves: a blocking hint and a covered control turn
 *      them red, so the suite cannot pass vacuously.
 *
 * Every check taps, or asks what a tap at a real coordinate would hit; none
 * asserts where a hint sits.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-issue-911-gesture-hints-non-blocking-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const SHOW_WAIT_MS = 1600; // show delay 800 ms + fade-in, with margin
const HINT = '[data-gesture-hint]';
const VIEWPORTS = [
  { width: 375, height: 560, name: 'phone 375x560' },
  { width: 360, height: 640, name: 'phone 360x640' },
];
const REQ_ID = 'abcdef0123456789';
const CODE = '7KX9QM';

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + (e && e.stack || e)); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

const hintsShown = (page) => page.evaluate((H) =>
  Array.from(document.querySelectorAll(H)).map(e => e.getAttribute('data-gesture-hint')), HINT);

async function waitHints(page, ids) {
  await page.waitForFunction(({ H, ids }) => ids.every(id => document.querySelector(H + '[data-gesture-hint="' + id + '"]')),
    { H: HINT, ids }, { timeout: 10000 });
  await page.waitForTimeout(400); // past the 240 ms slide-in
}

// swallowed() reports every tap a hint takes away from the page. A point
// counts when a tap there hits something else with the hints present than
// with them hidden. It samples (a) a 6 px grid over each hint outside its
// "Got it" button, where every tap must reach the page, and (b) the middle
// of every visible control, which no part of a hint, the button included,
// may cover.
async function swallowed(page) {
  return page.evaluate((H) => {
    const hints = Array.from(document.querySelectorAll(H));
    const name = (t) => !t ? 'nothing' : t.tagName.toLowerCase() + (t.id ? '#' + t.id : '') +
      (typeof t.className === 'string' && t.className.trim() ? '.' + t.className.trim().split(/\s+/).join('.') : '');
    const pts = [];
    for (const h of hints) {
      const r = h.getBoundingClientRect();
      const b = h.querySelector('[data-gesture-hint-dismiss]').getBoundingClientRect();
      for (let x = r.left + 1; x < r.right; x += 6) {
        for (let y = r.top + 1; y < r.bottom; y += 6) {
          if (x >= b.left && x <= b.right && y >= b.top && y <= b.bottom) continue;
          if (x < 0 || y < 0 || x >= innerWidth || y >= innerHeight) continue;
          pts.push({ x, y, what: 'pill of ' + h.getAttribute('data-gesture-hint') });
        }
      }
    }
    const controls = Array.from(document.querySelectorAll(
      'a[href], button, input, select, textarea, [role="button"], [role="tab"], tr[data-action="select"]'))
      .filter(c => !c.closest(H));
    for (const c of controls) {
      const r = c.getBoundingClientRect();
      if (!r.width || !r.height) continue;
      const x = r.left + r.width / 2, y = r.top + r.height / 2;
      if (x < 0 || y < 0 || x >= innerWidth || y >= innerHeight) continue;
      pts.push({ x, y, what: 'middle of ' + name(c) + ' "' + (c.getAttribute('aria-label') || c.textContent || '').trim().slice(0, 30) + '"', control: c });
    }
    const withHints = pts.map(p => document.elementFromPoint(p.x, p.y));
    const saved = hints.map(h => h.style.visibility);
    hints.forEach(h => { h.style.visibility = 'hidden'; });
    const without = pts.map(p => document.elementFromPoint(p.x, p.y));
    hints.forEach((h, i) => { h.style.visibility = saved[i]; });
    const bad = [];
    pts.forEach((p, i) => {
      const lost = p.control ? (p.control.contains(without[i]) && !p.control.contains(withHints[i]))
        : withHints[i] !== without[i];
      if (lost) bad.push(p.what + ' at (' + Math.round(p.x) + ',' + Math.round(p.y) + ') hits ' + name(withHints[i]) + ', not ' + name(without[i]));
    });
    return { hints: hints.length, points: pts.length, bad };
  }, HINT);
}
async function assertNothingSwallowed(page, where) {
  const s = await swallowed(page);
  assert(s.bad.length === 0, where + ': ' + s.bad.length + ' of ' + s.points + ' taps go to a hint: ' + s.bad.slice(0, 4).join('; '));
  return s;
}

// coveredPoint finds a point of the control that a hint's pill covers
// (outside its button), nearest the control's middle: the tap that a
// blocking hint would take. Null when no hint covers the control.
async function coveredPoint(page, sel) {
  return page.evaluate(({ H, sel }) => {
    const c = typeof sel === 'string' ? document.querySelector(sel) : sel;
    if (!c) return null;
    const r = c.getBoundingClientRect();
    const mx = r.left + r.width / 2, my = r.top + r.height / 2;
    let best = null;
    for (const h of document.querySelectorAll(H)) {
      const p = h.querySelector('.gesture-hint-inner').getBoundingClientRect();
      const b = h.querySelector('[data-gesture-hint-dismiss]').getBoundingClientRect();
      for (let x = Math.max(r.left, p.left) + 2; x < Math.min(r.right, p.right) - 2; x += 3) {
        for (let y = Math.max(r.top, p.top) + 2; y < Math.min(r.bottom, p.bottom, innerHeight) - 2; y += 3) {
          if (x >= b.left - 2 && x <= b.right + 2 && y >= b.top - 2 && y <= b.bottom + 2) continue;
          const d = (x - mx) * (x - mx) + (y - my) * (y - my);
          if (!best || d < best.d) best = { x, y, d, hint: h.getAttribute('data-gesture-hint') };
        }
      }
    }
    return best;
  }, { H: HINT, sel });
}

async function middleOf(page, sel) {
  await page.locator(sel).first().scrollIntoViewIfNeeded();
  const b = await page.locator(sel).first().boundingBox();
  assert(b, sel + ' has no box');
  return { x: b.x + b.width / 2, y: b.y + b.height / 2 };
}

(async () => {
  let browser;
  try {
    browser = await chromium.launch({
      headless: true,
      executablePath: process.env.CHROMIUM_PATH || undefined,
      args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
    });
  } catch (err) {
    if (process.env.CHROMIUM_REQUIRE === '1') { console.error(err); process.exit(1); }
    console.log(`test-issue-911-gesture-hints-non-blocking-e2e.js: SKIP (Chromium unavailable: ${err.message.split('\n')[0]})`);
    process.exit(0);
  }
  console.log(`\n=== #911 gesture hints never swallow a tap, against ${BASE} ===\n`);

  const nodes = await (await fetch(BASE + '/api/nodes?limit=500')).json();
  const repeater = (nodes.nodes || []).find(n => n.role === 'repeater' && n.lat && n.lon && n.name);
  assert(repeater, 'fixture needs a positioned repeater');
  const PK = repeater.public_key;

  // A cold start: a fresh context, no init script, nothing in localStorage.
  // The antenna-claim API is answered here: the CI server runs with it off.
  async function coldPhone(vp) {
    const ctx = await browser.newContext({
      viewport: { width: vp.width, height: vp.height }, hasTouch: true, isMobile: true });
    const pathOf = u => new URL(u).pathname;
    await ctx.route(u => pathOf(u) === '/api/antenna-claims/config', r =>
      r.fulfill({ status: 200, contentType: 'application/json', body: '{"enabled":true}' }));
    await ctx.route(u => /^\/api\/nodes\/[0-9a-f]{64}\/antenna-claims$/.test(pathOf(u)), r =>
      r.fulfill({ status: 202, contentType: 'application/json', body: JSON.stringify({
        requestId: REQ_ID, code: CODE, expiresAt: Date.now() + 86400000, nameBudgetBytes: 23,
        suggestedName: 'Rpt ' + CODE }) }));
    await ctx.route(u => /^\/api\/antenna-claims\/requests\//.test(pathOf(u)), r =>
      r.fulfill({ status: 200, contentType: 'application/json', body: '{"status":"pending"}' }));
    const page = await ctx.newPage();
    const errors = [];
    page.on('pageerror', e => errors.push(String(e)));
    return { ctx, page, errors };
  }
  const flags = (page) => page.evaluate(() => Object.keys(localStorage)
    .filter(k => k.indexOf('meshcore-gesture-hints-') === 0)
    .reduce((o, k) => { o[k.slice('meshcore-gesture-hints-'.length)] = localStorage.getItem(k); return o; }, {}));

  const homeCovered = [];
  for (const vp of VIEWPORTS) {
    await step(vp.name + ': Home, the first tap on "I know what I\'m doing" reaches it and dismisses the hints', async () => {
      const { ctx, page, errors } = await coldPhone(vp);
      await page.goto(BASE + '/#/', { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('#chooseExp', { timeout: 15000 });
      assert(Object.keys(await flags(page)).length === 0, 'the cold start was not cold');
      await waitHints(page, ['tab-swipe', 'edge-drawer']);
      await assertNothingSwallowed(page, 'Home');
      // At 360x640 the pill sits below the button: then the middle is tapped.
      const p = (await coveredPoint(page, '#chooseExp')) || Object.assign(await middleOf(page, '#chooseExp'), { hint: 'no' });
      if (p.hint !== 'no') homeCovered.push(vp.name);
      await page.touchscreen.tap(p.x, p.y);
      await page.waitForFunction(() => !document.getElementById('chooseExp'), null, { timeout: 3000 })
        .catch(() => { throw new Error('the tap at (' + Math.round(p.x) + ',' + Math.round(p.y) + ') on the ' + p.hint + ' pill did not reach the button'); });
      assert((await hintsShown(page)).length === 0, 'a tap outside "Got it" left hints up: ' + (await hintsShown(page)).join(','));
      const f = await flags(page);
      assert(f['tab-swipe'] === 'seen' && f['edge-drawer'] === 'seen', 'the outside tap did not dismiss the hints: ' + JSON.stringify(f));
      assert(errors.length === 0, 'page errors: ' + errors.join(' | '));
      await ctx.close();
    });

    await step(vp.name + ': Nodes, the first tap on the search field reaches it through the edge-swipe pill', async () => {
      const { ctx, page } = await coldPhone(vp);
      await page.goto(BASE + '/#/nodes', { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('#nodeSearch', { timeout: 15000 });
      await waitHints(page, ['edge-drawer']);
      await assertNothingSwallowed(page, 'Nodes');
      const p = await coveredPoint(page, '#nodeSearch');
      assert(p && p.hint === 'edge-drawer', 'the edge-swipe pill does not cover the search field, so this case would prove nothing');
      await page.touchscreen.tap(p.x, p.y);
      assert(await page.evaluate(() => document.activeElement && document.activeElement.id === 'nodeSearch'),
        'the first tap at (' + Math.round(p.x) + ',' + Math.round(p.y) + ') did not focus the search field');
      await page.keyboard.type('zz');
      assert((await page.inputValue('#nodeSearch')) === 'zz', 'typing after the first tap did not reach the search field');
      await ctx.close();
    });

    await step(vp.name + ': Home > More > Nodes, a row under the row-swipe pill opens on the first tap, no hint follows, Back to nodes', async () => {
      const { ctx, page, errors } = await coldPhone(vp);
      await page.goto(BASE + '/#/', { waitUntil: 'domcontentloaded' });
      await waitHints(page, ['tab-swipe', 'edge-drawer']);
      let p = await middleOf(page, '[data-bottom-nav-tab="more"]');
      await page.touchscreen.tap(p.x, p.y);
      await page.waitForSelector('[data-bottom-nav-more-route="nodes"]', { state: 'visible', timeout: 5000 });
      p = await middleOf(page, '[data-bottom-nav-more-route="nodes"]');
      await page.touchscreen.tap(p.x, p.y);
      await page.waitForSelector('#nodesTable tbody tr[data-action="select"]', { timeout: 15000 });
      await waitHints(page, ['row-swipe']);
      await assertNothingSwallowed(page, 'Nodes after More');
      // Bring a row under the pill the way a user's scroll would, without a
      // tap (a tap would dismiss the hint), then tap the row there.
      const key = await page.evaluate((H) => {
        const pill = document.querySelector(H + '[data-gesture-hint="row-swipe"] .gesture-hint-inner').getBoundingClientRect();
        const rows = Array.from(document.querySelectorAll('#nodesTable tbody tr[data-action="select"]'));
        const row = rows[Math.min(3, rows.length - 1)];
        let s = row.parentElement;
        while (s && !(s.scrollHeight > s.clientHeight && /(auto|scroll)/.test(getComputedStyle(s).overflowY))) s = s.parentElement;
        s = s || document.scrollingElement;
        const r = row.getBoundingClientRect();
        s.scrollTop += (r.top + r.height / 2) - (pill.top + pill.height / 2);
        return row.getAttribute('data-key');
      }, HINT);
      await page.waitForTimeout(200);
      const rowSel = '#nodesTable tbody tr[data-key="' + key + '"]';
      p = await coveredPoint(page, rowSel);
      assert(p && p.hint === 'row-swipe', 'the row-swipe pill does not cover the row, so this case would prove nothing');
      await assertNothingSwallowed(page, 'Nodes with a row under the pill');
      await page.touchscreen.tap(p.x, p.y);
      await page.waitForFunction((k) => location.hash.indexOf('#/nodes/' + k) === 0, key, { timeout: 5000 })
        .catch(() => { throw new Error('the first tap at (' + Math.round(p.x) + ',' + Math.round(p.y) + ') on the row did not open the node'); });
      await page.waitForSelector('#nodeBackBtn', { timeout: 15000 });
      await page.waitForTimeout(SHOW_WAIT_MS);
      const shown = await hintsShown(page);
      assert(shown.length === 0, 'hints followed onto the node page: ' + shown.join(','));
      await assertNothingSwallowed(page, 'node page');
      p = await middleOf(page, '#nodeBackBtn');
      await page.touchscreen.tap(p.x, p.y);
      await page.waitForFunction(() => /^#\/nodes\/?(\?|$)/.test(location.hash), null, { timeout: 5000 })
        .catch(() => { throw new Error('the first tap on Back to nodes did not go back'); });
      assert(errors.length === 0, 'page errors: ' + errors.join(' | '));
      await ctx.close();
    });

    await step(vp.name + ': node page from a cold deep link, no hint, real taps on height, Get code and Start over', async () => {
      const { ctx, page } = await coldPhone(vp);
      await page.goto(BASE + '/#/nodes/' + PK + '?section=node-antenna', { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('[data-testid="antenna-claim"]:visible', { timeout: 15000 });
      await page.waitForTimeout(SHOW_WAIT_MS);
      assert((await hintsShown(page)).length === 0, 'a hint shows on the node page: ' + (await hintsShown(page)).join(','));
      await assertNothingSwallowed(page, 'claim section');
      let p = await middleOf(page, '[data-testid="ac-height"]');
      await page.touchscreen.tap(p.x, p.y);
      await page.keyboard.type('18.5');
      assert((await page.inputValue('[data-testid="ac-height"]')) === '18.5', 'the first tap did not focus the height field');
      p = await middleOf(page, '[data-testid="ac-submit"]');
      await page.touchscreen.tap(p.x, p.y);
      await page.waitForSelector('[data-testid="ac-code"]', { timeout: 5000 });
      assert((await page.textContent('[data-testid="ac-code"]')) === CODE, 'Get code did not show the code');
      p = await middleOf(page, '[data-testid="ac-new"]');
      await page.touchscreen.tap(p.x, p.y);
      await page.waitForSelector('[data-testid="ac-height"]', { timeout: 5000 })
        .catch(() => { throw new Error('the tap on Start over did not bring the height field back'); });
      await ctx.close();
    });

    await step(vp.name + ': "Got it" works on the first tap, also while the hint fades in', async () => {
      const { ctx, page } = await coldPhone(vp);
      await page.goto(BASE + '/#/packets', { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('[data-gesture-hint="row-swipe"]', { timeout: 10000 });
      // Tap as soon as the hint exists: the fade-in has just started.
      const b = await page.locator('[data-gesture-hint="row-swipe"] [data-gesture-hint-dismiss]').boundingBox();
      assert(b && b.width > 0 && b.height >= 32, 'the button has no usable box: ' + JSON.stringify(b));
      await page.touchscreen.tap(b.x + b.width / 2, b.y + b.height / 2);
      await page.waitForFunction(() => !document.querySelector('[data-gesture-hint="row-swipe"]'), null, { timeout: 2000 });
      const f = await flags(page);
      assert(f['row-swipe'] === 'seen', 'the dismissal was not stored: ' + JSON.stringify(f));
      await ctx.close();
    });
  }

  await step('Home: at least one phone size tapped "I know what I\'m doing" through a pill', async () => {
    assert(homeCovered.length > 0, 'no hint covered the button at either size, so the Home case proved nothing');
  });

  await step('probe self-check: a blocking hint and a covered control are both caught', async () => {
    const { ctx, page } = await coldPhone(VIEWPORTS[0]);
    await page.goto(BASE + '/#/nodes', { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#nodeSearch', { timeout: 15000 });
    await waitHints(page, ['edge-drawer']);
    await page.addStyleTag({ content: '.gesture-hint-inner { pointer-events: auto !important }' });
    const s = await swallowed(page);
    assert(s.bad.some(b => /^pill of edge-drawer/.test(b)), 'a blocking pill was not caught: ' + JSON.stringify(s));
    await page.addStyleTag({ content: '.gesture-hint-inner { pointer-events: none !important }' });
    await page.evaluate(() => {
      const c = document.getElementById('nodeSearch').getBoundingClientRect();
      const b = document.querySelector('[data-gesture-hint="edge-drawer"] [data-gesture-hint-dismiss]');
      b.style.cssText += ';position:fixed;left:' + (c.left + c.width / 2 - 20) + 'px;top:' + (c.top + c.height / 2 - 20) + 'px';
    });
    const s2 = await swallowed(page);
    assert(s2.bad.some(b => /middle of input#nodeSearch/.test(b)), 'a button over the search field was not caught: ' + JSON.stringify(s2));
    await ctx.close();
  });

  await browser.close();
  console.log(`\ntest-issue-911-gesture-hints-non-blocking-e2e.js: ${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
