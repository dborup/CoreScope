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
 *      tab-swipe pill covers) reaches the button and dismisses that hint
 *      alone;
 *   2. Nodes: the first tap on the search field, on a point the edge-swipe
 *      pill covers, focuses it and dismisses the edge-swipe hint alone;
 *   3. the real flow Home > More > Nodes: the taps on More and Nodes
 *      dismiss nothing; on Nodes the row-swipe hint replaces the tab-swipe
 *      one (a tab swipe does nothing there); the first tap on a node row, on
 *      a point the row-swipe pill covers, opens the node; neither list hint
 *      follows onto the node page; Back to nodes takes the first tap there;
 *   4. the node page from a cold deep link shows the edge-swipe hint only,
 *      and real taps on the claim controls work: height field, Get code,
 *      Start over;
 *   5. "Got it" takes the first tap while the hint fades in: the tap hits
 *      the button itself, not the page under it;
 *   6. one hint per slot (Packets: row swipe, not tab swipe as well), and
 *      no two pills overlap; a tap outside every pill and a scroll that
 *      starts on a pill dismiss nothing;
 *   7. "Got it" never lies on the middle of a control: with a control put
 *      where the button is, the hint moves and "Got it" still takes a tap;
 *   8. the probes themselves: a blocking hint and a covered control turn
 *      them red, so the suite cannot pass vacuously.
 *
 * Every check taps, or asks what a tap at a real coordinate would hit; none
 * asserts where a hint sits.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-issue-911-gesture-hints-non-blocking-e2e.js
 */
'use strict';
const { chromium } = require('playwright');
const { doubleTapWait } = require('./tests/e2e-tap');

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

// pillUnder names the hint whose pill (outside its "Got it") is on top at
// (x, y), i.e. the hint a tap there goes through; null for none.
async function pillUnder(page, x, y) {
  return page.evaluate(({ H, x, y }) => {
    for (const h of document.querySelectorAll(H)) {
      const inner = h.querySelector('.gesture-hint-inner');
      const p = inner.getBoundingClientRect();
      const b = h.querySelector('[data-gesture-hint-dismiss]').getBoundingClientRect();
      if (x < p.left || x > p.right || y < p.top || y > p.bottom) continue;
      if (x >= b.left && x <= b.right && y >= b.top && y <= b.bottom) continue;
      const saved = inner.style.pointerEvents;
      inner.style.pointerEvents = 'auto';
      const t = document.elementFromPoint(x, y);
      inner.style.pointerEvents = saved;
      if (t && h.contains(t)) return h.getAttribute('data-gesture-hint');
    }
    return null;
  }, { H: HINT, x, y });
}

// tap is a finger tap at (x, y). A second tap within Chromium's double-tap
// window of the page's previous one (tests/e2e-tap.js) would be read as a
// double tap and lose its click, which a finger tapping More and then Nodes
// in the sheet never does: the window is waited out first.
const lastTapAt = new WeakMap();
async function tap(page, x, y) {
  const wait = doubleTapWait(lastTapAt.get(page), x, y, Date.now());
  if (wait > 0) await page.waitForTimeout(wait);
  await page.touchscreen.tap(x, y);
  lastTapAt.set(page, { x, y, t: Date.now() });
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
    await step(vp.name + ': Home, the first tap on "I know what I\'m doing" reaches it and dismisses only the hint it went through', async () => {
      const { ctx, page, errors } = await coldPhone(vp);
      await page.goto(BASE + '/#/', { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('#chooseExp', { timeout: 15000 });
      assert(Object.keys(await flags(page)).length === 0, 'the cold start was not cold');
      await waitHints(page, ['tab-swipe', 'edge-drawer']);
      await assertNothingSwallowed(page, 'Home');
      // At 360x640 the pill sits below the button: then the middle is tapped.
      const p = (await coveredPoint(page, '#chooseExp')) || Object.assign(await middleOf(page, '#chooseExp'), { hint: 'no' });
      if (p.hint !== 'no') homeCovered.push(vp.name);
      assert((await pillUnder(page, p.x, p.y)) === (p.hint === 'no' ? null : p.hint), 'the probes disagree on the pill at the tap');
      await tap(page, p.x, p.y);
      await page.waitForFunction(() => !document.getElementById('chooseExp'), null, { timeout: 3000 })
        .catch(() => { throw new Error('the tap at (' + Math.round(p.x) + ',' + Math.round(p.y) + ') on the ' + p.hint + ' pill did not reach the button'); });
      // Only the hint the tap went through is dismissed; the other stays up.
      const gone = p.hint === 'no' ? [] : [p.hint];
      const left = ['tab-swipe', 'edge-drawer'].filter(id => gone.indexOf(id) < 0);
      const shown = await hintsShown(page);
      assert(gone.every(id => shown.indexOf(id) < 0), 'the hint the tap went through is still up: ' + shown.join(','));
      assert(left.every(id => shown.indexOf(id) >= 0), 'a hint the tap did not touch went away: ' + shown.join(','));
      const f = await flags(page);
      assert(JSON.stringify(Object.keys(f).sort()) === JSON.stringify(gone) && gone.every(id => f[id] === 'seen'),
        'want only ' + JSON.stringify(gone) + ' marked as seen, got ' + JSON.stringify(f));
      assert(errors.length === 0, 'page errors: ' + errors.join(' | '));
      await ctx.close();
    });

    await step(vp.name + ': Nodes, the first tap on the search field reaches it through the edge-swipe pill and dismisses that hint alone', async () => {
      const { ctx, page } = await coldPhone(vp);
      await page.goto(BASE + '/#/nodes', { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('#nodeSearch', { timeout: 15000 });
      await waitHints(page, ['edge-drawer', 'row-swipe']);
      await assertNothingSwallowed(page, 'Nodes');
      const p = await coveredPoint(page, '#nodeSearch');
      assert(p && p.hint === 'edge-drawer', 'the edge-swipe pill does not cover the search field, so this case would prove nothing');
      await tap(page, p.x, p.y);
      assert(await page.evaluate(() => document.activeElement && document.activeElement.id === 'nodeSearch'),
        'the first tap at (' + Math.round(p.x) + ',' + Math.round(p.y) + ') did not focus the search field');
      await page.keyboard.type('zz');
      assert((await page.inputValue('#nodeSearch')) === 'zz', 'typing after the first tap did not reach the search field');
      const shown = await hintsShown(page);
      assert(shown.indexOf('edge-drawer') < 0 && shown.indexOf('row-swipe') >= 0, 'want only the edge-swipe hint gone, shown: ' + shown.join(','));
      const f = await flags(page);
      assert(JSON.stringify(f) === '{"edge-drawer":"seen"}', 'want only edge-drawer marked as seen, got ' + JSON.stringify(f));
      await ctx.close();
    });

    await step(vp.name + ': Home > More > Nodes, a row under the row-swipe pill opens on the first tap, no list hint follows, Back to nodes', async () => {
      const { ctx, page, errors } = await coldPhone(vp);
      await page.goto(BASE + '/#/', { waitUntil: 'domcontentloaded' });
      await waitHints(page, ['tab-swipe', 'edge-drawer']);
      let p = await middleOf(page, '[data-bottom-nav-tab="more"]');
      assert(await pillUnder(page, p.x, p.y) === null, 'More lies under a pill: this case assumes taps that miss every pill');
      await tap(page, p.x, p.y);
      await page.waitForSelector('[data-bottom-nav-more-route="nodes"]', { state: 'visible', timeout: 5000 });
      p = await middleOf(page, '[data-bottom-nav-more-route="nodes"]');
      assert(await pillUnder(page, p.x, p.y) === null, 'Nodes in the sheet lies under a pill: this case assumes taps that miss every pill');
      await tap(page, p.x, p.y);
      await page.waitForFunction(() => /^#\/nodes\/?(\?|$)/.test(location.hash), null, { timeout: 5000 })
        .catch(() => { throw new Error('the tap on Nodes in the More sheet did not open #/nodes'); });
      await page.waitForSelector('#nodesTable tbody tr[data-action="select"]', { timeout: 15000 });
      await waitHints(page, ['row-swipe']);
      assert(JSON.stringify(await flags(page)) === '{}', 'taps outside every pill marked hints as seen: ' + JSON.stringify(await flags(page)));
      assert((await hintsShown(page)).indexOf('tab-swipe') < 0, 'the tab-swipe hint is up on Nodes, where a tab swipe does nothing');
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
      await tap(page, p.x, p.y);
      await page.waitForFunction((k) => location.hash.indexOf('#/nodes/' + k) === 0, key, { timeout: 5000 })
        .catch(() => { throw new Error('the first tap at (' + Math.round(p.x) + ',' + Math.round(p.y) + ') on the row did not open the node'); });
      await page.waitForSelector('#nodeBackBtn', { timeout: 15000 });
      assert((await flags(page))['row-swipe'] === 'seen', 'the tap through the row-swipe pill did not dismiss it');
      await page.waitForTimeout(SHOW_WAIT_MS);
      const shown = await hintsShown(page);
      assert(shown.indexOf('row-swipe') < 0 && shown.indexOf('tab-swipe') < 0, 'a list hint followed onto the node page: ' + shown.join(','));
      await assertNothingSwallowed(page, 'node page');
      p = await middleOf(page, '#nodeBackBtn');
      await tap(page, p.x, p.y);
      await page.waitForFunction(() => /^#\/nodes\/?(\?|$)/.test(location.hash), null, { timeout: 5000 })
        .catch(() => { throw new Error('the first tap on Back to nodes did not go back'); });
      assert(errors.length === 0, 'page errors: ' + errors.join(' | '));
      await ctx.close();
    });

    await step(vp.name + ': node page from a cold deep link, only the edge-swipe hint, real taps on height, Get code and Start over', async () => {
      const { ctx, page } = await coldPhone(vp);
      await page.goto(BASE + '/#/nodes/' + PK + '?section=node-antenna', { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('[data-testid="antenna-claim"]:visible', { timeout: 15000 });
      // The edge swipe is not tied to a page, so its hint may show here; the
      // row and tab swipes do nothing on a node page, so theirs may not.
      await waitHints(page, ['edge-drawer']);
      await page.waitForTimeout(SHOW_WAIT_MS);
      const shown = await hintsShown(page);
      assert(JSON.stringify(shown) === '["edge-drawer"]', 'want only the edge-swipe hint on the node page, shown: ' + shown.join(','));
      await assertNothingSwallowed(page, 'claim section');
      let p = await middleOf(page, '[data-testid="ac-height"]');
      await tap(page, p.x, p.y);
      await page.keyboard.type('18.5');
      assert((await page.inputValue('[data-testid="ac-height"]')) === '18.5', 'the first tap did not focus the height field');
      p = await middleOf(page, '[data-testid="ac-submit"]');
      await tap(page, p.x, p.y);
      await page.waitForSelector('[data-testid="ac-code"]', { timeout: 5000 });
      assert((await page.textContent('[data-testid="ac-code"]')) === CODE, 'Get code did not show the code');
      p = await middleOf(page, '[data-testid="ac-new"]');
      await tap(page, p.x, p.y);
      await page.waitForSelector('[data-testid="ac-height"]', { timeout: 5000 })
        .catch(() => { throw new Error('the tap on Start over did not bring the height field back'); });
      await ctx.close();
    });

    await step(vp.name + ': "Got it" works on the first tap, also while the hint fades in', async () => {
      const { ctx, page } = await coldPhone(vp);
      await page.goto(BASE + '/#/packets', { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('[data-gesture-hint="row-swipe"]', { timeout: 10000 });
      // Tap as soon as the hint exists: the fade-in has just started.
      await page.evaluate(() => {
        window.__t911Click = null;
        window.addEventListener('click', (e) => { window.__t911Click = !!(e.target.closest && e.target.closest('[data-gesture-hint-dismiss]')); }, { capture: true, once: true });
      });
      const b = await page.locator('[data-gesture-hint="row-swipe"] [data-gesture-hint-dismiss]').boundingBox();
      assert(b && b.width > 0 && b.height >= 32, 'the button has no usable box: ' + JSON.stringify(b));
      const x = b.x + b.width / 2, y = b.y + b.height / 2;
      // The tap must hit "Got it" itself: a tap that fell through to the page
      // would also remove the hint (through its pill), and so prove nothing.
      const hit = await page.evaluate(({ x, y }) => { const t = document.elementFromPoint(x, y); return !!(t && t.closest('[data-gesture-hint-dismiss]')); }, { x, y });
      assert(hit, 'a tap at the middle of "Got it" does not hit the button');
      await tap(page, x, y);
      await page.waitForFunction(() => !document.querySelector('[data-gesture-hint="row-swipe"]'), null, { timeout: 2000 });
      assert(await page.evaluate(() => window.__t911Click) === true, 'the tap\'s click did not go to "Got it"');
      const f = await flags(page);
      assert(f['row-swipe'] === 'seen', 'the dismissal was not stored: ' + JSON.stringify(f));
      await ctx.close();
    });

    await step(vp.name + ': Packets, the row and tab swipes share a slot: one hint shows there, and no two pills overlap', async () => {
      const { ctx, page } = await coldPhone(vp);
      await page.goto(BASE + '/#/packets', { waitUntil: 'domcontentloaded' });
      await waitHints(page, ['row-swipe']);
      await page.waitForTimeout(SHOW_WAIT_MS);
      const r = await page.evaluate((H) => Array.from(document.querySelectorAll(H)).map((h) => {
        const b = h.querySelector('.gesture-hint-inner').getBoundingClientRect();
        return { id: h.getAttribute('data-gesture-hint'), l: b.left, t: b.top, r: b.right, b: b.bottom };
      }), HINT);
      assert(r.every(h => h.id !== 'tab-swipe'), 'the tab-swipe hint shows next to the row-swipe one: ' + JSON.stringify(r));
      for (let i = 0; i < r.length; i++) for (let j = i + 1; j < r.length; j++) {
        const a = r[i], c = r[j];
        assert(a.r <= c.l || c.r <= a.l || a.b <= c.t || c.b <= a.t, a.id + ' and ' + c.id + ' overlap');
      }
      await ctx.close();
    });

    await step(vp.name + ': a tap outside every pill and a scroll that starts on a pill dismiss nothing', async () => {
      const { ctx, page } = await coldPhone(vp);
      await page.goto(BASE + '/#/nodes', { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('#nodesTable tbody tr[data-action="select"]', { timeout: 15000 });
      await waitHints(page, ['row-swipe', 'edge-drawer']);
      // A tap on a control no pill lies over: the active role tab, a no-op.
      const free = await page.evaluate((H) => {
        const pills = Array.from(document.querySelectorAll(H + ' .gesture-hint-inner')).map(e => e.getBoundingClientRect());
        for (const c of document.querySelectorAll('.node-tab.active')) {
          const r = c.getBoundingClientRect();
          const x = r.left + r.width / 2, y = r.top + r.height / 2;
          if (!r.width || y < 0 || y > innerHeight) continue;
          if (pills.some(p => x >= p.left - 4 && x <= p.right + 4 && y >= p.top - 4 && y <= p.bottom + 4)) continue;
          if (document.elementFromPoint(x, y) !== c && !c.contains(document.elementFromPoint(x, y))) continue;
          return { x, y };
        }
        return null;
      }, HINT);
      assert(free, 'no control outside the pills to tap');
      await tap(page, free.x, free.y);
      await page.waitForTimeout(300);
      assert(JSON.stringify(await flags(page)) === '{}', 'a tap outside every pill marked hints as seen: ' + JSON.stringify(await flags(page)));
      assert((await hintsShown(page)).length === 2, 'a tap outside every pill removed a hint: ' + (await hintsShown(page)).join(','));
      // A touch scroll whose first touch lands on the row-swipe pill.
      const pill = await page.locator('[data-gesture-hint="row-swipe"] .gesture-hint-inner').boundingBox();
      const sx = pill.x + 24, sy = pill.y + pill.height / 2;
      assert(await pillUnder(page, sx, sy) === 'row-swipe', 'the scroll would not start on the pill');
      const scrolled = await page.evaluate(() => {
        const t = document.querySelector('#nodesTable tbody');
        let s = t && t.parentElement;
        while (s && !(s.scrollHeight > s.clientHeight && /(auto|scroll)/.test(getComputedStyle(s).overflowY))) s = s.parentElement;
        s = s || document.scrollingElement;
        window.__t911Scroller = s;
        return s.scrollTop;
      });
      // A real touch drag: the browser scrolls and cancels the pointer.
      const cdp = await ctx.newCDPSession(page);
      await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [{ x: sx, y: sy }] });
      for (let i = 1; i <= 10; i++) {
        await cdp.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [{ x: sx, y: sy - 12 * i }] });
        await page.waitForTimeout(16);
      }
      await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
      await page.waitForTimeout(300);
      const after = await page.evaluate(() => window.__t911Scroller.scrollTop);
      assert(after > scrolled, 'the gesture did not scroll (' + scrolled + ' -> ' + after + '), so this case would prove nothing');
      assert(JSON.stringify(await flags(page)) === '{}', 'a scroll marked hints as seen: ' + JSON.stringify(await flags(page)));
      const shown = await hintsShown(page);
      assert(shown.indexOf('row-swipe') >= 0, 'a scroll that started on the row-swipe pill removed it: ' + shown.join(','));
      await ctx.close();
    });

    await step(vp.name + ': "Got it" never lies on a control\'s middle: the hint moves off a control put under it', async () => {
      const { ctx, page } = await coldPhone(vp);
      await page.goto(BASE + '/#/nodes', { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('#nodesTable tbody tr[data-action="select"]', { timeout: 15000 });
      await waitHints(page, ['row-swipe', 'edge-drawer']);
      // A control where each "Got it" is, as a different font or data puts
      // one there (CI's layout put Domestic under the row-swipe button).
      const before = await page.evaluate((H) => Array.from(document.querySelectorAll(H)).map((h) => {
        const b = h.querySelector('[data-gesture-hint-dismiss]').getBoundingClientRect();
        const c = document.createElement('button');
        c.type = 'button';
        c.className = 't911-under';
        c.textContent = 'Under';
        c.style.cssText = 'position:fixed;z-index:1;width:72px;height:44px;left:' + (b.left + b.width / 2 - 36) + 'px;top:' + (b.top + b.height / 2 - 22) + 'px';
        document.body.appendChild(c);
        return { id: h.getAttribute('data-gesture-hint'), x: b.left + b.width / 2, y: b.top + b.height / 2 };
      }), HINT);
      assert(before.length === 2, 'want two hints, got ' + JSON.stringify(before));
      await page.waitForTimeout(700); // more than one placement poll
      await assertNothingSwallowed(page, 'Nodes with a control under each "Got it"');
      for (const h of before) {
        const b = await page.locator('[data-gesture-hint="' + h.id + '"] [data-gesture-hint-dismiss]').boundingBox();
        assert(b && await page.locator('[data-gesture-hint="' + h.id + '"]').isVisible(), h.id + ' is not shown any more');
        const x = b.x + b.width / 2, y = b.y + b.height / 2;
        assert(Math.abs(x - h.x) + Math.abs(y - h.y) > 1, h.id + '\'s "Got it" did not move off the control');
        const hit = await page.evaluate(({ x, y }) => { const t = document.elementFromPoint(x, y); return !!(t && t.closest('[data-gesture-hint-dismiss]')); }, { x, y });
        assert(hit, h.id + '\'s "Got it" does not take a tap where it moved to');
        await tap(page, x, y);
        await page.waitForFunction((id) => !document.querySelector('[data-gesture-hint="' + id + '"]'), h.id, { timeout: 2000 });
      }
      const f = await flags(page);
      assert(f['row-swipe'] === 'seen' && f['edge-drawer'] === 'seen', 'the moved "Got it" taps were not stored: ' + JSON.stringify(f));
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
    // The hints' placement would move the button off the field again (the
    // product at work): pin the hints so that only the probe is under test.
    await page.addStyleTag({ content: '[data-gesture-hint] { translate: none !important }' });
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
