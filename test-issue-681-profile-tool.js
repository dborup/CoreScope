/**
 * Unit tests (#681 M4): public/terrain-profile.js — the Path profile panel
 * and chart for the Map page.
 *
 * The module is loaded with vm.createContext in a browser-shaped sandbox,
 * the same way test-frontend-helpers.js exercises the real frontend files;
 * nothing here is a copy of production logic.
 *
 * Covered:
 *   1. deep-link parse/serialise round trip, including hostile input;
 *   2. antenna-height clamping;
 *   3. verdict → chip mapping, including unknown on a partial profile;
 *   4. SVG geometry: a known profile gives known coordinates;
 *   5. localStorage that is absent or throws;
 *   6. XSS escaping of node names in the chart <title>;
 *   7. M6: the customizer's terrain defaults — antenna heights per role and
 *      the default elevation model — and where they sit in the precedence;
 *   8. review round 2: the coverage gate on the chip (F5), intrusion bands
 *      that cannot contradict it (F8), the worst-sample marker (F3), the
 *      stale-response guard (F4), HTML chart labels (F7) and the plain
 *      decimal grammar for k and freq (F9).
 *
 * Usage: node test-issue-681-profile-tool.js
 */
'use strict';
const vm = require('vm');
const fs = require('fs');
const assert = require('assert');

let passed = 0, failed = 0;

/* The module runs in its own vm realm, so arrays and objects it returns do
 * not share this realm's prototypes and deepStrictEqual refuses them. Compare
 * the values, not the realm they came from. */
function eq(actual, expected, msg) {
  assert.deepStrictEqual(JSON.parse(JSON.stringify(actual === undefined ? null : actual)), expected, msg);
}
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}
// The request path is asynchronous, so those cases are queued here and
// drained after the synchronous ones, just before the summary.
const asyncTests = [];
function atest(name, fn) { asyncTests.push([name, fn]); }
const sleep = (ms) => new Promise(r => setTimeout(r, ms));

// ── Sandbox ────────────────────────────────────────────────────────────────

function makeStorage(impl) {
  if (impl === 'absent') return undefined;
  if (impl === 'throws') {
    return {
      getItem() { throw new Error('SecurityError'); },
      setItem() { throw new Error('QuotaExceededError'); },
      removeItem() { throw new Error('SecurityError'); },
    };
  }
  const store = {};
  return {
    getItem: k => (Object.prototype.hasOwnProperty.call(store, k) ? store[k] : null),
    setItem: (k, v) => { store[k] = String(v); },
    removeItem: k => { delete store[k]; },
    _store: store,
  };
}

function load(opts) {
  opts = opts || {};
  const win = {
    addEventListener() {}, removeEventListener() {}, dispatchEvent() {},
    localStorage: makeStorage(opts.storage),
    innerWidth: 1280, innerHeight: 800,
  };
  const ctx = {
    window: win, console, Math, Date, JSON, Array, Object, String, Number,
    RegExp, Error, TypeError, Boolean, isNaN, isFinite, parseInt, parseFloat,
    URLSearchParams, Map, Set, Promise,
    encodeURIComponent, decodeURIComponent,
    setTimeout: () => 0, clearTimeout: () => {},
    document: {
      createElement: () => ({ style: {}, classList: { add() {}, remove() {}, toggle() {} },
        setAttribute() {}, appendChild() {}, addEventListener() {}, textContent: '', innerHTML: '' }),
      addEventListener() {}, body: { appendChild() {} },
      querySelector: () => null, querySelectorAll: () => [],
    },
  };
  ctx.localStorage = win.localStorage;
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync('public/antenna-height.js', 'utf8'), ctx);
  vm.runInContext(fs.readFileSync('public/terrain-profile.js', 'utf8'), ctx);
  const TP = ctx.window.TerrainProfile;
  assert.ok(TP, 'terrain-profile.js must define window.TerrainProfile');
  return TP;
}

const TP = load();

// ── 1. Deep link ───────────────────────────────────────────────────────────

const PK_A = 'a'.repeat(64);
const PK_B = 'b'.repeat(64);

console.log('\n=== #681 M4: deep-link parse / serialise ===');

test('two node pubkeys round-trip', () => {
  const state = TP.parseState('profile=' + PK_A + ',' + PK_B);
  eq(state.a, { kind: 'node', pubkey: PK_A });
  eq(state.b, { kind: 'node', pubkey: PK_B });
  assert.strictEqual(state.model, 'both');
  const again = TP.parseState(new URLSearchParams(TP.serializeState(state)).toString());
  eq(again, JSON.parse(JSON.stringify(state)));
});

test('node + free point round-trips and keeps 5 decimals', () => {
  const state = TP.parseState('profile=' + PK_A + ',56.16290,10.20390&model=dtm&ha=12&hb=4&k=1.2&freq=869.618');
  eq(state.a, { kind: 'node', pubkey: PK_A });
  eq(state.b, { kind: 'point', lat: 56.1629, lon: 10.2039 });
  assert.strictEqual(state.model, 'dtm');
  assert.strictEqual(state.ha, 12);
  assert.strictEqual(state.hb, 4);
  assert.strictEqual(state.k, 1.2);
  assert.strictEqual(state.freq, 869.618);
  const ser = TP.serializeState(state);
  assert.strictEqual(ser.profile, PK_A + ',56.16290,10.20390');
  eq(TP.parseState(new URLSearchParams(ser).toString()), JSON.parse(JSON.stringify(state)));
});

test('point + point round-trips (four numeric tokens are unambiguous)', () => {
  const state = TP.parseState('profile=56.10000,10.00000,55.90000,9.50000');
  eq(state.a, { kind: 'point', lat: 56.1, lon: 10 });
  eq(state.b, { kind: 'point', lat: 55.9, lon: 9.5 });
  eq(TP.parseState(new URLSearchParams(TP.serializeState(state)).toString()), JSON.parse(JSON.stringify(state)));
});

test('serialise omits defaults', () => {
  const ser = TP.serializeState(TP.parseState('profile=' + PK_A + ',' + PK_B));
  eq(Object.keys(ser), ['profile']);
});

test('uppercase pubkeys are normalised to lower case', () => {
  const state = TP.parseState('profile=' + PK_A.toUpperCase() + ',' + PK_B);
  assert.strictEqual(state.a.pubkey, PK_A);
});

test('a missing profile parameter yields null', () => {
  assert.strictEqual(TP.parseState('model=dtm'), null);
  assert.strictEqual(TP.parseState(''), null);
});

{
  const hostile = [
    ['script tag', 'profile=<script>alert(1)</script>'],
    ['one bare token', 'profile=56.1'],
    ['three numbers', 'profile=56.1,10.0,55.9'],
    ['five numbers', 'profile=56.1,10.0,55.9,9.5,1.0'],
    ['pubkey plus junk', 'profile=' + PK_A + ',' + PK_B + ',' + PK_A],
    ['short hex', 'profile=' + 'a'.repeat(63) + ',' + PK_B],
    ['non-hex 64 chars', 'profile=' + 'z'.repeat(64) + ',' + PK_B],
    ['latitude out of range', 'profile=91.0,10.0,' + PK_B],
    ['longitude out of range', 'profile=56.1,181.0,' + PK_B],
    ['exponent notation', 'profile=5.61e1,10.0,' + PK_B],
    ['NaN', 'profile=NaN,NaN,' + PK_B],
    ['Infinity', 'profile=Infinity,10,' + PK_B],
    ['empty value', 'profile='],
    ['separator spam', 'profile=,,,,'],
    ['over-long input', 'profile=' + '9'.repeat(5000)],
    ['prototype pollution attempt', 'profile=__proto__,constructor'],
  ];
  hostile.forEach(([label, qs]) => {
    test('hostile input rejected: ' + label, () => {
      assert.strictEqual(TP.parseState(qs), null, 'expected null for ' + qs.slice(0, 60));
    });
  });
}

test('hostile control values fall back to defaults, the pair still parses', () => {
  const state = TP.parseState('profile=' + PK_A + ',' + PK_B + '&model=<img>&k=abc&freq=-1');
  assert.strictEqual(state.model, 'both');
  assert.strictEqual(state.k, null);
  assert.strictEqual(state.freq, null);
});

test('__proto__ in the query string does not pollute Object.prototype', () => {
  TP.parseState('profile=' + PK_A + ',' + PK_B + '&__proto__[polluted]=1');
  assert.strictEqual({}.polluted, undefined);
});

test('requestURL builds exactly the documented endpoint query', () => {
  const state = TP.parseState('profile=' + PK_A + ',56.16290,10.20390&model=dtm&ha=12&hb=4&k=1.2&freq=869.618');
  const url = TP.requestURL(state);
  assert.ok(url.indexOf('/api/terrain/profile?') === 0, url);
  const q = new URLSearchParams(url.split('?')[1]);
  assert.strictEqual(q.get('a'), PK_A);
  assert.strictEqual(q.get('b'), '56.16290,10.20390');
  assert.strictEqual(q.get('model'), 'dtm');
  assert.strictEqual(q.get('ha'), '12');
  assert.strictEqual(q.get('hb'), '4');
  assert.strictEqual(q.get('k'), '1.2');
  assert.strictEqual(q.get('freq'), '869.618');
  assert.strictEqual(url.indexOf('token'), -1, 'the browser must never send a token');
});

test('requestURL omits unset controls but always states the model', () => {
  // #681 M6: the model is the one control the request cannot leave out.
  // The server's own default is "both", so a customizer default of "dtm"
  // would silently come back with a surface model if the parameter were
  // omitted the way k, freq and the heights are.
  const url = TP.requestURL(TP.parseState('profile=' + PK_A + ',' + PK_B));
  const q = new URLSearchParams(url.split('?')[1]);
  eq(Array.from(q.keys()).sort(), ['a', 'b', 'model']);
  assert.strictEqual(q.get('model'), 'both');
});

// ── 2. Height clamping ─────────────────────────────────────────────────────

console.log('\n=== #681 M4: antenna-height clamping ===');

test('clampHeight keeps values inside 0-100', () => {
  assert.strictEqual(TP.clampHeight(0), 0);
  assert.strictEqual(TP.clampHeight(10), 10);
  assert.strictEqual(TP.clampHeight(100), 100);
  assert.strictEqual(TP.clampHeight(100.4), 100);
  assert.strictEqual(TP.clampHeight(1000), 100);
  assert.strictEqual(TP.clampHeight(-1), 0);
  assert.strictEqual(TP.clampHeight('12'), 12);
  assert.strictEqual(TP.clampHeight('12.5'), 12.5);
});

test('clampHeight rejects what is not a finite number', () => {
  [null, undefined, '', 'abc', NaN, Infinity, -Infinity, {}, []].forEach(v => {
    assert.strictEqual(TP.clampHeight(v), null, 'expected null for ' + JSON.stringify(v));
  });
});

test('clampHeight rounds to one decimal', () => {
  assert.strictEqual(TP.clampHeight(12.34), 12.3);
});

test('the deep link clamps an out-of-range height instead of dropping the link', () => {
  const state = TP.parseState('profile=' + PK_A + ',' + PK_B + '&ha=9999&hb=-5');
  assert.strictEqual(state.ha, 100);
  assert.strictEqual(state.hb, 0);
});

// ── 2b. k and freq follow the coordinate grammar (review F9) ───────────────

console.log('\n=== #681 M4: k and freq are plain decimals ===');

function ctrl(qs) { return TP.parseState('profile=' + PK_A + ',' + PK_B + '&' + qs); }

test('a hex literal is not a k factor', () => {
  assert.strictEqual(ctrl('k=0x5').k, null, '0x5 was accepted as k');
  assert.strictEqual(ctrl('freq=0x368').freq, null, '0x368 was accepted as freq');
});

test('exponent notation is not a k factor either', () => {
  assert.strictEqual(ctrl('k=1e0').k, null, '1e0 was accepted as k');
  assert.strictEqual(ctrl('freq=8.69618e2').freq, null, 'exponent accepted as freq');
  assert.strictEqual(ctrl('k=Infinity').k, null);
  assert.strictEqual(ctrl('k= 2 ').k, null, 'whitespace padding was coerced');
});

test('plain decimals still parse, and the bounds still hold', () => {
  assert.strictEqual(ctrl('k=1.3333').k, 1.3333);
  assert.strictEqual(ctrl('freq=869.618').freq, 869.618);
  assert.strictEqual(ctrl('k=0.4').k, null, 'below the 0.5 floor');
  assert.strictEqual(ctrl('freq=2500').freq, 2500);
  assert.strictEqual(ctrl('freq=2500.1').freq, null, 'above the 2500 ceiling');
});

// ── 3. Verdict chip ────────────────────────────────────────────────────────

console.log('\n=== #681 M4: verdict chip ===');

function resp(verdict, coverage) {
  return {
    verdict: Object.assign({ state: 'clear', criterion: 0.6, model: 'dtm' }, verdict),
    coverage: Object.assign({ state: 'full', pct: 100, firstGapM: null }, coverage),
  };
}

test('clear → ok tone', () => {
  const c = TP.verdictChip(resp({ state: 'clear' }));
  assert.strictEqual(c.state, 'clear');
  assert.strictEqual(c.tone, 'ok');
  assert.strictEqual(c.label, 'Clear');
});

test('fresnel100 → ok tone, criterion met', () => {
  const c = TP.verdictChip(resp({ state: 'fresnel100' }));
  assert.strictEqual(c.tone, 'ok');
  assert.ok(/60\s?%/.test(c.detail), c.detail);
});

test('fresnel60 → warn tone', () => {
  assert.strictEqual(TP.verdictChip(resp({ state: 'fresnel60' })).tone, 'warn');
});

test('blocked → bad tone', () => {
  const c = TP.verdictChip(resp({ state: 'blocked' }));
  assert.strictEqual(c.tone, 'bad');
  assert.strictEqual(c.label, 'Blocked');
});

test('unknown on a partial profile says the path leaves Denmark', () => {
  const c = TP.verdictChip(resp({ state: 'unknown' }, { state: 'partial', pct: 79.8 }));
  assert.strictEqual(c.state, 'unknown');
  assert.strictEqual(c.tone, 'unknown');
  assert.strictEqual(c.label, 'No verdict');
  assert.ok(/leaves Denmark/i.test(c.detail), c.detail);
  assert.ok(/79\.8\s?%/.test(c.detail), c.detail);
});

test('unknown with no coverage at all points at the Denmark-only limit', () => {
  const c = TP.verdictChip(resp({ state: 'unknown' }, { state: 'none', pct: 0 }));
  assert.strictEqual(c.label, 'No verdict');
  assert.ok(/Denmark only/i.test(c.detail), c.detail);
});

test('an unrecognised verdict state degrades to unknown, never to clear', () => {
  const c = TP.verdictChip(resp({ state: 'totally-new-state' }));
  assert.strictEqual(c.tone, 'unknown');
  assert.strictEqual(c.label, 'No verdict');
});

test('partial coverage is never clear, whatever the verdict says', () => {
  // The server forces unknown on a gap today; the client must hold that
  // line by itself, so a stale cache or a future server cannot paint a
  // confident green over a path that was never measured end to end.
  const c = TP.verdictChip(resp({ state: 'clear' }, { state: 'partial', pct: 62.5 }));
  assert.strictEqual(c.state, 'unknown', 'a partial profile reported ' + c.state);
  assert.strictEqual(c.tone, 'unknown');
  assert.strictEqual(c.label, 'No verdict');
  assert.ok(/leaves Denmark/i.test(c.detail), c.detail);
});

test('no coverage at all is never a verdict either', () => {
  ['clear', 'fresnel100', 'fresnel60', 'blocked'].forEach((state) => {
    const c = TP.verdictChip(resp({ state: state }, { state: 'none', pct: 0 }));
    assert.strictEqual(c.state, 'unknown', state + ' survived a coverage of none');
  });
});

test('a response with no coverage block is not treated as a gap', () => {
  // Absence of the block is absence of evidence, not evidence of a hole:
  // downgrading there would make every verdict unknown.
  const c = TP.verdictChip({ verdict: { state: 'clear', criterion: 0.6 } });
  assert.strictEqual(c.state, 'clear');
});

test('a missing response is unknown, not a crash', () => {
  assert.strictEqual(TP.verdictChip(null).state, 'unknown');
  assert.strictEqual(TP.verdictChip({}).state, 'unknown');
});

// ── 3b. Surface (buildings/trees) chip, #729 ───────────────────────────────

console.log('\n=== #729: surface chip ===');

/* The server already answers verdict.surface (cmd/server/terrain_profile.go,
 * #689 M3): the DTM decides the verdict, the DSM is information only (owner
 * decision 6 on #681). These cases pin the UI side of that split — the main
 * chip says which model it speaks for, and a surface verdict that is WORSE
 * than the terrain one gets a chip of its own instead of disappearing into
 * the hatched band. */
function surf(terrain, surface, over) {
  return Object.assign({
    model: 'both',
    coverage: { state: 'full', pct: 100, firstGapM: null },
    verdict: {
      state: terrain, model: 'dtm', criterion: 0.6,
      worstClearanceM: 6.9, worstAtM: 138, worstF1Fraction: 1.1,
      surface: surface === null ? undefined
        : { state: surface, worstClearanceM: -17.5, worstAtM: 138, worstF1Fraction: -2.1 },
    },
  }, over || {});
}

// Worst first. The chip appears exactly when the surface sits below the
// terrain on this ladder, which is the "worse than" of the issue.
const LADDER = ['blocked', 'fresnel60', 'fresnel100', 'clear'];

test('the Dokk1 case: terrain clear, surface blocked → a warn chip of its own', () => {
  const c = TP.surfaceChip(surf('clear', 'blocked'));
  assert.ok(c, 'no surface chip at all');
  assert.strictEqual(c.show, true, 'the surface chip was hidden');
  assert.strictEqual(c.state, 'blocked');
  assert.strictEqual(c.tone, 'warn', 'tone = ' + c.tone);
  assert.strictEqual(c.label, 'Buildings/trees: Blocked');
  assert.ok(/buildings|trees/i.test(c.detail), c.detail);
});

test('the main chip names its model when the surface is in play', () => {
  assert.strictEqual(TP.verdictChip(surf('clear', 'blocked')).label, 'Terrain: Clear');
  assert.strictEqual(TP.verdictChip(surf('blocked', 'blocked')).label, 'Terrain: Blocked');
});

test('with "Terrain only" the main chip keeps its bare label', () => {
  const d = surf('clear', 'blocked', { model: 'dtm' });
  assert.strictEqual(TP.verdictChip(d).label, 'Clear');
});

test('with "Terrain only" there is no surface chip, even with surface data', () => {
  assert.strictEqual(TP.surfaceChip(surf('clear', 'blocked', { model: 'dtm' })), null);
});

test('a response with no verdict.surface has no surface chip', () => {
  assert.strictEqual(TP.surfaceChip(surf('clear', null)), null);
  assert.strictEqual(TP.surfaceChip(null), null);
  assert.strictEqual(TP.surfaceChip({}), null);
});

test('every terrain/surface combination: shown only when the surface is worse', () => {
  LADDER.forEach((t, ti) => {
    LADDER.forEach((s, si) => {
      const c = TP.surfaceChip(surf(t, s));
      assert.ok(c, 'no chip object for ' + t + '/' + s);
      assert.strictEqual(c.state, s, t + '/' + s + ' mapped to ' + c.state);
      assert.strictEqual(c.label, 'Buildings/trees: ' + TP.verdictChip(surf(s, null, { model: 'dtm' })).label,
        'label mismatch for ' + t + '/' + s + ': ' + c.label);
      assert.strictEqual(c.show, si < ti,
        t + '/' + s + ' show = ' + c.show + ', expected ' + (si < ti));
    });
  });
});

test('the two agreeing hides the chip, on every state', () => {
  LADDER.forEach((s) => {
    assert.strictEqual(TP.surfaceChip(surf(s, s)).show, false, s + '/' + s + ' was shown');
  });
});

test('a surface better than the terrain is not a second verdict either', () => {
  // DSM ≥ DTM by construction, so this should never arrive; if it does, the
  // panel must not use it to soften a bad terrain verdict.
  assert.strictEqual(TP.surfaceChip(surf('blocked', 'clear')).show, false);
});

test('partial coverage makes the surface unknown, never clear (#689 N1)', () => {
  const d = surf('unknown', 'clear', { coverage: { state: 'partial', pct: 62.5, firstGapM: 7500 } });
  const c = TP.surfaceChip(d);
  assert.ok(c, 'no surface chip object on a partial profile');
  assert.strictEqual(c.state, 'unknown', 'a partial profile reported ' + c.state);
  assert.ok(!/clear/i.test(c.label), 'a partial profile offered "' + c.label + '"');
  assert.strictEqual(c.show, false, 'an unjudged surface was shown as a verdict');
});

test('partial coverage downgrades a blocked surface too', () => {
  const d = surf('unknown', 'blocked', { coverage: { state: 'partial', pct: 62.5, firstGapM: 7500 } });
  const c = TP.surfaceChip(d);
  assert.strictEqual(c.state, 'unknown', 'state = ' + c.state);
  assert.strictEqual(c.show, false);
});

test('no coverage at all is never a surface verdict', () => {
  LADDER.forEach((s) => {
    const c = TP.surfaceChip(surf('unknown', s, { coverage: { state: 'none', pct: 0, firstGapM: 0 } }));
    assert.strictEqual(c.state, 'unknown', s + ' survived a coverage of none');
    assert.strictEqual(c.show, false, s + ' was shown with no coverage');
  });
});

test('an unrecognised surface state degrades to unknown, never to a tone', () => {
  const c = TP.surfaceChip(surf('clear', 'totally-new-state'));
  assert.strictEqual(c.state, 'unknown');
  assert.strictEqual(c.show, false, 'a state this build does not know was shown anyway');
});

test('the explanatory line separates blocking from attenuation', () => {
  assert.ok(/buildings/i.test(TP.SURFACE_NOTE), TP.SURFACE_NOTE);
  assert.ok(/trees/i.test(TP.SURFACE_NOTE), TP.SURFACE_NOTE);
  assert.ok(/attenuat/i.test(TP.SURFACE_NOTE), TP.SURFACE_NOTE);
});

// ── 4. SVG geometry ────────────────────────────────────────────────────────

console.log('\n=== #681 M4: chart geometry ===');

// A flat 1 km path, both antennas 10 m, k = 1 so the bulge is the plain
// geometric d1·d2/(2R), λ = 0.344740 m (869.618 MHz). Every expected number
// below was computed from the published formulas, not from this module.
function flatPath() {
  return {
    distanceM: 1000,
    a: { lat: 56, lon: 10, kind: 'point', groundM: 0, antennaM: 10, antennaSource: 'request', assumed: false },
    b: { lat: 56.01, lon: 10, kind: 'point', groundM: 0, antennaM: 10, antennaSource: 'request', assumed: false },
    model: 'dtm',
    stepM: 500,
    sampleCount: 3,
    coverage: { state: 'full', pct: 100, firstGapM: null },
    samples: [{ d: 0, dtm: 0 }, { d: 500, dtm: 0 }, { d: 1000, dtm: 0 }],
    physics: { freqMHz: 869.618, freqSource: 'firmware', lambdaM: 0.344740, k: 1, kSource: 'request', f1MaxM: 9.28 },
    verdict: { state: 'clear', model: 'dtm', criterion: 0.6, worstClearanceM: 9.98, worstAtM: 500, worstF1Fraction: 1.07 },
    attribution: 'Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel, CC BY 4.0',
  };
}

test('plot box is the documented inset of the 820x300 viewBox', () => {
  const g = TP.chartGeometry(flatPath());
  eq(g.view, { w: 820, h: 300 });
  eq(g.plot, { x: 46, y: 14, w: 758, h: 256 });
});

test('x axis maps distance onto the plot box', () => {
  const g = TP.chartGeometry(flatPath());
  eq(g.x, [46, 425, 804]);
});

test('y domain covers terrain and the whole first Fresnel zone, with padding', () => {
  const g = TP.chartGeometry(flatPath());
  assert.strictEqual(g.domain.zMin, -1.54);
  assert.strictEqual(g.domain.zMax, 20.83);
});

test('terrain is drawn with the earth bulge added', () => {
  const g = TP.chartGeometry(flatPath());
  // d1·d2/(2kR) at mid-path = 500·500/(2·1·6371000) = 0.0196 m
  eq(g.terrainZ, [0, 0.02, 0]);
  eq(g.terrainY, [252.38, 252.15, 252.38]);
});

test('line of sight and both Fresnel bands land on the computed rows', () => {
  const g = TP.chartGeometry(flatPath());
  eq(g.losY, [137.94, 137.94, 137.94]);
  // √(λ·d1·d2/D) = √(0.34474·500·500/1000) = 9.28 m at mid-path
  eq(g.f1, [0, 9.28, 0]);
  eq(g.f1TopY, [137.94, 31.7, 137.94]);
  eq(g.f1BotY, [137.94, 244.18, 137.94]);
  eq(g.f60TopY, [137.94, 74.19, 137.94]);
});

test('a clear path has no intrusion runs', () => {
  eq(TP.chartGeometry(flatPath()).intrusions, []);
});

test('terrain above the 60 % line is reported as an intrusion run', () => {
  const d = flatPath();
  d.samples = [{ d: 0, dtm: 0 }, { d: 500, dtm: 6 }, { d: 1000, dtm: 0 }];
  d.verdict = { state: 'fresnel60', model: 'dtm', criterion: 0.6, worstClearanceM: 3.98, worstAtM: 500, worstF1Fraction: 0.43 };
  const g = TP.chartGeometry(d);
  // clearance 10 − (6 + 0.0196) = 3.98 m, 0.43 of the 9.28 m F1 radius: below 0.6
  assert.strictEqual(g.intrusions.length, 1);
  assert.strictEqual(g.intrusions[0].fromM, 500);
  assert.strictEqual(g.intrusions[0].toM, 500);
});

// A profile whose decimated samples rise through the 60 % zone at mid-path.
// The server judged the *full-resolution* profile, so its verdict is the
// authority; what these cases pin is that the chart never argues with it.
function intrudingPath(verdictState, coverage) {
  const d = flatPath();
  d.samples = [{ d: 0, dtm: 0 }, { d: 500, dtm: 6 }, { d: 1000, dtm: 0 }];
  d.verdict = { state: verdictState, model: 'dtm', criterion: 0.6, worstClearanceM: 3.98, worstAtM: 500, worstF1Fraction: 0.43 };
  if (coverage) d.coverage = coverage;
  return d;
}

test('a clear verdict never gets intrusion bands, even if the samples say otherwise', () => {
  const d = intrudingPath('clear');
  eq(TP.chartGeometry(d).intrusions, [], 'bands drawn under a Clear chip');
  assert.strictEqual(TP.buildChart(d, {}).indexOf('tp-intrusion'), -1, 'an intrusion rect reached a clear chart');
});

test('fresnel100 asserts the 60 % zone is clear, so it gets no bands either', () => {
  eq(TP.chartGeometry(intrudingPath('fresnel100')).intrusions, []);
});

test('a gap in the coverage removes the bands with the verdict', () => {
  // The chip is unknown here (coverage gate), so bands would contradict it.
  const d = intrudingPath('blocked', { state: 'partial', pct: 62.5, firstGapM: 800 });
  eq(TP.chartGeometry(d).intrusions, [], 'bands drawn on an unjudgeable profile');
  assert.strictEqual(TP.verdictChip(d).state, 'unknown');
});

test('bands and a warning chip can only appear together', () => {
  [['clear', false], ['fresnel100', false], ['fresnel60', true], ['blocked', true],
   ['unknown', false], ['totally-new-state', false]].forEach(([state, want]) => {
    const d = intrudingPath(state);
    const banded = TP.chartGeometry(d).intrusions.length > 0;
    const tone = TP.verdictChip(d).tone;
    assert.strictEqual(banded, want, state + ' bands = ' + banded);
    if (banded) assert.ok(tone === 'warn' || tone === 'bad', state + ' banded with a ' + tone + ' chip');
  });
});

test('out-of-coverage samples break the terrain into segments, not a straight line', () => {
  const d = flatPath();
  d.coverage = { state: 'partial', pct: 66.7, firstGapM: 500 };
  d.samples = [{ d: 0, dtm: 0 }, { d: 500, dtm: null }, { d: 1000, dtm: 0 }];
  d.verdict = { state: 'unknown', model: 'dtm', criterion: 0.6, worstClearanceM: null, worstAtM: null, worstF1Fraction: null };
  const g = TP.chartGeometry(d);
  assert.strictEqual(g.segments.length, 2, 'expected two terrain segments around the gap');
  eq(g.gaps, [{ fromM: 500, toM: 500, x1: 425, x2: 425 }]);
  eq(g.intrusions, [], 'no verdict means no intrusion markers');
});

test('the DSM clutter band is only built for model=both', () => {
  const d = flatPath();
  assert.strictEqual(TP.chartGeometry(d).surfaceY, null);
  d.model = 'both';
  d.samples = [{ d: 0, dtm: 0, dsm: 12 }, { d: 500, dtm: 0, dsm: 12 }, { d: 1000, dtm: 0, dsm: 12 }];
  assert.strictEqual(TP.chartGeometry(d).surfaceY.length, 3);
});

test('an empty sample list yields no geometry', () => {
  const d = flatPath();
  d.samples = [];
  d.coverage = { state: 'none', pct: 0, firstGapM: 0 };
  assert.strictEqual(TP.chartGeometry(d), null);
});

console.log('\n=== #681 M4: chart SVG ===');

test('the chart uses only CSS variables for its colours', () => {
  const d = flatPath();
  d.model = 'both';
  d.samples = [{ d: 0, dtm: 0, dsm: 9 }, { d: 500, dtm: 0, dsm: 9 }, { d: 1000, dtm: 0, dsm: 9 }];
  const svg = TP.buildChart(d, { nameA: 'Alfa', nameB: 'Bravo' });
  assert.strictEqual(/#[0-9a-fA-F]{3,8}\b/.test(svg), false, 'hard-coded colour in the chart SVG');
  ['--terrain-fill', '--terrain-clutter', '--los-line', '--fresnel-100', '--fresnel-60'].forEach(v => {
    assert.ok(svg.indexOf('var(' + v) >= 0, 'missing ' + v);
  });
});

test('the chart is an accessible image with the verdict in its title', () => {
  const svg = TP.buildChart(flatPath(), { nameA: 'Alfa', nameB: 'Bravo' });
  assert.ok(svg.indexOf('role="img"') >= 0, 'missing role="img"');
  const m = svg.match(/<title>([\s\S]*?)<\/title>/);
  assert.ok(m, 'missing <title>');
  assert.ok(/Alfa/.test(m[1]) && /Bravo/.test(m[1]), m[1]);
  assert.ok(/clear/i.test(m[1]), m[1]);
});

test('the CC BY 4.0 credit is the shared constant, not micro text in the SVG', () => {
  // Review F7: a 9px label inside an 820-unit viewBox stretched onto a
  // 340px panel renders about 3px tall. A licence credit nobody can read
  // is not a credit, so it is the panel's HTML line now; the E2E pins
  // that it is visible, and visible exactly once.
  assert.strictEqual(TP.ATTRIBUTION, 'Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel, CC BY 4.0');
  const svg = TP.buildChart(flatPath(), {});
  assert.strictEqual(svg.indexOf('Klimadatastyrelsen'), -1, 'the credit is back inside the stretched chart');
});

test('no chart text is drawn as SVG text at all', () => {
  const d = flatPath();
  d.coverage = { state: 'partial', pct: 66.7, firstGapM: 500 };
  d.samples = [{ d: 0, dtm: 0 }, { d: 500, dtm: null }, { d: 1000, dtm: 0 }];
  d.verdict = { state: 'unknown', model: 'dtm', criterion: 0.6, worstClearanceM: null, worstAtM: null, worstF1Fraction: null };
  const html = TP.buildChart(d, { nameA: 'Alfa', nameB: 'Bravo' });
  const svg = html.slice(html.indexOf('<svg'), html.indexOf('</svg>'));
  assert.strictEqual(/<text\b/.test(svg), false, 'SVG <text> survives in the stretched chart');
  // <title> is not drawn, it is what a screen reader reads out.
  assert.ok(/<title>/.test(svg), 'the accessible title was lost with the labels');
});

test('every chart label is HTML positioned over the plot in percentages', () => {
  const d = flatPath();
  d.coverage = { state: 'partial', pct: 66.7, firstGapM: 500 };
  d.samples = [{ d: 0, dtm: 0 }, { d: 500, dtm: null }, { d: 1000, dtm: 0 }];
  d.verdict = { state: 'unknown', model: 'dtm', criterion: 0.6, worstClearanceM: null, worstAtM: null, worstF1Fraction: null };
  const html = TP.buildChart(d, {});
  assert.ok(/^<div class="tp-plot">/.test(html), 'the chart is not wrapped in a positioning box');
  ['tp-ax-y', 'tp-ax-x0', 'tp-ax-x1', 'tp-ax-gap', 'tp-pin-a', 'tp-pin-b'].forEach((cls) => {
    assert.ok(html.indexOf(cls) >= 0, 'missing HTML label ' + cls);
  });
  // Five elevation ticks, one per gridline.
  assert.strictEqual((html.match(/class="tp-ax tp-ax-y"/g) || []).length, 5);
  assert.ok(html.indexOf('>no data</span>') >= 0, 'the gap label is not HTML');
  // x = 425 of 820 and y = 262 of 300 for the gap label.
  assert.ok(/left:51\.829%/.test(html), 'label x is not a percentage of the viewBox: ' + html.slice(html.indexOf('tp-ax-gap') - 40, html.indexOf('tp-ax-gap') + 80));
  // Both endpoint tags survive as readable text.
  assert.ok(html.indexOf('>A</span>') >= 0 && html.indexOf('>B</span>') >= 0, 'the A/B pin labels were lost');
});

test('a hostile node name cannot escape through an HTML label', () => {
  const html = TP.buildChart(flatPath(), { nameA: '<img src=x onerror=alert(1)>', nameB: 'ok' });
  assert.strictEqual(html.indexOf('<img'), -1, 'raw tag reached the chart HTML');
});

test('the worst sample is marked, at the distance the server reported', () => {
  const d = flatPath();
  d.samples = [{ d: 0, dtm: 0 }, { d: 500, dtm: 30 }, { d: 1000, dtm: 0 }];
  d.verdict = { state: 'blocked', model: 'dtm', criterion: 0.6, worstClearanceM: -20.02, worstAtM: 500, worstF1Fraction: -2.16 };
  const svg = TP.buildChart(d, {});
  const m = svg.match(/<line class="tp-worst"[^>]*>/);
  assert.ok(m, 'no worst-sample marker on a blocked profile');
  // worstAtM 500 of 1000 m → the middle of the 758-wide plot box at x=46.
  assert.ok(/x1="425"/.test(m[0]) && /x2="425"/.test(m[0]), 'marker is not at 500 m: ' + m[0]);
  assert.ok(/y1="14"/.test(m[0]) && /y2="270"/.test(m[0]), 'marker does not span the plot: ' + m[0]);
  assert.ok(/stroke="var\(--profile-blocked\)/.test(m[0]), 'marker is not drawn in the blocked token');
});

test('a clear path carries no worst-sample marker', () => {
  assert.strictEqual(TP.buildChart(flatPath(), {}).indexOf('tp-worst'), -1,
    'the worst-sample marker was drawn without an intrusion');
});

test('the chart emits no zero-radius dead markup', () => {
  const d = flatPath();
  d.samples = [{ d: 0, dtm: 0 }, { d: 500, dtm: 30 }, { d: 1000, dtm: 0 }];
  d.verdict = { state: 'blocked', model: 'dtm', criterion: 0.6, worstClearanceM: -20.02, worstAtM: 500, worstF1Fraction: -2.16 };
  assert.strictEqual(/r="0"/.test(TP.buildChart(d, {})), false, 'an invisible r="0" circle is still emitted');
});

test('a blocked path draws intrusion markers', () => {
  const d = flatPath();
  d.samples = [{ d: 0, dtm: 0 }, { d: 500, dtm: 30 }, { d: 1000, dtm: 0 }];
  d.verdict = { state: 'blocked', model: 'dtm', criterion: 0.6, worstClearanceM: -20.02, worstAtM: 500, worstF1Fraction: -2.16 };
  const svg = TP.buildChart(d, {});
  assert.ok(svg.indexOf('var(--profile-blocked') >= 0, 'missing --profile-blocked');
});

// ── 4b. The chart's CSS variables ──────────────────────────────────────────

console.log('\n=== #681 M4: chart CSS variables ===');

{
  const css = fs.readFileSync('public/style.css', 'utf8');
  const VARS = ['--terrain-fill', '--terrain-clutter', '--los-line', '--fresnel-100', '--fresnel-60', '--profile-blocked'];
  // style.css carries the dark palette twice on purpose (the prefers-color-scheme
  // media query and the [data-theme="dark"] block, both flagged "KEEP IN SYNC").
  // A token defined in only one of them is a theme that silently falls back to
  // the light value, so every one of the six must appear three times.
  test('every chart colour token is defined in :root and in both dark blocks', () => {
    VARS.forEach((v) => {
      const defs = (css.match(new RegExp('\\n\\s*' + v + '\\s*:', 'g')) || []).length;
      assert.strictEqual(defs, 3, `${v} is defined ${defs} times, want 3 (:root + both dark blocks)`);
    });
  });

  test('every chart colour token is actually used', () => {
    const used = fs.readFileSync('public/terrain-profile.js', 'utf8') + css;
    VARS.forEach((v) => {
      assert.ok(new RegExp('var\\(' + v + '[,)]').test(used), v + ' is defined but never used');
    });
  });

}

// ── 5. localStorage ────────────────────────────────────────────────────────

console.log('\n=== #681 M4: per-viewer height memory ===');

test('a stored height is remembered per pubkey', () => {
  const T = load({ storage: 'ok' });
  T.rememberHeight({ kind: 'node', pubkey: PK_A }, 18);
  assert.strictEqual(T.recallHeight({ kind: 'node', pubkey: PK_A }), 18);
  assert.strictEqual(T.recallHeight({ kind: 'node', pubkey: PK_B }), null);
});

test('free map points share one remembered height', () => {
  const T = load({ storage: 'ok' });
  T.rememberHeight({ kind: 'point', lat: 56, lon: 10 }, 3);
  assert.strictEqual(T.recallHeight({ kind: 'point', lat: 55, lon: 9 }), 3);
});

test('a stored height outside the range is clamped on read', () => {
  const T = load({ storage: 'ok' });
  T.rememberHeight({ kind: 'node', pubkey: PK_A }, 10);
  T.storageForTests().setItem('mc-terrain-h:' + PK_A, '9999');
  assert.strictEqual(T.recallHeight({ kind: 'node', pubkey: PK_A }), 100);
  T.storageForTests().setItem('mc-terrain-h:' + PK_A, 'not a number');
  assert.strictEqual(T.recallHeight({ kind: 'node', pubkey: PK_A }), null);
});

test('a localStorage that throws is survivable', () => {
  const T = load({ storage: 'throws' });
  assert.doesNotThrow(() => T.rememberHeight({ kind: 'node', pubkey: PK_A }, 18));
  assert.strictEqual(T.recallHeight({ kind: 'node', pubkey: PK_A }), null);
});

test('a missing localStorage is survivable', () => {
  const T = load({ storage: 'absent' });
  assert.doesNotThrow(() => T.rememberHeight({ kind: 'node', pubkey: PK_A }, 18));
  assert.strictEqual(T.recallHeight({ kind: 'node', pubkey: PK_A }), null);
});

test('a null endpoint never touches storage', () => {
  const T = load({ storage: 'ok' });
  assert.strictEqual(T.recallHeight(null), null);
  assert.doesNotThrow(() => T.rememberHeight(null, 5));
  eq(Object.keys(T.storageForTests()._store), []);
});

// ── 6. XSS ─────────────────────────────────────────────────────────────────

console.log('\n=== #681 M4: node names are escaped ===');

const TAG_PAYLOAD = '<img src=x onerror=alert(1)>';
const ATTR_PAYLOAD = "' onfocus=alert(1) '";

test('a hostile node name cannot inject markup into the chart title', () => {
  const svg = TP.buildChart(flatPath(), { nameA: TAG_PAYLOAD, nameB: ATTR_PAYLOAD });
  assert.strictEqual(svg.indexOf('<img'), -1, 'raw tag reached the SVG');
  // The payload text may survive, but only with its metacharacters escaped,
  // so it can never open an element or an attribute.
  assert.ok(svg.indexOf('&lt;img src=x onerror=alert(1)&gt;') >= 0, 'expected the payload escaped, not dropped');
  assert.ok(svg.indexOf('&#39; onfocus=alert(1) &#39;') >= 0, 'expected the apostrophes escaped');
});

test('a hostile node name cannot break out of the endpoint labels', () => {
  const svg = TP.buildChart(flatPath(), { nameA: '"><script>alert(1)</script>', nameB: 'ok' });
  assert.strictEqual(svg.indexOf('<script'), -1, 'raw script tag reached the SVG');
  assert.ok(svg.indexOf('&quot;&gt;&lt;script&gt;') >= 0, 'expected the break-out attempt escaped');
});

test('escaping covers every HTML metacharacter', () => {
  const svg = TP.buildChart(flatPath(), { nameA: `<>&"'`, nameB: 'ok' });
  assert.ok(svg.indexOf('&lt;&gt;&amp;&quot;&#39;') >= 0, 'incomplete escaping');
});

/* #729 — a Dokk1-shaped profile: the DTM is well clear, the DSM is a 28 m
 * building at 138 m of a 237 m path. Both verdicts have to reach a screen
 * reader, not just the hatched band in the picture. */
function dokk1Path() {
  const d = flatPath();
  d.model = 'both';
  d.distanceM = 237;
  d.samples = [{ d: 0, dtm: 0, dsm: 0 }, { d: 138, dtm: 0, dsm: 28 }, { d: 237, dtm: 0, dsm: 0 }];
  d.verdict = {
    state: 'clear', model: 'dtm', criterion: 0.6,
    worstClearanceM: 6.9, worstAtM: 138, worstF1Fraction: 1.1,
    surface: { state: 'blocked', worstClearanceM: -17.5, worstAtM: 138, worstF1Fraction: -2.6 },
  };
  return d;
}

test('#729: the chart title names both verdicts', () => {
  const svg = TP.buildChart(dokk1Path(), { nameA: 'Alfa', nameB: 'Bravo' });
  const m = svg.match(/<title>([\s\S]*?)<\/title>/);
  assert.ok(m, 'missing <title>');
  assert.ok(/Terrain: Clear/.test(m[1]), m[1]);
  assert.ok(/Buildings\/trees: Blocked/.test(m[1]), m[1]);
});

test('#729: the chart\'s accessible name carries both verdicts too', () => {
  // role="img" + aria-label: the label is the accessible NAME, so a verdict
  // that lives only in <title> is never announced.
  const svg = TP.buildChart(dokk1Path(), { nameA: 'Alfa', nameB: 'Bravo' });
  const m = svg.match(/aria-label="([^"]*)"/);
  assert.ok(m, 'missing aria-label');
  assert.ok(/Terrain: Clear/.test(m[1]), m[1]);
  assert.ok(/Buildings\/trees: Blocked/.test(m[1]), m[1]);
});

test('#729: an agreeing surface adds nothing to the chart title', () => {
  const d = dokk1Path();
  d.verdict.surface.state = 'clear';
  const svg = TP.buildChart(d, { nameA: 'Alfa', nameB: 'Bravo' });
  assert.strictEqual(svg.indexOf('Buildings/trees'), -1, 'a hidden chip still reached the title');
});

test('#729: a hostile node name is still escaped with the surface chip in play', () => {
  const svg = TP.buildChart(dokk1Path(), { nameA: TAG_PAYLOAD, nameB: 'ok' });
  assert.strictEqual(svg.indexOf('<img'), -1, 'raw tag reached the SVG');
  assert.ok(svg.indexOf('&lt;img src=x onerror=alert(1)&gt;') >= 0, 'expected the payload escaped');
  // The aria-label is a quoted attribute built by concatenation; a name that
  // closes it would hand an attacker an event handler on the <svg>.
  const m = svg.match(/aria-label="([^"]*)"/);
  assert.ok(m && /Terrain: Clear/.test(m[1]), 'aria-label broken: ' + (m && m[1]));
});

test('#729: the chart still uses only CSS variables with the surface band', () => {
  assert.strictEqual(/#[0-9a-fA-F]{3,8}\b/.test(TP.buildChart(dokk1Path(), {})), false,
    'hard-coded colour in the chart SVG');
});

// ── 7. The request sequence guard (review F4) ──────────────────────────────

console.log('\n=== #681 M4: a newer answer always wins ===');

/* A DOM just real enough for create(): elements that remember children,
 * classes, attributes and listeners. The panel never reaches a browser
 * here — what is under test is which response ends up in the chip. */
function fakeDom() {
  const made = [];
  function mkEl(tag) {
    const on = {};
    const classes = new Set();
    const e = {
      tagName: String(tag).toUpperCase(), children: [], attrs: {}, style: {},
      className: '', textContent: '', innerHTML: '', value: '', hidden: false,
      parentNode: null,
      classList: {
        add: (c) => classes.add(c),
        remove: (c) => classes.delete(c),
        contains: (c) => classes.has(c),
        toggle: (c, want) => { const on2 = want === undefined ? !classes.has(c) : !!want; if (on2) classes.add(c); else classes.delete(c); },
      },
      setAttribute: (k, v) => { e.attrs[k] = String(v); },
      getAttribute: (k) => (Object.prototype.hasOwnProperty.call(e.attrs, k) ? e.attrs[k] : null),
      appendChild: (c) => { e.children.push(c); c.parentNode = e; return c; },
      removeChild: (c) => { e.children = e.children.filter(x => x !== c); c.parentNode = null; return c; },
      addEventListener: (t, fn) => { (on[t] || (on[t] = [])).push(fn); },
      removeEventListener: (t, fn) => { on[t] = (on[t] || []).filter(f => f !== fn); },
      fire: (t) => { (on[t] || []).slice().forEach(fn => fn({ target: e, preventDefault() {} })); },
    };
    made.push(e);
    return e;
  }
  const body = mkEl('body');
  return {
    doc: { createElement: mkEl, body: body, addEventListener() {}, querySelector: () => null, querySelectorAll: () => [] },
    body: body,
    find: (id) => made.filter(e => e.attrs['data-testid'] === id).pop() || null,
  };
}

function loadLive() {
  const dom = fakeDom();
  const win = {
    addEventListener() {}, removeEventListener() {}, dispatchEvent() {},
    localStorage: makeStorage('ok'),
    innerWidth: 1280, innerHeight: 800,
    document: dom.doc,
    setTimeout: (fn, ms) => setTimeout(fn, ms),
    clearTimeout: (t) => clearTimeout(t),
  };
  const ctx = {
    window: win, console, Math, Date, JSON, Array, Object, String, Number,
    RegExp, Error, TypeError, Boolean, isNaN, isFinite, parseInt, parseFloat,
    URLSearchParams, Map, Set, Promise, encodeURIComponent, decodeURIComponent,
    setTimeout, clearTimeout, document: dom.doc,
  };
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync('public/antenna-height.js', 'utf8'), ctx);
  vm.runInContext(fs.readFileSync('public/terrain-profile.js', 'utf8'), ctx);
  // `win` comes back so a test can plant window.SITE_CONFIG — the customizer
  // defaults the module reads at call time (#681 M6).
  return { TP: ctx.window.TerrainProfile, dom: dom, win: win };
}

/* A fetchJSON whose answers are handed out one at a time, so a test can
 * land them in whatever order it likes. */
function deferredFetch() {
  const calls = [];
  const fn = (url) => {
    let settle;
    const promise = new Promise((resolve, reject) => { settle = { resolve, reject }; });
    calls.push({ url: url, settle: settle });
    return promise;
  };
  fn.calls = calls;
  return fn;
}

function chipOf(dom) {
  const chip = dom.find('tp-verdict');
  return chip ? { label: chip.textContent, state: chip.attrs['data-state'] } : null;
}

function answer(verdictState, coverageState) {
  return {
    a: { kind: 'node', groundM: 0, antennaM: 10, antennaSource: 'request', assumed: false },
    b: { kind: 'node', groundM: 0, antennaM: 10, antennaSource: 'request', assumed: false },
    distanceM: 1000, model: 'dtm', stepM: 500, sampleCount: 3,
    coverage: { state: coverageState || 'full', pct: 100, firstGapM: null },
    samples: [{ d: 0, dtm: 0 }, { d: 500, dtm: 0 }, { d: 1000, dtm: 0 }],
    physics: { freqMHz: 869.618, freqSource: 'firmware', lambdaM: 0.34474, k: 1, kSource: 'request', f1MaxM: 9.28 },
    verdict: { state: verdictState, model: 'dtm', criterion: 0.6, worstClearanceM: 9.98, worstAtM: 500, worstF1Fraction: 1.07 },
    attribution: 'Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel, CC BY 4.0',
  };
}

function openPair(T, dom, fetchJSON) {
  const tool = T.create({ mount: dom.body, fetchJSON: fetchJSON, syncHash: false,
    resolveNode: (pk) => ({ name: pk.slice(0, 4), lat: 56, lon: 10, role: 'repeater' }) });
  tool.setEndpoint('a', { kind: 'node', pubkey: PK_A });
  tool.setEndpoint('b', { kind: 'node', pubkey: PK_B });
  return tool;
}

atest('a slow answer landing after a model switch never reaches the chip', async () => {
  const { TP: T, dom } = loadLive();
  const fetchJSON = deferredFetch();
  openPair(T, dom, fetchJSON);
  assert.strictEqual(fetchJSON.calls.length, 1, 'the pair did not measure');
  assert.ok(/model=both/.test(fetchJSON.calls[0].url) || fetchJSON.calls[0].url.indexOf('model=') === -1,
    'first call was not the default model: ' + fetchJSON.calls[0].url);

  // Switch to terrain-only while the first request is still in flight.
  dom.find('tp-model-dtm').fire('click');
  assert.strictEqual(fetchJSON.calls.length, 2, 'the model switch did not re-measure');
  assert.ok(/model=dtm/.test(fetchJSON.calls[1].url), 'second call = ' + fetchJSON.calls[1].url);

  // The newer answer lands first and is drawn.
  fetchJSON.calls[1].settle.resolve(answer('clear'));
  await sleep(0);
  assert.strictEqual(chipOf(dom).state, 'clear', 'the dtm answer was not drawn');

  // The older "both" answer now arrives, carrying a different verdict.
  fetchJSON.calls[0].settle.resolve(answer('blocked'));
  await sleep(0);
  assert.strictEqual(chipOf(dom).state, 'clear',
    'a stale answer overwrote the newer one: chip = ' + JSON.stringify(chipOf(dom)));
});

atest('a slow answer landing after a height change never reaches the chip', async () => {
  const { TP: T, dom } = loadLive();
  const fetchJSON = deferredFetch();
  openPair(T, dom, fetchJSON);
  assert.strictEqual(fetchJSON.calls.length, 1);

  // Raise antenna A while the first request is in flight; the stepper is
  // debounced, so the second request only goes out after the delay.
  dom.find('tp-h-a-plus').fire('click');
  await sleep(500);
  assert.strictEqual(fetchJSON.calls.length, 2, 'the height change did not re-measure');
  assert.ok(/ha=1/.test(fetchJSON.calls[1].url), 'the new height was not sent: ' + fetchJSON.calls[1].url);

  fetchJSON.calls[1].settle.resolve(answer('fresnel60'));
  await sleep(0);
  assert.strictEqual(chipOf(dom).state, 'fresnel60', 'the newer answer was not drawn');

  fetchJSON.calls[0].settle.resolve(answer('clear'));
  await sleep(0);
  assert.strictEqual(chipOf(dom).state, 'fresnel60', 'the pre-height answer overwrote the newer one');
});

atest('a stale failure cannot wipe a newer answer either', async () => {
  const { TP: T, dom } = loadLive();
  const fetchJSON = deferredFetch();
  openPair(T, dom, fetchJSON);
  dom.find('tp-model-dtm').fire('click');
  fetchJSON.calls[1].settle.resolve(answer('clear'));
  await sleep(0);
  fetchJSON.calls[0].settle.reject(new Error('upstream timed out'));
  await sleep(0);
  assert.strictEqual(chipOf(dom).state, 'clear', 'a stale rejection replaced the newer answer');
});

atest('closing the panel drops every answer still in flight', async () => {
  const { TP: T, dom } = loadLive();
  const fetchJSON = deferredFetch();
  const tool = openPair(T, dom, fetchJSON);
  tool.close();
  fetchJSON.calls[0].settle.resolve(answer('blocked'));
  await sleep(0);
  assert.notStrictEqual(chipOf(dom).state, 'blocked', 'a closed panel still took an answer');
});

// ── 8. The "assumed heights" caveat (review F10, plan §3.2) ────────────────

console.log('\n=== #681 M4: assumed heights are flagged beside the verdict ===');

atest('the verdict carries an "assumed heights" caveat until both ends are set', async () => {
  const { TP: T, dom } = loadLive();
  const fetchJSON = deferredFetch();
  openPair(T, dom, fetchJSON);
  const withDefaults = answer('clear');
  withDefaults.a.assumed = true;
  withDefaults.a.antennaSource = 'role-default';
  fetchJSON.calls[0].settle.resolve(withDefaults);
  await sleep(0);
  const badge = dom.find('tp-assumed-chip');
  assert.ok(badge, 'no assumed-heights caveat in the panel');
  assert.strictEqual(badge.hidden, false, 'the caveat is hidden while A is a role default');
  assert.strictEqual(badge.textContent, T.ASSUMED_LABEL);
  assert.strictEqual(T.ASSUMED_LABEL, 'assumed heights');
});

atest('the caveat clears once the server confirms both heights', async () => {
  const { TP: T, dom } = loadLive();
  const fetchJSON = deferredFetch();
  openPair(T, dom, fetchJSON);
  const asked = answer('clear');           // both ends assumed:false
  fetchJSON.calls[0].settle.resolve(asked);
  await sleep(0);
  assert.strictEqual(dom.find('tp-assumed-chip').hidden, true,
    'the caveat stayed up with both heights confirmed');
});

atest('one assumed end is enough to caveat the verdict', async () => {
  const { TP: T, dom } = loadLive();
  const fetchJSON = deferredFetch();
  openPair(T, dom, fetchJSON);
  const half = answer('blocked');
  half.b.assumed = true;
  fetchJSON.calls[0].settle.resolve(half);
  await sleep(0);
  assert.strictEqual(dom.find('tp-assumed-chip').hidden, false, 'B being a default was not flagged');
});

// ── 9. #681 M6: the customizer's terrain defaults ──────────────────────────

console.log('\n=== #681 M6: antenna-height defaults per role ===');

/* The customizer section as the panel receives it on window.SITE_CONFIG. */
function terrainCfg(over) {
  return Object.assign({
    heightRepeater: 10, heightRoom: 5, heightCompanion: 2,
    heightSensor: 2, heightPoint: 2, model: 'both',
  }, over || {});
}

test('themeDefaults maps every customizer key onto a role', () => {
  const d = TP.themeDefaults(terrainCfg({
    heightRepeater: 18, heightRoom: 6, heightCompanion: 1.5,
    heightSensor: 3, heightPoint: 4,
  }));
  eq(d.heights, { repeater: 18, room: 6, companion: 1.5, sensor: 3, point: 4 });
  assert.strictEqual(d.model, 'both');
});

test('an absent section is no default at all, and the model falls back', () => {
  const d = TP.themeDefaults(null);
  eq(d.heights, {});
  assert.strictEqual(d.model, 'both');
});

test('a junk customizer value is not a default', () => {
  const d = TP.themeDefaults({ heightRepeater: 'tall', heightRoom: null, model: 'dsm' });
  eq(d.heights, {});
  assert.strictEqual(d.model, 'both', 'dsm is not a model the customizer offers');
});

test('an out-of-range customizer height is refused, not clamped', () => {
  // Review PR#714: the customizer refuses these, so clamping here would draw
  // a height the customizer never showed anybody (400 -> 100 m).
  const d = TP.themeDefaults({ heightRepeater: 400, heightSensor: -3, heightRoom: 100.5, heightCompanion: 100 });
  eq(d.heights, { companion: 100 });
});

test('a hex, exponent or padded customizer height is no default', () => {
  // The reviewer's repro: "0x1F4" was stored as 500 and drawn as 100 m.
  ['0x1F4', '0x10', '5abc', '1e2', ' 5', '-1', '100.5', 'NaN', 'Infinity', NaN, Infinity].forEach((v) => {
    const d = TP.themeDefaults({ heightRepeater: v });
    eq(d.heights, {}, JSON.stringify(String(v)) + ' became a default');
  });
  eq(TP.themeDefaults({ heightRepeater: '12.5', heightPoint: 3 }).heights, { repeater: 12.5, point: 3 });
});

test('themeHeight resolves a response endpoint by its role', () => {
  const d = TP.themeDefaults(terrainCfg({ heightRepeater: 18, heightPoint: 4 }));
  assert.strictEqual(TP.themeHeight({ kind: 'node', role: 'repeater' }, d), 18);
  assert.strictEqual(TP.themeHeight({ kind: 'point' }, d), 4);
});

test('an unrecognised role takes the handheld height, as the server does', () => {
  const d = TP.themeDefaults(terrainCfg({ heightCompanion: 1.8 }));
  assert.strictEqual(TP.themeHeight({ kind: 'node', role: 'observer' }, d), 1.8);
  assert.strictEqual(TP.themeHeight({ kind: 'node', role: undefined }, d), 1.8);
});

test('a customizer height reads as a default, never as yours or a link', () => {
  const badge = TP.heightBadge('request', 'theme');
  assert.ok(badge, 'no badge for a customizer-supplied height');
  assert.strictEqual(badge.label, 'default', 'the M5 badge vocabulary must not grow a fifth word');
  assert.strictEqual(badge.key, 'theme');
  assert.strictEqual(TP.heightBadge('request', 'yours').label, 'yours');
  assert.strictEqual(TP.heightBadge('request', null).label, 'link');
});

test('a customizer height stays an assumption', () => {
  // The server reports assumed:false for any height the request carried, so
  // without this the M5 caveat would vanish the moment an operator edited
  // the customizer — exactly when it is most needed.
  assert.strictEqual(TP.assumedHeight({ assumed: false }, 'theme'), true);
  assert.strictEqual(TP.assumedHeight({ assumed: false }, 'yours'), false);
  assert.strictEqual(TP.assumedHeight({ assumed: true }, null), true);
});

test('the wording distinguishes a typed height from a customizer one', () => {
  const theme = TP.sourceLabel('request', 'theme');
  const yours = TP.sourceLabel('request', 'yours');
  const link = TP.sourceLabel('request', null);
  assert.notStrictEqual(theme, link, 'a customizer default must not read "from this link"');
  assert.notStrictEqual(theme, yours);
  assert.strictEqual(TP.sourceLabel('operator-node', 'theme'), 'set by the operator for this node');
});

console.log('\n=== #681 M6: the default elevation model ===');

test('a link with no model takes the customizer default', () => {
  const st = TP.parseState('profile=' + PK_A + ',' + PK_B, { model: 'dtm' });
  assert.strictEqual(st.model, 'dtm');
});

test('a model spelled out in the link beats the customizer default', () => {
  assert.strictEqual(TP.parseState('profile=' + PK_A + ',' + PK_B + '&model=both', { model: 'dtm' }).model, 'both');
  assert.strictEqual(TP.parseState('profile=' + PK_A + ',' + PK_B + '&model=dtm', { model: 'both' }).model, 'dtm');
});

test('a junk model in the link falls back to the customizer default', () => {
  assert.strictEqual(TP.parseState('profile=' + PK_A + ',' + PK_B + '&model=dsm', { model: 'dtm' }).model, 'dtm');
});

test('the link leaves the model out only for both under a both default', () => {
  const both = { a: { kind: 'node', pubkey: PK_A }, b: { kind: 'node', pubkey: PK_B }, model: 'both' };
  const dtm = Object.assign({}, both, { model: 'dtm' });
  assert.strictEqual(TP.serializeState(both, { model: 'both' }).model, undefined);
  assert.strictEqual(TP.serializeState(dtm, { model: 'both' }).model, 'dtm');
  // With a customizer default of dtm the asymmetry flips: now "both" is the
  // choice that has to be written down, or reopening the link would undo it.
  assert.strictEqual(TP.serializeState(both, { model: 'dtm' }).model, 'both');
  // Review PR#714: anything but "both" is always written down, whatever the
  // sharer's own default is — the recipient may not share it.
  assert.strictEqual(TP.serializeState(dtm, { model: 'dtm' }).model, 'dtm');
});

test('a terrain-only link means terrain-only to a recipient on the standard default', () => {
  // The sharer's customizer default is dtm; the recipient has never opened
  // the customizer. The link must not quietly turn into terrain + surface.
  const state = { a: { kind: 'node', pubkey: PK_A }, b: { kind: 'node', pubkey: PK_B }, model: 'dtm',
    ha: null, hb: null, k: null, freq: null };
  const url = TP.shareURL(state, { origin: 'https://x.test', pathname: '/' }, TP.themeDefaults({ model: 'dtm' }));
  const qs = url.split('#/map?')[1];
  assert.strictEqual(new URLSearchParams(qs).get('model'), 'dtm', 'the link left the model out: ' + url);
  assert.strictEqual(TP.parseState(qs, TP.themeDefaults(null)).model, 'dtm');
  assert.strictEqual(TP.parseState(qs).model, 'dtm');
});

test('a non-default model round-trips through the link under either default', () => {
  [['dtm', 'both'], ['both', 'dtm']].forEach(([def, chosen]) => {
    const state = { a: { kind: 'node', pubkey: PK_A }, b: { kind: 'node', pubkey: PK_B }, model: chosen,
      ha: null, hb: null, k: null, freq: null };
    const qs = new URLSearchParams(TP.serializeState(state, { model: def })).toString();
    assert.strictEqual(TP.parseState(qs, { model: def }).model, chosen, def + ' default, ' + chosen + ' chosen');
  });
});

test('a new panel starts on the customizer model', () => {
  const live = loadLive();
  live.win.SITE_CONFIG = { terrain: terrainCfg({ model: 'dtm' }) };
  const tool = live.TP.create({ mount: live.dom.body, fetchJSON: deferredFetch(), syncHash: false });
  assert.strictEqual(tool.getState().model, 'dtm');
});

console.log('\n=== #681 M6: height precedence ===');

/* A response whose endpoints the server answered out of its own built-in
 * role defaults — the one case a customizer default may replace. */
function defaultsAnswer(over) {
  const a = answer('clear');
  a.a = { kind: 'node', role: 'repeater', groundM: 0, antennaM: 10, antennaSource: 'role-default', assumed: true };
  a.b = { kind: 'node', role: 'repeater', groundM: 0, antennaM: 10, antennaSource: 'role-default', assumed: true };
  return Object.assign(a, over || {});
}

function askedHeights(call) {
  const q = new URLSearchParams(String(call.url).split('?')[1]);
  return { ha: q.get('ha'), hb: q.get('hb') };
}

atest('a customizer height replaces the server built-in default', async () => {
  const live = loadLive();
  live.win.SITE_CONFIG = { terrain: terrainCfg({ heightRepeater: 25 }) };
  const f = deferredFetch();
  openPair(live.TP, live.dom, f);
  eq(askedHeights(f.calls[0]), { ha: null, hb: null }, 'the first ask must let the server speak first');
  f.calls[0].settle.resolve(defaultsAnswer());
  await sleep(0);
  assert.strictEqual(f.calls.length, 2, 'the customizer default was never applied');
  eq(askedHeights(f.calls[1]), { ha: '25', hb: '25' });
});

atest('the badge on a customizer height is "default", and it stays assumed', async () => {
  const live = loadLive();
  live.win.SITE_CONFIG = { terrain: terrainCfg({ heightRepeater: 25 }) };
  const f = deferredFetch();
  openPair(live.TP, live.dom, f);
  f.calls[0].settle.resolve(defaultsAnswer());
  await sleep(0);
  const second = defaultsAnswer();
  second.a = { kind: 'node', role: 'repeater', groundM: 0, antennaM: 25, antennaSource: 'request', assumed: false };
  second.b = Object.assign({}, second.a);
  f.calls[1].settle.resolve(second);
  await sleep(0);
  const badge = live.dom.find('tp-hsrc-a');
  assert.strictEqual(badge.textContent, 'default', 'a customizer height must not read as yours or a link');
  assert.strictEqual(live.dom.find('tp-assumed-chip').hidden, false,
    'a class-wide default is still an assumption');
  assert.strictEqual(live.dom.find('tp-h-a').value, '25');
});

atest('an operator height is never overruled by the customizer', async () => {
  for (const source of ['operator-node', 'operator-role']) {
    const live = loadLive();
    live.win.SITE_CONFIG = { terrain: terrainCfg({ heightRepeater: 25 }) };
    const f = deferredFetch();
    openPair(live.TP, live.dom, f);
    const data = defaultsAnswer();
    data.a.antennaSource = source;
    data.a.antennaM = 18;
    data.b.antennaSource = source;
    data.b.antennaM = 18;
    f.calls[0].settle.resolve(data);
    await sleep(0);
    assert.strictEqual(f.calls.length, 1, source + ' was re-asked with a customizer default');
    assert.strictEqual(live.dom.find('tp-hsrc-a').textContent, 'operator');
  }
});

atest('a remembered height outranks the customizer default', async () => {
  const live = loadLive();
  live.win.SITE_CONFIG = { terrain: terrainCfg({ heightRepeater: 25 }) };
  live.win.localStorage.setItem('mc-terrain-h:' + PK_A, '7');
  const f = deferredFetch();
  openPair(live.TP, live.dom, f);
  eq(askedHeights(f.calls[0]), { ha: '7', hb: null }, 'the remembered height was not sent');
  const data = defaultsAnswer();
  data.a = { kind: 'node', role: 'repeater', groundM: 0, antennaM: 7, antennaSource: 'request', assumed: false };
  f.calls[0].settle.resolve(data);
  await sleep(0);
  assert.strictEqual(f.calls.length, 2, 'B still had a built-in default to replace');
  eq(askedHeights(f.calls[1]), { ha: '7', hb: '25' }, 'A was overwritten by the customizer');
  const second = defaultsAnswer();
  second.a = { kind: 'node', role: 'repeater', groundM: 0, antennaM: 7, antennaSource: 'request', assumed: false };
  second.b = { kind: 'node', role: 'repeater', groundM: 0, antennaM: 25, antennaSource: 'request', assumed: false };
  f.calls[1].settle.resolve(second);
  await sleep(0);
  assert.strictEqual(live.dom.find('tp-hsrc-a').textContent, 'yours');
  assert.strictEqual(live.dom.find('tp-hsrc-b').textContent, 'default');
});

atest('a height on the link outranks the customizer default', async () => {
  const live = loadLive();
  live.win.SITE_CONFIG = { terrain: terrainCfg({ heightRepeater: 25 }) };
  const f = deferredFetch();
  const tool = live.TP.create({ mount: live.dom.body, fetchJSON: f, syncHash: false,
    resolveNode: (pk) => ({ name: pk.slice(0, 4), lat: 56, lon: 10, role: 'repeater' }) });
  tool.setState(live.TP.parseState('profile=' + PK_A + ',' + PK_B + '&ha=3&hb=4'));
  eq(askedHeights(f.calls[0]), { ha: '3', hb: '4' });
  const data = defaultsAnswer();
  data.a.antennaSource = 'request';
  data.b.antennaSource = 'request';
  f.calls[0].settle.resolve(data);
  await sleep(0);
  assert.strictEqual(f.calls.length, 1, 'a link height was re-asked');
  assert.strictEqual(live.dom.find('tp-hsrc-a').textContent, 'link');
});

atest('a customizer height equal to the server default costs no second ask', async () => {
  const live = loadLive();
  live.win.SITE_CONFIG = { terrain: terrainCfg() }; // the built-in 10 m repeater
  const f = deferredFetch();
  openPair(live.TP, live.dom, f);
  f.calls[0].settle.resolve(defaultsAnswer());
  await sleep(0);
  assert.strictEqual(f.calls.length, 1, 'an untouched customizer must change nothing');
  assert.strictEqual(live.dom.find('tp-hsrc-a').textContent, 'default');
});

atest('the repeat happens at most once, whatever the server answers', async () => {
  const live = loadLive();
  live.win.SITE_CONFIG = { terrain: terrainCfg({ heightRepeater: 25 }) };
  const f = deferredFetch();
  openPair(live.TP, live.dom, f);
  f.calls[0].settle.resolve(defaultsAnswer());
  await sleep(0);
  // A server that keeps answering "role-default" must not start a loop.
  f.calls[1].settle.resolve(defaultsAnswer());
  await sleep(0);
  assert.strictEqual(f.calls.length, 2, 'the adoption pass looped');
});

atest('a customizer height never reaches the shared link', async () => {
  const live = loadLive();
  live.win.SITE_CONFIG = { terrain: terrainCfg({ heightRepeater: 25 }) };
  const f = deferredFetch();
  const tool = openPair(live.TP, live.dom, f);
  f.calls[0].settle.resolve(defaultsAnswer());
  await sleep(0);
  const st = tool.getState();
  assert.strictEqual(st.ha, null, 'an ambient default became part of the state');
  assert.strictEqual(st.hb, null);
  const url = live.TP.shareURL(st, { origin: 'https://x.test', pathname: '/' }, live.TP.themeDefaults(terrainCfg({ heightRepeater: 25 })));
  assert.strictEqual(url.indexOf('ha='), -1, 'a customizer default was written into the link');
});

atest('typing a height drops the customizer default for that end', async () => {
  const live = loadLive();
  live.win.SITE_CONFIG = { terrain: terrainCfg({ heightRepeater: 25 }) };
  const f = deferredFetch();
  openPair(live.TP, live.dom, f);
  f.calls[0].settle.resolve(defaultsAnswer());
  await sleep(0);
  const input = live.dom.find('tp-h-a');
  input.value = '31';
  input.fire('change');
  await sleep(400); // the height debounce
  const last = f.calls[f.calls.length - 1];
  eq(askedHeights(last), { ha: '31', hb: '25' });
  const typed = defaultsAnswer();
  typed.a = { kind: 'node', role: 'repeater', groundM: 0, antennaM: 31, antennaSource: 'request', assumed: false };
  typed.b = { kind: 'node', role: 'repeater', groundM: 0, antennaM: 25, antennaSource: 'request', assumed: false };
  last.settle.resolve(typed);
  await sleep(0);
  assert.strictEqual(live.dom.find('tp-hsrc-a').textContent, 'yours');
});

console.log('\n=== #681 M6: the customizer moves while the panel is open ===');

atest('a changed default re-measures an open profile', async () => {
  const live = loadLive();
  live.win.SITE_CONFIG = { terrain: terrainCfg() };
  const f = deferredFetch();
  const tool = openPair(live.TP, live.dom, f);
  f.calls[0].settle.resolve(defaultsAnswer());
  await sleep(0);
  assert.strictEqual(f.calls.length, 1);
  live.win.SITE_CONFIG = { terrain: terrainCfg({ heightRepeater: 30, model: 'dtm' }) };
  tool.defaultsChangedForTests();
  assert.strictEqual(tool.getState().model, 'dtm', 'the model default did not follow');
  f.calls[1].settle.resolve(defaultsAnswer());
  await sleep(0);
  eq(askedHeights(f.calls[2]), { ha: '30', hb: '30' });
});

atest('a second customizer change replaces the height the first one adopted', async () => {
  // Review PR#714 R10: changing the default twice with the panel open must
  // not keep the height adopted from the first change.
  const live = loadLive();
  live.win.SITE_CONFIG = { terrain: terrainCfg() };
  const f = deferredFetch();
  const tool = openPair(live.TP, live.dom, f);
  f.calls[0].settle.resolve(defaultsAnswer());
  await sleep(0);
  live.win.SITE_CONFIG = { terrain: terrainCfg({ heightRepeater: 30 }) };
  tool.defaultsChangedForTests();
  f.calls[1].settle.resolve(defaultsAnswer());
  await sleep(0);
  eq(askedHeights(f.calls[2]), { ha: '30', hb: '30' }, 'the first change was not adopted');
  const at30 = defaultsAnswer();
  at30.a = { kind: 'node', role: 'repeater', groundM: 0, antennaM: 30, antennaSource: 'request', assumed: false };
  at30.b = Object.assign({}, at30.a);
  f.calls[2].settle.resolve(at30);
  await sleep(0);
  assert.strictEqual(live.dom.find('tp-h-a').value, '30');

  live.win.SITE_CONFIG = { terrain: terrainCfg({ heightRepeater: 45 }) };
  tool.defaultsChangedForTests();
  eq(askedHeights(f.calls[3]), { ha: null, hb: null }, 'the stale 30 m default was sent again');
  f.calls[3].settle.resolve(defaultsAnswer());
  await sleep(0);
  assert.strictEqual(f.calls.length, 5, 'the second change was never adopted');
  eq(askedHeights(f.calls[4]), { ha: '45', hb: '45' });
  const at45 = defaultsAnswer();
  at45.a = { kind: 'node', role: 'repeater', groundM: 0, antennaM: 45, antennaSource: 'request', assumed: false };
  at45.b = Object.assign({}, at45.a);
  f.calls[4].settle.resolve(at45);
  await sleep(0);
  assert.strictEqual(live.dom.find('tp-h-a').value, '45');
  assert.strictEqual(live.dom.find('tp-hsrc-a').textContent, 'default');
});

atest('a model the viewer chose survives a customizer change', async () => {
  const live = loadLive();
  live.win.SITE_CONFIG = { terrain: terrainCfg({ model: 'both' }) };
  const f = deferredFetch();
  const tool = openPair(live.TP, live.dom, f);
  f.calls[0].settle.resolve(defaultsAnswer());
  await sleep(0);
  live.dom.find('tp-model-dtm').fire('click');
  assert.strictEqual(tool.getState().model, 'dtm');
  live.win.SITE_CONFIG = { terrain: terrainCfg({ model: 'both' }) };
  tool.defaultsChangedForTests();
  assert.strictEqual(tool.getState().model, 'dtm', 'the viewer’s own pick was overwritten');
});

atest('destroy unhooks the customizer listener', async () => {
  const live = loadLive();
  const hooks = [];
  live.win.addEventListener = (t, fn) => hooks.push([t, fn]);
  live.win.removeEventListener = (t, fn) => {
    const i = hooks.findIndex(([ht, hf]) => ht === t && hf === fn);
    if (i >= 0) hooks.splice(i, 1);
  };
  const tool = live.TP.create({ mount: live.dom.body, fetchJSON: deferredFetch(), syncHash: false });
  assert.ok(hooks.some(([t]) => t === 'terrain-defaults-changed'), 'the tool never subscribed');
  tool.destroy();
  assert.ok(!hooks.some(([t]) => t === 'terrain-defaults-changed'), 'the subscription outlived the tool');
});

// ── Summary ────────────────────────────────────────────────────────────────

(async () => {
  for (const [name, fn] of asyncTests) {
    try { await fn(); passed++; console.log('  ✅ ' + name); }
    catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
  }
  console.log(`\n#681 M4/M6 unit: ${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
