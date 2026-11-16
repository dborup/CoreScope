# Share previews for node links

A shared `#/nodes/<key>` link previews only as the site's own card, because
nothing after `#` reaches the server. When node sharing is on, **Copy URL**
on a node's page and in the Nodes side pane hands out a `/p?node=<key>` link
instead. Pasted into Discord, Facebook, Slack or a similar chat, that link
shows a card of the node. Opened in a browser, it goes straight to the node's
page.

Node sharing is off by default (#811). It is the second card type after
[terrain profiles](share-previews.md), and it uses the same `/p` route.

## Which nodes get a card

- **Repeaters, rooms and sensors.** Copy URL hands out the `/p?node=` link.
- **Companions** are usually people's phones, so they get no card. Their
  Copy URL stays the plain `#/nodes/<key>` link. A `/p?node=` link to a
  companion that someone builds by hand still opens the node page, but its
  preview is the site's own card.
- **Hidden, unknown and inactive nodes** (`nodeBlacklist`,
  `observerBlacklist`, `hiddenNamePrefixes`) have no card. `/p?node=`
  answers with the site's own preview and a 404, and nothing from the link
  is echoed back.

`/p?node=` takes the full 64-hex key only. The 8-character keys of
**Copy short URL** (#772) are not accepted there, so Copy short URL keeps
handing out the plain `#/nodes/<prefix>` link.

Opening `/p?node=<key>` in a browser replaces the address with
`#/nodes/<key>`. No history entry is left for `/p`, so Back leaves the site,
as it would from the plain link. Only the key is carried over, never other
parameters in the link.

## What the card shows

The card is a 1200×630 PNG drawn on the server,
`GET /api/nodes/{pubkey}/card.png`. The `/p` page names it as its
`og:image`. The card shows:

- the node's name and role, the role badge in the role colour from the
  operator's palette. With `map.dhm.profile.card.names` set to `false`, the
  name is left out of the card and the preview's title, which show "Node"
  and the first 8 characters of the key instead;
- the configured area the node is in (from `areas`, for example
  "Midtjylland"), only when its position is inside one. It is never guessed;
- when the node was last heard, both as a relative time and in UTC;
- for repeaters and rooms: relays in the last 24 h and 1 h, and seen airtime
  over 24 h and in the busiest hour. Seen airtime is a lower bound, so it is
  shown with "≥" and flagged at 10 %, as in the busiest-repeaters table;
- the neighbour count;
- with the node page's labels and values (#857):
  - **First seen**, as a relative time and in UTC;
  - **Avg Hops**, the mean path length of the node's packets the server
    still holds in memory. It is left off when those hold no path, as the
    node page leaves it off;
  - **Own scope**, the region the node's own adverts carry: the scope,
    "unknown scope" for an empty one, and "confirmed" when the firmware
    reported it itself. A node with no known scope has no line;
  - for repeaters and rooms, **Transported scopes**: as many as fit, then
    "+N" for the rest;
  - **Estimated flood interval** and **Estimated zero-hop interval**, at
    medium or high confidence only, from the server's mesh-wide advert
    interval pass (the same estimate as the node page). An estimate with
    low confidence, too few adverts or no repeating gap is left off;
- "as of" with the time the card was drawn, and the site name.

A card with seven or eight values draws them in four columns, and one with
more in four columns by three rows. `og:description` lists the values in the order above and, when they do
not fit its 200 characters, leaves off the last ones whole.

A value the server does not have yet is left off, never drawn as 0. The
relay lines of a repeater or room are left off only until the server has
published its first relay snapshot; after that, a node the snapshot does not
hold relayed nothing and shows 0, as on its node page. The card
has **no map, no coordinates and no packet content**. The node's page still
shows its position to anyone who opens it.

The `/p` page carries a title such as *DK_7800_Kjeldbjerg · Repeater*, a
description with the same values as text, `og:url` built from the key, and
`robots noindex`.

## What it costs

- **No upstream service.** A node card needs no DHM token and no other
  upstream call, and it works without `map.dhm`.
- **Server.** Every value on the card comes from snapshots the server already
  keeps for the Nodes page and the advert interval analytics, the node's
  packets in memory, and one read of the node's own database row (its
  scope), so a card never scans the database. Drawing one takes about 10 ms
  of CPU on a fast machine, independent of the size of the network, and a
  card is about 45–85 KB. Cards are cached for 10 minutes
  (64 cards, also bounded by bytes) and are served with
  `Cache-Control: public, max-age=600`. A card that is not cached counts
  against the per-client `map.dhm.profile.rateLimitPerMin` and is drawn in the
  same render slots as the terrain card, so both card types share one budget.
- **`/p?node=` itself** reads only those snapshots.

## Turning it off

- **Operator.** `share.nodes.enabled` in `config.json` is `false` by default.
  Set it to `true` to turn node previews on, then restart. With it off, Copy
  URL hands out the plain `#/nodes/<key>` link,
  `/api/nodes/{pubkey}/card.png` answers 404, and `/p?node=` serves the
  site's own preview. Existing `/p?node=` links keep opening the node page
  either way.
- **Viewer.** In the customizer, open **Display → Share previews** (inside
  **Terrain Profile** when the server has the Path profile tool) and turn off
  **Copy link shares a preview**. That browser's Copy URL then hands out the
  plain node link, and its Path profile Copy link the plain map link: it is
  one switch for both card types. It is stored as `terrain.shareLinks` (only
  `false` is stored), goes into an export, and **Reset** removes it. It
  cannot turn previews on when the server has them off.

`share.nodes.enabled` and the card's look are server-only. The customizer
shows the card theme and the names setting, and stores nothing for them.

## The card's look

The node card is drawn with the terrain card's settings, set by the operator
for everyone:

- `map.dhm.profile.card.theme`: `dark` (the default) or `light`;
- `map.dhm.profile.card.names`: `true` (the default) or `false`;
- the colours come from `theme.json` (`theme`/`themeDark`, and the role
  colours from `nodeColors`). The server reads them at start, so a change
  shows after a restart.

These settings apply to node cards even when `map.dhm` is off.

## Privacy: what the image URL carries

The `og:image` URL holds the node's key, which is already in the link, and
`v=`, a short content hash of what the card shows. `v` changes whenever the
card would change, so a chat that fetches the `/p` page again sees a new
image. It holds no name or position.

## Chat services cache previews

Facebook, Discord and similar services keep a preview after they first fetch
it. See [Chat services cache previews](share-previews.md#chat-services-cache-previews).
A node card shows the node as it was when the chat fetched the link; its
"as of" line says when that was.
