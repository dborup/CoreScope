/**
 * E2E (#428): compare.js must not leak its delegated `keydown` handler onto
 * the shared #app container — the remainder of #405 the #419 sweep found
 * but left for the owner, confirmed live on master in #419's review (its
 * leaked keydown is why that PR's E2E used a fresh page per case).
 *
 * public/compare.js init() bound `keydown` (row Enter/Space activation,
 * #209) to the shared `#app` element and never removed it. After one visit
 * to Compare that handler kept firing on every later page, and each revisit
 * stacked another one.
 *
 * The fix binds it to the compare page's OWN container (`.compare-page`),
 * which the next page's `app.innerHTML = …` discards along with its
 * listeners — the same pattern #405/#419 used for observers.js.
 *
 * This runs against a real browser + Go server (the CI fixture). Each case
 * uses its own fresh page so no earlier navigation contaminates it. It
 * asserts:
 *   1. visiting Compare adds NO keydown listener to #app;
 *   2. on-page behaviour still works (row Enter navigates to a packet);
 *   3. after Compare -> Nodes, a sentinel row keydown injected onto #app is
 *      inert (no hash change), proving no compare handler is live on #app
 *      on a later page;
 *   4. visiting Compare three times does not stack listeners on #app.
 *
 * Mutant: rebind the handler back to `app` (the pre-#428 leak) → cases 1, 3, 4 fail.
 *
 * Usage: BASE_URL=http://localhost:13950 node test-issue-428-compare-listener-leak-e2e.js
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
// A fresh, isolated page that lands on Nodes (neutral) with an #app listener
// probe installed, counting live keydown listeners #app itself receives.
// Survives SPA (hash-only) navigation because the document never reloads.
async function freshPage() {
  const ctx = await browser.newContext({ viewport: { width: 1400, height: 900 } });
  const page = await ctx.newPage();
  await page.goto(`${BASE}/#/nodes`, { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#app', { timeout: 10000 });
  await page.evaluate(() => {
    const app = document.getElementById('app');
    window.__appAdds = { keydown: 0 };
    const origAdd = app.addEventListener.bind(app);
    app.addEventListener = function (type, fn, opts) {
      if (type === 'keydown') window.__appAdds[type]++;
      return origAdd(type, fn, opts);
    };
  });
  return page;
}
const appAdds = (page) => page.evaluate(() => window.__appAdds);

async function pickTwoObserverIds(page) {
  await page.goto(`${BASE}/#/observers`, { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#obsTable tbody tr[data-value]', { timeout: 15000 });
  const ids = await page.$$eval('#obsTable tbody tr[data-value]', (rows) =>
    rows.slice(0, 2).map((r) => decodeURIComponent((r.getAttribute('data-value') || '').replace('#/observers/', '')))
  );
  assert(ids.length === 2, 'need at least 2 observers in fixture, got ' + ids.length);
  return ids;
}

async function gotoCompare(page, idA, idB) {
  await page.goto(`${BASE}/#/compare?a=${encodeURIComponent(idA)}&b=${encodeURIComponent(idB)}`, { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('.compare-page', { timeout: 10000 });
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
  console.log(`\n=== #428 compare listener-leak E2E against ${BASE} ===`);

  const bootstrap = await browser.newContext({ viewport: { width: 1400, height: 900 } });
  const bp = await bootstrap.newPage();
  const [idA, idB] = await pickTwoObserverIds(bp);
  await bootstrap.close();

  await test('visiting Compare adds NO keydown listener to #app', async () => {
    const page = await freshPage();
    await gotoCompare(page, idA, idB);
    const adds = await appAdds(page);
    assert(adds.keydown === 0, '#app gained ' + adds.keydown + ' keydown listener(s)');
    await page.context().close();
  });

  await test('on Compare, Enter on a packet row still navigates', async () => {
    const page = await freshPage();
    await gotoCompare(page, idA, idB);
    await page.waitForSelector('[data-cview="onlyA"]', { timeout: 10000 });
    await page.click('[data-cview="onlyA"]'); // switch from Summary to a table view
    await page.waitForSelector('tr[data-action="navigate"]', { timeout: 10000 });
    const href = await page.$eval('tr[data-action="navigate"]', (tr) => tr.dataset.value);
    await page.$eval('tr[data-action="navigate"]', (tr) => {
      tr.focus();
      tr.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    });
    await page.waitForFunction((v) => location.hash === v, href, { timeout: 5000 });
    const h = await page.evaluate(() => location.hash);
    assert(h === href, 'row Enter navigated to ' + h + ', expected ' + href);
    await page.context().close();
  });

  await test('after Compare -> Nodes, Enter on a sentinel row on #app is inert', async () => {
    const page = await freshPage();
    await gotoCompare(page, idA, idB); // arm the (would-be) leak
    await gotoNodes(page);             // leave
    await page.evaluate(() => {
      const app = document.getElementById('app');
      const tbl = document.createElement('table');
      tbl.innerHTML = '<tbody><tr tabindex="0" data-action="navigate" data-value="#/__cmp428_keydown_leak__"><td>x</td></tr></tbody>';
      app.appendChild(tbl); // child of #app so the keydown bubbles to #app
      location.hash = '#/nodes';
    });
    await page.$eval('#app tr[data-action="navigate"][data-value="#/__cmp428_keydown_leak__"]', (tr) => {
      tr.focus();
      tr.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    });
    await page.waitForTimeout(150);
    const h = await page.evaluate(() => location.hash);
    assert(h !== '#/__cmp428_keydown_leak__', 'a leaked compare keydown handler navigated to ' + h);
    await page.context().close();
  });

  await test('visiting Compare three times does not stack listeners on #app', async () => {
    const page = await freshPage();
    for (let i = 0; i < 3; i++) {
      await gotoCompare(page, idA, idB);
      await gotoNodes(page);
    }
    const adds = await appAdds(page);
    assert(adds.keydown === 0, '#app accumulated ' + adds.keydown + ' keydown listener(s) over 3 visits');
    await page.context().close();
  });

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed === 0 ? 0 : 1);
})().catch((e) => { console.error(e); process.exit(1); });
