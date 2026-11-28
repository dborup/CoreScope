/**
 * #416 — named region quick-pick groups in the shared region filter.
 *
 *   - RegionFilter.getGroups() reports the groups the server serves, filtered
 *     to codes the region list actually offers. No `regionGroups` config, a
 *     failing or non-array response, or a group whose codes are all unknown:
 *     no quick picks, and the control renders exactly as it did before.
 *   - A quick pick selects that group's codes in one action, through the same
 *     path as a click on a region pill: saved to localStorage, re-rendered,
 *     and announced to listeners ONCE.
 *   - Picking the group that is already selected is a no-op: nothing is
 *     saved, nothing is announced, so no page refetches.
 *   - A group covering every known code selects all regions (null), the same
 *     normalization a pill click applies, and the "All" control is then the
 *     one that reads as active.
 *   - Clicking "All" while all regions are already selected no longer
 *     announces a change, so clearing filters cannot double-fetch.
 *   - The quick picks are real buttons with aria-pressed, and their labels
 *     and codes are escaped for their HTML slots (#333).
 *
 * Usage: node test-issue-416-region-quick-picks.js
 */
'use strict';
const assert = require('assert');
const fs = require('fs');
const vm = require('vm');

let passed = 0, failed = 0;
const queue = [];
function test(name, fn) { queue.push({ name, fn }); }
function section(title) { queue.push({ title }); }

const REGIONS = { SJC: 'San Jose', SFO: 'San Francisco', OAK: 'Oakland', MRY: 'Monterey' };
const GROUPS = [
  { name: 'Bay Area', codes: ['SJC', 'SFO', 'OAK'] },
  { name: 'South Bay', codes: ['SJC'] },
];

// The real public/region-filter.js in a sandbox. `groups` is what
// /api/config/regions/groups answers (default: GROUPS); `regions` what
// /api/config/regions answers; `stored` pre-seeds the saved selection.
function load(opts) {
  opts = opts || {};
  const store = {};
  if (opts.stored) store['meshcore-region-filter'] = JSON.stringify(opts.stored);
  const regions = opts.regions || REGIONS;
  const groups = 'groups' in opts ? opts.groups : GROUPS;
  const fetched = [];
  const replaced = [];
  const ctx = {
    window: {},
    addEventListener() {}, removeEventListener() {}, // #620 watchMenu: ctx.window = ctx
    document: { addEventListener() {}, removeEventListener() {} },
    location: { hash: opts.hash || '' },
    history: {
      state: null,
      replaceState(state, title, url) { replaced.push(url); ctx.location.hash = url; },
      pushState() { throw new Error('pushState must not be used'); },
    },
    URLSearchParams,
    localStorage: {
      getItem: (k) => (k in store ? store[k] : null),
      setItem: (k, v) => { store[k] = String(v); },
      removeItem: (k) => { delete store[k]; },
    },
    fetch: async (url) => {
      fetched.push(url);
      if (String(url).indexOf('/groups') >= 0) {
        if (groups instanceof Error) throw groups;
        return { json: async () => groups };
      }
      return { json: async () => regions };
    },
    escapeHtml: (s) => String(s)
      .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;').replace(/'/g, '&#39;'),
    console, setTimeout, clearTimeout,
  };
  ctx.window = ctx;
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync(__dirname + '/public/region-filter.js', 'utf8'), ctx, { filename: 'region-filter.js' });
  vm.runInContext(fs.readFileSync(__dirname + '/public/url-state.js', 'utf8'), ctx, { filename: 'url-state.js' });
  const RF = ctx.RegionFilter;
  const calls = [];
  const hashAtNotify = [];
  RF.onChange((sel) => {
    calls.push(sel ? Array.from(sel) : null);
    hashAtNotify.push(ctx.location.hash);
  });
  return { RF, store, calls, hashAtNotify, container: makeContainer(), ctx, fetched, replaced };
}

// A container stand-in: innerHTML is a plain string, and the handlers the
// component wires are invoked through click()/pick() below.
function makeContainer() {
  const classes = new Set();
  return {
    innerHTML: '', style: {}, onclick: null,
    classList: {
      add: (c) => classes.add(c), remove: (c) => classes.delete(c),
      contains: (c) => classes.has(c),
      toggle: (c, on) => { if (on) classes.add(c); else classes.delete(c); return classes.has(c); },
    },
    querySelector: () => ({ setAttribute() {}, onclick: null, onchange: null, hidden: true }),
  };
}

// A click on the element matching `attrs`, dispatched the way the browser
// would: the delegated container handler resolves it with closest().
function fire(container, attrs) {
  container.onclick({
    target: {
      closest: (sel) => {
        const key = sel.replace(/[[\]]/g, '');
        return key in attrs ? { dataset: datasetFor(attrs) } : null;
      },
    },
  });
}
function datasetFor(attrs) {
  const d = {};
  if ('data-region' in attrs) d.region = attrs['data-region'];
  if ('data-region-group' in attrs) d.regionGroup = attrs['data-region-group'];
  return d;
}
const clickRegion = (c, code) => fire(c, { 'data-region': code });
const pickGroup = (c, idx) => fire(c, { 'data-region-group': String(idx) });
const saved = (store) => (store['meshcore-region-filter'] ? JSON.parse(store['meshcore-region-filter']) : null);
const groupButtons = (html) => (html.match(/data-region-group="\d+"/g) || []).length;
// The component runs in a vm context, so its arrays and objects have that
// realm's prototypes. Round-tripping through JSON (and Array.from for the
// selection Set) gives deepStrictEqual values it can compare.
const plain = (v) => JSON.parse(JSON.stringify(v === undefined ? null : v));
const sel = (RF) => { const s = RF.getSelected(); return s ? Array.from(s) : null; };
const requireGroups = (RF) => {
  if (typeof RF.getGroups !== 'function') throw new Error('RegionFilter.getGroups is missing');
  return plain(RF.getGroups());
};

section('\n=== #416 group config: what the control accepts ===');

test('no groups configured: no quick picks, control unchanged', async () => {
  const t = load({ groups: [] });
  await t.RF.init(t.container);
  assert.deepStrictEqual(requireGroups(t.RF), []);
  assert.strictEqual(groupButtons(t.container.innerHTML), 0);
  assert.ok(/data-region="__all__"/.test(t.container.innerHTML), 'the region control still renders');
  assert.strictEqual(t.container.classList.contains('has-region-quick-picks'), false);
});

test('a failing groups request leaves the control working', async () => {
  const t = load({ groups: new Error('boom') });
  await t.RF.init(t.container);
  assert.deepStrictEqual(requireGroups(t.RF), []);
  assert.ok(/data-region="__all__"/.test(t.container.innerHTML));
  assert.strictEqual(t.RF.getSelected(), null);
});

test('a non-array groups response is ignored', async () => {
  for (const body of [null, {}, 'SJC', 42, REGIONS]) {
    const t = load({ groups: body });
    await t.RF.init(t.container);
    assert.deepStrictEqual(requireGroups(t.RF), [], JSON.stringify(body));
  }
});

test('malformed entries are dropped, usable ones kept', async () => {
  const t = load({ groups: [
    null,
    { codes: ['SJC'] },
    { name: 'No codes', codes: [] },
    { name: 'Codes not a list', codes: 'SJC' },
    { name: 'Bay Area', codes: ['SJC', 'SFO'] },
  ] });
  await t.RF.init(t.container);
  assert.deepStrictEqual(requireGroups(t.RF).map((g) => g.name), ['Bay Area']);
});

test('codes the region list does not offer are dropped', async () => {
  const t = load({ groups: [{ name: 'Bay Area', codes: ['SJC', 'XXX', 'OAK'] }] });
  await t.RF.init(t.container);
  assert.deepStrictEqual(requireGroups(t.RF)[0].codes, ['SJC', 'OAK']);
});

test('a group with no known code is not offered', async () => {
  const t = load({ groups: [{ name: 'Nowhere', codes: ['XXX'] }, { name: 'South Bay', codes: ['SJC'] }] });
  await t.RF.init(t.container);
  assert.deepStrictEqual(requireGroups(t.RF).map((g) => g.name), ['South Bay']);
  assert.strictEqual(groupButtons(t.container.innerHTML), 1);
});

test('codes match case-insensitively and take the served spelling', async () => {
  const t = load({ groups: [{ name: 'Bay Area', codes: ['sjc', 'oAk', 'SJC'] }] });
  await t.RF.init(t.container);
  assert.deepStrictEqual(requireGroups(t.RF)[0].codes, ['SJC', 'OAK']);
});

test('a one-region deployment renders neither control nor quick picks', async () => {
  const t = load({ regions: { SJC: 'San Jose' }, groups: [{ name: 'South Bay', codes: ['SJC'] }] });
  await t.RF.init(t.container);
  assert.strictEqual(t.container.innerHTML, '');
  assert.strictEqual(t.container.style.display, 'none');
});

section('\n=== #416 selecting a group ===');

test('a quick pick selects the group codes, saves and notifies once', async () => {
  const t = load();
  await t.RF.init(t.container);
  pickGroup(t.container, 0);
  assert.deepStrictEqual(sel(t.RF), ['SJC', 'SFO', 'OAK']);
  assert.strictEqual(t.RF.regionQueryString(), '&region=SJC%2CSFO%2COAK');
  assert.deepStrictEqual(saved(t.store), ['SJC', 'SFO', 'OAK']);
  assert.deepStrictEqual(t.calls, [['SJC', 'SFO', 'OAK']]);
});

test('picking the group already selected changes and announces nothing', async () => {
  const t = load({ stored: ['SJC', 'SFO', 'OAK'] });
  await t.RF.init(t.container);
  pickGroup(t.container, 0);
  assert.deepStrictEqual(t.calls, [], 'a no-op must not make pages refetch');
  assert.deepStrictEqual(saved(t.store), ['SJC', 'SFO', 'OAK']);
});

test('the same selection in another order is still a no-op', async () => {
  const t = load({ stored: ['OAK', 'SJC', 'SFO'] });
  await t.RF.init(t.container);
  pickGroup(t.container, 0);
  assert.deepStrictEqual(t.calls, []);
});

test('switching between two groups notifies once per change', async () => {
  const t = load();
  await t.RF.init(t.container);
  pickGroup(t.container, 0);
  pickGroup(t.container, 1);
  pickGroup(t.container, 1);
  pickGroup(t.container, 0);
  assert.deepStrictEqual(t.calls, [['SJC', 'SFO', 'OAK'], ['SJC'], ['SJC', 'SFO', 'OAK']]);
});

test('a group covering every region selects all regions', async () => {
  const t = load({ groups: [{ name: 'Everything', codes: ['SJC', 'SFO', 'OAK', 'MRY'] }], stored: ['SJC'] });
  await t.RF.init(t.container);
  pickGroup(t.container, 0);
  assert.strictEqual(t.RF.getSelected(), null, 'all codes selected means all regions');
  assert.strictEqual(t.RF.getRegionParam(), '');
  assert.strictEqual(saved(t.store), null);
  assert.deepStrictEqual(t.calls, [null]);
  assert.ok(/data-region="__all__" role="checkbox" aria-checked="true"/.test(t.container.innerHTML),
    'the All control reads as active: ' + t.container.innerHTML);
});

test('an all-regions group picked while all are selected is a no-op', async () => {
  const t = load({ groups: [{ name: 'Everything', codes: ['SJC', 'SFO', 'OAK', 'MRY'] }] });
  await t.RF.init(t.container);
  pickGroup(t.container, 0);
  assert.deepStrictEqual(t.calls, []);
});

test('an out-of-range or missing group index does nothing', async () => {
  const t = load({ stored: ['SJC'] });
  await t.RF.init(t.container);
  for (const idx of [2, -1, 99, 'x', '']) pickGroup(t.container, idx);
  assert.deepStrictEqual(t.calls, []);
  assert.deepStrictEqual(sel(t.RF), ['SJC']);
});

section('\n=== #416 the quick pick uses the pill path, not its own ===');

test('a region pill click still notifies once', async () => {
  const t = load();
  await t.RF.init(t.container);
  clickRegion(t.container, 'SJC');
  assert.deepStrictEqual(t.calls, [['SJC']]);
  assert.deepStrictEqual(saved(t.store), ['SJC']);
});

test('clicking All while all regions are selected announces nothing', async () => {
  const t = load();
  await t.RF.init(t.container);
  clickRegion(t.container, '__all__');
  assert.deepStrictEqual(t.calls, [], 'clearing an empty filter must not refetch');
});

test('clicking All after a quick pick clears the selection once', async () => {
  const t = load();
  await t.RF.init(t.container);
  pickGroup(t.container, 1);
  clickRegion(t.container, '__all__');
  assert.deepStrictEqual(t.calls, [['SJC'], null]);
  assert.strictEqual(saved(t.store), null);
});

test('a pill toggle after a quick pick works off the picked set', async () => {
  const t = load();
  await t.RF.init(t.container);
  pickGroup(t.container, 0);
  clickRegion(t.container, 'OAK');
  assert.deepStrictEqual(sel(t.RF), ['SJC', 'SFO']);
  assert.deepStrictEqual(t.calls, [['SJC', 'SFO', 'OAK'], ['SJC', 'SFO']]);
});

test('offChange stops a listener from hearing a quick pick', async () => {
  const t = load();
  await t.RF.init(t.container);
  const seen = [];
  const fn = t.RF.onChange((sel) => seen.push(sel));
  t.RF.offChange(fn);
  pickGroup(t.container, 0);
  assert.deepStrictEqual(seen, []);
  assert.strictEqual(t.calls.length, 1, 'the other listener still hears it');
});

section('\n=== #416 quick picks in dropdown mode ===');

test('dropdown mode renders the quick picks too', async () => {
  const t = load();
  t.container._forceDropdown = true;
  await t.RF.init(t.container, { dropdown: true });
  assert.ok(/region-dropdown-trigger/.test(t.container.innerHTML), 'dropdown control');
  assert.strictEqual(groupButtons(t.container.innerHTML), 2);
  pickGroup(t.container, 1);
  assert.deepStrictEqual(t.calls, [['SJC']]);
});

test('a dropdown checkbox click is not also handled as a quick pick', async () => {
  const t = load();
  await t.RF.init(t.container, { dropdown: true });
  // In dropdown mode the menu's own onchange owns [data-region]; the
  // delegated container handler must ignore it or the toggle fires twice.
  clickRegion(t.container, 'SJC');
  assert.deepStrictEqual(t.calls, []);
});

section('\n=== #416 URL state, through #531\'s replaceState helper ===');

// The quick picks carry no URL code of their own: applySelection calls
// #531/#549's syncUrlRegion for every real change, so these assert the quick
// pick travels that same path rather than re-testing the helper.

test('a quick pick updates a region parameter already in the hash', async () => {
  const t = load({ hash: '#/nodes?region=MRY&sortBy=lastSeen' });
  await t.RF.init(t.container);
  pickGroup(t.container, 0);
  assert.strictEqual(t.replaced.length, 1, 'one replaceState expected, got ' + t.replaced.length);
  const params = new URLSearchParams(String(t.ctx.location.hash).split('?')[1] || '');
  assert.strictEqual(params.get('region'), 'SJC,SFO,OAK');
  assert.strictEqual(params.get('sortBy'), 'lastSeen', 'the other parameters must survive');
  assert.ok(/^#\/nodes\b/.test(t.ctx.location.hash), 'the route must survive: ' + t.ctx.location.hash);
});

test('listeners hear the change only after the URL carries it', async () => {
  const t = load({ hash: '#/nodes?region=MRY' });
  await t.RF.init(t.container);
  pickGroup(t.container, 0);
  assert.deepStrictEqual(t.calls, [['SJC', 'SFO', 'OAK']]);
  assert.ok(/region=SJC%2CSFO%2COAK/.test(t.hashAtNotify[0]),
    'the hash at notify time was ' + t.hashAtNotify[0]);
});

test('an all-regions group takes the region parameter out of the hash', async () => {
  const t = load({
    hash: '#/nodes?region=SJC',
    stored: ['SJC'],
    groups: [{ name: 'Everything', codes: ['SJC', 'SFO', 'OAK', 'MRY'] }],
  });
  await t.RF.init(t.container);
  pickGroup(t.container, 0);
  assert.ok(!/region=/.test(t.ctx.location.hash), 'region left in the hash: ' + t.ctx.location.hash);
});

test('a hash with no region parameter is left alone', async () => {
  const t = load({ hash: '#/nodes?sortBy=lastSeen' });
  await t.RF.init(t.container);
  pickGroup(t.container, 0);
  assert.deepStrictEqual(t.replaced, [], 'the quick pick wrote to a hash that had no region');
  assert.strictEqual(t.ctx.location.hash, '#/nodes?sortBy=lastSeen');
});

test('a no-op quick pick writes no URL', async () => {
  const t = load({ hash: '#/nodes?region=SJC,SFO,OAK', stored: ['SJC', 'SFO', 'OAK'] });
  await t.RF.init(t.container);
  pickGroup(t.container, 0);
  assert.deepStrictEqual(t.replaced, []);
  assert.deepStrictEqual(t.calls, []);
});

test('a link selection lights the matching group', async () => {
  const t = load({ hash: '#/nodes?region=sjc,sfo,oak' });
  await t.RF.init(t.container);
  t.RF.syncFromUrl('#/nodes?region=sjc,sfo,oak');
  t.RF.render(t.container);
  assert.ok(/region-quick-pick region-quick-pick-active" data-region-group="0" aria-pressed="true"/
    .test(t.container.innerHTML), t.container.innerHTML);
});

section('\n=== #416 markup: accessible and escaped ===');

test('quick picks are buttons in a labelled group with aria-pressed', async () => {
  const t = load();
  await t.RF.init(t.container);
  const html = t.container.innerHTML;
  assert.ok(/<div class="region-quick-picks" role="group" aria-label="[^"]+"/.test(html), html);
  assert.strictEqual((html.match(/<button type="button" class="region-quick-pick/g) || []).length, 2, html);
  assert.ok(/aria-pressed="false"/.test(html), 'unselected group is not pressed');
  pickGroup(t.container, 0);
  assert.ok(/region-quick-pick region-quick-pick-active" data-region-group="0" aria-pressed="true"/
    .test(t.container.innerHTML), t.container.innerHTML);
  assert.ok(/aria-pressed="false"/.test(t.container.innerHTML), 'the other group is not pressed');
});

test('the container is flagged only while quick picks are rendered', async () => {
  const t = load();
  await t.RF.init(t.container);
  assert.strictEqual(t.container.classList.contains('has-region-quick-picks'), true);
  const none = load({ groups: [] });
  await none.RF.init(none.container);
  assert.strictEqual(none.container.classList.contains('has-region-quick-picks'), false);
});

test('a hostile group name and codes are escaped (#333)', async () => {
  const hostile = '"><img src=x onerror="window.__x=1">';
  const t = load({ groups: [{ name: hostile, codes: ['SJC'] }] });
  await t.RF.init(t.container);
  const html = t.container.innerHTML;
  assert.ok(!/<img/.test(html), 'no live tag: ' + html);
  assert.ok(html.indexOf('&lt;img') >= 0, 'escaped into the text slot: ' + html);
  assert.ok(html.indexOf('&quot;&gt;&lt;img') >= 0, 'escaped in the title attribute too: ' + html);
});

test('the codes of a group are listed in its title attribute', async () => {
  const t = load();
  await t.RF.init(t.container);
  assert.ok(/title="SJC, SFO, OAK"/.test(t.container.innerHTML), t.container.innerHTML);
});

section('\n=== #416 requests ===');

test('both config endpoints are fetched once, and only once', async () => {
  const t = load();
  await t.RF.init(t.container);
  await t.RF.fetchRegions();
  assert.deepStrictEqual(t.fetched.slice().sort(), ['/api/config/regions', '/api/config/regions/groups']);
});

test('a quick pick issues no config request of its own', async () => {
  const t = load();
  await t.RF.init(t.container);
  const before = t.fetched.length;
  pickGroup(t.container, 0);
  assert.strictEqual(t.fetched.length, before);
});

(async () => {
  for (const item of queue) {
    if (item.title) { console.log(item.title); continue; }
    try { await item.fn(); passed++; console.log('  ✅ ' + item.name); }
    catch (e) { failed++; console.log('  ❌ ' + item.name + ': ' + e.message); }
  }
  console.log('\n' + passed + ' passed, ' + failed + ' failed');
  process.exit(failed > 0 ? 1 : 0);
})();
