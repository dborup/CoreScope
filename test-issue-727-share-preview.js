#!/usr/bin/env node
/**
 * E2E (#727 M3): link previews of shared terrain profiles, end to end on
 * a real Go server: /p's Open Graph tags point at the share card,
 * GET /api/terrain/profile.png, and the card is a real 1200x630 PNG.
 *
 * The suite starts its own server (CORESCOPE_SERVER_BIN) on a copy of
 * test-fixtures/e2e-fixture.db, with sharing on, a dummy DHM token and
 * map.dhm.profile.share.upstreamPerMin 0, so the server never contacts the
 * elevation service: the fixture's nodes are in California, outside DHM,
 * which needs no elevation call at all (a complete card), and a path in
 * Denmark answers with a degraded card because the budget is zero.
 *
 * Asserts:
 *   1. /p fetched with a crawler User-Agent (no JavaScript) carries
 *      og:image = twitter:image = an absolute card URL with v, 1200x630,
 *      og:image:alt naming both nodes, og:title without a verdict word
 *      (the path is outside DHM, so it has none), and robots noindex;
 *   2. that URL is image/png, 1200x630 by its IHDR, nosniff, immutable,
 *      X-CoreScope-Card miss then hit with the same bytes;
 *   3. changing ha changes og:image and the card's bytes;
 *   4. a Danish path with no budget is a degraded card (max-age=60), and
 *      its bytes change with ha too; no elevation request is made;
 *   5. an unknown node is a 404 on the card and on /p, naming nothing;
 *   6. Chromium loads the og:image at 1440 and 360 px wide (naturalWidth
 *      1200), in the dark and the light card theme, and the two themes
 *      draw different pixels.
 *
 * Usage:
 *   (cd cmd/server && go build -o ../../corescope-server .)
 *   (cd cmd/migrate && go build -o ../../corescope-migrate .)
 *   CORESCOPE_SERVER_BIN=./corescope-server CORESCOPE_MIGRATE_BIN=./corescope-migrate \
 *     node test-issue-727-share-preview.js
 * The fixture copy is migrated with CORESCOPE_MIGRATE_BIN, as CI migrates
 * the fixture before any E2E suite (#1287); in CI that is already done and
 * the second run is a no-op.
 * Optional: E2E_PORT_BASE (default 13900) is the first port tried;
 * SCREENSHOT_DIR writes the PR screenshots and a card mock-up.
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
const CRAWLER = 'Mozilla/5.0 (compatible; Discordbot/2.0; +https://discordapp.com)';
const NODE_A = '988396af0ff369c2178a8c2a303661bf26193257136ed4452b6eadd9547011e9'; // KN6PLV-BrkOxfLA-Yebes
const NODE_B = '300ddc9e145dbe6cb4d6c2d93c74e9f862f5e247783f64ef89a4732e68dd473c'; // R5-D4 300D Rak
const DK_A = '56.16290,10.20390';
const DK_B = '56.16290,10.25000';
const delay = (ms) => new Promise(resolve => setTimeout(resolve, ms));

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}

function requiredFile(variable) {
  assert(process.env[variable], variable + ' is required; see the test header');
  const file = path.resolve(process.env[variable]);
  assert(fs.statSync(file).isFile(), file + ' must be a file');
  return file;
}

// freePort is the first port from E2E_PORT_BASE up that can be bound:
// other suites and servers may run on this machine.
async function freePort() {
  const base = Number(process.env.E2E_PORT_BASE || 13900);
  for (let port = base; port < base + 200; port++) {
    const ok = await new Promise(resolve => {
      const s = net.createServer();
      s.once('error', () => resolve(false));
      s.listen(port, '127.0.0.1', () => s.close(() => resolve(true)));
    });
    if (ok) return port;
  }
  throw new Error('no free port from ' + base);
}

async function stop(child) {
  if (!child || child.exitCode !== null || child.signalCode !== null) return;
  const ended = new Promise(resolve => child.once('exit', resolve));
  child.kill('SIGTERM');
  const timer = setTimeout(() => child.kill('SIGKILL'), 5000);
  await ended;
  clearTimeout(timer);
}

async function startServer(serverBin, scratch, theme) {
  const dir = path.join(scratch, theme);
  fs.mkdirSync(dir, { recursive: true });
  const db = path.join(dir, 'fixture.db');
  fs.copyFileSync(path.join(ROOT, 'test-fixtures/e2e-fixture.db'), db);
  execFileSync(requiredFile('CORESCOPE_MIGRATE_BIN'), ['-db', db], { stdio: 'pipe' });
  fs.writeFileSync(path.join(dir, 'config.json'), JSON.stringify({
    mqtt: { brokers: [] },
    map: { dhm: {
      enabled: true, token: 'e2e-dummy-not-a-token',
      profile: { enabled: true, share: { enabled: true, upstreamPerMin: 0 }, card: { theme } },
    } },
  }));
  const port = await freePort();
  const base = 'http://127.0.0.1:' + port;
  let log = '';
  const child = spawn(serverBin, ['-config-dir', dir, '-db', db, '-public', path.join(ROOT, 'public'), '-port', String(port)],
    { cwd: dir, stdio: ['ignore', 'pipe', 'pipe'] });
  child.stdout.on('data', c => { log = (log + c).slice(-60000); });
  child.stderr.on('data', c => { log = (log + c).slice(-60000); });
  for (let n = 0; n < 150; n++) {
    assert(child.exitCode === null && child.signalCode === null, 'server exited: ' + log);
    try {
      const r = await fetch(base + '/api/config/client', { signal: AbortSignal.timeout(2000) });
      if (r.ok) return { child, base, log: () => log };
    } catch (_) { /* not up yet */ }
    await delay(100);
  }
  throw new Error('server startup timed out: ' + log);
}

async function get(url, headers = {}) {
  const r = await fetch(url, { headers: Object.assign({ 'User-Agent': CRAWLER }, headers), redirect: 'manual', signal: AbortSignal.timeout(15000) });
  return { status: r.status, headers: r.headers, body: Buffer.from(await r.arrayBuffer()) };
}

function unescapeHTML(s) {
  return s.replace(/&#34;|&quot;/g, '"').replace(/&#39;/g, "'").replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&amp;/g, '&');
}

// meta returns the one <meta property|name="key" content="..."> value.
function meta(html, key) {
  const re = new RegExp('<meta (?:property|name)="' + key.replace(/[.:]/g, '\\$&') + '" content="([^"<>]*)">', 'g');
  const all = [...html.matchAll(re)];
  assert.equal(all.length, 1, 'exactly one ' + key + ' tag');
  return unescapeHTML(all[0][1]);
}

function pngSize(buf) {
  assert(buf.subarray(0, 8).equals(Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a])), 'PNG signature');
  assert.equal(buf.subarray(12, 16).toString('latin1'), 'IHDR');
  return { width: buf.readUInt32BE(16), height: buf.readUInt32BE(20) };
}

function shareURL(base, a, b, extra = '') {
  return base + '/p?profile=' + encodeURIComponent(a + ',' + b) + extra;
}

async function preview(base, a, b, extra) {
  const r = await get(shareURL(base, a, b, extra));
  assert.equal(r.status, 200, '/p status');
  const html = r.body.toString('utf8');
  assert(!html.includes('__OG_'), 'unfilled placeholder');
  return {
    html, title: meta(html, 'og:title'), image: meta(html, 'og:image'),
    twitterImage: meta(html, 'twitter:image'), alt: meta(html, 'og:image:alt'),
    width: meta(html, 'og:image:width'), height: meta(html, 'og:image:height'), robots: meta(html, 'robots'),
  };
}

async function card(url, state) {
  const r = await get(url);
  assert.equal(r.status, 200, 'card status for ' + url + ': ' + r.body.toString('utf8').slice(0, 200));
  assert.equal(r.headers.get('content-type'), 'image/png');
  assert.equal(r.headers.get('x-content-type-options'), 'nosniff');
  assert.equal(r.headers.get('x-corescope-card'), state);
  assert.equal(r.headers.get('cache-control'), state === 'degraded' ? 'public, max-age=60' : 'public, max-age=86400, immutable');
  assert.deepEqual(pngSize(r.body), { width: 1200, height: 630 });
  return r.body;
}

async function browserChecks(browser, base, imageURL, theme, shots) {
  for (const width of [1440, 360]) {
    const context = await browser.newContext({ viewport: { width, height: 900 }, colorScheme: theme === 'light' ? 'light' : 'dark' });
    const page = await context.newPage();
    const errors = [];
    page.on('pageerror', e => errors.push(e.message));
    const resp = await page.goto(imageURL);
    assert.equal(resp.status(), 200);
    const natural = await page.evaluate(() => { const i = document.querySelector('img'); return i ? [i.naturalWidth, i.naturalHeight] : null; });
    assert.deepEqual(natural, [1200, 630], 'Chromium decodes the card at ' + width + ' px');
    if (shots) await page.screenshot({ path: path.join(shots, `card-${theme}-${width}.png`), fullPage: true });
    assert.deepEqual(errors, []);
    await context.close();
  }
}

// mockUp renders the tags of a /p page as a chat-style link preview (a
// stand-in for Discord or Slack, which need a public host) and screenshots
// it: the image, title, description and site name as a crawler reads them.
async function mockUp(browser, p, shots) {
  const page = await browser.newPage({ viewport: { width: 760, height: 620 } });
  const esc = s => s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/"/g, '&quot;');
  await page.setContent(`<!doctype html><meta charset="utf-8"><body style="background:#313338;font:15px system-ui;color:#dbdee1;padding:24px">
    <div style="max-width:520px;border-left:4px solid #1e1f22;background:#2b2d31;border-radius:4px;padding:12px 16px">
    <div style="font-size:12px;color:#b5bac1">${esc(meta(p.html, 'og:site_name'))}</div>
    <div style="color:#00a8fc;font-weight:600;margin:6px 0">${esc(p.title)}</div>
    <div style="font-size:14px;margin-bottom:10px">${esc(meta(p.html, 'og:description'))}</div>
    <img src="${esc(p.image)}" alt="${esc(p.alt)}" style="width:100%;border-radius:4px"></div></body>`);
  await page.waitForFunction(() => document.querySelector('img').complete && document.querySelector('img').naturalWidth > 0);
  await page.screenshot({ path: path.join(shots, 'preview-mockup.png') });
  await page.close();
}

(async () => {
  const serverBin = requiredFile('CORESCOPE_SERVER_BIN');
  const scratch = fs.mkdtempSync(path.join(os.tmpdir(), 'corescope-727-preview-'));
  const shots = process.env.SCREENSHOT_DIR && path.resolve(process.env.SCREENSHOT_DIR);
  if (shots) fs.mkdirSync(shots, { recursive: true });
  let browser, server;
  const darkCards = {};
  try {
    browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_PATH || undefined, args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'] });
    for (const theme of ['dark', 'light']) {
      console.log(`\n=== #727 share preview, ${theme} card ===`);
      server = await startServer(serverBin, scratch, theme);
      const base = server.base;
      let p, first;

      await step('/p tags point at the card (crawler, no JavaScript)', async () => {
        p = await preview(base, NODE_A, NODE_B);
        assert.equal(p.image, p.twitterImage);
        const u = new URL(p.image);
        assert.equal(u.origin, base);
        assert.equal(u.pathname, '/api/terrain/profile.png');
        assert.equal(u.searchParams.get('a'), NODE_A);
        assert.equal(u.searchParams.get('b'), NODE_B);
        assert.match(u.searchParams.get('v'), /^[0-9a-f]{12}$/);
        assert.equal(p.width, '1200');
        assert.equal(p.height, '630');
        assert.equal(p.robots, 'noindex');
        // Outside DHM there is nothing to fetch: the profile is ready at
        // once, and has no verdict, so the title carries no verdict word
        // (#727 plan, #759 review finding 7); the card's badge still says
        // "No verdict".
        assert.match(p.title, /^Path profile: \d+(\.\d+)? (km|m)$/);
        assert(p.alt.startsWith('Terrain profile from KN6PLV-BrkOxfLA-Yebes to R5-D4 300D Rak, '), 'alt: ' + p.alt);
      });

      await step('the og:image URL is a 1200x630 PNG: miss, then hit with the same bytes', async () => {
        first = await card(p.image, 'miss');
        const again = await card(p.image, 'hit');
        assert(first.equals(again), 'a hit must serve the cached bytes');
        if (theme === 'dark') darkCards.nodes = first;
        else assert(!first.equals(darkCards.nodes), 'the light and the dark card are the same picture');
      });

      await step('ha changes og:image and the card', async () => {
        const q = await preview(base, NODE_A, NODE_B, '&ha=9');
        assert.notEqual(q.image, p.image);
        assert.equal(new URL(q.image).searchParams.get('ha'), '9');
        const other = await card(q.image, 'miss');
        assert(!other.equals(first), 'ha=9 drew the same card');
      });

      await step('a Danish path with no share budget is a degraded card, and ha still shows', async () => {
        const d = await preview(base, DK_A, DK_B);
        assert.match(d.title, /^Path profile: [\d.]+ (m|km)$/, 'no verdict word without a measured profile: ' + d.title);
        assert(d.alt.includes('not ready yet'), 'alt: ' + d.alt);
        const deg = await card(d.image, 'degraded');
        const d9 = await preview(base, DK_A, DK_B, '&ha=9');
        const deg9 = await card(d9.image, 'degraded');
        assert(!deg.equals(deg9), 'ha=9 drew the same degraded card');
        assert(!/raster|dataforsyningen/i.test(server.log()), 'the server tried the elevation service: ' + server.log().slice(-2000));
      });

      await step('an unknown node is 404 on the card and on /p, naming nothing', async () => {
        const unknown = 'f'.repeat(64);
        const c = await get(base + '/api/terrain/profile.png?a=' + unknown + '&b=' + NODE_B);
        assert.equal(c.status, 404);
        assert.equal(c.headers.get('cache-control'), 'no-store');
        const page = await get(shareURL(base, unknown, NODE_B));
        assert.equal(page.status, 404);
        const html = page.body.toString('utf8');
        assert(!html.includes('R5-D4') && !html.includes('profile.png'), 'the refused /p names a node or a card');
      });

      await step('Chromium loads the card at 1440 and 360 px', async () => {
        await browserChecks(browser, base, p.image, theme, shots);
        if (shots && theme === 'dark') await mockUp(browser, p, shots);
      });

      await stop(server.child); server = null;
    }
  } finally {
    if (server) await stop(server.child);
    if (browser) await browser.close();
    fs.rmSync(scratch, { recursive: true, force: true });
  }
  console.log(`\n${passed} passed, ${failed} failed`);
  if (failed) process.exitCode = 1;
})().catch(error => { console.error(error); process.exitCode = 1; });
