#!/usr/bin/env node
/* test-privacy-page.js — the opt-in #/privacy page and the navigation
   surfaces that link to it.

   Two halves:
     1. public/privacy.js rendering — run in a vm sandbox like
        test-frontend-helpers.js, with escapeHtml extracted from the REAL
        public/app.js source so escaping assertions exercise production
        behaviour, not a test-local copy.
     2. the navigation lifecycle — public/nav-drawer.js and
        public/bottom-nav.js run against a minimal DOM shim (no jsdom dep,
        same approach as test-issue-1461-mobile-page-actions.js). These
        pin the async-config race: the navs are built at DOMContentLoaded,
        long before /api/config/client resolves, so they must reconcile
        afterwards instead of freezing at their pre-config state.

   Contract reminder: the server withholds the whole privacy block unless
   privacy.enabled AND every required field is present (see
   PrivacyConfig.Validate, cmd/server/config.go), so the page never renders
   invented retention/legal-basis text. */
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

// A fully-populated, valid config — mirrors what the server publishes only
// after Validate() passes.
// Mirrors validPrivacy() in cmd/server/privacy_config_test.go: the server
// only ever publishes a block that passed Validate(), so the page's fixture
// is a COMPLETE one. REQUIRED_FIELDS drives the "blank one at a time" tests.
const REQUIRED_FIELDS = [
  'controllerName', 'contactEmail', 'effectiveDate', 'purposesText',
  'legalBasisType', 'legalBasisText', 'retentionText', 'recipientsText',
  'dataSourcesText', 'thirdPartyServicesText', 'internationalTransfersText',
  'browserStorageText', 'serverLogsText', 'rightsRequestText',
  'supervisoryAuthorityName', 'supervisoryAuthorityUrl',
  'automatedDecisionMakingText',
];

const VALID = {
  enabled: true,
  controllerName: 'Example Mesh Community',
  contactEmail: 'privacy@example.org',
  effectiveDate: '2026-09-01',
  purposesText: 'Operating and troubleshooting the community network.',
  legalBasisType: 'public_task',
  legalBasisText: 'Processing is necessary for our community task.',
  retentionText: 'Packet data is deleted after 30 days.',
  recipientsText: 'Website and API visitors; our hosting provider.',
  dataSourcesText: 'Observer nodes, radio packets and derived measurements.',
  thirdPartyServicesText: 'Map tiles are loaded from a third-party provider.',
  internationalTransfersText: 'No transfers outside the EU/EEA.',
  browserStorageText: 'Interface settings are stored in your browser.',
  serverLogsText: 'Our proxy keeps access logs for 14 days.',
  rightsRequestText: 'Email us and we will assess your request.',
  supervisoryAuthorityName: 'Datatilsynet',
  supervisoryAuthorityUrl: 'https://www.datatilsynet.dk',
  automatedDecisionMakingText: 'No automated decision-making is used.',
};
const withField = (k, v) => Object.assign({}, VALID, { [k]: v });

// ───────────────────────── privacy.js render sandbox ─────────────────────────

function makeSandbox(mcPrivacy, opts) {
  opts = opts || {};
  const pages = {};
  const ctx = {
    window: { MC_PRIVACY: mcPrivacy },
    document: {},
    console, String, Object, Array, Promise, JSON, Error,
    encodeURIComponent,
    registerPage: (name, mod) => { pages[name] = mod; },
  };
  if (opts.meshConfigReady) ctx.window.MeshConfigReady = opts.meshConfigReady;
  ctx.globalThis = ctx;
  vm.createContext(ctx);
  vm.runInContext(escMatch[0] + '\nthis.escapeHtml = escapeHtml;', ctx);
  vm.runInContext(fs.readFileSync('public/privacy.js', 'utf8'), ctx);
  return { ctx, pages };
}

async function renderWith(mcPrivacy, opts) {
  const { pages } = makeSandbox(mcPrivacy, opts);
  assert(pages.privacy, 'privacy page not registered');
  const container = { innerHTML: '' };
  await pages.privacy.init(container);
  return container.innerHTML;
}

// ───────────────────────── minimal DOM shim for the navs ─────────────────────

function makeDom() {
  let idSeq = 0;
  function El(tag) {
    return {
      tagName: String(tag).toUpperCase(),
      _id: ++idSeq,
      children: [],
      parentNode: null,
      attrs: {},
      _classes: {},
      style: {},
      innerHTML: '',
      textContent: '',
      hidden: false,
      _listeners: {},
      get id() { return this.attrs.id || ''; },
      set id(v) { this.attrs.id = v; },
      get className() { return Object.keys(this._classes).join(' '); },
      set className(v) {
        this._classes = {};
        String(v).split(/\s+/).forEach((c) => { if (c) this._classes[c] = 1; });
      },
      classList: {
        add(c) { this._el._classes[c] = 1; },
        remove(c) { delete this._el._classes[c]; },
        contains(c) { return !!this._el._classes[c]; },
      },
      setAttribute(k, v) { this.attrs[k] = String(v); },
      getAttribute(k) { return Object.prototype.hasOwnProperty.call(this.attrs, k) ? this.attrs[k] : null; },
      removeAttribute(k) { delete this.attrs[k]; },
      appendChild(c) { if (c.parentNode) c.parentNode.removeChild(c); c.parentNode = this; this.children.push(c); return c; },
      insertBefore(c, ref) {
        if (c.parentNode) c.parentNode.removeChild(c);
        c.parentNode = this;
        const i = this.children.indexOf(ref);
        if (i < 0) this.children.push(c); else this.children.splice(i, 0, c);
        return c;
      },
      removeChild(c) { const i = this.children.indexOf(c); if (i >= 0) { this.children.splice(i, 1); c.parentNode = null; } return c; },
      get firstChild() { return this.children[0] || null; },
      addEventListener(t, fn) { (this._listeners[t] = this._listeners[t] || []).push(fn); },
      removeEventListener(t, fn) {
        const l = this._listeners[t]; if (!l) return;
        const i = l.indexOf(fn); if (i >= 0) l.splice(i, 1);
      },
      dispatch(t, ev) { (this._listeners[t] || []).forEach((fn) => fn(ev || { type: t })); },
      querySelector(sel) { return matchIn(this, sel)[0] || null; },
      querySelectorAll(sel) { return matchIn(this, sel); },
      focus() {},
      getBoundingClientRect() { return { top: 0, left: 0, right: 0, bottom: 0, width: 0, height: 0, x: 0, y: 0 }; },
      contains(n) { let p = n; while (p) { if (p === this) return true; p = p.parentNode; } return false; },
    };
  }
  function create(tag) {
    const el = El(tag);
    el.classList._el = el;
    return el;
  }
  function walk(root, out) {
    root.children.forEach((c) => { out.push(c); walk(c, out); });
    return out;
  }
  function matches(el, sel) {
    if (sel.charAt(0) === '.') return !!el._classes[sel.slice(1)];
    if (sel.charAt(0) === '#') return el.attrs.id === sel.slice(1);
    if (sel.charAt(0) === '[') {
      const m = sel.match(/^\[([^\]=]+)(?:="([^"]*)")?\]$/);
      if (!m) return false;
      const v = el.getAttribute(m[1]);
      if (v === null) return false;
      return m[2] === undefined ? true : v === m[2];
    }
    return el.tagName === sel.toUpperCase();
  }
  function matchIn(root, sel) {
    return walk(root, []).filter((el) => matches(el, sel));
  }

  const documentElement = create('html');
  const body = create('body');
  const doc = {
    readyState: 'complete',
    documentElement,
    body,
    activeElement: null,
    _listeners: {},
    createElement: create,
    getElementById(id) { return matchIn(body, '#' + id)[0] || matchIn(documentElement, '#' + id)[0] || null; },
    querySelector(sel) { return matchIn(body, sel)[0] || null; },
    querySelectorAll(sel) { return matchIn(body, sel); },
    addEventListener(t, fn) { (this._listeners[t] = this._listeners[t] || []).push(fn); },
    removeEventListener() {},
    dispatch(t, ev) { (this._listeners[t] || []).forEach((fn) => fn(ev || { type: t })); },
  };
  documentElement.appendChild(body);
  return doc;
}

// Boots one of the nav scripts against the shim. `deferConfig` returns a
// settle() so a test can decide WHEN /api/config/client resolves relative
// to the nav being built — that ordering is the whole point.
function bootNav(file, opts) {
  opts = opts || {};
  const doc = makeDom();
  // Do NOT pre-create [data-bottom-nav]: build() early-returns when it
  // already exists, which would skip the sheet entirely and make these
  // tests vacuously pass. Let the real code build its own tab bar.
  const main = doc.createElement('main');
  doc.body.appendChild(main);
  const appEl = doc.createElement('div');
  appEl.id = 'app';
  doc.body.appendChild(appEl);

  let settle;
  const ctx = {
    console, String, Object, Array, Promise, JSON, Error, Math, Date,
    setTimeout, clearTimeout,
    // nav-drawer.js schedules DOM work on a frame; run it inline so the
    // shim stays deterministic.
    requestAnimationFrame: (fn) => { fn(); return 1; },
    cancelAnimationFrame: () => {},
    document: doc,
    // bottom-nav.js reads a bare `location` (window.location in a browser).
    location: { hash: opts.hash || '#/home' },
    localStorage: { getItem: () => null, setItem() {}, removeItem() {} },
    window: {
      MC_CLIENT_RX_COVERAGE: false,
      MC_PRIVACY: null,
      location: { hash: opts.hash || '#/home' },
      matchMedia: () => ({ matches: !!opts.narrow, addEventListener() {}, removeEventListener() {} }),
      addEventListener() {},
      removeEventListener() {},
      dispatchEvent: () => true,
      innerWidth: 400,
    },
  };
  if (opts.withConfigPromise) {
    ctx.window.MeshConfigReady = new Promise((res, rej) => {
      settle = (cfg, fail) => {
        if (fail) { rej(new Error('config fetch failed')); return; }
        if (cfg) {
          ctx.window.MC_PRIVACY = cfg.privacy || null;
          ctx.window.MC_CLIENT_RX_COVERAGE = !!cfg.rxCoverage;
        }
        res(cfg || {});
      };
    });
    ctx.window.MeshConfigReady.catch(() => {});
  }
  ctx.window.document = doc;
  ctx.globalThis = ctx;
  vm.createContext(ctx);
  vm.runInContext(fs.readFileSync(file, 'utf8'), ctx, { filename: file });
  return { ctx, doc, settle: settle || (() => {}), tick: () => new Promise((r) => setTimeout(r, 0)) };
}

// The More sheet is LAZY: build() only makes the tab bar, and
// getOrBuildSheet() creates the sheet on first open. Opening it is
// therefore part of the scenario, not incidental setup.
function openMoreSheet(doc) {
  const moreTab = doc.querySelector('[data-bottom-nav-tab="more"]');
  assert(moreTab, 'more tab should exist');
  moreTab.dispatch('click', { preventDefault() {}, stopPropagation() {}, target: moreTab });
}

const drawerLinks = (doc) => doc.querySelectorAll('[data-nav-drawer-item]').map((a) => a.getAttribute('data-route'));
const sheetLinks = (doc) => doc.querySelectorAll('[data-bottom-nav-more-route]').map((a) => a.getAttribute('data-route'));

(async () => {
  console.log('test-privacy-page.js');

  // ─── page registration + gating ───────────────────────────────────────────

  await test('registers privacy page with init + destroy (a11y route registration)', async () => {
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

  await test('deep-link before config: waits for MeshConfigReady, then renders', async () => {
    let settle;
    const ready = new Promise((r) => { settle = r; });
    const { pages, ctx } = makeSandbox(null, { meshConfigReady: ready });
    const container = { innerHTML: '' };
    const p = pages.privacy.init(container);
    assert(container.innerHTML.includes('Loading'), 'should show a loading state while config is in flight');
    ctx.window.MC_PRIVACY = VALID;   // config lands
    settle({});
    await p;
    assert(container.innerHTML.includes('Example Mesh Community'),
      'must render the notice once config resolves, not the disabled state');
  });

  await test('config-fetch failure renders the not-published state, never a partial notice', async () => {
    const rejected = Promise.reject(new Error('offline'));
    rejected.catch(() => {});
    const html = await renderWith(null, { meshConfigReady: rejected });
    assert(html.includes('has not published a privacy notice'), 'expected disabled state on config failure');
  });

  // ─── content correctness: no invented legal text ──────────────────────────

  await test('renders every configured field', async () => {
    const html = await renderWith(VALID);
    assert(html.includes('Example Mesh Community'), 'controller name missing');
    assert(html.includes('href="mailto:privacy@example.org"'), 'mailto link missing');
    assert(html.includes('2026-09-01'), 'effective date missing');
    for (const f of REQUIRED_FIELDS) {
      const v = VALID[f];
      if (f === 'legalBasisType') continue;   // rendered as a friendly label
      assert(html.includes(v), 'field ' + f + ' (' + v + ') missing from the page');
    }
    assert(html.includes('Public task'), 'legalBasisType should render as a readable label');
  });

  await test('each REQUIRED field blanked individually → notice withheld', async () => {
    for (const f of REQUIRED_FIELDS) {
      const html = await renderWith(withField(f, ''));
      if (f === 'controllerName') {
        assert(html.includes('has not published a privacy notice'),
          'blank controllerName must withhold the notice client-side too');
      } else {
        // The server withholds these; the page must at minimum never
        // invent a value for them.
        assert(!html.includes('undefined') && !html.includes('null'),
          'blank ' + f + ' must not leak a placeholder into the page');
      }
    }
  });

  await test('legitimateInterestsText is rendered for the legitimate_interests basis', async () => {
    const cfg = Object.assign({}, VALID, {
      legalBasisType: 'legitimate_interests',
      legitimateInterestsText: 'Keeping the community mesh operable; see our assessment.',
    });
    const html = await renderWith(cfg);
    assert(html.includes('Legitimate interests'), 'basis label missing');
    assert(html.includes('Keeping the community mesh operable'), 'the specific interests must be shown');
  });

  await test('optional DPO section appears only when configured', async () => {
    const without = await renderWith(VALID);
    assert(!without.includes('Data protection officer'), 'no DPO section when unconfigured');
    const withDpo = await renderWith(Object.assign({}, VALID, { dpoName: 'Jane Doe' }));
    assert(withDpo.includes('Data protection officer') && withDpo.includes('Jane Doe'),
      'DPO section should render when configured');
  });

  await test('ships NO default retention paragraph', async () => {
    const html = await renderWith(VALID);
    assert(!html.includes('historical archive'),
      'the old fabricated default retention paragraph must be gone');
    const bare = await renderWith({ enabled: true, contactEmail: 'a@b.co' });
    assert(!bare.includes('historical archive'),
      'a config missing retentionText must not fall back to invented text');
  });

  await test('does NOT assert a legal basis on the operator behalf', async () => {
    const html = await renderWith(VALID);
    assert(!/processed under <strong>legitimate interest<\/strong>/.test(html),
      'the software must not hardcode legitimate interest');
    assert(html.includes('stated by the operator'),
      'the page must attribute the legal basis to the operator');
  });

  await test('controllerName has NO fallback — blank withholds the notice', async () => {
    for (const v of ['', '   ', undefined, null]) {
      const html = await renderWith(withField('controllerName', v));
      assert(html.includes('has not published a privacy notice'),
        'blank controllerName (' + JSON.stringify(v) + ') must not render a notice');
    }
    const src = fs.readFileSync('public/privacy.js', 'utf8');
    assert(!src.includes('The operator of this site'),
      'the generic operator fallback must be gone from the source entirely');
    assert(!/DEFAULT_OPERATOR/.test(src), 'no default-operator constant should remain');
  });

  // ─── hidden-name prefixes: real list, or silence ──────────────────────────

  await test('names the ACTUAL configured hidden prefixes', async () => {
    const html = await renderWith(Object.assign({}, VALID, { hiddenNamePrefixes: ['##'] }));
    assert(html.includes('hides nodes whose name begins with'), 'the real prefix should be described');
    assert(html.includes('<span class="mono">##</span>'), 'the real configured prefix must be shown');
  });

  await test('multiple prefixes are all listed', async () => {
    const html = await renderWith(Object.assign({}, VALID, { hiddenNamePrefixes: ['##', 'zz'] }));
    assert(html.includes('##') && html.includes('zz'), 'both prefixes must be shown');
  });

  await test('no configured prefixes → no self-service promise', async () => {
    const html = await renderWith(VALID);
    assert(!html.includes('hides nodes whose name begins with'),
      'must not describe prefix hiding when none is configured');
    assert(html.includes('no name-prefix hiding configured'), 'must say so explicitly');
  });

  await test('does not hardcode the no-entry emoji as a guarantee', async () => {
    const src = fs.readFileSync('public/privacy.js', 'utf8');
    assert(!src.includes('0x1F6AB'), 'the hardcoded hidden-prefix character must be gone');
    const html = await renderWith(VALID);
    assert(!html.includes(String.fromCodePoint(0x1F6AB)),
      'no hardcoded prefix character should reach the page');
  });

  await test('explains the real scope of hiding (dashboard/API, not the mesh; history may remain)', async () => {
    const html = await renderWith(Object.assign({}, VALID, { hiddenNamePrefixes: ['##'] }));
    assert(/dashboard and API/i.test(html), 'must scope hiding to this site, not the mesh');
    assert(/does not remove a node from the radio network/i.test(html),
      'must say the node stays on the radio network');
    assert(/other listeners still receive/i.test(html), 'must say others still receive it');
    assert(/does not by itself delete stored packets/i.test(html),
      'must be honest that recorded history can persist');
  });

  // ─── escaping + mailto edge cases ─────────────────────────────────────────

  await test('config values are HTML-escaped (XSS)', async () => {
    const html = await renderWith(Object.assign({}, VALID, {
      controllerName: '<img src=x onerror=alert(1)>',
      contactEmail: '"><script>alert(2)</script>',
      retentionText: '<b onmouseover=alert(3)>bold</b>',
      legalBasisText: '<svg onload=alert(4)>',
      purposesText: '</p><iframe src=evil>',
      supervisoryAuthorityName: '<a href=javascript:alert(5)>x</a>',
      supervisoryAuthorityUrl: 'javascript:alert(6)',
    }));
    assert(!html.includes('<img'), 'unescaped <img in output');
    assert(!html.includes('<script'), 'unescaped <script in output');
    assert(!html.includes('<b '), 'unescaped <b in output');
    assert(!html.includes('<svg onload'), 'unescaped <svg in output');
    assert(html.includes('&lt;img src=x onerror=alert(1)&gt;'), 'escaped controller name missing');
    assert(!html.includes('<iframe'), 'unescaped <iframe in output');
    assert(!/href="javascript:/i.test(html), 'javascript: URL must never reach an href');
    assert(!html.includes('href="mailto:"><'), 'contactEmail broke out of href attribute');
  });

  await test('mailto href is percent-encoded: quotes, ampersand, query chars, CR/LF', async () => {
    const cases = [
      ['a"b@example.org', '%22'],
      ['a&cc=x@example.org', '%26'],
      ['a?subject=x@example.org', '%3F'],
      ['a\r\nBcc:v@example.org', '%0D'],
      ['a b@example.org', '%20'],
      // encodeURIComponent leaves "'" alone; escapeHtml then renders it as
      // &#39;, which cannot break a double-quoted attribute either way.
      ["a'b@example.org", '&#39;'],
    ];
    for (const [addr, expected] of cases) {
      const html = await renderWith(Object.assign({}, VALID, { contactEmail: addr }));
      const m = html.match(/href="mailto:([^"]*)"/);
      assert(m, 'no mailto href rendered for ' + JSON.stringify(addr));
      assert(m[1].includes(expected),
        JSON.stringify(addr) + ' must be encoded (' + expected + '); got: ' + m[1]);
      assert(!/[\r\n]/.test(m[1]), 'raw CR/LF must never survive into the href');
      // What the browser actually navigates to is the entity-DECODED href.
      // encodeURIComponent runs first, so any character that could act as a
      // mailto separator is already percent-encoded; the only entities that
      // can appear come from escapeHtml re-encoding characters
      // encodeURIComponent leaves alone (e.g. "'" -> &#39;). Decode before
      // asserting, otherwise the "&" that starts an entity looks like a
      // separator.
      const decoded = m[1]
        .replace(/&#39;/g, "'").replace(/&quot;/g, '"')
        .replace(/&lt;/g, '<').replace(/&gt;/g, '>')
        .replace(/&amp;/g, '&');
      assert(decoded.indexOf('?') < 0 && decoded.indexOf('&') < 0,
        'no raw mailto query separator may survive: ' + decoded);
      assert(!/[\r\n]/.test(decoded), 'no CR/LF after entity decoding either');
    }
  });

  await test('mailto keeps a normal address readable (@ not over-encoded)', async () => {
    const html = await renderWith(VALID);
    assert(html.includes('href="mailto:privacy@example.org"'),
      'a clean address should render as-is, not percent-mangled');
  });

  await test('the old problematic phrasings are gone from the source', async () => {
    const src = fs.readFileSync('public/privacy.js', 'utf8');
    const banned = [
      // invented operator identity
      ['The operator of this site', 'generic controller fallback'],
      ['DEFAULT_OPERATOR', 'default-operator constant'],
      // invented retention policy
      ['historical archive', 'fabricated retention paragraph'],
      // legal basis asserted by the software
      ['processed under <strong>legitimate interest</strong>', 'hardcoded legitimate-interest claim'],
      ['GDPR Art. 6(1)(f)', 'hardcoded article citation'],
      // over-broad claim about public channels
      ['receivable and readable by anyone with a radio', 'over-broad public-channel claim'],
      // hardcoded hide character + unconditional promises
      ['0x1F6AB', 'hardcoded hide-prefix character'],
      ['it will be hidden or removed', 'unconditional hiding/removal promise'],
      ['disappears from this site', 'unconditional disappearance promise'],
    ];
    for (const [needle, why] of banned) {
      assert(!src.includes(needle), 'removed phrasing reappeared (' + why + '): ' + needle);
    }
    const html = await renderWith(VALID);
    for (const [needle, why] of banned) {
      assert(!html.includes(needle), 'removed phrasing rendered (' + why + '): ' + needle);
    }
  });

  await test('the page states the correct message semantics', async () => {
    const html = await renderWith(VALID);
    assert(/Direct-message content is not decrypted/i.test(html),
      'must say direct message CONTENT is not decrypted');
    assert(/metadata associated with direct messages/i.test(html),
      'must say direct-message METADATA is still processed');
    assert(/may become readable later if a corresponding key/i.test(html),
      'must say stored ciphertext may be decodable later');
    assert(/channels whose keys are available to this deployment/i.test(html),
      'channel decoding must be scoped to keys this deployment holds');
  });

  await test('the page names both radio traffic and CoreScope-generated data', async () => {
    const html = await renderWith(VALID);
    assert(/Reception metadata produced by observer nodes/i.test(html),
      'must name observer-produced reception metadata');
    assert(/Derived information/i.test(html), 'must name derived analytics');
    assert(/Observer identity, status and operational metrics/i.test(html),
      'must name observer metadata');
  });

  await test('rights section does not promise unconditional erasure', async () => {
    const html = await renderWith(VALID);
    assert(/not automatically granted/i.test(html),
      'must say requests are assessed, not automatically granted');
    assert(html.includes('Datatilsynet'), 'supervisory authority name missing');
    assert(html.includes('https://www.datatilsynet.dk'), 'supervisory authority URL missing');
  });

  await test('a complete config renders the page AND enables the nav surfaces', async () => {
    const html = await renderWith(VALID);
    assert(!html.includes('has not published a privacy notice'), 'complete config must render the notice');
    assert(html.includes('Privacy Notice'), 'heading missing');
    // ...and the same config drives both dynamic nav surfaces.
    const d = bootNav('public/nav-drawer.js', { withConfigPromise: true });
    const b = bootNav('public/bottom-nav.js', { withConfigPromise: true });
    d.settle({ privacy: VALID });
    b.settle({ privacy: VALID });
    await d.tick(); await b.tick();
    openMoreSheet(b.doc);
    assert(drawerLinks(d.doc).includes('privacy'), 'drawer must show Privacy for a complete config');
    assert(sheetLinks(b.doc).includes('privacy'), 'More sheet must show Privacy for a complete config');
  });

  // ─── navigation lifecycle: nav-drawer ─────────────────────────────────────

  await test('drawer built BEFORE config still gains the Privacy link afterwards', async () => {
    const h = bootNav('public/nav-drawer.js', { withConfigPromise: true });
    assert(!drawerLinks(h.doc).includes('privacy'),
      'no Privacy link before config — nothing is known yet');
    h.settle({ privacy: VALID });
    await h.tick();
    assert(drawerLinks(h.doc).includes('privacy'),
      'the drawer must reconcile once config lands (this was the permanent-omission bug)');
  });

  await test('drawer shows exactly one Privacy link (no duplicates on refresh)', async () => {
    const h = bootNav('public/nav-drawer.js', { withConfigPromise: true });
    h.settle({ privacy: VALID });
    await h.tick(); await h.tick();
    const n = drawerLinks(h.doc).filter((r) => r === 'privacy').length;
    assert.strictEqual(n, 1, 'expected exactly one Privacy link, got ' + n);
  });

  await test('drawer omits Privacy when the feature is off', async () => {
    const h = bootNav('public/nav-drawer.js', { withConfigPromise: true });
    h.settle({ privacy: null });
    await h.tick();
    assert(!drawerLinks(h.doc).includes('privacy'), 'no Privacy link when disabled/unconfigured');
  });

  await test('drawer keeps rx-coverage behaviour (still gated, still reconciled)', async () => {
    const off = bootNav('public/nav-drawer.js', { withConfigPromise: true });
    off.settle({ rxCoverage: false });
    await off.tick();
    assert(!drawerLinks(off.doc).includes('rx-coverage'), 'coverage stays out when disabled');

    const on = bootNav('public/nav-drawer.js', { withConfigPromise: true });
    on.settle({ rxCoverage: true });
    await on.tick();
    const links = drawerLinks(on.doc);
    assert(links.includes('rx-coverage'), 'coverage appears when enabled');
    assert.strictEqual(links.filter((r) => r === 'rx-coverage').length, 1, 'exactly one coverage link');
  });

  await test('drawer survives a config-fetch failure (renders base routes, no crash)', async () => {
    const h = bootNav('public/nav-drawer.js', { withConfigPromise: true });
    h.settle(null, true);
    await h.tick();
    const links = drawerLinks(h.doc);
    assert(links.length > 0, 'base routes must still be present after a failed config fetch');
    assert(!links.includes('privacy'), 'and no Privacy link, since nothing enabled it');
  });

  await test('drawer with no MeshConfigReady at all builds synchronously (legacy ordering)', async () => {
    const h = bootNav('public/nav-drawer.js', {});
    assert(drawerLinks(h.doc).length > 0, 'drawer must still build without a config promise');
  });

  // ─── navigation lifecycle: bottom nav More sheet ──────────────────────────

  await test('More sheet OPENED BEFORE config still gains the Privacy link afterwards', async () => {
    const h = bootNav('public/bottom-nav.js', { withConfigPromise: true });
    openMoreSheet(h.doc);                       // user is fast; config is not
    assert(h.doc.getElementById('bottomNavMoreSheet'), 'sheet should be built on open');
    assert(!sheetLinks(h.doc).includes('privacy'), 'no Privacy link yet — config has not landed');
    h.settle({ privacy: VALID });
    await h.tick();
    assert(sheetLinks(h.doc).includes('privacy'),
      'an already-open sheet must be reconciled once config lands (this was the race)');
  });

  await test('More sheet opened AFTER config has the Privacy link immediately', async () => {
    const h = bootNav('public/bottom-nav.js', { withConfigPromise: true });
    h.settle({ privacy: VALID });
    await h.tick();
    openMoreSheet(h.doc);
    assert(sheetLinks(h.doc).includes('privacy'), 'sheet built post-config must include Privacy');
  });

  await test('More sheet shows exactly one Privacy link (no duplicates on refresh)', async () => {
    const h = bootNav('public/bottom-nav.js', { withConfigPromise: true });
    openMoreSheet(h.doc);
    h.settle({ privacy: VALID });
    await h.tick(); await h.tick();
    const n = sheetLinks(h.doc).filter((r) => r === 'privacy').length;
    assert.strictEqual(n, 1, 'expected exactly one Privacy link, got ' + n);
  });

  await test('More sheet refresh keeps the separator and dark-mode button intact', async () => {
    const h = bootNav('public/bottom-nav.js', { withConfigPromise: true });
    openMoreSheet(h.doc);
    h.settle({ privacy: VALID });
    await h.tick();
    const sheet = h.doc.getElementById('bottomNavMoreSheet');
    assert(sheet, 'sheet should exist');
    assert.strictEqual(sheet.querySelectorAll('.bottom-nav-sheet-sep').length, 1, 'exactly one separator');
    assert.strictEqual(sheet.querySelectorAll('[data-bottom-nav-dark-toggle]').length, 1,
      'exactly one dark-mode button — the refresh must not drop or duplicate it');
    const kids = sheet.children;
    const sepIdx = kids.findIndex((c) => c._classes['bottom-nav-sheet-sep']);
    const lastRoute = kids.map((c) => !!c.getAttribute('data-bottom-nav-more-route')).lastIndexOf(true);
    assert(lastRoute < sepIdx, 'route links must be inserted above the separator');
  });

  await test('More sheet omits Privacy when the feature is off', async () => {
    const h = bootNav('public/bottom-nav.js', { withConfigPromise: true });
    h.settle({ privacy: null });
    await h.tick();
    openMoreSheet(h.doc);
    assert(sheetLinks(h.doc).length > 0, 'base More routes must be present');
    assert(!sheetLinks(h.doc).includes('privacy'), 'no Privacy link when disabled/unconfigured');
  });

  await test('More tab is active on #/privacy (after config)', async () => {
    const h = bootNav('public/bottom-nav.js', { withConfigPromise: true, hash: '#/privacy' });
    h.settle({ privacy: VALID });
    await h.tick();
    const moreTab = h.doc.querySelector('[data-bottom-nav-tab="more"]');
    assert(moreTab, 'more tab should exist');
    assert(moreTab.classList.contains('active'),
      'the More tab must light up on #/privacy — it owns that long-tail route');
  });

  await test('More tab is active on #/rx-coverage too (same dynamic list)', async () => {
    const h = bootNav('public/bottom-nav.js', { withConfigPromise: true, hash: '#/rx-coverage' });
    h.settle({ rxCoverage: true });
    await h.tick();
    const moreTab = h.doc.querySelector('[data-bottom-nav-tab="more"]');
    assert(moreTab.classList.contains('active'), 'More tab must light up on #/rx-coverage');
  });

  await test('bottom nav survives a config-fetch failure', async () => {
    const h = bootNav('public/bottom-nav.js', { withConfigPromise: true });
    h.settle(null, true);
    await h.tick();
    openMoreSheet(h.doc);
    assert(sheetLinks(h.doc).length > 0, 'base More routes must still be present');
    assert(!sheetLinks(h.doc).includes('privacy'), 'and no Privacy link');
  });

  // ─── consistency across the three nav surfaces ────────────────────────────

  await test('drawer and More sheet agree on whether Privacy is present', async () => {
    for (const enabled of [true, false]) {
      const d = bootNav('public/nav-drawer.js', { withConfigPromise: true });
      const b = bootNav('public/bottom-nav.js', { withConfigPromise: true });
      d.settle({ privacy: enabled ? VALID : null });
      b.settle({ privacy: enabled ? VALID : null });
      await d.tick(); await b.tick();
      openMoreSheet(b.doc);
      assert.strictEqual(
        drawerLinks(d.doc).includes('privacy'),
        sheetLinks(b.doc).includes('privacy'),
        'drawer and bottom-nav disagree for enabled=' + enabled);
    }
  });

  await test('desktop nav injection is gated on the same MC_PRIVACY flag', async () => {
    const src = fs.readFileSync('public/roles.js', 'utf8');
    assert(/window\.MC_PRIVACY\s*=/.test(src), 'roles.js must set window.MC_PRIVACY');
    assert(/data-route="privacy"/.test(src), 'roles.js must inject the desktop Privacy link');
    assert(/if \(window\.MC_PRIVACY && !document\.querySelector/.test(src),
      'desktop injection must be gated AND guarded against duplicates');
  });

  // ─── test registration ────────────────────────────────────────────────────

  await test('test-privacy-page.js is registered exactly once in test-all.sh', async () => {
    const sh = fs.readFileSync('test-all.sh', 'utf8');
    const lines = sh.split('\n').filter((l) => l.trim() === 'node test-privacy-page.js');
    assert.strictEqual(lines.length, 1,
      'expected exactly one registration line in test-all.sh, got ' + lines.length);
    assert(/^set -e$/m.test(sh), 'test-all.sh must keep its set -e semantics');
  });

  console.log(`\n${passed} passed, ${failed} failed`);
  process.exit(failed ? 1 : 0);
})();
