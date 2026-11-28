// Behavioural regression tests for Go assignment, execution receipts and
// coverage merging. Fixtures use the real Go AST inventory, not grep mocks.
'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { spawnSync } = require('node:child_process');
const { PACKAGE, sha256, discoverTests, validateManifest, readManifest, assignedTests, shardSelection, assertPartition, stalePins, testPattern, MAX_PATTERN_BYTES, requireContext } = require('./scripts/ci/go-server-manifest');
const { verifyTestJSON, verifyRuntimeList, runShard, spawnLogged } = require('./scripts/run-go-server-shard');
const { parseCoverprofile, mergeCoverprofiles, aggregateShards, writeIngestorReceipt, verifyIngestorArtifact } = require('./scripts/report-go-server-shards');
const { readTimingInputs, planShards, buildManifest } = require('./scripts/rebalance-go-server-shards');

const temporary = fs.mkdtempSync(path.join(os.tmpdir(), 'ci-go-shards-'));
let passed = 0;
async function check(name, test) {
  await test();
  passed++;
  console.log(`PASS ${name}`);
}
const clone = value => JSON.parse(JSON.stringify(value));
const root = path.join(temporary, 'repo');
fs.mkdirSync(path.join(root, 'cmd/server'), { recursive: true });
fs.mkdirSync(path.join(root, 'scripts'), { recursive: true });
fs.writeFileSync(path.join(root, 'cmd/server/go.mod'), `module ${PACKAGE}\n\ngo 1.22\n`);
fs.writeFileSync(path.join(root, 'cmd/server/main.go'), 'package main\n\nfunc covered() int { return 1 }\n\nfunc uncovered() int { return 2 }\n\n// Answer and Unexecuted give the fixture Examples real identifiers (go vet).\nfunc Answer() {}\n\nfunc Unexecuted() {}\n\nfunc main() {}\n');
fs.writeFileSync(path.join(root, 'cmd/server/sample_test.go'), `package main
import ("fmt"; "testing")
// func TestComment(t *testing.T) {}
var fake = "func TestString(t *testing.T) {}"
type helper struct{}
func (helper) TestMethod(t *testing.T) {}
func TestMain(m *testing.M) { m.Run() }
func TestAlpha(t *testing.T) {}
func TestBeta(t *testing.T) {}
func TestGamma(t *testing.T) {}
func FuzzSmoke(f *testing.F) { f.Add("seed"); f.Fuzz(func(t *testing.T, s string) {}) }
func ExampleAnswer() { fmt.Println("answer") // Output: answer
}
func ExampleUnexecuted() {}
`);
fs.writeFileSync(path.join(root, 'cmd/server/only_windows_test.go'), `package main\nimport "testing"\nfunc TestWindows(t *testing.T) {}\n`);
let inventory;
let manifest;
let manifestFile;
const context = { GITHUB_RUN_ID: '17', GITHUB_RUN_ATTEMPT: '2', GITHUB_SHA: 'a'.repeat(40) };
// The third block stays at count 0 in every shard: an uncovered statement must
// stay uncovered after the merge, not count as covered.
const profileA = `mode: atomic\n${PACKAGE}/main.go:1.1,2.1 2 1\n${PACKAGE}/main.go:3.1,4.1 3 0\n${PACKAGE}/main.go:5.1,6.1 3 0\n`;
const profileB = `mode: atomic\n${PACKAGE}/main.go:1.1,2.1 2 0\n${PACKAGE}/main.go:3.1,4.1 3 7\n${PACKAGE}/main.go:5.1,6.1 3 0\n`;

function jsonFor(selected, statuses = {}) {
  return [
    { Action: 'start', Package: PACKAGE },
    ...selected.flatMap(test => [
      { Action: 'run', Package: PACKAGE, Test: test.name },
      { Action: statuses[test.name] || 'pass', Package: PACKAGE, Test: test.name, Elapsed: 0.01 },
    ]),
    { Action: 'pass', Package: PACKAGE, Elapsed: 0.1 },
  ].map(event => JSON.stringify(event)).join('\n') + '\n';
}

const testArgs = new Map();
async function makeArtifacts(directory) {
  fs.mkdirSync(directory, { recursive: true });
  const receipts = [];
  for (let shard = 1; shard <= manifest.shards; shard++) {
    const selected = assignedTests(manifest, inventory, shard);
    const folder = path.join(directory, `go-server-${shard}`);
    const receipt = await runShard({ root, manifestFile, shard, inventory, outputDir: folder, env: context,
      runProcess: async (_command, args, options) => {
        if (args[0] === 'test') {
          testArgs.set(shard, args);
          fs.writeFileSync(options.jsonFile, jsonFor(selected));
          fs.writeFileSync(args.find(arg => arg.startsWith('-coverprofile=')).slice('-coverprofile='.length), shard === 1 ? profileA : profileB);
        }
        return { code: 0, signal: null };
      },
    });
    receipts.push(receipt);
  }
  return receipts;
}

function reportOptions(directory) {
  return { root, manifestFile, artifactsDir: directory, outputDir: path.join(temporary, 'output'), inventory,
    env: { ...context, GO_GUARD_RESULT: 'success', GO_INGESTOR_RESULT: 'success', GO_SERVER_1_RESULT: 'success', GO_SERVER_2_RESULT: 'success', GO_SERVER_3_RESULT: 'success' },
  };
}

async function main() {
  await check('AST discovers Tests, executable Examples and Fuzz seed targets only', () => {
    inventory = discoverTests(root);
    assert.deepEqual(discoverTests(path.relative(process.cwd(), root)), inventory);
    assert.deepEqual(inventory.map(test => test.name).sort(), ['ExampleAnswer', 'FuzzSmoke', 'TestAlpha', 'TestBeta', 'TestGamma', 'TestWindows']);
    assert.equal(inventory.find(test => test.name === 'TestWindows').active, process.platform === 'win32');
    // Shards 1 and 2 run only their pinned entries; shard 3 is the catch-all
    // and runs every other entry, including Tests the manifest has never seen.
    manifest = { version: 2, shards: 3, package: PACKAGE, pinned: [
      { name: 'TestAlpha', shard: 1, samplesSeconds: [3, 5], meanSeconds: 4 },
      { name: 'TestBeta', shard: 2, samplesSeconds: [4], meanSeconds: 4 },
    ] };
    manifestFile = path.join(root, 'scripts/go-server-shards.json');
    fs.writeFileSync(manifestFile, JSON.stringify(manifest));
    validateManifest(manifest, root, inventory);
  });
  await check('a new Test without a manifest entry runs in the catch-all shard', () => {
    fs.writeFileSync(path.join(root, 'cmd/server/new_test.go'), 'package main\nimport "testing"\nfunc TestNew(t *testing.T) {}\n');
    try {
      const current = discoverTests(root);
      validateManifest(manifest, root, current);
      assert.deepEqual(assignedTests(manifest, current, 3).map(test => test.name), ['TestNew', 'TestGamma', 'FuzzSmoke', 'ExampleAnswer']);
      assert.deepEqual(assignedTests(manifest, current, 1).map(test => test.name), ['TestAlpha']);
      assert.deepEqual(assignedTests(manifest, current, 2).map(test => test.name), ['TestBeta']);
      assertPartition(manifest, current);
    } finally { fs.unlinkSync(path.join(root, 'cmd/server/new_test.go')); }
  });
  await check('every active entry lands in exactly one shard', () => {
    const shards = [1, 2, 3].map(shard => assignedTests(manifest, inventory, shard).map(test => test.name));
    const all = shards.flat();
    assert.equal(new Set(all).size, all.length, 'an entry is selected by two shards');
    assert.deepEqual([...all].sort(), inventory.filter(test => test.active).map(test => test.name).sort());
    assert.equal(assertPartition(manifest, inventory), all.length);
    assert.ok(!all.includes('TestWindows'), 'inactive build-constrained entries never run');
  });
  await check('duplicate, catch-all, out-of-range and invalid timing pins fail', () => {
    const duplicate = clone(manifest); duplicate.pinned.push({ ...duplicate.pinned[0], shard: 2 });
    assert.throws(() => validateManifest(duplicate, root, inventory), /duplicate pin: TestAlpha/);
    for (const shard of [0, 3, 4, 1.5]) {
      const bad = clone(manifest); bad.pinned[0].shard = shard;
      assert.throws(() => validateManifest(bad, root, inventory), /pinned shard must be 1\.\.2: TestAlpha/);
    }
    const empty = clone(manifest); empty.pinned[1].shard = 1;
    assert.throws(() => validateManifest(empty, root, inventory), /empty active Go shard 2.*rebalance-go-server-shards/);
    const timings = clone(manifest); timings.pinned[0].meanSeconds = 1;
    assert.throws(() => validateManifest(timings, root, inventory), /mean differs/);
    const noSamples = clone(manifest); noSamples.pinned[1].samplesSeconds = [];
    assert.throws(() => validateManifest(noSamples, root, inventory), /timing samples required/);
    const v1 = clone(manifest); v1.version = 1;
    assert.throws(() => validateManifest(v1, root, inventory), /unsupported Go manifest version/);
  });
  await check('a removed or renamed pinned Test is reported but does not fail', () => {
    const stale = clone(manifest);
    stale.pinned.push({ name: 'TestRemoved', shard: 1, samplesSeconds: [9], meanSeconds: 9 });
    validateManifest(stale, root, inventory);
    assert.deepEqual(stalePins(stale, inventory), ['TestRemoved']);
    assert.deepEqual(assignedTests(stale, inventory, 1).map(test => test.name), ['TestAlpha']);
    assert.ok(!shardSelection(stale, inventory, 3).pattern.includes('TestRemoved'));
  });
  await check('pinned shards use -run, the catch-all uses -skip of every pinned entry', () => {
    const pinned = shardSelection(manifest, inventory, 1);
    assert.equal(pinned.flag, '-run');
    assert.equal(pinned.pattern, '^(TestAlpha)$');
    const catchAll = shardSelection(manifest, inventory, 3);
    assert.equal(catchAll.flag, '-skip');
    assert.equal(catchAll.pattern, '^(TestAlpha|TestBeta)$');
    assert.deepEqual(catchAll.tests.map(test => test.name), ['TestGamma', 'FuzzSmoke', 'ExampleAnswer']);
    const skip = new RegExp(catchAll.pattern);
    assert.ok(!skip.test('TestAlphaExtra') && !skip.test('XTestBeta'), 'skip pattern must be anchored');
  });
  await check('Fuzz seed target is retained in an anchored selection', () => {
    const selected = assignedTests(manifest, inventory, 3);
    const pattern = testPattern(selected);
    assert.ok(pattern.startsWith('^(') && pattern.endsWith(')$'));
    assert.ok(new RegExp(pattern).test('FuzzSmoke'));
    assert.ok(!new RegExp(pattern).test('FuzzSmokeExtra'));
    assert.equal(verifyTestJSON(jsonFor(selected), selected).tests.find(test => test.name === 'FuzzSmoke').kind, 'fuzz');
  });
  await check('a pattern near the Linux single-argument limit is refused', () => {
    // go test hands the pattern to the test binary as one argv string
    // (-test.run=… / -test.skip=…); Linux caps one string at 128 KiB.
    assert.ok(MAX_PATTERN_BYTES < 128 * 1024);
    const many = Array.from({ length: 3000 }, (_, index) => ({ name: `TestLongGeneratedName_${String(index).padStart(5, '0')}_abcdefghij` }));
    assert.throws(() => testPattern(many), /MAX_ARG_STRLEN/);
    assert.ok(testPattern(many.slice(0, 100)).length < MAX_PATTERN_BYTES);
  });
  await check('runtime list verifies all active entries and one package', () => {
    const active = inventory.filter(test => test.active).map(test => test.name);
    const good = (_command, args) => ({ status: 0, signal: null, stdout: args[0] === 'list' ? PACKAGE + '\n' : active.join('\n') + '\nok ' + PACKAGE + '\n', stderr: '' });
    assert.deepEqual(verifyRuntimeList(root, inventory, good), active);
    const missing = (_command, args) => ({ ...good(_command, args), stdout: args[0] === 'list' ? PACKAGE + '\n' : active.slice(1).join('\n') });
    assert.throws(() => verifyRuntimeList(root, inventory, missing), /runtime list differs/);
    const extraPackage = () => ({ status: 0, signal: null, stdout: PACKAGE + '\n' + PACKAGE + '/other\n', stderr: '' });
    assert.throws(() => verifyRuntimeList(root, inventory, extraPackage), /unexpected package/);
  });
  await check('dropped, failed, duplicate and unexpected test JSON is rejected', () => {
    const selected = assignedTests(manifest, inventory, 1);
    assert.throws(() => verifyTestJSON(jsonFor(selected.slice(1)), selected), /missing or incomplete/);
    assert.throws(() => verifyTestJSON(jsonFor(selected, { [selected[0].name]: 'fail' }), selected), /failed Go event/);
    assert.throws(() => verifyTestJSON(jsonFor([...selected, selected[0]]), selected), /executed twice/);
    const unexpected = [...selected, { name: 'TestUnassigned', kind: 'test' }];
    assert.throws(() => verifyTestJSON(jsonFor(unexpected), selected), /unassigned Go test executed/);
    assert.throws(() => verifyTestJSON(jsonFor(selected).replace(/"Action":"pass","Package":"github.com\/corescope\/server","Elapsed":0.1/, '"Action":"output","Package":"github.com/corescope/server","Elapsed":0.1'), selected), /missing successful Go package/);
  });
  await check('existing top-level skip and all subtests remain accounted for', () => {
    const selected = assignedTests(manifest, inventory, 1);
    assert.equal(verifyTestJSON(jsonFor(selected, { [selected[0].name]: 'skip' }), selected).tests[0].status, 'skip');
    const raw = jsonFor(selected).split('\n');
    raw.splice(2, 0, JSON.stringify({ Action: 'run', Package: PACKAGE, Test: selected[0].name + '/seed#0' }));
    verifyTestJSON(raw.join('\n'), selected);
  });
  await check('coverage counters are summed and statement union is not averaged', () => {
    const merged = mergeCoverprofiles([profileA, profileB]);
    assert.equal(merged.totalStatements, 8);
    assert.equal(merged.coveredStatements, 5);
    assert.equal(merged.coveragePercent, 62.5);
    assert.ok(merged.profile.includes('3.1,4.1 3 7'));
    assert.ok(merged.profile.includes('5.1,6.1 3 0'), 'a block no shard covered stays at zero');
    const huge = profileA.replace('2 1', '2 9007199254740993');
    assert.ok(mergeCoverprofiles([huge, profileA]).profile.includes('2 9007199254740994'));
  });
  await check('wrong coverage mode, missing blocks and statement changes fail', () => {
    assert.throws(() => parseCoverprofile(profileA.replace('atomic', 'set')), /atomic mode/);
    assert.throws(() => mergeCoverprofiles([profileA, profileB.split('\n').slice(0, 2).join('\n') + '\n']), /block set differs/);
    assert.throws(() => mergeCoverprofiles([profileA, profileB.replace('3 7', '4 7')]), /statement count differs/);
    assert.throws(() => parseCoverprofile(profileA + profileA.split('\n')[1] + '\n'), /duplicate coverage block/);
  });
  await check('runner failure writes a failure receipt and does not run later steps', async () => {
    let calls = 0;
    const folder = path.join(temporary, 'failed');
    await assert.rejects(runShard({ root, manifestFile, shard: 1, inventory, outputDir: folder,
      runProcess: async () => { calls++; return { code: 3, signal: null }; },
    }), /Go build failed/);
    assert.equal(calls, 1);
    assert.equal(JSON.parse(fs.readFileSync(path.join(folder, 'receipt.json'))).status, 'failure');
  });
  await check('signal receipt is failure even with a zero child exit', async () => {
    const folder = path.join(temporary, 'signalled');
    await assert.rejects(runShard({ root, manifestFile, shard: 1, inventory, outputDir: folder,
      runProcess: async () => ({ code: 0, signal: 'SIGTERM' }),
    }), /terminated by a signal/);
    const receipt = JSON.parse(fs.readFileSync(path.join(folder, 'receipt.json')));
    assert.equal(receipt.status, 'failure');
    assert.equal(receipt.runs[0].signal, 'SIGTERM');
  });
  await check('real process cancellation cannot become a successful child', async () => {
    const probe = path.join(temporary, 'signal-parent.js');
    fs.writeFileSync(probe, `const {spawnLogged}=require(${JSON.stringify(path.resolve('scripts/run-go-server-shard.js'))});\nspawnLogged(process.execPath,['-e',"process.on('SIGTERM',()=>process.exit(0));console.log('ready');setInterval(()=>{},1000)"],{cwd:process.cwd(),env:process.env,log:${JSON.stringify(path.join(temporary, 'signal.log'))}}).then(r=>console.log(JSON.stringify(r)));\nsetTimeout(()=>process.kill(process.pid,'SIGTERM'),250);\n`);
    const result = spawnSync(process.execPath, [probe], { encoding: 'utf8', timeout: 5000 });
    assert.equal(result.status, 0, result.stderr);
    const last = result.stdout.trim().split('\n').at(-1);
    assert.deepEqual(JSON.parse(last), { code: 1, signal: 'SIGTERM' });
  });
  const artifacts = path.join(temporary, 'artifacts');
  await makeArtifacts(artifacts);
  await check('complete receipts and merged profile produce a coverage report', () => {
    const report = aggregateShards(reportOptions(artifacts));
    assert.equal(report.status, 'success');
    assert.equal(report.coveragePercent, 62.5);
    assert.equal(report.coveredStatements, 5);
    assert.equal(report.testCount, inventory.filter(test => test.active).length);
    assert.equal(fs.readFileSync(path.join(temporary, 'output/cmd/server/server-coverage.out'), 'utf8'), mergeCoverprofiles([profileA, profileB, profileB]).profile);
  });
  await check('runner passes -run to pinned shards and only -skip to the catch-all', () => {
    for (const shard of [1, 2]) {
      const args = testArgs.get(shard);
      assert.equal(args[args.indexOf('-run') + 1], shardSelection(manifest, inventory, shard).pattern);
      assert.ok(!args.includes('-skip'));
    }
    const args = testArgs.get(3);
    assert.ok(!args.includes('-run'), 'the catch-all must not restrict with -run');
    assert.equal(args[args.indexOf('-skip') + 1], '^(TestAlpha|TestBeta)$');
  });
  await check('report records per-shard test seconds for rebalancing', () => {
    const report = aggregateShards(reportOptions(artifacts));
    assert.equal(report.shardTestSeconds.length, 3);
    assert.ok(report.shardTestSeconds.every(value => Number.isFinite(value) && value >= 0));
  });
  await check('all shard, guard and ingestor outcomes are strictly successful', () => {
    for (const key of ['GO_GUARD_RESULT', 'GO_INGESTOR_RESULT', 'GO_SERVER_1_RESULT', 'GO_SERVER_2_RESULT', 'GO_SERVER_3_RESULT']) {
      for (const value of ['failure', 'skipped', 'cancelled', '']) {
        const options = reportOptions(artifacts); options.env[key] = value;
        assert.throws(() => aggregateShards(options), /succeed/);
      }
    }
  });
  await check('stale run/head and future attempts fail; carried-over success is valid', () => {
    requireContext({ runId: '17', headSha: context.GITHUB_SHA, attempt: '1' }, { runId: '17', headSha: context.GITHUB_SHA, attempt: '2' });
    assert.throws(() => requireContext({ runId: '18', headSha: context.GITHUB_SHA, attempt: '1' }, { runId: '17', headSha: context.GITHUB_SHA, attempt: '2' }), /another run or head/);
    assert.throws(() => requireContext({ runId: '17', headSha: 'b'.repeat(40), attempt: '1' }, { runId: '17', headSha: context.GITHUB_SHA, attempt: '2' }), /another run or head/);
    assert.throws(() => requireContext({ runId: '17', headSha: context.GITHUB_SHA, attempt: '3' }, { runId: '17', headSha: context.GITHUB_SHA, attempt: '2' }), /future attempt/);
  });
  await check('artifact corruption, stale manifest and incomplete receipt fail', () => {
    const file = path.join(artifacts, 'go-server-1/receipt.json');
    const original = fs.readFileSync(file, 'utf8');
    const bad = JSON.parse(original); bad.manifestSha256 = 'b'.repeat(64);
    try {
      fs.writeFileSync(file, JSON.stringify(bad));
      assert.throws(() => aggregateShards(reportOptions(artifacts)), /stale Go manifest/);
      bad.manifestSha256 = JSON.parse(original).manifestSha256;
      bad.completedAt = '2099-01-01T00:00:00Z';
      fs.writeFileSync(file, JSON.stringify(bad));
      assert.throws(() => aggregateShards(reportOptions(artifacts)), /from the future/);
      bad.completedAt = JSON.parse(original).completedAt;
      bad.manifestSha256 = JSON.parse(original).manifestSha256; bad.runs.pop();
      fs.writeFileSync(file, JSON.stringify(bad));
      assert.throws(() => aggregateShards(reportOptions(artifacts)), /missing Go build\/test/);
    } finally { fs.writeFileSync(file, original); }
    const profile = path.join(artifacts, 'go-server-1/profile.out');
    const raw = fs.readFileSync(profile, 'utf8');
    try {
      fs.appendFileSync(profile, '\n');
      assert.throws(() => aggregateShards(reportOptions(artifacts)), /coverage artifact differs/);
    } finally { fs.writeFileSync(profile, raw); }
  });
  await check('missing and extra artifact folders fail closed', () => {
    const folder = path.join(artifacts, 'go-server-3');
    fs.renameSync(folder, folder + '-missing');
    try { assert.throws(() => aggregateShards(reportOptions(artifacts)), /missing or unexpected/); }
    finally { fs.renameSync(folder + '-missing', folder); }
    fs.mkdirSync(path.join(artifacts, 'go-server-4'));
    try { assert.throws(() => aggregateShards(reportOptions(artifacts)), /missing or unexpected/); }
    finally { fs.rmdirSync(path.join(artifacts, 'go-server-4')); }
  });
  await check('ingestor coverage keeps its own profile and rejects stale or corrupt data', () => {
    const input = path.join(temporary, 'ingestor-input');
    fs.mkdirSync(path.join(input, 'cmd/ingestor'), { recursive: true });
    const profile = 'mode: set\ngithub.com/corescope/ingestor/main.go:1.1,2.1 2 1\n';
    fs.writeFileSync(path.join(input, 'cmd/ingestor/ingestor-coverage.out'), profile);
    writeIngestorReceipt(input, context);
    const folder = path.join(input, '.ci-go/ingestor');
    const output = path.join(temporary, 'ingestor-output');
    const env = { ...context, GO_INGESTOR_RESULT: 'success' };
    verifyIngestorArtifact(output, folder, env);
    assert.equal(fs.readFileSync(path.join(output, 'cmd/ingestor/ingestor-coverage.out'), 'utf8'), profile);
    assert.throws(() => verifyIngestorArtifact(output, folder, { ...env, GITHUB_SHA: 'b'.repeat(40) }), /another run or head/);
    assert.throws(() => verifyIngestorArtifact(output, folder, { ...env, GO_INGESTOR_RESULT: 'skipped' }), /succeed/);
    fs.appendFileSync(path.join(folder, 'profile.out'), '\n');
    assert.throws(() => verifyIngestorArtifact(output, folder, env), /coverage artifact differs/);
  });
  await check('real go test runs every fixture entry exactly once across the three shards', async () => {
    // No fakes: the real runner, go build, go test -race -run/-skip and the
    // real aggregator on the fixture module, including a Test that has no pin.
    fs.writeFileSync(path.join(root, 'cmd/server/late_test.go'), 'package main\nimport "testing"\nfunc TestLate(t *testing.T) { t.Run("sub", func(t *testing.T) { if covered() != 1 { t.Fatal() } }) }\n');
    const realArtifacts = path.join(temporary, 'real-artifacts');
    try {
      const current = discoverTests(root);
      for (let shard = 1; shard <= 3; shard++) {
        const receipt = await runShard({ root, manifestFile, shard, inventory: current, outputDir: path.join(realArtifacts, `go-server-${shard}`), env: { ...process.env, ...context } });
        assert.equal(receipt.status, 'success');
      }
      const report = aggregateShards({ ...reportOptions(realArtifacts), inventory: current });
      const ran = report.receipts.flatMap(receipt => receipt.tests.map(test => test.name)).sort();
      assert.deepEqual(ran, ['ExampleAnswer', 'FuzzSmoke', 'TestAlpha', 'TestBeta', 'TestGamma', 'TestLate']);
      assert.deepEqual(report.receipts[2].tests.map(test => test.name), ['TestLate', 'TestGamma', 'FuzzSmoke', 'ExampleAnswer']);
      const subtests = fs.readFileSync(path.join(realArtifacts, 'go-server-3/test.jsonl'), 'utf8');
      assert.ok(subtests.includes('"Test":"TestLate/sub"') && subtests.includes('"Test":"FuzzSmoke/seed#0"'), 'subtests and fuzz seeds of the catch-all still run');
      assert.ok(report.coveredStatements > 0 && report.coveredStatements < report.totalStatements, 'uncovered() stays uncovered');
    } finally { fs.unlinkSync(path.join(root, 'cmd/server/late_test.go')); }
  });
  await check('rebalance reads CI go test -json, pins the heavy entries and leaves the rest to the catch-all', () => {
    const event = (Action, Test, Elapsed) => JSON.stringify({ Action, Package: PACKAGE, ...(Test ? { Test } : {}), ...(Elapsed !== undefined ? { Elapsed } : {}) });
    const write = (file, tests) => {
      fs.mkdirSync(path.dirname(file), { recursive: true });
      fs.writeFileSync(file, [event('start'), ...Object.entries(tests).flatMap(([name, seconds]) => [event('run', name), event('pass', name + '/sub', 0), event(name === 'TestBeta' ? 'skip' : 'pass', name, seconds)]), event('pass', undefined, 1)].join('\n') + '\n');
    };
    const runA = path.join(temporary, 'rebalance/run-a');
    const runB = path.join(temporary, 'rebalance/run-b');
    write(path.join(runA, 'go-server-1/test.jsonl'), { TestAlpha: 40, FuzzSmoke: 0.5 });
    write(path.join(runA, 'go-server-2/test.jsonl'), { TestBeta: 30, TestGamma: 10, TestRemoved: 99 });
    write(path.join(runB, 'go-server-1/test.jsonl'), { TestAlpha: 60, TestBeta: 30 });
    write(path.join(runB, 'go-server-3/test.jsonl'), { TestGamma: 12, FuzzSmoke: 0.5 });
    const timings = readTimingInputs([runA, runB]);
    assert.deepEqual(timings.samples.get('TestAlpha'), [40, 60]);
    assert.deepEqual(timings.samples.get('TestBeta'), [30, 30], 'skips keep their elapsed time');
    assert.ok(!timings.samples.has('TestAlpha/sub'), 'subtests are inside their parent');
    assert.equal(timings.sources.length, 2);
    assert.ok(timings.sources[0].files.every(file => /^[0-9a-f]{64}$/.test(file.sha256)));

    const plan = planShards({ inventory, samples: timings.samples, shards: 3 });
    // Total 50 + 30 + 11 + 0.5 = 91.5; the catch-all takes the cheap tail up
    // to a third, the heavy entries are spread over the pinned shards.
    assert.deepEqual(plan.pinned.map(pin => [pin.name, pin.shard]), [['TestAlpha', 1], ['TestBeta', 2]]);
    assert.deepEqual(plan.projectedSeconds, [50, 30, 11.5]);
    assert.deepEqual(plan.unmeasured, ['ExampleAnswer']);
    const generated = buildManifest({ plan, sources: timings.sources, shards: 3 });
    validateManifest(generated, root, inventory);
    assert.deepEqual(generated.pinned[0], { name: 'TestAlpha', shard: 1, samplesSeconds: [40, 60], meanSeconds: 50 });
    assert.ok(!JSON.stringify(generated).includes('TestRemoved'), 'removed Tests are not pinned again');
    assert.deepEqual(buildManifest({ plan: planShards({ inventory, samples: timings.samples, shards: 3 }), sources: timings.sources, shards: 3 }), generated, 'output is deterministic');

    write(path.join(runA, 'go-server-3/test.jsonl'), { TestAlpha: 1 });
    assert.throws(() => readTimingInputs([runA]), /TestAlpha ran twice in one CI run/);
    assert.throws(() => readTimingInputs([path.join(temporary, 'rebalance/none')]), /no test\.jsonl/);
  });
  console.log(`Go shard infrastructure: ${passed}/${passed} behavioural cases passed`);
}

main().catch(error => { console.error(error); process.exitCode = 1; }).finally(() => fs.rmSync(temporary, { recursive: true, force: true }));
