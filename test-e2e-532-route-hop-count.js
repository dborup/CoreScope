/**
 * #532 — route meta must count recorded relays only, not payload-derived
 * sender/recipient endpoints (_fromPayload). Drives the real route flow
 * (sessionStorage 'map-route-hops' -> #/map -> drawPacketRoute -> route-view)
 * for desktop and mobile viewports against the e2e fixture.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-e2e-532-route-hop-count.js
 */
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';

let failed = false;
function check(cond, msg) {
  if (cond) console.log('  ✅', msg); else { failed = true; console.log('  ❌', msg); }
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage']
  });

  // Six relays + sender + recipient, all GPS-bearing nodes from the fixture.
  async function pickNodes(page) {
    const r = await page.request.get(BASE + '/api/nodes?limit=200');
    const j = await r.json();
    const withGps = (j.nodes || []).filter(n => n.lat && n.lon && n.public_key);
    return withGps.slice(0, 8).map(n => n.public_key);
  }

  async function run(label, ctxOpts, selector) {
    console.log(`\n#532 ${label} against ${BASE}`);
    const ctx = await browser.newContext(ctxOpts);
    const page = await ctx.newPage();
    page.setDefaultTimeout(20000);
    await page.goto(BASE + '/#/nodes', { waitUntil: 'domcontentloaded' });
    const keys = await pickNodes(page);
    check(keys.length === 8, 'fixture provides 8 GPS nodes');
    const relays = keys.slice(0, 6), sender = keys[6], recipient = keys[7];
    await page.evaluate(({ relays, sender, recipient }) => {
      sessionStorage.setItem('map-route-hops', JSON.stringify({
        hops: relays,
        origin: { pubkey: sender, name: 'Sender' },
        destination: { pubkey: recipient, name: 'Recipient' }
      }));
    }, { relays, sender, recipient });
    await page.goto(BASE + '/#/map', { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('.mc-rt-row', { state: 'attached' });
    const info = await page.evaluate((sel) => ({
      rows: document.querySelectorAll('.mc-rt-row').length,
      text: (document.querySelector(sel) || {}).textContent || ''
    }), selector);
    check(info.rows === 8, `all 8 display rows are kept (got ${info.rows})`);
    check(/\b6 hops\b/.test(info.text) && !/\b8 hops\b/.test(info.text),
      `${selector} says "6 hops", not 8: "${info.text.trim()}"`);
    await ctx.close();
  }

  await run('desktop meta', { viewport: { width: 1280, height: 800 } }, '.mc-rt-meta');
  await run('mobile summary', {
    viewport: { width: 375, height: 812 }, isMobile: true, hasTouch: true, deviceScaleFactor: 2
  }, '.mc-rt-mobile-summary');

  await browser.close();
  process.exit(failed ? 1 : 0);
})().catch(e => { console.error(e); process.exit(1); });
