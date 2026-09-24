#!/usr/bin/env node
/* Issue #2052 — static touch-target policy for the compact controls.
 *
 * Supplement only: test-issue-2052-touch-target-e2e.js measures the rendered
 * boxes in a browser and is the real guarantee.
 *
 * This test reads the shipped stylesheet and, for every rule anywhere in it
 * (including inside @media blocks), looks at each selector whose last compound
 * targets .nav-btn, .ch-icon-btn, .ch-share-btn or .ch-remove-btn. More
 * specific selectors such as `#chList .ch-icon-btn` count too; an earlier
 * version matched only the exact selector text and missed a 32px override.
 * Every min-width/min-height declared for these controls must be at least
 * 44px. Pseudo-class rules (:hover, :active, :focus, ...) are skipped because
 * they do not set target dimensions. The base .nav-btn and .ch-icon-btn rules
 * must also keep touch-action: manipulation.
 */
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

const css = fs.readFileSync(path.join(__dirname, 'public', 'style.css'), 'utf8')
  .replace(/\/\*[\s\S]*?\*\//g, '');

const CONTROLS = ['.nav-btn', '.ch-icon-btn', '.ch-share-btn', '.ch-remove-btn'];

// The innermost { } blocks are the style rules, also inside @media.
function rules() {
  const out = [];
  const rulePattern = /([^{}]+)\{([^{}]*)\}/g;
  let match;
  while ((match = rulePattern.exec(css)) !== null) {
    out.push({ selectors: match[1].split(',').map((s) => s.trim()), body: match[2] });
  }
  return out;
}

// The compact control a selector targets, or null. Only the last compound
// counts (`.ch-icon-btn .ph-icon` targets the icon, not the button).
function targetedControl(selector) {
  const last = selector.split(/[\s>+~]+/).filter(Boolean).pop() || '';
  if (/:/.test(last)) return null;
  return CONTROLS.find((c) => new RegExp('\\' + c + '(?![\\w-])').test(last)) || null;
}

function declared(body, property) {
  const values = [];
  const pattern = new RegExp(`(?:^|;)\\s*${property}\\s*:\\s*([^;]+)`, 'g');
  let m;
  while ((m = pattern.exec(body)) !== null) values.push(m[1].trim());
  return values;
}

const violations = [];
const seen = new Set();
let checked = 0;
for (const rule of rules()) {
  for (const selector of rule.selectors) {
    const control = targetedControl(selector);
    if (!control) continue;
    seen.add(control);
    for (const property of ['min-width', 'min-height']) {
      for (const value of declared(rule.body, property)) {
        checked++;
        const px = /^(\d+(?:\.\d+)?)px$/.exec(value);
        if (!px || Number(px[1]) < 44) violations.push(`${selector} { ${property}: ${value} }`);
      }
    }
  }
}

assert.deepStrictEqual(CONTROLS.filter((c) => !seen.has(c)), [], 'every compact control must have at least one rule');
assert(checked > 0, 'no min-width/min-height declarations found for the compact controls');
assert.deepStrictEqual(violations, [], 'compact controls must not declare a touch target below 44px:\n  ' + violations.join('\n  '));
console.log(`PASS ${checked} min-width/min-height declarations for ${CONTROLS.join(', ')} are all >= 44px`);

for (const base of ['.nav-btn', '.ch-icon-btn']) {
  const values = rules()
    .filter((r) => r.selectors.includes(base))
    .flatMap((r) => declared(r.body, 'touch-action'));
  assert.deepStrictEqual([...new Set(values)], ['manipulation'], `${base} must keep touch-action: manipulation; got ${JSON.stringify(values)}`);
  console.log(`PASS ${base}: touch-action manipulation`);
}

console.log('test-issue-2052-touch-target-css.js: OK');
