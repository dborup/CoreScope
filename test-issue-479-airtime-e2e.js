/**
 * E2E test (#479): rolling 1 h TX airtime per observer.
 *
 * The fixture DB has no observer_metrics inside the last hour, so the
 * summary endpoint is intercepted with page.route and given known values for
 * the fixture's own observer ids: one over the duty-cycle threshold, one
 * under, the rest unknown.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-issue-661-airtime-e2e.js
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

function summaryFor(ids, overId, underId) {
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
      tx_airtime_pct_1h: id === overId ? 12.5 : (id === underId ? 2 : null),
      rx_airtime_pct_1h: null,
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

  console.log(`\nRunning #479 observer airtime E2E tests against ${BASE}\n`);

  const obsResp = await page.request.get(`${BASE}/api/observers`);
  const ids = ((await obsResp.json()).observers || []).map((o) => o.id);
  assert(ids.length >= 2, `fixture needs >=2 observers, got ${ids.length}`);
  const [overId, underId] = ids;

  await page.route('**/api/observers/metrics/summary*', (route) => route.fulfill({
    status: 200,
    contentType: 'application/json',
    body: JSON.stringify(summaryFor(ids, overId, underId)),
  }));

  await test('observer list has a sortable numeric TX 1h column', async () => {
    await page.goto(`${BASE}/#/observers`, { waitUntil: 'domcontentloaded' });
    await page.evaluate(() => { try { localStorage.removeItem('meshcore-observers-sort'); } catch (_) {} });
    await page.reload({ waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#obsTable tbody tr');
    const th = await page.$('#obsTable thead th[data-sort-key="airtime_1h"]');
    assert(th, 'TX 1h <th> must carry data-sort-key="airtime_1h"');
    assert(await th.getAttribute('data-type') === 'numeric', 'TX 1h <th> must be data-type="numeric"');
  });

  await test('over-threshold observer is flagged, under-threshold is not, unknown is a dash', async () => {
    const cell = (id) => page.$eval(`#obsTable tbody tr[data-observer-id="${id}"] td[data-testid="obs-cell-airtime"]`,
      (td) => ({ html: td.innerHTML, text: td.textContent.trim(), value: td.getAttribute('data-value') }));
    const over = await cell(overId);
    assert(over.html.includes('airtime-over') && over.text === '12.5%', `over cell: ${JSON.stringify(over)}`);
    const under = await cell(underId);
    assert(!under.html.includes('airtime-over') && under.text === '2.0%', `under cell: ${JSON.stringify(under)}`);
    if (ids.length > 2) {
      const unknown = await cell(ids[2]);
      assert(unknown.text === '—' && unknown.value === '', `unknown cell: ${JSON.stringify(unknown)}`);
    }
  });

  for (const theme of ['light', 'dark']) {
    await test(`warning is styled from --danger in the ${theme} theme`, async () => {
      await page.goto(`${BASE}/#/observers`, { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('#obsTable tbody tr');
      await page.evaluate((t) => document.documentElement.setAttribute('data-theme', t), theme);
      const colors = await page.evaluate(([o, u]) => {
        const c = (id) => getComputedStyle(document.querySelector(
          `#obsTable tbody tr[data-observer-id="${id}"] td[data-testid="obs-cell-airtime"] .airtime-pct`)).color;
        const probe = document.createElement('span');
        probe.style.color = 'var(--danger)';
        document.body.appendChild(probe);
        const danger = getComputedStyle(probe).color;
        probe.remove();
        return { over: c(o), under: c(u), danger };
      }, [overId, underId]);
      assert(colors.over === colors.danger, `${theme}: over ${colors.over} != --danger ${colors.danger}`);
      assert(colors.under !== colors.over, `${theme}: under and over share colour ${colors.over}`);
    });
  }

  await test('clicking TX 1h sorts the highest share first', async () => {
    await page.click('#obsTable thead th[data-sort-key="airtime_1h"]');
    const first = await page.$eval('#obsTable tbody tr', (tr) => tr.getAttribute('data-observer-id'));
    assert(first === overId, `first row after sort = ${first}, want ${overId}`);
  });

  await test('clicking TX 1h again reverses the order of the known values', async () => {
    await page.click('#obsTable thead th[data-sort-key="airtime_1h"]');
    const rows = await page.$$eval('#obsTable tbody tr', (trs) => trs.map((tr) => ({
      id: tr.getAttribute('data-observer-id'),
      v: tr.querySelector('td[data-testid="obs-cell-airtime"]').getAttribute('data-value'),
    })));
    // Where unknown values land is TableSort's shared business; the known ones must ascend.
    const known = rows.filter((r) => r.v !== '');
    assert(known.length >= 2, `need two known values, got ${JSON.stringify(rows)}`);
    assert(known[0].id === underId && known[known.length - 1].id === overId,
      `known values ascending = ${known.map((r) => r.v)}`);
  });

  // RF Health renders its grid a second time when the page's theme-refresh
  // fires (~300 ms after load): renderTab() replaces the tab's innerHTML and
  // re-fetches the summary, so the grid is briefly empty. A tile found by
  // waitForSelector('.rf-cell') can be gone before a one-shot page.$eval runs.
  // Locator evaluation waits for the tile to be attached when it is used.
  await test('RF Health tiles show the TX 1h share with the warning', async () => {
    await page.goto(`${BASE}/#/analytics?tab=rf-health`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('.rf-cell');
    const over = await page.locator(`.rf-cell[data-observer="${overId}"] [data-testid="rf-cell-airtime"]`).evaluate(
      (el) => ({ html: el.innerHTML, text: el.textContent.trim() }));
    assert(over.html.includes('airtime-over') && over.text === 'TX 1h: 12.5%', `tile: ${JSON.stringify(over)}`);
  });

  await test('360x800: TX 1h tile stat stays inside its tile', async () => {
    await page.setViewportSize({ width: 360, height: 800 });
    await page.goto(`${BASE}/#/analytics?tab=rf-health`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('.rf-cell');
    const m = await page.locator(`.rf-cell[data-observer="${overId}"]`).evaluate((cell) => {
      const box = cell.getBoundingClientRect();
      const el = cell.querySelector('[data-testid="rf-cell-airtime"]');
      const stat = el.getBoundingClientRect();
      return { inside: stat.right <= box.right + 1 && stat.left >= box.left - 1,
               oneLine: stat.height <= parseFloat(getComputedStyle(el).fontSize) * 1.8 };
    });
    assert(m.inside, 'TX 1h stat overflows its tile at 360px');
    assert(m.oneLine, 'TX 1h stat wraps inside itself at 360px');
  });

  await browser.close();
  console.log('\nAll #479 airtime E2E tests passed.');
}

run().catch((e) => { console.error(e); process.exit(1); });
