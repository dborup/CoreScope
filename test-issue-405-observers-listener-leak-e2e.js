/**
 * E2E (#405): observers.js must not leak its delegated click/keydown handlers
 * onto the shared #app container.
 *
 * Before the fix, observers' init() bound its `click` and `keydown` delegation
 * to the shared `#app` element and never removed it. After one visit to
 * Observers those handlers kept firing on every later page, and each revisit
 * stacked another pair.
 *
 * This runs against a real browser + Go server (the CI fixture). Each case uses
 * its OWN fresh page so no earlier navigation contaminates it — in particular
 * the Compare page carries its own pre-existing #app keydown leak, so a case
 * that must isolate the *observers* handler never visits Compare.
 *
 * It asserts:
 *   1. visiting Observers adds NO click/keydown listener to #app (the fix binds
 *      to the page's own .observers-page container);
 *   2. on-page behaviour still works (Compare-observers button navigates; a row
 *      Enter navigates to the detail);
 *   3. after Observers -> Nodes, two OBSERVERS-ONLY triggers injected into #app
 *      — a [data-action="compare-observers"] button (click) and a
 *      tr[data-action="navigate"] sentinel row (keydown Enter) — produce no
 *      observers side effect (no hash change), proving no observers handler is
 *      live on #app on a later page;
 *   4. visiting Observers three times does not stack listeners on #app.
 *
 * Mutant: rebind either handler to `app` (the pre-#405 leak) → cases 1, 3, 4 fail.
 *
 * Usage: BASE_URL=http://localhost:13900 node test-issue-405-observers-listener-leak-e2e.js
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
// probe installed. The probe counts the live click/keydown listeners #app
// itself receives, per type; it survives SPA (hash-only) navigation because the
// document never reloads.
async function freshPage() {
  const ctx = await browser.newContext({ viewport: { width: 1400, height: 900 } });
  const page = await ctx.newPage();
  await page.goto(`${BASE}/#/nodes`, { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#app', { timeout: 10000 });
  await page.evaluate(() => {
    const app = document.getElementById('app');
    window.__appAdds = { click: 0, keydown: 0 };
    const origAdd = app.addEventListener.bind(app);
    app.addEventListener = function (type, fn, opts) {
      if (type === 'click' || type === 'keydown') window.__appAdds[type]++;
      return origAdd(type, fn, opts);
    };
  });
  return page;
}
const appAdds = (page) => page.evaluate(() => window.__appAdds);

async function gotoObservers(page) {
  await page.goto(`${BASE}/#/observers`, { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#obsTable tbody tr[data-action="navigate"]', { timeout: 10000 });
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
  console.log(`\n=== #405 observers listener-leak E2E against ${BASE} ===`);

  await test('visiting Observers adds NO click/keydown listener to #app', async () => {
    const page = await freshPage();
    await gotoObservers(page);
    const adds = await appAdds(page);
    assert(adds.click === 0, '#app gained ' + adds.click + ' click listener(s)');
    assert(adds.keydown === 0, '#app gained ' + adds.keydown + ' keydown listener(s)');
    await page.context().close();
  });

  await test('on Observers, the Compare-observers button still navigates', async () => {
    const page = await freshPage();
    await gotoObservers(page);
    await page.click('[data-action="compare-observers"]');
    await page.waitForFunction(() => location.hash.indexOf('#/compare') === 0, { timeout: 5000 });
    const h = await page.evaluate(() => location.hash);
    assert(h.indexOf('#/compare') === 0, 'hash after compare click: ' + h);
    await page.context().close();
  });

  await test('on Observers, Enter on a row still navigates to the detail', async () => {
    const page = await freshPage();
    await gotoObservers(page);
    const href = await page.$eval('#obsTable tbody tr[data-action="navigate"]', (tr) => tr.getAttribute('data-value'));
    await page.$eval('#obsTable tbody tr[data-action="navigate"]', (tr) => {
      tr.focus();
      tr.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    });
    await page.waitForFunction((v) => location.hash === v, href, { timeout: 5000 });
    const h = await page.evaluate(() => location.hash);
    assert(h === href, 'row Enter navigated to ' + h + ', expected ' + href);
    await page.context().close();
  });

  await test('after Observers -> Nodes, a Compare-observers click on #app is inert', async () => {
    const page = await freshPage();  // never visits Compare → isolates observers
    await gotoObservers(page);       // arm the (would-be) leak
    await gotoNodes(page);           // leave
    await page.evaluate(() => {
      const app = document.getElementById('app');
      const b = document.createElement('button');
      b.id = '__obs405ClickProbe';
      b.setAttribute('data-action', 'compare-observers');
      app.appendChild(b);            // child of #app so the click bubbles to #app
      location.hash = '#/nodes';     // known baseline
    });
    await page.click('#__obs405ClickProbe');
    await page.waitForTimeout(150);
    const h = await page.evaluate(() => location.hash);
    assert(h.indexOf('#/compare') !== 0, 'a leaked observers click handler navigated to ' + h);
    await page.context().close();
  });

  await test('after Observers -> Nodes, Enter on a sentinel row on #app is inert', async () => {
    const page = await freshPage();  // never visits Compare → the only handler that
    await gotoObservers(page);       // could navigate the sentinel is observers'
    await gotoNodes(page);
    await page.evaluate(() => {
      const app = document.getElementById('app');
      const tbl = document.createElement('table');
      tbl.innerHTML = '<tbody><tr tabindex="0" data-action="navigate" data-value="#/__obs405_keydown_leak__"><td>x</td></tr></tbody>';
      app.appendChild(tbl);          // child of #app so the keydown bubbles to #app
      location.hash = '#/nodes';
    });
    await page.$eval('#app tr[data-action="navigate"][data-value="#/__obs405_keydown_leak__"]', (tr) => {
      tr.focus();
      tr.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    });
    await page.waitForTimeout(150);
    const h = await page.evaluate(() => location.hash);
    assert(h !== '#/__obs405_keydown_leak__', 'a leaked observers keydown handler navigated to ' + h);
    await page.context().close();
  });

  await test('visiting Observers three times does not stack listeners on #app', async () => {
    const page = await freshPage();
    for (let i = 0; i < 3; i++) {
      await gotoObservers(page);
      await gotoNodes(page);
    }
    const adds = await appAdds(page);
    assert(adds.click === 0, '#app accumulated ' + adds.click + ' click listener(s) over 3 visits');
    assert(adds.keydown === 0, '#app accumulated ' + adds.keydown + ' keydown listener(s) over 3 visits');
    await page.context().close();
  });

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed === 0 ? 0 : 1);
})().catch((e) => { console.error(e); process.exit(1); });
