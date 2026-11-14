/**
 * Unit tests (#681 M6): the customizer's `terrain` section — antenna-height
 * defaults per role, the default elevation model, and the six path-profile
 * chart colours folded into the light and dark theme sections.
 *
 * public/customize-v2.js is loaded with vm.createContext in a
 * browser-shaped sandbox, the same way test-customizer-v2.js and
 * test-issue-1496-reset-all-complete.js do; nothing here copies production
 * logic.
 *
 * Covered:
 *   1. the section: VALID_SECTIONS / OBJECT_SECTIONS membership, and the
 *      six colour keys mapped onto their CSS variables;
 *   2. validation: heights 0-100 m, model only dtm or both, colours only
 *      valid CSS colours;
 *   3. export / import round trip, including a hostile import;
 *   4. the server-only side of map.dhm — the Dataforsyningen token above
 *      all — never reaching the customizer, an export or the client;
 *   5. preset detection surviving a recoloured profile chart.
 *
 * Usage: node test-issue-681-m6-customizer-terrain.js
 */
'use strict';

const vm = require('vm');
const fs = require('fs');
const path = require('path');
const assert = require('assert');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

// ── Sandbox ────────────────────────────────────────────────────────────────

function makeStyle() {
  const props = {};
  return {
    _props: props,
    setProperty(name, value) { props[name] = String(value); },
    removeProperty(name) { delete props[name]; },
    getPropertyValue(name) { return props[name] || ''; },
  };
}

function load(opts) {
  opts = opts || {};
  const store = {};
  const localStorage = {
    getItem(k) { return Object.prototype.hasOwnProperty.call(store, k) ? store[k] : null; },
    setItem(k, v) { store[k] = String(v); },
    removeItem(k) { delete store[k]; },
    _data: store,
  };
  const rootStyle = makeStyle();
  const bodyStyle = makeStyle();
  const warnings = [];
  const body = {
    style: bodyStyle, _attrs: {},
    setAttribute(n, v) { this._attrs[n] = v; },
    getAttribute(n) { return this._attrs[n] || null; },
    removeAttribute(n) { delete this._attrs[n]; },
    hasAttribute(n) { return n in this._attrs; },
  };
  const docEl = {
    style: rootStyle,
    getAttribute: (n) => (n === 'data-theme' ? (opts.theme || 'light') : null),
    setAttribute() {}, removeAttribute() {},
  };
  const ctx = {
    window: {
      addEventListener() {}, dispatchEvent() {}, _events: [],
      SITE_CONFIG: {}, setInstanceTitle() {},
    },
    document: {
      readyState: 'complete',
      createElement: () => ({
        id: '', textContent: '', innerHTML: '', className: '',
        setAttribute() {}, appendChild() {}, style: makeStyle(),
        addEventListener() {}, querySelectorAll: () => [], querySelector: () => null,
      }),
      head: { appendChild() {} },
      body: body,
      documentElement: docEl,
      getElementById: () => null,
      addEventListener() {},
      querySelector: () => null,
      querySelectorAll: () => [],
    },
    console: Object.assign({}, console, { warn: (...a) => { warnings.push(a.join(' ')); } }),
    localStorage,
    setTimeout: (fn) => fn(),
    clearTimeout() {},
    Date, Math, Array, Object, JSON, String, Number, Boolean, Set,
    parseInt, parseFloat, isNaN, isFinite, Infinity, NaN, undefined,
    MutationObserver: class { observe() {} },
    HashChangeEvent: class {},
    CustomEvent: class { constructor(t, o) { this.type = t; this.detail = o && o.detail; } },
    getComputedStyle: () => ({ getPropertyValue: () => '' }),
  };
  ctx.window.localStorage = localStorage;
  ctx.window.CustomEvent = ctx.CustomEvent;
  ctx.self = ctx.window;
  if (opts.dhm !== undefined) ctx.window.MC_DHM_CFG = opts.dhm;
  vm.createContext(ctx);
  // index.html order: the shared height rule first, then the customizer.
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'public', 'antenna-height.js'), 'utf8'), ctx,
    { filename: 'antenna-height.js' });
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'public', 'customize-v2.js'), 'utf8'), ctx,
    { filename: 'customize-v2.js' });
  const api = ctx.window._customizerV2;
  assert.ok(api, 'customize-v2.js must define window._customizerV2');
  if (opts.server) api.init(opts.server);
  return { api, ctx, localStorage, rootStyle, warnings };
}

const SOURCE = fs.readFileSync(path.join(__dirname, 'public', 'customize-v2.js'), 'utf8');
/* The comments in customize-v2.js explain which DHM values stay on the
 * server, so a source scan for those names has to look at code only. */
const CODE = SOURCE.replace(/\/\*[\s\S]*?\*\//g, '').replace(/^\s*\/\/.*$/gm, '');

console.log('\n📐 #681 M6 — customizer: terrain-profile defaults and chart colours\n');

// ── 1. The section exists and is shaped like the others ───────────────────

console.log('=== the section ===');

test('terrain is a known object section', () => {
  const { api } = load();
  assert.ok(api.VALID_SECTIONS.indexOf('terrain') >= 0, 'terrain missing from VALID_SECTIONS');
  const ok = api.validateShape({ terrain: { heightRepeater: 12 } });
  assert.strictEqual(ok.valid, true, ok.errors.join('; '));
  const bad = api.validateShape({ terrain: 'tall' });
  assert.strictEqual(bad.valid, false, 'a scalar terrain section was accepted');
  assert.ok(/must be an object/.test(bad.errors.join(' ')), bad.errors.join(' '));
});

test('the built-in defaults are the server’s own role heights', () => {
  // cmd/server/terrain_profile.go terrainRoleAntennaM, and model "both".
  // An untouched section must therefore change nothing about a profile.
  const { api, ctx } = load();
  assert.deepStrictEqual(Object.assign({}, api.TERRAIN_DEFAULTS), {
    heightRepeater: 10, heightRoom: 5, heightCompanion: 2,
    heightSensor: 2, heightPoint: 2, model: 'both',
  });
  assert.strictEqual(ctx.window.MCAntennaHeight.MAX_M, 100);
});

test('the six chart colours map onto the documented CSS variables', () => {
  const { api } = load();
  const want = {
    terrainFill: '--terrain-fill', terrainClutter: '--terrain-clutter',
    losLine: '--los-line', fresnel100: '--fresnel-100',
    fresnel60: '--fresnel-60', profileBlocked: '--profile-blocked',
  };
  Object.keys(want).forEach((k) => {
    assert.strictEqual(api.THEME_CSS_MAP[k], want[k], k + ' is not mapped to ' + want[k]);
    assert.ok(api.TERRAIN_COLOR_KEYS.indexOf(k) >= 0, k + ' missing from TERRAIN_COLOR_KEYS');
  });
  assert.strictEqual(api.TERRAIN_COLOR_KEYS.length, 6);
});

test('the chart colours live in the theme sections, light and dark apart', () => {
  ['theme', 'themeDark'].forEach((section) => {
    const { api, rootStyle } = load({ theme: section === 'themeDark' ? 'dark' : 'light' });
    const delta = {};
    delta[section] = { terrainFill: '#123456' };
    api.applyCSS(api.computeEffective({}, delta), delta);
    assert.strictEqual(rootStyle.getPropertyValue('--terrain-fill'), '#123456',
      section + ' did not reach --terrain-fill');
  });
});

test('a dark override does not leak into light mode', () => {
  const { api, rootStyle } = load({ theme: 'light' });
  const delta = { themeDark: { losLine: '#abcdef' } };
  api.applyCSS(api.computeEffective({}, delta), delta);
  assert.strictEqual(rootStyle.getPropertyValue('--los-line'), '',
    'the dark line colour was applied in light mode');
});

test('the swatch falls back to the colour the page actually draws with', () => {
  // The six chart colours have no server default on purpose, so without a
  // fallback every row would render a black swatch and an empty hex — a
  // colour the chart is not using. style.css is the source, read back
  // through getComputedStyle; nothing is hard-coded here or there.
  const { api, ctx } = load();
  const seen = [];
  ctx.getComputedStyle = () => ({ getPropertyValue: (n) => { seen.push(n); return ' rgba(84, 124, 92, 0.55) '; } });
  ctx.window.getComputedStyle = ctx.getComputedStyle;
  assert.strictEqual(typeof api.cssColorToHex, 'function');
  // The conversion the <input type="color"> needs; it has no alpha channel,
  // so the alpha is dropped for the swatch only.
  assert.strictEqual(api.cssColorToHex('rgba(84, 124, 92, 0.55)'), '#547c5c');
  assert.strictEqual(api.cssColorToHex('rgb(154, 138, 115)'), '#9a8a73');
  assert.strictEqual(api.cssColorToHex('#9a8a73'), '#9a8a73');
  assert.strictEqual(api.cssColorToHex('#abc'), '#aabbcc');
  assert.strictEqual(api.cssColorToHex('#9a8a73ff'), '#9a8a73');
  ['', 'not a colour', 'rgb(1,2)', 'url(x)', null, undefined, 42].forEach((v) => {
    assert.strictEqual(api.cssColorToHex(v), '', 'accepted ' + JSON.stringify(v));
  });
});

// ── 2. Validation ─────────────────────────────────────────────────────────

console.log('\n=== validation ===');

test('an antenna height is a finite number in 0-100 m', () => {
  const { api } = load();
  [0, 0.5, 2, 10, 99.5, 100, '12', '12.5'].forEach((v) => {
    assert.strictEqual(api.isValidAntennaHeight(v), true, 'rejected ' + JSON.stringify(v));
  });
  [-0.1, 100.1, 1000, NaN, Infinity, -Infinity, null, undefined, '', 'tall', {}, []].forEach((v) => {
    assert.strictEqual(api.isValidAntennaHeight(v), false, 'accepted ' + JSON.stringify(v));
  });
});

test('an out-of-range height is never stored', () => {
  const { api, localStorage, warnings } = load();
  api.writeOverrides({ terrain: { heightRepeater: 101, heightRoom: -1, heightSensor: 3 } });
  const stored = JSON.parse(localStorage.getItem('cs-theme-overrides'));
  assert.deepStrictEqual(stored.terrain, { heightSensor: 3 });
  assert.ok(warnings.some(w => /Invalid antenna height/.test(w)), 'no warning for the rejected heights');
});

test('a numeric string height is stored as a number', () => {
  const { api, localStorage } = load();
  api.writeOverrides({ terrain: { heightRepeater: '18.5' } });
  assert.strictEqual(JSON.parse(localStorage.getItem('cs-theme-overrides')).terrain.heightRepeater, 18.5);
});

test('the model is dtm or both, and nothing else', () => {
  const { api, localStorage } = load();
  ['dtm', 'both'].forEach((m) => {
    api.writeOverrides({ terrain: { model: m } });
    assert.strictEqual(JSON.parse(localStorage.getItem('cs-theme-overrides')).terrain.model, m);
  });
  // "dsm" is a real elevation model but not an offer the customizer makes:
  // the surface model never decides the verdict (#681 owner decision 6).
  ['dsm', 'DTM', '', 'both ', 1, null, {}].forEach((m) => {
    api.writeOverrides({ terrain: { model: m, heightSensor: 4 } });
    const got = JSON.parse(localStorage.getItem('cs-theme-overrides')).terrain;
    assert.strictEqual(got.model, undefined, 'accepted model ' + JSON.stringify(m));
    assert.strictEqual(got.heightSensor, 4, 'a bad model cost a good height');
  });
});

test('an unknown terrain key is dropped', () => {
  const { api, localStorage } = load();
  api.writeOverrides({ terrain: { heightRepeater: 12, kFactor: 1.6, token: 'secret', freq: 868.1 } });
  const got = JSON.parse(localStorage.getItem('cs-theme-overrides')).terrain;
  assert.deepStrictEqual(got, { heightRepeater: 12 });
});

test('a chart colour must be a valid CSS colour', () => {
  const { api, localStorage } = load();
  api.writeOverrides({ theme: {
    terrainFill: '#9a8a73',
    terrainClutter: 'rgba(84, 124, 92, 0.55)',
    losLine: 'url(https://evil.test/x)',
    fresnel60: 'red; background:url(x)',
    profileBlocked: 'expression(alert(1))',
  } });
  const got = JSON.parse(localStorage.getItem('cs-theme-overrides')).theme;
  assert.deepStrictEqual(got, { terrainFill: '#9a8a73', terrainClutter: 'rgba(84, 124, 92, 0.55)' });
});

test('validateShape reports every bad terrain value by name', () => {
  const { api } = load();
  const res = api.validateShape({ terrain: { heightRepeater: 400, model: 'dsm' } });
  assert.strictEqual(res.valid, false);
  const joined = res.errors.join(' | ');
  assert.ok(/heightRepeater/.test(joined), joined);
  assert.ok(/dsm/.test(joined), joined);
});

test('terrainSettings resolves the effective values over the built-ins', () => {
  const { api } = load();
  const t = api.terrainSettings({ terrain: { heightRepeater: 18, model: 'dtm', heightRoom: 'tall' } });
  assert.strictEqual(t.heightRepeater, 18);
  assert.strictEqual(t.heightRoom, 5, 'a junk value must fall back to the built-in');
  assert.strictEqual(t.model, 'dtm');
  const none = api.terrainSettings({});
  assert.deepStrictEqual(Object.assign({}, none), Object.assign({}, api.TERRAIN_DEFAULTS));
});

// ── 2b. Review PR#714: one height rule, and no way round it ──────────────

console.log('\n=== review PR#714: the height rule cannot be bypassed ===');

/* Every one of these got past the old parseFloat check: "0x1F4" read as 0
 * and was stored as Number("0x1F4") = 500, "5abc" was stored as NaN. */
const NOT_A_HEIGHT = ['0x1F4', '0x10', '5abc', '1e2', ' 5', '5 ', '-1', '+5', '100.5', 'NaN', 'Infinity',
  NaN, Infinity, -Infinity, -1, 100.5, true, '0b11', '.5', '5.'];

test('hex, exponents, padding, signs and out-of-range heights are refused', () => {
  const { api } = load();
  NOT_A_HEIGHT.forEach((v) => {
    assert.strictEqual(api.isValidAntennaHeight(v), false, 'accepted ' + JSON.stringify(String(v)));
  });
});

test('a refused height is never stored, whatever Number() would make of it', () => {
  NOT_A_HEIGHT.forEach((v) => {
    const { api, localStorage } = load();
    api.writeOverrides({ terrain: { heightRepeater: v, heightSensor: 3 } });
    const got = JSON.parse(localStorage.getItem('cs-theme-overrides')).terrain;
    assert.deepStrictEqual(got, { heightSensor: 3 }, JSON.stringify(String(v)) + ' was stored as ' + JSON.stringify(got));
  });
});

test('an import with a hex or garbage height is refused', () => {
  const { api } = load();
  ['0x1F4', '0x10', '5abc', '1e2', ' 5'].forEach((v) => {
    const res = api.validateShape({ terrain: { heightRepeater: v } });
    assert.strictEqual(res.valid, false, 'the import gate let ' + JSON.stringify(v) + ' through');
  });
});

test('the customizer shows the value the profile will use', () => {
  // The reviewer's repro: {terrain:{heightRepeater:"0x1F4"}} in SITE_CONFIG
  // must not show 10 here while the profile draws 100 m.
  const { api } = load();
  const t = api.terrainSettings({ terrain: { heightRepeater: '0x1F4', heightRoom: '5abc', heightSensor: '12.5' } });
  assert.strictEqual(t.heightRepeater, 10);
  assert.strictEqual(t.heightRoom, 5);
  assert.strictEqual(t.heightSensor, 12.5);
});

test('the customizer and the profile share one height rule', () => {
  const { api, ctx } = load();
  const rule = ctx.window.MCAntennaHeight;
  assert.ok(rule && typeof rule.parse === 'function', 'public/antenna-height.js must define MCAntennaHeight.parse');
  NOT_A_HEIGHT.concat([0, 2, '12', '12.5', 100, '100']).forEach((v) => {
    assert.strictEqual(api.isValidAntennaHeight(v), rule.parse(v) !== null, 'disagree on ' + JSON.stringify(String(v)));
  });
  const profile = fs.readFileSync(path.join(__dirname, 'public', 'terrain-profile.js'), 'utf8');
  assert.ok(/MCAntennaHeight\.parse/.test(CODE), 'customize-v2.js does not use the shared rule');
  assert.ok(/MCAntennaHeight\.parse/.test(profile), 'terrain-profile.js does not use the shared rule');
  const html = fs.readFileSync(path.join(__dirname, 'public', 'index.html'), 'utf8');
  const at = (f) => html.indexOf('src="' + f + '?');
  assert.ok(at('antenna-height.js') > 0, 'index.html does not load antenna-height.js');
  assert.ok(at('antenna-height.js') < at('customize-v2.js'), 'antenna-height.js loads after customize-v2.js');
  assert.ok(at('antenna-height.js') < at('terrain-profile.js'), 'antenna-height.js loads after terrain-profile.js');
});

test('readOverrides drops sections the customizer does not own', () => {
  // Overrides written before unknown sections were dropped on write still
  // sit in localStorage; they must not reach SITE_CONFIG or an export.
  const { api, localStorage } = load();
  localStorage.setItem('cs-theme-overrides', JSON.stringify({
    theme: { accent: '#123456' }, map: { dhm: { token: 'old' } }, radio: { kFactor: 1 },
  }));
  const got = api.readOverrides();
  assert.deepStrictEqual(Object.keys(got), ['theme']);
  const effective = api.computeEffective({}, got);
  assert.strictEqual(effective.map, undefined, 'a stale map section reached the effective config');
});

test('a key named hasOwnProperty does not break the import gate', () => {
  [{ terrain: { hasOwnProperty: 1 } }, { theme: { hasOwnProperty: 1 } }, { hasOwnProperty: 1 }].forEach((obj) => {
    const { api } = load();
    let res;
    assert.doesNotThrow(() => { res = api.validateShape(obj); }, 'validateShape threw on ' + JSON.stringify(obj));
    assert.ok(res && Array.isArray(res.errors), 'no result for ' + JSON.stringify(obj));
    // ...and the write that follows an accepted import does not throw either.
    assert.doesNotThrow(() => api.writeOverrides(JSON.parse(JSON.stringify(obj))), 'writeOverrides threw on ' + JSON.stringify(obj));
  });
});

test('a picked colour keeps the alpha of the colour it replaces', () => {
  // The native picker has no alpha channel, so the first pick for
  // --terrain-clutter (default alpha 0.55) would make the canopy opaque.
  const { api } = load();
  assert.strictEqual(typeof api.colorWithAlpha, 'function');
  assert.strictEqual(api.colorWithAlpha('#ff0000', 'rgba(84, 124, 92, 0.55)'), 'rgba(255, 0, 0, 0.55)');
  assert.strictEqual(api.colorWithAlpha('#ff0000', 'rgb(84 124 92 / 45%)'), 'rgba(255, 0, 0, 0.45)');
  assert.strictEqual(api.colorWithAlpha('#00ff00', '#11223380'), 'rgba(0, 255, 0, 0.502)');
  // An opaque colour stays a plain hex, exactly as before.
  ['#123456', 'rgb(1, 2, 3)', 'rgba(1, 2, 3, 1)', '', null, undefined, 'red'].forEach((cur) => {
    assert.strictEqual(api.colorWithAlpha('#00ff00', cur), '#00ff00', 'changed for ' + JSON.stringify(cur));
  });
  // And the result is still a colour the customizer will store.
  assert.ok(api.isValidColor(api.colorWithAlpha('#ff0000', 'rgba(84, 124, 92, 0.55)')));
});

test('the Terrain Profile section follows the server’s profile switch', () => {
  const on = { enabled: true, profile: true };
  const off = [null, undefined, { enabled: true, profile: false }, { enabled: false, profile: true }];
  const { api, ctx } = load({ dhm: on });
  ctx.window.TerrainProfile = { offered: (c) => !!(c && c.enabled === true && c.profile === true) };
  assert.ok(/Terrain Profile/.test(api.renderTerrainSettings()), 'hidden while the profile is on');
  off.forEach((cfg) => {
    ctx.window.MC_DHM_CFG = cfg;
    assert.strictEqual(api.renderTerrainSettings(), '', 'shown with dhm ' + JSON.stringify(cfg));
  });
  // No profile tool on the page at all: nothing to set defaults for.
  ctx.window.MC_DHM_CFG = on;
  delete ctx.window.TerrainProfile;
  assert.strictEqual(api.renderTerrainSettings(), '');
});

// ── 3. Export / import ───────────────────────────────────────────────────

console.log('\n=== export / import ===');

test('the terrain section and its colours survive an export round trip', () => {
  const first = load();
  const delta = {
    terrain: { heightRepeater: 18, heightPoint: 1.5, model: 'dtm' },
    theme: { terrainFill: '#9a8a73', losLine: '#1d4ed8' },
    themeDark: { terrainFill: '#5d5444', profileBlocked: '#f87171' },
  };
  first.api.writeOverrides(delta);
  const exported = first.api.readOverrides();
  assert.deepStrictEqual(JSON.parse(JSON.stringify(exported)), delta);

  // Import into a fresh browser, the way the Import File button does.
  const second = load();
  const shape = second.api.validateShape(exported);
  assert.strictEqual(shape.valid, true, shape.errors.join('; '));
  second.api.writeOverrides(exported);
  assert.deepStrictEqual(JSON.parse(JSON.stringify(second.api.readOverrides())), delta);
});

test('an import with a bad height is refused before anything is written', () => {
  const { api } = load();
  const res = api.validateShape({ terrain: { heightRepeater: 400 } });
  assert.strictEqual(res.valid, false, 'the import gate let an out-of-range height through');
});

// ── 4. The server-only side of map.dhm never reaches the client ───────────

console.log('\n=== map.dhm stays on the server ===');

test('the customizer surface names nothing under map.dhm', () => {
  const { api } = load();
  // The whole surface the customizer owns is VALID_SECTIONS. A token is not
  // a theme value; neither is a rate limit, a cache size or an upstream URL.
  assert.ok(api.VALID_SECTIONS.indexOf('map') === -1, 'map is a customizer section');
  assert.ok(api.VALID_SECTIONS.indexOf('dhm') === -1, 'dhm is a customizer section');
  Object.keys(api.THEME_CSS_MAP).forEach((k) => {
    assert.ok(!/token|dhm/i.test(k), 'theme key looks server-side: ' + k);
  });
  Object.keys(api.TERRAIN_DEFAULTS).forEach((k) => {
    assert.ok(!/token|freq|kfactor|url|rate|cache|timeout|chunk|path/i.test(k),
      'terrain key looks server-side: ' + k);
  });
});

test('an imported theme file cannot plant map.dhm.token in the client', () => {
  const { api, localStorage } = load();
  const hostile = {
    theme: { terrainFill: '#9a8a73' },
    map: { dhm: { enabled: true, token: 'not-a-real-token', url: 'https://evil.test/wcs' } },
    radio: { kFactor: 0.6, defaultFreqMHz: 100 },
  };
  api.writeOverrides(hostile);
  const raw = localStorage.getItem('cs-theme-overrides');
  assert.ok(raw.indexOf('token') === -1, 'a token was persisted: ' + raw);
  assert.ok(raw.indexOf('not-a-real-token') === -1, 'a token was persisted: ' + raw);
  assert.ok(raw.indexOf('kFactor') === -1, 'server physics was persisted: ' + raw);
  const stored = JSON.parse(raw);
  assert.deepStrictEqual(Object.keys(stored), ['theme']);
  // And it must not reach window.SITE_CONFIG through the merge either.
  const effective = api.computeEffective({}, api.readOverrides());
  assert.strictEqual(effective.map, undefined, 'map reached the effective config');
  assert.strictEqual(effective.radio, undefined, 'radio reached the effective config');
});

test('a server-side section already in config is not re-exported by the customizer', () => {
  // The server never sends map.dhm to the browser (asserted in Go:
  // cmd/server/terrain_profile_test.go). Even if something did, an export is
  // the user's delta, so it must stay out of it.
  const { api } = load({ server: { map: { dhm: { enabled: true } }, theme: { accent: '#4a9eff' } } });
  api.writeOverrides({ theme: { terrainFill: '#101010' } });
  const exported = JSON.stringify(api.readOverrides());
  assert.ok(exported.indexOf('dhm') === -1, 'an export carried map.dhm: ' + exported);
});

test('no customizer control is wired to a server-side DHM value', () => {
  // A source-level gate: the customizer renders its fields from
  // data-cv2-* hooks, and none of them may name a server-side key.
  const hooks = SOURCE.match(/data-cv2-[a-z-]+="[^"]*"/g) || [];
  hooks.forEach((h) => {
    assert.ok(!/token|dhm|kfactor|freq|ratelimit|cachettl|maxpath|chunk/i.test(h),
      'customizer field bound to a server-side value: ' + h);
  });
  assert.ok(hooks.some(h => /terrain-height/.test(h)), 'the antenna-height fields are not wired');
  assert.ok(CODE.indexOf('map.dhm') === -1, 'customizer code names map.dhm');
  assert.ok(!/['"]token['"]/.test(CODE), 'customizer code names a token key');
});

// ── 5. The live-update signal ────────────────────────────────────────────

console.log('\n=== the terrain-defaults-changed signal ===');

function events(ctx) {
  const seen = [];
  ctx.window.dispatchEvent = (ev) => { seen.push(ev && ev.type); return true; };
  return seen;
}

test('a changed antenna default announces itself once the pipeline has run', () => {
  const { api, ctx } = load({ server: {} });
  const seen = events(ctx);
  api.writeOverrides({ terrain: { heightRepeater: 45 } });
  api.init({});
  assert.ok(seen.indexOf('terrain-defaults-changed') >= 0,
    'no signal for a changed antenna default: ' + seen.join(', '));
});

test('a chart colour costs no re-measure', () => {
  // A colour is pure CSS. Announcing it would make every drag of a colour
  // picker re-request a profile.
  const { api, ctx } = load({ server: {} });
  api.init({});
  const seen = events(ctx);
  api.applyCSS(api.computeEffective({}, { theme: { terrainFill: '#123456' } }), { theme: { terrainFill: '#123456' } });
  assert.strictEqual(seen.indexOf('terrain-defaults-changed'), -1,
    'a colour change announced a terrain default change');
  assert.ok(seen.indexOf('theme-changed') >= 0, 'the usual theme-changed event went missing');
});

test('an unchanged section is silent', () => {
  const { api, ctx } = load({ server: {} });
  api.writeOverrides({ terrain: { heightRepeater: 45 } });
  api.init({});
  const seen = events(ctx);
  api.applyCSS(api.computeEffective({}, api.readOverrides()), api.readOverrides());
  assert.strictEqual(seen.indexOf('terrain-defaults-changed'), -1, 'a no-op run announced a change');
});

// ── 6. Presets ───────────────────────────────────────────────────────────

console.log('\n=== presets ===');

test('recolouring the profile chart does not unselect a theme preset', () => {
  // No PRESET defines the terrain keys, so counting them in the match would
  // make the Theme tab forget which preset is active the moment somebody
  // picks a chart colour.
  const { api } = load({ server: {} });
  assert.strictEqual(api.detectActivePreset(), 'default', 'baseline is not the default preset');
  api.writeOverrides({ theme: { terrainFill: '#9a8a73' } });
  api.init({});
  assert.strictEqual(api.detectActivePreset(), 'default', 'a chart colour unselected the preset');
});

// ── Summary ──────────────────────────────────────────────────────────────

console.log('\n' + passed + ' passed, ' + failed + ' failed\n');
if (failed > 0) process.exit(1);
