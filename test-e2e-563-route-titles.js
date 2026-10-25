/**
 * #563 — route-aware browser tab titles. Drives the real SPA router against the
 * e2e fixture: deep link, route change, node Reach/Analytics, back/forward,
 * unknown-route fallback, configured instance name (incl. a delayed theme
 * response), and a late node response after the user moved on.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-e2e-563-route-titles.js
 */
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
let failed = false;
function check(cond, msg) {
  if (cond) console.log('  ✅', msg); else { failed = true; console.log('  ❌', msg); }
}
const title = page => page.evaluate(() => document.title);
async function waitTitle(page, expected) {
  try { await page.waitForFunction(t => document.title === t, expected, { timeout: 8000 }); } catch (e) {}
  const got = await title(page);
  check(got === expected, `title is "${expected}"` + (got === expected ? '' : ` (got "${got}")`));
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage']
  });
  const newPage = async () => {
    const ctx = await browser.newContext();
    const page = await ctx.newPage();
    page.setDefaultTimeout(20000);
    return page;
  };

  // Pick a named node from the fixture.
  const probe = await newPage();
  const nodes = await (await probe.request.get(BASE + '/api/nodes?limit=200')).json();
  const node = (nodes.nodes || []).find(n => n.name && n.public_key);
  check(!!node, 'fixture provides a named node');
  const key = node.public_key;
  await probe.context().close();

  console.log(`\n#563 default instance name against ${BASE}`);
  let page = await newPage();
  await page.goto(BASE + '/#/live', { waitUntil: 'domcontentloaded' });
  await waitTitle(page, 'Live · CoreScope');
  await page.evaluate(() => { location.hash = '#/channels'; });
  await waitTitle(page, 'Channels · CoreScope');
  await page.evaluate(() => { location.hash = '#/analytics'; });
  await waitTitle(page, 'Analytics · CoreScope');
  await page.goBack();
  await waitTitle(page, 'Channels · CoreScope');
  await page.goForward();
  await waitTitle(page, 'Analytics · CoreScope');
  await page.evaluate(() => { location.hash = '#/no-such-view'; });
  await waitTitle(page, 'Not found · CoreScope');

  console.log('\n#563 node Reach / Analytics');
  await page.goto(BASE + '/#/nodes/' + encodeURIComponent(key) + '/reach', { waitUntil: 'domcontentloaded' });
  await waitTitle(page, `Reach · ${node.name} · CoreScope`);
  await page.evaluate(k => { location.hash = '#/nodes/' + encodeURIComponent(k) + '/analytics'; }, key);
  await waitTitle(page, `Node Analytics · ${node.name} · CoreScope`);
  await page.evaluate(() => { location.hash = '#/live'; });
  await waitTitle(page, 'Live · CoreScope');
  await page.context().close();

  console.log('\n#563 configured instance name, delayed theme response');
  page = await newPage();
  await page.route('**/api/config/theme', async route => {
    const resp = await route.fetch();
    const body = await resp.json();
    body.branding = Object.assign({}, body.branding, { siteName: 'Harbour Mesh' });
    await new Promise(r => setTimeout(r, 1500));
    await route.fulfill({ response: resp, json: body });
  });
  await page.goto(BASE + '/#/live', { waitUntil: 'domcontentloaded' });
  check((await title(page)) !== 'CoreScope', 'title is not the bare static "CoreScope" while theme is pending');
  await waitTitle(page, 'Live · Harbour Mesh');
  await page.evaluate(() => { location.hash = '#/channels'; });
  await waitTitle(page, 'Channels · Harbour Mesh');
  await page.context().close();

  console.log('\n#563 late node response after navigating away');
  page = await newPage();
  let release;
  const gate = new Promise(r => { release = r; });
  await page.route('**/api/nodes/*/reach*', async route => { await gate; await route.continue(); });
  await page.goto(BASE + '/#/nodes/' + encodeURIComponent(key) + '/reach', { waitUntil: 'domcontentloaded' });
  await waitTitle(page, 'Reach · CoreScope');
  await page.evaluate(() => { location.hash = '#/live'; });
  await waitTitle(page, 'Live · CoreScope');
  release();
  await page.waitForTimeout(1500);
  check((await title(page)) === 'Live · CoreScope', 'late Reach response does not rename the tab');
  await page.context().close();

  await browser.close();
  process.exit(failed ? 1 : 0);
})().catch(e => { console.error(e); process.exit(1); });
