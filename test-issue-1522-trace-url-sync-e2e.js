/**
 * E2E (#1522 / #1523, ported for #189): the Trace tool keeps the packet hash
 * in the URL.
 *
 *   1. Typing a hash on #/tools/trace/ and clicking Trace writes it into the
 *      URL (#/tools/trace/<hash>) with replaceState, so the URL can be shared
 *      and Back does not step through each trace.
 *   2. Opening #/tools/trace/<hash> directly pre-fills the input, runs the
 *      trace (GET /api/traces/<hash>) and leaves the URL as it was.
 *
 * Previously an @playwright/test spec; that runner is not a dependency, so it
 * never ran. This port uses the plain `playwright` API like the other
 * *-e2e.js files. The hashes are made up: the tests are about the URL, and
 * an unknown hash takes the same doTrace() path.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-issue-1522-trace-url-sync-e2e.js
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
const traceRequest = (hash) => (r) => new URL(r.url()).pathname === '/api/traces/' + hash;

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
      console.error('test-issue-1522-trace-url-sync-e2e.js: FAIL — Chromium required but unavailable: ' + err.message);
      process.exit(1);
    }
    console.log('test-issue-1522-trace-url-sync-e2e.js: SKIP (Chromium unavailable: ' + err.message.split('\n')[0] + ')');
    process.exit(0);
  }
  const ctx = await browser.newContext();
  const page = await ctx.newPage();
  page.setDefaultTimeout(8000);
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));

  console.log('\n=== #1522 trace URL sync E2E against ' + BASE + ' ===');
  try {
    await step('clicking Trace writes the hash into the URL (replaceState)', async () => {
      await page.goto(BASE + '/#/tools/trace/', { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('#traceHashInput');
      const historyBefore = await page.evaluate(() => history.length);
      await page.fill('#traceHashInput', 'deadbeef');
      const traced = page.waitForRequest(traceRequest('deadbeef'));
      await page.click('#traceBtn');
      await traced;
      await page.waitForFunction(() => /#\/tools\/trace\/deadbeef$/.test(location.hash));
      const historyAfter = await page.evaluate(() => history.length);
      assert(historyAfter === historyBefore, 'history grew from ' + historyBefore + ' to ' + historyAfter + ' (pushState instead of replaceState?)');
    });

    await step('a deep link pre-fills the input, runs the trace and keeps the URL', async () => {
      const traced = page.waitForRequest(traceRequest('cafebabe'));
      await page.goto(BASE + '/#/tools/trace/cafebabe', { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('#traceHashInput');
      assert((await page.inputValue('#traceHashInput')) === 'cafebabe', 'input not pre-filled');
      await traced;
      // The trace finished rendering (an unknown hash renders .trace-empty).
      await page.waitForSelector('#traceResults .trace-empty');
      const hash = await page.evaluate(() => location.hash);
      assert(/^#\/tools\/trace\/cafebabe$/.test(hash), 'URL changed to ' + hash);
    });

    await step('no page errors', async () => {
      assert(errors.length === 0, errors.join(' | '));
    });
  } finally {
    await browser.close();
  }
  console.log('\ntest-issue-1522-trace-url-sync-e2e.js: ' + passed + ' passed, ' + failed + ' failed');
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
