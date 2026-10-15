/**
 * #458: scope names (region names hashed over the air) must be displayed with
 * their exact case. The generic `.badge` class uppercases text, so scope values
 * in the node detail must use `.badge-scope`, which must not transform case.
 */
'use strict';
const fs = require('fs');
const assert = require('assert');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log(`  ✅ ${name}`); }
  catch (e) { failed++; console.log(`  ❌ ${name}: ${e.message}`); }
}

const nodes = fs.readFileSync(__dirname + '/public/nodes.js', 'utf8');
const css = fs.readFileSync(__dirname + '/public/style.css', 'utf8');

function ruleBody(selectorRe) {
  const m = css.match(new RegExp('(?:^|\\n)' + selectorRe + '\\s*\\{([^}]*)\\}'));
  return m ? m[1] : null;
}
function line(marker) {
  const l = nodes.split('\n').find(x => x.includes(marker));
  assert(l, 'marker not found: ' + marker);
  return l;
}

test('Own scope value uses badge-scope, not the uppercasing .badge', () => {
  const l = line("n.default_scope === null ?");
  assert(/class="badge-scope"[^>]*>' \+ escapeHtml\(n\.default_scope\)/.test(l), 'expected badge-scope span');
  assert(!/class="badge"/.test(l), 'must not use class="badge"');
});

test('Transported scopes use badge-scope, not the uppercasing .badge', () => {
  const l = line('id="row-transported-scopes"');
  assert(/class="badge-scope"[^>]*>' \+ escapeHtml\(String\(sc\)\)/.test(l), 'expected badge-scope span');
  assert(!/class="badge"/.test(l), 'must not use class="badge"');
});

test('.badge-scope explicitly preserves case', () => {
  const body = ruleBody('\\.badge-scope');
  assert(body, '.badge-scope rule missing');
  assert(/text-transform:\s*none/.test(body), 'text-transform:none required');
});

test('generic .badge still uppercases (unchanged)', () => {
  const body = ruleBody('\\.badge');
  assert(/text-transform:\s*uppercase/.test(body));
});

test('scope badge colors use CSS variables only', () => {
  const body = ruleBody('\\.badge-scope');
  assert(/var\(--/.test(body));
});

console.log(`\n${passed} passed, ${failed} failed`);
process.exit(failed ? 1 : 0);
