'use strict';

// Controlled promises exercise the shipped pages, not copies of their loaders.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const flush = async () => { for (let i = 0; i < 12; i++) await Promise.resolve(); };
let passed = 0, failed = 0;
async function test(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (error) { failed++; console.error('  ✗ ' + name + ': ' + error.message); }
}
function deferred() {
  let resolve, reject;
  const promise = new Promise((a, b) => { resolve = a; reject = b; });
  return { promise, resolve, reject };
}
function element(id) {
  const listeners = new Map();
  let html = '', buttons = [];
  return {
    id, value: '', textContent: '', style: {}, dataset: {},
    classList: { add() {}, remove() {}, contains: () => false },
    set innerHTML(value) {
      html = String(value);
      buttons = [...html.matchAll(/<button data-days="(\d+)"/g)].map(match => {
        const button = element('day-' + match[1]); button.dataset.days = match[1]; return button;
      });
    },
    get innerHTML() { return html; },
    addEventListener(type, fn) { listeners.set(type, fn); },
    removeEventListener(type) { listeners.delete(type); },
    dispatch(type, event = {}) { return listeners.get(type)?.(event); },
    querySelectorAll(selector) { return selector === '#timeRangeBtns button' ? buttons : []; },
    querySelector: () => null, setAttribute() {}, appendChild() {},
  };
}
function nodeData(name) {
  return {
    node: { name, public_key: 'ab'.repeat(32), role: 'repeater' },
    computedStats: {}, peerInteractions: [], activityTimeline: [], snrTrend: [],
    packetTypeBreakdown: [], observerCoverage: [], hopDistribution: [], uptimeHeatmap: [],
  };
}
function nodeEnvironment() {
  const app = element('app'), requests = [], elements = {};
  ['batteryStatusBadge', 'batteryEmpty', 'hopAnalyticsEmpty', 'hopAnalyticsSummary'].forEach(id => { elements[id] = element(id); });
  let page;
  const context = {
    console, Date, Math, JSON, Set, Map, Promise, URLSearchParams,
    document: { documentElement: {}, getElementById: id => elements[id] || null },
    getComputedStyle: () => ({ getPropertyValue: () => '' }),
    Chart: Object.assign(function () {}, { defaults: {} }),
    CLIENT_TTL: {}, escapeHtml: value => String(value).replace(/</g, '&lt;'),
    timeAgo: () => '', registerPage: (_name, p) => { page = p; },
    api: path => { const request = { path, ...deferred() }; requests.push(request); return request.promise; },
  };
  context.window = context;
  vm.runInNewContext(fs.readFileSync('public/node-analytics.js', 'utf8'), context);
  return { app, page, requests, elements, context, start: () => page.init(app, 'ab'.repeat(32) + '/analytics') };
}
function filter() {
  const listeners = new Set();
  return { init() {}, onChange(fn) { listeners.add(fn); return fn; }, offChange(fn) { listeners.delete(fn); },
    regionQueryString: () => '', areaQueryString: () => '', fire() { for (const fn of [...listeners]) fn(); }, listeners };
}
function analyticsEnvironment() {
  const app = element('app'), elements = {}, requests = [], RegionFilter = filter(), AreaFilter = filter();
  const el = id => elements[id] || (elements[id] = element(id));
  const windowListeners = new Map();
  let page;
  const context = {
    console, Date, Math, JSON, Set, Map, Promise, URLSearchParams, location: { hash: '#/analytics' },
    document: { documentElement: {}, getElementById: el, querySelectorAll: () => [], querySelector: () => null, createElement: () => element('') },
    getComputedStyle: () => ({ getPropertyValue: () => '' }),
    addEventListener: (type, fn) => windowListeners.set(type, fn), removeEventListener: type => windowListeners.delete(type),
    requestAnimationFrame() {}, cancelAnimationFrame() {}, setTimeout() {}, clearTimeout() {}, setInterval() {}, clearInterval() {},
    localStorage: { getItem: () => null, setItem() {} }, CLIENT_TTL: {}, RegionFilter, AreaFilter, initTabBar() {},
    registerPage: (_name, p) => { page = p; },
    api: path => { const request = { path, ...deferred() }; requests.push(request); return request.promise; },
    fetchAllNodes: () => new Promise(() => {}),
  };
  context.window = context;
  vm.runInNewContext(fs.readFileSync('public/analytics.js', 'utf8'), context);
  return { page, app, requests, el, RegionFilter, AreaFilter, context, windowListeners };
}

(async () => {
  for (const rejects of [false, true]) {
    await test('node analytics drops late ' + (rejects ? 'error' : 'success') + ' after leaving', async () => {
      const env = nodeEnvironment(); env.start(); env.page.destroy(); env.app.innerHTML = 'Nodes list';
      if (rejects) env.requests[0].reject(new Error('old request')); else env.requests[0].resolve(nodeData('Old node'));
      await flush();
      assert.equal(env.app.innerHTML, 'Nodes list');
      assert.equal(env.requests.length, 1, 'a stale result must not start child requests');
    });
  }
  await test('same-node re-entry does not revive an earlier visit', async () => {
    const env = nodeEnvironment(); env.start(); env.page.destroy(); env.start();
    env.requests[1].resolve(nodeData('Current visit')); await flush();
    env.requests[0].resolve(nodeData('Earlier visit')); await flush();
    assert.match(env.app.innerHTML, /Current visit/); assert.doesNotMatch(env.app.innerHTML, /Earlier visit/);
  });
  await test('latest time range wins when the previous range arrives last', async () => {
    const env = nodeEnvironment(); env.start(); env.requests[0].resolve(nodeData('Initial')); await flush();
    const buttons = env.app.querySelectorAll('#timeRangeBtns button');
    buttons.find(button => button.dataset.days === '1').dispatch('click');
    buttons.find(button => button.dataset.days === '30').dispatch('click');
    env.requests.find(request => /analytics\?days=30$/.test(request.path)).resolve(nodeData('Thirty days')); await flush();
    env.requests.find(request => /analytics\?days=1$/.test(request.path)).resolve(nodeData('One day')); await flush();
    assert.match(env.app.innerHTML, /Thirty days/); assert.doesNotMatch(env.app.innerHTML, /One day/);
  });
  for (const child of ['battery', 'hop_analytics']) {
    for (const rejects of [false, true]) {
      await test(child + ' drops a late ' + (rejects ? 'error' : 'success') + ' after destroy', async () => {
        const env = nodeEnvironment(); env.start(); env.requests[0].resolve(nodeData('Current')); await flush();
        const request = env.requests.find(item => item.path.includes('/' + child + '?'));
        assert.ok(request, 'child request must actually run');
        env.page.destroy();
        Object.values(env.elements).forEach(item => { item.innerHTML = 'New page'; item.textContent = 'New page'; });
        if (rejects) request.reject(new Error('earlier child')); else request.resolve({ samples: [], packets: [] });
        await flush();
        for (const item of Object.values(env.elements)) {
          assert.equal(item.innerHTML, 'New page'); assert.equal(item.textContent, 'New page');
        }
      });
    }
  }
  await test('current node success and current errors still render', async () => {
    const env = nodeEnvironment(); env.start(); env.requests[0].resolve(nodeData('Current node')); await flush();
    assert.match(env.app.innerHTML, /Current node/); assert.equal(env.requests.length, 3);
    env.page.destroy(); env.start(); env.requests[3].reject(new Error('current failure')); await flush();
    assert.match(env.app.innerHTML, /current failure/);
  });
  await test('Analytics unregisters both filters, remains inactive after destroy, and remounts once', async () => {
    const env = analyticsEnvironment(); await env.page.init(env.app); assert.equal(env.requests.length, 6);
    assert.equal(env.RegionFilter.listeners.size, 1); assert.equal(env.AreaFilter.listeners.size, 1);
    env.page.destroy();
    assert.equal(env.RegionFilter.listeners.size, 0); assert.equal(env.AreaFilter.listeners.size, 0);
    env.RegionFilter.fire(); env.AreaFilter.fire(); assert.equal(env.requests.length, 6);
    await env.page.init(env.app); env.RegionFilter.fire();
    assert.equal(env.requests.length, 18, 'one mount and one filter change, not leaked callbacks');
    env.page.destroy();
  });
  await test('a self-loading deep-linked tab starts before bulk answers land', async () => {
    const env = analyticsEnvironment(); env.context.location.hash = '#/analytics?tab=prefix-tool';
    // The tab buttons are actual known buttons, not a selector derived from URL text.
    env.el('analyticsTabs').querySelectorAll = () => [
      { dataset: { tab: 'overview' }, classList: { add() {}, remove() {} } },
      { dataset: { tab: 'prefix-tool' }, classList: { add() {}, remove() {} } },
    ];
    await env.page.init(env.app);
    assert.match(env.el('analyticsContent').innerHTML, /Loading prefix data/);
    assert.equal(env.requests.length, 7, 'one prefix request plus six bulk requests');
    const bulk = JSON.parse(fs.readFileSync('test-fixtures/analytics-tabs-172.json', 'utf8'));
    const key = { 'hash-sizes': 'hashData', rf: 'rfData', topology: 'topoData', channels: 'chanData', 'hash-collisions': 'collisionData' };
    for (const request of env.requests.slice(-6)) {
      const name = request.path.split('/').pop().split('?')[0];
      request.resolve(name === 'relay-airtime-share' ? { rows: [] } : bulk[key[name]]);
    }
    await flush();
    assert.equal(env.requests.length, 7, 'a bulk success must not start the independent tab again');
    env.AreaFilter.fire();
    assert.equal(env.requests.length, 14, 'a filter change refreshes bulk and the selected independent tab once');
    env.page.destroy();
  });
  console.log(`\n${passed} passed, ${failed} failed`);
  if (failed) process.exitCode = 1;
})().catch(error => { console.error(error); process.exitCode = 1; });
