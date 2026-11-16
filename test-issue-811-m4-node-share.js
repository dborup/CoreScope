/**
 * Unit tests (#811 M4): the node share link builder (public/node-share.js).
 *
 * Copy URL on the node page and in the side pane hands out /p?node=<pk>
 * only when all of these hold, and the plain #/nodes/<pk> link otherwise:
 *   - the server publishes share.nodes (share.nodes.enabled is on);
 *   - the node's role is one of share.roles (the server's card roles);
 *   - the key is a full 64-hex key (the only form /p?node= accepts);
 *   - the viewer has not turned "Copy link shares a preview" off
 *     (terrain.shareLinks, the #727 M4 switch, now for both card types).
 *
 * public/node-share.js runs in a vm; nothing is copied.
 *
 * Usage: node test-issue-811-m4-node-share.js
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
// viewerOn reuses TerrainProfile.themeDefaults (one copy of the switch's
// rule), so the real terrain-profile.js is loaded first, as in index.html.
const TP_SRC = ['antenna-height.js', 'terrain-profile.js'].map((f) => [f, fs.readFileSync(path.join(__dirname, 'public', f), 'utf8')]);
function load(win, opts) {
  const window = Object.assign({}, win || {});
  const ctx = vm.createContext({ window, URLSearchParams, encodeURIComponent, console });
  if (!(opts && opts.noTerrain)) TP_SRC.forEach(([f, src]) => vm.runInContext(src, ctx, { filename: f }));
  vm.runInContext(SRC, ctx, { filename: 'node-share.js' });
  return window;
}

const PK = '3f2a91c0'.repeat(8);
const LOC = { origin: 'https://mesh.example', pathname: '/' };
const ON = { nodes: true, roles: ['repeater', 'room', 'sensor'], card: { theme: 'dark', names: true } };
const HASH = 'https://mesh.example/#/nodes/' + PK;
const SHARE = 'https://mesh.example/p?node=' + PK;

console.log('\n🔗 #811 M4 — node share links\n');

test('the module exposes NodeShare with the builder and its gates', () => {
  const w = load();
  assert.ok(w.NodeShare, 'no window.NodeShare');
  ['offered', 'carded', 'viewerOn', 'url', 'copyURL'].forEach((f) => assert.strictEqual(typeof w.NodeShare[f], 'function', f));
});

test('sharing on, a carded role and a full key: the /p?node= link', () => {
  const NS = load().NodeShare;
  ['repeater', 'room', 'sensor'].forEach((role) => {
    assert.strictEqual(NS.url(LOC, PK, role, ON, true), SHARE, role);
  });
});

test('the /p link carries the key lower-cased and nothing else', () => {
  const NS = load().NodeShare;
  const u = NS.url(LOC, PK.toUpperCase(), 'repeater', ON, true);
  assert.strictEqual(u, SHARE);
  const q = new URLSearchParams(u.split('?')[1]);
  assert.deepStrictEqual(Array.from(q.keys()), ['node']);
});

test('a companion (no card) gets the plain hash link', () => {
  const NS = load().NodeShare;
  assert.strictEqual(NS.url(LOC, PK, 'companion', ON, true), HASH);
  assert.strictEqual(NS.url(LOC, PK, '', ON, true), HASH);
  assert.strictEqual(NS.url(LOC, PK, undefined, ON, true), HASH);
});

test('the roles are the server’s list, not a copy in the browser', () => {
  const NS = load().NodeShare;
  const onlyRooms = Object.assign({}, ON, { roles: ['room'] });
  assert.strictEqual(NS.url(LOC, PK, 'repeater', onlyRooms, true), HASH);
  assert.strictEqual(NS.url(LOC, PK, 'room', onlyRooms, true), SHARE);
  assert.strictEqual(NS.url(LOC, PK, 'repeater', { nodes: true }, true), HASH, 'no roles list is no role');
  assert.strictEqual(NS.url(LOC, PK, 'repeater', { nodes: true, roles: 'repeater' }, true), HASH, 'a string is no list');
});

test('sharing off on the server (no share block, or nodes not true): the hash link', () => {
  const NS = load().NodeShare;
  [null, undefined, {}, { nodes: false, roles: ON.roles }, { nodes: 'true', roles: ON.roles }, { nodes: 1, roles: ON.roles }].forEach((cfg) => {
    assert.strictEqual(NS.url(LOC, PK, 'repeater', cfg, true), HASH, JSON.stringify(cfg));
  });
});

test('the viewer switch off: the hash link', () => {
  const NS = load().NodeShare;
  assert.strictEqual(NS.url(LOC, PK, 'repeater', ON, false), HASH);
});

test('a key that is not a full 64-hex key never becomes a /p link', () => {
  const NS = load().NodeShare;
  [PK.slice(0, 8), PK.slice(0, 63), PK + 'a', 'g'.repeat(64), '', PK + '#x'].forEach((k) => {
    const u = NS.url(LOC, k, 'repeater', ON, true);
    assert.ok(u.indexOf('/p?') === -1, 'shared ' + JSON.stringify(k));
    assert.strictEqual(u, 'https://mesh.example/#/nodes/' + encodeURIComponent(k));
  });
});

test('viewerOn: on unless the customizer stored terrain.shareLinks false', () => {
  const NS = load().NodeShare;
  assert.strictEqual(NS.viewerOn(undefined), true);
  assert.strictEqual(NS.viewerOn({}), true);
  assert.strictEqual(NS.viewerOn({ terrain: {} }), true);
  assert.strictEqual(NS.viewerOn({ terrain: { shareLinks: true } }), true);
  assert.strictEqual(NS.viewerOn({ terrain: { shareLinks: false } }), false);
  ['false', 0, '', null].forEach((v) => assert.strictEqual(NS.viewerOn({ terrain: { shareLinks: v } }), true, JSON.stringify(v)));
});

test('viewerOn is the Path profile switch rule, and fails closed without it', () => {
  const w = load();
  [undefined, {}, { terrain: null }, { terrain: { shareLinks: false } }, { terrain: { shareLinks: 'false' } }].forEach((c) => {
    const want = w.TerrainProfile.themeDefaults((c && c.terrain) || null).shareLinks;
    assert.strictEqual(w.NodeShare.viewerOn(c), want, JSON.stringify(c));
  });
  const bare = load({}, { noTerrain: true }).NodeShare;
  assert.strictEqual(bare.viewerOn({}), false, 'no TerrainProfile: plain link');
});

test('copyURL reads the server block and the switch at copy time', () => {
  const w = load({ location: LOC });
  const n = { public_key: PK, role: 'repeater' };
  assert.strictEqual(w.NodeShare.copyURL(n), HASH, 'before /api/config/client lands');
  w.MC_SHARE_CFG = ON;
  assert.strictEqual(w.NodeShare.copyURL(n), SHARE);
  w.SITE_CONFIG = { terrain: { shareLinks: false } };
  assert.strictEqual(w.NodeShare.copyURL(n), HASH, 'the switch is read at copy time');
  w.SITE_CONFIG = { terrain: {} };
  assert.strictEqual(w.NodeShare.copyURL({ public_key: PK, role: 'companion' }), HASH);
});

console.log('\n' + passed + ' passed, ' + failed + ' failed\n');
if (failed > 0) process.exit(1);
