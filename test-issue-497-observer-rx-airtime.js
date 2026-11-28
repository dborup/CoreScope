/**
 * #497 — RX 1h (channel load) next to TX 1h: shared helper additions in
 * public/observer-airtime.js. Loads the real file in a vm sandbox.
 */
'use strict';
const fs = require('fs');
const path = require('path');
const vm = require('vm');
const assert = require('assert');

let passed = 0, failed = 0;
function t(name, fn) {
  try { fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}

const ctx = { window: {}, Number, Math, String, Array };
vm.createContext(ctx);
vm.runInContext(fs.readFileSync(path.join(__dirname, 'public', 'observer-airtime.js'), 'utf8'), ctx);
const A = ctx.window.ObserverAirtime;

const TX_TITLE = 'Share of the last hour this observer transmitted; flagged at or above the duty-cycle limit.';
const RX_TITLE = "Share of the last hour this observer's radio was receiving packets — a measure of channel load at this location, not a limit.";

console.log('observer-airtime.js (#497)');

t('rxBadge: formats one decimal with the channel-load tooltip', () => {
  const html = A.rxBadge(3.7);
  assert.ok(html.includes('3.7%'), html);
  assert.ok(html.includes(RX_TITLE.replace(/'/g, "'")) || html.includes('channel load at this location, not a limit'), html);
  assert.ok(html.includes('class="airtime-pct"'), html);
});

t('rxBadge: never carries the warning class, even far above the TX limit', () => {
  for (const v of [10, 17.5, 99.9, 100]) {
    const html = A.rxBadge(v, 10);
    assert.ok(!html.includes('airtime-over'), v + ': ' + html);
  }
});

t('rxBadge: null and undefined render a muted dash', () => {
  for (const v of [null, undefined]) {
    const html = A.rxBadge(v);
    assert.ok(html.includes('text-muted') && html.includes('—'), html);
    assert.ok(!html.includes('airtime-over'), html);
  }
});

t('rxBadge: tiny non-zero shares read <0.1% and are escaped', () => {
  const html = A.rxBadge(0.03);
  assert.ok(html.includes('&lt;0.1%') && !html.includes('><0.1%'), html);
  assert.ok(A.rxBadge(0).includes('0.0%'));
});

t('badge: TX tooltip is the #497 wording and still names the limit', () => {
  const html = A.badge(12.5, 10);
  assert.ok(html.includes(TX_TITLE), html);
  assert.ok(html.includes('10%'), html);
});

t('titles are exposed for the list header and tiles', () => {
  assert.strictEqual(A.TX_TITLE, TX_TITLE);
  assert.strictEqual(A.RX_TITLE, RX_TITLE);
});

t('lookup: finds one observer in a summary, with the threshold', () => {
  const s = { duty_cycle_warn_pct: 7, observers: [
    { observer_id: 'a', tx_airtime_pct_1h: 1, rx_airtime_pct_1h: 2 },
    { observer_id: 'b', tx_airtime_pct_1h: 12.5, rx_airtime_pct_1h: null },
  ] };
  const b = A.lookup(s, 'b');
  assert.strictEqual(b.tx, 12.5);
  assert.strictEqual(b.rx, null);
  assert.strictEqual(b.warnPct, 7);
});

t('lookup: missing observer, empty or absent summary read as unknown', () => {
  for (const s of [undefined, null, {}, { observers: [] }, { observers: [{ observer_id: 'x' }] }]) {
    const r = A.lookup(s, 'b');
    assert.strictEqual(r.tx, null);
    assert.strictEqual(r.rx, null);
  }
});

t('lookup: undefined values (field absent) become null, not undefined', () => {
  const r = A.lookup({ observers: [{ observer_id: 'b' }] }, 'b');
  assert.strictEqual(r.tx, null);
  assert.strictEqual(r.rx, null);
});

t('statCards: TX warns at the threshold, RX stays neutral at the same value', () => {
  const html = A.statCards({ tx: 10, rx: 10, warnPct: 10 });
  const tx = html.split('data-testid="obs-detail-rx-airtime"')[0];
  const rx = html.split('data-testid="obs-detail-rx-airtime"')[1];
  assert.ok(tx.includes('data-testid="obs-detail-tx-airtime"') && tx.includes('airtime-over'), html);
  assert.ok(!rx.includes('airtime-over'), html);
});

t('statCards: below the threshold TX is not flagged; unknown shows dashes', () => {
  assert.ok(!A.statCards({ tx: 9.9, rx: 3, warnPct: 10 }).includes('airtime-over'));
  const none = A.statCards({ tx: null, rx: null });
  assert.ok(!none.includes('airtime-over'));
  assert.strictEqual((none.match(/—/g) || []).length >= 2, true, none);
});

console.log(`\n${passed} passed, ${failed} failed`);
process.exit(failed ? 1 : 0);
