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
  const page = await ctx.newPage();
  page.setDefaultTimeout(10000);
  await page.goto(BASE + '/#/packets', { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#fNode');
  await page.waitForSelector('#pktBody tr[data-hash]', { state: 'attached' });
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
    const allRows = await page.locator('#pktBody tr[data-hash], #pktBody tr[data-id]').count();
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
    await page.locator('#observerMenu input[data-obs-id="__all__"]').check();
    gate.resolve();
    // After the late response lands, the table must still reflect "All".
    await page.evaluate(() => fetch('/api/healthz').then(r => r.text()));
    await page.evaluate(() => new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r))));
    const after = await page.locator('#pktBody tr[data-hash], #pktBody tr[data-id]').count();
    assert(after === allRows, 'table shows ' + after + ' rows, expected the unfiltered ' + allRows);
    await page.close();
  });

  await browser.close();
  console.log('\n' + passed + '/' + (passed + failed) + ' passed' + (failed ? ', ' + failed + ' failed' : ''));
  process.exit(failed ? 1 : 0);
})().catch(e => { console.error(e); process.exit(2); });
