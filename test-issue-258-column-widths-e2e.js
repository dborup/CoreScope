/**
 * E2E (#258): packets column widths do not depend on what the first render had.
 *
 * makeColumnsResizable() (app.js) used to size the columns once, from the first
 * render, and credited full-width colspan rows (vscroll spacers, "No packets
 * found") to column 0. With an aged fixture the default 15-min window is empty,
 * so the expand column got ~43% of the table and Details ~5% (65 px at 1200 px):
 * long advert names sat entirely on the line the #67 clamp hides, and only the
 * icon showed. Now an (almost) empty first render only gives provisional widths
 * and the columns are measured once more when real rows arrive.
 *
 * The fixture is aged by moving the browser clock forward (packets.js computes
 * the window's `since` from Date.now()). The offset is taken from the newest
 * packet, so the effective age is AGE_MIN regardless of when the e2e job runs.
 *
 * At 1200 px (window then 24 h) and 900 px (3 h, the widest at <=1024 px), for
 * an effective age of 20 and 120 min:
 *   - the default window renders no usable rows (so the deferred path is tested);
 *   - after widening the window Details is >= 15% of the table, expand <= 10%;
 *   - every on-screen advert link in Details shows >= 12 px of its name, and the
 *     pinned long name ("KN6PLV-BrkOxfLA-Yebes") shows at least half of it;
 *   - later window changes keep the widths (one re-measure, no per-render work).
 * Also: saved widths (meshcore-pkt-col-widths) are applied unchanged, and a
 * resize handle still resizes and saves (Details is the last column, so the
 * Path handle is dragged left, which gives the room to the columns after it).
 *
 * Usage: BASE_URL=http://localhost:13581 node test-issue-258-column-widths-e2e.js
 * SCREENSHOT_DIR=<dir> also saves screenshots.
 */
'use strict';
const path = require('path');
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const SHOTS = process.env.SCREENSHOT_DIR || '';
const PINNED_ADVERT_ROW = 'e8b09a35ac87fa5c'; // "KN6PLV-BrkOxfLA-Yebes", 21 chars (#252)
const MIN_VISIBLE_LINK_PX = 12;
const MIN_DETAILS_SHARE = 0.15;
const MAX_EXPAND_SHARE = 0.10;
const AGES_MIN = [20, 120];
const VIEWPORTS = [
  { w: 1200, h: 900, wide: '1440' },
  { w: 900, h: 1024, wide: '180' },
];

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

// Column widths and Details advert links, as rendered.
function measure(pinned) {
  const table = document.getElementById('pktTable');
  const ths = [...table.querySelectorAll('thead tr:first-child th')];
  const tw = table.getBoundingClientRect().width;
  const px = (cls) => { const th = ths.find((t) => t.classList.contains(cls)); return th ? th.getBoundingClientRect().width : 0; };
  const usableRows = [...document.querySelectorAll('#pktBody tr')]
    .filter((r) => r.children.length === ths.length && ![...r.children].some((c) => c.colSpan > 1)).length;
  const links = [];
  for (const a of document.querySelectorAll('#pktBody td.col-details .col-details-clip a.hop-link')) {
    const clip = a.closest('.col-details-clip').getBoundingClientRect();
    if (clip.height < 2 || clip.top < 0 || clip.bottom > innerHeight) continue;
    const frags = [...a.getClientRects()].filter((r) => r.width >= 1 && r.height >= 1);
    let visibleW = 0;
    for (const r of frags) {
      const w = Math.min(r.right, clip.right) - Math.max(r.left, clip.left);
      const h = Math.min(r.bottom, clip.bottom) - Math.max(r.top, clip.top);
      if (w >= 1 && h >= 1) { visibleW = Math.round(w); break; }
    }
    // The name's width on one line, in the link's own font.
    const cs = getComputedStyle(a);
    const c2d = document.createElement('canvas').getContext('2d');
    c2d.font = `${cs.fontStyle} ${cs.fontWeight} ${cs.fontSize} ${cs.fontFamily}`;
    links.push({ row: a.closest('tr').getAttribute('data-hash'), text: a.textContent.trim(), visibleW,
      textW: Math.round(c2d.measureText(a.textContent.trim()).width) });
  }
  return {
    tableW: Math.round(tw), expandW: Math.round(px('col-expand')), detailsW: Math.round(px('col-details')),
    thWidths: ths.map((t) => t.style.width), usableRows, links, pinned: links.find((l) => l.row === pinned) || null,
  };
}

// Select a time window; wait for > 20 rows, or with `empty` for the empty state.
async function setWindow(page, value, empty) {
  await page.evaluate((v) => {
    const sel = document.getElementById('fTimeWindow');
    sel.value = v;
    sel.dispatchEvent(new Event('change', { bubbles: true }));
  }, value);
  if (empty) {
    await page.waitForFunction(() => /No packets/.test(document.getElementById('pktBody').textContent) &&
      !document.querySelector('#pktBody tr[data-hash]'), null, { timeout: 10000 });
  } else {
    await page.waitForFunction(() => document.querySelectorAll('#pktBody tr[data-hash]').length > 20, null, { timeout: 10000 });
  }
  await page.waitForTimeout(400);
}

// Newest packet in the fixture, as the server reports it.
async function fixtureAgeMin(request) {
  const res = await request.get(BASE + '/api/packets?limit=1&groupByHash=true');
  const p = ((await res.json()).packets || [])[0];
  const t = p && Date.parse(p.latest || p.first_seen);
  assert(Number.isFinite(t), 'the fixture has a newest packet');
  return (Date.now() - t) / 60000;
}

async function openAged(browser, vp, ageMin, init) {
  const ctx = await browser.newContext({ viewport: { width: vp.w, height: vp.h } });
  if (init) await ctx.addInitScript(init.fn, init.arg);
  const page = await ctx.newPage();
  page.setDefaultTimeout(10000);
  page.on('pageerror', (e) => console.error('[pageerror]', e.message));
  const offsetMin = Math.max(0, ageMin - await fixtureAgeMin(page.request));
  await page.clock.install({ time: Date.now() + offsetMin * 60000 });
  await page.clock.resume();
  await page.goto(BASE + '/#/packets', { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('#pktTable[data-resizable]', { state: 'attached', timeout: 12000 });
  // The first load has finished once the body shows rows or the empty state.
  await page.waitForFunction(() => {
    const b = document.getElementById('pktBody');
    return !!b && (b.querySelector('tr[data-hash]') || /No packets/.test(b.textContent));
  }, null, { timeout: 12000 });
  await page.evaluate(() => document.fonts.ready);
  await page.waitForTimeout(300);
  return { ctx, page };
}

async function shot(page, name) {
  if (!SHOTS) return;
  await page.screenshot({ path: path.join(SHOTS, '258-' + name + '.png'), fullPage: false });
}

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  console.log(`\n=== #258 packets column widths E2E against ${BASE} ===`);

  for (const vp of VIEWPORTS) {
    for (const age of AGES_MIN) {
      const tag = `[${vp.w}px, fixture ${age} min]`;
      const { ctx, page } = await openAged(browser, vp, age);
      let first = null, after = null;

      await step(`${tag} the default 15-min window renders no usable rows`, async () => {
        first = await page.evaluate(measure, PINNED_ADVERT_ROW);
        assert(first.usableRows === 0, 'the first render must be empty to test the deferred measure, got ' + first.usableRows + ' rows');
      });

      await step(`${tag} after widening the window Details is >= ${MIN_DETAILS_SHARE * 100}% and expand <= ${MAX_EXPAND_SHARE * 100}% of the table`, async () => {
        await setWindow(page, vp.wide);
        after = await page.evaluate(measure, PINNED_ADVERT_ROW);
        const d = after.detailsW / after.tableW, x = after.expandW / after.tableW;
        assert(d >= MIN_DETAILS_SHARE, `Details is ${after.detailsW}px of ${after.tableW}px (${(d * 100).toFixed(1)}%); first render had ${first.detailsW}px`);
        assert(x <= MAX_EXPAND_SHARE, `expand is ${after.expandW}px of ${after.tableW}px (${(x * 100).toFixed(1)}%)`);
        await shot(page, `${vp.w}-age${age}`);
      });

      await step(`${tag} advert names in Details show text, not only the icon`, async () => {
        assert(after && after.links.length > 0, 'no on-screen advert link in Details');
        const hidden = after.links.filter((l) => l.visibleW < MIN_VISIBLE_LINK_PX);
        assert(hidden.length === 0, `${hidden.length}/${after.links.length} advert links show < ${MIN_VISIBLE_LINK_PX}px: ` + JSON.stringify(hidden));
        const p = after.pinned;
        assert(p, `pinned long-name row ${PINNED_ADVERT_ROW} is not on screen: ` + JSON.stringify(after.links.map((l) => l.row)));
        assert(p.visibleW >= p.textW / 2, `the long name shows ${p.visibleW}px of ${p.textW}px: ` + JSON.stringify(p));
      });

      await step(`${tag} later renders (adverts only, empty window) keep the widths: measured again only once`, async () => {
        const same = async (when) => {
          const now = await page.evaluate(measure, PINNED_ADVERT_ROW);
          assert(JSON.stringify(now.thWidths) === JSON.stringify(after.thWidths),
            when + ': th widths changed: ' + JSON.stringify(after.thWidths) + ' -> ' + JSON.stringify(now.thWidths));
        };
        // Different rows (adverts only), which a second measure would size differently.
        await page.fill('#packetFilterInput', 'type == ADVERT');
        await page.press('#packetFilterInput', 'Enter');
        await page.waitForFunction(() => {
          const rows = [...document.querySelectorAll('#pktBody tr[data-hash]')];
          return rows.length >= 5 && rows.every((r) => /advert/i.test(r.textContent));
        }, null, { timeout: 10000 });
        await same('adverts only');
        await page.fill('#packetFilterInput', '');
        await page.press('#packetFilterInput', 'Enter');
        await page.waitForTimeout(600);
        // Back to the (empty) default window and out again.
        await setWindow(page, '15', true);
        await same('empty window');
        await setWindow(page, vp.wide);
        await same('wide window again');
      });
      await ctx.close();
    }
  }

  // Saved widths: applied as saved, also when the first render is empty.
  {
    const vp = VIEWPORTS[0];
    let cols = 0;
    {
      const { ctx, page } = await openAged(browser, vp, 20);
      cols = await page.evaluate(() => document.querySelectorAll('#pktTable thead tr:first-child th').length);
      await ctx.close();
    }
    const savedW = Array.from({ length: cols }, (_, i) => (i === 0 ? 4 : 96 / (cols - 1)));
    const { ctx, page } = await openAged(browser, vp, 20, {
      fn: (w) => { localStorage.setItem('meshcore-pkt-col-widths', JSON.stringify(w)); },
      arg: savedW,
    });
    await step('[1200px] saved widths are applied unchanged and survive the rows arriving', async () => {
      const same = (got) => got.length === savedW.length && got.every((w, i) => /%$/.test(w) && Math.abs(parseFloat(w) - savedW[i]) < 0.001);
      const atFirst = await page.evaluate(measure, PINNED_ADVERT_ROW);
      assert(same(atFirst.thWidths), 'first render: ' + JSON.stringify(atFirst.thWidths) + ' vs ' + JSON.stringify(savedW));
      await setWindow(page, vp.wide);
      const filled = await page.evaluate(measure, PINNED_ADVERT_ROW);
      assert(same(filled.thWidths), 'after rows: ' + JSON.stringify(filled.thWidths) + ' vs ' + JSON.stringify(savedW));
    });
    await ctx.close();
  }

  // Resize handle: dragging still resizes and saves. The handle straddles the
  // column edge and the next header cell paints over its right half, so it is
  // grabbed at its left part, inside its own column.
  {
    const vp = VIEWPORTS[0];
    const { ctx, page } = await openAged(browser, vp, 20);
    await step('[1200px] dragging the Time resize handle widens Time and saves the widths', async () => {
      await setWindow(page, vp.wide);
      const colW = (cls) => page.evaluate((c) => document.querySelector('#pktTable th.' + c).getBoundingClientRect().width, cls);
      const before = await colW('col-time');
      const box = await page.evaluate(() => {
        const h = document.querySelector('#pktTable th.col-time .col-resize-handle');
        const r = h && h.getBoundingClientRect();
        return r ? { x: r.left + 2, y: r.top + r.height / 2 } : null;
      });
      assert(box, 'Time has a resize handle');
      await page.mouse.move(box.x, box.y);
      await page.mouse.down();
      await page.mouse.move(box.x + 30, box.y, { steps: 5 });
      await page.mouse.move(box.x + 60, box.y, { steps: 5 });
      await page.mouse.up();
      const after = await colW('col-time');
      const saved = await page.evaluate(() => JSON.parse(localStorage.getItem('meshcore-pkt-col-widths') || 'null'));
      assert(after > before + 10, `Time ${Math.round(before)}px -> ${Math.round(after)}px`);
      assert(Array.isArray(saved) && saved.length > 1, 'widths saved: ' + JSON.stringify(saved));
    });
    await ctx.close();
  }

  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed === 0 ? 0 : 1);
})().catch((e) => { console.error(e); process.exit(1); });
