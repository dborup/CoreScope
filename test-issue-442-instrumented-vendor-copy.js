#!/usr/bin/env node
/* test-issue-442-instrumented-vendor-copy.js — scripts/instrument-frontend.sh
   must copy public/vendor/ into public-instrumented/vendor/ RECURSIVELY.

   Found by CI (#442): a flat `cp public/vendor/* public-instrumented/vendor/`
   silently drops any file under a subdirectory, because plain `cp` (no -r)
   skips directories rather than erroring. #442 vendors Leaflet, Leaflet.heat,
   Chart.js and swagger-ui-dist under public/vendor/<name>/<version>/<file> —
   nested one level deeper than the pre-#442 flat vendor/*.js files — so the
   instrumented build the Playwright E2E job actually serves
   (-public public-instrumented) lost all four, and every page that loads one
   failed with "L is not defined" / "Chart is not defined" /
   "SwaggerUIBundle is not defined" (observed on CI: shard 2's
   test-e2e-playwright.js, run 38519758758).

   This test runs the real script (not a mock) and diffs the two trees, so a
   future vendor file nested at any depth is still caught.

   Relates to #442. */
'use strict';
const fs = require('fs');
const path = require('path');
const assert = require('assert');
const crypto = require('crypto');
const { execFileSync } = require('child_process');

let passed = 0, failed = 0;
function test(name, fn) {
  try {
    fn();
    passed++;
    console.log(`  ✅ ${name}`);
  } catch (e) {
    failed++;
    console.log(`  ❌ ${name}: ${e.message}`);
  }
}

const ROOT = __dirname;
const INSTRUMENTED = path.join(ROOT, 'public-instrumented');

function filesUnder(dir) {
  const out = [];
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const abs = path.join(dir, entry.name);
    if (entry.isDirectory()) out.push(...filesUnder(abs));
    else out.push(abs);
  }
  return out;
}

function sha256(p) {
  return crypto.createHash('sha256').update(fs.readFileSync(p)).digest('hex');
}

console.log('\n── scripts/instrument-frontend.sh: recursive public/vendor/ copy (#442) ──\n');

try {
  execFileSync('sh', ['scripts/instrument-frontend.sh'], { cwd: ROOT, stdio: 'pipe' });

  test('public-instrumented/vendor/ exists', () => {
    assert(fs.existsSync(path.join(INSTRUMENTED, 'vendor')), 'public-instrumented/vendor is missing');
  });

  test('every file under public/vendor/ (any depth) is byte-identical under public-instrumented/vendor/', () => {
    const srcRoot = path.join(ROOT, 'public/vendor');
    const bad = [];
    for (const src of filesUnder(srcRoot)) {
      const rel = path.relative(srcRoot, src);
      const dest = path.join(INSTRUMENTED, 'vendor', rel);
      if (!fs.existsSync(dest)) {
        bad.push(`${rel}: missing from public-instrumented/vendor/`);
        continue;
      }
      if (sha256(src) !== sha256(dest)) bad.push(`${rel}: content differs`);
    }
    assert.strictEqual(bad.length, 0, bad.join('\n    '));
  });

  test('the four #442 dependency directories specifically survive the copy', () => {
    for (const name of ['leaflet/1.9.4/leaflet.js', 'leaflet.heat/0.2.0/leaflet-heat.js',
      'chart.js/4.5.1/chart.umd.min.js', 'swagger-ui-dist/5.33.2/swagger-ui-bundle.js']) {
      const dest = path.join(INSTRUMENTED, 'vendor', name);
      assert(fs.existsSync(dest), `${dest} is missing (nested vendor dir dropped by a non-recursive copy)`);
    }
  });
} finally {
  fs.rmSync(INSTRUMENTED, { recursive: true, force: true });
}

console.log(`\n${passed} passed, ${failed} failed`);
process.exit(failed ? 1 : 0);
