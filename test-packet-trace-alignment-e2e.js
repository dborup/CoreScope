// Real packet-detail markup and stylesheet, with deterministic local API fixtures.
// Run: node test-packet-trace-alignment-e2e.js (requires Playwright Chromium).
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require('playwright');

(async () => {
  const browser = await chromium.launch({ headless: true });
  let checks = 0;
  try {
    const page = await browser.newPage();
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));
    // Fulfil locally: no server process, external service, or real packet data.
    await page.route('**/*', route => {
      const url = new URL(route.request().url());
      if (url.origin !== 'http://127.0.0.1:18721') return route.abort();
      if (url.pathname.startsWith('/api/')) return route.fulfill({ json: { nodes: [], observers: [] } });
      if (url.pathname === '/icons/phosphor-sprite.svg') return route.fulfill({
        contentType: 'image/svg+xml', body: fs.readFileSync(path.join(__dirname, 'public', url.pathname))
      });
      return route.fulfill({ contentType: 'text/html', body: '<html><body><main id="fixture"></main></body></html>' });
    });
    await page.goto('http://127.0.0.1:18721/');
    await page.addStyleTag({ path: path.join(__dirname, 'public/style.css') });
    for (const file of ['payload-labels.js', 'roles.js', 'app.js', 'packet-helpers.js']) {
      await page.addScriptTag({ path: path.join(__dirname, 'public', file) });
    }
    await page.evaluate(() => {
      window.fixturePages = {};
      registerPage = (name, handler) => { window.fixturePages[name] = handler; };
      api = async () => ({ packet: {
        id: 21, hash: '0123456789abcdef', payload_type: 5, route_type: 1,
        timestamp: '2026-01-01T00:00:00Z', path_json: '[]',
        decoded_json: JSON.stringify({ type: 'GRP_TXT', sender: 'Example', channel: 'test' })
      }, observations: [] });
    });
    await page.addScriptTag({ path: path.join(__dirname, 'public/packets.js') });
    for (const [name, width, panelWidth] of [['desktop', 1280, 800], ['narrow-panel', 1280, 350], ['mobile', 375, 343]]) {
      await page.setViewportSize({ width, height: 800 });
      await page.evaluate(async panelWidth => {
        const fixture = document.getElementById('fixture');
        fixture.style.width = panelWidth + 'px';
        await window.fixturePages['packet-detail'].init(fixture, '21');
      }, panelWidth);
      const geometry = await page.evaluate(() => {
        const link = document.querySelector('.detail-actions a.detail-map-link');
        const button = document.querySelector('.detail-actions button.detail-map-link');
        const measure = element => {
          const box = element.getBoundingClientRect();
          const icon = element.querySelector('svg').getBoundingClientRect();
          const text = [...element.childNodes].find(node => node.nodeType === Node.TEXT_NODE && node.textContent.trim());
          const range = document.createRange();
          range.selectNodeContents(text);
          const label = range.getBoundingClientRect();
          return { center: box.y + box.height / 2, icon: icon.y + icon.height / 2, text: label.y + label.height / 2 };
        };
        return { link: measure(link), button: measure(button), href: link.getAttribute('href'), tag: link.tagName };
      });
      if (process.env.SCREENSHOT_DIR) await page.screenshot({ path: path.join(process.env.SCREENSHOT_DIR, 'issue21-' + name + '.png'), fullPage: true });
      assert.equal(geometry.tag, 'A'); checks++;
      assert.equal(geometry.href, '#/traces/0123456789abcdef'); checks++;
      assert.ok(Math.abs(geometry.link.icon - geometry.link.center) <= 1.5, name + ': Trace icon centered ' + JSON.stringify(geometry)); checks++;
      assert.ok(Math.abs(geometry.link.text - geometry.link.center) <= 1.5, name + ': Trace text centered ' + JSON.stringify(geometry)); checks++;
      // Narrow native buttons wrap their labels; compare control centers there.
      assert.ok(Math.abs(geometry.link.center - geometry.button.center) <= 1, name + ': controls share center ' + JSON.stringify(geometry)); checks++;
      if (name === 'desktop') {
        assert.ok(Math.abs(geometry.link.text - geometry.button.text) <= 1.5, name + ': label aligns with View Path ' + JSON.stringify(geometry)); checks++;
      }
      console.log('PASS ' + name);
    }
    assert.deepEqual(errors, []); checks++;
    console.log(checks + ' checks passed');
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });
