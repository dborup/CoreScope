#!/usr/bin/env node
/* test-issue-628-vendor-gitattributes.js — audit of upstream PR 2299 (#628).

   Upstream vendored Leaflet, leaflet.heat and Chart.js because the pages loaded
   them from unpkg.com. This fork already serves them from public/vendor/ (#442),
   and test-issue-336-external-asset-pinning.js pins the tags, SRI and manifest
   digests. The one lesson from 2299 not yet pinned here: some vendored files
   (leaflet.css) ship CRLF upstream, and `* text=auto eol=lf` in .gitattributes
   would rewrite them on checkout, so the bytes no longer match the SRI hash and
   the browser refuses the file. Every manifest file must stay `-text`.

   Relates to #628. */
'use strict';
const fs = require('fs');
const path = require('path');
const assert = require('assert');
const { execFileSync } = require('child_process');

const ROOT = __dirname;
let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log(`  ✅ ${name}`); }
  catch (e) { failed++; console.log(`  ❌ ${name}: ${e.message}`); }
}

const manifest = JSON.parse(fs.readFileSync(path.join(ROOT, 'tools/browser-deps-manifest.json'), 'utf8'));
const vendored = [];
for (const dep of manifest.dependencies) {
  for (const f of dep.files) vendored.push(`public/vendor/${dep.name}/${dep.version}/${f.dest}`);
}

test('manifest lists the Leaflet, leaflet.heat and Chart.js files', () => {
  for (const want of ['leaflet/', 'leaflet.heat/', 'chart.js/']) {
    assert.ok(vendored.some((p) => p.includes('/vendor/' + want)), 'no manifest file under ' + want);
  }
});

test('git does not normalise any manifest-vendored file (-text), so SRI bytes survive checkout', () => {
  const out = execFileSync('git', ['check-attr', 'text', '--', ...vendored], { cwd: ROOT, encoding: 'utf8' });
  const bad = out.split('\n').filter((l) => l && !/: text: unset$/.test(l));
  assert.strictEqual(bad.length, 0, 'normalised by .gitattributes:\n' + bad.join('\n'));
});

test('leaflet.css keeps its upstream CRLF bytes in the working tree', () => {
  const css = fs.readFileSync(path.join(ROOT, 'public/vendor/leaflet/1.9.4/leaflet.css'));
  assert.ok(css.includes(Buffer.from('\r\n')), 'leaflet.css lost its CRLF line endings');
});

test('test-issue-628-vendor-gitattributes.js is registered exactly once in test-all.sh', () => {
  const sh = fs.readFileSync(path.join(ROOT, 'test-all.sh'), 'utf8');
  const n = sh.split('\n').filter((l) => l.trim() === 'run test-issue-628-vendor-gitattributes.js').length;
  assert.strictEqual(n, 1, 'registration lines: ' + n);
});

console.log(`\n${passed} passed, ${failed} failed`);
process.exit(failed ? 1 : 0);
