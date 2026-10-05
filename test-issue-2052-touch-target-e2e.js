#!/usr/bin/env node
/* Issue #2052 — rendered touch targets for the channel row actions.
 *
 * test-issue-2052-touch-target-css.js checks the stylesheet text; this test
 * measures what the browser actually renders, in the real responsive DOM:
 *
 *  - Desktop 1440x900 (sectioned .ch-item list): the share and remove actions
 *    of a user-added channel are at least 48x48 CSS px, visible, not clipped
 *    by any overflow container, hit at their centre, not overlapping each
 *    other or the row's other parts, reachable by keyboard with focus-visible,
 *    keep their ARIA/title contract, and do their job without side effects
 *    (share opens the modal, remove asks for confirmation, which is dismissed).
 *  - Mobile 390x844 touch (flat .ch-row list, #1367): the list renders no
 *    share/remove actions at all, every visible tap target in the channel
 *    sidebar is at least 48x48 (the coarse-pointer .region-pill included,
 *    #235), and tapping a row opens the channel. In the open channel the
 *    header's back button and every sender avatar, and in the add-channel
 *    dialog the close button, are at least 48x48 with no horizontal
 *    overflow (#235).
 *  - 768-1330 px (the narrow sectioned sidebar; 768 with touch): share and
 *    remove are at least 48x48, visible, unclipped, hit at their centre and
 *    do not overlap. Before upstream PR 2078 the remove action was clipped
 *    here; user-added rows now let their controls wrap.
 *  - The add-channel dialog's primary buttons (#249): at 320, 390, 640 and
 *    768 px with touch each is at least 48x48, unclipped, hit at its centre
 *    with a centred label, and inside a dialog that does not scroll
 *    sideways. At 1440 px without touch they keep their 32 px height: the
 *    change is touch-only, like the #630 coarse-pointer block.
 *
 * 48 is the house preference for these controls (upstream PR 2078 settled
 * #2052 on 48; WCAG 2.5.5 asks for 44).
 *
 * The user-added channel comes from a saved key in localStorage, the same
 * state a returning user has; no DOM is injected. No sleeps or retries: each
 * step waits for the element it needs.
 *
 * Defaults to localhost:13581 — NEVER point at prod (AGENTS.md). CI sets BASE_URL.
 * Run: BASE_URL=http://localhost:13581 node test-issue-2052-touch-target-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const ORIGIN = new URL(BASE).origin;
const CHANNEL = '#tt2052probe';
const HASH = 'user:' + CHANNEL;
const SHARE = `#chList [data-share-channel="${HASH}"]`;
const REMOVE = `#chList [data-remove-channel="${HASH}"]`;
const MIN = 48;
// A server channel the E2E fixture has messages in, so sender avatars render.
const MSG_CHANNEL = '#test';

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

async function openChannels(browser, errors, options) {
  const ctx = await browser.newContext(options);
  const page = await ctx.newPage();
  page.setDefaultTimeout(15000);
  page.on('pageerror', (e) => errors.push('pageerror: ' + e.message));
  page.on('console', (m) => {
    if (m.type() !== 'error') return;
    // Map tiles and other CDN resources are outside this app.
    const url = (m.location() && m.location().url) || '';
    if (url && !url.startsWith(ORIGIN)) return;
    errors.push('console.error: ' + m.text());
  });
  await page.addInitScript(([name]) => {
    localStorage.setItem('corescope_channel_keys', JSON.stringify({ [name]: '00112233445566778899aabbccddeeff' }));
    window.addEventListener('unhandledrejection', (e) => {
      console.error('unhandledrejection: ' + ((e.reason && e.reason.message) || e.reason));
    });
  }, [CHANNEL]);
  await page.goto(BASE + '/#/channels', { waitUntil: 'domcontentloaded' });
  return { ctx, page };
}

// Keyboard focus lands on the control, matches :focus-visible, and the
// existing focus rule (.ch-icon-btn:focus { opacity: 1 }) takes effect. The
// opacity has a 0.15s transition, so wait for its end state, not a time.
// This checks the focus state and the existing opacity cue only; the same
// rule also sets outline: none (unchanged here, as on master), so no focus
// ring is asserted.
async function assertFocused(page, selector, name) {
  const st = await page.evaluate((sel) => {
    const e = document.querySelector(sel);
    return { focused: document.activeElement === e, visible: e.matches(':focus-visible') };
  }, selector);
  assert(st.focused, name + ' is not focused');
  assert(st.visible, name + ' focus does not match :focus-visible');
  await page.waitForFunction((sel) => getComputedStyle(document.querySelector(sel)).opacity === '1', selector)
    .catch((e) => { throw new Error(name + ' never reached the focused opacity of 1: ' + e.message); });
}

// Geometry of one control, measured in the page.
function measure(page, selector) {
  return page.evaluate((sel) => {
    const el = document.querySelector(sel);
    if (!el) return null;
    const r = el.getBoundingClientRect();
    const cs = getComputedStyle(el);
    // How far the box sticks out of the viewport and of every clipping ancestor.
    let clippedBy = [];
    const outside = (box, name) => {
      const over = Math.max(box.left - r.left, r.right - box.right, box.top - r.top, r.bottom - box.bottom);
      if (over > 0.5) clippedBy.push(name + ' by ' + over.toFixed(1) + 'px');
    };
    outside({ left: 0, top: 0, right: innerWidth, bottom: innerHeight }, 'viewport');
    for (let a = el.parentElement; a; a = a.parentElement) {
      const c = getComputedStyle(a);
      if (c.overflowX !== 'visible' || c.overflowY !== 'visible') outside(a.getBoundingClientRect(), a.id || String(a.className).split(' ')[0] || a.tagName);
    }
    const hitEl = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2);
    return {
      w: r.width, h: r.height, left: r.left, right: r.right, top: r.top, bottom: r.bottom,
      visible: cs.visibility === 'visible' && cs.display !== 'none' && Number(cs.opacity) > 0 && r.width > 0 && r.height > 0,
      clippedBy,
      hit: !!hitEl && (hitEl === el || el.contains(hitEl)),
      hitOn: hitEl ? (hitEl.id || String(hitEl.className) || hitEl.tagName) : 'nothing',
    };
  }, selector);
}

function overlaps(a, b) {
  return !(a.right <= b.left || b.right <= a.left || a.bottom <= b.top || b.bottom <= a.top);
}

// One control: at least MIN square, visible, unclipped and hit at its centre.
async function assertTarget(page, selector, name) {
  const m = await measure(page, selector);
  assert(m, name + ' not rendered');
  assert(m.w >= MIN && m.h >= MIN, `${name} renders ${m.w.toFixed(1)}x${m.h.toFixed(1)}, below ${MIN}x${MIN}`);
  assert(m.visible, name + ' is not visible');
  assert(m.clippedBy.length === 0, name + ' is clipped by ' + m.clippedBy.join(', '));
  assert(m.hit, `${name} centre hits "${m.hitOn}", not the control`);
  return m;
}

// Share and remove: each a full target, and not on top of each other.
async function assertActionTargets(page) {
  const share = await assertTarget(page, SHARE, 'share');
  const remove = await assertTarget(page, REMOVE, 'remove');
  assert(!overlaps(share, remove), 'share and remove overlap');
}

// #249: the add-channel dialog's primary buttons.
const DIALOG_BUTTONS = ['#chGenerateBtn', '#chPskAddBtn', '#chHashtagBtn'];

async function openAddChannelDialog(page) {
  await page.click('#chAddChannelBtn');
  await page.waitForSelector('#chModalClose', { state: 'visible' });
}

// Where a button sits in the dialog, with the button scrolled into view: its
// label's offset from the centre, and whether it is inside the dialog.
function dialogPlacement(page, selector) {
  return page.evaluate((sel) => {
    const el = document.querySelector(sel);
    el.scrollIntoView({ block: 'center' });
    const b = el.getBoundingClientRect();
    const m = el.closest('.ch-modal').getBoundingClientRect();
    const range = document.createRange();
    range.selectNodeContents(el);
    const t = range.getBoundingClientRect();
    return {
      h: b.height,
      dx: (t.left + t.width / 2) - (b.left + b.width / 2),
      dy: (t.top + t.height / 2) - (b.top + b.height / 2),
      inside: b.left >= m.left && b.right <= m.right,
    };
  }, selector);
}

function horizontalOverflow(page) {
  return page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
}

async function main() {
  let browser;
  try {
    browser = await chromium.launch({
      headless: true,
      executablePath: process.env.CHROMIUM_PATH || undefined,
      args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
    });
  } catch (err) {
    if (process.env.CHROMIUM_REQUIRE === '1') {
      console.error('test-issue-2052-touch-target-e2e.js: FAIL — Chromium required but unavailable: ' + err.message);
      process.exit(1);
    }
    console.log('test-issue-2052-touch-target-e2e.js: SKIP (Chromium unavailable: ' + err.message.split('\n')[0] + ')');
    process.exit(0);
  }
  const errors = [];
  console.log(`\n=== #2052 channel action touch targets E2E against ${BASE} ===`);

  // ── Desktop: the sectioned list with inline actions ──────────────────────
  const desk = await openChannels(browser, errors, { viewport: { width: 1440, height: 900 } });
  const dp = desk.page;
  try {
    await dp.waitForSelector(REMOVE, { state: 'visible' });

    await step(`desktop 1440x900: share and remove are at least ${MIN}x${MIN}, visible, unclipped and hit at their centre`, async () => {
      await assertActionTargets(dp);
    });

    await step('desktop: actions do not overlap each other or the rest of the row, and the page has no horizontal overflow', async () => {
      const share = await measure(dp, SHARE), remove = await measure(dp, REMOVE);
      assert(!overlaps(share, remove), 'share and remove overlap');
      const parts = await dp.evaluate((sel) => {
        const row = document.querySelector(sel).closest('.ch-item');
        return [...row.querySelectorAll('.ch-badge, .ch-item-name, .ch-user-badge, .ch-unread-badge, .ch-color-dot, .ch-color-clear, .ch-item-time, .ch-item-preview')]
          .map((e) => { const r = e.getBoundingClientRect(); return { name: String(e.className).split(' ')[0], left: r.left, right: r.right, top: r.top, bottom: r.bottom }; });
      }, SHARE);
      assert(parts.length >= 4, 'row parts not found');
      for (const p of parts) {
        assert(!overlaps(share, p), 'share overlaps ' + p.name);
        assert(!overlaps(remove, p), 'remove overlaps ' + p.name);
      }
      const overflow = await dp.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
      assert(overflow <= 0, 'page overflows horizontally by ' + overflow + 'px');
    });

    await step('desktop: ARIA and title contract is unchanged', async () => {
      const a = await dp.evaluate(([s, r]) => [s, r].map((sel) => {
        const e = document.querySelector(sel);
        return { role: e.getAttribute('role'), tabindex: e.getAttribute('tabindex'), label: e.getAttribute('aria-label'), title: e.getAttribute('title'), popup: e.getAttribute('aria-haspopup') };
      }), [SHARE, REMOVE]);
      assert(a[0].role === 'button' && a[0].tabindex === '0' && a[0].label === 'Share ' + CHANNEL && a[0].title === 'Share channel key (QR + URL)' && a[0].popup === 'dialog',
        'share ARIA/title changed: ' + JSON.stringify(a[0]));
      assert(a[1].role === 'button' && a[1].tabindex === '0' && a[1].label === 'Remove ' + CHANNEL && a[1].title === 'Remove channel and clear saved key',
        'remove ARIA/title changed: ' + JSON.stringify(a[1]));
    });

    await step('desktop: Tab reaches share then remove with focus-visible, and Enter runs each action without side effects', async () => {
      await dp.focus(`#chList .ch-item[data-hash="${HASH}"]`);
      await dp.keyboard.press('Tab');
      await assertFocused(dp, SHARE, 'share (Tab from the row)');
      await dp.keyboard.press('Enter');
      await dp.waitForSelector('#chShareModal:not(.hidden)');
      await dp.keyboard.press('Escape');
      await dp.waitForFunction(() => document.getElementById('chShareModal').classList.contains('hidden'));

      await dp.focus(SHARE); // the closed modal does not have to hand focus back
      await dp.keyboard.press('Tab');
      await assertFocused(dp, REMOVE, 'remove (Tab from share)');
      // waitForEvent has a timeout, so a missing confirmation fails the step
      // instead of hanging the run.
      const dialog = dp.waitForEvent('dialog', { timeout: 10000 })
        .then(async (d) => { const text = d.message(); await d.dismiss(); return text; });
      await dp.keyboard.press('Enter');
      const text = await dialog;
      assert(/Remove channel/.test(text), 'remove did not ask for confirmation: ' + text);
      const kept = await dp.evaluate(([sel, name]) => !!document.querySelector(sel) &&
        !!JSON.parse(localStorage.getItem('corescope_channel_keys') || '{}')[name], [REMOVE, CHANNEL]);
      assert(kept, 'dismissing the confirmation still removed the channel');
    });
  } finally {
    await desk.ctx.close();
  }

  // ── Mobile: the flat chat-app list, no inline actions ────────────────────
  const mob = await openChannels(browser, errors, { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });
  const mp = mob.page;
  try {
    // Wait for the channel's row, whatever layout rendered it; the step below
    // asserts which layout it is.
    await mp.waitForSelector(`#chList [data-hash="${HASH}"]`, { state: 'visible' });

    await step(`mobile 390x844: the list renders no share/remove actions, and every visible tap target is at least ${MIN}x${MIN}`, async () => {
      const r = await mp.evaluate(([hash, min]) => ({
        mobileRow: !!document.querySelector(`#chList .ch-row[data-hash="${hash}"]`),
        actions: document.querySelectorAll('#chList .ch-icon-btn, #chList [data-share-channel], #chList [data-remove-channel]').length,
        desktopRows: document.querySelectorAll('#chList .ch-item').length,
        small: [...document.querySelectorAll('.ch-sidebar button, .ch-sidebar a[href], .ch-sidebar [role="button"], .ch-sidebar [tabindex="0"]')]
          .filter((e) => { const b = e.getBoundingClientRect(); return b.width > 0 && b.height > 0 && getComputedStyle(e).visibility === 'visible'; })
          .map((e) => { const b = e.getBoundingClientRect(); return { name: e.id || String(e.className).split(' ')[0] || e.tagName, w: +b.width.toFixed(1), h: +b.height.toFixed(1) }; })
          .filter((t) => t.w < min || t.h < min),
        overflow: document.documentElement.scrollWidth - document.documentElement.clientWidth,
      }), [HASH, MIN]);
      assert(r.mobileRow, 'the channel is not rendered as a mobile .ch-row');
      assert(r.actions === 0 && r.desktopRows === 0, `mobile list renders desktop actions (actions=${r.actions}, .ch-item rows=${r.desktopRows})`);
      assert(r.small.length === 0, `mobile tap targets below ${MIN}x${MIN}: ` + JSON.stringify(r.small));
      assert(r.overflow <= 0, 'mobile page overflows horizontally by ' + r.overflow + 'px');
    });

    await step('mobile: tapping the channel row opens the channel', async () => {
      await mp.tap(`#chList [data-hash="${HASH}"]`);
      await mp.waitForFunction((h) => location.hash.includes('/channels/') &&
        (location.hash.includes(encodeURIComponent(h)) || location.hash.includes(h)), HASH);
    });

    await step(`mobile: the open channel's back button is at least ${MIN}x${MIN}, unclipped and hit at its centre (#235)`, async () => {
      await mp.waitForSelector('#chHeader .ch-back', { state: 'visible' });
      await assertTarget(mp, '#chHeader .ch-back', 'back button');
      const overflow = await horizontalOverflow(mp);
      assert(overflow <= 0, 'channel view overflows horizontally by ' + overflow + 'px');
    });

    await step(`mobile: in a channel with messages every sender avatar is at least ${MIN}x${MIN}, and nothing overflows horizontally (#235)`, async () => {
      await mp.tap('#chHeader .ch-back');
      await mp.tap(`#chList [data-hash="${MSG_CHANNEL}"]`);
      await mp.waitForSelector('#chMessages .ch-avatar.ch-tappable', { state: 'visible' });
      const r = await mp.evaluate((min) => {
        const msgs = document.getElementById('chMessages');
        const avatars = [...msgs.querySelectorAll('.ch-avatar.ch-tappable')].map((e) => e.getBoundingClientRect());
        return {
          count: avatars.length,
          small: avatars.filter((b) => b.width < min || b.height < min).map((b) => b.width.toFixed(1) + 'x' + b.height.toFixed(1)),
          msgsOverflow: msgs.scrollWidth - msgs.clientWidth,
        };
      }, MIN);
      assert(r.count > 0, 'no sender avatars rendered in ' + MSG_CHANNEL);
      assert(r.small.length === 0, `${r.small.length} of ${r.count} avatars below ${MIN}x${MIN}: ` + r.small.slice(0, 5).join(', '));
      assert(r.msgsOverflow <= 0, 'message list overflows horizontally by ' + r.msgsOverflow + 'px');
      await assertTarget(mp, '#chHeader .ch-back', 'back button');
      const overflow = await horizontalOverflow(mp);
      assert(overflow <= 0, 'channel view overflows horizontally by ' + overflow + 'px');
    });

    await step(`mobile: the add-channel dialog's close button is at least ${MIN}x${MIN} and hit at its centre, and the page does not overflow (#235)`, async () => {
      await mp.tap('#chHeader .ch-back');
      await mp.tap('#chAddChannelBtn');
      await mp.waitForSelector('#chModalClose', { state: 'visible' });
      await assertTarget(mp, '#chModalClose', 'dialog close');
      const overflow = await horizontalOverflow(mp);
      assert(overflow <= 0, 'page with the dialog open overflows horizontally by ' + overflow + 'px');
      await mp.tap('#chModalClose');
      await mp.waitForFunction(() => document.getElementById('chAddChannelModal').classList.contains('hidden'));
    });
  } finally {
    await mob.ctx.close();
  }

  // ── Narrow sectioned sidebar: 768 (touch) to 1330 px ─────────────────────
  for (const width of [768, 1024, 1180, 1330]) {
    const narrow = await openChannels(browser, errors, { viewport: { width, height: 1024 }, hasTouch: width === 768 });
    try {
      await narrow.page.waitForSelector(REMOVE, { state: 'visible' });
      await step(`${width}x1024: share and remove are at least ${MIN}x${MIN}, unclipped, hit at their centre and do not overlap`, async () => {
        await assertActionTargets(narrow.page);
      });
    } finally {
      await narrow.ctx.close();
    }
  }

  // ── Add-channel dialog buttons (#249) ────────────────────────────────────
  for (const [width, height] of [[320, 640], [390, 844], [640, 900], [768, 1024]]) {
    const dlg = await openChannels(browser, errors, { viewport: { width, height }, hasTouch: true, isMobile: true });
    try {
      await step(`${width}x${height} touch: the add-channel dialog's buttons are at least ${MIN}x${MIN}, unclipped, hit at their centre with a centred label, and the dialog does not scroll sideways (#249)`, async () => {
        await openAddChannelDialog(dlg.page);
        for (const sel of DIALOG_BUTTONS) {
          const p = await dialogPlacement(dlg.page, sel);
          await assertTarget(dlg.page, sel, sel);
          assert(Math.abs(p.dx) <= 1 && Math.abs(p.dy) <= 1, `${sel} label is off centre by ${p.dx.toFixed(1)}/${p.dy.toFixed(1)}px`);
          assert(p.inside, sel + ' sticks out of the dialog');
        }
        const r = await dlg.page.evaluate(() => {
          const m = document.querySelector('#chAddChannelModal .ch-modal');
          return { dialog: m.scrollWidth - m.clientWidth, page: document.documentElement.scrollWidth - document.documentElement.clientWidth };
        });
        assert(r.dialog <= 0, 'the dialog scrolls horizontally by ' + r.dialog + 'px');
        assert(r.page <= 0, 'the page overflows horizontally by ' + r.page + 'px');
      });
    } finally {
      await dlg.ctx.close();
    }
  }

  const deskDlg = await openChannels(browser, errors, { viewport: { width: 1440, height: 900 } });
  try {
    await step('desktop 1440x900: the add-channel dialog\'s buttons keep their 32 px height (#249 is touch-only)', async () => {
      await openAddChannelDialog(deskDlg.page);
      for (const sel of DIALOG_BUTTONS) {
        const p = await dialogPlacement(deskDlg.page, sel);
        assert(Math.abs(p.h - 32) < 0.5, `${sel} is ${p.h.toFixed(1)}px tall on desktop, expected 32`);
        assert(p.inside, sel + ' sticks out of the dialog');
      }
    });
  } finally {
    await deskDlg.ctx.close();
  }

  await step('no page errors, console errors or unhandled rejections', async () => {
    assert(errors.length === 0, errors.length + ' error(s):\n    ' + errors.join('\n    '));
  });

  await browser.close();
  console.log(`\ntest-issue-2052-touch-target-e2e.js: ${passed} passed, ${failed} failed`);
  process.exit(failed > 0 ? 1 : 0);
}

main().catch((e) => { console.error(e); process.exit(1); });
