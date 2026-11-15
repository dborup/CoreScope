/**
 * Unit tests (#737 M3c): the Skråfoto viewer's marks — the crosshair on the
 * point, the line of sight A→B and the zoomed crop — in public/skraafoto.js,
 * and the line the Path profile hands it (public/terrain-profile.js).
 *
 * The real modules run in the vm sandbox the other terrain-profile suites
 * use; nothing here copies production logic. The shapes are the server's:
 * M3a's search fields (frame, point, inFrame, path, approx, in preview
 * pixels, cmd/server/skraafoto_proxy.go SkraafotoPhoto) and M3b's crop
 * headers (cmd/server/skraafoto_zoom.go: X-CoreScope-Skraafoto-Marker
 * "x,y" in crop pixels, X-CoreScope-Skraafoto-Crop "x0,y0,x1,y1" in
 * preview pixels).
 *
 * Covered:
 *   1. preview → displayed image: the object-fit: contain mapping the SVG
 *      layer shares (letterboxed either way, any size);
 *   2. crop → displayed image: preview pixels into the crop by its box,
 *      the marker and box headers parsed strictly;
 *   3. the marks of a photo, shown and zoomed: the crosshair only in frame,
 *      approx carried, the line only with two points;
 *   4. the zoom URL: only for a point in frame, in the server's grammar;
 *   5. the default direction: the oblique most perpendicular to A–B;
 *   6. the search carries the line (path, ha, hb) in the server's grammar,
 *      and the profile hands the line it measured;
 *   7. text equivalents: where the point is, and that a line is drawn;
 *   8. the viewer: the line is asked for, the perpendicular photo shown
 *      first, the crop fetched only on a click on a real aria-pressed
 *      button, zoom off again on another direction, the marks drawn as
 *      SVG with the data in attributes, every object URL revoked.
 *
 * Usage: node test-issue-737-m3c-skraafoto-marks.js
 */
'use strict';
const assert = require('assert');
const { loadTerrainProfile } = require('./tests/map-overlay-sandbox');
const { fakeDom } = require('./tests/skraafoto-fake-dom');

let passed = 0, failed = 0;
const asyncTests = [];
function test(name, fn) { asyncTests.push([name, fn]); }
function eq(actual, expected, msg) {
  assert.deepStrictEqual(JSON.parse(JSON.stringify(actual === undefined ? null : actual)), expected, msg);
}
function near(a, b, msg, tol) {
  assert.ok(Math.abs(a - b) <= (tol || 1e-9), (msg || '') + ': ' + a + ' vs ' + b);
}

const { TP, win } = loadTerrainProfile(['terrain-profile-cursor.js', 'skraafoto.js', 'heritage-info.js']);
const SF = win.Skraafoto;
assert.ok(SF, 'public/skraafoto.js must define window.Skraafoto');

const CFG = {
  enabled: true, searchUrl: '/api/skraafoto/search',
  previewUrl: '/api/skraafoto/preview/{collection}/{item}', maxPx: 1024, note: 'n', credit: null,
};
const FRAME = { width: 1024, height: 765 };

// One photo as M3a's search returns it.
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

console.log('\n=== #737 M3c Skråfoto marks ===\n');

// ── 1. Preview → displayed image ────────────────────────────────────────────

test('displayMap: a box wider than the photo letterboxes left and right (contain)', () => {
  // 1024x765 shown in a 732x400 box: height-bound, scale 400/765.
  const m = SF.displayMap(FRAME, { width: 732, height: 400 });
  near(m.scale, 400 / 765, 'scale');
  near(m.x, (732 - 1024 * 400 / 765) / 2, 'x offset');
  near(m.y, 0, 'y offset');
  const p = SF.toDisplay({ x: 512, y: 382.5 }, m);
  near(p.x, 366, 'the centre maps to the centre x');
  near(p.y, 200, 'the centre maps to the centre y');
  const c = SF.toDisplay({ x: 1024, y: 765 }, m);
  near(c.x, m.x + 1024 * m.scale, 'far corner x');
  near(c.y, 400, 'far corner y');
});

test('displayMap: a box taller than the photo letterboxes top and bottom', () => {
  const m = SF.displayMap(FRAME, { width: 343, height: 400 });
  near(m.scale, 343 / 1024, 'width-bound scale');
  near(m.x, 0, 'x offset');
  near(m.y, (400 - 765 * 343 / 1024) / 2, 'y offset');
  const p = SF.toDisplay({ x: 0, y: 0 }, m);
  near(p.y, m.y, 'the top-left corner sits at the letterbox edge');
});

test('displayMap: an exact fit has no offset; nothing for a missing or empty size', () => {
  const m = SF.displayMap({ width: 480, height: 320 }, { width: 960, height: 640 });
  eq([m.scale, m.x, m.y], [2, 0, 0]);
  [[null, { width: 1, height: 1 }], [FRAME, null], [{ width: 0, height: 5 }, { width: 1, height: 1 }],
    [FRAME, { width: 0, height: 0 }], [{ width: NaN, height: 5 }, { width: 1, height: 1 }]]
    .forEach(([s, b]) => eq(SF.displayMap(s, b), null, JSON.stringify([s, b])));
});

// ── 2. Crop → displayed image ───────────────────────────────────────────────

test('parseMarker / parseCropBox: the M3b headers, strictly', () => {
  eq(SF.parseMarker('512.25,349.5'), { x: 512.25, y: 349.5 });
  eq(SF.parseMarker('0,0'), { x: 0, y: 0 });
  ['', null, '1', '1,2,3', 'a,b', '1e3,2', '-1,2', ' 1,2', 'NaN,1', 'Infinity,1', '1,,2']
    .forEach((v) => eq(SF.parseMarker(v), null, String(v)));
  eq(SF.parseCropBox('128,64,384,238.75'), { x0: 128, y0: 64, x1: 384, y1: 238.75 });
  ['', null, '1,2,3', '10,0,5,8', '0,10,8,10', 'a,0,1,1', '-1,0,4,4', '0,0,1e3,4']
    .forEach((v) => eq(SF.parseCropBox(v), null, String(v)));
});

test('toCrop: preview pixels into the crop by its box and size', () => {
  // A 256x174.75 preview box cut 4x finer: a 1024x699 crop.
  const crop = { box: { x0: 128, y0: 64, x1: 384, y1: 238.75 }, width: 1024, height: 699 };
  eq(SF.toCrop({ x: 128, y: 64 }, crop), { x: 0, y: 0 });
  eq(SF.toCrop({ x: 384, y: 238.75 }, crop), { x: 1024, y: 699 });
  const p = SF.toCrop({ x: 300.5, y: 100 }, crop);
  near(p.x, (300.5 - 128) * 4, 'x');
  near(p.y, (100 - 64) * 699 / 174.75, 'y, by its own scale');
  // Each axis by its own scale: a box cut 4x across and 2x down.
  const odd = { box: { x0: 100, y0: 50, x1: 200, y1: 150 }, width: 400, height: 200 };
  eq(SF.toCrop({ x: 150, y: 100 }, odd), { x: 200, y: 100 });
  eq(SF.toCrop({ x: 1, y: 1 }, { box: crop.box, width: 0, height: 5 }), null);
  eq(SF.toCrop(null, crop), null);
});

test('crop → displayed: the crop marker lands where the zoomed photo shows the point', () => {
  const crop = { box: { x0: 172.5, y0: 312.8, x1: 428.5, y1: 487.55 }, width: 1024, height: 699 };
  const m = SF.displayMap({ width: crop.width, height: crop.height }, { width: 732, height: 500 });
  const pt = SF.toDisplay(SF.toCrop({ x: 300.5, y: 400.2 }, crop), m);
  near(pt.x, m.x + (300.5 - 172.5) * 4 * m.scale, 'x', 1e-6);
  near(pt.y, m.y + (400.2 - 312.8) * 4 * m.scale, 'y', 1e-6);
});

// ── 3. The marks of a photo ─────────────────────────────────────────────────

test('marks: the crosshair at the point, in preview space, only when inFrame', () => {
  eq(SF.marks(photo('north')), { width: 1024, height: 765, point: { x: 300.5, y: 400.2 }, path: null, approx: false });
  eq(SF.marks(photo('north', { approx: true })).approx, true, 'approx carried');
  eq(SF.marks(photo('north', { inFrame: false, point: { x: -40, y: 10 } })).point, null, 'out of frame: no crosshair');
  eq(SF.marks(photo('north', { point: undefined, inFrame: false })).point, null);
  eq(SF.marks(photo('north', { point: { x: 'x', y: 1 } })).point, null, 'a point that is not a number');
  eq(SF.marks(photo('north', { frame: undefined, point: undefined })), null, 'no camera, no marks');
  eq(SF.marks(null), null);
});

test('marks: the line of sight only with at least two points', () => {
  const path = [{ x: 0, y: 700 }, { x: 512, y: 380 }, { x: 1024, y: 60.5 }];
  eq(SF.marks(photo('east', { path })).path, path);
  eq(SF.marks(photo('east', { path: [{ x: 1, y: 2 }] })).path, null, 'one point is no line');
  eq(SF.marks(photo('east', { path: [{ x: 1, y: 2 }, { x: 'a', y: 3 }] })).path, null, 'junk is no line');
  eq(SF.marks(photo('east', { path: 'x' })).path, null);
  eq(SF.marks(photo('east', { inFrame: false, path })).path, path, 'the line can be in frame when the point is not');
});

test('marks: zoomed, the crop marker and the line moved into crop pixels', () => {
  const path = [{ x: 172.5, y: 312.8 }, { x: 428.5, y: 487.55 }];
  const crop = { box: { x0: 172.5, y0: 312.8, x1: 428.5, y1: 487.55 }, width: 1024, height: 699, marker: { x: 512, y: 349.6 } };
  const m = SF.marks(photo('south', { path, approx: true }), crop);
  eq(m, { width: 1024, height: 699, point: { x: 512, y: 349.6 }, path: [{ x: 0, y: 0 }, { x: 1024, y: 699 }], approx: true });
  eq(SF.marks(photo('south'), Object.assign({}, crop, { marker: null })).point, null, 'no marker header, no crosshair');
  eq(SF.marks(photo('south', { path }), Object.assign({}, crop, { box: null })).path, null, 'no box header, no line');
  eq(SF.marks(photo('south'), Object.assign({}, crop, { marker: { x: 1100, y: 3 } })).point, null, 'a marker outside the crop');
});

// ── 4. The zoom URL ─────────────────────────────────────────────────────────

test('zoomURL: the preview with zoom=1 and the point, only for a point in frame', () => {
  eq(SF.zoomURL(photo('north')), '/api/skraafoto/preview/skraafotos2025/2025_83_36_1_0020_00003045?zoom=1&x=300.5&y=400.2');
  eq(SF.zoomURL(photo('north', { point: { x: 0, y: 764.999 } })), '/api/skraafoto/preview/skraafotos2025/2025_83_36_1_0020_00003045?zoom=1&x=0&y=765');
  eq(SF.zoomURL(photo('north', { point: { x: -0.001, y: 3 } })), '/api/skraafoto/preview/skraafotos2025/2025_83_36_1_0020_00003045?zoom=1&x=0&y=3', 'a rounded -0 is plain 0');
  eq(SF.zoomURL(photo('north', { inFrame: false })), null);
  eq(SF.zoomURL(photo('north', { point: undefined })), null);
  eq(SF.zoomURL(photo('north', { previewUrl: 'https://evil.example/x' })), null, 'only the server\'s own preview');
  eq(SF.zoomURL(photo('north', { point: { x: 2000, y: 3 }, frame: { width: 3000, height: 2000 } })), null, 'beyond the server\'s 1024 px bound');
});

// ── 5. The default direction ────────────────────────────────────────────────

const LINE_EW = { a: { lat: 56.15, lon: 10.10 }, b: { lat: 56.15, lon: 10.30 } };
const LINE_NS = { a: { lat: 56.10, lon: 10.20 }, b: { lat: 56.20, lon: 10.20 } };
const all = SF.DIRECTIONS.map((d) => photo(d));

test('perpendicularDirection: east–west line → looking north (or south)', () => {
  eq(SF.perpendicularDirection(LINE_EW, all), 'north');
  eq(SF.perpendicularDirection(LINE_EW, all.filter((p) => p.direction !== 'north')), 'south');
});

test('perpendicularDirection: north–south line → looking east (or west)', () => {
  eq(SF.perpendicularDirection(LINE_NS, all), 'east');
  eq(SF.perpendicularDirection({ a: LINE_NS.b, b: LINE_NS.a }, all.filter((p) => p.direction !== 'east')), 'west');
});

test('perpendicularDirection: by ground distance, not degrees (cos latitude)', () => {
  // 0.1° lat × 0.15° lon at 56°: 11.1 km north, 9.4 km east. In degrees the
  // line is nearer east–west (56° from north); on the ground nearer north–
  // south (40°), so the side view looks east.
  eq(SF.perpendicularDirection({ a: { lat: 56.10, lon: 10.10 }, b: { lat: 56.20, lon: 10.25 } }, all), 'east');
  // A line 60° from north on the ground: north is more perpendicular.
  const k = Math.cos(56.15 * Math.PI / 180);
  const dlat = 0.05, dlon = dlat * Math.tan(60 * Math.PI / 180) / k;
  eq(SF.perpendicularDirection({ a: { lat: 56.10, lon: 10.1 }, b: { lat: 56.10 + dlat, lon: 10.1 + dlon } }, all), 'north');
});

test('perpendicularDirection: nadir never, nothing without a line or an oblique', () => {
  eq(SF.perpendicularDirection(LINE_EW, [photo('nadir'), photo('east')]), 'east', 'the only oblique');
  eq(SF.perpendicularDirection(LINE_EW, [photo('nadir')]), null);
  eq(SF.perpendicularDirection(null, all), null);
  eq(SF.perpendicularDirection({ a: LINE_EW.a, b: LINE_EW.a }, all), null, 'a line of no length');
  eq(SF.perpendicularDirection(LINE_EW, []), null);
});

// ── 6. The line in the search ───────────────────────────────────────────────

test('sightLine: two points and the antenna heights, rounded as the server takes them', () => {
  eq(SF.sightLine({ a: { lat: 56.170001, lon: 10.12 }, b: { lat: '56.12', lon: 10.22 }, ha: 10, hb: 2.25 }),
    { a: { lat: 56.17, lon: 10.12 }, b: { lat: 56.12, lon: 10.22 }, ha: 10, hb: 2.25 });
  eq(SF.sightLine({ a: { lat: 56.17, lon: 10.12 }, b: { lat: 56.12, lon: 10.22 } }),
    { a: { lat: 56.17, lon: 10.12 }, b: { lat: 56.12, lon: 10.22 }, ha: null, hb: null }, 'heights optional');
  eq(SF.sightLine({ a: { lat: 56.17, lon: 10.12 }, b: { lat: 56.12, lon: 10.22 }, ha: 101, hb: -1 }).ha, null, 'out of range: the server\'s default');
  [null, {}, { a: { lat: 56, lon: 10 } }, { a: { lat: 91, lon: 10 }, b: { lat: 56, lon: 10 } }]
    .forEach((l) => eq(SF.sightLine(l), null, JSON.stringify(l)));
});

test('searchURL: path=latA,lonA,latB,lonB with ha and hb, only with a line', () => {
  const line = SF.sightLine({ a: { lat: 56.17, lon: 10.12 }, b: { lat: 56.12, lon: 10.22 }, ha: 10, hb: 2.25 });
  eq(SF.searchURL(CFG, 56.15, 10.2, 0, line),
    '/api/skraafoto/search?lat=56.15000&lon=10.20000&path=56.17000,10.12000,56.12000,10.22000&ha=10&hb=2.25');
  eq(SF.searchURL(CFG, 56.15, 10.2, 2023, SF.sightLine({ a: line.a, b: line.b })),
    '/api/skraafoto/search?lat=56.15000&lon=10.20000&year=2023&path=56.17000,10.12000,56.12000,10.22000', 'no heights: none sent');
  eq(SF.searchURL(CFG, 56.15, 10.2), '/api/skraafoto/search?lat=56.15000&lon=10.20000', 'unchanged without a line');
});

test('TerrainProfile.sightLine: the profile\'s ends and the antenna heights it used', () => {
  const end = (lat, lon, h) => ({ lat, lon, kind: 'point', groundM: 30, antennaM: h, antennaSource: 'point-default' });
  eq(TP.sightLine({ a: end(56.17, 10.12, 10), b: end(56.12, 10.22, 2.5) }),
    { a: { lat: 56.17, lon: 10.12 }, b: { lat: 56.12, lon: 10.22 }, ha: 10, hb: 2.5 });
  eq(TP.sightLine({ a: end(56.17, 10.12, 10), b: end(56.12, 10.22, null) }).hb, null);
  eq(TP.sightLine({ a: end(56.17, 10.12, 10) }), null);
  eq(TP.sightLine(null), null);
});

// ── 7. Text equivalents ─────────────────────────────────────────────────────

test('placeWords: a 3x3 grid of the frame', () => {
  const W = 900, H = 600;
  eq(SF.placeWords({ x: 450, y: 300 }, W, H), 'centre');
  eq(SF.placeWords({ x: 100, y: 300 }, W, H), 'centre-left');
  eq(SF.placeWords({ x: 800, y: 300 }, W, H), 'centre-right');
  eq(SF.placeWords({ x: 100, y: 50 }, W, H), 'top-left');
  eq(SF.placeWords({ x: 450, y: 50 }, W, H), 'top-centre');
  eq(SF.placeWords({ x: 899, y: 599 }, W, H), 'bottom-right');
  eq(SF.placeWords({ x: 450, y: 590 }, W, H), 'bottom-centre');
});

test('markText: the point, how sure, and the line', () => {
  eq(SF.markText({ width: 900, height: 600, point: { x: 100, y: 300 }, path: null, approx: false }),
    'Point marked at the centre-left.');
  eq(SF.markText({ width: 900, height: 600, point: { x: 450, y: 300 }, path: null, approx: true }),
    'Point marked at the centre, position approximate.');
  eq(SF.markText({ width: 900, height: 600, point: null, path: [{ x: 0, y: 0 }, { x: 9, y: 9 }], approx: false }),
    'The point is outside this photo. Line of sight from A to B drawn on the photo.');
  eq(SF.markText({ width: 900, height: 600, point: { x: 800, y: 50 }, path: [{ x: 0, y: 0 }, { x: 9, y: 9 }], approx: false }),
    'Point marked at the top-right. Line of sight from A to B drawn on the photo.');
  eq(SF.markText(null), '');
});

// ── 8. The viewer ───────────────────────────────────────────────────────────

/* The search (M3a shape, the line echoed only when asked for) and the
 * preview and crop (M3b headers) endpoints. */
function fakeService(extra) {
  const svc = { search: [], preview: [], zoom: [], fail: null };
  const headers = (h) => ({ get: (k) => (Object.prototype.hasOwnProperty.call(h, k) ? h[k] : null) });
  svc.fetch = (u) => {
    if (u.indexOf(CFG.searchUrl + '?') === 0) {
      svc.search.push(u);
      const q = new URLSearchParams(u.split('?')[1]);
      const path = q.get('path') ? [{ x: 10, y: 700 }, { x: 1000, y: 80 }] : undefined;
      const body = { collection: 'skraafotos2025', year: 2025, years: [2025, 2023],
        photos: SF.DIRECTIONS.map((d) => photo(d, Object.assign({ path }, extra))), credit: null };
      return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(body) });
    }
    if (u.indexOf('?zoom=1') > 0) {
      svc.zoom.push(u);
      if (svc.fail) return Promise.resolve({ ok: false, status: svc.fail, headers: headers({}) });
      return Promise.resolve({ ok: true, status: 200,
        headers: headers({ 'X-CoreScope-Skraafoto-Marker': '512,349.5', 'X-CoreScope-Skraafoto-Crop': '172.5,312.8,428.5,487.55' }),
        blob: () => Promise.resolve({ of: u }) });
    }
    svc.preview.push(u);
    return Promise.resolve({ ok: true, status: 200, headers: headers({}), blob: () => Promise.resolve({ of: u }) });
  };
  return svc;
}

/* withViewer opens a viewer with o on a fake DOM and service; extra is
 * merged into every photo the search answers. */
async function withViewer(o, fn, extra) {
  const dom = fakeDom();
  const svc = fakeService(extra);
  const urls = { created: [], revoked: [] };
  const saved = { document: win.document, URL: win.URL };
  win.document = dom.doc;
  win.URL = {
    createObjectURL(b) { const u = 'blob:test/' + (urls.created.length + 1) + '#' + b.of; urls.created.push(u); return u; },
    revokeObjectURL(u) { urls.revoked.push(u); },
  };
  const settle = async () => { for (let i = 0; i < 4; i++) await new Promise((r) => setImmediate(r)); };
  const img = () => dom.byTid('sf-img');
  const t = {
    dom, svc, urls, settle, img,
    zoom: () => dom.byTid('sf-zoom'),
    marks: () => dom.byTid('sf-marks'),
    // An element of the marks now on screen (byTid also finds removed ones).
    mark: (tid) => find(dom.byTid('sf-marks'), (e) => e.attrs['data-testid'] === tid)[0] || null,
    /* load is the browser's load event of the image on screen, at its
     * natural size. */
    load(w, h) { img().naturalWidth = w; img().naturalHeight = h; img().onload(); },
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
  }
}

function find(e, pred, out) {
  out = out || [];
  if (pred(e)) out.push(e);
  (e.children || []).forEach((c) => find(c, pred, out));
  return out;
}
const byClass = (root, c) => find(root, (e) => (' ' + (e.attrs.class || e.className || '') + ' ').indexOf(' ' + c + ' ') >= 0);

test('viewer from a profile: the search asks for the line; the perpendicular photo is shown first', () => withViewer(
  { line: LINE_NS }, async (t) => {
    eq(t.svc.search.length, 1);
    assert.ok(/&path=56\.10000,10\.20000,56\.20000,10\.20000$/.test(t.svc.search[0]), t.svc.search[0]);
    eq(t.viewer.state().dir, 'east', 'a north–south line is seen from the side: looking east');
    eq(t.svc.preview, [photo('east').previewUrl], 'only that photo fetched');
  }));

test('viewer from a node: no line asked for, north first as before', () => withViewer({}, async (t) => {
  assert.ok(t.svc.search[0].indexOf('path=') < 0, t.svc.search[0]);
  eq(t.viewer.state().dir, 'north');
}));

test('viewer: a deep-linked direction wins over the perpendicular one', () => withViewer(
  { line: LINE_NS, dir: 'south' }, async (t) => {
    eq(t.viewer.state().dir, 'south');
  }));

test('viewer: the marks are an SVG layer in preview space, with the crosshair and the line', () => withViewer(
  { line: LINE_NS }, async (t) => {
    t.load(1024, 765);
    const svg = t.marks();
    eq(svg.tagName, 'SVG');
    eq(svg.namespaceURI, 'http://www.w3.org/2000/svg');
    eq(svg.hidden, false);
    eq(svg.getAttribute('viewBox'), '0 0 1024 765');
    eq(svg.getAttribute('preserveAspectRatio'), 'xMidYMid meet', 'the image is object-fit: contain');
    eq(svg.getAttribute('role'), 'img');
    eq(svg.getAttribute('aria-label'), 'Point marked at the centre-left. Line of sight from A to B drawn on the photo.');
    const cross = t.mark('sf-cross');
    eq([cross.getAttribute('data-x'), cross.getAttribute('data-y')], ['300.5', '400.2']);
    eq(cross.getAttribute('transform'), 'translate(300.5 400.2)');
    const los = t.mark('sf-los');
    eq(los.getAttribute('points'), '10,700 1000,80');
    eq(byClass(svg, 'sf-cross-approx').length, 0, 'not approximate');
  }));

test('viewer: an approximate point is drawn dashed', () => withViewer({}, async (t) => {
  t.load(1024, 765);
  eq(byClass(t.marks(), 'sf-cross-approx').length, 1, 'dashed when approx');
  eq(t.marks().getAttribute('aria-label'), 'Point marked at the centre-left, position approximate.');
}, { approx: true }));

test('viewer: out of frame, no crosshair and no zoom', () => withViewer({}, async (t) => {
  t.load(1024, 765);
  eq(t.mark('sf-cross'), null, 'no crosshair out of frame');
  eq(t.marks().getAttribute('aria-label'), 'The point is outside this photo.');
  eq(t.zoom().disabled, true);
  eq(t.zoom().getAttribute('aria-pressed'), 'false');
  await t.click('sf-zoom');
  eq(t.svc.zoom, [], 'a disabled toggle asks nothing');
}, { inFrame: false }));

test('viewer: zoom is a real button with aria-pressed; the crop is fetched only on a click', () => withViewer(
  { line: LINE_NS }, async (t) => {
    t.load(1024, 765);
    const z = t.zoom();
    eq(z.tagName, 'BUTTON');
    eq(z.type, 'button');
    eq(z.getAttribute('aria-pressed'), 'false');
    eq(z.textContent, 'Zoom in');
    eq(z.disabled, false);
    eq(t.svc.zoom, [], 'nothing zoomed before a click');
    await t.click('sf-zoom');
    eq(t.svc.zoom, [photo('east').previewUrl + '?zoom=1&x=300.5&y=400.2']);
    eq(z.getAttribute('aria-pressed'), 'true');
    eq(z.textContent, 'Zoom out');
    t.load(1024, 699);
    eq(t.marks().getAttribute('viewBox'), '0 0 1024 699', 'crop space');
    const cross = t.mark('sf-cross');
    eq([cross.getAttribute('data-x'), cross.getAttribute('data-y')], ['512', '349.5'], 'the marker header');
    // The line (10,700)-(1000,80) in preview pixels, moved into the crop.
    const want = [{ x: 10, y: 700 }, { x: 1000, y: 80 }].map((p) => SF.toCrop(p, { box: { x0: 172.5, y0: 312.8, x1: 428.5, y1: 487.55 }, width: 1024, height: 699 }));
    eq(t.mark('sf-los').getAttribute('points'), want.map((p) => +p.x.toFixed(2) + ',' + +p.y.toFixed(2)).join(' '));
    await t.click('sf-zoom');
    eq(z.getAttribute('aria-pressed'), 'false', 'zoom out');
    eq(t.svc.preview.length, 1, 'zoom out shows the kept preview, not fetched again');
    await t.click('sf-zoom');
    eq(t.svc.zoom.length, 1, 'zoom in again: the kept crop');
  }));

test('viewer: another direction turns zoom off; only that photo\'s preview is fetched', () => withViewer({}, async (t) => {
  t.load(1024, 765);
  await t.click('sf-zoom');
  await t.click('sf-dir-west');
  eq(t.zoom().getAttribute('aria-pressed'), 'false');
  eq(t.svc.zoom.length, 1, 'no crop of west');
  eq(t.svc.preview.length, 2);
}));

test('viewer: a failed crop says so and Zoom out goes back to the photo', () => withViewer({}, async (t) => {
  t.load(1024, 765);
  t.svc.fail = 503;
  await t.click('sf-zoom');
  eq(t.dom.byTid('sf-dialog').getAttribute('data-state'), 'error');
  eq(t.zoom().getAttribute('aria-pressed'), 'true');
  await t.click('sf-zoom');
  eq(t.dom.byTid('sf-dialog').getAttribute('data-state'), 'loading', 'the kept preview is put back');
  eq(t.img().getAttribute('src'), t.urls.created[0]);
}));

test('viewer: the crop\'s object URL is revoked once, on close', () => withViewer({}, async (t) => {
  t.load(1024, 765);
  await t.click('sf-zoom');
  t.load(1024, 699);
  eq(t.urls.created.length, 2);
  t.viewer.close(true);
  eq(t.urls.revoked.slice().sort(), t.urls.created.slice().sort());
}));

test('actionButton: the line from getPoint reaches the viewer', () => withViewer({}, async (t) => {
  t.viewer.close(false);
  t.viewer = null;
  const b = SF.actionButton({ cfg: CFG, getPoint: () => ({ lat: 56.15, lon: 10.2, line: LINE_EW }) });
  const realFetch = win.fetch;
  win.fetch = t.svc.fetch;
  try {
    b.fire('click');
    await t.settle();
    t.viewer = SF.current();
    assert.ok(/&path=56\.15000,10\.10000,56\.15000,10\.30000$/.test(t.svc.search[1]), String(t.svc.search[1]));
    eq(t.viewer.state().dir, 'north');
  } finally { win.fetch = realFetch; }
}));

(async () => {
  for (const [name, fn] of asyncTests) {
    try { await fn(); passed++; console.log('  ✅ ' + name); }
    catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
  }
  console.log('\n' + passed + ' passed, ' + failed + ' failed\n');
  process.exit(failed ? 1 : 0);
})();
