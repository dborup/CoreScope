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
 *   not the modal; the next Escape closes the modal. Escape with focus on
 *   the modal's own close or copy-link button closes it (#208).
 *
 * - Item 5 (800 and 1400 px): Back from a packet whose View Path modal is
 *   open closes the modal, and Forward onto that history entry (which still
 *   says viewPath=1) does not reopen it; the URL then drops viewPath.
 *
 * - Item 6 (390 px): Escape closes the mobile detail sheet; with the View
 *   Path modal open over it, the first Escape closes only the modal.
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

      const listLoaded = () => page.waitForResponse((r) => /\/api\/packets\?/.test(r.url()), { timeout: 15000 });
      await Promise.all([listLoaded(), page.click('#clearFiltersBtn')]);
      await page.waitForTimeout(500);
      let h = await page.evaluate(() => location.hash);
      // These three assertions are what Clear itself must do, and are the
      // ones a "Clear keeps ?timeWindow= / keeps the detail" mutant breaks:
      // the bare list URL, no detail pane, the saved window back at default.
      assert(h === '#/packets', 'URL after Clear: ' + h);
      assert(!(await clearShown(page)), 'Clear button still visible after Clear');
      assert(!(await detailPaneOpen(page)), 'detail pane still open after Clear');

      // #271: comparing row COUNTS at the default 15-min window across two
      // renders (Clear's in-SPA list, then a full reload) is a race against
      // the fixture's age — the CI fixture ages while the job runs, and a
      // packet can cross the 15-min boundary in the gap between the two
      // fetches (same class of problem #252 fixed for the Details test).
      // Pin a wide window (24h; the widest option, same as #252) for the
      // list-identity check below, and compare the actual set of packet
      // hashes rather than a count, so the comparison cannot depend on how
      // old the fixture is.
      await Promise.all([listLoaded(), page.evaluate(() => {
        const sel = document.getElementById('fTimeWindow');
        sel.value = '1440';
        sel.dispatchEvent(new Event('change', { bubbles: true }));
      })]);
      await page.waitForTimeout(500);
      const hashSet = async () => {
        await page.waitForSelector('#pktLeft .count', { timeout: 15000 });
        return page.evaluate(() => Array.from(document.querySelectorAll('#pktBody > tr:not(.group-child)')).map((tr) => tr.dataset.hash).sort());
      };
      const shown = await hashSet();
      assert(shown.length > 0, 'no rows to compare at the pinned 24h window');

      await Promise.all([listLoaded(), page.reload({ waitUntil: 'load' })]);
      await page.waitForTimeout(500);
      h = await page.evaluate(() => location.hash);
      assert(h === '#/packets?timeWindow=1440', 'URL after reload: ' + h);
      const reloaded = await hashSet();
      assert(JSON.stringify(reloaded) === JSON.stringify(shown), 'reload shows ' + JSON.stringify(reloaded) + ', pinned-window Clear state showed ' + JSON.stringify(shown));
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

    // #208 item 4: the modal's own controls are not a layer above it.
    for (const id of ['packetPathClose', 'packetPathCopyLink']) {
      await step(`desktop (1400): Escape with focus inside the View Path modal (#${id}) closes it`, async () => {
        await openModal();
        await page.focus('#' + id);
        await waitFor(page, (i) => document.activeElement && document.activeElement.id === i, 'focus did not move into the modal', id);
        await page.keyboard.press('Escape');
        await page.waitForSelector('#packetPathModal', { state: 'detached', timeout: 5000 })
          .catch(() => { throw new Error('Escape with focus inside the modal did not close it'); });
        await waitFor(page, () => !/viewPath=/.test(location.hash), 'viewPath still in URL after the modal closed');
      });
    }

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

  // ---- Item 5: Back/Forward and a closed View Path modal ----
  for (const vp of [
    { label: 'tablet (800)', width: 800, btn: '.slide-over-panel [data-view-path]' },
    { label: 'desktop (1400)', width: 1400, btn: '#pktRight [data-view-path]' },
  ]) {
    const { context, page } = await newPage({ viewport: { width: vp.width, height: 900 } });
    await step(`${vp.label}: Back closes the View Path modal; Forward does not reopen it and drops viewPath`, async () => {
      await fresh(page, `${BASE}/#/nodes`);
      await page.evaluate((h) => { location.hash = '#/packets/' + h; }, hash);
      await page.waitForSelector(vp.btn, { timeout: 15000 });
      await page.waitForTimeout(400);
      await page.click(vp.btn);
      await page.waitForSelector('#packetPathModal');
      await waitFor(page, () => /^#\/packets\/[^?]+\?(.*&)?viewPath=1(&|$)/.test(location.hash), 'viewPath=1 not written by the View Path button');

      await page.evaluate(() => history.back());
      await waitFor(page, () => location.hash.startsWith('#/nodes'), 'Back did not go to #/nodes');
      await page.waitForSelector('#packetPathModal', { state: 'detached', timeout: 5000 })
        .catch(() => { throw new Error('the View Path modal stayed open over #/nodes after Back'); });

      await page.evaluate(() => history.forward());
      await waitFor(page, () => location.hash.startsWith('#/packets/'), 'Forward did not return to the packet');
      await page.waitForSelector(vp.btn, { timeout: 15000 });
      await page.waitForTimeout(1000);
      assert(!(await page.evaluate(() => !!document.getElementById('packetPathModal'))), 'Forward reopened the closed View Path modal');
      await waitFor(page, () => !/viewPath=/.test(location.hash), 'viewPath=1 still in the URL with no modal open');
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

  // ---- Item 6: Escape closes the mobile detail sheet (≤640 px) ----
  {
    const { context, page } = await newPage({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });
    const sheetOpen = () => page.evaluate(() => {
      const s = document.getElementById('mobileDetailSheet');
      return !!s && s.classList.contains('open') && getComputedStyle(s).display !== 'none';
    });
    await step('mobile (390): Escape closes the packet detail sheet', async () => {
      await fresh(page, DETAIL);
      await page.waitForSelector('#mobileDetailSheet.open [data-view-path]', { timeout: 15000 });
      assert(await sheetOpen(), 'detail sheet not shown');
      await page.keyboard.press('Escape');
      await waitFor(page, () => !document.getElementById('mobileDetailSheet').classList.contains('open'), 'Escape did not close the detail sheet');
    });
    await step('mobile (390): with View Path open over the sheet, the first Escape closes only the modal, the next the sheet', async () => {
      await fresh(page, DETAIL);
      await page.waitForSelector('#mobileDetailSheet.open [data-view-path]', { timeout: 15000 });
      await page.click('#mobileDetailSheet [data-view-path]');
      await page.waitForSelector('#packetPathModal');
      await page.keyboard.press('Escape');
      await page.waitForSelector('#packetPathModal', { state: 'detached', timeout: 5000 })
        .catch(() => { throw new Error('the first Escape did not close the modal'); });
      assert(await sheetOpen(), 'the first Escape also closed the detail sheet');
      await page.keyboard.press('Escape');
      await waitFor(page, () => !document.getElementById('mobileDetailSheet').classList.contains('open'), 'the second Escape did not close the detail sheet');
    });
    await context.close();
  }

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
