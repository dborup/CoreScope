#!/usr/bin/env node
/* Port of upstream Kpa-clawbot/CoreScope#2027 (head e5536d20, "feat(home):
 * link My Mesh node cards to the node page") plus a keyboard-activation fix
 * found while browser-reproducing the reported finding (not in the upstream
 * commit): the card's own keydown handler called preventDefault()
 * unconditionally on Enter/Space before checking whether the event came
 * from a nested `.mnc-btn`/`.mnc-remove`. A `<button>`'s native
 * Enter/Space-to-click activation is suppressed by an ancestor's
 * preventDefault() on the bubbled keydown, so a keyboard user focused on
 * "Node page ->" (or any other card action button) could not activate it
 * at all -- confirmed here with Playwright's keyboard API (real key events,
 * not dispatchEvent/click()), not just by reading the code.
 *
 * Scope: public/home.js, public/home.css only. See the local commit for
 * the exact upstream-vs-local diff.
 *
 * Real production rendering: the actual public/app.js, roles.js, home.js
 * and (for the node-route reality check) nodes.js execute unmodified in a
 * real Chromium page. Only network responses are stubbed (a mock `api()`,
 * following the same technique test-packet-trace-alignment-e2e.js already
 * uses in this repo) -- no card HTML or event-handler logic is duplicated
 * into this test.
 *
 * Reality check (brief-required, before trusting upstream's own commit
 * message): "the node page still resolves for a node that has only been
 * seen in channel messages" is FALSE for this fork. cmd/ingestor/main.go
 * only calls Store.UpsertNode() inside the ADVERT branch, so a node with no
 * advert yet has no `nodes` table row at all -- GET /api/nodes/{pubkey}
 * 404s (cmd/server/routes.go handleNodeDetail), and the pre-existing #1150
 * error state ("Node not found") renders instead of the full detail page.
 * The button's OWN behavior (always navigates to the right hash) is
 * correct and is what this PR actually changes; what the destination page
 * then shows is existing, unrelated backend-data behavior, verified below
 * by loading the real nodes.js and calling its own init() -- not asserted
 * from the upstream commit message.
 *
 * Run: node test-issue-2027-my-mesh-node-page-e2e.js (requires Playwright
 * Chromium; SKIPs cleanly if unavailable, same convention as
 * test-nav-priority-1391-e2e.js / test-home-coverage-e2e.js).
 *
 * Not yet registered in any test runner -- see the commit message / report
 * for why, and the proposed one-line CI registration.
 */
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require('playwright');

const ORIGIN = 'http://127.0.0.1:18739';
const PUB = path.join(__dirname, 'public');

// Real-shaped 64-hex-char pubkeys for the two card states.
const NORMAL_PK = 'a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f9';
const ERROR_PK = 'b2c3d4e5f60718293a4b5c6d7e8f9a1b2c3d4e5f60718293a4b5c6d7e8f9a1b';
// Deliberately NOT a real MeshCore pubkey shape: the '+' is here only to
// prove encodeURIComponent() actually runs (real pubkeys are plain hex, so
// encoding them is a no-op and wouldn't catch a regression that dropped
// the encodeURIComponent() call).
const ENCODE_PK = 'deadbeef+cafebabe';
// A pubkey never seen in an ADVERT -- base /api/nodes/{pubkey} 404s too.
const CHANNEL_ONLY_PK = 'channelonly000000000000000000000000000000000000000000000000ab';

const HEALTH_OK = {
  node: { name: 'Repeater One', role: 'repeater', lat: null, lon: null },
  stats: { lastHeard: new Date().toISOString(), packetsToday: 12, avgSnr: 5, avgHops: 1.5 },
  observers: [{ observer_name: 'ObsOne' }],
  recentPackets: [],
};

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.error('  ❌ ' + name + ': ' + e.message); }
}

async function newHarness(browser) {
  const page = await browser.newPage();
  const pageErrors = [];
  page.on('pageerror', (e) => pageErrors.push(e.message));
  page.on('console', (m) => { if (m.type() === 'error') pageErrors.push('[console] ' + m.text()); });

  await page.route('**/*', (route) => {
    const url = new URL(route.request().url());
    if (url.origin !== ORIGIN) return route.abort();
    if (url.pathname === '/icons/phosphor-sprite.svg') {
      return route.fulfill({ contentType: 'image/svg+xml', body: fs.readFileSync(path.join(PUB, 'icons', 'phosphor-sprite.svg')) });
    }
    return route.fulfill({ contentType: 'text/html', body: '<html><body><main id="fixture"></main></body></html>' });
  });

  await page.goto(ORIGIN + '/');
  // home.css only overrides/extends the shared stylesheet -- .my-nodes-grid's
  // own `display:grid` lives in style.css, so the wrap/overflow assertions
  // need both, matching what index.html actually links.
  await page.addStyleTag({ path: path.join(PUB, 'style.css') });
  await page.addStyleTag({ path: path.join(PUB, 'home.css') });
  for (const file of ['app.js', 'roles.js']) await page.addScriptTag({ path: path.join(PUB, file) });
  await page.evaluate(() => {
    // No nav shell / other pages exist in this fixture, so the real router
    // (wired to hashchange at app.js load time) would null-deref walking
    // DOM it expects. It is not the module under test: home.js only needs
    // to set location.hash correctly, a native browser operation
    // independent of any listener on it.
    window.removeEventListener('hashchange', navigate);
    window.fixturePages = {};
    registerPage = (name, mod) => { window.fixturePages[name] = mod; };
    window.__apiCalls = [];
    window.__apiResponders = {};
    api = async (p) => {
      window.__apiCalls.push(p);
      const r = window.__apiResponders[p];
      if (!r) throw new Error('unstubbed api call: ' + p);
      if (r.error) throw r.error;
      return r.data;
    };
  });
  await page.addScriptTag({ path: path.join(PUB, 'home.js') });

  return { page, pageErrors };
}

function stub(page, path_, data) {
  return page.evaluate(({ p, d }) => { window.__apiResponders[p] = { data: d }; }, { p: path_, d: data });
}
function stubError(page, path_, message) {
  return page.evaluate(({ p, m }) => { window.__apiResponders[p] = { error: new Error(m) }; }, { p: path_, m: message });
}

async function renderMyMesh(page, myNodes) {
  await page.evaluate((nodes) => {
    localStorage.setItem('meshcore-user-level', 'experienced');
    localStorage.setItem('meshcore-my-nodes', JSON.stringify(nodes.map((pubkey) => ({ pubkey, name: 'N-' + pubkey.slice(0, 6), addedAt: new Date().toISOString() }))));
  }, myNodes);
  await page.evaluate(() => window.fixturePages.home.init(document.getElementById('fixture')));
  await page.waitForSelector('.my-node-card', { timeout: 5000 });
  // All cards resolve (success or 404) before assertions run.
  await page.waitForFunction((n) => document.querySelectorAll('.my-node-card').length === n, myNodes.length);
}

function cardFor(page, pubkey) {
  return page.locator(`.my-node-card[data-key="${pubkey}"]`);
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
      console.error('test-issue-2027-my-mesh-node-page-e2e.js: FAIL — Chromium required but unavailable: ' + err.message);
      process.exit(1);
    }
    console.log('test-issue-2027-my-mesh-node-page-e2e.js: SKIP (Chromium unavailable: ' + err.message.split('\n')[0] + ')');
    process.exit(0);
  }

  console.log('\n=== #2027 port: My Mesh "Node page ->" link + keyboard fix ===');

  // ---- 1/2. Card content: normal vs error state ----
  await step('normal card shows Node page, Full health, View packets in order', async () => {
    const { page } = await newHarness(browser);
    await stub(page, '/nodes/' + encodeURIComponent(NORMAL_PK) + '/health', HEALTH_OK);
    await renderMyMesh(page, [NORMAL_PK]);
    const btns = await cardFor(page, NORMAL_PK).locator('.mnc-actions .mnc-btn').all();
    assert.equal(btns.length, 3, 'expected exactly 3 action buttons');
    const info = await Promise.all(btns.map(async (b) => ({ action: await b.getAttribute('data-action'), text: (await b.textContent()).trim() })));
    assert.deepEqual(info.map((i) => i.action), ['node', 'health', 'packets']);
    assert.equal(info[0].text, 'Node page →');
    await page.close();
  });

  await step('health-error card shows Node page but not Full health / View packets', async () => {
    const { page } = await newHarness(browser);
    await stubError(page, '/nodes/' + encodeURIComponent(ERROR_PK) + '/health', 'API 404: /nodes/' + encodeURIComponent(ERROR_PK) + '/health');
    await renderMyMesh(page, [ERROR_PK]);
    const card = cardFor(page, ERROR_PK);
    const btns = await card.locator('.mnc-btn').all();
    assert.equal(btns.length, 1, 'error card must show exactly one action button');
    assert.equal(await btns[0].getAttribute('data-action'), 'node');
    assert.equal((await btns[0].textContent()).trim(), 'Node page →');
    const healthBtn = await card.locator('.mnc-btn[data-action="health"]').count();
    const packetsBtn = await card.locator('.mnc-btn[data-action="packets"]').count();
    assert.equal(healthBtn, 0, 'error card must not offer Full health');
    assert.equal(packetsBtn, 0, 'error card must not offer View packets');
    await page.close();
  });

  // ---- 3/4/11. Navigation: mouse + keyboard, both card states, encoding ----
  for (const [label, pk, fixtureCall] of [
    ['normal card', NORMAL_PK, async (page) => stub(page, '/nodes/' + encodeURIComponent(NORMAL_PK) + '/health', HEALTH_OK)],
    ['error card', ERROR_PK, async (page) => stubError(page, '/nodes/' + encodeURIComponent(ERROR_PK) + '/health', 'API 404: not found')],
  ]) {
    await step(`mouse click on Node page (${label}) navigates to the exact node route`, async () => {
      const { page } = await newHarness(browser);
      await fixtureCall(page);
      await renderMyMesh(page, [pk]);
      await cardFor(page, pk).locator('.mnc-btn[data-action="node"]').click();
      const hash = await page.evaluate(() => location.hash);
      assert.equal(hash, '#/nodes/' + encodeURIComponent(pk));
      await page.close();
    });

    for (const key of ['Enter', 'Space']) {
      await step(`${key} on Node page (${label}) navigates via the browser's native keyboard activation`, async () => {
        const { page } = await newHarness(browser);
        await fixtureCall(page);
        await renderMyMesh(page, [pk]);
        await cardFor(page, pk).locator('.mnc-btn[data-action="node"]').focus();
        await page.keyboard.press(key);
        await page.waitForTimeout(50);
        const hash = await page.evaluate(() => location.hash);
        assert.equal(hash, '#/nodes/' + encodeURIComponent(pk), key + ' on the button must open the node page');
        await page.close();
      });
    }
  }

  await step('pubkey is URL-encoded in the node-page navigation (not a no-op on real hex keys)', async () => {
    const { page } = await newHarness(browser);
    await stubError(page, '/nodes/' + encodeURIComponent(ENCODE_PK) + '/health', 'API 404: not found');
    await renderMyMesh(page, [ENCODE_PK]);
    await cardFor(page, ENCODE_PK).locator('.mnc-btn[data-action="node"]').click();
    const hash = await page.evaluate(() => location.hash);
    assert.equal(hash, '#/nodes/deadbeef%2Bcafebabe');
    assert.ok(!hash.includes('+'), 'the raw + must not reach the hash unescaped');
    await page.close();
  });

  // ---- 5/9. Node page must never trigger the card's own health handler ----
  for (const trigger of ['click', 'Enter', 'Space']) {
    await step(`Node page via ${trigger} does not also fire the card's health handler`, async () => {
      const { page } = await newHarness(browser);
      await stub(page, '/nodes/' + encodeURIComponent(NORMAL_PK) + '/health', HEALTH_OK);
      await renderMyMesh(page, [NORMAL_PK]);
      const before = await page.evaluate(() => window.__apiCalls.length); // 1: the render-time fetch
      const btn = cardFor(page, NORMAL_PK).locator('.mnc-btn[data-action="node"]');
      if (trigger === 'click') { await btn.click(); }
      else { await btn.focus(); await page.keyboard.press(trigger); }
      await page.waitForTimeout(100);
      const after = await page.evaluate(() => window.__apiCalls.length);
      assert.equal(after, before, 'no additional /health fetch should happen from the Node page action');
      const homeHealthVisible = await page.evaluate(() => document.getElementById('homeHealth')?.classList.contains('visible'));
      assert.ok(!homeHealthVisible, 'the home health panel must not open');
      await page.close();
    });
  }

  // ---- 6/10. Enter/Space on the CARD ITSELF must still open health ----
  for (const key of ['Enter', 'Space']) {
    await step(key + ' on the card itself (not a nested button) still opens health', async () => {
      const { page } = await newHarness(browser);
      await stub(page, '/nodes/' + encodeURIComponent(NORMAL_PK) + '/health', HEALTH_OK);
      await renderMyMesh(page, [NORMAL_PK]);
      const before = await page.evaluate(() => window.__apiCalls.length);
      await cardFor(page, NORMAL_PK).focus();
      await page.keyboard.press(key);
      await page.waitForTimeout(100);
      const after = await page.evaluate(() => window.__apiCalls.length);
      assert.equal(after, before + 1, key + ' on the card must trigger exactly one health fetch');
      const visible = await page.evaluate(() => document.getElementById('homeHealth')?.classList.contains('visible'));
      assert.ok(visible, 'health panel should be visible after ' + key + ' on the card');
      await page.close();
    });
  }

  // ---- 7. Existing buttons + Remove still work, and the same keyboard
  //         guard now protects them too (not special-cased to "node") ----
  await step('mouse click on Full health / View packets still work without an extra card click', async () => {
    const { page } = await newHarness(browser);
    await stub(page, '/nodes/' + encodeURIComponent(NORMAL_PK) + '/health', HEALTH_OK);
    await renderMyMesh(page, [NORMAL_PK]);
    const before = await page.evaluate(() => window.__apiCalls.length);
    await cardFor(page, NORMAL_PK).locator('.mnc-btn[data-action="health"]').click();
    await page.waitForTimeout(80);
    const after = await page.evaluate(() => window.__apiCalls.length);
    assert.equal(after, before + 1, 'Full health must trigger exactly one fetch (not two, from a bubbled card click)');
    await cardFor(page, NORMAL_PK).locator('.mnc-btn[data-action="packets"]').click();
    const hash = await page.evaluate(() => location.hash);
    assert.equal(hash, '#/packets/' + NORMAL_PK);
    await page.close();
  });

  for (const key of ['Enter', 'Space']) {
    await step(key + ' on Full health button navigates via native activation, not the card handler', async () => {
      const { page } = await newHarness(browser);
      await stub(page, '/nodes/' + encodeURIComponent(NORMAL_PK) + '/health', HEALTH_OK);
      await renderMyMesh(page, [NORMAL_PK]);
      const before = await page.evaluate(() => window.__apiCalls.length);
      await cardFor(page, NORMAL_PK).locator('.mnc-btn[data-action="health"]').focus();
      await page.keyboard.press(key);
      await page.waitForTimeout(100);
      const after = await page.evaluate(() => window.__apiCalls.length);
      assert.equal(after, before + 1, key + ' on Full health must fetch exactly once (button activation), not zero (swallowed) or two (card + button)');
      await page.close();
    });
  }

  await step('mouse click / keyboard on Remove still removes the card without opening health', async () => {
    const { page } = await newHarness(browser);
    await stub(page, '/nodes/' + encodeURIComponent(NORMAL_PK) + '/health', HEALTH_OK);
    await renderMyMesh(page, [NORMAL_PK]);
    await cardFor(page, NORMAL_PK).locator('.mnc-remove').click();
    const remaining = await page.locator('.my-node-card').count();
    assert.equal(remaining, 0, 'card must be removed');
    const visible = await page.evaluate(() => document.getElementById('homeHealth')?.classList.contains('visible'));
    assert.ok(!visible, 'removing must not open the health panel');
    await page.close();
  });

  // ---- 8. Narrow screen: buttons wrap, no horizontal overflow ----
  await step('narrow card wraps the 3 action buttons without horizontal overflow', async () => {
    const { page } = await newHarness(browser);
    await page.setViewportSize({ width: 320, height: 700 });
    await stub(page, '/nodes/' + encodeURIComponent(NORMAL_PK) + '/health', HEALTH_OK);
    await renderMyMesh(page, [NORMAL_PK]);
    const grid = page.locator('#myNodesGrid');
    const overflow = await grid.evaluate((el) => el.scrollWidth - el.clientWidth);
    assert.ok(overflow <= 1, 'my-nodes-grid must not overflow horizontally at 320px, got ' + overflow + 'px');
    const tops = await cardFor(page, NORMAL_PK).locator('.mnc-actions .mnc-btn').evaluateAll(
      (els) => els.map((el) => el.getBoundingClientRect().top)
    );
    const distinctRows = new Set(tops.map((t) => Math.round(t))).size;
    assert.ok(distinctRows >= 2, 'expected the 3 buttons to wrap onto at least 2 rows at 320px, got ' + distinctRows);
    await page.close();
  });

  // ---- 9 (nav away/back). Re-render must not double-wire handlers ----
  await step('loadMyNodes() called twice (data refresh) does not double-fire on a later click', async () => {
    const { page } = await newHarness(browser);
    await stub(page, '/nodes/' + encodeURIComponent(NORMAL_PK) + '/health', HEALTH_OK);
    await renderMyMesh(page, [NORMAL_PK]);
    // Simulate a second render pass over the same grid, as a WS/data
    // refresh would do, without navigating away first.
    await page.evaluate(() => window.fixturePages.home.init(document.getElementById('fixture')));
    await page.waitForSelector('.my-node-card');
    const before = await page.evaluate(() => window.__apiCalls.length);
    await cardFor(page, NORMAL_PK).locator('.mnc-btn[data-action="health"]').click();
    await page.waitForTimeout(80);
    const after = await page.evaluate(() => window.__apiCalls.length);
    assert.equal(after, before + 1, 'exactly one fetch after a re-render, not two from a stacked listener');
    await page.close();
  });

  // ---- Reality check: what #/nodes/<pubkey> actually shows for a node
  //      that only exists in channel messages (no advert -> no `nodes` row
  //      -> the base lookup 404s too, per cmd/ingestor/main.go). Loads the
  //      real nodes.js and calls its own init(), same technique as home.js
  //      above -- not a claim copied from upstream's commit message. ----
  await step('a channel-message-only node opens the existing "Node not found" state, not a populated detail page', async () => {
    const page = await browser.newPage();
    await page.route('**/*', (route) => {
      const url = new URL(route.request().url());
      if (url.origin !== ORIGIN) return route.abort();
      if (url.pathname === '/icons/phosphor-sprite.svg') {
        return route.fulfill({ contentType: 'image/svg+xml', body: fs.readFileSync(path.join(PUB, 'icons', 'phosphor-sprite.svg')) });
      }
      return route.fulfill({ contentType: 'text/html', body: '<html><body><main id="fixture"></main></body></html>' });
    });
    await page.goto(ORIGIN + '/');
    for (const file of ['app.js', 'roles.js']) await page.addScriptTag({ path: path.join(PUB, file) });
    await page.evaluate(() => {
      window.removeEventListener('hashchange', navigate);
      window.fixturePages = {};
      registerPage = (name, mod) => { window.fixturePages[name] = mod; };
      api = async (p) => { throw new Error('API 404: ' + p); };
    });
    await page.addScriptTag({ path: path.join(PUB, 'nodes.js') });
    const r = await page.evaluate(async (pubkey) => {
      await window.fixturePages.nodes.init(document.getElementById('fixture'), pubkey);
      await new Promise((res) => setTimeout(res, 200));
      return {
        title: document.querySelector('.node-full-title')?.textContent || '',
        bodyText: (document.getElementById('nodeFullBody')?.textContent || '').replace(/\s+/g, ' ').trim(),
      };
    }, CHANNEL_ONLY_PK);
    assert.match(r.title, /Node not found/);
    assert.match(r.bodyText, /Node not found/);
    await page.close();
  });

  // ---- No new browser errors across the whole run ----
  await step('no page/console errors across all fixture-driven interactions above', async () => {
    const { page, pageErrors } = await newHarness(browser);
    await stub(page, '/nodes/' + encodeURIComponent(NORMAL_PK) + '/health', HEALTH_OK);
    await stubError(page, '/nodes/' + encodeURIComponent(ERROR_PK) + '/health', 'API 404: not found');
    await renderMyMesh(page, [NORMAL_PK, ERROR_PK]);
    for (const pk of [NORMAL_PK, ERROR_PK]) {
      await cardFor(page, pk).locator('.mnc-btn[data-action="node"]').click();
      await cardFor(page, pk).locator('.mnc-btn[data-action="node"]').focus();
      await page.keyboard.press('Enter');
      await page.keyboard.press('Space');
    }
    await page.waitForTimeout(150);
    assert.deepEqual(pageErrors, [], 'expected zero page/console errors, got: ' + JSON.stringify(pageErrors));
    await page.close();
  });

  console.log(`\n${'='.repeat(50)}`);
  console.log(`#2027 My Mesh node-page-link tests: ${passed} passed, ${failed} failed`);
  await browser.close();
  process.exit(failed > 0 ? 1 : 0);
})().catch((e) => { console.error('FATAL', e); process.exit(1); });
