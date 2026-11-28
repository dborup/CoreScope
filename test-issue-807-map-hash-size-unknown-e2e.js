/**
 * E2E #807: a repeater whose hash size is unknown (no hash_size on
 * /api/nodes) is never drawn as a confirmed 1-byte label on the map.
 *
 * Part A — real server (CI fixture DB), desktop 1440x900, cold start:
 *   every rendered repeater label agrees with its node's hash_size. A node
 *   with a known size shows exactly that many prefix bytes and no unknown
 *   marker; a node without one shows the 1-byte prefix + "…", the hs-unknown
 *   class and "hash size unknown" in its aria-label. The fixture has
 *   repeaters that never sent an advert, so both kinds are present.
 *
 * Part B — phone 375x560, cold start, /api/nodes mocked with two repeaters
 *   (one 2-byte, one without hash_size): checks both labels and that tapping
 *   the unknown label opens a popup saying "size unknown", not "(1B)".
 *
 * Clustering is turned off (localStorage) so every label is in the DOM.
 * Set SCREENSHOT_DIR to save a screenshot of each part.
 *
 * Run: BASE_URL=http://localhost:13581 node test-issue-807-map-hash-size-unknown-e2e.js
 */
'use strict';
const path = require('path');
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const SHOTS = process.env.SCREENSHOT_DIR || '';

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

async function coldContext(browser, viewport) {
  const ctx = await browser.newContext({ viewport });
  await ctx.addInitScript(() => {
    try {
      localStorage.setItem('meshcore-map-clustering', 'false');
      localStorage.setItem('meshcore-map-hash-labels', 'true');
      localStorage.setItem('meshcore-map-byte-filter', 'all');
    } catch (e) { /* ignore */ }
  });
  return ctx;
}

// Every rendered repeater label with its node's hash_size, read from the
// map's own markers (_nodeKey) and node set (window.__mc_nodes).
function collectLabels(page) {
  return page.evaluate(() => {
    const byKey = new Map(window.__mc_nodes.map((n) => [n.public_key, n]));
    const out = [];
    const scan = (layer) => {
      if (!layer || !layer.eachLayer) return;
      layer.eachLayer((m) => {
        if (m.eachLayer) { scan(m); return; }
        if (!m._nodeKey || !m._icon) return;
        const el = m._icon.querySelector('.mc-mb-label');
        if (!el) return;
        const n = byKey.get(m._nodeKey) || {};
        out.push({
          key: m._nodeKey,
          hashSize: n.hash_size == null ? null : n.hash_size,
          text: el.textContent.replace(/^[^0-9A-F]* /, '').trim(),
          aria: el.getAttribute('aria-label') || '',
          unknown: el.classList.contains('hs-unknown'),
          title: el.getAttribute('title') || '',
        });
      });
    };
    window.__mc_map.eachLayer(scan);
    return out;
  });
}

async function partA(browser) {
  console.log(`\n=== #807 Part A: real server, 1440x900, cold start (${BASE}) ===`);
  const ctx = await coldContext(browser, { width: 1440, height: 900 });
  const page = await ctx.newPage();
  page.setDefaultTimeout(20000);
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  await page.goto(BASE + '/#/map', { waitUntil: 'load', timeout: 60000 });
  await page.waitForFunction(() => Array.isArray(window.__mc_nodes) && window.__mc_nodes.length > 0 && window.__mc_map);
  await page.waitForSelector('.mc-mb-label', { timeout: 20000 });
  const labels = await collectLabels(page);
  if (SHOTS) await page.screenshot({ path: path.join(SHOTS, '807-map-1440.png') });

  await step('the map renders labels for repeaters with and without a known hash size', async () => {
    const unknown = labels.filter((l) => l.hashSize == null).length;
    const known = labels.length - unknown;
    assert(unknown > 0, 'no label for a repeater without hash_size (fixture has advert-less repeaters)');
    assert(known > 0, 'no label for a repeater with a known hash_size');
    console.log(`    (${labels.length} labels: ${known} known size, ${unknown} unknown)`);
  });

  await step('a label without hash_size is marked unknown, never a plain 1-byte label', async () => {
    const bad = labels.filter((l) => l.hashSize == null && !(
      l.unknown && /hash size unknown$/.test(l.aria) && /^[0-9A-F]{2}…$/.test(l.text) && /unknown/i.test(l.title)));
    assert(bad.length === 0, 'unmarked unknown labels: ' + JSON.stringify(bad.slice(0, 3)));
  });

  await step('a label with a known hash_size shows that many bytes and no unknown marker', async () => {
    const bad = labels.filter((l) => l.hashSize != null && (
      l.unknown || /unknown/.test(l.aria) || l.text !== l.key.slice(0, l.hashSize * 2).toUpperCase()));
    assert(bad.length === 0, 'mismatching known labels: ' + JSON.stringify(bad.slice(0, 3)));
  });

  await step('no page errors', async () => {
    assert(errors.length === 0, errors.join(' | '));
  });
  await ctx.close();
}

const PK_KNOWN = '961d00aa11bb22cc33dd44ee55ff6600112233445566778899aabbccddeeff00';
const PK_UNKNOWN = '42ab00aa11bb22cc33dd44ee55ff6600112233445566778899aabbccddeeff00';

async function partB(browser) {
  console.log('\n=== #807 Part B: phone 375x560, cold start, mocked /api/nodes ===');
  const ctx = await coldContext(browser, { width: 375, height: 560 });
  const page = await ctx.newPage();
  page.setDefaultTimeout(20000);
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  const now = new Date().toISOString();
  const nodes = [
    { public_key: PK_KNOWN, name: 'Known 2B', role: 'repeater', lat: 55.676, lon: 12.568, last_seen: now, last_heard: now, hash_size: 2 },
    { public_key: PK_UNKNOWN, name: 'Unknown size', role: 'repeater', lat: 55.672, lon: 12.560, last_seen: now, last_heard: now },
  ];
  await page.route('**/api/**', (route) => {
    const p = new URL(route.request().url()).pathname;
    if (p === '/api/nodes') {
      return route.fulfill({ status: 200, contentType: 'application/json',
        body: JSON.stringify({ nodes, total: nodes.length, counts: { repeaters: 2, rooms: 0, companions: 0, sensors: 0 } }) });
    }
    if (p === '/api/observers') {
      return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ observers: [] }) });
    }
    return route.continue();
  });
  await page.goto(BASE + '/#/map', { waitUntil: 'load', timeout: 60000 });
  await page.waitForFunction(() => Array.isArray(window.__mc_nodes) && window.__mc_nodes.length === 2 && window.__mc_map);
  await page.waitForSelector('.mc-mb-label.hs-unknown', { timeout: 20000 });
  // Bring both markers into view on the small screen.
  await page.evaluate(() => new Promise((resolve) => {
    window.__mc_map.once('moveend', () => requestAnimationFrame(() => requestAnimationFrame(resolve)));
    window.__mc_map.setView([55.674, 12.564], 14, { animate: false });
  }));
  await page.waitForTimeout(500); // let any moveend-driven re-render settle
  const labels = await collectLabels(page);
  const known = labels.find((l) => l.key === PK_KNOWN);
  const unknown = labels.find((l) => l.key === PK_UNKNOWN);

  await step('2-byte repeater: label 961D, no unknown marker', async () => {
    assert(known, 'no label for the 2-byte repeater');
    assert(known.text === '961D' && !known.unknown && known.aria === 'repeater hash 961D', JSON.stringify(known));
  });

  await step('repeater without hash_size: label 42…, hs-unknown, aria says size unknown', async () => {
    assert(unknown, 'no label for the unknown-size repeater');
    assert(unknown.text === '42…' && unknown.unknown && unknown.aria === 'repeater hash 42, hash size unknown', JSON.stringify(unknown));
  });

  await step('the unknown label is visibly distinct (dashed outline, italic)', async () => {
    const style = await page.evaluate(() => {
      const el = document.querySelector('.mc-mb-label.hs-unknown');
      const cs = getComputedStyle(el);
      return { outline: cs.outlineStyle, fontStyle: cs.fontStyle };
    });
    assert(style.outline === 'dashed' && style.fontStyle === 'italic', JSON.stringify(style));
  });

  await step('tapping the unknown label opens a popup that says "size unknown", not "(1B)"', async () => {
    // A real tap at the label's centre (not a synthetic Leaflet event).
    const box = await page.locator('.mc-mb-label.hs-unknown').first().boundingBox();
    assert(box && box.x >= 0 && box.y >= 0 && box.x + box.width <= 375 && box.y + box.height <= 560,
      'unknown label not inside the 375x560 viewport: ' + JSON.stringify(box));
    await page.mouse.click(box.x + box.width / 2, box.y + box.height / 2);
    const popup = page.locator('.leaflet-popup-content');
    await popup.waitFor({ state: 'visible' });
    const text = await popup.innerText();
    assert(/size unknown/.test(text) && !/\(1B\)/.test(text), text);
    if (SHOTS) { await page.waitForTimeout(400); await page.screenshot({ path: path.join(SHOTS, '807-map-375x560-popup.png') }); } // popup fade-in
  });

  await step('no page errors', async () => {
    assert(errors.length === 0, errors.join(' | '));
  });
  await ctx.close();
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  try {
    await partA(browser);
    await partB(browser);
  } finally {
    await browser.close();
  }
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed > 0 ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
