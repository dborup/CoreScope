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
const SURFACES = [
  ...filesUnder('public', '.html'),
  ...filesUnder('tools', '.html'),
  ...filesUnder('cmd', '.go'),
  ...filesUnder('internal', '.go'),
];

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

/** Every off-origin subresource tag across the scanned surfaces. */
function externalRefs() {
  const refs = [];
  for (const file of SURFACES) {
    const src = fs.readFileSync(path.join(ROOT, file), 'utf8');
    let m;
    TAG_RE.lastIndex = 0;
    while ((m = TAG_RE.exec(src)) !== null) {
      const tag = m[1].toLowerCase();
      const a = attrs(m[2]);
      const url = tag === 'script' ? a.src : a.href;
      // Relative paths are same-origin and carry no third-party risk; a
      // protocol-relative URL is external and must not slip through.
      if (!url || !/^(https?:)?\/\//i.test(url)) continue;
      refs.push({ file, tag, url, attrs: a, line: src.slice(0, m.index).split('\n').length });
    }
  }
  return refs;
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

test('test-issue-336-external-asset-pinning.js is registered exactly once in test-all.sh', () => {
  const sh = fs.readFileSync(path.join(ROOT, 'test-all.sh'), 'utf8');
  const lines = sh.split('\n').filter((l) => l.trim() === 'run test-issue-336-external-asset-pinning.js');
  assert.strictEqual(lines.length, 1,
    'expected exactly one registration line in test-all.sh, got ' + lines.length);
});

console.log(`\n${passed} passed, ${failed} failed`);
process.exit(failed ? 1 : 0);
