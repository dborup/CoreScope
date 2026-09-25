#!/usr/bin/env node
/* Issue #2052 — rendered touch targets for the channel row actions.
 *
 * test-issue-2052-touch-target-css.js checks the stylesheet text; this test
 * measures what the browser actually renders, in the real responsive DOM:
 *
 *  - Desktop 1440x900 (sectioned .ch-item list): the share and remove actions
 *    of a user-added channel are at least 44x44 CSS px, visible, not clipped
 *    by any overflow container, hit at their centre, not overlapping each
 *    other or the row's other parts, reachable by keyboard with focus-visible,
 *    keep their ARIA/title contract, and do their job without side effects
 *    (share opens the modal, remove asks for confirmation, which is dismissed).
 *  - Mobile 390x844 touch (flat .ch-row list, #1367): the list renders no
 *    share/remove actions at all, every visible tap target in the channel
 *    sidebar is at least 44x44, and tapping a row opens the channel.
 *  - Tablet 768x1024 touch: characterization only. The sidebar is narrower
 *    than a channel row there, so the actions are clipped. That is a known,
 *    separate layout issue (also on master); this step logs the measurement
 *    and does not gate.
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
const MIN = 44;

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

    await step('desktop 1440x900: share and remove are at least 44x44, visible, unclipped and hit at their centre', async () => {
      for (const [name, sel] of [['share', SHARE], ['remove', REMOVE]]) {
        const m = await measure(dp, sel);
        assert(m, name + ' action not rendered');
        assert(m.w >= MIN && m.h >= MIN, `${name} renders ${m.w.toFixed(1)}x${m.h.toFixed(1)}, below ${MIN}x${MIN}`);
        assert(m.visible, name + ' is not visible');
        assert(m.clippedBy.length === 0, name + ' is clipped by ' + m.clippedBy.join(', '));
        assert(m.hit, `${name} centre hits "${m.hitOn}", not the control`);
      }
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

    await step('mobile 390x844: the list renders no share/remove actions, and every visible tap target is at least 44x44', async () => {
      const r = await mp.evaluate((hash) => ({
        mobileRow: !!document.querySelector(`#chList .ch-row[data-hash="${hash}"]`),
        actions: document.querySelectorAll('#chList .ch-icon-btn, #chList [data-share-channel], #chList [data-remove-channel]').length,
        desktopRows: document.querySelectorAll('#chList .ch-item').length,
        small: [...document.querySelectorAll('.ch-sidebar button, .ch-sidebar a[href], .ch-sidebar [role="button"], .ch-sidebar [tabindex="0"]')]
          .filter((e) => { const b = e.getBoundingClientRect(); return b.width > 0 && b.height > 0 && getComputedStyle(e).visibility === 'visible'; })
          .map((e) => { const b = e.getBoundingClientRect(); return { name: e.id || String(e.className).split(' ')[0] || e.tagName, w: +b.width.toFixed(1), h: +b.height.toFixed(1) }; })
          .filter((t) => t.w < 44 || t.h < 44),
        overflow: document.documentElement.scrollWidth - document.documentElement.clientWidth,
      }), HASH);
      assert(r.mobileRow, 'the channel is not rendered as a mobile .ch-row');
      assert(r.actions === 0 && r.desktopRows === 0, `mobile list renders desktop actions (actions=${r.actions}, .ch-item rows=${r.desktopRows})`);
      assert(r.small.length === 0, 'mobile tap targets below 44x44: ' + JSON.stringify(r.small));
      assert(r.overflow <= 0, 'mobile page overflows horizontally by ' + r.overflow + 'px');
    });

    await step('mobile: tapping the channel row opens the channel', async () => {
      await mp.tap(`#chList [data-hash="${HASH}"]`);
      await mp.waitForFunction((h) => location.hash.includes('/channels/') &&
        (location.hash.includes(encodeURIComponent(h)) || location.hash.includes(h)), HASH);
    });
  } finally {
    await mob.ctx.close();
  }

  // ── Tablet: characterization of the known clipping (not gating) ──────────
  const tab = await openChannels(browser, errors, { viewport: { width: 768, height: 1024 }, hasTouch: true });
  try {
    await tab.page.waitForSelector(REMOVE, { state: 'attached' });
    await step('tablet 768x1024: characterization only (known, separate layout issue)', async () => {
      const share = await measure(tab.page, SHARE), remove = await measure(tab.page, REMOVE);
      assert(share && remove, 'tablet actions not rendered');
      const fmt = (n, m) => `${n} ${m.w.toFixed(0)}x${m.h.toFixed(0)} hit=${m.hit ? 'yes' : 'no (' + m.hitOn + ')'} clipped=${m.clippedBy.join('; ') || 'no'}`;
      console.log('    KNOWN LIMITATION (tracked separately, not fixed here): ' + fmt('share', share) + ' | ' + fmt('remove', remove));
    });
  } finally {
    await tab.ctx.close();
  }

  await step('no page errors, console errors or unhandled rejections', async () => {
    assert(errors.length === 0, errors.length + ' error(s):\n    ' + errors.join('\n    '));
  });

  await browser.close();
  console.log(`\ntest-issue-2052-touch-target-e2e.js: ${passed} passed, ${failed} failed`);
  process.exit(failed > 0 ? 1 : 0);
}

main().catch((e) => { console.error(e); process.exit(1); });
