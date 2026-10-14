/* Unit tests for public/heard-by.js — the Heard By split (#76) shared by the
 * node detail page, side panel, Live panel, home and analytics. Loads the
 * real file in a vm context.
 * Run: node test-heard-by.js
 */
'use strict';
const assert = require('assert');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const src = fs.readFileSync(path.join(__dirname, 'public', 'heard-by.js'), 'utf8');
const ctx = { window: {}, console };
vm.createContext(ctx);
vm.runInContext(src, ctx, { filename: 'heard-by.js' });
const HB = ctx.window.HeardBy;

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

const direct = (id, n, snr, rssi, extra) => Object.assign({ observer_id: id, observer_name: 'Name ' + id, packetCount: n, avgSnr: snr, avgRssi: rssi }, extra || {});
const relay = (id, n, extra) => Object.assign({ observer_id: id, observer_name: 'Name ' + id, packetCount: n }, extra || {});
const health = {
  directObservers: [direct('d1', 4, 7.25, -101.4, { can_relay: true }), direct('d2', 1, null, -95)],
  relayObservers: [relay('r1', 12, { can_relay: false }), relay('r2', 3), relay('r3', 1)],
};
const text = html => html.replace(/<[^>]*>/g, ' ').replace(/&amp;/g, '&').replace(/\s+/g, ' ');

test('lists/counts/summary read both lists and tolerate missing fields', () => {
  assert.deepStrictEqual(JSON.parse(JSON.stringify(HB.counts(health))), { direct: 2, relay: 3, total: 5 });
  assert.strictEqual(HB.summary(health), '2 direct · 3 via relay');
  for (const h of [null, undefined, {}, { observers: [direct('old', 1, 1, 1)] }, { directObservers: 'x' }]) {
    assert.deepStrictEqual(JSON.parse(JSON.stringify(HB.counts(h))), { direct: 0, relay: 0, total: 0 });
  }
});

test('full page tables: direct rows carry signal, relay rows never do', () => {
  const html = HB.renderTables(health);
  const cut = html.indexOf('data-heard="relay"');
  const directPart = html.slice(0, cut), relayPart = html.slice(cut);
  assert.match(text(html), /Heard By \(5 observers\)/);
  assert.match(text(directPart), /Heard directly ⓘ .*\(2\)/);
  assert.match(text(directPart), /Name d1 repeater 4 7\.3 dB -101 dBm/);
  assert.match(text(directPart), /Name d2 1 — -95 dBm/);
  assert.match(text(relayPart), /Name r1 listener 12/);
  assert.ok(!/dB/.test(text(relayPart)), 'relay table must not show SNR/RSSI: ' + text(relayPart));
  assert.strictEqual((relayPart.match(/<th /g) || []).length, 2, 'relay table has Observer and Packets only');
  assert.strictEqual((html.match(/observer-sort-table/g) || []).length, 2);
});

test('relay rows ignore signal fields even if a server sent them', () => {
  const h = { directObservers: [], relayObservers: [relay('r', 2, { avgSnr: 9.5, avgRssi: -40 })] };
  for (const html of [HB.renderTables(h), HB.renderList(h)]) {
    assert.ok(!/9\.5|-40/.test(text(html)), 'relay row leaked another node\'s RF: ' + text(html));
  }
});

test('empty direct list says missing evidence, not out of range', () => {
  const h = { directObservers: [], relayObservers: [relay('r', 2)] };
  for (const html of [HB.renderTables(h), HB.renderList(h)]) {
    assert.ok(text(html).includes(HB.EMPTY_DIRECT), text(html));
    assert.ok(!/No observer is within radio range/.test(html));
    assert.ok(html.includes('title="' + HB.EMPTY_DIRECT_TIP.replace(/'/g, '&#39;') + '"'));
  }
  assert.ok(!HB.renderTables(health).includes(HB.EMPTY_DIRECT), 'no empty note when there are direct observers');
});

test('compact list (side panel / Live): stats per kind, no orphan separators', () => {
  const html = HB.renderList(health);
  const rows = html.split('<div class="observer-row').slice(1).map(text);
  assert.strictEqual(rows.length, 5);
  assert.match(rows[0], /Name d1 repeater 4 pkts · SNR 7\.3dB · RSSI -101 dBm/);
  assert.match(rows[1], /Name d2 1 pkts · RSSI -95 dBm/);
  assert.match(rows[2], /Name r1 listener 12 pkts\s*$/);
  for (const r of rows) assert.ok(!/·\s*·|·\s*$/.test(r.trim()), 'orphan separator: ' + r);
  assert.ok(!html.includes('href='), 'side panel names are not links');
  const linked = HB.renderList(health, { link: true });
  assert.ok(linked.includes('href="#/observers/d1"') && linked.includes('href="#/observers/r1"'));
});

test('tooltips are precise: routing evidence, per-observation rules, no radio-range claim', () => {
  assert.match(HB.DIRECT_TIP, /own transmission/);
  assert.match(HB.DIRECT_TIP, /empty flood path/);
  assert.match(HB.DIRECT_TIP, /zero-hop/);
  assert.match(HB.DIRECT_TIP, /last hop/);
  assert.match(HB.DIRECT_TIP, /observed routing evidence, not a radio range/);
  assert.match(HB.RELAY_TIP, /belong to another transmitter/);
  assert.match(HB.EMPTY_DIRECT_TIP, /missing evidence, not proof of missing coverage/);
  const html = HB.renderTables(health);
  assert.ok(html.includes('role="tooltip"') && /aria-describedby="heardByHelp\d+"/.test(html));
  const ids = html.match(/id="heardByHelp\d+"/g);
  assert.strictEqual(new Set(ids).size, ids.length, 'tooltip ids must be unique');
});

test('hostile names and ids are escaped', () => {
  const evil = '"><img src=x onerror=alert(1)>';
  const h = { directObservers: [direct(evil, 1, 1, 1, { observer_name: evil })], relayObservers: [relay("x'" + evil, 1, { observer_name: '' })] };
  for (const html of [HB.renderTables(h), HB.renderList(h), HB.renderList(h, { link: true })]) {
    assert.ok(!/<img/i.test(html), html);
    assert.ok(!/onerror="/.test(html.replace(/&quot;/g, '')) || !/<img/.test(html));
  }
});

test('singular heading', () => {
  assert.match(text(HB.renderList({ directObservers: [direct('a', 1, 1, 1)] })), /Heard By \(1 observer\)/);
});

console.log(`\n  test-heard-by: ${passed} passed, ${failed} failed`);
process.exit(failed ? 1 : 0);
