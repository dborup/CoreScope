/**
 * E2E (#259 item 1): a packet group expanded on desktop must not leave visible
 * child rows behind when the layout crosses to the mobile breakpoint.
 *
 * At <= 600 px mobile-page-actions.js (#1461 #7) hides the expand column and
 * makes activating a group row select it, and #255 renders the row as
 * select-hash without aria-expanded. Before this fix the row still carried
 * class="expanded" and its child rows stayed on screen with nothing left to
 * collapse them: the dead end upstream #1461 #7 describes.
 *
 * The fix keeps the hash in expandedHashes but leaves the children out of the
 * rendered slice while the mobile mode is active, so:
 * - 1400 px: expanding shows the children and aria-expanded="true" (#248/#255);
 * - 390 px: no child rows, no expanded class, no aria-expanded;
 * - back at 1400 px: the children and the expanded state are back.
 *
 * Runs against the fixture's seeded 3-observation group (see the "Seed
 * grouped-packet row for #1486" step in deploy.yml).
 *
 * Usage: BASE_URL=http://localhost:13581 node test-issue-259-mobile-expanded-group-e2e.js
 * SCREENSHOT_DIR=<dir> also saves screenshots (390 px and 1400 px, light and dark).
 */
'use strict';
const path = require('path');
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const SHOTS = process.env.SCREENSHOT_DIR || '';
const SEED_HASH = 'fae0c9e6d357a814';
const ROW = `#pktBody tr[data-hash="${SEED_HASH}"]`;

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

async function shot(page, name) {
  if (!SHOTS) return;
  await page.waitForTimeout(400);
  await page.screenshot({ path: path.join(SHOTS, '259-' + name + '.png'), fullPage: false });
}

// The rendered state of the seeded group row, plus whether its children are
// actually on screen (getClientRects, not just attached) and whether anything
// is left to collapse the group with.
async function rowState(page) {
  return page.evaluate(([sel, hash]) => {
    const tr = document.querySelector(sel);
    if (!tr) return null;
    const kids = [...document.querySelectorAll('#pktBody tr.group-child[data-parent-hash="' + hash + '"]')];
    const expandCell = tr.querySelector('td.col-expand');
    return {
      action: tr.getAttribute('data-action'),
      aria: tr.getAttribute('aria-expanded'),
      expandedClass: tr.classList.contains('expanded'),
      children: kids.length,
      visibleChildren: kids.filter((k) => k.getClientRects().length > 0).length,
      expandCellVisible: !!expandCell && expandCell.getClientRects().length > 0,
      carets: expandCell ? [...expandCell.querySelectorAll('use')].map((u) => (u.getAttribute('href') || '').split('#')[1]) : [],
    };
  }, [ROW, SEED_HASH]);
}

async function waitRow(page, want) {
  try {
    await page.waitForFunction(([sel, w]) => {
      const tr = document.querySelector(sel);
      if (!tr) return false;
      const action = tr.getAttribute('data-action');
      return w === 'desktop' ? action === 'toggle-select' : action === 'select-hash';
    }, [ROW, want], { timeout: 5000 });
  } catch (_) { /* reported below with the actual state */ }
  const s = await rowState(page);
  const action = want === 'desktop' ? 'toggle-select' : 'select-hash';
  assert(s && s.action === action, 'expected the ' + want + ' row, got ' + JSON.stringify(s));
  return s;
}

async function openPackets(page) {
  await page.goto(BASE + '/#/packets?hash=' + SEED_HASH + '&timeWindow=0', { waitUntil: 'domcontentloaded' });
  await page.waitForSelector(ROW, { timeout: 12000 });
  await page.waitForTimeout(300);
}

async function expandAtDesktop(page) {
  await waitRow(page, 'desktop');
  await page.click(ROW + ' td.col-expand');
  await page.waitForFunction((sel) => document.querySelector(sel).classList.contains('expanded'), ROW, { timeout: 8000 });
}

async function run(browser, theme) {
  const ctx = await browser.newContext({ viewport: { width: 1400, height: 900 } });
  await ctx.addInitScript((t) => { localStorage.setItem('meshcore-theme', t); }, theme);
  const page = await ctx.newPage();
  page.setDefaultTimeout(10000);
  page.on('pageerror', (e) => console.error('[pageerror]', e.message));

  await step(`[${theme}] 1400 px: expanding the group shows its children`, async () => {
    await openPackets(page);
    await expandAtDesktop(page);
    const s = await rowState(page);
    assert(s.aria === 'true', 'aria-expanded="true" on desktop: ' + JSON.stringify(s));
    assert(s.expandedClass, 'the row is marked expanded');
    assert(s.visibleChildren > 0, 'the children are on screen: ' + JSON.stringify(s));
    assert(s.carets.length === 1 && s.carets[0] === 'ph-caret-down', 'down caret while expanded: ' + JSON.stringify(s.carets));
    await shot(page, '1400-expanded-' + theme);
  });

  await step(`[${theme}] resize 1400 -> 390: no child row is left visible`, async () => {
    await page.setViewportSize({ width: 390, height: 844 });
    await waitRow(page, 'mobile');
    const s = await rowState(page);
    assert(s.visibleChildren === 0, 'no visible child rows on mobile: ' + JSON.stringify(s));
    assert(s.children === 0, 'the child rows are not rendered at all: ' + JSON.stringify(s));
    assert(!s.expandedClass, 'the row does not claim to be expanded: ' + JSON.stringify(s));
    assert(s.aria === null, 'and it announces no expanded state (#255)');
    assert(!s.expandCellVisible, 'the expand column is hidden at 390 px, as before');
    await shot(page, '390-after-resize-' + theme);
  });

  await step(`[${theme}] resize 390 -> 1400: the children and the state come back`, async () => {
    await page.setViewportSize({ width: 1400, height: 900 });
    await waitRow(page, 'desktop');
    const s = await rowState(page);
    assert(s.aria === 'true', 'the expansion was kept, not cleared: ' + JSON.stringify(s));
    assert(s.expandedClass, 'the row is marked expanded again');
    assert(s.visibleChildren > 0, 'the children are back: ' + JSON.stringify(s));
    assert(s.carets.length === 1 && s.carets[0] === 'ph-caret-down', 'down caret again: ' + JSON.stringify(s.carets));
  });

  await step(`[${theme}] 1400 px: the group still collapses from the caret`, async () => {
    await page.click(ROW + ' td.col-expand');
    await page.waitForFunction((sel) => !document.querySelector(sel).classList.contains('expanded'), ROW, { timeout: 8000 });
    const s = await rowState(page);
    assert(s.aria === 'false' && s.children === 0, 'collapsed again: ' + JSON.stringify(s));
    assert(s.carets.length === 1 && s.carets[0] === 'ph-caret-right', 'right caret while collapsed: ' + JSON.stringify(s.carets));
  });

  await ctx.close();
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });

  console.log(`\n=== #259 expanded group across the mobile breakpoint, against ${BASE} ===`);

  await run(browser, 'light');
  await run(browser, 'dark');

  // A touch context that starts at 390 px: the same group deep-linked and
  // expanded by the URL must not render children there either. Reaching the
  // row by hash keeps the state out of a desktop render entirely.
  {
    const ctx = await browser.newContext({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });
    const page = await ctx.newPage();
    page.setDefaultTimeout(10000);
    page.on('pageerror', (e) => console.error('[pageerror]', e.message));

    await step('390 px touch: a tap selects and leaves no child rows behind', async () => {
      await openPackets(page);
      await waitRow(page, 'mobile');
      await page.tap(ROW + ' td.col-time');
      await page.waitForFunction(() => {
        const s = document.getElementById('mobileDetailSheet');
        return !!s && s.classList.contains('open');
      }, null, { timeout: 8000 });
      const s = await rowState(page);
      assert(s.visibleChildren === 0 && !s.expandedClass, 'no children after a tap: ' + JSON.stringify(s));
      await shot(page, '390-touch-sheet');
    });
    await ctx.close();
  }

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed === 0 ? 0 : 1);
})().catch((e) => { console.error(e); process.exit(1); });
