/* test-958-movable-moments.js
 * #958 — the Live Moments window moves like the Feed panel. Unit tests for
 * the real public/live-moments.js, public/drag-manager.js and live.js's
 * panel-corner block, each loaded in a vm sandbox with a small fake DOM.
 *
 *   1. live-moments.js: with deps.panelId the window carries the drag-panel
 *      hooks (id, .live-overlay, the bar as .panel-header, a corner
 *      <button>); without it (kiosk) the DOM is exactly as before.
 *   2. drag-manager.js: unregister() releases a panel's header listeners and
 *      its slot; restorePositions(list) restores only the panels named.
 *   3. live.js: liveMoments is a PANEL_DEFAULTS panel anchored at 'bc'; its
 *      corner button cycles bc → tl → tr → br → bl → bc and never blocks a
 *      corner for the other panels; reset puts it back; attachMomentsPanel
 *      registers it once, restores a saved position only where a panel can
 *      be dragged, and does nothing at all in kiosk.
 *
 * Usage: node test-958-movable-moments.js
 */
'use strict';
const fs = require('fs');
const path = require('path');
const vm = require('vm');
const assert = require('assert');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.log('  ✗ ' + name + '\n    ' + (e && e.message)); }
}

const DRAG_MQ = '(pointer: fine) and (min-width: 768px)';

// ── Fake DOM ──────────────────────────────────────────────────────────────
class El {
  constructor(tag) {
    this.tagName = String(tag).toUpperCase(); this.children = []; this.parentNode = null;
    this.attrs = {}; this.className = ''; this.hidden = false; this.style = {};
    this._text = ''; this.listeners = {}; this.type = ''; this.id = '';
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
  get dataset() {
    const self = this;
    return new Proxy({}, {
      get: (_, k) => self.attrs['data-' + String(k)],
      set: (_, k, v) => { self.attrs['data-' + String(k)] = String(v); return true; },
    });
  }
  get textContent() { return this._text + this.children.map((c) => c.textContent).join(''); }
  set textContent(v) { this.children = []; this._text = String(v); }
  get firstChild() { return this.children[0] || null; }
  appendChild(c) { if (c.parentNode) c.parentNode.removeChild(c); c.parentNode = this; this.children.push(c); return c; }
  insertBefore(c, ref) {
    if (c.parentNode) c.parentNode.removeChild(c);
    const i = ref ? this.children.indexOf(ref) : -1;
    c.parentNode = this;
    if (i === -1) this.children.push(c); else this.children.splice(i, 0, c);
    return c;
  }
  removeChild(c) { const i = this.children.indexOf(c); if (i !== -1) this.children.splice(i, 1); c.parentNode = null; return c; }
  setAttribute(k, v) { this.attrs[k] = String(v); }
  getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; }
  hasAttribute(k) { return k in this.attrs; }
  removeAttribute(k) { delete this.attrs[k]; }
  addEventListener(t, fn) { (this.listeners[t] = this.listeners[t] || []).push(fn); }
  removeEventListener(t, fn) { this.listeners[t] = (this.listeners[t] || []).filter((f) => f !== fn); }
  listenerCount() { return Object.values(this.listeners).reduce((n, l) => n + l.length, 0); }
  focus() {}
  setPointerCapture() {}
  releasePointerCapture() {}
  getBoundingClientRect() {
    const l = parseFloat(this.style.left) || 0, t = parseFloat(this.style.top) || 0;
    return { left: l, top: t, right: l + 320, bottom: t + 60, width: 320, height: 60 };
  }
  all() { return [this].concat(...this.children.map((c) => c.all())); }
  find(cls) { return this.all().filter((e) => e.classList.contains(cls)); }
  // the selectors this file needs: '.cls' and '.cls[attr]'
  querySelector(sel) {
    const m = /^\.([\w-]+)(?:\[([\w-]+)\])?$/.exec(sel);
    if (!m) return null;
    return this.all().find((e) => e !== this && e.classList.contains(m[1]) && (!m[2] || e.hasAttribute(m[2]))) || null;
  }
}
function makeDoc() {
  const doc = new El('document');
  doc.createElement = (t) => new El(t);
  doc.createElementNS = (_ns, t) => new El(t);
  return doc;
}
function makeStorage() {
  const s = {};
  return {
    getItem: (k) => (k in s ? s[k] : null),
    setItem: (k, v) => { s[k] = String(v); },
    removeItem: (k) => { delete s[k]; },
    _s: s,
  };
}

// ── 1. live-moments.js ────────────────────────────────────────────────────
function loadMoments() {
  const ctx = { window: {}, console, Date, Math, Number, String, JSON, Array, Object, Set, Map, Promise,
    isFinite, isNaN, encodeURIComponent, AbortController };
  vm.createContext(ctx);
  const f = 'public/live-moments.js';
  vm.runInContext(fs.readFileSync(path.join(__dirname, f), 'utf8'), ctx, { filename: f });
  return ctx.window.LiveMoments;
}
function momentsRoot(panelId) {
  const LM = loadMoments();
  const doc = makeDoc();
  const root = new El('div');
  const c = LM.create({
    doc, root, panelId,
    onWS: () => {}, offWS: () => {}, fetchJSON: null,
    now: () => 0, setTimeout: () => 1, clearTimeout: () => {},
    matchMedia: () => ({ matches: false }),
  });
  return { c, root, wrap: root.find('live-moments')[0] };
}

console.log('\n#958 live-moments.js: drag-panel hooks');

test('with panelId the window is a drag panel: id, .live-overlay, the bar as .panel-header', () => {
  const { wrap } = momentsRoot('liveMoments');
  assert.strictEqual(wrap.id, 'liveMoments');
  assert.ok(wrap.classList.contains('live-overlay'), 'wrap is a .live-overlay (handleResize, the media gate)');
  const header = wrap.querySelector('.panel-header');
  assert.ok(header, 'a .panel-header for DragManager.register');
  assert.ok(header.classList.contains('live-moments-bar'), 'the header is the bar');
});

test('the corner button is a <button> in the bar, tagged with the panel id', () => {
  const { wrap } = momentsRoot('liveMoments');
  const bar = wrap.querySelector('.live-moments-bar');
  const btn = bar.querySelector('.panel-corner-btn');
  assert.ok(btn, 'corner button in the bar');
  // DragManager ignores a pointerdown on a button: it must be one.
  assert.strictEqual(btn.tagName, 'BUTTON');
  assert.strictEqual(btn.type, 'button');
  assert.strictEqual(btn.getAttribute('data-panel'), 'liveMoments');
  assert.ok(btn.getAttribute('aria-label'), 'labelled');
  // Every control in the bar is a button, so none of them starts a drag.
  bar.children.filter((e) => e.tagName !== 'SPAN').forEach((e) => assert.strictEqual(e.tagName, 'BUTTON'));
});

test('without panelId (kiosk) the window has no drag hooks at all', () => {
  const { wrap } = momentsRoot(undefined);
  assert.strictEqual(wrap.id, '');
  assert.ok(!wrap.classList.contains('live-overlay'));
  assert.strictEqual(wrap.querySelector('.panel-header'), null);
  assert.strictEqual(wrap.querySelector('.panel-corner-btn'), null);
});

test('destroy() removes the panel with the rest', () => {
  const { c, root } = momentsRoot('liveMoments');
  c.destroy();
  assert.strictEqual(root.find('live-moments').length, 0);
});

// ── 2. drag-manager.js ────────────────────────────────────────────────────
function loadDrag(storage) {
  const doc = new El('document');
  const ctx = { window: { innerWidth: 1000, innerHeight: 800 }, document: doc, localStorage: storage, console, Math, JSON };
  ctx.window.document = doc;
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'public/drag-manager.js'), 'utf8'), ctx, { filename: 'drag-manager.js' });
  return { DragManager: ctx.window.DragManager, doc };
}
function dragPanel(id) {
  const p = new El('div');
  p.id = id;
  p.appendChild(new El('div')).className = 'panel-header';
  return p;
}

console.log('\n#958 drag-manager.js: unregister and a scoped restore');

test('unregister() removes every header listener and the panel', () => {
  const { DragManager } = loadDrag(makeStorage());
  const m = new DragManager();
  const p = dragPanel('liveMoments');
  const header = p.querySelector('.panel-header');
  m.register(p);
  assert.strictEqual(m._panels.length, 1);
  assert.ok(header.listenerCount() >= 4, 'pointerdown/move/up/cancel bound');
  m.unregister(p);
  assert.strictEqual(m._panels.length, 0);
  assert.strictEqual(header.listenerCount(), 0);
  m.unregister(p); // twice is a no-op
  assert.strictEqual(m._panels.length, 0);
});

test('register/unregister cycles do not grow the panel list', () => {
  const { DragManager } = loadDrag(makeStorage());
  const m = new DragManager();
  m.register(dragPanel('liveFeed'));
  for (let i = 0; i < 5; i++) { const p = dragPanel('liveMoments'); m.register(p); m.unregister(p); }
  assert.strictEqual(m._panels.length, 1);
});

test('unregister() mid-drag drops the drag and its Escape listener', () => {
  const { DragManager, doc } = loadDrag(makeStorage());
  const m = new DragManager();
  const p = dragPanel('liveMoments');
  m.register(p);
  const header = p.querySelector('.panel-header');
  const target = { closest: () => null };
  header.listeners.pointerdown[0]({ button: 0, target, clientX: 10, clientY: 10, pointerId: 1, preventDefault() {} });
  assert.strictEqual(m.state, 'PENDING');
  assert.strictEqual((doc.listeners.keydown || []).length, 1);
  m.unregister(p);
  assert.strictEqual(m.state, 'IDLE');
  assert.strictEqual((doc.listeners.keydown || []).length, 0);
});

test('restorePositions(list) restores only the panels named', () => {
  const st = makeStorage();
  st.setItem('panel-drag-liveFeed', JSON.stringify({ xPct: 0.1, yPct: 0.1 }));
  st.setItem('panel-drag-liveMoments', JSON.stringify({ xPct: 0.5, yPct: 0.25 }));
  const { DragManager } = loadDrag(st);
  const m = new DragManager();
  const feed = dragPanel('liveFeed'), mom = dragPanel('liveMoments');
  m.register(feed); m.register(mom);
  m.restorePositions([mom]);
  assert.strictEqual(mom.style.left, '500px');
  assert.strictEqual(mom.style.top, '200px');
  assert.strictEqual(mom.getAttribute('data-dragged'), 'true');
  assert.strictEqual(feed.style.left, undefined, 'the feed is left alone');
  m.restorePositions();
  assert.strictEqual(feed.style.left, '100px', 'no argument still restores all');
});

// ── 3. live.js panel corners ──────────────────────────────────────────────
function loadLive(opts) {
  opts = opts || {};
  const storage = makeStorage();
  const elements = {};
  const doc = {
    getElementById: (id) => elements[id] || null,
    querySelectorAll: () => [],
    querySelector: () => null,
    documentElement: { getAttribute: () => null, style: {} },
    addEventListener: () => {}, removeEventListener: () => {},
    createElement: (t) => new El(t), createElementNS: (_n, t) => new El(t),
    body: { appendChild() {}, classList: { add() {}, remove() {} }, style: {} },
    head: { appendChild() {} },
  };
  const media = { fine: opts.fine !== false };
  const ctx = {
    document: doc, localStorage: storage, console, setTimeout, clearTimeout, setInterval, clearInterval,
    requestAnimationFrame: (cb) => setTimeout(cb, 0), cancelAnimationFrame: clearTimeout,
    matchMedia: (q) => ({ matches: q === DRAG_MQ && media.fine, addEventListener() {}, removeEventListener() {} }),
    navigator: { userAgent: '' }, performance: { now: () => Date.now() },
    MutationObserver: class { observe() {} disconnect() {} },
    ResizeObserver: class { observe() {} disconnect() {} },
    IntersectionObserver: class { observe() {} disconnect() {} },
    registerPage: () => {}, escapeHtml: (s) => String(s || ''), timeAgo: () => '—',
    getParsedPath: () => [], getParsedDecoded: () => ({}),
    TYPE_COLORS: {}, ROLE_COLORS: {}, ROLE_LABELS: {}, ROLE_STYLE: {}, ROLE_SORT: [],
    formatTimestampWithTooltip: () => '', getTimestampMode: () => 'relative',
    fetch: () => Promise.resolve({ ok: true, json: () => Promise.resolve([]) }),
    Image: class {}, HTMLElement: class {}, Event: class {},
    Number, String, Array, Object, JSON, Math, Date, RegExp, Error, Map, Set, WeakMap, Promise, Symbol,
    parseInt, parseFloat, isNaN, isFinite, encodeURIComponent, decodeURIComponent, queueMicrotask,
  };
  ctx.window = ctx; ctx.self = ctx; ctx.globalThis = ctx;
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'public/payload-labels.js'), 'utf8'), ctx);
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'public/drag-manager.js'), 'utf8'), ctx, { filename: 'drag-manager.js' });
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'public/live.js'), 'utf8'), ctx, { filename: 'live.js', timeout: 3000 });
  const add = (id) => {
    const p = dragPanel(id);
    const b = p.querySelector('.panel-header').appendChild(new El('button'));
    b.className = 'panel-corner-btn';
    b.setAttribute('data-panel', id);
    elements[id] = p;
    return p;
  };
  return { pc: ctx._panelCorner, storage, media, add, ctx };
}
function fakeMgr() {
  const calls = [];
  return {
    calls,
    register: (el) => calls.push(['register', el]),
    unregister: (el) => calls.push(['unregister', el]),
    restorePositions: (list) => calls.push(['restore', list]),
  };
}

console.log('\n#958 live.js: the Moments window as a corner panel');

test('liveMoments is a panel: default anchor bc, named Moments; the others keep theirs', () => {
  const { pc } = loadLive();
  assert.strictEqual(pc.PANEL_DEFAULTS.liveMoments, 'bc');
  assert.strictEqual(pc.PANEL_DEFAULTS.liveFeed, 'bl');
  assert.strictEqual(pc.PANEL_DEFAULTS.liveLegend, 'br');
  assert.strictEqual(pc.PANEL_DEFAULTS.liveNodeDetail, 'tr');
  assert.strictEqual(pc.getPanelPositions().liveMoments, 'bc');
});

test('the corner button cycles bc → tl → tr → br → bl → bc and saves each', () => {
  const { pc, storage, add } = loadLive();
  const el = add('liveMoments');
  const seen = [];
  for (let i = 0; i < 5; i++) {
    pc.onCornerClick('liveMoments');
    seen.push(el.getAttribute('data-position'));
    assert.strictEqual(storage.getItem('panel-corner-liveMoments'), seen[i]);
  }
  assert.deepStrictEqual(seen, ['tl', 'tr', 'br', 'bl', 'bc']);
  assert.ok(/Moments/.test(el.querySelector('.panel-corner-btn').getAttribute('aria-label')));
});

test('a corner click clears a saved drag of the Moments window', () => {
  const { pc, storage, add } = loadLive();
  const el = add('liveMoments');
  el.setAttribute('data-dragged', 'true');
  el.style.top = '300px';
  storage.setItem('panel-drag-liveMoments', '{"xPct":0.3,"yPct":0.3}');
  pc.onCornerClick('liveMoments');
  assert.strictEqual(el.getAttribute('data-dragged'), null);
  assert.strictEqual(el.style.top, '');
  assert.strictEqual(storage.getItem('panel-drag-liveMoments'), null);
});

test('the Moments window never blocks a corner for the other panels', () => {
  const { pc } = loadLive();
  // Moments sits below the panels (z-index 900), so it is no occupant.
  const pos = { liveFeed: 'bl', liveLegend: 'br', liveNodeDetail: 'tr', liveMoments: 'tl' };
  assert.strictEqual(pc.nextAvailableCorner('liveFeed', 'tl', pos), 'tl');
});

test('reset puts the Moments window back at bc and forgets its drag and corner', () => {
  const { pc, storage, add } = loadLive();
  const el = add('liveMoments');
  storage.setItem('panel-corner-liveMoments', 'tr');
  storage.setItem('panel-drag-liveMoments', '{"xPct":0.3,"yPct":0.3}');
  el.setAttribute('data-dragged', 'true');
  pc.resetPanelPositions();
  assert.strictEqual(el.getAttribute('data-position'), 'bc');
  assert.strictEqual(el.getAttribute('data-dragged'), null);
  assert.strictEqual(storage.getItem('panel-corner-liveMoments'), null);
  assert.strictEqual(storage.getItem('panel-drag-liveMoments'), null);
});

test('phone / touch (gate off): the window keeps its own anchor, no data-position', () => {
  const { pc, storage, add } = loadLive({ fine: false });
  const el = add('liveMoments');
  storage.setItem('panel-corner-liveMoments', 'tl');
  pc.applyPanelPosition('liveMoments', 'tl');
  assert.strictEqual(el.getAttribute('data-position'), null);
  // and the other panels are untouched by the gate
  const feed = add('liveFeed');
  pc.applyPanelPosition('liveFeed', 'tl');
  assert.strictEqual(feed.getAttribute('data-position'), 'tl');
});

test('attachMomentsPanel registers once, applies the saved corner and restores a saved drag (desktop)', () => {
  const { pc, storage, add } = loadLive();
  const el = add('liveMoments');
  storage.setItem('panel-corner-liveMoments', 'tr');
  const mgr = fakeMgr();
  const release = pc.attachMomentsPanel(el, mgr, false);
  assert.strictEqual(el.getAttribute('data-position'), 'tr');
  assert.deepStrictEqual(mgr.calls.map((c) => c[0]), ['register', 'restore']);
  assert.strictEqual(mgr.calls[0][1], el);
  assert.strictEqual(mgr.calls[1][1].length, 1);
  assert.strictEqual(mgr.calls[1][1][0], el, 'restores this panel only');
  const btn = el.querySelector('.panel-corner-btn');
  assert.strictEqual((btn.listeners.click || []).length, 1, 'corner button wired');
  assert.strictEqual(typeof release, 'function');
  release();
  assert.deepStrictEqual(mgr.calls.map((c) => c[0]), ['register', 'restore', 'unregister']);
  assert.strictEqual(mgr.calls[2][1], el);
});

test('attachMomentsPanel on a phone / touch device: registered (for a later wide window) but nothing restored', () => {
  const { pc, storage, add } = loadLive({ fine: false });
  const el = add('liveMoments');
  storage.setItem('panel-drag-liveMoments', '{"xPct":0.3,"yPct":0.3}');
  const mgr = fakeMgr();
  pc.attachMomentsPanel(el, mgr, false);
  assert.deepStrictEqual(mgr.calls.map((c) => c[0]), ['register']);
  assert.strictEqual(el.getAttribute('data-position'), null);
  assert.strictEqual(el.getAttribute('data-dragged'), null);
});

test('attachMomentsPanel in kiosk does nothing: no register, no position, no listener', () => {
  const { pc, storage, add } = loadLive();
  const el = add('liveMoments');
  storage.setItem('panel-corner-liveMoments', 'tl');
  storage.setItem('panel-drag-liveMoments', '{"xPct":0.3,"yPct":0.3}');
  const mgr = fakeMgr();
  const release = pc.attachMomentsPanel(el, mgr, true);
  assert.strictEqual(release, null);
  assert.strictEqual(mgr.calls.length, 0);
  assert.strictEqual(el.getAttribute('data-position'), null);
  assert.strictEqual((el.querySelector('.panel-corner-btn').listeners.click || []).length, 0);
});

test('the real DragManager: attach then release leaves no panel and no header listener', () => {
  const { pc, add, ctx } = loadLive();
  const el = add('liveMoments');
  const mgr = new ctx.DragManager();
  const header = el.querySelector('.panel-header');
  const release = pc.attachMomentsPanel(el, mgr, false);
  assert.strictEqual(mgr._panels.length, 1);
  release();
  assert.strictEqual(mgr._panels.length, 0);
  assert.strictEqual(header.listenerCount(), 0);
});

test('a corner button bound twice still moves one step per click', () => {
  const { pc, add } = loadLive();
  const el = add('liveMoments');
  const mgr = fakeMgr();
  pc.attachMomentsPanel(el, mgr, false);
  pc.attachMomentsPanel(el, mgr, false);
  const btn = el.querySelector('.panel-corner-btn');
  const fns = btn.listeners.click || [];
  // the browser drops a second identical listener; the fake keeps both,
  // so assert they are one and the same function
  assert.ok(fns.length >= 1 && fns.every((f) => f === fns[0]), 'one shared handler');
});

console.log('\n' + passed + ' passed, ' + failed + ' failed');
if (failed) process.exit(1);
