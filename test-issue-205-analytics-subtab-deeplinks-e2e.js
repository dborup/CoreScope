#!/usr/bin/env node
/**
 * E2E (#205): the Scopes tab's sub-tab and window, and the Wardriving tab's
 * window, are deep-linked as ?sub=, ?swin= and ?wdwin= next to ?tab=.
 *
 * - #/analytics?tab=scopes&sub=hopdepth opens Hop Depth on a cold load and
 *   when navigated to from another page;
 * - clicks write the URL, reload and back/forward restore the view;
 * - a URL value wins over the sessionStorage one;
 * - a hostile ?sub= falls back to Overview without a page error;
 * - the default view keeps the URL it had before (#/analytics?tab=scopes),
 *   and switching to another tab drops the keys;
 * - Hash Stats' multi-byte adopters filter is deep-linked as ?mbf= (#208).
 *
 * Usage: BASE_URL=http://localhost:13581 node test-issue-205-analytics-subtab-deeplinks-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const SUBS = ['overview', 'hopdepth', 'regions', 'hygiene'];

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

const hash = (page) => page.evaluate(() => location.hash);

// The active sub-tab button(s), the visible panel(s) and the active window
// button(s) of the visible panel, as the visitor sees them.
function scopesView(page) {
  return page.evaluate((subs) => {
    const active = Array.from(document.querySelectorAll('#scopesSubtabs [data-subtab].active')).map((b) => b.dataset.subtab);
    const visible = subs.filter((k) => {
      const p = document.getElementById('scopes-panel-' + k);
      return p && p.style.display !== 'none' && p.offsetParent !== null;
    });
    const win = Array.from(document.querySelectorAll('[id^="scopes-panel-"] [data-win].active'))
      .filter((b) => b.offsetParent !== null).map((b) => b.dataset.win);
    return { active, visible, win };
  }, SUBS);
}

async function waitScopes(page, sub) {
  await page.waitForSelector('#scopesSubtabs [data-subtab].active', { timeout: 15000 });
  try {
    await page.waitForFunction((s) => {
      const b = document.querySelector('#scopesSubtabs [data-subtab].active');
      return b && b.dataset.subtab === s;
    }, sub, { timeout: 8000 });
  } catch (_) {
    throw new Error('sub-tab ' + sub + ' not active; view ' + JSON.stringify(await scopesView(page)) + ', hash ' + await hash(page));
  }
}

async function expectScopes(page, sub, win) {
  await waitScopes(page, sub);
  const v = await scopesView(page);
  assert(JSON.stringify(v.active) === JSON.stringify([sub]), 'active sub-tab ' + JSON.stringify(v.active));
  assert(JSON.stringify(v.visible) === JSON.stringify([sub]), 'visible panel ' + JSON.stringify(v.visible));
  if (win) assert(JSON.stringify(v.win) === JSON.stringify([win]), 'active window ' + JSON.stringify(v.win));
}

// A full page load, then the one theme-refresh it fires (app.js), so the
// test does not race the re-render.
async function coldLoad(page, path) {
  await page.goto('about:blank');
  await page.goto(BASE + '/' + path, { waitUntil: 'load' });
  await page.waitForFunction(() => window.__themeRefreshed, null, { timeout: 8000 }).catch(() => {});
}

(async () => {
  const requireChromium = process.env.CHROMIUM_REQUIRE === '1';
  let browser;
  try {
    browser = await chromium.launch({
      headless: true,
      executablePath: process.env.CHROMIUM_PATH || undefined,
      args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
    });
  } catch (err) {
    if (requireChromium) {
      console.error('test-issue-205-analytics-subtab-deeplinks-e2e.js: FAIL — Chromium required but unavailable: ' + err.message);
      process.exit(1);
    }
    console.log('test-issue-205-analytics-subtab-deeplinks-e2e.js: SKIP (Chromium unavailable: ' + err.message.split('\n')[0] + ')');
    process.exit(0);
  }

  const ctx = await browser.newContext({ viewport: { width: 1400, height: 1000 } });
  await ctx.addInitScript(() => {
    window.addEventListener('theme-refresh', () => { window.__themeRefreshed = true; }, { once: true });
  });
  const page = await ctx.newPage();
  page.setDefaultTimeout(15000);
  const pageErrors = [];
  page.on('pageerror', (e) => { pageErrors.push(e.message); console.error('[pageerror]', e.message); });

  console.log('\n=== #205 analytics sub-tab deep links E2E against ' + BASE + ' ===');

  await step('cold load #/analytics?tab=scopes&sub=hopdepth opens Hop Depth, URL unchanged', async () => {
    await coldLoad(page, '#/analytics?tab=scopes&sub=hopdepth');
    await expectScopes(page, 'hopdepth', '24h');
    assert(await hash(page) === '#/analytics?tab=scopes&sub=hopdepth', 'hash ' + await hash(page));
  });

  await step('clicking sub-tabs and a window writes sub= and swin=', async () => {
    await page.click('#scopesSubtabs [data-subtab="regions"]');
    await expectScopes(page, 'regions');
    assert(await hash(page) === '#/analytics?tab=scopes&sub=regions', 'after Regions: ' + await hash(page));
    await page.click('#scopesSubtabs [data-subtab="hopdepth"]');
    const req = page.waitForRequest((r) => r.url().includes('/api/scope-stats?window=7d'), { timeout: 8000 });
    await page.click('#scopes-panel-hopdepth [data-win="7d"]');
    await req;
    await expectScopes(page, 'hopdepth', '7d');
    assert(await hash(page) === '#/analytics?tab=scopes&sub=hopdepth&swin=7d', 'after 7d: ' + await hash(page));
  });

  await step('reload keeps Hop Depth and 7d', async () => {
    const req = page.waitForRequest((r) => r.url().includes('/api/scope-stats?window=7d'), { timeout: 15000 });
    await page.reload({ waitUntil: 'load' });
    await req;
    await expectScopes(page, 'hopdepth', '7d');
    assert(await hash(page) === '#/analytics?tab=scopes&sub=hopdepth&swin=7d', 'hash ' + await hash(page));
  });

  await step('back and forward across another page restore the view', async () => {
    await page.evaluate(() => { location.hash = '#/nodes'; });
    await page.waitForFunction(() => location.hash === '#/nodes' && !document.getElementById('scopesSubtabs'));
    await page.goBack();
    await expectScopes(page, 'hopdepth', '7d');
    assert(await hash(page) === '#/analytics?tab=scopes&sub=hopdepth&swin=7d', 'after back: ' + await hash(page));
    await page.goForward();
    await page.waitForFunction(() => location.hash === '#/nodes' && !document.getElementById('scopesSubtabs'));
    await page.goBack();
    await expectScopes(page, 'hopdepth', '7d');
  });

  await step('navigating from another page to ?sub=regions: the URL wins over sessionStorage (hopdepth)', async () => {
    assert(await page.evaluate(() => sessionStorage.getItem('scopes_subtab')) === 'hopdepth', 'precondition: stored sub-tab');
    await page.evaluate(() => { location.hash = '#/nodes'; });
    await page.waitForFunction(() => !document.getElementById('scopesSubtabs'));
    await page.evaluate(() => { location.hash = '#/analytics?tab=scopes&sub=regions'; });
    await expectScopes(page, 'regions');
    assert(await page.evaluate(() => sessionStorage.getItem('scopes_subtab')) === 'regions', 'stored sub-tab not updated');
    // The stored window (7d) still applies without swin= and is written back.
    await page.waitForFunction(() => location.hash === '#/analytics?tab=scopes&sub=regions&swin=7d');
  });

  await step('switching to another tab drops sub= and swin=; the Scopes tab brings them back', async () => {
    await page.click('#analyticsTabs [data-tab="topology"]');
    await page.waitForFunction(() => location.hash === '#/analytics?tab=topology');
    await page.click('#analyticsTabs [data-tab="scopes"]');
    await expectScopes(page, 'regions');
    assert(await hash(page) === '#/analytics?tab=scopes&sub=regions&swin=7d', 'hash ' + await hash(page));
  });

  await step('a hostile ?sub= falls back to Overview without a page error', async () => {
    const before = pageErrors.length;
    await page.evaluate(() => { location.hash = '#/nodes'; });
    await page.waitForFunction(() => !document.getElementById('scopesSubtabs'));
    await page.evaluate(() => { location.hash = '#/analytics?tab=scopes&sub=' + encodeURIComponent('x"],[data-subtab="hygiene') + '&swin=24h'; });
    await expectScopes(page, 'overview', '24h');
    assert(await hash(page) === '#/analytics?tab=scopes', 'hash not canonical: ' + await hash(page));
    assert(pageErrors.length === before, 'page errors: ' + pageErrors.slice(before).join(' | '));
  });

  await step('default view keeps today\'s URL (fresh context, Scopes clicked from Overview)', async () => {
    const ctx2 = await browser.newContext({ viewport: { width: 1400, height: 1000 } });
    const p2 = await ctx2.newPage();
    try {
      await p2.goto(BASE + '/#/analytics', { waitUntil: 'load' });
      await p2.click('#analyticsTabs [data-tab="scopes"]');
      await expectScopes(p2, 'overview', '24h');
      await p2.waitForTimeout(500);
      assert(await hash(p2) === '#/analytics?tab=scopes', 'hash ' + await hash(p2));
    } finally {
      await ctx2.close();
    }
  });

  await step('cold load #/analytics?tab=wardriving&wdwin=7d selects 7d; clicking 1h writes wdwin=1h', async () => {
    const req = page.waitForRequest((r) => /\/api\/analytics\/wardriving\?.*window=7d/.test(r.url()), { timeout: 15000 });
    await coldLoad(page, '#/analytics?tab=wardriving&wdwin=7d');
    await req;
    await page.waitForSelector('[data-wdwin="7d"].active');
    assert(await hash(page) === '#/analytics?tab=wardriving&wdwin=7d', 'hash ' + await hash(page));
    await page.click('[data-wdwin="1h"]');
    await page.waitForSelector('[data-wdwin="1h"].active');
    assert(await hash(page) === '#/analytics?tab=wardriving&wdwin=1h', 'after 1h: ' + await hash(page));
  });

  // #208 item 6: Hash Stats' multi-byte adopters filter as mbf=.
  const mbActive = (p) => p.evaluate(() => Array.from(document.querySelectorAll('#mbCapFilters [data-mb-filter].active')).map((b) => b.dataset.mbFilter));
  async function expectMb(p, f) {
    try {
      await p.waitForFunction((want) => {
        const a = Array.from(document.querySelectorAll('#mbCapFilters [data-mb-filter].active'));
        return a.length === 1 && a[0].dataset.mbFilter === want;
      }, f, { timeout: 15000 });
    } catch (_) {
      throw new Error('filter ' + f + ' not active; active ' + JSON.stringify(await mbActive(p)) + ', hash ' + await hash(p));
    }
  }
  // The card wires its click handler 100 ms after it renders.
  async function clickMb(p, f) {
    await p.waitForTimeout(300);
    await p.click('#mbCapFilters [data-mb-filter="' + f + '"]');
    await expectMb(p, f);
  }

  await step('cold load #/analytics?tab=hashsizes&mbf=confirmed selects Confirmed, URL unchanged', async () => {
    await coldLoad(page, '#/analytics?tab=hashsizes&mbf=confirmed');
    await expectMb(page, 'confirmed');
    assert(await page.locator('#mbAdoptersTable tbody tr').count() > 0, 'no confirmed adopters in the table (fixture?)');
    assert(await hash(page) === '#/analytics?tab=hashsizes&mbf=confirmed', 'hash ' + await hash(page));
  });

  await step('clicking a filter writes mbf=, reload keeps it, All drops it', async () => {
    await clickMb(page, 'unknown');
    await page.waitForFunction(() => location.hash === '#/analytics?tab=hashsizes&mbf=unknown', null, { timeout: 5000 })
      .catch(async () => { throw new Error('after Unknown: ' + await hash(page)); });
    await page.reload({ waitUntil: 'load' });
    await page.waitForFunction(() => window.__themeRefreshed, null, { timeout: 8000 }).catch(() => {});
    await expectMb(page, 'unknown');
    await clickMb(page, 'all');
    await page.waitForFunction(() => location.hash === '#/analytics?tab=hashsizes', null, { timeout: 5000 })
      .catch(async () => { throw new Error('after All: ' + await hash(page)); });
    assert(await page.evaluate(() => Object.keys(sessionStorage).filter((k) => /mb/i.test(k)).length === 0), 'filter stored in sessionStorage');
  });

  await step('switching from Hash Stats to another tab drops mbf=', async () => {
    await clickMb(page, 'confirmed');
    await page.waitForFunction(() => location.hash === '#/analytics?tab=hashsizes&mbf=confirmed', null, { timeout: 5000 });
    await page.click('#analyticsTabs [data-tab="topology"]');
    await page.waitForFunction(() => location.hash === '#/analytics?tab=topology', null, { timeout: 5000 })
      .catch(async () => { throw new Error('after the tab switch: ' + await hash(page)); });
  });

  await step('a hostile ?mbf= falls back to All without a page error', async () => {
    const before = pageErrors.length;
    await coldLoad(page, '#/analytics?tab=hashsizes&mbf=' + encodeURIComponent('x"],[data-mb-filter="unknown'));
    await expectMb(page, 'all');
    assert(await hash(page) === '#/analytics?tab=hashsizes', 'hash not canonical: ' + await hash(page));
    assert(pageErrors.length === before, 'page errors: ' + pageErrors.slice(before).join(' | '));
  });

  await browser.close();

  console.log('\n' + passed + ' passed, ' + failed + ' failed');
  if (failed > 0) {
    console.error('test-issue-205-analytics-subtab-deeplinks-e2e.js: FAIL');
    process.exit(1);
  }
  console.log('test-issue-205-analytics-subtab-deeplinks-e2e.js: PASS');
})();
