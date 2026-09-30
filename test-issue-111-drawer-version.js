/* test-issue-111-drawer-version.js — running version in the nav-drawer footer (#111).
 *
 * Loads the real public/nav-drawer.js in a vm with a small fake DOM and a
 * counting fetch stub. Checks: footer label + fork releases link; no
 * /api/health request at page load or while the drawer is gated off
 * (narrow viewport); one request on the first wide open and none on
 * re-open; success fills "CoreScope <version>" and a commit/build tooltip;
 * rejected, non-OK, invalid-JSON, version-less and "unknown" responses keep
 * the neutral "CoreScope"; remote values only ever go through textContent.
 */
'use strict';
const vm = require('vm');
const fs = require('fs');
const assert = require('assert');

console.log('--- test-issue-111-drawer-version.js ---');
let passed = 0, failed = 0;
const pending = [];
function test(name, fn) {
  pending.push(async () => {
    try { await fn(); passed++; console.log('  ✅ ' + name); }
    catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
  });
}

const SRC = fs.readFileSync(__dirname + '/public/nav-drawer.js', 'utf8');
const RELEASES = 'https://github.com/dborup/CoreScope/releases';

// ── minimal DOM ───────────────────────────────────────────────────────────
function makeDoc() {
  let htmlWrites = 0;
  function El(tag) {
    this.tagName = String(tag).toUpperCase();
    this.children = [];
    this.parentNode = null;
    this.attrs = {};
    this.style = {};
    this.dataset = {};
    this._text = '';
    this._html = '';
    this.hidden = false;
    this.title = '';
    this.className = '';
    const self = this;
    this.classList = {
      add(c) { const s = new Set(self.className.split(/\s+/).filter(Boolean)); s.add(c); self.className = [...s].join(' '); },
      remove(c) { self.className = self.className.split(/\s+/).filter((x) => x && x !== c).join(' '); },
      contains(c) { return self.className.split(/\s+/).includes(c); },
      toggle(c) { if (this.contains(c)) this.remove(c); else this.add(c); },
    };
  }
  El.prototype = {
    appendChild(c) { c.parentNode = this; this.children.push(c); return c; },
    removeChild(c) { this.children = this.children.filter((x) => x !== c); c.parentNode = null; return c; },
    get firstChild() { return this.children[0] || null; },
    setAttribute(k, v) { this.attrs[k] = String(v); if (k === 'href') this._href = String(v); },
    getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; },
    removeAttribute(k) { delete this.attrs[k]; },
    hasAttribute(k) { return k in this.attrs; },
    addEventListener() {}, removeEventListener() {},
    focus() {}, getBoundingClientRect() { return { left: 0, right: 320, width: 320, top: 0, bottom: 800 }; },
    contains(n) { for (let x = n; x; x = x.parentNode) if (x === this) return true; return false; },
    get textContent() { return this._text + this.children.map((c) => c.textContent).join(''); },
    set textContent(v) { this.children = []; this._text = String(v); },
    get innerHTML() { return this._html; },
    set innerHTML(v) { htmlWrites++; this._html = String(v); this.children = []; },
    get href() { return this._href || this.attrs.href || ''; },
    set href(v) { this._href = String(v); this.attrs.href = String(v); },
    querySelector(sel) { return this.querySelectorAll(sel)[0] || null; },
    querySelectorAll(sel) {
      const parts = sel.split(',').map((s) => s.trim());
      const out = [];
      (function walk(n) {
        for (const c of n.children) {
          if (parts.some((p) => matches(c, p))) out.push(c);
          walk(c);
        }
      })(this);
      return out;
    },
  };
  function matches(el, sel) {
    const m = sel.match(/^([a-z]*)(?:\.([\w-]+))?(?:\[([\w-]+)(?:="([^"]*)")?\])?/i);
    if (!m) return false;
    if (m[1] && el.tagName !== m[1].toUpperCase()) return false;
    if (m[2] && !el.classList.contains(m[2])) return false;
    if (m[3] && !(m[3] in el.attrs) && !(m[3] === 'href' && el._href)) return false;
    if (m[4] != null && el.attrs[m[3]] !== m[4]) return false;
    return !!(m[1] || m[2] || m[3]);
  }
  const body = new El('body');
  const doc = {
    readyState: 'complete', body, activeElement: body,
    createElement: (t) => new El(t),
    addEventListener() {}, removeEventListener() {},
    querySelector: (s) => body.querySelector(s),
    querySelectorAll: (s) => body.querySelectorAll(s),
  };
  return { doc, htmlWrites: () => htmlWrites };
}

// ── harness ───────────────────────────────────────────────────────────────
function load(opts) {
  opts = opts || {};
  const { doc, htmlWrites } = makeDoc();
  const calls = [];
  let narrow = !!opts.narrow;
  const win = {
    innerWidth: narrow ? 700 : 1280,
    matchMedia: (q) => ({ get matches() { return /max-width/.test(q) ? narrow : false; }, addEventListener() {} }),
  };
  const ctx = {
    window: win, document: doc, console,
    requestAnimationFrame: (fn) => { fn(); return 1; },
    performance: { now: () => 0 },
    Promise, JSON, Object, Array, String, Number, Math, Error, Set,
    fetch: (url, init) => { calls.push(url); return opts.fetch ? opts.fetch(url, init) : new Promise(() => {}); },
  };
  win.fetch = ctx.fetch;
  vm.createContext(ctx);
  vm.runInContext(SRC, ctx);
  const drawer = () => doc.body.querySelector('[data-nav-drawer]');
  return {
    calls, doc, htmlWrites, win,
    api: win.__navDrawer,
    setNarrow(v) { narrow = v; win.innerWidth = v ? 700 : 1280; },
    footerLink: () => drawer() && drawer().querySelector('[data-nav-drawer-version]'),
  };
}
const flush = () => new Promise((r) => setTimeout(r, 0));
const ok = (body) => () => Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(body) });

// ── tests ─────────────────────────────────────────────────────────────────
test('the footer link is built with the neutral label and the fork releases URL, without a request', async () => {
  const b = load();
  await flush();
  const a = b.footerLink();
  assert(a, 'no [data-nav-drawer-version] footer link in the drawer');
  assert.strictEqual(a.tagName, 'A');
  assert.strictEqual(a.textContent, 'CoreScope');
  assert.strictEqual(a.href, RELEASES);
  assert.strictEqual(a.getAttribute('target') || a.target, '_blank');
  assert(/noopener/.test(a.getAttribute('rel') || a.rel || ''), 'rel lacks noopener');
  assert(a.parentNode && a.parentNode.classList.contains('nav-drawer-footer'), 'link is not inside .nav-drawer-footer');
  assert.strictEqual(b.calls.length, 0, 'fetched at page load');
});

test('no request while the drawer is gated off at narrow widths', async () => {
  const b = load({ narrow: true, fetch: ok({ version: 'v1.2.3' }) });
  b.api.open(); b.api.open();
  await flush();
  assert.strictEqual(b.calls.length, 0, b.calls.length + ' requests at a narrow width');
});

test('the first wide open fetches /api/health once and fills version and tooltip', async () => {
  const b = load({ fetch: ok({ version: 'v1.2.3', commit: 'abc1234', buildTime: '2026-09-01T10:00:00Z' }) });
  b.api.open();
  await flush(); await flush();
  assert.deepStrictEqual(b.calls.slice(), ['/api/health']);
  const a = b.footerLink();
  assert.strictEqual(a.textContent, 'CoreScope v1.2.3');
  assert(a.title.includes('abc1234') && a.title.includes('2026-09-01T10:00:00Z'), 'tooltip: ' + a.title);
});

test('re-opening (also after narrowing and widening) adds no requests', async () => {
  const b = load({ fetch: ok({ version: 'v1.2.3' }) });
  b.api.open(); b.api.close();
  await flush();
  b.api.open(); b.api.close(); b.api.toggle(); b.api.toggle();
  b.setNarrow(true); b.api.open(); b.setNarrow(false); b.api.open();
  await flush(); await flush();
  assert.strictEqual(b.calls.length, 1, b.calls.length + ' requests');
  assert.strictEqual(b.footerLink().textContent, 'CoreScope v1.2.3');
});

const neutral = {
  'a rejected fetch': () => Promise.reject(new Error('offline')),
  'a non-OK response': () => Promise.resolve({ ok: false, status: 503, json: () => Promise.resolve({ version: 'v9' }) }),
  'invalid JSON': () => Promise.resolve({ ok: true, status: 200, json: () => Promise.reject(new SyntaxError('bad')) }),
  'a missing version': ok({ commit: 'abc' }),
  'an empty version': ok({ version: '   ' }),
  'the server placeholder "unknown"': ok({ version: 'unknown', commit: 'unknown', buildTime: 'unknown' }),
  'a non-string version': ok({ version: { toString() { return 'x'; } } }),
  'a null body': ok(null),
};
for (const [what, fetch] of Object.entries(neutral)) {
  test(what + ' keeps the neutral "CoreScope" label and no tooltip, and is not retried', async () => {
    const b = load({ fetch });
    b.api.open();
    await flush(); await flush();
    const a = b.footerLink();
    assert.strictEqual(a.textContent, 'CoreScope');
    assert(!/undefined|null|unknown|object/i.test(a.textContent + ' ' + a.title), 'label/tooltip: ' + a.textContent + ' / ' + a.title);
    b.api.close(); b.api.open();
    await flush();
    assert.strictEqual(b.calls.length, 1, 'the failed request was retried on re-open');
  });
}

test('"unknown" commit/build are left out of the tooltip', async () => {
  const b = load({ fetch: ok({ version: 'v2', commit: 'unknown', buildTime: '2026-09-01' }) });
  b.api.open();
  await flush(); await flush();
  const a = b.footerLink();
  assert.strictEqual(a.textContent, 'CoreScope v2');
  assert(!/unknown/.test(a.title) && /2026-09-01/.test(a.title), 'tooltip: ' + a.title);
});

test('markup-shaped health values are rendered as text, never parsed as HTML', async () => {
  const evil = '<img src=x onerror=alert(1)>';
  const b = load({ fetch: ok({ version: evil, commit: '<b>c</b>', buildTime: '"><script>1</script>' }) });
  const before = b.htmlWrites();
  b.api.open();
  await flush(); await flush();
  const a = b.footerLink();
  assert.strictEqual(a.textContent, 'CoreScope ' + evil);
  assert.strictEqual(a.children.length, 0, 'the version created child elements');
  assert.strictEqual(a.innerHTML, '', 'innerHTML was used on the footer link');
  // opening the drawer writes no HTML except the static route icons
  assert.strictEqual(b.htmlWrites(), before, 'open() wrote HTML after the health response');
});

(async () => {
  for (const t of pending) await t();
  console.log(`\n${passed} passed, ${failed} failed`);
  if (failed > 0) process.exit(1);
})();
