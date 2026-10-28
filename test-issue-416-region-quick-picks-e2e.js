// test-issue-416-region-quick-picks-e2e.js
//
// #416 — named region quick-pick groups, against a running Go server with the
// CI-prepared e2e fixture (four observer regions: MRY, OAK, SFO, SJC).
//
// The server's own normalization of `regionGroups` is covered by
// cmd/server/region_groups_416_test.go; here the groups are injected by
// answering GET /api/config/regions/groups in the browser, so no config file
// is needed and the fixture stays untouched. One check does hit the real
// endpoint, to confirm a server with no `regionGroups` serves [] and the page
// then looks exactly as it did before the feature.
//
// Covered:
//   - a quick pick selects its codes in ONE action and the page refetches
//     once with region=<codes> (Nodes, pill mode)
//   - picking the group already selected fires no request at all
//   - the quick pick is reachable and operable from the keyboard
//   - "All" resets the selection and refetches once
//   - the selection survives a reload (localStorage) with the group active
//   - a ?region= link restores the selection and lights the matching group
//     (Packets, dropdown mode)
//   - 390 px: the quick picks are visible, inside the viewport, and meet the
//     48 px touch minimum on a touch device
//   - no groups configured: no quick-pick row anywhere
//
// Usage: BASE_URL=http://localhost:13581 node test-issue-416-region-quick-picks-e2e.js
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const GROUPS = [
  { name: 'Bay Area', codes: ['SJC', 'SFO'] },
  { name: 'Peninsula', codes: ['SFO'] },
];

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

// A context that answers the groups endpoint with `groups` and records every
// /api/nodes and /api/packets request the page makes.
async function ctxWith(browser, groups, viewport) {
  const ctx = await browser.newContext(viewport || { viewport: { width: 1400, height: 900 } });
  await ctx.route((url) => url.pathname === '/api/config/regions/groups', (route) =>
    route.fulfill({ contentType: 'application/json', body: JSON.stringify(groups) }));
  const page = await ctx.newPage();
  page.setDefaultTimeout(20000);
  page.on('pageerror', (e) => console.error('    [pageerror]', e.message));
  const dataReqs = [];
  page.on('request', (r) => {
    const u = new URL(r.url());
    if (u.pathname === '/api/nodes' || u.pathname === '/api/packets') {
      dataReqs.push({ path: u.pathname, region: u.searchParams.get('region') });
    }
  });
  page._dataReqs = dataReqs;
  return { ctx, page };
}

const quickPicks = (page) => page.locator('#nodesRegionFilter .region-quick-pick');
const nodeReqs = (page) => page._dataReqs.filter((r) => r.path === '/api/nodes');

// Pages refetch from RegionFilter.onChange, and app.js caches /api/nodes
// responses in memory, so going back to a selection that was already loaded
// is deliberately served without a request. The notification is therefore
// what "the page refetches once" means here; the region a request carries is
// asserted wherever one does go out.
async function watchNotifications(page) {
  await page.evaluate(() => {
    window.__r416 = [];
    window.RegionFilter.onChange((sel) => window.__r416.push(sel));
  });
}
const notes = (page) => page.evaluate(() => window.__r416.slice());
const clearNotes = (page) => page.evaluate(() => { window.__r416.length = 0; });
const sorted = (v) => (Array.isArray(v) ? v.slice().sort() : v);

async function gotoNodes(page) {
  await page.goto(BASE + '/#/nodes', { waitUntil: 'domcontentloaded' });
  await page.locator('#nodesRegionFilter .region-quick-picks').waitFor({ state: 'visible' });
  await page.waitForFunction(() => {
    const el = document.querySelector('#nodesLeft');
    return el && el.textContent.trim().length > 0;
  });
}

// Wait until no new /api/nodes request has arrived for `ms`, then return the
// requests seen since `from`.
async function settle(page, from, ms) {
  let last = -1, seen = nodeReqs(page).length;
  while (last !== seen) {
    last = seen;
    await page.waitForTimeout(ms || 900);
    seen = nodeReqs(page).length;
  }
  return nodeReqs(page).slice(from);
}

(async () => {
  const browser = await chromium.launch();
  console.log('#416 region quick picks — ' + BASE);

  console.log('\n── Nodes page (pill mode), desktop');
  {
    const { ctx, page } = await ctxWith(browser, GROUPS);
    await gotoNodes(page);
    await watchNotifications(page);

    await step('both configured groups render as buttons', async () => {
      assert(await quickPicks(page).count() === 2, 'got ' + (await quickPicks(page).count()) + ' quick picks');
      assert(await quickPicks(page).nth(0).innerText() === 'Bay Area', 'first label');
      assert(await quickPicks(page).nth(0).getAttribute('aria-pressed') === 'false', 'not pressed initially');
      assert(await quickPicks(page).nth(0).getAttribute('title') === 'SJC, SFO', 'codes in the title');
    });

    await step('one click selects the group and refetches once with its codes', async () => {
      await clearNotes(page);
      const from = nodeReqs(page).length;
      await quickPicks(page).nth(0).click();
      const reqs = await settle(page, from);
      assert(JSON.stringify(await notes(page)) === '[["SJC","SFO"]]',
        'one notification expected, got ' + JSON.stringify(await notes(page)));
      assert(reqs.length > 0, 'the page did not refetch');
      assert(reqs.every((r) => r.region === 'SJC,SFO'),
        'requests carried ' + JSON.stringify(reqs.map((r) => r.region)));
      assert(await quickPicks(page).nth(0).getAttribute('aria-pressed') === 'true', 'group not pressed');
      assert(await quickPicks(page).nth(1).getAttribute('aria-pressed') === 'false', 'other group pressed');
      const stored = await page.evaluate(() => JSON.parse(localStorage.getItem('meshcore-region-filter')));
      assert(JSON.stringify(stored.slice().sort()) === '["SFO","SJC"]', 'saved ' + JSON.stringify(stored));
    });

    await step('the matching region pills read as selected', async () => {
      const active = await page.locator('#nodesRegionFilter .region-pill-active').allInnerTexts();
      assert(JSON.stringify(active.slice().sort()) === '["SFO","SJC"]', 'active pills: ' + JSON.stringify(active));
    });

    await step('picking the same group again notifies nobody and fires no request', async () => {
      await clearNotes(page);
      const from = nodeReqs(page).length;
      await quickPicks(page).nth(0).click();
      await page.waitForTimeout(1200);
      assert(JSON.stringify(await notes(page)) === '[]',
        'a no-op announced ' + JSON.stringify(await notes(page)));
      const after = nodeReqs(page).slice(from);
      assert(after.length === 0, 'a no-op refetched: ' + JSON.stringify(after));
      assert(await quickPicks(page).nth(0).getAttribute('aria-pressed') === 'true', 'still pressed');
    });

    await step('the keyboard operates a quick pick', async () => {
      await clearNotes(page);
      const from = nodeReqs(page).length;
      await quickPicks(page).nth(1).focus();
      const focused = await page.evaluate(() => document.activeElement &&
        document.activeElement.className.indexOf('region-quick-pick') >= 0);
      assert(focused, 'the quick pick cannot take focus');
      await page.keyboard.press('Enter');
      const reqs = await settle(page, from);
      assert(JSON.stringify(await notes(page)) === '[["SFO"]]',
        'Enter announced ' + JSON.stringify(await notes(page)));
      assert(reqs.length > 0 && reqs.every((r) => r.region === 'SFO'),
        'Enter did not select Peninsula: ' + JSON.stringify(reqs.map((r) => r.region)));
      assert(await quickPicks(page).nth(1).getAttribute('aria-pressed') === 'true', 'Peninsula not pressed');
    });

    await step('All resets the selection and announces it once', async () => {
      await clearNotes(page);
      const from = nodeReqs(page).length;
      await page.locator('#nodesRegionFilter [data-region="__all__"]').click();
      const reqs = await settle(page, from);
      assert(JSON.stringify(await notes(page)) === '[null]',
        'the reset announced ' + JSON.stringify(await notes(page)));
      assert(reqs.every((r) => r.region === null),
        'a request after the reset still carried a region: ' + JSON.stringify(reqs.map((r) => r.region)));
      assert(await page.evaluate(() => localStorage.getItem('meshcore-region-filter')) === null, 'still saved');
      for (const i of [0, 1]) {
        assert(await quickPicks(page).nth(i).getAttribute('aria-pressed') === 'false', 'group ' + i + ' still pressed');
      }
    });

    await step('All again, with nothing selected, announces nothing', async () => {
      await clearNotes(page);
      const from = nodeReqs(page).length;
      await page.locator('#nodesRegionFilter [data-region="__all__"]').click();
      await page.waitForTimeout(1200);
      assert(JSON.stringify(await notes(page)) === '[]',
        'clearing an empty filter announced ' + JSON.stringify(await notes(page)));
      assert(nodeReqs(page).slice(from).length === 0, 'clearing an empty filter refetched');
    });

    await step('the selection and the active group survive a reload', async () => {
      await quickPicks(page).nth(0).click();
      await page.waitForTimeout(900);
      const stored = await page.evaluate(() => JSON.parse(localStorage.getItem('meshcore-region-filter')));
      assert(JSON.stringify(sorted(stored)) === '["SFO","SJC"]', 'saved ' + JSON.stringify(stored));
      page._dataReqs.length = 0;
      await page.reload({ waitUntil: 'domcontentloaded' });
      await page.locator('#nodesRegionFilter .region-quick-picks').waitFor({ state: 'visible' });
      const first = await settle(page, 0);
      assert(first.length > 0 && first.every((r) => r.region === 'SJC,SFO'),
        'the first fetch after a reload: ' + JSON.stringify(first.map((r) => r.region)));
      assert(await quickPicks(page).nth(0).getAttribute('aria-pressed') === 'true', 'group not restored');
    });

    await ctx.close();
  }

  console.log('\n── Packets page (dropdown mode): a ?region= link');
  {
    const { ctx, page } = await ctxWith(browser, GROUPS);
    await page.goto(BASE + '/#/packets?region=SJC,SFO', { waitUntil: 'domcontentloaded' });
    await page.locator('#packetsRegionFilter .region-quick-picks').waitFor({ state: 'visible' });

    await step('quick picks render beside the dropdown trigger', async () => {
      assert(await page.locator('#packetsRegionFilter .region-dropdown-trigger').count() === 1, 'no dropdown');
      assert(await page.locator('#packetsRegionFilter .region-quick-pick').count() === 2, 'no quick picks');
    });

    await step('the link selection lights the matching group', async () => {
      await page.waitForFunction(() => {
        const b = document.querySelector('#packetsRegionFilter .region-quick-pick');
        return b && b.getAttribute('aria-pressed') === 'true';
      });
      const pressed = await page.locator('#packetsRegionFilter .region-quick-pick[aria-pressed="true"]').allInnerTexts();
      assert(JSON.stringify(pressed) === '["Bay Area"]', 'pressed: ' + JSON.stringify(pressed));
    });

    await step('a quick pick puts its codes in the URL, a reset takes them out', async () => {
      // The Packets page keeps `region` in its own hash (updatePacketsUrl),
      // so a quick pick going through the shared selection path lands there
      // like a pill click does. #531/#549 generalizes this to every view
      // through RegionFilter's own replaceState helper; the quick pick needs
      // no code of its own either way.
      await page.goto(BASE + '/#/packets', { waitUntil: 'domcontentloaded' });
      await page.locator('#packetsRegionFilter .region-quick-pick').first().waitFor({ state: 'visible' });
      await page.locator('#packetsRegionFilter .region-quick-pick').first().click();
      await page.waitForFunction(() => /region=/.test(location.hash));
      let hash = await page.evaluate(() => location.hash);
      assert(/^#\/packets\b/.test(hash), 'the route changed: ' + hash);
      const param = new URLSearchParams(hash.split('?')[1] || '').get('region');
      assert(param === 'SJC,SFO', 'the URL carries region=' + param);

      await page.locator('#packetsRegionFilter .region-dropdown-trigger').click();
      await page.locator('#packetsRegionFilter [data-region="__all__"]').check();
      await page.waitForFunction(() => !/region=/.test(location.hash));
      hash = await page.evaluate(() => location.hash);
      assert(!/region=/.test(hash), 'the reset left a region in the URL: ' + hash);
      assert(await page.locator('#packetsRegionFilter .region-quick-pick[aria-pressed="true"]').count() === 0,
        'a group still reads as active after the reset');
    });

    await ctx.close();
  }

  console.log('\n── Mobile, 390 px');
  {
    const { ctx, page } = await ctxWith(browser, GROUPS, {
      viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true,
    });
    await gotoNodes(page);

    await step('the quick picks are visible and inside the viewport', async () => {
      assert(await quickPicks(page).count() === 2, 'quick picks missing on mobile');
      for (const i of [0, 1]) {
        const box = await quickPicks(page).nth(i).boundingBox();
        assert(box, 'group ' + i + ' has no box');
        assert(box.x >= 0 && box.x + box.width <= 390.5,
          'group ' + i + ' overflows: x=' + box.x + ' w=' + box.width);
      }
    });

    await step('the quick picks form their own full-width row under the control', async () => {
      const g = await page.evaluate(() => {
        const c = document.getElementById('nodesRegionFilter');
        const box = (el) => { const b = el.getBoundingClientRect();
          return { x: b.x, y: b.y, w: b.width, right: b.x + b.width, bottom: b.y + b.height }; };
        return {
          container: box(c), bar: box(c.querySelector('.region-filter-bar')),
          picks: box(c.querySelector('.region-quick-picks')),
          wrap: getComputedStyle(c).flexWrap,
        };
      });
      // The container has to wrap, or the region pills get squeezed into half
      // the width to make room for the quick picks beside them.
      assert(g.wrap === 'wrap', 'the container does not wrap: flex-wrap=' + g.wrap);
      assert(g.bar.w > 300, 'the region pill bar was squeezed to ' + g.bar.w.toFixed(1) + 'px');
      assert(g.picks.y >= g.bar.bottom - 1,
        'the quick picks sit beside the control, not under it (y=' + g.picks.y.toFixed(1) +
        ', bar bottom=' + g.bar.bottom.toFixed(1) + ')');
      // Its own row, so the hairline indent of the desktop layout is gone.
      assert(Math.abs(g.picks.x - g.container.x) < 1,
        'the quick-pick row is indented by ' + (g.picks.x - g.container.x).toFixed(1) + 'px');
      assert(g.picks.w >= g.container.w - 1,
        'the quick-pick row is ' + g.picks.w.toFixed(1) + 'px of ' + g.container.w.toFixed(1));
    });

    await step('each quick pick meets the 48 px touch minimum', async () => {
      const coarse = await page.evaluate(() => window.matchMedia('(pointer: coarse)').matches);
      for (const i of [0, 1]) {
        const box = await quickPicks(page).nth(i).boundingBox();
        const min = coarse ? 48 : 24;
        assert(box.height >= min && box.width >= min,
          'group ' + i + ' is ' + box.width + 'x' + box.height + ', want >= ' + min + ' (coarse=' + coarse + ')');
      }
    });

    await step('a tap selects the group and refetches with its codes', async () => {
      const from = nodeReqs(page).length;
      await quickPicks(page).nth(0).tap();
      const reqs = await settle(page, from);
      assert(reqs.length > 0 && reqs.every((r) => r.region === 'SJC,SFO'),
        'tap requests: ' + JSON.stringify(reqs.map((r) => r.region)));
    });

    await ctx.close();
  }

  console.log('\n── No groups configured (the real server)');
  {
    const ctx = await browser.newContext({ viewport: { width: 1400, height: 900 } });
    const page = await ctx.newPage();
    page.setDefaultTimeout(20000);

    await step('the endpoint serves an empty list', async () => {
      await page.goto(BASE + '/#/nodes', { waitUntil: 'domcontentloaded' });
      const body = await page.evaluate(() => fetch('/api/config/regions/groups').then((r) => r.json()));
      assert(Array.isArray(body), 'not an array: ' + JSON.stringify(body));
      assert(body.length === 0, 'expected no groups: ' + JSON.stringify(body));
    });

    await step('no quick-pick row is rendered anywhere', async () => {
      await page.locator('#nodesRegionFilter .region-pill').first().waitFor({ state: 'visible' });
      assert(await page.locator('.region-quick-picks').count() === 0, 'a quick-pick row appeared');
      assert(await page.locator('.region-filter-container.has-region-quick-picks').count() === 0,
        'the container was flagged with no groups');
      assert(await page.locator('#nodesRegionFilter .region-pill').count() === 5,
        'the region pills changed: ' + (await page.locator('#nodesRegionFilter .region-pill').count()));
    });

    await ctx.close();
  }

  await browser.close();
  console.log('\n' + passed + ' passed, ' + failed + ' failed');
  process.exit(failed > 0 ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
