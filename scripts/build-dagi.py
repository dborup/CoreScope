#!/usr/bin/env python3
"""Build the DAGI boundary assets in cmd/server/dagi/ (#740 M1).

DAGI (Danmarks Administrative Geografiske Inddeling) is Klimadatastyrelsen's
register of Danish administrative areas. CoreScope uses four of its entities
to tell which kommune, landsdel, region and postnummer a node's position is
in. The lookup runs offline against simplified polygons checked in next to
the server; this script makes them.

Source. The owner's one-off Datafordeler download (it needs a Datafordeler
API key, so it is never fetched here and never checked in, about 80 MB):
DAGI V4, TotalDownload, GPKG, TypeOfData Current, one file per entity,
generation 271, downloaded 2026-11-26:

    DAGI_V4_Kommuneinddeling_TotalDownload_gpkg_Current_271.gpkg
    DAGI_V4_Landsdel_TotalDownload_gpkg_Current_271.gpkg
    DAGI_V4_Regionsinddeling_TotalDownload_gpkg_Current_271.gpkg
    DAGI_V4_Postnummerinddeling_TotalDownload_gpkg_Current_271.gpkg

All four are EPSG:25832 (ETRS89 / UTM 32N); the geometry column geometri is a
GeoPackage blob (GP header, optional envelope, standard WKB).

Credit. Dataforsyningen's terms for Klimadatastyrelsen data ("Vilkår for
brug af data", v3.1, https://dataforsyningen.dk/Vilkaar, CC BY 4.0) require
the credit to name the owner, the dataset (with a link), the licence (with a
link) and whether the data was changed; the older "Vilkår for brug af frie
geografiske data" add when the data was fetched. credit() below carries all
of it, in the wording of #733 (internal/dhm), and every asset's header
repeats it with the links, so the obligation travels with the data.

Construction, per layer group:
  * kommune, landsdel and region share every vertex (landsdele and regions
    are unions of kommuner), so the three are one planar topology. Each
    ring is cut into arcs at the junctions (a vertex whose neighbours differ
    between the rings that use it) and every arc is simplified once, with
    Douglas-Peucker, whichever polygon uses it. A border between two
    kommuner is therefore the same polyline in both, and a region's border
    is the same polyline as its kommuner's: no gaps or overlaps between
    neighbours, and every kommune lies exactly in its region.
  * postnummer has its own geometry (it follows roads, not kommune borders,
    and reaches into the sea) and is its own topology, with its own
    tolerance.
  * Douglas-Peucker on separate arcs can still let two arcs cross, or cut a
    bay so that an islet ends up inside its neighbour. After simplifying,
    every segment pair and every vertex is checked, exactly, on the
    quantised output; a segment at fault gets back the source vertex
    farthest from it (and both halves are simplified again), until nothing
    conflicts. Vertices are only added, so the deviation stays within the
    tolerance. Every ring keeps at least three vertices, so no island is
    lost (Anholt, Christiansø, Læsø, Samsø); only islets under MIN_ISLET_M2
    with no neighbour are dropped.
  * The result is projected to WGS84 (ETRS89 is used as WGS84, the same as
    internal/dhm does; they differ by under a metre) and quantised to
    1e-5 degrees (at most 0.56 m north-south, 0.32 m east-west in Denmark).

Output, one JSON file per layer, cmd/server/dagi/dagi-<layer>.json: a header
(source, credit, licence, tolerance, measured deviation) and features
[{code, name, ...attributes, rings}], each ring one flat array of integers in
1e-5 degrees, [lon0, lat0, dlon1, dlat1, ...]: the first vertex absolute,
then deltas; the closing vertex is implicit. The rings of a feature are
tested even-odd, so a hole is just another ring.

Stdlib only (sqlite3, struct, json): the WKB reader, the UTM inverse and the
simplification are here, so the build needs no GDAL, shapely or pyproj.

Run once a year, or on a known boundary change:

    python3 scripts/build-dagi.py ~/CoreScope/.agent-in/dagi \\
        --downloaded 2026-11-26 --out cmd/server/dagi

then update the asset checksums pinned in cmd/server/dagi_asset_test.go
(the test prints them) and run `cd cmd/server && go test -run Dagi`.
`--self-test` checks the projection and the simplifier and touches nothing.
"""
import argparse
import glob
import gzip
import hashlib
import json
import math
import os
import re
import sqlite3
import struct
import sys

FORMAT = "corescope-dagi/1"
QUANTUM = 1e-5  # degrees per output unit

OWNER = "Klimadatastyrelsen"
DATASET_NAME = "DAGI"
DATASET_TITLE = "Danmarks Administrative Geografiske Inddeling"
DATASET_URL = "https://dataforsyningen.dk/data/3901"
LICENSE_NAME = "CC BY 4.0"
LICENSE_URL = "https://creativecommons.org/licenses/by/4.0/"
TERMS_URL = "https://dataforsyningen.dk/Vilkaar"
DERIVED_NOTE = "data bearbejdet"
DANISH_MONTHS = ("januar", "februar", "marts", "april", "maj", "juni", "juli",
                 "august", "september", "oktober", "november", "december")

# Douglas-Peucker tolerance per topology group, in metres: the largest
# distance of a dropped source vertex from the simplified border. Chosen to
# meet the #740 budget (<= 400 KB raw, <= 150 KB gzipped per layer) with
# about 8-10 % margin under Go's gzip.DefaultCompression, which
# cmd/server/dagi_asset_test.go measures and enforces. For generation 271:
# kommune 314 KB / 138 KB at 60 m, postnummer 342 KB / 135 KB at 225 m (at
# 175 m postnummer was 148 KB, too close). Postnummer is the dense one:
# 1,089 polygons whose borders follow roads, 486 of them small street
# postcodes in København.
TOLERANCE_M = {"admin": 60.0, "postnummer": 225.0}

# An islet (a ring with no neighbour) smaller than this is dropped, from
# every polygon that has it. Below the output quantum (about 1 m) such a ring
# collapses: a 70 m x 3 m breakwater in København cannot be drawn without
# crossing itself. The real islands are far above it (Christiansø's three
# islets together are about 0.4 km2).
MIN_ISLET_M2 = 1000.0

# The layers, in output order. group is the topology the layer is simplified
# in; code is the column the feature's stable code comes from.
LAYERS = (
    {"layer": "kommune", "group": "admin", "entity": "Kommuneinddeling",
     "table": "kommuneinddeling", "code": "kommunekode",
     "columns": ("kommunekode", "navn", "udenforkommuneinddeling", "regionlokalid")},
    {"layer": "landsdel", "group": "admin", "entity": "Landsdel",
     "table": "landsdel", "code": "id_lokalid", "columns": ("id_lokalid", "navn")},
    {"layer": "region", "group": "admin", "entity": "Regionsinddeling",
     "table": "regionsinddeling", "code": "regionskode",
     "columns": ("regionskode", "navn", "id_lokalid")},
    {"layer": "postnummer", "group": "postnummer", "entity": "Postnummerinddeling",
     "table": "postnummerinddeling", "code": "postnummer",
     "columns": ("postnummer", "navn", "ergadepostnummer")},
)


# --- GeoPackage / WKB ------------------------------------------------------

def parse_gpkg_geometry(blob):
    """The polygons of one GeoPackage geometry blob: a list of polygons, each
    a list of rings, each a list of (x, y). Polygon and MultiPolygon, 2D or
    with Z/M (dropped)."""
    blob = bytes(blob)
    if blob[:2] != b"GP":
        raise ValueError("not a GeoPackage geometry")
    flags = blob[3]
    if flags & 0x10:
        raise ValueError("empty geometry")
    env = {0: 0, 1: 32, 2: 48, 3: 48, 4: 64}[(flags >> 1) & 7]
    polys, end = parse_wkb(blob, 8 + env)
    if end != len(blob):
        raise ValueError("trailing bytes after WKB")
    return polys


def parse_wkb(b, pos):
    """Reads one WKB Polygon or MultiPolygon at pos; returns (polygons, end)."""
    bo = "<" if b[pos] == 1 else ">"
    (typ,) = struct.unpack_from(bo + "I", b, pos + 1)
    pos += 5
    base, dim = typ % 1000, 2 + (typ // 1000 in (1, 2)) + 2 * (typ // 1000 == 3)
    if typ & 0x80000000 or typ & 0x40000000:  # EWKB-style Z/M flags
        base, dim = typ & 0xFFFF, 2 + bool(typ & 0x80000000) + bool(typ & 0x40000000)
    if base == 3:
        (nrings,) = struct.unpack_from(bo + "I", b, pos)
        pos += 4
        rings = []
        for _ in range(nrings):
            (npts,) = struct.unpack_from(bo + "I", b, pos)
            pos += 4
            vals = struct.unpack_from(bo + "%dd" % (npts * dim), b, pos)
            pos += 8 * npts * dim
            rings.append([(vals[i], vals[i + 1]) for i in range(0, len(vals), dim)])
        return [rings], pos
    if base == 6:
        (n,) = struct.unpack_from(bo + "I", b, pos)
        pos += 4
        polys = []
        for _ in range(n):
            p, pos = parse_wkb(b, pos)
            polys.extend(p)
        return polys, pos
    raise ValueError("unsupported WKB type %d" % typ)


def read_layer(path, spec):
    db = sqlite3.connect("file:%s?mode=ro" % path, uri=True)
    try:
        (srs,) = db.execute("SELECT srs_id FROM gpkg_contents WHERE table_name = ?",
                            (spec["table"],)).fetchone()
        if srs != 25832:
            raise ValueError("%s: srs %s, want 25832" % (path, srs))
        (last_change,) = db.execute("SELECT last_change FROM gpkg_contents WHERE table_name = ?",
                                    (spec["table"],)).fetchone()
        rows = []
        sql = "SELECT %s, geometri FROM %s ORDER BY fid" % (", ".join(spec["columns"]), spec["table"])
        for r in db.execute(sql):
            rows.append((dict(zip(spec["columns"], r[:-1])), parse_gpkg_geometry(r[-1])))
    finally:
        db.close()
    return rows, last_change


# --- Projection --------------------------------------------------------------

class TransverseMercator:
    """Krüger series, fourth order in n (Karney 2011): the same formulas as
    internal/dhm/utm.go, so the asset and the server agree on the projection.
    Sub-millimetre within Denmark."""

    def __init__(self, a, inv_f, k0, lon0, fe, fn):
        f = 1 / inv_f
        n = f / (2 - f)
        n2, n3, n4 = n * n, n ** 3, n ** 4
        self.k0, self.lon0, self.fe, self.fn = k0, math.radians(lon0), fe, fn
        self.e = math.sqrt(f * (2 - f))
        self.kA = k0 * a / (1 + n) * (1 + n2 / 4 + n4 / 64)
        self.alpha = (n / 2 - 2 * n2 / 3 + 5 * n3 / 16 + 41 * n4 / 180,
                      13 * n2 / 48 - 3 * n3 / 5 + 557 * n4 / 1440,
                      61 * n3 / 240 - 103 * n4 / 140,
                      49561 * n4 / 161280)
        self.beta = (n / 2 - 2 * n2 / 3 + 37 * n3 / 96 - n4 / 360,
                     n2 / 48 + n3 / 15 - 437 * n4 / 1440,
                     17 * n3 / 480 - 37 * n4 / 840,
                     4397 * n4 / 161280)

    def forward(self, lat, lon):
        phi, dl = math.radians(lat), math.radians(lon) - self.lon0
        t = math.sinh(math.atanh(math.sin(phi)) - self.e * math.atanh(self.e * math.sin(phi)))
        xi = math.atan2(t, math.cos(dl))
        eta = math.atanh(math.sin(dl) / math.sqrt(1 + t * t))
        x, y = eta, xi
        for j, a in enumerate(self.alpha):
            k = 2 * (j + 1)
            x += a * math.cos(k * xi) * math.sinh(k * eta)
            y += a * math.sin(k * xi) * math.cosh(k * eta)
        return self.fe + self.kA * x, self.fn + self.kA * y

    def inverse(self, e, n):
        xi = (n - self.fn) / self.kA
        eta = (e - self.fe) / self.kA
        xp, ep = xi, eta
        for j, b in enumerate(self.beta):
            k = 2 * (j + 1)
            xp -= b * math.sin(k * xi) * math.cosh(k * eta)
            ep -= b * math.cos(k * xi) * math.sinh(k * eta)
        chi = math.asin(math.sin(xp) / math.cosh(ep))
        phi = chi
        for _ in range(20):
            s = self.e * math.sin(phi)
            nxt = 2 * math.atan(math.tan(math.pi / 4 + chi / 2) * ((1 + s) / (1 - s)) ** (self.e / 2)) - math.pi / 2
            if abs(nxt - phi) < 1e-15:
                phi = nxt
                break
            phi = nxt
        dl = math.atan2(math.sinh(ep), math.cos(xp))
        return math.degrees(phi), math.degrees(self.lon0 + dl)


UTM32 = TransverseMercator(6378137, 298.257222101, 0.9996, 9, 500000, 0)


# --- Topology and simplification --------------------------------------------
#
# Vertices are integer centimetres (the source has two decimals), so the
# topology is exact: two rings share a vertex when the integers are equal.

def cm(p):
    return (round(p[0] * 100), round(p[1] * 100))


def clean_ring(ring):
    """The ring as cm vertices, without the closing repeat or consecutive
    duplicates."""
    out = []
    for p in ring:
        q = cm(p)
        if not out or out[-1] != q:
            out.append(q)
    while len(out) > 1 and out[0] == out[-1]:
        out.pop()
    return out


def build_topology(rings):
    """Cuts every ring into arcs at its junctions. Returns (arcs, refs):
    arcs a list of vertex lists (an arc is shared, stored once), refs per
    ring a list of (arc index, reversed). A ring with no junction is one
    closed arc, rotated to start at its smallest vertex, so the same ring in
    two polygons (an enclave and its hole) is the same arc."""
    pair = {}
    junction = set()
    for r in rings:
        n = len(r)
        for i, v in enumerate(r):
            p, q = r[i - 1], r[(i + 1) % n]
            nb = (p, q) if p < q else (q, p)
            seen = pair.get(v)
            if seen is None:
                pair[v] = nb
            elif seen != nb:
                junction.add(v)
    del pair
    arcs, index, refs = [], {}, []
    for r in rings:
        n = len(r)
        cuts = [i for i, v in enumerate(r) if v in junction]
        pieces = []
        if not cuts:
            k = r.index(min(r))
            rot = r[k:] + r[:k]
            fwd = rot + [rot[0]]
            back = [rot[0]] + rot[:0:-1] + [rot[0]]
            pieces.append(fwd if fwd <= back else back)
            rev_flags = [fwd > back]
        else:
            rev_flags = []
            for a, b in zip(cuts, cuts[1:] + [cuts[0] + n]):
                piece = [r[i % n] for i in range(a, b + 1)]
                back = piece[::-1]
                pieces.append(piece if piece <= back else back)
                rev_flags.append(piece > back)
        ring_refs = []
        for piece, rev in zip(pieces, rev_flags):
            key = tuple(piece)
            idx = index.get(key)
            if idx is None:
                idx = index[key] = len(arcs)
                arcs.append(piece)
            ring_refs.append((idx, rev))
        refs.append(ring_refs)
    return arcs, refs


def seg_dist2(p, a, b):
    ax, ay = a
    dx, dy = b[0] - ax, b[1] - ay
    px, py = p[0] - ax, p[1] - ay
    l2 = dx * dx + dy * dy
    if l2 == 0:
        return px * px + py * py
    t = max(0.0, min(1.0, (px * dx + py * dy) / l2))
    ex, ey = px - t * dx, py - t * dy
    return ex * ex + ey * ey


def douglas_peucker(pts, tol, keep_min=2):
    """The indices of pts that Douglas-Peucker keeps at tol (same unit as
    pts), endpoints always kept, and the largest distance of a dropped vertex
    from the kept polyline. A closed arc (first == last) keeps its farthest
    vertex from the start too, and keep_min forces at least that many kept
    vertices (the most distant ones first)."""
    n = len(pts)
    if n <= 2:
        return list(range(n)), 0.0
    keep = [False] * n
    keep[0] = keep[-1] = True
    if pts[0] == pts[-1]:
        far = max(range(1, n - 1), key=lambda i: (pts[i][0] - pts[0][0]) ** 2 + (pts[i][1] - pts[0][1]) ** 2)
        keep[far] = True
        stack = [(0, far), (far, n - 1)]
    else:
        stack = [(0, n - 1)]
    tol2 = tol * tol
    worst = 0.0
    dropped = []  # (dist2, index, a, b) of the best split refused per span
    while stack:
        a, b = stack.pop()
        if b - a < 2:
            continue
        best, bi = -1.0, -1
        pa, pb = pts[a], pts[b]
        for i in range(a + 1, b):
            d = seg_dist2(pts[i], pa, pb)
            if d > best:
                best, bi = d, i
        if best > tol2:
            keep[bi] = True
            stack.append((a, bi))
            stack.append((bi, b))
        else:
            worst = max(worst, best)
            dropped.append((best, bi, a, b))
    kept = sum(keep)
    # Force keep_min: split the spans whose best candidate is farthest.
    while kept < keep_min and dropped:
        dropped.sort()
        best, bi, a, b = dropped.pop()
        keep[bi] = True
        kept += 1
        for lo, hi in ((a, bi), (bi, b)):
            if hi - lo >= 2:
                cb, ci = max((seg_dist2(pts[i], pts[lo], pts[hi]), i) for i in range(lo + 1, hi))
                dropped.append((cb, ci, lo, hi))
        worst = max([d for d, _, _, _ in dropped], default=0.0)
    return [i for i in range(n) if keep[i]], math.sqrt(worst)


# --- Exact checks on the quantised output -----------------------------------

def orient(a, b, c):
    v = (b[0] - a[0]) * (c[1] - a[1]) - (b[1] - a[1]) * (c[0] - a[0])
    return (v > 0) - (v < 0)


def on_seg(a, b, p):
    return min(a[0], b[0]) <= p[0] <= max(a[0], b[0]) and min(a[1], b[1]) <= p[1] <= max(a[1], b[1])


def segments_conflict(a, b, c, d):
    """Whether segments ab and cd meet anywhere other than at an endpoint
    they share. Integer coordinates, exact."""
    shared = {a, b} & {c, d}
    if len(shared) == 2:
        return False  # the same segment, once per ring that uses it
    o1, o2, o3, o4 = orient(a, b, c), orient(a, b, d), orient(c, d, a), orient(c, d, b)
    if shared:
        s = shared.pop()
        # Two segments from one vertex conflict only if they overlap.
        x = d if c == s else c
        y = b if a == s else a
        return orient(s, y, x) == 0 and (x[0] - s[0]) * (y[0] - s[0]) + (x[1] - s[1]) * (y[1] - s[1]) > 0
    if o1 != o2 and o3 != o4:
        return True
    return ((o1 == 0 and on_seg(a, b, c)) or (o2 == 0 and on_seg(a, b, d)) or
            (o3 == 0 and on_seg(c, d, a)) or (o4 == 0 and on_seg(c, d, b)))


def crossing_segments(arc_pts, cell):
    """The (arc, segment) pairs involved in a segment conflict between (or
    within) arcs. Segments are bucketed on a grid of `cell` units."""
    grid = {}
    segs = []
    for ai, pts in enumerate(arc_pts):
        for i in range(len(pts or ()) - 1):
            a, b = pts[i], pts[i + 1]
            si = len(segs)
            segs.append((a, b, ai, i))
            for gx in range(min(a[0], b[0]) // cell, max(a[0], b[0]) // cell + 1):
                for gy in range(min(a[1], b[1]) // cell, max(a[1], b[1]) // cell + 1):
                    grid.setdefault((gx, gy), []).append(si)
    bad = set()
    seen = set()
    for bucket in grid.values():
        for x in range(len(bucket)):
            for y in range(x + 1, len(bucket)):
                i, j = bucket[x], bucket[y]
                if (i, j) in seen:
                    continue
                seen.add((i, j))
                a, b, ai, ia = segs[i]
                c, d, aj, ja = segs[j]
                if ai == aj and abs(ia - ja) == 1:
                    continue  # consecutive segments of one arc share a vertex legitimately
                if segments_conflict(a, b, c, d):
                    bad.add((ai, ia))
                    bad.add((aj, ja))
    return bad


def bbox(pts):
    xs = [p[0] for p in pts]
    ys = [p[1] for p in pts]
    return min(xs), min(ys), max(xs), max(ys)


class FeatureIndex:
    """The edges of one feature's rings, bucketed in horizontal bands, for
    an exact even-odd point test that reads only the edges of one band."""

    def __init__(self, rings, band=500):  # 500 units of 1e-5 deg, about 550 m
        self.BAND = band
        self.box = bbox([p for r in rings for p in r])
        self.bands = {}
        for r in rings:
            for a, b in zip(r, r[1:]):
                for k in range(min(a[1], b[1]) // self.BAND, max(a[1], b[1]) // self.BAND + 1):
                    self.bands.setdefault(k, []).append((a, b))

    def locate(self, p):
        """1 strictly inside, 0 outside, -1 on the boundary."""
        b = self.box
        if not (b[0] <= p[0] <= b[2] and b[1] <= p[1] <= b[3]):
            return 0
        x, y = p
        inside = False
        for a, c in self.bands.get(y // self.BAND, ()):
            if orient(a, c, p) == 0 and on_seg(a, c, p):
                return -1
            if (a[1] > y) != (c[1] > y):
                lhs = (x - a[0]) * (c[1] - a[1])
                rhs = (y - a[1]) * (c[0] - a[0])
                if (lhs < rhs) == (c[1] > a[1]):
                    inside = not inside
        return 1 if inside else 0


def swallowed_segments(features_rings, arc_pts, feature_arcs, reach):
    """Segments that let a vertex of one feature lie strictly inside another
    feature of the same layer: an islet or a coast swallowed by a
    neighbour's simplified coast, where no two segments need cross. A
    shared vertex lies on the other feature's boundary, which is allowed.
    The culprit is a segment of the containing feature within the
    tolerance of the vertex (the area between a chord and the polyline it
    replaced lies within the tolerance of the chord), so every such segment,
    bbox grown by reach units, is returned."""
    index = [FeatureIndex(rings) for rings in features_rings]
    cell = 20000
    grid = {}
    for fi, fx in enumerate(index):
        b = fx.box
        for gx in range(b[0] // cell, b[2] // cell + 1):
            for gy in range(b[1] // cell, b[3] // cell + 1):
                grid.setdefault((gx, gy), []).append(fi)
    bad = set()
    for fi, rings in enumerate(features_rings):
        for p in {p for r in rings for p in r}:
            for fj in grid.get((p[0] // cell, p[1] // cell), ()):
                if fj != fi and index[fj].locate(p) == 1:
                    for a in feature_arcs[fj]:
                        pts = arc_pts[a]
                        for k in range(len(pts) - 1):
                            u, v = pts[k], pts[k + 1]
                            if (min(u[0], v[0]) - reach <= p[0] <= max(u[0], v[0]) + reach and
                                    min(u[1], v[1]) - reach <= p[1] <= max(u[1], v[1]) + reach):
                                bad.add((a, k))
    return bad


# --- Build ---------------------------------------------------------------------

def to_units(p_cm):
    lat, lon = UTM32.inverse(p_cm[0] / 100.0, p_cm[1] / 100.0)
    return (round(lon / QUANTUM), round(lat / QUANTUM))


def farthest(pts, i0, i1):
    """The interior vertex of pts[i0..i1] farthest from the chord, or None."""
    if i1 - i0 < 2:
        return None
    return max(range(i0 + 1, i1), key=lambda i: (seg_dist2(pts[i], pts[i0], pts[i1]), -i))


def quantise_arc(arc, kept, proj):
    """The quantised polyline of an arc's kept source vertices (indices into
    arc, ascending), consecutive repeats merged, and per segment k of it the
    source index range (i0, i1) it stands for."""
    pts, sp = [], []
    for i in kept:
        u = proj(arc[i])
        if pts and pts[-1] == u:
            if sp:
                sp[-1] = (sp[-1][0], i)
            continue  # before the first segment: it starts at kept[0] anyway
        if pts:
            sp.append((sp[-1][1] if sp else kept[0], i))
        pts.append(u)
    return pts, sp


def simplify_group(layers, tol_m, log):
    """Simplifies the rings of every feature of the layers in one topology.
    Returns per layer per feature its rings in quantised units (closed), and
    the stats.

    Each arc is simplified once with Douglas-Peucker; then, on the quantised
    output, every conflict (two segments that cross or overlap, a vertex
    strictly inside another feature, a ring with fewer than three distinct
    vertices) is resolved locally: the segment at fault gets back the source
    vertex farthest from it, the next Douglas-Peucker split. Only vertices
    are added, so the deviation never exceeds the tolerance."""
    rings, owner = [], []
    for li, (spec, feats) in enumerate(layers):
        for fi, (_, polys) in enumerate(feats):
            for poly in polys:
                for ring in poly:
                    r = clean_ring(ring)
                    if len(r) < 3:
                        raise ValueError("%s %d: degenerate source ring" % (spec["layer"], fi))
                    rings.append(r)
                    owner.append((li, fi))
    arcs, refs = build_topology(rings)
    log("  %d rings, %d source vertices, %d arcs" % (len(rings), sum(map(len, rings)), len(arcs)))
    tiny, tiny_area = set(), 0.0
    for ri, rr in enumerate(refs):
        if len(rr) == 1 and rr[0][0] not in tiny:
            area = abs(ring_area_units(rings[ri] + rings[ri][:1])) / 1e4  # cm2 -> m2
            if area < MIN_ISLET_M2:
                tiny.add(rr[0][0])
                tiny_area += area
    live = [ri for ri, rr in enumerate(refs) if not (len(rr) == 1 and rr[0][0] in tiny)]
    for li, (spec, feats) in enumerate(layers):
        have = {owner[ri][1] for ri in live if owner[ri][0] == li}
        if len(have) != len(feats):
            raise ValueError("%s: a feature lost every ring" % spec["layer"])
    log("  %d islets under %g m2 dropped (%.0f m2)" % (len(tiny), MIN_ISLET_M2, tiny_area))

    projected = {}

    def proj(v):
        u = projected.get(v)
        if u is None:
            u = projected[v] = to_units(v)
        return u

    # keep[a]: the kept source indices of arc a. A closed arc (an islet)
    # keeps four (start, two more, start), so it stays a triangle.
    keep = [None] * len(arcs)
    for a in range(len(arcs)):
        if a not in tiny:
            closed = arcs[a][0] == arcs[a][-1]
            keep[a] = douglas_peucker(arcs[a], tol_m * 100.0, 4 if closed else 2)[0]

    # out[a]: the quantised polyline, consecutive repeats merged; span[a][k]:
    # the source index range segment k of out[a] stands for.
    out, span = [None] * len(arcs), [None] * len(arcs)

    def quantise(a):
        out[a], span[a] = quantise_arc(arcs[a], keep[a], proj)

    for a in range(len(arcs)):
        if keep[a] is not None:
            quantise(a)
    reach = int(2 * tol_m) + 2  # units: 1e-5 deg is >= 0.63 m in Denmark
    rounds = added = 0
    while True:
        rounds += 1
        ring_pts = {}
        for ri in live:
            pts = []
            for a, rev in refs[ri]:
                seq = out[a][::-1] if rev else out[a]
                pts.extend(seq if not pts else seq[1:])
            if pts[0] != pts[-1]:
                pts.append(pts[0])
            ring_pts[ri] = pts
        bad = crossing_segments(out, 2000)
        feats_rings, feat_arcs = {}, {}
        for ri in live:
            li, fi = owner[ri]
            feats_rings.setdefault(li, {}).setdefault(fi, []).append(ring_pts[ri])
            feat_arcs.setdefault(li, {}).setdefault(fi, set()).update(a for a, _ in refs[ri])
        for li in feats_rings:
            n = len(layers[li][1])
            bad |= swallowed_segments([feats_rings[li][fi] for fi in range(n)], out,
                                      [feat_arcs[li][fi] for fi in range(n)], reach)
        for ri in live:
            if len(set(ring_pts[ri][:-1])) < 3:
                # The segment that stands for the most source vertices.
                best = max(((span[a][k][1] - span[a][k][0], a, k) for a, _ in refs[ri]
                            for k in range(len(span[a]))), default=None)
                if best is None or best[0] < 2:
                    raise ValueError("ring %d cannot keep three vertices" % ri)
                bad.add((best[1], best[2]))
        log("  round %d: %d conflicting segments" % (rounds, len(bad)))
        if not bad:
            break
        # A segment that is already a source segment cannot be refined; it
        # was flagged with the one at fault (the other of a crossing pair, or
        # another segment near a swallowed vertex). No progress at all means
        # the conflict is in the quantised source itself.
        touched = set()
        for a, k in sorted(bad):
            i0, i1 = span[a][k]
            i = farthest(arcs[a], i0, i1)
            if i is not None:
                # Split at the farthest vertex and simplify both halves
                # again, so no dropped vertex ends up farther than the
                # tolerance from the new, shorter chords.
                before = len(set(keep[a]))
                keep[a].append(i)
                for lo, hi in ((i0, i), (i, i1)):
                    sub, _ = douglas_peucker(arcs[a][lo:hi + 1], tol_m * 100.0)
                    keep[a].extend(lo + j for j in sub)
                keep[a] = sorted(set(keep[a]))
                touched.add(a)
                added += len(keep[a]) - before
        if not touched:
            raise ValueError("conflicts with no source vertex left to restore: %s" % sorted(bad)[:10])
        for a in touched:
            quantise(a)
    worst = 0.0
    for a in range(len(arcs)):
        if keep[a] is None:
            continue
        pts, ks = arcs[a], keep[a]
        for i0, i1 in zip(ks, ks[1:]):
            for i in range(i0 + 1, i1):
                worst = max(worst, seg_dist2(pts[i], pts[i0], pts[i1]))
    result = [[feats_rings[li][fi] for fi in range(len(feats))] for li, (_, feats) in enumerate(layers)]
    stats = {
        "toleranceM": tol_m,
        "maxDeviationM": round(math.sqrt(worst) / 100.0, 2),
        "conflictVerticesAdded": added,
        "sourceVertices": sum(map(len, rings)),
        "droppedIslets": len(tiny),
        "droppedIsletAreaM2": round(tiny_area),
        "minIsletM2": MIN_ISLET_M2,
        "arcs": len(arcs),
        "rounds": rounds,
    }
    return result, stats


def delta_ring(pts):
    """[lon0, lat0, dlon, dlat, ...] without the closing vertex."""
    flat = [pts[0][0], pts[0][1]]
    for a, b in zip(pts[:-2], pts[1:-1]):
        flat.append(b[0] - a[0])
        flat.append(b[1] - a[1])
    return flat


def ring_area_units(pts):
    s = 0
    for a, b in zip(pts, pts[1:]):
        s += a[0] * b[1] - b[0] * a[1]
    return s / 2


def source_area_m2(polys):
    """The area of the source geometry in m2 (EPSG:25832 is conformal, not
    equal-area; its scale is within 0.15 % of 1 in Denmark, so the area within 0.3 %): per polygon, its exterior minus its
    holes, as WKB marks them."""
    total = 0.0
    for poly in polys:
        areas = [abs(ring_area_units([cm(p) for p in r])) / 1e4 for r in poly]
        total += areas[0] - sum(areas[1:])
    return total


def feature_record(spec, attrs, region_codes):
    layer = spec["layer"]
    code = str(attrs[spec["code"]])
    rec = {"code": code, "name": attrs["navn"]}
    if layer == "kommune":
        rec["region"] = region_codes[str(attrs["regionlokalid"])]
        if int(attrs["udenforkommuneinddeling"]):
            rec["outsideKommune"] = True
    elif layer == "postnummer":
        if int(attrs["ergadepostnummer"]):
            rec["street"] = True
    return rec


def month_year(day):
    return "%s %s" % (DANISH_MONTHS[int(day[5:7]) - 1], day[:4])


def credit(downloaded):
    return "Indeholder data fra %s, %s, %s, %s — %s" % (
        OWNER, DATASET_NAME, month_year(downloaded), LICENSE_NAME, DERIVED_NOTE)


def file_sha256(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def find_source(src_dir, entity):
    pat = os.path.join(src_dir, "DAGI_V4_%s_TotalDownload_gpkg_Current_*.gpkg" % entity)
    hits = sorted(glob.glob(pat))
    if len(hits) != 1:
        raise SystemExit("want exactly one %s, found %d" % (pat, len(hits)))
    m = re.search(r"_Current_(\d+)\.gpkg$", hits[0])
    return hits[0], int(m.group(1))


def encode(doc):
    return (json.dumps(doc, ensure_ascii=False, separators=(",", ":")) + "\n").encode("utf-8")


def build(src_dir, out_dir, downloaded, tolerance, log):
    loaded = {}
    for spec in LAYERS:
        path, gen = find_source(src_dir, spec["entity"])
        rows, last_change = read_layer(path, spec)
        loaded[spec["layer"]] = (spec, rows, {
            "register": "DAGI",
            "entity": spec["entity"],
            "generation": gen,
            "typeOfData": "Current",
            "file": os.path.basename(path),
            "sha256": file_sha256(path),
            "lastChange": last_change,
            "features": len(rows),
        })
        log("%s: %d features from %s" % (spec["layer"], len(rows), os.path.basename(path)))
    region_codes = {str(a["id_lokalid"]): str(a["regionskode"]) for a, _ in loaded["region"][1]}
    groups = {}
    for spec in LAYERS:
        groups.setdefault(spec["group"], []).append(spec["layer"])
    results = {}
    for group, names in groups.items():
        log("group %s (%s), tolerance %g m" % (group, ", ".join(names), tolerance[group]))
        layers = [(loaded[n][0], loaded[n][1]) for n in names]
        rings, stats = simplify_group(layers, tolerance[group], log)
        for n, r in zip(names, rings):
            results[n] = (r, stats)
    sizes = {}
    for spec in LAYERS:
        name = spec["layer"]
        _, rows, source = loaded[name]
        feat_rings, stats = results[name]
        features = []
        for (attrs, polys), rings in zip(rows, feat_rings):
            rec = feature_record(spec, attrs, region_codes)
            rec["areaM2"] = round(source_area_m2(polys))
            rec["rings"] = [delta_ring(r) for r in rings]
            features.append(rec)
        features.sort(key=lambda f: f["code"])
        doc = {
            "format": FORMAT,
            "layer": name,
            "source": dict(source, downloaded=downloaded),
            "credit": credit(downloaded),
            "owner": OWNER,
            "dataset": "%s (%s)" % (DATASET_NAME, DATASET_TITLE),
            "datasetUrl": DATASET_URL,
            "license": LICENSE_NAME,
            "licenseUrl": LICENSE_URL,
            "termsUrl": TERMS_URL,
            "crs": "EPSG:4326 (from EPSG:25832; ETRS89 used as WGS84)",
            "quantum": QUANTUM,
            "encoding": "rings: [lon0, lat0, dlon, dlat, ...] in quantum degrees, closing vertex implicit, even-odd",
            "simplify": dict(stats, method="Douglas-Peucker on shared arcs, topology-checked",
                             group=spec["group"]),
            "generatedBy": "scripts/build-dagi.py",
            "features": features,
        }
        data = encode(doc)
        path = os.path.join(out_dir, "dagi-%s.json" % name)
        with open(path, "wb") as f:
            f.write(data)
        # zlib level 6; Go's DefaultCompression, which the server and the
        # budget test use, comes out about 5 % larger.
        gz = len(gzip.compress(data, 6, mtime=0))
        verts = sum(len(r) // 2 for ft in features for r in ft["rings"])
        sizes[name] = (len(data), gz, verts)
        log("%-10s %4d features %6d vertices %7d B raw %6d B gzip-6  max deviation %.2f m (tolerance %g m, %d vertices added)" % (
            name, len(features), verts, len(data), gz, stats["maxDeviationM"], stats["toleranceM"],
            stats["conflictVerticesAdded"]))
    return sizes


# --- Self-test ---------------------------------------------------------------

def self_test():
    # Reference values from internal/dhm.ToUTM32 (the server's projection).
    for lat, lon in ((56.15674, 10.21076), (55.67594, 12.56553), (55.10091, 14.70664), (57.72, 10.58)):
        e, n = UTM32.forward(lat, lon)
        la, lo = UTM32.inverse(e, n)
        assert abs(la - lat) < 1e-10 and abs(lo - lon) < 1e-10, (lat, lon, la, lo)
    for (lat, lon), (e0, n0) in (((56.0, 9.0), (500000.0, 6206079.587128)),
                                 ((56.15674, 10.21076), (575204.432995, 6224184.554368)),
                                 ((55.10091, 14.70664), (863920.292688, 6120905.012095))):
        e, n = UTM32.forward(lat, lon)
        assert abs(e - e0) < 1e-5 and abs(n - n0) < 1e-5, (lat, lon, e, n)
    la, lo = UTM32.inverse(574000, 6224000)
    assert abs(la - 56.1552705869) < 1e-9 and abs(lo - 10.1913225870) < 1e-9, (la, lo)
    # Douglas-Peucker keeps the endpoints and anything beyond tolerance.
    pts = [(0, 0), (1, 0.1), (2, -0.1), (3, 5), (4, 0), (5, 0)]
    idx, dev = douglas_peucker(pts, 1.0)
    assert idx == [0, 2, 3, 5], idx  # (4, 0) is 0.93 from (3, 5)-(5, 0)
    assert dev <= 1.0
    # A closed ring keeps a triangle even at a huge tolerance.
    sq = [(0, 0), (10, 0), (10, 10), (0, 10), (0, 0)]
    idx, _ = douglas_peucker(sq, 1e9, 4)
    assert len(idx) >= 4 and idx[0] == 0 and idx[-1] == 4, idx
    # Topology: two squares sharing an edge share one arc.
    a = [(0, 0), (10, 0), (10, 10), (0, 10)]
    b = [(10, 0), (20, 0), (20, 10), (10, 10)]
    arcs, refs = build_topology([a, b])
    shared = set(x for x, _ in refs[0]) & set(x for x, _ in refs[1])
    assert len(shared) == 1, (arcs, refs)
    # Crossing detection and the farthest split.
    assert farthest([(0, 0), (1, 1), (2, 3), (3, 0)], 0, 3) == 2
    assert farthest([(0, 0), (1, 0)], 0, 1) is None
    assert segments_conflict((0, 0), (10, 10), (0, 10), (10, 0))
    assert not segments_conflict((0, 0), (10, 0), (10, 0), (20, 5))
    assert segments_conflict((0, 0), (10, 0), (0, 0), (5, 0))
    fx = FeatureIndex([sq])
    assert fx.locate((5, 5)) == 1 and fx.locate((10, 5)) == -1 and fx.locate((11, 5)) == 0
    holed = FeatureIndex([sq, [(4, 4), (6, 4), (6, 6), (4, 6), (4, 4)]])
    assert holed.locate((5, 5)) == 0 and holed.locate((2, 5)) == 1 and holed.locate((8, 5)) == 1
    # WKB: a little-endian Polygon with one triangle.
    wkb = struct.pack("<BII", 1, 3, 1) + struct.pack("<I", 4) + struct.pack("<8d", 0, 0, 1, 0, 0, 1, 0, 0)
    polys, end = parse_wkb(wkb, 0)
    assert end == len(wkb) and polys == [[[(0, 0), (1, 0), (0, 1), (0, 0)]]], polys
    gp = b"GP\x00\x01" + struct.pack("<i", 25832) + wkb
    assert parse_gpkg_geometry(gp) == polys
    # With an XY envelope (flags bits 1-3 = 1: 4 doubles) before the WKB.
    gp = b"GP\x00\x03" + struct.pack("<i", 25832) + struct.pack("<4d", 0, 1, 0, 1) + wkb
    assert parse_gpkg_geometry(gp) == polys
    # The same triangle big-endian, and a big-endian MultiPolygon holding it
    # and the little-endian one (every WKB geometry has its own byte order).
    wkb_be = struct.pack(">BII", 0, 3, 1) + struct.pack(">I", 4) + struct.pack(">8d", 0, 0, 1, 0, 0, 1, 0, 0)
    assert parse_wkb(wkb_be, 0) == (polys, len(wkb_be))
    multi = struct.pack(">BII", 0, 6, 2) + wkb_be + wkb
    assert parse_wkb(multi, 0) == (polys + polys, len(multi))
    assert source_area_m2([[[(0, 0), (10, 0), (10, 10), (0, 10), (0, 0)],
                            [(2, 2), (4, 2), (4, 4), (2, 4), (2, 2)]]]) == 96.0
    assert credit("2026-11-26") == ("Indeholder data fra Klimadatastyrelsen, DAGI, november 2026, "
                                    "CC BY 4.0 — data bearbejdet")
    # A code keeps its leading zero (kommunekode 0101).
    kommune = next(s for s in LAYERS if s["layer"] == "kommune")
    rec = feature_record(kommune, {"kommunekode": "0101", "navn": "København", "udenforkommuneinddeling": 0,
                                   "regionlokalid": "x"}, {"x": "1084"})
    assert rec == {"code": "0101", "name": "København", "region": "1084"}, rec
    self_test_rings()
    self_test_simplify_group()
    print("self-test ok")


def self_test_rings():
    # clean_ring: centimetres, no consecutive repeat, no closing vertex.
    assert clean_ring([(0, 0), (1, 0), (1, 0), (1, 1), (0, 0), (0, 0)]) == [(0, 0), (100, 0), (100, 100)]
    # delta_ring: the first vertex absolute, then deltas, the closing one left out.
    assert delta_ring([(1, 2), (4, 6), (3, 3), (1, 2)]) == [1, 2, 3, 4, -1, -3]
    # quantise_arc: vertices that land on one output point merge, and each
    # segment spans the source indices from its first to its last vertex.
    def proj(p):
        return p[0] // 10, p[1] // 10
    arc = [(0, 0), (12, 0), (15, 0), (25, 0), (31, 0), (35, 0)]
    assert quantise_arc(arc, range(6), proj) == ([(0, 0), (1, 0), (2, 0), (3, 0)], [(0, 2), (2, 3), (3, 5)])
    assert quantise_arc(arc, [0, 3, 5], proj) == ([(0, 0), (2, 0), (3, 0)], [(0, 3), (3, 5)])
    # Merged before the first segment: that segment still starts at the first kept vertex.
    assert quantise_arc([(0, 0), (5, 0), (15, 0)], [0, 1, 2], proj) == ([(0, 0), (1, 0)], [(0, 2)])
    # crossing_segments: two segments of one arc that cross are a conflict;
    # consecutive ones, and the closing pair of a closed arc, are not.
    assert crossing_segments([[(0, 0), (10, 0), (10, 10), (5, -5)]], 4) == {(0, 0), (0, 2)}
    assert crossing_segments([[(0, 0), (10, 0), (10, 10), (0, 10), (0, 0)]], 4) == set()
    assert crossing_segments([[(0, 0), (10, 10)], [(0, 10), (10, 0)]], 4) == {(0, 0), (1, 0)}
    # swallowed_segments: a vertex strictly inside a neighbour flags the
    # neighbour's segments within reach of it; a vertex on its border does not.
    big = [(0, 0), (100, 0), (100, 100), (0, 100), (0, 0)]
    inside = [(40, 5), (60, 5), (50, 10), (40, 5)]
    touching = [(100, 50), (120, 40), (120, 60), (100, 50)]
    feats = [[big], [inside], [touching]]
    assert swallowed_segments(feats, [big, inside, touching], [{0}, {1}, {2}], 12) == {(0, 0)}
    assert swallowed_segments([[big], [touching]], [big, touching], [{0}, {1}], 12) == set()


def self_test_simplify_group():
    """simplify_group on synthetic features (metres in EPSG:25832, near 56 N
    9 E) at a 60 m tolerance."""
    x0, y0 = 500000, 6200000

    def poly(*rings):
        return [[(x0 + x, y0 + y) for x, y in r] for r in rings]

    def sq(xa, ya, xb, yb):
        return [(xa, ya), (xb, ya), (xb, yb), (xa, yb)]

    def unit(x, y):
        return to_units(cm((x0 + x, y0 + y)))

    notch = (1000, 1941)
    feats = [
        # A: a square whose top edge has a notch 59 m deep and a bump 55 m
        # high, which Douglas-Peucker alone cuts off at 60 m ...
        [poly([(0, 0), (2000, 0), (2000, 2000), (1900, 2055), (1800, 2000), (1100, 2000), notch,
               (900, 2000), (0, 2000)])],
        # B: ... swallowing this 1,440 m2 islet in the notch. Restoring the
        # notch frees it, and leaves the bump 60.8 m from the new chord, so
        # that half must be simplified again.
        [poly(sq(960, 1980, 1040, 1998))],
        # C: a body and a 400 m2 islet, under MIN_ISLET_M2.
        [poly(sq(3000, 0, 4000, 1000)), poly(sq(5000, 5000, 5020, 5020))],
        # E: a hole that is F's 400 m2 enclave; F: the enclave and a body.
        [poly(sq(0, 3000, 1000, 4000), sq(400, 3400, 420, 3420))],
        [poly(sq(400, 3400, 420, 3420)), poly(sq(2000, 3000, 3000, 4000))],
        # G, H: neighbours whose shared border wiggles 30 m.
        [poly([(0, 6000), (1000, 6000), (1000, 6200), (1030, 6500), (1000, 6800), (1000, 7000), (0, 7000)])],
        [poly([(1000, 6000), (2000, 6000), (2000, 7000), (1000, 7000), (1000, 6800), (1030, 6500), (1000, 6200)])],
        # K, L: a lens on K's edge, 50 m high: its coast and the border it
        # shares with K both simplify to the same chord, two vertices.
        [poly([(0, 9000), (1000, 9000), (1000, 10000), (700, 10000), (300, 10000), (0, 10000)])],
        [poly([(300, 10000), (700, 10000), (650, 10040), (500, 10050), (350, 10040)])],
    ]
    layer = ({"layer": "test"}, [({}, p) for p in feats])
    (out,), stats = simplify_group([layer], 60.0, lambda m: None)
    a, g, h = out[0], out[5], out[6]
    # The islet rule: both small islets go, the enclave from E and F alike,
    # each counted once; B's islet is above the limit and stays.
    assert stats["droppedIslets"] == 2 and stats["droppedIsletAreaM2"] == 800, stats
    assert [len(x) for x in out] == [1] * len(feats), [len(x) for x in out]
    # The conflict pass: B's islet is not inside A, because A got the notch
    # back; the bump came back with it (the deviation below); L's ring has
    # three vertices again (the check below).
    assert stats["conflictVerticesAdded"] > 0 and stats["rounds"] > 1, stats
    assert unit(*notch) in a[0] and unit(1900, 2055) in a[0], a
    index = [FeatureIndex(rings) for rings in out]
    for i, rings in enumerate(out):
        for r in rings:
            assert r[0] == r[-1] and len(set(r)) >= 3, (i, r)
            for p in r:
                for j, fx in enumerate(index):
                    assert j == i or fx.locate(p) != 1, ("vertex of", i, "inside", j, p)
    assert 0 < stats["maxDeviationM"] <= 60.0, stats
    # G and H keep one shared border, simplified once to a straight edge.
    edge = {unit(1000, 6000), unit(1000, 7000)}
    for r in (g[0], h[0]):
        assert any({p, q} == edge for p, q in zip(r, r[1:])), r
    # A feature with nothing but a small islet would vanish: refused.
    try:
        simplify_group([({"layer": "test"}, [({}, [poly(sq(0, 0, 20, 20))])])], 60.0, lambda m: None)
    except ValueError as err:
        assert "lost every ring" in str(err), err
    else:
        raise AssertionError("a feature that lost every ring was accepted")


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("src", nargs="?", help="directory with the four DAGI GPKG files")
    ap.add_argument("--downloaded", help="download date of the GPKG files, YYYY-MM-DD")
    ap.add_argument("--out", default=os.path.join(os.path.dirname(os.path.abspath(__file__)),
                                                  "..", "cmd", "server", "dagi"))
    ap.add_argument("--tolerance-admin", type=float, default=TOLERANCE_M["admin"])
    ap.add_argument("--tolerance-postnummer", type=float, default=TOLERANCE_M["postnummer"])
    ap.add_argument("--self-test", action="store_true")
    args = ap.parse_args()
    if args.self_test:
        self_test()
        return
    if not args.src or not args.downloaded or not re.fullmatch(r"\d{4}-\d{2}-\d{2}", args.downloaded):
        ap.error("src and --downloaded YYYY-MM-DD are required")
    os.makedirs(args.out, exist_ok=True)
    tol = {"admin": args.tolerance_admin, "postnummer": args.tolerance_postnummer}
    build(args.src, args.out, args.downloaded, tol, lambda m: print(m, file=sys.stderr, flush=True))


if __name__ == "__main__":
    main()
