#!/usr/bin/env node
/**
 * Regression (#1833 round 2): at 641..768px the bottom navigation shortens
 * .live-page, while fixed toggle buttons remain viewport-relative. The
 * higher-z-index VCR bar then covers the legend toggle and eats real clicks.
 *
 * Run: BASE_URL=http://localhost:13581 node test-issue-1833-legend-toggle-clickable-e2e.js
 */
'use strict';

const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const TOLERANCE_PX = 2;

let passed = 0;
let failed = 0;

function assert(condition, message) {
  if (!condition) throw new Error(message || 'assertion failed');
}

async function step(name, fn) {
  try {
    await fn();
    passed++;
    console.log('  \u2713 ' + name);
  } catch (err) {
    failed++;
    console.error('  \u2717 ' + name + ': ' + err.message);
  }
}

async function gotoLive(page) {
  await page.goto(BASE + '/#/live', { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#vcrBar');
  await page.waitForSelector('#legendToggleBtn');
  await page.waitForFunction(() => {
    const livePage = document.querySelector('.live-page');
    if (!livePage) return false;
    const value = getComputedStyle(livePage).getPropertyValue('--vcr-bar-height');
    return value && parseFloat(value) > 0;
  }, null, { timeout: 8000 });
  await page.waitForTimeout(150);
}

async function revealFeedShowButton(page) {
  const usable = await page.evaluate(() => {
    const button = document.getElementById('feedHideBtn');
    if (!button) return false;
    const rect = button.getBoundingClientRect();
    return getComputedStyle(button).display !== 'none' && rect.width > 0 && rect.height > 0;
  });
  if (!usable) return false;
  await page.click('#feedHideBtn');
  await page.waitForTimeout(100);
  return page.evaluate(() => {
    const button = document.getElementById('feedShowBtn');
    if (!button) return false;
    const rect = button.getBoundingClientRect();
    return getComputedStyle(button).display !== 'none' && rect.width > 0 && rect.height > 0;
  });
}

async function probe(page, id) {
  return page.evaluate((buttonId) => {
    const button = document.getElementById(buttonId);
    const bar = document.getElementById('vcrBar');
    const livePage = document.querySelector('.live-page');
    if (!button || !bar || !livePage) return null;

    const buttonRect = button.getBoundingClientRect();
    const barRect = bar.getBoundingClientRect();
    const pageRect = livePage.getBoundingClientRect();
    const hit = document.elementFromPoint(
      buttonRect.left + buttonRect.width / 2,
      buttonRect.top + buttonRect.height / 2
    );

    return {
      id: buttonId,
      position: getComputedStyle(button).position,
      visible: buttonRect.width > 0 && buttonRect.height > 0,
      bottom: buttonRect.bottom,
      barTop: barRect.top,
      slack: pageRect.bottom - buttonRect.bottom - barRect.height,
      gapBelowLivePage: window.innerHeight - pageRect.bottom,
      hitIsSelf: hit === button || button.contains(hit),
      hitName: hit ? hit.tagName + (hit.id ? '#' + hit.id : '') : 'none',
    };
  }, id);
}

async function launchBrowser() {
  try {
    return await chromium.launch({
      headless: true,
      executablePath: process.env.CHROMIUM_PATH || undefined,
      args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
    });
  } catch (err) {
    if (process.env.CHROMIUM_REQUIRE === '1') throw err;
    console.log('SKIP: Chromium unavailable: ' + err.message.split('\n')[0]);
    process.exit(0);
  }
}

(async () => {
  const browser = await launchBrowser();
  const context = await browser.newContext();
  const page = await context.newPage();
  page.setDefaultTimeout(10000);

  console.log('\n=== #1833 legend/feed toggle hit testing ===');

  await page.setViewportSize({ width: 720, height: 900 });
  await gotoLive(page);

  const narrowLegend = await probe(page, 'legendToggleBtn');
  await step('[720px] reserve band and legend geometry are active', async () => {
    assert(narrowLegend, 'legend button, VCR bar, or live page missing');
    assert(narrowLegend.gapBelowLivePage > 0,
      '.live-page is not shortened by the mobile bottom navigation');
    assert(narrowLegend.position === 'absolute',
      'legend button must be absolute, got ' + narrowLegend.position);
    assert(narrowLegend.visible, 'legend button is not visible');
    assert(narrowLegend.bottom <= narrowLegend.barTop + 0.5,
      'legend button overlaps VCR bar by ' + (narrowLegend.bottom - narrowLegend.barTop) + 'px');
    assert(narrowLegend.hitIsSelf,
      'legend button is covered by ' + narrowLegend.hitName);
  });

  await step('[720px] a real hit-tested click toggles the legend', async () => {
    const before = await page.$eval('#liveLegend', (el) => el.classList.contains('hidden'));
    await page.click('#legendToggleBtn');
    const after = await page.$eval('#liveLegend', (el) => el.classList.contains('hidden'));
    assert(before !== after, 'legend hidden state did not change');
  });

  let narrowFeed = null;
  await step('[720px] feed show button clears the bar and restores the feed', async () => {
    assert(await revealFeedShowButton(page), 'could not reveal feed show button');
    narrowFeed = await probe(page, 'feedShowBtn');
    assert(narrowFeed && narrowFeed.position === 'absolute',
      'feed show button must be absolute');
    assert(narrowFeed.bottom <= narrowFeed.barTop + 0.5,
      'feed show button overlaps VCR bar');
    assert(narrowFeed.hitIsSelf,
      'feed show button is covered by ' + narrowFeed.hitName);
    await page.click('#feedShowBtn');
    const hidden = await page.$eval('#liveFeed', (el) => el.classList.contains('hidden'));
    assert(!hidden, 'real click did not restore the live feed');
  });

  await page.setViewportSize({ width: 1440, height: 900 });
  await gotoLive(page);
  const wideLegend = await probe(page, 'legendToggleBtn');
  await revealFeedShowButton(page);
  const wideFeed = await probe(page, 'feedShowBtn');

  await step('[1440px] desktop remains clickable and uses the same anchor geometry', async () => {
    for (const pair of [[narrowLegend, wideLegend], [narrowFeed, wideFeed]]) {
      const narrow = pair[0];
      const wide = pair[1];
      assert(narrow && wide, 'missing narrow or wide probe');
      assert(wide.visible, '#' + wide.id + ' is not visible on desktop');
      assert(wide.bottom <= wide.barTop + 0.5,
        '#' + wide.id + ' overlaps VCR bar on desktop');
      assert(wide.hitIsSelf,
        '#' + wide.id + ' is covered by ' + wide.hitName + ' on desktop');
      assert(Math.abs(narrow.slack - wide.slack) <= TOLERANCE_PX,
        '#' + wide.id + ' moves with the viewport reserve: ' +
        narrow.slack + 'px narrow vs ' + wide.slack + 'px wide');
    }
  });

  await browser.close();
  console.log('\nResults: ' + passed + ' passed, ' + failed + ' failed');
  process.exit(failed ? 1 : 0);
})().catch((err) => {
  console.error('FAIL: ' + err.stack);
  process.exit(1);
});
