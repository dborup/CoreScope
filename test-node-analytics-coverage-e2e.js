#!/usr/bin/env node
'use strict';

// Exercise the real analytics page with explicit synthetic API coverage.
// No live mesh identities/data or non-local host may be used by this suite.
const assert = require('node:assert/strict');
const { chromium } = require('playwright');
const BASE = process.env.BASE_URL || 'http://localhost:13581';
assert.ok(['localhost', '127.0.0.1', '[::1]'].includes(new URL(BASE).hostname), 'local server required');
const PK = 'c048500000000000000000000000000000000000000000000000000000000000';
const FROM = '2026-10-15T12:00:00Z';
const TO = '2026-10-16T12:00:00Z';

function analytics(coverage) {
  return {
    node: { name: 'Coverage fixture', public_key: PK, role: 'repeater' },
    timeRange: { from: '2026-10-09T12:00:00Z', to: TO, days: 7 },
    coverage,
    computedStats: { totalPackets: 24, availabilityPct: 100, avgPacketsPerDay: 24, signalGrade: 'B', uniqueObservers: 2, relayPct: 50, longestSilenceMs: 0 },
    activityTimeline: [], snrTrend: [], packetTypeBreakdown: [], observerCoverage: [], hopDistribution: [], peerInteractions: [], uptimeHeatmap: [],
  };
}

(async () => {
  const browser = await chromium.launch({ headless: true, ...(process.env.CHROMIUM_PATH ? { executablePath: process.env.CHROMIUM_PATH } : {}) });
  let passed = 0;
  try {
    for (const width of [1440, 375]) {
      for (const state of ['partial', 'complete', 'unknown']) {
        const context = await browser.newContext({ viewport: { width, height: 900 } });
        const errors = [];
        const coverage = { from: FROM, to: TO, seconds: state === 'unknown' ? 0 : 86400, partial: state !== 'complete', loading: state === 'unknown', reason: state === 'unknown' ? 'loading' : state === 'complete' ? 'complete' : 'available_history' };
        await context.route(url => url.pathname === `/api/nodes/${PK}/analytics`, route => route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(analytics(coverage)) }));
        await context.route(url => url.pathname.startsWith(`/api/nodes/${PK}/`) && !url.pathname.endsWith('/analytics'), route => route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ packets: [], samples: [] }) }));
        const page = await context.newPage();
        page.on('pageerror', error => errors.push(error.message));
        await page.goto(`${BASE}/#/nodes/${PK}/analytics`, { waitUntil: 'domcontentloaded' });
        await page.locator('.analytics-stats').waitFor();
        const notice = page.locator('#nodeAnalyticsCoverageNotice');
        if (state === 'complete') {
          assert.equal(await notice.count(), 0, 'a complete range must not show partial notice');
        } else {
          await notice.waitFor({ state: 'visible' });
          const text = await notice.textContent();
          assert.match(text, /Partial history/);
          assert.match(text, /not measured radio uptime/);
          assert.equal(await notice.getAttribute('role'), 'status');
          if (state === 'partial') {
            assert.ok(text.includes(FROM) && text.includes(TO), 'effective interval must be visible');
            assert.match(text, /not the entire requested 7 days/);
          } else {
            assert.match(text, /supported history interval is not yet known/);
            assert.match(text, /still loading/);
          }
          assert.ok(await notice.evaluate(el => el.getBoundingClientRect().right <= window.innerWidth), 'notice must fit mobile/desktop viewport');
        }
        assert.deepEqual(errors, [], 'analytics page errors');
        console.log(`  ✓ ${state} coverage notice at ${width}px`);
        passed++;
        await context.close();
      }
    }
  } finally { await browser.close(); }
  console.log(`Node analytics coverage: ${passed}/6 passed`);
})().catch(error => { console.error(error); process.exitCode = 1; });
