#!/usr/bin/env python3
"""Generate internal/dhm/foreign_land.txt (#692).

The DHM WCS returns 0.0, not nodata, outside Danish coverage. On the sea
that is right (the sea is at 0 m, Danish or not), but on Swedish, German,
Polish or Norwegian land it would draw hills as sea level. The terrain
profile therefore treats a sample as uncovered when it lies on this
polygon: foreign land inside the DHM raster extent, in EPSG:25832 metres.

Sources (https://www.naturalearthdata.com/, public domain; no attribution
required): Natural Earth 1:10m Cultural Vectors, Admin 0 - Countries,
v5.1.1, and 1:10m Physical Vectors, Minor Islands, v4.1.0. Admin 0 lacks
islands such as Ven (Swedish, 7.5 km2, mid-Øresund); Minor Islands has
them, without a country.

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
  * Danish land (unsimplified) is subtracted, so neither the buffer nor
    the simplification can mark Danish land as foreign. Admin 0 alone is
    not all of it (#704): every minor island nearer Danish land than to
    foreign land, plus DANISH_ISLANDS, is subtracted with it, so an islet
    that only Minor Islands has, or that neither layer has, cannot be
    swallowed by the buffer around a nearby foreign coast.
  * Parts smaller than MIN_AREA_M2 are dropped: islets below a few
    profile samples, which read as sea (0 m) either way.

The output is one "ring E,N E,N ..." line per ring, integer metres; the
reader tests rings even-odd, so a hole is just another ring.

Dev-only tool, not part of the build. Pinned versions (the output is
byte-identical with these):
  uv venv && uv pip install numpy==2.5.4 pyproj==3.8.0 pyshp==3.1.8 shapely==2.2.0
  python3 scripts/gen-dhm-foreign-land.py ne_10m_admin_0_countries.shp \
      ne_10m_minor_islands.shp > internal/dhm/foreign_land.txt
Then update foreignLandSHA256 in internal/dhm/boundary_test.go.
"""
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

# Swedish islands missing from both Natural Earth layers (#693 review round
# 2, F3), as hand-made ellipses: centre (lat, lon), length and width in
# metres, and the bearing of the long axis. Not survey data: the centres
# are the review's, the sizes the commonly published ones (Peberholm,
# the artificial island of the Øresund link: about 4 km x 0.5 km, its
# long axis along the link toward Lernacken; Hallands Väderö: about
# 3 km2). BUFFER_M on top makes the outline uncritical.
EXTRA_ISLANDS = (
    ("Peberholm", 55.598, 12.745, 4000.0, 500.0, 110.0),
    ("Hallands Väderö", 56.447, 12.560, 2000.0, 2000.0, 0.0),
)

# Danish islets in neither Natural Earth layer, same form as
# EXTRA_ISLANDS, subtracted after the buffer (#704). Okseøerne lie in
# Flensborg Fjord off Sønderhav and are Danish (Aabenraa Kommune).
# Source: two centre points and two axis lengths read off OpenStreetMap
# ways 4098190 (Store Okseø) and 4100737 (Lille Okseø), © OpenStreetMap
# contributors, ODbL. They agree within 400 m with Wikidata Q2013616
# (54.85754, 9.50797, CC0) and da.wikipedia "Okseøerne" (54.85694,
# 9.50556). The axes are scaled to the published areas, 7.6 ha and
# 3.3 ha; this is not survey data.
# Neither islet reaches the buffered foreign region today: the nearest
# German land (Holnis, Glücksburg) is 2105 m from Store Okseø and
# 2290 m from Lille Okseø, against a BUFFER_M of 1250. They are listed
# so that stays true if BUFFER_M grows or the source coast moves.
DANISH_ISLANDS = (
    ("Store Okseø", 54.8576, 9.5092, 520.0, 190.0, 133.0),
    ("Lille Okseø", 54.8570, 9.5029, 340.0, 125.0, 20.0),
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


def main(countries, minor_islands):
    sf = shapefile.Reader(countries)
    x0, y0, x1, y1 = RASTER_EXTENT
    box = shapely.box(x0 - MARGIN_M, y0 - MARGIN_M, x1 + MARGIN_M, y1 + MARGIN_M)
    fo = land(sf, FOREIGN)
    dk = land(sf, ("DNK",))
    islands = [utm(shape(sr.shape.__geo_interface__)) for sr in shapefile.Reader(minor_islands).iterShapeRecords()]
    islands = [i for i in islands if i.intersects(box)]
    # A minor island belongs to whichever land it lies nearer; the Danish
    # ones join the subtrahend, not the buffered region (#704).
    danish = [i for i in islands if i.distance(fo) >= i.distance(dk)]
    islands = [i for i in islands if i.distance(fo) < i.distance(dk)]
    extra = [ellipse(*i[1:]) for i in EXTRA_ISLANDS]
    fo = unary_union([fo, *islands, *extra]).buffer(BUFFER_M, quad_segs=4).intersection(box)
    dk = unary_union([dk, *danish, *[ellipse(*i[1:]) for i in DANISH_ISLANDS]]).intersection(box)
    simple = fo.simplify(SIMPLIFY_M, preserve_topology=True)
    hausdorff = shapely.hausdorff_distance(fo, simple)
    region = simple.difference(dk)
    parts = [shapely.Polygon(p.exterior, [h for h in p.interiors if shapely.Polygon(h).area >= MIN_AREA_M2])
             for p in polygons(region) if p.area >= MIN_AREA_M2]
    region = shapely.MultiPolygon(parts)
    assert region.is_valid
    print("# Foreign land (SWE DEU POL NOR) in the DHM extent, EPSG:25832 metres;")
    print("# generated by scripts/gen-dhm-foreign-land.py from Natural Earth 10m")
    print("# Admin 0 Countries v5.1.1 and Minor Islands v4.1.0 (public domain).")
    print("# Plus hand-made Peberholm and Hallands Väderö; grown %gm out to sea." % BUFFER_M)
    print("# margin=%gm simplify=%gm min_area=%gm2" % (MARGIN_M, SIMPLIFY_M, MIN_AREA_M2))
    nv = 0
    for p in region.geoms:
        for ring in [p.exterior, *p.interiors]:
            pts = list(ring.coords)[:-1]
            nv += len(pts)
            print("ring " + " ".join("%d,%d" % (round(x), round(y)) for x, y in pts))
    src = sum(len(r.coords) - 1 for p in polygons(fo) for r in [p.exterior, *p.interiors])
    print("polygons=%d vertices=%d (source %d) hausdorff=%.0fm islands=%d danish_islands=%d" %
          (len(region.geoms), nv, src, hausdorff, len(islands), len(danish) + len(DANISH_ISLANDS)), file=sys.stderr)


if __name__ == "__main__":
    main(sys.argv[1], sys.argv[2])
