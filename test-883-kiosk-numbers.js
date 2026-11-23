/* test-883-kiosk-numbers.js
 * #883 M2 — Live kiosk: the big numbers, the kiosk theme and the size
 * tokens. Unit tests for the real public/live-kiosk.js in a vm sandbox
 * with a fake DOM, fake timers and a fake clock (no browser), plus a parse
 * of the real public/live.css and public/style.css:
 *   fmtCount / fmtKm       number formatting
 *   numbersModel()         the tiles' view model: whole-mesh and per-area
 *                          active nodes, a missing area, Moments off → no
 *                          records tile, records older than 24 h dropped
 *   create()               tiles write the DOM only when a value changes;
 *                          the number poll (one bulk call each per 60 s,
 *                          none while hidden, none per step); late answers
 *                          after destroy() are ignored
 *   parseKioskParams()     theme=dark|light|auto, dark by default
 *   applyKioskTheme()      applied without persisting, restored on exit
 *   live.css / style.css   the Moments size tokens default to the old px,
 *                          kiosk scales them; the kiosk tokens; the new
 *                          colours are variables
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

// ── Fake DOM (every text write is counted) ───────────────────────────────
let textWrites = 0;
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
    this.attrs = {}; this.className = ''; this._hidden = false; this.style = {}; this._text = ''; this.type = '';
    this.hiddenWrites = 0;
  }
  get hidden() { return this._hidden; }
  set hidden(v) { this.hiddenWrites++; this._hidden = !!v; }
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
  set textContent(v) { textWrites++; this.children.forEach((c) => { c.parentNode = null; }); this.children = []; this._text = String(v); }
  appendChild(c) { if (c.parentNode) c.parentNode.removeChild(c); c.parentNode = this; this.children.push(c); return c; }
  removeChild(c) { const i = this.children.indexOf(c); if (i !== -1) this.children.splice(i, 1); c.parentNode = null; return c; }
  contains(o) { for (let n = o; n; n = n.parentNode) if (n === this) return true; return false; }
  setAttribute(k, v) { this.attrs[k] = String(v); }
  getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; }
  focus() {}
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
  doc.fullscreenEnabled = false; // no start overlay in these tests
  return doc;
}

// ── Fake timers and clock ─────────────────────────────────────────────────
function makeClock() {
  let now = Date.UTC(2026, 10, 22, 12, 0, 0), seq = 0;
  const pending = new Map();
  return {
    now: () => now,
    setTimeout: (fn, ms) => { const id = ++seq; pending.set(id, { at: now + Math.max(0, ms || 0), fn }); return id; },
    clearTimeout: (id) => { pending.delete(id); },
    pending: () => pending.size,
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
const plain = (v) => JSON.parse(JSON.stringify(v));

const H = 3600 * 1000;
const NOW = Date.UTC(2026, 10, 22, 12, 0, 0);
const PK = (c) => c.repeat(64);
// Moments as LiveMoments.normalize() hands them on (at in ms).
const link = (km, agoMs, a, b) => ({ id: 'l' + km, kind: 'record_link', at: NOW - agoMs, node: null,
  link: { a: { publicKey: PK('a'), name: a || 'Odense-Rpt', role: 'repeater', lat: 55.4, lon: 10.4 },
          b: { publicKey: PK('b'), name: b || '', role: 'room', lat: 55.7, lon: 10.1 }, distanceKm: km } });
const busy = (name, relays, win, agoMs) => ({ id: 'b' + name + agoMs, kind: 'busiest_repeater', at: NOW - agoMs, link: null,
  node: { publicKey: PK('c'), name, role: 'repeater', lat: 55, lon: 10 }, relays, windowMinutes: win });

section('=== #883 M2 formatting ===');

test('fmtCount: whole numbers with thousands separators, "—" when unknown', () => {
  assert.strictEqual(K.fmtCount(0), '0');
  assert.strictEqual(K.fmtCount(142), '142');
  assert.strictEqual(K.fmtCount(1234), '1,234');
  assert.strictEqual(K.fmtCount(1234567), '1,234,567');
  assert.strictEqual(K.fmtCount(3.6), '4', 'rounded');
  assert.strictEqual(K.fmtCount('318'), '318', 'a numeric string is cast at the boundary');
  ['', 'abc', null, undefined, NaN, Infinity, -1, {}].forEach((v) => assert.strictEqual(K.fmtCount(v), '—', String(v)));
});

test('fmtKm: one decimal and the unit, "—" when unknown', () => {
  assert.strictEqual(K.fmtKm(38.24), '38.2 km');
  assert.strictEqual(K.fmtKm(0), '0.0 km');
  assert.strictEqual(K.fmtKm(123.45), '123.5 km');
  [null, undefined, NaN, 'x', -2].forEach((v) => assert.strictEqual(K.fmtKm(v), '—', String(v)));
});

section('=== #883 M2 numbersModel ===');

const DENSITY = [
  { areaKey: 'FYN', label: 'Fyn', total: 60, active: 41, degraded: 3, silent: 16 },
  { areaKey: 'SJL', label: 'Sjælland', total: 200, active: 150, degraded: 9, silent: 41 },
];
const base = (extra) => Object.assign({ rate: 142, status: { total: 400, active: 318 }, density: DENSITY,
  area: { key: 'FYN', label: 'Fyn (step label)' }, momentsOn: true, link: null, busiest: null, now: NOW }, extra || {});

test('rate and whole-mesh active nodes; the current tour area\'s active count from the areas analytics', () => {
  const m = K.numbersModel(base());
  assert.strictEqual(m.rate, '142');
  assert.strictEqual(m.active, '318');
  assert.strictEqual(m.area, 'Fyn 41', 'the server\'s label and density[area].active');
  assert.strictEqual(K.numbersModel(base({ area: { key: 'SJL', label: 'x' } })).area, 'Sjælland 150');
});

test('a missing area: no per-area part (unknown key, no tour, no density, a bad count)', () => {
  assert.strictEqual(K.numbersModel(base({ area: { key: 'NOPE', label: 'Nope' } })).area, '');
  assert.strictEqual(K.numbersModel(base({ area: null })).area, '');
  assert.strictEqual(K.numbersModel(base({ density: null })).area, '');
  assert.strictEqual(K.numbersModel(base({ density: [{ areaKey: 'FYN', label: 'Fyn', active: 'many' }] })).area, '');
  // Keys match exactly, never by label or case (resolveSteps already gave the real key).
  assert.strictEqual(K.numbersModel(base({ area: { key: 'fyn', label: 'Fyn' } })).area, '');
  // An entry without a label falls back to the step's label.
  assert.strictEqual(K.numbersModel(base({ density: [{ areaKey: 'FYN', active: 7 }] })).area, 'Fyn (step label) 7');
});

test('nothing fetched yet: "—", not 0', () => {
  const m = K.numbersModel(base({ rate: null, status: null, density: null }));
  assert.strictEqual(m.rate, '—');
  assert.strictEqual(m.active, '—');
  assert.strictEqual(m.area, '');
});

test('Moments off → no records tile', () => {
  assert.strictEqual(K.numbersModel(base({ momentsOn: false, link: link(38.2, H) })).records, null);
});

test('records: the newest record_link and busiest_repeater, labelled as rolling', () => {
  const m = K.numbersModel(base({ link: link(38.24, H, 'Odense-Rpt', ''), busiest: busy('Rpt-3', 212, 10, 60000) }));
  assert.deepStrictEqual(plain(m.records), {
    link: { label: 'Longest link (24 h)', value: '38.2 km', detail: 'Odense-Rpt ↔ bbbbbbbb' },
    busiest: { label: 'Busiest (10 min) · 1 min ago', value: 'Rpt-3', detail: '212 relays' },
  });
  const nw = K.numbersModel(base({ busiest: busy('', null, null, 0) })).records.busiest;
  assert.deepStrictEqual(plain(nw), { label: 'Busiest repeater · just now', value: 'cccccccc', detail: '' });
});

test('busiest tile shows the moment\'s age, so an old record never reads as "right now" (#949 review #3)', () => {
  const age = (agoMs, win) => K.numbersModel(base({ busiest: busy('Rpt-3', 9, win, agoMs) })).records.busiest.label;
  assert.strictEqual(age(3 * H, 10), 'Busiest (10 min) · 3 h ago');
  assert.strictEqual(age(3 * H, null), 'Busiest repeater · 3 h ago');
  assert.strictEqual(age(42 * 60000, 10), 'Busiest (10 min) · 42 min ago');
  assert.strictEqual(age(24 * H, 10), 'Busiest (10 min) · 24 h ago');
  assert.strictEqual(age(2000, 10), 'Busiest (10 min) · just now');
  // The link tile keeps its plain "24 h" label (it is a record, not a moment in time).
  assert.strictEqual(K.numbersModel(base({ link: link(38, 3 * H) })).records.link.label, 'Longest link (24 h)');
});

test('records older than 24 h are dropped (rolling window); empty shows "—" since restart', () => {
  const m = K.numbersModel(base({ link: link(90, 24 * H + 1), busiest: busy('Old', 5, 10, 25 * H) }));
  const empty = { value: '—', detail: 'since restart' };
  assert.deepStrictEqual(plain(m.records), {
    link: Object.assign({ label: 'Longest link (24 h)' }, empty),
    busiest: Object.assign({ label: 'Busiest repeater' }, empty),
  });
  const edge = K.numbersModel(base({ link: link(90, 24 * H) }));
  assert.strictEqual(edge.records.link.value, '90.0 km', 'exactly 24 h is still inside the window');
  const noAt = Object.assign(link(90, 0), { at: null });
  assert.strictEqual(K.numbersModel(base({ link: noAt })).records.link.value, '—', 'no time: cannot be shown as 24 h');
  assert.strictEqual(K.RECORD_WINDOW_MS, 24 * H);
});

section('=== #883 M2 create(): tiles and the number poll ===');

const AREAS = [
  { key: 'FYN', label: 'Fyn', latMin: 55, latMax: 55.6, lonMin: 9.7, lonMax: 10.9 },
  { key: 'SJL', label: 'Sjælland', latMin: 54.9, latMax: 56.1, lonMin: 11, lonMax: 12.7 },
];

function harness(opts) {
  opts = opts || {};
  const clock = makeClock();
  const doc = makeDoc();
  if (opts.hidden) doc.hidden = true;
  const win = new Target();
  const root = new El('div');
  const h = { clock, doc, win, root, status: 0, areaStats: 0, moments: opts.moments !== false,
    statusBody: { total: 400, active: 318 }, areaBody: { density: DENSITY }, pendingStatus: [] };
  const params = K.parseKioskParams(new URLSearchParams(opts.hash || 'kiosk=1&tour=fyn,SJL&every=60'));
  h.c = K.create({
    doc, win, root, params,
    fetchAreas: () => Promise.resolve(AREAS),
    fetchStatus: () => { h.status++; if (opts.holdStatus) return new Promise((r) => h.pendingStatus.push(r));
      return opts.statusFails ? Promise.reject(new Error('500')) : Promise.resolve(h.statusBody); },
    fetchAreaStats: () => { h.areaStats++; return Promise.resolve(h.areaBody); },
    momentsOn: () => h.moments,
    fly: () => {}, reducedMotion: () => true, getMode: () => 'LIVE',
    requestFullscreen: () => Promise.resolve(), exitFullscreen: () => Promise.resolve(), exit: () => {}, warn: () => {},
    now: clock.now, setTimeout: clock.setTimeout, clearTimeout: clock.clearTimeout,
  });
  h.text = (cls) => root.find(cls).map((e) => e.textContent).join('|');
  h.tile = (name) => root.all().find((e) => e.getAttribute('data-tile') === name);
  h.visibility = (state) => { doc.hidden = state; doc.fire('visibilitychange'); };
  return h;
}

test('tiles render from the injected sources: rate, active (whole mesh + tour area), records', async () => {
  const h = harness();
  await flush();
  h.c.setRate(142);
  h.c.onMoment(link(38.24, H));
  h.c.onMoment(busy('Rpt-3', 212, 10, 60000));
  assert.strictEqual(h.text('live-kiosk-tile-value'), '142|318', 'rate and active');
  assert.strictEqual(h.text('live-kiosk-tile-area'), 'Fyn 41', 'step 1 is FYN');
  assert.strictEqual(h.text('live-kiosk-tile-label'), 'packets/min|active nodes');
  assert.strictEqual(h.text('live-kiosk-record-label'), 'Longest link (24 h)|Busiest (10 min) · 1 min ago');
  assert.strictEqual(h.text('live-kiosk-record-value'), '38.2 km|Rpt-3');
  assert.strictEqual(h.tile('records').hidden, false);
  // The next tour step changes the area part with no request.
  const calls = [h.status, h.areaStats];
  h.clock.advance(60000 - 1);
  h.clock.advance(1);
  assert.strictEqual(h.text('live-kiosk-tile-area'), 'Sjælland 150');
  assert.deepStrictEqual([h.status, h.areaStats], [calls[0] + 1, calls[1] + 1], 'only the 60 s poll, nothing for the step');
  h.c.destroy();
});

test('the tiles are a labelled group, and every text reaches the page as textContent', async () => {
  const h = harness();
  await flush();
  const g = h.root.find('live-kiosk-tiles')[0];
  assert.ok(g, 'a tiles container');
  assert.strictEqual(g.getAttribute('role'), 'group');
  assert.ok(/numbers/i.test(g.getAttribute('aria-label') || ''), g.getAttribute('aria-label'));
  const src = fs.readFileSync(path.join(__dirname, 'public/live-kiosk.js'), 'utf8').replace(/\/\*[\s\S]*?\*\//g, '').replace(/\/\/.*$/gm, '');
  assert.ok(!/innerHTML|insertAdjacentHTML|outerHTML/.test(src), 'no HTML sink in live-kiosk.js');
  h.c.destroy();
});

test('Moments off → the records tile is hidden (and comes back when Moments start)', async () => {
  const h = harness({ moments: false });
  await flush();
  h.c.setRate(1);
  assert.strictEqual(h.tile('records').hidden, true);
  h.moments = true;
  h.c.setRate(2);
  assert.strictEqual(h.tile('records').hidden, false);
  assert.strictEqual(h.text('live-kiosk-record-value'), '—|—');
  h.c.destroy();
});

test('tiles write the DOM only when a value changes', async () => {
  const h = harness();
  await flush();
  h.c.setRate(142);
  const w0 = textWrites, s0 = h.c.stats().tileWrites, hid0 = h.tile('records').hiddenWrites;
  for (let i = 0; i < 50; i++) h.c.setRate(142);  // the 2 s tick with no change
  h.c.onMoment(busy('Rpt-3', 212, 10, 3 * H));     // older than nothing: shown
  const w1 = textWrites;
  h.c.onMoment(busy('Older', 1, 10, 4 * H));       // older than the shown one: ignored
  for (let i = 0; i < 50; i++) h.c.setRate(142);
  assert.strictEqual(textWrites, w1, 'no write for an unchanged value or an older moment');
  assert.ok(w1 - w0 >= 2 && w1 - w0 <= 3, 'one write per changed text (value, detail, label): ' + (w1 - w0));
  assert.strictEqual(h.c.stats().tileWrites - s0, w1 - w0, 'stats().tileWrites counts the same writes');
  assert.strictEqual(h.tile('records').hiddenWrites, hid0, 'hidden is written only when it changes');
  h.c.setRate(143);
  assert.strictEqual(textWrites, w1 + 1, 'a changed rate writes one text');
  h.c.destroy();
});

test('a newer moment replaces the shown record; seed order (newest first) does not matter', async () => {
  const h = harness();
  await flush();
  h.c.onMoment(link(50, 2 * H));
  h.c.onMoment(link(40, 5 * H)); // older (a seed entry): ignored
  assert.strictEqual(h.text('live-kiosk-record-value').split('|')[0], '50.0 km');
  h.c.onMoment(link(61.2, 0));
  assert.strictEqual(h.text('live-kiosk-record-value').split('|')[0], '61.2 km');
  h.c.onMoment({ id: 'x', kind: 'new_node', at: NOW, node: { publicKey: 'k', name: 'n' } }); // not a record
  h.c.onMoment(null);
  assert.strictEqual(h.text('live-kiosk-record-value').split('|')[0], '61.2 km');
  h.c.destroy();
});

test('a shown record ages out after 24 h on the next tick', async () => {
  const h = harness();
  await flush();
  h.c.onMoment(link(38.2, 23 * H));
  assert.strictEqual(h.text('live-kiosk-record-value').split('|')[0], '38.2 km');
  h.clock.advance(H + 1);
  h.c.setRate(0);
  assert.strictEqual(h.text('live-kiosk-record-value').split('|')[0], '—');
  assert.strictEqual(h.text('live-kiosk-record-detail').split('|')[0], 'since restart');
  h.c.destroy();
});

test('the poll: one status and one areas call at start, then one each per 60 s', async () => {
  const h = harness();
  await flush();
  assert.deepStrictEqual([h.status, h.areaStats], [1, 1]);
  h.clock.advance(59999);
  assert.deepStrictEqual([h.status, h.areaStats], [1, 1]);
  h.clock.advance(1);
  assert.deepStrictEqual([h.status, h.areaStats], [2, 2]);
  h.clock.advance(10 * 60000);
  assert.deepStrictEqual([h.status, h.areaStats], [12, 12], '≤ 2 bulk calls per minute');
  assert.strictEqual(K.NUMBERS_POLL_MS, 60000);
  h.c.destroy();
});

test('no tour: the status call only (no areas analytics)', async () => {
  const h = harness({ hash: 'kiosk=1' });
  await flush();
  h.clock.advance(180000);
  assert.deepStrictEqual([h.status, h.areaStats], [4, 0]);
  assert.strictEqual(h.text('live-kiosk-tile-area'), '');
  h.c.destroy();
});

test('hidden: no poll and no poll timer; visible again polls at once when one is due, else at the remainder', async () => {
  const h = harness();
  await flush();
  h.clock.advance(20000);
  h.visibility(true);
  assert.strictEqual(h.c.stats().pollTimer, false, 'the poll timer goes the moment the tab hides');
  h.clock.advance(10 * 60000);
  assert.deepStrictEqual([h.status, h.areaStats], [1, 1], 'nothing while hidden');
  assert.strictEqual(h.c.stats().pollTimer, false, 'no poll timer while hidden');
  h.visibility(false);
  assert.deepStrictEqual([h.status, h.areaStats], [2, 2], 'due: polled on show');
  // Hidden for less than a minute: no extra call, the next one at the remainder.
  h.clock.advance(10000);
  h.visibility(true);
  h.clock.advance(5000);
  h.visibility(false);
  assert.deepStrictEqual([h.status, h.areaStats], [2, 2], 'not due: no call on show');
  h.clock.advance(60000 - 15000 - 1);
  assert.deepStrictEqual([h.status, h.areaStats], [2, 2]);
  h.clock.advance(1);
  assert.deepStrictEqual([h.status, h.areaStats], [3, 3], 'the remainder, 60 s after the last poll');
  h.c.destroy();
});

test('a timer that fires while the tab is hidden (no visibilitychange seen) fetches nothing', async () => {
  const h = harness();
  await flush();
  h.doc.hidden = true; // e.g. a browser that throttles without the event
  h.clock.advance(5 * 60000);
  assert.deepStrictEqual([h.status, h.areaStats], [1, 1]);
  assert.strictEqual(h.c.stats().pollTimer, false, 'and it stops re-arming');
  h.visibility(false);
  assert.deepStrictEqual([h.status, h.areaStats], [2, 2], 'polls again on show');
  h.c.destroy();
});

test('opened in a hidden tab: no poll until the tab shows', async () => {
  const h = harness({ hidden: true });
  await flush();
  h.clock.advance(5 * 60000);
  assert.deepStrictEqual([h.status, h.areaStats], [0, 0]);
  h.visibility(false);
  assert.deepStrictEqual([h.status, h.areaStats], [1, 1]);
  h.c.destroy();
});

test('a failed poll keeps the last value; a late answer after destroy() writes nothing', async () => {
  const h = harness();
  await flush();
  assert.strictEqual(h.text('live-kiosk-tile-value').split('|')[1], '318');
  h.statusBody = null;
  h.clock.advance(60000);
  await flush();
  assert.strictEqual(h.text('live-kiosk-tile-value').split('|')[1], '318', 'a bad body keeps the number');
  h.c.destroy();
  const late = harness({ holdStatus: true });
  await flush();
  late.c.destroy();
  const w = textWrites;
  late.pendingStatus.forEach((r) => r({ active: 999 }));
  await flush();
  assert.strictEqual(textWrites, w, 'nothing written after destroy');
  assert.strictEqual(late.clock.pending(), 0, 'no timer left');
});

test('destroy() removes the tiles and the poll timer; liveCount back to the base', async () => {
  const base = K.liveCount();
  const h = harness();
  await flush();
  assert.ok(h.root.find('live-kiosk-tiles').length === 1);
  h.c.destroy();
  assert.strictEqual(h.root.find('live-kiosk-tiles').length, 0);
  assert.strictEqual(h.clock.pending(), 0);
  assert.strictEqual(K.liveCount(), base);
  h.c.setRate(5); h.c.onMoment(link(5, 0));
  assert.strictEqual(h.root.children.length, 0, 'calls after destroy do nothing');
});

section('=== #883 M2 theme ===');

test('parseKioskParams: theme dark by default; dark, light and auto; anything else is dark', () => {
  const p = (q) => K.parseKioskParams(new URLSearchParams(q)).theme;
  assert.strictEqual(p('kiosk=1'), 'dark');
  assert.strictEqual(p('kiosk=1&theme=dark'), 'dark');
  assert.strictEqual(p('kiosk=1&theme=light'), 'light');
  assert.strictEqual(p('kiosk=1&theme=auto'), 'auto');
  ['', 'DARK', 'blue', 'auto ', '1'].forEach((v) => assert.strictEqual(p('kiosk=1&theme=' + encodeURIComponent(v)), 'dark', v));
  assert.strictEqual(K.parseKioskParams(null).theme, 'dark');
});

test('stripKioskParams also drops theme and keeps the rest', () => {
  assert.strictEqual(K.stripKioskParams('#/live?kiosk=1&theme=light&region=SJC&tour=a'), '#/live?region=SJC');
});

function themeHost(initial) {
  const h = { theme: initial, applied: [] };
  h.o = { get: () => h.theme, apply: (t) => { h.applied.push(t); h.theme = t; } };
  return h;
}

test('applyKioskTheme: applies dark or light through the injected (non-persisting) apply; restore puts the viewer\'s back', () => {
  const h = themeHost('light');
  const restore = K.applyKioskTheme('dark', h.o);
  assert.strictEqual(h.theme, 'dark');
  assert.strictEqual(typeof restore, 'function');
  restore();
  assert.strictEqual(h.theme, 'light');
  assert.deepStrictEqual(h.applied, ['dark', 'light']);
  const l = themeHost('dark');
  K.applyKioskTheme('light', l.o)();
  assert.deepStrictEqual(l.applied, ['light', 'dark']);
});

test('applyKioskTheme: auto and an already-matching theme change nothing', () => {
  const a = themeHost('light');
  assert.strictEqual(K.applyKioskTheme('auto', a.o), null);
  const b = themeHost('dark');
  assert.strictEqual(K.applyKioskTheme('dark', b.o), null);
  assert.deepStrictEqual(a.applied.concat(b.applied), []);
  assert.strictEqual(K.applyKioskTheme('dark', null), null, 'no host');
  assert.strictEqual(K.applyKioskTheme('dark', { get: () => 'light' }), null, 'no apply function');
});

test('applyKioskTheme: a theme the viewer changed meanwhile is left alone', () => {
  const h = themeHost('light');
  const restore = K.applyKioskTheme('dark', h.o);
  h.theme = 'light'; // e.g. another tab's toggle (app.js storage sync)
  restore();
  assert.deepStrictEqual(h.applied, ['dark']);
  restore();
  assert.deepStrictEqual(h.applied, ['dark'], 'restore runs once');
});

test('app.js: applyThemeTransient sets data-theme without writing meshcore-theme; applyTheme persists through it', () => {
  const src = fs.readFileSync(path.join(__dirname, 'public/app.js'), 'utf8');
  const m = src.match(/function applyThemeTransient\(theme\) \{([\s\S]*?)\n  \}/);
  assert.ok(m, 'applyThemeTransient in app.js');
  assert.ok(/setAttribute\('data-theme', theme\)/.test(m[1]) && /reapplyUserThemeVars\(theme === 'dark'\)/.test(m[1]), m[1]);
  assert.ok(!/localStorage|sessionStorage/.test(m[1]), 'transient: no storage write');
  assert.ok(/window\.applyThemeTransient = applyThemeTransient/.test(src), 'exported for live.js');
  const a = src.match(/function applyTheme\(theme\) \{([\s\S]*?)\n  \}/);
  assert.ok(a && /applyThemeTransient\(theme\)/.test(a[1]) && /localStorage\.setItem\('meshcore-theme', theme\)/.test(a[1]), 'applyTheme = transient + persist');
});

section('=== #883 M2 CSS tokens ===');

const strip = (s) => s.replace(/\/\*[\s\S]*?\*\//g, '');
const liveCss = strip(fs.readFileSync(path.join(__dirname, 'public/live.css'), 'utf8'));
const styleCss = strip(fs.readFileSync(path.join(__dirname, 'public/style.css'), 'utf8'));
// Innermost rules: [selector, body]. Rules inside @media come out with their
// own selector; the media query is in `media` (the nearest enclosing one).
function rules(css) {
  const out = [];
  const re = /([^{}]+)\{([^{}]*)\}/g;
  let m;
  while ((m = re.exec(css))) {
    const before = css.slice(0, m.index);
    const open = before.lastIndexOf('@media');
    const media = open !== -1 && (before.slice(open).match(/\{/g) || []).length > (before.slice(open).match(/\}/g) || []).length
      ? before.slice(open, before.indexOf('{', open)).trim() : '';
    out.push({ sel: m[1].trim(), body: m[2], media });
  }
  return out;
}
const LIVE = rules(liveCss);
const decl = (body, prop) => { const m = body.match(new RegExp('(?:^|;|\\s)' + prop.replace(/[-]/g, '\\-') + '\\s*:\\s*([^;]+)')); return m ? m[1].trim() : null; };
const ruleFor = (sel, pred) => LIVE.filter((r) => r.sel.split(',').map((s) => s.trim()).includes(sel) && (!pred || pred(r)));

// The px values the Moments cards had before the tokens (#881 M2 live.css,
// .ph-icon's 16px for the icon).
const OLD = { '--moment-stack-width': '320px', '--moment-title-size': '12px', '--moment-detail-size': '12px',
  '--moment-icon-size': '16px', '--moment-pad': '6px 8px' };

test('the Moments size tokens default to the old px values, outside kiosk', () => {
  for (const [tok, px] of Object.entries(OLD)) {
    const defs = LIVE.filter((r) => decl(r.body, tok) !== null && !/body\.kiosk/.test(r.sel));
    assert.strictEqual(defs.length, 1, tok + ' defined once outside kiosk, got ' + defs.map((d) => d.sel).join(' / '));
    assert.strictEqual(defs[0].media, '', tok + ' default is not media-gated');
    assert.ok(defs[0].sel.split(',').map((s) => s.trim()).includes('.live-moments'), tok + ' on .live-moments: ' + defs[0].sel);
    assert.strictEqual(decl(defs[0].body, tok), px, tok);
  }
});

test('the card rules read the tokens (no px left behind)', () => {
  const use = (sel, prop, tok) => {
    const r = ruleFor(sel, (x) => x.media === '' && decl(x.body, prop) !== null);
    assert.ok(r.length, sel + ' { ' + prop + ' }');
    assert.ok(r.some((x) => decl(x.body, prop).includes('var(' + tok + ')')), sel + ' ' + prop + ': ' + r.map((x) => decl(x.body, prop)).join(' / '));
  };
  use('.live-moments', 'width', '--moment-stack-width');
  use('.live-moment-title', 'font-size', '--moment-title-size');
  use('.live-moment-detail', 'font-size', '--moment-detail-size');
  use('.live-moment-time', 'font-size', '--moment-detail-size');
  use('.live-moment-main .ph-icon', 'font-size', '--moment-icon-size');
  use('.live-moment-main', 'padding', '--moment-pad');
});

test('kiosk scales the Moments tokens by --kiosk-scale (desktop and TV, not the phone rule)', () => {
  const k = LIVE.filter((r) => /body\.kiosk \.live-moments\b/.test(r.sel) && decl(r.body, '--moment-title-size') !== null);
  assert.strictEqual(k.length, 1, 'one kiosk token rule');
  Object.keys(OLD).forEach((tok) => assert.ok(/var\(--kiosk-scale\)/.test(decl(k[0].body, tok) || ''), tok + ': ' + decl(k[0].body, tok)));
  assert.ok(/min-width:\s*641px/.test(k[0].media), 'desktop/TV only: ' + k[0].media);
});

test('the kiosk tokens: --kiosk-scale 1 and a 3 % safe inset, larger steps on big screens', () => {
  const root = LIVE.filter((r) => r.sel === 'body.kiosk' && r.media === '' && decl(r.body, '--kiosk-scale') !== null);
  assert.strictEqual(root.length, 1);
  assert.strictEqual(decl(root[0].body, '--kiosk-scale'), '1');
  assert.strictEqual(decl(root[0].body, '--kiosk-safe-inset'), '3vmin', 'owner decision 8: 3 %');
  const steps = LIVE.filter((r) => r.sel === 'body.kiosk' && r.media && decl(r.body, '--kiosk-scale') !== null)
    .map((r) => ({ media: r.media, v: Number(decl(r.body, '--kiosk-scale')) }));
  assert.ok(steps.length >= 2, JSON.stringify(steps));
  const vals = steps.map((s) => s.v);
  assert.deepStrictEqual(vals.slice().sort((a, b) => a - b), vals, 'ascending, so the larger screen wins');
  assert.ok(vals.every((v) => v > 1 && v <= 2), JSON.stringify(vals));
  assert.ok(steps.some((s) => s.v === 1.5 && /min-height:\s*1000px/.test(s.media)), '1080p is 1.5: ' + JSON.stringify(steps));
});

test('the kiosk credit sits inside the safe inset with a scaled font', () => {
  const b = LIVE.filter((r) => /body\.kiosk #liveMap \.leaflet-bottom/.test(r.sel) && decl(r.body, 'bottom') !== null);
  assert.ok(b.length && /var\(--kiosk-safe-inset\)/.test(decl(b[0].body, 'bottom')), 'bottom inset');
  const r = LIVE.filter((x) => /body\.kiosk #liveMap \.leaflet-right/.test(x.sel) && decl(x.body, 'right') !== null);
  assert.ok(r.length && /var\(--kiosk-safe-inset\)/.test(decl(r[0].body, 'right')), 'right inset');
  const f = LIVE.filter((x) => /body\.kiosk #liveMap \.leaflet-control-attribution/.test(x.sel) && decl(x.body, 'font-size') !== null);
  assert.ok(f.length && /var\(--kiosk-scale\)/.test(decl(f[0].body, 'font-size')), 'credit font scales');
});

test('new colours are CSS variables defined in style.css; the kiosk CSS has no colour literal', () => {
  ['--kiosk-tile-bg', '--kiosk-number'].forEach((v) => {
    const n = styleCss.split('\n').filter((l) => new RegExp('^\\s*' + v + ':').test(l)).length;
    assert.strictEqual(n, 1, v + ' defined ' + n + ' times in style.css');
  });
  const start = liveCss.indexOf('.live-kiosk-tiles');
  assert.ok(start > 0, 'tile rules in live.css');
  const kioskCss = LIVE.filter((r) => /kiosk/.test(r.sel)).map((r) => r.body).join('\n');
  assert.ok(!/#[0-9a-fA-F]{3,8}\b|rgba?\(|hsla?\(/.test(kioskCss), 'a colour literal in the kiosk CSS');
  const tiles = LIVE.filter((r) => r.sel === '.live-kiosk-tile');
  assert.ok(tiles.some((r) => /var\(--kiosk-tile-bg\)/.test(r.body)), 'tile background token');
  assert.ok(LIVE.some((r) => r.sel === '.live-kiosk-tile-value' && /var\(--kiosk-number\)/.test(r.body)), 'number colour token');
});

section('=== #883 M2 customizer (AGENTS.md theming: every new colour in THEME_CSS_MAP) ===');

const KIOSK_TOKENS = { kioskTileBg: '--kiosk-tile-bg', kioskNumber: '--kiosk-number' };
function loadCust() {
  const { loadCustomizer } = require('./tests/customizer-sandbox');
  return loadCustomizer({});
}

test('THEME_CSS_MAP maps both kiosk colours, outside preset detection', () => {
  const { api } = loadCust();
  Object.entries(KIOSK_TOKENS).forEach(([k, v]) => assert.strictEqual(api.THEME_CSS_MAP[k], v, k));
  assert.deepStrictEqual(plain(api.KIOSK_COLOR_KEYS), Object.keys(KIOSK_TOKENS));
});

test('a kiosk colour override reaches the page as its CSS variable', () => {
  const c = loadCust();
  c.api.init({});
  c.api.writeOverrides({ theme: { kioskNumber: '#112233' }, themeDark: { kioskTileBg: '#223344' } });
  c.api.applyCSS(c.api.computeEffective({}, c.api.readOverrides()), c.api.readOverrides());
  assert.strictEqual(c.rootStyle._props['--kiosk-number'], '#112233');
});

test('the Theme tab lists the kiosk colours as their own group, with labels and hints', () => {
  const src = fs.readFileSync(path.join(__dirname, 'public/customize-v2.js'), 'utf8');
  assert.ok(/cust-kiosk-colors/.test(src) && /KIOSK_COLOR_KEYS\.length/.test(src), 'a details group in _renderTheme');
  Object.keys(KIOSK_TOKENS).forEach((k) => {
    assert.ok(new RegExp('\\b' + k + ": 'Live kiosk").test(src), k + ' hint');
    assert.ok(new RegExp('\\b' + k + ": '[A-Z]").test(src), k + ' label');
  });
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
