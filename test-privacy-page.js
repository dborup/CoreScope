#!/usr/bin/env node
/* test-privacy-page.js — unit tests for the opt-in #/privacy page
   (public/privacy.js), run in a VM sandbox like test-frontend-helpers.js.
   escapeHtml is extracted from public/app.js source so these assertions
   exercise the REAL escaping behavior, not a test-local copy. */
'use strict';
const vm = require('vm');
const fs = require('fs');
const assert = require('assert');

let passed = 0, failed = 0;
async function test(name, fn) {
  try {
    await fn();
    passed++;
    console.log(`  ✅ ${name}`);
  } catch (e) {
    failed++;
    console.log(`  ❌ ${name}: ${e.message}`);
  }
}

// --- Extract the canonical escapeHtml from app.js (#1536 5-char OWASP set) ---
const appSrc = fs.readFileSync('public/app.js', 'utf8');
const escMatch = appSrc.match(/function escapeHtml\(s\) \{[\s\S]*?\n\}/);
assert(escMatch, 'could not extract escapeHtml from public/app.js');

// --- Sandbox: load privacy.js with a pages registry + window stub ---
function makeSandbox(mcPrivacy) {
  const pages = {};
  const ctx = {
    window: { MC_PRIVACY: mcPrivacy },
    document: {},
    console, String, Object, Array, Promise, JSON, Error,
    registerPage: (name, mod) => { pages[name] = mod; },
  };
  ctx.globalThis = ctx;
  vm.createContext(ctx);
  vm.runInContext(escMatch[0] + '\nthis.escapeHtml = escapeHtml;', ctx);
  vm.runInContext(fs.readFileSync('public/privacy.js', 'utf8'), ctx);
  return { ctx, pages };
}

// init() gates on window.MeshConfigReady when present; absent here, so it
// falls through to Promise.resolve() and renders synchronously after await.
async function renderWith(mcPrivacy) {
  const { pages } = makeSandbox(mcPrivacy);
  assert(pages.privacy, 'privacy page not registered');
  const container = { innerHTML: '' };
  await pages.privacy.init(container);
  return container.innerHTML;
}

(async () => {
  console.log('test-privacy-page.js');

  await test('registers privacy page with init + destroy', async () => {
    const { pages } = makeSandbox(null);
    assert.equal(typeof pages.privacy.init, 'function');
    assert.equal(typeof pages.privacy.destroy, 'function');
  });

  await test('absent config renders the not-published state', async () => {
    const html = await renderWith(null);
    assert(html.includes('has not published a privacy notice'), 'expected disabled state');
  });

  await test('enabled:false renders the not-published state (stale-cache guard)', async () => {
    const html = await renderWith({ enabled: false, operatorName: 'X' });
    assert(html.includes('has not published a privacy notice'), 'expected disabled state');
  });

  await test('renders operator, mailto contact and retention override', async () => {
    const html = await renderWith({
      enabled: true,
      operatorName: 'Example Mesh Community',
      contactEmail: 'privacy@example.org',
      retentionText: 'Packet data is deleted after 30 days.',
    });
    assert(html.includes('Example Mesh Community'), 'operator name missing');
    assert(html.includes('href="mailto:privacy@example.org"'), 'mailto link missing');
    assert(html.includes('Packet data is deleted after 30 days.'), 'retention override missing');
    assert(!html.includes('historical archive'), 'default retention should be replaced by override');
  });

  await test('empty operatorName falls back to neutral wording', async () => {
    const html = await renderWith({ enabled: true, contactEmail: '' });
    assert(html.includes('The operator of this site'), 'neutral operator fallback missing');
    assert(!html.includes('mailto:'), 'no mailto without contactEmail');
  });

  await test('empty retentionText falls back to default paragraph', async () => {
    const html = await renderWith({ enabled: true });
    assert(html.includes('historical archive'), 'default retention paragraph missing');
  });

  await test('config values are HTML-escaped (XSS)', async () => {
    const html = await renderWith({
      enabled: true,
      operatorName: '<img src=x onerror=alert(1)>',
      contactEmail: '"><script>alert(2)</script>',
      retentionText: '<b onmouseover=alert(3)>bold</b>',
    });
    assert(!html.includes('<img'), 'unescaped <img in output');
    assert(!html.includes('<script'), 'unescaped <script in output');
    assert(!html.includes('<b '), 'unescaped <b in output');
    assert(html.includes('&lt;img src=x onerror=alert(1)&gt;'), 'escaped operator name missing');
    // The quote in contactEmail must be escaped so it cannot break out of
    // the mailto href attribute (escapeHtml covers quotes since #1536).
    assert(!html.includes('href="mailto:"><'), 'contactEmail broke out of href attribute');
  });

  await test('mentions the default hidden-name prefix character', async () => {
    const html = await renderWith({ enabled: true });
    assert(html.includes(String.fromCodePoint(0x1F6AB)), 'hidden-prefix character missing');
  });

  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
