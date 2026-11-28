/* test-issue-117-ws-watchdog.js — shared WebSocket liveness (#117).
 *
 * Loads the real public/app.js in the shared vm sandbox (tests/ws-sandbox.js:
 * fake clock, fake timers, fake WebSocket, jitter pinned off) and checks:
 *  - a silent OPEN socket and a handshake that never opens are replaced
 *    once after WS_STALE_MS, measured from socket creation;
 *  - any frame (heartbeat or traffic) keeps the socket;
 *  - heartbeats are consumed before the logo pulse, cache invalidation and
 *    onWS listeners (so before every pause buffer); packets are unchanged;
 *  - wall-clock steps, online and visible-tab resume;
 *  - the first reconnect after an ordinary close waits the configured delay
 *    (the backoff itself is covered by test-issue-937-ws-backoff.js);
 *  - onclose, watchdog, resume and pull-to-reconnect never leave more than
 *    one live socket or more than one pending reconnect, and a replaced
 *    socket's late events are detached.
 */
'use strict';
const assert = require('assert');

console.log('--- test-issue-117-ws-watchdog.js ---');
let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

const { STALE, HEARTBEAT, PACKET, booted } = require('./tests/ws-sandbox');

console.log('\n=== silent sockets are replaced once ===');

test('a silent OPEN socket is replaced exactly once after WS_STALE_MS', () => {
  const b = booted();
  const s = b.current(); s.open();
  b.advance(10000); s.recv(PACKET);
  b.advance(STALE - 1);
  assert.strictEqual(b.sockets.length, 1, 'replaced before the threshold');
  b.advance(2);
  assert.strictEqual(b.sockets.length, 2, 'a socket silent for WS_STALE_MS was not replaced');
  assert(s.closeCalled, 'the stale socket was not closed');
  assert.strictEqual(s.onmessage, null, 'the stale socket still has its handlers');
  assert.strictEqual(b.live().length, 1);
  b.advance(1000); // the old socket's close event arrives: nothing more
  assert.strictEqual(b.sockets.length, 2, 'the replaced socket\'s close event scheduled another connection');
  assert.strictEqual(b.pendingReconnects(), 0);
});

test('a handshake that never opens is replaced after WS_STALE_MS from creation', () => {
  const b = booted();
  b.advance(STALE - 1);
  assert.strictEqual(b.sockets.length, 1);
  b.advance(2);
  assert.strictEqual(b.sockets.length, 2, 'a stuck handshake was never replaced');
  assert.strictEqual(b.live().length, 1);
});

test('heartbeats keep a quiet socket for 10 minutes', () => {
  const b = booted();
  const s = b.current(); s.open();
  for (let t = 0; t < 600000; t += 30000) { b.advance(30000); s.recv(HEARTBEAT); }
  assert.strictEqual(b.sockets.length, 1, 'a socket with regular heartbeats was replaced');
});

test('packet traffic alone keeps the socket', () => {
  const b = booted();
  const s = b.current(); s.open();
  for (let t = 0; t < 600000; t += STALE / 2) { b.advance(STALE / 2); s.recv(PACKET); }
  assert.strictEqual(b.sockets.length, 1);
});

console.log('\n=== heartbeats are consumed first ===');

test('a heartbeat reaches no onWS listener, no logo pulse and no cache invalidation', () => {
  const b = booted();
  const s = b.current(); s.open();
  const got = [];
  b.ctx.window.onWS((m) => got.push(m));
  const logo = b.ctx.window.__corescopeLogo;
  const before = logo.stats.triggered + logo.stats.dropped;
  s.recv(HEARTBEAT);
  assert.strictEqual(got.length, 0, 'the heartbeat was dispatched to a listener (and so to pause buffers)');
  assert.strictEqual(logo.stats.triggered + logo.stats.dropped, before, 'the heartbeat pulsed the logo');
  assert(!b.ctx.window.__api._invalidateTimer, 'the heartbeat scheduled cache invalidation');
});

test('packet messages are dispatched unchanged', () => {
  const b = booted();
  const s = b.current(); s.open();
  const got = [];
  b.ctx.window.onWS((m) => got.push(m));
  s.recv(PACKET);
  assert.strictEqual(got.length, 1);
  assert.deepStrictEqual(JSON.parse(JSON.stringify(got[0])), JSON.parse(PACKET));
});

console.log('\n=== clock steps and resume ===');

test('the wall clock stepping back does not postpone detection by the size of the step', () => {
  const b = booted();
  const s = b.current(); s.open();
  b.advance(1000); s.recv(PACKET);
  b.stepWall(-3600000); // one hour back
  b.advance(STALE + 1);
  assert.strictEqual(b.sockets.length, 2, 'after a backward clock step the silent socket was not replaced within WS_STALE_MS');
});

test('a forward step (sleep) is caught on visible-tab resume at once', () => {
  const b = booted();
  const s = b.current(); s.open(); s.recv(PACKET);
  b.setHidden(true);
  b.stepWall(10 * 60000); // asleep: timers did not run
  assert.strictEqual(b.sockets.length, 1, 'hiding the tab must not trigger a check');
  b.setHidden(false);
  assert.strictEqual(b.sockets.length, 2, 'resume after a long silence did not replace the socket');
  assert.strictEqual(b.live().length, 1);
});

test('online after a long silence reconnects at once; with recent traffic it does not', () => {
  const b = booted();
  const s = b.current(); s.open(); s.recv(PACKET);
  b.advance(5000);
  b.online();
  assert.strictEqual(b.sockets.length, 1, 'online with recent traffic replaced a healthy socket');
  b.stepWall(STALE);
  b.online();
  assert.strictEqual(b.sockets.length, 2, 'online after silence did not reconnect');
});

test('repeated resume events open one socket', () => {
  const b = booted();
  const s = b.current(); s.open();
  b.stepWall(STALE * 3);
  b.setHidden(false); b.online(); b.setHidden(false); b.online();
  assert.strictEqual(b.sockets.length, 2, b.sockets.length + ' sockets after four resume events');
  b.advance(STALE - 1);
  assert.strictEqual(b.sockets.length, 2);
  assert.strictEqual(b.live().length, 1);
});

console.log('\n=== close, pull and races ===');

test('an ordinary close keeps the configured reconnect delay, and schedules one reconnect', () => {
  const b = booted({ reconnectMs: 5000 });
  const s = b.current(); s.open(); s.recv(PACKET);
  s.serverClose();
  assert.strictEqual(b.pendingReconnects(), 1, 'onclose must leave exactly one pending reconnect');
  b.advance(4999);
  assert.strictEqual(b.sockets.length, 1, 'reconnected before the configured delay');
  b.advance(1);
  assert.strictEqual(b.sockets.length, 2);
  b.advance(STALE * 2); // no watchdog on the closed socket
  assert.strictEqual(b.live().length, 1);
});

test('resume during a pending reconnect replaces it with one socket; the watchdog does not', () => {
  const b = booted({ reconnectMs: 5000 });
  const s = b.current(); s.open();
  b.advance(STALE - 1000);
  s.serverClose();
  b.ctx.checkWSLiveness(); // a late watchdog run waits for the pending reconnect
  b.advance(1000);
  assert.strictEqual(b.sockets.length, 1, 'the watchdog bypassed the pending reconnect');
  b.stepWall(STALE);
  b.setHidden(false); b.online();
  assert.strictEqual(b.sockets.length, 2, 'a resume waited out the pending reconnect');
  assert.strictEqual(b.live().length, 1);
  assert.strictEqual(b.pendingReconnects(), 0);
  b.advance(10000); // past the 5 s the cancelled reconnect was due at
  assert.strictEqual(b.sockets.length, 2, 'the cancelled reconnect still ran');
  assert.strictEqual(b.live().length, 1);
});

test('a pull during the reconnect delay cancels the pending reconnect', () => {
  const b = booted({ reconnectMs: 5000 });
  const s = b.current(); s.open();
  s.serverClose();
  b.advance(1000);
  b.ctx.window.pullReconnect();
  assert.strictEqual(b.pendingReconnects(), 0, 'the reconnect scheduled by onclose is still pending');
  b.current().open();
  b.advance(10000);
  assert.strictEqual(b.sockets.length, 2, b.sockets.length + ' sockets: the old reconnect replaced the pulled socket');
  assert.strictEqual(b.live().length, 1);
});

test('pull-to-reconnect on a socket that is not open leaves one socket', () => {
  const b = booted();
  const s = b.current(); // still CONNECTING
  b.ctx.window.pullReconnect();
  b.advance(10000);
  assert.strictEqual(b.live().length, 1, b.live().length + ' live sockets after pull-to-reconnect');
  assert(s.closeCalled);
  assert.strictEqual(b.sockets.length, 2, b.sockets.length + ' sockets constructed');
});

test('pull-to-reconnect on an OPEN (possibly half-open) socket replaces it at once', () => {
  const b = booted({ closeEventMs: 60000 });
  const s = b.current(); s.open();
  b.ctx.window.pullReconnect();
  assert.strictEqual(b.sockets.length, 2, 'pull waited for the old socket\'s close event');
  b.advance(120000);
  assert.strictEqual(b.live().length, 1);
});

test('pull, watchdog and close racing each other end with one socket and no pending reconnect', () => {
  const b = booted({ reconnectMs: 3000 });
  let s = b.current(); s.open();
  b.ctx.window.pullReconnect();
  b.ctx.window.pullReconnect();
  s = b.current(); s.serverClose();
  b.ctx.window.pullReconnect();
  b.advance(STALE + 10); // watchdog of a never-opened socket
  b.setHidden(false); b.online();
  b.advance(20000);
  assert.strictEqual(b.live().length, 1, b.live().length + ' live sockets');
  assert(b.pendingReconnects() <= 1);
});

console.log(`\n${passed} passed, ${failed} failed`);
if (failed > 0) process.exit(1);
