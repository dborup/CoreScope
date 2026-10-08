#!/usr/bin/env node
/* Chromium E2E for #355's real Observer Neighbors UI, with an isolated API.
 * Run from the repo root: NODE_PATH=/path/to/node_modules node tools/e2e-region-scope-approval.js
 * No staging, production, or persistent database is touched.
 */
'use strict';

const assert = require('assert');
const fs = require('fs');
const http = require('http');
const os = require('os');
const path = require('path');
const { chromium } = require('playwright');

const source = fs.readFileSync(path.join(__dirname, '..', 'public', 'observer-neighbors-tool.js'));
const css = fs.readFileSync(path.join(__dirname, '..', 'public', 'style.css'));
const key = 'e2e-admin-key-only-in-memory';
const names = ['#North', '#South'];
const decisions = new Map();
const requests = new Map();
let nextID = 1;

function json(res, code, body) {
  res.writeHead(code, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' });
  res.end(JSON.stringify(body));
}

function pageHTML() {
  return '<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">' +
    '<link rel="stylesheet" href="/style.css"><style>body{margin:0;background:var(--bg,#121526);color:var(--text,#d7deec)}' +
    'main{padding:20px;max-width:1200px;margin:auto}details{padding:12px}button{cursor:pointer}</style></head>' +
    '<body><main id="app"></main><script>window.registerPage=function(){};window.timeAgo=function(){return "now"};</script>' +
    '<script src="/observer-neighbors-tool.js"></script><script>window.ObserverNeighborsTool.init(document.getElementById("app"))</script></body></html>';
}

const server = http.createServer((req, res) => {
  const url = new URL(req.url, 'http://localhost');
  if (url.pathname === '/' || url.pathname === '/index.html') {
    res.writeHead(200, { 'Content-Type': 'text/html' }); res.end(pageHTML()); return;
  }
  if (url.pathname === '/style.css') {
    res.writeHead(200, { 'Content-Type': 'text/css' }); res.end(css); return;
  }
  if (url.pathname === '/observer-neighbors-tool.js') {
    res.writeHead(200, { 'Content-Type': 'application/javascript' }); res.end(source); return;
  }
  if (url.pathname === '/api/observers/neighbors') {
    json(res, 200, { neighbors: [], unknownScopes: names.filter((name) => decisions.get(name) !== 'approved').map((name) => ({
      scope: name, count: 2, examples: ['Repeater A', 'Repeater B'],
    })) });
    return;
  }
  if (url.pathname.startsWith('/api/admin/')) {
    if (req.headers['x-api-key'] !== key) { json(res, 401, { error: 'invalid admin key' }); return; }
    if (url.pathname === '/api/admin/region-scopes' && req.method === 'GET') {
      json(res, 200, { decisions: Array.from(decisions, ([name, status]) => ({ name, status, createdAt: 1, reviewedAt: 2 })) }); return;
    }
    const statusMatch = url.pathname.match(/^\/api\/admin\/region-scopes\/requests\/([a-z0-9]+)$/);
    if (statusMatch) { json(res, 200, requests.get(statusMatch[1]) || { status: 'error', error: 'missing request' }); return; }
    const actionMatch = url.pathname.match(/^\/api\/admin\/region-scopes\/(approve|reject|revoke)$/);
    if (actionMatch && req.method === 'POST') {
      let body = '';
      req.on('data', (chunk) => { body += chunk; });
      req.on('end', () => {
        const { name } = JSON.parse(body);
        if (!names.includes(name)) { json(res, 400, { error: 'scope not observed' }); return; }
        const action = actionMatch[1];
        const status = action === 'approve' ? 'approved' : action === 'reject' ? 'rejected' : 'revoked';
        if (action === 'revoke' && decisions.get(name) !== 'approved') { json(res, 409, { error: 'not approved' }); return; }
        decisions.set(name, status);
        const requestId = 'request' + nextID++;
        requests.set(requestId, { status, proposal: { name, status } });
        json(res, 202, { requestId });
      });
      return;
    }
  }
  json(res, 404, { error: 'not found' });
});

(async () => {
  let browser;
  try {
    await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
    const base = 'http://127.0.0.1:' + server.address().port;
    browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_PATH || undefined,
      args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'] });
    const context = await browser.newContext({ viewport: { width: 1280, height: 900 } });
    const page = await context.newPage();
    const pageErrors = [];
    page.on('pageerror', (error) => pageErrors.push(error.message));
    page.on('dialog', (dialog) => dialog.accept());
    await page.goto(base, { waitUntil: 'networkidle' });
    await page.locator('#obs-nb-unknown-scopes-wrap').getByText('#North').waitFor();
    assert.strictEqual(await page.locator('[data-scope-action]').count(), 0, 'anonymous action buttons');

    await page.locator('#obs-nb-scope-admin summary').click();
    await page.locator('#obs-nb-admin-key').fill('wrong-key');
    await page.locator('#obs-nb-admin-load').click();
    await page.locator('#obs-nb-admin-status').getByText('invalid admin key').waitFor();
    assert.strictEqual(await page.locator('[data-scope-action]').count(), 0, 'wrong key unlocked actions');

    await page.locator('#obs-nb-admin-key').fill(key);
    await page.locator('#obs-nb-admin-load').click();
    await page.locator('button[data-scope-action="approve"][data-scope-name="#North"]').waitFor();
    await page.locator('button[data-scope-action="approve"][data-scope-name="#North"]').click();
    await page.locator('button[data-scope-action="revoke"][data-scope-name="#North"]').waitFor();
    await page.waitForFunction(() => !document.querySelector('#obs-nb-unknown-scopes-wrap').textContent.includes('#North'));
    assert.strictEqual(decisions.get('#North'), 'approved');

    await page.locator('button[data-scope-action="reject"][data-scope-name="#South"]').click();
    await page.locator('#obs-nb-admin-list').getByText('rejected').waitFor();
    assert.strictEqual(decisions.get('#South'), 'rejected');

    await page.locator('button[data-scope-action="revoke"][data-scope-name="#North"]').click();
    await page.waitForFunction(() => document.querySelector('#obs-nb-unknown-scopes-wrap').textContent.includes('#North'));
    assert.strictEqual(decisions.get('#North'), 'revoked');
    assert.strictEqual(await page.evaluate(() => localStorage.length), 0, 'admin key persisted to localStorage');
    assert.deepStrictEqual(pageErrors, [], 'browser runtime errors');

    const outDir = process.env.SCREENSHOT_DIR || os.tmpdir();
    const desktopShot = path.join(outDir, 'corescope-scope-approval-355-desktop.png');
    const mobileShot = path.join(outDir, 'corescope-scope-approval-355-mobile.png');
    await page.screenshot({ path: desktopShot, fullPage: true });
    await page.setViewportSize({ width: 390, height: 844 });
    const mobileTable = page.locator('#obs-nb-unknown-scopes-wrap .table-fluid-wrap');
    const mobileReject = page.locator('button[data-scope-action="reject"][data-scope-name="#South"]');
    const scrollable = await mobileTable.evaluate((el) => ({ max: el.scrollWidth - el.clientWidth,
      pageOverflow: document.documentElement.scrollWidth - document.documentElement.clientWidth }));
    assert(scrollable.max > 0, 'unknown-scope table should be horizontally scrollable at 390px');
    assert(scrollable.pageOverflow <= 1, 'mobile page itself must not overflow horizontally');
    await mobileTable.evaluate((el) => { el.scrollLeft = el.scrollWidth; });
    const geometry = await mobileReject.evaluate((el) => {
      const button = el.getBoundingClientRect();
      const wrapper = el.closest('.table-fluid-wrap').getBoundingClientRect();
      return { button: { left: button.left, right: button.right, width: button.width },
        wrapper: { left: wrapper.left, right: wrapper.right }, scrollLeft: el.closest('.table-fluid-wrap').scrollLeft };
    });
    assert(geometry.button.width > 0 && geometry.button.left >= geometry.wrapper.left - 1 &&
      geometry.button.right <= geometry.wrapper.right + 1,
      'reject action must be visible after scrolling its table, not clipped: ' + JSON.stringify(geometry));
    await page.screenshot({ path: mobileShot, fullPage: true });
    console.log('PASS: anonymous gate, wrong key, approve, reject, revoke, live refresh, key not persisted, no page errors');
    console.log('Screenshots: ' + desktopShot + ' and ' + mobileShot);
  } finally {
    if (browser) await browser.close();
    await new Promise((resolve) => server.close(resolve));
  }
})().catch((error) => { console.error(error); process.exitCode = 1; });
