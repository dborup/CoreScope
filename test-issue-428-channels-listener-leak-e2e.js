/**
 * E2E (#428): channels.js must not leak its 6 delegated/global handlers onto
 * the shared #app / document / window — the remainder of #405 the #419
 * sweep found but left for the owner:
 *
 *   1. `app` click (ch-back / ch-close-node delegation)
 *   2. `document` keydown — Add-channel modal Escape
 *   3. `document` keydown — Share-channel modal Escape
 *   4. `window` 'mc-channels-show-encrypted-changed'
 *   5. `document` mousemove — sidebar-resize drag
 *   6. `document` mouseup — sidebar-resize drag
 *
 * init() bound all six and never removed any of them in destroy(). #1
 * stacks click delegation directly on #app (same leak class as
 * compare/observers). #2-6 are global (document/window outlives the page),
 * so the fix stores each as a module ref and removes it in destroy(); #1 is
 * rebound to the channels page's own `.ch-layout` container instead.
 *
 * This runs against a real browser + Go server (the CI fixture). It installs
 * an init-script BEFORE the SPA's own scripts run, so every
 * addEventListener/removeEventListener on document/window is counted from a
 * clean baseline. The probe filters by the handler's OWN function name
 * (`_chAddModalEscHandler`, `_chShareModalEscHandler`,
 * `_chShowEncryptedChangedHandler`, `onSidebarDragMove`, `onSidebarDragUp` —
 * all named via plain variable-assigned function expressions, which the JS
 * engine's name inference exposes as `fn.name`) rather than by raw event
 * type: other pages bind/unbind their OWN keydown handlers too (confirmed:
 * nodes.js's own Escape binds already drift independently of this fix), so a
 * type-only count would be contaminated by unrelated pages. Name-filtering
 * isolates exactly the channels.js handlers under test. The document never
 * reloads on hash-only navigation (verified: a JS global set on one page
 * survives navigating to another), so the probe survives SPA routing.
 *
 * Mutant: revert the channels.js fix (rebind click to `app`; stop storing
 * and removing the other 5 refs in destroy()) → the stacking/leak cases go red.
 *
 * Usage: BASE_URL=http://localhost:13950 node test-issue-428-channels-listener-leak-e2e.js
 */
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:3000';

let passed = 0, failed = 0;
async function test(name, fn) {
  try { await fn(); passed++; console.log('  ✅ ' + name); }
  catch (err) { failed++; console.log('  ❌ ' + name + ': ' + err.message); }
}
function assert(cond, msg) { if (!cond) throw new Error(msg || 'assertion failed'); }

const HANDLER_NAMES = ['_chAddModalEscHandler', '_chShareModalEscHandler', '_chShowEncryptedChangedHandler', 'onSidebarDragMove', 'onSidebarDragUp'];

let browser;

// Installs the probe before any page script runs (addInitScript), so adds
// and removes are counted from a true zero baseline, filtered by the
// specific handler function names under test (immune to unrelated churn on
// other pages' own document/window listeners of the same event type).
async function freshPage() {
  const ctx = await browser.newContext({ viewport: { width: 1400, height: 900 } });
  const page = await ctx.newPage();
  await page.addInitScript((names) => {
    window.__appAdds = { click: 0 };
    window.__net = {};
    names.forEach((n) => { window.__net[n] = 0; });
    function patchNet(target) {
      const origAdd = target.addEventListener.bind(target);
      const origRemove = target.removeEventListener.bind(target);
      target.addEventListener = function (type, fn, opts) {
        if (fn && Object.prototype.hasOwnProperty.call(window.__net, fn.name)) window.__net[fn.name]++;
        return origAdd(type, fn, opts);
      };
      target.removeEventListener = function (type, fn, opts) {
        if (fn && Object.prototype.hasOwnProperty.call(window.__net, fn.name)) window.__net[fn.name]--;
        return origRemove(type, fn, opts);
      };
    }
    patchNet(document);
    patchNet(window);
    document.addEventListener('DOMContentLoaded', function () {
      var app = document.getElementById('app');
      if (!app) return;
      var origAppAdd = app.addEventListener.bind(app);
      app.addEventListener = function (type, fn, opts) {
        if (type === 'click') window.__appAdds.click++;
        return origAppAdd(type, fn, opts);
      };
    });
  }, HANDLER_NAMES);
  await page.goto(`${BASE}/#/nodes`, { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#app', { timeout: 10000 });
  return page;
}
const appAdds = (page) => page.evaluate(() => window.__appAdds);
const net = (page) => page.evaluate(() => window.__net);

async function gotoChannels(page) {
  await page.goto(`${BASE}/#/channels`, { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#chList', { timeout: 10000 });
  await page.waitForTimeout(150); // let loadChannels()/render settle
}
async function gotoNodes(page) {
  await page.goto(`${BASE}/#/nodes`, { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#nodesTable', { timeout: 10000 });
}
// Poll a boolean page condition instead of page.waitForSelector(), which
// treats a CSS selector's default wait state as "visible" — useless for
// asserting an element BECAME hidden.
async function waitFor(page, fn, args, timeout = 5000) {
  const start = Date.now();
  for (;;) {
    if (await page.evaluate(fn, args)) return;
    if (Date.now() - start > timeout) throw new Error('waitFor timed out');
    await page.waitForTimeout(50);
  }
}

(async () => {
  browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #428 channels listener-leak E2E against ${BASE} ===`);

  await test('visiting Channels adds NO click listener to #app', async () => {
    const page = await freshPage();
    await gotoChannels(page);
    const adds = await appAdds(page);
    assert(adds.click === 0, '#app gained ' + adds.click + ' click listener(s)');
    await page.context().close();
  });

  await test('on Channels, all 5 document/window handlers are live (net 1 each) while mounted', async () => {
    const page = await freshPage();
    await gotoChannels(page);
    const n = await net(page);
    for (const name of HANDLER_NAMES) assert(n[name] === 1, name + ' net while mounted: ' + n[name]);
    await page.context().close();
  });

  await test('leaving Channels removes all 5 document/window listeners (net back to 0)', async () => {
    const page = await freshPage();
    await gotoChannels(page);
    await gotoNodes(page);
    const n = await net(page);
    for (const name of HANDLER_NAMES) assert(n[name] === 0, name + ' net after leaving: ' + n[name]);
    await page.context().close();
  });

  await test('visiting Channels 3x does not stack #app click or any document/window listener', async () => {
    const page = await freshPage();
    for (let i = 0; i < 3; i++) {
      await gotoChannels(page);
      await gotoNodes(page);
    }
    const adds = await appAdds(page);
    const n = await net(page);
    assert(adds.click === 0, '#app accumulated ' + adds.click + ' click listener(s) over 3 visits');
    for (const name of HANDLER_NAMES) assert(n[name] === 0, name + ' accumulated over 3 visits: ' + n[name]);
    await page.context().close();
  });

  await test('after Channels -> Nodes, a ch-back click injected onto #app is inert', async () => {
    const page = await freshPage();
    await gotoChannels(page); // arm the (would-be) leak
    await gotoNodes(page);    // leave
    await page.evaluate(() => {
      const app = document.getElementById('app');
      const b = document.createElement('button');
      b.id = '__ch428ClickProbe';
      b.setAttribute('data-action', 'ch-back');
      app.appendChild(b); // child of #app so the click bubbles to #app
    });
    await page.$eval('#__ch428ClickProbe', (el) => el.click());
    await page.waitForTimeout(150);
    const h = await page.evaluate(() => location.hash);
    assert(h === '#/nodes', 'a leaked ch-back handler changed the hash to ' + h);
    await page.context().close();
  });

  // ─── Behaviour unchanged ───

  await test('Add-channel modal: open, then Escape still closes it', async () => {
    const page = await freshPage();
    await gotoChannels(page);
    await page.click('#chAddChannelBtn');
    await waitFor(page, () => !document.getElementById('chAddChannelModal').classList.contains('hidden'));
    await page.keyboard.press('Escape');
    await waitFor(page, () => document.getElementById('chAddChannelModal').classList.contains('hidden'));
    await page.context().close();
  });

  await test('Share-channel modal: generate a channel, open Share, Escape still closes it', async () => {
    const page = await freshPage();
    await gotoChannels(page);
    await page.click('#chAddChannelBtn');
    await waitFor(page, () => !document.getElementById('chAddChannelModal').classList.contains('hidden'));
    await page.fill('#chGenerateName', 'E428TestChan');
    await page.click('#chGenerateBtn');
    await page.waitForTimeout(200);
    await page.click('#chModalClose');
    await page.waitForSelector('[data-share-channel]', { timeout: 5000 });
    await page.click('[data-share-channel]');
    await waitFor(page, () => !document.getElementById('chShareModal').classList.contains('hidden'));
    await page.keyboard.press('Escape');
    await waitFor(page, () => document.getElementById('chShareModal').classList.contains('hidden'));
    await page.context().close();
  });

  await test('Sidebar resize drag still works end to end', async () => {
    const page = await freshPage();
    await gotoChannels(page);
    const before = await page.$eval('.ch-sidebar', (el) => el.getBoundingClientRect().width);
    const handle = await page.$('.ch-sidebar-resize');
    const box = await handle.boundingBox();
    // The handle sits half-outside .ch-sidebar's clipped (overflow:hidden)
    // bounds (CSS `right:-3px`); only its leftmost ~3px is actually hit-testable.
    const x = box.x + 1, y = box.y + box.height / 2;
    await page.mouse.move(x, y);
    await page.mouse.down();
    await page.mouse.move(x + 150, y, { steps: 10 });
    await page.waitForTimeout(50);
    await page.mouse.up();
    const after = await page.$eval('.ch-sidebar', (el) => el.getBoundingClientRect().width);
    assert(after > before + 50, 'sidebar did not resize: before=' + before + ' after=' + after);
    const saved = await page.evaluate(() => localStorage.getItem('channels-sidebar-width'));
    assert(saved && parseInt(saved, 10) > 0, 'resized width was not persisted to localStorage');
    await page.context().close();
  });

  await test('the "show encrypted channels" event still triggers a channel reload', async () => {
    const page = await freshPage();
    await gotoChannels(page);
    let reloaded = false;
    page.on('request', (req) => { if (req.url().includes('/api/channels')) reloaded = true; });
    await page.evaluate(() => {
      invalidateApiCache('/channels'); // force a cache miss so the reload is observable over the wire
      window.dispatchEvent(new CustomEvent('mc-channels-show-encrypted-changed'));
    });
    await page.waitForTimeout(400);
    assert(reloaded, 'dispatching mc-channels-show-encrypted-changed did not trigger a /api/channels request');
    await page.context().close();
  });

  await test('ch-back click delegation still navigates back to the channel list', async () => {
    const page = await freshPage();
    await gotoChannels(page);
    // Force the mobile "detail open" layout so ch-back reflects real usage,
    // then dispatch the click directly (the button may be visually hidden
    // at this viewport, but the delegated handler still runs on a real click).
    await page.evaluate(() => {
      document.querySelector('.ch-layout').classList.add('ch-detail-open');
    });
    await page.$eval('[data-action="ch-back"]', (el) => el.click());
    await page.waitForFunction(() => location.hash === '#/channels', { timeout: 5000 });
    const h = await page.evaluate(() => location.hash);
    assert(h === '#/channels', 'ch-back click left hash at ' + h);
    const stillDetailOpen = await page.$eval('.ch-layout', (el) => el.classList.contains('ch-detail-open'));
    assert(!stillDetailOpen, 'ch-back did not clear ch-detail-open');
    await page.context().close();
  });

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed === 0 ? 0 : 1);
})().catch((e) => { console.error(e); process.exit(1); });
