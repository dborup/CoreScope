/**
 * #968 M3 (AGENTS rule 8) — Customize → Display → Predicted reach: the
 * viewer's default environment loss.
 *
 * The real public/customize-v2.js and public/coverage.js run in vm
 * sandboxes; nothing is copied.
 *   1. the `reach` section: a known object section, stored values by the
 *      module's rule (PredictedReach.SETTINGS), import validation with the
 *      bounds named, export round trip, the Display badge, Reset All;
 *   2. the rendered control: shown only while the server offers the
 *      predicted reach, its bounds read from PredictedReach.BOUNDS.env (not
 *      a copy), empty meaning the operator's value, the override dot;
 *   3. the docs name the key.
 * The field's value is reachInputValue (the change handler's parse); the
 * handler itself is exercised in test-issue-962-reach-env-loss-e2e.js.
 * Usage: node test-968-m3-customizer-reach.js
 */
'use strict';
const fs = require('fs');
const path = require('path');
const assert = require('assert');
const { loadCustomizer } = require('./tests/customizer-sandbox');

let passed = 0, failed = 0;
const tests = [];
function test(name, fn) { tests.push([name, fn]); }
const plain = (v) => JSON.parse(JSON.stringify(v === undefined ? null : v));
const eq = (a, b, m) => assert.deepStrictEqual(plain(a), b, m);

const COVERAGE = { enabled: true, radio: { txPowerDbm: 22 }, environmentLossDb: 25 };

function load(o) {
  o = o || {};
  return loadCustomizer({
    scripts: o.noRule ? [] : ['coverage.js'],
    setup: (ctx) => { ctx.window.MC_COVERAGE_CFG = o.cfg === undefined ? COVERAGE : o.cfg; },
  });
}
const PR = (c) => c.ctx.window.PredictedReach;

// ── 1. The section ──────────────────────────────────────────────────────────

test('reach is a known object section; its key is the module rule', () => {
  const c = load();
  assert.ok(c.api.VALID_SECTIONS.indexOf('reach') >= 0);
  eq(c.api.reachSettings({}), { environmentLossDb: null });
  eq(c.api.reachSettings({ reach: { environmentLossDb: 40 } }), { environmentLossDb: 40 });
  assert.strictEqual(PR(c).SETTINGS.SECTION, 'reach', 'the section name is the module\'s');
  assert.strictEqual(c.api.validateShape({ reach: { environmentLossDb: 40 } }).valid, true);
  assert.strictEqual(c.api.validateShape({ reach: 40 }).valid, false, 'not checked as an object section');
});

test('writeOverrides keeps a valid value and drops the rest, by the module rule', () => {
  const c = load();
  c.api.writeOverrides({ reach: { environmentLossDb: 40, txPowerDbm: 10 } });
  eq(c.api.readOverrides(), { reach: { environmentLossDb: 40 } });
  for (const bad of [61, -1, '40', 12.55, true]) {
    const d = load();
    d.api.writeOverrides({ reach: { environmentLossDb: bad } });
    eq(d.api.readOverrides(), {}, JSON.stringify(bad));
    assert.ok(d.warnings.some((w) => /reach\.environmentLossDb/.test(w)), 'a rejected value is logged: ' + bad);
  }
  const z = load();
  z.api.writeOverrides({ reach: { environmentLossDb: 0 } });
  eq(z.api.readOverrides(), { reach: { environmentLossDb: 0 } }, '0 dB is a value, not "unset"');
});

test('without coverage.js on the page no value is accepted and nothing renders', () => {
  const c = load({ noRule: true });
  c.api.writeOverrides({ reach: { environmentLossDb: 40 } });
  eq(c.api.readOverrides(), {});
  assert.strictEqual(c.api.reachSettings({}), null);
  assert.strictEqual(c.api.renderReachSettings(), '');
});

test('validateShape names the bad value and the bounds from BOUNDS.env; null and unknown keys are no error', () => {
  const c = load();
  let res = c.api.validateShape({ reach: { environmentLossDb: 70, modelX: 1 } });
  assert.strictEqual(res.valid, false);
  assert.ok(/reach\.environmentLossDb = "70".*0-60/.test(res.errors.join('\n')), res.errors.join('\n'));
  assert.ok(!/modelX/.test(res.errors.join('\n')) && c.warnings.some((w) => /Unknown reach key.*modelX/.test(w)));
  assert.strictEqual(c.api.validateShape({ reach: { environmentLossDb: null } }).valid, true, 'the documented default written out');
  PR(c).BOUNDS.env = [5, 50, 1];
  res = c.api.validateShape({ reach: { environmentLossDb: 55 } });
  assert.ok(!res.valid && /5-50/.test(res.errors.join('\n')), 'bounds not read from BOUNDS.env: ' + res.errors.join('\n'));
  assert.ok(!load({ noRule: true }).api.validateShape({ reach: { environmentLossDb: 40 } }).valid, 'accepted without the module');
});

test('export round trip; the effective config carries the value the map reads', () => {
  const delta = { reach: { environmentLossDb: 40 } };
  const a = load();
  a.api.writeOverrides(delta);
  const exported = a.api.readOverrides();
  eq(exported, delta);
  const b = load();
  assert.strictEqual(b.api.validateShape(exported).valid, true);
  b.api.writeOverrides(exported);
  eq(b.api.readOverrides(), delta);
  const eff = b.api.computeEffective({}, exported);
  eq(PR(b).SETTINGS.resolve(eff.reach), { environmentLossDb: 40 });
  assert.strictEqual(PR(b).defaultRadio(COVERAGE, eff.reach).environmentLossDb, 40);
});

test('the Display badge counts the section; Reset All clears it', () => {
  const c = load();
  c.api.init({});
  const before = c.api.countOverrides('reach');
  c.api.writeOverrides({ reach: { environmentLossDb: 40 } });
  eq([before, c.api.countOverrides('reach')], [0, 1]);
  c.api.resetAll();
  eq(c.api.readOverrides(), {});
});

// ── 2. The control ──────────────────────────────────────────────────────────

const input = (html) => (html.match(/<input[^>]*data-cv2-reach="environmentLossDb"[^>]*>/) || [''])[0];

test('control: shown only while the server offers the predicted reach', () => {
  assert.ok(input(load().api.renderReachSettings()), 'no input');
  assert.strictEqual(load({ cfg: null }).api.renderReachSettings(), '', 'shown with coverage off');
  assert.strictEqual(load({ cfg: { enabled: false } }).api.renderReachSettings(), '', 'shown with coverage disabled');
});

test('control: bounds read from PredictedReach.BOUNDS.env, not a copy', () => {
  const c = load();
  let i = input(c.api.renderReachSettings());
  assert.ok(/type="number"/.test(i) && /min="0"/.test(i) && /max="60"/.test(i) && /step="1"/.test(i), i);
  PR(c).BOUNDS.env = [5, 50, 1];
  i = input(c.api.renderReachSettings());
  assert.ok(/min="5"/.test(i) && /max="50"/.test(i), 'bounds copied: ' + i);
});

test('control: empty means the operator\'s value, which the placeholder and the hint name', () => {
  const c = load({ cfg: Object.assign({}, COVERAGE, { environmentLossDb: 35 }) });
  const html = c.api.renderReachSettings();
  const i = input(html);
  assert.ok(/value=""/.test(i) && /placeholder="35"/.test(i), i);
  assert.ok(/server’s 35 dB|server's 35 dB/.test(html), 'the hint does not name the server value');
  assert.ok(/shared link’s own value still wins/.test(html), 'the hint does not say a link still wins');
  assert.ok(!/renv/.test(html), 'the hint names the renv parameter');
  assert.ok(/placeholder="25"/.test(input(load({ cfg: { enabled: true } }).api.renderReachSettings())), 'no operator value: 25');
});

// N1 on #976: the field checks only the decimal grammar (the module's
// isDecimal); BOUNDS.env alone decides the range, so the field never caps a
// value the rule allows (it once capped at two digits, 99.9).
test('control: the typed value — BOUNDS.env decides alone, no digit cap in the field', () => {
  const c = load();
  const val = (raw) => c.api.reachInputValue('environmentLossDb', raw);
  eq([val(''), val('  '), val('40'), val(' 40 '), val('0'), val('12.5'), val('60')], [null, null, 40, 40, 0, 12.5, 60]);
  for (const bad of ['61', '100', '-1', '12.55', '1e1', '4O', 'abc', '.5']) {
    assert.strictEqual(val(bad), undefined, bad);
  }
  PR(c).BOUNDS.env = [0, 120, 1];
  eq([val('100'), val('120'), val('99.5'), val('100.5')], [100, 120, 99.5, 100.5], 'the field caps what BOUNDS.env allows');
  assert.strictEqual(val('120.5'), undefined, 'over the stubbed bound');
  PR(c).BOUNDS.env = [5, 50, 1];
  assert.deepStrictEqual([val('4'), val('5'), val('51')], [undefined, 5, undefined], 'the lower bound is the rule\'s too');
  assert.strictEqual(load({ noRule: true }).api.reachInputValue('environmentLossDb', '40'), undefined, 'accepted without the module');
});

test('control: a stored value shows with the override dot', () => {
  const c = load();
  c.api.writeOverrides({ reach: { environmentLossDb: 40 } });
  c.ctx.window.SITE_CONFIG = c.api.computeEffective({}, c.api.readOverrides());
  const html = c.api.renderReachSettings();
  assert.ok(/value="40"/.test(input(html)), input(html));
  assert.ok(/data-reset-s="reach"/.test(html), 'no reset dot');
});

// ── 3. Docs ─────────────────────────────────────────────────────────────────

test('the docs name the key, its bounds and the precedence', () => {
  const custom = fs.readFileSync(path.join(__dirname, 'docs/CUSTOMIZATION.md'), 'utf8');
  assert.ok(/reach\.environmentLossDb/.test(custom), 'CUSTOMIZATION.md misses reach.environmentLossDb');
  const line = custom.split('\n').find((l) => /reach\.environmentLossDb/.test(l));
  assert.ok(/0 to 60/.test(line) && /renv/.test(line) && /map\.coverage\.environmentLossDb/.test(line), line);
});

for (const [name, fn] of tests) {
  try { fn(); passed++; console.log('  ✅ ' + name); } catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}
console.log('\n' + passed + ' passed, ' + failed + ' failed');
process.exit(failed ? 1 : 0);
