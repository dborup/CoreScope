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
const NEEDLE = 'tile.openstreetmap.org';

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
  const offenders = files.filter(f => !ALLOWED.has(f) &&
    fs.readFileSync(path.join(PUBLIC, f), 'utf8').includes(NEEDLE));
  assert.deepStrictEqual(offenders, [], 'uncredited OSM tile URL in: ' + offenders.join(', '));
});

console.log(`\n${passed} passed, ${failed} failed`);
process.exit(failed ? 1 : 0);
