#!/usr/bin/env node
'use strict';

// Real app + Chromium + network routing, with accelerated browser time and
// browser-driven SIMULATED visibility. Headless Chromium keeps both tabs
// visible even after bringToFront(), so this is not an OS-background test.
const assert = require('node:assert/strict');
const { chromium } = require('playwright');
const BASE = process.env.BASE_URL || 'http://127.0.0.1:13991';
assert.ok(['localhost', '127.0.0.1', '[::1]'].includes(new URL(BASE).hostname), 'local server only');
const measureOnly = process.argv.includes('--measure') || process.env.NAV_STATS_MEASURE === '1';

async function settle(page) {
  await page.evaluate(async () => { await Promise.allSettled([..._inflight.values()]); });
}
async function open(browser, { hidden = false, mobile = false } = {}) {
  const context = await browser.newContext({ viewport: mobile ? { width: 390, height: 844 } : { width: 1440, height: 900 } });
  const requests = [], sockets = [], errors = [];
  await context.addInitScript(initialHidden => {
    let hidden = initialHidden;
    Object.defineProperty(document, 'hidden', { configurable: true, get: () => hidden });
    Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => hidden ? 'hidden' : 'visible' });
    window.__navTestVisibility = value => {
      hidden = value;
      document.dispatchEvent(new Event('visibilitychange'));
    };
  }, hidden);
  await context.route('**/api/config/cache', route => route.fulfill({ json: {} }));
  await context.route('**/api/stats', async route => {
    requests.push(route.request().url());
    await route.fulfill({ json: { totalPackets: requests.length * 100, totalNodes: 2, totalObservers: 3 } });
  });
  await context.routeWebSocket(/.*/, socket => { sockets.push(socket); });
  const page = await context.newPage();
  page.setDefaultTimeout(15000);
  page.on('pageerror', error => errors.push(error.message));
  const date = new Date('2026-10-16T12:00:00Z');
  await page.clock.install({ time: date });
  await page.clock.pauseAt(new Date(date.getTime() + 1000));
  await page.goto(BASE + '/#/tools', { waitUntil: 'networkidle' });
  await settle(page);
  assert.ok(sockets.length, 'app shared WebSocket must connect');
  return {
    context, page, requests, errors,
    async ws() {
      await page.evaluate(() => {
        window.__navTestFrame = new Promise(resolve => {
          const once = () => { offWS(once); resolve(); };
          onWS(once);
        });
      });
      sockets[sockets.length - 1].send(JSON.stringify({ type: 'new_data' }));
      await page.evaluate(() => window.__navTestFrame);
    },
    async advance(ms) { await page.clock.runFor(ms); await settle(page); },
    async visibility(value) { await page.evaluate(value => window.__navTestVisibility(value), value); await settle(page); },
  };
}
async function traffic(h, seconds) {
  for (let i = 0; i < seconds; i++) { await h.ws(); await h.advance(1000); }
}
let passed = 0;
async function check(name, fn) { await fn(); passed++; console.log((measureOnly ? '  measured: ' : '  ✓ ') + name); }
(async () => {
  const browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_PATH || undefined });
  try {
    const visible = await open(browser);
    try {
      await check('visible sustained WS traffic: count actual fetches over inclusive simulated 0..60 seconds', async () => {
        await traffic(visible, 60);
        console.log('  measurement visible/60s inclusive: ' + visible.requests.length + ' actual /api/stats fetches');
        if (!measureOnly) assert.equal(visible.requests.length, 5);
      });
      await check('browser-driven hidden/foreground transition: actual network counts and refreshed counters', async () => {
        const start = visible.requests.length;
        await visible.visibility(true); await traffic(visible, 60);
        const hidden = visible.requests.length - start;
        await visible.visibility(false);
        const resumed = visible.requests.length - start - hidden;
        console.log('  measurement hidden/60s: ' + hidden + ', resume: ' + resumed + ' actual fetches');
        if (!measureOnly) {
          assert.equal(hidden, 0); assert.equal(resumed, 1);
          await visible.page.locator('#navStats').filter({ hasText: String(visible.requests.length * 100) + ' pkts' }).waitFor();
          for (let i = 0; i < 5; i++) await visible.visibility(false);
          assert.equal(visible.requests.length, start + 1);
        }
      });
      if (!measureOnly) {
        await check('SPA Live entry/exit leaves one nav poller and other shared WS listeners active', async () => {
          const before = await visible.page.evaluate(() => wsListeners.length);
          await visible.page.evaluate(() => { location.hash = '#/live'; });
          await visible.page.locator('#liveControls').waitFor();
          await settle(visible.page);
          // #514: Live renders #liveControls first and only registers its shared-WS
          // listener after `await loadNodes()` (live.js init), so the count is
          // sampled when that registration is observable, not when the DOM is.
          // If Live never subscribes this wait times out and the assertion fails.
          const subscribed = await visible.page
            .waitForFunction(n => wsListeners.length > n, before)
            .then(() => true, () => false);
          assert.ok(subscribed && await visible.page.evaluate(() => wsListeners.length) > before, 'Live must retain its independent subscription');
          await visible.page.evaluate(() => { location.hash = '#/tools'; });
          await visible.page.locator('.tools-landing').waitFor();
          assert.equal(await visible.page.evaluate(() => wsListeners.length), before);
          const start = visible.requests.length;
          await traffic(visible, 15); assert.equal(visible.requests.length, start + 1);
        });
        await check('no browser exceptions', async () => { assert.deepEqual(visible.errors, []); });
      }
    } finally { await visible.context.close(); }
    const initiallyHidden = await open(browser, { hidden: true, mobile: true });
    try {
      await check('mobile initially-hidden simulation and the first foreground transition', async () => {
        await traffic(initiallyHidden, 30);
        console.log('  measurement initially hidden/30s: ' + initiallyHidden.requests.length + ' actual fetches');
        if (!measureOnly) {
          assert.equal(initiallyHidden.requests.length, 0);
          await initiallyHidden.visibility(false);
          assert.equal(initiallyHidden.requests.length, 1);
          // The mobile design hides this desktop nav element; its DOM
          // counters must still refresh, not become a visible mobile control.
          assert.match(await initiallyHidden.page.locator('#navStats').textContent(), /100 pkts/);
          assert.deepEqual(initiallyHidden.errors, []);
        }
      });
    } finally { await initiallyHidden.context.close(); }
    console.log(measureOnly ? `\n${passed} browser scenarios measured (no regression assertions)` : `\n${passed} browser checks passed (regression assertions enabled)`);
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });
