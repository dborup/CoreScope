'use strict';
/* Issue #739 M2 — the og:image Denmark outline comes from the same
 * Topografisk Landpolygon GeoPackage as the DHM coverage gate (#739 M1),
 * not from Natural Earth. Static checks on scripts/og-image-meshview.html,
 * the generator and public/og-image-meshview.png; no network, no GeoPackage
 * needed (the generator records its source summary in the generated block). */
const assert = require('assert');
const fs = require('fs');
const path = require('path');
const zlib = require('zlib');

let pass = 0, fail = 0;
function test(name, fn) {
  try { fn(); pass++; console.log('  ✓ ' + name); }
  catch (e) { fail++; console.log('  ✗ ' + name + '\n    ' + e.message); }
}
const read = (p) => fs.readFileSync(path.join(__dirname, p), 'utf8');

const src = read('scripts/og-image-meshview.html');
const gen = read('scripts/gen-og-denmark.py');
const genDhm = read('scripts/gen-dhm-foreign-land.py');
const foreignHeader = read('internal/dhm/foreign_land.txt').split('\n').filter(l => l.startsWith('#')).join('\n');
const svg = src.slice(src.indexOf('<svg'), src.indexOf('</svg>'));
const block = (src.match(/<!-- gen-og-denmark:begin[\s\S]*?<!-- gen-og-denmark:end -->/) || [''])[0];

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
function edgeDist(pt, ring) {
  let best = Infinity;
  for (let i = 0, j = ring.length - 1; i < ring.length; j = i++) {
    const [ax, ay] = ring[j], [bx, by] = ring[i];
    const dx = bx - ax, dy = by - ay, l2 = dx * dx + dy * dy;
    const t = l2 ? Math.max(0, Math.min(1, ((pt[0] - ax) * dx + (pt[1] - ay) * dy) / l2)) : 0;
    best = Math.min(best, Math.hypot(pt[0] - ax - t * dx, pt[1] - ay - t * dy));
  }
  return best;
}
function bbox(rs) {
  const p = [].concat(...rs);
  const xs = p.map(a => a[0]), ys = p.map(a => a[1]);
  return [Math.min(...xs), Math.min(...ys), Math.max(...xs), Math.max(...ys)];
}
const outlineRings = [].concat(...[...svg.matchAll(/<path class="outline"[^>]*\sd="([^"]+)"/g)].map(m => rings(m[1])));
const insetM = svg.match(/<rect class="inset" x="([\d.]+)" y="([\d.]+)" width="([\d.]+)" height="([\d.]+)"/);
const inset = insetM ? insetM.slice(1).map(Number) : [0, 0, 0, 0];
const inInset = r => { const b = bbox([r]); return b[0] >= inset[0] && b[2] <= inset[0] + inset[2] && b[1] >= inset[1] && b[3] <= inset[1] + inset[3]; };
const mainRings = outlineRings.filter(r => !inInset(r));

// key=value tokens the generator writes into the generated block.
function field(k) {
  const m = block.match(new RegExp('\\b' + k + '=(\\S+)'));
  return m ? m[1] : null;
}
function genConst(name) {
  const m = gen.match(new RegExp('^' + name + '\\s*=\\s*([-\\d.]+)', 'm'));
  if (m) return Number(m[1]);
  // ORIGIN_X, ORIGIN_Y = 712.0, 62.0
  const t = gen.match(new RegExp('^(\\w+), (\\w+) = ([-\\d.]+), ([-\\d.]+)', 'gm')) || [];
  for (const line of t) {
    const [, a, b, va, vb] = line.match(/^(\w+), (\w+) = ([-\d.]+), ([-\d.]+)/);
    if (a === name) return Number(va);
    if (b === name) return Number(vb);
  }
  throw new Error('generator constant ' + name);
}

// EPSG:25832 -> lat/lon: the Krüger series of internal/dhm/utm.go (inverse).
const fromUTM32 = (() => {
  const a = 6378137, f = 1 / 298.257222101, n = f / (2 - f), n2 = n * n, n3 = n2 * n, n4 = n3 * n;
  const e = Math.sqrt(f * (2 - f)), kA = 0.9996 * a / (1 + n) * (1 + n2 / 4 + n4 / 64);
  const beta = [n / 2 - 2 * n2 / 3 + 37 * n3 / 96 - n4 / 360, n2 / 48 + n3 / 15 - 437 * n4 / 1440,
    17 * n3 / 480 - 37 * n4 / 840, 4397 * n4 / 161280];
  return (E, N) => {
    const xi = N / kA, eta = (E - 500000) / kA;
    let xp = xi, ep = eta;
    beta.forEach((b, j) => { const k = 2 * (j + 1); xp -= b * Math.sin(k * xi) * Math.cosh(k * eta); ep -= b * Math.cos(k * xi) * Math.sinh(k * eta); });
    const chi = Math.asin(Math.sin(xp) / Math.cosh(ep));
    let phi = chi;
    for (let i = 0; i < 20; i++) { const s = e * Math.sin(phi); phi = 2 * Math.atan(Math.tan(Math.PI / 4 + chi / 2) * Math.pow((1 + s) / (1 - s), e / 2)) - Math.PI / 2; }
    return [phi * 180 / Math.PI, 9 + Math.atan2(Math.sinh(ep), Math.cos(xp)) * 180 / Math.PI];
  };
})();

// The generator's own projection (constants read from it, not copied).
function project(lat, lon) {
  const ppd = genConst('PX_PER_DEG'), cos = Math.cos(56 * Math.PI / 180);
  return [genConst('ORIGIN_X') + (lon - genConst('LON_LEFT')) * cos * ppd, genConst('ORIGIN_Y') + (genConst('LAT_TOP') - lat) * ppd];
}

// Minimal PNG decoder: 8-bit RGB/RGBA, non-interlaced (what Playwright writes).
function decodePNG(buf) {
  let p = 8, w, h, ct, idat = [];
  while (p < buf.length) {
    const len = buf.readUInt32BE(p), type = buf.toString('ascii', p + 4, p + 8), d = buf.slice(p + 8, p + 8 + len);
    if (type === 'IHDR') { w = d.readUInt32BE(0); h = d.readUInt32BE(4); assert.strictEqual(d[8], 8); ct = d[9]; assert.strictEqual(d[12], 0); }
    if (type === 'IDAT') idat.push(d);
    p += 12 + len;
  }
  const bpp = ct === 6 ? 4 : ct === 2 ? 3 : 0;
  assert.ok(bpp, 'colour type ' + ct);
  const raw = zlib.inflateSync(Buffer.concat(idat)), stride = w * bpp, out = Buffer.alloc(h * stride);
  for (let y = 0; y < h; y++) {
    const ft = raw[y * (stride + 1)], line = raw.slice(y * (stride + 1) + 1, (y + 1) * (stride + 1));
    for (let x = 0; x < stride; x++) {
      const A = x >= bpp ? out[y * stride + x - bpp] : 0, B = y ? out[(y - 1) * stride + x] : 0;
      const C = x >= bpp && y ? out[(y - 1) * stride + x - bpp] : 0;
      let v = line[x];
      if (ft === 1) v += A; else if (ft === 2) v += B; else if (ft === 3) v += (A + B) >> 1;
      else if (ft === 4) { const pp = A + B - C, pa = Math.abs(pp - A), pb = Math.abs(pp - B), pc = Math.abs(pp - C); v += pa <= pb && pa <= pc ? A : pb <= pc ? B : C; }
      out[y * stride + x] = v & 255;
    }
  }
  return { w, h, px: (x, y) => { const i = y * stride + x * bpp; return [out[i], out[i + 1], out[i + 2]]; } };
}

console.log('\n=== #739 M2: one source with the coverage gate ===');
test('generated block cites Topografisk Landpolygon with the M1 GeoPackage sha256 and date', () => {
  assert.ok(/Topografisk Landpolygon/.test(block), 'no Landpolygon in the generated block');
  assert.ok(/^landpolygon_\d+$/.test(field('layer') || ''), 'layer ' + field('layer'));
  const m1sha = (genDhm.match(/sha256\s+([0-9a-f]{64})/) || [])[1];
  assert.ok(m1sha, 'M1 generator has no GeoPackage sha256');
  assert.strictEqual(field('gpkg_sha256'), m1sha);
  const m1day = (foreignHeader.match(/last_change (\d{4}-\d{2}-\d{2})/) || [])[1];
  assert.strictEqual(field('last_change'), m1day);
});
test('generated block carries the attribution of foreign_land.txt and the terms link', () => {
  const attr = (foreignHeader.match(/Indeholder data fra Klimadatastyrelsen, Topografisk Landpolygon, [a-zæøå]+ \d{4}\./) || [])[0];
  assert.ok(attr, 'no attribution in foreign_land.txt');
  assert.ok(block.includes(attr), 'attribution missing: ' + attr);
  assert.ok(block.includes('https://www.sdfe.dk/Media/638052180760639799/'), 'terms link');
});
test('README carries the same attribution and the terms link', () => {
  const attr = (foreignHeader.match(/Indeholder data fra Klimadatastyrelsen, Topografisk Landpolygon, [a-zæøå]+ \d{4}/) || [])[0];
  const readme = read('README.md');
  assert.ok(attr && readme.includes('"' + attr + '"'), 'README lacks: ' + attr);
  assert.ok(readme.includes('https://www.sdfe.dk/Media/638052180760639799/'), 'README lacks the terms link');
});
test('Natural Earth is no longer the outline source', () => {
  const head = src.slice(0, src.indexOf('<html'));
  assert.ok(!/Natural Earth/.test(head + block), 'still cites Natural Earth');
  assert.ok(!/naturalearth|ne_10m|import shapefile/.test(gen), 'generator still reads Natural Earth');
});
test('both generators read the GeoPackage through one shared reader', () => {
  const helper = read('scripts/landpolygon_gpkg.py');
  assert.ok(/def read_layer\(/.test(helper));
  for (const [name, s] of [['gen-og-denmark.py', gen], ['gen-dhm-foreign-land.py', genDhm]]) {
    assert.ok(/^import landpolygon_gpkg|^from landpolygon_gpkg import/m.test(s), name + ' does not import the shared reader');
    assert.ok(!/^def gpkg\(|from_wkb/m.test(s), name + ' has its own GeoPackage reader');
  }
});
test('the simplification tolerance in the block is the generator\'s, and documented in metres', () => {
  assert.strictEqual(Number(field('simplify_px')), genConst('SIMPLIFY_PX'));
  assert.strictEqual(Number(field('min_area_px2')), genConst('MIN_AREA_PX2'));
  // On the constant's own line (v0.2.9 review nit, #824 F2: the looser
  // pattern matched unrelated text and stayed green without the comment).
  assert.ok(/^SIMPLIFY_PX\s*=\s*[\d.]+\s*#.*\d+ m/m.test(gen), 'tolerance in metres not documented');
});

console.log('\n=== #739 M2: outline matches the source polygon ===');
test('one outline ring per source part above the size threshold', () => {
  const n = Number(field('source_parts'));
  assert.ok(n >= 20, 'source_parts ' + field('source_parts'));
  assert.strictEqual(outlineRings.length, n);
});
test('mainland bbox equals the source bbox within the simplification tolerance', () => {
  const want = (field('source_bbox_px') || '').split(',').map(Number);
  assert.strictEqual(want.length, 4, 'source_bbox_px ' + field('source_bbox_px'));
  const got = bbox(mainRings), tol = genConst('SIMPLIFY_PX') + 0.05;
  for (let i = 0; i < 4; i++) assert.ok(Math.abs(got[i] - want[i]) <= tol, 'bbox ' + got + ' vs ' + want);
});
test('DK-DE land border agrees with the M1 border fixture (landpolygon_10, 50 m off the border)', () => {
  const pts = read('internal/dhm/testdata/dk_border_739.txt').split('\n').filter(l => /^(dk|de) /.test(l))
    .map(l => { const [k, e, n] = l.split(/\s+/); return [k, project(...fromUTM32(+e, +n))]; });
  assert.ok(pts.length > 400, 'fixture points ' + pts.length);
  let deIn = 0, dkOut = 0;
  for (const [k, p] of pts) {
    const r = outlineRings.find(rr => inside(p, rr));
    if (k === 'de' && r) deIn = Math.max(deIn, edgeDist(p, r));
    if (k === 'dk' && !r) dkOut = Math.max(dkOut, Math.min(...outlineRings.map(rr => edgeDist(p, rr))));
  }
  // 1 px is about 730 m here. Natural Earth reached 2965 m (about 4 px)
  // into Germany (#739 M1).
  assert.ok(deIn <= 1.0, 'German point ' + deIn.toFixed(2) + ' px inside the outline');
  assert.ok(dkOut <= 1.0, 'Danish point ' + dkOut.toFixed(2) + ' px outside the outline');
});
test('fromUTM32 matches pyproj (Copenhagen)', () => {
  // pyproj EPSG:4326 -> EPSG:25832 of (55.6761 N, 12.5683 E).
  const [lat, lon] = fromUTM32(724351.93, 6175804.02);
  assert.ok(Math.abs(lat - 55.6761) < 1e-4 && Math.abs(lon - 12.5683) < 1e-4, lat + ',' + lon);
});

console.log('\n=== #739 M2: the PNG is rendered from this outline ===');
test('outline vertices sit on the outline stroke in public/og-image-meshview.png', () => {
  const img = decodePNG(fs.readFileSync(path.join(__dirname, 'public', 'og-image-meshview.png')));
  assert.strictEqual(img.w, 1200); assert.strictEqual(img.h, 630);
  let on = 0, all = 0;
  for (const r of outlineRings) for (const [x, y] of r) {
    const [R, G, B] = img.px(Math.floor(x), Math.floor(y));
    all++;
    // --outline (#2a3a5c) is bluer than every background and fill pixel.
    if (B - R >= 35 && G - R >= 8) on++;
  }
  assert.ok(on / all >= 0.97, (on + '/' + all) + ' vertices on the stroke');
});

console.log('\n' + pass + ' passed, ' + fail + ' failed');
process.exit(fail ? 1 : 0);
