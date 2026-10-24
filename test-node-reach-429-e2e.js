// E2E for #530: Reach HTTP 429 -> bounded, cancellable retry with busy feedback.
// Intercepts the real /api/nodes/<pk>/reach response in the browser.
// Defaults to localhost:3000 — NEVER point at prod (AGENTS.md). CI sets BASE_URL.
const { chromium } = require('playwright');
const BASE = process.env.BASE_URL || 'http://localhost:3000';
const REACH = '**/api/nodes/*/reach*';
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
function check(c, m) { if (!c) throw new Error(m); console.log('  ok - ' + m); }

(async () => {
  const browser = await chromium.launch(process.env.CHROMIUM_PATH ? { executablePath: process.env.CHROMIUM_PATH } : {});
  const page = await browser.newPage();
  const errors = [];
  page.on('pageerror', (e) => errors.push(String(e)));

  const nodes = await (await page.request.get(BASE + '/api/nodes?role=repeater&limit=1')).json();
  if (!nodes.nodes || !nodes.nodes.length) { console.log('reach-429 E2E SKIP (no repeater)'); await browser.close(); return; }
  const pk = nodes.nodes[0].public_key;
  const url = BASE + '/#/nodes/' + pk + '/reach';

  // Route handler factory: first `n429` hits answer 429, the rest hit the server.
  let hits = 0;
  async function install(n429, retryAfter) {
    await page.unroute(REACH).catch(() => {});
    hits = 0;
    await page.route(REACH, async (route) => {
      hits++;
      if (hits <= n429) {
        const headers = { 'content-type': 'application/json' };
        if (retryAfter != null) headers['retry-after'] = String(retryAfter);
        return route.fulfill({ status: 429, headers, body: '{"error":"busy"}' });
      }
      return route.continue();
    });
  }

  // 1. 429 then success: busy message while waiting, then the report.
  console.log('1. 429 -> retry -> success');
  await install(1, 1);
  await page.goto(url);
  await page.waitForSelector('.nq-busy', { timeout: 10000 });
  const busyText = await page.locator('.nq-busy').first().innerText();
  check(/busy/i.test(busyText) && /retry/i.test(busyText) && /in 1s/.test(busyText), 'busy/retrying message shown: ' + JSON.stringify(busyText));
  await page.waitForSelector('.nq-head', { timeout: 15000 });
  check(hits === 2, 'exactly one retry (2 requests), got ' + hits);
  check((await page.locator('.nq-busy').count()) === 0, 'busy message gone after success');
  check((await page.locator('.nq-error').count()) === 0, 'no error shown');

  // 2. 429 x (1 + 5): final, actionable error after exhaustion.
  console.log('2. 429 x max -> final error');
  await install(99, 1);
  await page.goto('about:blank'); // fresh load: no api() TTL cache from scenario 1
  await page.goto(url);
  await page.waitForSelector('.nq-error', { timeout: 20000 });
  const errText = await page.locator('.nq-error').first().innerText();
  check(hits === 6, '1 initial + 5 retries = 6 requests, got ' + hits);
  check(/busy/i.test(errText) && /try again|reload|later/i.test(errText), 'actionable final message: ' + JSON.stringify(errText));
  check((await page.locator('.nq-busy').count()) === 0, 'busy message cleared on final error');
  await sleep(2500);
  check(hits === 6, 'no requests after exhaustion');

  // 3. Navigation during the wait cancels; re-entry loads cleanly.
  console.log('3. navigate away during wait');
  await install(99, 2);
  await page.goto('about:blank');
  await page.goto(url);
  await page.waitForSelector('.nq-busy', { timeout: 10000 });
  check(hits === 1, 'one request so far');
  await page.goto(BASE + '/#/nodes');
  await sleep(3500);
  check(hits === 1, 'no retry fired after leaving (hits=' + hits + ')');
  check((await page.locator('.nq-busy, .nq-error').count()) === 0, 'stale busy/error not rendered on the other page');
  await install(0);
  await page.goto(url);
  await page.waitForSelector('.nq-head', { timeout: 15000 });
  check(hits === 1, 're-entry: single fresh request');
  check((await page.locator('.nq-busy, .nq-error').count()) === 0, 're-entry renders cleanly');

  // 4. Window change during the wait cancels the old wait.
  console.log('4. change window during wait');
  await install(0);
  await page.goto('about:blank');
  await page.goto(url);
  await page.waitForSelector('#nqDays', { timeout: 15000 });
  const seen = [];
  await page.unroute(REACH);
  hits = 0;
  await page.route(REACH, async (route) => {
    hits++;
    const u = route.request().url();
    seen.push(u);
    if (/days=30/.test(u)) {
      return route.fulfill({ status: 429, headers: { 'content-type': 'application/json', 'retry-after': '2' }, body: '{"error":"busy"}' });
    }
    return route.continue();
  });
  await page.click('#nqDays button[data-days="30"]');
  await page.waitForSelector('.nq-busy', { timeout: 10000 });
  await page.click('#nqDays button[data-days="14"]');
  await page.waitForFunction(() => !document.querySelector('.nq-busy'), null, { timeout: 10000 });
  await sleep(3500);
  const d30 = seen.filter((u) => /days=30/.test(u)).length;
  check(d30 === 1, 'the cancelled 30d window was never retried (' + d30 + ')');
  check(seen.some((u) => /days=14/.test(u)), '14d request issued');
  check((await page.locator('.nq-error').count()) === 0, 'no stale error from the cancelled window');

  check(errors.length === 0, 'no page errors: ' + errors.join('; '));
  console.log('node-reach 429 E2E OK');
  await browser.close();
})().catch((e) => { console.error(e); process.exit(1); });
