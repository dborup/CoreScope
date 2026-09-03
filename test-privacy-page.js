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
// ─────────────────────── the authoritative notice ───────────────────────
//
// This is the notice, verbatim, in the Markdown it was signed off in. It is
// the single source of truth for the golden test below: the rendered page,
// normalised back to visible text, must equal this EXACTLY -- so extra,
// missing or reworded text all fail, not just missing text.
//
// String.raw is load-bearing: a plain template literal would let JS eat the
// Markdown escapes (\. \@) and the trailing-backslash hard line break before
// the test ever parsed them, quietly weakening the comparison.
const AUTHORITATIVE = String.raw`## WHO WE ARE

meshview\.dk is a non-commercial community service that visualises the Danish [MeshCore](https://meshcore.co.uk/) LoRa mesh network. It runs the open-source CoreScope analyzer. The data controller is:

**The operator of meshview\.dk**\
Contact: **kontakt\@meshview\.dk**

## WHAT THIS SITE DOES

Volunteer-run observer nodes listen to MeshCore radio traffic and forward the packets they hear to this site over MQTT. The site displays a live map and analysis of the network so that node operators and the community can see coverage, diagnose problems, and keep the mesh healthy.

## WHAT DATA WE PROCESS

All data originates from radio packets that MeshCore devices broadcast themselves:

- **Node adverts**: node name, role, public key, and the GPS position the node is configured to advertise. Node names are chosen by their operators and may contain a personal handle or name; an advertised position may reveal where the operator lives.
- **Packet metadata**: timestamps, packet types, routing paths, hop counts, and signal measurements (SNR/RSSI) as heard by observers.
- **Node telemetry**: values a node chooses to broadcast, such as battery level and uptime.
- **Public channel messages**: messages sent on well-known public channels (whose encryption keys are community knowledge) are decoded and shown, including the sender's node name and timestamp. Direct (private) messages are end-to-end encrypted and are never decrypted or displayed.

**Website visitors**: the site uses no analytics, tracking, or advertising cookies. [Our web server keeps standard technical logs, including IP addresses, for a short period for security and abuse prevention.]

## WHY, AND ON WHAT LEGAL BASIS

We process this data under **legitimate interest** (GDPR Art. 6(1)(f)): operating, mapping, and troubleshooting a community radio network — the same purpose for which node operators broadcast this information in the first place. The data shown is limited to what devices already transmit openly over the air, and an easy opt-out exists (below).

## HOW LONG WE KEEP IT

Packet data, telemetry, and decoded public-channel messages are kept **indefinitely**, as a historical archive used for long-term network analysis (coverage trends, node health over time). We periodically review the archive and delete data that is no longer needed for that purpose. The node directory and map reflect the **current** state of the network; nodes that stop advertising disappear from the live view, though their historical packets remain in the archive.

## WHO CAN SEE IT, AND WHO WE SHARE IT WITH

The site is publicly accessible, so anything displayed here can be seen by anyone. We do not sell data or share it with third parties, apart from the hosting provider that technically operates the server [hosted within the EU/EEA].

## A NOTE ON PUBLIC CHANNELS

Public MeshCore channels are receivable and readable by anyone with a radio. Please do not send personal information over them — this site, like any other listener, will pick it up and keep it in the archive.`;

// The page is opt-in only; there is no operator content to configure.
const ENABLED = { enabled: true };

// A pre-removal config.json: every operator-text key the model used to
// carry. None of it may reach the page.
const STALE_CONFIG = {
  enabled: true,
  controllerName: 'STALE-controller',
  contactEmail: 'stale@example.invalid',
  effectiveDate: 'STALE-date',
  purposesText: 'STALE-purposes',
  legalBasisType: 'legitimate_interests',
  legalBasisText: 'STALE-basis',
  legitimateInterestsText: 'STALE-interests',
  retentionText: 'STALE-retention',
  recipientsText: 'STALE-recipients',
  dataSourcesText: 'STALE-sources',
  thirdPartyServicesText: 'STALE-third-party',
  internationalTransfersText: 'STALE-transfers',
  browserStorageText: 'STALE-storage',
  serverLogsText: 'STALE-logs',
  automatedDecisionMakingText: 'STALE-automated',
  rightsRequestText: 'STALE-rights',
  supervisoryAuthorityName: 'STALE-authority',
  supervisoryAuthorityUrl: 'https://stale.example',
  dpoName: 'STALE-dpo',
  dpoContact: 'STALE-dpo-contact',
  hiddenNamePrefixes: ['STALE-prefix'],
};

// One normaliser, applied to BOTH sides, so the comparison is about words
// and order -- never about indentation or how a line happens to wrap.
const normalize = (s) => s.split('\n').map((l) => l.trim()).filter(Boolean).join('\n');

// Markdown inline syntax -> the text a reader actually sees.
const inlineText = (s) => s
  .replace(/\[([^\]]+)\]\([^)\s]+\)/g, '$1')   // [label](url) -> label
  .replace(/\*\*([\s\S]+?)\*\*/g, '$1')        // **bold** -> bold
  .replace(/\\([\s\S])/g, '$1');                // \. \@ -> . @

// The authoritative Markdown, reduced to the visible text it specifies.
function expectedVisibleText(md) {
  const out = [];
  md.trim().split(/\n\s*\n/).forEach((chunk) => {
    chunk = chunk.replace(/^\n+|\n+$/g, '');
    if (chunk.startsWith('## ')) { out.push(chunk.slice(3).trim()); return; }
    const lines = chunk.split('\n');
    if (lines.every((l) => l.trim().startsWith('- '))) {
      lines.forEach((l) => out.push(inlineText(l.trim().slice(2))));
      return;
    }
    // A trailing backslash is a Markdown hard line break.
    out.push(chunk.split(/\\\n/).map((part) => inlineText(part.replace(/\n/g, ' '))).join('\n'));
  });
  return normalize(out.join('\n'));
}

// Rendered markup -> the visible text. Block ends and <br> become line
// breaks; everything else is tags, which carry no words.
function visibleText(html) {
  return normalize(html
    .replace(/<br\s*\/?>/g, '\n')
    .replace(/<\/(h1|h2|h3|h4|p|li|div)>/g, '\n')
    .replace(/<[^>]+>/g, '')
    .replace(/&amp;/g, '&').replace(/&lt;/g, '<').replace(/&gt;/g, '>')
    .replace(/&quot;/g, '"').replace(/&#39;/g, "'"));
}

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
    const html = await renderWith({ enabled: false });
    assert(html.includes('has not published a privacy notice'), 'expected disabled state');
  });

  await test('deep-link before config: waits for MeshConfigReady, then renders', async () => {
    let settle;
    const ready = new Promise((r) => { settle = r; });
    const { pages, ctx } = makeSandbox(null, { meshConfigReady: ready });
    const container = { innerHTML: '' };
    const p = pages.privacy.init(container);
    assert(container.innerHTML.includes('Loading'), 'should show a loading state while config is in flight');
    ctx.window.MC_PRIVACY = ENABLED; // config lands
    settle({});
    await p;
    assert(container.innerHTML.includes('WHO WE ARE'),
      'must render the notice once config resolves, not the disabled state');
  });

  await test('config-fetch failure renders the not-published state, never a partial notice', async () => {
    const rejected = Promise.reject(new Error('offline'));
    rejected.catch(() => {});
    const html = await renderWith(null, { meshConfigReady: rejected });
    assert(html.includes('has not published a privacy notice'), 'expected disabled state on config failure');
  });

  // ─── GOLDEN: the page is the notice, exactly ─────────────────────────────

  await test('GOLDEN: rendered visible text equals the authoritative notice, exactly', async () => {
    const actual = visibleText(await renderWith(ENABLED));
    const expected = expectedVisibleText(AUTHORITATIVE);
    if (actual !== expected) {
      // Report the first divergent line so a reword is obvious, not a wall.
      const a = actual.split('\n'), e = expected.split('\n');
      let i = 0;
      while (i < Math.max(a.length, e.length) && a[i] === e[i]) i++;
      assert.fail(
        `visible text diverges at line ${i + 1} of ${e.length}\n` +
        `  expected: ${JSON.stringify(e[i])}\n` +
        `  actual  : ${JSON.stringify(a[i])}\n` +
        `  (lines: expected ${e.length}, actual ${a.length})`);
    }
    assert.strictEqual(actual, expected);
  });

  await test('GOLDEN: no config value can add, remove or reword a single line', async () => {
    // Same assertion, but driven by a full pre-removal config. Byte-identical
    // output proves the page reads nothing but the enabled flag.
    const clean = visibleText(await renderWith(ENABLED));
    const stale = visibleText(await renderWith(STALE_CONFIG));
    assert.strictEqual(stale, clean, 'a stale config changed the rendered notice');
    assert(!/STALE-/.test(stale), 'a stale config value reached the page');
    assert.strictEqual(stale, expectedVisibleText(AUTHORITATIVE));
  });

  // ─── structure of the notice ──────────────────────────────────────────────

  await test('exactly 7 headings, in the authoritative order', async () => {
    const html = await renderWith(ENABLED);
    const headings = (html.match(/<h[1-6][^>]*>[\s\S]*?<\/h[1-6]>/g) || [])
      .map((h) => h.replace(/<[^>]*>/g, '').trim());
    assert.deepStrictEqual(headings, [
      'WHO WE ARE',
      'WHAT THIS SITE DOES',
      'WHAT DATA WE PROCESS',
      'WHY, AND ON WHAT LEGAL BASIS',
      'HOW LONG WE KEEP IT',
      'WHO CAN SEE IT, AND WHO WE SHARE IT WITH',
      'A NOTE ON PUBLIC CHANNELS',
    ]);
    assert.strictEqual(headings.length, 7, 'expected exactly 7 headings');
  });

  await test('exactly 4 data points, in order, each with its bold lead-in', async () => {
    const html = await renderWith(ENABLED);
    const items = (html.match(/<li>[\s\S]*?<\/li>/g) || []);
    assert.strictEqual(items.length, 4, 'expected exactly 4 list items');
    const leads = items.map((li) => (li.match(/<strong>([^<]*)<\/strong>/) || [])[1]);
    assert.deepStrictEqual(leads,
      ['Node adverts', 'Packet metadata', 'Node telemetry', 'Public channel messages']);
    assert.strictEqual((html.match(/<ul>/g) || []).length, 1, 'expected exactly one list');
  });

  await test('the MeshCore link points at the authoritative URL and is safe', async () => {
    const html = await renderWith(ENABLED);
    const links = html.match(/<a\b[^>]*>/g) || [];
    assert.strictEqual(links.length, 1, 'the notice has exactly one link');
    assert(/href="https:\/\/meshcore\.co\.uk\/"/.test(html), 'MeshCore href is wrong or missing');
    assert(/>MeshCore<\/a>/.test(html), 'the link text must be MeshCore');
    assert(/rel="noopener noreferrer"/.test(html), 'external link needs rel=noopener noreferrer');
    assert(!/href="(?!https?:)/i.test(html), 'only http(s) hrefs may be emitted');
    assert(!/javascript:|data:/i.test(html), 'no javascript:/data: URL anywhere');
  });

  await test('meshview.dk and kontakt@meshview.dk render without backslashes', async () => {
    const text = visibleText(await renderWith(ENABLED));
    assert(!text.includes('\\'), 'a Markdown escape leaked into the visible text');
    assert(text.includes('meshview.dk is a non-commercial community service'),
      'meshview.dk must render as plain text in the opening sentence');
    assert(text.includes('The operator of meshview.dk'), 'controller line must render unescaped');
    assert(text.includes('Contact: kontakt@meshview.dk'), 'contact must render unescaped');
    assert(!text.includes('meshview\\.dk'), 'escaped domain leaked');
    assert(!text.includes('kontakt\\@'), 'escaped address leaked');
    // The contact is bold text, not a mailto link.
    const html = await renderWith(ENABLED);
    assert(html.includes('<strong>kontakt@meshview.dk</strong>'), 'contact must be bold text');
    assert(!/mailto:/.test(html), 'the notice specifies no mailto link');
  });

  await test('both square-bracket passages survive verbatim, brackets included', async () => {
    const text = visibleText(await renderWith(ENABLED));
    assert(text.includes('[Our web server keeps standard technical logs, including IP addresses, ' +
      'for a short period for security and abuse prevention.]'),
      'the server-log passage must keep its square brackets');
    assert(text.includes('[hosted within the EU/EEA]'),
      'the hosting passage must keep its square brackets');
  });

  await test('no extra privacy sections: every removed section is gone', async () => {
    const text = visibleText(await renderWith(STALE_CONFIG));
    [
      'Privacy Notice', 'Effective date', 'Data controller', 'Privacy contact',
      'What data this site processes', 'Purpose of processing', 'Legal basis',
      'Sources of the data', 'Who can receive the data', 'Retention',
      'Channel and direct messages', 'Storage in your browser',
      'Server and proxy logs', 'External services', 'International transfers',
      'Hidden nodes', 'Your rights', 'Automated decision-making',
      'Changes to this notice', 'Data protection officer', 'Datatilsynet',
      'supervisory authority', 'CoreScope deployment provides status information',
      'not automatically granted', 'Send privacy requests to',
    ].forEach((s) => assert(!text.includes(s), 'removed section still rendered: ' + s));
    // Structural: exactly the blocks the notice specifies, nothing spare.
    const html = await renderWith(ENABLED);
    assert.strictEqual((html.match(/<h2/g) || []).length, 7, 'exactly 7 section headings');
    assert.strictEqual((html.match(/<p>/g) || []).length, 9, 'exactly 9 paragraphs');
    assert(!/<p>\s*<\/p>/.test(html), 'empty paragraph left behind');
    assert(!/<h2[^>]*>\s*<\/h2>/.test(html), 'empty heading left behind');
    assert(!/<ul>\s*<\/ul>|<li>\s*<\/li>/.test(html), 'empty list left behind');
    assert(!/<div[^>]*>\s*<\/div>/.test(html), 'empty wrapper left behind');
    assert(!/<hr\b/.test(html), 'no separator element belongs in the notice');
    assert.strictEqual((html.match(/<div/g) || []).length, 1, 'exactly one wrapper div');
  });

  // ─── safety of the fixed document ─────────────────────────────────────────

  await test('the page reads nothing from config but the enabled flag', async () => {
    const src = fs.readFileSync('public/privacy.js', 'utf8');
    const reads = src.match(/cfg\.[A-Za-z_$][\w$]*/g) || [];
    assert.deepStrictEqual([...new Set(reads)], ['cfg.enabled'],
      'privacy.js must read only cfg.enabled, got: ' + [...new Set(reads)].join(', '));
    assert(!/MC_PRIVACY\s*\.\s*[A-Za-z]/.test(src), 'no direct field read off MC_PRIVACY');
  });

  await test('hostile config values cannot inject markup', async () => {
    const html = await renderWith(Object.assign({}, STALE_CONFIG, {
      controllerName: '<img src=x onerror=alert(1)>',
      purposesText: '</p><iframe src=evil>',
      supervisoryAuthorityUrl: 'javascript:alert(2)',
    }));
    assert(!html.includes('<img'), 'unescaped <img in output');
    assert(!html.includes('<iframe'), 'unescaped <iframe in output');
    assert(!/onerror=/.test(html), 'event handler reached the DOM');
    assert(!/javascript:/i.test(html), 'javascript: URL reached the DOM');
    assert.strictEqual(visibleText(html), expectedVisibleText(AUTHORITATIVE),
      'hostile config changed the rendered notice');
  });

  await test('the notice text lives in code, not in config', async () => {
    const src = fs.readFileSync('public/privacy.js', 'utf8');
    ['WHO WE ARE', 'A NOTE ON PUBLIC CHANNELS', 'https://meshcore.co.uk/'].forEach((s) =>
      assert(src.includes(s), 'privacy.js must carry the notice itself: ' + s));
    const ex = JSON.parse(fs.readFileSync('config.example.json', 'utf8'));
    assert.deepStrictEqual(Object.keys(ex.privacy), ['enabled', '_comment'],
      'config.example.json privacy block must be the opt-in flag plus its comment');
    ['cmd/server/config.go', 'cmd/server/types.go', 'cmd/server/routes.go'].forEach((f) => {
      const go = fs.readFileSync(f, 'utf8');
      ['ControllerName', 'PurposesText', 'RetentionText', 'SupervisoryAuthorityName', 'DPOName']
        .forEach((g) => assert(!go.includes(g), f + ' still references removed privacy field ' + g));
    });
  });

  await test('an enabled config renders the notice AND enables the nav surfaces', async () => {
    const html = await renderWith(ENABLED);
    assert(!html.includes('has not published a privacy notice'), 'enabled must render the notice');
    assert(html.includes('WHO WE ARE'), 'notice missing');
    const d = bootNav('public/nav-drawer.js', { withConfigPromise: true });
    const b = bootNav('public/bottom-nav.js', { withConfigPromise: true });
    d.settle({ privacy: ENABLED });
    b.settle({ privacy: ENABLED });
    await d.tick(); await b.tick();
    openMoreSheet(b.doc);
    assert(drawerLinks(d.doc).includes('privacy'), 'drawer must show Privacy when enabled');
    assert(sheetLinks(b.doc).includes('privacy'), 'More sheet must show Privacy when enabled');
  });

  // ─── navigation lifecycle: nav-drawer ─────────────────────────────────────

  await test('drawer built BEFORE config still gains the Privacy link afterwards', async () => {
    const h = bootNav('public/nav-drawer.js', { withConfigPromise: true });
    assert(!drawerLinks(h.doc).includes('privacy'),
      'no Privacy link before config — nothing is known yet');
    h.settle({ privacy: ENABLED });
    await h.tick();
    assert(drawerLinks(h.doc).includes('privacy'),
      'the drawer must reconcile once config lands (this was the permanent-omission bug)');
  });

  await test('drawer shows exactly one Privacy link (no duplicates on refresh)', async () => {
    const h = bootNav('public/nav-drawer.js', { withConfigPromise: true });
    h.settle({ privacy: ENABLED });
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
    h.settle({ privacy: ENABLED });
    await h.tick();
    assert(sheetLinks(h.doc).includes('privacy'),
      'an already-open sheet must be reconciled once config lands (this was the race)');
  });

  await test('More sheet opened AFTER config has the Privacy link immediately', async () => {
    const h = bootNav('public/bottom-nav.js', { withConfigPromise: true });
    h.settle({ privacy: ENABLED });
    await h.tick();
    openMoreSheet(h.doc);
    assert(sheetLinks(h.doc).includes('privacy'), 'sheet built post-config must include Privacy');
  });

  await test('More sheet shows exactly one Privacy link (no duplicates on refresh)', async () => {
    const h = bootNav('public/bottom-nav.js', { withConfigPromise: true });
    openMoreSheet(h.doc);
    h.settle({ privacy: ENABLED });
    await h.tick(); await h.tick();
    const n = sheetLinks(h.doc).filter((r) => r === 'privacy').length;
    assert.strictEqual(n, 1, 'expected exactly one Privacy link, got ' + n);
  });

  await test('More sheet refresh keeps the separator and dark-mode button intact', async () => {
    const h = bootNav('public/bottom-nav.js', { withConfigPromise: true });
    openMoreSheet(h.doc);
    h.settle({ privacy: ENABLED });
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
    h.settle({ privacy: ENABLED });
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
      d.settle({ privacy: enabled ? ENABLED : null });
      b.settle({ privacy: enabled ? ENABLED : null });
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
