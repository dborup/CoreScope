'use strict';
/* Issue #715 — the og:image Denmark outline is generated from Natural Earth
 * (scripts/gen-og-denmark.py) and the nodes/links sit on that land. Static
 * checks on scripts/og-image-meshview.html; the PNG size is covered by
 * test-issue-646-og-meta.js. */
const assert = require('assert');
const fs = require('fs');
const path = require('path');
const { execSync } = require('child_process');

let pass = 0, fail = 0;
function test(name, fn) {
  try { fn(); pass++; console.log('  ✓ ' + name); }
  catch (e) { fail++; console.log('  ✗ ' + name + '\n    ' + e.message); }
}

const src = fs.readFileSync(path.join(__dirname, 'scripts', 'og-image-meshview.html'), 'utf8');
const genPath = path.join(__dirname, 'scripts', 'gen-og-denmark.py');
const gen = fs.existsSync(genPath) ? fs.readFileSync(genPath, 'utf8') : '';
const svg = src.slice(src.indexOf('<svg'), src.indexOf('</svg>'));

// Parse absolute M/L/Z path data into rings [[x,y],...].
function rings(d) {
  const out = []; let cur = null;
  const re = /([MLZ])([^MLZ]*)/g; let m;
  while ((m = re.exec(d))) {
    if (m[1] === 'Z') { if (cur) out.push(cur); cur = null; continue; }
    const n = m[2].trim().split(/[\s,]+/).map(Number);
    if (m[1] === 'M') { if (cur) out.push(cur); cur = []; }
    for (let i = 0; i + 1 < n.length; i += 2) cur.push([n[i], n[i + 1]]);
  }
  if (cur) out.push(cur);
  return out;
}
function inside(pt, ring) {
  let c = false;
  for (let i = 0, j = ring.length - 1; i < ring.length; j = i++) {
    const [xi, yi] = ring[i], [xj, yj] = ring[j];
    if ((yi > pt[1]) !== (yj > pt[1]) && pt[0] < (xj - xi) * (pt[1] - yi) / (yj - yi) + xi) c = !c;
  }
  return c;
}
function bbox(rs) {
  const p = [].concat(...rs);
  const xs = p.map(a => a[0]), ys = p.map(a => a[1]);
  return { x0: Math.min(...xs), x1: Math.max(...xs), y0: Math.min(...ys), y1: Math.max(...ys) };
}
const outlineRings = [].concat(...[...svg.matchAll(/<path class="outline"[^>]*\sd="([^"]+)"/g)].map(m => rings(m[1])));
const nodes = [...svg.matchAll(/<circle class="node" cx="([\d.]+)" cy="([\d.]+)"/g)].map(m => [+m[1], +m[2]]);
const links = [...svg.matchAll(/<line class="link" x1="([\d.]+)" y1="([\d.]+)" x2="([\d.]+)" y2="([\d.]+)"/g)].map(m => m.slice(1).map(Number));

console.log('\n=== #715: Denmark outline from Natural Earth ===');
test('outline is real geodata: many vertices, many islands', () => {
  const verts = outlineRings.reduce((n, r) => n + r.length, 0);
  assert.ok(outlineRings.length >= 12, 'rings ' + outlineRings.length);
  assert.ok(verts >= 600 && verts <= 5000, 'vertices ' + verts);
});
test('mainland outline fills the right half (~400x500 px) without stretching', () => {
  const big = outlineRings.slice().sort((a, b) => b.length - a.length);
  const b = bbox(outlineRings.filter(r => bbox([r]).x1 - bbox([r]).x0 > 5 && !(bbox([r]).x0 > 1050 && bbox([r]).y0 > 500)));
  assert.ok(b.x0 >= 640 && b.x1 <= 1180 && b.y0 >= 20 && b.y1 <= 610, JSON.stringify(b));
  const w = b.x1 - b.x0, h = b.y1 - b.y0;
  assert.ok(w >= 330 && h >= 440, w + 'x' + h);
  // true aspect: 4.6 deg lon * cos(56deg) vs 3.2 deg lat = 0.80
  assert.ok(Math.abs(w / h - 0.80) < 0.08, 'aspect ' + (w / h));
  assert.ok(big.length > 0);
});
test('Bornholm sits in a framed inset', () => {
  const m = svg.match(/<rect class="inset" x="([\d.]+)" y="([\d.]+)" width="([\d.]+)" height="([\d.]+)"/);
  assert.ok(m, 'inset rect');
  const [x, y, w, h] = m.slice(1).map(Number);
  const inBox = outlineRings.filter(r => { const b = bbox([r]); return b.x0 >= x && b.x1 <= x + w && b.y0 >= y && b.y1 <= y + h; });
  assert.strictEqual(inBox.length, 1, 'rings in inset ' + inBox.length);
  assert.ok(nodes.some(n => inside(n, inBox[0])), 'a node on Bornholm');
});
test('every node sits on land', () => {
  assert.ok(nodes.length >= 13, 'nodes ' + nodes.length);
  for (const n of nodes) assert.ok(outlineRings.some(r => inside(n, r)), 'node off land: ' + n);
});
test('every link joins two nodes', () => {
  assert.ok(links.length >= 14, 'links ' + links.length);
  const has = p => nodes.some(n => Math.abs(n[0] - p[0]) < 0.01 && Math.abs(n[1] - p[1]) < 0.01);
  for (const l of links) assert.ok(has([l[0], l[1]]) && has([l[2], l[3]]), 'dangling link ' + l);
});
test('Jutland, Funen and Zealand each hold nodes', () => {
  const owner = n => outlineRings.map((r, i) => inside(n, r) ? i : -1).find(i => i >= 0);
  const owners = new Set(nodes.map(owner));
  assert.ok(owners.size >= 4, 'islands with nodes ' + owners.size);
});

console.log('\n=== #715: provenance ===');
test('HTML comment cites Natural Earth as public domain', () => {
  const c = src.slice(0, src.indexOf('<html')).concat(src.match(/<!--[\s\S]*?-->/g).join(''));
  assert.ok(/Natural Earth/.test(c) && /public domain/i.test(c));
  assert.ok(/gen-og-denmark\.py/.test(c));
  assert.ok(!/Hand-drawn/i.test(c), 'stale hand-drawn claim');
});
test('generator pins dependencies and cites source + licence', () => {
  assert.ok(gen, 'scripts/gen-og-denmark.py missing');
  assert.ok(/pyshp==\d/.test(gen) && /shapely==\d/.test(gen));
  assert.ok(/naturalearthdata\.com/.test(gen) && /public domain/i.test(gen));
  assert.ok(/ne_10m_admin_0_countries/.test(gen));
});
test('no raw shapefiles are committed', () => {
  const files = execSync('git ls-files', { cwd: __dirname }).toString().split('\n');
  assert.deepStrictEqual(files.filter(f => /\.(shp|shx|dbf|prj|cpg)$/i.test(f)), []);
});
test('SVG stays small', () => {
  assert.ok(svg.length < 40 * 1024, 'svg bytes ' + svg.length);
});

console.log('\n' + pass + ' passed, ' + fail + ' failed');
process.exit(fail ? 1 : 0);
