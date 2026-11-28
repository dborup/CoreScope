/* test-issue-860-elevation-ramp.js — the colour-by-elevation ramp tuned to
 * Denmark (#860), the page half.
 *
 * Unit tests for public/map-tile-providers.js in the vm sandbox
 * (tests/map-overlay-sandbox.js) and for public/style.css:
 *   1. the stop table: the CSS defines exactly one --dhm-elevation-<m>
 *      variable per stop the server sends (no stale #792 stop is left);
 *   2. the ramp id: the elevation entry's `style` is sent as the tile's
 *      WMS `styles`, so a new ramp is a new URL; the other overlays keep
 *      an empty one; an entry without a well-formed id is not offered;
 *   3. the legend: one equal step per stop, so the low bands Denmark is
 *      made of are as wide as the hill bands; labels at least a seventh of
 *      the bar apart and clear of the last; every stop in its aria-label.
 *
 * Runs via: node test-issue-860-elevation-ramp.js
 */
'use strict';
const fs = require('fs');
const path = require('path');
const assert = require('assert');
const { makeSandbox, buildControl, makeElement, makeTextNode } = require('./tests/map-overlay-sandbox');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

const ATTRIBUTION = 'Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel, CC BY 4.0';
// The stops and the ramp id as cmd/server/dhm_proxy.go sends them since
// #860 (internal/dhm.ElevationRamp, ElevationStyle).
const LEGEND_M = [0, 2, 5, 10, 15, 20, 25, 30, 40, 50, 65, 80, 100, 130, 175];
const STYLE = 'ramp-0123abcd';
const ELEVATION = { id: 'elevation', minZoom: 9, maxZoom: 13, datasetName: 'Danmarks Højdemodel',
  datasetUrl: 'https://dataforsyningen.dk/data/930', attribution: ATTRIBUTION + ' — data bearbejdet',
  legendM: LEGEND_M, style: STYLE };
const DHM = {
  enabled: true, url: '/api/map/dhm/wms', layer: 'dhm_terraen_skyggekort',
  minZoom: 7, maxZoom: 19, bounds: [[54.4265, 7.99125], [57.778, 15.5995]],
  attribution: ATTRIBUTION,
  datasetName: 'Danmarks Højdemodel', datasetUrl: 'https://dataforsyningen.dk/data/930',
  licenseName: 'CC BY 4.0', licenseUrl: 'https://creativecommons.org/licenses/by/4.0/',
  overlays: [
    { id: 'terrain', minZoom: 7, maxZoom: 19, datasetName: 'Danmarks Højdemodel', datasetUrl: 'https://dataforsyningen.dk/data/930' },
    { id: 'surface', minZoom: 7, maxZoom: 19, datasetName: 'Danmarks Højdemodel', datasetUrl: 'https://dataforsyningen.dk/data/928' },
    { id: 'contours', minZoom: 14, maxZoom: 19, datasetName: 'Danmarks Højdemodel', datasetUrl: 'https://dataforsyningen.dk/data/926' },
    ELEVATION,
  ],
};
const LABEL = 'Colour by elevation (DHM, zoom 9+)';

function withElevation(patch) {
  const e = Object.assign({}, ELEVATION, patch);
  return Object.assign({}, DHM, { overlays: DHM.overlays.slice(0, 3).concat([e]) });
}
function setup(opts) {
  opts = opts || {};
  const ctx = makeSandbox(Object.assign({ dhm: DHM }, opts));
  const { map, ctl } = buildControl(ctx);
  return { ctx, map, ctl, D: ctx.window.MC_DHMOverlays };
}
const doc = { createElement: makeElement, createTextNode: makeTextNode };

console.log('\n=== #860 colour-by-elevation ramp for Denmark ===');

// ── 1. The stop table ───────────────────────────────────────────────────────

// The selectors enclosing each position of css, outermost first, comments
// skipped: a tiny brace walker, enough for public/style.css.
function selectorsAt(css) {
  const out = new Map();
  const stack = [];
  let start = 0;
  for (let i = 0; i < css.length; i++) {
    if (css.startsWith('/*', i)) { i = css.indexOf('*/', i + 2) + 1; start = i + 1; continue; }
    const c = css[i];
    if (c === '{') { stack.push(css.slice(start, i).trim()); start = i + 1; }
    else if (c === '}') { stack.pop(); start = i + 1; }
    else if (c === ';') start = i + 1;
    else if (css.startsWith('--dhm-elevation-', i)) out.set(i, stack.slice());
  }
  return out;
}

test('the CSS defines exactly one --dhm-elevation-<m> per stop, in a top-level :root only', () => {
  const css = fs.readFileSync(path.join(__dirname, 'public', 'style.css'), 'utf8');
  const all = Array.from(css.matchAll(/--dhm-elevation-(\d+):\s*(#[0-9a-f]{6});/gi)).map(m => Number(m[1]));
  assert.deepStrictEqual(all, LEGEND_M, 'variables ' + all.join(','));
  const where = selectorsAt(css);
  Array.from(css.matchAll(/--dhm-elevation-\d+:/g)).forEach(m => {
    assert.deepStrictEqual(where.get(m.index), [':root'], m[0] + ' in ' + JSON.stringify(where.get(m.index)));
  });
});

// ── 2. The ramp id in the tile URL ──────────────────────────────────────────

test('the elevation layer sends the ramp id as its WMS styles; the other overlays send none', () => {
  const { ctl } = setup();
  assert.strictEqual(ctl.overlays[LABEL].options.styles, STYLE);
  Object.keys(ctl.overlays).filter(n => n !== LABEL).forEach(n => {
    const o = ctl.overlays[n].options || {};
    if (o.layers !== undefined) assert.strictEqual(o.styles, '', n + ' styles ' + o.styles);
  });
});

test('a new ramp id from the server is a new tile URL', () => {
  const a = setup().ctl.overlays[LABEL].options;
  const b = setup({ dhm: withElevation({ style: 'ramp-ffff0000' }) }).ctl.overlays[LABEL].options;
  assert.notStrictEqual(a.styles, b.styles);
  assert.strictEqual(b.styles, 'ramp-ffff0000');
});

test('an entry without a well-formed ramp id is not offered', () => {
  [undefined, '', 7, null, 'Ramp-ABCD', 'ramp 1', 'x'.repeat(33), '<b>', 'ramp-1&layers=terrain'].forEach(style => {
    const { ctl } = setup({ dhm: withElevation({ style }) });
    assert.strictEqual(ctl.overlays[LABEL], undefined, 'offered with style ' + JSON.stringify(style));
  });
});

// ── 3. The legend ───────────────────────────────────────────────────────────

test('the bar gives every stop an equal step, from the CSS variables in order', () => {
  const { D } = setup();
  const bar = D.buildElevationLegend(doc, LEGEND_M).children[0];
  const parts = bar.style.background.replace(/^linear-gradient\(to right, /, '').replace(/\)$/, '').split(/, (?=var)/);
  assert.strictEqual(parts.length, LEGEND_M.length);
  parts.forEach((p, i) => {
    assert.strictEqual(p, 'var(--dhm-elevation-' + LEGEND_M[i] + ') ' + (i / (LEGEND_M.length - 1) * 100) + '%');
  });
  assert(!/#[0-9a-f]{3,8}\b|rgb/i.test(bar.style.background), 'a hardcoded colour');
});

test('the labels: 0 5 15 25 40 65 175 m, each at its stop', () => {
  const { D } = setup();
  assert.deepStrictEqual(Array.from(D.elevationLegendLabels(LEGEND_M)), [0, 5, 15, 25, 40, 65, 175]);
  const scale = D.buildElevationLegend(doc, LEGEND_M).children[1];
  assert.deepStrictEqual(scale.children.map(c => c.textContent), ['0', '5', '15', '25', '40', '65', '175 m']);
  assert.deepStrictEqual(scale.children.map(c => c.style.left),
    [0, 2, 4, 6, 8, 10, 14].map(i => (i / 14 * 100) + '%'));
  assert.deepStrictEqual(scale.children.map(c => c.style.transform || ''),
    ['', 'translateX(-50%)', 'translateX(-50%)', 'translateX(-50%)', 'translateX(-50%)', 'translateX(-50%)', 'translateX(-100%)']);
});

test('the label rule for any table: first and last, then a seventh apart and clear of the last', () => {
  const { D } = setup();
  for (let n = 2; n <= 16; n++) {
    const stops = Array.from({ length: n }, (_, i) => i * 10);
    const shown = Array.from(D.elevationLegendLabels(stops));
    const pos = shown.map(m => stops.indexOf(m) / (n - 1));
    assert.strictEqual(shown[0], 0, n + ' stops: first');
    assert.strictEqual(shown[shown.length - 1], stops[n - 1], n + ' stops: last');
    for (let i = 1; i < pos.length; i++) assert(pos[i] - pos[i - 1] >= 1 / 7 - 1e-9, n + ' stops: crowded ' + shown);
    pos.slice(1, -1).forEach(p => assert(p <= 0.8 + 1e-9, n + ' stops: crowds the last ' + shown));
    // Greedy: a stop left out is too close to the label before it, or to the last.
    stops.forEach((m, i) => {
      if (shown.indexOf(m) >= 0) return;
      const prev = Math.max.apply(null, pos.filter(p => p < i / (n - 1)));
      assert(i / (n - 1) - prev < 1 / 7 - 1e-9 || i / (n - 1) > 0.8 + 1e-9, n + ' stops: ' + m + ' left out');
    });
  }
  // The #792 table: every stop but 150 m, which would crowd the last.
  assert.deepStrictEqual(Array.from(D.elevationLegendLabels([0, 25, 50, 75, 100, 125, 150, 175])), [0, 25, 50, 75, 100, 125, 175]);
});

test('the aria-label names every stop', () => {
  const { D } = setup();
  const box = D.buildElevationLegend(doc, LEGEND_M);
  assert.strictEqual(box.attrs['aria-label'],
    'Elevation colours from 0 to 175 m, stops at 0, 2, 5, 10, 15, 20, 25, 30, 40, 50, 65, 80, 100, 130 and 175 m');
});

console.log('\n' + passed + ' passed, ' + failed + ' failed');
process.exit(failed ? 1 : 0);
