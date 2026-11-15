/* test-issue-764-no-osm-fallback.js — #764.
 * No public/*.js file may draw tile.openstreetmap.org tiles except the
 * provider registry (map-tile-providers.js), which is the one place that
 * pairs a tile URL with its attribution. A stray L.tileLayer('…openstreetmap…')
 * elsewhere is an uncredited OSM use (the insets run with attributionControl:false).
 */
'use strict';
const fs = require('fs');
const path = require('path');
const assert = require('assert');

const PUBLIC = path.join(__dirname, 'public');
const ALLOWED = new Set(['map-tile-providers.js']);
// Remaining OSM literals that are NOT uncredited fallbacks. Each is pinned by
// occurrence count so a new one in the same file still fails the test.
const CREDITED = {
  'roles.js': { count: 2, why: 'legacy TILE_DARK/TILE_LIGHT config defaults (#390)' },
  'nodes.js': { count: 1, why: 'getTileUrl() last resort in the helper, paired with an OSM credit' },
  'home.js': { count: 1, why: 'minimap draws its own OSM credit (#756)' },
  'regions-map.js': { count: 1, why: 'registry-less fallback, paired with an OSM credit' },
};
const NEEDLE = 'tile.openstreetmap.org';
// Count the needle only inside a string literal on one line, so a comment
// or doc line naming the domain is not mistaken for a tile URL (#767 review).
const LITERAL = /(['"`])[^'"`\n]*tile\.openstreetmap\.org[^'"`\n]*\1/g;
function literalCount(src) { return (src.match(LITERAL) || []).length; }

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

const files = fs.readdirSync(PUBLIC).filter(f => f.endsWith('.js'));

test('scans a non-trivial set of public/*.js files and the registry is among them', () => {
  assert.ok(files.length > 10, 'only ' + files.length + ' files found');
  assert.ok(files.includes('map-tile-providers.js'));
  assert.ok(fs.readFileSync(path.join(PUBLIC, 'map-tile-providers.js'), 'utf8').includes(NEEDLE),
    'registry no longer holds the OSM URL — the allow-list is stale');
});

test('no public/*.js outside the provider registry contains ' + NEEDLE, () => {
  const offenders = files.filter(f => {
    if (ALLOWED.has(f)) return false;
    const n = literalCount(fs.readFileSync(path.join(PUBLIC, f), 'utf8'));
    return n > ((CREDITED[f] && CREDITED[f].count) || 0);
  });
  assert.deepStrictEqual(offenders, [], 'uncredited OSM tile URL in: ' + offenders.join(', '));
});

test('a comment naming the domain is not counted; a string literal is', () => {
  assert.strictEqual(literalCount('// see tile.openstreetmap.org for tiles\n/* tile.openstreetmap.org */'), 0);
  assert.strictEqual(literalCount("L.tileLayer('https://tile.openstreetmap.org/{z}/{x}/{y}.png')"), 1);
  assert.strictEqual(literalCount('var u = "https://tile.openstreetmap.org/{z}/{x}/{y}.png";'), 1);
});

test('the pinned counts match the literals in each credited file exactly', () => {
  for (const f of Object.keys(CREDITED)) {
    const n = literalCount(fs.readFileSync(path.join(PUBLIC, f), 'utf8'));
    assert.strictEqual(n, CREDITED[f].count, f + ': ' + n + ' literals, pinned ' + CREDITED[f].count + ' (' + CREDITED[f].why + ')');
  }
});

console.log(`\n${passed} passed, ${failed} failed`);
process.exit(failed ? 1 : 0);
