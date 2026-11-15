# Protected ancient monuments (Denmark)

The Map and Live pages can show Denmark's protected ancient monuments
(*fredede fortidsminder*) and their protection zones as an optional map
layer. A click on the layer shows what the register says about a zone. The
Map page's Path profile also warns when one of its ends is in or near a
zone.

The layer is off by default on the server, and each viewer still turns it
on for themselves. It was built in #723:

- M1: the overlay;
- M2: click-for-info;
- M3: the profile warning;
- M4: the customizer settings and this page.

## Data source and credit

The data comes from the **Fund og Fortidsminder** register of the
**Slots- og Kulturstyrelsen** (the Danish Agency for Culture and Palaces).
It is served by the agency's public GeoServer at
`https://www.kulturarv.dk/ffgeoserver/public/`.

- **Overlay.** Browsers fetch the image tiles directly from the agency's WMS.
  The service needs no token and allows cross-origin use. The layer combines
  the protection-zone polygons (`fundogfortidsminder_areal_beskyttelse`)
  and the protected monument points (`fundogfortidsminder_punkt_fredet`).
  Tiles exist from zoom 12 in, so the layer label says "zoom 12+" and the
  layer draws nothing further out.
- **Click-for-info and the profile warning.** These go through this server,
  `GET /api/terrain/heritage?lat=&lon=`. The server asks the agency's WFS
  for one fixed 250 m cell at a time, then caches and rate-limits the
  answers. Viewer addresses are not sent to the agency.
- **Credit.** Every view of the data carries the credit
  **© Slots- og Kulturstyrelsen**: the overlay's attribution, the popup
  and the profile note.
- **Licence.** The data is free to use under the agency's own terms of
  use, not a Creative Commons licence, so no CC mark goes with the credit.
  The terms forbid **harvesting** the register. This is why the server
  never prefetches or walks the country. It asks only for the cells that a
  viewer clicks or measures, and every limit in `map.heritage` exists to
  keep it that way.
- **Accuracy.** Neither the museums nor the agency warrant that the data is
  correct. Some protected monuments have no zone polygon in the register.

## What the 100 m line means, and its caveat

Under the Danish Nature Protection Act (*naturbeskyttelsesloven*, § 18),
a protected ancient monument has a protection line of 100 m around it
(*fortidsmindebeskyttelseslinjen*), within which changes to the land are
restricted. The municipality is the authority, and the line can be reduced
in places (Slots- og Kulturstyrelsen,
[Fortidsmindebeskyttelseslinjen](https://slks.dk/omraader/kulturarv/beskyttede-fortidsminder/information-til-myndigheder/fortidsmindebeskyttelseslinjen/)).

The polygons drawn by the overlay are the register's protection-zone layer,
"Lokaliteter, beskyttelseslinier". Measured zones are about 220 m across
around a mound, which fits a 100 m line. However, **no public source ties
this exact layer to the statutory § 18 line in writing**, and a reduced line
would differ from it. CoreScope therefore makes no legal claim:

- the profile says *"Protected ancient monument / protection zone nearby —
  check before building"*;
- nothing in CoreScope names a paragraph, a dispensation or "100 m";
- nothing ever says that you may build somewhere.

Check with the municipality before you build.

## The profile warning

After a Path profile is measured, the server is asked about each end once.
When an end is inside a zone, or within the server's radius of one
(`map.heritage.radiusM`, 100 m by default), the panel shows:

- a warn-tone chip under the detail line, such as "In monument zone" or
  "Monument zone ~42 m";
- a note with the register's type for each end that was flagged.

The warning is advisory. **The verdict (clear, Fresnel zone, blocked) is
never changed by it.**

**Silence is never an all-clear.** The panel shows nothing in each of
these cases, and no text says that an end is free of monuments:

- nothing near;
- a failed lookup;
- the warning switched off;
- the layer not offered by the server.

Height edits and model switches ask nothing new. Moving one end asks about
that end only.

## Customizer settings

Every viewer can change these under **Customize → Display → Ancient
Monuments**. The section appears only when the server offers the layer. The
two profile settings appear only when the Path profile tool is offered too.
They are stored in this browser with the rest of the customizer (section
`heritage`), and they are exported, imported and cleared by Reset All like
the other customizer settings.

| Setting | Key | Default | Range |
|---|---|---|---|
| Show the overlay on the map by default | `enabled` | off | on / off |
| Overlay opacity | `opacityPct` | 85 % | 10–100 % |
| Warn about monuments in the Path profile | `profileWarning` | on | on / off |
| Warn within this distance of a zone | `warnDistanceM` | empty (every zone the server reports) | 0–250 m |

- **`enabled`.** This sets the starting state for the next map. A link with
  `?heritage=1` or `?heritage=0`, or the viewer's own last toggle on the map,
  still wins. Changing the setting forgets that remembered toggle, so the
  new default applies on the next map. When the default is on, turning the
  layer off writes `heritage=0` into the link, so a shared link keeps the
  layer off.
- **`opacityPct`.** Applies at once to a layer that is already on.
- **`profileWarning`.** When off, the profile asks the server nothing about
  monuments and shows no note. An open profile follows the change at once.
- **`warnDistanceM`.** This can only narrow the warning: the server never
  reports a zone further out than its own `radiusM`, which is at most 250 m.
  An end inside a zone is always listed, and so is an end whose distance the
  server did not give. An open profile redraws its note from the answers it
  already has, without asking the server again.

An imported file is checked against the same rules. A value out of range is
refused with a message, and an unknown key is ignored with a warning.

### What stays server-only

The operator's `map.heritage` keys in `config.json` are not in the
customizer:

- `enabled`;
- `radiusM`;
- `cacheEntries`;
- `cacheTTLHours`;
- `upstreamTimeoutMs`;
- `rateLimitPerMin`;
- `maxInFlight`.

These keys decide how much the server asks the agency for, and the
no-harvesting rule makes that the operator's decision. This follows the
earlier decisions for the terrain profile (#681 M6) and share previews
(#727 M4). See `config.example.json` for what each key does.

## Enabling it (operators)

```json
{
  "map": {
    "heritage": { "enabled": true }
  }
}
```

The service URL, layers, credit and zoom range are fixed in code, so an
operator cannot point browsers or the server at another service. Behind a
reverse proxy, set `webSocket.trustedProxies` as for the other per-client
limits. Otherwise every visitor shares one address and the limit is
skipped with a warning.
