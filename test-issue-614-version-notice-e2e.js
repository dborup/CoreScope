#!/usr/bin/env node
/* Issue #614 — "new version available" notice in an already-open tab.
 *
 * Stubs /api/health: first reading commit "old", then "new". Fires
 * visibilitychange and asserts the role=status banner appears, that nothing
 * reloads by itself, that Reload reloads, that Dismiss hides it for good,
 * and that it is visible and readable in light, dark and at 390px.
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const SHOTS = process.env.SHOT_DIR || '';
const NAME = 'test-issue-614-version-notice-e2e.js';
let passed = 0, failed = 0;
function ok(c, m) { if (c) { passed++; console.log('  ✅ ' + m); } else { failed++; console.log('  ❌ ' + m); } }

async function newPage(browser, theme, viewport, healthState) {
  const ctx = await browser.newContext({ viewport });
  await ctx.addInitScript((t) => { try { localStorage.setItem('meshcore-theme', t); } catch (e) {} }, theme);
  const page = await ctx.newPage();
  await page.route('**/api/health', (route) => {
    if (healthState.mode === '503') return route.fulfill({ status: 503, body: 'restarting' });
    route.fulfill({
      status: 200, contentType: 'application/json',
      body: JSON.stringify({ version: 'v1', commit: healthState.commit, buildTime: 't' }),
    });
  });
  await page.goto(BASE + '/#/nodes', { waitUntil: 'load' });
  await page.waitForFunction(() => window.VersionNotice && window.CoreHealth);
  await page.waitForTimeout(300); // baseline read
  return { ctx, page };
}
async function tick(page) {
  await page.evaluate(() => {
    const real = Date.now.bind(Date);
    const off = (window.__vnOff = (window.__vnOff || 0) + 120000);
    Date.now = () => real() + off;
    Object.defineProperty(document, 'hidden', { configurable: true, get: () => false });
    document.dispatchEvent(new Event('visibilitychange'));
  });
}

(async () => {
  let browser;
  try {
    browser = await chromium.launch({
      headless: true, executablePath: process.env.CHROMIUM_PATH || undefined,
      args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
    });
  } catch (err) {
    if (process.env.CHROMIUM_REQUIRE === '1') { console.error(`${NAME}: FAIL — Chromium unavailable: ${err.message}`); process.exit(1); }
    console.log(`${NAME}: SKIP (Chromium unavailable)`); process.exit(0);
  }

  const cases = [
    ['light', { width: 1280, height: 800 }],
    ['dark', { width: 1280, height: 800 }],
    ['light', { width: 390, height: 844 }],
  ];
  for (const [theme, vp] of cases) {
    const label = `${theme} ${vp.width}px`;
    console.log('--- ' + label);
    const st = { commit: 'old', mode: 'ok' };
    const { ctx, page } = await newPage(browser, theme, vp, st);
    let navs = 0;
    page.on('framenavigated', (f) => { if (f === page.mainFrame()) navs++; });
    ok(await page.locator('[data-version-notice]').count() === 0, 'no banner on first load');

    st.mode = '503';
    await tick(page); await page.waitForTimeout(300);
    ok(await page.locator('[data-version-notice]').count() === 0, 'no banner for a 503 during restart');

    st.mode = 'ok'; st.commit = 'new';
    await tick(page);
    const banner = page.locator('[data-version-notice]');
    await banner.waitFor({ state: 'visible', timeout: 5000 }).catch(() => {});
    ok(await banner.count() === 1 && await banner.isVisible(), 'banner visible after a new commit');
    ok(await banner.getAttribute('role') === 'status', 'role=status');
    const box = await banner.boundingBox();
    ok(box && box.x >= 0 && box.x + box.width <= vp.width + 1 && box.y >= 0 && box.y + box.height <= vp.height + 1, 'banner inside the viewport');
    const colors = await banner.evaluate((el) => { const s = getComputedStyle(el); return [s.backgroundColor, s.color]; });
    ok(colors[0] !== 'rgba(0, 0, 0, 0)' && colors[0] !== colors[1], 'banner has a themed background distinct from text: ' + colors.join(' / '));
    if (SHOTS) await page.screenshot({ path: `${SHOTS}/614-${theme}-${vp.width}.png` });

    await tick(page); await page.waitForTimeout(200);
    ok(await page.locator('[data-version-notice]').count() === 1, 'one banner per new commit');
    ok(navs === 0, 'no automatic reload');

    // keyboard: Reload button reachable by Tab and activates with Enter
    await page.locator('[data-version-notice-reload]').focus();
    ok(await page.evaluate(() => document.activeElement && document.activeElement.hasAttribute('data-version-notice-reload')), 'Reload focusable');
    const [ ] = await Promise.all([
      page.waitForEvent('framenavigated', { timeout: 5000 }).catch(() => null),
      page.keyboard.press('Enter'),
    ]);
    await page.waitForLoadState('load');
    ok(navs >= 1, 'Reload reloads the page');
    await ctx.close();
  }

  console.log('--- dismiss');
  const st = { commit: 'old', mode: 'ok' };
  const { ctx, page } = await newPage(browser, 'light', { width: 1280, height: 800 }, st);
  st.commit = 'new'; await tick(page);
  await page.locator('[data-version-notice]').waitFor({ state: 'visible', timeout: 5000 });
  await page.locator('[data-version-notice-dismiss]').click();
  ok(await page.locator('[data-version-notice]').count() === 0, 'Dismiss hides the banner');
  await tick(page); await page.waitForTimeout(300);
  ok(await page.locator('[data-version-notice]').count() === 0, 'banner does not return for the same commit');
  st.commit = 'newer'; await tick(page);
  await page.locator('[data-version-notice]').waitFor({ state: 'visible', timeout: 5000 }).catch(() => {});
  ok(await page.locator('[data-version-notice]').count() === 1, 'a later commit notifies again');
  await ctx.close();

  await browser.close();
  console.log(`\n${NAME}: ${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
