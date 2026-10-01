#!/usr/bin/env node
/**
 * #152 (decrypt race) — E2E: a PSK channel's decrypt doesn't get stuck, or
 * show stale-region messages, when the region filter changes while the
 * client-side decrypt fetch is still in flight.
 *
 * Bug (F1): opening a PSK channel calls selectChannel() -> decryptAndRender()
 * -> fetchAndDecryptChannel(), which fetches /api/packets?...&region=<R> and
 * decrypts matching GRP_TXT packets client-side (public/channel-decrypt.js).
 * If the region changes while that fetch is still in flight:
 *   1. the in-flight fetch's own staleness check (isStaleMessageRequest(),
 *      which compares RegionFilter.getRegionParam() against the region the
 *      request captured) discards its own result once it resolves, leaving
 *      the pane stuck on "Decrypting messages..." forever, AND
 *   2. refreshMessages()'s `if (selCh && selCh.encrypted) return;` early
 *      return means nothing re-fetches for the new region either, so even
 *      after the dust settles the pane never shows the new region's
 *      messages.
 *
 * This test seeds two genuinely-encrypted GRP_TXT packets directly into a
 * temp copy of the E2E fixture DB (one observed only from an SJC observer,
 * one only from an SFO observer) under a PSK key the server does NOT know
 * (so the ingestor/server never auto-decrypts it — this must go through the
 * client-side Web Crypto / pure-JS AES-ECB path in channel-decrypt.js).
 * It then: opens that PSK channel with no region filter selected (decrypt
 * fetch — covering both packets — delayed 3s via page.route), narrows the
 * region filter to SFO only mid-decrypt, and asserts the pane ends up
 * showing just the SFO message (not stuck "Decrypting...", not the stale
 * SJC-region message) with the conversation still open on the same hash.
 *
 * Runs at desktop (1280x800) and mobile (390x844) viewports.
 *
 * Seeding: no sqlite3 CLI is available in this environment, so this file
 * uses Node's built-in (experimental) `node:sqlite` module directly against
 * a temp copy of the fixture DB, before the (read-only, mode=ro) server is
 * started — consistent with AGENTS.md's read/write separation invariant.
 * The raw fixture ships in a schema the ingestor migrates on startup (the
 * server refuses to start against an unmigrated DB — "schema not migrated
 * by ingestor"), so this file briefly runs the real ingestor binary first
 * (against a minimal in-process fake MQTT broker, same trick as
 * test-channel-proposals-e2e.js, with zero real MQTT traffic), stops it once
 * migrations are applied, THEN seeds the encrypted packets, THEN starts the
 * read-only server — all writes happen before the read-only server exists.
 *
 * Usage:
 *   cd cmd/ingestor && go build -o ../../corescope-ingestor .
 *   cd cmd/server && go build -o ../../corescope-server .
 *   node test-channels-client-state-152-decrypt-e2e.js
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
const { spawn } = require('child_process');
const fs = require('fs');
const net = require('net');
const os = require('os');
const path = require('path');
const { DatabaseSync } = require('node:sqlite');
const { chromium } = require('playwright');

const ROOT = __dirname;
const SERVER_BIN = path.resolve(process.env.CORESCOPE_SERVER_BIN || path.join(ROOT, 'corescope-server'));
const INGESTOR_BIN = path.resolve(process.env.CORESCOPE_INGESTOR_BIN || path.join(ROOT, 'corescope-ingestor'));
const FIXTURE_DB = path.resolve(process.env.FIXTURE_DB || path.join(ROOT, 'test-fixtures', 'e2e-fixture.db'));
const PUBLIC_DIR = path.resolve(process.env.PUBLIC_DIR || path.join(ROOT, 'public'));
const TIMEOUT = 20000;
// The firmware-default "Public" channel key — must NOT collide with our
// generated key, since the ingestor/server auto-decrypts that one
// server-side (see cmd/ingestor/main.go), which would bypass the
// client-side decrypt path this test exercises.
const DEFAULT_PUBLIC_KEY_HEX = '8b3387e9c5cdea6ac9e5edbaa115cd72';

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + '\n    ' + ((e && e.stack) || e)); }
}
function ok(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

// ─── crypto: the exact client-side scheme from public/channel-decrypt.js ──
// (key is a raw 16-byte PSK, NOT derived from a channel name)
//   channelHash(1B) = SHA-256(key)[0]
//   ciphertext      = AES-128-ECB(plaintext), block-by-block, no chaining
//   plaintext       = timestamp(4 LE) + flags(1) + "sender: message" + 0x00,
//                     zero-padded to a 16-byte boundary
//   mac(2B)         = HMAC-SHA256(key + 16 zero bytes, ciphertext)[0:2]

function channelHashByteFor(keyBytes) {
  return crypto.createHash('sha256').update(keyBytes).digest()[0];
}

function buildPlaintext(timestampSec, flags, senderMessage) {
  const head = Buffer.alloc(5);
  head.writeUInt32LE(timestampSec >>> 0, 0);
  head.writeUInt8(flags & 0xff, 4);
  const body = Buffer.concat([head, Buffer.from(senderMessage, 'utf8'), Buffer.from([0x00])]);
  const padded = Buffer.alloc(Math.max(16, Math.ceil(body.length / 16) * 16), 0);
  body.copy(padded);
  return padded;
}

function encryptECB(keyBytes, plaintext) {
  const cipher = crypto.createCipheriv('aes-128-ecb', keyBytes, null);
  cipher.setAutoPadding(false);
  return Buffer.concat([cipher.update(plaintext), cipher.final()]);
}

function computeMac(keyBytes, ciphertext) {
  const secret = Buffer.concat([keyBytes, Buffer.alloc(16, 0)]);
  return crypto.createHmac('sha256', secret).update(ciphertext).digest().slice(0, 2);
}

function rfc3339(ms) {
  return new Date(ms).toISOString().replace(/\.\d{3}Z$/, 'Z');
}

// MeshCore wire header byte (cmd/server/decoder.go decodeHeader): bits 0-1
// route type, bits 2-5 payload type, bits 6-7 payload version. RouteDirect=2,
// PayloadGRP_TXT=5 — a real "direct, no path" GRP_TXT packet, so the
// server's startup content-hash pass (ComputeContentHash, cmd/server/
// decoder.go) parses it exactly as it would a captured one instead of
// mis-parsing an arbitrary first byte and colliding with unrelated rows.
const ROUTE_DIRECT = 2;
const PAYLOAD_GRP_TXT = 5;
const PAYLOAD_VERSION = 1;
const HEADER_BYTE = (ROUTE_DIRECT & 0x03) | ((PAYLOAD_GRP_TXT & 0x0f) << 2) | ((PAYLOAD_VERSION & 0x03) << 6);
const PATH_BYTE = 0x00; // hashSize bits=0, hashCount=0 — direct, no recorded hops

/** Build the realistic wire bytes for a GRP_TXT packet: header + path + payload. */
function buildRawPacket(channelHashByte, macBytes, ciphertext) {
  return Buffer.concat([Buffer.from([HEADER_BYTE, PATH_BYTE, channelHashByte]), macBytes, ciphertext]);
}

/**
 * JS port of cmd/server/decoder.go's ComputeContentHash, for a packet built
 * by buildRawPacket() (route=Direct, so no transport-code offset and no path
 * bytes to skip). Keeping this in lock-step with the real server logic means
 * the "hash" column we store is already the canonical value, so the
 * server's startup hash-migration pass (hash_migrate.go) has nothing to
 * change — it reads newHash === tx.Hash and leaves our seeded rows alone.
 */
function computeContentHash16(rawPacket) {
  const payloadType = (rawPacket[0] >> 2) & 0x0f;
  const payload = rawPacket.slice(2); // skip header byte + path byte
  const toHash = Buffer.concat([Buffer.from([payloadType]), payload]);
  return crypto.createHash('sha256').update(toHash).digest('hex').slice(0, 16);
}

/**
 * Seed two real, client-decryptable GRP_TXT packets into `dbPath`, under a
 * PSK key the server has never seen, before the (read-only) server starts.
 * One packet is observed only by an SJC-region observer, the other only by
 * an SFO-region observer — so a region-scoped /api/packets?region=<R>
 * fetch returns exactly one of them.
 *
 * Returns { keyHex, channelName, hash, sjc: {sender,text}, sfo: {sender,text} }
 */
function seedEncryptedChannel(dbPath) {
  const db = new DatabaseSync(dbPath);
  try {
    // Avoid colliding with any GRP_TXT channelHash byte already present in
    // the fixture (real captured traffic on other, unknown channels) — a
    // collision wouldn't break correctness (the MAC check still gates
    // rendering) but it would add noise to the candidate set.
    const used = new Set();
    for (const row of db.prepare(
      "SELECT decoded_json FROM transmissions WHERE payload_type = 5 AND decoded_json LIKE '%channelHash%'"
    ).all()) {
      try {
        const dj = JSON.parse(row.decoded_json);
        if (typeof dj.channelHash === 'number') used.add(dj.channelHash);
      } catch (e) { /* ignore malformed rows */ }
    }

    let keyBytes, keyHex, hashByte;
    for (let i = 0; i < 1000; i++) {
      keyBytes = crypto.randomBytes(16);
      keyHex = keyBytes.toString('hex');
      if (keyHex === DEFAULT_PUBLIC_KEY_HEX) continue;
      hashByte = channelHashByteFor(keyBytes);
      if (!used.has(hashByte)) break;
    }
    ok(keyBytes && !used.has(hashByte), 'could not find a non-colliding PSK key after 1000 tries');

    const hashHex = hashByte.toString(16).toUpperCase().padStart(2, '0');
    const channelName = 'psk:' + keyHex.substring(0, 8); // matches addUserChannel()'s naming

    function observerRowid(iata) {
      const row = db.prepare(
        'SELECT rowid FROM observers WHERE UPPER(TRIM(iata)) = ? ORDER BY rowid LIMIT 1'
      ).get(iata);
      ok(row, 'fixture has no observer with iata=' + iata);
      return row.rowid;
    }
    const sjcObserverRowid = observerRowid('SJC');
    const sfoObserverRowid = observerRowid('SFO');

    // #1690: post-migration schemas add transmissions.last_seen (used by the
    // server's memory-cap / hot-window load query, ORDER BY last_seen DESC).
    // Set it explicitly so our freshly-seeded rows always sort as "freshest"
    // rather than defaulting to 0 and risking eviction behind 500 real rows.
    const hasLastSeen = db.prepare("PRAGMA table_info(transmissions)").all()
      .some((c) => c.name === 'last_seen');

    const insertTx = db.prepare(
      `INSERT INTO transmissions (raw_hex, hash, first_seen, route_type, payload_type, payload_version, decoded_json, channel_hash${hasLastSeen ? ', last_seen' : ''})
       VALUES (?, ?, ?, ?, 5, 1, ?, ?${hasLastSeen ? ', ?' : ''})`
    );
    const insertObs = db.prepare(
      `INSERT INTO observations (transmission_id, observer_idx, direction, snr, rssi, score, path_json, timestamp)
       VALUES (?, ?, NULL, ?, ?, NULL, '[]', ?)`
    );

    function seedOne(observerRowid, sender, text, ageSec, snr) {
      const nowMs = Date.now();
      const tsSec = Math.floor((nowMs - ageSec * 1000) / 1000);
      const plaintext = buildPlaintext(tsSec, 0, sender + ': ' + text);
      const ciphertext = encryptECB(keyBytes, plaintext);
      const mac = computeMac(keyBytes, ciphertext);
      const macHex = mac.toString('hex');
      const encryptedDataHex = ciphertext.toString('hex');
      const decoded = JSON.stringify({
        type: 'GRP_TXT',
        encryptedData: encryptedDataHex,
        mac: macHex,
        channelHash: hashByte,
        channelHashHex: hashHex,
        decryptionStatus: 'no_key',
      });
      const rawPacket = buildRawPacket(hashByte, mac, ciphertext);
      const rawHex = rawPacket.toString('hex');
      const txHash = computeContentHash16(rawPacket);
      const args = [rawHex, txHash, rfc3339(tsSec * 1000), ROUTE_DIRECT, decoded, 'enc_' + hashHex];
      if (hasLastSeen) args.push(tsSec);
      const info = insertTx.run(...args);
      insertObs.run(info.lastInsertRowid, observerRowid, snr, snr - 90, tsSec);
      return { sender, text, hash: txHash };
    }

    const sjc = seedOne(sjcObserverRowid, 'IssueBot', 'SJC-ALPHA-MSG-152', 120, 6.5);
    const sfo = seedOne(sfoObserverRowid, 'IssueBot', 'SFO-BRAVO-MSG-152', 60, 5.0);

    return { keyHex, channelName, hash: 'user:' + channelName, sjc, sfo };
  } finally {
    db.close();
  }
}

// ─── process lifecycle (subset of test-channel-proposals-e2e.js's) ────────

function findFallbackChromium() {
  const base = process.env.PLAYWRIGHT_BROWSERS_PATH;
  if (!base || !fs.existsSync(base)) return undefined;
  let dirs;
  try { dirs = fs.readdirSync(base).filter((d) => d.startsWith('chromium-')); } catch (e) { return undefined; }
  for (const d of dirs) {
    const p = path.join(base, d, 'chrome-linux', 'chrome');
    if (fs.existsSync(p)) return p;
  }
  return undefined;
}

function freePort() {
  return new Promise((resolve, reject) => {
    const s = net.createServer();
    s.listen(0, '127.0.0.1', () => { const p = s.address().port; s.close(() => resolve(p)); });
    s.on('error', reject);
  });
}

const children = new Set();
let startCount = 0;
function startProcess(label, bin, args, dir) {
  const logFile = path.join(dir, label + '-' + (++startCount) + '.log');
  const out = fs.openSync(logFile, 'w');
  const proc = spawn(bin, args, { stdio: ['ignore', out, out] });
  fs.closeSync(out);
  proc.label = label;
  proc.logFile = logFile;
  children.add(proc);
  proc.once('exit', () => children.delete(proc));
  return proc;
}

async function stopProcess(proc) {
  if (!proc || proc.exitCode !== null) return;
  const exited = new Promise((r) => proc.once('exit', r));
  proc.kill('SIGTERM');
  const t = setTimeout(() => proc.kill('SIGKILL'), 5000);
  await exited;
  clearTimeout(t);
}

async function waitFor(what, fn, timeoutMs) {
  const deadline = Date.now() + (timeoutMs || TIMEOUT);
  let last;
  while (Date.now() < deadline) {
    try { const v = await fn(); if (v) return v; } catch (e) { last = e; }
    await new Promise((r) => setTimeout(r, 200));
  }
  throw new Error('timed out waiting for ' + what + (last ? ': ' + last.message : ''));
}

function logHas(proc, re) {
  try { return re.test(fs.readFileSync(proc.logFile, 'utf8')); } catch (e) { return false; }
}

// ─── minimal MQTT 3.1.1 broker (lets the ingestor start — no packets are
// published; everything this test needs is seeded directly into the DB
// instead). Copied from test-channel-proposals-e2e.js. ───────────────────
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

/**
 * Run the real ingestor binary just long enough to apply its schema
 * migrations to `dbPath` (the server refuses to start against an
 * unmigrated DB), then stop it. No MQTT traffic is needed or sent — the
 * fake broker only exists so the ingestor's startup sequence (which runs
 * migrations before connecting) completes cleanly instead of timing out.
 */
async function migrateFixture(dbPath, dir) {
  const broker = await startFakeBroker();
  const ingestorConfig = path.join(dir, 'ingestor.json');
  fs.writeFileSync(ingestorConfig, JSON.stringify({
    dbPath,
    mqttSources: [{ name: 'e2e', broker: 'mqtt://127.0.0.1:' + broker.port, topics: ['meshcore/#'], connectTimeoutSec: 5 }],
  }));
  const ingestor = startProcess('ingestor', INGESTOR_BIN, ['-config', ingestorConfig], dir);
  try {
    await waitFor('ingestor migration + MQTT subscription', () => {
      if (ingestor.exitCode !== null) throw new Error('ingestor exited: see ' + ingestor.logFile);
      return logHas(ingestor, /MQTT \[e2e\] subscribed/);
    });
  } finally {
    await stopProcess(ingestor);
    broker.close();
  }
}

// ─── page helpers ──────────────────────────────────────────────────────

// The region pills are a MULTI-select toggle (region-filter.js's
// toggleRegion): clicking a region that isn't selected ADDS it to the
// current set (or, when nothing is selected yet, becomes the sole
// selection — the "All regions" default is a null set, not an empty one).
// Starting from "All regions" (the page's default, untouched state) and
// clicking exactly one region pill is therefore a single, clean,
// uncontested region change — no intermediate combined-region state, and
// critically no intermediate /api/packets fetch that would populate
// channel-decrypt.js's (region-unaware) message cache with the wrong
// region's content before the real switch this test cares about happens.
async function selectOnlyRegion(page, code) {
  const pill = page.locator('#chRegionFilter [data-region="' + code + '"]');
  await pill.waitFor({ state: 'attached', timeout: 8000 });
  const response = page.waitForResponse((r) => {
    const u = decodeURIComponent(r.url());
    return u.indexOf('/api/channels?') !== -1 && u.indexOf('/messages') === -1 && u.indexOf('region=' + code) !== -1;
  }, { timeout: 8000 });
  const click = pill.click({ timeout: 2000 }).catch(() => pill.evaluate((el) => el.click()));
  await Promise.all([response, click]);
}

function messagePaneState(page) {
  return page.evaluate(() => {
    const msgEl = document.getElementById('chMessages');
    const row = document.querySelector('#chList .ch-row.selected, #chList .ch-item.selected, #chList [aria-selected="true"]');
    return {
      urlHash: location.hash,
      loading: !!(msgEl && (msgEl.querySelector('.ch-loading') || /Decrypting/.test(msgEl.textContent))),
      text: msgEl ? msgEl.textContent : '',
      senders: msgEl ? Array.from(msgEl.querySelectorAll('.ch-msg-sender')).map((e) => e.textContent) : [],
      bubbles: msgEl ? Array.from(msgEl.querySelectorAll('.ch-msg-bubble')).map((e) => e.textContent) : [],
      rowSelectedHash: row ? row.getAttribute('data-hash') : null,
    };
  });
}

// ─── test body ─────────────────────────────────────────────────────────

async function runViewport(browser, vp, base, seed) {
  const ctx = await browser.newContext({ viewport: { width: vp.width, height: vp.height } });
  const page = await ctx.newPage();
  page.setDefaultTimeout(10000);
  const pageErrors = [];
  page.on('pageerror', (e) => pageErrors.push(e.message));

  await page.goto(base + '/#/channels', { waitUntil: 'domcontentloaded' });
  await page.evaluate(() => { try { localStorage.clear(); } catch (e) {} });
  await page.reload({ waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#chList .ch-item, #chList .ch-row', { timeout: 10000 });

  // Region starts at its page default ("All regions", a null selection) —
  // deliberately NOT pre-selecting SJC here. The first decrypt fetch below
  // therefore runs unfiltered (sees both the SJC and SFO packets), and the
  // single-click switch to SFO further down is a clean, uncontested region
  // change (see selectOnlyRegion's doc comment) instead of needing a second
  // click to undo a prior region pre-selection — which would itself issue
  // an intermediate /api/packets fetch and pollute channel-decrypt.js's
  // (region-unaware, keyed only by channel name) message cache with
  // combined-region content before the real switch under test happens.

  // Delay only the FIRST /api/packets response by ~3s, so it's still
  // in-flight when the region switch below fires. Later calls (the
  // region-change handler's own re-fetch, once F1 is fixed) pass straight
  // through so the test doesn't just become an exercise in waiting.
  let packetsCalls = 0;
  await page.route('**/api/packets**', async (route) => {
    packetsCalls++;
    if (packetsCalls === 1) {
      await new Promise((r) => setTimeout(r, 3000));
    }
    await route.continue();
  });

  await step(vp.name + ': adding the seeded PSK opens its conversation and starts decrypting', async () => {
    await page.click('#chAddChannelBtn');
    await page.waitForSelector('#chAddChannelModal:not(.hidden)');
    await page.fill('#chPskKey', seed.keyHex);
    await page.fill('#chPskName', 'Issue152 Decrypt Race');
    await page.click('#chPskAddBtn');
    await page.waitForFunction((hash) => location.hash === '#/channels/' + encodeURIComponent(hash),
      seed.hash, { timeout: 10000 });
  });

  await step(vp.name + ': caught mid-decrypt — still showing "Decrypting messages..." when region changes', async () => {
    const s = await messagePaneState(page);
    ok(s.loading, 'expected the decrypt to still be in flight (pane: ' + JSON.stringify(s.text.slice(0, 80)) + ')');
    ok(packetsCalls === 1, 'expected exactly 1 /api/packets call so far, got ' + packetsCalls);
  });

  await step(vp.name + ': narrow to region SFO while the (unfiltered) decrypt is still in flight', async () => {
    await selectOnlyRegion(page, 'SFO');
  });

  await step(vp.name + ': the pane eventually settles (not stuck on "Decrypting...")', async () => {
    await page.waitForFunction(() => {
      const m = document.getElementById('chMessages');
      return m && !m.querySelector('.ch-loading') && !/Decrypting/.test(m.textContent);
    }, null, { timeout: 15000 });
  });

  await step(vp.name + ': the pane shows the NEW region (SFO) message, not the stale SJC one', async () => {
    const s = await messagePaneState(page);
    ok(s.text.indexOf(seed.sfo.text) !== -1,
      'expected the SFO message in the pane, got: ' + JSON.stringify(s.text.slice(0, 200)));
    ok(s.text.indexOf(seed.sjc.text) === -1,
      'the stale SJC-region message must not be shown after switching to SFO, got: ' + JSON.stringify(s.text.slice(0, 200)));
  });

  await step(vp.name + ': the conversation stayed open on the same PSK channel (not closed/reset)', async () => {
    const s = await messagePaneState(page);
    ok(s.urlHash === '#/channels/' + encodeURIComponent(seed.hash), 'URL must stay on the PSK channel, got ' + s.urlHash);
    ok(s.text.indexOf('Choose a channel') === -1, 'conversation must not have closed');
    ok(s.rowSelectedHash === seed.hash, 'the PSK row must still be selected, got ' + s.rowSelectedHash);
  });

  await step(vp.name + ': no uncaught page errors', async () => {
    // "L is not defined" is Leaflet (public/index.html loads it from
    // unpkg.com) failing to load because this test's sandbox has no
    // outbound internet access to third-party CDNs — unrelated to the
    // channels/decrypt code this test exercises, and already the case
    // with no PSK/region interaction at all. A real environment (CI,
    // staging) has network access and never sees it; test-channels-
    // client-state-152-e2e.js (this test's sibling for the same issue)
    // likewise only logs page errors for this page rather than failing on
    // them, for the same reason.
    const unexpected = pageErrors.filter((m) => !/^L is not defined$/.test(m));
    assert.deepStrictEqual(unexpected, [], 'unexpected page errors: ' + JSON.stringify(pageErrors));
  });

  await page.evaluate(() => { try { localStorage.clear(); } catch (e) {} });
  await ctx.close();
}

async function main() {
  for (const p of [SERVER_BIN, FIXTURE_DB]) {
    if (!fs.existsSync(p)) {
      console.error('test-channels-client-state-152-decrypt-e2e.js: FAIL — missing ' + p);
      process.exit(1);
    }
  }

  let browser;
  const launchArgs = { headless: true, args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'] };
  try {
    browser = await chromium.launch(Object.assign({}, launchArgs, { executablePath: process.env.CHROMIUM_PATH || undefined }));
  } catch (err) {
    // Fallback: the resolved `playwright` package's expected browser revision
    // may not match what's actually unpacked under PLAYWRIGHT_BROWSERS_PATH
    // (e.g. a sibling process installed a newer playwright into this shared
    // worktree's node_modules). Point at whatever chromium build IS present
    // instead of giving up — an executablePath override works across minor
    // version gaps for the plain navigation/fill/click/route APIs this test
    // uses.
    const fallback = !process.env.CHROMIUM_PATH && findFallbackChromium();
    if (fallback) {
      try {
        browser = await chromium.launch(Object.assign({}, launchArgs, { executablePath: fallback }));
        console.log('test-channels-client-state-152-decrypt-e2e.js: using fallback Chromium at ' + fallback);
      } catch (err2) {
        err = err2;
      }
    }
    if (!browser) {
      if (process.env.CHROMIUM_REQUIRE === '1') {
        console.error('test-channels-client-state-152-decrypt-e2e.js: FAIL — Chromium required but unavailable: ' + err.message);
        process.exit(1);
      }
      console.log('test-channels-client-state-152-decrypt-e2e.js: SKIP (Chromium unavailable: ' + err.message.split('\n')[0] + ')');
      process.exit(0);
    }
  }

  if (!fs.existsSync(INGESTOR_BIN)) {
    console.error('test-channels-client-state-152-decrypt-e2e.js: FAIL — missing ' + INGESTOR_BIN);
    process.exit(1);
  }

  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'corescope-decrypt152-e2e-'));
  const dbPath = path.join(dir, 'meshcore.db');
  const serverDir = path.join(dir, 'server');
  fs.mkdirSync(serverDir);
  fs.copyFileSync(FIXTURE_DB, dbPath);

  console.log('running the ingestor once to migrate the fixture schema ...');
  await migrateFixture(dbPath, dir);

  console.log('seeding encrypted PSK packets into ' + dbPath + ' ...');
  const seed = seedEncryptedChannel(dbPath);
  console.log('  PSK key:      ' + seed.keyHex);
  console.log('  channel hash: ' + seed.hash);
  console.log('  SJC message:  "' + seed.sjc.sender + ': ' + seed.sjc.text + '"');
  console.log('  SFO message:  "' + seed.sfo.sender + ': ' + seed.sfo.text + '"');

  const port = await freePort();
  const base = 'http://127.0.0.1:' + port;

  try {
    const server = startProcess('server', SERVER_BIN,
      ['-port', String(port), '-db', dbPath, '-public', PUBLIC_DIR, '-config-dir', serverDir],
      dir);
    await waitFor('server health', async () => {
      if (server.exitCode !== null) throw new Error('server exited early: see ' + server.logFile);
      const res = await fetch(base + '/api/healthz');
      return res.ok;
    });
    await waitFor('server reports SJC and SFO regions', async () => {
      const res = await fetch(base + '/api/config/regions');
      const regions = await res.json();
      return 'SJC' in regions && 'SFO' in regions;
    });

    console.log('\n=== #152 decrypt-race E2E against ' + base + ' ===');
    await runViewport(browser, { name: 'desktop', width: 1280, height: 800 }, base, seed);
    await runViewport(browser, { name: 'mobile', width: 390, height: 844 }, base, seed);
  } finally {
    await browser.close().catch(() => {});
    for (const proc of Array.from(children)) await stopProcess(proc);
    if (failed) console.error('logs kept in ' + dir);
    else fs.rmSync(dir, { recursive: true, force: true });
  }

  console.log('\n=== Results: ' + passed + ' passed, ' + failed + ' failed ===');
  process.exit(failed > 0 ? 1 : 0);
}

if (require.main === module) {
  main().catch((e) => {
    console.error('test-channels-client-state-152-decrypt-e2e.js: FATAL — ' + ((e && e.stack) || e));
    process.exit(1);
  });
}

module.exports = {
  seedEncryptedChannel, channelHashByteFor, buildPlaintext, encryptECB, computeMac,
  buildRawPacket, computeContentHash16,
};
