/**
 * E2E (#911): first-visit gesture hints must never block a page control.
 *
 * Cold start on a phone (nothing pre-seeded in localStorage, real touch
 * taps), at 375x560 and 360x640:
 *
 *   1. node page with the owner antenna-claim controls: after the hints'
 *      show delay, the element a finger hits at the middle of each claim
 *      control (height field, submit) is the control itself, and no hint
 *      whose gesture does not apply there (row swipe, tab swipe) is shown;
 *   2. a first tap on a claim control works (focus + typing) while hints
 *      are up;
 *   3. on a list page the hint is shown and "Got it" works on the FIRST
 *      real tap, even while the hint is still fading in;
 *   4. the hint left from a list page is gone after navigating to a node
 *      page (a hint does not carry over onto a page where it does not
 *      apply);
 *   5. the probe itself: a blocking overlay laid over a control makes the
 *      control check go red (so the test cannot pass vacuously).
 *
 * Usage: BASE_URL=http://localhost:13911 node test-issue-911-gesture-hints-non-blocking-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const SECTION = '[data-testid="antenna-claim"]';
const CONTROLS = ['[data-testid="ac-height"]', '[data-testid="ac-submit"]'];
const SHOW_WAIT_MS = 1600; // show delay 800 ms + fade-in, with margin
const VIEWPORTS = [
  { width: 375, height: 560, name: 'phone 375x560' },
  { width: 360, height: 640, name: 'phone 360x640' },
];

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + (e && e.stack || e)); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

// hitAt centres the control and reports what a finger at its middle hits.
async function hitAt(page, sel) {
  return page.evaluate((s) => {
    const el = document.querySelector(s);
    if (!el) return { found: false };
    el.scrollIntoView({ block: 'center', inline: 'nearest' });
    const r = el.getBoundingClientRect();
    const x = r.left + r.width / 2, y = r.top + r.height / 2;
    const t = document.elementFromPoint(x, y);
    return {
      found: true, x, y,
      ok: !!t && (t === el || el.contains(t)),
      hit: t ? t.tagName.toLowerCase() + (typeof t.className === 'string' && t.className ? '.' + t.className.trim().split(/\s+/).join('.') : '') : null,
    };
  }, sel);
}
const hintsShown = (page) => page.evaluate(() =>
  Array.from(document.querySelectorAll('[data-gesture-hint]')).map(e => e.getAttribute('data-gesture-hint')));

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #911 gesture hints non-blocking against ${BASE} ===\n`);

  const nodes = await (await fetch(BASE + '/api/nodes?limit=500')).json();
  const repeater = (nodes.nodes || []).find(n => n.role === 'repeater' && n.lat && n.lon && n.name);
  assert(repeater, 'fixture needs a positioned repeater');
  const PK = repeater.public_key;

  // A cold start: a fresh context, no init script, nothing in localStorage.
  async function coldPhone(vp) {
    const ctx = await browser.newContext({
      viewport: { width: vp.width, height: vp.height }, hasTouch: true, isMobile: true });
    await ctx.route(u => new URL(u).pathname === '/api/antenna-claims/config', r =>
      r.fulfill({ status: 200, contentType: 'application/json', body: '{"enabled":true}' }));
    const page = await ctx.newPage();
    const errors = [];
    page.on('pageerror', e => errors.push(String(e)));
    return { ctx, page, errors };
  }
  async function openNode(page) {
    await page.goto(BASE + '/#/nodes/' + PK + '?section=node-antenna', { waitUntil: 'domcontentloaded' });
    await page.waitForSelector(SECTION + ':visible', { timeout: 15000 });
    await page.waitForTimeout(SHOW_WAIT_MS);
  }

  for (const vp of VIEWPORTS) {
    await step(vp.name + ': claim controls are the hit target, no inapplicable hint on the node page', async () => {
      const { ctx, page, errors } = await coldPhone(vp);
      await openNode(page);
      assert((await page.evaluate(() => localStorage.length)) === 0 ||
        !(await page.evaluate(() => Object.keys(localStorage).some(k => k.indexOf('meshcore-gesture-hints-') === 0))),
        'the cold start was not cold');
      for (const sel of CONTROLS) {
        const h = await hitAt(page, sel);
        assert(h.found, sel + ' not rendered');
        assert(h.ok, sel + ' is covered at (' + Math.round(h.x) + ',' + Math.round(h.y) + ') by ' + h.hit);
      }
      const shown = await hintsShown(page);
      assert(!shown.includes('row-swipe') && !shown.includes('tab-swipe'),
        'hints whose gesture does not apply on a node page are up: ' + shown.join(','));
      assert(errors.length === 0, 'page errors: ' + errors.join(' | '));
      await ctx.close();
    });

    await step(vp.name + ': the first real tap on a claim control works', async () => {
      const { ctx, page } = await coldPhone(vp);
      await openNode(page);
      const sel = CONTROLS[0];
      await hitAt(page, sel);
      const b = await page.locator(sel).boundingBox();
      await page.touchscreen.tap(b.x + b.width / 2, b.y + b.height / 2);
      await page.keyboard.type('18.5');
      assert((await page.inputValue(sel)) === '18.5', 'the first tap did not focus the height field');
      await ctx.close();
    });

    await step(vp.name + ': "Got it" works on the first tap, also while the hint fades in', async () => {
      const { ctx, page } = await coldPhone(vp);
      await page.goto(BASE + '/#/packets', { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('[data-gesture-hint="row-swipe"]', { timeout: 10000 });
      // Tap as soon as the hint exists: the fade-in has just started.
      const btn = page.locator('[data-gesture-hint="row-swipe"] [data-gesture-hint-dismiss]');
      const b = await btn.boundingBox();
      assert(b && b.width > 0 && b.height >= 32, 'the button has no usable box: ' + JSON.stringify(b));
      const hit = await page.evaluate((p) => {
        const t = document.elementFromPoint(p.x, p.y);
        return !!t && !!t.closest('[data-gesture-hint-dismiss]');
      }, { x: b.x + b.width / 2, y: b.y + b.height / 2 });
      assert(hit, 'the button is not the hit target at its middle');
      await page.touchscreen.tap(b.x + b.width / 2, b.y + b.height / 2);
      await page.waitForFunction(() => !document.querySelector('[data-gesture-hint="row-swipe"]'), null, { timeout: 2000 });
      const flag = await page.evaluate(() => localStorage.getItem('meshcore-gesture-hints-row-swipe'));
      assert(flag === 'seen', 'the dismissal was not stored: ' + flag);
      await ctx.close();
    });

    await step(vp.name + ': a list-page hint does not carry over onto the node page', async () => {
      const { ctx, page } = await coldPhone(vp);
      await page.goto(BASE + '/#/nodes', { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('[data-gesture-hint="row-swipe"]', { timeout: 10000 });
      await page.evaluate((pk) => { location.hash = '#/nodes/' + pk; }, PK);
      await page.waitForSelector('#nodeFullBody .node-full-card', { timeout: 15000 });
      await page.waitForTimeout(SHOW_WAIT_MS);
      const shown = await hintsShown(page);
      assert(!shown.includes('row-swipe'), 'row-swipe hint still up on the node page: ' + shown.join(','));
      await ctx.close();
    });
  }

  await step('probe self-check: a blocking overlay over a claim control is detected', async () => {
    const { ctx, page } = await coldPhone(VIEWPORTS[0]);
    await openNode(page);
    await page.evaluate((sel) => {
      const r = document.querySelector(sel).getBoundingClientRect();
      const d = document.createElement('div');
      d.id = 'e2e-blocker';
      d.style.cssText = 'position:fixed;z-index:99999;left:' + r.left + 'px;top:' + r.top +
        'px;width:' + r.width + 'px;height:' + r.height + 'px';
      document.body.appendChild(d);
    }, CONTROLS[0]);
    const h = await hitAt(page, CONTROLS[0]);
    assert(h.found && !h.ok && /e2e-blocker|div/.test(h.hit || ''), 'the probe did not see the overlay: ' + JSON.stringify(h));
    await ctx.close();
  });

  await browser.close();
  console.log(`\ntest-issue-911-gesture-hints-non-blocking-e2e.js: ${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
