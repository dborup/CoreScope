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
  assert.equal(await items.count(), 45);
  assert.equal(await page.locator('details[open]').count(), 0, 'all answers closed without a hash');
  assert.equal(await page.locator('details.faq-item details, details.faq-item .faq-group').count(), 0, 'no answer nested inside another');
  const groups = await page.evaluate(() => [...document.querySelectorAll('.faq-group')].map(g => [g.id, g.querySelectorAll(':scope > details.faq-item').length]));
  assert.deepEqual(groups, [['kom-i-gang', 10], ['observer-og-mqtt', 11], ['kortet', 9], ['daekning-og-placering', 7], ['privatliv', 3], ['om-netvaerket', 5]]);
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
  // A deep link must land below the sticky nav: every question's scroll margin covers the nav height.
  const nav = await page.locator('nav').evaluate(n => n.getBoundingClientRect().height);
  const margins = await page.evaluate(() => [...document.querySelectorAll('details.faq-item, .faq-group')].map(e => parseFloat(getComputedStyle(e).scrollMarginTop)));
  assert(margins.every(m => m >= nav), `scroll margin ${Math.min(...margins)} px covers the ${nav} px nav`);
  await page.goto(origin + '/faq');
  await page.locator('#roller a[href="#observer"]').evaluate(a => a.closest('details').open = true);
  await page.locator('#roller a[href="#observer"]').tap();
  await page.waitForFunction(() => document.getElementById('observer').open);
  // Tapping the same in-page link again after closing the answer reopens it (the hash does not change).
  await page.locator('#observer').evaluate(d => { d.open = false; });
  await page.locator('#roller a[href="#observer"]').tap();
  await page.waitForFunction(() => document.getElementById('observer').open, null, { timeout: 2000 });

  // Facts fixed after review: repeaters get the longer grace period, hidden is not deleted,
  // and hashtag channels are readable by anyone who knows the name.
  const text = id => page.locator('#' + id).evaluate(d => d.textContent.replace(/\s+/g, ' '));
  const grey = await text('graa-noder');
  assert(!/repeatere skal høres oftere/i.test(grey), 'repeaters are not the ones that must be heard more often');
  assert(/Repeatere og room servers[^.]*lang tid/.test(grey) && /Companions[^.]*en time og et døgn/.test(grey), 'role thresholds stated the right way round');
  const hide = await text('skjul-node');
  assert(/ikke det samme som at slette/.test(hide) && !/fjerner/.test(hide), 'hiding is not presented as removal');
  assert(/alle, der kender navnet, kan derfor læse med/i.test(await text('beskeder')), 'hashtag channel keys come from the name');

  // Reduced motion turns smooth scrolling off.
  await page.emulateMedia({ reducedMotion: 'reduce' });
  assert.equal(await page.evaluate(() => getComputedStyle(document.documentElement).scrollBehavior), 'auto');
  await page.emulateMedia({ reducedMotion: 'no-preference' });

  // Site-relative links point at pages that exist.
  const rel = await page.evaluate(() => [...new Set([...document.querySelectorAll('a[href^="/"]')].map(a => a.getAttribute('href').split('#')[0]))]);
  for (const href of rel) {
    const r = await page.request.get(origin + href);
    assert.equal(r.status(), 200, `${href} exists`);
  }
  assert.deepEqual(external, [], 'no external request');
  assert.deepEqual(errors, []);
  console.log('PASS: /faq 45 answers, deep links open answers below the nav, links resolve, footer link on every page');
})().catch(e => { console.error(e); process.exitCode = 1; }).finally(async () => {
  if (browser) await browser.close();
  await new Promise(resolve => server.close(resolve));
  clearTimeout(timeout);
});
