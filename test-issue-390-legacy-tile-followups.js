/* test-issue-390-legacy-tile-followups.js — #390, follow-ups to #366/#364.
 *
 * #366 restored the legacy LIGHT tile-URL override (`tiles.light` /
 * `map.tiles.lightUrl`) in front of the registry's implicit fallback. Three
 * gaps were left behind:
 *
 *   P1  The DARK legacy override (`tiles.dark` / `map.tiles.darkUrl`) is
 *       still masked by the registry default exactly the way light was
 *       before #366 — no legacy branch existed for dark at all. The #366
 *       precedence must hold per theme:
 *         explicit browser choice > configured provider default
 *                                 > legacy URL > built-in fallback
 *       and an operator's own dark tileset must not be CSS-inverted on top
 *       of already being dark.
 *   P2  A legacy raw URL carries no vendor metadata, so it keeps the
 *       generic `© OpenStreetMap contributors` credit. That is by design
 *       but was undocumented; config.example.json must say so, and the
 *       documented credit must be the one the resolver really returns.
 *   P3  A legacy URL's spec borrows the built-in OSM default's zoom
 *       ceiling, so a custom provider that serves one level deeper is
 *       clipped. Also by design for now, and likewise documented — with
 *       the documented ceiling pinned to the real one.
 *
 * Runs via: node test-issue-390-legacy-tile-followups.js
 * Pure vm sandbox — no jsdom, no Playwright.
 */
'use strict';
const vm     = require('vm');
const fs     = require('fs');
const path   = require('path');
const assert = require('assert');

let passed = 0, failed = 0;
async function asyncTest(name, fn) {
  try { await fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

const GENERIC_CREDIT = '© OpenStreetMap contributors';

function makeStorage() {
  const store = {};
  return {
    getItem(k)    { return Object.prototype.hasOwnProperty.call(store, k) ? store[k] : null; },
    setItem(k, v) { store[k] = String(v); },
    removeItem(k) { delete store[k]; },
  };
}

// Browser-like sandbox: one object is both `window` and the global, so the
// bare-name references inside roles.js / map-tile-providers.js resolve the
// way they do in a page.
function makeSandbox(theme) {
  const tilePane = {
    style: { filter: '' },
    setAttribute: () => {},
    getAttribute: () => null,
    removeAttribute: () => {},
  };
  const ctx = {
    console,
    setTimeout, clearTimeout,
    JSON, Date, Math, Object, Array, String, Number, Boolean, Set, Map, Promise,
    localStorage: makeStorage(),
    addEventListener: () => {},
    dispatchEvent: () => true,
    matchMedia: () => ({ matches: theme === 'dark', addEventListener: () => {} }),
    document: {
      documentElement: {
        getAttribute: () => theme,
        setAttribute: () => {},
        style: { getPropertyValue: () => '', setProperty: () => {} },
      },
      querySelector: (sel) => (sel === '.leaflet-tile-pane' ? tilePane : null),
      querySelectorAll: () => [],
      getElementById: () => null,
      createElement: () => ({ style: {}, appendChild: () => {}, setAttribute: () => {}, addEventListener: () => {} }),
      addEventListener: () => {},
      body: { appendChild: () => {}, style: {} },
      head: { appendChild: () => {} },
      readyState: 'complete',
    },
    CustomEvent: function (type, init) { this.type = type; this.detail = (init && init.detail) || null; },
  };
  ctx.window = ctx;
  ctx.globalThis = ctx;
  ctx.tilePane = tilePane;
  vm.createContext(ctx);
  return ctx;
}

function loadInto(ctx, relPath) {
  vm.runInContext(fs.readFileSync(path.join(__dirname, relPath), 'utf8'), ctx, { filename: relPath });
}

// Drive the real async client-config loader so the legacy keys travel the
// production path (roles.js applyConfig → MC_initTileRegistry(true)).
async function configuredStack(config, theme, storedDark, storedLight) {
  const ctx = makeSandbox(theme || 'dark');
  if (storedDark)  ctx.localStorage.setItem('mc-dark-tile-provider', storedDark);
  if (storedLight) ctx.localStorage.setItem('mc-light-tile-provider', storedLight);
  ctx.fetch = () => Promise.resolve({ json: () => Promise.resolve(config) });
  loadInto(ctx, path.join('public', 'roles.js'));
  loadInto(ctx, path.join('public', 'map-tile-providers.js'));
  await ctx.MeshConfigReady;
  return ctx;
}

// Brace-matched extraction of `function <name>(...) { … }`, so the test runs
// the production helper body rather than a copy of it.
function extractFn(relPath, name) {
  const src = fs.readFileSync(path.join(__dirname, relPath), 'utf8');
  const m = src.match(new RegExp('function\\s+' + name + '\\s*\\([^)]*\\)\\s*\\{'));
  if (!m) throw new Error(name + ' definition not found in ' + relPath);
  let depth = 0, i = m.index;
  for (; i < src.length; i++) {
    if (src[i] === '{') depth++;
    else if (src[i] === '}') { depth--; if (depth === 0) { i++; break; } }
  }
  return src.slice(m.index, i);
}

function makeTileLayerSpy() {
  const calls = [];
  return {
    calls,
    L: { tileLayer(url, opts) { return { addTo(map) { calls.push({ url, opts, map }); return this; } }; } },
  };
}

const DARK_URL  = 'https://private.example/dark/{z}/{x}/{y}.png';
const LIGHT_URL = 'https://private.example/light/{z}/{x}/{y}.png';

// ═══ P1 — the dark legacy override is honoured, with #366 precedence ════
async function darkLegacyTests() {
  console.log('\n── #390 P1: dark legacy tile URL override ──');

  for (const [label, config] of [
    ['tiles.dark', { tiles: { dark: DARK_URL } }],
    ['map.tiles.darkUrl', { map: { tiles: { darkUrl: DARK_URL } } }],
  ]) {
    await asyncTest('P1 ' + label + ' overrides the implicit dark fallback', async () => {
      const ctx = await configuredStack(config, 'dark');
      assert.strictEqual(ctx.TILE_DARK, DARK_URL, 'the config loader actually ran');
      assert.strictEqual(ctx.getTileUrl(), DARK_URL);
      const spec = ctx.MC_getTileSpec('dark');
      assert.strictEqual(spec.url, DARK_URL, 'every resolver sees the same override');
      assert.strictEqual(spec.id, null, 'a raw URL is not a registry provider');
      assert.strictEqual(spec.attribution, GENERIC_CREDIT);
      assert.strictEqual(spec.refUrl, null);
      assert.strictEqual(spec.invertFilter, null, 'an operator dark tileset must not be inverted');
      assert.strictEqual(ctx.getActiveTileProvider(), null, 'an unknown vendor has no registry metadata');
    });

    await asyncTest('P1 ' + label + ' leaves the tile pane filter cleared', async () => {
      const ctx = await configuredStack(config, 'dark');
      ctx.tilePane.style.filter = 'stale filter';
      ctx.MC_applyTileFilter();
      assert.strictEqual(ctx.tilePane.style.filter, '',
        'inverting an already-dark custom tileset renders it light');
    });

    await asyncTest('P1 ' + label + ' drives the node-detail inset without borrowing metadata', async () => {
      const ctx = await configuredStack(config, 'dark');
      // Synthetic unused metadata makes borrowing the fallback provider's
      // credit, reference overlay or inversion observable in the real helper.
      Object.assign(ctx.MC_TILE_PROVIDERS['osm-dark'], {
        attribution: 'UNUSED VENDOR', refUrl: 'https://unused.example/labels/{z}/{x}/{y}', invertFilter: 'invert(1)',
      });
      const spy = makeTileLayerSpy();
      ctx.L = spy.L;
      const pane = { style: { filter: 'stale filter' } };
      ctx.__map = { getPane: () => pane };
      vm.runInContext(extractFn('public/nodes.js', '_applyTilesToNodeMap') + '\n_applyTilesToNodeMap(__map);', ctx);
      assert.strictEqual(spy.calls.length, 1, 'an unknown vendor must not borrow a reference overlay');
      assert.strictEqual(spy.calls[0].url, DARK_URL);
      assert.strictEqual(spy.calls[0].opts.attribution, GENERIC_CREDIT,
        'preserve the historical generic fallback credit');
      assert.strictEqual(pane.style.filter, '');
    });

    for (const stored of ['carto-dark', 'osm-dark']) {
      await asyncTest('P1 browser choice ' + stored + ' wins over ' + label, async () => {
        const ctx = await configuredStack(config, 'dark', stored);
        assert.strictEqual(ctx.MC_getTileSpec('dark').id, stored,
          'an explicit choice, including the fallback id itself, beats the legacy URL');
        assert.notStrictEqual(ctx.getTileUrl(), DARK_URL);
        assert.ok(ctx.getActiveTileProvider());
      });
    }

    await asyncTest('P1 invalid browser choice falls back to ' + label, async () => {
      const ctx = await configuredStack(config, 'dark', 'missing-provider');
      assert.strictEqual(ctx.getTileUrl(), DARK_URL);
    });

    await asyncTest('P1 ' + label + ' never replaces light-provider tiles', async () => {
      const ctx = await configuredStack(config, 'light');
      const spec = ctx.MC_getTileSpec('light');
      assert.strictEqual(spec.id, 'osm-standard');
      assert.notStrictEqual(ctx.getTileUrl(), DARK_URL);
      assert.ok(ctx.getActiveTileProvider(), 'the light provider keeps its registry metadata');
    });
  }

  await asyncTest('P1 configured darkDefault wins over the legacy URL; browser choice wins over both', async () => {
    const ctx = await configuredStack({
      map: { tiles: { darkUrl: DARK_URL, darkDefault: 'esri-darkgray-labels' } },
    }, 'dark');
    assert.strictEqual(ctx.MC_getTileSpec('dark').id, 'esri-darkgray-labels');
    assert.ok(ctx.getTileUrl().includes('arcgisonline.com'));
    ctx.MC_setDarkTileProvider('osm-dark');
    assert.strictEqual(ctx.MC_getTileSpec('dark').id, 'osm-dark');
    assert.ok(ctx.getTileUrl().includes('tile.openstreetmap.org'));
  });

  await asyncTest('P1 unavailable darkDefault and disabled cached provider recover to the legacy URL', async () => {
    const ctx = await configuredStack({
      map: { tiles: { darkUrl: DARK_URL, darkDefault: 'missing-provider', providers: { stamen: { enabled: false } } } },
    }, 'dark', 'stamen-toner-dark');
    assert.strictEqual(ctx.getTileUrl(), DARK_URL);
  });

  await asyncTest('P1 explicit programmatic server default wins over the legacy dark URL', async () => {
    const ctx = await configuredStack({ tiles: { dark: DARK_URL } }, 'dark');
    ctx.MC_setServerDefaultTileProvider('carto-dark');
    assert.strictEqual(ctx.MC_getTileSpec('dark').id, 'carto-dark');
    assert.ok(ctx.getTileUrl().includes('cartocdn.com'));
  });

  await asyncTest('P1 a legacy dark URL keeps the generic credit even with an unused OSM vendor configured', async () => {
    const ctx = await configuredStack({
      map: { tiles: { darkUrl: DARK_URL, providers: { osm: { enabled: true, provider: 'maptiler', token: 'synthetic-token' } } } },
    }, 'dark');
    assert.strictEqual(ctx.getTileUrl(), DARK_URL);
    assert.strictEqual(ctx.MC_getTileSpec('dark').attribution, GENERIC_CREDIT);
    assert.strictEqual(ctx.getActiveTileProvider(), null);
  });

  await asyncTest('P1 both legacy URLs at once resolve per theme', async () => {
    const ctx = await configuredStack({ tiles: { dark: DARK_URL, light: LIGHT_URL } }, 'dark');
    assert.strictEqual(ctx.MC_getTileSpec('dark').url, DARK_URL);
    assert.strictEqual(ctx.MC_getTileSpec('light').url, LIGHT_URL);
    assert.strictEqual(ctx.getTileUrl(), DARK_URL, 'dark theme takes the dark override');
  });

  await asyncTest('P1 MC_getDarkTileProvider() keeps returning a resolvable registry id', async () => {
    const ctx = await configuredStack({ tiles: { dark: DARK_URL } }, 'dark');
    const id = ctx.MC_getDarkTileProvider();
    assert.strictEqual(id, ctx.MC_DARK_TILE_DEFAULT,
      'the public accessor must not start returning null for legacy installs');
    assert.ok(ctx.MC_TILE_PROVIDERS[id], 'the layer control still finds the id in the registry');
  });

  await asyncTest('P1 no legacy config leaves the dark fallback and its invert filter intact', async () => {
    const ctx = await configuredStack({ map: { tiles: {} } }, 'dark');
    const spec = ctx.MC_getTileSpec('dark');
    assert.strictEqual(spec.id, 'osm-dark');
    assert.ok(spec.invertFilter, 'the built-in dark default still inverts');
    ctx.MC_applyTileFilter();
    assert.strictEqual(ctx.tilePane.style.filter, spec.invertFilter);
  });
}

// ═══ P2/P3 — the legacy URL's credit and zoom ceiling are documented ════
async function legacyDocTests() {
  console.log('\n── #390 P2/P3: legacy raw URLs are documented ──');

  const cfg  = JSON.parse(fs.readFileSync(path.join(__dirname, 'config.example.json'), 'utf8'));
  const note = (cfg.map && cfg.map.tiles && cfg.map.tiles._comment_legacy) || '';

  await asyncTest('P2 config.example.json documents all four legacy tile-URL keys', async () => {
    assert.ok(note, 'map.tiles._comment_legacy must document the legacy URL keys');
    for (const key of ['tiles.dark', 'tiles.light', 'darkUrl', 'lightUrl']) {
      assert.ok(note.indexOf(key) !== -1, 'the legacy note must name ' + key);
    }
    assert.ok(/darkDefault|lightDefault/.test(note),
      'the note must point operators at the provider-id form, which keeps priority');
  });

  await asyncTest('P2 the documented credit is the credit the resolver really returns', async () => {
    assert.ok(note.indexOf(GENERIC_CREDIT) !== -1,
      'the note must quote the generic credit a raw URL gets');
    const ctx = await configuredStack({ tiles: { dark: DARK_URL, light: LIGHT_URL } }, 'dark');
    assert.strictEqual(ctx.MC_getTileSpec('dark').attribution, GENERIC_CREDIT);
    assert.strictEqual(ctx.MC_getTileSpec('light').attribution, GENERIC_CREDIT);
  });

  await asyncTest('P3 the documented zoom ceiling is the ceiling the resolver really returns', async () => {
    const m = note.match(/capped at zoom (\d+)/);
    assert.ok(m, 'the legacy note must state the zoom ceiling a raw URL inherits');
    const ctx = await configuredStack({ tiles: { dark: DARK_URL, light: LIGHT_URL } }, 'dark');
    for (const theme of ['dark', 'light']) {
      assert.strictEqual(ctx.MC_getTileSpec(theme).maxZoom, Number(m[1]),
        'the note says zoom ' + m[1] + ' but the legacy ' + theme + ' spec declares ' +
        ctx.MC_getTileSpec(theme).maxZoom);
    }
  });
}

darkLegacyTests()
  .then(legacyDocTests)
  .then(() => {
    console.log('\n' + (failed === 0 ? '✅' : '❌') + ' #390: ' + passed + ' passed, ' + failed + ' failed');
    process.exit(failed === 0 ? 0 : 1);
  })
  .catch((e) => { console.error(e); process.exit(1); });
