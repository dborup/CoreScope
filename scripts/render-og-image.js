#!/usr/bin/env node
'use strict';
// Renders scripts/og-image-meshview.html to public/og-image-meshview.png
// (1200x630) with Playwright. Usage: node scripts/render-og-image.js [out.png]
// The page is local and self-contained: no network, no tiles, no fonts fetched.
const path = require('path');
const { chromium } = require('playwright');

(async () => {
  const src = path.resolve(__dirname, 'og-image-meshview.html');
  const out = path.resolve(process.argv[2] || path.join(__dirname, '..', 'public', 'og-image-meshview.png'));
  const browser = await chromium.launch(process.env.CHROMIUM_PATH ? { executablePath: process.env.CHROMIUM_PATH } : {});
  try {
    const page = await browser.newPage({ viewport: { width: 1200, height: 630 }, deviceScaleFactor: 1 });
    await page.goto('file://' + src);
    await page.screenshot({ path: out, type: 'png', clip: { x: 0, y: 0, width: 1200, height: 630 } });
  } finally {
    await browser.close();
  }
  console.log('wrote ' + out);
})().catch((e) => { console.error(e); process.exit(1); });
