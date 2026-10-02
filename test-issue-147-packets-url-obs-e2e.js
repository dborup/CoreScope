/**
 * E2E (#147): a packet-detail deep link #/packets/<hash>?obs=<id> keeps its
 * selected observation in the address bar on cold load, on filter changes
 * and on Clear Filters, and reopening the copied URL in a new tab selects the
 * same observation. ?viewPath=1 is kept while the View Path modal is open and
 * dropped when it closes.
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

async function stubDetail(context, hash) {
  await context.route((url) => url.pathname === '/api/packets/' + hash, async (route) => {
    const upstream = await context.request.fetch(route.request());
    const data = await upstream.json();
    const base = (data.observations && data.observations[0]) || {};
    const extra = Object.assign({}, base, { id: Number(SYN_OBS), observer_name: 'e2e-147-observer', snr: -3.5, rssi: -111 });
    data.observations = (data.observations || []).concat([extra]);
    await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(data) });
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

      await step('desktop: Clear Filters removes ?timeWindow=, keeps the detail and ?obs=', async () => {
        await page.click('#clearFiltersBtn');
        await waitHash(page, () => !/timeWindow=/.test(location.hash), 'timeWindow still in URL after Clear');
        const { q, hash: h } = await hashParams(page);
        assert(h.startsWith('#/packets/' + hash), 'detail subpath lost: ' + h);
        assert(q.obs === SYN_OBS, 'obs dropped by Clear Filters: ' + h);
      });

      await step('desktop: ?viewPath=1 is kept while the modal is open and dropped when it closes', async () => {
        await page.goto(`${DETAIL}?obs=${SYN_OBS}&viewPath=1`, { waitUntil: 'load' });
        await page.waitForSelector('#packetPathModal');
        await currentObs(page);
        let p = await hashParams(page);
        assert(p.q.viewPath === '1' && p.q.obs === SYN_OBS, 'cold load with modal open: ' + p.hash);
        await page.keyboard.press('Escape');
        await waitHash(page, () => !/viewPath=/.test(location.hash), 'viewPath still in URL after closing the modal');
        p = await hashParams(page);
        assert(p.q.obs === SYN_OBS, 'obs dropped when the modal closed: ' + p.hash);
      });
    }
    await context.close();
  }

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
