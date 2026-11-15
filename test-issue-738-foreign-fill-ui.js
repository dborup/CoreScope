/* test-issue-738-foreign-fill-ui.js — the mixed-resolution note and the
 * Copernicus credit in the profile panel (#738 M2).
 *
 * With map.dhm.profile.foreignFill on, the samples DHM does not cover are
 * filled from Copernicus DEM GLO-30 and the response says so
 * (coverage.mixed, coverage.fill[].resolutionM, attributionFill; #773).
 * The panel then says the profile is mixed, coarser and surface-influenced
 * outside Denmark with the surface unknown there, and carries the short
 * Copernicus credit built from the server's dhm.fill block through the one
 * credit walker, with the full Art. 6(b) line one step away (#738
 * decision 2). Neither appears on a profile that is not mixed, a failed
 * fill included.
 *
 * Covers:
 *   1. mixedNote: mixed, mixed on the ½ overview, not mixed, a failed fill
 *      (partial), and junk in the response;
 *   2. fillCredit: shown only when mixed, links from the block, the full
 *      line from the response, nothing becomes markup, the fallbacks;
 *   3. the chart's accessible title carries the note through the file's
 *      esc();
 *   4. the overview resolution matches cmd/server, and no public/ file
 *      carries a Copernicus credit string.
 *
 * Runs via: node test-issue-738-foreign-fill-ui.js
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

// ── The Copernicus credit, as internal/copdem spells it ────────────────────

const COPDEM_SRC = fs.readFileSync(path.join(__dirname, 'internal', 'copdem', 'copdem.go'), 'utf8');
function goConst(name) {
  const m = COPDEM_SRC.match(new RegExp('^\\s*' + name + '\\s*=\\s*"((?:[^"\\\\]|\\\\.)*)"\\s*$', 'm'));
  assert.ok(m, 'internal/copdem/copdem.go has no ' + name + ' string constant');
  return JSON.parse('"' + m[1] + '"');
}
const GO = {
  DatasetName: goConst('DatasetName'),
  DatasetURL: goConst('DatasetURL'),
  LicenseName: goConst('LicenseName'),
  LicenseURL: goConst('LicenseURL'),
  AttributionDerived: goConst('AttributionDerived'),
};
// Credit is joined from the parts; pinned as the expression, so a change
// to it fails here as well as in internal/copdem/credit_738_test.go.
const GO_CREDIT_EXPR = 'Credit = "Outside Denmark: produced using " + DatasetName + ", provided under COPERNICUS by the European Union and ESA (" + LicenseName + ")"';
const GO_CREDIT = 'Outside Denmark: produced using ' + GO.DatasetName + ', provided under COPERNICUS by the European Union and ESA (' + GO.LicenseName + ')';

// dhm.fill of /api/config/client (cmd/server/dhm_proxy.go DHMFillClientConfig).
const FILL_BLOCK = {
  credit: GO_CREDIT,
  datasetName: GO.DatasetName, datasetUrl: GO.DatasetURL,
  licenseName: GO.LicenseName, licenseUrl: GO.LicenseURL,
};

// A Sønderborg → Flensburg profile, field for field as the server sends
// it with the fill (cmd/server/terrain_profile.go TerrainProfileResponse).
function profile(o) {
  o = o || {};
  const D = 30000, n = 60;
  const samples = [];
  for (let i = 0; i < n; i++) samples.push({ d: Math.round(D * i / (n - 1) * 100) / 100, dtm: 10 + (i % 7), dsm: i < 40 ? 18 + (i % 7) : undefined });
  const coverage = { state: 'full', pct: 100, firstGapM: null };
  if (o.mixed !== false) {
    coverage.mixed = true;
    coverage.fill = [{ fromM: 20338.98, toM: D, source: 'copdem-glo30', resolutionM: o.resolutionM === undefined ? 30 : o.resolutionM }];
  }
  const out = {
    a: { lat: 54.9098, lon: 9.7922, kind: 'point', groundM: 10, antennaM: 10, antennaSource: 'request', assumed: false },
    b: { lat: 54.7937, lon: 9.4469, kind: 'point', groundM: 14, antennaM: 10, antennaSource: 'request', assumed: false },
    distanceM: D, stepM: 29.3, sampleCount: 1024, model: o.model || 'both',
    coverage: coverage, samples: samples,
    physics: { freqMHz: 869.618, freqSource: 'firmware', lambdaM: 0.34474, k: 4 / 3, kSource: 'default', f1MaxM: 50.8 },
    verdict: { state: 'clear', model: 'dtm', criterion: 0.6, worstClearanceM: 12, worstAtM: 15000, worstF1Fraction: 1.2,
      surface: { state: 'unknown', worstClearanceM: null, worstAtM: null, worstF1Fraction: null } },
    attribution: 'Indeholder data fra …',
  };
  if (o.mixed !== false) out.attributionFill = o.attributionFill || GO.AttributionDerived;
  return Object.assign(out, o.override || {});
}
// The same path with the fill failed: partial, no verdict, no fill fields
// (cmd/server TestTerrainFill_FailedFillKeepsDHMCached).
function failedFill() {
  const p = profile({ mixed: false });
  p.coverage = { state: 'partial', pct: 67.8, firstGapM: 20338.98 };
  p.samples.forEach((s, i) => { if (i >= 41) { s.dtm = null; delete s.dsm; } });
  p.verdict = { state: 'unknown', model: 'dtm', criterion: 0.6, worstClearanceM: null, worstAtM: null, worstF1Fraction: null,
    surface: { state: 'unknown', worstClearanceM: null, worstAtM: null, worstF1Fraction: null } };
  return p;
}
// A path DHM covers end to end.
function dkOnly() { return profile({ mixed: false }); }

function anchors(el) {
  const out = [];
  (function walk(e) {
    (e.children || []).forEach(c => {
      if (c.tagName === 'A') out.push({ text: c.textContent, attrs: c.attrs, className: c.className });
      walk(c);
    });
  })(el);
  return out;
}
function find(el, testid) {
  let hit = null;
  (function walk(e) {
    (e.children || []).forEach(c => {
      if (!hit && c.attrs && c.attrs['data-testid'] === testid) hit = c;
      if (!hit) walk(c);
    });
  })(el);
  return hit;
}

const { TP, win } = loadTerrainProfile();

function creditOf(cfg, data) {
  const box = makeElement('div');
  TP.fillCredit(box, cfg, data);
  return box;
}

// ── 1. The note ────────────────────────────────────────────────────────────

console.log('\n=== #738 M2 mixed-resolution note ===');

test('a mixed profile says outside Denmark, Copernicus 30 m, surface-influenced, surface unknown', () => {
  const note = TP.mixedNote(profile());
  assert.ok(note, 'no note');
  ['Mixed', 'outside Denmark', 'Copernicus 30 m', 'surface-influenced', 'surface unknown']
    .forEach(w => assert.ok(note.indexOf(w) >= 0, JSON.stringify(w) + ' missing from ' + JSON.stringify(note)));
  assert.strictEqual(note.indexOf('overview'), -1, note);
});

test('on the ½ overview the note says 60 m and that it is the overview', () => {
  const note = TP.mixedNote(profile({ resolutionM: 60 }));
  assert.ok(note && note.indexOf('Copernicus 60 m') >= 0 && note.indexOf('½ overview') >= 0, note);
});

test('the note takes the coarsest resolution of the fill ranges', () => {
  const p = profile();
  p.coverage.fill.push({ fromM: 100, toM: 200, source: 'copdem-glo30', resolutionM: 60 });
  assert.ok(TP.mixedNote(p).indexOf('60 m') >= 0, TP.mixedNote(p));
});

test('a profile that is not mixed has no note', () => {
  assert.strictEqual(TP.mixedNote(dkOnly()), null);
  assert.strictEqual(TP.mixedNote({}), null);
  assert.strictEqual(TP.mixedNote(null), null);
});

test('a failed fill (partial, no verdict) has no note and keeps the partial detail', () => {
  const p = failedFill();
  assert.strictEqual(TP.mixedNote(p), null);
  const chip = TP.verdictChip(p);
  assert.strictEqual(chip.state, 'unknown');
  assert.ok(/path leaves Denmark/.test(chip.detail), chip.detail);
});

test('only a literal mixed:true counts', () => {
  ['true', 1, 'yes'].forEach(v => {
    const p = profile();
    p.coverage.mixed = v;
    assert.strictEqual(TP.mixedNote(p), null, 'mixed=' + JSON.stringify(v));
  });
});

test('a junk resolution is left out of the note, not echoed', () => {
  ['<img src=x onerror=alert(1)>', -30, 0, 'NaN', null, Infinity].forEach(r => {
    const note = TP.mixedNote(profile({ resolutionM: r }));
    assert.ok(note && note.indexOf('surface unknown') >= 0, 'no note for ' + String(r));
    assert.ok(!/<|onerror|NaN|Infinity|-30| 0 m/.test(note), String(r) + ' -> ' + note);
  });
});

// ── 2. The credit ──────────────────────────────────────────────────────────

console.log('\n=== #738 M2 Copernicus credit ===');

test('a mixed profile shows the short credit from the block, with both marks linked', () => {
  const box = creditOf(FILL_BLOCK, profile());
  assert.strictEqual(box.hidden, false);
  const line = find(box, 'tp-credit-fill-line');
  assert.ok(line, 'no credit line');
  assert.strictEqual(line.textContent, GO_CREDIT);
  const as = anchors(line);
  assert.deepStrictEqual(as.map(a => [a.text, a.attrs.href, a.attrs['data-testid']]), [
    [GO.DatasetName, GO.DatasetURL, 'tp-credit-fill-dataset'],
    [GO.LicenseName, GO.LicenseURL, 'tp-credit-fill-license'],
  ]);
  as.forEach(a => {
    assert.strictEqual(a.attrs.target, '_blank');
    assert.ok(/\bnoopener\b/.test(a.attrs.rel), a.attrs.rel);
    assert.strictEqual(a.className, 'tp-credit-link');
  });
  assert.ok(/\blicense\b/.test(as[1].attrs.rel), as[1].attrs.rel);
});

test('the full Art. 6(b) line is the response\'s attributionFill, behind a disclosure', () => {
  const box = creditOf(FILL_BLOCK, profile());
  const full = find(box, 'tp-credit-fill-full');
  assert.ok(full, 'no full credit');
  assert.strictEqual(full.tagName, 'DETAILS');
  assert.strictEqual(full.attrs.open, undefined, 'the long line is shown inline');
  const text = find(full, 'tp-credit-fill-text');
  assert.strictEqual(text.textContent, GO.AttributionDerived);
  // Not inline: the short line does not carry it.
  assert.strictEqual(find(box, 'tp-credit-fill-line').textContent.indexOf(GO.AttributionDerived), -1);
});

test('the credit is shown on the overview too', () => {
  const box = creditOf(FILL_BLOCK, profile({ resolutionM: 60 }));
  assert.strictEqual(box.hidden, false);
  assert.strictEqual(find(box, 'tp-credit-fill-line').textContent, GO_CREDIT);
});

test('a profile that is not mixed shows no Copernicus credit', () => {
  const box = creditOf(FILL_BLOCK, dkOnly());
  assert.strictEqual(box.hidden, true);
  assert.strictEqual(box.textContent, '');
});

test('a failed fill shows no Copernicus credit', () => {
  const box = creditOf(FILL_BLOCK, failedFill());
  assert.strictEqual(box.hidden, true);
  assert.strictEqual(box.textContent, '');
});

test('the credit goes away again when the next profile is not mixed', () => {
  const box = makeElement('div');
  TP.fillCredit(box, FILL_BLOCK, profile());
  TP.fillCredit(box, FILL_BLOCK, dkOnly());
  assert.strictEqual(box.hidden, true);
  assert.strictEqual(box.textContent, '');
  // And a second mixed profile shows one credit, not two.
  TP.fillCredit(box, FILL_BLOCK, profile());
  TP.fillCredit(box, FILL_BLOCK, profile());
  assert.strictEqual(box.textContent.split(GO.DatasetName).length - 1, 1, box.textContent);
});

test('the credit follows the block, not a copy of its strings', () => {
  const cfg = { credit: 'Abroad: made with Elevation X (Licence Y)', datasetName: 'Elevation X', datasetUrl: 'https://example.test/x',
    licenseName: 'Licence Y', licenseUrl: 'https://example.test/y' };
  const line = find(creditOf(cfg, profile()), 'tp-credit-fill-line');
  assert.strictEqual(line.textContent, cfg.credit);
  assert.deepStrictEqual(anchors(line).map(a => [a.text, a.attrs.href]),
    [['Elevation X', 'https://example.test/x'], ['Licence Y', 'https://example.test/y']]);
});

test('nothing in the credit or the full line becomes markup', () => {
  const evil = '<img src=x onerror=alert(1)>';
  const box = creditOf(Object.assign({}, FILL_BLOCK, { credit: evil + ' ' + GO.DatasetName }), profile({ attributionFill: evil }));
  assert.strictEqual(/<img/.test(box.innerHTML), false, box.innerHTML);
  assert.strictEqual(find(box, 'tp-credit-fill-text').textContent, evil);
  assert.strictEqual(find(box, 'tp-credit-fill-line').textContent, evil + ' ' + GO.DatasetName);
});

test('a target that is not absolute https is not linked', () => {
  const line = find(creditOf(Object.assign({}, FILL_BLOCK, { licenseUrl: 'javascript:alert(1)' }), profile()), 'tp-credit-fill-line');
  assert.deepStrictEqual(anchors(line).map(a => a.text), [GO.DatasetName]);
  assert.strictEqual(line.textContent, GO_CREDIT);
});

test('without the walker the credit is still shown, as plain text', () => {
  const saved = [win.MC_appendCredit, win.MC_creditLinks];
  win.MC_appendCredit = undefined; win.MC_creditLinks = undefined;
  try {
    const box = creditOf(FILL_BLOCK, profile());
    assert.strictEqual(find(box, 'tp-credit-fill-line').textContent, GO_CREDIT);
    assert.strictEqual(anchors(box).length, 0);
  } finally { [win.MC_appendCredit, win.MC_creditLinks] = saved; }
});

test('a mixed profile without the block still carries the full credit', () => {
  // A mixed response is a licence obligation whatever the block says.
  [undefined, null, {}].forEach(cfg => {
    const box = creditOf(cfg, profile());
    assert.strictEqual(box.hidden, false, 'cfg ' + JSON.stringify(cfg));
    assert.strictEqual(find(box, 'tp-credit-fill-line').textContent, GO.AttributionDerived);
    assert.strictEqual(find(box, 'tp-credit-fill-full'), null, 'the long line twice');
  });
});

// ── 3. The chart title ─────────────────────────────────────────────────────

console.log('\n=== #738 M2 chart title ===');

function titleOf(html) {
  const m = /<title>([^<]*)<\/title>/.exec(html);
  assert.ok(m, 'no <title> in ' + html.slice(0, 200));
  return m[1];
}

test('the chart title carries the note, through esc()', () => {
  const html = TP.buildChart(profile(), { nameA: 'A & "B"', nameB: '<b>C</b>' });
  const title = titleOf(html);
  const note = TP.mixedNote(profile());
  const esc = s => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;').replace(/'/g, '&#39;');
  assert.ok(title.indexOf(esc(note)) >= 0, title);
  assert.ok(title.indexOf('A &amp; &quot;B&quot;') >= 0 && title.indexOf('&lt;b&gt;C&lt;/b&gt;') >= 0, title);
  assert.strictEqual(/<b>/.test(html), false);
});

test('a hostile resolution never reaches the chart markup', () => {
  const html = TP.buildChart(profile({ resolutionM: '"><script>alert(1)</script>' }), {});
  assert.strictEqual(/<script|alert\(1\)/.test(html), false, html.slice(0, 400));
});

test('a chart that is not mixed has no note in its title', () => {
  assert.strictEqual(/Mixed|Copernicus/.test(titleOf(TP.buildChart(dkOnly(), {}))), false);
});

// ── 4. One definition ──────────────────────────────────────────────────────

console.log('\n=== #738 M2 one definition ===');

test('the overview resolution is cmd/server\'s', () => {
  const src = fs.readFileSync(path.join(__dirname, 'cmd', 'server', 'terrain_profile.go'), 'utf8');
  const m = /fillResolutionM = \[\.\.\.\]int\{[^}]*fillOverview: (\d+)\}/.exec(src);
  assert.ok(m, 'cmd/server/terrain_profile.go has no fillResolutionM table');
  assert.strictEqual(TP.FILL_OVERVIEW_M, Number(m[1]));
});

test('the block this suite drives is internal/copdem\'s Credit', () => {
  assert.ok(COPDEM_SRC.indexOf(GO_CREDIT_EXPR) >= 0, 'internal/copdem/copdem.go no longer joins Credit this way');
});

function codeOnly(src) {
  let inBlock = false;
  return src.split('\n').filter((line) => {
    const t = line.trim();
    if (inBlock) { if (t.indexOf('*/') >= 0) inBlock = false; return false; }
    if (t.indexOf('/*') === 0) { if (t.indexOf('*/') < 0) inBlock = true; return false; }
    return t.indexOf('//') !== 0 && t.indexOf('*') !== 0;
  }).join('\n');
}

test('no public/ file carries a Copernicus credit string', () => {
  const strings = [GO.DatasetName, GO.DatasetURL, GO.LicenseName, GO.LicenseURL, GO.AttributionDerived,
    'provided under COPERNICUS', 'WorldDEM'];
  const files = fs.readdirSync(path.join(__dirname, 'public')).filter(f => f.endsWith('.js'));
  const carrying = files.filter(f => {
    const src = codeOnly(fs.readFileSync(path.join(__dirname, 'public', f), 'utf8'));
    return strings.some(s => src.indexOf(s) >= 0);
  });
  assert.deepStrictEqual(carrying, []);
});

console.log('\n' + passed + ' passed, ' + failed + ' failed');
process.exit(failed ? 1 : 0);
