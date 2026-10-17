/**
 * E2E test (#497): RX 1h (channel load) next to TX 1h in the observer list and
 * on the RF Health tiles, and both on the observer detail page.
 *
 * The fixture DB has no observer_metrics inside the last hour, so the summary
 * endpoint is intercepted with page.route (same pattern as
 * test-issue-479-airtime-e2e.js) and given known values.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-issue-497-rx-airtime-e2e.js
 */
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:3000';

async function test(name, fn) {
  try {
    await fn();
    console.log(`  ✅ ${name}`);
  } catch (err) {
    console.log(`  ❌ ${name}: ${err.message}`);
    process.exit(1);
  }
}

function assert(cond, msg) {
  if (!cond) throw new Error(msg || 'Assertion failed');
}

// over: TX over the limit and a high RX; edge: TX exactly at the limit;
// under: both low; everything else unknown.
function summaryFor(ids, over, edge, under) {
  const vals = {};
  vals[over] = [12.5, 17.5];
  vals[edge] = [10, 10];
  vals[under] = [2, 3.7];
  return {
    duty_cycle_warn_pct: 10,
    observers: ids.map((id) => ({
      observer_id: id,
      observer_name: id,
      current_noise_floor: -110,
      avg_noise_floor_24h: -111,
      max_noise_floor_24h: -105,
      battery_mv: null,
      sample_count: 12,
      sparkline: [-112, -110, -108],
      tx_airtime_pct_1h: vals[id] ? vals[id][0] : null,
      rx_airtime_pct_1h: vals[id] ? vals[id][1] : null,
    })),
  };
}

async function run() {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage']
  });
  const ctx = await browser.newContext({ viewport: { width: 1400, height: 900 } });
  const page = await ctx.newPage();
  page.setDefaultTimeout(15000);

  console.log(`\nRunning #497 RX 1h airtime E2E tests against ${BASE}\n`);

  const obsResp = await page.request.get(`${BASE}/api/observers`);
  const ids = ((await obsResp.json()).observers || []).map((o) => o.id);
  assert(ids.length >= 3, `fixture needs >=3 observers, got ${ids.length}`);
  const [overId, edgeId, underId] = ids;

  let summaryCalls = 0;
  let summaryDelayMs = 0; // F1: set > 0 to hold the summary response back
  await page.route('**/api/observers/metrics/summary*', async (route) => {
    summaryCalls++;
    if (summaryDelayMs > 0) await new Promise((r) => setTimeout(r, summaryDelayMs));
    return route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify(summaryFor(ids, overId, edgeId, underId)),
    });
  });

  // The detail cards are filled after the summary arrives, so wait for the value.
  const cardFilled = (which) => page.waitForFunction((w) => {
    const c = document.querySelector('[data-testid="obs-detail-' + w + '-airtime"]');
    return c && /\d%/.test(c.textContent);
  }, which);

  const rowCell = (id) => page.$eval(`#obsTable tbody tr[data-observer-id="${id}"] td[data-testid="obs-cell-rx-airtime"]`,
    (td) => ({ html: td.innerHTML, text: td.textContent.trim(), value: td.getAttribute('data-value') }));

  await test('observer list has a sortable numeric RX 1h column next to TX 1h', async () => {
    await page.goto(`${BASE}/#/observers`, { waitUntil: 'domcontentloaded' });
    await page.evaluate(() => { try { localStorage.removeItem('meshcore-observers-sort'); } catch (_) {} });
    await page.reload({ waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#obsTable tbody tr');
    const th = await page.$('#obsTable thead th[data-sort-key="rx_airtime_1h"]');
    assert(th, 'RX 1h <th> must carry data-sort-key="rx_airtime_1h"');
    assert(await th.getAttribute('data-type') === 'numeric', 'RX 1h <th> must be data-type="numeric"');
    assert((await th.textContent()).trim() === 'RX 1h', 'header text');
    assert((await th.getAttribute('title')).includes('channel load'), 'header tooltip');
    const order = await page.$$eval('#obsTable thead th', (ths) => ths.map((t) => t.getAttribute('data-sort-key')));
    assert(order.indexOf('rx_airtime_1h') === order.indexOf('airtime_1h') + 1, `RX 1h must follow TX 1h: ${order}`);
  });

  await test('RX cells show values, never a warning (even above the TX limit); unknown is a dash', async () => {
    const over = await rowCell(overId);
    assert(over.text === '17.5%' && !over.html.includes('airtime-over') && over.value === '17.5', JSON.stringify(over));
    const under = await rowCell(underId);
    assert(under.text === '3.7%' && !under.html.includes('airtime-over'), JSON.stringify(under));
    if (ids.length > 3) {
      const unknown = await rowCell(ids[3]);
      assert(unknown.text === '—' && unknown.value === '', JSON.stringify(unknown));
    }
  });

  await test('TX column keeps its warning (at the threshold too)', async () => {
    const tx = (id) => page.$eval(`#obsTable tbody tr[data-observer-id="${id}"] td[data-testid="obs-cell-airtime"]`, (td) => td.innerHTML);
    assert((await tx(overId)).includes('airtime-over'), 'over');
    assert((await tx(edgeId)).includes('airtime-over'), 'edge (10.0% at limit 10)');
    assert(!(await tx(underId)).includes('airtime-over'), 'under');
  });

  await test('clicking RX 1h sorts the highest share first, again reverses the known values', async () => {
    await page.click('#obsTable thead th[data-sort-key="rx_airtime_1h"]');
    const first = await page.$eval('#obsTable tbody tr', (tr) => tr.getAttribute('data-observer-id'));
    assert(first === overId, `first row after sort = ${first}, want ${overId}`);
    await page.click('#obsTable thead th[data-sort-key="rx_airtime_1h"]');
    const rows = await page.$$eval('#obsTable tbody tr', (trs) => trs.map((tr) => ({
      id: tr.getAttribute('data-observer-id'),
      v: tr.querySelector('td[data-testid="obs-cell-rx-airtime"]').getAttribute('data-value'),
    })));
    const known = rows.filter((r) => r.v !== '').map((r) => Number(r.v));
    assert(known.length >= 3 && known.every((v, i) => i === 0 || known[i - 1] <= v), `ascending expected: ${known}`);
  });

  await test('RF Health tiles show "RX 1h" next to "TX 1h", RX without warning', async () => {
    await page.goto(`${BASE}/#/analytics?tab=rf-health`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('.rf-cell');
    const rx = await page.$eval(`.rf-cell[data-observer="${overId}"] [data-testid="rf-cell-rx-airtime"]`,
      (el) => ({ html: el.innerHTML, text: el.textContent.trim(), prev: el.previousElementSibling && el.previousElementSibling.getAttribute('data-testid') }));
    assert(rx.text === 'RX 1h: 17.5%' && !rx.html.includes('airtime-over'), JSON.stringify(rx));
    assert(rx.prev === 'rf-cell-airtime', `RX must follow TX, got ${rx.prev}`);
  });

  await test('360x800: both tile stats stay inside the tile, each on one line', async () => {
    await page.setViewportSize({ width: 360, height: 800 });
    await page.goto(`${BASE}/#/analytics?tab=rf-health`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('.rf-cell');
    const m = await page.$eval(`.rf-cell[data-observer="${overId}"]`, (cell) => {
      const box = cell.getBoundingClientRect();
      return ['rf-cell-airtime', 'rf-cell-rx-airtime'].map((tid) => {
        const el = cell.querySelector(`[data-testid="${tid}"]`);
        const r = el.getBoundingClientRect();
        return { tid, inside: r.right <= box.right + 1 && r.left >= box.left - 1,
                 oneLine: r.height <= parseFloat(getComputedStyle(el).fontSize) * 1.8 };
      });
    });
    m.forEach((s) => assert(s.inside && s.oneLine, `${s.tid}: ${JSON.stringify(s)}`));
  });

  await test('a very narrow tile wraps between stats: no stat overflows or breaks inside itself', async () => {
    await page.goto(`${BASE}/#/analytics?tab=rf-health`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('.rf-cell');
    const m = await page.$eval(`.rf-cell[data-observer="${overId}"]`, (cell) => {
      cell.style.width = '130px';
      cell.style.boxSizing = 'border-box';
      const box = cell.getBoundingClientRect();
      const stats = Array.from(cell.querySelectorAll('.rf-cell-stats > span'));
      return stats.map((el) => {
        const r = el.getBoundingClientRect();
        return { tid: el.getAttribute('data-testid') || el.textContent.trim(),
                 nowrap: getComputedStyle(el).whiteSpace === 'nowrap',
                 inside: r.right <= box.right + 1,
                 oneLine: r.height <= parseFloat(getComputedStyle(el).fontSize) * 1.8 };
      });
    });
    assert(m.length >= 5, `expected >=5 stats in the tile, got ${m.length}`);
    m.forEach((x) => assert(x.inside && x.oneLine && x.nowrap, `stat ${x.tid}: ${JSON.stringify(x)}`));
  });

  await page.setViewportSize({ width: 1400, height: 900 });

  await test('observer detail shows TX 1h with the warning and RX 1h neutral', async () => {
    await page.goto(`${BASE}/#/observers/${encodeURIComponent(overId)}`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('[data-testid="obs-detail-tx-airtime"]');
    await cardFilled('tx');
    const tx = await page.$eval('[data-testid="obs-detail-tx-airtime"]', (c) => ({ html: c.innerHTML, text: c.textContent }));
    const rx = await page.$eval('[data-testid="obs-detail-rx-airtime"]', (c) => ({ html: c.innerHTML, text: c.textContent }));
    assert(tx.text.includes('TX 1h') && tx.text.includes('12.5%') && tx.html.includes('airtime-over'), JSON.stringify(tx));
    assert(rx.text.includes('RX 1h') && rx.text.includes('17.5%') && !rx.html.includes('airtime-over'), JSON.stringify(rx));
  });

  await test('detail: TX exactly at the threshold warns; below does not; unknown is a dash', async () => {
    const stat = async (id, which) => {
      await page.goto(`${BASE}/#/observers/${encodeURIComponent(id)}`, { waitUntil: 'domcontentloaded' });
      await page.waitForSelector(`[data-testid="obs-detail-${which}-airtime"]`);
      if (id !== ids[3]) await cardFilled(which); // the unknown observer stays a dash
      else await page.waitForTimeout(500);
      return page.$eval(`[data-testid="obs-detail-${which}-airtime"]`, (c) => ({ html: c.innerHTML, text: c.textContent }));
    };
    assert((await stat(edgeId, 'tx')).html.includes('airtime-over'), 'edge');
    const under = await stat(underId, 'tx');
    assert(!under.html.includes('airtime-over') && under.text.includes('2.0%'), JSON.stringify(under));
    if (ids.length > 3) {
      const unknown = await stat(ids[3], 'rx');
      assert(unknown.text.includes('—'), JSON.stringify(unknown));
    }
  });

  await test('detail: no extra summary request within the TTL (shared with the list through the client cache)', async () => {
    await page.goto(`${BASE}/#/observers`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#obsTable tbody tr');
    const before = summaryCalls;
    await page.goto(`${BASE}/#/observers/${encodeURIComponent(overId)}`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('[data-testid="obs-detail-tx-airtime"]');
    assert(summaryCalls === before, `detail page issued ${summaryCalls - before} extra summary requests (list loaded it within the 30 s TTL)`);
  });


  // F1 (review round 2): the detail page must not wait for the slow summary.
  await test('F1: detail renders at once with dashes while the summary is slow, then fills both cards', async () => {
    await page.evaluate(() => { try { sessionStorage.clear(); } catch (_) {} });
    await page.goto(`${BASE}/#/observers`, { waitUntil: 'domcontentloaded' });
    await page.waitForTimeout(100);
    // Past the 30 s client TTL is not reachable here, so bust the cache by reloading the SPA.
    summaryDelayMs = 4000;
    await page.goto('about:blank');
    await page.goto(`${BASE}/#/observers/${encodeURIComponent(overId)}`, { waitUntil: 'domcontentloaded' });
    const t0 = Date.now();
    await page.waitForSelector('[data-testid="obs-detail-tx-airtime"]', { timeout: 2500 });
    const early = Date.now() - t0;
    assert(early < 2500, `cards took ${early} ms with a 4000 ms summary`);
    const dash = await page.$$eval('[data-testid="obs-detail-tx-airtime"], [data-testid="obs-detail-rx-airtime"]',
      (cs) => cs.map((c) => c.textContent));
    assert(dash.length === 2 && dash.every((t) => t.includes('—') && !/\d%/.test(t)), 'placeholder dashes expected: ' + JSON.stringify(dash));
    const others = await page.$eval('#obsDetailContent', (el) => /Uptime/.test(el.textContent) && /Total Packets/.test(el.textContent));
    assert(others, 'the rest of the detail page must be rendered while the summary is pending');
    await cardFilled('tx');
    await cardFilled('rx');
    const late = await page.$eval('[data-testid="obs-detail-rx-airtime"]', (c) => c.textContent);
    assert(late.includes('17.5%'), 'RX filled after the summary: ' + late);
    summaryDelayMs = 0;
  });

  await test('F1: a late summary for observer A never lands in observer B\'s cards', async () => {
    summaryDelayMs = 3000;
    await page.goto('about:blank');
    await page.goto(`${BASE}/#/observers/${encodeURIComponent(overId)}`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('[data-testid="obs-detail-tx-airtime"]');
    // Record every card node ever inserted from here on.
    await page.evaluate(() => {
      window.__cardTexts = [];
      new MutationObserver((recs) => recs.forEach((r) => r.addedNodes.forEach((n) => {
        if (n.nodeType !== 1) return;
        const cards = n.matches && n.matches('[data-testid^="obs-detail-"]') ? [n] : Array.from(n.querySelectorAll('[data-testid^="obs-detail-"]'));
        cards.forEach((c) => window.__cardTexts.push(c.textContent));
      }))).observe(document.getElementById('app') || document.body, { childList: true, subtree: true });
    });
    await page.evaluate((id) => { location.hash = '#/observers/' + encodeURIComponent(id); }, underId);
    await page.waitForFunction((id) => document.getElementById('obsDetailContent') &&
      /\d/.test(document.getElementById('obsDetailContent').textContent), underId);
    await cardFilled('tx');
    await page.waitForTimeout(500);
    const texts = await page.evaluate(() => window.__cardTexts);
    assert(!texts.some((t) => t.includes('12.5%') || t.includes('17.5%')), 'observer A values leaked into B: ' + JSON.stringify(texts));
    const tx = await page.$eval('[data-testid="obs-detail-tx-airtime"]', (c) => c.textContent);
    assert(tx.includes('2.0%'), 'B shows its own TX: ' + tx);
    summaryDelayMs = 0;
  });

  for (const theme of ['light', 'dark']) {
    await test(`detail: TX warning uses --danger, RX does not, in the ${theme} theme`, async () => {
      await page.goto(`${BASE}/#/observers/${encodeURIComponent(overId)}`, { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('[data-testid="obs-detail-tx-airtime"]');
      await cardFilled('tx');
      await page.evaluate((t) => document.documentElement.setAttribute('data-theme', t), theme);
      const c = await page.evaluate(() => {
        const col = (tid) => getComputedStyle(document.querySelector(`[data-testid="${tid}"] .airtime-pct`)).color;
        const probe = document.createElement('span');
        probe.style.color = 'var(--danger)';
        document.body.appendChild(probe);
        const danger = getComputedStyle(probe).color;
        probe.remove();
        return { tx: col('obs-detail-tx-airtime'), rx: col('obs-detail-rx-airtime'), danger };
      });
      assert(c.tx === c.danger, `${theme}: tx ${c.tx} != --danger ${c.danger}`);
      assert(c.rx !== c.danger, `${theme}: rx must not use --danger`);
    });
  }

  await test('360x800 detail: both stat cards fit the viewport', async () => {
    await page.setViewportSize({ width: 360, height: 800 });
    await page.goto(`${BASE}/#/observers/${encodeURIComponent(overId)}`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('[data-testid="obs-detail-tx-airtime"]');
    const m = await page.evaluate(() => ['obs-detail-tx-airtime', 'obs-detail-rx-airtime'].map((tid) => {
      const r = document.querySelector(`[data-testid="${tid}"]`).getBoundingClientRect();
      return { tid, inside: r.left >= 0 && r.right <= window.innerWidth + 1 };
    }));
    m.forEach((s) => assert(s.inside, `${s.tid} overflows at 360px`));
  });

  await browser.close();
  console.log('\nAll #497 RX 1h airtime E2E tests passed.');
}

run().catch((e) => { console.error(e); process.exit(1); });
