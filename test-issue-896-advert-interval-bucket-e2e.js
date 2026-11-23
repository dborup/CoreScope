/**
 * #896 M1 — Analytics → Advert Intervals: a Flood or Zero-hop distribution
 * row opens the nodes of its bucket in the card's panel
 * (GET /api/analytics/advert-intervals/bucket), deep-linked as
 * ?bucket=<class>.<interval_s>.<s|u>.
 *
 * Needs the fixture seeded with test-fixtures/seed-245-advert-intervals.sql
 * (CI applies it after migrating the fixture; the #896 rows are at its end):
 * the snapped Flood 3 h bucket holds Alpha Flood E2E (SJC), bravo flood E2E
 * (OAK) and Short Flood E2E (SJC); the unsnapped Zero-hop 300 min bucket
 * holds Long Zero-hop E2E. Never run against prod.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-issue-896-advert-interval-bucket-e2e.js
 * SCREENSHOT_DIR=<dir> also saves JPEG screenshots (960 px wide) of the states.
 */
'use strict';

const path = require('path');
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const SHOTS = process.env.SCREENSHOT_DIR || '';
const pk = (n) => '245e2e00000000000000000000000000000000000000000000000000000000' + String(n).padStart(2, '0');
const ALPHA = pk(6), BRAVO = pk(7), SHORT = pk(5), LONG_ZH = pk(8);
const TAB = '#/analytics?tab=advert-intervals';
const FLOOD_3H = 'flood.10800.s', ZH_300 = 'zero_hop.18000.u';
const ROW = (key) => '[data-ai-bucket="' + key + '"]';
const BUCKET_API = '/api/analytics/advert-intervals/bucket';

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }
async function shot(page, name, opts) {
  if (!SHOTS) return;
  await page.waitForTimeout(300);
  await page.screenshot(Object.assign({ path: path.join(SHOTS, name + '.jpg'), type: 'jpeg', quality: 60 }, opts || {}));
}
const hashParam = (page, k) => page.evaluate((key) => new URLSearchParams(location.hash.split('?')[1] || '').get(key), k);

// The open panel as the user reads it.
async function readPanel(page, cls) {
  await page.waitForSelector('#ai-bucket-panel-' + cls + ':not([hidden]) .ai-bucket-body[aria-busy="false"]');
  return page.evaluate((c) => {
    const p = document.getElementById('ai-bucket-panel-' + c);
    const txt = (el) => (el ? el.textContent.replace(/\s+/g, ' ').trim() : '');
    return {
      heading: txt(p.querySelector('h4')),
      text: txt(p),
      members: Array.from(p.querySelectorAll('[data-ai-member]'), (li) => li.dataset.aiMember),
      names: Array.from(p.querySelectorAll('[data-ai-member] a'), (a) => txt(a)),
      hrefs: Array.from(p.querySelectorAll('[data-ai-member] a'), (a) => a.getAttribute('href')),
      expanded: Array.from(document.querySelectorAll('[data-ai-bucket][aria-expanded="true"]'), (r) => r.dataset.aiBucket),
      focus: document.activeElement && (document.activeElement.id || document.activeElement.dataset.aiBucket || document.activeElement.tagName),
    };
  }, cls);
}
const panelOpen = (page) => page.evaluate(() => Array.from(document.querySelectorAll('.ai-bucket-panel')).some((p) => !p.hidden));

// Requests the page makes while fn runs.
async function requestsDuring(page, fn) {
  const urls = [];
  const on = (r) => urls.push(new URL(r.url()).pathname);
  page.on('request', on);
  try { await fn(); } finally { page.off('request', on); }
  return { bucket: urls.filter((u) => u === BUCKET_API).length, nodes: urls.filter((u) => u.startsWith('/api/nodes/')).length, urls };
}

// Every page load re-renders the tab once on the theme-refresh that follows
// /api/config/theme (app.js); wait for it so a click is not raced by it.
const MARK_THEME_REFRESH = () => {
  window.__aiThemeRefreshes = 0;
  window.addEventListener('theme-refresh', () => { window.__aiThemeRefreshes++; });
  // The one-time gesture hints would cover the phone screenshots.
  try {
    ['row-swipe', 'tab-swipe', 'edge-drawer', 'pull-refresh'].forEach((k) => localStorage.setItem('meshcore-gesture-hints-' + k, 'seen'));
  } catch (_) { /* storage blocked */ }
};
async function settle(page) {
  await page.waitForFunction(() => window.__aiThemeRefreshes > 0, null, { timeout: 5000 }).catch(() => {});
  await page.waitForTimeout(100);
}
async function gotoSettled(page, hash) {
  await page.goto(BASE + '/' + hash, { waitUntil: 'domcontentloaded' });
  await settle(page);
}

async function openTabCold(page) {
  await gotoSettled(page, '#/analytics');
  await page.click('#analyticsTabs .tab-btn[data-tab="advert-intervals"]');
  await page.waitForSelector(ROW(FLOOD_3H));
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  const ctx = await browser.newContext({ viewport: { width: 960, height: 900 } });
  await ctx.addInitScript(MARK_THEME_REFRESH);
  const page = await ctx.newPage();
  page.setDefaultTimeout(15000);
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));

  console.log(`\n=== #896 M1 advert interval bucket nodes E2E against ${BASE} ===`);

  await step('API: the bucket lists the bar\'s nodes, filtered by region, unsnapped apart', async () => {
    await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
    const r = await page.evaluate(async (api) => {
      const get = async (q) => { const res = await fetch(api + q); return { status: res.status, body: await res.json() }; };
      return {
        hist: await (await fetch('/api/analytics/advert-intervals')).json(),
        flood: (await get('?class=flood&interval_s=10800&snapped=true')).body,
        sjc: (await get('?class=flood&interval_s=10800&snapped=true&region=SJC')).body,
        zh: (await get('?class=zero_hop&interval_s=18000&snapped=false')).body,
        bad: (await get('?class=mixed&interval_s=10800&snapped=true')).status,
        stale: await get('?class=flood&interval_s=10800&snapped=true&offset=1&snapshot=1999-01-01T00:00:00Z'),
      };
    }, BUCKET_API);
    const bar = r.hist.flood.buckets.find((b) => b.interval_s === 10800 && b.snapped);
    assert(bar && bar.nodes === r.flood.nodes && r.flood.nodes === 3, 'count = bar: ' + JSON.stringify({ bar, nodes: r.flood.nodes }));
    assert(r.flood.generated_at === r.hist.scan.generated_at, 'same snapshot');
    assert(r.flood.members.map((m) => m.public_key).join() === [ALPHA, BRAVO, SHORT].join(), 'name order: ' + r.flood.members.map((m) => m.name));
    assert(r.sjc.members.map((m) => m.public_key).join() === [ALPHA, SHORT].join() && r.sjc.nodes === 2, 'region=SJC: ' + JSON.stringify(r.sjc.members.map((m) => m.name)));
    assert(r.zh.members.length === 1 && r.zh.members[0].public_key === LONG_ZH && r.zh.members[0].snapped === false && r.zh.members[0].interval_s === 18000, 'unsnapped: ' + JSON.stringify(r.zh));
    assert(r.bad === 400, 'bad class: ' + r.bad);
    assert(r.stale.status === 409 && r.stale.body.generated_at === r.hist.scan.generated_at, '409: ' + JSON.stringify(r.stale));
  });

  await step('cold #/analytics: a click opens the Flood 3 h nodes — one bucket request, no /api/nodes/*', async () => {
    await openTabCold(page);
    const before = await page.evaluate(() => history.length);
    const req = await requestsDuring(page, async () => {
      await page.click(ROW(FLOOD_3H));
      await readPanel(page, 'flood');
    });
    assert(req.bucket === 1 && req.nodes === 0, 'requests: ' + JSON.stringify(req));
    const p = await readPanel(page, 'flood');
    assert(p.heading === 'Nodes with an estimated flood advert interval of 3 h', p.heading);
    assert(p.members.join() === [ALPHA, BRAVO, SHORT].join(), 'members: ' + p.names);
    assert(p.hrefs[0] === '#/nodes/' + ALPHA + '?section=node-packets&adverts=flood', p.hrefs[0]);
    assert(/Observed estimates/.test(p.text) && /All regions · All areas/.test(p.text) && /Showing 3 of 3/.test(p.text), p.text);
    assert(p.expanded.join() === FLOOD_3H && p.focus === 'ai-bucket-heading-flood', JSON.stringify(p));
    assert(await hashParam(page, 'bucket') === FLOOD_3H && await hashParam(page, 'tab') === 'advert-intervals', 'URL');
    assert(await page.evaluate(() => history.length) === before + 1, 'opening from closed pushes one entry');
    const vis = await page.evaluate(() => { const r = document.getElementById('ai-bucket-panel-flood').getBoundingClientRect(); return { top: r.top, bottom: r.bottom, vh: innerHeight }; });
    assert(vis.top >= 52 && vis.bottom <= vis.vh, 'the opened panel is scrolled into view: ' + JSON.stringify(vis));
    await shot(page, 'desktop-light-panel', { fullPage: false });
  });

  await step('the unsnapped Zero-hop row opens its own list; switching replaces the entry', async () => {
    const before = await page.evaluate(() => history.length);
    await page.click(ROW(ZH_300));
    const p = await readPanel(page, 'zero_hop');
    assert(p.heading === 'Nodes with an estimated zero-hop advert interval of 300 min (outside the settable range)', p.heading);
    assert(p.members.join() === LONG_ZH && p.expanded.join() === ZH_300, JSON.stringify(p));
    assert(await page.evaluate(() => document.getElementById('ai-bucket-panel-flood').hidden), 'flood panel closed');
    assert(await hashParam(page, 'bucket') === ZH_300, 'URL');
    assert(await page.evaluate(() => history.length) === before, 'switching buckets replaces');
    await page.click(ROW(FLOOD_3H));
    await readPanel(page, 'flood');
  });

  await step('a node link opens its adverts; Back reopens the panel; Close closes it and returns focus', async () => {
    await page.click('#ai-bucket-panel-flood [data-ai-member="' + ALPHA + '"] a');
    await page.waitForFunction((k) => location.hash === '#/nodes/' + k + '?section=node-packets&adverts=flood', ALPHA);
    await page.waitForSelector('#node-packets');
    await page.goBack();
    const p = await readPanel(page, 'flood');
    assert(p.members.join() === [ALPHA, BRAVO, SHORT].join(), 'reopened from the URL: ' + p.names);
    await page.click('#ai-bucket-panel-flood [data-ai-bucket-close]');
    await page.waitForFunction(() => !/[?&]bucket=/.test(location.hash));
    await page.waitForFunction((sel) => document.activeElement === document.querySelector(sel), ROW(FLOOD_3H));
    assert(!(await panelOpen(page)), 'panel closed');
    assert(await hashParam(page, 'tab') === 'advert-intervals', 'still on the tab');
  });

  await step('browser Back from an open bucket closes the panel', async () => {
    await page.click(ROW(FLOOD_3H));
    await readPanel(page, 'flood');
    await page.goBack();
    await page.waitForSelector(ROW(FLOOD_3H));
    await page.waitForFunction(() => !/[?&]bucket=/.test(location.hash));
    assert(!(await panelOpen(page)), 'panel closed after Back');
  });

  await step('cold deep link opens the panel with one bucket request; Close drops bucket= in place', async () => {
    await gotoSettled(page, TAB + '&bucket=' + FLOOD_3H);
    // The load's theme refresh re-renders the tab: it must reuse the list.
    const req = await requestsDuring(page, async () => {
      await page.reload({ waitUntil: 'domcontentloaded' });
      await settle(page);
      await readPanel(page, 'flood');
      await page.waitForTimeout(500);
    });
    assert(req.bucket === 1 && req.nodes === 0, 'cold deep link requests: ' + JSON.stringify({ bucket: req.bucket, nodes: req.nodes }));
    const p = await readPanel(page, 'flood');
    assert(p.members.length === 3 && p.expanded.join() === FLOOD_3H, JSON.stringify(p));
    const before = await page.evaluate(() => history.length);
    await page.click('#ai-bucket-panel-flood [data-ai-bucket-close]');
    await page.waitForFunction(() => !/[?&]bucket=/.test(location.hash));
    assert(!(await panelOpen(page)) && await page.evaluate(() => history.length) === before, 'closed in place');
    assert(await page.evaluate((sel) => document.activeElement === document.querySelector(sel), ROW(FLOOD_3H)), 'focus back on the row');
  });

  await step('region= link narrows the list and the panel names the region', async () => {
    await gotoSettled(page, TAB + '&bucket=' + FLOOD_3H + '&region=SJC');
    await page.waitForFunction(() => document.querySelectorAll('#ai-bucket-panel-flood [data-ai-member]').length === 2);
    const p = await readPanel(page, 'flood');
    assert(p.members.join() === [ALPHA, SHORT].join() && /Region SJC/.test(p.text), JSON.stringify(p));
    // A region link is remembered (#531); the steps below run unfiltered.
    await page.evaluate(() => window.RegionFilter.setSelected(null));
  });

  await step('switching tabs drops bucket= from the URL', async () => {
    await gotoSettled(page, TAB + '&bucket=' + FLOOD_3H);
    await readPanel(page, 'flood');
    await page.click('#analyticsTabs .tab-btn[data-tab="roles"]');
    await page.waitForFunction(() => /[?&]tab=roles\b/.test(location.hash));
    assert(await hashParam(page, 'bucket') === null, 'bucket= dropped');
  });

  await step('keyboard only: Tab to the row, Enter opens, Escape closes, Space opens', async () => {
    await gotoSettled(page, TAB);
    await page.waitForSelector(ROW(FLOOD_3H));
    await page.evaluate(() => document.activeElement && document.activeElement.blur());
    let onRow = false;
    for (let i = 0; i < 250 && !onRow; i++) {
      await page.keyboard.press('Tab');
      onRow = await page.evaluate((sel) => document.activeElement === document.querySelector(sel), ROW(FLOOD_3H));
    }
    assert(onRow, 'Tab reaches the Flood 3 h row');
    await page.keyboard.press('Enter');
    let p = await readPanel(page, 'flood');
    assert(p.focus === 'ai-bucket-heading-flood' && p.expanded.join() === FLOOD_3H, 'Enter opens, focus on the heading: ' + JSON.stringify(p));
    await page.keyboard.press('Tab');
    assert(await page.evaluate(() => document.activeElement.hasAttribute('data-ai-bucket-close')), 'Tab reaches Close');
    await page.keyboard.press('Escape');
    await page.waitForFunction(() => !/[?&]bucket=/.test(location.hash));
    await page.waitForFunction((sel) => document.activeElement === document.querySelector(sel), ROW(FLOOD_3H));
    assert(!(await panelOpen(page)), 'Escape closes');
    await page.keyboard.press(' ');
    p = await readPanel(page, 'flood');
    assert(p.members.length === 3 && p.focus === 'ai-bucket-heading-flood', 'Space opens');
  });

  await step('error state: a failed request shows Retry, Retry loads the list', async () => {
    await gotoSettled(page, TAB);
    await page.waitForSelector(ROW(FLOOD_3H));
    const fail = (route) => route.fulfill({ status: 500, contentType: 'application/json', body: '{"error":"boom"}' });
    await page.route('**' + BUCKET_API + '?*', fail);
    await page.click(ROW(FLOOD_3H));
    await page.waitForSelector('#ai-bucket-panel-flood [role="alert"] [data-ai-bucket-retry]');
    assert(!(await page.evaluate(() => document.querySelector('#ai-bucket-panel-flood [data-ai-member]'))), 'no list on error');
    await page.unroute('**' + BUCKET_API + '?*', fail);
    await page.click('#ai-bucket-panel-flood [data-ai-bucket-retry]');
    const p = await readPanel(page, 'flood');
    assert(p.members.length === 3, 'retry: ' + p.names);
  });

  await step('a refreshed snapshot is explained; Refresh chart refetches it', async () => {
    await gotoSettled(page, TAB);
    await page.waitForSelector(ROW(FLOOD_3H));
    const newer = async (route) => {
      const res = await route.fetch();
      const body = await res.json();
      body.generated_at = '2099-01-01T00:05:00Z';
      body.nodes += 1;
      body.withheld += 1;
      await route.fulfill({ response: res, body: JSON.stringify(body) });
    };
    await page.route('**' + BUCKET_API + '?*', newer);
    await page.click(ROW(FLOOD_3H));
    await page.waitForSelector('#ai-bucket-panel-flood .ai-bucket-notice');
    const p = await readPanel(page, 'flood');
    assert(/refreshed at 2099-01-01 00:05 UTC; this bucket now holds 4 nodes \(the chart shows 3\)/.test(p.text), p.text);
    assert(/1 of the 4 nodes in this bucket is not listed because this instance hides their identity/.test(p.text), p.text);
    await page.unroute('**' + BUCKET_API + '?*', newer);
    const req = await requestsDuring(page, async () => {
      await page.click('#ai-bucket-panel-flood [data-ai-bucket-refresh]');
      await page.waitForFunction(() => !document.querySelector('#ai-bucket-panel-flood .ai-bucket-notice') &&
        document.querySelectorAll('#ai-bucket-panel-flood [data-ai-member]').length === 3);
    });
    assert(req.urls.includes('/api/analytics/advert-intervals') && req.bucket === 1, 'chart and bucket refetched once: ' + JSON.stringify(req));
  });

  await step('Show more appends a page; a 409 on it restarts from page 1', async () => {
    await gotoSettled(page, TAB);
    await page.waitForSelector(ROW(FLOOD_3H));
    const seen = [];
    const paged = async (route) => {
      const u = new URL(route.request().url());
      seen.push(u.searchParams.get('offset') + '/' + (u.searchParams.get('snapshot') || ''));
      if (u.searchParams.get('offset') !== '0') {
        return route.fulfill({ status: 409, contentType: 'application/json', body: JSON.stringify({ error: 'changed', generated_at: 'x' }) });
      }
      const res = await route.fetch();
      const body = await res.json();
      if (seen.length === 1) { body.members = body.members.slice(0, 2); body.limit = 2; }
      await route.fulfill({ response: res, body: JSON.stringify(body) });
    };
    await page.route('**' + BUCKET_API + '?*', paged);
    await page.click(ROW(FLOOD_3H));
    let p = await readPanel(page, 'flood');
    assert(p.members.length === 2 && /Showing 2 of 3/.test(p.text) && /Show 1 more/.test(p.text), p.text);
    await page.click('#ai-bucket-panel-flood [data-ai-bucket-more]');
    await page.waitForFunction(() => document.querySelectorAll('#ai-bucket-panel-flood [data-ai-member]').length === 3);
    p = await readPanel(page, 'flood');
    assert(seen.length === 3 && seen[0].startsWith('0/') && /^2\/\d{4}-/.test(seen[1]) && seen[2].startsWith('0/'), 'page 2 names the snapshot, 409 restarts: ' + seen);
    assert(p.members.join() === [ALPHA, BRAVO, SHORT].join(), p.names);
    await page.unroute('**' + BUCKET_API + '?*', paged);
  });

  await step('empty state for a bucket the snapshot does not hold', async () => {
    await gotoSettled(page, TAB + '&bucket=flood.36000.s');
    const p = await readPanel(page, 'flood');
    assert(/No listed nodes in this bucket for the current selection/.test(p.text) && p.members.length === 0, p.text);
    await shot(page, 'desktop-empty', { fullPage: false });
  });

  await step('dark theme: panel readable, selected row marked with the theme accent', async () => {
    await page.evaluate(() => localStorage.setItem('meshcore-theme', 'dark'));
    await gotoSettled(page, TAB + '&bucket=' + FLOOD_3H);
    await page.reload({ waitUntil: 'domcontentloaded' });
    await settle(page);
    await readPanel(page, 'flood');
    const c = await page.evaluate((sel) => {
      const panel = document.getElementById('ai-bucket-panel-flood');
      return { theme: document.documentElement.getAttribute('data-theme'), bg: getComputedStyle(panel).backgroundColor,
        text: getComputedStyle(panel.querySelector('[data-ai-member] a')).color, row: getComputedStyle(document.querySelector(sel)).boxShadow };
    }, ROW(FLOOD_3H));
    assert(c.theme === 'dark' && c.bg !== c.text && /inset/.test(c.row), JSON.stringify(c));
    await page.evaluate(() => document.querySelector('[data-ai-section="dist-flood"]').scrollIntoView());
    await shot(page, 'desktop-dark-panel', { fullPage: false });
    await page.evaluate(() => localStorage.setItem('meshcore-theme', 'light'));
  });

  await step('375×560 touch: tap opens the panel in view, no overflow, 44 px rows, a tap follows a node link', async () => {
    const m = await browser.newContext({ viewport: { width: 375, height: 560 }, isMobile: true, hasTouch: true });
    await m.addInitScript(MARK_THEME_REFRESH);
    const mp = await m.newPage();
    mp.setDefaultTimeout(15000);
    mp.on('pageerror', (e) => errors.push(e.message));
    await gotoSettled(mp, '#/analytics');
    await mp.waitForSelector('#analyticsTabs .tab-btn[data-tab="advert-intervals"]');
    await mp.evaluate(() => document.querySelector('#analyticsTabs .tab-btn[data-tab="advert-intervals"]').scrollIntoView({ inline: 'center' }));
    await mp.tap('#analyticsTabs .tab-btn[data-tab="advert-intervals"]');
    await mp.waitForSelector(ROW(FLOOD_3H));
    const rowH = await mp.evaluate((sel) => document.querySelector(sel).getBoundingClientRect().height, ROW(FLOOD_3H));
    assert(rowH >= 44, 'row height ' + rowH);
    await mp.evaluate((sel) => document.querySelector(sel).scrollIntoView({ block: 'center' }), ROW(FLOOD_3H));
    const req = await requestsDuring(mp, async () => {
      await mp.tap(ROW(FLOOD_3H));
      await readPanel(mp, 'flood');
    });
    assert(req.bucket === 1 && req.nodes === 0, 'requests: ' + JSON.stringify(req));
    await mp.waitForTimeout(200);
    const v = await mp.evaluate(() => {
      const h = document.getElementById('ai-bucket-heading-flood').getBoundingClientRect();
      const top = document.querySelector('.top-nav').getBoundingClientRect().bottom;
      const nav = document.querySelector('.bottom-nav');
      const bottom = nav && getComputedStyle(nav).display !== 'none' ? nav.getBoundingClientRect().top : innerHeight;
      const lastLink = Array.from(document.querySelectorAll('#ai-bucket-panel-flood [data-ai-member] a')).pop().getBoundingClientRect();
      return { top: h.top, bottom: h.bottom, navTop: top, navBottom: bottom, lastLinkBottom: lastLink.bottom,
        sw: document.documentElement.scrollWidth, cw: document.documentElement.clientWidth,
        links: Array.from(document.querySelectorAll('#ai-bucket-panel-flood [data-ai-member] a'), (a) => a.getBoundingClientRect().height) };
    });
    assert(v.top >= v.navTop && v.bottom <= v.navBottom, 'heading visible between the navs: ' + JSON.stringify(v));
    assert(v.lastLinkBottom <= v.navBottom, 'the short list is visible above the bottom nav: ' + JSON.stringify(v));
    assert(v.sw <= 375 && v.sw <= v.cw, 'horizontal overflow: ' + JSON.stringify(v));
    assert(v.links.every((h) => h >= 44), 'link tap targets: ' + v.links);
    await shot(mp, 'mobile-375x560-panel', { fullPage: false });
    await mp.tap('#ai-bucket-panel-flood [data-ai-member="' + BRAVO + '"] a');
    await mp.waitForFunction((k) => location.hash.startsWith('#/nodes/' + k), BRAVO);
    await m.close();
  });

  await step('375×560 touch: a region pill tap while a bucket is open refetches once with region= and narrows the list (#948 F1)', async () => {
    const m = await browser.newContext({ viewport: { width: 375, height: 560 }, isMobile: true, hasTouch: true });
    await m.addInitScript(MARK_THEME_REFRESH);
    const mp = await m.newPage();
    mp.setDefaultTimeout(15000);
    mp.on('pageerror', (e) => errors.push(e.message));
    await gotoSettled(mp, TAB + '&bucket=' + FLOOD_3H);
    const before = await readPanel(mp, 'flood');
    assert(before.members.join() === [ALPHA, BRAVO, SHORT].join(), 'unfiltered: ' + before.names);
    const bucketUrls = [];
    mp.on('request', (r) => { const u = new URL(r.url()); if (u.pathname === BUCKET_API) bucketUrls.push(u.search); });
    await mp.evaluate(() => document.querySelector('#analyticsRegionFilter .region-pill[data-region="OAK"]').scrollIntoView({ block: 'center', inline: 'center' }));
    await mp.tap('#analyticsRegionFilter .region-pill[data-region="OAK"]');
    await mp.waitForFunction((k) => {
      const l = document.querySelectorAll('#ai-bucket-panel-flood [data-ai-member]');
      return l.length === 1 && l[0].dataset.aiMember === k;
    }, BRAVO);
    const p = await readPanel(mp, 'flood');
    assert(p.members.join() === BRAVO && /Region OAK/.test(p.text) && /Showing 1 of 1/.test(p.text), JSON.stringify(p));
    assert(bucketUrls.length === 1 && /[?&]region=OAK\b/.test(bucketUrls[0]), 'bucket requests: ' + bucketUrls);
    await m.close();
  });

  await step('no page errors', async () => {
    assert(errors.length === 0, errors.join(' | '));
  });

  await browser.close();
  console.log(`\n${passed}/${passed + failed} passed`);
  process.exit(failed === 0 ? 0 : 1);
})().catch(e => { console.error(e); process.exit(2); });
