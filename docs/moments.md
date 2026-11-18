# Live Moments

Moments are short cards on the Live map for the few events worth a look: a new
node, a repeater that is back, a new record link, the busiest repeater. They
are off unless the operator turns them on. They never show message content,
and they never show companions (personal devices).

## What each moment means

| Card | Icon | What happened |
|------|------|---------------|
| **New node** | star | A repeater, room or sensor whose key has **never been seen before in the retained history**. A key that comes back after being retired is *Back online*, not *New node*. |
| **Back online** | circular arrow | A repeater heard again after at least `backOnlineHours` (default 24) of silence. Repeaters only. |
| **Record link** | ruler | A new longest direct link between two visible nodes with a position, inside the rolling `recordWindowHours` (default 24), at least `recordMinKm` (default 5) long. Only links the neighbour graph is confident about, and no longer than `neighborGraph.maxEdgeKm`. |
| **Busiest repeater** | rising line | The repeater that relayed the most distinct packets in the last `busyWindowMinutes` (default 10), and at least `busyMinRelays` (default 20). Adverts do not count. Repeaters only. |

### What "first ever" means

*New node* says the key is new to **the retained history**, not new to the
mesh. When the operator's retention drops old nodes, a node that was away for
longer than that can show up as new again. The same holds for the records: a
record link is a record within the window, not an all-time record.

A burst of the same kind is folded into one card ("+N more"). A short rate
limit keeps a flood from becoming a wall of cards, so a moment can be skipped
when many fire at once.

## Using the cards

- Click a card to fly the map to the node (or to fit both ends of a link) and
  mark it for ten seconds. **Details** opens the node page, or the Path
  profile for a record link where that tool is offered.
- A card you hover or focus with the keyboard stays until you leave it.
- The list button opens the latest moments (the last 20), newest first.
  Escape or the backdrop closes it.
- Pause stops new cards; they still land in the list.
- Cards only show while the Live page is in LIVE. With the VCR paused or
  replaying, or while following a packet, moments go to the list only.
- On a phone there is room for one card at a time, and none on a short
  landscape screen. Everything is still in the list.

Turn Moments off for yourself with the **Moments** checkbox in the Live
settings, or with `#/live?moments=0` (`moments=1` turns them on). That choice
is remembered on this device.

## Your settings (Customize → Display → Live Moments)

These are your own display choices, stored in your browser. They appear only
where the operator has turned Moments on.

| Setting | Default | Range | Notes |
|---------|---------|-------|-------|
| Show Moments on the Live page by default | on | | A `?moments=` link, or your own last choice with the Live checkbox, still wins until **Reset All**. |
| Cards shown at once (desktop) | 3 | 1 to 5 | A phone always shows one. The rest wait their turn. |
| Seconds before a card fades | 8 | 3 to 30 | A card you hover or focus stays. |
| New node, Back online, Record link, Busiest repeater | all on | on/off | A switched-off kind gets no card and no row in the list. |

The colours are in **Customize → Theme → Live moments cards**: the card fill
and one accent for each kind (the stripe and the icon). The accents also
follow the colourblind preset you pick. Each one has at least 3:1 contrast on
both the light and the dark card, and the icon and title carry the kind too,
so colour is never the only cue.

What counts as a moment (the thresholds) and how many may fire is the
operator's setting, not yours.

## Operator configuration

Moments are off by default: no detector runs, nothing is broadcast and
`GET /api/moments` returns an empty list. Turn them on in `config.json`:

```json
"moments": {
  "enabled": true,
  "backOnlineHours": 24,
  "recordWindowHours": 24,
  "recordMinKm": 5,
  "busyWindowMinutes": 10,
  "busyMinRelays": 20,
  "maxPerMinute": 6,
  "historySize": 50
}
```

| Field | Default | Range | Description |
|-------|---------|-------|-------------|
| `enabled` | `false` | | Master switch. |
| `backOnlineHours` | `24` | 1 to 720 | Silence before a repeater counts as back. 24 is twice the firmware's 12 h repeater advert default. |
| `recordWindowHours` | `24` | 1 to 168 | The rolling window a record link has to beat. |
| `recordMinKm` | `5` | 0.1 to 1000 | The shortest link that can be a record. |
| `busyWindowMinutes` | `10` | 1 to 1440 | The window for the busiest repeater. |
| `busyMinRelays` | `20` | 1 to 1000000 | The least relays the leader needs. |
| `maxPerMinute` | `6` | 1 to 60 | Global rate, with a burst of 3, on top of fixed per-kind caps. |
| `historySize` | `50` | 1 to 500 | How many moments `/api/moments` keeps. In memory, lost on restart. |

Out-of-range values are clamped, with a line in the log. `0` or a missing
value takes the default. The browser is told only whether Moments are on and
the history size (`moments` in `/api/config/client`).

### Visibility

- `nodeBlacklist`, `observerBlacklist` and `hiddenNamePrefixes` apply. A
  hidden node never appears, and a link needs both ends visible. The check
  runs when the moment fires and again each time `/api/moments` is read, so a
  node hidden later also leaves the history. On read, a node's *current*
  names count too: a rename, or an observer name on the same key, that starts
  with a hidden prefix hides it.
- The node-list geographic filter applies. Positions are advertised GPS only,
  never estimated.
- Detection piggybacks on timers that already run (node delta every 15 s,
  neighbour graph every 60 s, the relay recompute). Nothing runs per packet.

The live WebSocket frame is `{"type":"moment","data":{...}}`; the shape is in
`/api/openapi.json` under `/api/moments`.
