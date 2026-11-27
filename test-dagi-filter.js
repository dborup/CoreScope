/**
 * Unit tests for public/dagi-filter.js (#740 M3a), the Danish areas filter.
 *
 * The real file (and the real url-state.js) runs in a vm sandbox with a
 * small DOM below: elements with bubbling events, a document whose capture
 * listeners run first, a location/history pair and a fake clock for the
 * search debounce. fetch answers GET /api/config/dagi in the server's shape
 * (M2, curled from a local server; synthetic areas here).
 */
'use strict';
const vm = require('vm');
const fs = require('fs');
const assert = require('assert');

let passed = 0, failed = 0;
const queue = [];
function test(name, fn) { queue.push([name, fn]); }
// Values from the vm realm compared as plain data.
const plain = (v) => (v === undefined ? v : JSON.parse(JSON.stringify(v)));
const same = (a, b, m) => assert.deepStrictEqual(plain(a), plain(b), m);
// Element identity with a short message. A failing assert.strictEqual on two
// fake-DOM elements renders both whole object graphs (the document, every
// listener) and can exhaust memory; this names them instead.
const desc = (n) => (n && n.tagName ? n.tagName.toLowerCase() + (n.className ? '.' + n.className.split(' ').join('.') : '') + ' "' + String(n.textContent).slice(0, 40) + '"' : String(n));
function sameEl(a, b, m) { if (a !== b) throw new assert.AssertionError({ message: (m ? m + ': ' : '') + desc(a) + ' is not ' + desc(b) }); }

/* ---------- a small DOM ---------- */

class Node {
  constructor(doc, tag) {
    this.ownerDocument = doc; this.tagName = String(tag).toUpperCase();
    this.children = []; this.parentNode = null; this.attrs = {}; this.className = '';
    this.hidden = false; this.style = {}; this._text = ''; this.listeners = [];
    this.type = ''; this.value = ''; this.href = ''; this.target = ''; this.rel = ''; this.tabIndex = 0;
  }
  get classList() {
    const self = this;
    const list = () => self.className.split(/\s+/).filter(Boolean);
    return {
      contains: (c) => list().includes(c),
      add: (c) => { if (!list().includes(c)) self.className = list().concat(c).join(' '); },
      remove: (c) => { self.className = list().filter((x) => x !== c).join(' '); },
    };
  }
  get textContent() { return this._text + this.children.map((c) => c.textContent).join(''); }
  set textContent(v) { this.children.forEach((c) => { c.parentNode = null; }); this.children = []; this._text = String(v); }
  get firstChild() { return this.children[0] || null; }
  appendChild(c) { if (c.parentNode) c.parentNode.removeChild(c); c.parentNode = this; this.children.push(c); return c; }
  insertBefore(c, ref) {
    if (c.parentNode) c.parentNode.removeChild(c);
    const i = ref ? this.children.indexOf(ref) : -1;
    c.parentNode = this;
    if (i < 0) this.children.push(c); else this.children.splice(i, 0, c);
    return c;
  }
  removeChild(c) { const i = this.children.indexOf(c); if (i !== -1) this.children.splice(i, 1); c.parentNode = null; return c; }
  contains(o) { for (let n = o; n; n = n.parentNode) if (n === this) return true; return false; }
  setAttribute(k, v) { this.attrs[k] = String(v); }
  getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; }
  addEventListener(t, fn, capture) { this.listeners.push({ t, fn, capture: !!capture }); }
  removeEventListener(t, fn, capture) {
    const i = this.listeners.findIndex((l) => l.t === t && l.fn === fn && l.capture === !!capture);
    if (i !== -1) this.listeners.splice(i, 1);
  }
  count(t) { return this.listeners.filter((l) => !t || l.t === t).length; }
  focus() { this.ownerDocument.activeElement = this; }
  scrollIntoView(o) { this.ownerDocument.scrolled.push([this, o && o.block]); }
  getBoundingClientRect() { return { left: 0, top: 0, right: 0, bottom: 0, width: 0, height: 0 }; }
  all() { return [this].concat(...this.children.map((c) => c.all())); }
  find(cls) { return this.all().filter((e) => e.classList.contains(cls)); }
  /** Shown: neither it nor an ancestor is hidden. */
  get shown() { for (let n = this; n; n = n.parentNode) if (n.hidden) return false; return true; }
}

function makeDom() {
  const doc = new Node(null, '#document');
  doc.ownerDocument = doc;
  doc.createElement = (t) => new Node(doc, t);
  doc.createElementNS = (ns, t) => Object.assign(new Node(doc, t), { namespaceURI: ns });
  doc.body = doc.appendChild(new Node(doc, 'body'));
  doc.documentElement = { clientWidth: 1280 };
  doc.activeElement = doc.body;
  doc.scrolled = [];
  const win = new Node(doc, '#window');
  // dispatch: document capture listeners, then the target and its
  // ancestors, then document bubble listeners; stopPropagation stops it.
  function dispatch(target, type, props) {
    const e = Object.assign({ type, target, stopped: false, defaultPrevented: false,
      stopPropagation() { this.stopped = true; }, preventDefault() { this.defaultPrevented = true; } }, props || {});
    const run = (n, capture) => n.listeners.filter((l) => l.t === type && l.capture === capture).forEach((l) => { if (!e.stopped) l.fn(e); });
    run(doc, true);
    for (let n = target; n && n !== doc && !e.stopped; n = n.parentNode) run(n, false);
    if (!e.stopped) run(doc, false);
    return e;
  }
  return { doc, win, dispatch };
}

function makeClock() {
  let seq = 0; const pending = new Map();
  return {
    setTimeout: (fn, ms) => { const id = ++seq; pending.set(id, fn); return id; },
    clearTimeout: (id) => { pending.delete(id); },
    flush() { const fns = Array.from(pending.values()); pending.clear(); fns.forEach((f) => f()); },
    get size() { return pending.size; },
  };
}

/* ---------- the sandbox ---------- */

const flush = () => new Promise((r) => setImmediate(r));

// A config in M2's shape: kommune (with Christiansø), landsdel, region and
// postnummer with one street-postcode group of 232 synthetic members.
function groupCodes() {
  const out = ['1050'];
  for (let c = 1051; out.length < 231; c++) out.push(String(c));
  out.push('1473');
  return out;
}
function makeConfig(over) {
  return Object.assign({
    enabled: true,
    credit: 'Indeholder data fra Klimadatastyrelsen, DAGI, november 2026, CC BY 4.0 — data bearbejdet',
    source: { licenseUrl: 'https://creativecommons.org/licenses/by/4.0/', termsUrl: 'https://dataforsyningen.dk/Vilkaar' },
    layers: [
      { layer: 'kommune', param: 'kommune', areas: [
        { code: '0101', name: 'København' }, { code: '0400', name: 'Bornholm' },
        { code: '0411', name: 'Christiansø', outsideKommune: true }, { code: '0492', name: 'Ærø' },
        { code: '0751', name: 'Aarhus' }, { code: '0860', name: 'Hjørring' }, { code: '0390', name: 'Vordingborg' },
      ] },
      { layer: 'landsdel', param: 'landsdel', areas: [{ code: '218546', name: 'Østjylland' }, { code: '218544', name: 'Vest- og Sydsjælland' }] },
      { layer: 'region', param: 'dkregion', areas: [{ code: '1082', name: 'Region Midtjylland' }, { code: '1084', name: 'Region Hovedstaden' }] },
      { layer: 'postnummer', param: 'postnr', areas: [
        { code: '1050', name: 'København K', codes: groupCodes() },
        { code: '2100', name: 'København Ø' }, { code: '8000', name: 'Aarhus C' }, { code: '8200', name: 'Aarhus N' },
        { code: '6960', name: 'Hvide Sande' },
      ] },
    ],
  }, over || {});
}

function load(opts) {
  opts = opts || {};
  const { doc, win, dispatch } = makeDom();
  const clock = makeClock();
  const storage = Object.create(null);
  if (opts.stored !== undefined) storage['meshcore-dagi-filter'] = typeof opts.stored === 'string' ? opts.stored : JSON.stringify(opts.stored);
  const localStorage = {
    getItem: (k) => (k in storage ? storage[k] : null),
    setItem: (k, v) => { storage[k] = String(v); },
    removeItem: (k) => { delete storage[k]; },
  };
  const requests = [];
  let response = opts.response === undefined ? makeConfig() : opts.response;
  let release = null;
  const fetch = (url) => {
    requests.push(url);
    if (opts.hold) return new Promise((r) => { release = () => r({ ok: true, json: () => Promise.resolve(response) }); });
    if (response instanceof Error) return Promise.reject(response);
    if (typeof response === 'number') return Promise.resolve({ ok: false, status: response, json: () => Promise.resolve({}) });
    return Promise.resolve({ ok: true, json: () => Promise.resolve(response) });
  };
  const location = { hash: opts.hash || '' };
  const replaced = [];
  const history = { state: opts.historyState || null, replaceState(st, _t, url) { this.state = st; location.hash = url; replaced.push(url); } };
  const media = { sheet: !!opts.sheet, touch: !!opts.touch };
  const placed = [];
  Object.assign(win, {
    location, history,
    matchMedia: (q) => ({ matches: /max-width/.test(q) ? media.sheet : /coarse/.test(q) ? media.touch : false }),
    RegionFilter: { placeMenu: (m) => placed.push(m) },
  });
  const ctx = {
    window: win, document: doc, localStorage, fetch, console, Promise, JSON, String, Number, Array, Object,
    Intl, encodeURIComponent, decodeURIComponent, URLSearchParams, setTimeout: clock.setTimeout, clearTimeout: clock.clearTimeout,
  };
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync('public/url-state.js', 'utf8'), ctx, { filename: 'public/url-state.js' });
  win.URLState = ctx.URLState || win.URLState;
  vm.runInContext(fs.readFileSync('public/dagi-filter.js', 'utf8'), ctx, { filename: 'public/dagi-filter.js' });
  return { DF: win.DagiFilter, doc, win, dispatch, clock, storage, requests, location, replaced, media, placed,
    setResponse: (r) => { response = r; }, release: () => release && release() };
}

async function mount(s) {
  const c = s.doc.body.appendChild(s.doc.createElement('div'));
  await s.DF.init(c);
  await flush();
  return c;
}
function one(root, cls) { const l = root.find(cls); assert.ok(l.length, 'no .' + cls); return l[0]; }
function shownRows(c) { return c.find('dagi-option').filter((r) => r.shown).map((r) => one(r, 'dagi-option-label').textContent); }
function rowByLabel(c, label) {
  const r = c.find('dagi-option').find((x) => one(x, 'dagi-option-label').textContent === label);
  assert.ok(r, 'no row ' + label);
  return r;
}
function type(s, c, text) {
  const input = one(c, 'dagi-search');
  input.value = text;
  s.dispatch(input, 'input');
  s.clock.flush();
}
function key(s, target, k) { return s.dispatch(target, 'keydown', { key: k }); }

/* ---------- state, links and storage ---------- */

test('no selection: "Denmark: All", no params, and ready() resolves without a request', async () => {
  const s = load();
  assert.strictEqual(s.DF.getSelected(), null);
  assert.strictEqual(s.DF.hashParam(), '');
  assert.strictEqual(s.DF.queryString(), '');
  assert.strictEqual(s.DF.triggerLabel(), 'Denmark: All');
  await s.DF.ready();
  same(s.requests, []);
});

test('a link param wins over the saved choice and is saved like a pick', async () => {
  const s = load({ stored: { param: 'kommune', code: '0860' } });
  assert.strictEqual(s.DF.syncFromUrl('#/nodes?kommune=0751'), true);
  same(s.DF.getSelected(), { param: 'kommune', code: '0751' });
  same(JSON.parse(s.storage['meshcore-dagi-filter']), { param: 'kommune', code: '0751' });
});

test('no DAGI param in the link keeps the saved choice', async () => {
  const s = load({ stored: { param: 'dkregion', code: '1082' } });
  assert.strictEqual(s.DF.syncFromUrl('#/nodes?tab=repeater&region=AAR'), false);
  same(s.DF.getSelected(), { param: 'dkregion', code: '1082' });
  assert.strictEqual(s.DF.hashParam(), 'dkregion=1082');
});

test('an empty DAGI param in the link keeps the saved choice', async () => {
  const s = load({ stored: { param: 'kommune', code: '0751' } });
  assert.strictEqual(s.DF.syncFromUrl('#/nodes?kommune='), false);
  same(s.DF.getSelected(), { param: 'kommune', code: '0751' });
});

test('a saved value that is not a {param, code} pair is ignored', async () => {
  for (const stored of ['not json', { param: 'area', code: 'X' }, { param: 'kommune', code: '' }, { param: 'kommune', code: 751 }]) {
    const s = load({ stored });
    assert.strictEqual(s.DF.getSelected(), null, JSON.stringify(stored));
  }
});

test('several DAGI params: the most specific wins and the others leave the URL', async () => {
  const s = load({ hash: '#/nodes?dkregion=1082&kommune=0751&tab=repeater&landsdel=218546&postnr=8000' });
  s.DF.syncFromUrl(s.location.hash);
  same(s.DF.getSelected(), { param: 'postnr', code: '8000' });
  assert.strictEqual(s.location.hash, '#/nodes?tab=repeater&postnr=8000');
  const t = load({ hash: '#/nodes?dkregion=1082&landsdel=218546' });
  t.DF.syncFromUrl(t.location.hash);
  same(t.DF.getSelected(), { param: 'landsdel', code: '218546' });
  const u = load({ hash: '#/nodes?dkregion=1082&kommune=0751' });
  u.DF.syncFromUrl(u.location.hash);
  same(u.DF.getSelected(), { param: 'kommune', code: '0751' });
  assert.strictEqual(u.location.hash, '#/nodes?kommune=0751');
});

test('a repeated param keeps its first value', async () => {
  const s = load();
  s.DF.syncFromUrl('#/nodes?kommune=0751&kommune=0860');
  same(s.DF.getSelected(), { param: 'kommune', code: '0751' });
});

test('ready() gates the query string until the config has checked the code', async () => {
  const s = load({ hash: '#/nodes?kommune=0751', hold: true });
  s.DF.syncFromUrl(s.location.hash);
  assert.strictEqual(s.DF.queryString(), '', 'sent before the check');
  assert.strictEqual(s.DF.hashParam(), 'kommune=0751', 'a pending link must survive a URL rewrite');
  let done = false;
  const p = s.DF.ready().then(() => { done = true; });
  await flush();
  assert.strictEqual(done, false, 'ready() resolved before the config loaded');
  s.release();
  await p;
  assert.strictEqual(s.DF.queryString(), '&kommune=0751');
  same(s.requests, ['/api/config/dagi']);
});

test('an unknown code is dropped from the selection, the storage and the URL, and never sent', async () => {
  const s = load({ hash: '#/nodes?tab=room&kommune=9999' });
  let heard = 0;
  s.DF.onChange(() => heard++);
  s.DF.syncFromUrl(s.location.hash);
  heard = 0;
  await s.DF.ready();
  assert.strictEqual(s.DF.getSelected(), null);
  assert.strictEqual(s.DF.queryString(), '');
  assert.ok(!('meshcore-dagi-filter' in s.storage), 'unknown code still saved');
  assert.strictEqual(s.location.hash, '#/nodes?tab=room');
  assert.strictEqual(heard, 0, 'nothing was sent with it, so nobody is told');
});

test('a code in the wrong layer is unknown', async () => {
  const s = load({ stored: { param: 'dkregion', code: '0751' } });
  await s.DF.ready();
  assert.strictEqual(s.DF.getSelected(), null);
});

test('a saved code is checked too (an old landsdel id after a rebuild)', async () => {
  const s = load({ stored: { param: 'landsdel', code: '111111' } });
  await s.DF.ready();
  assert.strictEqual(s.DF.getSelected(), null);
  assert.ok(!('meshcore-dagi-filter' in s.storage));
});

test('a link after the config has loaded is checked at once', async () => {
  const s = load();
  await mount(s);
  assert.strictEqual(s.DF.syncFromUrl('#/nodes?kommune=9999'), false);
  assert.strictEqual(s.DF.getSelected(), null);
  assert.strictEqual(s.DF.syncFromUrl('#/nodes?kommune=0751'), true);
  assert.strictEqual(s.DF.queryString(), '&kommune=0751');
});

test('a link that replaces a known choice with an unknown one tells the listeners', async () => {
  const s = load({ stored: { param: 'kommune', code: '0751' } });
  await mount(s);
  const heard = [];
  s.DF.onChange((v) => heard.push(v));
  s.DF.syncFromUrl('#/nodes?kommune=9999');
  same(heard, [null]);
});

test('a street-postcode member selects its group and keeps its code', async () => {
  const s = load({ hash: '#/nodes?postnr=1051' });
  s.DF.syncFromUrl(s.location.hash);
  await s.DF.ready();
  assert.strictEqual(s.DF.queryString(), '&postnr=1051');
  assert.strictEqual(s.DF.triggerLabel(), 'Postnr: 1050–1473 København K');
});

test('DAGI off (enabled:false): the saved choice is dropped and the control hidden', async () => {
  const s = load({ stored: { param: 'kommune', code: '0751' }, response: { enabled: false, layers: [] } });
  const c = await mount(s);
  assert.strictEqual(s.DF.getSelected(), null);
  assert.strictEqual(c.style.display, 'none');
  assert.strictEqual(c.children.length, 0);
});

test('enabled:false is off even if layers came along', async () => {
  const s = load({ stored: { param: 'kommune', code: '0751' }, response: makeConfig({ enabled: false }) });
  const c = await mount(s);
  assert.strictEqual(s.DF.getSelected(), null);
  assert.strictEqual(c.style.display, 'none');
});

test('a failed config request keeps the saved choice unsent, hides the control, and is retried', async () => {
  const s = load({ stored: { param: 'kommune', code: '0751' }, response: new Error('offline') });
  const c = await mount(s);
  same(s.DF.getSelected(), { param: 'kommune', code: '0751' });
  assert.strictEqual(s.DF.queryString(), '');
  assert.strictEqual(c.style.display, 'none');
  s.setResponse(makeConfig());
  await s.DF.ready();
  assert.strictEqual(s.DF.queryString(), '&kommune=0751');
  assert.strictEqual(s.requests.length, 2);
});

test('an HTTP error is a failed request, not DAGI off', async () => {
  const s = load({ stored: { param: 'kommune', code: '0751' }, response: 503 });
  await s.DF.ready();
  same(s.DF.getSelected(), { param: 'kommune', code: '0751' });
  assert.strictEqual(s.DF.queryString(), '');
});

test('the config is fetched once for init() and ready() together', async () => {
  const s = load({ stored: { param: 'kommune', code: '0751' } });
  const c = s.doc.body.appendChild(s.doc.createElement('div'));
  await Promise.all([s.DF.init(c), s.DF.ready(), s.DF.ready()]);
  same(s.requests, ['/api/config/dagi']);
});

/* ---------- search ---------- */

test('normalize(): case, æ/ø/å, other accents and punctuation', () => {
  const s = load();
  assert.strictEqual(s.DF.normalize('Ærø'), 'aeroe');
  assert.strictEqual(s.DF.normalize('ÅRHUS'), 'aarhus');
  assert.strictEqual(s.DF.normalize('Vest- og Sydsjælland'), 'vest og sydsjaelland');
  assert.strictEqual(s.DF.normalize('Tønder Ø'), 'toender oe');
  assert.strictEqual(s.DF.normalize('Café'), 'cafe');
  assert.strictEqual(s.DF.normalize(null), '');
});

async function loaded(opts) { const s = load(opts); await s.DF.init(s.doc.body.appendChild(s.doc.createElement('div'))); return s; }
const labels = (res, layer) => { const l = res.find((x) => x.layer === layer); return l ? l.hits.map((h) => h.label) : []; };

test('search: a word start in any layer, æ/aa-insensitive', async () => {
  const s = await loaded();
  const r = s.DF.search('aar');
  same(labels(r, 'kommune'), ['Aarhus']);
  same(labels(r, 'postnummer'), ['8000 Aarhus C', '8200 Aarhus N']);
  same(labels(s.DF.search('århus'), 'kommune'), ['Aarhus']);
  same(labels(s.DF.search('aero'), 'kommune'), ['Ærø']);
  same(labels(s.DF.search('ÆRØ'), 'kommune'), ['Ærø']);
  same(labels(s.DF.search('sydsj'), 'landsdel'), ['Vest- og Sydsjælland'], 'a later word');
  same(labels(s.DF.search('rhus'), 'kommune'), [], 'not a word start');
  same(s.DF.search('   '), []);
});

test('search: a code prefix, and a member code finds its group with "includes"', async () => {
  const s = await loaded();
  same(labels(s.DF.search('800'), 'postnummer'), ['8000 Aarhus C']);
  const g = s.DF.search('1051').find((x) => x.layer === 'postnummer').hits;
  same(g, [{ code: '1050', label: '1050–1473 København K', note: 'includes 1051' }]);
  const p = s.DF.search('105').find((x) => x.layer === 'postnummer').hits;
  same(p.map((h) => h.note), [''], 'a prefix is no "includes"');
  same(s.DF.search('1050').find((x) => x.layer === 'postnummer').hits[0].note, '', 'the group code itself');
  same(labels(s.DF.search('0751'), 'kommune'), ['Aarhus']);
  same(labels(s.DF.search('99'), 'postnummer'), []);
});

test('search: at most 30 hits per layer, with the rest counted', async () => {
  const areas = [];
  for (let i = 0; i < 45; i++) areas.push({ code: String(5000 + i), name: 'Odense ' + i });
  const s = await loaded({ response: makeConfig({ layers: [{ layer: 'postnummer', param: 'postnr', areas }] }) });
  const r = s.DF.search('odense');
  assert.strictEqual(r[0].hits.length, 30);
  assert.strictEqual(r[0].more, 15);
});

test('group labels: min–max and the member count, from the codes', async () => {
  const s = await loaded({ response: makeConfig({ layers: [{ layer: 'postnummer', param: 'postnr', areas: [
    { code: '1800', name: 'Frederiksberg C', codes: ['1974', '1800', '1801'] }, { code: '1799', name: 'København V' }] }] }) });
  const c = await mount(s);
  s.dispatch(one(c, 'dagi-trigger'), 'click');
  type(s, c, '18');
  const row = rowByLabel(c, '1800–1974 Frederiksberg C');
  assert.strictEqual(one(row, 'dagi-option-meta').textContent, '3 street postcodes');
  type(s, c, '1799');
  same(shownRows(c), ['1799 København V'], 'a regular postcode is no group');
});

test('Christiansø is listed as outside the kommune division', async () => {
  const s = await loaded();
  same(labels(s.DF.search('christ'), 'kommune'), ['Christiansø (outside the kommune division)']);
  s.DF.syncFromUrl('#/nodes?kommune=0411');
  assert.strictEqual(s.DF.triggerLabel(), 'Kommune: Christiansø (outside the kommune division)');
});

test('sorting: kommuner by Danish collation, postcodes by code', async () => {
  const s = load();
  const c = await mount(s);
  s.dispatch(one(c, 'dagi-trigger'), 'click');
  same(shownRows(c), ['All of Denmark', 'Bornholm', 'Christiansø (outside the kommune division)',
    'Hjørring', 'København', 'Vordingborg', 'Ærø', 'Aarhus'], 'Danish: Æ, Ø, then Å and Aa');
  s.dispatch(c.find('dagi-tab').find((t) => t.textContent.indexOf('Postnr') === 0), 'click');
  same(shownRows(c).slice(1), ['1050–1473 København K', '2100 København Ø', '6960 Hvide Sande', '8000 Aarhus C', '8200 Aarhus N']);
  same(c.find('dagi-subhead').filter((h) => h.shown).map((h) => h.textContent), ['1000–1999', '2000–2999', '6000–6999', '8000–8999']);
});

/* ---------- the control ---------- */

test('trigger: "Denmark: All", then "Kommune: Aarhus" with a ✕ that clears', async () => {
  const s = load();
  const c = await mount(s);
  const trig = one(c, 'dagi-trigger');
  assert.strictEqual(one(c, 'dagi-trigger-label').textContent, 'Denmark: All');
  assert.strictEqual(one(c, 'dagi-clear').hidden, true);
  s.DF.syncFromUrl('#/nodes?kommune=0751');
  assert.strictEqual(one(c, 'dagi-trigger-label').textContent, 'Kommune: Aarhus');
  assert.strictEqual(one(c, 'dagi-clear').hidden, false);
  assert.ok(one(c, 'dagi-filter-wrap').classList.contains('dagi-active'));
  assert.strictEqual(trig.getAttribute('aria-haspopup'), 'dialog');
});

test('the clear and close buttons use the Phosphor x icon with a label, not an emoji (#1648)', async () => {
  const s = load({ stored: { param: 'kommune', code: '0751' } });
  const c = await mount(s);
  s.dispatch(one(c, 'dagi-trigger'), 'click');
  for (const cls of ['dagi-clear', 'dagi-close']) {
    const b = one(c, cls);
    assert.strictEqual(b.textContent, '', cls + ' has text');
    const svg = b.children[0];
    assert.ok(svg && svg.namespaceURI === 'http://www.w3.org/2000/svg' && svg.getAttribute('class') === 'ph-icon' && svg.getAttribute('aria-hidden') === 'true', cls + ' icon');
    assert.strictEqual(svg.children[0].getAttribute('href'), '/icons/phosphor-sprite.svg#ph-x');
    assert.ok(b.getAttribute('aria-label'), cls + ' has no label');
  }
});

test('trigger labels per layer', async () => {
  const s = await loaded();
  for (const [hash, want] of [['landsdel=218546', 'Landsdel: Østjylland'], ['dkregion=1082', 'Region: Region Midtjylland'], ['postnr=8000', 'Postnr: 8000 Aarhus C']]) {
    s.DF.syncFromUrl('#/nodes?' + hash);
    assert.strictEqual(s.DF.triggerLabel(), want);
  }
});

test('open: Kommune tab by default, tabs with counts, credit with https links', async () => {
  const s = load();
  const c = await mount(s);
  s.dispatch(one(c, 'dagi-trigger'), 'click');
  const panel = one(c, 'dagi-panel');
  assert.strictEqual(panel.hidden, false);
  assert.strictEqual(one(c, 'dagi-trigger').getAttribute('aria-expanded'), 'true');
  same(c.find('dagi-tab').map((t) => t.textContent), ['Region2', 'Landsdel2', 'Kommune7', 'Postnr5']);
  assert.strictEqual(c.find('dagi-tab-active')[0].textContent, 'Kommune7');
  const credit = one(c, 'dagi-credit');
  assert.ok(credit.textContent.indexOf('Indeholder data fra Klimadatastyrelsen, DAGI') === 0);
  same(credit.find('dagi-credit-link').map((a) => [a.textContent, a.href, a.rel]), [
    ['Licence', 'https://creativecommons.org/licenses/by/4.0/', 'noopener noreferrer'],
    ['Terms', 'https://dataforsyningen.dk/Vilkaar', 'noopener noreferrer']]);
  sameEl(s.doc.activeElement, one(c, 'dagi-search'), 'desktop focuses the search');
  assert.ok(s.placed.length === 1 && s.placed[0] === panel, 'desktop popover is kept inside the viewport');
});

test('open: the selected row is scrolled into view, and nothing else', async () => {
  const s = load({ stored: { param: 'kommune', code: '0860' } });
  const c = await mount(s);
  s.dispatch(one(c, 'dagi-trigger'), 'click');
  assert.strictEqual(s.doc.scrolled.length, 1);
  sameEl(s.doc.scrolled[0][0], rowByLabel(c, 'Hjørring'));
  assert.strictEqual(s.doc.scrolled[0][1], 'nearest');
  const t = load();
  const d = await mount(t);
  t.dispatch(one(d, 'dagi-trigger'), 'click');
  assert.strictEqual(t.doc.scrolled.length, 0);
});

test('open: a selection in another layer opens on its tab with its row in view', async () => {
  const s = load({ stored: { param: 'postnr', code: '8200' } });
  const c = await mount(s);
  s.dispatch(one(c, 'dagi-trigger'), 'click');
  assert.strictEqual(c.find('dagi-tab-active')[0].textContent, 'Postnr5');
  assert.strictEqual(s.doc.scrolled.length, 1);
  sameEl(s.doc.scrolled[0][0], rowByLabel(c, '8200 Aarhus N'));
  assert.strictEqual(rowByLabel(c, '8200 Aarhus N').getAttribute('aria-selected'), 'true');
});

test('rows are made per layer on first use: the open tab first, the rest on a search or a tab click', async () => {
  const s = load();
  const c = await mount(s);
  s.dispatch(one(c, 'dagi-trigger'), 'click');
  assert.strictEqual(c.find('dagi-option').length, 1 + 7, 'All of Denmark + the 7 kommuner only');
  s.dispatch(c.find('dagi-tab')[0], 'click');
  assert.strictEqual(c.find('dagi-option').length, 1 + 7 + 2);
  type(s, c, 'aar');
  assert.strictEqual(c.find('dagi-option').length, 1 + 7 + 2 + 2 + 5);
  assert.deepStrictEqual(shownRows(c), ['Aarhus', '8000 Aarhus C', '8200 Aarhus N'], 'list order kept');
  type(s, c, '');
  s.dispatch(c.find('dagi-tab')[3], 'click');
  assert.strictEqual(c.find('dagi-subhead').length, 4, 'thousand headers made once');
  s.DF.syncFromUrl('#/nodes?postnr=6960');
  assert.strictEqual(rowByLabel(c, '6960 Hvide Sande').getAttribute('aria-selected'), 'true');
});

test('credit: a link that is not https:// stays out', async () => {
  const s = load({ response: makeConfig({ source: { licenseUrl: 'javascript:alert(1)', termsUrl: 'http://x' } }) });
  const c = await mount(s);
  s.dispatch(one(c, 'dagi-trigger'), 'click');
  assert.strictEqual(one(c, 'dagi-credit').find('dagi-credit-link').length, 0);
});

test('phone: a sheet with no autofocus in the search, not placed as a popover; the backdrop closes it', async () => {
  const s = load({ sheet: true, touch: true });
  const c = await mount(s);
  s.dispatch(one(c, 'dagi-trigger'), 'click');
  const panel = one(c, 'dagi-panel');
  sameEl(s.doc.activeElement, panel, 'touch: the dialog takes focus, not the search box');
  assert.strictEqual(s.placed.length, 0);
  assert.strictEqual(one(c, 'dagi-backdrop').hidden, false);
  assert.ok(one(c, 'dagi-filter-wrap').classList.contains('dagi-open'));
  s.dispatch(one(c, 'dagi-backdrop'), 'click');
  assert.strictEqual(panel.hidden, true);
  assert.strictEqual(one(c, 'dagi-backdrop').hidden, true);
});

test('search box: debounced, matching rows shown, tabs hidden, "N more" and "no match"', async () => {
  const s = load();
  const c = await mount(s);
  s.dispatch(one(c, 'dagi-trigger'), 'click');
  const input = one(c, 'dagi-search');
  input.value = 'aar';
  s.dispatch(input, 'input');
  assert.strictEqual(shownRows(c)[0], 'All of Denmark', 'filtered before the debounce');
  s.clock.flush();
  same(shownRows(c), ['Aarhus', '8000 Aarhus C', '8200 Aarhus N']);
  assert.strictEqual(one(c, 'dagi-tabs').hidden, true);
  same(c.find('dagi-section-head').filter((h) => h.shown).map((h) => h.textContent), ['Kommune', 'Postnr']);
  type(s, c, '1051');
  const g = rowByLabel(c, '1050–1473 København K');
  assert.strictEqual(one(g, 'dagi-option-meta').textContent, 'includes 1051');
  type(s, c, 'zzz');
  same(shownRows(c), []);
  assert.strictEqual(one(c, 'dagi-empty').hidden, false);
  type(s, c, '');
  assert.strictEqual(one(c, 'dagi-tabs').hidden, false);
  assert.strictEqual(one(c, 'dagi-empty').hidden, true);
  assert.strictEqual(one(g, 'dagi-option-meta').textContent, '232 street postcodes', 'browse mode restores the meta');
});

test('search box: more than 30 hits show "… N more – keep typing"', async () => {
  const areas = [];
  for (let i = 0; i < 33; i++) areas.push({ code: String(5000 + i), name: 'Odense ' + i });
  const s = load({ response: makeConfig({ layers: [{ layer: 'postnummer', param: 'postnr', areas }] }) });
  const c = await mount(s);
  s.dispatch(one(c, 'dagi-trigger'), 'click');
  type(s, c, 'odense');
  assert.strictEqual(shownRows(c).length, 30);
  const more = one(c, 'dagi-more');
  assert.strictEqual(more.hidden, false);
  assert.strictEqual(more.textContent, '… 3 more – keep typing');
});

test('pick: saved, in the URL with the other params kept, announced once, popover closed', async () => {
  const s = load({ hash: '#/nodes?tab=repeater&search=x' });
  const c = await mount(s);
  const heard = [];
  s.DF.onChange((v) => heard.push(v));
  s.dispatch(one(c, 'dagi-trigger'), 'click');
  s.dispatch(one(rowByLabel(c, 'Aarhus'), 'dagi-option-label'), 'click'); // a tap on the label bubbles to the row
  same(heard, [{ param: 'kommune', code: '0751' }]);
  assert.strictEqual(s.location.hash, '#/nodes?tab=repeater&search=x&kommune=0751');
  same(JSON.parse(s.storage['meshcore-dagi-filter']), { param: 'kommune', code: '0751' });
  assert.strictEqual(s.DF.queryString(), '&kommune=0751');
  assert.strictEqual(one(c, 'dagi-panel').hidden, true);
  assert.strictEqual(one(c, 'dagi-trigger-label').textContent, 'Kommune: Aarhus');
  sameEl(s.doc.activeElement, one(c, 'dagi-trigger'));
  assert.strictEqual(rowByLabel(c, 'Aarhus').getAttribute('aria-selected'), 'true');
  // the same pick again changes nothing
  s.dispatch(one(c, 'dagi-trigger'), 'click');
  s.dispatch(rowByLabel(c, 'Aarhus'), 'click');
  assert.strictEqual(heard.length, 1);
});

test('pick in another layer replaces the param; "All of Denmark" and ✕ clear it', async () => {
  const s = load({ hash: '#/nodes?kommune=0751' });
  s.DF.syncFromUrl(s.location.hash);
  const c = await mount(s);
  const heard = [];
  s.DF.onChange((v) => heard.push(v));
  s.dispatch(one(c, 'dagi-trigger'), 'click');
  type(s, c, '8000');
  s.dispatch(rowByLabel(c, '8000 Aarhus C'), 'click');
  assert.strictEqual(s.location.hash, '#/nodes?postnr=8000');
  s.dispatch(one(c, 'dagi-trigger'), 'click');
  s.dispatch(rowByLabel(c, 'All of Denmark'), 'click');
  assert.strictEqual(s.location.hash, '#/nodes');
  assert.strictEqual(s.DF.getSelected(), null);
  s.DF.syncFromUrl('#/nodes?dkregion=1082');
  s.location.hash = '#/nodes?dkregion=1082';
  s.dispatch(one(c, 'dagi-clear'), 'click');
  assert.strictEqual(s.location.hash, '#/nodes');
  assert.ok(!('meshcore-dagi-filter' in s.storage));
  same(heard, [{ param: 'postnr', code: '8000' }, null, { param: 'dkregion', code: '1082' }, null]);
});

test('the URL is only written on a #/ route', async () => {
  const s = load({ hash: '' });
  const c = await mount(s);
  s.dispatch(one(c, 'dagi-trigger'), 'click');
  s.dispatch(rowByLabel(c, 'Aarhus'), 'click');
  assert.strictEqual(s.location.hash, '');
  same(s.replaced, []);
});

test('keyboard: ArrowDown from the search, arrows between rows, Enter in the search picks the first hit, Escape closes', async () => {
  const s = load();
  const c = await mount(s);
  const trig = one(c, 'dagi-trigger');
  s.dispatch(trig, 'click');
  const input = one(c, 'dagi-search');
  key(s, input, 'ArrowDown');
  sameEl(s.doc.activeElement, rowByLabel(c, 'All of Denmark'));
  key(s, s.doc.activeElement, 'ArrowDown');
  sameEl(s.doc.activeElement, rowByLabel(c, 'Bornholm'));
  key(s, s.doc.activeElement, 'ArrowUp');
  key(s, s.doc.activeElement, 'ArrowUp');
  sameEl(s.doc.activeElement, input, 'ArrowUp from the first row goes back to the search');
  const esc = key(s, input, 'Escape');
  assert.ok(esc.defaultPrevented);
  assert.strictEqual(one(c, 'dagi-panel').hidden, true);
  sameEl(s.doc.activeElement, trig);
  s.dispatch(trig, 'click');
  input.value = 'hvide';
  s.dispatch(input, 'input'); // Enter before the debounce fires still sees the query
  const e = key(s, input, 'Enter');
  assert.ok(e.defaultPrevented);
  same(s.DF.getSelected(), { param: 'postnr', code: '6960' });
  s.dispatch(trig, 'click');
  key(s, one(c, 'dagi-search'), 'Enter');
  same(s.DF.getSelected(), { param: 'postnr', code: '6960' }, 'Enter in an empty search picks nothing');
});

test('keyboard: ArrowDown typed within the debounce goes to the first hit, not the old list', async () => {
  const s = load();
  const c = await mount(s);
  s.dispatch(one(c, 'dagi-trigger'), 'click');
  const input = one(c, 'dagi-search');
  input.value = 'hjoer';
  s.dispatch(input, 'input');
  key(s, input, 'ArrowDown');
  sameEl(s.doc.activeElement, rowByLabel(c, 'Hjørring'));
  assert.strictEqual(s.clock.size, 0, 'the debounce still pending');
});

test('keyboard: Enter with a value that fired no input event (autofill) still picks its first hit', async () => {
  const s = load();
  const c = await mount(s);
  s.dispatch(one(c, 'dagi-trigger'), 'click');
  one(c, 'dagi-search').value = 'vording';
  key(s, one(c, 'dagi-search'), 'Enter');
  same(s.DF.getSelected(), { param: 'kommune', code: '0390' });
});

test('an outside click closes the popover; a click inside does not', async () => {
  const s = load();
  const c = await mount(s);
  s.dispatch(one(c, 'dagi-trigger'), 'click');
  s.dispatch(one(c, 'dagi-search'), 'click');
  assert.strictEqual(one(c, 'dagi-panel').hidden, false);
  s.dispatch(s.doc.body, 'click');
  assert.strictEqual(one(c, 'dagi-panel').hidden, true);
  s.dispatch(one(c, 'dagi-trigger'), 'click');
  s.dispatch(one(c, 'dagi-trigger'), 'click');
  assert.strictEqual(one(c, 'dagi-panel').hidden, true, 'the trigger toggles');
});

test('a tab click shows that layer only', async () => {
  const s = load();
  const c = await mount(s);
  s.dispatch(one(c, 'dagi-trigger'), 'click');
  s.dispatch(one(c.find('dagi-tab')[0], 'dagi-tab-count'), 'click'); // Region, via a child
  same(shownRows(c), ['All of Denmark', 'Region Hovedstaden', 'Region Midtjylland']);
  assert.strictEqual(c.find('dagi-tab')[0].getAttribute('aria-selected'), 'true');
});

test('XSS: a hostile name is text, never markup', async () => {
  const evil = '<img src=x onerror=alert(1)>';
  const s = load({ response: makeConfig({ credit: '<b>credit</b>', layers: [{ layer: 'kommune', param: 'kommune', areas: [{ code: '0101', name: evil }] }] }) });
  const c = await mount(s);
  s.DF.syncFromUrl('#/nodes?kommune=0101');
  s.dispatch(one(c, 'dagi-trigger'), 'click');
  assert.strictEqual(one(c, 'dagi-trigger-label').textContent, 'Kommune: ' + evil);
  assert.strictEqual(one(rowByLabel(c, evil), 'dagi-option-label').textContent, evil);
  assert.strictEqual(one(c, 'dagi-credit-text').textContent, '<b>credit</b>');
  assert.ok(c.all().every((n) => !Object.prototype.hasOwnProperty.call(n, 'innerHTML')), 'innerHTML was set');
  const src = fs.readFileSync('public/dagi-filter.js', 'utf8').replace(/\/\*[\s\S]*?\*\/|\/\/.*$/gm, '');
  assert.ok(!/innerHTML|outerHTML|insertAdjacentHTML/.test(src), 'an HTML sink in the source');
});

test('release(): document and window listeners go, the container is let go, a later init() works', async () => {
  const s = load();
  const c = await mount(s);
  s.dispatch(one(c, 'dagi-trigger'), 'click');
  assert.strictEqual(s.doc.count('click'), 1);
  assert.strictEqual(s.win.count('resize'), 1);
  s.DF.release(c);
  assert.strictEqual(s.doc.count(), 0);
  assert.strictEqual(s.win.count(), 0);
  assert.strictEqual(s.clock.size, 0, 'a pending timer outlived release()');
  // a sync after release must not touch the old DOM
  s.DF.syncFromUrl('#/nodes?kommune=0751');
  assert.strictEqual(one(c, 'dagi-trigger-label').textContent, 'Denmark: All');
  const c2 = await mount(s);
  assert.strictEqual(one(c2, 'dagi-trigger-label').textContent, 'Kommune: Aarhus');
});

test('close() drops the listeners open() added; reopening does not stack them', async () => {
  const s = load();
  const c = await mount(s);
  for (let i = 0; i < 3; i++) { s.dispatch(one(c, 'dagi-trigger'), 'click'); s.dispatch(s.doc.body, 'click'); }
  assert.strictEqual(s.doc.count(), 0);
  assert.strictEqual(s.win.count(), 0);
  s.dispatch(one(c, 'dagi-trigger'), 'click');
  s.dispatch(s.win, 'resize');
  assert.strictEqual(s.clock.size, 1);
  s.dispatch(s.doc.body, 'click');
  assert.strictEqual(s.clock.size, 0, 'the resize timer outlived close()');
});

test('init() on a new container releases the old one', async () => {
  const s = load();
  const a = await mount(s);
  s.dispatch(one(a, 'dagi-trigger'), 'click');
  const b = await mount(s);
  assert.strictEqual(s.doc.count(), 0);
  assert.strictEqual(one(b, 'dagi-trigger-label').textContent, 'Denmark: All');
  s.DF.syncFromUrl('#/nodes?kommune=0751');
  assert.strictEqual(one(a, 'dagi-trigger-label').textContent, 'Denmark: All', 'the old container still updated');
  assert.strictEqual(one(b, 'dagi-trigger-label').textContent, 'Kommune: Aarhus');
});

test('offChange() stops the announcements', async () => {
  const s = await loaded();
  let n = 0;
  const fn = s.DF.onChange(() => n++);
  s.DF.syncFromUrl('#/nodes?kommune=0751');
  s.DF.offChange(fn);
  s.DF.syncFromUrl('#/nodes?kommune=0860');
  assert.strictEqual(n, 1);
});

/* ---------- the hidden-count notice ---------- */

test('hiddenNotice(): none for 0, singular and plural, and says the count is the whole list', () => {
  const s = load();
  assert.strictEqual(s.DF.hiddenNotice(0), '');
  assert.strictEqual(s.DF.hiddenNotice(undefined), '');
  assert.strictEqual(s.DF.hiddenNotice(1), '1 node has no position, so no Danish-area filter can include it. The count covers all nodes, before the other filters.');
  assert.strictEqual(s.DF.hiddenNotice(18), '18 nodes have no position, so no Danish-area filter can include them. The count covers all nodes, before the other filters.');
});

/* ---------- performance ---------- */

test('perf: parsing and normalising 720 areas, and one search over them, each under 50 ms', async () => {
  const areas = [];
  for (let i = 0; i < 605; i++) areas.push({ code: String(1000 + i * 14), name: 'Ærøskøbing Østre Å ' + i });
  const kom = [];
  for (let i = 0; i < 99; i++) kom.push({ code: String(i).padStart(4, '0'), name: 'Kommune Ø' + i });
  const cfg = makeConfig({ layers: [{ layer: 'kommune', param: 'kommune', areas: kom }, { layer: 'postnummer', param: 'postnr', areas }] });
  const s = load({ response: cfg });
  let t0 = process.hrtime.bigint();
  await s.DF.ready.call(null);
  const c = await mount(s);
  const loadMs = Number(process.hrtime.bigint() - t0) / 1e6;
  s.dispatch(one(c, 'dagi-trigger'), 'click');
  t0 = process.hrtime.bigint();
  type(s, c, 'aeroesk');
  const searchMs = Number(process.hrtime.bigint() - t0) / 1e6;
  console.log(`    (load+render ${loadMs.toFixed(1)} ms, open+search ${searchMs.toFixed(1)} ms over 704 rows)`);
  assert.ok(loadMs < 50 && searchMs < 50, `load ${loadMs} ms, search ${searchMs} ms`);
});

/* ---------- wiring: router and nodes.js ---------- */

test('app.js: the router syncs DAGI links right after the region links, before init()', () => {
  const src = fs.readFileSync('public/app.js', 'utf8');
  const region = src.indexOf('RegionFilter.syncFromUrl(location.hash)');
  const dagi = src.indexOf('DagiFilter.syncFromUrl(location.hash)');
  const init = src.indexOf('pages[basePage].init(app, routeParam)');
  assert.ok(region > 0 && dagi > region && dagi < init, `region ${region}, dagi ${dagi}, init ${init}`);
});

test('index.html: dagi-filter.js loads beside area-filter.js and before nodes.js, with __BUST__', () => {
  const html = fs.readFileSync('public/index.html', 'utf8');
  const at = (f) => html.indexOf('<script src="' + f + '?v=__BUST__"');
  assert.ok(at('dagi-filter.js') > 0, 'no script tag');
  assert.ok(at('dagi-filter.js') < at('nodes.js'));
});

function loadNodes(dagi) {
  const ctx = {
    console, Promise, JSON, String, Number, Array, Object, Map, Set, Date, Math, RegExp,
    encodeURIComponent, URLSearchParams, setTimeout, clearTimeout,
    registerPage: () => {}, location: { hash: '#/nodes' }, history: { replaceState() {} },
    localStorage: { getItem: () => null, setItem() {}, removeItem() {} },
    document: { addEventListener() {}, removeEventListener() {}, getElementById: () => null, querySelector: () => null, querySelectorAll: () => [] },
  };
  ctx.window = ctx; // as in a browser, window is the global object
  if (dagi) ctx.DagiFilter = dagi;
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync('public/url-state.js', 'utf8'), ctx);
  vm.runInContext(fs.readFileSync('public/nodes.js', 'utf8'), ctx, { filename: 'public/nodes.js' });
  return ctx;
}

test('nodes.js: buildNodesQuery keeps the DAGI param, so a tab, search or sort rewrite does not erase it', () => {
  const w = loadNodes({ hashParam: () => 'kommune=0751' });
  assert.strictEqual(w.buildNodesQuery('all', ''), '?kommune=0751');
  assert.strictEqual(w.buildNodesQuery('repeater', 'ab c'), '?tab=repeater&search=ab%20c&kommune=0751');
  const none = loadNodes({ hashParam: () => '' });
  assert.strictEqual(none.buildNodesQuery('repeater', ''), '?tab=repeater');
  const absent = loadNodes(null);
  assert.strictEqual(absent.buildNodesQuery('all', 'x'), '?search=x', 'no DagiFilter loaded');
});

test('nodes.js: the list waits for ready(), sends queryString(), shows the notice and releases the control', () => {
  const src = fs.readFileSync('public/nodes.js', 'utf8');
  const load = src.slice(src.indexOf('async function loadNodes('), src.indexOf('// window.MC_GEO_FILTER is set asynchronously'));
  const ready = load.indexOf('await DagiFilter.ready()');
  const qs = load.indexOf('DagiFilter.queryString()');
  const first = load.indexOf("await api('/nodes?'");
  assert.ok(ready > 0 && qs > ready && first > qs, `ready ${ready}, queryString ${qs}, first request ${first}`);
  assert.ok(/data\.dagi/.test(load), 'the hidden count is not read from the response');
  const destroy = src.slice(src.indexOf('function destroy() {'), src.indexOf('let _themeRefreshHandler'));
  assert.ok(/DagiFilter\.release\(/.test(destroy) && /DagiFilter\.offChange\(/.test(destroy), 'destroy() keeps the control');
  assert.ok(/id="nodesDagiFilter"/.test(src) && /id="nodesDagiNotice"[^>]*role="status"/.test(src), 'containers missing');
});

/* ---------- run ---------- */

(async () => {
  console.log('\n=== dagi-filter.js (#740 M3a) ===');
  for (const [name, fn] of queue) {
    try { await fn(); passed++; console.log(`  ✓ ${name}`); }
    catch (e) { failed++; console.error(`  ✗ ${name}: ${e.message}`); }
  }
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
