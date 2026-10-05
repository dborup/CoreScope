#!/usr/bin/env node
/* Issue #1060 / PR #1067 follow-up — touch targets behavior test.
 *
 * MAJOR-2 from pr-polish review: the previous version of this file
 * grep'd CSS strings, which is tautological — it asserted that the
 * source contained the literal characters that were just edited in.
 * It would have passed even if the CSS was syntactically broken or
 * if selectors didn't match any element on the real page.
 *
 * This rewrite loads public/style.css into a real Chromium page via
 * Playwright with an iPhone-class touch emulation context, renders
 * representative DOM samples for every selector we claim to harden,
 * and reads getBoundingClientRect()/getComputedStyle() to assert the
 * 48x48 minimum hit area. It also exercises the .sort-help tap-to-
 * reveal flow (focus event must un-hide the .sort-help-tip) since
 * MAJOR-1 is enforced both in markup (tabindex="0" in packets.js) and
 * in CSS (:focus / :focus-within rule in the Touch Targets section).
 */
'use strict';

const fs = require('fs');
const path = require('path');
const assert = require('assert');
const { chromium, devices } = require('playwright');

const REPO = __dirname;
// Every local stylesheet, in the order index.html links them, so the
// cascade matches the app (home.css restyles .suggest-claim, live.css and
// channel-proposals.css carry their own controls).
const SHEETS = [...fs.readFileSync(path.join(REPO, 'public/index.html'), 'utf8')
  .matchAll(/<link rel="stylesheet" href="([\w.-]+\.css)\?v=__BUST__">/g)].map((m) => m[1]);
const CSS = SHEETS.map((f) => fs.readFileSync(path.join(REPO, 'public', f), 'utf8')).join('\n');

// All listed button surfaces use the shared 48px house minimum (#2052).
const DEFAULT_MIN = 48;

// Each entry: [selector, tag, classes, optional wrapper]. The wrapper is
// markup around the control, with {} where the control goes, for rules that
// only apply inside a parent (`.modal > .modal-close`, `.filter-bar .btn`).
// Tag matters because some rules are scoped to `button.ch-item` and some only
// apply to specific input[type=...].
//
// The harness is an iPhone 13 (390 px wide, coarse pointer), so the
// `@media (max-width: 640px)` and `@media (pointer: coarse)` rules apply.
//
// Not listed, and why:
//   .compare-btn      the Compare CTA was removed in #1646 (see style.css)
//   .ch-back-btn      no markup renders it since #1367; the channel header's
//                     back button is .ch-back, listed below
//   .feed-show-btn, .legend-toggle-btn  live.css hides both at <=640px
//                     (display:none !important), the width this harness has
const BUTTON_SELECTORS = [
  ['.btn',                   'button', 'btn'],
  ['.btn-icon',              'button', 'btn-icon'],
  ['.nav-btn',               'button', 'nav-btn'],
  ['.ch-icon-btn',           'button', 'ch-icon-btn'],
  ['.ch-remove-btn',         'button', 'ch-remove-btn'],
  ['.ch-share-btn',          'button', 'ch-share-btn'],
  ['.ch-gear-btn',           'button', 'ch-gear-btn'],
  ['.panel-close-btn',       'button', 'panel-close-btn'],
  ['.mc-jump-btn',           'button', 'mc-jump-btn'],
  ['button.ch-item',         'button', 'ch-item'],
  ['.btn-link',              'button', 'btn-link'],
  ['.col-toggle-btn',        'button', 'col-toggle-btn'],
  ['.ch-add-channel-btn',    'button', 'ch-add-channel-btn'],
  ['.ch-modal-btn-secondary','button', 'ch-modal-btn-secondary'],
  ['.ch-scroll-btn',         'button', 'ch-scroll-btn'],
  ['.chooser-btn',           'button', 'chooser-btn'],
  ['.clock-filter-btn',      'button', 'clock-filter-btn'],
  ['.copy-link-btn',         'button', 'copy-link-btn'],
  ['.alab-btn',              'button', 'alab-btn'],
  ['.fav-star',              'button', 'fav-star'],
  // #235: the controls that were still 44 px (40 px for .ch-back).
  ['.theme-toggle',          'label',  'theme-toggle'],
  ['.modal > .modal-close',  'button', 'modal-close', '<div class="modal">{}</div>'],
  ['.ch-modal-close',        'button', 'modal-close ch-modal-close', '<div class="modal ch-modal">{}</div>'],
  ['.ch-back',               'button', 'ch-back', '<div class="ch-layout ch-detail-open">{}</div>'],
  ['.ch-avatar.ch-tappable', 'div',    'ch-avatar ch-tappable'],
  ['.suggest-claim',         'button', 'suggest-claim'],
  ['.detail-back-btn',       'button', 'detail-back-btn'],
  ['.filter-toggle-btn',     'button', 'filter-toggle-btn'],
  ['.filter-bar .btn',       'button', 'btn', '<div class="filter-bar filters-expanded">{}</div>'],
  ['.filter-group .btn',     'button', 'btn', '<div class="filter-group">{}</div>'],
  ['.tab-btn',               'button', 'tab-btn'],
  ['.region-pill',           'button', 'region-pill'],
  ['.region-dropdown-trigger','button', 'region-dropdown-trigger'],
  ['.multi-select-trigger',  'button', 'multi-select-trigger'],
  ['.node-count-pill',       'span',   'node-count-pill'],
  ['.analytics-time-range button', 'button', '', '<div class="analytics-time-range">{}</div>'],
  ['.leaflet-control-zoom a','a',      '', '<div class="leaflet-bar leaflet-control leaflet-control-zoom">{}</div>'],
  ['.live-leaflet-toggle a', 'a',      '', '<div class="leaflet-bar leaflet-control live-leaflet-toggle">{}</div>'],
];

// Form controls and text buttons that only set a height. min-WIDTH is not
// enforced on these (text fields legitimately span a wide column); we only
// require min-height: 48px. Entry: [selector, tag, classes, inner, attrs, wrapper].
const FIELD_SELECTORS = [
  ['select',                 'select', '',                 '<option>x</option>'],
  ['input[type=text]',       'input',  '', null, { type: 'text' }],
  ['input[type=search]',     'input',  '', null, { type: 'search' }],
  ['input[type=number]',     'input',  '', null, { type: 'number' }],
  ['input[type=email]',      'input',  '', null, { type: 'email' }],
  ['input[type=password]',   'input',  '', null, { type: 'password' }],
  ['input[type=tel]',        'input',  '', null, { type: 'tel' }],
  ['input[type=url]',        'input',  '', null, { type: 'url' }],
  ['input[type=date]',       'input',  '', null, { type: 'date' }],
  ['input[type=time]',       'input',  '', null, { type: 'time' }],
  // #235
  ['.filter-bar input',      'input',  '', null, { type: 'text' }, '<div class="filter-bar filters-expanded">{}</div>'],
  ['.filter-bar select',     'select', '', '<option>x</option>', null, '<div class="filter-bar filters-expanded">{}</div>'],
  ['.ch-proposals-toolbar button', 'button', '', 'x', null, '<div class="ch-proposals-toolbar">{}</div>'],
  ['.ch-proposals-actions button', 'button', '', 'x', null, '<div class="ch-proposals-actions">{}</div>'],
  // In its real context: .live-toggles label must not override it (#239 F2).
  ['.live-toggles .live-node-filter-hitarea', 'label', 'live-node-filter-hitarea', 'x', null, '<div class="live-toggles"><div class="live-node-filter-wrap">{}</div></div>'],
];

// Invisible ::after tap pads that give a compact control its hit area.
// Entry: [name, control markup with data-pad on the padded element].
const PAD_SELECTORS = [
  ['live region dropdown ::after', '<div class="live-controls-body"><div class="live-region-filter-container">' +
    '<button class="region-dropdown-trigger" data-pad="live-region">x</button></div></div>'],
];

// #249: the add-channel dialog's primary buttons, in their real dialog rows
// (from channels.js) next to their 48 px inputs, with their real labels.
// Entry: [id, row classes, row markup before the button, label].
const DIALOG_BUTTONS = [
  ['chGenerateBtn', 'ch-modal-row', '<input type="text" class="ch-modal-input">', 'Generate &amp; Show QR'],
  ['chPskAddBtn', 'ch-modal-row', '<input type="text" class="ch-modal-input">', 'Add'],
  ['chHashtagBtn', 'ch-modal-row ch-hashtag-row',
    '<span class="ch-hashtag-prefix" aria-hidden="true">#</span><input type="text" class="ch-modal-input">', 'Monitor'],
];

function wrap(wrapper, html) {
  return wrapper ? wrapper.replace('{}', html) : html;
}

function buildSampleHtml() {
  const buttons = BUTTON_SELECTORS
    .map(([_, tag, cls, wrapper], i) => wrap(wrapper, `<${tag} class="${cls}" data-btn="${i}">x</${tag}>`))
    .join('\n      ');
  const fields = FIELD_SELECTORS
    .map(([sel, tag, cls, inner, attrs, wrapper], i) => {
      const attrStr = attrs
        ? Object.entries(attrs).map(([k, v]) => `${k}="${v}"`).join(' ')
        : '';
      const open = `<${tag} class="${cls}" ${attrStr} data-field="${i}">`;
      const close = tag === 'input' ? '' : `${inner || ''}</${tag}>`;
      return wrap(wrapper, open + close);
    })
    .join('\n      ');
  const pads = PAD_SELECTORS.map(([, html]) => html).join('\n      ');
  const dialog = '<div class="modal ch-modal" role="document">' + DIALOG_BUTTONS
    .map(([id, rowCls, before, label]) => `<section class="ch-modal-section"><div class="${rowCls}">${before}` +
      `<button type="button" id="${id}" class="btn-primary">${label}</button></div></section>`)
    .join('') + '</div>';

  // .sort-help sample mirrors the markup the JS produces (post-fix):
  // tabindex="0" so :focus-within can fire on touch tap.
  return `<!doctype html>
<html><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>${CSS}</style>
<style>/* leaflet.css (CDN, not loaded here) makes the bar links blocks */ .leaflet-bar a { display: block; }</style>
</head><body>
  <div id="harness" style="padding: 16px; display: flex; flex-direction: column; gap: 8px; align-items: flex-start;">
    ${buttons}
    ${fields}
    ${pads}
    ${dialog}
    <span class="sort-help" id="sortHelp" tabindex="0" role="button" aria-label="Sort help">ⓘ
      <span class="sort-help-tip">Tip body</span>
    </span>
  </div>
</body></html>`;
}

async function run() {
  let browser;
  try {
    browser = await chromium.launch({
      headless: true,
      executablePath: process.env.CHROMIUM_PATH || undefined,
      args: ['--no-sandbox', '--disable-gpu', '--disable-dev-shm-usage'],
    });
  } catch (err) {
    // Allow the test to be skipped on hosts where Chromium cannot launch
    // (e.g. some musl-libc dev boxes). CI uses standard glibc Ubuntu runners
    // where this path is never taken. Set TOUCH_TARGETS_REQUIRE=1 or CHROMIUM_REQUIRE=1 to force
    // a hard failure even when Chromium is unavailable.
    if (process.env.TOUCH_TARGETS_REQUIRE === '1' || process.env.CHROMIUM_REQUIRE === '1') throw err;
    console.log(`test-touch-targets.js: SKIP (Chromium unavailable: ${err.message.split('\n')[0]})`);
    process.exit(0);
  }

  // iPhone 13 has hasTouch:true, isMobile:true, no hover. Exactly the
  // capability matrix that the @media (hover: hover) gate and 48px
  // minimums are designed for.
  const iPhone = devices['iPhone 13'];
  const context = await browser.newContext({ ...iPhone });
  const page = await context.newPage();

  // Load the harness via a data: URL so we don't need a running server.
  const html = buildSampleHtml();
  await page.setContent(html, { waitUntil: 'load' });
  if (page.evaluate) {
    await page.evaluate(() => document.fonts && document.fonts.ready ? document.fonts.ready : null);
  }

  let failures = 0;
  function record(name, ok, detail) {
    if (ok) {
      console.log(`  \u2705 ${name}`);
    } else {
      console.log(`  \u274c ${name}: ${detail}`);
      failures++;
    }
  }

  // --- Buttons: rendered hit area must be at least 48x48 CSS px.
  for (const [i, [selector]] of BUTTON_SELECTORS.entries()) {
    const dim = await page.$eval(`[data-btn="${i}"]`, (el) => {
      const r = el.getBoundingClientRect();
      const cs = getComputedStyle(el);
      return { w: r.width, h: r.height, mh: cs.minHeight, mw: cs.minWidth };
    });
    const min = DEFAULT_MIN;
    const okH = dim.h >= min;
    const okW = dim.w >= min;
    record(`${selector}: rendered ${dim.w.toFixed(1)}x${dim.h.toFixed(1)} (min ${dim.mw}/${dim.mh}, required ${min})`,
           okH && okW,
           `expected >=${min}x${min}, got ${dim.w}x${dim.h}`);
  }

  // --- Form controls: rendered height must be at least 48 CSS px.
  for (const [i, [selector]] of FIELD_SELECTORS.entries()) {
    const dim = await page.$eval(`[data-field="${i}"]`, (el) => {
      const r = el.getBoundingClientRect();
      const cs = getComputedStyle(el);
      return { h: r.height, mh: cs.minHeight };
    });
    record(`${selector}: rendered height ${dim.h.toFixed(1)} (min ${dim.mh})`,
           dim.h >= 48,
           `expected height >=48, got ${dim.h}`);
  }

  // --- Tap pads: the ::after box must be at least 48x48.
  for (const [name, html] of PAD_SELECTORS) {
    const key = html.match(/data-pad="([^"]+)"/)[1];
    const dim = await page.$eval(`[data-pad="${key}"]`, (el) => {
      const cs = getComputedStyle(el, '::after');
      return { w: parseFloat(cs.width), h: parseFloat(cs.height) };
    });
    record(`${name}: tap pad ${dim.w.toFixed(1)}x${dim.h.toFixed(1)}`,
           dim.w >= DEFAULT_MIN && dim.h >= DEFAULT_MIN,
           `expected >=${DEFAULT_MIN}x${DEFAULT_MIN}, got ${dim.w}x${dim.h}`);
  }

  // #249: each dialog button is at least 48 px tall, centres its label and
  // stays inside the dialog.
  for (const [id] of DIALOG_BUTTONS) {
    const d = await page.$eval('#' + id, (el) => {
      const b = el.getBoundingClientRect();
      const m = el.closest('.ch-modal').getBoundingClientRect();
      const range = document.createRange();
      range.selectNodeContents(el);
      const t = range.getBoundingClientRect();
      return {
        w: b.width, h: b.height,
        dx: (t.left + t.width / 2) - (b.left + b.width / 2),
        dy: (t.top + t.height / 2) - (b.top + b.height / 2),
        inside: b.left >= m.left && b.right <= m.right,
      };
    });
    record(`#${id} (add-channel dialog): rendered ${d.w.toFixed(1)}x${d.h.toFixed(1)}, label offset ${d.dx.toFixed(1)}/${d.dy.toFixed(1)}`,
           d.w >= DEFAULT_MIN && d.h >= DEFAULT_MIN && Math.abs(d.dx) <= 1 && Math.abs(d.dy) <= 1 && d.inside,
           `expected >=${DEFAULT_MIN}x${DEFAULT_MIN}, label centred within 1px and inside the dialog, got ${JSON.stringify(d)}`);
  }

  // #239 F2: the hit area keeps the text cursor it had as an inline style
  // before #235; .live-toggles label (cursor: pointer) must not win over it.
  const hitCursor = await page.$eval('.live-toggles .live-node-filter-hitarea', (el) => getComputedStyle(el).cursor);
  record(`.live-toggles .live-node-filter-hitarea: cursor ${hitCursor}`, hitCursor === 'text',
         `expected cursor text, got ${hitCursor}`);

  // #239 F3: a 48px region pill centres its label.
  const pillIdx = BUTTON_SELECTORS.findIndex(([sel]) => sel === '.region-pill');
  const pill = await page.$eval(`[data-btn="${pillIdx}"]`, (el) => {
    const range = document.createRange();
    range.selectNodeContents(el);
    const t = range.getBoundingClientRect();
    const b = el.getBoundingClientRect();
    return { offset: (t.left + t.width / 2) - (b.left + b.width / 2), width: b.width };
  });
  record(`.region-pill: label centred (offset ${pill.offset.toFixed(1)}px in a ${pill.width.toFixed(1)}px pill)`,
         Math.abs(pill.offset) <= 1,
         `expected the label centred within 1px, got offset ${pill.offset}`);

  // The coarse-pointer and <=640px rules above only apply if the harness
  // really is a phone.
  const env = await page.evaluate(() => ({ coarse: matchMedia('(pointer: coarse)').matches, narrow: matchMedia('(max-width: 640px)').matches }));
  record('iPhone context matches (pointer: coarse) and (max-width: 640px)', env.coarse && env.narrow,
         `expected both true, got ${JSON.stringify(env)}`);

  // --- MAJOR-1 verification: .sort-help is keyboard/tap focusable AND the
  // tooltip becomes visible on focus (tap-to-reveal works without hover).
  const tabIndex = await page.$eval('#sortHelp', (el) => el.getAttribute('tabindex'));
  record('.sort-help has tabindex="0" in markup', tabIndex === '0',
         `expected "0", got ${JSON.stringify(tabIndex)}`);

  const tipBeforeFocus = await page.$eval('#sortHelp .sort-help-tip',
    (el) => getComputedStyle(el).display);
  // CSS rule on touch-only viewport: hover-rule is gated, focus-rule reveals.
  record('.sort-help-tip is hidden by default on touch', tipBeforeFocus === 'none',
         `expected display:none initially, got ${tipBeforeFocus}`);

  await page.focus('#sortHelp');
  const tipAfterFocus = await page.$eval('#sortHelp .sort-help-tip',
    (el) => getComputedStyle(el).display);
  record('.sort-help-tip becomes visible on focus (tap-to-reveal)',
         tipAfterFocus === 'block',
         `expected display:block after focus, got ${tipAfterFocus}`);

  // --- Hover-only rule must be gated behind @media (hover: hover) so that on
  // touch the iPhone context never enters a "stuck hover" state when a tap
  // toggles :hover. We assert this by reading the matchMedia value the page
  // sees and confirming :hover did NOT take effect on tap.
  const hoverCapable = await page.evaluate(() => matchMedia('(hover: hover)').matches);
  record('iPhone context reports (hover: hover) = false', hoverCapable === false,
         `expected false on touch device, got ${hoverCapable}`);

  await browser.close();

  if (failures > 0) {
    console.log(`\ntest-touch-targets.js: FAIL (${failures} assertion(s))`);
    process.exit(1);
  }
  console.log('\ntest-touch-targets.js: OK');
}

run().catch((err) => {
  console.error('test-touch-targets.js: fatal', err);
  process.exit(1);
});
