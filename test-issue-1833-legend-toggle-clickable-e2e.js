#!/usr/bin/env node
/**
 * Regression (#1833 round 2): at 641..768px the bottom navigation shortens
 * .live-page, while fixed toggle buttons remain viewport-relative. The
 * higher-z-index VCR bar then covers the legend toggle and eats real clicks.
 *
 * Regression (#1833 round 3): round 2's fix anchors both toggle buttons to
 * .live-page instead. At short viewport heights in the same 641..768px band,
 * the same offset that clears .feed-show-btn of the VCR bar can push it up
 * far enough to land under .top-nav (#62, position:fixed, z-index:1100) and
 * eat the click there instead. A/B verified against master (position:fixed)
 * and against #1833 round 2 (position:absolute): the natural "hide feed,
 * then show feed" click sequence succeeds on master at every viewport down
 * to 720x250 and fails on round 2 at every height from 720x250 through
 * 720x320 plus 641/700/768x300 (deterministic across 10 repeats). It does
 * not affect the legend toggle, which stays clear down to ~215px height.
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

// Real user action: click #feedHideBtn (a click on .live-page, which is
// exactly what live.js's #62 nav-autohide showNav() listens for) and wait for
// the actual DOM/CSS consequences — never a fixed sleep. #liveFeed picks up
// .hidden synchronously in the click handler; #feedShowBtn's own CSS class
// removal (same handler) is what makes it start taking up layout space.
async function revealFeedShowButtonNoSleep(page) {
  await page.click('#feedHideBtn');
  await page.waitForFunction(() => {
    const feed = document.getElementById('liveFeed');
    return feed && feed.classList.contains('hidden');
  }, null, { timeout: 5000 });
  await page.waitForFunction(() => {
    const btn = document.getElementById('feedShowBtn');
    if (!btn) return false;
    const r = btn.getBoundingClientRect();
    return getComputedStyle(btn).display !== 'none' && r.width > 0 && r.height > 0;
  }, null, { timeout: 5000 });
}

// Geometry against the app's own top navigation (.top-nav), not just the VCR
// bar. On /live, live.js pins .top-nav to position:fixed, z-index:1100 (#62)
// — above .feed-show-btn's z-index:500 — so if the button's box falls inside
// the nav's band it silently eats the click instead of the button.
async function probeAgainstTopNav(page, id) {
  return page.evaluate((buttonId) => {
    const button = document.getElementById(buttonId);
    const nav = document.querySelector('.top-nav');
    if (!button || !nav) return null;
    const buttonRect = button.getBoundingClientRect();
    const navRect = nav.getBoundingClientRect();
    const overlapPx = Math.min(buttonRect.bottom, navRect.bottom) - Math.max(buttonRect.top, navRect.top);
    const hit = document.elementFromPoint(
      buttonRect.left + buttonRect.width / 2,
      buttonRect.top + buttonRect.height / 2
    );
    return {
      id: buttonId,
      visible: buttonRect.width > 0 && buttonRect.height > 0,
      overlapsTopNav: overlapPx > 0,
      overlapPx,
      hitIsSelf: hit === button || button.contains(hit),
      hitName: hit ? hit.tagName + (hit.id ? '#' + hit.id : '') : 'none',
    };
  }, id);
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

  // Regression: at short viewport heights in the 641-768px reserve band, the
  // vcr-bar-height + legend-toggle-stack offset that pins .feed-show-btn
  // above .vcr-bar can push it up far enough to land under .top-nav (#62's
  // position:fixed, z-index:1100 overlay), which eats the click instead of
  // the button. Natural flow only: hide the feed, then click show — the same
  // two .live-page clicks a real user makes, which is also exactly what
  // resets the nav-autohide idle timer (showNav()) and keeps the nav visible
  // through the whole sequence. No artificial waits.
  await page.setViewportSize({ width: 720, height: 300 });
  await gotoLive(page);

  const shortLegend = await probe(page, 'legendToggleBtn');
  await step('[720x300] legend toggle still clears the VCR bar at a short viewport', async () => {
    assert(shortLegend && shortLegend.visible, 'legend button missing or not visible');
    assert(shortLegend.bottom <= shortLegend.barTop + 0.5,
      'legend button overlaps VCR bar by ' + (shortLegend.bottom - shortLegend.barTop) + 'px');
    assert(shortLegend.hitIsSelf, 'legend button is covered by ' + shortLegend.hitName);
  });

  // page.goto() to the same #/live hash is a same-document no-op in this SPA
  // (verified: in-memory state and DOM survive it, only a full navigation
  // to a different route tears it down). So a step that fails before
  // restoring #liveFeed would leave it hidden for every later gotoLive()
  // call in this file. Assertions are collected rather than thrown
  // immediately, and the restoring click is always attempted, so this step
  // leaves the same clean state (feed visible) whether it passes or fails —
  // exactly what the fixed code produces on its own via a successful click.
  await step('[720x300] feed show button is not hidden under the top nav and a real click restores the feed', async () => {
    const problems = [];
    await revealFeedShowButtonNoSleep(page);
    const navProbe = await probeAgainstTopNav(page, 'feedShowBtn');
    if (!navProbe || !navProbe.visible) problems.push('feed show button missing or not visible');
    if (navProbe && navProbe.overlapsTopNav) problems.push('feed show button overlaps .top-nav by ' + navProbe.overlapPx + 'px');
    if (navProbe && !navProbe.hitIsSelf) problems.push('feed show button is covered by ' + navProbe.hitName + ' (expected .top-nav to be clear)');

    const hiddenBefore = await page.$eval('#liveFeed', (el) => el.classList.contains('hidden'));
    try {
      await page.click('#feedShowBtn');
    } catch (clickErr) {
      problems.push('real click on feed show button failed: ' + clickErr.message.split('\n')[0]);
    }
    const hiddenAfter = await page.$eval('#liveFeed', (el) => el.classList.contains('hidden'));
    if (!(hiddenBefore === true && hiddenAfter === false)) {
      problems.push('a real click on feed show button did not restore the live feed (hiddenBefore=' + hiddenBefore + ', hiddenAfter=' + hiddenAfter + ')');
    }
    if (hiddenAfter) {
      // Best-effort restore for the sections that follow, regardless of why
      // the click above did not land — mirrors exactly what the click
      // handler itself does (live.js feedShowBtn listener) on both elements,
      // and does not affect this step's own pass/fail result.
      await page.evaluate(() => {
        document.getElementById('liveFeed').classList.remove('hidden');
        document.getElementById('feedShowBtn').classList.add('hidden');
      }).catch(() => {});
    }
    assert(problems.length === 0, problems.join('; '));
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
