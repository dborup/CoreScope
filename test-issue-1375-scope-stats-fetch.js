/**
 * #1375 — regression(analytics): Scopes tab fetches `/api/api/scope-stats`
 * (duplicate prefix) → 404 → SPA HTML → JSON.parse error.
 *
 * The `api()` helper already prepends `/api`. Other callers in
 * public/analytics.js correctly pass `/scope-stats` style relative paths;
 * the Scopes loader was the lone offender passing `/api/scope-stats`,
 * producing the doubled prefix at runtime.
 *
 * Fix: drop the leading `/api` from the Scopes-tab call so the helper
 * builds `/api/scope-stats?window=…`.
 *
 * Originally landed on the PR #915 branch (commit 2fd22cee) but that
 * branch never merged, so the bug resurfaced in subsequent rebases.
 *
 * ── Contract, revised ─────────────────────────────────────────────────
 * This file previously also asserted "exactly one `api('/scope-stats'`
 * call exists" — true when this test was written (2026-05-25, commit
 * bfebf200, the Scopes tab's own loader at analytics.js ~5140), but a
 * second, independent and fully legitimate call site was added ~2 months
 * later (2026-07-23, commit 6c9de1254, "feat: flag bridge repeaters on
 * the Foreign Traffic unscoped-relay table", analytics.js ~6148) that
 * *deliberately* reuses the Scopes tab's cached response (see that call
 * site's own doc comment) rather than duplicating the request. "Exactly
 * one call site" was never the actual regression being guarded against —
 * it was an accidental byproduct of the file's state at the time — and a
 * cardinality count of source-text occurrences can't distinguish a real
 * duplicate fetch from two call sites correctly sharing one cache entry
 * in the first place.
 *
 * The real contract has two independent halves, both asserted below:
 *   1. STRUCTURAL (Part 2): every api() call site that references
 *      /scope-stats — however many there are — uses the correct,
 *      non-doubled relative form. This must hold with NO upper bound on
 *      call-site count, or the next legitimate feature that needs this
 *      endpoint breaks the test again for no real reason.
 *   2. BEHAVIORAL (Part 3): the two actual call sites, exercised through
 *      their real, unmodified production entry points (not copies, not
 *      direct api() calls with borrowed arguments), must not cause more
 *      real network fetches than the api() helper's own TTL-cache /
 *      in-flight-dedup contract allows. This is the part a source-text
 *      count could never prove either way.
 */
'use strict';

const vm = require('vm');
const fs = require('fs');
const path = require('path');
const assert = require('assert');

process.exitCode = 1; // Flipped to 0 only after every scenario (A-F) is confirmed to have run AND all passed.

const EXPECTED_SCENARIOS = ['A', 'B', 'C', 'D', 'E', 'F'];
const ranScenarios = new Set();

let passed = 0, failed = 0;
function check(cond, msg) {
  if (cond) { passed++; console.log('  ✓ ' + msg); }
  else { failed++; console.error('  ✗ ' + msg); }
}
async function checkAsync(name, fn) {
  const label = (/^([A-F])\./.exec(name) || [])[1];
  try {
    await fn();
    passed++;
    console.log('  ✓ ' + name);
  } catch (e) {
    failed++;
    console.error('  ✗ ' + name + ': ' + e.message);
  } finally {
    if (label) ranScenarios.add(label);
  }
}

// This file's behavioral scenarios (Part 3, A-F) load the REAL,
// unmodified public/app.js via vm, so they exercise api()'s actual
// `promise.finally(() => _inflight.delete(path))` line. That line
// discards its derived promise; when the underlying request rejects
// (e.g. scenario F's induced 500), the derived promise rejects too
// with nothing observing it -- a genuine unhandled rejection. PR #53
// fixes this in public/app.js via an added `.catch(() => {})`. Without
// PR #53 present in the tree, running this file against
// `codex/fix-1375-scope-stats-test` alone is expected to CRASH under
// Node's default unhandled-rejection behavior -- a real, documented
// cross-branch dependency, not a bug in this test. No longer suppressed.

const src = fs.readFileSync(
  path.join(__dirname, 'public', 'analytics.js'), 'utf8');

console.log('\n=== #1375 Part 1: historical doubled-prefix regression guard (unchanged) ===');

// Regression guard: the buggy doubled-prefix form must never reappear.
const badRe = /api\(\s*['"]\/api\/scope-stats/g;
const badMatches = src.match(badRe) || [];
check(badMatches.length === 0,
  "ZERO `api('/api/scope-stats'` occurrences in analytics.js " +
  '(regression guard for doubled /api prefix)');

console.log('\n=== #1375 Part 2: every /scope-stats call site uses the correct relative form (no call-count assumption) ===');

// Broad, non-circular net: capture every api(...) call whose FULL quoted
// path argument mentions "scope-stats" ANYWHERE, not just calls that
// already look correct. If a regressed doubled-prefix call existed, it
// would be caught HERE (it does mention "scope-stats"), not filtered out
// before this check runs — that's what makes this non-circular, unlike
// selecting only already-correct-looking calls and then asserting they
// look correct.
const anyScopeStatsCallRe = /api\(\s*(['"])((?:(?!\1)[\s\S])*?scope-stats(?:(?!\1)[\s\S])*?)\1/g;
const scopeStatsCallPaths = [];
let m;
while ((m = anyScopeStatsCallRe.exec(src))) scopeStatsCallPaths.push(m[2]);

check(scopeStatsCallPaths.length >= 1,
  'at least one api() call site references /scope-stats (found ' +
  scopeStatsCallPaths.length + ' — zero would mean the feature was removed, not fixed)');

const wrongForm = scopeStatsCallPaths.filter(p => !p.startsWith('/scope-stats'));
check(wrongForm.length === 0,
  'every /scope-stats call site uses the correct relative form (starts with `/scope-stats`, ' +
  'not a doubled `/api/scope-stats` prefix) — offenders: ' + JSON.stringify(wrongForm));

console.log('\n=== #1375 Part 3: behavioral — the two real call sites share the api() cache correctly ===');
console.log('    (Scopes tab load(w), analytics.js ~5140, and Foreign Traffic tab load(),');
console.log('     analytics.js ~6148, driven through their real, unmodified production entry');
console.log('     points — registerPage(\'analytics\',{init}) and the exported');
console.log('     window._analyticsRenderForeignTrafficTab — not copies, not a direct api()');
console.log('     call with borrowed Scopes arguments.)');

function makeSandbox() {
  const fetchLog = [];
  let fetchBehavior = (url) => ({ ok: true, status: 200, json: async () => ({}), headers: { get: () => null } });

  // Auto-vivifying generic mock element: renderScopesTab (analytics.js)
  // is a large function that wires many DOM ids while building its
  // static frame. Rather than enumerate every one (and risk silently
  // missing one as the production function evolves), any id not
  // explicitly pre-seeded gets a fresh, memoized, fully no-op element —
  // same style as this codebase's existing `getEl`/`makeEl` id-keyed
  // mock-element factories (see test-frontend-helpers.js), generalized
  // instead of hand-listing ids, since we do not copy renderScopesTab's
  // body into this test and so cannot enumerate its DOM touches by hand.
  const elements = new Map();
  function elFor(id) {
    if (!elements.has(id)) {
      elements.set(id, {
        id, innerHTML: '', textContent: '', value: '',
        style: {}, dataset: {}, classList: { add() {}, remove() {}, toggle() {}, contains() { return false; } },
        addEventListener() {}, removeEventListener() {},
        // init()'s deep-link tab selector (analytics.js ~205) does
        // `analyticsTabs.querySelector('[data-tab="scopes"]')` to find the
        // tab button matching a `#/analytics?tab=scopes` hash, then reads
        // it back truthy to set `_currentTab`. A generic `[data-tab="x"]`
        // match (rather than special-casing the analyticsTabs id) covers
        // that real production lookup without hand-modeling the tab bar's
        // actual child markup.
        querySelector(sel) {
          const m = /\[data-tab="([^"]+)"\]/.exec(sel || '');
          return m ? elFor('tab-btn:' + m[1]) : null;
        },
        querySelectorAll() { return []; },
        appendChild() {}, closest() { return null; },
        getAttribute() { return null; }, setAttribute() {}, removeAttribute() {},
      });
    }
    return elements.get(id);
  }

  const ctx = {
    window: { addEventListener: () => {}, dispatchEvent: () => {} },
    document: {
      readyState: 'complete',
      createElement: () => elFor(Symbol()),
      head: { appendChild: () => {} },
      getElementById: (id) => elFor(id),
      addEventListener: () => {},
      querySelectorAll: () => [],
      querySelector: () => null,
    },
    console, Date, Infinity, Math, Array, Object, String, Number, JSON, RegExp,
    Error, TypeError, parseInt, parseFloat, isNaN, isFinite,
    encodeURIComponent, decodeURIComponent,
    setTimeout: (fn) => setTimeout(fn, 0), clearTimeout: () => {},
    setInterval: () => 1, clearInterval: () => {},
    performance: { now: () => Date.now() },
    localStorage: (() => { const s = {}; return { getItem: k => (k in s ? s[k] : null), setItem: (k, v) => { s[k] = String(v); }, removeItem: k => { delete s[k]; } }; })(),
    sessionStorage: (() => { const s = {}; return { getItem: k => (k in s ? s[k] : null), setItem: (k, v) => { s[k] = String(v); }, removeItem: k => { delete s[k]; } }; })(),
    location: { hash: '' },
    getHashParams: function () { return new URLSearchParams((ctx.location.hash.split('?')[1] || '')); },
    CustomEvent: class CustomEvent {},
    Map, Promise, URLSearchParams, Set,
    addEventListener: () => {},
    dispatchEvent: () => {},
    requestAnimationFrame: (cb) => setTimeout(cb, 0),
    getComputedStyle: () => ({ getPropertyValue: () => '' }),
    registerPage: (name, mod) => { ctx.__registeredPages = ctx.__registeredPages || {}; ctx.__registeredPages[name] = mod; },
    timeAgo: (iso) => (iso ? 'x ago' : '—'),
    RegionFilter: { init: () => {}, onChange: () => {}, regionQueryString: () => '' },
    AreaFilter: { init: () => {}, onChange: () => {}, areaQueryString: () => '' },
    initTabBar: () => {},
    onWS: () => {}, offWS: () => {}, connectWS: () => {},
    makeColumnsResizable: () => {},
    IATA_COORDS_GEO: {},
  };
  ctx.fetch = function (url) {
    fetchLog.push({ url, t: Date.now() });
    const r = fetchBehavior(url);
    return r instanceof Promise ? r : Promise.resolve(r);
  };
  vm.createContext(ctx);

  function loadFile(file) {
    vm.runInContext(fs.readFileSync(file, 'utf8'), ctx);
    for (const k of Object.keys(ctx.window)) ctx[k] = ctx.window[k];
  }
  loadFile('public/roles.js');
  loadFile('public/app.js'); // REAL, unmodified api() — real _apiCache/_inflight.
  // app.js declares its OWN top-level `function registerPage(name, mod)`
  // (app.js ~1110), which — as a global function declaration evaluated in
  // this vm context — overwrites the ctx.registerPage stub set above.
  // Re-stub it now, after app.js has loaded and before analytics.js calls
  // it, so analytics.js's unqualified `registerPage(...)` reference
  // (resolved against the global object at call time, same as in a real
  // browser) reaches OUR capture instead of app.js's private `pages` map.
  ctx.registerPage = (name, mod) => { ctx.__registeredPages = ctx.__registeredPages || {}; ctx.__registeredPages[name] = mod; };
  ctx.window.MC_GEO_FILTER = { latMin: 53, latMax: 59, lonMin: 6, lonMax: 15 };
  ctx.fetchAllNodes = async () => ({ nodes: [] }); // Foreign Traffic's node list; irrelevant to this contract.
  try { loadFile('public/analytics.js'); } catch (e) { for (const k of Object.keys(ctx.window)) ctx[k] = ctx.window[k]; }

  return {
    ctx,
    setFetchBehavior: (fn) => { fetchBehavior = fn; },
    scopeStatsCalls: () => fetchLog.filter(e => e.url.includes('/scope-stats')),
    // Real production entry point for the Scopes tab: registerPage's
    // captured `init`, exactly as app.js's router invokes it (this
    // reaches the real, unexposed renderScopesTab -> load(w)).
    driveScopesTab: async function (windowValue) {
      if (windowValue) ctx.sessionStorage.setItem('scopes_window', windowValue);
      ctx.location.hash = '#/analytics?tab=scopes';
      const appEl = { innerHTML: '' };
      await ctx.__registeredPages.analytics.init(appEl);
    },
    // Real, exported production entry point for the Foreign Traffic tab.
    driveForeignTrafficTab: function () {
      return ctx.window._analyticsRenderForeignTrafficTab(elFor('foreignTrafficContent'));
    },
  };
}

// Drains already-scheduled microtasks/macrotasks so unawaited
// fire-and-forget chains inside init()/renderScopesTab() (loadAnalytics()
// and load(w) are both called without awaiting their result — matching
// the real production code) get a chance to run before we inspect the
// fetch log. setImmediate yields to the event loop; this is deterministic
// draining of already-queued work, not a real-time sleep or fake timer.
function flush(times) {
  let p = Promise.resolve();
  for (let i = 0; i < (times || 25); i++) p = p.then(() => new Promise((r) => setImmediate(r)));
  return p;
}

(async () => {
  await checkAsync('A. Scopes(24h) then Foreign Traffic, within TTL: exactly one fetch to /api/scope-stats?window=24h', async () => {
    const h = makeSandbox();
    await h.driveScopesTab('24h');
    await flush();
    h.driveForeignTrafficTab();
    await flush();
    const calls = h.scopeStatsCalls();
    assert.strictEqual(calls.length, 1, 'expected exactly 1 fetch, got ' + calls.length + ': ' + JSON.stringify(calls.map(c => c.url)));
    assert.strictEqual(calls[0].url, '/api/scope-stats?window=24h');
  });

  await checkAsync('B. Reverse order — Foreign Traffic then Scopes(24h), within TTL: same expectation', async () => {
    const h = makeSandbox();
    h.driveForeignTrafficTab();
    await flush();
    await h.driveScopesTab('24h');
    await flush();
    const calls = h.scopeStatsCalls();
    assert.strictEqual(calls.length, 1, 'expected exactly 1 fetch, got ' + calls.length + ': ' + JSON.stringify(calls.map(c => c.url)));
    assert.strictEqual(calls[0].url, '/api/scope-stats?window=24h');
  });

  await checkAsync('C. Scopes on a different valid window (7d), then Foreign Traffic: exactly two fetches, distinct URLs', async () => {
    const h = makeSandbox();
    await h.driveScopesTab('7d');
    await flush();
    h.driveForeignTrafficTab();
    await flush();
    const calls = h.scopeStatsCalls();
    assert.strictEqual(calls.length, 2, 'expected exactly 2 fetches, got ' + calls.length + ': ' + JSON.stringify(calls.map(c => c.url)));
    const urls = calls.map(c => c.url).sort();
    assert.deepStrictEqual(urls, ['/api/scope-stats?window=24h', '/api/scope-stats?window=7d']);
  });

  await checkAsync('D. Slow response race: both 24h code paths start before either resolves — one fetch via in-flight dedup', async () => {
    const h = makeSandbox();
    let resolveScopeStats;
    h.setFetchBehavior((url) => {
      if (url.includes('/scope-stats')) {
        return new Promise((res) => { resolveScopeStats = () => res({ ok: true, status: 200, json: async () => ({ bridgeRepeaters: [] }), headers: { get: () => null } }); });
      }
      return { ok: true, status: 200, json: async () => ({}), headers: { get: () => null } };
    });
    const p1 = h.driveScopesTab('24h'); // fires loadAnalytics() + eventually load('24h'), unawaited internally
    const p2 = h.driveForeignTrafficTab(); // fires its own /scope-stats call near-immediately
    await flush(5); // let both chains reach their fetch() call before either resolves
    const midCalls = h.scopeStatsCalls();
    assert.strictEqual(midCalls.length, 1, 'expected exactly 1 in-flight fetch before resolving, got ' + midCalls.length + ': ' + JSON.stringify(midCalls.map(c => c.url)));
    resolveScopeStats();
    await Promise.all([p1, p2]);
    await flush();
    const calls = h.scopeStatsCalls();
    assert.strictEqual(calls.length, 1, 'expected exactly 1 total fetch, got ' + calls.length + ': ' + JSON.stringify(calls.map(c => c.url)));
  });

  await checkAsync('E. Expired TTL: Scopes(24h), cache expires, Foreign Traffic legitimately re-fetches', async () => {
    const h = makeSandbox();
    await h.driveScopesTab('24h');
    await flush();
    assert.strictEqual(h.scopeStatsCalls().length, 1, 'sanity: first call happened');
    h.ctx.invalidateApiCache('/scope-stats'); // deterministic stand-in for the real 30s TTL elapsing
    h.driveForeignTrafficTab();
    await flush();
    const calls = h.scopeStatsCalls();
    assert.strictEqual(calls.length, 2, 'expected a fresh fetch after expiry, got ' + calls.length + ' total: ' + JSON.stringify(calls.map(c => c.url)));
  });

  await checkAsync('F. First response fails: the next call site must still legitimately re-fetch (a failure caches nothing)', async () => {
    const h = makeSandbox();
    let scopeStatsAttempts = 0;
    h.setFetchBehavior((url) => {
      if (!url.includes('/scope-stats')) return { ok: true, status: 200, json: async () => ({}), headers: { get: () => null } };
      scopeStatsAttempts++;
      return scopeStatsAttempts === 1
        ? { ok: false, status: 500, json: async () => ({}), headers: { get: () => null } }
        : { ok: true, status: 200, json: async () => ({ bridgeRepeaters: [] }), headers: { get: () => null } };
    });
    await h.driveScopesTab('24h'); // load(w) catches its own error internally (see analytics.js ~5148) — does not throw out of init()
    await flush();
    h.driveForeignTrafficTab();
    await flush();
    const calls = h.scopeStatsCalls();
    assert.strictEqual(calls.length, 2, 'expected a real retry after the first failure, got ' + calls.length + ': ' + JSON.stringify(calls.map(c => c.url)));
  });

  const missing = EXPECTED_SCENARIOS.filter((l) => !ranScenarios.has(l));
  if (missing.length > 0) {
    failed++;
    console.error('\n✗ INCOMPLETE SUITE: scenario(s) ' + missing.join(', ') + ' never ran to completion (expected exactly ' +
      EXPECTED_SCENARIOS.join(', ') + ')');
  }

  console.log('\n=== Summary ===');
  console.log('  Ran: ' + Array.from(ranScenarios).sort().join(', ') + ' (' + ranScenarios.size + '/' + EXPECTED_SCENARIOS.length + ')');
  console.log('  Passed: ' + passed);
  console.log('  Failed: ' + failed);
  const complete = missing.length === 0;
  console.log('\n#1375 ' + (failed === 0 && complete ? 'PASS' : 'FAIL'));
  process.exitCode = (failed === 0 && complete) ? 0 : 1;
})();
