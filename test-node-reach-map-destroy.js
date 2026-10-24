'use strict';
// #530 F1: NodeReachMap.destroy() must cancel the deferred invalidateSize()
// timer, otherwise it fires on a removed Leaflet map (TypeError ... _leaflet_pos).
// Loads the real public/node-reach-map.js with a fake Leaflet and fake timers.
const assert = require('assert');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

function mk() {
  const timers = new Map(); let nextId = 1;
  const log = { invalidated: 0, removed: false, cleared: [] };
  const map = {
    setView() { return map; }, fitBounds() {}, addLayer() {},
    invalidateSize() {
      if (log.removed) throw new TypeError("Cannot read properties of undefined (reading '_leaflet_pos')");
      log.invalidated++;
    },
    remove() { log.removed = true; },
  };
  const chain = { addTo() { return chain; }, bindPopup() { return chain; }, bindTooltip() { return chain; } };
  const sandbox = {
    window: {}, console: console, escapeHtml: String,
    document: { getElementById: () => ({}), documentElement: {} },
    getComputedStyle: () => ({ getPropertyValue: () => '#123' }),
    L: {
      map: () => map, tileLayer: () => chain, circleMarker: () => chain, polyline: () => chain,
      layerGroup: () => ({ addTo() { return { clearLayers() {} }; } }),
      control: () => chain, DomUtil: { create: () => ({}) },
    },
    setTimeout: (fn) => { const id = nextId++; timers.set(id, fn); return id; },
    clearTimeout: (id) => { log.cleared.push(id); timers.delete(id); },
  };
  vm.createContext(sandbox);
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'public', 'node-reach-map.js'), 'utf8'), sandbox);
  return { sandbox, timers, log, NRM: sandbox.window.NodeReachMap };
}

const tiers = [{ min: 0, label: 'x', legend: 'x', varName: '--a' }];
const node = { lat: 1, lon: 2, name: 'n' };

{ // destroy() before the deferred invalidateSize fires: nothing may run on the removed map
  const { timers, log, NRM } = mk();
  const ctl = NRM.render('m', node, tiers);
  assert.strictEqual(timers.size, 1, 'render schedules the deferred invalidateSize');
  ctl.destroy();
  assert.strictEqual(timers.size, 0, 'destroy() cancels the pending invalidateSize timer');
  assert.ok(log.removed, 'map removed');
  for (const fn of Array.from(timers.values())) fn(); // none left; would throw if one were
  assert.strictEqual(log.invalidated, 0, 'invalidateSize never ran on the removed map');
}
{ // a timer that does fire after destroy (e.g. already dequeued) is guarded
  const { timers, log, NRM } = mk();
  const ctl = NRM.render('m', node, tiers);
  const fn = Array.from(timers.values())[0];
  ctl.destroy();
  assert.doesNotThrow(fn, 'late timer callback is a no-op after destroy');
  assert.strictEqual(log.invalidated, 0);
}
{ // normal path still invalidates, and double destroy is safe
  const { timers, log, NRM } = mk();
  const ctl = NRM.render('m', node, tiers);
  Array.from(timers.values())[0]();
  assert.strictEqual(log.invalidated, 1, 'invalidateSize runs when the map is alive');
  ctl.destroy(); ctl.destroy();
}
console.log('test-node-reach-map-destroy.js OK');
