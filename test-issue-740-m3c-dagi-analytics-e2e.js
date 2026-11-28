#!/usr/bin/env node
/* Issue #740 M3c — the existing Analytics tabs honour the DAGI selection.
 *
 * Against the real Go server, unmocked (its real GET /api/config/dagi, the
 * real analytics endpoints and GET /api/analytics/dagi-unpositioned). The
 * fixture has no node inside Denmark, so a kommune answers empty analytics;
 * what is checked is what the page asks for and what it says.
 *
 *  1. Phone, 375x560, light and dark, cold start on #/analytics?tab=rf with
 *     real taps only: the DAGI trigger beside Region/Area reads "Denmark:
 *     All"; a tap opens the bottom sheet, "aar" and a tap on Aarhus put
 *     kommune=0751 in the hash next to tab=rf, the RF request carries it
 *     and the notice shows the server's hidden count; a tap on the Channels
 *     tab says the filter does not apply there; a tap on the clear button
 *     takes the param, the notice and the filter away. No sideways scroll.
 *  2. Desktop 1440x900, every tab that honours Area: a deep link with
 *     kommune=0751 sends it on every request of that tab (and never an
 *     unfiltered one first), answered 200, and shows the hidden count.
 *  3. Every tab that cannot honour it says so, and sends no DAGI param.
 *  4. Unknown code: #/analytics?tab=rf&kommune=9999 sends no request with
 *     9999, gets no 400, and leaves the hash.
 *  5. The hash keeps kommune across a tab click and a window change, and
 *     the window request carries both.
 *  6. Caches: a second selection is a new request, never the old answer;
 *     the hidden count is asked for once.
 *
 * Screenshots for the PR's browser validation go to DAGI_SHOTS_DIR when set.
 * Defaults to localhost:13581 — NEVER point at prod (AGENTS.md). CI sets BASE_URL.
 * Run: BASE_URL=http://localhost:13581 node test-issue-740-m3c-dagi-analytics-e2e.js
 */
'use strict';
const path = require('path');
const { chromium } = require('playwright');
const { tapWhenSettled } = require('./tests/e2e-tap');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const SHOTS = process.env.DAGI_SHOTS_DIR || '';

// The tabs that honour Area, so DAGI, and the request paths that must carry
// the selection (the unit test public/analytics.js twin is
// test-issue-740-m3c-dagi-analytics.js).
const HONOURING = {
  overview: ['/api/analytics/hash-sizes', '/api/analytics/hash-collisions', '/api/analytics/rf', '/api/analytics/topology'],
  rf: ['/api/analytics/rf'],
  topology: ['/api/analytics/topology'],
  hashsizes: ['/api/analytics/hash-sizes'],
  collisions: ['/api/analytics/hash-collisions'],
  nodes: ['/api/nodes', '/api/nodes/bulk-health'],
  'my-repeaters': ['/api/nodes'],
  'repeater-metrics': ['/api/nodes'],
  'clock-health': ['/api/nodes/clock-skew'],
  'advert-intervals': ['/api/analytics/advert-intervals'],
  'prefix-tool': ['/api/nodes', '/api/analytics/hash-sizes'],
};
const NOT_HONOURING = ['channels', 'subpaths', 'distance', 'neighbor-graph', 'rf-health', 'roles', 'scopes', 'foreign-traffic', 'wardriving', 'areas'];
const SHARED = /^\/api\/analytics\/(hash-sizes|hash-collisions|rf|topology|relay-airtime-share|channels)$/;

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
  console.log(`\n=== #740 M3c DAGI on the Analytics tabs against ${BASE} ===\n`);
  const errors = [];
  const api = async (p) => (await fetch(BASE + p)).json();
  const hidden = (await api('/api/analytics/dagi-unpositioned')).unpositionedHidden;
  const nodes = await api('/api/nodes?limit=5&role=repeater');
  const favKey = nodes.nodes && nodes.nodes[0] && nodes.nodes[0].public_key;

  async function cold(viewport, touch, theme) {
    const ctx = await browser.newContext({ viewport, hasTouch: touch, isMobile: touch });
    await ctx.addInitScript(([t, fav]) => {
      try {
        localStorage.setItem('meshcore-theme', t);
        if (fav) localStorage.setItem('meshcore-favorites', JSON.stringify([fav]));
        ['row-swipe', 'tab-swipe', 'edge-drawer', 'pull-refresh']
          .forEach((k) => localStorage.setItem('meshcore-gesture-hints-' + k, 'seen'));
      } catch (_) {}
    }, [theme || 'light', favKey]);
    const page = await ctx.newPage();
    page.setDefaultTimeout(15000);
    // 5xx from /api/observers/metrics: RF Health on a fixture without the
    // ingestor's observer_metrics table (pre-existing, not DAGI).
    const known = (u) => /\/api\/observers\/metrics\//.test(u);
    page.on('console', (m) => {
      if (m.type() !== 'error' || /tile|net::ERR|Failed to load resource: net/i.test(m.text())) return;
      if (/status of 500/.test(m.text()) && m.location() && known(m.location().url || '')) return;
      errors.push(m.text());
    });
    page.on('pageerror', (e) => errors.push(String(e)));
    const reqs = [];
    const bad = [];
    page.on('request', (r) => { if (/\/api\//.test(r.url())) reqs.push(r.url()); });
    page.on('response', (r) => { if (/\/api\//.test(r.url()) && r.status() >= 400 && r.status() !== 404 && !known(r.url())) bad.push(r.status() + ' ' + r.url()); });
    return { ctx, page, reqs, bad };
  }
  const pathOf = (u) => new URL(u).pathname;
  const kommuneOf = (u) => new URL(u).searchParams.get('kommune');
  const of = (reqs, p) => reqs.filter((u) => pathOf(u) === p);
  async function shot(page, name) { if (SHOTS) await page.screenshot({ path: path.join(SHOTS, name + '.png') }); }
  const notice = (page) => page.evaluate(() => { const n = document.getElementById('analyticsDagiNotice'); return n && !n.hidden ? n.textContent : ''; });
  const trigger = (page) => page.evaluate(() => { const l = document.querySelector('#analyticsDagiFilter .dagi-trigger-label'); return l ? l.textContent : ''; });
  const countText = (n) => n + ' nodes have no position, so no Danish-area filter can include them. The count covers all nodes, before the other filters.';

  await step('the fixture has nodes without a position (so the notice has something to say)', async () => {
    assert(hidden > 1, 'dagi-unpositioned: ' + hidden);
    assert(favKey, 'no repeater to star');
  });

  for (const theme of ['light', 'dark']) {
    await step(`375x560 ${theme}, cold start on RF, real taps: pick Aarhus, see Channels say so, clear it`, async () => {
      const { ctx, page, reqs, bad } = await cold({ width: 375, height: 560 }, true, theme);
      await page.goto(BASE + '/#/analytics?tab=rf', { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('#analyticsDagiFilter .dagi-trigger', { state: 'visible' });
      assert((await trigger(page)) === 'Denmark: All', 'trigger: ' + await trigger(page));
      assert((await notice(page)) === '', 'notice without a selection: ' + await notice(page));
      await shot(page, `analytics-phone-${theme}-1-closed`);

      await tapWhenSettled(page, '#analyticsDagiFilter .dagi-trigger');
      await page.waitForSelector('.dagi-panel', { state: 'visible' });
      const sheet = await page.evaluate(() => {
        const r = document.querySelector('.dagi-panel').getBoundingClientRect();
        return { left: r.left, right: r.right, bottom: r.bottom, top: r.top, sw: document.documentElement.scrollWidth,
          pos: getComputedStyle(document.querySelector('.dagi-panel')).position };
      });
      assert(sheet.pos === 'fixed' && sheet.left === 0 && sheet.right === 375 && Math.round(sheet.bottom) === 560 && sheet.top > 0,
        'not a bottom sheet inside the viewport: ' + JSON.stringify(sheet));
      assert(sheet.sw <= 375, 'sideways scroll: ' + sheet.sw);
      await tapWhenSettled(page, '.dagi-search');
      await page.keyboard.type('aar', { delay: 30 });
      await page.waitForFunction(() => Array.from(document.querySelectorAll('.dagi-option')).some((o) => !o.hidden && o.offsetParent && o.textContent === 'Aarhus'));
      await page.evaluate(() => Array.from(document.querySelectorAll('.dagi-option')).find((o) => !o.hidden && o.textContent === 'Aarhus').setAttribute('data-e2e', 'aarhus'));
      await shot(page, `analytics-phone-${theme}-2-search`);
      reqs.length = 0;
      await tapWhenSettled(page, '[data-e2e="aarhus"]');
      await page.waitForFunction(() => /[?&]kommune=0751(&|$)/.test(location.hash) && /[?&]tab=rf(&|$)/.test(location.hash));
      await page.waitForFunction(() => { const n = document.getElementById('analyticsDagiNotice'); return n && !n.hidden; });
      assert((await trigger(page)) === 'Kommune: Aarhus', 'trigger: ' + await trigger(page));
      const rf = of(reqs, '/api/analytics/rf');
      assert(rf.length >= 1 && rf.every((u) => kommuneOf(u) === '0751'), 'rf requests: ' + rf.join(' '));
      assert((await notice(page)) === countText(hidden), 'notice: ' + await notice(page));
      await page.waitForFunction(() => !/Loading analytics/.test(document.getElementById('analyticsContent').textContent));
      assert(await page.evaluate(() => document.documentElement.scrollWidth <= 375), 'sideways scroll with the selection on RF');
      await shot(page, `analytics-phone-${theme}-3-filtered`);

      await tapWhenSettled(page, '.tab-btn[data-tab="channels"]');
      await page.waitForFunction(() => /does not apply/.test((document.getElementById('analyticsDagiNotice') || {}).textContent || ''));
      assert((await notice(page)) === 'The Danish area filter (Kommune: Aarhus) does not apply to this tab: it shows data from all areas.', 'channels: ' + await notice(page));
      assert(await page.evaluate(() => /[?&]tab=channels(&|$)/.test(location.hash) && /[?&]kommune=0751(&|$)/.test(location.hash)), 'hash: ' + await page.evaluate(() => location.hash));
      await shot(page, `analytics-phone-${theme}-4-not-applied`);
      // Channels scrolls sideways at 375 px with or without a selection
      // (pre-existing, its table), so the clear is done back on RF.
      await tapWhenSettled(page, '.tab-btn[data-tab="rf"]');
      await page.waitForFunction(() => /no position/.test(document.getElementById('analyticsDagiNotice').textContent));

      reqs.length = 0;
      await tapWhenSettled(page, '#analyticsDagiFilter .dagi-clear');
      await page.waitForFunction(() => !/kommune=/.test(location.hash));
      await page.waitForFunction(() => document.getElementById('analyticsDagiNotice').hidden);
      assert((await trigger(page)) === 'Denmark: All', 'after the clear: ' + await trigger(page));
      assert(reqs.every((u) => !kommuneOf(u)), 'a DAGI param after the clear: ' + reqs.join(' '));
      // The unfiltered RF answer re-renders the tab after the clear; measure
      // the settled page (a lasting overflow still fails here).
      await page.waitForFunction(() => document.documentElement.scrollWidth <= 375, null, { timeout: 5000 }).catch(() => {});
      const wide = await page.evaluate(() => ({ sw: document.documentElement.scrollWidth, els: Array.from(document.querySelectorAll('body *'))
        .filter((e) => e.getBoundingClientRect().right > 376 && !e.closest('.analytics-tabs')).slice(0, 3).map((e) => e.tagName + '.' + e.className + '#' + e.id + ' in ' + (e.parentElement && (e.parentElement.className + '/' + getComputedStyle(e.parentElement).overflowX)) + ' ' + Math.round(e.getBoundingClientRect().right)),
        tab: location.hash, h: document.querySelector('#analyticsContent h3') && document.querySelector('#analyticsContent h3').textContent }));
      assert(wide.sw <= 375, 'sideways scroll after the flow: ' + JSON.stringify(wide));
      assert(!bad.length, 'error answers: ' + bad.join(' '));
      await ctx.close();
    });
  }

  for (const [tab, paths] of Object.entries(HONOURING)) {
    await step(`1440x900 #/analytics?tab=${tab}&kommune=0751: every request of the tab carries it, with the hidden count`, async () => {
      const { ctx, page, reqs, bad } = await cold({ width: 1440, height: 900 }, false);
      await page.goto(BASE + '/#/analytics?tab=' + tab + '&kommune=0751', { waitUntil: 'domcontentloaded' });
      await page.waitForFunction((ps) => ps.every((p) => performance.getEntriesByType('resource').some((e) => new URL(e.name).pathname === p)), paths);
      await page.waitForFunction(() => { const n = document.getElementById('analyticsDagiNotice'); return n && !n.hidden; });
      for (const p of paths) {
        const got = of(reqs, p);
        assert(got.length && got.every((u) => kommuneOf(u) === '0751'), p + ': ' + got.join(' '));
      }
      assert((await notice(page)) === countText(hidden), 'notice: ' + await notice(page));
      assert(!bad.length, 'error answers: ' + bad.join(' '));
      await ctx.close();
    });
  }

  await step('1440x900 light and dark: the tabs that cannot honour it say so, and send no DAGI param of their own', async () => {
    for (const theme of ['light', 'dark']) {
      const { ctx, page, reqs, bad } = await cold({ width: 1440, height: 900 }, false, theme);
      await page.goto(BASE + '/#/analytics?tab=channels&kommune=0751', { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('#analyticsDagiFilter .dagi-trigger', { state: 'visible' });
      for (const tab of NOT_HONOURING) {
        await page.click('.tab-btn[data-tab="' + tab + '"]');
        await page.waitForFunction(() => /does not apply/.test((document.getElementById('analyticsDagiNotice') || {}).textContent || ''));
        assert(/^The Danish area filter \(Kommune: Aarhus\) does not apply to this tab/.test(await notice(page)), tab + ': ' + await notice(page));
        if (tab === 'channels') await shot(page, `analytics-desktop-${theme}-not-applied`);
      }
      const own = reqs.filter((u) => !SHARED.test(pathOf(u)) && kommuneOf(u));
      assert(!own.length, 'DAGI param on a tab that does not honour it: ' + own.join(' '));
      const ch = of(reqs, '/api/analytics/channels');
      assert(ch.length && ch.every((u) => !kommuneOf(u)), 'channels: ' + ch.join(' '));
      assert(!bad.length, 'error answers: ' + bad.join(' '));
      await page.click('.tab-btn[data-tab="topology"]');
      await page.waitForFunction(() => /no position/.test(document.getElementById('analyticsDagiNotice').textContent));
      await shot(page, `analytics-desktop-${theme}-count`);
      await ctx.close();
    }
  });

  await step('unknown code: #/analytics?tab=rf&kommune=9999 is never sent, gets no 400, and leaves the hash', async () => {
    const { ctx, page, reqs, bad } = await cold({ width: 1440, height: 900 }, false);
    await page.goto(BASE + '/#/analytics?tab=rf&kommune=9999', { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#analyticsDagiFilter .dagi-trigger', { state: 'visible' });
    await page.waitForFunction(() => performance.getEntriesByType('resource').some((e) => /\/api\/analytics\/rf/.test(e.name)));
    assert(!reqs.some((u) => /9999/.test(u)), 'sent: ' + reqs.filter((u) => /9999/.test(u)).join(' '));
    assert(!bad.length, 'error answers: ' + bad.join(' '));
    assert(await page.evaluate(() => location.hash) === '#/analytics?tab=rf', 'hash: ' + await page.evaluate(() => location.hash));
    assert((await notice(page)) === '', 'notice: ' + await notice(page));
    await ctx.close();
  });

  await step('the hash keeps kommune across a tab click and a window change', async () => {
    const { ctx, page, reqs } = await cold({ width: 1440, height: 900 }, false);
    await page.goto(BASE + '/#/analytics?tab=rf&kommune=0751', { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#analyticsDagiFilter .dagi-trigger', { state: 'visible' });
    await page.click('.tab-btn[data-tab="topology"]');
    await page.selectOption('#analyticsTimeWindow', '7d');
    await page.waitForFunction(() => /[?&]window=7d(&|$)/.test(location.hash));
    const h = await page.evaluate(() => location.hash);
    assert(/[?&]tab=topology(&|$)/.test(h) && /[?&]kommune=0751(&|$)/.test(h), 'hash: ' + h);
    await page.waitForFunction(() => performance.getEntriesByType('resource').some((e) => /\/api\/analytics\/topology\?kommune=0751&window=7d$/.test(e.name)));
    assert(reqs.some((u) => /\/api\/analytics\/topology\?kommune=0751&window=7d$/.test(u)), reqs.join(' '));
    await ctx.close();
  });

  await step('a second selection is a new request, and the hidden count is asked for once', async () => {
    const { ctx, page, reqs } = await cold({ width: 1440, height: 900 }, false);
    await page.goto(BASE + '/#/analytics?tab=rf&kommune=0751', { waitUntil: 'domcontentloaded' });
    await page.waitForFunction(() => { const n = document.getElementById('analyticsDagiNotice'); return n && !n.hidden; });
    await page.evaluate(() => { location.hash = '#/analytics?tab=rf&kommune=0860'; });
    await page.waitForFunction(() => document.querySelector('#analyticsDagiFilter .dagi-trigger-label').textContent === 'Kommune: Hjørring');
    await page.waitForFunction(() => performance.getEntriesByType('resource').some((e) => /\/api\/analytics\/rf\?kommune=0860$/.test(e.name)));
    for (const p of ['/api/analytics/rf', '/api/analytics/topology', '/api/analytics/hash-sizes']) {
      const ks = of(reqs, p).map(kommuneOf);
      assert(ks.includes('0751') && ks.includes('0860') && !ks.includes(null), p + ': ' + ks.join());
    }
    assert(of(reqs, '/api/analytics/dagi-unpositioned').length === 1, 'count requests: ' + of(reqs, '/api/analytics/dagi-unpositioned').length);
    await ctx.close();
  });

  await step('no console errors', async () => { assert(!errors.length, errors.slice(0, 3).join(' | ')); });

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
