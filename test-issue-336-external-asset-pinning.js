#!/usr/bin/env node
/* test-issue-336-external-asset-pinning.js — every browser dependency that is
   fetched from a third party must be pinned to an exact version and verified
   with Subresource Integrity.

   Why a scanner and not a fixed list of lines: a floating tag (`chart.js@4`)
   silently changes what the browser executes whenever upstream publishes, and
   a missing `integrity` means a compromised or mutated CDN response runs with
   full page privileges. The rule therefore has to hold for every current and
   future <script src>/<link href> that points off-origin, not only for the
   ones that existed when this test was written.

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
   tools/. Every digest must also equal its REVIEWED_DIGESTS entry, so a
   wrong but well-formed hash cannot pass by being edited consistently.

   Out of scope by design: map tile providers. Tiles are configured endpoints
   requested by Leaflet from JavaScript, not <script>/<link> subresources, so
   they carry no SRI and are deliberately untouched here (see #332). The
   scanner only reads script/link tags, so tile URLs never reach it.

   Relates to #336. */
'use strict';
const fs = require('fs');
const path = require('path');
const assert = require('assert');

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

// --- pinning rules --------------------------------------------------------

// npm-backed CDNs serve an immutable artefact only for a complete
// major.minor.patch specifier; `@4` and `@^4.5.1` are ranges that resolve at
// request time. Any other host has no known pinning grammar, so it fails
// closed until someone adds the rule together with the dependency.
const NPM_CDN_HOSTS = new Set(['unpkg.com', 'cdn.jsdelivr.net']);
const EXACT_NPM_VERSION = /@\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?\//;

function pinProblem(url) {
  let parsed;
  try {
    parsed = new URL(url.startsWith('//') ? 'https:' + url : url);
  } catch {
    return 'unparseable URL';
  }
  if (!NPM_CDN_HOSTS.has(parsed.hostname)) {
    return `host ${parsed.hostname} has no pinning rule in this test; add one with the dependency`;
  }
  if (!EXACT_NPM_VERSION.test(parsed.pathname)) {
    return 'path carries no exact major.minor.patch version (a floating range resolves at request time)';
  }
  return null;
}

const SRI_RE = /^sha(256|384|512)-[A-Za-z0-9+/]+={0,2}$/;

// Reviewed digests, keyed by URL. The format rule above cannot tell a right
// digest from a wrong one, and a wrong one edited consistently into every
// page passes the "one URL, one hash" rule too -- the browser then blocks the
// file. This table is the second, deliberate place a digest has to change.
// Each value is sha384 over the bytes the pinned URL serves; recompute with
//   curl -sL <url> | openssl dgst -sha384 -binary | openssl base64 -A
// (see docs/browser-dependencies.md). The Swagger UI pair is also pinned in
// cmd/server/openapi_sri_test.go (wantSwaggerUIAssets); keep both in step.
const REVIEWED_DIGESTS = new Map([
  ['https://unpkg.com/leaflet@1.9.4/dist/leaflet.css',
    'sha384-sHL9NAb7lN7rfvG5lfHpm643Xkcjzp4jFvuavGOndn6pjVqS6ny56CAt3nsEVT4H'],
  ['https://unpkg.com/leaflet@1.9.4/dist/leaflet.js',
    'sha384-cxOPjt7s7Iz04uaHJceBmS+qpjv2JkIHNVcuOrM+YHwZOmJGBXI00mdUXEq65HTH'],
  ['https://unpkg.com/leaflet.heat@0.2.0/dist/leaflet-heat.js',
    'sha384-mFKkGiGvT5vo1fEyGCD3hshDdKmW3wzXW/x+fWriYJArD0R3gawT6lMvLboM22c0'],
  ['https://unpkg.com/chart.js@4.5.1/dist/chart.umd.min.js',
    'sha384-jb8JQMbMoBUzgWatfe6COACi2ljcDdZQ2OxczGA3bGNeWe+6DChMTBJemed7ZnvJ'],
  ['https://unpkg.com/swagger-ui-dist@5.33.2/swagger-ui.css',
    'sha384-Ov4/wv3j2bmct8cDc5X4ngJZohVPzEmc6uDPH8WeljUxO5vtoykvMEfbu9Vh6RaW'],
  ['https://unpkg.com/swagger-ui-dist@5.33.2/swagger-ui-bundle.js',
    'sha384-gNnUp++d7WqR9OKX4OMoUmV2w0JxEs1uu6NPXgcynGClIX5DOu6ol4Lp+lINyZcJ'],
]);

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

console.log('\n── external browser dependencies: pinning + SRI ──\n');

test('at least one external subresource is found (the scanner is not silently empty)', () => {
  assert(REFS.length > 0, `no external <script>/<link> found across ${SURFACES.length} scanned files`);
});

test('every external subresource is pinned to an exact version', () => {
  const bad = REFS.map((r) => [r, pinProblem(r.url)]).filter(([, p]) => p);
  assert.strictEqual(bad.length, 0,
    'not pinned:\n' + bad.map(([r, p]) => `    ${where(r)}\n      → ${p}`).join('\n'));
});

test('every external subresource carries a well-formed integrity hash', () => {
  const bad = REFS.filter((r) => !r.attrs.integrity || !SRI_RE.test(r.attrs.integrity.trim()));
  assert.strictEqual(bad.length, 0,
    'missing or malformed integrity:\n' +
    bad.map((r) => `    ${where(r)}\n      → integrity=${JSON.stringify(r.attrs.integrity)}`).join('\n'));
});

test('every external subresource sets crossorigin=anonymous', () => {
  // Without it the response is opaque, the browser cannot compare the digest,
  // and the integrity attribute is ignored rather than enforced.
  const bad = REFS.filter((r) => (r.attrs.crossorigin || '').trim().toLowerCase() !== 'anonymous');
  assert.strictEqual(bad.length, 0,
    'missing or wrong crossorigin:\n' +
    bad.map((r) => `    ${where(r)}\n      → crossorigin=${JSON.stringify(r.attrs.crossorigin)}`).join('\n'));
});

test('the same URL never ships under two different hashes', () => {
  const byUrl = new Map();
  for (const r of REFS) {
    const list = byUrl.get(r.url) || [];
    list.push(r);
    byUrl.set(r.url, list);
  }
  const split = [...byUrl.entries()]
    .filter(([, list]) => new Set(list.map((r) => (r.attrs.integrity || '').trim())).size > 1);
  assert.strictEqual(split.length, 0,
    'one URL, several hashes (at most one can be right):\n' +
    split.map(([url, list]) => `    ${url}\n` +
      list.map((r) => `      ${r.file}:${r.line} ${r.attrs.integrity}`).join('\n')).join('\n'));
});

test('every external subresource matches its reviewed digest', () => {
  const bad = REFS.filter((r) => (r.attrs.integrity || '').trim() !== REVIEWED_DIGESTS.get(r.url));
  assert.strictEqual(bad.length, 0,
    'integrity differs from the reviewed digest (or the URL has none):\n' +
    bad.map((r) => `    ${where(r)}\n      → integrity=${JSON.stringify(r.attrs.integrity)}` +
      `\n      → reviewed =${JSON.stringify(REVIEWED_DIGESTS.get(r.url))}`).join('\n'));
});

test('every reviewed digest is still in use (no stale pins)', () => {
  const live = new Set(REFS.map((r) => r.url));
  const stale = [...REVIEWED_DIGESTS.keys()].filter((u) => !live.has(u));
  assert.strictEqual(stale.length, 0, 'REVIEWED_DIGESTS lists URLs no page loads:\n    ' + stale.join('\n    '));
});

test('<link> rels that fetch nothing executable are not held to the SRI rule', () => {
  // preconnect / dns-prefetch open a connection, canonical / icon / alternate
  // are not scripts or stylesheets; none of them can carry integrity, so a
  // demand for it would be unsatisfiable.
  const fixture = [
    '<link rel="preconnect" href="https://unpkg.com">',
    '<link rel="dns-prefetch" href="//unpkg.com">',
    '<link rel="canonical" href="https://example.org/">',
    '<link rel="icon" href="https://example.org/favicon.ico">',
    '<link href="https://example.org/no-rel">',
  ].join('\n');
  const got = refsInSource('fixture.html', fixture).map((r) => r.attrs.rel);
  assert.deepStrictEqual(got, [], 'non-subresource <link> treated as a subresource: ' + JSON.stringify(got));
});

test('<link> rels that load script or style are still held to the SRI rule', () => {
  const fixture = [
    '<link rel="stylesheet" href="https://unpkg.com/a@1/a.css">',
    '<link rel="alternate stylesheet" href="https://unpkg.com/a@1/b.css">',
    '<link rel="modulepreload" href="https://unpkg.com/a@1/c.mjs">',
    '<link rel="preload" as="script" href="https://unpkg.com/a@1/d.js">',
    '<link rel="preload" as="style" href="https://unpkg.com/a@1/e.css">',
    '<link rel="PRELOAD" as="Script" href="//unpkg.com/a@1/f.js">',
    '<script src="https://unpkg.com/a@1/g.js"></script>',
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
    "importScripts('https://unpkg.com/x@1/w.js');",
    "await import('https://unpkg.com/x@1/m.mjs');",
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

test('public/index.html still loads Chart.js and Leaflet.heat', () => {
  // Guards against "pinning" something by deleting it: #336 must not change
  // shipped behaviour, so the analytics and heatmap dependencies stay.
  const index = REFS.filter((r) => r.file === 'public/index.html').map((r) => r.url);
  assert(index.some((u) => /\/chart\.js@/.test(u)), 'Chart.js is gone from public/index.html');
  assert(index.some((u) => /\/leaflet\.heat@/.test(u)), 'Leaflet.heat is gone from public/index.html');
  assert(index.some((u) => /\/leaflet@/.test(u)), 'Leaflet is gone from public/index.html');
});

test('cmd/server/openapi.go Swagger UI assets are pinned, hashed and crossorigin', () => {
  // The same rule, asserted against the Go-served page on its own, so the
  // Swagger UI surface cannot pass merely because the HTML files do.
  const swagger = REFS.filter((r) => r.file === 'cmd/server/openapi.go');
  assert.strictEqual(swagger.length, 2, `expected 2 external Swagger UI refs, got ${swagger.length}`);
  for (const r of swagger) {
    assert(/\/swagger-ui-dist@\d+\.\d+\.\d+\//.test(r.url), `not pinned: ${where(r)}`);
    assert(SRI_RE.test((r.attrs.integrity || '').trim()), `no integrity: ${where(r)}`);
    assert.strictEqual((r.attrs.crossorigin || '').toLowerCase(), 'anonymous', `no crossorigin: ${where(r)}`);
  }
});

test('documentation records how to update a pinned dependency', () => {
  const doc = path.join(ROOT, 'docs/browser-dependencies.md');
  assert(fs.existsSync(doc), 'docs/browser-dependencies.md is missing');
  const text = fs.readFileSync(doc, 'utf8');
  assert(/openssl dgst -sha384/.test(text), 'the hash recomputation command is not documented');
  // Every pinned URL must be listed, so the inventory cannot drift from reality.
  for (const url of new Set(REFS.map((r) => r.url))) {
    assert(text.includes(url), `docs/browser-dependencies.md does not list ${url}`);
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

test('documentation names every place a reviewed digest lives', () => {
  const text = fs.readFileSync(path.join(ROOT, 'docs/browser-dependencies.md'), 'utf8');
  for (const name of ['REVIEWED_DIGESTS', 'wantSwaggerUIAssets']) {
    assert(text.includes('`' + name + '`'), `docs/browser-dependencies.md does not mention \`${name}\``);
  }
});

test('documentation states the <link> rel scope and the dynamic-loader rule', () => {
  const text = fs.readFileSync(path.join(ROOT, 'docs/browser-dependencies.md'), 'utf8');
  assert(/rel="preconnect"/.test(text) && /rel="modulepreload"/.test(text),
    'docs/browser-dependencies.md does not say which <link> rel values are checked');
  assert(/createElement\('script'\)/.test(text),
    'docs/browser-dependencies.md does not document the dynamic script injection rule');
});

test('documentation keeps the local-hosting evaluation, not only the #336 pull request', () => {
  const text = fs.readFileSync(path.join(ROOT, 'docs/browser-dependencies.md'), 'utf8');
  assert(/^## Local hosting/m.test(text), 'docs/browser-dependencies.md has no "## Local hosting" section');
});

test('test-issue-336-external-asset-pinning.js is registered exactly once in test-all.sh', () => {
  const sh = fs.readFileSync(path.join(ROOT, 'test-all.sh'), 'utf8');
  const lines = sh.split('\n').filter((l) => l.trim() === 'run test-issue-336-external-asset-pinning.js');
  assert.strictEqual(lines.length, 1,
    'expected exactly one registration line in test-all.sh, got ' + lines.length);
});

console.log(`\n${passed} passed, ${failed} failed`);
process.exit(failed ? 1 : 0);
