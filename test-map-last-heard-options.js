/* test-map-last-heard-options.js
 * #662 — guard: every <option value> of the Map "Last Heard" select
 * (public/map.js #mcLastHeard) is a key the server's GetNodes accepts
 * (cmd/server/last_heard.go lastHeardWindows), and the menu matches the
 * issue's list. Reads the real source files; no DOM.
 */
'use strict';
const fs = require('fs');
const path = require('path');
const assert = require('assert');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

const mapSrc = fs.readFileSync(path.join(__dirname, 'public', 'map.js'), 'utf8');
const goSrc = fs.readFileSync(path.join(__dirname, 'cmd', 'server', 'last_heard.go'), 'utf8');

const sel = mapSrc.match(/<select id="mcLastHeard"[^>]*>([\s\S]*?)<\/select>/);
const optionValues = sel ? Array.from(sel[1].matchAll(/<option value="([^"]*)"/g), m => m[1]) : [];
const tableBody = goSrc.match(/var lastHeardWindows = \[\]lastHeardWindow\{([\s\S]*?)\n\}/);
const serverKeys = tableBody ? Array.from(tableBody[1].matchAll(/\{"([^"]+)",/g), m => m[1]) : [];

console.log('\n=== map last-heard options vs server keys ===');

test('found the Map select and the server table', () => {
  assert.ok(optionValues.length > 0, 'no #mcLastHeard options found');
  assert.ok(serverKeys.length > 0, 'no lastHeardWindows keys found');
});

test('every Map option value is a key the server accepts', () => {
  for (const v of optionValues) assert.ok(serverKeys.includes(v), 'server does not accept "' + v + '"');
});

test('Map menu is 1h, 3h, 6h, 12h, 24h, 2d-6d, 7d, 30d', () => {
  assert.deepStrictEqual(optionValues,
    ['1h', '3h', '6h', '12h', '24h', '2d', '3d', '4d', '5d', '6d', '7d', '30d']);
});

test('Map default stays 30d', () => {
  assert.ok(/lastHeard: '30d'/.test(mapSrc));
  assert.ok(/<option value="30d" selected>/.test(mapSrc));
});

console.log('\n' + passed + ' passed, ' + failed + ' failed');
process.exit(failed ? 1 : 0);
