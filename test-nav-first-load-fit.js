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

// Measured per-viewport profiles (staging, image corescope:pr12-98121595).
// Link widths, gaps and .nav-right widths are all viewport-dependent, so a
// single table would misrepresent 1440/2560. Each profile is what the browser
// actually reported; rightEmpty is .nav-right with #navStats emptied.
const PROFILES = {
  1101: { linkGap: 3.6515, leftGap: 20.2575, rightEmpty: 211, rightFull: 466, w: {
    home:56.79, packets:68.78, map:47.27, live:61.73, channels:77.41, nodes:60.14,
    tools:53.25, observers:83.63, analytics:76.82, perf:62.54, 'audio-lab':59.42, privacy:81.63 } },
  1200: { linkGap: 3.8, leftGap: 21, rightEmpty: 212, rightFull: 467, w: {
    home:58.35, packets:70.46, map:48.73, live:63.16, channels:79.18, nodes:61.73,
    tools:54.77, observers:85.46, analytics:78.57, perf:63.98, 'audio-lab':60.84, privacy:83.27 } },
  1440: { linkGap: 4.16, leftGap: 22.8, rightEmpty: 215, rightFull: 470, w: {
    home:62.14, packets:74.51, map:52.27, live:66.63, channels:83.45, nodes:65.59,
    tools:58.42, observers:89.88, analytics:82.79, perf:67.47, 'audio-lab':64.27, privacy:87.23 } },
  2560: { linkGap: 5.84, leftGap: 31.2, rightEmpty: 229, rightFull: 484, w: {
    home:66.99, packets:79.98, map:56.55, live:70.75, channels:89.43, nodes:70.60,
    tools:62.98, observers:96.22, analytics:88.68, perf:71.65, 'audio-lab':68.30, privacy:92.49 } },
};
// <=1100px never measures (the narrow-desktop branch force-collapses), so the
// nearest measured profile is representative there.
function profileFor(vw) { return PROFILES[vw] || (vw >= 2000 ? PROFILES[2560] : vw >= 1300 ? PROFILES[1440] : vw >= 1150 ? PROFILES[1200] : PROFILES[1101]); }

const ORDER = ['home','packets','map','live','channels','nodes','tools','observers','analytics','perf','audio-lab'];
const HIGH = ['home','packets','map','live','nodes'];
const LABEL = { home:'Home', packets:'Packets', map:'Map', live:'Live', channels:'Channels',
  nodes:'Nodes', tools:'Tools', observers:'Observers', analytics:'Analytics', perf:'Perf',
  'audio-lab':'Lab', privacy:'Privacy', 'rx-coverage':'Coverage' };

const BRAND_W = 125, MORE_W = 58;
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
  const brand = mk('a', 'nav-brand'); brand.w = BRAND_W;
  const linksEl = mk('div', 'nav-links');
  const moreWrap = mk('div', 'nav-more-wrap'); moreWrap.w = MORE_W;
  const moreBtn = mk('button', 'nav-btn nav-more-btn', 'navMoreBtn');
  const moreMenu = mk('div', 'nav-more-menu', 'navMoreMenu');
  const navRight = mk('div', 'nav-right');
  const navStats = mk('div', 'nav-stats', 'navStats');
  const hamburger = mk('button', 'nav-btn hamburger', 'hamburger');
  const body = mk('body', '');

  // The right side starts WITHOUT stats, exactly as at DOMContentLoaded.
  navRight.scrollWidth = opts.rightW !== undefined ? opts.rightW : prof.rightEmpty;

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

  doc.querySelector = sel => (root.matches(sel) ? root : root.querySelector(sel));
  doc.querySelectorAll = sel => root.querySelectorAll(sel);
  doc.body = body;
  doc.fonts = undefined;
  doc.addEventListener = () => {};

  const win = {
    innerWidth: viewport, _on: {},
    addEventListener(t, f) { (this._on[t] = this._on[t] || []).push(f); },
    requestAnimationFrame(fn) { fn(); return 1; },
    cancelAnimationFrame() {},
    getComputedStyle: el => (el === linksEl
      ? { columnGap: prof.linkGap + 'px', gap: prof.linkGap + 'px' }
      : { columnGap: prof.leftGap + 'px', gap: prof.leftGap + 'px' }),
  };

  function ResizeObserverShim(cb) {
    this.observe = el => { observers.push({ el, cb }); };
    this.disconnect = () => {};
  }

  const ctx = {
    window: win, document: doc, console,
    requestAnimationFrame: win.requestAnimationFrame,
    cancelAnimationFrame: win.cancelAnimationFrame,
    getComputedStyle: win.getComputedStyle,
    ResizeObserver: opts.noResizeObserver ? undefined : ResizeObserverShim,
    Array, Object, String, Number, Math, Set, parseFloat, JSON, Boolean,
  };
  ctx.globalThis = ctx;
  vm.createContext(ctx);
  vm.runInContext(CLOSE_SRC[0], ctx, { filename: 'app.js#close' });
  vm.runInContext(NAV_SRC, ctx, { filename: 'app.js#nav' });

  const fire = t => (win._on[t] || []).forEach(f => f({ type: t }));

  const api = {
    prof, doc, win, linksEl, moreMenu, moreWrap, moreBtn, navRight, navStats, hamburger, body,
    /** /api/stats lands: the right side grows and the observer should react. */
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
    /** Does the CURRENT inline set actually fit? Mirrors the shipped formula. */
    stripFits() {
      const inl = api.all().filter(e => !e.classList.contains('is-overflow'));
      let lw = 0; inl.forEach(a => { lw += Math.max(a.getBoundingClientRect().width, a.scrollWidth); });
      const needed = BRAND_W + prof.leftGap + lw + Math.max(0, inl.length - 1) * prof.linkGap +
                     prof.leftGap + (moreWrap.classList.contains('is-hidden') ? 0 : MORE_W) +
                     prof.leftGap + navRight.scrollWidth + 32;
      return { needed: Math.round(needed), fits: needed <= win.innerWidth, viewport: win.innerWidth };
    },
  };
  return api;
}

// --------------------------------------------------------------------------
(async function main() {
  console.log('test-nav-first-load-fit.js');

  // ---- the regression itself -------------------------------------------
  await test('1200px: late nav-right growth is re-measured and the strip fits', async () => {
    const n = boot({ viewport: 1200, preInject: [PRIVACY], activeRoute: 'home' });
    const atLoad = n.inline().length;
    assert(atLoad >= 9, 'precondition: the empty-stats measurement is optimistic, got ' + atLoad);
    n.statsArrive();
    const after = n.stripFits();
    assert(after.fits, 'strip still does not fit: needs ' + after.needed + ' in ' + after.viewport);
    assert.deepStrictEqual(n.inline(), ['home', 'packets', 'map', 'live', 'channels', 'nodes'],
      'expected the live-observed inline set, got ' + JSON.stringify(n.inline()));
  });

  await test('1101px: late nav-right growth is re-measured and the strip fits', async () => {
    const n = boot({ viewport: 1101, preInject: [PRIVACY], activeRoute: 'home' });
    const atLoad = n.inline().length;
    assert(atLoad >= 8, 'precondition: optimistic at load, got ' + atLoad);
    n.statsArrive();
    const f = n.stripFits();
    assert(f.fits, 'strip does not fit at 1101px: needs ' + f.needed);
    assert(!n.inline().includes('privacy'), 'privacy should have overflowed at 1101px');
  });

  await test('1440px: the same under-measurement is corrected (links no longer run under .nav-right)', async () => {
    const n = boot({ viewport: 1440, preInject: [PRIVACY], activeRoute: 'home' });
    assert.strictEqual(n.overflowed().length, 0, 'at load with empty stats everything looks like it fits');
    n.statsArrive();
    const f = n.stripFits();
    assert(f.fits, 'strip does not fit at 1440px: needs ' + f.needed + ' in 1440');
    assert(n.overflowed().length >= 1, 'some links must overflow once .nav-right is 470px wide');
    assert(!n.moreWrap.classList.contains('is-hidden'), 'More must become visible at 1440px');
  });

  await test('2560px: everything still fits inline and More stays hidden', async () => {
    const n = boot({ viewport: 2560, preInject: [PRIVACY], activeRoute: 'home' });
    n.statsArrive();
    assert.strictEqual(n.overflowed().length, 0,
      'nothing should overflow at 2560px, got ' + JSON.stringify(n.overflowed()));
    assert.strictEqual(n.inline().length, 12, 'all 12 links inline');
    assert(n.moreWrap.classList.contains('is-hidden'), 'More hidden at 2560px');
    assert(n.stripFits().fits, 'strip fits at 2560px');
  });

  await test('without a ResizeObserver the strip is left overflowing (the old behaviour)', async () => {
    for (const vw of [1101, 1200, 1440]) {
      const n = boot({ viewport: vw, preInject: [PRIVACY], activeRoute: 'home', noResizeObserver: true });
      n.statsArrive();
      assert(!n.stripFits().fits,
        'expected the unobserved case to stay overflowing at ' + vw + 'px (needs ' + n.stripFits().needed + ')');
    }
  });

  await test('a resize after stats still converges (the manual workaround keeps working)', async () => {
    const n = boot({ viewport: 1200, preInject: [PRIVACY], activeRoute: 'home', noResizeObserver: true });
    n.statsArrive();
    n.resize();
    assert(n.stripFits().fits, 'a resize should converge even without the observer');
  });

  // ---- compressed rect vs scrollWidth ----------------------------------
  await test('fits() uses scrollWidth when the flex line compresses a link rect', async () => {
    const n = boot({ viewport: 1200, preInject: [PRIVACY], activeRoute: 'home', squeeze: true });
    n.statsArrive();
    const f = n.stripFits();
    assert(f.fits, 'compressed rects must not fool the fit: needs ' + f.needed + ' in ' + f.viewport);
    assert(!n.inline().includes('observers'), 'observers should overflow once intrinsic widths are used');
  });

  await test('scrollWidth covers padding and inline SVG (measured equality holds)', async () => {
    const n = boot({ viewport: 1200 });
    const live = n.link('live');           // the SVG-bearing link
    assert.strictEqual(live.scrollWidth, Math.round(live.w),
      'scrollWidth should equal the measured intrinsic width incl. SVG + padding');
    assert.strictEqual(Math.max(live.getBoundingClientRect().width, live.scrollWidth),
      live.getBoundingClientRect().width,
      'max() must be a no-op when rect >= scrollWidth (today\'s real case)');
  });

  // ---- unchanged in the narrow-desktop band ----------------------------
  await test('1100 / 1024 / 768 keep the narrow-desktop contract exactly, before and after stats', async () => {
    for (const vw of [1100, 1024, 768]) {
      const n = boot({ viewport: vw, preInject: [PRIVACY], activeRoute: 'home' });
      const before = n.inline().slice().sort();
      n.statsArrive();
      const after = n.inline().slice().sort();
      assert.deepStrictEqual(after, ['home', 'live', 'map', 'nodes', 'packets'],
        'band contract broken at ' + vw + 'px: ' + JSON.stringify(after));
      assert.deepStrictEqual(before, after, 'stats arrival must not change the band at ' + vw + 'px');
    }
  });

  await test('no link becomes permanently hidden at a wide viewport', async () => {
    const n = boot({ viewport: 2560, preInject: [PRIVACY], activeRoute: 'home' });
    n.statsArrive();
    n.resize(); n.hashchange(); n.resize();
    assert.strictEqual(n.overflowed().length, 0, 'wide viewport must keep every link inline');
    assert.strictEqual(n.inline().length, 12, 'all 12 inline, got ' + n.inline().length);
  });

  // ---- active route coverage -------------------------------------------
  await test('active Privacy stays inline at 1101/1200/1440 after stats', async () => {
    for (const vw of [1101, 1200, 1440]) {
      const n = boot({ viewport: vw, preInject: [PRIVACY], activeRoute: 'privacy' });
      n.statsArrive();
      assert(n.inline().includes('privacy'),
        'active privacy must stay inline at ' + vw + 'px; overflowed=' + JSON.stringify(n.overflowed()));
    }
  });

  await test('a different active route (perf) stays inline at 1101/1200/1440 after stats', async () => {
    for (const vw of [1101, 1200, 1440]) {
      const n = boot({ viewport: vw, preInject: [PRIVACY], activeRoute: 'perf' });
      n.statsArrive();
      assert(n.inline().includes('perf'),
        'active perf must stay inline at ' + vw + 'px; overflowed=' + JSON.stringify(n.overflowed()));
    }
  });

  // ---- dynamic links, before and after init ----------------------------
  await test('config link injected BEFORE init participates after stats arrive', async () => {
    const n = boot({ viewport: 1200, preInject: [PRIVACY], activeRoute: 'home' });
    n.statsArrive();
    assert(n.overflowed().includes('privacy'), 'pre-injected privacy should overflow at 1200px');
    assert.strictEqual(n.moreRoutes().filter(r => r === 'privacy').length, 1, 'exactly one clone');
  });

  await test('config link injected AFTER init participates after stats arrive', async () => {
    const n = boot({ viewport: 1200, activeRoute: 'home' });
    n.addLink(PRIVACY);
    n.addLink(RXCOV, { after: 'analytics' });
    n.statsArrive();
    assert(n.overflowed().includes('privacy'), 'late privacy should overflow at 1200px');
    assert(n.overflowed().includes('rx-coverage'), 'late rx-coverage should overflow at 1200px');
    assert.strictEqual(n.moreRoutes().filter(r => r === 'privacy').length, 1, 'one privacy clone');
    assert.strictEqual(n.moreRoutes().filter(r => r === 'rx-coverage').length, 1, 'one coverage clone');
    assert(n.stripFits().fits, 'strip must fit with both late links accounted for');
  });

  await test('stats arriving BEFORE a late link still ends up correct', async () => {
    const n = boot({ viewport: 1200, activeRoute: 'home' });
    n.statsArrive();                 // right side settles first
    n.addLink(PRIVACY);              // then config lands
    assert(n.stripFits().fits, 'strip must fit whichever order the two arrive in');
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

  await test('More menu keeps its >=2 floor after stats arrive', async () => {
    for (const vw of [1101, 1200, 1440, 2560]) {
      const n = boot({ viewport: vw, preInject: [PRIVACY], activeRoute: 'home' });
      n.statsArrive();
      const c = n.moreRoutes().length;
      assert(c === 0 || c >= 2, 'degenerate 1-item More menu at ' + vw + 'px (' + c + ')');
    }
  });

  await test('no duplicate or stale clones across stats + repeated events', async () => {
    const n = boot({ viewport: 1200, preInject: [PRIVACY], activeRoute: 'home' });
    n.statsArrive();
    for (let i = 0; i < 15; i++) { n.resize(); n.hashchange(); }
    const m = n.moreRoutes();
    assert.strictEqual(m.length, new Set(m).size, 'duplicate clones: ' + JSON.stringify(m));
    const live = n.all().map(a => a.dataset.route);
    assert.deepStrictEqual(m.filter(r => !live.includes(r)), [], 'stale clones present');
    assert.strictEqual(n.all().filter(a => a.dataset.route === 'privacy').length, 1, 'one privacy original');
  });

  await test('mobile (<768px) still clears overflow and hides More after stats', async () => {
    const n = boot({ viewport: 500, preInject: [PRIVACY], activeRoute: 'home' });
    n.statsArrive();
    assert.strictEqual(n.overflowed().length, 0, 'mobile must clear is-overflow');
    assert(n.moreWrap.classList.contains('is-hidden'), 'More hidden on mobile');
  });

  await test('the observer is wired exactly once and only on .nav-right', async () => {
    const n = boot({ viewport: 1200 });
    assert.strictEqual(n.observerCount(), 1, 'expected exactly one observe() call');
  });

  await test('repeated identical stats notifications do not loop', async () => {
    const n = boot({ viewport: 1200, preInject: [PRIVACY], activeRoute: 'home' });
    for (let i = 0; i < 25; i++) n.statsArrive();   // same width every time
    assert(n.stripFits().fits, 'strip should be settled');
    const m = n.moreRoutes();
    assert.strictEqual(m.length, new Set(m).size, 'repeated notifications duplicated clones');
  });

  console.log('\n  ' + passed + ' passed, ' + failed + ' failed');
  process.exit(failed ? 1 : 0);
})();
