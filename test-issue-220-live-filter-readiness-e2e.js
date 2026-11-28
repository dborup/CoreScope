'use strict';
const assert = require('assert');
const { chromium } = require('playwright');
const BASE = process.env.BASE_URL || 'http://localhost:13581';
const KEY = 'a'.repeat(64);

async function hold(page, pattern, json) {
  let arrived;
  let timeout;
  const seen = new Promise((resolve, reject) => {
    timeout = setTimeout(() => reject(new Error('Request not observed: ' + pattern)), 15000);
    arrived = () => { clearTimeout(timeout); resolve(); };
  });
  // Navigation may fail before this promise is awaited; avoid an unhandled
  // rejection while retaining the error for the test's awaited promise.
  seen.catch(() => {});
  let release;
  const gate = new Promise(resolve => { release = resolve; });
  let first = true;
  await page.route(pattern, async route => {
    if (!first) return route.continue();
    first = false;
    arrived();
    const failure = await gate;
    try {
      if (failure) await route.abort();
      else if (json) await route.fulfill({ json });
      else await route.continue();
    } catch (_) { /* a full document navigation may cancel the request */ }
  });
  return { seen, release };
}

(async () => {
  const browser = await chromium.launch({ headless: true,
    ...(process.env.CHROMIUM_PATH ? { executablePath: process.env.CHROMIUM_PATH } : {}) });
  let count = 0;
  try {
    for (const mobile of [false, true]) {
      for (const [pattern, failure] of [
        ['**/api/config/map', false], ['**/api/config/map', true],
        ['**/api/nodes?*', false], ['**/api/nodes?*', true],
      ]) {
        const context = await browser.newContext({ viewport: mobile ? { width: 390, height: 844 } : { width: 1280, height: 900 }, isMobile: mobile, hasTouch: mobile });
        await context.addInitScript(() => {
          localStorage.setItem('live-node-filter', 'saved-node');
          localStorage.setItem('live-controls-expanded', 'true');
          localStorage.setItem('live-header-expanded', 'true');
        });
        const page = await context.newPage();
        const held = await hold(page, pattern);
        await page.route('**/api/nodes/search?*', route => route.fulfill({ json: { nodes: [{ name: 'Test Node', public_key: KEY }] } }));
        await page.goto(BASE + '/#/live?node=url-node', { waitUntil: 'domcontentloaded' });
        await held.seen;
        const input = page.locator('#liveNodeFilterInput');
        assert(await input.isDisabled(), 'input accepts typing before handlers exist');
        assert.strictEqual(await input.getAttribute('aria-busy'), 'true');
        if (process.env.SCREENSHOT_DIR && !mobile && !failure && pattern.includes('config')) {
          await page.screenshot({ path: process.env.SCREENSHOT_DIR + '/live-filter-loading.png' });
        }
        held.release(failure);
        await page.waitForFunction(() => document.getElementById('liveNodeFilterInput')?.disabled === false);
        assert.strictEqual(await input.inputValue(), 'url-node', 'URL filter must override saved filter');
        assert.strictEqual(await input.getAttribute('aria-busy'), 'false');
        // Open controls on narrow layouts using their real disclosure control.
        if (!(await input.isVisible())) await page.locator('#liveHeaderToggle').click();
        await input.fill('Test');
        await page.waitForFunction(() => localStorage.getItem('live-node-filter') === 'Test');
        await page.locator('.live-node-filter-option').waitFor({ state: 'visible' });
        await input.press('ArrowDown');
        await input.press('ArrowUp');
        assert.strictEqual(await input.getAttribute('aria-activedescendant'), 'liveNodeFilterOpt-0');
        await input.press('Escape');
        assert.strictEqual(await input.getAttribute('aria-expanded'), 'false');
        await input.fill('Test again');
        await page.locator('.live-node-filter-option').waitFor({ state: 'visible' });
        await input.press('ArrowDown');
        await input.press('Enter');
        assert.strictEqual(await page.evaluate(() => localStorage.getItem('live-node-filter')), KEY);
        assert(new URLSearchParams(page.url().split('?')[1]).get('node') === KEY);
        await input.fill('blur-check');
        await page.locator('.live-node-filter-option').waitFor({ state: 'visible' });
        await input.press('Tab');
        await page.waitForFunction(() => document.getElementById('liveNodeFilterDropdown').classList.contains('hidden'));
        await page.locator('#liveNodeFilterClear').click();
        assert.strictEqual(await page.evaluate(() => localStorage.getItem('live-node-filter')), '');
        console.log(`PASS delayed ${pattern} ${failure ? 'failure' : 'success'}, URL precedence, typing/keyboard/blur/clear (${mobile ? 'mobile' : 'desktop'})`);
        count++;
        await context.close();
      }
    }
    for (const pattern of ['**/api/config/map', '**/api/nodes?*']) {
      const context = await browser.newContext();
      await context.addInitScript(() => localStorage.setItem('live-node-filter', 'saved-node'));
      const page = await context.newPage();
      const errors = [];
      page.on('pageerror', err => errors.push(err.message));
      const held = await hold(page, pattern);
      await page.goto(BASE + '/#/live', { waitUntil: 'domcontentloaded' });
      await held.seen;
      assert(await page.locator('#liveNodeFilterInput').isDisabled());
      await page.evaluate(() => { location.hash = '#/home'; });
      await page.waitForFunction(() => !document.getElementById('liveNodeFilterInput'));
      await page.evaluate(() => { location.hash = '#/live?node=second-mount'; });
      await page.locator('#liveNodeFilterInput').waitFor({ state: 'attached' });
      held.release(false);
      await page.waitForFunction(() => document.getElementById('liveNodeFilterInput')?.disabled === false);
      // Flush the intercepted old request and its continuation before asserting.
      await page.waitForTimeout(500);
      assert.strictEqual(await page.locator('#liveNodeFilterInput').inputValue(), 'second-mount');
      assert.deepStrictEqual(errors, [], 'departed initialization must not throw or initialize the new map twice');
      await page.evaluate(() => { location.hash = '#/home'; });
      await page.waitForFunction(() => !document.getElementById('liveNodeFilterInput'));
      await page.evaluate(() => { location.hash = '#/live'; });
      await page.waitForFunction(() => document.getElementById('liveNodeFilterInput')?.disabled === false);
      assert.strictEqual(await page.locator('#liveNodeFilterInput').inputValue(), 'second-mount', 'saved filter restored on reentry');
      console.log('PASS stale init ignored at ' + pattern + ', saved filter restored on reentry');
      count++;
      await context.close();
    }
    {
      const context = await browser.newContext();
      await context.addInitScript(() => {
        localStorage.setItem('live-node-filter', 'saved-only');
        localStorage.setItem('live-controls-expanded', 'true');
        localStorage.setItem('live-header-expanded', 'true');
      });
      const page = await context.newPage();
      const delayedSearch = await hold(page, '**/api/nodes/search?*', {
        nodes: [{ name: 'Delayed result', public_key: KEY }],
      });
      await page.goto(BASE + '/#/live', { waitUntil: 'domcontentloaded' });
      await page.waitForFunction(() => document.getElementById('liveNodeFilterInput')?.disabled === false);
      const input = page.locator('#liveNodeFilterInput');
      assert.strictEqual(await input.inputValue(), 'saved-only', 'saved filter must restore without URL override');
      await input.fill('delayed-search');
      await delayedSearch.seen;
      await input.press('Tab');
      delayedSearch.release(false);
      await page.waitForTimeout(300);
      assert.strictEqual(await input.getAttribute('aria-expanded'), 'false', 'late result must not reopen a blurred combobox');
      // Input and route exit in one JS turn ensure its debounce is pending.
      await page.evaluate(() => {
        const input = document.getElementById('liveNodeFilterInput');
        input.value = 'departed-input';
        input.dispatchEvent(new Event('input', { bubbles: true }));
        location.hash = '#/home';
      });
      await page.waitForFunction(() => !document.getElementById('liveNodeFilterInput'));
      await page.evaluate(() => { location.hash = '#/live?node=new-visit'; });
      await page.waitForFunction(() => document.getElementById('liveNodeFilterInput')?.disabled === false);
      await page.waitForTimeout(300);
      assert.strictEqual(await input.inputValue(), 'new-visit');
      assert.strictEqual(await page.evaluate(() => localStorage.getItem('live-node-filter')), 'new-visit');
      assert(page.url().endsWith('node=new-visit'), 'departed debounce must not overwrite the new URL');
      console.log('PASS saved-only restore, late blurred search and departed debounce');
      count++;
      await context.close();
    }
    console.log(`${count} readiness E2E cases passed`);
  } finally { await browser.close(); }
})().catch(err => { console.error(err); process.exitCode = 1; });
