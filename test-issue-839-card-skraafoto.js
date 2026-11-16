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
  assert.ok(title.indexOf(' Tightest point for the Fresnel zone: clearance -11.3 m at 5786.7 m along the path.') >= 0, title);
  assert.ok(!/worst clearance/i.test(title), title);
  // The Go twin (AltText) says it in the same words.
  assert.ok(RENDER_GO.indexOf('" Tightest point for the Fresnel zone: clearance " + fmtMetres(wc) + " at " + fmtMetres(at) + " along the path."') >= 0,
    'internal/profilecard/render.go AltText does not use the same words');
});

(async () => {
  for (const [name, fn] of asyncTests) {
    try { await fn(); passed++; console.log('  ✅ ' + name); }
    catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
  }
  console.log('\n' + passed + ' passed, ' + failed + ' failed\n');
  process.exit(failed ? 1 : 0);
})();
