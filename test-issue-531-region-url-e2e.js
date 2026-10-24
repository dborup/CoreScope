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
 *      region-dependent request (no second load), same count as the baseline.
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
 *
 * Mutants: drop the RegionFilter.syncFromUrl() call in app.js's navigate()
 * (1, 2, 3, 6 red); restore Packets' own ?region= handling (3 red); drop the
 * off*() calls in packets/nodes/live destroy() (4, 5 red); notify after every
 * fresh-load region check (1 red: a second load).
 *
 * Usage: BASE_URL=http://localhost:13581 node test-issue-531-region-url-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:3000';
const SETTLE_MS = 2000;

let passed = 0, failed = 0;
async function test(name, fn) {
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
async function openPage(saved) {
  const ctx = await browser.newContext({ viewport: { width: 1400, height: 900 } });
  await ctx.addInitScript((sel) => {
    if (sel) localStorage.setItem('meshcore-region-filter', JSON.stringify(sel));
    else localStorage.removeItem('meshcore-region-filter');
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
  page.on('request', (r) => { if (r.url().includes('/api/')) page.__reqs.push(r.url()); });
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

async function goHash(page, hash) {
  await page.evaluate((h) => { location.hash = h; }, hash);
}

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
      const base = await openPage(['SJC']);
      await base.goto(`${BASE}/#/${from}`, { waitUntil: 'domcontentloaded' });
      await firstRequest(base, 0, view(from).api);
      await base.waitForTimeout(SETTLE_MS);
      let m = mark(base);
      await goHash(base, `#/${v.name}`);
      await firstRequest(base, m, v.api);
      await base.waitForTimeout(SETTLE_MS);
      const want = apiCounts(base, m);
      await base.context().close();

      const page = await openPage(['SFO']);
      await page.goto(`${BASE}/#/${from}`, { waitUntil: 'domcontentloaded' });
      await firstRequest(page, 0, view(from).api);
      await page.waitForTimeout(SETTLE_MS);
      m = mark(page);
      await goHash(page, `#/${v.name}?region=SJC`);
      const first = await firstRequest(page, m, v.api);
      eq(regionOf(first), 'SJC', 'first ' + v.api + ' region');
      eq(await shown(page, v.control), ['SJC'], 'control');
      await page.waitForTimeout(SETTLE_MS);
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

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
