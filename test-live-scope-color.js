/* test-live-scope-color.js
 * #882 — Live "Color by scope". Unit tests for the real
 * public/live-scope-color.js module, loaded into a vm sandbox. No DOM.
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
  const ctx = { window: {}, console, Date, Math, Number, String, JSON, Array, Object, Map, Set };
  vm.createContext(ctx);
  const src = fs.readFileSync(path.join(__dirname, 'public', 'live-scope-color.js'), 'utf8');
  vm.runInContext(src, ctx, { filename: 'live-scope-color.js' });
  return ctx.window.LiveScopeColor;
}

const S = load();
const plain = (o) => JSON.parse(JSON.stringify(o));
const MIN = 60000;
console.log('\n=== live-scope-color.js ===');

test('module is exported', () => assert.ok(S && typeof S.classify === 'function'));

test('classification table: route 0/3 named, route 0/3 unknown, route 1/2/missing none', () => {
  const rows = [
    [0, '#dk', 'named', '#dk'], [3, '#eu', 'named', '#eu'],
    [0, null, 'unknown', ''], [3, '', 'unknown', ''], [0, undefined, 'unknown', ''],
    [1, null, 'none', ''], [2, null, 'none', ''], [null, null, 'none', ''], [undefined, '#dk', 'none', ''],
    [1, '#dk', 'none', ''], [2, '#dk', 'none', ''],
    ['0', '#dk', 'named', '#dk'], ['3', null, 'unknown', ''], ['1', null, 'none', ''],
  ];
  for (const [rt, name, kind, outName] of rows) {
    const c = S.classify(rt, name);
    assert.strictEqual(c.kind, kind, 'rt=' + rt + ' name=' + name + ' → ' + c.kind);
    assert.strictEqual(c.name, outName, 'rt=' + rt + ' name=' + name + ' name ' + c.name);
  }
});

test('isScoped: route 0/3 only (unknown scope counts as scoped)', () => {
  assert.strictEqual(S.isScoped(0), true);
  assert.strictEqual(S.isScoped(3), true);
  assert.strictEqual(S.isScoped('3'), true);
  assert.strictEqual(S.isScoped(1), false);
  assert.strictEqual(S.isScoped(2), false);
  assert.strictEqual(S.isScoped(null), false);
  assert.strictEqual(S.isScoped(undefined), false);
});

test('fnv1a: 32-bit FNV-1a reference vectors', () => {
  assert.strictEqual(S.fnv1a(''), 0x811c9dc5);
  assert.strictEqual(S.fnv1a('a'), 0xe40c292c);
  assert.strictEqual(S.fnv1a('foobar'), 0xbf9cf968);
});

test('preferredSlot is fnv1a mod 12', () => {
  for (const n of ['#dk', '#eu', '#dk-mj', '#de', 'x']) assert.strictEqual(S.preferredSlot(n), S.fnv1a(n) % 12);
  assert.strictEqual(S.SLOTS, 12);
});

test('slot stable across call order (no collision)', () => {
  // Pick two names with distinct preferred slots.
  const names = ['#dk', '#eu', '#de', '#se', '#no', '#fi'];
  const a = names.find(n => true), b = names.find(n => S.preferredSlot(n) !== S.preferredSlot(a));
  const r1 = S.createRegistry(), r2 = S.createRegistry();
  r1.touch(a, 0); r1.touch(b, 1);
  r2.touch(b, 0); r2.touch(a, 1);
  assert.strictEqual(r1.slotOf(a), r2.slotOf(a));
  assert.strictEqual(r1.slotOf(b), r2.slotOf(b));
  assert.strictEqual(r1.slotOf(a), S.preferredSlot(a));
});

function collidingPair() {
  const seen = new Map();
  for (let i = 0; i < 1000; i++) {
    const n = '#s' + i, s = S.preferredSlot(n);
    if (seen.has(s)) return [seen.get(s), n];
    seen.set(s, n);
  }
  throw new Error('no collision');
}

test('collision: second name probes to the next free slot, first keeps its slot (sticky)', () => {
  const [a, b] = collidingPair();
  const r = S.createRegistry();
  const sa = r.touch(a, 0);
  const sb = r.touch(b, 1);
  assert.strictEqual(sa, S.preferredSlot(a));
  assert.strictEqual(sb, (sa + 1) % 12);
  // Re-touching keeps both slots (session-sticky).
  assert.strictEqual(r.touch(a, 2), sa);
  assert.strictEqual(r.touch(b, 3), sb);
});

test('a slot held by an expired scope is reusable; the returning scope re-probes', () => {
  const [a, b] = collidingPair();
  const r = S.createRegistry();
  const sa = r.touch(a, 0);
  // a expires (no traffic for > 15 min); b takes the free preferred slot.
  const t = 16 * MIN;
  assert.strictEqual(r.touch(b, t), sa);
  // a comes back: its slot is owned by active b, so it moves.
  const sa2 = r.touch(a, t + 1);
  assert.notStrictEqual(sa2, sa);
  assert.strictEqual(sa2, (sa + 1) % 12);
});

test('more than 12 active scopes: the 13th gets the "other" slot (-1)', () => {
  const r = S.createRegistry();
  const slots = new Set();
  for (let i = 0; i < 12; i++) slots.add(r.touch('#n' + i, i));
  assert.strictEqual(slots.size, 12, 'first 12 scopes get 12 distinct slots');
  assert.ok(!slots.has(-1));
  assert.strictEqual(r.touch('#n12', 12), S.OTHER);
  // Once one of the 12 expires, the overflow scope gets a real slot.
  for (let i = 1; i < 12; i++) r.touch('#n' + i, 20 * MIN);
  const s = r.touch('#n12', 20 * MIN + 1);
  assert.ok(s >= 0 && s < 12, 'slot ' + s);
});

test('LRU cap: never more than 64 entries, oldest evicted first', () => {
  const r = S.createRegistry();
  for (let i = 0; i < 200; i++) r.touch('#lru' + i, i);
  assert.strictEqual(r.size(), S.CAP);
  assert.strictEqual(S.CAP, 64);
  assert.strictEqual(r.has('#lru0'), false);
  assert.strictEqual(r.has('#lru199'), true);
  assert.strictEqual(r.has('#lru136'), true);
  assert.strictEqual(r.has('#lru135'), false);
  // A touch refreshes recency: #lru136 survives the next eviction.
  r.touch('#lru136', 300);
  r.touch('#new', 301);
  assert.strictEqual(r.has('#lru136'), true);
  assert.strictEqual(r.has('#lru137'), false);
});

test('legend: only scopes seen in the last 15 min, at most 12 rows plus a "more" count', () => {
  const r = S.createRegistry();
  r.touch('#old', 0);
  const now = 20 * MIN;
  for (let i = 0; i < 20; i++) r.touch('#v' + String(i).padStart(2, '0'), now - i * 1000);
  const L = r.legend(now);
  assert.strictEqual(L.rows.length, 12);
  assert.strictEqual(L.more, 8);
  assert.ok(!L.rows.some(x => x.name === '#old'), 'expired scope listed');
  // The 12 most recent, shown sorted by name for a stable layout.
  const names = L.rows.map(x => x.name);
  assert.deepStrictEqual(names, names.slice().sort());
  assert.ok(names.includes('#v00') && names.includes('#v11') && !names.includes('#v12'));
  assert.strictEqual(S.LEGEND_ROWS, 12);
  assert.strictEqual(S.WINDOW_MS, 15 * MIN);
});

test('legend window boundary: exactly 15 min old is still listed, older is not', () => {
  const r = S.createRegistry();
  r.touch('#edge', 0);
  assert.strictEqual(r.legend(15 * MIN).rows.length, 1);
  assert.strictEqual(r.legend(15 * MIN + 1).rows.length, 0);
});

test('legend rows carry their slot; signature changes only when rows change', () => {
  const r = S.createRegistry();
  r.touch('#dk', 0);
  const L1 = r.legend(1000);
  assert.strictEqual(L1.rows[0].slot, r.slotOf('#dk'));
  r.touch('#dk', 2000);
  assert.strictEqual(r.legend(3000).sig, L1.sig, 'same rows → same signature');
  r.touch('#eu', 4000);
  assert.notStrictEqual(r.legend(5000).sig, L1.sig);
});

test('slotOf for an unseen name returns the preferred slot without registering it', () => {
  const r = S.createRegistry();
  assert.strictEqual(r.slotOf('#ghost'), S.preferredSlot('#ghost'));
  assert.strictEqual(r.size(), 0);
});

test('paletteVar: slots, neutrals and other map to the CSS variables', () => {
  assert.strictEqual(S.paletteVar({ kind: 'named' }, 0), '--scope-1');
  assert.strictEqual(S.paletteVar({ kind: 'named' }, 11), '--scope-12');
  assert.strictEqual(S.paletteVar({ kind: 'named' }, -1), '--scope-other');
  assert.strictEqual(S.paletteVar({ kind: 'unknown' }, 3), '--scope-unknown');
  assert.strictEqual(S.paletteVar({ kind: 'none' }, 3), '--scope-none');
  assert.strictEqual(S.PALETTE_VARS.length, 15);
});

test('readPalette resolves every variable through the injected reader', () => {
  const p = S.readPalette((v) => 'c' + v);
  assert.strictEqual(p['--scope-1'], 'c--scope-1');
  assert.strictEqual(p['--scope-other'], 'c--scope-other');
  assert.strictEqual(Object.keys(p).length, 15);
  // Blank (unresolved) variables fall back to the "none" value, never ''.
  const q = S.readPalette((v) => v === '--scope-none' ? ' grey ' : '');
  assert.strictEqual(q['--scope-3'], 'grey');
});

test('resolveColorMode: valid ?color= wins over storage, invalid is ignored', () => {
  const st = { scope: false, hash: true };
  assert.deepStrictEqual(plain(S.resolveColorMode('scope', st)), { scope: true, hash: true, fromHash: true });
  assert.deepStrictEqual(plain(S.resolveColorMode('hash', { scope: true, hash: false })), { scope: false, hash: true, fromHash: true });
  assert.deepStrictEqual(plain(S.resolveColorMode('type', st)), { scope: false, hash: false, fromHash: true });
  assert.deepStrictEqual(plain(S.resolveColorMode('bogus', { scope: true, hash: false })), { scope: true, hash: false, fromHash: false });
  assert.deepStrictEqual(plain(S.resolveColorMode(null, st)), { scope: false, hash: true, fromHash: false });
});

test('resolveScopedOnly: ?scoped=1/0 wins over storage, others ignored', () => {
  assert.deepStrictEqual(plain(S.resolveScopedOnly('1', false)), { on: true, fromHash: true });
  assert.deepStrictEqual(plain(S.resolveScopedOnly('0', true)), { on: false, fromHash: true });
  assert.deepStrictEqual(plain(S.resolveScopedOnly('yes', true)), { on: true, fromHash: false });
  assert.deepStrictEqual(plain(S.resolveScopedOnly(null, false)), { on: false, fromHash: false });
});

test('perf: 30 000 classify+touch calls over 50 names < 50 ms, registry bounded', () => {
  const r = S.createRegistry();
  const names = Array.from({ length: 50 }, (_, i) => '#perf' + i);
  const t0 = process.hrtime.bigint();
  for (let i = 0; i < 30000; i++) {
    const c = S.classify(i % 4 === 1 ? 1 : 0, names[i % 50]);
    if (c.kind === 'named') r.touch(c.name, i);
  }
  const ms = Number(process.hrtime.bigint() - t0) / 1e6;
  assert.ok(ms < 50, ms.toFixed(1) + ' ms');
  assert.ok(r.size() <= S.CAP);
});

test('style.css defines all 15 --scope-* variables in light and both dark blocks (behind the colourblind hook of the mode), dark blocks in sync', () => {
  const css = fs.readFileSync(path.join(__dirname, 'public', 'style.css'), 'utf8');
  function block(startRe) {
    const m = startRe.exec(css);
    assert.ok(m, 'block not found: ' + startRe);
    let i = css.indexOf('{', m.index) + 1, depth = 1;
    const from = i;
    while (depth && i < css.length) { if (css[i] === '{') depth++; else if (css[i] === '}') depth--; i++; }
    return css.slice(from, i - 1);
  }
  function vars(text) {
    const out = {};
    const re = /(--scope-[a-z0-9]+)\s*:\s*([^;]+);/g;
    let m;
    // #882 M2: each value reads a colourblind preset hook first,
    // var(--mc-scope-<mode>-<n>, #default); the default is what counts here.
    while ((m = re.exec(text))) {
      const v = m[2].trim();
      const hook = /^var\(--mc-scope-(light|dark)-([a-z0-9]+),\s*(#[0-9a-f]{6})\)$/.exec(v);
      out[m[1]] = hook && '--scope-' + hook[2] === m[1] ? hook[1] + ':' + hook[3] : v;
    }
    return out;
  }
  // The light block is the :root that holds --trace-ghost-color (no nesting).
  const roots = css.match(/(^|\n):root\s*\{[^}]*\}/g) || [];
  const light = vars(roots.find((b) => b.includes('--trace-ghost-color')) || '');
  const media = vars(block(/@media \(prefers-color-scheme: dark\)\s*\{\s*:root:not\(\[data-theme="light"\]\)/));
  const manual = vars(block(/\n\[data-theme="dark"\]\s*\{\s*\n\s*color-scheme: dark;/));
  for (const v of S.PALETTE_VARS) {
    assert.ok(/^light:#[0-9a-f]{6}$/.test(light[v] || ''), 'light ' + v + ' = ' + light[v]);
    assert.ok(/^dark:#[0-9a-f]{6}$/.test(media[v] || ''), 'media dark ' + v + ' = ' + media[v]);
    assert.strictEqual(manual[v], media[v], 'dark blocks differ on ' + v);
  }
  const distinct = (o) => new Set(S.PALETTE_VARS.map((v) => o[v])).size;
  assert.strictEqual(distinct(light), 15, 'light palette has duplicate colours');
  assert.strictEqual(distinct(media), 15, 'dark palette has duplicate colours');
});

console.log('\n' + passed + ' passed, ' + failed + ' failed');
process.exit(failed ? 1 : 0);
