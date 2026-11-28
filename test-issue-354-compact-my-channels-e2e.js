#!/usr/bin/env node
/* Issue #354 — My Channels rows are as compact as Network rows, and Share /
 * Remove live in an accessible overflow menu.
 *
 *  (a) At 1440 px, in light and dark: a saved channel row is no taller than a
 *      Network row (+ a small tolerance), and the row has no inline Share /
 *      Remove controls. The My Channels and Network sections stay distinct.
 *  (b) Share and Remove work through the menu with the mouse and with the
 *      keyboard: Enter / Space / ArrowDown open it and focus the first item,
 *      arrows move, Escape closes it and focus returns to the trigger, Tab
 *      closes it. Share opens the share dialog. Remove asks for confirmation:
 *      dismissing it keeps the channel and its stored key, accepting removes
 *      both. Opening the menu has no side effect.
 *  (c) At 390 px (touch, flat .ch-row list, #1367) the list stays compact with
 *      no horizontal overflow and renders no row actions; at 768 px with touch
 *      (the narrowest sectioned sidebar) the trigger is a 48 px target, the
 *      menu stays inside the viewport, and Share / Remove work.
 *
 * The saved channel comes from a key in localStorage, the state a returning
 * user has; no DOM is injected. Waits are for elements, never sleeps.
 *
 * Defaults to localhost:13581 — NEVER point at prod (AGENTS.md). CI sets BASE_URL.
 * Run: BASE_URL=http://localhost:13581 node test-issue-354-compact-my-channels-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const ORIGIN = new URL(BASE).origin;
const NAME = '#c354probe';
const HASH = 'user:' + NAME;
const KEY = '00112233445566778899aabbccddeeff';
const TOLERANCE = 4; // px — borders / sub-pixel rounding only
const MIN_TARGET = 48;

const ROW = `#chList .ch-section-mychannels .ch-item[data-hash="${HASH}"]`;
const TRIGGER = `${ROW} [data-row-menu]`;
const MENU = '#chRowMenu';
const SHARE_ITEM = `${MENU} [data-share-channel]`;
const REMOVE_ITEM = `${MENU} [data-remove-channel]`;
const NETWORK_ROW = '#chList .ch-section-network .ch-item';

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

async function open(browser, errors, options, theme) {
  const ctx = await browser.newContext(options);
  const page = await ctx.newPage();
  page.setDefaultTimeout(15000);
  page.on('pageerror', (e) => errors.push('pageerror: ' + e.message));
  page.on('console', (m) => {
    if (m.type() !== 'error') return;
    const url = (m.location() && m.location().url) || '';
    if (url && !url.startsWith(ORIGIN)) return;
    errors.push('console.error: ' + m.text());
  });
  await page.addInitScript(([name, key, th]) => {
    if (!localStorage.getItem('corescope_channel_keys')) {
      localStorage.setItem('corescope_channel_keys', JSON.stringify({ [name]: key }));
    }
    if (th) localStorage.setItem('meshcore-theme', th);
  }, [NAME, KEY, theme || null]);
  await page.goto(BASE + '/#/channels', { waitUntil: 'domcontentloaded' });
  return { ctx, page };
}

const height = (page, sel) => page.$eval(sel, (el) => el.getBoundingClientRect().height);
const focusedIs = (page, sel) => page.evaluate((s) => {
  const el = document.querySelector(s); return !!el && document.activeElement === el;
}, sel);
const menuOpen = (page) => page.evaluate(() => {
  const m = document.getElementById('chRowMenu');
  return !!m && !m.hidden && getComputedStyle(m).display !== 'none';
});
const storedKeys = (page) => page.evaluate(() =>
  Object.keys(JSON.parse(localStorage.getItem('corescope_channel_keys') || '{}')));

async function ready(page, mobile) {
  await page.waitForSelector(mobile ? `#chList .ch-row[data-hash="${HASH}"]` : ROW);
  if (!mobile) await page.waitForSelector(NETWORK_ROW);
}


// The menu must sit next to its trigger: directly below it, or directly above
// when there is no room below.
async function assertAnchored(page) {
  const g = await page.evaluate(([t, m]) => {
    const a = document.querySelector(t).getBoundingClientRect(), b = document.querySelector(m).getBoundingClientRect();
    return { below: b.top - a.bottom, above: a.top - b.bottom, rightDelta: b.right - a.right, left: b.left, vw: innerWidth };
  }, [TRIGGER, MENU]);
  assert((g.below >= -1 && g.below <= 6) || (g.above >= -1 && g.above <= 6), 'menu is not adjacent to its trigger: ' + JSON.stringify(g));
  assert(g.rightDelta <= 8, 'menu is not right-aligned with its trigger: ' + JSON.stringify(g));
}

async function compactCheck(page) {
  const mine = await height(page, ROW);
  const net = await height(page, NETWORK_ROW);
  assert(mine <= net + TOLERANCE, `saved row is ${mine}px, Network row ${net}px (tolerance ${TOLERANCE})`);
  assert(await page.$(`${ROW} [data-share-channel], ${ROW} [data-remove-channel], ${ROW} .ch-share-btn, ${ROW} .ch-remove-btn`) === null,
    'saved row must not render inline Share / Remove controls');
  const headers = await page.$$eval('#chList .ch-section-header', (els) => els.map((e) => e.textContent));
  assert(headers.some((t) => /My Channels/.test(t)) && headers.some((t) => /Network/.test(t)),
    'My Channels and Network stay separate sections: ' + JSON.stringify(headers));
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  const errors = [];
  console.log(`\n=== #354 compact My Channels E2E against ${BASE} ===`);

  // ── (a) compact rows, both themes ──
  for (const theme of ['light', 'dark']) {
    const { ctx, page } = await open(browser, errors, { viewport: { width: 1440, height: 900 } }, theme);
    await ready(page, false);
    await step(`(a) ${theme}: saved row is no taller than a Network row, no inline actions`, async () => {
      await compactCheck(page);
    });
    await step(`(a) ${theme}: trigger is a 48px target that does not enlarge the row`, async () => {
      const r = await page.$eval(TRIGGER, (el) => { const b = el.getBoundingClientRect(); return { w: b.width, h: b.height }; });
      assert(r.w >= MIN_TARGET && r.h >= MIN_TARGET, `trigger is ${r.w}x${r.h}`);
    });
    await ctx.close();
  }

  // ── (b) mouse and keyboard ──
  {
    const { ctx, page } = await open(browser, errors, { viewport: { width: 1440, height: 900 } });
    await ready(page, false);

    await step('(b) trigger exposes menu semantics and opening it changes nothing', async () => {
      const a = await page.$eval(TRIGGER, (el) => ({
        role: el.getAttribute('role'), popup: el.getAttribute('aria-haspopup'),
        expanded: el.getAttribute('aria-expanded'), label: el.getAttribute('aria-label'),
        tab: el.tabIndex }));
      assert(a.role === 'button' && a.popup === 'menu' && a.expanded === 'false' && a.tab === 0, JSON.stringify(a));
      assert(/Actions for/.test(a.label) && a.label.includes(NAME), 'label: ' + a.label);
      assert(!(await menuOpen(page)), 'menu starts closed');
      const keysBefore = await storedKeys(page);
      await page.click(TRIGGER);
      await page.waitForSelector(MENU + ':not([hidden])');
      assert(await page.getAttribute(TRIGGER, 'aria-expanded') === 'true', 'aria-expanded=true while open');
      assert(await page.getAttribute(MENU, 'role') === 'menu', 'menu role');
      await assertAnchored(page);
      const items = await page.$$eval(`${MENU} [role="menuitem"]`, (els) => els.map((e) => e.textContent.trim()));
      assert(items.length === 2 && /Share/.test(items[0]) && /Remove/.test(items[1]), JSON.stringify(items));
      assert(JSON.stringify(await storedKeys(page)) === JSON.stringify(keysBefore), 'opening the menu must not touch stored keys');
      assert(await page.$(`${ROW}.selected`) === null, 'opening the menu must not select the channel');
    });

    await step('(b) Escape closes the menu and focus returns to the trigger', async () => {
      await page.keyboard.press('Escape');
      assert(!(await menuOpen(page)), 'menu closed');
      assert(await page.getAttribute(TRIGGER, 'aria-expanded') === 'false', 'aria-expanded back to false');
      assert(await focusedIs(page, TRIGGER), 'focus returns to the trigger');
    });

    await step('(b) outside click closes the menu', async () => {
      await page.click(TRIGGER);
      await page.waitForSelector(MENU + ':not([hidden])');
      await page.click('.ch-sidebar-title');
      assert(!(await menuOpen(page)), 'menu closed by outside click');
    });

    await step('(b) keyboard: Enter opens, focus lands on Share, arrows move, Escape closes', async () => {
      await page.focus(TRIGGER);
      await page.keyboard.press('Enter');
      await page.waitForSelector(MENU + ':not([hidden])');
      assert(await focusedIs(page, SHARE_ITEM), 'first item (Share) is focused');
      await page.keyboard.press('ArrowDown');
      assert(await focusedIs(page, REMOVE_ITEM), 'ArrowDown reaches Remove');
      await page.keyboard.press('ArrowDown');
      assert(await focusedIs(page, SHARE_ITEM), 'ArrowDown wraps to Share');
      await page.keyboard.press('ArrowUp');
      assert(await focusedIs(page, REMOVE_ITEM), 'ArrowUp wraps to Remove');
      await page.keyboard.press('Escape');
      assert(!(await menuOpen(page)) && await focusedIs(page, TRIGGER), 'closed, focus on trigger');
    });

    await step('(b) keyboard: Space and ArrowDown also open; Tab closes', async () => {
      await page.focus(TRIGGER);
      await page.keyboard.press('Space');
      await page.waitForSelector(MENU + ':not([hidden])');
      await page.keyboard.press('Escape');
      await page.keyboard.press('ArrowDown');
      await page.waitForSelector(MENU + ':not([hidden])');
      await page.keyboard.press('Tab');
      assert(!(await menuOpen(page)), 'Tab closes the menu');
    });

    await step('(b) keyboard: Share via Enter opens the share dialog and the menu closes', async () => {
      await page.focus(TRIGGER);
      await page.keyboard.press('Enter');
      await page.waitForSelector(MENU + ':not([hidden])');
      await page.keyboard.press('Enter');
      await page.waitForSelector('#chShareModal:not(.hidden)');
      assert(!(await menuOpen(page)), 'menu closed after choosing Share');
      const key = await page.$eval('#chShareKey', (el) => el.value);
      assert(key.toLowerCase() === KEY, 'share dialog shows the channel key');
      await page.keyboard.press('Escape');
      await page.waitForSelector('#chShareModal', { state: 'hidden' });
    });

    await step('(b) mouse: Share via the menu opens the share dialog', async () => {
      await page.click(TRIGGER);
      await page.click(SHARE_ITEM);
      await page.waitForSelector('#chShareModal:not(.hidden)');
      await page.keyboard.press('Escape');
      await page.waitForSelector('#chShareModal', { state: 'hidden' });
    });

    await step('(b) Remove is deliberate: dismissing the confirmation keeps channel and key', async () => {
      const confirms = [];
      const onDialog = (d) => { confirms.push(d.message()); d.dismiss(); };
      page.once('dialog', onDialog);
      await page.focus(TRIGGER);
      await page.keyboard.press('Enter');
      await page.waitForSelector(MENU + ':not([hidden])');
      await page.keyboard.press('ArrowDown');
      await page.keyboard.press('Enter');
      await page.waitForFunction(() => document.getElementById('chRowMenu').hidden);
      assert(confirms.length === 1 && confirms[0].includes(NAME), 'a confirmation naming the channel was shown: ' + JSON.stringify(confirms));
      assert(await page.$(ROW) !== null, 'row still present after dismiss');
      assert((await storedKeys(page)).includes(NAME), 'key still stored after dismiss');
    });

    await step('(b) mouse: accepting the confirmation removes the channel and its key', async () => {
      page.once('dialog', (d) => d.accept());
      await page.click(TRIGGER);
      await page.click(REMOVE_ITEM);
      await page.waitForFunction((h) => !document.querySelector('#chList [data-hash="' + h + '"]'), HASH);
      assert(!(await storedKeys(page)).includes(NAME), 'key removed');
      assert(!(await menuOpen(page)), 'menu closed');
    });

    await step('(b) keyboard: accepting the confirmation removes a channel', async () => {
      await page.evaluate(([n, k]) => localStorage.setItem('corescope_channel_keys', JSON.stringify({ [n]: k })), [NAME, KEY]);
      await page.reload({ waitUntil: 'domcontentloaded' });
      await ready(page, false);
      page.once('dialog', (d) => d.accept());
      await page.focus(TRIGGER);
      await page.keyboard.press('Enter');
      await page.waitForSelector(MENU + ':not([hidden])');
      await page.keyboard.press('ArrowDown');
      await page.keyboard.press('Enter');
      await page.waitForFunction((h) => !document.querySelector('#chList [data-hash="' + h + '"]'), HASH);
      assert(!(await storedKeys(page)).includes(NAME), 'key removed');
    });
    await ctx.close();
  }

  // ── (c) narrow / mobile ──
  {
    const { ctx, page } = await open(browser, errors,
      { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });
    await ready(page, true);
    await step('(c) 390px: flat list stays compact, no actions, no horizontal overflow', async () => {
      const rows = await page.$$eval('#chList .ch-row', (els) => els.map((e) => e.getBoundingClientRect().height));
      assert(rows.length >= 2, 'expected the saved channel and at least one other row');
      const mine = await height(page, `#chList .ch-row[data-hash="${HASH}"]`);
      assert(mine <= Math.min(...rows) + TOLERANCE, `saved row ${mine}px vs smallest row ${Math.min(...rows)}px`);
      assert(await page.$('#chList [data-row-menu], #chList [data-share-channel], #chList [data-remove-channel]') === null,
        'the mobile list renders no row actions (#1367)');
      const over = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
      assert(over <= 0, 'horizontal overflow ' + over + 'px');
    });
    await ctx.close();
  }
  {
    const { ctx, page } = await open(browser, errors,
      { viewport: { width: 768, height: 900 }, hasTouch: true });
    await ready(page, false);
    await step('(c) 768px touch: compact row, 48px trigger, menu inside viewport, Share works', async () => {
      await compactCheck(page);
      const t = await page.$eval(TRIGGER, (el) => { const b = el.getBoundingClientRect(); return { w: b.width, h: b.height }; });
      assert(t.w >= MIN_TARGET && t.h >= MIN_TARGET, `trigger ${t.w}x${t.h}`);
      await page.tap(TRIGGER);
      await page.waitForSelector(MENU + ':not([hidden])');
      await assertAnchored(page);
      const m = await page.$eval(MENU, (el) => { const b = el.getBoundingClientRect(); return { l: b.left, r: b.right, t: b.top, b: b.bottom, vw: innerWidth, vh: innerHeight }; });
      assert(m.l >= 0 && m.r <= m.vw && m.t >= 0 && m.b <= m.vh, 'menu outside viewport: ' + JSON.stringify(m));
      const items = await page.$$eval(`${MENU} [role="menuitem"]`, (els) => els.map((e) => { const b = e.getBoundingClientRect(); return [b.width, b.height]; }));
      assert(items.every(([w, h]) => w >= MIN_TARGET && h >= MIN_TARGET), 'menu items are >=48px: ' + JSON.stringify(items));
      await page.tap(SHARE_ITEM);
      await page.waitForSelector('#chShareModal:not(.hidden)');
    });
    await ctx.close();
  }

  await step('no page or console errors', async () => {
    assert(errors.length === 0, errors.join(' | '));
  });
  await browser.close();
  console.log(`\n=== #354 E2E: ${passed} passed, ${failed} failed ===\n`);
  process.exit(failed === 0 ? 0 : 1);
})().catch((e) => { console.error(e); process.exit(1); });
