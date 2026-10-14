#!/usr/bin/env node
/* Regions map page (#376): escaping, theme switching, teardown.
 * Mutation guards:
 *  - bindPopup(html string) / innerHTML of node name   -> XSS test fails
 *  - removing the data-theme MutationObserver          -> theme test fails
 *  - removing observer.disconnect() in destroy()       -> teardown test fails
 */
'use strict';
const assert = require('node:assert');
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const EVIL = '<img src=x onerror="window.__xss=1">';

function nodes() {
  const mk = (key, name, lat, lon) => ({ public_key: key, name, role: 'repeater', lat, lon,
    transported_scopes_recent: ['#dk'], last_relayed: '2026-10-08T00:00:00Z' });
  return [mk('aa11', EVIL, 55, 10), mk('bb22', 'b', 55, 11), mk('cc33', 'c', 56, 10)];
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
  page.on('pageerror', e => errors.push(String(e)));
  await page.route('**/api/nodes?*', r => r.fulfill({ status: 200, contentType: 'application/json',
    body: JSON.stringify({ nodes: nodes(), counts: { repeater: 3 }, total: 3 }) }));
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
    await page.goto(`${BASE}/#/regions?scope=%23dk`, { waitUntil: 'domcontentloaded' });
    await page.waitForFunction(() => document.querySelector('.regions-status')?.textContent.includes('3 positioned'));
  };

  await open('dark');

  await test('node name is escaped in the popup', async () => {
    await page.evaluate(() => { document.querySelectorAll('.regions-leaflet path.leaflet-interactive').forEach(p => p.dispatchEvent(new MouseEvent('click', { bubbles: true }))); });
    await page.waitForSelector('.leaflet-popup-content');
    assert.strictEqual(await page.evaluate(() => window.__xss), undefined, 'injected handler ran');
    assert.strictEqual(await page.locator('.leaflet-popup-content img').count(), 0, 'name parsed as HTML');
  });

  await test('page keeps the observed-routing disclaimer', async () => {
    const t = await page.locator('.regions-note').textContent();
    assert(/not a radio coverage measurement/.test(t), t);
  });

  await test('switching theme re-resolves the tile layer', async () => {
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
  });

  await test('destroy() tears down the map and the theme observer', async () => {
    await page.evaluate(() => { location.hash = '#/nodes'; });
    await page.waitForFunction(() => !document.querySelector('.regions-leaflet'));
    assert.strictEqual(await page.locator('.leaflet-container').count(), 0, 'leaflet container left behind');
    errors.length = 0;
    await page.evaluate(() => { document.documentElement.setAttribute('data-theme', 'dark'); });
    await page.waitForTimeout(200);
    assert.deepStrictEqual(errors, [], 'theme change after destroy raised errors');
    const live = await page.evaluate(() => window.__themeObservers.filter(o => o.live && o.page === 'regions').length);
    assert.strictEqual(live, 0, 'regions theme observer still connected');
  });

  await browser.close();
  if (failures) { console.error(failures + ' failure(s)'); process.exit(1); }
  console.log('regions map e2e passed');
}
main().catch(e => { console.error(e); process.exit(1); });
