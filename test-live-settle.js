#!/usr/bin/env node
/* Guard for tests/e2e-live-settle.js, the Live E2E settle helper (#958,
 * review PR#961 F1 and F5).
 *
 * The in-page predicate is run here, from its own source, in a vm sandbox
 * with a fake document, so what is checked is what the browser runs:
 *   - no Live page, no element or fonts still loading: not settled;
 *   - a page whose height the resize handler has not set yet (an empty
 *     style.height) or set for another viewport: not settled;
 *   - the height subtracts the bottom nav when it shows, else the
 *     --bottom-nav-reserve token, and the stale-source pills;
 *   - settled only once the rect is the same in two reads in a row;
 *   - the predicate answers a boolean, never a Promise (waitForFunction
 *     takes a Promise as truthy at once), and settleLive polls it per frame
 *     from a fresh start.
 *
 * Run: node test-live-settle.js
 */
'use strict';

const assert = require('assert');
const vm = require('vm');
const { settleLive, livePageSettled } = require('./tests/e2e-live-settle');

let passed = 0, failed = 0;
function test(name, fn) {
  return Promise.resolve().then(fn).then(
    () => { passed++; console.log('  ✓ ' + name); },
    (e) => { failed++; console.log('  ✗ ' + name + '\n    ' + (e && e.message)); });
}

// world: a fake Live page. o.rect is the watched element's rect, read live.
function world(o) {
  const rect = () => Object.assign({ left: 560, top: 753, width: 320, height: 40 }, o.rect);
  const page = { style: { height: o.pageHeight == null ? '' : o.pageHeight } };
  const el = { getBoundingClientRect: rect };
  const nav = { getBoundingClientRect: () => ({ height: o.navHeight || 0 }) };
  const body = { appendChild() {} };
  const ctx = {
    innerHeight: o.innerHeight || 900,
    document: {
      fonts: { status: o.fonts || 'loaded' },
      body,
      documentElement: {},
      querySelector: (sel) => (sel === '.live-page' ? (o.noPage ? null : page)
        : sel === '.bottom-nav' ? nav
        : sel === '.live-moments' ? (o.noEl ? null : el) : null),
      createElement: () => ({ style: {}, getBoundingClientRect: () => ({ height: o.reserveToken || 0 }), remove() {} }),
    },
    getComputedStyle: (node) => (node === nav ? { display: o.navShown ? 'flex' : 'none' }
      : { getPropertyValue: (k) => (k === '--warmup-pills-h' && o.pills ? o.pills + 'px' : '') }),
  };
  ctx.window = ctx;
  vm.createContext(ctx);
  const settled = vm.runInContext('(' + livePageSettled.toString() + ')', ctx);
  return { ctx, page, read: () => settled('.live-moments'), set: (r) => { o.rect = r; } };
}

(async () => {
  console.log('\n=== tests/e2e-live-settle.js ===');

  await test('no Live page, no element or fonts still loading: not settled', () => {
    for (const o of [{ noPage: true }, { noEl: true }, { fonts: 'loading' }]) {
      const w = world(Object.assign({ pageHeight: '900px' }, o));
      assert.strictEqual(w.read(), false, JSON.stringify(o));
      assert.strictEqual(w.read(), false, JSON.stringify(o) + ' (second read)');
    }
  });

  await test('before the resize handler ran (empty style.height): not settled, however still', () => {
    const w = world({ pageHeight: '' });
    for (let i = 0; i < 4; i++) assert.strictEqual(w.read(), false);
  });

  await test('a page height for another viewport (848 of 900): not settled', () => {
    const w = world({ pageHeight: '848px' });
    for (let i = 0; i < 3; i++) assert.strictEqual(w.read(), false);
    w.page.style.height = '900px';
    w.read();
    assert.strictEqual(w.read(), true);
  });

  await test('the height leaves out the bottom nav when it shows, else the reserve token, and the pills', () => {
    const shown = world({ innerHeight: 560, navShown: true, navHeight: 56.5, pageHeight: '503.5px' });
    shown.read();
    assert.strictEqual(shown.read(), true, 'nav shown');
    const token = world({ innerHeight: 560, navShown: false, reserveToken: 56, pageHeight: '504px' });
    token.read();
    assert.strictEqual(token.read(), true, 'reserve token');
    const pills = world({ innerHeight: 900, pills: 30, pageHeight: '870px' });
    pills.read();
    assert.strictEqual(pills.read(), true, 'pills');
    const noPills = world({ innerHeight: 900, pills: 30, pageHeight: '900px' });
    noPills.read();
    assert.strictEqual(noPills.read(), false, 'pills not left out');
  });

  await test('settled only when the rect is the same in two reads in a row', () => {
    const w = world({ pageHeight: '900px', rect: { top: 701 } });
    assert.strictEqual(w.read(), false, 'first read');
    w.set({ top: 753 });
    assert.strictEqual(w.read(), false, 'moved since the last read');
    assert.strictEqual(w.read(), true, 'held still');
  });

  await test('the predicate answers a boolean, never a Promise', () => {
    const w = world({ pageHeight: '900px' });
    for (let i = 0; i < 2; i++) assert.strictEqual(typeof w.read(), 'boolean');
  });

  await test('settleLive clears the last rect, then polls the predicate per frame', async () => {
    const calls = [];
    const page = {
      evaluate: async (fn) => { calls.push(['evaluate', String(fn)]); },
      waitForFunction: async (fn, arg, opts) => { calls.push(['wait', fn, arg, opts]); },
    };
    await settleLive(page, '#liveFeed');
    assert.strictEqual(calls.length, 2);
    assert.ok(/__e2eSettleLast\s*=\s*null/.test(calls[0][1]), 'resets the last rect first');
    assert.strictEqual(calls[1][1], livePageSettled);
    assert.strictEqual(calls[1][2], '#liveFeed');
    assert.strictEqual(calls[1][3].polling, 'raf');
    assert.strictEqual(calls[1][3].timeout, 10000);
  });

  console.log('\n' + passed + ' passed, ' + failed + ' failed');
  process.exit(failed ? 1 : 0);
})();
