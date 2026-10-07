/**
 * E2E (#189): a packet group in the grouped packets view shows a right caret
 * while collapsed and a down caret while expanded, once, and its toggle row
 * reports the state in aria-expanded.
 *
 * Against the e2e fixture's seeded 3-observation group (see the "Seed
 * grouped-packet row for #1486" step in deploy.yml):
 * - collapsed: #ph-caret-right, aria-expanded="false";
 * - expanded (after a click): #ph-caret-down, aria-expanded="true";
 * - collapsed again (second click): back to #ph-caret-right and "false";
 * - in each state the expand cell holds exactly one caret and no CSS triangle
 *   (style.css used to add a second ▶/▼ through ::before).
 *
 * Usage: BASE_URL=http://localhost:13581 node test-issue-189-group-caret-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const SEED_HASH = 'fae0c9e6d357a814';
const ROW = `#pktBody tr[data-hash="${SEED_HASH}"][data-action="toggle-select"]`;

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

// What the user sees in the group row's first cell, and what the row reports.
async function rowState(page) {
  return page.evaluate((sel) => {
    const tr = document.querySelector(sel);
    const td = tr.querySelector('td.col-expand');
    return {
      expandedClass: tr.classList.contains('expanded'),
      aria: tr.getAttribute('aria-expanded'),
      carets: [...td.querySelectorAll('use')].map((u) => (u.getAttribute('href') || '').split('#')[1]),
      before: getComputedStyle(tr.querySelector('td:first-child'), '::before').content,
    };
  }, ROW);
}

function assertState(s, caret, aria) {
  assert(s.carets.length === 1 && s.carets[0] === caret, 'expand cell should hold exactly #' + caret + ', got ' + JSON.stringify(s.carets));
  assert(s.aria === aria, 'aria-expanded should be "' + aria + '", got ' + JSON.stringify(s.aria));
  assert(s.expandedClass === (aria === 'true'), 'the .expanded class should match aria-expanded');
  assert(s.before === 'none' || s.before === 'normal', 'no CSS triangle next to the caret, got ::before ' + s.before);
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  const ctx = await browser.newContext({ viewport: { width: 1400, height: 900 } });
  const page = await ctx.newPage();
  page.setDefaultTimeout(10000);
  page.on('pageerror', (e) => console.error('[pageerror]', e.message));

  console.log(`\n=== #189 packet group caret E2E against ${BASE} ===`);

  await step('the seeded group is listed, collapsed', async () => {
    await page.goto(BASE + '/#/packets?hash=' + SEED_HASH + '&timeWindow=0', { waitUntil: 'domcontentloaded' });
    await page.waitForSelector(ROW, { timeout: 12000 });
    await page.waitForTimeout(300);
    assertState(await rowState(page), 'ph-caret-right', 'false');
  });

  await step('clicking the group expands it: down caret, aria-expanded true', async () => {
    await page.click(ROW + ' td.col-expand');
    await page.waitForFunction((sel) => document.querySelector(sel).classList.contains('expanded'), ROW, { timeout: 8000 });
    assertState(await rowState(page), 'ph-caret-down', 'true');
  });

  await step('clicking again collapses it: right caret, aria-expanded false', async () => {
    await page.click(ROW + ' td.col-expand');
    await page.waitForFunction((sel) => !document.querySelector(sel).classList.contains('expanded'), ROW, { timeout: 8000 });
    assertState(await rowState(page), 'ph-caret-right', 'false');
  });

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed === 0 ? 0 : 1);
})().catch((e) => { console.error(e); process.exit(1); });
