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

// #244: open with the widest selectable time window, not the default 15 min.
// makeColumnsResizable() (app.js) sizes the columns from the rows of the first
// render; since #258 a (nearly) empty first render is measured again once real
// rows arrive (test-issue-258-column-widths-e2e.js covers that). With the
// default window the first rows depend on the fixture age (a few packets ~13
// min after freshen-fixture.sh, none after 15 min). The newest rows of a 24 h
// window (3 h at <=1024px, where longer windows are disabled) are the same ones
// for the whole e2e job (150 min timeout), so this test measures one layout
// regardless of the fixture age.
const pinnedWindowMin = (vp) => (vp.w > 1024 ? 1440 : 180);
// A fixture advert with a long name ("KN6PLV-BrkOxfLA-Yebes", 21 chars); it
// renders near the top of the list. It is wider than its Details clip on
// mobile; at 900/1200px Details is wide enough to show it (#258).
const PINNED_ADVERT_ROW = 'e8b09a35ac87fa5c';
// Enough of a name to read and click: about two characters.
const MIN_VISIBLE_LINK_PX = 12;

async function gotoPackets(page, vp) {
  await page.goto(BASE + '/#/packets?timeWindow=' + pinnedWindowMin(vp), { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#packetFilterInput', { state: 'attached', timeout: 8000 });
  await page.waitForFunction(() => !!document.querySelector('#filterUxBar'), { timeout: 8000 });
  // Widen to "All time" so the fixture's long channel messages are rendered
  // (same as #1122) -- but only through the option the page really offers.
  // Below 1025px "All time" is deliberately absent (packets.js omits the
  // option on mobile), so assigning '0' there selects nothing and leaves the
  // select blank. Since #242 a blank select means "keep the saved window"
  // (15 min) rather than All time, so forcing '0' at those widths emptied the
  // list as soon as the fixture was older than 15 minutes. The URL window
  // pinned above already spans the whole fixture, so leave it in place there.
  await page.evaluate(() => {
    const sel = document.getElementById('fTimeWindow');
    if (!sel || !sel.querySelector('option[value="0"]')) return;
    sel.value = '0';
    sel.dispatchEvent(new Event('change', { bubbles: true }));
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
    // Visible height of the summary: the clip's content box (bounding box minus
    // its vertical padding). A wrapped summary makes this a multiple of the
    // line height; a one-line clamp keeps it at ~1 line. (getClientRects() is
    // not used: a block-level clip always reports a single rect.)
    const clipBox = clip.getBoundingClientRect();
    const clipH = clipBox.height - parseFloat(cs.paddingTop) - parseFloat(cs.paddingBottom);
    return {
      hash: r.getAttribute('data-hash'),
      rowH: r.getBoundingClientRect().height,
      clipH,
      lineH, textW: Math.round(textW), contentW: Math.round(contentW),
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
      await gotoPackets(page, vp);
      rows = await page.evaluate(measureRows);
      assert(rows.length > 0, 'no packet rows rendered');
    });

    await step(`[${vp.name}] long Details text is present (test is not vacuous)`, async () => {
      const over = rows.filter(r => r.overflowing);
      assert(over.length > 0,
        'no row has Details text wider than its column -- cannot exercise the clamp. Sample: ' +
        JSON.stringify(rows.slice(0, 3)));
    });

    await step(`[${vp.name}] Details summaries stay on one line`, async () => {
      // Every row, not just the overflowing ones; the overflowing ones are the
      // rows that would wrap without the clamp (proven present above).
      const bad = rows.filter(r => r.clipH > r.lineH * 1.5);
      assert(bad.length === 0,
        `${bad.length} Details summaries wrap (${bad.filter(r => r.overflowing).length} overflowing): ` + JSON.stringify(bad.slice(0, 4)));
    });

    await step(`[${vp.name}] every packet row stays < 60px`, async () => {
      const tall = rows.filter(r => r.rowH >= 60);
      assert(tall.length === 0,
        `${tall.length} rows >= 60px: ` + JSON.stringify(tall.slice(0, 4).map(r => ({ h: r.rowH, text: r.text }))));
    });

    await step(`[${vp.name}] advert links in Details stay visible and clickable`, async () => {
      // Every advert link in a Details summary that is on screen: the part of
      // the link inside the one-line clip (its first line fragment) must be
      // visible and be what a click there hits. A long name that does not fit
      // wraps; the clamp hides its 2nd+ lines, so the link's bounding box (the
      // union of its fragments) reaches into hidden lines and the next row --
      // probing the box centre (#244) tested a hidden spot, not the link.
      const res = await page.evaluate(() => {
        const desc = (el) => el ? {
          tag: el.tagName,
          cls: String(el.className && el.className.baseVal != null ? el.className.baseVal : el.className).slice(0, 60),
          text: (el.textContent || '').trim().slice(0, 40),
          row: el.closest('tr') ? el.closest('tr').getAttribute('data-hash') : undefined,
        } : null;
        const round = (r) => ({ left: Math.round(r.left), top: Math.round(r.top), right: Math.round(r.right), bottom: Math.round(r.bottom) });
        const checked = [];
        for (const a of document.querySelectorAll('#pktBody td.col-details .col-details-clip a.hop-link')) {
          const clip = a.closest('.col-details-clip').getBoundingClientRect();
          // Only rows fully on screen (elementFromPoint needs the viewport).
          if (clip.height < 2 || clip.top < 0 || clip.bottom > innerHeight) continue;
          const frags = [...a.getClientRects()].filter(r => r.width >= 1 && r.height >= 1);
          const out = (r) => r.bottom > clip.bottom + 0.5 || r.right > clip.right + 0.5;
          const item = { row: a.closest('tr').getAttribute('data-hash'), text: a.textContent, clip: round(clip),
            frags: frags.map(round), truncated: frags.some(out) };
          // Visible part of the first fragment that intersects the clip.
          for (const r of frags) {
            const vis = { left: Math.max(r.left, clip.left), right: Math.min(r.right, clip.right),
              top: Math.max(r.top, clip.top), bottom: Math.min(r.bottom, clip.bottom) };
            if (vis.right - vis.left < 1 || vis.bottom - vis.top < 1) continue;
            item.visibleW = Math.round(vis.right - vis.left);
            item.point = { x: vis.left + Math.min(4, (vis.right - vis.left) / 2), y: (vis.top + vis.bottom) / 2 };
            const hit = document.elementFromPoint(item.point.x, item.point.y);
            item.hitIsLink = !!(hit && (hit === a || a.contains(hit)));
            if (!item.hitIsLink) item.hit = desc(hit);
            break;
          }
          checked.push(item);
        }
        return checked;
      });
      assert(res.length > 0, 'no on-screen advert link in Details to check');
      // The pinned long-name row must be among them, so the test cannot pass
      // without exercising a long advert name. On mobile its clip is narrow and
      // the name must be cut by it (the clamped-link case). Wider, Details is
      // sized from the real rows (#258), so the name may fit; it must then show
      // at least half of itself, not just the icon.
      const pinned = res.find(r => r.row === PINNED_ADVERT_ROW);
      assert(pinned, `pinned advert row ${PINNED_ADVERT_ROW} is not on screen; checked: ` +
        JSON.stringify(res.map(r => [r.row, r.text])));
      if (vp.w <= 640) {
        assert(pinned.truncated,
          'pinned advert name fits its mobile Details clip -- the clamped long name is not exercised: ' + JSON.stringify(pinned));
      } else {
        const nameW = pinned.frags.reduce((s, r) => s + (r.right - r.left), 0);
        assert(pinned.visibleW >= nameW / 2,
          `pinned long advert name shows ${pinned.visibleW}px of ${Math.round(nameW)}px in Details: ` + JSON.stringify(pinned));
      }
      const hidden = res.filter(r => !(r.visibleW >= MIN_VISIBLE_LINK_PX));
      assert(hidden.length === 0, `${hidden.length}/${res.length} advert links show < ${MIN_VISIBLE_LINK_PX}px ` +
        'of their name in the one-line Details clip: ' + JSON.stringify(hidden.slice(0, 3)));
      const missed = res.filter(r => !r.hitIsLink);
      assert(missed.length === 0, `${missed.length}/${res.length} advert links in Details are not hit-testable ` +
        '(hit = element under the visible part of the link): ' + JSON.stringify(missed.slice(0, 3)));
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
      // Click a plain data cell (not the expand column, not a link).
      // No separate scrollIntoViewIfNeeded(): the page re-renders #pktBody
      // from under this step. The trigger measured in CI is the one-shot
      // theme-refresh — app.js fetches /api/config/theme, dispatches
      // theme-changed, debounces 300ms, and packets.js re-runs
      // renderTableRows(), which resets _lastVisibleStart and clears
      // tbody.innerHTML (observed: rows ready at +279ms, theme-refresh at
      // +496ms, every row detached at +535ms). The background hop-resolution
      // job re-renders the same way with unbounded latency.
      // scrollIntoViewIfNeeded() resolves one element handle and does NOT
      // re-resolve it, so a detach mid-action throws "Element is not attached
      // to the DOM". click() scrolls as part of its actionability checks and
      // re-resolves the selector on every retry, so it waits the row out.
      await row.locator('td.col-time').click();
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
