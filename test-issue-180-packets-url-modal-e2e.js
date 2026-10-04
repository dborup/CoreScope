/**
 * E2E (#180): packets URL and View Path modal leftovers from #167.
 *
 * - Item 2 (1400 px): Clear Filters on a detail URL #/packets/<hash>?… also
 *   leaves the detail, so the URL is the list and a reload shows the same
 *   unfiltered list (the subpath used to set the hash filter again).
 *
 * Usage: BASE_URL=http://localhost:13581 node test-issue-180-packets-url-modal-e2e.js
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
async function waitFor(page, pred, what, arg) {
  try {
    await page.waitForFunction(pred, arg, { timeout: 8000 });
  } catch (_) {
    throw new Error(what + ' — address bar: ' + (await page.evaluate(() => location.hash)));
  }
}

// Load url as a new document, so nothing from an earlier step carries over
// (goto alone is a same-document hash change).
async function fresh(page, url) {
  await page.goto(url, { waitUntil: 'load' });
  await page.reload({ waitUntil: 'load' });
}

// The "Latest Packets (N)" count, once the list has rendered.
async function listCount(page) {
  await page.waitForSelector('#pktLeft .count', { timeout: 15000 });
  return page.evaluate(() => Number((document.querySelector('#pktLeft .count').textContent.match(/\d+/) || [])[0]));
}
function clearShown(page) {
  return page.evaluate(() => {
    const b = document.getElementById('clearFiltersBtn');
    return !!b && getComputedStyle(b).display !== 'none';
  });
}
function detailPaneOpen(page) {
  return page.evaluate(() => {
    const r = document.getElementById('pktRight');
    return !!r && !r.classList.contains('empty');
  });
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #180 packets URL / View Path modal against ${BASE} ===`);

  const list = await (await fetch(BASE + '/api/packets?limit=50&groupByHash=true')).json();
  const pkt = (list.packets || []).find((p) => p.hash && /^[0-9a-f]+$/.test(p.hash));
  if (!pkt) { console.error('no packet with a hash in the fixture'); process.exit(1); }
  const hash = pkt.hash;
  const DETAIL = `${BASE}/#/packets/${hash}`;
  console.log('  packet ' + hash);

  async function newPage(opts) {
    const context = await browser.newContext(opts);
    await context.addInitScript(() => {
      try { localStorage.removeItem('meshcore-time-window'); localStorage.removeItem('meshcore-observer-filter'); localStorage.removeItem('meshcore-type-filter'); } catch (_) {}
    });
    const page = await context.newPage();
    page.setDefaultTimeout(15000);
    page.on('pageerror', (e) => console.error('[pageerror]', e.message));
    return { context, page };
  }

  // ---- Item 2: Clear Filters on a detail URL (desktop) ----
  {
    const { context, page } = await newPage({ viewport: { width: 1400, height: 900 } });
    await step('desktop (1400): Clear Filters on #/packets/<hash>?… closes the detail, writes #/packets, and a reload shows the same list', async () => {
      await fresh(page, `${DETAIL}?timeWindow=60`);
      await page.waitForSelector('#pktRight [data-view-path]', { timeout: 15000 });
      await waitFor(page, () => /\(1\)/.test(document.querySelector('#pktLeft .count').textContent), 'detail URL did not filter the list to 1 packet');
      assert(await clearShown(page), 'Clear button hidden on the filtered detail URL');

      await page.click('#clearFiltersBtn');
      await waitFor(page, () => location.hash === '#/packets', 'URL after Clear is not #/packets');
      await waitFor(page, () => !/\(1\)/.test(document.querySelector('#pktLeft .count').textContent), 'list still filtered to 1 packet after Clear');
      const shown = await listCount(page);
      assert(shown > 1, 'list after Clear shows ' + shown + ' packets');
      assert(!(await clearShown(page)), 'Clear button still visible after Clear');
      assert(!(await detailPaneOpen(page)), 'detail pane still open after Clear');

      await page.reload({ waitUntil: 'load' });
      await page.waitForSelector('#pktTable tbody tr[data-hash]', { timeout: 15000 });
      const h = await page.evaluate(() => location.hash);
      assert(h === '#/packets', 'URL after reload: ' + h);
      const reloaded = await listCount(page);
      assert(reloaded === shown, 'reload shows ' + reloaded + ' packets, Clear showed ' + shown);
      assert(!(await clearShown(page)), 'Clear button back after reload');
      assert(!(await detailPaneOpen(page)), 'detail pane open after reload');
      assert(await page.evaluate(() => document.getElementById('fHash').value === ''), 'hash filter input filled after reload');
    });
    await context.close();
  }

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
