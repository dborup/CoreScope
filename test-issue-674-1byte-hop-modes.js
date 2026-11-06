/**
 * #674 — 1-byte path hops: Show / Mark / Hide, with Mark as the default.
 *
 * Runs the real hop-filter.js, hop-display.js, packet-helpers.js and
 * route-view-utils.js in vm sandboxes:
 *   - the mode: default, validation, storage, migration of the #1633 key,
 *     the change event, and that the old hide API keeps its meaning;
 *   - one shared "uncertain hop" rule (HopDisplay.isUnsureInferredHop) used by
 *     the packet detail and the map route;
 *   - routeHopFromInferred passing the uncertainty on to the route view;
 *   - the route view's Mark-mode notice and per-hop chip.
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

const MODE_KEY = 'meshcore-1byte-hops-mode';
const LEGACY_KEY = 'meshcore-hide-1byte-hops';

function sandbox(store, files) {
  const events = [];
  const ctx = {
    console,
    localStorage: {
      getItem: (k) => (Object.prototype.hasOwnProperty.call(store, k) ? store[k] : null),
      setItem: (k, v) => { store[k] = String(v); },
      removeItem: (k) => { delete store[k]; },
    },
    CustomEvent: function (type, init) { this.type = type; this.detail = init && init.detail; },
    dispatchEvent: (e) => { events.push(e); return true; },
    addEventListener: () => {}, removeEventListener: () => {},
    document: { addEventListener: () => {}, querySelectorAll: () => [], querySelector: () => null,
      createElement: () => ({ style: {}, classList: { add() {}, remove() {} }, setAttribute() {}, appendChild() {} }),
      body: { appendChild() {} } },
    HopResolver: { LIKELY_CONFIDENCE: 0.7 },
  };
  ctx.window = ctx;
  vm.createContext(ctx);
  for (const f of files) {
    vm.runInContext(fs.readFileSync(path.join(__dirname, 'public', f), 'utf8'), ctx, { filename: f });
  }
  ctx.__events = events;
  return ctx;
}

console.log('\n=== #674 1-byte hop modes ===');

// ── Mode ────────────────────────────────────────────────────────────────
test('default mode is "mark", and mark does not hide anything', () => {
  const store = {};
  const w = sandbox(store, ['hop-filter.js']);
  assert(w.MC_get1ByteHopsMode() === 'mark', 'got ' + w.MC_get1ByteHopsMode());
  assert(w.MC_getHide1ByteHops() === false, 'mark must not hide');
  assert(w.MC_isVisibleHop('AB') === true, '1-byte hop visible in mark mode');
  assert(w.MC_filterPathHops(['AB', 'CDEF']).length === 2, 'filter keeps everything in mark mode');
});

test('the #1633 key ("hide" on) migrates to mode "hide" and is removed', () => {
  const store = { [LEGACY_KEY]: 'true' };
  const w = sandbox(store, ['hop-filter.js']);
  assert(w.MC_get1ByteHopsMode() === 'hide', 'got ' + w.MC_get1ByteHopsMode());
  assert(store[MODE_KEY] === 'hide' && !(LEGACY_KEY in store), 'store after migration: ' + JSON.stringify(store));
  assert(w.MC_getHide1ByteHops() === true && w.MC_isVisibleHop('AB') === false, 'hide still hides');
});

test('a stray legacy value other than "true" is dropped and the default applies', () => {
  const store = { [LEGACY_KEY]: 'false' };
  const w = sandbox(store, ['hop-filter.js']);
  assert(w.MC_get1ByteHopsMode() === 'mark' && !(LEGACY_KEY in store), JSON.stringify(store));
});

test('an explicit new mode wins over a leftover legacy key', () => {
  const store = { [MODE_KEY]: 'show', [LEGACY_KEY]: 'true' };
  const w = sandbox(store, ['hop-filter.js']);
  assert(w.MC_get1ByteHopsMode() === 'show', 'got ' + w.MC_get1ByteHopsMode());
});

test('an invalid stored mode reads as the default and is removed', () => {
  for (const junk of ['bogus', 'constructor', '__proto__', 'toString', '']) {
    const store = { [MODE_KEY]: junk };
    const w = sandbox(store, ['hop-filter.js']);
    assert(w.MC_get1ByteHopsMode() === 'mark', JSON.stringify(junk) + ' read as ' + w.MC_get1ByteHopsMode());
    assert(!(MODE_KEY in store), JSON.stringify(junk) + ' was not cleaned up');
  }
});

test('set1ByteHopsMode stores show/hide, clears the key for the default, rejects junk', () => {
  const store = {};
  const w = sandbox(store, ['hop-filter.js']);
  w.MC_set1ByteHopsMode('hide');
  assert(store[MODE_KEY] === 'hide', JSON.stringify(store));
  w.MC_set1ByteHopsMode('show');
  assert(store[MODE_KEY] === 'show', JSON.stringify(store));
  w.MC_set1ByteHopsMode('mark');
  assert(!(MODE_KEY in store), 'default is stored as "no key": ' + JSON.stringify(store));
  w.MC_set1ByteHopsMode('hide');
  for (const junk of ['nonsense', 'constructor', '__proto__', 'toString']) {
    w.MC_set1ByteHopsMode(junk);
    assert(w.MC_get1ByteHopsMode() === 'hide' && store[MODE_KEY] === 'hide', 'junk ' + junk + ' changed the mode');
  }
});

test('every mode change fires mc-hide-1byte-hops-changed with { mode, value }', () => {
  const w = sandbox({}, ['hop-filter.js']);
  w.MC_set1ByteHopsMode('show');
  w.MC_set1ByteHopsMode('hide');
  const d = w.__events.filter(e => e.type === 'mc-hide-1byte-hops-changed').map(e => e.detail);
  assert(d.length === 2, 'expected 2 events, got ' + d.length);
  assert(d[0].mode === 'show' && d[0].value === false && d[1].mode === 'hide' && d[1].value === true,
    JSON.stringify(d));
});

test('the old hide API keeps its meaning: on = hide, off = show (unmarked, as before)', () => {
  const store = {};
  const w = sandbox(store, ['hop-filter.js']);
  w.MC_setHide1ByteHops(true);
  assert(w.MC_get1ByteHopsMode() === 'hide', 'on → hide');
  w.MC_setHide1ByteHops(false);
  assert(w.MC_get1ByteHopsMode() === 'show', 'off → show');
});

// ── Shared "uncertain hop" rule ─────────────────────────────────────────
const C = (n) => Array.from({ length: n }, (_, i) => ({ name: 'N' + i, pubkey: 'p' + i, confidence: 0.1 }));
test('isUnsureInferredHop: one rule for guesses and unlisted hops', () => {
  const w = sandbox({}, ['hop-filter.js', 'hop-display.js']);
  const f = w.HopDisplay.isUnsureInferredHop;
  assert(typeof f === 'function', 'HopDisplay.isUnsureInferredHop missing');
  const rows = [
    [{ name: 'A', confidence: 0.4, candidates: C(7) }, true, 'a guess among 7'],
    [{ name: 'A', confidence: 0.4, candidates: C(1) }, false, 'a guess with no alternative'],
    [{ name: 'A', confidence: 0.9, candidates: C(7) }, false, 'likely (>= LIKELY_CONFIDENCE)'],
    [{ name: 'A', confidence: 0.2, canonical: true, candidates: C(7) }, false, 'canonical (ingestor resolved)'],
    [{ unlisted: true, name: null, confidence: 0.3, candidates: C(3) }, true, 'unlisted with rejected candidates'],
    [{ unlisted: true, name: 'A', confidence: 0.3, candidates: C(3) }, false, 'unlisted but named'],
    [{ unlisted: true, name: null, confidence: 0.3, candidates: [] }, false, 'unlisted, no candidates'],
    [null, false, 'no entry'],
  ];
  for (const [entry, want, label] of rows) {
    assert(f(entry) === want, label + ': expected ' + want);
  }
});

// ── routeHopFromInferred carries the uncertainty ────────────────────────
test('routeHopFromInferred: uncertain hop carries unsure, candidateCount and top-3 alternatives', () => {
  const w = sandbox({}, ['hop-filter.js', 'hop-display.js', 'packet-helpers.js']);
  const cands = [
    { name: 'Best', pubkey: 'pb', confidence: 0.4 }, { name: 'Two', pubkey: 'p2', confidence: 0.3 },
    { name: 'Three', pubkey: 'p3', confidence: 0.2 }, { name: 'Four', pubkey: 'p4', confidence: 0.1 },
  ];
  const hop = w.routeHopFromInferred('28', { hop: '28', name: 'Best', pubkey: 'pb', lat: 1, lon: 2, located: true, confidence: 0.4, candidates: cands }, 'repeater');
  assert(hop.unsure === true && hop.candidateCount === 4, JSON.stringify(hop));
  assert(hop.alternatives.map(a => a.name).join(',') === 'Best,Two,Three', 'alternatives: ' + JSON.stringify(hop.alternatives));
  const sure = w.routeHopFromInferred('ABCD', { hop: 'ABCD', name: 'X', pubkey: 'px', lat: 1, lon: 2, located: true, confidence: 1, candidates: [{ name: 'X', pubkey: 'px', confidence: 1 }] });
  assert(sure.unsure === false && sure.candidateCount === 1, JSON.stringify(sure));
  const unlisted = w.routeHopFromInferred('99', { hop: '99', name: null, pubkey: null, located: false, unlisted: true, confidence: 0.3, candidates: cands.slice(0, 2) });
  assert(unlisted.unsure === true && unlisted.candidateCount === 2, 'unlisted placeholder: ' + JSON.stringify(unlisted));
});

// ── Route view: notice + chip ───────────────────────────────────────────
function routeUtils() {
  const w = sandbox({}, ['route-view-utils.js']);
  // app.js's real badge helper is not loaded here; a stub proves the call.
  w.renderPathHashSize = (size, text) => '<span class="path-hash-warn">' + text + '</span>';
  return w;
}
const relay = (unsure) => ({ _hopHex: '28', unsure: unsure, candidateCount: unsure ? 7 : 1 });

test('Mark notice: 1-byte badge plus "N of M hops are uncertain" (relays only)', () => {
  const U = routeUtils().MC_ROUTE_UTILS;
  const unlisted = { _hopHex: '99', unsure: true, unlisted: true, candidateCount: 2 };
  const positions = [{ isOrigin: true }, relay(true), relay(false), relay(true), unlisted, { isDest: true }];
  const html = U.buildOneByteNotice(positions, 1, 'mark');
  assert(/Sent with: 1-byte/.test(html), 'badge missing: ' + html);
  assert(/2 of 4 hops are uncertain/.test(html), 'summary counts the marked guesses, not the unlisted hop: ' + html);
});

test('Mark notice: nothing in Show or Hide mode, nothing for a certain 2-byte route', () => {
  const U = routeUtils().MC_ROUTE_UTILS;
  const positions = [relay(true), relay(false)];
  const twoByte = { _hopHex: 'ABCD', unsure: true, candidateCount: 2 };
  assert(U.buildOneByteNotice([twoByte], 2, 'mark') === '', 'an uncertain 2-byte hop is not counted');
  assert(U.buildOneByteNotice(positions, 1, 'show') === '', 'show must render nothing');
  assert(U.buildOneByteNotice(positions, 1, 'hide') === '', 'hide must render nothing');
  assert(U.buildOneByteNotice([relay(false)], 2, 'mark') === '', 'certain multi-byte route: nothing to say');
});

test('Unsure chip: "? 1 of N" with escaped candidate names in the tooltip', () => {
  const U = routeUtils().MC_ROUTE_UTILS;
  const hop = { _hopHex: '28', unsure: true, candidateCount: 7,
    alternatives: [{ name: '<b>Best</b>', confidence: 0.4 }, { name: 'Two', confidence: 0.3 }] };
  const html = U.buildUnsureChip(hop);
  assert(/mc-rt-status-unsure/.test(html) && /1 of 7/.test(html), html);
  assert(!/<b>Best<\/b>/.test(html) && /&lt;b&gt;Best&lt;\/b&gt;/.test(html), 'names must be escaped: ' + html);
  assert(/40%/.test(html) && /30%/.test(html), 'confidences missing: ' + html);
  assert(/<span class="sr-only">Uncertain hop: 7 nodes share this prefix/.test(html),
    'screen readers need the words, not just the title: ' + html);
  assert(U.buildUnsureChip({ _hopHex: '28', unsure: false, candidateCount: 7 }) === '', 'a certain hop gets no chip');
  assert(U.buildUnsureChip({ _hopHex: '28', unsure: true, unlisted: true, candidateCount: 2 }) === '',
    'an unlisted hop already has its own chip; it is not marked twice');
  assert(U.buildUnsureChip({ _hopHex: 'ABCD', unsure: true, candidateCount: 2 }) === '',
    'Mark is about 1-byte hops: an uncertain 2-byte hop stays unmarked');
  const detail = U.buildUnsureDetail(hop);
  assert(/mc-rt-detail-unsure/.test(detail) && /Likeliest: &lt;b&gt;Best/.test(detail), 'touch detail line: ' + detail);
  assert(U.buildUnsureDetail({ _hopHex: 'ABCD', unsure: true, candidateCount: 2 }) === '', 'no detail line for 2-byte');
});

test('route-view.js wires the notice, chip and dashed styling to Mark mode', () => {
  const src = fs.readFileSync(path.join(__dirname, 'public', 'route-view.js'), 'utf8');
  assert(/buildOneByteNotice\(/.test(src), 'route-view must render the notice');
  assert(/buildUnsureChip\(/.test(src), 'route-view must render the chip');
  assert(/MC_get1ByteHopsMode/.test(src), 'route-view must read the mode');
  assert(/mc-rt-marker--unsure/.test(src), 'uncertain markers need their own class');
  assert(/refreshOneByteNotice\(iso\)/.test(src) && /refreshOneByteNotice\(positions\)/.test(src),
    'the notice follows an isolated path and comes back on restore');
  assert(/function unionHopUnsure\(/.test(src), 'the "All" overlay marks uncertain hops too');
  assert(/buildUnsureDetail\(p\)/.test(src), 'the hop detail panel carries the words for touch');
  assert(/ uncertain<\/span>/.test(src), 'the collapsed mobile summary carries the count');
});

test('customizer: a three-option select replaces the checkbox, and reset clears the new key', () => {
  const src = fs.readFileSync(path.join(__dirname, 'public', 'customize-v2.js'), 'utf8');
  assert(!/type="checkbox" id="cv2-hide-1byte-hops"/.test(src), 'checkbox still present');
  assert(/<select id="cv2-hide-1byte-hops"/.test(src), 'select missing');
  for (const v of ['show', 'mark', 'hide']) {
    assert(new RegExp("opt\\('" + v + "'").test(src), 'option ' + v + ' missing');
  }
  assert(/MC_set1ByteHopsMode/.test(src), 'select must call MC_set1ByteHopsMode');
  assert(src.indexOf("'" + MODE_KEY + "'") !== -1, 'reset-all must clear ' + MODE_KEY);
});

test('map.js passes the packet\'s path-hash size to the route view', () => {
  const src = fs.readFileSync(path.join(__dirname, 'public', 'map.js'), 'utf8');
  assert(/pathHashSize:\s*window\.MC_packetHashSize/.test(src), 'pktCtx.pathHashSize missing');
});

console.log('\n#674: ' + passed + ' passed, ' + failed + ' failed');
process.exit(failed ? 1 : 0);
