/* test-883-kiosk-moments.js
 * #883 M2 — large Moments in kiosk: the live-moments.js side. Unit tests
 * for the real public/live-moments.js in a vm sandbox with a fake DOM,
 * fake timers and a fake clock (no browser):
 *   visibleCap(isPhone, settings, cap)  kiosk caps the stack at 2
 *   create({ cardCap })                 at most cap cards, the rest queue
 *   refreshTimes()                      card (and open drawer) times follow
 *                                       the clock; writes only on a change
 *   create({ timeRefreshMs })           the refresh timer lives in the
 *                                       controller's own timer set, runs
 *                                       only while a card or the drawer
 *                                       shows, and is gone after destroy()
 *   create({ onMoment })                every valid moment (WS and the
 *                                       REST seed) reaches the kiosk tiles
 * Without the new deps the module behaves exactly as before (no timer).
 */
'use strict';
const fs = require('fs');
const path = require('path');
const vm = require('vm');
const assert = require('assert');

let passed = 0, failed = 0;
const queue = [];
function test(name, fn) { queue.push({ name, fn }); }

let textWrites = 0;
class El {
  constructor(tag) {
    this.tagName = tag.toUpperCase(); this.children = []; this.parentNode = null;
    this.attrs = {}; this.className = ''; this.hidden = false; this.style = {};
    this._text = ''; this.listeners = {}; this.href = ''; this.type = '';
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
  set textContent(v) { textWrites++; this.children.forEach((c) => { c.parentNode = null; }); this.children = []; this._text = String(v); }
  get firstChild() { return this.children[0] || null; }
  appendChild(c) { if (c.parentNode) c.parentNode.removeChild(c); c.parentNode = this; this.children.push(c); return c; }
  removeChild(c) { const i = this.children.indexOf(c); if (i !== -1) this.children.splice(i, 1); c.parentNode = null; return c; }
  setAttribute(k, v) { this.attrs[k] = String(v); }
  getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; }
  addEventListener(t, fn) { (this.listeners[t] = this.listeners[t] || []).push(fn); }
  removeEventListener(t, fn) { this.listeners[t] = (this.listeners[t] || []).filter((f) => f !== fn); }
  fire(t, e) { (this.listeners[t] || []).slice().forEach((fn) => fn(e || {})); }
  focus() {}
  all() { return [this].concat(...this.children.map((c) => c.all())); }
  find(cls) { return this.all().filter((e) => e.classList.contains(cls)); }
}
function makeDoc() {
  const doc = new El('document');
  doc.createElement = (t) => new El(t);
  doc.createElementNS = (_ns, t) => new El(t);
  return doc;
}
function makeClock() {
  let now = Date.UTC(2026, 10, 22, 12, 0, 0), seq = 0;
  const pending = new Map();
  return {
    now: () => now,
    setTimeout: (fn, ms) => { const id = ++seq; pending.set(id, { at: now + (ms || 0), fn }); return id; },
    clearTimeout: (id) => { pending.delete(id); },
    pending: () => pending.size,
    advance(ms) {
      const end = now + ms;
      for (;;) {
        let next = null;
        for (const [id, t] of pending) if (t.at <= end && (!next || t.at < next[1].at)) next = [id, t];
        if (!next) break;
        pending.delete(next[0]); now = next[1].at; next[1].fn();
      }
      now = end;
    },
  };
}
function load() {
  const ctx = { window: {}, console, Date, Math, Number, String, JSON, Array, Object, Set, Map, Promise,
    isFinite, isNaN, encodeURIComponent, AbortController };
  vm.createContext(ctx);
  const f = 'public/live-moments.js';
  vm.runInContext(fs.readFileSync(path.join(__dirname, f), 'utf8'), ctx, { filename: f });
  return ctx.window.LiveMoments;
}
const LM = load();
const flush = () => new Promise((r) => setImmediate(r));

const PK = (c) => c.repeat(64);
const NOW = Date.UTC(2026, 10, 22, 12, 0, 0);
const at = (agoMs) => new Date(NOW - agoMs).toISOString();
const nodeM = (id, agoMs) => ({ id, kind: 'new_node', at: at(agoMs || 0),
  node: { publicKey: PK('a'), name: 'N ' + id, role: 'repeater', lat: 55.25, lon: 9.49 } });
const linkM = (id, agoMs) => ({ id, kind: 'record_link', at: at(agoMs || 0),
  link: { a: { publicKey: PK('a'), name: 'A', lat: 55, lon: 9 }, b: { publicKey: PK('b'), name: 'B', lat: 55.5, lon: 10 }, distanceKm: 61.2 } });

function harness(opts) {
  opts = opts || {};
  const clock = makeClock();
  const doc = makeDoc();
  const root = new El('div');
  const listeners = [];
  const h = { clock, root, listeners, seen: [], settings: opts.settings || null, fetchResolve: null };
  const deps = {
    doc, root,
    onWS: (fn) => listeners.push(fn),
    offWS: (fn) => { const i = listeners.indexOf(fn); if (i !== -1) listeners.splice(i, 1); },
    fetchJSON: () => new Promise((res) => { h.fetchResolve = res; }),
    now: clock.now, setTimeout: clock.setTimeout, clearTimeout: clock.clearTimeout,
    matchMedia: (q) => ({ matches: !!(opts.media && opts.media[q]) }),
    getMode: () => 'LIVE',
    getSettings: () => h.settings,
    isSuppressed: () => false,
  };
  if (opts.cardCap !== undefined) deps.cardCap = opts.cardCap;
  if (opts.timeRefreshMs !== undefined) deps.timeRefreshMs = opts.timeRefreshMs;
  if (opts.onMoment) deps.onMoment = (m) => h.seen.push(m);
  h.c = LM.create(deps);
  h.send = (m) => listeners.slice().forEach((fn) => fn({ type: 'moment', data: m }));
  h.cards = () => root.find('live-moment-card').filter((e) => !e.classList.contains('is-leaving'));
  h.times = () => root.find('live-moment-card').map((c) => c.find('live-moment-time')[0].textContent);
  return h;
}

test('visibleCap: a cap limits the desktop stack, never raises it; the phone keeps one', () => {
  assert.strictEqual(LM.visibleCap(false, { visibleCards: 5 }, 2), 2);
  assert.strictEqual(LM.visibleCap(false, { visibleCards: 1 }, 2), 1);
  assert.strictEqual(LM.visibleCap(false, { visibleCards: 3 }), 3, 'no cap: the setting');
  assert.strictEqual(LM.visibleCap(false, { visibleCards: 3 }, 0), 3, '0 is no cap');
  assert.strictEqual(LM.visibleCap(true, { visibleCards: 5 }, 2), 1);
});

test('cardCap 2: at most two cards with visibleCards 5, the rest queue and follow', () => {
  const h = harness({ cardCap: 2, settings: { visibleCards: 5, dwellSeconds: 8 } });
  for (let i = 0; i < 4; i++) h.send(nodeM('c' + i));
  h.clock.advance(0);
  assert.strictEqual(h.cards().length, 2);
  assert.strictEqual(h.c.stats().queued, 2);
  h.clock.advance(8000 + 250);
  assert.strictEqual(h.cards().length, 2, 'queued ones take the free slots');
  assert.strictEqual(h.c.stats().queued, 0);
  h.c.destroy();
});

test('no cardCap: the viewer setting as before', () => {
  const h = harness({ settings: { visibleCards: 5 } });
  for (let i = 0; i < 6; i++) h.send(nodeM('d' + i));
  h.clock.advance(0);
  assert.strictEqual(h.cards().length, 5);
  h.c.destroy();
});

test('refreshTimes(): visible card times follow the clock; an unchanged time is not rewritten', () => {
  const h = harness({ settings: { dwellSeconds: 30 } });
  h.send(nodeM('t1', 0));
  h.clock.advance(0);
  assert.deepStrictEqual(h.times(), ['now']);
  const w = textWrites;
  assert.strictEqual(h.c.refreshTimes(), 0, 'nothing changed');
  assert.strictEqual(textWrites, w);
  h.clock.advance(20000);
  assert.strictEqual(h.c.refreshTimes(), 1);
  assert.deepStrictEqual(h.times(), ['20 s']);
  assert.strictEqual(textWrites, w + 1, 'one write for the one changed time');
  h.c.destroy();
});

test('refreshTimes() also refreshes an open drawer\'s rows', async () => {
  const h = harness();
  await flush();
  h.fetchResolve({ moments: [linkM('r1', 60000)] });
  await flush();
  h.clock.advance(0);
  h.c.openDrawer();
  const row = () => h.root.find('live-moment-row')[0].find('live-moment-time')[0].textContent;
  assert.strictEqual(row(), '1 min');
  h.clock.advance(4 * 60000);
  h.c.refreshTimes();
  assert.strictEqual(row(), '5 min');
  h.c.destroy();
});

test('timeRefreshMs: one timer in the controller\'s set while a card shows, refreshing; none without a card', () => {
  const h = harness({ timeRefreshMs: 30000, settings: { dwellSeconds: 30 } });
  h.clock.advance(0);
  const idle = h.c.stats().timers;
  h.send(nodeM('k1', 0));
  h.clock.advance(0);
  // dwell timer + the refresh timer
  assert.strictEqual(h.c.stats().timers, idle + 2, JSON.stringify(h.c.stats()));
  h.clock.advance(29000);
  assert.deepStrictEqual(h.times(), ['now'], 'not yet');
  h.clock.advance(1000 - 1);
  h.clock.advance(1);
  // The card leaves at its 30 s dwell; the refresh at 30 s ran first or with it.
  h.clock.advance(250);
  assert.strictEqual(h.cards().length, 0);
  assert.strictEqual(h.c.stats().timers, idle, 'no refresh timer once no card shows: ' + JSON.stringify(h.c.stats()));
  h.c.destroy();
  assert.strictEqual(h.clock.pending(), 0);
});

test('timeRefreshMs: a held card keeps refreshing every interval', () => {
  const h = harness({ timeRefreshMs: 30000, settings: { dwellSeconds: 8 } });
  h.send(nodeM('k2', 0));
  h.clock.advance(0);
  const card = h.cards()[0];
  card.fire('pointerenter', { pointerType: 'mouse' }); // held: the dwell stops
  h.clock.advance(30000);
  assert.deepStrictEqual(h.times(), ['30 s']);
  h.clock.advance(30000);
  assert.deepStrictEqual(h.times(), ['1 min']);
  assert.strictEqual(h.clock.pending(), 1, 'only the refresh timer');
  h.c.destroy();
  assert.strictEqual(h.clock.pending(), 0, 'destroy clears it');
});

test('without timeRefreshMs no refresh timer is ever armed (unchanged outside kiosk)', () => {
  const h = harness({ settings: { dwellSeconds: 8 } });
  h.send(nodeM('k3', 0));
  h.clock.advance(0);
  assert.strictEqual(h.clock.pending(), 1, 'the dwell timer only');
  h.cards()[0].fire('pointerenter', { pointerType: 'mouse' });
  assert.strictEqual(h.clock.pending(), 0);
  h.clock.advance(120000);
  assert.deepStrictEqual(h.times(), ['now'], 'times stay as built, as before');
  h.c.destroy();
});

test('onMoment: every valid moment, from the socket and the seed, normalized; invalid ones never', async () => {
  const h = harness({ onMoment: true, settings: { showRecordLink: false } });
  h.send(linkM('w1', 1000));
  h.send({ id: 'bad', kind: 'nope' });
  await flush();
  h.fetchResolve({ moments: [linkM('s1', 5000), nodeM('s2', 9000)] });
  await flush();
  assert.deepStrictEqual(h.seen.map((m) => m.id), ['w1', 's1', 's2'], 'a kind hidden from the cards still reaches the tiles');
  assert.strictEqual(typeof h.seen[0].at, 'number');
  assert.strictEqual(h.seen[0].link.distanceKm, 61.2);
  h.c.destroy();
  h.send(linkM('late', 0));
  assert.strictEqual(h.seen.length, 3, 'nothing after destroy');
});

test('onMoment that throws does not break the moments controller', () => {
  const h = harness();
  const c = LM.create({ doc: makeDoc(), root: new El('div'), onWS: (fn) => { h.ws = fn; }, offWS: () => {},
    now: h.clock.now, setTimeout: h.clock.setTimeout, clearTimeout: h.clock.clearTimeout,
    matchMedia: () => ({ matches: false }), getMode: () => 'LIVE', onMoment: () => { throw new Error('tile bug'); } });
  h.ws({ type: 'moment', data: nodeM('x1') });
  h.clock.advance(0);
  assert.strictEqual(c.stats().history, 1);
  c.destroy();
  h.c.destroy();
});

(async () => {
  for (const item of queue) {
    try { await item.fn(); passed++; console.log('  ✓ ' + item.name); }
    catch (e) { failed++; console.error('  ✗ ' + item.name + '\n    ' + (e && e.stack || e).toString().split('\n').slice(0, 3).join('\n    ')); }
  }
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
