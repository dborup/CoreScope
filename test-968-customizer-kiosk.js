/**
 * #968 M2 (AGENTS rule 8, K1 + K2) — Customize → Display → Live kiosk: the
 * screen's text and tile size (kiosk.scale) and its edge margin
 * (kiosk.safeInsetVmin), per browser, over the operator preset.
 *
 * The real public/live-kiosk.js and public/customize-v2.js run in vm
 * sandboxes; nothing is copied.
 *   1. the rule: LiveKiosk.SETTINGS knows the two keys (VIEWER_KEYS, SCALES,
 *      LIMITS), valid() and display(): viewer > operator preset > auto / 3;
 *   2. applying it: applyDisplay() sets only what differs from the CSS
 *      (auto, 3vmin), clearDisplay() takes it all off; insetPx();
 *   3. the `kiosk` customizer section: valid values kept, others dropped
 *      with a warning, import rejected with a message, export round trip,
 *      a value equal to the operator's not stored, badge and Reset All;
 *   4. the controls: one select and one bounded input, 48 px, a hint;
 *   5. live.js wiring (source pins), the docs.
 * Usage: node test-968-customizer-kiosk.js
 */
'use strict';
const fs = require('fs');
const path = require('path');
const assert = require('assert');
const { loadCustomizer, makeStyle } = require('./tests/customizer-sandbox');
const { loadPublic } = require('./tests/fake-dom');

let passed = 0, failed = 0;
const tests = [];
function test(name, fn) { tests.push([name, fn]); }
const plain = (v) => JSON.parse(JSON.stringify(v === undefined ? null : v));
const eq = (a, b, m) => assert.deepStrictEqual(plain(a), b, m);

const K = loadPublic('live-kiosk.js').LiveKiosk;
const S = K.SETTINGS;

function load(o) {
  o = o || {};
  return loadCustomizer({
    scripts: o.noRule ? [] : ['live-kiosk.js'],
    setup: (ctx) => { ctx.window.MC_KIOSK_CFG = o.preset === undefined ? null : o.preset; },
  });
}

// ── 1. The rule ─────────────────────────────────────────────────────────────

test('SETTINGS: the two viewer keys, their steps, bounds and defaults', () => {
  eq(S.VIEWER_KEYS, ['scale', 'safeInsetVmin']);
  eq(S.SCALES, ['auto', 1, 1.25, 1.5, 2]);
  eq(S.LIMITS.safeInsetVmin, [0, 10]);
  assert.strictEqual(S.DEFAULTS.scale, 'auto');
  assert.strictEqual(S.DEFAULTS.safeInsetVmin, 3);
  S.VIEWER_KEYS.forEach((k) => assert.ok(S.KEYS.indexOf(k) >= 0, k + ' not in KEYS'));
});

test('valid(): scale is auto or a step, exact; the inset a number 0-10', () => {
  const v = S.valid;
  ['auto', 1, 1.25, 1.5, 2].forEach((x) => assert.ok(v('scale', x), String(x)));
  ['Auto', '1.5', 1.75, 0, 3, null, true].forEach((x) => assert.ok(!v('scale', x), String(x)));
  [0, 0.5, 3, 6, 10].forEach((x) => assert.ok(v('safeInsetVmin', x), String(x)));
  [-0.5, 10.5, '6', NaN, Infinity, null].forEach((x) => assert.ok(!v('safeInsetVmin', x), String(x)));
});

test('display(): viewer > operator preset > auto / 3, per key', () => {
  const d = S.display;
  eq(d(null, null), { scale: 'auto', safeInsetVmin: 3 });
  eq(d(null, { scale: 1.5, safeInsetVmin: 5 }), { scale: 1.5, safeInsetVmin: 5 });
  eq(d({ scale: 2 }, { scale: 1.5, safeInsetVmin: 5 }), { scale: 2, safeInsetVmin: 5 });
  eq(d({ safeInsetVmin: 0 }, { scale: 1.5, safeInsetVmin: 5 }), { scale: 1.5, safeInsetVmin: 0 });
  eq(d({ scale: 'auto' }, { scale: 1.5 }), { scale: 'auto', safeInsetVmin: 3 }, 'a viewer auto beats a preset step');
  // A bad viewer value falls back to the preset, a bad preset to the default.
  eq(d({ scale: 9, safeInsetVmin: 99 }, { scale: 1.25, safeInsetVmin: 4 }), { scale: 1.25, safeInsetVmin: 4 });
  eq(d({ scale: 9 }, { scale: 7, safeInsetVmin: -1 }), { scale: 'auto', safeInsetVmin: 3 });
  // Only the two keys, whatever the sections carry.
  eq(Object.keys(d({ tour: ['a'], kiosk: true }, { tiles: ['rate'] })), ['scale', 'safeInsetVmin']);
});

test('parseKioskParams leaves them out: no link parameter sets them', () => {
  const p = K.parseKioskParams(new URLSearchParams('kiosk=1&scale=2&safeInsetVmin=6'), { scale: 1.5 });
  assert.ok(!('scale' in p) && !('safeInsetVmin' in p), JSON.stringify(p));
});

// ── 2. Applying it ──────────────────────────────────────────────────────────

test('applyDisplay(): only what differs from the CSS default is set inline', () => {
  const st = makeStyle();
  K.applyDisplay(st, { scale: 'auto', safeInsetVmin: 3 });
  eq(st._props, {});
  K.applyDisplay(st, { scale: 2, safeInsetVmin: 6 });
  eq(st._props, { '--kiosk-scale': '2', '--kiosk-safe-inset': '6vmin' });
  K.applyDisplay(st, { scale: 'auto', safeInsetVmin: 0.5 });
  eq(st._props, { '--kiosk-safe-inset': '0.5vmin' }, 'auto takes the inline scale off again');
  K.applyDisplay(st, { scale: 1.25, safeInsetVmin: 3 });
  eq(st._props, { '--kiosk-scale': '1.25' });
});

test('clearDisplay(): nothing of kiosk\'s stays on the style', () => {
  const st = makeStyle();
  st.setProperty('--other', 'x');
  K.applyDisplay(st, { scale: 2, safeInsetVmin: 6 });
  K.clearDisplay(st);
  eq(st._props, { '--other': 'x' });
  K.clearDisplay(null); // no body: no throw
});

test('insetPx(): the map padding follows the inset (3 % by default)', () => {
  assert.strictEqual(K.insetPx(3, 1920, 1080), 32);
  assert.strictEqual(K.insetPx(6, 1920, 1080), 65);
  assert.strictEqual(K.insetPx(0, 1920, 1080), 0);
  assert.strictEqual(K.insetPx(3, 375, 560), 11);
});

// ── 3. The customizer section ───────────────────────────────────────────────

test('kiosk is a known object section with the rule\'s viewer keys', () => {
  const { api } = load();
  assert.ok(api.VALID_SECTIONS.indexOf('kiosk') >= 0);
  eq(api.KIOSK_SETTING_KEYS, ['scale', 'safeInsetVmin']);
  eq(api.kioskSettings({}), { scale: 'auto', safeInsetVmin: 3 });
  assert.strictEqual(api.validateShape({ kiosk: { scale: 2 } }).valid, true);
  assert.strictEqual(api.validateShape({ kiosk: 2 }).valid, false);
});

test('writeOverrides keeps valid values, drops the rest and every operator-only key', () => {
  const c = load();
  c.api.writeOverrides({ kiosk: { scale: 1.75, safeInsetVmin: 6, tour: ['AAA'], tiles: ['rate'], theme: 'light', everySeconds: 30, remountHours: 2 } });
  eq(c.api.readOverrides(), { kiosk: { safeInsetVmin: 6 } });
  assert.ok(c.warnings.some((w) => /kiosk\.scale/.test(w)), 'a rejected value is logged');
});

test('without live-kiosk.js on the page no value is accepted', () => {
  const c = load({ noRule: true });
  c.api.writeOverrides({ kiosk: { scale: 2 } });
  eq(c.api.readOverrides(), {});
  assert.strictEqual(c.api.kioskSettings({}), null);
  assert.strictEqual(c.api.renderKioskSettings(), '');
});

test('import: each bad value is an error naming the allowed values; unknown keys only warn', () => {
  const { api, warnings } = load();
  const res = api.validateShape({ kiosk: { scale: '1.5', safeInsetVmin: 12, tour: ['a'] } });
  assert.strictEqual(res.valid, false);
  const txt = res.errors.join('\n');
  assert.ok(/kiosk\.scale = "1\.5".*auto, 1, 1\.25, 1\.5 or 2/.test(txt), txt);
  assert.ok(/kiosk\.safeInsetVmin = "12".*0-10/.test(txt), txt);
  assert.ok(!/tour/.test(txt), 'an operator key is not an error');
  assert.ok(warnings.some((w) => /Unknown kiosk key.*tour/.test(w)));
});

test('export round trip', () => {
  const delta = { kiosk: { scale: 1.25, safeInsetVmin: 4.5 } };
  const a = load();
  a.api.writeOverrides(delta);
  const exported = a.api.readOverrides();
  eq(exported, delta);
  const b = load();
  assert.strictEqual(b.api.validateShape(exported).valid, true);
  b.api.writeOverrides(exported);
  eq(b.api.kioskSettings(b.api.computeEffective({}, b.api.readOverrides())), { scale: 1.25, safeInsetVmin: 4.5 });
});

test('the effective values: viewer > operator preset > auto / 3', () => {
  const c = load({ preset: { scale: 1.5, safeInsetVmin: 5 } });
  eq(c.api.kioskSettings({}), { scale: 1.5, safeInsetVmin: 5 });
  eq(c.api.kioskSettings({ kiosk: { scale: 2 } }), { scale: 2, safeInsetVmin: 5 });
});

test('setKioskSetting: a value equal to the operator\'s is not stored; a bad one is refused', () => {
  const c = load({ preset: { scale: 1.5, safeInsetVmin: 5 } });
  c.api.init({});
  assert.strictEqual(c.api.setKioskSetting('scale', 2), true);
  assert.strictEqual(c.api.setKioskSetting('safeInsetVmin', 6), true);
  eq(c.api.readOverrides(), { kiosk: { scale: 2, safeInsetVmin: 6 } });
  assert.strictEqual(c.api.setKioskSetting('scale', 1.5), true, 'back to the operator\'s');
  eq(c.api.readOverrides(), { kiosk: { safeInsetVmin: 6 } });
  assert.strictEqual(c.api.setKioskSetting('scale', 'auto'), true, 'auto over a preset step is a choice');
  eq(c.api.readOverrides(), { kiosk: { scale: 'auto', safeInsetVmin: 6 } });
  assert.strictEqual(c.api.setKioskSetting('safeInsetVmin', 11), false);
  assert.strictEqual(c.api.setKioskSetting('tour', ['a']), false);
  eq(c.api.readOverrides(), { kiosk: { scale: 'auto', safeInsetVmin: 6 } });
  // Without a preset the built-ins are the operator's default.
  const d = load();
  d.api.init({});
  d.api.setKioskSetting('scale', 'auto');
  d.api.setKioskSetting('safeInsetVmin', 3);
  eq(d.api.readOverrides(), {});
});

test('the Display badge counts the section; Reset All brings auto / 3 back', () => {
  const c = load();
  c.api.init({});
  const before = c.api.countOverrides('kiosk');
  c.api.writeOverrides({ kiosk: { scale: 2, safeInsetVmin: 6 } });
  eq([before, c.api.countOverrides('kiosk')], [0, 2]);
  const tabs = c.api.renderTabs ? c.api.renderTabs('display') : '';
  assert.ok(/cv2-tab-badge">2</.test(tabs), 'Display badge: ' + tabs.slice(0, 200));
  c.api.resetAll();
  eq(c.api.readOverrides(), {});
  eq(c.api.kioskSettings(c.ctx.window.SITE_CONFIG), { scale: 'auto', safeInsetVmin: 3 });
});

test('theme-changed fires when the section changes (live.js reapplies on it)', () => {
  const c = load();
  c.api.init({});
  const n0 = c.events.filter((t) => t === 'theme-changed').length;
  c.api.setKioskSetting('scale', 2);
  assert.ok(c.events.filter((t) => t === 'theme-changed').length > n0);
});

// ── 4. The controls ─────────────────────────────────────────────────────────

test('controls: a select of the steps and a bounded inset input, 48 px, with the hint', () => {
  const c = load({ preset: { scale: 1.5, safeInsetVmin: 5 } });
  const html = c.api.renderKioskSettings();
  const sel = (html.match(/<select[^>]*data-cv2-kiosk="scale"[^>]*>[\s\S]*?<\/select>/) || [''])[0];
  assert.ok(sel, 'no scale select');
  eq((sel.match(/value="[^"]*"/g) || []), ['value="auto"', 'value="1"', 'value="1.25"', 'value="1.5"', 'value="2"']);
  assert.ok(/<option value="1\.5" selected/.test(sel), 'the operator value is shown');
  assert.ok(/min-height:48px/.test(sel));
  const inp = (html.match(/<input[^>]*data-cv2-kiosk="safeInsetVmin"[^>]*>/) || [''])[0];
  assert.ok(/type="number"/.test(inp) && /min="0"/.test(inp) && /max="10"/.test(inp) && /step="0\.5"/.test(inp), inp);
  assert.ok(/value="5"/.test(inp), inp);
  assert.ok(/class="cust-terrain-row"/.test(html), 'the 48 px number row');
  assert.ok(/Live kiosk/.test(html));
  assert.ok(/before opening the kiosk link/.test(html) && /outside kiosk/.test(html), 'the hint says how to use it');
  assert.ok(/1\.5/.test(html) && /server/i.test(html), 'the server\'s value is named');
  ['tour', 'everySeconds', 'remountHours', 'tiles'].forEach((k) => assert.ok(!html.includes('data-cv2-kiosk="' + k), k));
});

test('controls: an override dot per changed key', () => {
  const c = load();
  c.api.writeOverrides({ kiosk: { scale: 2, safeInsetVmin: 6 } });
  c.ctx.window.SITE_CONFIG = c.api.computeEffective({}, c.api.readOverrides());
  const html = c.api.renderKioskSettings();
  assert.strictEqual((html.match(/data-reset-s="kiosk"/g) || []).length, 2);
  assert.ok(/<option value="2" selected/.test(html));
});

test('the Display tab renders the section', () => {
  const c = load();
  assert.ok(/data-cv2-kiosk="scale"/.test(c.api.renderDisplay()));
});

// ── 5. live.js wiring, docs ─────────────────────────────────────────────────

const live = fs.readFileSync(path.join(__dirname, 'public/live.js'), 'utf8');
const cust = fs.readFileSync(path.join(__dirname, 'public/customize-v2.js'), 'utf8');

test('live.js applies the display on mount and on theme-changed, and clears it on exit', () => {
  const init = live.slice(live.indexOf('async function init(app)'), live.indexOf('app.innerHTML'));
  assert.ok(/applyKioskDisplay\(\)/.test(init), 'not applied on mount');
  const fn = live.slice(live.indexOf('function kioskDisplay('), live.indexOf('function applyKioskDisplay('));
  assert.ok(/SETTINGS\.display\(\s*window\.SITE_CONFIG && window\.SITE_CONFIG\.kiosk,\s*window\.MC_KIOSK_CFG\s*\)/.test(fn), fn);
  assert.ok(/addEventListener\('theme-changed', applyKioskDisplay\)/.test(live));
  const destroy = live.slice(live.indexOf('function destroy()'));
  assert.ok(/removeEventListener\('theme-changed', applyKioskDisplay\)/.test(destroy));
  assert.ok(/clearDisplay\(document\.body/.test(destroy), 'inline vars left on body');
  const kInit = live.slice(live.indexOf('async function initKiosk('), live.indexOf('function setKioskAutoReload('));
  assert.ok(/insetPx\(/.test(kInit) && !/0\.03 \*/.test(kInit), 'the fly padding ignores the inset');
});

test('the customizer has no colour of its own for the section', () => {
  const at = cust.indexOf('function _renderKioskSettings(');
  assert.ok(at >= 0, 'no _renderKioskSettings');
  const r = cust.slice(at, cust.indexOf('\n  }\n', at));
  assert.ok(/data-cv2-kiosk/.test(r), 'slice misses the body');
  assert.ok(!/#[0-9a-fA-F]{3,8}\b/.test(r));
});

test('the docs name both keys, both sides and the precedence', () => {
  const doc = fs.readFileSync(path.join(__dirname, 'docs/CUSTOMIZATION.md'), 'utf8');
  ['kiosk.scale', 'kiosk.safeInsetVmin', 'Live kiosk', '`auto`', '0 to 10'].forEach((w) => assert.ok(doc.includes(w), 'CUSTOMIZATION.md misses ' + w));
  assert.ok(/your browser.*operator|viewer.*preset/i.test(doc), 'precedence missing');
  assert.ok(!/follow in a later release \(#968 M2/.test(doc), 'the M1 placeholder sentence is still there');
});

for (const [name, fn] of tests) {
  try { fn(); passed++; console.log('  ✅ ' + name); } catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}
console.log('\n' + passed + ' passed, ' + failed + ' failed');
process.exit(failed ? 1 : 0);
