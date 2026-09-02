#!/usr/bin/env node
/* Priority+ first-load overlap at 1101-1200px.
 *
 * Root cause (measured live on staging, image corescope:pr12-98121595, 1200px):
 *   #navStats is filled from /api/stats, which lands AFTER DOMContentLoaded:
 *       /api/stats responseEnd      2106ms
 *       domContentLoadedEventEnd    2046ms
 *   applyNavPriority() runs inside the DOMContentLoaded handler, so it measured
 *   navRightEl.scrollWidth at 212px instead of its final 467px - a 255px
 *   under-reservation. fits() therefore returned true with the strip already
 *   too wide, the greedy loop stopped early, and links stayed inline
 *   underneath "More". Nothing re-measured until the next resize, which is why
 *   one resize "fixed" it and why <=1100px was clean (the narrow-desktop
 *   branch force-collapses without measuring).
 *
 *   The links themselves were NOT compressed - measured per link at 1200px:
 *       home       rect 58.35  offset 58  client 58  scroll 58
 *       observers  rect 85.46  offset 85  client 85  scroll 85
 *       live (SVG) rect 63.16  offset 63  client 63  scroll 63
 *   white-space:nowrap, border 0, padding 11.2px a side, box-sizing:border-box.
 *   So scrollWidth includes text, padding and inline SVG, and equals the rect.
 *
 * Fixes locked down here:
 *   1. A ResizeObserver on .nav-right re-runs the fit when the right-hand side
 *      finishes growing (safe: .nav-right is flex-grow:0/flex-shrink:0, so
 *      collapsing links on the left cannot change it - verified live).
 *   2. fits() sums Math.max(rect.width, scrollWidth) per visible link, so a
 *      compressed rect can never under-report an intrinsic width.
 *
 * Widths below are the REAL measured values, so the model reproduces the
 * browser: at 1200px with nav-right 212 the engine stops at 10 inline, and at
 * nav-right 467 it settles on exactly the 6 inline links observed live.
 *
 * Runs the REAL production source (sliced from public/app.js) in a vm sandbox.
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
    this.w = 0;          // intrinsic width  -> scrollWidth
    this.rectW = null;   // rendered width   -> getBoundingClientRect (null = same as w)
    this._scrollW = null;
    this.id = '';
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
    c.id = this.id; c.w = this.w; c.rectW = this.rectW; c._scrollW = this._scrollW;
    c.textContent = this.textContent; c._html = this._html;
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
    const w = this.classList.contains('is-hidden') ? 0 : (this.rectW !== null ? this.rectW : this.w);
    return { width: w, height: 0, top: 0, left: 0, right: w, bottom: 0 };
  }
}

// Measured per-viewport profiles (staging, image corescope:pr12-0d504756).
// Everything here came out of the browser on a fresh 1440/1200/1101/2560 load:
// link widths, both gap values, .top-nav's own content box and gap, the More
// button width and .nav-right with #navStats empty vs populated.
const PROFILES = {
  1101: { topNavClient: 1095, padL: 22.02, padR: 22.02, topGap: 16,
    leftGap: 20.2575, linkGap: 3.6515, brand: 125, moreW: 70.73,
    rightEmpty: 211, rightFull: 466, w: {
    home:56.79, packets:68.78, map:47.27, live:61.73, channels:77.41, nodes:60.14,
    tools:53.25, observers:83.63, analytics:76.82, perf:62.54, 'audio-lab':59.42,
    privacy:81.63, 'rx-coverage':86.00 } },
  1200: { topNavClient: 1194, padL: 24, padR: 24, topGap: 16,
    leftGap: 21, linkGap: 3.8, brand: 125, moreW: 70.73,
    rightEmpty: 212, rightFull: 467, w: {
    home:58.35, packets:70.46, map:48.73, live:63.16, channels:79.18, nodes:61.73,
    tools:54.77, observers:85.46, analytics:78.57, perf:63.98, 'audio-lab':60.84,
    privacy:83.27, 'rx-coverage':88.00 } },
  1440: { topNavClient: 1434, padL: 28.8, padR: 28.8, topGap: 16,
    leftGap: 22.8, linkGap: 4.16, brand: 125, moreW: 68.8,
    rightEmpty: 215, rightFull: 470, w: {
    home:62.14, packets:74.51, map:52.27, live:66.63, channels:83.45, nodes:65.59,
    tools:58.42, observers:89.88, analytics:82.79, perf:67.47, 'audio-lab':64.27,
    privacy:87.23, 'rx-coverage':92.00 } },
  2560: { topNavClient: 2554, padL: 32, padR: 32, topGap: 16,
    leftGap: 31.2, linkGap: 5.84, brand: 125, moreW: 68.8,
    rightEmpty: 229, rightFull: 484, w: {
    home:66.99, packets:79.98, map:56.55, live:70.75, channels:89.43, nodes:70.60,
    tools:62.98, observers:96.22, analytics:88.68, perf:71.65, 'audio-lab':68.30,
    privacy:92.49, 'rx-coverage':97.00 } },
};
// <=1100px never measures (the narrow-desktop branch force-collapses), so the
// nearest measured profile is representative there.
function profileFor(vw) {
  if (PROFILES[vw]) return PROFILES[vw];
  return vw >= 2000 ? PROFILES[2560] : vw >= 1300 ? PROFILES[1440]
       : vw >= 1150 ? PROFILES[1200] : PROFILES[1101];
}

const ORDER = ['home','packets','map','live','channels','nodes','tools','observers','analytics','perf','audio-lab'];
const HIGH = ['home','packets','map','live','nodes'];
const LABEL = { home:'Home', packets:'Packets', map:'Map', live:'Live', channels:'Channels',
  nodes:'Nodes', tools:'Tools', observers:'Observers', analytics:'Analytics', perf:'Perf',
  'audio-lab':'Lab', privacy:'Privacy', 'rx-coverage':'Coverage' };

const PRIVACY = { route: 'privacy' };
const RXCOV   = { route: 'rx-coverage' };

function mkLink(route, prof, opts) {
  const a = new El('a');
  a.className = 'nav-link';
  a.setAttribute('data-route', route);
  if (HIGH.indexOf(route) >= 0) a.setAttribute('data-priority', 'high');
  a.setAttribute('href', '#/' + route);
  a.textContent = LABEL[route] || route;
  a.w = prof.w[route] !== undefined ? prof.w[route] : 70;
  if (opts && opts.squeeze) a.rectW = a.w * 0.55;   // simulate a compressed flex line
  return a;
}

function boot(opts) {
  opts = opts || {};
  const viewport = opts.viewport || 1200;
  const prof = profileFor(viewport);
  const byId = {};
  const observers = [];

  const doc = { createElement: t => new El(t), getElementById: id => byId[id] || null };
  const mk = (tag, cls, id) => {
    const e = new El(tag);
    if (cls) e.className = cls;
    if (id) { e.id = id; e.setAttribute('id', id); byId[id] = e; }
    return e;
  };

  const topNav = mk('nav', 'top-nav');
  const navLeft = mk('div', 'nav-left');
  const brand = mk('a', 'nav-brand'); brand.w = prof.brand;
  const linksEl = mk('div', 'nav-links');
  const moreWrap = mk('div', 'nav-more-wrap'); moreWrap.w = prof.moreW;
  const moreBtn = mk('button', 'nav-btn nav-more-btn', 'navMoreBtn');
  const moreMenu = mk('div', 'nav-more-menu', 'navMoreMenu');
  const navRight = mk('div', 'nav-right');
  const navStats = mk('div', 'nav-stats', 'navStats');
  const hamburger = mk('button', 'nav-btn hamburger', 'hamburger');
  const body = mk('body', '');

  navRight.scrollWidth = opts.rightW !== undefined ? opts.rightW : prof.rightEmpty;
  topNav.clientWidth = prof.topNavClient;

  topNav.appendChild(navLeft);
  navLeft.appendChild(brand); navLeft.appendChild(linksEl); navLeft.appendChild(moreWrap);
  moreWrap.appendChild(moreBtn); moreWrap.appendChild(moreMenu);
  topNav.appendChild(navRight); navRight.appendChild(navStats); navRight.appendChild(hamburger);

  const root = new El('html');
  root.appendChild(topNav); root.appendChild(body);

  ORDER.forEach(r => linksEl.appendChild(mkLink(r, prof, opts)));
  (opts.preInject || []).forEach(spec => linksEl.appendChild(mkLink(spec.route, prof, opts)));
  if (opts.activeRoute) {
    const a = linksEl.querySelector('[data-route="' + opts.activeRoute + '"]');
    if (a) a.classList.add('active');
  }

  // --- live flex model for .nav-links -------------------------------------
  // natural content width = intrinsic link widths + inter-link gaps.
  // The flex line grants at most what is left of .top-nav's content box after
  // .nav-right (flex-shrink:0), the brand, the More button and the two
  // .nav-left gaps; below that it just wraps its content. Verified against the
  // browser: 1440 -> client 651 / scroll 669, 1200 -> 401/401, 1101 -> 309/309,
  // 2560 -> 979/979.
  function inlineLinks() {
    return linksEl.children.filter(e => e.classList.contains('nav-link') &&
                                        !e.classList.contains('is-overflow'));
  }
  function naturalScrollW() {
    const inl = inlineLinks();
    let w = 0; inl.forEach(a => { w += Math.max(a.getBoundingClientRect().width, a.scrollWidth); });
    return w + Math.max(0, inl.length - 1) * prof.linkGap;
  }
  function maxGrantedW() {
    const moreW = moreWrap.classList.contains('is-hidden') ? 0 : prof.moreW;
    return (prof.topNavClient - prof.padL - prof.padR) - prof.topGap - navRight.scrollWidth
           - prof.brand - 2 * prof.leftGap - moreW;
  }
  Object.defineProperty(linksEl, 'scrollWidth', { get: () => Math.round(naturalScrollW()) });
  Object.defineProperty(linksEl, 'clientWidth', {
    get: () => Math.round(Math.max(0, Math.min(naturalScrollW(), maxGrantedW()))) });

  doc.querySelector = sel => (root.matches(sel) ? root : root.querySelector(sel));
  doc.querySelectorAll = sel => root.querySelectorAll(sel);
  doc.body = body;
  doc.fonts = undefined;
  doc.addEventListener = () => {};

  const styleFor = el => {
    if (el === linksEl) return { columnGap: prof.linkGap + 'px', gap: prof.linkGap + 'px',
                                 paddingLeft: '0px', paddingRight: '0px' };
    if (el === topNav)  return { columnGap: prof.topGap + 'px', gap: prof.topGap + 'px',
                                 paddingLeft: prof.padL + 'px', paddingRight: prof.padR + 'px' };
    return { columnGap: prof.leftGap + 'px', gap: prof.leftGap + 'px',
             paddingLeft: '0px', paddingRight: '0px' };
  };

  const win = {
    innerWidth: viewport, _on: {},
    addEventListener(t, f) { (this._on[t] = this._on[t] || []).push(f); },
    requestAnimationFrame(fn) { fn(); return 1; },
    cancelAnimationFrame() {},
    getComputedStyle: styleFor,
  };

  function ResizeObserverShim(cb) {
    this.observe = el => { observers.push({ el, cb }); };
    this.disconnect = () => {};
  }

  const ctx = {
    window: win, document: doc, console,
    requestAnimationFrame: win.requestAnimationFrame,
    cancelAnimationFrame: win.cancelAnimationFrame,
    getComputedStyle: styleFor,
    ResizeObserver: opts.noResizeObserver ? undefined : ResizeObserverShim,
    Array, Object, String, Number, Math, Set, parseFloat, JSON, Boolean,
  };
  ctx.globalThis = ctx;
  vm.createContext(ctx);
  vm.runInContext(CLOSE_SRC[0], ctx, { filename: 'app.js#close' });
  vm.runInContext(NAV_SRC, ctx, { filename: 'app.js#nav' });

  const fire = t => (win._on[t] || []).forEach(f => f({ type: t }));

  const api = {
    prof, doc, win, linksEl, moreMenu, moreWrap, moreBtn, navRight, navStats, hamburger, body, topNav,
    statsArrive(w) {
      navRight.scrollWidth = w === undefined ? prof.rightFull : w;
      observers.filter(o => o.el === navRight).forEach(o => o.cb([{ target: navRight }]));
    },
    addLink(spec, o2) {
      const a = mkLink(spec.route, prof, opts);
      if (o2 && o2.after) linksEl.querySelector('[data-route="' + o2.after + '"]').insertAdjacentElement('afterend', a);
      else linksEl.appendChild(a);
      if (spec.active) a.classList.add('active');
      fire('resize');
      return a;
    },
    resize(w) { if (w) win.innerWidth = w; fire('resize'); },
    hashchange() { fire('hashchange'); },
    all: () => linksEl.children.filter(e => e.classList.contains('nav-link')),
    inline: () => api.all().filter(e => !e.classList.contains('is-overflow')).map(e => e.dataset.route),
    overflowed: () => api.all().filter(e => e.classList.contains('is-overflow')).map(e => e.dataset.route),
    moreRoutes: () => moreMenu.children.map(e => e.dataset.route),
    link: r => linksEl.querySelector('[data-route="' + r + '"]'),
    observerCount: () => observers.length,
    activeRoute: () => { const a = api.all().find(x => x.classList.contains('active')); return a ? a.dataset.route : null; },
    /** THE acceptance contract: the strip must fit the box flex granted it. */
    containment() {
      return { clientWidth: linksEl.clientWidth, scrollWidth: linksEl.scrollWidth,
               overrun: linksEl.scrollWidth - linksEl.clientWidth,
               ok: linksEl.scrollWidth <= linksEl.clientWidth + 1 };
    },
    /** Budget from the real container, mirroring the shipped fits(). */
    stripFits() {
      const inl = inlineLinks();
      let lw = 0; inl.forEach(a => { lw += Math.max(a.getBoundingClientRect().width, a.scrollWidth); });
      const moreW = moreWrap.classList.contains('is-hidden') ? 0 : prof.moreW;
      const avail = prof.topNavClient - prof.padL - prof.padR;
      const needed = prof.brand + prof.leftGap + lw + Math.max(0, inl.length - 1) * prof.linkGap +
                     prof.leftGap + moreW + prof.topGap + navRight.scrollWidth;
      return { needed: +needed.toFixed(2), avail: +avail.toFixed(2),
               fits: needed <= avail - 1, deficit: +(needed - avail).toFixed(2) };
    },
    /** No visible link may overlap More or nav-right. */
    noOverlap() {
      const granted = maxGrantedW();
      const contentEnd = naturalScrollW();
      return { contentEnd: +contentEnd.toFixed(2), granted: +granted.toFixed(2),
               ok: contentEnd <= granted + 1 };
    },
  };
  return api;
}

// --------------------------------------------------------------------------
function assertContract(n, label) {
  const c = n.containment();
  const o = n.noOverlap();
  // Containment is required WHENEVER the engine still has a droppable link.
  // The one documented exception is the pre-existing #1311/#1391 floor: when
  // every remaining inline link is high-priority or the active pill, the
  // engine is contractually forbidden from dropping any of them, and an
  // over-full strip is the accepted outcome (see the #1311 comment in
  // app.js). Measured at 1101px with a non-high active route: 6 pinned links
  // need 396px in a 333px box on BOTH 0d504756 and this commit - unchanged
  // by the budget fix, and not something it can address.
  const droppableLeft = n.inline().filter(r =>
    !HIGH.includes(r) && r !== n.activeRoute()).length;
  if (droppableLeft > 0) {
    assert(c.ok, label + ': .nav-links overruns its box by ' + c.overrun +
      'px (client ' + c.clientWidth + ', scroll ' + c.scrollWidth + ') while ' +
      droppableLeft + ' droppable link(s) were still inline');
    assert(o.ok, label + ': content ends at ' + o.contentEnd + ' but only ' + o.granted + ' was granted');
  }
  const HI = n.overflowed().filter(r => HIGH.includes(r));
  assert.strictEqual(HI.length, 0, label + ': high-priority overflowed: ' + HI);
  const m = n.moreRoutes();
  assert.strictEqual(m.length, new Set(m).size, label + ': duplicate clones ' + JSON.stringify(m));
  const live = n.all().map(a => a.dataset.route);
  assert.deepStrictEqual(m.filter(r => !live.includes(r)), [], label + ': stale clones');
  assert(m.length === 0 || m.length >= 2, label + ': degenerate 1-item More menu');
}

(async function main() {
  console.log('test-nav-first-load-fit.js');

  // ---- the 1440 budget regression --------------------------------------
  await test('1440px: strip is contained after stats (the 18px overrun is gone)', async () => {
    const n = boot({ viewport: 1440, preInject: [PRIVACY], activeRoute: 'home' });
    n.statsArrive();
    const c = n.containment();
    assert(c.ok, 'REGRESSION: overrun ' + c.overrun + 'px (client ' + c.clientWidth +
      ', scroll ' + c.scrollWidth + ')');
    assert(n.stripFits().fits, 'budget must also be satisfied: ' + JSON.stringify(n.stripFits()));
    assert(n.overflowed().length >= 4,
      'at least one more link must overflow than the old formula chose (got ' +
      n.overflowed().length + ')');
    assertContract(n, '1440');
  });

  await test('1440px: the OLD budget formula would have accepted the over-full strip', async () => {
    // Documents the arithmetic the fix corrects, independent of app.js.
    const p = PROFILES[1440];
    const inline9 = ['home','packets','map','live','channels','nodes','tools','observers','analytics'];
    const linkW = inline9.reduce((s, r) => s + p.w[r], 0);
    const linksGap = (inline9.length - 1) * p.linkGap;
    const oldNeeded = p.brand + p.leftGap + linkW + linksGap + p.leftGap + p.moreW + p.leftGap + p.rightFull + 32;
    const newAvail = p.topNavClient - p.padL - p.padR;
    const newNeeded = p.brand + p.leftGap + linkW + linksGap + p.leftGap + p.moreW + p.topGap + p.rightFull;
    assert(oldNeeded <= 1440, 'old formula should have said it fits (got ' + oldNeeded.toFixed(2) + ')');
    assert(newNeeded > newAvail, 'corrected formula must reject it (' +
      newNeeded.toFixed(2) + ' vs ' + newAvail.toFixed(2) + ')');
    // the missing budget, to the pixel
    const missing = (p.padL + p.padR) + (1440 - p.topNavClient) - (p.leftGap - p.topGap) - 32;
    assert(Math.abs(missing - 24.8) < 0.5, 'expected ~24.8px of unreserved budget, got ' + missing.toFixed(2));
  });

  await test('1101 / 1200 keep their measured outcome and stay contained', async () => {
    const expect = { 1101: 5, 1200: 6 };
    for (const vw of [1101, 1200]) {
      const n = boot({ viewport: vw, preInject: [PRIVACY], activeRoute: 'home' });
      n.statsArrive();
      assert.strictEqual(n.inline().length, expect[vw],
        vw + 'px: expected ' + expect[vw] + ' inline, got ' + JSON.stringify(n.inline()));
      assert(n.stripFits().fits, vw + 'px budget: ' + JSON.stringify(n.stripFits()));
      assertContract(n, vw + 'px');
    }
  });

  await test('2560px: everything inline, More hidden, contained', async () => {
    const n = boot({ viewport: 2560, preInject: [PRIVACY], activeRoute: 'home' });
    n.statsArrive();
    assert.strictEqual(n.overflowed().length, 0, 'nothing should overflow: ' + JSON.stringify(n.overflowed()));
    assert.strictEqual(n.inline().length, 12, 'all 12 inline');
    assert(n.moreWrap.classList.contains('is-hidden'), 'More hidden');
    assert(n.containment().ok, 'contained at 2560');
  });

  await test('1100 / 1024 / 768 keep the narrow-desktop contract, before and after stats', async () => {
    for (const vw of [1100, 1024, 768]) {
      const n = boot({ viewport: vw, preInject: [PRIVACY], activeRoute: 'home' });
      const before = n.inline().slice().sort();
      n.statsArrive();
      const after = n.inline().slice().sort();
      assert.deepStrictEqual(after, ['home', 'live', 'map', 'nodes', 'packets'],
        'band contract broken at ' + vw + 'px: ' + JSON.stringify(after));
      assert.deepStrictEqual(before, after, 'stats must not change the band at ' + vw + 'px');
    }
  });

  // ---- the ResizeObserver fix still works ------------------------------
  await test('late nav-right growth is still re-measured at 1101/1200/1440', async () => {
    for (const vw of [1101, 1200, 1440]) {
      const n = boot({ viewport: vw, preInject: [PRIVACY], activeRoute: 'home' });
      n.statsArrive();
      assert(n.stripFits().fits, vw + 'px: strip does not fit after stats');
      assertContract(n, vw + 'px after stats');
    }
  });

  await test('without a ResizeObserver the strip is left over-full (old behaviour)', async () => {
    for (const vw of [1101, 1200, 1440]) {
      const n = boot({ viewport: vw, preInject: [PRIVACY], activeRoute: 'home', noResizeObserver: true });
      n.statsArrive();
      assert(!n.stripFits().fits || !n.containment().ok,
        vw + 'px: expected the unobserved case to stay over-full');
    }
  });

  await test('a resize after stats still converges without the observer', async () => {
    for (const vw of [1200, 1440]) {
      const n = boot({ viewport: vw, preInject: [PRIVACY], activeRoute: 'home', noResizeObserver: true });
      n.statsArrive();
      n.resize();
      assert(n.stripFits().fits && n.containment().ok, vw + 'px: resize should converge');
    }
  });

  // ---- compressed rect vs scrollWidth ----------------------------------
  await test('fits() uses scrollWidth when the flex line compresses a link rect', async () => {
    const n = boot({ viewport: 1200, preInject: [PRIVACY], activeRoute: 'home', squeeze: true });
    n.statsArrive();
    assert(n.stripFits().fits, 'compressed rects must not fool the fit: ' + JSON.stringify(n.stripFits()));
    assertContract(n, '1200 squeezed');
  });

  await test('scrollWidth covers padding and inline SVG (measured equality holds)', async () => {
    const n = boot({ viewport: 1200 });
    const live = n.link('live');
    assert.strictEqual(live.scrollWidth, Math.round(live.w), 'scrollWidth == intrinsic incl. SVG + padding');
    assert.strictEqual(Math.max(live.getBoundingClientRect().width, live.scrollWidth),
      live.getBoundingClientRect().width, 'max() is a no-op when rect >= scrollWidth');
  });

  // ---- active route coverage -------------------------------------------
  await test('active Privacy stays inline and contained at 1101/1200/1440', async () => {
    for (const vw of [1101, 1200, 1440]) {
      const n = boot({ viewport: vw, preInject: [PRIVACY], activeRoute: 'privacy' });
      n.statsArrive();
      assert(n.inline().includes('privacy'),
        'active privacy must stay inline at ' + vw + 'px; overflowed=' + JSON.stringify(n.overflowed()));
      assertContract(n, vw + 'px active privacy');
    }
  });

  await test('a different active route (perf) stays inline and contained', async () => {
    for (const vw of [1101, 1200, 1440]) {
      const n = boot({ viewport: vw, preInject: [PRIVACY], activeRoute: 'perf' });
      n.statsArrive();
      assert(n.inline().includes('perf'), 'active perf must stay inline at ' + vw + 'px');
      assertContract(n, vw + 'px active perf');
    }
  });

  await test('pinned-set exception is documented and unchanged by this fix', async () => {
    // 1101px + a non-high active route: after every droppable link is in More,
    // the 5 high-priority links plus the active pill still need more room than
    // the box grants. #1311/#1391 forbid dropping any of them.
    const n = boot({ viewport: 1101, preInject: [PRIVACY], activeRoute: 'privacy' });
    n.statsArrive();
    const droppable = n.inline().filter(r => !HIGH.includes(r) && r !== 'privacy');
    assert.strictEqual(droppable.length, 0,
      'expected every droppable link already in More, got ' + JSON.stringify(droppable));
    assert.deepStrictEqual(n.inline().slice().sort(),
      ['home', 'live', 'map', 'nodes', 'packets', 'privacy'],
      'expected exactly the pinned set inline');
    assert(!n.containment().ok, 'this is the known over-full case (documents the floor)');
    // ...and at 1440 the same active route IS fully resolved by the fix.
    const m = boot({ viewport: 1440, preInject: [PRIVACY], activeRoute: 'privacy' });
    m.statsArrive();
    assert(m.containment().ok,
      '1440 active=privacy must be contained after the fix, got overrun ' + m.containment().overrun);
  });

  // ---- dynamic links ----------------------------------------------------
  await test('config link injected BEFORE init participates and stays contained', async () => {
    const n = boot({ viewport: 1440, preInject: [PRIVACY], activeRoute: 'home' });
    n.statsArrive();
    assert(n.overflowed().includes('privacy'), 'pre-injected privacy should overflow at 1440px');
    assert.strictEqual(n.moreRoutes().filter(r => r === 'privacy').length, 1, 'exactly one clone');
    assertContract(n, '1440 pre-injected');
  });

  await test('late config link + late nav-right growth converge together', async () => {
    for (const vw of [1200, 1440]) {
      const n = boot({ viewport: vw, activeRoute: 'home' });
      n.addLink(PRIVACY);
      n.addLink(RXCOV, { after: 'analytics' });
      n.statsArrive();                       // second async event
      assert(n.overflowed().includes('privacy'), vw + 'px: late privacy should overflow');
      assert(n.overflowed().includes('rx-coverage'), vw + 'px: late coverage should overflow');
      assert.strictEqual(n.moreRoutes().filter(r => r === 'privacy').length, 1, 'one privacy clone');
      assert.strictEqual(n.moreRoutes().filter(r => r === 'rx-coverage').length, 1, 'one coverage clone');
      assert(n.stripFits().fits, vw + 'px budget with both late links');
      assertContract(n, vw + 'px late links + stats');
    }
  });

  await test('stats arriving BEFORE a late link still ends up contained', async () => {
    const n = boot({ viewport: 1440, activeRoute: 'home' });
    n.statsArrive();
    n.addLink(PRIVACY);
    assertContract(n, '1440 stats-then-link');
    assert.strictEqual(n.moreRoutes().filter(r => r === 'privacy').length, 1, 'one privacy clone');
  });

  // ---- Priority+ contracts ---------------------------------------------
  await test('high-priority links never overflow at any width, before or after stats', async () => {
    for (const vw of [768, 1024, 1100, 1101, 1200, 1440, 2560]) {
      const n = boot({ viewport: vw, preInject: [PRIVACY], activeRoute: 'home' });
      let bad = n.overflowed().filter(r => HIGH.includes(r));
      assert.strictEqual(bad.length, 0, 'high-priority overflowed pre-stats at ' + vw + ': ' + bad);
      n.statsArrive();
      bad = n.overflowed().filter(r => HIGH.includes(r));
      assert.strictEqual(bad.length, 0, 'high-priority overflowed post-stats at ' + vw + ': ' + bad);
    }
  });

  await test('More menu keeps its >=2 floor after stats', async () => {
    for (const vw of [1101, 1200, 1440, 2560]) {
      const n = boot({ viewport: vw, preInject: [PRIVACY], activeRoute: 'home' });
      n.statsArrive();
      const c = n.moreRoutes().length;
      assert(c === 0 || c >= 2, 'degenerate 1-item More menu at ' + vw + 'px (' + c + ')');
    }
  });

  await test('no duplicate or stale clones across stats + repeated events', async () => {
    for (const vw of [1200, 1440]) {
      const n = boot({ viewport: vw, preInject: [PRIVACY], activeRoute: 'home' });
      n.statsArrive();
      for (let i = 0; i < 15; i++) { n.resize(); n.hashchange(); }
      assertContract(n, vw + 'px repeated events');
      assert.strictEqual(n.all().filter(a => a.dataset.route === 'privacy').length, 1, 'one privacy original');
    }
  });

  await test('repeated events do not change the settled result (deterministic)', async () => {
    const n = boot({ viewport: 1440, preInject: [PRIVACY], activeRoute: 'home' });
    n.statsArrive();
    const first = n.inline().join(',');
    for (let i = 0; i < 20; i++) { n.resize(); n.hashchange(); n.statsArrive(); }
    assert.strictEqual(n.inline().join(','), first, 'result drifted across repeated events');
  });

  await test('mobile (<768px) still clears overflow and hides More after stats', async () => {
    const n = boot({ viewport: 500, preInject: [PRIVACY], activeRoute: 'home' });
    n.statsArrive();
    assert.strictEqual(n.overflowed().length, 0, 'mobile must clear is-overflow');
    assert(n.moreWrap.classList.contains('is-hidden'), 'More hidden on mobile');
  });

  await test('the observer is wired exactly once and only on .nav-right', async () => {
    const n = boot({ viewport: 1440 });
    assert.strictEqual(n.observerCount(), 1, 'expected exactly one observe() call');
  });

  await test('repeated identical stats notifications do not loop', async () => {
    const n = boot({ viewport: 1440, preInject: [PRIVACY], activeRoute: 'home' });
    for (let i = 0; i < 25; i++) n.statsArrive();
    assert(n.stripFits().fits, 'strip should be settled');
    assertContract(n, '1440 repeated stats');
  });

  console.log('\n  ' + passed + ' passed, ' + failed + ' failed');
  process.exit(failed ? 1 : 0);
})();
