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
  let failures = 0;
  const test = async (name, fn) => {
    try { await fn(); console.log('  ✓ ' + name); } catch (e) { failures++; console.log('  ✗ ' + name + ': ' + e.message); }
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
      L.Map.prototype.fitBounds = function (...args) { window.__regionsFitCount++; return original.apply(this, args); };
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

  await test('destroy() tears down the map and the theme observer', async () => {
    // Navigation during Leaflet's 250ms zoom transition used to leave a
    // callback touching a removed map pane. Exercise the real zoom control.
    await page.locator('.regions-leaflet .leaflet-control-zoom-in').click();
    await page.waitForTimeout(40);
    await page.evaluate(() => { location.hash = '#/nodes'; });
    await page.waitForFunction(() => !document.querySelector('.regions-leaflet'));
    assert.strictEqual(await page.locator('.leaflet-container').count(), 0, 'leaflet container left behind');
    await page.evaluate(() => { document.documentElement.setAttribute('data-theme', 'dark'); });
    await page.waitForTimeout(350);
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
