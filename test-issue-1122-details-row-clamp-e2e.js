/**
 * E2E (#1122/#1124 follow-up): the packets "Details" cell must not make rows
 * taller by wrapping when table-layout:auto narrows its column.
 *
 * Background: test-issue-1122-packets-filter-ux-e2e.js asserts every path cell
 * (= its row) stays < 60px at 1400px. On Linux CI that started failing once
 * the fixture was ~11.5 min old: the auto table layout narrowed Details from
 * ~184px to ~135px and long channel messages wrapped to 2-3 lines (69/84px).
 * The same wrapping happens deterministically at ordinary desktop/tablet
 * widths (e.g. 1030-1280px, 900px), so this test controls the column width
 * via the viewport instead of waiting for fixture age.
 *
 * Contract asserted at each viewport:
 *   A. Rows whose Details text is longer than the column (proven present, so
 *      the test cannot pass vacuously) render the Details summary on ONE line,
 *      and every packet row stays < 60px.
 *   B. The full message stays reachable: selecting a long channel-message row
 *      shows the complete decoded text in a visible detail surface (#pktRight
 *      on desktop, SlideOver <=1023px, bottom sheet on small mobile).
 *   C. Links inside Details (advert node links) stay visible and hit-testable.
 *
 * Usage: BASE_URL=http://localhost:13581 node test-issue-1122-details-row-clamp-e2e.js
 */
'use strict';
const { chromium } = require('playwright');
const fs = require('fs');
const path = require('path');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const SHOT_DIR = 'e2e-screenshots';

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

const VIEWPORTS = [
  { w: 1200, h: 900, name: 'desktop-1200' }, // Details column ~176px: wraps without the clamp
  { w: 900, h: 1024, name: 'tablet-900' },   // above the 640px mobile clamp, below SlideOver BP
  { w: 375, h: 812, name: 'mobile-375' },    // repo mobile convention (#1668 M6)
];

async function gotoPackets(page) {
  await page.goto(BASE + '/#/packets', { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#packetFilterInput', { state: 'attached', timeout: 8000 });
  await page.waitForFunction(() => !!document.querySelector('#filterUxBar'), { timeout: 8000 });
  // All time, so the fixture's long channel messages are rendered (same as #1122).
  await page.evaluate(() => {
    const sel = document.getElementById('fTimeWindow');
    if (sel) { sel.value = '0'; sel.dispatchEvent(new Event('change', { bubbles: true })); }
  });
  await page.waitForFunction(
    () => document.querySelectorAll('#pktBody tr[data-hash] td.col-details .col-details-clip').length > 20,
    { timeout: 8000 });
  await page.evaluate(() => document.fonts.ready);
}

// Measures every rendered packet row. `overflowing` = rows whose Details text,
// laid out on a single line in the clip's own font, is wider than the cell's
// content box -- i.e. rows that WOULD wrap without a one-line clamp.
function measureRows() {
  const ctx = document.createElement('canvas').getContext('2d');
  const rows = [...document.querySelectorAll('#pktBody tr[data-hash]')]
    .filter(r => r.getBoundingClientRect().height > 0);
  return rows.map(r => {
    const td = r.querySelector('td.col-details');
    const clip = td && td.querySelector('.col-details-clip');
    if (!clip) return null;
    const cs = getComputedStyle(clip);
    ctx.font = `${cs.fontStyle} ${cs.fontWeight} ${cs.fontSize} ${cs.fontFamily}`;
    const tdcs = getComputedStyle(td);
    const contentW = td.clientWidth - parseFloat(tdcs.paddingLeft) - parseFloat(tdcs.paddingRight);
    const textW = ctx.measureText(clip.textContent.replace(/\s+/g, ' ').trim()).width;
    const lineH = parseFloat(cs.lineHeight) || parseFloat(cs.fontSize) * 1.2;
    // Line boxes the clip actually occupies (works for inline and block clips).
    const lineTops = new Set([...clip.getClientRects()].map(q => Math.round(q.top)));
    return {
      hash: r.getAttribute('data-hash'),
      rowH: r.getBoundingClientRect().height,
      clipH: clip.getBoundingClientRect().height,
      lineH, lines: lineTops.size, textW: Math.round(textW), contentW: Math.round(contentW),
      overflowing: textW > contentW + 1,
      text: clip.textContent.trim().slice(0, 60),
    };
  }).filter(Boolean);
}

(async () => {
  if (!fs.existsSync(SHOT_DIR)) fs.mkdirSync(SHOT_DIR, { recursive: true });
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });

  console.log(`\n=== #1122 Details row clamp E2E against ${BASE} ===`);

  for (const vp of VIEWPORTS) {
    const ctx = await browser.newContext({ viewport: { width: vp.w, height: vp.h } });
    const page = await ctx.newPage();
    page.setDefaultTimeout(8000);
    page.on('pageerror', (e) => console.error('[pageerror]', e.message));

    let rows = [];
    await step(`[${vp.name}] navigate to /packets`, async () => {
      await gotoPackets(page);
      rows = await page.evaluate(measureRows);
      assert(rows.length > 0, 'no packet rows rendered');
    });

    await step(`[${vp.name}] long Details text is present (test is not vacuous)`, async () => {
      const over = rows.filter(r => r.overflowing);
      assert(over.length > 0,
        'no row has Details text wider than its column -- cannot exercise the clamp. Sample: ' +
        JSON.stringify(rows.slice(0, 3)));
    });

    await step(`[${vp.name}] overflowing Details summaries stay on one line`, async () => {
      const bad = rows.filter(r => r.overflowing && (r.lines > 1 || r.clipH > r.lineH * 1.5));
      assert(bad.length === 0,
        `${bad.length} Details summaries wrap: ` + JSON.stringify(bad.slice(0, 4)));
    });

    await step(`[${vp.name}] every packet row stays < 60px`, async () => {
      const tall = rows.filter(r => r.rowH >= 60);
      assert(tall.length === 0,
        `${tall.length} rows >= 60px: ` + JSON.stringify(tall.slice(0, 4).map(r => ({ h: r.rowH, text: r.text }))));
    });

    await step(`[${vp.name}] advert links in Details stay visible and clickable`, async () => {
      const res = await page.evaluate(() => {
        const links = [...document.querySelectorAll('#pktBody td.col-details .col-details-clip a.hop-link')];
        for (const a of links) {
          const r = a.getBoundingClientRect();
          if (r.width < 2 || r.height < 2 || r.top < 0 || r.bottom > innerHeight) continue;
          const td = a.closest('td').getBoundingClientRect();
          const x = Math.min(r.left + 4, td.right - 2), y = r.top + r.height / 2;
          const hit = document.elementFromPoint(x, y);
          return { found: true, hitIsLink: !!(hit && (hit === a || a.contains(hit))), text: a.textContent };
        }
        return { found: false, count: links.length };
      });
      assert(res.found, 'no visible advert link in Details to check (' + JSON.stringify(res) + ')');
      assert(res.hitIsLink, 'advert link in Details is not hit-testable: ' + JSON.stringify(res));
    });

    await step(`[${vp.name}] full message is shown when a long row is selected`, async () => {
      // Pick an overflowing channel-message row and fetch its full decoded text.
      const target = await page.evaluate(async (hashes) => {
        for (const h of hashes) {
          const res = await fetch('/api/packets/' + h);
          if (!res.ok) continue;
          const data = await res.json();
          const pkt = data && data.packet;
          let d = pkt && pkt.decoded_json;
          if (typeof d === 'string') { try { d = JSON.parse(d); } catch (_) { d = null; } }
          if (d && d.type === 'CHAN' && typeof d.text === 'string' && d.text.length > 20) return { hash: h, text: d.text };
        }
        return null;
      }, rows.filter(r => r.overflowing).map(r => r.hash));
      assert(target, 'no overflowing channel-message row with decoded text found');
      const row = page.locator(`#pktBody tr[data-hash="${target.hash}"]`).first();
      await row.scrollIntoViewIfNeeded();
      await row.click({ position: { x: 60, y: 10 } });
      // The full text must be VISIBLE in a detail surface (never the table):
      // desktop split pane, SlideOver (<=1023px) or the small-mobile bottom
      // sheet (which shows it in its header summary, #1471). innerText only
      // includes rendered text, so display:none copies do not count.
      await page.waitForFunction((t) => {
        const norm = (s) => s.replace(/\s+/g, ' ');
        return ['#pktRight', '.slide-over-panel', '#mobileDetailSheet.open']
          .map(sel => document.querySelector(sel))
          .some(el => el && el.getBoundingClientRect().width > 0 && norm(el.innerText).includes(norm(t)));
      }, target.text, { timeout: 8000 });
      await page.screenshot({ path: path.join(SHOT_DIR, `issue-1122-details-clamp-${vp.name}.png`), fullPage: false });
    });

    await ctx.close();
  }

  await browser.close();
  console.log(`\n=== Results: passed ${passed} failed ${failed} ===`);
  process.exit(failed > 0 ? 1 : 0);
})().catch(e => { console.error(e); process.exit(1); });
