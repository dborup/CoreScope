/**
 * Unit tests (#812 M4): the customizer's "Share previews" switch covers
 * packet links too (public/customize-v2.js).
 *
 * There is no new switch: terrain.shareLinks ("Copy link shares a
 * preview", #727 M4, #811 M4) covers terrain profiles, node links and
 * packet links. share.packets.enabled stays server-only; the customizer
 * only reports that packet links get a card.
 *
 *   - packet sharing on, nothing else: a standalone Share previews section
 *     with the switch, whose hint names packet links and nothing else;
 *   - packet and node sharing: one switch, the hint names both;
 *   - packet sharing with the profile: the switch in the Terrain Profile
 *     section, not the off note, and no second section;
 *   - packet sharing off: everything as #811 M4 left it.
 *
 * Usage: node test-issue-812-m4-share-customizer.js
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
const PACKETS = { nodes: false, packets: true, card: { theme: 'light', names: false } };
const BOTH = { nodes: true, packets: true, roles: NODES.roles, card: { theme: 'dark', names: true } };
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

const text = (html) => html.replace(/<[^>]+>/g, '');
function shown(api) { return api.renderTerrainSettings() + api.renderShareSection(); }

console.log('\n🔗 #812 M4 — customizer: share previews for packet links\n');

test('packet sharing alone, no terrain profile: a standalone Share previews section', () => {
  const api = load(null, PACKETS);
  assert.strictEqual(api.renderTerrainSettings(), '');
  const html = api.renderShareSection();
  assert.ok(/Share previews/.test(html), 'no section');
  const box = html.match(SWITCH);
  assert.ok(box && / checked/.test(box[0]) && /data-cv2-terrain-share/.test(box[0]), 'no wired switch, on by default');
  const t = (html.match(/<div class="cust-hint">(Pasted into a chat[^<]*)<\/div>/) || [])[1] || '';
  assert.ok(/a packet link/.test(t), 'the hint does not say packet links get a card');
  assert.ok(/View Path/.test(t), 'the hint does not name View Path');
  assert.ok(!/node/i.test(t) && !/profile/i.test(t), 'a card type the server does not offer: ' + t);
  assert.ok(/id="cv2-card-theme"[^>]*>Light</.test(html), 'card theme from share.card');
  assert.ok(/id="cv2-card-names"[^>]*>Hidden</.test(html), 'names from share.card');
  assert.strictEqual(html.split(PALETTE_LINE).length, 2);
});

test('the switch reflects a stored false', () => {
  const box = load(null, PACKETS, { terrain: { shareLinks: false } }).renderShareSection().match(SWITCH);
  assert.ok(box && !/ checked/.test(box[0]));
});

test('node and packet sharing: one switch whose hint names both', () => {
  const api = load(null, BOTH);
  const html = shown(api);
  assert.strictEqual(html.split('id="cv2-terrain-share"').length, 2, 'not exactly one switch');
  const t = text(html).replace('Node names on the card', '');
  assert.ok(/a node link \(repeaters, rooms and sensors\) or a packet link/.test(t), t);
  assert.ok(/the plain node or packet link/.test(t), t);
  assert.ok(!/profile/i.test(t), 'a profile card on a server without one');
});

test('packet sharing with the profile but no profile sharing: the switch in Terrain Profile', () => {
  const api = load(PROFILE_ONLY, PACKETS);
  const html = shown(api);
  assert.strictEqual(html.split('id="cv2-terrain-share"').length, 2, 'not exactly one switch');
  assert.ok(!/Link previews are off on this server/.test(html), 'the off note while packet previews are on');
  assert.ok(/a packet link/.test(text(api.renderTerrainSettings())));
  assert.strictEqual(api.renderShareSection(), '', 'a second section next to the Terrain Profile one');
});

test('all three card types: one switch, one palette line, the hint names all three', () => {
  const api = load(PROFILE_SHARE, BOTH);
  const html = shown(api);
  assert.strictEqual(html.split('id="cv2-terrain-share"').length, 2);
  assert.strictEqual(html.split(PALETTE_LINE).length, 2);
  const t = text(html);
  assert.ok(/a profile link, a node link \(repeaters, rooms and sensors\) or a packet link/.test(t), t);
  assert.ok(/the plain map, node or packet link/.test(t), t);
});

test('packet sharing off: #811 M4’s sections are unchanged', () => {
  [{ nodes: true, roles: NODES.roles, card: NODES.card, packets: false }].forEach((share) => {
    [null, PROFILE_ONLY, PROFILE_SHARE].forEach((dhm) => {
      const a = load(dhm, share), b = load(dhm, NODES);
      assert.strictEqual(shown(a), shown(b), JSON.stringify(dhm));
    });
  });
  [{ packets: false }, { packets: 'true' }, { packets: 1 }].forEach((share) => {
    assert.strictEqual(load(null, share).renderShareSection(), '', JSON.stringify(share));
  });
});

console.log('\n' + passed + ' passed, ' + failed + ' failed\n');
if (failed > 0) process.exit(1);
