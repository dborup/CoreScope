/**
 * Unit tests (#737 M2): the Skråfoto viewer's pure helpers —
 * public/skraafoto.js, and the chart-point mapping it shares with the
 * #781 cursor in public/terrain-profile.js.
 *
 * The real modules are loaded with vm.createContext (the sandbox the other
 * terrain-profile suites use); nothing here copies production logic. The
 * server's wording (credit, privacy note, oldest year) is read from
 * internal/skraafoto/skraafoto.go, so the two cannot drift apart.
 *
 * Covered:
 *   1. the gate: offered only for an enabled block with a same-origin
 *      search path; a node only with a real fix (TerrainProfile.hasFix);
 *   2. URL building: the search URL (5 decimals, year only when valid),
 *      the preview URL (only the server's own preview path) and the
 *      "Full resolution" link (only the official viewer);
 *   3. directions: the five (four oblique plus nadir) in a fixed order,
 *      and which one is shown when the wanted one has no photo;
 *   4. years: newest first, 2017 left out, the selected year;
 *   5. the photo date, the error wording for 429/503/404/other;
 *   6. the deep link: format, parse, and writing it into a hash that keeps
 *      every other key;
 *   7. the credit links in the #733 order, and the server's wording;
 *   8. the profile: endpoint A/B and a chart sample → lat/lon, reusing
 *      the #781 mapping (fraction + latLonAt), on unevenly spaced samples
 *      (#818);
 *   9. the open viewer's object URLs (#818): one per preview fetched,
 *      none revoked while on screen, every one revoked once by close;
 *  10. the node action only inside the block's bounds, Denmark (#818),
 *      by the heritage overlay's own bounds test.
 *
 * Usage: node test-issue-737-skraafoto-viewer.js
 */
'use strict';
const fs = require('fs');
const path = require('path');
const assert = require('assert');
const { loadTerrainProfile } = require('./tests/map-overlay-sandbox');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}
function eq(actual, expected, msg) {
  assert.deepStrictEqual(JSON.parse(JSON.stringify(actual === undefined ? null : actual)), expected, msg);
}

const { TP, win } = loadTerrainProfile(['terrain-profile-cursor.js', 'skraafoto.js', 'heritage-info.js']);
const SF = win.Skraafoto;
const CUR = win.TerrainProfileCursor;
assert.ok(SF, 'public/skraafoto.js must define window.Skraafoto');

// internal/skraafoto/skraafoto.go: string and int constants.
function goConsts(file) {
  const src = fs.readFileSync(path.join(__dirname, file), 'utf8');
  const out = {};
  let m;
  const str = /^\s*([A-Z][A-Za-z]*)\s*=\s*"((?:[^"\\]|\\.)*)"\s*$/mg;
  while ((m = str.exec(src))) out[m[1]] = JSON.parse('"' + m[2] + '"');
  const num = /^\s*([A-Z][A-Za-z]*)\s*=\s*([0-9]+)\s*$/mg;
  while ((m = num.exec(src))) out[m[1]] = Number(m[2]);
  return out;
}
const GO = goConsts('internal/skraafoto/skraafoto.go');
const DHM = goConsts('internal/dhm/dhm.go');
// dhm.Coverage, the box the server sends as the block's bounds.
const COVERAGE = (function () {
  const src = fs.readFileSync(path.join(__dirname, 'internal', 'dhm', 'dhm.go'), 'utf8');
  const m = /^var Coverage = Bounds\{South: ([0-9.]+), West: ([0-9.]+), North: ([0-9.]+), East: ([0-9.]+)\}$/m.exec(src);
  assert.ok(m, 'dhm.Coverage not found in internal/dhm/dhm.go');
  return [[Number(m[1]), Number(m[2])], [Number(m[3]), Number(m[4])]];
})();

// /api/config/client's skraafoto block, field for field as
// cmd/server/skraafoto_proxy.go (SkraafotoClientConfig) sends it.
const CREDIT = {
  attribution: DHM.Owner + ', ' + GO.DatasetName + ', ' + DHM.LicenseName,
  owner: DHM.Owner, datasetName: GO.DatasetName, datasetUrl: GO.DatasetURL,
  licenseName: DHM.LicenseName, licenseUrl: DHM.LicenseURL,
};
const CFG = {
  enabled: true, searchUrl: '/api/skraafoto/search',
  previewUrl: '/api/skraafoto/preview/{collection}/{item}', maxPx: 1024,
  note: GO.PrivacyNote, credit: CREDIT, bounds: COVERAGE,
};

// One photo as GET /api/skraafoto/search returns it.
function photo(dir, year, n) {
  const id = (year || 2025) + '_83_36_' + (n || 1) + '_0020_00003045';
  const c = 'skraafotos' + (year || 2025);
  return {
    id, collection: c, direction: dir, datetime: '2025-05-07T10:21:33Z',
    viewerUrl: 'https://skraafoto.dataforsyningen.dk/?item=' + id,
    previewUrl: '/api/skraafoto/preview/' + c + '/' + id,
  };
}

console.log('\n=== #737 M2 Skråfoto viewer helpers ===\n');

// ── 1. The gate ─────────────────────────────────────────────────────────────

test('offered: only an enabled block with a same-origin search path', () => {
  eq(SF.offered(CFG), true);
  [null, undefined, {}, { enabled: false, searchUrl: CFG.searchUrl }, { enabled: 'true', searchUrl: CFG.searchUrl },
    { enabled: true }, { enabled: true, searchUrl: 'https://evil.example/api/skraafoto/search' },
    { enabled: true, searchUrl: '//evil.example/x' }, { enabled: true, searchUrl: 'javascript:alert(1)' }]
    .forEach((c) => eq(SF.offered(c), false, JSON.stringify(c)));
});

test('nodePoint: a node with a real fix, by TerrainProfile.hasFix', () => {
  assert.strictEqual(typeof TP.hasFix, 'function', 'TerrainProfile.hasFix is exported');
  eq(SF.nodePoint({ lat: 56.15674, lon: 10.21076 }), { lat: 56.15674, lon: 10.21076 });
  eq(SF.nodePoint({ lat: '56.156741234', lon: '10.2107649' }), { lat: 56.15674, lon: 10.21076 });
  [null, {}, { lat: 0, lon: 0 }, { lat: null, lon: 10 }, { lat: '', lon: '' }, { lat: 91, lon: 10 },
    { lat: 56, lon: 181 }, { lat: 'x', lon: 10 }]
    .forEach((n) => eq(SF.nodePoint(n), null, JSON.stringify(n)));
});

// ── 2. URL building ─────────────────────────────────────────────────────────

test('searchURL: five decimals, year only when it is one the viewer offers', () => {
  eq(SF.searchURL(CFG, 56.156741234, 10.21076), '/api/skraafoto/search?lat=56.15674&lon=10.21076');
  eq(SF.searchURL(CFG, 56.15674, 10.21076, 2021), '/api/skraafoto/search?lat=56.15674&lon=10.21076&year=2021');
  eq(SF.searchURL(CFG, 56.15674, 10.21076, 2017), '/api/skraafoto/search?lat=56.15674&lon=10.21076');
  eq(SF.searchURL(CFG, 56.15674, 10.21076, '2021'), '/api/skraafoto/search?lat=56.15674&lon=10.21076');
  eq(SF.searchURL(CFG, 55.5, -0.000001), '/api/skraafoto/search?lat=55.50000&lon=0.00000', 'a rounded -0 is plain 0');
  eq(SF.searchURL(CFG, NaN, 10), null);
  eq(SF.searchURL(CFG, 56, 200), null);
  eq(SF.searchURL({ enabled: false, searchUrl: CFG.searchUrl }, 56, 10), null);
});

test('previewURL: only the server\'s own preview path of the photo', () => {
  eq(SF.previewURL(photo('north')), '/api/skraafoto/preview/skraafotos2025/2025_83_36_1_0020_00003045');
  const bad = ['https://skraafoto-cdn.dataforsyningen.dk/skraafoto_server/x.tif', '//evil.example/api/skraafoto/preview/a/b',
    '/api/skraafoto/preview/skraafotos2025/../../config', '/api/skraafoto/preview/skraafotos2025/2025_1?token=x',
    '/api/skraafoto/preview/other/2025_1', 'javascript:alert(1)', '', null, 42];
  bad.forEach((u) => eq(SF.previewURL(Object.assign(photo('north'), { previewUrl: u })), null, String(u)));
  eq(SF.previewURL(null), null);
  const none = photo('east');
  delete none.previewUrl;
  eq(SF.previewURL(none), null, 'a photo without previewUrl has no preview');
});

test('viewerLink: only the official viewer, as "Full resolution"', () => {
  eq(SF.viewerLink(photo('north')), 'https://skraafoto.dataforsyningen.dk/?item=2025_83_36_1_0020_00003045');
  ['http://skraafoto.dataforsyningen.dk/?item=x', 'https://skraafoto.dataforsyningen.dk.evil.example/?item=x',
    'javascript:alert(1)', 'https://evil.example/?item=x', 'https://skraafoto.dataforsyningen.dk/?item=a b', null]
    .forEach((u) => eq(SF.viewerLink(Object.assign(photo('north'), { viewerUrl: u })), null, String(u)));
});

// ── 3. Directions ───────────────────────────────────────────────────────────

test('DIRECTIONS: the four oblique ones plus nadir, five in all', () => {
  eq(SF.DIRECTIONS, ['north', 'east', 'south', 'west', 'nadir']);
});

test('orderPhotos: fixed order, unknown and repeated directions dropped', () => {
  const got = SF.orderPhotos([photo('nadir', 2025, 5), photo('west', 2025, 4), photo('up'), photo('north', 2025, 1),
    photo('west', 2025, 9), null, photo('east', 2025, 2)]);
  eq(got.map((p) => p.direction), ['north', 'east', 'west', 'nadir']);
  eq(got[2].id, '2025_83_36_4_0020_00003045', 'the first west wins');
  eq(SF.orderPhotos(null), []);
});

test('pickDirection: the wanted one when it has a photo, else the first there is', () => {
  const all = SF.orderPhotos(SF.DIRECTIONS.map((d, i) => photo(d, 2025, i + 1)));
  eq(SF.pickDirection(all, 'south'), 'south');
  eq(SF.pickDirection(all, 'nadir'), 'nadir');
  eq(SF.pickDirection(all), 'north', 'default: north');
  const some = SF.orderPhotos([photo('west'), photo('nadir')]);
  eq(SF.pickDirection(some, 'south'), 'west', 'missing south → first in order');
  eq(SF.pickDirection(some, 'sideways'), 'west');
  eq(SF.pickDirection([], 'north'), null);
});

// ── 4. Years ────────────────────────────────────────────────────────────────

test('yearChoices: newest first, 2017 left out, junk dropped', () => {
  eq(GO.MinYear, 2019, 'the server leaves 2017 out');
  eq(SF.MIN_YEAR, GO.MinYear, 'the viewer\'s oldest year is the server\'s');
  eq(SF.yearChoices([2019, 2025, 2017, 2023, 2021, 2023, '2021', 2021.5, null]), [2025, 2023, 2021, 2019]);
  eq(SF.yearChoices(null), []);
});

test('selectedYear: the year searched when offered, else the newest', () => {
  eq(SF.selectedYear({ year: 2021, years: [2025, 2023, 2021, 2019] }), 2021);
  eq(SF.selectedYear({ year: 2017, years: [2025, 2017] }), 2025);
  eq(SF.selectedYear({ year: 2017, years: [2019, 2025, 2023] }), 2025, 'not offered → the newest, not the oldest');
  eq(SF.selectedYear({ year: 0, years: [2021, 2023] }), 2023, 'no year asked → the newest');
  eq(SF.selectedYear({ year: 0, years: [] }), 0);
});

// ── 5. Date and errors ──────────────────────────────────────────────────────

test('photoDate: the UTC date of the photo', () => {
  eq(SF.photoDate('2025-05-07T10:21:33Z'), '2025-05-07');
  eq(SF.photoDate('yesterday'), '');
  eq(SF.photoDate(null), '');
});

test('errorText: 429, 503, 404 and anything else in their own words', () => {
  assert.ok(/too many/i.test(SF.errorText(429)), SF.errorText(429));
  assert.ok(/30 s/.test(SF.errorText(429, 30)), 'Retry-After is told: ' + SF.errorText(429, 30));
  assert.ok(/busy|daily/i.test(SF.errorText(503)), SF.errorText(503));
  assert.ok(/not found/i.test(SF.errorText(404)), SF.errorText(404));
  const other = SF.errorText(502);
  assert.ok(/unavailable/i.test(other), other);
  eq(SF.errorText(0), other, 'a network failure reads as unavailable');
  assert.notStrictEqual(SF.errorText(429), SF.errorText(503));
});

// ── 6. Deep link ────────────────────────────────────────────────────────────

test('formatLink / parseLink round trip; junk is refused', () => {
  eq(SF.HASH_PARAM, 'skraafoto');
  const v = SF.formatLink({ lat: 56.156741, lon: 10.210761, dir: 'west', year: 2021 });
  eq(v, '56.15674,10.21076,west,2021');
  eq(SF.parseLink(v), { lat: 56.15674, lon: 10.21076, dir: 'west', year: 2021 });
  eq(SF.formatLink({ lat: 56.15674, lon: 10.21076, dir: 'north', year: 0 }), '56.15674,10.21076,north');
  eq(SF.parseLink('56.15674,10.21076,north'), { lat: 56.15674, lon: 10.21076, dir: 'north', year: 0 });
  eq(SF.parseLink('56.15674,10.21076'), { lat: 56.15674, lon: 10.21076, dir: null, year: 0 });
  ['', null, '56.1', '56.1,x', '91,10,north', '56,10,up', '56,10,north,2017', '56,10,north,abc', '56,10,north,2021,x', '1e1,10']
    .forEach((s) => eq(SF.parseLink(s), null, String(s)));
});

test('withParam: writes or removes only its own key, keeps the others', () => {
  eq(SF.withParam('#/nodes/abc?section=node-neighbors', '56.1,10.2,north'),
    '#/nodes/abc?section=node-neighbors&skraafoto=56.1%2C10.2%2Cnorth');
  eq(SF.withParam('#/nodes/abc?section=x&skraafoto=56.1%2C10.2%2Cnorth', null), '#/nodes/abc?section=x');
  eq(SF.withParam('#/nodes/abc?skraafoto=1', null), '#/nodes/abc');
  eq(SF.withParam('', '56.1,10.2,north'), '#?skraafoto=56.1%2C10.2%2Cnorth');
  eq(SF.withParam('#/map?profile=a,b', '1,2,east'), '#/map?profile=a%2Cb&skraafoto=1%2C2%2Ceast');
  eq(SF.linkFromHash('#/map?profile=x&skraafoto=56.1%2C10.2%2Csouth%2C2023'), { lat: 56.1, lon: 10.2, dir: 'south', year: 2023 });
  eq(SF.linkFromHash('#/map?profile=x'), null);
});

// ── 7. Credit ───────────────────────────────────────────────────────────────

test('the server\'s credit and privacy note are the owner\'s words', () => {
  eq(CREDIT.attribution, 'Klimadatastyrelsen, Skråfotos, CC BY 4.0', 'skraafoto.Attribution from its Go parts');
  eq(GO.PrivacyNote, 'Public aerial photos from Klimadatastyrelsen, shown at reduced size.');
  // Code only, as the #733 guard reads it: the comments may name the agency.
  const src = fs.readFileSync(path.join(__dirname, 'public', 'skraafoto.js'), 'utf8')
    .replace(/\/\*[\s\S]*?\*\//g, '').replace(/^\s*\/\/.*$/mg, '');
  [DHM.Owner, GO.PrivacyNote, GO.DatasetName, GO.DatasetURL].forEach((s) =>
    assert.strictEqual(src.indexOf(s), -1, 'public/skraafoto.js copies "' + s + '"; it must come from the server\'s block'));
});

test('creditLinks: dataset then licence (the #733/#765 ordering contract)', () => {
  const links = SF.creditLinks(CREDIT);
  eq(links.map((l) => [l.key, l.mark, l.href]), [
    ['dataset', 'Skråfotos', GO.DatasetURL],
    ['license', 'CC BY 4.0', DHM.LicenseURL],
  ]);
  assert.ok(/license/.test(links[1].rel), 'the licence link is rel=license');
  eq(SF.creditLinks(null), []);
});

// ── 8. Profile points ───────────────────────────────────────────────────────

/* A 101-sample free-point path from (56.17, 10.12) to (56.12, 10.22),
 * sampled the way the server samples: every stepM, with a shorter last
 * step to B. Unevenly spaced on purpose (#818): with even samples a
 * sample's fraction of the path equals i/(n-1), so a mapping that
 * counted samples instead of metres would pass. */
function profile() {
  const n = 101, step = 84, D = step * (n - 2) + 42, samples = [];
  for (let i = 0; i < n; i++) samples.push({ d: Math.min(step * i, D), dtm: 30, dsm: 32 });
  const end = (lat, lon) => ({ lat, lon, kind: 'point', groundM: 30, antennaM: 10, antennaSource: 'point-default', assumed: true });
  return {
    a: end(56.17, 10.12), b: end(56.12, 10.22), distanceM: D, stepM: step, sampleCount: n, model: 'both',
    coverage: { state: 'full', pct: 100, firstGapM: null }, samples,
    physics: { freqMHz: 869.618, freqSource: 'firmware', lambdaM: 0.34474, k: 1.3333333333333333, kSource: 'default', f1MaxM: 13.4 },
    verdict: { state: 'clear', model: 'dtm', criterion: 0.6, worstClearanceM: 5, worstAtM: 4200, worstF1Fraction: 1 },
  };
}

test('endPoint: A and B are the ends the server measured', () => {
  const data = profile();
  eq(TP.endPoint(data, 'a'), { lat: 56.17, lon: 10.12 });
  eq(TP.endPoint(data, 'b'), { lat: 56.12, lon: 10.22 });
  eq(TP.endPoint(data, 'c'), null);
  eq(TP.endPoint({ a: { lat: null, lon: 1 } }, 'a'), null);
  eq(TP.endPoint(null, 'a'), null);
});

test('pointAt: a chart sample is the #781 cursor\'s point on the drawn line', () => {
  const data = profile();
  const g = TP.chartGeometry(data);
  eq(TP.pointAt(data, g, 0), { lat: 56.17, lon: 10.12, distanceM: 0 });
  eq(TP.pointAt(data, g, 100), { lat: 56.12, lon: 10.22, distanceM: 8358 });
  const mid = TP.pointAt(data, g, 25);
  // Sample 25 is 2100 m along 8358 m: the fraction is in metres, not i/(n-1).
  const f = CUR.fraction(g, 25);
  assert.ok(Math.abs(f - 2100 / 8358) < 1e-12, 'fraction ' + f);
  const ll = CUR.latLonAt([56.17, 10.12], [56.12, 10.22], f);
  eq(mid, { lat: Math.round(ll[0] * 1e5) / 1e5, lon: Math.round(ll[1] * 1e5) / 1e5, distanceM: 2100 });
  const byCount = CUR.latLonAt([56.17, 10.12], [56.12, 10.22], 25 / 100);
  assert.notDeepStrictEqual([mid.lat, mid.lon], [Math.round(byCount[0] * 1e5) / 1e5, Math.round(byCount[1] * 1e5) / 1e5],
    'the fixture must tell metres from sample counts');
  const last = TP.pointAt(data, g, 99);
  const l99 = CUR.latLonAt([56.17, 10.12], [56.12, 10.22], 8316 / 8358);
  eq(last, { lat: Math.round(l99[0] * 1e5) / 1e5, lon: Math.round(l99[1] * 1e5) / 1e5, distanceM: 8316 }, 'the sample before the short last step');
  eq(TP.pointAt(data, g, -1), null);
  eq(TP.pointAt(data, g, 101), null);
  eq(TP.pointAt(data, null, 3), null);
});

test('chartIndexAt: a click x on the plot is the #781 nearest sample', () => {
  const data = profile();
  const g = TP.chartGeometry(data);
  const rect = { left: 100, width: 410 }; // the plot drawn at half its viewBox width
  eq(TP.chartIndexAt(g, rect, 100 + g.x[40] / 2), 40);
  eq(TP.chartIndexAt(g, rect, 0), 0);
  eq(TP.chartIndexAt(g, rect, 9999), 100);
  eq(TP.chartIndexAt(g, { left: 0, width: 0 }, 50), -1);
});

// ── 9. Object URLs of the open viewer (#818) ────────────────────────────────

/* A small DOM for open(): elements with listeners, attributes (src
 * mirrored), children and focus, all recorded so a test can find them by
 * data-testid and click them. */
function fakeDom() {
  const all = [];
  function element(tag) {
    const e = {
      tagName: String(tag).toUpperCase(), attrs: {}, children: [], listeners: {}, parentNode: null, style: {},
      hidden: false, disabled: false, value: '', className: '', _text: '',
      classList: { _c: new Set(), add(c) { this._c.add(c); }, remove(c) { this._c.delete(c); },
        toggle(c, on) { if (on) this._c.add(c); else this._c.delete(c); }, contains(c) { return this._c.has(c); } },
      setAttribute(k, v) { e.attrs[k] = String(v); },
      getAttribute(k) { return Object.prototype.hasOwnProperty.call(e.attrs, k) ? e.attrs[k] : null; },
      removeAttribute(k) { delete e.attrs[k]; },
      appendChild(c) { e.children.push(c); c.parentNode = e; return c; },
      removeChild(c) { e.children = e.children.filter((x) => x !== c); c.parentNode = null; return c; },
      addEventListener(t, f) { (e.listeners[t] = e.listeners[t] || []).push(f); },
      fire(t) { (e.listeners[t] || []).forEach((f) => f({ target: e, preventDefault() {}, stopImmediatePropagation() {} })); },
      focus() { doc.activeElement = e; },
      contains(x) { for (let n = x; n; n = n.parentNode) if (n === e) return true; return false; },
      querySelectorAll() { return []; },
    };
    Object.defineProperty(e, 'textContent', {
      get() { return e._text + e.children.map((c) => c.textContent).join(''); },
      set(v) { e._text = v == null ? '' : String(v); e.children = []; },
    });
    Object.defineProperty(e, 'innerHTML', { set(v) { e._text = String(v); } });
    Object.defineProperty(e, 'src', { get() { return e.getAttribute('src') || ''; }, set(v) { e.setAttribute('src', v); } });
    all.push(e);
    return e;
  }
  const doc = {
    createElement: element, activeElement: null,
    addEventListener() {}, removeEventListener() {},
    querySelector: () => null,
  };
  doc.body = element('body');
  return { doc, byTid: (t) => all.find((e) => e.getAttribute('data-testid') === t) };
}

/* The search and preview endpoints, as the M1 proxy answers. A preview
 * is held while svc.hold is set, until svc.release() lets the oldest go. */
function fakeService() {
  const svc = { search: [], preview: [], hold: false, held: [] };
  svc.fetch = (u) => {
    if (u.indexOf(CFG.searchUrl + '?') === 0) {
      svc.search.push(u);
      const year = Number(new URLSearchParams(u.split('?')[1]).get('year')) || 2025;
      const body = { collection: 'skraafotos' + year, year, years: [2025, 2023, 2021, 2019],
        photos: SF.DIRECTIONS.map((d, i) => photo(d, year, i + 1)), credit: CREDIT };
      return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(body) });
    }
    svc.preview.push(u);
    const answer = () => ({ ok: true, status: 200, blob: () => Promise.resolve({ of: u }) });
    if (!svc.hold) return Promise.resolve(answer());
    return new Promise((r) => svc.held.push(() => r(answer())));
  };
  svc.release = () => svc.held.shift()();
  return svc;
}

/* withViewer runs fn against a viewer opened on the fake DOM, with
 * URL.createObjectURL / revokeObjectURL counted, and restores the
 * sandbox after. */
async function withViewer(fn) {
  const dom = fakeDom();
  const svc = fakeService();
  const urls = { created: [], revoked: [], of: {} };
  const saved = { document: win.document, URL: win.URL };
  win.document = dom.doc;
  win.URL = {
    createObjectURL(b) { const u = 'blob:test/' + (urls.created.length + 1); urls.created.push(u); urls.of[b.of] = u; return u; },
    revokeObjectURL(u) { urls.revoked.push(u); },
  };
  const settle = () => new Promise((r) => setImmediate(r));
  const img = () => dom.byTid('sf-img');
  const t = {
    dom, svc, urls, settle,
    shown: () => img().getAttribute('src'),
    urlOf: (year, dir) => urls.of[SF.previewURL(photo(dir, year, SF.DIRECTIONS.indexOf(dir) + 1))],
    async tap(dir) { dom.byTid('sf-dir-' + dir).fire('click'); await settle(); t.check(); },
    async year(y) { const s = dom.byTid('sf-year'); s.value = String(y); s.fire('change'); await settle(); await settle(); t.check(); },
    // The photo on screen is never one already revoked.
    check() {
      const on = t.shown();
      assert.ok(!on || urls.revoked.indexOf(on) < 0, 'the photo on screen was revoked: ' + on);
    },
  };
  try {
    t.viewer = SF.open({ lat: 56.15674, lon: 10.21076, cfg: CFG, fetch: svc.fetch });
    assert.ok(t.viewer, 'the viewer opened');
    await settle(); await settle();
    await fn(t);
  } finally {
    if (t.viewer) t.viewer.close(false);
    win.document = saved.document;
    win.URL = saved.URL;
  }
}

/* Ten previews in one dialog, 2025 and 2023 in all five directions,
 * then back to 2025 north from the dialog's copy: on screen, and now the
 * oldest copy kept. */
async function tenThenBack(t) {
  for (const d of ['east', 'south', 'west', 'nadir']) await t.tap(d);
  await t.year(2023);
  for (const d of ['north', 'east', 'south', 'west']) await t.tap(d);
  eq(t.svc.preview.length, 10, 'ten previews fetched');
  await t.year(2025);
  await t.tap('north');
  eq(t.svc.preview.length, 10, 'back to 2025 north from the copy, not fetched again');
  eq(t.shown(), t.urlOf(2025, 'north'), '2025 north on screen');
  eq(t.urls.revoked, [], 'nothing revoked while ten are kept');
}

const asyncTests = [];
function testAsync(name, fn) { asyncTests.push([name, fn]); }

testAsync('object URLs: the eleventh preview drops 2025 north, revoked once it is off screen', () => withViewer(async (t) => {
  await tenThenBack(t);
  await t.year(2021); // 2021 north: the eleventh preview
  eq(t.svc.preview.length, 11);
  eq(t.shown(), t.urlOf(2021, 'north'), '2021 north on screen');
  eq(t.urls.revoked, [t.urlOf(2025, 'north')], 'the photo that left the screen, and only it');
  t.viewer.close(true);
  eq(t.urls.created.length, 11, 'one object URL per preview');
  eq(t.urls.revoked.slice().sort(), t.urls.created.slice().sort(), 'close revokes every one, each once');
}));

testAsync('object URLs: close revokes the photo on screen that was dropped from the copies', () => withViewer(async (t) => {
  await tenThenBack(t);
  t.svc.hold = true;
  await t.year(2021); // 2021 north is asked for and held
  await t.year(2019); // ... and is stale before it lands
  eq(t.svc.held.length, 2, '2021 and 2019 north held');
  t.svc.release(); // 2021 north lands: kept, not shown, and 2025 north drops out
  await t.settle();
  t.check();
  eq(t.shown(), t.urlOf(2025, 'north'), '2025 north is still the image source');
  eq(t.urls.revoked, [], 'not revoked while it is the image source');
  t.viewer.close(true);
  t.svc.release(); // 2019 north lands after close: no object URL for it
  await t.settle();
  eq(t.urls.created.length, 11);
  eq(t.shown(), null, 'close clears the image');
  eq(t.urls.revoked.slice().sort(), t.urls.created.slice().sort(), 'close revokes every one, each once');
}));

// ── 10. Only inside the coverage (#818) ─────────────────────────────────────

testAsync('node action: offered inside the block\'s bounds (Denmark), not outside', () => withViewer(async () => {
  eq(COVERAGE, [[54.4265, 7.99125], [57.778, 15.5995]], 'dhm.Coverage');
  const inDK = [{ lat: 56.15674, lon: 10.21076 }, { lat: 55.10, lon: 14.70 }, { lat: 57.72, lon: 10.58 },
    { lat: COVERAGE[0][0], lon: COVERAGE[0][1] }, { lat: COVERAGE[1][0], lon: COVERAGE[1][1] }];
  const outside = [{ lat: 37.888, lon: -122.268 }, { lat: 59.91, lon: 10.75 }, { lat: 52.52, lon: 13.40 },
    { lat: 56.15, lon: 7.99 }, { lat: 56.15, lon: 15.60 }, { lat: 54.42, lon: 10 }, { lat: 57.78, lon: 10 }];
  inDK.forEach((n) => assert.ok(SF.nodeAction(Object.assign({ name: 'x' }, n), CFG), 'no action at ' + JSON.stringify(n)));
  outside.forEach((n) => assert.strictEqual(SF.nodeAction(Object.assign({ name: 'x' }, n), CFG), null, 'action at ' + JSON.stringify(n)));
  const noBounds = Object.assign({}, CFG);
  delete noBounds.bounds;
  assert.ok(SF.nodeAction({ name: 'x', lat: 37.888, lon: -122.268 }, noBounds), 'a block without bounds sets no limit');
}));

testAsync('node action: the coverage test is the heritage overlay\'s, not a copy', () => withViewer(async () => {
  const hi = win.HeritageInfo;
  assert.strictEqual(typeof hi.inBounds, 'function', 'HeritageInfo.inBounds is exported');
  const asked = [];
  const real = hi.inBounds;
  hi.inBounds = (b, lat, lon) => { asked.push([b, lat, lon]); return false; };
  try {
    assert.strictEqual(SF.nodeAction({ name: 'x', lat: 56.15674, lon: 10.21076 }, CFG), null, 'its answer decides');
    eq(asked, [[COVERAGE, 56.15674, 10.21076]]);
  } finally { hi.inBounds = real; }
}));

(async () => {
  for (const [name, fn] of asyncTests) {
    try { await fn(); passed++; console.log('  ✅ ' + name); }
    catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
  }
  console.log('\n' + passed + ' passed, ' + failed + ' failed\n');
  process.exit(failed ? 1 : 0);
})();
