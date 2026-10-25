/* Unit tests for issue #304 (remainder) — My Mesh status evidence, SNR
 * attribution and traffic metrics.
 *
 * public/my-mesh-activity.js is the shared helper the My Mesh cards and the
 * home health panel use to label what the node-health payload actually
 * proves: which evidence drives the status badge, what the direct SNR mean
 * covers, and what the rolling 24-hour traffic count counts. It owns no
 * thresholds of its own — liveness stays with roles.js (#539) — and it never
 * turns missing evidence into a confident answer.
 *
 * Run: node test-issue-304-my-mesh-metrics.js
 */
'use strict';
const assert = require('assert');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const rolesSrc = fs.readFileSync(path.join(__dirname, 'public', 'roles.js'), 'utf8');
const src = fs.readFileSync(path.join(__dirname, 'public', 'my-mesh-activity.js'), 'utf8');
const ctx = {
  console, Date, fetch: async () => ({ json: async () => ({}) }),
  document: { readyState: 'complete', createElement: () => ({}), head: { appendChild() {} }, getElementById: () => null, addEventListener() {} },
};
ctx.window = ctx;
vm.createContext(ctx);
// roles.js is loaded for real: the helper must reuse the shipped liveness
// rules (getHealthStatus / getHealthLastHeard), not a copy of them.
vm.runInContext(rolesSrc, ctx, { filename: 'roles.js' });
vm.runInContext(src, ctx, { filename: 'my-mesh-activity.js' });
const MMA = ctx.MyMeshActivity;

const homeJs = fs.readFileSync(path.join(__dirname, 'public', 'home.js'), 'utf8');
const homeCss = fs.readFileSync(path.join(__dirname, 'public', 'home.css'), 'utf8');
const indexHtml = fs.readFileSync(path.join(__dirname, 'public', 'index.html'), 'utf8');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

const NOW = Date.parse('2026-10-25T12:00:00Z');
const ago = min => new Date(NOW - min * 60000).toISOString();
const VERDICTS = /excellent|\bgood\b|marginal|\bpoor\b/i;

// A rolling window payload as the server sends it (cmd/server/node_activity_24h.go).
function activity(opts) {
  const o = opts || {};
  const end = new Date(NOW).toISOString();
  const start = new Date(NOW - 24 * 3600000).toISOString();
  const prevStart = new Date(NOW - 48 * 3600000).toISOString();
  const counts = o.counts || [];
  return {
    windowStart: start, windowEnd: end,
    coverageStart: o.coverageStart === undefined ? prevStart : o.coverageStart,
    complete: o.complete !== false,
    buckets: Array.from({ length: 24 }, (_, i) => ({
      start: new Date(NOW - (24 - i) * 3600000).toISOString(),
      end: new Date(NOW - (23 - i) * 3600000).toISOString(),
      count: counts[i] || 0,
    })),
    previous: o.previous === null ? undefined
      : Object.assign({ windowStart: prevStart, windowEnd: start, count: 0, complete: true }, o.previous),
  };
}

console.log('\n=== #304 point 3: status evidence ===');

test('an own advert inside the threshold is confident, and says so', () => {
  const ev = MMA.statusEvidence({ node: { role: 'repeater' }, stats: { lastHeard: ago(10), lastAdvert: ago(10) }, nowMs: NOW });
  assert.strictEqual(ev.status, 'healthy');
  assert.strictEqual(ev.source, 'advert');
  assert.strictEqual(ev.confident, true);
  assert.strictEqual(ev.cls, 'status-ok');
  assert.strictEqual(ev.label, 'Active');
  assert.strictEqual(ev.text, 'Active');
  assert.strictEqual(ev.advertAt, ago(10));
  assert.strictEqual(ev.relayAt, null, 'relay activity is only reported when it is newer than the advert');
  assert.match(ev.explanation, /own advert/i);
  assert.match(ev.explanation, /not proof of continuous uptime|not proof .*online/i);
});

test('relay-only evidence never produces a confident green status', () => {
  // Own advert 30h old (degraded for infra), relay evidence 10 min old.
  const ev = MMA.statusEvidence({ node: { role: 'repeater' }, stats: { lastHeard: ago(10), lastAdvert: ago(30 * 60) }, nowMs: NOW });
  assert.strictEqual(ev.status, 'healthy', 'the shared thresholds still classify the newest evidence');
  assert.strictEqual(ev.source, 'relay');
  assert.strictEqual(ev.confident, false);
  assert.notStrictEqual(ev.cls, 'status-ok', 'an attributed match must not be shown as a confident green');
  assert.strictEqual(ev.cls, 'status-warn');
  assert.strictEqual(ev.glyph, 'ph-circle', 'an attributed status uses the hollow dot');
  assert.strictEqual(ev.text, 'Active · relay-attributed');
  assert.strictEqual(ev.relayAt, ago(10));
  assert.strictEqual(ev.advertAt, ago(30 * 60));
  assert.match(ev.explanation, /prefix/i, 'the explanation must name the short-prefix uncertainty');
});

test('a node with no own advert at all is relay-attributed, never confident', () => {
  const ev = MMA.statusEvidence({ node: { role: 'repeater' }, stats: { lastHeard: ago(5), lastAdvert: null }, nowMs: NOW });
  assert.strictEqual(ev.source, 'relay');
  assert.strictEqual(ev.confident, false);
  assert.notStrictEqual(ev.cls, 'status-ok');
  assert.strictEqual(ev.advertAt, null);
  assert.strictEqual(ev.relayAt, ago(5));
});

test('an advert that alone supports the badge stays confident even with newer relay traffic', () => {
  const ev = MMA.statusEvidence({ node: { role: 'repeater' }, stats: { lastHeard: ago(5), lastAdvert: ago(120) }, nowMs: NOW });
  assert.strictEqual(ev.status, 'healthy');
  assert.strictEqual(ev.source, 'advert');
  assert.strictEqual(ev.confident, true);
  assert.strictEqual(ev.cls, 'status-ok');
  assert.strictEqual(ev.relayAt, ago(5), 'the newer relay activity is still reported separately');
});

test('no identity-safe activity is reported as missing evidence', () => {
  for (const stats of [{}, { lastHeard: null }, { lastHeard: '', lastAdvert: '' }, { lastHeard: 'nonsense' }]) {
    const ev = MMA.statusEvidence({ node: { role: 'repeater' }, stats, nowMs: NOW });
    assert.strictEqual(ev.source, 'none', JSON.stringify(stats));
    assert.strictEqual(ev.confident, false);
    assert.strictEqual(ev.heardAt, null);
    assert.strictEqual(ev.relayAt, null);
    assert.match(ev.explanation, /missing evidence|not proof/i);
  }
});

test('the role thresholds come from roles.js, with no home-page copy', () => {
  // A 90-minute silence is healthy for infra (24h) and degraded for a
  // client (1h). Both must fall out of the shared classifier.
  const infra = MMA.statusEvidence({ node: { role: 'repeater' }, stats: { lastHeard: ago(90), lastAdvert: ago(90) }, nowMs: NOW });
  const client = MMA.statusEvidence({ node: { role: 'client' }, stats: { lastHeard: ago(90), lastAdvert: ago(90) }, nowMs: NOW });
  assert.strictEqual(infra.status, 'healthy');
  assert.strictEqual(client.status, 'degraded');
  assert.strictEqual(client.cls, 'status-warn');
  assert.ok(!/HEALTH_THRESHOLDS\s*=|degradedMs\s*[:=]\s*\d/.test(src), 'the helper must not define its own thresholds');
  // An injected classifier is used instead of the global: dependency
  // injection, so the helper is testable without roles.js at all.
  const injected = MMA.statusEvidence({
    node: { role: 'repeater' }, stats: { lastHeard: ago(1), lastAdvert: ago(1) }, nowMs: NOW,
    classify: () => 'silent',
  });
  assert.strictEqual(injected.status, 'silent');
  assert.strictEqual(injected.cls, 'status-err');
});

test('the two evidence rows are labelled separately and never merged', () => {
  assert.strictEqual(MMA.ADVERT_ROW_LABEL, 'Last own advert');
  assert.strictEqual(MMA.RELAY_ROW_LABEL, 'Last attributed relay activity');
  const ev = MMA.statusEvidence({ node: { role: 'repeater' }, stats: { lastHeard: ago(10), lastAdvert: ago(10) }, nowMs: NOW });
  assert.strictEqual(ev.relayText, MMA.RELAY_NONE_NEWER, 'no newer relay evidence must say so, not show the advert time');
  assert.notStrictEqual(MMA.RELAY_NONE_NEWER, MMA.UNKNOWN_VALUE);
  const noAdvert = MMA.statusEvidence({ node: { role: 'repeater' }, stats: { lastHeard: ago(5) }, nowMs: NOW });
  assert.strictEqual(noAdvert.advertText, MMA.ADVERT_NONE);
});

console.log('\n=== #304 point 4: SNR attribution ===');

test('the direct SNR mean carries no quality verdict and no verdict colour', () => {
  const direct = [{ packetCount: 30, avgSnr: 11 }, { packetCount: 12, avgSnr: 9 }];
  for (const avgSnr of [11.2, 4.5, -2.5, -12]) {
    const snr = MMA.snr({ avgSnr }, direct);
    assert.strictEqual(snr.available, true, String(avgSnr));
    assert.strictEqual(snr.value, avgSnr.toFixed(1) + ' dB');
    assert.ok(!VERDICTS.test(snr.label + snr.value), 'no verdict in ' + snr.label);
    assert.strictEqual(snr.color, undefined, 'the helper must not hand the card a verdict colour');
  }
  const code = src.replace(/\/\*[\s\S]*?\*\//g, '').replace(/^\s*\/\/.*$/gm, '');
  assert.ok(!VERDICTS.test(code), 'no quality verdict words in the helper code');
});

test('the SNR label states the sample scope it was averaged over', () => {
  const snr = MMA.snr({ avgSnr: -3.25 }, [{ packetCount: 30 }, { packetCount: 12 }]);
  assert.strictEqual(snr.samples, 42);
  assert.strictEqual(snr.label, 'Direct SNR · n=42');
  assert.strictEqual(snr.value, '-3.3 dB');
  assert.match(snr.tip, /direct reception/i);
  assert.match(snr.tip, /observer/i, 'the tip must say the value is measured at the observer');
  assert.match(snr.tip, /not .*(radio quality|signal quality|health)/i);
});

test('an SNR with no direct attribution behind it is not presented at all', () => {
  // #76: a mean without a proven direct reception would describe whichever
  // node the observer actually heard.
  const none = MMA.snr({ avgSnr: 7 }, []);
  assert.strictEqual(none.available, false);
  assert.strictEqual(none.value, MMA.UNKNOWN_VALUE);
  assert.strictEqual(none.label, 'Direct SNR');
  assert.strictEqual(MMA.snr({}, [{ packetCount: 3 }]).available, false);
  for (const v of [null, undefined, 'x', NaN, Infinity, {}, true]) {
    assert.strictEqual(MMA.snr({ avgSnr: v }, [{ packetCount: 3 }]).available, false, JSON.stringify(v));
  }
});

test('a non-numeric packet count never fabricates a sample size', () => {
  const snr = MMA.snr({ avgSnr: 1 }, [{ packetCount: '7' }, { packetCount: null }, {}]);
  assert.strictEqual(snr.available, true);
  assert.strictEqual(snr.samples, 7, 'a numeric string is cast at the boundary');
  const unknown = MMA.snr({ avgSnr: 1 }, [{ packetCount: 'many' }]);
  assert.strictEqual(unknown.samples, 0);
  assert.strictEqual(unknown.label, 'Direct SNR', 'an unknown sample size must not print n=0');
});

console.log('\n=== #304 point 5: traffic metrics ===');

test('the traffic metric is a labelled rolling 24h count, not "packets today"', () => {
  const t = MMA.traffic24h(activity({ counts: { 3: 35, 20: 5 } }));
  assert.strictEqual(t.available, true);
  assert.strictEqual(t.exact, true);
  assert.strictEqual(t.count, 40);
  assert.strictEqual(t.value, '40');
  assert.strictEqual(t.label, MMA.TRAFFIC_LABEL);
  assert.match(MMA.TRAFFIC_LABEL, /24\s?h/);
  assert.ok(!/today/i.test(MMA.TRAFFIC_LABEL + MMA.TRAFFIC_TIP), 'the window must not be described as a calendar day');
  assert.match(t.tip, /transmission/i);
  assert.ok(!/forward/i.test(t.tip) || /not .*forward/i.test(t.tip), 'node-associated traffic must not be sold as forwarded traffic');
});

test('an incomplete window is a lower bound, never a measured total', () => {
  const t = MMA.traffic24h(activity({ complete: false, coverageStart: new Date(NOW - 8 * 3600000).toISOString(), counts: { 20: 5 } }));
  assert.strictEqual(t.available, true);
  assert.strictEqual(t.exact, false);
  assert.strictEqual(t.count, 5);
  assert.strictEqual(t.value, '≥ 5');
  assert.match(t.tip, /incomplete|partial/i);
});

test('a missing or malformed window is unavailable, not zero', () => {
  for (const bad of [undefined, null, {}, { buckets: [] }, { complete: true, buckets: 'x' },
    { complete: true, buckets: Array.from({ length: 23 }, () => ({ start: ago(0), end: ago(0), count: 0 })) },
    { complete: true, buckets: Array.from({ length: 24 }, () => ({ start: ago(0), end: ago(0), count: -1 })) },
    { complete: true, buckets: Array.from({ length: 24 }, () => ({ start: 'x', end: 'y', count: 1 })) }]) {
    const t = MMA.traffic24h(bad);
    assert.strictEqual(t.available, false, JSON.stringify(bad));
    assert.strictEqual(t.value, MMA.UNKNOWN_VALUE, JSON.stringify(bad));
  }
  assert.strictEqual(MMA.buckets(activity()).length, 24);
  assert.strictEqual(MMA.buckets({ buckets: [] }), null);
});

test('a genuine quiet window with proven coverage is an exact zero', () => {
  const t = MMA.traffic24h(activity({}));
  assert.strictEqual(t.available, true);
  assert.strictEqual(t.exact, true);
  assert.strictEqual(t.value, '0');
});

test('the comparison needs two complete, like-for-like windows', () => {
  const up = MMA.compare24h(activity({ counts: { 1: 60 }, previous: { count: 50 } }));
  assert.strictEqual(up.available, true);
  assert.strictEqual(up.pct, 20);
  assert.strictEqual(up.text, '+20% vs previous 24 h');
  const down = MMA.compare24h(activity({ counts: { 1: 40 }, previous: { count: 50 } }));
  assert.strictEqual(down.pct, -20);
  assert.strictEqual(down.text, '-20% vs previous 24 h');
  assert.match(up.tip, /previous 24/i);
});

test('an unknown previous window is never treated as zero traffic', () => {
  for (const prev of [null, { complete: false, count: 0 }, { complete: false, count: 99 }, { count: 7, complete: undefined }, { count: '7' }, { count: 1.5 }]) {
    const c = MMA.compare24h(activity({ counts: { 1: 10 }, previous: prev }));
    assert.strictEqual(c.available, false, JSON.stringify(prev));
    assert.strictEqual(c.pct, null, JSON.stringify(prev));
    assert.strictEqual(c.text, MMA.COMPARE_NO_HISTORY, JSON.stringify(prev));
  }
  // An incomplete CURRENT window is not comparable either.
  const partial = MMA.compare24h(activity({ complete: false, counts: { 1: 10 }, previous: { count: 5 } }));
  assert.strictEqual(partial.available, false);
  assert.strictEqual(partial.text, MMA.COMPARE_NO_HISTORY);
});

test('a zero baseline produces no percentage and never divides by zero', () => {
  const c = MMA.compare24h(activity({ counts: { 1: 9 }, previous: { count: 0 } }));
  assert.strictEqual(c.available, true);
  assert.strictEqual(c.pct, null);
  assert.strictEqual(c.delta, 9);
  assert.strictEqual(c.text, '+9 vs 0 in the previous 24 h');
  assert.ok(Number.isFinite(c.delta));
  const quiet = MMA.compare24h(activity({ previous: { count: 0 } }));
  assert.strictEqual(quiet.pct, null);
  assert.strictEqual(quiet.text, '0 vs 0 in the previous 24 h');
});

test('avg hops is relabelled as an observed path length, with its sample scope', () => {
  const h = MMA.hops({ avgHops: 10, totalTransmissions: 240 });
  assert.strictEqual(h.available, true);
  assert.strictEqual(h.value, '10 hops');
  assert.strictEqual(h.label, MMA.HOPS_LABEL);
  assert.ok(!/avg hops/i.test(MMA.HOPS_LABEL));
  assert.match(MMA.HOPS_LABEL, /path/i);
  assert.match(h.tip, /distance/i, 'the tip must deny the distance reading');
  assert.match(h.tip, /path/i);
  assert.strictEqual(MMA.hops({ avgHops: 1 }).value, '1 hop');
  for (const v of [null, undefined, 'x', NaN, -1, {}]) {
    assert.strictEqual(MMA.hops({ avgHops: v }).available, false, JSON.stringify(v));
    assert.strictEqual(MMA.hops({ avgHops: v }).value, MMA.UNKNOWN_VALUE);
  }
  assert.strictEqual(MMA.hops({ avgHops: '3' }).value, '3 hops', 'cast at the boundary');
});

test('the window text states both bounds and the coverage caveat', () => {
  const w = MMA.windowText(activity());
  assert.ok(w.includes(new Date(NOW - 24 * 3600000).toLocaleString()), w);
  assert.ok(w.includes(new Date(NOW).toLocaleString()), w);
  const partial = MMA.windowText(activity({ complete: false, coverageStart: new Date(NOW - 8 * 3600000).toISOString() }));
  assert.match(partial, /incomplete|partial/i);
  assert.strictEqual(MMA.windowText(null), MMA.UNKNOWN_VALUE);
});

console.log('\n=== #304 home wiring (static) ===');

test('index.html loads my-mesh-activity.js before home.js', () => {
  const helper = indexHtml.indexOf('my-mesh-activity.js');
  const home = indexHtml.indexOf('home.js?v=');
  assert.ok(helper > 0, 'my-mesh-activity.js must be in index.html');
  assert.ok(home > 0 && helper < home, 'the helper must load before home.js');
});

test('home.js has no SNR verdict or verdict colour left', () => {
  assert.ok(!/snrLabel|snrColor/.test(homeJs), 'the SNR verdict and its colour must be gone');
  assert.ok(!VERDICTS.test(homeJs.replace(/\/\*[\s\S]*?\*\//g, '').replace(/^\s*\/\/.*$/gm, '')),
    'no quality verdict words left in home.js code');
});

test('home.js no longer shows an unexplained "Packets today" or "Avg hops"', () => {
  assert.ok(!/Packets\s+[Tt]oday/.test(homeJs), 'the calendar-day counter must be gone');
  assert.ok(!/>Avg [Hh]ops</.test(homeJs), 'the bare Avg hops label must be gone');
  assert.ok(/MyMeshActivity\.traffic24h\(/.test(homeJs), 'the card must use the rolling-window helper');
  assert.ok(/MyMeshActivity\.statusEvidence\(/.test(homeJs));
  assert.ok(/MyMeshActivity\.snr\(/.test(homeJs));
  assert.ok(/MyMeshActivity\.hops\(/.test(homeJs));
});

test('the card adds no API request of its own', () => {
  const calls = homeJs.match(/\bapi\(/g) || [];
  assert.strictEqual(calls.length, 5, 'exactly the pre-#304 call sites: node search, observers, card health, stats, panel health');
  assert.ok(!/api\([^)]*activity|api\([^)]*previous/.test(homeJs), 'no separate activity request');
});

test('the evidence disclosure is keyboard and touch reachable, and does not navigate', () => {
  assert.ok(/<details class="mnc-evidence"/.test(homeJs), 'the evidence block must be a native disclosure');
  assert.ok(/<summary/.test(homeJs), 'and must have a summary');
  // The card is a role=button that opens health on click/Enter/Space. The
  // disclosure must be excluded from that, or opening it would navigate.
  assert.ok(/closest\('\.mnc-evidence'\)/.test(homeJs), 'the card activation handlers must ignore the disclosure');
  assert.strictEqual((homeJs.match(/closest\('\.mnc-evidence'\)/g) || []).length, 2, 'both the click and the keydown path');
});

test('home.css styles the new #304 chrome with theme variables only', () => {
  for (const sel of ['.mnc-evidence', '.mnc-evidence-row', '.mnc-metric-sub', '.my-node-card.healthy.evidence-relay']) {
    assert.ok(homeCss.includes(sel), 'home.css must style ' + sel);
  }
  const from = homeCss.indexOf('.mnc-evidence');
  const block = homeCss.slice(from, homeCss.indexOf('.mnc-spark {', from));
  const hex = block.replace(/\/\*[\s\S]*?\*\//g, '').match(/#[0-9a-fA-F]{3,8}\b/g);
  assert.strictEqual(hex, null, 'no hardcoded colors in the #304 block, got ' + JSON.stringify(hex));
  assert.match(block, /evidence-relay[\s\S]{0,160}var\(--status-yellow\)/, 'an attributed status must not keep the green accent');
});

console.log('\n' + passed + ' passed, ' + failed + ' failed');
process.exit(failed ? 1 : 0);
