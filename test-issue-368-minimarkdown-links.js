/**
 * test-issue-368-minimarkdown-links.js
 *
 * Issue #368 — miniMarkdown() (public/roles.js) built its anchors with
 *   .replace(/\[([^\]]+)\]\(([^)]+)\)/g,
 *            '<a href="$2" target="_blank" ...>$1</a>')
 * The surrounding text had already been & < > escaped, but the captured
 * destination went into a double-quoted href with neither attribute escaping
 * nor any restriction on the URL scheme. Home checklist/step text is
 * site-customization content (importable, therefore untrusted) and is fed to
 * miniMarkdown() by public/home.js, so a crafted destination could add its
 * own attributes or use an executable scheme.
 *
 * These tests drive the REAL miniMarkdown from public/roles.js in a vm
 * sandbox and assert on the produced HTML string. The browser-level
 * counterpart (does anything actually execute on the Home page) is
 * test-xss-368-home-links-e2e.js.
 *
 * Mutants this file must catch (verified by hand, see the PR):
 *   M1  drop the escapeAttr() on the destination   -> A1 red
 *   M2  drop the control/whitespace stripping      -> A2 red
 *   M3  accept every scheme                        -> A2 red
 *   M4  go back to the '$2' replacement template   -> A1 red
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
const md = W.miniMarkdown;
assert(typeof md === 'function', 'public/roles.js did not export window.miniMarkdown');

// Names of the attributes a browser would see on the first anchor. Parsing
// the tag (rather than string-matching the payload) is what makes the
// quote-breakout assertions payload-independent.
function anchorAttrs(html) {
  const m = /<a\b([^>]*)>/i.exec(html);
  if (!m) return null;
  const names = [];
  const re = /([a-zA-Z_:][-a-zA-Z0-9_:.]*)\s*=\s*("([^"]*)"|'([^']*)'|[^\s>]+)/g;
  let a;
  while ((a = re.exec(m[1])) !== null) names.push(a[1].toLowerCase());
  return names;
}
const EXPECTED_ATTRS = ['href', 'target', 'rel', 'style'];

// ===== A1 — a hostile destination cannot create attributes ==============
console.log('\n=== #368 A1: destination cannot break out of href ===');
{
  // Each payload tries to close the href early and add an event handler.
  // None contains ')': miniMarkdown's destination group is [^)]+.
  const BREAKOUTS = [
    ['double quote + event handler', '" onmouseover="alert1'],
    ['double quote + autofocus/onfocus', '" autofocus onfocus="alert1'],
    ['single quote probe', "' onmouseover='alert1"],
    ['quote, handler, and a second attribute', '" onerror="alert1" x="'],
  ];
  for (const [label, dest] of BREAKOUTS) {
    test('no extra attribute for ' + label, () => {
      const html = md('[click](' + dest + ')');
      const attrs = anchorAttrs(html);
      assert(attrs !== null, 'expected an anchor, got: ' + html);
      assert.deepStrictEqual(attrs, EXPECTED_ATTRS,
        'destination created extra attributes: ' + html);
      // attrs is the parsed attribute-NAME list, so an `onerror=` that only
      // appears inside the (escaped) href value does not count.
      assert(!attrs.some(a => /^on/.test(a) || a === 'autofocus'),
        'destination created an executable attribute: ' + html);
    });
  }

  test('raw double quote is entity-encoded inside href', () => {
    const html = md('[click](" onmouseover="alert1)');
    assert(html.includes('&quot;'), 'missing &quot; marker: ' + html);
  });

  test('raw single quote is entity-encoded inside href', () => {
    const html = md("[click](/a' onmouseover='alert1)");
    assert(html.includes('&#39;'), 'missing &#39; marker: ' + html);
    assert(!/'\s*onmouseover\s*=/i.test(html), "raw ' onmouseover= survived: " + html);
  });

  // The old '$2' replacement template re-expanded $-sequences found in the
  // destination; the replacement function does not.
  test("a $' in the destination is not re-expanded", () => {
    const html = md("[click](/a$'b)");
    assert(html.includes('href="/a$&#39;b"'), 'destination was rewritten: ' + html);
  });
  test('a $& in the destination is not re-expanded', () => {
    const html = md('[click](/a$&b)');
    assert(html.includes('href="/a$&amp;b"'), 'destination was rewritten: ' + html);
  });
}

// ===== A2 — unsafe schemes are rejected, including obfuscated ones ======
console.log('\n=== #368 A2: scheme allowlist after normalisation ===');
{
  const REJECTED = [
    ['plain javascript:', 'javascript:alert1'],
    ['mixed case', 'JaVaScRiPt:alert1'],
    ['uppercase', 'JAVASCRIPT:alert1'],
    ['leading spaces', '   javascript:alert1'],
    ['embedded tab', 'java\tscript:alert1'],
    ['embedded newline', 'java\nscript:alert1'],
    ['embedded carriage return', 'java\rscript:alert1'],
    ['embedded NUL', 'java\u0000script:alert1'],
    ['embedded vertical tab', 'java\u000bscript:alert1'],
    ['embedded form feed', 'java\u000cscript:alert1'],
    ['embedded DEL', 'java\u007fscript:alert1'],
    ['leading control byte', '\u0001javascript:alert1'],
    ['space before the colon', 'javascript :alert1'],
    ['data: html', 'data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg=='],
    ['vbscript:', 'vbscript:msgbox1'],
    ['file:', 'file:///etc/passwd'],
    ['mailto: (not in the allowlist today)', 'mailto:a@b.example'],
    ['whitespace only', '   '],
    ['controls only', '\u0001\u0002'],
  ];
  for (const [label, dest] of REJECTED) {
    test('rejects ' + label, () => {
      const html = md('[click me](' + dest + ')');
      assert(!/<a\b/i.test(html), 'an anchor was rendered: ' + JSON.stringify(html));
      assert(html.includes('click me'), 'label text was lost: ' + JSON.stringify(html));
      assert(!/javascript|vbscript|data:text\/html|file:|mailto/i.test(html),
        'the rejected destination leaked into the output: ' + JSON.stringify(html));
    });
  }

  test('a rejected destination inside a sentence keeps the rest of the text', () => {
    assert.strictEqual(md('before [click me](javascript:alert1) after'),
      'before click me after');
  });

  test('a rejected destination still has its label HTML-escaped', () => {
    const html = md('[<img src=x onerror=alert1>](javascript:alert1)');
    assert(!/<img/i.test(html), 'raw <img survived in the fallback label: ' + html);
    assert(html.includes('&lt;img'), 'missing &lt;img marker: ' + html);
  });
}

// ===== A3 — safe links keep working ====================================
console.log('\n=== #368 A3: safe destinations still render as links ===');
{
  const ACCEPTED = [
    ['https', 'https://meshcore.example/docs'],
    ['http', 'http://meshcore.example/docs'],
    ['https with a mixed-case scheme', 'HTTPS://meshcore.example/docs'],
    ['root-relative path', '/nodes'],
    ['hash route', '#/map'],
    ['relative path', 'docs/getting-started'],
    ['dot-relative path', './docs'],
    ['query only', '?region=SJC'],
    ['path with a colon after the first segment', '/nodes/a:b'],
  ];
  for (const [label, dest] of ACCEPTED) {
    test('accepts ' + label, () => {
      const html = md('[Docs](' + dest + ')');
      const attrs = anchorAttrs(html);
      assert(attrs !== null, 'expected an anchor for ' + dest + ', got: ' + html);
      assert.deepStrictEqual(attrs, EXPECTED_ATTRS, 'unexpected attributes: ' + html);
      assert(html.includes('href="' + dest + '"'), 'destination was altered: ' + html);
      assert(html.includes('>Docs</a>'), 'label was altered: ' + html);
    });
  }

  test('an & in a safe destination stays entity-escaped exactly once', () => {
    const html = md('[Docs](https://meshcore.example/?a=1&b=2)');
    assert(html.includes('href="https://meshcore.example/?a=1&amp;b=2"'), html);
    assert(!html.includes('&amp;amp;'), 'double-escaped &: ' + html);
  });

  test('target/rel hardening is still present on an accepted link', () => {
    const html = md('[Docs](https://meshcore.example/)');
    assert(html.includes('target="_blank"'), html);
    assert(html.includes('rel="noopener"'), html);
    assert(html.includes('style="color:var(--link-color)"'), html);
  });
}

// ===== A4 — the rest of miniMarkdown is unchanged =======================
console.log('\n=== #368 A4: non-link Markdown behaviour unchanged ===');
{
  test('empty input', () => assert.strictEqual(md(''), ''));
  test('null input', () => assert.strictEqual(md(null), ''));
  test('bold', () => assert.strictEqual(md('a **b** c'), 'a <strong>b</strong> c'));
  test('italic', () => assert.strictEqual(md('a *b* c'), 'a <em>b</em> c'));
  test('code', () => assert.strictEqual(md('a `b` c'), 'a <code>b</code> c'));
  test('angle brackets escaped', () => assert.strictEqual(md('<b>'), '&lt;b&gt;'));
  test('ampersand escaped', () => assert.strictEqual(md('a & b'), 'a &amp; b'));
  test('line break', () => assert.strictEqual(md('a\nb'), 'a<br>b'));
  test('list wrapping', () => {
    assert.strictEqual(md('- one\n- two\n'), '<ul><li>one</li><li>two</li></ul>');
  });
  test('a link inside a list item still renders', () => {
    const html = md('- see [Docs](https://meshcore.example/)\n');
    assert(html.startsWith('<ul><li>see <a href="https://meshcore.example/"'), html);
  });
  test('two links in one line are handled independently', () => {
    const html = md('[ok](https://a.example/) and [no](javascript:void0)');
    assert(html.includes('<a href="https://a.example/"'), html);
    assert.strictEqual((html.match(/<a\b/g) || []).length, 1,
      'expected exactly one anchor: ' + html);
    assert(html.endsWith(' and no'), html);
  });
}

// ===== A5 — public/home.js reuses the exported escapeAttr ===============
console.log('\n=== #368 A5: home.js escapeAttr delegates to the shared helper ===');
{
  test('roles.js exports window.escapeAttr', () => {
    assert.strictEqual(typeof W.escapeAttr, 'function');
    assert.strictEqual(W.escapeAttr('a"b\'c'), 'a&quot;b&#39;c');
  });
  test('home.js has no second escapeAttr implementation', () => {
    const src = fs.readFileSync('public/home.js', 'utf8');
    const m = /function escapeAttr\(s\) \{([^\n]*)\n/.exec(src);
    assert(m, 'escapeAttr not found in public/home.js');
    assert(/return window\.escapeAttr\(s\);/.test(m[1]),
      'public/home.js defines its own escapeAttr body again: ' + m[1].trim());
  });
}

console.log('\n' + passed + ' passed, ' + failed + ' failed');
process.exit(failed ? 1 : 0);
