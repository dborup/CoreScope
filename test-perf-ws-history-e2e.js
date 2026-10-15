#!/usr/bin/env node
// Real Perf-page browser regression. Only the read-only WS history response is
// replaced with synthetic counts: no history is written and no host is changed.
// BASE_URL must be a local test server, as with the other frontend suites.
'use strict';

const assert = require('node:assert/strict');
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://127.0.0.1:13943';
const origin = new URL(BASE);
assert.ok(['localhost', '127.0.0.1', '[::1]'].includes(origin.hostname),
  'WS history E2E must use a local test server');

const RANGES = {
  '24h': { seconds: 86400, interval: 60, peak: 47 },
  '7d': { seconds: 7 * 86400, interval: 300, peak: 117 },
  '30d': { seconds: 30 * 86400, interval: 900, peak: 307 },
};

function syntheticHistory(range, healthy) {
  const spec = RANGES[range];
  const now = Date.now();
  const start = now - spec.seconds * 1000 + spec.interval * 1000;
  const end = now - spec.interval * 1000;
  // Two contiguous runs separated by an actual offline gap. A brief peak
  // survives even though open-at-sample counts stay low throughout.
  const samples = [
    { t: new Date(start).toISOString(), clients: 3, peak: 5 },
    { t: new Date(start + spec.interval * 1000).toISOString(), clients: 4, peak: spec.peak },
    { t: new Date(end - spec.interval * 1000).toISOString(), clients: 2, peak: 6, gapBefore: true },
    { t: new Date(end).toISOString(), clients: 3, peak: 4 },
  ];
  return {
    range, intervalSec: spec.interval, samples,
    persistence: { enabled: true, healthy },
  };
}

async function testPage(browser, options = {}) {
  const context = await browser.newContext({
    viewport: options.mobile ? { width: 375, height: 812 } : { width: 1440, height: 1000 },
    isMobile: !!options.mobile,
    hasTouch: !!options.mobile,
  });
  const requests = [];
  await context.route(url => url.pathname === '/api/perf/ws-clients', async route => {
    const range = new URL(route.request().url()).searchParams.get('range') || '24h';
    assert.ok(RANGES[range], 'unexpected WS history range: ' + range);
    requests.push(range);
    await route.fulfill({
      status: 200, contentType: 'application/json',
      body: JSON.stringify(syntheticHistory(range, options.healthy !== false)),
    });
  });
  const page = await context.newPage();
  page.setDefaultTimeout(15000);
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  return { context, page, requests, errors };
}

async function waitForRange(page, range) {
  await page.waitForFunction(({ range, peak }) => {
    const select = document.getElementById('wsHistoryRange');
    const content = document.getElementById('wsHistoryContent');
    return select && select.value === range && content &&
      content.textContent.includes(String(peak) + ' highest in period') && content.querySelector('svg.ws-history');
  }, { range, peak: RANGES[range].peak });
}

let passed = 0;
async function check(name, fn) {
  await fn();
  passed++;
  console.log('  ✓ ' + name);
}

(async () => {
  console.log('=== WS history ranges / persistence UI E2E ===');
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-dev-shm-usage'],
  });
  try {
    const desktop = await testPage(browser);
    try {
      const { page, requests } = desktop;
      await check('24-hour default uses its own endpoint response and an accessible selector', async () => {
        await page.goto(BASE + '/#/perf', { waitUntil: 'domcontentloaded' });
        await waitForRange(page, '24h');
        assert.deepEqual(await page.locator('#wsHistoryRange option').evaluateAll(options => options.map(o => o.value)), ['24h', '7d', '30d']);
        assert.equal(await page.getByRole('region', { name: 'WebSocket clients' }).getByRole('combobox', { name: 'Period' }).count(), 1);
        assert.equal(requests[0], '24h');
      });
      await check('7-day and 30-day selections fetch the selected range, without stale cached counts', async () => {
        for (const range of ['7d', '30d', '24h']) {
          const before = requests.length;
          const alreadyFetched = requests.includes(range);
          await page.locator('#wsHistoryRange').selectOption(range);
          await waitForRange(page, range);
          assert.equal(new URL(page.url()).hash.includes('wsRange=' + range), true);
          if (!alreadyFetched) assert.ok(requests.slice(before).includes(range), 'selection did not request ' + range);
          else assert.equal(requests.length, before, 'recent response was not reused for ' + range);
          const content = await page.locator('#wsHistoryContent').innerText();
          for (const other of Object.keys(RANGES).filter(other => other !== range)) {
            assert.ok(!content.includes(String(RANGES[other].peak) + ' highest'), 'cached ' + other + ' peak leaked into ' + range);
          }
        }
      });
      await check('native range control works by keyboard and keeps its focus', async () => {
        await page.locator('#wsHistoryRange').focus();
        // Native select type-ahead is keyboard interaction on every platform;
        // macOS headless Chromium cannot drive the OS arrow-key popup menu.
        await page.keyboard.press('7');
        await page.keyboard.press('Enter');
        await waitForRange(page, '7d');
        assert.equal(await page.locator('#wsHistoryRange').evaluate(element => element === document.activeElement), true);
      });
      await check('30-day deep link and reload preserve the selection and 307-connection peak', async () => {
        await page.goto(BASE + '/#/perf?wsRange=30d', { waitUntil: 'domcontentloaded' });
        await waitForRange(page, '30d');
        await page.reload({ waitUntil: 'domcontentloaded' });
        await waitForRange(page, '30d');
        assert.equal(requests[requests.length - 1], '30d');
        assert.match(await page.locator('#wsHistoryContent').innerText(), new RegExp(String(new Date().getFullYear())), 'multi-day timeline must include a calendar date');
      });
      await check('peak is retained and downtime splits the SVG rather than joining across missing history', async () => {
        const drawing = await page.locator('#wsHistoryContent svg.ws-history').evaluate(svg => ({
          solid: Array.from(svg.querySelectorAll('polyline')).filter(line => !line.hasAttribute('stroke-dasharray')).length,
          dashed: Array.from(svg.querySelectorAll('polyline')).filter(line => line.hasAttribute('stroke-dasharray')).length,
          label: svg.getAttribute('aria-label'),
        }));
        assert.ok(drawing.solid >= 2 && drawing.dashed >= 2, 'gap was rendered as one continuous line');
        assert.ok(drawing.label, 'chart has no accessible description');
        assert.match(await page.locator('#wsHistoryContent').innerText(), /307/);
        assert.match(await page.locator('#wsHistoryContent').innerText(), /saved|persist|restart/i);
      });
      assert.deepEqual(desktop.errors, [], 'unexpected desktop JavaScript errors');
    } finally { await desktop.context.close(); }

    const mobile = await testPage(browser, { mobile: true, healthy: false });
    try {
      await check('375px mobile: selected range is tappable and history card does not overflow', async () => {
        await mobile.page.goto(BASE + '/#/perf?wsRange=7d', { waitUntil: 'domcontentloaded' });
        await waitForRange(mobile.page, '7d');
        await mobile.page.locator('#wsHistoryRange').scrollIntoViewIfNeeded();
        const rect = await mobile.page.locator('#wsHistoryRange').boundingBox();
        assert.ok(rect && rect.width > 0 && rect.x >= 0 && rect.x + rect.width <= 375, 'range control is clipped');
        const hit = await mobile.page.locator('#wsHistoryRange').evaluate(element => {
          const r = element.getBoundingClientRect();
          const top = document.elementFromPoint(r.x + r.width / 2, r.y + r.height / 2);
          return top === element || element.contains(top);
        });
        assert.ok(hit, 'range control is covered by another element');
        await mobile.page.locator('#wsHistoryRange').selectOption('30d');
        await waitForRange(mobile.page, '30d');
        const bounds = await mobile.page.locator('#wsHistorySection').evaluate(element => ({
          client: element.clientWidth, scroll: element.scrollWidth,
          right: element.getBoundingClientRect().right,
        }));
        assert.ok(bounds.scroll <= bounds.client + 1 && bounds.right <= 376, 'WS history card scrolls sideways');
      });
      await check('persistence failure is visibly explained instead of claiming restart durability', async () => {
        const warning = mobile.page.locator('#wsHistorySection .ws-history-storage-note');
        assert.equal(await warning.isVisible(), true);
        assert.match(await warning.innerText(), /unavailable/i);
        assert.match(await warning.innerText(), /lost.*restart/i);
        assert.doesNotMatch(await warning.innerText(), /survives restarts/);
      });
      assert.deepEqual(mobile.errors, [], 'unexpected mobile JavaScript errors');
    } finally { await mobile.context.close(); }
  } finally { await browser.close(); }
  console.log('WS history E2E: ' + passed + ' passed, 0 failed');
})().catch(error => { console.error('  ✗ WS history E2E:', error.stack || error); process.exitCode = 1; });
