/* test-live-multibyte-filter.js
 * feat(live): packet-level multibyte classifier used by the "Multibyte only"
 * live filter. Pure function — no DOM. Mirrors app.js hash-size math:
 *   hashSize = (pathByte >> 6) + 1, pathByte at offset 5 (transport) else 1.
 *
 * #608 follow-up (review of #600, finding N2 / issue #608): packetHashSize()
 * used to take an externally-supplied routeType and trust it for the offset.
 * That byte can belong to a DIFFERENT observation than the raw_hex passed
 * alongside it (cmd/server/store.go StoreTx.routeMask: one content hash can
 * be heard on several routes). It now derives the route from rawHex's own
 * header byte and takes no second argument.
 */
'use strict';
const fs = require('fs');
const path = require('path');
const vm = require('vm');
const assert = require('assert');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

// Load hop-filter.js into a sandbox exposing module.exports.
function load() {
  const ctx = {
    window: { addEventListener() {}, dispatchEvent() {} },
    localStorage: { getItem: () => null, setItem() {}, removeItem() {} },
    console, Math, Number, String, JSON, parseInt, isNaN,
    module: { exports: {} }, exports: {},
  };
  ctx.window.localStorage = ctx.localStorage;
  vm.createContext(ctx);
  const src = fs.readFileSync(path.join(__dirname, 'public/hop-filter.js'), 'utf8');
  vm.runInContext(src, ctx, { filename: 'hop-filter.js' });
  return ctx.module.exports.packetHashSize;
}

const packetHashSize = load();

console.log('\n=== packetHashSize (multibyte classifier) ===');

test('non-transport single-byte: byte1 top bits 00 -> size 1', () => {
  // header=0x01 (payload_type 0, route 1=FLOOD, offset 1); byte1=0x00
  assert.strictEqual(packetHashSize('0100'), 1);
});

test('non-transport multibyte: byte1 0x40 -> size 2', () => {
  // byte1=0x40 = 0b01000000 -> (0x40>>6)+1 = 2
  assert.strictEqual(packetHashSize('0140'), 2);
});

test('non-transport 3-byte: byte1 0x80 -> size 3', () => {
  // byte1=0x80 = 0b10000000 -> (0x80>>6)+1 = 3
  assert.strictEqual(packetHashSize('0180'), 3);
});

test('transport route (header route bits 00) uses offset 5', () => {
  // header=0x00 (route 0 = TRANSPORT_FLOOD): 00 | 4 code bytes | path-len 0x40 | ...
  //                                           0    1  2  3  4     5
  assert.strictEqual(packetHashSize('00aabbccdd40'), 2);
});

test('transport route (header route bits 11) also uses offset 5', () => {
  // header=0x03 (route 3 = TRANSPORT_DIRECT)
  assert.strictEqual(packetHashSize('03aabbccdd00'), 1);
});

test('missing raw_hex -> 0 (unresolvable)', () => {
  assert.strictEqual(packetHashSize(''), 0);
  assert.strictEqual(packetHashSize(null), 0);
  assert.strictEqual(packetHashSize(undefined), 0);
});

test('too short for offset -> 0', () => {
  // header=0x01 (non-transport, needs >= 2 bytes); only 1 byte present
  assert.strictEqual(packetHashSize('01'), 0);
  // header=0x00 (transport, needs >= 6 bytes); only 5 present
  assert.strictEqual(packetHashSize('00aabbccdd'), 0);
});

test('garbage hex byte -> 0', () => {
  // header=0x01 (non-transport, offset 1); byte1='zz' is not hex
  assert.strictEqual(packetHashSize('01zz'), 0);
});

test('whitespace in raw_hex is tolerated', () => {
  assert.strictEqual(packetHashSize('01 40'), 2);
});

console.log('\n=== packetHashSize derives route from its OWN header, not a caller\'s field (#608) ===');

test('route is read from THIS frame\'s header byte, independent of any external route label', () => {
  // header=0x00 -> route 0 (TRANSPORT_FLOOD), offset 5. If the implementation
  // ever fell back to assuming non-transport (offset 1), it would read
  // byte1 (0xAA, part of the transport codes) instead of the real path-len
  // byte at offset 5 (0x80) -- a materially different, wrong hash size.
  const transportFrame = '00aabbccdd80ff'; // header=00, codes=aabbccdd, pathLen=0x80 (size 3), 1 payload byte
  assert.strictEqual(packetHashSize(transportFrame), 3,
    'must read offset 5 from the header\'s own route bits, not default to offset 1');
});

test('two frames with the SAME trailing bytes but different headers classify independently', () => {
  // Same bytes after the header; only the header's route bits differ. A
  // mutant that ignores the header (always offset 1, or always offset 5)
  // would make these two agree; they must not.
  const nonTransport = '01' + '80' + 'ff';          // route 1, offset 1 -> byte1=0x80 -> size 3
  const transport = '00' + 'aabbccdd' + '80' + 'ff'; // route 0, offset 5 -> byte5=0x80 -> size 3
  assert.strictEqual(packetHashSize(nonTransport), 3);
  assert.strictEqual(packetHashSize(transport), 3);
  // Now make byte1 (which transport skips) disagree with byte5 — only a
  // real per-frame route read gets both right.
  const transportDisagreeing = '00' + 'FFFFFFFF' + '40' + 'ff'; // byte1=0xFF would be size 4 if misread; real path byte (offset5)=0x40 -> size 2
  assert.strictEqual(packetHashSize(transportDisagreeing), 2,
    'offset 5 byte (0x40 -> size 2), not offset 1 byte (0xFF) decides the size');
});

console.log('\n--- ' + passed + ' passed, ' + failed + ' failed ---\n');
process.exit(failed > 0 ? 1 : 0);
