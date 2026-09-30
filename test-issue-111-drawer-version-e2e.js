/**
 * E2E (#111): running version in the nav-drawer footer.
 *
 * At drawer viewports (> 768px, incl. a short one) the footer sits at the
 * bottom of the drawer, inside it, below the route list, which keeps every
 * route reachable. /api/health is requested only on the first open and never
 * again; at a narrow width (drawer disabled) nothing is requested. With the
 * local server's "unknown" version the label stays "CoreScope"; with a routed
 * response it shows the version as text.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-issue-111-drawer-version-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

async function page(browser, viewport, errors) {
  const ctx = await browser.newContext({ viewport });
  const p = await ctx.newPage();
  p.setDefaultTimeout(15000);
  const health = [];
  p.on('request', (r) => { if (new URL(r.url()).pathname === '/api/health') health.push(r.url()); });
  p.on('pageerror', (e) => errors.push(e.message));
  await p.goto(BASE + '/#/packets');
  await p.waitForFunction(() => !!window.__navDrawer && !!document.querySelector('[data-nav-drawer]'));
  await p.waitForTimeout(500);
  return { ctx, p, health };
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  const errors = [];
  console.log('\n=== #111 nav-drawer version footer against ' + BASE + ' ===');

  for (const vp of [{ width: 1440, height: 900 }, { width: 1024, height: 768 }, { width: 800, height: 900 }, { width: 1280, height: 480 }]) {
    await step(`${vp.width}x${vp.height}: footer inside the drawer, below a reachable route list; one request`, async () => {
      const { ctx, p, health } = await page(browser, vp, errors);
      try {
        assert(health.length === 0, health.length + ' /api/health requests before the drawer was opened');
        await p.evaluate(() => window.__navDrawer.open());
        await p.waitForTimeout(400);
        const r = await p.evaluate(() => {
          const d = document.querySelector('[data-nav-drawer]');
          const a = d.querySelector('[data-nav-drawer-version]');
          if (!a) return null;
          const dr = d.getBoundingClientRect(), fr = a.closest('.nav-drawer-footer').getBoundingClientRect();
          const list = d.querySelector('.nav-drawer-list');
          const lr = list.getBoundingClientRect();
          const items = [...list.querySelectorAll('[data-nav-drawer-item]')];
          const last = items[items.length - 1];
          last.scrollIntoView({ block: 'nearest' });
          const lastR = last.getBoundingClientRect();
          const hit = document.elementFromPoint(lastR.left + 10, lastR.top + lastR.height / 2);
          return {
            text: a.textContent, href: a.getAttribute('href'),
            inside: fr.left >= dr.left - 1 && fr.right <= dr.right + 1 && fr.bottom <= dr.bottom + 1 && fr.top >= dr.top,
            belowList: fr.top >= lr.bottom - 1,
            lastReachable: last.contains(hit),
            footerVisible: fr.height > 0 && getComputedStyle(a).visibility !== 'hidden',
          };
        });
        assert(r, 'no version link in the drawer');
        assert(/^CoreScope( \S.*)?$/.test(r.text) && !/undefined|null|unknown/i.test(r.text), 'label: ' + r.text);
        assert(r.href === 'https://github.com/dborup/CoreScope/releases', 'href: ' + r.href);
        assert(r.inside && r.footerVisible, 'footer not inside the drawer: ' + JSON.stringify(r));
        assert(r.belowList, 'footer overlaps the route list');
        assert(r.lastReachable, 'the last route is covered by the footer');
        await p.evaluate(() => { window.__navDrawer.close(); window.__navDrawer.open(); window.__navDrawer.close(); window.__navDrawer.open(); });
        await p.waitForTimeout(300);
        assert(health.length === 1, health.length + ' /api/health requests after opening four times');
      } finally { await ctx.close(); }
    });
  }

  await step('at a narrow width (drawer disabled) opening requests nothing', async () => {
    const { ctx, p, health } = await page(browser, { width: 700, height: 900 }, errors);
    try {
      await p.evaluate(() => { window.__navDrawer.open(); window.__navDrawer.open(); });
      await p.waitForTimeout(400);
      assert(health.length === 0, health.length + ' requests at 700px');
    } finally { await ctx.close(); }
  });

  await step('a routed health response shows the version as text with a tooltip', async () => {
    const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 } });
    const p = await ctx.newPage();
    p.on('pageerror', (e) => errors.push(e.message));
    await p.route('**/api/health', (route) => route.fulfill({
      status: 200, contentType: 'application/json',
      body: JSON.stringify({ version: 'v3.1.4<b>x</b>', commit: 'abc1234', buildTime: '2026-09-01T10:00:00Z' }),
    }));
    try {
      await p.goto(BASE + '/#/packets');
      await p.waitForFunction(() => !!window.__navDrawer);
      await p.evaluate(() => window.__navDrawer.open());
      await p.waitForFunction(() => /v3/.test((document.querySelector('[data-nav-drawer-version]') || {}).textContent || ''));
      const r = await p.evaluate(() => {
        const a = document.querySelector('[data-nav-drawer-version]');
        return { text: a.textContent, title: a.title, kids: a.children.length };
      });
      assert(r.text === 'CoreScope v3.1.4<b>x</b>' && r.kids === 0, JSON.stringify(r));
      assert(/abc1234/.test(r.title) && /2026-09-01/.test(r.title), 'tooltip: ' + r.title);
    } finally { await ctx.close(); }
  });

  await step('no page errors', async () => { assert(errors.length === 0, errors.join(' | ')); });

  await browser.close();
  console.log('\n--- ' + passed + ' passed, ' + failed + ' failed ---');
  process.exit(failed ? 1 : 0);
})();
