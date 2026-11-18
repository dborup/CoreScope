/**
 * Unit tests (#789 M3): the viewer side of owner antenna heights in the
 * customizer (public/customize-v2.js) and the theme (public/style.css).
 *
 *   1. terrain.preferOwnerHeights — the viewer's "Prefer owner heights"
 *      switch, on by default: only a boolean is stored, import names a bad
 *      value, export and reset carry it, and toggling it re-measures an
 *      open Path profile (terrain-defaults-changed), because it changes
 *      which heights the viewer's defaults replace. The profile's side
 *      (owner heights replaced only when it is off) is in
 *      test-issue-681-profile-tool.js.
 *   2. ownerBadge — the owner badge's colour is the theme key ownerBadge
 *      on --owner-badge, labelled, in the profile colour group, light and
 *      dark apart; style.css draws the badge with it and defaults it to
 *      --success, so an untouched theme looks exactly as before.
 *
 * customize-v2.js runs in tests/customizer-sandbox.js; nothing is copied.
 * Usage: node test-issue-789-m3-customizer.js
 */
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');
const { loadCustomizer } = require('./tests/customizer-sandbox');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

const ON = { enabled: true, profile: true };
function load(opts) {
  return loadCustomizer(Object.assign({
    setup: (ctx) => { ctx.window.TerrainProfile = { offered: (c) => !!(c && c.enabled === true && c.profile === true), shareOffered: () => false }; },
  }, opts || {}));
}
const stored = (c) => JSON.parse(c.localStorage.getItem('cs-theme-overrides') || '{}');
const effective = (c) => c.api.computeEffective({}, c.api.readOverrides());

console.log('\n🏷️  #789 M3 — customizer: owner antenna heights\n');

console.log('=== terrain.preferOwnerHeights ===');

test('the key is terrain.preferOwnerHeights and it is on unless set false', () => {
  const { api } = load();
  assert.strictEqual(api.TERRAIN_OWNER_KEY, 'preferOwnerHeights');
  assert.strictEqual(api.ownerSettings({}).preferOwnerHeights, true);
  assert.strictEqual(api.ownerSettings({ terrain: { preferOwnerHeights: true } }).preferOwnerHeights, true);
  assert.strictEqual(api.ownerSettings({ terrain: { preferOwnerHeights: false } }).preferOwnerHeights, false);
  ['false', 0, '', null, {}].forEach((v) => {
    assert.strictEqual(api.ownerSettings({ terrain: { preferOwnerHeights: v } }).preferOwnerHeights, true, 'junk ' + JSON.stringify(v));
  });
});

test('only a boolean is stored, and a bad one costs no other terrain key', () => {
  [false, true].forEach((v) => {
    const c = load();
    c.api.writeOverrides({ terrain: { preferOwnerHeights: v } });
    assert.deepStrictEqual(stored(c).terrain, { preferOwnerHeights: v });
  });
  ['false', 0, 1, {}].forEach((v) => {
    const c = load();
    c.api.writeOverrides({ terrain: { preferOwnerHeights: v, heightSensor: 3 } });
    assert.deepStrictEqual(stored(c).terrain, { heightSensor: 3 }, 'stored ' + JSON.stringify(v));
    assert.ok(c.warnings.some((w) => /owner/i.test(w)), 'no warning for ' + JSON.stringify(v));
  });
});

test('validateShape accepts true and false and names anything else', () => {
  const { api, warnings } = load();
  [true, false].forEach((v) => {
    const ok = api.validateShape({ terrain: { preferOwnerHeights: v } });
    assert.strictEqual(ok.valid, true, ok.errors.join('; '));
  });
  assert.ok(!warnings.some((w) => /Unknown terrain key/.test(w)), warnings.join(' | '));
  ['false', 0, null].forEach((v) => {
    const bad = api.validateShape({ terrain: { preferOwnerHeights: v } });
    assert.strictEqual(bad.valid, false, 'accepted ' + JSON.stringify(v));
    assert.ok(/terrain\.preferOwnerHeights/.test(bad.errors.join(' ')), bad.errors.join(' '));
  });
});

test('an export round trip carries it, and resetAll strips it', () => {
  const first = load();
  const delta = { terrain: { heightRepeater: 18, preferOwnerHeights: false } };
  first.api.writeOverrides(delta);
  const exported = JSON.parse(JSON.stringify(first.api.readOverrides()));
  assert.deepStrictEqual(exported, delta);
  const second = load();
  second.api.writeOverrides(exported);
  assert.strictEqual(second.api.ownerSettings(effective(second)).preferOwnerHeights, false);
  second.api.resetAll();
  assert.strictEqual(second.api.ownerSettings(effective(second)).preferOwnerHeights, true);
});

test('toggling it re-measures an open profile, once', () => {
  const c = load({ server: {} });
  c.api.init({});
  c.events.length = 0;
  c.api.writeOverrides({ terrain: { preferOwnerHeights: false } });
  c.api.applyCSS(effective(c), c.api.readOverrides());
  assert.strictEqual(c.events.filter((e) => e === 'terrain-defaults-changed').length, 1, c.events.join(', '));
  c.events.length = 0;
  c.api.applyCSS(effective(c), c.api.readOverrides());
  assert.strictEqual(c.events.indexOf('terrain-defaults-changed'), -1, 'fired again without a change');
});

test('the section shows the switch, on by default and off once stored false, as a 48px row', () => {
  const html = load({ dhm: ON }).api.renderTerrainSettings();
  const box = html.match(/<input type="checkbox" id="cv2-terrain-owner"[^>]*>/);
  assert.ok(box, 'no switch');
  assert.ok(/data-cv2-terrain-owner/.test(box[0]) && / checked/.test(box[0]), box[0]);
  assert.ok(/<label class="cust-check" for="cv2-terrain-owner" style="[^"]*min-height:48px/.test(html), 'the row is under 48px');
  assert.ok(/Prefer owner heights/.test(html), 'no label');
  assert.ok(/aria-describedby="cv2-terrain-owner-hint"/.test(box[0]) && /id="cv2-terrain-owner-hint"/.test(html), 'the hint is not tied to the switch');
  const c = load({ dhm: ON });
  c.api.writeOverrides({ terrain: { preferOwnerHeights: false } });
  c.api.init({});
  const off = c.api.renderTerrainSettings().match(/<input type="checkbox" id="cv2-terrain-owner"[^>]*>/);
  assert.ok(off && !/ checked/.test(off[0]), 'a stored false still shows on');
});

console.log('\n=== ownerBadge ===');

test('ownerBadge maps onto --owner-badge, with a label, in the profile colour group', () => {
  const { api } = load();
  assert.strictEqual(api.THEME_CSS_MAP.ownerBadge, '--owner-badge');
  assert.ok(api.TERRAIN_COLOR_KEYS.indexOf('ownerBadge') >= 0, 'not in the profile colour group');
  const src = fs.readFileSync(path.join(__dirname, 'public', 'customize-v2.js'), 'utf8');
  assert.ok(/ownerBadge: 'Owner Height Badge'/.test(src), 'no label');
  assert.ok(/ownerBadge: '[^']*owner[^']*'/.test(src.slice(src.indexOf('var THEME_HINTS'))), 'no hint');
});

test('a theme or themeDark ownerBadge reaches --owner-badge, and reset clears it', () => {
  ['theme', 'themeDark'].forEach((section) => {
    const c = load({ theme: section === 'themeDark' ? 'dark' : 'light' });
    const delta = {};
    delta[section] = { ownerBadge: '#123456' };
    c.api.applyCSS(c.api.computeEffective({}, delta), delta);
    assert.strictEqual(c.rootStyle.getPropertyValue('--owner-badge'), '#123456', section);
    c.api.resetAll();
    assert.strictEqual(c.rootStyle.getPropertyValue('--owner-badge'), '', section + ' survived reset');
  });
});

test('style.css draws the owner badge with --owner-badge, which defaults to --success', () => {
  const css = fs.readFileSync(path.join(__dirname, 'public', 'style.css'), 'utf8');
  const rule = css.match(/\.tp-hbadge-owner\s*\{([^}]*)\}/);
  assert.ok(rule && /border-color:\s*var\(--owner-badge\)/.test(rule[1]), rule && rule[1]);
  const roots = css.match(/(^|\n):root\s*\{[^}]*\}/g) || [];
  assert.ok(roots.some((r) => /--owner-badge:\s*var\(--success\);/.test(r)), 'no --owner-badge default on :root');
  assert.ok(!/#[0-9a-f]{3,8}/i.test(rule[1]), 'a hardcoded colour in the badge rule');
});

console.log('\n' + passed + ' passed, ' + failed + ' failed');
process.exit(failed ? 1 : 0);
