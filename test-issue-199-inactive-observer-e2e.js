/**
 * E2E (#199): from the Observers page, a device without a `nodes` row must not
 * dead-end on a bare "Node not found".
 *
 * Needs test-fixtures/seed-199-inactive-observer.sql applied to the fixture:
 *   - observer 0D3B3F38... has its node row only in inactive_nodes, so the
 *     node page explains "No advert heard since <date>; this device is
 *     inactive" with the inactive row's name and role;
 *   - observer 424419FD... has no node record at all, so the node page says
 *     so and links back to the observer.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-issue-199-inactive-observer-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const INACTIVE_OBS = '0D3B3F382173A49EB0E3BB01AB2EA9B28601D9039E4BD52C55B91A8000CC092D';
const OBS_ONLY = '424419FDF9DD9D206A5A917979E56E843DE78E890359C70B9F59B7E2CF2CE392';

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

async function settledNodePage(page) {
  await page.waitForFunction(() => {
    const el = document.querySelector('.node-full-title');
    return el && (el.textContent || '').trim() !== 'Loading…';
  }, { timeout: 10000 });
  return page.evaluate(() => {
    const body = document.getElementById('nodeFullBody');
    const title = document.querySelector('.node-full-title');
    return {
      title: title ? title.textContent.trim() : '',
      text: body ? body.textContent : '',
      hrefs: body ? Array.from(body.querySelectorAll('a[href]')).map(a => a.getAttribute('href')) : [],
    };
  });
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  const ctx = await browser.newContext({ viewport: { width: 1400, height: 900 } });
  const page = await ctx.newPage();
  page.setDefaultTimeout(15000);
  page.on('pageerror', (e) => console.error('[pageerror]', e.message));

  console.log('\n=== #199 inactive observer node page E2E against ' + BASE + ' ===');

  await step('GET /api/nodes/<inactive observer> is a 404 that carries the inactive row (precondition)', async () => {
    const res = await page.request.get(BASE + '/api/nodes/' + INACTIVE_OBS.toLowerCase());
    assert(res.status() === 404, 'expected 404, got ' + res.status());
    const body = await res.json();
    assert(body.inactive_node && body.inactive_node.name === 'Inactive Observer E2E',
      'inactive_node missing (seed-199 applied?): ' + JSON.stringify(body));
    assert(body.observer && body.observer.id === INACTIVE_OBS, 'observer missing: ' + JSON.stringify(body));
  });

  await step('observer detail → "View node detail" explains the inactive node', async () => {
    await page.goto(BASE + '/#/observers/' + encodeURIComponent(INACTIVE_OBS), { waitUntil: 'domcontentloaded' });
    const link = page.locator('a', { hasText: 'View node detail' });
    await link.waitFor();
    await link.click();
    await page.waitForFunction(() => location.hash.indexOf('#/nodes/') === 0);
    const r = await settledNodePage(page);
    assert(r.text.indexOf('Node not found') === -1, 'dead-ended on "Node not found": ' + r.text.slice(0, 200));
    assert(/No advert heard since .+; this device is inactive/.test(r.text), 'missing inactive explanation: ' + r.text.slice(0, 300));
    assert(r.text.indexOf('Inactive Observer E2E') !== -1, 'inactive name missing');
    assert(/repeater/i.test(r.text), 'inactive role missing');
    assert(r.title.indexOf('Inactive Observer E2E') !== -1, 'title should name the device: ' + r.title);
    assert(r.hrefs.indexOf('#/observers/' + encodeURIComponent(INACTIVE_OBS)) !== -1, 'observer link missing: ' + r.hrefs);
    assert(r.hrefs.indexOf('#/nodes') !== -1, 'Back to Nodes link missing');
  });

  await step('observer with no node record: node page says so and links the observer', async () => {
    await page.goto(BASE + '/#/nodes/' + OBS_ONLY.toLowerCase(), { waitUntil: 'domcontentloaded' });
    const r = await settledNodePage(page);
    assert(r.text.indexOf('Node not found') === -1, 'dead-ended on "Node not found"');
    assert(/no node record/i.test(r.text), 'missing "no node record" explanation: ' + r.text.slice(0, 300));
    assert(r.hrefs.indexOf('#/observers/' + encodeURIComponent(OBS_ONLY)) !== -1, 'observer link missing: ' + r.hrefs);
  });

  await browser.close();

  console.log('\n--- ' + passed + ' passed, ' + failed + ' failed ---\n');
  process.exit(failed > 0 ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
