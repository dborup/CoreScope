/**
 * E2E (#1018 M1): sign in with a companion, and the synced My nodes list,
 * against a running server with the CI fixture.
 *
 * The CI server runs without accounts (the feature is off by default), so
 * the /api/account* routes are answered by Playwright in the shapes of
 * cmd/server/accounts.go; nothing is written anywhere. The node search is
 * answered too, so the companion and a repeater with the same name are
 * known. Real taps (page.touchscreen via tapWhenSettled) at 375x560, in
 * light and in dark.
 *
 * Asserts:
 *   1. feature off (the real server): Home has no account entry and
 *      #/account says accounts are off;
 *   2. Home signed out offers "Sign in with your companion"; the search
 *      lists the companion only, never the repeater; tapping it posts
 *      {pubkey} and shows the code, the suggested name with its byte count
 *      against the 31-byte budget, and the steps; nothing secret reaches the
 *      URL; the status follows pending → proven; the page redeems with the
 *      redeem secret and shows "Signed in as …" with the 8-hex prefix;
 *   3. "This is me" opens the account; the browser's saved nodes the account
 *      lacks are offered once; "Add all" posts one set with the union, and
 *      the browser's own list is untouched;
 *   4. Home's My Mesh grid shows the account's list: 24 cards of 30, then
 *      "Show more" renders the rest;
 *   5. every control is at least 44 px and nothing scrolls sideways;
 *   6. sign out returns to the sign-in card and to the browser's list.
 *
 * Usage:
 *   BASE_URL=http://localhost:13700 node test-1018-account-e2e.js
 * Optional: SCREENSHOT_DIR=<dir> writes the PR screenshots.
 */
'use strict';
const { chromium } = require('playwright');
const path = require('path');
const { tapWhenSettled } = require('./tests/e2e-tap');

const BASE = process.env.BASE_URL || 'http://localhost:13700';
const SHOT_DIR = process.env.SCREENSHOT_DIR || '';
const PK = 'a1b2c3d4' + 'e5'.repeat(28);
const REPEATER = 'b1'.repeat(32);
const REQ_ID = 'abcdef0123456789';
const CODE = '3HQ7ZB';
const SECRET = '5'.repeat(32);
const key = (i) => (i.toString(16).padStart(2, '0') + 'c0').repeat(16);

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + (e && e.stack || e)); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }
const pathOf = (u) => new URL(u).pathname;

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #1018 M1 owner account against ${BASE} ===\n`);

  // newCtx answers the account API from a small state machine.
  async function newCtx(theme, o) {
    o = o || {};
    const ctx = await browser.newContext({ viewport: { width: 375, height: 560 }, hasTouch: true, isMobile: true });
    await ctx.addInitScript(([t, local]) => {
      try {
        if (!sessionStorage.getItem('e2e-init')) {
          sessionStorage.setItem('e2e-init', '1');
          localStorage.setItem('meshcore-theme', t);
          localStorage.setItem('meshcore-user-level', 'experienced');
          // The gesture tips would cover the card in the screenshots.
          for (const h of ['row-swipe', 'tab-swipe', 'edge-drawer', 'pull-refresh']) localStorage.setItem('meshcore-gesture-hints-' + h, 'seen');
          localStorage.removeItem('meshcore-account-my-nodes');
          localStorage.setItem('meshcore-my-nodes', JSON.stringify(local));
        }
      } catch (_) {}
    }, [theme, o.local || []]);
    const rec = { logins: [], sessions: [], writes: [], statusCalls: 0, signedIn: false, myNodes: o.accountNodes || [] };
    if (o.off) return { ctx, rec };
    const json = (r, status, body, headers) => r.fulfill({ status, contentType: 'application/json', headers, body: JSON.stringify(body) });
    await ctx.route((u) => pathOf(u).startsWith('/api/account') || pathOf(u) === '/api/nodes/search', (r) => {
      const req = r.request();
      const p = pathOf(req.url());
      const m = req.method();
      if (p === '/api/nodes/search') {
        return json(r, 200, { nodes: [
          { public_key: PK, name: 'Anna T-Echo', role: 'companion' },
          { public_key: REPEATER, name: 'Anna Roof', role: 'repeater' },
        ] });
      }
      if (p === '/api/account/config') return json(r, 200, { enabled: true, available: true, sessionDays: 30, loginCodeMinutes: 15, maxMyNodes: 200 });
      if (p === '/api/account/login' && m === 'POST') {
        rec.logins.push(JSON.parse(req.postData() || '{}'));
        return json(r, 202, { requestId: REQ_ID, code: CODE, redeemSecret: SECRET, expiresAt: Date.now() + 15 * 60000, nameBudgetBytes: 31, suggestedName: 'Anna T-Echo ' + CODE });
      }
      if (p === '/api/account/login/' + REQ_ID) {
        rec.statusCalls++;
        return json(r, 200, { status: rec.statusCalls < 2 ? 'pending' : 'proven', expiresAt: Date.now() + 60000 });
      }
      if (p === '/api/account/session' && m === 'POST') {
        rec.sessions.push(JSON.parse(req.postData() || '{}'));
        rec.signedIn = true;
        return json(r, 200, { identityPrefix: PK.slice(0, 8), identityName: 'Anna T-Echo', expiresAt: Date.now() + 30 * 86400000 });
      }
      if (p === '/api/account/session' && m === 'DELETE') { rec.signedIn = false; return r.fulfill({ status: 204, body: '' }); }
      if (p === '/api/account' && m === 'GET') {
        if (!rec.signedIn) return json(r, 401, { error: 'not signed in' });
        return json(r, 200, { identityPrefix: PK.slice(0, 8), identityName: 'Anna T-Echo', signedInAt: Date.now(), myNodes: rec.myNodes, limits: { maxMyNodes: 200 } });
      }
      if (p === '/api/account/my-nodes' && m === 'POST') {
        const body = JSON.parse(req.postData() || '{}');
        rec.writes.push(body);
        if (body.op === 'set') rec.myNodes = body.nodes.map((n) => ({ pubkey: n.pubkey, name: n.name || '', addedAt: n.addedAt || 1 }));
        return json(r, 202, { requestId: 'f000000000000001' });
      }
      return json(r, 404, { error: 'Not found' });
    });
    return { ctx, rec };
  }

  async function noSideScroll(page) {
    const w = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
    assert(w <= 1, 'the page scrolls sideways by ' + w + ' px');
  }
  async function shot(page, name) {
    if (!SHOT_DIR) return;
    await page.waitForTimeout(400); // the page-enter fade
    await page.screenshot({ path: path.join(SHOT_DIR, name + '.png') });
  }

  await step('feature off (the real server): no Home entry, #/account says it is off', async () => {
    const { ctx } = await newCtx('light', { off: true });
    const page = await ctx.newPage();
    await page.goto(BASE + '/#/home', { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#homeSearch', { timeout: 15000 });
    await page.waitForTimeout(500);
    assert(await page.locator('[data-testid="home-account"]').count() === 0, 'an account entry with accounts off');
    await page.goto(BASE + '/#/account', { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('text=Accounts are not switched on', { timeout: 15000 });
    await ctx.close();
  });

  for (const theme of ['light', 'dark']) {
    const local = [{ pubkey: key(1), name: 'Old Roof', addedAt: '2026-01-01T00:00:00.000Z' }, { pubkey: key(2), name: 'Shared', addedAt: '2026-01-02T00:00:00.000Z' }];
    const { ctx, rec } = await newCtx(theme, { local, accountNodes: [{ pubkey: key(2), name: 'Shared', addedAt: 5 }] });
    const page = await ctx.newPage();
    const errors = [];
    page.on('pageerror', (e) => errors.push(String(e)));

    await step(`375x560 ${theme}: sign in with real taps, companions only, nothing secret in the URL`, async () => {
      await page.goto(BASE + '/#/home', { waitUntil: 'domcontentloaded' });
      const entry = page.locator('[data-testid="home-account"]');
      await entry.waitFor({ timeout: 15000 });
      assert(/Sign in with your companion/.test(await entry.textContent()), 'Home offers the sign-in');
      await tapWhenSettled(page, '[data-testid="home-account"]');
      await page.waitForSelector('[data-testid="account-signin"]', { timeout: 10000 });
      await shot(page, `account-1-choose-${theme}`);
      await noSideScroll(page);
      await tapWhenSettled(page, '#acctSearch');
      await page.keyboard.type('Anna');
      const pick = page.locator('.acct-pick');
      await pick.first().waitFor({ timeout: 5000 });
      assert(await pick.count() === 1, 'only the companion is offered, not the repeater: ' + await pick.count());
      await tapWhenSettled(page, '.acct-pick');
      await page.waitForSelector('[data-testid="account-code"]', { timeout: 10000 });
      assert(rec.logins.length === 1 && rec.logins[0].pubkey === PK, 'posted {pubkey}: ' + JSON.stringify(rec.logins));
      assert((await page.locator('[data-testid="account-code-value"]').textContent()) === CODE, 'the code is shown');
      assert((await page.locator('[data-testid="account-suggested-name"]').textContent()) === 'Anna T-Echo ' + CODE, 'the suggested name');
      assert(/18 \/ 31 bytes: the code fits/.test(await page.locator('.acct-bytes').textContent()), 'the byte count against the 31-byte budget');
      assert(/Flood/.test(await page.locator('.acct-steps').textContent()), 'the steps say Flood');
      await shot(page, `account-2-code-${theme}`);
      await noSideScroll(page);
      const url = page.url();
      assert(!url.includes(CODE) && !url.includes(SECRET) && !url.includes(REQ_ID), 'nothing secret in the URL: ' + url);
      await page.waitForSelector('[data-testid="account-confirm"]', { timeout: 20000 });
      assert(rec.sessions.length === 1 && rec.sessions[0].redeemSecret === SECRET && rec.sessions[0].requestId === REQ_ID, 'redeemed with the secret');
      const conf = await page.locator('[data-testid="account-confirm"]').textContent();
      assert(/Signed in as Anna T-Echo/.test(conf) && conf.includes(PK.slice(0, 8)) && !conf.includes(PK), 'who signed in, by prefix only: ' + conf);
      assert(await page.evaluate(() => sessionStorage.getItem('meshcore-account-signin')) === null, 'the pending sign-in is forgotten');
      await shot(page, `account-3-confirm-${theme}`);
    });

    await step(`375x560 ${theme}: "This is me", then the one-time migration of the browser's nodes`, async () => {
      await tapWhenSettled(page, '[data-testid="account-this-is-me"]');
      await page.waitForSelector('[data-testid="account-signed-in"]', { timeout: 10000 });
      const banner = page.locator('[data-testid="account-migrate"]');
      await banner.waitFor({ timeout: 5000 });
      assert(/This browser has 1 saved node/.test(await banner.textContent()), 'offers only the node the account lacks: ' + await banner.textContent());
      await shot(page, `account-4-migrate-${theme}`);
      await noSideScroll(page);
      await tapWhenSettled(page, '[data-testid="account-migrate-add"]');
      await page.waitForFunction(() => !document.querySelector('[data-testid="account-migrate"]'), null, { timeout: 5000 });
      const set = rec.writes.find((w) => w.op === 'set');
      assert(set && set.nodes.map((n) => n.pubkey).sort().join() === [key(1), key(2)].sort().join(), 'one set with the union: ' + JSON.stringify(rec.writes));
      assert(set.nodes.every((n) => typeof n.addedAt === 'number'), 'addedAt sent as Unix ms');
      const browserList = await page.evaluate(() => JSON.parse(localStorage.getItem('meshcore-my-nodes')).length);
      assert(browserList === 2, 'the browser list is untouched');
      assert(/2 \/ 200/.test(await page.locator('[data-testid="account-node-count"]').textContent()), 'the account lists 2 of 200');
      const small = await page.evaluate(() => [...document.querySelectorAll('.acct-card button, .acct-card a')]
        .filter((b) => b.offsetParent && b.closest('.acct-actions, .acct-node, .acct-profile') && b.getBoundingClientRect().height < 44)
        .map((b) => b.textContent.trim()));
      assert(!small.length, 'controls under 44 px: ' + small.join(', '));
      await shot(page, `account-5-signed-in-${theme}`);
    });

    await step(`375x560 ${theme}: Home shows the account's list, 24 cards then Show more`, async () => {
      rec.myNodes = Array.from({ length: 30 }, (_, i) => ({ pubkey: key(10 + i), name: 'N' + i, addedAt: i + 1 }));
      await page.goto(BASE + '/#/home', { waitUntil: 'domcontentloaded' });
      await page.reload({ waitUntil: 'domcontentloaded' });
      await page.waitForFunction(() => document.querySelectorAll('.my-node-card').length === 24, null, { timeout: 20000 });
      const more = page.locator('[data-testid="my-nodes-more"]');
      assert(/6 more/.test(await more.textContent()), 'Show more names the rest');
      assert(/Signed in as Anna T-Echo/.test(await page.locator('[data-testid="home-account"]').textContent()), 'Home says who is signed in');
      await shot(page, `account-6-home-grid-${theme}`);
      await tapWhenSettled(page, '[data-testid="my-nodes-more"]');
      await page.waitForFunction(() => document.querySelectorAll('.my-node-card').length === 30, null, { timeout: 20000 });
      assert(await page.locator('[data-testid="my-nodes-more"]').count() === 0, 'no Show more once all are shown');
      await noSideScroll(page);
    });

    await step(`375x560 ${theme}: sign out returns to the sign-in card and the browser's list`, async () => {
      await page.goto(BASE + '/#/account', { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('[data-testid="account-signed-in"]', { timeout: 10000 });
      await tapWhenSettled(page, '[data-testid="account-sign-out"]');
      await page.waitForSelector('[data-testid="account-signin"]', { timeout: 10000 });
      const st = await page.evaluate(() => ({ cache: localStorage.getItem('meshcore-account-my-nodes'), keys: MyNodes.keys().length }));
      assert(st.cache === null && st.keys === 2, 'back to the browser list: ' + JSON.stringify(st));
      assert(!errors.length, 'page errors: ' + errors.join(' | '));
    });
    await ctx.close();
  }

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed\n`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
