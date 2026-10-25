#!/usr/bin/env node
/* E2E: admin approval of observed region scopes (#355), with the real
 * binaries. Starts the ingestor and server (both built from this repo)
 * against a temp copy of the E2E fixture database and a minimal in-process
 * MQTT broker, seeds an observer /neighbors report with three scope names
 * (one of them configured), then drives Tools → Observer Neighbors:
 *
 *   anonymous   sees the two unknown names, read-only
 *   admin       a wrong key is refused; approve one name, reject the other
 *   packets     scoped packets published through the broker show that a new
 *               packet matches at once, that the ingestor's decode backfill
 *               then re-matches the stored unknown-scoped one (#380) and
 *               reports it on /api/healthz, and that a SIGHUP reload neither
 *               drops the approval nor brings back a revocation
 *   restart     the approval survives restarting both processes
 *   mobile      the decision buttons stay reachable at 390x844
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
const crypto = require('crypto');
const { execFileSync } = require('child_process');
const fs = require('fs');
const os = require('os');
const path = require('path');
const { chromium } = require('playwright');
const { startFakeBroker, freePort, createProcessGroup, stopProcess, waitFor, logText } = require('./scripts/e2e-real-stack');

const ROOT = __dirname;
const SERVER_BIN = path.resolve(process.env.CORESCOPE_SERVER_BIN || path.join(ROOT, 'corescope-server'));
const INGESTOR_BIN = path.resolve(process.env.CORESCOPE_INGESTOR_BIN || path.join(ROOT, 'corescope-ingestor'));
const FIXTURE_DB = path.resolve(process.env.FIXTURE_DB || path.join(ROOT, 'test-fixtures', 'e2e-fixture.db'));
const PUBLIC_DIR = path.resolve(process.env.PUBLIC_DIR || path.join(ROOT, 'public'));
const API_KEY = 'e2e-scope-approval-admin-key-0123456789';
const RUN = Date.now().toString(36).slice(-6);
const NORTH = '#E2eNorth' + RUN;
const SOUTH = '#E2eSouth' + RUN;
const CONFIGURED = '#E2eCfg' + RUN;
const TIMEOUT = 20000;
const SELF = 'test-region-scope-approval-e2e.js';

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

// A TRANSPORT_FLOOD TXT_MSG whose Code1 is what a node in scope `name` sends
// for this payload (firmware: HMAC-SHA256 keyed with SHA256(name)[:16]).
function code1For(name, payload) {
  const key = crypto.createHash('sha256').update(name).digest().subarray(0, 16);
  const mac = crypto.createHmac('sha256', key).update(Buffer.concat([Buffer.from([2]), payload])).digest();
  let code = mac[0] | (mac[1] << 8);
  if (code === 0) code = 1;
  else if (code === 0xffff) code = 0xfffe;
  return code;
}
function scopedPacket(name, text) {
  let payload = Buffer.from(text + ' ' + RUN);
  // Keep the fixture unambiguous: the code must not also match the other
  // live region (a 2/65534 chance otherwise).
  while (code1For(name, payload) === code1For(CONFIGURED, payload)) payload = Buffer.concat([payload, Buffer.from('.')]);
  const code = code1For(name, payload);
  const hex2 = (n) => n.toString(16).padStart(2, '0');
  return ('08' + hex2(code & 0xff) + hex2(code >> 8) + '0000' + '00' + payload.toString('hex')).toUpperCase();
}

async function main() {
  for (const file of [SERVER_BIN, INGESTOR_BIN, FIXTURE_DB]) {
    if (!fs.existsSync(file)) {
      console.error(SELF + ': FAIL — missing ' + file);
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
      console.error(SELF + ': FAIL — Chromium required but unavailable: ' + err.message);
      process.exit(1);
    }
    console.log(SELF + ': SKIP (Chromium unavailable: ' + err.message.split('\n')[0] + ')');
    process.exit(0);
  }

  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'corescope-scope-approval-e2e-'));
  const procs = createProcessGroup(dir);
  const broker = await startFakeBroker();
  const env = { db: path.join(dir, 'meshcore.db'), port: await freePort(), serverDir: path.join(dir, 'server') };
  env.base = 'http://127.0.0.1:' + env.port;
  env.ingestorConfig = path.join(dir, 'ingestor.json');
  fs.copyFileSync(FIXTURE_DB, env.db);
  fs.mkdirSync(env.serverDir);
  fs.writeFileSync(env.ingestorConfig, JSON.stringify({
    dbPath: env.db,
    mqttSources: [{ name: 'e2e', broker: 'mqtt://127.0.0.1:' + broker.port, topics: ['meshcore/#'], connectTimeoutSec: 5 }],
    hashRegions: [CONFIGURED],
    // #380: the decode backfill polls the effective key set every 30s by
    // default. One second here so an approval's re-match of stored history is
    // observable inside a test step; the pass itself is unchanged.
    decodeBackfill: { pollSeconds: 1, pauseMs: 1 },
  }));
  fs.writeFileSync(path.join(env.serverDir, 'config.json'), JSON.stringify({ apiKey: API_KEY, hashRegions: [CONFIGURED] }));

  async function startStack() {
    const ingestor = procs.start('ingestor', INGESTOR_BIN, ['-config', env.ingestorConfig]);
    await waitFor('ingestor MQTT subscription', () => {
      if (ingestor.exitCode !== null) throw new Error('ingestor exited: see ' + ingestor.logFile);
      return /MQTT \[e2e\] subscribed/.test(logText(ingestor));
    });
    const server = procs.start('server', SERVER_BIN, ['-port', String(env.port), '-db', env.db, '-public', PUBLIC_DIR, '-config-dir', env.serverDir]);
    await waitFor('server health', async () => {
      if (server.exitCode !== null) throw new Error('server exited: see ' + server.logFile);
      return (await fetch(env.base + '/api/healthz')).ok;
    });
    return { ingestor, server };
  }
  const sql = (statement) => execFileSync('sqlite3', ['-cmd', '.timeout 5000', env.db, statement], { encoding: 'utf8' }).trim();
  // null while the transmission is not stored yet; '' when stored unknown-scoped.
  const scopeOf = (raw) => {
    const out = sql("SELECT '[' || COALESCE(scope_name, '<null>') || ']' FROM transmissions WHERE upper(raw_hex) = '" + raw + "'");
    return out ? out.slice(1, -1) : null;
  };
  async function ingest(raw, observer) {
    const subscribers = broker.publish('meshcore/SJC/' + (observer || 'e2eobs1') + '/packets', JSON.stringify({ raw: raw }));
    assert.ok(subscribers > 0, 'no MQTT subscriber');
    await waitFor('transmission ' + raw.slice(0, 12), () => scopeOf(raw) !== null);
  }
  async function sighup(ingestor, approved) {
    const re = new RegExp('and ' + approved + ' approved region scope\\(s\\) kept');
    const before = (logText(ingestor).match(/\[hot-reload\] reloaded/g) || []).length;
    ingestor.kill('SIGHUP');
    await waitFor('SIGHUP reload', () => (logText(ingestor).match(/\[hot-reload\] reloaded/g) || []).length > before);
    const lines = logText(ingestor).split('\n').filter((l) => l.includes('[hot-reload] reloaded'));
    assert.match(lines[lines.length - 1], re);
  }
  const unknownText = (page) => page.locator('#obs-nb-unknown-scopes-wrap').innerText();

  let stack;
  const pageErrors = [];
  const dialogs = [];
  try {
    stack = await startStack();
    console.log(SELF + ' (' + RUN + ', ' + env.base + ')');
    sql("INSERT INTO observer_neighbors(observer_id, neighbor_pubkey, scopes, status, reported_at) VALUES " +
      "('e2eobs1', 'e2e355aa', '" + NORTH + ',' + SOUTH + ',' + CONFIGURED + "', 'responded', strftime('%Y-%m-%dT%H:%M:%SZ','now')), " +
      "('e2eobs1', 'e2e355bb', '" + NORTH + "', 'responded', strftime('%Y-%m-%dT%H:%M:%SZ','now'))");

    const context = await browser.newContext({ viewport: { width: 1280, height: 900 } });
    const page = await context.newPage();
    page.setDefaultTimeout(TIMEOUT);
    page.on('pageerror', (e) => pageErrors.push(e.message));
    page.on('dialog', (d) => { dialogs.push(d.message()); d.accept(); });

    const before = scopedPacket(NORTH, 'stored before approval');
    await step('a scoped packet for an unknown scope is stored unknown-scoped', async () => {
      await ingest(before);
      assert.strictEqual(scopeOf(before), '');
    });

    await step('anonymous: unknown scopes are listed read-only; the configured one is not', async () => {
      await page.goto(env.base + '/#/tools/observer-neighbors');
      await page.locator('#obs-nb-unknown-scopes-wrap').getByText(NORTH).waitFor();
      const text = await unknownText(page);
      assert.ok(text.includes(SOUTH), 'south missing');
      assert.ok(!text.includes(CONFIGURED), 'configured scope listed as unknown');
      assert.strictEqual(await page.locator('[data-scope-action]').count(), 0, 'anonymous action buttons');
    });

    await step('admin: a wrong key is refused', async () => {
      await page.locator('#obs-nb-scope-admin summary').click();
      await page.locator('#obs-nb-admin-key').fill('wrong-key-wrong-key-wrong-key');
      await page.locator('#obs-nb-admin-load').click();
      await page.waitForFunction(() => /key|unauthor|forbidden/i.test(document.getElementById('obs-nb-admin-status').textContent));
      assert.strictEqual(await page.locator('[data-scope-action]').count(), 0, 'wrong key unlocked actions');
    });

    await step('admin: approve one scope; it leaves the unknown list and the ingestor activates it', async () => {
      await page.locator('#obs-nb-admin-key').fill(API_KEY);
      await page.locator('#obs-nb-admin-load').click();
      await page.locator('#obs-nb-admin-list').getByText('No scope decisions yet.').waitFor();
      await page.locator(`button[data-scope-action="approve"][data-scope-name="${NORTH}"]`).click();
      await page.locator(`button[data-scope-action="revoke"][data-scope-name="${NORTH}"]`).waitFor();
      await page.waitForFunction((n) => !document.getElementById('obs-nb-unknown-scopes-wrap').textContent.includes(n), NORTH);
      await page.locator('#obs-nb-admin-list').getByText('1 of 128 approval slots used.').waitFor();
      assert.match(dialogs[dialogs.length - 1], /received from now on.*still unknown can be re-matched in the background/);
      assert.match(logText(stack.ingestor), new RegExp('approved "' + NORTH + '": 1 approved region scope\\(s\\) live'));
    });

    await step('admin: reject the other scope; it stays listed and nothing is activated', async () => {
      await page.locator(`button[data-scope-action="reject"][data-scope-name="${SOUTH}"]`).click();
      await page.locator('#obs-nb-admin-list').getByText('rejected').waitFor();
      assert.ok((await unknownText(page)).includes(SOUTH));
      assert.match(logText(stack.ingestor), new RegExp('rejected "' + SOUTH + '": 1 approved region scope\\(s\\) live'));
    });

    const during = scopedPacket(NORTH, 'received while approved');
    await step('SIGHUP keeps the approval; a new packet matches and the stored one is re-matched by the decode backfill (#380)', async () => {
      await sighup(stack.ingestor, 1);
      // The stored packet is heard again by another observer. MQTT keeps the
      // order on one connection, so once `during` is stored the repeat has
      // been processed too.
      broker.publish('meshcore/SJC/e2eobs2/packets', JSON.stringify({ raw: before }));
      await ingest(during);
      assert.strictEqual(scopeOf(during), NORTH);
      assert.strictEqual(sql("SELECT COUNT(*) FROM transmissions WHERE upper(raw_hex) = '" + before + "'"), '1');
      // #380: the packet stored before the approval is re-matched by the
      // ingestor's backfill, which re-decodes it from its stored frame.
      await waitFor('decode backfill re-matches the stored packet', () => scopeOf(before) === NORTH);
      assert.match(logText(stack.ingestor), /\[decode_backfill\] done: kinds=scope/);
      // The server reflects it without a restart: /api/transmissions-shaped
      // reads come from the database, and the caches were dropped on the next
      // poll tick.
      await waitFor('healthz reports the re-matched row', async () => {
        const health = await (await fetch(env.base + '/api/healthz')).json();
        return health.decode_backfill && health.decode_backfill.scopeUpdated >= 1;
      });
    });

    await step('revoke: listed as unknown again, still revoked after SIGHUP, history kept', async () => {
      await page.locator(`button[data-scope-action="revoke"][data-scope-name="${NORTH}"]`).click();
      await page.waitForFunction((n) => document.getElementById('obs-nb-unknown-scopes-wrap').textContent.includes(n), NORTH);
      assert.match(dialogs[dialogs.length - 1], /Stored packets already matched to a scope keep it/);
      await sighup(stack.ingestor, 0);
      const after = scopedPacket(NORTH, 'received after revocation');
      await ingest(after);
      assert.strictEqual(scopeOf(after), '');
      assert.strictEqual(scopeOf(during), NORTH, 'revocation rewrote a stored match');
    });

    await step('restart: a renewed approval survives restarting ingestor and server', async () => {
      await page.locator(`button[data-scope-action="approve"][data-scope-name="${NORTH}"]`).click();
      await page.locator(`button[data-scope-action="revoke"][data-scope-name="${NORTH}"]`).waitFor();
      await stopProcess(stack.server);
      await stopProcess(stack.ingestor);
      stack = await startStack();
      await waitFor('startup load', () => /\[scope-approval\] 1 approved region scope\(s\) loaded/.test(logText(stack.ingestor)));
      const restarted = scopedPacket(NORTH, 'received after restart');
      await ingest(restarted);
      assert.strictEqual(scopeOf(restarted), NORTH);
      const res = await fetch(env.base + '/api/admin/region-scopes', { headers: { 'X-API-Key': API_KEY } });
      const body = await res.json();
      assert.deepStrictEqual(body.decisions.map((d) => d.name + '=' + d.status).sort(), [NORTH + '=approved', SOUTH + '=rejected'].sort());
      assert.strictEqual(body.limit, 128);
    });

    await step('the admin key is kept only in memory', async () => {
      const leaks = await page.evaluate((k) => {
        const blob = JSON.stringify(Object.assign({}, localStorage)) + JSON.stringify(Object.assign({}, sessionStorage)) + location.href + document.cookie;
        return blob.includes(k);
      }, API_KEY);
      assert.strictEqual(leaks, false, 'admin key found in storage, cookie or URL');
    });

    await step('mobile: the reject action is reachable by scrolling its table', async () => {
      await page.reload();
      await page.locator('#obs-nb-scope-admin summary').click();
      await page.locator('#obs-nb-admin-key').fill(API_KEY);
      await page.locator('#obs-nb-admin-load').click();
      await page.setViewportSize({ width: 390, height: 844 });
      const reject = page.locator(`button[data-scope-action="reject"][data-scope-name="${SOUTH}"]`);
      await reject.waitFor();
      const table = page.locator('#obs-nb-unknown-scopes-wrap .table-fluid-wrap');
      const overflow = await table.evaluate((el) => ({ max: el.scrollWidth - el.clientWidth,
        page: document.documentElement.scrollWidth - document.documentElement.clientWidth }));
      assert.ok(overflow.max > 0, 'unknown-scope table should scroll horizontally at 390px');
      assert.ok(overflow.page <= 1, 'the page itself must not overflow horizontally');
      await table.evaluate((el) => { el.scrollLeft = el.scrollWidth; });
      const geometry = await reject.evaluate((el) => {
        const b = el.getBoundingClientRect();
        const w = el.closest('.table-fluid-wrap').getBoundingClientRect();
        // A cell with text-overflow:ellipsis paints an overflowing button as
        // "…" even though its box lies inside the cell, so also require the
        // action cell itself not to overflow.
        const td = el.closest('td');
        return { width: b.width, inside: b.left >= w.left - 1 && b.right <= w.right + 1,
          cellOverflow: td.scrollWidth - td.clientWidth, textFits: el.scrollWidth <= el.clientWidth + 1 };
      });
      assert.ok(geometry.width > 0 && geometry.inside && geometry.cellOverflow <= 1 && geometry.textFits, 'reject button clipped: ' + JSON.stringify(geometry));
      await page.screenshot({ path: path.join(process.env.SCREENSHOT_DIR || dir, 'scope-approval-355-mobile.png'), fullPage: true });
    });

    await step('no uncaught page errors', async () => {
      assert.deepStrictEqual(pageErrors, []);
    });
  } finally {
    await browser.close().catch(() => {});
    await procs.stopAll();
    broker.close();
    if (failed) console.error('logs kept in ' + dir);
    else fs.rmSync(dir, { recursive: true, force: true });
  }
  console.log(SELF + ': ' + passed + ' passed, ' + failed + ' failed');
  process.exit(failed ? 1 : 0);
}

main().catch((e) => {
  console.error(SELF + ': FAIL — ' + ((e && e.stack) || e));
  process.exit(1);
});
