/* tests/fake-dom.js — a small fake DOM, fake clock and vm loader for the
 * unit tests of the Live controllers (live-kiosk.js, live-moments.js), which
 * take every dependency through create(deps). Used by test-883-m3-kiosk.js.
 */
'use strict';
const fs = require('fs');
const path = require('path');
const vm = require('vm');

class Target {
  constructor() { this.listeners = {}; }
  addEventListener(t, fn) { (this.listeners[t] = this.listeners[t] || []).push(fn); }
  removeEventListener(t, fn) { this.listeners[t] = (this.listeners[t] || []).filter((f) => f !== fn); }
  count(t) {
    if (t) return (this.listeners[t] || []).length;
    return Object.values(this.listeners).reduce((n, a) => n + a.length, 0);
  }
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
    this.attrs = {}; this.className = ''; this.hidden = false; this.style = {}; this._text = '';
    this.type = ''; this.href = '';
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
  get firstChild() { return this.children[0] || null; }
  appendChild(c) { if (c.parentNode) c.parentNode.removeChild(c); c.parentNode = this; this.children.push(c); return c; }
  removeChild(c) { const i = this.children.indexOf(c); if (i !== -1) this.children.splice(i, 1); c.parentNode = null; return c; }
  contains(o) { for (let n = o; n; n = n.parentNode) if (n === this) return true; return false; }
  setAttribute(k, v) { this.attrs[k] = String(v); }
  getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; }
  focus() {}
  click() { this.fire('click', { target: this }); }
  all() { return [this].concat(...this.children.map((c) => c.all())); }
  find(cls) { return this.all().filter((e) => e.classList.contains(cls)); }
}

function makeDoc() {
  const doc = new El('document');
  doc.body = new El('body');
  doc.createElement = (t) => new El(t);
  doc.createElementNS = (_ns, t) => new El(t);
  doc.hidden = false;
  doc.fullscreenElement = null;
  doc.fullscreenEnabled = true;
  return doc;
}

// A wall clock and its timers. advance() fires due timers in time order;
// jump() moves the wall clock without firing anything, as a machine that
// slept (its timers did not run) or a throttled tab.
function makeClock() {
  let now = Date.UTC(2026, 10, 22, 12, 0, 0), seq = 0;
  const pending = new Map();
  return {
    now: () => now,
    setTimeout: (fn, ms) => { const id = ++seq; pending.set(id, { at: now + Math.max(0, ms || 0), fn }); return id; },
    clearTimeout: (id) => { pending.delete(id); },
    pending: () => pending.size,
    jump(ms) {
      now += ms;
      // A timer that was due during the jump fires on the next advance, late.
    },
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

// Loads public/<file> in a fresh vm context; returns its window.
function loadPublic(file, extra) {
  const ctx = Object.assign({ window: {}, console, Date, Math, Number, String, JSON, Array, Object, Set, Map, Promise,
    isFinite, isNaN, parseInt, encodeURIComponent, URLSearchParams, AbortController }, extra || {});
  vm.createContext(ctx);
  const f = path.join('public', file);
  vm.runInContext(fs.readFileSync(path.join(__dirname, '..', f), 'utf8'), ctx, { filename: f });
  return ctx.window;
}

const flush = () => new Promise((r) => setImmediate(r));

module.exports = { Target, El, makeDoc, makeClock, loadPublic, flush };
