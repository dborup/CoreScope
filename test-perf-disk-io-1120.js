/* Tests for perf.js Disk I/O + Write Sources + SQLite sections (#1120) */
'use strict';
const vm = require('vm');
const fs = require('fs');
const assert = require('assert');

let passed = 0, failed = 0;
async function test(name, fn) {
  try { await fn(); passed++; console.log(`  ✅ ${name}`); }
  catch (e) { failed++; console.log(`  ❌ ${name}: ${e.message}`); }
}

// The warning flag perf.js renders. 30627454 (#1648 M2) replaced the ⚠️
// emoji with this sprite, so every "⚠️" check below now matches WARN; the
// negative checks were passing vacuously while they looked for ⚠️.
const WARN = '<svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-warning"/></svg>';
const reEsc = (str) => str.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
// `value` followed by the warning icon in the same card (`value WARN`).
const flagged = (html, value) => new RegExp(reEsc(value) + ' ' + reEsc(WARN)).test(html);

function makeSandbox() {
  let capturedHtml = '';
  const pages = {};
  const ctx = {
    window: { addEventListener: () => {}, apiPerf: null },
    document: {
      getElementById: (id) => {
        if (id === 'perfContent') return { set innerHTML(v) { capturedHtml = v; } };
        return null;
      },
      addEventListener: () => {},
    },
    console,
    Date, Math, Array, Object, String, Number, JSON, RegExp, Error, TypeError,
    parseInt, parseFloat, isNaN, isFinite,
    setTimeout: () => {}, clearTimeout: () => {},
    setInterval: () => 0, clearInterval: () => {},
    performance: { now: () => Date.now() },
    Map, Set, Promise,
    registerPage: (name, handler) => { pages[name] = handler; },
    _apiCache: null,
    fetch: () => Promise.resolve({ json: () => Promise.resolve({}) }),
  };
  ctx.window.document = ctx.document;
  ctx.globalThis = ctx;
  return { ctx, pages, getHtml: () => capturedHtml };
}

function loadPerf() {
  const sb = makeSandbox();
  const code = fs.readFileSync('public/perf.js', 'utf8');
  vm.runInNewContext(code, sb.ctx);
  return sb;
}

function stubFetch(sb, perfData, healthData, ioData, sqliteData, sourcesData) {
  sb.ctx.fetch = (url) => {
    if (url === '/api/perf') return Promise.resolve({ json: () => Promise.resolve(perfData) });
    if (url === '/api/health') return Promise.resolve({ json: () => Promise.resolve(healthData) });
    if (url === '/api/perf/io') return Promise.resolve({ json: () => Promise.resolve(ioData) });
    if (url === '/api/perf/sqlite') return Promise.resolve({ json: () => Promise.resolve(sqliteData) });
    if (url === '/api/perf/write-sources') return Promise.resolve({ json: () => Promise.resolve(sourcesData) });
    return Promise.resolve({ json: () => Promise.resolve({}) });
  };
}

const basePerf = {
  totalRequests: 100, avgMs: 5, uptime: 3600,
  slowQueries: [], endpoints: {}, cache: null, packetStore: null, sqlite: null
};
const goRuntime = {
  goroutines: 17, numGC: 31, pauseTotalMs: 2.1, lastPauseMs: 0.03,
  heapAllocMB: 473, heapSysMB: 1035, heapInuseMB: 663, heapIdleMB: 371, numCPU: 2
};
const goHealth = { engine: 'go', uptimeHuman: '2h', websocket: { clients: 5 } };

const ioData = {
  readBytesPerSec: 1024, writeBytesPerSec: 2048,
  syscallsRead: 10, syscallsWrite: 20
};
const sqliteData = {
  walSizeMB: 12.3, walSize: 12900000, pageCount: 4096, pageSize: 4096,
  cacheSize: 2000, cacheHitRate: 0.987
};
const sourcesData = {
  sources: { tx_inserted: 25, obs_inserted: 1787, backfill_path_json: 0, node_upserts: 329, observer_upserts: 1823, walCommits: 100 },
  sampleAt: '2026-01-01T00:00:00Z'
};

console.log('\n🧪 perf.js — Disk I/O + Write Sources (#1120)\n');

(async () => {
await test('Renders Disk I/O section', async () => {
  const sb = loadPerf();
  stubFetch(sb, { ...basePerf, goRuntime }, goHealth, ioData, sqliteData, sourcesData);
  await sb.pages.perf.init({ set innerHTML(v) {} });
  await new Promise(r => setTimeout(r, 100));
  const html = sb.getHtml();
  assert.ok(html.includes('Disk I/O'), 'should show Disk I/O heading');
  assert.ok(/2\.0\s*KB/.test(html), 'should render write rate value (2048 B/s formatted as 2.0 KB/s)');
});

await test('Renders Write Sources section with non-zero rates', async () => {
  const sb = loadPerf();
  stubFetch(sb, { ...basePerf, goRuntime }, goHealth, ioData, sqliteData, sourcesData);
  await sb.pages.perf.init({ set innerHTML(v) {} });
  await new Promise(r => setTimeout(r, 100));
  const html = sb.getHtml();
  assert.ok(html.includes('Write Sources'), 'should show Write Sources heading');
  assert.ok(html.includes('tx_inserted'), 'should list tx_inserted source');
  assert.ok(html.includes('obs_inserted'), 'should list obs_inserted source');
});

await test('Renders SQLite section with WAL + cache hit rate', async () => {
  const sb = loadPerf();
  stubFetch(sb, { ...basePerf, goRuntime }, goHealth, ioData, sqliteData, sourcesData);
  await sb.pages.perf.init({ set innerHTML(v) {} });
  await new Promise(r => setTimeout(r, 100));
  const html = sb.getHtml();
  assert.ok(/WAL/i.test(html), 'should show WAL info');
  assert.ok(/Cache Hit/i.test(html) || /cacheHitRate/i.test(html), 'should show cache hit rate');
});

// === #1120 follow-up: cancelled writes + ingestor row + threshold UX ===

await test('Renders cancelledWriteBytesPerSec for server process', async () => {
  const sb = loadPerf();
  const io = { ...ioData, cancelledWriteBytesPerSec: 4096 };
  stubFetch(sb, { ...basePerf, goRuntime }, goHealth, io, sqliteData, sourcesData);
  await sb.pages.perf.init({ set innerHTML(v) {} });
  await new Promise(r => setTimeout(r, 100));
  const html = sb.getHtml();
  assert.ok(/Cancel(led)?/i.test(html), 'should show a Cancelled write label');
  assert.ok(/4\.0\s*KB/.test(html), 'should render cancelled write rate (4096 B/s → 4.0 KB/s)');
});

await test('Renders ingestor row alongside server row in Disk I/O', async () => {
  const sb = loadPerf();
  const io = {
    ...ioData,
    cancelledWriteBytesPerSec: 0,
    ingestor: {
      readBytesPerSec: 0,
      writeBytesPerSec: 1048576,
      cancelledWriteBytesPerSec: 0,
      syscallsRead: 0,
      syscallsWrite: 0,
    },
  };
  stubFetch(sb, { ...basePerf, goRuntime }, goHealth, io, sqliteData, sourcesData);
  await sb.pages.perf.init({ set innerHTML(v) {} });
  await new Promise(r => setTimeout(r, 100));
  const html = sb.getHtml();
  assert.ok(/Ingestor/i.test(html), 'should label ingestor row');
  assert.ok(/1\.0\s*MB/.test(html), 'should render ingestor write 1 MB/s');
});

await test('WAL >100 MB fires the warning flag', async () => {
  const sb = loadPerf();
  const sql = { ...sqliteData, walSizeMB: 150, walSize: 150 * 1048576 };
  stubFetch(sb, { ...basePerf, goRuntime }, goHealth, ioData, sql, sourcesData);
  await sb.pages.perf.init({ set innerHTML(v) {} });
  await new Promise(r => setTimeout(r, 100));
  const html = sb.getHtml();
  assert.ok(flagged(html, '150.0MB'), 'expected the warning icon next to the 150MB WAL value, html=' + html.slice(html.indexOf('WAL Size') - 200, html.indexOf('WAL Size') + 200));
});

await test('WAL <100 MB does NOT fire the warning flag', async () => {
  const sb = loadPerf();
  const sql = { ...sqliteData, walSizeMB: 12.3 };
  stubFetch(sb, { ...basePerf, goRuntime }, goHealth, ioData, sql, sourcesData);
  await sb.pages.perf.init({ set innerHTML(v) {} });
  await new Promise(r => setTimeout(r, 100));
  const html = sb.getHtml();
  assert.ok(html.includes('12.3MB'), 'WAL value rendered');
  assert.ok(!flagged(html, '12.3MB'), 'expected NO warning icon next to the 12.3MB WAL value');
});

await test('Cache hit <90% fires the warning flag', async () => {
  const sb = loadPerf();
  const sql = { ...sqliteData, cacheHitRate: 0.85 };
  stubFetch(sb, { ...basePerf, goRuntime }, goHealth, ioData, sql, sourcesData);
  await sb.pages.perf.init({ set innerHTML(v) {} });
  await new Promise(r => setTimeout(r, 100));
  const html = sb.getHtml();
  assert.ok(flagged(html, '85.0%'), 'expected the warning icon next to the 85.0% cache hit value');
});

await test('Cache hit ≥90% does NOT fire the warning flag', async () => {
  const sb = loadPerf();
  const sql = { ...sqliteData, cacheHitRate: 0.987 };
  stubFetch(sb, { ...basePerf, goRuntime }, goHealth, ioData, sql, sourcesData);
  await sb.pages.perf.init({ set innerHTML(v) {} });
  await new Promise(r => setTimeout(r, 100));
  const html = sb.getHtml();
  assert.ok(html.includes('98.7%'), 'cache hit value rendered');
  assert.ok(!flagged(html, '98.7%'), 'expected NO warning icon next to the 98.7% cache hit value');
});

// === #1167 must-fix #7: threshold boundary cases ===

await test('WAL exactly 100 MB does NOT fire the warning (boundary, strict >)', async () => {
  const sb = loadPerf();
  const sql = { ...sqliteData, walSizeMB: 100 };
  stubFetch(sb, { ...basePerf, goRuntime }, goHealth, ioData, sql, sourcesData);
  await sb.pages.perf.init({ set innerHTML(v) {} });
  await new Promise(r => setTimeout(r, 100));
  const html = sb.getHtml();
  assert.ok(html.includes('100.0MB'), 'WAL value rendered');
  assert.ok(!flagged(html, '100.0MB'), 'expected NO warning icon at exactly 100 MB WAL (boundary)');
});

await test('WAL infinitesimally over 100 MB DOES fire the warning', async () => {
  const sb = loadPerf();
  const sql = { ...sqliteData, walSizeMB: 100.01 };
  stubFetch(sb, { ...basePerf, goRuntime }, goHealth, ioData, sql, sourcesData);
  await sb.pages.perf.init({ set innerHTML(v) {} });
  await new Promise(r => setTimeout(r, 100));
  const html = sb.getHtml();
  assert.ok(flagged(html, '100.0MB'), 'expected the warning icon next to the 100.0MB WAL value (just over threshold)');
});

await test('Cache hit exactly 90% does NOT fire the warning (boundary, strict <)', async () => {
  const sb = loadPerf();
  const sql = { ...sqliteData, cacheHitRate: 0.90 };
  stubFetch(sb, { ...basePerf, goRuntime }, goHealth, ioData, sql, sourcesData);
  await sb.pages.perf.init({ set innerHTML(v) {} });
  await new Promise(r => setTimeout(r, 100));
  const html = sb.getHtml();
  assert.ok(html.includes('90.0%'), 'cache hit value rendered');
  assert.ok(!flagged(html, '90.0%'), 'expected NO warning icon at exactly 90.0% cache hit (boundary)');
});

await test('Cache hit infinitesimally below 90% DOES fire the warning', async () => {
  const sb = loadPerf();
  const sql = { ...sqliteData, cacheHitRate: 0.8999 };
  stubFetch(sb, { ...basePerf, goRuntime }, goHealth, ioData, sql, sourcesData);
  await sb.pages.perf.init({ set innerHTML(v) {} });
  await new Promise(r => setTimeout(r, 100));
  const html = sb.getHtml();
  assert.ok(flagged(html, '90.0%'), 'expected the warning icon next to the 90.0% cache hit value (just under threshold)');
});

// === Write Sources anomaly flag, rendered (#1120, detector from a26a412c / #1593) ===
// detectPerfAnomalies itself is unit-tested in test-perf-anomaly.js. These
// cases check the page wiring: history kept on window across refreshes, the
// detector's verdict rendered as the warning icon on that source's row only,
// and the 30 s minimum-history guard holding back a flag on a fresh page.

async function renderWriteSources(sb, snapshots) {
  for (const snap of snapshots) {
    stubFetch(sb, { ...basePerf, goRuntime }, goHealth, ioData, sqliteData, snap);
    await sb.pages.perf.init({ set innerHTML(v) {} });
    await new Promise(r => setTimeout(r, 30));
  }
  return sb.getHtml();
}
function sourceRow(html, name) {
  const idx = html.indexOf('<code>' + name + '</code>');
  assert.ok(idx >= 0, name + ' row missing');
  return html.slice(idx, html.indexOf('</tr>', idx));
}
const at = (sec) => new Date(Date.UTC(2026, 0, 1, 0, 0, sec)).toISOString();

await test('Write Sources: a source at >10× its rolling baseline gets the warning flag, a steady one does not', async () => {
  const sb = loadPerf();
  // 40 s of baseline (backfill 1/s, tx 5/s), then 1 s where backfill jumps
  // to 100/s while tx stays at 5/s.
  const html = await renderWriteSources(sb, [
    { sources: { tx_inserted: 0, backfill_path_json: 0 }, sampleAt: at(0) },
    { sources: { tx_inserted: 200, backfill_path_json: 40 }, sampleAt: at(40) },
    { sources: { tx_inserted: 205, backfill_path_json: 140 }, sampleAt: at(41) },
  ]);
  const backfill = sourceRow(html, 'backfill_path_json');
  assert.ok(backfill.includes('<td>100.00</td><td>1.00</td>'), 'backfill rate 100/s vs baseline 1/s rendered, row=' + backfill);
  assert.ok(backfill.includes(WARN), 'expected the warning icon on the backfill row, row=' + backfill);
  const tx = sourceRow(html, 'tx_inserted');
  assert.ok(tx.includes('<td>5.00</td><td>5.00</td>'), 'tx rate and baseline 5/s rendered, row=' + tx);
  assert.ok(!tx.includes(WARN), 'expected NO warning icon on the steady tx_inserted row, row=' + tx);
});

await test('Write Sources: less than 30 s of history SUPPRESSES the warning flag', async () => {
  const sb = loadPerf();
  // Same spike, but the baseline only spans 10 s (< minHistorySec 30).
  const html = await renderWriteSources(sb, [
    { sources: { tx_inserted: 0, backfill_path_json: 0 }, sampleAt: at(0) },
    { sources: { tx_inserted: 50, backfill_path_json: 10 }, sampleAt: at(10) },
    { sources: { tx_inserted: 55, backfill_path_json: 110 }, sampleAt: at(11) },
  ]);
  const backfill = sourceRow(html, 'backfill_path_json');
  assert.ok(backfill.includes('<td>100.00</td><td>—</td>'), 'rate shown, baseline not yet established, row=' + backfill);
  assert.ok(!backfill.includes(WARN), 'expected NO warning icon with < 30 s of history, row=' + backfill);
});

console.log(`\n${passed} passed, ${failed} failed\n`);
process.exit(failed ? 1 : 0);
})();
