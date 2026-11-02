/* test-live-heard-filter.js
 * #660 — Live map "Last heard" filter. Unit tests for the real
 * public/live-heard-filter.js module, run against the real roles.js
 * freshness helper (getNodeFreshness) in a vm sandbox. No DOM.
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

function load() {
  const ctx = {
    window: { addEventListener() {}, dispatchEvent() {} },
    document: {
      readyState: 'complete', documentElement: { style: {} },
      getElementById: () => null, querySelectorAll: () => [], querySelector: () => null,
      addEventListener() {}, createElement: () => ({ style: {} }), head: { appendChild() {} },
    },
    getComputedStyle: () => ({ getPropertyValue: () => '' }),
    localStorage: { getItem: () => null, setItem() {}, removeItem() {} },
    fetch: () => new Promise(() => {}),
    console, Date, Math, Number, String, JSON, Array, Object, Set, Map, parseInt, isFinite, isNaN,
    module: { exports: {} },
  };
  ctx.window.localStorage = ctx.localStorage;
  ctx.window.document = ctx.document;
  ctx.window.getComputedStyle = ctx.getComputedStyle;
  vm.createContext(ctx);
  for (const f of ['public/roles.js', 'public/live-heard-filter.js']) {
    vm.runInContext(fs.readFileSync(path.join(__dirname, f), 'utf8'), ctx, { filename: f });
    for (const k of Object.keys(ctx.window)) ctx[k] = ctx.window[k];
  }
  return { F: ctx.window.LiveHeardFilter, fresh: ctx.window.getNodeFreshness, mod: ctx.module.exports };
}

const { F, fresh, mod } = load();
const H = 3600000;
const NOW = Date.UTC(2026, 10, 2, 12, 0, 0);
const iso = (ms) => new Date(ms).toISOString();
const hidden = (node, key, now) => F.isHidden(node, F.windowMs(key), now == null ? NOW : now, fresh);

console.log('\n=== live-heard-filter: options, keys, hash/storage restore ===');

test('module exports the same object on window and module.exports', () => {
  assert.ok(F, 'window.LiveHeardFilter missing');
  assert.strictEqual(mod, F);
});

test('option set is All, 1h, 3h, 6h, 12h, 24h, 7d with All as default', () => {
  assert.deepStrictEqual(Array.from(F.OPTIONS, o => o.key), ['all', '1h', '3h', '6h', '12h', '24h', '7d']);
  assert.strictEqual(F.DEFAULT_KEY, 'all');
  assert.strictEqual(F.windowMs('all'), null);
  assert.strictEqual(F.windowMs('1h'), H);
  assert.strictEqual(F.windowMs('3h'), 3 * H);
  assert.strictEqual(F.windowMs('6h'), 6 * H);
  assert.strictEqual(F.windowMs('12h'), 12 * H);
  assert.strictEqual(F.windowMs('24h'), 24 * H);
  assert.strictEqual(F.windowMs('7d'), 7 * 24 * H);
});

test('normalizeKey rejects unknown, prototype and non-string values', () => {
  for (const bad of ['', '2h', 'ALL', '6H', 'constructor', '__proto__', 'toString', null, undefined, 6, {}]) {
    assert.strictEqual(F.normalizeKey(bad), 'all', 'for ' + String(bad));
    assert.strictEqual(F.windowMs(bad), null, 'windowMs for ' + String(bad));
  }
  assert.strictEqual(F.normalizeKey('12h'), '12h');
});

test('resolveInitialKey: hash wins, then storage, then All', () => {
  assert.strictEqual(F.resolveInitialKey('6h', '24h'), '6h');
  assert.strictEqual(F.resolveInitialKey(null, '24h'), '24h');
  assert.strictEqual(F.resolveInitialKey('bogus', '24h'), '24h');
  assert.strictEqual(F.resolveInitialKey('all', '24h'), 'all');
  assert.strictEqual(F.resolveInitialKey(null, null), 'all');
  assert.strictEqual(F.resolveInitialKey(null, 'junk'), 'all');
});

console.log('\n=== live-heard-filter: predicate ===');

test('All (null window) never hides, even a repeater with no timestamps', () => {
  assert.strictEqual(hidden({ role: 'repeater' }, 'all'), false);
  assert.strictEqual(hidden({ role: 'repeater', last_seen: iso(NOW - 400 * 24 * H) }, 'all'), false);
});

test('repeater heard 2h ago: hidden at 1h, visible at 3h', () => {
  const n = { role: 'repeater', last_seen: iso(NOW - 2 * H), last_heard: iso(NOW - 2 * H) };
  assert.strictEqual(hidden(n, '1h'), true);
  assert.strictEqual(hidden(n, '3h'), false);
});

test('boundary: age just under the window is visible, exactly at the window is hidden', () => {
  assert.strictEqual(hidden({ role: 'repeater', last_seen: NOW - H + 1 }, '1h'), false);
  assert.strictEqual(hidden({ role: 'repeater', last_seen: NOW - H }, '1h'), true);
  assert.strictEqual(hidden({ role: 'repeater', last_seen: NOW - H - 1 }, '1h'), true);
});

test('basis: newest of _lastHeard / last_heard / last_seen wins', () => {
  const old = iso(NOW - 10 * H);
  assert.strictEqual(hidden({ role: 'repeater', last_seen: old, last_heard: old, _lastHeard: NOW - 60000 }, '1h'), false);
  assert.strictEqual(hidden({ role: 'repeater', last_seen: old, last_heard: iso(NOW - 60000) }, '1h'), false);
  assert.strictEqual(hidden({ role: 'repeater', last_seen: iso(NOW - 60000), last_heard: old }, '1h'), false);
});

test('basis: _liveSeen (page-load stamp) is NOT evidence of traffic', () => {
  // live.js stamps _liveSeen = Date.now() on every API-loaded node, so a
  // filter that counted it would never hide anything after a reload.
  const n = { role: 'repeater', last_seen: iso(NOW - 10 * H), _liveSeen: NOW };
  assert.strictEqual(hidden(n, '1h'), true);
});

test('missing or invalid timestamps: in-scope node is hidden (no evidence of being heard)', () => {
  assert.strictEqual(hidden({ role: 'repeater' }, '24h'), true);
  assert.strictEqual(hidden({ role: 'room', last_seen: 'not a date', last_heard: '' }, '24h'), true);
  assert.strictEqual(hidden({ role: 'repeater', last_seen: true, last_heard: {} }, '24h'), true);
  assert.strictEqual(hidden({ role: 'repeater', last_seen: NaN }, '24h'), true);
});

test('future timestamps: within 30s tolerance count as now, beyond it are ignored', () => {
  assert.strictEqual(hidden({ role: 'repeater', last_seen: NOW + 20000 }, '1h'), false);
  assert.strictEqual(hidden({ role: 'repeater', last_seen: NOW + 5 * 60000 }, '1h'), true);
  // A bogus far-future value must not mask a real (old) one either way.
  assert.strictEqual(hidden({ role: 'repeater', last_seen: NOW + 5 * 60000, last_heard: iso(NOW - 2 * H) }, '3h'), false);
});

test('role scope: only repeater and room are filtered (case-insensitive)', () => {
  const old = iso(NOW - 48 * H);
  for (const role of ['repeater', 'room', 'Repeater', 'ROOM']) {
    assert.strictEqual(hidden({ role, last_seen: old }, '24h'), true, role);
  }
  for (const role of ['companion', 'sensor', 'observer', 'unknown', '', undefined, null, 7]) {
    assert.strictEqual(hidden({ role, last_seen: old }, '24h'), false, String(role));
  }
});

test('non-object node (ghost marker without nodeData) is never hidden', () => {
  assert.strictEqual(hidden(undefined, '1h'), false);
  assert.strictEqual(hidden(null, '1h'), false);
});

console.log('\n=== live-heard-filter: live traffic evidence ===');

const PK_A = 'aa'.repeat(32), PK_B = 'bb'.repeat(32), PK_C = 'cc'.repeat(32);

test('flood packet: every resolved relay hop is heard (null hops skipped, deduped)', () => {
  const keys = F.heardKeysFromPacket({ route_type: 1, payload_type: 5, resolved_path: [PK_A, null, PK_B, PK_A, ''] });
  assert.deepStrictEqual(Array.from(keys).sort(), [PK_A, PK_B]);
});

test('transport-flood (route 0) counts like flood', () => {
  assert.deepStrictEqual(Array.from(F.heardKeysFromPacket({ route_type: 0, payload_type: 4, resolved_path: [PK_C] })), [PK_C]);
});

test('direct routes (2, 3) do not count: their path is the remaining route, not relays (#623)', () => {
  assert.deepStrictEqual(Array.from(F.heardKeysFromPacket({ route_type: 2, payload_type: 5, resolved_path: [PK_A] })), []);
  assert.deepStrictEqual(Array.from(F.heardKeysFromPacket({ route_type: 3, payload_type: 5, resolved_path: [PK_A] })), []);
});

test('TRACE (payload 9) does not count: its path bytes are SNR values', () => {
  assert.deepStrictEqual(Array.from(F.heardKeysFromPacket({ route_type: 1, payload_type: 9, resolved_path: [PK_A] })), []);
});

test('unknown route or payload type does not count', () => {
  assert.deepStrictEqual(Array.from(F.heardKeysFromPacket({ payload_type: 5, resolved_path: [PK_A] })), []);
  assert.deepStrictEqual(Array.from(F.heardKeysFromPacket({ route_type: 1, resolved_path: [PK_A] })), []);
  assert.deepStrictEqual(Array.from(F.heardKeysFromPacket({ route_type: '1', payload_type: 5, resolved_path: [PK_A] })), []);
});

test('resolved_path must be an array (string or missing ignored)', () => {
  assert.deepStrictEqual(Array.from(F.heardKeysFromPacket({ route_type: 1, payload_type: 5, resolved_path: JSON.stringify([PK_A]) })), []);
  assert.deepStrictEqual(Array.from(F.heardKeysFromPacket({ route_type: 1, payload_type: 5 })), []);
});

test('ADVERT: the signed sender pubKey is heard, on any route', () => {
  const pkt = { route_type: 2, payload_type: 4, decoded: { header: { payloadTypeName: 'ADVERT' }, payload: { pubKey: PK_C } } };
  assert.deepStrictEqual(Array.from(F.heardKeysFromPacket(pkt)), [PK_C]);
  const notAdvert = { route_type: 2, payload_type: 5, decoded: { header: { payloadTypeName: 'GRP_TXT' }, payload: { pubKey: PK_C } } };
  assert.deepStrictEqual(Array.from(F.heardKeysFromPacket(notAdvert)), []);
});

test('null / garbage packet yields no keys', () => {
  for (const p of [null, undefined, 3, 'x', {}]) assert.strictEqual(F.heardKeysFromPacket(p).length, 0);
});

console.log('\n=== live-heard-filter: perf (10k nodes) ===');

test('predicate over 10k nodes (ISO strings, 3 fields each) stays fast', () => {
  const nodes = [];
  for (let i = 0; i < 10000; i++) {
    const t = NOW - (i % 200) * H / 10;
    nodes.push({ role: i % 3 ? 'repeater' : 'companion', last_seen: iso(t), last_heard: iso(t), _liveSeen: NOW, _lastHeard: i % 7 ? undefined : t });
  }
  const win = F.windowMs('6h');
  const bestOf = (fn) => {
    let best = Infinity, out = 0;
    for (let r = 0; r < 9; r++) {
      const t0 = process.hrtime.bigint();
      out = fn();
      best = Math.min(best, Number(process.hrtime.bigint() - t0) / 1e6);
    }
    return { best, out };
  };
  // Baseline: one bare getNodeFreshness pass over the same nodes. The
  // predicate is that call plus O(1) work, so it must stay close to it on
  // any machine; the absolute cap guards against a slow helper.
  const base = bestOf(() => {
    let c = 0;
    for (let i = 0; i < nodes.length; i++) if (fresh(nodes[i], NOW, { includeLiveSeen: false })) c++;
    return c;
  });
  const pred = bestOf(() => {
    let c = 0;
    for (let i = 0; i < nodes.length; i++) if (F.isHidden(nodes[i], win, NOW, fresh)) c++;
    return c;
  });
  console.log('     10k nodes: predicate best of 9 = ' + pred.best.toFixed(2) + ' ms (' + pred.out +
    ' hidden); bare getNodeFreshness pass = ' + base.best.toFixed(2) + ' ms');
  assert.ok(pred.out > 0 && pred.out < 10000, 'expected a mixed result, got ' + pred.out);
  assert.ok(pred.best < 1.5 * base.best + 0.5,
    'predicate ' + pred.best.toFixed(2) + ' ms vs freshness baseline ' + base.best.toFixed(2) + ' ms');
  assert.ok(pred.best < 25, 'predicate over 10k nodes took ' + pred.best.toFixed(2) + ' ms');
});

console.log('\n' + passed + ' passed, ' + failed + ' failed');
process.exit(failed ? 1 : 0);
