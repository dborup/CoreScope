/* #634 / upstream 2315: side and tablet previews show the same Traffic share
 * and Hash Prefix rows as the full node page, and the tablet preview reads the
 * API's `name` (not only `advert_name`). */
'use strict';
const assert = require('assert');
const { loadNodes } = require('./sandbox-634-nodes');
const ctx = loadNodes();
const w = ctx.window;
let failed = 0;
function test(name, fn) {
  try { fn(); console.log('  ✅ ' + name); } catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}
console.log('\n=== #634: node preview metadata ===');
const KEY = 'ab12cd34ef56'.repeat(5) + 'abcd';
const rep = { public_key: KEY, name: 'Rep One', role: 'repeater', hash_size: 2, traffic_share_score: 0.25 };

test('helpers are exported', () => {
  for (const f of ['_nodesRenderTrafficShare', '_nodesRenderHashPrefix', '_nodesRenderSlideOverBody'])
    assert.strictEqual(typeof w[f], 'function', f);
});
test('traffic share: table form keeps the row id, list form has no duplicate id', () => {
  const t = w._nodesRenderTrafficShare(rep, true);
  const l = w._nodesRenderTrafficShare(rep, false);
  assert.ok(t.includes('id="row-usefulness-score"') && t.includes('25.0%'), t);
  assert.ok(!l.includes('id="row-usefulness-score"') && l.includes('<dt') && l.includes('25.0%'), l);
});
test('traffic share: measured zero renders; legacy usefulness_score falls back; companion omitted', () => {
  assert.ok(w._nodesRenderTrafficShare({ role: 'repeater', traffic_share_score: 0, usefulness_score: 0.9 }, false).includes('0.0%'));
  assert.ok(w._nodesRenderTrafficShare({ role: 'room', usefulness_score: 0.5 }, false).includes('50.0%'));
  assert.strictEqual(w._nodesRenderTrafficShare({ role: 'companion', traffic_share_score: 0.5 }, false), '');
  assert.strictEqual(w._nodesRenderTrafficShare({ role: 'repeater' }, false), '');
});
test('hash prefix: shows the prefix, Unknown when missing, escapes sizes', () => {
  assert.ok(w._nodesRenderHashPrefix(rep, false).includes('AB12') && w._nodesRenderHashPrefix(rep, false).includes('(2-byte)'));
  assert.ok(w._nodesRenderHashPrefix({ public_key: KEY }, false).includes('Unknown'));
  const bad = w._nodesRenderHashPrefix({ public_key: KEY, hash_size: 1, hash_size_inconsistent: true, hash_sizes_seen: ['"><img src=x>'] }, false);
  assert.ok(!bad.includes('"><img'), bad);
});
test('slide-over body: uses `name`, falls back to advert_name, escapes', () => {
  assert.ok(w._nodesRenderSlideOverBody(rep).includes('<dd>Rep One</dd>'));
  const legacy = w._nodesRenderSlideOverBody({ public_key: KEY, advert_name: 'Legacy', role: 'repeater' });
  assert.ok(legacy.includes('<dd>Legacy</dd>'));
  assert.ok(!w._nodesRenderSlideOverBody({ public_key: KEY, name: '<b>x</b>' }).includes('<b>x</b>'));
});
test('slide-over body: repeater shows Last Relayed, Traffic share, Hash Prefix; companion does not show the first two', () => {
  const h = w._nodesRenderSlideOverBody(rep);
  assert.ok(h.includes('Last Relayed') && h.includes('Traffic share') && h.includes('Hash Prefix'), h);
  const c = w._nodesRenderSlideOverBody({ public_key: KEY, name: 'C', role: 'companion', traffic_share_score: 0.5 });
  assert.ok(!c.includes('Last Relayed') && !c.includes('Traffic share') && c.includes('Hash Prefix'), c);
});
test('side preview <dl class="detail-meta"> renders Traffic share and Hash Prefix once each', () => {
  const src = require('fs').readFileSync(__dirname + '/public/nodes.js', 'utf8');
  const a = src.indexOf('<dl class="detail-meta">');
  const dl = src.slice(a, src.indexOf('</dl>', a));
  assert.strictEqual((dl.match(/renderTrafficShare\(n, false\)/g) || []).length, 1);
  assert.strictEqual((dl.match(/renderHashPrefix\(n, false\)/g) || []).length, 1);
});
process.exit(failed ? 1 : 0);
