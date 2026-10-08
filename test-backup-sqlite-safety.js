'use strict';
// Execute the shipped operator script and the exact restore block from the
// deployment guide. All Docker calls go to a private executable mock; no daemon,
// container, network, production path or real database is contacted.
const assert = require('assert');
const fs = require('fs');
const os = require('os');
const path = require('path');
const { spawnSync } = require('child_process');

const root = __dirname;
let passed = 0;
let failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('PASS ' + name); }
  catch (err) { failed++; console.error('FAIL ' + name + ': ' + err.message); }
}

const mockSource = `#!/usr/bin/env node
const fs = require('fs'), path = require('path');
const statePath = process.env.MOCK_STATE;
const state = JSON.parse(fs.readFileSync(statePath, 'utf8'));
const args = process.argv.slice(2), op = args[0];
state.calls.push(args);
function done(code=0) { fs.writeFileSync(statePath, JSON.stringify(state)); process.exit(code); }
if (op === 'inspect') {
  if (process.env.MOCK_FAIL === 'inspect') done(21);
  console.log(args[2].includes('.Image') ? 'sha256:local-test-image' : String(state.running));
  done();
}
if (op === 'stop') {
  if (process.env.MOCK_FAIL === 'stop') done(22);
  state.running = false;
  if (process.env.MOCK_SIGNAL === 'stop') {
    fs.writeFileSync(statePath, JSON.stringify(state));
    process.kill(process.ppid, 'SIGTERM'); process.exit(0);
  }
  done();
}
if (op === 'start') {
  if (process.env.MOCK_RESTART_FAIL === '1') done(24);
  if (process.env.MOCK_SIGNAL === 'start' && !state.startSignalSent) {
    state.startSignalSent = true;
    fs.writeFileSync(statePath, JSON.stringify(state));
    process.kill(process.ppid, 'SIGTERM'); process.exit(0);
  }
  state.running = true; done();
}
if (op === 'run') {
  if (process.env.MOCK_FAIL === 'clear') done(25);
  if (!args.includes('--pull=never') || !args.includes('none')) done(90);
  const shell = args[args.length-1];
  for (const file of ['meshcore.db-wal', 'meshcore.db-shm', 'ping_scores_history.db-wal', 'ping_scores_history.db-shm']) {
    if (!shell.includes('/app/data/' + file)) done(91);
    fs.rmSync(path.join(state.data, file), {force:true});
  }
  done();
}
if (op === 'cp') {
  const src = args[1], dst = args[2];
  if (src.includes(':/app/data/')) {
    if (process.env.MOCK_SIGNAL === 'copy') {
      fs.mkdirSync(dst, {recursive:true});
      fs.writeFileSync(path.join(dst, 'partial.txt'), 'partial');
      fs.writeFileSync(statePath, JSON.stringify(state));
      process.kill(process.ppid, 'SIGTERM'); process.exit(0);
    }
    if (process.env.MOCK_FAIL === 'copy-main' || process.env.MOCK_FAIL === 'copy-wal') {
      fs.mkdirSync(dst, {recursive:true});
      fs.copyFileSync(path.join(state.data, 'meshcore.db'), path.join(dst, 'meshcore.db'));
      console.error(process.env.MOCK_FAIL === 'copy-wal' ? 'WAL COPY FAILED: ENOSPC' : 'MAIN COPY FAILED');
      done(23);
    }
    fs.cpSync(state.data, dst, {recursive:true}); done();
  }
  if (!dst.includes(':/app/data/')) done(92);
  const name = path.basename(dst);
  if (process.env.MOCK_FAIL === 'restore-main' && name === 'meshcore.db') done(26);
  if (process.env.MOCK_FAIL === 'restore-wal' && name.endsWith('-wal')) done(27);
  fs.copyFileSync(src, path.join(state.data, name)); done();
}
done(93);
`;

function fixture(fn, options = {}) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'corescope-backup-test-'));
  const data = path.join(dir, 'container-data');
  const bin = path.join(dir, 'bin');
  const snapshots = path.join(dir, 'snapshots');
  const incoming = path.join(dir, 'incoming');
  fs.mkdirSync(data); fs.mkdirSync(bin); fs.mkdirSync(snapshots); fs.mkdirSync(incoming);
  for (const db of ['meshcore', 'ping_scores_history']) {
    fs.writeFileSync(path.join(data, db + '.db'), 'old-' + db);
    fs.writeFileSync(path.join(incoming, db + '.db'), 'new-' + db);
    fs.writeFileSync(path.join(data, db + '.db-shm'), 'scratch');
    if (options.wal) {
      fs.writeFileSync(path.join(data, db + '.db-wal'), 'old-wal-' + db);
      fs.writeFileSync(path.join(incoming, db + '.db-wal'), 'new-wal-' + db);
    }
  }
  fs.writeFileSync(path.join(data, 'config.json'), 'PRIVATE_CONFIG_MUST_NOT_BE_PUBLISHED');
  fs.writeFileSync(path.join(data, 'broker.env'), 'PRIVATE_ENV_MUST_NOT_BE_PUBLISHED');
  fs.mkdirSync(path.join(data, 'extra'));
  fs.writeFileSync(path.join(data, 'extra', 'other.txt'), 'unrelated');
  fs.writeFileSync(path.join(bin, 'docker'), mockSource, {mode:0o700});
  const statePath = path.join(dir, 'state.json');
  fs.writeFileSync(statePath, JSON.stringify({data, running:options.running !== false, calls:[]}));
  const env = {...process.env, PATH:bin + path.delimiter + process.env.PATH, MOCK_STATE:statePath,
    CONTAINER:'corescope-test', RESTORE_DIR:incoming};
  const f = {dir, data, incoming, snapshots, env,
    state:() => JSON.parse(fs.readFileSync(statePath, 'utf8')),
    backup:(extra={}) => spawnSync('sh', [path.join(root, 'scripts/backup-sqlite.sh'), 'corescope-test', snapshots],
      {env:{...env,...extra}, encoding:'utf8', timeout:10000}),
    restore:(extra={}) => {
      const docs = fs.readFileSync(path.join(root, 'docs/deployment.md'), 'utf8');
      const section = docs.split('<!-- tested-sqlite-restore -->')[1];
      assert(section, 'restore instructions need an executable test marker');
      const match = section.match(/```sh\n([\s\S]*?)\n```/);
      assert(match, 'missing tested restore commands');
      return spawnSync('sh', ['-c', match[1]], {env:{...env,...extra}, encoding:'utf8', timeout:10000});
    }};
  try { fn(f); } finally { fs.rmSync(dir, {recursive:true,force:true}); }
}

const calls = (f, op) => f.state().calls.filter(c => c[0] === op);
function assertNoSnapshots(f) {
  assert.deepStrictEqual(fs.readdirSync(f.snapshots), [], 'failed backup published or left staging files');
}

for (const wal of [false, true]) test('backup captures both DBs and ' + (wal ? 'existing' : 'absent') + ' WALs', () => fixture(f => {
  const result = f.backup(); assert.strictEqual(result.status, 0, result.stderr);
  const snapshot = result.stdout.trim();
  assert(snapshot.startsWith(fs.realpathSync(f.snapshots) + path.sep));
  const want = ['meshcore.db', 'ping_scores_history.db'];
  if (wal) want.push('meshcore.db-wal', 'ping_scores_history.db-wal');
  assert.deepStrictEqual(fs.readdirSync(snapshot).sort(), want.sort());
  assert.strictEqual(fs.statSync(snapshot).mode & 0o777, 0o700);
  for (const db of ['meshcore','ping_scores_history']) {
    assert.strictEqual(fs.readFileSync(path.join(snapshot, db + '.db'),'utf8'), 'old-' + db);
    if (wal) assert.strictEqual(fs.readFileSync(path.join(snapshot, db + '.db-wal'),'utf8'), 'old-wal-' + db);
  }
  assert.strictEqual(f.state().running, true);
  assert.strictEqual(calls(f,'stop').length, 1); assert.strictEqual(calls(f,'start').length, 1);
}, {wal}));

for (const failure of ['copy-main','copy-wal']) test(failure + ' preserves error and restarts without publishing', () => fixture(f => {
  const result = f.backup({MOCK_FAIL:failure}); assert.strictEqual(result.status, 23, result.stderr);
  assert.strictEqual(f.state().running, true); assert.strictEqual(calls(f,'start').length, 1);
  assertNoSnapshots(f);
}, {wal:true}));

test('restart failure is fatal and no completed backup is published', () => fixture(f => {
  const result = f.backup({MOCK_RESTART_FAIL:'1'}); assert.strictEqual(result.status, 24, result.stderr);
  assert.match(result.stderr, /restart/i); assert.strictEqual(f.state().running, false);
  assertNoSnapshots(f);
}));

test('copy and restart errors preserve the original copy status', () => fixture(f => {
  const result = f.backup({MOCK_FAIL:'copy-wal', MOCK_RESTART_FAIL:'1'});
  assert.strictEqual(result.status, 23); assert.match(result.stderr, /restart/i);
  assertNoSnapshots(f);
}));

test('failed stop never copies data and returns its original status', () => fixture(f => {
  const result = f.backup({MOCK_FAIL:'stop'}); assert.strictEqual(result.status, 22);
  assert.strictEqual(calls(f,'cp').length, 0); assert.strictEqual(calls(f,'start').length, 1);
  assertNoSnapshots(f);
}));

test('stop and restart errors preserve the original stop status', () => fixture(f => {
  const result = f.backup({MOCK_FAIL:'stop', MOCK_RESTART_FAIL:'1'});
  assert.strictEqual(result.status, 22); assert.match(result.stderr, /restart/i);
  assertNoSnapshots(f);
}));

for (const phase of ['stop','copy']) test('SIGTERM during ' + phase + ' restarts and does not publish', () => fixture(f => {
  const result = f.backup({MOCK_SIGNAL:phase}); assert.strictEqual(result.status, 143, result.stderr);
  assert.strictEqual(calls(f,'start').length, 1); assert.strictEqual(f.state().running, true);
  assertNoSnapshots(f);
}));

test('SIGTERM during final restart retries recovery and does not publish', () => fixture(f => {
  const result = f.backup({MOCK_SIGNAL:'start'}); assert.strictEqual(result.status, 143, result.stderr);
  assert.strictEqual(calls(f,'start').length, 2); assert.strictEqual(f.state().running, true);
  assertNoSnapshots(f);
}));

test('failed inspect never stops the container', () => fixture(f => {
  assert.strictEqual(f.backup({MOCK_FAIL:'inspect'}).status, 21);
  assert.strictEqual(calls(f,'stop').length, 0); assertNoSnapshots(f);
}));

test('backup of an already stopped container leaves it stopped', () => fixture(f => {
  const result = f.backup(); assert.strictEqual(result.status, 0, result.stderr);
  assert.strictEqual(calls(f,'stop').length, 0); assert.strictEqual(calls(f,'start').length, 0);
  assert.strictEqual(f.state().running, false);
}, {running:false}));

test('repeated backups have fresh destinations and no stale WALs', () => fixture(f => {
  const first = f.backup(); assert.strictEqual(first.status, 0, first.stderr);
  for (const db of ['meshcore','ping_scores_history']) fs.rmSync(path.join(f.data, db + '.db-wal'));
  const second = f.backup(); assert.strictEqual(second.status, 0, second.stderr);
  assert.notStrictEqual(first.stdout, second.stdout);
  assert.deepStrictEqual(fs.readdirSync(second.stdout.trim()).sort(), ['meshcore.db','ping_scores_history.db']);
}, {wal:true}));

test('missing mandatory database fails before publication and restarts', () => fixture(f => {
  fs.rmSync(path.join(f.data, 'ping_scores_history.db'));
  const result = f.backup(); assert.strictEqual(result.status, 1);
  assert.strictEqual(calls(f,'start').length, 1);
  assert.strictEqual(f.state().running, true); assertNoSnapshots(f);
}));

for (const wal of [false,true]) test('documented restore installs matching pair, ' + (wal ? 'with WALs' : 'without stale WALs'), () => fixture(f => {
  if (!wal) for (const db of ['meshcore','ping_scores_history']) fs.writeFileSync(path.join(f.data, db + '.db-wal'), 'STALE');
  const result = f.restore(); assert.strictEqual(result.status, 0, result.stderr);
  for (const db of ['meshcore','ping_scores_history']) {
    assert.strictEqual(fs.readFileSync(path.join(f.data, db + '.db'),'utf8'), 'new-' + db);
    assert.strictEqual(fs.existsSync(path.join(f.data, db + '.db-shm')), false);
    assert.strictEqual(fs.existsSync(path.join(f.data, db + '.db-wal')), wal);
    if (wal) assert.strictEqual(fs.readFileSync(path.join(f.data, db + '.db-wal'),'utf8'), 'new-wal-' + db);
  }
  assert.strictEqual(fs.readFileSync(path.join(f.data,'config.json'),'utf8'), 'PRIVATE_CONFIG_MUST_NOT_BE_PUBLISHED');
  assert.strictEqual(f.state().running, true);
}, {wal}));

for (const [failure, status] of [['clear',25],['restore-main',26],['restore-wal',27]]) test('restore ' + failure + ' aborts and leaves the service stopped', () => fixture(f => {
  const result = f.restore({MOCK_FAIL:failure}); assert.strictEqual(result.status, status, result.stderr);
  assert.strictEqual(calls(f,'start').length, 0); assert.strictEqual(f.state().running, false);
}, {wal:true}));

test('restore prevalidates both main files before stopping', () => fixture(f => {
  fs.rmSync(path.join(f.incoming,'ping_scores_history.db'));
  const result = f.restore(); assert.strictEqual(result.status, 1, result.stderr);
  assert.strictEqual(calls(f,'stop').length, 0); assert.strictEqual(f.state().running, true);
}));

test('invalid container argument is rejected without Docker calls', () => fixture(f => {
  const result = spawnSync('sh', [path.join(root,'scripts/backup-sqlite.sh'), '-bad', f.snapshots],
    {env:f.env, encoding:'utf8', timeout:10000});
  assert.strictEqual(result.status, 2); assert.deepStrictEqual(f.state().calls, []);
}));

test('restore rejects a symlink WAL before stopping', () => fixture(f => {
  fs.symlinkSync(path.join(f.data,'config.json'), path.join(f.incoming,'meshcore.db-wal'));
  const result = f.restore(); assert.strictEqual(result.status, 1, result.stderr);
  assert.strictEqual(calls(f,'stop').length, 0);
}));

test('restore inspect failure never stops or writes', () => fixture(f => {
  assert.strictEqual(f.restore({MOCK_FAIL:'inspect'}).status, 21);
  assert.strictEqual(calls(f,'stop').length, 0); assert.strictEqual(calls(f,'cp').length, 0);
}));

test('restore stop failure never installs or starts', () => fixture(f => {
  assert.strictEqual(f.restore({MOCK_FAIL:'stop'}).status, 22);
  assert.strictEqual(calls(f,'cp').length, 0); assert.strictEqual(calls(f,'run').length, 0);
  assert.strictEqual(calls(f,'start').length, 0);
}));

test('restore restart failure is surfaced', () => fixture(f => {
  const result = f.restore({MOCK_RESTART_FAIL:'1'}); assert.strictEqual(result.status, 24);
  assert.strictEqual(f.state().running, false);
}));

console.log(`${passed} passed, ${failed} failed`);
process.exitCode = failed === 0 ? 0 : 1;
