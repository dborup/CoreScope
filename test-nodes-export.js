'use strict';
// Unit test for nodes-export.js — the MeshCore companion "contacts" JSON
// export of the visible node list. Loads the browser IIFE in a vm sandbox
// (pattern from test-node-reach-coverage.js) and exercises the pure
// mapping/validation helpers directly.
//
// Ported from upstream Kpa-clawbot/CoreScope#1889 and rewritten against
// dborup/CoreScope's decoded-ADVERT role vocabulary (cmd/ingestor/decoder.go
// advertRole(): none/companion/repeater/room/sensor/type-N for N in 5..15,
// see also issue #1888's design report) plus review-fund tightening of the
// role/pubkey/coordinate validation rules (#1889 review).
const assert = require('assert');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const code = fs.readFileSync(path.join(__dirname, 'public', 'nodes-export.js'), 'utf8');
const sandbox = { window: {}, document: {} };
vm.createContext(sandbox);
vm.runInContext(code, sandbox);

const { buildContacts, filename, roleToType, validPubkey, validCoord } = sandbox.window.NodesExport;

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}

// ─── Field mapping ───────────────────────────────────────────────────────────
console.log('\n=== Field mapping ===');
// The companion app reads a fixed key set; emitting extra/renamed keys makes the
// import silently drop contacts, so both the keys AND their order are asserted.
const REF_KEYS = ['type', 'name', 'custom_name', 'public_key', 'flags', 'latitude',
  'longitude', 'last_advert', 'last_modified', 'out_path_list'];

const PK = 'efef7943505052b47f1809488ea4b4d3942d4ed72d2b1953b90a9f5e62a65fb5';
const repeater = {
  public_key: PK, name: 'BE-BRE-ON8AR🔋', role: 'repeater',
  lat: 51.137798, lon: 5.590199, last_seen: '2026-05-14T09:25:43Z',
};

test('top level is a single contacts array', () => {
  const out = buildContacts([repeater]);
  assert.deepStrictEqual(Object.keys(out), ['contacts']);
});
test('exactly one contact for one valid node', () => {
  assert.strictEqual(buildContacts([repeater]).contacts.length, 1);
});
const c = buildContacts([repeater]).contacts[0];
test('contact keys match the companion format, in order', () => {
  assert.deepStrictEqual(Object.keys(c), REF_KEYS);
});
test('repeater → type 2', () => { assert.strictEqual(c.type, 2); });
test('name passes through unchanged, emoji included', () => { assert.strictEqual(c.name, 'BE-BRE-ON8AR🔋'); });
test('custom_name is always null', () => { assert.strictEqual(c.custom_name, null); });
test('public_key passes through unchanged', () => { assert.strictEqual(c.public_key, PK); });
test('flags is always 0', () => { assert.strictEqual(c.flags, 0); });
test('latitude is a string', () => { assert.strictEqual(c.latitude, '51.137798'); });
test('longitude is a string', () => { assert.strictEqual(c.longitude, '5.590199'); });
test('last_seen → unix seconds for last_advert', () => { assert.strictEqual(c.last_advert, 1778750743); });
test('last_modified mirrors last_advert', () => { assert.strictEqual(c.last_modified, 1778750743); });
test('out_path_list is always null', () => { assert.strictEqual(c.out_path_list, null); });

// ─── Review fix 2: role → type, full 0-15 vocabulary + skip rules ───────────
console.log('\n=== Role → type mapping (review fix 2) ===');
function typeOf(role) {
  return buildContacts([Object.assign({}, repeater, { role: role })]).contacts.length
    ? buildContacts([Object.assign({}, repeater, { role: role })]).contacts[0].type
    : 'SKIPPED';
}
test('roleToType: none → 0', () => assert.strictEqual(roleToType('none'), 0));
test('roleToType: companion → 1', () => assert.strictEqual(roleToType('companion'), 1));
test('roleToType: repeater → 2', () => assert.strictEqual(roleToType('repeater'), 2));
test('roleToType: room → 3', () => assert.strictEqual(roleToType('room'), 3));
test('roleToType: sensor → 4', () => assert.strictEqual(roleToType('sensor'), 4));
for (let n = 5; n <= 15; n++) {
  test('roleToType: type-' + n + ' → ' + n, () => assert.strictEqual(roleToType('type-' + n), n));
}
test('role match is case-insensitive (Repeater)', () => assert.strictEqual(roleToType('Repeater'), 2));
test('role match is case-insensitive (TYPE-7)', () => assert.strictEqual(roleToType('TYPE-7'), 7));
test('end-to-end: repeater/companion/room/sensor via buildContacts', () => {
  assert.strictEqual(typeOf('repeater'), 2);
  assert.strictEqual(typeOf('companion'), 1);
  assert.strictEqual(typeOf('room'), 3);
  assert.strictEqual(typeOf('sensor'), 4);
});

console.log('\n=== Role skip rules — unknown/malformed roles are SKIPPED, never defaulted to companion (review fix 2) ===');
test('roleToType: unknown role "observer" → null (skip)', () => assert.strictEqual(roleToType('observer'), null));
test('roleToType: out-of-range "type-16" → null (skip)', () => assert.strictEqual(roleToType('type-16'), null));
test('roleToType: out-of-range "type-4" → null (4 is sensor by name, not a reserved type-N)', () => assert.strictEqual(roleToType('type-4'), null));
test('roleToType: out-of-range "type-0" → null', () => assert.strictEqual(roleToType('type-0'), null));
test('roleToType: malformed "type-abc" → null (skip)', () => assert.strictEqual(roleToType('type-abc'), null));
test('roleToType: malformed "type-" (no digits) → null (skip)', () => assert.strictEqual(roleToType('type-'), null));
test('roleToType: malformed "type-5x" (trailing garbage) → null (skip)', () => assert.strictEqual(roleToType('type-5x'), null));
test('roleToType: empty role "" → null (skip)', () => assert.strictEqual(roleToType(''), null));
test('roleToType: whitespace-only role → null (skip)', () => assert.strictEqual(roleToType('   '), null));
test('roleToType: missing role (undefined) → null (skip)', () => assert.strictEqual(roleToType(undefined), null));
test('roleToType: missing role (null) → null (skip)', () => assert.strictEqual(roleToType(null), null));
test('end-to-end: node with role "observer" is skipped entirely, not exported as companion', () => {
  assert.strictEqual(typeOf('observer'), 'SKIPPED');
});
test('end-to-end: node with missing role is skipped entirely', () => {
  assert.strictEqual(typeOf(undefined), 'SKIPPED');
});
test('end-to-end: node with empty role is skipped entirely', () => {
  assert.strictEqual(typeOf(''), 'SKIPPED');
});

// ─── Review fix 3: strict public_key validation ─────────────────────────────
console.log('\n=== Public key validation (review fix 3) ===');
test('validPubkey: exactly 64 lowercase hex chars → true', () => assert.strictEqual(validPubkey(PK), true));
const PK_UPPER = PK.toUpperCase();
const PK_MIXED = 'Ef' + PK.slice(2);
test('validPubkey: exactly 64 uppercase hex chars → true', () => assert.strictEqual(validPubkey(PK_UPPER), true));
test('validPubkey: mixed-case hex → true', () => assert.strictEqual(validPubkey(PK_MIXED), true));
test('validPubkey: 63 chars (too short) → false', () => assert.strictEqual(validPubkey(PK.slice(0, 63)), false));
test('validPubkey: 65 chars (too long) → false', () => assert.strictEqual(validPubkey(PK + 'a'), false));
test('validPubkey: 64 chars with one non-hex char → false', () => assert.strictEqual(validPubkey(PK.slice(0, 63) + 'g'), false));
test('validPubkey: 64 chars with an embedded space → false', () => assert.strictEqual(validPubkey(PK.slice(0, 32) + ' ' + PK.slice(33)), false));
test('validPubkey: leading/trailing whitespace around a valid key → false', () => assert.strictEqual(validPubkey(' ' + PK + ' '), false));
test('validPubkey: empty string → false', () => assert.strictEqual(validPubkey(''), false));
test('validPubkey: missing (undefined) → false', () => assert.strictEqual(validPubkey(undefined), false));
test('validPubkey: null → false', () => assert.strictEqual(validPubkey(null), false));
test('validPubkey: non-string (number) → false', () => assert.strictEqual(validPubkey(1234), false));
test('case is preserved on export (uppercase key stays uppercase)', () => {
  const out = buildContacts([Object.assign({}, repeater, { public_key: PK_UPPER })]).contacts[0];
  assert.strictEqual(out.public_key, PK_UPPER);
});
test('case is preserved on export (mixed-case key stays mixed-case)', () => {
  const out = buildContacts([Object.assign({}, repeater, { public_key: PK_MIXED })]).contacts[0];
  assert.strictEqual(out.public_key, PK_MIXED);
});

// ─── Review fix 4: strict coordinate validation ─────────────────────────────
console.log('\n=== Coordinate validation (review fix 4) ===');
test('validCoord: NaN → null', () => assert.strictEqual(validCoord(NaN, -90, 90), null));
test('validCoord: Infinity → null', () => assert.strictEqual(validCoord(Infinity, -90, 90), null));
test('validCoord: -Infinity → null', () => assert.strictEqual(validCoord(-Infinity, -90, 90), null));
test('validCoord: string "Infinity" → null', () => assert.strictEqual(validCoord('Infinity', -90, 90), null));
test('validCoord: empty string → null', () => assert.strictEqual(validCoord('', -90, 90), null));
test('validCoord: whitespace-only string → null', () => assert.strictEqual(validCoord('   ', -90, 90), null));
test('validCoord: null → null', () => assert.strictEqual(validCoord(null, -90, 90), null));
test('validCoord: undefined → null', () => assert.strictEqual(validCoord(undefined, -90, 90), null));
test('validCoord: non-numeric string → null', () => assert.strictEqual(validCoord('n/a', -90, 90), null));
test('validCoord: out of range above max → null', () => assert.strictEqual(validCoord(91, -90, 90), null));
test('validCoord: out of range below min → null', () => assert.strictEqual(validCoord(-181, -180, 180), null));
test('validCoord: exactly at max boundary → kept', () => assert.strictEqual(validCoord(90, -90, 90), 90));
test('validCoord: exactly at min boundary → kept', () => assert.strictEqual(validCoord(-180, -180, 180), -180));
test('validCoord: legitimate 0 → kept (not treated as empty)', () => assert.strictEqual(validCoord(0, -90, 90), 0));
test('validCoord: numeric string is parsed', () => assert.strictEqual(validCoord('51.5', -90, 90), 51.5));

function kept(patch) {
  return buildContacts([Object.assign({}, repeater, patch)]).contacts.length;
}
test('lat NaN → skipped', () => assert.strictEqual(kept({ lat: NaN }), 0));
test('lon Infinity → skipped', () => assert.strictEqual(kept({ lon: Infinity }), 0));
test('lat -Infinity → skipped', () => assert.strictEqual(kept({ lat: -Infinity }), 0));
test('lat out of range (91) → skipped', () => assert.strictEqual(kept({ lat: 91 }), 0));
test('lat out of range (-91) → skipped', () => assert.strictEqual(kept({ lat: -91 }), 0));
test('lon out of range (181) → skipped', () => assert.strictEqual(kept({ lon: 181 }), 0));
test('lon out of range (-181) → skipped', () => assert.strictEqual(kept({ lon: -181 }), 0));
test('no name → skipped', () => assert.strictEqual(kept({ name: null }), 0));
test('blank name → skipped', () => assert.strictEqual(kept({ name: '   ' }), 0));
test('short pubkey → skipped', () => assert.strictEqual(kept({ public_key: 'efef7943' }), 0));
test('missing lat → skipped', () => assert.strictEqual(kept({ lat: null }), 0));
test('missing lon → skipped', () => assert.strictEqual(kept({ lon: undefined }), 0));
test('null island (0,0) → skipped', () => assert.strictEqual(kept({ lat: 0, lon: 0 }), 0));
test('null island as strings ("0","0") → skipped', () => assert.strictEqual(kept({ lat: '0', lon: '0' }), 0));
test('lat 0 with a real lon is a valid position (equator)', () => assert.strictEqual(kept({ lat: 0, lon: 5.59 }), 1));
test('lon 0 with a real lat is a valid position (prime meridian)', () => assert.strictEqual(kept({ lat: 51.5, lon: 0 }), 1));
test('boundary lat=90/lon=180 is valid', () => assert.strictEqual(kept({ lat: 90, lon: 180 }), 1));
test('boundary lat=-90/lon=-180 is valid', () => assert.strictEqual(kept({ lat: -90, lon: -180 }), 1));
test('non-numeric lat → skipped', () => assert.strictEqual(kept({ lat: 'n/a' }), 0));
test('a node without last_seen is still exported', () => assert.strictEqual(kept({ last_seen: null }), 1));
test('missing last_seen → last_advert 0', () => {
  assert.strictEqual(
    buildContacts([Object.assign({}, repeater, { last_seen: null })]).contacts[0].last_advert, 0);
});
test('empty input → empty contacts', () => assert.strictEqual(buildContacts([]).contacts.length, 0));
test('null input → empty contacts', () => assert.strictEqual(buildContacts(null).contacts.length, 0));

// ─── Order is NOT computed here — buildContacts trusts caller-supplied order ─
console.log('\n=== buildContacts never reorders its input (ordering is the caller\'s job, #1889 review fix 1) ===');
const two = buildContacts([
  Object.assign({}, repeater, { name: 'second', public_key: PK.replace(/^efef/, 'aaaa') }),
  Object.assign({}, repeater, { name: 'first' }),
]);
// Joined, not deepStrictEqual: arrays built inside the vm sandbox have a
// different Array prototype and would fail the realm check.
test('input order is preserved exactly, even when it looks "unsorted"', () => {
  assert.strictEqual(two.contacts.map(x => x.name).join(','), 'second,first');
});

// ─── Filename ────────────────────────────────────────────────────────────────
console.log('\n=== Filename ===');
const d = new Date(2026, 7, 12, 16, 5, 17); // local time, 2026-08-12 16:05:17
test('area + date → stamped filename', () => assert.strictEqual(filename('BE-LIM', d), 'corescope_nodes_BE-LIM_2026-08-12-160517.json'));
test('no area selected → "all"', () => assert.strictEqual(filename(null, d), 'corescope_nodes_all_2026-08-12-160517.json'));
test('empty area string → "all"', () => assert.strictEqual(filename('', d), 'corescope_nodes_all_2026-08-12-160517.json'));
test('unsafe filename chars are collapsed to underscores', () => assert.strictEqual(filename('NL / Zuid', d), 'corescope_nodes_NL_Zuid_2026-08-12-160517.json'));

console.log('\n' + passed + ' passed, ' + failed + ' failed');
process.exit(failed ? 1 : 0);
