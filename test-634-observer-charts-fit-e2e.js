#!/usr/bin/env node
/* #634 / upstream 2344: observer detail charts must stay inside the viewport
 * at narrow widths (the grid min of 400px used to force horizontal overflow),
 * and still lay out in several columns on wide screens. */
'use strict';
const assert = require('assert');
const { chromium } = require('playwright');
const BASE = process.env.BASE_URL || 'http://localhost:13581';

async function main() {
  const browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_PATH || undefined,
    args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'] });
  let passes = 0;
  try {
    const list = await (await fetch(BASE + '/api/observers')).json();
    const obs = (list.observers || []).find(o => o.packet_count > 0) || (list.observers || [])[0];
    assert.ok(obs, 'fixture has an observer');
    for (const theme of ['light', 'dark']) {
      for (const width of [320, 375, 390, 1280]) {
        const ctx = await browser.newContext({ viewport: { width, height: 800 } });
        await ctx.addInitScript(t => { try { localStorage.setItem('meshcore-theme', t); } catch (e) {} }, theme);
        const page = await ctx.newPage();
        const errors = [];
        page.on('pageerror', e => errors.push(e.message));
        await page.goto(BASE + '/#/observers/' + encodeURIComponent(obs.id), { waitUntil: 'networkidle' });
        await page.waitForSelector('.obs-charts .chart-card canvas', { timeout: 15000 });
        const m = await page.evaluate(() => {
          const grid = document.querySelector('.obs-charts');
          const gr = grid.getBoundingClientRect();
          const cards = [...grid.querySelectorAll('.chart-card')].map(c => c.getBoundingClientRect());
          const canv = [...grid.querySelectorAll('canvas')].map(c => c.getBoundingClientRect());
          return { vw: document.documentElement.clientWidth, docW: document.documentElement.scrollWidth,
            gridRight: gr.right, gridLeft: gr.left, cardRights: cards.map(r => r.right), cardLefts: cards.map(r => r.left),
            canvasRights: canv.map(r => r.right), cols: new Set(cards.map(r => Math.round(r.left))).size };
        });
        const tag = theme + ' ' + width + 'px';
        assert.ok(m.gridRight <= m.vw + 0.5, tag + ': chart grid overflows viewport');
        m.cardRights.forEach(r => assert.ok(r <= m.vw + 0.5, tag + ': chart card overflows viewport'));
        m.canvasRights.forEach(r => assert.ok(r <= m.vw + 0.5, tag + ': canvas overflows viewport'));
        if (width <= 390) assert.strictEqual(m.cols, 1, tag + ': single column on narrow screens');
        if (width === 1280) assert.ok(m.cols >= 2, tag + ': multiple columns on desktop');
        assert.deepStrictEqual(errors, []);
        passes++;
        await ctx.close();
      }
    }
  } finally { await browser.close(); }
  console.log('  ✅ ' + passes + ' observer chart fit checks passed (320/375/390/1280, light + dark)');
}
main().catch(e => { console.error('  ❌ ' + (e && e.message || e)); process.exit(1); });
