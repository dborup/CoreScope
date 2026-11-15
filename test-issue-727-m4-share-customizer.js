/**
 * Unit tests (#727 M4): the share previews in the customizer's Terrain
 * Profile section (public/customize-v2.js).
 *
 * What the section owns, and what it only shows:
 *   - terrain.shareLinks is the viewer's own switch: whether the panel's
 *     Copy link hands out the /p link that previews in a chat, or the plain
 *     #/map link. It cannot create a preview the server does not serve.
 *   - the card's theme and names are the operator's (map.dhm.profile.card,
 *     published as dhm.card); the card is drawn once on the server for
 *     everyone, so the customizer shows them and stores nothing for them.
 *   - one line says the card uses the operator's saved palette, not this
 *     browser's colour overrides.
 *
 * customize-v2.js runs in tests/customizer-sandbox.js; nothing is copied.
 *
 * Covered:
 *   1. the stored key: only a boolean is stored, other terrain keys
 *      survive a bad one, and the effective value is on unless set false;
 *   2. import: validateShape names a bad value, an export round trip
 *      carries it, and resetAll strips it;
 *   3. the switch is not a terrain default: toggling it fires no
 *      terrain-defaults-changed (no re-measure) and moves no height;
 *   4. the rendered section: the switch with dhm.share, the operator's
 *      theme and names from dhm.card in fixed words only, the palette
 *      line, and the off-on-this-server note without dhm.share.
 *
 * Usage: node test-issue-727-m4-share-customizer.js
 */
'use strict';

const assert = require('assert');
const { loadCustomizer } = require('./tests/customizer-sandbox');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

const ON = { enabled: true, profile: true, share: true, card: { theme: 'dark', names: true } };
const PALETTE_LINE = 'The card is drawn on the server in the operator’s saved palette, not with the colour overrides in this browser.';

/* The two gates the section asks public/terrain-profile.js (the real ones
 * are covered in test-issue-727-share-link.js). */
function profileGates(ctx) {
  ctx.window.TerrainProfile = {
    offered: (c) => !!(c && c.enabled === true && c.profile === true),
    shareOffered: (c) => !!(c && c.enabled === true && c.profile === true && c.share === true),
  };
}

function load(opts) {
  opts = Object.assign({ setup: profileGates }, opts || {});
  return loadCustomizer(opts);
}

const stored = (c) => JSON.parse(c.localStorage.getItem('cs-theme-overrides') || '{}');

console.log('\n🔗 #727 M4 — customizer: share previews\n');

// ── 1. The stored key ────────────────────────────────────────────────────

console.log('=== terrain.shareLinks ===');

test('the key is terrain.shareLinks and it is on unless set false', () => {
  const { api } = load();
  assert.strictEqual(api.TERRAIN_SHARE_KEY, 'shareLinks');
  assert.strictEqual(typeof api.shareSettings, 'function');
  assert.strictEqual(api.shareSettings({}).shareLinks, true);
  assert.strictEqual(api.shareSettings({ terrain: {} }).shareLinks, true);
  assert.strictEqual(api.shareSettings({ terrain: { shareLinks: true } }).shareLinks, true);
  assert.strictEqual(api.shareSettings({ terrain: { shareLinks: false } }).shareLinks, false);
  // Junk is no choice: the link stays what the server offers.
  ['false', 0, '', null, 'no', {}].forEach((v) => {
    assert.strictEqual(api.shareSettings({ terrain: { shareLinks: v } }).shareLinks, true, 'junk ' + JSON.stringify(v) + ' turned it off');
  });
});

test('only a boolean is stored, and a bad one costs no other terrain key', () => {
  [false, true].forEach((v) => {
    const c = load();
    c.api.writeOverrides({ terrain: { shareLinks: v } });
    assert.deepStrictEqual(stored(c).terrain, { shareLinks: v });
  });
  ['false', 0, 1, 'yes', {}, []].forEach((v) => {
    const c = load();
    c.api.writeOverrides({ terrain: { shareLinks: v, heightSensor: 3 } });
    assert.deepStrictEqual(stored(c).terrain, { heightSensor: 3 }, 'stored ' + JSON.stringify(v));
    assert.ok(c.warnings.some(w => /share/i.test(w)), 'no warning for ' + JSON.stringify(v));
  });
});

test('the override counts on the Display tab badge like any terrain default', () => {
  const { api } = load();
  api.writeOverrides({ terrain: { shareLinks: false } });
  assert.strictEqual(api.readOverrides().terrain.shareLinks, false);
});

// ── 2. Import, export, reset ─────────────────────────────────────────────

console.log('\n=== import / export / reset ===');

test('validateShape accepts true and false and names anything else', () => {
  const { api, warnings } = load();
  [true, false].forEach((v) => {
    const ok = api.validateShape({ terrain: { shareLinks: v } });
    assert.strictEqual(ok.valid, true, ok.errors.join('; '));
  });
  assert.ok(!warnings.some(w => /Unknown terrain key/.test(w)), 'shareLinks warned as unknown: ' + warnings.join(' | '));
  ['false', 0, null, 'off'].forEach((v) => {
    const bad = api.validateShape({ terrain: { shareLinks: v } });
    assert.strictEqual(bad.valid, false, 'accepted ' + JSON.stringify(v));
    assert.ok(/terrain\.shareLinks/.test(bad.errors.join(' ')), bad.errors.join(' '));
  });
});

test('an export round trip carries the switch next to the terrain defaults', () => {
  const first = load();
  const delta = { terrain: { heightRepeater: 18, model: 'dtm', shareLinks: false } };
  first.api.writeOverrides(delta);
  const exported = JSON.parse(JSON.stringify(first.api.readOverrides()));
  assert.deepStrictEqual(exported, delta);
  const second = load();
  const shape = second.api.validateShape(exported);
  assert.strictEqual(shape.valid, true, shape.errors.join('; '));
  second.api.writeOverrides(exported);
  assert.deepStrictEqual(JSON.parse(JSON.stringify(second.api.readOverrides())), delta);
  assert.strictEqual(second.api.shareSettings(second.api.computeEffective({}, second.api.readOverrides())).shareLinks, false);
});

test('resetAll strips the switch with the rest of the customizer', () => {
  const c = load();
  c.api.writeOverrides({ terrain: { shareLinks: false, heightRoom: 7 } });
  assert.strictEqual(stored(c).terrain.shareLinks, false, 'precondition');
  c.api.resetAll();
  assert.strictEqual(c.localStorage.getItem('cs-theme-overrides'), null);
  assert.strictEqual(c.api.shareSettings(c.api.computeEffective({}, c.api.readOverrides())).shareLinks, true);
});

// ── 3. Not a terrain default ─────────────────────────────────────────────

console.log('\n=== the switch is not a profile default ===');

test('toggling it fires no terrain-defaults-changed and moves no height', () => {
  const c = load({ server: {} });
  c.api.init({});
  const before = JSON.stringify(c.api.terrainSettings(c.api.computeEffective({}, c.api.readOverrides())));
  c.events.length = 0;
  c.api.writeOverrides({ terrain: { shareLinks: false } });
  c.api.applyCSS(c.api.computeEffective({}, c.api.readOverrides()), c.api.readOverrides());
  assert.strictEqual(c.events.indexOf('terrain-defaults-changed'), -1,
    'the share switch would re-measure an open profile: ' + c.events.join(', '));
  assert.strictEqual(JSON.stringify(c.api.terrainSettings(c.api.computeEffective({}, c.api.readOverrides()))), before);
});

// ── 4. The rendered section ──────────────────────────────────────────────

console.log('\n=== the rendered section ===');

function section(dhm, delta) {
  const c = load({ dhm });
  if (delta) { c.api.writeOverrides(delta); c.api.init({}); }
  return c.api.renderTerrainSettings();
}

test('with dhm.share: the switch, on by default, and off once stored false', () => {
  const html = section(ON);
  assert.ok(/Share previews/.test(html), 'no Share previews heading');
  const box = html.match(/<input type="checkbox" id="cv2-terrain-share"[^>]*>/);
  assert.ok(box, 'no switch');
  assert.ok(/data-cv2-terrain-share/.test(box[0]), 'switch not wired');
  assert.ok(/ checked/.test(box[0]), 'the switch is off by default');
  const off = section(ON, { terrain: { shareLinks: false } }).match(/<input type="checkbox" id="cv2-terrain-share"[^>]*>/);
  assert.ok(off && !/ checked/.test(off[0]), 'a stored false still shows on');
  // A touch target like the other switches in the section.
  assert.ok(/<label class="cust-check" for="cv2-terrain-share" style="[^"]*min-height:48px/.test(html), 'the switch row is under 48px');
});

test('the operator’s card theme and names are shown, not offered', () => {
  const dark = section(ON);
  assert.ok(/id="cv2-card-theme"[^>]*>Dark</.test(dark), 'theme not shown: ' + dark);
  assert.ok(/id="cv2-card-names"[^>]*>Shown</.test(dark), 'names not shown');
  const light = section(Object.assign({}, ON, { card: { theme: 'light', names: false } }));
  assert.ok(/id="cv2-card-theme"[^>]*>Light</.test(light), 'light theme not shown');
  assert.ok(/id="cv2-card-names"[^>]*>Hidden</.test(light), 'hidden names not shown');
  // Nothing in the customizer can change them: no input, no stored key.
  assert.ok(!/<(input|select)[^>]*cv2-card-/.test(light), 'the card theme or names became a control');
});

test('the card words are fixed: a server value is never echoed', () => {
  const html = section(Object.assign({}, ON, { card: { theme: '<img src=x onerror=alert(1)>', names: 'yes' } }));
  assert.ok(html.indexOf('<img') === -1 && html.indexOf('onerror') === -1, 'echoed the theme');
  assert.ok(/id="cv2-card-theme"[^>]*>Dark</.test(html), 'an unknown theme is not the dark default');
  assert.ok(/id="cv2-card-names"[^>]*>Shown</.test(html), 'a non-boolean names is not the default');
});

test('one line says the card uses the operator’s saved palette', () => {
  const html = section(ON);
  assert.strictEqual(html.split(PALETTE_LINE).length, 2, 'the palette line is missing or repeated');
});

test('an older server without dhm.card: the switch, no theme or names rows', () => {
  const html = section({ enabled: true, profile: true, share: true });
  assert.ok(/id="cv2-terrain-share"/.test(html));
  assert.ok(!/cv2-card-theme|cv2-card-names/.test(html), 'rows for values the server did not send');
  assert.strictEqual(html.split(PALETTE_LINE).length, 2);
});

test('without dhm.share: no switch, a note that previews are off here', () => {
  const html = section({ enabled: true, profile: true, share: false, card: { theme: 'light', names: false } });
  assert.ok(/Terrain Profile/.test(html), 'the section itself went missing');
  assert.ok(!/cv2-terrain-share|cv2-card-/.test(html), 'share controls without dhm.share');
  assert.ok(/Link previews are off on this server/.test(html), 'no note');
  assert.ok(html.indexOf(PALETTE_LINE) === -1, 'a palette line for a card this server does not draw');
});

test('without the profile: no section, no share block', () => {
  assert.strictEqual(section({ enabled: true, profile: false, share: true }), '');
});

// ── Summary ──────────────────────────────────────────────────────────────

console.log('\n' + passed + ' passed, ' + failed + ' failed\n');
if (failed > 0) process.exit(1);
