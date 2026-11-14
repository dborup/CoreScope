/* test-issue-723-heritage-info.js — click-for-info on the protected ancient
 * monuments overlay (#723 M2), public/heritage-info.js.
 *
 * Runs the real file in a vm context with a recording Leaflet map stub and
 * a minimal DOM:
 *   - click precedence: no request with the overlay off, below its
 *     minZoom, while the host is busy (profile picking / dragging, also
 *     when the host disarms in the same click), on a marker, path, popup
 *     or control, or for a click a layer stopped;
 *   - one request per click, to /api/terrain/heritage?lat=&lon= only, the
 *     previous one aborted, a stale answer never rendered;
 *   - the popup for inside / nearby / nothing / truncated / 429 / failure,
 *     built with textContent only: dataset text is never parsed as HTML;
 *   - the overlay going off closes the popup and aborts its request;
 *   - the popup CSS uses CSS variables only and a 44px close button on
 *     touch screens;
 *   - Map and Live attach it, and index.html loads it before them.
 *
 * The real Leaflet path is covered by test-issue-723-heritage-info-e2e.js.
 * Runs via: node test-issue-723-heritage-info.js
 */
'use strict';
const fs = require('fs');
const path = require('path');
const vm = require('vm');
const assert = require('assert');

let passed = 0, failed = 0;
const pending = [];
function test(name, fn) { pending.push([name, fn]); }

const SRC = fs.readFileSync(path.join(__dirname, 'public', 'heritage-info.js'), 'utf8');

// ── Minimal DOM ────────────────────────────────────────────────────────────
class El {
  constructor(tag) { this.tagName = tag.toUpperCase(); this.children = []; this.attrs = {}; this.className = ''; this._text = ''; this.parentNode = null; }
  appendChild(c) { c.parentNode = this; this.children.push(c); return c; }
  setAttribute(k, v) { this.attrs[k] = String(v); }
  getAttribute(k) { return Object.prototype.hasOwnProperty.call(this.attrs, k) ? this.attrs[k] : null; }
  set textContent(v) { this.children = []; this._text = String(v); }
  get textContent() { return this._text + this.children.map(c => c.textContent).join(''); }
  set innerHTML(_) { throw new Error('innerHTML used'); }
  closest(sel) {
    const classes = sel.split(',').map(s => s.trim().replace(/^\./, ''));
    for (let el = this; el; el = el.parentNode) {
      const own = (el.className || '').split(/\s+/);
      if (classes.some(c => own.includes(c))) return el;
    }
    return null;
  }
  all() { return [this].concat(...this.children.map(c => c.all())); }
  byClass(c) { return this.all().filter(e => (e.className || '').split(/\s+/).includes(c)); }
}
function makeDocument() {
  return { createElement: (t) => new El(t) };
}

// ── Leaflet stub ───────────────────────────────────────────────────────────
function makeMap({ zoom = 13, overlayOn = true } = {}) {
  const h = {};
  const popups = [];
  const heritageLayer = { id: 'heritage' };
  const container = new El('div'); container.className = 'leaflet-container';
  const map = {
    _zoom: zoom,
    _layers: new Set(overlayOn ? [heritageLayer] : []),
    _mcHeritageLayer: heritageLayer,
    on(types, fn) { types.split(' ').forEach(t => (h[t] = h[t] || []).push(fn)); return map; },
    off(types, fn) { types.split(' ').forEach(t => { if (h[t]) h[t] = h[t].filter(f => f !== fn); }); return map; },
    fire(t, e) { (h[t] || []).slice().forEach(fn => fn(Object.assign({ type: t }, e || {}))); return map; },
    listens(t) { return !!(h[t] && h[t].length); },
    getZoom() { return map._zoom; },
    hasLayer(l) { return map._layers.has(l); },
    getContainer() { return container; },
    closePopup(p) { if (p) p._open = false; },
    popups, container, heritageLayer,
  };
  return map;
}
function makeL(map) {
  return {
    popup(opts) {
      const p = {
        options: opts, _open: false, _latlng: null, _content: null, _h: {},
        setLatLng(ll) { p._latlng = ll; return p; },
        setContent(c) { p._content = c; return p; },
        openOn(m) { p._open = true; m.popups.push(p); return p; },
        isOpen() { return p._open; },
        on(t, fn) { (p._h[t] = p._h[t] || []).push(fn); return p; },
        off() { return p; },
        fire(t) { (p._h[t] || []).forEach(fn => fn({ type: t })); },
        getElement() { return null; },
      };
      return p;
    },
    DomEvent: { stopPropagation() {} },
  };
}

function loadModule(extra) {
  const ctx = vm.createContext(Object.assign({
    console, document: makeDocument(), URLSearchParams, Promise, setTimeout, clearTimeout,
    AbortController: class { constructor() { this.signal = { aborted: false }; } abort() { this.signal.aborted = true; } },
  }, extra || {}));
  ctx.window = ctx;
  vm.runInContext(SRC, ctx, { filename: 'heritage-info.js' });
  assert(ctx.HeritageInfo, 'window.HeritageInfo not exported');
  return ctx;
}

// A fetch stub whose answers the test resolves by hand.
function makeFetch() {
  const calls = [];
  const fn = (url, init) => new Promise((resolve, reject) => { calls.push({ url, init, resolve, reject }); });
  fn.calls = calls;
  return fn;
}
function answer(call, status, body, headers) {
  call.resolve({ ok: status >= 200 && status < 300, status,
    headers: { get: (k) => (headers || {})[k.toLowerCase()] || null },
    json: () => Promise.resolve(body) });
}
const tick = () => new Promise(r => setTimeout(r, 0));

const CFG = { enabled: true, minZoom: 12 };
const INSIDE = { radiusM: 100, count: 1, truncated: false, attribution: '© Slots- og Kulturstyrelsen',
  nearest: { type: 'Rundhøj', dating: 'Oldtid', status: 'Fredet', protectionNumber: '3624:15', systemNumber: '190001', distanceM: 0, insideZone: true } };

function setup(opts) {
  opts = opts || {};
  const map = makeMap(opts);
  const fetch = makeFetch();
  const ctx = loadModule({ L: makeL(map) });
  const tile = new El('img'); tile.className = 'leaflet-tile';
  map.container.appendChild(tile);
  const ctl = ctx.HeritageInfo.attach(map, {
    config: () => (opts.cfg === undefined ? CFG : opts.cfg),
    isBusy: opts.isBusy || (() => false),
    fetch,
    document: ctx.document,
  });
  function click(target, extra) {
    const originalEvent = Object.assign({ target: target || tile }, extra || {});
    map.fire('preclick', { latlng: { lat: 55.7563, lng: 9.4201 }, originalEvent });
    map.fire('click', { latlng: { lat: 55.7563, lng: 9.4201 }, originalEvent });
  }
  return { map, fetch, ctx, ctl, click, tile };
}

// ── Precedence ─────────────────────────────────────────────────────────────

test('no request with the overlay off', () => {
  const s = setup({ overlayOn: false });
  s.click();
  assert.strictEqual(s.fetch.calls.length, 0);
  assert.strictEqual(s.map.popups.length, 0);
});

test('no request below the overlay minZoom (z11), one at z12', () => {
  const s = setup({ zoom: 11 });
  s.click();
  assert.strictEqual(s.fetch.calls.length, 0);
  s.map._zoom = 12;
  s.click();
  assert.strictEqual(s.fetch.calls.length, 1);
});

test('no request outside the overlay bounds', () => {
  const s = setup({ cfg: { enabled: true, minZoom: 12, bounds: [[54.4646, 8.0899], [57.7289, 15.5689]] } });
  s.map.fire('preclick', { latlng: { lat: 52.52, lng: 13.40 }, originalEvent: { target: s.tile } });
  s.map.fire('click', { latlng: { lat: 52.52, lng: 13.40 }, originalEvent: { target: s.tile } });
  assert.strictEqual(s.fetch.calls.length, 0, 'request for Berlin');
  s.click();
  assert.strictEqual(s.fetch.calls.length, 1, 'no request for Jelling');
});

test('no request without a heritage config block', () => {
  const s = setup({ cfg: null });
  s.click();
  assert.strictEqual(s.fetch.calls.length, 0);
});

test('profile picking / dragging wins: no request while the host is busy', () => {
  let busy = true;
  const s = setup({ isBusy: () => busy });
  s.click();
  assert.strictEqual(s.fetch.calls.length, 0);
  busy = false;
  s.click();
  assert.strictEqual(s.fetch.calls.length, 1);
});

test('profile pick that disarms during the same click still wins (busy at preclick)', () => {
  let busy = true;
  const s = setup({ isBusy: () => busy });
  // Re-attach behind the host's own click handler, the worst order for
  // us: the profile's handler runs first and disarms the picker.
  s.ctl.detach();
  s.map.on('click', () => { busy = false; });
  s.ctx.HeritageInfo.attach(s.map, { config: () => CFG, isBusy: () => busy, fetch: s.fetch, document: s.ctx.document });
  s.click();
  assert.strictEqual(s.fetch.calls.length, 0, 'popup opened for the profile pick');
});

test('a marker, a path, a popup or a control wins', () => {
  const s = setup();
  for (const cls of ['leaflet-marker-icon leaflet-interactive', 'leaflet-interactive', 'leaflet-popup mc-heritage-popup', 'leaflet-control-layers leaflet-control']) {
    const host = new El('div'); host.className = cls;
    const inner = new El('span'); host.appendChild(inner);
    s.click(inner);
  }
  assert.strictEqual(s.fetch.calls.length, 0);
  s.click(s.tile, { _stopped: true });
  assert.strictEqual(s.fetch.calls.length, 0, 'a click a layer stopped');
});

// ── Request ────────────────────────────────────────────────────────────────

test('one request per click: /api/terrain/heritage with lat and lon only', () => {
  const s = setup();
  s.click();
  assert.strictEqual(s.fetch.calls.length, 1);
  const u = new URL(s.fetch.calls[0].url, 'http://x');
  assert.strictEqual(u.pathname, '/api/terrain/heritage');
  assert.deepStrictEqual(Array.from(u.searchParams.keys()).sort(), ['lat', 'lon']);
  assert.strictEqual(u.searchParams.get('lat'), '55.756300');
  assert.strictEqual(u.searchParams.get('lon'), '9.420100');
  assert(s.fetch.calls[0].init && s.fetch.calls[0].init.signal, 'no abort signal');
  assert.strictEqual(s.map.popups.length, 1);
  assert(/Looking up/.test(s.map.popups[0]._content.textContent), 'no loading state');
});

test('a new click aborts the previous request and a stale answer is never shown', async () => {
  const s = setup();
  s.click();
  s.click();
  assert.strictEqual(s.fetch.calls.length, 2);
  assert(s.fetch.calls[0].init.signal.aborted, 'first request not aborted');
  assert.strictEqual(s.map.popups.length, 1, 'one popup, reused');
  answer(s.fetch.calls[1], 200, INSIDE);
  await tick(); await tick();
  const second = s.map.popups[0]._content.textContent;
  answer(s.fetch.calls[0], 200, Object.assign({}, INSIDE, { nearest: Object.assign({}, INSIDE.nearest, { type: 'STALE' }) }));
  await tick(); await tick();
  assert(!/STALE/.test(s.map.popups[0]._content.textContent), 'stale answer rendered');
  assert(/Rundhøj/.test(second));
});

test('a body that arrives after a newer click is not shown either', async () => {
  const s = setup();
  s.click();
  let release;
  const late = new Promise(r => { release = r; });
  s.fetch.calls[0].resolve({ ok: true, status: 200, headers: { get: () => null }, json: () => late });
  await tick();
  s.click();
  release(Object.assign({}, INSIDE, { nearest: Object.assign({}, INSIDE.nearest, { type: 'STALE' }) }));
  await tick(); await tick();
  assert(!/STALE/.test(s.map.popups[0]._content.textContent), 'stale body rendered');
  assert(/Looking up/.test(s.map.popups[0]._content.textContent));
});

test('the overlay going off closes the popup and aborts the request', () => {
  const s = setup();
  s.click();
  s.map._layers.clear();
  s.map.fire('overlayremove', { layer: s.map.heritageLayer });
  assert(s.fetch.calls[0].init.signal.aborted, 'request not aborted');
  assert.strictEqual(s.map.popups[0]._open, false, 'popup left open');
});

test('detach removes the listeners', () => {
  const s = setup();
  s.ctl.detach();
  s.click();
  assert.strictEqual(s.fetch.calls.length, 0);
  assert(!s.map.listens('click') && !s.map.listens('preclick'));
});

// ── Popup ──────────────────────────────────────────────────────────────────

async function rendered(status, body, headers) {
  const s = setup();
  s.click();
  answer(s.fetch.calls[0], status, body, headers);
  await tick(); await tick();
  return s.map.popups[0]._content;
}

test('inside a zone: the dataset fields and a neutral inside line', async () => {
  const el = await rendered(200, INSIDE);
  const t = el.textContent;
  for (const s of ['Rundhøj', 'Oldtid', 'Fredet', '3624:15', '190001', 'Inside protection zone', 'Yes', '© Slots- og Kulturstyrelsen']) {
    assert(t.includes(s), 'missing ' + s + ' in ' + t);
  }
  assert(!/building|unlikely|forbidden|permission|warning/i.test(t), 'advisory text belongs to M3: ' + t);
  assert(!/Distance/.test(t), 'distance shown while inside');
  assert.strictEqual(el.all().filter(e => e.tagName === 'A').length, 0, 'no link may be rendered');
});

test('nearby: distance to the zone and "No"', async () => {
  const el = await rendered(200, Object.assign({}, INSIDE, { nearest: Object.assign({}, INSIDE.nearest, { insideZone: false, distanceM: 40, protectionNumber: undefined }) }));
  const t = el.textContent;
  assert(t.includes('No') && t.includes('40 m'), t);
  assert(!t.includes('Protection no.'), 'empty field shown: ' + t);
});

test('nothing within the radius', async () => {
  const t = (await rendered(200, { radiusM: 100, count: 0, truncated: false, nearest: null, attribution: 'x' })).textContent;
  assert(/No protection zone within 100 m/.test(t), t);
});

test('a missing type falls back to a neutral title', async () => {
  const t = (await rendered(200, Object.assign({}, INSIDE, { nearest: Object.assign({}, INSIDE.nearest, { type: '' }) }))).textContent;
  assert(t.includes('Protection zone'), t);
});

test('truncated answers say the list may be incomplete', async () => {
  const t = (await rendered(200, Object.assign({}, INSIDE, { truncated: true }))).textContent;
  assert(/may be incomplete/.test(t), t);
});

test('429 says when to try again; other failures stay vague', async () => {
  let t = (await rendered(429, { error: 'rate limit exceeded' }, { 'retry-after': '7' })).textContent;
  assert(/try again in 7 s/.test(t), t);
  t = (await rendered(502, { error: 'heritage data unavailable' })).textContent;
  assert(/unavailable/.test(t) && !/heritage data/.test(t), t);
  t = (await rendered(200, 'not an object')).textContent;
  assert(/unavailable/.test(t), t);
});

test('dataset text is never parsed as HTML (XSS)', async () => {
  const evil = '<img src=x onerror="window.__xss=1">';
  const el = await rendered(200, { radiusM: 100, count: 1, truncated: false, attribution: evil,
    nearest: { type: evil, dating: evil, status: evil, protectionNumber: evil, systemNumber: evil, distanceM: 3, insideZone: false } });
  assert(el.all().every(e => e.tagName !== 'IMG'), 'an <img> element was created');
  assert(el.textContent.split(evil).length - 1 >= 5, 'payload not shown as text');
  // The module builds the popup without innerHTML at all: the stub throws on it.
  assert(!/innerHTML|insertAdjacentHTML|bindPopup\(|setContent\(['"`]/.test(SRC), 'HTML sink in heritage-info.js');
});

test('distance is a whole number; a non-numeric one is not shown', async () => {
  let t = (await rendered(200, Object.assign({}, INSIDE, { nearest: Object.assign({}, INSIDE.nearest, { insideZone: false, distanceM: 'x<b>' }) }))).textContent;
  assert(!t.includes('x<b>') && !/Distance/.test(t), t);
});

// ── Wiring and CSS ─────────────────────────────────────────────────────────

test('Map and Live attach it; index.html loads it before map.js and live.js', () => {
  const html = fs.readFileSync(path.join(__dirname, 'public', 'index.html'), 'utf8');
  const at = (f) => html.indexOf('src="' + f + '?v=__BUST__"');
  assert(at('heritage-info.js') > 0, 'not loaded');
  assert(at('heritage-info.js') < at('map.js') && at('heritage-info.js') < at('live.js'), 'loaded after its users');
  const mapJs = fs.readFileSync(path.join(__dirname, 'public', 'map.js'), 'utf8');
  const liveJs = fs.readFileSync(path.join(__dirname, 'public', 'live.js'), 'utf8');
  assert(/HeritageInfo\.attach\(map, \{[^}]*isBusy: function \(\) \{ return !!profileArm \|\| !!profileDrag; \}/.test(mapJs), 'Map does not pass the profile picker as busy');
  assert(/HeritageInfo\.attach\(map/.test(liveJs), 'Live does not attach');
});

test('popup CSS: CSS variables only, 44px close button on touch screens', () => {
  const css = fs.readFileSync(path.join(__dirname, 'public', 'style.css'), 'utf8');
  const start = css.indexOf('/* #723 M2 heritage popup */');
  const end = css.indexOf('/* end #723 M2 heritage popup */');
  assert(start >= 0 && end > start, 'popup CSS block not found');
  const block = css.slice(start, end).replace(/\/\*[\s\S]*?\*\//g, '');
  assert(!/#[0-9a-fA-F]{3,8}\b|rgba?\(|hsla?\(/.test(block), 'hard-coded colour in the popup CSS');
  assert(/@media \(pointer: coarse\)[\s\S]*\.leaflet-container \.mc-heritage-popup a\.leaflet-popup-close-button[\s\S]*width: 44px[\s\S]*height: 44px/.test(block), 'no 44px touch close button');
});

(async () => {
  for (const [name, fn] of pending) {
    try { await fn(); passed++; console.log('  ✅ ' + name); }
    catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
  }
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
