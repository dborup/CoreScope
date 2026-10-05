/**
 * E2E (#96): optional "Hide CONTROL packets" checkbox on the packets page.
 *
 * Against the e2e fixture (4 CONTROL packets, all older than the default
 * time window, so every step opens #/packets?timeWindow=525600):
 * - default: the checkbox is unchecked and CONTROL packets are listed;
 * - checking it hides exactly the CONTROL packets, without a new
 *   /api/packets request, and puts hideControl=1 in the address bar;
 * - the choice survives a reload, checked and explicitly unchecked;
 * - hideControl in the URL wins over the saved choice;
 * - a direct link to one CONTROL packet still opens it while the filter is on;
 * - at a phone width the checkbox is reachable through the Filters toggle.
 * #211 review:
 * - an explicit hideControl=0 over a saved "1" survives the page's own URL
 *   rewrite and a reload;
 * - the empty-list note names CONTROL only when hiding CONTROL emptied it;
 * - a localStorage.setItem that throws does not stop the checkbox.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-issue-96-hide-control-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const LIST = BASE + '/#/packets?timeWindow=525600';
const PREF_KEY = 'meshcore-hide-control';
const CONTROL = 11;

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

// Load url as a new document (goto alone may be a same-document hash change).
async function open(page, url) {
  await page.goto('about:blank');
  await page.goto(url, { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#fHideControl', { state: 'attached', timeout: 15000 });
  await page.waitForSelector('table tbody tr[data-hash]', { timeout: 15000 });
}

// The packet count the list header shows, "(N)".
async function shownCount(page) {
  const txt = await page.textContent('#pktLeft .count');
  const m = /\((\d+)\)/.exec(txt || '');
  if (!m) throw new Error('no count in list header: ' + txt);
  return Number(m[1]);
}

// Load url as a new document and wait for the empty-list row.
async function openEmpty(page, url) {
  await page.goto('about:blank');
  await page.goto(url, { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#fHideControl', { state: 'attached', timeout: 15000 });
  await page.waitForFunction(() => /No packets/.test(document.querySelector('#pktBody td')?.textContent || ''), null, { timeout: 15000 });
  return (await page.textContent('#pktBody td')).trim();
}

async function hashQuery(page) {
  return page.evaluate(() => Object.fromEntries(new URLSearchParams(location.hash.split('?')[1] || '')));
}

(async () => {
  const browser = await chromium.launch({ headless: true, args: ['--no-sandbox'] });
  const context = await browser.newContext({ viewport: { width: 1280, height: 900 } });
  const page = await context.newPage();
  let packetsRequests = 0;
  page.on('request', (r) => { if (/\/api\/packets\?/.test(r.url())) packetsRequests++; });

  // Ground truth from the API: the CONTROL packets of the fixture.
  const api = await (await page.request.get(BASE + '/api/packets?limit=50000&groupByHash=true')).json();
  const all = api.packets || [];
  const controls = all.filter((p) => p.payload_type === CONTROL);
  console.log(`fixture: ${all.length} grouped packets, ${controls.length} CONTROL`);

  let countAll = 0;

  await step('no saved choice: checkbox unchecked, CONTROL packets listed', async () => {
    assert(controls.length > 0, 'the e2e fixture has no CONTROL packet');
    await open(page, LIST);
    assert(!(await page.isChecked('#fHideControl')), 'checkbox is checked by default');
    const box = await page.locator('#fHideControl').boundingBox();
    assert(box && box.width <= 24 && box.height <= 24, 'the checkbox is drawn as a text-input box: ' + JSON.stringify(box));
    countAll = await shownCount(page);
    // Earlier E2E steps in the same CI job can add packets to the server, so
    // the list may hold more than the snapshot above, never fewer.
    assert(countAll >= all.length, `list shows ${countAll}, API had ${all.length}`);
    const q = await hashQuery(page);
    assert(!('hideControl' in q), 'hideControl in the URL by default: ' + JSON.stringify(q));
  });

  await step('checking it hides exactly the CONTROL packets, without a new API request', async () => {
    const before = packetsRequests;
    await page.check('#fHideControl');
    await page.waitForFunction((n) => {
      const m = /\((\d+)\)/.exec(document.querySelector('#pktLeft .count')?.textContent || '');
      return m && Number(m[1]) === n;
    }, countAll - controls.length, { timeout: 5000 });
    assert(packetsRequests === before, `toggling made ${packetsRequests - before} /api/packets request(s)`);
    const q = await hashQuery(page);
    assert(q.hideControl === '1', 'hideControl=1 not in the URL: ' + JSON.stringify(q));
    const shownHashes = await page.$$eval('table tbody tr[data-hash]', (rows) => rows.map((r) => r.dataset.hash));
    for (const c of controls) assert(!shownHashes.includes(c.hash), 'CONTROL packet ' + c.hash + ' still listed');
  });

  await step('the checked choice survives a reload (URL without the param)', async () => {
    await open(page, LIST);
    assert(await page.isChecked('#fHideControl'), 'checkbox not checked after reload');
    assert((await shownCount(page)) === countAll - controls.length, 'CONTROL packets listed after reload');
    const q = await hashQuery(page);
    assert(q.hideControl === '1', 'restored choice not written to the URL: ' + JSON.stringify(q));
  });

  await step('a direct link to a CONTROL packet still opens it while the filter is on', async () => {
    const target = controls[0].hash;
    await page.goto('about:blank');
    await page.goto(BASE + '/#/packets/' + target + '?timeWindow=525600', { waitUntil: 'domcontentloaded' });
    await page.waitForSelector(`table tbody tr[data-hash="${target}"]`, { timeout: 15000 });
    await page.waitForFunction((h) => (document.getElementById('pktRight')?.textContent || '').includes(h), target, { timeout: 15000 });
    assert(await page.evaluate((k) => localStorage.getItem(k), PREF_KEY) === '1', 'the saved choice changed');
  });

  await step('unchecking shows them again, and the unchecked choice survives a reload', async () => {
    await open(page, LIST);
    await page.uncheck('#fHideControl');
    await page.waitForFunction((n) => {
      const m = /\((\d+)\)/.exec(document.querySelector('#pktLeft .count')?.textContent || '');
      return m && Number(m[1]) === n;
    }, countAll, { timeout: 5000 });
    assert(await page.evaluate((k) => localStorage.getItem(k), PREF_KEY) === '0', 'explicit unchecked choice not saved as "0"');
    await open(page, LIST);
    assert(!(await page.isChecked('#fHideControl')), 'checkbox checked after reload');
    assert((await shownCount(page)) === countAll, 'CONTROL packets hidden after reload');
  });

  await step('hideControl=1 in the URL wins over a saved unchecked choice', async () => {
    await open(page, LIST + '&hideControl=1');
    assert(await page.isChecked('#fHideControl'), 'URL hideControl=1 did not check the box');
    assert((await shownCount(page)) === countAll - controls.length, 'CONTROL packets listed with hideControl=1');
  });

  await step('#211: an explicit hideControl=0 over a saved "1" survives the URL rewrite and a reload', async () => {
    await page.evaluate((k) => localStorage.setItem(k, '1'), PREF_KEY);
    await open(page, LIST + '&hideControl=0');
    assert(!(await page.isChecked('#fHideControl')), 'URL hideControl=0 did not uncheck the box');
    assert((await shownCount(page)) === countAll, 'CONTROL packets hidden with hideControl=0');
    // renderLeft() rewrites the URL on load; the override must still be there.
    let q = await hashQuery(page);
    assert(q.hideControl === '0', 'hideControl=0 dropped by the URL rewrite: ' + JSON.stringify(q));
    await page.reload({ waitUntil: 'domcontentloaded' });
    await page.waitForSelector('table tbody tr[data-hash]', { timeout: 15000 });
    assert(!(await page.isChecked('#fHideControl')), 'the saved "1" won after a reload of the same URL');
    assert((await shownCount(page)) === countAll, 'CONTROL packets hidden after a reload of the same URL');
    q = await hashQuery(page);
    assert(q.hideControl === '0', 'hideControl=0 lost after the reload: ' + JSON.stringify(q));
    assert(await page.evaluate((k) => localStorage.getItem(k), PREF_KEY) === '1', 'the URL override changed the saved choice');
  });

  await step('#211: the empty-list note names CONTROL only when hiding CONTROL emptied the list', async () => {
    const onlyControl = await openEmpty(page, LIST + '&hideControl=1&filter=' + encodeURIComponent('type == CONTROL'));
    assert(onlyControl === 'No packets found (CONTROL packets are hidden)', 'expression matching only CONTROL: ' + onlyControl);
    const nothing = await openEmpty(page, LIST + '&hideControl=1&filter=' + encodeURIComponent('snr > 999'));
    assert(nothing === 'No packets found', 'expression matching nothing blamed CONTROL: ' + nothing);
  });

  await step('#211: a localStorage.setItem that throws does not stop the checkbox', async () => {
    const ctx2 = await browser.newContext({ viewport: { width: 1280, height: 900 } });
    await ctx2.addInitScript((k) => {
      const orig = Storage.prototype.setItem;
      Storage.prototype.setItem = function (key, value) {
        if (key === k) throw new DOMException('quota exceeded', 'QuotaExceededError');
        return orig.call(this, key, value);
      };
    }, PREF_KEY);
    const p = await ctx2.newPage();
    const errors = [];
    p.on('pageerror', (e) => errors.push(e.message));
    await open(p, LIST);
    await p.check('#fHideControl');
    await p.waitForFunction((n) => {
      const m = /\((\d+)\)/.exec(document.querySelector('#pktLeft .count')?.textContent || '');
      return m && Number(m[1]) === n;
    }, countAll - controls.length, { timeout: 5000 });
    const q = await hashQuery(p);
    assert(q.hideControl === '1', 'hideControl=1 not in the URL: ' + JSON.stringify(q));
    assert(!errors.some((m) => /quota/i.test(m)), 'the storage error escaped the handler: ' + errors.join(' | '));
    await ctx2.close();
  });

  await step('phone width: the checkbox is reachable through the Filters toggle and fits the screen', async () => {
    const phone = await browser.newContext({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });
    const p = await phone.newPage();
    await p.goto(BASE + '/#/packets', { waitUntil: 'domcontentloaded' });
    await p.waitForSelector('#fHideControl', { state: 'attached', timeout: 15000 });
    if (!(await p.isVisible('#fHideControl'))) {
      // On mobile the in-page Filters button is replaced by a navbar mirror
      // (mobile-page-actions.js); the in-page one is the fallback.
      const toggle = (await p.$('.filter-toggle-btn-mirror')) ? '.filter-toggle-btn-mirror' : '#filterToggleBtn';
      await p.click(toggle);
    }
    await p.waitForSelector('#fHideControl', { state: 'visible', timeout: 5000 });
    const box = await p.locator('label:has(#fHideControl)').boundingBox();
    assert(box && box.x >= 0 && box.x + box.width <= 390, 'checkbox label does not fit the 390 px screen: ' + JSON.stringify(box));
    const input = await p.locator('#fHideControl').boundingBox();
    // The label, not the box, is the 44 px touch target.
    assert(input && input.width <= 24 && input.height <= 24, 'the checkbox is drawn as a text-input box: ' + JSON.stringify(input));
    assert(box.height >= 44, 'the label is not a 44 px touch target: ' + JSON.stringify(box));
    await p.check('#fHideControl');
    assert(await p.evaluate((k) => localStorage.getItem(k), PREF_KEY) === '1', 'checking it at phone width did not save the choice');
    await phone.close();
  });

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed > 0 ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
