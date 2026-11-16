/**
 * Unit tests (#839, the rc19 test pass): the share card's twin words in
 * the Path profile and the Skråfoto viewer.
 *
 * The real modules run in the vm sandbox the other terrain-profile suites
 * use; nothing here copies production logic. The Go twins of the panel's
 * words are read from internal/profilecard/render.go, so the two cannot
 * drift apart.
 *
 * Covered:
 *   4. the chart's text names the verdict's worst point for what it is,
 *      the tightest point of the Fresnel zone, never "worst clearance";
 *   5. the profile's oblique-photo points: inside the Skråfoto block's
 *      bounds (Skraafoto.covered, the #818 test) and on a stretch DHM
 *      itself measured: not in a Copernicus fill range, not in a gap;
 *   9. a path wholly outside Denmark: the Go card's words, not "Denmark
 *      only";
 *  10. the empty state: no empty Year select, and the stage stacks its
 *      status under the photo instead of beside an empty frame;
 *  11. the drop line from the crosshair (on the ground) up to the line of
 *      sight (at antenna height), moved into a crop like the line, and the
 *      words that say why the two are apart.
 *
 * Usage: node test-issue-839-card-skraafoto.js
 */
'use strict';
const assert = require('assert');
const fs = require('fs');
const path = require('path');
const { loadTerrainProfile } = require('./tests/map-overlay-sandbox');
const { fakeDom } = require('./tests/skraafoto-fake-dom');

let passed = 0, failed = 0;
const asyncTests = [];
function test(name, fn) { asyncTests.push([name, fn]); }
function eq(actual, expected, msg) {
  const args = [JSON.parse(JSON.stringify(actual === undefined ? null : actual)), expected];
  if (msg !== undefined) args.push(msg);
  assert.deepStrictEqual(...args);
}

const { TP, win } = loadTerrainProfile(['terrain-profile-cursor.js', 'skraafoto.js', 'heritage-info.js']);
const SF = win.Skraafoto;
assert.ok(SF, 'public/skraafoto.js must define window.Skraafoto');

const RENDER_GO = fs.readFileSync(path.join(__dirname, 'internal', 'profilecard', 'render.go'), 'utf8');
const CSS = fs.readFileSync(path.join(__dirname, 'public', 'style.css'), 'utf8');

// The skraafoto block's bounds: dhm.Coverage, as dhmCoverageBounds() sends it.
const BOUNDS = [[54.4265, 7.99125], [57.778, 15.5995]];
const CFG = {
  enabled: true, searchUrl: '/api/skraafoto/search', bounds: BOUNDS,
  previewUrl: '/api/skraafoto/preview/{collection}/{item}', maxPx: 1024, note: 'n', credit: null,
};

/* A profile as the server sends it (cmd/server/terrain_profile.go
 * TerrainProfileResponse): D metres, n samples; o.fill the Copernicus
 * ranges (#738), o.gapFrom the first sample DHM has no data for (fill
 * off), o.none no DHM sample at all. */
function profile(o) {
  o = o || {};
  const D = o.D || 8000, n = 41;
  const samples = [];
  for (let i = 0; i < n; i++) {
    const d = Math.round(D * i / (n - 1) * 100) / 100;
    const gap = o.none || (o.gapFrom !== undefined && d >= o.gapFrom);
    samples.push({ d, dtm: gap ? null : 10 + (i % 5) });
  }
  const coverage = o.none ? { state: 'none', pct: 0, firstGapM: 0 }
    : o.gapFrom !== undefined ? { state: 'partial', pct: Math.round(1000 * o.gapFrom / D) / 10, firstGapM: o.gapFrom }
    : { state: 'full', pct: 100, firstGapM: null };
  if (o.fill) { coverage.mixed = true; coverage.fill = o.fill; }
  const verdict = o.none || o.gapFrom !== undefined
    ? { state: 'unknown', model: 'dtm', criterion: 0.6, worstClearanceM: null, worstAtM: null, worstF1Fraction: null }
    : { state: 'blocked', model: 'dtm', criterion: 0.6, worstClearanceM: -11.3, worstAtM: 5786.7, worstF1Fraction: -1.267 };
  return {
    a: Object.assign({ lat: 54.826, lon: 9.36, kind: 'point', groundM: 10, antennaM: 10, antennaSource: 'request', assumed: false }, o.a),
    b: Object.assign({ lat: 54.79, lon: 9.43, kind: 'point', groundM: 12, antennaM: 10, antennaSource: 'request', assumed: false }, o.b),
    distanceM: D, stepM: D / (n - 1), sampleCount: n, model: 'dtm', coverage, samples,
    physics: { freqMHz: 869.618, freqSource: 'firmware', lambdaM: 0.34474, k: 4 / 3, kSource: 'default', f1MaxM: 12 },
    verdict, attribution: 'Indeholder data fra …',
  };
}

// ── 4. The worst point's name ───────────────────────────────────────────────

console.log('\n=== #839 finding 4: the tightest Fresnel point ===\n');

test('the chart text names the tightest point of the Fresnel zone, as the card does', () => {
  const html = TP.buildChart(profile({ fill: [{ fromM: 3000, toM: 8000, source: 'copdem-glo30', resolutionM: 30 }] }), {});
  const title = /<title>([^<]*)<\/title>/.exec(html)[1];
  assert.ok(title.indexOf(' Tightest point for the Fresnel zone: clearance -11.3 m at 5.79 km along the path.') >= 0, title);
  assert.ok(!/worst clearance/i.test(title), title);
  // The Go twin (AltText) says it in the same words, as one of its
  // sentences (#850: they are joined with a space).
  assert.ok(RENDER_GO.indexOf('"Tightest point for the Fresnel zone: clearance " + fmtMetres(wc) + " at " + fmtDistance(at) + " along the path."') >= 0,
    'internal/profilecard/render.go AltText does not use the same words');
});

// ── 5. Where the photos are ─────────────────────────────────────────────────

console.log('\n=== #839 finding 5: oblique photos only where they are ===\n');

test('Padborg → Flensburg with the fill: A (Danish) has photos, B (in the fill) has none', () => {
  // The bounds rectangle takes in Flensburg: the fill range is what says it is abroad.
  const d = profile({ fill: [{ fromM: 3000, toM: 8000, source: 'copdem-glo30', resolutionM: 30 }] });
  assert.ok(SF.covered(CFG, d.b), 'the #818 bounds alone take in Flensburg (the reason for this fix)');
  eq(TP.photoCovered(d, CFG, TP.endPoint(d, 'a'), 0), true, 'A');
  eq(TP.photoCovered(d, CFG, TP.endPoint(d, 'b'), d.distanceM), false, 'B');
  eq(TP.photoCovered(d, CFG, { lat: 54.82, lon: 9.37 }, 1000), true, 'a chart point in Denmark');
  eq(TP.photoCovered(d, CFG, { lat: 54.80, lon: 9.41 }, 5000), false, 'a chart point in the fill');
  eq(TP.photoCovered(d, CFG, { lat: 54.81, lon: 9.39 }, 3000), false, 'the fill range includes its ends');
  eq(TP.dhmStretch(d), true, 'the Danish stretch is there to pick on');
});

test('without the fill: the gap is where DHM has no data', () => {
  const d = profile({ gapFrom: 3000 });
  eq(TP.photoCovered(d, CFG, TP.endPoint(d, 'a'), 0), true, 'A');
  eq(TP.photoCovered(d, CFG, TP.endPoint(d, 'b'), d.distanceM), false, 'B');
  eq(TP.photoCovered(d, CFG, { lat: 54.8, lon: 9.4 }, 2990), false, 'nearest sample in the gap');
  eq(TP.photoCovered(d, CFG, { lat: 54.82, lon: 9.37 }, 2700), true, 'nearest sample measured');
});

test('a path wholly in Skåne: no photo point at all', () => {
  const d = profile({ none: true, a: { lat: 55.605, lon: 13.0, groundM: null }, b: { lat: 55.65, lon: 13.1, groundM: null } });
  assert.ok(SF.covered(CFG, d.a) && SF.covered(CFG, d.b), 'the #818 bounds alone take in Malmö');
  eq(TP.photoCovered(d, CFG, TP.endPoint(d, 'a'), 0), false, 'A');
  eq(TP.photoCovered(d, CFG, TP.endPoint(d, 'b'), d.distanceM), false, 'B');
  eq(TP.dhmStretch(d), false, 'nothing to pick on');
});

test('the #818 bounds still apply, and a block without bounds sets no limit', () => {
  const d = profile({ a: { lat: 53.5, lon: 9.36 } });
  eq(TP.photoCovered(d, CFG, TP.endPoint(d, 'a'), 0), false, 'outside the bounds');
  eq(TP.photoCovered(d, Object.assign({}, CFG, { bounds: undefined }), TP.endPoint(d, 'a'), 0), true, 'an older server');
  eq(TP.photoCovered(d, CFG, null, 0), false, 'no point');
});

test('a Danish path end to end: every point has photos', () => {
  const d = profile({ a: { lat: 56.15, lon: 10.2 }, b: { lat: 56.16, lon: 10.1 } });
  eq(TP.photoCovered(d, CFG, TP.endPoint(d, 'a'), 0), true);
  eq(TP.photoCovered(d, CFG, TP.endPoint(d, 'b'), d.distanceM), true);
  eq(TP.dhmStretch(d), true);
});

// ── 9. A wholly foreign path ────────────────────────────────────────────────

console.log('\n=== #839 finding 9: a path wholly outside Denmark ===\n');

test('the words are the card\'s, and no longer claim "Denmark only"', () => {
  const m = /case "none":\s*\n(?:\s*\/\/[^\n]*\n)*\s*detail = "([^"]*)"/.exec(RENDER_GO);
  assert.ok(m, 'render.go mainChip has no "none" detail');
  const c = TP.verdictChip(profile({ none: true }));
  eq(c.detail, m[1]);
  eq(c.detail, 'This path lies entirely outside Denmark, so there is no terrain data for it.');
});

// ── 10/11. The viewer ──────────────────────────────────────────────────────

console.log('\n=== #839 findings 10, 11: the Skråfoto viewer ===\n');

const FRAME = { width: 1024, height: 765 };
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
const LINE = { a: { lat: 56.1, lon: 10.2 }, b: { lat: 56.2, lon: 10.2 }, ha: 20, hb: 20 };
const CROP = { box: { x0: 172.5, y0: 312.8, x1: 428.5, y1: 487.55 }, width: 1024, height: 699 };

/* A search answering photos (none with empty: true), the line and the
 * beam only when a path is asked for; previews and the M3b crop. */
function fakeService(o) {
  o = o || {};
  const svc = { search: [] };
  const headers = (h) => ({ get: (k) => (Object.prototype.hasOwnProperty.call(h, k) ? h[k] : null) });
  svc.fetch = (u) => {
    if (u.indexOf(CFG.searchUrl + '?') === 0) {
      svc.search.push(u);
      const line = new URLSearchParams(u.split('?')[1]).get('path');
      const extra = line ? { path: [{ x: 10, y: 700 }, { x: 1000, y: 80 }], beam: o.beam === undefined ? { x: 302, y: 380.6 } : o.beam } : {};
      const body = o.empty ? { collection: 'skraafotos2025', year: 0, years: [], photos: [], credit: null }
        : { collection: 'skraafotos2025', year: 2025, years: [2025, 2023], photos: SF.DIRECTIONS.map((d) => photo(d, extra)), credit: null };
      return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(body) });
    }
    if (u.indexOf('?zoom=1') > 0) {
      return Promise.resolve({ ok: true, status: 200,
        headers: headers({ 'X-CoreScope-Skraafoto-Marker': '512,349.5', 'X-CoreScope-Skraafoto-Crop': '172.5,312.8,428.5,487.55' }),
        blob: () => Promise.resolve({ of: u }) });
    }
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

async function withViewer(o, svcOpts, fn) {
  const dom = fakeDom();
  const svc = fakeService(svcOpts);
  const saved = { document: win.document, URL: win.URL };
  win.document = dom.doc;
  let n = 0;
  win.URL = { createObjectURL: () => 'blob:test/' + (++n), revokeObjectURL() {} };
  const settle = async () => { for (let i = 0; i < 4; i++) await new Promise((r) => setImmediate(r)); };
  const img = () => dom.byTid('sf-img');
  const t = {
    dom, svc, settle,
    marks: () => dom.byTid('sf-marks'),
    mark: (tid) => find(dom.byTid('sf-marks'), (e) => e.attrs['data-testid'] === tid)[0] || null,
    load(w, h) { img().naturalWidth = w; img().naturalHeight = h; img().onload(); },
    async click(tid) { dom.byTid(tid).fire('click'); await settle(); },
  };
  let viewer = null;
  try {
    viewer = SF.open(Object.assign({ lat: 56.15, lon: 10.2, cfg: CFG, fetch: svc.fetch }, o));
    assert.ok(viewer, 'the viewer opened');
    await settle();
    await fn(t);
  } finally {
    if (viewer) viewer.close(false);
    win.document = saved.document;
    win.URL = saved.URL;
  }
}

test('10: an empty answer shows no empty Year select', () => withViewer({}, { empty: true }, async (t) => {
  eq(t.dom.byTid('sf-dialog').getAttribute('data-state'), 'empty');
  eq(t.dom.byTid('sf-status').textContent, 'No oblique photos at this point.');
  eq(t.dom.byTid('sf-year-wrap').hidden, true, 'the Year field is hidden with no year to pick');
}));

test('10: a photo with years shows the Year field again', () => withViewer({}, {}, async (t) => {
  eq(t.dom.byTid('sf-year-wrap').hidden, false);
}));

test('10: the stage stacks the status under the photo frame, centred', () => {
  const m = /\n\.sf-stage \{([^}]*)\}/.exec(CSS);
  assert.ok(m, 'no .sf-stage rule');
  assert.ok(/flex-direction:\s*column;/.test(m[1]), '.sf-stage is a row: the empty frame pushes the status off centre');
  assert.ok(/\.sf-year-wrap\[hidden\]\s*\{\s*display:\s*none;\s*\}/.test(CSS), '.sf-year-wrap is inline-flex, which beats [hidden]');
});

test('11: marks carry the beam point, moved into a crop like the line', () => {
  const p = photo('east', { path: [{ x: 10, y: 700 }, { x: 1000, y: 80 }], beam: { x: 302, y: 380.6 } });
  eq(SF.marks(p, null).beam, { x: 302, y: 380.6 });
  const z = SF.marks(p, Object.assign({ marker: { x: 512, y: 349.5 } }, CROP));
  eq(z.beam, JSON.parse(JSON.stringify(SF.toCrop({ x: 302, y: 380.6 }, CROP))));
  eq(SF.marks(photo('east', { beam: { x: 302, y: 380.6 } }), null).beam, null, 'no beam without a line');
  eq(SF.marks(photo('east', { path: [{ x: 10, y: 700 }, { x: 1000, y: 80 }], beam: { x: 'a' } }), null).beam, null, 'junk is dropped');
});

test('11: from a profile, a dotted drop joins the crosshair to the line, and the words say why', () => withViewer(
  { line: LINE }, {}, async (t) => {
    t.load(1024, 765);
    const drop = t.mark('sf-drop');
    assert.ok(drop, 'no drop line');
    eq([drop.getAttribute('x1'), drop.getAttribute('y1'), drop.getAttribute('x2'), drop.getAttribute('y2')], ['300.5', '400.2', '302', '380.6']);
    assert.ok(/at antenna height/.test(t.marks().getAttribute('aria-label')), t.marks().getAttribute('aria-label'));
    const note = t.dom.byTid('sf-los-note');
    eq(note.hidden, false);
    eq(note.textContent, 'The line of sight is drawn at antenna height, above the ground; the dotted line drops from it to the point on the ground.');
    await t.click('sf-zoom');
    t.load(1024, 699);
    const z = t.mark('sf-drop');
    const b = SF.toCrop({ x: 302, y: 380.6 }, CROP);
    eq([z.getAttribute('x1'), z.getAttribute('y1'), z.getAttribute('x2'), z.getAttribute('y2')],
      ['512', '349.5', String(Math.round(b.x * 100) / 100), String(Math.round(b.y * 100) / 100)], 'zoomed: marker to the beam in the crop');
  }));

test('11: an answer without a beam still says why the line is apart from the point', () => withViewer(
  { line: LINE }, { beam: null }, async (t) => {
    t.load(1024, 765);
    eq(t.mark('sf-drop'), null);
    eq(t.dom.byTid('sf-los-note').textContent, 'The line of sight is drawn at antenna height, so it passes above the point on the ground.');
  }));

test('11: from a node (no line): no drop and no note', () => withViewer({}, {}, async (t) => {
  t.load(1024, 765);
  eq(t.mark('sf-drop'), null);
  eq(t.dom.byTid('sf-los-note').hidden, true);
}));

(async () => {
  for (const [name, fn] of asyncTests) {
    try { await fn(); passed++; console.log('  ✅ ' + name); }
    catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
  }
  console.log('\n' + passed + ' passed, ' + failed + ' failed\n');
  process.exit(failed ? 1 : 0);
})();
