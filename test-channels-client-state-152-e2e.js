/**
 * #152 — E2E: an open PSK conversation and My Channels survive a region
 * change on the Channels page.
 *
 * Adds a test PSK through the Add Channel modal (the real UI path), which
 * selects the new user:* channel, then switches region with the region
 * pills. Before the fix the refresh evicted the user:* selection: the
 * conversation closed ("Choose a channel from the sidebar"), the URL was
 * rewritten to #/channels and the My Channels section disappeared.
 *
 * Runs at desktop and mobile width.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-channels-client-state-152-e2e.js
 */
'use strict';

const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const PSK_KEY = '0152c0de0152c0de0152c0de0152c0de';
const PSK_HASH = 'user:psk:0152c0de';
const PSK_LABEL = 'Issue152 Team';

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

async function conversationState(page) {
  return page.evaluate((hash) => {
    const header = document.querySelector('#chHeader .ch-header-text');
    const msgs = document.getElementById('chMessages');
    const row = document.querySelector('#chList [data-hash="' + hash + '"]');
    const mine = document.querySelector('#chList .ch-section-mychannels');
    return {
      urlHash: location.hash,
      header: header ? header.textContent : '',
      messagesText: msgs ? msgs.textContent : '',
      rowPresent: !!row,
      rowSelected: !!row && (row.getAttribute('aria-selected') === 'true' || row.classList.contains('selected')),
      rowInMyChannels: !!(mine && mine.querySelector('[data-hash="' + hash + '"]')),
      myChannelsText: mine ? mine.textContent : '',
    };
  }, PSK_HASH);
}

async function switchRegion(page, code) {
  const pill = page.locator('#chRegionFilter [data-region="' + code + '"]');
  await pill.waitFor({ state: 'attached', timeout: 8000 });
  // Each step selects a region set not requested before, so api()'s TTL
  // cache cannot answer it and a real /api/channels request goes out.
  const response = page.waitForResponse((r) => {
    const u = decodeURIComponent(r.url());
    return u.indexOf('/api/channels?') !== -1 && u.indexOf('/messages') === -1 && u.indexOf(code) !== -1;
  }, { timeout: 8000 });
  // The mobile detail view hides or covers the sidebar's region pills; fall
  // back to a DOM click, which still goes through the real RegionFilter
  // handler.
  const click = pill.click({ timeout: 2000 }).catch(() => pill.evaluate((el) => el.click()));
  await Promise.all([response, click]);
  // loadChannels() renders and reconciles right after the response resolves.
  await page.waitForTimeout(300);
}

async function runViewport(browser, vp) {
  const ctx = await browser.newContext({ viewport: { width: vp.width, height: vp.height } });
  const page = await ctx.newPage();
  page.setDefaultTimeout(8000);
  page.on('pageerror', (e) => console.error('[pageerror ' + vp.name + ']', e.message));

  await page.goto(BASE + '/#/channels', { waitUntil: 'domcontentloaded' });
  await page.evaluate(() => { try { localStorage.clear(); } catch (e) {} });
  await page.reload({ waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#chList .ch-item, #chList .ch-row', { timeout: 10000 });

  await step(vp.name + ': adding a PSK in the UI opens its conversation', async () => {
    await page.click('#chAddChannelBtn');
    await page.fill('#chPskKey', PSK_KEY);
    await page.fill('#chPskName', PSK_LABEL);
    await page.click('#chPskAddBtn');
    await page.waitForFunction((hash) => location.hash === '#/channels/' + encodeURIComponent(hash), PSK_HASH, { timeout: 10000 });
    await page.waitForFunction((label) => {
      const h = document.querySelector('#chHeader .ch-header-text');
      return h && h.textContent.indexOf(label) !== -1;
    }, PSK_LABEL, { timeout: 10000 });
    const s = await conversationState(page);
    assert(s.rowPresent && s.rowSelected, 'PSK row must be listed and selected, got ' + JSON.stringify(s));
    if (vp.mobile === false) assert(s.rowInMyChannels, 'PSK row must be in My Channels');
  });

  const before = await conversationState(page);

  for (const code of ['SJC', 'SFO']) {
    await step(vp.name + ': region change to ' + code + ' keeps the PSK conversation and My Channels', async () => {
      await switchRegion(page, code);
      const s = await conversationState(page);
      assert(s.urlHash === before.urlHash, 'URL must stay on the PSK channel, got ' + s.urlHash);
      assert(s.header.indexOf(PSK_LABEL) !== -1, 'header must still name the PSK channel, got ' + JSON.stringify(s.header));
      assert(s.messagesText.indexOf('Choose a channel') === -1, 'conversation must not close, got ' + JSON.stringify(s.messagesText.slice(0, 120)));
      assert(s.messagesText === before.messagesText, 'message pane must be unchanged');
      assert(s.rowPresent && s.rowSelected, 'PSK row must stay listed and selected, got ' + JSON.stringify(s));
      if (vp.mobile === false) {
        assert(s.rowInMyChannels, 'My Channels must still contain the PSK row');
        assert(s.myChannelsText.indexOf(PSK_LABEL) !== -1, 'My Channels must still show the label');
      }
    });
  }

  await page.evaluate(() => { try { localStorage.clear(); } catch (e) {} });
  await ctx.close();
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log('\n=== #152 E2E against ' + BASE + ' ===');
  await runViewport(browser, { name: 'desktop', width: 1280, height: 800, mobile: false });
  await runViewport(browser, { name: 'mobile', width: 390, height: 844, mobile: true });
  console.log('\n=== Results: ' + passed + ' passed, ' + failed + ' failed ===');
  await browser.close();
  process.exit(failed > 0 ? 1 : 0);
})().catch((e) => { console.error('FATAL:', e); process.exit(1); });
