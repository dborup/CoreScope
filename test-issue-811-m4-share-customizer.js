/**
 * Unit tests (#811 M4): the customizer's "Share previews" switch covers
 * node cards as well as terrain-profile cards (public/customize-v2.js).
 *
 * The #727 M4 switch (terrain.shareLinks) becomes the one "Copy link shares
 * a preview" setting for both card types; there is no second, node-only
 * setting. share.nodes.enabled and the card's look stay server-only: the
 * customizer shows them (from /api/config/client's share block) and stores
 * nothing for them.
 *
 *   - node sharing on, no terrain profile: a standalone Share previews
 *     section with the switch, the card look and the palette line;
 *   - node sharing on with the profile but without profile sharing: the
 *     switch in the Terrain Profile section, not the off note;
 *   - both on: one switch, whose hint names both card types;
 *   - node sharing off: everything as #727 M4 left it.
 *
 * Usage: node test-issue-811-m4-share-customizer.js
 */
'use strict';

const assert = require('assert');
const { loadCustomizer } = require('./tests/customizer-sandbox');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

const PALETTE_LINE = 'The card is drawn on the server in the operator’s saved palette, not with the colour overrides in this browser.';
const NODES = { nodes: true, roles: ['repeater', 'room', 'sensor'], card: { theme: 'light', names: false } };
const PROFILE_ONLY = { enabled: true, profile: true, share: false };
const PROFILE_SHARE = { enabled: true, profile: true, share: true, card: { theme: 'dark', names: true } };
const SWITCH = /<input type="checkbox" id="cv2-terrain-share"[^>]*>/;

function load(dhm, share, delta) {
  const c = loadCustomizer({
    dhm,
    scripts: ['node-share.js'],
    setup: (ctx) => {
      ctx.window.MC_SHARE_CFG = share;
      ctx.window.TerrainProfile = {
        offered: (cfg) => !!(cfg && cfg.enabled === true && cfg.profile === true),
        shareOffered: (cfg) => !!(cfg && cfg.enabled === true && cfg.profile === true && cfg.share === true),
      };
    },
  });
  if (delta) { c.api.writeOverrides(delta); c.api.init({}); }
  return c.api;
}

/** The whole share UI the Display tab would show. */
function shown(api) { return api.renderTerrainSettings() + api.renderShareSection(); }

console.log('\n🔗 #811 M4 — customizer: share previews for node cards\n');

test('node sharing on, no terrain profile: a standalone Share previews section', () => {
  const api = load(null, NODES);
  assert.strictEqual(api.renderTerrainSettings(), '', 'no profile, no Terrain Profile section');
  const html = api.renderShareSection();
  assert.ok(/Share previews/.test(html), 'no heading');
  const box = html.match(SWITCH);
  assert.ok(box && / checked/.test(box[0]) && /data-cv2-terrain-share/.test(box[0]), 'no wired switch, on by default');
  assert.ok(/node/i.test(html), 'the hint does not say node links get a card');
  assert.ok(!/profile/i.test(html.replace(/<[^>]+>/g, '')), 'a profile card mentioned on a server without one');
  assert.ok(/id="cv2-card-theme"[^>]*>Light</.test(html), 'card theme from share.card');
  assert.ok(/id="cv2-card-names"[^>]*>Hidden</.test(html), 'names from share.card');
  assert.ok(!/<(input|select)[^>]*cv2-card-/.test(html), 'the card look became a control');
  assert.strictEqual(html.split(PALETTE_LINE).length, 2, 'palette line missing or repeated');
});

test('the standalone switch reflects a stored false', () => {
  const box = load(null, NODES, { terrain: { shareLinks: false } }).renderShareSection().match(SWITCH);
  assert.ok(box && !/ checked/.test(box[0]));
});

test('node sharing on, profile without profile sharing: the switch, not the off note', () => {
  const api = load(PROFILE_ONLY, NODES);
  const html = shown(api);
  assert.strictEqual(html.split('id="cv2-terrain-share"').length, 2, 'not exactly one switch');
  assert.ok(!/Link previews are off on this server/.test(html), 'the off note while node previews are on');
  assert.ok(/node/i.test(api.renderTerrainSettings()), 'the hint does not say node links get a card');
  assert.strictEqual(api.renderShareSection(), '', 'a second section next to the Terrain Profile one');
});

test('both card types on: one switch whose hint names both, one palette line', () => {
  const api = load(PROFILE_SHARE, NODES);
  const html = shown(api);
  assert.strictEqual(html.split('id="cv2-terrain-share"').length, 2, 'not exactly one switch');
  const text = html.replace(/<[^>]+>/g, '');
  assert.ok(/profile/i.test(text) && /node/i.test(text), 'the hint does not name both card types');
  assert.strictEqual(html.split(PALETTE_LINE).length, 2);
  // Both cards use map.dhm.profile.card, so one look is shown.
  assert.strictEqual(html.split('id="cv2-card-theme"').length, 2);
});

test('node sharing off: the #727 M4 section is unchanged', () => {
  [null, undefined, { nodes: false, roles: NODES.roles }].forEach((share) => {
    const on = load(PROFILE_SHARE, share);
    const ref = load(PROFILE_SHARE, undefined);
    assert.strictEqual(on.renderTerrainSettings(), ref.renderTerrainSettings(), JSON.stringify(share));
    assert.strictEqual(on.renderShareSection(), '');
    const off = load(PROFILE_ONLY, share);
    assert.ok(/Link previews are off on this server/.test(off.renderTerrainSettings()));
    assert.strictEqual(load(null, share).renderShareSection(), '', 'a section with nothing to share');
  });
});

test('the profile hint is #727 M4’s wording when only profiles are shared', () => {
  const html = load(PROFILE_SHARE, null).renderTerrainSettings();
  assert.ok(/Pasted into a chat, the link shows a card of the profile\. Off: the plain map link, without a card\./.test(html));
});

test('a server value is compared, never printed', () => {
  const evil = { nodes: true, roles: ['repeater'], card: { theme: '<img src=x onerror=alert(1)>', names: '<b>' } };
  const html = load(null, evil).renderShareSection();
  assert.ok(!/<img|<b>/.test(html), 'server text echoed');
  assert.ok(/id="cv2-card-theme"[^>]*>Dark</.test(html), 'unknown theme is not the dark default');
  assert.ok(/id="cv2-card-names"[^>]*>Shown</.test(html), 'non-boolean names is not the default');
});

console.log('\n' + passed + ' passed, ' + failed + ' failed\n');
if (failed > 0) process.exit(1);
