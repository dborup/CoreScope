const assert = require('node:assert/strict');
const http = require('node:http');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require('playwright');
const routes = { '/velkommen': 'index.html', '/observer': 'observer.html', '/hardware': 'hardware.html', '/radio': 'radio.html' };
const expected = 'set wifi.ssid NAVN_PÅ_WIFI\nset wifi.pwd KODE_TIL_WIFI\nset mqtt.iata DIT_IATA\nset mqtt1.preset custom\nset mqtt1.server wss://mqtt.meshview.dk:443/mqtt\nset mqtt1.audience mqtt.meshview.dk\nset mqtt1.topic meshcore/{iata}/{device}/{type}\nset mqtt.owner DIT_PUB_KEY\nreboot';
const server = http.createServer((req, res) => {
  const url = new URL(req.url, 'http://localhost');
  if (url.pathname.startsWith('/api/')) {
    res.setHeader('Content-Type', 'application/json');
    const now = new Date().toISOString();
    const data = url.pathname === '/api/stats' ? { totalNodes: 1, totalPackets: 12, totalObservers: 1 } :
      url.pathname === '/api/nodes' ? [{ name: 'Testnode', role: 'repeater', last_seen: now, lat: 56, lon: 10 }] :
      url.pathname === '/api/observers' ? [{ name: 'Testobserver', packetsLastHour: 5 }] :
      [{ observer_name: 'Testobserver', snr: 8, timestamp: now }];
    return res.end(JSON.stringify(data));
  }
  let file = routes[url.pathname.replace(/\/$/, '')];
  if (url.pathname.startsWith('/meshview-info-assets/')) file = 'assets/' + url.pathname.slice('/meshview-info-assets/'.length);
  if (!file || file.includes('..') || !fs.existsSync(path.join(__dirname, file))) { res.statusCode = 404; return res.end(); }
  res.setHeader('Content-Type', file.endsWith('.css') ? 'text/css' : file.endsWith('.js') ? 'text/javascript' : file.endsWith('.ttf') ? 'font/ttf' : 'text/html; charset=utf-8');
  res.end(fs.readFileSync(path.join(__dirname, file)));
});
let browser;
const timeout = setTimeout(() => { console.error('TIMEOUT'); process.exit(1); }, 60000);
(async () => {
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const origin = `http://127.0.0.1:${server.address().port}`;
  browser = await chromium.launch({ headless: true });
  const out = process.env.SCREENSHOT_DIR;
  if (out) fs.mkdirSync(out, { recursive: true });
  for (const width of [320, 375, 390, 768, 1280]) {
    const context = await browser.newContext({ viewport: { width, height: 844 }, permissions: ['clipboard-read', 'clipboard-write'] });
    const page = await context.newPage();
    const errors = [], external = [], failures = [];
    page.on('pageerror', e => errors.push(e.message));
    page.on('response', r => { if (r.status() >= 400) failures.push(r.url()); });
    await page.route('**/*', route => { if (!route.request().url().startsWith(origin)) { external.push(route.request().url()); return route.abort(); } return route.continue(); });
    for (const route of Object.keys(routes)) {
      await page.goto(origin + route);
      await page.evaluate(() => document.fonts.ready);
      assert.deepEqual(errors, [], `${route} JS errors`);
      assert.deepEqual(failures, [], `${route} failed requests`);
      assert.deepEqual(external, [], `${route} external requests`);
      assert(await page.evaluate(() => document.fonts.check('16px Syne') && document.fonts.check('16px "Share Tech Mono"')));
      const overflow = await page.evaluate(() => [...document.querySelectorAll('body *')].filter(el => {
        if (el.closest('pre, svg, .table-wrap')) return false;
        const r = el.getBoundingClientRect();
        return r.width && (r.right > innerWidth + 1 || r.left < -1);
      }).map(el => el.tagName + '.' + el.className));
      assert.deepEqual(overflow, [], `${route} width ${width}: clipped/overflowing content`);
      assert(await page.locator('.nav-links').isVisible(), 'mobile navigation visible');
      if (route === '/observer') {
        assert.equal(await page.locator('.copy-commands').count(), 2);
        assert.equal(await page.locator('pre code').first().textContent(), expected);
        for (let n = 0; n < 2; n++) {
          const b = page.locator('.copy-commands').nth(n);
          assert((await b.boundingBox()).height >= 44);
          await b.focus(); await page.keyboard.press(n ? 'Space' : 'Enter');
          await page.waitForFunction(n => document.querySelectorAll('.copy-commands')[n].textContent === 'Kopieret!', n);
          assert.equal(await page.evaluate(() => navigator.clipboard.readText()), await page.locator('pre code').nth(n).textContent());
        }
      }
      if (out && width === 375) await page.screenshot({ path: path.join(out, route.slice(1) + '.png'), fullPage: true });
      console.log(`PASS ${route} ${width}px`);
    }
    await context.close();
  }
})().catch(e => { console.error(e); process.exitCode = 1; }).finally(async () => {
  if (browser) await browser.close();
  await new Promise(resolve => server.close(resolve));
  clearTimeout(timeout);
});
