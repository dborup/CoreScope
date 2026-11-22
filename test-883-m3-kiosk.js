/* test-883-m3-kiosk.js
 * #883 M3 — Live kiosk long-run robustness. Unit tests for the real
 * public/live-kiosk.js (and live-moments.js reseed()) in a vm sandbox with
 * a fake DOM, fake timers and a fake wall clock (tests/fake-dom.js):
 *   remount     in place every 6 h (owner decision 6), at a step boundary
 *               with no Moments card up; the step index carries over through
 *               module state; a wall-clock deadline that a sleep cannot stretch
 *   wake lock   acquired while visible, re-acquired after the browser drops
 *               it, released on destroy
 *   ws-open     each socket (re)open re-seeds Moments
 *   sleep/wake  a heartbeat that runs late re-anchors the tour from now
 *   autoReload  on while a kiosk controller is mounted
 *   budget      at most 3 kiosk timers in the steady state
 */
'use strict';
const assert = require('assert');
const { Target, El, makeDoc, makeClock, loadPublic, flush } = require('./tests/fake-dom');

let passed = 0, failed = 0;
const queue = [];
function test(name, fn) { queue.push({ name, fn }); }
function section(title) { queue.push({ title }); }

const K = loadPublic('live-kiosk.js').LiveKiosk;
const H = 3600000;

const AREAS = [
  { key: 'CCC', label: 'Area C', latMin: 57, latMax: 58, lonMin: 9, lonMax: 10 },
  { key: 'AAA', label: 'Area A', latMin: 55, latMax: 56, lonMin: 9, lonMax: 10 },
  { key: 'BBB', label: 'Area B', polygon: [[55, 11], [56, 12], [55.5, 12.5]] },
];
const BOUNDS = { AAA: [[55, 9], [56, 10]], BBB: [[55, 11], [56, 12.5]], CCC: [[57, 9], [58, 10]] };
const TOUR = 'kiosk=1&tour=aaa,bbb,ccc&every=60';

// A fake Screen Wake Lock: request() → a sentinel; drop() is the browser
// releasing it (the page went hidden).
function makeWakeLock(opts) {
  opts = opts || {};
  const wl = { requests: 0, held: [], rejectNext: !!opts.reject, defer: null };
  wl.request = (type) => {
    wl.requests++;
    assert.strictEqual(type, 'screen');
    if (wl.rejectNext) { wl.rejectNext = false; return Promise.reject(new Error('NotAllowedError')); }
    const s = new Target();
    s.released = false;
    s.release = () => { if (!s.released) { s.released = true; wl.held = wl.held.filter((x) => x !== s); s.fire('release'); } return Promise.resolve(); };
    wl.held.push(s);
    if (wl.defer) return new Promise((res) => { wl.defer = () => res(s); });
    return Promise.resolve(s);
  };
  wl.drop = () => wl.held.slice().forEach((s) => s.release());
  return wl;
}

// One kiosk "page": a document and window that outlive the mounts, and a
// mount() that creates a controller the way live.js does. remount() is
// deps.remount: destroy() + create() in the same document.
function page(opts) {
  opts = opts || {};
  const clock = opts.clock || makeClock();
  const doc = makeDoc();
  doc.body.className = 'kiosk';
  if (opts.hidden) doc.hidden = true;
  const win = new Target();
  const p = { clock, doc, win, hash: '#/live?' + (opts.hash || TOUR), flights: [], mounts: 0, remountCalls: 0,
    reseeds: 0, cards: 0, mode: 'LIVE', autoReload: [], c: null, wakeLock: opts.wakeLock === undefined ? makeWakeLock() : opts.wakeLock };
  p.mount = () => {
    p.mounts++;
    const root = new El('div');
    const map = new Target();
    map.on = (t, fn) => map.addEventListener(t, fn);
    map.off = (t, fn) => map.removeEventListener(t, fn);
    p.root = root; p.map = map;
    p.c = K.create({
      doc, win, root, map,
      params: K.parseKioskParams(new URLSearchParams(p.hash.split('?')[1] || '')),
      fetchAreas: () => Promise.resolve(AREAS),
      fly: (bounds) => p.flights.push({ mount: p.mounts, bounds: JSON.parse(JSON.stringify(bounds)), at: clock.now() }),
      reducedMotion: () => true,
      getMode: () => p.mode,
      requestFullscreen: () => Promise.resolve(),
      exitFullscreen: () => Promise.resolve(),
      exit: () => {},
      warn: () => {},
      now: clock.now, setTimeout: clock.setTimeout, clearTimeout: clock.clearTimeout,
      // #883 M3
      hash: () => p.hash,
      remount: opts.noRemount ? undefined : () => { p.remountCalls++; p.c.destroy(); p.mount(); },
      momentsVisible: () => p.cards > 0,
      reseed: opts.noReseed ? undefined : () => { p.reseeds++; },
      wakeLock: p.wakeLock,
      setAutoReload: (on) => p.autoReload.push(!!on),
      remountMs: opts.remountMs,
    });
    return p.c;
  };
  p.mount();
  p.last = () => p.flights[p.flights.length - 1];
  p.key = (b) => Object.keys(BOUNDS).find((k) => JSON.stringify(BOUNDS[k]) === JSON.stringify(b));
  p.keys = (from) => p.flights.slice(from || 0).map((f) => p.key(f.bounds));
  p.show = (hidden) => { doc.hidden = !!hidden; doc.fire('visibilitychange'); };
  p.destroy = () => { p.c.destroy(); };
  return p;
}

section('=== #883 M3 remount: interval ===');

test('remountDelayMs: 6 h by default, clamped to 1–24 h', () => {
  assert.strictEqual(K.REMOUNT_MS_DEFAULT, 6 * H);
  assert.strictEqual(K.remountDelayMs(undefined), 6 * H);
  assert.strictEqual(K.remountDelayMs('x'), 6 * H);
  assert.strictEqual(K.remountDelayMs(0), 6 * H);
  assert.strictEqual(K.remountDelayMs(-5), 6 * H);
  assert.strictEqual(K.remountDelayMs(0.5 * H), 1 * H);
  assert.strictEqual(K.remountDelayMs(12 * H), 12 * H);
  assert.strictEqual(K.remountDelayMs(48 * H), 24 * H);
});

section('=== #883 M3 remount: when ===');

test('a touring kiosk remounts once at the first step boundary after 6 h, not before', async () => {
  const p = page();
  await flush();
  p.clock.advance(6 * H - 60000);
  await flush();
  assert.strictEqual(p.remountCalls, 0, 'not before 6 h');
  const before = p.flights.length;
  p.clock.advance(60000 + 60000);   // past 6 h and at least one boundary
  await flush();
  assert.strictEqual(p.remountCalls, 1, 'one remount');
  assert.strictEqual(p.mounts, 2);
  assert.strictEqual(K.liveCount(), 1, 'one controller');
  assert(p.flights.length > before, 'the tour goes on');
  p.clock.advance(5 * H);
  await flush();
  assert.strictEqual(p.remountCalls, 1, 'the next one is 6 h after the remount');
  p.destroy();
});

test('the step index survives the remount: the new mount shows the step the old one was due to show', async () => {
  const p = page();
  await flush();
  // 6 h at every=60 over 3 steps: find the boundary right after 6 h.
  p.clock.advance(6 * H - 30000);
  await flush();
  const i0 = p.flights.length;
  const shownLast = p.key(p.last().bounds);
  p.clock.advance(60000);
  await flush();
  assert.strictEqual(p.remountCalls, 1);
  const order = ['AAA', 'BBB', 'CCC'];
  const expected = order[(order.indexOf(shownLast) + 1) % 3];
  const after = p.flights.slice(i0);
  assert.deepStrictEqual(after.map((f) => p.key(f.bounds)), [expected], 'one fit, by the new mount, of the next step');
  assert.strictEqual(after[0].mount, 2, 'fitted by the new mount, not the old one');
  assert.strictEqual(p.c.stats().step, order.indexOf(expected));
  p.clock.advance(60000);
  assert.strictEqual(p.key(p.last().bounds), order[(order.indexOf(expected) + 1) % 3], 'and tours on from there');
  p.destroy();
});

test('the carried step applies only to the same kiosk URL, and only once', async () => {
  const p = page();
  await flush();
  p.clock.advance(6 * H - 30000);
  await flush();
  // The remount lands on a different kiosk link (an operator edit): step 1.
  p.hash = '#/live?kiosk=1&tour=aaa,bbb,ccc&every=60&region=x';
  p.clock.advance(60000);
  await flush();
  assert.strictEqual(p.remountCalls, 1);
  assert.strictEqual(p.c.stats().step, 0, 'another URL starts at step 1');
  // A later plain mount (exit and re-enter) also starts at step 1.
  p.destroy(); p.mount(); await flush();
  assert.strictEqual(p.c.stats().step, 0);
  p.destroy();
});

test('a Moments card on screen postpones the remount to the next boundary without one', async () => {
  const p = page();
  await flush();
  p.cards = 1;
  p.clock.advance(6 * H + 3 * 60000);
  await flush();
  assert.strictEqual(p.remountCalls, 0, 'never with a card up');
  assert.strictEqual(p.c.stats().remountDue, true);
  p.cards = 0;
  p.clock.advance(60000);
  await flush();
  assert.strictEqual(p.remountCalls, 1);
  p.destroy();
});

test('no tour (static view) and one step: remounts on the heartbeat after 6 h, with no card up', async () => {
  for (const hash of ['kiosk=1', 'kiosk=1&tour=ccc']) {
    const p = page({ hash });
    await flush();
    p.clock.advance(6 * H - 1);
    assert.strictEqual(p.remountCalls, 0, hash + ': early');
    p.clock.advance(K.BEAT_MS);
    await flush();
    assert.strictEqual(p.remountCalls, 1, hash + ': by one heartbeat after 6 h');
    p.destroy();
  }
});

test('a paused tour (interaction, VCR out of LIVE) holds the remount; resuming remounts at that step', async () => {
  const p = page();
  await flush();
  p.mode = 'PAUSED';
  p.c.onModeChange();
  p.clock.advance(7 * H);
  await flush();
  assert.strictEqual(p.remountCalls, 0, 'not while the viewer has the VCR paused');
  p.mode = 'LIVE';
  p.c.onModeChange();
  p.clock.advance(0);
  await flush();
  assert.strictEqual(p.remountCalls, 1, 'at the resume step');
  assert.strictEqual(p.flights[p.flights.length - 1].mount, 2, 'the step is fitted by the new mount');
  p.destroy();
});

test('the deadline is wall clock: a 7 h sleep (no timer ran) remounts at the first boundary after waking', async () => {
  const p = page();
  await flush();
  p.clock.jump(7 * H);
  p.clock.advance(60000);
  await flush();
  assert.strictEqual(p.remountCalls, 1);
  p.destroy();
});

test('hidden: no heartbeat; on visible the overdue remount runs at the resume step', async () => {
  const p = page();
  await flush();
  p.show(true);
  assert.strictEqual(p.c.stats().timers, p.c.stats().overlay ? 1 : 0, 'no kiosk timer but the overlay while hidden: ' + JSON.stringify(p.c.stats()));
  p.clock.jump(8 * H);
  p.clock.advance(60000);
  assert.strictEqual(p.remountCalls, 0, 'nothing while hidden');
  p.show(false);
  p.clock.advance(0);
  await flush();
  assert.strictEqual(p.remountCalls, 1);
  p.destroy();
});

test('the test-only override shortens the interval (soak test), and the clamp holds without it', async () => {
  K._setRemountOverrideMs(120000);
  try {
    const p = page();
    await flush();
    p.clock.advance(120000 + 30000);
    await flush();
    assert.strictEqual(p.remountCalls, 1, 'at the boundary 2 min in');
    p.destroy();
  } finally { K._setRemountOverrideMs(null); }
  const q = page({ remountMs: 60000 });
  await flush();
  q.clock.advance(55 * 60000);
  assert.strictEqual(q.remountCalls, 0, 'config below 1 h is clamped to 1 h');
  q.destroy();
});

test('an in-place remount offers no start overlay and no skipped note again', async () => {
  const p = page({ hash: 'kiosk=1&tour=aaa,nope,bbb&every=60' });
  await flush();
  assert.strictEqual(p.c.stats().overlay, true, 'the first mount offers it');
  assert.strictEqual(p.root.find('live-kiosk-note').length, 1);
  p.clock.advance(6 * H + 60000);
  await flush();
  assert.strictEqual(p.remountCalls, 1);
  assert.strictEqual(p.c.stats().overlay, false, 'not after a remount');
  assert.strictEqual(p.root.find('live-kiosk-note').length, 0);
  assert.strictEqual(JSON.stringify(p.c.stats().skipped), '["nope"]', 'still skipped');
  p.destroy();
});

test('without a remount dependency nothing remounts and no heartbeat runs (an M1 host)', async () => {
  const p = page({ noRemount: true, wakeLock: null });
  await flush();
  assert.strictEqual(p.c.stats().beat, false);
  p.clock.advance(7 * H);
  assert.strictEqual(p.remountCalls, 0);
  p.destroy();
});

section('=== #883 M3 wake lock ===');

test('acquired once while visible, re-acquired after the browser drops it, released on destroy', async () => {
  const p = page();
  await flush();
  const wl = p.wakeLock;
  assert.strictEqual(wl.requests, 1);
  assert.strictEqual(wl.held.length, 1);
  assert.strictEqual(p.c.stats().wakeLock, true);
  p.show(false);
  await flush();
  assert.strictEqual(wl.requests, 1, 'not again while held');
  // Hidden: the browser releases it; visible: re-acquired.
  p.show(true); wl.drop();
  await flush();
  assert.strictEqual(p.c.stats().wakeLock, false);
  assert.strictEqual(wl.requests, 1, 'never requested while hidden');
  p.show(false);
  await flush();
  assert.strictEqual(wl.requests, 2);
  assert.strictEqual(wl.held.length, 1);
  const l = p.c.stats().listeners;
  for (let i = 0; i < 20; i++) { p.show(true); wl.drop(); await flush(); p.show(false); await flush(); }
  assert.strictEqual(p.c.stats().listeners, l, 'no listener per lock');
  assert.strictEqual(wl.held.length, 1);
  p.destroy();
  await flush();
  assert.strictEqual(wl.held.length, 0, 'released on destroy');
});

test('a kiosk opened hidden waits for visible; a rejected request is retried on the next visible', async () => {
  const wl = makeWakeLock({ reject: true });
  const p = page({ hidden: true, wakeLock: wl });
  await flush();
  assert.strictEqual(wl.requests, 0, 'not while hidden');
  p.show(false);
  await flush();
  assert.strictEqual(wl.requests, 1);
  assert.strictEqual(p.c.stats().wakeLock, false, 'rejected: no lock, no error');
  p.show(true); p.show(false);
  await flush();
  assert.strictEqual(wl.requests, 2);
  assert.strictEqual(p.c.stats().wakeLock, true);
  p.destroy();
});

test('a lock granted after destroy is released at once; no API (plain http) is a no-op', async () => {
  const wl = makeWakeLock();
  wl.defer = true;
  const p = page({ wakeLock: wl });
  p.destroy();
  wl.defer();
  await flush();
  assert.strictEqual(wl.held.length, 0);
  const q = page({ wakeLock: null });
  await flush();
  assert.strictEqual(q.c.stats().wakeLock, false);
  q.destroy();
});

test('an in-place remount hands the lock over: one held, never zero for long', async () => {
  const p = page();
  await flush();
  p.clock.advance(6 * H + 60000);
  await flush();
  assert.strictEqual(p.remountCalls, 1);
  assert.strictEqual(p.wakeLock.held.length, 1, 'the new mount holds one');
  p.destroy();
  await flush();
  assert.strictEqual(p.wakeLock.held.length, 0);
});

section('=== #883 M3 ws-open: re-seed ===');

test('each corescope:ws-open re-seeds Moments once; none after destroy', async () => {
  const p = page();
  await flush();
  p.win.fire('corescope:ws-open');
  p.win.fire('corescope:ws-open');
  assert.strictEqual(p.reseeds, 2);
  p.destroy();
  p.win.fire('corescope:ws-open');
  assert.strictEqual(p.reseeds, 2);
  assert.strictEqual(p.win.count(), 0, 'no window listener left');
});

test('without a reseed dependency no ws-open listener is bound (an M1 host)', async () => {
  const p = page({ noReseed: true });
  await flush();
  assert.strictEqual(p.win.count('corescope:ws-open'), 0);
  p.destroy();
});

test('live-moments reseed(): one more history fetch, deduped against what is shown', async () => {
  const LM = loadPublic('live-moments.js').LiveMoments;
  const clock = makeClock();
  const doc = makeDoc();
  const root = new El('div');
  const fetches = [];
  const at = new Date(clock.now() - 10000).toISOString();
  const m = (id) => ({ id, kind: 'new_node', at, node: { publicKey: id.repeat(64).slice(0, 64), name: 'N' + id, role: 'repeater', lat: 55, lon: 9 } });
  let body = { moments: [m('a')] };
  const c = LM.create({ doc, root, onWS: () => {}, offWS: () => {},
    fetchJSON: (url) => { fetches.push(url); return Promise.resolve(body); },
    now: clock.now, setTimeout: clock.setTimeout, clearTimeout: clock.clearTimeout,
    matchMedia: () => ({ matches: false }), getMode: () => 'PAUSED', getZoom: () => 9 });
  await flush(); await flush();
  assert.strictEqual(fetches.length, 1);
  assert.strictEqual(c.stats().history, 1);
  body = { moments: [m('b'), m('a')] };
  c.reseed();
  await flush(); await flush();
  assert.strictEqual(fetches.length, 2);
  assert.strictEqual(fetches[1], fetches[0], 'the same bulk request');
  assert.strictEqual(c.stats().history, 2, 'b added, a not twice');
  c.destroy();
  c.reseed();
  await flush();
  assert.strictEqual(fetches.length, 2, 'nothing after destroy');
});

section('=== #883 M3 sleep / wake ===');

test('a heartbeat late by more than 2 beats (a sleep) re-anchors the tour from now and re-takes the lock', async () => {
  const p = page();
  await flush();
  p.clock.advance(30000);
  const n = p.flights.length;
  p.wakeLock.drop();        // the OS sleep released it (no visibilitychange)
  p.clock.jump(2 * H);      // asleep: no timer ran
  p.clock.advance(1);       // the overdue timers fire now
  await flush();
  const s = p.c.stats();
  assert(s.wakes >= 1, 'a wake was detected: ' + JSON.stringify(s));
  assert.strictEqual(p.wakeLock.held.length, 1, 'lock re-taken');
  const woke = p.flights.length;
  assert(woke - n <= 2, 'no catch-up burst: ' + (woke - n) + ' steps');
  const t = p.clock.now();
  p.clock.advance(59000);
  assert.strictEqual(p.flights.length, woke, 'the next step is a full step after waking');
  p.clock.advance(1000);
  assert.strictEqual(p.flights.length, woke + 1);
  assert(Math.abs(p.last().at - t - 60000) <= 1, 'anchored to the wake: ' + (p.last().at - t));
  p.destroy();
});

test('a heartbeat on time is not a wake', async () => {
  const p = page();
  await flush();
  p.clock.advance(10 * K.BEAT_MS);
  assert.strictEqual(p.c.stats().wakes, 0);
  p.destroy();
});

section('=== #883 M3 autoReload and budget ===');

test('autoReload is on while a kiosk controller is mounted (off, on across a remount)', async () => {
  const p = page();
  await flush();
  assert.deepStrictEqual(p.autoReload, [true]);
  p.clock.advance(6 * H + 60000);
  await flush();
  assert.deepStrictEqual(p.autoReload, [true, false, true]);
  p.destroy();
  assert.deepStrictEqual(p.autoReload.slice(-1), [false]);
});

test('steady state: at most 3 kiosk timers (step, cursor, heartbeat) at any time over 13 h', async () => {
  const p = page();
  await flush();
  let max = 0;
  for (let t = 0; t < 13 * 60; t++) {           // one sample a minute
    p.clock.advance(60000);
    await flush();
    if (t * 60000 > 20000) max = Math.max(max, p.c.stats().timers);
    if (t % 7 === 0) p.doc.fire('pointermove', {});
  }
  assert.strictEqual(p.remountCalls, 2, 'two remounts in 13 h');
  assert(max <= 3, 'timers ' + max);
  p.destroy();
  assert.strictEqual(p.c.stats().timers, 0);
  assert.strictEqual(K.liveCount(), 0);
});

(async () => {
  for (const item of queue) {
    if (item.title) { console.log('\n' + item.title); continue; }
    try { await item.fn(); passed++; console.log('  ✓ ' + item.name); }
    catch (e) { failed++; console.error('  ✗ ' + item.name + '\n    ' + (e && e.stack || e).toString().split('\n').slice(0, 3).join('\n    ')); }
  }
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
