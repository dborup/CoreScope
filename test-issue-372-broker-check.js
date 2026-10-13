'use strict';
// Issue #372 — manage.sh must be able to tell an operator, before a start
// or an update, whether the broker it is about to run can actually ingest
// anything, and it must never print a credential while doing so.
//
// Every case runs the shipped manage.sh in a throwaway directory with a
// stub `docker` on PATH. No daemon, container, network, production path or
// real database is contacted.
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

// A sandbox holds a copy of manage.sh, a data dir, a .env, and a stub
// `docker` so manage.sh's compose detection succeeds without a daemon.
function sandbox({ env = {}, config = null }) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'cs372-'));
  fs.copyFileSync(path.join(root, 'manage.sh'), path.join(dir, 'manage.sh'));
  fs.chmodSync(path.join(dir, 'manage.sh'), 0o755);
  for (const name of ['docker-compose.yml', 'docker-compose.no-mosquitto.yml']) {
    fs.copyFileSync(path.join(root, name), path.join(dir, name));
  }
  const data = path.join(dir, 'data');
  fs.mkdirSync(data, { recursive: true });
  if (config !== null) fs.writeFileSync(path.join(data, 'config.json'), config);

  const bin = path.join(dir, 'bin');
  fs.mkdirSync(bin);
  // `docker compose version` must succeed; nothing else is called.
  fs.writeFileSync(path.join(bin, 'docker'), '#!/bin/sh\nexit 0\n');
  fs.chmodSync(path.join(bin, 'docker'), 0o755);

  const lines = Object.entries({ PROD_DATA_DIR: data, ...env })
    .map(([k, v]) => `${k}=${v}`).join('\n');
  fs.writeFileSync(path.join(dir, '.env'), lines + '\n');
  return { dir, bin, data };
}

function brokerCheck(opts) {
  const sb = sandbox(opts);
  const res = spawnSync('bash', ['manage.sh', 'broker-check'], {
    cwd: sb.dir,
    encoding: 'utf8',
    env: { PATH: sb.bin + ':' + process.env.PATH, HOME: sb.dir, TERM: 'dumb' },
  });
  return { ...res, out: (res.stdout || '') + (res.stderr || '') };
}

const externalWithGoodBroker = JSON.stringify({
  mqttSources: [{ name: 'ext', broker: 'mqtt://broker.example:1883', topics: ['meshcore/#'] }],
});

test('bundled mode with no external config is reported as fine', () => {
  const r = brokerCheck({ env: {}, config: null });
  assert.strictEqual(r.status, 0, 'expected exit 0, got ' + r.status + '\n' + r.out);
  assert.match(r.out, /bundled/i, r.out);
  assert.doesNotMatch(r.out, /\berror\b/i, r.out);
});

test('external mode with no configured source is an error', () => {
  const r = brokerCheck({ env: { DISABLE_MOSQUITTO: 'true' }, config: null });
  assert.strictEqual(r.status, 1, 'expected exit 1, got ' + r.status + '\n' + r.out);
  assert.match(r.out, /DISABLE_MOSQUITTO/, r.out);
  assert.match(r.out, /mqttSources/, r.out);
});

test('external mode with only loopback sources is an error', () => {
  const config = JSON.stringify({
    mqttSources: [{ name: 'local', broker: 'mqtt://127.0.0.1:1883', topics: ['meshcore/#'] }],
  });
  const r = brokerCheck({ env: { DISABLE_MOSQUITTO: 'true' }, config });
  assert.strictEqual(r.status, 1, 'expected exit 1, got ' + r.status + '\n' + r.out);
  assert.match(r.out, /loopback/i, r.out);
});

test('external mode with a real broker passes', () => {
  const r = brokerCheck({ env: { DISABLE_MOSQUITTO: 'true' }, config: externalWithGoodBroker });
  assert.strictEqual(r.status, 0, 'expected exit 0, got ' + r.status + '\n' + r.out);
  assert.match(r.out, /broker\.example/, r.out);
});

test('the legacy single-broker config block counts as a source', () => {
  const config = JSON.stringify({ mqtt: { broker: 'mqtt://legacy.example:1883', topic: 'meshcore/#' } });
  const r = brokerCheck({ env: { DISABLE_MOSQUITTO: 'true' }, config });
  assert.strictEqual(r.status, 0, 'expected exit 0, got ' + r.status + '\n' + r.out);
});

test('MQTT_BROKER overrides a loopback-only config.json', () => {
  const config = JSON.stringify({
    mqttSources: [{ name: 'local', broker: 'mqtt://localhost:1883', topics: ['meshcore/#'] }],
  });
  const r = brokerCheck({
    env: { DISABLE_MOSQUITTO: 'true', MQTT_BROKER: 'mqtt://env.example:1883' },
    config,
  });
  assert.strictEqual(r.status, 0, 'env precedence must match the ingestor; got ' + r.status + '\n' + r.out);
  assert.match(r.out, /env\.example/, r.out);
});

test('a DISABLE_MOSQUITTO value the entrypoint does not accept is reported', () => {
  // manage.sh's is_true() takes 'yes'; the container entrypoint only takes
  // the exact string 'true'. The two then disagree about which broker runs.
  const r = brokerCheck({ env: { DISABLE_MOSQUITTO: 'yes' }, config: externalWithGoodBroker });
  assert.match(r.out, /DISABLE_MOSQUITTO/, r.out);
  assert.match(r.out, /yes/, r.out);
  assert.match(r.out, /bundled/i, r.out);
  assert.strictEqual(r.status, 0, 'an ambiguous value is a warning, not a failure\n' + r.out);
});

test('an ambiguous value reports the mode the container will actually be in', () => {
  // DISABLE_MOSQUITTO=yes: the entrypoint starts the bundled broker, so the
  // report must say bundled — and must not demand an external source that
  // the container is not going to need.
  const r = brokerCheck({ env: { DISABLE_MOSQUITTO: 'yes' }, config: null });
  assert.match(r.out, /Broker mode: bundled/, r.out);
  assert.doesNotMatch(r.out, /Broker mode: external/, r.out);
  assert.strictEqual(r.status, 0, 'the bundled broker needs no external source\n' + r.out);
});

test('an unambiguous value produces no ambiguity warning', () => {
  for (const value of ['true', 'false']) {
    const r = brokerCheck({ env: { DISABLE_MOSQUITTO: value }, config: externalWithGoodBroker });
    assert.doesNotMatch(r.out, /neither/i, 'DISABLE_MOSQUITTO=' + value + ':\n' + r.out);
  }
});

test('broker credentials never reach the output', () => {
  const config = JSON.stringify({
    mqttSources: [
      { name: 'ext', broker: 'mqtt://observer-rw:sup3rs3cret@broker.example:1883?token=tok-abcdef', topics: ['meshcore/#'] },
      { name: 'loop', broker: 'mqtt://observer-rw:sup3rs3cret@localhost:1883', topics: ['meshcore/#'] },
    ],
  });
  const r = brokerCheck({ env: { DISABLE_MOSQUITTO: 'true' }, config });
  for (const secret of ['sup3rs3cret', 'observer-rw', 'tok-abcdef']) {
    assert.ok(!r.out.includes(secret), 'leaked ' + secret + ':\n' + r.out);
  }
  assert.match(r.out, /\*\*\*\*/, 'a removed credential must leave a marker:\n' + r.out);
});

test('mask_broker_text strips credentials out of a log line', () => {
  // verify_health echoes matching lines straight out of `docker logs`, which
  // can come from an ingestor of any version. They go through the filter.
  const sb = sandbox({ env: {}, config: null });
  const line = 'ERROR mqtt connect tcp://observer-rw:sup3rs3cret@broker.example:1883?token=tok-abcdef refused';
  const res = spawnSync('bash', ['-c',
    'L="$1"; set -- help; . ./manage.sh >/dev/null 2>&1; printf "%s\\n" "$L" | mask_broker_text', 'sh', line], {
    cwd: sb.dir,
    encoding: 'utf8',
    env: { PATH: sb.bin + ':' + process.env.PATH, HOME: sb.dir, TERM: 'dumb' },
  });
  const out = (res.stdout || '') + (res.stderr || '');
  for (const secret of ['sup3rs3cret', 'observer-rw', 'tok-abcdef']) {
    assert.ok(!out.includes(secret), 'leaked ' + secret + ':\n' + out);
  }
  assert.match(out, /\*\*\*\*@broker\.example/, out);
  assert.match(out, /refused/, 'the rest of the line must survive:\n' + out);
});

test('verify_health masks the MQTT log lines it echoes', () => {
  const src = fs.readFileSync(path.join(root, 'manage.sh'), 'utf8');
  const block = src.slice(src.indexOf('verify_health()'));
  const echoLine = block.split('\n').find((l) => /echo "\$mqtt_errors"/.test(l));
  assert.ok(echoLine, 'verify_health must still echo the MQTT errors it found');
  assert.ok(/mask_broker_text/.test(echoLine),
    'the echoed log lines must pass through mask_broker_text: ' + echoLine);
});

test('start, update, restore, promote and setup all run the broker preflight', () => {
  const src = fs.readFileSync(path.join(root, 'manage.sh'), 'utf8');
  for (const fn of ['cmd_setup', 'cmd_start', 'cmd_update', 'cmd_restore', 'cmd_promote']) {
    const start = src.indexOf(fn + '()');
    assert.ok(start > 0, 'missing ' + fn);
    // Each command body runs until the next top-level function definition.
    const rest = src.slice(start + fn.length);
    const end = rest.search(/\n[a-z_]+\(\) \{/);
    const body = end < 0 ? rest : rest.slice(0, end);
    assert.ok(/broker_check_report/.test(body),
      fn + ' must run broker_check_report so a broker that cannot ingest is reported before the change');
  }
});

test('the preflight never blocks a command', () => {
  // A broker diagnostic must not turn into a refusal to start: these hosts
  // differ and the owner decides. Only the explicit broker-check exits 1.
  const src = fs.readFileSync(path.join(root, 'manage.sh'), 'utf8');
  for (const line of src.split('\n')) {
    if (!line.includes('broker_check_report')) continue;
    if (line.trimStart().startsWith('#')) continue;
    if (/^broker_check_report\(\)/.test(line.trim())) continue;
    // `if broker_check_report ...` handles the result explicitly; only the
    // advisory call sites must be non-blocking.
    if (/^if /.test(line.trim())) continue;
    assert.ok(/\|\| true/.test(line),
      'broker_check_report must be called non-blocking (|| true): ' + line.trim());
  }
});

test('broker-check is discoverable in the help text', () => {
  const sb = sandbox({ env: {}, config: null });
  const res = spawnSync('bash', ['manage.sh', 'help'], {
    cwd: sb.dir, encoding: 'utf8',
    env: { PATH: sb.bin + ':' + process.env.PATH, HOME: sb.dir, TERM: 'dumb' },
  });
  assert.match((res.stdout || '') + (res.stderr || ''), /broker-check/, 'help must list broker-check');
});

console.log('\n' + passed + ' passed, ' + failed + ' failed');
process.exit(failed === 0 ? 0 : 1);
