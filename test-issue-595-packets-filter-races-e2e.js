/**
 * E2E (#595): Packets page filter races.
 *
 * Node filter: picking an autocomplete suggestion must apply the filter even
 * when the press is slow (the input's blur handler hides the list 200 ms after
 * focus leaves) or when a search response rebuilds the list mid-press.
 *
 * Observer filter: ticking an observer must re-fetch /api/packets with
 * observer=, and a slow response for an earlier selection must not overwrite
 * the table for the later one.
 *
 * Responses are held with route interception and released explicitly; the
 * only waits are on conditions, never on a fixed sleep for the outcome.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-issue-595-packets-filter-races-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
// The packets page defaults to a 15-minute window and the freshened CI fixture
// ages out of it while the shard runs, so a row count taken before a race can
// differ from the count after it (#658). Every page here pins a 1-year window.
const SEEDED_WINDOW_MIN = 525600;
// The first /api/packets `since` must reach at least this far back.
const MIN_WINDOW_MS = 300 * 24 * 3600 * 1000;
let passed = 0, failed = 0;
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
// Rejects instead of hanging when the awaited request never happens.
function within(promise, ms, what) {
  return Promise.race([promise, new Promise((_, rej) => setTimeout(() => rej(new Error('no ' + what + ' within ' + ms + ' ms')), ms))]);
}
function deferred() {
  let resolve; const promise = new Promise(r => { resolve = r; });
  return { promise, resolve };
}

async function openPackets(browserOrCtx) {
  // A fresh context per page: the observer filter persists in localStorage.
  const ctx = browserOrCtx.newContext ? await browserOrCtx.newContext() : browserOrCtx;
  // packets.js reads meshcore-time-window once, when the script is evaluated,
  // so the seed must be in place before the first page load.
  await ctx.addInitScript(min => { localStorage.setItem('meshcore-time-window', String(min)); }, SEEDED_WINDOW_MIN);
  const page = await ctx.newPage();
  page.setDefaultTimeout(10000);
  let firstPacketsUrl = null;
  page.on('request', r => { if (!firstPacketsUrl && /\/api\/packets\?/.test(r.url())) firstPacketsUrl = r.url(); });
  await page.goto(BASE + '/#/packets', { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#fNode');
  await page.waitForSelector('#pktBody tr[data-hash]', { state: 'attached' });
  const since = firstPacketsUrl && new URL(firstPacketsUrl).searchParams.get('since');
  const reach = since ? Date.now() - Date.parse(since) : Infinity;
  assert(reach >= MIN_WINDOW_MS,
    'first /api/packets request has since=' + since + ' (' + Math.round(reach / 60000) + ' min back), the pinned window was not in effect, so row counts depend on fixture age (#658)');
  return page;
}

// Holds the first /api/nodes/search response after arm() until release().
async function holdSearch(page) {
  const gate = deferred(); const inflight = deferred();
  let armed = false;
  await page.route(u => u.pathname === '/api/nodes/search', async route => {
    if (!armed) return route.continue();
    armed = false; inflight.resolve();
    await gate.promise;
    await route.continue();
  });
  return { arm() { armed = true; }, inflight: inflight.promise, release: gate.resolve };
}

async function suggestion(page, q) {
  await page.fill('#fNode', '');
  await page.type('#fNode', q);
  await page.waitForSelector('#fNodeDropdown:not(.hidden) .node-filter-option');
  return page.locator('#fNodeDropdown .node-filter-option').first();
}

async function press(page, opt, duringPress) {
  const box = await opt.boundingBox();
  assert(box, 'suggestion must be visible');
  await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
  await page.mouse.down();
  if (duringPress) await duringPress();
  await page.mouse.up();
}

async function assertPicked(page, name, key) {
  try {
    await page.waitForFunction(k => location.hash.includes('node=' + k), key, { timeout: 3000 });
  } catch (_) { throw new Error('URL has no node=' + key.slice(0, 8) + ', got ' + page.url()); }
  const v = await page.inputValue('#fNode');
  assert(v === name, 'input must show "' + name + '", got "' + v + '"');
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  const ctx = await browser.newContext();
  console.log('Packets filter races E2E (#595) against ' + BASE);

  const res = await ctx.request.get(BASE + '/api/nodes?limit=50');
  assert(res.ok(), 'fixture node request must succeed');
  const named = ((await res.json()).nodes || []).find(n => n.name && n.name.length >= 4 && n.public_key);
  assert(named, 'fixture must contain a named node');
  const query = named.name.slice(0, 3);

  await step('node filter: a normal click applies the pick', async () => {
    const page = await openPackets(browser);
    const opt = await suggestion(page, query);
    const name = await opt.getAttribute('data-name'), key = await opt.getAttribute('data-key');
    await opt.click();
    await assertPicked(page, name, key);
    await page.close();
  });

  await step('node filter: a press held past the blur delay still applies the pick', async () => {
    const page = await openPackets(browser);
    const opt = await suggestion(page, query);
    const name = await opt.getAttribute('data-name'), key = await opt.getAttribute('data-key');
    // Hold until the blur timer would have hidden the list, observed as a
    // condition on the page rather than slept for.
    await press(page, opt, () => page.evaluate(() => new Promise(r => setTimeout(r, 350))));
    await assertPicked(page, name, key);
    await page.close();
  });

  await step('node filter: a late search response does not drop the pick or reopen the list', async () => {
    const page = await openPackets(browser);
    const hold = await holdSearch(page);
    const opt = await suggestion(page, query);
    const name = await opt.getAttribute('data-name'), key = await opt.getAttribute('data-key');
    hold.arm();
    await page.type('#fNode', named.name.charAt(3));
    await within(hold.inflight, 5000, 'in-flight node search');
    await press(page, opt);
    await assertPicked(page, name, key);
    hold.release();
    // The held response resolves after the pick; wait until it has been
    // consumed by the page (a following search round-trip) then check.
    await page.evaluate(() => fetch('/api/nodes/search?q=zz').then(r => r.text()));
    await page.evaluate(() => new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r))));
    const reopened = await page.$('#fNodeDropdown:not(.hidden)');
    assert(!reopened, 'late response for the older query reopened the list after the pick');
    await page.close();
  });

  // --- Observer filter ---
  const obsRes = await ctx.request.get(BASE + '/api/observers');
  const observers = ((await obsRes.json()).observers || []);
  assert(observers.length >= 1, 'fixture must contain an observer');

  await step('observer filter: ticking an observer re-fetches with observer=', async () => {
    const page = await openPackets(browser);
    await page.click('#observerTrigger');
    const box = page.locator('#observerMenu input[data-obs-id]:not([data-obs-id="__all__"])').first();
    const id = await box.getAttribute('data-obs-id');
    const req = page.waitForRequest(r => {
      const u = new URL(r.url());
      return u.pathname === '/api/packets' && (u.searchParams.get('observer') || '').split(',').includes(id);
    }, { timeout: 5000 }).catch(() => null);
    await box.check();
    assert(await req, 'ticking observer ' + id + ' issued no /api/packets request with observer=');
    await page.close();
  });

  await step('observer filter: a slow response for an earlier selection does not overwrite the later one', async () => {
    const page = await openPackets(browser);
    const rowCount = () => page.locator('#pktBody tr[data-hash], #pktBody tr[data-id]').count();
    const frames = () => page.evaluate(() => new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r))));
    const gate = deferred(); const inflight = deferred();
    let held = false;
    await page.route(u => u.pathname === '/api/packets', async route => {
      const o = new URL(route.request().url()).searchParams.get('observer');
      if (o && !held) { held = true; inflight.resolve(); await gate.promise; }
      await route.continue();
    });
    await page.click('#observerTrigger');
    const box = page.locator('#observerMenu input[data-obs-id]:not([data-obs-id="__all__"])').first();
    await box.check();
    await within(inflight.promise, 5000, 'held /api/packets request with observer=');
    // The "All" refetch must land before the earlier response is released:
    // released first, the late response is overwritten by "All" even without
    // the stale-response guard, and the step cannot fail for the real bug.
    const allLanded = page.waitForResponse(r => {
      const u = new URL(r.url());
      return u.pathname === '/api/packets' && !u.searchParams.get('observer');
    });
    await page.locator('#observerMenu input[data-obs-id="__all__"]').check();
    await within(allLanded, 5000, '/api/packets response for "All"');
    await frames();
    // The baseline is read after "All" has settled, not before the race: the
    // fixture ages out of the packets window, so an earlier count can be stale (#658).
    const allRows = await rowCount();
    assert(allRows > 0, 'the "All" selection rendered no rows');
    gate.resolve();
    // After the late response lands, the table must still reflect "All".
    await page.evaluate(() => fetch('/api/healthz').then(r => r.text()));
    await frames();
    const after = await rowCount();
    assert(after === allRows, 'table shows ' + after + ' rows, expected the unfiltered ' + allRows);
    await page.close();
  });

  await browser.close();
  console.log('\n' + passed + '/' + (passed + failed) + ' passed' + (failed ? ', ' + failed + ' failed' : ''));
  process.exit(failed ? 1 : 0);
})().catch(e => { console.error(e); process.exit(2); });
