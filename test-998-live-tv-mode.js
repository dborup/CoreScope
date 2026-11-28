/**
 * #998 — Live: a visible "TV mode" button that builds and starts the kiosk
 * link (#883 kiosk, #968 operator preset).
 *
 * The real public/live-tv-mode.js, public/live-kiosk.js and
 * public/area-filter.js run in vm sandboxes with the shared fake DOM
 * (tests/fake-dom.js); nothing is copied.
 *   1. the link builder: tour order, bounds, theme, Moments, unknown and
 *      duplicate area keys dropped, the step cap, and a round trip through
 *      LiveKiosk.parseKioskParams (the kiosk's own rules);
 *   2. the dialog's defaults from the operator preset (window.MC_KIOSK_CFG);
 *   3. the dialog (fake DOM): the area list in tick order, the bounded
 *      seconds field, the theme, Moments shown only when the server has
 *      them, Start here, Copy link, the QR code, Escape and its listener;
 *   4. AreaFilter.list(): the cached areas config, one request at most;
 *   5. live.js, index.html and the customizer pointer (source pins).
 * Usage: node test-998-live-tv-mode.js
 */
'use strict';
const fs = require('fs');
const path = require('path');
const assert = require('assert');
const { makeDoc, loadPublic, flush } = require('./tests/fake-dom');

let passed = 0, failed = 0;
const tests = [];
function test(name, fn) { tests.push([name, fn]); }
const plain = (v) => JSON.parse(JSON.stringify(v === undefined ? null : v));
const eq = (a, b, m) => assert.deepStrictEqual(plain(a), b, m);

const K = loadPublic('live-kiosk.js').LiveKiosk;
const T = loadPublic('live-tv-mode.js').LiveTvMode;

// As /api/config/areas serves them: random order, key and label.
const AREAS = [
  { key: 'jyl', label: 'Jylland' },
  { key: 'sj', label: 'Sjælland' },
  { key: 'fyn', label: 'Fyn' },
];
const query = (hash) => {
  assert.ok(hash.indexOf('#/live?') === 0, 'not a Live link: ' + hash);
  return new URLSearchParams(hash.slice('#/live?'.length));
};
const build = (choice, o) => T.buildHash(Object.assign({ tour: [], every: 60, theme: 'dark', moments: true }, choice), (o && o.areas) || AREAS, K, o && o.preset);

// ── 1. The link builder ─────────────────────────────────────────────────────

test('buildHash: kiosk=1, the tour in the given order (not sorted), every and theme', () => {
  assert.strictEqual(build({ tour: ['sj', 'fyn', 'jyl'], every: 90, theme: 'light' }),
    '#/live?kiosk=1&tour=sj,fyn,jyl&every=90&theme=light');
  assert.strictEqual(build({ tour: ['jyl', 'sj'] }), '#/live?kiosk=1&tour=jyl,sj&every=60&theme=dark');
});

test('buildHash: an empty tour is written as tour= (a static view, not the preset tour)', () => {
  const h = build({ tour: [] }, { preset: { tour: ['sj', 'fyn'] } });
  assert.strictEqual(h, '#/live?kiosk=1&tour=&every=60&theme=dark');
  eq(K.parseKioskParams(query(h), { tour: ['sj', 'fyn'] }).tour, []);
});

test('buildHash: every within the kiosk bounds (15-3600, whole seconds); not a number is the preset value', () => {
  const every = (v, preset) => query(build({ every: v }, { preset })).get('every');
  const cases = [[5, '15'], [14, '15'], [15, '15'], [60.9, '60'], ['45', '45'], [' 120', '120'], [3600, '3600'], [3601, '3600'], [99999, '3600'], [-5, '15']];
  for (const [v, want] of cases) assert.strictEqual(every(v), want, 'every ' + JSON.stringify(v));
  assert.strictEqual(every('abc'), '60', 'NaN, no preset');
  assert.strictEqual(every('', { everySeconds: 30 }), '30', 'empty: the preset');
  assert.strictEqual(every(null, { everySeconds: 30 }), '30', 'null: the preset');
  assert.strictEqual(K.EVERY_MIN, 15);
  assert.strictEqual(K.EVERY_MAX, 3600);
});

test('buildHash: theme dark, light or auto; anything else is the preset theme, then dark', () => {
  for (const t of ['dark', 'light', 'auto']) assert.strictEqual(query(build({ theme: t })).get('theme'), t);
  assert.strictEqual(query(build({ theme: 'neon' })).get('theme'), 'dark');
  assert.strictEqual(query(build({ theme: 'Light' }, { preset: { theme: 'auto' } })).get('theme'), 'auto');
  assert.strictEqual(query(build({ theme: null }, { preset: { theme: 'light' } })).get('theme'), 'light');
});

test('buildHash: Moments off adds moments=0; on adds nothing (on is the kiosk default)', () => {
  assert.strictEqual(query(build({ moments: false })).get('moments'), '0');
  assert.strictEqual(query(build({ moments: true })).get('moments'), null);
  assert.ok(!/moments/.test(build({ moments: true })));
});

test('buildHash: unknown and duplicate area keys are dropped; the rest keep their order', () => {
  assert.strictEqual(query(build({ tour: ['nope', 'fyn', 'SJ', 'fyn', 'jyl', '', null, 7] })).get('tour'), 'fyn,jyl');
  // Case matters here: the dialog offers the configured keys only.
  assert.strictEqual(query(build({ tour: ['Fyn'] })).get('tour'), '');
  assert.strictEqual(query(build({ tour: ['fyn'] }, { areas: [] })).get('tour'), '', 'no areas configured');
});

test('buildHash: at most LiveKiosk.MAX_STEPS steps', () => {
  const many = Array.from({ length: 20 }, (_, i) => ({ key: 'a' + i, label: 'Area ' + i }));
  const tour = query(build({ tour: many.map((a) => a.key).reverse() }, { areas: many })).get('tour').split(',');
  assert.strictEqual(K.MAX_STEPS, 12);
  assert.strictEqual(tour.length, 12);
  assert.strictEqual(tour[0], 'a19', 'the first ticked stays first');
});

test('buildHash: a key is percent-encoded; a key the link cannot carry is not offered', () => {
  const areas = [{ key: 'Nord & Syd', label: 'NS' }, { key: 'a,b', label: 'comma' }, { key: ' pad', label: 'pad' }, { key: 'ø', label: 'oe' }];
  const h = build({ tour: ['ø', 'Nord & Syd', 'a,b', ' pad'] }, { areas });
  assert.ok(h.indexOf('tour=%C3%B8,Nord%20%26%20Syd&') !== -1, h);
  eq(K.parseKioskParams(query(h)).tour, ['ø', 'Nord & Syd']);
});

test('buildHash: round trip through LiveKiosk.parseKioskParams gives exactly the choice', () => {
  const preset = { tour: ['jyl'], everySeconds: 300, theme: 'auto' };
  const h = build({ tour: ['fyn', 'sj'], every: 45, theme: 'light', moments: false }, { preset });
  const p = K.parseKioskParams(query(h), preset);
  assert.strictEqual(p.kiosk, true);
  eq(p.tour, ['fyn', 'sj']);
  assert.strictEqual(p.every, 45);
  assert.strictEqual(p.theme, 'light');
  // Live reads moments= itself (live.js initMoments): moments=0 is off.
  assert.strictEqual(query(h).get('moments'), '0');
});

test('linkUrl: the page\'s origin and path with the hash; the query (cache busters) is dropped', () => {
  const loc = { origin: 'https://mesh.example', pathname: '/corescope/', search: '?r=123', hash: '#/live' };
  assert.strictEqual(T.linkUrl(loc, '#/live?kiosk=1&tour=&every=60&theme=dark'),
    'https://mesh.example/corescope/#/live?kiosk=1&tour=&every=60&theme=dark');
  assert.strictEqual(T.linkUrl({ origin: 'http://localhost:3000', pathname: '/' }, '#/live?kiosk=1'), 'http://localhost:3000/#/live?kiosk=1');
});

test('areaList: valid keys only, deduped by key, sorted by label', () => {
  const list = T.areaList([{ key: 'b', label: 'Beta' }, { key: 'a', label: 'alpha' }, { key: 'b', label: 'Again' },
    { key: '', label: 'empty' }, { key: 'x' }, null, { key: 'a,b', label: 'comma' }, { key: 'n', label: 7 }, { key: 'c', label: 'Gamma' }]);
  // No usable label: the key, as the kiosk's step label (resolveSteps).
  eq(list, [{ key: 'a', label: 'alpha' }, { key: 'b', label: 'Beta' }, { key: 'c', label: 'Gamma' }, { key: 'n', label: 'n' }, { key: 'x', label: 'x' }]);
  eq(T.areaList(null), []);
  eq(T.areaList({ key: 'a' }), []);
});

// ── 2. The defaults from the operator preset ────────────────────────────────

test('defaults: no preset is no tour, 60 s, dark, Moments on', () => {
  eq(T.defaults(null, AREAS, K), { tour: [], every: 60, theme: 'dark', moments: true });
});

test('defaults: the preset\'s tour (its order, keys matched as the kiosk does, unknown dropped), every and theme', () => {
  const preset = { tour: ['SJ', 'nope', 'jyl', 'sj'], everySeconds: 120, theme: 'light', tiles: ['rate'], remountHours: 2 };
  eq(T.defaults(preset, AREAS, K), { tour: ['sj', 'jyl'], every: 120, theme: 'light', moments: true });
});

test('defaults: an invalid preset value is the built-in default (LiveKiosk.SETTINGS.resolve)', () => {
  eq(T.defaults({ tour: 'sj', everySeconds: 5, theme: 'neon' }, AREAS, K), { tour: [], every: 60, theme: 'dark', moments: true });
  eq(T.defaults({ everySeconds: 3600 }, AREAS, K).every, 3600);
});

// ── 3. The dialog ───────────────────────────────────────────────────────────

function fakeQr() {
  const calls = { data: [] };
  const factory = (type, level) => {
    calls.type = type; calls.level = level;
    return { addData: (d) => calls.data.push(d), make: () => {}, getModuleCount: () => 3, isDark: (r, c) => (r + c) % 2 === 0 };
  };
  factory.calls = calls;
  return factory;
}

function openDialog(o) {
  o = o || {};
  const doc = makeDoc();
  const h = { doc, nav: [], copies: [], closed: 0, focused: 0 };
  const returnFocus = { focus: () => { h.focused++; } };
  h.ctl = T.open({
    doc,
    root: doc.body,
    K,
    areas: o.areas || AREAS,
    preset: o.preset || null,
    momentsAvailable: o.moments !== false,
    location: { origin: 'https://mesh.example', pathname: '/', search: '?r=1', hash: '#/live' },
    qrcode: o.qr === undefined ? fakeQr() : o.qr,
    copy: (text, ok, fail) => { h.copies.push(text); if (o.copyFails) fail(); else ok(); },
    navigate: (hash) => h.nav.push(hash),
    returnFocus,
    onClose: () => { h.closed++; },
  });
  h.root = doc.body.children[0];
  h.one = (cls) => { const f = h.root.find(cls); assert.strictEqual(f.length, 1, cls + ': ' + f.length); return f[0]; };
  h.boxes = () => h.root.find('live-tv-area-input');
  h.box = (key) => h.boxes().find((b) => b.getAttribute('data-area') === key);
  h.tick = (key, on) => { const b = h.box(key); b.checked = on; b.fire('change', { target: b }); };
  h.url = () => h.one('live-tv-url').textContent;
  h.orders = () => h.root.find('live-tv-area').map((li) => li.find('live-tv-area-label')[0].textContent + ':' + li.find('live-tv-order')[0].textContent);
  return h;
}

test('dialog: a modal labelled TV mode, in the root, with the areas sorted by label', () => {
  const h = openDialog();
  assert.strictEqual(h.doc.body.children.length, 1);
  assert.strictEqual(h.root.getAttribute('role'), 'dialog');
  assert.strictEqual(h.root.getAttribute('aria-modal'), 'true');
  const title = h.one('live-tv-title');
  assert.strictEqual(title.textContent, 'TV mode');
  assert.strictEqual(h.root.getAttribute('aria-labelledby'), title.id);
  eq(h.orders(), ['Fyn:', 'Jylland:', 'Sjælland:']);
  assert.strictEqual(h.url(), 'https://mesh.example/#/live?kiosk=1&tour=&every=60&theme=dark');
});

test('dialog: the tour is the tick order, shown as 1, 2, …; unticking renumbers', () => {
  const h = openDialog();
  h.tick('sj', true);
  h.tick('fyn', true);
  h.tick('jyl', true);
  eq(h.orders(), ['Fyn:2', 'Jylland:3', 'Sjælland:1']);
  assert.ok(h.url().endsWith('#/live?kiosk=1&tour=sj,fyn,jyl&every=60&theme=dark'), h.url());
  h.tick('sj', false);
  eq(h.orders(), ['Fyn:1', 'Jylland:2', 'Sjælland:']);
  assert.ok(/tour=fyn,jyl&/.test(h.url()), h.url());
  eq(h.ctl.state().tour, ['fyn', 'jyl']);
});

test('dialog: the preset fills it: its tour ticked in order, its seconds and its theme', () => {
  const h = openDialog({ preset: { tour: ['jyl', 'fyn'], everySeconds: 30, theme: 'auto' } });
  eq(h.orders(), ['Fyn:2', 'Jylland:1', 'Sjælland:']);
  assert.strictEqual(h.box('jyl').checked, true);
  assert.strictEqual(h.box('sj').checked, false);
  assert.strictEqual(h.one('live-tv-every').value, '30');
  assert.strictEqual(h.one('live-tv-theme').value, 'auto');
  assert.ok(h.url().endsWith('tour=jyl,fyn&every=30&theme=auto'), h.url());
});

test('dialog: seconds field bounded by the kiosk (min/max/step); a value outside is pulled in on change', () => {
  const h = openDialog();
  const inp = h.one('live-tv-every');
  assert.strictEqual(inp.type, 'number');
  assert.strictEqual(inp.getAttribute('min'), '15');
  assert.strictEqual(inp.getAttribute('max'), '3600');
  assert.strictEqual(inp.getAttribute('step'), '1');
  inp.value = '5';
  inp.fire('input', { target: inp });
  assert.ok(/&every=15&/.test(h.url()), 'input: ' + h.url());
  inp.fire('change', { target: inp });
  assert.strictEqual(inp.value, '15', 'shown as the kiosk will use it');
  inp.value = '90000';
  inp.fire('change', { target: inp });
  assert.strictEqual(inp.value, '3600');
  inp.value = '';
  inp.fire('change', { target: inp });
  assert.strictEqual(inp.value, '60', 'empty: the default');
});

test('dialog: the theme select offers dark, light and auto, and writes the link', () => {
  const h = openDialog();
  const sel = h.one('live-tv-theme');
  eq(sel.children.map((o) => o.value), ['dark', 'light', 'auto']);
  sel.value = 'light';
  sel.fire('change', { target: sel });
  assert.ok(h.url().endsWith('&theme=light'), h.url());
});

test('dialog: Moments on by default; off adds moments=0; not offered without server Moments', () => {
  const h = openDialog();
  const m = h.one('live-tv-moments');
  assert.strictEqual(m.checked, true);
  m.checked = false;
  m.fire('change', { target: m });
  assert.ok(h.url().endsWith('&moments=0'), h.url());
  const n = openDialog({ moments: false });
  assert.strictEqual(n.root.find('live-tv-moments').length, 0);
  assert.ok(!/moments/.test(n.url()));
});

test('dialog: at the step cap the unticked areas are disabled until one is unticked', () => {
  const many = Array.from({ length: 14 }, (_, i) => ({ key: 'k' + String(i).padStart(2, '0'), label: 'Area ' + String(i).padStart(2, '0') }));
  const h = openDialog({ areas: many });
  many.slice(0, 12).forEach((a) => h.tick(a.key, true));
  assert.strictEqual(h.box('k12').disabled, true);
  assert.strictEqual(h.box('k13').disabled, true);
  assert.strictEqual(h.box('k00').disabled, false, 'a ticked one can be unticked');
  h.tick('k03', false);
  assert.strictEqual(h.box('k12').disabled, false);
});

test('dialog: without configured areas it says the screen shows one view, and offers no list', () => {
  const h = openDialog({ areas: [] });
  assert.strictEqual(h.boxes().length, 0);
  assert.ok(/No areas are configured/.test(h.root.textContent), h.root.textContent);
});

test('dialog: Start here closes the dialog and navigates to the built link in this tab', () => {
  const h = openDialog();
  h.tick('fyn', true);
  h.one('live-tv-start').click();
  eq(h.nav, ['#/live?kiosk=1&tour=fyn&every=60&theme=dark']);
  assert.strictEqual(h.doc.body.children.length, 0, 'dialog removed');
  assert.strictEqual(h.closed, 1);
});

test('dialog: Copy link copies the full URL and says so; a failure says how to copy by hand', () => {
  const h = openDialog();
  h.tick('jyl', true);
  h.one('live-tv-copy').click();
  eq(h.copies, ['https://mesh.example/#/live?kiosk=1&tour=jyl&every=60&theme=dark']);
  assert.ok(/copied/i.test(h.one('live-tv-status').textContent));
  assert.strictEqual(h.one('live-tv-status').getAttribute('role'), 'status');
  assert.strictEqual(h.doc.body.children.length, 1, 'stays open');
  const f = openDialog({ copyFails: true });
  f.one('live-tv-copy').click();
  assert.ok(/select the link/i.test(f.one('live-tv-status').textContent), f.one('live-tv-status').textContent);
});

test('dialog: QR code of the full URL on demand, redrawn when the link changes; no button without the helper', () => {
  const qr = fakeQr();
  const h = openDialog({ qr });
  const btn = h.one('live-tv-qr-btn');
  const box = h.one('live-tv-qr');
  assert.strictEqual(box.hidden, true);
  assert.strictEqual(qr.calls.data.length, 0, 'nothing drawn before asked');
  btn.click();
  assert.strictEqual(box.hidden, false);
  assert.strictEqual(btn.getAttribute('aria-expanded'), 'true');
  assert.strictEqual(qr.calls.type, 0);
  assert.strictEqual(qr.calls.level, 'M');
  eq(qr.calls.data, ['https://mesh.example/#/live?kiosk=1&tour=&every=60&theme=dark']);
  const svg = box.children[0];
  assert.strictEqual(svg.tagName, 'SVG');
  // 3 modules plus a 4-module quiet zone on each side; the dark ones in one path.
  assert.strictEqual(svg.getAttribute('viewBox'), '0 0 11 11');
  const d = svg.children[0].getAttribute('d');
  assert.strictEqual((d.match(/M/g) || []).length, 5, d);
  assert.ok(d.indexOf('M4 4h1v1h-1z') === 0, d);
  h.tick('sj', true);
  assert.strictEqual(qr.calls.data[qr.calls.data.length - 1], 'https://mesh.example/#/live?kiosk=1&tour=sj&every=60&theme=dark');
  btn.click();
  assert.strictEqual(box.hidden, true);
  const n = openDialog({ qr: null });
  assert.strictEqual(n.root.find('live-tv-qr-btn').length, 0);
});

test('dialog: Escape, the close button and the backdrop close it; the keydown listener goes, focus returns', () => {
  for (const how of ['escape', 'button', 'backdrop']) {
    const h = openDialog();
    assert.strictEqual(h.doc.count('keydown'), 1);
    if (how === 'escape') h.doc.fire('keydown', { key: 'Escape' });
    else if (how === 'button') h.one('live-tv-close').click();
    else h.root.fire('click', { target: h.root });
    assert.strictEqual(h.doc.body.children.length, 0, how);
    assert.strictEqual(h.doc.count('keydown'), 0, how + ': listener left');
    assert.strictEqual(h.closed, 1, how);
    assert.strictEqual(h.focused, 1, how + ': focus');
    h.ctl.close();
    assert.strictEqual(h.closed, 1, how + ': close() twice is one close');
  }
  const h = openDialog();
  h.one('live-tv-panel').fire('click', { target: h.one('live-tv-panel') });
  assert.strictEqual(h.doc.body.children.length, 1, 'a click inside does not close');
});

test('dialog: every target is at least 44 px (the CSS), and the panel fits 375x560 (scrolls inside)', () => {
  const css = fs.readFileSync(path.join(__dirname, 'public/live.css'), 'utf8');
  const at = css.indexOf('/* #998');
  assert.ok(at >= 0, 'no #998 block in live.css');
  const block = css.slice(at);
  const rules = [...block.replace(/\/\*[\s\S]*?\*\//g, '').matchAll(/([^{}]+)\{([^{}]*)\}/g)]
    .map((m) => ({ sels: m[1].split(',').map((s) => s.trim()), body: m[2] }));
  ['.live-tv-btn', '.live-tv-close', '.live-tv-action', '.live-tv-area', '.live-tv-every', '.live-tv-theme', '.live-tv-check']
    .forEach((sel) => assert.ok(rules.some((r) => r.sels.includes(sel) && /min-height:\s*44px/.test(r.body) &&
      (sel === '.live-tv-area' || sel === '.live-tv-check' || /min-width:\s*44px/.test(r.body))), sel + ' < 44 px'));
  assert.ok(/\.live-tv-panel\s*\{[^}]*max-height:[^}]*overflow-y: auto/.test(block), 'the panel scrolls inside');
  assert.ok(!/#[0-9a-fA-F]{3,8}\b/.test(block.replace(/\/\*[\s\S]*?\*\//g, '')), 'a hard-coded colour');
});

// ── 4. AreaFilter.list(): the cached areas config ───────────────────────────

test('AreaFilter.list(): one /api/config/areas request for any number of callers; a copy each time', async () => {
  let fetches = 0, release;
  const gate = new Promise((r) => { release = r; });
  const W = loadPublic('area-filter.js', {
    localStorage: { getItem: () => null, setItem: () => {}, removeItem: () => {} },
    fetch: (url) => { fetches++; assert.strictEqual(url, '/api/config/areas'); return gate.then(() => ({ json: () => AREAS })); },
  });
  const A = W.AreaFilter;
  const a = A.list(), b = A.list();
  release();
  const [la, lb] = await Promise.all([a, b]);
  assert.strictEqual(fetches, 1, 'concurrent callers share one request');
  const want = plain(AREAS);
  eq(la, want);
  la.push({ key: 'x', label: 'X' });
  eq(await A.list(), want, 'a copy');
  assert.strictEqual(fetches, 1, 'cached');
  await flush();
});

// ── 5. live.js, index.html, the customizer ──────────────────────────────────

const live = fs.readFileSync(path.join(__dirname, 'public/live.js'), 'utf8');
const html = fs.readFileSync(path.join(__dirname, 'public/index.html'), 'utf8');
const tv = fs.readFileSync(path.join(__dirname, 'public/live-tv-mode.js'), 'utf8');

test('live.js: the TV mode button is in the Live controls (the gear menu), after every other control', () => {
  const body = live.slice(live.indexOf('id="liveControlsBody"'), live.indexOf('<!-- /#liveHeader -->'));
  const at = body.indexOf('id="liveTvModeBtn"');
  assert.ok(at > 0, 'no button in #liveControlsBody');
  assert.ok(at > body.indexOf('id="audioControls"'), 'not appended last');
  assert.ok(/<button type="button" id="liveTvModeBtn" class="vcr-btn live-tv-btn" aria-haspopup="dialog">TV mode<\/button>/.test(body));
});

test('live.js: it opens LiveTvMode with the cached areas, the preset and the kiosk rule; destroy() closes it', () => {
  const fn = live.slice(live.indexOf('async function openTvMode('), live.indexOf('function closeTvMode('));
  assert.ok(/AreaFilter\.list\(\)/.test(fn), 'areas not from the cached config');
  assert.ok(/LiveTvMode\.open\(/.test(fn) && /preset: window\.MC_KIOSK_CFG/.test(fn) && /K: window\.LiveKiosk/.test(fn), fn);
  assert.ok(/momentsAvailable: !!window\.MC_MOMENTS_CFG/.test(fn));
  assert.ok(!/\bapi\(|fetch\(/.test(fn), 'a request of its own');
  const destroy = live.slice(live.indexOf('function destroy()'));
  assert.ok(/closeTvMode\(\)/.test(destroy), 'destroy() leaves the dialog');
  // Wired with the other controls, before init()'s first await (#125, #220).
  const wire = live.slice(live.indexOf('  function wireLiveControls()'), live.indexOf('  function wireFollowNext()'));
  assert.ok(/wireTvMode\(\);/.test(wire), 'not wired in wireLiveControls()');
  const tvWire = live.slice(live.indexOf('  function wireTvMode()'), live.indexOf('  function setLiveHashParam('));
  assert.ok(/getElementById\('liveTvModeBtn'\)/.test(tvWire) && /openTvMode\(btn\)/.test(tvWire), tvWire);
  const init = live.slice(live.indexOf('  async function init(app)'), live.indexOf("    map = L.map('liveMap'"));
  assert.ok(!/getElementById\('liveTvModeBtn'\)/.test(init), 'init() reaches for the button itself');
});

test('index.html loads live-tv-mode.js after live-kiosk.js and before live.js', () => {
  const k = html.indexOf('src="live-kiosk.js?v=__BUST__"'), t = html.indexOf('src="live-tv-mode.js?v=__BUST__"'), l = html.indexOf('src="live.js?v=__BUST__"');
  assert.ok(k > 0 && t > k && l > t, [k, t, l].join(' '));
});

test('live-tv-mode.js builds DOM only: no HTML sink', () => {
  const src = tv.replace(/\/\*[\s\S]*?\*\//g, '').replace(/\/\/.*$/gm, '');
  assert.ok(!/innerHTML|outerHTML|insertAdjacentHTML|createImgTag|createSvgTag|document\.write/.test(src));
});

test('the customizer\'s Display → Live kiosk section points to the TV mode button', () => {
  const { loadCustomizer } = require('./tests/customizer-sandbox');
  const c = loadCustomizer({ scripts: ['live-kiosk.js'], setup: (ctx) => { ctx.window.MC_KIOSK_CFG = null; } });
  const out = c.api.renderKioskSettings();
  assert.ok(/TV mode/.test(out), 'no pointer');
  assert.ok(/Live/.test(out) && /gear|settings/i.test(out), 'says where the button is');
});

test('docs/CUSTOMIZATION.md describes the TV mode button next to the kiosk preset', () => {
  const doc = fs.readFileSync(path.join(__dirname, 'docs/CUSTOMIZATION.md'), 'utf8');
  const at = doc.indexOf('### Live kiosk preset');
  assert.ok(at >= 0);
  const sec = doc.slice(at, doc.indexOf('\n## ', at));
  assert.ok(/\*\*TV mode\*\*/.test(sec) && /QR code/.test(sec) && /empty `tour=`/.test(sec), 'TV mode not described');
});

(async () => {
  console.log('\n=== #998 Live TV mode: link builder, dialog, wiring ===');
  for (const [name, fn] of tests) {
    try { await fn(); passed++; console.log('  ✓ ' + name); }
    catch (e) { failed++; console.error('  ✗ ' + name + '\n    ' + (e && e.stack || e)); }
  }
  console.log('\n' + passed + ' passed, ' + failed + ' failed');
  process.exit(failed ? 1 : 0);
})();
