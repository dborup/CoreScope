#!/usr/bin/env node
/* Guard for tests/e2e-tap.js, the phone suites' real-tap helper (#789 M2b,
 * M3; review PR#893 N4 and the CI failure on it).
 *
 * The helper is run here against a fake page and locator. Its in-page
 * functions (the centring, the hit test at the control's middle) run
 * against a fake element and document, so what is checked is what the
 * browser runs:
 *   - a null box, a box that cannot be read and a detached control are
 *     "not settled yet", not a failure (N4: `box null` failed at once);
 *   - the control is centred, so one that sits in the viewport under the
 *     bottom nav is tapped once centred (CI: `.ac-copy never settled under
 *     a tap`, when a top banner pushed the claim card down and
 *     scrollIntoViewIfNeeded left the button under the nav);
 *   - the tap waits until the box holds still and is the hit element;
 *   - a control that settles too short or wider than the viewport still
 *     fails with its box, and a covered one fails at the bound, naming what
 *     covers it. Nothing is tapped in those cases;
 *   - a tap at (nearly) the point of the page's previous tap waits out the
 *     double-tap window, so Chromium does not read the pair as a double tap
 *     and drop the text typed next (review PR#908 F1); a tap elsewhere, or
 *     on another page, does not wait.
 *
 * Run: node test-tap-when-settled.js
 */
'use strict';

const assert = require('assert');
const { tapWhenSettled, doubleTapWait, DOUBLE_TAP_MS, DOUBLE_TAP_SLOP_PX } = require('./tests/e2e-tap');

const NAV = { tagName: 'BUTTON', id: '', className: 'bottom-nav-tab active' };

// fakePage scripts the control: reads[i] is what the i-th read sees, the
// last one repeating. A read is { box, covered } or { throws: true }; a
// covered control has NAV at its middle. centredAfter (optional) is the
// read from which on the control is only uncovered once it was centred.
function fakePage(reads, opts) {
  opts = opts || {};
  const rec = { taps: [], at: [], centred: [], reads: 0 };
  const el = {
    tagName: 'BUTTON', id: '', className: 'btn ac-copy',
    scrollIntoView(o) { rec.centred.push(o); },
    contains(t) { return t === el; },
  };
  const cur = () => reads[Math.min(rec.reads, reads.length - 1)];
  global.document = {
    elementFromPoint() {
      const r = cur();
      const centred = rec.centred.some(o => o && o.block === 'center');
      if (r.covered && !(opts.centring && centred)) return NAV;
      return el;
    },
  };
  const loc = {
    first() { return loc; },
    async evaluate(fn, arg) {
      if (cur().throws) { rec.reads++; throw new Error('element is not attached to the DOM'); }
      return fn(el, arg);
    },
    async boundingBox() {
      const r = cur();
      rec.reads++;
      if (r.throws) throw new Error('element is not attached to the DOM');
      return r.box;
    },
  };
  const page = {
    viewportSize() { return { width: 375, height: 560 }; },
    locator() { return loc; },
    touchscreen: { async tap(x, y) { rec.taps.push([x, y]); rec.at.push(Date.now()); } },
  };
  return { page, rec };
}

const BOX = { x: 282, y: 448, width: 60, height: 48 };
const FAST = { intervalMs: 1, timeoutMs: 300 };
let passed = 0;
async function test(name, fn) {
  await fn();
  passed++;
  console.log('  ✓ ' + name);
}

(async () => {
  console.log('\n=== tests/e2e-tap.js ===\n');

  await test('a null box and a detached control are retried, then tapped in the middle', async () => {
    const { page, rec } = fakePage([{ box: null }, { throws: true }, { box: null }, { box: BOX }]);
    const b = await tapWhenSettled(page, '.ac-copy', FAST);
    assert.deepStrictEqual(b, BOX);
    assert.deepStrictEqual(rec.taps, [[312, 472]]);
  });

  await test('the control is centred, not scrolled only "if needed"', async () => {
    const { page, rec } = fakePage([{ box: BOX }]);
    await tapWhenSettled(page, '.ac-copy', FAST);
    assert.ok(rec.centred.length > 0 && rec.centred.every(o => o.block === 'center'), JSON.stringify(rec.centred));
  });

  await test('a control in the viewport but under the bottom nav is tapped once centred (CI .ac-copy)', async () => {
    const under = { x: 282, y: 482, width: 60, height: 48 };
    const { page, rec } = fakePage([{ box: under, covered: true }], { centring: true });
    await tapWhenSettled(page, '.ac-copy', FAST);
    assert.strictEqual(rec.taps.length, 1);
  });

  await test('no tap while the box still moves', async () => {
    const moving = [0, 1, 2, 3].map(i => ({ box: { x: 282, y: 448 + 10 * i, width: 60, height: 48 } }));
    const { page, rec } = fakePage(moving.concat([{ box: BOX }]));
    await tapWhenSettled(page, '.ac-copy', FAST);
    assert.ok(rec.reads >= 6, 'tapped after ' + rec.reads + ' reads');
    assert.deepStrictEqual(rec.taps, [[312, 472]]);
  });

  await test('a covered control fails at the bound, naming what covers it, with no tap', async () => {
    const { page, rec } = fakePage([{ box: BOX, covered: true }]);
    await assert.rejects(tapWhenSettled(page, '.ac-copy', FAST), /\.ac-copy never settled under a tap within 300 ms; last read: .*button\.bottom-nav-tab\.active/);
    assert.strictEqual(rec.taps.length, 0);
  });

  await test('a control that never gets a box fails at the bound', async () => {
    const { page, rec } = fakePage([{ box: null }]);
    await assert.rejects(tapWhenSettled(page, '.ac-copy', FAST), /never settled under a tap within 300 ms/);
    assert.strictEqual(rec.taps.length, 0);
  });

  await test('a settled control under 44 px (or the given height) fails with its box', async () => {
    let f = fakePage([{ box: { x: 10, y: 100, width: 60, height: 40 } }]);
    await assert.rejects(tapWhenSettled(f.page, '.ac-copy', FAST), /\.ac-copy box \{"x":10,"y":100,"width":60,"height":40\}/);
    assert.strictEqual(f.rec.taps.length, 0);
    f = fakePage([{ box: { x: 10, y: 100, width: 60, height: 44 } }]);
    await assert.rejects(tapWhenSettled(f.page, 'label', Object.assign({ minHeight: 48 }, FAST)), /at least 48 px/);
    f = fakePage([{ box: { x: 10, y: 100, width: 60, height: 30 } }]);
    await tapWhenSettled(f.page, 'tr', Object.assign({ minHeight: 30 }, FAST));
    assert.strictEqual(f.rec.taps.length, 1);
  });

  await test('a settled control wider than the viewport, or off its left edge, fails', async () => {
    let f = fakePage([{ box: { x: 0, y: 100, width: 376, height: 48 } }]);
    await assert.rejects(tapWhenSettled(f.page, '.ac-copy', FAST), /375 px viewport/);
    f = fakePage([{ box: { x: -1, y: 100, width: 60, height: 48 } }]);
    await assert.rejects(tapWhenSettled(f.page, '.ac-copy', FAST), /375 px viewport/);
    assert.strictEqual(f.rec.taps.length, 0);
  });

  await test('doubleTapWait: only a nearby tap inside the window waits, for the rest of it', async () => {
    const prev = { x: 100, y: 200, t: 1000 };
    assert.strictEqual(doubleTapWait(null, 100, 200, 1000), 0);
    assert.strictEqual(doubleTapWait(prev, 100, 200, 1000), DOUBLE_TAP_MS);
    assert.strictEqual(doubleTapWait(prev, 100 + DOUBLE_TAP_SLOP_PX, 200, 1100), DOUBLE_TAP_MS - 100);
    assert.strictEqual(doubleTapWait(prev, 100 + DOUBLE_TAP_SLOP_PX + 1, 200, 1000), 0);
    assert.strictEqual(doubleTapWait(prev, 100, 200, 1000 + DOUBLE_TAP_MS), 0);
    // The measured window is ~30 px and ~300 ms: the bounds keep a margin.
    assert.ok(DOUBLE_TAP_SLOP_PX >= 40 && DOUBLE_TAP_MS >= 400);
  });

  await test('two taps at the same point are spaced past the double-tap window (F1)', async () => {
    const { page, rec } = fakePage([{ box: BOX }]);
    await tapWhenSettled(page, '#acHeight', FAST);
    await tapWhenSettled(page, '#acGain', Object.assign({}, FAST, { timeoutMs: 2000 }));
    assert.strictEqual(rec.taps.length, 2);
    assert.ok(rec.at[1] - rec.at[0] >= DOUBLE_TAP_MS, 'second tap after ' + (rec.at[1] - rec.at[0]) + ' ms');
  });

  await test('a tap far from the previous one, or on another page, does not wait', async () => {
    const FAR = { x: 10, y: 100, width: 60, height: 48 };
    // Two reads settle the first tap on BOX; every later read is FAR.
    const f = fakePage([{ box: BOX }, { box: BOX }, { box: FAR }]);
    await tapWhenSettled(f.page, '.a', FAST);
    await tapWhenSettled(f.page, '.b', FAST);
    assert.deepStrictEqual(f.rec.taps, [[312, 472], [40, 124]]);
    assert.ok(f.rec.at[1] - f.rec.at[0] < DOUBLE_TAP_MS, 'a far tap waited ' + (f.rec.at[1] - f.rec.at[0]) + ' ms');
    const other = fakePage([{ box: BOX }]);
    const t0 = Date.now();
    await tapWhenSettled(other.page, '.c', FAST);
    assert.ok(Date.now() - t0 < DOUBLE_TAP_MS, 'a tap on another page waited');
  });

  console.log(`\ntests/e2e-tap.js: ${passed} passed, 0 failed`);
})().catch((e) => { console.error('  ✗ ' + (e && e.stack || e)); process.exit(1); });
