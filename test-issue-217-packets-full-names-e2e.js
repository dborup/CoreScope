/**
 * E2E (#217, port of upstream `Kpa-clawbot/CoreScope` PR 2090, adapted):
 * the packets "Full Names" toggle and the full-path popover.
 *
 *   A. Full Names: toggle, saved preference, URL override that does not
 *      overwrite the saved preference, hop chips lose their 120px cap.
 *   B. The "+N" pill is visible inside its path cell (it used to sit past the
 *      clipped edge, so nobody could click it).
 *   C. Popover: hover and focus open it, leaving closes it, click / Enter /
 *      Space pin it without selecting the row, Escape and outside click
 *      close it, a pinned popover goes when its row scrolls away or the page
 *      changes, and it lists every hop on its own numbered line.
 *   D. No document listener outlives the packets page (#419/#437).
 *   E. Desktop and mobile, light and dark theme.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-issue-217-packets-full-names-e2e.js
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

// Tracks the document listeners that are live, by the source text of the
// handler, so the path popover's can be told from other pages' (filter-ux.js
// and region-filter.js leak a click listener per visit; that is not this
// test's concern). Wrapped before any page script runs.
const TRACK_LISTENERS = () => {
  const live = new Set();
  const add = document.addEventListener.bind(document);
  const rem = document.removeEventListener.bind(document);
  const cap = (o) => (o === true || (o && o.capture) ? 'c' : 'b');
  document.addEventListener = function (t, fn, o) {
    if (fn) live.add({ t, fn, c: cap(o) });
    return add(t, fn, o);
  };
  document.removeEventListener = function (t, fn, o) {
    for (const e of live) if (e.t === t && e.fn === fn && e.c === cap(o)) live.delete(e);
    return rem(t, fn, o);
  };
  window.__livePopoverListeners = () => [...live].filter(e => /path-overflow-pill|pillOf/.test(String(e.fn))).length;
};

// Not timeWindow=0 (that falls back to the 15 minute default): the newest rows
// of a 24 h window (3 h at <=1024px, where longer windows are disabled) are the
// same ones for the whole CI job, so the result does not depend on fixture age
// (see test-issue-1122-details-row-clamp-e2e.js, #244).
let PKTS = '#/packets?timeWindow=1440';
async function openPackets(page, hash) {
  await page.goto(BASE + '/' + (hash || PKTS), { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#pktBody tr[data-action]');
  // Rows are drawn, observers resolved and overflow pills measured.
  await page.waitForFunction(() => document.querySelectorAll('.path-overflow-pill').length > 0, null, { timeout: 8000 });
}
const savedPref = (page) => page.evaluate(() => localStorage.getItem('meshcore-full-names'));
const hasFull = (page) => page.evaluate(() => document.getElementById('pktTable').classList.contains('pkt-full-names'));
const popover = (page) => page.locator('.path-popover');

async function longObserver(page) {
  const names = await page.evaluate(async () => (await (await fetch('/api/observers')).json()).observers.map(o => o.name));
  return names.filter(n => n && n.length > 18).sort((a, b) => b.length - a.length)[0];
}
const observerTexts = (page) => page.$$eval('#pktBody td.col-observer', tds => tds.map(t => t.textContent));
const firstPill = (page) => page.locator('#pktBody .path-overflow-pill').first();
async function pillInfo(page) {
  return page.$$eval('#pktBody .path-overflow-pill', ps => ps.map(p => {
    const r = p.getBoundingClientRect(), h = p.closest('.path-hops').getBoundingClientRect();
    return { text: p.textContent, inside: r.width > 0 && r.left >= h.left - 1 && r.right <= h.right + 1 };
  }));
}

(async () => {
  const browser = await chromium.launch({
    headless: true, executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #217 packets Full Names + path popover E2E against ${BASE} ===`);
  process.on('unhandledRejection', (e) => { console.error('  ✗ unhandled: ' + e.message); process.exit(1); });

  for (const theme of ['light', 'dark']) {
    const ctx = await browser.newContext({ viewport: { width: 1400, height: 900 } });
    await ctx.addInitScript(TRACK_LISTENERS);
    await ctx.addInitScript((t) => { try { localStorage.setItem('meshcore-theme', t); } catch (_) {} }, theme);
    const page = await ctx.newPage();
    page.setDefaultTimeout(8000);
    page.on('pageerror', (e) => console.error('[pageerror]', e.message));
    console.log(`\n-- desktop 1400px, ${theme} theme --`);

    await openPackets(page);
    const longName = await longObserver(page);
    assert(longName, 'fixture has an observer name longer than 18 chars');

    await step('A1 toggle exists and is off by default', async () => {
      assert(await page.locator('#fullNamesToggle').count() === 1, 'no #fullNamesToggle');
      assert(!(await hasFull(page)) && await savedPref(page) === null, 'on by default');
    });
    await step('A2 off: a long observer name is cut with an ellipsis', async () => {
      const cut = (await observerTexts(page)).filter(t => t.includes('…'));
      assert(cut.length > 0, 'no truncated observer cell in the fixture');
    });
    await step('A3 click: whole names, saved, and fullNames=1 in the URL', async () => {
      await page.click('#fullNamesToggle');
      assert(await hasFull(page), 'table lacks pkt-full-names');
      assert(await savedPref(page) === 'true', 'preference not saved');
      assert(/fullNames=1/.test(await page.evaluate(() => location.hash)), 'URL lacks fullNames=1');
      await page.waitForFunction(() => ![...document.querySelectorAll('#pktBody td.col-observer')].some(t => t.textContent.includes('…')));
      assert((await observerTexts(page)).every(t => !t.includes('…')), 'a cell still ends in an ellipsis');
    });
    await step('A4 hop chips lose the 120px cap under Full Names', async () => {
      const widths = await page.$$eval('#pktBody .path-hops .hop-named', cs => cs.map(c => c.offsetWidth));
      assert(widths.length > 0 && Math.max(...widths) > 121, 'widest chip ' + Math.max(...widths));
    });
    await step('A5 reload keeps Full Names from the saved preference', async () => {
      await openPackets(page, PKTS);
      assert(await hasFull(page), 'not restored');
    });
    await step('A6 click again: back to cut names, preference false, no parameter', async () => {
      await page.click('#fullNamesToggle');
      assert(!(await hasFull(page)) && await savedPref(page) === 'false', 'did not switch off');
      assert(!/fullNames/.test(await page.evaluate(() => location.hash)), 'URL kept fullNames');
    });
    await step('A7 ?fullNames=1 applies without overwriting the saved preference', async () => {
      await page.evaluate(() => localStorage.removeItem('meshcore-full-names'));
      await openPackets(page, PKTS + '&fullNames=1');
      assert(await hasFull(page), 'link did not turn Full Names on');
      assert(await savedPref(page) === null, 'link overwrote the visitor preference: ' + await savedPref(page));
    });
    await step('A8 ?fullNames=0 overrides a saved "on" and survives a reload', async () => {
      await page.evaluate(() => localStorage.setItem('meshcore-full-names', 'true'));
      await openPackets(page, PKTS + '&fullNames=0');
      assert(!(await hasFull(page)), 'link did not turn Full Names off');
      assert(await savedPref(page) === 'true', 'link overwrote the visitor preference');
      assert(/fullNames=0/.test(await page.evaluate(() => location.hash)), 'override dropped from the URL');
      await page.reload({ waitUntil: 'domcontentloaded' });
      await page.waitForSelector('#pktBody tr[data-action]');
      assert(!(await hasFull(page)), 'reload lost the override');
      await page.evaluate(() => localStorage.removeItem('meshcore-full-names'));
    });

    await openPackets(page);
    await step('B1 every +N pill sits inside its path cell', async () => {
      const info = await pillInfo(page);
      assert(info.length > 0, 'no pills');
      const out = info.filter(i => !i.inside);
      assert(out.length === 0, out.length + ' of ' + info.length + ' pills lie outside their cell');
    });

    await step('C1 hover opens a vertical, numbered popover that counts the hidden hops', async () => {
      const pill = firstPill(page);
      const hidden = Number((await pill.textContent()).replace('+', ''));
      await pill.hover();
      await popover(page).waitFor({ state: 'visible' });
      const items = await popover(page).locator('li').count();
      assert(items > hidden, 'lists ' + items + ' hops, expected more than the ' + hidden + ' hidden');
      assert(new RegExp('Full path[^0-9]*' + items + ' hops').test(await popover(page).locator('.path-popover-title').textContent()), 'title does not count ' + items);
      const tops = await popover(page).locator('li').evaluateAll(ls => ls.map(l => Math.round(l.getBoundingClientRect().top)));
      assert(new Set(tops).size === tops.length, 'hops share a line');
      assert(await popover(page).locator('li').first().locator('.path-popover-idx').textContent() === '1', 'first hop not numbered 1');
      const bg = await popover(page).evaluate(e => getComputedStyle(e).backgroundColor);
      assert(!/rgba\(0, 0, 0, 0\)|transparent/.test(bg), 'popover background is ' + bg);
    });
    await step('C2 moving into the popover keeps it, leaving both closes it', async () => {
      const box = await popover(page).boundingBox();
      await page.mouse.move(box.x + box.width / 2, box.y + 10);
      await page.waitForTimeout(400);
      assert(await popover(page).count() === 1, 'closed while the pointer was inside');
      await page.mouse.move(5, 5);
      await popover(page).waitFor({ state: 'detached' });
    });
    await step('C3 focus opens it, Escape closes it', async () => {
      await firstPill(page).focus();
      await popover(page).waitFor({ state: 'visible' });
      await page.keyboard.press('Escape');
      await popover(page).waitFor({ state: 'detached' });
    });
    await step('C4 Enter pins it without selecting the row; pointer leaving keeps it', async () => {
      await firstPill(page).focus();
      await page.keyboard.press('Enter');
      await page.mouse.move(5, 5);
      await page.waitForTimeout(400);
      assert(await popover(page).count() === 1, 'unpinned');
      assert(await page.locator('#pktBody tr.selected').count() === 0, 'Enter selected the row');
      assert(!/#\/packets\/[0-9a-f]{6}/i.test(await page.evaluate(() => location.hash)), 'Enter opened a packet');
    });
    await step('C5 Escape closes a pinned popover', async () => {
      await page.keyboard.press('Escape');
      await popover(page).waitFor({ state: 'detached' });
    });
    await step('C6 Space pins; an outside click closes it', async () => {
      await firstPill(page).focus();
      await page.keyboard.press(' ');
      await page.mouse.move(5, 5);
      await page.waitForTimeout(400);
      assert(await popover(page).count() === 1, 'Space did not pin');
      await page.mouse.click(5, 5);
      await popover(page).waitFor({ state: 'detached' });
    });
    await step('C7 click pins without selecting the row; a second click closes', async () => {
      await firstPill(page).click();
      await page.mouse.move(5, 5);
      await page.waitForTimeout(400);
      assert(await popover(page).count() === 1, 'click did not pin');
      assert(await page.locator('#pktBody tr.selected').count() === 0, 'click selected the row');
      await firstPill(page).click();
      await popover(page).waitFor({ state: 'detached' });
    });
    await step('C8 a pinned popover closes when its row is re-rendered away', async () => {
      await page.waitForTimeout(1300); // let the delayed overflow pass settle
      await firstPill(page).click();
      await popover(page).waitFor({ state: 'visible' });
      // A scripted click: a real one would also dismiss it with its mousedown.
      await page.evaluate(() => document.querySelector('#pktTable th.col-size').click());
      await popover(page).waitFor({ state: 'detached' });
    });
    await step('C9 a pinned popover closes when the page changes', async () => {
      await openPackets(page);
      await page.waitForTimeout(1300);
      await firstPill(page).click();
      await popover(page).waitFor({ state: 'visible' });
      await page.evaluate(() => { location.hash = '#/nodes'; });
      await popover(page).waitFor({ state: 'detached' });
    });
    await step('C10 popover chips are not capped at 120px', async () => {
      await openPackets(page, PKTS + '&fullNames=1');
      await firstPill(page).hover();
      await popover(page).waitFor({ state: 'visible' });
      const w = await popover(page).locator('.hop-named').evaluateAll(cs => cs.map(c => c.scrollWidth - c.offsetWidth));
      assert(w.every(d => d <= 1), 'a popover chip is clipped by ' + Math.max(...w) + 'px');
    });

    await step('D1 the popover leaves no document listener behind', async () => {
      const live = () => page.evaluate(() => window.__livePopoverListeners());
      for (let i = 0; i < 3; i++) {
        await openPackets(page);
        assert(await live() === 0, 'visit ' + (i + 1) + ': ' + await live() + ' popover listeners before any popover is open');
        await firstPill(page).click(); // pinned: it is open when the page is left
        await popover(page).waitFor({ state: 'visible' });
        assert(await live() > 0, 'no outside-press listener while a popover is open (test would pass vacuously)');
        await page.evaluate(() => { location.hash = '#/nodes'; });
        await page.waitForTimeout(600);
        assert(await live() === 0, 'visit ' + (i + 1) + ': ' + await live() + ' popover listeners after leaving packets');
      }
    });
    await step('D2 Escape closes the popover first and leaves the open detail panel alone', async () => {
      await openPackets(page);
      await page.waitForTimeout(1300);
      await page.locator('#pktBody tr[data-action] td.col-type').first().click();
      await page.waitForSelector('#pktRight:not(.empty)', { timeout: 5000 }).catch(() => {});
      const detailOpen = () => page.evaluate(() => { const r = document.getElementById('pktRight'); return !!r && !r.classList.contains('empty') && r.offsetWidth > 0; });
      assert(await detailOpen(), 'detail panel did not open');
      await firstPill(page).click();
      await popover(page).waitFor({ state: 'visible' });
      await page.keyboard.press('Escape');
      await popover(page).waitFor({ state: 'detached' });
      assert(await detailOpen(), 'Escape closed the detail panel along with the popover');
      await page.keyboard.press('Escape');
      await page.waitForFunction(() => { const r = document.getElementById('pktRight'); return !r || r.classList.contains('empty') || r.offsetWidth === 0; });
    });

    await ctx.close();
  }

  for (const theme of ['light', 'dark']) {
    const ctx = await browser.newContext({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });
    await ctx.addInitScript((t) => { try { localStorage.setItem('meshcore-theme', t); } catch (_) {} }, theme);
    const page = await ctx.newPage();
    page.setDefaultTimeout(8000);
    page.on('pageerror', (e) => console.error('[pageerror]', e.message));
    PKTS = '#/packets?timeWindow=180';
    console.log(`\n-- mobile 390px, ${theme} theme --`);
    await page.goto(BASE + '/' + PKTS, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('#pktBody tr[data-action]');
    await step('E1 the Full Names toggle is reachable and works', async () => {
      const btn = page.locator('#fullNamesToggle');
      // The filter groups sit behind the "Filters" button on a phone.
      if (!(await btn.isVisible())) await page.locator('.filter-toggle-btn-mirror').tap();
      await btn.scrollIntoViewIfNeeded();
      const box = await btn.boundingBox();
      assert(box && box.width > 0 && box.x >= 0 && box.x + box.width <= 390, 'toggle outside the viewport: ' + JSON.stringify(box));
      await btn.tap();
      assert(await hasFull(page) && await savedPref(page) === 'true', 'tap did not switch Full Names on');
      await btn.tap();
      assert(!(await hasFull(page)), 'second tap did not switch it off');
    });
    await step('E2 the page does not scroll sideways', async () => {
      const over = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
      assert(over <= 1, 'page overflows by ' + over + 'px');
    });
    await step('E3 a visible pill opens the popover on tap and closes on an outside tap', async () => {
      const pills = await pillInfo(page);
      if (!pills.length) { console.log('    (no path pill on mobile: the Path column is hidden here)'); return; }
      await firstPill(page).tap();
      await popover(page).waitFor({ state: 'visible' });
      const box = await popover(page).boundingBox();
      assert(box.x >= 0 && box.x + box.width <= 391, 'popover leaves the viewport: ' + JSON.stringify(box));
      await page.touchscreen.tap(5, 5);
      await popover(page).waitFor({ state: 'detached' });
    });
    await ctx.close();
  }

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
