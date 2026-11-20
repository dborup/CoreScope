/* test-883-kiosk-tour.js
 * #883 M1 — Live kiosk: the tour scheduler and the controller. Unit tests
 * for the real public/live-kiosk.js in a vm sandbox with a fake DOM, fake
 * timers and a fake clock (no browser):
 *   createTour()  one chained setTimeout per step, re-anchor on a late fire,
 *                 pause on interaction / hidden / not LIVE, resume at the
 *                 next step, 0 and 1 steps arm no timer
 *   create()      areas → steps, reduced motion, idle cursor, start
 *                 overlay, Escape two-step, exit button, skipped note,
 *                 retry on ws-open, destroy() → liveCount() === 0
 */
'use strict';
const fs = require('fs');
const path = require('path');
const vm = require('vm');
const assert = require('assert');

let passed = 0, failed = 0;
const queue = [];
function test(name, fn) { queue.push({ name, fn }); }
function section(title) { queue.push({ title }); }

// ── Fake DOM ──────────────────────────────────────────────────────────────
class Target {
  constructor() { this.listeners = {}; }
  addEventListener(t, fn) { (this.listeners[t] = this.listeners[t] || []).push(fn); }
  removeEventListener(t, fn) { this.listeners[t] = (this.listeners[t] || []).filter((f) => f !== fn); }
  count() { return Object.values(this.listeners).reduce((n, a) => n + a.length, 0); }
  fire(t, e) {
    e = Object.assign({ type: t, defaultPrevented: false, preventDefault() { this.defaultPrevented = true; } }, e || {});
    (this.listeners[t] || []).slice().forEach((fn) => fn(e));
    return e;
  }
}
class El extends Target {
  constructor(tag) {
    super();
    this.tagName = tag.toUpperCase(); this.children = []; this.parentNode = null;
    this.attrs = {}; this.className = ''; this.hidden = false; this.style = {}; this._text = ''; this.type = '';
    this.focused = false;
  }
  get classList() {
    const self = this;
    const list = () => self.className.split(/\s+/).filter(Boolean);
    return {
      contains: (c) => list().includes(c),
      add: (c) => { if (!list().includes(c)) self.className = list().concat(c).join(' '); },
      remove: (c) => { self.className = list().filter((x) => x !== c).join(' '); },
      toggle: (c, on) => { if (on === undefined) on = !list().includes(c); if (on) self.classList.add(c); else self.classList.remove(c); },
    };
  }
  get textContent() { return this._text + this.children.map((c) => c.textContent).join(''); }
  set textContent(v) { this.children.forEach((c) => { c.parentNode = null; }); this.children = []; this._text = String(v); }
  appendChild(c) { if (c.parentNode) c.parentNode.removeChild(c); c.parentNode = this; this.children.push(c); return c; }
  removeChild(c) { const i = this.children.indexOf(c); if (i !== -1) this.children.splice(i, 1); c.parentNode = null; return c; }
  contains(o) { for (let n = o; n; n = n.parentNode) if (n === this) return true; return false; }
  setAttribute(k, v) { this.attrs[k] = String(v); }
  getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; }
  focus() { this.focused = true; }
  click() { this.fire('click', { target: this }); }
  all() { return [this].concat(...this.children.map((c) => c.all())); }
  find(cls) { return this.all().filter((e) => e.classList.contains(cls)); }
}
function makeDoc() {
  const doc = new El('document');
  doc.body = new El('body');
  doc.body.className = 'kiosk';
  doc.createElement = (t) => new El(t);
  doc.hidden = false;
  doc.fullscreenElement = null;
  doc.fullscreenEnabled = true;
  return doc;
}

// ── Fake timers and clock ─────────────────────────────────────────────────
function makeClock() {
  let now = Date.UTC(2026, 10, 19, 12, 0, 0), seq = 0;
  const pending = new Map();
  return {
    now: () => now,
    setTimeout: (fn, ms) => { const id = ++seq; pending.set(id, { at: now + Math.max(0, ms || 0), fn }); return id; },
    armed: () => seq, // setTimeout calls so far
    clearTimeout: (id) => { pending.delete(id); },
    pending: () => pending.size,
    // jump: move the wall clock without firing (sleep, a throttled tab).
    jump(ms) { now += ms; },
    advance(ms) {
      const end = now + ms;
      for (;;) {
        let next = null;
        for (const [id, t] of pending) if (t.at <= end && (!next || t.at < next[1].at)) next = [id, t];
        if (!next) break;
        pending.delete(next[0]); now = Math.max(now, next[1].at); next[1].fn();
      }
      now = end;
    },
  };
}

function load() {
  const ctx = { window: {}, console, Date, Math, Number, String, JSON, Array, Object, Set, Map, Promise,
    isFinite, isNaN, parseInt, URLSearchParams };
  vm.createContext(ctx);
  const f = 'public/live-kiosk.js';
  vm.runInContext(fs.readFileSync(path.join(__dirname, f), 'utf8'), ctx, { filename: f });
  return ctx.window.LiveKiosk;
}
const K = load();
const flush = () => new Promise((r) => setImmediate(r));

// ── Tour harness ──────────────────────────────────────────────────────────
function tour(count, every, extra) {
  const clock = makeClock();
  const shown = [];
  const t = K.createTour(Object.assign({
    count, everyMs: every * 1000, resumeAfterMs: K.resumeAfterMs(every),
    setTimeout: clock.setTimeout, clearTimeout: clock.clearTimeout, now: clock.now,
    show: (i) => shown.push(i),
  }, extra || {}));
  return { t, clock, shown };
}

section('=== #883 createTour: scheduling ===');

test('advances every N s through the steps and wraps; one timer at a time', () => {
  const { t, clock, shown } = tour(3, 60);
  t.start();
  assert.deepStrictEqual(shown, [0]);
  assert.strictEqual(clock.pending(), 1);
  clock.advance(59999);
  assert.deepStrictEqual(shown, [0]);
  clock.advance(1);
  assert.deepStrictEqual(shown, [0, 1]);
  clock.advance(120000);
  assert.deepStrictEqual(shown, [0, 1, 2, 0]);
  assert.strictEqual(clock.pending(), 1, 'a chained setTimeout, never more than one');
  assert.strictEqual(t.stats().step, 0);
});

test('0 steps: nothing shown, no timer; 1 step: shown once, no timer', () => {
  const a = tour(0, 60); a.t.start();
  assert.deepStrictEqual(a.shown, []); assert.strictEqual(a.clock.pending(), 0);
  const b = tour(1, 60); b.t.start();
  assert.deepStrictEqual(b.shown, [0]); assert.strictEqual(b.clock.pending(), 0);
  b.t.interact(); b.clock.advance(600000);
  assert.deepStrictEqual(b.shown, [0], 'a single step is not re-fitted');
  assert.strictEqual(b.clock.pending(), 0);
});

test('the cadence holds: a small lateness does not drift the next step', () => {
  const { t, clock, shown } = tour(3, 60);
  t.start();
  clock.jump(60500); // the timer fires 500 ms late
  clock.advance(0);
  assert.deepStrictEqual(shown, [0, 1]);
  clock.advance(59499);
  assert.deepStrictEqual(shown, [0, 1], 'next boundary is the old anchor + 60 s');
  clock.advance(1);
  assert.deepStrictEqual(shown, [0, 1, 2]);
  assert.strictEqual(t.stats().reanchors, 0);
});

test('a fire late by more than 2 × every re-anchors to now, no catch-up', () => {
  const { t, clock, shown } = tour(3, 60);
  t.start();
  clock.jump(60000 + 150000); // asleep for 2.5 × every past the boundary
  clock.advance(0);
  assert.deepStrictEqual(shown, [0, 1], 'one step, not the three that were missed');
  assert.strictEqual(t.stats().reanchors, 1);
  clock.advance(59999);
  assert.deepStrictEqual(shown, [0, 1]);
  clock.advance(1);
  assert.deepStrictEqual(shown, [0, 1, 2], 'a full step from the re-anchor');
});

section('=== #883 createTour: pausing ===');

test('interaction pauses; resumes after resumeAfter at the next step', () => {
  const { t, clock, shown } = tour(3, 60);
  t.start();
  clock.advance(10000);
  t.interact();
  assert.strictEqual(t.stats().paused, true);
  clock.advance(119999); // resumeAfter(60) = 120 s
  assert.deepStrictEqual(shown, [0], 'no step while paused');
  clock.advance(1);
  assert.deepStrictEqual(shown, [0, 1], 'resumed at the next step');
  assert.strictEqual(t.stats().paused, false);
  clock.advance(60000);
  assert.deepStrictEqual(shown, [0, 1, 2], 'a full step after the resume');
});

test('further interaction pushes the resume out, without a timer per event', () => {
  const { t, clock, shown } = tour(3, 15);
  t.start();
  t.interact();
  const armed = clock.pending(), calls = clock.armed();
  for (let i = 0; i < 50; i++) { clock.advance(1000); t.interact(); }
  assert.strictEqual(clock.pending(), armed, 'one resume timer, not one per event');
  assert.strictEqual(clock.armed(), calls, 'no setTimeout per event');
  clock.advance(59999);
  assert.deepStrictEqual(shown, [0]);
  clock.advance(1);
  assert.deepStrictEqual(shown, [0, 1]);
});

test('hidden pauses with no timer armed; visible resumes at the next step without catch-up', () => {
  const { t, clock, shown } = tour(3, 60);
  t.start();
  t.interact();
  t.setHidden(true);
  assert.strictEqual(clock.pending(), 0, 'no tour or resume timer while hidden');
  clock.jump(3600000);
  t.setHidden(false);
  assert.deepStrictEqual(shown, [0, 1], 'one step on return, not 60');
  assert.strictEqual(clock.pending(), 1);
  clock.advance(60000);
  assert.deepStrictEqual(shown, [0, 1, 2]);
});

test('not LIVE (VCR paused, replay, Follow) pauses; LIVE again resumes at the next step', () => {
  const { t, clock, shown } = tour(3, 60);
  t.start();
  t.setBlocked(true);
  assert.strictEqual(clock.pending(), 0);
  clock.advance(600000);
  assert.deepStrictEqual(shown, [0]);
  t.setBlocked(true); // idempotent
  t.setBlocked(false);
  assert.deepStrictEqual(shown, [0, 1]);
  t.setBlocked(false); // idempotent: no extra step
  assert.deepStrictEqual(shown, [0, 1]);
});

test('an interaction that ends while not LIVE does not resume the tour', () => {
  const { t, clock, shown } = tour(3, 15);
  t.start();
  t.interact();
  t.setBlocked(true);
  clock.advance(600000);
  assert.deepStrictEqual(shown, [0]);
  assert.strictEqual(t.stats().paused, true);
  t.setBlocked(false);
  assert.deepStrictEqual(shown, [0, 1]);
});

test('destroy clears every timer and stops for good', () => {
  const { t, clock, shown } = tour(3, 15);
  t.start(); t.interact();
  t.destroy();
  assert.strictEqual(clock.pending(), 0);
  t.setBlocked(false); t.setHidden(false); t.interact();
  clock.advance(600000);
  assert.deepStrictEqual(shown, [0]);
  assert.strictEqual(clock.pending(), 0);
});

// ── Controller harness ────────────────────────────────────────────────────
const AREAS = [
  { key: 'CCC', label: 'Area C', latMin: 57, latMax: 58, lonMin: 9, lonMax: 10 },
  { key: 'AAA', label: 'Area A', latMin: 55, latMax: 56, lonMin: 9, lonMax: 10 },
  { key: 'BBB', label: 'Area B', polygon: [[55, 11], [56, 12], [55.5, 12.5]] },
];

function harness(opts) {
  opts = opts || {};
  const clock = makeClock();
  const doc = makeDoc();
  const win = new Target();
  const root = new El('div');
  if (opts.hidden) doc.hidden = true;
  if (opts.noFullscreen) doc.fullscreenEnabled = false;
  const map = new Target();
  map.on = (t, fn) => map.addEventListener(t, fn);
  map.off = (t, fn) => map.removeEventListener(t, fn);
  const h = { clock, doc, win, root, map, flights: [], exits: 0, fsRequests: 0, fsExits: 0, warns: [], fetches: 0,
    mode: 'LIVE', reduced: !!opts.reduced };
  const params = K.parseKioskParams(new URLSearchParams(opts.hash || 'kiosk=1&tour=aaa,bbb,ccc&every=60'));
  h.c = K.create({
    doc, win, root, map, params,
    fetchAreas: () => { h.fetches++; return opts.fetch ? opts.fetch(h.fetches) : Promise.resolve(AREAS); },
    fly: (bounds, o) => { h.flights.push({ bounds: JSON.parse(JSON.stringify(bounds)), animate: o.animate });
      if (opts.flyFires) map.fire('zoomstart'); },
    reducedMotion: () => h.reduced,
    getMode: () => h.mode,
    requestFullscreen: () => { h.fsRequests++; return opts.fsReject ? Promise.reject(new Error('denied')) : Promise.resolve(); },
    exitFullscreen: () => { h.fsExits++; doc.fullscreenElement = null; return Promise.resolve(); },
    exit: () => { h.exits++; },
    warn: (m) => h.warns.push(m),
    now: clock.now, setTimeout: clock.setTimeout, clearTimeout: clock.clearTimeout,
  });
  h.labels = () => h.flights.length;
  h.text = (cls) => root.find(cls).map((e) => e.textContent).join('|');
  h.key = (key, extra) => doc.fire('keydown', Object.assign({ key, target: doc.body }, extra || {}));
  return h;
}

section('=== #883 create(): tour from the areas ===');

test('steps follow the URL; the first fit is immediate; area 2 after every s', async () => {
  const h = harness();
  await flush();
  assert.strictEqual(h.fetches, 1);
  assert.deepStrictEqual(h.flights.map((f) => f.bounds), [[[55, 9], [56, 10]]]);
  assert.strictEqual(h.text('live-kiosk-step-label'), 'Area A');
  assert.strictEqual(h.text('live-kiosk-step-count'), '1 / 3');
  h.clock.advance(60000);
  assert.deepStrictEqual(h.flights[1].bounds, [[55, 11], [56, 12.5]]);
  assert.strictEqual(h.text('live-kiosk-step-label'), 'Area B');
  assert.strictEqual(h.text('live-kiosk-step-count'), '2 / 3');
  assert.strictEqual(h.fetches, 1, 'no request per step');
  h.c.destroy();
});

test('reduced motion → animate: false; otherwise animate: true', async () => {
  const a = harness({ reduced: true }); await flush();
  assert.strictEqual(a.flights[0].animate, false);
  assert.strictEqual(a.root.find('live-kiosk-progress').length, 0, 'no progress bar under reduced motion');
  a.c.destroy();
  const b = harness(); await flush();
  assert.strictEqual(b.flights[0].animate, true);
  assert.strictEqual(b.root.find('live-kiosk-progress').length, 1);
  b.c.destroy();
});

test('unknown keys are skipped: warned once each and noted on screen for 10 s', async () => {
  const h = harness({ hash: 'kiosk=1&tour=aaa,nope,bbb,nope,zzz' });
  await flush();
  assert.strictEqual(h.c.stats().steps, 2);
  assert.strictEqual(h.warns.length, 2, h.warns.join('; '));
  assert.ok(/nope/.test(h.warns[0]) && /zzz/.test(h.warns[1]));
  assert.strictEqual(h.text('live-kiosk-note'), 'Skipped: nope, zzz');
  h.clock.advance(9999);
  assert.strictEqual(h.root.find('live-kiosk-note').length, 1);
  h.clock.advance(1);
  assert.strictEqual(h.root.find('live-kiosk-note').length, 0);
  h.c.destroy();
});

test('no tour: a static view, no fit and no tour timer', async () => {
  const h = harness({ hash: 'kiosk=1' });
  await flush();
  assert.strictEqual(h.fetches, 0, 'no areas request without a tour');
  assert.strictEqual(h.flights.length, 0);
  assert.strictEqual(h.root.find('live-kiosk-step').length, 0);
  h.c.destroy();
});

test('one valid step: fitted once, no indicator count, no tour timer', async () => {
  const h = harness({ hash: 'kiosk=1&tour=ccc' });
  await flush();
  assert.strictEqual(h.flights.length, 1);
  assert.strictEqual(h.text('live-kiosk-step-label'), 'Area C');
  assert.strictEqual(h.text('live-kiosk-step-count'), '');
  h.clock.advance(3600000);
  assert.strictEqual(h.flights.length, 1);
  h.c.destroy();
});

test('areas request fails: 0 steps, retried once on the next ws-open', async () => {
  const h = harness({ fetch: (n) => n === 1 ? Promise.reject(new Error('503')) : Promise.resolve(AREAS) });
  await flush();
  assert.strictEqual(h.flights.length, 0);
  assert.strictEqual(h.win.count(), 1, 'one ws-open listener');
  h.win.fire('corescope:ws-open');
  await flush();
  assert.strictEqual(h.fetches, 2);
  assert.strictEqual(h.flights.length, 1, 'the tour starts after the retry');
  assert.strictEqual(h.win.count(), 0, 'the retry listener is gone');
  h.win.fire('corescope:ws-open');
  await flush();
  assert.strictEqual(h.fetches, 2, 'retried once only');
  h.c.destroy();
});

test('areas request fails twice: the retry is not retried again', async () => {
  const h = harness({ fetch: () => Promise.reject(new Error('503')) });
  await flush();
  h.win.fire('corescope:ws-open');
  await flush();
  assert.strictEqual(h.fetches, 2);
  assert.strictEqual(h.win.count(), 0, 'no listener after the failed retry');
  h.win.fire('corescope:ws-open');
  await flush();
  assert.strictEqual(h.fetches, 2);
  assert.strictEqual(h.flights.length, 0);
  h.c.destroy();
});

test('a late areas response after destroy does nothing', async () => {
  let resolve;
  const h = harness({ fetch: () => new Promise((r) => { resolve = r; }) });
  h.c.destroy();
  resolve(AREAS);
  await flush();
  assert.strictEqual(h.flights.length, 0);
  assert.strictEqual(h.clock.pending(), 0);
});

section('=== #883 create(): pausing from the page ===');

test('pointerdown, wheel and keys pause; the tour resumes at the next step', async () => {
  for (const ev of ['pointerdown', 'wheel', 'keydown']) {
    const h = harness();
    await flush();
    h.doc.fire(ev, { key: 'ArrowLeft', target: h.doc.body });
    assert.strictEqual(h.c.stats().paused, true, ev);
    h.clock.advance(119999);
    assert.strictEqual(h.flights.length, 1, ev + ': no step while paused');
    h.clock.advance(1);
    assert.strictEqual(h.flights.length, 2, ev + ': resumed');
    h.c.destroy();
  }
});

test('a user drag or zoom on the map pauses; the tour\'s own fit does not', async () => {
  const h = harness({ flyFires: true });
  await flush();
  assert.strictEqual(h.c.stats().paused, false, 'own zoomstart ignored');
  h.clock.advance(60000);
  assert.strictEqual(h.c.stats().paused, false);
  h.map.fire('dragstart');
  assert.strictEqual(h.c.stats().paused, true);
  h.c.destroy();
  const z = harness(); await flush();
  z.map.fire('zoomstart');
  assert.strictEqual(z.c.stats().paused, true);
  z.c.destroy();
});

test('hidden → paused, no tour or cursor timer; visible → next step', async () => {
  const h = harness();
  await flush();
  h.doc.hidden = true; h.doc.fire('visibilitychange');
  const s = h.c.stats();
  assert.strictEqual(s.paused, true);
  assert.strictEqual(s.timers, h.c.stats().overlay ? 1 : 0, 'only the overlay timeout may stay: ' + JSON.stringify(s));
  h.clock.jump(7200000);
  h.doc.hidden = false; h.doc.fire('visibilitychange');
  assert.strictEqual(h.flights.length, 2);
  h.c.destroy();
});

test('a kiosk opened in a hidden tab: no step timer until shown, then the tour starts', async () => {
  // review PR#939 F3 (mutant R6: no setHidden(doc.hidden) after start()).
  const h = harness({ hidden: true, noFullscreen: true });
  await flush();
  const s = h.c.stats();
  assert.ok(s.paused && s.reasons.includes('hidden'), JSON.stringify(s));
  assert.strictEqual(s.timers, 0, 'no step or cursor timer while hidden: ' + JSON.stringify(s));
  h.clock.advance(600000);
  assert.strictEqual(h.flights.length, 1, 'the first fit only, no step while hidden');
  h.doc.hidden = false; h.doc.fire('visibilitychange');
  assert.strictEqual(h.flights.length, 2, 'shown: the tour moves on');
  assert.deepStrictEqual(h.c.stats().reasons, []);
  h.clock.advance(60000);
  assert.strictEqual(h.flights.length, 3, 'and keeps stepping');
  h.c.destroy();
});

test('onModeChange: not LIVE pauses, LIVE resumes', async () => {
  const h = harness();
  await flush();
  h.mode = 'PAUSED'; h.c.onModeChange();
  h.clock.advance(600000);
  assert.strictEqual(h.flights.length, 1);
  h.mode = 'LIVE'; h.c.onModeChange();
  assert.strictEqual(h.flights.length, 2);
  h.mode = 'REPLAY'; h.c.onModeChange();
  assert.strictEqual(h.c.stats().paused, true);
  h.c.destroy();
});

test('Follow pauses the tour; its end resumes at the next step', async () => {
  // review PR#939 F4: Follow starts in kiosk from a path popup or a feed
  // card. live.js reports it as getMode() 'FOLLOW' (liveMode(); the E2E
  // pins that wiring).
  const h = harness();
  await flush();
  h.mode = 'FOLLOW'; h.c.onModeChange();
  const s = h.c.stats();
  assert.ok(s.paused && s.reasons.includes('mode'), JSON.stringify(s));
  h.clock.advance(600000);
  assert.strictEqual(h.flights.length, 1, 'no step while following');
  h.mode = 'LIVE'; h.c.onModeChange();
  assert.strictEqual(h.flights.length, 2, 'resumed at the next step');
  assert.strictEqual(h.c.stats().paused, false);
  h.c.destroy();
});

section('=== #883 create(): cursor, overlay and exit ===');

test('idle cursor after 3 s without pointermove; a move shows it again', async () => {
  const h = harness();
  await flush();
  assert.strictEqual(h.doc.body.classList.contains('kiosk-idle'), false);
  h.clock.advance(2999);
  assert.strictEqual(h.doc.body.classList.contains('kiosk-idle'), false);
  h.clock.advance(1);
  assert.strictEqual(h.doc.body.classList.contains('kiosk-idle'), true);
  h.clock.advance(5000);
  h.doc.fire('pointermove');
  assert.strictEqual(h.doc.body.classList.contains('kiosk-idle'), false);
  assert.strictEqual(h.c.stats().paused, false, 'a move does not pause the tour');
  const before = h.clock.pending();
  for (let i = 0; i < 100; i++) { h.clock.advance(10); h.doc.fire('pointermove'); }
  assert.strictEqual(h.clock.pending(), before, 'no timer per move');
  h.clock.advance(2999);
  assert.strictEqual(h.doc.body.classList.contains('kiosk-idle'), false);
  h.clock.advance(1);
  assert.strictEqual(h.doc.body.classList.contains('kiosk-idle'), true);
  h.c.destroy();
  assert.strictEqual(h.doc.body.classList.contains('kiosk-idle'), false, 'destroy shows the cursor');
});

test('start overlay: shown when fullscreen is possible, focused, a click requests fullscreen and closes it', async () => {
  const h = harness();
  await flush();
  const btn = h.root.find('live-kiosk-start')[0];
  assert.ok(btn, 'overlay button');
  assert.ok(btn.focused, 'focused for Enter');
  assert.strictEqual(h.c.stats().overlay, true);
  btn.click();
  assert.strictEqual(h.fsRequests, 1);
  assert.strictEqual(h.c.stats().overlay, false);
  assert.strictEqual(h.root.find('live-kiosk-overlay').length, 0);
  h.c.destroy();
});

test('start overlay: a click on it does not pause the tour; a rejected request is caught', async () => {
  const h = harness({ fsReject: true });
  await flush();
  const btn = h.root.find('live-kiosk-start')[0];
  h.doc.fire('pointerdown', { target: btn });
  btn.click();
  await flush();
  assert.strictEqual(h.c.stats().paused, false);
  assert.strictEqual(h.fsRequests, 1);
  h.c.destroy();
});

test('start overlay: hidden after 15 s without a gesture; absent when already fullscreen or unsupported', async () => {
  const h = harness();
  await flush();
  h.clock.advance(14999);
  assert.strictEqual(h.c.stats().overlay, true);
  h.clock.advance(1);
  assert.strictEqual(h.c.stats().overlay, false);
  h.c.destroy();
  for (const set of [(d) => { d.fullscreenElement = {}; }, (d) => { d.fullscreenEnabled = false; }]) {
    // Build one whose document is already fullscreen / cannot go fullscreen.
    const doc = makeDoc(); set(doc);
    const c = K.create({ doc, win: new Target(), root: new El('div'), params: K.parseKioskParams(new URLSearchParams('kiosk=1')),
      fetchAreas: () => Promise.resolve([]), fly: () => {}, reducedMotion: () => false, getMode: () => 'LIVE',
      requestFullscreen: () => Promise.resolve(), exitFullscreen: () => Promise.resolve(), exit: () => {}, warn: () => {},
      now: Date.now, setTimeout: (f, ms) => setTimeout(f, ms), clearTimeout: (id) => clearTimeout(id) });
    assert.strictEqual(c.stats().overlay, false);
    c.destroy();
  }
});

test('F requests fullscreen (a gesture) without pausing; the #1532 toggle does not see it', async () => {
  const h = harness();
  await flush();
  const e = h.key('f');
  assert.strictEqual(h.fsRequests, 1);
  assert.strictEqual(e.defaultPrevented, true);
  assert.strictEqual(h.c.stats().paused, false);
  assert.strictEqual(h.c.stats().overlay, false);
  const typing = h.key('f', { target: h.doc.createElement('input') });
  assert.strictEqual(typing.defaultPrevented, false, 'not while typing');
  h.c.destroy();
});

test('Escape: leaves browser fullscreen first, then exits kiosk', async () => {
  const h = harness();
  await flush();
  h.doc.fullscreenElement = {};
  const e1 = h.key('Escape');
  assert.strictEqual(e1.defaultPrevented, true, 'the #1532 Escape handler does not see it');
  assert.strictEqual(h.fsExits, 1);
  assert.strictEqual(h.exits, 0);
  assert.strictEqual(h.c.stats().paused, false, 'Escape does not pause');
  h.key('Escape');
  assert.strictEqual(h.exits, 1);
  h.c.destroy();
});

test('the Exit kiosk button exits; it is hidden with the cursor', async () => {
  const h = harness();
  await flush();
  const btn = h.root.find('live-kiosk-exit')[0];
  assert.ok(btn);
  assert.strictEqual(btn.textContent, 'Exit kiosk');
  btn.click();
  assert.strictEqual(h.exits, 1);
  h.c.destroy();
});

section('=== #883 create(): teardown ===');

test('destroy removes every listener, timer and node; liveCount() back to 0', async () => {
  const base = K.liveCount();
  const h = harness({ hash: 'kiosk=1&tour=aaa,nope,bbb&every=15' });
  await flush();
  assert.strictEqual(K.liveCount(), base + 1);
  h.doc.fire('pointerdown', { target: h.doc.body });
  const s = h.c.stats();
  assert.ok(s.listeners > 0 && s.timers > 0, JSON.stringify(s));
  h.c.destroy();
  h.c.destroy(); // idempotent
  assert.strictEqual(K.liveCount(), base);
  assert.strictEqual(h.doc.count(), 0, 'document listeners');
  assert.strictEqual(h.win.count(), 0, 'window listeners');
  assert.strictEqual(h.map.count(), 0, 'map listeners');
  assert.strictEqual(h.clock.pending(), 0, 'timers');
  assert.strictEqual(h.root.children.length, 0, 'nodes');
  const after = h.c.stats();
  assert.strictEqual(after.listeners, 0);
  assert.strictEqual(after.timers, 0);
  assert.strictEqual(after.destroyed, true);
});

test('create and destroy 10 times leaves no listener behind', async () => {
  const base = K.liveCount();
  const doc = makeDoc(); const win = new Target();
  for (let i = 0; i < 10; i++) {
    const clock = makeClock();
    const c = K.create({ doc, win, root: new El('div'), params: K.parseKioskParams(new URLSearchParams('kiosk=1&tour=aaa,bbb')),
      fetchAreas: () => Promise.reject(new Error('x')), fly: () => {}, reducedMotion: () => false, getMode: () => 'LIVE',
      requestFullscreen: () => Promise.resolve(), exitFullscreen: () => Promise.resolve(), exit: () => {}, warn: () => {},
      now: clock.now, setTimeout: clock.setTimeout, clearTimeout: clock.clearTimeout });
    await flush();
    c.destroy();
  }
  assert.strictEqual(doc.count(), 0);
  assert.strictEqual(win.count(), 0);
  assert.strictEqual(K.liveCount(), base);
});

(async () => {
  for (const item of queue) {
    if (item.title) { console.log('\n' + item.title); continue; }
    try { await item.fn(); passed++; console.log('  ✓ ' + item.name); }
    catch (e) { failed++; console.error('  ✗ ' + item.name + '\n    ' + (e && e.stack || e)); }
  }
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
