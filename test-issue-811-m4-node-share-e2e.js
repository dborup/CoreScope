#!/usr/bin/env node
/**
 * E2E (#811 M4): node share links, from Copy URL to the node page, on a
 * real Go server with share.nodes.enabled on.
 *
 * The suite starts its own server (CORESCOPE_SERVER_BIN) on a migrated copy
 * of test-fixtures/e2e-fixture.db (or E2E_FIXTURE_DB, a copy already
 * prepared as in CI) with `share.nodes.enabled: true` and no map.dhm, so
 * the node card is the only card type and the customizer shows its own
 * Share previews section. Nothing leaves the machine.
 *
 * Every flow starts cold (a new browser context) and uses real clicks, or
 * real taps on a 375x560 touch phone. The clipboard is the page's own
 * navigator.clipboard, replaced by a recorder.
 *
 * Asserts:
 *   1. 1440: Nodes → search → a repeater's row opens the side pane, whose
 *      Copy URL hands out /p?node=<pk>; Details → the node page's Copy URL
 *      hands out the same link; Copy short URL stays a #/nodes/ prefix link;
 *      a companion (no card) gets the plain #/nodes/<pk> link in both places;
 *   2. 1440: the customizer's Display tab shows Share previews with the
 *      switch; turned off by a click, Copy URL hands out the plain link
 *      without a reload; on again, the /p link;
 *   3. 1440 and 375x560: the copied /p?node= link, opened cold, lands on the
 *      node page with #/nodes/<pk>, one document request, no history entry
 *      for /p (Back returns to the page before), and no page error; at
 *      375x560 nothing overflows sideways;
 *   4. 375x560 (touch): More → Nodes → search → tap the row → the node page
 *      → tap Copy URL hands out /p?node=<pk>;
 *   5. the card the /p page names as og:image loads in Chromium at 1200x630,
 *      at 1440 and at 375 px;
 *   6. on a second, real server with share.nodes.enabled off: no share
 *      block in /api/config/client, Copy URL hands out the plain link and
 *      the card is 404;
 *   7. 1440, with the profile's dhm block (profile, share and dhm.card)
 *      added to /api/config/client as a server with map.dhm sends it: the
 *      switch lives in the Terrain Profile section, once, with the hint for
 *      both card types and the card look from dhm.card, and no separate
 *      Share previews section; off by a click, node Copy URL is plain.
 *
 * Usage:
 *   (cd cmd/server && go build -o ../../corescope-server .)
 *   (cd cmd/migrate && go build -o ../../corescope-migrate .)
 *   CORESCOPE_SERVER_BIN=./corescope-server CORESCOPE_MIGRATE_BIN=./corescope-migrate \
 *     node test-issue-811-m4-node-share-e2e.js
 * Optional: E2E_PORT_BASE (default 13950) is the first port tried;
 * E2E_FIXTURE_DB is the fixture to copy; SCREENSHOT_DIR writes screenshots.
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
const REPEATER = '988396af0ff369c2178a8c2a303661bf26193257136ed4452b6eadd9547011e9'; // KN6PLV-BrkOxfLA-Yebes
const REPEATER_NAME = 'KN6PLV-BrkOxfLA-Yebes';
const COMPANION = '2e0de40ad148de6f86cb11c49eb19a138693b4568a184ab368b2db4e6769606a'; // Crayons
const COMPANION_NAME = 'Crayons';
const SHOTS = process.env.SCREENSHOT_DIR || '';
const PNG_1X1 = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGM4ceIEAAS0AlkWLoFAAAAAAElFTkSuQmCC',
  'base64');
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
  const base = Number(process.env.E2E_PORT_BASE || 13950);
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

async function startServer(serverBin, dir, shareOn) {
  const db = path.join(dir, 'fixture.db');
  fs.copyFileSync(process.env.E2E_FIXTURE_DB || path.join(ROOT, 'test-fixtures/e2e-fixture.db'), db);
  execFileSync(requiredFile('CORESCOPE_MIGRATE_BIN'), ['-db', db], { stdio: 'pipe' });
  fs.writeFileSync(path.join(dir, 'config.json'), JSON.stringify({
    mqtt: { brokers: [] },
    share: { nodes: { enabled: shareOn } },
  }));
  const port = await freePort();
  const base = 'http://127.0.0.1:' + port;
  let log = '';
  const child = spawn(serverBin, ['-config-dir', dir, '-db', db, '-public', path.join(ROOT, 'public'), '-port', String(port)],
    { cwd: dir, stdio: ['ignore', 'pipe', 'pipe'] });
  child.stdout.on('data', c => { log = (log + c).slice(-60000); });
  child.stderr.on('data', c => { log = (log + c).slice(-60000); });
  for (let n = 0; n < 300; n++) {
    assert(child.exitCode === null && child.signalCode === null, 'server exited: ' + log);
    try {
      const r = await fetch(base + '/api/config/client', { signal: AbortSignal.timeout(2000) });
      if (r.ok) return { child, base, log: () => log };
    } catch (_) { /* not up yet */ }
    await delay(100);
  }
  throw new Error('server startup timed out: ' + log);
}

function meta(html, key) {
  const re = new RegExp('<meta (?:property|name)="' + key.replace(/[.:]/g, '\\$&') + '" content="([^"<>]*)">', 'g');
  const all = [...html.matchAll(re)];
  assert.equal(all.length, 1, 'exactly one ' + key + ' tag');
  return all[0][1].replace(/&amp;/g, '&');
}

async function main() {
  const serverBin = requiredFile('CORESCOPE_SERVER_BIN');
  const scratch = fs.mkdtempSync(path.join(os.tmpdir(), 'e2e-811-m4-'));
  const browser = await chromium.launch({
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  let srv;
  try {
    srv = await startServer(serverBin, scratch, true);
    const BASE = srv.base;
    const SHARE = BASE + '/p?node=' + REPEATER;
    console.log('\n#811 M4 — node share links (' + BASE + ')\n');

    /* A cold context: clipboard recorder, tips seen, page errors and
     * document requests recorded. opts.dhm adds a dhm block to the client
     * config, as a server with map.dhm and the profile sends it; nothing
     * then leaves the machine (other hosts and the WMS proxy get a PNG). */
    async function cold(opts) {
      const phone = !!opts.phone;
      const ctx = await browser.newContext({
        viewport: phone ? { width: 375, height: 560 } : { width: 1440, height: 900 },
        hasTouch: phone, isMobile: phone,
      });
      const rec = { errors: [], documents: [] };
      if (opts.dhm) {
        await ctx.route(url => new URL(url).pathname === '/api/config/client', async (route) => {
          const r = await route.fetch();
          const body = await r.json();
          body.dhm = opts.dhm;
          await route.fulfill({ response: r, json: body });
        });
        await ctx.route(/^https?:\/\/(?!localhost|127\.0\.0\.1)/i, route => route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 }));
        await ctx.route(url => new URL(url).pathname === '/api/map/dhm/wms', route => route.fulfill({ status: 200, contentType: 'image/png', body: PNG_1X1 }));
      }
      await ctx.addInitScript(() => {
        try {
          ['row-swipe', 'tab-swipe', 'edge-drawer', 'pull-refresh']
            .forEach(k => localStorage.setItem('meshcore-gesture-hints-' + k, 'seen'));
        } catch (_) {}
        window.__copied = [];
        try {
          Object.defineProperty(navigator, 'clipboard', { configurable: true, value: {
            writeText: (t) => { window.__copied.push(String(t)); return Promise.resolve(); },
          } });
        } catch (_) {}
      });
      const page = await ctx.newPage();
      page.on('pageerror', e => rec.errors.push(String(e)));
      page.on('request', r => { if (r.resourceType() === 'document') rec.documents.push(r.url()); });
      return { ctx, page, rec };
    }

    async function lastCopied(page, before) {
      for (let i = 0; i < 40; i++) {
        const all = await page.evaluate(() => window.__copied.slice());
        if (all.length > before) return all[all.length - 1];
        await delay(50);
      }
      throw new Error('nothing was copied');
    }
    async function copyCount(page) { return page.evaluate(() => window.__copied.length); }

    /* A list row can be re-rendered (a debounced search, a WebSocket
     * refresh) between finding it and pressing it; press it again then. */
    async function press(page, sel, phone) {
      for (let attempt = 1; ; attempt++) {
        const loc = page.locator(sel).first();
        try {
          await loc.waitFor({ state: 'visible', timeout: 10000 });
          await loc.scrollIntoViewIfNeeded();
          if (phone) await loc.tap(); else await loc.click();
          return;
        } catch (e) {
          if (attempt >= 3 || !/not attached|detached/i.test(e.message)) throw e;
        }
      }
    }

    /* Nodes list → search → the row, by real clicks or taps. */
    async function openFromList(page, name, pk, phone) {
      if (phone) {
        await press(page, '[data-bottom-nav-tab="more"]', true);
        await press(page, '.bottom-nav-sheet a[href="#/nodes"], [role="dialog"] a[href="#/nodes"]', true);
      } else {
        await press(page, 'nav a[href="#/nodes"], .nav-links a[href="#/nodes"]', false);
      }
      await press(page, '#nodeSearch', phone);
      await page.locator('#nodeSearch').fill(name);
      // Wait out the debounced search: only matching rows are left.
      await page.waitForFunction((n) => {
        const rows = document.querySelectorAll('#nodesBody tr[data-value]');
        return rows.length > 0 && Array.from(rows).every(r => r.textContent.indexOf(n) >= 0);
      }, name, { timeout: 10000 });
      await press(page, 'tr[data-value="' + pk + '"]', phone);
    }

    async function copyWith(page, sel, phone) {
      const n = await copyCount(page);
      await press(page, sel, phone);
      return lastCopied(page, n);
    }

    // ── 1 and 2. Desktop: side pane, node page, companion, customizer ────
    {
      const { ctx, page, rec } = await cold({});
      await page.goto(BASE + '/');
      await step('1440: the side pane’s Copy URL hands out /p?node=<pk> for a repeater', async () => {
        await openFromList(page, REPEATER_NAME, REPEATER, false);
        assert.equal(await copyWith(page, '#panelCopyUrlBtn', false), SHARE);
      });
      await step('1440: Details → the node page’s Copy URL hands out the same link', async () => {
        await press(page, '.node-detail-btn', false);
        await page.waitForFunction(pk => location.hash === '#/nodes/' + pk && document.getElementById('copyUrlBtn'), REPEATER, { timeout: 10000 });
        assert.equal(await copyWith(page, '#copyUrlBtn', false), SHARE);
        if (SHOTS) await page.screenshot({ path: path.join(SHOTS, '811-m4-node-page-1440.png') });
      });
      await step('1440: Copy short URL stays the #/nodes/<prefix> link', async () => {
        assert.equal(await copyWith(page, '#copyShortUrlBtn', false), BASE + '/#/nodes/' + REPEATER.slice(0, 8));
      });
      await page.goto(BASE + '/');
      await step('1440: a companion gets the plain link, in the side pane and on its page', async () => {
        await openFromList(page, COMPANION_NAME, COMPANION, false);
        const plain = BASE + '/#/nodes/' + COMPANION;
        assert.equal(await copyWith(page, '#panelCopyUrlBtn', false), plain);
        await press(page, '.node-detail-btn', false);
        await page.waitForFunction(pk => location.hash === '#/nodes/' + pk && document.getElementById('copyUrlBtn'), COMPANION, { timeout: 10000 });
        assert.equal(await copyWith(page, '#copyUrlBtn', false), plain);
      });

      await page.goto(BASE + '/#/nodes/' + REPEATER);
      await page.waitForSelector('#copyUrlBtn', { timeout: 10000 });
      await step('1440: the customizer shows Share previews; off by a click → the plain link, on → /p', async () => {
        await press(page, '#customizeToggle', false);
        await page.waitForSelector('.cust-overlay:not(.hidden)', { timeout: 5000 });
        await press(page, '.cust-tab[data-tab="display"]', false);
        await page.waitForSelector('.cust-panel[data-panel="display"].active', { timeout: 4000 });
        const text = await page.locator('.cust-panel[data-panel="display"]').innerText();
        assert(/Share previews/.test(text) && /node link/.test(text), 'no node Share previews section');
        assert(!/Terrain Profile/.test(text), 'a Terrain Profile section on a server without DHM');
        assert(await page.isChecked('#cv2-terrain-share'), 'the switch is off by default');
        if (SHOTS) await page.screenshot({ path: path.join(SHOTS, '811-m4-customizer-1440.png') });
        await press(page, 'label[for="cv2-terrain-share"]', false);
        await page.waitForFunction(() => {
          try { return JSON.parse(localStorage.getItem('cs-theme-overrides')).terrain.shareLinks === false; } catch (_) { return false; }
        }, null, { timeout: 4000 });
        await press(page, '#customizeToggle', false);
        await page.waitForSelector('.cust-overlay.hidden', { state: 'attached', timeout: 4000 });
        assert.equal(await copyWith(page, '#copyUrlBtn', false), BASE + '/#/nodes/' + REPEATER, 'switch off');
        await press(page, '#customizeToggle', false);
        await press(page, '.cust-tab[data-tab="display"]', false);
        await press(page, 'label[for="cv2-terrain-share"]', false);
        await page.waitForFunction(() => {
          try { const t = (JSON.parse(localStorage.getItem('cs-theme-overrides') || '{}').terrain) || {}; return t.shareLinks === undefined; } catch (_) { return false; }
        }, null, { timeout: 4000 });
        await press(page, '#customizeToggle', false);
        await page.waitForSelector('.cust-overlay.hidden', { state: 'attached', timeout: 4000 });
        assert.equal(await copyWith(page, '#copyUrlBtn', false), SHARE, 'switch back on');
      });
      await step('1440: no page error', async () => assert.deepEqual(rec.errors, []));
      await ctx.close();
    }

    // ── 3. The copied link, opened cold ──────────────────────────────────
    for (const phone of [false, true]) {
      const label = phone ? '375x560' : '1440';
      const { ctx, page, rec } = await cold({ phone });
      await page.goto(BASE + '/#/home');
      await page.waitForSelector('[data-bottom-nav-tab], nav', { timeout: 10000 });
      rec.documents.length = 0;
      await step(label + ': /p?node= lands on the node page, no history entry for /p', async () => {
        await page.goto(SHARE + '&fbclid=x1&utm_source=discord');
        await page.waitForFunction(name => {
          const el = document.querySelector('.node-detail-name');
          return el && el.textContent.indexOf(name) >= 0;
        }, REPEATER_NAME, { timeout: 15000 });
        const where = await page.evaluate(() => ({ path: location.pathname, search: location.search, hash: location.hash }));
        assert.deepEqual(where, { path: '/', search: '', hash: '#/nodes/' + REPEATER });
        assert.equal(rec.documents.length, 1, 'documents: ' + rec.documents.join(', '));
        assert.equal(new URL(rec.documents[0]).pathname, '/p');
        assert.deepEqual(rec.errors, []);
        if (phone) {
          const over = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
          assert(over <= 0, 'horizontal overflow ' + over + 'px');
          if (SHOTS) await page.screenshot({ path: path.join(SHOTS, '811-m4-landing-375x560.png') });
        }
      });
      await step(label + ': Back goes where the user came from, not to /p', async () => {
        await page.goBack({ waitUntil: 'domcontentloaded' });
        await page.waitForTimeout(500);
        const u = new URL(page.url());
        assert(u.pathname === '/' && u.hash === '#/home', 'back went to ' + page.url());
      });
      await ctx.close();
    }

    // ── 4. The phone flow, with taps ─────────────────────────────────────
    {
      const { ctx, page, rec } = await cold({ phone: true });
      await page.goto(BASE + '/');
      await step('375x560: More → Nodes → search → tap the row → tap Copy URL → /p?node=<pk>', async () => {
        await openFromList(page, REPEATER_NAME, REPEATER, true);
        await page.waitForFunction(pk => location.hash === '#/nodes/' + pk, REPEATER, { timeout: 10000 });
        const copied = await copyWith(page, '#copyUrlBtn', true);
        assert.equal(copied, SHARE);
        const box = await page.locator('#copyUrlBtn').boundingBox();
        assert(box && box.x >= 0 && box.x + box.width <= 375, 'Copy URL off screen ' + JSON.stringify(box));
        assert.deepEqual(rec.errors, []);
      });
      await ctx.close();
    }

    // ── 5. The card ──────────────────────────────────────────────────────
    let image = '';
    await step('the /p page names a card URL as og:image', async () => {
      const r = await fetch(SHARE, { headers: { 'User-Agent': CRAWLER } });
      assert.equal(r.status, 200);
      const html = await r.text();
      image = meta(html, 'og:image');
      assert(image.startsWith(BASE + '/api/nodes/' + REPEATER + '/card.png?v='), image);
    });
    for (const width of [1440, 375]) {
      await step('Chromium loads the card at 1200x630 (' + width + ' px wide)', async () => {
        const ctx = await browser.newContext({ viewport: { width, height: 800 } });
        const page = await ctx.newPage();
        const resp = await page.goto(image);
        assert.equal(resp.status(), 200);
        assert.equal(resp.headers()['content-type'], 'image/png');
        const natural = await page.evaluate(() => { const i = document.querySelector('img'); return i ? [i.naturalWidth, i.naturalHeight] : null; });
        assert.deepEqual(natural, [1200, 630]);
        if (SHOTS && width === 1440) await page.screenshot({ path: path.join(SHOTS, '811-m4-card.png') });
        await ctx.close();
      });
    }

    // ── 7. The profile and node sharing on the same server ───────────────
    {
      const dhm = {
        enabled: true, url: '/api/map/dhm/wms', layer: 'dhm_terraen_skyggekort',
        minZoom: 7, maxZoom: 19, profile: true, share: true,
        card: { theme: 'light', names: false },
        bounds: [[54.4265, 7.99125], [57.778, 15.5995]],
        attribution: 'Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel, CC BY 4.0',
      };
      const { ctx, page, rec } = await cold({ dhm });
      await page.goto(BASE + '/#/nodes/' + REPEATER);
      await page.waitForSelector('#copyUrlBtn', { timeout: 10000 });
      await step('1440, profile and node cards: one switch in Terrain Profile, both card types, look from dhm.card', async () => {
        assert.equal(await copyWith(page, '#copyUrlBtn', false), SHARE, 'before the switch');
        await press(page, '#customizeToggle', false);
        await page.waitForSelector('.cust-overlay:not(.hidden)', { timeout: 5000 });
        await press(page, '.cust-tab[data-tab="display"]', false);
        await page.waitForSelector('.cust-panel[data-panel="display"].active', { timeout: 4000 });
        const panel = page.locator('.cust-panel[data-panel="display"]');
        const text = await panel.innerText();
        assert(/Terrain Profile/.test(text), 'no Terrain Profile section');
        assert.equal(await page.locator('#cv2-terrain-share').count(), 1, 'switch count');
        assert.equal((text.match(/Share previews/g) || []).length, 1, 'Share previews appears once (inside Terrain Profile)');
        assert(text.indexOf('Share previews') > text.indexOf('Terrain Profile'), 'Share previews is not inside Terrain Profile');
        assert(/a profile link or a node link/.test(text), 'the hint does not name both card types');
        assert.equal(await page.locator('#cv2-card-theme').innerText(), 'Light');
        assert.equal(await page.locator('#cv2-card-names').innerText(), 'Hidden');
        assert(await page.isChecked('#cv2-terrain-share'), 'the switch is off by default');
        if (SHOTS) await page.screenshot({ path: path.join(SHOTS, '811-m4-customizer-combined-1440.png') });
        await press(page, 'label[for="cv2-terrain-share"]', false);
        await page.waitForFunction(() => {
          try { return JSON.parse(localStorage.getItem('cs-theme-overrides')).terrain.shareLinks === false; } catch (_) { return false; }
        }, null, { timeout: 4000 });
        await press(page, '#customizeToggle', false);
        await page.waitForSelector('.cust-overlay.hidden', { state: 'attached', timeout: 4000 });
        assert.equal(await copyWith(page, '#copyUrlBtn', false), BASE + '/#/nodes/' + REPEATER, 'switch off');
      });
      await step('1440, profile and node cards: no page error', async () => assert.deepEqual(rec.errors, []));
      await ctx.close();
    }

    // ── 6. Sharing off on a real server ──────────────────────────────────
    await stop(srv.child);
    const offDir = path.join(scratch, 'off');
    fs.mkdirSync(offDir);
    srv = await startServer(serverBin, offDir, false);
    {
      const OFF = srv.base;
      const { ctx, page } = await cold({});
      await page.goto(OFF + '/#/nodes/' + REPEATER);
      await page.waitForSelector('#copyUrlBtn', { timeout: 10000 });
      await step('share.nodes.enabled off: no share block, Copy URL is the plain link, the card is 404', async () => {
        const cfg = await (await fetch(OFF + '/api/config/client')).json();
        assert.equal(cfg.share, undefined, 'share block on a share-off server');
        assert.equal(await copyWith(page, '#copyUrlBtn', false), OFF + '/#/nodes/' + REPEATER);
        assert.equal((await fetch(OFF + '/api/nodes/' + REPEATER + '/card.png')).status, 404);
      });
      await ctx.close();
    }
  } finally {
    await browser.close();
    if (srv) await stop(srv.child);
    fs.rmSync(scratch, { recursive: true, force: true });
  }
  console.log('\n' + passed + ' passed, ' + failed + ' failed\n');
  if (failed > 0) process.exit(1);
}

main().catch((e) => { console.error(e); process.exit(1); });
