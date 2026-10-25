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
const { spawn } = require('child_process');
const fs = require('fs');
const os = require('os');
const path = require('path');
const net = require('net');

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

// Order-independent equality for the four known-role count objects --
// /api/nodes' `counts` and the DOM-parsed Role Breakdown enumerate their
// keys in different orders, so a JSON.stringify comparison is not safe.
function sameCounts(a, b) {
  const keys = ['repeaters', 'rooms', 'companions', 'sensors'];
  return keys.every((k) => (a[k] || 0) === (b[k] || 0));
}

// ===== area-only / combined region+area: CS-Minimax PR#580 round-2 review,
// finding 1. The shared BASE_URL server (and the committed e2e-fixture.db)
// has no `areas` configured at all, so an area filter cannot be exercised
// against it. Spin up a SEPARATE, disposable server instance -- a copy of
// the same migrated fixture, with one test-only area added via -config-dir
// -- mirroring the private-server pattern already used by
// test-issue-315-estimated-positions-e2e.js. Requires CORESCOPE_SERVER_BIN
// and CORESCOPE_MIGRATE_BIN (already set for that suite in e2e-shards.json);
// skips gracefully when they are not set, e.g. for a quick local run of just
// this file against BASE_URL.
async function freePort() {
  const socket = net.createServer();
  await new Promise((resolve, reject) => { socket.once('error', reject); socket.listen(0, '127.0.0.1', resolve); });
  const port = socket.address().port;
  await new Promise((resolve) => socket.close(resolve));
  return port;
}

async function stopChild(child) {
  if (!child || child.exitCode !== null || child.signalCode !== null) return;
  const ended = new Promise((resolve) => child.once('exit', resolve));
  child.kill('SIGTERM');
  const timer = setTimeout(() => child.kill('SIGKILL'), 5000);
  await ended;
  clearTimeout(timer);
}

async function waitServerHealthy(base, child, getLog) {
  const deadline = Date.now() + 15000;
  while (Date.now() < deadline) {
    assert(child.exitCode === null && child.signalCode === null, 'private server exited early: ' + getLog());
    try {
      const r = await fetch(base + '/api/healthz');
      if (r.ok) return;
    } catch (_) { /* not listening yet */ }
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  throw new Error('private server did not become healthy in time: ' + getLog());
}

// A bounding box covering 6 of the fixture's 13 MRY-region nodes (all
// repeaters) plus 43 further nodes outside MRY (38 repeaters, 4 rooms, 1
// companion) -- chosen by inspecting the committed fixture's real node
// coordinates, so area-only (49), region-only (13) and the MRY∩SFBAY
// combination (6) are three genuinely different, non-overlapping-with-
// unfiltered populations.
const SFBAY_AREA_CONFIG = { label: 'SF Bay (test)', latMin: 37.7, latMax: 38.0, lonMin: -122.6, lonMax: -122.0 };

async function withPrivateAreaServer(fn) {
  const serverBin = process.env.CORESCOPE_SERVER_BIN;
  const migrateBin = process.env.CORESCOPE_MIGRATE_BIN;
  if (!serverBin || !migrateBin) {
    console.log('  … SKIP area-only / combined region+area checks (set CORESCOPE_SERVER_BIN and CORESCOPE_MIGRATE_BIN to run them)');
    return;
  }
  const { execFileSync } = require('child_process');
  const scratch = fs.mkdtempSync(path.join(os.tmpdir(), 'corescope-560-e2e-'));
  let child = null;
  try {
    const database = path.join(scratch, 'fixture.db');
    fs.copyFileSync(path.join(__dirname, 'test-fixtures/e2e-fixture.db'), database);
    execFileSync(migrateBin, ['-db', database], { stdio: 'pipe' });
    fs.writeFileSync(path.join(scratch, 'config.json'), JSON.stringify({ areas: { SFBAY: SFBAY_AREA_CONFIG } }));
    const port = await freePort();
    const base = 'http://127.0.0.1:' + port;
    let log = '';
    child = spawn(serverBin, ['-config-dir', scratch, '-db', database, '-public', path.join(__dirname, 'public'), '-port', String(port)],
      { cwd: __dirname, stdio: ['ignore', 'pipe', 'pipe'] });
    child.stdout.on('data', (c) => { log = (log + c).slice(-20000); });
    child.stderr.on('data', (c) => { log = (log + c).slice(-20000); });
    await waitServerHealthy(base, child, () => log);
    await fn(base);
  } finally {
    await stopChild(child);
    fs.rmSync(scratch, { recursive: true, force: true });
  }
}

async function pickArea(page, key) {
  await page.waitForSelector('#analyticsAreaFilter .region-dropdown-trigger', { state: 'attached', timeout: 15000 });
  await page.click('#analyticsAreaFilter .region-dropdown-trigger');
  await page.waitForSelector('#analyticsAreaFilter [data-area="' + key + '"]', { state: 'attached', timeout: 15000 });
  await page.click('#analyticsAreaFilter [data-area="' + key + '"]');
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

    // CS-Minimax PR#580 round-2 review, finding 4: the suite checked the
    // breakdown's own internal sum against Total Nodes, but never against
    // the server's /api/nodes `counts` directly. Unfiltered, these two MUST
    // agree -- that is the baseline #560 preserves.
    await step('unfiltered Role Breakdown equals /api/nodes\' own global counts', async () => {
      const serverCounts = await page.evaluate(() => fetch('/api/nodes?limit=1').then((r) => r.json()).then((d) => d.counts));
      assert(sameCounts(unfilteredBreakdown, serverCounts),
        'unfiltered breakdown ' + JSON.stringify(unfilteredBreakdown) + ' must equal /api/nodes counts ' + JSON.stringify(serverCounts));
    });

    // CS-Minimax PR#580 round-2 review, finding 3: this PR touches no Go,
    // so the global /api/nodes `counts` contract (an unconditional COUNT(*)
    // per role, unaffected by region/area filters -- GetAllRoleCounts in
    // cmd/server/db.go) must still hold. Make that explicit rather than
    // leaving it implicit in "no Go changed".
    await step('/api/nodes global counts contract is unaffected by a region filter', async () => {
      const [plain, mry] = await Promise.all([
        page.evaluate(() => fetch('/api/nodes?limit=1').then((r) => r.json())),
        page.evaluate(() => fetch('/api/nodes?limit=1&region=MRY').then((r) => r.json())),
      ]);
      for (const key of ['repeaters', 'rooms', 'companions', 'sensors']) {
        assert(typeof plain.counts[key] === 'number', 'counts.' + key + ' must be a number, got: ' + JSON.stringify(plain.counts));
      }
      assert(sameCounts(mry.counts, plain.counts),
        'global counts must stay the server-wide total under a region filter (this PR only changed the CLIENT-side breakdown): ' +
        JSON.stringify(mry.counts) + ' vs ' + JSON.stringify(plain.counts));
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

    // CS-Minimax PR#580 round-2 review, finding 1: area-only and combined
    // region+area coverage, against a private server with one test area.
    await withPrivateAreaServer(async (base) => {
      const areaPage = await openPage(browser);
      let baseTotal;
      await step('loads the Nodes analytics tab unfiltered on the private area server', async () => {
        await areaPage.goto(base + '/#/analytics?tab=nodes', { waitUntil: 'domcontentloaded' });
        await areaPage.waitForSelector('#analyticsContent h3:has-text("Role Breakdown")', { timeout: 15000 });
        await apiIdle(areaPage);
        baseTotal = await totalNodesStat(areaPage);
        assert(baseTotal > 0, 'Total Nodes must render a positive count, got ' + baseTotal);
      });

      await step('an area-only filter: the role breakdown sum equals the Network Status total, and narrows the unfiltered total', async () => {
        await pickArea(areaPage, 'SFBAY');
        await apiIdle(areaPage);
        await areaPage.waitForFunction((want) => {
          const card = Array.from(document.querySelectorAll('.analytics-stat-card')).find((c) => /Total Nodes/.test(c.textContent));
          const m = card && /(\d+)/.exec(card.textContent);
          return !!m && Number(m[1]) !== want;
        }, baseTotal, { timeout: 15000 });

        const total = await totalNodesStat(areaPage);
        const breakdown = await roleBreakdown(areaPage);
        assert(total !== baseTotal, 'the SFBAY area must narrow Total Nodes from the unfiltered ' + baseTotal + ', got ' + total);
        assert(sumOf(breakdown) === total,
          'area-only role-breakdown sum (' + sumOf(breakdown) + ') must equal the Network Status total (' + total + ')');
      });

      await step('a combined region+area filter narrows further, and the breakdown sum still equals the total', async () => {
        await pickRegion(areaPage, 'MRY');
        await apiIdle(areaPage);
        await areaPage.waitForFunction(() => {
          const card = Array.from(document.querySelectorAll('.analytics-stat-card')).find((c) => /Total Nodes/.test(c.textContent));
          return !!card && !/^\s*$/.test(card.textContent);
        }, { timeout: 15000 });

        const total = await totalNodesStat(areaPage);
        const breakdown = await roleBreakdown(areaPage);
        // MRY ∩ SFBAY is exactly 6 of the fixture's known nodes, all
        // repeaters (verified directly against the Go server's own
        // /api/nodes?area=SFBAY&region=MRY response while building this
        // fixture area -- see SFBAY_AREA_CONFIG above).
        assert(total === 6, 'MRY ∩ SFBAY must be exactly 6 nodes in this fixture, got ' + total);
        assert(sumOf(breakdown) === total,
          'combined region+area role-breakdown sum (' + sumOf(breakdown) + ') must equal the Network Status total (' + total + ')');
        assert(breakdown.repeaters === 6, 'all 6 MRY ∩ SFBAY nodes are repeaters in this fixture, got ' + breakdown.repeaters);
      });

      await step('no page errors were raised on the private area server', async () => {
        assert(areaPage.__errors.length === 0, 'page errors: ' + areaPage.__errors.join('; '));
      });
      await areaPage.close();
    });
  } finally {
    await browser.close();
  }

  console.log('\n' + passed + ' passed, ' + failed + ' failed');
  process.exit(failed === 0 ? 0 : 1);
})();
