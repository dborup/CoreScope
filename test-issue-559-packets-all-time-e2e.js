/**
 * E2E (#559): the desktop Packets time window "All time" (value 0) survives a
 * reload and a shared link, and a missing, empty or invalid value never turns
 * into All time.
 *
 * Against a real browser + the Go server on the CI fixture, which holds one
 * grouped packet (hash SEED_HASH, three observations) dated well outside the
 * default 15 minute window.
 *
 *   1. Desktop: choosing All time puts timeWindow=0 in the URL, 0 in storage,
 *      and sends /api/packets without since; the response holds the old packet.
 *      After a reload the control still reads All time, and so do the
 *      request and the row.
 *   2. Grouping, and a second refetch, keep All time (no since).
 *   3. A fresh browser context opened from the shared link ?timeWindow=0
 *      shows All time (nothing saved in it).
 *   4. ?timeWindow= (empty), =abc and =-5: the 15 minute default, a since in
 *      the request, no old row; the saved All time of the context is replaced
 *      by the default.
 *   5. Saved storage '' and 'abc' on desktop: the default, not All time.
 *   6. Mobile (375 px): a link with timeWindow=0 or 1440 and a saved 0 give
 *      15 minutes, never All time; 180 is kept.
 *
 * Mutants: normalizeTimeWindow returns the default for 0 (1, 3 red); skips
 * the empty-string check (4, 5 red); mobile keeps 0 (6 red); the URL restore
 * reads only positive values (3 red); the change handler stores
 * Number(value) || 15 (1 red); buildPacketsQuery drops 0 (1 red).
 *
 * Usage: BASE_URL=http://localhost:13581 node test-issue-559-packets-all-time-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const SEED_HASH = 'fae0c9e6d357a814';
const KEY = 'meshcore-time-window';

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }
function eq(a, b, m) { if (a !== b) throw new Error((m || 'mismatch') + ': got ' + JSON.stringify(a) + ', want ' + JSON.stringify(b)); }

// Packet-list requests (not hash lookups) a page makes.
function trackPacketRequests(page) {
  const urls = [];
  const byRequest = new Map();
  page.on('request', (r) => {
    const u = new URL(r.url());
    if (u.pathname === '/api/packets') { urls.push(u); byRequest.set(r, u); }
  });
  // The table is virtualised, so the old packet's row is not in the DOM.
  // Whether it came back is read from the response of each request.
  page.on('response', async (resp) => {
    const u = new URL(resp.url());
    if (u.pathname !== '/api/packets') return;
    const entry = byRequest.get(resp.request());
    try { const t = await resp.text(); if (entry) entry.seed = t.includes(SEED_HASH); } catch (_) { if (entry) entry.seed = false; }
  });
  return urls;
}
const lastRequest = (urls) => urls[urls.length - 1];

async function load(page, hash) {
  // A hash-only change does not re-run the page's init; reload so every load
  // is a fresh start from this link and the saved state.
  await page.goto(BASE + '/#/' + hash, { waitUntil: 'domcontentloaded' });
  await page.reload({ waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#fTimeWindow', { state: 'attached', timeout: 10000 });
  await page.waitForLoadState('networkidle').catch(() => {});
  await page.waitForTimeout(600);
}
async function waitSeed(urls) {
  const end = Date.now() + 15000;
  while (Date.now() < end) {
    const last = urls[urls.length - 1];
    if (last && last.seed === true) return;
    await new Promise((r) => setTimeout(r, 200));
  }
  throw new Error('old packet never came back in the response');
}
const control = (page) => page.$eval('#fTimeWindow', (el) => el.value);
const stored = (page) => page.evaluate((k) => localStorage.getItem(k), KEY);
async function hashParam(page, name) {
  return page.evaluate((n) => new URLSearchParams(location.hash.split('?')[1] || '').get(n), name);
}
function minutesBack(u) {
  const since = u.searchParams.get('since');
  return since ? (Date.now() - Date.parse(since)) / 60000 : null;
}
function assertDefaultWindow(u, label) {
  const m = minutesBack(u);
  assert(m !== null, label + ': request has no since (All time)');
  assert(m > 10 && m < 25, label + ': expected ~15 min, got ' + (m && m.toFixed(1)));
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #559 packets All time E2E against ${BASE} ===`);

  const desk = await browser.newContext({ viewport: { width: 1400, height: 900 } });
  const page = await desk.newPage();
  page.setDefaultTimeout(10000);
  page.on('pageerror', (e) => console.error('[pageerror]', e.message));
  let reqs = trackPacketRequests(page);

  await step('desktop default: 15 minutes, old packet absent', async () => {
    await load(page, 'packets');
    eq(await control(page), '15', 'control');
    assertDefaultWindow(lastRequest(reqs), 'default');
    assert(lastRequest(reqs).seed === false, 'old packet must not come back in the default window');
  });

  await step('choosing All time: URL, storage, request and old packet row', async () => {
    reqs.length = 0;
    await page.selectOption('#fTimeWindow', '0');
    await waitSeed(reqs);
    eq(await control(page), '0', 'control');
    eq(await stored(page), '0', 'storage');
    eq(await hashParam(page, 'timeWindow'), '0', 'URL');
    assert(reqs.length > 0, 'a request was made');
    eq(lastRequest(reqs).searchParams.get('since'), null, 'since');
  });

  await step('reload keeps All time (control, storage, request, old packet)', async () => {
    reqs.length = 0;
    await page.reload({ waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#fTimeWindow');
    await waitSeed(reqs);
    eq(await control(page), '0', 'control');
    eq(await stored(page), '0', 'storage');
    eq(await hashParam(page, 'timeWindow'), '0', 'URL');
    eq(lastRequest(reqs).searchParams.get('since'), null, 'since');
  });

  await step('reload of a plain #/packets with only storage 0 keeps All time', async () => {
    reqs.length = 0;
    await load(page, 'packets');
    await waitSeed(reqs);
    eq(await control(page), '0', 'control');
    eq(lastRequest(reqs).searchParams.get('since'), null, 'since');
  });

  await step('toggling grouping keeps All time', async () => {
    reqs.length = 0;
    await page.click('#fGroup');
    await page.waitForTimeout(1500);
    assert(reqs.length > 0, 'grouping refetched');
    for (const u of reqs) eq(u.searchParams.get('since'), null, 'since after grouping');
    eq(await control(page), '0', 'control');
    eq(await stored(page), '0', 'storage');
    await page.click('#fGroup');
    await page.waitForTimeout(1500);
    eq(lastRequest(reqs).searchParams.get('since'), null, 'since after second toggle');
  });

  await step('fresh context from the shared link ?timeWindow=0 shows All time', async () => {
    const fresh = await browser.newContext({ viewport: { width: 1400, height: 900 } });
    const p2 = await fresh.newPage();
    const r2 = trackPacketRequests(p2);
    await load(p2, 'packets?timeWindow=0');
    await waitSeed(r2);
    eq(await control(p2), '0', 'control');
    eq(await stored(p2), '0', 'storage');
    eq(lastRequest(r2).searchParams.get('since'), null, 'since');
    await fresh.close();
  });

  for (const q of ['timeWindow=', 'timeWindow=abc', 'timeWindow=-5']) {
    await step(`link ${q} (saved All time): default window, not All time`, async () => {
      await page.evaluate((k) => localStorage.setItem(k, '0'), KEY);
      reqs.length = 0;
      await load(page, 'packets?' + q);
      eq(await control(page), '15', 'control');
      eq(await stored(page), '15', 'storage');
      assertDefaultWindow(lastRequest(reqs), q);
      assert(lastRequest(reqs).seed === false, 'old packet must not come back');
    });
  }

  for (const bad of ['', 'abc', '-3']) {
    await step(`saved storage ${JSON.stringify(bad)} on desktop: default window`, async () => {
      await page.evaluate(([k, v]) => localStorage.setItem(k, v), [KEY, bad]);
      reqs.length = 0;
      await load(page, 'packets');
      eq(await control(page), '15', 'control');
      assertDefaultWindow(lastRequest(reqs), 'storage ' + bad);
    });
  }

  await step('supported positive windows still work from a link (30, 1440)', async () => {
    for (const w of ['30', '1440']) {
      reqs.length = 0;
      await load(page, 'packets?timeWindow=' + w);
      eq(await control(page), w, 'control');
      const m = minutesBack(lastRequest(reqs));
      assert(m !== null && Math.abs(m - Number(w)) < 10, w + ': got ' + m);
    }
  });
  await desk.close();

  const mob = await browser.newContext({ viewport: { width: 375, height: 800 } });
  const mp = await mob.newPage();
  mp.setDefaultTimeout(10000);
  const mreqs = trackPacketRequests(mp);
  for (const [name, saved, q, want] of [
    ['link timeWindow=0', null, 'timeWindow=0', '15'],
    ['link timeWindow=1440', null, 'timeWindow=1440', '15'],
    ['saved 0', '0', '', '15'],
    ['link timeWindow=180', null, 'timeWindow=180', '180'],
  ]) {
    await step(`mobile: ${name} -> ${want} min, never All time`, async () => {
      await mp.goto(BASE + '/#/home', { waitUntil: 'domcontentloaded' });
      await mp.evaluate(([k, v]) => { if (v === null) localStorage.removeItem(k); else localStorage.setItem(k, v); }, [KEY, saved]);
      mreqs.length = 0;
      await load(mp, 'packets' + (q ? '?' + q : ''));
      eq(await control(mp), want, 'control');
      const m = minutesBack(lastRequest(mreqs));
      assert(m !== null, 'mobile must never send an All time request');
      assert(Math.abs(m - Number(want)) < 10, 'expected ~' + want + ', got ' + m.toFixed(1));
    });
  }
  await mob.close();

  await browser.close();
  console.log(`\n#559 E2E: ${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
