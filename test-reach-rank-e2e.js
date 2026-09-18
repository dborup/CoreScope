/**
 * E2E: Reach leaderboard (#/reach-rank) + the Reach page's Rank card link.
 *
 *  1. /api/reach-rank shape; the page renders the API's first page (ranks,
 *     names, neighbour counts, Reach links) with the "historical neighbour
 *     count" disclaimer and the snapshot time.
 *  2. Paging keeps global ranks; search shows global ranks (no renumbering);
 *     no-match and past-the-end states.
 *  3. Keyboard: search → Enter → Tab to a row link → Enter opens that node's
 *     Reach page, whose Rank card matches the leaderboard (same snapshot) and
 *     links back; Back restores the search from the hash.
 *  4. Mobile 375×812: no horizontal scroll, all three columns visible.
 *  5. Hostile node names render as text (API response intercepted); a failed
 *     API call shows an error, never an empty leaderboard.
 *
 * Defaults to localhost:13581 — NEVER point at prod (AGENTS.md). CI sets BASE_URL.
 * Run: BASE_URL=http://localhost:13581 node test-reach-rank-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const DISCLAIMER = 'Historical neighbour count — not a measure of radio quality or range.';

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

async function getJson(page, path) {
  const r = await page.request.get(BASE + path);
  if (!r.ok()) throw new Error('GET ' + path + ' → HTTP ' + r.status());
  return r.json();
}

async function openBoard(page, hash) {
  await page.goto(BASE + '/#/reach-rank' + (hash || ''));
  await page.waitForSelector('#rrRows tr');
  await page.waitForFunction(() => document.getElementById('rrTable') &&
    !document.getElementById('rrTable').hasAttribute('aria-busy'));
}

async function tableRows(page) {
  return page.$$eval('#rrRows tr', trs => trs.map(tr => {
    const tds = tr.querySelectorAll('td');
    const a = tr.querySelector('a.nq-link');
    return {
      cells: tds.length,
      rank: tds[0] ? tds[0].textContent.trim() : '',
      node: a ? a.textContent.replace('Reach page for ', '').trim() : '',
      href: a ? a.getAttribute('href') : '',
      neighbors: tds[2] ? tds[2].textContent.trim() : '',
    };
  }));
}

function nodeLabel(r) { return r.name || r.pubkey.slice(0, 12); }

async function main() {
  let browser;
  try {
    browser = await chromium.launch({
      headless: true,
      executablePath: process.env.CHROMIUM_PATH || undefined,
      args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
    });
  } catch (err) {
    if (process.env.CHROMIUM_REQUIRE === '1') {
      console.error('test-reach-rank-e2e.js: FAIL — Chromium required but unavailable: ' + err.message);
      process.exit(1);
    }
    console.log('test-reach-rank-e2e.js: SKIP (Chromium unavailable: ' + err.message.split('\n')[0] + ')');
    process.exit(0);
  }
  const ctx = await browser.newContext({ viewport: { width: 1200, height: 900 } });
  const page = await ctx.newPage();
  page.setDefaultTimeout(20000);
  // Leaving a node's Reach page while its Leaflet map is still animating
  // throws "reading '_leaflet_pos'" — a pre-existing map-teardown race that
  // reproduces on master's frontend and is unrelated to the leaderboard. This
  // test leaves the Reach page quickly, so ignore that one message only.
  const LEAFLET_TEARDOWN = /reading '_leaflet_pos'/;
  const pageErrors = [];
  page.on('pageerror', e => { if (!LEAFLET_TEARDOWN.test(e.message)) pageErrors.push(e.message); });

  const api = await getJson(page, '/api/reach-rank');
  console.log('reach-rank E2E: total=' + api.total + ' snapshot_at=' + api.snapshot_at);

  await step('API shape', async () => {
    for (const k of ['snapshot_at', 'total', 'matched', 'offset', 'limit', 'q', 'rows']) {
      assert(k in api, 'missing key ' + k);
    }
    assert(api.limit === 50 && api.offset === 0 && api.matched === api.total, 'defaults: ' + JSON.stringify(api));
    assert(api.total > 0, 'fixture must rank at least one node');
    assert(api.rows[0].rank === 1, 'first placement must be #1');
    for (let i = 1; i < api.rows.length; i++) {
      const a = api.rows[i - 1], b = api.rows[i];
      if (a.neighbors === b.neighbors) {
        assert(a.rank === b.rank && a.pubkey < b.pubkey, 'tie at ' + i + ' must share a rank, pubkey-ordered');
      } else {
        assert(a.neighbors > b.neighbors && b.rank === i + 1, 'competition rank broken at ' + i);
      }
    }
  });

  await step('desktop: first page matches the API', async () => {
    await openBoard(page);
    const rows = await tableRows(page);
    assert(rows.length === api.rows.length, 'rows ' + rows.length + ' != ' + api.rows.length);
    for (let i = 0; i < rows.length; i++) {
      const r = api.rows[i];
      assert(rows[i].rank === '#' + r.rank, 'row ' + i + ' rank ' + rows[i].rank + ' != #' + r.rank);
      assert(rows[i].node === nodeLabel(r), 'row ' + i + ' name ' + rows[i].node + ' != ' + nodeLabel(r));
      assert(rows[i].neighbors === String(r.neighbors), 'row ' + i + ' neighbours');
      assert(rows[i].href === '#/nodes/' + r.pubkey + '/reach', 'row ' + i + ' href ' + rows[i].href);
    }
    const note = await page.textContent('.rr-note');
    assert(note.includes(DISCLAIMER), 'disclaimer missing: ' + note);
    const snap = await page.textContent('#rrSnapshot');
    assert(/^Snapshot .+ ago\)\.$/.test(snap.trim()), 'snapshot text: ' + snap);
    assert(await page.isDisabled('#rrPrev'), 'Previous disabled on page 1');
    assert((await page.isDisabled('#rrNext')) === !(api.total > 50), 'Next state vs total');
  });

  if (api.total > 50) {
    await step('paging keeps global ranks and updates the hash', async () => {
      const p2 = await getJson(page, '/api/reach-rank?offset=50');
      await page.click('#rrNext');
      await page.waitForFunction(r => document.querySelector('#rrRows tr td') &&
        document.querySelector('#rrRows tr td').textContent.trim() === '#' + r, p2.rows[0].rank);
      assert(page.url().endsWith('#/reach-rank?page=2'), 'hash: ' + page.url());
      const status = await page.textContent('#rrStatus');
      assert(status.startsWith('Showing 51–'), 'status: ' + status);
      await page.click('#rrPrev');
      await page.waitForFunction(() => document.querySelector('#rrRows tr td').textContent.trim() === '#1');
    });
  }

  // A ranked node below #1 whose Reach page renders the stats (it needs a
  // reliable path-hash token; without one the page shows only a notice).
  let target = null;
  for (const r of api.rows.slice(1)) {
    const rep = await getJson(page, '/api/nodes/' + r.pubkey + '/reach');
    if (rep.reliable_tokens && rep.reliable_tokens.length) { target = r; break; }
  }
  if (!target) throw new Error('fixture has no ranked node with a Reach stats card');
  console.log('reach-rank E2E: target #' + target.rank + ' ' + nodeLabel(target));
  await step('search shows global ranks (no renumbering)', async () => {
    const q = target.name ? target.name : target.pubkey.slice(0, 10);
    const expect = await getJson(page, '/api/reach-rank?q=' + encodeURIComponent(q));
    await openBoard(page);
    await page.fill('#rrSearch', q);
    await page.press('#rrSearch', 'Enter');
    await page.waitForFunction(n => document.querySelectorAll('#rrRows a.nq-link').length === n, expect.rows.length);
    const rows = await tableRows(page);
    for (let i = 0; i < rows.length; i++) {
      assert(rows[i].rank === '#' + expect.rows[i].rank, 'search row ' + i + ' rank ' + rows[i].rank);
    }
    assert(rows.some(r => r.href === '#/nodes/' + target.pubkey + '/reach' && r.rank === '#' + target.rank),
      'target keeps its global rank #' + target.rank);
    assert(page.url().includes('q=' + encodeURIComponent(q)), 'hash carries q: ' + page.url());
  });

  await step('search by pubkey prefix and no-match message', async () => {
    await page.fill('#rrSearch', target.pubkey.slice(0, 16).toUpperCase());
    await page.press('#rrSearch', 'Enter');
    await page.waitForFunction(h => {
      const links = document.querySelectorAll('#rrRows a.nq-link');
      return links.length === 1 && links[0].getAttribute('href') === h;
    }, '#/nodes/' + target.pubkey + '/reach');
    await page.fill('#rrSearch', 'zz-no-such-node-zz');
    await page.press('#rrSearch', 'Enter');
    await page.waitForFunction(() => /No ranked node matches/.test(document.getElementById('rrRows').textContent));
    assert(await page.isDisabled('#rrNext') && await page.isDisabled('#rrPrev'), 'pager disabled on no match');
  });

  await step('past-the-end ?page snaps back to the last page', async () => {
    await openBoard(page, '?page=999');
    await page.waitForFunction(() => !/page=999/.test(location.hash));
    assert((await tableRows(page)).length > 0, 'rows rendered after snapping back');
  });

  await step('keyboard: search → Tab → Enter opens Reach; Rank card matches; Back restores search', async () => {
    const q = target.name ? target.name : target.pubkey.slice(0, 10);
    await openBoard(page);
    await page.focus('#rrSearch');
    await page.keyboard.type(q);
    await page.keyboard.press('Enter');
    await page.waitForFunction(h => [...document.querySelectorAll('#rrRows a.nq-link')].some(a => a.getAttribute('href') === h),
      '#/nodes/' + target.pubkey + '/reach');
    // Tab from the search box walks the rows' links in order.
    let found = false;
    for (let i = 0; i < 60 && !found; i++) {
      await page.keyboard.press('Tab');
      found = await page.evaluate(h => document.activeElement && document.activeElement.getAttribute('href') === h,
        '#/nodes/' + target.pubkey + '/reach');
    }
    assert(found, 'target row link not reachable by Tab');
    await page.keyboard.press('Enter');
    await page.waitForSelector('.nq-rank-link');
    assert(page.url().endsWith('#/nodes/' + target.pubkey + '/reach'), 'navigated to ' + page.url());
    const reach = await getJson(page, '/api/nodes/' + target.pubkey + '/reach');
    const board = await getJson(page, '/api/reach-rank');
    assert(reach.importance.rank_snapshot_at === board.snapshot_at, 'same snapshot expected');
    assert(reach.importance.rank_status === 'ranked', 'rank_status ' + reach.importance.rank_status);
    assert(reach.importance.degree_rank === target.rank && reach.importance.nodes_with_edges === board.total,
      'Reach rank #' + reach.importance.degree_rank + '/' + reach.importance.nodes_with_edges + ' != board #' + target.rank + '/' + board.total);
    const cardText = await page.$$eval('.analytics-stat-card', cs => cs.map(c => c.textContent).join('|'));
    assert(cardText.includes('#' + target.rank + ' / ' + board.total), 'Rank card text: ' + cardText);
    assert(await page.getAttribute('.nq-rank-link', 'href') === '#/reach-rank', 'View leaderboard link');
    await page.goBack();
    await page.waitForSelector('#rrSearch');
    assert(await page.inputValue('#rrSearch') === q, 'search restored after Back');
  });

  await step('Reach card: "View leaderboard" opens the leaderboard', async () => {
    await page.goto(BASE + '/#/nodes/' + target.pubkey + '/reach');
    await page.waitForSelector('.nq-rank-link');
    await page.click('.nq-rank-link');
    await page.waitForSelector('#rrRows a.nq-link');
    assert(page.url().endsWith('#/reach-rank'), 'url ' + page.url());
  });

  await step('mobile 375×812: no horizontal scroll, three columns visible', async () => {
    const m = await browser.newContext({ viewport: { width: 375, height: 812 }, isMobile: true, hasTouch: true });
    const mp = await m.newPage();
    await openBoard(mp);
    const dims = await mp.evaluate(() => {
      const se = document.scrollingElement;
      const t = document.getElementById('rrTable');
      const ths = [...t.querySelectorAll('thead th')].filter(th => th.offsetWidth > 0).length;
      return { sw: se.scrollWidth, cw: se.clientWidth, tw: t.scrollWidth, tcw: t.parentElement.clientWidth, ths };
    });
    assert(dims.sw <= dims.cw + 1, 'page scrolls horizontally: ' + JSON.stringify(dims));
    assert(dims.tw <= dims.tcw + 1, 'table overflows: ' + JSON.stringify(dims));
    assert(dims.ths === 3, 'visible columns ' + dims.ths);
    assert(await mp.isVisible('#rrSearch') && await mp.isVisible('#rrStatus'), 'search + status visible');
    await m.close();
  });

  await step('hostile node names render as text (intercepted API)', async () => {
    const x = await browser.newContext({ viewport: { width: 1200, height: 900 } });
    const xp = await x.newPage();
    const evilPk = 'ab'.repeat(32);
    await xp.route('**/api/reach-rank*', route => route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        snapshot_at: new Date().toISOString(), total: 1, matched: 1, offset: 0, limit: 50, q: '',
        rows: [{ rank: 1, pubkey: evilPk, name: '<img src=x onerror="window.__rrXss=1">', neighbors: 3 }],
      }),
    }));
    await openBoard(xp);
    const cell = await xp.textContent('#rrRows td.rr-node a');
    assert(cell.includes('<img src=x onerror="window.__rrXss=1">'), 'name not shown literally: ' + cell);
    assert(await xp.$('#rrRows img') === null, 'markup injected');
    assert(!(await xp.evaluate(() => window.__rrXss)), 'script executed');
    await x.close();
  });

  await step('API failure shows an error, not an empty leaderboard', async () => {
    const f = await browser.newContext({ viewport: { width: 1200, height: 900 } });
    const fp = await f.newPage();
    await fp.route('**/api/reach-rank*', route => route.fulfill({
      status: 500, contentType: 'application/json', body: '{"error":"reach rank unavailable"}',
    }));
    await fp.goto(BASE + '/#/reach-rank');
    await fp.waitForSelector('#rrError:not([hidden])');
    const err = await fp.textContent('#rrError');
    assert(/Failed to load the leaderboard/.test(err), 'error text: ' + err);
    const body = await fp.textContent('#rrRows');
    assert(/unavailable/i.test(body) && !/No ranked nodes/.test(body), 'rows: ' + body);
    await f.close();
  });

  await step('no page errors', async () => {
    assert(pageErrors.length === 0, pageErrors.join(' | '));
  });

  await browser.close();
  console.log('\ntest-reach-rank-e2e.js: ' + passed + ' passed, ' + failed + ' failed');
  if (failed) process.exit(1);
}

main().catch(e => { console.error(e); process.exit(1); });
