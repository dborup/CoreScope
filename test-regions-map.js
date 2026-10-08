'use strict';

const assert = require('node:assert/strict');
const regions = require('./public/regions-map.js');

const sample = [
  { public_key: 'a', name: 'A', role: 'repeater', lat: 55, lon: 10, transported_scopes_recent: ['#dk'] },
  { public_key: 'b', name: 'B', role: 'room', lat: 55, lon: 11, transported_scopes_recent: ['#dk'] },
  { public_key: 'c', name: 'C', role: 'repeater', lat: 56, lon: 10, transported_scopes_recent: ['#dk'] },
  { public_key: 'd', name: 'D', role: 'companion', lat: 57, lon: 10, transported_scopes_recent: ['#dk'] },
  { public_key: 'e', name: 'E', role: 'repeater', lat: 0, lon: 0, transported_scopes_recent: ['#dk'] },
  { public_key: 'f', name: 'F', role: 'repeater', lat: 55, lon: 10, transported_scopes: ['#old'] },
];

assert.deepEqual(regions.collectScopePoints(sample).map(p => p.publicKey), ['a', 'b', 'c']);
assert.deepEqual(regions.listScopes(sample), ['#dk']);
assert.equal(regions.validScope('#DK$'), true);
assert.equal(regions.validScope('#ø'), true);
assert.equal(regions.validScope('#bad:scope'), false);
assert.equal(regions.validScope('#bad<scope'), false);
const manyScopes = Array.from({ length: 129 }, (_, i) => ({ ...sample[0], public_key: 'p' + i, transported_scopes_recent: ['#r' + i] }));
assert.equal(regions.scopeCatalog(manyScopes).truncated, true);
assert.equal(regions.scopeCatalog(manyScopes).scopes.length, 128);
assert.deepEqual(regions.convexHull([[10, 55], [11, 55], [10, 56], [10.2, 55.2]]), [[10, 55], [11, 55], [10, 56]]);
assert.deepEqual(regions.convexHull([[10, 55], [11, 55]]), []);
assert.equal(regions.parseSelectedScope('#/regions?scope=%23dk'), '#dk');
assert.equal(regions.parseSelectedScope('#/regions?scope=%3Cbad%3E'), '');
assert.equal(regions.scopeHash('#dk'), '#/regions?scope=%23dk');

console.log('regions map unit tests passed');
