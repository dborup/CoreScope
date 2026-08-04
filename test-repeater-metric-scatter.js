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
  block + '\nreturn { REPEATER_METRIC_AXES, REPEATER_METRIC_AXIS_GROUPS, SCOPE_FILTERS, _scopeFilterPredicate, REPEATER_METRIC_PRESETS, REPEATER_METRIC_POINT_CAP, _niceCeil, _axisFmt, _axisFromMax, _resolveAxis, _sampleExact, _sampleForPlot, _toScatterPoints, renderMetricScatter, renderRepeaterMetricsTab };')(
  esc, windowStub, RegionFilter, AreaFilter, fetchAllNodes, getFavorites, CLIENT_TTL, fakeLocalStorage);

console.log('\n=== scope-ext [1]/[2]/[11] axis registry: all 12 axes exist (8 original + 4 scope) and read the right point fields ===');
assert(M.REPEATER_METRIC_AXES.map(a => a.key).join(',') === 'traffic,usefulness,bridge,coverage,redundancy,relay1h,relay24h,adverts,unscopedRelays24h,unscopedRatio24h,scopesObserved,scopesRecent',
  'axis registry exposes the original 8 metrics UNCHANGED, in order, plus the 4 new scope axes appended after them');
{
  const sentinel = { traffic: 1, usefulness: 2, bridge: 3, coverage: 4, redundancy: 5, relay1h: 6, relay24h: 7, adverts: 8, unscopedRelays24h: 9, unscopedRatio24h: 10, scopesObserved: 11, scopesRecent: 12 };
  M.REPEATER_METRIC_AXES.forEach((a, i) => {
    assert(a.get(sentinel) === i + 1, `axis "${a.key}" getter reads its own point.${a.key} field, not another axis's`);
  });
}
assert(M.REPEATER_METRIC_AXES.every(a => typeof a.score === 'boolean'),
  'every axis declares score:true/false (percent vs. integer formatting)');
assert(M.REPEATER_METRIC_AXES.filter(a => a.score).map(a => a.key).join(',') === 'traffic,usefulness,bridge,coverage,redundancy,unscopedRatio24h',
  'the six percent-formatted axes (five original #672 scores + Unscoped ratio) are marked score:true');
assert(M.REPEATER_METRIC_AXES.filter(a => !a.score).map(a => a.key).join(',') === 'relay1h,relay24h,adverts,unscopedRelays24h,scopesObserved,scopesRecent',
  'the six integer-formatted count axes are marked score:false');

console.log('\n=== scope-ext [12] optgroup axis grouping ===');
assert(M.REPEATER_METRIC_AXIS_GROUPS.join(',') === 'Network importance,Activity,Scope health',
  'the three axis groups are defined in the required display order');
assert(M.REPEATER_METRIC_AXES.filter(a => a.group === 'Network importance').map(a => a.key).join(',') === 'traffic,usefulness,bridge,coverage,redundancy',
  '"Network importance" group contains exactly Traffic share, Usefulness, Bridge, Coverage, Redundancy');
assert(M.REPEATER_METRIC_AXES.filter(a => a.group === 'Activity').map(a => a.key).join(',') === 'relay1h,relay24h,adverts',
  '"Activity" group contains exactly Relays (1h/24h) and Adverts');
assert(M.REPEATER_METRIC_AXES.filter(a => a.group === 'Scope health').map(a => a.key).join(',') === 'unscopedRelays24h,unscopedRatio24h,scopesObserved,scopesRecent',
  '"Scope health" group contains exactly the 4 new scope axes');
assert(M.REPEATER_METRIC_AXES.every(a => M.REPEATER_METRIC_AXIS_GROUPS.includes(a.group)),
  'every axis belongs to one of the three declared groups (none orphaned)');

console.log('\n=== scope-ext [17] every preset resolves to two real, registered axis keys ===');
assert(M.REPEATER_METRIC_PRESETS.every(p => M.REPEATER_METRIC_AXES.some(a => a.key === p.x) && M.REPEATER_METRIC_AXES.some(a => a.key === p.y)),
  'every preset\'s x/y keys resolve to a real entry in REPEATER_METRIC_AXES');
{
  const byKey = k => M.REPEATER_METRIC_PRESETS.find(p => p.key === k);
  assert(byKey('scope-risk').x === 'usefulness' && byKey('scope-risk').y === 'unscopedRatio24h', '"Scope risk vs importance" = usefulness × unscopedRatio24h');
  assert(byKey('bridge-risk').x === 'bridge' && byKey('bridge-risk').y === 'unscopedRelays24h', '"Bridge risk" = bridge × unscopedRelays24h');
  assert(byKey('scope-diversity').x === 'traffic' && byKey('scope-diversity').y === 'scopesRecent', '"Scope diversity vs traffic" = traffic × scopesRecent');
  assert(byKey('activity-vs-unscoped').x === 'relay24h' && byKey('activity-vs-unscoped').y === 'unscopedRatio24h', '"Activity vs unscoped ratio" = relay24h × unscopedRatio24h');
}

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
         mapped[2].relay24h === null && mapped[2].adverts === null &&
         mapped[2].unscopedRelays24h === null && mapped[2].unscopedRatio24h === null &&
         mapped[2].scopesObserved === null && mapped[2].scopesRecent === null,
    'a node with no scores at all maps every metric (including all 4 scope axes) to null, not 0/undefined');
  assert(mapped[3].name === 'NAMELESS0000',
    'nameless node falls back to a 12-char pubkey prefix');
  assert(mapped[4].name === '?' && mapped[4].pk === undefined,
    'node with neither name nor pubkey falls back to "?"');
}

console.log('\n=== scope-ext [3]/[4]/[5]/[6]/[7] unscoped ratio formula ===');
{
  const favSet = new Set();
  const [withBoth, zeroDenom, missingDenom, missingNumerator, over1, nonArrayScopes] = M._toScatterPoints([
    { public_key: 'A', role: 'repeater', relay_count_24h: 20, unscoped_relay_count_24h: 8 },
    { public_key: 'B', role: 'repeater', relay_count_24h: 0, unscoped_relay_count_24h: 5 },
    { public_key: 'C', role: 'repeater', unscoped_relay_count_24h: 5 }, // relay_count_24h absent
    { public_key: 'D', role: 'repeater', relay_count_24h: 20 }, // unscoped_relay_count_24h absent
    { public_key: 'E', role: 'repeater', relay_count_24h: 10, unscoped_relay_count_24h: 15 }, // inconsistent data: unscoped > total
    { public_key: 'F', role: 'repeater', relay_count_24h: 20, unscoped_relay_count_24h: 8, transported_scopes: 'not-an-array', transported_scopes_recent: 42 },
  ], favSet);

  console.log('\n=== scope-ext [3] unscoped ratio = unscoped / relay24h ===');
  assert(Math.abs(withBoth.unscopedRatio24h - 0.4) < 1e-9,
    '8 unscoped of 20 relays24h computes to a 0.4 ratio');

  console.log('\n=== scope-ext [4] a zero denominator gives null, not 0 or Infinity ===');
  assert(zeroDenom.unscopedRatio24h === null,
    'relay_count_24h === 0 makes the ratio null (never divides by zero, never coerces to 0)');

  console.log('\n=== scope-ext [5] a missing denominator gives null ===');
  assert(missingDenom.unscopedRatio24h === null && missingDenom.relay24h === null,
    'relay_count_24h absent (not just falsy) makes the ratio null');

  console.log('\n=== scope-ext [6] a missing unscoped field is handled per the actual API contract ===');
  // Per cmd/server/routes.go, unscoped_relay_count_24h is set unconditionally
  // alongside relay_count_24h in the same enrichment step -- it is never
  // "missing while relay24h is known" on a current server. This test proves
  // the DEFENSIVE behavior for the case anyway (e.g. an old server that
  // predates the feature): the missing field must become null, and must
  // NEVER be silently treated as 0 (which would falsely claim "0 unscoped").
  assert(missingNumerator.unscopedRelays24h === null && missingNumerator.unscopedRatio24h === null,
    'a missing unscoped_relay_count_24h maps to null (not 0), so the ratio is also null rather than a false "0%"');

  console.log('\n=== scope-ext [7] a ratio above 1 is surfaced, never silently clamped ===');
  assert(over1.unscopedRatio24h === 1.5,
    'unscoped (15) exceeding relay24h (10) -- a real data inconsistency -- produces an UNCLAMPED 1.5 ratio, not a hidden/clamped 1.0');

  console.log('\n=== scope-ext [10] non-array scope fields do not crash and map to null ===');
  assert(nonArrayScopes.scopesObserved === null && nonArrayScopes.scopesRecent === null,
    'a string/number in transported_scopes/transported_scopes_recent (not a real array) is treated as absent, not a crash');
}

console.log('\n=== scope-ext [8]/[9] transported_scopes / transported_scopes_recent are counted correctly ===');
{
  const favSet = new Set();
  const [withScopes, withRecentOnly, withNeither] = M._toScatterPoints([
    { public_key: 'A', role: 'repeater', transported_scopes: ['dk-mj', 'dk-oj', 'se12'], transported_scopes_recent: ['dk-mj'] },
    { public_key: 'B', role: 'room', transported_scopes_recent: ['dk-oj', 'se12'] },
    { public_key: 'C', role: 'repeater' },
  ], favSet);
  assert(withScopes.scopesObserved === 3, 'transported_scopes.length (3 scopes) maps to scopesObserved');
  assert(withScopes.scopesRecent === 1, 'transported_scopes_recent.length (1 scope) maps to scopesRecent');
  assert(withRecentOnly.scopesObserved === null && withRecentOnly.scopesRecent === 2,
    'transported_scopes omitted (server omits when empty) -> null; transported_scopes_recent present -> counted independently');
  assert(withNeither.scopesObserved === null && withNeither.scopesRecent === null,
    'a repeater with neither scope field present maps both to null, not 0');
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
    { pk: 'AA', name: 'Rptr Eins', role: 'repeater', fav: true, traffic: 0.42, usefulness: 0.55, bridge: 0.1, coverage: 0.2, redundancy: 0.3, relay1h: 12, relay24h: 200, adverts: 50,
      defaultScope: 'dk-mj', defaultScopeConfirmed: true, unscopedRelays24h: 8, unscopedRatio24h: 0.04, scopesRecent: 2 },
    { pk: 'BB', name: 'Raum <Zwei>', role: 'room', fav: false, traffic: 0.05, usefulness: 0.2, bridge: 0.8, coverage: 0.1, redundancy: 0.1, relay1h: 0, relay24h: 3, adverts: 5,
      defaultScope: null, defaultScopeConfirmed: false, unscopedRelays24h: null, unscopedRatio24h: null, scopesRecent: null },
    { pk: 'CC', name: 'Rptr Drei', role: 'repeater', fav: false, traffic: null, usefulness: 0.4, bridge: 0.3, coverage: 0.3, redundancy: 0.3, relay1h: 4, relay24h: 40, adverts: 9 },
    { pk: 'DD', name: 'Rptr Vier', role: 'repeater', fav: false, traffic: 0.15, usefulness: 0.25, bridge: 0.15, coverage: 0.15, redundancy: 0.15, relay1h: 1, relay24h: 10, adverts: 2,
      defaultScope: 'se12', defaultScopeConfirmed: false, unscopedRelays24h: 1, unscopedRatio24h: 0.1, scopesRecent: 0 },
  ];
  const xa = M._resolveAxis('traffic', pts), ya = M._resolveAxis('bridge', pts);
  const svg = M.renderMetricScatter(pts, xa, ya);
  assert(svg.startsWith('<svg') && svg.endsWith('</svg>') && !/NaN/.test(svg),
    'produces a valid <svg> with no NaN coordinates');
  assert((svg.match(/href="#\/nodes\//g) || []).length === 3,
    'plots only points with BOTH axis values present (CC has null traffic → skipped; AA/BB/DD plotted)');
  assert(/href="#\/nodes\/AA\/analytics"/.test(svg),
    'each point links to its per-node analytics page');
  assert(/fill="#3b82f6"/.test(svg) && /fill="#a855f7"/.test(svg),
    'point fill follows the ROLE_COLORS node-role colour');
  assert(/Raum &lt;Zwei&gt;/.test(svg),
    'point names are HTML-escaped in the tooltip');
  assert(/stroke="var\(--text\)"/.test(svg),
    'favorite ring uses the neutral var(--text), not a status colour');
  assert(/tabindex="-1"/.test(svg), 'points carry tabindex="-1" (out of sequential keyboard tab order)');

  console.log('\n=== scope-ext [19]/[20]/[21] tooltip: default-scope status, numerator/denominator/percent, escaping ===');
  assert(/Rptr Eins · [^<]*Scope: dk-mj \(confirmed\)/.test(svg),
    'a confirmed default scope shows "<scope> (confirmed)" in the tooltip');
  assert(/Rptr Vier · [^<]*Scope: se12 \(inferred\)/.test(svg),
    'an inferred (unconfirmed) default scope shows "<scope> (inferred)" in the tooltip');
  assert(/Raum &lt;Zwei&gt; · [^<]*Scope: No default scope/.test(svg),
    'a node with no default scope shows "No default scope" in the tooltip, and the name is still escaped in this same string');
  assert(/Unscoped: 8 of 200 relays \(4%\)/.test(svg),
    'tooltip shows numerator, denominator, AND the percentage together: "8 of 200 relays (4%)"');
  assert(/Unscoped: —/.test(svg),
    'a node with no unscoped/relay24h data shows an em dash, not "undefined" or "0 of 0"');
  assert(/Recent scopes: 2/.test(svg) && /Recent scopes: —/.test(svg),
    'recent-scope count is shown numerically when known (2) and as an em dash when null');

  console.log('\n=== [15] empty axis combination shows a message, not a blank plot ===');
  {
    const empties = [{ pk: 'E', name: 'e', role: 'repeater', fav: false, traffic: null, bridge: null }];
    const emptySvg = M.renderMetricScatter(empties, M._resolveAxis('traffic', empties), M._resolveAxis('bridge', empties));
    assert(/No repeaters have values/.test(emptySvg),
      'an axis combination with no plottable points renders an explicit message');
    assert(!/showing \d+ of \d+ points/.test(svg),
      'no sampling disclosure is shown for a small point set');
  }
}

console.log('\n=== review-fix [overlap 1]/[2]/[3]/[4]/[5] co-located points: capped names, exact remainder, self-exclusion, escaping ===');
{
  // 8 points sharing the exact same (traffic, bridge) coordinate, so all 8
  // round to the same pixel position and form one overlap group.
  const overlapPts = [];
  for (let i = 0; i < 8; i++) {
    overlapPts.push({ pk: 'OV' + i, name: 'Overlap <' + i + '>', role: 'repeater', fav: false, traffic: 0.5, bridge: 0.5 });
  }
  const oxa = M._resolveAxis('traffic', overlapPts), oya = M._resolveAxis('bridge', overlapPts);
  const osvg = M.renderMetricScatter(overlapPts, oxa, oya);

  assert(/7 other node\(s\) here:/.test(osvg),
    'review-fix [1]: each of the 8 co-located points reports the exact total (7 others) sharing its coordinate');

  // The first <title> in the SVG is the chart-level accessibility title
  // (e.g. "Repeater metric scatter: Traffic share (x) vs Bridge (y)"), not
  // a point tooltip -- drop it before indexing against overlapPts.
  const titles = [...osvg.matchAll(/<title>([^<]*)<\/title>/g)].map(m => m[1]).slice(1);
  assert(titles.length === 8, 'sanity: found exactly 8 per-point tooltip <title> blocks (chart-level title excluded)');
  titles.forEach((title, i) => {
    const otherPart = (title.split('other node(s) here: ')[1] || '').replace(/, and \d+ more$/, '');
    const shownNames = otherPart ? otherPart.split(', ') : [];
    assert(shownNames.length <= 5,
      `review-fix [2]: tooltip ${i} lists at most 5 other names (got ${shownNames.length})`);
    const ownEscapedName = overlapPts[i].name.replace(/</g, '&lt;').replace(/>/g, '&gt;');
    assert(!shownNames.includes(ownEscapedName),
      `review-fix [4]: point ${i}'s own name never appears in its own "others" list`);
  });
  assert(/, and 2 more/.test(osvg),
    'review-fix [3]: with 8 points (7 others, 5 shown per tooltip), the remainder is EXACTLY 2 ("and 2 more")');
  assert(osvg.includes('Overlap &lt;0&gt;') && !osvg.includes('Overlap <0>'),
    'review-fix [5]: names containing HTML characters are still escaped in the overlap note (no raw "<"/">" leaks through)');
}

console.log('\n=== review-fix [overlap 6]/[7]/[8]/[9] 2000 identical coordinates: linear size, no O(n^2) blowup, bounded time ===');
{
  const many = [];
  for (let i = 0; i < 2000; i++) many.push({ pk: 'z' + i, name: 'Node' + i, role: 'repeater', fav: false, traffic: 0.5, bridge: 0.5 });
  const mxa = M._resolveAxis('traffic', many), mya = M._resolveAxis('bridge', many);
  const t0 = Date.now();
  const bigOverlapSvg = M.renderMetricScatter(many, mxa, mya);
  const elapsedMs = Date.now() - t0;

  // review-fix [7]: a fixture upper bound loose enough not to be flaky, but
  // tight enough to catch the O(n^2) regression -- the previous
  // implementation's overlap note alone would emit ~2000 names (each
  // several bytes) PER tooltip, ~2000 times over, i.e. tens of MB just for
  // that one coordinate. This build's per-tooltip overlap note is capped
  // at 5 names, so total SVG growth across all 2000 points is linear.
  assert(bigOverlapSvg.length < 2_000_000,
    `review-fix [6]/[7]: SVG output for 2000 identical points stays well under a 2MB bound (got ${bigOverlapSvg.length} bytes)`);
  // review-fix [9]: a conservative, CI-stable bound, not a tight
  // microbenchmark -- this build should finish in low tens of ms; the old
  // O(n^2) code (~4,000,000 filter/string ops for this one coordinate)
  // would still blow well past even this generous 1s ceiling.
  assert(elapsedMs < 1000,
    `review-fix [8]/[9]: rendering 2000 identical points completes in well under a conservative 1000ms bound (took ${elapsedMs}ms)`);
  assert(/, and 1994 more/.test(bigOverlapSvg),
    'the remainder count is exact even at scale: 2000 points -> 1999 others -> 5 shown -> 1994 more');
}

console.log('\n=== scope-ext [13] each scope filter includes/excludes the correct fixtures ===');
{
  const fixtures = [
    { pk: 'no-default', defaultScope: null, defaultScopeConfirmed: false, scopesObserved: null, scopesRecent: null },
    { pk: 'confirmed', defaultScope: 'dk-mj', defaultScopeConfirmed: true, scopesObserved: 1, scopesRecent: 1 },
    { pk: 'inferred', defaultScope: 'dk-oj', defaultScopeConfirmed: false, scopesObserved: 2, scopesRecent: 0 },
    { pk: 'relays-others-observed', defaultScope: null, defaultScopeConfirmed: false, scopesObserved: 3, scopesRecent: 0 },
    { pk: 'relays-others-recent-only', defaultScope: null, defaultScopeConfirmed: false, scopesObserved: null, scopesRecent: 1 },
    { pk: 'no-default-no-relays', defaultScope: null, defaultScopeConfirmed: false, scopesObserved: null, scopesRecent: null },
  ];
  const byKey = key => fixtures.filter(M._scopeFilterPredicate(key)).map(p => p.pk);

  assert(byKey('all').length === fixtures.length, '"all" passes every fixture through unfiltered');
  assert(byKey('no-default').sort().join(',') === ['no-default', 'relays-others-observed', 'relays-others-recent-only', 'no-default-no-relays'].sort().join(','),
    '"no-default" includes every fixture with a falsy defaultScope, regardless of scope-transport activity');
  assert(byKey('confirmed-default').join(',') === 'confirmed',
    '"confirmed-default" includes only the fixture with BOTH a defaultScope AND defaultScopeConfirmed');
  assert(byKey('inferred-default').join(',') === 'inferred',
    '"inferred-default" includes only the fixture with a defaultScope but NOT confirmed');
  assert(byKey('relays-without-own').sort().join(',') === ['relays-others-observed', 'relays-others-recent-only'].sort().join(','),
    '"relays-without-own" includes fixtures with no default scope AND (scopesObserved>0 OR scopesRecent>0), excluding the truly idle no-default fixture');
  assert(!byKey('relays-without-own').includes('no-default-no-relays'),
    'a no-default fixture that has never transported any scope is excluded from "relays-without-own"');
  assert(byKey('some-stale-unknown-key').length === fixtures.length,
    'an unrecognized filter key falls back to the "all" predicate (matches _resolveAxis\'s own fallback style)');
}

console.log('\n=== scope-ext [22]/[23] _sampleExact selects EXACTLY min(budget, length), not ~budget/ceil(length/budget) ===');
{
  const arr2500 = Array.from({ length: 2500 }, (_, i) => i);
  const sampled = M._sampleExact(arr2500, 2000);
  assert(sampled.length === 2000,
    '_sampleExact(2500 items, budget 2000) selects EXACTLY 2000 -- the old stride (i % Math.ceil(2500/2000) === 0 -> step 2) selected only ~1250');
  assert(new Set(sampled).size === 2000, 'all 2000 selected items are distinct (no duplicate indices)');
  assert(sampled[0] === arr2500[0] && sampled[sampled.length - 1] >= arr2500.length - 3,
    'sampling spans the full input range (starts at the first source item, ends within a few items of the last)');
  assert(M._sampleExact(arr2500, 3000).length === 2500,
    '_sampleExact never fabricates points: budget > length returns the whole array (2500, not 3000)');
  assert(M._sampleExact(arr2500, 0).length === 0, '_sampleExact with a 0 budget returns nothing');
}

console.log('\n=== scope-ext [22]/[23]/[24]/[25]/[26] favorites survive sampling; ~2000 cap holds even under an all-favorite set ===');
{
  const many = [];
  for (let i = 0; i < 2500; i++) many.push({ pk: 'p' + i, name: 'n' + i, role: 'repeater', fav: (i % 500 === 0), traffic: i / 2500, bridge: (i % 7) / 7 });
  const favCount = many.filter(p => p.fav).length; // 5 favorites (i=0,500,1000,1500,2000)
  const bigSvg = M.renderMetricScatter(many, M._resolveAxis('traffic', many), M._resolveAxis('bridge', many));
  assert(/showing \d+ of 2500 points/.test(bigSvg),
    'sampling >2000 points is disclosed in-plot ("showing N of 2500 points")');
  const plotted = (bigSvg.match(/href="#\/nodes\//g) || []).length;
  assert(plotted === M.REPEATER_METRIC_POINT_CAP,
    `plotted point count is EXACTLY the ${M.REPEATER_METRIC_POINT_CAP}-point cap (5 favorites + 1995 sampled others), not an approximate ~1250`);
  const allFavsShown = many.filter(p => p.fav).every(p => bigSvg.includes('#/nodes/' + p.pk + '/analytics'));
  assert(allFavsShown, 'ALL favorites survive sampling — the legend favorite count is never a lie');

  const { eligible, shown } = M._sampleForPlot(many, M._resolveAxis('traffic', many), M._resolveAxis('bridge', many));
  assert(eligible.length === 2500 && shown.length === 2000,
    '_sampleForPlot reports eligible=2500 (all have both values), shown=2000 (the cap) -- the single source of truth renderMetricScatter and the legend both read from');

  const allFav = [];
  for (let i = 0; i < 2500; i++) allFav.push({ pk: 'q' + i, name: 'n' + i, role: 'repeater', fav: true, traffic: i / 2500, bridge: 0.5 });
  const allFavSvg = M.renderMetricScatter(allFav, M._resolveAxis('traffic', allFav), M._resolveAxis('bridge', allFav));
  const allFavPlotted = (allFavSvg.match(/href="#\/nodes\//g) || []).length;
  assert(allFavPlotted === M.REPEATER_METRIC_POINT_CAP,
    '2500 favorites are themselves sampled down to EXACTLY the cap — the cap cannot be bypassed by favoriting everything');
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

  console.log('\n=== scope-ext [29] pre-existing (old) localStorage axis choices still resolve after this extension ===');
  {
    fakeLocalStorage.setItem('meshcore-repeater-scatter-x', 'bridge');
    fakeLocalStorage.setItem('meshcore-repeater-scatter-y', 'redundancy');
    nodesData = [{ public_key: 'A', name: 'A', role: 'repeater', bridge_score: 0.3, redundancy_score: 0.6 }];
    const el = makeElement('root');
    await M.renderRepeaterMetricsTab(el);
    assert(/id="metricScatterX"[\s\S]{0,900}option value="bridge" selected/.test(el.innerHTML),
      'an old stored X axis key ("bridge", one of the original 8) still resolves and renders selected');
    assert(/id="metricScatterY"[\s\S]{0,900}option value="redundancy" selected/.test(el.innerHTML),
      'an old stored Y axis key ("redundancy") still resolves and renders selected');
    fakeLocalStorage.removeItem('meshcore-repeater-scatter-x');
    fakeLocalStorage.removeItem('meshcore-repeater-scatter-y');
  }

  console.log('\n=== scope-ext [14]/[15] scope filter is applied BEFORE axis-max/sampling, and the choice persists ===');
  {
    fakeLocalStorage.setItem('meshcore-repeater-scatter-x', 'traffic');
    fakeLocalStorage.setItem('meshcore-repeater-scatter-y', 'bridge');
    nodesData = [
      { public_key: 'NODEFAULT', name: 'NoDefault', role: 'repeater', default_scope: null, traffic_share_score: 0.1, bridge_score: 0.1 },
      { public_key: 'HASDEFAULT', name: 'HasDefault', role: 'repeater', default_scope: 'dk-mj', traffic_share_score: 0.9, bridge_score: 0.9 },
    ];
    favorites = [];
    const el = makeElement('root');
    await M.renderRepeaterMetricsTab(el);
    const scopeSel = el.querySelector('#metricScatterScopeFilter');
    scopeSel.value = 'no-default';
    scopeSel.fireChange();
    const plot = el.querySelector('#metricScatterPlot').innerHTML;
    assert(!/>100%</.test(plot),
      'axis domain reflects ONLY the scope-filtered node (max 0.1 -> a 10% ceiling) -- if the excluded HasDefault node\'s 0.9 had leaked into the max/sampling pass, a 100% gridline would appear');
    assert((plot.match(/href="#\/nodes\//g) || []).length === 1,
      'only the no-default node is actually plotted once the scope filter is applied');
    assert(fakeLocalStorage.getItem('meshcore-repeater-scatter-scope-filter') === 'no-default',
      'selecting a scope filter persists its key to localStorage');
  }

  console.log('\n=== scope-ext [16] an unknown/stale stored scope-filter value falls back to "All scope states" ===');
  {
    fakeLocalStorage.setItem('meshcore-repeater-scatter-scope-filter', 'no-such-filter-anymore');
    nodesData = [
      { public_key: 'A', name: 'A', role: 'repeater', default_scope: null, traffic_share_score: 0.5, usefulness_score: 0.5 },
      { public_key: 'B', name: 'B', role: 'repeater', default_scope: 'dk-mj', traffic_share_score: 0.5, usefulness_score: 0.5 },
    ];
    const el = makeElement('root');
    let threw = false;
    try { await M.renderRepeaterMetricsTab(el); } catch (e) { threw = true; }
    assert(!threw, 'a stale scope-filter localStorage value does not crash the render');
    assert(/id="metricScatterScopeFilter"[\s\S]{0,300}option value="all" selected/.test(el.innerHTML),
      'the rendered scope-filter select falls back to "all" when the stored value is unrecognized');
    fakeLocalStorage.removeItem('meshcore-repeater-scatter-scope-filter');
  }

  console.log('\n=== scope-ext [17]/[18] presets select the correct axes, persist through the normal storage keys, and redraw ===');
  {
    fakeLocalStorage.removeItem('meshcore-repeater-scatter-x');
    fakeLocalStorage.removeItem('meshcore-repeater-scatter-y');
    nodesData = [
      { public_key: 'A', name: 'A', role: 'repeater', usefulness_score: 0.9, relay_count_24h: 10, unscoped_relay_count_24h: 5 },
      { public_key: 'B', name: 'B', role: 'repeater', usefulness_score: 0.1, relay_count_24h: 20, unscoped_relay_count_24h: 2 },
    ];
    const el = makeElement('root');
    await M.renderRepeaterMetricsTab(el);
    const plotBefore = el.querySelector('#metricScatterPlot').innerHTML;
    const presetSel = el.querySelector('#metricScatterPreset');
    presetSel.value = 'scope-risk';
    presetSel.fireChange();
    assert(el.querySelector('#metricScatterX').value === 'usefulness' && el.querySelector('#metricScatterY').value === 'unscopedRatio24h',
      'the "Scope risk vs importance" preset sets X=usefulness, Y=unscopedRatio24h');
    assert(fakeLocalStorage.getItem('meshcore-repeater-scatter-x') === 'usefulness' && fakeLocalStorage.getItem('meshcore-repeater-scatter-y') === 'unscopedRatio24h',
      'choosing a preset persists the resulting axis pair through the SAME localStorage keys a manual axis change uses -- no separate storage path');
    const plotAfter = el.querySelector('#metricScatterPlot').innerHTML;
    assert(plotAfter !== plotBefore, 'choosing a preset redraws the plot');

    console.log('\n=== review-fix [preset 13]/[14]/[15]/[16] preset select resets, is reselectable, never persists itself ===');
    const presetSelAfter = el.querySelector('#metricScatterPreset');
    assert(presetSelAfter.value === '',
      'review-fix [13]: the preset <select> resets to the "" placeholder immediately after applying a preset');
    assert(fakeLocalStorage.getItem('meshcore-repeater-preset') === null &&
           fakeLocalStorage.getItem('meshcore-repeater-scatter-preset') === null,
      'review-fix [16]: the preset CHOICE itself is never written to localStorage under any key -- only the resulting X/Y axis keys are');

    // review-fix [15]: a manual axis change right after a preset must not
    // leave a stale "active preset" impression -- already satisfied by [13]
    // resetting to "" the moment the preset was applied, before any manual
    // change even happens. Confirm the select still reads "" afterwards.
    const xSelAfterPreset = el.querySelector('#metricScatterX');
    xSelAfterPreset.value = 'bridge';
    xSelAfterPreset.fireChange();
    assert(el.querySelector('#metricScatterPreset').value === '',
      'review-fix [15]: a manual axis change after a preset leaves the preset select at "" (never re-claims the old preset as active)');

    // review-fix [14]: the SAME preset can be selected again immediately --
    // this only works because the select's value returned to "" after the
    // first application; a <select> that stayed on "scope-risk" would never
    // fire a second 'change' event for the same value.
    const presetSel2 = el.querySelector('#metricScatterPreset');
    presetSel2.value = 'scope-risk';
    presetSel2.fireChange();
    assert(el.querySelector('#metricScatterX').value === 'usefulness' && el.querySelector('#metricScatterY').value === 'unscopedRatio24h',
      'review-fix [14]: choosing the SAME preset again (after a manual change moved the axes away) re-applies it correctly');
    assert(presetSel2.value === '', 'the preset select resets to "" again after this second application too');
  }

  console.log('\n=== scope-ext [26] legend favorite count reflects the scope-filtered view, not the unfiltered dataset ===');
  {
    fakeLocalStorage.removeItem('meshcore-repeater-scatter-x');
    fakeLocalStorage.removeItem('meshcore-repeater-scatter-y');
    fakeLocalStorage.removeItem('meshcore-repeater-scatter-scope-filter');
    nodesData = [
      { public_key: 'FAV_INCLUDED', name: 'FavIncluded', role: 'repeater', default_scope: null, traffic_share_score: 0.5, usefulness_score: 0.5 },
      { public_key: 'FAV_EXCLUDED', name: 'FavExcluded', role: 'repeater', default_scope: 'dk-mj', traffic_share_score: 0.5, usefulness_score: 0.5 },
      { public_key: 'PLAIN', name: 'Plain', role: 'repeater', default_scope: null, traffic_share_score: 0.5, usefulness_score: 0.5 },
    ];
    favorites = ['FAV_INCLUDED', 'FAV_EXCLUDED'];
    const el = makeElement('root');
    await M.renderRepeaterMetricsTab(el);
    const scopeSel = el.querySelector('#metricScatterScopeFilter');
    scopeSel.value = 'no-default';
    scopeSel.fireChange();
    const legend = el.querySelector('#metricScatterLegend').innerHTML;
    assert(/Favorites shown: 1\b/.test(legend) && !/Favorites shown: 2\b/.test(legend),
      'the legend counts only the favorite that survived the scope filter (FAV_EXCLUDED has a default scope and is filtered out) -- not both originally-fetched favorites. The "N of M eligible" branch (favorites dropped by SAMPLING rather than filtering) is covered at the _sampleForPlot unit level above, where eligible=2500/shown=2000 is exact and independent of any DOM.');
    favorites = [];
  }

  console.log('\n=== review-fix [counter 17]/[18]/[19]/[20]/[21] repeater counter tracks the scope filter ===');
  {
    fakeLocalStorage.removeItem('meshcore-repeater-scatter-x');
    fakeLocalStorage.removeItem('meshcore-repeater-scatter-y');
    fakeLocalStorage.removeItem('meshcore-repeater-scatter-scope-filter');
    nodesData = [
      { public_key: 'A', name: 'A', role: 'repeater', default_scope: null, traffic_share_score: 0.5, usefulness_score: 0.5 },
      { public_key: 'B', name: 'B', role: 'repeater', default_scope: 'dk-mj', traffic_share_score: 0.5, usefulness_score: 0.5 },
      { public_key: 'C', name: 'C', role: 'repeater', default_scope: null, traffic_share_score: 0.5 }, // no usefulness_score -> not eligible for the default X/Y pair
      { public_key: 'D', name: 'D', role: 'room', default_scope: null, traffic_share_score: 0.5, usefulness_score: 0.5 },
    ];
    favorites = [];
    _regionQS = ''; _areaQS = '';
    const el = makeElement('root');
    await M.renderRepeaterMetricsTab(el);
    // This minimal fake <select> stub does not derive .value from the
    // rendered `<option selected>` markup (unlike a real browser), so the
    // default axis pair must be set explicitly here for draw() to resolve
    // X=traffic/Y=usefulness precisely -- otherwise both would silently
    // fall back to REPEATER_METRIC_AXES[0] (traffic) via the empty-string
    // .find() miss, making X and Y the same axis and hiding the very
    // eligible/scopeFiltered distinction this test is checking for [21].
    el.querySelector('#metricScatterX').value = 'traffic';
    el.querySelector('#metricScatterY').value = 'usefulness';
    const countText = () => el.querySelector('#metricScatterCount').textContent;

    assert(countText() === '4 repeaters',
      'review-fix [17]: with no scope filter active, the counter shows the plain total ("4 repeaters"), matching points.length');

    const scopeSel = el.querySelector('#metricScatterScopeFilter');
    scopeSel.value = 'no-default';
    scopeSel.fireChange();
    // A, C, D have no default scope; B does -> scope-filtered count is 3 of 4.
    assert(countText().startsWith('3 of 4 repeaters'),
      'review-fix [18]/[19]/[20]: selecting a scope filter immediately updates the counter to "X of Y repeaters" using the scope-filtered population, not the unfiltered total');
    // C is scope-filtered IN (no default scope) but lacks usefulness_score,
    // so it is not eligible for the default traffic x usefulness pair --
    // only A and D are -> "2 plottable".
    assert(countText() === '3 of 4 repeaters · 2 plottable',
      'review-fix [21]: the "plottable" suffix (2) matches _sampleForPlot().eligible.length exactly -- not scopeFiltered.length (3) and not total (4)');
  }

  console.log('\n=== review-fix [counter 22] sampling never changes the scope-filtered/eligible counter totals ===');
  {
    fakeLocalStorage.setItem('meshcore-repeater-scatter-scope-filter', 'all');
    const many = [];
    for (let i = 0; i < 2500; i++) many.push({ public_key: 'm' + i, name: 'm' + i, role: 'repeater', default_scope: null, traffic_share_score: 0.5, usefulness_score: 0.5 });
    nodesData = many;
    const el = makeElement('root');
    await M.renderRepeaterMetricsTab(el);
    assert(el.querySelector('#metricScatterCount').textContent === '2500 repeaters',
      'review-fix [22]: with all 2500 points scope-filtered-in AND eligible, the counter reports the full 2500 -- the 2000-point drawing cap does not shrink the "X of Y"/"plottable" figures, only what actually gets a circle drawn');
    fakeLocalStorage.removeItem('meshcore-repeater-scatter-scope-filter');
  }

  console.log('\n=== review-fix [counter 23] "Y" reflects the already region/area-filtered fetch ===');
  {
    _regionQS = '&region=dk-mj';
    nodesData = [{ public_key: 'ONLY', name: 'Only', role: 'repeater', default_scope: null, traffic_share_score: 0.5, usefulness_score: 0.5 }];
    const el = makeElement('root');
    await M.renderRepeaterMetricsTab(el);
    assert(el.querySelector('#metricScatterCount').textContent === '1 repeater',
      'review-fix [23]: Y is points.length, built from whatever fetchAllNodes returned for the active region/area filter -- here the mock simulates the server already having narrowed the dataset to 1 node, and the counter reports exactly that, not a client-recomputed total. Combined with the existing [16] test (region/area query params reach fetchAllNodes), this confirms Y is the already-filtered fetch, not a separate client-side re-filter.');
    _regionQS = '';
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
