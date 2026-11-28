# Area Filter

The area filter is a **GPS-based display filter** that scopes the dashboard to nodes within a defined geographic area. It is distinct from the [region filter](configuration.md#regions), which groups data by the observer's IATA location code.

## How it differs from the region filter

| | Region filter | Area filter |
|--|--|--|
| Based on | Observer's IATA code (from MQTT topic) | Transmitting node's own GPS coordinates |
| Set by | MQTT topic structure | Node's advertised GPS position |
| Use case | Separate traffic by observer location | Separate traffic by where nodes physically are |

Because the region filter is observer-based, a node broadcasting in San Jose can appear under "San Francisco" if a San Francisco observer hears it first. The area filter avoids this cross-region pollution by attributing packets to areas based on where the **sending node** is located.

## Configuration

Add an `areas` block to `config.json`:

```json
"areas": {
  "BAY": {
    "label": "Bay Area",
    "polygon": [
      [37.90, -122.55],
      [37.90, -121.75],
      [37.25, -121.75],
      [37.25, -122.55]
    ]
  },
  "SJC": {
    "label": "San Jose",
    "latMin": 37.20,
    "latMax": 37.45,
    "lonMin": -122.05,
    "lonMax": -121.75
  }
}
```

Each entry defines one area. Two shape formats are supported:

| Format | Fields | Notes |
|--------|--------|-------|
| Polygon | `polygon: [[lat, lon], ...]` | At least 3 points. Supports irregular shapes. |
| Bounding box | `latMin`, `latMax`, `lonMin`, `lonMax` | Simpler rectangles. |

The `label` field controls what appears in the filter pill bar in the UI.

Remove the `areas` block to disable the area filter entirely — the pill bar disappears automatically.

## Using the area filter

When `areas` is configured, a pill bar appears below the main navigation on:

- **Packets** — shows only packets where the transmitting node is within the selected area
- **Nodes** — shows only nodes whose GPS position falls within the area
- **Analytics** — all charts and tables are scoped to nodes in the area
- **Channels** — channel message list is scoped to the area

Click a pill to select that area. Click again (or click the active pill) to deselect. Only one area can be active at a time. The selection is saved in `localStorage` and persists across page reloads.

## Danish areas (DAGI)

A second, separate filter scopes the **Nodes**, **Packets** and **Map** pages to one Danish administrative area: a kommune, a landsdel, a region or a postnummer. The boundaries come from DAGI (Danmarks Administrative Geografiske Inddeling) and ship with the server, so there is nothing to configure. An operator can switch them off with `"dagi": { "enabled": false }` in `config.json`. Without DAGI the control does not appear.

Like the area filter, it works on the **node's own GPS position**, not on where an observer is. The region filter (observer IATA) and the area filter still apply, and all of them are combined: a node must pass every active filter.

### Picking an area

The control sits under the region filter and reads **Denmark: All** until you pick something. It then reads, for example, **Kommune: Aarhus** or **Postnr: 8000 Aarhus C**, and a ✕ beside it clears the filter.

- **On a desktop** it opens a popover under the control.
- **On a phone** (600 px wide or less) it opens a sheet from the bottom of the screen. The search box does not take focus by itself, so the keyboard only opens when you tap the box.
- **Search** matches the start of any word in a name, and the start of a code, in every layer at once:
  - "aar" finds Aarhus, Aars and Årslev;
  - "800" finds 8000 Aarhus C;
  - æ, ø and å match ae, oe and aa, and case does not matter.
  - Each layer shows up to 30 hits; type more to narrow the list.
- **The tabs** (Region, Landsdel, Kommune, Postnr) browse a whole layer while the search box is empty.
  - Kommuner are sorted the Danish way, so Aabenraa, Aalborg and Aarhus come last, as if they started with Å.
  - Postcodes are sorted by number, under a header for each thousand.
- **Keyboard:** the arrow keys move through the list, Enter in the search box picks the first hit, and Escape closes the picker.

One area is selected at a time. A new pick replaces the previous one, even in another layer.

**Copenhagen street postcodes.** Central Copenhagen has hundreds of single-street postcodes. The picker shows them as three rows: "1050–1473 København K", "1550–1799 København V" and "1800–1974 Frederiksberg C". Searching for one of them, such as 1051, finds its row, marked "includes 1051".

**Christiansø** is listed under Kommune as "Christiansø (outside the kommune division)": it belongs to no kommune.

### Links and saved choice

A pick puts the selection in the page's URL, so a link can be shared. A choice restored from `localStorage` (see below) stays out of the URL until you pick again, or until the Nodes page rewrites its URL for a search, a tab or a sort change:

| Layer | Link |
|--|--|
| Kommune | `#/nodes?kommune=0751` (kommunekode) |
| Landsdel | `#/nodes?landsdel=218546` |
| Region | `#/nodes?dkregion=1082` (regionskode; `region` is already the observer filter) |
| Postnummer | `#/nodes?postnr=8000` (a street postcode such as `1051` selects its group) |

- **A link wins over the saved choice.** The last pick is kept in `localStorage` (`meshcore-dagi-filter`) and applies when a link has no DAGI parameter.
- **A link with several DAGI parameters** keeps the most specific one (postnr, then kommune, then landsdel, then region) and drops the others from the URL.
- **An unknown code** is dropped without a message, and the page shows all of Denmark. This can happen with an old link, or with a landsdel id after a yearly boundary update. The code is checked against `/api/config/dagi` before any request uses it, so it is never sent to the server.

### Nodes without a position

A node with no GPS position (none, or 0,0) is in no area, so an active Danish-area filter always hides it. The Nodes page then says how many, for example "18 nodes have no position, so no Danish-area filter can include them". **The count covers all nodes, before the other filters** (role tab, search, region, area), so it can be higher than the number the current view would otherwise show.

A node at sea, off the coast, matches no kommune either.

### Packets

The control sits beside the area filter (under **Filters** on a phone). On packets, the filter works like `area`: it keeps only **ADVERT** packets whose sender is in the area, because only an ADVERT names its sender's public key. Other packets (channel messages, direct messages, traces and so on) are not shown while a Danish-area filter is active, and the page says so under the filter bar, with the count of nodes that have no position.

Live packets follow the same rule. When you pick an area, the page fetches that area's nodes once and checks each live ADVERT's sender against them, so no request is made per packet. A node that gets a position in the area while the page is open is only included after the next pick or visit.

### Map

The control sits in the map controls panel. It filters the node markers, and draws the selected area's outline (in the geo-filter colour) with the DAGI credit in the map's attribution.

- **A pick** in the control zooms the map to the area.
- **A link** (or a return to the Map with a saved choice) draws the outline where the map already is, and does not move it: a link may carry its own view.
- The outline is fetched once per area and cached by the browser for a day; panning and zooming fetch nothing. ✕ removes it.

### Other pages

The API also accepts the same parameters on the analytics endpoints that take `area`; the analytics pages do not have the control yet.

### Credit

The boundaries and names are DAGI data: "Indeholder data fra Klimadatastyrelsen, DAGI, november 2026, CC BY 4.0 — data bearbejdet". The picker's footer shows this credit with links to the [licence](https://creativecommons.org/licenses/by/4.0/) and the [terms](https://dataforsyningen.dk/Vilkaar), and the Map's attribution shows it while an outline is drawn.

## Area Map tool

The Area Map is a visual debug and builder tool served at `/area-map.html` on your CoreScope instance.

### Viewing existing areas

1. Open `/area-map.html` in your browser.
2. Leave the server field empty (uses the current origin) and click **Load**.
3. Each configured area is drawn as a colored polygon on the map.
4. Colored dots show nodes that the server returns when that area is selected — this is what the filter actually returns, so you can verify the boundaries are correct.
5. Use the checkboxes in the sidebar to toggle individual areas on or off.
6. Enable **All nodes (grey)** to overlay every node with GPS — nodes outside all areas appear grey, making it easy to spot incorrectly excluded or included nodes.

### Drawing a new area

1. Fill in **Key** (e.g. `ANT`) and **Label** (e.g. `Antwerp`) in the sidebar.
2. Click **Draw** — the cursor turns to a crosshair.
3. Click on the map to add polygon vertices. The polygon updates after each click.
4. Use **↩ Undo** to remove the last point, **✕ Clear** to start over.
5. When satisfied, the JSON snippet in the output box is ready to copy:

```json
"ANT": {
  "label": "Antwerp",
  "polygon": [[51.28, 4.20], [51.28, 4.55], [51.10, 4.55], [51.10, 4.20]]
}
```

6. Paste this entry into the `areas` object in `config.json` and restart the server.

## API

```
GET /api/config/areas
```

Returns the list of configured area keys and labels (no polygon data). Used by the frontend to build the pill bar.

```
GET /api/config/areas/polygons
```

Returns full area definitions including polygon coordinates. Used by the Area Map tool.

Both endpoints require no authentication.

```
GET /api/config/dagi
```

Returns the Danish areas per layer (`layer`, its filter `param` and the `areas` with `code` and `name`), the credit and the source and licence links. Used by the Danish-area picker. With DAGI off it returns `{"enabled": false, "layers": []}`.

```
GET /api/config/dagi/polygons?layer=<kommune|landsdel|region|postnummer>&code=<code>
```

Returns one area's boundary rings (delta-encoded in `quantum` degrees) and the credit, cacheable for a day with an ETag. Used by the Map to draw the selected area's outline. A street postcode returns its whole group.
