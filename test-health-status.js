// Run the real shared role/health helpers; expected cut-offs are independent
// constants, not calculated from the implementation under test.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');
const source = fs.readFileSync(path.join(__dirname, 'public/roles.js'), 'utf8');
// #304: the My Mesh and Home-health decision blocks now go through the
// shared evidence helper, which reads these very thresholds. It is loaded
// into the SAME context, so the blocks below still execute the real rules.
const activitySource = fs.readFileSync(path.join(__dirname, 'public/my-mesh-activity.js'), 'utf8');
const HOUR = 3600000;
const NOW = Date.UTC(2026, 9, 24, 12);
let passed = 0;

async function load(config = {}) {
  const ctx = {
    console, Date, fetch: async () => ({ json: async () => config }),
    document: { readyState: 'complete', createElement: () => ({}), head: { appendChild() {} }, getElementById: () => null, addEventListener() {} },
  };
  ctx.window = ctx;
  vm.runInNewContext(source, ctx);
  vm.runInNewContext(activitySource, ctx);
  await ctx.MeshConfigReady;
  return ctx;
}
function check(label, fn) {
  fn(); passed++; console.log('PASS ' + label);
}

(async () => {
  const r = await load();
  for (const role of ['repeater', 'room', 'companion', 'sensor', 'unknown', null, undefined]) {
    const degraded = role === 'repeater' || role === 'room' ? 24 : 1;
    const silent = role === 'repeater' || role === 'room' ? 72 : 24;
    check(String(role) + ': both boundaries, numeric and ISO', () => {
      for (const [age, want] of [[0, 'healthy'], [degraded * HOUR - 1, 'healthy'], [degraded * HOUR, 'degraded'], [silent * HOUR - 1, 'degraded'], [silent * HOUR, 'silent'], [silent * HOUR + 1, 'silent']]) {
        for (const heard of [NOW - age, new Date(NOW - age).toISOString()]) {
          assert.equal(r.getHealthStatus(role, heard, NOW), want, `${role} age=${age} heard=${heard}`);
        }
      }
    });
  }
  check('malformed, missing and non-positive timestamps never count as healthy', () => {
    for (const value of [null, undefined, '', 'invalid', ' ', false, true, [], {}, NaN, Infinity, -Infinity, -1, 0, '1969-12-31T23:59:59Z', '1970-01-01T00:00:00Z']) {
      assert.equal(r.getHealthStatus('repeater', value, NOW), null, String(value));
    }
  });
  check('small clock skew is tolerated but far-future data stays unknown', () => {
    assert.equal(r.getHealthStatus('repeater', NOW + 30000, NOW), 'healthy');
    assert.equal(r.getHealthStatus('repeater', NOW + 30001, NOW), null);
  });
  const custom = await load({ healthThresholds: { infraDegradedMs: 2 * HOUR, infraSilentMs: 6 * HOUR, nodeDegradedMs: HOUR / 2, nodeSilentMs: 3 * HOUR } });
  check('custom threshold values flow through the real client-config promise', () => {
    assert.equal(custom.getHealthStatus('repeater', NOW - 2 * HOUR, NOW), 'degraded');
    assert.equal(custom.getHealthStatus('room', NOW - 6 * HOUR, NOW), 'silent');
    assert.equal(custom.getHealthStatus('sensor', NOW - HOUR / 2, NOW), 'degraded');
    assert.equal(custom.getHealthStatus('sensor', NOW - 3 * HOUR, NOW), 'silent');
    assert.equal(r.getHealthStatus('repeater', NOW - 6 * HOUR, NOW), 'healthy');
  });
  check('health lastHeard null is authoritative, not a directory fallback', () => {
    const node = { last_heard: NOW, last_seen: NOW, last_relayed: NOW };
    assert.equal(r.getHealthLastHeard(node, { lastHeard: null, lastAdvert: null }), null);
    assert.equal(r.getHealthLastHeard(node, { lastHeard: null }), null);
    assert.equal(r.getHealthLastHeard(node, { lastHeard: 'invalid' }), null);
    assert.equal(r.getHealthLastHeard(node, { lastHeard: 0 }), null);
    assert.equal(r.getHealthLastHeard(node, { lastHeard: NOW - HOUR }), NOW - HOUR);
  });
  check('legacy/directory fallback prefers usable last_heard then last_seen', () => {
    assert.equal(r.getHealthLastHeard({ last_heard: NOW - HOUR, last_seen: NOW }), NOW - HOUR);
    for (const invalid of [undefined, null, '', 'invalid', -1, 0, false]) {
      assert.equal(r.getHealthLastHeard({ last_heard: invalid, last_seen: NOW }), NOW);
    }
    assert.equal(r.getHealthLastHeard({ last_seen: NOW }, {}), NOW);
    assert.equal(r.getHealthLastHeard({ last_relayed: NOW }), null);
    assert.equal(r.getHealthLastHeard(null), null);
  });
  check('default clock argument works', () => {
    assert.equal(r.getHealthStatus('repeater', Date.now() - 5 * HOUR), 'healthy');
    assert.equal(r.getHealthStatus('companion', Date.now() - 5 * HOUR), 'degraded');
  });

  // Execute the actual decision blocks, not hand-copied equivalents. Browser
  // tests cover their surrounding asynchronous/DOM wiring and visible labels.
  const callArgs = ['node', 'stats', 'h', 'n', 'getHealthLastHeard', 'getHealthStatus', 'MyMeshActivity'];
  function decision(file, start, end, returned) {
    const src = fs.readFileSync(path.join(__dirname, 'public', file), 'utf8');
    const from = src.indexOf(start), to = src.indexOf(end, from);
    assert.ok(from >= 0 && to > from, 'production decision block exists in ' + file);
    const fn = new Function(...callArgs, 'timeAgo', src.slice(from, to) + '\nreturn ' + returned + ';');
    return (...values) => fn(...values, r.MyMeshActivity, () => 'synthetic age');
  }
  const blocks = [
    ['My Mesh', decision('home.js', 'const ev = MyMeshActivity.statusEvidence({ node, stats });', 'const statusDot', 'status')],
    ['Home health', decision('home.js', 'const ev = MyMeshActivity.statusEvidence({ node, stats });\n      const heardAt', 'const snr = MyMeshActivity.snr', 'status')],
    ['Favorites', decision('app.js', 'const heardAt = getHealthLastHeard(h.node, h.stats);', 'return \'<a href=', 'statusLabel')],
    ['Live', decision('live.js', 'const status = getHealthStatus(', 'let html =', 'status')],
  ];
  check('real Home, Favorites and Live decision blocks pass the node role', () => {
    for (const [name, block] of blocks) {
      for (const [role, want] of [['repeater', 'healthy'], ['room', 'healthy'], ['companion', 'degraded']]) {
        const node = { role, last_seen: Date.now() - 5 * HOUR };
        const stats = { lastHeard: node.last_seen };
        assert.equal(block(node, stats, { node, stats }, node, r.getHealthLastHeard, r.getHealthStatus), want, name + '/' + role);
      }
    }
  });
  check('real health decision blocks preserve confirmed relays and explicit null', () => {
    for (const [name, block] of blocks.filter(([name]) => name !== 'Live')) {
      const node = { role: 'repeater', last_seen: Date.now() };
      for (const [heard, want] of [[Date.now() - HOUR / 2, 'healthy'], [null, name === 'Favorites' ? 'unknown' : 'silent']]) {
        const stats = { lastHeard: heard, lastAdvert: null };
        assert.equal(block(node, stats, { node, stats }, node, r.getHealthLastHeard, r.getHealthStatus), want, name);
      }
    }
  });
  console.log(`${passed} checks passed`);
})().catch(error => { console.error(error); process.exitCode = 1; });
