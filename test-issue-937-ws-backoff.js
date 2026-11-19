/* test-issue-937-ws-backoff.js — shared WebSocket reconnect backoff and
 * listener isolation (#937).
 *
 * Loads the real public/app.js in the shared vm sandbox (tests/ws-sandbox.js:
 * fake clock, fake timers, fake WebSocket, pinned Math.random) and checks:
 *  - the reconnect delay starts at WS_RECONNECT_MS (operator `wsReconnectMs`,
 *    default 3 s), doubles per failed attempt and is capped at 30 s;
 *  - ±20 % jitter, cut (not clamped) at the cap, so capped delays spread
 *    over 24–30 s instead of piling up on 30 s;
 *  - the delay resets to the base once a socket opens;
 *  - the #117 watchdog and the visible/online resume check still reconnect at
 *    once, whatever the backoff state; a resume does not wait out a pending
 *    backoff reconnect, the watchdog and onclose do;
 *  - one throwing onWS listener does not starve the others, and is logged.
 */
'use strict';
const assert = require('assert');

console.log('--- test-issue-937-ws-backoff.js ---');
let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

const { STALE, PACKET, booted } = require('./tests/ws-sandbox');

// The one pending reconnect's delay, measured from now.
function pendingDelay(b) {
  const pending = [];
  for (const t of b.timers.values()) if (t.fn === b.ctx.connectWS) pending.push(t.at - b.clock.mono);
  assert.strictEqual(pending.length, 1, pending.length + ' pending reconnects');
  return pending[0];
}

// Closes the current socket (as a failed attempt when it never opened) and
// returns the reconnect delay it scheduled, then lets the reconnect run.
function closeAndWait(b) {
  const before = b.sockets.length;
  b.current().serverClose();
  const ms = pendingDelay(b);
  b.advance(ms - 1);
  assert.strictEqual(b.sockets.length, before, 'reconnected before the scheduled delay');
  b.advance(1);
  assert.strictEqual(b.sockets.length, before + 1, 'no reconnect after the scheduled delay');
  return ms;
}

function delays(b, n) {
  const out = [];
  for (let i = 0; i < n; i++) out.push(closeAndWait(b));
  return out;
}

console.log('\n=== delay sequence ===');

// Math.random is pinned to 0.5: the middle of the jitter range, which is the
// nominal step below the cap and 27 s (the middle of 24–30 s) at it.
test('default base 3 s doubles per failed attempt and is capped at 30 s', () => {
  const b = booted();
  assert.deepStrictEqual(delays(b, 7), [3000, 6000, 12000, 24000, 27000, 27000, 27000]);
});

test('the first close of an open socket waits the base delay', () => {
  const b = booted();
  b.current().open(); b.current().recv(PACKET);
  assert.strictEqual(closeAndWait(b), 3000);
});

test('many failed attempts stay at the cap', () => {
  const b = booted();
  const d = delays(b, 60);
  assert.strictEqual(d[59], 27000);
  assert(d.every((ms) => ms > 0 && ms <= 30000), 'a delay left (0, 30000]: ' + d.join(','));
});

console.log('\n=== jitter ===');

test('jitter is ±20 %: random 0 gives 0.8×, random 0.5 gives 1×', () => {
  const lo = booted({ random: () => 0 });
  assert.deepStrictEqual(delays(lo, 6), [2400, 4800, 9600, 19200, 24000, 24000]);
  const mid = booted({ random: () => 0.5 });
  assert.deepStrictEqual(delays(mid, 3), [3000, 6000, 12000]);
});

test('random just below 1 gives 1.2× below the cap and never exceeds 30 s', () => {
  const hi = booted({ random: () => 0.999999 });
  const d = delays(hi, 6);
  assert.deepStrictEqual(d.slice(0, 3), [3600, 7200, 14400]);
  assert(d[3] >= 28800 && d[3] <= 30000, 'fourth delay ' + d[3]);
  assert.strictEqual(d[4], 30000);
  assert.strictEqual(d[5], 30000);
});

test('at the cap the jitter stays spread: few delays land on exactly 30 s', () => {
  const b = booted({ random: Math.random });
  let atCap = 0, min = Infinity, max = 0;
  for (let i = 0; i < 10000; i++) {
    const ms = b.ctx.wsReconnectDelay(20);
    if (ms === 30000) atCap++;
    min = Math.min(min, ms); max = Math.max(max, ms);
  }
  assert(atCap < 500, atCap + ' of 10000 capped delays are exactly 30000 ms');
  assert(min >= 24000 && max <= 30000, 'capped delays span ' + min + '–' + max);
  assert(max - min > 5000, 'capped delays span only ' + min + '–' + max);
});

test('random delays stay within ±20 % of the nominal step', () => {
  let x = 12345;
  const lcg = () => { x = (x * 1103515245 + 12345) % 2147483648; return x / 2147483648; };
  const b = booted({ random: lcg });
  const d = delays(b, 40);
  const nominal = (i) => Math.min(3000 * Math.pow(2, i), 30000);
  d.forEach((ms, i) => {
    assert(ms >= nominal(i) * 0.8 && ms <= Math.min(nominal(i) * 1.2, 30000),
      'attempt ' + i + ': ' + ms + ' ms outside ±20 % of ' + nominal(i));
  });
  assert(new Set(d.slice(5)).size > 5, 'capped delays carry no jitter: ' + d.slice(5).join(','));
});

console.log('\n=== reset ===');

test('the delay resets to the base once a socket opens', () => {
  const b = booted();
  assert.deepStrictEqual(delays(b, 3), [3000, 6000, 12000]);
  b.current().open();
  assert.strictEqual(closeAndWait(b), 3000, 'onopen did not reset the backoff');
  assert.strictEqual(closeAndWait(b), 6000);
});

test('creating a socket alone does not reset the backoff', () => {
  const b = booted();
  delays(b, 4);
  assert.strictEqual(closeAndWait(b), 27000);
});

console.log('\n=== wsReconnectMs ===');

test('WS_RECONNECT_MS (operator wsReconnectMs) is the base', () => {
  const b = booted({ reconnectMs: 5000 });
  assert.deepStrictEqual(delays(b, 5), [5000, 10000, 20000, 27000, 27000]);
});

test('a WS_RECONNECT_MS set after boot (config fetch) is used', () => {
  const b = booted();
  b.ctx.window.WS_RECONNECT_MS = 1000;
  assert.deepStrictEqual(delays(b, 7), [1000, 2000, 4000, 8000, 16000, 27000, 27000]);
});

test('a base above the cap is honoured, not lowered', () => {
  const b = booted({ reconnectMs: 45000 });
  assert.deepStrictEqual(delays(b, 3), [40500, 40500, 40500]);
  const hi = booted({ reconnectMs: 45000, random: () => 0.999999 });
  assert.strictEqual(closeAndWait(hi), 45000);
});

console.log('\n=== watchdog and resume are unaffected ===');

test('deep in backoff, a silent socket is still replaced after WS_STALE_MS, at once', () => {
  const b = booted();
  delays(b, 5); // backoff at the cap
  const n = b.sockets.length;
  b.advance(STALE - 1);
  assert.strictEqual(b.sockets.length, n);
  b.advance(2);
  assert.strictEqual(b.sockets.length, n + 1, 'the watchdog waited for the backoff');
  assert.strictEqual(b.live().length, 1);
});

test('deep in backoff, a visible-tab resume after a long silence reconnects at once', () => {
  const b = booted();
  delays(b, 5);
  const n = b.sockets.length;
  b.setHidden(true);
  b.stepWall(10 * 60000);
  b.setHidden(false);
  assert.strictEqual(b.sockets.length, n + 1, 'resume did not reconnect at once');
  assert.strictEqual(b.live().length, 1);
});

test('deep in backoff, online after a long silence reconnects at once', () => {
  const b = booted();
  delays(b, 5);
  const n = b.sockets.length;
  b.stepWall(STALE + 1);
  b.online();
  assert.strictEqual(b.sockets.length, n + 1, 'online did not reconnect at once');
});

test('resume during a pending backoff reconnect reconnects at once, keeping the backoff', () => {
  for (const resume of ['visible', 'online']) {
    const b = booted();
    delays(b, 4);
    b.current().serverClose(); // the cap is pending
    const n = b.sockets.length;
    if (resume === 'visible') { b.setHidden(true); b.setHidden(false); } else b.online();
    assert.strictEqual(b.sockets.length, n + 1, resume + ': waited out the pending backoff');
    assert.strictEqual(b.live().length, 1, resume + ': ' + b.live().length + ' live sockets');
    assert.strictEqual(b.pendingReconnects(), 0, resume + ': the old reconnect is still pending');
    // The server is still down: the next close continues the backoff.
    assert.strictEqual(closeAndWait(b), 27000, resume + ': the resume reset the backoff');
  }
});

test('only resume skips the pending reconnect, not the watchdog or another close', () => {
  const b = booted();
  delays(b, 4);
  b.current().serverClose();
  const n = b.sockets.length;
  const ms = pendingDelay(b);
  b.ctx.checkWSLiveness(); // a late watchdog run
  b.advance(ms - 1);
  assert.strictEqual(b.sockets.length, n, 'the watchdog skipped the pending backoff');
  assert.strictEqual(pendingDelay(b), 1);
  b.advance(1);
  assert.strictEqual(b.sockets.length, n + 1);
  assert.strictEqual(b.live().length, 1);
});

console.log('\n=== listener fan-out ===');

test('a throwing listener does not starve the listeners after it, and is logged', () => {
  const b = booted();
  const s = b.current(); s.open();
  const got = [];
  b.ctx.window.onWS(() => { throw new Error('boom'); });
  b.ctx.window.onWS((m) => got.push(m.type));
  s.recv(PACKET);
  s.recv(PACKET);
  assert.deepStrictEqual(got, ['packet', 'packet'], 'the listener after a throwing one missed messages');
  assert.strictEqual(b.errors.length, 2, b.errors.length + ' errors logged');
  assert(b.errors[0].some((a) => a && a.message === 'boom'), 'the listener error was not logged');
});

test('every listener runs once per message, in order, around several throwers', () => {
  const b = booted();
  const s = b.current(); s.open();
  const order = [];
  b.ctx.window.onWS(() => { order.push(1); throw new Error('a'); });
  b.ctx.window.onWS(() => order.push(2));
  b.ctx.window.onWS(() => { order.push(3); throw new Error('b'); });
  b.ctx.window.onWS(() => order.push(4));
  s.recv(PACKET);
  assert.deepStrictEqual(order, [1, 2, 3, 4]);
});

test('a malformed frame still reaches no listener', () => {
  const b = booted();
  const s = b.current(); s.open();
  let calls = 0;
  b.ctx.window.onWS(() => calls++);
  s.recv('{not json');
  assert.strictEqual(calls, 0);
});

console.log(`\n${passed} passed, ${failed} failed`);
if (failed > 0) process.exit(1);
