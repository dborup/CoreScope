'use strict';

const assert = require('node:assert/strict');
const regions = require('./public/regions-map.js');

const scope = (name, lastRelayed = '2026-10-16T11:00:00Z') => ({ name, lastRelayed });
const node = (publicKey, extra = {}) => ({ publicKey, name: publicKey, role: 'repeater',
  lat: 55, lon: 10, positionSource: 'reported', scopes: [scope('#dk')], ...extra });
const sample = [
  node('a', { scopes: [scope('#dk'), scope('#se', '2026-10-16T10:00:00Z')] }),
  node('b', { role: 'room', lon: 11 }),
  node('c', { lat: 56 }),
  node('companion', { role: 'companion' }),
  node('origin', { lat: 0, lon: 0 }),
  node('missing', { lat: null, lon: null, positionSource: 'none', scopes: [scope('#no')] }),
  node('half-missing', { lat: null, lon: 10 }),
  node('estimated', { positionSource: 'estimated' }),
  node('empty-position', { lat: '' }),
  node('invalid-coordinate', { lat: 91 }),
  node('no-relay', { scopes: [] }),
  node('a', { lat: 1 }),
];

assert.deepEqual(regions.collectScopePoints(sample).map(p => p.publicKey), ['a', 'b', 'c']);
assert.deepEqual(regions.listScopes(sample), ['#dk', '#no', '#se'], 'unpositioned relay scopes remain discoverable');
assert.equal(regions.collectScopeNodes(sample).length, 9, 'unpositioned active relays are retained and keys deduplicated');
assert.equal(regions.selectionSummary(regions.collectScopeNodes(sample), '#dk').missingPositionCount, 5);
assert.equal(regions.selectionSummary(regions.collectScopeNodes(sample), '#no').missingPositionCount, 1);
assert.equal(regions.selectionSummary(regions.collectScopeNodes(sample), '#no').points.length, 0);
assert.equal(regions.lastRelayedForScope(regions.collectScopeNodes(sample)[0], '#se'), '2026-10-16T10:00:00Z');
assert.equal(regions.lastRelayedForScope(regions.collectScopeNodes(sample)[0], ''), '2026-10-16T11:00:00Z');
assert.equal(regions.lastRelayedForScope(regions.collectScopeNodes(sample)[0], '#absent'), '');
assert.equal(regions.validPosition({ lat: null, lon: 10 }), false, 'null must not become latitude zero');
assert.equal(regions.validPosition({ lat: 0, lon: 10 }), true, 'equator is valid with a real longitude');
assert.equal(regions.validPosition({ lat: '55', lon: '10' }), true);
assert.equal(regions.validScope('#DK$'), true);
assert.equal(regions.validScope('#ø'), true);
assert.equal(regions.validScope('#bad:scope'), false);
assert.equal(regions.validScope('#bad<scope'), false);
const manyScopes = Array.from({ length: 129 }, (_, i) => node('p' + i, { scopes: [scope('#r' + i)] }));
assert.equal(regions.scopeCatalog(manyScopes).truncated, true);
assert.equal(regions.scopeCatalog(manyScopes).scopes.length, 128);
const routeOnlyCatalog = regions.mergeScopeCatalog({ scopes: [], truncated: false },
  Array.from({ length: 129 }, (_, i) => '#route' + String(i).padStart(3, '0')));
assert.equal(routeOnlyCatalog.truncated, true, 'route-only scopes participate in the selector cap');
assert.equal(routeOnlyCatalog.scopes.length, 128);
assert.equal(routeOnlyCatalog.scopes.includes('#route128'), false);
assert.deepEqual(regions.mergeScopeCatalog({ scopes: ['#history'], truncated: false }, ['#route', '#route', '<bad>']),
  { scopes: ['#history', '#route'], truncated: false });
assert.equal(regions.collectScopeNodes(Array.from({ length: 5001 }, (_, i) => node('p' + i))).length, 5000);
assert.deepEqual(regions.convexHull([[10, 55], [11, 55], [10, 56], [10.2, 55.2]]), [[10, 55], [11, 55], [10, 56]]);
assert.deepEqual(regions.convexHull([[10, 55], [11, 55]]), []);

assert.deepEqual(regions.parseSelection('#/regions?window=7d&scope=%23dk'), { window: '7d', scope: '#dk' });
assert.deepEqual(regions.parseSelection('#/regions?window=oops&scope=%3Cbad%3E'), { window: '24h', scope: '' });
assert.deepEqual(regions.parseSelection('#/regions'), { window: '24h', scope: '' });
assert.equal(regions.scopeHash('#dk', '1h'), '#/regions?window=1h&scope=%23dk');
assert.equal(regions.scopeHash('', '7d'), '#/regions?window=7d');
assert.equal(regions.parseSelectedScope('#/regions?scope=%23dk'), '#dk');
assert.deepEqual(regions.parseSelection('#/regions?view=timeline&window=1h&scope=%23dk&at=2026-10-16T12%3A37%3A00Z&compare=1'),
  { window: '1h', scope: '#dk', view: 'timeline', at: '2026-10-16T12:37:00.000Z', compare: true });
assert.deepEqual(regions.parseSelection('#/regions?view=timeline&at=bad&compare=true'),
  { window: '24h', scope: '', view: 'timeline', at: '', compare: false });
assert.equal(regions.scopeHash('#dk', '1h', { view: 'timeline', at: '2026-10-16T12:00:00Z', compare: true }),
  '#/regions?window=1h&scope=%23dk&view=timeline&at=2026-10-16T12%3A00%3A00.000Z&compare=1');
assert.equal(regions.scopeHash('#dk', '1h', { view: 'latest', at: '2026-10-16T12:00:00Z', compare: true }),
  '#/regions?window=1h&scope=%23dk', 'latest URLs omit timeline state');
assert.deepEqual(regions.parseSelection('#/regions?view=health&window=7d&scope=%23dk&at=2026-10-16T11%3A37%3A00Z&compare=1'),
  { window: '7d', scope: '#dk', view: 'health', at: '2026-10-16T11:37:00.000Z', compare: true });
assert.deepEqual(regions.parseSelection('#/regions?view=health&window=bad&scope=%3Cbad%3E&at=bad&compare=true'),
  { window: '24h', scope: '', view: 'health', at: '', compare: false });
assert.equal(regions.scopeHash('#dk', '24h', { view: 'health', at: '2026-10-16T12:00:00Z', compare: true }),
  '#/regions?window=24h&scope=%23dk&view=health&at=2026-10-16T12%3A00%3A00.000Z&compare=1');
const healthBookmark = { window: '24h', scope: '#se', view: 'health', at: '2026-10-16T10:00:00.000Z', compare: false };
assert.deepEqual(regions.parseSelection(regions.scopeHash(healthBookmark.scope, healthBookmark.window, healthBookmark)), healthBookmark,
  'health selection round-trips through its bookmark');

const snapshot = { window: '24h', windowStart: '2026-10-15T12:00:00Z', windowEnd: '2026-10-16T12:00:00Z',
  generatedAt: '2026-10-16T12:00:00Z', coverage: { start: '2026-10-16T09:00:00Z', complete: false, loading: false } };
let coverage = regions.coverageInfo(snapshot);
assert.equal(coverage.kind, 'partial');
assert.equal(coverage.availableStart, '2026-10-16T09:00:00.000Z');
assert.match(coverage.message, /Partial retained history/);
coverage = regions.coverageInfo({ ...snapshot, coverage: { start: '2026-10-01T00:00:00Z', complete: true, loading: false } });
assert.equal(coverage.kind, 'complete');
assert.equal(coverage.availableStart, '2026-10-15T12:00:00.000Z', 'available interval is clipped to the requested window');
assert.equal(regions.coverageInfo({ ...snapshot, coverage: { start: null, complete: false, loading: false } }).kind, 'unknown');
assert.equal(regions.coverageInfo({ ...snapshot, coverage: { start: null, complete: false, loading: true } }).kind, 'loading');
assert.match(regions.coverageInfo({ ...snapshot, coverage: { start: '2026-10-17T00:00:00Z', complete: false } }).message, /No retained history/);
assert.equal(regions.coverageInfo({ ...snapshot, coverage: { start: '2026-10-16T09:00:00Z', complete: true } }).kind, 'partial', 'inconsistent complete flag cannot overstate available history');
const omittedEvidence = regions.coverageInfo({ ...snapshot, coverage: { start: '2026-10-01T00:00:00Z', complete: true,
  evidence: { excluded: 4, unprovable: 2, capped: 1, truncated: false, scopes: [] } } });
assert.equal(omittedEvidence.kind, 'complete', 'retention and attributable evidence are separate');
assert.match(omittedEvidence.message, /Observation evidence is incomplete in this interval: 2 observations could not be attributed .* and 1 were omitted by safety limits/);
assert.match(omittedEvidence.message, /4 observations were identified as direct, unscoped or for a different transport code and excluded; this does not make counts incomplete/);
const excludedOnly = regions.coverageInfo({ ...snapshot, coverage: { start: '2026-10-01T00:00:00Z', complete: true, evidence: { excluded: 3, unprovable: 0, capped: 0, truncated: false, scopes: [] } } });
assert.doesNotMatch(excludedOnly.message, /incomplete in this interval/, 'identified exclusions are not incomplete evidence');
assert.match(regions.evidenceText({ truncated: true }), /affected hours are unknown/);
assert.equal(regions.evidenceText(undefined), '');

const history = require('./public/regions-history.js');
const historyEnd = '2026-10-16T12:00:00Z';
const historyStart = '2026-09-25T12:00:00Z';
const historical = (key, buckets, extra = {}) => node(key, { scopes: [{ name: '#dk', buckets }], ...extra });
const bucket = (index, count = 1) => ({ index, count, lastRelayed: new Date(Date.parse(historyStart) + index * 3600000 + 1000).toISOString() });
const historyData = (extra = {}) => ({ generatedAt: '2026-10-16T12:34:00Z', historyStart, historyEnd,
  bucketSeconds: 3600, bucketCount: 504, coverage: { start: historyStart, loading: false },
  nodes: [historical('both', [bucket(502, 2), bucket(503, 3)]), historical('added', [bucket(503, 4)]),
    historical('before', [bucket(502, 5)], { lat: null, lon: null, positionSource: 'none' })], ...extra });
let indexed = history.indexHistory(historyData(), historyData().nodes);
let frame = history.selectFrame(indexed, { window: '1h', scope: '#dk', compare: true });
assert.equal(frame.interval.start, '2026-10-16T11:00:00.000Z');
assert.equal(frame.interval.previousStart, '2026-10-16T10:00:00.000Z');
assert.equal(frame.currentCount, 2);
assert.equal(frame.previousCount, 2, 'unpositioned relays belong in comparison counts');
assert.equal(frame.currentAppearances, 7);
assert.equal(frame.previousAppearances, 7);
assert.deepEqual(frame.added.map(n => n.publicKey), ['added']);
assert.deepEqual(frame.notObserved.map(n => n.publicKey), ['before']);
assert.deepEqual(frame.retained.map(n => n.publicKey), ['both']);
assert.equal(frame.percentChange, 0);
assert.equal(frame.rows[0].node.lat, 55, 'historical frames retain supplied latest coordinates');
frame = history.selectFrame(indexed, { window: '24h', at: '2026-10-16T11:53:00Z', scope: '#dk' });
assert.equal(frame.interval.end, '2026-10-16T11:00:00.000Z', 'frame boundaries are completed UTC hours');
assert.equal(frame.interval.start, '2026-10-15T11:00:00.000Z');
assert.equal(frame.currentAppearances, 7, 'the selected end is exclusive');
assert.equal(history.frameTime(indexed, 0), '2026-10-09T12:00:00.000Z');
assert.equal(history.frameTime(indexed, 168), '2026-10-16T12:00:00.000Z');
assert.equal(history.frameIndex(indexed, '2099-01-01T00:00:00Z'), 168);
assert.equal(history.frameIndex(indexed, '2000-01-01T00:00:00Z'), 0);
for (const coverage of [{ start: null, loading: false }, { start: historyStart, loading: true },
  { start: '2026-10-16T10:30:00Z', loading: false }]) {
  const partial = history.indexHistory(historyData({ coverage }), historyData().nodes);
  const limited = history.selectFrame(partial, { window: '1h', compare: true });
  assert.equal(limited.comparable, false);
  assert.equal(limited.added, null, 'unavailable history must withhold new/missing classifications');
  assert.equal(limited.percentChange, null);
  assert.equal(limited.currentAppearances, 7, 'limited history still shows observed counts');
}
frame = history.selectFrame(history.indexHistory(historyData({ truncated: true }), historyData().nodes), { window: '1h', compare: true });
assert.equal(frame.comparable, false, 'caps withhold changes');
assert.equal(frame.limited, true);
const unverifiedHistory = history.indexHistory(historyData({ coverage: { start: historyStart, loading: false,
  evidence: { excluded: 0, unprovable: 1, capped: 0, truncated: false, scopes: [{ name: '#dk', excluded: 0, unprovable: 1, capped: 0, buckets: [{ index: 503, excluded: 0, unprovable: 1, capped: 0 }] }] } } }), historyData().nodes);
frame = history.selectFrame(unverifiedHistory, { window: '1h', compare: true });
assert.equal(frame.currentCoverage, 'complete', 'evidence omissions do not rewrite the retained-history floor');
assert.equal(frame.limited, false, 'evidence gaps are not snapshot-wide publication limits');
assert.equal(frame.evidence.incomplete, true);
assert.equal(frame.comparable, false, 'unattributable observations cannot support change classifications');
assert.equal(frame.added, null);
assert.equal(history.selectActivity(history.indexActivity(unverifiedHistory, ''), unverifiedHistory, frame).ratePerHour, null);
frame = history.selectFrame(indexed, { window: '1h', scope: '#absent', compare: true });
assert.equal(frame.currentCount, 0);
assert.equal(frame.currentCoverage, 'complete', 'no observations is separate from unavailable history');
assert.equal(frame.percentChange, null, 'zero baseline has no percentage change');
const tooMany = Array.from({ length: 5001 }, (_, i) => historical('p' + i, [bucket(503)]));
indexed = history.indexHistory(historyData(), tooMany);
assert.equal(indexed.nodes.length, 5000);
assert.equal(indexed.limited, true);
const badBuckets = [bucket(-1), bucket(504), bucket(503, -1), bucket(503, Number.MAX_VALUE), bucket(503, 2)];
indexed = history.indexHistory(historyData(), [historical('bad', badBuckets)]);
assert.equal(history.selectFrame(indexed, { window: '1h' }).currentAppearances, 2);
assert.equal(indexed.limited, true, 'invalid cells cannot support a complete comparison');
indexed = history.indexHistory(historyData(), [historical('many-scopes', [], {
  scopes: Array.from({ length: 33 }, (_, i) => ({ name: '#s' + i, buckets: [bucket(503)] })),
})]);
assert.equal(history.selectFrame(indexed, { window: '1h' }).currentAppearances, 32);
assert.equal(indexed.limited, true, 'the 32-scope per-node cap withholds comparisons');
const fullCells = Array.from({ length: 501 }, (_, i) => bucket(i + 3));
indexed = history.indexHistory(historyData(), Array.from({ length: 500 }, (_, i) => historical('bounded' + i, fullCells)));
assert.equal(indexed.limited, true, 'the global 250,000-cell bound is enforced');
assert.equal(history.selectFrame(indexed, { window: '1h', compare: true }).added, null);
assert(indexed.nodes.reduce((total, n) => total + n.scopes.reduce((sum, s) => sum + s.indices.length, 0), 0) <= 250000);
assert.throws(() => history.indexHistory(historyData({ bucketCount: 505 }), []), /history/i);
assert.throws(() => history.indexHistory(historyData({ historyEnd: '2026-10-16T12:30:00Z' }), []), /history/i);

const healthNodes = [historical('health-a', [], { scopes: [
  { name: '#dk', buckets: [bucket(502, 2), bucket(503, 3)] },
  { name: '#se', buckets: [{ ...bucket(503, 4), lastRelayed: '2026-10-16T11:59:00Z' }] },
] }), historical('health-b', [{ ...bucket(503, 5), lastRelayed: '2026-10-16T11:00:03Z' }]),
historical('health-unpositioned', [], { lat: null, lon: null, positionSource: 'none', metadataSource: 'archived',
  scopes: [{ name: '#no', buckets: [bucket(502, 7)] }] })];
const healthHistory = history.indexHistory(historyData(), healthNodes);
const activity = history.indexActivity(healthHistory, '');
const healthFrame = history.selectFrame(healthHistory, { window: '1h', compare: true });
const frameBeforeHealth = JSON.stringify(healthFrame);
let health = history.selectActivity(activity, healthHistory, healthFrame);
assert.equal(health.bins.length, 1);
assert.deepEqual(health.bins[0], { start: '2026-10-16T11:00:00.000Z', end: '2026-10-16T12:00:00.000Z',
  coverage: 'complete', evidenceIncomplete: false, observedAppearances: 12, observedNodes: 2, appearances: 12, nodes: 2 });
assert.equal(health.lastRelayed, '2026-10-16T11:59:00.000Z');
assert.equal(health.ratePerHour, 12);
assert.equal(JSON.stringify(healthFrame), frameBeforeHealth, 'health derivation does not change shared comparison results');
const dkActivity = history.indexActivity(healthHistory, '#dk');
const dkFrame = history.selectFrame(healthHistory, { window: '1h', scope: '#dk', compare: true });
health = history.selectActivity(dkActivity, healthHistory, dkFrame);
assert.equal(health.bins[0].appearances, 8, 'scope selection excludes other scoped appearances');
assert.equal(health.bins[0].nodes, 2, 'all-scopes aggregation counts the same relay once per hour');
assert.equal(health.lastRelayed, '2026-10-16T11:00:03.000Z', 'a newer relay in another scope cannot advance the selected scope timestamp');
for (const [window, hours] of [['1h', 1], ['24h', 24], ['7d', 168]]) {
  const selected = history.selectFrame(healthHistory, { window });
  const view = history.selectActivity(activity, healthHistory, selected);
  assert.equal(view.bins.length, hours);
  assert.equal(view.bins.reduce((sum, bin) => sum + bin.observedAppearances, 0), selected.currentAppearances);
  assert.equal(view.ratePerHour, selected.currentAppearances / hours);
  for (let i = 1; i < view.bins.length; i++) assert.equal(view.bins[i - 1].end, view.bins[i].start, 'hourly bins are disjoint and contiguous');
}
health = history.selectActivity(activity, healthHistory, history.selectFrame(healthHistory, { window: '1h', at: '2026-10-16T11:00:00Z' }));
assert.equal(health.bins[0].appearances, 9, 'selected end excludes the next hour');
assert.equal(health.bins[0].nodes, 2, 'unpositioned archived relays contribute activity evidence');
assert.equal(health.lastRelayed, '2026-10-16T10:00:01.000Z', 'last relay is restricted to selected interval');
health = history.selectActivity(activity, healthHistory, history.selectFrame(healthHistory, { window: '1h', at: '2026-10-16T10:00:00Z' }));
assert.equal(health.bins[0].appearances, 0, 'complete empty hours are measured zero');
assert.equal(health.bins[0].nodes, 0);
assert.equal(health.lastRelayed, '');
assert.equal(health.ratePerHour, 0);
for (const [coverage, expected] of [
  [{ start: null, loading: false }, 'unknown'], [{ start: historyStart, loading: true }, 'loading'],
  [{ start: '2026-10-16T12:00:00Z', loading: false }, 'unavailable'],
  [{ start: '2026-10-16T11:30:00Z', loading: false }, 'partial'],
]) {
  const h = history.indexHistory(historyData({ coverage }), healthNodes);
  const selected = history.selectFrame(h, { window: '1h' });
  const view = history.selectActivity(history.indexActivity(h, ''), h, selected);
  assert.equal(view.bins[0].coverage, expected);
  assert.equal(view.bins[0].appearances, null, 'incomplete history cannot become chart zero');
  assert.equal(view.bins[0].nodes, null);
  assert.equal(view.bins[0].observedAppearances, 12, 'partial observed evidence stays available');
  assert.equal(view.ratePerHour, null, 'incomplete intervals have no hourly rate');
}
for (const flag of ['truncated', 'scopesTruncated', 'bucketsTruncated']) {
  const h = history.indexHistory(historyData({ [flag]: true }), healthNodes);
  const view = history.selectActivity(history.indexActivity(h, ''), h, history.selectFrame(h, { window: '1h' }));
  assert.equal(view.bins[0].coverage, 'complete', 'retention coverage remains distinct from safety limits');
  assert.equal(view.bins[0].appearances, null);
  assert.equal(view.bins[0].nodes, null);
  assert.equal(view.ratePerHour, null);
}
const mixedCoverage = history.indexHistory(historyData({ coverage: { start: '2026-10-16T10:30:00Z', loading: false } }), healthNodes);
const mixedActivity = history.indexActivity(mixedCoverage, '');
const mixedFrame = history.selectFrame(mixedCoverage, { window: '1h', compare: true });
assert.equal(mixedFrame.comparable, false);
assert.equal(history.selectActivity(mixedActivity, mixedCoverage, mixedFrame).ratePerHour, 12,
  'complete selected rate does not require complete reference history');
health = history.selectActivity(mixedActivity, mixedCoverage, history.selectFrame(mixedCoverage, { window: '24h' }));
assert.equal(health.bins[22].coverage, 'partial');
assert.equal(health.bins[22].appearances, null);
assert.equal(health.bins[22].observedAppearances, 9);
assert.equal(health.bins[23].appearances, 12, 'complete bins remain available inside a partial interval');
assert.equal(health.ratePerHour, null);
const isolatedActivity = history.indexActivity(healthHistory, '#absent');
health = history.selectActivity(isolatedActivity, healthHistory, history.selectFrame(healthHistory, { window: '1h', scope: '#absent' }));
assert.equal(health.bins[0].appearances, 0);
assert.equal(health.lastRelayed, '');
assert.throws(() => history.selectActivity(activity, mixedCoverage, mixedFrame), /activity/i, 'stale aggregate cannot be paired with a different history snapshot');
for (const bounds of [
  { start: '2026-10-16T10:00:00Z', end: '2026-10-16T12:00:00Z' },
  { start: '2026-10-16T11:30:00Z', end: '2026-10-16T12:30:00Z' },
  { start: '2026-09-24T11:00:00Z', end: '2026-09-24T12:00:00Z' },
  { start: '2026-10-16T12:00:00Z', end: '2026-10-16T13:00:00Z' },
  { start: 'invalid', end: 'invalid' },
]) assert.throws(() => history.selectActivity(activity, healthHistory, { ...healthFrame, interval: bounds }), /activity interval/i);

// F1 (PR #507 review): evidence gaps are per hour bucket and scope. Excluded
// observations never withhold values; unprovable/capped ones withhold only the
// rates, chart values and comparisons that include an affected bucket/scope.
const evidenceCoverage = { start: historyStart, loading: false, evidence: { excluded: 9, unprovable: 1, capped: 1, truncated: false, scopes: [
  { name: '#dk', excluded: 9, unprovable: 1, capped: 0, buckets: [{ index: 500, excluded: 0, unprovable: 1, capped: 0 }, { index: 503, excluded: 9, unprovable: 0, capped: 0 }] },
  { name: '#se', excluded: 0, unprovable: 0, capped: 1, buckets: [{ index: 503, excluded: 0, unprovable: 0, capped: 1 }] },
] } };
const gapHistory = history.indexHistory(historyData({ coverage: evidenceCoverage }), healthNodes);
assert.equal(gapHistory.limited, false, 'evidence gaps are not a snapshot-wide limit');
let gapFrame = history.selectFrame(gapHistory, { window: '1h', scope: '#dk', compare: true });
assert.equal(gapFrame.comparable, true, 'excluded-only #dk hours stay comparable');
assert.deepEqual(gapFrame.evidence, { excluded: 9, unprovable: 0, capped: 0, incomplete: false });
let gapView = history.selectActivity(history.indexActivity(gapHistory, '#dk'), gapHistory, gapFrame);
assert.equal(gapView.ratePerHour, 8, 'an unaffected bucket keeps its rate despite gaps elsewhere');
assert.equal(gapView.bins[0].appearances, 8);
gapFrame = history.selectFrame(gapHistory, { window: '1h', compare: true });
assert.equal(gapFrame.comparable, false, 'all scopes include the capped #se hour');
assert.deepEqual(gapFrame.evidence, { excluded: 9, unprovable: 0, capped: 1, incomplete: true });
gapView = history.selectActivity(history.indexActivity(gapHistory, ''), gapHistory, gapFrame);
assert.equal(gapView.ratePerHour, null);
assert.equal(gapView.bins[0].appearances, null);
assert.equal(gapView.bins[0].evidenceIncomplete, true);
assert.equal(gapView.bins[0].observedAppearances, 12, 'retained observations stay visible');
gapFrame = history.selectFrame(gapHistory, { window: '24h', scope: '#dk' });
gapView = history.selectActivity(history.indexActivity(gapHistory, '#dk'), gapHistory, gapFrame);
assert.equal(gapView.ratePerHour, null, 'the 24h #dk interval contains the unprovable hour');
assert.equal(gapView.bins[20].appearances, null, 'bucket 500 is withheld');
assert.equal(gapView.bins[20].evidenceIncomplete, true);
assert.equal(gapView.bins[23].appearances, 8, 'other #dk bins in the same interval stay available');
assert.equal(gapView.bins[22].appearances, 2);
assert.equal(history.selectFrame(gapHistory, { window: '1h', at: '2026-10-16T11:00:00Z', scope: '#dk', compare: true }).comparable, true,
  'buckets 502 vs 501 are unaffected');
assert.equal(history.selectFrame(gapHistory, { window: '1h', at: '2026-10-16T10:00:00Z', scope: '#dk', compare: true }).comparable, false,
  'the previous interval contains unprovable bucket 500');
assert.equal(history.selectFrame(gapHistory, { window: '1h', at: '2026-10-16T10:00:00Z', scope: '#se', compare: true }).comparable, true,
  'another scope is unaffected by the #dk gap');
for (const evidence of [{ ...evidenceCoverage.evidence, truncated: true }, { scopes: [{ name: '#dk', buckets: [{ index: 504, unprovable: 1 }] }] }, { scopes: 'bad' }]) {
  const failClosed = history.indexHistory(historyData({ coverage: { ...evidenceCoverage, evidence } }), healthNodes);
  const selected = history.selectFrame(failClosed, { window: '1h', scope: '#dk', compare: true });
  assert.equal(selected.comparable, false, 'unknown evidence placement fails closed');
  assert.equal(history.selectActivity(history.indexActivity(failClosed, '#dk'), failClosed, selected).ratePerHour, null);
}

// Reading the sparse payload after indexing must not be part of chart playback.
const playbackFrame = history.selectFrame(healthHistory, { window: '7d' });
Object.defineProperty(healthHistory, 'nodes', { get() { throw new Error('sparse node rescan'); } });
assert.equal(history.selectActivity(activity, healthHistory, playbackFrame).bins.length, 168);
const capNodes = Array.from({ length: 500 }, (_, i) => historical('cap-health-' + i, Array.from({ length: 500 }, (_, index) => bucket(index + 4))));
const capHistory = history.indexHistory(historyData(), capNodes);
const capActivity = history.indexActivity(capHistory, '');
const capFrame = history.selectFrame(capHistory, { window: '7d' });
health = history.selectActivity(capActivity, capHistory, capFrame);
assert.equal(capHistory.limited, false);
assert.equal(health.bins.length, 168);
assert.equal(health.bins[0].nodes, 500);
assert.equal(health.bins[0].appearances, 500);
assert.equal(health.bins.reduce((total, bin) => total + bin.appearances, 0), capFrame.currentAppearances);

const allDayNode = historical('same-relay-all-day', Array.from({ length: 24 }, (_, index) => bucket(480 + index, 2)));
const allDayHistory = history.indexHistory(historyData(), [allDayNode]);
const allDayFrame = history.selectFrame(allDayHistory, { window: '24h' });
health = history.selectActivity(history.indexActivity(allDayHistory, ''), allDayHistory, allDayFrame);
assert.equal(allDayFrame.currentCount, 1, 'one relay remains one unique relay across the whole selected interval');
assert(health.bins.every(bin => bin.nodes === 1));
assert.equal(health.bins.reduce((total, bin) => total + bin.nodes, 0), 24, 'summing hourly relay counts is not a unique interval relay count');
for (const ending of ['2026-10-26T12:00:00Z', '2026-11-02T12:00:00Z']) {
  const end = Date.parse(ending), start = end - 504 * 3600000;
  const dstData = historyData({ historyStart: new Date(start).toISOString(), historyEnd: ending,
    generatedAt: ending, coverage: { start: new Date(start).toISOString(), loading: false } });
  const dstHistory = history.indexHistory(dstData, []);
  const dstFrame = history.selectFrame(dstHistory, { window: '7d' });
  const dstHealth = history.selectActivity(history.indexActivity(dstHistory, ''), dstHistory, dstFrame);
  assert.equal(dstHealth.bins.length, 168, 'UTC week keeps 168 hours across European and US DST changes');
  assert(dstHealth.bins.every(bin => Date.parse(bin.end) - Date.parse(bin.start) === 3600000));
  assert.equal(dstHealth.bins[167].end, new Date(end).toISOString());
}

const routes = require('./public/regions-routes.js');
const fullKey = letter => letter.repeat(64);
const routeNodes = [historical(fullKey('a'), [], { scopes: [{ name: '#a|#b', buckets: [bucket(503)] }, { name: '#c', buckets: [bucket(503)] }] }),
  historical(fullKey('b'), [], { scopes: [{ name: '#a', buckets: [bucket(503)] }, { name: '#b|#c', buckets: [bucket(503)] }] }),
  historical(fullKey('c'), [], { positionSource: 'none', lat: null, lon: null, scopes: [{ name: '#a', buckets: [bucket(503)] }, { name: '#c', buckets: [bucket(503)] }] })];
const routeData = { ...historyData(), nodes: routeNodes, steps: [
  { from: fullKey('a'), to: fullKey('b'), scope: '#a', buckets: [bucket(503, 3)] },
  { from: fullKey('b'), to: fullKey('a'), scope: '#a', buckets: [bucket(503, 2)] },
  { from: fullKey('a'), to: fullKey('c'), scope: '#c', buckets: [bucket(503)] },
], stepEvidence: { excludedObservations: 5, unprovableObservations: 0, cappedObservations: 0, unresolvedHops: 0, cappedStepAppearances: 0, sourceTruncated: false, incomplete: false } };
const routeHistory = history.indexHistory(routeData, routeNodes);
let routeIndex = routes.indexRoutes(routeData, routeHistory, regions.validScope);
let routeFrame = history.selectFrame(routeHistory, { window: '1h' });
let connections = routes.selectRoutes(routeIndex, routeHistory, routeFrame, {});
assert.equal(connections.steps.length, 3);
assert.equal(connections.stepAppearances, 6, 'step totals count edge appearances, not global distinct packets');
assert.equal(connections.unmappedSteps, 1);
assert.equal(connections.pairs.length, 3, 'scope names containing delimiters must not collide');
assert.equal(connections.pairs.find(pair => pair.from === '#a|#b' && pair.to === '#c').count, 1);
assert.equal(connections.pairs.find(pair => pair.from === '#a' && pair.to === '#b|#c').count, 1);
assert.equal(connections.complete, true);
connections = routes.selectRoutes(routeIndex, routeHistory, routeFrame, { scope: '#a' });
assert.equal(connections.steps.length, 2);
assert.equal(connections.pairs.length, 2);
assert(connections.steps.some(step => step.from === fullKey('a') && step.to === fullKey('b')));
assert(connections.steps.some(step => step.from === fullKey('b') && step.to === fullKey('a')));
for (const [index, expected] of [[503, false], [400, true]]) {
  const gapRoutes = { ...routeData, coverage: { ...routeData.coverage, evidence: { excluded: 0, unprovable: 1, capped: 0, truncated: false,
    scopes: [{ name: '#a', excluded: 0, unprovable: 1, capped: 0, buckets: [{ index, excluded: 0, unprovable: 1, capped: 0 }] }] } } };
  const gapHistory = history.indexHistory(gapRoutes, routeNodes);
  const selected = routes.selectRoutes(routes.indexRoutes(gapRoutes, gapHistory, regions.validScope), gapHistory, history.selectFrame(gapHistory, { window: '1h' }), {});
  assert.equal(selected.sharedComplete, expected, 'shared memberships are qualified only by gaps inside the selected interval');
  assert.equal(selected.complete, expected);
}
const limitedData = { ...routeData, stepBucketsTruncated: true };
connections = routes.selectRoutes(routes.indexRoutes(limitedData, routeHistory, regions.validScope), routeHistory, routeFrame, {});
assert.equal(connections.complete, false);
assert.equal(connections.stepAppearances, 6, 'limited evidence remains an explicit lower bound');
assert.equal(connections.evidence.excludedObservations, 5, 'identified exclusions are disclosed without qualifying steps');
for (const key of ['unprovableObservations', 'cappedObservations', 'unresolvedHops', 'cappedStepAppearances', 'missingEndpointSteps']) {
  const contradictory = { ...routeData, stepEvidence: { ...routeData.stepEvidence, [key]: 1, incomplete: false } };
  const selected = routes.selectRoutes(routes.indexRoutes(contradictory, routeHistory, regions.validScope), routeHistory, routeFrame, {});
  assert.equal(selected.complete, false, 'omitted evidence cannot be complete even when the server flag contradicts diagnostics');
  assert.equal(selected.stepAppearances, 6);
}
const badRoutes = { ...routeData, steps: [{ ...routeData.steps[0], to: 'bad' }, { ...routeData.steps[0], scope: '<bad>' },
  { ...routeData.steps[0], buckets: [bucket(504)] }] };
assert.equal(routes.indexRoutes(badRoutes, routeHistory, regions.validScope).limited, true);
const cliqueNodes = Array.from({ length: 5000 }, (_, i) => historical(i.toString(16).padStart(64, '0'), [], {
  scopes: Array.from({ length: 32 }, (_, j) => ({ name: '#scope' + j, buckets: [bucket(503)] })),
}));
const cliqueHistory = history.indexHistory(historyData(), cliqueNodes);
const cliqueIndex = routes.indexRoutes({ steps: [] }, cliqueHistory, regions.validScope);
const cliqueFrame = history.selectFrame(cliqueHistory, { window: '1h' });
const clique = routes.selectRoutes(cliqueIndex, cliqueHistory, cliqueFrame, {});
assert.equal(clique.pairs.length, 496);
assert(clique.pairs.every(pair => pair.count === 5000), 'retained pairs have exact distinct-relay weights');
assert(clique.pairs.every(pair => pair.examples.length <= 3));
assert(clique.pairs.length <= 1000);
assert.equal(routes.selectRoutes(cliqueIndex, cliqueHistory, cliqueFrame, {}).pairs, clique.pairs, 'same interval reuses bounded relation aggregation');
const routeOnlyData = { ...routeData, stepNodes: [
  { publicKey: fullKey('d'), name: 'Route only from', role: 'repeater', lat: 55, lon: 10, positionSource: 'reported' },
  { publicKey: fullKey('e'), name: 'Route only to', role: 'room', lat: 56, lon: 11, positionSource: 'reported' },
], steps: [{ from: fullKey('d'), to: fullKey('e'), scope: '#routeonly', buckets: [bucket(503, 7)] }] };
const routeOnly = routes.selectRoutes(routes.indexRoutes(routeOnlyData, routeHistory, regions.validScope, regions.validPosition), routeHistory, routeFrame, { scope: '#routeonly' });
assert.equal(routeOnly.steps.length, 1, 'own flood endpoint metadata is independent of canonical-route relay history');
assert.equal(routeOnly.stepAppearances, 7);
assert.equal(routeOnly.pairs.length, 0, 'route-only endpoints must not create scope membership');
assert.equal(routeOnly.mapSteps.length, 1);
assert.equal(regions.parseSelection('#/regions?view=connections&window=1h&scope=%23routeonly&compare=1&page=2').compare, false);
assert.equal(regions.scopeHash('#routeonly', '1h', { view: 'connections', at: historyEnd, page: 2, pairsPage: 3, compare: true }),
  '#/regions?window=1h&scope=%23routeonly&view=connections&at=2026-10-16T12%3A00%3A00.000Z&page=2&pairsPage=3');
assert.equal(history.scopeCount(cliqueHistory.nodes[0].scopes[0], 503, 504).count, 1, 'connections reuse the existing sparse range reader');
const populatedSteps = Array.from({ length: 5000 }, (_, index) => ({ from: index.toString(16).padStart(64, '0'),
  to: ((index + 1) % 5000).toString(16).padStart(64, '0'), scope: '#scope0', buckets: Array.from({ length: 20 }, (_, n) => bucket(484 + n)) }));
const fullRoutes = routes.indexRoutes({ steps: populatedSteps, stepEvidence: routeData.stepEvidence }, cliqueHistory, regions.validScope);
const fullConnections = routes.selectRoutes(fullRoutes, cliqueHistory, cliqueFrame, { scope: '#scope0' });
assert.equal(fullConnections.steps.length, 5000);
assert.equal(fullConnections.mapSteps.length, 200, 'the map has a separate bounded rendering budget');
assert.equal(fullRoutes.steps.reduce((sum, step) => sum + step.indices.length, 0), 100000);
const cappedSteps = routes.indexRoutes({ steps: [...populatedSteps, populatedSteps[0]], stepEvidence: routeData.stepEvidence }, cliqueHistory, regions.validScope);
assert.equal(cappedSteps.limited, true, 'row caps cannot silently imply complete evidence');
const diverseNodes = Array.from({ length: 80 }, (_, i) => historical((10000 + i).toString(16).padStart(64, '0'), [], {
  scopes: Array.from({ length: 32 }, (_, j) => ({ name: '#group' + i + 's' + j, buckets: [bucket(503)] })),
}));
const diverseHistory = history.indexHistory(historyData(), diverseNodes);
const diverseFrame = history.selectFrame(diverseHistory, { window: '1h' });
const diverseIndex = routes.indexRoutes({ steps: [] }, diverseHistory, regions.validScope);
const cappedPairs = routes.selectRoutes(diverseIndex, diverseHistory, diverseFrame, {});
assert.equal(cappedPairs.pairs.length, 1000);
assert.equal(cappedPairs.pairsTruncated, true);
assert(cappedPairs.pairs.every(pair => pair.count === 1), 'retained pair counts stay exact after discovery reaches its cap');
assert.equal(routes.selectRoutes(diverseIndex, diverseHistory, diverseFrame, { scope: '#group79s0' }).pairs.length, 31,
  'scope filtering finds exact pairs beyond the unfiltered retained subset');

console.log('regions map, timeline, activity and connection unit tests passed');
