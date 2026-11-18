// #902: a node whose public key is a recently reporting observer is credited
// as Active, and companions/sensors say "Quiet" instead of "Degraded".
//
// Runs the REAL public/roles.js, my-mesh-observers.js and my-mesh-activity.js
// in one vm context. Expected cut-offs are independent constants, never
// computed from the code under test.
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');

const read = f => fs.readFileSync(path.join(__dirname, 'public', f), 'utf8');
const MIN = 60000;
const HOUR = 60 * MIN;
const NOW = Date.UTC(2026, 10, 18, 9);
const iso = ago => new Date(NOW - ago).toISOString();
let passed = 0;

async function load(config = {}) {
  const ctx = {
    console, Date, Map, Set,
    fetch: async () => ({ json: async () => config }),
    document: { readyState: 'complete', createElement: () => ({}), head: { appendChild() {} }, getElementById: () => null, addEventListener() {} },
  };
  ctx.window = ctx;
  for (const f of ['roles.js', 'my-mesh-observers.js', 'my-mesh-activity.js']) vm.runInNewContext(read(f), ctx);
  await ctx.MeshConfigReady;
  // The server always sends observerOnlineMs (config.go, default 60 min);
  // the stubbed config here does not, so the helper's own default applies.
  return ctx;
}
function check(label, fn) { fn(); passed++; console.log('PASS ' + label); }

(async () => {
  const r = await load();
  const A = r.MyMeshActivity;
  const ev = (role, advertAgo, observerAgo) => A.statusEvidence({
    node: { role },
    stats: { lastHeard: iso(advertAgo), lastAdvert: iso(advertAgo) },
    observerReportedAt: observerAgo == null ? undefined : iso(observerAgo),
    nowMs: NOW,
  });

  check('companion, advert 10 h old, not an observer: Quiet, not Degraded, neutral colour', () => {
    const e = ev('companion', 10 * HOUR);
    assert.equal(e.status, 'degraded', 'internal bucket is unchanged');
    assert.equal(e.label, 'Quiet');
    assert.equal(e.text, 'Quiet');
    assert.equal(e.quiet, true);
    assert.equal(e.cls, 'status-muted');
    assert.notEqual(e.cls, 'status-warn');
    assert.doesNotMatch(e.label + e.text, /Degraded/);
    assert.match(e.explanation, /not a fault/);
    assert.equal(e.observerAt, null);
    assert.equal(e.isObserver, false);
  });

  check('sensor is Quiet too', () => {
    const e = ev('sensor', 10 * HOUR);
    assert.equal(e.label, 'Quiet');
    assert.equal(e.cls, 'status-muted');
  });

  check('same companion as an observer that reported 1 min ago: Active, observer reporting evidence', () => {
    const e = ev('companion', 10 * HOUR, MIN);
    assert.equal(e.status, 'healthy');
    assert.equal(e.label, 'Active');
    assert.equal(e.source, 'observer');
    assert.equal(e.confident, true);
    assert.equal(e.cls, 'status-ok');
    assert.equal(e.glyph, 'ph-circle-fill');
    assert.equal(e.text, 'Active · observer reporting');
    assert.equal(e.observerAt, iso(MIN));
    assert.equal(e.statusAt, iso(MIN), 'the card time follows the evidence that carries the badge');
    assert.equal(e.heardAt, iso(10 * HOUR), 'radio evidence is still reported as it is');
    assert.equal(A.OBSERVER_ROW_LABEL, 'Observer reporting');
    assert.equal(e.explanation, A.EXPLAIN_OBSERVER);
    // The explanation must not claim an advert.
    assert.doesNotMatch(e.explanation, /follows this node.s own advert|recent advert/i);
  });

  check('an observer that reported 2 h ago (over the online threshold) is not credited', () => {
    const e = ev('companion', 10 * HOUR, 2 * HOUR);
    assert.equal(e.status, 'degraded');
    assert.equal(e.label, 'Quiet');
    assert.notEqual(e.source, 'observer');
    assert.equal(e.observerAt, null);
    assert.equal(e.isObserver, true);
    assert.equal(e.observerText, A.OBSERVER_NOT_COUNTED);
  });

  check('observer online boundary: threshold + 30 s clock tolerance, like the Observers page', () => {
    assert.ok(r.getObserverReportCredit(NOW - HOUR - 30000 + 1, NOW));
    assert.equal(r.getObserverReportCredit(NOW - HOUR - 30000, NOW), null);
    assert.equal(r.getObserverReportCredit(NOW + 30001, NOW), null, 'far-future report is not evidence');
    for (const bad of [null, undefined, '', 'nope', 0, -1, NaN, {}, true]) assert.equal(r.getObserverReportCredit(bad, NOW), null, String(bad));
  });

  check('a repeater is unchanged: Degraded label and warning colour', () => {
    const e = ev('repeater', 30 * HOUR);
    assert.equal(e.status, 'degraded');
    assert.equal(e.label, 'Degraded');
    assert.equal(e.cls, 'status-warn');
    assert.equal(e.quiet, false);
    const stale = ev('repeater', 30 * HOUR, 2 * HOUR);
    assert.equal(stale.label, 'Degraded', 'a stale observer report does not change it');
  });

  check('a room is unchanged', () => {
    const e = ev('room', 30 * HOUR);
    assert.equal(e.label, 'Degraded');
    assert.equal(e.cls, 'status-warn');
    assert.equal(ev('room', 2 * HOUR).label, 'Active');
    assert.equal(ev('room', 80 * HOUR).label, 'Silent');
  });

  check('observer credit applies to every role, including a silent one', () => {
    for (const role of ['repeater', 'room', 'companion', 'sensor']) {
      const e = ev(role, 100 * HOUR, 5 * MIN);
      assert.equal(e.status, 'healthy', role);
      assert.equal(e.source, 'observer', role);
    }
    // No radio evidence at all, but the observer reports: still Active.
    const none = A.statusEvidence({ node: { role: 'companion' }, stats: { lastHeard: null, lastAdvert: null }, observerReportedAt: iso(MIN), nowMs: NOW });
    assert.equal(none.status, 'healthy');
    assert.equal(none.source, 'observer');
  });

  check('a fresh advert keeps advert as the evidence even when the observer also reports', () => {
    const e = ev('companion', 10 * MIN, MIN);
    assert.equal(e.status, 'healthy');
    assert.equal(e.source, 'advert');
  });

  check('shared helpers: one label and class per role/status', () => {
    const cases = [
      ['companion', 'healthy', 'Active', 'status-ok'], ['companion', 'degraded', 'Quiet', 'status-muted'], ['companion', 'silent', 'Silent', 'status-err'],
      ['sensor', 'degraded', 'Quiet', 'status-muted'],
      ['repeater', 'degraded', 'Degraded', 'status-warn'], ['room', 'degraded', 'Degraded', 'status-warn'],
      ['repeater', 'healthy', 'Active', 'status-ok'], ['room', 'silent', 'Silent', 'status-err'],
    ];
    for (const [role, status, label, cls] of cases) {
      assert.equal(r.getHealthLabel(role, status), label, role + '/' + status);
      assert.equal(r.getHealthStatusClass(role, status), cls, role + '/' + status);
    }
    assert.equal(r.getHealthLabel('companion', null), null);
    assert.equal(r.isQuietHealthRole('companion'), true);
    assert.equal(r.isQuietHealthRole('sensor'), true);
    for (const role of ['repeater', 'room', 'observer', '', null, undefined]) assert.equal(r.isQuietHealthRole(role), false, String(role));
  });

  check('getNodeHealth: the one entry point every view uses', () => {
    assert.deepEqual(JSON.parse(JSON.stringify(r.getNodeHealth('companion', iso(10 * HOUR), iso(MIN), NOW))), { status: 'healthy', source: 'observer', at: iso(MIN) });
    assert.deepEqual(JSON.parse(JSON.stringify(r.getNodeHealth('companion', iso(10 * HOUR), iso(2 * HOUR), NOW))), { status: 'degraded', source: 'heard', at: iso(10 * HOUR) });
    assert.deepEqual(JSON.parse(JSON.stringify(r.getNodeHealth('companion', null, null, NOW))), { status: null, source: null, at: null });
    assert.equal(r.getNodeHealth('repeater', iso(HOUR), iso(MIN), NOW).source, 'heard', 'healthy radio evidence wins');
  });

  const custom = await load({ healthThresholds: { observerOnlineMs: 3 * HOUR } });
  check('observerOnlineMs from config moves the credit window; node thresholds are untouched', () => {
    assert.equal(custom.getNodeHealth('companion', iso(10 * HOUR), iso(2 * HOUR), NOW).status, 'healthy');
    assert.equal(custom.getNodeHealth('companion', iso(10 * HOUR), iso(4 * HOUR), NOW).status, 'degraded');
    assert.equal(custom.getHealthStatus('companion', iso(2 * HOUR), NOW), 'degraded');
  });

  check('MyMeshObservers.reportedAt: full-key match only, from the bulk roster', () => {
    const key = 'b'.repeat(64);
    const idx = r.MyMeshObservers.index({ observers: [{ id: key.toUpperCase(), last_seen: iso(MIN) }, { id: 'a'.repeat(12), last_seen: iso(MIN) }] });
    assert.equal(r.MyMeshObservers.reportedAt(idx, key), iso(MIN));
    assert.equal(r.MyMeshObservers.reportedAt(idx, 'a'.repeat(64)), null);
    assert.equal(r.MyMeshObservers.reportedAt(r.MyMeshObservers.index(null), key), null);
  });

  // Execute the real decision blocks of the other three-state views so they
  // agree with My Mesh. Each block is sliced out of production source and run
  // with the real helpers; no hand-copied logic.
  function block(file, start, end, args, returned) {
    const src = read(file);
    const from = src.indexOf(start), to = src.indexOf(end, from);
    assert.ok(from >= 0 && to > from, 'decision block exists in ' + file + ': ' + start);
    return new Function(...args, src.slice(from, to) + '\nreturn ' + returned + ';');
  }
  const KEY = 'c'.repeat(64);
  const obsIdx = (ago) => r.MyMeshObservers.index({ observers: ago == null ? [] : [{ id: KEY.toUpperCase(), last_seen: iso(ago) }] });
  const companion = { public_key: KEY, role: 'companion', last_heard: iso(10 * HOUR), last_seen: iso(10 * HOUR) };

  check('Analytics network status counts the observer as active', () => {
    const fn = block('analytics.js', 'let active = 0, degraded = 0, silent = 0;\n      nodes.forEach', 'const totalNodes', ['nodes', 'now', 'observerIdx', 'getNodeHealth', 'getHealthLastHeard', 'MyMeshObservers'], '[active, degraded, silent]');
    const run = ago => fn([companion], NOW, obsIdx(ago), r.getNodeHealth, r.getHealthLastHeard, r.MyMeshObservers);
    assert.deepEqual(run(MIN), [1, 0, 0]);
    assert.deepEqual(run(2 * HOUR), [0, 1, 0]);
    assert.deepEqual(run(null), [0, 1, 0]);
    assert.match(read('analytics.js'), /> Degraded \/ quiet<\/div>/);
  });

  check('Favorites dropdown uses the shared status, label and class', () => {
    const fn = block('app.js', 'const heardAt = getHealthLastHeard(h.node, h.stats);', 'return \'<a href=', ['h', 'pk', 'observerIdx', 'getHealthLastHeard', 'getNodeHealth', 'getHealthLabel', 'getHealthStatusClass', 'MyMeshObservers'], '[statusLabel, statusCls]');
    const h = { node: { role: 'companion' }, stats: { lastHeard: iso(10 * HOUR) } };
    const run = ago => fn(h, KEY, obsIdx(ago), r.getHealthLastHeard, (a, b, c) => r.getNodeHealth(a, b, c, NOW), r.getHealthLabel, r.getHealthStatusClass, r.MyMeshObservers);
    assert.deepEqual(run(MIN), ['Active', 'status-ok']);
    assert.deepEqual(run(2 * HOUR), ['Quiet', 'status-muted']);
  });

  check('Live popup uses the shared status and the Quiet wording', () => {
    // After #901 the popup's decision is the liveNodeHealthView helper; the
    // call site must pass the roster's report into it.
    assert.match(read('live.js'), /liveNodeHealthView\(n, stats, undefined, MyMeshObservers\.reportedAt\(observerIdx, pubkey\)\)/);
    const fn = block('live.js', 'function liveNodeHealthView(', 'async function showNodeDetail', ['getHealthLastHeard', 'getNodeHealth', 'getHealthLabel', 'isQuietHealthRole'], 'liveNodeHealthView');
    const view = fn(r.getHealthLastHeard, r.getNodeHealth, r.getHealthLabel, r.isQuietHealthRole);
    const run = (ago, role, heardAgo = 10 * HOUR) => {
      const v = view({ role: role || 'companion' }, { lastHeard: iso(heardAgo) }, NOW, ago == null ? null : iso(ago));
      return [v.status, v.label, v.dotClass];
    };
    assert.deepEqual(run(MIN), ['healthy', 'Online', 'health-green']);
    assert.deepEqual(run(2 * HOUR), ['degraded', 'Quiet', 'health-muted']);
    assert.deepEqual(run(null), ['degraded', 'Quiet', 'health-muted']);
    assert.deepEqual(run(null, 'repeater', 30 * HOUR), ['degraded', 'Degraded', 'health-yellow']);
    assert.deepEqual(run(MIN, 'repeater', 30 * HOUR), ['healthy', 'Online', 'health-green']);
    assert.deepEqual(run(null, 'room', 80 * HOUR), ['silent', 'Offline', 'health-red']);
  });

  console.log(`${passed} checks passed`);
})().catch(error => { console.error(error); process.exitCode = 1; });
