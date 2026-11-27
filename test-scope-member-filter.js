/* #990: unit tests for public/scope-member-filter.js, the live side of a
 * server member-set filter (?area= today, the DAGI params in #740 M3b).
 *
 * The server keeps a packet under ?area= when its sender is a member of the
 * area: the decoded ADVERT public_key (or pubKey) must be in the member set
 * (filterPacketsPrepared and packetWhere in cmd/server). A packet with no
 * sender is dropped. An area with no positioned member resolves to no set at
 * all and filters nothing (areaMemberSet returns nil).
 *
 * The real module runs in a vm context. fetchMembers and keyOf are fakes the
 * tests control, so every await is deterministic.
 *
 * Usage: node test-scope-member-filter.js
 */
'use strict';
const vm = require('vm');
const fs = require('fs');
const assert = require('assert');

let passed = 0, failed = 0;
const queue = [];
function test(name, fn) { queue.push([name, fn]); }

function load() {
  const ctx = { window: {}, Set, Promise, console };
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync('public/scope-member-filter.js', 'utf8'), ctx,
    { filename: 'public/scope-member-filter.js' });
  return ctx.window.ScopeMemberFilter;
}

async function settle() {
  for (let i = 0; i < 20; i++) await Promise.resolve();
}

// A fetchMembers whose answers the test releases one by one.
function heldFetch() {
  const calls = [];
  const fetchMembers = (key) => new Promise((resolve, reject) => {
    calls.push({ key, resolve, reject });
  });
  return { calls, fetchMembers };
}

// Items are the senders themselves, so keyOf is the identity, counted.
function counted() {
  let n = 0;
  return { keyOf: (pk) => { n++; return pk; }, count: () => n };
}

const SMF = load();

test('with no selection every packet passes, and no sender is read', async () => {
  const f = heldFetch();
  const k = counted();
  const m = SMF.create({ fetchMembers: f.fetchMembers, keyOf: k.keyOf, emptyMatchesAll: true });
  assert.strictEqual(m.accepts('aa'), true);
  assert.strictEqual(m.accepts(''), true);
  await m.setScope('');
  assert.strictEqual(m.accepts('aa'), true);
  assert.strictEqual(f.calls.length, 0, 'no selection fetches nothing');
  assert.strictEqual(k.count(), 0, 'no selection reads no sender');
});

test('a selection keeps only its members, and a packet with no sender is dropped', async () => {
  const f = heldFetch();
  const m = SMF.create({ fetchMembers: f.fetchMembers, keyOf: (pk) => pk, emptyMatchesAll: true });
  const done = m.setScope('IN');
  await settle();
  assert.deepStrictEqual(f.calls.map(c => c.key), ['IN']);
  f.calls[0].resolve(['aa', 'bb']);
  await done;
  assert.strictEqual(m.accepts('aa'), true);
  assert.strictEqual(m.accepts('bb'), true);
  assert.strictEqual(m.accepts('cc'), false, 'a sender outside the area');
  assert.strictEqual(m.accepts(''), false, 'no resolvable sender');
  assert.strictEqual(m.accepts(undefined), false, 'no resolvable sender');
  assert.strictEqual(m.accepts('AA'), false, 'the server compares keys exactly');
  assert.strictEqual(m.scope(), 'IN');
});

test('the member set is fetched once per change of selection', async () => {
  const f = heldFetch();
  const m = SMF.create({ fetchMembers: f.fetchMembers, keyOf: (pk) => pk, emptyMatchesAll: true });
  m.setScope('IN');
  m.setScope('IN');
  await settle();
  assert.strictEqual(f.calls.length, 1, 'a second call while loading joins the first');
  f.calls[0].resolve(['aa']);
  await settle();
  await m.setScope('IN');
  for (let i = 0; i < 1000; i++) m.accepts('aa');
  assert.strictEqual(f.calls.length, 1, 'neither the same selection nor packets fetch again');
  m.setScope('OTHER');
  await settle();
  assert.deepStrictEqual(f.calls.map(c => c.key), ['IN', 'OTHER']);
  f.calls[1].resolve(['bb']);
  await settle();
  assert.strictEqual(m.accepts('aa'), false);
  assert.strictEqual(m.accepts('bb'), true);
  m.setScope('IN');
  await settle();
  assert.deepStrictEqual(f.calls.map(c => c.key), ['IN', 'OTHER', 'IN'], 'going back is a change too');
});

test('nothing passes while the member set loads', async () => {
  const f = heldFetch();
  const m = SMF.create({ fetchMembers: f.fetchMembers, keyOf: (pk) => pk, emptyMatchesAll: true });
  m.setScope('IN');
  await settle();
  assert.strictEqual(m.accepts('aa'), false, 'a member is not known to be one yet');
  f.calls[0].resolve(['aa']);
  await settle();
  assert.strictEqual(m.accepts('aa'), true);
  m.setScope('OTHER');
  assert.strictEqual(m.accepts('aa'), false, 'the old set does not answer for the new selection');
});

test('only the latest selection installs its member set', async () => {
  const f = heldFetch();
  const m = SMF.create({ fetchMembers: f.fetchMembers, keyOf: (pk) => pk, emptyMatchesAll: true });
  m.setScope('IN');
  m.setScope('OTHER');
  await settle();
  f.calls[1].resolve(['bb']);
  await settle();
  f.calls[0].resolve(['aa']);
  await settle();
  assert.strictEqual(m.accepts('aa'), false, 'a late answer for an old selection is ignored');
  assert.strictEqual(m.accepts('bb'), true);
  // Clearing the selection drops a load still in flight.
  m.setScope('THIRD');
  await settle();
  m.setScope('');
  f.calls[2].resolve([]);
  await settle();
  assert.strictEqual(m.scope(), '');
  assert.strictEqual(m.accepts('zz'), true);
});

test('an empty member set filters nothing when emptyMatchesAll is set (the Area rule)', async () => {
  const f = heldFetch();
  const m = SMF.create({ fetchMembers: f.fetchMembers, keyOf: (pk) => pk, emptyMatchesAll: true });
  m.setScope('EMPTY');
  await settle();
  f.calls[0].resolve([]);
  await settle();
  assert.strictEqual(m.accepts('aa'), true);
  assert.strictEqual(m.accepts(''), true, 'even a packet with no sender, as the server');
});

test('an empty member set matches nothing without emptyMatchesAll (the DAGI rule)', async () => {
  const f = heldFetch();
  const m = SMF.create({ fetchMembers: f.fetchMembers, keyOf: (pk) => pk });
  m.setScope('0751');
  await settle();
  f.calls[0].resolve([]);
  await settle();
  assert.strictEqual(m.accepts('aa'), false);
  assert.strictEqual(m.accepts(''), false);
});

test('a null answer counts as an empty member set', async () => {
  const f = heldFetch();
  const m = SMF.create({ fetchMembers: f.fetchMembers, keyOf: (pk) => pk, emptyMatchesAll: true });
  m.setScope('EMPTY');
  await settle();
  f.calls[0].resolve(null);
  await settle();
  assert.strictEqual(m.accepts('aa'), true);
});

test('a failed load drops packets, and the next setScope retries', async () => {
  const f = heldFetch();
  const m = SMF.create({ fetchMembers: f.fetchMembers, keyOf: (pk) => pk, emptyMatchesAll: true });
  const done = m.setScope('IN');
  await settle();
  f.calls[0].reject(new Error('offline'));
  await done; // resolves: a failed load is not an unhandled rejection
  assert.strictEqual(m.accepts('aa'), false);
  m.setScope('IN');
  await settle();
  assert.strictEqual(f.calls.length, 2, 'a failed selection is fetched again');
  f.calls[1].resolve(['aa']);
  await settle();
  assert.strictEqual(m.accepts('aa'), true);
});

test('a fetchMembers that throws counts as a failed load', async () => {
  const m = SMF.create({ fetchMembers: () => { throw new Error('sync'); }, keyOf: (pk) => pk, emptyMatchesAll: true });
  await m.setScope('IN');
  assert.strictEqual(m.accepts('aa'), false);
});

test('ready() says whether accepts() answers from a loaded set', async () => {
  const f = heldFetch();
  const m = SMF.create({ fetchMembers: f.fetchMembers, keyOf: (pk) => pk, emptyMatchesAll: true });
  assert.strictEqual(m.ready(), true, 'no selection');
  m.setScope('IN');
  assert.strictEqual(m.ready(), false, 'loading');
  await settle();
  f.calls[0].resolve(['aa']);
  await settle();
  assert.strictEqual(m.ready(), true, 'loaded');
  m.setScope('OTHER');
  await settle();
  f.calls[1].reject(new Error('offline'));
  await settle();
  assert.strictEqual(m.ready(), false, 'failed');
  m.setScope('');
  assert.strictEqual(m.ready(), true, 'cleared');
});

test('keyOf reads the sender only when the set can decide', async () => {
  const f = heldFetch();
  const k = counted();
  const m = SMF.create({ fetchMembers: f.fetchMembers, keyOf: k.keyOf, emptyMatchesAll: true });
  m.setScope('IN');
  await settle();
  m.accepts('aa');
  assert.strictEqual(k.count(), 0, 'not while loading');
  f.calls[0].resolve([]);
  await settle();
  m.accepts('aa');
  assert.strictEqual(k.count(), 0, 'not when the empty set filters nothing');
  m.setScope('OTHER');
  await settle();
  f.calls[1].resolve(['aa']);
  await settle();
  m.accepts('aa');
  assert.strictEqual(k.count(), 1);
});

test('advertSender is the server rule: public_key, else pubKey, else none', () => {
  assert.strictEqual(SMF.advertSender({ type: 'ADVERT', public_key: 'aa' }), 'aa');
  assert.strictEqual(SMF.advertSender({ pubKey: 'bb' }), 'bb');
  assert.strictEqual(SMF.advertSender({ public_key: 'aa', pubKey: 'bb' }), 'aa');
  assert.strictEqual(SMF.advertSender({ public_key: '', pubKey: 'bb' }), 'bb');
  assert.strictEqual(SMF.advertSender({ public_key: 42, pubKey: 'bb' }), 'bb', 'a non-string key is no key');
  assert.strictEqual(SMF.advertSender({ public_key: 42 }), '');
  assert.strictEqual(SMF.advertSender({ type: 'GRP_TXT', channel: '#test' }), '');
  assert.strictEqual(SMF.advertSender({}), '');
  assert.strictEqual(SMF.advertSender(null), '');
  assert.strictEqual(SMF.advertSender(undefined), '');
  assert.strictEqual(SMF.advertSender('aa'), '');
});

test('perf: the per-packet check is a Set lookup, 1M checks on 2,000 members', async () => {
  const members = [];
  for (let i = 0; i < 2000; i++) members.push(i.toString(16).padStart(64, '0'));
  const m = SMF.create({ fetchMembers: () => Promise.resolve(members), keyOf: (pk) => pk, emptyMatchesAll: true });
  await m.setScope('IN');
  const probe = [members[0], members[1999], 'f'.repeat(64), ''];
  let hits = 0;
  const t0 = process.hrtime.bigint();
  for (let i = 0; i < 1e6; i++) if (m.accepts(probe[i & 3])) hits++;
  const ms = Number(process.hrtime.bigint() - t0) / 1e6;
  assert.strictEqual(hits, 5e5);
  console.log('    1M accepts() on 2,000 members: ' + ms.toFixed(1) + ' ms');
  // A scan over the members would take ~1M x 1,000 compares, seconds.
  assert.ok(ms < 1000, 'took ' + ms.toFixed(1) + ' ms');
});

(async () => {
  console.log('\n=== #990 scope-member-filter.js ===');
  for (const [name, fn] of queue) {
    try { await fn(); passed++; console.log('  ✅ ' + name); }
    catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
  }
  console.log('\n' + passed + ' passed, ' + failed + ' failed');
  if (failed) process.exit(1);
})().catch((e) => { console.error(e); process.exit(1); });
