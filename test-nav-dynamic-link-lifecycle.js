#!/usr/bin/env node
/* Nav Priority+ lifecycle for DYNAMICALLY injected links.
 *
 * Root cause this locks down:
 *   public/app.js took `allLinks` as an init-time snapshot of .nav-link.
 *   public/roles.js injects the opt-in routes (privacy, rx-coverage) only
 *   once /api/config/client resolves, which RACES that init. When app.js
 *   won the race (typically a warm cache: the 99KB bundle is instant while
 *   the config still needs a round trip), the injected links were absent
 *   from the snapshot forever. They never gained .is-overflow, their width
 *   never entered fits(), and the "More" button rendered on top of them.
 *
 *   Observed live on staging 2026-09-01:
 *     config 791.7ms < app.js 933.8ms  -> Privacy handled correctly
 *     link re-added after nav-init     -> overflowCapable=false, stuck inline
 *
 *   The click lifecycle had the same shape: a per-link `closeNav` listener
 *   registered once at init missed every late link, so a dynamically added
 *   inline link did not close the hamburger on mobile.
 *
 * This test runs the REAL production source — the nav block is sliced out
 * of public/app.js and evaluated in a vm sandbox against a DOM shim. It is
 * not a reimplementation of the algorithm; if app.js changes, this runs the
 * changed code. (Playwright/jsdom are not installed in this repo, so the
 * existing browser-based nav suites cannot run here — see test-nav-priority-*.js.)
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

// ---------------------------------------------------------------------------
// Extract the real production code
// ---------------------------------------------------------------------------
const APP = fs.readFileSync('public/app.js', 'utf8');

function slice(startMarker, endMarker, label) {
  const a = APP.indexOf(startMarker);
  const b = APP.indexOf(endMarker, a + 1);
  if (a < 0 || b < 0) throw new Error('could not slice ' + label + ' from public/app.js');
  return APP.slice(a, b);
}
// The nav block: hamburger wiring + delegated click + the whole Priority+ engine.
const NAV_SRC = slice('// --- Hamburger Menu ---', "document.addEventListener('keydown'", 'nav block');
// The real closeNav/closeMoreMenu the nav block calls.
const CLOSE_SRC = APP.match(/function closeNav\(\)[\s\S]*?\n\}\n\nfunction closeMoreMenu\(\)[\s\S]*?\n\}/);
if (!CLOSE_SRC) throw new Error('could not slice closeNav/closeMoreMenu from public/app.js');

// ---------------------------------------------------------------------------
// Minimal DOM shim (no jsdom in this repo)
// ---------------------------------------------------------------------------
function camel(s) { return s.replace(/-([a-z])/g, (_, c) => c.toUpperCase()); }

class ClassList {
  constructor() { this._s = new Set(); }
  add(...c) { c.forEach(x => x && this._s.add(x)); }
  remove(...c) { c.forEach(x => this._s.delete(x)); }
  contains(c) { return this._s.has(c); }
  toggle(c, force) {
    const want = force === undefined ? !this._s.has(c) : !!force;
    if (want) this._s.add(c); else this._s.delete(c);
    return want;
  }
  get value() { return [...this._s].join(' '); }
}

class El {
  constructor(tag) {
    this.tagName = String(tag).toUpperCase();
    this.classList = new ClassList();
    this.dataset = {};
    this.children = [];
    this.parentElement = null;
    this.attrs = {};
    this._on = {};
    this.style = {};
    this.textContent = '';
    this.w = 0;             // intrinsic width for getBoundingClientRect
    this.scrollWidth = 0;
    this.id = '';
  }
  get className() { return this.classList.value; }
  set className(v) { this.classList._s = new Set(String(v).split(/\s+/).filter(Boolean)); }

  appendChild(c) {
    if (c.parentElement) c.parentElement.removeChild(c);
    c.parentElement = this; this.children.push(c); return c;
  }
  removeChild(c) {
    const i = this.children.indexOf(c);
    if (i >= 0) this.children.splice(i, 1);
    c.parentElement = null; return c;
  }
  remove() { if (this.parentElement) this.parentElement.removeChild(this); }
  insertAdjacentElement(pos, el) {
    const p = this.parentElement;
    const i = p.children.indexOf(this);
    if (el.parentElement) el.parentElement.removeChild(el);
    el.parentElement = p;
    p.children.splice(pos === 'afterend' ? i + 1 : i, 0, el);
    return el;
  }
  setAttribute(k, v) {
    this.attrs[k] = String(v);
    if (k === 'class') this.className = v;
    else if (k === 'id') this.id = String(v);
    else if (k.indexOf('data-') === 0) this.dataset[camel(k.slice(5))] = String(v);
  }
  getAttribute(k) {
    if (k === 'class') return this.className;
    if (k === 'id') return this.id || null;
    return this.attrs[k] !== undefined ? this.attrs[k] : null;
  }
  get innerHTML() { return this._html || ''; }
  set innerHTML(v) {
    this._html = String(v);
    if (String(v) === '') { this.children.forEach(c => { c.parentElement = null; }); this.children = []; }
  }
  cloneNode(deep) {
    const c = new El(this.tagName);
    c.className = this.className;
    Object.assign(c.dataset, this.dataset);
    Object.assign(c.attrs, this.attrs);
    c.id = this.id; c.w = this.w; c.textContent = this.textContent; c._html = this._html;
    if (deep) this.children.forEach(ch => c.appendChild(ch.cloneNode(true)));
    return c;
  }
  contains(n) { return n === this || this.children.some(c => c.contains(n)); }
  closest(sel) { let n = this; while (n) { if (n.matches(sel)) return n; n = n.parentElement; } return null; }
  matches(sel) {
    // Single compound selector: tag / #id / .cls (repeatable) / [k="v"]
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

function fireClick(el) {
  const ev = { type: 'click', target: el, stopPropagation() {}, preventDefault() {} };
  let n = el;
  while (n) { (n._on['click'] || []).forEach(f => f(ev)); n = n.parentElement; }
}

// Static nav, mirroring public/index.html (widths are plausible px values).
const STATIC_LINKS = [
  { route: 'home', label: 'Home', w: 60, high: true },
  { route: 'packets', label: 'Packets', w: 76, high: true },
  { route: 'map', label: 'Map', w: 50, high: true },
  { route: 'live', label: 'Live', w: 60, high: true },
  { route: 'channels', label: 'Channels', w: 84 },
  { route: 'nodes', label: 'Nodes', w: 62, high: true },
  { route: 'tools', label: 'Tools', w: 56 },
  { route: 'observers', label: 'Observers', w: 92 },
  { route: 'analytics', label: 'Analytics', w: 84 },
  { route: 'perf', label: 'Perf', w: 56 },
  { route: 'audio-lab', label: 'Lab', w: 52 },
];

function makeLink(doc, spec) {
  const a = doc.createElement('a');
  a.className = 'nav-link';
  a.setAttribute('data-route', spec.route);
  if (spec.high) a.setAttribute('data-priority', 'high');
  a.setAttribute('href', '#/' + spec.route);
  a.textContent = spec.label;
  a.w = spec.w;
  return a;
}

/**
 * Build the DOM, optionally pre-injecting links, then evaluate the REAL
 * nav source. `preInject` models config-wins-the-race (link present before
 * nav-init); using api.addLink() afterwards models app.js-wins-the-race.
 */
function boot(opts) {
  opts = opts || {};
  const viewport = opts.viewport || 1920;
  const byId = {};

  const doc = {
    createElement: (t) => new El(t),
    getElementById: (id) => byId[id] || null,
  };

  const mk = (tag, cls, id) => {
    const e = new El(tag);
    if (cls) e.className = cls;
    if (id) { e.id = id; e.setAttribute('id', id); byId[id] = e; }
    return e;
  };

  const topNav = mk('nav', 'top-nav');
  const navLeft = mk('div', 'nav-left');
  const brand = mk('a', 'nav-brand'); brand.w = 125;
  const linksEl = mk('div', 'nav-links');
  const moreWrap = mk('div', 'nav-more-wrap'); moreWrap.w = 70;
  const moreBtn = mk('button', 'nav-btn nav-more-btn', 'navMoreBtn');
  const moreMenu = mk('div', 'nav-more-menu', 'navMoreMenu');
  const navRight = mk('div', 'nav-right'); navRight.scrollWidth = opts.rightW || 300;
  // applyNavPriority() budgets against .top-nav's own content box and gap
  // (see fits() in public/app.js), so the shim has to model them.
  const TOPNAV_PAD = 24, TOP_GAP = 16, SCROLLBAR = 6;
  topNav.clientWidth = viewport - SCROLLBAR;
  const hamburger = mk('button', 'nav-btn hamburger', 'hamburger');
  const body = mk('body', '');

  topNav.appendChild(navLeft);
  navLeft.appendChild(brand);
  navLeft.appendChild(linksEl);
  navLeft.appendChild(moreWrap);
  moreWrap.appendChild(moreBtn);
  moreWrap.appendChild(moreMenu);
  topNav.appendChild(navRight);
  navRight.appendChild(hamburger);

  const root = new El('html');
  root.appendChild(topNav);
  root.appendChild(body);

  // .nav-links reports what flex granted it vs what its content needs.
  const LINK_GAP = 24, LEFT_GAP = 24, BRAND_W = 125, MORE_W = 70;
  function inlineLinks() {
    return linksEl.children.filter(e => e.classList && e.classList.contains('nav-link') &&
                                        !e.classList.contains('is-overflow'));
  }
  function naturalScrollW() {
    const inl = inlineLinks();
    let w = 0; inl.forEach(a => { w += Math.max(a.getBoundingClientRect().width, a.scrollWidth); });
    return w + Math.max(0, inl.length - 1) * LINK_GAP;
  }
  function maxGrantedW() {
    const moreW = moreWrap.classList.contains('is-hidden') ? 0 : MORE_W;
    return (topNav.clientWidth - 2 * TOPNAV_PAD) - TOP_GAP - navRight.scrollWidth
           - BRAND_W - 2 * LEFT_GAP - moreW;
  }
  Object.defineProperty(linksEl, 'scrollWidth', { get: () => Math.round(naturalScrollW()) });
  Object.defineProperty(linksEl, 'clientWidth', {
    get: () => Math.round(Math.max(0, Math.min(naturalScrollW(), maxGrantedW()))) });

  STATIC_LINKS.forEach(s => linksEl.appendChild(makeLink(doc, s)));
  (opts.preInject || []).forEach(s => linksEl.appendChild(makeLink(doc, s)));
  if (opts.activeRoute) {
    const a = linksEl.querySelector('[data-route="' + opts.activeRoute + '"]');
    if (a) { a.classList.add('active'); a.w += 20; } // active pill is wider
  }

  doc.querySelector = (sel) => (root.matches(sel) ? root : root.querySelector(sel));
  doc.querySelectorAll = (sel) => root.querySelectorAll(sel);
  doc.body = body;
  doc.fonts = undefined;                 // skip the async font hook
  doc.addEventListener = () => {};       // document-level handlers are outside the slice

  const win = {
    innerWidth: viewport,
    _on: {},
    addEventListener(t, f) { (this._on[t] = this._on[t] || []).push(f); },
    requestAnimationFrame(fn) { fn(); return 1; },   // synchronous => deterministic
    cancelAnimationFrame() {},
    getComputedStyle: el => (el === topNav
      ? { columnGap: TOP_GAP + 'px', gap: TOP_GAP + 'px',
          paddingLeft: TOPNAV_PAD + 'px', paddingRight: TOPNAV_PAD + 'px' }
      : { columnGap: '24px', gap: '24px', paddingLeft: '0px', paddingRight: '0px' }),
  };

  const ctx = {
    window: win, document: doc, console,
    requestAnimationFrame: win.requestAnimationFrame,
    cancelAnimationFrame: win.cancelAnimationFrame,
    getComputedStyle: win.getComputedStyle,
    Array, Object, String, Number, Math, Set, parseFloat, JSON, Boolean,
  };
  ctx.globalThis = ctx;
  vm.createContext(ctx);
  vm.runInContext(CLOSE_SRC[0], ctx, { filename: 'app.js#close' });
  vm.runInContext(NAV_SRC, ctx, { filename: 'app.js#nav' });

  const fire = (type) => (win._on[type] || []).forEach(f => f({ type }));

  const api = {
    doc, win, linksEl, moreMenu, moreWrap, moreBtn, hamburger, body,
    // Inject a link the way roles.js does, then nudge with resize.
    addLink(spec, opts2) {
      const a = makeLink(doc, spec);
      if (opts2 && opts2.after) {
        linksEl.querySelector('[data-route="' + opts2.after + '"]').insertAdjacentElement('afterend', a);
      } else {
        linksEl.appendChild(a);
      }
      if (spec.active) { a.classList.add('active'); a.w += 20; }
      fire('resize');
      return a;
    },
    resize(w) { if (w) win.innerWidth = w; fire('resize'); },
    hashchange() { fire('hashchange'); },
    inline: () => linksEl.children.filter(e => e.classList.contains('nav-link') && !e.classList.contains('is-overflow')).map(e => e.dataset.route),
    overflowed: () => linksEl.children.filter(e => e.classList.contains('nav-link') && e.classList.contains('is-overflow')).map(e => e.dataset.route),
    moreRoutes: () => moreMenu.children.map(e => e.dataset.route),
    link: (r) => linksEl.querySelector('[data-route="' + r + '"]'),
  };
  return api;
}

const PRIVACY = { route: 'privacy', label: 'Privacy', w: 72 };
const RXCOV = { route: 'rx-coverage', label: 'Coverage', w: 88 };

// ---------------------------------------------------------------------------
(async function main() {
  console.log('test-nav-dynamic-link-lifecycle.js');

  // --- A: link present BEFORE nav-init (config won the race) -------------
  await test('A. dynamic Privacy link injected BEFORE nav-init is overflow-capable', async () => {
    const n = boot({ viewport: 1000, preInject: [PRIVACY] });
    assert(n.link('privacy'), 'privacy link should exist');
    assert(n.overflowed().includes('privacy'),
      'privacy should overflow at 1000px; overflowed=' + JSON.stringify(n.overflowed()));
    assert(n.moreRoutes().includes('privacy'), 'privacy should appear in the More menu');
  });

  // --- B: link added AFTER nav-init (app.js won the race) — THE BUG ------
  await test('B. dynamic Privacy link injected AFTER nav-init is overflow-capable', async () => {
    const n = boot({ viewport: 1000 });
    assert(!n.link('privacy'), 'privacy must not exist before injection');
    n.addLink(PRIVACY);
    assert(n.overflowed().includes('privacy'),
      'REGRESSION: late link stuck inline; overflowed=' + JSON.stringify(n.overflowed()));
    assert(n.moreRoutes().includes('privacy'), 'late link should appear in the More menu');
  });

  // --- C: rx-coverage, the same injection pattern ------------------------
  await test('C. dynamic rx-coverage link injected AFTER nav-init is overflow-capable', async () => {
    const n = boot({ viewport: 1000 });
    n.addLink(RXCOV, { after: 'analytics' });          // roles.js inserts it here
    assert(n.link('rx-coverage'), 'rx-coverage link should exist');
    assert(n.overflowed().includes('rx-coverage'),
      'REGRESSION: rx-coverage stuck inline; overflowed=' + JSON.stringify(n.overflowed()));
    assert(n.moreRoutes().includes('rx-coverage'), 'rx-coverage should appear in More');
  });

  // --- D: wide viewport keeps the late link inline -----------------------
  await test('D. late link stays inline at a wide viewport, More stays hidden', async () => {
    const n = boot({ viewport: 2560 });
    n.addLink(PRIVACY);
    assert(n.inline().includes('privacy'), 'privacy should be inline at 2560px');
    assert.strictEqual(n.overflowed().length, 0,
      'nothing should overflow at 2560px; got ' + JSON.stringify(n.overflowed()));
    assert(n.moreWrap.classList.contains('is-hidden'), 'More button should be hidden at 2560px');
  });

  // --- E: narrow desktop moves the late link into More -------------------
  await test('E. late link moves into More at a narrow desktop viewport', async () => {
    const n = boot({ viewport: 2560 });
    n.addLink(PRIVACY);
    assert(n.inline().includes('privacy'), 'inline at 2560px first');
    n.resize(1000);
    assert(n.overflowed().includes('privacy'), 'privacy should overflow after resize to 1000px');
    assert(!n.moreWrap.classList.contains('is-hidden'), 'More button should be visible');
  });

  // --- F: an ACTIVE late link stays inline at >=768px (#1391) ------------
  await test('F. active late link stays inline at >=768px', async () => {
    for (const vw of [768, 900, 1000, 1100, 1300]) {
      const n = boot({ viewport: vw });
      n.addLink(Object.assign({}, PRIVACY, { active: true }));
      assert(n.inline().includes('privacy'),
        'active late link must stay inline at ' + vw + 'px; overflowed=' + JSON.stringify(n.overflowed()));
    }
  });

  // --- G: a high-priority late link is never overflowed (#1311) ----------
  await test('G. high-priority late link is never overflowed', async () => {
    for (const vw of [800, 1000, 1200]) {
      const n = boot({ viewport: vw });
      n.addLink({ route: 'privacy', label: 'Privacy', w: 72, high: true });
      assert(n.inline().includes('privacy'),
        'high-priority late link must stay inline at ' + vw + 'px');
    }
  });

  // --- H: removing the original clears its stale clone -------------------
  await test('H. removing a late link removes its clone from More', async () => {
    const n = boot({ viewport: 1000 });
    n.addLink(PRIVACY);
    assert(n.moreRoutes().includes('privacy'), 'precondition: privacy in More');
    n.link('privacy').remove();
    n.resize();
    assert(!n.moreRoutes().includes('privacy'),
      'stale clone survived removal; More=' + JSON.stringify(n.moreRoutes()));
    assert(!n.link('privacy'), 'original should be gone');
  });

  // --- I: repeated events never accumulate clones ------------------------
  await test('I. repeated resize/hashchange yields exactly one clone', async () => {
    const n = boot({ viewport: 1000 });
    n.addLink(PRIVACY);
    for (let i = 0; i < 12; i++) { n.resize(); n.hashchange(); }
    const count = n.moreRoutes().filter(r => r === 'privacy').length;
    assert.strictEqual(count, 1, 'expected exactly 1 privacy clone, got ' + count);
    const inlineCount = n.linksEl.children.filter(e => e.dataset.route === 'privacy').length;
    assert.strictEqual(inlineCount, 1, 'expected exactly 1 privacy original, got ' + inlineCount);
    // and the menu as a whole must not have grown
    assert.strictEqual(n.moreRoutes().length, new Set(n.moreRoutes()).size,
      'More menu contains duplicates: ' + JSON.stringify(n.moreRoutes()));
  });

  // --- J: a late inline link closes the hamburger on mobile --------------
  await test('J. late inline link closes the hamburger on mobile', async () => {
    const n = boot({ viewport: 500 });
    const late = n.addLink(PRIVACY);
    n.linksEl.classList.add('open');
    n.body.classList.add('nav-open');
    fireClick(late);
    assert(!n.linksEl.classList.contains('open'),
      'REGRESSION: late link did not close the hamburger panel');
    assert(!n.body.classList.contains('nav-open'), 'body.nav-open should be cleared');
  });

  await test('J2. a STATIC link still closes the hamburger (no lost coverage)', async () => {
    const n = boot({ viewport: 500 });
    n.linksEl.classList.add('open');
    n.body.classList.add('nav-open');
    fireClick(n.link('tools'));
    assert(!n.linksEl.classList.contains('open'), 'static link must still close the panel');
  });

  await test('J3. delegation does not double-register on static links', async () => {
    // A second listener would still only toggle once (closeNav is idempotent),
    // so assert structurally: no per-link click listeners are registered.
    const n = boot({ viewport: 500 });
    const perLink = n.link('tools')._on['click'] || [];
    assert.strictEqual(perLink.length, 0,
      'links should have no per-element click handler (delegated on .nav-links)');
    const delegated = n.linksEl._on['click'] || [];
    assert.strictEqual(delegated.length, 1,
      'exactly one delegated handler expected, got ' + delegated.length);
  });

  // --- K: pre-existing Priority+ contracts still hold --------------------
  await test('K1. 768-1100px band shows exactly the high-priority set', async () => {
    for (const vw of [768, 900, 1100]) {
      const n = boot({ viewport: vw });
      assert.deepStrictEqual(n.inline().sort(),
        ['home', 'live', 'map', 'nodes', 'packets'],
        'band contract broken at ' + vw + 'px: inline=' + JSON.stringify(n.inline()));
    }
  });

  await test('K2. 768-1100px band contract survives a late link', async () => {
    const n = boot({ viewport: 1000 });
    n.addLink(PRIVACY);
    assert.deepStrictEqual(n.inline().sort(),
      ['home', 'live', 'map', 'nodes', 'packets'],
      'late link broke the band contract: inline=' + JSON.stringify(n.inline()));
  });

  await test('K3. high-priority links are never overflowed at any desktop width', async () => {
    for (const vw of [768, 900, 1000, 1100, 1200, 1400, 1920, 2560]) {
      const n = boot({ viewport: vw });
      n.addLink(PRIVACY);
      const HIGH = ['home', 'packets', 'map', 'live', 'nodes'];
      const bad = n.overflowed().filter(r => HIGH.includes(r));
      assert.strictEqual(bad.length, 0,
        'high-priority overflowed at ' + vw + 'px: ' + JSON.stringify(bad));
    }
  });

  await test('K4. active route stays inline at every desktop width', async () => {
    for (const vw of [768, 1000, 1100, 1200, 1600, 2560]) {
      const n = boot({ viewport: vw, activeRoute: 'perf' });
      n.addLink(PRIVACY);
      assert(n.inline().includes('perf'),
        'active route overflowed at ' + vw + 'px; overflowed=' + JSON.stringify(n.overflowed()));
    }
  });

  await test('K5. More menu keeps its >=2 floor where possible', async () => {
    for (const vw of [1000, 1200, 1400, 1600]) {
      const n = boot({ viewport: vw });
      n.addLink(PRIVACY);
      const c = n.moreRoutes().length;
      assert(c === 0 || c >= 2, 'degenerate 1-item More menu at ' + vw + 'px');
    }
  });

  await test('K6. More button active state tracks an overflowed active route', async () => {
    const n = boot({ viewport: 1000, activeRoute: 'tools' });
    // tools is non-high; at <=1100 the band keeps the ACTIVE link inline,
    // so More must NOT claim active while the active link is inline.
    assert(n.inline().includes('tools'), 'active tools should stay inline');
    assert(!n.moreBtn.classList.contains('active'),
      'More must not be active while the active route is inline');
  });

  await test('K7. mobile (<768px) clears overflow state for late links too', async () => {
    const n = boot({ viewport: 1000 });
    n.addLink(PRIVACY);
    assert(n.overflowed().includes('privacy'), 'precondition: overflowed at 1000px');
    n.resize(500);
    assert.strictEqual(n.overflowed().length, 0,
      'mobile must clear is-overflow; got ' + JSON.stringify(n.overflowed()));
    assert(n.moreWrap.classList.contains('is-hidden'), 'More hidden on mobile');
  });

  await test('K8. More menu clones carry role=menuitem and drop is-overflow', async () => {
    const n = boot({ viewport: 1000 });
    const late = n.addLink(PRIVACY);
    const clone = n.moreMenu.children.find(c => c.dataset.route === 'privacy');
    assert(clone, 'late overflowed link should be cloned into More at 1000px');
    assert.strictEqual(clone.getAttribute('role'), 'menuitem', 'clone needs role=menuitem');
    assert(!clone.classList.contains('is-overflow'),
      'the clone lives in the menu, so it must not carry is-overflow');
    assert(late.classList.contains('is-overflow'), 'the original stays marked overflowed');
    assert.strictEqual(clone.dataset.route, 'privacy', 'clone keeps its data-route');
  });

  await test('K9. active route is never cloned into More at >=768px (#1391)', async () => {
    for (const vw of [1000, 1400]) {
      const n = boot({ viewport: vw, activeRoute: 'audio-lab' });
      n.addLink(PRIVACY);
      assert(!n.moreRoutes().includes('audio-lab'),
        'active route leaked into More at ' + vw + 'px');
      assert(!n.moreBtn.classList.contains('active'),
        'More must not be active while the active route is inline at ' + vw + 'px');
    }
  });

  console.log('\n  ' + passed + ' passed, ' + failed + ' failed');
  process.exit(failed ? 1 : 0);
})();
