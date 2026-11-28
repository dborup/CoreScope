/* Port of upstream 2323: observer detail responses stay tied to the current
 * view. Real observer-detail.js runs in a vm with explicitly ordered API
 * completions. */
'use strict';
const assert = require('assert');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

function harness() {
  const elements = new Map();
  const requests = [];
  const charts = [];
  let page;
  function element(id) {
    const el = { textContent: '', firstChild: { textContent: '' }, listeners: {}, value: '',
      addEventListener(name, fn) { this.listeners[name] = fn; }, setAttribute() {}, replaceWith() {} };
    let html = '';
    Object.defineProperty(el, 'innerHTML', {
      get() { return html; },
      set(value) {
        html = value;
        el.firstChild = { textContent: '' };
        for (const match of value.matchAll(/id="([^"]+)"/g)) elements.set(match[1], element(match[1]));
      },
    });
    return el;
  }
  const app = element('app');
  class Chart {
    static defaults = {};
    constructor(canvas, config) { if (Chart.failAt != null && charts.length >= Chart.failAt) throw new Error('chart construction failed'); this.config = config; this.destroyed = false; charts.push(this); }
    destroy() { this.destroyed = true; }
  }
  const context = vm.createContext({
    window: { ObserverAirtime: { lookup: () => null, statCards: () => '' } },
    document: { documentElement: {}, getElementById: id => elements.get(id), querySelector: () => null, addEventListener() {}, removeEventListener() {},
      createElement: () => element('tmp') },
    Chart, getComputedStyle: () => ({ getPropertyValue: () => '' }),
    HEALTH_THRESHOLDS: {}, escapeHtml: s => String(s), timeAgo: () => 'now',
    formatSkew: s => s + ' seconds', renderSkewBadge: () => '', observerSkewSeverity: () => 'ok',
    registerPage: (name, mod) => { page = mod; },
    api(url, options) {
      if (url === '/observers') return Promise.resolve({ observers: [] });
      if (url.includes('/neighbors') || url.includes('/metrics/summary')) return Promise.resolve({ neighbors: [] });
      const request = { url, options };
      requests.push(request);
      return new Promise((resolve, reject) => Object.assign(request, { resolve, reject }));
    },
  });
  vm.runInContext(fs.readFileSync(path.resolve(__dirname, 'public/observer-detail.js'), 'utf8'), context);
  function visit(id) {
    page.destroy();
    page.init(app, id);
    return requests.slice(-3);
  }
  function days(value) {
    elements.get('obsDaysSelect').listeners.change({ target: { value: String(value) } });
    return requests.slice(-3);
  }
  function snapshot() {
    return { title: elements.get('obsTitle').textContent,
      html: elements.get('obsDetailContent').innerHTML,
      error: elements.get('obsDetailContent').firstChild.textContent,
      charts: charts.filter(c => !c.destroyed) };
  }
  return { page, app, elements, requests, charts, visit, days, snapshot, Chart };
}

function complete(requests, id, label = id) {
  requests[0].resolve({ id, name: label, last_seen: null });
  requests[1].resolve({ timeline: [{ label, count: 1 }], packetTypes: { 4: 1 },
    nodesTimeline: [{ label, count: 1 }], snrDistribution: [{ range: '0 to 5', count: 1 }], recentPackets: [] });
  requests[2].resolve([{ observerID: 'other', offsetSec: 99, samples: 99 },
    { observerID: id, offsetSec: 2, samples: 2 }]);
}
// One event-loop turn after resolving promises, not a guessed delay.
const settled = () => new Promise(resolve => setImmediate(resolve));
let passed = 0, failed = 0;
async function test(name, fn) {
  try { await fn(); passed++; console.log('  ✅ ' + name); }
  catch (error) { failed++; console.log('  ❌ ' + name + ': ' + error.message.split('\n')[0]); }
}

(async () => {
  await test('current success renders four charts for the current observer', async () => {
    const h = harness();
    complete(h.visit('A'), 'A', 'Current observer');
    await settled();
    assert.strictEqual(h.snapshot().title, 'Current observer');
    assert.strictEqual(h.snapshot().charts.length, 4);
    assert.match(h.elements.get('obsClockRegion').innerHTML, /2 seconds/);
    assert.doesNotMatch(h.elements.get('obsClockRegion').innerHTML, /99 seconds/);
  });

  for (const failure of [false, true]) {
    await test('A to B ignores late A ' + (failure ? 'failure' : 'success') + ' and keeps B charts', async () => {
      const h = harness();
      const old = h.visit('A');
      complete(h.visit('B'), 'B');
      await settled();
      const before = h.snapshot();
      if (failure) old[1].reject(new Error('old failure'));
      complete(old, 'A');
      await settled();
      assert.deepStrictEqual(h.snapshot(), before, 'late A must not change B content or charts');
    });
  }

  await test('A to B to A: the old A visit does not own the second A visit', async () => {
    const h = harness();
    const old = h.visit('A');
    complete(h.visit('B'), 'B');
    await settled();
    complete(h.visit('A'), 'A', 'A new visit');
    await settled();
    const before = h.snapshot();
    complete(old, 'A', 'A old visit');
    await settled();
    assert.deepStrictEqual(h.snapshot(), before);
  });

  for (const failure of [false, true]) {
    await test('rapid day changes ignore older ' + (failure ? 'errors' : 'results'), async () => {
      const h = harness();
      const week = h.visit('A');
      const day = h.days(1);
      const threeDays = h.days(3);
      complete(threeDays, 'A', 'three days');
      await settled();
      const before = h.snapshot();
      if (failure) day[0].reject(new Error('old day failed'));
      complete(day, 'A', 'one day');
      complete(week, 'A', 'seven days');
      await settled();
      assert.deepStrictEqual(h.snapshot(), before, 'only the latest selected window may update the view');
    });
  }

  await test('stale success cannot clear the current request error', async () => {
    const h = harness();
    const old = h.visit('A');
    const current = h.visit('B');
    current[0].reject(new Error('current error'));
    complete(current, 'B');
    await settled();
    const before = h.snapshot();
    assert.strictEqual(before.error, 'Error: current error');
    complete(old, 'A');
    await settled();
    assert.deepStrictEqual(h.snapshot(), before, 'current error must remain visible');
  });

  for (const failure of [false, true]) {
    await test('destroy invalidates pending ' + (failure ? 'failure' : 'success'), async () => {
      const h = harness();
      complete(h.visit('A'), 'A');
      await settled();
      const pending = h.days(30);
      h.page.destroy();
      const before = h.snapshot();
      if (failure) pending[0].reject(new Error('disposed failure'));
      complete(pending, 'A', 'disposed');
      await settled();
      assert.deepStrictEqual(h.snapshot(), before, 'destroyed page must receive no further writes');
    });
  }

  await test('each new visit requests the displayed seven-day default', async () => {
    const h = harness();
    complete(h.visit('A'), 'A');
    await settled();
    complete(h.days(30), 'A');
    await settled();
    const next = h.visit('B');
    assert.match(h.app.innerHTML, /value="7" selected/);
    assert.strictEqual(next[1].url, '/observers/B/analytics?days=7');
    complete(next, 'B');
    await settled();
  });

  // #634 (upstream 2339): metadata renders as soon as the observer request
  // finishes; analytics, clock skew and neighbors fill their own regions.
  const OBS = id => ({ id, name: 'Obs ' + id, last_seen: null });
  const ANALYTICS = { timeline: [{ label: 'x', count: 1 }], packetTypes: { 4: 1 },
    nodesTimeline: [{ label: 'x', count: 1 }], snrDistribution: [{ range: '0 to 5', count: 1 }], recentPackets: [] };

  await test('metadata renders before analytics finish; charts follow', async () => {
    const h = harness();
    const r = h.visit('A');
    r[0].resolve(OBS('A'));
    await settled();
    assert.strictEqual(h.snapshot().title, 'Obs A');
    assert.match(h.snapshot().html, /ID: A/);
    assert.strictEqual(h.snapshot().charts.length, 0, 'no charts before analytics');
    r[1].resolve(ANALYTICS);
    await settled();
    assert.strictEqual(h.snapshot().charts.length, 4);
  });

  await test('analytics arriving before the observer waits for the metadata shell', async () => {
    const h = harness();
    const r = h.visit('A');
    r[1].resolve(ANALYTICS);
    await settled();
    assert.strictEqual(h.snapshot().charts.length, 0);
    r[0].resolve(OBS('A'));
    await settled();
    assert.strictEqual(h.snapshot().charts.length, 4);
    assert.match(h.snapshot().html, /ID: A/);
  });

  await test('analytics failure keeps the metadata and reports in the charts region', async () => {
    const h = harness();
    const r = h.visit('A');
    r[0].resolve(OBS('A'));
    r[1].reject(new Error('analytics down'));
    await settled();
    assert.match(h.snapshot().html, /ID: A/);
    assert.strictEqual(h.snapshot().error, '', 'whole view must not become an error');
    assert.match(h.elements.get('obsChartsRegion').firstChild.textContent, /analytics down/);
  });

  await test('clock skew fills its own region without touching the metadata node', async () => {
    const h = harness();
    const r = h.visit('A');
    r[0].resolve(OBS('A'));
    await settled();
    const content = h.elements.get('obsDetailContent');
    const before = content.innerHTML;
    r[2].resolve([{ observerID: 'A', offsetSec: 2, samples: 2 }]);
    await settled();
    assert.strictEqual(h.elements.get('obsDetailContent'), content);
    assert.strictEqual(content.innerHTML, before, 'metadata markup must not be rewritten');
    assert.match(h.elements.get('obsClockRegion').innerHTML, /2 seconds/);
  });

  await test('identity failure still shows the error view', async () => {
    const h = harness();
    const r = h.visit('A');
    r[1].resolve(ANALYTICS);
    r[0].reject(new Error('no such observer'));
    await settled();
    assert.strictEqual(h.snapshot().error, 'Error: no such observer');
    assert.strictEqual(h.snapshot().charts.length, 0);
  });

  await test('late secondary replies of an old visit cannot write to the new visit', async () => {
    const h = harness();
    const old = h.visit('A');
    old[0].resolve(OBS('A'));
    await settled();
    const next = h.visit('B');
    next[0].resolve(OBS('B'));
    await settled();
    const before = h.snapshot();
    old[1].resolve(ANALYTICS);
    old[2].resolve([{ observerID: 'A', offsetSec: 99, samples: 99 }]);
    await settled();
    assert.deepStrictEqual(h.snapshot(), before);
    assert.doesNotMatch(h.elements.get('obsClockRegion').innerHTML, /99 seconds/);
  });

  await test('a chart that fails to construct leaves no half-built charts and keeps the metadata', async () => {
    const h = harness();
    h.Chart.failAt = 2;
    const r = h.visit('A');
    r[0].resolve(OBS('A'));
    r[1].resolve(ANALYTICS);
    await settled();
    assert.strictEqual(h.snapshot().charts.length, 0, 'partial charts must be destroyed');
    assert.match(h.snapshot().html, /ID: A/);
    assert.match(h.elements.get('obsChartsRegion').firstChild.textContent, /chart construction failed/);
  });

  console.log(`Observer detail ownership: ${passed} passed, ${failed} failed`);
  process.exitCode = failed ? 1 : 0;
})();
