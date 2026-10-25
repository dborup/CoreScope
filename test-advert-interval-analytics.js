/* Unit tests for public/advert-interval-analytics.js — the Analytics →
 * Advert Intervals tab (#245 M2), rendered from
 * GET /api/analytics/advert-intervals (docs/api-spec.md). Loads the real
 * files in a vm context (node-adverts.js supplies the interval formatter).
 * Run: node test-advert-interval-analytics.js
 */
'use strict';
const assert = require('assert');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const ctx = { window: {}, console };
vm.createContext(ctx);
for (const f of ['node-adverts.js', 'advert-interval-analytics.js']) {
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'public', f), 'utf8'), ctx, { filename: f });
}
const AI = ctx.window.AdvertIntervalAnalytics;
const SRC = fs.readFileSync(path.join(__dirname, 'public', 'advert-interval-analytics.js'), 'utf8');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

function summary(over) {
  return Object.assign({ estimated: 0, low_confidence: 0, irregular: 0, too_few: 0, not_observed: 0, buckets: [] }, over || {});
}
function data(over) {
  return Object.assign({
    window: 20, min_confidence: 'medium', nodes: 7,
    flood: summary({ estimated: 5, low_confidence: 1, too_few: 1, buckets: [
      { interval_s: 43200, snapped: true, nodes: 1 }, { interval_s: 169200, snapped: true, nodes: 3 }, { interval_s: 1080000, snapped: false, nodes: 1 }] }),
    zero_hop: summary({ estimated: 3, not_observed: 4, buckets: [
      { interval_s: 120, snapped: true, nodes: 1 }, { interval_s: 7200, snapped: true, nodes: 2 }] }),
    by_role: [
      { key: 'repeater', nodes: 5, flood: summary({ estimated: 4, buckets: [{ interval_s: 43200, snapped: true, nodes: 1 }, { interval_s: 169200, snapped: true, nodes: 3 }] }), zero_hop: summary({ estimated: 3, not_observed: 2, buckets: [{ interval_s: 7200, snapped: true, nodes: 3 }] }) },
      { key: 'companion', nodes: 2, flood: summary({ not_observed: 2 }), zero_hop: summary({ not_observed: 2 }) },
    ],
    by_region: [
      { key: 'CPH', nodes: 4, flood: summary({ estimated: 4, buckets: [{ interval_s: 169200, snapped: true, nodes: 4 }] }), zero_hop: summary({ not_observed: 4 }) },
      { key: 'unknown', nodes: 3, flood: summary(), zero_hop: summary() },
    ],
    regions_truncated: false,
    outliers: [
      { public_key: 'ab<cd', name: '<img src=x onerror=alert(1)>', role: 'repeater', regions: ['CPH'], class: 'zero_hop', reason: 'zero_hop_new_install_default', median_gap_s: 120, samples: 20, interval_s: 120 },
      { public_key: 'ef01', name: '', role: 'companion', regions: [], class: 'flood', reason: 'flood_faster_than_settable', median_gap_s: 600, samples: 9, interval_s: null },
    ],
    outliers_total: 2,
    flood_re_arm: { zero_hop_paced_by_flood: 2, zero_hop_maybe_suppressed: 1 },
    scan: { generated_at: '2026-10-25T08:00:00Z', rows: 1234, truncated: false },
  }, over || {});
}
function text(html) { return html.replace(/<[^>]+>/g, ' ').replace(/&amp;/g, '&').replace(/\s+/g, ' ').trim(); }
function section(html, attr) {
  const i = html.indexOf(attr);
  assert.ok(i >= 0, attr + ' missing');
  const end = html.indexOf('data-ai-section=', i + attr.length);
  return html.slice(i, end < 0 ? html.length : end);
}

console.log('── advert-interval-analytics.js ──');

test('summary cards: nodes, estimated per class, zero-hop not observed worded as such', () => {
  const t = text(section(AI.render(data()), 'data-ai-section="summary"'));
  assert.ok(/7 Nodes with adverts/.test(t), t);
  assert.ok(/5 Flood interval estimated/.test(t), t);
  assert.ok(/3 Zero-hop interval estimated/.test(t), t);
  assert.ok(/4 Zero-hop not observed off, or no observer in direct range/.test(t), t);
  assert.ok(/2 Outliers/.test(t), t);
});

test('distributions: firmware units, bars relative to the largest bucket, unsnapped marked', () => {
  const html = AI.render(data());
  const flood = section(html, 'data-ai-section="dist-flood"');
  assert.ok(/data-interval="169200"[\s\S]*?47 h[\s\S]*?width:100%[\s\S]*?>3</.test(flood), flood);
  assert.ok(/12 h[\s\S]*?width:33%/.test(flood), 'one third of the largest: ' + flood);
  assert.ok(/300 h[\s\S]*?outside the settable range/.test(flood), 'unsnapped flagged: ' + flood);
  const zh = section(html, 'data-ai-section="dist-zero_hop"');
  assert.ok(/2 min/.test(zh) && /120 min/.test(zh), zh);
  // The counts beside the distribution: every node in exactly one state.
  assert.ok(/Estimated 5 · low confidence 1 · irregular 0 · too few adverts 1 · not observed 0/.test(text(flood)), text(flood));
  assert.ok(/not observed \(off, or no observer in direct range\) 4/.test(text(zh)), text(zh));
});

test('empty distribution says so', () => {
  const html = AI.render(data({ zero_hop: summary({ not_observed: 7 }) }));
  assert.ok(/No medium or high confidence estimates/.test(text(section(html, 'data-ai-section="dist-zero_hop"'))));
});

test('by role and by region: top values per class and the not-observed count', () => {
  const html = AI.render(data());
  const role = text(section(html, 'data-ai-section="by-role"'));
  assert.ok(/repeater 5 47 h × 3 · 12 h × 1 120 min × 3 2/.test(role), role);
  assert.ok(/companion 2 — — 2/.test(role), role);
  const region = text(section(html, 'data-ai-section="by-region"'));
  assert.ok(/CPH 4 47 h × 4 — 4/.test(region), region);
  assert.ok(/No observer region 3/.test(region), region);
  assert.ok(!/50 largest/.test(region), 'no truncation note');
  assert.ok(/50 largest regions/.test(text(AI.render(data({ regions_truncated: true })))));
});

test('topBuckets: most nodes first, ties by the shorter interval, capped', () => {
  const b = AI.topBuckets([{ interval_s: 7200, nodes: 2 }, { interval_s: 3600, nodes: 2 }, { interval_s: 120, nodes: 5 }, { interval_s: 9000, nodes: 1 }], 3);
  assert.deepStrictEqual(Array.from(b, (x) => x.interval_s), [120, 3600, 7200]);
  assert.strictEqual(AI.topBuckets(null, 3).length, 0);
});

test('outliers: escaped names, node links, reasons in words, gap in the class unit', () => {
  const html = AI.render(data());
  const out = section(html, 'data-ai-section="outliers"');
  assert.ok(!/<img/.test(out), 'name must be escaped: ' + out);
  assert.ok(/&lt;img src=x onerror=alert\(1\)&gt;/.test(out), out);
  assert.ok(/href="#\/nodes\/ab%3Ccd"/.test(out), 'pubkey encoded in the link: ' + out);
  const t = text(out);
  assert.ok(/zero-hop every 2 min: the new-install default/.test(t), t);
  assert.ok(/ef01/.test(t) && /flood more often than every 3 h/.test(t) && /10 min/.test(t), t);
  assert.ok(!/showing/.test(t), t);
  assert.ok(/showing 2 of 60/.test(text(AI.render(data({ outliers_total: 60 })))));
  assert.ok(/No outliers/.test(text(section(AI.render(data({ outliers: [], outliers_total: 0 })), 'data-ai-section="outliers"'))));
});

test('flood re-arm notes cite the firmware behaviour', () => {
  const t = text(section(AI.render(data()), 'data-ai-section="re-arm"'));
  assert.ok(/Zero-hop paced by flood 2/.test(t) && /Zero-hop maybe suppressed by flood 1/.test(t), t);
  assert.ok(/re-arms the zero-hop timer/.test(t), t);
});

test('method note: window, confidence, firmware values, snapshot time, truncation', () => {
  let t = text(section(AI.render(data()), 'data-ai-section="method"'));
  assert.ok(/newest 20 flood and 20 zero-hop adverts/.test(t) && /medium and high confidence/.test(t), t);
  assert.ok(/3–168 h/.test(t) && /60–240 min/.test(t) && /2 min on an untouched new install/.test(t), t);
  assert.ok(/2026-10-25T08:00:00Z/.test(t) && !/newest 1,?234 adverts only/.test(t), t);
  t = text(AI.render(data({ scan: { generated_at: 'x', rows: 1000000, truncated: true } })));
  assert.ok(/newest 1000000 adverts only/.test(t), t);
});

test('no nodes: an empty-state message, nothing else', () => {
  const html = AI.render(data({ nodes: 0 }));
  assert.ok(/No nodes with adverts/.test(text(html)) && !/data-ai-section="outliers"/.test(html));
  assert.ok(/No nodes with adverts/.test(text(AI.render(null))));
});

test('theming: colours only through CSS variables', () => {
  const code = SRC.replace(/\/\*[\s\S]*?\*\//g, '').replace(/^\s*\/\/.*$/gm, '');
  assert.ok(!/#[0-9a-fA-F]{3,8}\b/.test(code), 'hex colour in the module');
  assert.ok(!/rgba?\(|hsla?\(/.test(SRC), 'rgb/hsl colour in the module');
  const css = fs.readFileSync(path.join(__dirname, 'public', 'style.css'), 'utf8');
  const block = css.slice(css.indexOf('/* #245 M2'), css.indexOf('/* end #245 M2 */'));
  assert.ok(block.length > 0, 'style.css block missing');
  assert.ok(!/#[0-9a-fA-F]{3,8}\b|rgba?\(/.test(block.replace(/\/\*[\s\S]*?\*\//g, '')), 'hardcoded colour in the CSS block: ' + block);
  assert.ok(/var\(--accent\)/.test(block), 'bars use the theme accent');
});

console.log(`\n${passed} passed, ${failed} failed`);
process.exit(failed ? 1 : 0);
