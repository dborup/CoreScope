#!/usr/bin/env node
/* Issue #740 M3a — the Danish areas (DAGI) filter on the Nodes page.
 *
 * Against the real Go server (its real GET /api/config/dagi). The fixture
 * has no node inside Denmark, so the phone flow answers the one filtered
 * /api/nodes request (kommune=0751) itself, in the server's shape, from a
 * real fixture node; every other case uses the server unmocked.
 *
 *  1. Phone, 375x560, light and dark, cold start (fresh context, opened on
 *     #/nodes) with real taps only: the trigger reads "Denmark: All"; a tap
 *     opens a bottom sheet inside the viewport (no sideways scroll, 44 px
 *     rows, no autofocus in the search box); a tap on the search box and
 *     "aar" show Aarhus; a tap on it puts kommune=0751 in the URL, labels the
 *     trigger "Kommune: Aarhus", sends kommune=0751, lists the answered node
 *     and shows the hidden-count notice; a tap on ✕ removes the param, sends
 *     no DAGI param and lists the unfiltered nodes again.
 *  2. Real API: #/nodes?kommune=0860 (known, nobody in it) shows the
 *     server's own unpositionedHidden count in the notice, an empty list and
 *     no error.
 *  3. Unknown code: #/nodes?tab=repeater&kommune=9999 sends no request with
 *     9999, leaves #/nodes?tab=repeater, and the trigger reads "Denmark: All".
 *  4. Several DAGI params: the most specific wins, in the URL and the request.
 *  5. Desktop 1440x900: keyboard (type, ArrowDown, Enter) picks, Escape
 *     closes and returns focus, and a Nodes search and a tab change keep the
 *     param in the URL; the popover stays inside the viewport.
 *  6. Timing: the first open (the panel and the Kommune tab's ~100 rows)
 *     and one search (the other ~620 rows) plus the pick, at 4x CPU
 *     throttling, within the budget.
 *  7. Reload: the pick survives a reload with no param (localStorage), and a
 *     link param overrides it.
 *
 * Screenshots for the PR's browser validation go to DAGI_SHOTS_DIR when set.
 * Defaults to localhost:13581 — NEVER point at prod (AGENTS.md). CI sets BASE_URL.
 * Run: BASE_URL=http://localhost:13581 node test-issue-740-m3a-dagi-filter-e2e.js
 */
'use strict';
const path = require('path');
const { chromium } = require('playwright');
const { tapWhenSettled } = require('./tests/e2e-tap');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const SHOTS = process.env.DAGI_SHOTS_DIR || '';
// At 4x CPU throttling. Measured locally: first open ~58 ms (about 48 ms of
// it the browser's style and layout pass), open + search + pick ~100 ms.
// The budgets catch an order-of-magnitude regression (a rebuild per
// keystroke, an O(n^2) match), not CI jitter.
const OPEN_BUDGET_MS = 150;
const SEARCH_BUDGET_MS = 300;

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #740 M3a DAGI filter against ${BASE} ===\n`);
  const errors = [];

  async function cold(viewport, touch, theme) {
    const ctx = await browser.newContext({ viewport, hasTouch: touch, isMobile: touch });
    await ctx.addInitScript((t) => {
      try {
        localStorage.setItem('meshcore-theme', t);
        ['row-swipe', 'tab-swipe', 'edge-drawer', 'pull-refresh']
          .forEach((k) => localStorage.setItem('meshcore-gesture-hints-' + k, 'seen'));
      } catch (_) {}
    }, theme || 'light');
    const page = await ctx.newPage();
    page.setDefaultTimeout(15000);
    page.on('console', (m) => { if (m.type() === 'error' && !/tile|net::ERR|Failed to load resource/i.test(m.text())) errors.push(m.text()); });
    page.on('pageerror', (e) => errors.push(String(e)));
    const nodeReqs = [];
    page.on('request', (r) => { if (/\/api\/nodes\?/.test(r.url())) nodeReqs.push(r.url()); });
    return { ctx, page, nodeReqs };
  }
  const dagiParams = (url) => {
    const q = new URL(url).searchParams;
    return ['kommune', 'landsdel', 'dkregion', 'postnr'].filter((p) => q.has(p)).map((p) => p + '=' + q.get(p));
  };
  async function shot(page, name) { if (SHOTS) await page.screenshot({ path: path.join(SHOTS, name + '.png') }); }
  async function listLoaded(page) {
    await page.waitForFunction(() => { const l = document.getElementById('nodesLeft'); return l && l.getAttribute('data-loaded') === 'true'; });
  }
  const rowNames = (page) => page.evaluate(() => Array.from(document.querySelectorAll('#nodesBody tr[data-key]')).map((tr) => tr.textContent));
  const trigger = (page) => page.evaluate(() => document.querySelector('.dagi-trigger-label').textContent);

  for (const theme of ['light', 'dark']) {
    await step(`375x560 ${theme}, cold start, real taps: pick Aarhus from a search, then clear it`, async () => {
      const { ctx, page, nodeReqs } = await cold({ width: 375, height: 560 }, true, theme);
      // One filtered request answered in the server's shape, from a real
      // fixture node renamed: the fixture has no node in Denmark.
      let filtered = 0;
      await ctx.route(/\/api\/nodes\?.*kommune=0751/, async (route) => {
        filtered++;
        const real = await route.fetch({ url: route.request().url().replace(/&?kommune=0751/, '') });
        const body = await real.json();
        const node = Object.assign({}, body.nodes[0], { name: 'Aarhus DAGI Test Node' });
        await route.fulfill({ json: { nodes: [node], total: 1, counts: body.counts, dagi: { unpositionedHidden: 3 } } });
      });
      await page.goto(BASE + '/#/nodes', { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('.dagi-trigger', { state: 'visible' });
      await listLoaded(page);
      const before = (await rowNames(page)).length;
      assert(before > 1, 'unfiltered list has ' + before + ' rows');
      assert((await trigger(page)) === 'Denmark: All', 'trigger: ' + await trigger(page));
      assert(await page.evaluate(() => document.getElementById('nodesDagiNotice').hidden), 'notice shown without a filter');
      assert(await page.evaluate(() => getComputedStyle(document.querySelector('.dagi-clear')).display === 'none'), '✕ shown with nothing selected');
      await shot(page, `phone-${theme}-1-closed`);

      await tapWhenSettled(page, '.dagi-trigger');
      await page.waitForSelector('.dagi-panel', { state: 'visible' });
      const sheet = await page.evaluate(() => {
        const r = document.querySelector('.dagi-panel').getBoundingClientRect();
        const opt = document.querySelector('.dagi-option:not([hidden])').getBoundingClientRect();
        return { left: r.left, right: r.right, bottom: r.bottom, top: r.top, sw: document.documentElement.scrollWidth,
          pos: getComputedStyle(document.querySelector('.dagi-panel')).position,
          backdrop: getComputedStyle(document.querySelector('.dagi-backdrop')).display,
          rowH: opt.height, focusInSearch: document.activeElement === document.querySelector('.dagi-search') };
      });
      assert(sheet.pos === 'fixed' && sheet.left === 0 && sheet.right === 375 && Math.round(sheet.bottom) === 560 && sheet.top > 0,
        'not a bottom sheet inside the viewport: ' + JSON.stringify(sheet));
      assert(sheet.sw <= 375, 'sideways scroll: ' + sheet.sw);
      assert(sheet.backdrop === 'block', 'no dimmed page behind the sheet');
      assert(sheet.rowH >= 44, 'row height ' + sheet.rowH);
      assert(!sheet.focusInSearch, 'the search box took focus on a touch screen (the keyboard would cover the sheet)');
      await shot(page, `phone-${theme}-2-sheet`);

      await tapWhenSettled(page, '.dagi-search');
      await page.keyboard.type('aar', { delay: 30 });
      // Search mode: the tabs go, and the hits of every layer show.
      await page.waitForFunction(() => document.querySelector('.dagi-tabs').offsetParent === null &&
        Array.from(document.querySelectorAll('.dagi-option')).filter((o) => o.offsetParent).map((o) => o.textContent).join('|') ===
        // The real config: "aar" is a word start in Aarhus and Aars, and in
        // Årslev and Årre once å is spelt aa.
        'Aarhus|5560 Aarup|5792 Årslev|6818 Årre|8000 Aarhus C|8200 Aarhus N|8210 Aarhus V|9600 Aars');
      await shot(page, `phone-${theme}-3-search`);
      const aarhus = await page.evaluateHandle(() => Array.from(document.querySelectorAll('.dagi-option'))
        .find((o) => !o.hidden && o.textContent === 'Aarhus'));
      await aarhus.evaluate((o) => o.setAttribute('data-e2e', 'aarhus'));
      nodeReqs.length = 0;
      await tapWhenSettled(page, '[data-e2e="aarhus"]');
      await page.waitForFunction(() => /kommune=0751/.test(location.hash));
      await page.waitForFunction(() => Array.from(document.querySelectorAll('#nodesBody tr[data-key]')).length === 1);
      assert((await trigger(page)) === 'Kommune: Aarhus', 'trigger: ' + await trigger(page));
      assert(await page.evaluate(() => document.querySelector('.dagi-panel').hidden), 'sheet still open');
      assert(filtered >= 1 && nodeReqs.some((u) => dagiParams(u).join() === 'kommune=0751'), 'no kommune=0751 request: ' + nodeReqs.join(' '));
      assert((await rowNames(page))[0].indexOf('Aarhus DAGI Test Node') >= 0, 'the answered node is not listed');
      const notice = await page.evaluate(() => { const n = document.getElementById('nodesDagiNotice'); return n.hidden ? '' : n.textContent; });
      assert(notice.indexOf('3 nodes have no position') === 0, 'notice: ' + notice);
      await shot(page, `phone-${theme}-4-filtered`);

      nodeReqs.length = 0;
      await tapWhenSettled(page, '.dagi-clear');
      await page.waitForFunction(() => !/kommune=/.test(location.hash));
      await page.waitForFunction((n) => document.querySelectorAll('#nodesBody tr[data-key]').length === n, before);
      assert((await trigger(page)) === 'Denmark: All', 'after ✕: ' + await trigger(page));
      // The unfiltered pages may come from the client cache (no request at
      // all); whatever is requested carries no DAGI param.
      assert(nodeReqs.every((u) => dagiParams(u).length === 0), 'a DAGI param after ✕: ' + nodeReqs.join(' '));
      assert(await page.evaluate(() => document.getElementById('nodesDagiNotice').hidden), 'notice left after ✕');
      assert(await page.evaluate(() => document.documentElement.scrollWidth <= 375), 'sideways scroll after the flow');
      await ctx.close();
    });
  }

  await step('real API: #/nodes?kommune=0860 shows the server\'s hidden count and an empty list', async () => {
    const { ctx, page } = await cold({ width: 1440, height: 900 }, false);
    const res = await page.request.get(BASE + '/api/nodes?limit=1&kommune=0860');
    const want = (await res.json()).dagi.unpositionedHidden;
    assert(want > 0, 'the fixture has no unpositioned node: ' + want);
    await page.goto(BASE + '/#/nodes?kommune=0860', { waitUntil: 'domcontentloaded' });
    await listLoaded(page);
    await page.waitForFunction(() => !document.getElementById('nodesDagiNotice').hidden);
    const notice = await page.evaluate(() => document.getElementById('nodesDagiNotice').textContent);
    assert(notice.indexOf(want + ' nodes have no position') === 0, `notice "${notice}", want the server's ${want}`);
    assert((await rowNames(page)).length === 0, 'rows listed for an empty kommune');
    assert((await trigger(page)) === 'Kommune: Hjørring', 'trigger: ' + await trigger(page));
    assert(await page.evaluate(() => location.hash) === '#/nodes?kommune=0860', 'hash changed');
    await shot(page, 'desktop-light-notice');
    await ctx.close();
  });

  await step('unknown code: #/nodes?tab=repeater&kommune=9999 is dropped, never sent, and the URL is cleaned', async () => {
    const { ctx, page, nodeReqs } = await cold({ width: 1440, height: 900 }, false);
    await page.goto(BASE + '/#/nodes?tab=repeater&kommune=9999', { waitUntil: 'domcontentloaded' });
    await listLoaded(page);
    await page.waitForSelector('.dagi-trigger', { state: 'visible' });
    assert(nodeReqs.length > 0, 'no /api/nodes request seen');
    assert(nodeReqs.every((u) => dagiParams(u).length === 0), 'sent: ' + nodeReqs.filter((u) => dagiParams(u).length).join(' '));
    assert(await page.evaluate(() => location.hash) === '#/nodes?tab=repeater', 'hash: ' + await page.evaluate(() => location.hash));
    assert((await trigger(page)) === 'Denmark: All', 'trigger: ' + await trigger(page));
    assert(await page.evaluate(() => localStorage.getItem('meshcore-dagi-filter')) === null, 'unknown code saved');
    await ctx.close();
  });

  await step('several DAGI params: the most specific wins, in the URL and the request', async () => {
    const { ctx, page, nodeReqs } = await cold({ width: 1440, height: 900 }, false);
    await page.goto(BASE + '/#/nodes?dkregion=1084&kommune=0860&landsdel=218546', { waitUntil: 'domcontentloaded' });
    await listLoaded(page);
    assert(await page.evaluate(() => location.hash) === '#/nodes?kommune=0860', 'hash: ' + await page.evaluate(() => location.hash));
    const sent = nodeReqs.map((u) => dagiParams(u).join('&')).filter(Boolean);
    assert(sent.length > 0 && sent.every((s) => s === 'kommune=0860'), 'sent: ' + sent.join(' '));
    await ctx.close();
  });

  await step('1440x900 desktop: keyboard pick, Escape, and a search or tab change keeps the param', async () => {
    for (const theme of ['light', 'dark']) {
      const { ctx, page } = await cold({ width: 1440, height: 900 }, false, theme);
      await page.goto(BASE + '/#/nodes', { waitUntil: 'domcontentloaded' });
      await listLoaded(page);
      await page.click('.dagi-trigger');
      await page.waitForSelector('.dagi-panel', { state: 'visible' });
      assert(await page.evaluate(() => document.activeElement === document.querySelector('.dagi-search')), 'desktop: search not focused');
      const box = await page.evaluate(() => { const r = document.querySelector('.dagi-panel').getBoundingClientRect(); return { l: r.left, r: r.right, b: r.bottom, pos: getComputedStyle(document.querySelector('.dagi-panel')).position }; });
      assert(box.pos === 'absolute' && box.l >= 0 && box.r <= 1440 && box.b <= 900, 'popover box ' + JSON.stringify(box));
      await shot(page, `desktop-${theme}-popover`);
      await page.keyboard.press('Escape');
      assert(await page.evaluate(() => document.querySelector('.dagi-panel').hidden), 'Escape did not close');
      assert(await page.evaluate(() => document.activeElement === document.querySelector('.dagi-trigger')), 'focus not back on the trigger');
      await page.click('.dagi-trigger');
      await page.keyboard.type('hjør');
      await page.keyboard.press('ArrowDown');
      assert(await page.evaluate(() => document.activeElement.textContent) === 'Hjørring', 'ArrowDown: ' + await page.evaluate(() => document.activeElement.textContent));
      await shot(page, `desktop-${theme}-search`);
      await page.keyboard.press('Enter');
      await page.waitForFunction(() => location.hash === '#/nodes?kommune=0860');
      await listLoaded(page);
      await page.fill('#nodeSearch', 'zz');
      await page.waitForFunction(() => /search=zz/.test(location.hash));
      assert(/kommune=0860/.test(await page.evaluate(() => location.hash)), 'search dropped the param: ' + await page.evaluate(() => location.hash));
      await page.click('.node-tab[data-tab="repeater"]');
      await page.waitForFunction(() => /tab=repeater/.test(location.hash));
      assert(/kommune=0860/.test(await page.evaluate(() => location.hash)), 'tab dropped the param: ' + await page.evaluate(() => location.hash));
      await ctx.close();
    }
  });

  await step(`timing at 4x CPU throttling: the first open under ${OPEN_BUDGET_MS} ms, open + search + pick under ${SEARCH_BUDGET_MS} ms`, async () => {
    const { ctx, page } = await cold({ width: 375, height: 560 }, true);
    await page.goto(BASE + '/#/nodes', { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('.dagi-trigger', { state: 'visible' });
    await listLoaded(page);
    const cdp = await ctx.newCDPSession(page);
    await cdp.send('Emulation.setCPUThrottlingRate', { rate: 4 });
    const ms = await page.evaluate(() => {
      const t0 = performance.now();
      document.querySelector('.dagi-trigger').click(); // builds the panel and the open tab's rows (sync)
      const opened = performance.now() - t0;
      const firstRows = document.querySelectorAll('.dagi-option').length;
      const input = document.querySelector('.dagi-search');
      input.value = 'aar';
      input.dispatchEvent(new Event('input', { bubbles: true }));
      // Enter runs the pending search over every layer (making the other
      // layers' rows) and picks the first hit.
      input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
      return { opened, firstRows, total: performance.now() - t0, rows: document.querySelectorAll('.dagi-option').length, hash: location.hash };
    });
    await cdp.send('Emulation.setCPUThrottlingRate', { rate: 1 });
    console.log(`    (first open ${ms.opened.toFixed(1)} ms with ${ms.firstRows} rows, open + search + pick ${ms.total.toFixed(1)} ms with ${ms.rows} rows, 4x throttled)`);
    assert(ms.firstRows > 90 && ms.firstRows < 120, 'first open made ' + ms.firstRows + ' rows, want only the Kommune tab');
    assert(ms.rows > 700, 'rows after a search: ' + ms.rows);
    assert(ms.hash === '#/nodes?kommune=0751', 'Enter picked ' + ms.hash);
    assert(ms.opened < OPEN_BUDGET_MS, `first open took ${ms.opened.toFixed(1)} ms`);
    assert(ms.total < SEARCH_BUDGET_MS, `open + search + pick took ${ms.total.toFixed(1)} ms`);
    await ctx.close();
  });

  await step('reload: the pick is kept without a param, and a link param overrides it', async () => {
    const { ctx, page } = await cold({ width: 1440, height: 900 }, false);
    await page.goto(BASE + '/#/nodes?dkregion=1084', { waitUntil: 'domcontentloaded' });
    await listLoaded(page);
    await page.goto(BASE + '/#/home', { waitUntil: 'domcontentloaded' });
    await page.goto(BASE + '/#/nodes', { waitUntil: 'domcontentloaded' });
    await listLoaded(page);
    await page.waitForSelector('.dagi-trigger', { state: 'visible' });
    assert((await trigger(page)) === 'Region: Region Hovedstaden', 'after reload: ' + await trigger(page));
    await page.goto(BASE + '/#/nodes?postnr=1051', { waitUntil: 'domcontentloaded' });
    await page.waitForFunction(() => document.querySelector('.dagi-trigger-label') && document.querySelector('.dagi-trigger-label').textContent.indexOf('Postnr') === 0);
    assert((await trigger(page)) === 'Postnr: 1050–1473 København K', 'street postcode: ' + await trigger(page));
    await ctx.close();
  });

  await step('no console errors', async () => { assert(!errors.length, errors.slice(0, 3).join(' | ')); });

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
