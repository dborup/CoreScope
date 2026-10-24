'use strict';
// Unit tests for #530: bounded, cancellable retry of the Reach GET on HTTP 429.
// Loads the real public/node-reach.js in a vm sandbox and drives
// window.NodeReach.fetchWithRetry / parseRetryAfter with injected fakes.
const assert = require('assert');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const registered = {};
const sandbox = {
  window: {}, document: {}, console: console,
  registerPage: function (n, m) { registered[n] = m; },
  escapeHtml: function (s) { return String(s); },
  setTimeout: setTimeout, clearTimeout: clearTimeout,
};
vm.createContext(sandbox);
vm.runInContext(fs.readFileSync(path.join(__dirname, 'public', 'node-reach.js'), 'utf8'), sandbox);
const NR = sandbox.window.NodeReach;
assert.ok(NR && typeof NR.fetchWithRetry === 'function', 'NodeReach.fetchWithRetry exported');
assert.ok(typeof NR.parseRetryAfter === 'function', 'NodeReach.parseRetryAfter exported');

const err = (status, ra) => { const e = new Error('API ' + status); e.status = status; if (ra !== undefined) e.retryAfter = ra; return e; };
// sleep stub: records the requested delay, resolves true (elapsed) at once.
function mkOpts(extra) {
  const o = { waits: [], busy: [], isCurrent: () => true };
  o.sleep = (ms) => { o.waits.push(ms); return Promise.resolve(true); };
  o.onBusy = (info) => o.busy.push(info);
  return Object.assign(o, extra || {});
}

const unhandled = [];
process.on('unhandledRejection', (r) => unhandled.push(r));

(async () => {
  // --- parseRetryAfter: header edge cases -----------------------------------
  const P = NR.parseRetryAfter;
  assert.strictEqual(P('3'), 3000, 'integer seconds');
  assert.strictEqual(P(' 4 '), 4000, 'whitespace trimmed');
  assert.strictEqual(P('1.5'), 1500, 'decimal seconds');
  assert.strictEqual(P('999'), 30000, 'capped at 30s');
  assert.strictEqual(P('30'), 30000, 'exactly the cap');
  for (const bad of [undefined, null, '', '0', '-5', 'abc', '2abc', 'Wed, 21 Oct 2026 07:28:00 GMT', 'NaN', 'Infinity', '0.0']) {
    assert.strictEqual(P(bad), 2000, 'fallback 2s for ' + JSON.stringify(bad));
  }

  // --- 429 -> 429 -> success: waits honour Retry-After ------------------------
  {
    let calls = 0; const o = mkOpts();
    const r = await NR.fetchWithRetry(() => { calls++; return calls < 3 ? Promise.reject(err(429, '3')) : Promise.resolve({ ok: 1 }); }, o);
    assert.deepStrictEqual(r, { ok: 1 });
    assert.strictEqual(calls, 3);
    assert.deepStrictEqual(o.waits, [3000, 3000]);
    assert.strictEqual(o.busy.length, 2, 'busy callback per wait');
    assert.strictEqual(o.busy[0].attempt, 1); assert.strictEqual(o.busy[1].attempt, 2);
    assert.strictEqual(o.busy[0].max, 5);
    assert.strictEqual(o.busy[0].delayMs, 3000);
  }

  // --- exhaustion: 1 try + 5 retries, then the 429 error is thrown ------------
  {
    let calls = 0; const o = mkOpts();
    let thrown = null;
    try { await NR.fetchWithRetry(() => { calls++; return Promise.reject(err(429)); }, o); } catch (e) { thrown = e; }
    assert.ok(thrown && thrown.status === 429, 'final 429 error propagates');
    assert.strictEqual(calls, 6, '1 initial + 5 retries');
    assert.strictEqual(o.waits.length, 5);
    assert.ok(o.waits.every((w) => w === 2000), 'fallback 2s when no header');
  }

  // --- other statuses are not retried ------------------------------------------
  for (const st of [400, 404, 500, 502, 503, undefined]) {
    let calls = 0; const o = mkOpts(); let thrown = null;
    try { await NR.fetchWithRetry(() => { calls++; return Promise.reject(err(st)); }, o); } catch (e) { thrown = e; }
    assert.ok(thrown, 'rethrown for ' + st);
    assert.strictEqual(calls, 1, 'no retry for ' + st);
    assert.strictEqual(o.waits.length, 0);
  }

  // --- cancellation during the wait --------------------------------------------
  {
    let calls = 0; let cur = true;
    const o = mkOpts({ sleep: () => { cur = false; return Promise.resolve(false); } });
    o.isCurrent = () => cur;
    const r = await NR.fetchWithRetry(() => { calls++; return Promise.reject(err(429, '1')); }, o);
    assert.strictEqual(calls, 1, 'no further request after cancel during wait');
    assert.ok(r && r.cancelled === true, 'resolves {cancelled:true}');
  }
  // sleep elapsed, but the load was superseded meanwhile
  {
    let calls = 0; let cur = true;
    const o = mkOpts({ sleep: () => { cur = false; return Promise.resolve(true); } });
    o.isCurrent = () => cur;
    const r = await NR.fetchWithRetry(() => { calls++; return Promise.reject(err(429)); }, o);
    assert.strictEqual(calls, 1, 'stale after wait: no new request');
    assert.ok(r && r.cancelled);
  }

  // --- cancellation during the request: late success and late error ignored ----
  {
    let cur = true; const o = mkOpts(); o.isCurrent = () => cur;
    const r = await NR.fetchWithRetry(() => { cur = false; return Promise.resolve({ late: 1 }); }, o);
    assert.ok(r && r.cancelled, 'late success ignored');
    assert.strictEqual(o.waits.length, 0);
  }
  {
    let cur = true; const o = mkOpts(); o.isCurrent = () => cur;
    const r = await NR.fetchWithRetry(() => { cur = false; return Promise.reject(err(429)); }, o);
    assert.ok(r && r.cancelled, 'late 429 neither retried nor reported');
    assert.strictEqual(o.waits.length, 0);
    assert.strictEqual(o.busy.length, 0);
  }
  {
    let cur = true; const o = mkOpts(); o.isCurrent = () => cur;
    const r = await NR.fetchWithRetry(() => { cur = false; return Promise.reject(err(500)); }, o);
    assert.ok(r && r.cancelled, 'late non-429 error ignored');
  }

  // --- real sleep: destroy() cancels the pending timer -------------------------
  assert.ok(typeof NR.cancelWait === 'function' && typeof NR.sleep === 'function');
  {
    const p = NR.sleep(60000);
    NR.cancelWait();
    assert.strictEqual(await p, false, 'cancelWait resolves a pending sleep with false');
    const q = NR.sleep(5);
    assert.strictEqual(await q, true, 'uncancelled sleep elapses');
  }

  // --- F2: cancelWait() really calls clearTimeout (no leaked wait timer) ------
  {
    const real = { st: sandbox.setTimeout, ct: sandbox.clearTimeout };
    const set = [], cleared = [];
    sandbox.setTimeout = (fn, ms) => { const id = real.st(fn, ms); set.push(id); return id; };
    sandbox.clearTimeout = (id) => { cleared.push(id); return real.ct(id); };
    const p = NR.sleep(60000);
    assert.strictEqual(set.length, 1, 'sleep() armed one timer');
    NR.cancelWait();
    assert.strictEqual(await p, false);
    assert.deepStrictEqual(cleared, set, 'cancelWait() cleared exactly the armed timer');
    NR.cancelWait(); // idempotent: nothing pending, nothing cleared
    assert.strictEqual(cleared.length, 1, 'no clearTimeout when nothing is pending');
    sandbox.setTimeout = real.st; sandbox.clearTimeout = real.ct;
  }

  // --- a pending wait ends when the page is destroyed or a new load starts ----
  {
    const page = registered['node-reach'];
    assert.ok(page && typeof page.init === 'function' && typeof page.destroy === 'function');
    sandbox.api = () => new Promise(() => {}); // never settles: the load stays in flight
    const container = { innerHTML: '', querySelector: () => null };

    const p1 = NR.sleep(60000);
    page.destroy();
    assert.strictEqual(await p1, false, 'destroy() cancels the pending wait');

    const p2 = NR.sleep(60000);
    page.init(container, 'aabbcc/reach');
    assert.strictEqual(await p2, false, 'starting a load cancels a wait left by an older load');
    page.destroy();
  }

  await new Promise((r) => setImmediate(r));
  assert.strictEqual(unhandled.length, 0, 'zero unhandled rejections: ' + unhandled.map(String));
  console.log('test-node-reach-429-retry.js OK');
})().catch((e) => { console.error(e); process.exit(1); });
