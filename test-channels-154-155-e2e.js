/**
 * #154 + #155 — E2E on the Channels page.
 *
 * #154: overlapping loadChannels() calls. The /api/channels?region=SJC
 * response is held back ~2.5 s by a Playwright route; before it lands, the
 * SFO pill is added (region SJC,SFO). The SJC,SFO list renders first, then
 * the late SJC response arrives. Before the fix it replaced the list, so the
 * pills showed SJC+SFO while the sidebar showed the SJC set (no #chat). The
 * newer list must stay, and each pill click sends exactly one request.
 * Desktop and mobile width.
 *
 * #155: unread badge on the #1367 mobile rows (390 px). A test PSK is added
 * through the Add Channel modal, another channel is opened, and live
 * client-decryptable GRP_TXT frames for the PSK are injected into the page's
 * real WebSocket (page.routeWebSocket, forwarded to the server otherwise).
 * The PSK row must show the badge, update it, and clear it when the channel
 * is opened. The same steps run at desktop width as a control.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-channels-154-155-e2e.js
 */
'use strict';

const { chromium } = require('playwright');
const {
  channelHashByteFor, buildPlaintext, encryptECB, computeMac,
} = require('./test-channels-client-state-152-decrypt-e2e.js');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const PSK_KEY = '0155c0de0155c0de0155c0de0155c0de';
const PSK_HASH = 'user:psk:0155c0de';
const PSK_LABEL = 'Issue155 Team';
const SJC_DELAY_MS = 2500;

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

function isChannelListRequest(url) {
  const u = new URL(url);
  return u.pathname === '/api/channels';
}

async function openChannelsPage(ctx) {
  const page = await ctx.newPage();
  page.setDefaultTimeout(8000);
  page.on('pageerror', (e) => console.error('[pageerror]', e.message));
  await page.goto(BASE + '/#/channels', { waitUntil: 'domcontentloaded' });
  await page.evaluate(() => { try { localStorage.clear(); } catch (e) {} });
  await page.reload({ waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#chList .ch-item, #chList .ch-row', { timeout: 10000 });
  return page;
}

function rowSelector(hash) { return '#chList [data-hash="' + hash + '"]'; }

// Opens a channel by clicking its name, not the row: page.click() aims at
// the row's centre, and where a desktop My Channels row wraps (font
// metrics; the unread badge makes it wider) the centre is the Share button.
async function openChannel(page, hash) {
  const sel = rowSelector(hash);
  await page.click(sel + ' .ch-item-name, ' + sel + ' .ch-row-name');
  await page.waitForFunction((h) => location.hash === '#/channels/' + encodeURIComponent(h), hash);
}

async function runRace(browser, vp) {
  const ctx = await browser.newContext({ viewport: { width: vp.width, height: vp.height } });
  // Hold back exactly the SJC-only list; every other request goes through.
  await ctx.route('**/api/channels?**', async (route) => {
    const u = new URL(route.request().url());
    if (u.pathname === '/api/channels' && u.searchParams.get('region') === 'SJC') {
      await new Promise((r) => setTimeout(r, SJC_DELAY_MS));
    }
    await route.continue();
  });
  const page = await openChannelsPage(ctx);
  const listRequests = [];
  page.on('request', (r) => { if (isChannelListRequest(r.url())) listRequests.push(new URL(r.url()).searchParams.get('region')); });

  await step(vp.name + ': #154 a late SJC response does not replace the SJC+SFO list', async () => {
    const sjcPill = page.locator('#chRegionFilter [data-region="SJC"]');
    const sfoPill = page.locator('#chRegionFilter [data-region="SFO"]');
    await sjcPill.waitFor({ state: 'attached' });
    const sjcResponse = page.waitForResponse((r) => isChannelListRequest(r.url()) && new URL(r.url()).searchParams.get('region') === 'SJC', { timeout: SJC_DELAY_MS + 8000 });
    await sjcPill.click();
    await page.waitForFunction(() => window.RegionFilter && window.RegionFilter.getRegionParam() === 'SJC');
    const bothResponse = page.waitForResponse((r) => {
      if (!isChannelListRequest(r.url())) return false;
      const region = new URL(r.url()).searchParams.get('region') || '';
      return region.indexOf('SJC') !== -1 && region.indexOf('SFO') !== -1;
    });
    await sfoPill.click();
    await bothResponse;
    // The SJC+SFO list renders while the SJC response is still held back.
    await page.waitForSelector(rowSelector('#chat'), { timeout: 3000 });
    await sjcResponse;
    await page.waitForTimeout(500);
    const s = await page.evaluate(() => ({
      region: window.RegionFilter.getRegionParam(),
      hashes: Array.from(document.querySelectorAll('#chList [data-hash]')).map((e) => e.getAttribute('data-hash')),
    }));
    assert(s.region.indexOf('SJC') !== -1 && s.region.indexOf('SFO') !== -1, 'region stays SJC+SFO, got ' + s.region);
    assert(s.hashes.indexOf('#chat') !== -1, 'the SJC+SFO list (with #chat) must stay rendered, got ' + JSON.stringify(s.hashes));
    assert(listRequests.length === 2, 'exactly one /api/channels request per pill click, got ' + JSON.stringify(listRequests));
  });

  await ctx.close();
}

// A live GRP_TXT frame the server would broadcast, encrypted with PSK_KEY so
// only this browser can decrypt it.
function livePskFrame(n, sender, text) {
  const key = Buffer.from(PSK_KEY, 'hex');
  const ciphertext = encryptECB(key, buildPlaintext(Math.floor(Date.now() / 1000), 0, sender + ': ' + text));
  return JSON.stringify({
    type: 'packet',
    data: {
      id: 1550000 + n,
      hash: 'e2e155' + String(n).padStart(10, '0'),
      decoded: {
        header: { payloadTypeName: 'GRP_TXT' },
        payload: {
          type: 'GRP_TXT',
          channelHash: channelHashByteFor(key),
          encryptedData: ciphertext.toString('hex'),
          mac: computeMac(key, ciphertext).toString('hex'),
        },
      },
    },
  });
}

async function badgeState(page) {
  return page.evaluate((hash) => {
    const row = document.querySelector('#chList [data-hash="' + hash + '"]');
    const badge = row && row.querySelector('.ch-unread-badge');
    if (!badge) return { row: !!row, rowClass: row ? row.className : '', badge: null };
    const rb = row.getBoundingClientRect();
    const bb = badge.getBoundingClientRect();
    return {
      row: true,
      rowClass: row.className,
      badge: {
        text: badge.textContent,
        title: badge.getAttribute('title'),
        aria: badge.getAttribute('aria-label'),
        inLine1: !!badge.closest('.ch-row-line1, .ch-item-top'),
        visible: bb.width > 0 && bb.height > 0 && getComputedStyle(badge).visibility !== 'hidden',
        insideRow: bb.left >= rb.left && bb.right <= rb.right + 0.5,
        insideViewport: bb.right <= window.innerWidth + 0.5,
      },
    };
  }, PSK_HASH);
}

async function backToListIfMobile(page, vp) {
  if (!vp.mobile) return;
  await page.click('.ch-back');
  await page.waitForSelector('#chList .ch-row', { state: 'visible' });
}

async function runUnread(browser, vp) {
  const ctx = await browser.newContext({ viewport: { width: vp.width, height: vp.height } });
  let pageSocket = null;
  await ctx.routeWebSocket(/.*/, (ws) => {
    ws.connectToServer();
    pageSocket = ws;
  });
  const page = await openChannelsPage(ctx);
  const expectedRowClass = vp.mobile ? 'ch-row' : 'ch-item';

  await step(vp.name + ': #155 setup — add a PSK, then open another channel', async () => {
    await page.click('#chAddChannelBtn');
    await page.fill('#chPskKey', PSK_KEY);
    await page.fill('#chPskName', PSK_LABEL);
    await page.click('#chPskAddBtn');
    await page.waitForFunction((hash) => location.hash === '#/channels/' + encodeURIComponent(hash), PSK_HASH, { timeout: 10000 });
    await backToListIfMobile(page, vp);
    await openChannel(page, '#test');
    await backToListIfMobile(page, vp);
    assert(pageSocket, 'the page opened its WebSocket');
    const s = await badgeState(page);
    assert(s.row && s.rowClass.split(' ').indexOf(expectedRowClass) !== -1, 'PSK row rendered as .' + expectedRowClass + ', got ' + JSON.stringify(s));
    assert(s.badge === null, 'no badge before live traffic, got ' + JSON.stringify(s.badge));
  });

  await step(vp.name + ': #155 a live-decrypted message for the closed PSK shows the badge, a second one updates it', async () => {
    pageSocket.send(livePskFrame(1, 'Bob', 'first live'));
    await page.waitForFunction((hash) => {
      const b = document.querySelector('#chList [data-hash="' + hash + '"] .ch-unread-badge');
      return b && b.textContent === '1';
    }, PSK_HASH, { timeout: 8000 });
    let s = await badgeState(page);
    assert(s.badge.title === '1 new' && s.badge.aria === '1 unread', 'title/aria-label, got ' + JSON.stringify(s.badge));
    assert(s.badge.inLine1 && s.badge.visible && s.badge.insideRow && s.badge.insideViewport, 'badge visible inside the row, got ' + JSON.stringify(s.badge));
    pageSocket.send(livePskFrame(2, 'Bob', 'second live'));
    await page.waitForFunction((hash) => {
      const b = document.querySelector('#chList [data-hash="' + hash + '"] .ch-unread-badge');
      return b && b.textContent === '2';
    }, PSK_HASH, { timeout: 8000 });
    s = await badgeState(page);
    assert(s.badge.aria === '2 unread', 'badge updated, got ' + JSON.stringify(s.badge));
  });

  await step(vp.name + ': #155 opening the PSK channel clears the badge', async () => {
    await openChannel(page, PSK_HASH);
    await backToListIfMobile(page, vp);
    const s = await badgeState(page);
    assert(s.row && s.badge === null, 'badge cleared, got ' + JSON.stringify(s));
  });

  await page.evaluate(() => { try { localStorage.clear(); } catch (e) {} });
  await ctx.close();
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log('\n=== #154 + #155 E2E against ' + BASE + ' ===');
  const desktop = { name: 'desktop', width: 1280, height: 800, mobile: false };
  const mobile = { name: 'mobile', width: 390, height: 844, mobile: true };
  await runRace(browser, desktop);
  await runRace(browser, mobile);
  await runUnread(browser, mobile);
  await runUnread(browser, desktop);
  console.log('\n=== Results: ' + passed + ' passed, ' + failed + ' failed ===');
  await browser.close();
  process.exit(failed > 0 ? 1 : 0);
})().catch((e) => { console.error('FATAL:', e); process.exit(1); });
