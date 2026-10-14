#!/usr/bin/env node
/* test-issue-336-external-asset-pinning.js — every browser dependency the UI
   loads must be a locally-vendored, pinned, SRI-verified asset: no page may
   load one from a third-party CDN.

   History: #336 pinned the six dependencies (Leaflet CSS+JS, Leaflet.heat,
   Chart.js, swagger-ui-dist CSS+JS) to an exact version with a reviewed sha384
   digest while still loading them from unpkg.com. #400 closed scanner gaps
   (the `<link rel>` scope, dynamic `createElement('script'|'link')`
   injection, docs accuracy). #442 is the owner's follow-on decision (see the
   evaluation in #387/#400): host the six artefacts from public/vendor/ so the
   browser makes no third-party request at all — ending the privacy exposure
   (every visitor's IP/UA/referrer reaching the CDN) and the CDN availability
   dependency, and letting an offline/API-only install render the full UI.

   Why a scanner and not a fixed list of lines: a floating tag
   (`chart.js@4`) or a reintroduced CDN tag silently changes what the browser
   executes or re-opens the third-party request, so the rule has to hold for
   every current and future <script src>/<link href>, not only the ones that
   existed when this test was written.

   Scanned surfaces:
     - every *.html under public/ (shipped: `COPY public/ ./public/`, Dockerfile)
     - every *.html under tools/ (developer helpers, same rule so they cannot
       become the stale example someone copies)
     - every non-test *.go under cmd/ and internal/, which today means
       cmd/server/openapi.go's swaggerUIHTML const served at /api/docs (the Go
       side is additionally pinned by cmd/server/openapi_sri_test.go), and
       tomorrow means any new handler that prints HTML

   Only <link> tags that load a script or stylesheet are checked (stylesheet,
   modulepreload, preload as=script|style); preconnect, dns-prefetch,
   canonical and other hints fetch nothing executable and cannot carry SRI.
   Script/stylesheet injection from code (createElement('script'|'link'),
   importScripts, off-origin import()) is invisible to a tag scanner, so it is
   rejected outright on the same surfaces plus every *.js under public/ and
   tools/.

   tools/browser-deps-manifest.json is now the single source of truth for
   version + sha384 (consumed by tools/fetch-browser-deps.sh, this test and
   cmd/server/openapi_sri_test.go): a wrong-but-well-formed digest edited
   consistently into every tag still fails, because the committed bytes on
   disk are hashed and compared against the manifest independently of what
   any tag claims.

   Out of scope by design: map tile providers. Tiles are configured endpoints
   requested by Leaflet from JavaScript, not <script>/<link> subresources, so
   they carry no SRI and are deliberately untouched here (see #332). The
   scanner only reads script/link tags, so tile URLs never reach it.

   Relates to #336, #400, #442. */
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

// --- surfaces -------------------------------------------------------------

/** Every file under `dir` whose name ends in `ext`, recursively. Recursive on
    purpose: a page added in a subdirectory must not escape the rule. */
function filesUnder(dir, ext, skip = new Set(['node_modules', '.git'])) {
  const abs = path.join(ROOT, dir);
  if (!fs.existsSync(abs)) return [];
  const out = [];
  for (const entry of fs.readdirSync(abs, { withFileTypes: true }).sort((a, b) => a.name < b.name ? -1 : 1)) {
    const rel = path.posix.join(dir, entry.name);
    if (entry.isDirectory()) {
      if (!skip.has(entry.name)) out.push(...filesUnder(rel, ext, skip));
    } else if (entry.name.endsWith(ext) && !entry.name.endsWith('_test.go')) {
      out.push(rel);
    }
  }
  return out;
}

// Go sources too, not just openapi.go: any handler that starts printing HTML
// with a CDN tag in it is held to the same rule from the first commit. Test
// files are excluded by filesUnder -- a fixture may legitimately contain an
// unpinned tag to assert against.
const SURFACE_ROOTS = [
  ['public', '.html'],
  ['tools', '.html'],
  ['cmd', '.go'],
  ['internal', '.go'],
];
const SURFACES = SURFACE_ROOTS.flatMap(([dir, ext]) => filesUnder(dir, ext));

// --- tag scanner ----------------------------------------------------------

// Deliberately attribute-order agnostic: the rule must not be satisfiable by
// reordering, and must not be breakable by reformatting a tag onto one line.
const TAG_RE = /<(script|link)\b([^>]*)>/gi;
const ATTR_RE = /([a-zA-Z-]+)\s*=\s*("([^"]*)"|'([^']*)'|([^\s"'>]+))/g;

function attrs(raw) {
  const out = {};
  let m;
  ATTR_RE.lastIndex = 0;
  while ((m = ATTR_RE.exec(raw)) !== null) {
    out[m[1].toLowerCase()] = m[3] !== undefined ? m[3] : m[4] !== undefined ? m[4] : m[5];
  }
  return out;
}

/** Whether a <link> loads a script or stylesheet, i.e. something that can
    carry integrity. preconnect, dns-prefetch, canonical, icon and friends
    fetch nothing executable, and demanding SRI from them is unsatisfiable. */
function linkLoadsScriptOrStyle(a) {
  const rels = (a.rel || '').toLowerCase().split(/\s+/);
  if (rels.includes('stylesheet') || rels.includes('modulepreload')) return true;
  return rels.includes('preload') && ['script', 'style'].includes((a.as || '').toLowerCase());
}

/** Every off-origin subresource tag in one source text. */
function refsInSource(file, src) {
  const refs = [];
  let m;
  TAG_RE.lastIndex = 0;
  while ((m = TAG_RE.exec(src)) !== null) {
    const tag = m[1].toLowerCase();
    const a = attrs(m[2]);
    if (tag === 'link' && !linkLoadsScriptOrStyle(a)) continue;
    const url = tag === 'script' ? a.src : a.href;
    // Relative paths are same-origin and carry no third-party risk; a
    // protocol-relative URL is external and must not slip through.
    if (!url || !/^(https?:)?\/\//i.test(url)) continue;
    refs.push({ file, tag, url, attrs: a, line: src.slice(0, m.index).split('\n').length });
  }
  return refs;
}

/** Every off-origin subresource tag across the scanned surfaces. */
function externalRefs() {
  return SURFACES.flatMap((file) => refsInSource(file, fs.readFileSync(path.join(ROOT, file), 'utf8')));
}

const REFS = externalRefs();
const where = (r) => `${r.file}:${r.line} <${r.tag}> ${r.url}`;

// --- local (vendored) subresource scanner ---------------------------------

// A vendored tag's URL carries "vendor/<name>/<version>/<file>" somewhere in
// it (relative in public/*.html and tools/*.html -- siblings of public/vendor
// -- absolute ("/vendor/...") in cmd/server/openapi.go, which is served at
// /api/docs and needs a root-relative path to reach it).
const LOCAL_VENDOR_RE = /vendor\/([^/"'?]+)\/([^/"'?]+)\/([^"'?]+)/;

/** Every script/stylesheet tag in one source text that loads a vendored
    local dependency (as opposed to an arbitrary same-origin asset). */
function localVendorRefsInSource(file, src) {
  const refs = [];
  let m;
  TAG_RE.lastIndex = 0;
  while ((m = TAG_RE.exec(src)) !== null) {
    const tag = m[1].toLowerCase();
    const a = attrs(m[2]);
    if (tag === 'link' && !linkLoadsScriptOrStyle(a)) continue;
    const url = tag === 'script' ? a.src : a.href;
    if (!url) continue;
    const vm = LOCAL_VENDOR_RE.exec(url);
    if (!vm) continue;
    refs.push({
      file, tag, url, name: vm[1], version: vm[2], dest: vm[3], attrs: a,
      line: src.slice(0, m.index).split('\n').length,
    });
  }
  return refs;
}

function localVendorRefs() {
  return SURFACES.flatMap((file) => localVendorRefsInSource(file, fs.readFileSync(path.join(ROOT, file), 'utf8')));
}

const LOCAL_REFS = localVendorRefs();

// --- manifest ---------------------------------------------------------------

// tools/browser-deps-manifest.json is the single source of truth for what
// gets vendored and at what digest: tools/fetch-browser-deps.sh, this test
// and cmd/server/openapi_sri_test.go all read it instead of each keeping
// their own copy, so there is exactly one place to edit when a pin moves.
const MANIFEST_PATH = path.join(ROOT, 'tools/browser-deps-manifest.json');
const manifest = JSON.parse(fs.readFileSync(MANIFEST_PATH, 'utf8'));

function manifestFiles() {
  const out = [];
  for (const dep of manifest.dependencies) {
    for (const f of dep.files) {
      out.push({ name: dep.name, version: dep.version, dest: f.dest, url: f.url, sha384: f.sha384 });
    }
  }
  return out;
}

function sha384OfFile(absPath) {
  const data = fs.readFileSync(absPath);
  return 'sha384-' + crypto.createHash('sha384').update(data).digest('base64');
}

const SRI_RE = /^sha(256|384|512)-[A-Za-z0-9+/]+={0,2}$/;
const EXACT_VERSION = /^\d+\.\d+\.\d+(?:[-.][0-9A-Za-z.]+)?$/;

// Script or stylesheet loaders reached from code rather than markup. The tag
// scanner cannot see what URL they load or whether it carries integrity, so
// none is allowed until the rule is extended together with the first one.
const DYNAMIC_LOADER_RE =
  /createElement\(\s*(['"`])(?:script|link)\1\s*\)|\bimportScripts\s*\(|\bimport\s*\(\s*(['"`])(?:https?:)?\/\//gi;
const DYNAMIC_SURFACES = [
  ...SURFACES,
  ...filesUnder('public', '.js'),
  ...filesUnder('tools', '.js'),
];

function dynamicLoadersIn(file, src) {
  const out = [];
  let m;
  DYNAMIC_LOADER_RE.lastIndex = 0;
  while ((m = DYNAMIC_LOADER_RE.exec(src)) !== null) {
    out.push({ file, match: m[0], line: src.slice(0, m.index).split('\n').length });
  }
  return out;
}

console.log('\n── browser dependencies: local vendoring + SRI ──\n');

test('no third-party CDN subresource remains; every browser dependency is vendored locally (#442)', () => {
  assert.strictEqual(REFS.length, 0,
    'off-origin <script>/<link> found (should be vendored under public/vendor/ instead):\n' +
    REFS.map(where).join('\n'));
});

test('at least one locally vendored subresource is found (the scanner is not silently empty)', () => {
  assert(LOCAL_REFS.length > 0, `no vendored <script>/<link> found across ${SURFACES.length} scanned files`);
});

test('the manifest is not silently empty, and lists the six #336 dependencies', () => {
  const files = manifestFiles();
  const runtime = files.filter((f) => !/^LICENSE|^NOTICE/i.test(f.dest));
  assert.strictEqual(runtime.length, 6, `expected 6 runtime assets in the manifest, got ${runtime.length}`);
});

test('every manifest entry names a well-formed sha384 and an exact version', () => {
  const bad = manifestFiles().filter((f) => !SRI_RE.test(f.sha384) || !EXACT_VERSION.test(f.version));
  assert.strictEqual(bad.length, 0,
    'malformed manifest entry:\n' + bad.map((f) => `    ${f.name}/${f.version}/${f.dest}`).join('\n'));
});

test('every committed vendor file matches the digest recorded in the manifest', () => {
  const bad = [];
  for (const f of manifestFiles()) {
    const abs = path.join(ROOT, 'public/vendor', f.name, f.version, f.dest);
    if (!fs.existsSync(abs)) {
      bad.push(`${f.name}/${f.version}/${f.dest}: not vendored (expected at public/vendor/${f.name}/${f.version}/${f.dest})`);
      continue;
    }
    const got = sha384OfFile(abs);
    if (got !== f.sha384) {
      bad.push(`${f.name}/${f.version}/${f.dest}: want ${f.sha384}, got ${got}`);
    }
  }
  assert.strictEqual(bad.length, 0, bad.join('\n'));
});

test('tools/fetch-browser-deps.sh --check verifies the committed files with no network access', () => {
  const out = execFileSync('sh', ['tools/fetch-browser-deps.sh', '--check'], { cwd: ROOT, encoding: 'utf8' });
  assert(/all files verified/.test(out), 'unexpected output:\n' + out);
});

test('every locally vendored subresource names an exact version and a well-formed integrity hash', () => {
  const bad = LOCAL_REFS.filter((r) => !EXACT_VERSION.test(r.version) ||
    !r.attrs.integrity || !SRI_RE.test(r.attrs.integrity.trim()));
  assert.strictEqual(bad.length, 0,
    'not pinned/hashed:\n' + bad.map((r) => `    ${where(r)} integrity=${JSON.stringify(r.attrs.integrity)}`).join('\n'));
});

test('every locally vendored subresource sets crossorigin=anonymous', () => {
  const bad = LOCAL_REFS.filter((r) => (r.attrs.crossorigin || '').trim().toLowerCase() !== 'anonymous');
  assert.strictEqual(bad.length, 0,
    'missing or wrong crossorigin:\n' + bad.map((r) => `    ${where(r)}`).join('\n'));
});

test('every locally vendored subresource matches the digest recorded in the manifest', () => {
  const byNameVersionDest = new Map(manifestFiles().map((f) => [`${f.name}/${f.version}/${f.dest}`, f.sha384]));
  const bad = LOCAL_REFS.filter((r) => {
    const want = byNameVersionDest.get(`${r.name}/${r.version}/${r.dest}`);
    return !want || (r.attrs.integrity || '').trim() !== want;
  });
  assert.strictEqual(bad.length, 0,
    'tag integrity differs from the manifest (or the file is not in the manifest):\n' +
    bad.map((r) => `    ${where(r)} integrity=${JSON.stringify(r.attrs.integrity)}`).join('\n'));
});

test('<link> rels that fetch nothing executable are not held to the SRI rule', () => {
  // preconnect / dns-prefetch open a connection, canonical / icon / alternate
  // are not scripts or stylesheets; none of them can carry integrity, so a
  // demand for it would be unsatisfiable.
  const fixture = [
    '<link rel="preconnect" href="https://example.com">',
    '<link rel="dns-prefetch" href="//example.com">',
    '<link rel="canonical" href="https://example.org/">',
    '<link rel="icon" href="https://example.org/favicon.ico">',
    '<link href="https://example.org/no-rel">',
  ].join('\n');
  const got = refsInSource('fixture.html', fixture).map((r) => r.attrs.rel);
  assert.deepStrictEqual(got, [], 'non-subresource <link> treated as a subresource: ' + JSON.stringify(got));
});

test('<link> rels that load script or style are still held to the SRI rule', () => {
  const fixture = [
    '<link rel="stylesheet" href="https://example.com/a@1/a.css">',
    '<link rel="alternate stylesheet" href="https://example.com/a@1/b.css">',
    '<link rel="modulepreload" href="https://example.com/a@1/c.mjs">',
    '<link rel="preload" as="script" href="https://example.com/a@1/d.js">',
    '<link rel="preload" as="style" href="https://example.com/a@1/e.css">',
    '<link rel="PRELOAD" as="Script" href="//example.com/a@1/f.js">',
    '<script src="https://example.com/a@1/g.js"></script>',
  ].join('\n');
  assert.strictEqual(refsInSource('fixture.html', fixture).length, 7);
});

test('the dynamic-loader check recognises script/stylesheet injection from code', () => {
  // Self-test of the regex, so the repo-wide check below cannot pass by
  // matching nothing at all.
  const fixture = [
    "const s = document.createElement('script');",
    'const l = document.createElement( "link" );',
    'const t = document.createElement(`script`);',
    "importScripts('https://example.com/x@1/w.js');",
    "await import('https://example.com/x@1/m.mjs');",
    "document.createElement('div'); import('./local.js');",
  ].join('\n');
  const lines = dynamicLoadersIn('fixture.js', fixture).map((f) => f.line);
  assert.deepStrictEqual(lines, [1, 2, 3, 4, 5]);
});

test('no script or stylesheet is injected from code, where the tag scanner cannot check it', () => {
  const found = DYNAMIC_SURFACES.flatMap((file) =>
    dynamicLoadersIn(file, fs.readFileSync(path.join(ROOT, file), 'utf8')));
  assert.strictEqual(found.length, 0,
    'dynamic script/stylesheet loader(s) found; load the dependency with a static, pinned tag, or extend\n' +
    '    this test to verify the injected URL and integrity (see docs/browser-dependencies.md):\n' +
    found.map((f) => `    ${f.file}:${f.line} ${f.match}`).join('\n'));
});

test('public/index.html still loads Chart.js, Leaflet and Leaflet.heat, now vendored locally', () => {
  // Guards against "pinning" something by deleting it: #442 must not change
  // shipped behaviour, so the analytics and heatmap dependencies stay.
  const index = LOCAL_REFS.filter((r) => r.file === 'public/index.html').map((r) => r.name);
  assert(index.includes('chart.js'), 'Chart.js is gone from public/index.html');
  assert(index.includes('leaflet.heat'), 'Leaflet.heat is gone from public/index.html');
  assert(index.includes('leaflet'), 'Leaflet is gone from public/index.html');
});

test('cmd/server/openapi.go Swagger UI assets are vendored locally, pinned and hashed', () => {
  // The same rule, asserted against the Go-served page on its own, so the
  // Swagger UI surface cannot pass merely because the HTML files do.
  const swagger = LOCAL_REFS.filter((r) => r.file === 'cmd/server/openapi.go');
  assert.strictEqual(swagger.length, 2, `expected 2 vendored Swagger UI refs, got ${swagger.length}`);
  for (const r of swagger) {
    assert.strictEqual(r.name, 'swagger-ui-dist', `unexpected dependency name: ${where(r)}`);
    assert(SRI_RE.test((r.attrs.integrity || '').trim()), `no integrity: ${where(r)}`);
    assert.strictEqual((r.attrs.crossorigin || '').toLowerCase(), 'anonymous', `no crossorigin: ${where(r)}`);
  }
});

test('license texts are vendored for every dependency', () => {
  // Matched by the licence *shape*, not the SPDX id as a literal word: the
  // BSD-2-Clause text (Leaflet, Leaflet.heat) never spells out "BSD".
  const SHAPE_BY_LICENSE = {
    'BSD-2-Clause': /Redistribution and use in source and binary forms/i,
    MIT: /Permission is hereby granted, free of charge/i,
    'Apache-2.0': /Apache License/i,
  };
  for (const dep of manifest.dependencies) {
    const re = SHAPE_BY_LICENSE[dep.license];
    assert(re, `no known shape for licence ${dep.license} (${dep.name})`);
    const licenseFile = dep.files.find((f) => /^LICENSE/i.test(f.dest));
    assert(licenseFile, `manifest entry for ${dep.name} has no LICENSE file`);
    const abs = path.join(ROOT, 'public/vendor', dep.name, dep.version, licenseFile.dest);
    assert(fs.existsSync(abs), `${abs} is missing`);
    const text = fs.readFileSync(abs, 'utf8');
    assert(re.test(text), `${abs} does not look like a ${dep.license} licence (expected to match ${re})`);
  }
  // swagger-ui-dist (Apache-2.0) additionally carries a NOTICE file.
  const swagger = manifest.dependencies.find((d) => d.name === 'swagger-ui-dist');
  const noticeFile = swagger.files.find((f) => f.dest === 'NOTICE');
  assert(noticeFile, 'manifest entry for swagger-ui-dist has no NOTICE file');
  assert(fs.existsSync(path.join(ROOT, 'public/vendor', swagger.name, swagger.version, 'NOTICE')),
    'public/vendor/swagger-ui-dist/<version>/NOTICE is missing');
});

test('documentation records the update procedure and the fetch script', () => {
  const doc = path.join(ROOT, 'docs/browser-dependencies.md');
  assert(fs.existsSync(doc), 'docs/browser-dependencies.md is missing');
  const text = fs.readFileSync(doc, 'utf8');
  assert(/openssl dgst -sha384/.test(text), 'the hash recomputation command is not documented');
  assert(text.includes('tools/fetch-browser-deps.sh'), 'docs/browser-dependencies.md does not mention the fetch script');
  assert(text.includes('tools/browser-deps-manifest.json'), 'docs/browser-dependencies.md does not mention the manifest');
  // Every upstream URL must still be listed for provenance, so the inventory
  // cannot drift from what is actually vendored.
  for (const f of manifestFiles()) {
    if (/^LICENSE|^NOTICE/i.test(f.dest)) continue;
    assert(text.includes(f.url), `docs/browser-dependencies.md does not list ${f.url}`);
  }
});

test('documentation names every scanned surface, not only openapi.go', () => {
  const text = fs.readFileSync(path.join(ROOT, 'docs/browser-dependencies.md'), 'utf8');
  for (const [dir, ext] of SURFACE_ROOTS) {
    assert(text.includes('`' + dir + '/`'), `docs/browser-dependencies.md does not say \`${dir}/\` (*${ext}) is scanned`);
  }
  assert(!/plus `cmd\/server\/openapi\.go`/.test(text),
    'docs/browser-dependencies.md still says the scanner covers only cmd/server/openapi.go among Go files');
});

test('documentation names every place a digest or pin is recorded', () => {
  const text = fs.readFileSync(path.join(ROOT, 'docs/browser-dependencies.md'), 'utf8');
  for (const name of ['tools/browser-deps-manifest.json', 'wantSwaggerUIAssets']) {
    assert(text.includes(name), `docs/browser-dependencies.md does not mention ${name}`);
  }
});

test('documentation states the <link> rel scope and the dynamic-loader rule', () => {
  const text = fs.readFileSync(path.join(ROOT, 'docs/browser-dependencies.md'), 'utf8');
  assert(/rel="preconnect"/.test(text) && /rel="modulepreload"/.test(text),
    'docs/browser-dependencies.md does not say which <link> rel values are checked');
  assert(/createElement\('script'\)/.test(text),
    'docs/browser-dependencies.md does not document the dynamic script injection rule');
});

test('documentation records local hosting as implemented, not as a pending evaluation', () => {
  const text = fs.readFileSync(path.join(ROOT, 'docs/browser-dependencies.md'), 'utf8');
  assert(/^## Local hosting/m.test(text), 'docs/browser-dependencies.md has no "## Local hosting" section');
  assert(!/evaluated, not done/i.test(text),
    'docs/browser-dependencies.md still describes local hosting as a pending evaluation (#442 implemented it)');
  assert(text.includes('public/vendor/'), 'docs/browser-dependencies.md does not describe the public/vendor/ layout');
});

test('documentation ships the licence position for every vendored dependency', () => {
  const text = fs.readFileSync(path.join(ROOT, 'docs/browser-dependencies.md'), 'utf8');
  for (const spdx of ['BSD-2-Clause', 'MIT', 'Apache-2.0']) {
    assert(text.includes(spdx), `docs/browser-dependencies.md does not mention the ${spdx} licence`);
  }
  assert(/NOTICE/.test(text), 'docs/browser-dependencies.md does not mention the swagger-ui-dist NOTICE requirement');
});

test('test-issue-336-external-asset-pinning.js is registered exactly once in test-all.sh', () => {
  const sh = fs.readFileSync(path.join(ROOT, 'test-all.sh'), 'utf8');
  const lines = sh.split('\n').filter((l) => l.trim() === 'run test-issue-336-external-asset-pinning.js');
  assert.strictEqual(lines.length, 1,
    'expected exactly one registration line in test-all.sh, got ' + lines.length);
});

console.log(`\n${passed} passed, ${failed} failed`);
process.exit(failed ? 1 : 0);
