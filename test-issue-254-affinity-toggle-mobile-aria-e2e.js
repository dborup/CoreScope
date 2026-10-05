/**
 * E2E (#254), follow-up to #248 (#189):
 *
 * 1. Node page "Affinity Debug" card (only shown with debugAffinity; here the
 *    localStorage flag meshcore-affinity-debug=true). Its heading is a
 *    disclosure button: collapsed it shows caret-right with
 *    aria-expanded="false" and a hidden body; a click (and Enter / Space)
 *    opens it (caret-down, "true", body shown) and closes it again.
 *    /api/debug/affinity is answered by page.route, so no request reaches the
 *    server and no API key is sent.
 *
 * 2. Packets group rows against the fixture's seeded 3-observation group (see
 *    the "Seed grouped-packet row for #1486" step in deploy.yml):
 *    - 1400 px: the #189 toggle, data-action="toggle-select" with aria-expanded;
 *    - 390 px (touch): mobile-page-actions.js (#1461 #7) makes activation
 *      select, so the row is select-hash without aria-expanded, and a tap,
 *      Enter or Space (#259 item 4) opens the detail sheet instead of
 *      expanding the group;
 *    - resizing 1400 → 390 → 1400 re-renders the row for each side.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-issue-254-affinity-toggle-mobile-aria-e2e.js
 * SCREENSHOT_DIR=<dir> also saves screenshots (light, dark, 390 px).
 */
'use strict';
const path = require('path');
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const SHOTS = process.env.SCREENSHOT_DIR || '';
const SEED_HASH = 'fae0c9e6d357a814';
const ROW = `#pktBody tr[data-hash="${SEED_HASH}"]`;
const CARD = '#node-affinity-debug';
const BTN = CARD + ' button.affinity-debug-toggle';

// A canned /api/debug/affinity answer, so the test never needs the API key.
const AFFINITY = {
  edges: [],
  resolutions: [],
  stats: {
    totalEdges: 0, totalNodes: 0, resolvedCount: 0, ambiguousCount: 0, unresolvedCount: 0,
    avgConfidence: 0, coldStartCoverage: 0, cacheAge: 'E2E-254 mock', lastRebuild: 'N/A',
  },
};

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

async function shot(page, name, selector) {
  if (!SHOTS) return;
  await page.waitForTimeout(500);
  if (selector) await page.$eval(selector, (el) => el.scrollIntoView({ block: 'center' }));
  await page.screenshot({ path: path.join(SHOTS, '254-' + name + '.png'), fullPage: false });
}

// --- 1. Affinity Debug card ------------------------------------------------

async function cardState(page) {
  return page.evaluate(([card, btnSel]) => {
    const c = document.querySelector(card);
    const btn = document.querySelector(btnSel);
    const body = c && c.querySelector('.affinity-debug-body');
    const h4 = c && c.querySelector('h4');
    return {
      cardVisible: !!c && getComputedStyle(c).display !== 'none',
      hasButton: !!btn,
      aria: btn ? btn.getAttribute('aria-expanded') : null,
      controls: btn ? btn.getAttribute('aria-controls') : null,
      bodyId: body ? body.id : null,
      bodyVisible: !!body && body.getClientRects().length > 0,
      carets: btn ? [...btn.querySelectorAll('.toggle-icon use')].map((u) => (u.getAttribute('href') || '').split('#')[1]) : [],
      inlineHandlers: c ? [c, ...c.querySelectorAll('*')].filter((el) => [...el.attributes].some((a) => /^on/i.test(a.name))).length : -1,
      content: (document.getElementById('affinityDebugContent') || { textContent: '' }).textContent,
      h4Click: h4 ? h4.getAttribute('onclick') : null,
    };
  }, [CARD, BTN]);
}

function assertCard(s, open) {
  assert(s.cardVisible, 'the card is shown with debugAffinity');
  assert(s.hasButton, 'the heading holds a button.affinity-debug-toggle');
  assert(s.inlineHandlers === 0 && s.h4Click === null, 'no inline handlers in the card, got ' + s.inlineHandlers);
  assert(s.controls && s.controls === s.bodyId, 'aria-controls names the body: ' + s.controls + ' vs ' + s.bodyId);
  assert(s.aria === String(open), 'aria-expanded should be "' + open + '", got ' + JSON.stringify(s.aria));
  assert(s.bodyVisible === open, 'body should be ' + (open ? 'shown' : 'hidden'));
  const caret = open ? 'ph-caret-down' : 'ph-caret-right';
  assert(s.carets.length === 1 && s.carets[0] === caret, 'toggle should show exactly #' + caret + ', got ' + JSON.stringify(s.carets));
}

async function affinityCard(browser, theme) {
  const ctx = await browser.newContext({ viewport: { width: 1400, height: 900 } });
  await ctx.addInitScript((t) => {
    localStorage.setItem('meshcore-affinity-debug', 'true');
    localStorage.setItem('meshcore-theme', t);
    window.addEventListener('theme-refresh', () => { window.__254ThemeReady = true; }, { once: true });
  }, theme);
  const affinityRequests = [];
  await ctx.route('**/api/debug/affinity**', (route) => {
    affinityRequests.push({ url: route.request().url(), apiKey: route.request().headers()['x-api-key'] || '' });
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(AFFINITY) });
  });
  const page = await ctx.newPage();
  page.setDefaultTimeout(10000);
  page.on('pageerror', (e) => console.error('[pageerror]', e.message));

  const res = await page.request.get(BASE + '/api/nodes?limit=1');
  const pk = ((await res.json()).nodes || [])[0]?.public_key;
  assert(pk, 'the fixture has a node');

  await page.goto(BASE + '/#/nodes/' + encodeURIComponent(pk), { waitUntil: 'domcontentloaded' });
  await page.waitForSelector(CARD, { state: 'attached' });
  // The first theme load re-renders the full node view once (theme-refresh →
  // loadFullNode), replacing the card. Wait for that, then for the heading to
  // stay the same element, before clicking.
  await page.waitForFunction(() => window.__254ThemeReady === true, null, { timeout: 8000 }).catch(() => {});
  await page.waitForFunction((sel) => {
    const el = document.querySelector(sel);
    if (!el) return false;
    if (window.__254Stable !== el) { window.__254Stable = el; window.__254Since = performance.now(); return false; }
    return performance.now() - window.__254Since > 500;
  }, CARD + ' h4', { polling: 100 });
  return { ctx, page, affinityRequests };
}

// --- 2. Packets group rows ---------------------------------------------------

async function rowState(page) {
  return page.evaluate(([sel, hash]) => {
    const tr = document.querySelector(sel);
    if (!tr) return null;
    return {
      action: tr.getAttribute('data-action'),
      aria: tr.getAttribute('aria-expanded'),
      expanded: tr.classList.contains('expanded'),
      children: document.querySelectorAll('#pktBody tr.group-child[data-parent-hash="' + hash + '"]').length,
    };
  }, [ROW, SEED_HASH]);
}

const isDesktopRow = (s) => !!s && s.action === 'toggle-select' && s.aria === 'false';
const isMobileRow = (s) => !!s && s.action === 'select-hash' && s.aria === null;

async function waitRow(page, pred, what) {
  try {
    await page.waitForFunction(([sel, want]) => {
      const tr = document.querySelector(sel);
      if (!tr) return false;
      const aria = tr.getAttribute('aria-expanded');
      const action = tr.getAttribute('data-action');
      return want === 'desktop' ? (action === 'toggle-select' && aria === 'false')
        : (action === 'select-hash' && aria === null);
    }, [ROW, what], { timeout: 5000 });
  } catch (_) { /* reported below with the actual state */ }
  const s = await rowState(page);
  assert(pred(s), 'expected the ' + what + ' row, got ' + JSON.stringify(s));
  return s;
}

async function openPackets(page) {
  await page.goto(BASE + '/#/packets?hash=' + SEED_HASH + '&timeWindow=0', { waitUntil: 'domcontentloaded' });
  await page.waitForSelector(ROW, { timeout: 12000 });
  await page.waitForTimeout(300);
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });

  console.log(`\n=== #254 Affinity Debug toggle + mobile group-row aria E2E against ${BASE} ===`);

  {
    const { ctx, page, affinityRequests } = await affinityCard(browser, 'light');

    await step('Affinity Debug: shown with the flag, collapsed, caret-right, no inline handler', async () => {
      assertCard(await cardState(page), false);
      await shot(page, 'affinity-collapsed-light', CARD);
    });

    await step('Affinity Debug: a click on the heading opens it (caret-down, aria-expanded true)', async () => {
      await page.click(BTN);
      const s = await cardState(page);
      assertCard(s, true);
      assert(/E2E-254 mock/.test(s.content), 'the body shows the mocked debug data, got ' + JSON.stringify(s.content.slice(0, 120)));
      await shot(page, 'affinity-expanded-light', CARD);
    });

    await step('Affinity Debug: a second click closes it (caret-right, aria-expanded false)', async () => {
      await page.click(BTN);
      assertCard(await cardState(page), false);
    });

    await step('Affinity Debug: Enter opens and Space closes it from the keyboard', async () => {
      await page.focus(BTN);
      await page.keyboard.press('Enter');
      assertCard(await cardState(page), true);
      await page.keyboard.press('Space');
      assertCard(await cardState(page), false);
    });

    await step('Affinity Debug: /api/debug/affinity was answered by the mock, without an API key', async () => {
      assert(affinityRequests.length >= 1, 'the card requested its data');
      assert(affinityRequests.every((r) => r.apiKey === ''), 'no API key sent: ' + JSON.stringify(affinityRequests));
    });
    await ctx.close();
  }

  {
    const { ctx, page } = await affinityCard(browser, 'dark');
    await step('Affinity Debug (dark): toggles the same way', async () => {
      assertCard(await cardState(page), false);
      await shot(page, 'affinity-collapsed-dark', CARD);
      await page.click(BTN);
      assertCard(await cardState(page), true);
      await shot(page, 'affinity-expanded-dark', CARD);
      await page.click(BTN);
      assertCard(await cardState(page), false);
    });
    await ctx.close();
  }

  {
    const ctx = await browser.newContext({ viewport: { width: 1400, height: 900 } });
    const page = await ctx.newPage();
    page.setDefaultTimeout(10000);
    page.on('pageerror', (e) => console.error('[pageerror]', e.message));

    await step('1400 px: the group row is the #189 toggle with aria-expanded', async () => {
      await openPackets(page);
      await waitRow(page, isDesktopRow, 'desktop');
      await page.click(ROW + ' td.col-expand');
      await page.waitForFunction((sel) => document.querySelector(sel).classList.contains('expanded'), ROW, { timeout: 8000 });
      const s = await rowState(page);
      assert(s.aria === 'true' && s.children > 0, 'expanded on desktop: ' + JSON.stringify(s));
      await page.click(ROW + ' td.col-expand');
      await page.waitForFunction((sel) => !document.querySelector(sel).classList.contains('expanded'), ROW, { timeout: 8000 });
    });

    await step('resize 1400 → 390: the row becomes select-hash without aria-expanded', async () => {
      await page.setViewportSize({ width: 390, height: 844 });
      await waitRow(page, isMobileRow, 'mobile');
    });

    await step('resize 390 → 1400: the row is the toggle with aria-expanded again', async () => {
      await page.setViewportSize({ width: 1400, height: 900 });
      await waitRow(page, isDesktopRow, 'desktop');
    });
    await ctx.close();
  }

  {
    const ctx = await browser.newContext({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });
    const page = await ctx.newPage();
    page.setDefaultTimeout(10000);
    page.on('pageerror', (e) => console.error('[pageerror]', e.message));

    await step('390 px: the group row is select-hash and carries no aria-expanded', async () => {
      await openPackets(page);
      await waitRow(page, isMobileRow, 'mobile');
      await shot(page, 'packets-390-row');
    });

    await step('390 px: a tap opens the detail sheet and does not expand the group', async () => {
      await page.tap(ROW + ' td.col-time');
      await page.waitForFunction(() => {
        const s = document.getElementById('mobileDetailSheet');
        return !!s && s.classList.contains('open');
      }, null, { timeout: 8000 });
      const s = await rowState(page);
      assert(s && !s.expanded && s.children === 0, 'the group did not expand: ' + JSON.stringify(s));
      assert(isMobileRow(s), 'still the mobile row after the re-render: ' + JSON.stringify(s));
      await shot(page, 'packets-390-sheet');
      await page.click('#mobileSheetClose').catch(() => {});
    });

    await step('390 px: Enter on the focused row selects too, it does not expand', async () => {
      await page.evaluate(() => { const s = document.getElementById('mobileDetailSheet'); if (s) s.classList.remove('open'); });
      await page.focus(ROW);
      await page.keyboard.press('Enter');
      await page.waitForFunction(() => {
        const s = document.getElementById('mobileDetailSheet');
        return !!s && s.classList.contains('open');
      }, null, { timeout: 8000 });
      const s = await rowState(page);
      assert(s && !s.expanded && s.children === 0, 'Enter did not expand the group: ' + JSON.stringify(s));
    });

    // #259 (4): the row handler treats Enter and Space the same
    // (packets.js, the keydown branch); the original #254 E2E only pressed
    // Enter, so a mutant that dropped Space went unnoticed here.
    await step('390 px: Space on the focused row selects too, it does not expand', async () => {
      await page.evaluate(() => { const s = document.getElementById('mobileDetailSheet'); if (s) s.classList.remove('open'); });
      await page.focus(ROW);
      await page.keyboard.press('Space');
      await page.waitForFunction(() => {
        const s = document.getElementById('mobileDetailSheet');
        return !!s && s.classList.contains('open');
      }, null, { timeout: 8000 });
      const s = await rowState(page);
      assert(s && !s.expanded && s.children === 0, 'Space did not expand the group: ' + JSON.stringify(s));
      assert(isMobileRow(s), 'still the mobile row after Space: ' + JSON.stringify(s));
    });
    await ctx.close();
  }

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed === 0 ? 0 : 1);
})().catch((e) => { console.error(e); process.exit(1); });
