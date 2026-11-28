/**
 * #480 — browser regression: a channel message sent without a region scope
 * (route type FLOOD) shows a compact amber "no scope" marker on the Channels
 * meta line and in the packet-detail Scope row; scoped messages (including a
 * scoped one whose name is unknown), DIRECT routes and non-channel packets
 * show none. Light and dark theme, desktop and 360x800, axe color-contrast on
 * every marker, no horizontal overflow.
 *
 * Uses the real fixture (an unscoped #wardriving message, the unscoped
 * GRP_DATA packet 17910000deadbeef) plus channels.js' own test hooks and a
 * stubbed /api/packets/<hash> for the scoped variants. Only the local test
 * server is contacted.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-issue-480-no-scope-e2e.js
 * Optional: SCREENSHOT_DIR=<dir> saves one screenshot per page/theme/viewport.
 */
'use strict';
const path = require('path');
const { chromium } = require('playwright');
const { AxeBuilder } = require('@axe-core/playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const SHOTS = process.env.SCREENSHOT_DIR || '';
const GRP_DATA_HASH = '17910000deadbeef';
const H_UNSCOPED = 'e2e480aa11223344';
const H_SCOPED = 'e2e480bb11223344';
const H_UNKNOWN = 'e2e480cc11223344';
const H_DIRECT = 'e2e480dd11223344';
const H_TFNULL = 'e2e480ff11223344';
const H_ADVERT = 'e2e480ee11223344';
const DECODED = JSON.stringify({ type: 'CHAN', channel: '#e2e-480', sender: 'ScopeNode', text: 'scope check' });
const TIP = /^Sent without a scope\. The message floods outside your region, and repeaters that limit unscoped traffic \(flood\.max\.unscoped \/ region denyf\) may stop it\. Recommended: set a default scope \(region\) in the companion app\.$/;

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

function packetBody(hash, o) {
  const raw = '15' + '00' + 'aabbccddeeff00112233445566778899';
  const obs = {
    id: 948000001, raw_hex: raw, timestamp: '2026-10-07T10:00:00Z',
    observer_id: 'e2e-480-obs', observer_name: 'e2e-480-obs', direction: 'rx',
    snr: -5, rssi: -101, score: null, hash, route_type: o.route_type, payload_type: o.payload_type,
    payload_version: 0, path_json: '[]', decoded_json: DECODED, created_at: '2026-10-07T10:00:00Z',
  };
  return {
    packet: {
      id: 948000, hash, raw_hex: raw, route_type: o.route_type, payload_type: o.payload_type, payload_version: 0,
      first_seen: obs.timestamp, timestamp: obs.timestamp, observer_id: obs.observer_id,
      snr: -5, rssi: -101, path_json: '[]', decoded_json: DECODED, scope_name: o.scope_name,
    },
    path: [], observation_count: 1, observations: [obs],
  };
}
const BODIES = {
  [H_UNSCOPED]: packetBody(H_UNSCOPED, { route_type: 1, payload_type: 5, scope_name: null }),
  [H_SCOPED]: packetBody(H_SCOPED, { route_type: 0, payload_type: 5, scope_name: '#dk' }),
  [H_UNKNOWN]: packetBody(H_UNKNOWN, { route_type: 0, payload_type: 5, scope_name: '' }),
  [H_DIRECT]: packetBody(H_DIRECT, { route_type: 2, payload_type: 5, scope_name: null }),
  [H_TFNULL]: packetBody(H_TFNULL, { route_type: 0, payload_type: 5, scope_name: null }),
  [H_ADVERT]: packetBody(H_ADVERT, { route_type: 1, payload_type: 4, scope_name: null }),
};
async function stubPackets(context) {
  await context.route((url) => /^\/api\/packets\/e2e480/.test(url.pathname), async (route) => {
    const hash = new URL(route.request().url()).pathname.split('/').pop();
    try {
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(BODIES[hash]) });
    } catch (_) { try { await route.abort(); } catch (__) { /* closed */ } }
  });
}

async function readMarker(locator) {
  return locator.evaluate((el) => {
    const probe = document.createElement('span');
    probe.style.color = 'var(--path-hash-warn)';
    document.body.appendChild(probe);
    const want = getComputedStyle(probe).color;
    probe.style.color = 'var(--warning)';
    const warning = getComputedStyle(probe).color;
    probe.style.color = 'var(--danger)';
    const danger = getComputedStyle(probe).color;
    probe.remove();
    const icon = el.querySelector('svg.ph-icon use[href$="#ph-warning"]');
    const r = el.getBoundingClientRect();
    return {
      cls: el.className, title: el.getAttribute('title') || '', color: getComputedStyle(el).color, want, warning, danger,
      iconHidden: !!icon && icon.closest('svg').getAttribute('aria-hidden') === 'true',
      iconVisible: !!icon && icon.closest('svg').getBoundingClientRect().width > 0,
      visible: Array.from(el.childNodes).filter((n) => n.nodeType === 3).map((n) => n.textContent).join(''),
      sr: (el.querySelector('.sr-only') || {}).textContent || '',
      right: r.right, vw: document.documentElement.clientWidth,
      overflow: document.documentElement.scrollWidth - document.documentElement.clientWidth,
    };
  });
}
function assertMarker(w, where) {
  assert(w.cls.split(/\s+/).includes('no-scope') && w.cls.split(/\s+/).includes('path-hash-warn'), where + ': class ' + w.cls);
  assert(w.iconHidden && w.iconVisible, where + ': warning icon missing');
  assert(w.visible === 'no scope', where + ': visible text ' + JSON.stringify(w.visible));
  assert(/^ — sent without a scope\. Recommended: set a default scope/.test(w.sr), where + ': sr text ' + JSON.stringify(w.sr));
  assert(TIP.test(w.title), where + ': tooltip ' + JSON.stringify(w.title));
  assert(w.color === w.want && w.want === w.warning, where + ': colour ' + w.color + ' vs ' + w.want);
  assert(w.want !== w.danger, where + ': amber must not equal --danger');
  assert(w.right <= w.vw + 0.5, where + ': runs off-screen');
  assert(w.overflow <= 0, where + ': page scrolls horizontally by ' + w.overflow + 'px');
}
async function assertContrast(page, selector, where) {
  await page.waitForFunction((sel) => {
    const el = document.querySelector(sel);
    return !!el && document.getAnimations().every((a) => {
      const t = a.effect && a.effect.target;
      return !(t && t.contains(el)) || a.playState !== 'running';
    });
  }, selector);
  const r = await new AxeBuilder({ page }).include(selector).withRules(['color-contrast']).analyze();
  const bad = r.violations.flatMap((v) => v.nodes.map((n) => n.failureSummary || n.html));
  assert(bad.length === 0, where + ': axe color-contrast ' + bad.join(' | '));
  const measured = r.passes.reduce((k, v) => k + v.nodes.length, 0);
  if (measured < 1) {
    const ratio = await page.$eval(selector, (el) => {
      const toRgb = (s) => (s.match(/\d+(\.\d+)?/g) || []).slice(0, 3).map(Number);
      const lin = (c) => { c /= 255; return c <= 0.03928 ? c / 12.92 : Math.pow((c + 0.055) / 1.055, 2.4); };
      const lum = (x) => 0.2126 * lin(x[0]) + 0.7152 * lin(x[1]) + 0.0722 * lin(x[2]);
      let bgEl = el, bg = null;
      while (bgEl) { const c = getComputedStyle(bgEl).backgroundColor; if (c && c !== 'rgba(0, 0, 0, 0)' && c !== 'transparent') { bg = c; break; } bgEl = bgEl.parentElement; }
      const lf = lum(toRgb(getComputedStyle(el).color)), lb = lum(toRgb(bg || 'rgb(255,255,255)'));
      return (Math.max(lf, lb) + 0.05) / (Math.min(lf, lb) + 0.05);
    });
    assert(ratio >= 4.5, where + ': contrast ' + ratio.toFixed(2) + ':1 < AA 4.5');
  }
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #480 channel messages without a region scope (${BASE}) ===`);

  const variants = [
    { name: 'light desktop', theme: 'light', viewport: { width: 1400, height: 900 } },
    { name: 'dark desktop', theme: 'dark', viewport: { width: 1400, height: 900 } },
    { name: 'light 360x800', theme: 'light', viewport: { width: 360, height: 800 } },
    { name: 'dark 360x800', theme: 'dark', viewport: { width: 360, height: 800 } },
  ];

  // API shape the UI relies on (AGENTS.md rule 4): routeType, not scope name.
  const probe = await browser.newContext();
  const msgs = (await (await probe.request.get(BASE + '/api/channels/%23wardriving/messages?limit=5')).json()).messages || [];
  const grp = await (await probe.request.get(BASE + '/api/packets/' + GRP_DATA_HASH)).json();
  await probe.close();
  await step('API: fixture channel messages carry routeType 1 and no scope; GRP_DATA packet route_type 1', async () => {
    assert(msgs.length > 0, 'no fixture messages');
    assert(msgs.every((m) => m.routeType === 1 && (m.scope == null)), JSON.stringify(msgs.map((m) => [m.routeType, m.scope])));
    assert(grp.packet.route_type === 1 && grp.packet.payload_type === 6 && grp.packet.scope_name == null, 'GRP_DATA shape');
  });

  for (const v of variants) {
    const context = await browser.newContext({ viewport: v.viewport, colorScheme: v.theme });
    await context.addInitScript((theme) => { try { localStorage.setItem('meshcore-theme', theme); } catch (_) {} }, v.theme);
    await stubPackets(context);
    const page = await context.newPage();
    page.setDefaultTimeout(15000);
    page.on('pageerror', (e) => console.error('[pageerror]', e.message));
    const shot = async (label) => {
      if (SHOTS) await page.screenshot({ path: path.join(SHOTS, `480-${label}-${v.name.replace(/\W+/g, '-')}.png`) });
    };

    await step(`${v.name}: theme applied`, async () => {
      await page.goto(BASE + '/#/channels', { waitUntil: 'domcontentloaded' });
      const t = await page.evaluate(() => document.documentElement.getAttribute('data-theme'));
      assert(t === v.theme, 'data-theme is ' + t);
    });

    await step(`${v.name}: channels — real fixture message from the API shows the marker`, async () => {
      await page.goto(BASE + '/#/channels/%23wardriving', { waitUntil: 'load' });
      const marker = page.locator('#chMessages .no-scope').first();
      await marker.waitFor();
      await marker.scrollIntoViewIfNeeded();
      assertMarker(await readMarker(marker), 'channels (fixture)');
      await assertContrast(page, '#chMessages .no-scope', 'channels (fixture)');
      assert(await page.locator('#chMessages .ch-msg-meta', { hasText: 'scope:' }).count() === 0, 'unscoped fixture shows "scope:"');
      await shot('channels-fixture');
    });

    await step(`${v.name}: channels — marker only on unscoped; scoped/unknown/direct unchanged`, async () => {
      await page.waitForFunction(() => typeof window._channelsRenderMessagesForTest === 'function');
      await page.evaluate(() => {
        const base = { text: 'hello', timestamp: '2026-10-07T08:00:00Z', observers: [], repeats: 1 };
        window._channelsSetStateForTest({
          channels: [{ hash: '#e2e-480', name: '#e2e-480', messageCount: 5 }],
          messages: [
            Object.assign({}, base, { sender: 'Unscoped', packetHash: 'h1', routeType: 1, scope: null }),
            Object.assign({}, base, { sender: 'Scoped', packetHash: 'h2', routeType: 0, scope: '#dk' }),
            Object.assign({}, base, { sender: 'UnknownScope', packetHash: 'h3', routeType: 0, scope: '' }),
            Object.assign({}, base, { sender: 'TransportNoName', packetHash: 'h5', routeType: 0, scope: null }),
            Object.assign({}, base, { sender: 'Direct', packetHash: 'h4', routeType: 2, scope: null }),
          ],
          selectedHash: '#e2e-480',
        });
        document.querySelector('.ch-layout')?.classList.add('ch-detail-open');
        window._channelsRenderMessagesForTest();
      });
      const metas = await page.locator('#chMessages .ch-msg-meta').evaluateAll((els) => els.map((el) => ({
        marker: !!el.querySelector('.no-scope'), text: el.textContent })));
      assert(metas.length === 5, 'meta lines ' + metas.length);
      assert(JSON.stringify(metas.map((m) => m.marker)) === '[true,false,false,false,false]', JSON.stringify(metas));
      assert(metas[1].text.includes('scope: #dk'), 'scoped meta: ' + metas[1].text);
      assert(metas[2].text.includes('scope: unknown'), 'unknown-scope meta: ' + metas[2].text);
      assert(metas[3].text.includes('scope: unknown'), 'transport meta: ' + metas[3].text);
      assert(!metas[4].text.includes('scope'), 'direct meta: ' + metas[4].text);
      const marker = page.locator('#chMessages .no-scope');
      await marker.scrollIntoViewIfNeeded();
      assertMarker(await readMarker(marker), 'channels (injected)');
      await assertContrast(page, '#chMessages .no-scope', 'channels (injected)');
      await shot('channels');
    });

    await step(`${v.name}: packet detail — unscoped GRP_TXT shows the Scope marker`, async () => {
      await page.goto(`${BASE}/#/packet/${H_UNSCOPED}`, { waitUntil: 'load' });
      await page.reload({ waitUntil: 'load' });
      const marker = page.locator('dl.detail-meta dd .no-scope');
      await marker.waitFor();
      await marker.scrollIntoViewIfNeeded();
      assertMarker(await readMarker(marker), 'packet detail');
      assert((await page.locator('dl.detail-meta dt', { hasText: /^Scope$/ }).count()) === 1, 'Scope row missing');
      await assertContrast(page, 'dl.detail-meta dd .no-scope', 'packet detail');
      await shot('packet');
    });

    await step(`${v.name}: packet detail — real unscoped GRP_DATA shows the marker`, async () => {
      await page.goto(`${BASE}/#/packet/${GRP_DATA_HASH}`, { waitUntil: 'load' });
      await page.reload({ waitUntil: 'load' });
      const marker = page.locator('dl.detail-meta dd .no-scope');
      await marker.waitFor();
      assertMarker(await readMarker(marker), 'packet detail (GRP_DATA)');
    });

    await step(`${v.name}: packet detail — scoped, unknown-scope, direct and non-channel get no marker`, async () => {
      const expectRow = { [H_SCOPED]: '#dk', [H_UNKNOWN]: 'unknown scope', [H_DIRECT]: null, [H_TFNULL]: null, [H_ADVERT]: null };
      for (const [hash, text] of Object.entries(expectRow)) {
        await page.goto(`${BASE}/#/packet/${hash}`, { waitUntil: 'load' });
        await page.reload({ waitUntil: 'load' });
        await page.locator('dl.detail-meta').waitFor();
        assert(await page.locator('dl.detail-meta .no-scope').count() === 0, hash + ' shows the marker');
        const scopeDd = page.locator('dl.detail-meta dt', { hasText: /^Scope$/ }).locator('xpath=following-sibling::dd[1]');
        if (text == null) assert(await page.locator('dl.detail-meta dt', { hasText: /^Scope$/ }).count() === 0, hash + ' has a Scope row');
        else assert((await scopeDd.textContent()).trim() === text, hash + ' scope text ' + (await scopeDd.textContent()));
      }
    });

    await context.close();
  }

  await browser.close();
  console.log(`\n=== ${passed} passed, ${failed} failed ===`);
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
