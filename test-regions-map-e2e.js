#!/usr/bin/env node
/* Regions map: snapshot periods, honest coverage, URL history and lifecycle.
 * Mutation guards:
 *  - bindPopup(html string) / innerHTML of node name   -> XSS test fails
 *  - removing the data-theme MutationObserver          -> theme test fails
 *  - removing observer.disconnect() in destroy()       -> teardown test fails
 */
'use strict';
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const EVIL = '<img src=x onerror="window.__xss=1">';

function nodes() {
  const mk = (key, name, lat, lon) => ({ publicKey: key, name, role: 'repeater', lat, lon,
    positionSource: lat == null ? 'none' : 'reported', scopes: [{ name: '#example', lastRelayed: '2026-10-16T11:10:00Z' }] });
  const values = [mk('aa11', EVIL, 55, 10), mk('bb22', 'b', 55, 11), mk('cc33', 'c', 56, 10),
    mk('dd44', 'No position', null, null), mk('ee55', 'Half position', null, 10)];
  values[0].scopes.push({ name: '#other', lastRelayed: '2026-10-16T11:30:00Z' });
  values.push({ ...mk('ff66', 'Only unpositioned scope', null, null), scopes: [{ name: '#unpositioned', lastRelayed: '2026-10-16T10:00:00Z' }] });
  return values;
}

function snapshot(window, coverage) {
  const duration = { '1h': 3600000, '24h': 86400000, '7d': 604800000 }[window];
  const end = new Date('2026-10-16T12:00:00Z');
  const values = nodes();
  return { window, generatedAt: end.toISOString(), windowStart: new Date(end.getTime() - duration).toISOString(),
    windowEnd: end.toISOString(), coverage: coverage || { start: '2026-10-16T09:00:00Z', complete: false, loading: false },
    nodes: window === '1h' ? [values[0], values[3]] : values, truncated: false, scopesTruncated: false };
}

function historySnapshot(override = {}) {
  const historyStart = '2026-09-25T12:00:00Z', historyEnd = '2026-10-16T12:00:00Z';
  const bucket = (index, count) => ({ index, count, lastRelayed: new Date(Date.parse(historyStart) + index * 3600000 + 1000).toISOString() });
  const mk = (key, name, lat, lon, buckets) => ({ publicKey: key, name, role: 'repeater', lat, lon,
    positionSource: lat == null ? 'none' : 'reported', metadataSource: 'current', scopes: [{ name: '#example', buckets }] });
  const values = [mk('aa11', EVIL, 55, 10, [bucket(502, 2), bucket(503, 3)]),
    mk('bb22', 'New relay', 55, 11, [bucket(503, 4)]), mk('cc33', 'Previous relay', 56, 10, [bucket(502, 5)]),
    mk('dd44', 'No position', null, null, [bucket(503, 1)]), mk('ff66', 'Old distant relay', -40, -120, [bucket(0, 1)])];
  values[0].scopes.push({ name: '#other', buckets: [bucket(503, 10)] });
  values[2].metadataSource = 'archived';
  return { generatedAt: '2026-10-16T12:34:00Z', historyStart, historyEnd, bucketSeconds: 3600, bucketCount: 504,
    coverage: { start: historyStart, loading: false }, nodes: values,
    truncated: false, scopesTruncated: false, bucketsTruncated: false, ...override };
}

function routesSnapshot(override = {}) {
  const data = historySnapshot();
  data.nodes = data.nodes.map(node => ({ ...node, publicKey: node.publicKey.padEnd(64, '0') }));
  const key = short => short.padEnd(64, '0');
  const bucket = (index, count) => ({ index, count, lastRelayed: new Date(Date.parse(data.historyStart) + index * 3600000 + 1000).toISOString() });
  return { ...data, stepNodes: [
    { publicKey: key('6666'), name: 'Only own flood from', role: 'repeater', lat: 57, lon: 11, positionSource: 'reported' },
    { publicKey: key('7777'), name: 'Only own flood to', role: 'room', lat: 56, lon: 12, positionSource: 'reported' },
  ], steps: [
    { from: key('aa11'), to: key('bb22'), scope: '#example', buckets: [bucket(502, 2), bucket(503, 3)] },
    { from: key('bb22'), to: key('aa11'), scope: '#example', buckets: [bucket(503, 2)] },
    { from: key('aa11'), to: key('dd44'), scope: '#example', buckets: [bucket(503, 1)] },
    { from: key('cc33'), to: key('bb22'), scope: '#other', buckets: [bucket(503, 4)] },
    { from: key('6666'), to: key('7777'), scope: '#routeonly', buckets: [bucket(503, 7)] },
  ], stepsTruncated: false, stepBucketsTruncated: false,
  stepEvidence: { skippedObservations: 0, unresolvedHops: 0, missingEndpointSteps: 0, sourceTruncated: false, incomplete: false }, ...override };
}

async function main() {
  let browser;
  try {
    browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_PATH || undefined,
      args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'] });
  } catch (err) {
    if (process.env.CHROMIUM_REQUIRE === '1') { console.error('FAIL — Chromium required: ' + err.message); process.exit(1); }
    console.log('SKIP (Chromium unavailable)'); process.exit(0);
  }
  const ctx = await browser.newContext();
  const page = await ctx.newPage();
  page.setDefaultTimeout(15000);
  const errors = [];
  page.on('pageerror', e => errors.push(e.stack || String(e)));
  let holdHour = false, heldHour = null, failWeek = false, coverageOverride = null;
  let historyOverride = {}, failHistory = false;
  let holdHistory = false, heldHistory = null;
  let historyRequests = 0;
  let routesRequests = 0, routesOverride = {}, holdRoutes = false, heldRoutes = null;
  const requests = [];
  const waitForHeldHour = async () => {
    const deadline = Date.now() + 5000;
    while (!heldHour) {
      if (Date.now() > deadline) throw new Error('Timed out waiting for delayed 1h request');
      await page.waitForTimeout(10);
    }
  };
  const fulfill = (r, window) => r.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(snapshot(window, coverageOverride)) });
  await page.route('**/api/regions/observed?*', async r => {
    const window = new URL(r.request().url()).searchParams.get('window');
    requests.push(window);
    if (window === '1h' && holdHour) { heldHour = r; return; }
    if (window === '7d' && failWeek) return r.fulfill({ status: 500, body: '{}' });
    await fulfill(r, window);
  });
  await page.route('**/api/regions/history*', async r => {
    historyRequests++;
    if (holdHistory) { holdHistory = false; heldHistory = r; return; }
    if (failHistory) return r.fulfill({ status: 500, body: '{}' });
    await r.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(historySnapshot(historyOverride)) });
  });
  await page.route('**/api/regions/routes*', async route => {
    routesRequests++;
    if (holdRoutes) { holdRoutes = false; heldRoutes = route; return; }
    await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(routesSnapshot(routesOverride)) });
  });
  let failures = 0;
  let coverageCapture = 0;
  async function captureCoverage() {
    const coverage = await page.evaluate(() => {
      if (!window.__coverage__ || !Object.keys(window.__coverage__).length) return null;
      const snapshot = JSON.parse(JSON.stringify(window.__coverage__));
      for (const file of Object.values(window.__coverage__)) {
        for (const key of Object.keys(file.s || {})) file.s[key] = 0;
        for (const key of Object.keys(file.f || {})) file.f[key] = 0;
        for (const key of Object.keys(file.b || {})) file.b[key].fill(0);
      }
      return snapshot;
    });
    if (!coverage) return;
    const directory = path.join(__dirname, '.nyc_output');
    fs.mkdirSync(directory, { recursive: true });
    fs.writeFileSync(path.join(directory, 'regions-map-e2e-' + process.pid + '-' + (++coverageCapture) + '.json'), JSON.stringify(coverage));
  }
  const test = async (name, fn) => {
    try { await fn(); console.log('  ✓ ' + name); } catch (e) { failures++; console.log('  ✗ ' + name + ': ' + e.message); }
    finally {
      try { await captureCoverage(); } catch (e) { failures++; console.log('  ✗ coverage capture: ' + e.message); }
    }
  };
  // Track MutationObservers watching data-theme so teardown can be asserted.
  await page.addInitScript(() => {
    window.__themeObservers = [];
    const Orig = window.MutationObserver;
    window.MutationObserver = class extends Orig {
      observe(target, opts) {
        if (target === document.documentElement && opts && opts.attributeFilter && opts.attributeFilter.includes('data-theme')) {
          const rec = { live: true, page: /regions-map\.js/.test(new Error().stack) ? 'regions' : 'other' };
          window.__themeObservers.push(rec); this.__rec = rec;
        }
        return super.observe(target, opts);
      }
      disconnect() { if (this.__rec) this.__rec.live = false; return super.disconnect(); }
    };
  });
  const open = async (theme) => {
    await page.addInitScript(t => { try { localStorage.setItem('meshcore-theme', t); } catch (e) {} }, theme);
    await page.goto(`${BASE}/#/regions?window=24h&scope=%23example`, { waitUntil: 'domcontentloaded' });
    await page.waitForFunction(() => document.querySelector('.regions-status')?.textContent.includes('3 positioned'));
  };

  await open('dark');

  await test('shared period and scope are restored with exact partial-history bounds', async () => {
    assert.strictEqual(await page.getByLabel('Region period').inputValue(), '24h');
    assert.strictEqual(await page.getByLabel('Region scope').inputValue(), '#example');
    assert.strictEqual(await page.locator('.regions-coverage').getAttribute('data-coverage-state'), 'partial');
    assert.strictEqual(await page.locator('.regions-details').getAttribute('open'), null, 'long details should start collapsed');
    assert.strictEqual(await page.locator('.regions-snapshot time').getAttribute('datetime'), '2026-10-16T12:00:00.000Z');
    assert.match(await page.locator('.regions-exact-bounds').textContent(), /2026-10-15T12:00:00.000Z/);
    const coverage = await page.locator('.regions-coverage').textContent();
    assert.match(coverage, /16 Oct 2026, 09:00 UTC/);
    assert.strictEqual(await page.locator('.regions-coverage time').getAttribute('datetime'), '2026-10-16T09:00:00.000Z');
    assert.match(await page.locator('.regions-details').textContent(), /not observer uptime/);
    assert.match(await page.locator('.regions-details').textContent(), /self-reported; their accuracy is unverified/);
    assert.match(await page.locator('.regions-status').textContent(), /2 observed relay nodes without usable positions/);
    assert.strictEqual(await page.locator('.regions-leaflet path.leaflet-interactive').count(), 3, 'null latitude must not create an equator marker');
  });

  if (process.env.REGIONS_SCREENSHOT_DIR) {
    fs.mkdirSync(process.env.REGIONS_SCREENSHOT_DIR, { recursive: true });
    await page.setViewportSize({ width: 1440, height: 1000 });
    await page.waitForFunction(() => document.querySelectorAll('.regions-leaflet path.leaflet-interactive').length === 3 && !document.querySelector('.regions-leaflet .leaflet-zoom-anim'));
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    await page.screenshot({ path: path.join(process.env.REGIONS_SCREENSHOT_DIR, 'regions-desktop.png'), fullPage: true });
    await page.setViewportSize({ width: 390, height: 844 });
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    await page.screenshot({ path: path.join(process.env.REGIONS_SCREENSHOT_DIR, 'regions-mobile.png'), fullPage: true });
    await test('mobile controls stay within the viewport', async () => {
      assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1));
    });
    await page.setViewportSize({ width: 1440, height: 1000 });
  }

  await test('node name is escaped in the popup', async () => {
    await page.locator('.regions-leaflet path.leaflet-interactive').first().dispatchEvent('click');
    await page.waitForSelector('.leaflet-popup-content');
    assert.strictEqual(await page.evaluate(() => window.__xss), undefined, 'injected handler ran');
    assert.strictEqual(await page.locator('.leaflet-popup-content img').count(), 0, 'name parsed as HTML');
    const text = await page.locator('.leaflet-popup-content').textContent();
    assert(text.includes(EVIL), 'literal name missing from popup');
    assert.match(text, /Last observed relay for #example: 2026-10-16T11:10:00Z/);
    assert(!text.includes('2026-10-16T11:30:00Z'), 'another scope supplied the displayed relay time');
    assert.match(text, /self-reported; accuracy unverified/);
  });

  await test('page keeps the observed-routing disclaimer', async () => {
    const t = await page.locator('.regions-note').textContent();
    assert(/not a radio coverage measurement/.test(t), t);
  });

  await test('switching theme re-resolves the tile layer', async () => {
    await page.evaluate(() => {
      const original = L.Map.prototype.fitBounds;
      window.__regionsFitCount = 0;
      L.Map.prototype.fitBounds = function (...args) { window.__regionsFitCount++; window.__regionsFitBounds = {
        south: args[0].getSouth(), west: args[0].getWest(), north: args[0].getNorth(), east: args[0].getEast(),
      }; return original.apply(this, args); };
    });
    const dark = await page.evaluate(() => window.MC_getTileSpec('dark'));
    const light = await page.evaluate(() => window.MC_getTileSpec('light'));
    await page.evaluate(() => document.documentElement.setAttribute('data-theme', 'light'));
    await page.waitForFunction(() => {
      const f = document.querySelector('.regions-leaflet .leaflet-tile-pane').style.filter;
      return !f;
    }, null, { timeout: 3000 }).catch(() => {});
    const state = await page.evaluate(() => ({
      filter: document.querySelector('.regions-leaflet .leaflet-tile-pane').style.filter,
      src: (document.querySelector('.regions-leaflet .leaflet-tile') || {}).src || '',
    }));
    if (dark.invertFilter) assert.strictEqual(state.filter, '', 'dark invert filter left on light theme');
    if (dark.url !== light.url) {
      const host = new URL(light.url.replace('{s}', 'a')).host;
      await page.waitForFunction(h => [...document.querySelectorAll('.regions-leaflet .leaflet-tile')].some(i => i.src.includes(h)), host, { timeout: 5000 });
    }
    assert.strictEqual(await page.evaluate(() => window.__regionsFitCount), 0, 'theme change refitted the map');
  });

  await test('unpositioned-only scopes remain selectable and counted', async () => {
    const before = requests.length;
    await page.getByLabel('Region scope').selectOption('#unpositioned');
    assert.match(await page.locator('.regions-status').textContent(), /0 positioned relay nodes; 1 observed relay nodes without usable positions/);
    assert.strictEqual(await page.locator('.regions-leaflet path.leaflet-interactive').count(), 0);
    assert.strictEqual(new URLSearchParams((await page.evaluate(() => location.hash)).split('?')[1]).get('scope'), '#unpositioned');
    assert.strictEqual(requests.length, before, 'scope filtering made another API request');
    await page.getByLabel('Region scope').selectOption('#example');
  });

  await test('period change clears old data while pending and ignores a late response', async () => {
    holdHour = true;
    await page.getByLabel('Region period').selectOption('1h');
    await page.waitForFunction(() => document.querySelector('.regions-status').textContent.includes('Loading'));
    assert.strictEqual(await page.locator('.regions-leaflet path.leaflet-interactive').count(), 0);
    assert.strictEqual(await page.locator('.regions-snapshot').textContent(), '');
    assert.strictEqual(await page.locator('.regions-coverage').textContent(), '');
    await waitForHeldHour();
    await page.getByLabel('Region period').selectOption('7d');
    await page.waitForFunction(() => document.querySelector('.regions-status').textContent.includes('3 positioned'));
    const finished = page.waitForResponse(r => r.url().includes('/api/regions/observed?window=1h'));
    await fulfill(heldHour, '1h'); heldHour = null; holdHour = false;
    await finished;
    await page.waitForTimeout(100);
    assert.strictEqual(await page.getByLabel('Region period').inputValue(), '7d');
    assert.match(await page.locator('.regions-status').textContent(), /3 positioned/);
    assert.match(await page.locator('.regions-exact-bounds').textContent(), /2026-10-09T12:00:00.000Z/);
  });

  await test('browser Back and direct hash navigation restore period and scope', async () => {
    await page.goBack();
    await page.waitForFunction(() => document.querySelector('[aria-label="Region period"]')?.value === '1h' && document.querySelector('.regions-status')?.textContent.includes('1 positioned'));
    assert.strictEqual(await page.getByLabel('Region scope').inputValue(), '#example');
    await page.evaluate(() => { location.hash = '#/regions?window=24h&scope=%23unpositioned'; });
    await page.waitForFunction(() => document.querySelector('[aria-label="Region scope"]')?.value === '#unpositioned' && document.querySelector('.regions-status')?.textContent.includes('1 observed relay nodes'));
    assert.strictEqual(await page.getByLabel('Region period').inputValue(), '24h');
  });

  await test('failed periods do not leave old markers or coverage under new controls', async () => {
    failWeek = true;
    await page.getByLabel('Region period').selectOption('7d');
    await page.waitForFunction(() => document.querySelector('.regions-status').textContent.includes('Could not load'));
    assert.strictEqual(await page.locator('.regions-leaflet path.leaflet-interactive').count(), 0);
    assert.strictEqual(await page.locator('.regions-snapshot').textContent(), '');
    assert.strictEqual(await page.locator('.regions-coverage').textContent(), '');
    failWeek = false;
    await page.getByRole('button', { name: 'Refresh', exact: true }).click();
    await page.waitForFunction(() => document.querySelector('.regions-coverage').getAttribute('data-coverage-state') === 'partial');
  });

  await test('loading and unknown coverage never claim full history', async () => {
    coverageOverride = { start: null, complete: false, loading: true };
    await page.getByRole('button', { name: 'Refresh', exact: true }).click();
    await page.waitForFunction(() => document.querySelector('.regions-coverage').getAttribute('data-coverage-state') === 'loading');
    assert.match(await page.locator('.regions-coverage').textContent(), /History is still loading/);
    coverageOverride = { start: null, complete: false, loading: false };
    await page.getByRole('button', { name: 'Refresh', exact: true }).click();
    await page.waitForFunction(() => document.querySelector('.regions-coverage').getAttribute('data-coverage-state') === 'unknown');
    assert.match(await page.locator('.regions-coverage').textContent(), /bounds are unknown/);
    coverageOverride = null;
  });

  await test('timeline deep links clamp to completed hours and compare equal adjacent periods', async () => {
    await page.evaluate(() => { location.hash = '#/regions?view=timeline&window=1h&scope=%23example&at=2026-10-16T12%3A47%3A00Z&compare=1'; });
    await page.waitForFunction(() => document.querySelector('.regions-comparison')?.getAttribute('data-comparable') === 'true');
    assert.strictEqual(await page.getByLabel('Region view').inputValue(), 'timeline');
    assert.strictEqual(await page.getByLabel('Timeline interval ending').inputValue(), '168');
    assert.strictEqual(await page.getByLabel('Compare previous period').isChecked(), true);
    assert.strictEqual(await page.getByRole('button', { name: 'Play timeline', exact: true }).getAttribute('aria-pressed'), 'false');
    assert.match(await page.locator('.regions-interval').textContent(), /2026-10-16 11:00 — 2026-10-16 12:00 UTC/);
    assert.match(await page.locator('.regions-reference').textContent(), /2026-10-16 10:00 — 2026-10-16 11:00 UTC/);
    assert.match(await page.locator('.regions-status').textContent(), /2 positioned relay nodes; 1 observed relay nodes without usable positions/);
    assert.match(await page.locator('.regions-comparison').textContent(), /8 selected \/ 7 previous/);
    assert.match(await page.locator('.regions-state-added').textContent(), /2 newly observed/);
    assert.match(await page.locator('.regions-state-retained').textContent(), /1 observed in both/);
    assert.match(await page.locator('.regions-state-not-observed').textContent(), /1 not observed/);
    assert.match(await page.locator('.regions-appearance-change').textContent(), /\+1 \(\+14\.3%\)/);
    assert.strictEqual(await page.locator('.regions-leaflet path.leaflet-interactive').count(), 3, 'previous-only positioned relays remain visible in comparison');
    const params = new URLSearchParams((await page.evaluate(() => location.hash)).split('?')[1]);
    assert.strictEqual(params.get('at'), '2026-10-16T12:00:00.000Z');
    assert.match(await page.locator('.regions-details').textContent(), /playback shows activity, not historical movement/);
    assert.match(await page.locator('.regions-details').textContent(), /Current catalog metadata can revise historical attribution/);
  });

  if (process.env.REGIONS_SCREENSHOT_DIR) {
    await page.setViewportSize({ width: 1440, height: 1000 });
    await page.locator('.regions-change-list summary').first().click();
    await page.evaluate(() => Promise.all(document.querySelector('#app').getAnimations().map(animation => animation.finished.catch(() => {}))));
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    await page.screenshot({ path: path.join(process.env.REGIONS_SCREENSHOT_DIR, 'regions-timeline-desktop.png'), fullPage: true });
    await page.setViewportSize({ width: 390, height: 844 });
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    await page.screenshot({ path: path.join(process.env.REGIONS_SCREENSHOT_DIR, 'regions-timeline-mobile.png'), fullPage: true });
    await test('timeline controls and comparison stay within the mobile viewport', async () => {
      assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1));
      assert(await page.evaluate(() => {
        const status = document.querySelector('.regions-status').getBoundingClientRect();
        const meta = document.querySelector('.regions-snapshot').getBoundingClientRect();
        return status.bottom <= meta.top;
      }), 'wrapped status overlaps snapshot metadata');
    });
    await page.locator('.regions-leaflet').scrollIntoViewIfNeeded();
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    await page.screenshot({ path: path.join(process.env.REGIONS_SCREENSHOT_DIR, 'regions-timeline-mobile-map.png'), fullPage: true });
    await page.setViewportSize({ width: 1440, height: 1000 });
  }

  await test('timeline popup uses literal names and interval-specific appearance counts', async () => {
    await page.locator('.regions-leaflet path.leaflet-interactive').first().dispatchEvent('click');
    await page.waitForSelector('.leaflet-popup-content');
    assert.strictEqual(await page.locator('.leaflet-popup-content img').count(), 0);
    const text = await page.locator('.leaflet-popup-content').textContent();
    assert(text.includes(EVIL));
    assert.match(text, /Selected interval: 3 relay appearances/);
    assert.match(text, /Previous interval: 2 relay appearances/);
    assert.match(text, /Not a historical position/);
    assert.strictEqual(await page.evaluate(() => window.__xss), undefined);
  });

  await test('timeline keyboard, scope, period and comparison changes need no extra requests or map refits', async () => {
    const before = historyRequests;
    await page.evaluate(() => { window.__regionsFitCount = 0; });
    await page.getByLabel('Timeline interval ending').focus();
    await page.getByLabel('Timeline interval ending').press('ArrowLeft');
    await page.waitForFunction(() => document.querySelector('.regions-interval').textContent.includes('2026-10-16 10:00 — 2026-10-16 11:00'));
    assert.strictEqual(await page.getByLabel('Timeline interval ending').inputValue(), '167');
    assert.match(await page.locator('.regions-appearance-change').textContent(), /percentage unavailable/);
    await page.getByLabel('Region period').selectOption('7d');
    assert.match(await page.locator('.regions-interval').textContent(), /2026-10-09 11:00/);
    await page.getByLabel('Region scope').selectOption('#other');
    await page.getByLabel('Compare previous period').uncheck();
    assert.strictEqual(await page.locator('.regions-comparison').isVisible(), false);
    await page.getByLabel('Region period').selectOption('1h');
    await page.getByLabel('Region scope').selectOption('#example');
    await page.getByLabel('Compare previous period').check();
    assert.strictEqual(historyRequests, before);
    assert.strictEqual(await page.evaluate(() => window.__regionsFitCount), 0);
    await page.getByRole('button', { name: 'Fit map', exact: true }).click();
    assert.strictEqual(await page.evaluate(() => window.__regionsFitCount), 1);
    const bounds = await page.evaluate(() => window.__regionsFitBounds);
    assert(bounds.south > 50 && bounds.west > 9, 'an old distant relay outside both displayed intervals distorted the fit');
  });

  await test('playback advances locally, stops at newest hour and pauses on hidden documents', async () => {
    const before = historyRequests;
    await page.getByLabel('Timeline interval ending').focus();
    await page.getByLabel('Timeline interval ending').press('End');
    await page.getByLabel('Timeline interval ending').press('ArrowLeft');
    await page.getByRole('button', { name: 'Play timeline', exact: true }).click();
    await page.waitForFunction(() => document.querySelector('[aria-label="Timeline interval ending"]').value === '168' && document.querySelector('[aria-label="Play timeline"]')?.getAttribute('aria-pressed') === 'false');
    assert.strictEqual(historyRequests, before);
    await page.getByLabel('Timeline interval ending').press('ArrowLeft');
    await page.getByRole('button', { name: 'Play timeline', exact: true }).click();
    await page.evaluate(() => { Object.defineProperty(document, 'hidden', { configurable: true, value: true }); document.dispatchEvent(new Event('visibilitychange')); });
    assert.strictEqual(await page.getByRole('button', { name: 'Play timeline', exact: true }).getAttribute('aria-pressed'), 'false');
    await page.evaluate(() => { delete document.hidden; });
    await page.getByLabel('Timeline interval ending').press('End');
  });

  await test('partial, unknown, loading and capped history withhold all change claims', async () => {
    const overrides = [{ coverage: { start: '2026-10-16T10:30:00Z', loading: false } },
      { coverage: { start: null, loading: false } }, { coverage: { start: null, loading: true } }, { bucketsTruncated: true }];
    for (const value of overrides) {
      historyOverride = value;
      await page.getByRole('button', { name: 'Refresh', exact: true }).click();
      await page.waitForFunction(() => document.querySelector('.regions-page')?.getAttribute('aria-busy') === 'false' && document.querySelector('.regions-comparison')?.getAttribute('data-comparable') === 'false');
      assert.match(await page.locator('.regions-comparison-limited').textContent(), /Comparison limited by available history/);
      assert.strictEqual(await page.locator('.regions-state-added, .regions-state-not-observed, .regions-appearance-change').count(), 0);
      assert.match(await page.locator('.regions-status').textContent(), /8 relay appearances/);
      assert.strictEqual(await page.locator('.regions-leaflet path.leaflet-interactive').count(), 2, 'limited comparison maps only observed selected relays');
    }
    historyOverride = {};
    await page.getByRole('button', { name: 'Refresh', exact: true }).click();
    await page.waitForFunction(() => document.querySelector('.regions-comparison')?.getAttribute('data-comparable') === 'true');
  });

  await test('failed history refresh clears stale markers and comparison before retry', async () => {
    failHistory = true;
    await page.getByRole('button', { name: 'Refresh', exact: true }).click();
    await page.waitForFunction(() => document.querySelector('.regions-status').textContent.includes('Could not load'));
    assert.strictEqual(await page.locator('.regions-leaflet path.leaflet-interactive').count(), 0);
    assert.strictEqual(await page.locator('.regions-comparison').isVisible(), false);
    assert.strictEqual(await page.getByLabel('Timeline interval ending').isDisabled(), true);
    failHistory = false;
    await page.getByRole('button', { name: 'Refresh', exact: true }).click();
    await page.waitForFunction(() => document.querySelector('.regions-comparison')?.getAttribute('data-comparable') === 'true');
  });

  await test('health deep links share the UTC timeline interval and show observed activity cards', async () => {
    await page.evaluate(() => { location.hash = '#/regions?view=health&window=24h&scope=%23example&at=2026-10-16T14%3A48%3A00%2B02%3A00&compare=1'; });
    await page.waitForFunction(() => document.querySelector('.regions-health')?.getAttribute('data-ready') === 'true');
    assert.strictEqual(await page.getByLabel('Region view').inputValue(), 'health');
    assert.strictEqual(await page.locator('.regions-leaflet').isVisible(), false);
    assert.strictEqual(await page.getByRole('button', { name: 'Play timeline', exact: true }).isVisible(), false);
    assert.strictEqual(await page.getByRole('button', { name: 'Fit map', exact: true }).isVisible(), false);
    assert.strictEqual(await page.locator('[data-metric="nodes"] .regions-health-value').textContent(), '4');
    assert.strictEqual(await page.locator('[data-metric="appearances"] .regions-health-value').textContent(), '15');
    assert.match(await page.locator('.regions-health-rate').textContent(), /0\.63 relay appearances per hour/);
    assert.strictEqual(await page.locator('[data-metric="last-relayed"] time').getAttribute('datetime'), '2026-10-16T11:00:01.000Z');
    assert.strictEqual(await page.locator('.regions-trend-bar').count(), 24);
    assert.strictEqual(await page.locator('.regions-trend-gap').count(), 0);
    assert.strictEqual(await page.locator('.regions-trend-bar[data-count="0"]').count(), 22, 'complete zero hours are valid measurements');
    assert.match(await page.locator('.regions-health-note').textContent(), /spam|observer|routing/);
    const params = new URLSearchParams((await page.evaluate(() => location.hash)).split('?')[1]);
    assert.strictEqual(params.get('view'), 'health');
    assert.strictEqual(params.get('at'), '2026-10-16T12:00:00.000Z');
  });

  await test('health filters, slider and timeline parity reuse one history payload', async () => {
    const before = historyRequests;
    await page.evaluate(() => { window.__regionsFitCount = 0; });
    await page.getByLabel('Region period').selectOption('7d');
    assert.strictEqual(await page.locator('.regions-trend-bar').count(), 168);
    await page.getByLabel('Region scope').selectOption('#other');
    assert.strictEqual(await page.locator('[data-metric="nodes"] .regions-health-value').textContent(), '1');
    assert.strictEqual(await page.locator('[data-metric="appearances"] .regions-health-value').textContent(), '10');
    await page.getByLabel('Region scope').selectOption('');
    assert.strictEqual(await page.locator('[data-metric="nodes"] .regions-health-value').textContent(), '4', 'node appears in multiple scopes and hours only once in interval total');
    assert.strictEqual(await page.locator('[data-metric="appearances"] .regions-health-value').textContent(), '25');
    await page.getByLabel('Region period').selectOption('1h');
    await page.getByLabel('Region scope').selectOption('#example');
    await page.getByLabel('Timeline interval ending').focus();
    await page.getByLabel('Timeline interval ending').press('ArrowLeft');
    await page.waitForFunction(() => document.querySelector('[data-metric="appearances"] .regions-health-value')?.textContent === '7');
    assert.strictEqual(await page.locator('[data-metric="nodes"] .regions-health-value').textContent(), '2');
    assert.strictEqual(await page.locator('.regions-trend-bar').count(), 1);
    assert.match(await page.locator('.regions-appearance-change').textContent(), /percentage unavailable/);
    assert.strictEqual(await page.evaluate(() => window.__regionsFitCount), 0, 'health rendering must not fit the hidden map');
    const at = await page.evaluate(() => new URLSearchParams(location.hash.split('?')[1]).get('at'));
    await page.getByLabel('Region view').selectOption('timeline');
    assert.match(await page.locator('.regions-status').textContent(), /7 relay appearances/);
    assert.strictEqual(await page.locator('.regions-leaflet').isVisible(), true);
    assert.strictEqual(await page.evaluate(() => new URLSearchParams(location.hash.split('?')[1]).get('at')), at);
    await page.getByLabel('Region view').selectOption('health');
    assert.strictEqual(await page.locator('[data-metric="appearances"] .regions-health-value').textContent(), '7');
    assert.strictEqual(historyRequests, before, 'historical views and filters made another API request');
    await page.getByLabel('Timeline interval ending').press('End');
    await page.waitForFunction(() => document.querySelector('[data-metric="appearances"] .regions-health-value')?.textContent === '8');
  });

  await test('health renders without Leaflet or hidden map initialization', async () => {
    await page.evaluate(() => {
      window.__regionsSavedL = window.L; window.L = undefined;
      location.hash = '#/regions?view=health&window=1h&scope=%23example&compare=1';
    });
    try {
      await page.waitForFunction(() => document.querySelector('.regions-health')?.getAttribute('data-ready') === 'true');
      assert.strictEqual(await page.locator('[data-metric="appearances"] .regions-health-value').textContent(), '8');
      assert.strictEqual(await page.locator('.regions-leaflet.leaflet-container').count(), 0);
      assert.strictEqual(await page.locator('.regions-leaflet .leaflet-tile').count(), 0);
    } finally { await page.evaluate(() => { window.L = window.__regionsSavedL; delete window.__regionsSavedL; }); }
  });

  await test('health chart separates unavailable history from zero and withholds limited rates and deltas', async () => {
    await page.getByLabel('Region scope').selectOption('#other');
    await page.getByLabel('Timeline interval ending').focus();
    await page.getByLabel('Timeline interval ending').press('ArrowLeft');
    await page.waitForFunction(() => document.querySelector('[data-metric="appearances"] .regions-health-value')?.textContent === '0');
    assert.match(await page.locator('.regions-status').textContent(), /No scoped relay activity observed in this complete interval/);
    assert.strictEqual(await page.locator('.regions-trend-bar[data-count="0"]').getAttribute('height'), '0');
    assert.strictEqual(await page.locator('.regions-trend-gap').count(), 0);
    assert.match(await page.locator('.regions-health-rate').textContent(), /0\.00 relay appearances per hour/);
    await page.getByLabel('Timeline interval ending').press('End');
    await page.getByLabel('Region scope').selectOption('#example');
    await page.getByLabel('Region period').selectOption('24h');
    historyOverride = { coverage: { start: '2026-10-16T10:30:00Z', loading: false } };
    await page.getByRole('button', { name: 'Refresh', exact: true }).click();
    await page.waitForFunction(() => document.querySelector('.regions-health')?.getAttribute('data-ready') === 'true');
    assert.strictEqual(await page.locator('.regions-trend-gap').count(), 23);
    assert.strictEqual(await page.locator('.regions-trend-bar').count(), 1);
    assert.match(await page.locator('.regions-health-rate').textContent(), /unavailable/);
    assert.strictEqual(await page.locator('.regions-appearance-change').count(), 0);
    await page.locator('.regions-hourly-details summary').click();
    assert.strictEqual(await page.locator('.regions-hourly-table tbody tr').count(), 24);
    const partial = page.locator('.regions-hourly-table tbody tr[data-start="2026-10-16T10:00:00.000Z"]');
    assert.match(await partial.textContent(), /partial/i);
    assert.match(await partial.textContent(), /7 appearances; 2 relay nodes/);
    assert.strictEqual(await partial.locator('[data-column="appearances"]').textContent(), '—');
    for (const override of [{ coverage: { start: null, loading: false } }, { coverage: { start: null, loading: true } },
      { truncated: true }, { scopesTruncated: true }, { bucketsTruncated: true }]) {
      historyOverride = override;
      await page.getByRole('button', { name: 'Refresh', exact: true }).click();
      await page.waitForFunction(() => document.querySelector('.regions-health')?.getAttribute('data-ready') === 'true');
      assert.strictEqual(await page.locator('.regions-trend-gap').count(), 24);
      assert.strictEqual(await page.locator('.regions-trend-bar').count(), 0);
      assert.match(await page.locator('.regions-health-rate').textContent(), /unavailable/);
      assert.strictEqual(await page.locator('.regions-appearance-change').count(), 0);
      assert.strictEqual(await page.locator('[data-metric="appearances"] .regions-health-value').textContent(), '15', 'retained counts stay explicit');
    }
    historyOverride = {};
    await page.getByRole('button', { name: 'Refresh', exact: true }).click();
    await page.waitForFunction(() => document.querySelector('.regions-health')?.getAttribute('data-ready') === 'true');
  });

  await test('health SVG and exact-value table are accessible, themed and retain keyed rows', async () => {
    const chart = page.getByRole('img', { name: /Hourly relay appearances/ });
    assert.strictEqual(await chart.count(), 1);
    assert.match(await chart.getAttribute('aria-label'), /#example/);
    const wasOpen = await page.locator('.regions-hourly-details').evaluate(details => details.open);
    await page.locator('.regions-hourly-details summary').focus();
    await page.locator('.regions-hourly-details summary').press('Enter');
    assert.strictEqual(await page.locator('.regions-hourly-table').isVisible(), !wasOpen, 'keyboard did not toggle exact hourly values');
    if (wasOpen) await page.locator('.regions-hourly-details summary').press('Enter');
    assert.strictEqual(await page.locator('.regions-hourly-table').isVisible(), true);
    await page.evaluate(() => { window.__healthRow = document.querySelector('.regions-hourly-table tbody tr:last-child'); });
    await page.getByLabel('Region scope').selectOption('#other');
    assert.strictEqual(await page.evaluate(() => window.__healthRow === document.querySelector('.regions-hourly-table tbody tr:last-child')), true, 'scope filtering rebuilt unchanged hourly rows');
    await page.getByLabel('Region scope').selectOption('#example');
    await page.evaluate(() => document.documentElement.setAttribute('data-theme', 'dark'));
    const fill = await page.locator('.regions-trend-bar').last().evaluate(element => getComputedStyle(element).fill);
    assert(fill && fill !== 'none');
    assert.strictEqual(await page.evaluate(() => window.__xss), undefined);
    assert.strictEqual(await page.locator('.regions-health img').count(), 0);
    assert.match(await page.locator('.regions-health').textContent(), /traffic increase does not establish better health/i);
  });

  if (process.env.REGIONS_SCREENSHOT_DIR) {
    await page.setViewportSize({ width: 1440, height: 1000 });
    await page.screenshot({ path: path.join(process.env.REGIONS_SCREENSHOT_DIR, 'regions-health-desktop.png'), fullPage: true });
    await page.setViewportSize({ width: 390, height: 844 });
    await page.screenshot({ path: path.join(process.env.REGIONS_SCREENSHOT_DIR, 'regions-health-mobile.png'), fullPage: true });
    await test('health chart and controls fit the mobile viewport', async () => {
      assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1));
      const bounds = await page.locator('.regions-trend').boundingBox();
      assert(bounds.width <= 370 && bounds.height >= 120);
    });
    await page.locator('.regions-trend').scrollIntoViewIfNeeded();
    await page.screenshot({ path: path.join(process.env.REGIONS_SCREENSHOT_DIR, 'regions-health-mobile-chart.png'), fullPage: true });
    await page.setViewportSize({ width: 1440, height: 1000 });
  }

  await test('a delayed health refresh cannot replace a newer timeline selection', async () => {
    holdHistory = true;
    await page.getByRole('button', { name: 'Refresh', exact: true }).click();
    const deadline = Date.now() + 5000;
    while (!heldHistory) {
      if (Date.now() > deadline) throw new Error('Timed out waiting for delayed history');
      await page.waitForTimeout(10);
    }
    assert.strictEqual(await page.locator('.regions-health').isVisible(), false, 'old health cards stayed visible while refreshing');
    await page.getByLabel('Region view').selectOption('timeline');
    await page.waitForFunction(() => document.querySelector('.regions-status')?.textContent.includes('15 relay appearances'));
    const request = heldHistory.request();
    const finished = page.waitForResponse(response => response.request() === request);
    await heldHistory.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(historySnapshot({ nodes: [] })) });
    heldHistory = null;
    await finished;
    await page.waitForTimeout(100);
    assert.strictEqual(await page.getByLabel('Region view').inputValue(), 'timeline');
    assert.match(await page.locator('.regions-status').textContent(), /15 relay appearances/);
    assert.strictEqual(await page.locator('.regions-health').isVisible(), false);
    await page.getByLabel('Region view').selectOption('health');
    assert.strictEqual(await page.locator('[data-metric="appearances"] .regions-health-value').textContent(), '15');
  });

  await test('health returns to Latest and teardown stops historical controls', async () => {
    await page.getByLabel('Region view').selectOption('latest');
    await page.waitForFunction(() => document.querySelector('.regions-status')?.textContent.includes('3 positioned'));
    assert.strictEqual(await page.locator('.regions-health').isVisible(), false);
    assert.strictEqual(await page.locator('.regions-leaflet').isVisible(), true);
    await page.getByLabel('Region view').selectOption('health');
    await page.waitForFunction(() => document.querySelector('.regions-health')?.getAttribute('data-ready') === 'true');
    await page.getByLabel('Region view').selectOption('timeline');
    assert.strictEqual(await page.locator('.regions-health').isVisible(), false);
  });

  await test('Connections distinguishes shared membership from directed scoped steps', async () => {
    const before = routesRequests;
    await page.evaluate(() => { location.hash = '#/regions?view=connections&window=1h&scope=%23example&at=2026-10-16T12%3A48%3A00Z&compare=1'; });
    await page.waitForFunction(() => document.querySelector('.regions-connections')?.getAttribute('data-ready') === 'true');
    assert.strictEqual(routesRequests, before + 1);
    assert.strictEqual(await page.getByLabel('Region view').inputValue(), 'connections');
    assert.strictEqual(await page.getByRole('button', { name: 'Play timeline', exact: true }).isVisible(), false);
    assert.strictEqual(await page.getByLabel('Compare previous period').isVisible(), false);
    assert.strictEqual(await page.locator('.regions-pairs-table tbody tr').count(), 1);
    assert.match(await page.locator('.regions-pairs-table tbody').textContent(), /#example — #other/);
    assert.match(await page.locator('.regions-connections-note').first().textContent(), /does not establish cross-scope traffic/);
    assert.strictEqual(await page.locator('.regions-steps-table tbody tr').count(), 3);
    assert.match(await page.locator('.regions-steps-summary').textContent(), /6 step appearances/);
    assert.match(await page.locator('.regions-steps-summary').textContent(), /1 rows have endpoints without usable positions/);
    const directions = await page.locator('.regions-steps-table tbody tr td:first-child').allTextContents();
    assert(directions.some(value => value.includes(EVIL + ' → New relay')));
    assert(directions.some(value => value.includes('New relay → ' + EVIL)));
    assert.strictEqual(await page.locator('.regions-directed-step').count(), 2);
    assert(await page.locator('.regions-directed-step').first().getAttribute('marker-end'));
    assert.match(await page.locator('.regions-step-diagnostics').textContent(), /Snapshot-wide evidence diagnostics \(not selected-interval totals\)/);
    assert.strictEqual(await page.locator('.regions-connections img').count(), 0);
    assert.strictEqual(await page.evaluate(() => window.__xss), undefined);
    const params = new URLSearchParams((await page.evaluate(() => location.hash)).split('?')[1]);
    assert.strictEqual(params.get('at'), '2026-10-16T12:00:00.000Z');
    assert.strictEqual(params.has('compare'), false);
  });

  await test('Connections filters frames locally and preserves route-only scope endpoints', async () => {
    const before = routesRequests;
    await page.evaluate(() => { window.__regionsFitCount = 0; });
    await page.getByLabel('Region scope').selectOption('#routeonly');
    assert.strictEqual(await page.locator('.regions-steps-table tbody tr').count(), 1);
    assert.strictEqual(await page.locator('.regions-pairs-table tbody tr').count(), 0, 'route-only metadata created false membership');
    assert.match(await page.locator('.regions-steps-table tbody').textContent(), /Only own flood from → Only own flood to/);
    assert.match(await page.locator('.regions-steps-summary').textContent(), /7 step appearances/);
    assert.strictEqual(await page.locator('.regions-directed-step').count(), 1);
    await page.getByLabel('Timeline interval ending').focus();
    await page.getByLabel('Timeline interval ending').press('ArrowLeft');
    await page.waitForFunction(() => document.querySelector('.regions-steps-summary')?.textContent.includes('0 directed'));
    assert.match(await page.locator('.regions-steps-summary').textContent(), /No qualifying directed steps were observed/);
    await page.getByLabel('Timeline interval ending').press('End');
    await page.getByLabel('Region period').selectOption('24h');
    await page.getByLabel('Region scope').selectOption('#example');
    assert.match(await page.locator('.regions-steps-summary').textContent(), /8 step appearances/);
    assert.strictEqual(await page.evaluate(() => window.__regionsFitCount), 0);
    assert.strictEqual(routesRequests, before);
    await page.getByLabel('Region period').selectOption('1h');
  });

  await test('Connections reports partial, unknown, loading and capped evidence as lower bounds', async () => {
    for (const override of [{ coverage: { start: '2026-10-16T11:30:00Z', loading: false } },
      { coverage: { start: null, loading: false } }, { coverage: { start: null, loading: true } },
      { stepsTruncated: true }, { stepBucketsTruncated: true }, { stepEvidence: { sourceTruncated: true, incomplete: true } },
      { stepEvidence: { skippedObservations: 12, unresolvedHops: 3, missingEndpointSteps: 0, sourceTruncated: false, incomplete: false } }]) {
      routesOverride = override;
      await page.getByRole('button', { name: 'Refresh', exact: true }).click();
      await page.waitForFunction(() => document.querySelector('.regions-connections')?.getAttribute('data-ready') === 'true');
      assert.strictEqual(await page.locator('.regions-connections').getAttribute('data-complete'), 'false');
      assert.match(await page.locator('.regions-connections-summary').textContent(), /lower bounds/);
      assert.match(await page.locator('.regions-steps-summary').textContent(), /6 step appearances/);
    }
    routesOverride = {};
    await page.getByRole('button', { name: 'Refresh', exact: true }).click();
    await page.waitForFunction(() => document.querySelector('.regions-connections')?.getAttribute('data-complete') === 'true');
  });

  await test('Connections works as accessible evidence tables without Leaflet', async () => {
    await page.evaluate(() => { window.__regionsSavedL = window.L; window.L = undefined; location.hash = '#/regions?view=connections&window=1h&scope=%23routeonly'; });
    try {
      await page.waitForFunction(() => document.querySelector('.regions-connections')?.getAttribute('data-ready') === 'true');
      assert.strictEqual(await page.locator('.regions-steps-table tbody tr').count(), 1);
      assert.match(await page.locator('.regions-status').textContent(), /evidence tables remain available/);
      assert.strictEqual(await page.locator('.regions-leaflet.leaflet-container').count(), 0);
    } finally { await page.evaluate(() => { window.L = window.__regionsSavedL; delete window.__regionsSavedL; }); }
    await page.getByLabel('Region scope').selectOption('#example');
    await page.evaluate(() => document.documentElement.setAttribute('data-theme', 'dark'));
    assert(await page.locator('.regions-directed-step').first().evaluate(element => getComputedStyle(element).stroke !== 'none'));
  });

  await test('Connections warns when route-only scopes exceed the bounded selector and preserves an outside selection', async () => {
    const base = routesSnapshot();
    routesOverride = { nodes: [], steps: Array.from({ length: 129 }, (_, index) => ({ from: base.stepNodes[0].publicKey,
      to: base.stepNodes[1].publicKey, scope: '#route' + String(index).padStart(3, '0'), buckets: [base.steps[4].buckets[0]] })) };
    await page.evaluate(() => { location.hash = '#/regions?view=connections&window=1h&scope=%23route128'; });
    await page.waitForFunction(() => document.querySelector('.regions-connections')?.getAttribute('data-ready') === 'true');
    assert.strictEqual(await page.getByLabel('Region scope').inputValue(), '#route128');
    assert.strictEqual(await page.getByLabel('Region scope').locator('option').count(), 130);
    assert.match(await page.getByLabel('Region scope').locator('option:checked').textContent(), /Selected \(outside list\)/);
    assert.strictEqual(await page.locator('.regions-steps-table tbody tr').count(), 1);
    assert.strictEqual(await page.locator('.regions-pairs-table tbody tr').count(), 0);
    assert.strictEqual(await page.locator('.regions-connections').getAttribute('data-complete'), 'false');
    assert.match(await page.locator('.regions-connections-summary').textContent(), /lower bounds/);
    assert.match(await page.locator('.regions-status').textContent(), /selector is limited to 128 names/);
    routesOverride = {};
    await page.evaluate(() => { location.hash = '#/regions?view=connections&window=1h&scope=%23example'; });
    await page.waitForFunction(() => document.querySelector('.regions-connections')?.getAttribute('data-complete') === 'true');
  });

  await test('returning from Health refreshes Connections and Latest tiles and hidden-map dimensions', async () => {
    const viewport = page.viewportSize();
    await page.route('https://regions-tiles.invalid/**', route => route.fulfill({ status: 200, contentType: 'image/svg+xml',
      body: '<svg xmlns="http://www.w3.org/2000/svg" width="256" height="256"/>' }));
    await page.evaluate(() => {
      window.__regionsResume = { spec: window.MC_getTileSpec, invalidate: L.Map.prototype.invalidateSize,
        fit: L.Map.prototype.fitBounds, theme: document.documentElement.getAttribute('data-theme'), invalidations: 0 };
      window.MC_getTileSpec = theme => ({ url: 'https://regions-tiles.invalid/' + theme + '/{z}/{x}/{y}.png', maxZoom: 19 });
      L.Map.prototype.invalidateSize = function (...args) {
        window.__regionsResume.invalidations++; return window.__regionsResume.invalidate.apply(this, args);
      };
      L.Map.prototype.fitBounds = function (...args) {
        window.__regionsResume.map = this; return window.__regionsResume.fit.apply(this, args);
      };
      document.documentElement.setAttribute('data-theme', 'dark');
    });
    try {
      // Force a theme mutation even when the previous case already used dark.
      await page.evaluate(() => document.documentElement.setAttribute('data-theme', 'light'));
      await page.evaluate(() => document.documentElement.setAttribute('data-theme', 'dark'));
      await page.waitForFunction(() => [...document.querySelectorAll('.regions-leaflet .leaflet-tile')].some(tile => tile.src.includes('regions-tiles.invalid/dark/')));
      await page.getByRole('button', { name: 'Fit map', exact: true }).click();
      await page.getByLabel('Region view').selectOption('health');
      await page.evaluate(() => document.documentElement.setAttribute('data-theme', 'light'));
      await page.setViewportSize({ width: 390, height: 844 });
      await page.waitForTimeout(100); // Leaflet's resize frame runs while hidden.
      await page.evaluate(() => { window.__regionsResume.invalidations = 0; });
      await page.getByLabel('Region view').selectOption('connections');
      const state = await page.evaluate(() => {
        const record = window.__regionsResume, element = document.querySelector('.regions-leaflet'), size = record.map.getSize();
        return { refreshedLightTiles: [...element.querySelectorAll('.leaflet-tile')].some(tile => tile.src.includes('regions-tiles.invalid/light/')),
          invalidatedOnReturn: record.invalidations > 0, currentDimensions: size.x === element.clientWidth && size.y === element.clientHeight };
      });
      assert.deepStrictEqual(state, { refreshedLightTiles: true, invalidatedOnReturn: true, currentDimensions: true });
      await page.getByLabel('Region view').selectOption('health');
      await page.evaluate(() => document.documentElement.setAttribute('data-theme', 'dark'));
      await page.setViewportSize({ width: 1440, height: 1000 });
      await page.waitForTimeout(100);
      await page.evaluate(() => { window.__regionsResume.invalidations = 0; });
      await page.getByLabel('Region view').selectOption('latest');
      await page.waitForFunction(() => document.querySelector('.regions-status')?.textContent.includes('positioned relay'));
      const latestState = await page.evaluate(() => {
        const record = window.__regionsResume, element = document.querySelector('.regions-leaflet'), size = record.map.getSize();
        return { refreshedDarkTiles: [...element.querySelectorAll('.leaflet-tile')].some(tile => tile.src.includes('regions-tiles.invalid/dark/')),
          invalidatedOnReturn: record.invalidations > 0, currentDimensions: size.x === element.clientWidth && size.y === element.clientHeight };
      });
      assert.deepStrictEqual(latestState, { refreshedDarkTiles: true, invalidatedOnReturn: true, currentDimensions: true });
    } finally {
      await page.evaluate(() => {
        const record = window.__regionsResume;
        window.MC_getTileSpec = record.spec; L.Map.prototype.invalidateSize = record.invalidate; L.Map.prototype.fitBounds = record.fit;
        document.documentElement.setAttribute('data-theme', record.theme); delete window.__regionsResume;
      });
      await page.setViewportSize(viewport);
      await page.unroute('https://regions-tiles.invalid/**');
      if (await page.getByLabel('Region view').inputValue() !== 'connections') await page.getByLabel('Region view').selectOption('connections');
      await page.waitForFunction(() => document.querySelector('.regions-connections')?.getAttribute('data-ready') === 'true');
    }
  });

  await test('Connections paginates bounded keyed rows and caps only map rendering', async () => {
    const base = routesSnapshot(), source = base.nodes[0].publicKey;
    const targets = Array.from({ length: 230 }, (_, index) => ({ publicKey: (10000 + index).toString(16).padStart(64, '0'),
      name: 'Relay ' + index, role: 'repeater', lat: 55 + (index % 20) / 100, lon: 10 + index / 1000, positionSource: 'reported' }));
    routesOverride = { stepNodes: targets, steps: targets.map((node, index) => ({ from: source, to: node.publicKey, scope: '#example',
      buckets: [{ index: 503, count: index + 1, lastRelayed: '2026-10-16T11:00:01Z' }] })) };
    await page.getByRole('button', { name: 'Refresh', exact: true }).click();
    await page.waitForFunction(() => document.querySelector('.regions-steps-summary')?.textContent.includes('230 directed'));
    assert.strictEqual(await page.locator('.regions-steps-table tbody tr').count(), 25);
    assert.strictEqual(await page.locator('.regions-directed-step').count(), 200);
    const before = routesRequests;
    await page.getByRole('button', { name: 'Next directed relay steps page', exact: true }).click();
    assert.strictEqual(await page.locator('.regions-steps-table tbody tr').count(), 25);
    assert.strictEqual(await page.evaluate(() => new URLSearchParams(location.hash.split('?')[1]).get('page')), '2');
    assert.strictEqual(routesRequests, before);
    await page.getByRole('button', { name: 'Previous directed relay steps page', exact: true }).click();
    assert.strictEqual(await page.evaluate(() => new URLSearchParams(location.hash.split('?')[1]).has('page')), false);
    routesOverride = {};
    await page.getByRole('button', { name: 'Refresh', exact: true }).click();
    await page.waitForFunction(() => document.querySelector('.regions-steps-summary')?.textContent.includes('3 directed'));
    assert.strictEqual(await page.locator('.regions-directed-step').count(), 2);
  });

  if (process.env.REGIONS_SCREENSHOT_DIR) {
    await page.setViewportSize({ width: 1440, height: 1000 });
    await page.screenshot({ path: path.join(process.env.REGIONS_SCREENSHOT_DIR, 'regions-connections-desktop.png'), fullPage: true });
    await page.setViewportSize({ width: 390, height: 844 });
    await page.screenshot({ path: path.join(process.env.REGIONS_SCREENSHOT_DIR, 'regions-connections-mobile.png'), fullPage: true });
    await test('Connections tables and controls fit narrow mobile layouts', async () => {
      assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1));
      await page.locator('.regions-relations-scroll').first().focus();
      assert(await page.locator('.regions-relations-scroll').first().evaluate(element => document.activeElement === element));
    });
    await page.locator('.regions-leaflet').scrollIntoViewIfNeeded();
    await page.screenshot({ path: path.join(process.env.REGIONS_SCREENSHOT_DIR, 'regions-connections-mobile-map.png'), fullPage: true });
    await page.setViewportSize({ width: 1440, height: 1000 });
  }

  await test('late Connections evidence cannot replace a newer historical view', async () => {
    holdRoutes = true;
    await page.getByRole('button', { name: 'Refresh', exact: true }).click();
    const deadline = Date.now() + 5000;
    while (!heldRoutes) { if (Date.now() > deadline) throw new Error('Timed out waiting for delayed routes'); await page.waitForTimeout(10); }
    assert.strictEqual(await page.locator('.regions-connections').isVisible(), false);
    await page.getByLabel('Region view').selectOption('health');
    await page.waitForFunction(() => document.querySelector('.regions-health')?.getAttribute('data-ready') === 'true');
    const request = heldRoutes.request(), finished = page.waitForResponse(response => response.request() === request);
    await heldRoutes.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(routesSnapshot({ steps: [] })) }); heldRoutes = null;
    await finished; await page.waitForTimeout(100);
    assert.strictEqual(await page.getByLabel('Region view').inputValue(), 'health');
    assert.strictEqual(await page.locator('.regions-connections').isVisible(), false);
    assert.strictEqual(await page.locator('.regions-leaflet').isVisible(), false);
    await page.getByLabel('Region view').selectOption('timeline');
  });

  await test('destroy() tears down the map and the theme observer', async () => {
    // Navigation during Leaflet's 250ms zoom transition used to leave a
    // callback touching a removed map pane. Exercise the real zoom control.
    await page.getByLabel('Timeline interval ending').focus();
    await page.getByLabel('Timeline interval ending').press('ArrowLeft');
    await page.getByRole('button', { name: 'Play timeline', exact: true }).click();
    await page.locator('.regions-leaflet .leaflet-control-zoom-in').click();
    await page.waitForTimeout(40);
    await page.evaluate(() => { location.hash = '#/nodes'; });
    await page.waitForFunction(() => !document.querySelector('.regions-leaflet'));
    assert.strictEqual(await page.locator('.leaflet-container').count(), 0, 'leaflet container left behind');
    await page.evaluate(() => { document.documentElement.setAttribute('data-theme', 'dark'); });
    const hashAfterDestroy = await page.evaluate(() => location.hash);
    await page.waitForTimeout(1150);
    assert.strictEqual(await page.evaluate(() => location.hash), hashAfterDestroy, 'playback timer changed URL after navigation');
    assert.deepStrictEqual(errors, [], 'theme change after destroy raised errors');
    const live = await page.evaluate(() => window.__themeObservers.filter(o => o.live && o.page === 'regions').length);
    assert.strictEqual(live, 0, 'regions theme observer still connected');
  });

  await test('a response arriving after navigation cannot resurrect the regions page', async () => {
    holdHour = true;
    await page.evaluate(() => { location.hash = '#/regions?window=1h&scope=%23example'; });
    await page.waitForSelector('.regions-leaflet');
    await waitForHeldHour();
    await page.evaluate(() => { location.hash = '#/nodes'; });
    await page.waitForFunction(() => !document.querySelector('.regions-leaflet'));
    const finished = page.waitForResponse(r => r.url().includes('/api/regions/observed?window=1h'));
    await fulfill(heldHour, '1h'); heldHour = null; holdHour = false;
    await finished;
    await page.waitForTimeout(100);
    assert.strictEqual(await page.locator('.regions-page').count(), 0);
    assert.deepStrictEqual(errors, [], 'late response raised a page error');
  });

  await browser.close();
  if (failures) { console.error(failures + ' failure(s)'); process.exit(1); }
  console.log('regions map e2e passed');
}
main().catch(e => { console.error(e); process.exit(1); });
