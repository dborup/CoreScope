/* test-live-follow.js
 * #880 M1 — Live "Follow a packet". Unit tests for the real
 * public/live-follow.js module, run with the real roles.js
 * (observerSkewSeverity) and hop-resolver.js (haversineKm) in a vm
 * sandbox. No DOM, no server.
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
    console, Date, Math, Number, String, JSON, Array, Object, Set, Map, parseInt, parseFloat, isFinite, isNaN,
  };
  ctx.window.localStorage = ctx.localStorage;
  ctx.window.document = ctx.document;
  ctx.window.getComputedStyle = ctx.getComputedStyle;
  vm.createContext(ctx);
  for (const f of ['public/roles.js', 'public/hop-resolver.js', 'public/live-follow.js']) {
    vm.runInContext(fs.readFileSync(path.join(__dirname, f), 'utf8'), ctx, { filename: f });
    for (const k of Object.keys(ctx.window)) ctx[k] = ctx.window[k];
  }
  return ctx.window;
}

const W = load();
const LF = W.LiveFollow;
const haversineKm = W.HopResolver.haversineKm;
const severity = W.observerSkewSeverity;
const T0 = Date.UTC(2026, 10, 18, 12, 0, 0) / 1000;
const iso = (sec) => new Date(sec * 1000).toISOString();

// A resolver stand-in for live.js resolveHopEntries: hop prefix → node.
function resolverFrom(nodes) {
  return function (hops) {
    return hops.map((h) => {
      const n = nodes[h];
      return n ? { key: n.key, name: n.name, pos: n.pos } : null;
    });
  };
}
const NODES = {
  AA: { key: 'aa' + '0'.repeat(62), name: 'Relay A', pos: [55.0, 12.0] },
  BB: { key: 'bb' + '0'.repeat(62), name: 'Relay B', pos: [55.1, 12.1] },
  CC: { key: 'cc' + '0'.repeat(62), name: 'Relay C', pos: [55.3, 12.3] },
  DD: { key: 'dd' + '0'.repeat(62), name: 'Relay D (no GPS)', pos: null },
};
const ROSTER = LF.buildObserverRoster({ observers: [
  { id: 'OBS1', name: 'One', iata: 'CPH', lat: 55.01, lon: 12.01, clock_naive: false },
  { id: 'OBS2', name: 'Two', iata: 'cph', lat: '55.2', lon: '12.2', clock_naive: false },
  { id: 'OBS3', name: 'Three', iata: 'AAR', lat: 56.1, lon: 10.2, clock_naive: true },
  { id: 'OBS4', name: 'Four', iata: null, lat: 0, lon: 0 },
  { id: 'OBS5', name: 'Five', iata: 'ODE', lat: 55.4, lon: 10.4 },
] });
const SKEW = LF.buildSkewMap([{ observerID: 'OBS5', offsetSec: 900, samples: 12 }, { observerID: 'OBS1', offsetSec: 4, samples: 50 }]);

function obs(id, observer, tSec, hops, extra) {
  return Object.assign({ id, observer_id: observer, observer_name: 'name-' + observer, observer_iata: null,
    path_json: JSON.stringify(hops), timestamp: tSec == null ? null : iso(tSec) }, extra || {});
}
function advertDetail(observations, over) {
  return {
    packet: Object.assign({ hash: 'A1B2C3D4E5F60718', route_type: 1, payload_type: 4,
      decoded_json: JSON.stringify({ type: 'ADVERT', pubKey: 'ee' + '0'.repeat(62), name: 'Origin', lat: 54.9, lon: 11.9 }) }, over || {}),
    observations,
  };
}
const DEPS = {
  roster: ROSTER, skew: SKEW, resolveHops: resolverFrom(NODES), severity,
  typeName: (id) => ({ 4: 'ADVERT', 5: 'GRP_TXT', 9: 'TRACE' })[id] || 'UNKNOWN',
};
const reduce = (detail, deps) => LF.reduceFollow(detail, Object.assign({}, DEPS, deps || {}));

console.log('\n=== live-follow: deep link parsing ===');

test('parseFollowParam accepts a 16-hex hash and lower-cases it', () => {
  assert.strictEqual(LF.parseFollowParam('a1b2c3d4e5f60718'), 'a1b2c3d4e5f60718');
  assert.strictEqual(LF.parseFollowParam('A1B2C3D4E5F60718'), 'a1b2c3d4e5f60718');
  assert.strictEqual(LF.parseFollowParam('  a1b2c3d4e5f60718 '), 'a1b2c3d4e5f60718');
});

test('parseFollowParam rejects anything else', () => {
  for (const bad of [null, undefined, '', 'xyz', 'a1b2c3d4e5f6071', 'a1b2c3d4e5f607189', 'g1b2c3d4e5f60718',
    'a1b2c3d4e5f60718<', '<img src=x>', 12345]) {
    assert.strictEqual(LF.parseFollowParam(bad), null, 'accepted ' + bad);
  }
});

test('HASH_PARAM is "follow"', () => {
  assert.strictEqual(LF.HASH_PARAM, 'follow');
});

console.log('\n=== live-follow: settle wait before the one fetch ===');

test('settleMs: a fresh packet waits out what is left of the propagation window', () => {
  const now = 1_000_000;
  assert.strictEqual(LF.settleMs(now, 2500, now), 2500);
  assert.strictEqual(LF.settleMs(now - 1000, 2500, now), 1500);
  assert.strictEqual(LF.settleMs(now + 5000, 2500, now), 2500, 'clock ahead: never more than the window');
});

test('settleMs: an older line or packet fetches at once', () => {
  const now = 1_000_000;
  // Review #6b: "always the full buffer" kept every E2E green.
  assert.strictEqual(LF.settleMs(now - 2500, 2500, now), 0);
  assert.strictEqual(LF.settleMs(now - 60000, 2500, now), 0);
});

test('settleMs: no buffer or a bad timestamp never waits', () => {
  const now = 1_000_000;
  assert.strictEqual(LF.settleMs(now, 0, now), 0);
  assert.strictEqual(LF.settleMs(now, undefined, now), 0);
  assert.strictEqual(LF.settleMs(now, '2500', now), 2500, 'a numeric string is cast');
  assert.strictEqual(LF.settleMs(undefined, 2500, now), 0, 'no timestamp: treated as old');
  assert.strictEqual(LF.settleMs('garbage', 2500, now), 0);
  assert.strictEqual(LF.settleMs(now, 2500, NaN), 0);
});

console.log('\n=== live-follow: timestamps cast at the boundary ===');

test('toEpochSec takes ISO strings, seconds, milliseconds and digit strings', () => {
  assert.strictEqual(LF.toEpochSec(iso(T0)), T0);
  assert.strictEqual(LF.toEpochSec('2026-11-18T12:00:00.000Z'), T0);
  assert.strictEqual(LF.toEpochSec(T0), T0);
  assert.strictEqual(LF.toEpochSec(T0 * 1000 + 999), T0);
  assert.strictEqual(LF.toEpochSec(String(T0)), T0);
});

test('toEpochSec returns null for null, garbage and non-finite input', () => {
  for (const bad of [null, undefined, '', 'yesterday', NaN, Infinity, {}, []]) {
    assert.strictEqual(LF.toEpochSec(bad), null, 'accepted ' + JSON.stringify(bad));
  }
});

console.log('\n=== live-follow: reduce ===');

test('one entry per observer, its EARLIEST observation (not the deepest)', () => {
  const m = reduce(advertDetail([
    obs(1, 'OBS1', T0 + 3, ['AA', 'BB', 'CC']),
    obs(2, 'OBS1', T0, ['AA']),
    obs(3, 'OBS2', T0 + 1, ['AA', 'BB']),
  ]));
  assert.strictEqual(m.observers.length, 2);
  const o1 = m.observers.find((o) => o.id === 'OBS1');
  assert.strictEqual(o1.hops, 1);
  assert.strictEqual(o1.tSec, T0);
});

test('the earliest report wins even when a later one has fewer hops (not the shallowest)', () => {
  // Review #6d: "keep the shallowest" passed the test above, where the
  // earliest report was also the shallowest.
  const m = reduce(advertDetail([
    obs(1, 'OBS1', T0, ['AA', 'BB', 'CC']),
    obs(2, 'OBS1', T0 + 4, ['AA']),
  ]));
  assert.strictEqual(m.observers.length, 1);
  assert.strictEqual(m.observers[0].hops, 3);
  assert.strictEqual(m.observers[0].tSec, T0);
  assert.strictEqual(m.depth, 3);
});

test('same second: the observation with fewer hops wins', () => {
  const m = reduce(advertDetail([
    obs(1, 'OBS1', T0, ['AA', 'BB']),
    obs(2, 'OBS1', T0, ['AA']),
  ]));
  assert.strictEqual(m.observers[0].hops, 1);
});

test('relays dedupe by key across paths and keep their smallest depth (index + 1)', () => {
  const m = reduce(advertDetail([
    obs(1, 'OBS1', T0, ['AA', 'BB', 'CC']),
    obs(2, 'OBS2', T0, ['BB', 'CC']),
    obs(3, 'OBS3', T0 + 1, ['DD', 'EE']),
  ]));
  const byKey = {};
  for (const r of m.relays) byKey[r.name] = r;
  assert.strictEqual(m.relays.length, 5, JSON.stringify(m.relays.map((r) => r.key)));
  assert.strictEqual(byKey['Relay A'].depth, 1);
  assert.strictEqual(byKey['Relay B'].depth, 1);
  assert.strictEqual(byKey['Relay C'].depth, 2);
  assert.strictEqual(byKey['Relay D (no GPS)'].pos, null);
  assert.strictEqual(byKey.EE.key, 'hop-EE', 'unresolved hop keyed by prefix');
  assert.strictEqual(m.depth, 3);
});

test('ADVERT origin comes from the decoded payload; (0,0) means no origin', () => {
  const m = reduce(advertDetail([obs(1, 'OBS1', T0, [])]));
  assert.deepStrictEqual(Array.from(m.origin.pos), [54.9, 11.9]);
  assert.strictEqual(m.origin.name, 'Origin');
  assert.strictEqual(m.typeName, 'ADVERT');
  assert.strictEqual(m.hash, 'a1b2c3d4e5f60718');
  const z = reduce(advertDetail([obs(1, 'OBS1', T0, [])], {
    decoded_json: JSON.stringify({ pubKey: 'ee', lat: 0, lon: 0 }) }));
  assert.strictEqual(z.origin, null);
  const chan = reduce(advertDetail([obs(1, 'OBS1', T0, [])], { payload_type: 5, decoded_json: '{"text":"hi"}' }));
  assert.strictEqual(chan.origin, null);
  assert.strictEqual(chan.typeName, 'GRP_TXT');
});

test('observer positions and regions come from the roster; (0,0) has no position', () => {
  const m = reduce(advertDetail([obs(1, 'OBS2', T0, []), obs(2, 'OBS4', T0, []), obs(3, 'GONE', T0, [])]));
  const by = {}; for (const o of m.observers) by[o.id] = o;
  assert.deepStrictEqual(Array.from(by.OBS2.pos), [55.2, 12.2]);
  assert.strictEqual(by.OBS2.iata, 'cph');
  assert.strictEqual(by.OBS4.pos, null);
  assert.strictEqual(by.GONE.pos, null);
  assert.strictEqual(by.GONE.name, 'name-GONE');
});

test('DIRECT and TRACE packets are not replayable: summary data only, depth 0', () => {
  for (const over of [{ route_type: 2 }, { route_type: 3 }, { payload_type: 9 }]) {
    const m = reduce(advertDetail([obs(1, 'OBS1', T0, ['AA', 'BB'])], over));
    assert.strictEqual(m.replayable, false, JSON.stringify(over));
    assert.strictEqual(m.relays.length, 0);
    assert.strictEqual(m.depth, 0);
    assert.strictEqual(m.observers.length, 1);
  }
  assert.strictEqual(reduce(advertDetail([obs(1, 'OBS1', T0, ['AA'])], { route_type: 0 })).replayable, true);
});

test('caps processing at MAX_OBSERVATIONS (200), keeping the earliest', () => {
  assert.strictEqual(LF.MAX_OBSERVATIONS, 200);
  const list = [];
  for (let i = 0; i < 250; i++) list.push(obs(i + 1, 'X' + i, T0 + (250 - i), ['AA']));
  const m = reduce(advertDetail(list));
  assert.strictEqual(m.total, 250);
  assert.strictEqual(m.capped, true);
  assert.strictEqual(m.observers.length, 200);
  assert.ok(!m.observers.some((o) => o.id === 'X0'), 'latest observation must be dropped');
  assert.ok(m.observers.some((o) => o.id === 'X249'), 'earliest observation must be kept');
  const small = reduce(advertDetail(list.slice(0, 200)));
  assert.strictEqual(small.capped, false);
});

test('path depth is clamped at MAX_DEPTH (64, firmware MAX_PATH_SIZE)', () => {
  assert.strictEqual(LF.MAX_DEPTH, 64);
  const hops = []; for (let i = 0; i < 80; i++) hops.push(('0' + i.toString(16)).slice(-2).toUpperCase());
  const m = reduce(advertDetail([obs(1, 'OBS1', T0, hops)]));
  assert.strictEqual(m.depth, 64);
  assert.strictEqual(m.observers[0].hops, 64);
});

test('path_json as an array, a string, null or garbage', () => {
  const m = reduce(advertDetail([
    Object.assign(obs(1, 'OBS1', T0, []), { path_json: ['AA', 'BB'] }),
    Object.assign(obs(2, 'OBS2', T0, []), { path_json: null }),
    Object.assign(obs(3, 'OBS3', T0, []), { path_json: '{not json' }),
  ]));
  const by = {}; for (const o of m.observers) by[o.id] = o;
  assert.strictEqual(by.OBS1.hops, 2);
  assert.strictEqual(by.OBS2.hops, 0);
  assert.strictEqual(by.OBS3.hops, 0);
});

test('an empty or malformed detail reduces to an empty model', () => {
  for (const d of [null, {}, { packet: null, observations: null }]) {
    const m = reduce(d);
    assert.strictEqual(m.observers.length, 0);
    assert.strictEqual(m.depth, 0);
  }
});

console.log('\n=== live-follow: labels ===');

test('offsets are whole seconds from the first report; >60 s is late', () => {
  const m = reduce(advertDetail([
    obs(1, 'OBS1', T0, []), obs(2, 'OBS2', T0 + 2, ['AA']), obs(3, 'OBS3', T0 + 75, ['AA']),
    obs(4, 'OBS5', null, ['AA']),
  ]));
  const by = {}; for (const o of m.observers) by[o.id] = o;
  assert.strictEqual(LF.formatOffset(by.OBS1), 'first second');
  assert.strictEqual(LF.formatOffset(by.OBS2), '+2 s');
  assert.strictEqual(by.OBS3.late, true);
  assert.strictEqual(LF.formatOffset(by.OBS3), 'late (+75 s)');
  assert.strictEqual(by.OBS5.deltaSec, null);
  assert.strictEqual(LF.formatOffset(by.OBS5), 'time unknown');
  assert.strictEqual(LF.LATE_SEC, 60);
  const edge = reduce(advertDetail([obs(1, 'OBS1', T0, []), obs(2, 'OBS2', T0 + 60, [])]));
  assert.strictEqual(edge.observers.find((o) => o.id === 'OBS2').late, false, 'exactly 60 s is not late');
});

test('observers come out in the order heard (offset, then id)', () => {
  const m = reduce(advertDetail([obs(1, 'OBS2', T0 + 1, []), obs(2, 'OBS3', T0, []), obs(3, 'OBS1', T0, [])]));
  assert.deepStrictEqual(Array.from(m.observers.map((o) => o.id)), ['OBS1', 'OBS3', 'OBS2']);
});

test('CAPTION says the times are as reported by observers', () => {
  assert.ok(/as reported by observers/i.test(LF.CAPTION), LF.CAPTION);
  assert.ok(/not clock-corrected/i.test(LF.CAPTION), LF.CAPTION);
});

console.log('\n=== live-follow: clock flags (flag only, never correct) ===');

test('|offsetSec| >= 300 flags warning, >= 3600 critical, clock_naive flags naive', () => {
  const skew = LF.buildSkewMap([
    { observerID: 'OBS1', offsetSec: 299 }, { observerID: 'OBS2', offsetSec: -300 },
    { observerID: 'OBS5', offsetSec: 3600 },
  ]);
  const m = reduce(advertDetail([obs(1, 'OBS1', T0, []), obs(2, 'OBS2', T0, []), obs(3, 'OBS3', T0, []),
    obs(4, 'OBS5', T0, [])]), { skew });
  const by = {}; for (const o of m.observers) by[o.id] = o;
  assert.strictEqual(by.OBS1.clock, null);
  assert.strictEqual(by.OBS2.clock, 'warning');
  assert.strictEqual(by.OBS3.clock, 'naive');
  assert.strictEqual(by.OBS5.clock, 'critical');
});

test('times are never shifted by the skew offset', () => {
  const m = reduce(advertDetail([obs(1, 'OBS1', T0, []), obs(2, 'OBS5', T0 + 3, [])]));
  const o5 = m.observers.find((o) => o.id === 'OBS5');
  assert.strictEqual(o5.clock, 'warning');
  assert.strictEqual(o5.tSec, T0 + 3);
  assert.strictEqual(o5.deltaSec, 3);
});

test('no skew list and no roster: nothing is flagged, nothing throws', () => {
  const m = reduce(advertDetail([obs(1, 'OBS5', T0, [])]), { skew: null, roster: null });
  assert.strictEqual(m.observers[0].clock, null);
  assert.strictEqual(m.observers[0].pos, null);
});

test('buildObserverRoster takes the wrapped and the bare-array shape', () => {
  const a = LF.buildObserverRoster([{ id: 'X', name: 'x', lat: 1, lon: 2, clock_naive: 1 }]);
  assert.deepStrictEqual(Array.from(a.X.pos), [1, 2]);
  assert.strictEqual(a.X.clockNaive, true);
  assert.deepStrictEqual(Object.keys(LF.buildObserverRoster(null)), []);
  assert.deepStrictEqual(Object.keys(LF.buildObserverRoster({ observers: [null, { name: 'no id' }] })), []);
  assert.deepStrictEqual(Object.keys(LF.buildSkewMap({ nope: 1 })), []);
});

console.log('\n=== live-follow: schedule (hop depth drives the clock) ===');

test('stepMs = clamp(TARGET_MS / (D + 1), 350, 1500) / speed', () => {
  assert.strictEqual(LF.TARGET_MS, 6000);
  assert.strictEqual(LF.stepMsFor(0), 1500);
  assert.strictEqual(LF.stepMsFor(3), 1500);
  assert.strictEqual(LF.stepMsFor(4), 1200);
  assert.strictEqual(LF.stepMsFor(64), 350);
  assert.strictEqual(LF.stepMsFor(4, { speed: 2 }), 600);
  assert.strictEqual(LF.stepMsFor(4, { speed: 0.5 }), 2400);
  assert.strictEqual(LF.stepMsFor(4, { speed: 3 }), 1200, 'unsupported speed falls back to 1x');
  assert.strictEqual(LF.stepMsFor(4, { speed: 'fast' }), 1200);
});

function sampleModel() {
  return reduce(advertDetail([
    obs(1, 'OBS1', T0 + 1, []),             // depth 0
    obs(2, 'OBS2', T0 + 2, ['AA']),         // depth 1
    obs(3, 'OBS3', T0 + 1, ['AA']),         // depth 1, earlier than OBS2
    obs(4, 'OBS5', T0 + 3, ['AA', 'BB', 'CC']), // depth 3
    obs(5, 'OBS4', T0 + 90, ['AA']),        // late → last step
  ]));
}

test('events: origin first, one ring per depth, summary last at (D + 1) * stepMs', () => {
  const s = LF.buildFollowSchedule(sampleModel());
  assert.strictEqual(s.stepMs, 1500);
  assert.strictEqual(s.events[0].kind, 'origin');
  assert.strictEqual(s.events[0].at, 0);
  const rings = s.events.filter((e) => e.kind === 'ring');
  assert.deepStrictEqual(Array.from(rings.map((e) => e.depth)), [0, 1, 2, 3]);
  assert.deepStrictEqual(Array.from(rings.map((e) => e.at)), [0, 1500, 3000, 4500]);
  const last = s.events[s.events.length - 1];
  assert.strictEqual(last.kind, 'summary');
  assert.strictEqual(last.at, 6000);
  assert.strictEqual(s.totalMs, 6000);
  for (let i = 1; i < s.events.length; i++) assert.ok(s.events[i].at >= s.events[i - 1].at, 'sorted by time');
});

test('relays light at their depth; observers at their hop count, ordered by offset inside the step', () => {
  const m = sampleModel();
  const s = LF.buildFollowSchedule(m);
  for (const e of s.events.filter((x) => x.kind === 'relay')) {
    assert.strictEqual(e.at, m.relays[e.index].depth * s.stepMs);
  }
  const obsEv = s.events.filter((e) => e.kind === 'observer').map((e) => ({ id: m.observers[e.index].id, at: e.at }));
  const at = {}; for (const o of obsEv) at[o.id] = o.at;
  assert.strictEqual(at.OBS1, 0);
  assert.ok(at.OBS3 >= 1500 && at.OBS2 > at.OBS3, 'OBS3 (+0 s) before OBS2 (+1 s) inside depth 1: ' + JSON.stringify(at));
  assert.ok(at.OBS2 < 1500 + 0.6 * 1500, 'stagger stays inside 60% of the step');
  assert.ok(at.OBS5 >= 4500 && at.OBS5 < 6000, 'depth 3');
  assert.ok(at.OBS4 >= 4500 && at.OBS4 < 6000, 'late observer is revealed at the last step');
});

test('depth, not reported time, decides the order across steps', () => {
  // OBS5 reported an EARLIER second than OBS2 but sits 3 hops out.
  const m = reduce(advertDetail([obs(1, 'OBS5', T0, ['AA', 'BB', 'CC']), obs(2, 'OBS2', T0 + 5, ['AA'])]));
  const s = LF.buildFollowSchedule(m);
  const at = {}; for (const e of s.events) if (e.kind === 'observer') at[m.observers[e.index].id] = e.at;
  assert.ok(at.OBS2 < at.OBS5, JSON.stringify(at));
});

test('D = 0 and D = 64 stay inside 1 s .. 15 s at 1x', () => {
  const m0 = reduce(advertDetail([obs(1, 'OBS1', T0, [])]));
  const s0 = LF.buildFollowSchedule(m0);
  assert.strictEqual(s0.totalMs, 1500);
  const hops = []; for (let i = 0; i < 64; i++) hops.push(('0' + i.toString(16)).slice(-2));
  const s64 = LF.buildFollowSchedule(reduce(advertDetail([obs(1, 'OBS1', T0, hops)])));
  assert.strictEqual(s64.totalMs, 65 * 350);
  assert.strictEqual(s64.events.filter((e) => e.kind === 'ring').length, 65);
});

test('speed scales the whole schedule', () => {
  const m = sampleModel();
  assert.strictEqual(LF.buildFollowSchedule(m, { speed: 2 }).totalMs, 3000);
  assert.strictEqual(LF.buildFollowSchedule(m, { speed: 0.5 }).totalMs, 12000);
});

test('reduced motion: a single frame at t = 0, summary still last', () => {
  const s = LF.buildFollowSchedule(sampleModel(), { reducedMotion: true });
  assert.strictEqual(s.stepMs, 0);
  assert.strictEqual(s.totalMs, 0);
  assert.ok(s.events.every((e) => e.at === 0));
  assert.strictEqual(s.events[s.events.length - 1].kind, 'summary');
});

test('a non-replayable packet schedules no relays and opens the summary after one step', () => {
  const m = reduce(advertDetail([obs(1, 'OBS1', T0, ['AA']), obs(2, 'OBS2', T0, ['AA', 'BB'])], { route_type: 2 }));
  const s = LF.buildFollowSchedule(m);
  assert.strictEqual(s.events.filter((e) => e.kind === 'relay').length, 0);
  assert.strictEqual(s.events[s.events.length - 1].kind, 'summary');
});

console.log('\n=== live-follow: summary ===');

test('longest single hop skips segments with an unknown position and counts them', () => {
  const m = reduce(advertDetail([
    obs(1, 'OBS1', T0, ['AA']),       // Origin→A→OBS1
    obs(2, 'OBS2', T0 + 1, ['AA', 'DD']), // A→D(no pos)→OBS2: two unpositioned segments
    obs(3, 'OBS3', T0 + 2, ['CC']),   // Origin→C→OBS3 (C→OBS3 is the long one)
  ]));
  const s = LF.summarizeFollow(m, haversineKm);
  const expect = haversineKm(55.3, 12.3, 56.1, 10.2);
  assert.ok(Math.abs(s.longestKm - expect) < 1e-9, s.longestKm + ' vs ' + expect);
  assert.strictEqual(s.longestFrom, 'Relay C');
  assert.strictEqual(s.longestTo, 'name-OBS3', 'the observation\'s observer name');
  assert.strictEqual(s.unpositionedHops, 2);
});

test('regions dedupe case-insensitively; first/last/spread; flagged count', () => {
  const m = reduce(advertDetail([
    obs(1, 'OBS1', T0, [], { observer_iata: 'CPH' }), obs(2, 'OBS2', T0 + 2, [], { observer_iata: 'cph' }),
    obs(3, 'OBS3', T0 + 3, [], { observer_iata: 'AAR' }), obs(4, 'OBS5', T0 + 1, []),
  ]));
  const s = LF.summarizeFollow(m, haversineKm);
  assert.deepStrictEqual(Array.from(s.regions), ['AAR', 'CPH', 'ODE']);
  assert.strictEqual(s.observers, 4);
  assert.strictEqual(s.flagged, 2, 'OBS3 naive + OBS5 skewed');
  assert.strictEqual(s.firstSec, T0);
  assert.strictEqual(s.lastSec, T0 + 3);
  assert.strictEqual(s.spreadSec, 3);
  assert.strictEqual(s.hopsMax, 0);
});

test('summaryRows: label/value pairs for the card', () => {
  const m = sampleModel();
  const s = LF.summarizeFollow(m, haversineKm);
  const rows = LF.summaryRows(s, (sec) => 'T' + (sec - T0));
  const by = {}; for (const r of rows) by[r.label] = r.value;
  assert.strictEqual(by['Hops (max)'], '3');
  assert.strictEqual(by['Longest single hop'], s.longestKm.toFixed(1) + ' km (Relay A → name-OBS3) · 1 hop without position');
  assert.strictEqual(by.Observers, '5 (2 with unreliable clock)');
  assert.strictEqual(by['First / last heard'], 'T1 / T90 (+89 s)');
  assert.strictEqual(by['Regions reached'], 'AAR, CPH, ODE');
});

test('summaryRows for DIRECT and for no data', () => {
  const d = LF.summarizeFollow(reduce(advertDetail([obs(1, 'OBS4', T0, ['AA'])], { route_type: 2 })), haversineKm);
  const by = {}; for (const r of LF.summaryRows(d, String)) by[r.label] = r.value;
  assert.strictEqual(by['Hops (max)'], '—');
  assert.strictEqual(by['Longest single hop'], '—');
  assert.strictEqual(by['Regions reached'], '—');
  const e = LF.summarizeFollow(reduce({}), haversineKm);
  const be = {}; for (const r of LF.summaryRows(e, String)) be[r.label] = r.value;
  assert.strictEqual(be.Observers, '0');
  assert.strictEqual(be['First / last heard'], '—');
});

console.log('\n=== live-follow: perf budget ===');

// Best of 7 against a generous budget: idle runs take ~7 ms, and a busy CI
// runner (review #7: 18–44 ms medians under parallel load) inflates single
// runs but rarely all seven. Quadratic work over the N·H = 12,800 hop
// entries would blow past it.
test('reduce + schedule + summary of 200 observations × 64 hops: best of 7 runs < 60 ms', () => {
  const nodes = {};
  const hops = [];
  for (let i = 0; i < 64; i++) {
    const h = ('0' + i.toString(16)).slice(-2).toUpperCase();
    hops.push(h);
    nodes[h] = { key: h.toLowerCase() + '0'.repeat(62), name: 'R' + i, pos: [55 + i / 100, 12 + i / 100] };
  }
  const list = [];
  const roster = {};
  for (let i = 0; i < 260; i++) {
    list.push(obs(i + 1, 'O' + i, T0 + (i % 7), hops.slice(0, 1 + (i % 64))));
    roster['O' + i] = { id: 'O' + i, name: 'O' + i, pos: [56, 11], iata: 'X' + (i % 5) };
  }
  const detail = advertDetail(list);
  const deps = Object.assign({}, DEPS, { roster, resolveHops: resolverFrom(nodes) });
  const run = () => {
    const m = LF.reduceFollow(detail, deps);
    LF.buildFollowSchedule(m);
    LF.summarizeFollow(m, haversineKm);
    return m;
  };
  run(); run(); // warm up
  const times = [];
  for (let i = 0; i < 7; i++) { const t = process.hrtime.bigint(); run(); times.push(Number(process.hrtime.bigint() - t) / 1e6); }
  times.sort((a, b) => a - b);
  const best = times[0];
  const median = times[3];
  const m = run();
  assert.strictEqual(m.observers.length, 200);
  assert.strictEqual(m.depth, 64);
  assert.ok(best < 60, 'best ' + best.toFixed(2) + ' ms (median ' + median.toFixed(2) + ' ms)');
  console.log('     (best ' + best.toFixed(2) + ' ms, median ' + median.toFixed(2) + ' ms)');
});

console.log('\n' + passed + ' passed, ' + failed + ' failed');
process.exit(failed ? 1 : 0);
