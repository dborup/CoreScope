'use strict';

const assert = require('node:assert/strict');
const { chromium } = require('playwright');
const BASE = process.env.BASE_URL || 'http://localhost:13581';
assert.ok(['localhost', '127.0.0.1', '[::1]'].includes(new URL(BASE).hostname), 'local server required');
const PK = 'ab'.repeat(32);
function analytics(name) {
  return {
    node: { name, public_key: PK, role: 'repeater' },
    computedStats: { totalPackets: 12, availabilityPct: 100, signalGrade: 'B', avgPacketsPerDay: 12, uniqueObservers: 1, relayPct: 0, longestSilenceMs: 0 },
    activityTimeline: [], snrTrend: [], packetTypeBreakdown: [], observerCoverage: [], hopDistribution: [], peerInteractions: [], uptimeHeatmap: [],
  };
}
const json = body => ({ status: 200, contentType: 'application/json', body: JSON.stringify(body) });
const deferred = () => { let resolve; const promise = new Promise(r => { resolve = r; }); return { promise, resolve }; };
const settledFrames = page => page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
async function waitForHeldRequest(promise) {
  let timer;
  try {
    await Promise.race([promise, new Promise((_resolve, reject) => { timer = setTimeout(() => reject(new Error('expected held request was not sent')), 15000); })]);
  } finally { clearTimeout(timer); }
}

(async () => {
  const browser = await chromium.launch({ headless: true, ...(process.env.CHROMIUM_PATH ? { executablePath: process.env.CHROMIUM_PATH } : {}) });
  let passed = 0;
  try {
    for (const width of [1440, 375]) {
      for (const status of [200, 500]) {
        const context = await browser.newContext({ viewport: { width, height: 900 } });
        const page = await context.newPage(), errors = [], arrived = deferred(), release = deferred();
        page.on('pageerror', error => errors.push(error.message));
        await context.route(url => url.pathname === `/api/nodes/${PK}/analytics`, async route => {
          arrived.resolve(); await release.promise;
          await route.fulfill(status === 200 ? json(analytics('Earlier visit')) : { status, contentType: 'application/json', body: '{"error":"old failure"}' });
        });
        await page.goto(`${BASE}/#/nodes/${PK}/analytics`, { waitUntil: 'domcontentloaded' });
        await waitForHeldRequest(arrived.promise);
        await page.evaluate(() => { location.hash = '#/nodes'; });
        const rows = page.locator('table tbody tr:not([id^="vscroll"])');
        await rows.first().waitFor();
        const before = await rows.count(); assert.ok(before > 0, 'Nodes must actually render before the held response');
        const response = page.waitForResponse(url => url.url().includes(`/api/nodes/${PK}/analytics`));
        release.resolve(); await (await response).finished(); await settledFrames(page);
        assert.equal(await rows.count(), before, 'late response must not replace the Nodes table');
        assert.equal(await page.locator('.analytics-stats').count(), 0, 'late success must not mount analytics');
        assert.equal(await page.getByText('Failed to load analytics:', { exact: false }).count(), 0, 'late failure must not replace Nodes');
        assert.deepEqual(errors, [], 'no unhandled browser errors');
        console.log(`  ✓ late ${status} leaves Nodes intact at ${width}px`); passed++;
        await context.close();
      }
      const context = await browser.newContext({ viewport: { width, height: 900 } });
      const page = await context.newPage(), arrived = deferred(), release = deferred(), errors = [];
      page.on('pageerror', error => errors.push(error.message));
      await context.route(url => url.pathname === `/api/nodes/${PK}/analytics`, route => route.fulfill(json(analytics('Current range'))));
      await context.route(url => url.pathname === `/api/nodes/${PK}/hop_analytics`, route => route.fulfill(json({ packets: [] })));
      await context.route(url => url.pathname === `/api/nodes/${PK}/battery`, async route => {
        if (new URL(route.request().url()).searchParams.get('days') === '7') {
          arrived.resolve(); await release.promise;
          await route.fulfill(json({ status: 'critical', samples: [] }));
        } else await route.fulfill(json({ status: 'ok', samples: [] }));
      });
      await page.goto(`${BASE}/#/nodes/${PK}/analytics`, { waitUntil: 'domcontentloaded' });
      await page.locator('.analytics-stats').waitFor(); await waitForHeldRequest(arrived.promise);
      await page.locator('#timeRangeBtns button[data-days="30"]').click();
      await page.waitForFunction(() => document.querySelector('#batteryStatusBadge')?.textContent.includes('OK'));
      const oldBattery = page.waitForResponse(response => response.url().includes(`/api/nodes/${PK}/battery?days=7`));
      release.resolve(); await (await oldBattery).finished(); await settledFrames(page);
      assert.match(await page.locator('#batteryStatusBadge').textContent(), /OK/);
      assert.doesNotMatch(await page.locator('#batteryStatusBadge').textContent(), /Critical/);
      assert.deepEqual(errors, [], 'current page has no unhandled browser errors');
      console.log(`  ✓ stale battery cannot overwrite the newer range at ${width}px`); passed++;
      await context.close();
    }
  } finally { await browser.close(); }
  console.log(`Page lifecycle: ${passed}/6 passed`);
})().catch(error => { console.error(error); process.exitCode = 1; });
