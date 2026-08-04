/**
 * Repeater Metric Scatter — an analytics tab plotting each repeater (or
 * room) as a point with two selectable #672 usefulness metrics on the axes
 * (ported from upstream Kpa-clawbot/CoreScope PR #1760, adapted to this
 * fork's full 4-axis #672 model — see the "Traffic vs. Usefulness" block
 * comment above REPEATER_METRIC_AXES in public/analytics.js).
 *
 * The metrics are NOT computed here: /api/nodes already attaches
 * bridge_score, coverage_score, redundancy_score, traffic_share_score, the
 * composite usefulness_score, and relay_count_1h/24h to repeater/room rows;
 * advert_count comes from the base node row. This tab only plots them.
 *
 * Follows this repo's established analytics.js test convention (same
 * technique as test-my-repeaters-dashboard.js): a named start/end marker
 * pair (plain indexOf, not a fragile regex-slice) delimits the renderer
 * block, which is then executed for real via `new Function` against named
 * stub globals — not reimplemented, not copied by hand.
 *
 * Three layers of coverage:
 *  - structural pins (file-grep) for tab WIRING that can't run without a DOM;
 *  - BEHAVIORAL tests executing the pure pipeline (REPEATER_METRIC_AXES …
 *    renderMetricScatter) against fixtures;
 *  - BEHAVIORAL tests executing the full async renderRepeaterMetricsTab
 *    against a minimal-but-real DOM stub (id-addressable children,
 *    working addEventListener) for default axes, region/area filter
 *    propagation, and interactive axis-change re-render + persistence.
 */
'use strict';

const fs = require('fs');
const path = require('path');

let passed = 0, failed = 0;
function assert(cond, msg) {
  if (cond) { passed++; console.log('  ✓ ' + msg); }
  else { failed++; console.error('  ✗ ' + msg); }
}

const src = fs.readFileSync(path.join(__dirname, 'public', 'analytics.js'), 'utf8');

console.log('\n=== tab wiring (structural — not executable without a DOM) ===');
assert(/data-tab="repeater-metrics"[^>]*>\s*Repeater Metrics\s*</.test(src),
  'tab bar has a "Repeater Metrics" button');
assert(/case 'repeater-metrics':\s*await renderRepeaterMetricsTab\(el\)/.test(src),
  'renderTab dispatches repeater-metrics');
assert(/AREA_FILTER_TABS[\s\S]{0,250}'repeater-metrics'/.test(src),
  'repeater-metrics participates in region/area filtering');
// --- Extract and execute the pure render pipeline with stub globals
//     (same named-marker + new Function technique as test-my-repeaters-dashboard.js). ---
const start = src.indexOf('const REPEATER_METRIC_AXES');
const end = src.indexOf('// === REPEATER METRICS BLOCK END');
if (start < 0 || end < 0) {
  console.error('  ✗ could not locate the REPEATER_METRIC_AXES … renderRepeaterMetricsTab block');
  process.exit(1);
}
const block = src.slice(start, end);

// Scoped to THIS tab's own block, not the whole file: a pre-existing,
// unrelated helper elsewhere in analytics.js (My Repeaters' `trafficOf`,
// line ~2479) still has the old fallback chain and is out of scope for this
// port — see the report for that finding. This assertion only guards
// against the fallback creeping back into the Repeater Metrics code itself.
assert(!/traffic_share_score\s*!=\s*null\s*\?\s*n\.traffic_share_score\s*:\s*\(n\.usefulness_score/.test(block),
  'this tab\'s _toScatterPoints must NOT fall back traffic to usefulness_score (the #1760 semantic fix)');
const esc = s => s ? String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;') : '';
const windowStub = { ROLE_COLORS: { repeater: '#3b82f6', room: '#a855f7' } };
let _regionQS = '', _areaQS = '';
const RegionFilter = { regionQueryString: () => _regionQS };
const AreaFilter = { areaQueryString: () => _areaQS };
let nodesData = [];
let lastFetchArg = null;
const fetchAllNodes = async (arg) => { lastFetchArg = arg; return { nodes: nodesData }; };
let favorites = [];
const getFavorites = () => favorites.slice();
const CLIENT_TTL = { nodeList: 0 };
const fakeLocalStorage = (() => {
  const s = {};
  return { getItem: k => (k in s ? s[k] : null), setItem: (k, v) => { s[k] = String(v); }, removeItem: k => { delete s[k]; } };
})();

const M = new Function(
  'esc', 'window', 'RegionFilter', 'AreaFilter', 'fetchAllNodes', 'getFavorites', 'CLIENT_TTL', 'localStorage',
  block + '\nreturn { REPEATER_METRIC_AXES, _niceCeil, _axisFmt, _axisFromMax, _resolveAxis, _toScatterPoints, renderMetricScatter, renderRepeaterMetricsTab };')(
  esc, windowStub, RegionFilter, AreaFilter, fetchAllNodes, getFavorites, CLIENT_TTL, fakeLocalStorage);

console.log('\n=== [1] axis registry: all 8 axes exist and read the right point fields ===');
assert(M.REPEATER_METRIC_AXES.map(a => a.key).join(',') === 'traffic,usefulness,bridge,coverage,redundancy,relay1h,relay24h,adverts',
  'axis registry exposes exactly the 8 required metrics, in order');
{
  const sentinel = { traffic: 1, usefulness: 2, bridge: 3, coverage: 4, redundancy: 5, relay1h: 6, relay24h: 7, adverts: 8 };
  M.REPEATER_METRIC_AXES.forEach((a, i) => {
    assert(a.get(sentinel) === i + 1, `axis "${a.key}" getter reads its own point.${a.key} field, not another axis's`);
  });
}
assert(M.REPEATER_METRIC_AXES.every(a => typeof a.score === 'boolean'),
  'every axis declares score:true/false (percent vs. integer formatting)');
assert(M.REPEATER_METRIC_AXES.filter(a => a.score).map(a => a.key).join(',') === 'traffic,usefulness,bridge,coverage,redundancy',
  'the five #672 score axes are marked score:true; the three count axes are not');

console.log('\n=== [2] default axes: X=Traffic share, Y=Usefulness ===');
assert(/xKey\s*=\s*localStorage\.getItem\('meshcore-repeater-scatter-x'\)\s*\|\|\s*'traffic'/.test(block),
  'default X axis key is "traffic"');
assert(/yKey\s*=\s*localStorage\.getItem\('meshcore-repeater-scatter-y'\)\s*\|\|\s*'usefulness'/.test(block),
  'default Y axis key is "usefulness"');

console.log('\n=== [3] traffic_share_score and usefulness_score stay independent (the core semantic fix) ===');
{
  const favSet = new Set(['FAV0000000000000']);
  const mapped = M._toScatterPoints([
    { public_key: 'FAV0000000000000', name: 'Fav Rptr', role: 'repeater',
      traffic_share_score: 0.5, usefulness_score: 0.31, bridge_score: 0.2, coverage_score: 0.4, redundancy_score: 0.6,
      relay_count_1h: 1, relay_count_24h: 2, advert_count: 3 },
    { public_key: 'ONLYUSEFULNESS00', name: 'Composite Only', role: 'room', usefulness_score: 0.07 },
    { public_key: 'NOSCORES00000000', name: 'Bare', role: 'repeater' },
    { public_key: 'NAMELESS00000000ABCDEF', role: 'repeater' },
    { public_key: 'COMPANION0000000', name: 'Phone', role: 'companion', traffic_share_score: 0.9 },
    { role: 'repeater' },
  ], favSet);

  console.log('\n=== [5] repeater/room included, other roles filtered out ===');
  assert(mapped.length === 5 && !mapped.some(p => p.role === 'companion'),
    'non-repeater/room roles are filtered out');

  console.log('\n=== [3] (continued) ===');
  assert(mapped[0].traffic === 0.5 && mapped[0].usefulness === 0.31,
    'traffic_share_score and usefulness_score map to two DIFFERENT point fields with their own distinct values');
  assert(mapped[0].traffic !== mapped[0].usefulness,
    'a node with different traffic_share_score and usefulness_score is never collapsed to one value');
  assert(mapped[1].traffic === null,
    'a node with ONLY usefulness_score set does NOT have that value leak into the traffic axis (no fallback)');
  assert(mapped[1].usefulness === 0.07,
    'usefulness_score itself is still read correctly on that same node');

  console.log('\n=== [4] coverage and redundancy map correctly ===');
  assert(mapped[0].coverage === 0.4 && mapped[0].redundancy === 0.6,
    'coverage_score → point.coverage, redundancy_score → point.redundancy');
  assert(mapped[0].bridge === 0.2 && mapped[0].relay1h === 1 && mapped[0].relay24h === 2 && mapped[0].adverts === 3,
    'bridge/relay/advert counts map onto their point fields (advert_count → adverts)');
  assert(mapped[0].fav === true && mapped[1].fav === false,
    'favorite flag is set from the provided favorites Set');

  console.log('\n=== [6] missing values become null, not 0 ===');
  assert(mapped[2].traffic === null && mapped[2].usefulness === null && mapped[2].bridge === null &&
         mapped[2].coverage === null && mapped[2].redundancy === null && mapped[2].relay1h === null &&
         mapped[2].relay24h === null && mapped[2].adverts === null,
    'a node with no scores at all maps every metric to null (not 0/undefined) so plots can skip it');
  assert(mapped[3].name === 'NAMELESS0000',
    'nameless node falls back to a 12-char pubkey prefix');
  assert(mapped[4].name === '?' && mapped[4].pk === undefined,
    'node with neither name nor pubkey falls back to "?"');
}

console.log('\n=== [8] axis formatting: scores as %, counts as integers ===');
assert(M._niceCeil(0) === 1 && M._niceCeil(0.07) === 0.1 && M._niceCeil(37) === 50,
  '_niceCeil rounds up to 1/2/5×10ⁿ');
assert(M._niceCeil(-1) === 1 && M._niceCeil(NaN) === 1 && M._niceCeil(-0.5) === 1,
  '_niceCeil guards NaN/negative/zero → 1');
assert(M._axisFmt({ score: true }, 0.1234) === '12.3%' && M._axisFmt({ score: false }, 5) === '5',
  '_axisFmt renders scores as % and counts as integers');
assert(M._axisFmt({ score: true }, 0.2) === '20%' && M._axisFmt({ score: true }, 0) === '0%',
  '_axisFmt drops the trailing .0 on whole-percent gridlines');
assert(M._axisFmt({ score: true }, null) === '—' && M._axisFmt({ score: false }, null) === '—',
  '_axisFmt renders a missing value as an em dash on either axis kind');
assert(M._axisFmt({ score: true }, 0.0002) === '0.02%' && M._axisFmt({ score: true }, 0.00005) === '0.01%',
  '_axisFmt review fix: sub-1% values get a 2nd decimal so a run of tiny Traffic share gridlines is never all "0.0%"');
assert(M._axisFmt({ score: true }, 0.033) === '3.3%',
  '_axisFmt keeps 1 decimal for non-integer values ≥1% (not needlessly precise)');

console.log('\n=== [9] unknown/stale localStorage axis key falls back safely ===');
{
  const bogus = M._resolveAxis('bogus-stale-key', [{ traffic: 0.4 }]);
  assert(bogus.key === 'traffic' && bogus.max > 0,
    "_resolveAxis('bogus-stale-key') falls back to the first axis (traffic) with a real domain, not a throw/undefined");
}

console.log('\n=== [7]/[10]/[11] scatter render: both-values-required, escaping, node links ===');
{
  const pts = [
    { pk: 'AA', name: 'Rptr Eins', role: 'repeater', fav: true, traffic: 0.42, usefulness: 0.55, bridge: 0.1, coverage: 0.2, redundancy: 0.3, relay1h: 12, relay24h: 200, adverts: 50 },
    { pk: 'BB', name: 'Raum <Zwei>', role: 'room', fav: false, traffic: 0.05, usefulness: 0.2, bridge: 0.8, coverage: 0.1, redundancy: 0.1, relay1h: 0, relay24h: 3, adverts: 5 },
    { pk: 'CC', name: 'Rptr Drei', role: 'repeater', fav: false, traffic: null, usefulness: 0.4, bridge: 0.3, coverage: 0.3, redundancy: 0.3, relay1h: 4, relay24h: 40, adverts: 9 },
  ];
  const xa = M._resolveAxis('traffic', pts), ya = M._resolveAxis('bridge', pts);
  const svg = M.renderMetricScatter(pts, xa, ya);
  assert(svg.startsWith('<svg') && svg.endsWith('</svg>') && !/NaN/.test(svg),
    'produces a valid <svg> with no NaN coordinates');
  assert((svg.match(/href="#\/nodes\//g) || []).length === 2,
    'plots only points with BOTH axis values present (CC has null traffic → skipped)');
  assert(/href="#\/nodes\/AA\/analytics"/.test(svg),
    'each point links to its per-node analytics page');
  assert(/fill="#3b82f6"/.test(svg) && /fill="#a855f7"/.test(svg),
    'point fill follows the ROLE_COLORS node-role colour');
  assert(/Raum &lt;Zwei&gt;/.test(svg),
    'point names are HTML-escaped in the tooltip');
  assert(/stroke="var\(--text\)"/.test(svg),
    'favorite ring uses the neutral var(--text), not a status colour');
  assert(/tabindex="-1"/.test(svg), 'points carry tabindex="-1" (out of sequential keyboard tab order)');

  console.log('\n=== [15] empty axis combination shows a message, not a blank plot ===');
  const empties = [{ pk: 'E', name: 'e', role: 'repeater', fav: false, traffic: null, bridge: null }];
  const emptySvg = M.renderMetricScatter(empties, M._resolveAxis('traffic', empties), M._resolveAxis('bridge', empties));
  assert(/No repeaters have values/.test(emptySvg),
    'an axis combination with no plottable points renders an explicit message');
  assert(!/showing \d+ of \d+ points/.test(svg),
    'no sampling disclosure is shown for a small point set');
}

console.log('\n=== [12]/[13]/[14] favorites survive sampling; ~2000 cap holds even under an all-favorite set ===');
{
  const many = [];
  for (let i = 0; i < 2500; i++) many.push({ pk: 'p' + i, name: 'n' + i, role: 'repeater', fav: (i % 500 === 0), traffic: i / 2500, bridge: (i % 7) / 7 });
  const favCount = many.filter(p => p.fav).length;
  const bigSvg = M.renderMetricScatter(many, M._resolveAxis('traffic', many), M._resolveAxis('bridge', many));
  assert(/showing \d+ of 2500 points/.test(bigSvg),
    'sampling >2000 points is disclosed in-plot ("showing N of 2500 points")');
  const plotted = (bigSvg.match(/href="#\/nodes\//g) || []).length;
  assert(plotted <= 2000 + favCount && plotted > 1000, 'plotted point count respects the ~2000 cap');
  const allFavsShown = many.filter(p => p.fav).every(p => bigSvg.includes('#/nodes/' + p.pk + '/analytics'));
  assert(allFavsShown, 'ALL favorites survive sampling — the legend favorite count is never a lie');

  const allFav = [];
  for (let i = 0; i < 2500; i++) allFav.push({ pk: 'q' + i, name: 'n' + i, role: 'repeater', fav: true, traffic: i / 2500, bridge: 0.5 });
  const allFavSvg = M.renderMetricScatter(allFav, M._resolveAxis('traffic', allFav), M._resolveAxis('bridge', allFav));
  const allFavPlotted = (allFavSvg.match(/href="#\/nodes\//g) || []).length;
  assert(allFavPlotted <= 2000 && allFavPlotted > 1000,
    '2500 favorites are themselves strided to respect the cap — the cap cannot be bypassed by favoriting everything');
}

function makeElement(id) {
  const children = {};
  return {
    id: id || '',
    _listeners: {},
    value: '',
    style: {},
    get innerHTML() { return this._html || ''; },
    set innerHTML(html) {
      this._html = html;
      const re = /id="([^"]+)"/g;
      let m;
      while ((m = re.exec(html))) { if (!children[m[1]]) children[m[1]] = makeElement(m[1]); }
    },
    querySelector(sel) {
      const m = sel.match(/^#(.+)$/);
      return m ? (children[m[1]] || null) : null;
    },
    querySelectorAll() { return []; },
    addEventListener(type, fn) { (this._listeners[type] = this._listeners[type] || []).push(fn); },
    fireChange() { (this._listeners.change || []).forEach(fn => fn({ target: this })); },
  };
}

(async () => {
  console.log('\n=== [2] (full render) default axes selected with an empty localStorage ===');
  {
    fakeLocalStorage.removeItem('meshcore-repeater-scatter-x');
    fakeLocalStorage.removeItem('meshcore-repeater-scatter-y');
    nodesData = [{ public_key: 'AA', name: 'A', role: 'repeater', traffic_share_score: 0.4, usefulness_score: 0.5 }];
    favorites = [];
    _regionQS = ''; _areaQS = '';
    const el = makeElement('root');
    await M.renderRepeaterMetricsTab(el);
    assert(/id="metricScatterX"[\s\S]{0,400}option value="traffic" selected/.test(el.innerHTML),
      'rendered X-axis select defaults to "Traffic share"');
    assert(/id="metricScatterY"[\s\S]{0,400}option value="usefulness" selected/.test(el.innerHTML),
      'rendered Y-axis select defaults to "Usefulness"');
  }

  console.log('\n=== [9] (full render) a stale/unknown stored axis key does not throw ===');
  {
    fakeLocalStorage.setItem('meshcore-repeater-scatter-x', 'no-such-axis-anymore');
    const el = makeElement('root');
    let threw = false;
    try { await M.renderRepeaterMetricsTab(el); } catch (e) { threw = true; }
    assert(!threw, 'a stale localStorage axis key does not crash the render');
    fakeLocalStorage.removeItem('meshcore-repeater-scatter-x');
  }

  console.log('\n=== [16] region and area filters are included in the node query ===');
  {
    _regionQS = '&region=dk-mj';
    _areaQS = '&area=se12-kladdarp';
    lastFetchArg = null;
    const el = makeElement('root');
    await M.renderRepeaterMetricsTab(el);
    assert(typeof lastFetchArg === 'string' && lastFetchArg.includes('&region=dk-mj'),
      'the node fetch carries the active region filter');
    assert(typeof lastFetchArg === 'string' && lastFetchArg.includes('&area=se12-kladdarp'),
      'the node fetch carries the active area filter');
    _regionQS = ''; _areaQS = '';
  }

  console.log('\n=== [17] changing X or Y axis redraws the plot and persists the choice ===');
  {
    nodesData = [
      { public_key: 'AA', name: 'A', role: 'repeater', traffic_share_score: 0.9, usefulness_score: 0.1, bridge_score: 0.05 },
      { public_key: 'BB', name: 'B', role: 'repeater', traffic_share_score: 0.1, usefulness_score: 0.2, bridge_score: 0.95 },
    ];
    const el = makeElement('root');
    await M.renderRepeaterMetricsTab(el);
    const plotBefore = el.querySelector('#metricScatterPlot').innerHTML;
    const xSel = el.querySelector('#metricScatterX');
    xSel.value = 'bridge';
    xSel.fireChange();
    const plotAfter = el.querySelector('#metricScatterPlot').innerHTML;
    assert(plotAfter !== plotBefore, 'changing the X axis redraws the plot with different content');
    assert(fakeLocalStorage.getItem('meshcore-repeater-scatter-x') === 'bridge',
      'the new X axis choice is persisted to localStorage');

    const ySel = el.querySelector('#metricScatterY');
    ySel.value = 'coverage';
    ySel.fireChange();
    assert(fakeLocalStorage.getItem('meshcore-repeater-scatter-y') === 'coverage',
      'the new Y axis choice is persisted to localStorage');
  }

  console.log('\n=== empty node set shows an explanatory empty state ===');
  {
    nodesData = [{ public_key: 'C1', name: 'Companion', role: 'companion', traffic_share_score: 0.9 }];
    const el = makeElement('root');
    await M.renderRepeaterMetricsTab(el);
    assert(/No repeater or room nodes/.test(el.innerHTML),
      'a node set with no repeater/room rows shows an explicit empty state');
  }

  console.log('\n────────────────────────────────────────');
  console.log(`  ${passed} passed, ${failed} failed`);
  if (failed) process.exit(1);
})();
