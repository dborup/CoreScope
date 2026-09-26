/**
 * #2073 — Recent Adverts split by route class (port/extension of upstream
 * `Kpa-clawbot/CoreScope#2073`, title from `#2071`), on the full node page,
 * in the side panel and at phone width.
 *
 * Needs the fixture seeded with test-fixtures/seed-2073-route-adverts.sql
 * (CI applies it after migrating the fixture). Never run against prod.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-issue-2073-recent-adverts-e2e.js
 * SCREENSHOT_DIR=<dir> also saves screenshots of each state.
 */
'use strict';

const path = require('path');
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const SHOTS = process.env.SCREENSHOT_DIR || '';
const MIX = '2073e2e000000000000000000000000000000000000000000000000000000001';
const ZH_ONLY = '2073e2e000000000000000000000000000000000000000000000000000000002';

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }
// Screenshots wait out the page fade-in and the .tab-btn color transition.
async function shot(page, name, selector) {
  if (!SHOTS) return;
  await page.waitForTimeout(800);
  if (selector) await page.$eval(selector, (el) => el.scrollIntoView({ block: 'start' }));
  await page.screenshot({ path: path.join(SHOTS, '2073-' + name + '.png'), fullPage: false });
}

// State of the panel inside root: title, counts text, tabs, visible entries.
async function panelState(page, root) {
  return page.$eval(root, (el) => {
    const bar = el.querySelector('.node-adverts-tabs');
    const tabs = bar ? Array.from(bar.querySelectorAll('[role="tab"]')) : [];
    const visible = Array.from(el.querySelectorAll('.node-adverts-panel')).filter(p => !p.hidden);
    const entries = visible.length ? Array.from(visible[0].querySelectorAll('.node-activity-item, .advert-entry')) : [];
    const h4 = el.querySelector('h4');
    const active = document.activeElement;
    return {
      title: h4 ? h4.textContent.trim() : '',
      tip: h4 ? h4.getAttribute('title') || '' : '',
      counts: (el.querySelector('.node-adverts-counts') || { textContent: '' }).textContent.replace(/\s+/g, ' ').trim(),
      note: !!el.querySelector('.node-adverts-note'),
      barRole: bar ? bar.getAttribute('role') : null,
      tabs: tabs.map(t => ({ key: t.dataset.advertsTab, selected: t.getAttribute('aria-selected'), tabindex: t.getAttribute('tabindex') })),
      visiblePanels: visible.map(p => p.id),
      entries: entries.length,
      badges: visible.length ? Array.from(visible[0].querySelectorAll('.advert-route-badge')).map(b => b.dataset.routeClass) : [],
      empty: visible.length ? (visible[0].querySelector('.node-adverts-empty') || { textContent: '' }).textContent.trim() : '',
      hrefs: entries.map(e => (e.querySelector('a.ch-analyze-link') || {}).getAttribute ? e.querySelector('a.ch-analyze-link').getAttribute('href') : ''),
      focusedTab: active && active.dataset ? active.dataset.advertsTab || null : null,
      focusVisible: !!(active && active.matches && active.matches(':focus-visible')),
    };
  });
}
const selectedTab = (s) => (s.tabs.find(t => t.selected === 'true') || {}).key;

// The first theme load re-renders the node views once (theme-refresh →
// loadFullNode / selectNode, same as on master), replacing the tab bar. Wait
// for that and for the tab bar to stay the same element before interacting.
const THEME_FLAG = () => { window.addEventListener('theme-refresh', () => { window.__2073ThemeReady = true; }, { once: true }); };
async function waitSettled(page, root) {
  const sel = root + ' .node-adverts-tabs[role="tablist"]';
  await page.waitForFunction(() => window.__2073ThemeReady === true);
  await page.waitForFunction((s) => {
    const el = document.querySelector(s);
    if (!el) return false;
    if (window.__2073Stable !== el) { window.__2073Stable = el; window.__2073Since = performance.now(); return false; }
    return performance.now() - window.__2073Since > 400;
  }, sel, { polling: 100 });
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  const ctx = await browser.newContext({ viewport: { width: 1440, height: 1000 } });
  await ctx.addInitScript(THEME_FLAG);
  const page = await ctx.newPage();
  page.setDefaultTimeout(15000);
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  // Node-detail requests (GET /api/nodes/{pubkey}, no sub-path) the node
  // page makes: all of them must opt in to the breakdown.
  const detailRequests = [];
  page.on('request', (req) => {
    const u = new URL(req.url());
    if (/^\/api\/nodes\/[0-9a-f]{64}$/.test(u.pathname)) detailRequests.push(u.pathname + u.search);
  });

  console.log(`\n=== #2073 Recent Adverts E2E against ${BASE} ===`);
  const full = '#node-packets';

  await step('full page: title, tooltip, counts and default All tab', async () => {
    await page.goto(BASE + '/#/nodes/' + MIX, { waitUntil: 'domcontentloaded' });
    await waitSettled(page, full);
    const s = await panelState(page, full);
    assert(s.title === 'Recent Adverts (7)', 'title: ' + s.title);
    assert(s.tip.includes('from_pubkey'), 'tooltip explains the advert-only list');
    assert(s.counts.includes('24h Flood 2 · Zero-hop 1 · Mixed 1'), 'counts: ' + s.counts);
    assert(s.counts.includes('7d Flood 3 · Zero-hop 2 · Mixed 2'), 'counts: ' + s.counts);
    assert(!/Unknown/.test(s.counts), 'unknown hidden at zero');
    assert(s.note, 'provisional note while the route_mask backfill is pending');
    assert(s.tabs.map(t => t.key).join() === 'all,flood,zero_hop,mixed', 'tabs: ' + JSON.stringify(s.tabs));
    assert(selectedTab(s) === 'all' && s.entries === 7, 'All selected with 7 adverts');
    assert(s.badges.length === 7 && s.badges.filter(b => b === 'mixed').length === 2, 'route badge per advert: ' + s.badges);
    await shot(page, 'full-all-light', full);
  });

  await step('full page: Flood tab lists every flood advert and deep-links', async () => {
    await page.click(full + ' [data-adverts-tab="flood"]');
    const s = await panelState(page, full);
    assert(selectedTab(s) === 'flood' && s.visiblePanels.join() === 'nodeFullAdverts-panel-flood', 'flood panel shown');
    assert(s.entries === 3, 'flood adverts: ' + s.entries);
    assert(s.hrefs.some(h => h.endsWith('e2e2073flood0002')), 'transport-flood advert listed');
    assert(s.hrefs.some(h => h.endsWith('e2e2073legacy001')), 'legacy NULL-mask advert listed via route_type');
    assert(!s.hrefs.some(h => /mixed/.test(h)), 'mixed advert never under Flood');
    assert(new URL(page.url()).hash === '#/nodes/' + MIX + '?adverts=flood', 'hash: ' + new URL(page.url()).hash);
    await shot(page, 'full-flood-light', full);
  });

  await step('full page: reload restores the tab from the URL', async () => {
    await page.reload({ waitUntil: 'domcontentloaded' });
    await waitSettled(page, full);
    const s = await panelState(page, full);
    assert(selectedTab(s) === 'flood' && s.entries === 3, 'restored: ' + JSON.stringify(s.tabs));
  });

  await step('full page: arrow keys move between tabs with visible focus', async () => {
    await page.focus(full + ' [data-adverts-tab="flood"]');
    await page.keyboard.press('ArrowRight');
    let s = await panelState(page, full);
    assert(selectedTab(s) === 'zero_hop' && s.focusedTab === 'zero_hop' && s.entries === 2, 'ArrowRight → Zero-hop');
    assert(s.focusVisible, 'focused tab matches :focus-visible');
    assert(s.tabs.filter(t => t.tabindex === '0').length === 1, 'roving tabindex');
    await page.keyboard.press('ArrowRight');
    s = await panelState(page, full);
    assert(selectedTab(s) === 'mixed' && s.entries === 2, 'ArrowRight → Mixed');
    assert(new URL(page.url()).hash.endsWith('?adverts=mixed'), 'hash follows the keyboard');
    await shot(page, 'full-mixed-light', full);
    await page.keyboard.press('Home');
    s = await panelState(page, full);
    assert(selectedTab(s) === 'all' && new URL(page.url()).hash === '#/nodes/' + MIX, 'Home → All drops the param');
  });

  await step('full page: empty state explains itself', async () => {
    await page.goto(BASE + '/#/nodes/' + ZH_ONLY + '?adverts=flood', { waitUntil: 'domcontentloaded' });
    await waitSettled(page, full);
    const s = await panelState(page, full);
    assert(selectedTab(s) === 'flood' && s.entries === 0, 'flood tab empty');
    assert(s.empty === 'No flood adverts from this node on record', 'empty text: ' + s.empty);
    await shot(page, 'full-flood-empty-light', full);
  });

  await step('full page: dark theme renders', async () => {
    // The app keeps the chosen theme in localStorage (app.js).
    await page.evaluate(() => localStorage.setItem('meshcore-theme', 'dark'));
    await page.goto(BASE + '/#/nodes/' + MIX + '?adverts=mixed', { waitUntil: 'domcontentloaded' });
    await page.reload({ waitUntil: 'domcontentloaded' });
    await waitSettled(page, full);
    assert(await page.evaluate(() => document.documentElement.getAttribute('data-theme')) === 'dark', 'dark theme active');
    const s = await panelState(page, full);
    assert(selectedTab(s) === 'mixed' && s.entries === 2, 'mixed in dark');
    await shot(page, 'full-mixed-dark', full);
    await page.click(full + ' [data-adverts-tab="all"]');
    await shot(page, 'full-all-dark', full);
    await page.evaluate(() => localStorage.setItem('meshcore-theme', 'light'));
  });

  await step('side panel: same component, tabs and deep link', async () => {
    await page.goto(BASE + '/#/nodes?search=' + encodeURIComponent('Route Mix E2E'), { waitUntil: 'domcontentloaded' });
    await page.click('tr[data-key="' + MIX + '"]');
    const pane = '#node-pane-adverts';
    await waitSettled(page, pane);
    let s = await panelState(page, pane);
    assert(s.title === 'Recent Adverts (7)' && s.counts.includes('24h Flood 2 · Zero-hop 1 · Mixed 1'), 'pane title/counts');
    assert(selectedTab(s) === 'all' && s.entries === 7, 'pane All');
    await shot(page, 'pane-all-light');
    await page.click(pane + ' [data-adverts-tab="mixed"]');
    s = await panelState(page, pane);
    assert(selectedTab(s) === 'mixed' && s.entries === 2, 'pane Mixed');
    assert(new URL(page.url()).hash === '#/nodes/' + MIX + '?adverts=mixed', 'pane hash: ' + new URL(page.url()).hash);
    await shot(page, 'pane-mixed-light');
  });

  await step('opt-in: the node page asks for the breakdown, the plain URL stays master-shaped', async () => {
    assert(detailRequests.length > 0 && detailRequests.every(u => /[?&]include=advertRoutes(&|$)/.test(u)),
      'node page detail requests: ' + detailRequests.join(' '));
    const r = await page.evaluate(async (pk) => {
      const plain = await (await fetch('/api/nodes/' + pk)).text();
      const opted = await (await fetch('/api/nodes/' + pk + '?include=advertRoutes')).text();
      return {
        plainKeys: Object.keys(JSON.parse(plain)).sort().join(),
        plainRouteClass: plain.includes('route_class'),
        optedKeys: Object.keys(JSON.parse(opted)).sort().join(),
        optedRouteClass: opted.includes('"route_class"'),
      };
    }, MIX);
    assert(r.plainKeys === 'node,recentAdverts' && !r.plainRouteClass, 'plain: ' + JSON.stringify(r));
    assert(r.optedKeys === 'advertCounts,node,recentAdverts,recentAdvertsByRoute' && r.optedRouteClass, 'opted in: ' + JSON.stringify(r));
  });

  await step('mobile 390×844: no horizontal overflow, tabs usable', async () => {
    const m = await browser.newContext({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });
    await m.addInitScript(THEME_FLAG);
    const mp = await m.newPage();
    mp.on('pageerror', (e) => errors.push(e.message));
    await mp.goto(BASE + '/#/nodes/' + MIX, { waitUntil: 'domcontentloaded' });
    await waitSettled(mp, full);
    await mp.$eval(full, (el) => el.scrollIntoView());
    for (const tab of ['all', 'flood', 'zero_hop', 'mixed']) {
      await mp.tap(full + ' [data-adverts-tab="' + tab + '"]');
      const o = await mp.evaluate((sel) => {
        const card = document.querySelector(sel);
        return { doc: document.documentElement.scrollWidth - document.documentElement.clientWidth, card: card.scrollWidth - card.clientWidth };
      }, full);
      assert(o.doc <= 0 && o.card <= 0, tab + ': horizontal overflow ' + JSON.stringify(o));
      if (tab === 'all' || tab === 'flood') await shot(mp, 'mobile-' + tab, full);
    }
    const s = await panelState(mp, full);
    assert(selectedTab(s) === 'mixed' && s.entries === 2, 'mobile Mixed');
    await m.close();
  });

  await step('no page errors', async () => {
    assert(errors.length === 0, errors.join(' | '));
  });

  await browser.close();
  console.log(`\n${passed}/${passed + failed} passed`);
  process.exit(failed === 0 ? 0 : 1);
})().catch(e => { console.error(e); process.exit(2); });
