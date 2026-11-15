/* test-issue-733-dhm-credit.js — DHM credit compliance (#733).
 *
 * Dataforsyningen's terms for Klimadatastyrelsen data under CC BY 4.0
 * ("Vilkår for brug af data", v3.1, August 2024,
 * https://dataforsyningen.dk/Vilkaar) require a credit to name the owner,
 * name the licence and link to it, link to the specific dataset, and state
 * whether the data was changed. internal/dhm owns the strings; the Go side
 * is pinned by internal/dhm/credit_733_test.go and
 * cmd/server/dhm_credit_733_test.go. This file covers the browser:
 *
 *   1. the map overlay credit (public/map-tile-providers.js): the dataset
 *      and licence marks inside the server's plain credit become anchors
 *      with rel="noopener", the rendered text is unchanged, and a block
 *      without the link fields — or with a target that is not absolute
 *      https — keeps the plain credit;
 *   2. nothing in a credit can become markup;
 *   3. the profile footer (public/terrain-profile.js): the same two links
 *      plus the "derived" note;
 *   4. the JS copy of the credit strings equals the Go one, so the two
 *      cannot drift;
 *   5. the credit strings live in exactly one public/ file, so the map
 *      credit really does come from the server.
 *
 * Runs via: node test-issue-733-dhm-credit.js
 */
'use strict';
const fs = require('fs');
const path = require('path');
const assert = require('assert');
const { makeSandbox, buildControl, makeElement, liveCredits, loadTerrainProfile } = require('./tests/map-overlay-sandbox');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

// ── The credit, as internal/dhm spells it ──────────────────────────────────

const GO_SRC = fs.readFileSync(path.join(__dirname, 'internal', 'dhm', 'dhm.go'), 'utf8');

function goConst(name) {
  const m = GO_SRC.match(new RegExp('^\\s*' + name + '\\s*=\\s*"((?:[^"\\\\]|\\\\.)*)"', 'm'));
  assert.ok(m, 'internal/dhm/dhm.go has no ' + name + ' string constant');
  return JSON.parse('"' + m[1] + '"');
}

const GO = {
  Owner: goConst('Owner'),
  DatasetName: goConst('DatasetName'),
  DatasetURL: goConst('DatasetURL'),
  LicenseName: goConst('LicenseName'),
  LicenseURL: goConst('LicenseURL'),
  DerivedNote: goConst('DerivedNote'),
};
// The two composed constants, pinned as expressions so a change to how Go
// joins the parts fails here too and not only in the Go tests.
const GO_ATTRIBUTION_EXPR = 'const Attribution = "Indeholder data fra " + Owner + ", " + DatasetName + ", " + LicenseName';
const GO_DERIVED_EXPR = 'const AttributionDerived = Attribution + " — " + DerivedNote';
const GO_ATTRIBUTION = 'Indeholder data fra ' + GO.Owner + ', ' + GO.DatasetName + ', ' + GO.LicenseName;
const GO_ATTRIBUTION_DERIVED = GO_ATTRIBUTION + ' — ' + GO.DerivedNote;

// The `dhm` block of /api/config/client, exactly as cmd/server sends it
// (cmd/server/dhm_proxy.go dhmClientConfig + dhm_credit_733_test.go).
const DHM_BLOCK = {
  enabled: true, url: '/api/map/dhm/wms', layer: 'dhm_terraen_skyggekort',
  minZoom: 7, maxZoom: 19, bounds: [[54.4265, 7.99125], [57.778, 15.5995]],
  attribution: GO_ATTRIBUTION,
  datasetName: GO.DatasetName, datasetUrl: GO.DatasetURL,
  licenseName: GO.LicenseName, licenseUrl: GO.LicenseURL,
};
// The pre-#733 block, still the shape test-issue-680-dhm-terrain.js drives:
// no link fields at all.
const PLAIN_BLOCK = (() => {
  const b = Object.assign({}, DHM_BLOCK);
  delete b.datasetName; delete b.datasetUrl; delete b.licenseName; delete b.licenseUrl;
  return b;
})();

// Serialised anchors, parsed back into something assertable. The credit is
// an HTML string because that is what Leaflet's attribution control takes.
function anchors(html) {
  const out = [];
  const re = /<a\s([^>]*)>([^<]*)<\/a>/g;
  let m;
  while ((m = re.exec(html)) !== null) {
    const attrs = {};
    const ar = /([a-zA-Z-]+)="([^"]*)"/g;
    let a;
    while ((a = ar.exec(m[1])) !== null) attrs[a[1]] = a[2];
    out.push({ attrs, text: m[2] });
  }
  return out;
}
function textOf(html) { return String(html).replace(/<[^>]*>/g, '').replace(/&amp;/g, '&').replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&quot;/g, '"'); }

function credit(ctx, cfg) {
  const layer = buildControl(ctx).ctl.overlays['Terrain (DHM hillshade)'];
  assert.ok(layer, 'no terrain overlay for ' + JSON.stringify(cfg));
  return layer.options.attribution;
}

console.log('\n=== #733 map overlay credit ===');

test('the hillshade credit links the dataset and the licence', () => {
  const html = credit(makeSandbox({ dhm: DHM_BLOCK }), DHM_BLOCK);
  const as = anchors(html);
  assert.strictEqual(as.length, 2, 'anchors: ' + html);
  assert.deepStrictEqual(as.map(a => a.text), [GO.DatasetName, GO.LicenseName]);
  assert.strictEqual(as[0].attrs.href, GO.DatasetURL);
  assert.strictEqual(as[1].attrs.href, GO.LicenseURL);
  as.forEach((a) => {
    assert.strictEqual(a.attrs.target, '_blank', 'target: ' + JSON.stringify(a.attrs));
    assert.ok(/\bnoopener\b/.test(a.attrs.rel), 'rel: ' + a.attrs.rel);
  });
  // The licence anchor also declares what it links to.
  assert.ok(/\blicense\b/.test(as[1].attrs.rel), 'licence rel: ' + as[1].attrs.rel);
});

test('the rendered credit text is the server\'s plain credit, unchanged', () => {
  const html = credit(makeSandbox({ dhm: DHM_BLOCK }), DHM_BLOCK);
  assert.strictEqual(textOf(html), DHM_BLOCK.attribution);
  // Leaflet counts, finds and removes a credit by its string, so the owner
  // must still be named in the text and the markup must not duplicate it.
  assert.strictEqual(textOf(html).split(GO.Owner).length - 1, 1);
});

test('the credit names the owner and does not claim the tiles were changed', () => {
  const html = credit(makeSandbox({ dhm: DHM_BLOCK }), DHM_BLOCK);
  assert.ok(textOf(html).indexOf(GO.Owner) >= 0, html);
  // The agency renders the hillshade; CoreScope shows it as served.
  assert.strictEqual(textOf(html).indexOf(GO.DerivedNote), -1, html);
});

test('a block without the link fields keeps the plain credit (pre-#733 server)', () => {
  const html = credit(makeSandbox({ dhm: PLAIN_BLOCK }), PLAIN_BLOCK);
  assert.strictEqual(html, PLAIN_BLOCK.attribution);
  assert.strictEqual(anchors(html).length, 0);
});

for (const [name, href] of [
  ['javascript:', 'javascript:alert(1)'],
  ['data:', 'data:text/html,<script>alert(1)</script>'],
  ['plain http', 'http://dataforsyningen.dk/data/930'],
  ['protocol-relative', '//evil.example/data/930'],
  ['same-origin path', '/data/930'],
  ['not a string', 930],
]) {
  test('a ' + name + ' dataset target is not linked at all', () => {
    const cfg = Object.assign({}, DHM_BLOCK, { datasetUrl: href });
    const html = credit(makeSandbox({ dhm: cfg }), cfg);
    const as = anchors(html);
    assert.strictEqual(as.length, 1, 'anchors: ' + html);
    assert.strictEqual(as[0].attrs.href, GO.LicenseURL);
    assert.strictEqual(textOf(html), DHM_BLOCK.attribution);
    assert.strictEqual(html.indexOf(String(href)), -1, html);
  });
}

test('a mark that is not in the credit text is skipped', () => {
  const cfg = Object.assign({}, DHM_BLOCK, { datasetName: 'Danmarks Dybdemodel' });
  const html = credit(makeSandbox({ dhm: cfg }), cfg);
  const as = anchors(html);
  assert.strictEqual(as.length, 1, 'anchors: ' + html);
  assert.strictEqual(as[0].text, GO.LicenseName);
  assert.strictEqual(textOf(html), DHM_BLOCK.attribution);
});

test('neither mark linkable → the plain credit, not an empty line', () => {
  const cfg = Object.assign({}, DHM_BLOCK, { datasetUrl: 'ftp://x/y', licenseUrl: 'ftp://x/z' });
  const html = credit(makeSandbox({ dhm: cfg }), cfg);
  assert.strictEqual(html, DHM_BLOCK.attribution);
});

test('nothing in a credit becomes markup', () => {
  const cfg = Object.assign({}, DHM_BLOCK, {
    attribution: 'Indeholder data fra <img src=x onerror=alert(1)>, ' + GO.DatasetName + ', ' + GO.LicenseName,
    datasetUrl: 'https://dataforsyningen.dk/data/930?a="><script>alert(1)</script>',
  });
  const html = credit(makeSandbox({ dhm: cfg }), cfg);
  // The only tags in the credit are the two anchors the builder made: the
  // <img> in the text and the <script> smuggled through the href both come
  // back as escaped characters, not elements.
  assert.deepStrictEqual((html.match(/<\/?([a-zA-Z][a-zA-Z0-9]*)/g) || []), ['<a', '</a', '<a', '</a'], html);
  assert.strictEqual(textOf(html), cfg.attribution);
  // The href cannot break out of its own quoted attribute.
  assert.strictEqual(anchors(html)[0].attrs.href.indexOf('"'), -1, html);
  assert.ok(html.indexOf('&quot;&gt;&lt;script&gt;') > 0, html);
});

test('Live counts and drops the linked credit as one string', () => {
  const ctx = makeSandbox({ dhm: DHM_BLOCK });
  const { map, ctl } = buildControl(ctx);
  const layer = ctl.overlays['Terrain (DHM hillshade)'];
  map.addLayer(layer);
  assert.deepStrictEqual(liveCredits(map), [layer.options.attribution]);
  assert.ok(anchors(liveCredits(map)[0]).length === 2, liveCredits(map)[0]);
  map.removeLayer(layer);
  assert.deepStrictEqual(liveCredits(map), []);
});

test('#723 monuments: no link fields, so the courtesy credit stays plain', () => {
  // Fund og Fortidsminder is free to use under the agency's own rules with
  // no credit requirement, and the register's textual data is CC0, which
  // waives attribution: nothing is owed, so nothing is linked.
  const HERITAGE = {
    enabled: true, url: 'https://www.kulturarv.dk/ffgeoserver/public/wms',
    layers: 'fundogfortidsminder_areal_beskyttelse,fundogfortidsminder_punkt_fredet',
    minZoom: 12, maxZoom: 19, bounds: [[54.4646, 8.0899], [57.7289, 15.5689]],
    attribution: '© Slots- og Kulturstyrelsen',
  };
  const ctx = makeSandbox({ heritage: HERITAGE });
  const { ctl } = buildControl(ctx);
  const layer = ctl.overlays['Protected ancient monuments (zoom 12+)'];
  assert.ok(layer, 'no heritage overlay: ' + Object.keys(ctl.overlays || {}));
  assert.strictEqual(layer.options.attribution, HERITAGE.attribution);
  assert.strictEqual(anchors(layer.options.attribution).length, 0);
});

// ── The profile footer ─────────────────────────────────────────────────────

// #738: one loader for terrain-profile.js, shared with test-issue-738-foreign-fill-ui.js.
const { TP, win: TP_WIN } = loadTerrainProfile();

// The dhm block with the derived credit cmd/server sends since #765.
const PROFILE_BLOCK = Object.assign({}, DHM_BLOCK, { profile: true, attributionDerived: GO_ATTRIBUTION_DERIVED });

function footerOf(cfg) {
  const p = makeElement('p');
  TP.footerCredit(p, cfg);
  return p;
}

console.log('\n=== #733 / #765 profile chart footer ===');

test('the footer is the server\'s derived credit, the profile being computed from DHM', () => {
  const p = footerOf(PROFILE_BLOCK);
  assert.strictEqual(p.textContent, GO_ATTRIBUTION_DERIVED);
  assert.ok(p.textContent.endsWith(GO.DerivedNote), p.textContent);
  // The owner is named exactly once, so the panel shows one credit.
  assert.strictEqual(p.textContent.split(GO.Owner).length - 1, 1, p.textContent);
});

test('the footer links the dataset and the licence, both with rel=noopener', () => {
  const p = footerOf(PROFILE_BLOCK);
  const as = anchors(p.innerHTML);
  assert.strictEqual(as.length, 2, p.innerHTML);
  assert.deepStrictEqual(as.map(a => a.text), [GO.DatasetName, GO.LicenseName]);
  assert.strictEqual(as[0].attrs.href, GO.DatasetURL);
  assert.strictEqual(as[1].attrs.href, GO.LicenseURL);
  assert.strictEqual(as[0].attrs['data-testid'], 'tp-credit-dataset');
  assert.strictEqual(as[1].attrs['data-testid'], 'tp-credit-link');
  as.forEach(a => {
    assert.strictEqual(a.attrs.target, '_blank');
    assert.ok(/\bnoopener\b/.test(a.attrs.rel), a.attrs.rel);
  });
  assert.ok(/\blicense\b/.test(as[1].attrs.rel), as[1].attrs.rel);
  // The panel's own link style.
  p.children.filter(c => c.tagName === 'A').forEach(a => assert.strictEqual(a.className, 'tp-credit-link'));
});

test('the footer follows the block, not a copy of its strings (#765)', () => {
  // Wording and targets the page has never seen: only a footer built from
  // the block can show them.
  const cfg = Object.assign({}, PROFILE_BLOCK, {
    attributionDerived: 'Contains data from the Danish Agency, Elevation Model DK, CC BY 4.0 — data processed',
    datasetName: 'Elevation Model DK', datasetUrl: 'https://example.test/dataset',
    licenseUrl: 'https://example.test/licence',
  });
  const p = footerOf(cfg);
  assert.strictEqual(p.textContent, cfg.attributionDerived);
  const as = anchors(p.innerHTML);
  assert.deepStrictEqual(as.map(a => [a.text, a.attrs.href]),
    [['Elevation Model DK', 'https://example.test/dataset'], ['CC BY 4.0', 'https://example.test/licence']]);
});

test('a block without the derived credit falls back to the plain one', () => {
  const p = footerOf(DHM_BLOCK);
  assert.strictEqual(p.textContent, GO_ATTRIBUTION);
  assert.strictEqual(anchors(p.innerHTML).length, 2, p.innerHTML);
});

test('a block without the link fields renders the plain text, unlinked', () => {
  const p = footerOf(PLAIN_BLOCK);
  assert.strictEqual(p.textContent, GO_ATTRIBUTION);
  assert.strictEqual(anchors(p.innerHTML).length, 0, p.innerHTML);
});

test('a dataset target that is not absolute https is not linked in the footer either', () => {
  const p = footerOf(Object.assign({}, PROFILE_BLOCK, { datasetUrl: 'javascript:alert(1)' }));
  const as = anchors(p.innerHTML);
  assert.deepStrictEqual(as.map(a => a.text), [GO.LicenseName], p.innerHTML);
  assert.strictEqual(p.textContent, GO_ATTRIBUTION_DERIVED);
});

test('nothing in the footer becomes markup', () => {
  const p = footerOf(Object.assign({}, PROFILE_BLOCK, {
    attributionDerived: '<b>fra</b> ' + GO.DatasetName + ' & "x"',
  }));
  assert.strictEqual(/<b>|&(?!amp;|quot;|lt;|gt;)/.test(p.innerHTML), false, p.innerHTML);
  assert.strictEqual(p.textContent, '<b>fra</b> ' + GO.DatasetName + ' & "x"');
});

test('without the walker the footer still shows the credit as plain text', () => {
  // map-tile-providers.js failed to load: the credit is not lost with it.
  const saved = [TP_WIN.MC_appendCredit, TP_WIN.MC_creditLinks];
  TP_WIN.MC_appendCredit = undefined; TP_WIN.MC_creditLinks = undefined;
  try {
    const p = footerOf(PROFILE_BLOCK);
    assert.strictEqual(p.textContent, GO_ATTRIBUTION_DERIVED);
    assert.strictEqual(anchors(p.innerHTML).length, 0);
  } finally { [TP_WIN.MC_appendCredit, TP_WIN.MC_creditLinks] = saved; }
});

// ── One walker ─────────────────────────────────────────────────────────────

console.log('\n=== #765 one credit walker ===');

test('the map credit and the footer are one walk: same text, same links', () => {
  const html = credit(makeSandbox({ dhm: DHM_BLOCK }), DHM_BLOCK);
  const p = footerOf(DHM_BLOCK);
  assert.strictEqual(p.textContent, textOf(html));
  assert.deepStrictEqual(anchors(p.innerHTML).map(a => [a.text, a.attrs.href, a.attrs.rel, a.attrs.target]),
    anchors(html).map(a => [a.text, a.attrs.href, a.attrs.rel, a.attrs.target]));
});

test('marks are consumed in text order: a list out of order links only the later mark', () => {
  // The contract _overlayCredit documents: each mark is looked for after
  // the previous anchor, so a list must follow the text.
  const p = makeElement('p');
  const links = TP_WIN.MC_creditLinks(DHM_BLOCK).slice().reverse();
  assert.strictEqual(TP_WIN.MC_appendCredit(p, GO_ATTRIBUTION, links), 1);
  assert.deepStrictEqual(anchors(p.innerHTML).map(a => a.text), [GO.LicenseName], p.innerHTML);
  assert.strictEqual(p.textContent, GO_ATTRIBUTION);
});

test('terrain-profile.js keeps no walker and no credit string of its own', () => {
  const src = fs.readFileSync(path.join(__dirname, 'public', 'terrain-profile.js'), 'utf8');
  assert.strictEqual(/function appendCredit\b/.test(src), false, 'a second credit walker is back');
  ['ATTRIBUTION', 'ATTRIBUTION_DERIVED', 'ATTRIBUTION_URL', 'DATASET_MARK', 'DATASET_URL', 'DERIVED_NOTE', 'CREDIT_LINKS', 'appendCredit']
    .forEach(k => assert.strictEqual(TP[k], undefined, 'TerrainProfile.' + k + ' is still exported'));
});

// ── One definition ─────────────────────────────────────────────────────────

console.log('\n=== #733 one credit definition ===');

test('the block this suite drives is the Go credit, joined as Go joins it', () => {
  assert.ok(GO_SRC.indexOf(GO_ATTRIBUTION_EXPR) >= 0, 'internal/dhm/dhm.go no longer joins Attribution from the parts');
  assert.ok(GO_SRC.indexOf(GO_DERIVED_EXPR) >= 0, 'internal/dhm/dhm.go no longer joins AttributionDerived from Attribution + DerivedNote');
  // cmd/server/dhm_credit_733_test.go pins that the server sends these.
  assert.strictEqual(PROFILE_BLOCK.attribution, GO_ATTRIBUTION);
  assert.strictEqual(PROFILE_BLOCK.attributionDerived, GO_ATTRIBUTION_DERIVED);
});

/* Comments may of course name the owner and cite the terms; a second
 * hard-coded copy of the strings is what must not exist. Lines that are
 * wholly a comment are dropped, and a trailing block comment is trimmed —
 * enough for this scan, and it never touches a line that assigns a string. */
function codeOnly(src) {
  let inBlock = false;
  return src.split('\n').filter((line) => {
    const t = line.trim();
    if (inBlock) { if (t.indexOf('*/') >= 0) inBlock = false; return false; }
    if (t.indexOf('/*') === 0) { if (t.indexOf('*/') < 0) inBlock = true; return false; }
    return t.indexOf('//') !== 0 && t.indexOf('*') !== 0;
  }).join('\n');
}

test('no public/ file carries the credit strings', () => {
  const files = fs.readdirSync(path.join(__dirname, 'public')).filter(f => f.endsWith('.js'));
  const carrying = files.filter(f => {
    const src = codeOnly(fs.readFileSync(path.join(__dirname, 'public', f), 'utf8'));
    // #772 F3: the licence URL and the dataset name are credit strings too
    // (a copy of either would let a public/ file drift from the server's
    // block the same way a copy of the owner would), so the guard covers
    // all five strings, not just three.
    return src.indexOf(GO.Owner) >= 0 || src.indexOf(GO.DatasetURL) >= 0 || src.indexOf(GO.DerivedNote) >= 0
      || src.indexOf(GO.LicenseURL) >= 0 || src.indexOf(GO.DatasetName) >= 0;
  });
  // The map credit and the profile footer both come from the server's dhm
  // block (#765), so no copy of the owner, the dataset link, the licence
  // link, the dataset name or the derived note belongs anywhere in public/.
  assert.deepStrictEqual(carrying, []);
});

test('the licence-URL and dataset-name check does not trip on OpenTopoMap\'s own licence (#772 F3)', () => {
  // OpenTopoMap's CC BY-SA 3.0 link (public/map-tile-providers.js) shares a
  // creativecommons.org/licenses prefix with GO.LicenseURL but a different
  // licence and version, and "OpenTopoMap" is not GO.DatasetName. Neither
  // is a substring of the other, so the stricter guard above must not flag
  // map-tile-providers.js on their account.
  const src = codeOnly(fs.readFileSync(path.join(__dirname, 'public', 'map-tile-providers.js'), 'utf8'));
  assert.ok(src.indexOf('creativecommons.org/licenses/by-sa/3.0') >= 0, 'setup: OpenTopoMap licence link missing');
  assert.strictEqual(src.indexOf(GO.LicenseURL), -1, 'OpenTopoMap\'s link false-positives as the DHM licence URL');
  assert.strictEqual(src.indexOf(GO.DatasetName), -1, 'map-tile-providers.js unexpectedly names the DHM dataset');
});

console.log('\n' + passed + ' passed, ' + failed + ' failed');
process.exit(failed ? 1 : 0);
