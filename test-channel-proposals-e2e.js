#!/usr/bin/env node
/* E2E: shared channel proposals, end to end with the real binaries.
 *
 * Starts its own ingestor and server (both built from this repo) against a
 * temp copy of the E2E fixture database, plus a minimal in-process MQTT
 * broker so the ingestor can start (it only needs CONNECT/SUBSCRIBE/PING —
 * no packets are published). Then drives independent browser sessions:
 *
 *   A      suggests a channel from the Channels page (desktop)
 *   admin  opens #/channels?view=proposals, unlocks with the apiKey, approves
 *   B      a fresh session that never interacted: the approved channel is
 *          listed as a shared channel (no traffic, no remove control)
 *   mobile the shared channel and the suggest/admin UI at 390x844
 *
 * and finally restarts both processes to check the approval persists.
 *
 * Env:
 *   CORESCOPE_SERVER_BIN   (default ./corescope-server)
 *   CORESCOPE_INGESTOR_BIN (default ./corescope-ingestor)
 *   FIXTURE_DB             (default test-fixtures/e2e-fixture.db)
 *   PUBLIC_DIR             (default public)
 *   CHROMIUM_PATH, CHROMIUM_REQUIRE=1 (fail instead of skip without Chromium)
 */
'use strict';

const assert = require('assert');
const { spawn } = require('child_process');
const fs = require('fs');
const net = require('net');
const os = require('os');
const path = require('path');
const { chromium } = require('playwright');

const ROOT = __dirname;
const SERVER_BIN = path.resolve(process.env.CORESCOPE_SERVER_BIN || path.join(ROOT, 'corescope-server'));
const INGESTOR_BIN = path.resolve(process.env.CORESCOPE_INGESTOR_BIN || path.join(ROOT, 'corescope-ingestor'));
const FIXTURE_DB = path.resolve(process.env.FIXTURE_DB || path.join(ROOT, 'test-fixtures', 'e2e-fixture.db'));
const PUBLIC_DIR = path.resolve(process.env.PUBLIC_DIR || path.join(ROOT, 'public'));
const API_KEY = 'e2e-proposals-admin-key-0123456789';
const NAME = '#E2eShared' + Date.now().toString(36).slice(-6); // unique, case preserved, < 31 bytes
const TIMEOUT = 20000;

let passed = 0;
let failed = 0;
async function step(name, fn) {
  try {
    await fn();
    passed++;
    console.log('  ✓ ' + name);
  } catch (e) {
    failed++;
    console.error('  ✗ ' + name + '\n    ' + ((e && e.stack) || e));
  }
}

// ── minimal MQTT 3.1.1 broker ───────────────────────────────────────────
function startFakeBroker() {
  return new Promise((resolve) => {
    const sockets = new Set();
    const server = net.createServer((sock) => {
      sockets.add(sock);
      sock.on('close', () => sockets.delete(sock));
      sock.on('error', () => {});
      let buf = Buffer.alloc(0);
      sock.on('data', (d) => {
        buf = Buffer.concat([buf, d]);
        for (;;) {
          if (buf.length < 2) return;
          let len = 0;
          let mult = 1;
          let i = 1;
          let complete = false;
          for (; i < buf.length && i <= 4; i++) {
            len += (buf[i] & 0x7f) * mult;
            mult *= 128;
            if ((buf[i] & 0x80) === 0) { complete = true; i++; break; }
          }
          if (!complete || buf.length < i + len) return;
          const type = buf[0] >> 4;
          const body = buf.slice(i, i + len);
          buf = buf.slice(i + len);
          if (type === 1) sock.write(Buffer.from([0x20, 0x02, 0x00, 0x00])); // CONNACK accepted
          else if (type === 8) { // SUBSCRIBE → SUBACK granting QoS 0 per topic
            let n = 0;
            for (let p = 2; p + 2 <= body.length;) {
              p += 2 + body.readUInt16BE(p) + 1;
              n++;
            }
            sock.write(Buffer.from([0x90, 2 + n, body[0], body[1]].concat(new Array(n).fill(0))));
          } else if (type === 12) sock.write(Buffer.from([0xd0, 0x00])); // PINGRESP
          else if (type === 14) sock.end();
        }
      });
    });
    server.listen(0, '127.0.0.1', () => resolve({
      port: server.address().port,
      close: () => { sockets.forEach((s) => s.destroy()); server.close(); },
    }));
  });
}

function freePort() {
  return new Promise((resolve, reject) => {
    const s = net.createServer();
    s.listen(0, '127.0.0.1', () => { const p = s.address().port; s.close(() => resolve(p)); });
    s.on('error', reject);
  });
}

// ── processes ───────────────────────────────────────────────────────────
// Every spawned process is registered so cleanup stops it even when a start
// fails half-way; each start gets its own log file so a wait can never match
// a line written by an earlier process.
const children = new Set();
let startCount = 0;
function startProcess(label, bin, args, dir) {
  const logFile = path.join(dir, label + '-' + (++startCount) + '.log');
  const out = fs.openSync(logFile, 'w');
  const child = spawn(bin, args, { stdio: ['ignore', out, out] });
  fs.closeSync(out);
  child.label = label;
  child.logFile = logFile;
  children.add(child);
  child.once('exit', () => children.delete(child));
  return child;
}

async function stopProcess(child) {
  if (!child || child.exitCode !== null) return;
  const exited = new Promise((r) => child.once('exit', r));
  child.kill('SIGTERM');
  const t = setTimeout(() => child.kill('SIGKILL'), 5000);
  await exited;
  clearTimeout(t);
}

async function waitFor(what, fn, timeoutMs) {
  const deadline = Date.now() + (timeoutMs || TIMEOUT);
  let last;
  while (Date.now() < deadline) {
    try {
      const v = await fn();
      if (v) return v;
    } catch (e) { last = e; }
    await new Promise((r) => setTimeout(r, 200));
  }
  throw new Error('timed out waiting for ' + what + (last ? ': ' + last.message : ''));
}

function logHas(child, re) {
  try { return re.test(fs.readFileSync(child.logFile, 'utf8')); } catch (e) { return false; }
}

async function startStack(env) {
  const ingestor = startProcess('ingestor', INGESTOR_BIN, ['-config', env.ingestorConfig], env.dir);
  await waitFor('ingestor MQTT subscription', () => {
    if (ingestor.exitCode !== null) throw new Error('ingestor exited: see ' + ingestor.logFile);
    return logHas(ingestor, /MQTT \[e2e\] subscribed/);
  });
  const server = startProcess('server', SERVER_BIN,
    ['-port', String(env.port), '-db', env.db, '-public', PUBLIC_DIR, '-config-dir', env.serverDir],
    env.dir);
  await waitFor('server health', async () => {
    if (server.exitCode !== null) throw new Error('server exited: see ' + server.logFile);
    const res = await fetch(env.base + '/api/healthz');
    return res.ok;
  });
  return { ingestor, server };
}

async function main() {
  for (const bin of [SERVER_BIN, INGESTOR_BIN, FIXTURE_DB]) {
    if (!fs.existsSync(bin)) {
      console.error('test-channel-proposals-e2e.js: FAIL — missing ' + bin);
      process.exit(1);
    }
  }
  let browser;
  try {
    browser = await chromium.launch({
      headless: true,
      executablePath: process.env.CHROMIUM_PATH || undefined,
      args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
    });
  } catch (err) {
    if (process.env.CHROMIUM_REQUIRE === '1') {
      console.error('test-channel-proposals-e2e.js: FAIL — Chromium required but unavailable: ' + err.message);
      process.exit(1);
    }
    console.log('test-channel-proposals-e2e.js: SKIP (Chromium unavailable: ' + err.message.split('\n')[0] + ')');
    process.exit(0);
  }

  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'corescope-proposals-e2e-'));
  const broker = await startFakeBroker();
  const env = { dir, db: path.join(dir, 'meshcore.db'), port: await freePort(), serverDir: path.join(dir, 'server') };
  env.base = 'http://127.0.0.1:' + env.port;
  env.ingestorConfig = path.join(dir, 'ingestor.json');
  fs.copyFileSync(FIXTURE_DB, env.db);
  fs.mkdirSync(env.serverDir);
  const proposals = { enabled: true };
  fs.writeFileSync(env.ingestorConfig, JSON.stringify({
    dbPath: env.db,
    mqttSources: [{ name: 'e2e', broker: 'mqtt://127.0.0.1:' + broker.port, topics: ['meshcore/#'], connectTimeoutSec: 5 }],
    channelProposals: proposals,
  }));
  fs.writeFileSync(path.join(env.serverDir, 'config.json'), JSON.stringify({ apiKey: API_KEY, channelProposals: proposals }));

  let stack;
  const pageErrors = [];
  const watch = (page, label) => page.on('pageerror', (e) => pageErrors.push(label + ': ' + e.message));
  try {
    stack = await startStack(env);
    console.log('test-channel-proposals-e2e.js (' + NAME + ', ' + env.base + ')');

    const ctxA = await browser.newContext({ viewport: { width: 1280, height: 850 } });
    const ctxAdmin = await browser.newContext({ viewport: { width: 1280, height: 850 } });
    const ctxB = await browser.newContext({ viewport: { width: 1280, height: 850 } });
    const ctxMobile = await browser.newContext({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });
    const pageA = await ctxA.newPage();
    const pageAdmin = await ctxAdmin.newPage();
    const pageB = await ctxB.newPage();
    const pageMobile = await ctxMobile.newPage();
    for (const [p, l] of [[pageA, 'A'], [pageAdmin, 'admin'], [pageB, 'B'], [pageMobile, 'mobile']]) {
      p.setDefaultTimeout(TIMEOUT);
      watch(p, l);
    }

    await step('config endpoint reports suggestions enabled', async () => {
      const res = await fetch(env.base + '/api/channel-proposals/config');
      assert.deepStrictEqual(await res.json(), { enabled: true });
    });

    await step('A: the Add modal keeps the local channel tools and shows the suggest form', async () => {
      await pageA.goto(env.base + '/#/channels');
      await pageA.click('#chAddChannelBtn');
      await pageA.waitForSelector('#chAddChannelModal:not(.hidden)');
      for (const id of ['#chGenerateBtn', '#chPskAddBtn', '#chHashtagBtn']) {
        assert.ok(await pageA.isVisible(id), id + ' (local Add channel) must stay');
      }
      await pageA.waitForSelector('#chSuggestSection:not([hidden]) #chSuggestName');
    });

    await step('A: invalid names are refused before any request', async () => {
      let posted = 0;
      const onReq = (r) => { if (r.method() === 'POST' && r.url().includes('/api/channel-proposals')) posted++; };
      pageA.on('request', onReq);
      await pageA.fill('#chSuggestName', 'x'.repeat(40));
      await pageA.click('#chSuggestBtn');
      await pageA.waitForFunction(() => /at most 31 bytes/.test(document.getElementById('chSuggestStatus').textContent));
      assert.strictEqual(await pageA.getAttribute('#chSuggestStatus', 'data-kind'), 'error');
      pageA.off('request', onReq);
      assert.strictEqual(posted, 0);
    });

    await step('A: suggesting a channel reaches "waiting for review" via the ingestor', async () => {
      await pageA.fill('#chSuggestName', NAME.slice(1)); // without '#', like the Monitor field
      await pageA.click('#chSuggestBtn');
      await pageA.waitForFunction((n) => {
        const s = document.getElementById('chSuggestStatus');
        return s && s.textContent.includes(n) && /waiting for an administrator/.test(s.textContent);
      }, NAME);
    });

    await step('A: polling stops when navigating away', async () => {
      let polls = 0;
      pageA.on('request', (r) => { if (r.url().includes('/api/channel-proposals/requests/')) polls++; });
      await pageA.goto(env.base + '/#/packets');
      // Proving an absence needs an observation window: at least one full
      // backoff interval (1s → 1.5s → 2.25s) must pass without a request.
      await pageA.waitForTimeout(3000);
      assert.strictEqual(polls, 0, 'status requests after leaving the page');
    });

    await step('admin: deep link opens the dialog; a wrong key is refused', async () => {
      await pageAdmin.goto(env.base + '/#/channels?view=proposals');
      await pageAdmin.waitForSelector('#chProposalsAdmin[role="dialog"][aria-modal="true"]');
      await pageAdmin.waitForFunction(() => document.activeElement && document.activeElement.id === 'chProposalsKey');
      await pageAdmin.fill('#chProposalsKey', 'wrong-key-wrong-key-wrong-key');
      await pageAdmin.keyboard.press('Enter');
      await pageAdmin.waitForFunction(() => /not accepted/.test(document.getElementById('chProposalsStatus').textContent));
      await pageAdmin.waitForSelector('#chProposalsKey');
    });

    await step('admin: approve the suggestion; the key is kept only in memory', async () => {
      await pageAdmin.fill('#chProposalsKey', API_KEY);
      await pageAdmin.keyboard.press('Enter');
      const row = `.ch-proposals-item:has(.ch-proposals-name:text-is("${NAME}"))`;
      await pageAdmin.waitForSelector(row);
      const leaks = await pageAdmin.evaluate((k) => {
        const blob = JSON.stringify(Object.assign({}, localStorage)) + JSON.stringify(Object.assign({}, sessionStorage)) + location.href + document.cookie;
        return blob.includes(k);
      }, API_KEY);
      assert.strictEqual(leaks, false, 'admin key found in storage, cookie or URL');
      await pageAdmin.click(row + ' [data-proposals-decide="approve"]');
      await pageAdmin.waitForFunction((n) => document.getElementById('chProposalsStatus').textContent.includes(n + ' is now shared'), NAME);
      await pageAdmin.click('[data-proposals-filter="approved"]');
      await pageAdmin.waitForSelector(row + ' .ch-proposals-state[data-state="approved"]');
    });

    await step('admin: Escape closes the dialog, clears the deep link and restores focus', async () => {
      await pageAdmin.keyboard.press('Escape');
      await pageAdmin.waitForSelector('#chProposalsAdmin', { state: 'detached' });
      assert.strictEqual(await pageAdmin.evaluate(() => location.hash), '#/channels');
      const focused = await pageAdmin.evaluate(() => document.activeElement && document.activeElement.id);
      assert.strictEqual(focused, 'chAddChannelBtn');
    });

    await step('ingestor added the approved channel to its live keys', async () => {
      await waitFor('ingestor approval log', () => logHas(stack.ingestor, new RegExp('approved "' + NAME + '" — added to channel keys')));
    });

    await step('B: an independent session lists the channel as shared, without traffic or remove control', async () => {
      await pageB.goto(env.base + '/#/channels');
      const row = `#chList .ch-item[data-hash="${NAME}"]`;
      await pageB.waitForSelector(row + '[data-shared="true"]');
      assert.ok(await pageB.isVisible(row + ' .ch-shared-badge'));
      assert.strictEqual(await pageB.locator(row + ' .ch-remove-btn, ' + row + ' .ch-share-btn').count(), 0);
      assert.match(await pageB.textContent(row + ' .ch-item-preview'), /no messages yet/);
      assert.strictEqual(await pageB.getAttribute(row, 'data-user-added'), null);
      const inNetwork = await pageB.locator(`#chList .ch-section-network .ch-item[data-hash="${NAME}"]`).count();
      assert.strictEqual(inNetwork, 1, 'shared channels are listed under Network');
    });

    await step('A: the suggester sees it too after reloading', async () => {
      // A real reload: within one tab the channel list is served from the
      // client cache (CLIENT_TTL.channels, 15s), like any other channel data.
      await pageA.goto(env.base + '/#/channels');
      await pageA.reload();
      await pageA.waitForSelector(`#chList [data-hash="${NAME}"][data-shared="true"]`);
    });

    await step('mobile: the shared channel, the suggest form and the admin dialog fit 390px', async () => {
      await pageMobile.goto(env.base + '/#/channels');
      await pageMobile.waitForSelector(`#chList .ch-row[data-hash="${NAME}"]`);
      assert.match(await pageMobile.textContent(`#chList .ch-row[data-hash="${NAME}"] .ch-row-preview`), /Shared channel/);
      await pageMobile.tap('#chAddChannelBtn');
      await pageMobile.waitForSelector('#chSuggestSection:not([hidden]) #chSuggestBtn');
      await pageMobile.locator('#chSuggestName').scrollIntoViewIfNeeded();
      assert.ok(await pageMobile.isVisible('#chSuggestName'));
      await pageMobile.goto(env.base + '/#/channels?view=proposals');
      await pageMobile.waitForSelector('#chProposalsAdmin #chProposalsKey');
      const overflow = await pageMobile.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
      assert.ok(overflow <= 0, 'horizontal overflow ' + overflow + 'px');
      const box = await pageMobile.locator('#chProposalsAdmin .ch-proposals-modal').boundingBox();
      assert.ok(box && box.x >= 0 && box.x + box.width <= 390, 'dialog exceeds the viewport: ' + JSON.stringify(box));
    });

    await step('mobile: Remove opens a confirm dialog that fits 390px (cancelled, so the channel stays approved)', async () => {
      await pageMobile.fill('#chProposalsKey', API_KEY);
      await pageMobile.tap('#chProposalsKeyForm button[type="submit"]');
      await pageMobile.click('[data-proposals-filter="approved"]');
      const row = `.ch-proposals-item:has(.ch-proposals-name:text-is("${NAME}"))`;
      await pageMobile.waitForSelector(row + ' [data-proposals-decide="revoke"]');
      await pageMobile.tap(row + ' [data-proposals-decide="revoke"]');
      await pageMobile.waitForSelector('#chProposalsConfirm[role="alertdialog"]');
      const overflow = await pageMobile.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
      assert.ok(overflow <= 0, 'confirm dialog causes horizontal overflow: ' + overflow + 'px');
      const box = await pageMobile.locator('#chProposalsConfirm .ch-proposals-confirm').boundingBox();
      assert.ok(box && box.x >= 0 && box.x + box.width <= 390, 'confirm dialog exceeds the viewport: ' + JSON.stringify(box));
      // Cancel: this mobile pass only proves the dialog itself; the actual
      // revoke below happens once, from the admin desktop session.
      await pageMobile.tap('[data-proposals-confirm-action="cancel"]');
      await pageMobile.waitForSelector('#chProposalsConfirm', { state: 'detached' });
      await pageMobile.waitForSelector(row + ' .ch-proposals-state[data-state="approved"]');
    });

    await step('restart: the approval survives restarting ingestor and server', async () => {
      await stopProcess(stack.server);
      await stopProcess(stack.ingestor);
      stack = await startStack(env);
      await waitFor('ingestor startup load', () => logHas(stack.ingestor, /approved shared channel\(s\) added to channel keys/));
      const res = await fetch(env.base + '/api/channels');
      const body = await res.json();
      const names = (body.approvedChannels || []).map((c) => c.name);
      assert.ok(names.includes(NAME), 'approvedChannels after restart: ' + JSON.stringify(names));
    });

    await step('admin: revoke the approved channel — Escape cancels the confirm layer only, keeping the admin dialog open', async () => {
      await pageAdmin.goto(env.base + '/#/channels?view=proposals');
      await pageAdmin.waitForSelector('#chProposalsAdmin[role="dialog"][aria-modal="true"]');
      // adminKey lives only in this tab's JS memory (see the module doc
      // comment); a hash-only navigation is not a full reload, so it is
      // still unlocked here from the earlier "admin: approve…" step and the
      // list renders directly rather than the key form.
      await pageAdmin.click('[data-proposals-filter="approved"]');
      const row = `.ch-proposals-item:has(.ch-proposals-name:text-is("${NAME}"))`;
      await pageAdmin.waitForSelector(row + ' [data-proposals-decide="revoke"]');
      await pageAdmin.click(row + ' [data-proposals-decide="revoke"]');
      await pageAdmin.waitForSelector('#chProposalsConfirm[role="alertdialog"]');
      assert.match(await pageAdmin.textContent('#chProposalsConfirm'), /stop being shared with everyone/);

      await pageAdmin.keyboard.press('Escape');
      await pageAdmin.waitForSelector('#chProposalsConfirm', { state: 'detached' });
      assert.ok(await pageAdmin.isVisible('#chProposalsAdmin'), 'the admin dialog must stay open after cancelling the confirm');
      const focused = await pageAdmin.evaluate(() => document.activeElement && document.activeElement.getAttribute('data-proposals-decide'));
      assert.strictEqual(focused, 'revoke', 'focus must return to the Remove button that opened the confirm');
    });

    await step('admin: confirming Remove revokes it', async () => {
      const row = `.ch-proposals-item:has(.ch-proposals-name:text-is("${NAME}"))`;
      await pageAdmin.click(row + ' [data-proposals-decide="revoke"]');
      await pageAdmin.waitForSelector('#chProposalsConfirm [data-proposals-confirm-action="confirm"]');
      await pageAdmin.click('#chProposalsConfirm [data-proposals-confirm-action="confirm"]');
      await pageAdmin.waitForFunction((n) => document.getElementById('chProposalsStatus').textContent.includes(n + ' was removed'), NAME);
      await pageAdmin.click('[data-proposals-filter="revoked"]');
      await pageAdmin.waitForSelector(row + ' .ch-proposals-state[data-state="revoked"]');
    });

    await step('ingestor removed the revoked channel from its live keys', async () => {
      await waitFor('ingestor revoke log', () => logHas(stack.ingestor, new RegExp('revoked "' + NAME + '" — removed from channel keys')));
    });

    await step('B: no longer sees the channel as shared once revoked', async () => {
      await pageB.goto(env.base + '/#/channels');
      await pageB.reload();
      const row = `#chList .ch-item[data-hash="${NAME}"]`;
      await pageB.waitForFunction((sel) => !document.querySelector(sel), row);
    });

    await step('restart after revoke: the revoked channel stays gone', async () => {
      await stopProcess(stack.server);
      await stopProcess(stack.ingestor);
      // startStack() already waits for the ingestor's MQTT subscription log,
      // which happens after LoadApproved in main.go — a sufficient readiness
      // signal here. The "N approved shared channel(s) added" log line from
      // the earlier restart step is NOT reusable as a wait condition after a
      // revoke: LoadApproved now legitimately finds zero approved channels,
      // so main.go's `else if n > 0` guard means that line is never printed.
      stack = await startStack(env);
      const res = await fetch(env.base + '/api/channels');
      const body = await res.json();
      const names = (body.approvedChannels || []).map((c) => c.name);
      assert.ok(!names.includes(NAME), 'a revoked channel reappeared after restart: ' + JSON.stringify(names));
    });

    await step('re-suggesting the same name after revoke lands as pending, never auto-approved', async () => {
      await pageA.goto(env.base + '/#/channels');
      await pageA.click('#chAddChannelBtn');
      await pageA.waitForSelector('#chSuggestSection:not([hidden]) #chSuggestName');
      await pageA.fill('#chSuggestName', NAME.slice(1));
      await pageA.click('#chSuggestBtn');
      await pageA.waitForFunction((n) => {
        const s = document.getElementById('chSuggestStatus');
        return s && s.textContent.includes(n) && /waiting for an administrator/.test(s.textContent);
      }, NAME);
      const pendingRes = await fetch(env.base + '/api/admin/channel-proposals?status=pending', { headers: { 'X-API-Key': API_KEY } });
      const pendingNames = ((await pendingRes.json()).proposals || []).map((p) => p.name);
      assert.ok(pendingNames.includes(NAME), 'resuggested channel not pending: ' + JSON.stringify(pendingNames));
      const approvedRes = await fetch(env.base + '/api/admin/channel-proposals?status=approved', { headers: { 'X-API-Key': API_KEY } });
      const approvedNames = ((await approvedRes.json()).proposals || []).map((p) => p.name);
      assert.ok(!approvedNames.includes(NAME), 'resuggestion must never be auto-approved');
    });

    await step('no uncaught page errors', async () => {
      assert.deepStrictEqual(pageErrors, []);
    });
  } finally {
    await browser.close().catch(() => {});
    for (const child of Array.from(children)) await stopProcess(child);
    broker.close();
    if (failed) console.error('logs kept in ' + dir);
    else fs.rmSync(dir, { recursive: true, force: true });
  }
  console.log(`test-channel-proposals-e2e.js: ${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
}

main().catch((e) => {
  console.error('test-channel-proposals-e2e.js: FAIL — ' + ((e && e.stack) || e));
  process.exit(1);
});
