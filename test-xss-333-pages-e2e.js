// test-xss-333-pages-e2e.js
//
// Full-page XSS regression test for issue #333 round 2 — the adjacent sinks
// found in review of PR #373:
//   F1  packets region badge in the flat view and in expanded group children
//   F2  the shared region filter (pills, dropdown items, dropdown trigger)
//   F3  observers-row navigation with an observer id that contains a quote
//   N1  (round 3) the nodes-page detail-panel "Regions:" summary line
//
// Unlike test-xss-333-e2e.js (production sink fragments rendered on a blank
// page), this drives the REAL pages against a running Go server. The hostile
// values are injected by rewriting two API responses in the browser
// (/api/observers, /api/config/regions) with page.route — the fixture DB is
// untouched and no payload leaves the machine. Every injected element would
// run `window.__xss333++`, so the check is "did any handler fire" plus "is
// there any <img src=x> in the DOM", not a string match on the source.
//
// Requires a server with the CI-prepared e2e fixture (see deploy.yml): SJC
// observers exist, and the #1486 seed adds hash fae0c9e6d357a814 with three
// observations (observers OAK, SJC, OAK).
//
// Usage: BASE_URL=http://localhost:13581 node test-xss-333-pages-e2e.js
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const SEED_HASH = 'fae0c9e6d357a814';

// Breaks out of a double-quoted attribute and carries a tag for the text
// context; the trailing quote probes single-quoted contexts.
const HOSTILE = '"><img src=x onerror="window.__xss333=(window.__xss333||0)+1">\'';
// Observer id that breaks out of a single-quoted JS string (F3). Only
// characters encodeURIComponent leaves alone (' ( ) - letters digits), so
// URL-encoding cannot neutralise it; a run shows up as an alert dialog.
const QUOTE_ID = "xss333'-alert(333)-'";
const BENIGN_REGION = 'KØB & Æ';

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

// Rewrite /api/observers (SJC → HOSTILE, optional extra observers) and
// /api/config/regions (whatever `regions` returns for the real response).
async function hostileContext(browser, { regions, extraObservers = [], iataMap = { SJC: HOSTILE } }) {
  const ctx = await browser.newContext({ viewport: { width: 1400, height: 900 } });
  await ctx.route((url) => url.pathname === '/api/observers', async (route) => {
    const res = await route.fetch();
    const body = await res.json();
    for (const o of body.observers || []) if (o.iata in iataMap) o.iata = iataMap[o.iata];
    body.observers = (body.observers || []).concat(extraObservers);
    await route.fulfill({ response: res, json: body });
  });
  await ctx.route((url) => url.pathname === '/api/config/regions', async (route) => {
    const res = await route.fetch();
    await route.fulfill({ response: res, json: regions(await res.json()) });
  });
  const page = await ctx.newPage();
  page.setDefaultTimeout(15000);
  page.on('pageerror', (e) => console.error('    [pageerror]', e.message));
  page._xssDialogs = 0;
  page.on('dialog', (d) => { page._xssDialogs++; d.dismiss().catch(() => {}); });
  return { ctx, page };
}

// Real regions with SJC replaced by the hostile code (4 codes → pill mode).
const hostileRegions = (real) => {
  const out = {};
  for (const k of Object.keys(real)) out[k === 'SJC' ? HOSTILE : k] = k === 'SJC' ? HOSTILE : real[k];
  return out;
};

// Count live injections anywhere in the document.
async function injections(page) {
  const r = await page.evaluate(() => ({
    fired: window.__xss333 || 0,
    imgs: document.querySelectorAll('img[src="x"]').length,
  }));
  r.dialogs = page._xssDialogs;
  return r;
}
function assertInert(r, label) {
  assert(r.fired === 0 && r.imgs === 0 && r.dialogs === 0,
    `${label}: ${r.imgs} injected <img>, ${r.fired} handler call(s), ${r.dialogs} dialog(s)`);
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #333 round 2 full-page XSS E2E against ${BASE} ===`);

  // ---- F1: packets flat view --------------------------------------------
  await step('F1 packets flat view: hostile observer IATA renders as badge text', async () => {
    const { ctx, page } = await hostileContext(browser, { regions: (r) => r });
    try {
      // 24 h window: the fixture is freshened to ~now before the run, and a
      // 15-minute default would empty the list on a slow runner (cf. #180).
      await page.goto(BASE + '/#/packets?timeWindow=1440', { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('#fGroup');
      await page.waitForSelector('#pktBody tr[data-hash]');
      await page.click('#fGroup'); // Group by Hash off → flat rows
      await page.waitForFunction(() => !document.getElementById('fGroup').classList.contains('active'));
      // Rows from SJC observers carry the hostile region; wait until the flat
      // rows have rendered region cells, then judge the DOM.
      await page.waitForFunction(() => document.querySelectorAll(
        '#pktBody tr[data-action="select-hash"] td.col-region').length > 0);
      await page.waitForTimeout(500);
      assertInert(await injections(page), 'flat view');
      const r = await page.evaluate((h) => {
        const rows = Array.from(document.querySelectorAll('#pktBody tr[data-action="select-hash"]'));
        const badges = Array.from(document.querySelectorAll('#pktBody .badge-region')).map((b) => b.textContent);
        return { rows: rows.length, hostile: badges.filter((t) => t === h).length,
          groupHeaders: document.querySelectorAll('#pktBody tr.group-header').length };
      }, HOSTILE);
      assert(r.groupHeaders === 0, 'expected the flat view, found group headers');
      assert(r.hostile > 0, 'no flat row shows the hostile region as text');
    } finally { await ctx.close(); }
  });

  // ---- F1: packets group child rows ---------------------------------------
  await step('F1 packets expanded group: child-row region badges render as text', async () => {
    const { ctx, page } = await hostileContext(browser, { regions: (r) => r });
    try {
      await page.goto(BASE + '/#/packets?hash=' + SEED_HASH + '&timeWindow=0', { waitUntil: 'domcontentloaded' });
      const sel = `#pktBody tr[data-hash="${SEED_HASH}"][data-action="toggle-select"]`;
      await page.waitForSelector(sel);
      await page.click(sel + ' td.col-expand');
      await page.waitForFunction(() => document.querySelectorAll('#pktBody tr.group-child').length === 3);
      const badges = await page.$$eval('#pktBody tr.group-child .badge-region', (els) => els.map((e) => e.textContent));
      assert(JSON.stringify(badges.slice().sort()) === JSON.stringify([HOSTILE, 'OAK', 'OAK'].sort()),
        'child badges must be OAK, SJC→hostile, OAK as text, got ' + JSON.stringify(badges));
      assertInert(await injections(page), 'group children');
    } finally { await ctx.close(); }
  });

  // ---- F2: region filter pills (Observers) + filtering still works --------
  await step('F2 region filter pills: hostile code inert, selecting it filters the observers table', async () => {
    const { ctx, page } = await hostileContext(browser, { regions: hostileRegions });
    try {
      await page.goto(BASE + '/#/observers', { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('#obsRegionFilter .region-pill');
      await page.waitForSelector('#obsTable tbody tr');
      const pills = await page.$$eval('#obsRegionFilter .region-pill',
        (els) => els.map((e) => ({ region: e.dataset.region, text: e.textContent })));
      const hostilePill = pills.find((p) => p.region === HOSTILE);
      assert(hostilePill, 'no pill carries the hostile code in data-region: ' + JSON.stringify(pills));
      assert(hostilePill.text === HOSTILE, 'hostile pill label is not the literal code: ' + hostilePill.text);
      assertInert(await injections(page), 'region pills');

      const total = await page.$$eval('#obsTable tbody tr[data-action="navigate"]', (els) => els.length);
      await page.locator('#obsRegionFilter .region-pill').filter({ hasText: 'onerror' }).click();
      await page.waitForFunction((n) => document.querySelectorAll('#obsTable tbody tr[data-action="navigate"]').length < n, total);
      const r = await page.evaluate(() => ({
        stored: localStorage.getItem('meshcore-region-filter'),
        regions: Array.from(document.querySelectorAll('#obsTable tbody tr[data-action="navigate"] .badge-region'))
          .map((b) => b.textContent),
        active: Array.from(document.querySelectorAll('#obsRegionFilter .region-pill-active')).map((e) => e.dataset.region),
      }));
      assert(r.stored === JSON.stringify([HOSTILE]), 'persisted selection is not the exact code: ' + r.stored);
      assert(r.regions.length > 0 && r.regions.every((t) => t === HOSTILE),
        'filtered table must only show the hostile region: ' + JSON.stringify(r.regions));
      assert(JSON.stringify(r.active) === JSON.stringify([HOSTILE]), 'active pill: ' + JSON.stringify(r.active));
      assertInert(await injections(page), 'region pills after selection');
    } finally { await ctx.close(); }
  });

  // ---- F2: region filter dropdown (Observers, 5 regions) -----------------
  await step('F2 region filter dropdown: items + trigger label inert, selection filters', async () => {
    const { ctx, page } = await hostileContext(browser, {
      regions: (real) => Object.assign(hostileRegions(real), { [BENIGN_REGION]: BENIGN_REGION }),
    });
    try {
      await page.goto(BASE + '/#/observers', { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('#obsRegionFilter .region-dropdown-trigger');
      await page.waitForSelector('#obsTable tbody tr');
      await page.click('#obsRegionFilter .region-dropdown-trigger');
      const items = await page.$$eval('#obsRegionFilter .region-dropdown-item',
        (els) => els.map((e) => ({ region: e.querySelector('input').dataset.region, text: e.textContent.trim() })));
      const h = items.find((i) => i.region === HOSTILE);
      assert(h && h.text === HOSTILE + ' - ' + HOSTILE, 'hostile dropdown item: ' + JSON.stringify(items));
      const b = items.find((i) => i.region === BENIGN_REGION);
      assert(b && b.text === BENIGN_REGION + ' - ' + BENIGN_REGION, 'benign item mangled: ' + JSON.stringify(b));
      assertInert(await injections(page), 'region dropdown');

      await page.evaluate((code) => {
        const input = Array.from(document.querySelectorAll('#obsRegionFilter input[data-region]'))
          .find((i) => i.dataset.region === code);
        input.click();
      }, HOSTILE);
      await page.waitForFunction(() => document.querySelector('#obsRegionFilter .region-dropdown-trigger')
        .textContent !== 'All Regions ▾');
      const label = await page.$eval('#obsRegionFilter .region-dropdown-trigger', (e) => e.textContent);
      assert(label === HOSTILE + ' ▾', 'trigger label is not the literal code: ' + JSON.stringify(label));
      const regions = await page.$$eval('#obsTable tbody tr[data-action="navigate"] .badge-region', (els) => els.map((e) => e.textContent));
      assert(regions.length > 0 && regions.every((t) => t === HOSTILE),
        'filtered table must only show the hostile region: ' + JSON.stringify(regions));
      assertInert(await injections(page), 'region dropdown trigger label');
    } finally { await ctx.close(); }
  });

  // ---- F2: the filter on the other pages that render it ------------------
  for (const [hash, sel] of [['#/packets', '#packetsRegionFilter'], ['#/nodes', '#nodesRegionFilter'], ['#/analytics', '#analyticsRegionFilter']]) {
    await step(`F2 region filter on ${hash}: hostile code inert`, async () => {
      const { ctx, page } = await hostileContext(browser, { regions: hostileRegions });
      try {
        await page.goto(BASE + '/' + hash, { waitUntil: 'domcontentloaded' });
        await page.waitForFunction((s) => {
          const el = document.querySelector(s);
          return el && Array.from(el.querySelectorAll('[data-region]')).length >= 4;
        }, sel);
        const codes = await page.$$eval(sel + ' [data-region]', (els) => els.map((e) => e.dataset.region));
        assert(codes.includes(HOSTILE), hash + ': hostile code lost from data-region: ' + JSON.stringify(codes));
        assertInert(await injections(page), hash + ' region filter');
      } finally { await ctx.close(); }
    });
  }

  // ---- F3: observers row with a quote in the id --------------------------
  await step('F3 observers row: id with a quote navigates without running code', async () => {
    const { ctx, page } = await hostileContext(browser, {
      regions: (r) => r,
      extraObservers: [{ id: QUOTE_ID, name: 'xss333 quote', iata: 'SJC', last_seen: new Date().toISOString() }],
    });
    try {
      await page.goto(BASE + '/#/observers', { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('#obsTable tbody tr');
      const idx = await page.$$eval('#obsTable tbody tr[data-action="navigate"]',
        (els, id) => els.findIndex((e) => e.dataset.observerId === id), QUOTE_ID);
      assert(idx >= 0, 'synthetic observer row not rendered');
      await page.locator('#obsTable tbody tr[data-action="navigate"]').nth(idx).locator('[data-testid="obs-cell-name"]').click();
      await page.waitForFunction((want) => location.hash === want, '#/observers/' + encodeURIComponent(QUOTE_ID));
      assertInert(await injections(page), 'observer row click');
    } finally { await ctx.close(); }
  });

  await step('F3 observers row: plain id still navigates to the detail page', async () => {
    const { ctx, page } = await hostileContext(browser, { regions: (r) => r });
    try {
      await page.goto(BASE + '/#/observers', { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('#obsTable tbody tr[data-action="navigate"]');
      const id = await page.$eval('#obsTable tbody tr[data-action="navigate"]', (e) => e.dataset.observerId);
      await page.locator('#obsTable tbody tr[data-action="navigate"]').first().locator('[data-testid="obs-cell-name"]').click();
      await page.waitForFunction((want) => location.hash === want, '#/observers/' + encodeURIComponent(id));
    } finally { await ctx.close(); }
  });

  // The inline onclick also navigated on narrow viewports, where the
  // delegated handler is meant to open the slide-over INSTEAD (#1056 AC#4).
  await step('F3 observers row on a narrow viewport: opens the slide-over, list stays', async () => {
    const ctx = await browser.newContext({ viewport: { width: 500, height: 900 } });
    try {
      const page = await ctx.newPage();
      await page.goto(BASE + '/#/observers', { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('#obsTable tbody tr[data-action="navigate"]');
      await page.locator('#obsTable tbody tr[data-action="navigate"]').first().locator('[data-testid="obs-cell-name"]').click();
      await page.waitForFunction(() => window.SlideOver && window.SlideOver.isOpen());
      await page.waitForTimeout(500); // a stray navigation would land by now
      const hash = await page.evaluate(() => location.hash);
      assert(hash === '#/observers', 'narrow row click navigated away: ' + hash);
    } finally { await ctx.close(); }
  });

  // ---- N1 (round 3): nodes-page detail panel "Regions:" summary ----------
  // The detail panel (renderDetail → #nodesRight) joins the observer IATAs
  // from /api/nodes/{pk}/health into a "Regions:" line, one line above the
  // already-escaped per-observer badge. Select a node, inject the hostile
  // IATA into its health response, and judge the DOM.
  await step('N1 nodes detail panel: Regions summary renders the hostile IATA as text', async () => {
    const ctx = await browser.newContext({ viewport: { width: 1400, height: 900 } });
    let dialogs = 0;
    try {
      await ctx.route((url) => /\/api\/nodes\/[^/]+\/health$/.test(url.pathname), async (route) => {
        const res = await route.fetch();
        const body = await res.json();
        // Guarantee at least one observer so the Regions section renders,
        // whichever node the first row happens to be.
        body.observers = (body.observers && body.observers.length)
          ? body.observers
          : [{ observer_id: 'OBS-H', observer_name: 'h', packetCount: 1 }];
        for (const o of body.observers) o.iata = HOSTILE;
        await route.fulfill({ response: res, json: body });
      });
      const page = await ctx.newPage();
      page.setDefaultTimeout(15000);
      page.on('dialog', (d) => { dialogs++; d.dismiss().catch(() => {}); });
      await page.goto(BASE + '/#/nodes', { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('#nodesBody tr[data-action="select"]');
      await page.click('#nodesBody tr[data-action="select"]');
      await page.waitForFunction(() => {
        const r = document.getElementById('nodesRight');
        return Array.from(r ? r.querySelectorAll('div') : [])
          .some((d) => d.innerHTML.trim().startsWith('<strong>Regions:</strong>'));
      });
      await page.waitForTimeout(400);
      const r = await page.evaluate((h) => {
        const right = document.getElementById('nodesRight');
        // The exact summary div, not an ancestor (whose innerHTML could carry
        // an unrelated <img> such as a node avatar).
        const line = Array.from(right.querySelectorAll('div'))
          .find((d) => d.innerHTML.trim().startsWith('<strong>Regions:</strong>'));
        return {
          fired: window.__xss333 || 0,
          imgs: document.querySelectorAll('img[src="x"]').length,
          showsText: !!line && line.textContent.includes(h),
          hasRawImg: !!line && /<img\b/i.test(line.innerHTML),
        };
      }, HOSTILE);
      r.dialogs = dialogs;
      assertInert(r, 'nodes detail Regions summary');
      assert(r.showsText, 'the hostile IATA is not shown as text in the Regions summary');
      assert(!r.hasRawImg, 'a raw <img> survived in the Regions summary');
    } finally { await ctx.close(); }
  });

  await browser.close();
  console.log('\n' + passed + ' passed, ' + failed + ' failed');
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
