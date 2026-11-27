/**
 * #882 M2 (AGENTS rule 8) — the Live scope palette and the scope legend
 * window in the customizer.
 *
 * The real public/customize-v2.js, public/live-scope-color.js and
 * public/cb-presets.js run in vm sandboxes; nothing is copied.
 *   1. palette keys: THEME_CSS_MAP and THEME_COLOR_KEYS carry the 15 scope
 *      keys; every theme preset has a light and a dark value for each; the
 *      Default preset is the stylesheet (no change at the defaults);
 *   2. applyCSS: a pick reaches --scope-N; a value equal to the stylesheet
 *      default leaves the cascade to style.css (and a colourblind preset);
 *   3. colourblind presets: a light and a dark palette in every preset,
 *      >= 3:1 against the map tile, 12 distinct slots; clearPreset and
 *      style.css wiring;
 *   4. legend window rule (LiveScopeColor.SETTINGS): keys, defaults,
 *      bounds; the registry follows it;
 *   5. the liveScope section: storage, import validation, export round
 *      trip, the Display badge, the controls (createElement, bounded inputs).
 * Usage: node test-882-m2-scope-customizer.js
 */
'use strict';
const fs = require('fs');
const path = require('path');
const vm = require('vm');
const assert = require('assert');
const { loadCustomizer } = require('./tests/customizer-sandbox');

let passed = 0, failed = 0;
const tests = [];
function test(name, fn) { tests.push([name, fn]); }
const plain = (v) => JSON.parse(JSON.stringify(v === undefined ? null : v));
const eq = (a, b, m) => assert.deepStrictEqual(plain(a), b, m);

const SCOPE_KEYS = ['scope1', 'scope2', 'scope3', 'scope4', 'scope5', 'scope6', 'scope7', 'scope8', 'scope9',
  'scope10', 'scope11', 'scope12', 'scopeNone', 'scopeUnknown', 'scopeOther'];
const VAR = (k) => (k === 'scopeNone' ? '--scope-none' : k === 'scopeUnknown' ? '--scope-unknown' : k === 'scopeOther' ? '--scope-other' : '--scope-' + k.slice(5));
const SUFFIX = (k) => VAR(k).replace('--scope-', '');
const HEX = /^#[0-9a-f]{6}$/i;

const css = fs.readFileSync(path.join(__dirname, 'public/style.css'), 'utf8');

// The stylesheet's default for one scope variable: the fallback of the
// first declaration (light :root) or of the dark blocks.
function cssDefaults(mode) {
  const out = {};
  SCOPE_KEYS.forEach((k) => {
    const s = SUFFIX(k);
    const re = new RegExp('^\\s*' + VAR(k) + ':\\s*var\\(--mc-scope-' + mode + '-' + s + ',\\s*(#[0-9a-f]{6})\\);', 'gim');
    const all = [...css.matchAll(re)].map((m) => m[1].toLowerCase());
    out[k] = all;
  });
  return out;
}

function load(o) {
  o = o || {};
  return loadCustomizer({ theme: o.theme, scripts: o.noRule ? [] : ['live-scope-color.js'], setup: o.setup });
}

// ── 1. Palette keys ─────────────────────────────────────────────────────────

test('THEME_CSS_MAP maps the 15 scope keys; all are THEME_COLOR_KEYS (preset detection)', () => {
  const { api } = load();
  SCOPE_KEYS.forEach((k) => assert.strictEqual(api.THEME_CSS_MAP[k], VAR(k), k));
  eq(api.SCOPE_COLOR_KEYS, SCOPE_KEYS);
  SCOPE_KEYS.forEach((k) => assert.ok(api.THEME_COLOR_KEYS.indexOf(k) >= 0, k + ' not in THEME_COLOR_KEYS'));
});

test('every theme preset has a light and a dark value for every scope key', () => {
  const { api } = load();
  const ids = Object.keys(api.PRESETS);
  assert.ok(ids.length >= 8, 'presets: ' + ids);
  ids.forEach((id) => {
    const p = api.PRESETS[id];
    SCOPE_KEYS.forEach((k) => {
      assert.ok(HEX.test(p.theme[k] || ''), id + ' light ' + k + ' = ' + p.theme[k]);
      assert.ok(HEX.test(p.themeDark[k] || ''), id + ' dark ' + k + ' = ' + p.themeDark[k]);
    });
  });
});

test('no change at the defaults: style.css keeps the M1 values, the Default preset is the stylesheet', () => {
  const M1_LIGHT = ['#0072b2', '#009e73', '#b35c00', '#7e3fa0', '#a8780b', '#00838f', '#b0306f', '#4b5eaa', '#5e8b2f', '#8b4513', '#c026d3', '#6d6d00', '#a1a1aa', '#57534e', '#111827'];
  const M1_DARK = ['#3d8bfd', '#3fd7a5', '#f0a03c', '#c49be0', '#e6c34a', '#4dd0e1', '#f06292', '#8c9eff', '#a5d66b', '#d7a07a', '#e879f9', '#dce775', '#6b7280', '#c4b5a5', '#f8fafc'];
  const L = cssDefaults('light'), D = cssDefaults('dark');
  const { api } = load();
  SCOPE_KEYS.forEach((k, i) => {
    eq(L[k], [M1_LIGHT[i]], 'style.css light ' + k);
    eq(D[k], [M1_DARK[i], M1_DARK[i]], 'style.css dark blocks ' + k);
    assert.strictEqual(api.PRESETS.default.theme[k], M1_LIGHT[i], 'Default preset light ' + k);
    assert.strictEqual(api.PRESETS.default.themeDark[k], M1_DARK[i], 'Default preset dark ' + k);
  });
});

test('preset detection: a pre-M2 preset pick (no scope keys) still matches, a recoloured scope does not', () => {
  const { api, ctx } = load();
  const p = api.PRESETS.ocean;
  const strip = (o) => { const c = Object.assign({}, o); SCOPE_KEYS.forEach((k) => delete c[k]); return c; };
  ctx.window.SITE_CONFIG = { theme: strip(p.theme), themeDark: strip(p.themeDark) };
  assert.strictEqual(api.detectActivePreset(), 'ocean');
  ctx.window.SITE_CONFIG = { theme: Object.assign({}, p.theme), themeDark: Object.assign({}, p.themeDark) };
  assert.strictEqual(api.detectActivePreset(), 'ocean', 'a full M2 preset pick');
  ctx.window.SITE_CONFIG = { theme: Object.assign({}, p.theme, { scope3: '#123456' }), themeDark: Object.assign({}, p.themeDark) };
  assert.strictEqual(api.detectActivePreset(), null);
});

test('Theme tab: a "Live scope colours" group with a row per key, for the mode being edited', () => {
  const { api } = load();
  const html = api.renderTheme();
  const m = html.match(/<details class="cust-scope-colors"[\s\S]*?<\/details>/);
  assert.ok(m, 'no cust-scope-colors group');
  SCOPE_KEYS.forEach((k) => assert.ok(m[0].includes('data-cv2-field="theme.' + k + '"'), k));
  assert.ok(/Live scope colours \(15 options\)/.test(m[0]), 'summary');
  const dark = load({ theme: 'dark' }).api.renderTheme();
  assert.ok(dark.includes('data-cv2-field="themeDark.scope1"'), 'dark mode edits themeDark');
});

// ── 2. applyCSS ─────────────────────────────────────────────────────────────

test('applyCSS: a pick reaches --scope-N, light and dark', () => {
  const c = load();
  c.api.init({});
  c.api.writeOverrides({ theme: { scope3: '#112233', scopeNone: '#445566' } });
  c.api.applyCSS(c.api.computeEffective({}, c.api.readOverrides()), c.api.readOverrides());
  assert.strictEqual(c.rootStyle._props['--scope-3'], '#112233');
  assert.strictEqual(c.rootStyle._props['--scope-none'], '#445566');
  const d = load({ theme: 'dark' });
  d.api.init({});
  d.api.writeOverrides({ themeDark: { scope3: '#abcdef' } });
  d.api.applyCSS(d.api.computeEffective({}, d.api.readOverrides()), d.api.readOverrides());
  assert.strictEqual(d.rootStyle._props['--scope-3'], '#abcdef');
});

test('applyCSS: a value equal to the stylesheet default (a preset pick) leaves --scope-N to style.css; a cleared pick is removed', () => {
  const c = load();
  c.api.init({});
  const p = c.api.PRESETS.ocean;
  c.api.writeOverrides({ theme: Object.assign({}, p.theme), themeDark: Object.assign({}, p.themeDark) });
  c.api.applyCSS(c.api.computeEffective({}, c.api.readOverrides()), c.api.readOverrides());
  SCOPE_KEYS.forEach((k) => assert.strictEqual(c.rootStyle._props[VAR(k)], undefined, k + ' stamped inline: a colourblind preset could not win'));
  c.api.writeOverrides({ theme: { scope1: '#010203' } });
  c.api.applyCSS(c.api.computeEffective({}, c.api.readOverrides()), c.api.readOverrides());
  assert.strictEqual(c.rootStyle._props['--scope-1'], '#010203');
  c.api.writeOverrides({});
  c.api.applyCSS(c.api.computeEffective({}, c.api.readOverrides()), c.api.readOverrides());
  assert.strictEqual(c.rootStyle._props['--scope-1'], undefined, 'a cleared pick stays inline');
});

// ── 3. Colourblind presets ──────────────────────────────────────────────────

function loadPresets() {
  const props = {};
  const style = { setProperty: (k, v) => { props[k] = v; }, removeProperty: (k) => { delete props[k]; }, getPropertyValue: (k) => props[k] || '' };
  const ctx = { window: { dispatchEvent() {}, CustomEvent: class {}, addEventListener() {} }, document: { body: { setAttribute() {}, removeAttribute() {} }, documentElement: { style } },
    localStorage: { getItem: () => null, setItem() {}, removeItem() {} }, console };
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'public/cb-presets.js'), 'utf8'), ctx, { filename: 'cb-presets.js' });
  return { MCP: ctx.window.MeshCorePresets, props };
}
const CB_IDS = ['default', 'deut', 'prot', 'trit', 'achromat'];

test('cb-presets: every preset sets a light and a dark value for all 15 scope colours; clearPreset removes them', () => {
  const { MCP, props } = loadPresets();
  CB_IDS.forEach((id) => {
    assert.ok(MCP.applyPreset(id, { skipPersist: true }), id);
    ['light', 'dark'].forEach((mode) => SCOPE_KEYS.forEach((k) => {
      const v = props['--mc-scope-' + mode + '-' + SUFFIX(k)];
      assert.ok(HEX.test(v || ''), id + ' ' + mode + ' ' + k + ' = ' + v);
    }));
  });
  MCP.clearPreset();
  eq(Object.keys(props).filter((k) => /scope/.test(k)), []);
});

test('cb-presets: the Default (Wong) preset is the stylesheet palette', () => {
  const { MCP, props } = loadPresets();
  MCP.applyPreset('default', { skipPersist: true });
  const L = cssDefaults('light'), D = cssDefaults('dark');
  SCOPE_KEYS.forEach((k) => {
    assert.strictEqual(props['--mc-scope-light-' + SUFFIX(k)].toLowerCase(), L[k][0], 'light ' + k);
    assert.strictEqual(props['--mc-scope-dark-' + SUFFIX(k)].toLowerCase(), D[k][0], 'dark ' + k);
  });
});

test('cb-presets: tuned palettes have 12 distinct slots, each >= 3:1 on the map tile of its mode (WCAG 1.4.11)', () => {
  const { MCP, props } = loadPresets();
  const C = MCP.wcag.contrast;
  ['deut', 'prot', 'trit', 'achromat'].forEach((id) => {
    MCP.applyPreset(id, { skipPersist: true });
    ['light', 'dark'].forEach((mode) => {
      const tile = mode === 'light' ? MCP.wcag.TILE_LIGHT : MCP.wcag.TILE_DARK;
      const slots = SCOPE_KEYS.slice(0, 12).map((k) => props['--mc-scope-' + mode + '-' + SUFFIX(k)].toLowerCase());
      assert.strictEqual(new Set(slots).size, 12, id + ' ' + mode + ' slots repeat');
      slots.forEach((c, i) => assert.ok(C(c, tile) >= 3, id + ' ' + mode + ' slot ' + (i + 1) + ' ' + c + ' ' + C(c, tile).toFixed(2)));
      const n = props['--mc-scope-' + mode + '-none'], u = props['--mc-scope-' + mode + '-unknown'];
      assert.notStrictEqual(n.toLowerCase(), u.toLowerCase(), id + ' ' + mode + ' none = unknown');
    });
  });
});

test('style.css: each --scope-* reads the colourblind preset hook of its mode, once per block', () => {
  const L = cssDefaults('light'), D = cssDefaults('dark');
  SCOPE_KEYS.forEach((k) => {
    assert.strictEqual(L[k].length, 1, VAR(k) + ' light declarations: ' + L[k].length);
    assert.strictEqual(D[k].length, 2, VAR(k) + ' dark declarations (media + [data-theme=dark]): ' + D[k].length);
    const plainDecl = css.split('\n').filter((l) => new RegExp('^\\s*' + VAR(k) + ':\\s*#').test(l));
    eq(plainDecl, [], VAR(k) + ' still has a bare hex declaration');
  });
});

test('Reset All strips the colourblind scope variables too', () => {
  const c = load();
  c.api.init({});
  c.rootStyle.setProperty('--mc-scope-light-3', '#000000');
  c.rootStyle.setProperty('--mc-scope-dark-none', '#000000');
  c.api.resetAll();
  assert.strictEqual(c.rootStyle._props['--mc-scope-light-3'], undefined);
  assert.strictEqual(c.rootStyle._props['--mc-scope-dark-none'], undefined);
});

// ── 4. The legend window rule ───────────────────────────────────────────────

function loadScope() {
  const ctx = { window: {}, console };
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'public/live-scope-color.js'), 'utf8'), ctx, { filename: 'live-scope-color.js' });
  return ctx.window.LiveScopeColor;
}

test('SETTINGS: the M0 window (15 min, 12 rows) is the default; cap 64 stays fixed', () => {
  const S = loadScope();
  const R = S.SETTINGS;
  eq(R.KEYS, ['windowMinutes', 'legendRows']);
  eq(R.DEFAULTS, { windowMinutes: 15, legendRows: 12 });
  eq(R.LIMITS, { windowMinutes: [1, 60], legendRows: [1, 12] });
  assert.strictEqual(S.CAP, 64);
  assert.strictEqual(S.WINDOW_MS, 15 * 60000);
  assert.strictEqual(S.LEGEND_ROWS, 12);
  eq(R.resolve({}), plain(R.DEFAULTS));
  eq(R.resolve(null), plain(R.DEFAULTS));
});

test('SETTINGS.valid: whole numbers inside the bounds only', () => {
  const R = loadScope().SETTINGS;
  [[1, true], [60, true], [15, true], [0, false], [61, false], [2.5, false], ['15', false], [null, false], [NaN, false], [Infinity, false], [true, false]]
    .forEach(([v, ok]) => assert.strictEqual(R.valid('windowMinutes', v), ok, 'windowMinutes ' + v));
  [[1, true], [12, true], [0, false], [13, false], [6.5, false], ['6', false]]
    .forEach(([v, ok]) => assert.strictEqual(R.valid('legendRows', v), ok, 'legendRows ' + v));
  assert.strictEqual(R.valid('cap', 10), false, 'an unknown key');
  eq(R.resolve({ windowMinutes: 5, legendRows: 99, extra: 1 }), { windowMinutes: 5, legendRows: 12 });
});

test('registry: the window and the row count follow configure(), defaults unchanged', () => {
  const S = loadScope();
  const r = S.createRegistry();
  const t0 = 1e9;
  for (let i = 0; i < 14; i++) r.touch('#s' + i, t0 + i * 60000); // one a minute
  const now = t0 + 13 * 60000;
  let L = r.legend(now);
  eq([L.rows.length, L.more, L.windowMinutes], [12, 2, 15]);
  r.configure({ windowMinutes: 5, legendRows: 3 });
  L = r.legend(now);
  // A 5 min window (inclusive) keeps the 6 scopes seen at minutes 8..13.
  eq([L.rows.length, L.more, L.windowMinutes], [3, 3, 5]);
  eq(L.rows.map((x) => x.name), ['#s11', '#s12', '#s13'], 'the most recent rows');
  r.configure(null);
  eq([r.legend(now).rows.length, r.legend(now).more], [12, 2], 'configure(null) restores the defaults');
});

test('registry: a shorter window frees colour slots sooner; a bad configure() keeps the defaults', () => {
  const S = loadScope();
  const r = S.createRegistry({ windowMinutes: 2, legendRows: 12 });
  const t0 = 1e9;
  r.touch('#a', t0);
  const L = r.legend(t0 + 3 * 60000);
  eq([L.rows.length, L.windowMinutes], [0, 2], 'a scope older than the window is still listed');
  r.configure({ windowMinutes: 'x', legendRows: 0 });
  eq(r.settings(), { windowMinutes: 15, legendRows: 12 });
});

// ── 5. The liveScope section ────────────────────────────────────────────────

test('liveScope is a known object section; values by the module rule', () => {
  const c = load();
  assert.ok(c.api.VALID_SECTIONS.indexOf('liveScope') >= 0);
  eq(c.api.liveScopeSettings({}), { windowMinutes: 15, legendRows: 12 });
  c.api.writeOverrides({ liveScope: { windowMinutes: 30, legendRows: 13, cap: 10 } });
  eq(c.api.readOverrides(), { liveScope: { windowMinutes: 30 } });
  assert.ok(c.warnings.some((w) => /liveScope\.legendRows/.test(w)), 'a rejected value is logged');
  eq(c.api.liveScopeSettings(c.api.computeEffective({}, c.api.readOverrides())), { windowMinutes: 30, legendRows: 12 });
});

test('without the module on the page no value is accepted', () => {
  const c = load({ noRule: true });
  c.api.writeOverrides({ liveScope: { windowMinutes: 30 } });
  eq(c.api.readOverrides(), {});
  assert.strictEqual(c.api.liveScopeSettings({}), null);
});

test('validateShape names each bad value and its range; unknown keys only warn', () => {
  const { api, warnings } = load();
  const res = api.validateShape({ liveScope: { windowMinutes: 0, legendRows: 2.5, cap: 64 } });
  assert.strictEqual(res.valid, false);
  const txt = res.errors.join('\n');
  assert.ok(/liveScope\.windowMinutes = "0".*1-60/.test(txt), txt);
  assert.ok(/liveScope\.legendRows = "2.5".*1-12/.test(txt), txt);
  assert.ok(!/cap/.test(txt), 'an unknown key is not an error');
  assert.ok(warnings.some((w) => /Unknown liveScope key.*cap/.test(w)));
  assert.strictEqual(api.validateShape({ liveScope: 'on' }).valid, false);
  assert.strictEqual(api.validateShape({ liveScope: { windowMinutes: 5 } }).valid, true);
});

test('export / import round trip: palette and window', () => {
  const delta = { theme: { scope1: '#102030', scopeUnknown: '#405060' }, themeDark: { scope12: '#a0b0c0' }, liveScope: { windowMinutes: 45, legendRows: 6 } };
  const a = load();
  a.api.writeOverrides(delta);
  const exported = JSON.parse(JSON.stringify(a.api.readOverrides()));
  eq(exported, delta);
  const b = load();
  assert.strictEqual(b.api.validateShape(exported).valid, true, JSON.stringify(b.api.validateShape(exported).errors));
  b.api.writeOverrides(exported);
  eq(b.api.readOverrides(), delta);
  b.api.init({});
  b.api.applyCSS(b.api.computeEffective({}, b.api.readOverrides()), b.api.readOverrides());
  assert.strictEqual(b.rootStyle._props['--scope-1'], '#102030');
  assert.strictEqual(b.rootStyle._props['--scope-unknown'], '#405060');
  assert.ok(b.api.validateShape({ theme: { scope2: 'red;}' } }).valid === false, 'a bad colour is accepted on import');
});

test('persistence: the section is stored per browser in cs-theme-overrides', () => {
  const c = load();
  c.api.writeOverrides({ liveScope: { legendRows: 4 } });
  eq(JSON.parse(c.localStorage.getItem('cs-theme-overrides')), { liveScope: { legendRows: 4 } });
});

test('the Display badge counts the section', () => {
  const c = load();
  const before = c.api.countOverrides('liveScope');
  c.api.writeOverrides({ liveScope: { windowMinutes: 5, legendRows: 3 } });
  c.ctx.window.SITE_CONFIG = c.api.computeEffective({}, c.api.readOverrides());
  eq([before, c.api.countOverrides('liveScope')], [0, 2]);
  assert.ok(/data-tab="display"[^>]*>[\s\S]*?cv2-tab-badge">2</.test(c.api.renderTabs()), 'badge');
});

// A small DOM for the createElement-built section.
function fakeDoc() {
  function el(tag) {
    return {
      tagName: tag.toUpperCase(), children: [], attrs: {}, style: {}, className: '', textContent: '', dataset: {},
      appendChild(c) { this.children.push(c); return c; },
      setAttribute(n, v) { this.attrs[n] = String(v); if (n.indexOf('data-') === 0) this.dataset[n.slice(5).replace(/-([a-z0-9])/g, (_, x) => x.toUpperCase())] = String(v); },
      getAttribute(n) { return n in this.attrs ? this.attrs[n] : null; },
      addEventListener() {},
      set innerHTML(v) { throw new Error('innerHTML used: ' + v); },
    };
  }
  return { createElement: el, createElementNS: (_ns, tag) => el(tag) };
}
function walk(n, fn) { fn(n); (n.children || []).forEach((c) => walk(c, fn)); }

test('Display section: built with createElement, one bounded number input per key, the current value', () => {
  const c = load();
  c.api.writeOverrides({ liveScope: { legendRows: 7 } });
  c.ctx.window.SITE_CONFIG = c.api.computeEffective({}, c.api.readOverrides());
  const root = c.api.buildLiveScopeSettings(fakeDoc());
  const inputs = [], texts = [];
  walk(root, (n) => { if (n.tagName === 'INPUT') inputs.push(n); if (n.textContent) texts.push(n.textContent); });
  const by = {};
  inputs.forEach((i) => { by[i.attrs['data-cv2-live-scope']] = i; });
  eq(Object.keys(by).sort(), ['legendRows', 'windowMinutes']);
  eq([by.windowMinutes.attrs.type, by.windowMinutes.attrs.min, by.windowMinutes.attrs.max, by.windowMinutes.value], ['number', '1', '60', '15']);
  eq([by.legendRows.attrs.min, by.legendRows.attrs.max, by.legendRows.value], ['1', '12', '7']);
  assert.ok(texts.some((t) => /Live scope legend/.test(t)), 'section title');
  assert.ok(root.attrs['data-cv2-live-scope-section'] !== undefined, 'section hook');
  assert.strictEqual(c.api.buildLiveScopeSettings(fakeDoc()) !== null, true);
  assert.strictEqual(load({ noRule: true }).api.buildLiveScopeSettings(fakeDoc()), null, 'shown without the module');
});

test('Display panel: a mount point for the section, after the other Live sections', () => {
  const html = load().api.renderDisplay();
  assert.ok(/<div data-cv2-live-scope-mount><\/div>/.test(html), 'no mount point');
});

test('CUSTOMIZATION.md names both window keys with their bounds and every palette key', () => {
  const doc = fs.readFileSync(path.join(__dirname, 'docs/CUSTOMIZATION.md'), 'utf8');
  assert.ok(/`liveScope\.windowMinutes` \(1 to 60, default 15\)/.test(doc), 'windowMinutes');
  assert.ok(/`liveScope\.legendRows` \(1 to 12, default 12\)/.test(doc), 'legendRows');
  ['scope1', 'scope12', 'scopeNone', 'scopeUnknown', 'scopeOther'].forEach((k) => assert.ok(doc.includes('`' + k + '`'), k));
});

for (const [name, fn] of tests) {
  try { fn(); passed++; console.log('  ✅ ' + name); } catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}
console.log('\n' + passed + ' passed, ' + failed + ' failed');
process.exit(failed ? 1 : 0);
