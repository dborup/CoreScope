#!/usr/bin/env node
/**
 * E2E (#812 M4): packet share links, from Copy link to the packet page, on
 * a real Go server with share.packets.enabled on.
 *
 * The suite starts its own server (CORESCOPE_SERVER_BIN) on a migrated copy
 * of test-fixtures/e2e-fixture.db (or E2E_FIXTURE_DB, a copy already
 * prepared as in CI) with `share.packets.enabled: true`, node sharing off
 * and no map.dhm, so the packet card is the only card type and the
 * customizer shows its own Share previews section. Nothing leaves the
 * machine.
 *
 * Every flow starts cold (a new browser context) and uses real clicks, or
 * real taps on a 375x560 touch phone. The clipboard is the page's own
 * navigator.clipboard, replaced by a recorder.
 *
 * Asserts:
 *   1. 1440: the packet detail's Copy Link hands out /p?packet=<hash> for
 *      the fixture GRP_TXT; its View Path modal's Copy link hands out
 *      /p?packet=<hash>&viewPath=1; with an observation selected (?obs=)
 *      Copy Link keeps the plain #/packets/<hash>?obs=<id> link; the
 *      standalone #/packet/<id> page's Copy Link hands out /p?packet=<hash>;
 *   2. 1440: the customizer's Display tab shows Share previews with the
 *      packet hint; off by a click → both Copy links hand out the plain
 *      links without a reload; on again → the /p links;
 *   3. 1440 and 375x560: the copied /p?packet= link, opened cold, lands on
 *      the packet page (#/packets/<hash>) with one document request, no
 *      history entry for /p (Back returns to the page before) and no page
 *      error; the View Path link lands with the View Path modal open; at
 *      375x560 nothing overflows sideways;
 *   4. 375x560 (touch), cold: tap Packets → tap a row → tap Copy Link →
 *      /p?packet=<that hash>; tap View Path → tap Copy link → the
 *      &viewPath=1 link;
 *   5. the card the /p page names as og:image loads in Chromium at
 *      1200x630, at 1440 and at 375 px;
 *   6. privacy: the fixture GRP_TXT has decrypted text, a channel and a
 *      sender (checked through the API, so the check is not vacuous), and
 *      the preview page of the copied link holds none of them, while it
 *      names the payload type;
 *   7. on a second, real server with share.packets.enabled off: no share
 *      block in /api/config/client, both Copy links hand out the plain
 *      links, the card is 404, and an existing /p?packet= link still lands
 *      on the packet page.
 *
 * Usage:
 *   (cd cmd/server && go build -o ../../corescope-server .)
 *   (cd cmd/migrate && go build -o ../../corescope-migrate .)
 *   CORESCOPE_SERVER_BIN=./corescope-server CORESCOPE_MIGRATE_BIN=./corescope-migrate \
 *     node test-issue-812-m4-packet-share-e2e.js
 * Optional: E2E_PORT_BASE (default 13900) is the first port tried;
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
const { pinPacketsTimeWindow, withoutPinnedWindow } = require('./tests/e2e-packets-window');

const ROOT = __dirname;
const CRAWLER = 'Mozilla/5.0 (compatible; Discordbot/2.0; +https://discordapp.com)';
// A decrypted GRP_TXT in the fixture: channel #chat, sender KC6ZKT.
const GRP = 'dec2833150e18744';
const GRP_SECRETS = ['Quiet here', 'KC6ZKT', '#chat'];
const SHOTS = process.env.SCREENSHOT_DIR || '';
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

async function startServer(serverBin, dir, shareOn) {
  const db = path.join(dir, 'fixture.db');
  fs.copyFileSync(process.env.E2E_FIXTURE_DB || path.join(ROOT, 'test-fixtures/e2e-fixture.db'), db);
  execFileSync(requiredFile('CORESCOPE_MIGRATE_BIN'), ['-db', db], { stdio: 'pipe' });
  fs.writeFileSync(path.join(dir, 'config.json'), JSON.stringify({
    mqtt: { brokers: [] },
    share: { packets: { enabled: shareOn } },
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
  const scratch = fs.mkdtempSync(path.join(os.tmpdir(), 'e2e-812-m4-'));
  const browser = await chromium.launch({
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  let srv;
  try {
    srv = await startServer(serverBin, scratch, true);
    const BASE = srv.base;
    const SHARE = BASE + '/p?packet=' + GRP;
    const SHARE_VP = SHARE + '&viewPath=1';
    const PLAIN = BASE + '/#/packets/' + GRP;
    const PLAIN_VP = PLAIN + '?viewPath=1';
    console.log('\n#812 M4 — packet share links (' + BASE + ')\n');

    const detail = await (await fetch(BASE + '/api/packets/' + GRP)).json();
    assert(detail && detail.packet && detail.packet.hash === GRP, 'fixture packet ' + GRP + ' missing');
    const GRP_ID = detail.packet.id;

    /* A cold context: clipboard recorder, tips seen, page errors and
     * document requests recorded. */
    async function cold(opts) {
      const phone = !!opts.phone;
      const ctx = await browser.newContext({
        viewport: phone ? { width: 375, height: 560 } : { width: 1440, height: 900 },
        hasTouch: phone, isMobile: phone,
      });
      // The phone flow reads packet rows: pin the window (shard timing).
      await pinPacketsTimeWindow(ctx);
      const rec = { errors: [], documents: [] };
      // Map tiles stay on this machine.
      await ctx.route(/^https?:\/\/(?!localhost|127\.0\.0\.1)/i, route => route.fulfill({ status: 204, body: '' }));
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

    /* A row or a pane can be re-rendered (a WebSocket refresh) between
     * finding it and pressing it; press it again then. */
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

    async function copyWith(page, sel, phone) {
      const n = await copyCount(page);
      await press(page, sel, phone);
      return lastCopied(page, n);
    }

    const COPY = (hash) => '.copy-link-btn[data-packet-hash="' + hash + '"]';

    /* Opens the View Path modal from the visible detail, copies its link
     * and closes it again. */
    async function viewPathCopy(page, hash, phone) {
      await press(page, '[data-view-path="' + hash + '"]', phone);
      await page.waitForSelector('#packetPathModal', { timeout: 10000 });
      const u = await copyWith(page, '#packetPathCopyLink', phone);
      // Close once the path is drawn: Leaflet throws when its map is
      // removed in the middle of the fit animation (not this suite's
      // subject).
      await page.waitForFunction(() => {
        const s = document.getElementById('packetPathStatus');
        return s && s.textContent && !/Loading/.test(s.textContent);
      }, null, { timeout: 10000 });
      await page.waitForTimeout(400);
      await press(page, '#packetPathClose', phone);
      await page.waitForSelector('#packetPathModal', { state: 'detached', timeout: 5000 });
      return u;
    }

    // ── 1 and 2. Desktop: detail, View Path, standalone page, customizer ─
    {
      const { ctx, page, rec } = await cold({});
      await page.goto(BASE + '/#/packets/' + GRP);
      await step('1440: the packet detail’s Copy Link hands out /p?packet=<hash>', async () => {
        assert.equal(await copyWith(page, COPY(GRP), false), SHARE);
        if (SHOTS) await page.screenshot({ path: path.join(SHOTS, '812-m4-packet-detail-1440.png') });
      });
      await step('1440: View Path’s Copy link hands out /p?packet=<hash>&viewPath=1', async () => {
        assert.equal(await viewPathCopy(page, GRP, false), SHARE_VP);
      });

      await step('1440: the customizer shows Share previews for packet links; off → plain links, on → /p', async () => {
        await press(page, '#customizeToggle', false);
        await page.waitForSelector('.cust-overlay:not(.hidden)', { timeout: 5000 });
        await press(page, '.cust-tab[data-tab="display"]', false);
        await page.waitForSelector('.cust-panel[data-panel="display"].active', { timeout: 4000 });
        const text = await page.locator('.cust-panel[data-panel="display"]').innerText();
        assert(/Share previews/.test(text) && /a packet link/.test(text), 'no packet Share previews section');
        assert(!/Terrain Profile/.test(text), 'a Terrain Profile section on a server without DHM');
        assert(await page.isChecked('#cv2-terrain-share'), 'the switch is off by default');
        if (SHOTS) await page.screenshot({ path: path.join(SHOTS, '812-m4-customizer-1440.png') });
        await press(page, 'label[for="cv2-terrain-share"]', false);
        await page.waitForFunction(() => {
          try { return JSON.parse(localStorage.getItem('cs-theme-overrides')).terrain.shareLinks === false; } catch (_) { return false; }
        }, null, { timeout: 4000 });
        await press(page, '#customizeToggle', false);
        await page.waitForSelector('.cust-overlay.hidden', { state: 'attached', timeout: 4000 });
        assert.equal(await copyWith(page, COPY(GRP), false), PLAIN, 'switch off');
        assert.equal(await viewPathCopy(page, GRP, false), PLAIN_VP, 'switch off, View Path');
        await press(page, '#customizeToggle', false);
        await press(page, '.cust-tab[data-tab="display"]', false);
        await press(page, 'label[for="cv2-terrain-share"]', false);
        await page.waitForFunction(() => {
          try { const t = (JSON.parse(localStorage.getItem('cs-theme-overrides') || '{}').terrain) || {}; return t.shareLinks === undefined; } catch (_) { return false; }
        }, null, { timeout: 4000 });
        await press(page, '#customizeToggle', false);
        await page.waitForSelector('.cust-overlay.hidden', { state: 'attached', timeout: 4000 });
        assert.equal(await copyWith(page, COPY(GRP), false), SHARE, 'switch back on');
        assert.equal(await viewPathCopy(page, GRP, false), SHARE_VP, 'switch back on, View Path');
      });

      await step('1440: with an observation selected (?obs=), Copy Link keeps the plain ?obs= link', async () => {
        const obs = (detail.observations || [])[0];
        assert(obs && obs.id != null, 'fixture packet has no observation');
        await page.goto(BASE + '/#/packets/' + GRP + '?obs=' + obs.id);
        await page.waitForSelector(COPY(GRP), { timeout: 15000 });
        assert.equal(await copyWith(page, COPY(GRP), false), PLAIN + '?obs=' + obs.id);
      });

      await page.goto(BASE + '/#/packet/' + GRP_ID);
      await step('1440: the standalone #/packet/<id> page’s Copy Link hands out /p?packet=<hash>', async () => {
        assert.equal(await copyWith(page, COPY(GRP), false), SHARE);
      });
      await step('1440: no page error', async () => assert.deepEqual(rec.errors, []));
      await ctx.close();
    }

    // ── 3. The copied links, opened cold ─────────────────────────────────
    for (const phone of [false, true]) {
      const label = phone ? '375x560' : '1440';
      const { ctx, page, rec } = await cold({ phone });
      await page.goto(BASE + '/#/home');
      await page.waitForSelector('[data-bottom-nav-tab], nav', { timeout: 10000 });
      rec.documents.length = 0;
      await step(label + ': /p?packet= lands on the packet page, no history entry for /p', async () => {
        await page.goto(SHARE + '&fbclid=x1&utm_source=discord');
        await page.waitForSelector(COPY(GRP), { state: 'attached', timeout: 15000 });
        const where = await page.evaluate(() => ({ path: location.pathname, search: location.search, hash: location.hash }));
        where.hash = withoutPinnedWindow(where.hash);
        assert.equal(where.path, '/');
        assert.equal(where.search, '');
        assert(where.hash === '#/packets/' + GRP || where.hash.startsWith('#/packets/' + GRP + '?'), 'hash ' + where.hash);
        assert.equal(rec.documents.length, 1, 'documents: ' + rec.documents.join(', '));
        assert.equal(new URL(rec.documents[0]).pathname, '/p');
        assert.deepEqual(rec.errors, []);
        if (phone) {
          const over = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
          assert(over <= 0, 'horizontal overflow ' + over + 'px');
          if (SHOTS) await page.screenshot({ path: path.join(SHOTS, '812-m4-landing-375x560.png') });
        }
      });
      await step(label + ': Back goes where the user came from, not to /p', async () => {
        await page.goBack({ waitUntil: 'domcontentloaded' });
        await page.waitForTimeout(500);
        const u = new URL(page.url());
        assert(u.pathname === '/' && u.hash === '#/home', 'back went to ' + page.url());
      });
      rec.documents.length = 0;
      await step(label + ': the View Path link lands with the View Path modal open', async () => {
        await page.goto(SHARE_VP);
        await page.waitForSelector('#packetPathModal[data-hash="' + GRP + '"]', { timeout: 15000 });
        const where = await page.evaluate(() => ({ path: location.pathname, search: location.search, hash: location.hash }));
        where.hash = withoutPinnedWindow(where.hash);
        assert.deepEqual(where, { path: '/', search: '', hash: '#/packets/' + GRP + '?viewPath=1' });
        assert.equal(rec.documents.length, 1, 'documents: ' + rec.documents.join(', '));
        assert.deepEqual(rec.errors, []);
        if (SHOTS) await page.screenshot({ path: path.join(SHOTS, '812-m4-viewpath-landing-' + label + '.png') });
      });
      await ctx.close();
    }

    // ── 4. The phone flow, with taps from a cold start ───────────────────
    {
      const { ctx, page, rec } = await cold({ phone: true });
      await page.goto(BASE + '/');
      await step('375x560: tap Packets → tap a row → tap Copy Link → /p?packet=<that hash>', async () => {
        await press(page, '[data-bottom-nav-tab="packets"]', true);
        await page.waitForFunction(() => location.hash.startsWith('#/packets'), null, { timeout: 10000 });
        const row = '#pktTable tbody tr[data-action="select-hash"][data-hash]';
        await page.waitForSelector(row, { timeout: 15000 });
        // The first row with a real hash (seeded test rows carry others).
        const hash = await page.evaluate(sel => {
          const r = Array.from(document.querySelectorAll(sel)).find(tr => /^[0-9a-f]{16}$/.test(tr.dataset.hash));
          return r ? r.dataset.hash : null;
        }, row);
        assert(hash, 'no row with a 16-hex hash');
        await press(page, '#pktTable tbody tr[data-action="select-hash"][data-hash="' + hash + '"]', true);
        const copied = await copyWith(page, COPY(hash), true);
        assert.equal(copied, BASE + '/p?packet=' + hash);
        const box = await page.locator(COPY(hash)).first().boundingBox();
        assert(box && box.x >= 0 && box.x + box.width <= 375, 'Copy Link off screen ' + JSON.stringify(box));
        if (SHOTS) await page.screenshot({ path: path.join(SHOTS, '812-m4-phone-detail-375x560.png') });
        await press(page, '[data-view-path="' + hash + '"]', true);
        await page.waitForSelector('#packetPathModal', { timeout: 10000 });
        const vbox = await page.locator('#packetPathCopyLink').boundingBox();
        assert(vbox && vbox.x >= 0 && vbox.x + vbox.width <= 375, 'View Path Copy link off screen ' + JSON.stringify(vbox));
        assert.equal(await copyWith(page, '#packetPathCopyLink', true), BASE + '/p?packet=' + hash + '&viewPath=1');
        if (SHOTS) await page.screenshot({ path: path.join(SHOTS, '812-m4-phone-viewpath-375x560.png') });
        assert.deepEqual(rec.errors, []);
      });
      await ctx.close();
    }

    // ── 5 and 6. The card, and what the preview page never holds ─────────
    let image = '';
    await step('privacy: the fixture GRP_TXT holds text, channel and sender (the check is not vacuous)', async () => {
      const decoded = JSON.parse(detail.packet.decoded_json || '{}');
      assert.equal(detail.packet.payload_type, 5, 'not a GRP_TXT');
      GRP_SECRETS.forEach((s) => assert(JSON.stringify(decoded).includes(s), 'fixture lacks ' + s));
    });
    await step('privacy: the copied link’s preview page has no message text, channel or sender', async () => {
      for (const link of [SHARE, SHARE_VP]) {
        const r = await fetch(link, { headers: { 'User-Agent': CRAWLER } });
        assert.equal(r.status, 200);
        const html = await r.text();
        GRP_SECRETS.forEach((s) => assert(!html.includes(s), link + ' holds ' + JSON.stringify(s)));
        const tags = ['og:title', 'og:description', 'og:image:alt'].map((k) => meta(html, k)).join(' | ');
        assert(!/chat/i.test(tags), 'a channel name in the tags: ' + tags);
        assert(meta(html, 'og:title').startsWith('GRP_TXT · '), 'payload type not named: ' + meta(html, 'og:title'));
        image = meta(html, 'og:image');
      }
      assert(image.startsWith(BASE + '/api/packets/' + GRP + '/card.png?v='), image);
    });
    for (const width of [1440, 375]) {
      await step('Chromium loads the card at 1200x630 (' + width + ' px wide)', async () => {
        const ctx = await browser.newContext({ viewport: { width, height: 800 } });
        await pinPacketsTimeWindow(ctx);
        const page = await ctx.newPage();
        const resp = await page.goto(image);
        assert.equal(resp.status(), 200);
        assert.equal(resp.headers()['content-type'], 'image/png');
        const natural = await page.evaluate(() => { const i = document.querySelector('img'); return i ? [i.naturalWidth, i.naturalHeight] : null; });
        assert.deepEqual(natural, [1200, 630]);
        if (SHOTS && width === 1440) await page.screenshot({ path: path.join(SHOTS, '812-m4-card.png') });
        await ctx.close();
      });
    }

    // ── 7. Sharing off on a real server ──────────────────────────────────
    await stop(srv.child);
    const offDir = path.join(scratch, 'off');
    fs.mkdirSync(offDir);
    srv = await startServer(serverBin, offDir, false);
    {
      const OFF = srv.base;
      const { ctx, page, rec } = await cold({});
      await page.goto(OFF + '/#/packets/' + GRP);
      await step('share.packets.enabled off: no share block, both Copy links plain, the card is 404', async () => {
        const cfg = await (await fetch(OFF + '/api/config/client')).json();
        assert.equal(cfg.share, undefined, 'share block on a share-off server');
        assert.equal(await copyWith(page, COPY(GRP), false), OFF + '/#/packets/' + GRP);
        assert.equal(await viewPathCopy(page, GRP, false), OFF + '/#/packets/' + GRP + '?viewPath=1');
        assert.equal((await fetch(OFF + '/api/packets/' + GRP + '/card.png')).status, 404);
      });
      await step('share.packets.enabled off: an existing /p?packet= link still lands on the packet page', async () => {
        await page.goto(OFF + '/p?packet=' + GRP);
        await page.waitForSelector(COPY(GRP), { state: 'attached', timeout: 15000 });
        assert.equal(new URL(page.url()).pathname, '/');
        assert.deepEqual(rec.errors, []);
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
