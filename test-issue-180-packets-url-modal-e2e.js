/**
 * E2E (#180): packets URL and View Path modal leftovers from #167.
 *
 * - Item 2 (1400 px): Clear Filters on a detail URL #/packets/<hash>?… also
 *   leaves the detail, so the URL is the list and a reload shows the same
 *   unfiltered list (the subpath used to set the hash filter again).
 *
 * - Item 3 (641, 800, 1023 px): the SlideOver's close button is the topmost
 *   element at its centre (it used to sit under the sticky .top-nav), is at
 *   least 48×48, and a real click on it closes the SlideOver.
 *
 * - Item 4 (1400 px): with the View Path modal open, Escape in a layer opened
 *   over it (global search via Ctrl+K, the nav More menu) closes that layer,
 *   not the modal; the next Escape closes the modal.
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

      // The fixture's packets may have aged out of the default 15 min window
      // that Clear restores, so compare what Clear showed with what the
      // reload shows rather than expecting a number of rows.
      const listLoaded = () => page.waitForResponse((r) => /\/api\/packets\?/.test(r.url()), { timeout: 15000 });
      await Promise.all([listLoaded(), page.click('#clearFiltersBtn')]);
      await page.waitForTimeout(500);
      let h = await page.evaluate(() => location.hash);
      assert(h === '#/packets', 'URL after Clear: ' + h);
      const shown = await listCount(page);
      assert(!(await clearShown(page)), 'Clear button still visible after Clear');
      assert(!(await detailPaneOpen(page)), 'detail pane still open after Clear');

      await Promise.all([listLoaded(), page.reload({ waitUntil: 'load' })]);
      await page.waitForTimeout(500);
      h = await page.evaluate(() => location.hash);
      assert(h === '#/packets', 'URL after reload: ' + h);
      const reloaded = await listCount(page);
      assert(reloaded === shown, 'reload shows ' + reloaded + ' packets, Clear showed ' + shown);
      assert(!(await clearShown(page)), 'Clear button back after reload');
      assert(!(await detailPaneOpen(page)), 'detail pane open after reload');
      assert(await page.evaluate(() => document.getElementById('fHash').value === ''), 'hash filter input filled after reload');
    });
    await context.close();
  }

  // ---- Item 4: Escape in a layer over the View Path modal (desktop) ----
  {
    const { context, page } = await newPage({ viewport: { width: 1400, height: 900 } });
    const modalOpen = () => page.evaluate(() => !!document.getElementById('packetPathModal'));
    async function openModal() {
      await fresh(page, `${DETAIL}?viewPath=1`);
      await page.waitForSelector('#packetPathModal');
      await page.waitForSelector('#pktRight [data-view-path]', { timeout: 15000 });
    }
    async function secondEscapeClosesModal() {
      await page.keyboard.press('Escape');
      await page.waitForSelector('#packetPathModal', { state: 'detached', timeout: 5000 })
        .catch(() => { throw new Error('the next Escape did not close the modal'); });
      await waitFor(page, () => !/viewPath=/.test(location.hash), 'viewPath still in URL after the modal closed');
    }

    await step('desktop (1400): Escape in the global search (Ctrl+K) over the View Path modal closes the search, not the modal', async () => {
      await openModal();
      await page.keyboard.press('Control+k');
      await waitFor(page, () => !document.getElementById('searchOverlay').classList.contains('hidden') && document.activeElement && document.activeElement.id === 'searchInput', 'search did not open with focus in its input');
      await page.keyboard.press('Escape');
      await waitFor(page, () => document.getElementById('searchOverlay').classList.contains('hidden'), 'Escape did not close the search');
      assert(await modalOpen(), 'Escape in the search closed the View Path modal underneath');
      const h = await page.evaluate(() => location.hash);
      assert(/[?&]viewPath=1(&|$)/.test(h), 'viewPath dropped although the modal is open: ' + h);
      await secondEscapeClosesModal();
    });

    await step('desktop (1400): Escape in the nav More menu over the View Path modal closes the menu, not the modal', async () => {
      await openModal();
      await page.click('#navMoreBtn');
      await waitFor(page, () => document.getElementById('navMoreMenu').classList.contains('open') && document.getElementById('navMoreMenu').contains(document.activeElement), 'More menu did not open with focus in it');
      await page.keyboard.press('Escape');
      await waitFor(page, () => !document.getElementById('navMoreMenu').classList.contains('open'), 'Escape did not close the More menu');
      assert(await modalOpen(), 'Escape in the More menu closed the View Path modal underneath');
      await secondEscapeClosesModal();
    });
    await context.close();
  }

  // ---- Item 3: SlideOver × above the top nav (641–1023 px) ----
  for (const width of [641, 800, 1023]) {
    const { context, page } = await newPage({ viewport: { width, height: 900 } });
    await step(`tablet (${width}): the SlideOver × is topmost, 48×48, and a real click closes it`, async () => {
      await fresh(page, DETAIL);
      await page.waitForSelector('.slide-over-panel [data-view-path]', { timeout: 15000 });
      // Past the 200 ms slideInRight animation.
      await page.waitForTimeout(400);
      const x = await page.evaluate(() => {
        const btn = document.querySelector('.slide-over-panel .slide-over-close');
        const r = btn.getBoundingClientRect();
        const top = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2);
        return { w: r.width, h: r.height, onTop: btn.contains(top), top: top ? (top.className && top.className.baseVal !== undefined ? top.className.baseVal : top.className) || top.tagName : null };
      });
      assert(x.onTop, 'the element at the × centre is "' + x.top + '", not the close button');
      assert(x.w >= 48 && x.h >= 48, 'close button is ' + x.w + '×' + x.h + ', expected at least 48×48');
      await page.click('.slide-over-panel .slide-over-close', { timeout: 3000 });
      await waitFor(page, () => !window.SlideOver.isOpen(), 'the SlideOver is still open after a click on ×');
      const h = await page.evaluate(() => location.hash);
      assert(h.startsWith('#/packets') && !h.startsWith('#/packets/'), 'URL after closing the SlideOver: ' + h);
    });
    await context.close();
  }

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
