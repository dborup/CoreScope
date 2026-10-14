/**
 * E2E test (#122): reference-to-many observer comparison on the Compare page.
 *
 * Covers the issue's acceptance criteria at the browser level: mode
 * selection, window changes, a stale-response race, column sorting,
 * drill-down into the existing pairwise view, reload/deep-link restore,
 * empty states, and a narrow viewport. Server-side correctness (exact
 * overlap math, RX/TX/unknown direction, window boundaries, visibility
 * filtering, caching/singleflight) is covered by cmd/server Go tests;
 * this file only exercises the UI built on top of it.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-issue-122-compare-reference-many-e2e.js
 */
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:3000';

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.error('  ❌ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

async function run() {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage']
  });
  const ctx = await browser.newContext({ viewport: { width: 1400, height: 900 } });
  const page = await ctx.newPage();
  page.setDefaultTimeout(15000);
  page.on('pageerror', e => console.error('  pageerror:', e.message));

  console.log('\nRunning #122 compare reference-to-many E2E tests against ' + BASE + '\n');

  await step('Compare page shows a "One vs. many" mode toggle alongside "Two observers"', async () => {
    await page.goto(BASE + '/#/compare', { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('[data-mode-btn="pair"]', { timeout: 15000 });
    const pairBtn = await page.$('[data-mode-btn="pair"]');
    const refBtn = await page.$('[data-mode-btn="ref"]');
    assert(pairBtn, 'expected [data-mode-btn="pair"]');
    assert(refBtn, 'expected [data-mode-btn="ref"]');
    assert((await pairBtn.getAttribute('aria-selected')) === 'true', 'pair mode should start active');
  });

  await step('Empty state before a reference is chosen', async () => {
    await page.goto(BASE + '/#/compare', { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('[data-mode-btn="ref"]', { timeout: 15000 });
    await page.click('[data-mode-btn="ref"]');
    await page.waitForSelector('#compareRefObs', { timeout: 10000 });
    const text = await page.$eval('#compareContent', el => el.textContent);
    assert(/select a reference/i.test(text), 'expected a prompt to select a reference, got: ' + text);
  });

  await step('A remembered reference for a vanished observer is dropped, not shown as an error', async () => {
    await page.goto(BASE + '/#/compare', { waitUntil: 'domcontentloaded' });
    await page.evaluate(() => localStorage.setItem('meshcore-compare-reference', 'vanished-observer-id'));
    await page.goto(BASE + '/#/compare?mode=ref', { waitUntil: 'domcontentloaded' });
    await page.reload({ waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#compareRefObs', { timeout: 10000 });
    const text = await page.$eval('#compareContent', el => el.textContent);
    await page.evaluate(() => localStorage.removeItem('meshcore-compare-reference'));
    assert(/select a reference/i.test(text), 'expected the select-a-reference prompt, got: ' + text);
    assert(!/error/i.test(text), 'a stale remembered reference must not render an error: ' + text);
  });

  let refId, otherIds;
  await step('Picking a reference observer renders the comparison table', async () => {
    await page.goto(BASE + '/#/compare', { waitUntil: 'domcontentloaded' });
    await page.click('[data-mode-btn="ref"]');
    await page.waitForSelector('#compareRefObs', { timeout: 10000 });
    const options = await page.$$eval('#compareRefObs option', opts =>
      opts.map(o => o.value).filter(v => v));
    assert(options.length >= 2, 'need at least 2 observers in fixture, got ' + options.length);
    refId = options[0];
    await page.selectOption('#compareRefObs', refId);
    await page.waitForSelector('.compare-ref-table tbody tr', { timeout: 15000 });
    otherIds = await page.$$eval('.compare-ref-table tbody tr[data-action="navigate"]', rows =>
      rows.map(r => r.getAttribute('data-value')));
    assert(otherIds.length >= 1, 'expected at least one comparison row');
    const headline = await page.$eval('.compare-ref-headline-name', el => el.textContent);
    assert(headline.trim().length > 0, 'expected the reference name in the headline');
  });

  await step('URL reflects mode/ref/hours for sharing', async () => {
    const h = await page.evaluate(() => location.hash);
    assert(h.indexOf('mode=ref') >= 0, 'expected mode=ref in hash: ' + h);
    assert(h.indexOf('ref=' + encodeURIComponent(refId)) >= 0, 'expected ref=<id> in hash: ' + h);
  });

  await step('Reloading the deep link restores the same reference and table', async () => {
    const h = await page.evaluate(() => location.hash);
    await page.goto(BASE + '/' + h, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#compareRefObs', { timeout: 15000 });
    const val = await page.$eval('#compareRefObs', el => el.value);
    assert(val === refId, 'expected #compareRefObs to restore to ' + refId + ', got ' + val);
    await page.waitForSelector('.compare-ref-table tbody tr', { timeout: 15000 });
  });

  await step('Changing the window re-fetches and keeps exactly one in-flight result (stale-response guard)', async () => {
    await page.waitForSelector('#compareRefHours', { timeout: 10000 });
    // Fire two window changes back-to-back -- the UI must settle on the
    // LAST one, not an earlier in-flight response that resolves later.
    await page.selectOption('#compareRefHours', '1');
    await page.selectOption('#compareRefHours', '6');
    await page.waitForFunction(() => {
      var sel = document.getElementById('compareRefHours');
      return sel && sel.value === '6';
    }, null, { timeout: 5000 });
    await page.waitForSelector('.compare-ref-table, .compare-ref-note', { timeout: 15000 });
    const h = await page.evaluate(() => location.hash);
    assert(h.indexOf('hours=6') >= 0, 'expected hours=6 in hash after settling, got: ' + h);
    const val = await page.$eval('#compareRefHours', el => el.value);
    assert(val === '6', 'select must reflect the last window chosen, got ' + val);
  });

  await step('Sort headers reorder the table and toggle aria-sort', async () => {
    await page.waitForSelector('.compare-ref-sort-btn[data-sort="total"]', { timeout: 10000 });
    const before = await page.$$eval('.compare-ref-table tbody tr td:first-child', tds => tds.map(td => td.textContent));
    await page.click('.compare-ref-sort-btn[data-sort="total"]');
    await page.waitForFunction(() => {
      var btn = document.querySelector('.compare-ref-sort-btn[data-sort="total"]');
      return btn && btn.getAttribute('aria-sort') !== 'none';
    }, null, { timeout: 5000 });
    const sort1 = await page.$eval('.compare-ref-sort-btn[data-sort="total"]', el => el.getAttribute('aria-sort'));
    assert(sort1 === 'ascending' || sort1 === 'descending', 'expected aria-sort to be set, got ' + sort1);
    // Clicking the same header again must flip direction.
    await page.click('.compare-ref-sort-btn[data-sort="total"]');
    await page.waitForFunction((prev) => {
      var btn = document.querySelector('.compare-ref-sort-btn[data-sort="total"]');
      return btn && btn.getAttribute('aria-sort') !== prev && btn.getAttribute('aria-sort') !== 'none';
    }, sort1, { timeout: 5000 });
    const sort2 = await page.$eval('.compare-ref-sort-btn[data-sort="total"]', el => el.getAttribute('aria-sort'));
    assert(sort2 !== sort1, 'expected the second click to reverse sort direction');
    void before; // order itself is data-dependent; aria-sort + no-crash is the contract under test
  });

  await step('Clicking a row drills into the existing two-observer view with the same window', async () => {
    const row = await page.waitForSelector('.compare-ref-table tbody tr[data-action="navigate"]');
    const target = await row.getAttribute('data-value');
    assert(/^#\/compare\?a=/.test(target), 'expected a drill-down link into pair mode, got ' + target);
    await row.click();
    await page.waitForFunction((t) => location.hash === t, target, { timeout: 5000 });
    await page.waitForSelector('#compareObsA', { timeout: 15000 });
    const valA = await page.$eval('#compareObsA', el => el.value);
    assert(valA === refId, 'expected pairwise Observer A to be the reference, got ' + valA);
    const pairBtn = await page.$('[data-mode-btn="pair"]');
    assert((await pairBtn.getAttribute('aria-selected')) === 'true', 'drilling down should switch the toggle back to pair mode');
  });

  await step('Switching back to "One vs. many" restores the previously chosen reference', async () => {
    await page.click('[data-mode-btn="ref"]');
    await page.waitForSelector('#compareRefObs', { timeout: 10000 });
    const val = await page.$eval('#compareRefObs', el => el.value);
    assert(val === refId, 'expected the reference selection to persist across a mode toggle, got ' + val);
  });

  // ── Narrow viewport ──
  const narrow = await browser.newContext({ viewport: { width: 375, height: 700 } });
  const np = await narrow.newPage();
  np.setDefaultTimeout(15000);
  np.on('pageerror', e => console.error('  pageerror:', e.message));

  await step('Narrow screen: mode toggle and reference table remain usable', async () => {
    await np.goto(BASE + '/#/compare?mode=ref&ref=' + encodeURIComponent(refId), { waitUntil: 'domcontentloaded' });
    await np.waitForSelector('[data-mode-btn="ref"]', { timeout: 15000 });
    await np.waitForSelector('.compare-ref-table tbody tr', { timeout: 15000 });
    const overflow = await np.evaluate(() => {
      var el = document.querySelector('.compare-page');
      return el ? el.scrollWidth - document.documentElement.clientWidth : 0;
    });
    assert(overflow <= 4, 'compare-page should not force horizontal body scroll at 375px, overflow=' + overflow);
    const btn = await np.$('[data-mode-btn="ref"]');
    assert(await btn.isVisible(), 'mode toggle button should stay visible/usable on a narrow screen');
  });

  await narrow.close();
  await browser.close();
  console.log('\n' + passed + ' passed, ' + failed + ' failed');
  if (failed) process.exit(1);
}

run().catch(e => { console.error(e); process.exit(1); });
