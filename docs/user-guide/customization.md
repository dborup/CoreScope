# Customization

CoreScope includes a built-in theme customizer. Access it from **Tools → Customization** in the navigation menu.

[Screenshot: theme customizer panel with color pickers]

## What you can customize

### Branding

- **Site name** — displayed in the nav bar and browser tab
- **Tagline** — shown on the home page
- **Logo URL** — replace the default logo
- **Favicon URL** — custom browser tab icon

### Theme colors (Light & Dark)

Every color in the UI is customizable:

- **Accent** — primary color for links, buttons, highlights
- **Navigation** — nav bar background, text, and muted text colors
- **Background** — page background and content area
- **Surfaces** — cards, panels, input fields, detail panes
- **Status** — green (healthy), yellow (degraded), red (silent)
- **Text** — primary text, muted text, borders
- **Tables** — row stripe, hover, and selected row colors

Both light and dark themes are independently configurable.

### Node colors

Set the color for each role: repeater, companion, room, sensor, observer. These colors appear on the map, in node badges, and throughout the UI.

### Packet type colors

Customize the color for each packet type: Advert, Channel Msg, Direct Msg, ACK, Request, Response, Trace, Path.

### Home page

Customize the onboarding experience:

- Hero title and subtitle
- Getting-started steps (emoji, title, description for each)
- FAQ items
- Footer links

### Timestamps

- **Display mode** — relative ("5 min ago") or absolute
- **Timezone** — local or UTC
- **Format preset** — ISO or other presets

### Terrain Profile (Display tab)

Shown only where the server offers the Map's Path profile tool:

- **Antenna heights** per node role and for a map point, and the default
  **elevation model**. These are defaults: a height in a link, a height you
  type and one the operator set in server config all take precedence.
- **Share previews**, when the server offers them. **Copy link shares a
  preview** picks whether Copy link hands out a link that shows a preview
  card in chats, or the plain map link. The card's theme and whether it
  shows node names are set by the operator. The card is drawn on the server
  with the operator's saved palette, not the colours you set here. See
  [Share previews](../share-previews.md).

### Share previews for node links

When the server shares node cards (`share.nodes.enabled`), the same **Copy
link shares a preview** switch also decides what **Copy URL** on a node's
page and in the Nodes side pane hands out: a link that shows a card of the
node in chats (repeaters, rooms and sensors), or the plain node link. On a
server without the Path profile tool, the switch has a **Share previews**
section of its own on the Display tab. See
[Share previews for node links](../node-share-previews.md).

## Live preview

Changes apply instantly as you edit. You see the result in real time without saving.

## Exporting a theme

Click **Export JSON** to download your customizations as a JSON file. This produces a config-compatible block you can paste into your `config.json`.

## Importing a theme

Click **Import JSON** and paste a previously exported theme. The customizer loads all values and applies them immediately.

## Resetting

Click **Reset to Defaults** to restore all settings to the built-in defaults.

## GeoFilter (admin only)

The **🗺️ GeoFilter** tab lets operators configure geographic filtering directly from the customizer. It shows the active polygon on a Leaflet map and — on servers with a write-capable `apiKey` — allows editing the polygon and saving back to `config.json` without a restart.

The editing controls are only revealed after the server confirms write access. On public deployments without an `apiKey`, the tab is read-only.

See [Geographic Filtering](geofilter.md) for the full guide, including the API, the prune script, and the standalone GeoFilter Builder.

## How it works

The customizer writes CSS custom properties (variables) to override the defaults. Exported JSON maps directly to the `theme`, `nodeColors`, `branding`, and `home` sections of [config.json](configuration.md).

## Tips

- Start with the accent color — it cascades through buttons, links, and highlights
- Dark mode has its own color set (`themeDark`), independent of light mode
- Node colors affect the [Map](map.md), [Live](live.md) page, and node badges everywhere
- Export your theme before upgrading CoreScope, then re-import it after
