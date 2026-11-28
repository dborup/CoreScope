'use strict';
// #562 — battery badge is built with explicit DOM; voltage stays a text node.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const SRC = process.env.NODE_ANALYTICS_SRC || 'public/node-analytics.js';
let passed = 0, failed = 0;
async function test(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
const flush = async () => { for (let i = 0; i < 12; i++) await Promise.resolve(); };
function node(tag, text) {
  const n = { tag, nodeType: text === undefined ? 1 : 3, text, children: [], attrs: {}, style: {}, className: '',
    setAttribute(k, v) { this.attrs[k] = String(v); }, appendChild(c) { this.children.push(c); return c; },
    replaceChildren(...c) { this.children = c; } };
  Object.defineProperty(n, 'textContent', {
    get() { return n.nodeType === 3 ? n.text : n.children.map(c => c.textContent).join(''); },
    set(v) { n.children = [node('#text', String(v))]; } });
  return n;
}
function walk(n, out = []) { out.push(n); (n.children || []).forEach(c => walk(c, out)); return out; }
function setup() {
  const badge = node('span'), empty = node('div');
  const els = { batteryStatusBadge: badge, batteryEmpty: empty };
  let page, pending = [];
  const app = { innerHTML: '', querySelectorAll: () => [], querySelector: () => null, setAttribute() {}, addEventListener() {}, removeEventListener() {} };
  const context = {
    console, Date, Math, JSON, Set, Map, Promise, URLSearchParams, Error,
    document: { documentElement: {}, getElementById: id => els[id] || null,
      createElement: t => node(t), createElementNS: (_ns, t) => node(t), createTextNode: t => node('#text', String(t)) },
    getComputedStyle: () => ({ getPropertyValue: () => '' }),
    Chart: Object.assign(function () {}, { defaults: {} }), CLIENT_TTL: {},
    escapeHtml: v => String(v), timeAgo: () => '', registerPage: (_n, p) => { page = p; },
    api: path => new Promise((resolve, reject) => pending.push({ path, resolve, reject })),
  };
  context.window = context;
  vm.runInNewContext(fs.readFileSync(SRC, 'utf8'), context);
  return { badge, page, app, pending, start: () => page.init(app, 'ab'.repeat(32) + '/analytics') };
}
const analytics = { node: { name: 'N', public_key: 'ab'.repeat(32), role: 'repeater' }, computedStats: {}, peerInteractions: [],
  activityTimeline: [], snrTrend: [], packetTypeBreakdown: [], observerCoverage: [], hopDistribution: [], uptimeHeatmap: [] };
async function render(battery) {
  const env = setup(); env.start(); env.pending[0].resolve(analytics); await flush();
  const req = env.pending.find(r => r.path.includes('/battery?'));
  if (battery instanceof Error) req.reject(battery); else req.resolve(battery);
  await flush(); return env;
}
(async () => {
  for (const [status, cls, icon, label] of [['ok', 'status-ok', 'ph-battery-high', 'OK'], ['low', 'status-warn', 'ph-warning', 'Low'], ['critical', 'status-err', 'ph-battery-low', 'Critical']]) {
    await test(status + ': real SVG icon element, label text, themed class, no SVG source as text', async () => {
      const { badge } = await render({ status, latest_mv: 3456, samples: [] });
      const all = walk(badge);
      const svg = all.find(n => n.tag === 'svg'); assert.ok(svg, 'svg element exists');
      assert.equal(svg.attrs['aria-hidden'], 'true'); assert.equal(svg.attrs.class, 'ph-icon');
      assert.equal(all.find(n => n.tag === 'use').attrs.href, '/icons/phosphor-sprite.svg#' + icon);
      assert.match(badge.className, new RegExp(cls));
      assert.equal(badge.style.color, '', 'no hardcoded inline colour');
      assert.ok(!/[<>]|svg/.test(badge.textContent), 'no markup in text: ' + badge.textContent);
      assert.equal(badge.textContent, ' ' + label + ' · 3456 mV');
      assert.ok(all.some(n => n.nodeType === 3 && n.text === ' · 3456 mV'), 'voltage is a text node');
    });
  }
  await test('unknown / no data: text only, muted class', async () => {
    for (const d of [{ status: 'unknown', samples: [] }, {}, null]) {
      const { badge } = await render(d);
      assert.equal(walk(badge).filter(n => n.tag === 'svg').length, 0);
      assert.equal(badge.textContent, 'No data'); assert.match(badge.className, /status-muted/);
    }
  });
  await test('hostile voltage renders as harmless text only', async () => {
    const evil = '<img src=x onerror=alert(1)><script>alert(2)</script>';
    const { badge } = await render({ status: 'ok', latest_mv: evil, samples: [] });
    const all = walk(badge);
    assert.deepEqual(all.filter(n => n.nodeType === 1).map(n => n.tag).sort(), ['span', 'span', 'svg', 'use']);
    assert.ok(!all.some(n => ['img', 'script'].includes(n.tag)), 'no injected element');
    assert.ok(all.every(n => !Object.keys(n.attrs).some(k => /^on/i.test(k))), 'no event handler');
    assert.ok(badge.textContent.includes(evil), 'payload visible only as text');
  });
  await test('request failure keeps honest unavailable state; badge untouched', async () => {
    const env = setup(); env.start(); env.pending[0].resolve(analytics); await flush();
    env.pending.find(r => r.path.includes('/battery?')).reject(new Error('boom')); await flush();
    assert.equal(env.badge.children.length, 0);
  });
  await test('stale battery response after destroy cannot update the badge', async () => {
    const env = setup(); env.start(); env.pending[0].resolve(analytics); await flush();
    const req = env.pending.find(r => r.path.includes('/battery?'));
    env.page.destroy(); req.resolve({ status: 'ok', latest_mv: 4000, samples: [] }); await flush();
    assert.equal(env.badge.children.length, 0);
  });
  console.log(`\n${passed} passed, ${failed} failed`); process.exit(failed ? 1 : 0);
})();
