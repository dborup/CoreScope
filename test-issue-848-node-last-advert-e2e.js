/**
 * E2E #848: a repeater quiet longer than the packet-store window is no longer
 * "Stale — Last activity unknown" on its node page. The server reads its
 * newest own ADVERT from the database, and the node page, the nodes list and
 * the map agree on its status.
 *
 * Starts its own Go server on a copy of the CI fixture DB with
 * packetStore.retentionHours 24 (default health thresholds: infra degraded
 * 24h, silent 72h), seeded with three repeaters whose prefixes appear in no
 * other key and no path (so no relay evidence can stand in for an advert):
 *   - QUIET: own ADVERT 33h ago        → Active, "Last activity …"
 *   - LONG:  own ADVERT 100h ago       → Stale, "No activity for 4d — …"
 *   - FORGED: only an ADVERT with signatureValid false → no advert (API)
 *
 * Desktop 1440x900, cold start: health API, node page, nodes list row and map
 * marker for QUIET and LONG. Phone 375x560 (touch), cold start: Home → "I
 * know what I'm doing" → bottom nav More → Nodes → search → tap the row →
 * the node page shows the same status.
 *
 * Env: CORESCOPE_SERVER_BIN (default ./corescope-server), FIXTURE_DB
 * (default test-fixtures/e2e-fixture.db, migrated as in CI), PUBLIC_DIR
 * (default public), E2E_848_PORT (default: a free port), SCREENSHOT_DIR.
 *
 * Run: node test-issue-848-node-last-advert-e2e.js
 */
'use strict';
const fs = require('fs');
const os = require('os');
const path = require('path');
const { execFileSync } = require('child_process');
const { chromium } = require('playwright');
const { freePort, createProcessGroup, waitFor } = require('./scripts/e2e-real-stack');

const SELF = 'test-issue-848-node-last-advert-e2e';
const ROOT = __dirname;
const SERVER_BIN = path.resolve(process.env.CORESCOPE_SERVER_BIN || path.join(ROOT, 'corescope-server'));
const FIXTURE_DB = path.resolve(process.env.FIXTURE_DB || path.join(ROOT, 'test-fixtures', 'e2e-fixture.db'));
const PUBLIC_DIR = path.resolve(process.env.PUBLIC_DIR || path.join(ROOT, 'public'));
const SHOTS = process.env.SCREENSHOT_DIR || '';
const HOUR = 3600000;

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

const iso = (ms) => new Date(ms).toISOString().replace(/\.\d{3}Z$/, 'Z');

// Three free 1-byte prefixes: used by no node key and by no path hop, so the
// seeded repeaters can only be "heard" through their own adverts.
function freePrefixes(sql, n) {
  const used = new Set(sql("SELECT DISTINCT lower(substr(public_key,1,2)) FROM nodes").split('\n'));
  const paths = sql("SELECT group_concat(lower(path_json), ' ') FROM (SELECT DISTINCT path_json FROM observations WHERE path_json IS NOT NULL AND path_json != '[]')");
  const out = [];
  for (let b = 0xff; b >= 0x10 && out.length < n; b--) {
    const p = b.toString(16);
    if (!used.has(p) && paths.indexOf('"' + p) < 0) out.push(p);
  }
  if (out.length < n) throw new Error('no free key prefixes in the fixture');
  return out;
}

function seed(sql, now) {
  const [p1, p2, p3] = freePrefixes(sql, 3);
  const key = (p) => p + '848e2e'.padEnd(62, '0');
  const nodes = {
    QUIET: { pk: key(p1), name: 'E2E848 Quiet', ageH: 33, lat: 55.70, lon: 12.40, sig: 'true' },
    LONG: { pk: key(p2), name: 'E2E848 Long', ageH: 100, lat: 55.72, lon: 12.45, sig: 'true' },
    FORGED: { pk: key(p3), name: 'E2E848 Forged', ageH: 30, lat: 55.74, lon: 12.50, sig: 'false' },
  };
  const stmts = [];
  let id = 848000000;
  for (const n of Object.values(nodes)) {
    n.at = iso(now - n.ageH * HOUR);
    const dj = JSON.stringify({ type: 'ADVERT', pubKey: n.pk, name: n.name, signatureValid: n.sig === 'true' });
    const raw = '1100' + n.pk + '00000000' + '00'.repeat(64) + '02';
    stmts.push(`INSERT INTO nodes (public_key, name, role, lat, lon, last_seen, first_seen, advert_count) VALUES ('${n.pk}', '${n.name}', 'repeater', ${n.lat}, ${n.lon}, '${n.at}', '${n.at}', 1)`);
    stmts.push(`INSERT INTO transmissions (id, raw_hex, hash, first_seen, route_type, payload_type, decoded_json, from_pubkey) VALUES (${id}, '${raw}', '848e2e${String(id).slice(-6)}', '${n.at}', 1, 4, '${dj}', '${n.pk}')`);
    id++;
  }
  sql(stmts.join(';\n') + ';');
  return nodes;
}

// Status and explanation text of the node page (#/nodes/{pk}).
async function nodePageStatus(page, pk) {
  await page.waitForFunction((k) => {
    const card = document.querySelector('.node-detail-key');
    return card && card.textContent.trim() === k && [...document.querySelectorAll('td')].some((td) => td.textContent.trim() === 'Status');
  }, pk, { timeout: 20000 });
  return page.evaluate(() => {
    const cell = (label) => {
      const td = [...document.querySelectorAll('tr td:first-child')].find((t) => t.textContent.trim() === label);
      return td ? td.nextElementSibling.textContent.replace(/\s+/g, ' ').trim() : null;
    };
    return { status: cell('Status'), advert: cell('Last Heard (advert)') };
  });
}

function checkPage(where, got, want) {
  assert(got.status && got.status.indexOf(want.label) === 0, `${where}: status "${got.status}", want it to start with ${want.label}`);
  assert(want.expl.test(got.status), `${where}: status "${got.status}" does not match ${want.expl}`);
  assert(!/unknown/i.test(got.status), `${where}: still says unknown: "${got.status}"`);
  assert(got.advert && got.advert !== '—', `${where}: Last Heard (advert) is "${got.advert}"`);
}

const WANT = {
  QUIET: { label: 'Active', stale: false, expl: /Active Last activity / },
  LONG: { label: 'Stale', stale: true, expl: /Stale No activity for 4d — repeaters typically advertise every 12-24h/ },
};

async function main() {
  for (const file of [SERVER_BIN, FIXTURE_DB]) {
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

  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'corescope-848-e2e-'));
  const procs = createProcessGroup(dir);
  const db = path.join(dir, 'meshcore.db');
  const serverDir = path.join(dir, 'server');
  fs.copyFileSync(FIXTURE_DB, db);
  fs.mkdirSync(serverDir);
  fs.writeFileSync(path.join(serverDir, 'config.json'), JSON.stringify({
    packetStore: { retentionHours: 24 },
    // The fixture has 200+ fresher nodes; bulk-health (newest first) must reach ours.
    listLimits: { bulkHealthMax: 1000 },
  }));
  const sql = (s) => execFileSync('sqlite3', ['-cmd', '.timeout 5000', db, s], { encoding: 'utf8' }).trim();
  const nodes = seed(sql, Date.now());
  const port = Number(process.env.E2E_848_PORT) || await freePort();
  const base = 'http://127.0.0.1:' + port;

  try {
    const server = procs.start('server', SERVER_BIN, ['-port', String(port), '-db', db, '-public', PUBLIC_DIR, '-config-dir', serverDir]);
    await waitFor('server health', async () => {
      if (server.exitCode !== null) throw new Error('server exited: see ' + server.logFile);
      return (await fetch(base + '/api/healthz')).ok;
    }, 60000);
    console.log(`\n=== #848 (${base}, retentionHours 24) ===`);

    await step('health API: adverts outside the window come from the database, a forged one never', async () => {
      for (const k of ['QUIET', 'LONG']) {
        const s = (await (await fetch(`${base}/api/nodes/${nodes[k].pk}/health`)).json()).stats;
        assert(s.totalPackets === 0, `${k}: precondition, advert must be outside the window (totalPackets ${s.totalPackets})`);
        assert(s.lastAdvert === nodes[k].at && s.lastHeard === nodes[k].at && s.lastAdvertSource === 'db',
          `${k}: ${JSON.stringify(s)}, want lastAdvert/lastHeard ${nodes[k].at} from db`);
      }
      const f = (await (await fetch(`${base}/api/nodes/${nodes.FORGED.pk}/health`)).json()).stats;
      assert(f.lastAdvert === null && f.lastHeard === null && f.lastAdvertSource === undefined, `FORGED: ${JSON.stringify(f)}`);
      const bulk = await (await fetch(`${base}/api/nodes/bulk-health?limit=1000`)).json();
      const q = bulk.find((e) => e.public_key === nodes.QUIET.pk);
      assert(q && q.stats.lastAdvert === nodes.QUIET.at && q.stats.lastAdvertSource === 'db', `bulk-health QUIET: ${JSON.stringify(q && q.stats)}`);
    });

    // ── Desktop 1440x900, cold start ──
    const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 } });
    await ctx.addInitScript(() => {
      try { localStorage.setItem('meshcore-map-clustering', 'false'); } catch (e) { /* ignore */ }
    });
    const page = await ctx.newPage();
    page.setDefaultTimeout(20000);
    const errors = [];
    page.on('pageerror', (e) => errors.push(String(e)));

    const pageStale = {}; // status the node page showed, per node
    for (const k of ['QUIET', 'LONG']) {
      await step(`1440: node page of ${k} (${nodes[k].ageH}h) shows ${WANT[k].label}, not "unknown"`, async () => {
        await page.goto(`${base}/#/nodes/${nodes[k].pk}`, { waitUntil: 'domcontentloaded' });
        const got = await nodePageStatus(page, nodes[k].pk);
        pageStale[k] = /^Stale/.test(got.status || '');
        checkPage(k, got, WANT[k]);
        if (SHOTS) await page.screenshot({ path: path.join(SHOTS, `848-${k.toLowerCase()}-1440.png`) });
      });
    }

    await step('1440: nodes list rows agree with the node page', async () => {
      await page.goto(`${base}/#/nodes`, { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('#nodeSearch');
      await page.fill('#nodeSearch', 'E2E848');
      for (const k of ['QUIET', 'LONG']) {
        const sel = `#nodesTable tbody tr[data-key="${nodes[k].pk}"]`;
        await page.waitForSelector(sel);
        const listStale = await page.$eval(sel, (tr) => !!tr.querySelector('td.last-seen-stale'));
        assert(listStale === pageStale[k], `${k}: list says ${listStale ? 'stale' : 'active'}, node page said ${pageStale[k] ? 'stale' : 'active'}`);
        assert(listStale === WANT[k].stale, `${k}: list stale ${listStale}, want ${WANT[k].stale}`);
      }
    });

    await step('1440: map markers agree with the node page', async () => {
      await page.goto(`${base}/#/map`, { waitUntil: 'domcontentloaded' });
      await page.waitForFunction((keys) => {
        if (!window.__mc_map) return false;
        const seen = new Set();
        const scan = (l) => { if (l && l.eachLayer) l.eachLayer((m) => { if (m.eachLayer) scan(m); else if (m._nodeKey && m._icon) seen.add(m._nodeKey); }); };
        window.__mc_map.eachLayer(scan);
        return keys.every((key) => seen.has(key));
      }, [nodes.QUIET.pk, nodes.LONG.pk], { timeout: 20000 });
      const stale = await page.evaluate((keys) => {
        const out = {};
        const scan = (l) => { if (l && l.eachLayer) l.eachLayer((m) => { if (m.eachLayer) scan(m); else if (keys.includes(m._nodeKey) && m._icon) out[m._nodeKey] = m._icon.classList.contains('marker-stale'); }); };
        window.__mc_map.eachLayer(scan);
        return out;
      }, [nodes.QUIET.pk, nodes.LONG.pk]);
      for (const k of ['QUIET', 'LONG']) {
        assert(stale[nodes[k].pk] === pageStale[k], `${k}: map marker says ${stale[nodes[k].pk] ? 'stale' : 'active'}, node page said ${pageStale[k] ? 'stale' : 'active'}`);
        assert(stale[nodes[k].pk] === WANT[k].stale, `${k}: marker-stale ${stale[nodes[k].pk]}, want ${WANT[k].stale}`);
      }
      assert(errors.length === 0, 'page errors: ' + errors[0]);
    });
    await ctx.close();

    // ── Phone 375x560, touch, cold start, real taps ──
    await step('375x560: Home → More → Nodes → search → tap row → node page agrees', async () => {
      const pctx = await browser.newContext({ viewport: { width: 375, height: 560 }, hasTouch: true, isMobile: true });
      const p = await pctx.newPage();
      const perrors = [];
      p.on('pageerror', (e) => perrors.push(String(e)));
      try {
        await p.goto(`${base}/`, { waitUntil: 'domcontentloaded' });
        await p.waitForSelector('#chooseExp', { timeout: 15000 });
        await p.tap('#chooseExp');
        await p.tap('[data-bottom-nav-tab="more"]');
        await p.waitForSelector('[data-bottom-nav-more-route="nodes"]', { state: 'visible', timeout: 5000 });
        await p.tap('[data-bottom-nav-more-route="nodes"]');
        await p.waitForSelector('#nodeSearch', { state: 'visible', timeout: 15000 });
        await p.waitForTimeout(1200); // gesture tips appear ~800 ms after settle
        for (let i = 0; i < 6; i++) {
          const btn = await p.$('[data-gesture-hint-dismiss]:visible');
          if (!btn) break;
          await btn.tap().catch(() => {});
          await p.waitForTimeout(250);
        }
        await p.tap('#nodeSearch');
        await p.keyboard.type('E2E848 Long');
        const row = `#nodesTable tbody tr[data-key="${nodes.LONG.pk}"]`;
        await p.waitForSelector(row, { timeout: 10000 });
        const listStale = await p.$eval(row, (tr) => !!tr.querySelector('td.last-seen-stale'));
        assert(listStale, 'list row is not stale');
        await p.tap(`${row} td:first-child`);
        await p.waitForFunction((k) => location.hash.indexOf(k) >= 0, nodes.LONG.pk, { timeout: 8000 });
        const phoneGot = await nodePageStatus(p, nodes.LONG.pk);
        assert(/^Stale/.test(phoneGot.status || '') === listStale, `phone: list stale ${listStale}, node page "${phoneGot.status}"`);
        checkPage('LONG (phone)', phoneGot, WANT.LONG);
        const overflow = await p.evaluate(() => document.documentElement.scrollWidth - innerWidth);
        assert(overflow <= 0, `horizontal overflow ${overflow}px`);
        if (SHOTS) await p.screenshot({ path: path.join(SHOTS, '848-long-375.png') });
        assert(perrors.length === 0, 'page errors: ' + perrors[0]);
      } finally {
        await pctx.close();
      }
    });
  } finally {
    await procs.stopAll();
    await browser.close();
    fs.rmSync(dir, { recursive: true, force: true });
  }
  console.log(`\n${SELF}: ${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
}

main().catch((e) => { console.error(SELF + ': FAIL — ' + (e && e.stack || e)); process.exit(1); });
