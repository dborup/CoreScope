/**
 * E2E (Relates to #560): Analytics -> Nodes' Role Breakdown must describe
 * the SAME population as that view's own Network Status once a region
 * filter is active, not the server's global /api/nodes `counts` metadata
 * (cmd/server/db.go GetAllRoleCounts, an unconditional COUNT(*) over the
 * whole `nodes` table regardless of the region/area filter on the page).
 *
 * Against a real browser and the Go server on the CI fixture (regions MRY,
 * OAK, SFO, SJC; 200 nodes total: 168 repeaters, 23 rooms, 9 companions, 0
 * sensors). MRY holds exactly 13 nodes, all repeaters -- a region small
 * enough, and lopsided enough, that the pre-fix bug is unmistakable: every
 * badge kept showing the unfiltered global counts (168/23/9/0) no matter
 * which region was picked, while the Network Status "Total Nodes" card
 * (already computed from the filtered rows before this fix) correctly
 * dropped to 13 -- two numbers for what is supposed to be one population.
 *
 * Usage: BASE_URL=http://localhost:13820 node test-issue-560-role-breakdown-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

async function openPage(browser) {
  const ctx = await browser.newContext({ viewport: { width: 1400, height: 900 } });
  const page = await ctx.newPage();
  page.__errors = [];
  page.on('pageerror', (e) => page.__errors.push(e.message));
  page.__reqs = [];
  page.__open = new Set();
  page.on('request', (r) => { if (r.url().includes('/api/')) { page.__reqs.push(r.url()); page.__open.add(r); } });
  page.on('requestfinished', (r) => page.__open.delete(r));
  page.on('requestfailed', (r) => page.__open.delete(r));
  return page;
}

// Until no /api/ request is open and none started for QUIET_MS (bounded).
// A fixed settle window alone is unreliable on a loaded machine.
const QUIET_MS = 800;
async function apiIdle(page) {
  const deadline = Date.now() + 20000;
  let n = page.__reqs.length, quietFrom = Date.now();
  while (Date.now() < deadline) {
    await page.waitForTimeout(100);
    if (page.__reqs.length !== n || page.__open.size > 0) { n = page.__reqs.length; quietFrom = Date.now(); continue; }
    if (Date.now() - quietFrom >= QUIET_MS) return;
  }
}

function roleBreakdown(page) {
  return page.evaluate(() => {
    const h3 = Array.from(document.querySelectorAll('#analyticsContent h3')).find((h) => h.textContent.includes('Role Breakdown'));
    if (!h3) return null;
    const row = h3.nextElementSibling;
    const counts = {};
    Array.from(row.querySelectorAll('.badge')).forEach((b) => {
      const m = /^(\w+):\s*(\d+)$/.exec(b.textContent.trim());
      if (m) counts[m[1]] = Number(m[2]);
    });
    return counts;
  });
}

function totalNodesStat(page) {
  return page.evaluate(() => {
    const card = Array.from(document.querySelectorAll('.analytics-stat-card')).find((c) => /Total Nodes/.test(c.textContent));
    if (!card) return null;
    const m = /(\d+)/.exec(card.textContent);
    return m ? Number(m[1]) : null;
  });
}

async function pickRegion(page, code) {
  await page.waitForSelector('#analyticsRegionFilter [data-region="' + code + '"]', { state: 'attached', timeout: 15000 });
  await page.evaluate((c) => document.querySelector('#analyticsRegionFilter [data-region="' + c + '"]').click(), code);
}

function sumOf(counts) {
  return Object.keys(counts).reduce((a, k) => a + counts[k], 0);
}

(async () => {
  const browser = await chromium.launch();
  try {
    const page = await openPage(browser);

    let unfilteredTotal, unfilteredBreakdown;
    await step('loads the Nodes analytics tab unfiltered', async () => {
      await page.goto(BASE + '/#/analytics?tab=nodes', { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('#analyticsContent h3:has-text("Role Breakdown")', { timeout: 15000 });
      await apiIdle(page);
      unfilteredTotal = await totalNodesStat(page);
      unfilteredBreakdown = await roleBreakdown(page);
      assert(unfilteredTotal > 0, 'Total Nodes must render a positive count, got ' + unfilteredTotal);
      assert(sumOf(unfilteredBreakdown) === unfilteredTotal,
        'unfiltered role-breakdown sum (' + sumOf(unfilteredBreakdown) + ') must equal Total Nodes (' + unfilteredTotal + ')');
    });

    await step('an MRY region filter: the role breakdown sum equals the Network Status total', async () => {
      await pickRegion(page, 'MRY');
      await apiIdle(page);
      await page.waitForFunction(() => {
        const card = Array.from(document.querySelectorAll('.analytics-stat-card')).find((c) => /Total Nodes/.test(c.textContent));
        return !!card && !/^\s*$/.test(card.textContent);
      }, { timeout: 15000 });

      const total = await totalNodesStat(page);
      const breakdown = await roleBreakdown(page);

      assert(total < unfilteredTotal, 'MRY must narrow Total Nodes below the unfiltered ' + unfilteredTotal + ', got ' + total);
      const sum = sumOf(breakdown);
      assert(sum === total, 'MRY-filtered role-breakdown sum (' + sum + ') must equal the Network Status total (' + total + ')');

      // The regression this closes: before the fix every badge showed the
      // server's global, filter-blind counts no matter which region was
      // active, so the filtered breakdown was identical to the unfiltered
      // one. MRY's own repeater count must differ from the unfiltered one.
      assert(breakdown.repeaters !== unfilteredBreakdown.repeaters,
        'MRY repeaters (' + breakdown.repeaters + ') must differ from the unfiltered global count (' + unfilteredBreakdown.repeaters + ')');
    });

    await step('clearing the region filter restores the unfiltered numbers (no leftover stale state)', async () => {
      await pickRegion(page, '__all__');
      await apiIdle(page);
      await page.waitForFunction((want) => {
        const card = Array.from(document.querySelectorAll('.analytics-stat-card')).find((c) => /Total Nodes/.test(c.textContent));
        const m = card && /(\d+)/.exec(card.textContent);
        return !!m && Number(m[1]) === want;
      }, unfilteredTotal, { timeout: 15000 });

      const total = await totalNodesStat(page);
      const breakdown = await roleBreakdown(page);
      assert(total === unfilteredTotal, 'clearing the filter should restore Total Nodes to ' + unfilteredTotal + ', got ' + total);
      assert(sumOf(breakdown) === total, 'post-clear role-breakdown sum must again equal Total Nodes');
      assert(breakdown.repeaters === unfilteredBreakdown.repeaters, 'post-clear repeaters count must match the original unfiltered count');
    });

    await step('no page errors were raised', async () => {
      assert(page.__errors.length === 0, 'page errors: ' + page.__errors.join('; '));
    });
  } finally {
    await browser.close();
  }

  console.log('\n' + passed + ' passed, ' + failed + ' failed');
  process.exit(failed === 0 ? 0 : 1);
})();
