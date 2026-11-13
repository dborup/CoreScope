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
 *   6. XSS escaping of node names in the chart <title>.
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

test('requestURL omits unset controls', () => {
  const url = TP.requestURL(TP.parseState('profile=' + PK_A + ',' + PK_B));
  const q = new URLSearchParams(url.split('?')[1]);
  eq(Array.from(q.keys()).sort(), ['a', 'b']);
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

test('a missing response is unknown, not a crash', () => {
  assert.strictEqual(TP.verdictChip(null).state, 'unknown');
  assert.strictEqual(TP.verdictChip({}).state, 'unknown');
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

test('the chart footer carries the CC BY 4.0 attribution', () => {
  const svg = TP.buildChart(flatPath(), {});
  assert.ok(svg.indexOf('Indeholder data fra Klimadatastyrelsen') >= 0, 'missing attribution');
  assert.strictEqual(TP.ATTRIBUTION, 'Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel, CC BY 4.0');
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

// ── Summary ────────────────────────────────────────────────────────────────

console.log(`\n#681 M4 unit: ${passed} passed, ${failed} failed`);
process.exit(failed ? 1 : 0);
