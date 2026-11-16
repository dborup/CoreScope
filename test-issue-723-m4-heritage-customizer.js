/* test-issue-723-m4-heritage-customizer.js — #723 M4: the protected
 * ancient monuments settings in the customizer (public/customize-v2.js,
 * section `heritage`), the one rule they follow
 * (public/map-tile-providers.js, MC_HERITAGE_SETTINGS) and how the overlay
 * and the Path profile warning use them.
 *
 * The real files run in vm sandboxes; nothing is copied:
 *   - the overlay in the recording Leaflet stub (tests/map-overlay-sandbox.js);
 *   - the profile in tests/heritage-profile-sandbox.js, with the rule
 *     loaded first as index.html does;
 *   - the customizer in tests/customizer-sandbox.js.
 *
 * Covered:
 *   1. the rule: the keys, the defaults M1-M3 hard-coded (overlay off,
 *      opacity 0.85, the warning on, every distance the server reports),
 *      the bounds (warnDistanceM up to internal/heritage MaxMarginM), and
 *      the viewer's section over the defaults;
 *   2. the overlay: its default and opacity follow the settings, live on
 *      heritage-defaults-changed while on; a remembered choice and the hash
 *      still win; "off" goes into the hash only when the default is on;
 *      the other overlays are untouched;
 *   3. the profile: the warning distance narrows the note, never the
 *      verdict; the warning off asks for nothing; an open profile follows
 *      the event without asking again;
 *   4. the customizer: the section, stored values, import validation,
 *      export round trip, the event, the rendered inputs, one rule;
 *   5. the docs page names the credit the server sends.
 *
 * Runs via: node test-issue-723-m4-heritage-customizer.js
 */
'use strict';
const fs = require('fs');
const path = require('path');
const assert = require('assert');
const { makeSandbox, buildControl } = require('./tests/map-overlay-sandbox');
const { loadCustomizer: loadCustomizerSandbox } = require('./tests/customizer-sandbox');
const HP = require('./tests/heritage-profile-sandbox');

let passed = 0, failed = 0;
const tests = [];
function test(name, fn) { tests.push([name, fn]); }
const sleep = (ms) => new Promise(r => setTimeout(r, ms));
const plain = (o) => JSON.parse(JSON.stringify(o));

const HERITAGE = {
  enabled: true, url: 'https://www.kulturarv.dk/ffgeoserver/public/wms',
  layers: 'fundogfortidsminder_areal_beskyttelse,fundogfortidsminder_punkt_fredet',
  minZoom: 12, maxZoom: 19, bounds: [[54.4646, 8.0899], [57.7289, 15.5689]],
  attribution: '© Slots- og Kulturstyrelsen',
};
const HERITAGE_ID = 'heritage-monuments';
const DEFAULTS = { enabled: false, opacityPct: 85, profileWarning: true, warnDistanceM: null };

function rulesOf(ctx) {
  const R = ctx.window.MC_HERITAGE_SETTINGS;
  assert(R && typeof R.resolve === 'function' && typeof R.valid === 'function', 'MC_HERITAGE_SETTINGS missing');
  return R;
}

// ── 1. The rule ─────────────────────────────────────────────────────────────

test('the keys, the defaults M1-M3 hard-coded, and the bounds', () => {
  const R = rulesOf(makeSandbox({ heritage: HERITAGE }));
  assert.deepStrictEqual(Array.from(R.KEYS), ['enabled', 'opacityPct', 'profileWarning', 'warnDistanceM']);
  assert.deepStrictEqual(plain(R.DEFAULTS), DEFAULTS);
  assert.deepStrictEqual(Array.from(R.LIMITS.opacityPct), [10, 100]);
  // The server never reports a zone further out than its radius, which is
  // bounded by internal/heritage MaxMarginM; a wider viewer value is moot.
  const go = fs.readFileSync(path.join(__dirname, 'internal', 'heritage', 'cell.go'), 'utf8');
  const m = go.match(/\bMaxMarginM\s*=\s*(\d+)\b/);
  assert(m, 'MaxMarginM not found');
  assert.deepStrictEqual(Array.from(R.LIMITS.warnDistanceM), [0, Number(m[1])]);
});

test('valid: whole numbers in bounds, booleans for the switches, nothing else', () => {
  const R = rulesOf(makeSandbox({ heritage: HERITAGE }));
  const ok = { enabled: [true, false], profileWarning: [true, false], opacityPct: [10, 55, 85, 100], warnDistanceM: [0, 30, 250] };
  const bad = {
    enabled: [1, 0, 'true', null, undefined, {}],
    profileWarning: [1, 'false', null],
    opacityPct: [9, 101, 0, 0.85, 50.5, '85', NaN, Infinity, null, true],
    warnDistanceM: [-1, 251, 12.5, '30', NaN, null, false, [30]],
  };
  for (const k of Object.keys(ok)) ok[k].forEach((v) => assert.strictEqual(R.valid(k, v), true, k + ' rejected ' + JSON.stringify(v)));
  for (const k of Object.keys(bad)) bad[k].forEach((v) => assert.strictEqual(R.valid(k, v), false, k + ' accepted ' + String(v)));
  ['hasOwnProperty', 'constructor', '__proto__', 'radiusM', 'opacity', 'url'].forEach((k) => assert.strictEqual(R.valid(k, 1), false, k));
});

test('resolve: the viewer section over the defaults; junk is not a setting', () => {
  const ctx = makeSandbox({ heritage: HERITAGE });
  const R = rulesOf(ctx);
  assert.deepStrictEqual(plain(R.resolve({})), DEFAULTS);
  assert.deepStrictEqual(plain(R.resolve(null)), DEFAULTS);
  assert.deepStrictEqual(plain(R.resolve({ enabled: true, opacityPct: 40, profileWarning: false, warnDistanceM: 30 })),
    { enabled: true, opacityPct: 40, profileWarning: false, warnDistanceM: 30 });
  assert.deepStrictEqual(plain(R.resolve({ enabled: 'yes', opacityPct: 0.4, profileWarning: 0, warnDistanceM: '30' })), DEFAULTS);
  // Without an argument: the live window.SITE_CONFIG.heritage.
  assert.deepStrictEqual(plain(R.resolve()), DEFAULTS);
  ctx.window.SITE_CONFIG = { heritage: { opacityPct: 60 } };
  assert.strictEqual(R.resolve().opacityPct, 60);
});

// ── 2. The overlay ──────────────────────────────────────────────────────────

function overlaySandbox(o) {
  o = o || {};
  const ctx = makeSandbox(Object.assign({ heritage: HERITAGE }, o.sandbox || {}));
  if (o.section !== undefined) ctx.window.SITE_CONFIG = { heritage: o.section };
  return ctx;
}
function heritageLayer(map) { return map._mcHeritageLayer; }
function onMap(o) {
  const ctx = overlaySandbox(o);
  const { map } = buildControl(ctx, true);
  const l = heritageLayer(map);
  assert(l, 'no heritage layer');
  return { ctx, map, layer: l, on: map.hasLayer(l) };
}

test('overlay default: off as in M1 without a setting; on when the viewer chose it', () => {
  assert.strictEqual(onMap().on, false, 'M1 default changed');
  assert.strictEqual(onMap({ section: { enabled: false } }).on, false);
  assert.strictEqual(onMap({ section: { enabled: true } }).on, true, 'customizer on ignored');
});

test('overlay default: a remembered choice and the hash still win', () => {
  assert.strictEqual(onMap({ section: { enabled: true }, sandbox: { storage: { 'mc-heritage': '0' } } }).on, false, 'stored off lost');
  assert.strictEqual(onMap({ section: { enabled: false }, sandbox: { storage: { 'mc-heritage': '1' } } }).on, true, 'stored on lost');
  assert.strictEqual(onMap({ section: { enabled: true }, sandbox: { hash: '#/map?heritage=0' } }).on, false, 'hash off lost');
  assert.strictEqual(onMap({ section: { enabled: false }, sandbox: { hash: '#/map?heritage=1' } }).on, true, 'hash on lost');
});

test('overlay off: dropped from the hash while the default is off (as M1), written as =0 while it is on', () => {
  const a = onMap({ sandbox: { hash: '#/map?heritage=1' } });
  a.map.removeLayer(a.layer);
  assert.strictEqual(a.ctx.location.hash, '#/map', 'default off: ' + a.ctx.location.hash);
  const b = onMap({ section: { enabled: true } });
  assert(b.on);
  b.map.removeLayer(b.layer);
  assert(/heritage=0/.test(b.ctx.location.hash), 'default on: ' + b.ctx.location.hash);
  // The antenna sites keep writing =0 either way (#724 M5).
  const c = makeSandbox({ masts: { enabled: true, url: '/api/masts', minZoom: 13, cellSizeM: 2000, cellExtent: [219, 3017, 462, 3209], sourceUrl: 'https://mastedatabasen.dk/', attribution: 'x' } });
  c.window.SITE_CONFIG = { masts: { enabled: false } };
  const cm = buildControl(c, true).map;
  cm.addLayer(cm._mcMastsSitesLayer);
  cm.removeLayer(cm._mcMastsSitesLayer);
  assert(/masts=0/.test(c.location.hash), 'masts off no longer =0: ' + c.location.hash);
});

test('overlay opacity: 0.85 as M1 by default, the viewer percentage otherwise', () => {
  assert.strictEqual(onMap().layer.options.opacity, 0.85);
  assert.strictEqual(onMap({ section: { opacityPct: 40 } }).layer.options.opacity, 0.4);
  // The other overlays keep their own.
  const ctx = overlaySandbox({ section: { opacityPct: 40 } });
  ctx.window.MC_DHM_CFG = { enabled: true, url: '/api/map/dhm/wms', layer: 'dhm_terraen_skyggekort', minZoom: 7, maxZoom: 19, bounds: [[54, 7], [58, 16]], attribution: 'x' };
  const { map } = buildControl(ctx, true);
  assert.strictEqual(map._mcTerrainLayer.options.opacity, 0.65);
});

test('overlay opacity: follows heritage-defaults-changed while on, and not once removed', () => {
  const h = onMap({ section: { enabled: true } });
  assert(h.on);
  h.ctx.window.SITE_CONFIG = { heritage: { enabled: true, opacityPct: 30 } };
  h.ctx.window.dispatchEvent(new h.ctx.CustomEvent('heritage-defaults-changed'));
  assert.strictEqual(h.layer.options.opacity, 0.3, 'not live');
  h.map.removeLayer(h.layer);
  h.ctx.window.SITE_CONFIG = { heritage: { opacityPct: 70 } };
  h.ctx.window.dispatchEvent(new h.ctx.CustomEvent('heritage-defaults-changed'));
  assert.strictEqual(h.layer.options.opacity, 0.3, 'a removed layer still listens');
  // Re-added: it reads the current setting.
  h.map.addLayer(h.layer);
  assert.strictEqual(h.layer.options.opacity, 0.7, 'stale opacity on re-add');
});

// ── 3. The profile ──────────────────────────────────────────────────────────

const FAR = JSON.parse(JSON.stringify(HP.NEAR)); // 42 m from a zone
const loadProfile = (section) => HP.load({ rules: true, setup: (win) => { if (section) win.SITE_CONFIG = { heritage: section }; } });

test('note: the warning distance narrows the ends listed; inside a zone always shows', () => {
  const { TP } = loadProfile();
  const both = { a: HP.INSIDE, b: FAR };
  assert(/B: Vandfyldt grav, about 42 m/.test(TP.heritageNote(both, HP.CFG, { profileWarning: true, warnDistanceM: null }).text));
  assert(/B: Vandfyldt grav/.test(TP.heritageNote(both, HP.CFG, { profileWarning: true, warnDistanceM: 42 }).text), '42 m at 42 dropped');
  const near = TP.heritageNote(both, HP.CFG, { profileWarning: true, warnDistanceM: 41 });
  assert(near && /A: Rundhøj, inside/.test(near.text) && !/B:/.test(near.text), near && near.text);
  assert.strictEqual(TP.heritageNote({ a: null, b: FAR }, HP.CFG, { profileWarning: true, warnDistanceM: 0 }), null);
  assert(TP.heritageNote({ a: HP.INSIDE, b: null }, HP.CFG, { profileWarning: true, warnDistanceM: 0 }), 'inside dropped at 0 m');
  // A zone near with no usable distance is still listed: the server put it
  // within its own radius.
  const nodist = JSON.parse(JSON.stringify(FAR)); delete nodist.nearest.distanceM;
  assert(TP.heritageNote({ a: nodist, b: null }, HP.CFG, { profileWarning: true, warnDistanceM: 0 }), 'no-distance answer dropped');
});

test('note: the warning off gives no note; no settings is M3 as it was', () => {
  const { TP } = loadProfile();
  assert.strictEqual(TP.heritageNote({ a: HP.INSIDE, b: FAR }, HP.CFG, { profileWarning: false, warnDistanceM: null }), null);
  assert(/B: Vandfyldt grav/.test(TP.heritageNote({ a: HP.INSIDE, b: FAR }, HP.CFG).text));
});

test('panel: with the warning off no monument lookup is made; the verdict is unchanged', async () => {
  const env = loadProfile({ profileWarning: false });
  const { tool, fetchJSON } = HP.openTool(env, { heritage: HP.byEnd(HP.INSIDE, FAR) });
  HP.setPoints(tool, HP.A1, HP.B1);
  await sleep(500);
  assert.strictEqual(fetchJSON.profiles().length, 1, 'profile not measured');
  assert.deepStrictEqual(fetchJSON.heritage(), [], 'asked with the warning off');
  const u = HP.ui(env);
  assert(!u.shown && !u.noteShown);
  assert(u.verdictSnap && u.verdictSnap.state === 'fresnel60', JSON.stringify(u.verdictSnap));
});

test('panel: the warning distance applies to the panel', async () => {
  const env = loadProfile({ warnDistanceM: 20 });
  const { tool } = HP.openTool(env, { heritage: HP.byEnd(HP.NONE, FAR) });
  HP.setPoints(tool, HP.A1, HP.B1);
  await sleep(500);
  assert(!HP.ui(env).shown, '42 m shown at 20 m');
});

test('panel: an open profile follows heritage-defaults-changed without asking again', async () => {
  const env = loadProfile();
  const { tool, fetchJSON } = HP.openTool(env, { heritage: HP.byEnd(HP.INSIDE, FAR) });
  HP.setPoints(tool, HP.A1, HP.B1);
  await sleep(500);
  assert(HP.ui(env).shown, 'no note at the defaults');
  const asked = fetchJSON.heritage().length;
  const profiles = fetchJSON.profiles().length;
  const fire = async (section) => {
    env.win.SITE_CONFIG = { heritage: section };
    env.win.dispatchEvent(new env.win.CustomEvent('heritage-defaults-changed'));
    await sleep(20);
  };
  await fire({ warnDistanceM: 30 });
  let u = HP.ui(env);
  assert(u.shown && !/B:/.test(u.chip.title), 'B still listed at 30 m: ' + u.chip.title);
  await fire({ profileWarning: false });
  u = HP.ui(env);
  assert(!u.shown && !u.noteShown, 'note kept with the warning off');
  await fire({});
  u = HP.ui(env);
  assert(u.shown && /B: Vandfyldt grav/.test(u.chip.title), 'note not back');
  assert.strictEqual(fetchJSON.heritage().length, asked, 'asked again: ' + fetchJSON.heritage());
  assert.strictEqual(fetchJSON.profiles().length, profiles, 'the event re-measured the profile');
  // Closed: the event shows nothing.
  tool.close();
  await fire({ warnDistanceM: 250 });
  assert(!HP.ui(env).shown, 'a closed panel showed the note');
  tool.destroy();
  assert.strictEqual((env.win.listeners['heritage-defaults-changed'] || []).length, 0, 'listener left after destroy');
});

// ── 4. The customizer ───────────────────────────────────────────────────────

function loadCustomizer(opts) {
  opts = opts || {};
  const cfg = 'cfg' in opts ? opts.cfg : HERITAGE;
  return loadCustomizerSandbox({
    server: opts.server,
    dhm: opts.profile ? { enabled: true, url: '/api/map/dhm/wms', profile: true } : undefined,
    scripts: opts.noRule ? [] : ['map-tile-providers.js'].concat(opts.profile ? ['terrain-profile.js'] : []),
    setup: (ctx) => {
      ctx.location = { hash: '#/map', pathname: '/', search: '' };
      ctx.history = { state: null, replaceState() {} };
      ctx.URLSearchParams = URLSearchParams;
      ctx.Promise = Promise;
      if (cfg) ctx.window.MC_HERITAGE_CFG = cfg;
    },
  });
}
const stored = (c) => JSON.parse(c.localStorage.getItem('cs-theme-overrides') || '{}');

test('heritage is a known object section', () => {
  const { api } = loadCustomizer();
  assert(api.VALID_SECTIONS.indexOf('heritage') >= 0);
  assert.deepStrictEqual(Array.from(api.HERITAGE_SETTING_KEYS), ['enabled', 'opacityPct', 'profileWarning', 'warnDistanceM']);
  assert.strictEqual(api.validateShape({ heritage: { opacityPct: 50 } }).valid, true);
  const bad = api.validateShape({ heritage: 'on' });
  assert.strictEqual(bad.valid, false);
  assert(/must be an object/.test(bad.errors.join(' ')), bad.errors.join(' '));
});

test('isValidHeritageSetting is exported and judges one key/value', () => {
  // v0.2.9 review nit (#828 round 3, I2): the export had no test.
  const { api } = loadCustomizer();
  assert.strictEqual(typeof api.isValidHeritageSetting, 'function');
  assert.strictEqual(api.isValidHeritageSetting('opacityPct', 50), true);
  assert.strictEqual(api.isValidHeritageSetting('opacityPct', 'x'), false);
  assert.strictEqual(api.isValidHeritageSetting('radiusM', 50), false);
});

test('only valid settings are stored; each rejected one warns', () => {
  const c = loadCustomizer();
  c.api.writeOverrides({ heritage: { enabled: 'yes', opacityPct: 5, profileWarning: false, warnDistanceM: 400, radiusM: 50 } });
  assert.deepStrictEqual(stored(c).heritage, { profileWarning: false });
  assert.strictEqual(c.warnings.filter((w) => /Invalid ancient monuments setting/.test(w)).length, 3, c.warnings.join('\n'));
  const d = loadCustomizer();
  d.api.writeOverrides({ heritage: { enabled: true, opacityPct: 100, profileWarning: true, warnDistanceM: 0 } });
  assert.deepStrictEqual(stored(d).heritage, { enabled: true, opacityPct: 100, profileWarning: true, warnDistanceM: 0 });
});

test('without the rule on the page no setting is accepted', () => {
  const c = loadCustomizer({ noRule: true });
  c.api.writeOverrides({ heritage: { opacityPct: 50 } });
  assert.strictEqual(stored(c).heritage, undefined);
  assert.strictEqual(c.api.heritageSettings({}), null);
  assert.strictEqual(c.api.renderHeritageSettings(), '');
  assert.strictEqual(c.api.validateShape({ heritage: { opacityPct: 50 } }).valid, false);
});

test('validateShape names every bad value and its bounds; unknown keys only warn', () => {
  const { api, warnings } = loadCustomizer();
  const res = api.validateShape({ heritage: { opacityPct: 5, warnDistanceM: 300, enabled: 1, profileWarning: true, radiusM: 9 } });
  assert.strictEqual(res.valid, false);
  const msg = res.errors.join(' | ');
  assert(/heritage\.opacityPct = "5" .*10-100/.test(msg), msg);
  assert(/heritage\.warnDistanceM = "300" .*0-250/.test(msg), msg);
  assert(/heritage\.enabled = "1" .*true or false/.test(msg), msg);
  assert.strictEqual(res.errors.length, 3, msg);
  assert(warnings.some((w) => /Unknown heritage key in import: radiusM/.test(w)), warnings.join('\n'));
});

test('an import stating warnDistanceM: null (the default) is accepted, as on write (#825 F3)', () => {
  const c = loadCustomizer();
  const res = c.api.validateShape({ heritage: { warnDistanceM: null, opacityPct: 50 } });
  assert.strictEqual(res.valid, true, res.errors.join(' | '));
  c.api.writeOverrides({ heritage: { warnDistanceM: null, opacityPct: 50 } });
  assert.deepStrictEqual(plain(stored(c).heritage), { opacityPct: 50 });
  // null stays refused for a key whose default is not null.
  assert.strictEqual(c.api.validateShape({ heritage: { opacityPct: null } }).valid, false);
});

test('the heritage section survives an export round trip', () => {
  const first = loadCustomizer();
  const delta = { heritage: { enabled: true, opacityPct: 55, profileWarning: false, warnDistanceM: 40 } };
  first.api.writeOverrides(delta);
  const exported = first.api.readOverrides();
  assert.deepStrictEqual(plain(exported), delta);
  const second = loadCustomizer();
  const shape = second.api.validateShape(exported);
  assert.strictEqual(shape.valid, true, shape.errors.join('; '));
  second.api.writeOverrides(exported);
  assert.deepStrictEqual(plain(second.api.readOverrides()), delta);
  assert.deepStrictEqual(plain(second.api.heritageSettings(second.api.computeEffective({}, exported))), delta.heritage);
});

test('a changed setting announces heritage-defaults-changed once the pipeline has run; an unchanged one is silent', () => {
  const c = loadCustomizer({ server: {} });
  c.events.length = 0;
  c.api.writeOverrides({ heritage: { opacityPct: 40 } });
  c.api.init({});
  assert(c.events.indexOf('heritage-defaults-changed') >= 0, 'no signal: ' + c.events.join(', '));
  c.events.length = 0;
  c.api.applyCSS(c.api.computeEffective({}, c.api.readOverrides()), c.api.readOverrides());
  assert.strictEqual(c.events.indexOf('heritage-defaults-changed'), -1, 'a no-op run announced a change');
});

test('Reset All: the section is gone, the defaults apply and open maps hear of it', () => {
  const c = loadCustomizer({ server: {} });
  c.api.writeOverrides({ heritage: { enabled: true, opacityPct: 30, profileWarning: false, warnDistanceM: 20 } });
  c.api.init({});
  c.events.length = 0;
  c.api.resetAll();
  assert.strictEqual(c.localStorage.getItem('cs-theme-overrides'), null);
  const eff = c.api.computeEffective({}, c.api.readOverrides());
  assert.deepStrictEqual(plain(c.api.heritageSettings(eff)), DEFAULTS);
  // The Reset button runs the pipeline next (customize-v2.js), as here.
  c.api.applyCSS(eff, c.api.readOverrides());
  assert(c.events.indexOf('heritage-defaults-changed') >= 0, 'no signal after reset: ' + c.events.join(', '));
});

test('the Ancient Monuments section: shown only with the overlay offered; the profile rows only with the profile', () => {
  const c = loadCustomizer();
  c.ctx.window.SITE_CONFIG = { heritage: { enabled: true, opacityPct: 60 } };
  const html = c.api.renderHeritageSettings();
  assert(/Ancient Monuments/.test(html), html);
  const input = (h, key) => {
    const m = h.match(new RegExp('<input[^>]*data-cv2-heritage="' + key + '"[^>]*>'));
    return m && m[0];
  };
  assert(/type="checkbox"/.test(input(html, 'enabled')) && /checked/.test(input(html, 'enabled')), input(html, 'enabled'));
  assert(/min="10" max="100"/.test(input(html, 'opacityPct')) && /value="60"/.test(input(html, 'opacityPct')), input(html, 'opacityPct'));
  assert(/for="cv2-heritage-opacityPct"/.test(html) && /id="cv2-heritage-opacityPct"/.test(html), 'label not bound');
  assert.strictEqual(input(html, 'profileWarning'), null, 'profile row without the profile tool');
  assert.strictEqual(loadCustomizer({ cfg: null }).api.renderHeritageSettings(), '', 'shown without the overlay offered');

  const p = loadCustomizer({ profile: true });
  p.ctx.window.SITE_CONFIG = { heritage: { profileWarning: false } };
  const ph = p.api.renderHeritageSettings();
  assert(/type="checkbox"/.test(input(ph, 'profileWarning')) && !/checked/.test(input(ph, 'profileWarning')), input(ph, 'profileWarning'));
  const w = input(ph, 'warnDistanceM');
  assert(w && /min="0" max="250"/.test(w) && /value=""/.test(w) && /placeholder="/.test(w), w);
  p.ctx.window.SITE_CONFIG = { heritage: { warnDistanceM: 30 } };
  assert(/value="30"/.test(input(p.api.renderHeritageSettings(), 'warnDistanceM')));
});

test('the customizer and the overlay share one rule (no second copy of the bounds)', () => {
  const src = fs.readFileSync(path.join(__dirname, 'public', 'customize-v2.js'), 'utf8');
  const code = src.replace(/\/\*[\s\S]*?\*\//g, '').replace(/^\s*\/\/.*$/gm, '');
  assert(!/\b250\b/.test(code) && !/opacityPct\s*[<>]=?\s*\d/.test(code) && !/\b0\.85\b/.test(code), 'customize-v2.js hard-codes a heritage bound');
  assert(/MC_HERITAGE_SETTINGS/.test(code));
  const tp = fs.readFileSync(path.join(__dirname, 'public', 'map-tile-providers.js'), 'utf8');
  assert(!/opacity:\s*0\.85/.test(tp), 'the overlay still hard-codes 0.85');
});

// ── 5. Docs ─────────────────────────────────────────────────────────────────

test('docs/ancient-monuments.md: the credit, the settings and the caveat', () => {
  const doc = fs.readFileSync(path.join(__dirname, 'docs', 'ancient-monuments.md'), 'utf8');
  const go = fs.readFileSync(path.join(__dirname, 'cmd', 'server', 'heritage_overlay.go'), 'utf8');
  const credit = go.match(/heritageAttribution\s*=\s*"([^"]+)"/)[1];
  assert(doc.indexOf(credit) >= 0, 'the credit ' + credit + ' is not in the docs');
  const R = rulesOf(makeSandbox({ heritage: HERITAGE }));
  // Each key has its row in the settings table, with its default.
  R.KEYS.forEach((k) => assert(new RegExp('^\\|[^|\\n]+\\|\\s*`' + k + '`\\s*\\|[^|\\n]+\\|[^|\\n]+\\|$', 'm').test(doc), k + ' has no row in the settings table'));
  assert(/100 m/.test(doc) && /§\s*18/.test(doc), 'the 100 m line and its caveat');
  assert(/not .*all-clear|never .*all-clear/i.test(doc), 'silence is never an all-clear');
  assert(/harvest/i.test(doc), 'the no-harvesting rule');
  assert(fs.readFileSync(path.join(__dirname, 'docs', 'CUSTOMIZATION.md'), 'utf8').indexOf('ancient-monuments.md') >= 0, 'not linked from CUSTOMIZATION.md');
});

// ── Run ─────────────────────────────────────────────────────────────────────

(async () => {
  console.log('\n=== #723 M4: ancient monuments settings ===');
  for (const [name, fn] of tests) {
    try { await fn(); passed++; console.log('  ✅ ' + name); }
    catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
  }
  console.log(`\n#723 M4 unit: ${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
