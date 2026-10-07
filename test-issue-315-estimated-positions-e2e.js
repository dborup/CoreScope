#!/usr/bin/env node
/**
 * #315: real Go server + real Leaflet, isolated synthetic SQLite data.
 * No production endpoint, MQTT broker, API mock, or external browser request.
 *
 * Build cmd/server and cmd/migrate first, then run with:
 * CORESCOPE_SERVER_BIN=/absolute/server CORESCOPE_MIGRATE_BIN=/absolute/migrate
 * CORESCOPE_LEAFLET_DIR=/absolute/leaflet-1.9.4/dist
 * node test-issue-315-estimated-positions-e2e.js
 *
 * Leaflet assets must already exist locally; this test never downloads them.
 * SCREENSHOT_DIR optionally selects an output directory. CHROMIUM_PATH can
 * select a preinstalled browser; Playwright's installed Chromium is default.
 */
'use strict';

const assert = require('assert/strict');
const fs = require('fs');
const os = require('os');
const path = require('path');
const net = require('net');
const { spawn, execFileSync } = require('child_process');
const { chromium } = require('playwright');

const ROOT = __dirname;
const REPORTED = '3151' + '0'.repeat(60);
const MISSING = '3152' + '0'.repeat(60);
const UNKNOWN = '3155' + '0'.repeat(60);
const HASH = 'e2e3150000000001';
const DISABLED = 'Estimated positions are disabled by the instance operator.';
const delay = (ms) => new Promise(resolve => setTimeout(resolve, ms));

function requiredFile(variable, suffix = '') {
  assert(process.env[variable], variable + ' is required; see the test header');
  const file = path.resolve(process.env[variable], suffix);
  assert(fs.statSync(file).isFile(), file + ' must be a file');
  return file;
}

async function freePort() {
  const socket = net.createServer();
  await new Promise((resolve, reject) => { socket.once('error', reject); socket.listen(0, '127.0.0.1', resolve); });
  const port = socket.address().port;
  await new Promise(resolve => socket.close(resolve));
  return port;
}

async function stop(child) {
  if (!child || child.exitCode !== null || child.signalCode !== null) return;
  const ended = new Promise(resolve => child.once('exit', resolve));
  child.kill('SIGTERM');
  const timer = setTimeout(() => child.kill('SIGKILL'), 5000);
  await ended;
  clearTimeout(timer);
}

async function json(base, endpoint) {
  const response = await fetch(base + endpoint, { signal: AbortSignal.timeout(5000) });
  assert.equal(response.status, 200, endpoint + ': ' + response.status);
  return response.json();
}

async function waitReady(base, child, getLog) {
  for (let n = 0; n < 100; n++) {
    assert(child.exitCode === null && child.signalCode === null, 'server exited: ' + getLog());
    try { await json(base, '/api/config/client'); return; } catch (_) { await delay(100); }
  }
  throw new Error('server startup timed out: ' + getLog());
}

async function apiChecks(base, enabled) {
  const config = await json(base, '/api/config/client');
  assert.equal(config.estimatedPositions.enabled, enabled);
  for (const key of [REPORTED, MISSING]) {
    const { node } = await json(base, '/api/nodes/' + key + '?estimatedNodes=1&estimatedPositions=true');
    assert.equal(node.public_key, key);
    if (key === REPORTED) { assert.equal(node.lat, 56.5); assert.equal(node.lon, 12); }
    else { assert(node.lat == null && node.lon == null, 'reported GPS must not be invented'); }
    if (enabled) {
      assert(Number.isFinite(node.estimated_lat) && Number.isFinite(node.estimated_lon));
      assert.equal(node.estimated_contributor_count, 2);
    } else {
      assert(!Object.keys(node).some(k => k.startsWith('estimated_')), 'disabled node leaked an estimate');
    }
  }
  const packet = await json(base, '/api/packets/' + HASH + '/path?estimatedNodes=1');
  assert(packet.branches.length > 0, 'fixture must exercise a real resolved path');
  for (const branch of [packet.first, ...packet.branches]) {
    assert.deepEqual(branch.points.map(p => p.publicKey), [REPORTED, MISSING, UNKNOWN]);
    const [real, missing, unknown] = branch.points;
    assert.equal(real.lat, 56.5); assert.equal(real.lon, 12); assert(!real.approx);
    assert.equal(branch.observer.lat, 55); assert.equal(branch.observer.lon, 12);
    assert.equal(Boolean(missing.approx), enabled);
    if (enabled) assert(Number.isFinite(missing.lat));
    else assert(missing.lat == null && missing.lon == null);
    assert(unknown.lat == null && unknown.lon == null && !unknown.approx);
  }
  const areas = await json(base, '/api/analytics/areas?estimatedNodes=1');
  assert(areas.density.some(a => a.total > 0), 'real-GPS area density must survive');
  assert(Array.isArray(areas.bridgeNodes), 'ordinary bridge analytics must survive');
  const sanity = await json(base, '/api/analytics/gps-sanity');
  if (enabled) {
    assert(areas.estimatedNodes.some(n => n.publicKey === MISSING));
    assert(areas.positionGaps.some(a => a.approximated > 0));
    assert(sanity.nodes.some(n => n.publicKey === REPORTED), 'fixture must produce a genuine sanity result');
  } else {
    assert.equal(areas.estimatedPositionsEnabled, false);
    for (const key of ['estimatedNodes', 'positionGaps', 'unpositionedNoNeighborFix']) assert(!(key in areas));
    assert.deepEqual(sanity, { estimatedPositionsEnabled: false });
  }
}

async function browserChecks(browser, base, enabled, mode, assets, shots) {
  const context = await browser.newContext({ viewport: { width: 1360, height: 980 }, serviceWorkers: 'block' });
  // Serve exactly the already-used Leaflet version, preserving the index's
  // integrity checks. Other external resources (including map tiles) cannot
  // leave the browser; no API endpoint is intercepted or fulfilled here.
  await context.route('**/*', async route => {
    const url = new URL(route.request().url());
    if (url.origin === base) return route.continue();
    const asset = assets[url.href];
    if (asset) return route.fulfill({ path: asset, headers: { 'access-control-allow-origin': '*' } });
    return route.abort();
  });
  await context.addInitScript(() => {
    localStorage.setItem('meshcore-theme', 'light');
    localStorage.setItem('map-estimated-nodes', 'true');
    localStorage.setItem('estimatedPositions', JSON.stringify({ enabled: true }));
    localStorage.setItem('estimatedPositions.enabled', 'true');
  });
  const page = await context.newPage();
  page.setDefaultTimeout(10000);
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  async function go(hash) {
    await page.goto(base + '/' + hash, { waitUntil: 'load' });
    await page.waitForFunction(() => window.L && window.L.version === '1.9.4');
    await page.evaluate(() => window.MeshConfigReady);
  }
  async function screenshot(name) {
    if (shots) await page.screenshot({ path: path.join(shots, '315-' + mode + '-' + name + '.png'), fullPage: false, animations: 'disabled' });
  }
  async function nodeState(selector, hasGPS) {
    await page.waitForSelector(selector);
    if (enabled) {
      await page.waitForFunction(sel => document.querySelector(sel).textContent.includes('(estimated)'), selector);
    } else {
      await page.waitForSelector(selector + ' [data-estimated-positions-disabled]');
      assert((await page.textContent(selector)).includes(DISABLED));
      assert(!(await page.textContent(selector)).includes('Neighbor Estimate'));
    }
    const mapSelector = selector === '#nodeFullBody' ? '#nodeFullMap' : '#nodeMap';
    if (hasGPS || enabled) {
      await page.waitForSelector(mapSelector + '.leaflet-container');
      if (hasGPS) await page.waitForSelector(mapSelector + ' .leaflet-marker-icon');
      if (enabled) await page.waitForSelector(mapSelector + ' path[stroke-dasharray]');
      assert.equal(await page.locator(mapSelector + ' path[stroke-dasharray]').count(), enabled ? (hasGPS ? 2 : 1) : 0);
    } else assert.equal(await page.locator(mapSelector).count(), 0);
    if (hasGPS) assert((await page.textContent(selector)).includes('56.50000'));
  }
  try {
    await go('#/nodes/' + REPORTED);
    await nodeState('#nodeFullBody', true);
    await screenshot('reported-full');
    await go('#/nodes/' + MISSING);
    await nodeState('#nodeFullBody', false);
    await screenshot('missing-full');
    await go('#/nodes?search=Position%20Policy');
    await page.click('tr[data-key="' + REPORTED + '"]');
    await nodeState('.node-detail', true);
    await screenshot('reported-pane');
    await go('#/nodes?search=Position%20Policy');
    await page.click('tr[data-key="' + MISSING + '"]');
    await nodeState('.node-detail', false);
    await screenshot('missing-pane');
    await go('#/packets/' + HASH + '?viewPath=1');
    await page.waitForSelector('#packetPathModal .leaflet-container');
    await page.waitForSelector('#packetPathModal path.leaflet-interactive');
    if (!enabled) {
      assert((await page.textContent('#packetPathEstimatePolicy')).includes(DISABLED));
      assert.equal(await page.locator('#packetPathApproxLegend').isVisible(), false);
      assert.equal(await page.locator('#packetPathModal path[stroke-dasharray]').count(), 0);
    } else {
      assert.equal(await page.locator('#packetPathApproxLegend').isVisible(), true);
      assert(await page.locator('#packetPathModal path[stroke-dasharray]').count() > 0);
    }
    await screenshot('packet-path');
    await go('#/map?estimatedNodes=1');
    await page.waitForSelector('.mc-estimated-nodes-label');
    const label = await page.textContent('.mc-estimated-nodes-label');
    if (!enabled) assert(label.includes(DISABLED));
    else assert(!label.includes(DISABLED));
    const markers = await page.evaluate(() => {
      const out = []; window.__mc_map.eachLayer(l => {
        if (l instanceof L.CircleMarker || l instanceof L.Marker) out.push({ lat: l.getLatLng().lat, approx: Boolean(l.options.dashArray) });
      }); return out;
    });
    if (!enabled) assert(markers.some(m => m.lat === 56.5 && !m.approx), 'reported GPS map marker must remain');
    assert.equal(markers.some(m => m.approx), enabled, 'query/localStorage must not override server policy');
    await screenshot('map');
    await go('#/tools/position-gaps');
    await page.waitForFunction(() => !document.querySelector('#position-gaps-content').textContent.includes('Loading'));
    if (!enabled) {
      assert((await page.textContent('#position-gaps-content')).includes(DISABLED));
      assert.equal(await page.locator('#position-gaps-est-table').count(), 0);
    } else await page.waitForSelector('#position-gaps-est-table');
    await screenshot('position-gaps');
    await go('#/tools/gps-sanity');
    await page.waitForFunction(() => !document.querySelector('#gps-sanity-content').textContent.includes('Loading'));
    if (!enabled) {
      assert((await page.textContent('#gps-sanity-content')).includes(DISABLED));
      assert.equal(await page.locator('#gps-sanity-table').count(), 0);
    } else await page.waitForSelector('#gps-sanity-table');
    await screenshot('gps-sanity');
    await go('#/analytics?tab=areas');
    await page.waitForSelector('#areasDensity table');
    if (!enabled) {
      assert((await page.textContent('#areasPositionGaps')).includes(DISABLED));
      assert.equal(await page.locator('#areasViewEstimatedNodes').count(), 0);
    } else await page.waitForSelector('#areasViewEstimatedNodes');
    await screenshot('areas');
    if (!enabled) {
      await page.setViewportSize({ width: 390, height: 844 });
      await go('#/nodes/' + REPORTED);
      await nodeState('#nodeFullBody', true);
      await page.locator('[data-estimated-positions-disabled]').scrollIntoViewIfNeeded();
      await screenshot('reported-mobile');
    }
    assert.deepEqual(errors, [], 'browser runtime errors');
  } catch (error) {
    await screenshot('failure');
    throw error;
  } finally { await context.close(); }
}

(async () => {
  const serverBin = requiredFile('CORESCOPE_SERVER_BIN');
  const migrateBin = requiredFile('CORESCOPE_MIGRATE_BIN');
  const assets = {
    'https://unpkg.com/leaflet@1.9.4/dist/leaflet.js': requiredFile('CORESCOPE_LEAFLET_DIR', 'leaflet.js'),
    'https://unpkg.com/leaflet@1.9.4/dist/leaflet.css': requiredFile('CORESCOPE_LEAFLET_DIR', 'leaflet.css'),
  };
  for (const image of ['marker-icon.png', 'marker-icon-2x.png', 'marker-shadow.png']) {
    const local = path.join(process.env.CORESCOPE_LEAFLET_DIR, 'images', image);
    if (fs.existsSync(local)) assets['https://unpkg.com/leaflet@1.9.4/dist/images/' + image] = local;
  }
  const scratch = fs.mkdtempSync(path.join(os.tmpdir(), 'corescope-315-e2e-'));
  const shots = process.env.SCREENSHOT_DIR && path.resolve(process.env.SCREENSHOT_DIR);
  if (shots) fs.mkdirSync(shots, { recursive: true });
  let browser, child;
  try {
    const database = path.join(scratch, 'synthetic.db');
    // Copy only the tracked fixture's schema: none of its real mesh records
    // enter this suite. Migrations remain owned by the dedicated Go binary.
    const schema = execFileSync('sqlite3', [path.join(ROOT, 'test-fixtures/e2e-fixture.db'),
      "SELECT sql || ';' FROM sqlite_master WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%' ORDER BY CASE type WHEN 'table' THEN 0 ELSE 1 END, rowid"]);
    execFileSync('sqlite3', [database], { input: schema });
    execFileSync(migrateBin, ['-db', database], { stdio: 'pipe' });
    execFileSync('sqlite3', [database], { input: fs.readFileSync(path.join(ROOT, 'test-fixtures/seed-315-estimated-positions.sql')) });
    browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_PATH || undefined });
    for (const mode of ['default', 'enabled', 'disabled']) {
      const enabled = mode !== 'disabled';
      const config = {
        areas: { synthetic: { label: 'Synthetic Test Area', latMin: 54, latMax: 58, lonMin: 11, lonMax: 16 } },
        mapDefaults: { center: [55.7, 12], zoom: 7 }, mqtt: { brokers: [] },
      };
      if (mode !== 'default') config.estimatedPositions = { enabled };
      fs.writeFileSync(path.join(scratch, 'config.json'), JSON.stringify(config));
      const port = await freePort();
      const base = 'http://127.0.0.1:' + port;
      let log = '';
      child = spawn(serverBin, ['-config-dir', scratch, '-db', database, '-public', path.join(ROOT, 'public'), '-port', String(port)], { cwd: ROOT, stdio: ['ignore', 'pipe', 'pipe'] });
      child.stdout.on('data', chunk => { log = (log + chunk).slice(-30000); });
      child.stderr.on('data', chunk => { log = (log + chunk).slice(-30000); });
      await waitReady(base, child, () => log);
      await apiChecks(base, enabled);
      await browserChecks(browser, base, enabled, mode, assets, shots);
      console.log('PASS #315 ' + mode + ': real API, node detail/pane, map, tools, areas');
      await stop(child); child = null;
    }
  } finally {
    await stop(child);
    if (browser) await browser.close();
    fs.rmSync(scratch, { recursive: true, force: true });
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
