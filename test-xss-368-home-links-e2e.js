// test-xss-368-home-links-e2e.js
//
// Browser regression test for issue #368 — miniMarkdown() link destinations
// in the REAL Home rendering path.
//
// public/home.js feeds the Home "Getting on the mesh" steps and the FAQ
// checklist through window.miniMarkdown() (public/roles.js) and assigns the
// result with innerHTML. Both come from site-customization content
// (SITE_CONFIG.home, served by /api/config/theme and importable), so the
// Markdown link destination is attacker-reachable. Before the fix the
// destination went into `href="$2"` with no attribute escaping and no scheme
// check, so it could close the attribute and add an event handler, or use
// javascript: / data:.
//
// The hostile customization is injected by rewriting /api/config/theme in the
// browser with page.route — the fixture DB is untouched and no payload leaves
// the machine. The checks are DOM-level and payload-independent:
//   * did any injected handler run (window.__xss368) or any dialog open
//   * does any anchor the checklist rendered resolve to an unsafe scheme
//     (per the live HTMLAnchorElement.protocol a browser computed, not a
//     string match on the source)
//   * do the rendered anchors carry exactly the four attributes miniMarkdown
//     is supposed to emit, and no inline event handler
//   * do safe https / http / relative / fragment links still render, still
//     point where they were told to, and still navigate
//
// Usage: BASE_URL=http://localhost:13581 node test-xss-368-home-links-e2e.js
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';

// Group 1 — destinations that try to close href="…" and add their own
// attributes. These stay links (they carry no scheme), but the quotes must
// come out entity-encoded so the anchor gains nothing. No payload in this
// file contains ')': miniMarkdown's destination group is [^)]+, so a ')'
// would end the destination early and truncate the payload before it ever
// reached the href.
const BREAKOUT_DESTS = [
  '" onmouseover="window.__xss368=1',
  '" autofocus onfocus="window.__xss368=1',
  '" onerror="window.__xss368=1" x="',
  "/a' onmouseover='window.__xss368=1",
];

// Group 2 — destinations whose scheme is not on the allowlist, including
// ones obfuscated with case and with ASCII controls/whitespace that a
// browser ignores when it resolves a scheme. These must not become links at
// all; the label survives as plain text.
const UNSAFE_SCHEME_DESTS = [
  'javascript:window.__xss368=1',
  'JaVaScRiPt:window.__xss368=1',
  'java\u0000script:window.__xss368=1',
  'java\tscript:window.__xss368=1',
  '   javascript:window.__xss368=1',
  'data:text/html;base64,PHNjcmlwdD53aW5kb3cuX194c3MzNjg9MTwvc2NyaXB0Pg==',
  'vbscript:msgbox1',
];

// Group 3 — safe destinations that must keep working exactly as before.
const SAFE_DESTS = [
  'https://meshcore.example/docs',
  'http://meshcore.example/docs',
  '/nodes',
  '#/map',
];

// Steps carry groups 1 and 3, the FAQ checklist carries group 2 — both go
// through miniMarkdown() on the same render pass.
const STEP_DESTS = BREAKOUT_DESTS.concat(SAFE_DESTS);
const HOSTILE_HOME = {
  heroTitle: 'CoreScope',
  heroSubtitle: 'issue #368 fixture',
  steps: STEP_DESTS.map((d, i) => ({
    emoji: '',
    title: 'Step ' + i,
    description: 'see [xss368-step-' + i + '](' + d + ') now',
  })),
  checklist: UNSAFE_SCHEME_DESTS.map((d, i) => ({
    question: 'Question ' + i,
    answer: 'see [xss368-scheme-' + i + '](' + d + ') now',
  })),
};

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

async function hostileHomePage(browser) {
  const ctx = await browser.newContext({ viewport: { width: 1400, height: 900 } });
  await ctx.route((url) => url.pathname === '/api/config/theme', async (route) => {
    const res = await route.fetch();
    let body = {};
    try { body = await res.json(); } catch (e) { body = {}; }
    body.home = HOSTILE_HOME;
    await route.fulfill({ response: res, json: body });
  });
  const page = await ctx.newPage();
  page.setDefaultTimeout(15000);
  page.on('pageerror', (e) => console.error('    [pageerror]', e.message));
  page._dialogs = 0;
  page.on('dialog', (d) => { page._dialogs++; d.dismiss().catch(() => {}); });
  return { ctx, page };
}

(async () => {
  let browser;
  try {
    browser = await chromium.launch();
  } catch (e) {
    if (process.env.CHROMIUM_REQUIRE === '1') {
      console.error('Chromium required but unavailable: ' + e.message);
      process.exit(1);
    }
    console.log('⚠️  Chromium unavailable, skipping: ' + e.message);
    process.exit(0);
  }

  const { ctx, page } = await hostileHomePage(browser);
  await page.goto(BASE + '/#/home', { waitUntil: 'domcontentloaded' });
  // A context with no 'meshcore-user-level' gets the experience chooser
  // first; the checklist section only renders for the "I'm new" level.
  await page.locator('#chooseNew').click();
  // The checklist is rendered from SITE_CONFIG.home, which lands after
  // /api/config/theme resolves, so wait for our own fixture text.
  await page.waitForFunction(
    () => /xss368-step-0/.test(document.body.textContent || ''),
    null, { timeout: 15000 });
  // Give any injected timer/handler a tick to fire before we count.
  await page.waitForTimeout(300);

  console.log('\n=== #368 — hostile Home Markdown link destinations ===');

  await step('both payload groups actually reached the checklist', async () => {
    const seen = await page.evaluate(() => ({
      bodies: document.querySelectorAll('.home-checklist .checklist-a').length,
      text: document.body.textContent || '',
    }));
    const want = STEP_DESTS.length + UNSAFE_SCHEME_DESTS.length;
    assert(seen.bodies === want, 'rendered ' + seen.bodies + ' checklist bodies, want ' + want);
    assert(seen.text.includes('xss368-scheme-0'), 'scheme payloads never rendered');
  });

  await step('no injected handler ran and no dialog opened', async () => {
    const fired = await page.evaluate(() => window.__xss368 || 0);
    assert(fired === 0, 'injected handler fired ' + fired + ' time(s)');
    assert(page._dialogs === 0, page._dialogs + ' dialog(s) opened');
  });

  await step('no checklist anchor resolves to an unsafe scheme', async () => {
    const bad = await page.evaluate(() => Array.from(
      document.querySelectorAll('.home-checklist a'))
      .map(a => ({ proto: a.protocol, raw: a.getAttribute('href') || '' }))
      .filter(x => !['http:', 'https:'].includes(x.proto))
      .map(x => x.proto + ' ' + x.raw));
    assert(bad.length === 0, 'unsafe anchors: ' + JSON.stringify(bad));
  });

  await step('every unsafe-scheme destination degraded to plain label text', async () => {
    const rows = await page.evaluate((n) => {
      const out = [];
      for (let i = 0; i < n; i++) {
        const label = 'xss368-scheme-' + i;
        out.push({
          label,
          anchored: Array.from(document.querySelectorAll('.home-checklist a'))
            .some(a => (a.textContent || '').trim() === label),
          present: (document.body.textContent || '').includes(label),
        });
      }
      return out;
    }, UNSAFE_SCHEME_DESTS.length);
    const anchored = rows.filter(r => r.anchored).map(r => r.label);
    assert(anchored.length === 0, 'unsafe destinations became links: ' + anchored.join(', '));
    const missing = rows.filter(r => !r.present).map(r => r.label);
    assert(missing.length === 0, 'label text was dropped entirely: ' + missing.join(', '));
  });

  await step('no checklist anchor grew an attribute miniMarkdown does not emit', async () => {
    const extra = await page.evaluate(() => {
      const allowed = ['href', 'target', 'rel', 'style'];
      const out = [];
      document.querySelectorAll('.home-checklist .checklist-a a').forEach((a) => {
        const names = Array.from(a.attributes).map(x => x.name.toLowerCase());
        const unexpected = names.filter(n => !allowed.includes(n));
        if (unexpected.length) out.push(a.outerHTML.slice(0, 160) + ' -> ' + unexpected.join(','));
      });
      return out;
    });
    assert(extra.length === 0, 'unexpected attributes: ' + JSON.stringify(extra));
  });

  await step('no element the checklist rendered carries an inline event handler', async () => {
    const handlers = await page.evaluate(() => {
      const out = [];
      document.querySelectorAll('.home-checklist, .home-checklist *').forEach((el) => {
        Array.from(el.attributes || []).forEach((at) => {
          if (/^on/i.test(at.name)) out.push(el.tagName + '[' + at.name + ']');
        });
      });
      return out;
    });
    assert(handlers.length === 0, 'inline handlers: ' + JSON.stringify(handlers));
  });

  await step('quote-breakout destinations stay inert inside the href value', async () => {
    const rows = await page.evaluate((dests) => dests.map((d, i) => {
      const label = 'xss368-step-' + i;
      const a = Array.from(document.querySelectorAll('.home-checklist .checklist-a a'))
        .find(x => (x.textContent || '').trim() === label);
      return { label, found: !!a, href: a ? a.getAttribute('href') : null, want: d };
    }), BREAKOUT_DESTS);
    for (const r of rows) {
      assert(r.found, r.label + ' did not render (a schemeless destination stays a link)');
      // The whole payload is still exactly one attribute VALUE: nothing
      // escaped into attribute position.
      assert(r.href === r.want, r.label + ' href is ' + JSON.stringify(r.href) +
        ', want ' + JSON.stringify(r.want));
    }
  });

  console.log('\n=== #368 — safe Home Markdown links still work ===');

  await step('every safe destination still renders as an anchor with the given href', async () => {
    const rows = await page.evaluate((args) => args.dests.map((d, i) => {
      const label = 'xss368-step-' + (args.offset + i);
      const a = Array.from(document.querySelectorAll('.home-checklist .checklist-a a'))
        .find(x => (x.textContent || '').trim() === label);
      return {
        label,
        found: !!a,
        href: a ? a.getAttribute('href') : null,
        target: a ? a.getAttribute('target') : null,
        rel: a ? a.getAttribute('rel') : null,
        want: d,
      };
    }), { dests: SAFE_DESTS, offset: BREAKOUT_DESTS.length });
    for (const r of rows) {
      assert(r.found, r.label + ' did not render as a link');
      assert(r.href === r.want, r.label + ' href is ' + r.href + ', want ' + r.want);
      assert(r.target === '_blank', r.label + ' lost target="_blank"');
      assert(r.rel === 'noopener', r.label + ' lost rel="noopener"');
    }
  });

  await step('a safe hash link still navigates', async () => {
    const label = 'xss368-step-' + (STEP_DESTS.length - 1); // the '#/map' one
    await page.evaluate((l) => {
      const a = Array.from(document.querySelectorAll('.home-checklist .checklist-a a'))
        .find(x => (x.textContent || '').trim() === l);
      a.removeAttribute('target'); // keep the navigation in this tab
      a.click();
    }, label);
    await page.waitForFunction(() => location.hash === '#/map', null, { timeout: 10000 });
    assert((await page.evaluate(() => location.hash)) === '#/map', 'hash link did not navigate');
  });

  await ctx.close();
  await browser.close();

  console.log('\n' + passed + ' passed, ' + failed + ' failed');
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
