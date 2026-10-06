'use strict';
// Exercise the actual nodes.js helper, not a copied implementation.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const context = vm.createContext({
  window: {}, localStorage: { getItem: () => null }, registerPage: () => {},
  escapeHtml: value => String(value).replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]),
});
vm.runInContext(fs.readFileSync(path.join(__dirname, 'public/nodes.js'), 'utf8'), context);
const view = context.window._nodesNeighborEstimateView;
let checks = 0;
function test(name, fn) { fn(); checks++; console.log('PASS ' + name); }
function node(estimate = {}) {
  return { neighbor_estimate: {
    status: 'estimated', method: 'neighbor_cluster_v1', lat: 55.123456, lon: 12.654321,
    contributor_count: 3, candidate_count: 5, spread_km: 4.1234,
    oldest_seen: '2026-01-01T10:00:00Z', newest_seen: '2026-01-03T10:00:00Z',
    unknown_freshness_count: 0, ...estimate,
    area: estimate.area === undefined ? { kind: 'polygon', vertices: [{lat:55,lon:12},{lat:55.1,lon:12},{lat:55,lon:12.1}] } : estimate.area,
  } };
}
test('no data remains absent on old APIs', () => {
  assert.equal(view({}).visible, false);
});
test('supported estimate is approximate and explains the limits', () => {
  const result = view(node());
  assert.equal(result.hasPosition, true);
  assert.equal(result.kind, 'polygon');
  assert.match(result.html, /Neighbor evidence area/);
  assert.doesNotMatch(result.html, /55\.12345/);
  assert.match(result.html, /3 of 5 candidate neighbors/);
  assert.match(result.html, /4\.1 km/);
  assert.match(result.html, /not a location error radius/);
  assert.match(result.html, /not triangulation/);
  assert.match(result.html, /2026-01-01/);
  assert.match(result.html, /2026-01-03/);
});
test('strings are converted at the boundary, including zero coordinates', () => {
  const result = view(node({ lat: '0', lon: '12.1', contributor_count: '2', candidate_count: '3', spread_km: '0' }));
  assert.equal(result.hasPosition, true);
  assert.equal(result.vertices.length, 3);
  assert.match(result.html, /2 of 3/);
  assert.match(result.html, /0\.0 km/);
});
for (const status of ['insufficient', 'ambiguous', 'unavailable']) {
  test(status + ' cannot reuse stale typed or legacy coordinates', () => {
    const result = view({ ...node({ status }), estimated_lat: 55, estimated_lon: 12, estimated_contributor_count: 7 });
    assert.equal(result.hasPosition, false);
    assert.equal(result.visible, true);
    assert.doesNotMatch(result.html, /~55/);
    assert.match(result.html, status === 'insufficient' ? /Insufficient/ : status === 'ambiguous' ? /Conflicting/ : /unavailable/i);
  });
}
test('a single contributor cannot masquerade as a supported estimate', () => {
  const result = view(node({ contributor_count: 1 }));
  assert.equal(result.hasPosition, false);
  assert.match(result.html, /Insufficient/);
});
test('invalid coordinates, numbers and unknown statuses never make markers', () => {
  for (const patch of [{ lat: null }, { lat: '' }, { lat: true }, { lat: 'NaN' }, { lon: 'Infinity' }, { lat: 91 }, { lon: -181 }, { contributor_count: -2 }, { contributor_count: 2.5 }, { status: '<img src=x onerror=alert(1)>' }]) {
    const result = view(node(patch));
    assert.equal(result.hasPosition, false, JSON.stringify(patch));
    assert.doesNotMatch(result.html, /<img|NaN|Infinity/);
  }
});
test('invalid optional evidence is omitted, not fabricated or interpolated', () => {
  const result = view(node({ oldest_seen: '<img src=x>', newest_seen: 'nonsense', spread_km: -1, candidate_count: '<script>', unknown_freshness_count: '<img>' }));
  assert.equal(result.hasPosition, true);
  assert.doesNotMatch(result.html, /<img|<script|NaN|Invalid Date|Neighbor spread/);
});
test('unknown freshness is explicit and dates describe link sightings', () => {
  const result = view(node({ unknown_freshness_count: 1 }));
  assert.match(result.html, /Neighbor links last seen/);
  assert.match(result.html, /1 neighbor has unknown freshness/);
});
test('legacy coordinates alone never make a precise-looking point', () => {
  const result = view({ estimated_lat: '55.1', estimated_lon: '12.2' });
  assert.equal(result.hasPosition, false);
  assert.match(result.html, /Legacy estimate/);
  assert.match(result.html, /evidence quality unavailable/);
  assert.doesNotMatch(result.html, /spread|last seen/i);
});
test('missing, oversized or invalid geometry abstains without inventing a point', () => {
  for (const area of [null, {kind:'polygon',vertices:[{lat:55,lon:12}]}, {kind:'polygon',vertices:Array(21).fill({lat:55,lon:12})}, {kind:'line',vertices:[{lat:91,lon:12},{lat:55,lon:12}]}]) {
    const result=view(node({area}));
    assert.equal(result.hasPosition,false);
    assert.doesNotMatch(result.html,/~55/);
  }
});
test('dateline evidence is unwrapped locally rather than spanning Greenwich', () => {
  const result=view(node({area:{kind:'line',vertices:[{lat:10,lon:179.9},{lat:10.1,lon:-179.9}]}}));
  assert.equal(result.kind,'line');
  assert.ok(Math.abs(result.vertices[1][1]-result.vertices[0][1])<1);
  assert.match(result.html,/no area can be inferred/);
});
test('legacy single-neighbor and invalid coordinate estimates are withheld', () => {
  assert.equal(view({ estimated_lat: 55, estimated_lon: 12, estimated_contributor_count: 1 }).hasPosition, false);
  assert.equal(view({ estimated_lat: '', estimated_lon: 12 }).hasPosition, false);
});
test('reported-position comparison is retained and not called an error bound', () => {
  const result = view({ ...node(), lat: 55, lon: 12, estimated_distance_km: '8.7' });
  assert.match(result.html, /8\.7 km from reported position \(not an error bound\)/);
  for (const patch of [{ lat: null }, { lat: 0, lon: 0 }, { estimated_distance_km: -1 }, { estimated_distance_km: '' }]) {
    assert.doesNotMatch(view({ ...node(), lat: 55, lon: 12, estimated_distance_km: 8.7, ...patch }).html, /from reported position/);
  }
});
console.log(checks + ' neighbor-estimate checks passed');
