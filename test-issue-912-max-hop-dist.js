/**
 * #912 M3 — the configured maxHopDist (window.MAX_HOP_DIST, degrees) must
 * reach the HopResolver "too far" sanity check. It used to be a private
 * const 1.8 that nothing could change.
 */
'use strict';
const fs = require('fs');
const vm = require('vm');

const code = fs.readFileSync(__dirname + '/public/hop-resolver.js', 'utf8');

function load(maxHopDist, setKey) {
  const win = {};
  if (setKey) win.MAX_HOP_DIST = maxHopDist;
  const sandbox = { window: win, console, Math, Object, Array, Number, Date, Map, Set, parseInt, parseFloat, encodeURIComponent, isFinite };
  vm.createContext(sandbox);
  vm.runInContext(code, sandbox);
  return sandbox.window.HopResolver;
}

let passed = 0, failed = 0;
function assert(c, m) { if (c) { passed++; console.log('  ✓ ' + m); } else { failed++; console.error('  ✗ ' + m); } }

// Three repeaters on one meridian: ~150 km (1.35°) between neighbours.
const A = { public_key: 'a1' + '00'.repeat(30), name: 'A', role: 'repeater', lat: 37.0, lon: -122.0 };
const B = { public_key: 'b2' + '00'.repeat(30), name: 'B', role: 'repeater', lat: 38.35, lon: -122.0 };
const C = { public_key: 'c3' + '00'.repeat(30), name: 'C', role: 'repeater', lat: 39.7, lon: -122.0 };

function runB(hr) {
  hr.init([A, B, C]);
  return hr.resolve(['a1', 'b2', 'c3'], null, null, null, null, null);
}

console.log('\n=== #912 M3 maxHopDist ===\n');

console.log('default limit when key absent');
{
  const hr = load(undefined, false);
  assert(typeof hr.getMaxHopDist === 'function', 'getMaxHopDist exported');
  assert(hr.getMaxHopDist() === 1.8, 'default is 1.8');
  const r = runB(hr);
  assert(r['b2'] && !r['b2'].unreliable, '150 km hop is kept with the default');
}

console.log('lowered limit marks the long hop unreliable');
{
  const hr = load(1.0, true);
  assert(hr.getMaxHopDist() === 1.0, 'configured 1.0 is read at check time');
  const r = runB(hr);
  assert(r['b2'] && r['b2'].unreliable === true, '150 km hop is unreliable at 1.0 deg');
}

console.log('limit is read at check time, not at load');
{
  const win = {};
  const sandbox = { window: win, console, Math, Object, Array, Number, Date, Map, Set, parseInt, parseFloat, encodeURIComponent, isFinite };
  vm.createContext(sandbox);
  vm.runInContext(code, sandbox);
  const hr = win.HopResolver;
  win.MAX_HOP_DIST = 1.0; // set after load, like roles.js applying /api/config/client
  const r = runB(hr);
  assert(r['b2'] && r['b2'].unreliable === true, 'late-set limit still applies');
}

console.log('junk values fall back to 1.8');
for (const junk of ['abc', '0.3', '5', NaN, 0, -1, Infinity, null, '', {}]) {
  const hr = load(junk, true);
  assert(hr.getMaxHopDist() === 1.8, 'fallback for ' + (typeof junk === 'number' ? String(junk) : JSON.stringify(junk)));
}

console.log(`\n${passed} passed, ${failed} failed`);
process.exit(failed ? 1 : 0);
