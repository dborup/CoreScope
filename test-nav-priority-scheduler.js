#!/usr/bin/env node
/* Priority+ rAF coalescing: no unbounded backlog in a hidden tab.
 *
 * Problem this locks down (measured on staging, image corescope:pr12-aa584358):
 *   rAF callbacks do not run while a tab is hidden. Before the fix, resize and
 *   the ResizeObserver shared a cancel/recreate id, while hashchange and
 *   updateNavStats each queued their OWN unguarded requestAnimationFrame:
 *
 *       window.addEventListener('hashchange', function () {
 *         requestAnimationFrame(applyNavPriority);        // unguarded
 *       });
 *       ...
 *       if (navPriorityFn) requestAnimationFrame(navPriorityFn);  // unguarded
 *
 *   updateNavStats() runs at init, on a 15s setInterval AND on every debounced
 *   WebSocket event, so a backgrounded tab accumulated one queued layout pass
 *   per tick. 74 Priority+ runs were released in a single frame when the pane
 *   became visible. Not an infinite loop - an unbounded backlog of redundant
 *   work, of which only the last run can matter.
 *
 * The fix is one coalescing scheduler that owns the nav rAF id; every async
 * trigger goes through it. This test drives the REAL production scheduler:
 * the nav block is sliced out of public/app.js and evaluated in a vm sandbox
 * with a rAF stub that QUEUES callbacks instead of running them, which is what
 * a hidden tab does. It is not a reimplementation - if app.js changes, this
 * runs the changed code.
 */
'use strict';

const fs = require('fs');
const vm = require('vm');
const assert = require('assert');

let passed = 0, failed = 0;
async function test(name, fn) {
  try { await fn(); console.log('  ✅ ' + name); passed++; }
  catch (e) { console.log('  ❌ ' + name + '\n     ' + e.message); failed++; }
}

const APP = fs.readFileSync('public/app.js', 'utf8');
const a0 = APP.indexOf('// --- Hamburger Menu ---');
const b0 = APP.indexOf("document.addEventListener('keydown'", a0 + 1);
if (a0 < 0 || b0 < 0) throw new Error('could not slice the nav block from public/app.js');
const NAV_SRC = APP.slice(a0, b0);
const CLOSE_SRC = APP.match(/function closeNav\(\)[\s\S]*?\n\}\n\nfunction closeMoreMenu\(\)[\s\S]*?\n\}/);
if (!CLOSE_SRC) throw new Error('could not slice closeNav/closeMoreMenu from public/app.js');

// --------------------------------------------------------------------------
// Minimal DOM shim. Widths are the measured 1440px staging profile, so the
// layout result stays realistic while we exercise scheduling.
// --------------------------------------------------------------------------
function camel(s) { return s.replace(/-([a-z])/g, (_, c) => c.toUpperCase()); }

class ClassList {
  constructor() { this._s = new Set(); }
  add(...c) { c.forEach(x => x && this._s.add(x)); }
  remove(...c) { c.forEach(x => this._s.delete(x)); }
  contains(c) { return this._s.has(c); }
  toggle(c, f) { const w = f === undefined ? !this._s.has(c) : !!f; if (w) this._s.add(c); else this._s.delete(c); return w; }
  get value() { return [...this._s].join(' '); }
}

class El {
  constructor(tag) {
    this.tagName = String(tag).toUpperCase();
    this.classList = new ClassList();
    this.dataset = {}; this.children = []; this.parentElement = null;
    this.attrs = {}; this._on = {}; this.style = {}; this.textContent = '';
    this.w = 0; this._scrollW = null; this.id = '';
  }
  get scrollWidth() { return this._scrollW !== null ? this._scrollW : Math.round(this.w); }
  set scrollWidth(v) { this._scrollW = v; }
  get className() { return this.classList.value; }
  set className(v) { this.classList._s = new Set(String(v).split(/\s+/).filter(Boolean)); }
  appendChild(c) { if (c.parentElement) c.parentElement.removeChild(c); c.parentElement = this; this.children.push(c); return c; }
  removeChild(c) { const i = this.children.indexOf(c); if (i >= 0) this.children.splice(i, 1); c.parentElement = null; return c; }
  remove() { if (this.parentElement) this.parentElement.removeChild(this); }
  insertAdjacentElement(pos, el) {
    const p = this.parentElement, i = p.children.indexOf(this);
    if (el.parentElement) el.parentElement.removeChild(el);
    el.parentElement = p; p.children.splice(pos === 'afterend' ? i + 1 : i, 0, el); return el;
  }
  setAttribute(k, v) {
    this.attrs[k] = String(v);
    if (k === 'class') this.className = v;
    else if (k === 'id') this.id = String(v);
    else if (k.indexOf('data-') === 0) this.dataset[camel(k.slice(5))] = String(v);
  }
  getAttribute(k) { if (k === 'class') return this.className; if (k === 'id') return this.id || null; return this.attrs[k] !== undefined ? this.attrs[k] : null; }
  get innerHTML() { return this._html || ''; }
  set innerHTML(v) { this._html = String(v); if (String(v) === '') { this.children.forEach(c => { c.parentElement = null; }); this.children = []; } }
  cloneNode(deep) {
    const c = new El(this.tagName);
    c.className = this.className; Object.assign(c.dataset, this.dataset); Object.assign(c.attrs, this.attrs);
    c.id = this.id; c.w = this.w; c._scrollW = this._scrollW; c.textContent = this.textContent; c._html = this._html;
    if (deep) this.children.forEach(ch => c.appendChild(ch.cloneNode(true)));
    return c;
  }
  contains(n) { return n === this || this.children.some(c => c.contains(n)); }
  closest(sel) { let n = this; while (n) { if (n.matches(sel)) return n; n = n.parentElement; } return null; }
  matches(sel) {
    const toks = String(sel).match(/(\[[^\]]+\]|[.#]?[\w-]+)/g) || [];
    return toks.every(t => {
      if (t[0] === '.') return this.classList.contains(t.slice(1));
      if (t[0] === '#') return this.id === t.slice(1);
      if (t[0] === '[') {
        const m = t.match(/^\[([^\]=]+)(?:="?([^\]"]*)"?)?\]$/);
        if (!m) return false;
        const have = this.getAttribute(m[1]);
        return m[2] === undefined ? have !== null : have === m[2];
      }
      return this.tagName === t.toUpperCase();
    });
  }
  _desc(out) { this.children.forEach(c => { out.push(c); c._desc(out); }); return out; }
  querySelectorAll(sel) { return this._desc([]).filter(e => e.matches(sel)); }
  querySelector(sel) { return this.querySelectorAll(sel)[0] || null; }
  addEventListener(t, f) { (this._on[t] = this._on[t] || []).push(f); }
  focus() {}
  getBoundingClientRect() {
    const w = this.classList.contains('is-hidden') ? 0 : this.w;
    return { width: w, height: 0, top: 0, left: 0, right: w, bottom: 0 };
  }
}

// Measured 1440px staging profile.
const P = {
  topNavClient: 1434, padL: 28.8, padR: 28.8, topGap: 16,
  leftGap: 22.8, linkGap: 4.16, brand: 125, moreW: 68.8,
  rightEmpty: 215, rightFull: 470,
  w: { home:62.14, packets:74.51, map:52.27, live:66.63, channels:83.45, nodes:65.59,
       tools:58.42, observers:89.88, analytics:82.79, perf:67.47, 'audio-lab':64.27,
       privacy:87.23, 'rx-coverage':92.00 },
};
// How updateNavStats() invokes navPriorityFn in the tree under test.
// aa584358 and earlier: `requestAnimationFrame(navPriorityFn)` (unguarded).
// After the scheduler fix: `navPriorityFn()` (coalesced).
const STATS_WRAPS_IN_RAF = /requestAnimationFrame\(navPriorityFn\)/.test(APP);

const ORDER = ['home','packets','map','live','channels','nodes','tools','observers','analytics','perf','audio-lab'];
const HIGH  = ['home','packets','map','live','nodes'];

function mkLink(route) {
  const a = new El('a');
  a.className = 'nav-link';
  a.setAttribute('data-route', route);
  if (HIGH.indexOf(route) >= 0) a.setAttribute('data-priority', 'high');
  a.setAttribute('href', '#/' + route);
  a.textContent = route;
  a.w = P.w[route] !== undefined ? P.w[route] : 70;
  return a;
}

/**
 * rAF is QUEUED, never auto-run — exactly how a hidden tab behaves.
 * flushFrame() plays the queue that existed when the frame started, which is
 * what the browser does: callbacks added during a frame run in the NEXT one.
 */
function boot(opts) {
  opts = opts || {};
  const byId = {};
  const rafQueue = [];
  let nextRafId = 1;
  let applyRuns = 0;

  const doc = { createElement: t => new El(t), getElementById: id => byId[id] || null };
  const mk = (tag, cls, id) => {
    const e = new El(tag);
    if (cls) e.className = cls;
    if (id) { e.id = id; e.setAttribute('id', id); byId[id] = e; }
    return e;
  };

  const topNav = mk('nav', 'top-nav');
  const navLeft = mk('div', 'nav-left');
  const brand = mk('a', 'nav-brand'); brand.w = P.brand;
  const linksEl = mk('div', 'nav-links');
  const moreWrap = mk('div', 'nav-more-wrap'); moreWrap.w = P.moreW;
  const moreBtn = mk('button', 'nav-btn nav-more-btn', 'navMoreBtn');
  const moreMenu = mk('div', 'nav-more-menu', 'navMoreMenu');
  const navRight = mk('div', 'nav-right');
  const navStats = mk('div', 'nav-stats', 'navStats');
  const hamburger = mk('button', 'nav-btn hamburger', 'hamburger');
  const body = mk('body', '');

  navRight.scrollWidth = P.rightEmpty;
  topNav.clientWidth = P.topNavClient;

  topNav.appendChild(navLeft);
  navLeft.appendChild(brand); navLeft.appendChild(linksEl); navLeft.appendChild(moreWrap);
  moreWrap.appendChild(moreBtn); moreWrap.appendChild(moreMenu);
  topNav.appendChild(navRight); navRight.appendChild(navStats); navRight.appendChild(hamburger);

  const root = new El('html');
  root.appendChild(topNav); root.appendChild(body);
  ORDER.forEach(r => linksEl.appendChild(mkLink(r)));
  linksEl.appendChild(mkLink('privacy'));
  linksEl.querySelector('[data-route="home"]').classList.add('active');

  function inlineLinks() {
    return linksEl.children.filter(e => e.classList.contains('nav-link') &&
                                        !e.classList.contains('is-overflow'));
  }
  function naturalScrollW() {
    const inl = inlineLinks();
    let w = 0; inl.forEach(a => { w += Math.max(a.getBoundingClientRect().width, a.scrollWidth); });
    return w + Math.max(0, inl.length - 1) * P.linkGap;
  }
  function maxGrantedW() {
    const moreW = moreWrap.classList.contains('is-hidden') ? 0 : P.moreW;
    return (P.topNavClient - P.padL - P.padR) - P.topGap - navRight.scrollWidth
           - P.brand - 2 * P.leftGap - moreW;
  }
  Object.defineProperty(linksEl, 'scrollWidth', { get: () => Math.round(naturalScrollW()) });
  Object.defineProperty(linksEl, 'clientWidth', {
    get: () => Math.round(Math.max(0, Math.min(naturalScrollW(), maxGrantedW()))) });

  doc.querySelector = sel => (root.matches(sel) ? root : root.querySelector(sel));
  doc.querySelectorAll = sel => root.querySelectorAll(sel);
  doc.body = body;
  doc.addEventListener = () => {};

  // fonts.ready as a controllable promise, so the fonts trigger is testable.
  let resolveFonts;
  const fontsReady = new Promise(res => { resolveFonts = res; });
  doc.fonts = opts.noFonts ? undefined : { ready: fontsReady, status: 'loading' };

  const styleFor = el => {
    if (el === linksEl) return { columnGap: P.linkGap + 'px', gap: P.linkGap + 'px', paddingLeft: '0px', paddingRight: '0px' };
    if (el === topNav)  return { columnGap: P.topGap + 'px', gap: P.topGap + 'px', paddingLeft: P.padL + 'px', paddingRight: P.padR + 'px' };
    return { columnGap: P.leftGap + 'px', gap: P.leftGap + 'px', paddingLeft: '0px', paddingRight: '0px' };
  };

  const win = {
    innerWidth: opts.viewport || 1440, _on: {},
    addEventListener(t, f) { (this._on[t] = this._on[t] || []).push(f); },
    // Hidden-tab semantics: queue, never run.
    requestAnimationFrame(fn) { const id = nextRafId++; rafQueue.push({ id, fn }); return id; },
    cancelAnimationFrame(id) { const i = rafQueue.findIndex(e => e.id === id); if (i >= 0) rafQueue.splice(i, 1); },
    getComputedStyle: styleFor,
  };

  const observers = [];
  function ResizeObserverShim(cb) { this.observe = el => observers.push({ el, cb }); this.disconnect = () => {}; }

  const ctx = {
    window: win, document: doc, console,
    requestAnimationFrame: win.requestAnimationFrame,
    cancelAnimationFrame: win.cancelAnimationFrame,
    getComputedStyle: styleFor,
    ResizeObserver: ResizeObserverShim,
    Promise, Array, Object, String, Number, Math, Set, parseFloat, JSON, Boolean,
  };
  ctx.globalThis = ctx;
  vm.createContext(ctx);
  vm.runInContext(CLOSE_SRC[0], ctx, { filename: 'app.js#close' });

  // Count real applyNavPriority executions by counting the More-menu rebuild
  // each pass performs (navMoreMenu.innerHTML = '' happens once per run).
  let counting = false;
  const htmlDesc = Object.getOwnPropertyDescriptor(El.prototype, 'innerHTML');
  Object.defineProperty(moreMenu, 'innerHTML', {
    configurable: true,
    get() { return htmlDesc.get.call(this); },
    set(v) { if (counting && v === '') applyRuns++; htmlDesc.set.call(this, v); },
  });

  // `navPriorityFn` is a top-level `let` inside the slice, and vm does not
  // surface those as context properties. Export the real binding so the
  // updateNavStats callsite can be driven exactly as app.js drives it.
  vm.runInContext(NAV_SRC + '\n;globalThis.__navPriorityFn = navPriorityFn;',
                  ctx, { filename: 'app.js#nav' });
  counting = true;              // ignore the synchronous init pass

  const fire = t => (win._on[t] || []).forEach(f => f({ type: t }));

  return {
    linksEl, moreMenu, moreWrap, navRight, navStats, topNav,
    /** How many rAF callbacks are queued but not yet run. */
    pending: () => rafQueue.length,
    /** Real applyNavPriority executions since init. */
    runs: () => applyRuns,
    resetRuns: () => { applyRuns = 0; },
    resize: () => fire('resize'),
    hashchange: () => fire('hashchange'),
    /** What updateNavStats does: fill #navStats, then poke navPriorityFn. */
    statsTick(width) {
      // Reproduce updateNavStats() EXACTLY as the current source writes it:
      // fill #navStats, then invoke navPriorityFn the way app.js does. The
      // pre-fix source wraps it in its own requestAnimationFrame; the fixed
      // source calls the scheduler directly. Reading the shape from the source
      // keeps this faithful to whichever tree is under test instead of
      // hard-coding one call style.
      navStats.innerHTML = 'x';
      if (width !== undefined) navRight.scrollWidth = width;
      const f = ctx.__navPriorityFn;
      if (typeof f !== 'function') throw new Error('navPriorityFn is not a function: ' + typeof f);
      if (STATS_WRAPS_IN_RAF) win.requestAnimationFrame(f);
      else f();
      return f;
    },
    observerTick(width) {
      navRight.scrollWidth = width;
      observers.filter(o => o.el === navRight).forEach(o => o.cb([{ target: navRight }]));
    },
    fontsReady() { doc.fonts.status = 'loaded'; resolveFonts(); return fontsReady; },
    /** Run exactly the callbacks queued when the frame started. */
    flushFrame() {
      const batch = rafQueue.splice(0, rafQueue.length);
      batch.forEach(e => e.fn());
      return batch.length;
    },
    ctx,
    inline: () => inlineLinks().map(a => a.dataset.route),
    overflowed: () => linksEl.children.filter(e => e.classList.contains('nav-link') && e.classList.contains('is-overflow')).map(a => a.dataset.route),
    moreRoutes: () => moreMenu.children.map(c => c.dataset.route),
    contained: () => linksEl.scrollWidth <= linksEl.clientWidth + 1,
  };
}

// --------------------------------------------------------------------------
(async function main() {
  console.log('test-nav-priority-scheduler.js');

  await test('a single resize queues exactly one callback', async () => {
    const n = boot();
    assert.strictEqual(n.pending(), 0, 'nothing pending before the event');
    n.resize();
    assert.strictEqual(n.pending(), 1, 'expected 1 pending, got ' + n.pending());
  });

  await test('100 resizes still queue exactly one callback', async () => {
    const n = boot();
    for (let i = 0; i < 100; i++) n.resize();
    assert.strictEqual(n.pending(), 1, 'BACKLOG: ' + n.pending() + ' pending callbacks');
  });

  await test('100 hashchanges still queue exactly one callback', async () => {
    const n = boot();
    for (let i = 0; i < 100; i++) n.hashchange();
    assert.strictEqual(n.pending(), 1, 'BACKLOG: ' + n.pending() + ' pending callbacks');
  });

  await test('100 stats updates still queue exactly one callback', async () => {
    const n = boot();
    for (let i = 0; i < 100; i++) n.statsTick();
    assert.strictEqual(n.pending(), 1, 'BACKLOG: ' + n.pending() + ' pending callbacks');
  });

  await test('mixed resize/hash/stats/observer/fonts still queue exactly one', async () => {
    const n = boot();
    await n.fontsReady();
    for (let i = 0; i < 40; i++) {
      n.resize();
      n.hashchange();
      n.statsTick();
      n.observerTick(P.rightEmpty + (i % 7));   // varying width so the guard passes
    }
    assert.strictEqual(n.pending(), 1, 'BACKLOG: ' + n.pending() + ' pending callbacks');
  });

  await test('24 hidden hours of 15s stats ticks queue exactly one callback', async () => {
    const n = boot();
    const TICKS = 24 * 60 * 60 / 15;            // 5760
    for (let i = 0; i < TICKS; i++) n.statsTick();
    assert.strictEqual(n.pending(), 1,
      'BACKLOG after ' + TICKS + ' ticks: ' + n.pending() + ' pending callbacks');
  });

  await test('the first visible frame performs exactly one layout pass', async () => {
    const n = boot();
    for (let i = 0; i < 5760; i++) n.statsTick();
    n.resetRuns();
    const ran = n.flushFrame();
    assert.strictEqual(ran, 1, 'expected 1 rAF callback in the frame, got ' + ran);
    assert.strictEqual(n.runs(), 1, 'expected 1 applyNavPriority, got ' + n.runs());
  });

  await test('the pending id is cleared after the frame', async () => {
    const n = boot();
    n.resize();
    assert.strictEqual(n.pending(), 1);
    n.flushFrame();
    assert.strictEqual(n.pending(), 0, 'queue should be empty after the frame');
  });

  await test('a new event after the frame schedules a fresh single callback', async () => {
    const n = boot();
    n.resize();
    n.flushFrame();
    assert.strictEqual(n.pending(), 0);
    n.resize();
    assert.strictEqual(n.pending(), 1, 'scheduler must not be stuck after flushing');
    n.resetRuns();
    n.flushFrame();
    assert.strictEqual(n.runs(), 1, 'the follow-up frame must still do the work');
  });

  await test('no callback is permanently lost after the first frame', async () => {
    const n = boot();
    for (let round = 0; round < 25; round++) {
      n.statsTick();
      assert.strictEqual(n.pending(), 1, 'round ' + round + ': expected 1 pending');
      n.resetRuns();
      n.flushFrame();
      assert.strictEqual(n.runs(), 1, 'round ' + round + ': expected 1 run');
    }
  });

  await test('events fired DURING a layout pass schedule at most one more frame', async () => {
    const n = boot();
    // Re-entrancy: the More-menu rebuild fires events mid-pass.
    let fired = 0;
    const cur = Object.getOwnPropertyDescriptor(n.moreMenu, 'innerHTML');
    Object.defineProperty(n.moreMenu, 'innerHTML', {
      configurable: true,
      get() { return cur.get.call(this); },
      set(v) {
        cur.set.call(this, v);          // keeps the run counter working
        if (v === '' && fired < 10) { fired++; n.resize(); n.hashchange(); n.statsTick(); }
      },
    });
    n.resize();
    assert.strictEqual(n.pending(), 1);
    n.flushFrame();
    assert(fired > 0, 'the re-entrancy hook should have fired');
    assert.strictEqual(n.pending(), 1,
      'events during the pass must coalesce into exactly one follow-up, got ' + n.pending());
  });

  await test('the layout result is unchanged by the scheduler', async () => {
    const n = boot();
    n.observerTick(P.rightFull);      // stats land
    n.flushFrame();
    assert(n.contained(), 'strip must be contained: ' +
      n.linksEl.scrollWidth + ' vs ' + n.linksEl.clientWidth);
    assert.deepStrictEqual(n.inline(),
      ['home', 'packets', 'map', 'live', 'channels', 'nodes', 'tools', 'observers'],
      'unexpected inline set: ' + JSON.stringify(n.inline()));
    const HI = n.overflowed().filter(r => HIGH.includes(r));
    assert.strictEqual(HI.length, 0, 'high-priority overflowed: ' + HI);
    assert(n.inline().includes('home'), 'active route must stay inline');
    const m = n.moreRoutes();
    assert.strictEqual(m.length, new Set(m).size, 'duplicate clones: ' + JSON.stringify(m));
    assert.strictEqual(m.filter(r => r === 'privacy').length, 1, 'privacy exactly once in More');
  });

  await test('the ResizeObserver width guard is preserved', async () => {
    const n = boot();
    n.observerTick(P.rightFull);
    n.flushFrame();
    assert.strictEqual(n.pending(), 0);
    n.observerTick(P.rightFull);      // SAME width
    assert.strictEqual(n.pending(), 0, 'an unchanged width must not schedule anything');
    n.observerTick(P.rightFull + 5);  // changed
    assert.strictEqual(n.pending(), 1, 'a changed width must schedule exactly one');
  });

  await test('three further frames with no events do no extra work', async () => {
    const n = boot();
    for (let i = 0; i < 5760; i++) n.statsTick();
    n.flushFrame();
    n.resetRuns();
    for (let i = 0; i < 3; i++) {
      const ran = n.flushFrame();
      assert.strictEqual(ran, 0, 'frame ' + i + ' should have nothing queued, ran ' + ran);
    }
    assert.strictEqual(n.runs(), 0, 'no further layout passes without new events');
  });

  await test('static: no direct rAF on the layout pass remains in app.js', async () => {
    assert.strictEqual((APP.match(/requestAnimationFrame\(applyNavPriority\)/g) || []).length, 0,
      'a direct requestAnimationFrame(applyNavPriority) is back');
    assert.strictEqual((APP.match(/requestAnimationFrame\(navPriorityFn\)/g) || []).length, 0,
      'a direct requestAnimationFrame(navPriorityFn) is back');
    assert.ok(/function scheduleNavPriority\(\)/.test(APP), 'scheduleNavPriority must exist');
    assert.ok(/navPriorityFn = scheduleNavPriority/.test(APP),
      'navPriorityFn must point at the scheduler, not the raw layout pass');
    // exactly one owner of the pending slot, and it is claimed BEFORE the
    // rAF call so a synchronous rAF cannot wedge the scheduler shut.
    const owners = (APP.match(/navPriorityPending = true;\s*\n\s*requestAnimationFrame\(/g) || []).length;
    assert.strictEqual(owners, 1, 'exactly one scheduler may own the pending slot, found ' + owners);
    assert.ok(/navPriorityPending = false;\s*\n\s*applyNavPriority\(\);/.test(APP),
      'the pending flag must be cleared immediately before the layout pass');
  });

  console.log('\n  ' + passed + ' passed, ' + failed + ' failed');
  process.exit(failed ? 1 : 0);
})();
