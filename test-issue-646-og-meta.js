'use strict';
/* Issue #646 — static checks for the link-preview (Open Graph) work.
 * The server-side rendering itself is covered by cmd/server/og_meta_test.go. */
const assert = require('assert');
const fs = require('fs');
const path = require('path');

let pass = 0, fail = 0;
function test(name, fn) {
  try { fn(); pass++; console.log('  ✓ ' + name); }
  catch (e) { fail++; console.log('  ✗ ' + name + '\n    ' + e.message); }
}

const html = fs.readFileSync(path.join(__dirname, 'public', 'index.html'), 'utf8');
const png = fs.readFileSync(path.join(__dirname, 'public', 'og-image-meshview.png'));
const src = fs.readFileSync(path.join(__dirname, 'scripts', 'og-image-meshview.html'), 'utf8');

console.log('\n=== #646: index.html OG placeholders ===');
test('og:image and twitter:image are placeholders, not external hosts', () => {
  assert.ok(/property="og:image" content="__OG_IMAGE__"/.test(html));
  assert.ok(/name="twitter:image" content="__OG_IMAGE__"/.test(html));
  assert.ok(!/(og|twitter):image"\s+content="https?:\/\//.test(html), 'external image host');
  assert.ok(!/githubusercontent/.test(html));
});
test('title/description tags are placeholders', () => {
  assert.ok(html.includes('<title>__OG_TITLE__</title>'));
  assert.ok(/og:site_name" content="__OG_TITLE__"/.test(html));
  assert.ok(/og:description" content="__OG_DESCRIPTION__"/.test(html));
  assert.ok(/twitter:description" content="__OG_DESCRIPTION__"/.test(html));
  assert.ok(html.includes('__OG_IMAGE_DIMS__'));
});

console.log('\n=== #646: MeshView DK preview image ===');
test('PNG is 1200x630 and under 300 KB', () => {
  assert.strictEqual(png.slice(1, 4).toString(), 'PNG');
  assert.strictEqual(png.readUInt32BE(16), 1200);
  assert.strictEqual(png.readUInt32BE(20), 630);
  assert.ok(png.length < 300 * 1024, 'size ' + png.length);
});
test('source has the required text and no remote resources', () => {
  assert.ok(src.includes('MeshView'));
  assert.ok(src.includes('Dansk MeshCore overblik'));
  assert.ok(!/(src|href)\s*=\s*["']?https?:/i.test(src), 'remote resource');
  assert.ok(!/url\(\s*["']?https?:/i.test(src), 'remote url()');
  assert.ok(!/<img\b/i.test(src), 'raster art');
});
test('source colours are CSS variables defined in :root', () => {
  const body = src.replace(/<!--[\s\S]*?-->/g, '').replace(/:root\s*\{[^}]*\}/, '');
  assert.ok(!/#[0-9a-fA-F]{3,8}\b/.test(body), 'hardcoded hex outside :root');
});
test('config.example.json documents the new branding keys', () => {
  const b = JSON.parse(fs.readFileSync(path.join(__dirname, 'config.example.json'), 'utf8')).branding;
  assert.ok('ogImageUrl' in b && 'publicBaseUrl' in b);
});

console.log('\n' + pass + ' passed, ' + fail + ' failed');
process.exit(fail ? 1 : 0);
