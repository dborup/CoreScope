/* test-881-live-moments.js
 * #881 M2 — Live "Moments". Unit tests for the real public/live-moments.js,
 * loaded in a vm sandbox with a small fake DOM, fake timers and a fake
 * clock. No browser.
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
const created = [];
class El {
  constructor(tag) {
    this.tagName = tag.toUpperCase(); this.children = []; this.parentNode = null;
    this.attrs = {}; this.className = ''; this.hidden = false; this.style = {};
    this._text = ''; this.listeners = {}; this.href = ''; this.type = '';
    created.push(this);
  }
  get classList() {
    const self = this;
    const list = () => self.className.split(/\s+/).filter(Boolean);
    return {
      contains: (c) => list().includes(c),
      add: (c) => { if (!list().includes(c)) self.className = list().concat(c).join(' '); },
      remove: (c) => { self.className = list().filter((x) => x !== c).join(' '); },
      toggle: (c, on) => { if (on === undefined) on = !list().includes(c); if (on) this.classList.add(c); else this.classList.remove(c); },
    };
  }
  get textContent() { return this._text + this.children.map((c) => c.textContent).join(''); }
  set textContent(v) { this.children.forEach((c) => { c.parentNode = null; }); this.children = []; this._text = String(v); }
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

// ── Fake timers and clock ─────────────────────────────────────────────────
function makeClock() {
  let now = Date.UTC(2026, 10, 18, 12, 0, 0), seq = 0;
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

const PK = (c) => c.repeat(64);
const AT = new Date(Date.UTC(2026, 10, 18, 11, 59, 50)).toISOString();
const nodeM = (id, extra) => Object.assign({ id, kind: 'new_node', at: AT,
  node: { publicKey: PK('a'), name: 'DK_6100_Haderslev', role: 'repeater', lat: 55.25, lon: 9.49 } }, extra || {});
const linkM = (id) => ({ id, kind: 'record_link', at: AT, previousKm: 40,
  link: { a: { publicKey: PK('a'), name: 'A-node', role: 'repeater', lat: 55, lon: 9 },
          b: { publicKey: PK('b'), name: 'B-node', role: 'room', lat: 55.5, lon: 10 }, distanceKm: 61.234 } });

function harness(opts) {
  opts = opts || {};
  const clock = makeClock();
  const doc = makeDoc();
  const root = new El('div');
  const media = Object.assign({ '(max-width: 640px)': false, '(max-height: 419px)': false, '(prefers-reduced-motion: reduce)': false }, opts.media || {});
  const listeners = [];
  const h = { clock, doc, root, media, listeners, flights: [], nav: [], mode: 'LIVE', suppressed: false, fetchResolve: null, settings: opts.settings || null };
  const fetchJSON = opts.fetch === false ? null : () => new Promise((res) => { h.fetchResolve = res; });
  h.c = LM.create({
    doc, root,
    onWS: (fn) => listeners.push(fn),
    offWS: (fn) => { const i = listeners.indexOf(fn); if (i !== -1) listeners.splice(i, 1); },
    fetchJSON,
    now: clock.now, setTimeout: clock.setTimeout, clearTimeout: clock.clearTimeout,
    matchMedia: (q) => ({ matches: !!media[q] }),
    getMode: () => h.mode,
    getZoom: () => 9,
    getSettings: () => h.settings,
    isSuppressed: () => h.suppressed,
    profileOffered: () => !!opts.profile,
    navigate: (href) => h.nav.push(href),
    onFly: (plan, t, m) => h.flights.push({ plan, t, m }),
  });
  h.send = (m) => listeners.slice().forEach((fn) => fn({ type: 'moment', data: m }));
  h.tick = (ms) => clock.advance(ms || 0);
  h.cards = () => root.find('live-moment-card').filter((e) => !e.classList.contains('is-leaving'));
  return h;
}

section('=== live-moments: normalize / describe ===');

test('normalize rejects unknown kinds, missing ids and nodes', () => {
  assert.strictEqual(LM.normalize(null), null);
  assert.strictEqual(LM.normalize({ id: 'x', kind: 'nope', node: { publicKey: 'k' } }), null);
  assert.strictEqual(LM.normalize({ kind: 'new_node', node: { publicKey: 'k' } }), null);
  assert.strictEqual(LM.normalize({ id: 'x', kind: 'new_node' }), null);
  assert.strictEqual(LM.normalize({ id: 'x', kind: 'record_link', link: { a: { publicKey: 'a' } } }), null);
});

test('normalize drops (0,0) and half positions, casts numbers', () => {
  const m = LM.normalize(nodeM('1', { node: { publicKey: 'k', lat: 0, lon: 0 } }));
  assert.strictEqual(m.node.lat, null);
  assert.strictEqual(LM.normalize(nodeM('1', { node: { publicKey: 'k', lat: 55 } })).node.lat, null);
  const b = LM.normalize({ id: 'b', kind: 'back_online', at: AT, silentHours: '37.4', node: { publicKey: 'k' } });
  assert.strictEqual(b.silentHours, 37.4);
});

test('describe covers every kind and "+N more"', () => {
  assert.strictEqual(LM.describe(LM.normalize(nodeM('1', { more: 2 }))).detail, 'DK_6100_Haderslev · repeater +2 more');
  assert.strictEqual(LM.describe(LM.normalize(linkM('2'))).detail, '61.2 km · A-node ↔ B-node');
  assert.strictEqual(LM.describe(LM.normalize({ id: '3', kind: 'back_online', silentHours: 37.4, node: { publicKey: PK('c') } })).detail,
    'cccccccc · silent 37 h');
  const busy = LM.describe(LM.normalize({ id: '4', kind: 'busiest_repeater', relays: 42, windowMinutes: 10, node: { publicKey: 'k', name: 'R' } }));
  assert.strictEqual(busy.title, 'Busiest repeater');
  assert.strictEqual(busy.detail, 'R · 42 relays / 10 min');
});

section('=== live-moments: links, fly plan, initial state ===');

test('detailsHref uses node, profile and neighbor routes, never #/live?node=', () => {
  const n = LM.normalize(nodeM('1'));
  const l = LM.normalize(linkM('2'));
  assert.strictEqual(LM.detailsHref(n, false), '#/nodes/' + PK('a'));
  assert.strictEqual(LM.detailsHref(l, true), '#/map?profile=' + PK('a') + ',' + PK('b'));
  assert.strictEqual(LM.detailsHref(l, false), '#/nodes/' + PK('a') + '?section=node-neighbors');
  const odd = LM.normalize(nodeM('3', { node: { publicKey: 'a/b?node=x#y' } }));
  assert.strictEqual(LM.detailsHref(odd, false), '#/nodes/a%2Fb%3Fnode%3Dx%23y');
  for (const m of [n, l, odd]) for (const p of [true, false]) assert.ok(!/#\/live/.test(LM.detailsHref(m, p)));
});

test('a link to a non-hex key falls back to neighbors even with the profile tool', () => {
  const l = LM.normalize(linkM('2')); l.link.b.publicKey = 'not-a-key';
  assert.strictEqual(LM.detailsHref(l, true), '#/nodes/' + PK('a') + '?section=node-neighbors');
});

test('flyPlan: reduced motion picks setView({animate:false}), otherwise flyTo', () => {
  const t = LM.flyTarget(LM.normalize(nodeM('1')));
  const r = LM.flyPlan(t, true, 9);
  assert.strictEqual(r.method, 'setView');
  assert.deepStrictEqual(JSON.parse(JSON.stringify(r.args)), [[55.25, 9.49], 12, { animate: false }]);
  const f = LM.flyPlan(t, false, 14);
  assert.strictEqual(f.method, 'flyTo');
  assert.strictEqual(f.args[1], 14, 'keeps a closer zoom');
  const lt = LM.flyTarget(LM.normalize(linkM('2')));
  assert.strictEqual(LM.flyPlan(lt, true, 9).method, 'fitBounds');
  assert.strictEqual(LM.flyPlan(lt, true, 9).args[1].animate, false);
  assert.strictEqual(LM.flyPlan(lt, false, 9).args[1].animate, true);
  assert.strictEqual(LM.flyTarget(LM.normalize(nodeM('3', { node: { publicKey: 'k' } }))), null);
});

test('resolveInitialEnabled: ?moments= wins, default on', () => {
  assert.strictEqual(LM.resolveInitialEnabled(null, null), true);
  assert.strictEqual(LM.resolveInitialEnabled(null, 'false'), false);
  assert.strictEqual(LM.resolveInitialEnabled('1', 'false'), true);
  assert.strictEqual(LM.resolveInitialEnabled('0', 'true'), false);
  assert.strictEqual(LM.resolveInitialEnabled('junk', 'false'), false);
});

section('=== live-moments: card stream ===');

test('desktop shows at most 3 cards; the rest queue and follow', () => {
  const h = harness();
  for (let i = 0; i < 5; i++) h.send(nodeM('m' + i));
  h.tick();
  assert.strictEqual(h.cards().length, 3);
  assert.strictEqual(h.c.stats().queued, 2);
  assert.strictEqual(h.root.find('live-moments-more')[0].textContent, '+2');
  h.tick(LM.DWELL_MS);
  assert.strictEqual(h.cards().length, 2, 'the first three left, the two queued took over');
  h.tick(LM.DWELL_MS + 300);
  assert.strictEqual(h.root.find('live-moment-card').length, 0, 'faded cards are removed');
});

test('phone shows one card', () => {
  const h = harness({ media: { '(max-width: 640px)': true } });
  h.send(nodeM('a')); h.send(nodeM('b')); h.tick();
  assert.strictEqual(h.cards().length, 1);
  assert.strictEqual(h.c.stats().queued, 1);
});

const MOUSE = { pointerType: 'mouse' };
const TOUCH = { pointerType: 'touch' };
// The target of a focusin: matches(':focus-visible') is true for keyboard
// focus and false for the focus a click or tap leaves on the button.
const focusFrom = (keyboard) => ({ target: { matches: (q) => q === ':focus-visible' && keyboard } });

test('dwell is 8 s; mouse hover holds the card, leaving resumes the rest', () => {
  const h = harness();
  h.send(nodeM('a')); h.tick();
  h.tick(4000);
  const card = h.cards()[0];
  card.fire('pointerenter', MOUSE);
  h.tick(20000);
  assert.strictEqual(h.cards().length, 1, 'held while hovered');
  card.fire('pointerleave', MOUSE);
  h.tick(3999);
  assert.strictEqual(h.cards().length, 1);
  h.tick(1);
  assert.strictEqual(h.cards().length, 0);
});

test('keyboard focus holds a card like hover; focus and hover together hold until both end', () => {
  const h = harness();
  h.send(nodeM('a')); h.tick();
  const card = h.cards()[0];
  card.fire('focusin', focusFrom(true));
  h.tick(30000);
  assert.strictEqual(h.cards().length, 1, 'held while focused');
  card.fire('pointerenter', MOUSE);
  card.fire('focusout');
  h.tick(30000);
  assert.strictEqual(h.cards().length, 1, 'still held by the hover');
  card.fire('pointerleave', MOUSE);
  h.tick(LM.DWELL_MS);
  assert.strictEqual(h.cards().length, 0, 'left once neither holds it');
});

test('phone tap: a tapped card still leaves after the dwell and the next moment shows', () => {
  const h = harness({ media: { '(max-width: 640px)': true } });
  h.send(nodeM('a')); h.send(nodeM('b')); h.tick();
  const card = h.cards()[0];
  // What a tap fires: a touch pointerenter, focus on the button (not
  // :focus-visible), the click; pointerleave/focusout only come with the
  // next tap elsewhere.
  card.fire('pointerenter', TOUCH);
  card.fire('focusin', focusFrom(false));
  card.find('live-moment-main')[0].fire('click');
  assert.strictEqual(h.flights.length, 1, 'the tap flew');
  h.tick(LM.DWELL_MS);
  assert.deepStrictEqual(h.cards().map((c) => c.getAttribute('data-moment-id')), ['b'],
    'the tapped card left and the queued one took its slot');
  assert.strictEqual(h.c.stats().queued, 0, 'nothing piles up behind it');
});

section('=== live-moments: bounded state (rule 0) ===');

test('the queue keeps the newest 10 waiting moments and drops the oldest', () => {
  const h = harness();
  for (let i = 0; i < 3; i++) h.send(nodeM('q' + i));
  h.tick();
  assert.strictEqual(h.cards().length, 3);
  for (let i = 3; i < 3 + 15; i++) h.send(nodeM('q' + i));
  h.tick();
  assert.strictEqual(h.c.stats().queued, 10, 'queue capped at 10');
  assert.strictEqual(h.root.find('live-moments-more')[0].textContent, '+10');
  // The three cards leave; the next shown are the oldest survivors, q8..q10.
  h.tick(LM.DWELL_MS);
  assert.deepStrictEqual(h.cards().map((c) => c.getAttribute('data-moment-id')), ['q8', 'q9', 'q10'],
    'q3..q7 were dropped from the front of the queue');
});

test('dedup remembers the last 200 ids, so an id older than that is accepted again', () => {
  const h = harness({ fetch: false });
  h.send(nodeM('first'));
  h.send(nodeM('first'));
  assert.strictEqual(h.c.stats().history, 1, 'a repeat inside the window is dropped');
  for (let i = 0; i < 199; i++) h.send(nodeM('d' + i));
  h.send(nodeM('first'));
  assert.strictEqual(h.c.stats().history, LM.DRAWER_LEN, 'still remembered after 199 more');
  h.send(nodeM('d199')); h.tick();   // the 201st id evicts "first"
  h.send(nodeM('first')); h.tick();
  h.c.openDrawer();
  const ids = h.root.find('live-moment-row').map((r) => r.getAttribute('data-moment-id'));
  assert.deepStrictEqual(ids.slice(0, 2), ['first', 'd199'], 'the evicted id is accepted again');
});

test('reduced motion removes a card at once, with no fade', () => {
  const h = harness({ media: { '(prefers-reduced-motion: reduce)': true } });
  h.send(nodeM('a')); h.tick(); h.tick(LM.DWELL_MS);
  assert.strictEqual(h.root.find('live-moment-card').length, 0);
});

test('dedup by id across WS and REST', () => {
  const h = harness();
  h.send(nodeM('a')); h.send(nodeM('a')); h.tick();
  assert.strictEqual(h.c.stats().history, 1);
  assert.strictEqual(h.cards().length, 1);
});

test('pause sends moments to history only and clears the cards', () => {
  const h = harness();
  h.send(nodeM('a')); h.tick();
  h.c.setPaused(true); h.tick();
  assert.strictEqual(h.cards().length, 0);
  h.send(nodeM('b')); h.tick();
  assert.strictEqual(h.cards().length, 0);
  assert.strictEqual(h.c.stats().history, 2);
  const pill = h.root.find('live-moments-count')[0];
  assert.strictEqual(pill.textContent, 'Moments · 2');
  assert.strictEqual(h.root.find('live-moments-pause')[0].getAttribute('aria-pressed'), 'true');
});

test('VCR PAUSED / REPLAY: history only; leaving LIVE drops the cards', () => {
  const h = harness();
  h.send(nodeM('a')); h.tick();
  h.mode = 'PAUSED'; h.c.onModeChange(); h.tick();
  assert.strictEqual(h.cards().length, 0);
  h.send(nodeM('b')); h.mode = 'REPLAY'; h.send(nodeM('c')); h.tick();
  assert.strictEqual(h.cards().length, 0);
  assert.strictEqual(h.c.stats().history, 3);
  h.mode = 'LIVE'; h.c.onModeChange(); h.send(nodeM('d')); h.tick();
  assert.strictEqual(h.cards().length, 1);
});

test('phone: an open sheet or a short viewport sends moments to history only', () => {
  const h = harness({ media: { '(max-width: 640px)': true } });
  h.suppressed = true; h.send(nodeM('a')); h.tick();
  assert.strictEqual(h.cards().length, 0);
  h.suppressed = false; h.media['(max-height: 419px)'] = true; h.send(nodeM('b')); h.tick();
  assert.strictEqual(h.cards().length, 0);
  assert.strictEqual(h.c.stats().history, 2);
});

test('a short landscape viewport wider than a phone also sends moments to history only', () => {
  const h = harness({ media: { '(max-height: 419px)': true } });
  h.send(nodeM('a')); h.tick();
  assert.strictEqual(h.cards().length, 0);
  assert.strictEqual(h.c.stats().history, 1);
});

test('suppression only applies on phones', () => {
  const h = harness();
  h.suppressed = true; h.send(nodeM('a')); h.tick();
  assert.strictEqual(h.cards().length, 1);
});

test('a name with <script> renders as text', () => {
  const h = harness();
  const before = created.length;
  const evil = '<script>alert(1)</script><img src=x onerror=alert(2)>';
  h.send(nodeM('x', { node: { publicKey: PK('d'), name: evil, role: 'room', lat: 55, lon: 9 } }));
  h.tick();
  const made = created.slice(before);
  assert.ok(!made.some((e) => e.tagName === 'SCRIPT' || e.tagName === 'IMG'));
  const detail = h.cards()[0].find('live-moment-detail')[0];
  assert.strictEqual(detail.textContent, evil.slice(0, 64) + ' · room');
  assert.strictEqual(detail.children.length, 0);
});

test('non-moment frames are ignored without work', () => {
  const h = harness();
  h.listeners.forEach((fn) => { fn({ type: 'packet', data: {} }); fn(null); fn({ type: 'moment', data: { bogus: 1 } }); });
  h.tick();
  assert.strictEqual(h.c.stats().history, 0);
  assert.strictEqual(h.clock.pending(), 0);
});

section('=== live-moments: click and drawer ===');

test('click flies via onFly; reduced motion passes setView', () => {
  const h = harness({ media: { '(prefers-reduced-motion: reduce)': true } });
  h.send(nodeM('a')); h.tick();
  h.cards()[0].find('live-moment-main')[0].fire('click');
  assert.strictEqual(h.flights.length, 1);
  assert.strictEqual(h.flights[0].plan.method, 'setView');
  assert.strictEqual(h.flights[0].t.key, PK('a'));
});

test('a moment with no GPS opens its details instead', () => {
  const h = harness();
  h.send(nodeM('a', { node: { publicKey: PK('e'), name: 'n' } })); h.tick();
  h.cards()[0].find('live-moment-main')[0].fire('click');
  assert.strictEqual(h.flights.length, 0);
  assert.deepStrictEqual(h.nav, ['#/nodes/' + PK('e')]);
});

test('card Details link uses the profile route when offered', () => {
  const h = harness({ profile: true });
  h.send(linkM('l')); h.tick();
  assert.strictEqual(h.cards()[0].find('live-moment-link')[0].href, '#/map?profile=' + PK('a') + ',' + PK('b'));
});

test('drawer lists history, Escape closes it and drops its key listener', () => {
  const h = harness();
  for (let i = 0; i < 25; i++) h.send(nodeM('d' + i));
  h.tick();
  h.root.find('live-moments-open')[0].fire('click');
  const drawer = h.root.find('live-moments-drawer')[0];
  assert.strictEqual(drawer.hidden, false);
  assert.strictEqual(h.root.find('live-moment-row').length, 20, 'history capped at 20');
  assert.strictEqual(h.root.find('live-moment-row')[0].getAttribute('data-moment-id'), 'd24', 'newest first');
  assert.strictEqual(h.c.stats().keyListener, true);
  h.doc.fire('keydown', { key: 'Enter' });
  assert.strictEqual(drawer.hidden, false);
  h.doc.fire('keydown', { key: 'Escape' });
  assert.strictEqual(drawer.hidden, true);
  assert.strictEqual((h.doc.listeners.keydown || []).length, 0);
});

test('REST history fills the drawer once, after WS frames that raced it', async () => {
  const h = harness();
  await Promise.resolve();
  h.send(nodeM('ws-new')); h.tick();
  h.fetchResolve({ moments: [nodeM('ws-new'), nodeM('rest-1'), linkM('rest-2')] });
  await new Promise((r) => setImmediate(r));
  h.tick();
  h.c.openDrawer();
  const ids = h.root.find('live-moment-row').map((r) => r.getAttribute('data-moment-id'));
  assert.deepStrictEqual(ids, ['ws-new', 'rest-1', 'rest-2']);
  assert.strictEqual(h.cards().length, 1, 'REST history never becomes cards');
});

section('=== live-moments: teardown ===');

test('destroy unsubscribes, clears every timer and listener, removes the DOM', async () => {
  const base = LM.liveCount();
  const h = harness();
  await Promise.resolve();
  assert.strictEqual(LM.liveCount(), base + 1);
  for (let i = 0; i < 4; i++) h.send(nodeM('t' + i));
  h.tick(); h.c.openDrawer();
  assert.ok(h.clock.pending() > 0);
  h.c.destroy();
  assert.strictEqual(h.listeners.length, 0, 'WS listener removed');
  assert.strictEqual(h.clock.pending(), 0, 'no timer left');
  assert.strictEqual(h.c.stats().timers, 0);
  assert.strictEqual((h.doc.listeners.keydown || []).length, 0, 'Escape listener removed');
  assert.strictEqual(h.root.children.length, 0, 'DOM removed');
  assert.strictEqual(LM.liveCount(), base);
  h.fetchResolve({ moments: [nodeM('late')] });
  await new Promise((r) => setImmediate(r));
  assert.strictEqual(h.c.stats().history, 0, 'a late fetch is ignored');
  h.c.destroy();
  assert.strictEqual(LM.liveCount(), base, 'destroy is idempotent');
});

test('100 mount/destroy cycles leave nothing behind', () => {
  const base = LM.liveCount();
  for (let i = 0; i < 100; i++) {
    const h = harness();
    h.send(nodeM('c' + i)); h.tick(100);
    h.c.destroy();
    assert.strictEqual(h.clock.pending(), 0);
    assert.strictEqual(h.listeners.length, 0);
  }
  assert.strictEqual(LM.liveCount(), base);
});

section('=== live-moments: viewer settings (#881 M3) ===');

test('SETTINGS: keys, defaults = the M2 constants, bounds, valid, resolve', () => {
  const R = LM.SETTINGS;
  assert.deepStrictEqual(JSON.parse(JSON.stringify(R.KEYS)), ['enabled', 'visibleCards', 'dwellSeconds', 'showNewNode', 'showBackOnline', 'showRecordLink', 'showBusiest']);
  assert.deepStrictEqual(JSON.parse(JSON.stringify(R.DEFAULTS)),
    { enabled: true, visibleCards: 3, dwellSeconds: 8, showNewNode: true, showBackOnline: true, showRecordLink: true, showBusiest: true });
  assert.strictEqual(LM.DWELL_MS, 8000);
  [['visibleCards', [1, 5]], ['dwellSeconds', [3, 30]]].forEach(([k, [lo, hi]]) => {
    [lo, hi, lo + 1].forEach((v) => assert.strictEqual(R.valid(k, v), true, k + ' ' + v));
    [lo - 1, hi + 1, 0, -1, 2.5, '4', NaN, Infinity, null, true].forEach((v) => assert.strictEqual(R.valid(k, v), false, k + ' ' + String(v)));
  });
  ['enabled', 'showNewNode', 'showBusiest'].forEach((k) => {
    assert.strictEqual(R.valid(k, false), true);
    [0, 1, 'true', null, undefined].forEach((v) => assert.strictEqual(R.valid(k, v), false, k + ' ' + String(v)));
  });
  ['hasOwnProperty', 'constructor', '__proto__', 'maxPerMinute', 'historySize'].forEach((k) => assert.strictEqual(R.valid(k, 1), false, k));
  assert.deepStrictEqual(JSON.parse(JSON.stringify(R.resolve(null))), JSON.parse(JSON.stringify(R.DEFAULTS)));
  assert.deepStrictEqual(JSON.parse(JSON.stringify(R.resolve({ visibleCards: 9, dwellSeconds: '5', enabled: 'no', junk: 1 }))), JSON.parse(JSON.stringify(R.DEFAULTS)));
  assert.strictEqual(R.resolve({ visibleCards: 2 }).visibleCards, 2);
});

test('every kind has its own show switch', () => {
  assert.deepStrictEqual(JSON.parse(JSON.stringify(Object.keys(LM.KIND_SETTING).sort())), ['back_online', 'busiest_repeater', 'new_node', 'record_link']);
  Object.values(LM.KIND_SETTING).forEach((k) => assert.ok(LM.SETTINGS.KEYS.includes(k), k));
});

test('resolveInitialEnabled: hash > saved choice > customizer default > on', () => {
  assert.strictEqual(LM.resolveInitialEnabled(null, null, false), false, 'default off');
  assert.strictEqual(LM.resolveInitialEnabled(null, null, true), true);
  assert.strictEqual(LM.resolveInitialEnabled(null, null, undefined), true, 'no setting: on');
  assert.strictEqual(LM.resolveInitialEnabled(null, 'true', false), true, 'saved on beats default off');
  assert.strictEqual(LM.resolveInitialEnabled(null, 'false', true), false, 'saved off beats default on');
  assert.strictEqual(LM.resolveInitialEnabled('1', 'false', false), true);
  assert.strictEqual(LM.resolveInitialEnabled('0', 'true', true), false);
});

test('visibleCap: the desktop setting, a phone always one', () => {
  assert.strictEqual(LM.visibleCap(false), 3);
  assert.strictEqual(LM.visibleCap(false, { visibleCards: 5 }), 5);
  assert.strictEqual(LM.visibleCap(true, { visibleCards: 5 }), 1);
});

test('visibleCards: the stack takes the setting, the rest queue', () => {
  const h = harness({ settings: { visibleCards: 2 } });
  for (let i = 0; i < 4; i++) h.send(nodeM('v' + i));
  h.tick();
  assert.strictEqual(h.cards().length, 2);
  assert.strictEqual(h.c.stats().queued, 2);
});

test('visibleCards: a phone ignores a larger setting', () => {
  const h = harness({ settings: { visibleCards: 5 }, media: { '(max-width: 640px)': true } });
  h.send(nodeM('a')); h.send(nodeM('b')); h.tick();
  assert.strictEqual(h.cards().length, 1);
});

test('dwellSeconds: a card fades after the setting, not before', () => {
  const h = harness({ settings: { dwellSeconds: 3 } });
  h.send(nodeM('d')); h.tick();
  h.tick(2900);
  assert.strictEqual(h.cards().length, 1, 'still up at 2.9 s');
  h.tick(200);
  assert.strictEqual(h.cards().length, 0, 'gone after 3 s');
  const slow = harness({ settings: { dwellSeconds: 20 } });
  slow.send(nodeM('d')); slow.tick();
  slow.tick(19000);
  assert.strictEqual(slow.cards().length, 1);
});

test('a bad setting is the default, never a broken stack', () => {
  const h = harness({ settings: { visibleCards: 99, dwellSeconds: -4 } });
  for (let i = 0; i < 4; i++) h.send(nodeM('b' + i));
  h.tick();
  assert.strictEqual(h.cards().length, 3);
  h.tick(7900);
  assert.strictEqual(h.cards().length, 3, '8 s dwell');
});

test('per-kind switch: a hidden kind gets no card, no history, no queue slot', () => {
  const h = harness({ settings: { showRecordLink: false } });
  assert.strictEqual(h.c.ingest(linkM('l1')), false);
  h.send(nodeM('n1')); h.tick();
  assert.strictEqual(h.cards().length, 1);
  assert.strictEqual(h.cards()[0].attrs['data-kind'], 'new_node');
  assert.strictEqual(h.c.stats().history, 1);
  h.c.openDrawer();
  assert.strictEqual(h.root.find('live-moment-row').length, 1);
});

test('per-kind switch: the REST history skips a hidden kind too', () => {
  const h = harness({ settings: { showNewNode: false } });
  h.c.seed([nodeM('s1'), linkM('s2')]);
  assert.strictEqual(h.c.stats().history, 1);
});

test('onSettingsChanged: hidden kinds leave, the cap re-fits, the drawer re-lists', () => {
  const h = harness();
  h.send(nodeM('a')); h.send(linkM('b')); h.send(nodeM('c')); h.send(nodeM('d')); h.tick();
  assert.strictEqual(h.cards().length, 3);
  assert.strictEqual(h.c.stats().queued, 1);
  h.settings = { visibleCards: 1, showRecordLink: false };
  h.c.onSettingsChanged(); h.tick();
  assert.strictEqual(h.cards().every((c) => c.attrs['data-kind'] === 'new_node'), true, 'the record link card is gone');
  assert.strictEqual(h.cards().length, 1, 'cap 1');
  h.c.openDrawer();
  assert.strictEqual(h.root.find('live-moment-row').filter((r) => r.attrs['data-kind'] === 'record_link').length, 0);
  assert.strictEqual(h.root.find('live-moment-row').length, 3, 'the three node moments');
  h.settings = null; h.c.onSettingsChanged();
  assert.strictEqual(h.root.find('live-moment-row').length, 4, 'back to defaults: the history is whole again');
});

test('all kinds hidden: the drawer says there is nothing, no error', () => {
  const h = harness({ settings: { showNewNode: false, showBackOnline: false, showRecordLink: false, showBusiest: false } });
  h.send(nodeM('a')); h.tick(); h.c.openDrawer();
  assert.strictEqual(h.cards().length, 0);
  assert.strictEqual(h.root.find('live-moments-empty').length, 1);
});

test('onSettingsChanged after destroy is a no-op', () => {
  const h = harness(); h.c.destroy(); h.c.onSettingsChanged();
});

(async () => {
  for (const t of queue) {
    if (t.title) { console.log('\n' + t.title); continue; }
    try { await t.fn(); passed++; console.log('  ✅ ' + t.name); }
    catch (e) { failed++; console.log('  ❌ ' + t.name + ': ' + e.message); }
  }
  console.log('\n' + passed + ' passed, ' + failed + ' failed');
  process.exit(failed ? 1 : 0);
})();
