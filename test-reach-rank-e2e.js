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
 *     links back; Back restores the search from the hash (field, URL and
 *     filtered rows), twice via Back/Forward, without reloading the page.
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

// Runs a navigation that should end on the leaderboard and waits until the
// board it produced is ready for input. A same-document hash navigation
// resolves page.goto()/goBack() as soon as the history entry commits, but
// app.js re-renders on the later hashchange, and one frame after each mount
// it moves focus to the page heading (#630-7). Acting earlier types into the
// board that is about to be replaced, or loses keystrokes to that focus move.
// So when the hash changes: wait for a #rrSearch that did not exist before,
// then for the focus hand-off; then for the mount's first fetch. When the hash
// does not change, the router does not re-render and the current board stays.
async function settleBoard(page, navigate) {
  const before = await page.evaluate(() => {
    const el = document.getElementById('rrSearch');
    if (el) el.__rrStale = true;
    window.__rrDoc = window.__rrDoc || String(Math.random());
    return { hash: location.hash, doc: window.__rrDoc };
  });
  await navigate();
  const remounted = await page.evaluate(b => window.__rrDoc !== b.doc || location.hash !== b.hash, before);
  if (remounted) {
    await page.waitForFunction(() => {
      const el = document.getElementById('rrSearch');
      if (el && !el.__rrStale) return true;
      if (window.__rrFlushRoute) window.__rrFlushRoute(); // a held route (see installRouteHold) may run now
      return false;
    }).catch(() => { throw new Error('the leaderboard never re-rendered after navigating to ' + page.url()); });
    await page.waitForFunction(() => {
      const app = document.getElementById('app');
      const target = app.querySelector('h1, h2, h3, [role="heading"]') || app; // app.js #630-7
      if (document.activeElement === target) return true;
      if (window.__rrFlushFrames) window.__rrFlushFrames(); // the held route's frame callbacks may run now
      return false;
    }).catch(() => { throw new Error("app.js never moved focus to the leaderboard heading after navigating to " + page.url()); });
  }
  await page.waitForSelector('#rrRows tr');
  await page.waitForFunction(() => document.getElementById('rrTable') &&
    !document.getElementById('rrTable').hasAttribute('aria-busy'));
}

async function openBoard(page, hash) {
  await settleBoard(page, () => page.goto(BASE + '/#/reach-rank' + (hash || '')));
}

// Race amplifier for the keyboard step. In CI the router usually re-renders
// within a millisecond of a hash navigation committing, and moves focus one
// frame later, so acting too early failed only sometimes. installRouteHold()
// wraps app.js's router (the hashchange listener named 'navigate');
// holdNextRoute() then holds the next route change, and after it runs, the
// animation-frame callbacks it scheduled (the focus hand-off), until the
// page's next key press. settleBoard releases them only after seeing that its
// condition does not hold yet (__rrFlushRoute, __rrFlushFrames). Typing before
// the board is ready therefore loses the search every time instead of now and
// then. Nothing is timed.
async function installRouteHold(page) {
  await page.addInitScript(() => {
    const add = window.addEventListener;
    window.addEventListener = function (type, fn, opts) {
      if (type !== 'hashchange' || typeof fn !== 'function' || fn.name !== 'navigate') return add.call(this, type, fn, opts);
      window.__rrRouteHooked = true;
      return add.call(this, type, function (e) {
        if (!window.__rrHoldNext) return fn.call(this, e);
        window.__rrHoldNext = false;
        let pending = true;
        const run = () => {
          if (!pending) return;
          pending = false;
          window.__rrFlushRoute = null;
          window.removeEventListener('keydown', run, true);
          const raf = window.requestAnimationFrame;
          const frames = [];
          window.requestAnimationFrame = cb => { frames.push(cb); return 0; };
          try { fn.call(window, e); } finally { window.requestAnimationFrame = raf; }
          if (!frames.length) return;
          const runFrames = () => {
            window.__rrFlushFrames = null;
            window.removeEventListener('keydown', runFrames, true);
            frames.splice(0).forEach(cb => cb(performance.now()));
          };
          window.__rrFlushFrames = runFrames;
          add.call(window, 'keydown', runFrames, true);
        };
        window.__rrFlushRoute = run;
        add.call(window, 'keydown', run, true);
      }, opts);
    };
  });
}

async function holdNextRoute(page) {
  const hooked = await page.evaluate(() => { window.__rrHoldNext = true; return window.__rrRouteHooked === true; });
  assert(hooked, "the route hold did not catch app.js's hashchange router (listener named 'navigate')");
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

// Reach + leaderboard read one shared snapshot. Fetch both until they report
// the same snapshot time (a 60s refresh can land between two requests).
async function sameSnapshot(page, pubkey) {
  for (let i = 0; i < 3; i++) {
    const reach = await getJson(page, '/api/nodes/' + pubkey + '/reach');
    const board = await getJson(page, '/api/reach-rank?limit=100&q=' + pubkey);
    if (reach.importance.rank_snapshot_at === board.snapshot_at) return { reach, board };
  }
  throw new Error('Reach and leaderboard never reported the same snapshot');
}

// Top node of the committed CI fixture: 10 neighbours before legacy
// empty-endpoint edges were excluded, 9 real ones after.
const FIXTURE_TOP = '1000009e310d729534b70faa33a1abe5cd5e45d594f72f786febccbb770b7e74';

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
  await installRouteHold(page);

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
    const expect = await getJson(page, '/api/reach-rank?q=' + encodeURIComponent(q));
    const expectHrefs = expect.rows.map(r => '#/nodes/' + r.pubkey + '/reach');
    // The board shows the search, both in the field and in the URL, and lists
    // exactly the API's matches for it.
    const searchShown = async (when) => {
      await page.waitForFunction(n => document.querySelectorAll('#rrRows a.nq-link').length === n &&
        !document.getElementById('rrTable').hasAttribute('aria-busy'), expect.rows.length)
        .catch(() => { throw new Error('rows never matched the search for "' + q + '" ' + when); });
      const st = await page.evaluate(() => ({ hash: location.hash, value: document.getElementById('rrSearch').value,
        hrefs: [...document.querySelectorAll('#rrRows a.nq-link')].map(a => a.getAttribute('href')) }));
      assert(st.value === q, 'search field shows ' + JSON.stringify(st.value) + ', want ' + JSON.stringify(q) + ' ' + when);
      assert(st.hash.includes('q=' + encodeURIComponent(q)), 'hash lacks the search ' + when + ': ' + st.hash);
      assert(JSON.stringify(st.hrefs) === JSON.stringify(expectHrefs), 'rows are not the search results ' + when);
    };
    // Hold the router for this navigation so acting on the old board fails
    // deterministically (see installRouteHold).
    await holdNextRoute(page);
    await openBoard(page);
    assert(await page.evaluate(() => window.__rrHoldNext === false), 'openBoard did not change route, so the held navigation never happened');
    await page.focus('#rrSearch');
    await page.keyboard.type(q);
    await page.keyboard.press('Enter');
    await searchShown('after typing it and pressing Enter');
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
    const { reach, board } = await sameSnapshot(page, target.pubkey);
    const row = board.rows.find(r => r.pubkey === target.pubkey);
    assert(row, 'target missing from leaderboard search');
    assert(reach.importance.rank_status === 'ranked', 'rank_status ' + reach.importance.rank_status);
    assert(reach.importance.degree_rank === row.rank && reach.importance.nodes_with_edges === board.total &&
      reach.importance.neighbor_degree === row.neighbors,
      'Reach #' + reach.importance.degree_rank + '/' + reach.importance.nodes_with_edges + ' (' + reach.importance.neighbor_degree +
      ') != board #' + row.rank + '/' + board.total + ' (' + row.neighbors + ')');
    const cardText = await page.$$eval('.analytics-stat-card', cs => cs.map(c => c.textContent).join('|'));
    assert(cardText.includes('#' + target.rank + ' / ' + board.total), 'Rank card text: ' + cardText);
    assert(await page.getAttribute('.nq-rank-link', 'href') === '#/reach-rank', 'View leaderboard link');
    // Back (twice, with Forward in between) must be a history traversal inside
    // the same document, not a reload, and must bring the search back.
    const reachUrl = page.url();
    const doc = await page.evaluate(() => ({ len: history.length, doc: window.__rrDoc }));
    for (let round = 1; round <= 2; round++) {
      await settleBoard(page, () => page.goBack());
      const after = await page.evaluate(() => ({ len: history.length, doc: window.__rrDoc }));
      assert(after.doc === doc.doc && after.len === doc.len, 'Back ' + round + ' reloaded the page or changed history (' + JSON.stringify(after) + ' vs ' + JSON.stringify(doc) + ')');
      await searchShown('after Back ' + round);
      assert(await page.inputValue('#rrSearch') === q, 'search restored after Back');
      if (round === 2) break;
      await page.goForward();
      await page.waitForSelector('.nq-rank-link');
      assert(page.url() === reachUrl, 'Forward returned to ' + page.url());
    }
  });

  await step('legacy empty-endpoint edges are not neighbours (fixture top node shows 9, not 10)', async () => {
    const board = await getJson(page, '/api/reach-rank?q=' + FIXTURE_TOP);
    if (!board.rows.length) { console.log('    (fixture top node not present — skipped)'); return; }
    const row = board.rows[0];
    assert(row.neighbors === 9 && row.rank === 1, 'fixture top node: #' + row.rank + ' with ' + row.neighbors + ' neighbours');
    const reach = await getJson(page, '/api/nodes/' + FIXTURE_TOP + '/reach');
    assert(reach.importance.neighbor_degree === 9, 'Reach Neighbours ' + reach.importance.neighbor_degree);
    await page.goto(BASE + '/#/nodes/' + FIXTURE_TOP + '/reach');
    await page.waitForSelector('.nq-rank-link');
    const cards = await page.$$eval('.analytics-stat-card', cs => cs.map(c => c.innerText.replace(/\s+/g, ' ')));
    assert(cards.some(t => /NEIGHBOURS 9 /i.test(t + ' ')), 'Neighbours card: ' + cards.join(' | '));
  });

  await step('ranked node without a reliable path token still shows its Rank card', async () => {
    const all = (await getJson(page, '/api/reach-rank?limit=100')).rows
      .concat((await getJson(page, '/api/reach-rank?limit=100&offset=100')).rows);
    let noToken = null;
    for (const r of all) {
      const rep = await getJson(page, '/api/nodes/' + r.pubkey + '/reach');
      if (!rep.reliable_tokens || !rep.reliable_tokens.length) { noToken = r; break; }
    }
    if (!noToken) { console.log('    (no ranked node without tokens in dataset — skipped)'); return; }
    await page.goto(BASE + '/#/nodes/' + noToken.pubkey + '/reach');
    await page.waitForSelector('.nq-msg');
    await page.waitForSelector('.nq-rank-link');
    const text = await page.$$eval('.analytics-stat-card', cs => cs.map(c => c.innerText).join(' | '));
    assert(text.includes('#' + noToken.rank + ' / '), 'no-token Rank card: ' + text);
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
