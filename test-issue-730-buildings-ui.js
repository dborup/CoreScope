/* test-issue-730-buildings-ui.js — buildings versus trees in the profile
 * panel (#730 M3).
 *
 * With map.dhm.profile.buildings on, a model=both profile that DHM covers
 * end to end tells its elevated surface samples apart (cmd/server
 * terrain_buildings.go): classification.state, samples[].cls "b"/"v" and
 * verdict.surface.cause. The panel colours the surface band by class,
 * names the cause in the surface chip ("Blocked by building", or "Through
 * trees" in a milder tone), adds a muted note when the classification is
 * partial or unavailable, credits the footprints from the server's
 * dhm.buildings block, and the hover readout names the class.
 *
 * Covers:
 *   1. band colouring: runs by class, tiling the clutter band exactly;
 *      unclassified answers draw byte for byte what they drew before;
 *   2. the surface chip for each cause × classification state × coverage,
 *      and #729's rule that it is never a second verdict;
 *   3. the partial and unavailable notes; the legend; the accessible
 *      title and name;
 *   4. escaping through the file's esc(): node names and junk classes;
 *   5. the credit from dhm.buildings through the one walker, and no
 *      public/ file carries a credit string of the footprints;
 *   6. the hover readout's class;
 *   7. the customizer keys and the CSS tokens, with their contrast;
 *   8. perf: a 1024-sample profile of alternating classes;
 *   9. the panel: a failed answer takes the note and the footprints'
 *      credit away (#797 review finding 6, mutant F1).
 *
 * Runs via: node test-issue-730-buildings-ui.js
 */
'use strict';
const fs = require('fs');
const path = require('path');
const assert = require('assert');
const { makeElement, loadTerrainProfile } = require('./tests/map-overlay-sandbox');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}
function eq(actual, expected, msg) {
  assert.deepStrictEqual(JSON.parse(JSON.stringify(actual === undefined ? null : actual)), expected, msg);
}

const { TP, win } = loadTerrainProfile(['terrain-profile-cursor.js']);
const CUR = win.TerrainProfileCursor;

// ── The credit, as internal/dhm and internal/buildings spell it ────────────

function goConsts(file) {
  const src = fs.readFileSync(path.join(__dirname, file), 'utf8');
  const out = {};
  const re = /^\s*([A-Z][A-Za-z]*)\s*=\s*"((?:[^"\\]|\\.)*)"\s*$/mg;
  let m;
  while ((m = re.exec(src))) out[m[1]] = JSON.parse('"' + m[2] + '"');
  return out;
}
const DHM = goConsts('internal/dhm/dhm.go');
const BLD = goConsts('internal/buildings/credit.go');
['Owner', 'DatasetName', 'DatasetURL', 'LicenseName', 'LicenseURL', 'DerivedNote'].forEach(k => assert.ok(DHM[k], 'dhm.' + k));
['DatasetName', 'DatasetURL', 'Source', 'SourceHandle', 'SourceTermsURL'].forEach(k => assert.ok(BLD[k], 'buildings.' + k));
const DHM_DERIVED = 'Indeholder data fra ' + DHM.Owner + ', ' + DHM.DatasetName + ', ' + DHM.LicenseName + ' — ' + DHM.DerivedNote;
// buildings.AttributionDerived: dhm.AttributionDerived with the footprints
// named after DHM (internal/buildings/credit.go, strings.Replace).
const BLD_CREDIT = DHM_DERIVED.replace(DHM.DatasetName,
  DHM.DatasetName + ' og ' + BLD.DatasetName + ' (' + BLD.Source + ', ' + BLD.SourceHandle + ')');

// The dhm block of /api/config/client with the classification on
// (cmd/server/dhm_proxy.go DHMClientConfig, DHMBuildingsClientConfig).
const DHM_BLOCK = {
  attribution: 'Indeholder data fra ' + DHM.Owner + ', ' + DHM.DatasetName + ', ' + DHM.LicenseName,
  attributionDerived: DHM_DERIVED,
  datasetName: DHM.DatasetName, datasetUrl: DHM.DatasetURL,
  licenseName: DHM.LicenseName, licenseUrl: DHM.LicenseURL,
  buildings: {
    credit: BLD_CREDIT,
    datasetName: BLD.DatasetName, datasetUrl: BLD.DatasetURL,
    sourceName: BLD.Source, sourceUrl: BLD.SourceTermsURL,
  },
};

// ── Reference profiles, field for field as the server answers them ─────────

/* A Dokk1-like path (#729): 237 m in Aarhus, flat ground at 5 m, the
 * library's 28 m roof from 118 to 158 m; the terrain clears the link and
 * the building blocks it. */
function dokk1(o) {
  o = o || {};
  const D = 237, n = 25, samples = [];
  for (let i = 0; i < n; i++) {
    const d = Math.round(D * i / (n - 1) * 100) / 100;
    const roof = d >= 118 && d <= 158;
    const s = { d, dtm: 5, dsm: roof ? 33 : 5.4 };
    if (roof) s.cls = 'b';
    samples.push(s);
  }
  return finish({
    D, samples, worstAtM: 138,
    surface: { state: 'blocked', worstClearanceM: -17.5, worstAtM: 138, worstF1Fraction: -2.1, cause: 'building' },
  }, o);
}

/* A forest path: 2 km, 18 m of trees over all of it but 100 m at each
 * end, no footprint; the terrain clears it, the canopy makes it
 * marginal. */
function forest(o) {
  o = o || {};
  const D = 2000, n = 201, samples = [];
  for (let i = 0; i < n; i++) {
    const d = 10 * i;
    const trees = d > 100 && d < 1900;
    const s = { d, dtm: 50, dsm: trees ? 68 : 50 };
    if (trees) s.cls = 'v';
    samples.push(s);
  }
  return finish({
    D, samples, worstAtM: 1000,
    surface: { state: 'fresnel60', worstClearanceM: 2.1, worstAtM: 1000, worstF1Fraction: 0.4, cause: 'vegetation' },
  }, o);
}

function finish(p, o) {
  const out = {
    a: { lat: 56.1535, lon: 10.2142, kind: 'point', groundM: p.samples[0].dtm, antennaM: 10, antennaSource: 'request', assumed: false },
    b: { lat: 56.1514, lon: 10.2150, kind: 'point', groundM: p.samples[p.samples.length - 1].dtm, antennaM: 10, antennaSource: 'request', assumed: false },
    distanceM: p.D, stepM: 10, sampleCount: p.samples.length, model: 'both',
    coverage: { state: 'full', pct: 100, firstGapM: null }, samples: p.samples,
    physics: { freqMHz: 869.618, freqSource: 'firmware', lambdaM: 0.34474, k: 4 / 3, kSource: 'default', f1MaxM: 5.6 },
    verdict: { state: 'clear', model: 'dtm', criterion: 0.6, worstClearanceM: 9.6, worstAtM: p.worstAtM, worstF1Fraction: 1.7, surface: p.surface },
    classification: { state: o.state || 'full', truncated: false },
    attribution: BLD_CREDIT,
  };
  if (o.state === null) delete out.classification;
  return out;
}

// The same answer as the classification off would give it: no
// classification, no cls, no cause, DHM's credit.
function unclassified(p) {
  const q = JSON.parse(JSON.stringify(p));
  delete q.classification;
  q.samples.forEach(s => { delete s.cls; });
  delete q.verdict.surface.cause;
  q.attribution = DHM_DERIVED;
  return q;
}

const bands = (svg, cls) => [...svg.matchAll(new RegExp('<polygon class="tp-band tp-band-' + cls + '"[^>]*points="([^"]+)"', 'g'))].map(m => m[1]);
const clutter = (svg) => [...svg.matchAll(/<polygon points="([^"]+)" fill="var\(--terrain-clutter\)"/g)].map(m => m[1]);

// ── 1. Band colouring ──────────────────────────────────────────────────────

console.log('\n=== #730 M3 the surface band by class ===');

test('Dokk1: the roof is one building run, the rest of the band stays unclassified', () => {
  const svg = TP.buildChart(dokk1(), {});
  assert.strictEqual(bands(svg, 'b').length, 1, svg);
  assert.strictEqual(bands(svg, 'v').length, 0);
  assert.ok(/<polygon class="tp-band tp-band-b"[^>]*fill="var\(--terrain-building\)"/.test(svg), 'building fill');
  // The low stretches either side are no class: the old clutter colour.
  assert.strictEqual(clutter(svg).length, 2, clutter(svg).join('\n'));
  const g = TP.chartGeometry(dokk1());
  eq(g.bands.map(b => [b.from, b.to, b.cls]), [[0, 11, null], [12, 16, 'b'], [17, 24, null]]);
});

test('forest: the canopy is trees/other, and there is no building', () => {
  const svg = TP.buildChart(forest(), {});
  assert.strictEqual(bands(svg, 'v').length, 1);
  assert.strictEqual(bands(svg, 'b').length, 0);
  assert.ok(/<polygon class="tp-band tp-band-v"[^>]*fill="var\(--terrain-vegetation\)"/.test(svg), 'vegetation fill');
});

test('the runs tile the band: a class claims the slope to a sample without one; two classes meet halfway', () => {
  // Six samples 0, 100, ..., 500 m; classes none, b, b, none, v, b.
  const p = forest();
  p.distanceM = 500;
  p.samples = [0, 1, 2, 3, 4, 5].map(i => ({ d: 100 * i, dtm: 10, dsm: 30 }));
  p.samples[1].cls = 'b'; p.samples[2].cls = 'b'; p.samples[4].cls = 'v'; p.samples[5].cls = 'b';
  const g = TP.chartGeometry(p);
  // The samples without a class lie between classified ones, so the
  // classes claim both their slopes and they draw nothing of their own.
  eq(g.bands.map(b => [b.from, b.to, b.cls]), [[1, 2, 'b'], [4, 4, 'v'], [5, 5, 'b']]);
  const pts = g.bands.map(b => b.points.split(' ').map(xy => xy.split(',').map(Number)));
  const x = g.x, sY = g.surfaceY, tY = g.terrainY;
  const mid = (a, b) => Math.round((a + b) / 2 * 100) / 100;
  const m = (arr, i) => mid(arr[i], arr[i + 1]);
  const at = (i) => [x[i], sY[i]], down = (i) => [x[i], tY[i]];
  // The building runs from sample 0 to sample 3: both slopes are its own.
  eq(pts[0], [at(0), at(1), at(2), at(3), down(3), down(2), down(1), down(0)]);
  // The trees claim the slope from sample 3 and meet the building halfway.
  eq(pts[1], [at(3), at(4), [m(x, 4), m(sY, 4)], [m(x, 4), m(tY, 4)], down(4), down(3)]);
  // The last building ends on the last sample.
  eq(pts[2], [[m(x, 4), m(sY, 4)], at(5), down(5), [m(x, 4), m(tY, 4)]]);
});

test('a stretch without a class between two classified runs draws only what they leave', () => {
  // none, b, none, none, none, v: the middle sample of the three keeps a
  // run of its own, from sample 2 to sample 4.
  const p = forest();
  p.distanceM = 500;
  p.samples = [0, 1, 2, 3, 4, 5].map(i => ({ d: 100 * i, dtm: 10, dsm: 30 }));
  p.samples[1].cls = 'b'; p.samples[5].cls = 'v';
  const g = TP.chartGeometry(p);
  eq(g.bands.map(b => [b.from, b.to, b.cls]), [[1, 1, 'b'], [2, 4, null], [5, 5, 'v']]);
  const x = g.x;
  const xs = g.bands.map(b => b.points.split(' ').map(xy => Number(xy.split(',')[0])));
  eq(xs.map(a => [Math.min.apply(null, a), Math.max.apply(null, a)]), [[x[0], x[2]], [x[2], x[4]], [x[4], x[5]]]);
});

test('a classified chart cases the line of sight in the panel colour, under it', () => {
  // Over a solid building or canopy the line of sight would be under 2:1;
  // the casing keeps it against the panel colour wherever it crosses.
  const svg = TP.buildChart(dokk1(), {});
  const casing = svg.indexOf('<line class="tp-los-casing"');
  const los = svg.indexOf('stroke="var(--los-line)" stroke-width="1.6"/>');
  assert.ok(casing > 0 && los > casing, 'casing ' + casing + ', line ' + los);
  assert.ok(/<line class="tp-los-casing"[^>]*stroke="var\(--card-bg\)"/.test(svg), 'casing colour');
  assert.ok(svg.lastIndexOf('<polygon class="tp-band') < casing, 'the casing is under a band');
  assert.ok(TP.buildChart(unclassified(dokk1()), {}).indexOf('tp-los-casing') < 0, 'casing without classes');
});

test('an unclassified answer draws byte for byte what it drew without the classification', () => {
  [dokk1(), forest()].forEach(p => {
    const plain = TP.buildChart(unclassified(p), {});
    assert.ok(!/tp-band|terrain-building|terrain-vegetation/.test(plain), 'classes drawn for an unclassified answer');
    ['unavailable', null].forEach(state => {
      const q = unclassified(p);
      if (state) q.classification = { state, truncated: false };
      assert.strictEqual(TP.buildChart(q, {}), plain, 'state ' + state);
    });
  });
});

test('classes are only drawn for a full or partial classification of a fully covered profile', () => {
  const none = (p) => !/tp-band/.test(TP.buildChart(p, {}));
  assert.ok(!none(dokk1({ state: 'partial' })), 'partial: the classified runs are still drawn');
  assert.ok(none(dokk1({ state: 'unavailable' })), 'unavailable');
  assert.ok(none(dokk1({ state: null })), 'no classification block');
  const odd = dokk1(); odd.classification.state = 'maybe';
  assert.ok(none(odd), 'an unknown state');
  const part = dokk1(); part.coverage = { state: 'partial', pct: 80, firstGapM: 200 };
  assert.ok(none(part), 'partial coverage');
  const dtm = dokk1(); dtm.model = 'dtm';
  assert.ok(none(dtm), 'model=dtm');
});

test('a class the server never sends is no class, and is never echoed', () => {
  const p = dokk1();
  p.samples[13].cls = '<img src=x onerror=alert(1)>';
  p.samples[14].cls = 'B';
  const svg = TP.buildChart(p, {});
  assert.ok(!/onerror|<img/.test(svg), 'junk class echoed');
  eq(TP.chartGeometry(p).bands.map(b => b.cls), [null, 'b', null, 'b', null]);
});

// ── 2. The surface chip ────────────────────────────────────────────────────

console.log('\n=== #730 M3 the surface chip ===');

test('Dokk1: "Blocked by building", warn tone, the detail says it is a building', () => {
  const c = TP.surfaceChip(dokk1());
  assert.strictEqual(c.label, 'Blocked by building');
  assert.strictEqual(c.tone, 'warn');
  assert.strictEqual(c.show, true);
  assert.ok(/building footprint/i.test(c.detail), c.detail);
});

test('forest: "Marginal — through trees", in the milder tone', () => {
  const c = TP.surfaceChip(forest());
  assert.strictEqual(c.label, 'Marginal — through trees');
  assert.strictEqual(c.tone, 'mild');
  assert.strictEqual(c.show, true);
  assert.ok(/trees or other/i.test(c.detail), c.detail);
  const blocked = forest();
  blocked.verdict.surface.state = 'blocked';
  assert.strictEqual(TP.surfaceChip(blocked).label, 'Through trees');
  assert.strictEqual(TP.surfaceChip(blocked).tone, 'mild');
});

test('the chip matrix: the cause is named for a full or partial classification', () => {
  const generic = TP.surfaceChip(unclassified(dokk1()));
  assert.strictEqual(generic.label, 'Buildings/trees: Blocked');
  // The server's cause is exact on a partial classification too: it says
  // building only where a classified building obstructs, and vegetation
  // only when every obstructing raised sample is classified trees/other
  // (cmd/server terrainClasses.surfaceCause; #797 review finding 4).
  const named = { building: 'Blocked by building', vegetation: 'Through trees' };
  const want = { full: named, partial: named };
  const tones = { building: 'warn', vegetation: 'mild' };
  ['full', 'partial', 'unavailable', null].forEach(state => {
    ['building', 'vegetation', 'unknown', undefined, '<b>x</b>'].forEach(cause => {
      const p = dokk1({ state });
      if (cause === undefined) delete p.verdict.surface.cause; else p.verdict.surface.cause = cause;
      const c = TP.surfaceChip(p);
      const label = (want[state] && want[state][cause]) || generic.label;
      assert.strictEqual(c.label, label, state + '/' + cause);
      assert.strictEqual(c.show, generic.show, state + '/' + cause + ' moved show');
      assert.strictEqual(c.tone, label === generic.label ? 'warn' : tones[cause], state + '/' + cause + ' tone');
      if (label === generic.label) assert.strictEqual(c.detail, generic.detail, state + '/' + cause + ' detail');
    });
  });
});

test('#729: the cause is never a second verdict', () => {
  // Every surface state × terrain state × cause × classification: the
  // chip shows exactly when the generic one would, in the warn tone or
  // the milder one (never ok, bad or unknown), names no verdict but the
  // surface's own, and leaves the terrain chip as it is.
  const WORDS = { clear: 'Clear', fresnel100: 'Usable', fresnel60: 'Marginal', blocked: 'Blocked' };
  const states = Object.keys(WORDS);
  states.forEach(surface => states.forEach(terrain => ['building', 'vegetation'].forEach(cause => ['full', 'partial'].forEach(cs => {
    const p = dokk1({ state: cs });
    p.verdict.state = terrain;
    p.verdict.surface.state = surface;
    p.verdict.surface.cause = cause;
    const q = unclassified(p);
    const c = TP.surfaceChip(p), g = TP.surfaceChip(q);
    const at = [surface, terrain, cause, cs].join('/');
    assert.strictEqual(c.show, g.show, at + ': show');
    assert.ok(c.tone === 'warn' || c.tone === 'mild', at + ': tone ' + c.tone);
    states.filter(x => x !== surface).forEach(x => {
      assert.ok(c.label.indexOf(WORDS[x]) < 0, at + ': "' + c.label + '" names ' + WORDS[x]);
    });
    assert.strictEqual(JSON.stringify(TP.verdictChip(p)), JSON.stringify(TP.verdictChip(q)), at + ': terrain chip');
  }))));
});

test('the cause never shows a chip the terrain/surface rule would not', () => {
  // Surface clear, cause building (the server would not say so, but a
  // stale cache might): no chip.
  const p = dokk1();
  p.verdict.surface.state = 'clear';
  assert.strictEqual(TP.surfaceChip(p).show, false);
  // Terrain blocked too: the surface is not worse, no chip.
  const q = dokk1();
  q.verdict.state = 'blocked';
  assert.strictEqual(TP.surfaceChip(q).show, false);
});

test('partial coverage: the surface is unknown, whatever the cause says', () => {
  const p = dokk1();
  p.coverage = { state: 'partial', pct: 80, firstGapM: 200 };
  const c = TP.surfaceChip(p);
  assert.ok(!/building|trees/.test(c.label.replace('Buildings/trees: ', '')), c.label);
  assert.strictEqual(c.tone, 'warn');
});

test('an antenna below the surface keeps its own chip', () => {
  const p = dokk1();
  p.verdict.surface.reason = 'antennaBelowSurface';
  p.verdict.surface.endpoint = 'A';
  const c = TP.surfaceChip(p);
  assert.ok(/antenna below the surface at A/.test(c.label), c.label);
});

test('"Terrain only": no chip and no note', () => {
  const p = dokk1(); p.model = 'dtm';
  assert.strictEqual(TP.surfaceChip(p), null);
  assert.strictEqual(TP.classNote(p), null);
});

// ── 3. Partial note, legend, accessible text ──────────────────────────────

console.log('\n=== #730 M3 note, legend and accessible text ===');

test('the muted note: partial says part of the path, unavailable the whole path', () => {
  assert.strictEqual(TP.classNote(dokk1({ state: 'partial' })), 'Buildings could not be told apart from trees on part of this path.');
  // #797 review finding 1: the lookup failed, so nothing could be told
  // apart; the hint line says so instead of saying nothing.
  assert.strictEqual(TP.classNote(dokk1({ state: 'unavailable' })), 'Buildings could not be told apart from trees on this path.');
  ['full', null].forEach(s => assert.strictEqual(TP.classNote(dokk1({ state: s })), null, String(s)));
  ['partial', 'unavailable'].forEach(s => {
    const part = dokk1({ state: s }); part.coverage = { state: 'partial', pct: 80, firstGapM: 200 };
    assert.strictEqual(TP.classNote(part), null, s + ' on partial coverage');
    const dtm = dokk1({ state: s }); dtm.model = 'dtm';
    assert.strictEqual(TP.classNote(dtm), null, s + ' on model=dtm');
  });
  assert.strictEqual(TP.classNote(null), null);
});

test('the legend names both classes in text, and only when a class is drawn', () => {
  const legend = (p) => {
    const m = TP.buildChart(p, {}).match(/<div class="tp-legend"[^>]*>([\s\S]*?)<\/div>/);
    return m ? m[1].replace(/<[^>]+>/g, '|').split('|').map(s => s.trim()).filter(Boolean) : null;
  };
  eq(legend(dokk1()), ['Building', 'Trees/other']);
  eq(legend(forest()), ['Building', 'Trees/other']);
  // Partial: what is left in the clutter colour may be either.
  eq(legend(dokk1({ state: 'partial' })), ['Building', 'Trees/other', 'Not told apart']);
  assert.strictEqual(legend(unclassified(dokk1())), null);
  assert.strictEqual(legend(dokk1({ state: 'unavailable' })), null);
  assert.ok(/class="tp-legend"[^>]*data-testid="tp-legend"/.test(TP.buildChart(dokk1(), {})), 'test id');
});

test('the accessible name and the title carry the cause; the title the band and the note', () => {
  const svg = TP.buildChart(dokk1({ state: 'full' }), {});
  const label = svg.match(/aria-label="([^"]*)"/)[1];
  assert.ok(/Blocked by building/.test(label), label);
  const title = svg.match(/<title>([^<]*)<\/title>/)[1];
  assert.ok(/Blocked by building/.test(title), title);
  assert.ok(/surface band/i.test(title), title);
  const p = dokk1({ state: 'partial' });
  assert.ok(TP.buildChart(p, {}).match(/<title>([^<]*)<\/title>/)[1].indexOf(TP.CLASS_PARTIAL_NOTE) >= 0, 'partial note in the title');
});

// ── 4. Escaping ────────────────────────────────────────────────────────────

console.log('\n=== #730 M3 escaping ===');

test('node names go through esc() into the classified chart', () => {
  const svg = TP.buildChart(dokk1(), { nameA: '<script>alert(1)</script>', nameB: '"onload="x' });
  assert.ok(svg.indexOf('<script>') < 0, 'raw <script>');
  assert.ok(svg.indexOf('&lt;script&gt;') >= 0, 'escaped name missing');
  assert.ok(svg.indexOf('&quot;onload=&quot;x') >= 0, 'escaped quote missing');
});

test('a junk cause is not echoed into the chip or the chart', () => {
  const p = dokk1();
  p.verdict.surface.cause = '<img src=x onerror=alert(1)>';
  assert.ok(!/onerror/.test(TP.surfaceChip(p).label + TP.surfaceChip(p).detail));
  assert.ok(!/onerror/.test(TP.buildChart(p, {})));
});

// ── 5. The credit ──────────────────────────────────────────────────────────

console.log('\n=== #730 M3 the credit ===');

function anchors(el) {
  const out = [];
  (function walk(e) {
    (e.children || []).forEach(c => {
      if (c.tagName === 'A') out.push([c.textContent, c.attrs.href, c.attrs['data-testid'], c.attrs.rel]);
      walk(c);
    });
  })(el);
  return out;
}

test('a classified profile: the footprints credit, with four marks linked in text order', () => {
  const box = makeElement('p');
  TP.footerCredit(box, DHM_BLOCK, dokk1());
  assert.strictEqual(box.textContent, BLD_CREDIT);
  eq(anchors(box), [
    [DHM.DatasetName, DHM.DatasetURL, 'tp-credit-dataset', 'noopener noreferrer'],
    [BLD.DatasetName, BLD.DatasetURL, 'tp-credit-buildings-dataset', 'noopener noreferrer'],
    [BLD.Source, BLD.SourceTermsURL, 'tp-credit-buildings-source', 'noopener noreferrer'],
    [DHM.LicenseName, DHM.LicenseURL, 'tp-credit-link', 'noopener noreferrer license'],
  ]);
  assert.ok(box.textContent.indexOf(BLD.SourceHandle) >= 0, 'the handle is in the text');
});

test('an unclassified profile, and the panel before any answer: the DHM credit', () => {
  [undefined, unclassified(dokk1()), dokk1({ state: 'unavailable' })].forEach((data, i) => {
    const box = makeElement('p');
    if (data && i === 2) data.attribution = DHM_DERIVED;
    TP.footerCredit(box, DHM_BLOCK, data);
    assert.strictEqual(box.textContent, DHM_DERIVED, 'case ' + i);
    eq(anchors(box).map(a => a[2]), ['tp-credit-dataset', 'tp-credit-link']);
  });
});

test('the footer is rebuilt, not appended to, answer after answer', () => {
  const box = makeElement('p');
  TP.footerCredit(box, DHM_BLOCK);
  TP.footerCredit(box, DHM_BLOCK, dokk1());
  TP.footerCredit(box, DHM_BLOCK, unclassified(dokk1()));
  assert.strictEqual(box.textContent, DHM_DERIVED);
});

test('without the block the footprints are never credited from the page', () => {
  const cfg = Object.assign({}, DHM_BLOCK);
  delete cfg.buildings;
  const box = makeElement('p');
  TP.footerCredit(box, cfg, dokk1());
  assert.strictEqual(box.textContent, DHM_DERIVED);
});

test('no public/ file carries a credit string of the footprints', () => {
  const dir = path.join(__dirname, 'public');
  const hits = [];
  fs.readdirSync(dir).filter(f => /\.(js|html|css)$/.test(f)).forEach(f => {
    const src = fs.readFileSync(path.join(dir, f), 'utf8');
    [BLD.DatasetName, BLD.SourceHandle, BLD.DatasetURL, BLD.SourceTermsURL, 'Bygninger'].forEach(s => {
      if (src.indexOf(s) >= 0) hits.push(f + ': ' + s);
    });
  });
  assert.deepStrictEqual(hits, []);
});

// ── 6. The hover readout ───────────────────────────────────────────────────

console.log('\n=== #730 M3 the hover readout ===');

test('the readout names the class of the hovered sample, and keeps its four rows', () => {
  const p = dokk1();
  const g = TP.chartGeometry(p);
  const roof = CUR.readout(g, p, 14);
  assert.strictEqual(roof.lines.length, 4);
  assert.strictEqual(roof.lines[2], 'Surface 33.0 m (building)');
  assert.strictEqual(CUR.readout(g, p, 3).lines[2], 'Surface 5.4 m');
  // The roof's edges (#797 review finding 6, mutant F2): samples 12 and
  // 16 are the first and last on the roof, 11 and 17 the ground beside.
  eq([11, 12, 16, 17].map(i => CUR.readout(g, p, i).lines[2]),
    ['Surface 5.4 m', 'Surface 33.0 m (building)', 'Surface 33.0 m (building)', 'Surface 5.4 m']);
  const f = forest();
  assert.strictEqual(CUR.readout(TP.chartGeometry(f), f, 100).lines[2], 'Surface 68.0 m (trees/other)');
  // Unclassified, or an unavailable classification: no class.
  [unclassified(p), dokk1({ state: 'unavailable' })].forEach(q => {
    assert.strictEqual(CUR.readout(TP.chartGeometry(q), q, 14).lines[2], 'Surface 33.0 m');
  });
});

// ── 7. Customizer and CSS tokens ───────────────────────────────────────────

console.log('\n=== #730 M3 colours ===');

const CSS = fs.readFileSync(path.join(__dirname, 'public', 'style.css'), 'utf8');

// Every block the selector opens, joined (style.css has two :root blocks).
function blockOf(selectorRe) {
  const out = [];
  let m;
  while ((m = selectorRe.exec(CSS))) {
    let i = CSS.indexOf('{', m.index) + 1, depth = 1;
    const start = i;
    while (depth && i < CSS.length) { if (CSS[i] === '{') depth++; else if (CSS[i] === '}') depth--; i++; }
    out.push(CSS.slice(start, i - 1));
  }
  assert.ok(out.length, 'no block ' + selectorRe);
  return out.join('\n');
}
const BLOCKS = {
  light: blockOf(/^:root\s*\{/mg),
  darkMedia: blockOf(/^\s*:root:not\(\[data-theme="light"\]\)\s*\{/mg),
  dark: blockOf(/^\[data-theme="dark"\]\s*\{/mg),
};
function varIn(block, name) {
  const m = block.match(new RegExp('^\\s*' + name.replace(/-/g, '\\-') + ':\\s*([^;]+);', 'm'));
  return m ? m[1].trim() : null;
}
function lum(hex) {
  const c = hex.replace('#', '').match(/../g).map(h => parseInt(h, 16) / 255)
    .map(v => v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4));
  return 0.2126 * c[0] + 0.7152 * c[1] + 0.0722 * c[2];
}
function contrast(a, b) { const x = lum(a), y = lum(b); return (Math.max(x, y) + 0.05) / (Math.min(x, y) + 0.05); }
function resolve(block, v) {
  let out = v;
  for (let i = 0; i < 4 && /^var\(/.test(out); i++) {
    const name = out.match(/^var\((--[a-z0-9-]+)/)[1];
    out = varIn(block, name) || varIn(BLOCKS.light, name);
  }
  return out;
}

test('both tokens are defined in :root and in both dark blocks, the dark ones alike', () => {
  ['--terrain-building', '--terrain-vegetation'].forEach(v => {
    Object.keys(BLOCKS).forEach(b => assert.ok(/^#[0-9a-f]{6}$/i.test(varIn(BLOCKS[b], v) || ''), v + ' in ' + b + ': ' + varIn(BLOCKS[b], v)));
    assert.strictEqual(varIn(BLOCKS.darkMedia, v), varIn(BLOCKS.dark, v), v + ': the dark blocks differ');
  });
});

test('contrast ≥ 3:1: each class against the panel, and the two classes against each other', () => {
  [['light', BLOCKS.light], ['dark', BLOCKS.dark]].forEach(([name, block]) => {
    const card = resolve(block, varIn(block, '--card-bg') || varIn(BLOCKS.light, '--card-bg'));
    const b = varIn(block, '--terrain-building'), v = varIn(block, '--terrain-vegetation');
    [['building/panel', b, card], ['trees/panel', v, card], ['building/trees', b, v]].forEach(([what, x, y]) => {
      const r = contrast(x, y);
      assert.ok(r >= 3, name + ' ' + what + ' ' + x + ' on ' + y + ' is ' + r.toFixed(2) + ':1');
    });
  });
});

test('the milder chip tone is drawn from theme tokens only, and is not the warn colour', () => {
  const m = CSS.match(/\.tp-chip-mild\s*\{([^}]*)\}/);
  assert.ok(m, 'no .tp-chip-mild rule');
  assert.ok(!/#[0-9a-f]{3,8}\b|rgb/i.test(m[1]), 'hardcoded colour: ' + m[1]);
  assert.ok(/color:\s*var\(--text\)/.test(m[1]), 'text colour: ' + m[1]);
});

test('the customizer maps both tokens, among the terrain chart colours', () => {
  const src = fs.readFileSync(path.join(__dirname, 'public', 'customize-v2.js'), 'utf8');
  const map = src.match(/var THEME_CSS_MAP = \{([\s\S]*?)\};/)[1];
  assert.ok(/terrainBuilding: '--terrain-building'/.test(map), 'terrainBuilding');
  assert.ok(/terrainVegetation: '--terrain-vegetation'/.test(map), 'terrainVegetation');
  const keys = src.match(/var TERRAIN_COLOR_KEYS = \[([^\]]*)\]/)[1];
  assert.ok(/'terrainBuilding'/.test(keys) && /'terrainVegetation'/.test(keys), keys);
});

// ── 8. Perf ────────────────────────────────────────────────────────────────

console.log('\n=== #730 M3 perf ===');

test('1024 samples of alternating classes: geometry and SVG in well under 50 ms', () => {
  const D = 80000, n = 1024, samples = [];
  for (let i = 0; i < n; i++) {
    const s = { d: Math.round(D * i / (n - 1) * 100) / 100, dtm: 40 + (i % 13), dsm: 60 + (i % 13) };
    s.cls = (i % 2) ? 'b' : 'v';
    samples.push(s);
  }
  const p = forest();
  p.distanceM = D; p.samples = samples;
  for (let i = 0; i < 5; i++) TP.buildChart(p, {}); // warm up
  const runs = 20, t0 = process.hrtime.bigint();
  let svg;
  for (let i = 0; i < runs; i++) svg = TP.buildChart(p, {});
  const ms = Number(process.hrtime.bigint() - t0) / 1e6 / runs;
  console.log('    buildChart, 1024 samples / 1024 runs: ' + ms.toFixed(2) + ' ms per chart');
  assert.strictEqual(bands(svg, 'b').length + bands(svg, 'v').length, n);
  assert.ok(ms < 50, ms + ' ms');
});

// ── 9. The panel ───────────────────────────────────────────────────────────

/* A panel on a recording fake DOM with real timers, as the #723 suite
 * drives it; fetchJSON answers from `answers`, one per request. */
function loadPanel() {
  const vm = require('vm');
  const { makeTextNode } = require('./tests/map-overlay-sandbox');
  const made = [];
  function mk(tag) {
    const e = makeElement(tag);
    e.hidden = false; e.disabled = false; e.title = '';
    e.addEventListener = () => {}; e.removeEventListener = () => {};
    e.classList.toggle = (c, w) => { const v = w === undefined ? !e.classList.contains(c) : !!w; if (v) e.classList.add(c); else e.classList.remove(c); };
    e.removeChild = (c) => { e.children = e.children.filter(x => x !== c); return c; };
    made.push(e);
    return e;
  }
  const body = mk('body');
  const doc = { createElement: mk, createTextNode: makeTextNode, addEventListener() {}, body, documentElement: { getAttribute: () => 'light' }, querySelector: () => null, querySelectorAll: () => [] };
  const loc = { origin: 'http://x', pathname: '/', search: '', hash: '#/map' };
  const history = { state: null, replaceState() {} };
  const win = {
    addEventListener() {}, removeEventListener() {}, dispatchEvent() {}, matchMedia: () => ({ matches: false }),
    innerWidth: 1280, innerHeight: 800, document: doc, location: loc, history, setTimeout, clearTimeout,
    localStorage: { getItem: () => null, setItem() {}, removeItem() {} },
  };
  const ctx = {
    window: win, document: doc, console, Math, Date, JSON, Array, Object, String, Number, RegExp, Error, TypeError,
    Boolean, isNaN, isFinite, parseInt, parseFloat, URLSearchParams, Map, Set, Promise, encodeURIComponent,
    decodeURIComponent, setTimeout, clearTimeout, location: loc, history, CustomEvent: function () {},
  };
  vm.createContext(ctx);
  ['map-tile-providers.js', 'antenna-height.js', 'terrain-profile.js'].forEach(f =>
    vm.runInContext(fs.readFileSync(path.join(__dirname, 'public', f), 'utf8'), ctx));
  const answers = [];
  const tool = ctx.window.TerrainProfile.create({
    mount: body, dhm: DHM_BLOCK,
    fetchJSON: () => { const a = answers.shift(); return a instanceof Error ? Promise.reject(a) : Promise.resolve(a); },
  });
  const find = (id) => made.filter(e => e.attrs['data-testid'] === id).pop() || null;
  let lon = 10.2142;
  return {
    answers,
    // A new path, so the panel asks again.
    measure: () => { lon += 0.001; tool.setState({ a: { kind: 'point', lat: 56.1535, lon }, b: { kind: 'point', lat: 56.1514, lon: 10.2150 } }); },
    state: () => {
      const note = find('tp-class-note'), credit = made.filter(e => e.className === 'tp-credit-line').pop();
      return { note: note && !note.hidden ? note.textContent : null, credit: credit ? credit.textContent : null, verdict: find('tp-verdict').textContent };
    },
  };
}
const SETTLE_MS = 450; // the panel's request debounce is 350 ms
const sleep = (ms) => new Promise(r => setTimeout(r, ms));
const asyncTests = [];

asyncTests.push(['a failed answer takes the note and the footprints\' credit away', async () => {
  const P = loadPanel();
  const partial = dokk1({ state: 'partial' });
  // A request that fails, and an answer the panel cannot draw: both end
  // on "No profile", with no note and DHM's credit alone.
  const broken = dokk1({ state: 'partial' });
  broken.samples = { length: 1 };
  [new Error('Request failed (502)'), broken].forEach(bad => {
    P.answers.push(partial, bad);
  });
  for (const what of ['a failed request', 'an answer it cannot draw']) {
    P.measure();
    await sleep(SETTLE_MS);
    eq([P.state().note, P.state().credit], [TP.classNote(partial), BLD_CREDIT], what + ': the partial answer first');
    P.measure();
    await sleep(SETTLE_MS);
    eq(P.state(), { note: null, credit: DHM_DERIVED, verdict: 'No profile' }, what);
  }
}]);

(async () => {
  console.log('\n=== #730 M3 the panel ===');
  for (const [name, fn] of asyncTests) {
    try { await fn(); passed++; console.log('  ✅ ' + name); }
    catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
  }
  console.log('\n' + passed + ' passed, ' + failed + ' failed');
  process.exit(failed ? 1 : 0);
})();
