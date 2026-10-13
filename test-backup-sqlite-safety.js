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

// Docker's daemon keeps stopping a container after the CLI is killed. The mock
// models that as a persisted deadline at which the container exits, whether
// or not any CLI process is still alive to observe it.
function settle(state) {
  if (state.stopAt && Date.now() >= state.stopAt) { state.running = false; delete state.stopAt; }
}
function which(tool) {
  const found = spawnSync('/bin/sh', ['-c', 'command -v "$1"', 'sh', tool], {encoding:'utf8'}).stdout.trim();
  assert(found.startsWith('/'), 'missing tool ' + tool); return found;
}
function sleep(ms) { if (ms > 0) Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms); }

const mockSource = `#!/usr/bin/env node
const fs = require('fs'), path = require('path');
const statePath = process.env.MOCK_STATE;
let state = JSON.parse(fs.readFileSync(statePath, 'utf8'));
const args = process.argv.slice(2), op = args[0];
${settle}
${sleep}
settle(state);
state.calls.push(args);
function save() { fs.writeFileSync(statePath, JSON.stringify(state)); }
function done(code=0) { save(); process.exit(code); }
// Like Docker's Go CLI: an inherited ignored SIGHUP stays ignored, but
// SIGINT/SIGTERM are re-enabled (signal.Notify) and kill it.
function groupSignal(signal, recovery) {
  state.groupSignals++; save();
  if (recovery && signal === 'SIGHUP') process.on(signal, () => {});
  process.kill(-state.pgid, signal);
}
// A repeated signal while recovery runs a start or inspect.
if (state.groupSignals > 0 && (process.env.MOCK_RECOVERY_SIGNAL_OPS || '').split(',').includes(op))
  groupSignal(process.env.MOCK_RECOVERY_SIGNAL, true);
if (op === 'compose') done();
if (op === 'inspect') {
  if (process.env.MOCK_FAIL === 'inspect') done(21);
  if (args.some(a => a.includes('.State.Health'))) { console.log('healthy'); done(); }
  console.log(args[2].includes('.Image') ? 'sha256:local-test-image' : String(state.running));
  done();
}
if (op === 'stop') {
  if (process.env.MOCK_FAIL === 'stop') done(22);
  const delay = Number(process.env.MOCK_STOP_DELAY_MS || 0);
  if (delay) {
    // Slow stop: the CLI waits for the daemon. A signal to the helper's
    // process group kills this CLI mid-wait; the container still exits.
    if (state.running && !state.stopAt) state.stopAt = Date.now() + delay;
    const first = state.groupSignals === 0;
    // The helper is the session leader (setsid -w), so its PID is its group.
    if (first) state.pgid = process.ppid;
    const signal = first ? process.env.MOCK_GROUP_SIGNAL : process.env.MOCK_RECOVERY_SIGNAL;
    if (signal && state.groupSignals < 1 + Number(process.env.MOCK_RECOVERY_SIGNALS || 1)) groupSignal(signal, !first);
    save();
    if (state.stopAt) sleep(state.stopAt - Date.now() + 5);
    state = JSON.parse(fs.readFileSync(statePath, 'utf8')); settle(state); process.exit(0);
  }
  if (process.env.MOCK_STOP_NOOP !== '1') state.running = false;
  if (process.env.MOCK_SIGNAL === 'stop') {
    fs.writeFileSync(statePath, JSON.stringify(state));
    if (process.env.MOCK_FILL_STDERR === '1' && !fs.existsSync(statePath + '.signalled')) {
      // Fill the helper's stderr pipe so its next write blocks.
      const fd = fs.openSync('/proc/self/fd/2', fs.constants.O_WRONLY | fs.constants.O_NONBLOCK);
      for (const size of [4096, 1]) {
        try { for (;;) fs.writeSync(fd, Buffer.alloc(size, 'x')); } catch (err) { if (err.code !== 'EAGAIN') throw err; }
      }
      fs.writeFileSync(statePath + '.signalled', '');
    }
    process.kill(process.ppid, process.env.MOCK_SIGNAL_NAME || 'SIGTERM'); process.exit(0);
  }
  done();
}
if (op === 'start') {
  if (process.env.MOCK_RESTART_FAIL === '1') done(24);
  if (process.env.MOCK_START_NOOP === '1') done();
  if (process.env.MOCK_PRECREATE_DEST) {
    // Something else takes this run's destination name before publication.
    const root = process.env.MOCK_BACKUP_ROOT;
    for (const name of fs.readdirSync(root).filter(n => n.startsWith('.corescope-backup-'))) {
      const destination = path.join(root, name.slice(1));
      if (process.env.MOCK_PRECREATE_DEST === 'symlink') fs.symlinkSync(process.env.MOCK_OUTSIDE, destination);
      else if (process.env.MOCK_PRECREATE_DEST === 'dangling') fs.symlinkSync(process.env.MOCK_OUTSIDE + '-missing', destination);
      else { fs.mkdirSync(destination); fs.writeFileSync(path.join(destination, 'sentinel'), 'keep'); }
    }
  }
  if (process.env.MOCK_SIGNAL === 'start' && !state.startSignalSent) {
    state.startSignalSent = true;
    fs.writeFileSync(statePath, JSON.stringify(state));
    process.kill(process.ppid, 'SIGTERM'); process.exit(0);
  }
  const delay = Number(process.env.MOCK_START_DELAY_MS || 0);
  if (delay) {
    // Slow start: the container runs only once the CLI returns.
    sleep(delay); state = JSON.parse(fs.readFileSync(statePath, 'utf8')); settle(state); state.calls.push(args);
  }
  // Like Docker, starting a still-running (even stopping) container is a no-op.
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
    state.copiedWhileRunning.push(state.running);
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
    fs.writeFileSync(path.join(data, db + '.db'), 'old-' + db, {mode:0o644});
    fs.writeFileSync(path.join(incoming, db + '.db'), 'new-' + db);
    fs.writeFileSync(path.join(data, db + '.db-shm'), 'scratch');
    if (options.wal) {
      fs.writeFileSync(path.join(data, db + '.db-wal'), 'old-wal-' + db, {mode:0o644});
      fs.writeFileSync(path.join(incoming, db + '.db-wal'), 'new-wal-' + db);
    }
  }
  fs.writeFileSync(path.join(data, 'config.json'), 'PRIVATE_CONFIG_MUST_NOT_BE_PUBLISHED');
  fs.writeFileSync(path.join(data, 'broker.env'), 'PRIVATE_ENV_MUST_NOT_BE_PUBLISHED');
  fs.mkdirSync(path.join(data, 'extra'));
  fs.writeFileSync(path.join(data, 'extra', 'other.txt'), 'unrelated');
  fs.writeFileSync(path.join(bin, 'docker'), mockSource, {mode:0o700});
  const statePath = path.join(dir, 'state.json');
  fs.writeFileSync(statePath, JSON.stringify({data, running:options.running !== false, calls:[],
    groupSignals:0, copiedWhileRunning:[]}));
  const env = {...process.env, PATH:bin + path.delimiter + process.env.PATH, MOCK_STATE:statePath,
    CONTAINER:'corescope-test', RESTORE_DIR:incoming, MOCK_BACKUP_ROOT:snapshots};
  const f = {dir, data, incoming, snapshots, env,
    state:() => JSON.parse(fs.readFileSync(statePath, 'utf8')),
    // Container state once any stop already in progress has completed.
    settledRunning:() => {
      const state = JSON.parse(fs.readFileSync(statePath, 'utf8'));
      if (state.stopAt) sleep(state.stopAt - Date.now() + 5);
      settle(state); return state.running;
    },
    backup:(extra={}, {target=snapshots, cwd=dir, group=false}={}) => {
      // group: run in a new session so the mock can signal the helper's whole
      // process group, like a terminal hangup, Ctrl-C or a supervisor would.
      const argv = ['sh', path.join(root, 'scripts/backup-sqlite.sh'), 'corescope-test', target];
      if (group) argv.unshift(which('setsid'), '-w');
      return spawnSync(argv[0], argv.slice(1), {env:{...env,...extra}, cwd, encoding:'utf8', timeout:10000});
    },
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
  for (const file of want) assert.strictEqual(fs.statSync(path.join(snapshot, file)).mode & 0o777, 0o600, file);
  // Private staging (raw /app/data copy incl. config/env files) is gone.
  assert.deepStrictEqual(fs.readdirSync(f.snapshots), [path.basename(snapshot)]);
  assert.deepStrictEqual(f.state().copiedWhileRunning, [false], 'copied while the container was running');
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

for (const [signal, status] of [['SIGHUP',129],['SIGINT',130],['SIGTERM',143]]) test(signal + ' to the process group during a slow stop restarts and fails', () => fixture(f => {
  const result = f.backup({MOCK_STOP_DELAY_MS:'300', MOCK_GROUP_SIGNAL:signal}, {group:true});
  assert.strictEqual(result.status, status, result.stderr);
  assert.strictEqual(f.settledRunning(), true, 'service left stopped after the interrupted stop');
  assert.match(result.stderr, new RegExp('ERROR: interrupted by ' + signal));
  assert.strictEqual(calls(f,'cp').length, 0); assertNoSnapshots(f);
}, {wal:true}));

// Repeated Ctrl-C or kills of the process group while recovery waits for the
// pending stop: the recovery CLI must not be in that group.
for (const [first, again, status] of [['SIGHUP','SIGHUP',129],['SIGHUP','SIGINT',129],
  ['SIGINT','SIGINT',130],['SIGTERM','SIGTERM',143]]) test('repeated group ' + first + '/' + again + ' does not abort recovery', () => fixture(f => {
  const result = f.backup({MOCK_STOP_DELAY_MS:'300', MOCK_GROUP_SIGNAL:first, MOCK_RECOVERY_SIGNAL:again,
    MOCK_RECOVERY_SIGNALS:'3'}, {group:true});
  assert.strictEqual(result.status, status, result.stderr);
  assert.strictEqual(f.settledRunning(), true, 'service left stopped after a repeated signal');
  assert.doesNotMatch(result.stderr, /restart failed/);
  assertNoSnapshots(f);
}));

test('repeated group signals during the recovery start and inspect do not abort it', () => fixture(f => {
  const result = f.backup({MOCK_STOP_DELAY_MS:'300', MOCK_GROUP_SIGNAL:'SIGINT', MOCK_RECOVERY_SIGNAL:'SIGINT',
    MOCK_RECOVERY_SIGNAL_OPS:'start,inspect'}, {group:true});
  assert.strictEqual(result.status, 130, result.stderr);
  assert.strictEqual(f.settledRunning(), true, 'service left stopped after a repeated signal');
  assert.doesNotMatch(result.stderr, /restart failed|not verified/);
  assertNoSnapshots(f);
}));

test('without setsid, recovery retries a stop killed by a repeated signal', () => fixture(f => {
  // A PATH with the helper's tools but no setsid(1), as on macOS.
  const tools = path.join(f.dir, 'tools'); fs.mkdirSync(tools);
  for (const tool of ['sh','mkdir','mktemp','date','chmod','rm','mv','node']) fs.symlinkSync(which(tool), path.join(tools, tool));
  // Two recovery signals: the third and last stop attempt is the one that holds.
  const result = f.backup({PATH:path.join(f.dir, 'bin') + path.delimiter + tools, MOCK_STOP_DELAY_MS:'300',
    MOCK_GROUP_SIGNAL:'SIGINT', MOCK_RECOVERY_SIGNAL:'SIGINT', MOCK_RECOVERY_SIGNALS:'2'}, {group:true});
  assert.strictEqual(result.status, 130, result.stderr);
  assert.strictEqual(f.settledRunning(), true, 'service left stopped after a repeated signal');
  assert.doesNotMatch(result.stderr, /restart failed|not verified/);
  assertNoSnapshots(f);
}));

test('recovery never trusts a start before the container is confirmed stopped', () => fixture(f => {
  const result = f.backup({MOCK_FAIL:'copy-wal', MOCK_STOP_NOOP:'1'}); assert.strictEqual(result.status, 23);
  assert.match(result.stderr, /could not confirm that corescope-test stopped/);
  assert.match(result.stderr, /restart not verified/); assertNoSnapshots(f);
}, {wal:true}));

test('a start that leaves the container stopped is a restart failure', () => fixture(f => {
  const result = f.backup({MOCK_START_NOOP:'1'}); assert.notStrictEqual(result.status, 0);
  assert.match(result.stderr, /restart failed/); assert.strictEqual(f.state().running, false);
  assertNoSnapshots(f);
}));

test('recovery reports a start that leaves the container stopped', () => fixture(f => {
  const result = f.backup({MOCK_FAIL:'copy-wal', MOCK_START_NOOP:'1'}); assert.strictEqual(result.status, 23);
  assert.match(result.stderr, /restart failed/); assert.strictEqual(f.state().running, false);
  assertNoSnapshots(f);
}, {wal:true}));

for (const kind of ['symlink','directory']) test('non-regular WAL (' + kind + ') fails, restarts and does not publish', () => fixture(f => {
  const wal = path.join(f.data, 'meshcore.db-wal');
  if (kind === 'symlink') fs.symlinkSync(path.join(f.data, 'config.json'), wal);
  else fs.mkdirSync(wal);
  const result = f.backup(); assert.strictEqual(result.status, 1, result.stderr);
  assert.match(result.stderr, /regular WAL meshcore\.db-wal/);
  assert.strictEqual(f.state().running, true); assertNoSnapshots(f);
}));

for (const kind of ['directory','symlink','dangling']) test('existing destination (' + kind + ') is never overwritten', () => fixture(f => {
  const outside = path.join(f.dir, 'outside');
  fs.mkdirSync(outside); fs.writeFileSync(path.join(outside, 'sentinel'), 'keep');
  const result = f.backup({MOCK_PRECREATE_DEST:kind, MOCK_OUTSIDE:outside});
  assert.strictEqual(result.status, 1, result.stderr); assert.match(result.stderr, /refusing to overwrite/);
  assert.deepStrictEqual(fs.readdirSync(outside), ['sentinel']);
  const left = fs.readdirSync(f.snapshots);
  assert.strictEqual(left.length, 1); assert(!left[0].startsWith('.'), 'staging left behind');
  if (kind === 'directory') assert.deepStrictEqual(fs.readdirSync(path.join(f.snapshots, left[0])), ['sentinel']);
  assert.strictEqual(fs.existsSync(outside + '-missing'), false);
  assert.strictEqual(f.state().running, true);
}));

for (const name of ['-', '-snapshots']) test('relative backup directory ' + JSON.stringify(name) + ' is not an option or cd -', () => fixture(f => {
  const decoy = path.join(f.dir, 'decoy'); fs.mkdirSync(decoy);
  const result = f.backup({OLDPWD:decoy}, {target:name});
  assert.strictEqual(result.status, 0, result.stderr);
  assert.strictEqual(path.dirname(result.stdout.trim()), fs.realpathSync(path.join(f.dir, name)));
  assert.deepStrictEqual(fs.readdirSync(decoy), []);
}));

test('CDPATH cannot redirect a relative backup directory', () => fixture(f => {
  const decoy = path.join(f.dir, 'decoy'); fs.mkdirSync(path.join(decoy, 'out'), {recursive:true});
  fs.mkdirSync(path.join(f.dir, 'out'));
  const result = f.backup({CDPATH:decoy}, {target:'out'});
  assert.strictEqual(result.status, 0, result.stderr);
  assert.strictEqual(path.dirname(result.stdout.trim()), fs.realpathSync(path.join(f.dir, 'out')));
  assert.deepStrictEqual(fs.readdirSync(path.join(decoy, 'out')), []);
}));

test('failed inspect never stops the container', () => fixture(f => {
  assert.strictEqual(f.backup({MOCK_FAIL:'inspect'}).status, 21);
  assert.strictEqual(calls(f,'stop').length, 0); assertNoSnapshots(f);
}));

test('backup of an already stopped container leaves it stopped', () => fixture(f => {
  const result = f.backup(); assert.strictEqual(result.status, 0, result.stderr);
  assert.strictEqual(calls(f,'stop').length, 0); assert.strictEqual(calls(f,'start').length, 0);
  assert.strictEqual(f.state().running, false);
  assert.deepStrictEqual(fs.readdirSync(f.snapshots), [path.basename(result.stdout.trim())]);
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

test('the deployment guide states the number of recovery stop attempts', () => {
  const helper = fs.readFileSync(path.join(root, 'scripts/backup-sqlite.sh'), 'utf8');
  const attempts = Number(helper.match(/while \[ "\$tries" -lt (\d+) \]/)[1]);
  const words = ['zero', 'one', 'two', 'three', 'four', 'five'];
  const docs = fs.readFileSync(path.join(root, 'docs/deployment.md'), 'utf8').replace(/\s+/g, ' ');
  const claim = 'recovery stop gets up to ' + words[attempts] + ' attempts';
  assert(docs.includes(claim), 'docs/deployment.md must say: ' + claim);
});

test('job control (sh -m on a terminal) does not break the isolated Docker calls', () => fixture(f => {
  // With job control a child leads its own process group, so setsid(1) would
  // fork and return before the Docker command has finished.
  const quote = s => "'" + s.replace(/'/g, "'\\''") + "'";
  const command = ['sh', '-m', path.join(root, 'scripts/backup-sqlite.sh'), 'corescope-test', f.snapshots].map(quote).join(' ');
  const result = spawnSync(which('script'), ['-qec', command, '/dev/null'],
    {env:{...f.env, MOCK_START_DELAY_MS:'300'}, encoding:'utf8', timeout:10000});
  assert.strictEqual(result.status, 0, result.stdout + result.stderr);
  assert.doesNotMatch(result.stdout, /ERROR/);
  assert.strictEqual(f.state().running, true);
  assert.strictEqual(fs.readdirSync(f.snapshots).filter(n => n.startsWith('corescope-backup-')).length, 1);
}, {wal:true}));

test('a further signal while the first is handled cannot preempt recovery', () => fixture(f => {
  // The helper's stderr is a full FIFO, so the `interrupted` write for the
  // first signal (HUP) blocks; TERM arrives while that handler runs. Pending
  // traps run in signal-number order, so even an early TERM cannot overtake HUP.
  const fifo = path.join(f.dir, 'stderr.fifo'), errors = path.join(f.dir, 'stderr.txt');
  assert.strictEqual(spawnSync('mkfifo', [fifo]).status, 0);
  const driver = `
    exec 3<>"$FIFO"
    sh "$HELPER" corescope-test "$OUT" 2>&3 & pid=$!
    n=0; until [ -e "$MOCK_STATE.signalled" ]; do
      n=$((n + 1)); [ "$n" -lt 400 ] || { kill -9 "$pid"; echo 'first signal never sent' >&2; exit 99; }
      sleep 0.02
    done
    sleep 0.5; kill -TERM "$pid"; sleep 0.2
    cat <&3 >"$ERRORS" & reader=$!
    wait "$pid"; status=$?
    sleep 0.2; kill "$reader"; exit "$status"`;
  const result = spawnSync('sh', ['-c', driver], {env:{...f.env, MOCK_SIGNAL:'stop', MOCK_SIGNAL_NAME:'SIGHUP',
    MOCK_FILL_STDERR:'1', FIFO:fifo, HELPER:path.join(root, 'scripts/backup-sqlite.sh'), OUT:f.snapshots, ERRORS:errors},
    encoding:'utf8', timeout:20000});
  const stderr = fs.existsSync(errors) ? fs.readFileSync(errors, 'utf8').replace(/x+/g, '') : '';
  assert.strictEqual(result.status, 129, result.stderr + stderr);
  assert.match(stderr, /interrupted by SIGHUP/);
  assert.doesNotMatch(stderr, /SIGTERM/);
  assert.strictEqual(f.state().running, true); assert.strictEqual(calls(f,'start').length, 1);
  assertNoSnapshots(f);
}));

// The other hot single-file DB copies (#389) now use the helper.
const HOT_DB_COPY = /\bcp\b[^\n]*\b(?:meshcore|ping_scores_history)\.db\b/;
function shellFunction(source, name) {
  const match = source.match(new RegExp('^' + name + '\\(\\) \\{\\n[\\s\\S]*?\\n\\}$', 'm'));
  assert(match, 'missing function ' + name); return match[0];
}
for (const doc of ['DEPLOY.md', 'docs/specs/deployment-simplification.md']) test(doc + ' backs up with the helper, not a hot copy', () => fixture(f => {
  const text = fs.readFileSync(path.join(root, doc), 'utf8');
  assert.strictEqual((text.match(HOT_DB_COPY) || [])[0], undefined, 'hot DB copy');
  const command = (text.match(/`([^`\n]*scripts\/backup-sqlite\.sh[^`\n]*)`/) || [])[1];
  assert(command, 'no helper command');
  // The documented container name is the one the guide's `docker run` creates.
  assert.strictEqual(command.split(/\s+/)[2], text.match(/docker run -d --name (\S+)/)[1]);
  const result = spawnSync('sh', ['-c', command], {cwd:root, env:{...f.env, HOME:f.dir}, encoding:'utf8', timeout:10000});
  assert.strictEqual(result.status, 0, result.stderr);
  assert(result.stdout.trim().startsWith(fs.realpathSync(f.dir) + path.sep), result.stdout);
  assert.deepStrictEqual(fs.readdirSync(result.stdout.trim()).sort(),
    ['meshcore.db', 'meshcore.db-wal', 'ping_scores_history.db', 'ping_scores_history.db-wal']);
  assert.deepStrictEqual(f.state().copiedWhileRunning, [false]);
}, {wal:true}));

function manage(f, args, {input='', extra={}}={}) {
  return spawnSync('bash', [path.join(root, 'manage.sh'), ...args], {cwd:f.dir, input,
    env:{...f.env, HOME:f.dir, PROD_DATA_DIR:f.data, ...extra}, encoding:'utf8', timeout:20000});
}
function snapshotIn(dir) {
  const found = fs.readdirSync(dir).filter(n => n.startsWith('corescope-backup-'));
  assert.strictEqual(found.length, 1, 'expected one helper snapshot in ' + dir + ': ' + fs.readdirSync(dir));
  return path.join(dir, found[0]);
}
const ALL_DB_FILES = ['meshcore.db', 'meshcore.db-wal', 'ping_scores_history.db', 'ping_scores_history.db-wal'];

test('manage.sh backup and promote contain no hot DB copy', () => {
  const source = fs.readFileSync(path.join(root, 'manage.sh'), 'utf8');
  for (const name of ['cmd_backup', 'cmd_promote'])
    assert.strictEqual((shellFunction(source, name).match(HOT_DB_COPY) || [])[0], undefined, name);
});

test('manage.sh backup snapshots both DBs and WALs offline via the helper', () => fixture(f => {
  const out = path.join(f.dir, 'manual');
  const result = manage(f, ['backup', out]); assert.strictEqual(result.status, 0, result.stdout + result.stderr);
  assert.deepStrictEqual(f.state().copiedWhileRunning, [false], 'copied while the container was running');
  assert(calls(f,'stop').every(c => c.includes('corescope-prod')));
  assert.deepStrictEqual(fs.readdirSync(snapshotIn(out)).sort(), ALL_DB_FILES);
  assert.strictEqual(fs.existsSync(path.join(out, 'meshcore.db')), false, 'hot single-file copy');
  assert.strictEqual(fs.readFileSync(path.join(out, 'config.json'), 'utf8'), 'PRIVATE_CONFIG_MUST_NOT_BE_PUBLISHED');
  assert.strictEqual(f.state().running, true);
}, {wal:true}));

test('manage.sh backup fails loudly when the helper fails', () => fixture(f => {
  const out = path.join(f.dir, 'manual');
  const result = manage(f, ['backup', out], {extra:{MOCK_FAIL:'copy-main'}});
  assert.notStrictEqual(result.status, 0);
  assert.match(result.stdout, /Database backup failed/);
  assert.strictEqual(fs.existsSync(path.join(out, 'meshcore.db')), false);
  assert.strictEqual(f.state().running, true);
}));

test('manage.sh backup without a prod container skips the DB with a warning', () => fixture(f => {
  const out = path.join(f.dir, 'manual');
  const result = manage(f, ['backup', out], {extra:{MOCK_FAIL:'inspect'}});
  assert.strictEqual(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /Database not backed up/);
  assert.deepStrictEqual(calls(f,'stop'), []);
  assert.deepStrictEqual(fs.readdirSync(out), ['config.json']);
}));

test('manage.sh restore finds the database in a manage.sh backup', () => fixture(f => {
  const out = path.join(f.dir, 'manual');
  assert.strictEqual(manage(f, ['backup', out]).status, 0);
  fs.writeFileSync(path.join(f.data, 'meshcore.db'), 'changed');
  const result = manage(f, ['restore', out], {input:'y'});
  assert.strictEqual(result.status, 0, result.stdout + result.stderr);
  assert.strictEqual(fs.readFileSync(path.join(f.data, 'meshcore.db'), 'utf8'), 'old-meshcore');
}));

test('manage.sh restore stops when the pre-restore backup fails', () => fixture(f => {
  const out = path.join(f.dir, 'manual');
  assert.strictEqual(manage(f, ['backup', out]).status, 0);
  fs.writeFileSync(path.join(f.data, 'meshcore.db'), 'changed');
  const result = manage(f, ['restore', out], {input:'y', extra:{MOCK_FAIL:'copy-main'}});
  assert.notStrictEqual(result.status, 0);
  assert.strictEqual(fs.readFileSync(path.join(f.data, 'meshcore.db'), 'utf8'), 'changed');
  assert.strictEqual(calls(f,'compose').filter(c => c.includes('stop')).length, 0);
}));

test('manage.sh promote backs up offline via the helper first', () => fixture(f => {
  const result = manage(f, ['promote'], {input:'y'});
  assert.strictEqual(result.status, 0, result.stdout + result.stderr);
  const backups = fs.readdirSync(path.join(f.dir, 'backups')).filter(n => n.startsWith('pre-promotion-'));
  assert.strictEqual(backups.length, 1);
  assert.deepStrictEqual(fs.readdirSync(snapshotIn(path.join(f.dir, 'backups', backups[0]))).sort(), ALL_DB_FILES);
  assert.deepStrictEqual(f.state().copiedWhileRunning, [false]);
  const order = f.state().calls.map(c => c[0]).filter(op => op === 'cp' || op === 'compose');
  // `compose version` at startup, then the helper's copy, then the recreate.
  assert.deepStrictEqual(order, ['compose', 'cp', 'compose'], 'backup must precede the recreate');
}, {wal:true}));

test('manage.sh promote aborts before recreating prod when the backup fails', () => fixture(f => {
  const result = manage(f, ['promote'], {input:'y', extra:{MOCK_FAIL:'copy-main'}});
  assert.notStrictEqual(result.status, 0);
  assert.strictEqual(calls(f,'compose').filter(c => c.includes('up')).length, 0);
  assert.strictEqual(f.state().running, true);
}));

// Host-tool gate (#389 review): the cases that drive the helper through
// process-group signals or a pty need tools that not every host has. They must
// skip with a clear reason instead of failing the suite, and the detection must
// probe the tool and the capability, never the OS name.
test('an unavailable tool skips its case instead of failing the suite', () => {
  const missing = new Set(['setsid', 'script -c', 'mkfifo']);
  for (const key of missing) {
    let err; try { need(key, missing); } catch (e) { err = e; }
    assert(err instanceof Skipped, key + ' must raise Skipped, got: ' + err);
    assert(err.message.startsWith('needs ' + key + ': '), 'unclear skip reason: ' + err.message);
    assert(/\S/.test(err.message.slice(('needs ' + key + ': ').length)), 'empty skip reason');
    // Precise detection: the probe runs the tool, it does not read the platform.
    assert.strictEqual(typeof capabilities[key].probe, 'function');
    assert.strictEqual(/darwin|platform|\bos\b/.test(String(capabilities[key].probe)), false,
      key + ' must probe the tool, not the OS name');
  }
  // The runner counts it as skipped, not failed.
  const before = failed, wasSkipped = skipped;
  test('inner probe of the skip path (an expected SKIP)', () => need('setsid', missing));
  assert.strictEqual(failed, before, 'a skipped case must not be counted as a failure');
  assert.strictEqual(skipped, wasSkipped + 1, 'a skipped case must be counted as skipped');
});

test('optional host tools are only reached through the skip gate', () => {
  const source = fs.readFileSync(__filename, 'utf8');
  for (const tool of ['setsid', 'script', 'mkfifo']) {
    assert.strictEqual(source.includes("which('" + tool + "')"), false,
      tool + '(1) must come from gated(), not which(), so a host without it skips');
    assert.strictEqual(source.includes("spawnSync('" + tool + "'"), false,
      tool + '(1) must come from gated(), not a bare spawnSync');
  }
  // No case may depend on a Linux-only kernel path.
  assert.strictEqual(source.includes('/proc/' + 'self/fd'), false,
    'the stderr fill must use the FIFO path, which is portable');
});

// Guarded against recursion: the nested run does not register this case.
if (!process.env.CORESCOPE_TEST_MISSING_TOOLS) test('the suite is green and skips those cases on a host without the tools', () => {
  const keys = Object.keys(capabilities);
  const result = spawnSync(process.execPath, [__filename], {encoding:'utf8', timeout:600000,
    env:{...process.env, CORESCOPE_TEST_MISSING_TOOLS:keys.join(',')}});
  assert.strictEqual(result.status, 0, 'suite must exit 0 without the tools:\n' + result.stdout + result.stderr);
  const skips = result.stdout.split('\n').filter(line => line.startsWith('SKIP '));
  assert.strictEqual(skips.length, 11, 'expected the 11 tool-dependent cases to skip, got ' + skips.length);
  for (const line of skips) assert.match(line, new RegExp('needs (' + keys.join('|') + '): \\S'), line);
  assert.match(result.stdout, /\b0 failed\b/);
  assert.match(result.stdout, /\b11 skipped\b/);
});

console.log(`${passed} passed, ${failed} failed`);
process.exitCode = failed === 0 ? 0 : 1;
