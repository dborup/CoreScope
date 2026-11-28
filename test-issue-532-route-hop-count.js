/* #532 — route meta hop count excludes payload-derived endpoints.
 * Loads the shipping public/route-view.js in a vm context and exercises the
 * exported helpers (no copied implementation). */
'use strict';
const fs = require('fs');
const path = require('path');
const vm = require('vm');

let passed = 0, failed = 0;
function assert(cond, msg) {
  if (cond) { passed++; console.log('  ✓ ' + msg); }
  else { failed++; console.error('  ✗ ' + msg); }
}

const src = fs.readFileSync(path.join(__dirname, 'public', 'route-view.js'), 'utf8');
const sandbox = { console, setTimeout, clearTimeout, WeakSet, URLSearchParams };
sandbox.window = sandbox;
sandbox.document = { addEventListener() {}, getElementById() { return null; } };
vm.createContext(sandbox);
// route-view.js reads shared helpers (escapeHtml) from route-view-utils.js,
// which index.html loads first; do the same with the real file.
vm.runInContext(fs.readFileSync(path.join(__dirname, 'public', 'route-view-utils.js'), 'utf8'), sandbox);
vm.runInContext(src, sandbox, { filename: 'route-view.js' });
const RV = sandbox.MeshRouteView;

function relay(n) { return { name: 'R' + n, lat: 50 + n * 0.1, lon: 8 }; }
function endpoint(n) { return { name: 'E' + n, _fromPayload: true, lat: 50, lon: 8 + n }; }

console.log('\n=== #532 relayHopCount ===');
assert(RV && typeof RV.relayHopCount === 'function', 'MeshRouteView.relayHopCount exported');
if (RV && RV.relayHopCount) {
  const dm = [endpoint(1)].concat([1, 2, 3, 4, 5, 6].map(relay), [endpoint(2)]);
  assert(dm.length === 8, 'fixture: six-relay DM has 8 display rows');
  assert(RV.relayHopCount(dm) === 6, 'six-relay DM with two payload endpoints = 6');
  assert(RV.relayHopCount([endpoint(1), endpoint(2)]) === 0, 'payload-only route = 0');
  assert(RV.relayHopCount([1, 2, 3].map(relay)) === 3, 'ordinary path counts every row');
  assert(RV.relayHopCount([]) === 0, 'empty input = 0');
  assert(RV.relayHopCount(null) === 0 && RV.relayHopCount(undefined) === 0, 'null/undefined = 0');
  assert(RV.relayHopCount([relay(1), null, relay(2)]) === 2, 'null rows are not counted');
  assert(dm.length === 8 && dm[0]._fromPayload === true, 'input rows are not mutated or removed');
}

console.log('\n=== #532 routeMetaText (desktop + mobile variants) ===');
assert(RV && typeof RV.routeMetaText === 'function', 'MeshRouteView.routeMetaText exported');
if (RV && RV.routeMetaText) {
  const dm = [endpoint(1)].concat([1, 2, 3, 4, 5, 6].map(relay), [endpoint(2)]);
  assert(RV.routeMetaText(dm, 42) === '6 hops · 42 km', 'desktop meta shows 6 hops, not 8');
  assert(RV.routeMetaText(dm, 1, { type: '<i>' }) === '<b>&lt;i&gt;</b> · 6 hops · 1 km', 'type label is escaped');
  assert(RV.routeMetaText(dm, 42, { type: 'TXT MSG', multiPath: true, totalObservers: 3 })
    === '<b>TXT MSG</b> · 6 hops · 42 km · 3 obs', 'mobile summary shows 6 hops with type and obs');
  assert(RV.routeMetaText([endpoint(1), endpoint(2)], 0) === '0 hops · 0 km', 'payload-only = 0 hops');
  assert(RV.routeMetaText([], 0) === '0 hops · 0 km', 'empty = 0 hops');
  assert(RV.routeMetaText([1, 2].map(relay), 7) === '2 hops · 7 km', 'ordinary path unchanged');
}

console.log('\n' + passed + ' passed, ' + failed + ' failed');
process.exit(failed ? 1 : 0);
