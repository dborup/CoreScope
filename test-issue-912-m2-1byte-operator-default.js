/**
 * #912 M2 — the operator chooses the default 1-byte hop mode.
 *
 * Runs the real hop-filter.js, roles.js' client-config hand-off and
 * customize-v2.js' select in vm sandboxes:
 *   - resolution order: the visitor's stored choice, then the operator default
 *     (customizer.oneByteHopsDefault), then mark;
 *   - an explicit choice is always stored, so an explicit "mark" survives a
 *     "hide" default;
 *   - a visitor who never chose follows the operator default, also when it
 *     changes later, and is told so through mc-hide-1byte-hops-changed;
 *   - the #1633 boolean key still migrates;
 *   - the customizer marks the server default.
 */
'use strict';
const fs = require('fs');
const path = require('path');
const vm = require('vm');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.log('  ✗ ' + name + ': ' + e.message); }
}
function assert(cond, msg) { if (!cond) throw new Error(msg); }

const MODE_KEY = 'meshcore-1byte-hops-mode';
const LEGACY_KEY = 'meshcore-hide-1byte-hops';
const SRC = fs.readFileSync(path.join(__dirname, 'public', 'hop-filter.js'), 'utf8');

function sandbox(store, pre) {
  const events = [];
  const ctx = {
    console,
    localStorage: {
      getItem: (k) => (Object.prototype.hasOwnProperty.call(store, k) ? store[k] : null),
      setItem: (k, v) => { store[k] = String(v); },
      removeItem: (k) => { delete store[k]; },
    },
    CustomEvent: function (type, init) { this.type = type; this.detail = init && init.detail; },
    dispatchEvent: (e) => { events.push(e); return true; },
  };
  ctx.window = ctx;
  if (pre) pre(ctx);
  vm.createContext(ctx);
  vm.runInContext(SRC, ctx, { filename: 'hop-filter.js' });
  ctx.__changes = () => events.filter(e => e.type === 'mc-hide-1byte-hops-changed').map(e => e.detail.mode);
  return ctx;
}

console.log('\n=== #912 M2 operator default for the 1-byte hop mode ===');

// ── Resolution order ────────────────────────────────────────────────────
test('no stored choice, no operator default: mark', () => {
  const w = sandbox({});
  assert(w.MC_get1ByteHopsMode() === 'mark', 'got ' + w.MC_get1ByteHopsMode());
  assert(w.MC_get1ByteHopsDefault() === 'mark', 'default ' + w.MC_get1ByteHopsDefault());
});

test('no stored choice: the operator default applies', () => {
  for (const d of ['show', 'mark', 'hide']) {
    const w = sandbox({});
    w.MC_set1ByteHopsDefault(d);
    assert(w.MC_get1ByteHopsMode() === d, d + ': got ' + w.MC_get1ByteHopsMode());
    assert(w.MC_get1ByteHopsDefault() === d, d + ': default ' + w.MC_get1ByteHopsDefault());
  }
  const w = sandbox({});
  w.MC_set1ByteHopsDefault('hide');
  assert(w.MC_getHide1ByteHops() === true && w.MC_isVisibleHop('AB') === false, 'a hide default must hide');
});

test('the stored choice wins over the operator default', () => {
  for (const [stored, d] of [['show', 'hide'], ['mark', 'hide'], ['hide', 'show'], ['mark', 'show']]) {
    const w = sandbox({ [MODE_KEY]: stored });
    w.MC_set1ByteHopsDefault(d);
    assert(w.MC_get1ByteHopsMode() === stored, `stored ${stored}, default ${d}: got ${w.MC_get1ByteHopsMode()}`);
  }
});

test('an invalid operator default is ignored: mark', () => {
  for (const junk of ['bogus', 'constructor', '__proto__', 'toString', '', null, undefined, 3, 'Hide']) {
    const w = sandbox({});
    w.MC_set1ByteHopsDefault(junk);
    assert(w.MC_get1ByteHopsMode() === 'mark', JSON.stringify(junk) + ' read as ' + w.MC_get1ByteHopsMode());
    assert(w.MC_get1ByteHopsDefault() === 'mark', JSON.stringify(junk) + ' default ' + w.MC_get1ByteHopsDefault());
  }
});

test('a client config that arrived before hop-filter.js loaded is picked up', () => {
  const w = sandbox({}, (c) => { c.MC_CUSTOMIZER_CFG = { disabledTabs: [], oneByteHopsDefault: 'hide' }; });
  assert(w.MC_get1ByteHopsMode() === 'hide', 'got ' + w.MC_get1ByteHopsMode());
});

// ── Explicit storage ────────────────────────────────────────────────────
test('every explicit choice is stored, the default one too', () => {
  const store = {};
  const w = sandbox(store);
  for (const m of ['hide', 'show', 'mark']) {
    w.MC_set1ByteHopsMode(m);
    assert(store[MODE_KEY] === m, m + ' stored as ' + JSON.stringify(store));
  }
});

test('an explicit "mark" survives a "hide" operator default', () => {
  const store = {};
  const w = sandbox(store);
  w.MC_set1ByteHopsDefault('hide');
  w.MC_set1ByteHopsMode('mark');
  assert(w.MC_get1ByteHopsMode() === 'mark', 'same page: ' + w.MC_get1ByteHopsMode());
  // Next visit: a fresh page, the server still says hide.
  const next = sandbox(store);
  next.MC_set1ByteHopsDefault('hide');
  assert(next.MC_get1ByteHopsMode() === 'mark', 'after reload: ' + next.MC_get1ByteHopsMode());
});

test('junk is still rejected and stores nothing', () => {
  const store = {};
  const w = sandbox(store);
  for (const junk of ['nonsense', 'constructor', '__proto__', '']) w.MC_set1ByteHopsMode(junk);
  assert(!(MODE_KEY in store), JSON.stringify(store));
});

// ── Following the operator default ──────────────────────────────────────
test('a visitor who never chose follows a later change of the default, and is told', () => {
  const store = {};
  const w = sandbox(store);
  w.MC_set1ByteHopsDefault('hide');
  assert(w.MC_get1ByteHopsMode() === 'hide', 'first default');
  w.MC_set1ByteHopsDefault('show');
  assert(w.MC_get1ByteHopsMode() === 'show', 'changed default');
  assert(!(MODE_KEY in store), 'following the default stores nothing: ' + JSON.stringify(store));
  assert(JSON.stringify(w.__changes()) === '["hide","show"]', 'events ' + JSON.stringify(w.__changes()));
});

test('a default equal to the current mode fires no event; a stored choice hides the change', () => {
  const w = sandbox({});
  w.MC_set1ByteHopsDefault('mark');
  assert(w.__changes().length === 0, 'mark → mark fired ' + JSON.stringify(w.__changes()));
  const s = sandbox({ [MODE_KEY]: 'show' });
  s.MC_set1ByteHopsDefault('hide');
  assert(s.__changes().length === 0, 'a stored choice is unaffected, no event: ' + JSON.stringify(s.__changes()));
});

// ── Legacy migration ────────────────────────────────────────────────────
test('the #1633 "true" key still migrates to a stored hide, even under a show default', () => {
  const store = { [LEGACY_KEY]: 'true' };
  const w = sandbox(store);
  w.MC_set1ByteHopsDefault('show');
  assert(w.MC_get1ByteHopsMode() === 'hide', 'got ' + w.MC_get1ByteHopsMode());
  assert(store[MODE_KEY] === 'hide' && !(LEGACY_KEY in store), JSON.stringify(store));
});

test('another legacy value is dropped and the operator default applies', () => {
  const store = { [LEGACY_KEY]: 'false' };
  const w = sandbox(store);
  w.MC_set1ByteHopsDefault('hide');
  assert(w.MC_get1ByteHopsMode() === 'hide' && !(LEGACY_KEY in store) && !(MODE_KEY in store), JSON.stringify(store));
});

test('a junk stored mode is dropped and the operator default applies', () => {
  const store = { [MODE_KEY]: 'bogus' };
  const w = sandbox(store);
  w.MC_set1ByteHopsDefault('show');
  assert(w.MC_get1ByteHopsMode() === 'show' && !(MODE_KEY in store), JSON.stringify(store));
});

// ── roles.js hands the server value to hop-filter.js ───────────────────
test('roles.js passes customizer.oneByteHopsDefault from /api/config/client to hop-filter.js', () => {
  const roles = fs.readFileSync(path.join(__dirname, 'public', 'roles.js'), 'utf8');
  const start = roles.indexOf('window.MC_CUSTOMIZER_CFG =');
  assert(start !== -1, 'MC_CUSTOMIZER_CFG assignment not found');
  const end = roles.indexOf(';', roles.indexOf('MC_set1ByteHopsDefault', start));
  assert(end > start, 'roles.js does not call MC_set1ByteHopsDefault after MC_CUSTOMIZER_CFG');
  const snippet = roles.slice(start, end + 1);
  for (const [cust, want] of [[{ oneByteHopsDefault: 'hide', disabledTabs: ['x'] }, 'hide'], [undefined, 'mark'], [{ oneByteHopsDefault: 7 }, 'mark']]) {
    const w = sandbox({});
    vm.runInContext('(function (cfg) {' + snippet + '})(' + JSON.stringify({ customizer: cust }) + ')', w);
    assert(w.MC_get1ByteHopsMode() === want, JSON.stringify(cust) + ' → ' + w.MC_get1ByteHopsMode());
    assert(Array.isArray(w.MC_CUSTOMIZER_CFG.disabledTabs), 'disabledTabs must stay an array');
  }
});

// ── The customizer marks the server default ─────────────────────────────
test('the customizer select labels the server default and selects the effective mode', () => {
  const src = fs.readFileSync(path.join(__dirname, 'public', 'customize-v2.js'), 'utf8');
  const m = src.match(/function _renderHide1ByteHopsToggle\(\) \{[\s\S]*?\n  \}\n/);
  assert(m, '_renderHide1ByteHopsToggle not found');
  for (const [d, stored] of [['hide', null], ['show', 'mark'], ['mark', null]]) {
    const w = sandbox(stored ? { [MODE_KEY]: stored } : {});
    w.MC_set1ByteHopsDefault(d);
    const html = vm.runInContext('(' + m[0].trim() + ')()', w);
    const opts = [...html.matchAll(/<option value="(\w+)"( selected)?>([^<]*)<\/option>/g)];
    assert(opts.length === 3, 'options: ' + html);
    const marked = opts.filter(o => /\(server default\)/.test(o[3])).map(o => o[1]);
    assert(JSON.stringify(marked) === JSON.stringify([d]), `default ${d}: marked ${JSON.stringify(marked)}`);
    const sel = opts.filter(o => o[2]).map(o => o[1]);
    assert(JSON.stringify(sel) === JSON.stringify([stored || d]), `default ${d}, stored ${stored}: selected ${JSON.stringify(sel)}`);
  }
});

console.log(`\n#912 M2: ${passed} passed, ${failed} failed`);
process.exit(failed ? 1 : 0);
