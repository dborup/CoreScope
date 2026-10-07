/* Tests for perf.js Go runtime vs Node event loop rendering (fixes #153) */
'use strict';
const vm = require('vm');
const fs = require('fs');
const assert = require('assert');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log(`  ✅ ${name}`); }
  catch (e) { failed++; console.log(`  ❌ ${name}: ${e.message}`); }
}

// Minimal sandbox to run perf.js in a browser-like context
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

// Load perf.js into sandbox
function loadPerf() {
  const sb = makeSandbox();
  const code = fs.readFileSync('public/perf.js', 'utf8');
  vm.runInNewContext(code, sb.ctx);
  return sb;
}

// Stub fetch to return controlled data
function stubFetch(sb, perfData, healthData) {
  sb.ctx.fetch = (url) => {
    if (url === '/api/perf') return Promise.resolve({ json: () => Promise.resolve(perfData) });
    if (url === '/api/health') return Promise.resolve({ json: () => Promise.resolve(healthData) });
    return Promise.resolve({ json: () => Promise.resolve({}) });
  };
}

const basePerf = {
  totalRequests: 100, avgMs: 5, uptime: 3600,
  slowQueries: [], endpoints: {}, cache: null, packetStore: null, sqlite: null
};

const nodeHealth = {
  engine: 'node',
  uptimeHuman: '1h',
  memory: { heapUsed: 100, heapTotal: 200, rss: 250 },
  eventLoop: { p95Ms: 10, maxLagMs: 20, currentLagMs: 1 },
  websocket: { clients: 3 }
};

const goRuntime = {
  goroutines: 17, numGC: 31, pauseTotalMs: 2.1, lastPauseMs: 0.03,
  heapAllocMB: 473, heapSysMB: 1035, heapInuseMB: 663, heapIdleMB: 371, numCPU: 2
};

const goHealth = {
  engine: 'go',
  uptimeHuman: '2h',
  websocket: { clients: 5 }
};

console.log('\n🧪 perf.js — Go Runtime vs Node Event Loop\n');

// --- Node engine tests ---

test('Node engine shows Event Loop labels', async () => {
  const sb = loadPerf();
  stubFetch(sb, basePerf, nodeHealth);
  await sb.pages.perf.init({ set innerHTML(v) {} });
  // Wait for async refresh
  await new Promise(r => setTimeout(r, 50));
  const html = sb.getHtml();
  assert.ok(html.includes('Event Loop p95'), 'should show Event Loop p95');
  assert.ok(html.includes('EL Max Lag'), 'should show EL Max Lag');
  assert.ok(html.includes('EL Current'), 'should show EL Current');
  assert.ok(html.includes('System Health'), 'should show System Health heading');
});

test('Node engine does NOT show Go Runtime heading', async () => {
  const sb = loadPerf();
  stubFetch(sb, basePerf, nodeHealth);
  await sb.pages.perf.init({ set innerHTML(v) {} });
  await new Promise(r => setTimeout(r, 50));
  const html = sb.getHtml();
  assert.ok(!html.includes('Go Runtime'), 'should not show Go Runtime');
  assert.ok(!html.includes('Goroutines'), 'should not show Goroutines');
});

test('Node engine shows memory stats', async () => {
  const sb = loadPerf();
  stubFetch(sb, basePerf, nodeHealth);
  await sb.pages.perf.init({ set innerHTML(v) {} });
  await new Promise(r => setTimeout(r, 50));
  const html = sb.getHtml();
  assert.ok(html.includes('Heap Used'), 'should show Heap Used');
  assert.ok(html.includes('RSS'), 'should show RSS');
});

// --- Go engine tests ---

test('Go engine shows Go Runtime heading', async () => {
  const sb = loadPerf();
  stubFetch(sb, { ...basePerf, goRuntime }, goHealth);
  await sb.pages.perf.init({ set innerHTML(v) {} });
  await new Promise(r => setTimeout(r, 50));
  const html = sb.getHtml();
  assert.ok(html.includes('Go Runtime'), 'should show Go Runtime heading');
});

test('Go engine shows all goRuntime fields', async () => {
  const sb = loadPerf();
  stubFetch(sb, { ...basePerf, goRuntime }, goHealth);
  await sb.pages.perf.init({ set innerHTML(v) {} });
  await new Promise(r => setTimeout(r, 50));
  const html = sb.getHtml();
  assert.ok(html.includes('Goroutines'), 'should show Goroutines');
  assert.ok(html.includes('GC Collections'), 'should show GC Collections');
  assert.ok(html.includes('GC Pause Total'), 'should show GC Pause Total');
  assert.ok(html.includes('Last GC Pause'), 'should show Last GC Pause');
  assert.ok(html.includes('Heap Alloc'), 'should show Heap Alloc');
  assert.ok(html.includes('Heap Sys'), 'should show Heap Sys');
  assert.ok(html.includes('Heap Inuse'), 'should show Heap Inuse');
  assert.ok(html.includes('Heap Idle'), 'should show Heap Idle');
  assert.ok(html.includes('CPUs'), 'should show CPUs');
});

test('Go engine shows goRuntime values', async () => {
  const sb = loadPerf();
  stubFetch(sb, { ...basePerf, goRuntime }, goHealth);
  await sb.pages.perf.init({ set innerHTML(v) {} });
  await new Promise(r => setTimeout(r, 50));
  const html = sb.getHtml();
  assert.ok(html.includes('17'), 'goroutines value');
  assert.ok(html.includes('31'), 'numGC value');
  assert.ok(html.includes('2.1ms'), 'pauseTotalMs value');
  assert.ok(html.includes('0.03ms'), 'lastPauseMs value');
  assert.ok(html.includes('473MB'), 'heapAllocMB value');
  assert.ok(html.includes('1035MB'), 'heapSysMB value');
  assert.ok(html.includes('663MB'), 'heapInuseMB value');
  assert.ok(html.includes('371MB'), 'heapIdleMB value');
});

test('Go engine does NOT show Event Loop labels', async () => {
  const sb = loadPerf();
  stubFetch(sb, { ...basePerf, goRuntime }, goHealth);
  await sb.pages.perf.init({ set innerHTML(v) {} });
  await new Promise(r => setTimeout(r, 50));
  const html = sb.getHtml();
  assert.ok(!html.includes('Event Loop'), 'should not show Event Loop');
  assert.ok(!html.includes('EL Max Lag'), 'should not show EL Max Lag');
  assert.ok(!html.includes('EL Current'), 'should not show EL Current');
});

test('Go engine still shows WS Clients', async () => {
  const sb = loadPerf();
  stubFetch(sb, { ...basePerf, goRuntime }, goHealth);
  await sb.pages.perf.init({ set innerHTML(v) {} });
  await new Promise(r => setTimeout(r, 50));
  const html = sb.getHtml();
  assert.ok(html.includes('WS Clients'), 'should show WS Clients');
  assert.ok(html.includes('>5<'), 'should show 5 WS clients');
});

// --- GC color threshold tests ---

test('Go GC pause green when lastPauseMs <= 1', async () => {
  const sb = loadPerf();
  const gr = { ...goRuntime, lastPauseMs: 0.5 };
  stubFetch(sb, { ...basePerf, goRuntime: gr }, goHealth);
  await sb.pages.perf.init({ set innerHTML(v) {} });
  await new Promise(r => setTimeout(r, 50));
  const html = sb.getHtml();
  assert.ok(html.includes('var(--status-green)'), 'should use green for low GC pause');
});

test('Go GC pause yellow when lastPauseMs > 1 and <= 5', async () => {
  const sb = loadPerf();
  const gr = { ...goRuntime, lastPauseMs: 3 };
  stubFetch(sb, { ...basePerf, goRuntime: gr }, goHealth);
  await sb.pages.perf.init({ set innerHTML(v) {} });
  await new Promise(r => setTimeout(r, 50));
  const html = sb.getHtml();
  assert.ok(html.includes('var(--status-yellow)'), 'should use yellow for moderate GC pause');
});

test('Go GC pause red when lastPauseMs > 5', async () => {
  const sb = loadPerf();
  const gr = { ...goRuntime, lastPauseMs: 10 };
  stubFetch(sb, { ...basePerf, goRuntime: gr }, goHealth);
  await sb.pages.perf.init({ set innerHTML(v) {} });
  await new Promise(r => setTimeout(r, 50));
  const html = sb.getHtml();
  assert.ok(html.includes('var(--status-red)'), 'should use red for high GC pause');
});

// --- Fallback: engine=go but no goRuntime falls back to Node UI ---

test('engine=go but missing goRuntime falls back to Node UI', async () => {
  const sb = loadPerf();
  const goHealthWithMemory = {
    ...goHealth,
    memory: { heapUsed: 50, heapTotal: 100, rss: 80 },
    eventLoop: { p95Ms: 5, maxLagMs: 10, currentLagMs: 1 }
  };
  stubFetch(sb, basePerf, goHealthWithMemory);
  await sb.pages.perf.init({ set innerHTML(v) {} });
  await new Promise(r => setTimeout(r, 50));
  const html = sb.getHtml();
  assert.ok(html.includes('Event Loop p95'), 'should fall back to Event Loop');
  assert.ok(!html.includes('Go Runtime'), 'should not show Go Runtime');
});

// --- Missing engine field ---

test('Missing engine field shows Node UI', async () => {
  const sb = loadPerf();
  const healthNoEngine = {
    uptimeHuman: '1h',
    memory: { heapUsed: 100, heapTotal: 200, rss: 250 },
    eventLoop: { p95Ms: 10, maxLagMs: 20, currentLagMs: 1 },
    websocket: { clients: 2 }
  };
  stubFetch(sb, basePerf, healthNoEngine);
  await sb.pages.perf.init({ set innerHTML(v) {} });
  await new Promise(r => setTimeout(r, 50));
  const html = sb.getHtml();
  assert.ok(html.includes('Event Loop p95'), 'should show Event Loop');
  assert.ok(!html.includes('Go Runtime'), 'should not show Go Runtime');
});

// --- Version card ---

test('Version card shows the version string exactly as reported', async () => {
  const sb = loadPerf();
  const health = { ...goHealth, version: '3.8.2', commit: 'unknown' };
  stubFetch(sb, basePerf, health);
  await sb.pages.perf.init({ set innerHTML(v) {} });
  await new Promise(r => setTimeout(r, 50));
  const html = sb.getHtml();
  assert.ok(html.includes('>3.8.2<'), 'should show version');
  assert.ok(!html.includes('v3.8.2'), 'should not add a "v" prefix');
  assert.ok(html.includes('Version'), 'should show Version label');
});

test('Version card shows commit hash when version is unknown', async () => {
  const sb = loadPerf();
  const health = { ...goHealth, version: 'unknown', commit: 'abc1234def' };
  stubFetch(sb, basePerf, health);
  await sb.pages.perf.init({ set innerHTML(v) {} });
  await new Promise(r => setTimeout(r, 50));
  const html = sb.getHtml();
  assert.ok(html.includes('>abc1234d<'), 'should show 8-char commit hash');
  assert.ok(!html.includes('unknown'), 'should not render literal "unknown"');
});

test('Version card shows both version and commit when both are known', async () => {
  const sb = loadPerf();
  const health = { ...goHealth, version: 'v3.8.2', commit: 'deadbeef1234' };
  stubFetch(sb, basePerf, health);
  await sb.pages.perf.init({ set innerHTML(v) {} });
  await new Promise(r => setTimeout(r, 50));
  const html = sb.getHtml();
  assert.ok(html.includes('v3.8.2'), 'should show version');
  assert.ok(html.includes('>deadbeef<'), 'should show 8-char commit');
});

test('Version card is absent when health has no version or commit', async () => {
  const sb = loadPerf();
  const health = { ...goHealth }; // no version/commit fields
  stubFetch(sb, basePerf, health);
  await sb.pages.perf.init({ set innerHTML(v) {} });
  await new Promise(r => setTimeout(r, 50));
  const html = sb.getHtml();
  assert.ok(!html.includes('perf-label">Version<'), 'should not show Version card when fields absent');
});

test('Version card is absent when both version and commit are "unknown"', async () => {
  const sb = loadPerf();
  const health = { ...goHealth, version: 'unknown', commit: 'unknown' };
  stubFetch(sb, basePerf, health);
  await sb.pages.perf.init({ set innerHTML(v) {} });
  await new Promise(r => setTimeout(r, 50));
  const html = sb.getHtml();
  assert.ok(!html.includes('perf-label">Version<'), 'should not show Version card when all values are unknown');
});

// --- renderVersionCard unit tests (direct helper) ---

const SOURCE_REPO = 'https://github.com/dborup/CoreScope';
const MASTER_SHA = 'ba6f170e6d0216c6119209e822a17681fecf16f1';
const LOCAL_INTEGRATION_SHA = 'dc1297db36e0e306044e16161695f4a494ea11f8';

// The only anchor in the card is the fixed source-code link; version and
// commit are never turned into release-tag or commit links.
function assertOnlySourceLink(card) {
  const anchors = card.match(/<a\b[^>]*>/g) || [];
  assert.deepStrictEqual(anchors, ['<a href="' + SOURCE_REPO + '" target="_blank" rel="noopener">'],
    'only the source-code link should be an anchor');
  assert.ok(card.includes('>Source Code</a>'), 'should label the link "Source Code"');
  assert.ok(!card.includes('releases/tag'), 'should not link a release tag');
  assert.ok(!card.includes('/commit/'), 'should not link a commit');
  assert.ok(!/kpa-clawbot/i.test(card), 'should not point at the upstream repo');
}

test('renderVersionCard: release version and commit render as text', () => {
  const sb = loadPerf();
  const card = sb.ctx.renderVersionCard({ version: 'v3.9.2', commit: MASTER_SHA });
  assert.ok(card.includes('<div class="perf-num perf-num--small">v3.9.2 · <span title="' + MASTER_SHA + '">ba6f170e</span></div>'), card);
  assertOnlySourceLink(card);
});

test('renderVersionCard: keeps the version string exactly (no "v" prefix)', () => {
  const sb = loadPerf();
  const card = sb.ctx.renderVersionCard({ version: '3.8.2', commit: 'unknown' });
  assert.ok(card.includes('<div class="perf-num perf-num--small">3.8.2</div>'), card);
  assert.ok(!card.includes('v3.8.2'), 'should not add a "v" prefix');
  assertOnlySourceLink(card);
});

test('renderVersionCard: git-describe version renders as text', () => {
  const sb = loadPerf();
  const card = sb.ctx.renderVersionCard({ version: 'v0.0.1-37-gba6f170e', commit: MASTER_SHA });
  assert.ok(card.includes('>v0.0.1-37-gba6f170e · <span title="' + MASTER_SHA + '">ba6f170e</span><'), card);
  assertOnlySourceLink(card);
});

test('renderVersionCard: local integration SHA is kept, not replaced', () => {
  const sb = loadPerf();
  const card = sb.ctx.renderVersionCard({ version: 'v0.0.1-36-gdc1297db', commit: LOCAL_INTEGRATION_SHA });
  assert.ok(card.includes('>v0.0.1-36-gdc1297db · <span title="' + LOCAL_INTEGRATION_SHA + '">dc1297db</span><'), card);
  assert.ok(!card.includes('ba6f170e'), 'should not substitute another commit');
  assertOnlySourceLink(card);
});

test('renderVersionCard: commit shorter than 8 chars is shown whole', () => {
  const sb = loadPerf();
  const card = sb.ctx.renderVersionCard({ version: 'unknown', commit: 'abc1234' });
  assert.ok(card.includes('<div class="perf-num perf-num--small"><span title="abc1234">abc1234</span></div>'), card);
  assert.ok(!card.includes('unknown'), 'should not render literal "unknown"');
  assertOnlySourceLink(card);
});

test('renderVersionCard: handles missing and empty values', () => {
  const sb = loadPerf();
  const render = sb.ctx.renderVersionCard;
  assert.strictEqual(render({}), '');
  assert.strictEqual(render({ version: '', commit: '' }), '');
  assert.strictEqual(render({ version: 'unknown' }), '');
  assert.strictEqual(render({ commit: 'unknown' }), '');
  const versionOnly = render({ version: 'v1.0.0' });
  assert.ok(versionOnly.includes('<div class="perf-num perf-num--small">v1.0.0</div>'), versionOnly);
  assert.ok(!versionOnly.includes('title='), 'should not add a commit title without a commit');
  assertOnlySourceLink(versionOnly);
});

test('renderVersionCard: escapes HTML special characters', () => {
  const sb = loadPerf();
  const card = sb.ctx.renderVersionCard({ version: '<img src=x onerror="alert(1)">&\'', commit: '"><b>&\'x<i>' });
  assert.ok(card.includes('>&lt;img src=x onerror=&quot;alert(1)&quot;&gt;&amp;&#39; · '), card);
  // Truncated to 8 characters before escaping, so no entity is cut in half.
  assert.ok(card.includes('<span title="&quot;&gt;&lt;b&gt;&amp;&#39;x&lt;i&gt;">&quot;&gt;&lt;b&gt;&amp;&#39;x</span>'), card);
  assert.ok(!/<(img|b|i)\b/.test(card), 'should not emit injected tags');
  assertOnlySourceLink(card);
});

test('renderVersionCard: keeps the "Version" label and makes no network calls', () => {
  const sb = loadPerf();
  let calls = 0;
  sb.ctx.fetch = () => { calls++; return Promise.resolve({ json: () => Promise.resolve({}) }); };
  const card = sb.ctx.renderVersionCard({ version: 'v0.0.1-36-gdc1297db', commit: LOCAL_INTEGRATION_SHA });
  assert.ok(card.includes('<div class="perf-label">Version</div>'), 'e2e locates the card by this exact label');
  assert.strictEqual(calls, 0, 'rendering the card should not fetch');
});

test('perf.js: no upstream repo or derived release/commit links in source', () => {
  const src = fs.readFileSync('public/perf.js', 'utf8');
  assert.ok(!/kpa-clawbot/i.test(src), 'should not reference the upstream repo');
  assert.ok(!src.includes('releases/tag'), 'should not build release-tag links');
  assert.ok(!src.includes('/commit/'), 'should not build commit links');
  assert.ok(src.includes("'" + SOURCE_REPO + "'"), 'should use the fork repo URL');
});

test('renderVersionCard: returns empty string for null health', () => {
  const sb = loadPerf();
  assert.strictEqual(sb.ctx.renderVersionCard(null), '');
});

test('renderVersionCard: returns empty string when both fields are unknown', () => {
  const sb = loadPerf();
  assert.strictEqual(sb.ctx.renderVersionCard({ version: 'unknown', commit: 'unknown' }), '');
});

test('renderVersionCard: uses perf-num--small class (no inline style)', () => {
  const sb = loadPerf();
  const card = sb.ctx.renderVersionCard({ version: '1.0.0', commit: 'abc1234' });
  assert.ok(card.includes('perf-num--small'), 'should use perf-num--small class');
  assert.ok(!card.includes('font-size'), 'should not use inline font-size style');
});

console.log(`\n${passed} passed, ${failed} failed\n`);
process.exit(failed ? 1 : 0);
