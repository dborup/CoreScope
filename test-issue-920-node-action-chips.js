/**
 * Unit tests (#920): the "Terrain profile" and "Oblique photo" node actions
 * use the compact chip style of Details, Analytics and Reach, through one
 * shared class, `node-action-chip`.
 *
 * public/terrain-profile.js and public/skraafoto.js are loaded in the same
 * vm sandbox the #737 suites use and the real nodeEntryAction() /
 * nodeAction() are called; public/style.css is read as shipped. Nothing is
 * a copy of production logic.
 *
 * Covered:
 *   1. both actions carry node-action-chip, and neither carries the old
 *      48 px node-terrain-action class;
 *   2. the labels are "Terrain profile" and "Oblique photo" (no "to…");
 *   3. behaviour and test ids are unchanged: a link for the profile, a
 *      button for the photo, the same data-testids, the same gates;
 *   4. the chip rule is compact (no 48 px box outside the phone query),
 *      keeps a 44 px target on phones, and uses CSS variables only;
 *   5. the old 48 px rule is gone;
 *   6. every built-in action in both rows (page: Copy URL, Copy short URL,
 *      Analytics, Reach, Observer →; pane: Details, Analytics, Reach)
 *      carries the same class and sets no size of its own inline, so the
 *      whole row shares one height on desktop and on phones; the rows do
 *      not stretch their chips, so a drifting chip cannot hide.
 *
 * Usage: node test-issue-920-node-action-chips.js
 */
'use strict';
const fs = require('fs');
const path = require('path');
const assert = require('assert');
const { loadTerrainProfile } = require('./tests/map-overlay-sandbox');
const { fakeDom } = require('./tests/skraafoto-fake-dom');

let passed = 0, failed = 0;
function test(name, fn) {
  try { fn(); passed++; console.log('  ✅ ' + name); }
  catch (e) { failed++; console.log('  ❌ ' + name + ': ' + e.message); }
}

const { TP, win } = loadTerrainProfile(['terrain-profile-cursor.js', 'skraafoto.js', 'heritage-info.js']);
const SF = win.Skraafoto;
assert.ok(TP && SF, 'TerrainProfile and Skraafoto must load');
win.document = fakeDom().doc;

const PK = 'a'.repeat(64);
const NODE = { public_key: PK, name: 'Hill', lat: 56.15674, lon: 10.21076 };
const DHM = { enabled: true, profile: true };
const SKF = {
  enabled: true, searchUrl: '/api/skraafoto/search',
  previewUrl: '/api/skraafoto/preview/{collection}/{item}', maxPx: 1024,
  bounds: [[54.4265, 7.99125], [57.778, 15.5995]],
};

const css = fs.readFileSync(path.join(__dirname, 'public', 'style.css'), 'utf8');
const nodesJs = fs.readFileSync(path.join(__dirname, 'public', 'nodes.js'), 'utf8');

/* The <a>/<button> opening tags inside the first <div> whose class attribute
 * starts with `cls` in public/nodes.js, up to that div's closing tag (the
 * rows hold no nested divs). */
function actionTags(cls) {
  const start = nodesJs.indexOf('<div class="' + cls + '"');
  assert.ok(start !== -1, 'no ' + cls + ' row in nodes.js');
  const end = nodesJs.indexOf('</div>', start);
  return nodesJs.slice(start, end).match(/<(?:a|button)\b[^>]*>/g) || [];
}
const attr = (tag, name) => {
  const m = new RegExp('\\s' + name + '="([^"]*)"').exec(tag);
  return m ? m[1] : '';
};

/* The declaration blocks of every rule whose selector list names `cls`,
 * with the at-rule (if any) each sits in. Brace-aware, one level of @media. */
function rulesFor(cls) {
  const out = [];
  let i = 0, media = null, depth = 0;
  const stack = [];
  const re = /([^{}]*)\{|\}/g;
  let m;
  const src = css.replace(/\/\*[\s\S]*?\*\//g, '');
  let bodyStart = -1, sel = '';
  while ((m = re.exec(src))) {
    if (m[0] === '}') {
      const top = stack.pop();
      if (top && top.kind === 'rule') {
        if (top.sel.split(',').some(s => s.indexOf('.' + cls) !== -1)) {
          out.push({ sel: top.sel.trim(), media: top.media, body: src.slice(top.start, m.index) });
        }
      }
      continue;
    }
    const head = m[1].trim();
    if (head.startsWith('@media')) stack.push({ kind: 'media', head });
    else if (head.startsWith('@')) stack.push({ kind: 'at', head });
    else {
      const med = stack.filter(s => s.kind === 'media').map(s => s.head).join(' and ');
      stack.push({ kind: 'rule', sel: head, media: med || null, start: re.lastIndex });
    }
  }
  return out;
}
const px = (body, prop) => {
  const m = new RegExp('(?:^|[;\\s])' + prop + '\\s*:\\s*([0-9.]+)px').exec(body);
  return m ? Number(m[1]) : null;
};
const classes = (e) => String(e.className).split(/\s+/);
// The label is the last child; the icon before it is static markup.
const textOf = (e) => e.children[e.children.length - 1].textContent;

console.log('\n=== #920 node action chips ===\n');

const tpAction = TP.nodeEntryAction(NODE, DHM);
const sfAction = SF.nodeAction(NODE, SKF);

test('both actions are offered for a node with a fix', () => {
  assert.ok(tpAction, 'no terrain action');
  assert.ok(sfAction, 'no photo action');
});

test('both carry the one shared class node-action-chip', () => {
  assert.ok(classes(tpAction).includes('node-action-chip'), 'terrain: ' + tpAction.className);
  assert.ok(classes(sfAction).includes('node-action-chip'), 'photo: ' + sfAction.className);
});

test('neither carries the old 48 px node-terrain-action class', () => {
  assert.ok(!classes(tpAction).includes('node-terrain-action'), tpAction.className);
  assert.ok(!classes(sfAction).includes('node-terrain-action'), sfAction.className);
});

test('both are btn-primary, like Details, Analytics and Reach', () => {
  assert.ok(classes(tpAction).includes('btn-primary'));
  assert.ok(classes(sfAction).includes('btn-primary'));
});

test('labels are "Terrain profile" and "Oblique photo"', () => {
  assert.strictEqual(textOf(tpAction), 'Terrain profile');
  assert.strictEqual(textOf(sfAction), 'Oblique photo');
});

test('behaviour and test ids are kept', () => {
  assert.strictEqual(tpAction.tagName, 'A');
  assert.strictEqual(tpAction.attrs.href, '#/map?profile=' + PK);
  assert.strictEqual(tpAction.attrs['data-testid'], 'node-terrain-profile');
  assert.strictEqual(sfAction.tagName, 'BUTTON');
  assert.strictEqual(sfAction.attrs['data-testid'], 'node-skraafoto');
  assert.ok(sfAction.title || sfAction.attrs.title, 'photo title kept');
  assert.ok(tpAction.attrs.title, 'terrain title kept');
});

test('gates are kept: no fix or switched off gives no action', () => {
  assert.strictEqual(TP.nodeEntryAction({ public_key: PK }, DHM), null);
  assert.strictEqual(TP.nodeEntryAction(NODE, { enabled: true }), null);
  assert.strictEqual(SF.nodeAction({ name: 'x' }, SKF), null);
  assert.strictEqual(SF.nodeAction(NODE, { enabled: false }), null);
});

test('the chip rule is compact: no 48 px box outside a phone query', () => {
  const rules = rulesFor('node-action-chip');
  assert.ok(rules.length, 'no .node-action-chip rule in style.css');
  rules.filter(r => !r.media).forEach(r => {
    const h = px(r.body, 'min-height'), w = px(r.body, 'min-width');
    assert.ok(h === null || h < 44, r.sel + ' min-height ' + h);
    assert.ok(w === null || w < 44, r.sel + ' min-width ' + w);
  });
});

test('on phones the chip is at least 44 px tall', () => {
  const phone = rulesFor('node-action-chip').filter(r => r.media && /max-width\s*:\s*768px/.test(r.media));
  assert.ok(phone.length, 'no phone rule for .node-action-chip');
  assert.ok(phone.some(r => px(r.body, 'min-height') >= 44), 'no min-height >= 44px at phone width');
});

test('the chip rule uses CSS variables only: no hex or rgb colours', () => {
  rulesFor('node-action-chip').forEach(r => {
    assert.ok(!/#[0-9a-fA-F]{3,8}\b|rgba?\(/.test(r.body), r.sel + ' hardcodes a colour: ' + r.body);
  });
});

test('the old 48 px node-terrain-action rule is gone', () => {
  assert.strictEqual(rulesFor('node-terrain-action').length, 0);
});

for (const [row, n] of [['node-full-actions', 5], ['node-detail-role', 3]]) {
  test(`${row}: all ${n} built-in actions carry node-action-chip`, () => {
    const tags = actionTags(row);
    assert.strictEqual(tags.length, n, tags.join('\n'));
    tags.forEach(t => assert.ok(attr(t, 'class').split(/\s+/).includes('node-action-chip'), t));
  });
  test(`${row}: no action sets its own size inline (font-size, padding, display)`, () => {
    actionTags(row).forEach(t => {
      assert.ok(!/font-size|padding|display/.test(attr(t, 'style')), t);
    });
  });
}

test('both rows align chips to the top, so a drifting chip is not stretched to match', () => {
  for (const row of ['node-full-actions', 'node-detail-actions']) {
    const rules = rulesFor(row).filter(r => !r.media && r.sel === '.' + row);
    assert.ok(rules.some(r => /align-items\s*:\s*flex-start/.test(r.body)), row + ' stretches its chips');
  }
});

test('the pane chip size applies to the whole pane, not only the module row', () => {
  const pane = rulesFor('node-action-chip').filter(r => !r.media && /\.node-detail\s/.test(r.sel));
  assert.ok(pane.length, 'no .node-detail .node-action-chip rule');
  assert.ok(pane.some(r => px(r.body, 'font-size') === 11), 'pane chips are not 11 px');
});

console.log(`\n${passed} passed, ${failed} failed`);
process.exit(failed ? 1 : 0);
