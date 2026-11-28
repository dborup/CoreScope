# Customizing Your Instance

## Quick Start

1. Open your analyzer in a browser
2. Go to **Tools → Customize**
3. Change colors, branding, home page content
4. Click **💾 Download theme.json**
5. Put the file next to your `config.json` on the server
6. Refresh the page — done

No restart needed. The server picks up changes to `theme.json` on every page load.

## Where Does theme.json Go?

**Next to config.json.** However you deployed, put them side by side.

**Docker:**
```bash
# Add to your docker run command:
-v /path/to/theme.json:/app/theme.json:ro

# Or if you bind-mount the data directory:
# Just put theme.json in that directory
```

**Bare metal / PM2 / systemd:**
```bash
# Same directory as server.js and config.json
cp theme.json /path/to/corescope/
```

Check the server logs on startup — it tells you where it's looking:
```
[theme] Loaded from /app/theme.json
```
or:
```
[theme] No theme.json found. Place it next to config.json or in data/ to customize.
```

## What Can You Customize?

### Branding
```json
{
  "branding": {
    "siteName": "Bay Area Mesh",
    "tagline": "Community LoRa mesh network",
    "logoUrl": "/my-logo.svg",
    "faviconUrl": "/my-favicon.png"
  }
}
```

Logo replaces the 🍄 emoji in the nav bar (renders at 24px height). Favicon replaces the browser tab icon. Use a URL path for files in the `public/` folder, or a full URL for external images.

### Theme Colors (Light Mode)
```json
{
  "theme": {
    "accent": "#ff6b6b",
    "navBg": "#1a1a2e",
    "navText": "#ffffff",
    "background": "#f4f5f7",
    "text": "#1a1a2e",
    "statusGreen": "#22c55e",
    "statusYellow": "#eab308",
    "statusRed": "#ef4444"
  }
}
```

### Theme Colors (Dark Mode)
```json
{
  "themeDark": {
    "accent": "#57f2a5",
    "navBg": "#0a0a1a",
    "background": "#0f0f23",
    "text": "#e2e8f0"
  }
}
```

Only include colors you want to change — everything else stays default.

### All Available Theme Keys

| Key | What It Controls |
|-----|-----------------|
| `accent` | Buttons, links, active tabs, badges, charts |
| `accentHover` | Hover state for accent elements |
| `navBg` | Nav bar background (gradient start) |
| `navBg2` | Nav bar gradient end |
| `navText` | Nav bar text and links |
| `navTextMuted` | Inactive nav links, stats |
| `background` | Main page background |
| `text` | Primary text color |
| `textMuted` | Labels, timestamps, secondary text |
| `statusGreen` | Healthy/online indicators |
| `statusYellow` | Warning/degraded indicators |
| `statusRed` | Error/offline indicators |
| `border` | Dividers, table borders |
| `surface1` | Card backgrounds |
| `surface2` | Nested panels |
| `cardBg` | Detail panels, modals |
| `contentBg` | Content area behind cards |
| `detailBg` | Side panels, packet detail |
| `inputBg` | Text inputs, dropdowns |
| `rowStripe` | Alternating table rows |
| `rowHover` | Table row hover |
| `selectedBg` | Selected/active rows |
| `font` | Body font stack |
| `mono` | Monospace font (hex, hashes, code) |

### Node Role Colors
```json
{
  "nodeColors": {
    "repeater": "#dc2626",
    "companion": "#2563eb",
    "room": "#16a34a",
    "sensor": "#d97706",
    "observer": "#8b5cf6"
  }
}
```

Affects map markers, packet path badges, node lists, and legends.

### Packet Type Colors
```json
{
  "typeColors": {
    "ADVERT": "#22c55e",
    "GRP_TXT": "#3b82f6",
    "TXT_MSG": "#f59e0b",
    "ACK": "#6b7280",
    "REQ": "#a855f7",
    "RESPONSE": "#06b6d4",
    "TRACE": "#ec4899",
    "PATH": "#14b8a6",
    "ANON_REQ": "#f43f5e"
  }
}
```

Affects packet badges, feed dots, map markers, and chart colors.

### Home Page Content
```json
{
  "home": {
    "heroTitle": "Welcome to Bay Area Mesh",
    "heroSubtitle": "Find your nodes to start monitoring them.",
    "steps": [
      { "emoji": "📡", "title": "Connect", "description": "Link your node to the mesh" },
      { "emoji": "🔍", "title": "Monitor", "description": "Watch packets flow in real-time" }
    ],
    "checklist": [
      { "question": "How do I add my node?", "answer": "Search by name or paste your public key." }
    ],
    "footerLinks": [
      { "label": "📦 Packets", "url": "#/packets" },
      { "label": "🗺️ Map", "url": "#/map" }
    ]
  }
}
```

Step descriptions and checklist answers support Markdown (`**bold**`, `*italic*`, `` `code` ``, `[links](url)`).

## User vs Admin Themes

- **Admin theme** (`theme.json`): Default for all users. Edit the file, refresh.
- **User theme** (browser): Each user can override the admin theme via Tools → Customize → "Save as my theme". Stored in localStorage, only affects that browser.

User themes take priority over admin themes. Users can reset their personal theme to go back to the admin default.

`theme.json` holds these sections only: `branding`, `theme`, `themeDark`, `nodeColors`, `typeColors`, `home` and `markerStroke`. A downloaded customizer file can hold more (the Display tab's settings), and the server ignores those sections in `theme.json`. They stay per-browser settings.

### Operator display defaults (`config.json`)

Two Display settings take a default for every browser from `config.json`, served on `/api/config/client`:

- **Timestamps:** the `timestamps` block.

  ```json
  "timestamps": {
    "defaultMode": "absolute",
    "timezone": "utc",
    "formatPreset": "iso-seconds",
    "customFormat": "",
    "allowCustomFormat": false
  }
  ```

  `defaultMode` is `ago` or `absolute`, `timezone` is `local` or `utc`, and `formatPreset` is `iso`, `iso-seconds` or `locale`. An invalid value logs a warning and takes the built-in (`ago`, `local`, `iso`). `allowCustomFormat: true` offers the **Custom Format** field in the Display tab; `customFormat` is then its default, with the tokens `YYYY`, `MM`, `DD`, `HH`, `mm`, `ss`, `SSS` and `Z`.
- **Live Moments:** `moments.display`, while `moments.enabled` is on. See [moments.md](moments.md#display-defaults-for-your-viewers).

A viewer's own value in the customizer wins over the operator's, and the operator's wins over the built-in. For Moments on/off, a `?moments=` link and the viewer's Live checkbox win over both. Keys are case-sensitive: a different spelling (`AllowCustomFormat`) is ignored with a warning in the server log.

### Map layer settings

Some of the customizer's Display settings belong to an optional map layer and only appear when the server offers that layer:

- **Ancient Monuments**: the protected ancient monuments overlay and its Path profile warning. See [ancient-monuments.md](ancient-monuments.md).
- **Live Moments**: the Live page's moment cards, only while the operator has `moments.enabled`. `moments.enabled` (default `true`) is whether the Live page starts with them (a `?moments=` link and your own last choice on the Live page still win until Reset All); `moments.visibleCards` (1 to 5, default 3, desktop only: a phone shows one); `moments.dwellSeconds` (3 to 30, default 8) is how long a card stays; `moments.showNewNode`, `showBackOnline`, `showRecordLink` and `showBusiest` (default `true`) switch a kind off. The operator sets the defaults with `moments.display` in `config.json` (see above). The card fill and the four kind accents are the theme keys `momentCardBg`, `momentNewNode`, `momentBackOnline`, `momentRecordLink` and `momentBusiest` (CSS `--moment-*`), and the accents follow the colourblind preset. The operator's thresholds and rate stay in `config.json`. See [moments.md](moments.md).
- **Live Map** (Follow a packet): how the replay of one followed packet plays in this browser, from the next follow. `follow.speed` is `0.5`, `1` (default) or `2` and divides every hop step; `follow.targetSeconds` (3 to 15 whole seconds, default 6) is the replay length aimed for at 1× (each hop step stays between 0.35 s and 1.5 s, so a very short or very deep path takes less or more); `follow.settleSeconds` (0 to 30 whole seconds, or empty) is how long following a just-heard packet waits for late observations before it loads. Empty is the server's `liveMap.propagationBufferMs` (5 s unless the operator changed it), and a value equal to it is not stored. A deep link, or a packet heard longer ago than the delay, loads at once. The colours are the theme keys `followRing` (rings, origin and card accent), `followRelay`, `followObserver` and `followFlag` (the unreliable-clock flag), CSS `--follow-*`, in light and dark; a colourblind preset sets the relay and observer pair.
- **Live kiosk**: how a TV or wall screen running `#/live?kiosk=1` looks in this browser. `kiosk.scale` is the text and tile size: `auto` (the default: picked by the screen's size, 1 up to 2) or a fixed `1`, `1.25`, `1.5` or `2`. `kiosk.safeInsetVmin` is the margin kept clear at the screen edges for TV overscan, in % of the screen's short side, 0 to 10 (default 3). Set them once in the screen's own browser, outside kiosk, before opening the kiosk link (a kiosk screen hides the customizer). Your browser's value wins over the operator's `kiosk` preset in `config.json` (below), which wins over the defaults; a value equal to the operator's is not stored. A running kiosk takes a change at once. In an exported file this section is per browser: placed on a server as `theme.json` it is ignored there, so put the operator's values in `config.json`.
- **Oblique Photos**: how the oblique aerial photo (Skråfoto) viewer opens, only while the server offers the photos. `skraafoto.direction` is the first photo: `auto` (the default: across the line when opened from a Path profile, otherwise north), `north`, `east`, `south`, `west` or `nadir`; a shared link's direction still wins. `skraafoto.zoom` is `zoomed` (the default: each new photo opens on the crop around the point, where the point is in the photo, otherwise on the whole photo) or `overview` (the whole photo first). `skraafoto.lineOfSight` (default `true`) draws the line of sight from a Path profile; this row is shown only with the Path profile tool. These are viewer settings; the server's `map.skraafoto` config is not part of the customizer.
- **Predicted reach**: the default environment loss of the Map's Predicted reach panel (#968), only while the server offers the predicted reach. `reach.environmentLossDb` (0 to 60 dB, empty by default) is the buildings, trees and other clutter the terrain model does not include. Empty means the operator's `map.coverage.environmentLossDb` in `config.json` (default 25 dB). A link's own value (`renv`) still wins, and the panel's Reset returns to your default. It stays in the browser: it is never sent to the server and changes no request. Like the other sections here it is a per-browser value: a `theme.json` on the server ignores it, so operators set theirs in `config.json`.

### Live kiosk preset (operator, `config.json`)

A TV or wall screen runs Live in kiosk mode from a link such as `#/live?kiosk=1&tour=CPH,AAR&every=60&theme=dark`. The operator can set the defaults for every kiosk screen once, in the `kiosk` block of **`config.json`** (not `theme.json`, and not the customizer: a kiosk screen has no one to click). The server serves it on `/api/config/client`.

```json
"kiosk": {
  "tour": ["CPH", "AAR"],
  "everySeconds": 60,
  "theme": "dark",
  "tiles": ["rate", "active", "records"],
  "remountHours": 6,
  "scale": "auto",
  "safeInsetVmin": 3
}
```

| Key | Default | Bounds | What it does |
|---|---|---|---|
| `tour` | none | area keys from `areas`, at most 12 | The tour for a kiosk link without `tour=`. |
| `everySeconds` | `60` | whole seconds, 15 to 3600 | Seconds per tour step for a link without `every=`. |
| `theme` | `dark` | `dark`, `light`, `auto` | The kiosk theme for a link without `theme=`. `auto` keeps the screen's own theme. |
| `tiles` | all three | `rate`, `active`, `records` | The big-number tiles shown. A tile that is not listed is not built, and without `active` the screen makes no `network-status` and `analytics/areas` requests. An empty list or only unknown names means all three. |
| `remountHours` | `6` | 1 to 24 | How often a long-running kiosk remounts Live in place. |
| `scale` | `auto` | `auto`, `1`, `1.25`, `1.5`, `2` | The kiosk text and tile size. `auto` picks it by the screen's size. |
| `safeInsetVmin` | `3` | 0 to 10 | The margin kept clear at the screen edges (TV overscan), in % of the short side. |

- **Precedence:** the link's own `tour=`, `every=` and `theme=` win, then the preset, then the built-in default. A link with an empty `tour=` is a static view even when the preset has a tour. The tiles and the remount interval have no link parameter.
- **The preset never turns kiosk on.** Only a link with `kiosk=1` does; normal Live is unchanged.
- **Keys are case-sensitive** (as everywhere in `config.json`): `EverySeconds` is ignored with a warning in the log.
- **A value outside its bounds**, or `null`, logs one warning at startup and takes its default. A tour longer than 12 keeps the first 12. `"kiosk": null` is no preset, with a warning.
- **`scale` and `safeInsetVmin` per screen:** a viewer can set both in Customize → Display → **Live kiosk** in the screen's own browser (see Display above). That browser's value wins over this preset; there is no link parameter for them.

## Full Example

```json
{
  "branding": {
    "siteName": "Bay Area MeshCore",
    "tagline": "Community mesh monitoring for the Bay Area",
    "logoUrl": "https://example.com/logo.svg"
  },
  "theme": {
    "accent": "#2563eb",
    "statusGreen": "#16a34a",
    "statusYellow": "#ca8a04",
    "statusRed": "#dc2626"
  },
  "themeDark": {
    "accent": "#60a5fa",
    "navBg": "#0a0a1a",
    "background": "#111827"
  },
  "nodeColors": {
    "repeater": "#ef4444",
    "observer": "#a855f7"
  },
  "home": {
    "heroTitle": "Bay Area MeshCore",
    "heroSubtitle": "Real-time monitoring for our community mesh network.",
    "steps": [
      { "emoji": "💬", "title": "Join our Discord", "description": "Get help and connect with local operators." },
      { "emoji": "📡", "title": "Advertise your node", "description": "Send an ADVERT so the network can see you." },
      { "emoji": "🗺️", "title": "Check the map", "description": "Find repeaters near you." }
    ]
  }
}
```
