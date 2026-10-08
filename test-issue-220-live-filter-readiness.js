'use strict';
const assert = require('assert');
const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(__dirname + '/public/live.js', 'utf8');

(async () => {
  const input = src.match(/<input[^>]*id="liveNodeFilterInput"[^>]*>/)[0];
  assert(/\sdisabled(?:\s|>)/.test(input), 'node filter must render disabled before asynchronous initialization');
  assert(input.includes('aria-busy="true"'), 'loading state must be accessible');
  const wiring = src.slice(src.indexOf('// Node filter input'), src.indexOf('// Geo filter overlay'));
  assert(wiring.indexOf('nodeFilterInput.disabled = false') > wiring.lastIndexOf("addEventListener('click'"),
    'enable the input only after all input and clear handlers are installed');

  let releaseConfig;
  const configCtx = {
    liveGeneration: 0, window: {}, buildLegendHtml: () => '', wireLiveControls() {},
    fetch: () => new Promise(resolve => { releaseConfig = resolve; }),
    location: { hash: '#/live' }, continued: false,
  };
  vm.createContext(configCtx);
  // Stop immediately before Leaflet construction; everything before that is
  // the actual initialization code, including its first asynchronous boundary.
  const initStart = src.indexOf('  async function init(app)');
  const mapStart = src.indexOf("    map = L.map('liveMap'", initStart);
  vm.runInContext(src.slice(initStart, mapStart) + 'continued = true; }', configCtx);
  const oldInit = configCtx.init({ innerHTML: '' });
  configCtx.liveGeneration++;
  releaseConfig({ json: async () => ({ center: [1, 1], zoom: 9 }) });
  await oldInit;
  assert.strictEqual(configCtx.continued, false, 'stale config response must not construct a map');

  // Execute the real loader with a deferred response. A departed mount must
  // not populate a new mount's shared node state or start background timers.
  const start = src.indexOf('  async function loadNodes(');
  const end = src.indexOf('  let _affinityInterval', start);
  let release;
  let updates = 0;
  const ctx = {
    liveGeneration: 1, AreaFilter: { areaQueryString: () => '' }, window: {},
    nodesLayer: { clearLayers() {} }, nodeMarkers: {}, nodeData: {},
    fetchAllNodes: () => new Promise(resolve => { release = resolve; }),
    document: { getElementById() { updates++; return null; } },
    addNodeMarker() { updates++; }, fetchAffinityData() { updates++; },
    startAffinityRefresh() { updates++; }, console, Date,
  };
  vm.createContext(ctx);
  vm.runInContext(src.slice(start, end), ctx);
  const pending = ctx.loadNodes();
  ctx.liveGeneration++;
  release({ nodes: [{ public_key: 'new', lat: 1, lon: 1 }] });
  await pending;
  assert.strictEqual(updates, 0, 'stale node response must not touch DOM, markers or timers');
  assert.deepStrictEqual(Object.keys(ctx.nodeData), []);
  console.log('Live filter readiness: markup, wiring and stale loader guards passed');
})().catch(err => { console.error(err); process.exitCode = 1; });
