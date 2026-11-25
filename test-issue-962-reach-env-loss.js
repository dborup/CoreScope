/* test-issue-962-reach-env-loss.js — the environment loss of the Predicted
 * reach link budget (#962), in public/coverage.js, without a browser.
 *
 * Covers:
 *   1. budgetOf with the new term: power + both gains − cable loss −
 *      sensitivity − environment loss; the state, else the operator's
 *      value (the client config's coverage.environmentLossDb), else 25 dB;
 *   2. the link: renv round-trips, is bounded 0–60 and a link without it
 *      uses the operator's value; the request never carries it;
 *   3. an environment loss of 0 gives today's colour table, shares and
 *      node classes byte for byte, and 25 dB moves every class by 25 dB;
 *   4. #968 M3, the viewer's default (Customize → Display → Predicted
 *      reach): SETTINGS and the exported BOUNDS, the precedence renv >
 *      viewer > operator > 25 dB, invalid viewer values ignored, and in the
 *      real panel (create() in a fake DOM) the slider's start, the Reset's
 *      label and its landing on the viewer's default.
 *
 * Runs via: node test-issue-962-reach-env-loss.js
 */
'use strict';
const fs = require('fs');
const path = require('path');
const vm = require('vm');
const assert = require('assert');
const { makeElement } = require('./tests/map-overlay-sandbox');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

const SRC = fs.readFileSync(path.join(__dirname, 'public', 'coverage.js'), 'utf8');
function load() {
  const ctx = {
    window: null, document: { createElement: makeElement }, URLSearchParams, Uint8ClampedArray, Uint32Array,
    Uint8Array, Math, Number, String, Object, Array, isFinite, encodeURIComponent, console,
  };
  ctx.window = ctx;
  vm.createContext(ctx);
  vm.runInContext(SRC, ctx);
  return ctx.PredictedReach;
}
const PR = load();
const J = (v) => JSON.parse(JSON.stringify(v));
const AT = 'reach=56.1629,10.2039';
const RADIO = { txPowerDbm: 22, antennaGainDbi: 3, rxGainDbi: 3, lossDb: 0, sensitivityDbm: -130 };

console.log('\n=== #962 Predicted reach: the environment loss ===\n');

// ── 1. The budget ────────────────────────────────────────────────────────

test('the default budget 158 dB less the default 25 dB environment loss is 133 dB', () => {
  assert.strictEqual(PR.DEFAULT_ENV_LOSS_DB, 25);
  assert.strictEqual(PR.budgetOf({}, PR.operatorRadio({ radio: RADIO, environmentLossDb: 25 })), 133);
  assert.strictEqual(PR.budgetOf({}, PR.operatorRadio({ radio: RADIO })), 133, 'no operator value: 25 dB');
  assert.strictEqual(PR.budgetOf({}, null), 133, 'no config at all: 25 dB');
});

test('the operator value is the budget term, and the state overrides it', () => {
  const op = PR.operatorRadio({ radio: RADIO, environmentLossDb: 35 });
  assert.strictEqual(PR.budgetOf({}, op), 123);
  assert.strictEqual(PR.budgetOf({ env: 5 }, op), 153);
  assert.strictEqual(PR.budgetOf({ env: 0 }, op), 158, 'a state of 0 is 0, not the operator value');
  assert.strictEqual(PR.budgetOf({}, PR.operatorRadio({ radio: RADIO, environmentLossDb: 0 })), 158, 'an operator 0 is 0, not 25');
  assert.strictEqual(PR.budgetOf({ power: 10, loss: 4, env: 12 },
    PR.operatorRadio({ radio: { txPowerDbm: 20, antennaGainDbi: 5, rxGainDbi: 3, lossDb: 1, sensitivityDbm: -125 }, environmentLossDb: 30 })), 127);
});

test('operatorRadio keeps the radio block and adds only the environment loss', () => {
  const op = PR.operatorRadio({ radio: RADIO, environmentLossDb: 7 });
  assert.deepStrictEqual(J(op), Object.assign({}, RADIO, { environmentLossDb: 7 }));
  assert.deepStrictEqual(J(RADIO), { txPowerDbm: 22, antennaGainDbi: 3, rxGainDbi: 3, lossDb: 0, sensitivityDbm: -130 }, 'radio mutated');
  assert.strictEqual(PR.budgetOf({}, PR.operatorRadio({ radio: RADIO, environmentLossDb: 'x' })), 133, 'junk operator value: 25 dB');
  assert.strictEqual(PR.budgetOf({}, PR.operatorRadio(null)), 133);
});

// ── 2. The link and the request ──────────────────────────────────────────

test('renv round-trips, as the other budget fields do', () => {
  assert.strictEqual(PR.KEYS.env, 'renv');
  const st = PR.parseState(AT + '&renv=12.5&rpwr=14');
  assert.deepStrictEqual(J(st), { at: { kind: 'point', lat: 56.1629, lon: 10.2039 }, power: 14, env: 12.5 });
  const ser = PR.serializeState(st);
  assert.strictEqual(ser.renv, '12.5');
  assert.deepStrictEqual(J(PR.parseState(new URLSearchParams(ser).toString())), J(st));
  assert.strictEqual(PR.serializeState(PR.parseState(AT + '&renv=0')).renv, '0', 'renv=0 is kept');
});

test('renv is bounded 0–60 dB; anything else is dropped', () => {
  for (const ok of ['0', '60', '20', '33.3']) assert.strictEqual(PR.parseState(AT + '&renv=' + ok).env, Number(ok), ok);
  for (const bad of ['-1', '-0.1', '60.1', '61', '100', '1e1', 'NaN', 'Infinity', '0x10', '', 'abc']) {
    assert.ok(!('env' in PR.parseState(AT + '&renv=' + bad)), bad);
  }
});

test('a link without renv uses the operator value', () => {
  const st = PR.parseState(AT + '&rpwr=14');
  assert.ok(!('env' in st));
  assert.strictEqual(PR.budgetOf(st, PR.operatorRadio({ radio: RADIO, environmentLossDb: 25 })), 125);
  assert.ok(!('renv' in PR.serializeState(st)), 'renv written for an unset field');
});

test('withReachParams replaces renv; the request never carries the environment loss', () => {
  const h = PR.withReachParams('#/map?' + AT + '&renv=30&view=1', PR.serializeState({ at: { kind: 'point', lat: 55.5, lon: 9.25 } }));
  assert.ok(!/renv/.test(h) && /view=1/.test(h), h);
  const url = PR.requestURL(PR.parseState(AT + '&renv=37&rkm=10'));
  assert.ok(!/renv|env|37/.test(url), url);
  assert.ok(PR.sameGeometry(PR.parseState(AT + '&renv=37'), PR.parseState(AT)), 'renv changed the geometry');
});

// ── 3. 0 dB is today's prediction, byte for byte ─────────────────────────

const META = { lossBaseDb: 60, lossStepDb: 0.5, noData: 255 };
const COLORS = { good: [21, 128, 61], marginal: [234, 179, 8], weak: [234, 88, 12] };
// The budget before #962, written out: the oracle.
function todayBudget(st, r) {
  const v = (f, k) => (st && typeof st[f] === 'number' ? st[f] : r[k]);
  return v('power', 'txPowerDbm') + v('txGain', 'antennaGainDbi') + v('rxGain', 'rxGainDbi') - v('loss', 'lossDb') - v('sens', 'sensitivityDbm');
}
const RADIOS = [RADIO, { txPowerDbm: 30, antennaGainDbi: 20, rxGainDbi: 20, lossDb: 0, sensitivityDbm: -150 },
  { txPowerDbm: -10, antennaGainDbi: -10, rxGainDbi: -10, lossDb: 30, sensitivityDbm: -80 },
  { txPowerDbm: 14, antennaGainDbi: 2.5, rxGainDbi: 0, lossDb: 1.5, sensitivityDbm: -124 }];
const STATES = [{}, { power: 17 }, { sens: -111, loss: 3 }];
const ALL_BYTES = Uint8Array.from({ length: 256 * 3 }, (_, i) => i % 256);
const NODES = Array.from({ length: 120 }, (_, i) => ({ pubkey: (i % 16).toString(16).repeat(64), name: 'n' + i, role: 'repeater', lossDb: 90 + i * 0.75 }));

test('an operator environment loss of 0 gives today\'s classes byte for byte', () => {
  for (const r of RADIOS) for (const st of STATES) {
    const now = PR.budgetOf(st, PR.operatorRadio({ radio: r, environmentLossDb: 0 }));
    const was = todayBudget(st, r);
    assert.strictEqual(now, was);
    assert.deepStrictEqual(Array.from(PR.buildLut(now, META, COLORS)), Array.from(PR.buildLut(was, META, COLORS)));
    const a = new Uint8ClampedArray(ALL_BYTES.length * 4), b = new Uint8ClampedArray(ALL_BYTES.length * 4);
    PR.paint(a, ALL_BYTES, PR.buildLut(now, META, COLORS));
    PR.paint(b, ALL_BYTES, PR.buildLut(was, META, COLORS));
    assert.ok(Buffer.from(a.buffer).equals(Buffer.from(b.buffer)), 'painted pixels differ');
    assert.deepStrictEqual(J(PR.classShares(ALL_BYTES, now, META)), J(PR.classShares(ALL_BYTES, was, META)));
    assert.deepStrictEqual(J(PR.reachRows({ nodes: NODES }, now)), J(PR.reachRows({ nodes: NODES }, was)));
  }
});

test('a link\'s renv=0 gives today\'s classes over any operator value', () => {
  const st = PR.parseState(AT + '&renv=0');
  assert.strictEqual(PR.budgetOf(st, PR.operatorRadio({ radio: RADIO, environmentLossDb: 45 })), todayBudget(st, RADIO));
});

test('25 dB shifts every class by exactly 25 dB, in the table and in the list', () => {
  const op = PR.operatorRadio({ radio: RADIO, environmentLossDb: 25 });
  assert.deepStrictEqual(Array.from(PR.buildLut(PR.budgetOf({}, op), META, COLORS)), Array.from(PR.buildLut(133, META, COLORS)));
  const rows = PR.reachRows({ nodes: [{ pubkey: '1'.repeat(64), lossDb: 115 }, { pubkey: '2'.repeat(64), lossDb: 125 },
    { pubkey: '3'.repeat(64), lossDb: 131 }, { pubkey: '4'.repeat(64), lossDb: 135 }] }, PR.budgetOf({}, op));
  assert.deepStrictEqual(J(rows.map(r => [r.margin, r.cls])), [[18, 'good'], [8, 'marginal'], [2, 'weak'], [-2, 'none']]);
});

test('the panel field is a budget field: built as DOM with its hint and a reset, no HTML string', () => {
  const i = SRC.indexOf("field(bud, 'env'");
  assert.ok(i > 0, 'no environment loss field in the link-budget group');
  const body = SRC.slice(i, SRC.indexOf('ui.budget = el(', i));
  assert.ok(/buildings, trees and other clutter the terrain model does not include/i.test(body), 'hint');
  assert.ok(/reach-env-reset/.test(body), 'reset');
  assert.ok(!/innerHTML|insertAdjacentHTML|outerHTML/.test(body), 'an HTML sink in the field code');
});

// ── 4. #968 M3: the viewer's default (Customize → Display) ─────────────

console.log('\n=== #968 M3 the viewer\'s default environment loss ===\n');

test('BOUNDS is exported and env is the server\'s 0–60 dB', () => {
  assert.deepStrictEqual(J(PR.BOUNDS.env), [0, 60, 1]);
});

test('SETTINGS: the customizer\'s reach section, one key, empty by default (the operator\'s value)', () => {
  assert.strictEqual(PR.SETTINGS.SECTION, 'reach');
  assert.deepStrictEqual(J(PR.SETTINGS.KEYS), ['environmentLossDb']);
  assert.deepStrictEqual(J(PR.SETTINGS.DEFAULTS), { environmentLossDb: null });
});

test('SETTINGS.valid: a number inside BOUNDS.env in the renv grammar; nothing else', () => {
  for (const ok of [0, 60, 40, 12.5, 33.3]) assert.ok(PR.SETTINGS.valid('environmentLossDb', ok), String(ok));
  for (const bad of [-1, -0.1, 60.1, 61, 1000, 12.55, NaN, Infinity, -Infinity, '40', '', null, undefined, true, {}, [40]]) {
    assert.ok(!PR.SETTINGS.valid('environmentLossDb', bad), JSON.stringify(bad) + ' accepted');
  }
  assert.ok(!PR.SETTINGS.valid('txPowerDbm', 10), 'another key accepted');
});

test('SETTINGS.valid reads BOUNDS.env, not a copy', () => {
  const was = PR.BOUNDS.env;
  try {
    PR.BOUNDS.env = [5, 50, 1];
    assert.ok(PR.SETTINGS.valid('environmentLossDb', 50) && PR.SETTINGS.valid('environmentLossDb', 5));
    assert.ok(!PR.SETTINGS.valid('environmentLossDb', 4) && !PR.SETTINGS.valid('environmentLossDb', 55));
  } finally { PR.BOUNDS.env = was; }
});

test('SETTINGS.resolve: a valid value, else null; never throws', () => {
  assert.deepStrictEqual(J(PR.SETTINGS.resolve({ environmentLossDb: 40 })), { environmentLossDb: 40 });
  assert.deepStrictEqual(J(PR.SETTINGS.resolve({ environmentLossDb: 0 })), { environmentLossDb: 0 });
  for (const sec of [{}, { environmentLossDb: 99 }, { environmentLossDb: '40' }, { other: 1 }, null, undefined, 'x', 7]) {
    assert.deepStrictEqual(J(PR.SETTINGS.resolve(sec)), { environmentLossDb: null }, JSON.stringify(sec));
  }
});

// The environment loss a budget used: every other term zeroed, so the
// budget is 0 − env.
const envOf = (st, cfg, viewer) => 0 - PR.budgetOf(Object.assign({ power: 0, txGain: 0, rxGain: 0, loss: 0, sens: 0 }, st), PR.defaultRadio(cfg, viewer));

test('precedence: renv > the viewer\'s default > the operator\'s value > 25 dB', () => {
  const op = { radio: RADIO, environmentLossDb: 35 };
  assert.strictEqual(envOf({}, null, null), 25, 'nothing set');
  assert.strictEqual(envOf({}, op, null), 35, 'operator');
  assert.strictEqual(envOf({}, op, { environmentLossDb: 40 }), 40, 'viewer over operator');
  assert.strictEqual(envOf({}, null, { environmentLossDb: 40 }), 40, 'viewer without operator');
  assert.strictEqual(envOf({ env: 10 }, op, { environmentLossDb: 40 }), 10, 'renv over viewer');
  assert.strictEqual(envOf({ env: 0 }, op, { environmentLossDb: 40 }), 0, 'renv=0 over viewer');
  assert.strictEqual(envOf({}, op, { environmentLossDb: 0 }), 0, 'a viewer 0 is 0, not the operator\'s');
  assert.strictEqual(PR.budgetOf(PR.parseState(AT), PR.defaultRadio(op, { environmentLossDb: 40 })), 118, 'a link without renv');
  assert.strictEqual(PR.budgetOf(PR.parseState(AT + '&renv=10'), PR.defaultRadio(op, { environmentLossDb: 40 })), 148, 'a link with renv');
});

test('an invalid viewer value falls back to the operator\'s, else 25 dB', () => {
  const op = { radio: RADIO, environmentLossDb: 35 };
  for (const v of [-1, 60.5, 99, NaN, '40', null, true, {}]) {
    assert.strictEqual(envOf({}, op, { environmentLossDb: v }), 35, JSON.stringify(v));
    assert.strictEqual(envOf({}, null, { environmentLossDb: v }), 25, JSON.stringify(v));
  }
  assert.strictEqual(envOf({}, op, 'junk'), 35);
});

test('defaultRadio keeps the radio block and mutates nothing', () => {
  const cfg = { radio: Object.assign({}, RADIO), environmentLossDb: 35 }, viewer = { environmentLossDb: 40 };
  assert.deepStrictEqual(J(PR.defaultRadio(cfg, viewer)), Object.assign({}, RADIO, { environmentLossDb: 40 }));
  assert.deepStrictEqual(J(cfg), { radio: RADIO, environmentLossDb: 35 });
  assert.deepStrictEqual(J(viewer), { environmentLossDb: 40 });
  assert.deepStrictEqual(J(PR.defaultRadio(cfg, null)), J(PR.operatorRadio(cfg)), 'no viewer: the operator\'s budget');
});

// The panel itself: the real create() in a small fake DOM, with a request
// that never answers (the budget needs none).
function fakeEl(tag) {
  const e = makeElement(tag);
  e.listeners = {};
  e.addEventListener = (t, fn) => { (e.listeners[t] = e.listeners[t] || []).push(fn); };
  e.removeEventListener = (t, fn) => { e.listeners[t] = (e.listeners[t] || []).filter(f => f !== fn); };
  e.removeChild = (c) => { e.children = e.children.filter(x => x !== c); return c; };
  e.classList.toggle = (c, on) => { if (on === undefined ? !e.classList.contains(c) : on) e.classList.add(c); else e.classList.remove(c); };
  e.fire = (t) => (e.listeners[t] || []).forEach(fn => fn({ type: t, target: e }));
  return e;
}
function byTestid(root, id) {
  if (root && root.attrs && root.attrs['data-testid'] === id) return root;
  for (const c of (root && root.children) || []) { const f = byTestid(c, id); if (f) return f; }
  return null;
}
function panelHarness(o) {
  const timers = [];
  const urls = [];
  const winListeners = {};
  const ctx = {
    document: { createElement: fakeEl, addEventListener() {}, removeEventListener() {}, activeElement: null },
    location: { hash: o.hash, pathname: '/', search: '' },
    history: { state: null, replaceState(_s, _t, url) { ctx.location.hash = url.slice(url.indexOf('#')); } },
    setTimeout: (fn) => { timers.push(fn); return timers.length; }, clearTimeout() {},
    requestAnimationFrame: (fn) => { fn(); return 1; },
    addEventListener: (t, fn) => { (winListeners[t] = winListeners[t] || []).push(fn); }, removeEventListener() {},
    URLSearchParams, Uint8ClampedArray, Uint32Array, Uint8Array, Math, Number, String, Object, Array, isFinite, encodeURIComponent, console,
  };
  ctx.window = ctx;
  vm.createContext(ctx);
  vm.runInContext(SRC, ctx);
  const map = { getContainer: () => fakeEl('div'), on() {}, off() {}, hasLayer: () => false, removeLayer() {} };
  const mount = fakeEl('div');
  let viewer = o.viewer;
  const tool = ctx.PredictedReach.create({
    map, mount, cfg: o.cfg,
    viewerSettings: () => viewer,
    fetchFn: (u) => { urls.push(u); return new Promise(() => {}); },
  });
  tool.open(ctx.PredictedReach.parseState(o.hash.split('?')[1]));
  const q = (id) => byTestid(mount, id);
  return {
    tool, urls,
    setViewer: (v) => { viewer = v; },
    themeRefresh: () => (winListeners['theme-refresh'] || []).forEach(fn => fn({ type: 'theme-refresh' })),
    flush: () => { while (timers.length) timers.shift()(); },
    hash: () => ctx.location.hash,
    read: () => ({ env: q('reach-env').value, out: q('reach-env-value').textContent, budget: q('reach-budget').textContent,
      resetDisabled: !!q('reach-env-reset').disabled, resetLabel: q('reach-env-reset').getAttribute('aria-label') }),
    reset: () => q('reach-env-reset').fire('click'),
    slide: (v) => { const s = q('reach-env'); s.value = String(v); s.fire('input'); },
  };
}
const OP25 = { enabled: true, radio: RADIO, environmentLossDb: 25 };

test('panel: with no renv the slider starts at the viewer\'s default, and Reset names it', () => {
  const h = panelHarness({ cfg: OP25, viewer: { environmentLossDb: 40 }, hash: '#/map?' + AT });
  const s = h.read();
  assert.deepStrictEqual([s.env, s.out, s.budget, s.resetDisabled], ['40', '40 dB', 'Maximum path loss 118 dB', true], JSON.stringify(s));
  assert.ok(/your default, 40 dB/.test(s.resetLabel), s.resetLabel);
  h.flush();
  assert.ok(!/renv/.test(h.hash()), 'renv written for the viewer\'s default: ' + h.hash());
});

test('panel: without a viewer default it is the operator\'s, and Reset says so', () => {
  const h = panelHarness({ cfg: Object.assign({}, OP25, { environmentLossDb: 35 }), viewer: undefined, hash: '#/map?' + AT });
  const s = h.read();
  assert.deepStrictEqual([s.env, s.budget], ['35', 'Maximum path loss 123 dB'], JSON.stringify(s));
  assert.ok(/server default, 35 dB/.test(s.resetLabel), s.resetLabel);
  const bad = panelHarness({ cfg: OP25, viewer: { environmentLossDb: 99 }, hash: '#/map?' + AT }).read();
  assert.deepStrictEqual([bad.env, /server default, 25 dB/.test(bad.resetLabel)], ['25', true], 'an out-of-range viewer value is ignored: ' + JSON.stringify(bad));
});

test('panel: a link\'s renv wins over the viewer\'s default; Reset drops it and lands on the viewer\'s default', () => {
  const h = panelHarness({ cfg: OP25, viewer: { environmentLossDb: 40 }, hash: '#/map?' + AT + '&renv=10' });
  let s = h.read();
  assert.deepStrictEqual([s.env, s.budget, s.resetDisabled], ['10', 'Maximum path loss 148 dB', false], JSON.stringify(s));
  h.flush();
  assert.ok(/[?&]renv=10(&|$)/.test(h.hash()), h.hash());
  h.reset();
  h.flush();
  s = h.read();
  assert.deepStrictEqual([s.env, s.out, s.budget, s.resetDisabled], ['40', '40 dB', 'Maximum path loss 118 dB', true], JSON.stringify(s));
  assert.ok(!('env' in h.tool.getState()), 'state.env kept after Reset');
  assert.ok(!/renv/.test(h.hash()), 'renv kept after Reset: ' + h.hash());
});

test('panel: a slider move then Reset returns to the viewer\'s default, with no request', () => {
  const h = panelHarness({ cfg: OP25, viewer: { environmentLossDb: 40 }, hash: '#/map?' + AT });
  const n = h.urls.length;
  h.slide(5);
  assert.strictEqual(h.read().budget, 'Maximum path loss 153 dB');
  h.reset();
  assert.deepStrictEqual([h.read().env, h.read().budget], ['40', 'Maximum path loss 118 dB']);
  assert.strictEqual(h.urls.length, n, 'a budget change asked the server');
  assert.ok(h.urls.every(u => !/env|renv/.test(u)), h.urls.join(' '));
});

test('panel: the viewer\'s default is read when used, so a customizer change applies at the next update', () => {
  const h = panelHarness({ cfg: OP25, viewer: { environmentLossDb: 40 }, hash: '#/map?' + AT + '&renv=10' });
  h.setViewer({ environmentLossDb: 50 });
  h.reset();
  const s = h.read();
  assert.deepStrictEqual([s.env, s.budget], ['50', 'Maximum path loss 108 dB'], JSON.stringify(s));
  assert.ok(/your default, 50 dB/.test(s.resetLabel), s.resetLabel);
  h.setViewer(undefined);
  h.slide(3);
  h.reset();
  assert.deepStrictEqual([h.read().env, /server default, 25 dB/.test(h.read().resetLabel)], ['25', true]);
});

test('panel: a customizer change (theme-refresh) moves an open panel without renv, and leaves a link\'s renv alone', () => {
  const h = panelHarness({ cfg: OP25, viewer: undefined, hash: '#/map?' + AT });
  h.setViewer({ environmentLossDb: 40 });
  h.themeRefresh();
  assert.deepStrictEqual([h.read().env, h.read().budget], ['40', 'Maximum path loss 118 dB']);
  h.flush();
  assert.ok(!/renv/.test(h.hash()), 'a customizer change wrote renv: ' + h.hash());
  const l = panelHarness({ cfg: OP25, viewer: undefined, hash: '#/map?' + AT + '&renv=10' });
  l.setViewer({ environmentLossDb: 40 });
  l.themeRefresh();
  assert.strictEqual(l.read().env, '10');
});

console.log('\n' + passed + ' passed, ' + failed + ' failed\n');
process.exit(failed ? 1 : 0);
