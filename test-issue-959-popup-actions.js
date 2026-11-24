/* test-issue-959-popup-actions.js — #959: the Map page's node popup offers
 * "Terrain profile" and "Predicted reach" exactly where node detail does.
 *
 * The shared builder (roles.js nodeToolActions) and the shared mount
 * (roles.js mountConfigAction) run as shipped in a vm sandbox, against the
 * real public/terrain-profile.js and public/coverage.js. Nothing here is a
 * copy of production logic.
 *
 * Covers:
 *   1. every feature combination (each on, off, absent) for a node with a
 *      fix, a node without a position and the 0,0 no-lock sentinel: the
 *      builder builds an action exactly when the module's own
 *      nodeEntryAction does, with the module's own link;
 *   2. the order is node detail's (Terrain profile, then Predicted reach)
 *      and a missing module drops its action, not the other one;
 *   3. the config is read when build() runs, not when the list is made, so
 *      a late /api/config/client still applies;
 *   4. mountConfigAction waits for MeshConfigReady, appends once per test
 *      id, and appends nothing to a row that left the page;
 *   5. the wiring: node detail and the Map popup both go through the shared
 *      builder (no third copy of the gates), the popup string itself never
 *      carries an action, and the popup mounts on Leaflet's popupopen;
 *   6. one lookup of a node by key in map.js (findNodeByKey), and on a
 *      phone the reach sheet steps aside while a popup is open.
 *
 * Usage: node test-issue-959-popup-actions.js
 */
'use strict';
const fs = require('fs');
const path = require('path');
const vm = require('vm');
const assert = require('assert');
const { loadTerrainProfile, makeElement } = require('./tests/map-overlay-sandbox');

let passed = 0, failed = 0;
const pending = [];
function test(name, fn) {
  try {
    const out = fn();
    if (out && typeof out.then === 'function') {
      pending.push(out.then(() => { passed++; console.log('  ✅ ' + name); },
        (e) => { failed++; console.log('  ❌ ' + name + ': ' + e.message); }));
      return;
    }
    passed++; console.log('  ✅ ' + name);
  } catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

const read = (f) => fs.readFileSync(path.join(__dirname, 'public', f), 'utf8');

// The real modules.
const { TP } = loadTerrainProfile();
const PR = (() => {
  const ctx = {
    window: null, document: { createElement: makeElement }, URLSearchParams, Uint8ClampedArray, Uint32Array,
    Uint8Array, Math, Number, String, Object, Array, isFinite, encodeURIComponent, console,
  };
  ctx.window = ctx;
  vm.createContext(ctx);
  vm.runInContext(read('coverage.js'), ctx);
  return ctx.PredictedReach;
})();

/* roles.js as shipped; its /api/config/client fetch is held open, so the
 * test decides when the config is "ready". */
function loadRoles() {
  let release;
  const ready = new Promise((r) => { release = r; });
  const win = { addEventListener() {}, removeEventListener() {}, dispatchEvent() {}, matchMedia: () => ({ matches: false }) };
  const ctx = {
    window: win, console, Math, Date, JSON, Array, Object, String, Number, RegExp, Error, TypeError,
    parseInt, parseFloat, isNaN, isFinite, Promise, Map, Set, URLSearchParams,
    setTimeout, clearTimeout, setInterval: () => 0, clearInterval() {},
    document: {
      readyState: 'complete', documentElement: { style: {}, getAttribute: () => null },
      createElement: makeElement, head: { appendChild() {} }, body: { appendChild() {} },
      getElementById: () => null, querySelector: () => null, querySelectorAll: () => [], addEventListener() {},
    },
    getComputedStyle: () => ({ getPropertyValue: () => '' }),
    localStorage: { getItem: () => null, setItem() {}, removeItem() {} },
    navigator: { userAgent: '' },
    fetch: () => ready.then(() => ({ json: () => ({}) })),
  };
  vm.createContext(ctx);
  vm.runInContext(read('roles.js'), ctx);
  return { win, release };
}
const { win: W, release } = loadRoles();

const PK = 'ab'.repeat(32);
const FIX = { public_key: PK, name: 'Hill', lat: 56.15674, lon: 10.21076 };
const NODES = {
  'with a fix': FIX,
  'without a position': { public_key: PK, name: 'Nowhere', lat: null, lon: null },
  'at the 0,0 sentinel': { public_key: PK, name: 'NoLock', lat: 0, lon: 0 },
};
const DHM = {
  on: { enabled: true, profile: true },
  'tiles only': { enabled: true, profile: false },
  absent: null,
};
const COV = {
  on: { enabled: true, maxRadiusKm: 40 },
  off: { enabled: false },
  absent: null,
};

function env(dhm, cov, mods) {
  const m = mods || {};
  return {
    TerrainProfile: 'tp' in m ? m.tp : TP,
    PredictedReach: 'pr' in m ? m.pr : PR,
    MC_DHM_CFG: dhm, MC_COVERAGE_CFG: cov,
  };
}
// The sandbox's arrays come from another realm: copied before comparing.
const tools = (node, e) => Array.from(W.nodeToolActions(node, e));
const built = (list) => Array.from(list).map(a => ({ testid: a.testid, el: a.build() })).filter(a => a.el);

console.log('\n=== #959 Map popup actions: the shared builder ===\n');

test('roles.js exports nodeToolActions and mountConfigAction', () => {
  assert.strictEqual(typeof W.nodeToolActions, 'function', 'nodeToolActions');
  assert.strictEqual(typeof W.mountConfigAction, 'function', 'mountConfigAction');
});

// ── 1. Every combination, against the modules' own gates ────────────────

for (const [nk, node] of Object.entries(NODES)) {
  for (const [dk, dhm] of Object.entries(DHM)) {
    for (const [ck, cov] of Object.entries(COV)) {
      test(`node ${nk}, profile ${dk}, reach ${ck}: the actions node detail would show`, () => {
        const want = [];
        const tpEl = TP.nodeEntryAction(node, dhm);
        const prEl = PR.nodeEntryAction(node, cov);
        if (tpEl) want.push(['node-terrain-profile', tpEl.getAttribute('href')]);
        if (prEl) want.push(['node-predicted-reach', prEl.getAttribute('href')]);
        const got = built(tools(node, env(dhm, cov))).map(a => [a.testid, a.el.getAttribute('href')]);
        assert.deepStrictEqual(got, want);
        // The rule, spelled out: only a node with a fix, only with the feature on.
        const fix = nk === 'with a fix';
        assert.strictEqual(got.some(a => a[0] === 'node-terrain-profile'), fix && dk === 'on', 'terrain profile');
        assert.strictEqual(got.some(a => a[0] === 'node-predicted-reach'), fix && ck === 'on', 'predicted reach');
        for (const [id] of got) {
          const el = built(tools(node, env(dhm, cov))).find(a => a.testid === id).el;
          assert.strictEqual(el.getAttribute('data-testid'), id, 'the module\'s own test id');
        }
      });
    }
  }
}

test('both on, a node with a fix: the module links, Terrain profile first', () => {
  const got = built(tools(FIX, env(DHM.on, COV.on)));
  assert.deepStrictEqual(got.map(a => a.testid), ['node-terrain-profile', 'node-predicted-reach']);
  assert.strictEqual(got[0].el.getAttribute('href'), '#/map?profile=' + PK);
  assert.strictEqual(got[1].el.getAttribute('href'), '#/map?reach=' + PK);
  // The label is the last child; the icon before it is static markup.
  const label = (e) => e.children[e.children.length - 1].textContent;
  assert.strictEqual(label(got[0].el), 'Terrain profile');
  assert.strictEqual(label(got[1].el), 'Predicted reach');
});

// ── 2. A missing module ──────────────────────────────────────────────────

test('a module that is not loaded drops its own action only', () => {
  assert.deepStrictEqual(tools(FIX, env(DHM.on, COV.on, { tp: undefined })).map(a => a.testid), ['node-predicted-reach']);
  assert.deepStrictEqual(tools(FIX, env(DHM.on, COV.on, { pr: undefined })).map(a => a.testid), ['node-terrain-profile']);
  assert.deepStrictEqual(tools(FIX, env(DHM.on, COV.on, { tp: {}, pr: {} })), []);
});

// ── 3. The config is read at build time ──────────────────────────────────

test('build() reads the config when it runs: a late config still applies', () => {
  const e = env(null, null);
  const list = tools(FIX, e);
  assert.strictEqual(list.length, 2, 'both modules are listed before the config arrives');
  assert.deepStrictEqual(built(list), [], 'nothing before the config');
  e.MC_DHM_CFG = DHM.on;
  e.MC_COVERAGE_CFG = COV.on;
  assert.deepStrictEqual(built(list).map(a => a.testid), ['node-terrain-profile', 'node-predicted-reach']);
});

test('without an env it reads the page globals', () => {
  W.TerrainProfile = TP; W.PredictedReach = PR;
  W.MC_DHM_CFG = DHM.on; W.MC_COVERAGE_CFG = null;
  try {
    assert.deepStrictEqual(built(tools(FIX)).map(a => a.testid), ['node-terrain-profile']);
  } finally {
    delete W.TerrainProfile; delete W.PredictedReach;
  }
});

// ── 4. The shared mount ──────────────────────────────────────────────────

function fakeRow() {
  const kids = [];
  return {
    isConnected: true, kids,
    appendChild(c) { kids.push(c); return c; },
    querySelector(sel) {
      const m = /^\[data-testid="([^"]+)"\]$/.exec(sel);
      return m ? kids.find(k => k.getAttribute('data-testid') === m[1]) || null : null;
    },
  };
}

test('mountConfigAction waits for the config, appends once per test id, and resolves to the action', async () => {
  const row = fakeRow(), gone = fakeRow();
  gone.isConnected = false;
  let builds = 0;
  const build = () => { builds++; return TP.nodeEntryAction(FIX, DHM.on); };
  const done = [
    W.mountConfigAction(row, 'node-terrain-profile', build),
    W.mountConfigAction(row, 'node-terrain-profile', build),
    W.mountConfigAction(row, 'node-predicted-reach', () => null),
    W.mountConfigAction(gone, 'node-terrain-profile', build),
  ];
  await new Promise(r => setTimeout(r, 10));
  assert.strictEqual(row.kids.length, 0, 'appended before MeshConfigReady');
  release();
  await W.MeshConfigReady;
  await new Promise(r => setTimeout(r, 0));
  assert.deepStrictEqual(row.kids.map(k => k.getAttribute('data-testid')), ['node-terrain-profile'], 'one chip, null skipped');
  assert.strictEqual(gone.kids.length, 0, 'a detached row gets nothing');
  assert.strictEqual(builds, 1, 'built once: the duplicate and the detached row never build');
  // Each mount resolves to what it appended, so the Map popup knows when to refit.
  const got = await Promise.all(done);
  assert.strictEqual(got[0], row.kids[0], 'the first mount resolves to its action');
  assert.deepStrictEqual(got.slice(1), [null, null, null], 'nothing appended resolves to null');
});

// ── 5. Wiring ────────────────────────────────────────────────────────────

const nodesJs = read('nodes.js');
const mapJs = read('map.js');

test('node detail mounts the shared list (no copy of the gates in nodes.js)', () => {
  assert.ok(/nodeToolActions\(n\)\.forEach\(function \(a\) \{ mountConfigAction\(row, a\.testid, a\.build\); \}\)/.test(nodesJs),
    'nodes.js mountNodeActions must go through nodeToolActions');
  assert.ok(!/\.nodeEntryAction\(/.test(nodesJs), 'nodes.js calls a nodeEntryAction itself');
  assert.ok(!/function mountConfigAction/.test(nodesJs), 'nodes.js keeps its own copy of mountConfigAction');
});

test('the Map popup goes through the same builder and mount, on popupopen', () => {
  assert.ok(!/\.nodeEntryAction\(/.test(mapJs), 'map.js calls a nodeEntryAction itself');
  assert.ok(/map\.on\('popupopen', onNodePopupOpen\)/.test(mapJs), 'no popupopen handler on the map');
  const i = mapJs.indexOf('function onNodePopupOpen(');
  assert.ok(i > 0, 'no onNodePopupOpen');
  const body = mapJs.slice(i, mapJs.indexOf('\n  }\n', i));
  assert.ok(/window\.nodeToolActions\(node\)/.test(body) && /window\.mountConfigAction\(/.test(body), 'onNodePopupOpen must use the shared builder and mount');
  assert.ok(/popupRefit\(popup\)/.test(body), 'the grown popup must be laid out and panned again');
});

test('the popup HTML string carries no action (built as DOM after it opens)', () => {
  const i = mapJs.indexOf('function buildPopup(node)');
  const body = mapJs.slice(i, mapJs.indexOf('\n  }\n', i));
  assert.ok(i > 0 && body.length > 100, 'buildPopup not found');
  assert.ok(!/reach|terrain|profile|nodeToolActions/i.test(body), 'buildPopup mentions an action');
  assert.ok(/class="map-popup" data-node-popup/.test(body), 'the node popup is marked for onNodePopupOpen');
});

test('the popup chips use CSS variables only', () => {
  const css = read('style.css');
  const rules = css.replace(/\/\*[\s\S]*?\*\//g, '').match(/[^{}]*\.map-popup-tools[^{]*\{[^}]*\}/g) || [];
  assert.ok(rules.length >= 2, 'no .map-popup-tools rules');
  for (const r of rules) assert.ok(!/#[0-9a-f]{3,8}\b|rgb\(|hsl\(/i.test(r.replace(/var\([^)]*\)/g, '')), 'hard-coded colour: ' + r.trim());
  assert.ok(rules.some(r => /\.leaflet-container \.map-popup-tools \.node-action-chip[^{]*\{[^}]*color:\s*var\(--text-on-accent\)/.test(r)),
    'Leaflet\'s popup link colour must not win over the chip text');
});

test('one node lookup by key: findNodeByKey, not a scan in each caller (#959 F3)', () => {
  const scans = mapJs.match(/\.public_key \|\| ''\)\.toLowerCase\(\) === want/g) || [];
  assert.strictEqual(scans.length, 1, 'linear scans of nodes by public key: ' + scans.length);
  const i = mapJs.indexOf('function findNodeByKey(');
  assert.ok(i > 0 && mapJs.indexOf('=== want', i) < mapJs.indexOf('\n  }\n', i), 'the one scan lives in findNodeByKey');
  for (const fn of ['onNodePopupOpen', 'profileResolveNode', 'profileIsNode']) {
    const j = mapJs.indexOf('function ' + fn + '(');
    assert.ok(j > 0, 'no ' + fn);
    assert.ok(/findNodeByKey\(/.test(mapJs.slice(j, mapJs.indexOf('\n  }\n', j))), fn + ' must use findNodeByKey');
  }
});

/* enclosingMedia answers the @media preludes a CSS rule sits in. */
function enclosingMedia(css, at) {
  const stack = [];
  const re = /([^{}]*)\{|\}/g;
  let m;
  while ((m = re.exec(css)) && m.index < at) {
    if (m[0] === '}') stack.pop();
    else stack.push(/^@media/.test(m[1].trim()) ? m[1].trim() : null);
  }
  return stack.filter(Boolean);
}

test('on a phone the reach sheet steps aside while a map popup is open (#959 F1)', () => {
  assert.ok(/map\.on\('popupopen popupclose', onPopupSheet\)/.test(mapJs), 'no popupopen/popupclose handler for the sheet');
  const i = mapJs.indexOf('function onPopupSheet(');
  const body = mapJs.slice(i, mapJs.indexOf('\n  }\n', i));
  assert.ok(i > 0 && /classList\.toggle\('has-map-popup', e\.type === 'popupopen'\)/.test(body), 'onPopupSheet must mark #map-wrap while a popup is open');
  const css = read('style.css').replace(/\/\*[\s\S]*?\*\//g, '');
  const at = css.search(/\.has-map-popup \.reach-panel\s*\{\s*display:\s*none;\s*\}/);
  assert.ok(at > 0, 'no .has-map-popup .reach-panel { display: none }');
  const media = enclosingMedia(css, at);
  assert.deepStrictEqual(media, ['@media (max-width: 640px)'], 'only the phone\'s bottom sheet steps aside: ' + JSON.stringify(media));
});

Promise.all(pending).then(() => {
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
});
