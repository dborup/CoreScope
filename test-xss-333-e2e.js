// test-xss-333-e2e.js
//
// Browser-level XSS regression test for issue #333 — node/observer/route
// values rendered into HTML must display as inert text, never as markup.
//
// Unlike the string-marker assertions in test-xss-escape-sinks.js, this test
// parses the PRODUCTION-rendered HTML with a real Chromium DOM and inspects
// the resulting node tree: an unescaped sink produces a real <img>/handler
// element, an escaped one produces a text node. That is the ground-truth
// definition of "safe" — the browser's own parser decides, not a regex.
//
// Self-contained (no BASE_URL / live Go server), following the convention of
// test-nodes-favorite-rerender-e2e.js: a tiny local static server provides a
// real http origin, the real escapeHtml and the real sink templates are
// lifted from public/*.js, and each representative path is rendered and
// inspected in-browser.
//
// Representative paths (issue #333): unknown route, observer region, packet
// region badge, geo-filter prune preview (with customize-v2.js's own esc()),
// favorites. No payload ever leaves this machine. The full-page versions of
// the round-2 sinks live in test-xss-333-pages-e2e.js.
'use strict';
const { chromium } = require('playwright');
const http = require('http');
const fs = require('fs');
const path = require('path');

let passed = 0, failed = 0;
async function test(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}

// --- Hostile + benign bindings -------------------------------------------
// TAG breaks text context (angle brackets); the trailing quotes probe
// double/single-quoted attribute breakout.
const TAG = '<img src=x onerror="window.__xss333=(window.__xss333||0)+1">';
const ATTR = '"\' onmouseover="window.__xss333=1"';
const PAYLOAD = TAG + ATTR;
// Benign name that MUST render verbatim: ampersand shown as '&' (not &amp;),
// Danish letters and an emoji preserved.
const BENIGN = 'Æbleø & Mesh 🛰 café';

// --- Lift the real escapeHtml source from public/app.js ------------------
function escapeHtmlSrc() {
  const src = fs.readFileSync(path.join(__dirname, 'public', 'app.js'), 'utf8');
  const m = src.match(/function escapeHtml\(s\)\s*\{[\s\S]*?\n\}/);
  if (!m) throw new Error('could not lift escapeHtml from public/app.js');
  return m[0];
}

// --- Lift a sink template / expression from a source file ----------------
function liftTemplate(file, regex) {
  const src = fs.readFileSync(path.join(__dirname, 'public', file), 'utf8');
  const m = src.match(regex);
  if (!m) throw new Error(`sink not found in public/${file} via ${regex}`);
  return m[1] || m[0];
}

// #333 F6: sink regexes match the escaped AND the unescaped shape, so a
// mutant that drops the escaper still finds the sink and fails on behaviour
// (injected markup), not on "sink not found".
const OBS_REGION_CELL = /(<td data-value="\$\{(?:escapeHtml\()?o\.iata[\s\S]*?<\/td>)/;
const PKT_REGION_CELL = /(<td class="col-region">\$\{groupRegion \? `<span class="badge-region">\$\{[^}]*groupRegion[^}]*\}<\/span>` : '—'\}<\/td>)/;

// --- Start a static server on a free port >= 13900 -----------------------
function startServer() {
  return new Promise((resolve, reject) => {
    const server = http.createServer((req, res) => {
      res.writeHead(200, { 'Content-Type': 'text/html' });
      res.end('<!DOCTYPE html><html><body><div id="host"></div></body></html>');
    });
    let port = 13911;
    const tryListen = () => {
      server.once('error', (err) => {
        if (err.code === 'EADDRINUSE' && port < 13999) { port++; tryListen(); }
        else reject(err);
      });
      server.listen(port, '127.0.0.1', () => resolve({ server, port }));
    };
    tryListen();
  });
}

// Render `renderExpr` (a JS expression yielding an HTML string) in-browser,
// inject it into a real DOM subtree, and report what the parser produced.
// `escName`/`escSrc` override the escaper in scope (e.g. customize-v2.js's
// own esc), so a sink is rendered with the helper its file really calls.
async function renderAndInspect(page, { renderExpr, argNames, args, wrap, escName, escSrc }) {
  return page.evaluate(({ appEscSrc, renderExpr, argNames, args, wrap, escName, escSrc }) => {
    delete window.__xss333;
    const truncate = (s, n) => (s == null ? '' : String(s).slice(0, n));
    const escapeHtml = (new Function(appEscSrc + '\nreturn escapeHtml;'))();
    const localEsc = escSrc ? (new Function(escSrc + '\nreturn ' + escName + ';'))() : escapeHtml;
    const fn = new Function(...argNames, 'escapeHtml', 'truncate', escName || '__unusedEsc',
      'return (' + renderExpr + ');');
    const html = fn(...args, escapeHtml, truncate, localEsc);
    const host = document.getElementById('host');
    // A standalone <td> is foster-parented (dropped) unless it sits inside a
    // table, so table-cell sinks must be re-homed before the parser runs.
    host.innerHTML = wrap === 'table'
      ? '<table><tbody><tr>' + html + '</tr></tbody></table>'
      : html;
    const dangerous = host.querySelectorAll('img, script, iframe, svg');
    const handlerEls = Array.from(host.querySelectorAll('*')).filter(
      (el) => el.getAttributeNames().some((a) => /^on/i.test(a)));
    const dataValueEl = host.querySelector('[data-value]');
    const out = {
      html,
      dangerousCount: dangerous.length,
      handlerCount: handlerEls.length,
      sideEffect: window.__xss333,
      text: host.textContent,
      dataValue: dataValueEl ? dataValueEl.getAttribute('data-value') : null,
      hasRegionBadge: !!host.querySelector('.badge-region'),
    };
    host.innerHTML = '';
    return out;
  }, { appEscSrc: escapeHtmlSrc(), renderExpr, argNames, args, wrap, escName, escSrc });
}

function assertInert(r, label) {
  if (r.dangerousCount !== 0)
    throw new Error(`${label}: ${r.dangerousCount} markup element(s) injected — escape failed: ${r.html}`);
  if (r.handlerCount !== 0)
    throw new Error(`${label}: ${r.handlerCount} element(s) carry an on* handler attribute: ${r.html}`);
  if (r.sideEffect !== undefined)
    throw new Error(`${label}: an injected handler fired (window.__xss333 set): ${r.html}`);
}

(async () => {
  const { server, port } = await startServer();
  const baseUrl = `http://127.0.0.1:${port}/`;
  const browser = await chromium.launch({ headless: true, args: ['--no-sandbox'] });
  const context = await browser.newContext();
  const page = await context.newPage();
  await page.goto(baseUrl);

  // 1. Unknown-route heading — route comes from the URL hash.
  await test('unknown route: hostile hash renders as inert text, no markup', async () => {
    const tpl = liftTemplate('app.js', /(`<div style="[^`]*?Page not yet implemented[^`]*?<\/div>`)/);
    const r = await renderAndInspect(page, {
      renderExpr: tpl, argNames: ['route'], args: ['#/' + PAYLOAD],
    });
    assertInert(r, 'unknown route');
    if (!r.text.includes(PAYLOAD))
      throw new Error('unknown route: payload not shown as literal text: ' + r.text);
  });

  await test('unknown route: benign route name displays verbatim', async () => {
    const tpl = liftTemplate('app.js', /(`<div style="[^`]*?Page not yet implemented[^`]*?<\/div>`)/);
    const r = await renderAndInspect(page, {
      renderExpr: tpl, argNames: ['route'], args: ['#/' + BENIGN],
    });
    assertInert(r, 'unknown route benign');
    if (!r.text.includes(BENIGN))
      throw new Error('unknown route: benign name mangled: ' + r.text);
  });

  // 2. Observer region — iata in the observers-table region cell (attr +
  //    badge text). Also confirms the data-value sort key survives.
  await test('observer region: hostile iata renders inert, sort key preserved', async () => {
    const tpl = liftTemplate('observers.js',
      OBS_REGION_CELL);
    const r = await renderAndInspect(page, {
      renderExpr: '`' + tpl + '`', argNames: ['o'], args: [{ iata: PAYLOAD }], wrap: 'table',
    });
    assertInert(r, 'observer region');
    if (r.dataValue !== PAYLOAD)
      throw new Error('observer region: data-value sort key not preserved: ' + r.dataValue);
    if (!r.hasRegionBadge)
      throw new Error('observer region: badge-region span missing (display regressed)');
  });

  await test('observer region: benign iata displays verbatim in the badge', async () => {
    const tpl = liftTemplate('observers.js',
      OBS_REGION_CELL);
    const r = await renderAndInspect(page, {
      renderExpr: '`' + tpl + '`', argNames: ['o'], args: [{ iata: 'KÖLN & Co' }], wrap: 'table',
    });
    assertInert(r, 'observer region benign');
    if (!r.text.includes('KÖLN & Co'))
      throw new Error('observer region: benign iata mangled: ' + r.text);
  });

  // 3. Packet region badge — groupRegion from the observer map iata.
  await test('packet region badge: hostile region renders inert', async () => {
    const tpl = liftTemplate('packets.js',
      PKT_REGION_CELL);
    const r = await renderAndInspect(page, {
      renderExpr: '`' + tpl + '`', argNames: ['groupRegion'], args: [PAYLOAD], wrap: 'table',
    });
    assertInert(r, 'packet region badge');
    if (!r.hasRegionBadge)
      throw new Error('packet region badge: badge-region span missing');
  });

  // 3b. Geo-filter prune preview (customize-v2.js) — rendered with the
  //     file's own textContent-based esc(), serialized by the real parser.
  await test('geo-filter prune preview: hostile node name renders inert with customize-v2 esc()', async () => {
    const src = fs.readFileSync(path.join(__dirname, 'public', 'customize-v2.js'), 'utf8');
    const escFn = src.match(/\n\s*(function esc\(s\) \{[^\n]*\})/);
    if (!escFn) throw new Error('could not lift esc() from public/customize-v2.js');
    const expr = liftTemplate('customize-v2.js', /return '<div>'\s*\+\s*([\s\S]*?)\s*\+\s*coords\s*\+\s*'<\/div>';/);
    for (const name of [PAYLOAD, BENIGN]) {
      const r = await renderAndInspect(page, {
        renderExpr: "'<div>' + (" + expr + ") + '</div>'", argNames: ['n'],
        args: [{ name, pubkey: 'abcdef012345' }], escName: 'esc', escSrc: escFn[1],
      });
      assertInert(r, 'geo-filter prune preview');
      if (r.text !== name)
        throw new Error('geo-filter prune preview: name not shown verbatim: ' + JSON.stringify(r.text));
    }
  });

  // 4. Favorites dropdown — node name in the fav-dd-name span.
  await test('favorites dropdown: hostile node name renders inert', async () => {
    const expr = liftTemplate('app.js',
      /<span class="fav-dd-name">'\s*\+\s*([\s\S]*?)\s*\+\s*'<\/span>'/);
    const renderExpr = "'<span class=\"fav-dd-name\">' + (" + expr + ") + '</span>'";
    const r = await renderAndInspect(page, {
      renderExpr, argNames: ['h', 'pk'],
      args: [{ node: { name: PAYLOAD } }, 'abcdef0123456789'],
    });
    assertInert(r, 'favorites dropdown');
    if (!r.text.includes(PAYLOAD))
      throw new Error('favorites dropdown: payload not shown as literal text: ' + r.text);
  });

  await test('favorites dropdown: benign node name displays verbatim', async () => {
    const expr = liftTemplate('app.js',
      /<span class="fav-dd-name">'\s*\+\s*([\s\S]*?)\s*\+\s*'<\/span>'/);
    const renderExpr = "'<span class=\"fav-dd-name\">' + (" + expr + ") + '</span>'";
    const r = await renderAndInspect(page, {
      renderExpr, argNames: ['h', 'pk'],
      args: [{ node: { name: BENIGN } }, 'abcdef0123456789'],
    });
    assertInert(r, 'favorites dropdown benign');
    if (r.text !== BENIGN)
      throw new Error('favorites dropdown: benign name mangled: ' + JSON.stringify(r.text));
  });

  await context.close();
  await browser.close();
  await new Promise((resolve) => server.close(resolve));

  console.log('\n' + passed + ' passed, ' + failed + ' failed');
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
