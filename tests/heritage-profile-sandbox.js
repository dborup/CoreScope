/* tests/heritage-profile-sandbox.js — public/terrain-profile.js in a
 * browser-shaped vm sandbox for the #723 profile warning suites
 * (test-issue-723-profile-warning.js, test-issue-723-m4-heritage-customizer.js).
 * Moved here from the M3 suite unchanged, except:
 *   - the window keeps its event listeners, so a suite can dispatch the
 *     customizer's events (#723 M4: heritage-defaults-changed);
 *   - load({ rules: true }) runs public/map-tile-providers.js first, as
 *     index.html does, for the monuments settings rule (#723 M4).
 * Nothing of production logic is copied; no request leaves the process.
 */
'use strict';
const vm = require('vm');
const fs = require('fs');
const path = require('path');
const ROOT = path.join(__dirname, '..');
const SRC = fs.readFileSync(path.join(ROOT, 'public', 'terrain-profile.js'), 'utf8');
const PK_A = 'a'.repeat(64);
const PK_B = 'b'.repeat(64);
const NODE_LL = { [PK_A]: [56.43001, 9.40002], [PK_B]: [56.44003, 9.43004] };

const CFG = {
  enabled: true, url: 'https://www.kulturarv.dk/ffgeoserver/public/wms', layers: 'x', minZoom: 12, maxZoom: 19,
  bounds: [[54.4, 7.9], [57.8, 15.7]], attribution: '© Slots- og Kulturstyrelsen',
};
const INSIDE = {
  radiusM: 100, count: 1, truncated: false, attribution: '© Slots- og Kulturstyrelsen',
  nearest: { type: 'Rundhøj', dating: 'Oldtid', status: 'Fredet', protectionNumber: '3624:15', systemNumber: '190001', distanceM: 0, insideZone: true },
};
const NEAR = {
  radiusM: 100, count: 1, truncated: false, attribution: '© Slots- og Kulturstyrelsen',
  nearest: { type: 'Vandfyldt grav', dating: 'Middelalder', status: 'Fredet', distanceM: 42, insideZone: false },
};
const NONE = { radiusM: 100, count: 0, truncated: false, attribution: '© Slots- og Kulturstyrelsen', nearest: null };
const WARNING = 'Protected ancient monument / protection zone nearby — check before building';

// ── Sandbox ────────────────────────────────────────────────────────────────

function makeStorage() {
  const store = {};
  return {
    getItem: k => (Object.prototype.hasOwnProperty.call(store, k) ? store[k] : null),
    setItem: (k, v) => { store[k] = String(v); },
    removeItem: k => { delete store[k]; },
  };
}

function fakeDom() {
  const made = [];
  function mkEl(tag) {
    const on = {};
    const classes = new Set();
    const e = {
      tagName: String(tag).toUpperCase(), children: [], attrs: {}, style: {},
      className: '', textContent: '', innerHTML: '', value: '', hidden: false, disabled: false, title: '',
      parentNode: null,
      classList: {
        add: (c) => classes.add(c),
        remove: (c) => classes.delete(c),
        contains: (c) => classes.has(c),
        toggle: (c, want) => { const v = want === undefined ? !classes.has(c) : !!want; if (v) classes.add(c); else classes.delete(c); },
      },
      setAttribute: (k, v) => { e.attrs[k] = String(v); },
      getAttribute: (k) => (Object.prototype.hasOwnProperty.call(e.attrs, k) ? e.attrs[k] : null),
      appendChild: (c) => { e.children.push(c); c.parentNode = e; return c; },
      removeChild: (c) => { e.children = e.children.filter(x => x !== c); c.parentNode = null; return c; },
      addEventListener: (t, fn) => { (on[t] || (on[t] = [])).push(fn); },
      removeEventListener: (t, fn) => { on[t] = (on[t] || []).filter(f => f !== fn); },
      fire: (t) => { (on[t] || []).slice().forEach(fn => fn({ target: e, preventDefault() {} })); },
    };
    made.push(e);
    return e;
  }
  const body = mkEl('body');
  return {
    doc: { createElement: mkEl, body: body, addEventListener() {}, querySelector: () => null, querySelectorAll: () => [] },
    body,
    find: (id) => made.filter(e => e.attrs['data-testid'] === id).pop() || null,
  };
}

function load(opts) {
  opts = opts || {};
  const dom = fakeDom();
  const listeners = {};
  const loc = { origin: 'http://x', pathname: '/', search: '', hash: '#/map' };
  const win = {
    addEventListener: (t, fn) => { (listeners[t] = listeners[t] || []).push(fn); },
    removeEventListener: (t, fn) => { listeners[t] = (listeners[t] || []).filter(f => f !== fn); },
    dispatchEvent: (ev) => { (listeners[ev.type] || []).slice().forEach(fn => fn(ev)); return true; },
    listeners,
    localStorage: makeStorage(), document: dom.doc, location: loc,
    history: { state: null, replaceState: (s, t, url) => { const hi = url.indexOf('#'); loc.hash = hi >= 0 ? url.slice(hi) : ''; } },
    setTimeout: (fn, ms) => setTimeout(fn, ms),
    clearTimeout: (t) => clearTimeout(t),
  };
  const ctx = {
    window: win, console, Math, Date, JSON, Array, Object, String, Number,
    RegExp, Error, TypeError, Boolean, isNaN, isFinite, parseInt, parseFloat,
    URLSearchParams, Map, Set, Promise, encodeURIComponent, decodeURIComponent,
    setTimeout, clearTimeout, document: dom.doc,
  };
  vm.createContext(ctx);
  ctx.location = loc;
  ctx.history = win.history;
  ctx.localStorage = win.localStorage;
  ctx.CustomEvent = function (type, init) { this.type = type; this.detail = (init && init.detail) || null; };
  win.CustomEvent = ctx.CustomEvent;
  if (opts.setup) opts.setup(win);
  if (opts.rules) vm.runInContext(fs.readFileSync(path.join(ROOT, 'public', 'map-tile-providers.js'), 'utf8'), ctx, { filename: 'map-tile-providers.js' });
  vm.runInContext(fs.readFileSync(path.join(ROOT, 'public', 'antenna-height.js'), 'utf8'), ctx);
  vm.runInContext(SRC, ctx, { filename: 'terrain-profile.js' });
  return { TP: ctx.window.TerrainProfile, dom, win };
}

function endLL(ref) {
  if (NODE_LL[ref]) return NODE_LL[ref];
  return ref.split(',').map(Number);
}

/* One stub for both endpoints. heritage(lat, lon) picks the answer for a
 * lookup: an object is the body, an Error a failed request, and a
 * function returning a promise one the test resolves by hand. */
function stubFetch(heritage) {
  const calls = [];
  const fn = (url) => {
    calls.push(url);
    const [path, qs] = url.split('?');
    const q = new URLSearchParams(qs);
    if (path === '/api/terrain/heritage') {
      const r = heritage ? heritage(Number(q.get('lat')), Number(q.get('lon'))) : NONE;
      if (r instanceof Error) return Promise.reject(r);
      if (typeof r === 'function') return r();
      return Promise.resolve(JSON.parse(JSON.stringify(r)));
    }
    if (path !== '/api/terrain/profile') return Promise.reject(new Error('unexpected ' + url));
    if (fn.failProfile) return Promise.reject(new Error('Request failed (502)'));
    const end = (side, h) => {
      const ll = endLL(q.get(side));
      return {
        lat: ll[0], lon: ll[1], kind: 'point', groundM: 0,
        antennaM: q.get(h) !== null ? Number(q.get(h)) : 2,
        antennaSource: q.get(h) !== null ? 'request' : 'point-default', assumed: q.get(h) === null,
      };
    };
    return Promise.resolve({
      a: end('a', 'ha'), b: end('b', 'hb'), distanceM: 1000, model: q.get('model'),
      coverage: { state: 'full', pct: 100 },
      samples: [{ d: 0, dtm: 0 }, { d: 500, dtm: 0 }, { d: 1000, dtm: 0 }],
      physics: { freqMHz: 869.618, freqSource: 'firmware', lambdaM: 0.34474, k: 1, f1MaxM: 9 },
      verdict: { state: 'fresnel60', criterion: 0.6 },
    });
  };
  fn.calls = calls;
  fn.heritage = () => calls.filter(u => u.indexOf('/api/terrain/heritage?') === 0);
  fn.profiles = () => calls.filter(u => u.indexOf('/api/terrain/profile?') === 0);
  return fn;
}

function openTool(env, opts) {
  opts = opts || {};
  const fetchJSON = stubFetch(opts.heritage);
  const tool = env.TP.create(Object.assign({
    mount: env.dom.body, fetchJSON, heritage: opts.cfg === undefined ? CFG : opts.cfg,
  }, opts.extra || {}));
  return { tool, fetchJSON };
}

const A1 = '56.43000,9.40000', B1 = '56.44000,9.43000';
function setPoints(tool, a, b) {
  const ra = a.split(',').map(Number), rb = b.split(',').map(Number);
  tool.setState({ a: { kind: 'point', lat: ra[0], lon: ra[1] }, b: { kind: 'point', lat: rb[0], lon: rb[1] } });
}

function ui(env) {
  const chip = env.dom.find('tp-heritage-chip'), note = env.dom.find('tp-heritage-note'), verdict = env.dom.find('tp-verdict');
  return {
    chip, note, verdict,
    shown: !!(chip && !chip.hidden), noteShown: !!(note && !note.hidden),
    verdictSnap: verdict ? { text: verdict.textContent, cls: verdict.className, state: verdict.attrs['data-state'], title: verdict.title } : null,
    detail: env.dom.find('tp-detail').textContent,
  };
}

// Answers by end: A1 inside a zone, B1 42 m from one.
const byEnd = (a, b) => (lat) => (Math.abs(lat - 56.43) < 0.005 ? a : b);

module.exports = { SRC, PK_A, PK_B, NODE_LL, CFG, INSIDE, NEAR, NONE, WARNING, makeStorage, fakeDom, load, endLL, stubFetch, openTool, A1, B1, setPoints, ui, byEnd };
