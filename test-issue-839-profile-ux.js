/**
 * Unit tests (#839): the Path profile fixes from the rc19 test pass, in
 * public/terrain-profile.js — the pure rules the Map page and the panel use.
 *
 *   §2 panelCap: the panel never leaves less than --tp-map-keep of map, the
 *      same rule as the .tp-panel max-height in style.css;
 *      fitPadding's left inset and fitPaddingBeside (the docked panel);
 *   §3 endsVisible: an end under the panel, off the map or under a button
 *      is hidden;
 *   §6 refusal: "path too long" with the limit in km;
 *   §7 invalidLink: a profile link neither reader accepts;
 *   §8 fmtDistance: a distance along the path is never "8141.4 m".
 *
 * Usage: node test-issue-839-profile-ux.js
 */
'use strict';
const assert = require('assert');
const fs = require('fs');
const vm = require('vm');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
// The module runs in its own vm realm: compare values, not prototypes.
const eq = (a, b) => assert.deepStrictEqual(JSON.parse(JSON.stringify(a)), JSON.parse(JSON.stringify(b)));

function load() {
  const win = { addEventListener() {}, removeEventListener() {}, dispatchEvent() {} };
  const ctx = {
    window: win, console, Math, Date, JSON, Array, Object, String, Number,
    RegExp, Error, TypeError, Boolean, isNaN, isFinite, parseInt, parseFloat,
    URLSearchParams, Map, Set, Promise, encodeURIComponent, decodeURIComponent,
    setTimeout: () => 0, clearTimeout: () => {},
    document: { createElement: () => ({}), addEventListener() {}, querySelector: () => null },
  };
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync('public/antenna-height.js', 'utf8'), ctx);
  vm.runInContext(fs.readFileSync('public/terrain-profile.js', 'utf8'), ctx);
  vm.runInContext(fs.readFileSync('public/terrain-profile-cursor.js', 'utf8'), ctx);
  return { TP: ctx.window.TerrainProfile, C: ctx.window.TerrainProfileCursor };
}
const { TP, C } = load();
const M = TP.FIT_MARGIN_PX;

console.log('\n=== #839 §2: the panel leaves a strip of map ===');

test('panelCap: a tall map keeps the CSS cap', () => {
  eq(TP.panelCap(848, 420, 184, 120), 420);
  eq(TP.panelCap(632, 420, 184, 120), 420);
});

test('panelCap: a phone keeps --tp-map-keep of map above the panel (375x560: 452 px map)', () => {
  eq(TP.panelCap(452, 420, 184, 120), 268);
  eq(TP.panelCap(559, 420, 184, 120), 375);
});

test('panelCap: never under the floor', () => {
  eq(TP.panelCap(252, 420, 184, 120), 120);
});

test('panelCap: unreadable variables fall back to the cap, as before', () => {
  eq(TP.panelCap(452, 420, NaN, 120), 420);
  assert.ok(Number.isNaN(TP.panelCap(452, NaN, NaN, NaN)));
});

test('the phone fit now has its strip: 375x560 reserves the capped panel', () => {
  const cap = TP.panelCap(452, 420, 184, 120);
  eq(TP.fitPadding(452, 90, cap).paddingBottomRight, [M, cap + M]);
  // Before #839 the 420 px cap left no strip, and the panel was not reserved.
  eq(TP.fitPadding(452, 90, 420).paddingBottomRight, [M, M]);
});

test('style.css: the grown panel is capped by the same three variables, and the keep is the fit\'s strip', () => {
  const css = fs.readFileSync('public/style.css', 'utf8');
  assert.ok(/max-height:\s*min\(var\(--tp-panel-max-h\),\s*max\(var\(--tp-panel-min-h\),\s*calc\(100% - var\(--tp-map-keep\)\)\)\)/.test(css),
    'the .tp-panel max-height is not panelCap\'s rule');
  const keep = Number((css.match(/--tp-map-keep:\s*(\d+)px/) || [])[1]);
  const floor = Number((css.match(/--tp-panel-min-h:\s*(\d+)px/) || [])[1]);
  assert.ok(keep > 0 && floor > 0, 'variables missing');
  // At the floor of the strip, fitPadding still reserves the panel.
  const mapH = 452, cap = TP.panelCap(mapH, 420, keep, floor);
  assert.ok(TP.fitPadding(mapH, cap, cap).paddingBottomRight[1] > M, 'the strip --tp-map-keep leaves is too small to fit into');
});

test('fitPadding: the left inset clears the map buttons; without one it is the margin', () => {
  eq(TP.fitPadding(848, 295, 420).paddingTopLeft, [M, M]);
  eq(TP.fitPadding(848, 295, 420, 62).paddingTopLeft, [62 + M / 2, M]);
  eq(TP.fitPadding(848, 295, 420, 10).paddingTopLeft, [M, M]);
});

test('fitPadding: the right inset clears the open Map Controls', () => {
  eq(TP.fitPadding(848, 295, 420, 62, 240).paddingBottomRight, [240 + M / 2, 452]);
  eq(TP.fitPadding(848, 295, 420, 62, 0).paddingBottomRight, [M, 452]);
});

test('fitPaddingBeside: a docked panel costs right padding only', () => {
  eq(TP.fitPaddingBeside(640, 384, 62), { paddingTopLeft: [62 + M / 2, M], paddingBottomRight: [384 + M, M] });
  // Too narrow beside it: not reserved.
  eq(TP.fitPaddingBeside(420, 384, 0).paddingBottomRight, [M, M]);
});

console.log('\n=== #839 §3: is an end in view? ===');

const size = { x: 1408, y: 848 };
const pad = TP.fitPadding(848, 420, 420, 62);
const zoom = { left: 10, top: 10, right: 62, bottom: 110 };

test('endsVisible: both ends above the panel', () => {
  assert.strictEqual(TP.endsVisible([{ x: 300, y: 100 }, { x: 900, y: 400 }], size, pad, [zoom]), true);
});

test('endsVisible: an end where the panel grows to is hidden', () => {
  assert.strictEqual(TP.endsVisible([{ x: 300, y: 100 }, { x: 900, y: 540 }], size, pad, [zoom]), false);
});

test('endsVisible: off the map, or under the zoom buttons, is hidden', () => {
  assert.strictEqual(TP.endsVisible([{ x: -5, y: 100 }, { x: 900, y: 100 }], size, pad, []), false);
  assert.strictEqual(TP.endsVisible([{ x: 40, y: 60 }, { x: 900, y: 100 }], size, pad, [zoom]), false);
});

test('endsVisible: near an edge, but on the map, is in view (the fit margin is not required)', () => {
  assert.strictEqual(TP.endsVisible([{ x: 80, y: 8 }, { x: 1400, y: 420 }], size, pad, [zoom]), true);
});

test('endsVisible: beside a docked panel', () => {
  const side = TP.fitPaddingBeside(640, 384, 62);
  assert.strictEqual(TP.endsVisible([{ x: 100, y: 150 }, { x: 240, y: 40 }], { x: 640, y: 252 }, side, []), true);
  assert.strictEqual(TP.endsVisible([{ x: 100, y: 150 }, { x: 300, y: 40 }], { x: 640, y: 252 }, side, []), false);
});

console.log('\n=== #839 §6: a path that is too long ===');

const tooLong = (km) => Object.assign(new Error('path too long'), km === undefined ? {} : { maxPathKm: km });

test('refusal names the limit in km', () => {
  const r = TP.refusal(tooLong(80));
  eq(r.label, 'Too long');
  assert.ok(/too long/i.test(r.text) && r.text.indexOf('up to 80 km') >= 0, r.text);
  assert.ok(TP.refusal(tooLong(12.5)).text.indexOf('up to 12.5 km') >= 0);
});

test('refusal without a limit (an older server) still reads as a sentence', () => {
  const r = TP.refusal(tooLong());
  assert.ok(r && /too long/i.test(r.text) && !/undefined|NaN|up to/.test(r.text), r && r.text);
});

test('any other error is not a refusal', () => {
  eq(TP.refusal(new Error('node not found')), null);
  eq(TP.refusal(null), null);
});

console.log('\n=== #839 §7: a profile link that cannot be read ===');

test('invalidLink: junk, half a pair, out-of-range coordinates', () => {
  ['profile=garbage', 'profile=56.1,10.2', 'profile=91,10,56,10', 'profile=', '?profile=a,b,c,d']
    .forEach(q => assert.strictEqual(TP.invalidLink(q), true, q));
});

test('invalidLink: a full link, a half (node) link and no profile at all are not invalid', () => {
  const pk = 'ab'.repeat(32);
  ['profile=56.1,10.2,56.2,10.3', 'profile=' + pk, 'lat=56&lon=10&zoom=9', '']
    .forEach(q => assert.strictEqual(TP.invalidLink(q), false, q));
});

test('withProfileParams drops the junk and keeps the rest of the hash', () => {
  eq(TP.withProfileParams('#/map?profile=garbage', {}), '#/map');
  eq(TP.withProfileParams('#/map?lat=56&profile=garbage&ha=3', {}), '#/map?lat=56');
});

console.log('\n=== #839 §8: distances along the path ===');

test('fmtDistance: metres under 1 km, two decimals to 10 km, one beyond', () => {
  const cases = { 0: '0 m', 12.4: '12 m', 999.4: '999 m', 999.6: '1.00 km', 3912.3: '3.91 km', 8141.4: '8.14 km',
    9994.9: '9.99 km', 9999.9: '10.0 km', 10000: '10.0 km', 11150: '11.2 km', 80000: '80.0 km' };
  Object.keys(cases).forEach(k => eq(TP.fmtDistance(Number(k)), cases[k]));
  eq(TP.fmtDistance(NaN), '—');
});

test('the chart, its title and the hover readout share it', () => {
  const D = 8141.4, samples = [];
  for (let i = 0; i < 20; i++) samples.push({ d: Math.round(D * i / 19 * 10) / 10, dtm: 30, dsm: null });
  const data = {
    a: { antennaM: 10, groundM: 30 }, b: { antennaM: 10, groundM: 30 }, distanceM: D, model: 'dtm', samples,
    coverage: { state: 'full', pct: 100 }, physics: { lambdaM: 0.345, k: 4 / 3 },
    verdict: { state: 'clear', criterion: 0.6, worstClearanceM: 9.6, worstAtM: 4070.7, worstF1Fraction: 1.2 },
  };
  const html = TP.buildChart(data, {});
  assert.ok(html.indexOf('>8.14 km</span>') >= 0, 'axis end is not 8.14 km');
  assert.ok(html.indexOf('8141.4') < 0, 'a decimetre distance is left in the chart');
  assert.ok(/, 8\.14 km\./.test(html) && /at 4\.07 km along the path/.test(html), 'chart title');
  const g = TP.chartGeometry(data);
  eq(C.readout(g, data, 19).lines[0], '8.14 km from A');
});

console.log(`\n${passed} passed, ${failed} failed\n`);
process.exit(failed ? 1 : 0);
