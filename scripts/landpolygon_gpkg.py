"""Shared reader for Klimadatastyrelsen's Topografisk Landpolygon GeoPackage
(#739). Both scripts/gen-dhm-foreign-land.py (the DHM coverage gate, M1)
and scripts/gen-og-denmark.py (the og:image outline, M2) read Danish land
through this module, so the two can only differ in the layer and the
simplification they choose, never in how the source is read.

A GeoPackage is SQLite; each geometry is a GP header, an optional envelope
(size from flags bits 1-3) and standard WKB, in EPSG:25832. Only stdlib
sqlite3 and shapely are needed; no GDAL.
"""
import hashlib
import sqlite3

import shapely

DANISH_MONTHS = ("januar", "februar", "marts", "april", "maj", "juni", "juli",
                 "august", "september", "oktober", "november", "december")
TERMS_URL = "https://www.sdfe.dk/Media/638052180760639799/"


def read_layer(path, table, region=None):
    """The polygons of a GeoPackage layer, and the layer's last_change.
    With region, only the polygons that meet it, clipped to it."""
    db = sqlite3.connect("file:%s?mode=ro" % path, uri=True)
    out = []
    for (blob,) in db.execute("SELECT geometri FROM %s ORDER BY ogc_fid" % table):
        if blob[:2] != b"GP":
            raise ValueError("%s: not a GeoPackage geometry" % table)
        env = {0: 0, 1: 32, 2: 48, 3: 48, 4: 64}[(blob[3] >> 1) & 7]
        g = shapely.from_wkb(bytes(blob[8 + env:]))
        if region is None:
            out.append(g)
        elif g.intersects(region):
            out.append(g.intersection(region))
    (changed,) = db.execute("SELECT last_change FROM gpkg_contents WHERE table_name = ?", (table,)).fetchone()
    db.close()
    return out, changed


def attribution(last_change):
    """The attribution line the KDS terms require, dated by the month of
    the layers' last_change ("2026-11-14..." -> "november 2026")."""
    day = last_change[:10]
    return "Indeholder data fra Klimadatastyrelsen, Topografisk Landpolygon, %s %s." % (
        DANISH_MONTHS[int(day[5:7]) - 1], day[:4])


def file_sha256(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()
