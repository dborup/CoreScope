#!/usr/bin/env node
/* Issue #354 — My Channels rows are compact; Share and Remove live in an
 * overflow menu. Unit tests for the row markup, the menu markup and the
 * keyboard-navigation logic, evaluated from the real public/channels.js
 * (no copies). Browser behaviour is covered by
 * test-issue-354-compact-my-channels-e2e.js.
 */
'use strict';
const fs = require('fs');
const path = require('path');
const vm = require('vm');

let passed = 0, failed = 0;
function assert(c, m) {
  if (c) { passed++; console.log('  ✓ ' + m); } else { failed++; console.error('  ✗ ' + m); }
}

const chSrc = fs.readFileSync(path.join(__dirname, 'public/channels.js'), 'utf8');
const cssSrc = fs.readFileSync(path.join(__dirname, 'public/style.css'), 'utf8');

function extractFn(src, header) {
  const start = src.indexOf(header);
  if (start < 0) return null;
  let depth = 0;
  for (let j = src.indexOf('{', start); j < src.length; j++) {
    if (src[j] === '{') depth++;
    else if (src[j] === '}' && --depth === 0) return src.substring(start, j + 1);
  }
  return null;
}

const sandbox = {
  escapeHtml: s => String(s == null ? '' : s).replace(/[&<>"']/g, c => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c])),
  truncate: (s, n) => (s && s.length > n ? s.substring(0, n) + '…' : s || ''),
  formatSecondsAgo: () => '5m',
  formatHashHex: h => h,
  getChannelColor: () => '#fff',
  selectedHash: null,
  openRowMenuHash: null,
  window: {},
};
vm.createContext(sandbox);
vm.runInContext('const PRIVATE_CHANNEL_LABEL = "Private Channel";', sandbox);
for (const h of ['function channelDisplayName(ch', 'function renderUnreadBadge(ch)',
  'function renderChannelRow(ch)', 'function renderRowMenu(', 'function rowMenuNextIndex(']) {
  const fn = extractFn(chSrc, h);
  assert(fn, 'channels.js defines ' + h.replace('function ', '').replace(/\($/, ''));
  if (fn) vm.runInContext(fn, sandbox);
}

console.log('\n=== Row markup: one compact overflow trigger, no inline actions ===');
const row = sandbox.renderChannelRow && sandbox.renderChannelRow({
  hash: 'user:Crew', name: 'Crew', userAdded: true, encrypted: true,
  messageCount: 0, lastActivityMs: Date.now() });
assert(row && /data-row-menu="user:Crew"/.test(row), 'saved channel row carries a row-menu trigger');
assert(row && /aria-haspopup="menu"/.test(row) && /aria-expanded="false"/.test(row),
  'trigger announces a menu and starts collapsed');
assert(row && /aria-label="Actions for Crew"/.test(row), 'trigger has an accessible name that includes the channel');
assert(row && /role="button"/.test(row) && !/<button[^>]*data-row-menu/.test(row),
  'trigger is a span[role=button], never a nested <button> (#1033)');
assert(row && !/data-share-channel/.test(row) && !/data-remove-channel/.test(row),
  'row no longer renders inline Share/Remove controls');
assert(row && !/ch-share-btn|ch-remove-btn/.test(row), 'row has no ch-share-btn / ch-remove-btn chips');
assert(row && /ch-user-badge/.test(row) && /ch-item-name/.test(row) && /ch-item-time/.test(row),
  'name, key badge and activity time stay visible');
const netRow = sandbox.renderChannelRow && sandbox.renderChannelRow({
  hash: 'abc', name: '#test', userAdded: false, encrypted: false, messageCount: 3, lastActivityMs: 1 });
assert(netRow && !/data-row-menu/.test(netRow), 'network rows get no menu trigger');
const encRow = sandbox.renderChannelRow && sandbox.renderChannelRow({
  hash: 'ff', name: 'Net', userAdded: false, encrypted: true, messageCount: 0, lastActivityMs: 0 });
assert(encRow && !/data-row-menu/.test(encRow), 'encrypted (unkeyed) rows get no menu trigger');

console.log('\n=== Row menu markup ===');
const menu = sandbox.renderRowMenu && sandbox.renderRowMenu('user:Crew', 'Crew');
assert(menu && (menu.match(/role="menuitem"/g) || []).length === 2, 'menu has exactly two menu items');
assert(menu && /data-share-channel="user:Crew"/.test(menu) && /data-remove-channel="user:Crew"/.test(menu),
  'items keep the data-share-channel / data-remove-channel hooks');
assert(menu && menu.indexOf('data-share-channel') < menu.indexOf('data-remove-channel'),
  'Share comes first, Remove last (destructive item is furthest from the default)');
assert(menu && /ch-row-menu-danger[^>]*data-remove-channel|data-remove-channel[^>]*ch-row-menu-danger/.test(menu),
  'Remove is marked as the destructive item');
assert(menu && /aria-haspopup="dialog"[^>]*data-share-channel|data-share-channel[^>]*aria-haspopup="dialog"/.test(menu),
  'Share still announces it opens a dialog');
assert(menu && /tabindex="-1"/.test(menu), 'items are focus-managed (roving tabindex), not in the tab order');
const evil = sandbox.renderRowMenu && sandbox.renderRowMenu('user:"><img onerror=x>', '"><img onerror=x>');
assert(evil && !/<img/.test(evil), 'hash and name are escaped in the menu');

console.log('\n=== Keyboard navigation logic ===');
const nav = sandbox.rowMenuNextIndex;
assert(nav && nav('ArrowDown', 0, 2) === 1, 'ArrowDown moves to the next item');
assert(nav && nav('ArrowDown', 1, 2) === 0, 'ArrowDown wraps to the first item');
assert(nav && nav('ArrowUp', 0, 2) === 1, 'ArrowUp wraps to the last item');
assert(nav && nav('ArrowUp', 1, 2) === 0, 'ArrowUp moves to the previous item');
assert(nav && nav('Home', 1, 2) === 0 && nav('End', 0, 2) === 1, 'Home / End jump to the ends');
assert(nav && nav('a', 0, 2) === -1 && nav('Tab', 0, 2) === -1, 'other keys are not handled');
assert(nav && nav('ArrowDown', -1, 2) === 0 && nav('ArrowUp', -1, 2) === 1, 'no current item: Down -> first, Up -> last');
assert(nav && nav('ArrowDown', 0, 0) === -1, 'empty menu handles nothing');

console.log('\n=== Wiring in channels.js ===');
assert(/id="chRowMenu"/.test(chSrc), 'init() renders the shared #chRowMenu inside the page subtree (torn down with it)');
assert(/role="menu"[^>]*id="chRowMenu"|id="chRowMenu"[^>]*role="menu"/.test(chSrc), '#chRowMenu has role="menu"');
assert(/removeUserChannelKey\(/.test(chSrc) && /confirm\(/.test(chSrc),
  'Remove still goes through a confirm() before deleting the key');

console.log('\n=== CSS ===');
function rules(sel) {
  const out = [];
  const re = new RegExp('(^|\\n)\\s*' + sel.replace(/[.*+?^${}()|[\]\\]/g, '\\$&') + '[^{,]*\\{[^}]*\\}', 'g');
  let m; while ((m = re.exec(cssSrc))) out.push(m[0]);
  return out.join('\n');
}
const menuCss = rules('.ch-row-menu') + rules('.ch-row-menu-btn') + rules('.ch-row-menu-item');
assert(/\.ch-row-menu\b/.test(cssSrc), 'stylesheet defines .ch-row-menu');
assert(menuCss.length > 0 && !/#[0-9a-fA-F]{3,8}\b(?![^)]*\))/.test(menuCss.replace(/var\([^)]*\)/g, '')),
  'menu rules use CSS variables only, no hardcoded hex colours');
assert(/\.ch-row-menu-btn[^{]*\{[^}]*margin:[^}]*-/.test(cssSrc),
  'trigger keeps its 48px target but is pulled out of the row height with negative margins');

console.log('\nPassed: ' + passed + ', Failed: ' + failed);
process.exit(failed > 0 ? 1 : 0);
