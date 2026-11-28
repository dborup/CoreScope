/**
 * test-issue-429-config-url-schemes.js
 *
 * Issue #429 — #420 (#368) gave miniMarkdown() link destinations a scheme
 * allowlist, but the same Home customization source still had an href sink
 * with no scheme check: footerLinks[].url in public/home.js, rendered as
 * href="${escapeAttr(l.url)}". escapeAttr only covers the two quote
 * characters, so `javascript:` went straight through, and so did an
 * entity-encoded scheme (`&#106;avascript:`) that the browser decodes back
 * into `javascript:` when it parses the attribute.
 *
 * The other URL sinks fed by importable customization are the branding
 * logo / favicon setters in public/customize-v2.js. They now go through the
 * same check.
 *
 * These tests drive the REAL helpers from public/roles.js in a vm sandbox
 * and pin, at source level, that the sinks use them instead of growing a
 * second allowlist. The browser-level counterpart (what the Home page and
 * the navbar actually render and execute) is test-xss-429-home-footer-e2e.js.
 *
 * Mutants this file must catch (verified by hand, see the PR):
 *   N1  safeUrlAttr() stops escaping '&'                     -> B2 red
 *   N2  isSafeLinkUrl() drops the control/whitespace strip   -> B1 red
 *   N3  isSafeLinkUrl() accepts every scheme                 -> B1, B3 red
 *   N4  home.js footer goes back to escapeAttr(l.url)        -> B4 red
 *   N5  a .brand-icon innerHTML logo sink comes back         -> B5 red
 *   N9  isSafeImageUrl() accepts any data: URL              -> B6 red
 */
'use strict';
const vm = require('vm');
const fs = require('fs');
const assert = require('assert');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

// --- Load public/roles.js the way index.html does -----------------------
function loadRoles() {
  const ctx = {
    window: { addEventListener: () => {}, dispatchEvent: () => {} },
    document: {
      readyState: 'complete',
      createElement: () => ({ id: '', textContent: '', innerHTML: '', style: {} }),
      head: { appendChild: () => {} },
      getElementById: () => null,
      addEventListener: () => {},
      querySelectorAll: () => [],
      querySelector: () => null,
    },
    console, Date, Math, JSON, RegExp, Error, TypeError,
    parseInt, parseFloat, isNaN, isFinite,
    encodeURIComponent, decodeURIComponent,
    setTimeout: () => {}, clearTimeout: () => {},
    localStorage: { getItem: () => null, setItem: () => {}, removeItem: () => {} },
    location: { hash: '' },
    navigator: {},
    fetch: () => Promise.resolve({ json: () => Promise.resolve({}) }),
  };
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync('public/roles.js', 'utf8'), ctx);
  return ctx.window;
}
const W = loadRoles();
const isSafe = W.isSafeLinkUrl;
const safeAttr = W.safeUrlAttr;

// Hostile URLs, raw (as they would sit in a customization JSON). Every one
// carries a scheme that is not http/https once case and the ASCII
// controls/whitespace a browser ignores are taken out.
const HOSTILE = [
  ['plain javascript:', 'javascript:alert(1)'],
  ['mixed case', 'JaVaScRiPt:alert(1)'],
  ['upper case', 'JAVASCRIPT:alert(1)'],
  ['leading spaces', '   javascript:alert(1)'],
  ['leading TAB/LF', '\t\njavascript:alert(1)'],
  ['leading C0 control', '\u0001javascript:alert(1)'],
  ['embedded TAB', 'java\tscript:alert(1)'],
  ['embedded LF', 'java\nscript:alert(1)'],
  ['embedded CR', 'java\rscript:alert(1)'],
  ['embedded NUL', 'java\u0000script:alert(1)'],
  ['embedded DEL', 'java\u007fscript:alert(1)'],
  ['space before colon', 'javascript :alert(1)'],
  ['data: html', 'data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg=='],
  ['data: svg image', 'data:image/svg+xml,<svg onload=alert(1)>'],
  ['vbscript:', 'vbscript:msgbox(1)'],
  ['file:', 'file:///etc/passwd'],
  ['mailto: (not on the allowlist, see #368)', 'mailto:someone@example.org'],
];
const EMPTYISH = [
  ['empty', ''],
  ['whitespace only', ' \t\n'],
  ['null', null],
  ['undefined', undefined],
];
// Safe URLs and what safeUrlAttr() must turn them into (fully escaped for a
// double- or single-quoted attribute: & < > " ').
const SAFE = [
  ['https', 'https://meshcore.example/docs', 'https://meshcore.example/docs'],
  ['http', 'http://meshcore.example/docs', 'http://meshcore.example/docs'],
  ['mixed-case scheme', 'HTTPS://meshcore.example/', 'HTTPS://meshcore.example/'],
  ['hash route', '#/packets', '#/packets'],
  ['root-relative', '/nodes', '/nodes'],
  ['relative', 'docs/x', 'docs/x'],
  ['query only', '?a=1', '?a=1'],
  ['colon after the first path segment', '/nodes/a:b', '/nodes/a:b'],
  ['query with &', 'https://m.example/?a=1&b=2', 'https://m.example/?a=1&amp;b=2'],
];
// No-scheme strings that a browser would turn INTO a scheme if '&' were left
// alone in the attribute: the entity is decoded while the attribute is
// parsed. They stay (relative) links, but the '&' must come out as &amp; so
// the browser sees the literal text.
const ENTITY_OBFUSCATED = [
  ['decimal entity', '&#106;avascript:alert(1)', '&amp;#106;avascript:alert(1)'],
  ['hex entity', '&#x6A;avascript:alert(1)', '&amp;#x6A;avascript:alert(1)'],
  ['named &colon;', 'javascript&colon;alert(1)', 'javascript&amp;colon;alert(1)'],
  ['entity TAB inside', 'jav&#x09;ascript:alert(1)', 'jav&amp;#x09;ascript:alert(1)'],
];
const QUOTE_BREAKOUT = [
  ['double-quote breakout', '" onmouseover="alert(1)', '&quot; onmouseover=&quot;alert(1)'],
  ['single-quote breakout', "/a' onmouseover='alert(1)", '/a&#39; onmouseover=&#39;alert(1)'],
  ['tag breakout', '"><img src=x onerror=alert(1)>', '&quot;&gt;&lt;img src=x onerror=alert(1)&gt;'],
];

// ===== B1 — isSafeLinkUrl: the shared allowlist predicate ===============
console.log('\n=== #429 B1: window.isSafeLinkUrl (shared scheme check) ===');
{
  test('roles.js exports window.isSafeLinkUrl', () => {
    assert.strictEqual(typeof isSafe, 'function');
  });
  for (const [name, url] of HOSTILE) {
    test('rejects ' + name, () => assert.strictEqual(isSafe(url), false, JSON.stringify(url)));
  }
  for (const [name, url] of EMPTYISH) {
    test('rejects ' + name, () => assert.strictEqual(isSafe(url), false, JSON.stringify(url)));
  }
  for (const [name, url] of SAFE) {
    test('accepts ' + name, () => assert.strictEqual(isSafe(url), true, JSON.stringify(url)));
  }
}

// ===== B2 — safeUrlAttr: raw URL -> attribute value, or '' ==============
console.log('\n=== #429 B2: window.safeUrlAttr (raw URL to attribute value) ===');
{
  test('roles.js exports window.safeUrlAttr', () => {
    assert.strictEqual(typeof safeAttr, 'function');
  });
  for (const [name, url] of HOSTILE.concat(EMPTYISH)) {
    test('returns "" for ' + name, () => assert.strictEqual(safeAttr(url), '', JSON.stringify(url)));
  }
  for (const [name, url, want] of SAFE) {
    test('keeps ' + name, () => assert.strictEqual(safeAttr(url), want));
  }
  for (const [name, url, want] of ENTITY_OBFUSCATED) {
    test('entity-obfuscated scheme stays literal: ' + name, () => {
      assert.strictEqual(safeAttr(url), want);
    });
  }
  for (const [name, url, want] of QUOTE_BREAKOUT) {
    test('cannot leave the attribute: ' + name, () => {
      const out = safeAttr(url);
      assert.strictEqual(out, want);
      assert(!/["'<>]/.test(out), 'raw quote or bracket survived: ' + out);
    });
  }
  test('& is escaped exactly once', () => {
    assert.strictEqual(safeAttr('/a?x=1&amp;y=2'), '/a?x=1&amp;amp;y=2');
  });
}

// ===== B3 — one allowlist: miniMarkdown and the raw-URL path agree ======
console.log('\n=== #429 B3: miniMarkdown and safeUrlAttr share one allowlist ===');
{
  // Parentheses dropped so miniMarkdown's [^)]+ captures the whole URL.
  const MATRIX = HOSTILE.concat(SAFE).map(r => r[1].replace(/[()]/g, ''));
  test('the agreement matrix covers every hostile and safe URL', () => {
    assert.strictEqual(MATRIX.length, HOSTILE.length + SAFE.length);
  });
  for (const url of MATRIX) {
    test('same verdict for ' + JSON.stringify(url), () => {
      const linked = /<a\b/.test(W.miniMarkdown('[x](' + url + ')'));
      assert.strictEqual(linked, isSafe(url), 'miniMarkdown linked=' + linked + ', isSafeLinkUrl=' + isSafe(url));
    });
  }
  test('roles.js declares the scheme allowlist exactly once', () => {
    const src = fs.readFileSync('public/roles.js', 'utf8');
    assert.strictEqual((src.match(/SAFE_LINK_SCHEMES\s*=/g) || []).length, 1);
  });
}

// ===== B6 — isSafeImageUrl: the image-sink form of the same check =======
console.log('\n=== #429 B6: window.isSafeImageUrl (logo / favicon sinks) ===');
{
  const isImg = W.isSafeImageUrl;
  test('roles.js exports window.isSafeImageUrl', () => assert.strictEqual(typeof isImg, 'function'));
  // Everything the link check accepts, the image check accepts too.
  for (const [name, url] of SAFE) {
    test('accepts ' + name, () => assert.strictEqual(isImg(url), true, JSON.stringify(url)));
  }
  // Inline images stay possible (the customizer has always taken them),
  // after the same normalisation.
  for (const url of ['data:image/png;base64,iVBORw0KGgo=', 'data:image/svg+xml;base64,PHN2Zy8+', ' DATA:Image/png;base64,AA', 'da\tta:image/gif;base64,R0lG']) {
    test('accepts inline image ' + JSON.stringify(url), () => assert.strictEqual(isImg(url), true));
  }
  // Every other hostile URL is still rejected, data:text/html included.
  for (const [name, url] of HOSTILE.concat(EMPTYISH)) {
    if (/^data:image\//.test(url || '')) continue;
    test('rejects ' + name, () => assert.strictEqual(isImg(url), false, JSON.stringify(url)));
  }
  test('rejects a data: URL that only mentions image/', () => {
    assert.strictEqual(isImg('data:text/html,image/'), false);
    assert.strictEqual(isImg('data:,image/png'), false);
  });
}

// ===== B4 — the Home footer sink uses the shared helper =================
console.log('\n=== #429 B4: home.js footerLinks go through safeUrlAttr ===');
{
  const home = fs.readFileSync('public/home.js', 'utf8');
  test('no footer href is built from escapeAttr(l.url) any more', () => {
    assert(!/href="\$\{escapeAttr\(l\.url\)\}"/.test(home), 'raw escapeAttr(l.url) href is back');
  });
  test('footer links are rendered through window.safeUrlAttr', () => {
    const fn = /function footerLinkHtml\(l\) \{([\s\S]*?)\n  \}/.exec(home);
    assert(fn, 'footerLinkHtml(l) not found in public/home.js');
    assert(/safeUrlAttr\(l\.url\)/.test(fn[1]), 'footerLinkHtml does not call safeUrlAttr(l.url)');
    assert(/homeCfg\.footerLinks\.map\(footerLinkHtml\)/.test(home), 'footerLinks are not mapped through footerLinkHtml');
  });
  test('home.js grows no scheme allowlist of its own', () => {
    assert(!/javascript:|SAFE_LINK_SCHEMES|https\?:/i.test(home.replace(/\/\/[^\n]*|\/\*[\s\S]*?\*\//g, '')),
      'public/home.js contains its own scheme check');
  });
}

// ===== B5 — branding URL sinks in customize-v2.js =======================
console.log('\n=== #429 B5: customize-v2.js branding URL sinks ===');
{
  const cv2 = fs.readFileSync('public/customize-v2.js', 'utf8');
  const code = cv2.replace(/\/\/[^\n]*|\/\*[\s\S]*?\*\//g, '');
  test('no innerHTML <img src> is built from a logo URL', () => {
    const hits = code.split('\n').filter(l => /innerHTML\s*=/.test(l) && /<img src="'\s*\+/.test(l));
    assert.strictEqual(hits.length, 0, 'innerHTML logo sinks: ' + JSON.stringify(hits.map(s => s.trim())));
  });
  test('_setBrandLogoUrl checks the URL with the shared predicate', () => {
    const fn = /function _setBrandLogoUrl\(url, alt\) \{([\s\S]*?)\n  \}/.exec(cv2);
    assert(fn, '_setBrandLogoUrl not found');
    assert(/_brandUrlOk\(url\)/.test(fn[1]), '_setBrandLogoUrl does not call _brandUrlOk(url)');
  });
  test('_brandUrlOk delegates to window.isSafeImageUrl and fails closed', () => {
    const fn = /function _brandUrlOk\(url\) \{([\s\S]*?)\n  \}/.exec(cv2);
    assert(fn, '_brandUrlOk not found');
    assert(/typeof window\.isSafeImageUrl === 'function' && window\.isSafeImageUrl\(url\)/.test(fn[1]), fn[1]);
  });
  test('every favicon write goes through _setFaviconUrl', () => {
    const setter = /function _setFaviconUrl\(url\) \{([\s\S]*?)\n  \}/.exec(code);
    assert(setter, '_setFaviconUrl not found');
    assert(/_brandUrlOk\(url\)/.test(setter[1]), '_setFaviconUrl does not call _brandUrlOk(url)');
    const rest = code.replace(setter[0], '');
    // The download link (a.href = URL.createObjectURL(...)) is the only other
    // .href write in the file and is not customization-fed.
    const direct = rest.split('\n').filter(l => /\.href\s*=/.test(l) && !/createObjectURL/.test(l));
    assert.strictEqual(direct.length, 0, 'direct .href writes: ' + JSON.stringify(direct.map(s => s.trim())));
  });
  test('the customizer logo preview is gated by the same check', () => {
    const line = cv2.split('\n').find(l => /<img class="cust-preview-img"/.test(l));
    assert(line, 'logo preview line not found');
    assert(/b\.logoUrl && _brandUrlOk\(b\.logoUrl\)/.test(line), line.trim());
  });
}

console.log('\n' + passed + ' passed, ' + failed + ' failed');
process.exit(failed ? 1 : 0);
