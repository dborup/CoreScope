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

const go = job('go-test');
const shards = [1, 2, 3].map(number => job(`e2e-test-${number}`));
const report = job('e2e-report');
const image = job('image-check');
const publish = job('build-and-publish');
for (const [name, body] of [['Go', go], ['image', image], ...shards.map((body, index) => [`E2E shard ${index + 1}`, body])]) {
  assert.ok(!/^    needs:/m.test(body), `${name} must start independently`);
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
assert.match(publish, /^    needs: \[go-test, e2e-test-1, e2e-test-2, e2e-test-3, e2e-report, image-check\]$/m);
assert.match(publish, /^    if: \$\{\{ !cancelled\(\) \}\}$/m);
assert.match(publish, /github\.repository == 'Kpa-clawbot\/CoreScope'/);
assert.doesNotMatch(publish, /^        if:.*\b(always|cancelled|failure)\(\)/m);

const required = publish.match(/      - name: Require tests and image validation\n([\s\S]*?)(?=      - name: Checkout code)/);
assert.ok(required, 'stable gate needs its explicit prerequisite check');
const script = required[1].match(/^        run: \|\n((?:^          .*\n)+)/m);
assert.ok(script, 'missing gate script');
const bash = script[1].replace(/^          /gm, '');
const keys = ['GO_RESULT', 'E2E_1_RESULT', 'E2E_2_RESULT', 'E2E_3_RESULT', 'E2E_REPORT_RESULT', 'IMAGE_RESULT'];
const expectedBindings = ['go-test', 'e2e-test-1', 'e2e-test-2', 'e2e-test-3', 'e2e-report', 'image-check'];
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
console.log('CI independent shards and all-six fail-closed prerequisites: PASS');
