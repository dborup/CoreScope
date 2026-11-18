/* test-issue-914-heat-gradient.js — #914: the node heat map has its own
 * single-hue palette (violet -> magenta), read from theme tokens through ONE
 * helper (public/heat-gradient.js) that Map and Live both use.
 *
 * The real files run in vm sandboxes or are read as source; nothing is copied.
 *
 * Covered:
 *   1. the gradient comes from the --heat-1..5 tokens, in leaflet.heat's
 *      0.2..1.0 stops; --mc-heat-N (colour-blind preset) wins over --heat-N;
 *      a missing/empty/invalid token falls back to the default stop;
 *   2. map.js and live.js build the layer through HeatGradient and carry no
 *      hex gradient of their own (the mutant: a hardcoded one goes red);
 *   3. style.css defines the tokens in :root and both dark blocks, no hex
 *      outside them, and the defaults are in the violet-magenta hue band
 *      that the elevation ramp (below its plum top) does not use;
 *   4. the customizer maps the tokens (THEME_CSS_MAP) and keeps them out of
 *      preset detection; every cb-preset carries a heatRamp and applyPreset
 *      sets / clears --mc-heat-N;
 *   5. watch(): a theme or preset change calls back once when the gradient
 *      changed, not when it did not; the unsubscribe stops it; both pages
 *      subscribe and unsubscribe.
 *
 * Runs via: node test-issue-914-heat-gradient.js
 */
'use strict';
const fs = require('fs');
const path = require('path');
const vm = require('vm');
const assert = require('assert');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}
const read = (f) => fs.readFileSync(path.join(__dirname, 'public', f), 'utf8');

function loadHelper(vars) {
  const listeners = {};
  const win = {
    addEventListener: (t, f) => { (listeners[t] = listeners[t] || []).push(f); },
    removeEventListener: (t, f) => { listeners[t] = (listeners[t] || []).filter((x) => x !== f); },
  };
  const style = { getPropertyValue: (k) => (vars[k] == null ? '' : vars[k]) };
  const ctx = {
    window: win,
    document: { documentElement: { style: {}, getAttribute: () => 'light' } },
    getComputedStyle: () => style,
    setTimeout: (f) => { f(); return 1; }, clearTimeout: () => {},
    Object, Array, String, Number, JSON,
  };
  ctx.window.window = ctx.window;
  vm.createContext(ctx);
  vm.runInContext(read('heat-gradient.js'), ctx);
  return { HG: ctx.window.HeatGradient, listeners, vars };
}

const STOPS = ['0.2', '0.4', '0.6', '0.8', '1'];
const hue = (hex) => {
  const n = parseInt(hex.slice(1), 16);
  const r = (n >> 16) / 255, g = ((n >> 8) & 255) / 255, b = (n & 255) / 255;
  const mx = Math.max(r, g, b), mn = Math.min(r, g, b), d = mx - mn;
  if (!d) return -1;
  let h = mx === r ? ((g - b) / d) % 6 : mx === g ? (b - r) / d + 2 : (r - g) / d + 4;
  h *= 60; return h < 0 ? h + 360 : h;
};

console.log('— #914 heat gradient');

test('the gradient comes from the --heat-N tokens at stops 0.2..1.0', () => {
  const { HG } = loadHelper({ '--heat-1': '#111111', '--heat-2': '#222222', '--heat-3': '#333333', '--heat-4': '#444444', '--heat-5': '#555555' });
  const g = HG.gradient();
  // integer-like key '1' enumerates first; order does not matter to leaflet.heat
  assert.deepStrictEqual(Object.keys(g).sort((a, b) => a - b), STOPS);
  assert.deepStrictEqual(STOPS.map((k) => g[k]), ['#111111', '#222222', '#333333', '#444444', '#555555']);
});

test('--mc-heat-N (colour-blind preset) wins over --heat-N', () => {
  const { HG } = loadHelper({ '--heat-1': '#111111', '--mc-heat-1': '#abcdef' });
  assert.strictEqual(HG.gradient()['0.2'], '#abcdef');
  assert.strictEqual(HG.gradient()['0.4'], HG.DEFAULTS[1]);
});

test('a missing, empty or invalid token falls back to the default stop', () => {
  const { HG } = loadHelper({ '--heat-1': '  ', '--heat-2': 'url(javascript:1)', '--heat-3': 'red; x' });
  const g = HG.gradient();
  assert.strictEqual(g['0.2'], HG.DEFAULTS[0]);
  assert.strictEqual(g['0.4'], HG.DEFAULTS[1]);
  assert.strictEqual(g['0.6'], HG.DEFAULTS[2]);
  assert.strictEqual(g['1'], HG.DEFAULTS[4]);
});

test('options() hands leaflet.heat the gradient and keeps the shared geometry', () => {
  const { HG } = loadHelper({});
  const o = HG.options();
  assert.deepStrictEqual(o.gradient, HG.gradient());
  assert.strictEqual(o.radius, 25); assert.strictEqual(o.blur, 15);
  assert.strictEqual(o.maxZoom, 14); assert.strictEqual(o.minOpacity, 0.05);
});

test('map.js and live.js use the helper and carry no hex gradient (mutant guard)', () => {
  for (const f of ['map.js', 'live.js']) {
    const src = read(f);
    assert(/HeatGradient\.options\(\)/.test(src), f + ' must build the layer through HeatGradient.options()');
    assert(!/gradient\s*:\s*\{/.test(src), f + ' must not carry its own gradient literal');
    const heatBlock = src.slice(src.indexOf('L.heatLayer('), src.indexOf('L.heatLayer(') + 400);
    assert(!/#[0-9a-fA-F]{6}/.test(heatBlock), f + ' hardcodes a colour near L.heatLayer');
  }
});

test('index.html loads heat-gradient.js before map.js and live.js', () => {
  const h = read('index.html');
  const i = h.indexOf('heat-gradient.js');
  assert(i > 0, 'not loaded');
  assert(i < h.indexOf('src="map.js'), 'after map.js');
  assert(i < h.indexOf('src="live.js'), 'after live.js');
});

test('style.css defines --heat-1..5 in :root and both dark blocks', () => {
  const css = read('style.css');
  for (let n = 1; n <= 5; n++) {
    const re = new RegExp('--heat-' + n + ':\\s*#[0-9a-fA-F]{6}', 'g');
    assert.strictEqual((css.match(re) || []).length, 3, '--heat-' + n + ' must appear in :root and both dark blocks');
  }
});

test('the default heat stops sit in the violet-magenta band, apart from the elevation ramp', () => {
  const css = read('style.css');
  const heat = [];
  for (let n = 1; n <= 5; n++) {
    for (const m of css.matchAll(new RegExp('--heat-' + n + ':\\s*(#[0-9a-fA-F]{6})', 'g'))) heat.push(m[1]);
  }
  assert.strictEqual(heat.length, 15);
  for (const h of heat) { const v = hue(h); assert(v >= 255 && v <= 325, h + ' hue ' + v + ' is outside violet-magenta'); }
  const elev = fs.readFileSync(path.join(__dirname, 'internal', 'dhm', 'elevation.go'), 'utf8');
  const ramp = [...elev.matchAll(/\{(\d+),\s*0x([0-9a-f]{2}),\s*0x([0-9a-f]{2}),\s*0x([0-9a-f]{2})\}/g)]
    .map((m) => ({ m: +m[1], hex: '#' + m[2] + m[3] + m[4] }));
  assert(ramp.length >= 10, 'ramp not parsed');
  for (const s of ramp.filter((x) => x.m < 130)) {
    const v = hue(s.hex);
    assert(!(v >= 255 && v <= 325), 'elevation stop ' + s.m + ' m (' + s.hex + ') is in the heat band');
  }
});

test('the customizer maps the tokens and keeps them out of preset detection', () => {
  const src = read('customize-v2.js');
  for (let n = 1; n <= 5; n++) assert(new RegExp("heat" + n + ": '--heat-" + n + "'").test(src), 'heat' + n);
  assert(/HEAT_COLOR_KEYS/.test(src) && /HEAT_COLOR_KEYS\.indexOf\(k\) === -1/.test(src));
});

test('every cb-preset has a 5-stop heatRamp; applyPreset sets and clears --mc-heat-N', () => {
  const props = {};
  const style = { setProperty: (k, v) => { props[k] = v; }, removeProperty: (k) => { delete props[k]; } };
  const ctx = {
    window: { addEventListener() {}, dispatchEvent() {}, CustomEvent: function () {} },
    document: { body: { setAttribute() {} }, documentElement: { style } },
    localStorage: { getItem: () => null, setItem() {} },
    Object, Array, Math, JSON,
  };
  ctx.window.window = ctx.window;
  vm.createContext(ctx);
  vm.runInContext(read('cb-presets.js'), ctx);
  const P = ctx.window.MeshCorePresets;
  for (const p of P.list) {
    if (p.id === 'default') continue;
    assert(Array.isArray(p.heatRamp) && p.heatRamp.length === 5, p.id + ' heatRamp');
    P.applyPreset(p.id);
    for (let n = 1; n <= 5; n++) assert.strictEqual(props['--mc-heat-' + n], p.heatRamp[n - 1], p.id + ' --mc-heat-' + n);
  }
  P.applyPreset('default');
  for (let n = 1; n <= 5; n++) assert(!('--mc-heat-' + n in props), 'default leaves --mc-heat-' + n + ' set');
  P.applyPreset('deut'); P.clearPreset();
  for (let n = 1; n <= 5; n++) assert(!('--mc-heat-' + n in props), 'clearPreset leaves --mc-heat-' + n + ' set');
});

test('watch() calls back once per real change, never on an unchanged gradient, and unsubscribes', () => {
  const { HG, listeners, vars } = loadHelper({ '--heat-1': '#111111' });
  let n = 0;
  const off = HG.watch(() => { n++; });
  listeners['theme-changed'].forEach((f) => f());
  assert.strictEqual(n, 0, 'unchanged gradient must not rebuild');
  vars['--heat-1'] = '#222222';
  listeners['theme-changed'].forEach((f) => f());
  assert.strictEqual(n, 1);
  vars['--mc-heat-2'] = '#333333';
  listeners['cb-preset-changed'].forEach((f) => f());
  assert.strictEqual(n, 2);
  off();
  vars['--heat-1'] = '#444444';
  (listeners['theme-changed'] || []).forEach((f) => f());
  assert.strictEqual(n, 2, 'unsubscribed');
});

test('Map and Live subscribe to the helper and unsubscribe on destroy', () => {
  for (const f of ['map.js', 'live.js']) {
    const src = read(f);
    assert(/HeatGradient\.watch\(/.test(src), f + ' must watch');
    assert(/_heatUnwatch/.test(src) && /_heatUnwatch\(\)/.test(src), f + ' must unwatch on destroy');
  }
});

console.log('\n' + passed + ' passed, ' + failed + ' failed');
process.exit(failed ? 1 : 0);
