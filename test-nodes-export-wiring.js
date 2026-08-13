/**
 * Wiring tests for the Nodes JSON export: index.html loads nodes-export.js,
 * nodes.js renders an Export JSON button in the topbar that hands the
 * currently displayed node order + active area key to NodesExport.download(),
 * and — the #1889 review's core structural fix — renderRows() and the
 * export handler both call the SAME computeDisplayOrder() helper rather
 * than each having their own copy of the sort/claimed/favorites-pinning
 * logic.
 *
 * Pure source-string assertions (no browser); mapping/validation behavior
 * lives in test-nodes-export.js, shared-order behavior in
 * test-nodes-export-order.js, DOM behavior in test-nodes-export-e2e.js.
 */
'use strict';

const fs = require('fs');
const path = require('path');

let passed = 0;
let failed = 0;
function assert(cond, msg) {
  if (cond) { passed++; console.log('  ✓ ' + msg); }
  else { failed++; console.error('  ✗ ' + msg); }
}

const html = fs.readFileSync(path.join(__dirname, 'public/index.html'), 'utf8');
const src = fs.readFileSync(path.join(__dirname, 'public/nodes.js'), 'utf8');

console.log('\n=== nodes-export.js is loaded by the SPA shell ===');
assert(/<script src="nodes-export\.js\?v=__BUST__"/.test(html),
  'index.html loads nodes-export.js with the __BUST__ cache buster');
const exportTagIdx = html.indexOf('src="nodes-export.js');
const nodesTagIdx = html.indexOf('src="nodes.js');
assert(exportTagIdx > 0 && nodesTagIdx > 0 && exportTagIdx < nodesTagIdx,
  'nodes-export.js is loaded before nodes.js');

console.log('\n=== Nodes topbar renders the export button ===');
assert(/id="nodesExportBtn"/.test(src), 'nodes.js renders an element with id nodesExportBtn');
const topbarIdx = src.indexOf('nodes-topbar');
const topbarBlock = src.substring(topbarIdx, src.indexOf('</div>', src.indexOf('nodesAreaFilter')));
assert(topbarIdx > 0 && /nodesExportBtn/.test(topbarBlock),
  'the export button sits in the nodes topbar');

console.log('\n=== Single shared order helper — no duplicated sort/pin logic (#1889 review fix 1) ===');
const helperDeclIdx = src.indexOf('function computeDisplayOrder(');
assert(helperDeclIdx > 0, 'nodes.js defines a single computeDisplayOrder() helper');
// The helper itself is the only place allowed to build the claimed/favorites
// stable re-sort — count occurrences of the pinning comparator's tell-tale
// shape (aMy/bMy claimed-then-favorite comparison) across the whole file;
// it must appear exactly once (inside the helper), not once per call site.
const pinningComparatorMatches = src.match(/const aMy = myKeys\.has\(a\.public_key\)/g) || [];
assert(pinningComparatorMatches.length === 1,
  'the claimed/favorites pinning comparator exists in exactly one place in nodes.js (got ' + pinningComparatorMatches.length + ')');

const renderRowsIdx = src.indexOf('function renderRows()');
assert(renderRowsIdx > helperDeclIdx, 'computeDisplayOrder() is defined before renderRows()');
const renderRowsBlock = src.substring(renderRowsIdx, renderRowsIdx + 1200);
assert(/const sorted = computeDisplayOrder\(nodes\)/.test(renderRowsBlock),
  'renderRows() computes its row order via computeDisplayOrder(nodes), not an inline copy');

console.log('\n=== Click handler exports the shared display order for the active area ===');
const handlerIdx = src.indexOf("getElementById('nodesExportBtn')");
assert(handlerIdx > 0, 'found the nodesExportBtn handler block');
const handlerBlock = src.substring(handlerIdx, handlerIdx + 600);
assert(/NodesExport\.download\s*\(/.test(handlerBlock),
  'handler calls NodesExport.download(...)');
assert(/NodesExport\.download\(\s*computeDisplayOrder\(\s*nodes\s*\)\s*,/.test(handlerBlock),
  'handler passes computeDisplayOrder(nodes) — the same helper renderRows() uses — not the raw unsorted `nodes` array');
assert(/AreaFilter\.getSelected\(\)/.test(handlerBlock),
  'handler passes the active area key from AreaFilter.getSelected()');

console.log('\n=== Empty list disables the button ===');
assert(/function updateExportBtn\(\)/.test(src), 'nodes.js defines updateExportBtn()');
const updateBtnIdx = src.indexOf('function updateExportBtn()');
const updateBtnBlock = src.substring(updateBtnIdx, updateBtnIdx + 400);
assert(/\.disabled\s*=\s*n\s*===\s*0/.test(updateBtnBlock),
  'the export button is disabled exactly when the exportable contact count is 0');
assert(/updateExportBtn\(\);/.test(src.substring(src.indexOf('async function loadNodes'), src.indexOf('async function loadNodes') + 8000)),
  'loadNodes() calls updateExportBtn() so the button count/disabled-state stays in sync with the visible list');

console.log('\n=== Test hook exposed for order-sharing tests ===');
assert(/window\._nodesComputeDisplayOrder = computeDisplayOrder;/.test(src),
  'computeDisplayOrder is exposed as a test hook (used by test-nodes-export-order.js)');

console.log('\n' + passed + ' passed, ' + failed + ' failed');
process.exit(failed ? 1 : 0);
