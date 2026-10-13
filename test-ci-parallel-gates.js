// Regression checks for the required CI gate. A skipped required job can be
// treated as passing by GitHub branch protection, so failed dependencies must
// cause a red build-and-publish check rather than skipping it.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const { spawnSync } = require('node:child_process');

const workflow = fs.readFileSync('.github/workflows/deploy.yml', 'utf8');

function job(name) {
  const marker = `\n  ${name}:\n`;
  const start = workflow.indexOf(marker);
  assert.notEqual(start, -1, `missing ${name} job`);
  const remainder = workflow.slice(start + marker.length);
  const next = remainder.search(/\n  [a-z][a-z-]*:\n/);
  return next < 0 ? remainder : remainder.slice(0, next);
}

const go = job('go-test');
const e2e = job('e2e-test');
const image = job('image-check');
const publish = job('build-and-publish');

assert.ok(!/^    needs:/m.test(go), 'Go tests must start independently');
assert.ok(!/^    needs:/m.test(e2e), 'E2E must not wait for Go tests');
assert.ok(!/^    needs:/m.test(image), 'image check must start independently');
assert.match(image, /docker compose .* build/, 'image check must build the staging image');
assert.match(publish, /^    needs: \[go-test, e2e-test, image-check\]$/m);
// A cancelled run reports the gate as cancelled (not skipped), which does
// not pass, so !cancelled() is enough.
assert.match(publish, /^    if: \$\{\{ !cancelled\(\) \}\}$/m, 'gate must run after failed or skipped prerequisites');
assert.match(publish, /github\.repository == 'Kpa-clawbot\/CoreScope'/);

// The gate job runs after failures, so only the implicit success() of each
// later step keeps publishing behind the gate step. No step may override it.
assert.doesNotMatch(publish, /^        if:.*\b(always|cancelled|failure)\(\)/m, 'publish steps must stay behind the gate step');

const gate = publish.match(/      - name: Require tests and image validation\n([\s\S]*?)(?=      - name: Checkout code)/);
assert.ok(gate, 'stable required check needs an explicit gate');
const script = gate[1].match(/^        run: \|\n((?:^          .*\n)+)/m);
assert.ok(script, 'missing gate script');
const bash = script[1].replace(/^          /gm, '');

for (const [goResult, e2eResult, imageResult, expected] of [
  ['success', 'success', 'success', 0],
  ['failure', 'success', 'success', 1],
  ['success', 'failure', 'success', 1],
  ['success', 'success', 'failure', 1],
  ['skipped', 'success', 'success', 1],
  ['success', 'cancelled', 'success', 1],
  ['success', 'success', 'skipped', 1],
  ['cancelled', 'success', 'success', 1],
  ['success', 'success', 'cancelled', 1],
  ['', 'success', 'success', 1],
]) {
  const result = spawnSync('bash', ['-e', '-c', bash], {
    env: { ...process.env, GO_RESULT: goResult, E2E_RESULT: e2eResult, IMAGE_RESULT: imageResult },
    encoding: 'utf8',
  });
  assert.equal(result.status, expected, `${goResult}/${e2eResult}/${imageResult}: ${result.stderr}`);
}

console.log('CI parallelism and fail-closed gates: PASS');

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
