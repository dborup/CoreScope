#!/usr/bin/env node
/**
 * E2E: Analytics → Repeater Metrics → Busiest Repeaters.
 *
 * The table ranks repeaters and rooms by relay_count_24h (or _1h) from
 * /api/nodes, hides repeaters that are also observers unless asked, shows
 * the top 50 with a "Show all" button, and keeps sort, observer choice and
 * limit in the URL (bsort=, bobs=, blimit=).
 *
 * #697 M2 adds the seen-airtime columns (a lower bound, shown as "≥") and a busiest-hour sort
 * (bsort=peak) from est_airtime_pct_24h / est_airtime_peak_pct.
 *
 * Every check reads the table's own cells and compares them with the API
 * rows the page itself fetches, so a wrong order, a missing filter or a
 * stale URL fails here.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-busiest-repeaters-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const TOP = 50;

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

const hashParams = (page) => page.evaluate(() => Object.fromEntries(new URLSearchParams(location.hash.split('?')[1] || '')));

// The tab renders again on the first theme refresh; wait for that, then for
// the table (or its empty state).
async function open(page, query) {
  await page.goto('about:blank');
  await page.goto(BASE + '/#/analytics?tab=repeater-metrics' + (query || ''), { waitUntil: 'load' });
  await page.waitForFunction(() => window.__themeRefreshed, null, { timeout: 8000 }).catch(() => {});
  await page.waitForSelector('#busiestRepeatersTable table, #busiestRepeatersTable .text-muted', { timeout: 30000 });
  await page.waitForTimeout(300);
}

// Rows as shown: pubkey from the link, the two counts, the observer tag.
function readRows(page) {
  return page.evaluate(() => Array.from(document.querySelectorAll('#busiestRepeatersTable tbody tr')).map((tr) => {
    const a = tr.querySelector('a');
    const href = a ? a.getAttribute('href') : '';
    return {
      pk: decodeURIComponent(href.replace('#/nodes/', '').replace('/analytics', '')),
      relay24h: parseInt(tr.children[2].textContent.replace(/\D/g, ''), 10),
      relay1h: parseInt(tr.children[3].textContent.replace(/\D/g, ''), 10),
      observer: /observer/.test(tr.children[1].textContent),
    };
  }));
}

// What the table should hold, from the same endpoints the tab uses.
function expected(page, sortKey, includeObservers) {
  return page.evaluate(async ({ sortKey, includeObservers }) => {
    // fetchAllNodes pages through /api/nodes like the tab does.
    const [nodesResp, obsResp] = await Promise.all([
      window.fetchAllNodes('&sortBy=lastSeen'),
      fetch('/api/observers').then((r) => r.json()),
    ]);
    const nodes = nodesResp.nodes || nodesResp;
    const obs = new Set((obsResp.observers || []).map((o) => String(o.id || '').toLowerCase()));
    const k = { '1h': 'relay_count_1h', peak: 'est_airtime_peak_pct' }[sortKey] || 'relay_count_24h';
    const rows = nodes
      .filter((n) => (n.role === 'repeater' || n.role === 'room') && Number(n.relay_count_24h) > 0)
      .map((n) => ({ pk: n.public_key, v: Number(n[k]) || 0, observer: obs.has(String(n.public_key).toLowerCase()) }));
    return {
      total: rows.filter((r) => includeObservers || !r.observer).length,
      observerCount: rows.filter((r) => r.observer).length,
      byPk: Object.fromEntries(rows.map((r) => [r.pk, r])),
    };
  }, { sortKey, includeObservers });
}

function assertOrdered(rows, key) {
  for (let i = 1; i < rows.length; i++) {
    assert(rows[i - 1][key] >= rows[i][key], `row ${i + 1} (${rows[i][key]}) ranks below row ${i} (${rows[i - 1][key]}) by ${key}`);
  }
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 900 } });
  const page = await ctx.newPage();
  page.setDefaultTimeout(30000);
  const errors = [];
  page.on('pageerror', (e) => errors.push(String(e.message)));

  console.log(`\nBusiest Repeaters E2E against ${BASE}\n`);

  await step('default: repeaters ranked by 24h relays, observer repeaters hidden, top 50', async () => {
    await open(page);
    const rows = await readRows(page);
    const exp = await expected(page, '24h', false);
    assert(exp.total > 0, 'fixture has no repeater with relays in the last 24 h');
    assert(rows.length === Math.min(TOP, exp.total), `expected ${Math.min(TOP, exp.total)} rows, got ${rows.length}`);
    assertOrdered(rows, 'relay24h');
    rows.forEach((r) => {
      const e = exp.byPk[r.pk];
      assert(e, r.pk + ' is listed but is not a repeater with relays');
      assert(!e.observer && !r.observer, r.pk + ' is an observer repeater and should be hidden');
      assert(e.v === r.relay24h, `${r.pk}: table says ${r.relay24h}, API says ${e.v}`);
    });
    const label = await page.textContent('label:has(#busiestObservers)');
    assert(label.includes('(' + exp.observerCount + ')'), `checkbox label "${label.trim()}" should count ${exp.observerCount}`);
    const p = await hashParams(page);
    assert(!p.bsort && !p.bobs && !p.blimit, 'defaults stay out of the URL: ' + JSON.stringify(p));
  });

  await step('sorting by 1h re-ranks and writes bsort=1h', async () => {
    await page.selectOption('#busiestSort', '1h');
    const rows = await readRows(page);
    assertOrdered(rows, 'relay1h');
    assert((await hashParams(page)).bsort === '1h', 'bsort=1h missing from the URL');
  });

  await step('including observer repeaters lists them, tagged, and writes bobs=show', async () => {
    await page.check('#busiestObservers');
    const exp = await expected(page, '1h', true);
    const rows = await readRows(page);
    assert(rows.length === Math.min(TOP, exp.total), `expected ${Math.min(TOP, exp.total)} rows, got ${rows.length}`);
    assertOrdered(rows, 'relay1h');
    rows.forEach((r) => assert(r.observer === exp.byPk[r.pk].observer, r.pk + ' observer tag does not match /api/observers'));
    assert((await hashParams(page)).bobs === 'show', 'bobs=show missing from the URL');
  });

  await step('a deep link restores sort, observer choice and limit', async () => {
    await open(page, '&bsort=1h&bobs=show&blimit=all');
    assert(await page.inputValue('#busiestSort') === '1h', 'sort select not on 1h');
    assert(await page.isChecked('#busiestObservers'), 'observer checkbox not checked');
    const exp = await expected(page, '1h', true);
    const rows = await readRows(page);
    assert(rows.length === exp.total, `blimit=all should list all ${exp.total}, got ${rows.length}`);
    assertOrdered(rows, 'relay1h');
  });

  await step('the limit button switches between all rows and the top 50', async () => {
    const exp = await expected(page, '1h', true);
    if (exp.total <= TOP) {
      assert(!(await page.$('#busiestLimitToggle')), 'no limit button when every row fits');
      return;
    }
    await page.click('#busiestLimitToggle');
    assert((await readRows(page)).length === TOP, 'top 50 after the button');
    assert(!(await hashParams(page)).blimit, 'blimit dropped from the URL at the default');
    await page.click('#busiestLimitToggle');
    assert((await readRows(page)).length === exp.total, 'all rows again');
    assert((await hashParams(page)).blimit === 'all', 'blimit=all back in the URL');
  });

  await step('sorting by the busiest hour ranks by the estimate and flags the 10% limit', async () => {
    await open(page);
    await page.selectOption('#busiestSort', 'peak');
    assert((await hashParams(page)).bsort === 'peak', 'bsort=peak missing from the URL');
    const exp = await expected(page, 'peak', false);
    const rows = await readRows(page);
    const est = rows.map((r) => exp.byPk[r.pk].v);
    assert(est.some((v) => v > 0), 'fixture repeaters have no estimated airtime');
    for (let i = 1; i < est.length; i++) {
      assert(est[i - 1] >= est[i], `row ${i + 1} (${est[i]}%) ranks below row ${i} (${est[i - 1]}%) by busiest hour`);
    }
    const cells = await page.evaluate(() => Array.from(document.querySelectorAll('#busiestRepeatersTable tbody tr'))
      .map((tr) => ({ text: tr.children[5].textContent.trim(), over: !!tr.children[5].querySelector('.airtime-over') })));
    cells.forEach((c, i) => {
      assert(/^(<0\.1|0\.0|≥\d+\.\d)%$/.test(c.text), `row ${i + 1} busiest hour "${c.text}" is not a lower-bound percentage`);
      const over = Math.round(est[i] * 10) / 10 >= 10;
      assert(c.over === over, `row ${i + 1} ${est[i]}%: flagged=${c.over}, want ${over}`);
    });
  });

  await step('another tab drops bsort, bobs and blimit from the URL', async () => {
    await page.click('.tab-btn[data-tab="topology"]');
    await page.waitForFunction(() => !/[?&]tab=repeater-metrics/.test(location.hash));
    const p = await hashParams(page);
    assert(!p.bsort && !p.bobs && !p.blimit, 'leftover params: ' + JSON.stringify(p));
  });

  await step('phone width: all six columns fit, the page does not scroll sideways', async () => {
    await page.setViewportSize({ width: 375, height: 812 });
    await open(page);
    const m = await page.evaluate(() => {
      const t = document.querySelector('[data-testid="busiest-repeaters-table"]');
      const last = t.querySelector('thead tr:last-child th:last-child').getBoundingClientRect();
      return { over: document.documentElement.scrollWidth - window.innerWidth, table: t.getBoundingClientRect().width, box: t.parentElement.clientWidth, lastRight: last.right, vw: window.innerWidth };
    });
    assert(m.over <= 1, `page scrolls ${m.over}px sideways`);
    assert(m.table <= m.box + 1, `table ${m.table}px is wider than its ${m.box}px box`);
    assert(m.lastRight <= m.vw, `the last column ends at ${m.lastRight}px, past the ${m.vw}px screen`);
  });

  await step('no page errors', async () => {
    assert(errors.length === 0, errors.join(' | '));
  });

  await browser.close();
  console.log(`\nBusiest Repeaters E2E: ${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error('  ✗ Busiest Repeaters E2E:', e.stack || e); process.exit(1); });
