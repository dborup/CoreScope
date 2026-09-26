/* Unit tests for public/node-adverts.js — the Recent Adverts panel shared by
 * the node detail page and the side panel (port/extension of upstream
 * `Kpa-clawbot/CoreScope#2071` / `#2073`). Loads the real file in a vm context.
 * Run: node test-node-adverts.js
 */
'use strict';
const assert = require('assert');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const src = fs.readFileSync(path.join(__dirname, 'public', 'node-adverts.js'), 'utf8');
const ctx = { window: {}, console };
vm.createContext(ctx);
vm.runInContext(src, ctx, { filename: 'node-adverts.js' });
const NA = ctx.window.NodeAdverts;

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

function row(id, cls, extra) {
  return Object.assign({
    id: id, hash: 'h' + id, timestamp: '2026-09-25T10:00:0' + (id % 10) + 'Z', first_seen: '2026-09-25T10:00:00Z',
    payload_type: 4, route_type: cls === 'zero_hop' ? 2 : 1, route_class: cls,
    decoded_json: JSON.stringify({ type: 'ADVERT', name: 'Node ' + id }), observation_count: 1, observations: [],
  }, extra || {});
}
const counts = (h24, d7, extra) => Object.assign({ '24h': h24, '7d': d7, truncated: false, route_mask_backfill: { status: 'complete', remaining: 0 } }, extra || {});
const c = (flood, zh, mixed, unknown) => ({ flood: flood, zero_hop: zh, mixed: mixed, unknown: unknown || 0 });

function detail(over) {
  return Object.assign({
    recentAdverts: [row(5, 'zero_hop'), row(4, 'zero_hop'), row(3, 'mixed')],
    recentAdvertsByRoute: { limit: 20, flood: [row(1, 'flood'), row(0, 'flood')], zero_hop: [row(5, 'zero_hop'), row(4, 'zero_hop')], mixed: [row(3, 'mixed')] },
    advertCounts: counts(c(2, 94, 1), c(9, 600, 3)),
  }, over || {});
}
const opts = (over) => Object.assign({ variant: 'full', idPrefix: 'na', timestampHtml: (iso) => '<time>' + iso + '</time>' }, over || {});
// The markup of one tab panel: from its id to the next panel (or the end).
function panel(html, key) {
  const start = html.indexOf('id="na-panel-' + key + '"');
  assert.ok(start >= 0, 'panel ' + key + ' missing');
  const next = html.indexOf('id="na-panel-', start + 1);
  return html.slice(start, next < 0 ? html.length : next);
}

console.log('── node-adverts.js ──');

test('parseTab: reads ?adverts= and falls back to all', () => {
  assert.strictEqual(NA.parseTab('#/nodes/abc?adverts=flood'), 'flood');
  assert.strictEqual(NA.parseTab('#/nodes/abc?section=node-packets&adverts=zero_hop'), 'zero_hop');
  assert.strictEqual(NA.parseTab('#/nodes/abc?adverts=mixed'), 'mixed');
  assert.strictEqual(NA.parseTab('#/nodes/abc?adverts=unknown'), 'unknown');
  assert.strictEqual(NA.parseTab('#/nodes/abc?adverts=<script>'), 'all');
  assert.strictEqual(NA.parseTab('#/nodes/abc'), 'all');
  assert.strictEqual(NA.parseTab(''), 'all');
});

test('hashWithTab: sets, replaces and drops the param, keeps path and others', () => {
  assert.strictEqual(NA.hashWithTab('#/nodes/abc', 'flood'), '#/nodes/abc?adverts=flood');
  assert.strictEqual(NA.hashWithTab('#/nodes/abc?section=node-packets', 'mixed'), '#/nodes/abc?section=node-packets&adverts=mixed');
  assert.strictEqual(NA.hashWithTab('#/nodes/abc?adverts=flood&section=x', 'zero_hop'), '#/nodes/abc?adverts=zero_hop&section=x');
  assert.strictEqual(NA.hashWithTab('#/nodes/abc?adverts=flood', 'all'), '#/nodes/abc');
  assert.strictEqual(NA.hashWithTab('#/nodes/abc?section=x&adverts=flood', 'all'), '#/nodes/abc?section=x');
});

test('title is Recent Adverts with the from_pubkey tooltip', () => {
  const html = NA.render(detail(), opts());
  assert.ok(/<h4[^>]*title="[^"]*from_pubkey[^"]*"[^>]*>Recent Adverts \(3\)<\/h4>/.test(html), html.slice(0, 400));
  assert.ok(!html.includes('Recent Packets'));
});

test('counts: 24h and 7d rows per class, unknown hidden at zero', () => {
  const html = NA.render(detail(), opts());
  const text = html.replace(/<[^>]+>/g, ' ').replace(/\s+/g, ' ');
  assert.ok(text.includes('24h Flood 2 · Zero-hop 94 · Mixed 1'), text);
  assert.ok(text.includes('7d Flood 9 · Zero-hop 600 · Mixed 3'), text);
  assert.ok(!/Unknown \d/.test(text), 'unknown shown at zero');
  const withUnknown = NA.render(detail({ advertCounts: counts(c(0, 1, 0, 2), c(0, 1, 0, 5)) }), opts()).replace(/<[^>]+>/g, ' ').replace(/\s+/g, ' ');
  assert.ok(withUnknown.includes('24h Flood 0 · Zero-hop 1 · Mixed 0 · Unknown 2'), withUnknown);
});

test('tabs: All is the default, ARIA wired, deep-linked tab selected', () => {
  let html = NA.render(detail(), opts());
  const tabs = html.match(/<button[^>]*role="tab"[^>]*>/g) || [];
  assert.strictEqual(tabs.length, 4, 'All/Flood/Zero-hop/Mixed');
  assert.ok(/data-adverts-tab="all"[^>]*aria-selected="true"/.test(html) || /aria-selected="true"[^>]*data-adverts-tab="all"/.test(html));
  assert.ok(/aria-controls="na-panel-flood"/.test(html));
  assert.ok(/<div[^>]*class="node-adverts-tabs"[^>]*aria-label="[^"]+"/.test(html));
  assert.ok(!/id="na-panel-all"[^>]*hidden/.test(html), 'All panel visible');
  assert.ok(/id="na-panel-flood"[^>]*hidden/.test(html), 'Flood panel hidden');
  html = NA.render(detail(), opts({ tab: 'flood' }));
  assert.ok(/id="na-panel-flood"(?![^>]*hidden)[^>]*>/.test(html), 'Flood panel visible when deep-linked');
  assert.ok(/id="na-panel-all"[^>]*hidden/.test(html));
  assert.ok(/class="tab-btn active"[^>]*data-adverts-tab="flood"/.test(html));
});

test('panels: per-class lists come from recentAdvertsByRoute, All from recentAdverts', () => {
  const html = NA.render(detail(), opts());
  const flood = panel(html, 'flood');
  assert.ok(flood.includes('#/packets/h1') && flood.includes('#/packets/h0'));
  assert.ok(!flood.includes('#/packets/h5') && !flood.includes('#/packets/h3'));
  const all = panel(html, 'all');
  assert.ok(all.includes('#/packets/h5') && all.includes('#/packets/h3'));
  assert.ok(!all.includes('#/packets/h1'), 'All is the chronological list, not a merge of the class lists');
});

test('All shows a route badge per advert; class tabs do not repeat it', () => {
  const html = NA.render(detail(), opts());
  const all = panel(html, 'all');
  assert.strictEqual((all.match(/class="advert-route-badge"/g) || []).length, 3);
  assert.ok(/data-route-class="mixed"[^>]*>Mixed</.test(all));
  assert.strictEqual((panel(html, 'flood').match(/class="advert-route-badge"/g) || []).length, 0);
});

test('empty states explain themselves', () => {
  const html = NA.render(detail({ recentAdvertsByRoute: { limit: 20, flood: [], zero_hop: [row(5, 'zero_hop')], mixed: [] } }), opts());
  assert.ok(panel(html, 'flood').includes('No flood adverts from this node on record'));
  assert.ok(panel(html, 'mixed').includes('No mixed adverts from this node on record'));
  const none = NA.render(detail({ recentAdverts: [], recentAdvertsByRoute: { limit: 20, flood: [], zero_hop: [], mixed: [] } }), opts());
  assert.ok(panel(none, 'all').includes('No recent adverts'));
});

test('unknown tab only when there is data', () => {
  assert.ok(!NA.render(detail(), opts()).includes('data-adverts-tab="unknown"'));
  const html = NA.render(detail({ recentAdvertsByRoute: { limit: 20, flood: [], zero_hop: [], mixed: [], unknown: [row(7, 'unknown')] } }), opts());
  assert.ok(html.includes('data-adverts-tab="unknown"'));
  // A deep link to a tab that is not rendered falls back to All.
  const fallback = NA.render(detail(), opts({ tab: 'unknown' }));
  assert.ok(/id="na-panel-all"(?![^>]*hidden)[^>]*>/.test(fallback));
});

test('provisional note while the route_mask backfill is not complete', () => {
  assert.ok(!NA.render(detail(), opts()).includes('node-adverts-note'));
  const html = NA.render(detail({ advertCounts: counts(c(1, 1, 0), c(1, 1, 0), { route_mask_backfill: { status: 'backfilling', remaining: 12 } }) }), opts());
  assert.ok(/class="node-adverts-note[^"]*"[^>]*>[^<]*provisional/i.test(html), html);
});

test('older server or hidden identity: only the chronological list, no tabs', () => {
  const html = NA.render({ recentAdverts: [row(5, 'zero_hop')] }, opts());
  assert.ok(html.includes('Recent Adverts (1)'));
  assert.ok(!html.includes('role="tab"'));
  assert.ok(!html.includes('node-adverts-counts'));
  assert.ok(html.includes('#/packets/h5'));
});

test('escaping: node-controlled text never reaches the HTML raw', () => {
  const evil = row(9, 'flood', {
    hash: 'x"><img src=x onerror=alert(1)>',
    decoded_json: JSON.stringify({ name: '<script>alert(1)</script>' }),
    observer_name: '<b onmouseover=alert(2)>obs</b>',
    route_class: '"><svg onload=alert(3)>',
  });
  const html = NA.render(detail({ recentAdverts: [evil], recentAdvertsByRoute: { limit: 20, flood: [evil], zero_hop: [], mixed: [] } }), opts());
  assert.ok(!html.includes('<script>'), 'decoded name escaped');
  assert.ok(!html.includes('<b onmouseover'), 'observer escaped');
  assert.ok(!html.includes('<img src=x'), 'hash escaped in href');
  assert.ok(!html.includes('<svg onload'), 'route_class escaped');
  assert.ok(html.includes('&lt;script&gt;'));
});

test('existing badges and signal readouts are kept', () => {
  const r = row(8, 'flood', { observation_count: 3, snr: 7.5, rssi: -91, observer_name: 'ObsA', raw_hex: '11c1' });
  const d = detail({ recentAdverts: [r] });
  const full = panel(NA.render(d, opts({ hashSizeInconsistent: true })), 'all');
  assert.ok(full.includes('badge-obs') && full.includes('SNR 7.5dB') && full.includes('RSSI -91dBm') && full.includes('via ObsA'));
  assert.ok(/class="badge advert-hs-badge hs-\d"/.test(full), 'hash-size badge kept on the full page');
  assert.ok(full.includes('node-activity-item'));
  const pane = NA.render(d, opts({ variant: 'pane', roleColor: 'var(--role-repeater)' }));
  assert.ok(pane.includes('advert-entry') && pane.includes('advert-dot') && pane.includes('badge-obs'));
});

// PR #97 review P3: every node-controlled or caller-supplied value that
// reaches the markup is escaped - one test per value, so dropping a single
// esc() fails it.
function evilRender(extra, o) {
  const r = row(9, 'flood', extra);
  return NA.render(detail({ recentAdverts: [r], recentAdvertsByRoute: { limit: 20, flood: [r], zero_hop: [], mixed: [] } }), opts(o));
}

test('escaping: decoded text', () => {
  const html = evilRender({ decoded_json: JSON.stringify({ text: '<img src=x onerror=alert(4)>' }) });
  assert.ok(!html.includes('<img src=x'), 'decoded.text reached the HTML raw');
  assert.ok(html.includes(': &lt;img src=x onerror=alert(4)&gt;'), html);
});

test('escaping: SNR and RSSI', () => {
  const html = evilRender({ snr: '<b id=snr>1</b>', rssi: '<b id=rssi>2</b>' });
  assert.ok(!html.includes('<b id=snr>'), 'snr reached the HTML raw');
  assert.ok(!html.includes('<b id=rssi>'), 'rssi reached the HTML raw');
  assert.ok(html.includes('SNR &lt;b id=snr&gt;1&lt;/b&gt;dB') && html.includes('RSSI &lt;b id=rssi&gt;2&lt;/b&gt;dBm'), html);
});

test('escaping: role colour in the pane dot', () => {
  const html = evilRender({}, { variant: 'pane', roleColor: 'red"><script>alert(5)</script>' });
  assert.ok(!html.includes('<script>alert(5)'), 'roleColor reached the HTML raw');
  assert.ok(html.includes('style="background:red&quot;&gt;&lt;script&gt;alert(5)&lt;/script&gt;"'), html);
});

test('escaping: timestamp without a timestampHtml formatter', () => {
  const html = evilRender({ timestamp: '<i id=ts>2026</i>' }, { timestampHtml: null });
  assert.ok(!html.includes('<i id=ts>'), 'timestamp fallback reached the HTML raw');
  assert.ok(html.includes('&lt;i id=ts&gt;2026&lt;/i&gt;'), html);
});

test('tabs: exactly the selected tab is aria-selected at render, the rest false and out of the tab order', () => {
  for (const tab of ['all', 'flood', 'zero_hop', 'mixed']) {
    const html = NA.render(detail(), opts({ tab: tab }));
    const tabs = html.match(/<button[^>]*role="tab"[^>]*>/g) || [];
    assert.strictEqual(tabs.length, 4);
    for (const b of tabs) {
      const key = /data-adverts-tab="([^"]+)"/.exec(b)[1];
      const on = key === tab;
      assert.ok(b.includes('aria-selected="' + on + '"'), tab + ': ' + b);
      assert.ok(b.includes('tabindex="' + (on ? '0' : '-1') + '"'), tab + ': ' + b);
    }
  }
});

// PR #97 review P2-2: the breakdown costs a scan of the node's adverts, so
// only the node page (full view and side panel, both through nodes.js
// fetchNodeDetail) asks for it. Packets, live, channels, route view and the
// claimed-nodes lookups keep the plain /api/nodes/{pubkey} URL.
test('detailPath: the opt-in URL, a distinct client-cache key under the plain one', () => {
  assert.strictEqual(NA.detailPath('ab/c d'), '/nodes/ab%2Fc%20d?include=advertRoutes');
  const plain = '/nodes/' + encodeURIComponent('abc');
  assert.notStrictEqual(NA.detailPath('abc'), plain, 'api() caches by path: the two responses must not share a key');
  assert.ok(NA.detailPath('abc').startsWith(plain + '?'), "invalidateApiCache('/nodes/' + pk) must still clear it");
});

test('only the node page sends include=advertRoutes', () => {
  const dir = path.join(__dirname, 'public');
  const offenders = [];
  for (const f of fs.readdirSync(dir).filter((f) => f.endsWith('.js'))) {
    if (f === 'node-adverts.js') continue;
    const code = fs.readFileSync(path.join(dir, f), 'utf8');
    if (f !== 'nodes.js' && /advertRoutes|detailPath/.test(code)) offenders.push(f);
  }
  assert.deepStrictEqual(offenders, [], 'files other than nodes.js ask for the breakdown');
  const nodesJs = fs.readFileSync(path.join(dir, 'nodes.js'), 'utf8');
  assert.ok(!/advertRoutes/.test(nodesJs), 'nodes.js must go through NodeAdverts.detailPath');
  const uses = nodesJs.match(/NodeAdverts\.detailPath\(/g) || [];
  assert.strictEqual(uses.length, 1, 'exactly one node-detail fetch sends it');
  const fetchFn = /async function fetchNodeDetail\(pubkey\) \{[\s\S]*?\n  \}\n/.exec(nodesJs);
  assert.ok(fetchFn, 'fetchNodeDetail not found');
  assert.ok(/api\(NodeAdverts\.detailPath\(pubkey\), \{ ttl: CLIENT_TTL\.nodeDetail \}\)/.test(fetchFn[0]), 'fetchNodeDetail (full view + side panel) must send it');
  assert.ok(/async function selectNode[\s\S]*?fetchNodeDetail\(/.test(nodesJs) && /async function loadFullNode[\s\S]*?fetchNodeDetail\(/.test(nodesJs),
    'the side panel and the full view both fetch through fetchNodeDetail');
  // The claimed-nodes lookup stays on the plain URL.
  assert.ok(/missing\.map\(mn => api\('\/nodes\/' \+ encodeURIComponent\(mn\.pubkey\), \{ ttl: CLIENT_TTL\.nodeDetail \}\)\)/.test(nodesJs), 'claimed nodes changed');
});

console.log('\n' + passed + ' passed, ' + failed + ' failed');
if (failed) process.exit(1);
