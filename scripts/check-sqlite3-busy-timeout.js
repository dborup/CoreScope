#!/usr/bin/env node
/*
 * scripts/check-sqlite3-busy-timeout.js — issue #606
 *
 * Walks .js files and asserts that every process call invoking the
 * `sqlite3` CLI (execFileSync/execSync/spawnSync/spawn) sets a busy
 * timeout, either via `-cmd`/`.timeout N` CLI args or an embedded
 * `PRAGMA busy_timeout=N` statement.
 *
 * Why: the sqlite3 CLI has no busy timeout by default. An E2E suite that
 * shells out to it while a real server/ingestor process holds a write
 * lock on the same DB fails at once with "database is locked" instead of
 * waiting (#606), which is flaky rather than deterministic.
 *
 * Matching is a heuristic, not a JS parser: it finds `fn('sqlite3'` call
 * heads, then walks forward counting '(' / ')' characters (including
 * ones inside string/SQL arguments — SQL statements here keep their
 * parens balanced, so this still lands on the real matching close-paren)
 * to capture the full call's argument text, and checks that text for a
 * timeout marker.
 *
 * Exit code: 0 = clean, 1 = one or more sqlite3 CLI calls missing a busy
 * timeout (with file:line locations), 2 = usage/environment error.
 *
 * Usage:
 *   node scripts/check-sqlite3-busy-timeout.js          # lints the repo
 *   node scripts/check-sqlite3-busy-timeout.js --dir x  # lint a different directory
 */
'use strict';
const fs = require('fs');
const path = require('path');

let dir = path.join(__dirname, '..');
for (let i = 2; i < process.argv.length; i++) {
  if (process.argv[i] === '--dir' && process.argv[i + 1]) { dir = process.argv[++i]; }
}

if (!fs.existsSync(dir)) {
  console.error('check-sqlite3-busy-timeout: directory not found: ' + dir);
  process.exit(2);
}

const SKIP_DIRS = new Set(['node_modules', 'vendor', '.git']);
// Its own self-test embeds deliberately-bad "no timeout" call fixtures as
// literal JS source strings (written out to a tmp dir and linted there);
// scanned here in place they'd read as real violations.
const SKIP_FILES = new Set(['test-check-sqlite3-busy-timeout.js']);
function walk(root) {
  const out = [];
  const stack = [root];
  while (stack.length) {
    const cur = stack.pop();
    let entries;
    try { entries = fs.readdirSync(cur, { withFileTypes: true }); }
    catch (e) { continue; }
    for (const ent of entries) {
      const full = path.join(cur, ent.name);
      if (ent.isDirectory()) {
        if (SKIP_DIRS.has(ent.name) || ent.name.startsWith('_wt-')) continue;
        stack.push(full);
      } else if (ent.isFile() && path.extname(ent.name) === '.js' && !SKIP_FILES.has(ent.name)) {
        out.push(full);
      }
    }
  }
  return out;
}

// Matches the head of a process call whose first argument is a string
// starting with "sqlite3" (covers both `fn('sqlite3', [...])` and
// `fn('sqlite3 ' + rest)` shell-string forms).
const CALL_HEAD_RE = /\b(?:execFileSync|execSync|execFile|exec|spawnSync|spawn)\s*\(\s*[`'"]sqlite3\b/g;
const TIMEOUT_RE = /\.timeout\b|busy_timeout/i;

function findMatchingClose(src, openParenIdx) {
  let depth = 0;
  for (let i = openParenIdx; i < src.length; i++) {
    if (src[i] === '(') depth++;
    else if (src[i] === ')') {
      depth--;
      if (depth === 0) return i;
    }
  }
  return -1;
}

const violations = []; // { file, line }
let calls = 0;

function scanFile(f) {
  const src = fs.readFileSync(f, 'utf8');
  CALL_HEAD_RE.lastIndex = 0;
  let m;
  while ((m = CALL_HEAD_RE.exec(src)) !== null) {
    const openParenIdx = src.indexOf('(', m.index);
    if (openParenIdx === -1) continue;
    const closeParenIdx = findMatchingClose(src, openParenIdx);
    const span = closeParenIdx === -1 ? src.slice(openParenIdx) : src.slice(openParenIdx, closeParenIdx + 1);
    calls++;
    if (!TIMEOUT_RE.test(span)) {
      const line = src.slice(0, m.index).split('\n').length;
      violations.push({ file: f, line });
    }
  }
}

for (const f of walk(dir)) scanFile(f);

if (violations.length) {
  console.error('check-sqlite3-busy-timeout: ' + violations.length + ' sqlite3 CLI call(s) missing a busy timeout:');
  for (const v of violations) console.error('  ' + v.file + ':' + v.line);
  console.error("Fix: add ['-cmd', '.timeout 5000', ...] to the call's args, or embed a 'PRAGMA busy_timeout=5000;' statement.");
  process.exit(1);
}
console.log('check-sqlite3-busy-timeout: OK — ' + calls + ' sqlite3 CLI call(s), 0 missing a busy timeout.');
