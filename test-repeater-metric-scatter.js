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

console.log('\n=== [S0] Scope Analytics stays fully removed — this re-fix must not resurrect it ===');
[
  'unscopedRelays24h', 'unscopedRatio24h', 'scopesObserved', 'scopesRecent',
  'REPEATER_METRIC_AXIS_GROUPS', 'SCOPE_FILTERS', '_scopeFilterPredicate',
  'REPEATER_METRIC_PRESETS', 'metricScatterScopeFilter', 'metricScatterPreset',
  'Scope health', 'meshcore-repeater-scatter-scope-filter',
].forEach(sym => {
  assert(!src.includes(sym), `Scope Analytics symbol/string "${sym}" is absent from analytics.js`);
});

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
// Busiest Repeaters formats estimated airtime with the real ObserverAirtime.
new Function('window', fs.readFileSync(path.join(__dirname, 'public', 'observer-airtime.js'), 'utf8'))(windowStub);
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

// Busiest Repeaters (the tab's second section) keeps its URL view-param
// specs next to the other tabs' ones, outside the block: run the real ones.
const specStart = src.indexOf('var BUSIEST_SORT');
const specEnd = src.indexOf('// === BUSIEST SPECS END');
if (specStart < 0 || specEnd < 0) {
  console.error('  ✗ could not locate the BUSIEST_* view-param specs');
  process.exit(1);
}
const SPECS = new Function(src.slice(specStart, specEnd) + '\nreturn { BUSIEST_SORT, BUSIEST_OBSERVERS, BUSIEST_LIMIT };')();
let observersData = { observers: [] };
let lastApiPath = null;
const api = async (p) => { lastApiPath = p; return observersData; };
// The URL as restoreViewParams/setViewParam would read and write it.
const viewParams = {};
const restoreViewParams = specs => specs.map(s => {
  const v = viewParams[s.param];
  return v != null && s.allowed.indexOf(v) >= 0 ? v : s.dflt;
});
const setViewParam = (spec, v) => { viewParams[spec.param] = v; };
CLIENT_TTL.observers = 0;

const M = new Function(
  'esc', 'window', 'RegionFilter', 'AreaFilter', 'fetchAllNodes', 'getFavorites', 'CLIENT_TTL', 'localStorage',
  'api', 'restoreViewParams', 'setViewParam', 'BUSIEST_SORT', 'BUSIEST_OBSERVERS', 'BUSIEST_LIMIT',
  block + '\nreturn { REPEATER_METRIC_AXES, REPEATER_METRIC_POINT_CAP, _niceCeil, _axisFmt, _axisFromMax, _resolveAxis, _toScatterPoints, _sampleExact, _sampleForPlot, renderMetricScatter, renderRepeaterMetricsTab, _busiestRepeaterRows, renderBusiestRepeatersTable, BUSIEST_DEFAULT_ROWS };')(
  esc, windowStub, RegionFilter, AreaFilter, fetchAllNodes, getFavorites, CLIENT_TTL, fakeLocalStorage,
  api, restoreViewParams, setViewParam, SPECS.BUSIEST_SORT, SPECS.BUSIEST_OBSERVERS, SPECS.BUSIEST_LIMIT);

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

console.log('\n=== [S1] _sampleExact: exactness, no duplicates, full-range coverage, budget edges ===');
{
  assert(M.REPEATER_METRIC_POINT_CAP === 2000, 'REPEATER_METRIC_POINT_CAP is 2000');

  const arr = Array.from({ length: 37 }, (_, i) => i);
  assert(M._sampleExact(arr, 0).length === 0, '_sampleExact(arr, 0) returns an empty array');
  assert(M._sampleExact(arr, -5).length === 0, '_sampleExact(arr, negative budget) returns an empty array, not a throw');

  const full = M._sampleExact(arr, 37);
  assert(full.length === 37 && full.every((v, i) => v === i), '_sampleExact(arr, arr.length) returns an exact full copy, in order');

  const over = M._sampleExact(arr, 1000);
  assert(over.length === 37 && over.every((v, i) => v === i), '_sampleExact(arr, budget > length) is capped at length — never fabricates elements');

  const budget = 10;
  const sampled = M._sampleExact(arr, budget);
  assert(sampled.length === budget, `_sampleExact returns exactly the requested budget (${budget}) when budget < length`);
  assert(new Set(sampled).size === sampled.length, '_sampleExact never returns duplicate elements');
  assert(sampled.every(v => arr.includes(v)), '_sampleExact never fabricates elements absent from the input');
  assert(sampled[0] === 0, '_sampleExact always includes the very first element of the range');
  assert(sampled[sampled.length - 1] > arr.length / 2, '_sampleExact spreads across the WHOLE input range, not just the head');
  assert(sampled.every((v, i) => i === 0 || v > sampled[i - 1]),
    '_sampleExact indices are strictly increasing for an ascending input — proof of even spacing, not a clustered head-only slice');

  const again = M._sampleExact(arr, budget);
  assert(JSON.stringify(again) === JSON.stringify(sampled), '_sampleExact is deterministic — identical input always yields the identical sample');
}

console.log('\n=== [S2] _sampleForPlot: eligible/shown definitions ===');
{
  const pts = [
    { pk: 'A', fav: false, traffic: 0.1, bridge: 0.2 },
    { pk: 'B', fav: true,  traffic: 0.3, bridge: null },  // missing Y -> not eligible
    { pk: 'C', fav: true,  traffic: null, bridge: 0.4 },  // missing X -> not eligible
    { pk: 'D', fav: true,  traffic: 0.5, bridge: 0.6 },
  ];
  const xa = M._resolveAxis('traffic', pts), ya = M._resolveAxis('bridge', pts);
  const { eligible, shown } = M._sampleForPlot(pts, xa, ya);
  assert(eligible.length === 2 && eligible.every(p => p.pk === 'A' || p.pk === 'D'),
    'eligible = points with values for BOTH selected axes (B and C are excluded, each missing one)');
  assert(shown.length === eligible.length && shown.every(p => eligible.includes(p)),
    'when eligible.length <= cap, shown is exactly eligible — no sampling needed');
  const eligibleFavs = eligible.filter(p => p.fav);
  assert(eligibleFavs.length === 1 && eligibleFavs[0].pk === 'D',
    'a favorite missing either axis value (B, C) does not count as an eligible favorite');
}

console.log('\n=== [S3] _sampleForPlot at scale: favorite preservation and the hard cap ===');
{
  const many = [];
  for (let i = 0; i < 2500; i++) many.push({ pk: 'p' + i, fav: (i % 500 === 0), traffic: i / 2500, bridge: (i % 7) / 7 });
  const xa = M._resolveAxis('traffic', many), ya = M._resolveAxis('bridge', many);
  const { eligible, shown } = M._sampleForPlot(many, xa, ya);
  assert(eligible.length === 2500, 'eligible reflects the full input when every point has both axis values');
  assert(shown.length === 2000, 'shown is capped at exactly REPEATER_METRIC_POINT_CAP (2000) once eligible exceeds it');
  const favsIn = many.filter(p => p.fav);
  assert(favsIn.every(p => shown.includes(p)), 'all 5 favorites (well under the cap, out of 2500) survive sampling into shown');

  const allFav = [];
  for (let i = 0; i < 2500; i++) allFav.push({ pk: 'q' + i, fav: true, traffic: i / 2500, bridge: 0.5 });
  const r2 = M._sampleForPlot(allFav, xa, ya);
  assert(r2.shown.length === 2000, '2500 favorites are themselves sampled down to exactly the 2000 cap');
  assert(r2.shown.every(p => p.fav), 'when favorites alone exceed the cap, shown contains ONLY favorites — no non-favorite squeezed in');
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
    fireClick() { (this._listeners.click || []).forEach(fn => fn({ target: this })); },
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

  console.log('\n=== [S4] (full render) legend favorite text: plain count when nothing is sampled away ===');
  {
    nodesData = [
      { public_key: 'F1', name: 'Fav One', role: 'repeater', traffic_share_score: 0.1, usefulness_score: 0.2 },
      { public_key: 'F2', name: 'Fav Two', role: 'repeater', traffic_share_score: 0.3, usefulness_score: 0.4 },
      { public_key: 'N1', name: 'Plain', role: 'repeater', traffic_share_score: 0.5, usefulness_score: 0.6 },
    ];
    favorites = ['F1', 'F2'];
    fakeLocalStorage.setItem('meshcore-repeater-scatter-x', 'traffic');
    fakeLocalStorage.setItem('meshcore-repeater-scatter-y', 'usefulness');
    const el = makeElement('root');
    await M.renderRepeaterMetricsTab(el);
    // The fake-DOM <select>.value doesn't derive from rendered "selected"
    // markup, so the auto-run initial draw() falls back to axis[0] for BOTH
    // X and Y — force the intended X/Y pair with an explicit change event.
    el.querySelector('#metricScatterX').value = 'traffic';
    const ySel4 = el.querySelector('#metricScatterY');
    ySel4.value = 'usefulness';
    ySel4.fireChange();
    const legend = el.querySelector('#metricScatterLegend').innerHTML;
    assert(/Favorites shown: 2(?!\s*of)/.test(legend),
      'legend shows a plain "Favorites shown: 2" when nothing was sampled away');
    favorites = [];
  }

  console.log('\n=== [S5] (full render) a favorite missing the selected axis value is not counted as shown ===');
  {
    nodesData = [
      { public_key: 'F1', name: 'Fav Complete', role: 'repeater', traffic_share_score: 0.1, usefulness_score: 0.2 },
      { public_key: 'F2', name: 'Fav Incomplete', role: 'repeater', traffic_share_score: 0.3 }, // no usefulness_score
    ];
    favorites = ['F1', 'F2'];
    fakeLocalStorage.setItem('meshcore-repeater-scatter-x', 'traffic');
    fakeLocalStorage.setItem('meshcore-repeater-scatter-y', 'usefulness');
    const el = makeElement('root');
    await M.renderRepeaterMetricsTab(el);
    el.querySelector('#metricScatterX').value = 'traffic';
    const ySel5 = el.querySelector('#metricScatterY');
    ySel5.value = 'usefulness';
    ySel5.fireChange();
    const legend = el.querySelector('#metricScatterLegend').innerHTML;
    assert(/Favorites shown: 1(?!\s*of)/.test(legend),
      'a favorite missing the Y-axis value is excluded from BOTH the eligible and shown favorite counts');
    favorites = [];
  }

  console.log('\n=== [S6] (full render) favorites past the cap show "Favorites shown: N of M eligible" ===');
  {
    const many = [];
    for (let i = 0; i < 2500; i++) many.push({ public_key: 'p' + i, name: 'n' + i, role: 'repeater', traffic_share_score: i / 2500, usefulness_score: (i % 7) / 7 });
    nodesData = many;
    favorites = many.map(n => n.public_key); // every node favorited -> 2500 eligible favorites, cap = 2000
    fakeLocalStorage.setItem('meshcore-repeater-scatter-x', 'traffic');
    fakeLocalStorage.setItem('meshcore-repeater-scatter-y', 'usefulness');
    const el = makeElement('root');
    await M.renderRepeaterMetricsTab(el);
    el.querySelector('#metricScatterX').value = 'traffic';
    const ySel6 = el.querySelector('#metricScatterY');
    ySel6.value = 'usefulness';
    ySel6.fireChange();
    const legend = el.querySelector('#metricScatterLegend').innerHTML;
    assert(/Favorites shown: 2000 of 2500 eligible/.test(legend),
      'legend discloses "Favorites shown: 2000 of 2500 eligible" once sampling drops favorites past the cap');
    const plot = el.querySelector('#metricScatterPlot').innerHTML;
    assert(/showing 2000 of 2500 points/.test(plot),
      'the in-plot sampling disclosure agrees with the legend — same shown/eligible numbers, one source of truth');
    favorites = [];
  }

  console.log('\n=== [S7] (full render) legend role list reflects only eligible (plottable) points ===');
  {
    nodesData = [
      { public_key: 'R1', name: 'Repeater With Both', role: 'repeater', traffic_share_score: 0.2, usefulness_score: 0.3 },
      { public_key: 'M1', name: 'Room Missing Y', role: 'room', traffic_share_score: 0.4 }, // no usefulness_score -> not eligible
    ];
    favorites = [];
    fakeLocalStorage.setItem('meshcore-repeater-scatter-x', 'traffic');
    fakeLocalStorage.setItem('meshcore-repeater-scatter-y', 'usefulness');
    const el = makeElement('root');
    await M.renderRepeaterMetricsTab(el);
    el.querySelector('#metricScatterX').value = 'traffic';
    const ySel7 = el.querySelector('#metricScatterY');
    ySel7.value = 'usefulness';
    ySel7.fireChange();
    const legend = el.querySelector('#metricScatterLegend').innerHTML;
    assert(/fill="#3b82f6"/.test(legend), 'legend includes the repeater role swatch (eligible)');
    assert(!/fill="#a855f7"/.test(legend), 'legend excludes the room role swatch — the only room node is not eligible for the selected axes');
  }

  console.log('\n=== empty node set shows an explanatory empty state ===');
  {
    nodesData = [{ public_key: 'C1', name: 'Companion', role: 'companion', traffic_share_score: 0.9 }];
    const el = makeElement('root');
    await M.renderRepeaterMetricsTab(el);
    assert(/No repeater or room nodes/.test(el.innerHTML),
      'a node set with no repeater/room rows shows an explicit empty state');
  }

  console.log('\n=== [B1] Busiest Repeaters rows: who is listed, in which order ===');
  {
    const nodes = [
      { public_key: 'aa01', name: 'Alpha', role: 'repeater', relay_count_1h: 10, relay_count_24h: 300 },
      { public_key: 'bb02', name: 'Bravo', role: 'repeater', relay_count_1h: 40, relay_count_24h: 200 },
      { public_key: 'cc03', name: 'Charlie', role: 'room', relay_count_1h: '5', relay_count_24h: '300' },
      { public_key: 'dd04', name: 'Delta', role: 'repeater', relay_count_1h: 0, relay_count_24h: 0 },
      { public_key: 'ee05', name: 'Echo', role: 'companion', relay_count_1h: 99, relay_count_24h: 999 },
      { public_key: 'FF06', name: 'Foxtrot', role: 'repeater', relay_count_1h: 1, relay_count_24h: 500 },
    ];
    const obs = new Set(['ff06']);
    const r24 = M._busiestRepeaterRows(nodes, obs, '24h', false);
    assert(r24.map(r => r.name).join(',') === 'Alpha,Charlie,Bravo',
      '24h: repeaters and rooms with relays, most first; ties go to the 1h count; got ' + r24.map(r => r.name).join(','));
    assert(r24[1].relay24h === 300 && r24[1].relay1h === 5, 'string counts from the API are cast to numbers');
    const r1 = M._busiestRepeaterRows(nodes, obs, '1h', false);
    assert(r1.map(r => r.name).join(',') === 'Bravo,Alpha,Charlie', '1h: ranked by the last hour; got ' + r1.map(r => r.name).join(','));
    const withObs = M._busiestRepeaterRows(nodes, obs, '24h', true);
    assert(withObs[0].name === 'Foxtrot' && withObs[0].observer === true,
      'an observer repeater is listed when included, matched on a lower-cased public key');
    assert(withObs.filter(r => r.observer).length === 1, 'only the observer repeater is marked as one');
    assert(M._busiestRepeaterRows(nodes, new Set(), '24h', false).length === 4,
      'without an observer list nothing is hidden');
  }

  console.log('\n=== [B2] Busiest Repeaters table markup ===');
  {
    const rows = Array.from({ length: 60 }, (_, i) => ({ pk: 'k' + i, name: 'R' + i, role: 'repeater', relay1h: 1, relay24h: 1000 - i, observer: false }));
    rows[0].name = '<img src=x onerror=alert(1)>';
    const top = M.renderBusiestRepeatersTable(rows, '24h', '50');
    assert((top.match(/<tr><td/g) || []).length === M.BUSIEST_DEFAULT_ROWS, 'the default limit shows the top 50 rows');
    assert(!top.includes('<img src=x') && top.includes('&lt;img src=x'), 'a node name is escaped');
    assert(/id="busiestLimitToggle"[^>]*>Show all 60</.test(top), 'a "Show all 60" button follows the truncated list');
    assert(/aria-sort="descending">24h</.test(top) && !/aria-sort="descending">1h</.test(top),
      'the sorted column is marked aria-sort="descending"');
    const all = M.renderBusiestRepeatersTable(rows, '1h', 'all');
    assert((all.match(/<tr><td/g) || []).length === 60 && /Show top 50</.test(all), '"all" shows every row and offers the top 50 again');
    assert(/aria-sort="descending">1h</.test(all), 'sorting by 1h marks the 1h column');
    const few = M.renderBusiestRepeatersTable(rows.slice(0, 3), '24h', '50');
    assert(!few.includes('busiestLimitToggle'), 'no limit button when every row fits');
    assert(/No repeater in this view relayed/.test(M.renderBusiestRepeatersTable([], '24h', '50')), 'an empty list says so');
    assert(/<td style="text-align:right">1<\/td><td[^>]*><a href="#\/nodes\/k0\/analytics"/.test(top), 'rows are numbered and link to the node analytics');
    assert(/^<div class="analytics-table-scroll"><table class="analytics-table busiest-repeaters-table"/.test(top),
      'the table carries its own #206 scroll wrapper, so a redraw keeps it, and the class that lifts the 480px minimum');
  }

  console.log('\n=== [B3] Busiest Repeaters in the rendered tab: URL state and controls ===');
  {
    nodesData = [
      { public_key: 'aa01', name: 'Alpha', role: 'repeater', relay_count_1h: 10, relay_count_24h: 300 },
      { public_key: 'bb02', name: 'Bravo', role: 'repeater', relay_count_1h: 40, relay_count_24h: 200 },
      { public_key: 'ff06', name: 'ObsRep', role: 'repeater', relay_count_1h: 1, relay_count_24h: 900 },
    ];
    observersData = { observers: [{ id: 'FF06', name: 'ObsRep' }] };
    Object.keys(viewParams).forEach(k => { delete viewParams[k]; });
    const el = makeElement('root');
    await M.renderRepeaterMetricsTab(el);
    assert(lastApiPath === '/observers', 'the tab fetches the observer list');
    const table = () => el.querySelector('#busiestRepeatersTable').innerHTML;
    const names = () => Array.from(table().matchAll(/class="analytics-link">([^<]+)</g), m => m[1]).join(',');
    assert(names() === 'Alpha,Bravo', 'default: 24h order with the observer repeater hidden; got ' + names());
    assert(/Include observer repeaters \(1\)/.test(el.innerHTML), 'the checkbox shows how many observer repeaters are hidden');
    assert(/option value="24h" selected/.test(el.innerHTML), 'the sort select opens on 24h');

    const sortSel = el.querySelector('#busiestSort');
    sortSel.value = '1h';
    sortSel.fireChange();
    assert(names() === 'Bravo,Alpha' && viewParams.bsort === '1h', 'choosing 1h re-sorts and writes bsort=1h to the URL');

    const obsBox = el.querySelector('#busiestObservers');
    obsBox.checked = true;
    obsBox.fireChange();
    assert(names() === 'Bravo,Alpha,ObsRep' && viewParams.bobs === 'show', 'including observers lists them and writes bobs=show');
    assert(/ObsRep<\/a> <span class="text-muted">observer<\/span>/.test(table()), 'an observer repeater is tagged as one');

    sortSel.value = 'bogus';
    sortSel.fireChange();
    assert(viewParams.bsort === '24h', 'an unknown sort value falls back to 24h');
  }
  {
    // Deep link: the URL decides the state on render.
    viewParams.bsort = '1h'; viewParams.bobs = 'show'; viewParams.blimit = 'all';
    nodesData = Array.from({ length: 55 }, (_, i) => ({ public_key: 'p' + i, name: 'N' + i, role: 'repeater', relay_count_1h: i, relay_count_24h: 100 }));
    const el = makeElement('root');
    await M.renderRepeaterMetricsTab(el);
    assert(/option value="1h" selected/.test(el.innerHTML) && /id="busiestObservers" checked/.test(el.innerHTML),
      'bsort=1h and bobs=show from the URL select the controls');
    const html = el.querySelector('#busiestRepeatersTable').innerHTML;
    assert((html.match(/<tr><td/g) || []).length === 55, 'blimit=all from the URL shows every row');
    el.querySelector('#busiestRepeatersTable').querySelector('#busiestLimitToggle').fireClick();
    assert(viewParams.blimit === '50' && (el.querySelector('#busiestRepeatersTable').innerHTML.match(/<tr><td/g) || []).length === 50,
      'the limit button goes back to the top 50 and writes it to the URL');
    Object.keys(viewParams).forEach(k => { delete viewParams[k]; });
  }
  {
    const failingApi = observersData;
    observersData = null;
    nodesData = [{ public_key: 'aa01', name: 'Alpha', role: 'repeater', relay_count_1h: 1, relay_count_24h: 2 }];
    const el = makeElement('root');
    await M.renderRepeaterMetricsTab(el);
    assert(/Alpha/.test(el.querySelector('#busiestRepeatersTable').innerHTML) && /Include observer repeaters \(0\)/.test(el.innerHTML),
      'without an observer list the table still renders and hides nobody');
    observersData = failingApi;
  }

  console.log('\n=== [B5] seen airtime columns and the busiest-hour sort (#697 M2) ===');
  {
    const nodes = [
      { public_key: 'a1', name: 'Quiet', role: 'repeater', relay_count_1h: 50, relay_count_24h: 900, est_airtime_pct_24h: 0.4, est_airtime_peak_pct: 1.2 },
      { public_key: 'b2', name: 'Busy', role: 'repeater', relay_count_1h: 5, relay_count_24h: 100, est_airtime_pct_24h: 3.1, est_airtime_peak_pct: 10.04 },
      { public_key: 'c3', name: 'OldServer', role: 'repeater', relay_count_1h: 99, relay_count_24h: 999 },
      { public_key: 'd4', name: 'Tiny', role: 'repeater', relay_count_1h: 0, relay_count_24h: 1, est_airtime_pct_24h: 0.0012, est_airtime_peak_pct: '0.03' },
    ];
    const peak = M._busiestRepeaterRows(nodes, new Set(), 'peak', false);
    assert(peak.map(r => r.name).join(',') === 'Busy,Quiet,Tiny,OldServer',
      'peak: ranked by the estimated busiest hour, rows without an estimate last; got ' + peak.map(r => r.name).join(','));
    assert(peak[2].estPeak === 0.03, 'a string estimate from the API is cast to a number');
    assert(peak[3].estAvg === null && peak[3].estPeak === null, 'a missing estimate stays null, not 0');
    const html = M.renderBusiestRepeatersTable(peak, 'peak', '50');
    const row = name => (html.split('<tr>').find(tr => tr.includes('>' + name + '<')) || '');
    assert(/airtime-pct airtime-over[^>]*>≥10\.0%</.test(row('Busy')), 'a seen busiest hour at the 10% limit reads ≥10.0% and is flagged like observer TX airtime');
    assert(/airtime-pct"[^>]*>≥1\.2%</.test(row('Quiet')) && !/airtime-over/.test(row('Quiet')), 'below the limit it reads ≥1.2%, not flagged');
    assert(/>&lt;0\.1%</.test(row('Tiny')) && !/≥&lt;/.test(row('Tiny')), 'a very small value reads <0.1%, escaped, without ≥');
    assert(/>0\.0%</.test(M.renderBusiestRepeatersTable([{ pk: 'z', name: 'Zero', role: 'repeater', relay1h: 0, relay24h: 1, estAvg: 0, estPeak: 0, observer: false }], 'peak', '50')),
      'zero seen airtime reads 0.0%, not ≥0.0%');
    assert((row('OldServer').match(/<span class="text-muted">—<\/span>/g) || []).length === 2, 'no estimate shows a dash in both airtime columns');
    assert(/aria-sort="descending">Busiest hour</.test(html), 'sorting by peak marks the Busiest hour column');
    assert(/colspan="2"[^>]*>Relayed<\/th>/.test(html) && /colspan="2"[^>]*>Seen airtime<\/th>/.test(html),
      'the counts and the estimates are grouped under two header cells');
    assert((html.match(/<tr><td/g) || []).length === peak.length && (html.split('<tr><td')[1].match(/<td/g) || []).length === 5,
      'every row has six cells, matching the six columns');
  }
  {
    nodesData = [{ public_key: 'a1', name: 'A', role: 'repeater', relay_count_1h: 1, relay_count_24h: 2, est_airtime_pct_24h: 1, est_airtime_peak_pct: 2 }];
    observersData = { observers: [] };
    Object.keys(viewParams).forEach(k => { delete viewParams[k]; });
    const el = makeElement('root');
    await M.renderRepeaterMetricsTab(el);
    assert(/option value="peak">Busiest hour \(seen airtime\)</.test(el.innerHTML), 'the sort select offers the busiest hour');
    const sel = el.querySelector('#busiestSort');
    sel.value = 'peak';
    sel.fireChange();
    assert(viewParams.bsort === 'peak', 'choosing it writes bsort=peak');
    viewParams.bsort = 'peak';
    const el2 = makeElement('root');
    await M.renderRepeaterMetricsTab(el2);
    assert(/option value="peak" selected/.test(el2.innerHTML), 'bsort=peak from the URL selects it');
    Object.keys(viewParams).forEach(k => { delete viewParams[k]; });
  }

  console.log('\n=== [B4] wiring (structural) ===');
  assert(/'repeater-metrics': \[BUSIEST_SORT\.param, BUSIEST_OBSERVERS\.param, BUSIEST_LIMIT\.param\]/.test(src),
    'TAB_URL_PARAMS drops bsort/bobs/blimit when another tab is selected');
  assert(/api\('\/observers', \{ ttl: CLIENT_TTL\.observers \}\)\.catch\(/.test(block),
    'a failing observer fetch cannot break the Repeater Metrics tab');

  console.log('\n────────────────────────────────────────');
  console.log(`  ${passed} passed, ${failed} failed`);
  if (failed) process.exit(1);
})();
