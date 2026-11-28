/**
 * #1418 — map.js loadRouteFromDeepLink raw_hex byte extraction.
 *
 * The deep-link loader peeks at chosen.raw_hex when decoded JSON is empty,
 * to extract src/destHash and (for GRP_TXT) channel_hash. Wire layout per
 * firmware/docs/packet_format.md:
 *   header, 4 transport-code bytes on routes 0/3, path_len, path bytes, then ...
 * The payload offset comes from app.js rawPayloadOffset() (#594).
 *
 *   TXT_MSG (type 2):  destHash + srcHash bytes after path
 *   RESPONSE (type 1): destHash + srcHash bytes after path
 *   ANON_REQ (type 7): destHash ONLY (no srcHash byte — sender anonymous)
 *   PATH (type 8):     destHash + srcHash bytes after path
 *   GRP_TXT (type 5):  channel_hash byte after path
 *
 * #608 follow-up (review of #600, finding N1): this used to assert the
 * extraction with source regexes and a hand-copied REPLICA of map.js's
 * slicing. A mutant on the real map.js (`const destOff = payOff * 2 + 2;`)
 * survived every suite. map.js now extracts the slicing into
 * extractSrcDstFromRawHex()/extractChannelHashFromRawHex(), exposed via
 * window.__meshcoreMapInternals (the same test-seam pattern used by the
 * Top Routes overlay and scope-filter tests) — this file drives the REAL
 * production functions through a vm sandbox, same as those.
 */
'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

let passed = 0, failed = 0;
function assert(cond, msg) {
  if (cond) { passed++; console.log('  ✓ ' + msg); }
  else { failed++; console.error('  ✗ ' + msg); }
}

const mapSrc = fs.readFileSync(path.join(__dirname, 'public', 'map.js'), 'utf8');

console.log('\n=== #1418 raw_hex A: source invariants in map.js ===');
assert(/TYPES_WITH_DST_SRC\s*=\s*\[\s*1\s*,\s*2\s*,\s*7\s*,\s*8\s*\]/.test(mapSrc),
  'TYPES_WITH_DST_SRC = [1, 2, 7, 8] (RESPONSE, TXT_MSG, ANON_REQ, PATH)');
assert(/payloadType\s*!==\s*7/.test(mapSrc),
  'ANON_REQ (type 7) special-cased to skip srcHash extraction');
assert(/PAYLOAD_TYPE_MAP\s*=\s*\{[^}]*0:\s*'REQ'[^}]*1:\s*'RESPONSE'[^}]*2:\s*'TXT_MSG'/m.test(mapSrc),
  'PAYLOAD_TYPE_MAP covers 0=REQ, 1=RESPONSE, 2=TXT_MSG');
assert(/5:\s*'GRP_TXT'[^}]*7:\s*'ANON_REQ'[^}]*8:\s*'PATH'/m.test(mapSrc),
  'PAYLOAD_TYPE_MAP covers 5=GRP_TXT, 7=ANON_REQ, 8=PATH');

// #594: the payload offset comes from app.js rawPayloadOffset() - path_len
// read behind the 4 transport-code bytes on routes 0/3, then hash_count x
// hash_size path bytes, capped by firmware Packet::isValidPathLen - in BOTH
// the TXT-family branch and the GRP_TXT channel-hash branch. The byte-1
// read it replaced was wrong on transport routes and multi-byte paths.
assert((mapSrc.match(/rawPayloadOffset\(hex\)/g) || []).length >= 2,
  'both raw_hex branches take the payload offset from rawPayloadOffset() (#594)');
assert(!/hex\.slice\(2,\s*4\)/.test(mapSrc),
  'no raw_hex branch reads path_len at byte 1 (#594)');

console.log('\n=== #1418 raw_hex B: real map.js extractors (vm.createContext, #608) ===');

// Minimal browser-like sandbox. map.js's top-level code (filters init,
// localStorage reads, the final window.__meshcoreMapInternals assignment)
// needs these; nothing here needs Leaflet since the extractors never touch
// map/marker/layer objects.
function makeSandbox() {
  const noop = () => {};
  const ctx = {
    window: { addEventListener: noop, removeEventListener: noop, dispatchEvent: noop },
    document: {
      readyState: 'complete', addEventListener: noop, removeEventListener: noop,
      getElementById: () => null, querySelector: () => null, querySelectorAll: () => [],
      createElement: () => ({ style: {}, classList: { add: noop, remove: noop, toggle: noop }, appendChild: noop, addEventListener: noop, setAttribute: noop }),
      head: { appendChild: noop }, body: { appendChild: noop },
    },
    console, location: { hash: '' },
    fetch: () => Promise.resolve({ json: () => Promise.resolve({}) }),
    setTimeout: noop, clearTimeout: noop, setInterval: () => 0, clearInterval: noop,
    localStorage: { getItem: () => null, setItem: noop, removeItem: noop },
    navigator: { userAgent: '' },
    getComputedStyle: () => ({ getPropertyValue: () => '' }),
  };
  vm.createContext(ctx);
  return ctx;
}

const ctx = makeSandbox();
// app.js first (defines rawPayloadOffset, a free variable map.js's
// extractors close over, exactly as index.html loads them in order).
for (const f of ['payload-labels.js', 'app.js']) {
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'public', f), 'utf8'), ctx, { filename: f });
}
vm.runInContext(mapSrc, ctx, { filename: 'map.js' });
const internals = ctx.window.__meshcoreMapInternals;
if (!internals || typeof internals.extractSrcDstFromRawHex !== 'function') {
  console.error('  ✗ window.__meshcoreMapInternals.extractSrcDstFromRawHex not found — map.js failed to load?');
  process.exit(1);
}
const extractSrcDst = internals.extractSrcDstFromRawHex;
const extractChannelHash = internals.extractChannelHashFromRawHex;

// Build a frame: header (payload type in bits 2-5, route in bits 0-1), the
// 4 transport-code bytes on routes 0/3, path_len, path bytes, payload.
// firmware/docs/packet_format.md.
function build(payloadType, route, pathLen, pathBytes, payloadBytes) {
  const hex2 = (n) => n.toString(16).padStart(2, '0').toUpperCase();
  const codes = route === 0 || route === 3 ? 'C0DE0000' : '';
  return hex2(payloadType << 2 | route) + codes + hex2(pathLen) + pathBytes.join('') + payloadBytes.join('');
}

// Fixture 1: TXT_MSG (type 2), flood, 2 path hops AB,CD, destHash=42, srcHash=99
let r = extractSrcDst(build(2, 1, 0x02, ['AB', 'CD'], ['42', '99', 'FF', 'EE']), 2);
assert(r.dst === '42' && r.src === '99',
  'TXT_MSG (type 2) extracts destHash=42, srcHash=99 after 2-hop path (got dst=' + r.dst + ', src=' + r.src + ')');

// Fixture 2: RESPONSE (type 1), direct, 0-hop path
r = extractSrcDst(build(1, 2, 0x00, [], ['7A', '3C']), 1);
assert(r.dst === '7A' && r.src === '3C',
  'RESPONSE (type 1) extracts destHash + srcHash on 0-hop path (got dst=' + r.dst + ', src=' + r.src + ')');

// Fixture 3: ANON_REQ (type 7) — destHash present, srcHash MUST be null
r = extractSrcDst(build(7, 1, 0x01, ['11'], ['DD', 'BB', 'CC']), 7);
assert(r.dst === 'DD', 'ANON_REQ (type 7) extracts destHash=DD');
assert(r.src === null, 'ANON_REQ (type 7) MUST NOT extract srcHash (anonymous sender) — got ' + r.src);

// Fixture 4: PATH (type 8) carries both hashes
r = extractSrcDst(build(8, 1, 0x03, ['AA', 'BB', 'CC'], ['11', '22']), 8);
assert(r.dst === '11' && r.src === '22',
  'PATH (type 8) extracts destHash + srcHash after 3-hop path (got dst=' + r.dst + ', src=' + r.src + ')');

// Fixture 5: GRP_TXT (type 5) — channel_hash extraction, NOT src/dst
const grp = build(5, 1, 0x01, ['77'], ['AB', 'EE']);
assert(extractChannelHash(grp, 5) === 'AB', 'GRP_TXT (type 5) extracts channel_hash=AB after 1-hop path');
r = extractSrcDst(grp, 5);
assert(r.src === null && r.dst === null,
  'GRP_TXT (type 5) is NOT in TYPES_WITH_DST_SRC — extractor returns nulls');

// Fixture 6: non-extracting types (REQ=0, ACK=3, ADVERT=4, MULTIPART=10, …)
[0, 3, 4, 6, 9, 10, 11, 12].forEach(function (pt) {
  r = extractSrcDst(build(pt, 1, 0x00, [], ['FF', 'FF']), pt);
  assert(r.src === null && r.dst === null,
    'payload_type=' + pt + ' (not in TYPES_WITH_DST_SRC) → no extraction');
});

// #594: one golden frame per route type. The transport-code bytes (C0DE0000)
// sit where the byte-1 read used to find path_len.
[[0, 'TRANSPORT_FLOOD'], [1, 'FLOOD'], [2, 'DIRECT'], [3, 'TRANSPORT_DIRECT']].forEach(function (rt) {
  r = extractSrcDst(build(2, rt[0], 0x01, ['AB'], ['42', '99', 'FF']), 2);
  assert(r.dst === '42' && r.src === '99', rt[1] + ' (route ' + rt[0] + ') TXT_MSG → dst=42, src=99 (got dst=' + r.dst + ', src=' + r.src + ')');
  const c = extractChannelHash(build(5, rt[0], 0x01, ['77'], ['AB', 'EE']), 5);
  assert(c === 'AB', rt[1] + ' (route ' + rt[0] + ') GRP_TXT → channel_hash=AB (got ' + c + ')');
});
// #594: multi-byte hops - path_len 0x42 is two 2-byte hops (4 path bytes),
// not 66 1-byte hops.
r = extractSrcDst(build(2, 1, 0x42, ['AAAA', 'BBBB'], ['42', '99', 'FF']), 2);
assert(r.dst === '42' && r.src === '99', '2-byte hops: path_len 0x42 skips 4 path bytes (got dst=' + r.dst + ', src=' + r.src + ')');

// Edge case: raw_hex too short (path length claims more bytes than present)
r = extractSrcDst(build(2, 1, 0x04, ['AB'], []), 2);
assert(r.src === null && r.dst === null, 'truncated raw_hex → null extraction (no crash)');

// Polish review (djb): a crafted path_len MUST be rejected even when the body
// is long enough to slice, or random body bytes surface as src/destHash.
// Firmware Packet::isValidPathLen: hash size 4 (0xC0 bits) is reserved and the
// path holds at most MAX_PATH_SIZE (64) bytes.
r = extractSrcDst('0A' + 'C8' + 'AB'.repeat(500), 2);
assert(r.src === null && r.dst === null, 'reserved hash size (path_len 0xC8) → rejected, no OOB-style byte surfacing');
assert(extractChannelHash('15' + 'C8' + 'AB'.repeat(500), 5) === null, 'reserved hash size (GRP_TXT) → rejected');
// Boundary: 64 path bytes (0x60 = 32 x 2-byte hops) still works; 66 rejected.
r = extractSrcDst(build(2, 1, 0x60, ['AB'.repeat(64)], ['EE', 'FF']), 2);
assert(r.dst === 'EE' && r.src === 'FF', '64 path bytes (MAX_PATH_SIZE) still extracts');
r = extractSrcDst(build(2, 1, 0x61, ['AB'.repeat(66)], ['EE', 'FF']), 2);
assert(r.src === null && r.dst === null, '66 path bytes → rejected (above MAX_PATH_SIZE 64)');

// #608: a mutant on the real map.js slicing (`destOff = payOff * 2 + 2`,
// i.e. off by one path byte) must be caught. With it, the TXT_MSG fixture
// above would read 'FF' as dst instead of '42'. Pin the exact byte math so
// that mutant fails this assertion rather than only the replica's.
r = extractSrcDst(build(2, 1, 0x01, ['AB'], ['42', '99', 'FF']), 2);
assert(r.dst === '42' && r.src === '99',
  'destOff = payOff * 2 (not +2): dst/src read right at the payload start, no off-by-one (got dst=' + r.dst + ', src=' + r.src + ')');

console.log('\n=== #1418 raw_hex C: channel_hash NOT extracted for non-GRP_TXT ===');
[0, 1, 2, 3, 4, 6, 7, 8, 9, 10, 11, 12].forEach(function (pt) {
  const v = extractChannelHash(build(5, 1, 0x00, [], ['AB']), pt);
  assert(v === null, 'payload_type=' + pt + ' returns null channel_hash');
});

console.log('\n=== Summary ===');
console.log('  passed: ' + passed);
console.log('  failed: ' + failed);
process.exit(failed > 0 ? 1 : 0);
