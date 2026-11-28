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
assert.equal(regions.collectScopeNodes(Array.from({ length: 5001 }, (_, i) => node('p' + i))).length, 5000);
assert.deepEqual(regions.convexHull([[10, 55], [11, 55], [10, 56], [10.2, 55.2]]), [[10, 55], [11, 55], [10, 56]]);
assert.deepEqual(regions.convexHull([[10, 55], [11, 55]]), []);

assert.deepEqual(regions.parseSelection('#/regions?window=7d&scope=%23dk'), { window: '7d', scope: '#dk' });
assert.deepEqual(regions.parseSelection('#/regions?window=oops&scope=%3Cbad%3E'), { window: '24h', scope: '' });
assert.deepEqual(regions.parseSelection('#/regions'), { window: '24h', scope: '' });
assert.equal(regions.scopeHash('#dk', '1h'), '#/regions?window=1h&scope=%23dk');
assert.equal(regions.scopeHash('', '7d'), '#/regions?window=7d');
assert.equal(regions.parseSelectedScope('#/regions?scope=%23dk'), '#dk');

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

// #523: Regions was trimmed back to the Latest view (#501). The removed
// history/routes endpoints and their page scripts must not be referenced
// by the frontend, the SPA shell or the docs, and the docs must not keep
// announcing the removed views as planned milestones.
const fs = require('node:fs');
const path = require('node:path');
const removedRegionRefs = /\/api\/regions\/(history|routes)\b|regions-(history|routes)\.js/;
const scanFiles = dir => fs.readdirSync(dir, { withFileTypes: true }).flatMap(entry => {
  const full = path.join(dir, entry.name);
  if (entry.isDirectory()) return entry.name === 'vendor' ? [] : scanFiles(full);
  return /\.(js|html|css|md)$/.test(entry.name) ? [full] : [];
});
const releaseNote = rel => /^docs\/release-notes\//.test(rel) || /^docs\/RELEASE-NOTES-/.test(rel);
const residualRefs = root => scanFiles(path.join(root, 'public')).concat(scanFiles(path.join(root, 'docs')))
  .filter(file => !releaseNote(path.relative(root, file).split(path.sep).join('/')))
  .filter(file => removedRegionRefs.test(fs.readFileSync(file, 'utf8')))
  .map(file => path.relative(root, file));
const residual = residualRefs(__dirname);
assert.deepEqual(residual, [], 'removed Regions history/routes references remain');
for (const file of ['regions-history.js', 'regions-routes.js']) {
  assert.equal(fs.existsSync(path.join(__dirname, 'public', file)), false, file + ' must stay removed');
}
assert.doesNotMatch(fs.readFileSync(path.join(__dirname, 'docs/regions-observed.md'), 'utf8'), /## Following milestones/,
  'removed Regions views must not be listed as upcoming milestones');

// Nit 2 (round 2): a release note about the removal legitimately names the
// removed endpoints. It must not trip the scan, while stale docs still do.
const os = require('node:os');
const fixtureRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'regions-residual-'));
try {
  for (const dir of ['public', 'docs/release-notes']) fs.mkdirSync(path.join(fixtureRoot, dir), { recursive: true });
  const note = 'Removed /api/regions/history and regions-routes.js.\n';
  fs.writeFileSync(path.join(fixtureRoot, 'docs/release-notes/v9.md'), note);
  fs.writeFileSync(path.join(fixtureRoot, 'docs/RELEASE-NOTES-v9.md'), note);
  assert.deepEqual(residualRefs(fixtureRoot), [], 'release notes may mention the removal');
  fs.writeFileSync(path.join(fixtureRoot, 'docs/stale.md'), note);
  fs.writeFileSync(path.join(fixtureRoot, 'public/app.js'), "fetch('/api/regions/routes');\n");
  assert.deepEqual(residualRefs(fixtureRoot).sort(), ['docs/stale.md', 'public/app.js'], 'non-release docs and frontend still flagged');
} finally { fs.rmSync(fixtureRoot, { recursive: true, force: true }); }

console.log('regions map unit tests passed');
