/* #563 — route-aware browser tab titles: unit tests for public/page-title.js */
'use strict';
const assert = require('assert');
const PT = require('./public/page-title.js');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}
// Fresh instance bound to a fake document so tests never share state.
function make(instance) {
  const doc = { title: 'CoreScope' };
  const t = PT.create(doc);
  if (instance) t.setInstance(instance);
  return { doc, t };
}

console.log('page-title.js (#563)');

test('titleFor joins view, entity and instance with " · "', () => {
  assert.strictEqual(PT.titleFor('Reach', 'Node A', 'Mesh'), 'Reach · Node A · Mesh');
  assert.strictEqual(PT.titleFor('Live', '', 'Mesh'), 'Live · Mesh');
  assert.strictEqual(PT.titleFor('', '', 'Mesh'), 'Mesh');
});

test('route titles use "<View> · <instance>"', () => {
  const { doc, t } = make('Mesh');
  t.setRoute('live');
  assert.strictEqual(doc.title, 'Live · Mesh');
  t.setRoute('channels');
  assert.strictEqual(doc.title, 'Channels · Mesh');
  t.setRoute('node-analytics');
  assert.strictEqual(doc.title, 'Node Analytics · Mesh');
  t.setRoute('analytics');
  assert.strictEqual(doc.title, 'Analytics · Mesh');
});

test('unknown route falls back to "Not found · <instance>"', () => {
  const { doc, t } = make('Mesh');
  t.setRoute('does-not-exist');
  assert.strictEqual(doc.title, 'Not found · Mesh');
});

test('entity label is added once known, and it is plain text', () => {
  const { doc, t } = make('Mesh');
  t.setRoute('node-reach');
  assert.strictEqual(doc.title, 'Reach · Mesh');
  assert.strictEqual(t.setEntity('<img src=x onerror=alert(1)>', t.visit()), true);
  assert.strictEqual(doc.title, 'Reach · <img src=x onerror=alert(1)> · Mesh');
});

test('entity labels are collapsed to one line and bounded', () => {
  const { doc, t } = make('Mesh');
  t.setRoute('node-reach');
  t.setEntity('A\n\tB\u0000C ' + 'x'.repeat(200), t.visit());
  assert.ok(!/[\n\t\u0000]/.test(doc.title));
  assert.ok(doc.title.length < 120, 'title length ' + doc.title.length);
});

test('late entity label from an earlier visit is ignored', () => {
  const { doc, t } = make('Mesh');
  t.setRoute('node-reach');
  const stale = t.visit();
  t.setRoute('live');
  assert.strictEqual(t.setEntity('Old Node', stale), false);
  assert.strictEqual(doc.title, 'Live · Mesh');
});

test('leaving and revisiting the same route invalidates the old token', () => {
  const { doc, t } = make('Mesh');
  t.setRoute('node-reach');
  const first = t.visit();
  t.setRoute('live');
  t.setRoute('node-reach');
  assert.strictEqual(t.setEntity('Old Node', first), false);
  assert.strictEqual(doc.title, 'Reach · Mesh');
  assert.strictEqual(t.setEntity('New Node', t.visit()), true);
  assert.strictEqual(doc.title, 'Reach · New Node · Mesh');
});

test('navigating clears the previous entity label', () => {
  const { doc, t } = make('Mesh');
  t.setRoute('node-reach');
  t.setEntity('Node A', t.visit());
  t.setRoute('node-analytics');
  assert.strictEqual(doc.title, 'Node Analytics · Mesh');
});

test('setInstance after navigation keeps the route title', () => {
  const { doc, t } = make();
  t.setRoute('live');
  t.setInstance('My Mesh');
  assert.strictEqual(doc.title, 'Live · My Mesh');
  t.setRoute('channels');
  assert.strictEqual(doc.title, 'Channels · My Mesh');
});

test('setInstance keeps the entity label', () => {
  const { doc, t } = make('Mesh');
  t.setRoute('node-reach');
  t.setEntity('Node A', t.visit());
  t.setInstance('Renamed');
  assert.strictEqual(doc.title, 'Reach · Node A · Renamed');
});

test('empty or blank instance name is ignored (keeps previous)', () => {
  const { doc, t } = make('Mesh');
  t.setRoute('live');
  t.setInstance('   ');
  assert.strictEqual(doc.title, 'Live · Mesh');
});

test('before any route is known the bare instance name is shown', () => {
  const { doc, t } = make();
  t.setInstance('Solo');
  assert.strictEqual(doc.title, 'Solo');
});

test('every registered page name has a label', () => {
  ['home','packets','packet-detail','map','live','channels','nodes','node-analytics','node-reach',
   'observers','observer-detail','analytics','perf','tools-landing','traces','path-inspector',
   'observer-neighbors-tool','new-nodes-tool','node-changes-tool','network-digest-tool',
   'position-gaps-tool','gps-sanity-tool','compare','audio-lab','privacy','ping-scores',
   'reach-rank','rx-coverage','regions'].forEach(p => {
    assert.ok(PT.ROUTE_LABELS[p], 'missing label for ' + p);
  });
});

console.log('\n' + passed + ' passed, ' + failed + ' failed');
process.exit(failed ? 1 : 0);
