/**
 * #245 M1 — estimated flood / zero-hop advert intervals on node detail
 * (Recent Adverts section, full page, side panel and phone width).
 *
 * Needs the fixture seeded with test-fixtures/seed-245-advert-intervals.sql
 * (CI applies it after migrating the fixture). Never run against prod.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-issue-245-advert-intervals-e2e.js
 * SCREENSHOT_DIR=<dir> also saves screenshots of each state.
 */
'use strict';

const path = require('path');
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const SHOTS = process.env.SCREENSHOT_DIR || '';
const BOTH = '245e2e0000000000000000000000000000000000000000000000000000000001';
const FLOOD_ONLY = '245e2e0000000000000000000000000000000000000000000000000000000002';

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }
async function shot(page, name, selector) {
  if (!SHOTS) return;
  await page.waitForTimeout(800);
  if (selector) await page.$eval(selector, (el) => el.scrollIntoView({ block: 'start' }));
  await page.screenshot({ path: path.join(SHOTS, '245-' + name + '.png'), fullPage: false });
}

// The interval rows inside root, by class, as the user reads them.
async function intervals(page, root) {
  await page.waitForSelector(root + ' .node-adverts-intervals');
  return page.$eval(root, (el) => {
    const out = {};
    el.querySelectorAll('.node-adverts-interval-row').forEach((r) => {
      out[r.dataset.advertInterval] = r.textContent.replace(/\s+/g, ' ').trim();
    });
    const block = el.querySelector('.node-adverts-intervals');
    out.tip = block.getAttribute('title') || '';
    out.afterCounts = !!(el.querySelector('.node-adverts-counts') &&
      (el.querySelector('.node-adverts-counts').compareDocumentPosition(block) & Node.DOCUMENT_POSITION_FOLLOWING));
    return out;
  });
}

const FLOOD_12H = 'Estimated flood interval ≈ 12 h (10 adverts, high confidence)';
const ZH_120 = 'Estimated zero-hop interval ≈ 120 min (6 adverts, medium confidence)';
const FLOOD_24H = 'Estimated flood interval ≈ 24 h (8 adverts, high confidence)';
const ZH_NONE = 'Estimated zero-hop interval: none observed (off, or no observer in direct range)';

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
  const full = '#node-packets';

  console.log(`\n=== #245 advert intervals E2E against ${BASE} ===`);

  await step('API: advertIntervals from the seeded gaps, only with include=advertRoutes', async () => {
    await page.goto(BASE + '/', { waitUntil: 'domcontentloaded' });
    const r = await page.evaluate(async (pks) => {
      const get = async (u) => (await fetch(u)).json();
      return {
        plain: 'advertIntervals' in (await get('/api/nodes/' + pks[0])),
        both: (await get('/api/nodes/' + pks[0] + '?include=advertRoutes')).advertIntervals,
        floodOnly: (await get('/api/nodes/' + pks[1] + '?include=advertRoutes')).advertIntervals,
      };
    }, [BOTH, FLOOD_ONLY]);
    assert(!r.plain, 'plain detail must not carry advertIntervals');
    const b = r.both, f = r.floodOnly;
    assert(b && b.window === 20, 'window: ' + JSON.stringify(b));
    assert(b.flood.interval_s === 43200 && b.flood.snapped && b.flood.samples === 10 && b.flood.gaps_used === 7 && b.flood.confidence === 'high',
      'flood (2x and 3x gaps, one manual advert): ' + JSON.stringify(b.flood));
    assert(b.zero_hop.interval_s === 7200 && b.zero_hop.samples === 6 && b.zero_hop.gaps_used === 5 && b.zero_hop.confidence === 'medium',
      'zero-hop: ' + JSON.stringify(b.zero_hop));
    assert(f.flood.interval_s === 86400 && f.flood.gaps_used === 7 && f.flood.confidence === 'high',
      'flood with a sender clock reset: ' + JSON.stringify(f.flood));
    assert(f.zero_hop.samples === 0 && f.zero_hop.interval_s === null && f.zero_hop.confidence === 'none' && f.zero_hop.last_advert === null,
      'no zero-hop: ' + JSON.stringify(f.zero_hop));
  });

  await step('full page: both classes under the counts', async () => {
    await page.goto(BASE + '/#/nodes/' + BOTH, { waitUntil: 'domcontentloaded' });
    const s = await intervals(page, full);
    assert(s.flood === FLOOD_12H, 'flood: ' + s.flood);
    assert(s.zero_hop === ZH_120, 'zero-hop: ' + s.zero_hop);
    assert(s.afterCounts, 'below the 24h / 7d counts');
    assert(/median/.test(s.tip) && /3-168 h/.test(s.tip), 'tooltip explains the method: ' + s.tip);
    await shot(page, 'full-both-light', full);
  });

  await step('full page: no zero-hop adverts reads as off or out of range', async () => {
    await page.goto(BASE + '/#/nodes/' + FLOOD_ONLY, { waitUntil: 'domcontentloaded' });
    const s = await intervals(page, full);
    assert(s.flood === FLOOD_24H, 'flood: ' + s.flood);
    assert(s.zero_hop === ZH_NONE, 'zero-hop: ' + s.zero_hop);
    await shot(page, 'full-flood-only-light', full);
  });

  await step('full page: dark theme', async () => {
    await page.evaluate(() => localStorage.setItem('meshcore-theme', 'dark'));
    await page.goto(BASE + '/#/nodes/' + BOTH, { waitUntil: 'domcontentloaded' });
    await page.reload({ waitUntil: 'domcontentloaded' });
    const s = await intervals(page, full);
    assert(await page.evaluate(() => document.documentElement.getAttribute('data-theme')) === 'dark', 'dark theme active');
    assert(s.flood === FLOOD_12H, 'flood in dark: ' + s.flood);
    const colors = await page.$eval(full + ' .node-adverts-intervals', (el) => {
      const cs = getComputedStyle(el), strong = getComputedStyle(el.querySelector('strong'));
      return { text: cs.color, label: strong.color, bg: getComputedStyle(document.body).backgroundColor };
    });
    assert(colors.text !== colors.bg && colors.label !== colors.bg, 'readable in dark: ' + JSON.stringify(colors));
    await shot(page, 'full-both-dark', full);
    await page.evaluate(() => localStorage.setItem('meshcore-theme', 'light'));
  });

  await step('side panel: same text', async () => {
    await page.goto(BASE + '/#/nodes?search=' + encodeURIComponent('Advert Interval E2E'), { waitUntil: 'domcontentloaded' });
    await page.click('tr[data-key="' + BOTH + '"]');
    const s = await intervals(page, '#node-pane-adverts');
    assert(s.flood === FLOOD_12H && s.zero_hop === ZH_120, 'pane: ' + JSON.stringify(s));
    await shot(page, 'pane-both-light');
  });

  await step('mobile 390×844: no horizontal overflow', async () => {
    const m = await browser.newContext({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });
    const mp = await m.newPage();
    mp.on('pageerror', (e) => errors.push(e.message));
    await mp.goto(BASE + '/#/nodes/' + FLOOD_ONLY, { waitUntil: 'domcontentloaded' });
    const s = await intervals(mp, full);
    assert(s.zero_hop === ZH_NONE, 'mobile zero-hop: ' + s.zero_hop);
    const o = await mp.evaluate((sel) => {
      const card = document.querySelector(sel);
      return { doc: document.documentElement.scrollWidth - document.documentElement.clientWidth, card: card.scrollWidth - card.clientWidth };
    }, full);
    assert(o.doc <= 0 && o.card <= 0, 'horizontal overflow ' + JSON.stringify(o));
    await shot(mp, 'mobile-flood-only', full + ' .node-adverts-intervals');
    await m.close();
  });

  await step('no page errors', async () => {
    assert(errors.length === 0, errors.join(' | '));
  });

  await browser.close();
  console.log(`\n${passed}/${passed + failed} passed`);
  process.exit(failed === 0 ? 0 : 1);
})().catch(e => { console.error(e); process.exit(2); });
