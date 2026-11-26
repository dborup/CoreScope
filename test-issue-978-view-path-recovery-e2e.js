/**
 * E2E (#978): the View Path modal recovers when its script or its path
 * request fails, on a 375x560 touch phone from a cold start, in the light
 * and the dark theme.
 *
 * - Item 3: with GET /api/packets/<hash>/path aborted once, tap a packet's
 *   View Path. The modal says the path failed to load and shows a
 *   "Try again" of at least 44x44; tapping it refetches and the map shows.
 *
 * - Item 2: with packet-path-map.js blocked, open a shared
 *   #/packets/<hash>?viewPath=1 link. A notice says the path view could not
 *   be opened, ?viewPath=1 stays in the URL, and its "Try again" (44x44 or
 *   more) reloads the page: while the script is still blocked the notice
 *   comes back, and once it is reachable the reload opens the modal. Close
 *   on the notice drops ?viewPath=1.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-issue-978-view-path-recovery-e2e.js
 * Optional: SCREENSHOT_DIR writes the phone screenshots there.
 */
'use strict';
const path = require('path');
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const SHOTS = process.env.SCREENSHOT_DIR || '';
const PHONE = { viewport: { width: 375, height: 560 }, isMobile: true, hasTouch: true };

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

async function tapTarget(page, sel) {
  const loc = page.locator(sel);
  await loc.waitFor({ state: 'visible', timeout: 10000 });
  const box = await loc.boundingBox();
  assert(box && box.width >= 44 && box.height >= 44, sel + ' is smaller than 44x44: ' + JSON.stringify(box));
  return loc;
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #978 View Path recovery against ${BASE} ===`);

  // A packet whose path has located points, so a retry ends on a drawn map
  // (the first packet with a hash when none has).
  const list = await (await fetch(BASE + '/api/packets?limit=50&groupByHash=true')).json();
  const hashes = (list.packets || []).map((p) => p.hash).filter((h) => h && /^[0-9a-f]+$/.test(h));
  if (!hashes.length) { console.error('no packet with a hash in the fixture'); process.exit(1); }
  let hash = hashes[0];
  let drawn = false;
  for (const h of hashes.slice(0, 20)) {
    const p = await (await fetch(BASE + '/api/packets/' + h + '/path')).json();
    const located = (p.branches || []).reduce((n, b) => n + (b.points || []).filter((pt) => pt.lat != null && pt.lon != null).length, 0);
    if (located > 1) { hash = h; drawn = true; break; }
  }
  console.log('  packet ' + hash + (drawn ? ' (path with located points)' : ''));
  async function mapDrawn(page) {
    if (!drawn) return;
    await page.waitForSelector('#packetPathMapContainer .leaflet-interactive', { timeout: 15000 });
  }

  async function coldPhone(theme) {
    const context = await browser.newContext(PHONE);
    await context.addInitScript((t) => { try { localStorage.setItem('meshcore-theme', t); } catch (_) { /* no storage */ } }, theme);
    const page = await context.newPage();
    return { context, page };
  }
  async function shot(page, name) {
    if (SHOTS) await page.screenshot({ path: path.join(SHOTS, name) });
  }

  for (const theme of ['light', 'dark']) {
    {
      const { context, page } = await coldPhone(theme);
      let pathCalls = 0;
      await page.route('**/api/packets/' + hash + '/path*', (route) => {
        pathCalls++;
        return pathCalls === 1 ? route.abort('failed') : route.continue();
      });
      await step(`375x560 ${theme}: a failed path request offers "Try again", and tapping it shows the path`, async () => {
        await page.goto(`${BASE}/#/packets/${hash}`, { waitUntil: 'load' });
        assert(await page.evaluate((t) => document.documentElement.getAttribute('data-theme') === t, theme), 'theme is not ' + theme);
        const btn = page.locator('#mobileDetailSheet.open [data-view-path]');
        await btn.waitFor({ state: 'visible', timeout: 15000 });
        await btn.tap();
        const retry = await tapTarget(page, '#packetPathModal #packetPathRetry');
        const status = await page.textContent('#packetPathStatus');
        assert(/Failed to load path/.test(status), 'status: ' + status);
        assert(pathCalls === 1, 'path requests before Try again: ' + pathCalls);
        await shot(page, `978-path-failed-375x560-${theme}.png`);
        await retry.tap();
        await page.waitForFunction(() => {
          const s = document.getElementById('packetPathStatus');
          return s && !/Loading|Failed/.test(s.textContent) && !document.getElementById('packetPathRetry');
        }, null, { timeout: 15000 });
        assert(pathCalls === 2, 'Try again did not refetch: ' + pathCalls + ' request(s)');
        assert(await page.isVisible('#packetPathMapContainer'), 'the map is not shown after Try again');
        await mapDrawn(page);
        assert(/viewPath=1/.test(await page.evaluate(() => location.hash)), 'the URL lost ?viewPath=1');
        await shot(page, `978-path-retried-375x560-${theme}.png`);
      });
      await context.close();
    }

    {
      const { context, page } = await coldPhone(theme);
      const blocked = (route) => route.abort('failed');
      await page.route('**/packet-path-map.js*', blocked);
      await step(`375x560 ${theme}: a shared ?viewPath=1 link without its script says so, keeps the link, and "Try again" opens the modal`, async () => {
        await page.goto(`${BASE}/#/packets/${hash}?viewPath=1`, { waitUntil: 'load' });
        assert(await page.evaluate(() => !window.PacketPathMap), 'packet-path-map.js was not blocked');
        let retry = await tapTarget(page, '#packetPathLoadError #packetPathLoadRetry');
        const text = await page.textContent('#packetPathLoadErrorText');
        assert(/could not be opened/.test(text), 'notice: ' + text);
        // On top of the detail sheet that opens under it: tap() would fail
        // on a covered button, and this checks the centre explicitly.
        assert(await page.evaluate(() => {
          const b = document.getElementById('packetPathLoadRetry').getBoundingClientRect();
          const top = document.elementFromPoint(b.left + b.width / 2, b.top + b.height / 2);
          return !!top && top.id === 'packetPathLoadRetry';
        }), 'Try again is covered');
        assert(/[?&]viewPath=1(&|$)/.test(await page.evaluate(() => location.hash)), 'the URL dropped ?viewPath=1');
        assert(!(await page.$('#packetPathModal')), 'no modal without its script');
        await shot(page, `978-script-blocked-375x560-${theme}.png`);

        // Still blocked: Try again reloads the page, and the new document
        // shows the notice again, on the same link.
        await page.evaluate(() => { window.__before978Retry = true; });
        await Promise.all([page.waitForEvent('load'), retry.tap()]);
        assert(await page.evaluate(() => !window.__before978Retry), 'Try again did not reload the page');
        retry = await tapTarget(page, '#packetPathLoadError #packetPathLoadRetry');
        assert(/[?&]viewPath=1(&|$)/.test(await page.evaluate(() => location.hash)), 'the reload dropped ?viewPath=1');

        await page.unroute('**/packet-path-map.js*', blocked);
        await Promise.all([page.waitForEvent('load'), retry.tap()]);
        await page.waitForSelector('#packetPathModal', { timeout: 10000 });
        assert(!(await page.$('#packetPathLoadError')), 'the notice is still shown over the modal');
        assert(/[?&]viewPath=1(&|$)/.test(await page.evaluate(() => location.hash)), 'the URL lost ?viewPath=1 after Try again');
        await page.waitForFunction(() => {
          const s = document.getElementById('packetPathStatus');
          return s && !/Loading|Failed/.test(s.textContent);
        }, null, { timeout: 15000 });
        await mapDrawn(page);
        await shot(page, `978-script-retried-375x560-${theme}.png`);
      });

      await step(`375x560 ${theme}: Close on the notice drops ?viewPath=1`, async () => {
        await page.route('**/packet-path-map.js*', blocked);
        // A new document: the script loaded above is gone again.
        await page.goto(`${BASE}/#/packets/${hash}?viewPath=1`, { waitUntil: 'load' });
        await page.reload({ waitUntil: 'load' });
        await tapTarget(page, '#packetPathLoadError #packetPathLoadRetry');
        await page.locator('#packetPathLoadError .btn-secondary').tap();
        await page.waitForFunction(() => !document.getElementById('packetPathLoadError') && !/viewPath=1/.test(location.hash), null, { timeout: 5000 });
        assert(new RegExp('^#/packets/' + hash).test(await page.evaluate(() => location.hash)), 'Close left the packet');
      });
      await context.close();
    }
  }

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
