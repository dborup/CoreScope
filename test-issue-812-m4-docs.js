/**
 * Docs check (#812 M4): docs/share-previews.md has a packet links section
 * that says what the card shows, what it never shows, what it costs and
 * how to turn it off, and the customizer guide points to it.
 *
 * Usage: node test-issue-812-m4-docs.js
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

/** The text of the "## Packet links" section, up to the next "## ". */
function packetSection() {
  const md = read('docs/share-previews.md');
  const at = md.indexOf('\n## Packet links');
  assert.ok(at >= 0, 'no "## Packet links" section');
  const rest = md.slice(at + 1);
  const next = rest.indexOf('\n## ', 3);
  return next < 0 ? rest : rest.slice(0, next);
}

console.log('\n📄 #812 M4 — packet share previews docs\n');

test('the section names the links, the switch and the card endpoint', () => {
  const s = packetSection();
  ['share.packets.enabled', '/p?packet=', '&viewPath=1', '/api/packets/{hash}/card.png', 'Copy link shares a preview',
    'terrain.shareLinks', '#/packet/', 'View Path'].forEach((k) => assert.ok(s.includes(k), 'missing ' + JSON.stringify(k)));
});

test('it says what the card shows', () => {
  const s = packetSection();
  ['### What the card shows', 'payload type', 'observer', 'airtime', 'First heard by'].forEach((k) => assert.ok(s.includes(k), 'missing ' + JSON.stringify(k)));
});

test('it says what is never shown', () => {
  const s = packetSection();
  assert.ok(s.includes('### What is never shown'), 'no "What is never shown" heading');
  ['message text', 'channel name', 'sender', 'hidden'].forEach((k) => assert.ok(new RegExp(k, 'i').test(s), 'missing ' + JSON.stringify(k)));
  assert.ok(/ADVERT/.test(s), 'does not say only an advert names its origin');
});

test('it says what it costs: the caches, Cache-Control and the shared budget', () => {
  const s = packetSection();
  assert.ok(s.includes('### What it costs'), 'no cost heading');
  ['max-age=120', 'max-age=86400', 'rateLimitPerMin', 'No upstream'].forEach((k) => assert.ok(s.includes(k), 'missing ' + JSON.stringify(k)));
});

test('it says how to turn it off, for the operator and the viewer', () => {
  const s = packetSection();
  assert.ok(s.includes('### Turning it off'), 'no off heading');
  assert.ok(/\*\*Operator\.\*\*/.test(s) && /\*\*Viewer\.\*\*/.test(s));
  assert.ok(/\?obs=/.test(s), 'does not say a selected observation keeps the plain link');
});

test('the customizer guide and the node page mention packet links', () => {
  assert.ok(read('docs/user-guide/customization.md').includes('(../share-previews.md#packet-links)'), 'customization.md');
  assert.ok(read('docs/node-share-previews.md').includes('(share-previews.md#packet-links)'), 'node-share-previews.md');
});

test('every relative .md link in the three pages resolves', () => {
  ['docs/node-share-previews.md', 'docs/share-previews.md', 'docs/user-guide/customization.md'].forEach((f) => {
    const links = [...read(f).matchAll(/\]\(([^)#:]+\.md)(#[^)]*)?\)/g)].map((m) => m[1]);
    links.forEach((l) => {
      assert.ok(fs.existsSync(path.join(__dirname, path.dirname(f), l)), f + ' links to a missing ' + l);
    });
  });
});

console.log('\n' + passed + ' passed, ' + failed + ' failed\n');
if (failed > 0) process.exit(1);
