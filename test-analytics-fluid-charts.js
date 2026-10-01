/**
 * E2E (#1058): Analytics chart containers — fluid + auto-stacking via
 * container queries.
 *
 * Boots Chromium with a minimal HTML harness that links public/style.css
 * and renders the .analytics-charts grid at 768/1080/1440 viewports.
 *
 * Asserts:
 *  - No horizontal overflow of the chart grid (scrollWidth <= clientWidth).
 *  - Cards STACK (single column) when the .analytics-charts container is
 *    narrower than 800px.
 *  - Cards are SIDE-BY-SIDE (≥2 columns) when the container is at least
 *    1200px wide.
 *  - The .analytics-charts element opts in to container queries via
 *    `container-type: inline-size`.
 *
 * Pure file:// harness — does not require the Go server.
 *
 * Usage: node test-analytics-fluid-charts.js
 *
 * FLUID_CHARTS_CSS_DELAY_MS=<ms> holds style.css back that long on every
 * load, as a slow runner would (#148).
 */
'use strict';
const { chromium } = require('playwright');
const fs = require('fs');
const path = require('path');
const os = require('os');

const CSS_PATH = path.join(__dirname, 'public', 'style.css');
const cssHref = 'file://' + CSS_PATH;

// Minimal harness: a sized wrapper that defines the available width
// for the .analytics-charts container, plus a handful of chart cards
// matching the production markup.
function harnessHTML(wrapperWidth) {
  const card = (full) =>
    `<div class="analytics-chart-card${full ? ' full' : ''}">` +
    `<h4>Card</h4>` +
    `<div class="analytics-chart-desc">Desc</div>` +
    `<svg viewBox="0 0 800 200" style="width:100%;max-height:160px"><rect width="800" height="200" fill="#888"/></svg>` +
    `</div>`;
  return `<!doctype html><html><head>
  <meta charset="utf-8">
  <link rel="stylesheet" href="${cssHref}">
  <style>
    /* Sized wrapper simulates the page's content column width — the
       .analytics-charts inside MUST stay fluid relative to this. */
    #wrap { width: ${wrapperWidth}px; box-sizing: border-box; padding: 0; margin: 0; }
    body { margin: 0; }
  </style>
  </head><body>
  <div id="wrap">
    <div class="analytics-charts" id="grid">
      ${card(false)}${card(false)}${card(false)}${card(false)}
    </div>
  </div>
  </body></html>`;
}

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  \u2713 ' + name); }
  catch (e) { failed++; console.error('  \u2717 ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

(async () => {
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.CHROMIUM_PATH || '/usr/bin/chromium',
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
  });
  const ctx = await browser.newContext();
  // #148: on a slow runner style.css can apply after DOMContentLoaded.
  // cssDelayMs holds it back that long, so a test can reproduce that.
  const envCssDelayMs = Number(process.env.FLUID_CHARTS_CSS_DELAY_MS) || 0;
  let cssDelayMs = envCssDelayMs;
  await ctx.route((url) => url.href === cssHref, async (route) => {
    if (cssDelayMs > 0) await new Promise((r) => setTimeout(r, cssDelayMs));
    await route.continue();
  });
  const page = await ctx.newPage();
  page.on('pageerror', (e) => console.error('[pageerror]', e.message));

  console.log('\n=== #1058 Analytics fluid charts E2E ===');

  async function load(wrapperWidth, viewportWidth) {
    await page.setViewportSize({ width: viewportWidth, height: 900 });
    const tmp = path.join(os.tmpdir(),
      `1058-harness-${wrapperWidth}-${viewportWidth}.html`);
    fs.writeFileSync(tmp, harnessHTML(wrapperWidth));
    await page.goto('file://' + tmp, { waitUntil: 'load' });
    await waitForStyles();
  }

  // #148: DOMContentLoaded does not wait for style.css, and unstyled
  // cards stack in one column. Wait until the stylesheet is attached,
  // then for two frames so layout reflects it, before measuring. This
  // waits on the stylesheet itself, not on the properties under test.
  async function waitForStyles() {
    try {
      await page.waitForFunction(() => {
        const link = document.querySelector('link[rel="stylesheet"]');
        return !!(link && link.sheet);
      }, null, { timeout: 15000 });
    } catch (e) {
      throw new Error('style.css was not applied: ' + e.message);
    }
    await page.evaluate(() => new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r))));
  }

  // Helper: count distinct column-x-positions of chart cards.
  async function colCount() {
    return page.evaluate(() => {
      const cards = Array.from(document.querySelectorAll(
        '.analytics-charts > .analytics-chart-card'));
      const xs = new Set(cards.map(c =>
        Math.round(c.getBoundingClientRect().left)));
      return xs.size;
    });
  }
  async function overflow() {
    return page.evaluate(() => {
      const g = document.getElementById('grid');
      return { scrollW: g.scrollWidth, clientW: g.clientWidth };
    });
  }

  // --- Container-query opt-in -------------------------------------------
  await step('analytics-charts opts in to container queries', async () => {
    await load(1200, 1440);
    const ct = await page.evaluate(() => {
      const g = document.getElementById('grid');
      return getComputedStyle(g).containerType;
    });
    assert(/inline-size|size/.test(ct),
      `expected container-type to be inline-size; got "${ct}"`);
  });

  // --- Viewport 1440: container ≥1200 → side-by-side --------------------
  await step('viewport 1440 / wrapper 1300px → side-by-side (≥2 cols)', async () => {
    await load(1300, 1440);
    const cols = await colCount();
    assert(cols >= 2, `expected ≥2 columns at wrapper 1300px; got ${cols}`);
    const o = await overflow();
    assert(o.scrollW <= o.clientW + 1,
      `horizontal overflow: scrollW=${o.scrollW} clientW=${o.clientW}`);
  });

  // --- #148: measure only once style.css applies ------------------------
  // Same case as above with style.css held back for a second. Unstyled
  // cards stack in one column, so a measurement taken before the
  // stylesheet applies reads 1 column.
  await step('viewport 1440 / wrapper 1300px with style.css held back 1s → side-by-side (≥2 cols)', async () => {
    cssDelayMs = Math.max(envCssDelayMs, 1000);
    try {
      await load(1300, 1440);
    } finally {
      cssDelayMs = envCssDelayMs;
    }
    const cols = await colCount();
    assert(cols >= 2, `expected ≥2 columns at wrapper 1300px; got ${cols}`);
  });

  // --- Viewport 1080: medium width — must not overflow ------------------
  await step('viewport 1080 / wrapper 1040px → no horizontal overflow', async () => {
    await load(1040, 1080);
    const o = await overflow();
    assert(o.scrollW <= o.clientW + 1,
      `horizontal overflow: scrollW=${o.scrollW} clientW=${o.clientW}`);
  });

  // --- Viewport 768: container <800 → must stack vertically -------------
  await step('viewport 768 / wrapper 760px → cards stack (1 col)', async () => {
    await load(760, 768);
    const cols = await colCount();
    assert(cols === 1, `expected 1 column at wrapper 760px; got ${cols}`);
    const o = await overflow();
    assert(o.scrollW <= o.clientW + 1,
      `horizontal overflow: scrollW=${o.scrollW} clientW=${o.clientW}`);
  });

  // --- THE bug: wide viewport + narrow container — must stack ----------
  // Today's @media (max-width:768px) is keyed off viewport, not container.
  // A narrow wrapper inside a wide viewport (e.g., side pane on a 1440
  // screen) should still stack the charts via container queries.
  await step('viewport 1440 / wrapper 600px → cards stack via container query', async () => {
    await load(600, 1440);
    const cols = await colCount();
    assert(cols === 1,
      `expected 1 column when container <800px regardless of viewport; got ${cols}`);
    const o = await overflow();
    assert(o.scrollW <= o.clientW + 1,
      `horizontal overflow at wide-viewport/narrow-container: scrollW=${o.scrollW} clientW=${o.clientW}`);
  });

  // --- Viewport 1920: large desktop → side-by-side, no overflow --------
  await step('viewport 1920 / wrapper 1880px → side-by-side (≥2 cols), no overflow', async () => {
    await load(1880, 1920);
    const cols = await colCount();
    assert(cols >= 2, `expected ≥2 columns at wrapper 1880px; got ${cols}`);
    const o = await overflow();
    assert(o.scrollW <= o.clientW + 1,
      `horizontal overflow at 1920: scrollW=${o.scrollW} clientW=${o.clientW}`);
  });

  // --- Viewport 2560: ultra-wide → side-by-side, no overflow -----------
  await step('viewport 2560 / wrapper 2520px → side-by-side (≥2 cols), no overflow', async () => {
    await load(2520, 2560);
    const cols = await colCount();
    assert(cols >= 2, `expected ≥2 columns at wrapper 2520px; got ${cols}`);
    const o = await overflow();
    assert(o.scrollW <= o.clientW + 1,
      `horizontal overflow at 2560: scrollW=${o.scrollW} clientW=${o.clientW}`);
  });

  // --- AC3: charts must redraw/relayout on viewport resize -------------
  // Open at 1440 wide (side-by-side), then shrink the wrapper to 760
  // (sub-800 container) and assert the layout actually flips to a
  // single column. This guards against any future regression where
  // the grid is computed once and stuck.
  await step('AC3: layout reflows on resize (1440 side-by-side → 768 stacked)', async () => {
    await load(1300, 1440);
    const colsWide = await colCount();
    assert(colsWide >= 2,
      `precondition failed: expected ≥2 cols at 1300px; got ${colsWide}`);
    // Shrink only the wrapper (no full reload) — proves the layout
    // recomputes from the current container width, not a one-shot value.
    await page.evaluate(() => {
      document.getElementById('wrap').style.width = '760px';
    });
    await page.setViewportSize({ width: 768, height: 900 });
    // Same wait as after a load: styles applied, then two frames so the
    // layout is recomputed for the new width.
    await waitForStyles();
    const colsNarrow = await colCount();
    assert(colsNarrow === 1,
      `expected layout to reflow to 1 column after shrink; got ${colsNarrow}`);
    const o = await overflow();
    assert(o.scrollW <= o.clientW + 1,
      `horizontal overflow after resize: scrollW=${o.scrollW} clientW=${o.clientW}`);
  });

  await browser.close();

  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
