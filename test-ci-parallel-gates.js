// Regression checks for independent shards and the stable fail-closed gate.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const { spawnSync } = require('node:child_process');
const workflow = fs.readFileSync('.github/workflows/deploy.yml', 'utf8');

function job(name) {
  const marker = `\n  ${name}:\n`;
  const start = workflow.indexOf(marker);
  assert.notEqual(start, -1, `missing ${name} job`);
  const remainder = workflow.slice(start + marker.length);
  const next = remainder.search(/\n  [a-z][a-z0-9-]*:\n/);
  return next < 0 ? remainder : remainder.slice(0, next);
}

const count = JSON.parse(fs.readFileSync('scripts/go-server-shards.json', 'utf8')).shards;
assert.ok(count === 3 || count === 4, 'Go server must have three or four measured shards');
const goJobs = ['go-shards-check', ...Array.from({ length: count }, (_, i) => `go-server-${i + 1}`),
  'go-ingestor', 'go-libraries', 'js-test', 'script-test'];
const go = job('go-test');
const shards = [1, 2, 3].map(number => job(`e2e-test-${number}`));
const report = job('e2e-report');
const image = job('image-check');
const publish = job('build-and-publish');
for (const [name, body] of [...goJobs.map(name => [name, job(name)]), ['image', image], ...shards.map((body, index) => [`E2E shard ${index + 1}`, body])]) {
  assert.ok(!/^    needs:/m.test(body), `${name} must start independently`);
}
assert.ok(go.includes(`    needs: [${goJobs.join(', ')}]\n`), 'Go coverage must depend on every Go shard and split job');
assert.match(go, /^    if: \$\{\{ !cancelled\(\) \}\}$/m);
assert.match(go, /node scripts\/report-go-server-shards\.js/);
assert.match(job('go-shards-check'), /node scripts\/run-go-server-shard\.js --validate/);
assert.match(job('go-shards-check'), /--runtime-list/);
assert.match(job('go-server-1'), /^    steps: &go-server-shard-steps$/m);
assert.match(job('go-server-1'), /node scripts\/run-go-server-shard\.js\n/);
assert.match(job('go-server-1'), /name: go-server-\$\{\{ env\.GO_SERVER_SHARD \}\}\n          overwrite: true\n/);
for (let number = 1; number <= count; number++) {
  const body = job(`go-server-${number}`);
  assert.match(body, new RegExp(`^      GO_SERVER_SHARD: '${number}'$`, 'm'));
  if (number > 1) assert.match(body, /^    steps: \*go-server-shard-steps$/m);
}
for (const [index, shard] of shards.entries()) {
  assert.match(shard, new RegExp(`^      E2E_SHARD: '${index + 1}'$`, 'm'));
  assert.match(shard, new RegExp(`^      E2E_PORT: '${13581 + index}'$`, 'm'));
  assert.match(shard, new RegExp(`^      BASE_URL: http://localhost:${13581 + index}$`, 'm'));
}
assert.match(shards[0], /^    steps: &e2e-shard-steps$/m);
for (const shard of shards.slice(1)) assert.match(shard, /^    steps: \*e2e-shard-steps$/m);
assert.match(shards[0], /node scripts\/run-e2e-shard\.js --validate/);
assert.match(shards[0], /node scripts\/run-e2e-shard\.js\n/);
assert.match(shards[0], /name: e2e-shard-\$\{\{ env\.E2E_SHARD \}\}\n          overwrite: true\n/);
assert.match(shards[0], /github\.event_name == 'push' && steps\.shard-meta\.outputs\.core_shard == env\.E2E_SHARD/);
assert.match(report, /^    needs: \[e2e-test-1, e2e-test-2, e2e-test-3\]$/m);
assert.match(report, /^    if: \$\{\{ !cancelled\(\) \}\}$/m);
assert.match(report, /node scripts\/report-e2e-shards\.js/);
assert.match(report, /name: e2e-badges\n          overwrite: true\n/);
assert.match(image, /docker compose .* build/);
assert.match(publish, /^    name: "🏗️ Build & Publish Docker Image"$/m);
const prerequisites = [...goJobs, 'go-test', 'e2e-test-1', 'e2e-test-2', 'e2e-test-3', 'e2e-report', 'image-check'];
assert.ok(publish.includes(`    needs: [${prerequisites.join(', ')}]\n`), 'stable gate must require every split job');
assert.match(publish, /^    if: \$\{\{ !cancelled\(\) \}\}$/m);
assert.match(publish, /github\.repository == 'Kpa-clawbot\/CoreScope'/);
assert.doesNotMatch(publish, /^        if:.*\b(always|cancelled|failure)\(\)/m);

const required = publish.match(/      - name: Require tests and image validation\n([\s\S]*?)(?=      - name: Checkout code)/);
assert.ok(required, 'stable gate needs its explicit prerequisite check');
const script = required[1].match(/^        run: \|\n((?:^          .*\n)+)/m);
assert.ok(script, 'missing gate script');
const bash = script[1].replace(/^          /gm, '');
const goKeys = ['GO_GUARD_RESULT', ...Array.from({ length: count }, (_, i) => `GO_SERVER_${i + 1}_RESULT`),
  'GO_INGESTOR_RESULT', 'GO_LIBRARIES_RESULT', 'JS_RESULT', 'SCRIPTS_RESULT'];
const keys = [...goKeys, 'GO_RESULT', 'E2E_1_RESULT', 'E2E_2_RESULT', 'E2E_3_RESULT', 'E2E_REPORT_RESULT', 'IMAGE_RESULT'];
const expectedBindings = prerequisites;
for (const [index, key] of keys.entries()) {
  assert.ok(required[1].includes(`${key}: \${{ needs.${expectedBindings[index]}.result }}`), `missing needs binding for ${key}`);
}
const positive = Object.fromEntries(keys.map(key => [key, 'success']));
const cases = [{ values: positive, expected: 0 }];
for (const key of keys) for (const value of ['failure', 'skipped', 'cancelled', '']) {
  cases.push({ values: { ...positive, [key]: value }, expected: 1 });
}
for (const { values, expected } of cases) {
  const result = spawnSync('bash', ['-e', '-c', bash], { env: { ...process.env, ...values }, encoding: 'utf8' });
  assert.equal(result.status, expected, `${JSON.stringify(values)}: ${result.stderr}`);
}
// The Go/coverage aggregator itself must also fail closed, not just rely on
// the publisher. Execute its real prerequisite loop with every negative value.
const goGate = go.match(/      - name: Require all Go and split checks\n([\s\S]*?)(?=      - name: Checkout code)/);
assert.ok(goGate, 'Go report needs an explicit fail-closed prerequisite step');
const goScript = goGate[1].match(/^        run: \|\n((?:^          .*\n)+)/m);
assert.ok(goScript, 'Go report gate script is missing');
for (const [index, key] of goKeys.entries()) {
  assert.ok(goGate[1].includes(`${key}: \${{ needs.${goJobs[index]}.result }}`), `missing Go needs binding ${key}`);
}
const goPositive = Object.fromEntries(goKeys.map(key => [key, 'success']));
for (const values of [goPositive, ...goKeys.flatMap(key => ['failure', 'skipped', 'cancelled', '']
  .map(value => ({ ...goPositive, [key]: value })))]) {
  const result = spawnSync('bash', ['-e', '-c', goScript[1].replace(/^          /gm, '')], {
    env: { ...process.env, ...values }, encoding: 'utf8',
  });
  assert.equal(result.status, Object.values(values).every(value => value === 'success') ? 0 : 1,
    `Go report ${JSON.stringify(values)}: ${result.stderr}`);
}
// Follow-ups to #383 (#426).
// 1. Registry login/push steps must be fork-guarded in EVERY job, not only
//    build-and-publish (job-level or step-level guard).
const GUARD = "github.repository == 'Kpa-clawbot/CoreScope'";
const jobNames = [...workflow.slice(workflow.indexOf('\njobs:')).matchAll(/^  ([a-z][a-z0-9-]*):\s*$/gm)].map((m) => m[1]);
assert.ok(jobNames.length >= 5, 'parsed jobs: ' + jobNames);
for (const name of jobNames) {
  const body = job(name);
  const jobGuarded = /^    if:[\s\S]*?(?=^    [a-z-]+:)/m.exec(body)?.[0].includes(GUARD) ?? false;
  for (const step of body.split(/^      - /m).slice(1)) {
    const stepIf = step.match(/^        if:(.*)$/m);
    const stepGuarded = !!stepIf && stepIf[1].includes(GUARD);
    for (const marker of ['docker/login-action', 'docker/build-push-action', 'docker push', 'docker login', 'push: true']) {
      if (step.includes(marker)) {
        assert.ok(jobGuarded || stepGuarded, `${name}: step using ${marker} must be fork-guarded`);
      }
    }
  }
}
// 2. image-check is least-privilege. 3. It has a bounded timeout.
assert.match(image, /^    permissions:\n      contents: read\n/m, 'image-check must be contents: read only');
assert.doesNotMatch(image, /packages:\s*write/);
const timeout = image.match(/^    timeout-minutes: (\d+)$/m);
assert.ok(timeout && +timeout[1] >= 10 && +timeout[1] <= 30, 'image-check needs timeout-minutes of about 20');
console.log(`CI independent shards: ${cases.length} stable-gate cases and ${1 + 4 * goKeys.length} Go-gate cases PASS`);
