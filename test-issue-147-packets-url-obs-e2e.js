/**
 * E2E (#147): a packet-detail deep link #/packets/<hash>?obs=<id> keeps its
 * selected observation in the address bar on cold load and on filter changes
 * (Clear Filters leaves the detail, #180), and reopening the copied URL in a new tab selects the
 * same observation. ?viewPath=1 is in the URL while the View Path modal is
 * open, also when the detail's View Path button opened it, and is dropped
 * when it closes.
 *
 * #167 round 2:
 * - Escape closes only the top layer: the modal first (dropping viewPath,
 *   keeping the detail and ?obs=), the detail on the next Escape.
 * - An observation-row click and the SlideOver close (641–1023 px) keep the
 *   active filters in the URL.
 * - The standalone page #/packet/<hash> writes #/packet/<hash>?obs=<id> and
 *   reads it back on load, instead of the packets-list URL.
 *
 * The e2e fixture has no packet with two observations reachable through the
 * API, so the packet-detail response is stubbed (context.route, so new tabs
 * see it too) with one extra synthetic observation. That makes the chosen
 * ?obs= distinguishable from the default (first) observation.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-issue-147-packets-url-obs-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const SYN_OBS = '990000147';

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }
function hashParams(page) {
  return page.evaluate(() => {
    const h = location.hash;
    return { hash: h, q: Object.fromEntries(new URLSearchParams(h.split('?')[1] || '')) };
  });
}
async function waitHash(page, pred, what) {
  try {
    await page.waitForFunction(pred, null, { timeout: 8000 });
  } catch (_) {
    throw new Error(what + ' — address bar: ' + (await page.evaluate(() => location.hash)));
  }
}
async function currentObs(page) {
  await page.waitForSelector(`.detail-obs-row[data-obs-id="${SYN_OBS}"]`, { timeout: 15000 });
  // Let init()'s auto-select finish before reading the selection.
  await page.waitForTimeout(500);
  return page.evaluate(() => {
    const r = document.querySelector('.detail-obs-row.observation-current');
    return r ? r.dataset.obsId : null;
  });
}

// Load url as a new document, so no modal or selection from an earlier step
// carries over (goto alone is a same-document hash change).
async function fresh(page, url) {
  await page.goto(url, { waitUntil: 'load' });
  await page.reload({ waitUntil: 'load' });
}

// The other (real) observation's id, from the rendered rows.
function otherObs(page) {
  return page.evaluate((syn) => {
    const r = Array.from(document.querySelectorAll('.detail-obs-row')).find((el) => el.dataset.obsId !== syn);
    return r ? r.dataset.obsId : null;
  }, SYN_OBS);
}
function detailPaneOpen(page) {
  return page.evaluate(() => {
    const r = document.getElementById('pktRight');
    return !!r && !r.classList.contains('empty') && !!r.querySelector('.detail-obs-row');
  });
}

async function stubDetail(context, hash) {
  await context.route((url) => url.pathname === '/api/packets/' + hash, async (route) => {
    try {
      const upstream = await context.request.fetch(route.request());
      const data = await upstream.json();
      const base = (data.observations && data.observations[0]) || {};
      const extra = Object.assign({}, base, { id: Number(SYN_OBS), observer_name: 'e2e-147-observer', snr: -3.5, rssi: -111 });
      data.observations = (data.observations || []).concat([extra]);
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(data) });
    } catch (_) {
      // A refetch still in flight when the context closes; nothing to answer.
      try { await route.abort(); } catch (__) { /* already closed */ }
    }
  });
}

async function setTimeWindow(page, value) {
  // Works on desktop and on the mobile layout (where the bar can be collapsed).
  await page.evaluate((v) => {
    const el = document.getElementById('fTimeWindow');
    el.value = v;
    el.dispatchEvent(new Event('change', { bubbles: true }));
  }, value);
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #147 packets detail URL keeps ?obs= / ?viewPath= against ${BASE} ===`);

  const list = await (await fetch(BASE + '/api/packets?limit=50&groupByHash=true')).json();
  const pkt = (list.packets || []).find((p) => p.hash && /^[0-9a-f]+$/.test(p.hash));
  if (!pkt) { console.error('no packet with a hash in the fixture'); process.exit(1); }
  const hash = pkt.hash;
  const DETAIL = `${BASE}/#/packets/${hash}`;
  console.log('  packet ' + hash + ', synthetic observation ' + SYN_OBS);

  for (const vp of [
    { label: 'desktop', opts: { viewport: { width: 1400, height: 900 } } },
    { label: 'mobile', opts: { viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true } },
  ]) {
    const context = await browser.newContext(vp.opts);
    await context.addInitScript(() => {
      try { localStorage.removeItem('meshcore-time-window'); localStorage.removeItem('meshcore-observer-filter'); } catch (_) {}
    });
    await stubDetail(context, hash);
    const page = await context.newPage();
    page.setDefaultTimeout(15000);
    page.on('pageerror', (e) => console.error('[pageerror]', e.message));

    await step(`${vp.label}: cold load ?obs=<id> selects that observation and keeps ?obs=`, async () => {
      await page.goto(`${DETAIL}?obs=${SYN_OBS}`, { waitUntil: 'load' });
      const cur = await currentObs(page);
      assert(cur === SYN_OBS, 'selected observation is ' + cur + ', expected ' + SYN_OBS);
      const { q, hash: h } = await hashParams(page);
      assert(q.obs === SYN_OBS, 'obs missing after cold load: ' + h);
    });

    await step(`${vp.label}: changing the time window keeps ?obs= and adds ?timeWindow=`, async () => {
      await setTimeWindow(page, '60');
      await waitHash(page, () => /[?&]timeWindow=60(&|$)/.test(location.hash), 'timeWindow=60 not written');
      const { q, hash: h } = await hashParams(page);
      assert(q.obs === SYN_OBS, 'obs dropped by the filter change: ' + h);
    });

    if (vp.label === 'desktop') {
      await step('desktop: copied URL opened in a new tab keeps observation and filter', async () => {
        const url = page.url();
        const tab = await context.newPage();
        await tab.goto(url, { waitUntil: 'load' });
        const cur = await currentObs(tab);
        assert(cur === SYN_OBS, 'new tab selected ' + cur + ', expected ' + SYN_OBS + ' (url ' + url + ')');
        const tw = await tab.evaluate(() => document.getElementById('fTimeWindow').value);
        assert(tw === '60', 'new tab time window ' + tw);
        const { q, hash: h } = await hashParams(tab);
        assert(q.obs === SYN_OBS && q.timeWindow === '60', 'new tab URL ' + h);
        await tab.close();
      });

      // #180 decision: Clear Filters also leaves the detail (its subpath
      // would set the hash filter again on reload), so ?obs= goes with it.
      await step('desktop: Clear Filters removes ?timeWindow= and the detail (subpath, ?obs=) (#180)', async () => {
        await page.click('#clearFiltersBtn');
        await waitHash(page, () => !/timeWindow=/.test(location.hash), 'timeWindow still in URL after Clear');
        const { hash: h } = await hashParams(page);
        assert(h === '#/packets', 'URL after Clear: ' + h);
        assert(!(await detailPaneOpen(page)), 'detail pane still open after Clear');
      });

      await step('desktop: Escape closes only the View Path modal (drops viewPath, keeps detail + obs); a second Escape closes the detail', async () => {
        await page.goto(`${DETAIL}?obs=${SYN_OBS}&viewPath=1`, { waitUntil: 'load' });
        await page.waitForSelector('#packetPathModal');
        await currentObs(page);
        let p = await hashParams(page);
        assert(p.q.viewPath === '1' && p.q.obs === SYN_OBS, 'cold load with modal open: ' + p.hash);
        await page.keyboard.press('Escape');
        await page.waitForSelector('#packetPathModal', { state: 'detached' });
        await waitHash(page, () => !/viewPath=/.test(location.hash), 'viewPath still in URL after closing the modal');
        p = await hashParams(page);
        assert(p.q.obs === SYN_OBS && p.hash.startsWith('#/packets/' + hash), 'detail URL changed when the modal closed: ' + p.hash);
        assert(await detailPaneOpen(page), 'the first Escape also closed the detail pane');
        await page.keyboard.press('Escape');
        await page.waitForFunction(() => document.getElementById('pktRight').classList.contains('empty'), null, { timeout: 5000 })
          .catch(() => { throw new Error('the second Escape did not close the detail pane'); });
        // The URL after closing the detail pane itself is not asserted: a
        // desktop detail close never wrote the URL (out of scope, see #180).
      });

      await step('desktop: View Path button writes ?viewPath=1 next to obs and filters; reload reopens it; closing drops it', async () => {
        await fresh(page, `${DETAIL}?timeWindow=60&obs=${SYN_OBS}`);
        assert(await currentObs(page) === SYN_OBS, 'observation not selected before opening View Path');
        await page.click('#pktRight [data-view-path]');
        await page.waitForSelector('#packetPathModal');
        await waitHash(page, () => /[?&]viewPath=1(&|$)/.test(location.hash), 'viewPath=1 not written by the View Path button');
        let p = await hashParams(page);
        assert(p.hash.startsWith('#/packets/' + hash + '?') && p.q.obs === SYN_OBS && p.q.timeWindow === '60', 'button URL: ' + p.hash);
        await page.reload({ waitUntil: 'load' });
        await page.waitForSelector('#packetPathModal');
        assert(await currentObs(page) === SYN_OBS, 'reload did not restore the observation');
        p = await hashParams(page);
        assert(p.q.viewPath === '1' && p.q.obs === SYN_OBS && p.q.timeWindow === '60', 'after reload: ' + p.hash);
        await page.click('#packetPathClose');
        await page.waitForSelector('#packetPathModal', { state: 'detached' });
        await waitHash(page, () => !/viewPath=/.test(location.hash), 'viewPath still in URL after closing the modal');
        p = await hashParams(page);
        assert(p.q.obs === SYN_OBS && p.q.timeWindow === '60', 'close dropped more than viewPath: ' + p.hash);
      });

      await step('desktop: clicking another observation row writes its ?obs= and keeps the filters', async () => {
        await fresh(page, `${DETAIL}?timeWindow=60&obs=${SYN_OBS}`);
        assert(await currentObs(page) === SYN_OBS, 'observation not selected before the click');
        const other = await otherObs(page);
        assert(other, 'no second observation row');
        await page.click(`#pktRight .detail-obs-row[data-obs-id="${other}"]`);
        await waitHash(page, () => /[?&]obs=/.test(location.hash) && !location.hash.includes('obs=990000147'), 'obs not switched');
        const p = await hashParams(page);
        assert(p.hash.startsWith('#/packets/' + hash + '?'), 'detail subpath lost: ' + p.hash);
        assert(p.q.obs === other && p.q.timeWindow === '60', 'observation click URL: ' + p.hash);
      });

      await step('desktop: standalone #/packet/<hash>?obs=<id> shows that observation; a row click writes #/packet/<hash>?obs=<new>', async () => {
        // A fresh tab, so no observation selected on the packets page leaks in.
        const tab = await context.newPage();
        tab.on('pageerror', (e) => console.error('[pageerror]', e.message));
        await tab.goto(`${BASE}/#/packet/${hash}?obs=${SYN_OBS}`, { waitUntil: 'load' });
        assert(await currentObs(tab) === SYN_OBS, 'standalone page ignored ?obs=');
        const other = await otherObs(tab);
        await tab.click(`.detail-obs-row[data-obs-id="${other}"]`);
        await waitHash(tab, () => !location.hash.includes('obs=990000147'), 'obs not switched');
        const h = await tab.evaluate(() => location.hash);
        assert(h === `#/packet/${hash}?obs=${other}`, 'standalone URL after the click: ' + h);
        assert(await tab.evaluate(() => !document.getElementById('pktLeft')), 'not on the standalone page any more');
        await tab.reload({ waitUntil: 'load' });
        assert(await currentObs(tab) === other, 'reload of the standalone URL lost the observation');
        await tab.close();
      });
    }
    await context.unrouteAll({ behavior: 'ignoreErrors' });
    await context.close();
  }

  // 641–1023 px: the detail opens in a SlideOver.
  {
    const context = await browser.newContext({ viewport: { width: 800, height: 900 } });
    await context.addInitScript(() => {
      try { localStorage.removeItem('meshcore-time-window'); localStorage.removeItem('meshcore-observer-filter'); } catch (_) {}
    });
    await stubDetail(context, hash);
    const page = await context.newPage();
    page.setDefaultTimeout(15000);
    page.on('pageerror', (e) => console.error('[pageerror]', e.message));
    const slideOverOpen = () => page.evaluate(() => !!(window.SlideOver && window.SlideOver.isOpen()));

    await step('tablet (800): closing the SlideOver with a filter writes the list URL and keeps the filter', async () => {
      await page.goto(`${DETAIL}?timeWindow=60&obs=${SYN_OBS}`, { waitUntil: 'load' });
      assert(await currentObs(page) === SYN_OBS, 'observation not selected in the SlideOver');
      assert(await slideOverOpen(), 'detail did not open in a SlideOver');
      // A real click on the backdrop (left of the panel). A click on ×
      // is covered by test-issue-180-packets-url-modal-e2e.js (#180).
      await page.click('.slide-over-backdrop', { position: { x: 40, y: 450 } });
      await waitHash(page, () => !location.hash.startsWith('#/packets/'), 'detail subpath still in URL after the SlideOver closed');
      const p = await hashParams(page);
      // The list is still filtered to the packet's hash (from the subpath),
      // so the list URL carries it as ?hash=.
      assert(p.hash.startsWith('#/packets?'), 'list URL: ' + p.hash);
      assert(p.q.timeWindow === '60' && p.q.hash === hash && !('obs' in p.q), 'list URL: ' + p.hash);
    });

    await step('tablet (800): Escape closes only the View Path modal; the SlideOver stays until the next Escape', async () => {
      await fresh(page, `${DETAIL}?timeWindow=60&obs=${SYN_OBS}`);
      assert(await currentObs(page) === SYN_OBS, 'observation not selected in the SlideOver');
      await page.click('.slide-over-panel [data-view-path]');
      await page.waitForSelector('#packetPathModal');
      await waitHash(page, () => /[?&]viewPath=1(&|$)/.test(location.hash), 'viewPath=1 not written by the View Path button');
      await page.keyboard.press('Escape');
      await page.waitForSelector('#packetPathModal', { state: 'detached' });
      await waitHash(page, () => !/viewPath=/.test(location.hash), 'viewPath still in URL after closing the modal');
      assert(await slideOverOpen(), 'the first Escape also closed the SlideOver');
      let p = await hashParams(page);
      assert(p.q.obs === SYN_OBS && p.q.timeWindow === '60', 'URL after closing the modal: ' + p.hash);
      await page.keyboard.press('Escape');
      await page.waitForFunction(() => !window.SlideOver.isOpen(), null, { timeout: 5000 })
        .catch(() => { throw new Error('the second Escape did not close the SlideOver'); });
      p = await hashParams(page);
      assert(p.hash.startsWith('#/packets?') && p.q.timeWindow === '60', 'list URL after the SlideOver closed: ' + p.hash);
    });

    await context.unrouteAll({ behavior: 'ignoreErrors' });
    await context.close();
  }

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
