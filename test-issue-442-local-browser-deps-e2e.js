#!/usr/bin/env node
/**
 * E2E (#442): charts, the live heatmap and Swagger UI must work against a
 * real local Go server with EVERY off-origin network request blocked.
 *
 * Before #442 (#336/#400), Leaflet, Leaflet.heat, Chart.js and swagger-ui-dist
 * were pinned+hashed but still fetched from unpkg.com: with outbound network
 * blocked, `#/live` loses its map/heatmap, `#/nodes/<pk>/analytics` loses its
 * charts, and `/api/docs` renders empty (this is the "Failure mode when the
 * CDN is unreachable" documented in docs/browser-dependencies.md pre-#442).
 * This test proves the opposite now holds: with every off-origin request
 * aborted by the browser itself, all three surfaces still render correctly,
 * because the six dependencies are vendored same-origin under public/vendor/.
 *
 * Build cmd/server and cmd/migrate first, then run with:
 *   CORESCOPE_SERVER_BIN=/absolute/corescope-server \
 *   CORESCOPE_MIGRATE_BIN=/absolute/corescope-migrate \
 *   node test-issue-442-local-browser-deps-e2e.js
 *
 * CHROMIUM_PATH optionally selects a preinstalled browser.
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
const NODE_KEY = process.env.NODE_KEY ||
  '988396af0ff369c2178a8c2a303661bf26193257136ed4452b6eadd9547011e9';
const delay = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

function requiredFile(variable) {
  assert(process.env[variable], variable + ' is required; see the test header');
  const file = path.resolve(process.env[variable]);
  assert(fs.statSync(file).isFile(), file + ' must be a file');
  return file;
}

async function freePort() {
  const socket = net.createServer();
  await new Promise((resolve, reject) => { socket.once('error', reject); socket.listen(0, '127.0.0.1', resolve); });
  const port = socket.address().port;
  await new Promise((resolve) => socket.close(resolve));
  return port;
}

async function stop(child) {
  if (!child || child.exitCode !== null || child.signalCode !== null) return;
  const ended = new Promise((resolve) => child.once('exit', resolve));
  child.kill('SIGTERM');
  const timer = setTimeout(() => child.kill('SIGKILL'), 5000);
  await ended;
  clearTimeout(timer);
}

async function waitReady(base, child, getLog) {
  for (let n = 0; n < 100; n++) {
    assert(child.exitCode === null && child.signalCode === null, 'server exited: ' + getLog());
    try {
      const res = await fetch(base + '/api/config/client', { signal: AbortSignal.timeout(2000) });
      if (res.status === 200) return;
    } catch (_) { /* retry */ }
    await delay(100);
  }
  throw new Error('server startup timed out: ' + getLog());
}

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}

(async () => {
  const serverBin = requiredFile('CORESCOPE_SERVER_BIN');
  const migrateBin = requiredFile('CORESCOPE_MIGRATE_BIN');

  const scratch = fs.mkdtempSync(path.join(os.tmpdir(), 'corescope-442-e2e-'));
  const database = path.join(scratch, 'fixture.db');
  let browser, child;
  try {
    fs.copyFileSync(path.join(ROOT, 'test-fixtures/e2e-fixture.db'), database);
    execFileSync(migrateBin, ['-db', database], { stdio: 'pipe' });

    const port = await freePort();
    const base = 'http://127.0.0.1:' + port;
    let log = '';
    child = spawn(serverBin, ['-db', database, '-public', path.join(ROOT, 'public'), '-port', String(port)],
      { cwd: ROOT, stdio: ['ignore', 'pipe', 'pipe'] });
    child.stdout.on('data', (chunk) => { log = (log + chunk).slice(-30000); });
    child.stderr.on('data', (chunk) => { log = (log + chunk).slice(-30000); });
    await waitReady(base, child, () => log);

    browser = await chromium.launch({
      headless: true,
      executablePath: process.env.CHROMIUM_PATH || undefined,
      args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
    });

    console.log('\n=== #442 local browser dependencies (outbound network blocked) ===\n');

    // Hosts the six #442 dependencies used to be fetched from. A request to
    // either here would mean a dependency is still CDN-loaded; this is the
    // one thing this test must prove never happens again. Other off-origin
    // requests (map tiles -- out of scope, see #332 and
    // docs/browser-dependencies.md) are aborted the same way outbound
    // network blocked would, but are not themselves a failure.
    const DEPENDENCY_CDN_HOSTS = new Set(['unpkg.com', 'cdn.jsdelivr.net']);

    async function withBlockedNetwork(fn) {
      const context = await browser.newContext({ viewport: { width: 1280, height: 900 }, serviceWorkers: 'block' });
      const dependencyCDNRequests = [];
      await context.route('**/*', async (route) => {
        const url = new URL(route.request().url());
        if (url.origin === base) return route.continue();
        // Every off-origin request is aborted -- this is "outbound network
        // blocked". A request to a dependency CDN host is recorded as the
        // failure this test watches for; anything else (e.g. map tiles) is
        // just blocked, as it would be on an air-gapped install.
        if (DEPENDENCY_CDN_HOSTS.has(url.hostname)) dependencyCDNRequests.push(url.href);
        return route.abort();
      });
      const page = await context.newPage();
      page.setDefaultTimeout(10000);
      const pageErrors = [];
      page.on('pageerror', (error) => pageErrors.push(error.message));
      const consoleErrors = [];
      page.on('console', (msg) => { if (msg.type() === 'error') consoleErrors.push(msg.text()); });
      try {
        await fn(page);
      } finally {
        await context.close();
      }
      return { dependencyCDNRequests, pageErrors, consoleErrors };
    }

    await step('#/live loads Leaflet and the heatmap plugin locally', async () => {
      const { dependencyCDNRequests, pageErrors, consoleErrors } = await withBlockedNetwork(async (page) => {
        await page.goto(base + '/#/live', { waitUntil: 'load' });
        await page.waitForFunction(() => window.L && window.L.version === '1.9.4');
        const hasHeat = await page.evaluate(() => typeof window.L.heatLayer === 'function');
        assert.equal(hasHeat, true, 'L.heatLayer did not load (leaflet.heat blocked?)');
      });
      assert.deepEqual(dependencyCDNRequests, [], 'dependency CDN request(s) made:\n    ' + dependencyCDNRequests.join('\n    '));
      assert.deepEqual(pageErrors, [], 'browser runtime errors: ' + pageErrors.join('; '));
      const sriErrors = consoleErrors.filter((m) => /integrity|CORS|cross-origin/i.test(m));
      assert.deepEqual(sriErrors, [], 'SRI/CORS console errors: ' + sriErrors.join('; '));
    });

    await step('#/nodes/<pk>/analytics renders Chart.js charts locally', async () => {
      const { dependencyCDNRequests, pageErrors, consoleErrors } = await withBlockedNetwork(async (page) => {
        await page.goto(base + '/#/nodes/' + NODE_KEY + '/analytics', { waitUntil: 'load' });
        await page.waitForFunction(() => window.Chart && window.Chart.version);
        const version = await page.evaluate(() => window.Chart.version);
        assert(version.startsWith('4.'), 'unexpected Chart.js version: ' + version);
        await page.waitForSelector('#activityChart');
        const chartCount = await page.evaluate(() => {
          const canvases = ['activityChart', 'snrChart', 'packetTypeChart'];
          return canvases.filter((id) => {
            const el = document.getElementById(id);
            return el && window.Chart.getChart && window.Chart.getChart(el);
          }).length;
        });
        assert(chartCount > 0, 'no live Chart.js instance found on activityChart/snrChart/packetTypeChart');
      });
      assert.deepEqual(dependencyCDNRequests, [], 'dependency CDN request(s) made:\n    ' + dependencyCDNRequests.join('\n    '));
      assert.deepEqual(pageErrors, [], 'browser runtime errors: ' + pageErrors.join('; '));
      const sriErrors = consoleErrors.filter((m) => /integrity|CORS|cross-origin/i.test(m));
      assert.deepEqual(sriErrors, [], 'SRI/CORS console errors: ' + sriErrors.join('; '));
    });

    await step('/api/docs renders Swagger UI locally', async () => {
      const { dependencyCDNRequests, pageErrors, consoleErrors } = await withBlockedNetwork(async (page) => {
        await page.goto(base + '/api/docs', { waitUntil: 'load' });
        await page.waitForFunction(() => window.SwaggerUIBundle);
        await page.waitForSelector('.opblock', { timeout: 10000 });
        const count = await page.locator('.opblock').count();
        assert(count > 0, 'Swagger UI rendered no operations');
      });
      assert.deepEqual(dependencyCDNRequests, [], 'dependency CDN request(s) made:\n    ' + dependencyCDNRequests.join('\n    '));
      assert.deepEqual(pageErrors, [], 'browser runtime errors: ' + pageErrors.join('; '));
      const sriErrors = consoleErrors.filter((m) => /integrity|CORS|cross-origin/i.test(m));
      assert.deepEqual(sriErrors, [], 'SRI/CORS console errors: ' + sriErrors.join('; '));
    });

    console.log(`\n${passed} passed, ${failed} failed`);
    process.exitCode = failed ? 1 : 0;
  } finally {
    await stop(child);
    if (browser) await browser.close();
    fs.rmSync(scratch, { recursive: true, force: true });
  }
})().catch((error) => { console.error(error); process.exitCode = 1; });
