/* Unit tests for node aging system */
'use strict';
const vm = require('vm');
const fs = require('fs');
const assert = require('assert');

// Load roles.js in a sandboxed context
const ctx = { window: {}, console, Date, Infinity, document: { readyState: 'complete', createElement: () => ({ id: '' }), head: { appendChild: () => {} }, getElementById: () => null, addEventListener: () => {} }, fetch: () => Promise.resolve({ json: () => Promise.resolve({}) }) };
vm.createContext(ctx);
vm.runInContext(fs.readFileSync('public/roles.js', 'utf8'), ctx);

// The IIFE assigns to window.*, but the functions reference HEALTH_THRESHOLDS as a bare global
// In the VM context, window.X doesn't create a global X, so we need to copy them
for (const k of Object.keys(ctx.window)) {
  ctx[k] = ctx.window[k];
}

const { getNodeStatus, getHealthThresholds, HEALTH_THRESHOLDS } = ctx.window;

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log(`  ✅ ${name}`); }
  catch (e) { failed++; console.log(`  ❌ ${name}: ${e.message}`); }
}

console.log('\n=== HEALTH_THRESHOLDS ===');
test('infraSilentMs = 72h (259200000)', () => assert.strictEqual(HEALTH_THRESHOLDS.infraSilentMs, 259200000));
test('nodeSilentMs = 24h (86400000)', () => assert.strictEqual(HEALTH_THRESHOLDS.nodeSilentMs, 86400000));

console.log('\n=== getHealthThresholds ===');
test('repeater uses infra thresholds', () => {
  const t = getHealthThresholds('repeater');
  assert.strictEqual(t.silentMs, 259200000);
});
test('room uses infra thresholds', () => {
  const t = getHealthThresholds('room');
  assert.strictEqual(t.silentMs, 259200000);
});
test('companion uses node thresholds', () => {
  const t = getHealthThresholds('companion');
  assert.strictEqual(t.silentMs, 86400000);
});

console.log('\n=== getNodeStatus ===');
const now = Date.now();
const h = 3600000;

test('repeater seen 1h ago → active', () => assert.strictEqual(getNodeStatus('repeater', now - 1*h), 'active'));
test('repeater seen 71h ago → active', () => assert.strictEqual(getNodeStatus('repeater', now - 71*h), 'active'));
test('repeater seen 73h ago → stale', () => assert.strictEqual(getNodeStatus('repeater', now - 73*h), 'stale'));
test('room seen 73h ago → stale (same as repeater)', () => assert.strictEqual(getNodeStatus('room', now - 73*h), 'stale'));
test('companion seen 1h ago → active', () => assert.strictEqual(getNodeStatus('companion', now - 1*h), 'active'));
test('companion seen 23h ago → active', () => assert.strictEqual(getNodeStatus('companion', now - 23*h), 'active'));
test('companion seen 25h ago → stale', () => assert.strictEqual(getNodeStatus('companion', now - 25*h), 'stale'));
test('sensor seen 25h ago → stale', () => assert.strictEqual(getNodeStatus('sensor', now - 25*h), 'stale'));
test('unknown role → uses node (24h) threshold', () => assert.strictEqual(getNodeStatus('unknown', now - 25*h), 'stale'));
test('unknown role seen 23h ago → active', () => assert.strictEqual(getNodeStatus('unknown', now - 23*h), 'active'));
test('null lastSeenMs → stale', () => assert.strictEqual(getNodeStatus('repeater', null), 'stale'));
test('undefined lastSeenMs → stale', () => assert.strictEqual(getNodeStatus('repeater', undefined), 'stale'));
test('0 lastSeenMs → stale', () => assert.strictEqual(getNodeStatus('repeater', 0), 'stale'));



// === Bug check: nodes.js row rendering must use the node-object form of
// getNodeStatus (getNodeStatus(n)) so relay-touched last_seen, _liveSeen,
// and last_heard/_lastHeard are all considered via getNodeFreshness --
// not an inlined last_heard||last_seen fallback that could silently drop
// a signal again in the future. A real, failing assertion (not just a
// console warning): reviewfix on 06cd0b73 found the prior version only
// logged and never touched `failed`, so a regression here could never
// fail the process. Anchored to the status+lastSeenClass pairing that is
// unique to the row-render block (public/nodes.js), not a bare
// getNodeStatus\(n\) regex that other callsites could also satisfy. ===
console.log('\n=== BUG CHECK ===');
const nodesJs = fs.readFileSync('public/nodes.js', 'utf8');
test('nodes.js row rendering still calls the node-object getNodeStatus(n) form', () => {
  const renderRowsMatch = nodesJs.match(
    /const status = getNodeStatus\(n\);\s*\n\s*const lastSeenClass = status === 'active'/
  );
  assert.ok(renderRowsMatch, 'row-render status/lastSeenClass block must call getNodeStatus(n)');
});

console.log(`\n=== Results: ${passed} passed, ${failed} failed ===\n`);
process.exit(failed > 0 ? 1 : 0);
