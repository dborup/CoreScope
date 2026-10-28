#!/usr/bin/env node
/* #634 / upstream 2333: refitting an open More menu (resize) rebuilds its
 * clones; the focused destination must keep focus whether it stays in More or
 * moves inline, an empty menu closes, and focus on other controls is left
 * alone. */
'use strict';
const assert = require('assert');
const { chromium } = require('playwright');
const BASE = process.env.BASE_URL || 'http://localhost:13581';

async function main() {
  const browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'] });
  let passes = 0;
  try {
    for (const theme of ['light', 'dark']) {
      const ctx = await browser.newContext({ viewport: { width: 1250, height: 800 } });
      await ctx.addInitScript(t => { try { localStorage.setItem('meshcore-theme', t); } catch (e) {} }, theme);
      const page = await ctx.newPage();
      const errors = [];
      page.on('pageerror', e => errors.push(e.message));
      await page.goto(BASE + '/#/home', { waitUntil: 'networkidle' });
      const resize = async width => {
        await page.setViewportSize({ width, height: 800 });
        await page.evaluate(() => new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r))));
      };
      const activeHref = () => page.evaluate(() => document.activeElement && document.activeElement.getAttribute('href'));

      await resize(1250);
      assert.ok(await page.locator('#navMoreBtn').isVisible(), 'More must be present at 1250px');
      await page.locator('#navMoreBtn').click();
      const first = await page.locator('#navMoreMenu .nav-link').first().getAttribute('href');
      assert.strictEqual(await activeHref(), first, theme + ': opening More focuses the first item');
      await resize(1249);
      assert.strictEqual(await activeHref(), first, theme + ': focused destination kept after refit');
      assert.strictEqual(await page.evaluate(() => document.getElementById('navMoreMenu').contains(document.activeElement)), true);
      assert.strictEqual(await page.locator('#navMoreBtn').getAttribute('aria-expanded'), 'true');
      passes++;

      await resize(2560);
      assert.strictEqual(await page.evaluate(h => document.activeElement ===
        [...document.querySelectorAll('.nav-links .nav-link')].find(l => l.getAttribute('href') === h), first), true,
      theme + ': focus follows a destination that moves inline');
      assert.strictEqual(await page.locator('#navMoreMenu .nav-link').count(), 0);
      assert.strictEqual(await page.locator('#navMoreMenu').evaluate(m => m.classList.contains('open')), false);
      assert.strictEqual(await page.locator('#navMoreBtn').getAttribute('aria-expanded'), 'false');
      passes++;

      await resize(1250);
      await page.locator('#navMoreBtn').click();
      await page.keyboard.press('Tab');
      const second = await page.locator('#navMoreMenu .nav-link').nth(1).getAttribute('href');
      assert.strictEqual(await activeHref(), second);
      await resize(1249);
      assert.strictEqual(await activeHref(), second, theme + ': later destination kept, not reset to first');
      await page.keyboard.press('Escape');
      passes++;

      await resize(1250);
      await page.locator('#navMoreBtn').click();
      await page.locator('.nav-links .nav-link').first().focus();
      await resize(1249);
      assert.strictEqual(await page.evaluate(() => document.activeElement === document.querySelector('.nav-links .nav-link')), true,
        theme + ': refit must not steal focus from another control');
      passes++;
      assert.deepStrictEqual(errors, []);
      await ctx.close();
    }
  } finally { await browser.close(); }
  console.log('  ✅ ' + passes + ' More-menu focus checks passed (light + dark)');
}
main().catch(e => { console.error('  ❌ ' + (e && e.stack || e)); process.exit(1); });
