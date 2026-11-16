/**
 * Docs check (#811 M4): the node share previews page exists, documents the
 * operator switch, the card roles and the viewer switch, and is linked from
 * the terrain share page and the customizer guide.
 *
 * Usage: node test-issue-811-m4-docs.js
 */
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}
const read = (f) => fs.readFileSync(path.join(__dirname, f), 'utf8');

console.log('\n📄 #811 M4 — node share previews docs\n');

test('docs/node-share-previews.md covers the switches, the roles and the cost', () => {
  const md = read('docs/node-share-previews.md');
  ['share.nodes.enabled', '/p?node=', '/api/nodes/{pubkey}/card.png', 'Copy link shares a preview',
    'terrain.shareLinks', 'Companions', 'max-age=600', 'map.dhm.profile.card.theme'].forEach((s) => {
    assert.ok(md.includes(s), 'missing ' + JSON.stringify(s));
  });
  assert.ok(/no map, no coordinates/i.test(md), 'does not say the card has no coordinates');
});

test('the terrain share page and the customizer guide link to it', () => {
  assert.ok(read('docs/share-previews.md').includes('(node-share-previews.md'), 'share-previews.md');
  assert.ok(read('docs/user-guide/customization.md').includes('(../node-share-previews.md)'), 'customization.md');
});

console.log('\n' + passed + ' passed, ' + failed + ' failed\n');
if (failed > 0) process.exit(1);
