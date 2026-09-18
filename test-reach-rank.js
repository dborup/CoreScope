'use strict';
// Unit test for reach-rank.js (Reach leaderboard page). Loads the browser IIFE
// in a vm sandbox (pattern from test-node-reach-coverage.js) with the real
// escapeHtml from app.js, and exercises the pure render helpers: escaping of
// node-controlled fields, the pubkey fallback, global ranks in the status
// line, the snapshot-age text and hash-state parsing.
const assert = require('assert');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const appSrc = fs.readFileSync(path.join(__dirname, 'public', 'app.js'), 'utf8');
const escSrc = appSrc.match(/function escapeHtml\(s\) \{[\s\S]*?\n\}/);
assert.ok(escSrc, 'escapeHtml not found in public/app.js');

const registered = {};
const sandbox = {
  window: {},
  document: {},
  registerPage: function (name, mod) { registered[name] = mod; },
  timeAgo: function () { return '2m ago'; },
  URLSearchParams: URLSearchParams,
};
vm.createContext(sandbox);
vm.runInContext(escSrc[0], sandbox);
vm.runInContext(fs.readFileSync(path.join(__dirname, 'public', 'reach-rank.js'), 'utf8'), sandbox);

assert.ok(registered['reach-rank'] && typeof registered['reach-rank'].init === 'function', 'page reach-rank registered');
const { rowHtml, statusText, snapshotText, parseState } = sandbox.window.ReachRank;

const PK = 'a1b2c3d4e5f6' + '0'.repeat(52);

// --- rowHtml: link, rank, neighbours --------------------------------------
{
  const html = rowHtml({ rank: 3, pubkey: PK, name: 'Aarhus Hub', neighbors: 42 });
  assert.ok(html.includes('href="#/nodes/' + PK + '/reach"'), 'links to the node Reach page: ' + html);
  assert.ok(html.includes('>#3<'), 'global rank rendered: ' + html);
  assert.ok(html.includes('>42<'), 'neighbour count rendered');
  assert.ok(html.includes('>Aarhus Hub</a>'), 'name is the link text');
  assert.ok(html.includes('>a1b2c3d4<'), 'short pubkey shown next to a named node');
  assert.ok(html.includes('title="' + PK + '"'), 'full pubkey in the title');
}

// --- rowHtml: pubkey fallback for a node without a name ------------------------
{
  const html = rowHtml({ rank: 1, pubkey: PK, name: '', neighbors: 7 });
  assert.ok(html.includes('>a1b2c3d4e5f6</a>'), 'unnamed node falls back to its pubkey: ' + html);
  assert.ok(!html.includes('rr-pk'), 'no duplicate pubkey span for the fallback');
}

// --- rowHtml: XSS — every node-controlled field is escaped ----------------------
{
  const evil = '<img src=x onerror="alert(1)">\'"&';
  const html = rowHtml({ rank: '<b>1</b>', pubkey: '"><script>x()</script>', name: evil, neighbors: '<i>9</i>' });
  assert.ok(!/<img|<script|<b>|<i>/i.test(html), 'no raw markup survives: ' + html);
  assert.ok(html.includes('&lt;img src=x onerror=&quot;alert(1)&quot;&gt;&#39;&quot;&amp;'), 'name escaped verbatim');
  assert.ok(html.includes('href="#/nodes/%22%3E%3Cscript%3Ex()%3C%2Fscript%3E/reach"'), 'pubkey URL-encoded in href: ' + html);
  assert.ok(!html.includes('title=""'), 'title attribute not broken out of');
}

// --- statusText: global placements, no renumbering in search -----------------------
assert.strictEqual(statusText({ q: '', total: 212, matched: 212, offset: 50, rows: new Array(50) }),
  'Showing 51–100 of 212 ranked nodes.');
assert.strictEqual(statusText({ q: 'hub', total: 212, matched: 3, offset: 0, rows: new Array(3) }),
  'Showing 1–3 of 3 matches for “hub” (212 ranked nodes).');
assert.strictEqual(statusText({ q: 'x', total: 212, matched: 1, offset: 0, rows: new Array(1) }),
  'Showing 1–1 of 1 match for “x” (212 ranked nodes).');
assert.strictEqual(statusText({ q: 'zzz', total: 212, matched: 0, offset: 0, rows: [] }),
  'No ranked node matches “zzz” (212 ranked nodes).');
assert.strictEqual(statusText({ q: '', total: 0, matched: 0, offset: 0, rows: [] }), 'No ranked nodes yet.');

// --- snapshotText: states the data age; tolerates bad input -------------------------
assert.ok(/^Snapshot .+ \(2m ago\)\.$/.test(snapshotText('2026-09-18T12:00:00Z')), snapshotText('2026-09-18T12:00:00Z'));
assert.strictEqual(snapshotText(''), 'Snapshot time unknown.');
assert.strictEqual(snapshotText('not a date'), 'Snapshot time unknown.');

// --- parseState: hash ?q=&page= ------------------------------------------------------
{
  let s = parseState(new URLSearchParams('q=%20Hub%20&page=3'));
  assert.strictEqual(s.q, 'Hub');
  assert.strictEqual(s.page, 3);
  s = parseState(new URLSearchParams('page=-2'));
  assert.strictEqual(s.page, 1, 'negative page clamps to 1');
  s = parseState(new URLSearchParams('page=abc&q=' + 'a'.repeat(100)));
  assert.strictEqual(s.page, 1, 'garbage page → 1');
  assert.strictEqual(s.q.length, 64, 'query capped at the server limit');
  s = parseState(new URLSearchParams(''));
  assert.deepStrictEqual({ q: s.q, page: s.page }, { q: '', page: 1 });
}

// --- node-reach.js wires the Rank card to the leaderboard -----------------------------
{
  const reachSrc = fs.readFileSync(path.join(__dirname, 'public', 'node-reach.js'), 'utf8');
  assert.ok(reachSrc.includes('href="#/reach-rank"'), 'Reach Rank card links to #/reach-rank');
  assert.ok(/rank_status === 'ranked'/.test(reachSrc), 'Rank card reads rank_status (no "#0 / N" for unranked)');
  const html = fs.readFileSync(path.join(__dirname, 'public', 'index.html'), 'utf8');
  assert.ok(html.includes('<script src="reach-rank.js?v=__BUST__"'), 'index.html loads reach-rank.js with the cache-buster placeholder');
}

console.log('reach-rank.js helpers OK (escaping, fallback, status, snapshot, state)');
