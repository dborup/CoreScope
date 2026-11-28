// /guider: click-to-play YouTube cards. Nothing is requested from YouTube before a click;
// a card without a valid id is disabled; every info page links to /guider exactly once.
const assert = require('node:assert/strict');
const http = require('node:http');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require('playwright');
const pages = { '/velkommen': 'index.html', '/observer': 'observer.html', '/hardware': 'hardware.html', '/radio': 'radio.html', '/guider': 'guider.html' };
const VALID = 'dQw4w9WgXcQ';
const types = { '.css': 'text/css', '.js': 'text/javascript', '.ttf': 'font/ttf', '.jpg': 'image/jpeg' };
// /guider-ids serves guider.html with ids filled in: card 1 valid, card 2 malformed, card 3 empty.
function withIds(html) {
  let n = 0;
  return html.replace(/data-youtube=""/g, () => {
    n++;
    return n === 1 ? `data-youtube="${VALID}"` : n === 2 ? 'data-youtube="abc&quot;onerror"' : 'data-youtube=""';
  });
}
const server = http.createServer((req, res) => {
  const url = new URL(req.url, 'http://localhost');
  if (url.pathname.startsWith('/api/')) { res.setHeader('Content-Type', 'application/json'); return res.end('[]'); }
  let file = pages[url.pathname.replace(/\/$/, '')];
  if (url.pathname === '/guider-ids') file = 'guider.html';
  if (url.pathname.startsWith('/meshview-info-assets/')) file = 'assets/' + url.pathname.slice('/meshview-info-assets/'.length);
  if (!file || file.includes('..') || !fs.existsSync(path.join(__dirname, file))) { res.statusCode = 404; return res.end(); }
  res.setHeader('Content-Type', types[path.extname(file)] || 'text/html; charset=utf-8');
  let body = fs.readFileSync(path.join(__dirname, file));
  if (url.pathname === '/guider-ids') body = withIds(body.toString());
  res.end(body);
});
let browser;
const timeout = setTimeout(() => { console.error('TIMEOUT'); process.exit(1); }, 60000);
(async () => {
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const origin = `http://127.0.0.1:${server.address().port}`;
  browser = await chromium.launch({ headless: true });
  const context = await browser.newContext({ viewport: { width: 375, height: 740 }, hasTouch: true, isMobile: true });
  const page = await context.newPage();
  const external = [], errors = [];
  page.on('pageerror', e => errors.push(e.message));
  await page.route('**/*', route => {
    const u = route.request().url();
    if (u.startsWith(origin)) return route.continue();
    external.push(u);
    return route.fulfill({ status: 204, body: '' });
  });

  // Every page links to /guider exactly once in the nav.
  for (const p of Object.keys(pages)) {
    await page.goto(origin + p);
    assert.equal(await page.locator('.nav-links a[href="/guider"]').count(), 1, `${p}: one Guider link in the nav`);
  }

  // Published page: no ids yet, so every card is disabled and says so; no external request.
  await page.goto(origin + '/guider');
  const cards = page.locator('.guide-card');
  assert.equal(await cards.count(), 3);
  for (let i = 0; i < 3; i++) {
    const b = cards.nth(i).locator('.guide-play');
    assert.equal(await b.isDisabled(), true, `card ${i} disabled without an id`);
    assert.equal(await cards.nth(i).locator('.guide-badge.soon').textContent(), 'Video på vej');
    assert.match(await b.getAttribute('aria-label'), /video på vej$/);
    for (const a of await cards.nth(i).locator('.guide-links a').all()) assert((await a.boundingBox()).height >= 44, 'link target >= 44 px');
  }
  assert.deepEqual(external, [], 'no external request on /guider');

  // With ids: nothing from YouTube until the tap; the tap swaps in the nocookie iframe.
  await page.goto(origin + '/guider-ids');
  assert.deepEqual(external, [], 'no external request before play');
  const first = cards.nth(0).locator('.guide-play');
  assert.equal(await first.isDisabled(), false);
  assert.equal(await first.getAttribute('aria-label'), 'Afspil video: Danske kort og lag');
  assert.equal(await cards.nth(1).locator('.guide-play').isDisabled(), true, 'malformed id stays disabled');
  assert.equal(await cards.nth(2).locator('.guide-play').isDisabled(), true, 'empty id stays disabled');
  await first.tap();
  const frame = cards.nth(0).locator('iframe');
  await frame.waitFor();
  const src = await frame.getAttribute('src');
  assert.equal(new URL(src).origin, 'https://www.youtube-nocookie.com');
  assert.equal(new URL(src).pathname, '/embed/' + VALID);
  assert.equal(new URL(src).searchParams.get('autoplay'), '1');
  assert.equal(await frame.getAttribute('title'), 'Danske kort og lag');
  assert.equal(await cards.nth(0).locator('.guide-play').count(), 0, 'button replaced by the player');
  assert(external.some(u => u.startsWith('https://www.youtube-nocookie.com/embed/' + VALID)), 'player requested after the tap');
  assert(!external.some(u => /youtube\.com|ytimg|googlevideo/.test(new URL(u).hostname) && !u.startsWith('https://www.youtube-nocookie.com/')), 'only the nocookie host');
  assert.deepEqual(errors, []);
  console.log('PASS: /guider click-to-play, disabled cards without ids, one nav link per page, no external request before play');
})().catch(e => { console.error(e); process.exitCode = 1; }).finally(async () => {
  if (browser) await browser.close();
  await new Promise(resolve => server.close(resolve));
  clearTimeout(timeout);
});
