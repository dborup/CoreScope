#!/usr/bin/env node
/* Guard for tests/e2e-touch-swipe.js, the phone suites' swipe helper (#967).
 *
 * Run against a fake page and CDP session, so what is checked is the order
 * and the shape of what the helper sends:
 *   - the finger travels the distance to scroll plus TOUCH_SLOP_PX, up for a
 *     positive distance and down for a negative one, in whole pixels, ending
 *     exactly there (a swipe of the distance alone scrolled 15 px short);
 *   - the scroll containers are waited on before the finger lands and after
 *     it lifts, and a call without them is refused;
 *   - the finger holds still for HOLD_MS between its last move and the lift
 *     (a lift while moving starts a fling of varying length).
 * test-touch-swipe-e2e.js checks the distances in a real browser.
 *
 * Run: node test-touch-swipe-helper.js
 */
'use strict';

const assert = require('assert');
const { touchSwipe, swipePath, TOUCH_SLOP_PX, HOLD_MS } = require('./tests/e2e-touch-swipe');

let passed = 0;
async function test(name, fn) { await fn(); passed++; console.log('  ✓ ' + name); }

function fakes() {
  const log = [];
  const page = {
    async evaluate() { log.push(['evaluate']); },
    async waitForFunction(fn, arg, opts) { log.push(['settled', arg, opts && opts.polling]); },
    async waitForTimeout(ms) { log.push(['wait', ms]); },
  };
  const cdp = { async send(method, params) { assert.equal(method, 'Input.dispatchTouchEvent'); log.push([params.type, params.touchPoints]); } };
  return { page, cdp, log };
}

(async () => {
  console.log('\n=== #967 touchSwipe helper ===\n');

  await test('TOUCH_SLOP_PX is Chromium\'s 15 px and the hold is at least 100 ms', async () => {
    assert.equal(TOUCH_SLOP_PX, 15);
    assert.ok(HOLD_MS >= 100, 'HOLD_MS ' + HOLD_MS);
  });

  await test('the path travels the distance plus the slop, up for a positive distance', async () => {
    const p = swipePath({ x: 180, y: 500 }, 36, 8);
    assert.equal(p.length, 8);
    assert.deepEqual(p[p.length - 1], { x: 180, y: 500 - 36 - TOUCH_SLOP_PX });
    for (let i = 1; i < p.length; i++) assert.ok(p[i].y < p[i - 1].y, 'not moving up at ' + i);
    assert.ok(p.every(q => q.x === 180 && Number.isInteger(q.y)), JSON.stringify(p));
  });

  await test('down for a negative distance, and 1 px still clears the slop', async () => {
    const p = swipePath({ x: 50, y: 200 }, -21, 10);
    assert.equal(p.length, 10);
    assert.deepEqual(p[p.length - 1], { x: 50, y: 200 + 21 + TOUCH_SLOP_PX });
    assert.equal(swipePath({ x: 0, y: 100 }, 1, 4).pop().y, 100 - 1 - TOUCH_SLOP_PX);
  });

  await test('touchSwipe: settle, land, move, hold, lift, settle', async () => {
    const { page, cdp, log } = fakes();
    await touchSwipe(page, cdp, { x: 10, y: 300 }, 65, { scrollers: ['#box'], steps: 4 });
    const kinds = log.map(e => e[0]);
    const start = kinds.indexOf('touchStart'), end = kinds.indexOf('touchEnd');
    assert.ok(start > 0 && kinds.slice(0, start).includes('settled'), 'no wait for the containers before the finger lands: ' + kinds.join());
    assert.deepEqual(log[start][1], [{ x: 10, y: 300 }]);
    const moves = log.filter(e => e[0] === 'touchMove').map(e => e[1][0]);
    assert.deepEqual(moves, swipePath({ x: 10, y: 300 }, 65, 4));
    assert.deepEqual(log[end - 1], ['wait', HOLD_MS], 'no hold before the lift: ' + JSON.stringify(log[end - 1]));
    const after = log.slice(end + 1).find(e => e[0] === 'settled');
    assert.ok(after && after[1][0] === '#box' && after[2] === 'raf', 'no wait for the containers after the lift');
  });

  await test('touchSwipe refuses a call without scroll containers', async () => {
    const { page, cdp, log } = fakes();
    await assert.rejects(() => touchSwipe(page, cdp, { x: 0, y: 0 }, 10, {}), /scrollers/);
    assert.equal(log.length, 0);
  });

  console.log(`\n${passed} passed\n`);
})().catch((e) => { console.error(e); process.exit(1); });
