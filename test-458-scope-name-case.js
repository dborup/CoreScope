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

// All rules (in file order) whose selector list names `cls` as a class token.
function rulesFor(cls) {
  const re = /([^{}]+)\{([^}]*)\}/g;
  const tokenRe = new RegExp('\\.' + cls.replace(/[-]/g, '\\-') + '(?![\\w-])');
  const out = [];
  let m;
  while ((m = re.exec(css))) {
    const sel = m[1].replace(/\/\*[\s\S]*?\*\//g, '');
    if (sel.split(',').some(s => tokenRe.test(s))) out.push({ selector: sel.trim(), body: m[2] });
  }
  return out;
}
function textTransforms(rules) {
  const vals = [];
  rules.forEach(r => { const re = /text-transform:\s*([a-z-]+)/g; let m; while ((m = re.exec(r.body))) vals.push(m[1]); });
  return vals;
}

function assertCaseSafe(cls) {
  const rules = rulesFor(cls);
  assert(rules.length, '.' + cls + ' rule missing');
  const vals = textTransforms(rules);
  assert(vals.length, '.' + cls + ' must declare text-transform');
  assert(vals.every(v => v === 'none'), '.' + cls + ' has non-none text-transform in some rule: ' + vals.join(','));
}
function assertNoHardcodedColors(cls) {
  rulesFor(cls).forEach(r => {
    const noVarFallbacks = r.body.replace(/var\([^)]*\)/g, '');
    assert(!/#[0-9a-fA-F]{3,8}\b|rgba?\(|hsla?\(/.test(noVarFallbacks), r.selector + ' has hardcoded color outside var(): ' + noVarFallbacks.trim());
  });
}

test('.badge-scope preserves case in every rule (no later uppercase override)', () => assertCaseSafe('badge-scope'));
test('.badge-transport preserves case in every rule (#458 F1)', () => assertCaseSafe('badge-transport'));

test('generic .badge still uppercases (unchanged)', () => {
  const body = ruleBody('\\.badge');
  assert(/text-transform:\s*uppercase/.test(body));
});

test('scope badge colors use CSS variables only (no hardcoded colors in any rule)', () => {
  assert(/var\(--/.test(ruleBody('\\.badge-scope')));
  assertNoHardcodedColors('badge-scope');
  assertNoHardcodedColors('badge-scope-unknown');
});

test('Configured scope value is exact case: plain <code>, no badge, no transform', () => {
  const l = line('id="row-configured-scope"');
  const m = l.match(/<code([^>]*)>\$\{escapeHtml\(n\.configured_scope\)\}<\/code>/);
  assert(m, 'expected <code> wrapping escapeHtml(n.configured_scope)');
  assert(!/text-transform\s*:\s*(?!none)/.test(m[1]), 'inline text-transform on <code>');
  assert(!/class=/.test(m[1]), '<code> must not carry a class (e.g. badge)');
  const codeVals = textTransforms(css.split(/(?<=\})/).map(x => x.trim()).filter(x => /^(?:[^{]*[\s,])?code\s*(?:,[^{]*)?\{/.test(x) || /^code\s*\{/.test(x)).map(x => ({ body: x.slice(x.indexOf('{') + 1) })));
  assert(codeVals.every(v => v === 'none'), 'a bare `code` rule transforms case: ' + codeVals.join(','));
  assert(!/class="badge"/.test(l), 'Configured scope row must not use class="badge"');
});

console.log(`\n${passed} passed, ${failed} failed`);
process.exit(failed ? 1 : 0);
