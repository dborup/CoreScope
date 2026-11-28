// Guard for #454: every static top-nav route in public/index.html must be
// reachable on phones, i.e. present in the bottom-nav TABS or MORE_ROUTES.
// Parses the real files; no hand-maintained route list.
'use strict';
const fs = require('fs');
const path = require('path');
const assert = require('assert');

const html = fs.readFileSync(path.join(__dirname, 'public/index.html'), 'utf8');
const js = fs.readFileSync(path.join(__dirname, 'public/bottom-nav.js'), 'utf8');

function topNavRoutes(src) {
  const out = [];
  const re = /<a\b[^>]*\bclass="[^"]*\bnav-link\b[^"]*"[^>]*>/g;
  let m;
  while ((m = re.exec(src))) {
    const r = /\bdata-route="([^"]+)"/.exec(m[0]);
    if (r) out.push(r[1]);
  }
  return out;
}

function arrayRoutes(src, name) {
  const m = new RegExp('var ' + name + ' = \\[([\\s\\S]*?)\\n  \\];').exec(src);
  if (!m) throw new Error('could not find ' + name + ' in bottom-nav.js');
  const out = [];
  const re = /route:\s*'([^']+)'/g;
  let r;
  while ((r = re.exec(m[1]))) out.push(r[1]);
  return out;
}

// Routes in the top-nav that the bottom nav does not surface.
function unreachable(htmlSrc, jsSrc) {
  const reachable = new Set(arrayRoutes(jsSrc, 'TABS').concat(arrayRoutes(jsSrc, 'MORE_ROUTES')));
  return topNavRoutes(htmlSrc).filter(function (r) { return !reachable.has(r); });
}

let n = 0;
function t(name, fn) { fn(); n++; console.log('  ✓ ' + name); }

t('parser finds the real top-nav routes', function () {
  const r = topNavRoutes(html);
  assert.ok(r.length >= 10, 'expected >=10 top-nav routes, got ' + r.length);
  assert.ok(r.includes('packets'));
});

t('every static top-nav route is in TABS or MORE_ROUTES (#454)', function () {
  assert.deepStrictEqual(unreachable(html, js), []);
});

t('regions precedes nodes in MORE_ROUTES (top-nav order)', function () {
  const more = arrayRoutes(js, 'MORE_ROUTES');
  assert.ok(more.includes('regions'), 'regions missing');
  assert.ok(more.indexOf('regions') < more.indexOf('nodes'));
});

t('guard detects a top-nav route missing from the bottom nav', function () {
  const extra = html.replace('</nav>', '<a href="#/newpage" class="nav-link" data-route="newpage">N</a></nav>');
  assert.notStrictEqual(extra, html, 'test fixture injection failed');
  assert.deepStrictEqual(unreachable(extra, js), ['newpage']);
});

t('guard detects regions removed from MORE_ROUTES', function () {
  const mutated = js.replace(/\n\s*\{ route: 'regions'[^\n]*/, '');
  assert.notStrictEqual(mutated, js);
  assert.deepStrictEqual(unreachable(html, mutated), ['regions']);
});

console.log(n + ' passed');
