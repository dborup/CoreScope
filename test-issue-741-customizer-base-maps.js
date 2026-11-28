/* test-issue-741-customizer-base-maps.js — #741 customizer follow-up
 * (AGENTS rule 8): the Dataforsyningen base maps as the viewer's default
 * light and dark base-map choice in the customizer.
 *
 * The real files run in vm sandboxes; nothing is copied. The tile registry
 * (public/map-tile-providers.js) runs in tests/map-overlay-sandbox.js with
 * the operator's map.tiles block; the customizer (public/customize-v2.js)
 * runs in tests/customizer-sandbox.js and is handed that registry's public
 * functions and the same localStorage, as the page does through window.
 *
 * Covered:
 *   O  Options: with the gate on, the light list offers Skærmkort standard,
 *      grå, dæmpet and Natur- og Friluftskort, and the dark list Skærmkort
 *      grå (the dark style #805 chose); with the gate off, or for a near
 *      miss (no token, enabled as a string), none of them in either list.
 *   S  A stale saved choice: a stored Dataforsyningen light/dark choice
 *      while the gate is off renders the built-in default as selected, and
 *      every map resolves the built-in default (a tile URL, no WMS), so no
 *      map is blank. The saved value is left alone, so it comes back if
 *      the operator turns the gate on again.
 *   R  Reset all: clears the light choice as well as the dark one, and
 *      re-applies the effective light provider so open maps swap back.
 *   X  Reset all is offered (Export tab) when the only customization is a
 *      base-map choice, light or dark — not only with a theme override.
 *
 * No token is real and nothing is fetched.
 *
 * Runs via: node test-issue-741-customizer-base-maps.js
 */
'use strict';
const assert = require('assert');
const { makeSandbox } = require('./tests/map-overlay-sandbox');
const { loadCustomizer } = require('./tests/customizer-sandbox');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

const TOKEN = 'e2e-741-dummy-token';
const LIGHT_DF = ['dataforsyningen-skaermkort', 'dataforsyningen-skaermkort-graa',
  'dataforsyningen-skaermkort-daempet', 'dataforsyningen-natur-friluft'];
const DARK_DF = ['dataforsyningen-skaermkort-graa-dark'];
const ON = { tiles: { providers: { dataforsyningen: { enabled: true, token: TOKEN } } } };
const OFF_CASES = {
  'no map.tiles block': undefined,
  'enabled: false': { tiles: { providers: { dataforsyningen: { enabled: false, token: TOKEN } } } },
  'enabled without a token': { tiles: { providers: { dataforsyningen: { enabled: true } } } },
  'enabled as a string': { tiles: { providers: { dataforsyningen: { enabled: 'true', token: TOKEN } } } },
};
const PAGE_FNS = ['MC_TILE_PROVIDERS', 'MC_DARK_TILE_DEFAULT', 'MC_LIGHT_TILE_DEFAULT',
  'MC_getDarkTileProvider', 'MC_getLightTileProvider', 'MC_setDarkTileProvider', 'MC_setLightTileProvider'];

// The registry with `mapCfg` and `storage`, and the customizer wired to it
// the way the page wires them: through window, over one localStorage.
function page(mapCfg, storage) {
  const reg = makeSandbox({ mapCfg, storage });
  const regEvents = [];
  const regDispatch = reg.window.dispatchEvent;
  reg.window.dispatchEvent = (ev) => { regEvents.push(ev); return regDispatch(ev); };
  const cust = loadCustomizer({
    setup(ctx) {
      PAGE_FNS.forEach((k) => { ctx.window[k] = reg.window[k]; });
      ctx.localStorage = reg.localStorage;
      ctx.window.localStorage = reg.localStorage;
      // esc() reads a div's innerHTML after setting its textContent, as a
      // browser would escape it; the shared stub element keeps them apart.
      const make = ctx.document.createElement;
      ctx.document.createElement = (tag) => {
        const el = make(tag);
        let text = '';
        Object.defineProperty(el, 'textContent', { get: () => text, set: (v) => { text = String(v); } });
        Object.defineProperty(el, 'innerHTML', { get: () => text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;') });
        return el;
      };
    },
  });
  return { reg, regEvents, cust, storage: reg.localStorage };
}

// The options of one rendered <select>, in order, and the selected one.
function select(html, id) {
  const m = html.match(new RegExp('<select id="' + id + '"[^>]*>([\\s\\S]*?)</select>'));
  if (!m) return null;
  const opts = [];
  let selected = null;
  m[1].replace(/<option value="([^"]*)"( selected)?>/g, (_, v, s) => { opts.push(v); if (s) selected = v; return ''; });
  return { opts, selected };
}
function render(p) {
  const html = p.cust.api.renderTileProviderSelector();
  return { html, light: select(html, 'cv2-light-tile-provider'), dark: select(html, 'cv2-dark-tile-provider') };
}
const dfIn = (list) => list.filter((id) => id.indexOf('dataforsyningen-') === 0);

console.log('\n=== #741 customizer: Danish base maps ===');

// ── O. Options ──────────────────────────────────────────────────────────────

test('O1 the customizer exposes its base-map selector for tests', () => {
  assert.strictEqual(typeof page(ON).cust.api.renderTileProviderSelector, 'function');
});

test('O2 gate on: the light list offers Skærmkort standard, grå, dæmpet and Natur- og Friluftskort', () => {
  const r = render(page(ON));
  assert.ok(r.light, 'no light select rendered');
  assert.deepStrictEqual(dfIn(r.light.opts).sort(), LIGHT_DF.slice().sort());
  // The built-in default stays offered next to them.
  assert.ok(r.light.opts.includes('osm-standard'), 'built-in light default missing: ' + r.light.opts);
});

test('O3 gate on: the dark list offers Skærmkort grå (dark) and no light-only style', () => {
  const r = render(page(ON));
  assert.ok(r.dark, 'no dark select rendered');
  assert.deepStrictEqual(dfIn(r.dark.opts), DARK_DF);
  assert.ok(r.dark.opts.includes('osm-dark'), 'built-in dark default missing: ' + r.dark.opts);
});

test('O4 gate on: each option carries its Denmark label', () => {
  const { html } = render(page(ON));
  for (const label of ['Skærmkort (Denmark)', 'Skærmkort grå (Denmark)', 'Skærmkort dæmpet (Denmark)', 'Natur- og Friluftskort (Denmark)']) {
    assert.ok(html.includes('>' + label + '</option>'), 'label missing: ' + label);
  }
});

for (const [name, cfg] of Object.entries(OFF_CASES)) {
  test('O5 gate off (' + name + '): no Dataforsyningen style in either list', () => {
    const r = render(page(cfg));
    assert.deepStrictEqual(dfIn(r.light.opts), [], 'light: ' + r.light.opts);
    assert.deepStrictEqual(dfIn(r.dark.opts), [], 'dark: ' + r.dark.opts);
    assert.ok(!/dataforsyningen|Skærmkort|Friluft/.test(r.html), 'mentioned in the HTML');
  });
}

test('O6 picking a Dataforsyningen style in the customizer stores it and the maps resolve it (gate on)', () => {
  const p = page(ON);
  assert.strictEqual(p.reg.window.MC_setLightTileProvider('dataforsyningen-natur-friluft'), true);
  assert.strictEqual(p.reg.window.MC_setDarkTileProvider('dataforsyningen-skaermkort-graa-dark'), true);
  assert.strictEqual(p.storage.getItem('mc-light-tile-provider'), 'dataforsyningen-natur-friluft');
  const r = render(p);
  assert.strictEqual(r.light.selected, 'dataforsyningen-natur-friluft');
  assert.strictEqual(r.dark.selected, 'dataforsyningen-skaermkort-graa-dark');
  assert.ok(p.reg.window.MC_getTileSpec('light').wms, 'the light map is not the WMS style');
});

// ── S. A stale saved choice ─────────────────────────────────────────────────

const STALE = { 'mc-light-tile-provider': 'dataforsyningen-skaermkort-graa', 'mc-dark-tile-provider': 'dataforsyningen-skaermkort-graa-dark' };

for (const [name, cfg] of Object.entries(OFF_CASES)) {
  test('S1 stale saved choice, gate off (' + name + '): the built-in defaults are selected', () => {
    const r = render(page(cfg, STALE));
    assert.strictEqual(r.light.selected, 'osm-standard');
    assert.strictEqual(r.dark.selected, 'osm-dark');
  });
}

test('S2 stale saved choice, gate off: every map resolves a built-in tile URL, never a WMS or a blank', () => {
  const p = page(OFF_CASES['enabled without a token'], STALE);
  for (const type of ['light', 'dark']) {
    const spec = p.reg.window.MC_getTileSpec(type);
    assert.strictEqual(spec.id, type === 'light' ? 'osm-standard' : 'osm-dark', type + ' id');
    assert.ok(typeof spec.url === 'string' && /openstreetmap/.test(spec.url), type + ' url: ' + spec.url);
    assert.strictEqual(spec.wms, undefined, type + ' still carries a WMS descriptor');
  }
});

test('S3 stale saved choice: the saved value is kept and comes back when the gate is turned on again', () => {
  const off = page(OFF_CASES['enabled: false'], STALE);
  assert.strictEqual(off.storage.getItem('mc-light-tile-provider'), STALE['mc-light-tile-provider']);
  const on = page(ON, STALE);
  const r = render(on);
  assert.strictEqual(r.light.selected, 'dataforsyningen-skaermkort-graa');
  assert.strictEqual(r.dark.selected, 'dataforsyningen-skaermkort-graa-dark');
});

test('S4 stale saved choice with an operator default: the operator default wins over the built-in', () => {
  // #845: CARTO is opt-in now, so the operator who names it turns it on.
  const r = render(page({ tiles: { lightDefault: 'carto-light', darkDefault: 'carto-dark', providers: { carto: { enabled: true } } } }, STALE));
  assert.strictEqual(r.light.selected, 'carto-light');
  assert.strictEqual(r.dark.selected, 'carto-dark');
});

// ── R. Reset all ────────────────────────────────────────────────────────────

test('R1 Reset all clears a saved Dataforsyningen light choice', () => {
  const p = page(ON, { 'mc-light-tile-provider': 'dataforsyningen-natur-friluft' });
  p.cust.api.resetAll();
  assert.strictEqual(p.storage.getItem('mc-light-tile-provider'), null);
  assert.strictEqual(p.reg.window.MC_getLightTileProvider(), 'osm-standard');
});

test('R2 Reset all clears a saved Dataforsyningen dark choice', () => {
  const p = page(ON, { 'mc-dark-tile-provider': 'dataforsyningen-skaermkort-graa-dark' });
  p.cust.api.resetAll();
  assert.strictEqual(p.storage.getItem('mc-dark-tile-provider'), null);
  assert.strictEqual(p.reg.window.MC_getDarkTileProvider(), 'osm-dark');
});

test('R3 Reset all tells open maps to swap the light base map back', () => {
  const p = page(ON, { 'mc-light-tile-provider': 'dataforsyningen-skaermkort' });
  p.cust.api.resetAll();
  const light = p.regEvents.filter((e) => e.type === 'mc-tile-provider-changed' && e.detail && e.detail.type === 'light');
  assert.strictEqual(light.length, 1, 'light re-apply events: ' + light.length);
  assert.strictEqual(light[0].detail.id, 'osm-standard');
});

test('R3b Reset all tells open maps to swap the dark base map back', () => {
  // v0.2.9 review nit (#828 N1): the dark half of the re-apply loop.
  const p = page(ON, { 'mc-dark-tile-provider': 'dataforsyningen-skaermkort-graa-dark' });
  p.cust.api.resetAll();
  const dark = p.regEvents.filter((e) => e.type === 'mc-tile-provider-changed' && e.detail && e.detail.type === 'dark');
  assert.strictEqual(dark.length, 1, 'dark re-apply events: ' + dark.length);
  assert.strictEqual(dark[0].detail.id, 'osm-dark');
});

test('R4 Reset all leaves no tile key behind, after the re-apply', () => {
  const p = page(ON, Object.assign({}, STALE));
  p.cust.api.resetAll();
  assert.deepStrictEqual(Object.keys(p.storage._raw).filter((k) => /tile-provider/.test(k)), []);
});

// ── X. Reset all is offered for a base-map-only customization ───────────────

test('X1 nothing customized: Reset all is not offered', () => {
  assert.strictEqual(page(ON).cust.api.hasResettableCustomization(), false);
});
test('X2 only a light base-map choice: Reset all is offered', () => {
  assert.strictEqual(page(ON, { 'mc-light-tile-provider': 'dataforsyningen-skaermkort-graa' }).cust.api.hasResettableCustomization(), true);
});
test('X3 only a dark base-map choice: Reset all is offered', () => {
  assert.strictEqual(page(ON, { 'mc-dark-tile-provider': 'dataforsyningen-skaermkort-graa-dark' }).cust.api.hasResettableCustomization(), true);
});
test('X4 a theme override alone still offers Reset all', () => {
  assert.strictEqual(page(ON, { 'cs-theme-overrides': JSON.stringify({ theme: { accent: '#3366cc' } }) }).cust.api.hasResettableCustomization(), true);
});
test('X5 after Reset all of a base-map-only choice, Reset all is no longer offered', () => {
  const p = page(ON, { 'mc-light-tile-provider': 'dataforsyningen-skaermkort' });
  p.cust.api.resetAll();
  assert.strictEqual(p.cust.api.hasResettableCustomization(), false);
});

console.log('\n' + passed + ' passed, ' + failed + ' failed');
if (failed) process.exit(1);
