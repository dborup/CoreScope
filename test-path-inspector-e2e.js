/**
 * E2E (Path Inspector spec §2.7 / §2.8, ported for #189): the Path Inspector
 * side pane on the map page, and the Tools landing.
 *
 * Previously an @playwright/test spec; that runner is not a dependency, so it
 * never ran. This port uses the plain `playwright` API like the other
 * *-e2e.js files and keeps the cases nothing else in CI covers:
 *   1. #mapSidePane is visible and collapsed on load; the toggle expands it;
 *   2. submitting prefixes in the pane renders a candidate table;
 *   3. "Show on Map" draws the candidate route (an .mc-rt-edge path) and
 *      enters route view;
 *   4. the Tools landing links to the Path Inspector and to Trace.
 *
 * Dropped from the old spec, covered elsewhere in CI:
 *   - standalone deep link ?prefixes= auto-fills and runs →
 *     test-path-inspector-coverage-e2e.js ("deep link ?prefixes=2c …");
 *   - #/traces/<hash> → #/tools/trace/<hash> → test-issue-1883-redirect-history.js;
 *   - "switching candidate clears prior polyline" asserted nothing.
 *
 * The old spec used the fixed prefixes 2c,a1, which have no candidates in
 * the CI fixture. The test now asks /api/paths/inspect for a two-hop pair
 * of repeater prefixes that does (bounded search).
 *
 * Usage: BASE_URL=http://localhost:13581 node test-path-inspector-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const MAX_PROBES = 400; // bounded /api/paths/inspect calls for the pair search

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

// A pair of 1-byte repeater prefixes with at least one candidate path.
async function findCandidatePair(request) {
  const nodes = await (await request.get(BASE + '/api/nodes?role=repeater&limit=500')).json();
  const prefixes = [...new Set((nodes.nodes || []).map((n) => n.public_key.slice(0, 2).toLowerCase()))].sort();
  let probes = 0;
  for (const a of prefixes) {
    for (const b of prefixes) {
      if (a === b) continue;
      if (++probes > MAX_PROBES) return null;
      const res = await request.post(BASE + '/api/paths/inspect', { data: { prefixes: [a, b] } });
      if (!res.ok()) continue;
      const body = await res.json();
      if (body.candidates && body.candidates.length) return { prefixes: a + ',' + b, count: body.candidates.length };
    }
  }
  return null;
}

(async () => {
  const requireChromium = process.env.CHROMIUM_REQUIRE === '1';
  let browser;
  try {
    browser = await chromium.launch({
      headless: true,
      executablePath: process.env.CHROMIUM_PATH || undefined,
      args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
    });
  } catch (err) {
    if (requireChromium) {
      console.error('test-path-inspector-e2e.js: FAIL — Chromium required but unavailable: ' + err.message);
      process.exit(1);
    }
    console.log('test-path-inspector-e2e.js: SKIP (Chromium unavailable: ' + err.message.split('\n')[0] + ')');
    process.exit(0);
  }
  const ctx = await browser.newContext({ viewport: { width: 1400, height: 900 } });
  const page = await ctx.newPage();
  page.setDefaultTimeout(15000);
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));

  console.log('\n=== Path Inspector map pane + Tools landing E2E against ' + BASE + ' ===');
  try {
    const pair = await findCandidatePair(ctx.request);

    await page.goto(BASE + '/#/map', { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#mapSidePane', { state: 'visible' });

    await step('side pane is visible and collapsed on load', async () => {
      const cls = await page.getAttribute('#mapSidePane', 'class');
      assert(!/\bexpanded\b/.test(cls), 'pane should start collapsed, class="' + cls + '"');
    });

    await step('clicking the toggle expands the pane', async () => {
      await page.click('#mapPaneToggle');
      await page.waitForFunction(() => document.getElementById('mapSidePane').classList.contains('expanded'));
      assert(await page.isVisible('#mapPiInput'), '#mapPiInput not visible in the expanded pane');
    });

    await step('submitting prefixes renders a candidate table' + (pair ? ' (' + pair.prefixes + ')' : ''), async () => {
      assert(pair, 'no repeater prefix pair with candidates found in ' + MAX_PROBES + ' probes');
      await page.fill('#mapPiInput', pair.prefixes);
      await page.click('#mapPiSubmit');
      await page.waitForSelector('#mapPiResults .path-inspector-table');
      const err = ((await page.textContent('#mapPiError')) || '').trim();
      assert(!err, 'unexpected error: ' + err);
      const rows = await page.locator('#mapPiResults button[data-idx]').count();
      assert(rows === pair.count, 'rendered ' + rows + ' candidates, API returned ' + pair.count);
    });

    await step('"Show on Map" draws the candidate route', async () => {
      assert(pair, 'no candidate to show');
      await page.click('#mapPiResults button[data-idx="0"]');
      await page.waitForSelector('#leaflet-map .leaflet-overlay-pane path.mc-rt-edge', { state: 'visible' });
      assert(await page.evaluate(() => document.body.classList.contains('mc-route-active')), 'route view not active');
    });

    await step('standalone "Show on Map" navigates, consumes the handoff, and draws the route', async () => {
      assert(pair, 'no candidate to show');
      await page.goto(BASE + '/#/tools/path-inspector?prefixes=' + pair.prefixes, { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('#path-inspector-results button[data-idx="0"]');
      await page.click('#path-inspector-results button[data-idx="0"]');
      await page.waitForFunction(() => location.hash === '#/map');
      await page.waitForSelector('#leaflet-map .leaflet-overlay-pane path.mc-rt-edge', { state: 'visible' });
      assert(await page.evaluate(() => document.body.classList.contains('mc-route-active')), 'route view not active');
      assert(await page.evaluate(() => !window._pendingPathInspectorRoute), 'pending route was not consumed');
    });

    await step('Tools landing links to the Path Inspector and to Trace', async () => {
      await page.goto(BASE + '/#/tools', { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('.tools-landing', { state: 'visible' });
      assert(await page.isVisible('.tools-landing a[href="#/tools/path-inspector"]'), 'Path Inspector entry missing');
      assert(await page.isVisible('.tools-landing a[href^="#/tools/trace"]'), 'Trace entry missing');
    });

    await step('no page errors', async () => {
      assert(errors.length === 0, errors.join(' | '));
    });
  } finally {
    await browser.close();
  }
  console.log('\ntest-path-inspector-e2e.js: ' + passed + ' passed, ' + failed + ' failed');
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
