// test-issue-620-region-dropdown-viewport-e2e.js
//
// #620 — the region dropdown menu must stay inside the viewport on every page
// that hosts a region filter, the quick-picks row must fit its container, and
// the open menu must not cover the quick picks. Shared code, no per-page cases.
//
// Regions and groups are injected by answering the config endpoints in the
// browser (six regions forces dropdown mode), so the fixture stays untouched.
//
// Usage: BASE_URL=http://localhost:13581 node test-issue-620-region-dropdown-viewport-e2e.js
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const REGIONS = {
  SJC: 'San Jose, California, United States', SFO: 'San Francisco, California, United States',
  OAK: 'Oakland, California, United States', MRY: 'Monterey, California, United States',
  SMF: 'Sacramento, California, United States', LAX: 'Los Angeles International Airport Metropolitan Area, California, United States',
};
const GROUPS = [
  { name: 'Denmark', codes: ['SJC', 'SFO'] }, { name: 'Sweden', codes: ['OAK'] },
  { name: 'Norway and Finland', codes: ['MRY'] }, { name: 'Baltics', codes: ['SMF', 'LAX'] },
  { name: 'Whole of Northern Europe', codes: ['SJC', 'OAK'] },
];
// page route -> container id of the region filter on that page
const PAGES = [
  ['#/channels', '#chRegionFilter'], ['#/nodes', '#nodesRegionFilter'],
  ['#/packets', '#packetsRegionFilter'], ['#/observers', '#obsRegionFilter'],
  ['#/analytics', '#analyticsRegionFilter'], ['#/live', '#liveRegionFilter'],
];
const VIEWPORTS = [[320, 700], [375, 812], [768, 900], [1280, 800]];
// Optional narrowing for quick local mutant runs: ONLY_PAGES=#/channels,#/nodes
const ONLY = process.env.ONLY_PAGES ? process.env.ONLY_PAGES.split(',') : null;

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

(async () => {
  // Static guard: no host may re-anchor the shared menu to its right edge
  // (the Channels header used to, which pushed it 160+ px off-screen).
  await step('no stylesheet rule pins .region-dropdown-menu with right: 0', async () => {
    const css = await (await fetch(BASE + '/style.css')).text();
    const bad = [];
    css.replace(/([^{}]*region-dropdown-menu[^{}]*)\{([^}]*)\}/g, (m, sel, body) => {
      if (/(^|[;\s])right\s*:\s*0(px)?\s*(;|$)/.test(body)) bad.push(sel.trim());
    });
    assert(bad.length === 0, 'right: 0 on ' + bad.join(' | '));
  });
  const browser = await chromium.launch({
    headless: true, executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-setuid-sandbox'],
  });
  for (const [w, h] of VIEWPORTS) {
    for (const [route, sel] of PAGES) {
      if (ONLY && !ONLY.includes(route)) continue;
      const ctx = await browser.newContext({ viewport: { width: w, height: h } });
      await ctx.route((u) => u.pathname === '/api/config/regions', (r) =>
        r.fulfill({ contentType: 'application/json', body: JSON.stringify(REGIONS) }));
      await ctx.route((u) => u.pathname === '/api/config/regions/groups', (r) =>
        r.fulfill({ contentType: 'application/json', body: JSON.stringify(GROUPS) }));
      const page = await ctx.newPage();
      page.setDefaultTimeout(20000);
      await page.goto(BASE + '/' + route, { waitUntil: 'domcontentloaded' });
      await step(`${w}px ${route}: menu inside viewport, picks fit, menu does not cover picks`, async () => {
        // Packets and Live collapse their filters behind a toggle on a phone.
        for (const t of ['.filter-toggle-btn-mirror', '#filterToggleBtn', '#liveControlsToggle']) {
          const tog = await page.$(t);
          if (tog && await tog.isVisible()) {
            const hidden = await page.$eval(sel + ' .region-dropdown-trigger', (el) => !el.offsetParent).catch(() => true);
            if (hidden) await tog.click();
          }
        }
        await page.waitForSelector(sel + ' .region-dropdown-trigger', { state: 'visible' });
        await page.waitForSelector(sel + ' .region-quick-pick');
        const base = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
        await page.click(sel + ' .region-dropdown-trigger');
        await page.waitForSelector(sel + ' .region-dropdown-menu:not([hidden])');
        const r = await page.evaluate((s) => {
                    const vw = document.documentElement.clientWidth;
          const c = document.querySelector(s);
          const box = (el) => { const b = el.getBoundingClientRect(); return { l: b.left, r: b.right, t: b.top, b: b.bottom }; };
          const menu = box(c.querySelector('.region-dropdown-menu'));
          const picks = c.querySelector('.region-quick-picks');
          const pb = box(picks);
          const chips = [...picks.querySelectorAll('.region-quick-pick')].map(box);
          const cb = box(c);
          return {
            vw, menu, pb, cb, chips,
            overflow: document.documentElement.scrollWidth - vw,
          };
        }, sel);
        const e = 0.5;
        assert(r.menu.l >= -e && r.menu.r <= r.vw + e, `menu ${r.menu.l.toFixed(0)}..${r.menu.r.toFixed(0)} outside 0..${r.vw}`);
        assert(r.pb.r <= r.vw + e && r.pb.l >= -e, `quick-picks row ${r.pb.l.toFixed(0)}..${r.pb.r.toFixed(0)} outside viewport ${r.vw}`);
        assert(r.cb.r <= r.vw + e, `filter container right edge ${r.cb.r.toFixed(0)} > viewport ${r.vw}`);
        const bad = r.chips.filter((c) => c.l < r.pb.l - e || c.r > r.pb.r + e || c.r > r.vw + e);
        assert(bad.length === 0, bad.length + ' quick-pick chip(s) outside their row/viewport');
        const overlap = !(r.menu.r <= r.pb.l || r.menu.l >= r.pb.r || r.menu.b <= r.pb.t || r.menu.t >= r.pb.b);
        assert(!overlap, 'open menu covers the quick-picks row');
        // The Analytics tab strip overflows a phone on its own (not a region
        // control); the region UI must not add to whatever the page already has.
        assert(r.overflow <= Math.max(base, 0), 'open menu / quick picks add horizontal page overflow ' + r.overflow + 'px (page baseline ' + base + 'px)');
        if (route !== '#/analytics') assert(r.overflow <= 0, 'horizontal page overflow ' + r.overflow + 'px');
      });
      await ctx.close();
    }
  }
  await browser.close();
  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
