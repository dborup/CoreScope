/**
 * E2E (#76): Heard By is split into "heard directly" and "heard via relay"
 * on every consumer of the node health observers: the node detail page, the
 * node side panel, the Live node panel, the home My Mesh card and the
 * Analytics node rankings. Runs against the Go server with the CI fixture
 * (no stubs), so the API shape and the UI are checked together:
 *   - the API has no mixed `observers` list, relay rows carry no SNR/RSSI,
 *     and single and bulk health agree;
 *   - direct rows show the node's own signal, relay rows show none;
 *   - counts on every page match the API.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-issue-76-heard-by-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }
const ids = rows => rows.map(r => r.observer_id).sort();
const norm = s => String(s || '').replace(/\s+/g, ' ').trim();

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  const ctx = await browser.newContext({ viewport: { width: 1400, height: 900 } });
  const page = await ctx.newPage();
  page.setDefaultTimeout(15000);
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));

  console.log('\n=== #76 Heard By direct/relay E2E against ' + BASE + ' ===');

  let bulk = [], target = null, health = null;
  await step('bulk-health: split lists, no mixed list, relay rows without signal', async () => {
    const res = await page.request.get(BASE + '/api/nodes/bulk-health?limit=200');
    assert(res.ok(), 'bulk-health returned ' + res.status());
    bulk = await res.json();
    assert(bulk.length > 0, 'fixture must have nodes');
    for (const e of bulk) {
      assert(!('observers' in e), 'mixed observers list must be gone: ' + e.public_key);
      assert(Array.isArray(e.directObservers) && Array.isArray(e.relayObservers), 'both lists must be arrays: ' + e.public_key);
      for (const r of e.relayObservers) assert(!('avgSnr' in r) && !('avgRssi' in r), 'relay row carries signal: ' + JSON.stringify(r));
      const seen = new Set(ids(e.directObservers));
      for (const r of e.relayObservers) assert(!seen.has(r.observer_id), 'observer in both lists: ' + r.observer_id);
    }
    // The node page, side panel and Live panel all need a node with both
    // kinds of observers and a position (Live map marker).
    target = bulk.filter(e => e.directObservers.length && e.relayObservers.length && e.lat != null && e.lon != null)
      .sort((a, b) => b.directObservers.length - a.directObservers.length || (a.public_key < b.public_key ? -1 : 1))[0];
    assert(target, 'fixture must have a positioned node with direct and relay observers');
  });

  await step('node health matches bulk-health for the target node', async () => {
    const res = await page.request.get(BASE + '/api/nodes/' + target.public_key + '/health');
    assert(res.ok(), 'health returned ' + res.status());
    health = await res.json();
    assert(!('observers' in health), 'mixed observers list must be gone');
    assert(JSON.stringify(ids(health.directObservers)) === JSON.stringify(ids(target.directObservers)), 'direct lists differ');
    assert(JSON.stringify(ids(health.relayObservers)) === JSON.stringify(ids(target.relayObservers)), 'relay lists differ');
    const snrs = health.directObservers.map(o => o.avgSnr).filter(v => v != null);
    if (snrs.length) assert(health.stats.avgSnr != null, 'stats.avgSnr must come from the direct receptions');
  });

  const total = () => health.directObservers.length + health.relayObservers.length;

  await step('node detail page: two tables, signal only on direct rows', async () => {
    await page.goto(BASE + '/#/nodes/' + target.public_key, { waitUntil: 'domcontentloaded' });
    const card = page.locator('#node-observers');
    await card.waitFor({ state: 'visible' });
    const title = norm(await card.locator('h4').textContent()); // CSS may upper-case innerText
    assert(title === `Heard By (${total()} observers)`, 'heading: ' + title);
    const directRows = card.locator('table[data-heard="direct"] tbody tr');
    const relayRows = card.locator('table[data-heard="relay"] tbody tr');
    assert(await directRows.count() === health.directObservers.length, 'direct row count');
    assert(await relayRows.count() === health.relayObservers.length, 'relay row count');
    assert(await card.locator('table[data-heard="relay"] th').count() === 2, 'relay table must have Observer and Packets only');
    assert(!/dB/.test(await card.locator('table[data-heard="relay"]').innerText()), 'relay table shows signal');
    const top = health.directObservers[0];
    const row = norm(await directRows.filter({ hasText: top.observer_name || top.observer_id }).first().innerText());
    if (top.avgSnr != null) assert(row.includes(Number(top.avgSnr).toFixed(1) + ' dB'), 'direct SNR missing: ' + row);
    // The help tooltip is reachable by keyboard.
    // The page may re-render once after load, which drops focus: retry.
    const tip = page.locator('#node-observers .heard-by-help .sort-help-tip').first();
    let shown = false;
    for (let i = 0; i < 10 && !shown; i++) {
      await page.locator('#node-observers .heard-by-help').first().focus();
      shown = await tip.evaluate(el => getComputedStyle(el).display !== 'none');
      if (!shown) await page.waitForTimeout(300);
    }
    assert(shown, 'direct help tooltip must show on focus');
    assert(/not a radio range/.test(await tip.textContent()), 'tooltip wording');
  });

  await step('side panel: compact rows per kind, signal only on direct rows', async () => {
    await page.goto(BASE + '/#/nodes', { waitUntil: 'domcontentloaded' });
    await page.fill('#nodeSearch', target.public_key.slice(0, 16));
    const sel = '#nodesBody tr[data-key="' + target.public_key + '"]';
    await page.waitForSelector(sel);
    await page.click(sel);
    const pane = page.locator('#nodesRight .heard-by');
    await pane.waitFor({ state: 'visible' });
    assert(await pane.locator('.observer-row[data-heard="direct"]').count() === health.directObservers.length, 'pane direct rows');
    const relayText = await pane.locator('.observer-row[data-heard="relay"]').allInnerTexts();
    assert(relayText.length === health.relayObservers.length, 'pane relay rows');
    for (const t of relayText) assert(!/SNR|RSSI|dB/.test(t), 'pane relay row shows signal: ' + t);
  });

  await step('home My Mesh card: direct names, relay count, dialog lists both', async () => {
    await page.goto(BASE + '/#/nodes', { waitUntil: 'domcontentloaded' });
    await page.evaluate((mn) => {
      localStorage.setItem('meshcore-user-level', 'experienced'); // skip the first-visit chooser
      localStorage.setItem('meshcore-my-nodes', JSON.stringify([mn]));
    }, { pubkey: target.public_key, name: target.name || 'Target', addedAt: Date.now() });
    await page.goto(BASE + '/#/home', { waitUntil: 'domcontentloaded' });
    const card = page.locator('.my-node-card[data-key="' + target.public_key + '"]');
    await card.waitFor({ state: 'visible' });
    const line = norm(await card.locator('.mnc-observers').innerText());
    assert(line.startsWith('Heard directly by:'), 'line: ' + line);
    assert(line.includes(health.relayObservers.length + ' via relay'), 'relay count: ' + line);
    const metric = card.locator('.mnc-metric', { hasText: 'Direct observers' });
    assert(norm(await metric.locator('.mnc-val').innerText()) === String(health.directObservers.length), 'direct metric');
    await card.locator('.mnc-view-all').click();
    const dialog = page.locator('#homeObserversDialog');
    await dialog.waitFor({ state: 'visible' });
    const items = await dialog.locator('li').allInnerTexts();
    assert(items.length === total(), 'dialog lists every observer: ' + items.length);
    assert(items.filter(t => t.endsWith('(via relay)')).length === health.relayObservers.length, 'relay items are marked');
    await page.keyboard.press('Escape');
    await page.evaluate(() => { localStorage.removeItem('meshcore-my-nodes'); localStorage.removeItem('meshcore-user-level'); });
  });

  await step('analytics: Most Observed ranks by direct observers', async () => {
    const res = await page.request.get(BASE + '/api/nodes/bulk-health?limit=50');
    const top50 = await res.json();
    const maxDirect = Math.max(...top50.map(e => e.directObservers.length));
    await page.goto(BASE + '/#/analytics?tab=nodes', { waitUntil: 'domcontentloaded' });
    const heading = page.locator('h3', { hasText: 'Most Observed Nodes' });
    await heading.waitFor({ state: 'visible', timeout: 30000 });
    const table = heading.locator('xpath=following-sibling::*[1]').locator('table').first();
    // The heading can be visible a frame before its table is (re)rendered.
    await table.locator('tbody tr').first().waitFor({ state: 'visible' });
    const headers = (await table.locator('th').allTextContents()).map(norm);
    assert(headers.includes('Direct observers') && headers.includes('Via relay') && headers.includes('Direct SNR'), 'headers: ' + headers);
    const col = headers.indexOf('Direct observers') + 1;
    const direct = (await table.locator(`tbody tr td:nth-child(${col})`).allInnerTexts()).map(Number);
    assert(direct.length > 0, 'ranking has rows');
    const relayCol = headers.indexOf('Via relay') + 1;
    const relay = (await table.locator(`tbody tr td:nth-child(${relayCol})`).allInnerTexts()).map(Number);
    for (let i = 1; i < direct.length; i++) {
      assert(direct[i] <= direct[i - 1], 'not ranked by direct observers: ' + direct);
      // Ties on direct observers are broken by relay observers, most first.
      if (direct[i] === direct[i - 1]) assert(relay[i] <= relay[i - 1], `tie on ${direct[i]} direct not ranked by relay: ${relay}`);
    }
    assert(direct[0] === maxDirect, `top row ${direct[0]} != max direct ${maxDirect}`);
  });

  await step('Live node panel: direct rows with signal, relay rows without', async () => {
    await page.goto(BASE + '/#/live', { waitUntil: 'domcontentloaded' });
    await page.waitForFunction((pk) => window._liveNodeMarkers && window._liveNodeMarkers()[pk], target.public_key, { timeout: 30000 });
    await page.evaluate((pk) => window._liveNodeMarkers()[pk].fire('click'), target.public_key);
    const panel = page.locator('#nodeDetailContent .heard-by');
    await panel.waitFor({ state: 'visible' });
    assert(await panel.locator('.observer-row[data-heard="direct"]').count() === health.directObservers.length, 'live direct rows');
    const relayText = await panel.locator('.observer-row[data-heard="relay"]').allInnerTexts();
    assert(relayText.length === health.relayObservers.length, 'live relay rows');
    for (const t of relayText) assert(!/SNR|RSSI|dB/.test(t), 'live relay row shows signal: ' + t);
    const first = health.directObservers[0];
    const href = await panel.locator('.observer-row[data-heard="direct"] a').first().getAttribute('href');
    assert(href === '#/observers/' + encodeURIComponent(first.observer_id), 'observer link: ' + href);
  });

  await step('no page errors', async () => {
    assert(errors.length === 0, 'page errors: ' + errors.join(' | '));
  });

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
