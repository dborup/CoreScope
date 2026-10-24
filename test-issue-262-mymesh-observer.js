/* Unit tests for issue #262 — My Mesh observer identity, status and traffic.
 *
 * public/my-mesh-observers.js is the shared helper the My Mesh cards use to
 * decide whether one of the operator's nodes is ALSO a registered observer,
 * and to label the observer's reporting status and traffic without borrowing
 * the node's own numbers. Loads the real file in a vm context, plus static
 * pins on home.js / home.css / index.html for the wiring.
 *
 * Run: node test-issue-262-mymesh-observer.js
 */
'use strict';
const assert = require('assert');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const src = fs.readFileSync(path.join(__dirname, 'public', 'my-mesh-observers.js'), 'utf8');
const ctx = { window: {}, console };
vm.createContext(ctx);
vm.runInContext(src, ctx, { filename: 'my-mesh-observers.js' });
const MMO = ctx.window.MyMeshObservers;

const homeJs = fs.readFileSync(path.join(__dirname, 'public', 'home.js'), 'utf8');
const homeCss = fs.readFileSync(path.join(__dirname, 'public', 'home.css'), 'utf8');
const indexHtml = fs.readFileSync(path.join(__dirname, 'public', 'index.html'), 'utf8');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

// Synthetic fixture identities (never production keys). Two devices whose
// NAMES are confusable but whose keys differ: the staging report had "Dokk1"
// wrongly sharing a card badge with "Dokk1 Meshview".
const KEY_PLAIN = 'a'.repeat(64);          // node only, no observer record
const KEY_OBS = 'b'.repeat(64);            // node AND observer
const KEY_OBS_UPPER = KEY_OBS.toUpperCase();
const KEY_ROOM = 'c'.repeat(64);           // observer with a room mesh role

const payload = {
  server_time: '2026-10-24T10:00:00Z',
  observers: [
    { id: KEY_OBS_UPPER, name: 'Dokk1 Lab Meshview', last_seen: '2026-10-24T09:58:00Z', last_packet_at: '2026-10-24T09:59:00Z', packetsLastHour: 1234, packet_count: 99999, nodeRole: 'repeater' },
    { id: KEY_ROOM.toUpperCase(), name: 'Aarhus Chat', last_seen: '2026-10-23T09:00:00Z', last_packet_at: null, packetsLastHour: 0 },
  ],
};

console.log('\n=== #262 observer index + identity matching ===');

test('index() builds one O(1) map keyed by the normalised full public key', () => {
  const idx = MMO.index(payload);
  assert.strictEqual(idx.available, true);
  assert.strictEqual(idx.byId.constructor.name, 'Map', 'index must expose a Map, not an array to scan');
  assert.strictEqual(typeof idx.byId.get, 'function', 'lookups must be O(1) map gets');
  assert.strictEqual(idx.byId.size, 2);
  assert.ok(idx.byId.has(KEY_OBS), 'key must be stored lowercased');
});

test('match() is case-insensitive on the FULL key in both directions', () => {
  const idx = MMO.index(payload);
  assert.strictEqual(MMO.match(idx, KEY_OBS).name, 'Dokk1 Lab Meshview');
  assert.strictEqual(MMO.match(idx, KEY_OBS_UPPER).name, 'Dokk1 Lab Meshview');
  assert.strictEqual(MMO.match(idx, KEY_OBS.slice(0, 32) + KEY_OBS.slice(32).toUpperCase()).name, 'Dokk1 Lab Meshview');
});

test('a node without an observer record never matches', () => {
  assert.strictEqual(MMO.match(MMO.index(payload), KEY_PLAIN), null);
});

test('a short prefix of a registered observer key never matches', () => {
  const idx = MMO.index(payload);
  for (const prefix of [KEY_OBS.slice(0, 2), KEY_OBS.slice(0, 12), KEY_OBS.slice(0, 63)]) {
    assert.strictEqual(MMO.match(idx, prefix), null, 'prefix ' + prefix.length + ' must not match');
  }
});

test('two different FULL keys sharing a long leading prefix never match (no prefix fallback)', () => {
  // Observer and node keys agree on the first 12..62 hex chars but differ at
  // the end: only the full normalised key is an identity.
  for (const shared of [12, 16, 32, 63]) {
    const observerKey = KEY_OBS.slice(0, shared) + 'e'.repeat(64 - shared);
    const nodeKey = KEY_OBS.slice(0, shared) + 'f'.repeat(64 - shared);
    assert.notStrictEqual(observerKey, nodeKey);
    const idx = MMO.index({ observers: [{ id: observerKey.toUpperCase(), name: 'Neighbour' }] });
    assert.strictEqual(idx.byId.size, 1);
    assert.strictEqual(MMO.match(idx, nodeKey), null, shared + ' shared leading hex chars must not match');
    assert.strictEqual(MMO.match(idx, observerKey).name, 'Neighbour');
  }
});

test('"Dokk1" and "Dokk1 Meshview" stay separate devices — names never match', () => {
  const idx = MMO.index(payload);
  // The plain node's display name is a prefix of the observer's display name.
  assert.strictEqual(MMO.match(idx, 'Dokk1 Lab'), null);
  assert.strictEqual(MMO.match(idx, 'Dokk1 Lab Meshview'), null);
  // And nothing in the index is keyed by a name.
  assert.ok(![...idx.byId.keys()].some(k => /dokk/i.test(k)), 'observer names must not become index keys');
});

test('ids that are not a full 64-hex public key are not indexed', () => {
  const idx = MMO.index({ observers: [{ id: 'dokk1' }, { id: KEY_OBS.slice(0, 63) }, { id: KEY_OBS + 'ab' }, { id: 'g'.repeat(64) }, { id: 42 }, { id: null }, {}] });
  assert.strictEqual(idx.available, true);
  assert.strictEqual(idx.byId.size, 0);
});

test('surrounding whitespace on an id is normalised away', () => {
  const idx = MMO.index({ observers: [{ id: '  ' + KEY_OBS_UPPER + '\n', name: 'Padded' }] });
  assert.strictEqual(MMO.match(idx, KEY_OBS).name, 'Padded');
});

test('a missing/failed/garbage payload yields an UNAVAILABLE index, never an empty-but-ok one', () => {
  for (const bad of [null, undefined, {}, { observers: null }, { observers: 'x' }, []]) {
    const idx = MMO.index(bad);
    assert.strictEqual(idx.available, false, 'payload ' + JSON.stringify(bad) + ' must be unavailable');
    assert.strictEqual(idx.byId.size, 0);
  }
  // An unavailable index must never report a match — that would be a guess.
  assert.strictEqual(MMO.match(MMO.index(null), KEY_OBS), null);
});

test('an empty but successful observer list is available with zero rows', () => {
  const idx = MMO.index({ observers: [] });
  assert.strictEqual(idx.available, true);
  assert.strictEqual(idx.byId.size, 0);
});

console.log('\n=== #262 dual role labelling ===');

test('roleSummary keeps the mesh role as a secondary term', () => {
  assert.strictEqual(MMO.roleSummary('repeater'), 'Observer · Repeater');
  assert.strictEqual(MMO.roleSummary('ROOM'), 'Observer · Room');
  assert.strictEqual(MMO.roleSummary('room_server'), 'Observer · Room_server');
});

test('roleSummary never invents a mesh role when the node has none', () => {
  for (const role of [null, undefined, '', '   ', 42, {}]) {
    assert.strictEqual(MMO.roleSummary(role), 'Observer', 'role ' + JSON.stringify(role));
  }
});

test('OBSERVER is the primary badge and the mesh role is not overwritten', () => {
  assert.strictEqual(MMO.PRIMARY_BADGE, 'Observer');
});

console.log('\n=== #262 observer reporting status ===');

// The observers page owns the thresholds (#1552/#1563). This helper must
// delegate to that classifier and only map its class onto the card's dots.
const classifier = lastSeen => {
  const ago = Date.parse('2026-10-24T10:00:00Z') - Date.parse(lastSeen);
  if (ago < 3600000) return { cls: 'health-green', label: 'Online' };
  if (ago < 86400000) return { cls: 'health-yellow', label: 'Stale' };
  return { cls: 'health-red', label: 'Offline' };
};

test('status() uses the observer status report, not the node advert', () => {
  const o = { id: KEY_OBS, last_seen: '2026-10-24T09:58:00Z' };
  const s = MMO.status(o, classifier);
  assert.strictEqual(s.label, 'Online');
  assert.strictEqual(s.cls, 'status-ok');
  assert.strictEqual(s.reportedAt, '2026-10-24T09:58:00Z');
});

test('status() maps every classifier class onto a distinct card dot', () => {
  assert.strictEqual(MMO.status({ last_seen: '2026-10-24T05:00:00Z' }, classifier).cls, 'status-warn');
  assert.strictEqual(MMO.status({ last_seen: '2026-10-24T05:00:00Z' }, classifier).label, 'Stale');
  assert.strictEqual(MMO.status({ last_seen: '2026-10-20T05:00:00Z' }, classifier).cls, 'status-err');
  assert.strictEqual(MMO.status({ last_seen: '2026-10-20T05:00:00Z' }, classifier).label, 'Offline');
});

test('no observer status report yields Unknown, never a false Offline', () => {
  for (const last of [null, undefined, '', '   ', 7, {}]) {
    const s = MMO.status({ last_seen: last }, classifier);
    assert.strictEqual(s.label, 'Unknown', 'last_seen ' + JSON.stringify(last));
    assert.strictEqual(s.cls, 'status-muted');
    assert.strictEqual(s.reportedAt, null);
  }
});

test('an unparseable status timestamp yields Unknown rather than being handed to the classifier', () => {
  let called = 0;
  const s = MMO.status({ last_seen: 'not-a-date' }, () => { called++; return { cls: 'health-green', label: 'Online' }; });
  assert.strictEqual(s.label, 'Unknown');
  assert.strictEqual(s.cls, 'status-muted');
  assert.strictEqual(called, 0, 'an unparseable timestamp must not reach the classifier');
});

test('status() falls back to window.observerHealthStatus, the shared classifier', () => {
  ctx.window.observerHealthStatus = () => ({ cls: 'health-yellow', label: 'Stale' });
  try {
    assert.strictEqual(MMO.status({ last_seen: '2026-10-24T09:58:00Z' }).label, 'Stale');
  } finally { delete ctx.window.observerHealthStatus; }
});

test('with no classifier available the status is Unknown, not a locally invented threshold', () => {
  const s = MMO.status({ last_seen: '2026-10-24T09:58:00Z' });
  assert.strictEqual(s.label, 'Unknown');
  assert.strictEqual(s.cls, 'status-muted');
});

test('the status row names its own source without repeating the heading', () => {
  assert.strictEqual(MMO.STATUS_ROW_LABEL, 'Status');
  assert.strictEqual(MMO.LAST_REPORT_ROW_LABEL, 'Last packet reported');
  assert.strictEqual(MMO.STATUS_REPORTED_PREFIX, ' · reported ');
  assert.ok(/observer/i.test(MMO.STATUS_TIP), 'the tooltip must still say whose status this is');
  assert.ok(/MQTT|own status report/i.test(MMO.STATUS_TIP));
  assert.ok(/60 minutes|last hour/i.test(MMO.TRAFFIC_TIP));
  assert.ok(/not traffic sent by the node/i.test(MMO.TRAFFIC_TIP), 'the traffic tip must disclaim node traffic');
});

test('status(null) is null — a non-observer card has no observer status row', () => {
  assert.strictEqual(MMO.status(null, classifier), null);
});

test('lastReport() returns the observer packet timestamp only when it parses', () => {
  assert.strictEqual(MMO.lastReport({ last_packet_at: '2026-10-24T09:59:00Z' }), '2026-10-24T09:59:00Z');
  for (const v of [null, undefined, '', 'nope', 5, {}]) assert.strictEqual(MMO.lastReport({ last_packet_at: v }), null, JSON.stringify(v));
  assert.strictEqual(MMO.lastReport(null), null);
});

console.log('\n=== #262 observer traffic label + window ===');

test('traffic() names the population AND the time window', () => {
  assert.strictEqual(MMO.TRAFFIC_LABEL, 'Packets reported · last hour');
  const t = MMO.traffic({ packetsLastHour: 1234 });
  assert.strictEqual(t.label, 'Packets reported · last hour');
  assert.ok(/last hour/.test(t.label), 'the window must be stated');
  assert.ok(!/today/i.test(t.label), 'an hourly count must never be labelled "today"');
  assert.strictEqual(t.available, true);
  assert.strictEqual(t.value, '1,234');
});

test('a genuine zero stays a zero', () => {
  const t = MMO.traffic({ packetsLastHour: 0 });
  assert.strictEqual(t.available, true);
  assert.strictEqual(t.value, '0');
});

test('missing or invalid observer traffic is never shown as zero', () => {
  for (const v of [undefined, null, '12', NaN, Infinity, -1, {}, true]) {
    const t = MMO.traffic({ packetsLastHour: v });
    assert.strictEqual(t.available, false, 'packetsLastHour ' + JSON.stringify(v) + ' must be unavailable');
    assert.strictEqual(t.value, '—', 'packetsLastHour ' + JSON.stringify(v) + ' must render an em dash, not 0');
  }
  assert.strictEqual(MMO.traffic(null).available, false);
  assert.strictEqual(MMO.traffic(null).value, '—');
});

test('the traffic label is not the node sparkline label', () => {
  assert.notStrictEqual(MMO.TRAFFIC_LABEL, MMO.NODE_SECTION_LABEL);
  assert.strictEqual(MMO.NODE_SECTION_LABEL, 'Node activity');
  assert.strictEqual(MMO.OBSERVER_SECTION_LABEL, 'Observer reporting');
});

console.log('\n=== #262 observer navigation ===');

test('pageHref targets the matched observer with its id case preserved', () => {
  // observers rows are stored uppercase, nodes lowercase (#1836) — the card
  // must link with the observer record's own id, not a re-cased node key.
  assert.strictEqual(MMO.pageHref({ id: KEY_OBS_UPPER }), '#/observers/' + KEY_OBS_UPPER);
  assert.strictEqual(MMO.pageHref(MMO.match(MMO.index(payload), KEY_OBS)), '#/observers/' + KEY_OBS_UPPER);
});

test('pageHref percent-encodes and refuses an unusable id', () => {
  assert.strictEqual(MMO.pageHref({ id: 'a b/c#d' }), '#/observers/a%20b%2Fc%23d');
  for (const o of [null, undefined, {}, { id: '' }, { id: 5 }]) assert.strictEqual(MMO.pageHref(o), null, JSON.stringify(o));
});

console.log('\n=== #262 home wiring (static) ===');

test('index.html loads my-mesh-observers.js before home.js', () => {
  const helper = indexHtml.indexOf('my-mesh-observers.js');
  const home = indexHtml.indexOf('home.js?v=');
  assert.ok(helper > 0, 'my-mesh-observers.js must be in index.html');
  assert.ok(home > 0 && helper < home, 'the helper must load before home.js');
});

test('home.js fetches the observer list in bulk, exactly once per grid render', () => {
  assert.ok(/api\('\/observers'[^)]*CLIENT_TTL\.observers/.test(homeJs),
    "home.js must fetch '/observers' once with CLIENT_TTL.observers");
  const fetches = homeJs.match(/api\('\/observers'/g) || [];
  assert.strictEqual(fetches.length, 1, 'exactly one /observers call site, never one per card');
  // The call must not sit inside the per-node map callback.
  const mapAt = homeJs.indexOf('myNodes.map(async (mn)');
  assert.ok(mapAt > 0, 'per-node map callback not found');
  assert.ok(homeJs.indexOf("api('/observers'") < mapAt, '/observers must be fetched before the per-card loop');
});

test('home.js matches by key through the helper, never by node name', () => {
  assert.ok(/MyMeshObservers\.match\(/.test(homeJs), 'home.js must use MyMeshObservers.match');
  assert.ok(!/observer[^\n]*\.name\s*===/.test(homeJs), 'no name equality matching in home.js');
});

test('home.js keeps the mesh role badge alongside the observer badge', () => {
  assert.ok(/class="mnc-role"/.test(homeJs), 'the mesh role badge must survive');
  assert.ok(/mnc-badge-observer/.test(homeJs), 'the observer badge must be rendered');
});

test('home.js shows an explicit notice when the observer list is unavailable', () => {
  assert.ok(/my-nodes-observer-notice/.test(homeJs), 'an unavailable observer list must surface a notice');
});

test('home.css styles the new observer chrome with theme variables only', () => {
  for (const sel of ['.mnc-badge-observer', '.mnc-observer-panel', '.mnc-observer-row', '.mnc-section-lbl', '.my-nodes-observer-notice']) {
    assert.ok(homeCss.includes(sel), 'home.css must style ' + sel);
  }
  const from = homeCss.indexOf('.mnc-badge-observer');
  const block = homeCss.slice(from, homeCss.indexOf('.mnc-metrics {', from));
  assert.ok(block.includes('.my-nodes-observer-notice'), 'the scanned #262 block must cover every new rule');
  // Strip comments first: an issue reference like "#304" is not a color.
  const hex = block.replace(/\/\*[\s\S]*?\*\//g, '').match(/#[0-9a-fA-F]{3,8}\b/g);
  assert.strictEqual(hex, null, 'no hardcoded colors in the #262 block, got ' + JSON.stringify(hex));
  assert.ok(/\.mnc-badge-observer[\s\S]{0,260}var\(--role-observer\)/.test(homeCss), 'the observer badge must use --role-observer');
});

console.log('\n' + passed + ' passed, ' + failed + ' failed');
process.exit(failed ? 1 : 0);
