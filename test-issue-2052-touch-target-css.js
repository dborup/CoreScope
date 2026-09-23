#!/usr/bin/env node
/* Issue #2052 — keep the nav and channel icon touch-target policy
 * unambiguous. Both controls render at 44x44 CSS px today, but earlier
 * duplicate 48px declarations made the source disagree with the cascade.
 *
 * This test reads the shipped stylesheet and requires one consistent value
 * across each selector's base declarations. Pseudo-class rules such as
 * :hover/:active are excluded because they do not define target dimensions.
 */
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

const css = fs.readFileSync(path.join(__dirname, 'public', 'style.css'), 'utf8')
  .replace(/\/\*[\s\S]*?\*\//g, '');

function policyDeclarations(selector) {
  const declarations = [];
  const rulePattern = /([^{}]+)\{([^{}]*)\}/g;
  let match;

  while ((match = rulePattern.exec(css)) !== null) {
    const selectors = match[1].split(',').map((part) => part.trim());
    if (!selectors.includes(selector)) continue;

    for (const property of ['min-width', 'min-height', 'touch-action']) {
      const propertyPattern = new RegExp(`(?:^|;)\\s*${property}\\s*:\\s*([^;]+)`, 'g');
      let propertyMatch;
      while ((propertyMatch = propertyPattern.exec(match[2])) !== null) {
        declarations.push({ property, value: propertyMatch[1].trim() });
      }
    }
  }

  return declarations;
}

for (const selector of ['.nav-btn', '.ch-icon-btn']) {
  const declarations = policyDeclarations(selector);
  const valuesByProperty = Object.fromEntries(['min-width', 'min-height', 'touch-action'].map((property) => [
    property,
    [...new Set(declarations.filter((entry) => entry.property === property).map((entry) => entry.value))],
  ]));
  assert.deepStrictEqual(
    valuesByProperty,
    { 'min-width': ['44px'], 'min-height': ['44px'], 'touch-action': ['manipulation'] },
    `${selector} must have one unambiguous 44px touch-target policy; got ${JSON.stringify(declarations)}`
  );
  console.log(`PASS ${selector}: one consistent 44x44 minimum`);
}

console.log('test-issue-2052-touch-target-css.js: OK');
