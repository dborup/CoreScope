#!/usr/bin/env node
/* Guard for tests/e2e-touch-swipe.js, the phone suites' swipe helper (#967).
 *
 * Run against a model page: a fake CDP session moves a fake finger, and a
 * fake page scrolls a box the way Chromium does (nothing until the finger
 * has moved TOUCH_SLOP_PX, then with it; a fling when it lifts while it
 * moves), one animation frame at a time. The helper's page functions run
 * against the model's document in a vm context, as Playwright would run them
 * in the page. Checked:
 *   - the finger travels the distance to scroll plus TOUCH_SLOP_PX, up for a
 *     positive distance and down for a negative one, in whole pixels, ending
 *     exactly there (a swipe of the distance alone scrolled 15 px short);
 *   - scrollSettled returns only once the box has stopped (two equal
 *     frames), not while it still moves;
 *   - the containers are waited on before the finger lands and after it
 *     lifts, and a call without them is refused;
 *   - the finger holds still for HOLD_MS between its last move and the lift
 *     (a lift while moving starts a fling of varying length);
 *   - a swipe whose first moves scroll nothing (a loaded host) is made up
 *     inside the same touch, as is one measured while the box still catches
 *     up, without overshooting;
 *   - a swipe still short after the lift is finished by further swipes only
 *     within opts.maxSwipes, which touchSwipe returns the count of; it stops
 *     at the content's end and never swipes back over an overshoot.
 * test-touch-swipe-e2e.js checks the distances in a real browser.
 *
 * Run: node test-touch-swipe-helper.js
 */
'use strict';

const assert = require('assert');
const vm = require('vm');
const { touchSwipe, swipePath, scrollSettled, TOUCH_SLOP_PX, HOLD_MS, MAX_CORRECTIONS } = require('./tests/e2e-touch-swipe');

let passed = 0;
async function test(name, fn) { await fn(); passed++; console.log('  ✓ ' + name); }

/* model is a page with one scroll box '#box' and a fake CDP session.
 *   eat: the first `eat` moves of each touch scroll nothing (a loaded host);
 *   lag: px the box moves per frame towards where the finger has put it
 *        (Infinity: at once);
 *   liftLoss: px each successive touch's scroll loses (> 0) or gains (< 0)
 *        when it lifts;
 *   top, max: the box's scrollTop and its largest. */
function model(o) {
  o = Object.assign({ eat: 0, lag: Infinity, liftLoss: [], top: 1000, max: 5000 }, o);
  const log = [];
  let frame = 0, top = o.top, aim = o.top, touch = null, touches = 0;
  const box = { get scrollTop() { return Math.round(top); }, get scrollLeft() { return 0; } };
  const sandbox = { document: { querySelector: (s) => (s === '#box' ? box : null) }, innerHeight: 560 };
  sandbox.window = sandbox;
  vm.createContext(sandbox);
  const run = (fn, arg) => vm.runInContext('(' + fn.toString() + ')', sandbox)(arg);
  const clamp = (v) => Math.max(0, Math.min(o.max, v));
  function tick() {
    frame++;
    top = aim > top ? Math.min(aim, top + o.lag) : Math.max(aim, top - o.lag);
  }
  function aimAt(y) {
    // The scroll follows the finger past the slop, less what the eaten
    // moves covered.
    const d = touch.y0 - y, past = Math.max(0, Math.abs(d) - TOUCH_SLOP_PX - touch.lost);
    aim = clamp(touch.top0 + Math.sign(d) * past);
  }
  const page = {
    async evaluate(fn, arg) { log.push(['evaluate']); return run(fn, arg); },
    async waitForFunction(fn, arg, opts) {
      log.push(['settled', arg, opts && opts.polling]);
      for (let i = 0; i < 600; i++) { tick(); if (run(fn, arg)) return; }
      throw new Error('waitForFunction timed out');
    },
    async waitForTimeout(ms) { log.push(['wait', ms]); for (let i = 0; i < Math.ceil(ms / 16); i++) tick(); },
  };
  const cdp = {
    async send(method, params) {
      assert.equal(method, 'Input.dispatchTouchEvent');
      log.push([params.type, params.touchPoints]);
      const p = params.touchPoints[0];
      if (params.type === 'touchStart') touch = { y0: p.y, y: p.y, top0: aim, moves: 0, lost: 0, at: frame, n: touches++ };
      else if (params.type === 'touchMove') {
        if (++touch.moves <= o.eat) touch.lost += Math.abs(p.y - touch.y);
        touch.y = p.y; touch.at = frame;
        aimAt(p.y);
      } else if (params.type === 'touchEnd') {
        const dir = Math.sign(aim - touch.top0);
        if (frame - touch.at < 3) aim = clamp(aim + dir * 20); // lifted while moving: a fling
        aim = clamp(aim - dir * (o.liftLoss[touch.n] || 0));
        touch = null;
      }
    },
  };
  return { page, cdp, log, top: () => Math.round(top), swipes: () => log.filter(e => e[0] === 'touchStart').length };
}

(async () => {
  console.log('\n=== #967 touchSwipe helper ===\n');

  await test('TOUCH_SLOP_PX is Chromium\'s 15 px, the hold is at least 100 ms, and a shortfall may be corrected', async () => {
    assert.equal(TOUCH_SLOP_PX, 15);
    assert.ok(HOLD_MS >= 100, 'HOLD_MS ' + HOLD_MS);
    assert.ok(MAX_CORRECTIONS >= 1, 'MAX_CORRECTIONS ' + MAX_CORRECTIONS);
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

  await test('scrollSettled returns only once a moving box has stopped', async () => {
    const m = model({ lag: 2 });
    // Put the box in motion: 40 px to go at 2 px a frame.
    await m.cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [{ x: 10, y: 300 }] });
    await m.cdp.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [{ x: 10, y: 300 - 40 - TOUCH_SLOP_PX }] });
    assert.equal(m.top(), 1000);
    await scrollSettled(m.page, ['#box']);
    assert.equal(m.top(), 1040, 'returned while the box was still moving');
  });

  await test('touchSwipe: settle, land, move, hold, lift, settle; one swipe, exact', async () => {
    const m = model();
    assert.equal(await touchSwipe(m.page, m.cdp, { x: 10, y: 300 }, 65, { scrollers: ['#box'], steps: 4 }), 1);
    assert.equal(m.top(), 1065);
    const kinds = m.log.map(e => e[0]);
    const start = kinds.indexOf('touchStart'), end = kinds.indexOf('touchEnd');
    assert.ok(start > 0 && kinds.slice(0, start).includes('settled'), 'no wait for the containers before the finger lands: ' + kinds.join());
    assert.deepEqual(m.log[start][1], [{ x: 10, y: 300 }]);
    const moves = m.log.filter(e => e[0] === 'touchMove').map(e => e[1][0]);
    assert.deepEqual(moves, swipePath({ x: 10, y: 300 }, 65, 4), 'an exact swipe needs no correction');
    const held = m.log.slice(kinds.lastIndexOf('touchMove') + 1, end);
    assert.ok(held.some(e => e[0] === 'wait' && e[1] >= HOLD_MS), 'no hold before the lift: ' + JSON.stringify(held));
    const after = m.log.slice(end + 1).find(e => e[0] === 'settled');
    assert.ok(after && after[1][0] === '#box' && after[2] === 'raf', 'no wait for the containers after the lift');
  });

  await test('a swipe whose first three moves scroll nothing is made up inside the same touch, both ways', async () => {
    for (const px of [30, 53, -30, -53]) {
      const m = model({ eat: 3 });
      assert.equal(await touchSwipe(m.page, m.cdp, { x: 10, y: 300 }, px, { scrollers: ['#box'], steps: 10 }), 1, 'swipes for ' + px);
      assert.equal(m.swipes(), 1);
      assert.equal(m.top() - 1000, px, 'scrolled for ' + px);
      const moves = m.log.filter(e => e[0] === 'touchMove').length;
      assert.ok(moves > 10, 'the finger did not move on for ' + px);
      const kinds = m.log.map(e => e[0]);
      const held = m.log.slice(kinds.lastIndexOf('touchMove') + 1, kinds.indexOf('touchEnd'));
      assert.ok(held.some(e => e[0] === 'wait' && e[1] >= HOLD_MS), 'no hold before the lift after the correction');
    }
  });

  await test('a box still catching up when the finger holds is not overshot', async () => {
    // 2 px a frame: 65 px take 33 frames, longer than the hold.
    const m = model({ lag: 2 });
    await touchSwipe(m.page, m.cdp, { x: 10, y: 300 }, 65, { scrollers: ['#box'], steps: 8 });
    assert.equal(m.top(), 1065);
  });

  await test('at the content\'s end the correction stops, and so do further swipes', async () => {
    const m = model({ top: 1000, max: 1020 });
    assert.equal(await touchSwipe(m.page, m.cdp, { x: 10, y: 300 }, 50, { scrollers: ['#box'], maxSwipes: 5 }), 2);
    assert.equal(m.top(), 1020);
    assert.ok(m.log.filter(e => e[0] === 'touchMove').length < 8 + 2 * (MAX_CORRECTIONS + 1) * 20, 'the finger kept moving');
  });

  await test('a swipe short after the lift is finished by further swipes, only within maxSwipes', async () => {
    let m = model({ liftLoss: [12] });
    assert.equal(await touchSwipe(m.page, m.cdp, { x: 10, y: 300 }, 50, { scrollers: ['#box'] }), 1, 'swiped again without maxSwipes');
    assert.equal(m.top(), 1038);
    m = model({ liftLoss: [12] });
    assert.equal(await touchSwipe(m.page, m.cdp, { x: 10, y: 300 }, 50, { scrollers: ['#box'], maxSwipes: 3 }), 2);
    assert.equal(m.top(), 1050);
    m = model({ liftLoss: [12, 5, 4, 3] });
    assert.equal(await touchSwipe(m.page, m.cdp, { x: 10, y: 300 }, -50, { scrollers: ['#box'], maxSwipes: 3 }), 3, 'more than maxSwipes');
    assert.equal(m.swipes(), 3);
    assert.equal(m.top(), 1000 - 50 + 4);
  });

  await test('an overshoot is not swiped back', async () => {
    const m = model({ liftLoss: [-3] });
    assert.equal(await touchSwipe(m.page, m.cdp, { x: 10, y: 300 }, 50, { scrollers: ['#box'], maxSwipes: 3 }), 1);
    assert.equal(m.top(), 1053);
  });

  await test('touchSwipe refuses a call without scroll containers', async () => {
    const m = model();
    await assert.rejects(() => touchSwipe(m.page, m.cdp, { x: 0, y: 0 }, 10, {}), /scrollers/);
    assert.equal(m.log.length, 0);
  });

  console.log(`\n${passed} passed\n`);
})().catch((e) => { console.error(e); process.exit(1); });
