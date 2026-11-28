'use strict';
// Issue #441 — the bundled mosquitto is opt-in, and DISABLE_MOSQUITTO means
// the same thing to every launcher.
//
// The three readers — docker/entrypoint-go.sh (inside the image), manage.sh
// (on the host) and the ingestor (cmd/ingestor, tested in Go) — are driven
// here, for the shell two, over the one table in
// test-fixtures/disable-mosquitto-cases.json that the Go test also loads.
//
// The entrypoint is the shipped file with its absolute paths pointed into a
// throwaway directory and a stub supervisord that prints the config it was
// handed. manage.sh runs in a sandbox with a stub `docker` that records its
// argv. No daemon, container, network or real data directory is touched.
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

const CASES = JSON.parse(fs.readFileSync(path.join(root, 'test-fixtures', 'disable-mosquitto-cases.json'), 'utf8')).cases;
const show = (v) => (v === null ? '<unset>' : JSON.stringify(v));

// ─── docker/entrypoint-go.sh ─────────────────────────────────────────────

// Absolute paths in the entrypoint, and where the harness puts them. Each
// must be present: if one moves, this harness must be updated rather than
// silently running the entrypoint against the real filesystem.
function entrypointSandbox() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'cs441-ep-'));
  fs.mkdirSync(path.join(dir, 'app', 'data'), { recursive: true });
  const stub = path.join(dir, 'supervisord');
  fs.writeFileSync(stub, '#!/bin/sh\necho "argv: $*"\n');
  fs.chmodSync(stub, 0o755);
  const nonroot = path.join(dir, 'nonroot.sh');
  fs.writeFileSync(nonroot, '#!/bin/sh\necho "nonroot helper must not run in this test" >&2\nexit 1\n');
  fs.chmodSync(nonroot, 0o755);

  let src = fs.readFileSync(path.join(root, 'docker', 'entrypoint-go.sh'), 'utf8');
  for (const [from, to] of [
    ['/usr/bin/supervisord', stub],
    ['/usr/local/lib/corescope/nonroot.sh', nonroot],
    ['/app/', path.join(dir, 'app') + '/'],
  ]) {
    assert.ok(src.includes(from), 'docker/entrypoint-go.sh no longer mentions ' + from + '; update this harness');
    src = src.split(from).join(to);
  }
  fs.writeFileSync(path.join(dir, 'entrypoint.sh'), src);
  return dir;
}

function runEntrypoint({ procEnv = {}, dataEnv = null } = {}) {
  const dir = entrypointSandbox();
  if (dataEnv !== null) fs.writeFileSync(path.join(dir, 'app', 'data', '.env'), dataEnv);
  const res = spawnSync('sh', [path.join(dir, 'entrypoint.sh')], {
    cwd: dir,
    encoding: 'utf8',
    env: { PATH: process.env.PATH, ...procEnv },
  });
  const out = (res.stdout || '') + (res.stderr || '');
  const m = out.match(/^argv: -c \/etc\/supervisor\/conf\.d\/(\S+)$/m);
  return { status: res.status, out, conf: m ? m[1] : null };
}

function entrypointFor(value, extra = {}) {
  const procEnv = { ...extra };
  if (value !== null) procEnv.DISABLE_MOSQUITTO = value;
  return runEntrypoint({ procEnv });
}

test('entrypoint: every DISABLE_MOSQUITTO value selects the table\'s broker mode', () => {
  for (const c of CASES) {
    const r = entrypointFor(c.value);
    assert.strictEqual(r.status, 0, show(c.value) + ':\n' + r.out);
    const want = c.disabled ? 'supervisord-no-mosquitto.conf' : 'supervisord.conf';
    assert.strictEqual(r.conf, want, 'DISABLE_MOSQUITTO=' + show(c.value) + ':\n' + r.out);
    if (c.disabled) assert.match(r.out, /internal MQTT broker disabled/, show(c.value) + ':\n' + r.out);
    else assert.doesNotMatch(r.out, /internal MQTT broker disabled/, show(c.value) + ':\n' + r.out);
  }
});

test('entrypoint: a junk value is reported and falls back to the default', () => {
  for (const c of CASES) {
    const r = entrypointFor(c.value);
    if (c.recognised) {
      assert.doesNotMatch(r.out, /not a recognised/i, show(c.value) + ' is recognised:\n' + r.out);
    } else {
      assert.match(r.out, /DISABLE_MOSQUITTO/, r.out);
      assert.match(r.out, /not a recognised/i, show(c.value) + ' is junk and must be reported:\n' + r.out);
      assert.match(r.out, /default/i, 'the report must say the default applies:\n' + r.out);
    }
  }
});

test('entrypoint: DISABLE_CADDY still combines with the normalised value', () => {
  const both = entrypointFor('Yes', { DISABLE_CADDY: 'true' });
  assert.strictEqual(both.conf, 'supervisord-no-mosquitto-no-caddy.conf', both.out);
  const unsetBroker = entrypointFor(null, { DISABLE_CADDY: 'true' });
  assert.strictEqual(unsetBroker.conf, 'supervisord-no-mosquitto-no-caddy.conf', unsetBroker.out);
  const caddyOnly = entrypointFor('OFF', { DISABLE_CADDY: 'true' });
  assert.strictEqual(caddyOnly.conf, 'supervisord-no-caddy.conf', caddyOnly.out);
});

test('entrypoint: the data-volume .env is read with the same rules', () => {
  // docker/entrypoint-go.sh sources /app/data/.env after the container
  // environment is set, so a value there overrides compose's.
  const on = runEntrypoint({ procEnv: { DISABLE_MOSQUITTO: 'false' }, dataEnv: 'DISABLE_MOSQUITTO=YES\n' });
  assert.strictEqual(on.conf, 'supervisord-no-mosquitto.conf', on.out);
  const off = runEntrypoint({ procEnv: { DISABLE_MOSQUITTO: 'true' }, dataEnv: 'DISABLE_MOSQUITTO=Off\n' });
  assert.strictEqual(off.conf, 'supervisord.conf', off.out);
});

// ─── manage.sh ───────────────────────────────────────────────────────────

const externalWithGoodBroker = JSON.stringify({
  mqttSources: [{ name: 'ext', broker: 'mqtt://broker.example:1883', topics: ['meshcore/#'] }],
});

// `route` is how the value reaches manage.sh: through .env (its own reader
// trims it) or through the process environment (delivered verbatim).
const manageCache = new Map();
function manage(args, opts) {
  const key = JSON.stringify([args, opts]);
  if (!manageCache.has(key)) manageCache.set(key, runManage(args, opts));
  return manageCache.get(key);
}

function runManage(args, { value, route = 'env', config = externalWithGoodBroker }) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'cs441-ms-'));
  fs.copyFileSync(path.join(root, 'manage.sh'), path.join(dir, 'manage.sh'));
  for (const name of ['docker-compose.yml', 'docker-compose.no-mosquitto.yml']) {
    fs.copyFileSync(path.join(root, name), path.join(dir, name));
  }
  const data = path.join(dir, 'data');
  fs.mkdirSync(data);
  if (config !== null) fs.writeFileSync(path.join(data, 'config.json'), config);
  const bin = path.join(dir, 'bin');
  fs.mkdirSync(bin);
  const argvLog = path.join(dir, 'docker-argv.log');
  fs.writeFileSync(path.join(bin, 'docker'), '#!/bin/sh\necho "$*" >> "' + argvLog + '"\nexit 0\n');
  fs.chmodSync(path.join(bin, 'docker'), 0o755);

  const envLines = ['PROD_DATA_DIR=' + data];
  const procEnv = {};
  if (value !== null) {
    if (route === 'env') envLines.push('DISABLE_MOSQUITTO=' + value);
    else procEnv.DISABLE_MOSQUITTO = value;
  }
  fs.writeFileSync(path.join(dir, '.env'), envLines.join('\n') + '\n');

  const res = spawnSync('bash', ['manage.sh', ...args], {
    cwd: dir,
    encoding: 'utf8',
    env: { PATH: bin + ':' + process.env.PATH, HOME: dir, TERM: 'dumb', ...procEnv },
  });
  const argv = fs.existsSync(argvLog) ? fs.readFileSync(argvLog, 'utf8') : '';
  return { status: res.status, out: (res.stdout || '') + (res.stderr || ''), argv };
}

// .env cannot carry a value whose meaning depends on a leading or trailing
// tab the reader strips, but every case can travel through the process env.
const ROUTES = ['env', 'proc'];

test('manage.sh: broker-check reports the table\'s mode for every value and route', () => {
  for (const c of CASES) {
    for (const route of ROUTES) {
      const r = manage(['broker-check'], { value: c.value, route });
      const want = c.disabled ? 'external' : 'bundled';
      const other = c.disabled ? 'bundled' : 'external';
      assert.match(r.out, new RegExp('Broker mode: ' + want), show(c.value) + ' via ' + route + ':\n' + r.out);
      assert.doesNotMatch(r.out, new RegExp('Broker mode: ' + other), show(c.value) + ' via ' + route + ':\n' + r.out);
    }
  }
});

test('manage.sh: dc_prod picks the compose file the table implies', () => {
  for (const c of CASES) {
    for (const route of ROUTES) {
      const r = manage(['logs', 'prod'], { value: c.value, route });
      const line = r.argv.split('\n').find((l) => /\blogs\b/.test(l)) || '';
      assert.ok(line, 'no `docker compose ... logs` call recorded:\n' + r.argv + '\n' + r.out);
      const noMosq = /-f docker-compose\.no-mosquitto\.yml/.test(line);
      assert.strictEqual(noMosq, c.disabled,
        'DISABLE_MOSQUITTO=' + show(c.value) + ' via ' + route + ' ran: ' + line);
    }
  }
});

test('manage.sh: no launcher-mismatch warning is left; junk is reported once', () => {
  for (const c of CASES) {
    for (const route of ROUTES) {
      const r = manage(['broker-check'], { value: c.value, route });
      assert.doesNotMatch(r.out, /different things to different launchers|is_true\(\)/,
        show(c.value) + ' via ' + route + ': parsing is consistent, so no mismatch may be reported:\n' + r.out);
      if (c.recognised) {
        assert.doesNotMatch(r.out, /not a recognised/i, show(c.value) + ' via ' + route + ':\n' + r.out);
      } else {
        assert.match(r.out, /not a recognised/i, show(c.value) + ' via ' + route + ' is junk:\n' + r.out);
      }
    }
  }
});

test('manage.sh: broker-check names the effective value it acted on', () => {
  const yes = manage(['broker-check'], { value: 'YES', route: 'env' });
  assert.match(yes.out, /Broker mode: external \(DISABLE_MOSQUITTO=YES, read as true\)/, yes.out);
  const off = manage(['broker-check'], { value: 'Off', route: 'env' });
  assert.match(off.out, /Broker mode: bundled mosquitto \(DISABLE_MOSQUITTO=Off, read as false\)/, off.out);
  const unset = manage(['broker-check'], { value: null });
  assert.match(unset.out, /Broker mode: external \(DISABLE_MOSQUITTO unset, default true\)/, unset.out);
  const junk = manage(['broker-check'], { value: 'ture', route: 'env' });
  assert.match(junk.out, /Broker mode: external \(DISABLE_MOSQUITTO=ture, not recognised, default true\)/, junk.out);
});

test('manage.sh: an unset DISABLE_MOSQUITTO with no source fails the check (opt-in broker)', () => {
  const r = manage(['broker-check'], { value: null, config: null });
  assert.strictEqual(r.status, 1, 'unset now means external, so a missing source must fail:\n' + r.out);
  assert.match(r.out, /DISABLE_MOSQUITTO=false/, 'the remediation must name the opt-in:\n' + r.out);
  const optedIn = manage(['broker-check'], { value: 'false', config: null });
  assert.strictEqual(optedIn.status, 0, optedIn.out);
});

test('manage.sh: the setup wizard defaults to no bundled broker', () => {
  const src = fs.readFileSync(path.join(root, 'manage.sh'), 'utf8');
  const prompt = src.match(/^\s*if (confirm\w*) "Use built-in MQTT broker\?"; then$/m);
  assert.ok(prompt, 'the setup wizard must still ask about the built-in broker');
  assert.strictEqual(prompt[1], 'confirm',
    'the built-in broker is opt-in, so the prompt must default to No (confirm, not confirm_yes_default)');
  assert.doesNotMatch(src, /DISABLE_MOSQUITTO:-false/, 'no manage.sh default may still be false');
});

// ─── shipped defaults ────────────────────────────────────────────────────

test('.env.example ships DISABLE_MOSQUITTO=true with that default documented', () => {
  const env = fs.readFileSync(path.join(root, '.env.example'), 'utf8');
  assert.match(env, /^DISABLE_MOSQUITTO=true$/m, env);
  const lines = env.split('\n');
  const i = lines.findIndex((l) => l.startsWith('DISABLE_MOSQUITTO='));
  const block = lines.slice(Math.max(0, i - 8), i).join('\n');
  assert.match(block, /^# Default: true$/m, block);
});

console.log('\n' + passed + ' passed, ' + failed + ' failed');
process.exit(failed ? 1 : 0);
