#!/usr/bin/env node
/*
 * scripts/test-check-sqlite3-busy-timeout.js — gates scripts/check-sqlite3-busy-timeout.js
 *
 * #606: confirms the lint catches a sqlite3 CLI call with no busy timeout,
 * accepts both ways of setting one (-cmd '.timeout N' args, or an embedded
 * PRAGMA busy_timeout statement), and still matches a call whose argument
 * list spans multiple lines (the shape used by the real E2E helpers, e.g.
 * a schema-extraction call piping one sqlite3 invocation's output into
 * another).
 */
'use strict';
const fs = require('fs');
const os = require('os');
const path = require('path');
const { spawnSync } = require('child_process');

const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'check-sqlite3-busy-timeout-test-'));
const lintPath = path.join(__dirname, 'check-sqlite3-busy-timeout.js');
let pass = 0, fail = 0;

function check(name, cond, info) {
  if (cond) { console.log('PASS', name); pass++; }
  else { console.error('FAIL', name, info || ''); fail++; }
}

function writeOnly(name, content) {
  for (const f of fs.readdirSync(tmp)) fs.unlinkSync(path.join(tmp, f));
  fs.writeFileSync(path.join(tmp, name), content);
}

try {
  // 1. A call with a busy timeout via -cmd/.timeout args is clean.
  writeOnly('ok.js',
    "const { execFileSync } = require('child_process');\n" +
    "execFileSync('sqlite3', ['-cmd', '.timeout 5000', db, sql]);\n");
  let r = spawnSync('node', [lintPath, '--dir', tmp], { encoding: 'utf8' });
  check('timeout via -cmd args -> exit 0', r.status === 0, r.stderr || r.stdout);

  // 2. The same call with no timeout at all MUST be flagged, by file:line.
  writeOnly('broken.js',
    "const { execFileSync } = require('child_process');\n" +
    "execFileSync('sqlite3', [db, sql]);\n");
  r = spawnSync('node', [lintPath, '--dir', tmp], { encoding: 'utf8' });
  check('no timeout -> exit 1', r.status === 1, 'got status=' + r.status);
  check('error names the offending file:line',
    /broken\.js:2/.test(r.stderr), 'stderr=' + r.stderr);

  // 3. An embedded PRAGMA busy_timeout statement is an accepted alternative.
  writeOnly('pragma.js',
    "const { execFileSync } = require('child_process');\n" +
    "execFileSync('sqlite3', [db], { input: 'PRAGMA busy_timeout=5000;\\nINSERT INTO t VALUES (1);' });\n");
  r = spawnSync('node', [lintPath, '--dir', tmp], { encoding: 'utf8' });
  check('embedded PRAGMA busy_timeout -> exit 0', r.status === 0, r.stderr || r.stdout);

  // 4. A call whose args span multiple lines, with the timeout on a later
  //    line, must still be matched as ONE call (regression: a naive
  //    same-line-only scan would misreport this as missing).
  writeOnly('multiline.js',
    "const { execFileSync } = require('child_process');\n" +
    "const schema = execFileSync('sqlite3', ['-cmd', '.timeout 5000',\n" +
    "  path.join(ROOT, 'fixture.db'),\n" +
    "  \"SELECT sql FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'\"]);\n");
  r = spawnSync('node', [lintPath, '--dir', tmp], { encoding: 'utf8' });
  check('multi-line call with timeout -> exit 0', r.status === 0, r.stderr || r.stdout);

  // 5. A multi-line call with NO timeout must still be flagged (same span
  //    logic, opposite outcome).
  writeOnly('multiline-broken.js',
    "const { execFileSync } = require('child_process');\n" +
    "const schema = execFileSync('sqlite3', [\n" +
    "  path.join(ROOT, 'fixture.db'),\n" +
    "  \"SELECT sql FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'\"]);\n");
  r = spawnSync('node', [lintPath, '--dir', tmp], { encoding: 'utf8' });
  check('multi-line call without timeout -> exit 1', r.status === 1, 'got status=' + r.status);
  check('multi-line violation names line 2',
    /multiline-broken\.js:2/.test(r.stderr), 'stderr=' + r.stderr);
} finally {
  for (const f of fs.readdirSync(tmp)) fs.unlinkSync(path.join(tmp, f));
  fs.rmdirSync(tmp);
}

console.log('\n=== test-check-sqlite3-busy-timeout: ' + pass + ' pass, ' + fail + ' fail ===');
process.exit(fail > 0 ? 1 : 0);
