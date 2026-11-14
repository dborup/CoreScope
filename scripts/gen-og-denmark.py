#!/usr/bin/env python3
"""Generate the Denmark outline, nodes and links of the og:image (#715).

Rewrites the block between the "gen-og-denmark:begin/end" markers in
scripts/og-image-meshview.html; render the PNG afterwards with
node scripts/render-og-image.js.

Source (https://www.naturalearthdata.com/, public domain; no attribution
required): Natural Earth 1:10m Cultural Vectors, Admin 0 - Countries,
v5.1.1, the polygons with ADM0_A3 == "DNK" (Denmark proper: Jutland and
all islands; Greenland and the Faroes are separate entries).

Construction:
  * Equirectangular projection with x scaled by cos(56 deg), so Denmark is
    not stretched; PX_PER_DEG pixels per degree of latitude.
  * Bornholm (14.7 E) is moved into a framed inset at the lower right,
    at true scale, as on printed maps of Denmark; everything else keeps
    its true position.
  * Douglas-Peucker with SIMPLIFY_PX tolerance, topology preserved.
    Parts smaller than MIN_AREA_PX2 (islets) are dropped.
  * NODES / LINKS are given as lon/lat of real towns. The script fails
    if a node is not at least MARGIN_PX inside land, so the network always
    sits on land. It is still a stylised network, not real topology.

Dev-only tool, not part of the build; no shapefile is committed. Pinned
versions (the output is byte-identical with these):
  python3 -m venv .venv && .venv/bin/pip install pyshp==3.1.8 shapely==2.2.0 numpy==2.5.4
  curl -LO https://naturalearth.s3.amazonaws.com/10m_cultural/ne_10m_admin_0_countries.zip
  unzip ne_10m_admin_0_countries.zip -d ne
  .venv/bin/python scripts/gen-og-denmark.py ne/ne_10m_admin_0_countries.shp
"""
import math
import os
import re
import sys

import numpy as np
import shapefile
import shapely
from shapely.affinity import translate
from shapely.geometry import Point, shape

PX_PER_DEG = 153.0            # px per degree latitude: ~500 px for Denmark
LAT_TOP = 57.77               # Skagen is 57.75
LON_LEFT = 8.07               # Blavand is 8.09
ORIGIN_X, ORIGIN_Y = 712.0, 62.0
COS_LAT = math.cos(math.radians(56.0))
SIMPLIFY_PX = 0.6
MIN_AREA_PX2 = 14.0
MARGIN_PX = 4.0
BORNHOLM_LON = 13.5           # polygons east of this are Bornholm
INSET = (1086.0, 506.0, 80.0, 80.0)   # x, y, w, h of the Bornholm frame

# name: (lon, lat, radius, halo radius or 0)
NODES = {
    'skagen':    (10.34, 57.58, 9, 0),
    'aalborg':   (9.92, 56.94, 9, 0),
    'viborg':    (9.40, 56.45, 9, 0),
    # Lemvig is 56.548 N on the Nissum Bredning shore; nudged 3 km south so
    # the dot clears the coast by MARGIN_PX.
    'lemvig':    (8.31, 56.52, 9, 0),
    'aarhus':    (10.02, 56.25, 14, 26),
    'herning':   (8.98, 56.14, 9, 0),
    'esbjerg':   (8.62, 55.55, 9, 0),
    'kolding':   (9.32, 55.59, 12, 22),
    'sonderborg': (9.94, 54.96, 9, 0),
    'odense':    (10.22, 55.33, 10, 0),
    'slagelse':  (11.46, 55.49, 9, 0),
    'copenhagen': (12.40, 55.70, 14, 26),
    'helsingor': (12.34, 55.99, 9, 0),
    'nykobing':  (11.68, 54.74, 10, 0),
    'ronne':     (14.92, 55.12, 9, 0),
}
LINKS = [
    ('skagen', 'aalborg'), ('aalborg', 'aarhus'), ('aarhus', 'kolding'),
    ('kolding', 'sonderborg'), ('aarhus', 'viborg'), ('viborg', 'aalborg'),
    ('aarhus', 'herning'), ('herning', 'esbjerg'), ('esbjerg', 'kolding'),
    ('herning', 'lemvig'),
    ('kolding', 'odense'), ('odense', 'slagelse'), ('slagelse', 'copenhagen'),
    ('copenhagen', 'helsingor'), ('copenhagen', 'nykobing'),
    ('nykobing', 'ronne'), ('odense', 'nykobing'), ('aalborg', 'copenhagen'),
]


def project(lon, lat):  # works on scalars and numpy arrays
    return (ORIGIN_X + (lon - LON_LEFT) * COS_LAT * PX_PER_DEG,
            ORIGIN_Y + (LAT_TOP - lat) * PX_PER_DEG)


def project_geom(g):
    return shapely.transform(g, lambda a: np.column_stack(project(a[:, 0], a[:, 1])))


def fmt(v):
    s = '%.1f' % v
    return s[:-2] if s.endswith('.0') else s


def path_d(poly):
    pts = list(poly.exterior.coords)[:-1]
    return 'M' + 'L'.join(fmt(x) + ' ' + fmt(y) for x, y in pts) + 'Z'


def main():
    if len(sys.argv) != 2:
        sys.exit('usage: gen-og-denmark.py ne_10m_admin_0_countries.shp')
    r = shapefile.Reader(sys.argv[1])
    land = None
    for s, rec in zip(r.shapes(), r.records()):
        if rec.as_dict()['ADM0_A3'] == 'DNK':
            land = shape(s.__geo_interface__)
    if land is None:
        sys.exit('no ADM0_A3 == DNK in ' + sys.argv[1])

    parts = []
    for p in land.geoms:
        if p.centroid.x > BORNHOLM_LON:
            g = project_geom(p)
            b = g.bounds
            g = translate(g, INSET[0] + (INSET[2] - (b[2] - b[0])) / 2 - b[0],
                          INSET[1] + (INSET[3] - (b[3] - b[1])) / 2 - b[1])
            shift = (g.bounds[0] - b[0], g.bounds[1] - b[1])
            parts.append((g, shift))
        else:
            parts.append((project_geom(p), (0.0, 0.0)))
    kept = []
    for g, shift in parts:
        if g.area < MIN_AREA_PX2:
            continue
        g = g.simplify(SIMPLIFY_PX, preserve_topology=True)
        if g.geom_type == 'Polygon' and not g.is_empty:
            kept.append((g, shift))
    kept.sort(key=lambda t: -t[0].area)

    # Bornholm shift: recompute from its own polygon (same for all its nodes).
    bh = [t for t in parts if t[1] != (0.0, 0.0)][0][1]

    def xy(name):
        lon, lat = NODES[name][:2]
        x, y = project(lon, lat)
        return (x + bh[0], y + bh[1]) if lon > BORNHOLM_LON else (x, y)

    for name in NODES:
        x, y = xy(name)
        if not any(g.buffer(-MARGIN_PX).contains(Point(x, y)) for g, _ in kept):
            sys.exit('node %s at (%.1f, %.1f) is not %g px inside land' % (name, x, y, MARGIN_PX))

    lines = ['  <!-- gen-og-denmark:begin (generated by scripts/gen-og-denmark.py; do not edit by hand) -->']
    for g, _ in kept:
        lines.append('  <path class="outline" d="%s"/>' % path_d(g))
    lines.append('  <rect class="inset" x="%s" y="%s" width="%s" height="%s" rx="6"/>' % tuple(fmt(v) for v in INSET))
    lines.append('  <g class="links">')
    for a, b in LINKS:
        (x1, y1), (x2, y2) = xy(a), xy(b)
        lines.append('    <line class="link" x1="%s" y1="%s" x2="%s" y2="%s"/>' % (fmt(x1), fmt(y1), fmt(x2), fmt(y2)))
    lines.append('  </g>')
    lines.append('  <g>')
    for name, (_, _, rad, halo) in NODES.items():
        if halo:
            x, y = xy(name)
            lines.append('    <circle class="halo" cx="%s" cy="%s" r="%d"/>' % (fmt(x), fmt(y), halo))
    for name, (_, _, rad, _h) in NODES.items():
        x, y = xy(name)
        lines.append('    <circle class="node" cx="%s" cy="%s" r="%d"/>' % (fmt(x), fmt(y), rad))
    lines.append('  </g>')
    lines.append('  <!-- gen-og-denmark:end -->')
    block = '\n'.join(lines)

    html = os.path.join(os.path.dirname(os.path.abspath(__file__)), 'og-image-meshview.html')
    text = open(html, encoding='utf-8').read()
    pat = re.compile(r'[ \t]*<!-- gen-og-denmark:begin.*?<!-- gen-og-denmark:end -->', re.S)
    if not pat.search(text):
        sys.exit('markers missing in ' + html)
    open(html, 'w', encoding='utf-8').write(pat.sub(lambda m: block, text))
    print('%d parts, %d bytes of SVG' % (len(kept), len(block)), file=sys.stderr)


if __name__ == '__main__':
    main()
