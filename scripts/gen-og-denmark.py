#!/usr/bin/env python3
"""Generate the Denmark outline, nodes and links of the og:image (#715).

Rewrites the block between the "gen-og-denmark:begin/end" markers in
scripts/og-image-meshview.html; render the PNG afterwards with
node scripts/render-og-image.js.

Source (#739 M2): Klimadatastyrelsen, Topografisk Landpolygon
(https://dataforsyningen.dk/data/4826), the same GeoPackage
(landpolygon/full) as the DHM coverage gate's Danish side
(scripts/gen-dhm-foreign-land.py, #739 M1), read through the same
reader (scripts/landpolygon_gpkg.py), layer OG_LAYER: the 1:1 000 000
level of the Danish land polygon, EPSG:25832. Every Danish island in it
above MIN_AREA_PX2 is drawn, Saltholm and the small islands of the
South Funen archipelago included. Terms: "Vilkår for brug af frie
geografiske data" (1 December 2022); the attribution line they require,
the GeoPackage's sha256 and the layer's last_change are written into the
generated block. The GeoPackage needs a free Dataforsyningen login and is
never checked in; only the derived SVG path is.

Construction:
  * EPSG:25832 -> WGS84 (pyproj), then equirectangular with x scaled by
    cos(56 deg), so Denmark is not stretched; PX_PER_DEG pixels per
    degree of latitude.
  * Bornholm (14.7 E) is moved into a framed inset at the lower right,
    at true scale, as on printed maps of Denmark; everything else keeps
    its true position. One shift, from Bornholm's own bounds, moves
    every part east of BORNHOLM_LON.
  * Each part's outer ring only (lakes are not drawn). Parts smaller
    than MIN_AREA_PX2 (about 7.4 km2) are dropped; the rest are
    simplified with Douglas-Peucker at SIMPLIFY_PX, topology preserved.
    0.6 px is about 440 m on the ground (1 px = 1/153 deg of latitude,
    about 727 m): under one pixel of the 1200x630 card, and the order
    of the 1:1 000 000 level's own generalisation. The number of parts
    and the mainland bbox before simplification are written into the
    block as source_parts / source_bbox_px, so a test can check the
    outline against its source without the GeoPackage.
  * NODES / LINKS are given as lon/lat of real towns. The script fails
    if a node is not at least MARGIN_PX inside land, so the network always
    sits on land. It is still a stylised network, not real topology.

Dev-only tool, not part of the build. Same venv and GeoPackage as
scripts/gen-dhm-foreign-land.py (see there for how to fetch them); pinned
versions, the output is byte-identical run to run with these (macOS
arm64, Python 3.14.3, GEOS 3.14.1):
  uv venv -p 3.14 ~/geodata/venv
  VIRTUAL_ENV=~/geodata/venv uv pip install numpy==2.5.4 pyproj==3.8.0 \
      pyshp==3.1.8 shapely==2.2.0
  ~/geodata/venv/bin/python -I scripts/gen-og-denmark.py ~/geodata/landpolygon.gpkg
  node scripts/render-og-image.js
"""
import math
import os
import re
import sys

import numpy as np
import shapely
from pyproj import Transformer
from shapely.affinity import translate
from shapely.geometry import Point
from shapely.ops import unary_union

# -I drops the script's directory from sys.path; the shared GeoPackage
# reader lives next to this script.
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import landpolygon_gpkg  # noqa: E402

OG_LAYER = "landpolygon_1000"
PX_PER_DEG = 153.0            # px per degree latitude: ~500 px for Denmark
LAT_TOP = 57.77               # Skagen is 57.75
LON_LEFT = 8.07               # Blavand is 8.09
ORIGIN_X, ORIGIN_Y = 712.0, 62.0
COS_LAT = math.cos(math.radians(56.0))
SIMPLIFY_PX = 0.6             # about 440 m on the ground
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


to_lonlat = Transformer.from_crs("EPSG:25832", "EPSG:4326", always_xy=True).transform


def project_utm(g):
    """EPSG:25832 metres to card pixels."""
    def f(a):
        lon, lat = to_lonlat(a[:, 0], a[:, 1])
        return np.column_stack(project(lon, lat))
    return shapely.transform(g, f)


def fmt(v):
    s = '%.1f' % v
    return s[:-2] if s.endswith('.0') else s


def path_d(poly):
    pts = list(poly.exterior.coords)[:-1]
    return 'M' + 'L'.join(fmt(x) + ' ' + fmt(y) for x, y in pts) + 'Z'


def main():
    if len(sys.argv) != 2:
        sys.exit('usage: gen-og-denmark.py landpolygon.gpkg')
    gpkg = sys.argv[1]
    polys, changed = landpolygon_gpkg.read_layer(gpkg, OG_LAYER)
    land = unary_union(polys)
    parts = [project_utm(shapely.Polygon(p.exterior))
             for p in getattr(land, 'geoms', [land]) if isinstance(p, shapely.Polygon)]
    parts = [g for g in parts if g.area >= MIN_AREA_PX2]
    east_x = project(BORNHOLM_LON, LAT_TOP)[0]
    east = [g for g in parts if g.centroid.x > east_x]
    main_parts = [g for g in parts if g.centroid.x <= east_x]
    if not east or not main_parts:
        sys.exit('no Bornholm or no mainland in ' + OG_LAYER)
    b = max(east, key=lambda g: g.area).bounds
    bh = (INSET[0] + (INSET[2] - (b[2] - b[0])) / 2 - b[0],
          INSET[1] + (INSET[3] - (b[3] - b[1])) / 2 - b[1])
    source_bbox = unary_union(main_parts).bounds
    kept = []
    for g in main_parts + [translate(g, *bh) for g in east]:
        s = g.simplify(SIMPLIFY_PX, preserve_topology=True)
        if s.geom_type != 'Polygon' or s.is_empty:
            sys.exit('part at %s did not survive simplification' % (g.centroid,))
        kept.append(s)
    kept.sort(key=lambda g: (-g.area, g.bounds))

    def xy(name):
        lon, lat = NODES[name][:2]
        x, y = project(lon, lat)
        return (x + bh[0], y + bh[1]) if lon > BORNHOLM_LON else (x, y)

    for name in NODES:
        x, y = xy(name)
        if not any(g.buffer(-MARGIN_PX).contains(Point(x, y)) for g in kept):
            sys.exit('node %s at (%.1f, %.1f) is not %g px inside land' % (name, x, y, MARGIN_PX))

    lines = ['  <!-- gen-og-denmark:begin (generated by scripts/gen-og-denmark.py; do not edit by hand) -->',
             '  <!-- Denmark: Topografisk Landpolygon layer=%s last_change=%s' % (OG_LAYER, changed[:10]),
             '       gpkg_sha256=%s' % landpolygon_gpkg.file_sha256(gpkg),
             '       simplify_px=%s min_area_px2=%s source_parts=%d source_bbox_px=%s' % (
                 fmt(SIMPLIFY_PX), fmt(MIN_AREA_PX2), len(parts), ','.join(fmt(v) for v in source_bbox)),
             '       ' + landpolygon_gpkg.attribution(changed),
             '       Terms: Vilkår for brug af frie geografiske data, ' + landpolygon_gpkg.TERMS_URL + ' -->']
    for g in kept:
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
    nv = sum(len(g.exterior.coords) - 1 for g in kept)
    print('%s: %d parts, %d vertices, %d bytes of SVG' % (OG_LAYER, len(kept), nv, len(block)), file=sys.stderr)


if __name__ == '__main__':
    main()
