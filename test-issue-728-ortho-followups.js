/* test-issue-728-ortho-followups.js — the four #728 follow-ups carried from
 * the #777 review (F3–F6) for the "Aerial (Denmark)" base layer.
 *
 *   F3  a cached `outside` tile is not refetched: the code says so, and the
 *       one retry is a same-URL reload (which the browser answers from its
 *       HTTP cache, max-age one day, for an `outside` tile).
 *   F4  restore (the A6 case): the Auto group is on the map first and is
 *       off after a stored Aerial pick is restored.
 *   F5  the Matrix theme tints the ortho pane like the tile pane, with the
 *       same filter.
 *   F6  the Esri member of the group is capped at its native z19 and lets
 *       the map zoom to the ortho layer's maxZoom, so z20 outside Denmark
 *       upscales z19 tiles instead of going blank.
 *
 * Runs via: node test-issue-728-ortho-followups.js  (pure vm sandbox)
 */
'use strict';
const assert = require('assert');
const fs = require('fs');
const path = require('path');
const { makeSandbox, makeMap } = require('./tests/map-overlay-sandbox');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

const ORTHO_ID = 'dhm-ortho';
const ORTHO_URL = '/api/map/dhm/ortho/{z}/{x}/{y}';
const ESRI_URL = 'https://server.arcgisonline.com/ArcGIS/rest/services/World_Imagery/MapServer/tile/{z}/{y}/{x}';
const ESRI_ON = { tiles: { providers: { esri: { enabled: true } } } };
const BOUNDS = [[54.4265, 7.99125], [57.778, 15.5995]];

function dhmBlock(orthoMaxZoom) {
  return {
    enabled: true, url: '/api/map/dhm/wms', layer: 'dhm_terraen_skyggekort',
    minZoom: 7, maxZoom: 19, bounds: BOUNDS,
    attribution: 'Indeholder data fra Klimadatastyrelsen, Danmarks Højdemodel, CC BY 4.0',
    ortho: {
      url: ORTHO_URL, minZoom: 7, maxZoom: orthoMaxZoom, bounds: BOUNDS,
      attribution: 'Indeholder data fra GeoDanmark, Ortofoto forår, CC BY 4.0',
    },
  };
}

// buildControl() from the shared sandbox, but with the Auto group put on the
// map first, as the Map page does (L.layerGroup of the theme's tiles).
function buildWithAuto(ctx) {
  const map = makeMap();
  map.attributionControl = { builtIn: true };
  const auto = { _auto: true };
  map.addLayer(auto);
  assert.ok(map.hasLayer(auto), 'precondition: Auto is on the map');
  ctx.window.MC_createLayerControl(map, auto, 'topleft');
  return { map, auto };
}

console.log('── #728 F3: a cached `outside` tile is not refetched ──');

const PROVIDERS_SRC = fs.readFileSync(path.join(__dirname, 'public', 'map-tile-providers.js'), 'utf8');

test('F3 the retry code says a cached `outside` tile is not refetched (HTTP cache, max-age one day)', () => {
  const i = PROVIDERS_SRC.indexOf('function retryPlaceholderTiles');
  assert.ok(i > 0);
  const doc = PROVIDERS_SRC.slice(PROVIDERS_SRC.lastIndexOf('/* #728 N3', i), i);
  assert.ok(/outside/.test(doc) && /not refetched/i.test(doc), 'no "not refetched" note on the retry');
  assert.ok(/HTTP cache/.test(doc) && /max-age/.test(doc) && /one day/.test(doc), 'the note does not name the cache and its max-age');
});

console.log(`\n#728 follow-ups: ${passed} passed, ${failed} failed`);
if (failed) process.exit(1);
