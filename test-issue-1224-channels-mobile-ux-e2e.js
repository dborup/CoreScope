/**
 * E2E (#1224): Channels page mobile UX overhaul.
 *
 * At 375x800 viewport the channels page must:
 *  - Render a compact header strip above the channel list: at most two rows
 *    (title + Add chip, then the region filter; ≤120px since #235 made the
 *    controls 48px), with every control inside the strip and the list
 *    starting below it. The strip used to be capped at 56px, which kept it
 *    under the old ≤60px bound while the list was drawn over the pills.
 *  - Render "+ Add Channel" as a compact chip — NOT a full-width hero (the
 *    add control must be narrower than 65% of the sidebar width).
 *  - Render channel rows where the channel name has computed-width > 150px
 *    (the row must not be clipped by oversized inline action buttons).
 *  - Render the "Select a channel" empty state container occupying < 40% of
 *    the viewport height (no desktop-thinking empty state on mobile).
 *
 * Run: BASE_URL=http://localhost:13581 node test-issue-1224-channels-mobile-ux-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  \u2713 ' + name); }
  catch (e) { failed++; console.error('  \u2717 ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

async function run() {
  const launchOpts = { args: ['--no-sandbox'] };
  if (process.env.CHROMIUM_PATH) launchOpts.executablePath = process.env.CHROMIUM_PATH;
  const browser = await chromium.launch(launchOpts);
  const ctx = await browser.newContext({ viewport: { width: 375, height: 800 } });
  const page = await ctx.newPage();

  await page.goto(BASE + '/#/channels', { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#chList', { timeout: 10000 });
  await page.waitForFunction(() => {
    const l = document.getElementById('chList');
    // #1367: mobile now renders flat .ch-row entries; older .ch-item
    // markup still ships on desktop. Accept either so this regression
    // test keeps gating the header/empty-state/name-width invariants
    // (which apply to both layouts) without pinning the row markup.
    return l && l.querySelectorAll('.ch-item, .ch-row').length > 0;
  }, { timeout: 15000 });
  await page.waitForTimeout(300);

  await step('header strip is \u2264120px, holds all its controls, and the list starts below it', async () => {
    const r = await page.evaluate(() => {
      const sidebar = document.querySelector('.ch-sidebar');
      const header = sidebar && sidebar.querySelector('.ch-sidebar-header');
      if (!header) return null;
      const h = header.getBoundingClientRect();
      const listTop = document.getElementById('chList').getBoundingClientRect().top;
      const outside = [...header.querySelectorAll('.ch-sidebar-title, button')]
        .map((e) => ({ name: e.id || String(e.className).split(' ')[0] || e.tagName, b: e.getBoundingClientRect() }))
        .filter((c) => c.b.width > 0 && (c.b.top < h.top - 0.5 || c.b.bottom > h.bottom + 0.5 || c.b.left < h.left - 0.5 || c.b.right > h.right + 0.5))
        .map((c) => c.name + ' ' + Math.round(c.b.top) + '-' + Math.round(c.b.bottom));
      return { height: Math.round(h.height), top: Math.round(h.top), bottom: Math.round(h.bottom), listTop: Math.round(listTop), outside };
    });
    assert(r !== null, 'sidebar header not found');
    assert(r.height <= 120, 'sidebar header must be \u2264120px on mobile, got ' + r.height + 'px');
    assert(r.outside.length === 0, 'controls stick out of the header (' + r.top + '-' + r.bottom + '): ' + r.outside.join(', '));
    assert(r.listTop >= r.bottom - 0.5, 'channel list starts at ' + r.listTop + 'px, above the header bottom ' + r.bottom + 'px');
  });

  await step('"+ Add Channel" is a compact chip, not full-width hero', async () => {
    const ratio = await page.evaluate(() => {
      const sidebar = document.querySelector('.ch-sidebar');
      const btn = document.getElementById('chAddChannelBtn');
      if (!sidebar || !btn) return null;
      const sw = sidebar.getBoundingClientRect().width;
      const bw = btn.getBoundingClientRect().width;
      return bw / sw;
    });
    assert(ratio !== null, 'add channel button not found');
    assert(ratio < 0.65, 'add button width must be <65% of sidebar, got ratio=' + ratio.toFixed(2));
  });

  await step('first channel row name has computed-width >150px', async () => {
    const nameW = await page.evaluate(() => {
      // #1367: chat-app mobile row uses .ch-row + .ch-row-name. Fall back to
      // the legacy .ch-item .ch-item-name so this test still works on the
      // desktop layout / any regression that re-renders the old markup.
      const name = document.querySelector('#chList .ch-row .ch-row-name')
        || document.querySelector('#chList .ch-item .ch-item-name');
      if (!name) return null;
      return Math.round(name.getBoundingClientRect().width);
    });
    assert(nameW !== null, 'first channel name element not found');
    assert(nameW > 150, 'channel name width must be >150px on mobile, got ' + nameW + 'px');
  });

  await step('empty-state container is < 40% of viewport height', async () => {
    const data = await page.evaluate(() => {
      const empty = document.querySelector('.ch-empty');
      if (!empty) return null;
      return {
        h: Math.round(empty.getBoundingClientRect().height),
        vh: window.innerHeight,
      };
    });
    assert(data !== null, 'empty state element not found');
    const pct = data.h / data.vh;
    assert(pct < 0.40,
      'empty-state height ' + data.h + 'px is ' + Math.round(pct * 100) +
      '% of viewport (' + data.vh + 'px) \u2014 must be <40%');
  });

  // Desktop guard: at 1024x800 the sidebar must remain side-by-side with main
  // (layout flex-direction stays row), not stacked. This protects the desktop
  // experience from a regression introduced by the mobile fix.
  await ctx.close();
  const ctx2 = await browser.newContext({ viewport: { width: 1024, height: 800 } });
  const p2 = await ctx2.newPage();
  await p2.goto(BASE + '/#/channels', { waitUntil: 'domcontentloaded' });
  await p2.waitForSelector('.ch-layout', { timeout: 10000 });
  await p2.waitForTimeout(200);

  await step('desktop (1024px): ch-layout stays row (side-by-side)', async () => {
    const dir = await p2.evaluate(() => {
      const l = document.querySelector('.ch-layout');
      return l ? getComputedStyle(l).flexDirection : null;
    });
    assert(dir === 'row', 'desktop ch-layout flex-direction must be "row", got ' + dir);
  });

  await browser.close();

  console.log('\n' + passed + '/' + (passed + failed) + ' tests passed' +
              (failed ? ', ' + failed + ' failed' : ''));
  process.exit(failed > 0 ? 1 : 0);
}

run().catch(err => { console.error('Fatal:', err); process.exit(1); });
