/**
 * E2E (#531): a link's ?region= sets the region selection on every view,
 * before the view's first region-dependent request, and pages remove their
 * region/area listeners when they are left.
 *
 * Against a real browser + the Go server on the CI fixture, whose region list
 * is MRY, OAK, SFO and SJC. Each case saves SFO as the visitor's selection
 * first, so a view that ignores the link asks for SFO instead of SJC.
 *
 *   1. Fresh load of #/<view>?region=SJC on Packets, Nodes, Live (its early
 *      node fetch), Channels and Analytics: the first region-dependent
 *      request carries SJC, the control shows SJC, SJC is saved, and the view
 *      makes the same API requests as a load with SJC already saved.
 *   2. SPA navigation into each of those views with ?region=SJC: the same, one
 *      region-dependent request (no second load), same count as the baseline
 *      (#556: each step waits for the API to go quiet, not only a fixed
 *      settle window, so no view's late requests land in another count).
 *   3. Comma lists (trimmed, repeats dropped) and unknown codes, fresh and in
 *      the app; Packets keeps no URL handling of its own (its old setSelected
 *      would save the unknown code again); a link equal to the saved
 *      selection loads once.
 *   4. Ten navigations across the five views: each view holds exactly one
 *      RegionFilter listener and a stable number of AreaFilter listeners,
 *      and leaving for Home leaves none.
 *   5. After Packets, a region click on Nodes neither reloads packets nor
 *      rewrites the URL to #/packets.
 *   6. Back and forward restore each entry's ?region=: the view loads with it
 *      and any request it makes carries it.
 *   7. A change in the control keeps a ?region= in the hash in step with the
 *      selection on every view with a control (replaceState, other
 *      parameters kept), so a stale link value cannot override the choice on
 *      Back or on reload; picking All removes it. #556: a choice made on a
 *      later entry also stands on Back/Forward to the link's entry and on
 *      reload there; a deep link in a new tab still wins.
 *   8. ?region=All in any case means all regions: one unfiltered request.
 *
 * Mutants: drop the RegionFilter.syncFromUrl() call in app.js's navigate()
 * (1, 2, 3, 6 red); restore Packets' own ?region= handling (3 red); drop the
 * off*() calls in packets/nodes/live destroy() (4, 5 red); notify after every
 * fresh-load region check (1 red: a second load); drop the hash write-back
 * after a control change (7 red); treat All as a code (8 red: two loads);
 * drop the history.state check in syncFromUrl (#556 Back test red); drop
 * the API-quiet waits in section 2 (2 red with SLOW_API=page:/api/packets:4000
 * or SLOW_API=base:/api/nodes:4000).
 *
 * Usage: BASE_URL=http://localhost:13581 [ONLY=<regex>] node test-issue-531-region-url-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:3000';
const SETTLE_MS = 2000;
// #556: SLOW_API=<base|page>:<path>:<ms> delays that API's responses in
// section 2's baseline or page under test, as a loaded machine does, so a
// view finishes loading late. Reproduces the count flake on demand, e.g.
// page:/api/packets:4000 (Packets' late requests) or base:/api/nodes:4000
// (Live's late requests).
const SLOW_API = (() => {
  const m = /^(base|page):([^:]+):(\d+)$/.exec(process.env.SLOW_API || '');
  return m ? { side: m[1], path: m[2], ms: Number(m[3]) } : null;
})();
const slowFor = (side) => (SLOW_API && SLOW_API.side === side ? SLOW_API : null);

// ONLY=<regex> runs the matching tests only (for repeat runs of one section).
const ONLY = process.env.ONLY ? new RegExp(process.env.ONLY) : null;

let passed = 0, failed = 0;
async function test(name, fn) {
  if (ONLY && !ONLY.test(name)) return;
  try { await fn(); passed++; console.log('  ✅ ' + name); }
  catch (err) { failed++; console.log('  ❌ ' + name + ': ' + err.message); }
}
function assert(cond, msg) { if (!cond) throw new Error(msg || 'assertion failed'); }
function eq(a, b, msg) {
  if (JSON.stringify(a) !== JSON.stringify(b)) throw new Error((msg || 'mismatch') + ': got ' + JSON.stringify(a) + ', want ' + JSON.stringify(b));
}

// The region-dependent request each view makes on load, and its control.
const VIEWS = [
  { name: 'packets', api: /\/api\/packets\?/, control: 'packetsRegionFilter' },
  { name: 'nodes', api: /\/api\/nodes\?/, control: 'nodesRegionFilter' },
  { name: 'live', api: /\/api\/nodes\?/, control: 'liveRegionFilter' },
  { name: 'channels', api: /\/api\/channels(\?|$)/, control: 'chRegionFilter' },
  { name: 'analytics', api: /\/api\/analytics\/rf\?/, control: 'analyticsRegionFilter' },
];
const view = (name) => VIEWS.find((v) => v.name === name);

let browser;

// A page whose visitor has `saved` as region selection. Records every /api/
// request and counts the RegionFilter/AreaFilter listeners still registered.
async function openPage(saved, slow) {
  const ctx = await browser.newContext({ viewport: { width: 1400, height: 900 } });
  if (slow) {
    await ctx.route((u) => u.pathname === slow.path, async (route) => {
      await new Promise((r) => setTimeout(r, slow.ms));
      await route.continue();
    });
  }
  await ctx.addInitScript((sel) => {
    // Seeded once per tab, so a reload keeps what the page saved.
    if (!sessionStorage.getItem('__seeded531')) {
      sessionStorage.setItem('__seeded531', '1');
      if (sel) localStorage.setItem('meshcore-region-filter', JSON.stringify(sel));
      else localStorage.removeItem('meshcore-region-filter');
    }
    window.__listeners = { region: new Set(), area: new Set() };
    function count(globalName, key) {
      let api;
      Object.defineProperty(window, globalName, {
        configurable: true,
        get() { return api; },
        set(obj) {
          const on = obj.onChange, off = obj.offChange;
          obj.onChange = function (fn) { window.__listeners[key].add(fn); return on.call(this, fn); };
          obj.offChange = function (fn) { window.__listeners[key].delete(fn); return off.call(this, fn); };
          api = obj;
        },
      });
    }
    count('RegionFilter', 'region');
    count('AreaFilter', 'area');
  }, saved || null);
  const page = await ctx.newPage();
  page.__errors = [];
  page.on('pageerror', (e) => page.__errors.push(e.message));
  page.__reqs = [];
  page.__open = new Set();
  page.on('request', (r) => { if (r.url().includes('/api/')) { page.__reqs.push(r.url()); page.__open.add(r); } });
  page.on('requestfinished', (r) => page.__open.delete(r));
  page.on('requestfailed', (r) => page.__open.delete(r));
  return page;
}
const mark = (page) => page.__reqs.length;
const since = (page, from, re) => page.__reqs.slice(from).filter((u) => re.test(u));
const regionOf = (url) => new URL(url).searchParams.get('region');
const listeners = (page) => page.evaluate(() => ({ region: window.__listeners.region.size, area: window.__listeners.area.size }));
const saved = (page) => page.evaluate(() => JSON.parse(localStorage.getItem('meshcore-region-filter') || 'null'));

async function firstRequest(page, from, re) {
  const deadline = Date.now() + 15000;
  while (Date.now() < deadline) {
    const hit = since(page, from, re)[0];
    if (hit) return hit;
    await page.waitForTimeout(50);
  }
  throw new Error('no request matching ' + re + ' within 15s');
}

// Codes the control shows as selected, or 'ALL'.
async function shown(page, controlId) {
  await page.waitForSelector('#' + controlId + ' [data-region]', { state: 'attached', timeout: 15000 });
  return page.evaluate((id) => {
    const els = Array.from(document.querySelectorAll('#' + id + ' [data-region]'));
    const on = (el) => el.getAttribute('aria-checked') === 'true' || el.checked === true;
    const all = els.find((el) => el.dataset.region === '__all__');
    if (all && on(all)) return 'ALL';
    return els.filter((el) => el.dataset.region !== '__all__' && on(el)).map((el) => el.dataset.region);
  }, controlId);
}

// Path counts of every /api/ request after `from`, minus pollers.
function apiCounts(page, from) {
  const counts = {};
  page.__reqs.slice(from).forEach((u) => {
    const p = new URL(u).pathname;
    if (p === '/api/stats' || p === '/api/healthz') return;
    counts[p] = (counts[p] || 0) + 1;
  });
  return counts;
}

// #556: until no /api/ request is open and none started for QUIET_MS (at
// most 20s). A fixed settle window alone is not enough on a loaded machine:
// a view's later requests (Packets' hops and filters after /api/packets,
// Live's after its node list) then fall on either side of it.
const QUIET_MS = 1000;
async function apiIdle(page) {
  const deadline = Date.now() + 20000;
  let n = page.__reqs.length, quietFrom = Date.now();
  while (Date.now() < deadline) {
    await page.waitForTimeout(100);
    if (page.__reqs.length !== n || page.__open.size > 0) { n = page.__reqs.length; quietFrom = Date.now(); continue; }
    if (Date.now() - quietFrom >= QUIET_MS) return;
  }
}

async function goHash(page, hash) {
  await page.evaluate((h) => { location.hash = h; }, hash);
}

// Toggles `code` in the view's control, pill bar or dropdown (the menu
// re-renders closed after each change). el.click() also reaches Live's
// dropdown, whose menu may sit in a collapsed panel.
async function pick(page, controlId, code) {
  await page.waitForSelector('#' + controlId + ' [data-region="' + code + '"]', { state: 'attached', timeout: 15000 });
  await page.evaluate(([id, c]) => document.querySelector('#' + id + ' [data-region="' + c + '"]').click(), [controlId, code]);
}
// From the single selected `from` to the single selected `to`: add, then drop.
async function switchRegion(page, controlId, from, to) {
  await pick(page, controlId, to);
  await pick(page, controlId, from);
  eq(await shown(page, controlId), [to], 'control after the switch');
}
const hashRegions = (page) => page.evaluate(() => new URLSearchParams(location.hash.split('?')[1] || '').getAll('region'));

(async () => {
  browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #531 region URL across views E2E against ${BASE} ===`);

  // 1. Fresh load.
  for (const v of VIEWS) {
    await test(`fresh #/${v.name}?region=SJC over saved SFO: first request, control, saved, API calls`, async () => {
      const base = await openPage(['SJC']);
      await base.goto(`${BASE}/#/${v.name}`, { waitUntil: 'domcontentloaded' });
      await firstRequest(base, 0, v.api);
      await base.waitForTimeout(SETTLE_MS);
      const want = apiCounts(base, 0);
      await base.context().close();

      const page = await openPage(['SFO']);
      await page.goto(`${BASE}/#/${v.name}?region=SJC`, { waitUntil: 'domcontentloaded' });
      const first = await firstRequest(page, 0, v.api);
      eq(regionOf(first), 'SJC', 'first ' + v.api + ' region');
      eq(await shown(page, v.control), ['SJC'], 'control');
      eq(await saved(page), ['SJC'], 'saved selection');
      await page.waitForTimeout(SETTLE_MS);
      eq(apiCounts(page, 0), want, 'API calls vs a load with SJC saved');
      eq(page.__errors, [], 'page errors');
      await page.context().close();
    });
  }

  // 2. SPA navigation.
  for (const v of VIEWS) {
    const from = v.name === 'packets' ? 'nodes' : 'packets';
    await test(`#/${from} → #/${v.name}?region=SJC over saved SFO: first request, one load, API calls`, async () => {
      const base = await openPage(['SJC'], slowFor('base'));
      await base.goto(`${BASE}/#/${from}`, { waitUntil: 'domcontentloaded' });
      await firstRequest(base, 0, view(from).api);
      await base.waitForTimeout(SETTLE_MS);
      await apiIdle(base);
      let m = mark(base);
      await goHash(base, `#/${v.name}`);
      await firstRequest(base, m, v.api);
      await base.waitForTimeout(SETTLE_MS);
      await apiIdle(base);
      const want = apiCounts(base, m);
      await base.context().close();

      const page = await openPage(['SFO'], slowFor('page'));
      await page.goto(`${BASE}/#/${from}`, { waitUntil: 'domcontentloaded' });
      await firstRequest(page, 0, view(from).api);
      await page.waitForTimeout(SETTLE_MS);
      await apiIdle(page);
      m = mark(page);
      await goHash(page, `#/${v.name}?region=SJC`);
      const first = await firstRequest(page, m, v.api);
      eq(regionOf(first), 'SJC', 'first ' + v.api + ' region');
      eq(await shown(page, v.control), ['SJC'], 'control');
      await page.waitForTimeout(SETTLE_MS);
      await apiIdle(page);
      eq(since(page, m, v.api).map(regionOf), since(page, m, v.api).map(() => 'SJC'), 'every request after the link');
      eq(apiCounts(page, m), want, 'API calls vs navigating with SJC saved');
      eq(page.__errors, [], 'page errors');
      await page.context().close();
    });
  }

  // 3. Lists, unknown codes, no-op.
  await test('fresh comma list " SJC ,OAK,SJC": first request SJC,OAK', async () => {
    const page = await openPage(['SFO']);
    await page.goto(`${BASE}/#/nodes?region=%20SJC%20,OAK,SJC`, { waitUntil: 'domcontentloaded' });
    eq(regionOf(await firstRequest(page, 0, /\/api\/nodes\?/)), 'SJC,OAK');
    eq((await shown(page, 'nodesRegionFilter')).sort(), ['OAK', 'SJC'], 'control');
    await page.context().close();
  });

  await test('fresh link with only an unknown code: ends on all regions, last request unfiltered, nothing saved', async () => {
    const page = await openPage(['SFO']);
    await page.goto(`${BASE}/#/nodes?region=XXX`, { waitUntil: 'domcontentloaded' });
    await firstRequest(page, 0, /\/api\/nodes\?/);
    eq(await shown(page, 'nodesRegionFilter'), 'ALL', 'control');
    await page.waitForTimeout(SETTLE_MS);
    const reqs = since(page, 0, /\/api\/nodes\?/);
    eq(regionOf(reqs[reqs.length - 1]), null, 'last /api/nodes region');
    eq(await saved(page), null, 'saved selection');
    await page.context().close();
  });

  await test('in the app, "SJC,XXX" drops XXX before the first request and loads once', async () => {
    const page = await openPage(['SFO']);
    await page.goto(`${BASE}/#/packets`, { waitUntil: 'domcontentloaded' });
    await firstRequest(page, 0, /\/api\/packets\?/);
    await page.waitForTimeout(SETTLE_MS);
    const m = mark(page);
    await goHash(page, '#/nodes?region=SJC,XXX');
    await firstRequest(page, m, /\/api\/nodes\?/);
    await page.waitForTimeout(SETTLE_MS);
    eq(since(page, m, /\/api\/nodes\?/).map(regionOf), ['SJC'], '/api/nodes regions');
    eq(await saved(page), ['SJC'], 'saved selection');
    await page.context().close();
  });

  await test('Packets uses the shared handling: "SJC,XXX" from Nodes is saved and written back as SJC', async () => {
    const page = await openPage(['SFO']);
    await page.goto(`${BASE}/#/nodes`, { waitUntil: 'domcontentloaded' });
    await firstRequest(page, 0, /\/api\/nodes\?/);
    await page.waitForTimeout(SETTLE_MS);
    const m = mark(page);
    await goHash(page, '#/packets?region=SJC,XXX');
    eq(regionOf(await firstRequest(page, m, /\/api\/packets\?/)), 'SJC', 'first /api/packets region');
    eq(await shown(page, 'packetsRegionFilter'), ['SJC'], 'control');
    await page.waitForTimeout(SETTLE_MS);
    eq(await saved(page), ['SJC'], 'saved selection');
    const hash = await page.evaluate(() => location.hash);
    eq(new URLSearchParams(hash.split('?')[1] || '').get('region'), 'SJC', 'region in the packets URL ' + hash);
    await page.context().close();
  });

  await test('a link equal to the saved selection loads once', async () => {
    const page = await openPage(['SJC']);
    await page.goto(`${BASE}/#/packets`, { waitUntil: 'domcontentloaded' });
    await firstRequest(page, 0, /\/api\/packets\?/);
    await page.waitForTimeout(SETTLE_MS);
    const m = mark(page);
    await goHash(page, '#/nodes?region=SJC');
    await firstRequest(page, m, /\/api\/nodes\?/);
    await page.waitForTimeout(SETTLE_MS);
    eq(since(page, m, /\/api\/nodes\?/).map(regionOf), ['SJC'], '/api/nodes regions');
    await page.context().close();
  });

  // 4. Listener counts.
  await test('10 navigations across the views: one region listener per view, area count stable, none on Home', async () => {
    const page = await openPage(['SFO']);
    await page.goto(`${BASE}/#/home`, { waitUntil: 'domcontentloaded' });
    await page.waitForTimeout(500);
    const seen = {};
    const log = [];
    for (let i = 0; i < 10; i++) {
      const v = VIEWS[i % VIEWS.length];
      // Revisits may answer from the client API cache, so wait on the
      // view's own (freshly rendered) control rather than on a request.
      await goHash(page, `#/${v.name}`);
      await shown(page, v.control);
      await page.waitForTimeout(600); // Live's init resumes past its awaits
      const n = await listeners(page);
      log.push(v.name + ' ' + n.region + '/' + n.area);
      assert(n.region === 1, `region listeners on ${v.name} (nav ${i + 1}): ${n.region}; ${log.join(', ')}`);
      if (seen[v.name]) assert(n.area === seen[v.name].area, `area listeners on ${v.name} grew ${seen[v.name].area} → ${n.area}; ${log.join(', ')}`);
      else seen[v.name] = n;
    }
    await goHash(page, '#/home');
    await page.waitForTimeout(500);
    eq(await listeners(page), { region: 0, area: 0 }, 'listeners on Home; ' + log.join(', '));
    eq(page.__errors, [], 'page errors');
    await page.context().close();
  });

  // 5. Stale Packets listener.
  await test('after Packets, a region click on Nodes neither reloads packets nor rewrites the URL', async () => {
    const page = await openPage(['SFO']);
    await page.goto(`${BASE}/#/packets`, { waitUntil: 'domcontentloaded' });
    await firstRequest(page, 0, /\/api\/packets\?/);
    await page.waitForTimeout(SETTLE_MS);
    let m = mark(page);
    await goHash(page, '#/nodes');
    await firstRequest(page, m, /\/api\/nodes\?/);
    await shown(page, 'nodesRegionFilter');
    await page.waitForTimeout(SETTLE_MS);
    m = mark(page);
    await page.click('#nodesRegionFilter [data-region="OAK"]');
    const req = await firstRequest(page, m, /\/api\/nodes\?/);
    eq(regionOf(req).split(',').sort(), ['OAK', 'SFO'], 'nodes reload region');
    await page.waitForTimeout(SETTLE_MS);
    eq(since(page, m, /\/api\/packets\?/), [], '/api/packets requests after the click');
    const hash = await page.evaluate(() => location.hash);
    assert(hash.startsWith('#/nodes'), 'URL rewritten to ' + hash);
    await page.context().close();
  });

  // 6. Back and forward.
  await test('back and forward restore each entry\'s ?region=', async () => {
    const page = await openPage(['SFO']);
    await page.goto(`${BASE}/#/nodes?region=SJC`, { waitUntil: 'domcontentloaded' });
    eq(regionOf(await firstRequest(page, 0, /\/api\/nodes\?/)), 'SJC', 'first /api/nodes');
    await page.waitForTimeout(SETTLE_MS);
    let m = mark(page);
    await goHash(page, '#/channels?region=OAK');
    eq(regionOf(await firstRequest(page, m, /\/api\/channels(\?|$)/)), 'OAK', 'channels');
    await page.waitForTimeout(SETTLE_MS);
    // The entries' responses may come from the client API cache, so check
    // the selection the view loads with and any request it does make.
    m = mark(page);
    await page.goBack();
    eq(await shown(page, 'nodesRegionFilter'), ['SJC'], 'control after back');
    eq(await page.evaluate(() => location.hash), '#/nodes?region=SJC', 'hash after back');
    eq(await page.evaluate(() => RegionFilter.getRegionParam()), 'SJC', 'selection after back');
    await page.waitForTimeout(SETTLE_MS);
    since(page, m, /\/api\/nodes\?/).forEach((u) => eq(regionOf(u), 'SJC', '/api/nodes after back'));
    m = mark(page);
    await page.goForward();
    eq(await shown(page, 'chRegionFilter'), ['OAK'], 'control after forward');
    eq(await page.evaluate(() => RegionFilter.getRegionParam()), 'OAK', 'selection after forward');
    await page.waitForTimeout(SETTLE_MS);
    since(page, m, /\/api\/channels(\?|$)/).forEach((u) => eq(regionOf(u), 'OAK', '/api/channels after forward'));
    await page.context().close();
  });

  // 7. A control change keeps the hash ?region= in step.
  const WRITE_BACK = [
    { name: 'packets', extra: '', control: 'packetsRegionFilter', api: view('packets').api },
    { name: 'nodes', extra: 'keep=1', control: 'nodesRegionFilter', api: view('nodes').api },
    { name: 'live', extra: 'keep=1', control: 'liveRegionFilter', api: view('live').api },
    { name: 'channels', extra: 'keep=1', control: 'chRegionFilter', api: view('channels').api },
    { name: 'analytics', extra: 'tab=topology', control: 'analyticsRegionFilter', api: /\/api\/analytics\/topology\?/ },
    { name: 'observers', extra: 'keep=1', control: 'obsRegionFilter', api: null },
  ];
  for (const v of WRITE_BACK) {
    const link = `#/${v.name}?${v.extra ? v.extra + '&' : ''}region=SJC`;
    await test(`${link} over saved SFO, then OAK in the control: the hash says OAK and a reload keeps it`, async () => {
      const page = await openPage(['SFO']);
      await page.goto(`${BASE}/${link}`, { waitUntil: 'domcontentloaded' });
      eq(await shown(page, v.control), ['SJC'], 'control from the link');
      await page.waitForTimeout(SETTLE_MS);
      await switchRegion(page, v.control, 'SJC', 'OAK');
      eq(await hashRegions(page), ['OAK'], 'hash region after the switch');
      const hash = await page.evaluate(() => location.hash);
      assert(hash.startsWith('#/' + v.name + '?'), 'route kept: ' + hash);
      if (v.extra) {
        const [k, val] = v.extra.split('=');
        eq(new URLSearchParams(hash.split('?')[1]).get(k), val, v.extra + ' kept in ' + hash);
      }
      eq(await saved(page), ['OAK'], 'saved selection');
      await page.waitForTimeout(SETTLE_MS);
      const r = mark(page);
      await page.reload({ waitUntil: 'domcontentloaded' });
      if (v.api) eq(regionOf(await firstRequest(page, r, v.api)), 'OAK', 'first request after reload');
      eq(await shown(page, v.control), ['OAK'], 'control after reload');
      eq(await saved(page), ['OAK'], 'saved after reload');
      eq(page.__errors, [], 'page errors');
      await page.context().close();
    });
  }

  await test('fresh #/nodes?region=SJC, OAK in the control, Channels, Back: OAK, not SJC', async () => {
    const page = await openPage(['SFO']);
    await page.goto(`${BASE}/#/nodes?region=SJC`, { waitUntil: 'domcontentloaded' });
    eq(await shown(page, 'nodesRegionFilter'), ['SJC'], 'control from the link');
    await page.waitForTimeout(SETTLE_MS);
    await switchRegion(page, 'nodesRegionFilter', 'SJC', 'OAK');
    // Checked last, so a failure first shows what the user sees after Back.
    const afterSwitch = await page.evaluate(() => location.hash);
    await page.waitForTimeout(SETTLE_MS);
    let m = mark(page);
    await goHash(page, '#/channels');
    eq(regionOf(await firstRequest(page, m, view('channels').api)), 'OAK', 'channels');
    await page.waitForTimeout(SETTLE_MS);
    m = mark(page);
    await page.goBack();
    eq(await shown(page, 'nodesRegionFilter'), ['OAK'], 'control after back');
    eq(await page.evaluate(() => RegionFilter.getRegionParam()), 'OAK', 'selection after back');
    eq(await saved(page), ['OAK'], 'saved after back');
    await page.waitForTimeout(SETTLE_MS);
    since(page, m, /\/api\/nodes\?/).forEach((u) => eq(regionOf(u), 'OAK', '/api/nodes after back'));
    await page.reload({ waitUntil: 'domcontentloaded' });
    eq(await shown(page, 'nodesRegionFilter'), ['OAK'], 'control after reload');
    eq(afterSwitch, '#/nodes?region=OAK', 'hash after the switch');
    await page.context().close();
  });

  await test('#556: fresh #/nodes?region=SJC, Channels, OAK there, Back: OAK, not SJC', async () => {
    const page = await openPage(['SFO']);
    await page.goto(`${BASE}/#/nodes?region=SJC`, { waitUntil: 'domcontentloaded' });
    eq(await shown(page, 'nodesRegionFilter'), ['SJC'], 'control from the link');
    await page.waitForTimeout(SETTLE_MS);
    await goHash(page, '#/channels');
    eq(await shown(page, 'chRegionFilter'), ['SJC'], 'channels control');
    await page.waitForTimeout(SETTLE_MS);
    await switchRegion(page, 'chRegionFilter', 'SJC', 'OAK');
    eq(await page.evaluate(() => location.hash), '#/channels', 'channels hash untouched');
    await page.waitForTimeout(SETTLE_MS);
    let m = mark(page);
    await page.goBack();
    eq(await page.evaluate(() => location.hash), '#/nodes?region=SJC', 'hash after back');
    eq(await shown(page, 'nodesRegionFilter'), ['OAK'], 'control after back');
    eq(await page.evaluate(() => RegionFilter.getRegionParam()), 'OAK', 'selection after back');
    eq(await saved(page), ['OAK'], 'saved after back');
    await page.waitForTimeout(SETTLE_MS);
    since(page, m, /\/api\/nodes\?/).forEach((u) => eq(regionOf(u), 'OAK', '/api/nodes after back'));
    await page.goForward();
    eq(await shown(page, 'chRegionFilter'), ['OAK'], 'control after forward');
    await page.goBack();
    eq(await shown(page, 'nodesRegionFilter'), ['OAK'], 'control after back again');
    m = mark(page);
    await page.reload({ waitUntil: 'domcontentloaded' });
    eq(regionOf(await firstRequest(page, m, /\/api\/nodes\?/)), 'OAK', 'first /api/nodes after reload');
    eq(await shown(page, 'nodesRegionFilter'), ['OAK'], 'control after reload');
    eq(page.__errors, [], 'page errors');
    await page.context().close();
  });

  await test('#556: a deep link in a new tab still wins over the saved choice', async () => {
    const page = await openPage(['OAK']);
    await page.goto(`${BASE}/#/nodes?region=SJC`, { waitUntil: 'domcontentloaded' });
    eq(regionOf(await firstRequest(page, 0, /\/api\/nodes\?/)), 'SJC', 'first /api/nodes');
    eq(await shown(page, 'nodesRegionFilter'), ['SJC'], 'control');
    await page.context().close();
  });

  await test('fresh #/nodes?region=SJC, All in the control: region leaves the hash; Back and reload keep All', async () => {
    const page = await openPage(['SFO']);
    await page.goto(`${BASE}/#/nodes?keep=1&region=SJC`, { waitUntil: 'domcontentloaded' });
    eq(await shown(page, 'nodesRegionFilter'), ['SJC'], 'control from the link');
    await page.waitForTimeout(SETTLE_MS);
    await pick(page, 'nodesRegionFilter', '__all__');
    eq(await page.evaluate(() => location.hash), '#/nodes?keep=1', 'hash after All');
    await goHash(page, '#/channels');
    await shown(page, 'chRegionFilter');
    await page.goBack();
    eq(await shown(page, 'nodesRegionFilter'), 'ALL', 'control after back');
    await page.reload({ waitUntil: 'domcontentloaded' });
    eq(await shown(page, 'nodesRegionFilter'), 'ALL', 'control after reload');
    eq(await saved(page), null, 'saved selection');
    await page.context().close();
  });

  // 8. ?region=All.
  for (const [v, all] of [['nodes', 'All'], ['live', 'aLL']]) {
    await test(`fresh #/${v}?region=${all} over saved SFO: all regions, one unfiltered request`, async () => {
      const page = await openPage(['SFO']);
      await page.goto(`${BASE}/#/${v}?region=${all}`, { waitUntil: 'domcontentloaded' });
      await firstRequest(page, 0, view(v).api);
      eq(await shown(page, view(v).control), 'ALL', 'control');
      await page.waitForTimeout(SETTLE_MS);
      eq(since(page, 0, view(v).api).map(regionOf), [null], view(v).api + ' regions');
      eq(await saved(page), null, 'saved selection');
      await page.context().close();
    });
  }

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
