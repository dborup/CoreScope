#!/usr/bin/env python3
"""Generate internal/dhm/foreign_land.txt (#692).

The DHM WCS returns 0.0, not nodata, outside Danish coverage. On the sea
that is right (the sea is at 0 m, Danish or not), but on Swedish, German,
Polish or Norwegian land it would draw hills as sea level. The terrain
profile therefore treats a sample as uncovered when it lies on this
polygon: foreign land inside the DHM raster extent, in EPSG:25832 metres.

Sources. Foreign land (https://www.naturalearthdata.com/, public domain;
no attribution required): Natural Earth 1:10m Cultural Vectors, Admin 0 - Countries,
v5.1.1, and 1:10m Physical Vectors, Minor Islands, v4.1.0. Admin 0 lacks
islands such as Ven (Swedish, 7.5 km2, mid-Øresund); Minor Islands has
them, without a country. Danish land and sea: Klimadatastyrelsen,
Topografisk Landpolygon (https://dataforsyningen.dk/data/4826), GPKG
landpolygon/full, layers landpolygon_10 and havpolygon_10, EPSG:25832,
fetched November 2026 (gpkg_contents last_change 2026-11-14, sha256
e078fae27dedaabc7ddcd362be94522f9d64b1dd527f77f3e11b6890e5e9dc33).
Terms: "Vilkår for brug af frie geografiske data" (1 December 2022,
https://www.sdfe.dk/Media/638052180760639799/); the attribution line the
terms require is in the output header. The GeoPackage needs a free
Dataforsyningen login and is never checked in; only the derived polygon
is.

Construction, in EPSG:25832:
  * The land of ADM0_A3 in FOREIGN (Sweden, Germany, Poland, Norway; not
    ISO_A3, which is -99 for Norway), plus every minor island nearer to
    that land than to Danish land, plus EXTRA_ISLANDS, is unioned, grown
    by BUFFER_M out to sea, and clipped to
    internal/dhm RasterExtent plus MARGIN_M. Clipped edges then lie
    outside the extent, where the profile's own extent check decides.
  * BUFFER_M: errors must fall on the safe side. Sea marked foreign only
    makes a path partial with no verdict; foreign land read as sea gives
    a confident wrong verdict (#693 review round 2, F1). The 1:10m coast
    lies up to 1000 m seaward of real land: measured distances from the
    source coast to Kärnan 209 m, Helsingborg Knutpunkten 461 m,
    Landskrona 658-782 m, Turning Torso 674 m, Sylt List 687 m,
    Smygehuk 738 m, Malmö Västra hamnen 897 m, Falsterbo lighthouse
    1000 m. 1250 m covers the worst of these plus the 150 m
    simplification with 100 m to spare. The narrowest Danish-only sea
    path measured, Kollund→Egernsund in Flensborg Fjord, keeps 1961 m to
    the source coast, so it stays covered; Helsingør→Copenhagen keeps
    5.2 km. Only a path hugging the German shore of Flensborg Fjord, or
    ending within 1.25 km of a foreign coast, becomes partial.
  * It is simplified with Douglas-Peucker, tolerance SIMPLIFY_M, topology
    preserved: every source vertex stays within SIMPLIFY_M of the result
    (the Hausdorff distance is printed). 150 m is under the positional
    accuracy of a 1:10m source itself (several hundred metres along the
    coast) and is 6-15 profile samples (10-25 m steps), so it adds little
    error to what the source already has, and it removes most of the
    vertices the buffer's rounded corners add.
  * Danish territory is subtracted, so neither the buffer nor the
    simplification can mark Danish land or sea as foreign (#739 M1). It
    comes from Klimadatastyrelsen's Topografisk Landpolygon, not from
    Natural Earth: the GeoPackage layers DK_LAYERS (land and sea at
    1:10 000; the sea reaches out to Denmark's outer sea limit, the
    DK-DE land border is the topographic one, and every Danish islet is
    in it, Okseøerne and Peberholm included). Land and sea are unioned
    first, so the coast is inside the subtrahend and leaves no sliver;
    then the union is simplified with Douglas-Peucker at DK_SIMPLIFY_M
    and shrunk by DK_SHRINK_M, so the subtrahend stays inside real
    Danish territory (asserted) and subtracting it can never turn
    foreign land into covered. Only the part within CLIP_M of the
    buffered foreign region is read; the rest cannot touch it.
  * Natural Earth's Denmark reaches up to 2965 m into Germany: 208 of
    242 test points 50 m south of the real DK-DE land border lie in it
    (internal/dhm/testdata/dk_border_739.txt). Subtracting it, as before
    #739 M1, read all 208 as covered; 93 are beyond the 1250 m buffer,
    so the precise subtrahend alone would still leave those. So inside
    the bbox of the land border, extended BORDER_SOUTH_M south,
    everything that is not Danish (German land, and the German water at
    the border's two ends) is foreign too, before Danish territory is
    subtracted.
  * Parts smaller than MIN_AREA_M2 are dropped: islets below a few
    profile samples, which read as sea (0 m) either way.

The output is one "ring E,N E,N ..." line per ring, integer metres; the
reader tests rings even-odd, so a hole is just another ring.

Dev-only tool, not part of the build. Pinned versions; with these the
output is byte-identical run to run (checked twice for #739 M1 on macOS
arm64, Python 3.14.3, GEOS 3.14.1). Another platform's GEOS build can
move a few buffered vertices: regenerating the #766 file here changed
two rings by up to a few hundred metres, so record the platform with a
new snapshot.
  uv venv -p 3.14 ~/geodata/venv
  VIRTUAL_ENV=~/geodata/venv uv pip install numpy==2.5.4 pyproj==3.8.0 \
      pyshp==3.1.8 shapely==2.2.0
  # Natural Earth, read-only, into their own directories:
  #   naciscdn.org/naturalearth/5.1.1/10m/cultural/ne_10m_admin_0_countries.zip
  #     (sha256 ce1ac7036499a0edd641fbc093cd209a98f96a49d2eca8480aaacad35138a7f6)
  #   naciscdn.org/naturalearth/10m/physical/ne_10m_minor_islands.zip (4.1.0;
  #     sha256 47c0b3ce26df7b4dbabdbb251023918cb2f5c83462a426288f1f253da7284db2)
  ~/geodata/venv/bin/python -I scripts/gen-dhm-foreign-land.py \
      ~/geodata/ne_admin0/ne_10m_admin_0_countries.shp \
      ~/geodata/ne_minor/ne_10m_minor_islands.shp \
      ~/geodata/landpolygon.gpkg > internal/dhm/foreign_land.txt
Then update foreignLandSHA256 in internal/dhm/boundary_test.go, and
regenerate internal/dhm/testdata/dk_border_739.txt with
scripts/gen-dhm-border-fixture.py if the GeoPackage changed.
"""
import sqlite3
import sys

import numpy as np
import shapefile
from pyproj import Transformer
import shapely
import shapely.affinity
from shapely.geometry import shape
from shapely.ops import unary_union

RASTER_EXTENT = (440000, 6048000, 894000, 6404000)  # internal/dhm/wcs.go
MARGIN_M = 2000.0
SIMPLIFY_M = 150.0
MIN_AREA_M2 = 20000.0  # about 140 m square
BUFFER_M = 1250.0
FOREIGN = ("SWE", "DEU", "POL", "NOR")
DK_LAYERS = ("landpolygon_10", "havpolygon_10")
DK_SIMPLIFY_M = 10.0
DK_SHRINK_M = 10.0
CLIP_M = 500.0
BORDER_SOUTH_M = 5000.0
BORDER_MAX_N = 6110000.0  # about 55.1 N; the land border is at 54.80-54.91 N
DANISH_MONTHS = ("januar", "februar", "marts", "april", "maj", "juni", "juli",
                 "august", "september", "oktober", "november", "december")

# Swedish islands missing from both Natural Earth layers (#693 review round
# 2, F3), as hand-made ellipses: centre (lat, lon), length and width in
# metres, and the bearing of the long axis. Not survey data: the centre is
# the review's, the size the commonly published one (Hallands Väderö:
# about 3 km2). BUFFER_M on top makes the outline uncritical.
# Peberholm, the artificial island of the Øresund link, which F3 also
# listed here, must not be (#763): it is Danish, in Tårnby Kommune. It is
# part of the "Danmark" country unit and of Region Hovedstaden in the
# INSPIRE administrative units of Klimadatastyrelsen (Dataforsyningen,
# au_inspire), and en.wikipedia "Peberholm" says it "belongs to Denmark".
# As Swedish land, grown by BUFFER_M, it marked Peberholm, the southern
# tip of Saltholm and the sea off Dragør as foreign. Nor does it need a
# Danish entry: Topografisk Landpolygon has it (#739 M1).
EXTRA_ISLANDS = (
    ("Hallands Väderö", 56.447, 12.560, 2000.0, 2000.0, 0.0),
)

to_utm = Transformer.from_crs("EPSG:4326", "EPSG:25832", always_xy=True).transform


def utm(g):
    return shapely.transform(g, lambda c: np.column_stack(to_utm(c[:, 0], c[:, 1])))


def land(sf, codes):
    return utm(unary_union([shape(sr.shape.__geo_interface__) for sr in sf.iterShapeRecords()
                            if sr.record["ADM0_A3"] in codes]))


def ellipse(lat, lon, length, width, bearing):
    e, n = to_utm(lon, lat)
    g = shapely.affinity.scale(shapely.Point(0, 0).buffer(1.0, quad_segs=8), width / 2, length / 2)
    return shapely.affinity.translate(shapely.affinity.rotate(g, -bearing, origin=(0, 0)), e, n)


def polygons(g):
    return [p for p in getattr(g, "geoms", [g]) if isinstance(p, shapely.Polygon) and not p.is_empty]


def gpkg(path, table, region):
    """The polygons of a GeoPackage layer that meet region, clipped to it.
    A GeoPackage is SQLite; each geometry is a GP header, an optional
    envelope (size from flags bits 1-3) and standard WKB."""
    db = sqlite3.connect("file:%s?mode=ro" % path, uri=True)
    out = []
    for (blob,) in db.execute("SELECT geometri FROM %s ORDER BY ogc_fid" % table):
        if blob[:2] != b"GP":
            raise ValueError("%s: not a GeoPackage geometry" % table)
        env = {0: 0, 1: 32, 2: 48, 3: 48, 4: 64}[(blob[3] >> 1) & 7]
        g = shapely.from_wkb(bytes(blob[8 + env:]))
        if g.intersects(region):
            out.append(g.intersection(region))
    (changed,) = db.execute("SELECT last_change FROM gpkg_contents WHERE table_name = ?", (table,)).fetchone()
    db.close()
    return out, changed


def lines(g):
    g = shapely.line_merge(g) if not g.is_empty else g
    return [ln for ln in getattr(g, "geoms", [g]) if isinstance(ln, shapely.LineString) and ln.length > 0]


def main(countries, minor_islands, landpolygon):
    sf = shapefile.Reader(countries)
    x0, y0, x1, y1 = RASTER_EXTENT
    box = shapely.box(x0 - MARGIN_M, y0 - MARGIN_M, x1 + MARGIN_M, y1 + MARGIN_M)
    fo = land(sf, FOREIGN)
    dk_ne = land(sf, ("DNK",))
    islands = [utm(shape(sr.shape.__geo_interface__)) for sr in shapefile.Reader(minor_islands).iterShapeRecords()]
    islands = [i for i in islands if i.intersects(box)]
    # A minor island belongs to whichever land it lies nearer. Natural
    # Earth's Denmark only decides that; Topografisk Landpolygon is the
    # Danish side (#739 M1).
    danish_minor = [i for i in islands if i.distance(fo) >= i.distance(dk_ne)]
    islands = [i for i in islands if i.distance(fo) < i.distance(dk_ne)]
    extra = [ellipse(*i[1:]) for i in EXTRA_ISLANDS]
    fo = unary_union([fo, *islands, *extra]).buffer(BUFFER_M, quad_segs=4).intersection(box)
    simple = fo.simplify(SIMPLIFY_M, preserve_topology=True)
    hausdorff = shapely.hausdorff_distance(fo, simple)

    # The DK-DE land border: the outline of Danish land south of
    # BORDER_MAX_N that no Danish sea touches (the coast does), minus the
    # strip's own edges. It is read on its own, not from the clip below:
    # where Natural Earth's Denmark reaches into Germany, the real border
    # can lie outside the buffered foreign region.
    strip = shapely.box(x0 - MARGIN_M, y0 - MARGIN_M, x1 + MARGIN_M, BORDER_MAX_N)
    s_land = unary_union(gpkg(landpolygon, DK_LAYERS[0], strip)[0])
    s_sea = unary_union(gpkg(landpolygon, DK_LAYERS[1], strip)[0])
    border = s_land.boundary.difference(s_sea.buffer(1.0)).difference(strip.boundary.buffer(1.0))
    border = [ln for ln in lines(border) if ln.length >= 1000.0]
    assert border, "no DK-DE land border found"
    be0, bn0, be1, bn1 = unary_union(border).bounds
    border_box = shapely.box(be0, bn0 - BORDER_SOUTH_M, be1, bn1)

    # Danish territory near the foreign region and the border box, land
    # and sea in one.
    clip = unary_union([simple, border_box]).buffer(CLIP_M, quad_segs=4).intersection(box)
    layers, changed = [], []
    for layer in DK_LAYERS:
        p, c = gpkg(landpolygon, layer, clip)
        layers.append(unary_union(p))
        changed.append(c)
    dk = unary_union(layers)
    dk_sub = dk.simplify(DK_SIMPLIFY_M, preserve_topology=True).buffer(-DK_SHRINK_M, quad_segs=4)
    leak = dk_sub.difference(dk).area
    assert leak < 1.0, "subtrahend leaves Danish territory by %.1f m2" % leak
    near_border = border_box.difference(dk_sub)

    region = unary_union([simple, near_border]).difference(dk_sub)
    parts = [shapely.Polygon(p.exterior, [h for h in p.interiors if shapely.Polygon(h).area >= MIN_AREA_M2])
             for p in polygons(region) if p.area >= MIN_AREA_M2]
    region = shapely.MultiPolygon(parts)
    assert region.is_valid
    # Danish territory left in the region is the shrink strip along the
    # outer limit, no deeper than simplification plus shrink.
    deep = region.intersection(dk.buffer(-(DK_SIMPLIFY_M + DK_SHRINK_M + 1.0))).area
    assert deep < 1.0, "%.1f m2 of Danish territory deep inside the region" % deep
    day = min(changed)[:10]
    month = "%s %s" % (DANISH_MONTHS[int(day[5:7]) - 1], day[:4])
    print("# Foreign land (SWE DEU POL NOR) in the DHM extent, EPSG:25832 metres;")
    print("# generated by scripts/gen-dhm-foreign-land.py from Natural Earth 10m")
    print("# Admin 0 Countries v5.1.1 and Minor Islands v4.1.0 (public domain),")
    print("# plus hand-made Hallands Väderö; grown %gm out to sea. Near the" % BUFFER_M)
    print("# DK-DE land border, all that is not Danish. Minus Danish land and sea")
    print("# from Topografisk Landpolygon (%s, last_change %s)," % (" ".join(DK_LAYERS), day))
    print("# simplified %gm and shrunk %gm." % (DK_SIMPLIFY_M, DK_SHRINK_M))
    print("# Indeholder data fra Klimadatastyrelsen, Topografisk Landpolygon, %s." % month)
    print("# Terms: Vilkår for brug af frie geografiske data,")
    print("# https://www.sdfe.dk/Media/638052180760639799/")
    print("# margin=%gm simplify=%gm min_area=%gm2" % (MARGIN_M, SIMPLIFY_M, MIN_AREA_M2))
    nv = 0
    for p in region.geoms:
        for ring in [p.exterior, *p.interiors]:
            # Integer metres; a vertex that rounds onto the one before it
            # would be a zero-length edge.
            pts = []
            for x, y in list(ring.coords)[:-1]:
                q = (round(x), round(y))
                if not pts or q != pts[-1]:
                    pts.append(q)
            while len(pts) > 1 and pts[0] == pts[-1]:
                pts.pop()
            assert len(pts) >= 3
            nv += len(pts)
            print("ring " + " ".join("%d,%d" % q for q in pts))
    src = sum(len(r.coords) - 1 for p in polygons(fo) for r in [p.exterior, *p.interiors])
    dk_in = region.intersection(dk).area
    print("polygons=%d vertices=%d (source %d) hausdorff=%.0fm islands=%d danish_minor=%d "
          "border=%.1fkm dk_inside=%.0fm2 dk_parts=%d" %
          (len(region.geoms), nv, src, hausdorff, len(islands), len(danish_minor),
           sum(ln.length for ln in border) / 1000, dk_in, len(polygons(dk))), file=sys.stderr)


if __name__ == "__main__":
    main(sys.argv[1], sys.argv[2], sys.argv[3])
