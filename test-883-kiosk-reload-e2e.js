/**
 * E2E: the kiosk auto-reload and the in-place remount, through live.js
 * (#883 M3, review of #950 findings 1 and 4, and the M2 theme).
 *
 * The unit tests cover live-kiosk.js and version-notice.js one at a time;
 * these cases load the real page, so the glue in live.js (initKiosk()'s
 * setAutoReload, destroy()'s handover) is what they test. /api/health is
 * stubbed (commit "old", then "new"), the socket is replaced by
 * page.routeWebSocket and the timers run on Playwright's clock.
 *
 *   1. A kiosk page reloads 30–120 s after a new commit, with no banner;
 *      plain Live shows the banner and never reloads.
 *   2. An in-place remount while a reload is pending keeps the reload and
 *      its time (no off/on: no banner, no new random delay), and does not
 *      touch data-theme (no flip to the viewer's theme and back, so the
 *      theme observers do not fire).
 *   3. In-place remounts keep no old page alive (the handed-over theme
 *      restore must not hold the first mount), and leaving kiosk still puts
 *      the viewer's theme back (one data-theme change) and saves nothing.
 *   4. Leaving during the remount gap (the new mount still waits for
 *      /api/nodes, so it has no controller yet) turns the auto-reload off:
 *      the banner shows and the page does not reload.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-883-kiosk-reload-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
if (!['localhost', '127.0.0.1', '[::1]'].includes(new URL(BASE).hostname)) throw new Error('local server only');

const GAP_MS = 31000;          // past version-notice's 30 s floor between checks

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + (e && e.stack || e)); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

async function open(browser, hash) {
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 720 }, reducedMotion: 'reduce' });
  await ctx.routeWebSocket(/.*/, () => {});
  // nodes.hold: /api/nodes answers wait until release() (the remount gap).
  const nodes = { hold: false, held: [] };
  await ctx.route(/\/api\/nodes\?/, (r) => { if (nodes.hold) nodes.held.push(r); else r.continue(); });
  nodes.release = () => { nodes.hold = false; nodes.held.splice(0).forEach((r) => r.continue().catch(() => {})); };
  const health = { commit: 'old', served: [] };
  await ctx.route('**/api/health', (r) => {
    health.served.push(health.commit);
    r.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ version: 'v1', commit: health.commit, buildTime: 't' }) });
  });
  // The viewer's own theme is light; kiosk shows dark unless &theme=.
  await ctx.addInitScript(() => { try { localStorage.setItem('meshcore-theme', 'light'); } catch (_) {} });
  const page = await ctx.newPage();
  const h = { ctx, page, health, nodes, errors: [], navs: 0 };
  page.on('pageerror', (e) => h.errors.push(String(e.message)));
  await page.clock.install();
  await page.goto(BASE + '/' + hash);
  // Each predicate returns a boolean: waitForFunction keeps a handle to its
  // result, and a handle to a controller would keep its mount alive.
  await page.waitForFunction(() => !!(window._liveGetMap && window._liveGetMap() && document.querySelector('.live-page')), null, { timeout: 20000 });
  if (hash.includes('kiosk=1')) await page.waitForFunction(() => !!(window._liveKiosk && window._liveKiosk()), null, { timeout: 20000 });
  // A reload is a new document: 'load' (framenavigated also fires on a
  // hash change).
  page.on('load', () => { h.navs++; });
  // Watch data-theme (what the theme observers watch) and the banner; a
  // fixed random keeps the reload delay at its 120 s maximum.
  await page.evaluate(() => {
    window.__themeChanges = 0;
    window.__bannerSeen = 0;
    new MutationObserver((r) => { window.__themeChanges += r.length; })
      .observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] });
    new MutationObserver(() => { if (document.querySelector('[data-version-notice]')) window.__bannerSeen++; })
      .observe(document.body, { childList: true });
    Math.random = () => 0.999999;
  });
  // A new commit, read on a socket (re)open; resolves once it was served.
  h.newCommit = async () => {
    await page.clock.runFor(GAP_MS);
    health.commit = 'new';
    const n = health.served.length;
    await page.evaluate(() => window.dispatchEvent(new Event('corescope:ws-open')));
    for (let i = 0; i < 100 && !health.served.slice(n).includes('new'); i++) await page.waitForTimeout(50);
    assert(health.served.slice(n).includes('new'), 'no health read after ws-open');
    await page.waitForTimeout(300); // the reading's promise chain
    return page.evaluate(() => Date.now());
  };
  h.state = () => page.evaluate(() => ({
    theme: document.documentElement.getAttribute('data-theme'),
    saved: localStorage.getItem('meshcore-theme'),
    themeChanges: window.__themeChanges,
    bannerSeen: window.__bannerSeen,
    banner: !!document.querySelector('[data-version-notice]'),
    remounts: window.LiveKiosk.remounts(),
  }));
  // The in-place remount: due at once, taken on the next heartbeat (a
  // static view remounts there), then held off again.
  h.remount = async () => {
    const before = await page.evaluate(() => { window.__k0 = window._liveKiosk(); window.LiveKiosk._setRemountOverrideMs(1000); return window.LiveKiosk.remounts(); });
    await page.clock.runFor(60000);
    await page.waitForFunction((b) => window.LiveKiosk.remounts() === b + 1 && !!window._liveKiosk() && window._liveKiosk() !== window.__k0,
      before, { timeout: 20000 });
    await page.evaluate(() => { window.__k0 = null; window.LiveKiosk._setRemountOverrideMs(null); });
  };
  // After GC: .live-page elements no longer in the document (a closure
  // that outlives its mount keeps that mount's whole page alive).
  h.detachedPages = async () => {
    const cdp = await ctx.newCDPSession(page);
    await cdp.send('HeapProfiler.collectGarbage');
    const { result: proto } = await cdp.send('Runtime.evaluate', { expression: 'HTMLDivElement.prototype' });
    const { objects } = await cdp.send('Runtime.queryObjects', { prototypeObjectId: proto.objectId });
    const { result } = await cdp.send('Runtime.callFunctionOn', { objectId: objects.objectId, returnByValue: true,
      functionDeclaration: 'function () { return this.filter(function (d) { return d.classList.contains("live-page") && !d.isConnected; }).length; }' });
    await cdp.send('Runtime.releaseObject', { objectId: objects.objectId });
    await cdp.detach();
    return result.value;
  };
  return h;
}

(async () => {
  const browser = await chromium.launch({ args: ['--no-sandbox'] });
  console.log('\n=== #883 M3 kiosk auto-reload and in-place remount E2E ===');
  try {
    await step('1 a kiosk page reloads 30–120 s after a new commit, with no banner; plain Live shows the banner', async () => {
      const h = await open(browser, '#/live?kiosk=1');
      const t = await h.newCommit();
      let s = await h.state();
      assert(!s.banner && s.bannerSeen === 0, 'kiosk showed the banner (live.js did not turn auto-reload on)');
      await h.page.clock.runFor(t + 120000 - 1000 - await h.page.evaluate(() => Date.now()));
      assert(h.navs === 0, 'reloaded before the delay');
      const nav = h.page.waitForEvent('load', { timeout: 10000 }).catch(() => null);
      await h.page.clock.runFor(2000);
      await nav;
      assert(h.navs === 1, 'no reload at 120 s: ' + h.navs);
      assert(!h.errors.length, h.errors.join('; '));
      await h.ctx.close();

      const p = await open(browser, '#/live');
      await p.newCommit();
      s = await p.state();
      assert(s.banner, 'plain Live: no banner');
      await p.page.clock.runFor(150000);
      assert(p.navs === 0, 'plain Live reloaded by itself');
      assert(!p.errors.length, p.errors.join('; '));
      await p.ctx.close();
    });

    await step('2 a remount with a reload pending keeps the reload and its time; no banner, no data-theme change', async () => {
      const h = await open(browser, '#/live?kiosk=1');
      let s = await h.state();
      assert(s.theme === 'dark' && s.saved === 'light', 'kiosk theme ' + JSON.stringify(s));
      const t = await h.newCommit();
      await h.remount();
      s = await h.state();
      assert(s.remounts === 1, 'remounts ' + s.remounts);
      assert(s.bannerSeen === 0 && !s.banner, 'the remount flashed the banner');
      assert(s.themeChanges === 0, 'data-theme changed ' + s.themeChanges + ' times across the remount');
      assert(s.theme === 'dark', 'theme ' + s.theme);
      // The reload set before the remount (t + 120 s), not a new one.
      await h.page.clock.runFor(t + 120000 - 1000 - await h.page.evaluate(() => Date.now()));
      assert(h.navs === 0, 'reloaded early');
      const nav = h.page.waitForEvent('load', { timeout: 10000 }).catch(() => null);
      await h.page.clock.runFor(2000);
      await nav;
      assert(h.navs === 1, 'the pending reload did not keep its time (navs ' + h.navs + ')');
      assert(!h.errors.length, h.errors.join('; '));
      await h.ctx.close();
    });

    await step('3 after in-place remounts no old page is kept alive, and leaving kiosk puts the viewer\'s theme back once and saves nothing', async () => {
      const h = await open(browser, '#/live?kiosk=1');
      await h.remount();
      await h.remount();
      let s = await h.state();
      assert(s.remounts === 2 && s.themeChanges === 0 && s.theme === 'dark', JSON.stringify(s));
      // The handed-over theme restore must not keep the first mount alive.
      await h.page.clock.runFor(20000);     // the last mount's own timers settle
      const detached = await h.detachedPages();
      assert(detached === 0, detached + ' detached .live-page element(s) alive after 2 in-place remounts');
      await h.page.keyboard.press('Escape');
      await h.page.waitForFunction(() => !document.body.classList.contains('kiosk') && !!window._liveKiosk && !window._liveKiosk() &&
        !!(window._liveGetMap && window._liveGetMap()), null, { timeout: 20000 });
      s = await h.state();
      assert(s.theme === 'light', 'theme after exit ' + s.theme);
      assert(s.themeChanges === 1, 'data-theme changes ' + s.themeChanges);
      assert(s.saved === 'light', 'saved ' + s.saved);
      assert(!h.errors.length, h.errors.join('; '));
      await h.ctx.close();
    });

    await step('4 leaving kiosk in the remount gap (no controller yet) turns auto-reload off: banner, no reload', async () => {
      const h = await open(browser, '#/live?kiosk=1');
      await h.newCommit();
      h.nodes.hold = true;
      await h.page.evaluate(() => { window.__k0 = window._liveKiosk(); window.LiveKiosk._setRemountOverrideMs(1000); });
      await h.page.clock.runFor(60000);
      await h.page.waitForFunction(() => window.LiveKiosk.remounts() === 1, null, { timeout: 20000 });
      for (let i = 0; i < 100 && !h.nodes.held.length; i++) await h.page.waitForTimeout(50);
      assert(h.nodes.held.length > 0, 'the new mount did not ask for /api/nodes');
      let s = await h.page.evaluate(() => ({ kiosk: !!window._liveKiosk(), banner: !!document.querySelector('[data-version-notice]') }));
      assert(!s.kiosk && !s.banner, 'in the gap: ' + JSON.stringify(s));
      await h.page.evaluate(() => { window.__k0 = null; window.LiveKiosk._setRemountOverrideMs(null); location.hash = '#/packets'; });
      await h.page.waitForFunction(() => !document.querySelector('.live-page'), null, { timeout: 20000 });
      h.nodes.release();
      s = await h.state();
      assert(s.banner, 'no banner after leaving kiosk in the gap');
      assert(s.theme === 'light', 'theme ' + s.theme);
      await h.page.clock.runFor(150000);
      assert(h.navs === 0, 'reloaded after leaving kiosk (auto-reload left on)');
      assert(!h.errors.length, h.errors.join('; '));
      await h.ctx.close();
    });
  } finally {
    await browser.close();
  }
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
