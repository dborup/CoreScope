/**
 * Unit tests (#737 "Later (customizer)", AGENTS rule 8): the Skråfoto
 * viewer's defaults in the customizer.
 *
 * Three viewer-side settings, section `skraafoto` of the customizer's
 * overrides (public/customize-v2.js), with their one rule in
 * public/skraafoto.js (Skraafoto.SETTINGS):
 *   - direction: 'auto' (M3c: perpendicular to A–B from a Path profile,
 *     otherwise north), or 'north' | 'east' | 'south' | 'west' | 'nadir';
 *   - zoom: 'overview' (M3c) or 'zoomed' (the M3b crop first);
 *   - lineOfSight: true (M3c: drawn from a profile) or false.
 * The server's map.skraafoto config stays server-only.
 *
 * The real modules run in vm sandboxes; nothing is copied:
 *   - the viewer on the fake DOM the M2/M3c suites share
 *     (tests/skraafoto-fake-dom.js), in the terrain-profile sandbox;
 *   - the customizer in tests/customizer-sandbox.js.
 *
 * Covered:
 *   1. the rule: the keys, the defaults (M3c's behaviour), valid values
 *      only, the section over the defaults, the live SITE_CONFIG;
 *   2. the viewer's precedence: deep link > customizer > auto, for the
 *      direction; the default zoom on each new photo, a click still wins;
 *      the line neither asked for nor drawn when off, while the side view
 *      still comes from the line; settings read when the viewer opens;
 *   3. the customizer: a known object section, stored values, import
 *      validation, the export round trip, the rendered inputs, shown only
 *      with the server's block (the line row only with the profile tool),
 *      no server key accepted, one shared rule.
 * Reset All is in test-issue-1496-reset-all-complete.js.
 *
 * Usage: node test-issue-737-customizer-skraafoto.js
 */
'use strict';
const fs = require('fs');
const path = require('path');
const assert = require('assert');
const { loadTerrainProfile } = require('./tests/map-overlay-sandbox');
const { fakeDom } = require('./tests/skraafoto-fake-dom');
const { loadCustomizer: loadCustomizerSandbox } = require('./tests/customizer-sandbox');

let passed = 0, failed = 0;
const tests = [];
function test(name, fn) { tests.push([name, fn]); }
function eq(actual, expected, msg) {
  // Node 26 rejects an explicit undefined message, so it is passed only when given.
  const args = [JSON.parse(JSON.stringify(actual === undefined ? null : actual)), expected];
  if (msg !== undefined) args.push(msg);
  assert.deepStrictEqual(...args);
}

const { win } = loadTerrainProfile(['terrain-profile-cursor.js', 'skraafoto.js', 'heritage-info.js']);
const SF = win.Skraafoto;
assert.ok(SF, 'public/skraafoto.js must define window.Skraafoto');

const CFG = {
  enabled: true, searchUrl: '/api/skraafoto/search',
  previewUrl: '/api/skraafoto/preview/{collection}/{item}', maxPx: 1024, note: 'n', credit: null,
  bounds: [[54.4265, 7.99125], [57.778, 15.5995]],
};
const FRAME = { width: 1024, height: 765 };
const DEFAULTS = { direction: 'auto', zoom: 'zoomed', lineOfSight: true }; // #864: zoomed is the built-in
const LINE_NS = { a: { lat: 56.10, lon: 10.20 }, b: { lat: 56.20, lon: 10.20 } };

function rule() {
  const R = SF.SETTINGS;
  assert.ok(R && typeof R.resolve === 'function' && typeof R.valid === 'function', 'Skraafoto.SETTINGS missing');
  return R;
}

// ── 1. The rule ─────────────────────────────────────────────────────────────

test('the keys and the defaults M3c hard-coded', () => {
  const R = rule();
  eq(R.KEYS, ['direction', 'zoom', 'lineOfSight']);
  eq(R.DEFAULTS, DEFAULTS);
  eq(R.DIRECTIONS, ['auto', 'north', 'east', 'south', 'west', 'nadir']);
  eq(R.ZOOMS, ['overview', 'zoomed']);
});

test('valid: the listed values only', () => {
  const R = rule();
  const ok = { direction: ['auto', 'north', 'east', 'south', 'west', 'nadir'], zoom: ['overview', 'zoomed'], lineOfSight: [true, false] };
  const bad = {
    direction: ['Auto', 'N', 'up', '', null, 0, true, ['north'], 'perpendicular'],
    zoom: ['in', 'Zoomed', true, 1, null, ''],
    lineOfSight: [1, 0, 'true', 'false', null, undefined, {}],
  };
  for (const k of Object.keys(ok)) ok[k].forEach((v) => assert.strictEqual(R.valid(k, v), true, k + ' rejected ' + JSON.stringify(v)));
  for (const k of Object.keys(bad)) bad[k].forEach((v) => assert.strictEqual(R.valid(k, v), false, k + ' accepted ' + String(v)));
  // Server-only keys and prototype names are never settings.
  ['enabled', 'token', 'searchUrl', 'maxPx', 'hasOwnProperty', '__proto__', 'constructor'].forEach((k) =>
    assert.strictEqual(R.valid(k, true), false, k));
});

test('resolve: the section over the defaults, junk dropped key by key', () => {
  const R = rule();
  eq(R.resolve({}), DEFAULTS);
  eq(R.resolve(null), DEFAULTS);
  eq(R.resolve('west'), DEFAULTS);
  eq(R.resolve({ direction: 'west', zoom: 'zoomed', lineOfSight: false }), { direction: 'west', zoom: 'zoomed', lineOfSight: false });
  eq(R.resolve({ direction: 'up', zoom: 'zoomed', lineOfSight: 0 }), { direction: 'auto', zoom: 'zoomed', lineOfSight: true });
  // Inherited keys are not settings.
  eq(R.resolve(Object.create({ direction: 'west' })), DEFAULTS);
});

test('resolve(): without an argument, the live window.SITE_CONFIG.skraafoto', () => {
  const R = rule();
  const saved = win.SITE_CONFIG;
  try {
    delete win.SITE_CONFIG;
    eq(R.resolve(), DEFAULTS);
    win.SITE_CONFIG = { skraafoto: { zoom: 'overview' } };
    eq(R.resolve().zoom, 'overview', '#864: a stored overview wins over the zoomed default');
  } finally { win.SITE_CONFIG = saved; }
});

// ── 2. The viewer ───────────────────────────────────────────────────────────

function photo(dir, extra) {
  const n = SF.DIRECTIONS.indexOf(dir) + 1;
  const id = '2025_83_36_' + n + '_0020_00003045';
  return Object.assign({
    id, collection: 'skraafotos2025', direction: dir, datetime: '2025-05-07T10:21:33Z',
    viewerUrl: 'https://skraafoto.dataforsyningen.dk/?item=' + id,
    previewUrl: '/api/skraafoto/preview/skraafotos2025/' + id,
    frame: FRAME, point: { x: 300.5, y: 400.2 }, inFrame: true, approx: false,
  }, extra || {});
}

/* The M3a search (the path echoed only when asked for), the preview and
 * the M3b crop. o.missing drops directions; o.extra goes into each photo. */
function fakeService(o) {
  o = o || {};
  const svc = { search: [], preview: [], zoom: [] };
  const headers = (h) => ({ get: (k) => (Object.prototype.hasOwnProperty.call(h, k) ? h[k] : null) });
  svc.fetch = (u) => {
    if (u.indexOf(CFG.searchUrl + '?') === 0) {
      svc.search.push(u);
      const q = new URLSearchParams(u.split('?')[1]);
      const p = q.get('path') ? [{ x: 10, y: 700 }, { x: 1000, y: 80 }] : undefined;
      const body = { collection: 'skraafotos2025', year: 2025, years: [2025, 2023],
        photos: SF.DIRECTIONS.filter((d) => !(o.missing || []).includes(d)).map((d) => photo(d, Object.assign({ path: p }, o.extra))),
        credit: null };
      return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(body) });
    }
    if (u.indexOf('?zoom=1') > 0) {
      svc.zoom.push(u);
      return Promise.resolve({ ok: true, status: 200,
        headers: headers({ 'X-CoreScope-Skraafoto-Marker': '512,349.5', 'X-CoreScope-Skraafoto-Crop': '172.5,312.8,428.5,487.55' }),
        blob: () => Promise.resolve({ of: u }) });
    }
    svc.preview.push(u);
    return Promise.resolve({ ok: true, status: 200, headers: headers({}), blob: () => Promise.resolve({ of: u }) });
  };
  return svc;
}

function find(e, pred, out) {
  out = out || [];
  if (pred(e)) out.push(e);
  (e.children || []).forEach((c) => find(c, pred, out));
  return out;
}

/* withViewer opens a viewer with o, the customizer section `section`
 * (window.SITE_CONFIG.skraafoto), on a fake DOM and service. */
async function withViewer(o, section, fn, svcOpts) {
  const dom = fakeDom();
  const svc = fakeService(svcOpts);
  const saved = { document: win.document, URL: win.URL, SITE_CONFIG: win.SITE_CONFIG };
  win.document = dom.doc;
  win.URL = { createObjectURL: (b) => 'blob:test#' + b.of, revokeObjectURL() {} };
  win.SITE_CONFIG = section === undefined ? {} : { skraafoto: section };
  const settle = async () => { for (let i = 0; i < 4; i++) await new Promise((r) => setImmediate(r)); };
  const t = {
    dom, svc, settle,
    img: () => dom.byTid('sf-img'),
    zoom: () => dom.byTid('sf-zoom'),
    marks: () => dom.byTid('sf-marks'),
    los: () => find(dom.byTid('sf-marks'), (e) => e.attrs['data-testid'] === 'sf-los'),
    load(w, h) { const i = dom.byTid('sf-img'); i.naturalWidth = w; i.naturalHeight = h; i.onload(); },
    async click(tid) { dom.byTid(tid).fire('click'); await settle(); },
  };
  try {
    t.viewer = SF.open(Object.assign({ lat: 56.15, lon: 10.2, cfg: CFG, fetch: svc.fetch }, o));
    assert.ok(t.viewer, 'the viewer opened');
    await settle();
    await fn(t);
  } finally {
    if (t.viewer) t.viewer.close(false);
    win.document = saved.document;
    win.URL = saved.URL;
    win.SITE_CONFIG = saved.SITE_CONFIG;
  }
}

test('viewer, no setting (#864): node north, profile the side view, the crop first, line drawn', async () => {
  await withViewer({}, undefined, async (t) => {
    eq(t.viewer.state().dir, 'north');
    eq(t.svc.zoom, [photo('north').previewUrl + '?zoom=1&x=300.5&y=400.2'], 'the crop first');
    eq(t.svc.preview, [], 'no overview fetched first');
    eq(t.zoom().getAttribute('aria-pressed'), 'true');
    await t.click('sf-zoom');
    eq(t.zoom().getAttribute('aria-pressed'), 'false', 'Zoom out shows the overview');
    eq(t.svc.preview, [photo('north').previewUrl]);
  });
  await withViewer({}, { zoom: 'overview' }, async (t) => {
    eq(t.svc.zoom, [], 'a stored overview still opens on the whole photo');
    eq(t.svc.preview, [photo('north').previewUrl]);
  });
  await withViewer({ line: LINE_NS }, undefined, async (t) => {
    eq(t.viewer.state().dir, 'east', 'the side view of a north–south line');
    assert.ok(/&path=/.test(t.svc.search[0]), t.svc.search[0]);
    t.load(1024, 765);
    eq(t.los().length, 1, 'the line drawn');
  });
});

test('direction auto is the same as no setting', () => withViewer({ line: LINE_NS }, { direction: 'auto' }, async (t) => {
  eq(t.viewer.state().dir, 'east');
}));

test('direction west: the first photo from a node and from a profile', async () => {
  await withViewer({}, { direction: 'west', zoom: 'overview' }, async (t) => {
    eq(t.viewer.state().dir, 'west');
    eq(t.svc.preview, [photo('west').previewUrl], 'only that photo fetched');
  });
  await withViewer({ line: LINE_NS }, { direction: 'west' }, async (t) => {
    eq(t.viewer.state().dir, 'west', 'the customizer over the side view');
  });
  await withViewer({ line: LINE_NS }, { direction: 'nadir' }, async (t) => {
    eq(t.viewer.state().dir, 'nadir');
  });
});

test('a deep-linked direction wins over the customizer', () => withViewer({ dir: 'south' }, { direction: 'west' }, async (t) => {
  eq(t.viewer.state().dir, 'south');
}));

test('a chosen direction the point has no photo of falls back as before', () => withViewer({}, { direction: 'west' }, async (t) => {
  eq(t.viewer.state().dir, 'north');
}, { missing: ['west'] }));

test('zoom zoomed: the first photo is the crop, not the preview', () => withViewer({}, { zoom: 'zoomed' }, async (t) => {
  eq(t.svc.zoom, [photo('north').previewUrl + '?zoom=1&x=300.5&y=400.2']);
  eq(t.svc.preview, [], 'no overview fetched first');
  eq(t.zoom().getAttribute('aria-pressed'), 'true');
  eq(t.zoom().textContent, 'Zoom out');
  t.load(1024, 699);
  eq(t.marks().getAttribute('viewBox'), '0 0 1024 699', 'crop space');
  // A click still wins: Zoom out shows the overview.
  await t.click('sf-zoom');
  eq(t.zoom().getAttribute('aria-pressed'), 'false');
  eq(t.svc.preview, [photo('north').previewUrl]);
}));

test('zoom zoomed: another direction opens zoomed too; overview still turns it off (M3c)', async () => {
  await withViewer({}, { zoom: 'zoomed' }, async (t) => {
    await t.click('sf-zoom'); // out
    await t.click('sf-dir-west');
    eq(t.zoom().getAttribute('aria-pressed'), 'true', 'the default again for the next photo');
    eq(t.svc.zoom.length, 2);
    eq(t.svc.zoom[1], photo('west').previewUrl + '?zoom=1&x=300.5&y=400.2');
  });
  await withViewer({}, { zoom: 'overview' }, async (t) => {
    t.load(1024, 765);
    await t.click('sf-zoom');
    await t.click('sf-dir-west');
    eq(t.zoom().getAttribute('aria-pressed'), 'false');
    eq(t.svc.zoom.length, 1, 'no crop of west');
  });
});

test('zoom zoomed: a year change opens the new year zoomed', () => withViewer({}, { zoom: 'zoomed' }, async (t) => {
  const y = t.dom.byTid('sf-year');
  y.value = '2023';
  y.fire('change');
  await t.settle();
  eq(t.svc.search.length, 2);
  eq(t.zoom().getAttribute('aria-pressed'), 'true');
  // The stub answers every year with the same ids, so the kept crop is shown.
  assert.ok(/\?zoom=1&/.test(t.img().src), 'the crop on screen: ' + t.img().src);
  eq(t.svc.preview, [], 'no overview fetched');
}));

test('zoom zoomed: out of frame there is nothing to zoom on, so the overview', () => withViewer({}, { zoom: 'zoomed' }, async (t) => {
  eq(t.svc.zoom, []);
  eq(t.svc.preview, [photo('north').previewUrl]);
  eq(t.zoom().getAttribute('aria-pressed'), 'false');
  eq(t.zoom().disabled, true);
}, { extra: { inFrame: false } }));

test('line of sight off: not asked for, not drawn, not named; the side view stays', () => withViewer({ line: LINE_NS }, { lineOfSight: false, zoom: 'overview' }, async (t) => {
  assert.ok(t.svc.search[0].indexOf('path=') < 0, t.svc.search[0]);
  assert.ok(t.svc.search[0].indexOf('ha=') < 0 && t.svc.search[0].indexOf('hb=') < 0, t.svc.search[0]);
  eq(t.viewer.state().dir, 'east', 'the default direction still sees the line from the side');
  t.load(1024, 765);
  eq(t.los().length, 0, 'no line');
  eq(t.marks().getAttribute('aria-label'), 'Point marked at the centre-left.');
}));

test('line of sight off: a path the server sends anyway is not drawn', () => withViewer({ line: LINE_NS }, { lineOfSight: false }, async (t) => {
  t.load(1024, 765);
  eq(t.los().length, 0);
}, { extra: { path: [{ x: 1, y: 2 }, { x: 3, y: 4 }] } }));

test('the settings are read when a viewer opens: the next one follows a change', async () => {
  const dom = fakeDom();
  const svc = fakeService();
  const saved = { document: win.document, URL: win.URL, SITE_CONFIG: win.SITE_CONFIG };
  win.document = dom.doc;
  win.URL = { createObjectURL: (b) => 'blob:test#' + b.of, revokeObjectURL() {} };
  const settle = async () => { for (let i = 0; i < 4; i++) await new Promise((r) => setImmediate(r)); };
  try {
    win.SITE_CONFIG = { skraafoto: { direction: 'east' } };
    let v = SF.open({ lat: 56.15, lon: 10.2, cfg: CFG, fetch: svc.fetch });
    await settle();
    eq(v.state().dir, 'east');
    v.close(false);
    win.SITE_CONFIG = { skraafoto: { direction: 'south' } };
    v = SF.open({ lat: 56.15, lon: 10.2, cfg: CFG, fetch: svc.fetch });
    await settle();
    eq(v.state().dir, 'south');
    v.close(false);
  } finally {
    win.document = saved.document;
    win.URL = saved.URL;
    win.SITE_CONFIG = saved.SITE_CONFIG;
  }
});

/* A cold page load of a deep link, in its own sandbox: skraafoto.js runs
 * with the hash already set, the client config ready and (opts.customizer)
 * the customizer still waiting for /api/config/theme. Returns the window,
 * the fake DOM, the search URLs and fire(type) for window events. */
function coldLoad(opts) {
  const vm = require('vm');
  const dom = fakeDom();
  const listeners = {};
  const search = [];
  const fetch = (u) => {
    if (u.indexOf(CFG.searchUrl + '?') === 0) {
      search.push(u);
      const body = { collection: 'skraafotos2025', year: 2025, years: [2025], photos: SF.DIRECTIONS.map((d) => photo(d)), credit: null };
      return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(body) });
    }
    return Promise.resolve({ ok: true, status: 200, headers: { get: () => null }, blob: () => Promise.resolve({ of: u }) });
  };
  const w = {
    document: dom.doc, fetch, Promise, setTimeout, clearTimeout, URLSearchParams, AbortController,
    URL: { createObjectURL: (b) => 'blob:test#' + b.of, revokeObjectURL() {} },
    location: { hash: '#/nodes/x?skraafoto=56.15000,10.20000,north', pathname: '/', search: '' },
    history: { state: null, replaceState() {} },
    MC_SKRAAFOTO_CFG: CFG, MeshConfigReady: Promise.resolve({}),
    SITE_CONFIG: {},
    addEventListener(t, f) { (listeners[t] = listeners[t] || []).push(f); },
    removeEventListener(t, f) { listeners[t] = (listeners[t] || []).filter((x) => x !== f); },
  };
  if (opts.customizer) w._customizerV2 = { initDone: false };
  w.window = w;
  const ctx = vm.createContext(w);
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'public', 'skraafoto.js'), 'utf8'), ctx, { filename: 'skraafoto.js' });
  return { w, dom, search, fire: (t) => (listeners[t] || []).slice().forEach((f) => f({ type: t })) };
}
const settleAll = async (n) => { for (let i = 0; i < (n || 6); i++) await new Promise((r) => setTimeout(r, 0)); };

test('a cold deep link waits for the customizer\'s overrides before it opens', async () => {
  const c = coldLoad({ customizer: true });
  await settleAll();
  assert.strictEqual(c.w.Skraafoto.current(), null, 'opened before the overrides were applied');
  eq(c.search, []);
  // The customizer's pipeline: SITE_CONFIG set, theme-changed, then initDone.
  c.w.SITE_CONFIG = { skraafoto: { zoom: 'zoomed' } };
  c.fire('theme-changed');
  c.w._customizerV2.initDone = true;
  await settleAll();
  const v = c.w.Skraafoto.current();
  assert.ok(v, 'the deep link did not open');
  eq(v.state().dir, 'north', 'the link\'s direction');
  eq(c.dom.byTid('sf-zoom').getAttribute('aria-pressed'), 'true', 'the customizer\'s zoom applies');
  v.close(false);
});

test('a cold deep link without the customizer opens at once, as before', async () => {
  const c = coldLoad({});
  await settleAll();
  assert.ok(c.w.Skraafoto.current(), 'not opened');
  eq(c.search.length, 1);
  c.w.Skraafoto.current().close(false);
});

// ── 3. The customizer ───────────────────────────────────────────────────────

/* The customizer with the viewer loaded first (index.html order is the
 * other way round, but the customizer reads the rule only when it is
 * used, after every script has run). o.sf: the server's block (default
 * CFG; null: none); o.profile: the Path profile tool offered; o.noRule:
 * without skraafoto.js. */
function loadCustomizer(o) {
  o = o || {};
  const sf = 'sf' in o ? o.sf : CFG;
  return loadCustomizerSandbox({
    server: o.server,
    dhm: o.profile ? { enabled: true, url: '/api/map/dhm/wms', profile: true } : undefined,
    scripts: o.noRule ? [] : (o.profile ? ['terrain-profile.js'] : []).concat(['skraafoto.js']),
    setup: (ctx) => {
      ctx.location = { hash: '#/map', pathname: '/', search: '' };
      ctx.history = { state: null, replaceState() {} };
      ctx.URLSearchParams = URLSearchParams;
      ctx.Promise = Promise;
      if (sf) ctx.window.MC_SKRAAFOTO_CFG = sf;
    },
  });
}
const stored = (c) => JSON.parse(c.localStorage.getItem('cs-theme-overrides') || '{}');
const plain = (v) => JSON.parse(JSON.stringify(v));

test('skraafoto is a known object section', () => {
  const { api } = loadCustomizer();
  assert.ok(api.VALID_SECTIONS.indexOf('skraafoto') >= 0);
  eq(api.SKRAAFOTO_SETTING_KEYS, ['direction', 'zoom', 'lineOfSight']);
  assert.strictEqual(api.validateShape({ skraafoto: { direction: 'west' } }).valid, true);
  const bad = api.validateShape({ skraafoto: 'west' });
  assert.strictEqual(bad.valid, false);
  assert.ok(/must be an object/.test(bad.errors.join(' ')), bad.errors.join(' '));
});

test('only valid settings are stored; each rejected one warns; no server key', () => {
  const c = loadCustomizer();
  c.api.writeOverrides({ skraafoto: { direction: 'up', zoom: 'zoomed', lineOfSight: 'no', enabled: false, token: 'x', maxPx: 2048 } });
  eq(stored(c).skraafoto, { zoom: 'zoomed' });
  assert.strictEqual(c.warnings.filter((w) => /Invalid oblique photo setting/.test(w)).length, 2, c.warnings.join('\n'));
  const d = loadCustomizer();
  d.api.writeOverrides({ skraafoto: { direction: 'nadir', zoom: 'overview', lineOfSight: false } });
  eq(stored(d).skraafoto, { direction: 'nadir', zoom: 'overview', lineOfSight: false });
});

test('without the viewer on the page no setting is accepted', () => {
  const c = loadCustomizer({ noRule: true });
  c.api.writeOverrides({ skraafoto: { direction: 'west' } });
  assert.strictEqual(stored(c).skraafoto, undefined);
  assert.strictEqual(c.api.skraafotoSettings({}), null);
  assert.strictEqual(c.api.renderSkraafotoSettings(), '');
  assert.strictEqual(c.api.validateShape({ skraafoto: { direction: 'west' } }).valid, false);
});

test('validateShape names every bad value and the allowed ones; unknown keys only warn', () => {
  const { api, warnings } = loadCustomizer();
  const res = api.validateShape({ skraafoto: { direction: 'up', zoom: 2, lineOfSight: 'yes', token: 'x' } });
  assert.strictEqual(res.valid, false);
  const msg = res.errors.join(' | ');
  assert.ok(/skraafoto\.direction = "up" .*auto, north, east, south, west or nadir/.test(msg), msg);
  assert.ok(/skraafoto\.zoom = "2" .*overview or zoomed/.test(msg), msg);
  assert.ok(/skraafoto\.lineOfSight = "yes" .*true or false/.test(msg), msg);
  assert.strictEqual(res.errors.length, 3, msg);
  assert.ok(warnings.some((w) => /Unknown skraafoto key in import: token/.test(w)), warnings.join('\n'));
});

test('the skraafoto section survives an export round trip', () => {
  const first = loadCustomizer();
  const delta = { skraafoto: { direction: 'east', zoom: 'zoomed', lineOfSight: false } };
  first.api.writeOverrides(delta);
  const exported = first.api.readOverrides();
  eq(exported, delta);
  const second = loadCustomizer();
  const shape = second.api.validateShape(exported);
  assert.strictEqual(shape.valid, true, shape.errors.join('; '));
  second.api.writeOverrides(exported);
  eq(second.api.readOverrides(), delta);
  eq(second.api.skraafotoSettings(second.api.computeEffective({}, exported)), delta.skraafoto);
  // And the effective config the viewer reads is the same.
  second.api.init({});
  eq(second.ctx.window.SITE_CONFIG.skraafoto, delta.skraafoto);
});

test('the section: shown only with the server\'s block; the line row only with the profile tool', () => {
  const input = (h, key) => {
    const m = h.match(new RegExp('<(select|input)[^>]*data-cv2-skraafoto="' + key + '"[^>]*>'));
    return m && m[0];
  };
  const c = loadCustomizer();
  c.ctx.window.SITE_CONFIG = { skraafoto: { direction: 'west', zoom: 'zoomed' } };
  const html = c.api.renderSkraafotoSettings();
  assert.ok(/Oblique Photos/.test(html), html);
  assert.ok(/^<select/.test(input(html, 'direction')) && /id="cv2-skraafoto-direction"/.test(html) && /for="cv2-skraafoto-direction"/.test(html), 'direction select');
  const opts = (key) => {
    const m = html.match(new RegExp('<select[^>]*data-cv2-skraafoto="' + key + '"[^>]*>([\\s\\S]*?)</select>'));
    return m ? Array.from(m[1].matchAll(/<option value="([^"]+)"( selected)?/g)).map((x) => x[1] + (x[2] ? '*' : '')) : null;
  };
  eq(opts('direction'), ['auto', 'north', 'east', 'south', 'west*', 'nadir']);
  eq(opts('zoom'), ['overview', 'zoomed*']);
  assert.strictEqual(input(html, 'lineOfSight'), null, 'line row without the profile tool');
  assert.ok(!/#[0-9a-fA-F]{3,8}\b/.test(html), 'a hard-coded colour');
  assert.strictEqual(loadCustomizer({ sf: null }).api.renderSkraafotoSettings(), '', 'shown without the server block');
  assert.strictEqual(loadCustomizer({ sf: Object.assign({}, CFG, { enabled: false }) }).api.renderSkraafotoSettings(), '', 'shown with the gate off');

  const p = loadCustomizer({ profile: true });
  p.ctx.window.SITE_CONFIG = { skraafoto: { lineOfSight: false } };
  const ph = p.api.renderSkraafotoSettings();
  const l = input(ph, 'lineOfSight');
  assert.ok(l && /type="checkbox"/.test(l) && !/checked/.test(l), String(l));
  p.ctx.window.SITE_CONFIG = {};
  assert.ok(/checked/.test(input(p.api.renderSkraafotoSettings(), 'lineOfSight')), 'on by default');
});

test('the Display tab badge counts the section', () => {
  const c = loadCustomizer();
  const before = c.api.countOverrides('skraafoto');
  c.api.writeOverrides({ skraafoto: { direction: 'west', zoom: 'zoomed' } });
  eq([before, c.api.countOverrides('skraafoto')], [0, 2]);
  const src = fs.readFileSync(path.join(__dirname, 'public', 'customize-v2.js'), 'utf8');
  assert.ok(/id: 'display'[^\n]*_countOverrides\('skraafoto'\)/.test(src), 'the Display badge leaves the section out');
});

test('one rule: the customizer keeps no copy of the values', () => {
  const src = fs.readFileSync(path.join(__dirname, 'public', 'customize-v2.js'), 'utf8');
  const code = src.replace(/\/\*[\s\S]*?\*\//g, '').replace(/^\s*\/\/.*$/gm, '');
  assert.ok(!/'zoomed'|'overview'|\['auto'/.test(code), 'customize-v2.js hard-codes a Skråfoto value');
  assert.ok(/Skraafoto/.test(code) && /\.SETTINGS\b/.test(code));
  // The viewer section never reaches the server's own block.
  assert.ok(!/MC_SKRAAFOTO_CFG\s*=/.test(code), 'the customizer writes the server block');
});

(async () => {
  console.log('\n=== #737 customizer: Skråfoto viewer defaults ===\n');
  for (const [name, fn] of tests) {
    try { await fn(); passed++; console.log('  ✅ ' + name); }
    catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
  }
  console.log('\n' + passed + ' passed, ' + failed + ' failed\n');
  process.exit(failed ? 1 : 0);
})();
