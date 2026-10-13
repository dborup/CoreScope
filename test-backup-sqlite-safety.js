'use strict';
// Execute the shipped operator script and the exact restore block from the
// deployment guide. All Docker calls go to a private executable mock; no daemon,
// container, network, production path or real database is contacted.
const assert = require('assert');
const fs = require('fs');
const os = require('os');
const path = require('path');
const crypto = require('crypto');
const { spawnSync } = require('child_process');

const root = __dirname;
const HELPER = path.join(root, 'scripts/backup-sqlite.sh');
let passed = 0;
let failed = 0;
let skipped = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('PASS ' + name); }
  catch (err) {
    if (err instanceof Skipped) { skipped++; console.log('SKIP ' + name + ': ' + err.message); return; }
    failed++; console.error('FAIL ' + name + ': ' + err.message);
  }
}

// Docker's daemon keeps stopping a container after the CLI is killed. The mock
// models that as a persisted deadline at which the container exits, whether
// or not any CLI process is still alive to observe it.
function settle(state) {
  if (state.stopAt && Date.now() >= state.stopAt) { state.running = false; delete state.stopAt; }
}
function locate(tool) {
  const found = spawnSync('/bin/sh', ['-c', 'command -v "$1"', 'sh', tool], {encoding:'utf8'}).stdout.trim();
  return found.startsWith('/') ? found : '';
}
function which(tool) { const found = locate(tool); assert(found, 'missing tool ' + tool); return found; }
function sleep(ms) { if (ms > 0) Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms); }

// Some cases drive the helper through process-group signals or a pty. That
// needs tools which not every host ships: util-linux setsid(1) and its `-w`,
// and a script(1) that takes `-c` (the BSD/macOS one does not). Such a case
// skips with a reason instead of failing, so the suite stays green there while
// still running — red or green — everywhere the tools exist. Each probe runs
// the tool and checks the capability it needs; none looks at the OS name.
class Skipped extends Error {}
function probe(tool, args, want = 0) {
  const result = spawnSync(tool, args, {stdio:'ignore', timeout:10000});
  return result.status === want;
}
const capabilities = {
  setsid: {tool:'setsid', reason:'setsid(1) with -w, which waits for the command, is not available',
    probe:() => probe('setsid', ['-w', '/bin/sh', '-c', 'exit 7'], 7)},
  'script -c': {tool:'script', reason:'script(1) does not accept -c, so no case can get a pty',
    probe:() => probe('script', ['-qec', 'exit 7', '/dev/null'], 7)},
  mkfifo: {tool:'mkfifo', reason:'mkfifo(1) cannot create a FIFO here',
    probe:() => {
      const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'corescope-backup-probe-'));
      try { return probe('mkfifo', [path.join(dir, 'fifo')]) && fs.statSync(path.join(dir, 'fifo')).isFIFO(); }
      catch { return false; } finally { fs.rmSync(dir, {recursive:true, force:true}); }
    }},
  // The helper locks an open descriptor (`flock -n 9`), as util-linux does.
  flock: {tool:'flock', reason:'flock(1) cannot lock an open descriptor here, so the helper uses its mkdir lock',
    probe:() => {
      const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'corescope-backup-probe-'));
      try { return probe('/bin/sh', ['-c', 'exec 9>>"$1" && flock -n 9 && exit 7', 'sh', path.join(dir, 'lock')], 7); }
      finally { fs.rmSync(dir, {recursive:true, force:true}); }
    }},
};
// A few cases stub ls(1) or id(1) on PATH; a BusyBox sh built to prefer its
// own applets never runs such a stub.
capabilities['PATH stubs'] = {tool:'sh', reason:'this sh runs its own applets instead of commands found on PATH',
  probe:() => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'corescope-backup-probe-'));
    try {
      fs.writeFileSync(path.join(dir, 'id'), '#!/bin/sh\necho 4242\n', {mode:0o700});
      return spawnSync('sh', ['-c', '[ "$(id -u)" = 4242 ] && exit 7'],
        {stdio:'ignore', timeout:10000, env:{...process.env, PATH:dir + path.delimiter + process.env.PATH}}).status === 7;
    } finally { fs.rmSync(dir, {recursive:true, force:true}); }
  }};
// Every caller runs the helper as `sh …`, and /bin/sh differs between hosts:
// dash (Debian, Ubuntu), bash in POSIX mode (RHEL, Fedora, Arch; bash 3.2 on
// macOS) or BusyBox ash (Alpine). Each runs as a symlink named `sh`, which is
// what puts bash into POSIX mode. CORESCOPE_TEST_BASH picks the bash binary,
// e.g. a bash 3.2, which clears the script's arguments on `command exec`
// inside a function (#424 review F1) where bash 5 does not.
const SHELLS = {'bash as sh':process.env.CORESCOPE_TEST_BASH || 'bash', dash:'dash', 'busybox sh':'busybox'};
for (const [key, tool] of Object.entries(SHELLS)) capabilities[key] = {tool,
  reason:tool + ' cannot run a script as sh here',
  probe:() => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'corescope-backup-probe-'));
    try {
      const found = locate(tool);
      if (!found) return false;
      fs.symlinkSync(found, path.join(dir, 'sh'));
      return probe(path.join(dir, 'sh'), ['-c', 'exit 7'], 7);
    } finally { fs.rmSync(dir, {recursive:true, force:true}); }
  }};
// A seam, so the skip path itself is covered on a host that has every tool.
const forcedMissing = new Set((process.env.CORESCOPE_TEST_MISSING_TOOLS || '').split(',').filter(Boolean));
const probed = new Map();
function need(key, missing = forcedMissing) {
  const capability = capabilities[key];
  assert(capability, 'unknown capability ' + key);
  if (!missing.has(key)) {
    if (!probed.has(key)) probed.set(key, capability.probe());
    if (probed.get(key)) return;
  }
  throw new Skipped('needs ' + key + ': ' + capability.reason);
}
// Absolute path of an optional tool, or a skip when the host cannot provide it.
function gated(key) { need(key); return which(capabilities[key].tool); }

const mockSource = `#!/usr/bin/env node
const fs = require('fs'), path = require('path');
const statePath = process.env.MOCK_STATE;
${settle}
${sleep}
// Overlapping helpers (each with its own MOCK_TAG) share one state file:
// serialise its read-modify-write, except while a call deliberately waits.
const tag = process.env.MOCK_TAG || '', mutex = tag ? statePath + '.mutex' : '';
let held = false;
function lockState() {
  for (let i = 0; mutex && !held; i++) {
    try { fs.mkdirSync(mutex); held = true; }
    catch (err) { if (err.code !== 'EEXIST' || i > 5000) throw err; sleep(2); }
  }
}
function unlockState() { if (held) { fs.rmdirSync(mutex); held = false; } }
lockState(); process.on('exit', unlockState);
let state = JSON.parse(fs.readFileSync(statePath, 'utf8'));
const args = process.argv.slice(2), op = args[0];
const format = args.find(a => a.includes('{{')) || '';
settle(state);
state.calls.push(args);
function event(kind) { state.events.push([tag, kind]); }
event(op !== 'inspect' ? op : format.includes('.Id') ? 'inspect-id' :
  format.includes('.State.Running') ? 'inspect-running' : 'inspect');
function save() { fs.writeFileSync(statePath, JSON.stringify(state)); }
function done(code=0) { save(); process.exit(code); }
// Let the other helpers run for a while, then pick up what they changed.
function unlocked(fn) {
  save(); unlockState(); fn(); lockState();
  state = JSON.parse(fs.readFileSync(statePath, 'utf8')); settle(state);
}
const sync = process.env.MOCK_SYNC;
function mark(name) { if (sync) fs.writeFileSync(path.join(sync, name), ''); }
function waitFor(markers) {
  if (!markers) return;
  unlocked(() => {
    for (let i = 0; !markers.split(',').some(m => fs.existsSync(path.join(sync, m))); i++) {
      if (i > 2000) { console.error('mock: timed out waiting for ' + markers); process.exit(97); }
      sleep(10);
    }
  });
}
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
  // Docker resolves a name, a full ID and a unique ID prefix to the same ID.
  if (format.includes('.Id')) { console.log(process.env.MOCK_ID ?? state.id); done(); }
  if (format.includes('.State.Running')) mark(tag + '-inspected');
  console.log(args[2].includes('.Image') ? 'sha256:local-test-image' : String(state.running));
  done();
}
if (op === 'stop') {
  waitFor(process.env.MOCK_STOP_WAITS_FOR);
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
    if (process.env.MOCK_FILL_STDERR && !fs.existsSync(statePath + '.signalled')) {
      // Fill the helper's stderr pipe so its next write blocks. The FIFO is
      // opened by path, not through /proc, so this works off Linux too.
      const fd = fs.openSync(process.env.MOCK_FILL_STDERR, fs.constants.O_WRONLY | fs.constants.O_NONBLOCK);
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
  // Slow start: the container runs only once the CLI returns.
  if (delay) unlocked(() => sleep(delay));
  // Like Docker, starting a still-running (even stopping) container is a no-op.
  event('start-done'); state.running = true; done();
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
    if (process.env.MOCK_PAUSE_COPY === '1') {
      // The main DB files are copied; the WALs wait until the test resumes.
      fs.mkdirSync(dst, {recursive:true});
      for (const name of fs.readdirSync(state.data).filter(n => n.endsWith('.db')))
        fs.copyFileSync(path.join(state.data, name), path.join(dst, name));
      mark('paused'); waitFor('resume');
      state.walCopyRunning.push(state.running);
    }
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
    fs.cpSync(state.data, dst, {recursive:true}); event('cp-done'); done();
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
  // A fresh container ID per fixture, so no two cases ever share a lock.
  const id = crypto.randomBytes(32).toString('hex');
  fs.writeFileSync(statePath, JSON.stringify({data, id, running:options.running !== false, calls:[],
    events:[], groupSignals:0, copiedWhileRunning:[], walCopyRunning:[]}));
  const locks = path.join(dir, 'locks'), lockDir = path.join(locks, 'corescope-backup-locks');
  const env = {...process.env, PATH:bin + path.delimiter + process.env.PATH, MOCK_STATE:statePath,
    CONTAINER:'corescope-test', RESTORE_DIR:incoming, MOCK_BACKUP_ROOT:snapshots, CORESCOPE_BACKUP_LOCK_DIR:locks};
  const f = {dir, data, incoming, snapshots, env, id, locks, lockDir, lock:path.join(lockDir, 'corescope-backup-' + id + '.lock'),
    state:() => JSON.parse(fs.readFileSync(statePath, 'utf8')),
    // Container state once any stop already in progress has completed.
    settledRunning:() => {
      const state = JSON.parse(fs.readFileSync(statePath, 'utf8'));
      if (state.stopAt) sleep(state.stopAt - Date.now() + 5);
      settle(state); return state.running;
    },
    backup:(extra={}, {target=snapshots, cwd=dir, group=false, helper=HELPER}={}) => {
      // group: run in a new session so the mock can signal the helper's whole
      // process group, like a terminal hangup, Ctrl-C or a supervisor would.
      const argv = ['sh', helper, 'corescope-test', target];
      if (group) argv.unshift(gated('setsid'), '-w');
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
// Every tool the helper runs. A PATH with some of them left out models a host
// without them, e.g. macOS, which ships neither setsid(1) nor flock(1).
const HELPER_TOOLS = ['sh','mkdir','mktemp','date','chmod','rm','mv','rmdir','sleep','ps','node','setsid','flock','ls','id'];
function pathWithout(f, omit) {
  const tools = fs.mkdtempSync(path.join(f.dir, 'tools-'));
  for (const tool of HELPER_TOOLS.filter(t => !omit.includes(t))) {
    const found = locate(tool);
    if (found) fs.symlinkSync(found, path.join(tools, tool));
  }
  return path.join(f.dir, 'bin') + path.delimiter + tools;
}
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
  // Two recovery signals: the third and last stop attempt is the one that holds.
  const result = f.backup({PATH:pathWithout(f, ['setsid', 'flock']), MOCK_STOP_DELAY_MS:'300',
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
  const result = spawnSync(gated('script -c'), ['-qec', command, '/dev/null'],
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
  assert.strictEqual(spawnSync(gated('mkfifo'), [fifo]).status, 0);
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
    MOCK_FILL_STDERR:fifo, FIFO:fifo, HELPER:path.join(root, 'scripts/backup-sqlite.sh'), OUT:f.snapshots, ERRORS:errors},
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

// Overlapping invocations for one container (#403). Each backend is driven
// explicitly: flock(1) where the host has it, the mkdir lock everywhere.
const LOCK_BACKENDS = {
  flock: f => { need('flock'); return f.env.PATH; },
  mkdir: f => pathWithout(f, ['flock']),
};
const quote = s => "'" + String(s).replace(/'/g, "'\\''") + "'";
// The reproduction from #403: A and B both target the container, with their
// own backup directories. A takes its stop only once B has either read the
// running state or is waiting for the lock, and pauses after copying the main
// DB files; B stops only once A is paused. Without serialisation B therefore
// stops, copies and restarts the writer while A still has its WALs to copy.
function overlap(f, helperPath, {a, b, aEnv={}}) {
  const sync = path.join(f.dir, 'sync'); fs.mkdirSync(sync);
  const run = (tag, ref, extra) => '{ ' + ['env', ...Object.entries({PATH:helperPath, MOCK_TAG:tag,
    CORESCOPE_BACKUP_LOCK_WAIT:'30', ...extra}).map(([k, v]) => k + '=' + v),
    'sh', path.join(root, 'scripts/backup-sqlite.sh'), ref, path.join(f.dir, 'out-' + tag)].map(quote).join(' ') +
    ` >"$S/${tag}.out" 2>"$S/${tag}.err"; echo $? >"$S/${tag}.status"; } &`;
  const driver = `S=$MOCK_SYNC
    ${run('A', a, {MOCK_STOP_WAITS_FOR:'B-inspected,B-waiting', MOCK_PAUSE_COPY:'1', ...aEnv})}
    n=0; until [ -e "$S/A-inspected" ]; do n=$((n + 1)); [ "$n" -lt 750 ] || exit 98; sleep 0.02; done
    ${run('B', b, {MOCK_STOP_WAITS_FOR:'paused'})}
    n=0; until [ -e "$S/paused" ] && { [ -e "$S/B.status" ] || [ -e "$S/B-waiting" ]; }; do
      if grep -q Waiting "$S/B.err" 2>/dev/null; then : >"$S/B-waiting"; fi
      n=$((n + 1)); [ "$n" -lt 750 ] || exit 98; sleep 0.02
    done
    : >"$S/resume"; wait`;
  const result = spawnSync('sh', ['-c', driver], {env:{...f.env, MOCK_SYNC:sync}, encoding:'utf8', timeout:60000});
  const read = name => fs.existsSync(path.join(sync, name)) ? fs.readFileSync(path.join(sync, name), 'utf8') : '';
  assert.strictEqual(result.status, 0, 'driver: ' + result.status + result.stderr + read('A.err') + read('B.err'));
  const out = {};
  for (const tag of ['A', 'B']) out[tag] = {status:Number(read(tag + '.status')), stdout:read(tag + '.out'), stderr:read(tag + '.err')};
  return out;
}

const OVERLAPS = [
  {name:'same name, different backup directories', a:() => 'corescope-test', b:() => 'corescope-test'},
  {name:'name and full ID', a:() => 'corescope-test', b:f => f.id},
  {name:'short ID and name, slow restart', a:f => f.id.slice(0, 12), b:() => 'corescope-test',
    aEnv:{MOCK_START_DELAY_MS:'1500'}},
  {name:'A interrupted mid-copy, slow recovery', a:() => 'corescope-test', b:() => 'corescope-test',
    aEnv:{MOCK_SIGNAL:'copy', MOCK_START_DELAY_MS:'1500'}, aStatus:143},
];
for (const [backend, helperPath] of Object.entries(LOCK_BACKENDS)) for (const c of OVERLAPS)
  test('overlapping backups never restart the writer mid-copy (' + backend + ', ' + c.name + ')', () => fixture(f => {
    const r = overlap(f, helperPath(f), {a:c.a(f), b:c.b(f), aEnv:c.aEnv});
    assert.strictEqual(r.A.status, c.aStatus || 0, 'A: ' + r.A.stderr);
    assert.strictEqual(r.B.status, 0, 'B: ' + r.B.stderr);
    const state = f.state(), events = state.events.filter(([tag]) => tag);
    assert.deepStrictEqual(state.walCopyRunning, [false], "A copied its WALs while the writer was running");
    // B may only resolve the ID before A is done: its running-state read,
    // stop, copy and start all wait for A's last Docker call to complete.
    const lastA = events.map(([tag]) => tag).lastIndexOf('A');
    const firstB = events.findIndex(([tag, kind]) => tag === 'B' && kind !== 'inspect-id');
    assert(firstB > lastA, 'B used the container while A was still running: ' + JSON.stringify(events));
    assert.match(r.B.stderr, /Waiting up to 30s for another backup of /);
    assert(state.copiedWhileRunning.every(running => running === false), 'copied while running');
    assert.strictEqual(state.running, true);
    for (const tag of c.aStatus ? ['B'] : ['A', 'B'])
      assert.deepStrictEqual(fs.readdirSync(snapshotIn(path.join(f.dir, 'out-' + tag))).sort(), ALL_DB_FILES);
    if (c.aStatus) assert.deepStrictEqual(fs.readdirSync(path.join(f.dir, 'out-A')), []);
    // The flock(1) lock file stays (unlinking it would race); a mkdir lock is gone.
    if (backend === 'flock') assert(fs.lstatSync(f.lock).isFile(), 'flock lock file');
    else assert.strictEqual(fs.existsSync(f.lock), false, 'mkdir lock left behind');
  }, {wal:true}));

function deadPid() {
  const pid = Number(spawnSync('/bin/sh', ['-c', 'echo $$'], {encoding:'utf8'}).stdout.trim());
  assert(pid > 1); return pid;
}
// The helper's private lock directory, as it creates it: mode 0700, this user's.
function privateLockDir(f) { fs.mkdirSync(f.lockDir, {recursive:true, mode:0o700}); fs.chmodSync(f.lockDir, 0o700); }
function plantLock(f, owner) {
  privateLockDir(f); fs.mkdirSync(f.lock);
  if (owner !== undefined) fs.writeFileSync(path.join(f.lock, 'pid'), owner + '\n');
}
const touched = f => f.state().events.filter(([, kind]) => kind !== 'inspect-id');

test('a mkdir lock whose owner has died is reclaimed', () => fixture(f => {
  const owner = deadPid(); plantLock(f, owner);
  const result = f.backup({PATH:pathWithout(f, ['flock']), CORESCOPE_BACKUP_LOCK_WAIT:'0'});
  assert.strictEqual(result.status, 0, result.stderr);
  assert.match(result.stderr, new RegExp('Removed a stale backup lock left by PID ' + owner));
  assert.strictEqual(fs.existsSync(f.lock), false, 'lock left behind');
  assert.strictEqual(calls(f, 'stop').length, 1); assert.strictEqual(f.state().running, true);
}));

test('a stale mkdir lock is left alone while another invocation is reclaiming it', () => fixture(f => {
  plantLock(f, deadPid()); fs.mkdirSync(f.lock + '.reclaim');
  const result = f.backup({PATH:pathWithout(f, ['flock']), CORESCOPE_BACKUP_LOCK_WAIT:'0'});
  assert.strictEqual(result.status, 75, result.stderr);
  assert.deepStrictEqual(fs.readdirSync(f.lock), ['pid']); assert.deepStrictEqual(touched(f), []);
}));

test('a signal during a stale-lock reclaim waits until the lock state is consistent', () => fixture(f => {
  // ps(1), which the reclaim runs, delivers SIGTERM to the helper once.
  const ps = path.join(f.dir, 'bin', 'ps');
  fs.writeFileSync(ps, `#!/bin/sh\nif [ ! -e "$0.sent" ]; then : >"$0.sent"; kill -TERM "$PPID"; fi\nexec ${quote(which('ps'))} "$@"\n`,
    {mode:0o700});
  plantLock(f, deadPid());
  const result = f.backup({PATH:pathWithout(f, ['flock', 'ps']), CORESCOPE_BACKUP_LOCK_WAIT:'0'});
  assert.strictEqual(result.status, 143, result.stderr); assert(fs.existsSync(ps + '.sent'));
  assert.match(result.stderr, /Removed a stale backup lock/);
  for (const left of [f.lock, f.lock + '.reclaim']) assert.strictEqual(fs.existsSync(left), false, left + ' left behind');
  assert.deepStrictEqual(touched(f), []);
}));

// The test runner itself, and PID 1, which belongs to another user unless the
// tests run as root (kill -0 then fails with EPERM, yet the owner is alive).
for (const [who, owner] of [['this user', process.pid], ['PID 1', 1], ['an unknown owner', undefined]])
  test('a mkdir lock held by ' + who + ' is never reclaimed and the wait is bounded', () => fixture(f => {
    plantLock(f, owner);
    const started = Date.now();
    const result = f.backup({PATH:pathWithout(f, ['flock']), CORESCOPE_BACKUP_LOCK_WAIT:'1'});
    assert.strictEqual(result.status, 75, result.stderr);
    assert(Date.now() - started < 8000, 'the wait is not bounded');
    assert.match(result.stderr, /Waiting up to 1s for another backup of corescope-test/);
    assert.match(result.stderr, /still holds .*nothing was stopped/);
    assert.deepStrictEqual(touched(f), [], 'the running state was read or the container touched');
    assert.deepStrictEqual(fs.readdirSync(f.lock), owner === undefined ? [] : ['pid']);
    if (owner !== undefined) assert.strictEqual(fs.readFileSync(path.join(f.lock, 'pid'), 'utf8'), owner + '\n');
    assertNoSnapshots(f);
  }));

for (const [backend, helperPath] of Object.entries(LOCK_BACKENDS))
  test('SIGTERM during the copy releases the ' + backend + ' lock', () => fixture(f => {
    const PATH = helperPath(f);
    const result = f.backup({PATH, MOCK_SIGNAL:'copy'}); assert.strictEqual(result.status, 143, result.stderr);
    if (backend === 'mkdir') assert.strictEqual(fs.existsSync(f.lock), false, 'lock left behind');
    const again = f.backup({PATH, CORESCOPE_BACKUP_LOCK_WAIT:'0'});
    assert.strictEqual(again.status, 0, again.stderr);
  }, {wal:true}));

test('a group SIGINT during a slow stop releases the mkdir lock after recovery', () => fixture(f => {
  const PATH = pathWithout(f, ['flock']);
  const result = f.backup({PATH, MOCK_STOP_DELAY_MS:'300', MOCK_GROUP_SIGNAL:'SIGINT'}, {group:true});
  assert.strictEqual(result.status, 130, result.stderr);
  assert.strictEqual(f.settledRunning(), true);
  assert.strictEqual(fs.existsSync(f.lock), false, 'lock left behind');
}));

// The holder: a mkdir lock owned by the live test runner, or a flock(1)
// process. The waiting helper is interrupted and must leave the lock alone.
for (const backend of Object.keys(LOCK_BACKENDS))
  test('a ' + backend + ' waiter interrupted by SIGTERM leaves the holder\'s lock alone', () => fixture(f => {
    const sync = path.join(f.dir, 'sync'); fs.mkdirSync(sync);
    let PATH, holder = ':';
    if (backend === 'mkdir') { PATH = pathWithout(f, ['flock']); plantLock(f, process.pid); }
    else {
      PATH = LOCK_BACKENDS.flock(f); privateLockDir(f);
      holder = `${quote(gated('flock'))} "$LOCK" sh -c ': >"$MOCK_SYNC/held"; until [ -e "$MOCK_SYNC/release" ]; do sleep 0.02; done' &
        until [ -e "$S/held" ]; do sleep 0.02; done`;
    }
    const driver = `S=$MOCK_SYNC; ${holder}
      PATH=$HELPER_PATH sh "$HELPER" corescope-test "$OUT" 2>"$S/err" & pid=$!
      n=0; until grep -q Waiting "$S/err" 2>/dev/null; do n=$((n + 1)); [ "$n" -lt 750 ] || exit 98; sleep 0.02; done
      kill -TERM "$pid"; wait "$pid"; echo $? >"$S/status"
      ${backend === 'flock' ? `if ${quote(gated('flock'))} -n "$LOCK" true; then echo free >"$S/lock"; fi; : >"$S/release"; wait` : ''}`;
    const result = spawnSync('sh', ['-c', driver], {env:{...f.env, MOCK_SYNC:sync, LOCK:f.lock, HELPER_PATH:PATH,
      HELPER:path.join(root, 'scripts/backup-sqlite.sh'), OUT:f.snapshots, CORESCOPE_BACKUP_LOCK_WAIT:'30'},
      encoding:'utf8', timeout:30000});
    assert.strictEqual(result.status, 0, result.stderr);
    const read = name => fs.readFileSync(path.join(sync, name), 'utf8');
    assert.strictEqual(read('status').trim(), '143', read('err'));
    assert.match(read('err'), /interrupted by SIGTERM/);
    assert.deepStrictEqual(touched(f), []);
    if (backend === 'mkdir') assert.strictEqual(fs.readFileSync(path.join(f.lock, 'pid'), 'utf8'), process.pid + '\n');
    else assert.strictEqual(fs.existsSync(path.join(sync, 'lock')), false, 'the holder lost its flock lock');
  }));

for (const [backend, wrong] of [['flock', 'directory'], ['flock', 'symlink'], ['mkdir', 'file'], ['mkdir', 'symlink']])
  test('a ' + wrong + ' at the ' + backend + ' lock path fails clearly without touching the container', () => fixture(f => {
    const PATH = LOCK_BACKENDS[backend](f);
    privateLockDir(f);
    if (wrong === 'directory') fs.mkdirSync(f.lock);
    else if (wrong === 'file') fs.writeFileSync(f.lock, '');
    else fs.symlinkSync(f.dir, f.lock);
    const result = f.backup({PATH, CORESCOPE_BACKUP_LOCK_WAIT:'0'});
    assert.strictEqual(result.status, 1, result.stderr);
    assert.match(result.stderr, /is not a .*lock.*remove it once no backup of corescope-test is running/);
    assert.deepStrictEqual(touched(f), []); assertNoSnapshots(f);
  }));

for (const [name, extra] of [['lock wait', {CORESCOPE_BACKUP_LOCK_WAIT:'5m'}], ['lock wait', {CORESCOPE_BACKUP_LOCK_WAIT:''}],
  ['relative lock directory', {CORESCOPE_BACKUP_LOCK_DIR:'locks'}]])
  test('an invalid ' + name + ' ' + JSON.stringify(Object.values(extra)[0]) + ' is rejected without Docker calls', () => fixture(f => {
    const result = f.backup(extra);
    assert.strictEqual(result.status, 2, result.stderr); assert.deepStrictEqual(f.state().calls, []);
  }));

for (const id of ['', 'not-an-id', 'a'.repeat(63), '../' + 'a'.repeat(61)])
  test('an unusable container ID ' + JSON.stringify(id) + ' fails before locking or stopping', () => fixture(f => {
    const result = f.backup({MOCK_ID:id});
    assert.strictEqual(result.status, 1, result.stderr); assert.match(result.stderr, /unknown container ID/);
    assert.deepStrictEqual(touched(f), []); assert.strictEqual(fs.existsSync(path.dirname(f.lock)), false);
  }));

test('the lock lives in /tmp unless CORESCOPE_BACKUP_LOCK_DIR is set, whatever TMPDIR says', () => fixture(f => {
  const PATH = LOCK_BACKENDS.flock(f);
  const dir = path.join('/tmp', path.basename(f.lockDir)), lock = path.join(dir, path.basename(f.lock));
  const existed = fs.existsSync(dir);
  try {
    const env = {...f.env, PATH, TMPDIR:path.join(f.dir, 'tmpdir')}; delete env.CORESCOPE_BACKUP_LOCK_DIR;
    const result = spawnSync('sh', [path.join(root, 'scripts/backup-sqlite.sh'), 'corescope-test', f.snapshots],
      {env, encoding:'utf8', timeout:10000});
    assert.strictEqual(result.status, 0, result.stderr);
    assert(fs.lstatSync(lock).isFile(), 'no lock file in ' + dir);
    assert.strictEqual(fs.existsSync(path.join(f.dir, 'tmpdir')), false);
  } finally {
    fs.rmSync(lock, {force:true});
    if (!existed) try { fs.rmdirSync(dir); } catch { /* another run's lock is in it */ }
  }
}));

// #424 review F4: /tmp is writable by every user, so a lock path directly in
// it could be a symlink planted between the helper's check and its open. The
// locks therefore live in a directory that only this user can write to, which
// the helper verifies after creating it, inside a parent where no other user
// can rename it away (sticky like /tmp, or not writable by others).
const lsAs = (f, uid) => {
  // ls(1) reporting the private lock directory as uid's, as for a directory
  // another user created first. Everything else is the real ls.
  need('PATH stubs');
  const shim = path.join(f.dir, 'bin', 'ls');
  fs.writeFileSync(shim, `#!/bin/sh
for last; do :; done
case "$last" in */corescope-backup-locks) ${quote(which('ls'))} "$@" | sed 's/^\\([^ ]*  *[^ ]*  *\\)[0-9][0-9]*/\\1${uid}/'; exit ;; esac
exec ${quote(which('ls'))} "$@"
`, {mode:0o700});
};
const idAs = (f, uid) => { need('PATH stubs'); fs.writeFileSync(path.join(f.dir, 'bin', 'id'),
  `#!/bin/sh\nif [ "$1" = -u ]; then echo ${uid}; else exec ${quote(which('id'))} "$@"; fi\n`, {mode:0o700}); };
for (const backend of Object.keys(LOCK_BACKENDS)) {
  test('the ' + backend + ' lock lives in a private directory of this user', () => fixture(f => {
    const result = f.backup({PATH:LOCK_BACKENDS[backend](f)});
    assert.strictEqual(result.status, 0, result.stderr);
    const dir = fs.lstatSync(f.lockDir);
    assert(dir.isDirectory(), 'no private lock directory');
    assert.strictEqual(dir.mode & 0o7777, 0o700, 'lock directory mode');
    assert.strictEqual(dir.uid, process.getuid(), 'lock directory owner');
    if (backend === 'flock') assert(fs.lstatSync(f.lock).isFile(), 'flock lock file');
    assert.deepStrictEqual(fs.readdirSync(f.locks), [path.basename(f.lockDir)], 'lock outside the private directory');
  }));

  for (const [what, plant, wrong] of [
    ['a symlink', f => fs.symlinkSync(path.join(f.dir, 'planted'), f.lockDir), /is not a private lock directory/],
    ['a dangling symlink', f => fs.symlinkSync(path.join(f.dir, 'planted', 'missing'), f.lockDir), /is not a private lock directory/],
    ['a directory other users can enter', f => { fs.mkdirSync(f.lockDir); fs.chmodSync(f.lockDir, 0o755); },
      /is not a private lock directory/],
    ['a directory of another user', f => { privateLockDir(f); lsAs(f, 4242); }, /is not a private lock directory/],
    ['a world-writable parent without the sticky bit', f => fs.chmodSync(f.locks, 0o777), /lets other users replace/],
    ['a parent of another user', f => idAs(f, 4242), /lets other users replace/],
  ]) test('the ' + backend + ' lock refuses ' + what + ' and touches neither it nor the container', () => fixture(f => {
    const planted = path.join(f.dir, 'planted'); fs.mkdirSync(planted);
    fs.mkdirSync(f.locks, {mode:0o700}); plant(f);
    const result = f.backup({PATH:LOCK_BACKENDS[backend](f), CORESCOPE_BACKUP_LOCK_WAIT:'0'});
    assert.strictEqual(result.status, 1, result.stderr);
    assert.match(result.stderr, wrong);
    assert.deepStrictEqual(fs.readdirSync(planted), [], 'created a file where the planted path points');
    assert.strictEqual(fs.existsSync(path.join(f.dir, 'planted', 'missing')), false);
    assert.deepStrictEqual(touched(f), []); assertNoSnapshots(f);
  }));

  test('the ' + backend + ' lock accepts a sticky world-writable parent like /tmp', () => fixture(f => {
    fs.mkdirSync(f.locks); fs.chmodSync(f.locks, 0o1777);
    const result = f.backup({PATH:LOCK_BACKENDS[backend](f)});
    assert.strictEqual(result.status, 0, result.stderr);
    assert.strictEqual(fs.lstatSync(f.lockDir).mode & 0o7777, 0o700);
  }));
}

test('the deployment guide documents the lock settings and the helper\'s defaults', () => {
  const helper = fs.readFileSync(path.join(root, 'scripts/backup-sqlite.sh'), 'utf8');
  const wait = helper.match(/lock_wait=\$\{CORESCOPE_BACKUP_LOCK_WAIT-(\d+)\}/)[1];
  const dir = helper.match(/lock_root=\$\{CORESCOPE_BACKUP_LOCK_DIR-([^}]+)\}/)[1];
  const docs = fs.readFileSync(path.join(root, 'docs/deployment.md'), 'utf8').replace(/\s+/g, ' ');
  for (const claim of ['`CORESCOPE_BACKUP_LOCK_WAIT` (default ' + wait + ' seconds)',
    '`CORESCOPE_BACKUP_LOCK_DIR` (default `' + dir + '`)', 'exits with status 75'])
    assert(docs.includes(claim), 'docs/deployment.md must say: ' + claim);
  assert.doesNotMatch(docs, /do not overlap|until serialization is implemented/i, 'stale interim mitigation');
});

// #424 review F1: bash 3.2 (also macOS's /bin/sh) clears the script's
// positional parameters when a function runs `command exec 9>>file`, so the
// helper must read its arguments before any function runs.
test('the helper reads its arguments before any function can clear them', () => {
  const late = [];
  let inFunction = false, sawFunction = false;
  for (const [i, line] of fs.readFileSync(HELPER, 'utf8').split('\n').entries()) {
    if (/^[a-z_]+\(\) \{$/.test(line)) { inFunction = sawFunction = true; continue; }
    if (inFunction) { if (line === '}') inFunction = false; continue; }
    if (sawFunction && !/^\s*#/.test(line) && /\$[1-9@*#]|\$\{[1-9@*#]\}/.test(line)) late.push((i + 1) + ': ' + line);
  }
  assert(sawFunction, 'no shell function found');
  assert.deepStrictEqual(late, [], 'positional parameters read after a function ran');
});

// #424 review F3: an abort that the EXIT trap sees as status 0 (bash 3.2
// after a `set -u` error) must not turn into success. The fault is injected
// into a copy of the shipped helper right after it stops the container.
function faultyHelper(f, fault) {
  const source = fs.readFileSync(HELPER, 'utf8'), stop = '  docker stop "$container" >/dev/null\n';
  assert.strictEqual(source.split(stop).length, 2, 'no unique stop to inject the fault after');
  const file = path.join(f.dir, 'backup-sqlite.sh');
  fs.writeFileSync(file, source.replace(stop, stop + '  ' + fault + '\n'));
  return file;
}
const UNBOUND = ': "$corescope_test_unbound_variable"';
for (const [what, fault, status] of [['a set -u error', UNBOUND], ['an abort the EXIT trap sees as status 0', 'exit 0', 1]])
  test(what + ' after the stop fails, restarts and publishes nothing', () => fixture(f => {
    const result = f.backup({}, {helper:faultyHelper(f, fault)});
    if (status) assert.strictEqual(result.status, status, result.stderr);
    else assert.notStrictEqual(result.status, 0, result.stderr);
    if (fault === 'exit 0') assert.match(result.stderr, /ERROR: stopped before completing/);
    assert.strictEqual(result.stdout, '');
    assert.strictEqual(calls(f, 'start').length, 1); assert.strictEqual(f.state().running, true);
    assertNoSnapshots(f);
  }, {wal:true}));

// The manage.sh message for a contended lock (#424 review F6).
test('manage.sh backup says another backup holds the lock, not that prod is down', () => fixture(f => {
  const sync = path.join(f.dir, 'sync'); fs.mkdirSync(sync); privateLockDir(f);
  const driver = `${quote(gated('flock'))} "$LOCK" sh -c ': >"$S/held"; until [ -e "$S/release" ]; do sleep 0.02; done' &
    until [ -e "$S/held" ]; do sleep 0.02; done
    bash "$MANAGE" backup "$OUT" >"$S/out" 2>&1; echo $? >"$S/status"; : >"$S/release"; wait`;
  const result = spawnSync('sh', ['-c', driver], {cwd:f.dir, encoding:'utf8', timeout:30000, env:{...f.env, HOME:f.dir,
    PROD_DATA_DIR:f.data, S:sync, LOCK:f.lock, MANAGE:path.join(root, 'manage.sh'), OUT:path.join(f.dir, 'manual'),
    CORESCOPE_BACKUP_LOCK_WAIT:'0'}});
  assert.strictEqual(result.status, 0, result.stderr);
  const out = fs.readFileSync(path.join(sync, 'out'), 'utf8');
  assert.notStrictEqual(fs.readFileSync(path.join(sync, 'status'), 'utf8').trim(), '0', out);
  assert.match(out, /another backup of corescope-prod is still running/);
  assert.doesNotMatch(out, /check that corescope-prod is running/);
  assert.deepStrictEqual(calls(f, 'stop'), []);
}));

// #424 review F1/F2: the cases that matter most, under every /bin/sh found
// here and with both lock backends.
// The host's whole PATH minus some tools, for manage.sh, which needs far more
// tools than the helper does.
function pathMirror(f, omit) {
  const dir = fs.mkdtempSync(path.join(f.dir, 'mirror-'));
  for (const entry of process.env.PATH.split(path.delimiter)) {
    let names = []; try { names = fs.readdirSync(entry); } catch { continue; }
    // The first PATH entry with a name wins, as in a PATH lookup.
    for (const name of names.filter(n => !omit.includes(n) && !fs.existsSync(path.join(dir, n))))
      fs.symlinkSync(path.join(entry, name), path.join(dir, name));
  }
  return dir;
}
function shellPath(f, key, backend) {
  const dir = fs.mkdtempSync(path.join(f.dir, 'shell-'));
  fs.symlinkSync(gated(key), path.join(dir, 'sh'));
  return dir + path.delimiter + LOCK_BACKENDS[backend](f);
}
const SHELL_CASES = {
  'backs up a running container and restarts it': (f, PATH) => {
    const result = f.backup({PATH}); assert.strictEqual(result.status, 0, result.stderr);
    assert.deepStrictEqual(fs.readdirSync(result.stdout.trim()).sort(), ALL_DB_FILES);
    assert.deepStrictEqual(f.state().copiedWhileRunning, [false]); assert.strictEqual(f.state().running, true);
  },
  'keeps a failed copy\'s status and restarts': (f, PATH) => {
    const result = f.backup({PATH, MOCK_FAIL:'copy-wal'}); assert.strictEqual(result.status, 23, result.stderr);
    assert.strictEqual(f.state().running, true); assertNoSnapshots(f);
  },
  'fails on a set -u error after the stop and restarts': (f, PATH) => {
    const result = f.backup({PATH}, {helper:faultyHelper(f, UNBOUND)});
    assert.notStrictEqual(result.status, 0, result.stderr); assert.strictEqual(result.stdout, '');
    assert.strictEqual(f.state().running, true); assertNoSnapshots(f);
  },
  'lets manage.sh promote abort when the backup fails': (f, PATH, backend) => {
    const rest = backend === 'flock' ? process.env.PATH : pathMirror(f, ['flock']);
    const result = manage(f, ['promote'], {input:'y', extra:{PATH:PATH + path.delimiter + rest, MOCK_FAIL:'copy-main'}});
    assert.notStrictEqual(result.status, 0, result.stdout + result.stderr);
    assert.strictEqual(calls(f, 'compose').filter(c => c.includes('up')).length, 0);
  },
  'serialises overlapping backups': (f, PATH) => {
    const r = overlap(f, PATH, {a:'corescope-test', b:f.id});
    assert.strictEqual(r.A.status, 0, 'A: ' + r.A.stderr); assert.strictEqual(r.B.status, 0, 'B: ' + r.B.stderr);
    assert.deepStrictEqual(f.state().walCopyRunning, [false], 'A copied its WALs while the writer was running');
    assert.match(r.B.stderr, /Waiting up to 30s/);
  },
};
for (const key of Object.keys(SHELLS)) for (const backend of Object.keys(LOCK_BACKENDS))
  for (const [name, check] of Object.entries(SHELL_CASES))
    test('under ' + key + ' with the ' + backend + ' lock the helper ' + name, () => fixture(f => check(f, shellPath(f, key, backend), backend), {wal:true}));

// Host-tool gate (#389 review): the cases that drive the helper through
// process-group signals or a pty need tools that not every host has. They must
// skip with a clear reason instead of failing the suite, and the detection must
// probe the tool and the capability, never the OS name.
const SELF_CHECK = 'self-check: ';
test('an unavailable tool skips its case instead of failing the suite', () => {
  const missing = new Set(['setsid', 'script -c', 'mkfifo']);
  for (const key of missing) {
    let err; try { need(key, missing); } catch (e) { err = e; }
    assert(err instanceof Skipped, key + ' must raise Skipped, got: ' + err);
    assert(err.message.startsWith('needs ' + key + ': '), 'unclear skip reason: ' + err.message);
    assert(/\S/.test(err.message.slice(('needs ' + key + ': ').length)), 'empty skip reason');
    // Precise detection: the probe runs the tool, it does not read the platform.
    assert.strictEqual(typeof capabilities[key].probe, 'function');
    assert.strictEqual(/process\.platform|os\.(?:platform|type|release)\(|darwin|win32|\blinux\b/
      .test(String(capabilities[key].probe)), false, key + ' must probe the tool, not the OS name');
  }
  // The runner counts it as skipped, not failed.
  const before = failed, wasSkipped = skipped;
  test(SELF_CHECK + 'probing the skip path, so this SKIP line is expected', () => need('setsid', missing));
  assert.strictEqual(failed, before, 'a skipped case must not be counted as a failure');
  assert.strictEqual(skipped, wasSkipped + 1, 'a skipped case must be counted as skipped');
});

test('optional host tools are only reached through the skip gate', () => {
  const source = fs.readFileSync(__filename, 'utf8');
  for (const tool of ['setsid', 'script', 'mkfifo', 'flock', 'dash', 'busybox']) {
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
  // Minus the deliberate SKIP that the runner self-check above produces.
  const skips = result.stdout.split('\n')
    .filter(line => line.startsWith('SKIP ') && !line.includes(SELF_CHECK));
  assert.strictEqual(skips.length, 62, 'expected the 62 tool-dependent cases to skip, got ' + skips.length);
  for (const line of skips) assert.match(line, new RegExp('needs (' + keys.join('|') + '): \\S'), line);
  assert.match(result.stdout, /\b0 failed\b/);
  assert.match(result.stdout, /\b63 skipped\b/);
});

console.log(`${passed} passed, ${failed} failed` + (skipped ? `, ${skipped} skipped` : ''));
process.exitCode = failed === 0 ? 0 : 1;
