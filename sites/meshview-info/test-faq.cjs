// /faq: grouped <details> answers, a deep link opens its answer, every in-page and site link
// resolves, and every info page links to /faq from its footer. No external request.
const assert = require('node:assert/strict');
const http = require('node:http');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require('playwright');
const pages = { '/velkommen': 'index.html', '/observer': 'observer.html', '/hardware': 'hardware.html', '/radio': 'radio.html', '/guider': 'guider.html', '/faq': 'faq.html' };
const types = { '.css': 'text/css', '.js': 'text/javascript', '.ttf': 'font/ttf', '.jpg': 'image/jpeg' };
const server = http.createServer((req, res) => {
  const url = new URL(req.url, 'http://localhost');
  if (url.pathname.startsWith('/api/')) { res.setHeader('Content-Type', 'application/json'); return res.end('[]'); }
  let file = pages[url.pathname.replace(/\/$/, '')];
  if (url.pathname.startsWith('/meshview-info-assets/')) file = 'assets/' + url.pathname.slice('/meshview-info-assets/'.length);
  if (!file || file.includes('..') || !fs.existsSync(path.join(__dirname, file))) { res.statusCode = 404; return res.end(); }
  res.setHeader('Content-Type', types[path.extname(file)] || 'text/html; charset=utf-8');
  res.end(fs.readFileSync(path.join(__dirname, file)));
});
let browser;
const timeout = setTimeout(() => { console.error('TIMEOUT'); process.exit(1); }, 60000);
(async () => {
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const origin = `http://127.0.0.1:${server.address().port}`;
  browser = await chromium.launch({ headless: true });
  const page = await (await browser.newContext({ viewport: { width: 375, height: 740 }, hasTouch: true, isMobile: true })).newPage();
  const external = [], errors = [];
  page.on('pageerror', e => errors.push(e.message));
  await page.route('**/*', r => { if (r.request().url().startsWith(origin)) return r.continue(); external.push(r.request().url()); return r.abort(); });

  for (const p of Object.keys(pages)) {
    await page.goto(origin + p);
    assert.equal(await page.locator('footer a[href="/faq"]').count(), 1, `${p}: one FAQ link in the footer`);
  }

  await page.goto(origin + '/faq');
  const items = page.locator('details.faq-item');
  assert.equal(await items.count(), 14);
  assert.equal(await page.locator('details[open]').count(), 0, 'all answers closed without a hash');
  const ids = await page.evaluate(() => [...document.querySelectorAll('[id]')].map(e => e.id));
  assert.equal(new Set(ids).size, ids.length, 'ids are unique');
  const broken = await page.evaluate(() => [...document.querySelectorAll('a[href^="#"]')].map(a => a.getAttribute('href').slice(1)).filter(id => id && !document.getElementById(id)));
  assert.deepEqual(broken, [], 'in-page anchors resolve');
  for (const s of await page.locator('summary').all()) assert((await s.boundingBox()).height >= 44, 'summary target >= 44 px');

  // A tap opens an answer.
  const first = items.first();
  await first.locator('summary').tap();
  assert.equal(await first.evaluate(d => d.open), true);

  // A deep link opens its answer and shows it below the sticky nav; an in-page link does too.
  await page.goto('about:blank');
  await page.goto(origin + '/faq#hash-storrelse');
  await page.evaluate(() => document.fonts.ready);
  await page.waitForTimeout(300);
  assert.equal(await page.locator('#hash-storrelse').evaluate(d => d.open), true, 'deep link opens the answer');
  await page.locator('#hash-storrelse').evaluate(d => d.scrollIntoView({ behavior: 'instant', block: 'start' }));
  const navBottom = await page.locator('nav').evaluate(n => n.getBoundingClientRect().bottom);
  const top = await page.locator('#hash-storrelse summary').evaluate(s => s.getBoundingClientRect().top);
assert(top >= navBottom - 1, `deep-linked question not hidden under the nav (${top} vs ${navBottom})`);
  await page.goto(origin + '/faq');
  await page.locator('#roller a[href="#observer"]').evaluate(a => a.closest('details').open = true);
  await page.locator('#roller a[href="#observer"]').tap();
  await page.waitForFunction(() => document.getElementById('observer').open);

  // Site-relative links point at pages that exist.
  const rel = await page.evaluate(() => [...new Set([...document.querySelectorAll('a[href^="/"]')].map(a => a.getAttribute('href').split('#')[0]))]);
  for (const href of rel) {
    const r = await page.request.get(origin + href);
    assert.equal(r.status(), 200, `${href} exists`);
  }
  assert.deepEqual(external, [], 'no external request');
  assert.deepEqual(errors, []);
  console.log('PASS: /faq 14 answers, deep links open answers below the nav, links resolve, footer link on every page');
})().catch(e => { console.error(e); process.exitCode = 1; }).finally(async () => {
  if (browser) await browser.close();
  await new Promise(resolve => server.close(resolve));
  clearTimeout(timeout);
});
