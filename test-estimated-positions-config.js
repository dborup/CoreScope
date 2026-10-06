'use strict';

// Exercise the shared runtime policy and the actual tool renderers, not copies.
const assert = require('assert');
const fs = require('fs');
const vm = require('vm');

function sandbox(config, response, configFetch) {
  const elements = new Map();
  const element = id => {
    if (!elements.has(id)) elements.set(id, {
      innerHTML: '', textContent: '', style: {}, value: '',
      addEventListener() {}, querySelectorAll() { return []; }, querySelector() { return null; }
    });
    return elements.get(id);
  };
  const ctx = vm.createContext({
    window: {}, console, Promise, URLSearchParams,
    fetch: configFetch || (() => Promise.resolve({ json: () => Promise.resolve(config) })),
    api: () => Promise.resolve(response),
    document: { getElementById: element, querySelector: () => null },
    registerPage() {}, location: { hash: '' }
  });
  vm.runInContext(fs.readFileSync('public/roles.js', 'utf8'), ctx);
  return { ctx, element, load: file => vm.runInContext(fs.readFileSync('public/' + file, 'utf8'), ctx) };
}

async function run() {
  for (const config of [{}, { estimatedPositions: {} }, { estimatedPositions: { enabled: true } }]) {
    const { ctx } = sandbox(config);
    await ctx.window.MeshConfigReady;
    assert.strictEqual(ctx.window.EstimatedPositions.enabled(), true, 'omitted or enabled config preserves existing behavior');
    assert.strictEqual(ctx.window.EstimatedPositions.enabled({ estimatedPositionsEnabled: false }), false, 'endpoint disabled flag takes precedence');
  }
  const off = sandbox({ estimatedPositions: { enabled: false } });
  await off.ctx.window.MeshConfigReady;
  assert.strictEqual(off.ctx.window.EstimatedPositions.enabled(), false);
  assert.strictEqual(off.ctx.window.EstimatedPositions.enabled({ estimatedPositionsEnabled: true }), false, 'response cannot override operator disable');
  const unavailable = sandbox({}, {}, () => Promise.reject(new Error('offline')));
  await unavailable.ctx.window.MeshConfigReady;
  assert.strictEqual(unavailable.ctx.window.EstimatedPositions.enabled(), true, 'failed config retains legacy default; backend remains authoritative');

  for (const [file, tool, prefix] of [
    ['position-gaps.js', 'PositionGapsTool', 'position-gaps'],
    ['gps-sanity.js', 'GPSSanityTool', 'gps-sanity']
  ]) {
    for (const [config, response] of [
      [{}, { estimatedPositionsEnabled: false }],
      [{ estimatedPositions: { enabled: false } }, { nodes: [], positionGaps: [], estimatedNodes: [] }]
    ]) {
      const s = sandbox(config, response);
      await s.ctx.window.MeshConfigReady;
      s.load(file);
      s.ctx.window[tool].init({ innerHTML: '' });
      await new Promise(resolve => setTimeout(resolve, 0));
      const html = s.element(prefix + '-content').innerHTML;
      assert.ok(html.includes('disabled by the instance operator'), file + ' explains disabled state');
      assert.ok(!html.includes('No suspicious GPS') && !html.includes('No Areas'), file + ' must not claim no evidence');
      assert.strictEqual(s.element(prefix + '-status').textContent, '', file + ' must not report manufactured zero counts');
    }
    let finishConfig;
    const pending = sandbox(null, {}, () => new Promise(resolve => { finishConfig = resolve; }));
    pending.load(file);
    pending.ctx.window[tool].init({ innerHTML: '' });
    await new Promise(resolve => setTimeout(resolve, 0));
    assert.ok(pending.element(prefix + '-content').innerHTML.includes('Loading'), file + ' waits for operator policy');
    finishConfig({ json: () => Promise.resolve({ estimatedPositions: { enabled: false } }) });
    await new Promise(resolve => setTimeout(resolve, 0));
    assert.ok(pending.element(prefix + '-content').innerHTML.includes('disabled by the instance operator'), file + ' delayed false policy wins');
  }
  console.log('Estimated-position policy and disabled tool states passed.');
}

run().catch(error => { console.error(error); process.exitCode = 1; });
