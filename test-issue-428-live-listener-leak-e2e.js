/**
 * E2E (#428): live.js must not leak its 8 global `document`/`window`
 * handlers — the remainder of #405 the #419 sweep found but left for the
 * owner:
 *
 *   1. `window` orientationchange — a REF-MISMATCH leak: destroy() removed
 *      `_onResize` under the 'orientationchange' event name, which never
 *      matched the actual anonymous listener bound there, so it was never
 *      removed at all.
 *   2. `window` 'mc-tile-provider-changed'
 *   3. `window` resize — DragManager resize-clamping
 *   4. `document` mousemove — feed-panel resize drag
 *   5. `document` mouseup — feed-panel resize drag
 *   6. `document` click — VCR-scope "more" overflow menu outside-click
 *   7. `document` mousemove — VCR timeline scrub drag
 *   8. `document` mouseup — VCR timeline scrub drag
 *
 * The fix stores each as a module ref (with a distinctly named function —
 * `_onOrientationChange`, `_onTileProviderChanged`, `_onDragManagerResize`,
 * `_onFeedResizeMove`/`_onFeedResizeUp`, `_moreMenuDocClickHandler`,
 * `_onTimelineScrubMove`/`_onTimelineScrubUp`) and removes it in destroy().
 *
 * This runs against a real browser + Go server (the CI fixture). It installs
 * an init-script BEFORE the SPA's own scripts run, so every
 * addEventListener/removeEventListener on document/window is counted from a
 * clean baseline, filtered by the handler's OWN function name (JS name
 * inference exposes the variable name on a function expression/arrow as
 * `fn.name`) rather than by raw event type — other pages bind/unbind their
 * OWN keydown/mousemove/etc. handlers too (confirmed while building the
 * sibling #428 channels test: nodes.js's own Escape binds drift
 * independently of any of this), so a type-only count would be contaminated
 * by unrelated pages. The document never reloads on hash-only navigation, so
 * the probe survives SPA routing.
 *
 * Also covers the known Live init/teardown race (its own async init() can
 * still be resuming from an `await` after the router has already moved to
 * another page): entering and leaving Live three times in quick succession
 * (not waiting for the page to settle) must not leak any of the 8 handlers,
 * nor throw.
 *
 * Mutant: revert the live.js fix (restore the anonymous orientationchange
 * listener + the ref-mismatched removal, and stop storing/removing the
 * other 7 refs in destroy()) → the leak/stacking cases go red.
 *
 * Usage: BASE_URL=http://localhost:13950 node test-issue-428-live-listener-leak-e2e.js
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

const HANDLER_NAMES = [
  '_onOrientationChange', '_onTileProviderChanged', '_onDragManagerResize',
  '_onFeedResizeMove', '_onFeedResizeUp', '_moreMenuDocClickHandler',
  '_onTimelineScrubMove', '_onTimelineScrubUp',
];

let browser;

async function freshPage() {
  const ctx = await browser.newContext({ viewport: { width: 1400, height: 900 } });
  const page = await ctx.newPage();
  page.on('pageerror', (e) => { page.__errors = page.__errors || []; page.__errors.push(e.message); });
  await page.addInitScript((names) => {
    window.__net = {};
    names.forEach((n) => { window.__net[n] = 0; });
    function patchNet(target) {
      const origAdd = target.addEventListener.bind(target);
      const origRemove = target.removeEventListener.bind(target);
      target.addEventListener = function (type, fn, opts) {
        if (fn && Object.prototype.hasOwnProperty.call(window.__net, fn.name)) window.__net[fn.name]++;
        return origAdd(type, fn, opts);
      };
      target.removeEventListener = function (type, fn, opts) {
        if (fn && Object.prototype.hasOwnProperty.call(window.__net, fn.name)) window.__net[fn.name]--;
        return origRemove(type, fn, opts);
      };
    }
    patchNet(document);
    patchNet(window);
  }, HANDLER_NAMES);
  await page.goto(`${BASE}/#/nodes`, { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#app', { timeout: 10000 });
  return page;
}
const net = (page) => page.evaluate(() => window.__net);

async function gotoLive(page) {
  await page.goto(`${BASE}/#/live`, { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('.live-page', { timeout: 10000 });
  await page.waitForTimeout(300); // let the async init() settle past its awaits
}
async function gotoNodes(page) {
  await page.goto(`${BASE}/#/nodes`, { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#nodesTable', { timeout: 10000 });
}

(async () => {
  browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #428 live listener-leak E2E against ${BASE} ===`);

  await test('on Live, all 8 document/window handlers are live (net 1 each) while mounted', async () => {
    const page = await freshPage();
    await gotoLive(page);
    const n = await net(page);
    for (const name of HANDLER_NAMES) assert(n[name] === 1, name + ' net while mounted: ' + n[name]);
    assert(!page.__errors, 'page errors: ' + JSON.stringify(page.__errors));
    await page.context().close();
  });

  await test('leaving Live removes all 8 document/window listeners (net back to 0)', async () => {
    const page = await freshPage();
    await gotoLive(page);
    await gotoNodes(page);
    const n = await net(page);
    for (const name of HANDLER_NAMES) assert(n[name] === 0, name + ' net after leaving: ' + n[name]);
    await page.context().close();
  });

  await test('visiting Live 3x does not stack any of the 8 listeners', async () => {
    const page = await freshPage();
    for (let i = 0; i < 3; i++) {
      await gotoLive(page);
      await gotoNodes(page);
    }
    const n = await net(page);
    for (const name of HANDLER_NAMES) assert(n[name] === 0, name + ' accumulated over 3 visits: ' + n[name]);
    assert(!page.__errors, 'page errors: ' + JSON.stringify(page.__errors));
    await page.context().close();
  });

  await test('orientationchange specifically: the real listener is removed (ref-mismatch regression)', async () => {
    // The pre-#428 bug: destroy() removed `_onResize` under the
    // 'orientationchange' name, which was never the listener actually bound
    // there (an anonymous arrow) — so this one leaked even though destroy()
    // "looked" like it handled it. Isolate it directly.
    const page = await freshPage();
    await gotoLive(page);
    await gotoNodes(page);
    const n = await net(page);
    assert(n._onOrientationChange === 0, '_onOrientationChange net after leaving: ' + n._onOrientationChange);
    await page.context().close();
  });

  await test('entering and leaving Live 3x quickly (race) does not leak or throw', async () => {
    // Live's init() is async and can still be resuming from an `await` after
    // the router has already moved on. Don't wait for the page to settle —
    // leave immediately, three times — then land on a third page and check
    // for leaks/errors once things quiesce.
    const page = await freshPage();
    for (let i = 0; i < 3; i++) {
      await page.goto(`${BASE}/#/live`, { waitUntil: 'domcontentloaded' });
      await page.goto(`${BASE}/#/nodes`, { waitUntil: 'domcontentloaded' });
    }
    await page.goto(`${BASE}/#/observers`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#obsTable', { timeout: 10000 });
    await page.waitForTimeout(500); // let any in-flight suspended init() resolve
    const n = await net(page);
    for (const name of HANDLER_NAMES) assert(n[name] === 0, name + ' leaked after rapid enter/leave: ' + n[name]);
    assert(!page.__errors, 'page errors during rapid enter/leave: ' + JSON.stringify(page.__errors));
    await page.context().close();
  });

  await test('after the rapid enter/leave race, a normal Live visit still works and binds the WS beacon', async () => {
    // Confirms the race test above didn't regress Live's real init/WS setup
    // (the issue's explicit "do not regress" requirement) — a full,
    // unhurried visit right after the rapid cycling must still mount cleanly.
    const page = await freshPage();
    for (let i = 0; i < 3; i++) {
      await page.goto(`${BASE}/#/live`, { waitUntil: 'domcontentloaded' });
      await page.goto(`${BASE}/#/nodes`, { waitUntil: 'domcontentloaded' });
    }
    await gotoLive(page);
    const beaconExists = await page.evaluate(() => !!document.querySelector('.live-beacon'));
    assert(beaconExists, 'live-beacon element missing after rapid cycling + a clean visit');
    const n = await net(page);
    for (const name of HANDLER_NAMES) assert(n[name] === 1, name + ' net on the settled visit: ' + n[name]);
    assert(!page.__errors, 'page errors: ' + JSON.stringify(page.__errors));
    await page.context().close();
  });

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed === 0 ? 0 : 1);
})().catch((e) => { console.error(e); process.exit(1); });
