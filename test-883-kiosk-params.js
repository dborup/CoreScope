/* test-883-kiosk-params.js
 * #883 M1 — Live kiosk: the URL and the tour steps. Unit tests for the real
 * public/live-kiosk.js in a vm sandbox (no DOM, no browser):
 *   parseKioskParams()  kiosk / tour / every
 *   areaBounds()        polygon vs. box, degenerate geometry
 *   resolveSteps()      exact key, then case-insensitive key, never label
 *   resumeAfterMs()     max(60 s, 2 × every), capped at 5 min
 *   stripKioskParams()  the exit hash keeps every other param
 */
'use strict';
const fs = require('fs');
const path = require('path');
const vm = require('vm');
const assert = require('assert');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + '\n    ' + (e && e.stack || e)); }
}

function load() {
  const ctx = { window: {}, console, Date, Math, Number, String, JSON, Array, Object, Set, Map, Promise,
    isFinite, isNaN, parseInt, URLSearchParams };
  vm.createContext(ctx);
  const f = 'public/live-kiosk.js';
  vm.runInContext(fs.readFileSync(path.join(__dirname, f), 'utf8'), ctx, { filename: f });
  return ctx.window.LiveKiosk;
}
const K = load();
const P = (s) => K.parseKioskParams(new URLSearchParams(s));
// Arrays and objects come from the sandbox realm; compare as plain JSON.
const eq = (a, b, m) => assert.deepStrictEqual(JSON.parse(JSON.stringify(a)), b, m);

console.log('\n=== #883 live-kiosk: parseKioskParams ===');

test('kiosk: only the exact value 1 turns it on', () => {
  assert.strictEqual(P('kiosk=1').kiosk, true);
  for (const v of ['', '0', 'true', 'yes', '01', ' 1', '1 ', '2', 'on']) {
    assert.strictEqual(P('kiosk=' + encodeURIComponent(v)).kiosk, false, 'kiosk=' + JSON.stringify(v));
  }
  assert.strictEqual(P('').kiosk, false, 'absent');
  assert.strictEqual(K.parseKioskParams(null).kiosk, false, 'no params at all');
});

test('defaults: no tour, every 60 s', () => {
  eq(P('kiosk=1'), { kiosk: true, tour: [], every: 60 });
  eq(K.parseKioskParams(null), { kiosk: false, tour: [], every: 60 });
});

test('every: parseInt, NaN or empty is 60, clamped to [15, 3600], fractions floored', () => {
  const cases = { '': 60, 'abc': 60, '5': 15, '15': 15, '14': 15, '60': 60, '60.9': 60, '90': 90,
    '3600': 3600, '3601': 3600, '99999': 3600, '-5': 15, '-0': 15, '0': 15, ' 45': 45 };
  for (const [raw, want] of Object.entries(cases)) {
    assert.strictEqual(P('kiosk=1&every=' + encodeURIComponent(raw)).every, want, 'every=' + JSON.stringify(raw));
  }
  assert.strictEqual(P('kiosk=1').every, 60, 'absent');
  assert.strictEqual(K.EVERY_MIN, 15);
  assert.strictEqual(K.EVERY_MAX, 3600);
  assert.strictEqual(K.EVERY_DEFAULT, 60);
});

test('tour: trimmed, empties dropped, URL order, duplicates kept', () => {
  eq(P('kiosk=1&tour=' + encodeURIComponent(' fyn , ,JYLLAND,,fyn,')).tour, ['fyn', 'JYLLAND', 'fyn']);
  eq(P('kiosk=1&tour=').tour, []);
  eq(P('kiosk=1&tour=,,,').tour, []);
});

test('tour: at most 12 steps', () => {
  const keys = Array.from({ length: 15 }, (_, i) => 'a' + i);
  const t = P('kiosk=1&tour=' + keys.join(',')).tour;
  assert.strictEqual(K.MAX_STEPS, 12);
  eq(t, keys.slice(0, 12));
});

test('tour and every are parsed even without kiosk (the caller decides)', () => {
  eq(P('tour=a,b&every=30'), { kiosk: false, tour: ['a', 'b'], every: 30 });
});

console.log('\n=== #883 live-kiosk: areaBounds ===');

test('a polygon with ≥3 points wins over the box', () => {
  const b = K.areaBounds({ polygon: [[55.0, 11.0], [56.0, 12.5], [55.5, 10.5]], latMin: 1, latMax: 2, lonMin: 1, lonMax: 2 });
  eq(b, [[55.0, 10.5], [56.0, 12.5]]);
});

test('no polygon: the full box', () => {
  eq(K.areaBounds({ latMin: 37.2, latMax: 37.45, lonMin: -122.05, lonMax: -121.75 }), [[37.2, -122.05], [37.45, -121.75]]);
});

test('a polygon with fewer than 3 points falls back to the box, or is null', () => {
  eq(K.areaBounds({ polygon: [[1, 1], [2, 2]], latMin: 10, latMax: 11, lonMin: 20, lonMax: 21 }), [[10, 20], [11, 21]]);
  assert.strictEqual(K.areaBounds({ polygon: [[1, 1], [2, 2]] }), null);
});

test('degenerate geometry is null: partial box, inverted box, zero-area polygon, non-numbers, out of range', () => {
  assert.strictEqual(K.areaBounds(null), null);
  assert.strictEqual(K.areaBounds({}), null);
  assert.strictEqual(K.areaBounds({ latMin: 1, latMax: 2, lonMin: 3 }), null, 'partial box');
  assert.strictEqual(K.areaBounds({ latMin: 2, latMax: 1, lonMin: 3, lonMax: 4 }), null, 'inverted lat');
  assert.strictEqual(K.areaBounds({ latMin: 1, latMax: 1, lonMin: 3, lonMax: 4 }), null, 'zero height');
  assert.strictEqual(K.areaBounds({ latMin: '1', latMax: 2, lonMin: 3, lonMax: 4 }), null, 'string');
  assert.strictEqual(K.areaBounds({ latMin: -95, latMax: 2, lonMin: 3, lonMax: 4 }), null, 'lat out of range');
  assert.strictEqual(K.areaBounds({ polygon: [[5, 5], [5, 5], [5, 5]] }), null, 'zero-area polygon');
  assert.strictEqual(K.areaBounds({ polygon: [[5, 5], [6, 'x'], [7, 7]] }), null, 'bad point');
  assert.strictEqual(K.areaBounds({ polygon: [[5, 5], [6, 190], [7, 7]] }), null, 'lon out of range');
  assert.strictEqual(K.areaBounds({ polygon: 'nope' }), null);
});

console.log('\n=== #883 live-kiosk: resolveSteps ===');

// The server ranges over a Go map, so the response order is random: the
// steps follow the URL, never the response.
const AREAS = [
  { key: 'jylland', label: 'Jylland', latMin: 54.8, latMax: 57.8, lonMin: 8.0, lonMax: 10.9 },
  { key: 'SJ', label: 'Sjælland og øerne', polygon: [[54.5, 10.9], [56.2, 12.7], [55.2, 12.7]] },
  { key: 'Fyn', label: 'Fyn', latMin: 54.9, latMax: 55.6, lonMin: 9.7, lonMax: 10.9 },
  { key: 'fyn', label: 'other fyn', latMin: 1, latMax: 2, lonMin: 1, lonMax: 2 },
  { key: 'thin', label: 'Thin', latMin: 1, latMax: 2 },
];

test('URL order, exact key first, then case-insensitive key', () => {
  const r = K.resolveSteps(['sj', 'Fyn', 'JYLLAND'], AREAS);
  eq(r.steps.map((s) => s.key), ['SJ', 'Fyn', 'jylland']);
  eq(r.steps.map((s) => s.label), ['Sjælland og øerne', 'Fyn', 'Jylland']);
  eq(r.steps[2].bounds, [[54.8, 8.0], [57.8, 10.9]]);
  eq(r.skipped, []);
});

test('exact key wins over a case-insensitive twin; ties pick the smallest key', () => {
  eq(K.resolveSteps(['fyn'], AREAS).steps.map((s) => s.label), ['other fyn']);
  eq(K.resolveSteps(['FYN'], AREAS).steps.map((s) => s.key), ['Fyn']);
  // Same answer whatever the response order.
  eq(K.resolveSteps(['FYN'], AREAS.slice().reverse()).steps.map((s) => s.key), ['Fyn']);
});

test('never matched by label', () => {
  const r = K.resolveSteps(['Jylland', 'Sjælland og øerne'], AREAS);
  eq(r.steps.map((s) => s.key), ['jylland']);
  eq(r.skipped, [{ key: 'Sjælland og øerne', reason: 'unknown' }]);
});

test('unknown keys and degenerate areas are skipped with a reason; duplicates stay', () => {
  const r = K.resolveSteps(['nope', 'Fyn', 'thin', 'Fyn'], AREAS);
  eq(r.steps.map((s) => s.key), ['Fyn', 'Fyn']);
  eq(r.skipped, [{ key: 'nope', reason: 'unknown' }, { key: 'thin', reason: 'geometry' }]);
});

test('an empty label falls back to the key', () => {
  eq(K.resolveSteps(['x'], [{ key: 'x', label: '', latMin: 1, latMax: 2, lonMin: 1, lonMax: 2 }]).steps[0].label, 'x');
});

test('a non-array areas response gives no steps and skips nothing', () => {
  for (const bad of [null, undefined, {}, 'x']) {
    eq(K.resolveSteps(['a', 'b'], bad), { steps: [], skipped: [] });
  }
});

test('junk entries in the areas response are ignored', () => {
  const r = K.resolveSteps(['a'], [null, 5, { label: 'no key' }, { key: 7 }, { key: 'a', latMin: 1, latMax: 2, lonMin: 1, lonMax: 2 }]);
  eq(r.steps.map((s) => s.key), ['a']);
});

console.log('\n=== #883 live-kiosk: resumeAfterMs / stripKioskParams ===');

test('resumeAfter = max(60 s, 2 × every), capped at 5 min', () => {
  assert.strictEqual(K.resumeAfterMs(15), 60000);
  assert.strictEqual(K.resumeAfterMs(30), 60000);
  assert.strictEqual(K.resumeAfterMs(60), 120000);
  assert.strictEqual(K.resumeAfterMs(120), 240000);
  assert.strictEqual(K.resumeAfterMs(150), 300000);
  assert.strictEqual(K.resumeAfterMs(3600), 300000);
});

test('stripKioskParams drops kiosk, tour and every and keeps the rest', () => {
  assert.strictEqual(K.stripKioskParams('#/live?kiosk=1&tour=a,b&every=60'), '#/live');
  assert.strictEqual(K.stripKioskParams('#/live?kiosk=1&region=CPH&tour=a&every=30&heard=24h'), '#/live?region=CPH&heard=24h');
  assert.strictEqual(K.stripKioskParams('#/live?kiosk=1'), '#/live');
  assert.strictEqual(K.stripKioskParams('#/live'), '#/live');
  assert.strictEqual(K.stripKioskParams(''), '#/live');
});

console.log(`\n${passed} passed, ${failed} failed`);
process.exit(failed ? 1 : 0);
