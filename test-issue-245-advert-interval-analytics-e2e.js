/**
 * #245 M2 — Analytics → Advert Intervals: mesh-wide estimated flood /
 * zero-hop advert intervals by role and region, nodes without zero-hop
 * adverts, flood re-arm notes and outliers (very frequent adverts).
 *
 * Needs the fixture seeded with test-fixtures/seed-245-advert-intervals.sql
 * (CI applies it after migrating the fixture; the M2 rows are at its end).
 * Never run against prod.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-issue-245-advert-interval-analytics-e2e.js
 * SCREENSHOT_DIR=<dir> also saves screenshots of each state.
 */
'use strict';

const path = require('path');
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const SHOTS = process.env.SCREENSHOT_DIR || '';
const pk = (n) => '245e2e00000000000000000000000000000000000000000000000000000000' + String(n).padStart(2, '0');
const DEFAULT_ZH = pk(3), FAST_FLOOD = pk(4), SHORT_FLOOD = pk(5), BOTH = pk(1);
const TAB = '#/analytics?tab=advert-intervals';

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }
async function shot(page, name) {
  if (!SHOTS) return;
  await page.waitForTimeout(500);
  await page.screenshot({ path: path.join(SHOTS, '245-m2-' + name + '.png'), fullPage: true });
}

// The rendered tab as the user reads it.
async function readTab(page) {
  await page.waitForSelector('[data-testid="advert-intervals"]');
  return page.evaluate(() => {
    const root = document.querySelector('[data-testid="advert-intervals"]');
    const txt = (el) => (el ? el.textContent.replace(/\s+/g, ' ').trim() : '');
    const stat = (id) => Number(txt(root.querySelector('[data-ai-stat="' + id + '"] .stat-value')));
    const outliers = {};
    root.querySelectorAll('[data-ai-outlier]').forEach((r) => {
      outliers[r.dataset.aiOutlier] = { text: txt(r), href: r.querySelector('a') && r.querySelector('a').getAttribute('href') };
    });
    const groups = (sec) => Array.from(root.querySelectorAll('[data-ai-section="' + sec + '"] tr[data-ai-group]'), (r) => r.dataset.aiGroup);
    return {
      nodes: stat('nodes'), zhNotObserved: stat('zero_hop-not-observed'),
      zhNotObservedText: txt(root.querySelector('[data-ai-stat="zero_hop-not-observed"]')),
      floodDist: Array.from(root.querySelectorAll('[data-ai-section="dist-flood"] .ai-dist-row'), (r) => txt(r)),
      zhDist: Array.from(root.querySelectorAll('[data-ai-section="dist-zero_hop"] .ai-dist-row'), (r) => txt(r)),
      reArm: txt(root.querySelector('[data-ai-section="re-arm"]')),
      roles: groups('by-role'), regions: groups('by-region'),
      outliers,
      activeTab: (document.querySelector('#analyticsTabs .tab-btn.active') || {}).dataset,
    };
  });
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  const ctx = await browser.newContext({ viewport: { width: 1440, height: 1000 } });
  const page = await ctx.newPage();
  page.setDefaultTimeout(15000);
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));

  console.log(`\n=== #245 M2 advert interval analytics E2E against ${BASE} ===`);

  await step('API: seeded nodes estimated, outliers, re-arm, region and area filters', async () => {
    await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
    const r = await page.evaluate(async () => {
      const get = async (u) => { const res = await fetch(u); return { status: res.status, body: await res.json() }; };
      return {
        all: (await get('/api/analytics/advert-intervals')).body,
        sjc: (await get('/api/analytics/advert-intervals?region=sjc')).body,
        badArea: (await get('/api/analytics/advert-intervals?area=no-such-area')).status,
      };
    });
    const a = r.all;
    assert(a.window === 20 && a.min_confidence === 'medium' && a.nodes >= 5, 'shape: ' + JSON.stringify({ window: a.window, nodes: a.nodes }));
    const flood = a.flood.buckets.map((b) => b.interval_s);
    for (const v of [10800, 43200, 86400, 169200]) assert(flood.includes(v), 'flood bucket ' + v + ' in ' + flood);
    const zh = a.zero_hop.buckets.map((b) => b.interval_s);
    assert(zh.includes(120) && zh.includes(7200), 'zero-hop buckets ' + zh);
    const byKey = (list) => Object.fromEntries(list.map((o) => [o.public_key + '/' + o.class, o]));
    const out = byKey(a.outliers);
    const d = out[DEFAULT_ZH + '/zero_hop'], f = out[FAST_FLOOD + '/flood'];
    assert(d && d.reason === 'zero_hop_new_install_default' && d.median_gap_s === 120 && d.regions.join() === 'SJC', 'default outlier: ' + JSON.stringify(d));
    assert(f && f.reason === 'flood_faster_than_settable' && f.median_gap_s === 600 && f.regions.join() === 'OAK', 'fast flood outlier: ' + JSON.stringify(f));
    assert(!out[BOTH + '/flood'] && !out[SHORT_FLOOD + '/flood'], 'regular nodes are not outliers');
    assert(a.flood_re_arm.zero_hop_maybe_suppressed >= 1, 're-arm: ' + JSON.stringify(a.flood_re_arm));
    assert(a.scan.generated_at && a.scan.truncated === false, 'scan: ' + JSON.stringify(a.scan));
    const sjc = byKey(r.sjc.outliers);
    assert(sjc[DEFAULT_ZH + '/zero_hop'] && !sjc[FAST_FLOOD + '/flood'], 'region=sjc keeps SJC nodes only: ' + Object.keys(sjc));
    assert(r.sjc.nodes < a.nodes && r.sjc.by_region.some((g) => g.key === 'SJC'), 'region=sjc narrows: ' + r.sjc.nodes + ' of ' + a.nodes);
    assert(r.badArea === 400, 'unknown area: ' + r.badArea);
  });

  await step('deep link #/analytics?tab=advert-intervals renders the view', async () => {
    await page.goto(BASE + '/' + TAB, { waitUntil: 'domcontentloaded' });
    const s = await readTab(page);
    assert(s.activeTab && s.activeTab.tab === 'advert-intervals', 'active tab: ' + JSON.stringify(s.activeTab));
    assert(s.nodes >= 5, 'nodes card: ' + s.nodes);
    assert(/Zero-hop not observed/.test(s.zhNotObservedText) && /off, or no observer in direct range/.test(s.zhNotObservedText), s.zhNotObservedText);
    assert(s.floodDist.some((t) => /^47 h/.test(t)) && s.floodDist.some((t) => /^3 h/.test(t)), 'flood distribution: ' + s.floodDist);
    assert(s.zhDist.some((t) => /^2 min/.test(t)) && s.zhDist.some((t) => /^120 min/.test(t)), 'zero-hop distribution: ' + s.zhDist);
    assert(/Zero-hop maybe suppressed by flood [1-9]/.test(s.reArm), 're-arm: ' + s.reArm);
    assert(s.roles.includes('repeater'), 'by role: ' + s.roles);
    assert(s.regions.includes('SJC') && s.regions.includes('OAK'), 'by region: ' + s.regions);
    const d = s.outliers[DEFAULT_ZH], f = s.outliers[FAST_FLOOD];
    assert(d && /Zero-hop Default E2E/.test(d.text) && /new-install default/.test(d.text) && d.href === '#/nodes/' + DEFAULT_ZH, 'default row: ' + JSON.stringify(d));
    assert(f && /Fast Flood E2E/.test(f.text) && /10 min/.test(f.text) && /more often than every 3 h/.test(f.text), 'fast flood row: ' + JSON.stringify(f));
    await shot(page, 'desktop-light');
  });

  await step('tab click keeps ?tab= in the URL; an outlier links to its node', async () => {
    await page.goto(BASE + '/#/analytics', { waitUntil: 'domcontentloaded' });
    await page.click('#analyticsTabs .tab-btn[data-tab="advert-intervals"]');
    await readTab(page);
    assert(/[?&]tab=advert-intervals\b/.test(await page.evaluate(() => location.hash)), 'hash after click');
    await page.click('[data-ai-outlier="' + DEFAULT_ZH + '"] a');
    await page.waitForFunction((k) => location.hash === '#/nodes/' + k, DEFAULT_ZH);
  });

  await step('region deep link narrows the view', async () => {
    await page.goto(BASE + '/' + TAB + '&region=SJC', { waitUntil: 'domcontentloaded' });
    await page.waitForFunction((keys) => document.querySelector('[data-ai-outlier="' + keys[0] + '"]') &&
      !document.querySelector('[data-ai-outlier="' + keys[1] + '"]'), [DEFAULT_ZH, FAST_FLOOD]);
    const s = await readTab(page);
    assert(s.outliers[DEFAULT_ZH] && !s.outliers[FAST_FLOOD], 'SJC outliers: ' + Object.keys(s.outliers));
    assert(s.regions[0] === 'SJC', 'SJC first: ' + s.regions);
  });

  await step('dark theme: bars use the theme accent, text readable', async () => {
    await page.evaluate(() => localStorage.setItem('meshcore-theme', 'dark'));
    await page.goto(BASE + '/' + TAB, { waitUntil: 'domcontentloaded' });
    await page.reload({ waitUntil: 'domcontentloaded' });
    await readTab(page);
    const c = await page.evaluate(() => {
      const fill = document.querySelector('.ai-dist-fill');
      const probe = document.createElement('span');
      probe.style.background = 'var(--accent)';
      document.body.appendChild(probe);
      const accent = getComputedStyle(probe).backgroundColor;
      probe.remove();
      return {
        theme: document.documentElement.getAttribute('data-theme'),
        fill: getComputedStyle(fill).backgroundColor, accent,
        text: getComputedStyle(document.querySelector('.ai-dist-row')).color,
        bg: getComputedStyle(document.querySelector('[data-ai-section="dist-flood"]')).backgroundColor,
      };
    });
    assert(c.theme === 'dark', 'dark theme active');
    assert(c.fill === c.accent, 'bar = --accent: ' + JSON.stringify(c));
    assert(c.text !== c.bg, 'readable: ' + JSON.stringify(c));
    await shot(page, 'desktop-dark');
    await page.evaluate(() => localStorage.setItem('meshcore-theme', 'light'));
  });

  await step('mobile 390×844: no horizontal page overflow, tables scroll in place', async () => {
    const m = await browser.newContext({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });
    const mp = await m.newPage();
    mp.on('pageerror', (e) => errors.push(e.message));
    await mp.goto(BASE + '/' + TAB, { waitUntil: 'domcontentloaded' });
    const s = await readTab(mp);
    assert(s.outliers[DEFAULT_ZH], 'mobile outliers');
    await mp.waitForFunction(() => document.querySelectorAll('[data-testid="advert-intervals"] .analytics-table-scroll .analytics-table').length === 3);
    const o = await mp.evaluate(() => ({
      doc: document.documentElement.scrollWidth - document.documentElement.clientWidth,
      cards: Array.from(document.querySelectorAll('[data-testid="advert-intervals"] .analytics-card'),
        (c) => c.getBoundingClientRect().right - document.documentElement.clientWidth).filter((d) => d > 1),
    }));
    assert(o.doc <= 0 && o.cards.length === 0, 'horizontal overflow ' + JSON.stringify(o));
    await shot(mp, 'mobile');
    await m.close();
  });

  await step('no page errors', async () => {
    assert(errors.length === 0, errors.join(' | '));
  });

  await browser.close();
  console.log(`\n${passed}/${passed + failed} passed`);
  process.exit(failed === 0 ? 0 : 1);
})().catch(e => { console.error(e); process.exit(2); });
