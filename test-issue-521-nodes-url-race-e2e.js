/**
 * E2E (#521): the Nodes page must not rewrite the URL once the user has
 * navigated away.
 *
 * Nodes keeps its filters in the hash (`#/nodes?tab=..&search=..`) and writes
 * them with history.replaceState from the tab bar, the search box (debounced)
 * and the table sort (which also fires once while the page is initialised).
 * A navigation sets location.hash at once, but the router only runs on the
 * hashchange event that follows. A replaceState from Nodes in that gap
 * overwrote the pending hash, so the router read `#/nodes?...` again and the
 * navigation was lost: the "home My Mesh card" E2E in
 * test-issue-76-heard-by-e2e.js never saw the home page and timed out on a
 * request that was never sent.
 *
 * The gap is closed deterministically here: one synchronous task sets the new
 * hash and then triggers a Nodes URL write, which is exactly the order the
 * flaky run produced (hashchange fires only after the task ends).
 *
 * Usage: BASE_URL=http://localhost:13581 node test-issue-521-nodes-url-race-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  const ctx = await browser.newContext({ viewport: { width: 1400, height: 900 } });
  const page = await ctx.newPage();
  page.setDefaultTimeout(15000);
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));

  console.log('\n=== #521 Nodes URL write vs. pending navigation E2E against ' + BASE + ' ===');

  async function openNodes(hash) {
    await page.goto(BASE + '/#/nodes', { waitUntil: 'domcontentloaded' });
    await page.evaluate((h) => { localStorage.setItem('meshcore-user-level', 'experienced'); if (h) location.hash = h; }, hash || null);
    await page.locator('#nodesBody tr[data-key]').first().waitFor({ state: 'visible' });
  }

  await step('a Nodes URL write in the same task as a navigation does not undo it', async () => {
    await openNodes();
    await page.evaluate(() => {
      location.hash = '#/home';
      // Same task: hashchange has not fired yet, so Nodes is still mounted.
      document.querySelector('#nodeTabs .node-tab[data-tab="repeater"]').click();
    });
    await page.locator('.home-hero').waitFor({ state: 'visible' });
    const hash = await page.evaluate(() => location.hash);
    assert(hash === '#/home', 'hash after navigating home: ' + hash);
  });

  await step('the debounced search write from a page the user left does not pull them back', async () => {
    await openNodes();
    await page.evaluate(() => {
      const input = document.getElementById('nodeSearch');
      input.value = 'abc';
      input.dispatchEvent(new Event('input', { bubbles: true })); // starts the 250 ms debounce
      location.hash = '#/home'; // hashchange runs destroy(), which cannot cancel the debounce
    });
    await page.locator('.home-hero').waitFor({ state: 'visible' });
    await page.waitForTimeout(600); // let the debounce fire on the destroyed page
    const hash = await page.evaluate(() => location.hash);
    assert(hash === '#/home', 'hash after the debounce fired: ' + hash);
  });

  await step('on the Nodes route the tab still writes its filter into the hash', async () => {
    await openNodes();
    await page.click('#nodeTabs .node-tab[data-tab="repeater"]');
    await page.waitForFunction(() => location.hash === '#/nodes?tab=repeater');
  });

  await step('a selected node keeps its subpath when the tab writes the hash', async () => {
    await openNodes();
    const key = await page.locator('#nodesBody tr[data-key]').first().getAttribute('data-key');
    await page.click('#nodesBody tr[data-key="' + key + '"]');
    await page.waitForFunction((k) => location.hash === '#/nodes/' + k, key);
    await page.click('#nodeTabs .node-tab[data-tab="repeater"]');
    await page.waitForFunction((k) => location.hash === '#/nodes/' + k + '?tab=repeater', key);
  });

  await step('no page errors', async () => {
    assert(errors.length === 0, errors.join(' | '));
  });

  await browser.close();
  console.log('\n' + passed + ' passed, ' + failed + ' failed');
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
