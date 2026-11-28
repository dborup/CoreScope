/**
 * #1418 — map.js loadRouteFromDeepLink:
 *   - Hop resolution for the chosen observation.
 *   - GRP_TXT channel hash → name resolution (enc_ placeholder, SHA-256 byte
 *     match for keyed channels, fallback to "channel 0x<HEX>").
 *
 * The deep-link loader is a giant async function; we don't run it end-to-end.
 * Instead we verify:
 *   1. Source invariants for the hop resolution the loader actually performs.
 *   2. The REAL shared helper that resolves the chosen observation's hops
 *      (window.inferObservationPath), exercised on fixtures.
 *   3. Replica of the channel-match predicate (the same `find` callback).
 *   4. Live SubtleCrypto comparison: SHA-256(name)[0] === target byte
 *      reproduced via node's built-in crypto.
 *
 * #598 — the three-tier priority this file used to pin (server resolved_path >
 * HopResolver.resolve > raw prefixes) is gone. The loader now infers the chosen
 * observation's hops as ONE path, with that observation's resolved_path as a
 * hint rather than as an overriding tier, which is also what the packet detail
 * pane does — so the two agree. Section A and B below were a copy of the old
 * selection logic living in this test file; they now exercise the real helper.
 */
'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');
const crypto = require('crypto');

let passed = 0, failed = 0;
function assert(cond, msg) {
  if (cond) { passed++; console.log('  ✓ ' + msg); }
  else { failed++; console.error('  ✗ ' + msg); }
}

const mapSrc = fs.readFileSync(path.join(__dirname, 'public', 'map.js'), 'utf8');

console.log('\n=== #1418 hop-priority A: source invariants ===');

// The loader resolves the chosen observation's hops through the shared helper,
// not through a priority ladder of its own, and hands the entries to whichever
// renderer draws the route.
const deepLink = mapSrc.slice(mapSrc.indexOf('async function loadRouteFromDeepLink'));
assert(/inferObservationPath\(chosen, obsPos, originPos\)/.test(deepLink),
  'the loader infers the CHOSEN observation\'s hops with the shared helper');
assert(!/else if\s*\(\s*window\.HopResolver/.test(mapSrc),
  'the old three-tier priority ladder is gone');
assert(!/HopResolver\.resolve\(/.test(deepLink),
  'and the loader no longer resolves prefixes one at a time');
assert((deepLink.match(/inferredHops: inferredHops/g) || []).length === 2,
  'both route renderers receive the inferred entries, so neither re-resolves');
assert(/const chosenPath = inferredHops \? rawHops\.map\(\(h, i\) => inferredHops\[i\]\.pubkey \|\| h\) : rawHops/.test(deepLink),
  'an unlisted hop keeps its raw prefix in the canonical path, rather than borrowing a neighbour\'s pubkey');

console.log('\n=== #1418 hop-priority B: the real inferObservationPath() ===');

// The helper, loaded for real alongside the resolver it calls. Two repeaters
// share "AA" and two share "BB"; LOCAL-* sit on the observer, REMOTE-* about
// 1,200 km away, so path continuity has something to decide.
const LOCAL_AA = { public_key: 'aaee0002', name: 'LOCAL-AA', role: 'repeater', lat: 55, lon: 10 };
const REMOTE_AA = { public_key: 'aaff0001', name: 'REMOTE-AA', role: 'repeater', lat: 45, lon: 2 };
const LOCAL_BB = { public_key: 'bbee0002', name: 'LOCAL-BB', role: 'repeater', lat: 55, lon: 10 };
const REMOTE_BB = { public_key: 'bbff0001', name: 'REMOTE-BB', role: 'repeater', lat: 45, lon: 2 };
const HINTED = { public_key: 'cc99000a', name: 'SERVER-SAYS', role: 'room_server', lat: 55.05, lon: 10.05 };

function loadHelper() {
  const ctx = {
    window: {}, console, Math, Object, Array, Number, Date, Map, Set, JSON, String, Boolean,
    Float64Array, Int32Array, Infinity, parseInt, parseFloat, isNaN, encodeURIComponent,
    api: () => Promise.resolve({ edges: [] }),
  };
  vm.createContext(ctx);
  for (const f of ['hop-resolver.js', 'packet-helpers.js']) {
    vm.runInContext(fs.readFileSync(path.join(__dirname, 'public', f), 'utf8'), ctx, { filename: f });
  }
  ctx.window.HopResolver.init([LOCAL_AA, REMOTE_AA, LOCAL_BB, REMOTE_BB, HINTED],
    { observers: [{ id: 'TEST-RX', iata: 'TST' }], iataCoords: { TST: { lat: 55, lon: 10 } } });
  ctx.window.HopResolver.setAffinity({});
  return ctx.window;
}
// What the loader builds from the entries: a pubkey per position, or the raw
// prefix where the hop was left unlisted.
function chosenPathFor(W, obs, observer) {
  const rawHops = JSON.parse(obs.path_json);
  const entries = W.inferObservationPath(obs, observer || null, null);
  return rawHops.map((h, i) => entries[i].pubkey || h);
}

{
  const W = loadHelper();
  const observer = { lat: 55, lon: 10 };

  // The observation's own resolved_path is a hint: a plausible one is used.
  const hinted = chosenPathFor(W, {
    observer_id: 'TEST-RX', path_json: '["AA","BB"]',
    resolved_path: JSON.stringify([HINTED.public_key, null]),
  }, observer);
  assert(hinted[0] === HINTED.public_key,
    'a non-null resolved_path entry names that hop (got ' + hinted[0] + ')');
  assert(hinted[1] === LOCAL_BB.public_key,
    'and the hop the server left null is inferred, anchored on the observer (got ' + hinted[1] + ')');

  // A resolved_path of the wrong length describes another frame.
  const mismatched = chosenPathFor(W, {
    observer_id: 'TEST-RX', path_json: '["AA","BB"]',
    resolved_path: JSON.stringify([HINTED.public_key]),
  }, observer);
  assert(mismatched[0] === LOCAL_AA.public_key,
    'a resolved_path of the wrong length is ignored, not misaligned (got ' + mismatched[0] + ')');

  // No hint at all: both hops are inferred.
  const inferred = chosenPathFor(W, { observer_id: 'TEST-RX', path_json: '["AA","BB"]' }, observer);
  assert(JSON.stringify(inferred) === JSON.stringify([LOCAL_AA.public_key, LOCAL_BB.public_key]),
    'with no hint the whole path is inferred (got ' + JSON.stringify(inferred) + ')');

  // A repeated prefix is not one node twice: a node forwards a flood once.
  const repeated = chosenPathFor(W, { observer_id: 'TEST-RX', path_json: '["AA","BB","AA"]' }, observer);
  assert(new Set(repeated.filter(h => h.length > 2)).size === repeated.filter(h => h.length > 2).length,
    'no node is placed at two positions (got ' + JSON.stringify(repeated) + ')');
  assert(repeated[0] === 'AA',
    'the position no listed node plausibly fits keeps its raw prefix, so the renderer draws it as ' +
    'unlisted instead of borrowing REMOTE-AA 1,200 km away (got ' + repeated[0] + ')');

  // With no observer position the IATA centre stands in, so a deep link from
  // an observer without GPS still gets a path rather than bare prefixes.
  const viaIata = chosenPathFor(W, { observer_id: 'TEST-RX', path_json: '["AA","BB"]' }, null);
  assert(viaIata[1] === LOCAL_BB.public_key,
    'the observer\'s IATA centre anchors the last hop when it reports no position (got ' + viaIata[1] + ')');

  // An empty path infers nothing and resolves to nothing.
  assert(chosenPathFor(W, { observer_id: 'TEST-RX', path_json: '[]' }, observer).length === 0,
    'an empty path yields an empty canonical path');
}

console.log('\n=== #1418 channel A: GRP_TXT match predicate (sync part) ===');

// Replica of the channel-find predicate from loadRouteFromDeepLink.
function findChannelSync(chList, wantHex) {
  const wantUp = String(wantHex).toUpperCase();
  return chList.find(c => {
    const ch = String(c.hash || '').toUpperCase();
    const nm = String(c.name || '').toUpperCase();
    return ch.startsWith(wantUp) ||
           ch === 'ENC_' + wantUp ||
           nm.includes('0X' + wantUp);
  }) || null;
}

const channels = [
  { hash: 'public_full_hash_AB...', name: 'Public' },
  { hash: 'enc_77', name: 'Encrypted (0x77)', encrypted: true },
  { hash: 'unknown', name: 'channel 0xCD' }
];

// hash starts with target hex
let m = findChannelSync([{ hash: 'AB1234', name: 'Test' }], 'AB');
assert(m && m.name === 'Test', 'finds channel where hash starts with target hex');

// enc_<HEX> placeholder
m = findChannelSync(channels, '77');
assert(m && m.name === 'Encrypted (0x77)',
  'matches enc_<HEX> placeholder ("enc_77") for encrypted channel');

// name contains "0x<HEX>"
m = findChannelSync(channels, 'CD');
assert(m && m.name === 'channel 0xCD',
  'matches name containing "0x<HEX>" placeholder');

// Case-insensitive
m = findChannelSync([{ hash: 'enc_ff', name: 'lower' }], 'FF');
assert(m && m.name === 'lower', 'case-insensitive match on enc_<HEX>');

// No match → null (caller falls back to "channel 0x<HEX>")
m = findChannelSync(channels, 'XX');
assert(m === null, 'no match → null (so caller renders "channel 0x<HEX>" fallback)');

console.log('\n=== #1418 channel B: SHA-256(name)[0] keyed-channel match ===');

// The async fallback (SubtleCrypto) computes SHA-256(name)[0] and checks
// it against the target byte. Reproduce in node and verify the formula
// matches the firmware/decoder convention (first byte of SHA-256).
function sha256Byte0(name) {
  const buf = crypto.createHash('sha256').update(name, 'utf8').digest();
  return buf[0].toString(16).padStart(2, '0').toUpperCase();
}

// Known channel name → its derived byte
const wellKnown = ['Public', 'Test Channel', 'mesh-control', 'general'];
wellKnown.forEach(name => {
  const byte = sha256Byte0(name);
  assert(/^[0-9A-F]{2}$/.test(byte),
    'SHA-256("' + name + '")[0] = 0x' + byte + ' (valid 2-hex)');
});

// Construct a fixture where we deliberately want to match channel "Public"
const target = sha256Byte0('Public');
// Simulate the async match loop: walk the channel list, hash each name,
// return the one whose first byte === target.
function findChannelAsync(chList, wantHex) {
  const wantUp = String(wantHex).toUpperCase();
  for (const c of chList) {
    if (c.encrypted) continue;
    if (!c.name) continue;
    if (sha256Byte0(c.name) === wantUp) return c;
  }
  return null;
}

const result = findChannelAsync([
  { name: 'Public' },
  { name: 'Other' },
  { name: 'Public', encrypted: true } // would match but encrypted → skipped
], target);
assert(result && result.name === 'Public' && !result.encrypted,
  'SHA-256 match: returns first non-encrypted channel whose name SHA-256[0] === target byte');

// Source invariants: the async block exists in map.js
assert(/window\.crypto\.subtle/.test(mapSrc), 'map.js uses window.crypto.subtle for SHA-256 fallback');
assert(/'SHA-256'/.test(mapSrc), 'map.js requests SHA-256 specifically');
assert(/if\s*\(c\.encrypted\)\s*continue/.test(mapSrc),
  'async loop skips already-known encrypted/placeholder channels');
assert(/byteHex\s*===\s*wantUp/.test(mapSrc),
  'async loop compares first-byte hex to target (byteHex === wantUp)');

console.log('\n=== #1418 channel C: fallback label format ===');
// When no match found, caller renders "Encrypted (0x<HEX>)" for encrypted,
// "channel 0x<HEX>" otherwise. Just guard the literal templates exist.
assert(/Encrypted \(0x/.test(mapSrc),
  'encrypted-channel fallback label "Encrypted (0x..." present in map.js');

console.log('\n=== Summary ===');
console.log('  passed: ' + passed);
console.log('  failed: ' + failed);
if (failed > 0) process.exit(1);
