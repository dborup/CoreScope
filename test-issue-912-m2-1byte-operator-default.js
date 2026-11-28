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
 * PR #1001 review:
 *   - F1 a visitor with a stored choice can go back to following the
 *     default ("Use server default", MC_clear1ByteHopsMode);
 *   - F2 that, and Reset All, fire mc-hide-1byte-hops-changed;
 *   - F3 an open customizer follows a client config that lands later
 *     (mc-1byte-hops-default-changed);
 *   - F5 a junk early MC_CUSTOMIZER_CFG, and the #1633 key under a
 *     load-time default.
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
  ctx.__events = events;
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
  for (const [d, stored] of [['hide', null], ['show', 'mark'], ['mark', null]]) {
    const p = customizerPage(stored ? { [MODE_KEY]: stored } : {});
    p.ctx.window.MC_set1ByteHopsDefault(d);
    const { html, sel } = p.mount();
    assert(sel.options.length === 3, 'options: ' + html);
    const s = p.shown();
    assert(JSON.stringify(s.defaults) === JSON.stringify([d]), `default ${d}: marked ${JSON.stringify(s.defaults)}`);
    assert(s.value === (stored || d), `default ${d}, stored ${stored}: selected ${s.value}`);
  }
});

// ── Review of PR #1001 (F1–F3, F5) ─────────────────────────────────────
// F1: a visitor who picked a mode has a way back to following the operator
// default. F2: that way back (and Reset All) tells the open page. F3: an open
// customizer follows a client config that lands after it rendered.
const defaultChanges = (w) => w.__events.filter(e => e.type === 'mc-1byte-hops-default-changed').map(e => e.detail.default);

test('F1 MC_get1ByteHopsChoice: the stored pick, or null when the visitor follows the default', () => {
  const w = sandbox({});
  w.MC_set1ByteHopsDefault('hide');
  assert(w.MC_get1ByteHopsChoice() === null, 'no pick: ' + w.MC_get1ByteHopsChoice());
  w.MC_set1ByteHopsMode('hide');
  assert(w.MC_get1ByteHopsChoice() === 'hide', 'a pick equal to the default is still a pick: ' + w.MC_get1ByteHopsChoice());
  assert(sandbox({ [MODE_KEY]: 'bogus' }).MC_get1ByteHopsChoice() === null, 'junk is no pick');
  const store = { [LEGACY_KEY]: 'true' };
  assert(sandbox(store).MC_get1ByteHopsChoice() === 'hide' && store[MODE_KEY] === 'hide', 'the #1633 key migrates: ' + JSON.stringify(store));
});

test('F1 MC_clear1ByteHopsMode: the pick goes, the visitor follows the default again, later changes too', () => {
  const store = { [MODE_KEY]: 'show', [LEGACY_KEY]: 'true' };
  const w = sandbox(store);
  w.MC_set1ByteHopsDefault('hide');
  assert(w.MC_get1ByteHopsMode() === 'show', 'before: ' + w.MC_get1ByteHopsMode());
  w.MC_clear1ByteHopsMode();
  assert(!(MODE_KEY in store) && !(LEGACY_KEY in store), 'keys left: ' + JSON.stringify(store));
  assert(w.MC_get1ByteHopsMode() === 'hide' && w.MC_get1ByteHopsChoice() === null, 'after: ' + w.MC_get1ByteHopsMode());
  w.MC_set1ByteHopsDefault('show');
  assert(w.MC_get1ByteHopsMode() === 'show', 'a later default is followed: ' + w.MC_get1ByteHopsMode());
});

test('F2 MC_clear1ByteHopsMode fires mc-hide-1byte-hops-changed with the mode now in force', () => {
  const w = sandbox({ [MODE_KEY]: 'show' });
  w.MC_set1ByteHopsDefault('hide');
  w.MC_clear1ByteHopsMode();
  assert(JSON.stringify(w.__changes()) === '["hide"]', 'events ' + JSON.stringify(w.__changes()));
  const e = w.__events.filter(x => x.type === 'mc-hide-1byte-hops-changed').pop();
  assert(e.detail.value === true, 'detail.value must say hide: ' + JSON.stringify(e.detail));
});

test('F3 a changed operator default fires mc-1byte-hops-default-changed, also behind a stored pick', () => {
  const w = sandbox({ [MODE_KEY]: 'show' });
  w.MC_set1ByteHopsDefault('hide');
  w.MC_set1ByteHopsDefault('hide');
  w.MC_set1ByteHopsDefault('junk');
  assert(JSON.stringify(defaultChanges(w)) === '["hide","mark"]', 'default events ' + JSON.stringify(defaultChanges(w)));
  assert(w.__changes().length === 0, 'the stored pick hides the mode change: ' + JSON.stringify(w.__changes()));
  const m = sandbox({});
  m.MC_set1ByteHopsDefault('mark');
  assert(defaultChanges(m).length === 0, 'mark → mark fired ' + JSON.stringify(defaultChanges(m)));
});

test('F5 an early MC_CUSTOMIZER_CFG with a junk default is ignored: mark', () => {
  for (const junk of ['bogus', 'constructor', '__proto__', 'Hide', 3]) {
    const w = sandbox({}, (c) => { c.MC_CUSTOMIZER_CFG = { disabledTabs: [], oneByteHopsDefault: junk }; });
    assert(w.MC_get1ByteHopsDefault() === 'mark', JSON.stringify(junk) + ': default ' + w.MC_get1ByteHopsDefault());
    assert(w.MC_get1ByteHopsMode() === 'mark', JSON.stringify(junk) + ': mode ' + w.MC_get1ByteHopsMode());
  }
});

test('F5 a legacy "true" migrates on the first read under a load-time default (no earlier get)', () => {
  const store = { [LEGACY_KEY]: 'true' };
  const w = sandbox(store, (c) => { c.MC_CUSTOMIZER_CFG = { disabledTabs: [], oneByteHopsDefault: 'show' }; });
  assert(w.MC_get1ByteHopsMode() === 'hide', 'got ' + w.MC_get1ByteHopsMode());
  assert(store[MODE_KEY] === 'hide' && !(LEGACY_KEY in store), JSON.stringify(store));
});

// The customizer, run in tests/customizer-sandbox.js with the real
// hop-filter.js over the same window and localStorage, as on the page.
function customizerPage(stored) {
  const { loadCustomizer } = require('./tests/customizer-sandbox');
  const listeners = {};
  const fired = [];
  const els = {};
  const p = loadCustomizer({
    setup(ctx) {
      Object.assign(ctx.localStorage._data, stored || {});
      ctx.window.addEventListener = (t, fn) => { (listeners[t] = listeners[t] || []).push(fn); };
      ctx.window.dispatchEvent = (ev) => { fired.push(ev); (listeners[ev.type] || []).forEach(fn => fn(ev)); return true; };
      ctx.document.getElementById = (id) => els[id] || null;
    },
  });
  vm.runInContext(SRC, p.ctx, { filename: 'hop-filter.js' });
  p.fired = fired;
  // A select and reset button built from the rendered markup, standing in
  // for the open panel's DOM.
  p.mount = () => {
    const html = p.api.renderDisplay();
    const m = html.match(/<select id="cv2-hide-1byte-hops"[^>]*>([\s\S]*?)<\/select>/);
    assert(m, 'no 1-byte hop select in the Display tab');
    const sel = { value: null, options: [] };
    for (const o of m[1].matchAll(/<option value="(\w+)"( selected)?>([^<]*)<\/option>/g)) {
      sel.options.push({ value: o[1], textContent: o[3] });
      if (o[2]) sel.value = o[1];
    }
    const btn = html.match(/<button[^>]*id="cv2-hide-1byte-hops-reset"[^>]*>/);
    els['cv2-hide-1byte-hops'] = sel;
    els['cv2-hide-1byte-hops-reset'] = btn ? { hidden: / hidden[ >]/.test(btn[0]) } : null;
    return { html, sel, btn: els['cv2-hide-1byte-hops-reset'] };
  };
  p.shown = () => {
    const sel = els['cv2-hide-1byte-hops'];
    return {
      value: sel.value,
      defaults: sel.options.filter(o => /\(server default\)/.test(o.textContent)).map(o => o.value),
      reset: els['cv2-hide-1byte-hops-reset'] && !els['cv2-hide-1byte-hops-reset'].hidden,
    };
  };
  return p;
}

test('F1 the customizer offers "Use server default" only while a pick is stored', () => {
  const fresh = customizerPage({});
  fresh.ctx.window.MC_set1ByteHopsDefault('hide');
  const a = fresh.mount();
  assert(a.btn, 'no reset button rendered: ' + a.html.slice(a.html.indexOf('cv2-hide-1byte-hops'), a.html.indexOf('cv2-hide-1byte-hops') + 600));
  assert(a.btn.hidden, 'a visitor who follows the default needs no reset');
  const picked = customizerPage({ [MODE_KEY]: 'show' });
  picked.ctx.window.MC_set1ByteHopsDefault('hide');
  const b = picked.mount();
  assert(b.btn && !b.btn.hidden, 'a stored pick must offer the way back');
  assert(/data-cv2-hide-1byte-hops-reset[^>]*>[^<]*Use server default</.test(b.html), 'button text');
});

test('F2 Reset All tells the page that the 1-byte hop mode changed', () => {
  const p = customizerPage({ [MODE_KEY]: 'show' });
  p.ctx.window.MC_set1ByteHopsDefault('hide');
  p.api.resetAll();
  const modes = p.fired.filter(e => e.type === 'mc-hide-1byte-hops-changed').map(e => e.detail.mode);
  assert(JSON.stringify(modes) === '["hide"]', 'events ' + JSON.stringify(modes));
  assert(p.localStorage.getItem(MODE_KEY) === null && p.ctx.window.MC_get1ByteHopsMode() === 'hide', 'mode ' + p.ctx.window.MC_get1ByteHopsMode());
});

test('F3 an open customizer follows a client config that lands after it rendered', () => {
  // A visitor who follows the default: the selection and the label move.
  const p = customizerPage({});
  p.mount();
  assert(JSON.stringify(p.shown()) === '{"value":"mark","defaults":["mark"],"reset":false}', 'before ' + JSON.stringify(p.shown()));
  p.ctx.window.MC_set1ByteHopsDefault('hide');
  assert(JSON.stringify(p.shown()) === '{"value":"hide","defaults":["hide"],"reset":false}', 'after ' + JSON.stringify(p.shown()));
  // A stored pick: the selection stays, the label still moves.
  const s = customizerPage({ [MODE_KEY]: 'show' });
  s.mount();
  s.ctx.window.MC_set1ByteHopsDefault('hide');
  assert(JSON.stringify(s.shown()) === '{"value":"show","defaults":["hide"],"reset":true}', 'stored ' + JSON.stringify(s.shown()));
});

test('F1 the reset button and a pick keep the open select in step', () => {
  const p = customizerPage({});
  p.ctx.window.MC_set1ByteHopsDefault('hide');
  p.mount();
  p.ctx.window.MC_set1ByteHopsMode('show');
  assert(JSON.stringify(p.shown()) === '{"value":"show","defaults":["hide"],"reset":true}', 'after a pick ' + JSON.stringify(p.shown()));
  p.ctx.window.MC_clear1ByteHopsMode();
  assert(JSON.stringify(p.shown()) === '{"value":"hide","defaults":["hide"],"reset":false}', 'after the reset ' + JSON.stringify(p.shown()));
});

console.log(`\n#912 M2: ${passed} passed, ${failed} failed`);
process.exit(failed ? 1 : 0);
