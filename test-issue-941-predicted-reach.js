/* test-issue-941-predicted-reach.js — public/coverage.js, the Predicted
 * reach tool (#941 M1), without a browser.
 *
 * Covers:
 *   1. the #/map?reach=… link: parse ↔ serialise round trip, junk
 *      rejected (the centre) or dropped (an optional value), other hash
 *      parameters kept;
 *   2. the request: the centre and the geometry only — power, gains,
 *      cable loss and sensitivity never reach the server;
 *   3. the link budget and the margin classes at their boundaries;
 *   4. the 256-entry colour table and the paint through it;
 *   5. the panel's refusal words (no server text echoed);
 *   6. the node-detail chip's gate and DOM;
 *   7. the page wiring: the script tag, the CSS variables in all three
 *      theme blocks, no hard-coded colour in the module;
 *   8. a recolour of a 40 km raster stays well inside the 50 ms budget.
 *
 * Runs via: node test-issue-941-predicted-reach.js
 */
'use strict';
const fs = require('fs');
const path = require('path');
const vm = require('vm');
const assert = require('assert');
const { makeElement } = require('./tests/map-overlay-sandbox');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

const SRC = fs.readFileSync(path.join(__dirname, 'public', 'coverage.js'), 'utf8');
function load() {
  const ctx = {
    window: null, document: { createElement: makeElement }, URLSearchParams, Uint8ClampedArray, Uint32Array,
    Uint8Array, Math, Number, String, Object, Array, isFinite, encodeURIComponent, console,
  };
  ctx.window = ctx;
  vm.createContext(ctx);
  vm.runInContext(SRC, ctx);
  return ctx.PredictedReach;
}
const PR = load();
const PK = 'ab'.repeat(32);

console.log('\n=== #941 Predicted reach: public/coverage.js ===\n');

// ── 1. Deep link ─────────────────────────────────────────────────────────

test('a point link round-trips with every field', () => {
  const st = { at: { kind: 'point', lat: 56.1629, lon: 10.2039 }, tx: 12.5, rx: 1.5, radius: 25, freq: 868.1,
    power: 14, txGain: 5, rxGain: 0, loss: 2, sens: -125 };
  const ser = PR.serializeState(st);
  assert.strictEqual(ser.reach, '56.1629,10.2039');
  const qs = new URLSearchParams(ser).toString();
  assert.deepStrictEqual(JSON.parse(JSON.stringify(PR.parseState(qs))), st);
});

test('a node link round-trips, and an upper-case key is read as lower case', () => {
  const st = PR.parseState('reach=' + PK.toUpperCase());
  assert.deepStrictEqual(JSON.parse(JSON.stringify(st)), { at: { kind: 'node', pubkey: PK } });
  assert.strictEqual(new URLSearchParams(PR.serializeState(st)).toString(), 'reach=' + PK);
});

test('unset fields stay out of the link, so the server default applies', () => {
  assert.deepStrictEqual(JSON.parse(JSON.stringify(PR.serializeState({ at: { kind: 'point', lat: 55.5, lon: 9.25 } }))),
    { reach: '55.5,9.25' });
});

test('a centre that cannot be read is no link at all', () => {
  for (const bad of ['', 'reach=', 'reach=abc', 'reach=1e1,10', 'reach=0x38,10', 'reach=NaN,10', 'reach=91,10',
    'reach=56,181', 'reach=56,10,1', 'reach=' + PK.slice(2), 'reach=' + 'g'.repeat(64), 'reach=56.1%20,10',
    'reach=' + '1'.repeat(300)]) {
    assert.strictEqual(PR.parseState(bad), null, bad);
  }
  assert.strictEqual(PR.invalidLink('reach=abc&lat=55'), true);
  assert.strictEqual(PR.invalidLink('lat=55&zoom=8'), false);
  assert.strictEqual(PR.invalidLink('reach=56.1,10.2'), false);
});

test('an optional value that is junk or out of range is dropped, the rest kept', () => {
  const st = PR.parseState('reach=56.1,10.2&rtx=1e2&rrx=-1&rkm=0&rmhz=100&rpwr=99&rgtx=Infinity&rgrx=4&rloss=0x1&rsens=-131');
  assert.deepStrictEqual(JSON.parse(JSON.stringify(st)), { at: { kind: 'point', lat: 56.1, lon: 10.2 }, rxGain: 4, sens: -131 });
});

test('withReachParams replaces the reach keys and keeps the rest of the hash', () => {
  const h = '#/map?lat=56&lon=10&zoom=9&base=dark&reach=55,9&rtx=3&rpwr=10';
  const out = PR.withReachParams(h, PR.serializeState({ at: { kind: 'node', pubkey: PK }, power: 20 }));
  const q = new URLSearchParams(out.split('?')[1]);
  assert.strictEqual(out.split('?')[0], '#/map');
  assert.strictEqual(q.get('lat'), '56');
  assert.strictEqual(q.get('base'), 'dark');
  assert.strictEqual(q.get('reach'), PK);
  assert.strictEqual(q.get('rpwr'), '20');
  assert.strictEqual(q.get('rtx'), null);
  assert.strictEqual(PR.withReachParams(h, {}), '#/map?lat=56&lon=10&zoom=9&base=dark');
  assert.strictEqual(PR.withReachParams('#/map?reach=55,9', {}), '#/map');
});

test('the reach keys never collide with the Path profile\'s', () => {
  const profileKeys = ['profile', 'ha', 'hb', 'k', 'freq', 'model'];
  const reachKeys = [PR.HASH_PARAM].concat(Object.keys(PR.KEYS).map(k => PR.KEYS[k]));
  reachKeys.forEach(k => assert.ok(!profileKeys.includes(k), k));
});

// ── 2. Request ───────────────────────────────────────────────────────────

test('the request carries the centre and the geometry, never the budget', () => {
  const url = PR.requestURL({ at: { kind: 'point', lat: 56.1629, lon: 10.2039 }, tx: 10, rx: 2, radius: 30, freq: 869.618,
    power: 14, txGain: 5, rxGain: 1, loss: 2, sens: -120 });
  assert.strictEqual(url, '/api/terrain/coverage?at=56.1629%2C10.2039&ha=10&hr=2&r=30&freq=869.618');
  for (const leak of ['14', 'rpwr', '-120', 'sens', 'gain', 'p=', 'g=', 's=']) assert.ok(!url.includes(leak), leak);
  assert.strictEqual(PR.requestURL({ at: { kind: 'node', pubkey: PK } }), '/api/terrain/coverage?at=' + PK);
});

// ── 3. Budget and classes ────────────────────────────────────────────────

test('the default budget is 22 + 3 + 3 + 130 = 158 dB (M0 §3.5)', () => {
  assert.strictEqual(PR.budgetOf({}, null), 158);
  assert.strictEqual(PR.budgetOf(null, { txPowerDbm: 20, antennaGainDbi: 5, rxGainDbi: 3, lossDb: 1, sensitivityDbm: -125 }), 152);
  assert.strictEqual(PR.budgetOf({ power: 10, loss: 4 }, { txPowerDbm: 20, antennaGainDbi: 5, rxGainDbi: 3, lossDb: 1, sensitivityDbm: -125 }), 139);
});

test('margin classes: a boundary belongs to the better class', () => {
  const cases = [[25, 'good'], [10, 'good'], [9.999, 'marginal'], [3, 'marginal'], [2.999, 'weak'], [0, 'weak'],
    [-0.001, 'none'], [-40, 'none'], [NaN, 'none'], [Infinity, 'none'], [undefined, 'none']];
  for (const [m, want] of cases) assert.strictEqual(PR.marginClass(m), want, String(m));
});

// ── 4. Colour table ──────────────────────────────────────────────────────

const META = { lossBaseDb: 60, lossStepDb: 0.5, noData: 255 };
const COLORS = { good: [21, 128, 61], marginal: [234, 179, 8], weak: [234, 88, 12] };
function lutAt(lut, v) { return Array.from(lut.slice(4 * v, 4 * v + 4)); }

test('the table colours each loss byte by its margin, at the class boundaries', () => {
  const lut = PR.buildLut(158, META, COLORS);
  assert.strictEqual(lut.length, 1024);
  const byte = (loss) => (loss - 60) / 0.5;
  assert.deepStrictEqual(lutAt(lut, byte(148)), [21, 128, 61, 255]);   // margin 10: good
  assert.deepStrictEqual(lutAt(lut, byte(148.5)), [234, 179, 8, 255]); // 9.5: marginal
  assert.deepStrictEqual(lutAt(lut, byte(155)), [234, 179, 8, 255]);   // 3: marginal
  assert.deepStrictEqual(lutAt(lut, byte(155.5)), [234, 88, 12, 255]); // 2.5: weak
  assert.deepStrictEqual(lutAt(lut, byte(158)), [234, 88, 12, 255]);   // 0: weak
  assert.deepStrictEqual(lutAt(lut, byte(158.5)), [0, 0, 0, 0]);       // below 0: transparent
  assert.deepStrictEqual(lutAt(lut, 0), [21, 128, 61, 255]);
  assert.deepStrictEqual(lutAt(lut, 254), [0, 0, 0, 0]);
  assert.deepStrictEqual(lutAt(lut, 255), [0, 0, 0, 0]);               // no data
  // At the largest budget the panel allows (30 + 20 + 20 + 150 dB) every
  // loss byte is coloured, and no data still is not.
  const max = PR.buildLut(220, META, COLORS);
  assert.deepStrictEqual(lutAt(max, 254), [21, 128, 61, 255]);
  assert.deepStrictEqual(lutAt(max, 255), [0, 0, 0, 0]);
});

test('a budget change moves the boundaries: +8 dB turns weak into good', () => {
  const v = (155.5 - 60) / 0.5; // margin 2.5 dB at the default budget
  assert.deepStrictEqual(lutAt(PR.buildLut(158, META, COLORS), v).slice(0, 3), COLORS.weak);
  assert.deepStrictEqual(lutAt(PR.buildLut(160, META, COLORS), v).slice(0, 3), COLORS.marginal);
  assert.deepStrictEqual(lutAt(PR.buildLut(166, META, COLORS), v).slice(0, 3), COLORS.good);
  assert.deepStrictEqual(lutAt(PR.buildLut(150, META, COLORS), v), [0, 0, 0, 0]);
});

test('without a colour for a class the class is transparent, and junk meta paints nothing', () => {
  assert.deepStrictEqual(lutAt(PR.buildLut(158, META, { good: COLORS.good }), (155 - 60) / 0.5), [0, 0, 0, 0]);
  const junk = PR.buildLut(158, { lossBaseDb: 'x', lossStepDb: 0.5 }, COLORS);
  assert.ok(junk.every(b => b === 0));
});

test('paint maps every raster byte through the table', () => {
  const lut = PR.buildLut(158, META, COLORS);
  const raster = new Uint8Array([0, 176, 190, 197, 255, 254]);
  const rgba = PR.paint(new Uint8ClampedArray(raster.length * 4), raster, lut);
  for (let i = 0; i < raster.length; i++) {
    assert.deepStrictEqual(Array.from(rgba.slice(4 * i, 4 * i + 4)), lutAt(lut, raster[i]), 'pixel ' + i);
  }
});

test('classShares counts the classes over the pixels with data', () => {
  // 148 dB, 148 dB, 148.5, 155, 155.5, ≥ 187, no data, no data.
  const raster = new Uint8Array([0, 0, 177, 190, 191, 254, 255, 255]);
  const s = PR.classShares(raster, 158, META);
  assert.deepStrictEqual(JSON.parse(JSON.stringify(s)), { good: 2, marginal: 2, weak: 1, none: 1, total: 6 });
});

test('parseColor reads what getComputedStyle returns, and themeColors reads the variables', () => {
  const rgb = (s) => JSON.parse(JSON.stringify(PR.parseColor(s)));
  assert.deepStrictEqual(rgb('#15803d'), [21, 128, 61]);
  assert.deepStrictEqual(rgb(' #fc0 '), [255, 204, 0]);
  assert.deepStrictEqual(rgb('rgb(1, 2, 3)'), [1, 2, 3]);
  assert.deepStrictEqual(rgb('rgba(10 20 30 / 0.5)'), [10, 20, 30]);
  assert.strictEqual(PR.parseColor('red'), null);
  assert.strictEqual(PR.parseColor(''), null);
  const vars = { '--reach-good': '#000001', '--reach-marginal': 'rgb(0,0,2)', '--reach-weak': ' #000003' };
  assert.deepStrictEqual(JSON.parse(JSON.stringify(PR.themeColors(n => vars[n]))), { good: [0, 0, 1], marginal: [0, 0, 2], weak: [0, 0, 3] });
});

// ── 5. Refusals ──────────────────────────────────────────────────────────

test('the panel words a refusal by status, with the radius limit as a number only', () => {
  assert.strictEqual(PR.refusal(400, { error: 'radius too large', maxRadiusKm: 25 }), 'The radius is limited to 25 km on this server.');
  assert.ok(/Denmark\.$/.test(PR.refusal(400, { error: 'point not covered' }, null, {})));
  assert.ok(/or near it/.test(PR.refusal(400, { error: 'point not covered' }, null, { fill: {} })));
  assert.ok(/in 7 s/.test(PR.refusal(429, { error: 'rate limit exceeded' }, '7')));
  assert.ok(/busy/.test(PR.refusal(503, {}, '5')));
  assert.ok(/timed out/.test(PR.refusal(504, {})));
  assert.ok(/unavailable/.test(PR.refusal(502, {})));
  assert.ok(/unknown, hidden/.test(PR.refusal(404, {})));
  for (const status of [400, 500, 418]) {
    const msg = PR.refusal(status, { error: '<img src=x onerror=alert(1)>', maxRadiusKm: '<b>' });
    assert.ok(!/img|onerror|<b>/.test(msg), msg);
  }
  assert.ok(!/<b>/.test(PR.refusal(400, { error: 'radius too large', maxRadiusKm: '<b>' })));
});

test('the Copernicus credit says "Outside Denmark" only beside DHM', () => {
  const fill = { credit: 'Outside Denmark: produced using Copernicus DEM GLO-30, provided under COPERNICUS by the European Union and ESA (Copernicus DEM licence)',
    datasetName: 'Copernicus DEM GLO-30', licenseName: 'Copernicus DEM licence' };
  assert.strictEqual(PR.fillShortCredit({ dhm: true, fill }), fill.credit);
  assert.strictEqual(PR.fillShortCredit({ dhm: false, fill }), 'Terrain: Copernicus DEM GLO-30 (Copernicus DEM licence)');
  assert.strictEqual(PR.fillShortCredit({ dhm: true }), '');
});

// ── 6. Node chip ─────────────────────────────────────────────────────────

test('node detail offers the reach only when it is on and the node has a fix', () => {
  const on = { enabled: true };
  const node = { public_key: PK, lat: 56.1, lon: 10.2, name: '<img src=x onerror=alert(1)>' };
  assert.strictEqual(PR.nodeEntryHash(node, on), '#/map?reach=' + PK);
  assert.strictEqual(PR.nodeEntryHash(node, null), null);
  assert.strictEqual(PR.nodeEntryHash(node, { enabled: 'true' }), null);
  assert.strictEqual(PR.nodeEntryHash(Object.assign({}, node, { lat: 0, lon: 0 }), on), null);
  assert.strictEqual(PR.nodeEntryHash(Object.assign({}, node, { lat: null }), on), null);
  assert.strictEqual(PR.nodeEntryHash(Object.assign({}, node, { public_key: 'abc' }), on), null);
  const a = PR.nodeEntryAction(node, on);
  assert.strictEqual(a.getAttribute('href'), '#/map?reach=' + PK);
  assert.strictEqual(a.getAttribute('data-testid'), 'node-predicted-reach');
  assert.ok(/Predicted reach/.test(a.textContent));
  assert.ok(!/img|onerror/.test(a.outerHTML), 'a node field reached the chip');
  assert.strictEqual(PR.nodeEntryAction(node, null), null);
});

// ── 7. Wiring ────────────────────────────────────────────────────────────

test('index.html loads coverage.js with the cache buster, before map.js and nodes.js', () => {
  const html = fs.readFileSync(path.join(__dirname, 'public', 'index.html'), 'utf8');
  const i = html.indexOf('<script src="coverage.js?v=__BUST__"');
  assert.ok(i > 0, 'no coverage.js script tag with __BUST__');
  assert.ok(i < html.indexOf('<script src="map.js?v=__BUST__"') && i < html.indexOf('<script src="nodes.js?v=__BUST__"'));
});

test('the class colours are CSS variables in :root and both dark blocks, and the module hard-codes none', () => {
  const css = fs.readFileSync(path.join(__dirname, 'public', 'style.css'), 'utf8');
  for (const v of ['--reach-good', '--reach-marginal', '--reach-weak', '--reach-centre']) {
    const n = (css.match(new RegExp('^\\s*' + v + ':\\s*#[0-9a-f]{6};', 'gm')) || []).length;
    assert.strictEqual(n, 3, v + ' defined ' + n + ' times');
  }
  PR.CLASSES.forEach(c => assert.ok(css.includes('var(' + c.cssVar + ')'), c.cssVar + ' unused in the legend'));
  const code = SRC.replace(/\/\*[\s\S]*?\*\//g, '').replace(/\/\/.*$/gm, '');
  assert.ok(!/#[0-9a-f]{3,8}\b(?!-)/i.test(code.replace(/'#\/map[^']*'/g, '').replace(/#ph-[a-z-]+/g, '')), 'a hex colour in coverage.js');
  assert.ok(!/rgb\(/.test(code.replace(/\/\^rgba\?.*$/m, '')), 'an rgb() colour in coverage.js');
});

test('roles.js publishes the coverage block, and nodes.js mounts the chip', () => {
  const roles = fs.readFileSync(path.join(__dirname, 'public', 'roles.js'), 'utf8');
  assert.ok(/window\.MC_COVERAGE_CFG = cfg\.coverage \|\| null;/.test(roles));
  const nodes = fs.readFileSync(path.join(__dirname, 'public', 'nodes.js'), 'utf8');
  assert.ok(/mountConfigAction\(row, 'node-predicted-reach'/.test(nodes));
  const map = fs.readFileSync(path.join(__dirname, 'public', 'map.js'), 'utf8');
  assert.ok(/initReachTool\(\);/.test(map) && /destroyReachTool\(\);/.test(map));
});

// ── 8. Perf ──────────────────────────────────────────────────────────────

test('recolouring an 816×816 raster (table + paint) takes a few ms here', () => {
  const n = 816 * 816;
  const raster = new Uint8Array(n);
  for (let i = 0; i < n; i++) raster[i] = (i * 2654435761 >>> 24);
  const rgba = new Uint8ClampedArray(4 * n);
  PR.paint(rgba, raster, PR.buildLut(158, META, COLORS)); // warm
  const runs = 10, t0 = process.hrtime.bigint();
  for (let r = 0; r < runs; r++) PR.paint(rgba, raster, PR.buildLut(150 + r, META, COLORS));
  const ms = Number(process.hrtime.bigint() - t0) / 1e6 / runs;
  console.log('     recolour 816×816: ' + ms.toFixed(2) + ' ms (desktop Node; the phone budget is 50 ms)');
  assert.ok(ms < 25, ms + ' ms');
});

console.log('\n' + passed + ' passed, ' + failed + ' failed\n');
process.exit(failed ? 1 : 0);
