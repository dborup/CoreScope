/* test-issue-914-heat-gradient.js — #914: the node heat map has its own
 * single-hue palette (violet -> magenta), read from theme tokens through ONE
 * helper (public/heat-gradient.js) that Map and Live both use.
 *
 * The real files run in vm sandboxes or are read as source; nothing is copied.
 *
 * Covered:
 *   1. the gradient comes from the --heat-stop-1..5 tokens, in leaflet.heat's
 *      0.2..1.0 stops; a missing/empty/invalid token falls back to the
 *      default stop, and the defaults are the :root --heat-N values;
 *   2. map.js and live.js build the layer through HeatGradient and carry no
 *      hex gradient of their own (the mutant: a hardcoded one goes red);
 *   3. style.css defines the tokens in :root and both dark blocks, no hex
 *      outside them, and the defaults are in the violet-magenta hue band
 *      that the elevation ramp (below its plum top) does not use;
 *      --heat-stop-N is the user's pick (--heat-user-N), else a preset's
 *      light ramp in :root and its dark ramp in both dark blocks, else the
 *      theme's --heat-N;
 *   3b. the cascade, with the real applyCSS and applyPreset writing the
 *      root's inline style and style.css's var() chain resolved over it:
 *      user override > colour-blind preset > server config > style.css
 *      (the #1446 rule; #917 delta review finding 3);
 *   4. the customizer maps the tokens (THEME_CSS_MAP) and keeps them out of
 *      preset detection; a cb-preset's heatRamp has a light and a dark ramp
 *      and applyPreset sets / clears --mc-heat-{light,dark}-N, also on a
 *      direct switch from a preset with a ramp to one without;
 *   4b. every preset, in light and dark, under its own colour-vision
 *      simulation: the densest stop stands out from the base map at the
 *      default Map opacity, and the ramp stays away from the elevation ramp
 *      (the #917 review: near-white preset ramps vanished on light maps and
 *      the tritan ramp read as terrain);
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
const { loadCustomizer } = require('./tests/customizer-sandbox');

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

// style.css's three theme blocks: :root, the @media dark :root and the
// [data-theme="dark"] block. decl() reads one custom property from a block.
function blockAt(css, i) {
  const open = css.indexOf('{', i);
  let depth = 1, j = open + 1;
  while (depth && j < css.length) { if (css[j] === '{') depth++; else if (css[j] === '}') depth--; j++; }
  return css.slice(open + 1, j - 1);
}
// style.css has several blocks per selector; the theme's is the one that
// declares the heat tokens.
function heatBlock(css, head) {
  for (let i = css.indexOf(head); i >= 0; i = css.indexOf(head, i + 1)) {
    const b = blockAt(css, i);
    if (/\n\s*--heat-1:/.test(b)) return b;
  }
  return '';
}
function cssBlocks(css) {
  return {
    root: heatBlock(css, '\n:root {'),
    media: heatBlock(css, '\n  :root:not([data-theme="light"]) {'),
    dark: heatBlock(css, '\n[data-theme="dark"] {'),
  };
}
function decl(block, name) {
  const m = block.match(new RegExp('\\n\\s*' + name + ':\\s*([^;]+);'));
  return m ? m[1].trim() : null;
}

// Colour-vision simulation (Machado, Oliveira & Fernandes 2009, severity 1,
// in linear sRGB; achromatopsia as luminance) and CIE76 ΔE in CIELAB (D65).
const lin = (c) => { c /= 255; return c <= 0.04045 ? c / 12.92 : Math.pow((c + 0.055) / 1.055, 2.4); };
const enc = (v) => { v = Math.min(1, Math.max(0, v)); return 255 * (v <= 0.0031308 ? 12.92 * v : 1.055 * Math.pow(v, 1 / 2.4) - 0.055); };
const rgb = (h) => { const n = parseInt(h.slice(1), 16); return [(n >> 16) & 255, (n >> 8) & 255, n & 255]; };
const MACHADO = {
  prot: [[0.152286, 1.052583, -0.204868], [0.114503, 0.786281, 0.099216], [-0.003882, -0.048116, 1.051998]],
  deut: [[0.367322, 0.860646, -0.227968], [0.280085, 0.672501, 0.047413], [-0.011820, 0.042940, 0.968881]],
  trit: [[1.255528, -0.076749, -0.178779], [-0.078411, 0.930809, 0.147602], [0.004733, 0.691367, 0.303900]],
};
function simulate(c, kind) {
  if (kind === 'none') return c;
  const l = c.map(lin);
  if (kind === 'achromat') { const y = enc(0.2126 * l[0] + 0.7152 * l[1] + 0.0722 * l[2]); return [y, y, y]; }
  return MACHADO[kind].map((r) => enc(r[0] * l[0] + r[1] * l[1] + r[2] * l[2]));
}
function lab(c) {
  const [r, g, b] = c.map(lin);
  const f = (t) => (t > 216 / 24389 ? Math.cbrt(t) : (24389 / 27 * t + 16) / 116);
  const fx = f((0.4124 * r + 0.3576 * g + 0.1805 * b) / 0.95047), fy = f(0.2126 * r + 0.7152 * g + 0.0722 * b);
  const fz = f((0.0193 * r + 0.1192 * g + 0.9505 * b) / 1.08883);
  return [116 * fy - 16, 500 * (fx - fy), 200 * (fy - fz)];
}
const deltaE = (a, b) => { const p = lab(a), q = lab(b); return Math.hypot(p[0] - q[0], p[1] - q[1], p[2] - q[2]); };

function elevationRamp() {
  const elev = fs.readFileSync(path.join(__dirname, 'internal', 'dhm', 'elevation.go'), 'utf8');
  return [...elev.matchAll(/\{(\d+),\s*0x([0-9a-f]{2}),\s*0x([0-9a-f]{2}),\s*0x([0-9a-f]{2})\}/g)]
    .map((m) => ({ m: +m[1], hex: '#' + m[2] + m[3] + m[4] }));
}

console.log('— #914 heat gradient');

test('the gradient comes from the --heat-stop-N tokens at stops 0.2..1.0', () => {
  const { HG } = loadHelper({ '--heat-stop-1': '#111111', '--heat-stop-2': '#222222', '--heat-stop-3': '#333333', '--heat-stop-4': '#444444', '--heat-stop-5': '#555555' });
  const g = HG.gradient();
  // integer-like key '1' enumerates first; order does not matter to leaflet.heat
  assert.deepStrictEqual(Object.keys(g).sort((a, b) => a - b), STOPS);
  assert.deepStrictEqual(STOPS.map((k) => g[k]), ['#111111', '#222222', '#333333', '#444444', '#555555']);
});

test('HeatGradient.DEFAULTS are the :root --heat-N values (no drift)', () => {
  const { HG } = loadHelper({});
  const root = cssBlocks(read('style.css')).root;
  assert.deepStrictEqual(Array.from(HG.DEFAULTS), [1, 2, 3, 4, 5].map((n) => decl(root, '--heat-' + n)));
});

test('a missing, empty or invalid token falls back to the default stop', () => {
  const { HG } = loadHelper({ '--heat-stop-1': '  ', '--heat-stop-2': 'url(javascript:1)', '--heat-stop-3': 'red; x' });
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

test('--heat-stop-N: the user pick, else the preset light ramp in :root and the dark ramp in both dark blocks', () => {
  const b = cssBlocks(read('style.css'));
  for (let n = 1; n <= 5; n++) {
    assert.strictEqual(decl(b.root, '--heat-stop-' + n), 'var(--heat-user-' + n + ', var(--mc-heat-light-' + n + ', var(--heat-' + n + ')))', ':root --heat-stop-' + n);
    for (const k of ['media', 'dark']) {
      assert.strictEqual(decl(b[k], '--heat-stop-' + n), 'var(--heat-user-' + n + ', var(--mc-heat-dark-' + n + ', var(--heat-' + n + ')))', k + ' --heat-stop-' + n);
      assert(/^#[0-9a-f]{6}$/i.test(decl(b[k], '--heat-' + n) || ''), k + ' --heat-' + n);
    }
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
  const ramp = elevationRamp();
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

function loadPresets() {
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
  return { P: ctx.window.MeshCorePresets, props };
}
const MODES = ['light', 'dark'];

test('a preset heatRamp has 5 light and 5 dark stops; applyPreset sets and clears --mc-heat-{light,dark}-N', () => {
  const { P, props } = loadPresets();
  const withRamp = P.list.filter((p) => p.heatRamp);
  assert(withRamp.length >= 1, 'no preset carries a heatRamp');
  for (const p of P.list) {
    P.applyPreset(p.id);
    for (const mode of MODES) {
      if (p.heatRamp) assert(Array.isArray(p.heatRamp[mode]) && p.heatRamp[mode].length === 5, p.id + ' heatRamp.' + mode);
      for (let n = 1; n <= 5; n++) {
        const k = '--mc-heat-' + mode + '-' + n;
        if (p.heatRamp) assert.strictEqual(props[k], p.heatRamp[mode][n - 1], p.id + ' ' + k);
        else assert(!(k in props), p.id + ' leaves ' + k + ' set');
      }
    }
  }
  P.applyPreset(withRamp[0].id); P.clearPreset();
  for (const mode of MODES) for (let n = 1; n <= 5; n++) assert(!('--mc-heat-' + mode + '-' + n in props), 'clearPreset leaves --mc-heat-' + mode + '-' + n);
});

test('a direct switch from a preset with a heat ramp to one without clears --mc-heat-* (no clearPreset between)', () => {
  const { P, props } = loadPresets();
  const withRamp = P.list.filter((p) => p.heatRamp), without = P.list.filter((p) => !p.heatRamp);
  assert(withRamp.length && without.length, 'need presets with and without a heatRamp');
  for (const from of withRamp) {
    for (const to of without) {
      P.applyPreset(from.id);
      assert('--mc-heat-light-5' in props, from.id + ' did not set its ramp');
      P.applyPreset(to.id);
      for (const mode of MODES) for (let n = 1; n <= 5; n++) {
        assert(!('--mc-heat-' + mode + '-' + n in props), from.id + ' -> ' + to.id + ' leaves --mc-heat-' + mode + '-' + n);
      }
    }
  }
});

// #917 review findings 1 and 2. Each preset is judged under the vision it is
// for. Base maps are flat approximations of the light and dark tiles; the
// heat canvas is drawn at the default Map opacity (0.25; Live uses 0.3).
const PRESET_VISION = { default: 'none', deut: 'deut', prot: 'prot', trit: 'trit', achromat: 'achromat' };
const BASE = { light: '#f2efe9', dark: '#262626' };
const MAP_OPACITY = 0.25;
const PEAK_FLOOR = 12;  // ΔE76, densest stop over the base map (the old near-white preset ramps: 1.6-8.4)
const ELEV_FLOOR = 15;  // ΔE76, nearest heat stop to an elevation stop below 130 m (old palette: 8.8, old tritan ramp: 9.7)

function effectiveRamp(p, mode) {
  if (p.heatRamp) return p.heatRamp[mode];
  const b = cssBlocks(read('style.css'));
  return [1, 2, 3, 4, 5].map((n) => decl(mode === 'light' ? b.root : b.dark, '--heat-' + n));
}

test('every preset, light and dark: the densest heat stop stands out from the base map under its own vision', () => {
  const { P } = loadPresets();
  for (const p of P.list) {
    const vision = PRESET_VISION[p.id];
    assert(vision, 'no colour-vision simulation for preset ' + p.id + ': add it to PRESET_VISION');
    for (const mode of MODES) {
      const bg = rgb(BASE[mode]), peak = rgb(effectiveRamp(p, mode)[4]);
      const drawn = peak.map((v, i) => MAP_OPACITY * v + (1 - MAP_OPACITY) * bg[i]);
      const d = deltaE(simulate(drawn, vision), simulate(bg, vision));
      assert(d >= PEAK_FLOOR, p.id + ' ' + mode + ': peak ' + effectiveRamp(p, mode)[4] + ' is ΔE ' + d.toFixed(1) + ' from the base map, under ' + PEAK_FLOOR);
    }
  }
});

test('every preset, light and dark: no heat stop reads as elevation under its own vision', () => {
  const { P } = loadPresets();
  const elev = elevationRamp().filter((x) => x.m < 130);
  for (const p of P.list) {
    const vision = PRESET_VISION[p.id];
    assert(vision, 'no colour-vision simulation for preset ' + p.id);
    // Achromatopsia sees the elevation ramp as a full grey ramp, so no heat
    // ramp can be told apart from it by colour; only its contrast counts.
    if (vision === 'achromat') continue;
    for (const mode of MODES) {
      for (const h of effectiveRamp(p, mode)) {
        for (const e of elev) {
          const d = deltaE(simulate(rgb(h), vision), simulate(rgb(e.hex), vision));
          assert(d >= ELEV_FLOOR, p.id + ' ' + mode + ': ' + h + ' is ΔE ' + d.toFixed(1) + ' from elevation ' + e.m + ' m (' + e.hex + '), under ' + ELEV_FLOOR);
        }
      }
    }
  }
});

// #917 delta review finding 3: the #1446 cascade. The real customizer
// (applyCSS) and the real presets (applyPreset) write the root's inline
// style; resolve() then plays the browser: an inline custom property beats
// the stylesheet's, var() takes its fallback when the name is unset, and a
// dark block inherits what it does not declare from :root.
function heatCascade(theme) {
  const c = loadCustomizer({ theme, scripts: ['cb-presets.js'] });
  const b = cssBlocks(read('style.css'));
  const block = theme === 'dark' ? b.dark : b.root;
  function lookup(name) {
    if (c.rootStyle._props[name] != null) return c.rootStyle._props[name];
    const v = decl(block, name);
    return v != null ? v : decl(b.root, name);
  }
  function value(v) {
    const m = v && v.match(/^var\((--[\w-]+)(?:,\s*(.*))?\)$/);
    if (!m) return v;
    const hit = lookup(m[1]);
    if (hit != null) return value(hit);
    return m[2] != null ? value(m[2]) : null;
  }
  const P = c.ctx.window.MeshCorePresets;
  return {
    P,
    peak: () => value(lookup('--heat-stop-5')),
    apply: (server, user) => {
      c.api.writeOverrides(user || {});
      c.api.applyCSS(c.api.computeEffective(server || {}, c.api.readOverrides()), c.api.readOverrides());
    },
  };
}

test('cascade: user override > colour-blind preset > server config > style.css, light and dark', () => {
  for (const theme of MODES) {
    const k = heatCascade(theme);
    const sheet = decl(theme === 'dark' ? cssBlocks(read('style.css')).dark : cssBlocks(read('style.css')).root, '--heat-5');
    const ramp = k.P.list.find((p) => p.id === 'achromat').heatRamp[theme][4];
    const server = { theme: { heat5: '#0000aa' } };
    k.apply({}, {});
    assert.strictEqual(k.peak(), sheet, theme + ': style.css');
    k.apply(server, {});
    assert.strictEqual(k.peak(), '#0000aa', theme + ': server config over style.css');
    k.P.applyPreset('achromat');
    assert.strictEqual(k.peak(), ramp, theme + ': the preset over server config');
    k.apply(server, { theme: { heat5: '#ff0000' } });
    assert.strictEqual(k.peak(), '#ff0000', theme + ': the user override over the preset');
    k.apply(server, {});
    assert.strictEqual(k.peak(), ramp, theme + ': a cleared override falls back to the preset');
    k.P.clearPreset();
    assert.strictEqual(k.peak(), '#0000aa', theme + ': a cleared preset falls back to server config');
    k.apply({}, {});
    assert.strictEqual(k.peak(), sheet, theme + ': and then to style.css');
  }
});

test('cascade, dark: a server themeDark colour still beats the user light pick (as for every theme key), and the preset beats it', () => {
  const k = heatCascade('dark');
  const ramp = k.P.list.find((p) => p.id === 'achromat').heatRamp.dark[4];
  k.apply({ themeDark: { heat5: '#0000aa' } }, { theme: { heat5: '#ff0000' } });
  assert.strictEqual(k.peak(), '#0000aa', 'server themeDark over the user light pick');
  k.P.applyPreset('achromat');
  assert.strictEqual(k.peak(), ramp, 'the preset over a server value');
  k.apply({ themeDark: { heat5: '#0000aa' } }, { themeDark: { heat5: '#00aa00' } });
  assert.strictEqual(k.peak(), '#00aa00', 'the user dark pick over the preset');
});

test('watch() calls back once per real change, never on an unchanged gradient, and unsubscribes', () => {
  const { HG, listeners, vars } = loadHelper({ '--heat-stop-1': '#111111' });
  let n = 0;
  const off = HG.watch(() => { n++; });
  listeners['theme-changed'].forEach((f) => f());
  assert.strictEqual(n, 0, 'unchanged gradient must not rebuild');
  vars['--heat-stop-1'] = '#222222';
  listeners['theme-changed'].forEach((f) => f());
  assert.strictEqual(n, 1);
  vars['--heat-stop-2'] = '#333333';
  listeners['cb-preset-changed'].forEach((f) => f());
  assert.strictEqual(n, 2);
  off();
  vars['--heat-stop-1'] = '#444444';
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
