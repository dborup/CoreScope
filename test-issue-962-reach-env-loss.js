/* test-issue-962-reach-env-loss.js — the environment loss of the Predicted
 * reach link budget (#962), in public/coverage.js, without a browser.
 *
 * Covers:
 *   1. budgetOf with the new term: power + both gains − cable loss −
 *      sensitivity − environment loss; the state, else the operator's
 *      value (the client config's coverage.environmentLossDb), else 20 dB;
 *   2. the link: renv round-trips, is bounded 0–60 and a link without it
 *      uses the operator's value; the request never carries it;
 *   3. an environment loss of 0 gives today's colour table, shares and
 *      node classes byte for byte, and 20 dB moves every class by 20 dB.
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

test('the default budget 158 dB less the default 20 dB environment loss is 138 dB', () => {
  assert.strictEqual(PR.DEFAULT_ENV_LOSS_DB, 20);
  assert.strictEqual(PR.budgetOf({}, PR.operatorRadio({ radio: RADIO, environmentLossDb: 20 })), 138);
  assert.strictEqual(PR.budgetOf({}, PR.operatorRadio({ radio: RADIO })), 138, 'no operator value: 20 dB');
  assert.strictEqual(PR.budgetOf({}, null), 138, 'no config at all: 20 dB');
});

test('the operator value is the budget term, and the state overrides it', () => {
  const op = PR.operatorRadio({ radio: RADIO, environmentLossDb: 35 });
  assert.strictEqual(PR.budgetOf({}, op), 123);
  assert.strictEqual(PR.budgetOf({ env: 5 }, op), 153);
  assert.strictEqual(PR.budgetOf({ env: 0 }, op), 158, 'a state of 0 is 0, not the operator value');
  assert.strictEqual(PR.budgetOf({}, PR.operatorRadio({ radio: RADIO, environmentLossDb: 0 })), 158, 'an operator 0 is 0, not 20');
  assert.strictEqual(PR.budgetOf({ power: 10, loss: 4, env: 12 },
    PR.operatorRadio({ radio: { txPowerDbm: 20, antennaGainDbi: 5, rxGainDbi: 3, lossDb: 1, sensitivityDbm: -125 }, environmentLossDb: 30 })), 127);
});

test('operatorRadio keeps the radio block and adds only the environment loss', () => {
  const op = PR.operatorRadio({ radio: RADIO, environmentLossDb: 7 });
  assert.deepStrictEqual(J(op), Object.assign({}, RADIO, { environmentLossDb: 7 }));
  assert.deepStrictEqual(J(RADIO), { txPowerDbm: 22, antennaGainDbi: 3, rxGainDbi: 3, lossDb: 0, sensitivityDbm: -130 }, 'radio mutated');
  assert.strictEqual(PR.budgetOf({}, PR.operatorRadio({ radio: RADIO, environmentLossDb: 'x' })), 138, 'junk operator value: 20 dB');
  assert.strictEqual(PR.budgetOf({}, PR.operatorRadio(null)), 138);
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

test('20 dB shifts every class by exactly 20 dB, in the table and in the list', () => {
  const op = PR.operatorRadio({ radio: RADIO, environmentLossDb: 20 });
  assert.deepStrictEqual(Array.from(PR.buildLut(PR.budgetOf({}, op), META, COLORS)), Array.from(PR.buildLut(138, META, COLORS)));
  const rows = PR.reachRows({ nodes: [{ pubkey: '1'.repeat(64), lossDb: 120 }, { pubkey: '2'.repeat(64), lossDb: 130 },
    { pubkey: '3'.repeat(64), lossDb: 136 }, { pubkey: '4'.repeat(64), lossDb: 140 }] }, PR.budgetOf({}, op));
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

console.log('\n' + passed + ' passed, ' + failed + ' failed\n');
process.exit(failed ? 1 : 0);
