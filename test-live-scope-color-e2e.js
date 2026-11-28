/**
 * E2E: Live "Color by scope" and "Scoped only" (#882 M1).
 *   1. #/live?color=scope selects scope mode (saved), disables the hash
 *      checkbox and shows the SCOPES legend section.
 *   2. Stubbed WS packets of every class: named scopes (route 0 and 3),
 *      unknown scope (route 0/3, no name), FLOOD (1), DIRECT (2) and a
 *      foreign-origin scoped packet. Routes and feed stripes take the
 *      --scope-* colours; named scopes get distinct colours; foreign red wins.
 *   3. The legend lists the named scopes (sorted) plus the two neutral rows;
 *      a hostile scope name is rendered as text.
 *   4. "Scoped only" (real click): the URL gets scoped=1, FLOOD/DIRECT
 *      leave the feed and new ones are not drawn; unknown scope still is.
 *   5. Scope off: the hash checkbox is enabled again, ?color= is dropped,
 *      the legend section hides and new routes use the packet-type colour.
 *   6. Deep links and storage: bare #/live restores from storage,
 *      ?color=scope&scoped=0, ?color=hash, ?color=type win and are saved,
 *      a bogus value is ignored.
 *   7. Theme switch: the next route uses the dark palette (cache re-resolved).
 *   8. Phone 375x560 with touch, cold start, real taps: settings gear, the
 *      Color by scope checkbox, the legend: rows inside the viewport, legible,
 *      no sideways page scroll.
 *
 * /api/nodes is served by request interception so the two route endpoints
 * are known nodes at the viewport centre.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-live-scope-color-e2e.js
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

const A = 'e2e882' + 'a'.repeat(58);
const B = 'e2e882' + 'b'.repeat(58);
const F = 'e2e882' + 'f'.repeat(58); // foreign-origin node
const VIEW = 'lat=50.5&lon=10.5&zoom=11';
const HOSTILE = '#<img src=x onerror="window.__xss882=1">';

function nodesBody() {
  const now = new Date().toISOString();
  const node = (pk, name, lat, lon, foreign) => ({
    public_key: pk, name, role: 'repeater', lat, lon, last_seen: now, last_heard: now,
    first_seen: now, advert_count: 3, foreign: !!foreign,
  });
  const nodes = [
    node(A, 'E2E-882 A', 50.5, 10.5),
    node(B, 'E2E-882 B', 50.51, 10.52),
    node(F, 'E2E-882 F', 50.49, 10.48, true),
  ];
  return JSON.stringify({ nodes, total: nodes.length, counts: {} });
}

async function newPage(browser, errors, opts) {
  const ctx = await browser.newContext(Object.assign({ viewport: { width: 1400, height: 900 } }, opts || {}));
  const page = await ctx.newPage();
  page.on('pageerror', (e) => errors.push('pageerror: ' + e.message));
  await page.route(/\/api\/nodes\?/, (route) => route.fulfill({ status: 200, contentType: 'application/json', body: nodesBody() }));
  return { ctx, page };
}

async function gotoLive(page, query) {
  await page.goto(BASE + '/?r=' + Date.now() + '#/live?' + (query ? query + '&' : '') + VIEW, { waitUntil: 'domcontentloaded' });
  await page.waitForFunction((k) => {
    const m = window._liveNodeMarkers && window._liveNodeMarkers();
    return !!(window._liveWSHandler && window._liveWSHandler()) && m && m[k];
  }, A, { timeout: 20000 });
}

let seq = 0;
// One stubbed WS packet; returns its hash. origin: first resolved hop.
async function send(page, routeType, scopeName, origin) {
  const hash = 'e2e882' + (++seq) + 'x' + Date.now().toString(16);
  await page.evaluate((a) => window._liveWSHandler()({ type: 'packet', data: {
    id: 882000 + a.seq, hash: a.hash, raw_hex: '1500', route_type: a.rt, payload_type: 5,
    scope_name: a.scope, path_json: '["E2","E2"]', resolved_path: [a.origin, a.b],
    observer_id: 'e2e-882', observer_name: 'e2e-882', timestamp: new Date().toISOString(), snr: 5, rssi: -90,
    decoded: { header: { payloadTypeName: 'GRP_TXT' }, payload: { text: 'x' }, path: { hops: [] } },
  } }), { seq, hash, rt: routeType, scope: scopeName, origin: origin || A, b: B });
  return hash;
}

// Colour a drawn route used, or throws after the wait.
async function routeColor(page, hash) {
  await page.waitForFunction((h) => window._liveClickablePaths.some((e) => e.hash === h), hash, { timeout: 8000 });
  return page.evaluate((h) => window._liveClickablePaths.find((e) => e.hash === h).color, hash);
}
async function wasDrawn(page, hash, waitMs) {
  await page.waitForTimeout(waitMs);
  return page.evaluate((h) => window._liveClickablePaths.some((e) => e.hash === h), hash);
}
function cssVar(page, name) {
  return page.evaluate((n) => getComputedStyle(document.documentElement).getPropertyValue(n).trim(), name);
}
// Normalises any CSS colour to rgb() for comparison.
function norm(page, c) {
  return page.evaluate((x) => { const d = document.createElement('div'); d.style.color = x; document.body.appendChild(d); const v = getComputedStyle(d).color; d.remove(); return v; }, c);
}
async function stripe(page, hash) {
  return page.evaluate((h) => { const el = document.querySelector('.live-feed-item[data-hash="' + h + '"]'); return el ? getComputedStyle(el).borderLeftColor + '|' + getComputedStyle(el).borderLeftWidth : null; }, hash);
}
async function legendRows(page) {
  await page.waitForTimeout(1300); // legend re-render is coalesced to ≤ 1/s
  return page.$$eval('#scopeLegendList li', (lis) => lis.map((li) => {
    const sw = li.querySelector('.live-scope-swatch');
    return { text: li.textContent.trim(), sw: sw ? getComputedStyle(sw).backgroundColor : null };
  }));
}
function hashParams(page) {
  return page.evaluate(() => new URLSearchParams(location.hash.split('?')[1] || ''));
}
async function param(page, k) {
  return page.evaluate((key) => new URLSearchParams(location.hash.split('?')[1] || '').get(key), k);
}
// Opens the settings panel unless it is open already (its state persists).
async function openControls(page, tap) {
  if (await page.$eval('#liveControlsToggle', (e) => e.getAttribute('aria-expanded')) === 'true') return;
  if (tap) await page.tap('#liveControlsToggle'); else await page.click('#liveControlsToggle');
  await page.waitForSelector('#liveColorScopeToggle', { state: 'visible' });
}
function stored(page, k) { return page.evaluate((key) => localStorage.getItem(key), k); }

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  const errors = [];
  const { ctx, page } = await newPage(browser, errors);
  await page.addInitScript(() => { try { if (!sessionStorage.getItem('e2e882')) { localStorage.removeItem('live-legend-hidden'); localStorage.setItem('meshcore-theme', 'light'); sessionStorage.setItem('e2e882', '1'); } } catch (_) {} });
  console.log('\n=== live colour by scope E2E against ' + BASE + ' ===');

  const h = {};
  await step('?color=scope: scope mode on and saved, hash checkbox disabled, legend section shown', async () => {
    await gotoLive(page, 'color=scope');
    assert(await page.$eval('#liveColorScopeToggle', (e) => e.checked), 'scope checkbox not restored from hash');
    assert(await page.$eval('#liveColorHashToggle', (e) => e.disabled), 'hash checkbox not disabled');
    assert(/colorHashScopeNote/.test(await page.$eval('#liveColorHashToggle', (e) => e.getAttribute('aria-describedby'))), 'precedence note not described');
    assert(await stored(page, 'meshcore-color-packets-by-scope') === 'true', 'deep link not saved');
    assert(await page.$eval('#scopeLegendSection', (e) => !e.hidden && e.getBoundingClientRect().height > 0), 'legend section hidden');
    const rows = await legendRows(page);
    assert(rows.some((r) => r.text === 'No named scopes yet'), 'empty-state row missing: ' + JSON.stringify(rows));
  });

  await step('every packet class gets its colour; foreign red wins over scope', async () => {
    h.dk = await send(page, 0, '#dk');
    h.eu = await send(page, 3, '#eu');
    h.mj = await send(page, 0, '#dk-mj');
    h.unk0 = await send(page, 0, null);
    h.unk3 = await send(page, 3, '');
    h.flood = await send(page, 1, null);
    h.direct = await send(page, 2, null);
    h.foreign = await send(page, 0, '#dk', F);
    const reg = await page.evaluate(() => { const r = window._liveScopeRegistry(); return { dk: r.slotOf('#dk'), eu: r.slotOf('#eu'), mj: r.slotOf('#dk-mj') }; });
    const want = {};
    for (const k of ['dk', 'eu', 'mj']) want[k] = await cssVar(page, '--scope-' + (reg[k] + 1));
    const c = {};
    for (const k of Object.keys(h)) c[k] = await routeColor(page, h[k]);
    assert(c.dk === want.dk && c.eu === want.eu && c.mj === want.mj, 'named: ' + JSON.stringify({ c, want }));
    assert(new Set([c.dk, c.eu, c.mj]).size === 3, 'named scopes share a colour: ' + JSON.stringify(c));
    const unk = await cssVar(page, '--scope-unknown'), none = await cssVar(page, '--scope-none');
    assert(unk && none && unk !== none, 'neutrals equal or missing');
    assert(c.unk0 === unk && c.unk3 === unk, 'unknown: ' + c.unk0 + ' ' + c.unk3);
    assert(c.flood === none && c.direct === none, 'none: ' + c.flood + ' ' + c.direct);
    assert(/^hsl\(0, 85%/.test(c.foreign), 'foreign not red: ' + c.foreign);
    // Feed stripes follow the same colours.
    const s = await stripe(page, h.dk);
    assert(s && s.startsWith(await norm(page, want.dk)) && s.endsWith('4px'), 'feed stripe ' + s);
    assert((await stripe(page, h.flood)).startsWith(await norm(page, none)), 'flood stripe');
  });

  await step('legend: named scopes sorted with their swatches, neutral rows labelled, hostile name is text', async () => {
    await send(page, 0, HOSTILE);
    const rows = await legendRows(page);
    const texts = rows.map((r) => r.text);
    const named = texts.slice(0, 4);
    assert(JSON.stringify(named) === JSON.stringify([HOSTILE, '#dk', '#dk-mj', '#eu'].sort()), 'rows ' + JSON.stringify(texts));
    assert(texts.includes('Unknown scope (T?)') && texts.includes('No scope'), 'neutral rows ' + JSON.stringify(texts));
    const dkRow = rows.find((r) => r.text === '#dk');
    assert(dkRow.sw === await norm(page, await routeColor(page, h.dk)), 'swatch ' + dkRow.sw);
    assert(await page.$$eval('#liveLegend img', (x) => x.length) === 0, 'hostile name became markup');
    assert(await page.evaluate(() => window.__xss882) === undefined, 'hostile name executed');
    // 14 named scopes in the window: 12 rows plus "+2 more".
    for (let i = 0; i < 10; i++) await send(page, 0, '#extra' + i);
    const capped = (await legendRows(page)).map((r) => r.text);
    assert(capped.includes('+2 more'), 'no "+2 more" row: ' + JSON.stringify(capped));
    assert(capped.filter((t) => t.startsWith('#')).length === 12, '12 named rows expected: ' + JSON.stringify(capped));
  });

  await step('"Scoped only" (click): URL scoped=1, FLOOD/DIRECT leave the feed and are not drawn, unknown still is', async () => {
    await openControls(page);
    await page.click('#liveScopedOnlyToggle');
    assert(await param(page, 'scoped') === '1' && await param(page, 'color') === 'scope', 'hash ' + await page.evaluate(() => location.hash));
    assert(await stored(page, 'live-scoped-only') === 'true', 'not saved');
    assert(await page.$('.live-feed-item[data-hash="' + h.flood + '"]') === null, 'flood still in feed');
    assert(await page.$('.live-feed-item[data-hash="' + h.dk + '"]') !== null, '#dk left the feed');
    const f2 = await send(page, 1, null), d2 = await send(page, 2, null);
    const u2 = await send(page, 3, null);
    assert((await routeColor(page, u2)) === await cssVar(page, '--scope-unknown'), 'unknown not drawn');
    assert(!(await wasDrawn(page, f2, 300)) && !(await wasDrawn(page, d2, 0)), 'flood/direct drawn');
    assert(await page.$('.live-feed-item[data-hash="' + f2 + '"]') === null, 'new flood in feed');
    const texts = (await legendRows(page)).map((r) => r.text);
    assert(!texts.includes('No scope') && texts.includes('Unknown scope (T?)'), 'legend ' + JSON.stringify(texts));
  });

  await step('scope off (click): hash re-enabled, ?color dropped, legend hidden, type colour is back', async () => {
    await page.click('#liveColorScopeToggle');
    assert(!(await page.$eval('#liveColorHashToggle', (e) => e.disabled)), 'hash still disabled');
    assert(await param(page, 'color') === null, 'color param kept');
    assert(await stored(page, 'meshcore-color-packets-by-scope') === 'false', 'not saved');
    assert(await page.$eval('#scopeLegendSection', (e) => e.hidden), 'legend section visible');
    const hk = await send(page, 0, '#dk');
    const c = await routeColor(page, hk);
    const type = await page.evaluate(() => window.TYPE_COLORS.GRP_TXT);
    assert(c === type, 'route ' + c + ' type ' + type);
    // Already-drawn routes keep their colour (no retroactive recolouring).
    assert((await routeColor(page, h.dk)) !== type, 'old route recoloured');
  });

  await step('deep links and storage: bare restores, valid params win and are saved, bogus ignored', async () => {
    await gotoLive(page, '');
    assert(!(await page.$eval('#liveColorScopeToggle', (e) => e.checked)) && await page.$eval('#liveScopedOnlyToggle', (e) => e.checked), 'bare #/live did not restore storage');
    await gotoLive(page, 'color=scope&scoped=0');
    assert(await page.$eval('#liveColorScopeToggle', (e) => e.checked) && !(await page.$eval('#liveScopedOnlyToggle', (e) => e.checked)), 'color=scope&scoped=0');
    assert(await stored(page, 'live-scoped-only') === 'false', 'scoped=0 not saved');
    await gotoLive(page, 'color=bogus&scoped=maybe');
    assert(await page.$eval('#liveColorScopeToggle', (e) => e.checked), 'bogus colour changed the mode');
    await gotoLive(page, 'color=type');
    assert(!(await page.$eval('#liveColorScopeToggle', (e) => e.checked)) && !(await page.$eval('#liveColorHashToggle', (e) => e.checked)), 'color=type');
    assert(await stored(page, 'meshcore-color-packets-by-hash') === 'false', 'color=type not saved');
    await openControls(page);
    await page.click('#liveColorHashToggle');
    assert(await param(page, 'color') === 'hash', 'hash toggle did not rewrite ?color=type');
    await gotoLive(page, 'color=hash');
    assert(!(await page.$eval('#liveColorScopeToggle', (e) => e.checked)) && await page.$eval('#liveColorHashToggle', (e) => e.checked), 'color=hash');
  });

  await step('theme switch: next route uses the dark palette value', async () => {
    await gotoLive(page, 'color=scope');
    const light = await routeColor(page, await send(page, 0, '#dk'));
    await page.evaluate(() => document.documentElement.setAttribute('data-theme', 'dark'));
    const dark = await routeColor(page, await send(page, 0, '#dk'));
    const slot = await page.evaluate(() => window._liveScopeRegistry().slotOf('#dk'));
    assert(dark === await cssVar(page, '--scope-' + (slot + 1)) && dark !== light, 'light ' + light + ' dark ' + dark);
    await page.evaluate(() => document.documentElement.setAttribute('data-theme', 'light'));
    await page.waitForTimeout(400);
    // A customizer change with no theme switch ('theme-changed') drops the cache too.
    await routeColor(page, await send(page, 0, '#dk'));
    await page.evaluate((n) => { document.documentElement.style.setProperty(n, '#123456'); window.dispatchEvent(new CustomEvent('theme-changed')); }, '--scope-' + (slot + 1));
    const custom = await routeColor(page, await send(page, 0, '#dk'));
    assert(custom === '#123456', 'customizer override not picked up: ' + custom);
    await page.evaluate((n) => document.documentElement.style.removeProperty(n), '--scope-' + (slot + 1));
  });

  await ctx.close();

  await step('phone 375x560, cold start, real taps: SCOPES list readable in the settings panel', async () => {
    const p = await newPage(browser, errors, { viewport: { width: 375, height: 560 }, hasTouch: true, isMobile: true });
    const ph = p.page;
    await gotoLive(ph, '');
    await openControls(ph, true);
    await ph.$eval('#liveColorScopeToggle', (e) => e.scrollIntoView({ block: 'nearest' }));
    const box = await ph.$eval('#liveColorScopeToggle', (e) => { const r = e.closest('label').getBoundingClientRect(); return { x: r.x, y: r.y, w: r.width, h: r.height }; });
    assert(box.y >= 0 && box.y + box.h <= 560 && box.x >= 0 && box.x + box.w <= 375, 'checkbox off screen ' + JSON.stringify(box));
    // The topmost element at the label centre is the label or its input.
    assert(await ph.evaluate((b) => { const el = document.elementFromPoint(b.x + b.w / 2, b.y + b.h / 2); return !!el && !!el.closest('label') && !!el.closest('label').querySelector('#liveColorScopeToggle'); }, box), 'checkbox label covered');
    await ph.tap('#liveColorScopeToggle');
    assert(await ph.$eval('#liveColorScopeToggle', (e) => e.checked), 'tap did not check');
    assert(await param(ph, 'color') === 'scope', 'tap did not write ?color=scope');
    for (const n of ['#dk', '#dk-mj', '#eu-a-very-long-scope-name-that-must-wrap']) await send(ph, 0, n);
    await send(ph, 1, null);
    await ph.waitForTimeout(1300);
    // #875 unchanged: the legend panel stays hidden on a phone.
    assert(await ph.$eval('#liveLegend', (e) => getComputedStyle(e).display) === 'none', 'legend panel shown on phone');
    await ph.$eval('#scopeLegendMenu', (e) => e.scrollIntoView({ block: 'nearest' }));
    await ph.waitForTimeout(200);
    const info = await ph.evaluate(() => {
      const sec = document.getElementById('scopeLegendMenu');
      const r = sec.getBoundingClientRect();
      const lis = [...sec.querySelectorAll('li')].map((li) => {
        const b = li.getBoundingClientRect(); const cs = getComputedStyle(li);
        return { t: li.textContent.trim(), l: b.left, r: b.right, top: b.top, bottom: b.bottom, fs: parseFloat(cs.fontSize) };
      });
      return { lis, visible: getComputedStyle(sec).display !== 'none' && r.height > 0,
        sw: document.scrollingElement.scrollWidth, iw: innerWidth };
    });
    assert(info.visible, 'SCOPES list not visible in the settings panel');
    assert(info.sw <= info.iw, 'sideways page scroll ' + info.sw + ' > ' + info.iw);
    const texts = info.lis.map((x) => x.t);
    for (const want of ['#dk', '#dk-mj', '#eu-a-very-long-scope-name-that-must-wrap', 'Unknown scope (T?)', 'No scope']) assert(texts.includes(want), 'row ' + want + ' missing: ' + JSON.stringify(texts));
    for (const li of info.lis) {
      assert(li.l >= 0 && li.r <= 375, 'row outside viewport ' + JSON.stringify(li));
      assert(li.fs >= 11, 'row font too small ' + JSON.stringify(li));
    }
    // Every row can be brought on screen (the panel scrolls).
    for (let i = 0; i < info.lis.length; i++) {
      const r = await ph.evaluate((k) => { const li = document.querySelectorAll('#scopeLegendMenu li')[k]; li.scrollIntoView({ block: 'nearest' }); const b = li.getBoundingClientRect(); return { top: b.top, bottom: b.bottom }; }, i);
      assert(r.top >= 0 && r.bottom <= 560, 'row ' + i + ' unreachable ' + JSON.stringify(r));
    }
    if (process.env.E2E_SHOT_DIR) await ph.screenshot({ path: process.env.E2E_SHOT_DIR + '/live-scope-375x560.png' });
    // Both breakpoints where the legend panel is hidden show the list: a
    // taller phone (only <= 640px wide) and a 700x560 window (only the
    // 501-620px-tall rule); a desktop window shows the panel instead.
    for (const vp of [{ width: 375, height: 700 }, { width: 700, height: 560 }, { width: 1400, height: 900 }]) {
      await ph.setViewportSize(vp);
      await ph.waitForTimeout(200);
      const st = await ph.evaluate(() => ({ menu: getComputedStyle(document.getElementById('scopeLegendMenu')).display, legend: getComputedStyle(document.getElementById('liveLegend')).display }));
      const desktop = vp.width > 768;
      assert(desktop ? st.menu === 'none' && st.legend !== 'none' : st.menu !== 'none' && st.legend === 'none', JSON.stringify(vp) + ' ' + JSON.stringify(st));
    }
    await p.ctx.close();
  });

  await browser.close();
  assert(true);
  if (errors.length) { console.error('  page errors:\n    ' + errors.join('\n    ')); failed++; }
  console.log('\n' + passed + ' passed, ' + failed + ' failed');
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
