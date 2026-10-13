/**
 * E2E test (#1640): Observer comparison must be a first-class IA citizen.
 *
 * Asserts THREE new entry points to `#/compare`, beyond the pre-existing
 * 🔍 button on the observers page header:
 *
 *   (A) Observers page header — a labeled button reading "Compare observers"
 *       (text + icon, NOT a bare emoji).
 *   (B) Observer-detail page — a "Compare with…" affordance that opens
 *       #/compare?a=<this>&b=<picked> pre-populated.
 *   (D) Multi-select on observers table — checkbox-per-row, enabling a
 *       "Compare selected" button once exactly two observers are checked.
 *
 * Also asserts:
 *   - The compare page renders breadcrumb links back to BOTH observer
 *     detail pages.
 *   - The legacy deep-link `#/compare?a=...&b=...` continues to work.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-issue-1640-compare-discovery-e2e.js
 */
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:3000';

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  \u2705 ' + name); }
  catch (e) { failed++; console.error('  \u274c ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

async function pickTwoObserverIds(page) {
  await page.goto(BASE + '/#/observers', { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#obsTable tbody tr', { timeout: 15000 });
  const ids = await page.$$eval('#obsTable tbody tr[data-value]', rows =>
    rows.slice(0, 2).map(r => decodeURIComponent(
      (r.getAttribute('data-value') || '').replace('#/observers/', '')
    ))
  );
  assert(ids.length === 2, 'need at least 2 observers in fixture, got ' + ids.length);
  return ids;
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
  page.on('pageerror', e => console.error('  pageerror:', e.message));

  console.log('\nRunning #1640 compare-discovery E2E tests against ' + BASE + '\n');

  const [idA, idB] = await pickTwoObserverIds(page);

  // ── Entry point A: labeled "Compare observers" on observers page ──
  await step('(A) Observers page header has labeled "Compare observers" button', async () => {
    await page.goto(BASE + '/#/observers', { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#obsTable tbody tr', { timeout: 15000 });
    const btn = await page.$('[data-action="compare-observers"]');
    assert(btn, 'expected element with data-action="compare-observers" in observers page header');
    const text = (await btn.textContent() || '').trim();
    assert(/compare/i.test(text),
      'compare button must have visible text mentioning "Compare", got "' + text + '"');
  });

  await step('(A) Clicking "Compare observers" navigates to #/compare', async () => {
    await page.goto(BASE + '/#/observers', { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('[data-action="compare-observers"]', { timeout: 10000 });
    await page.click('[data-action="compare-observers"]');
    await page.waitForFunction(() => location.hash.startsWith('#/compare'), null, { timeout: 5000 });
    assert(/^#\/compare/.test(await page.evaluate(() => location.hash)),
      'expected hash to become #/compare');
  });

  // ── Entry point B: observer-detail "Compare with…" picker ──
  await step('(B) Observer detail page exposes a "Compare with…" affordance', async () => {
    await page.goto(BASE + '/#/observers/' + idA, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#obsTitle', { timeout: 15000 });
    // Give detail page a moment to finish rendering.
    await page.waitForSelector('[data-action="compare-with-picker"]', { timeout: 10000 });
    const picker = await page.$('[data-action="compare-with-picker"]');
    assert(picker, 'expected [data-action="compare-with-picker"] (select) on observer-detail');
    const options = await picker.$$('option');
    assert(options.length >= 2, 'compare-with picker should be populated with other observers');
  });

  await step('(B) Picking another observer + Compare navigates to #/compare?a=<idA>&b=<other>', async () => {
    await page.goto(BASE + '/#/observers/' + idA, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('[data-action="compare-with-picker"]', { timeout: 10000 });
    await page.selectOption('[data-action="compare-with-picker"]', idB);
    await page.click('[data-action="compare-with-go"]');
    await page.waitForFunction((idA) =>
      location.hash.indexOf('#/compare') === 0 &&
      location.hash.indexOf('a=' + idA) >= 0, idA, { timeout: 5000 });
    const h = await page.evaluate(() => location.hash);
    assert(h.indexOf('b=' + idB) >= 0, 'expected deep-link to carry b=<picked>, got: ' + h);
  });

  // ── Entry point D: multi-select on observers table ──
  await step('(D) Observers table renders one checkbox per row + "Compare selected" button', async () => {
    await page.goto(BASE + '/#/observers', { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#obsTable tbody tr', { timeout: 15000 });
    const boxes = await page.$$('#obsTable tbody input[type="checkbox"][data-compare-select]');
    assert(boxes.length >= 2,
      'expected per-row checkboxes ([data-compare-select]); got ' + boxes.length);
    const btn = await page.$('[data-action="compare-selected"]');
    assert(btn, 'expected [data-action="compare-selected"] button');
    const disabled = await btn.evaluate(el => el.disabled || el.getAttribute('aria-disabled') === 'true');
    assert(disabled, '"Compare selected" must be disabled when 0 rows are selected');
  });

  await step('(D) Selecting exactly two rows enables "Compare selected" and navigates correctly', async () => {
    await page.goto(BASE + '/#/observers', { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#obsTable tbody tr', { timeout: 15000 });
    const boxes = await page.$$('#obsTable tbody input[type="checkbox"][data-compare-select]');
    await boxes[0].check();
    await boxes[1].check();
    const btn = await page.$('[data-action="compare-selected"]');
    const stillDisabled = await btn.evaluate(el => el.disabled || el.getAttribute('aria-disabled') === 'true');
    assert(!stillDisabled, '"Compare selected" must be enabled when exactly 2 rows are checked');
    await btn.click();
    await page.waitForFunction(() => location.hash.indexOf('#/compare?') === 0, null, { timeout: 5000 });
    const h = await page.evaluate(() => location.hash);
    assert(/a=[^&]+&b=[^&]+/.test(h),
      'expected hash to carry both ?a=&b= deep-link params, got: ' + h);
  });

  await step('(D) Selecting a third row disables "Compare selected" again', async () => {
    await page.goto(BASE + '/#/observers', { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#obsTable tbody tr', { timeout: 15000 });
    const boxes = await page.$$('#obsTable tbody input[type="checkbox"][data-compare-select]');
    if (boxes.length < 3) return; // fixture might only have 2; skip silently
    await boxes[0].check();
    await boxes[1].check();
    await boxes[2].check();
    const btn = await page.$('[data-action="compare-selected"]');
    const disabled = await btn.evaluate(el => el.disabled || el.getAttribute('aria-disabled') === 'true');
    assert(disabled, '"Compare selected" must re-disable when count !== 2');
  });

  // ── Compare page breadcrumbs to both observer detail pages ──
  await step('Compare page renders breadcrumb links back to both observer detail pages', async () => {
    await page.goto(BASE + '/#/compare?a=' + idA + '&b=' + idB, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('.compare-page', { timeout: 15000 });
    await page.waitForSelector('[data-role="compare-breadcrumbs"]', { timeout: 10000 });
    const linkA = await page.$('[data-role="compare-breadcrumbs"] a[href="#/observers/' + idA + '"]');
    const linkB = await page.$('[data-role="compare-breadcrumbs"] a[href="#/observers/' + idB + '"]');
    assert(linkA, 'expected breadcrumb anchor → #/observers/<idA>');
    assert(linkB, 'expected breadcrumb anchor → #/observers/<idB>');
  });

  // ── Legacy deep-link regression guard ──
  await step('Legacy deep-link #/compare?a=...&b=... still pre-populates both selects', async () => {
    await page.goto(BASE + '/#/compare?a=' + idA + '&b=' + idB, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#compareObsA', { timeout: 15000 });
    const valA = await page.$eval('#compareObsA', el => el.value);
    const valB = await page.$eval('#compareObsB', el => el.value);
    assert(valA === idA, 'compareObsA should be pre-selected to a=, got ' + valA);
    assert(valB === idB, 'compareObsB should be pre-selected to b=, got ' + valB);
  });

  // ── #393 F1: compare's row-click listener must not outlive the page ──
  // compare.js once bound its delegated row click to the shared #app
  // container and never removed it, so after one compare visit it fired on
  // every later page's tr[data-action="navigate"]. On #/observers at
  // <=1023px the observers handler opens the #1056 AC#4 slide-over and calls
  // preventDefault(); the leaked compare handler navigated anyway. Drive the
  // real SPA flow (hash navigation, no reload) home → compare → observers.
  const narrow = await browser.newContext({ viewport: { width: 800, height: 800 } });
  const np = await narrow.newPage();
  np.setDefaultTimeout(15000);
  np.on('pageerror', e => console.error('  pageerror:', e.message));

  await step('#393 F1 compare row click still navigates to the packet', async () => {
    await np.goto(BASE + '/#/home', { waitUntil: 'domcontentloaded' });
    await np.evaluate(([a, b]) => { location.hash = '#/compare?a=' + a + '&b=' + b; }, [idA, idB]);
    await np.waitForSelector('#compareContent [data-cview]', { timeout: 20000 });
    // Open the first non-empty packet list (fixture overlap varies).
    const view = await np.$$eval('#compareContent [data-cview]', btns => {
      const b = btns.find(x => x.dataset.cview !== 'summary' && /\(([1-9]\d*)\)/.test(x.textContent));
      return b ? b.dataset.cview : null;
    });
    assert(view, 'fixture observers ' + idA + '/' + idB + ' have no packets to list');
    await np.click('#compareContent [data-cview="' + view + '"]');
    const row = await np.waitForSelector('#compareDetail tr[data-action="navigate"]');
    const target = await row.getAttribute('data-value');
    await row.click();
    await np.waitForFunction(t => location.hash === t, target, { timeout: 5000 });
  });

  await step('#393 F1 after compare, an observers row opens the slide-over and does not navigate', async () => {
    await np.evaluate(() => { location.hash = '#/observers'; });
    await np.waitForSelector('#obsTable tbody tr[data-action="navigate"]');
    await np.evaluate(() => {
      window.__f1HashChanges = 0;
      window.addEventListener('hashchange', () => { window.__f1HashChanges++; });
    });
    await np.click('#obsTable tbody tr[data-action="navigate"] td:not(:empty)');
    await np.waitForFunction(() => {
      const p = document.querySelector('.slide-over-panel');
      return p && !p.hidden;
    }, null, { timeout: 10000 });
    // Give a leaked handler's navigation time to land before asserting.
    await np.waitForTimeout(500);
    const st = await np.evaluate(() => ({ hash: location.hash, changes: window.__f1HashChanges }));
    assert(st.hash === '#/observers',
      'observers row click navigated after a compare visit (leaked compare listener): ' + st.hash);
    assert(st.changes === 0, 'expected no hashchange on a slide-over row click, got ' + st.changes);
  });

  await narrow.close();
  await browser.close();
  console.log('\n' + passed + ' passed, ' + failed + ' failed');
  if (failed) process.exit(1);
}

run().catch(e => { console.error(e); process.exit(1); });
