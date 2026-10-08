#!/usr/bin/env node
/**
 * E2E (#226): the Hash Stats multi-byte adopters table sorts by the clicked
 * column, in both directions, keeps the sort across a filter click and a
 * reload, and deep-links it as ?mbsort= and ?mbdir=.
 *
 * Before #226 each header read the cell to its left (the column map missed
 * Role), Last Seen compared "5m ago" text, the sort was ascending only, had
 * no indicator and was lost at the next filter click.
 *
 * The checks are on the table's own cells: each column must be in order
 * after its header is clicked. Last Seen is checked against the adopters'
 * timestamps from /api/analytics/hash-sizes, not against the "x ago" text.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-issue-226-hash-stats-sort-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const COLS = ['name', 'role', 'status', 'hashSize', 'packets', 'lastSeen'];

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

const hash = (page) => page.evaluate(() => location.hash);
// The hash keys as a sorted list, for checks where the key order follows the
// click order.
const hashKeys = async (page) => (await hash(page)).split('?')[1].split('&').sort().join('&');

async function coldLoad(page, path) {
  await page.goto('about:blank');
  await page.goto(BASE + '/' + path, { waitUntil: 'load' });
  await settle(page);
}

// The tab renders again on the first theme refresh; the card wires its click
// handler 100 ms after each render.
async function settle(page) {
  await page.waitForFunction(() => window.__themeRefreshed, null, { timeout: 8000 }).catch(() => {});
  await page.waitForSelector('#mbAdoptersTable tbody tr', { timeout: 15000 });
  await page.waitForTimeout(300);
}

// The table as the visitor sees it: per header its sort key, aria-sort and
// text; per row its pubkey and cell texts.
function readTable(page) {
  return page.evaluate(() => {
    const t = document.getElementById('mbAdoptersTable');
    if (!t) return null;
    const headers = Array.from(t.querySelectorAll('thead th')).map((th) => ({
      col: th.dataset.sort, ariaSort: th.getAttribute('aria-sort'), text: th.textContent.trim(),
    }));
    const rows = Array.from(t.querySelectorAll('tbody tr')).map((tr) => ({
      pubkey: decodeURIComponent((tr.dataset.value || '').replace('#/nodes/', '')),
      cells: Array.from(tr.children).map((td) => td.textContent.trim()),
    }));
    return { headers, rows };
  });
}

// The comparable value of a cell of a column; Last Seen comes from the API
// timestamp of the row's node.
function cellValue(col, row, idx, lastSeenByPk) {
  const text = row.cells[idx];
  switch (col) {
    case 'name': case 'role': return text.toLowerCase();
    case 'status': return { confirmed: 0, suspected: 1, unknown: 2 }[text.toLowerCase().split(' ').pop()];
    case 'hashSize': case 'packets': return parseInt(text, 10);
    case 'lastSeen': {
      const iso = lastSeenByPk[row.pubkey];
      return iso ? Date.parse(iso) : NaN;
    }
  }
  throw new Error('column ' + col);
}

function assertOrdered(table, col, dir, lastSeenByPk) {
  const idx = table.headers.findIndex((h) => h.col === col);
  assert(idx >= 0, 'no header for ' + col);
  const vals = table.rows.map((r) => cellValue(col, r, idx, lastSeenByPk));
  const known = vals.filter((v) => !(typeof v === 'number' && isNaN(v)));
  // Rows without a value come last.
  assert(vals.slice(0, known.length).every((v) => !(typeof v === 'number' && isNaN(v))), col + ': a missing value is not last');
  for (let i = 1; i < known.length; i++) {
    const ok = dir === 'asc' ? known[i - 1] <= known[i] : known[i - 1] >= known[i];
    assert(ok, col + ' ' + dir + ' out of order at row ' + i + ': ' + JSON.stringify(known.slice(Math.max(0, i - 2), i + 2)));
  }
  const marked = table.headers.filter((h) => h.ariaSort === 'ascending' || h.ariaSort === 'descending');
  assert(marked.length === 1 && marked[0].col === col, 'aria-sort on ' + JSON.stringify(marked.map((h) => h.col)));
  assert(marked[0].ariaSort === (dir === 'asc' ? 'ascending' : 'descending'), 'aria-sort ' + marked[0].ariaSort);
  assert(marked[0].text.indexOf(dir === 'asc' ? '↑' : '↓') >= 0, 'arrow missing: ' + marked[0].text);
}

async function clickHeader(page, col) {
  await page.click('#mbAdoptersTable thead th[data-sort="' + col + '"]');
}

(async () => {
  const requireChromium = process.env.CHROMIUM_REQUIRE === '1';
  let browser;
  try {
    browser = await chromium.launch({
      headless: true,
      executablePath: process.env.CHROMIUM_PATH || undefined,
      args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
    });
  } catch (err) {
    if (requireChromium) {
      console.error('test-issue-226-hash-stats-sort-e2e.js: FAIL — Chromium required but unavailable: ' + err.message);
      process.exit(1);
    }
    console.log('test-issue-226-hash-stats-sort-e2e.js: SKIP (Chromium unavailable: ' + err.message.split('\n')[0] + ')');
    process.exit(0);
  }

  const ctx = await browser.newContext({ viewport: { width: 1400, height: 1000 } });
  await ctx.addInitScript(() => {
    window.addEventListener('theme-refresh', () => { window.__themeRefreshed = true; }, { once: true });
  });
  const page = await ctx.newPage();
  page.setDefaultTimeout(15000);
  const pageErrors = [];
  page.on('pageerror', (e) => { pageErrors.push(e.message); console.error('[pageerror]', e.message); });

  console.log('\n=== #226 Hash Stats adopters sort E2E against ' + BASE + ' ===');

  const api = await (await page.request.get(BASE + '/api/analytics/hash-sizes')).json();
  const nodes = api.multiByteNodes || [];
  const lastSeenByPk = Object.fromEntries(nodes.map((n) => [n.pubkey, n.lastSeen]));
  const serverOrder = nodes.map((n) => n.pubkey);

  await step('precondition: the fixture has adopters whose Last Seen and names differ', async () => {
    assert(nodes.length >= 3, nodes.length + ' adopters');
    assert(new Set(nodes.map((n) => n.lastSeen)).size >= 3, 'Last Seen values are all equal');
    assert(new Set(nodes.map((n) => n.name)).size >= 3, 'names are all equal');
  });

  await step('a plain visit keeps the server order and marks no column', async () => {
    await coldLoad(page, '#/analytics?tab=hashsizes');
    const t = await readTable(page);
    assert(JSON.stringify(t.rows.map((r) => r.pubkey)) === JSON.stringify(serverOrder), 'not the server order');
    assert(t.headers.every((h) => h.ariaSort === 'none'), 'unexpected active aria-sort: ' + JSON.stringify(t.headers));
    assert(await hash(page) === '#/analytics?tab=hashsizes', 'hash ' + await hash(page));
  });

  for (const col of COLS) {
    await step('clicking ' + col + ' uses its type default, then toggles; the URL follows', async () => {
      await coldLoad(page, '#/analytics?tab=hashsizes');
      const first = ['name', 'role', 'status'].includes(col) ? 'asc' : 'desc';
      for (const dir of [first, first === 'asc' ? 'desc' : 'asc']) {
        await clickHeader(page, col);
        assertOrdered(await readTable(page), col, dir, lastSeenByPk);
        assert(await hash(page) === '#/analytics?tab=hashsizes&mbsort=' + col + (dir === 'desc' ? '&mbdir=desc' : ''), 'hash ' + await hash(page));
      }
    });
  }

  await step('Last Seen is not sorted by its "x ago" text', async () => {
    await coldLoad(page, '#/analytics?tab=hashsizes');
    await clickHeader(page, 'lastSeen');
    const t = await readTable(page);
    const idx = t.headers.findIndex((h) => h.col === 'lastSeen');
    const texts = t.rows.map((r) => r.cells[idx]);
    const ts = t.rows.map((r) => Date.parse(lastSeenByPk[r.pubkey]));
    assert(ts.every((v, i) => i === 0 || ts[i - 1] >= v), 'timestamps not descending');
    // The fixture spans more than one unit ("…m ago", "…h ago"), so text order would differ.
    const textSorted = texts.slice().sort();
    assert(JSON.stringify(textSorted) !== JSON.stringify(texts) || new Set(texts).size < 2, 'text order equals time order; the check proves nothing on this fixture');
  });

  await step('a filter click keeps the sort; All brings back every row in the same order', async () => {
    await coldLoad(page, '#/analytics?tab=hashsizes');
    await clickHeader(page, 'lastSeen');
    await page.click('#mbCapFilters [data-mb-filter="confirmed"]');
    const t = await readTable(page);
    assert(t.rows.length > 0, 'no confirmed rows');
    assertOrdered(t, 'lastSeen', 'desc', lastSeenByPk);
    assert(await hashKeys(page) === 'mbdir=desc&mbf=confirmed&mbsort=lastSeen&tab=hashsizes', 'hash ' + await hash(page));
    await page.click('#mbCapFilters [data-mb-filter="all"]');
    const all = await readTable(page);
    assert(all.rows.length === nodes.length, all.rows.length + ' rows under All');
    assertOrdered(all, 'lastSeen', 'desc', lastSeenByPk);
    assert(await hashKeys(page) === 'mbdir=desc&mbsort=lastSeen&tab=hashsizes', 'hash ' + await hash(page));
  });

  await step('reload keeps the sort and the filter', async () => {
    await page.click('#mbCapFilters [data-mb-filter="confirmed"]');
    await page.reload({ waitUntil: 'load' });
    await settle(page);
    assertOrdered(await readTable(page), 'lastSeen', 'desc', lastSeenByPk);
    const active = await page.evaluate(() => Array.from(document.querySelectorAll('#mbCapFilters [data-mb-filter].active')).map((b) => b.dataset.mbFilter));
    assert(JSON.stringify(active) === '["confirmed"]', 'active filter ' + JSON.stringify(active));
  });

  await step('cold load of #/analytics?tab=hashsizes&mbsort=name&mbdir=desc opens sorted, URL unchanged', async () => {
    await coldLoad(page, '#/analytics?tab=hashsizes&mbsort=name&mbdir=desc');
    assertOrdered(await readTable(page), 'name', 'desc', lastSeenByPk);
    assert(await hash(page) === '#/analytics?tab=hashsizes&mbsort=name&mbdir=desc', 'hash ' + await hash(page));
  });

  await step('a hostile ?mbsort= keeps the server order, canonical URL, no page error', async () => {
    const before = pageErrors.length;
    await coldLoad(page, '#/analytics?tab=hashsizes&mbsort=' + encodeURIComponent('x"],[data-sort="name') + '&mbdir=desc');
    const t = await readTable(page);
    assert(JSON.stringify(t.rows.map((r) => r.pubkey)) === JSON.stringify(serverOrder), 'not the server order');
    assert(await hash(page) === '#/analytics?tab=hashsizes', 'hash not canonical: ' + await hash(page));
    assert(pageErrors.length === before, 'page errors: ' + pageErrors.slice(before).join(' | '));
  });

  await step('switching to another tab drops mbsort= and mbdir=', async () => {
    await coldLoad(page, '#/analytics?tab=hashsizes&mbsort=packets&mbdir=desc');
    await page.click('#analyticsTabs [data-tab="topology"]');
    await page.waitForFunction(() => location.hash === '#/analytics?tab=topology', null, { timeout: 5000 })
      .catch(async () => { throw new Error('after the tab switch: ' + await hash(page)); });
  });

  if (process.env.SCREENSHOT) {
    await coldLoad(page, '#/analytics?tab=hashsizes&mbsort=lastSeen&mbdir=desc');
    const card = await page.$('#mbAdoptersSection');
    if (card) await card.screenshot({ path: process.env.SCREENSHOT });
  }

  await step('no page errors', async () => {
    assert(pageErrors.length === 0, pageErrors.join(' | '));
  });

  await browser.close();

  console.log('\n' + passed + ' passed, ' + failed + ' failed');
  if (failed > 0) {
    console.error('test-issue-226-hash-stats-sort-e2e.js: FAIL');
    process.exit(1);
  }
  console.log('test-issue-226-hash-stats-sort-e2e.js: PASS');
})();
