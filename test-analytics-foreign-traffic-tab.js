/**
 * DOM-rendering tests for the "Foreign Traffic" Analytics tab
 * (renderForeignTrafficTab, public/analytics.js).
 *
 * Per bot review on PR #1852 (comment 5012304813): the tab shipped with
 * zero tests, repeating the same TDD-policy violation flagged on a prior
 * round. This seeds a fetchAllNodes() stub with a mix of
 * unscoped_relay_count_24h values and asserts the rendered row order,
 * exclusion rules, and the foreignCount summary note — plus that the
 * 60s auto-refresh timer the bot also flagged as missing now exists and
 * is safely stoppable.
 */
'use strict';

const vm = require('vm');
const fs = require('fs');
const assert = require('assert');

let passed = 0, failed = 0;
async function testAsync(name, fn) {
  try {
    await fn();
    passed++;
    console.log(`  ✅ ${name}`);
  } catch (e) {
    failed++;
    console.log(`  ❌ ${name}: ${e.message}`);
  }
}

// Faithful copy of test-frontend-helpers.js's proven sandbox (same file
// this suite's makeAnalyticsSandbox() pattern is borrowed from) — rolling
// a fresh one by hand tends to miss a global roles.js/app.js touch eagerly
// at load time (window.addEventListener, fetch, etc.).
function makeSandbox() {
  const ctx = {
    window: { addEventListener: () => {}, dispatchEvent: () => {} },
    document: {
      readyState: 'complete',
      createElement: () => ({ id: '', textContent: '', innerHTML: '' }),
      head: { appendChild: () => {} },
      getElementById: () => null,
      addEventListener: () => {},
      querySelectorAll: () => [],
      querySelector: () => null,
    },
    console, Date, Infinity, Math, Array, Object, String, Number, JSON, RegExp,
    Error, TypeError, parseInt, parseFloat, isNaN, isFinite,
    encodeURIComponent, decodeURIComponent,
    setTimeout: () => {}, clearTimeout: () => {},
    fetch: () => Promise.resolve({ json: () => Promise.resolve({}) }),
    performance: { now: () => Date.now() },
    localStorage: (() => { const s = {}; return { getItem: k => s[k] || null, setItem: (k, v) => { s[k] = String(v); }, removeItem: k => { delete s[k]; } }; })(),
    location: { hash: '' },
    getHashParams: function() { return new URLSearchParams((ctx.location.hash.split('?')[1] || '')); },
    CustomEvent: class CustomEvent {},
    Map, Promise, URLSearchParams,
    addEventListener: () => {},
    dispatchEvent: () => {},
    requestAnimationFrame: (cb) => setTimeout(cb, 0),
  };
  // Spies (not just no-ops) so tests can verify a timer that was really
  // registered gets really cleared, instead of only asserting stop()
  // doesn't throw — a `function stop(){}` no-op would pass that alone.
  // Defined as closures (not object methods) so they work under the
  // sandboxed code's 'use strict', where a bare `setInterval(...)` call
  // has `this === undefined`.
  let nextIntervalId = 1;
  const liveIntervalIds = new Set();
  const clearedIntervalIds = [];
  ctx.__liveIntervalIds = liveIntervalIds;
  ctx.__clearedIntervalIds = clearedIntervalIds;
  ctx.setInterval = function () {
    const id = nextIntervalId++;
    liveIntervalIds.add(id);
    return id;
  };
  ctx.clearInterval = function (id) {
    liveIntervalIds.delete(id);
    clearedIntervalIds.push(id);
  };
  vm.createContext(ctx);
  return ctx;
}

function loadInCtx(ctx, file) {
  if (!ctx.__payloadLabelsLoaded && file !== 'public/payload-labels.js') {
    ctx.__payloadLabelsLoaded = true;
    vm.runInContext(fs.readFileSync('public/payload-labels.js', 'utf8'), ctx);
  }
  vm.runInContext(fs.readFileSync(file, 'utf8'), ctx);
  for (const k of Object.keys(ctx.window)) ctx[k] = ctx.window[k];
}

function makeAnalyticsSandbox(nodesFixture, apiStub) {
  const ctx = makeSandbox();
  ctx.getComputedStyle = () => ({ getPropertyValue: () => '' });
  ctx.registerPage = () => {};
  ctx.timeAgo = (iso) => iso ? 'x ago' : '—';
  ctx.RegionFilter = { init: () => {}, onChange: () => {}, regionQueryString: () => '' };
  ctx.onWS = () => {};
  ctx.offWS = () => {};
  ctx.connectWS = () => {};
  ctx.invalidateApiCache = () => {};
  ctx.makeColumnsResizable = () => {};
  ctx.initTabBar = () => {};
  ctx.IATA_COORDS_GEO = {};
  loadInCtx(ctx, 'public/roles.js');
  loadInCtx(ctx, 'public/app.js');
  // Override fetchAllNodes AND api (both loaded from app.js as top-level
  // function declarations, which clobber whatever ctx.api/.fetchAllNodes
  // were set to before loadInCtx ran) with stubs — fetchAllNodes hands
  // back a fixed node list instead of exercising its real pagination loop,
  // and api() is what Entry Points' per-node-detail + resolve-hops calls
  // go through.
  ctx.fetchAllNodes = async () => ({ nodes: nodesFixture });
  ctx.api = apiStub || (() => Promise.resolve({}));
  try { loadInCtx(ctx, 'public/analytics.js'); } catch (e) {
    for (const k of Object.keys(ctx.window)) ctx[k] = ctx.window[k];
  }
  return ctx;
}

function fakeEl() {
  return { innerHTML: '' };
}

(async () => {
  console.log('\n=== analytics.js: renderForeignTrafficTab ===');

  await testAsync('falls back to sorting by unscoped_relay_count_24h when airtime data is absent', async () => {
    const ctx = makeAnalyticsSandbox([
      { public_key: 'pkA', name: 'RepeaterLow', role: 'repeater', unscoped_relay_count_24h: 10, relay_count_24h: 20, foreign: false },
      { public_key: 'pkB', name: 'RepeaterHigh', role: 'repeater', unscoped_relay_count_24h: 50, relay_count_24h: 50, foreign: false },
      { public_key: 'pkC', name: 'RoomMid', role: 'room', unscoped_relay_count_24h: 25, relay_count_24h: 30, foreign: false },
    ]);
    const el = fakeEl();
    await ctx.window._analyticsRenderForeignTrafficTab(el);
    const idxHigh = el.innerHTML.indexOf('RepeaterHigh');
    const idxMid = el.innerHTML.indexOf('RoomMid');
    const idxLow = el.innerHTML.indexOf('RepeaterLow');
    assert.ok(idxHigh > -1 && idxMid > -1 && idxLow > -1, 'all three repeaters should be rendered');
    assert.ok(idxHigh < idxMid && idxMid < idxLow, 'rows should be sorted by unscoped_relay_count_24h descending');
  });

  await testAsync('sorts by unscoped_airtime_ms_24h (airtime cost) ahead of raw relay count when they disagree', async () => {
    const ctx = makeAnalyticsSandbox([
      // FewButBig has fewer unscoped relays than ManySmall, but each is a
      // much larger packet — more real channel-time cost despite the lower
      // count. Airtime-primary sort must rank it first; a count-primary
      // sort would rank ManySmall first instead.
      { public_key: 'pkFew', name: 'FewButBig', role: 'repeater', unscoped_relay_count_24h: 3, relay_count_24h: 3, unscoped_airtime_ms_24h: 9000, relay_airtime_ms_24h: 9000, foreign: false },
      { public_key: 'pkMany', name: 'ManySmall', role: 'repeater', unscoped_relay_count_24h: 100, relay_count_24h: 100, unscoped_airtime_ms_24h: 500, relay_airtime_ms_24h: 500, foreign: false },
    ]);
    const el = fakeEl();
    await ctx.window._analyticsRenderForeignTrafficTab(el);
    const idxFew = el.innerHTML.indexOf('FewButBig');
    const idxMany = el.innerHTML.indexOf('ManySmall');
    assert.ok(idxFew > -1 && idxMany > -1, 'both repeaters should be rendered');
    assert.ok(idxFew < idxMany, 'FewButBig (9000ms airtime, only 3 packets) must rank above ManySmall (500ms airtime, 100 packets) — airtime cost, not count, is the primary sort key');
  });

  await testAsync('excludes non-repeater/room roles and zero-unscoped repeaters', async () => {
    const ctx = makeAnalyticsSandbox([
      { public_key: 'pkClient', name: 'NoisyClient', role: 'client', unscoped_relay_count_24h: 999, relay_count_24h: 999, foreign: false },
      { public_key: 'pkClean', name: 'CleanRepeater', role: 'repeater', unscoped_relay_count_24h: 0, relay_count_24h: 40, foreign: false },
      { public_key: 'pkDirty', name: 'DirtyRepeater', role: 'repeater', unscoped_relay_count_24h: 5, relay_count_24h: 40, foreign: false },
    ]);
    const el = fakeEl();
    await ctx.window._analyticsRenderForeignTrafficTab(el);
    assert.ok(!el.innerHTML.includes('NoisyClient'), 'a client-role node must never appear, regardless of its unscoped count');
    assert.ok(!el.innerHTML.includes('CleanRepeater'), 'a repeater with zero unscoped relays must not appear');
    assert.ok(el.innerHTML.includes('DirtyRepeater'), 'a repeater with unscoped relays > 0 must appear');
  });

  await testAsync('shows the empty-state message when no repeater has unscoped relays', async () => {
    const ctx = makeAnalyticsSandbox([
      { public_key: 'pkClean', name: 'CleanRepeater', role: 'repeater', unscoped_relay_count_24h: 0, relay_count_24h: 40, foreign: false },
    ]);
    const el = fakeEl();
    await ctx.window._analyticsRenderForeignTrafficTab(el);
    assert.ok(el.innerHTML.includes('No repeater has relayed an unscoped flood packet'), 'empty state message should be shown');
  });

  await testAsync('foreignCount note reflects the number of foreign-flagged nodes', async () => {
    const ctx = makeAnalyticsSandbox([
      { public_key: 'pkA', name: 'RepeaterA', role: 'repeater', unscoped_relay_count_24h: 5, relay_count_24h: 5, foreign: false },
      { public_key: 'pkForeign1', name: 'ForeignNode1', role: 'client', unscoped_relay_count_24h: 0, relay_count_24h: 0, foreign: true },
      { public_key: 'pkForeign2', name: 'ForeignNode2', role: 'client', unscoped_relay_count_24h: 0, relay_count_24h: 0, foreign: true },
    ]);
    const el = fakeEl();
    await ctx.window._analyticsRenderForeignTrafficTab(el);
    assert.ok(el.innerHTML.includes('2 node(s)'), 'note should mention the count of foreign-flagged nodes (2)');
  });

  await testAsync('foreignCount note falls back to the no-foreign-yet message when none are flagged', async () => {
    const ctx = makeAnalyticsSandbox([
      { public_key: 'pkA', name: 'RepeaterA', role: 'repeater', unscoped_relay_count_24h: 5, relay_count_24h: 5, foreign: false },
    ]);
    const el = fakeEl();
    await ctx.window._analyticsRenderForeignTrafficTab(el);
    assert.ok(el.innerHTML.includes('No foreign-origin node has advertised'), 'should show the zero-foreign fallback message');
  });

  await testAsync('foreign-flagged nodes list renders every foreign node, sorted newest-heard first', async () => {
    const ctx = makeAnalyticsSandbox([
      { public_key: 'pkA', name: 'RepeaterA', role: 'repeater', unscoped_relay_count_24h: 5, relay_count_24h: 5, foreign: false },
      { public_key: 'pkOld', name: 'OldForeign', role: 'companion', lat: 44.4, lon: 26.1, first_seen: '2026-07-01T00:00:00Z', last_seen: '2026-07-01T00:00:00Z', foreign: true },
      { public_key: 'pkNew', name: 'NewForeign', role: 'client', lat: 52.4, lon: 10.8, first_seen: '2026-07-18T00:00:00Z', last_seen: '2026-07-18T12:00:00Z', foreign: true },
    ]);
    const el = fakeEl();
    await ctx.window._analyticsRenderForeignTrafficTab(el);
    assert.ok(el.innerHTML.includes('Foreign-Flagged Nodes (2)'), 'heading should show the correct count');
    const startIdx = el.innerHTML.indexOf('Foreign-Flagged Nodes');
    const endIdx = el.innerHTML.indexOf('Repeaters Relaying Unscoped Traffic');
    const foreignSectionHtml = el.innerHTML.slice(startIdx, endIdx > -1 ? endIdx : undefined);
    assert.ok(!foreignSectionHtml.includes('RepeaterA'), 'a non-foreign node must not appear in the foreign-nodes section');
    const idxNew = el.innerHTML.indexOf('NewForeign');
    const idxOld = el.innerHTML.indexOf('OldForeign');
    assert.ok(idxNew > -1 && idxOld > -1, 'both foreign nodes should be listed');
    assert.ok(idxNew < idxOld, 'more recently heard foreign node should be listed first');
  });

  await testAsync('foreign-flagged nodes list shows a placeholder when none are flagged yet', async () => {
    const ctx = makeAnalyticsSandbox([
      { public_key: 'pkA', name: 'RepeaterA', role: 'repeater', unscoped_relay_count_24h: 5, relay_count_24h: 5, foreign: false },
    ]);
    const el = fakeEl();
    await ctx.window._analyticsRenderForeignTrafficTab(el);
    assert.ok(el.innerHTML.includes('Foreign-Flagged Nodes (0)'), 'heading should show a zero count');
    assert.ok(el.innerHTML.includes('None yet'), 'placeholder message should be shown when no node is foreign-flagged');
  });

  await testAsync('Entry Points ranks unique_prefix repeaters by observation count and counts distinct foreign nodes', async () => {
    const apiStub = (path) => {
      if (path.indexOf('/nodes/pkForeign1') === 0) {
        return Promise.resolve({ recentAdverts: [{ observations: [
          { path_json: '["AAAA","1111"]' },
          { path_json: '["AAAA","2222"]' },
        ] }] });
      }
      if (path.indexOf('/nodes/pkForeign2') === 0) {
        return Promise.resolve({ recentAdverts: [{ observations: [
          { path_json: '["AAAA","3333"]' },
          { path_json: '["CCCC"]' },
        ] }] });
      }
      if (path.indexOf('/resolve-hops') === 0) {
        return Promise.resolve({ resolved: {
          AAAA: { name: 'GatewayRepeater', pubkey: 'pkGateway', confidence: 'unique_prefix' },
          CCCC: { name: 'RareRepeater', pubkey: 'pkRare', confidence: 'unique_prefix' },
        } });
      }
      return Promise.resolve({});
    };
    const ctx = makeAnalyticsSandbox([
      { public_key: 'pkForeign1', name: 'Foreign1', role: 'companion', foreign: true, last_seen: '2026-07-19T00:00:00Z' },
      { public_key: 'pkForeign2', name: 'Foreign2', role: 'client', foreign: true, last_seen: '2026-07-19T00:00:00Z' },
    ], apiStub);
    const el = fakeEl();
    await ctx.window._analyticsRenderForeignTrafficTab(el);

    const startIdx = el.innerHTML.indexOf('Entry Points');
    const endIdx = el.innerHTML.indexOf('Repeaters Relaying Unscoped Traffic');
    const section = el.innerHTML.slice(startIdx, endIdx);
    assert.ok(section.includes('GatewayRepeater'), 'GatewayRepeater (3 observations, both foreign nodes) should appear');
    assert.ok(section.includes('RareRepeater'), 'RareRepeater (1 observation, one foreign node) should appear');
    const idxGateway = section.indexOf('GatewayRepeater');
    const idxRare = section.indexOf('RareRepeater');
    assert.ok(idxGateway < idxRare, 'GatewayRepeater (3 obs) should be ranked above RareRepeater (1 obs)');
    // GatewayRepeater: 3 observations total, but only 2 distinct foreign nodes contributed them.
    const gatewayRow = section.slice(idxGateway, section.indexOf('</tr>', idxGateway));
    assert.ok(gatewayRow.includes('>3<'), 'GatewayRepeater should show 3 total observations');
    assert.ok(gatewayRow.includes('>2<'), 'GatewayRepeater should show 2 distinct foreign nodes');
  });

  await testAsync('Entry Points folds non-unique_prefix (ambiguous) resolutions into a single bucket instead of guessing a name', async () => {
    const apiStub = (path) => {
      if (path.indexOf('/nodes/pkForeign1') === 0) {
        return Promise.resolve({ recentAdverts: [{ observations: [{ path_json: '["3E"]' }] }] });
      }
      if (path.indexOf('/resolve-hops') === 0) {
        return Promise.resolve({ resolved: {
          '3E': { name: 'BestGuessRepeater', pubkey: 'pkGuess', confidence: 'gps_preference' },
        } });
      }
      return Promise.resolve({});
    };
    const ctx = makeAnalyticsSandbox([
      { public_key: 'pkForeign1', name: 'Foreign1', role: 'companion', foreign: true, last_seen: '2026-07-19T00:00:00Z' },
    ], apiStub);
    const el = fakeEl();
    await ctx.window._analyticsRenderForeignTrafficTab(el);

    const startIdx = el.innerHTML.indexOf('Entry Points');
    const endIdx = el.innerHTML.indexOf('Repeaters Relaying Unscoped Traffic');
    const section = el.innerHTML.slice(startIdx, endIdx);
    assert.ok(!section.includes('BestGuessRepeater'), 'a non-unique_prefix resolution must not be shown as a specific named repeater — #1751-style false attribution');
    assert.ok(section.includes('Ambiguous'), 'ambiguous hops should be folded into an explicit "Ambiguous" bucket');
  });

  await testAsync('Entry Points shows an empty-state message when no foreign node has any path data', async () => {
    const ctx = makeAnalyticsSandbox([
      { public_key: 'pkForeign1', name: 'Foreign1', role: 'companion', foreign: true, last_seen: '2026-07-19T00:00:00Z' },
    ]);
    const el = fakeEl();
    await ctx.window._analyticsRenderForeignTrafficTab(el);
    const startIdx = el.innerHTML.indexOf('Entry Points');
    const endIdx = el.innerHTML.indexOf('Repeaters Relaying Unscoped Traffic');
    const section = el.innerHTML.slice(startIdx, endIdx);
    assert.ok(section.includes('No traceable relay path yet'), 'empty state should be shown when no path data resolves');
  });

  await testAsync('Distance vs Hop Count computes real haversine distance / hops and sorts farthest first', async () => {
    // Copenhagen-ish node ~55.7,12.6; two observers at known offsets so the
    // exact km figures are independently checkable, not just "some number".
    const apiStub = (path) => {
      if (path.indexOf('/nodes/pkForeign1') === 0) {
        return Promise.resolve({ recentAdverts: [{ observations: [
          { observer_id: 'obsNear', path_json: '["3E"]' },
          { observer_id: 'obsFar', path_json: '["3E","1F"]' },
        ] }] });
      }
      if (path.indexOf('/observers') === 0) {
        return Promise.resolve({ observers: [
          { id: 'obsNear', name: 'NearObserver', lat: 55.7, lon: 12.6 },
          { id: 'obsFar', name: 'FarObserver', lat: 44.4, lon: 26.1 }, // ~2137km from CPH, matches RYDBOHOLM's real Bornholm-observer distance order of magnitude
        ] });
      }
      return Promise.resolve({});
    };
    const ctx = makeAnalyticsSandbox([
      { public_key: 'pkForeign1', name: 'Foreign1', role: 'companion', foreign: true, lat: 55.7, lon: 12.6, last_seen: '2026-07-19T00:00:00Z' },
    ], apiStub);
    const el = fakeEl();
    await ctx.window._analyticsRenderForeignTrafficTab(el);

    const startIdx = el.innerHTML.indexOf('Distance vs Hop Count');
    const endIdx = el.innerHTML.indexOf('Repeaters Relaying Unscoped Traffic');
    const section = el.innerHTML.slice(startIdx, endIdx);
    assert.ok(section.includes('FarObserver'), 'the far observer pair should appear');
    assert.ok(section.includes('NearObserver'), 'the near observer pair (distance ~0km) should appear');
    const idxFar = section.indexOf('FarObserver');
    const idxNear = section.indexOf('NearObserver');
    assert.ok(idxFar < idxNear, 'the far (larger-distance) pair should be sorted before the near pair');
    // Real haversine(55.7,12.6, 44.4,26.1) is ~1578km — verifies the actual
    // formula ran, not just that "some number" appeared.
    const farRow = section.slice(idxFar - 200, idxFar + 200);
    assert.ok(/>157\d</.test(farRow), 'far pair distance should be ~1578km for these coordinates, got: ' + farRow);
  });

  await testAsync('Distance vs Hop Count shows an empty-state message when nothing resolves', async () => {
    const ctx = makeAnalyticsSandbox([
      { public_key: 'pkForeign1', name: 'Foreign1', role: 'companion', foreign: true, last_seen: '2026-07-19T00:00:00Z' },
    ]);
    const el = fakeEl();
    await ctx.window._analyticsRenderForeignTrafficTab(el);
    const startIdx = el.innerHTML.indexOf('Distance vs Hop Count');
    const endIdx = el.innerHTML.indexOf('Repeaters Relaying Unscoped Traffic');
    const section = el.innerHTML.slice(startIdx, endIdx);
    assert.ok(section.includes('No distance data yet'), 'empty state should be shown when no distance/hop pair resolves');
  });

  await testAsync('rendering registers a real interval, and stop() actually clears it (not a no-op)', async () => {
    const ctx = makeAnalyticsSandbox([]);
    const stop = ctx.window._analyticsStopForeignTrafficRefresh;
    assert.strictEqual(typeof stop, 'function', '_stopForeignTrafficRefresh must be exported for testing/cleanup');

    // Calling stop() before any render must not throw (no timer registered yet).
    stop();
    assert.strictEqual(ctx.__clearedIntervalIds.length, 0, 'stop() before any render should not call clearInterval at all');

    await ctx.window._analyticsRenderForeignTrafficTab(fakeEl());
    assert.strictEqual(ctx.__liveIntervalIds.size, 1, 'render should register exactly one live interval');
    const [registeredId] = ctx.__liveIntervalIds;

    stop();
    assert.strictEqual(ctx.__liveIntervalIds.size, 0, 'stop() should leave zero live intervals');
    assert.deepStrictEqual(ctx.__clearedIntervalIds, [registeredId], 'stop() should call clearInterval with the exact id that render registered');

    // Idempotent: calling stop() again with nothing live must not clear anything a second time.
    stop();
    assert.deepStrictEqual(ctx.__clearedIntervalIds, [registeredId], 'a second stop() call must not clear anything again — the timer reference should already be null');
  });

  console.log('\n════════════════════════════════════════');
  console.log(`  Foreign Traffic tab: ${passed} passed, ${failed} failed`);
  console.log('════════════════════════════════════════');
  process.exit(failed === 0 ? 0 : 1);
})();
