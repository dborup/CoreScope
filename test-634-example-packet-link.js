/* #634 / upstream 2296: the observed-path sample link goes to one example
 * packet, so it must say so (not the generic "Analyze"). */
'use strict';
const assert = require('assert');
const { loadNodes } = require('./sandbox-634-nodes');
const ctx = loadNodes();
let failed = 0;
function test(name, fn) {
  try { fn(); console.log('  ✅ ' + name); } catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}
console.log('\n=== #634: example packet link ===');
const render = ctx.window._nodesRenderExamplePacketLink;
test('renderExamplePacketLink is exported', () => assert.strictEqual(typeof render, 'function'));
test('label says "View example packet →" and keeps the canonical packet href', () => {
  const html = render('abc123');
  assert.ok(html.includes('>View example packet →</a>'), html);
  assert.ok(html.includes('href="#/packets/abc123"'), html);
  assert.ok(!html.includes('Analyze'), html);
});
test('hash is attribute-escaped', () => {
  const html = render('"><img src=x>');
  assert.ok(!html.includes('"><img'), html);
});
test('both observed-path views use the shared helper', () => {
  const src = require('fs').readFileSync(__dirname + '/public/nodes.js', 'utf8');
  assert.strictEqual((src.match(/renderExamplePacketLink\(p\.sampleHash\)/g) || []).length, 2);
  assert.ok(!/class="ch-analyze-link">Analyze/.test(src));
});
process.exit(failed ? 1 : 0);
