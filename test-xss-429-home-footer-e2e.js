// test-xss-429-home-footer-e2e.js
//
// Browser regression test for issue #429 — URL schemes in customization-fed
// href/src sinks, on the REAL pages.
//
// 1. Home footer links. public/home.js renders SITE_CONFIG.home.footerLinks
//    (served by /api/config/theme, importable customization) as
//    <a href="${escapeAttr(l.url)}">. escapeAttr only covered the quotes, so a
//    javascript:/data:/vbscript: URL became a live link, and so did an
//    entity-encoded scheme (&#106;avascript:) that the browser decodes while
//    it parses the attribute.
// 2. Branding logo / favicon. public/customize-v2.js wrote branding.logoUrl
//    into the navbar <img src> and branding.faviconUrl into
//    <link rel=icon>.href with no scheme check, from the server theme, from
//    the stored overrides (cs-theme-overrides, also what a theme import
//    writes) and from the customizer input; it also wrote an unescaped
//    '<img src="' + logoUrl + '">' into any .brand-icon element.
//
// Hostile customization is injected by rewriting /api/config/theme with
// page.route or by seeding localStorage with addInitScript — the fixture DB
// is untouched and no payload leaves the machine. The checks are DOM-level:
// did any injected handler run (window.__xss429) or dialog open, which
// protocol did the browser compute for each anchor/img/link, and which
// attributes did each footer anchor end up with.
//
// Usage: BASE_URL=http://localhost:13581 node test-xss-429-home-footer-e2e.js
'use strict';
const { chromium } = require('playwright');

const BASE = process.env.BASE_URL || 'http://localhost:13581';
const SAFE_PROTOCOLS = ['http:', 'https:'];

// Group 1 — unsafe schemes, plain and obfuscated with case and the ASCII
// controls/whitespace a browser ignores. Must not become links; the label
// survives as plain text.
const UNSAFE_URLS = [
  'javascript:window.__xss429=1',
  'JaVaScRiPt:window.__xss429=1',
  ' \t\njavascript:window.__xss429=1',
  'java\nscript:window.__xss429=1',
  'java\u0000script:window.__xss429=1',
  '\u0001javascript:window.__xss429=1',
  'data:text/html;base64,PHNjcmlwdD53aW5kb3cuX194c3M0Mjk9MTwvc2NyaXB0Pg==',
  'vbscript:msgbox1',
];
// Group 2 — no scheme in the raw text, but a scheme once an HTML entity is
// decoded. They may stay links, but only as inert relative URLs.
const ENTITY_URLS = [
  '&#106;avascript:window.__xss429=1',
  '&#x6A;avascript:window.__xss429=1',
  'javascript&colon;window.__xss429=1',
];
// Group 3 — try to leave the attribute. Stay links with exactly the four
// attributes the footer emits.
const BREAKOUT_URLS = [
  '" onmouseover="window.__xss429=1',
  '" autofocus onfocus="window.__xss429=1" x="',
];
// Group 4 — safe links that must keep working exactly as before.
const SAFE_URLS = [
  'https://meshcore.example/docs',
  'http://meshcore.example/docs',
  '/nodes',
  '#/map',
];
const FOOTER = []
  .concat(UNSAFE_URLS.map((u, i) => ({ label: 'xss429-bad-' + i, url: u })))
  .concat(ENTITY_URLS.map((u, i) => ({ label: 'xss429-ent-' + i, url: u })))
  .concat(BREAKOUT_URLS.map((u, i) => ({ label: 'xss429-brk-' + i, url: u })))
  .concat(SAFE_URLS.map((u, i) => ({ label: 'xss429-ok-' + i, url: u })))
  // A rejected link with a hostile label: the label must come out as text.
  .concat([{ label: '<img src=x onerror="window.__xss429=1">xss429-lbl', url: 'javascript:void0' }]);

const HOSTILE_LOGOS = [
  'javascript:window.__xss429=1',
  ' JaVa\tScRiPt:window.__xss429=1',
  'data:text/html;base64,PHNjcmlwdD53aW5kb3cuX194c3M0Mjk9MTwvc2NyaXB0Pg==',
];
// Image sinks keep accepting an inline data:image/ logo (nothing in an
// <img> runs script, and the customizer has always taken one).
const INLINE_LOGO = 'data:image/svg+xml;base64,PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciLz4=';
const SAFE_LOGO = '/img/corescope-logo.svg?ok=429';
const SAFE_FAVICON = '/favicon.ico?ok=429';

let passed = 0, failed = 0;
async function step(name, fn) {
  try { await fn(); passed++; console.log('  ✓ ' + name); }
  catch (e) { failed++; console.error('  ✗ ' + name + ': ' + e.message); }
}
function assert(c, m) { if (!c) throw new Error(m || 'assertion failed'); }

// Opens a fresh context. `theme` is merged into the /api/config/theme body;
// `overrides` seeds cs-theme-overrides; `brandIcon` adds the legacy
// <span class="brand-icon"> a custom index.html may still carry.
async function openPage(browser, opts) {
  const ctx = await browser.newContext({ viewport: { width: 1400, height: 900 } });
  if (opts.theme) {
    await ctx.route((url) => url.pathname === '/api/config/theme', async (route) => {
      const res = await route.fetch();
      let body = {};
      try { body = await res.json(); } catch (e) { body = {}; }
      Object.keys(opts.theme).forEach((k) => {
        body[k] = Object.assign({}, body[k] || {}, opts.theme[k]);
      });
      await route.fulfill({ response: res, json: body });
    });
  }
  await ctx.addInitScript((o) => {
    try {
      localStorage.setItem('meshcore-user-level', 'new');
      if (o.overrides) localStorage.setItem('cs-theme-overrides', JSON.stringify(o.overrides));
    } catch (e) {}
    if (o.brandIcon) {
      document.addEventListener('readystatechange', () => {
        const brand = document.querySelector('.nav-brand');
        if (brand && !brand.querySelector('.brand-icon')) {
          const s = document.createElement('span');
          s.className = 'brand-icon';
          brand.appendChild(s);
        }
      });
    }
  }, { overrides: opts.overrides || null, brandIcon: !!opts.brandIcon });
  const page = await ctx.newPage();
  page.setDefaultTimeout(15000);
  page.on('pageerror', (e) => console.error('    [pageerror]', e.message));
  page._dialogs = 0;
  page.on('dialog', (d) => { page._dialogs++; d.dismiss().catch(() => {}); });
  return { ctx, page };
}

async function brandState(page) {
  return page.evaluate(() => {
    const logo = document.querySelector('.nav-brand .brand-logo');
    const fav = document.querySelector('link[rel="icon"]');
    const iconImgs = Array.from(document.querySelectorAll('.brand-icon img'));
    return {
      logoTag: logo ? logo.tagName.toLowerCase() : null,
      logoSrc: logo && logo.tagName.toLowerCase() === 'img' ? logo.src : null,
      favHref: fav ? fav.href : null,
      iconImgs: iconImgs.map(i => ({ src: i.src, attrs: Array.from(i.attributes).map(a => a.name) })),
      fired: window.__xss429 || 0,
    };
  });
}
function protocolOf(u) {
  try { return new URL(u).protocol; } catch (e) { return 'unparseable:'; }
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

  // ── 1. Home footer links ────────────────────────────────────────────
  {
    const { ctx, page } = await openPage(browser, {
      theme: { home: { heroTitle: 'CoreScope', heroSubtitle: 'issue #429 fixture', footerLinks: FOOTER } },
    });
    await page.goto(BASE + '/#/home', { waitUntil: 'domcontentloaded' });
    await page.waitForFunction(
      () => /xss429-ok-0/.test((document.querySelector('.home-footer-links') || {}).textContent || ''),
      null, { timeout: 15000 });
    // Hover and focus every footer anchor so an injected mouseover/focus
    // handler gets its chance to run before we count.
    const n = await page.locator('.home-footer-links a').count();
    for (let i = 0; i < n; i++) {
      const a = page.locator('.home-footer-links a').nth(i);
      await a.hover().catch(() => {});
      await a.focus().catch(() => {});
    }
    await page.waitForTimeout(300);

    console.log('\n=== #429 — hostile Home footer links ===');

    const rows = await page.evaluate(() => {
      const anchors = Array.from(document.querySelectorAll('.home-footer-links a'));
      return {
        text: (document.querySelector('.home-footer-links') || {}).textContent || '',
        anchors: anchors.map(a => ({
          label: (a.textContent || '').trim(),
          proto: a.protocol,
          href: a.getAttribute('href'),
          attrs: Array.from(a.attributes).map(x => x.name.toLowerCase()),
        })),
        imgs: document.querySelectorAll('.home-footer-links img').length,
        fired: window.__xss429 || 0,
      };
    });
    const byLabel = (l) => rows.anchors.find(a => a.label === l);

    await step('every footer entry reached the page', async () => {
      const missing = FOOTER.map(f => f.label.replace(/^<img[^>]*>/, ''))
        .filter(l => !rows.text.includes(l));
      assert(missing.length === 0, 'missing: ' + missing.join(', '));
    });

    await step('no injected handler ran and no dialog opened', async () => {
      assert(rows.fired === 0, 'injected handler fired ' + rows.fired + ' time(s)');
      assert(page._dialogs === 0, page._dialogs + ' dialog(s) opened');
    });

    await step('no footer anchor resolves to an unsafe scheme', async () => {
      const bad = rows.anchors.filter(a => !['http:', 'https:'].includes(a.proto))
        .map(a => a.label + ' -> ' + a.proto + ' ' + JSON.stringify(a.href));
      assert(bad.length === 0, 'unsafe anchors: ' + JSON.stringify(bad));
    });

    await step('every unsafe-scheme URL degraded to plain label text', async () => {
      const anchored = UNSAFE_URLS.map((_, i) => 'xss429-bad-' + i).filter(l => byLabel(l));
      assert(anchored.length === 0, 'unsafe URLs became links: ' + anchored.join(', '));
      assert(!rows.anchors.some(a => a.label.includes('xss429-lbl')), 'the hostile-label entry became a link');
    });

    await step('a rejected entry keeps its label as escaped text, not markup', async () => {
      assert(rows.text.includes('<img src=x onerror="window.__xss429=1">xss429-lbl'),
        'label text was not rendered verbatim');
      assert(rows.imgs === 0, rows.imgs + ' <img> element(s) inside the footer');
    });

    await step('entity-obfuscated schemes stay literal, inert relative URLs', async () => {
      ENTITY_URLS.forEach((u, i) => {
        const a = byLabel('xss429-ent-' + i);
        if (!a) return; // dropping it would be safe too
        assert(SAFE_PROTOCOLS.includes(a.proto), a.label + ' resolves to ' + a.proto);
        assert(a.href === u, a.label + ' href decoded to ' + JSON.stringify(a.href) + ', want the literal ' + JSON.stringify(u));
      });
    });

    await step('quote-breakout URLs add no attribute to the anchor', async () => {
      BREAKOUT_URLS.forEach((u, i) => {
        const a = byLabel('xss429-brk-' + i);
        assert(a, 'xss429-brk-' + i + ' did not render (a schemeless URL stays a link)');
        assert(JSON.stringify(a.attrs) === JSON.stringify(['href', 'class', 'target', 'rel']),
          a.label + ' carries ' + a.attrs.join(','));
        assert(a.href === u, a.label + ' href is ' + JSON.stringify(a.href));
      });
    });

    console.log('\n=== #429 — safe Home footer links still work ===');

    await step('every safe URL still renders as a link with the given href', async () => {
      SAFE_URLS.forEach((u, i) => {
        const a = byLabel('xss429-ok-' + i);
        assert(a, 'xss429-ok-' + i + ' did not render as a link');
        assert(a.href === u, a.label + ' href is ' + a.href + ', want ' + u);
        assert(JSON.stringify(a.attrs) === JSON.stringify(['href', 'class', 'target', 'rel']),
          a.label + ' carries ' + a.attrs.join(','));
      });
    });

    await step('a safe hash footer link still navigates', async () => {
      const label = 'xss429-ok-' + SAFE_URLS.indexOf('#/map');
      await page.evaluate((l) => {
        const a = Array.from(document.querySelectorAll('.home-footer-links a'))
          .find(x => (x.textContent || '').trim() === l);
        a.removeAttribute('target'); // keep the navigation in this tab
        a.click();
      }, label);
      await page.waitForFunction(() => location.hash === '#/map', null, { timeout: 10000 });
    });

    await ctx.close();
  }

  // ── 2. Branding logo / favicon ──────────────────────────────────────
  console.log('\n=== #429 — hostile branding.logoUrl / faviconUrl ===');

  for (let i = 0; i < HOSTILE_LOGOS.length; i++) {
    const u = HOSTILE_LOGOS[i];
    await step('server theme: logo/favicon ' + JSON.stringify(u) + ' is not applied', async () => {
      const { ctx, page } = await openPage(browser, {
        theme: { branding: { logoUrl: u, faviconUrl: u } },
      });
      await page.goto(BASE + '/#/home', { waitUntil: 'load' });
      await page.waitForFunction(() => !!window.SITE_CONFIG, null, { timeout: 15000 });
      await page.waitForTimeout(300);
      const s = await brandState(page);
      await ctx.close();
      assert(s.fired === 0, 'injected handler fired');
      if (s.logoSrc) assert(SAFE_PROTOCOLS.includes(protocolOf(s.logoSrc)), 'logo img src is ' + s.logoSrc.slice(0, 60));
      assert(!s.favHref || SAFE_PROTOCOLS.includes(protocolOf(s.favHref)), 'favicon href is ' + String(s.favHref).slice(0, 60));
    });
  }

  await step('stored overrides (theme import): hostile logo/favicon are not applied', async () => {
    const u = HOSTILE_LOGOS[0];
    const { ctx, page } = await openPage(browser, {
      overrides: { branding: { logoUrl: u, faviconUrl: u } },
    });
    await page.goto(BASE + '/#/home', { waitUntil: 'load' });
    await page.waitForFunction(() => !!window.SITE_CONFIG, null, { timeout: 15000 });
    await page.waitForTimeout(300);
    const s = await brandState(page);
    await ctx.close();
    if (s.logoSrc) assert(SAFE_PROTOCOLS.includes(protocolOf(s.logoSrc)), 'logo img src is ' + s.logoSrc);
    assert(!s.favHref || SAFE_PROTOCOLS.includes(protocolOf(s.favHref)), 'favicon href is ' + s.favHref);
  });

  await step('customizer: a hostile stored logo gets no preview <img>', async () => {
    const { ctx, page } = await openPage(browser, {
      overrides: { branding: { logoUrl: HOSTILE_LOGOS[0] } },
    });
    await page.goto(BASE + '/#/home', { waitUntil: 'load' });
    await page.click('#customizeToggle');
    const tab = await page.$('.cust-tab[data-tab="branding"]');
    if (tab) await tab.click();
    await page.waitForSelector('input[data-cv2-field="branding.logoUrl"]', { timeout: 5000 });
    const previews = await page.evaluate(() =>
      Array.from(document.querySelectorAll('.cust-preview-img')).map(i => i.src));
    await ctx.close();
    const bad = previews.filter(p => !SAFE_PROTOCOLS.includes(protocolOf(p)));
    assert(bad.length === 0, 'unsafe preview src: ' + JSON.stringify(bad));
  });

  await step('customizer: typing a javascript: logo URL does not reach the navbar', async () => {
    const { ctx, page } = await openPage(browser, {});
    await page.goto(BASE + '/#/home', { waitUntil: 'load' });
    await page.click('#customizeToggle');
    const tab = await page.$('.cust-tab[data-tab="branding"]');
    if (tab) await tab.click();
    const inp = await page.waitForSelector('input[data-cv2-field="branding.logoUrl"]', { timeout: 5000 });
    await inp.fill(HOSTILE_LOGOS[0]);
    await inp.dispatchEvent('input');
    const fav = await page.waitForSelector('input[data-cv2-field="branding.faviconUrl"]', { timeout: 5000 });
    await fav.fill(HOSTILE_LOGOS[0]);
    await fav.dispatchEvent('input');
    await page.waitForTimeout(300);
    const s = await brandState(page);
    await ctx.close();
    if (s.logoSrc) assert(SAFE_PROTOCOLS.includes(protocolOf(s.logoSrc)), 'logo img src is ' + s.logoSrc);
    assert(!s.favHref || SAFE_PROTOCOLS.includes(protocolOf(s.favHref)), 'favicon href is ' + s.favHref);
  });

  await step('a legacy .brand-icon never receives logo markup', async () => {
    const { ctx, page } = await openPage(browser, {
      brandIcon: true,
      theme: { branding: { logoUrl: 'x" onerror="window.__xss429=1' } },
      overrides: { branding: { logoUrl: 'y" onerror="window.__xss429=1' } },
    });
    await page.goto(BASE + '/#/home', { waitUntil: 'load' });
    await page.waitForFunction(() => !!window.SITE_CONFIG, null, { timeout: 15000 });
    await page.waitForTimeout(500);
    const s = await brandState(page);
    const hasIcon = await page.evaluate(() => !!document.querySelector('.brand-icon'));
    await ctx.close();
    assert(hasIcon, 'test setup: the .brand-icon element was not injected');
    assert(s.fired === 0, 'injected handler fired ' + s.fired + ' time(s)');
    const handlers = s.iconImgs.filter(i => i.attrs.some(a => /^on/i.test(a)));
    assert(handlers.length === 0, '.brand-icon img with inline handler: ' + JSON.stringify(handlers));
  });

  console.log('\n=== #429 — safe branding still applies ===');

  await step('a safe logo and favicon are still applied', async () => {
    const { ctx, page } = await openPage(browser, {
      theme: { branding: { logoUrl: SAFE_LOGO, faviconUrl: SAFE_FAVICON } },
    });
    await page.goto(BASE + '/#/home', { waitUntil: 'load' });
    await page.waitForFunction(() => {
      const l = document.querySelector('.nav-brand .brand-logo');
      return l && l.tagName.toLowerCase() === 'img';
    }, null, { timeout: 15000 });
    const s = await brandState(page);
    await ctx.close();
    assert(s.logoSrc && s.logoSrc.endsWith(SAFE_LOGO), 'logo img src is ' + s.logoSrc);
    assert(s.favHref && s.favHref.endsWith(SAFE_FAVICON), 'favicon href is ' + s.favHref);
  });

  await step('an inline data:image/ logo is still applied', async () => {
    const { ctx, page } = await openPage(browser, {
      theme: { branding: { logoUrl: INLINE_LOGO } },
    });
    await page.goto(BASE + '/#/home', { waitUntil: 'load' });
    await page.waitForFunction(() => {
      const l = document.querySelector('.nav-brand .brand-logo');
      return l && l.tagName.toLowerCase() === 'img';
    }, null, { timeout: 15000 });
    const s = await brandState(page);
    await ctx.close();
    assert(s.logoSrc === INLINE_LOGO, 'logo img src is ' + String(s.logoSrc).slice(0, 60));
  });

  await step('the customizer shows a preview for a safe stored logo', async () => {
    const { ctx, page } = await openPage(browser, {
      overrides: { branding: { logoUrl: SAFE_LOGO } },
    });
    await page.goto(BASE + '/#/home', { waitUntil: 'load' });
    await page.click('#customizeToggle');
    const tab = await page.$('.cust-tab[data-tab="branding"]');
    if (tab) await tab.click();
    await page.waitForSelector('input[data-cv2-field="branding.logoUrl"]', { timeout: 5000 });
    const previews = await page.evaluate(() =>
      Array.from(document.querySelectorAll('.cust-preview-img')).map(i => i.src));
    await ctx.close();
    assert(previews.some(p => p.endsWith(SAFE_LOGO)), 'no preview for the safe logo: ' + JSON.stringify(previews));
  });

  await browser.close();

  console.log('\n' + passed + ' passed, ' + failed + ' failed');
  process.exit(failed ? 1 : 0);
})().catch((e) => { console.error(e); process.exit(1); });
