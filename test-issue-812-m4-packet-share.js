/**
 * Unit tests (#812 M4): the packet share link builder (public/node-share.js,
 * window.PacketShare, next to #811 M4's NodeShare and sharing its rules).
 *
 * Copy link on the packet detail page, the standalone #/packet/<id> page
 * and View Path hands out /p?packet=<hash> (View Path: &viewPath=1) only
 * when all of these hold, and the plain link of before otherwise:
 *   - the server publishes share.packets (share.packets.enabled is on);
 *   - the hash is exactly 16 hex characters (the only form /p?packet=
 *     accepts);
 *   - no observation is selected (/p cannot carry ?obs=, so the plain
 *     #/packets/<hash>?obs=<id> link keeps the selection);
 *   - the viewer has not turned "Copy link shares a preview" off
 *     (terrain.shareLinks, the one switch of #727 M4 / #811 M4).
 *
 * public/node-share.js runs in a vm; nothing is copied.
 *
 * Usage: node test-issue-812-m4-packet-share.js
 */
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

const SRC = fs.readFileSync(path.join(__dirname, 'public', 'node-share.js'), 'utf8');
const TP_SRC = ['antenna-height.js', 'terrain-profile.js'].map((f) => [f, fs.readFileSync(path.join(__dirname, 'public', f), 'utf8')]);
function load(win, opts) {
  const window = Object.assign({}, win || {});
  const ctx = vm.createContext({ window, URLSearchParams, encodeURIComponent, console });
  if (!(opts && opts.noTerrain)) TP_SRC.forEach(([f, src]) => vm.runInContext(src, ctx, { filename: f }));
  vm.runInContext(SRC, ctx, { filename: 'node-share.js' });
  return window;
}

const HASH = 'dec2833150e18744';
const LOC = { origin: 'https://mesh.example', pathname: '/' };
const ON = { nodes: false, packets: true, card: { theme: 'dark', names: true } };
const PLAIN = 'https://mesh.example/#/packets/' + HASH;
const PLAIN_VP = PLAIN + '?viewPath=1';
const SHARE = 'https://mesh.example/p?packet=' + HASH;
const SHARE_VP = SHARE + '&viewPath=1';

console.log('\n🔗 #812 M4 — packet share links\n');

test('the module exposes PacketShare next to NodeShare', () => {
  const w = load();
  assert.ok(w.PacketShare, 'no window.PacketShare');
  ['offered', 'url', 'copyURL'].forEach((f) => assert.strictEqual(typeof w.PacketShare[f], 'function', f));
  assert.ok(w.NodeShare, 'NodeShare is gone');
});

test('sharing on, a full hash: /p?packet=<hash>; View Path adds viewPath=1', () => {
  const PS = load().PacketShare;
  assert.strictEqual(PS.url(LOC, HASH, null, ON, true), SHARE);
  assert.strictEqual(PS.url(LOC, HASH, {}, ON, true), SHARE);
  assert.strictEqual(PS.url(LOC, HASH, { viewPath: true }, ON, true), SHARE_VP);
});

test('the /p link carries the hash lower-cased and only the allow-listed keys', () => {
  const PS = load().PacketShare;
  const u = PS.url(LOC, HASH.toUpperCase(), { viewPath: true }, ON, true);
  assert.strictEqual(u, SHARE_VP);
  assert.deepStrictEqual(Array.from(new URLSearchParams(u.split('?')[1]).keys()), ['packet', 'viewPath']);
});

test('a hash /p?packet= does not accept gets the plain link', () => {
  const PS = load().PacketShare;
  ['dec2833150e1874', 'dec2833150e187444', 'dec2833150e1874g', '', null, undefined].forEach((h) => {
    const u = PS.url(LOC, h, { id: 42 }, ON, true);
    assert.ok(u.indexOf('/p?') < 0, String(h) + ' → ' + u);
  });
  // Without a hash the packet's id is the link, as before.
  assert.strictEqual(PS.url(LOC, '', { id: 42 }, ON, true), 'https://mesh.example/#/packets/42');
});

test('a selected observation keeps the plain ?obs= link', () => {
  const PS = load().PacketShare;
  assert.strictEqual(PS.url(LOC, HASH, { obs: '7' }, ON, true), PLAIN + '?obs=7');
  assert.strictEqual(PS.url(LOC, '', { id: 42, obs: '7' }, ON, true), 'https://mesh.example/#/packets/42?obs=7');
  // An empty or absent selection is none.
  assert.strictEqual(PS.url(LOC, HASH, { obs: '' }, ON, true), SHARE);
  assert.strictEqual(PS.url(LOC, HASH, { obs: null }, ON, true), SHARE);
});

test('sharing off on the server: the plain link of before', () => {
  const PS = load().PacketShare;
  [null, undefined, {}, { nodes: true, roles: ['repeater'] }, { packets: false }, { packets: 'true' }, { packets: 1 }].forEach((cfg) => {
    assert.strictEqual(PS.url(LOC, HASH, null, cfg, true), PLAIN, JSON.stringify(cfg));
    assert.strictEqual(PS.url(LOC, HASH, { viewPath: true }, cfg, true), PLAIN_VP, JSON.stringify(cfg));
    assert.strictEqual(PS.offered(cfg), false, JSON.stringify(cfg));
  });
  assert.strictEqual(PS.offered(ON), true);
});

test('the viewer switch off: the plain link; only a strict true is on', () => {
  const PS = load().PacketShare;
  [false, undefined, null, 'true', 1].forEach((v) => {
    assert.strictEqual(PS.url(LOC, HASH, null, ON, v), PLAIN, String(v));
    assert.strictEqual(PS.url(LOC, HASH, { viewPath: true }, ON, v), PLAIN_VP, String(v));
  });
});

test('copyURL reads the config, the location and the switch at copy time', () => {
  const w = load({ location: LOC, MC_SHARE_CFG: ON, SITE_CONFIG: null });
  assert.strictEqual(w.PacketShare.copyURL(HASH), SHARE, 'default switch is on');
  assert.strictEqual(w.PacketShare.copyURL(HASH, { viewPath: true }), SHARE_VP);
  w.SITE_CONFIG = { terrain: { shareLinks: false } };
  assert.strictEqual(w.PacketShare.copyURL(HASH), PLAIN, 'switch off in the customizer');
  assert.strictEqual(w.PacketShare.copyURL(HASH, { viewPath: true }), PLAIN_VP);
  w.SITE_CONFIG = null;
  w.MC_SHARE_CFG = null;
  assert.strictEqual(w.PacketShare.copyURL(HASH), PLAIN, 'config gone');
});

test('without the terrain module there is no preview link (fail closed)', () => {
  const w = load({ location: LOC, MC_SHARE_CFG: ON }, { noTerrain: true });
  assert.strictEqual(w.PacketShare.copyURL(HASH), PLAIN);
});

test('node links are unaffected by packet sharing', () => {
  const NS = load().NodeShare;
  const PK = '3f2a91c0'.repeat(8);
  assert.strictEqual(NS.url(LOC, PK, 'repeater', ON, true), 'https://mesh.example/#/nodes/' + PK, 'packets on, nodes off');
});

console.log('\n' + passed + ' passed, ' + failed + ' failed\n');
if (failed > 0) process.exit(1);
