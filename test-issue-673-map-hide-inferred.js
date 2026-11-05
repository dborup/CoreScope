/**
 * #673 — the map's packet route ignored "Hide short (1-byte) path-hash hops"
 * for inferred hops.
 *
 * loadRouteFromDeepLink() hands drawPacketRoute/drawPacketRouteMulti the
 * inferred pubkeys instead of the raw prefixes, and inferredRouteHops() used
 * that pubkey as each position's _hopHex. MC_isVisibleHop only hides 1-byte
 * hex tokens, so a 64-char pubkey always survived. drawPacketRouteMulti (any
 * packet heard by more than one observer) did not apply the filter at all.
 *
 * Runs the real hop-filter.js, packet-helpers.js and map.js in a vm sandbox
 * (the #608 pattern) and exercises the helpers map.js exports through
 * window.__meshcoreMapInternals.
 */
'use strict';
const fs = require('fs');
const path = require('path');
const vm = require('vm');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.log('  ✗ ' + name + ': ' + e.message); }
}
function assert(cond, msg) { if (!cond) throw new Error(msg); }

function makeSandbox(hideOn) {
  const noop = () => {};
  const store = hideOn ? { 'meshcore-hide-1byte-hops': 'true' } : {};
  const ctx = {
    addEventListener: noop, removeEventListener: noop, dispatchEvent: noop,
    document: {
      readyState: 'complete', addEventListener: noop, removeEventListener: noop,
      getElementById: () => null, querySelector: () => null, querySelectorAll: () => [],
      createElement: () => ({ style: {}, classList: { add: noop, remove: noop, toggle: noop }, appendChild: noop, addEventListener: noop, setAttribute: noop }),
      head: { appendChild: noop }, body: { appendChild: noop },
    },
    console, location: { hash: '' },
    fetch: () => Promise.resolve({ json: () => Promise.resolve({}) }),
    setTimeout: noop, clearTimeout: noop, setInterval: () => 0, clearInterval: noop,
    localStorage: {
      getItem: (k) => (k in store ? store[k] : null),
      setItem: (k, v) => { store[k] = String(v); },
      removeItem: (k) => { delete store[k]; },
    },
    navigator: { userAgent: '' },
    getComputedStyle: () => ({ getPropertyValue: () => '' }),
  };
  // As in a browser, window IS the global object: packet-helpers.js sets
  // window.routeHopFromInferred and map.js calls it as a bare global.
  ctx.window = ctx;
  vm.createContext(ctx);
  // Same order as index.html: hop-filter.js → app.js → packet-helpers.js → map.js.
  for (const f of ['payload-labels.js', 'hop-filter.js', 'app.js', 'packet-helpers.js', 'map.js']) {
    vm.runInContext(fs.readFileSync(path.join(__dirname, 'public', f), 'utf8'), ctx, { filename: f });
  }
  return ctx;
}

const PK_28 = '281454da547128ed335e15e75b5f5106a126a90bf4cac811560832a613ea8041';
const PK_92 = '928005e30bdc1f3dc51e40b4ae0b21030748ed11685665203c075f970b3f0c09';
const PK_ABCD = 'abcd' + '0'.repeat(60);
// inferPath entries for a mixed path: two 1-byte hops and one 2-byte hop.
const inferred = [
  { hop: '28', name: 'One-byte A', pubkey: PK_28, lat: 57.7, lon: 11.9, located: true, confidence: 0.4 },
  { hop: 'ABCD', name: 'Two-byte', pubkey: PK_ABCD, lat: 56.0, lon: 10.0, located: true, confidence: 1 },
  { hop: '92', name: 'One-byte B', pubkey: PK_92, lat: 57.0, lon: 9.9, located: true, confidence: 0.9 },
];
// What loadRouteFromDeepLink passes on: the inferred pubkey for every hop.
const pubkeyPath = inferred.map(e => e.pubkey);

console.log('\n=== #673 map route honours "hide 1-byte hops" for inferred hops ===');

const off = makeSandbox(false);
const on = makeSandbox(true);
const I = on.window.__meshcoreMapInternals;
const Ioff = off.window.__meshcoreMapInternals;

test('internals export inferredRouteHops and hide1ByteRoutePositions', () => {
  assert(I && typeof I.inferredRouteHops === 'function', 'inferredRouteHops not exported');
  assert(typeof I.hide1ByteRoutePositions === 'function', 'hide1ByteRoutePositions not exported');
});

test('inferredRouteHops tags each position with the RAW prefix, not the inferred pubkey', () => {
  const hops = I.inferredRouteHops(pubkeyPath, inferred);
  assert(hops.map(h => h._hopHex).join(',') === '28,ABCD,92',
    'expected _hopHex 28,ABCD,92, got ' + hops.map(h => h._hopHex).join(','));
  assert(hops[0].pubkey === PK_28 && hops[0].name === 'One-byte A', 'the inferred node itself is kept');
});

test('hide ON: inferred 1-byte hops are dropped, the 2-byte hop and origin/destination stay', () => {
  const origin = { name: 'Sender', isOrigin: true, lat: 57.7, lon: 11.9 };
  const dest = { name: 'Recipient', lat: 55.6, lon: 12.5 };
  const positions = [origin].concat(I.inferredRouteHops(pubkeyPath, inferred), [dest]);
  const r = I.hide1ByteRoutePositions(positions);
  assert(r.positions.map(p => p.name).join(',') === 'Sender,Two-byte,Recipient',
    'got ' + r.positions.map(p => p.name).join(','));
  assert(r.hopsHiddenCount === 2, 'hopsHiddenCount should be 2, got ' + r.hopsHiddenCount);
  assert(r.positions[r.positions.length - 1].isDest === true, 'last survivor is the destination');
});

test('hide ON: a dropped last hop re-marks the last survivor as destination', () => {
  const positions = I.inferredRouteHops(pubkeyPath, inferred);
  positions[2].isDest = true;
  const r = I.hide1ByteRoutePositions(positions);
  assert(r.positions.length === 1 && r.positions[0].name === 'Two-byte', 'only the 2-byte hop survives');
  assert(r.positions[0].isDest === true, 'the survivor becomes the destination');
});

test('hide OFF: positions are returned unchanged and nothing is counted', () => {
  const positions = Ioff.inferredRouteHops(pubkeyPath, inferred);
  const r = Ioff.hide1ByteRoutePositions(positions);
  assert(r.positions.length === 3 && r.hopsHiddenCount === 0, 'expected 3 positions and 0 hidden');
});

test('hide ON: the helper never mutates the caller\'s array', () => {
  const positions = I.inferredRouteHops(pubkeyPath, inferred);
  I.hide1ByteRoutePositions(positions);
  assert(positions.length === 3, 'input array was mutated');
});

test('map.js: drawPacketRoute and drawPacketRouteMulti both filter through the shared helper', () => {
  const src = fs.readFileSync(path.join(__dirname, 'public', 'map.js'), 'utf8');
  const body = (name) => {
    const start = src.indexOf('async function ' + name + '(');
    assert(start >= 0, name + ' not found');
    const next = src.indexOf('\n  async function ', start + 10);
    return src.slice(start, next < 0 ? undefined : next);
  };
  for (const fn of ['drawPacketRoute', 'drawPacketRouteMulti']) {
    const b = body(fn);
    assert(/hide1ByteRoutePositions\(/.test(b), fn + ' must call hide1ByteRoutePositions');
    assert(/hopsHiddenCount:\s*\w+\.hopsHiddenCount|hopsHiddenCount:\s*hopsHiddenCount/.test(b),
      fn + ' must pass hopsHiddenCount to the renderer');
  }
});

console.log('\n#673: ' + passed + ' passed, ' + failed + ' failed');
process.exit(failed ? 1 : 0);
