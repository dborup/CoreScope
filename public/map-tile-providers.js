/* map-tile-providers.js — Registry of map tile providers & runtime switcher (#1165/#1420).
 *
 * Scope:
 *   - Multiple providers: OSM (default), Carto, Stamen, OpenTopoMap, USGS,
 *     Esri, Dataforsyningen (#741 Skærmkort and Natur- og Friluftskort,
 *     WMS). Carto (#845), OpenTopoMap, USGS, Esri and Dataforsyningen are
 *     opt-in (map.tiles.providers.*.enabled).
 *   - MC_setDarkTileProvider(id) / MC_setLightTileProvider(id) persist per-browser
 *     to localStorage and dispatch `mc-tile-provider-changed`.
 *   - Resolves localStorage → server default → 'osm-dark' / 'osm-standard' (#332).
 *   - MC_applyTileFilter() applies/clears the CSS filter on
 *     `.leaflet-tile-pane` based on current theme + selected provider.
 *   - MC_createLayerControl() builds a Leaflet control mapping providers,
 *     plus the optional DHM terrain hillshade (#680), protected ancient
 *     monuments (#723) and Mastedatabasen antenna site (#724) overlays.
 *
 * No new deps — tile-URL and WMS providers, both built by createBaseLayer.
 */
(function () {
  'use strict';

  var STORAGE_KEY  = 'mc-dark-tile-provider';
  var STORAGE_KEY_LIGHT = 'mc-light-tile-provider';

  // #332 — the built-in fallback must NOT be CARTO. Since August 2026 CARTO
  // stamps "API KEY REQUIRED" into every keyless raster tile, and this fork
  // has no basemap-key plumbing, so an unconfigured instance that defaulted
  // to 'carto-dark' / 'carto-light' rendered watermarked maps. OSM Standard
  // is the keyless baseline: no token, tiles to zoom 18, and it is already
  // the unconditional provider for the home mini-map, node-reach, packet-path
  // and area maps — so this default introduces no new third party. Operators
  // who want CARTO (or anything else) still set map.tiles.darkDefault /
  // lightDefault, which keep priority over these.
  var DEFAULT_ID   = 'osm-dark';
  var DEFAULT_ID_LIGHT = 'osm-standard';
  var EVENT_NAME   = 'mc-tile-provider-changed';
  
  var _serverDefault = null;
  var _serverDefaultLight = null;
  
  var INVERT_CSS   = 'invert(1) hue-rotate(180deg) brightness(0.9) contrast(1.05)';
  /* #364/#390 — a legacy raw tile URL carries no vendor metadata, so it
   * keeps the historical generic credit. Documented for operators in
   * config.example.json `map.tiles._comment_legacy`. */
  var LEGACY_ATTRIBUTION = '© OpenStreetMap contributors';

  var _cfg = null;

  var _getCartoBase = function() { return (_cfg && _cfg.providers && _cfg.providers.carto && _cfg.providers.carto.domain) ? 'https://{s}.' + _cfg.providers.carto.domain + '.cartocdn.com' : 'https://{s}.basemaps.cartocdn.com'; };
  var _getStamenUrl = function() { return 'https://tiles.stadiamaps.com/tiles/stamen_toner_lite/{z}/{x}/{y}{r}.png' + ((_cfg && _cfg.providers && _cfg.providers.stamen && _cfg.providers.stamen.token) ? '?api_key=' + encodeURIComponent(_cfg.providers.stamen.token) : ''); };
  var _getOsmUrl = function() {
    if (_cfg && _cfg.providers && _cfg.providers.osm && _cfg.providers.osm.provider && _cfg.providers.osm.token) {
      var prov = _cfg.providers.osm.provider.toLowerCase();
      var key = encodeURIComponent(_cfg.providers.osm.token);
      if (prov === 'thunderforest') return 'https://{s}.tile.thunderforest.com/osm-carto/{z}/{x}/{y}.png?apikey=' + key;
      if (prov === 'maptiler') return 'https://api.maptiler.com/maps/streets/{z}/{x}/{y}.png?key=' + key;
      if (prov === 'mapbox') return 'https://api.mapbox.com/styles/v1/mapbox/streets-v11/tiles/256/{z}/{x}/{y}@2x?access_token=' + key;
    }
    return 'https://{s}.tile.openstreetmap.org/{z}/{x}/{y}.png';
  };
  /* #332 — credit the vendor that actually serves the tiles. osm-standard /
   * osm-dark used to credit "Maps © Mapbox/Thunderforest/MapTiler"
   * unconditionally; #332 makes them the default every unconfigured
   * instance shows, and such an instance uses none of those three — its
   * tiles come straight from the OSMF servers. Resolved the same way
   * _getOsmUrl() picks the URL, and re-resolved on every registry init so
   * config landing after parse time still applies. */
  var _getOsmAttribution = function() {
    var OSM = '© OpenStreetMap contributors';
    if (_cfg && _cfg.providers && _cfg.providers.osm && _cfg.providers.osm.provider && _cfg.providers.osm.token) {
      var prov = _cfg.providers.osm.provider.toLowerCase();
      if (prov === 'thunderforest') return OSM + ', Tiles © Thunderforest';
      if (prov === 'maptiler')      return OSM + ', Tiles © MapTiler';
      if (prov === 'mapbox')        return OSM + ', Tiles © Mapbox';
    }
    return OSM;
  };

  /* #728 — Esri requires "Powered by Esri", with "Esri" linking to
   * esri.com, in the credit of every Esri basemap. */
  var ESRI_POWERED_BY = 'Powered by <a href="https://www.esri.com">Esri</a> | ';

  /* #728 — Esri World Imagery. The service is {z}/{y}/{x} (row before
   * column) like the Dark Gray canvas. Rural Denmark is already upsampled
   * at z19, so it is capped there like the other Esri style. The credit is
   * the service's own copyrightText (Maxar is now Vantor). */
  var ESRI_IMAGERY_URL = 'https://server.arcgisonline.com/ArcGIS/rest/services/World_Imagery/MapServer/tile/{z}/{y}/{x}';
  var ESRI_IMAGERY_ATTRIBUTION = ESRI_POWERED_BY + 'Tiles © Esri — Source: Esri, Vantor, Earthstar Geographics, and the GIS User Community';

  /* #741 M1b — Skærmkortet from Klimadatastyrelsen / Dataforsyningen. It
   * has no Web Mercator WMTS, so it comes from the one WMS that serves all
   * three variants in EPSG:3857 (topo_skaermkort_DAF), built by the shared
   * factory's WMS branch.
   *   - The browser fetches the tiles itself, like Carto and Esri, with the
   *     operator's base-map token: map.tiles.providers.dataforsyningen.token,
   *     client-visible by design and never map.dhm.token (the server drops
   *     a value that contains the DHM token). The gateway accepts the
   *     parameter only as `token`/`TOKEN`, so it sits on the base URL, where
   *     no Leaflet option can rename it.
   *   - 512 px tiles: a 256 px GetMap renders each tile alone and cuts the
   *     labels at its edges; 512 px halves the cuts and is a quarter of the
   *     requests (there are no cache headers, so every pan is a fresh
   *     render). Leaflet derives a WMS bbox from the tile's coordinates and
   *     size, so a 512 px tile at zoom z covers its 512 px of that zoom;
   *     zoomOffset -1 is the usual pairing and only affects a {z} template.
   *   - Zoom 6-19 is the range the source pyramid has detail for, and the
   *     bounds are the layers' own WGS84 box, so nothing is requested
   *     outside Denmark.
   *   - Dark mode is the grey variant under the existing invert filter: it
   *     is achromatic, so it inverts to a neutral dark map.
   *
   * #741 M2 — Natur- og Friluftskort, the same way: its WMTS is EPSG:25832
   * only, and wms/natur_friluftskort serves EPSG:3857 (owner token checks).
   * Same gate, same token, same tile options as Skærmkort, so the same
   * number of requests per view and no tile churn of its own; only the
   * service, the layer and the credit differ.
   *   - Natur_Friluft_basis only: the service's Vejdirektoratet layer comes
   *     under a second terms document nobody has read yet, and the place
   *     and road names are separate layers (Stednavne, Vejnavne) that would
   *     add render time to a service that is already ~1 s a tile cold.
   *   - Light only: a colour-coded outdoor map (forest, water, paths) has no
   *     achromatic variant, and the invert filter would turn its fills dark
   *     and muddy, as for the standard and dæmpet Skærmkort, which are
   *     light only too. Picked in the dark theme it shows as published.
   *   - The bounds are Skærmkort's: the Natur capabilities need a token, so
   *     its own box was not read; both cover Denmark. */
  var DATAFORSYNINGEN_API    = 'https://api.dataforsyningen.dk/';
  var DATAFORSYNINGEN_BOUNDS = [[53.015, 2.47842], [58.6403, 17.5578]];
  // The CC BY 4.0 credits, with their dataset and licence links (#733).
  var DATAFORSYNINGEN_CREDITS = {
    skaermkort: {
      attribution: 'Indeholder data fra Klimadatastyrelsen og GeoDanmark, Skærmkort, CC BY 4.0',
      datasetName: 'Skærmkort', datasetUrl: 'https://dataforsyningen.dk/data/962',
      licenseName: 'CC BY 4.0', licenseUrl: 'https://creativecommons.org/licenses/by/4.0/'
    },
    naturFriluft: {
      attribution: 'Indeholder data fra Klimadatastyrelsen, Natur- og Friluftskort, CC BY 4.0',
      datasetName: 'Natur- og Friluftskort', datasetUrl: 'https://dataforsyningen.dk/data/2683',
      licenseName: 'CC BY 4.0', licenseUrl: 'https://creativecommons.org/licenses/by/4.0/'
    }
  };
  // The base-map token as the gate reads it: a non-blank string, trimmed;
  // '' otherwise. Exact-case keys only, like every other provider block.
  var _getDataforsyningenToken = function() {
    var p = _cfg && _cfg.providers && _cfg.providers.dataforsyningen;
    return (p && typeof p.token === 'string') ? p.token.trim() : '';
  };
  // A WMS descriptor for one layer of a Dataforsyningen service, with the
  // base-map token on its base URL, re-read whenever the URL is resolved.
  function _dataforsyningenWms(service, layer) {
    return {
      url: function() { return DATAFORSYNINGEN_API + service + '?token=' + encodeURIComponent(_getDataforsyningenToken()); },
      layers: layer, tileSize: 512, zoomOffset: -1, minZoom: 6, maxZoom: 19, bounds: DATAFORSYNINGEN_BOUNDS
    };
  }

  var BASE_STYLES = {
    'carto-dark': { provider: 'carto', label: 'Carto Dark', url: function() { return _getCartoBase() + '/dark_all/{z}/{x}/{y}{r}.png'; }, invertFilter: null, type: 'dark', attribution: '© OpenStreetMap © CartoDB', maxZoom: 19 },
    'carto-light': { provider: 'carto', label: 'Carto Positron', url: function() { return _getCartoBase() + '/light_all/{z}/{x}/{y}{r}.png'; }, invertFilter: null, type: 'light', attribution: '© OpenStreetMap © CartoDB', maxZoom: 19 },
    'carto-voyager': { provider: 'carto', label: 'Carto Voyager', url: function() { return _getCartoBase() + '/rastertiles/voyager/{z}/{x}/{y}{r}.png'; }, invertFilter: null, type: 'light', attribution: '© OpenStreetMap © CartoDB', maxZoom: 19 },
    'carto-voyager-dark': { provider: 'carto', label: 'Carto Voyager', url: function() { return _getCartoBase() + '/rastertiles/voyager/{z}/{x}/{y}{r}.png'; }, invertFilter: INVERT_CSS, type: 'dark', attribution: '© OpenStreetMap © CartoDB', maxZoom: 19 },
    'positron-dark': { provider: 'carto', label: 'Carto Positron', url: function() { return _getCartoBase() + '/light_all/{z}/{x}/{y}{r}.png'; }, invertFilter: INVERT_CSS, type: 'dark', attribution: '© OpenStreetMap © CartoDB', maxZoom: 19 },
    'osm-standard': { provider: 'osm', label: 'OSM Standard', url: _getOsmUrl, invertFilter: null, type: 'light', attribution: '© OpenStreetMap contributors, Maps © Mapbox/Thunderforest/MapTiler', maxZoom: 18 },
    'osm-dark': { provider: 'osm', label: 'OSM Standard', url: _getOsmUrl, invertFilter: INVERT_CSS, type: 'dark', attribution: '© OpenStreetMap contributors, Maps © Mapbox/Thunderforest/MapTiler', maxZoom: 18 },
    'opentopomap': { provider: 'opentopomap', label: 'OpenTopoMap', url: function() { return 'https://{s}.tile.opentopomap.org/{z}/{x}/{y}.png'; }, invertFilter: null, type: 'light', attribution: 'Map data: © <a href="https://www.openstreetmap.org/copyright">OpenStreetMap</a> contributors, <a href="https://viewfinderpanoramas.org">SRTM</a> | Map style: © <a href="https://opentopomap.org">OpenTopoMap</a> (<a href="https://creativecommons.org/licenses/by-sa/3.0/">CC-BY-SA</a>)', maxZoom: 17 },
    'stamen-toner-lite': { provider: 'stamen', label: 'Stamen Toner Lite', url: _getStamenUrl, invertFilter: null, type: 'light', attribution: '© Stadia Maps © Stamen Design © OpenStreetMap', maxZoom: 20 },
    'stamen-toner-dark': { provider: 'stamen', label: 'Stamen Toner Lite', url: _getStamenUrl, invertFilter: INVERT_CSS, type: 'dark', attribution: '© Stadia Maps © Stamen Design © OpenStreetMap', maxZoom: 20 },
    'usgs-topo': { provider: 'usgs', label: 'USGS Topographic', url: function() { return 'https://basemap.nationalmap.gov/arcgis/rest/services/USGSTopo/MapServer/tile/{z}/{y}/{x}'; }, invertFilter: null, type: 'light', attribution: 'U.S. Geological Survey, National Geospatial Program', maxZoom: 16 },
    'usgs-imagery': { provider: 'usgs', label: 'USGS Imagery', url: function() { return 'https://basemap.nationalmap.gov/arcgis/rest/services/USGSImageryTopo/MapServer/tile/{z}/{y}/{x}'; }, invertFilter: null, type: 'light', attribution: 'U.S. Geological Survey, National Geospatial Program', maxZoom: 16 },
    'esri-darkgray-labels': { provider: 'esri', label: 'Esri Dark Gray Canvas', url: function() { return 'https://server.arcgisonline.com/ArcGIS/rest/services/Canvas/World_Dark_Gray_Base/MapServer/tile/{z}/{y}/{x}'; }, invertFilter: null, type: 'dark', attribution: ESRI_POWERED_BY + 'Tiles © Esri', maxZoom: 19 },
    // #728 — aerial photo: never inverted, so the same tiles serve both themes.
    'esri-imagery': { provider: 'esri', label: 'Esri World Imagery', url: function() { return ESRI_IMAGERY_URL; }, invertFilter: null, type: 'light', attribution: ESRI_IMAGERY_ATTRIBUTION, maxZoom: 19 },
    'esri-imagery-dark': { provider: 'esri', label: 'Esri World Imagery', url: function() { return ESRI_IMAGERY_URL; }, invertFilter: null, type: 'dark', attribution: ESRI_IMAGERY_ATTRIBUTION, maxZoom: 19 },
    // #741 M1b — WMS styles: a `wms` descriptor instead of a tile `url`,
    // and a `credit` the registry builds the linked attribution from.
    'dataforsyningen-skaermkort': { provider: 'dataforsyningen', label: 'Skærmkort (Denmark)', wms: _dataforsyningenWms('topo_skaermkort_DAF', 'dtk_skaermkort'), invertFilter: null, type: 'light', credit: DATAFORSYNINGEN_CREDITS.skaermkort, maxZoom: 19 },
    'dataforsyningen-skaermkort-graa': { provider: 'dataforsyningen', label: 'Skærmkort grå (Denmark)', wms: _dataforsyningenWms('topo_skaermkort_DAF', 'dtk_skaermkort_graa'), invertFilter: null, type: 'light', credit: DATAFORSYNINGEN_CREDITS.skaermkort, maxZoom: 19 },
    'dataforsyningen-skaermkort-daempet': { provider: 'dataforsyningen', label: 'Skærmkort dæmpet (Denmark)', wms: _dataforsyningenWms('topo_skaermkort_DAF', 'dtk_skaermkort_daempet'), invertFilter: null, type: 'light', credit: DATAFORSYNINGEN_CREDITS.skaermkort, maxZoom: 19 },
    // #741 M2 — light only (see above).
    'dataforsyningen-natur-friluft': { provider: 'dataforsyningen', label: 'Natur- og Friluftskort (Denmark)', wms: _dataforsyningenWms('wms/natur_friluftskort', 'Natur_Friluft_basis'), invertFilter: null, type: 'light', credit: DATAFORSYNINGEN_CREDITS.naturFriluft, maxZoom: 19 },
    'dataforsyningen-skaermkort-graa-dark': { provider: 'dataforsyningen', label: 'Skærmkort grå (Denmark)', wms: _dataforsyningenWms('topo_skaermkort_DAF', 'dtk_skaermkort_graa'), invertFilter: INVERT_CSS, type: 'dark', credit: DATAFORSYNINGEN_CREDITS.skaermkort, maxZoom: 19 }
  };

  var REGISTRY = {};

  function _hasId(id) {
    return typeof id === 'string' && Object.prototype.hasOwnProperty.call(REGISTRY, id);
  }

  /* #845 — a CARTO style id saved while CARTO was on (a viewer's pick, a
   * customizer default, an operator's darkDefault/lightDefault) resolves to
   * its nearest keyless equivalent while CARTO is off: Positron and Voyager
   * to OSM Standard (light, or its dark variant), Carto Dark to Esri Dark
   * Gray Canvas when Esri is enabled, else OSM Standard (dark). The saved
   * value itself is left alone, so it comes back if the operator turns
   * CARTO on again. Any other id is returned as it is. */
  var CARTO_MIGRATION = {
    'carto-light': ['osm-standard'],
    'carto-voyager': ['osm-standard'],
    'positron-dark': ['osm-dark'],
    'carto-voyager-dark': ['osm-dark'],
    'carto-dark': ['esri-darkgray-labels', 'osm-dark']
  };
  function migrateTileId(id) {
    if (typeof id !== 'string' || _hasId(id) || !Object.prototype.hasOwnProperty.call(CARTO_MIGRATION, id)) return id;
    var to = CARTO_MIGRATION[id];
    for (var i = 0; i < to.length; i++) if (_hasId(to[i])) return to[i];
    return id;
  }

  /* #728 — a configured default whose provider is not enabled is skipped
   * (the resolvers fall through to the legacy URL or the built-in
   * fallback), so the map still works. Say so once per page, naming the
   * switch, so an operator who upgraded past the Esri opt-in can find it. */
  var _warnedDefaults = {};
  // What switches a gated provider on, when it is more than `enabled`.
  var GATE_HINT = {
    dataforsyningen: 'set map.tiles.providers.dataforsyningen.enabled: true and a non-empty map.tiles.providers.dataforsyningen.token'
  };
  /* #859 — the warning names the id the operator configured, not the one
   * it was migrated to (#845), and says why: a CARTO default resolves to
   * its keyless equivalent while CARTO is off, which is the default. */
  function unavailableDefaultMessage(key, id) {
    if (typeof id !== 'string' || !id || _hasId(id)) return null;
    var style = Object.prototype.hasOwnProperty.call(BASE_STYLES, id) ? BASE_STYLES[id] : null;
    var head = '[map tiles] map.tiles.' + key + ' "' + id + '" is ';
    if (style && style.provider === 'carto') {
      var to = migrateTileId(id);
      return head + 'not used: CARTO is off by default since #845 (keyless CARTO tiles are stamped ' +
        '"API KEY REQUIRED"; set map.tiles.providers.carto.enabled: true to use it anyway). ' +
        (to !== id ? 'Using "' + to + '" instead.' : 'Using the fallback base map instead.');
    }
    var why = style
      ? 'its provider is not enabled (' + (GATE_HINT[style.provider] || 'set map.tiles.providers.' + style.provider + '.enabled: true') + ')'
      : 'it is not a known tile style';
    return head + 'ignored: ' + why + '. Using the fallback base map instead.';
  }
  function _warnUnavailableDefault(key, id) {
    var msg = unavailableDefaultMessage(key, id);
    if (!msg || _warnedDefaults[key + '=' + id]) return;
    _warnedDefaults[key + '=' + id] = true;
    try {
      if (typeof console !== 'undefined' && console.warn) console.warn(msg);
    } catch (_) {}
  }

  // #873 — true once /api/config/client has fed the registry (or failed):
  // until then a base= link is only kept, not judged (it may name a style
  // the config is about to enable).
  var _configSettled = false;

  window.MC_initTileRegistry = function(fromAsync) {
    _cfg = (typeof window !== 'undefined' && window.MC_MAP_CFG && window.MC_MAP_CFG.tiles) ? window.MC_MAP_CFG.tiles : null;

    // #845 — opt-in, like Esri: since August 2026 every keyless CARTO tile
    // is stamped "API KEY REQUIRED" (#332) and this fork has no key
    // plumbing, so CARTO is off unless map.tiles.providers.carto.enabled is
    // the JSON boolean true. Saved picks of a CARTO style are migrated by
    // migrateTileId() below.
    var HAS_CARTO = !!(_cfg && _cfg.providers && _cfg.providers.carto && _cfg.providers.carto.enabled === true);
    var HAS_OSM = _cfg && _cfg.providers && _cfg.providers.osm && _cfg.providers.osm.enabled;
    var HAS_OPENTOPOMAP = _cfg && _cfg.providers && _cfg.providers.opentopomap && _cfg.providers.opentopomap.enabled;
    var HAS_USGS = _cfg && _cfg.providers && _cfg.providers.usgs && _cfg.providers.usgs.enabled;
    var HAS_STAMEN = _cfg && _cfg.providers && _cfg.providers.stamen && _cfg.providers.stamen.enabled && !!_cfg.providers.stamen.token;
    // #728 — opt-in like OpenTopoMap and USGS. The public Esri endpoint is
    // licensed under the Esri Master License Agreement, so an operator has
    // to switch it on deliberately; only the JSON boolean true counts.
    var HAS_ESRI = !!(_cfg && _cfg.providers && _cfg.providers.esri && _cfg.providers.esri.enabled === true);
    // #741 — the same JSON-boolean opt-in, and a token, like Stamen: without
    // one every Skærmkort tile is a 403.
    var HAS_DATAFORSYNINGEN = !!(_cfg && _cfg.providers && _cfg.providers.dataforsyningen &&
      _cfg.providers.dataforsyningen.enabled === true && _getDataforsyningenToken());

    // #332 — resolve the OSM credit against the configured vendor before
    // the registry is handed out. Keeping `attribution` a plain string here
    // means every reader (getTileSpec, the layer control, the inset-map
    // helper, map.js/live.js legacy branches) stays unchanged.
    BASE_STYLES['osm-standard'].attribution = _getOsmAttribution();
    BASE_STYLES['osm-dark'].attribution     = _getOsmAttribution();

    REGISTRY = {};
    for (var key in BASE_STYLES) {
      var style = BASE_STYLES[key];
      if (style.provider === 'carto' && HAS_CARTO) REGISTRY[key] = style;
      if (style.provider === 'osm' && HAS_OSM) REGISTRY[key] = style;
      if (style.provider === 'opentopomap' && HAS_OPENTOPOMAP) REGISTRY[key] = style;
      if (style.provider === 'stamen' && HAS_STAMEN) REGISTRY[key] = style;
      if (style.provider === 'usgs' && HAS_USGS) REGISTRY[key] = style;
      if (style.provider === 'esri' && HAS_ESRI) REGISTRY[key] = style;
      if (style.provider === 'dataforsyningen' && HAS_DATAFORSYNINGEN) REGISTRY[key] = style;
      // #741 — a style's CC BY 4.0 credit, its dataset and licence linked
      // by the shared walker (#733); built here, where the DOM is ready.
      if (style.credit) style.attribution = buildCredit(style.credit.attribution, creditLinks(style.credit));
    }

    // #332 — the fallback defaults must stay resolvable no matter how the
    // optional providers are gated. Without this a config that disables
    // everything leaves REGISTRY without the active id, every caller falls
    // through to a URL of its own, and that is exactly how the hard-coded
    // CARTO templates spread in the first place. It also keeps the default
    // selectable in the layer picker, so a user who switches away can
    // switch back. `enabled` still governs every other style of a provider.
    if (!REGISTRY[DEFAULT_ID])       REGISTRY[DEFAULT_ID]       = BASE_STYLES[DEFAULT_ID];
    if (!REGISTRY[DEFAULT_ID_LIGHT]) REGISTRY[DEFAULT_ID_LIGHT] = BASE_STYLES[DEFAULT_ID_LIGHT];

    // Keep the public reference in sync with the newly rebuilt REGISTRY
    window.MC_TILE_PROVIDERS = REGISTRY;

    if (_hasId(_cfgDefault('darkDefault'))) _serverDefault = _cfgDefault('darkDefault');
    if (_hasId(_cfgDefault('lightDefault'))) _serverDefaultLight = _cfgDefault('lightDefault');
    // #859 — warned about as configured, before any CARTO migration.
    _warnUnavailableDefault('darkDefault', _cfg && _cfg.darkDefault);
    _warnUnavailableDefault('lightDefault', _cfg && _cfg.lightDefault);

    // When called after async config loads, notify maps to re-sync tiles
    if (fromAsync) {
      _configSettled = true; // #873 — a base= link can now be judged
      try {
        var ev = (typeof CustomEvent === 'function')
          ? new CustomEvent('mc-tile-provider-changed', { detail: { fromConfig: true } })
          : { type: 'mc-tile-provider-changed', detail: { fromConfig: true } };
        window.dispatchEvent(ev);
      } catch (_) {}
    }
  };

  // Run once immediately with whatever config is available at parse time
  window.MC_initTileRegistry();

  function _isDark() {
    try {
      var attr = document.documentElement.getAttribute('data-theme');
      if (attr === 'dark') return true;
      if (attr === 'light') return false;
      return !!(window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches);
    } catch (_) { return false; }
  }

  
  // The operator's map.tiles.darkDefault / lightDefault, migrated (#845).
  function _cfgDefault(key) {
    return _cfg && _cfg[key] ? migrateTileId(_cfg[key]) : null;
  }

  // #390 — mirror of getSelectedLightId(): a valid explicit choice can
  // itself be the built-in fallback id, so an implicit fallback has to stay
  // distinguishable from one the operator or user actually picked.
  function getSelectedDarkId() {
    try {
      var stored = migrateTileId(window.localStorage && window.localStorage.getItem(STORAGE_KEY));
      if (_hasId(stored) && REGISTRY[stored].type === 'dark') return stored;
    } catch (_) {}
    if (_hasId(_cfgDefault('darkDefault'))) return _cfgDefault('darkDefault');
    if (_hasId(_serverDefault)) return _serverDefault;
    return null;
  }

  function getActiveId() {
    return getSelectedDarkId() || DEFAULT_ID;
  }

  // A valid explicit choice can itself be the built-in fallback id. Keep
  // that separate from an implicit fallback when resolving legacy URLs.
  function getSelectedLightId() {
    try {
      var stored = migrateTileId(window.localStorage && window.localStorage.getItem(STORAGE_KEY_LIGHT));
      if (_hasId(stored) && REGISTRY[stored].type === 'light') return stored;
    } catch (_) {}
    if (_hasId(_cfgDefault('lightDefault'))) return _cfgDefault('lightDefault');
    if (_hasId(_serverDefaultLight)) return _serverDefaultLight;
    return null;
  }

  function getActiveLightId() {
    return getSelectedLightId() || DEFAULT_ID_LIGHT;
  }
  
  function setActive(id, type) {
    if (!_hasId(id)) return false;
    if (REGISTRY[id].type !== type) return false;
    var skey = type === 'light' ? STORAGE_KEY_LIGHT : STORAGE_KEY;
    try {
      if (window.localStorage) window.localStorage.setItem(skey, id);
    } catch (_) { }
    var detail = { id: id, provider: REGISTRY[id], type: type };
    try {
      var ev = (typeof CustomEvent === 'function')
        ? new CustomEvent(EVENT_NAME, { detail: detail })
        : { type: EVENT_NAME, detail: detail };
      window.dispatchEvent(ev);
    } catch (_) { }
    applyTileFilter();
    return true;
  }


  function setServerDefault(id) { if (_hasId(id)) _serverDefault = id; }
  function setServerDefaultLight(id) { if (_hasId(id)) _serverDefaultLight = id; }

  
  function _isDarkEffective() {
    return _isDark();
  }

  function applyTileFilter() {
    var pane;
    try { pane = document.querySelector('.leaflet-tile-pane'); } catch (_) { pane = null; }
    if (!pane || !pane.style) return;
    // Contract: if an explicit layer was selected via the map control, it locks
    // the CSS filter state. We bypass our auto-theme logic here so we don't accidentally
    // invert an explicitly chosen Light map while the app theme is Dark.
    if (pane.getAttribute('data-explicit-layer') === 'true') {
      return;
    }

    // #390 — resolve through getTileSpec so the legacy-URL branch is
    // honoured here too. A legacy dark URL is an operator's own dark
    // tileset; inverting it on top of that renders the map light.
    var isDark = _isDarkEffective();
    var spec = getTileSpec(isDark ? 'dark' : 'light');
    pane.style.filter = (isDark && spec && spec.invertFilter) ? spec.invertFilter : '';
  }


  /* #332 — getTileSpec(type): the ONE place a map resolves the active
   * provider. `type` is 'dark' or 'light'; anything else is read as 'dark'.
   * Returns a plain object so no caller needs to know that a style's `url`
   * may be a lazy function, nor to carry its own fallback URL/attribution:
   *
   *   { id, url, attribution, refUrl, maxZoom, invertFilter[, wms] }
   *
   * `url` is always a string template (or null if a style is malformed, or
   * a WMS style, #741) and
   * `attribution` always comes from the resolved provider — never from a
   * hard-coded CARTO credit. Re-resolved on every call so config landing
   * after map init still applies.
   */
  function getTileSpec(type) {
    var isLight  = (type === 'light');
    var fallback = isLight ? DEFAULT_ID_LIGHT : DEFAULT_ID;
    // #364/#390 precedence, applied per theme: browser selection >
    // configured provider default > legacy URL > built-in fallback.
    // roles.js records the actual loaded URL, not TILE_DARK/TILE_LIGHT's
    // always-present built-in value. Unknown custom vendors keep the
    // historical generic credit and no inversion; id:null means this is
    // not a registry provider with vendor metadata. #390 gives dark the
    // branch light got in #366 — `tiles.dark` / `map.tiles.darkUrl` were
    // masked by the registry default the same way.
    var legacyUrl = isLight ? window.MC_LIGHT_TILE_URL_OVERRIDE : window.MC_DARK_TILE_URL_OVERRIDE;
    var selected  = isLight ? getSelectedLightId() : getSelectedDarkId();
    if (!selected && typeof legacyUrl === 'string' && legacyUrl) {
      return { id: null, url: legacyUrl, attribution: LEGACY_ATTRIBUTION, refUrl: null, maxZoom: BASE_STYLES[fallback].maxZoom, invertFilter: null };
    }
    var id       = isLight ? getActiveLightId() : getActiveId();
    var p        = REGISTRY[id] || BASE_STYLES[id] || BASE_STYLES[fallback];
    var u        = p ? (p.url || p.baseUrl) : null;
    var spec = {
      id: id,
      url: (typeof u === 'function' ? u() : u) || null,
      attribution: (p && p.attribution) || BASE_STYLES[fallback].attribution,
      refUrl: (p && p.refUrl) || null,
      maxZoom: (p && p.maxZoom) || 19,
      invertFilter: (p && p.invertFilter) || null
    };
    // #741 M1b — a WMS style hands over its descriptor, as a copy with the
    // lazy url resolved, so the pages can build it through the factory. A
    // tile style gets no `wms` key at all: the field is purely additive.
    if (p && p.wms) spec.wms = _resolveWms(p.wms);
    return spec;
  }

  function _resolveWms(w) {
    var o = {};
    for (var k in w) {
      if (!Object.prototype.hasOwnProperty.call(w, k)) continue;
      o[k] = (k === 'bounds' && Array.isArray(w[k])) ? w[k].map(function (c) { return c.slice(); }) : w[k];
    }
    if (typeof o.url === 'function') o.url = o.url();
    return o;
  }

  /* #741 M1a — createBaseLayer(spec, opts): the ONE place a provider spec
   * becomes a Leaflet base layer, so a style can be a tile template or a
   * WMS layer without every map growing a branch of its own. Before this,
   * ten call sites did `L.tileLayer(spec.url, …)` themselves, and a WMS
   * base map (the only Leaflet-compatible route to the Danish base maps —
   * neither Skaermkortet nor Natur- og Friluftskort has a Web Mercator
   * WMTS) could not be expressed at all.
   *
   * `spec` is a getTileSpec() result or a BASE_STYLES entry, carrying one
   * of two shapes:
   *   - `url` — a tile URL template, or a function returning one (the
   *     BASE_STYLES entries are lazy) → L.tileLayer;
   *   - `wms` — { url, layers, … } → L.tileLayer.wms.
   * `attribution` and `maxZoom` are taken from the spec when it has them.
   * A `wms.maxZoom` overrides a spec-level one — the WMS layer's own
   * ceiling is the more specific of the two — and the call site's `opts`
   * still win over both.
   *
   * `opts` are the Leaflet options the call site passes and they win over
   * everything derived from the spec, so routing a call site through this
   * builds exactly the layer it built inline before — including the two
   * different hard-coded zoom ceilings (18 in nodes.js, 19 in
   * map.js/live.js). That inconsistency is deliberately carried over, not
   * silently "fixed" here; it is its own change.
   *
   * Only keys the spec or the caller really carry are set, so a site that
   * passed no maxZoom (the home minimap) keeps Leaflet's own default
   * instead of one invented here. For the WMS branch that is a hard
   * requirement rather than tidiness: Leaflet turns every option it does
   * not recognise into a WMS request parameter, so only WMS_SPEC_KEYS may
   * ever reach it — the same discipline _createOverlayLayer follows for
   * the overlays.
   */
  var WMS_SPEC_KEYS = ['layers', 'styles', 'format', 'transparent', 'version',
                       'crs', 'tileSize', 'zoomOffset', 'minZoom', 'maxZoom', 'bounds'];

  function _withOpts(o, opts) {
    if (opts) {
      for (var k in opts) {
        if (Object.prototype.hasOwnProperty.call(opts, k)) o[k] = opts[k];
      }
    }
    return o;
  }

  function createBaseLayer(spec, opts) {
    var s = spec || {};
    var o = {};
    if (s.attribution !== undefined) o.attribution = s.attribution;
    if (s.maxZoom !== undefined) o.maxZoom = s.maxZoom;
    if (s.wms) {
      var w = s.wms;
      o.styles = '';
      o.format = 'image/png';
      o.transparent = 'FALSE';
      o.version = '1.3.0';
      o.crs = L.CRS.EPSG3857;
      o.tileSize = 256;
      for (var i = 0; i < WMS_SPEC_KEYS.length; i++) {
        var k = WMS_SPEC_KEYS[i];
        if (w[k] === undefined) continue;
        o[k] = (k === 'bounds') ? L.latLngBounds(w[k]) : w[k];
      }
      // WMS 1.3.0 (OGC 06-042 §7.3.3.9) spells TRANSPARENT as TRUE or
      // FALSE, and Dataforsyningen enforces it: Leaflet's boolean is sent
      // as "false", which the Skærmkort service answers with a
      // ServiceException ("TRANSPARENT must be either TRUE or FALSE")
      // instead of a tile. Always send the uppercase string.
      if (typeof o.transparent === 'boolean') o.transparent = o.transparent ? 'TRUE' : 'FALSE';
      var url = typeof w.url === 'function' ? w.url() : w.url;
      var layer = L.tileLayer.wms(url, _withOpts(o, opts));
      layer._mcWmsKey = _wmsKey(w, url);
      return retryErroredTiles(layer);
    }
    return L.tileLayer(typeof s.url === 'function' ? s.url() : s.url, _withOpts(o, opts));
  }

  // What makes two WMS descriptors the same layer: the resolved URL (so the
  // token too) and every option the factory passes on.
  function _wmsKey(w, url) {
    var o = {};
    for (var i = 0; i < WMS_SPEC_KEYS.length; i++) {
      if (w[WMS_SPEC_KEYS[i]] !== undefined) o[WMS_SPEC_KEYS[i]] = w[WMS_SPEC_KEYS[i]];
    }
    return url + ' ' + JSON.stringify(o);
  }

  /* #741 M1b — syncBaseLayer(group, layer, spec, opts): keeps the base
   * layer in `group` (Map's and Live's Auto group) showing `spec`, and
   * returns the layer that does. A tile template changes in place with
   * setUrl, exactly as before. A WMS style cannot be a template swap: to or
   * from one, or to another WMS, the layer is rebuilt by the factory and
   * replaces the old one in the group. The same WMS again (a theme switch
   * between styles of one layer) keeps the layer, so no tile is fetched
   * twice. O(1) per theme or provider change. */
  function syncBaseLayer(group, layer, spec, opts) {
    var w = spec && spec.wms;
    var key = w ? _wmsKey(w, typeof w.url === 'function' ? w.url() : w.url) : null;
    if (layer && (layer._mcWmsKey || null) === key) {
      if (!key) layer.setUrl(spec.url);
      return layer;
    }
    var next = createBaseLayer(spec, opts);
    if (layer) group.removeLayer(layer);
    group.addLayer(next);
    return next;
  }

  /* Optional map overlays: #680 DHM terrain hillshade, #723 protected
   * ancient monuments, #724 antenna sites (all Denmark only).
   *
   * Each is offered only when /api/config/client carries its enabled block
   * (roles.js stores them as window.MC_DHM_CFG / window.MC_HERITAGE_CFG /
   * window.MC_MASTS_CFG).
   * The plumbing below is shared: an overlay is one entry in OVERLAYS,
   * never a second copy of it.
   *
   *   - A WMS 1.3.0 EPSG:3857 tile layer in its own pane between the base
   *     tiles (tilePane, 200) and the vector overlays/markers (overlayPane
   *     400, markerPane 600). That also keeps it out of the dark-theme
   *     invert filter on the tile pane.
   *   - The antenna sites are not tiles: their entries bring their own
   *     `create` (public/masts-layer.js, canvas markers) instead.
   *   - On/off is remembered per viewer (localStorage) and mirrored in the
   *     hash (?terrain=1, ?heritage=1, ?masts=0, ?mastsInt=1). A =1 / =0 in
   *     the hash wins over the stored choice and is saved, like Live's
   *     ?heard= (#660). An overlay is off until chosen, except one marked
   *     defaultOn, whose "off" is then written to the hash as =0 while that
   *     default is on. defaultOn may be a function, asked each time: the
   *     antenna sites' and the monuments' defaults are the customizer's
   *     (#724 M5, #723 M4). The antenna sites are also marked offInHash, so
   *     their "off" is written as =0 either way. The DHM overlays have a
   *     customDefault instead (#734, the customizer's dhmOverlays): used
   *     only when neither the hash nor storage says, and their "off" is
   *     written as =0 only while that default is on (see _offInHash).
   *   - A map without an attribution control (Live) gets one for its
   *     lifetime (_ensureAttribution), because the base map's credit and
   *     every overlay's credit are required.
   *
   * DHM tiles come from the server's same-origin proxy, which adds the
   * token. The monument tiles come straight from the agency's WMS (no
   * token, open CORS); the server sends its fixed https URL.
   */
  var TERRAIN_ID    = 'dhm-terrain';
  var TERRAIN_LABEL = 'Terrain (DHM hillshade)';
  var HERITAGE_ID   = 'heritage-monuments';

  /* #734 M2 — the DHM overlays by id, as /api/config/client lists them in
   * dhm.overlays (cmd/server/dhm_proxy.go). The page sends the id as the
   * tile request's `layers`, and the proxy maps it to the upstream layer
   * and style, so no upstream name or style is ever spelled here.
   *   - terrain and surface are one either-or hillshade ("group"): one
   *     stored key and one hash parameter, ?terrain=1 (or =terrain) for
   *     terrain, as since #680, and ?terrain=surface for surface. The
   *     control shows them as two checkboxes of which at most one is
   *     checked (_uncheckGroupSiblings); both unchecked is Off.
   *   - contours is a toggle of its own, ?contours=1, above the hillshade
   *     and from the server's minZoom, so no tile is asked for below it.
   *   - #792: elevation (colour by height) is a toggle of its own,
   *     ?elevation=1, listed under the contours and drawn under the
   *     hillshade (pane 245), so the hillshade shades the colours. The server renders it
   *     from z9 to its maxZoom (13); above that the page upscales the z13
   *     tiles (maxNativeZoom) up to the block's maxZoom, with no further
   *     request. Its entry carries its own credit (the derived one, "data
   *     bearbejdet") and the ramp's metres for the legend.
   * Each credit links its own dataset (#734 Q5). A block without
   * `overlays` comes from a server before #734: terrain only, as #680. */
  var DHM_OVERLAY_LABELS = {
    terrain: function () { return 'Hillshade: terrain (DHM)'; },
    surface: function () { return 'Hillshade: surface, buildings and trees (DHM)'; },
    contours: function (cfg) { return 'Contour lines (DHM, zoom ' + cfg.minZoom + '+)'; },
    elevation: function (cfg) { return 'Colour by elevation (DHM, zoom ' + cfg.minZoom + '+)'; }
  };

  // A path on this server: not protocol-relative ("//host"), and not
  // "/\host", which browsers read the same way.
  // A same-origin absolute path: "/" and then neither "/" nor "\\" (protocol-
  // relative) nor an ASCII control or space. The URL parser strips tab, LF and
  // CR, so "/\t/host" would otherwise resolve to a foreign host (#777 review).
  function _isSameOriginPath(u) {
    return typeof u === 'string' && /^\/[^\/\\\x00-\x20]/.test(u);
  }

  function getTerrainConfig() {
    var c = window.MC_DHM_CFG;
    if (!c || c.enabled !== true || !_isSameOriginPath(c.url)) return null;
    return c;
  }

  function _isZoom(z) { return typeof z === 'number' && Math.floor(z) === z && z >= 0 && z <= 22; }

  // The tile layer config of one DHM overlay id: the block's URL, bounds
  // and credit with the overlay's own zoom range and dataset link, and
  // `layers` = the id. null while the server does not offer it.
  function getDHMOverlayConfig(id) {
    var c = getTerrainConfig();
    if (!c) return null;
    if (!Array.isArray(c.overlays)) {
      return id === 'terrain' ? Object.assign({}, c, { layers: c.layer, label: TERRAIN_LABEL }) : null;
    }
    for (var i = 0; i < c.overlays.length; i++) {
      var e = c.overlays[i];
      if (!e || e.id !== id || !DHM_OVERLAY_LABELS[id]) continue;
      if (!_isZoom(e.minZoom) || !_isZoom(e.maxZoom) || e.maxZoom > 19 || e.minZoom > e.maxZoom) return null;
      var cfg = Object.assign({}, c, {
        layers: id, minZoom: e.minZoom, maxZoom: e.maxZoom,
        datasetName: e.datasetName, datasetUrl: e.datasetUrl
      });
      if (id === 'contours') cfg.minZoom = _contourMinZoom(e, _dhmSection());
      if (id === 'elevation') {
        // #792: its own credit and legend; #860: its ramp id, sent as the
        // tile's WMS styles. A malformed entry is not offered.
        var legend = _legendM(e.legendM);
        if (typeof e.attribution !== 'string' || !e.attribution || !legend) return null;
        if (typeof e.style !== 'string' || !RAMP_STYLE_RE.test(e.style)) return null;
        cfg.attribution = e.attribution;
        cfg.legendM = legend;
        cfg.style = e.style;
        cfg.maxNativeZoom = e.maxZoom;
        if (_isZoom(c.maxZoom) && c.maxZoom <= 19 && c.maxZoom > e.maxZoom) cfg.maxZoom = c.maxZoom;
      }
      cfg.label = DHM_OVERLAY_LABELS[id](cfg);
      return cfg;
    }
    return null;
  }

  // #860: the ramp id the server renders the tiles with (dhm.ElevationStyle,
  // "ramp-" and hex digits). It is the tile URL's `styles`, so a new ramp
  // is a URL no browser or shared cache has a tile of.
  var RAMP_STYLE_RE = /^[a-z0-9-]{1,32}$/;

  // #792: the legend's ramp stops, whole metres rising from 0, 2 to 16 of
  // them; anything else is null.
  function _legendM(a) {
    if (!Array.isArray(a) || a.length < 2 || a.length > 16 || a[0] !== 0) return null;
    for (var i = 0; i < a.length; i++) {
      if (typeof a[i] !== 'number' || Math.floor(a[i]) !== a[i] || a[i] > 9000 || (i && a[i] <= a[i - 1])) return null;
    }
    return a.slice();
  }

  /* #792 — the elevation legend under its toggle: a bar of the ramp's
   * colours, which are CSS variables (--dhm-elevation-<m>, public/
   * style.css, the server's ramp: TestDHM792_LegendCSSMatchesTheRamp), and
   * the metres of some stops at their place on it. Built with
   * createElement and textContent only.
   * #860: the stops are not evenly spaced in metres (most of Denmark lies
   * below 50 m), so every stop gets an equal step of the bar, as each band
   * holds a fair share of the land. Labels: the first; then each stop at
   * least a seventh of the bar after the previous label (about 26 px of
   * the 180 px bar) and not past four fifths, where it would crowd the
   * last; and the last with its unit. The aria-label names every stop. */
  var LEGEND_LABEL_GAP = 1 / 7;
  var LEGEND_LABEL_END = 4 / 5;

  function _legendPos(legendM, i) { return i / (legendM.length - 1); }

  function elevationLegendLabels(legendM) {
    var n = legendM.length, prev = 0;
    return legendM.filter(function (m, i) {
      var p = _legendPos(legendM, i);
      if (i === 0 || i === n - 1) return true;
      if (p - prev < LEGEND_LABEL_GAP - 1e-9 || p > LEGEND_LABEL_END + 1e-9) return false;
      prev = p;
      return true;
    });
  }

  function buildElevationLegend(doc, legendM) {
    var top = legendM[legendM.length - 1];
    var box = doc.createElement('div');
    box.className = 'mc-elev-legend';
    box.setAttribute('role', 'img');
    box.setAttribute('aria-label', 'Elevation colours from ' + legendM[0] + ' to ' + top + ' m, stops at ' +
      legendM.slice(0, -1).join(', ') + ' and ' + top + ' m');
    var bar = doc.createElement('div');
    bar.className = 'mc-elev-legend-bar';
    bar.style.background = 'linear-gradient(to right, ' + legendM.map(function (m, i) {
      return 'var(--dhm-elevation-' + m + ') ' + (_legendPos(legendM, i) * 100) + '%';
    }).join(', ') + ')';
    box.appendChild(bar);
    var scale = doc.createElement('div');
    scale.className = 'mc-elev-legend-scale';
    var shown = elevationLegendLabels(legendM);
    shown.forEach(function (m, i) {
      var t = doc.createElement('span');
      t.textContent = i === shown.length - 1 ? m + ' m' : String(m);
      // The first label starts at the bar's left end, the last ends at its
      // right end, the others are centred on their stop.
      t.style.left = (_legendPos(legendM, legendM.indexOf(m)) * 100) + '%';
      if (i === shown.length - 1) t.style.transform = 'translateX(-100%)';
      else if (i > 0) t.style.transform = 'translateX(-50%)';
      scale.appendChild(t);
    });
    box.appendChild(scale);
    return box;
  }

  /* #792 (#831 F1) — the legend of this control build shows only while the
   * elevation layer is on the map: hidden now if it is off, then shown and
   * hidden with each toggle. One listener per map; each control build
   * replaces the legend it updates. */
  function _followElevationLegend(map, legend) {
    var on = function () { return !!(map._mcElevationLayer && map.hasLayer(map._mcElevationLayer)); };
    legend.hidden = !on();
    map._mcElevationLegend = legend;
    if (map._mcElevationLegendWired) return;
    map._mcElevationLegendWired = true;
    map.on('overlayadd overlayremove', function (e) {
      if (!e.layer || e.layer !== map._mcElevationLayer || !map._mcElevationLegend) return;
      map._mcElevationLegend.hidden = e.type === 'overlayremove';
    });
  }

  /* #734 (customizer, AGENTS rule 8) — this viewer's DHM overlay defaults,
   * the customizer section `dhmOverlays` on window.SITE_CONFIG
   * (public/customize-v2.js). The server's map.dhm config stays server-only
   * (#681 M6): only the zoom range of an offered overlay is read here.
   *   - hillshade: which hillshade a map starts with when neither the link
   *     nor the layer control says (off, terrain or surface);
   *   - contours: whether the contour lines start on;
   *   - contourMinZoom: the first zoom the contours draw at, inside the
   *     server's range (it can only be raised to at most the maximum).
   * Built-ins: off, off and the server's minZoom, so a viewer who sets
   * nothing sees exactly M2. The precedence is that of #724 M5: the link,
   * then the choice this browser remembers, then this default.
   *   - elevationOpacity (#792): the colour-by-elevation layer's opacity in
   *     whole percent, 10-100, built-in DHM_ELEVATION_OPACITY. Present in
   *     the settings only while the server offers that overlay. */
  var DHM_SETTING_KEYS = ['hillshade', 'contours', 'contourMinZoom', 'elevationOpacity'];
  var DHM_HILLSHADE_VALUES = ['off', 'terrain', 'surface'];
  var DHM_ELEVATION_OPACITY = 60;
  var DHM_ELEVATION_OPACITY_RANGE = [10, 100];

  function validDHMSetting(key, v) {
    if (key === 'hillshade') return DHM_HILLSHADE_VALUES.indexOf(v) >= 0;
    if (key === 'contours') return typeof v === 'boolean';
    if (key === 'contourMinZoom') return _isZoom(v) && v <= 19;
    if (key === 'elevationOpacity') {
      return typeof v === 'number' && Math.floor(v) === v &&
        v >= DHM_ELEVATION_OPACITY_RANGE[0] && v <= DHM_ELEVATION_OPACITY_RANGE[1];
    }
    return false;
  }

  function _dhmSection() {
    try {
      var s = window.SITE_CONFIG && window.SITE_CONFIG.dhmOverlays;
      return s && typeof s === 'object' ? s : {};
    } catch (_) { return {}; }
  }

  // The contour minZoom: the section's, inside the server entry's range.
  function _contourMinZoom(e, section) {
    var z = section && validDHMSetting('contourMinZoom', section.contourMinZoom) ? section.contourMinZoom : e.minZoom;
    return Math.max(e.minZoom, Math.min(e.maxZoom, z));
  }

  // The server's contour zoom range [min, max], or null while not offered.
  function dhmContourZoomRange() {
    var c = getTerrainConfig();
    if (!c || !Array.isArray(c.overlays) || !getDHMOverlayConfig('contours')) return null;
    for (var i = 0; i < c.overlays.length; i++) {
      var e = c.overlays[i];
      if (e && e.id === 'contours') return [e.minZoom, e.maxZoom];
    }
    return null;
  }

  /** The effective DHM overlay defaults: `section` (default: the live
   *  SITE_CONFIG.dhmOverlays) over the built-ins. contourMinZoom is null
   *  while the server offers no contours. */
  function dhmOverlaySettings(section) {
    if (section === undefined) section = _dhmSection();
    var src = section && typeof section === 'object' ? section : {};
    var range = dhmContourZoomRange();
    var out = {
      hillshade: validDHMSetting('hillshade', src.hillshade) ? src.hillshade : 'off',
      contours: validDHMSetting('contours', src.contours) ? src.contours : false,
      contourMinZoom: range ? _contourMinZoom({ minZoom: range[0], maxZoom: range[1] }, src) : null
    };
    if (getDHMOverlayConfig('elevation')) {
      out.elevationOpacity = validDHMSetting('elevationOpacity', src.elevationOpacity) ? src.elevationOpacity : DHM_ELEVATION_OPACITY;
    }
    return out;
  }

  // The DHM ids the server offers, in the control's order.
  function offeredDHMOverlays() {
    return ['terrain', 'surface', 'contours', 'elevation'].filter(function (id) { return !!getDHMOverlayConfig(id); });
  }

  function getHeritageConfig() {
    var c = window.MC_HERITAGE_CFG;
    if (!c || c.enabled !== true || typeof c.url !== 'string' || c.url.indexOf('https://') !== 0) return null;
    if (typeof c.layers !== 'string' || !c.layers || !_isZoom(c.minZoom) || !_isZoom(c.maxZoom)) return null;
    if (!Array.isArray(c.bounds) || c.bounds.length !== 2) return null;
    return c;
  }

  /* #723 M4 — the monuments settings this viewer may change in the
   * customizer (public/customize-v2.js, section `heritage`), and the one
   * rule for them, shared by the overlay below, the Path profile warning
   * (public/terrain-profile.js) and the customizer itself:
   *   - enabled: whether the overlay starts on (M1: off until chosen);
   *   - opacityPct: the overlay's opacity in percent (M1: 85);
   *   - profileWarning: whether the profile looks up and shows the
   *     monument warning at all (M3: on);
   *   - warnDistanceM: list an end outside a zone only up to this many
   *     metres from it; null (the default) lists everything the server
   *     reports, which is at most map.heritage.radiusM. An end inside a
   *     zone is always listed. The server never reports past its radius,
   *     and that is bounded by internal/heritage MaxMarginM (250), so the
   *     viewer's value can only narrow the warning.
   * The server's own map.heritage values (radius, cache, rate limits) decide
   * what goes upstream and stay server-only (as #681 M6 and #727 M4). */
  var HERITAGE_SETTING_KEYS = ['enabled', 'opacityPct', 'profileWarning', 'warnDistanceM'];
  var HERITAGE_DEFAULTS = { enabled: false, opacityPct: 85, profileWarning: true, warnDistanceM: null };
  var HERITAGE_LIMITS = { opacityPct: [10, 100], warnDistanceM: [0, 250] };

  /** Whether `v` is a usable value of monuments setting `key`. */
  function validHeritageSetting(key, v) {
    if (key === 'enabled' || key === 'profileWarning') return typeof v === 'boolean';
    var b = Object.prototype.hasOwnProperty.call(HERITAGE_LIMITS, key) ? HERITAGE_LIMITS[key] : null;
    return !!b && typeof v === 'number' && Math.floor(v) === v && v >= b[0] && v <= b[1];
  }

  /** The effective monuments settings: `section` (default: the live
   *  window.SITE_CONFIG.heritage) over the defaults, valid values only. */
  function heritageSettings(section) {
    var src = section;
    if (src === undefined) {
      try { src = window.SITE_CONFIG && window.SITE_CONFIG.heritage; } catch (_) { src = null; }
    }
    var out = {};
    for (var i = 0; i < HERITAGE_SETTING_KEYS.length; i++) {
      var k = HERITAGE_SETTING_KEYS[i];
      out[k] = src && typeof src === 'object' && Object.prototype.hasOwnProperty.call(src, k) &&
        validHeritageSetting(k, src[k]) ? src[k] : HERITAGE_DEFAULTS[k];
    }
    return out;
  }

  // #724 — the masts block as cmd/server/masts_proxy.go sends it: a
  // same-origin path, integer zoom gate and cell size, the four-index cell
  // extent and an https source link. Needs public/masts-layer.js.
  function getMastsConfig() {
    var c = window.MC_MASTS_CFG;
    if (!c || c.enabled !== true || !window.MastsLayer) return null;
    if (!_isSameOriginPath(c.url)) return null;
    if (!_isZoom(c.minZoom) || typeof c.cellSizeM !== 'number' || !(c.cellSizeM > 0) || Math.floor(c.cellSizeM) !== c.cellSizeM) return null;
    if (!Array.isArray(c.cellExtent) || c.cellExtent.length !== 4) return null;
    for (var i = 0; i < 4; i++) if (typeof c.cellExtent[i] !== 'number' || Math.floor(c.cellExtent[i]) !== c.cellExtent[i]) return null;
    if (typeof c.sourceUrl !== 'string' || c.sourceUrl.indexOf('https://') !== 0 || typeof c.attribution !== 'string') return null;
    return c;
  }

  // The DHM overlays' opacity is fixed here, but for the elevation colours'
  // (#792, the customizer's elevationOpacity). Their defaults and the
  // contour min-zoom come from the customizer (#734), as do the
  // monuments' default and opacity (#723 M4) and the antenna sites'
  // default (#724 M5). An entry's `opacity` may be a function, read
  // whenever the layer is added; with `settingsEvent` the layer also
  // follows that window event while it is on the map.
  var OVERLAYS = [
    {
      id: TERRAIN_ID, storageKey: 'mc-terrain-dhm', hashParam: 'terrain', layerProp: '_mcTerrainLayer',
      group: 'dhm-hillshade', onValues: ['1', 'terrain'],
      pane: 'mcTerrainPane', paneZ: 250, paneClass: 'mc-terrain-pane', className: 'mc-terrain-tiles', opacity: 0.65,
      retryPlaceholders: true, // #728 N3: a DHM proxy tile
      // #734: this viewer's customizer default (dhmOverlaySettings).
      customDefault: function () { return dhmOverlaySettings().hillshade === 'terrain'; },
      config: function () { return getDHMOverlayConfig('terrain'); },
      label: function (cfg) { return cfg.label; },
      layers: function (cfg) { return cfg.layers; }
    },
    {
      // #734 M2: the surface hillshade, the terrain one's either-or twin
      // (same key, parameter, pane and opacity).
      id: 'dhm-surface', storageKey: 'mc-terrain-dhm', hashParam: 'terrain', layerProp: '_mcSurfaceLayer',
      group: 'dhm-hillshade', onValues: ['surface'],
      pane: 'mcTerrainPane', paneZ: 250, paneClass: 'mc-terrain-pane', className: 'mc-surface-tiles', opacity: 0.65,
      retryPlaceholders: true,
      customDefault: function () { return dhmOverlaySettings().hillshade === 'surface'; },
      config: function () { return getDHMOverlayConfig('surface'); },
      label: function (cfg) { return cfg.label; },
      layers: function (cfg) { return cfg.layers; }
    },
    {
      // #734 M2: contour lines, drawn as served (opacity 1) between the
      // hillshade (250) and the monuments (260).
      id: 'dhm-contours', storageKey: 'mc-contours-dhm', hashParam: 'contours', layerProp: '_mcContourLayer',
      pane: 'mcContourPane', paneZ: 255, paneClass: 'mc-contour-pane', className: 'mc-contour-tiles', opacity: 1,
      retryPlaceholders: true,
      customDefault: function () { return dhmOverlaySettings().contours; },
      config: function () { return getDHMOverlayConfig('contours'); },
      label: function (cfg) { return cfg.label; },
      layers: function (cfg) { return cfg.layers; }
    },
    {
      // #792: colour by elevation, rendered by the server. Listed under the
      // contour lines (#831 F1: on a 375x560 phone a row above them pushed
      // them out of the visible list) but drawn under the hillshade (245 <
      // 250), outside the dark-theme filters: the colours are shown as
      // served, at this viewer's opacity. Its legend shows only while it
      // is on, so the off row costs one line.
      id: 'dhm-elevation', storageKey: 'mc-elevation-dhm', hashParam: 'elevation', layerProp: '_mcElevationLayer',
      pane: 'mcElevationPane', paneZ: 245, paneClass: 'mc-elevation-pane', className: 'mc-elevation-tiles',
      opacity: function () { return dhmOverlaySettings().elevationOpacity / 100; },
      settingsEvent: 'dhm-defaults-changed',
      retryPlaceholders: true,
      config: function () { return getDHMOverlayConfig('elevation'); },
      label: function (cfg) { return cfg.label; },
      layers: function (cfg) { return cfg.layers; },
      // #860: the ramp id, so a new ramp is a new tile URL.
      styles: function (cfg) { return cfg.style; },
      decorate: function (map, cfg, labelEl) {
        var doc = labelEl.ownerDocument || document;
        if (!labelEl.parentNode) return;
        var legend = buildElevationLegend(doc, cfg.legendM);
        _followElevationLegend(map, legend);
        labelEl.parentNode.insertBefore(legend, labelEl.nextSibling);
      }
    },
    {
      // Protection-zone polygons under the monument points. The WMS draws
      // nothing further out than minZoom, so the label says where it starts.
      id: HERITAGE_ID, storageKey: 'mc-heritage', hashParam: 'heritage', layerProp: '_mcHeritageLayer',
      pane: 'mcHeritagePane', paneZ: 260, paneClass: 'mc-heritage-pane', className: 'mc-heritage-tiles',
      defaultOn: function () { return heritageSettings().enabled; },
      opacity: function () { return heritageSettings().opacityPct / 100; },
      settingsEvent: 'heritage-defaults-changed',
      config: getHeritageConfig,
      label: function (cfg) { return 'Protected ancient monuments (zoom ' + cfg.minZoom + '+)'; },
      layers: function (cfg) { return cfg.layers; }
    },
    {
      // #724 option A: every site, honestly labelled, on when offered
      // unless this viewer turned the default off in the customizer.
      id: 'masts-sites', storageKey: 'mc-masts', hashParam: 'masts', layerProp: '_mcMastsSitesLayer',
      defaultOn: function () { return window.MastsLayer.settings(getMastsConfig()).enabled; }, offInHash: true,
      config: getMastsConfig,
      label: function () { return window.MastsLayer.SITE_LABEL; },
      // #859 — the zoom gate as the row's hint; this viewer's, as the
      // status line under the pair shows it (the label stays the layer's
      // key, so it carries none).
      hint: function (cfg) { return 'zoom ' + window.MastsLayer.settings(cfg).minZoom + '+'; },
      create: function (map, cfg) { return window.MastsLayer.layer(map, cfg, 'sites'); }
    },
    {
      // Sites with a 700/800/900 MHz entry; off until chosen. The status
      // line (zoom gate, loading, unavailable) sits under this, the last
      // of the pair.
      id: 'masts-interference', storageKey: 'mc-masts-int', hashParam: 'mastsInt', layerProp: '_mcMastsIntLayer',
      config: getMastsConfig,
      label: function () { return window.MastsLayer.INTERFERENCE_LABEL; },
      create: function (map, cfg) { return window.MastsLayer.layer(map, cfg, 'interference'); },
      decorate: function (map, cfg, labelEl) { window.MastsLayer.decorate(map, cfg, labelEl); }
    }
  ];

  /* Whether an overlay is on: a hash value the parameter knows wins, then a
   * known stored value, then the default. `onValues` are the values that
   * mean this overlay and `knownValues` every value of its parameter
   * (#734: the hillshade pair shares one); both default to #680's '1' and
   * '1'/'0'. */
  function resolveOverlayOn(hashValue, stored, defaultOn, onValues, knownValues) {
    var onv = onValues || ['1'], known = knownValues || ['1', '0'];
    if (known.indexOf(hashValue) >= 0) return onv.indexOf(hashValue) >= 0;
    if (known.indexOf(stored) >= 0) return onv.indexOf(stored) >= 0;
    return !!defaultOn;
  }

  function _onValues(o) { return o.onValues || ['1']; }

  // Whether o starts on when neither the link nor storage says: its fixed
  // (or #724 M5 function) defaultOn, else its #734 customizer default.
  function _defaultOn(o) {
    if (typeof o.defaultOn === 'function') return !!o.defaultOn();
    if (o.defaultOn) return true;
    return !!(o.customDefault && o.customDefault());
  }

  // o and the overlays sharing its hash parameter (o may be outside
  // OVERLAYS, like the ortho pick).
  function _paramOverlays(o) {
    return [o].concat(OVERLAYS.filter(function (m) { return m !== o && m.hashParam === o.hashParam; }));
  }

  // Every value of o's hash parameter: '0' and each overlay's on values.
  function _knownValues(o) {
    var known = ['0'];
    _paramOverlays(o).forEach(function (m) { known = known.concat(_onValues(m)); });
    return known;
  }

  // The value stored for hash value v of o's parameter: the first on value
  // of the overlay v means ('terrain' is stored as #680's '1'), else '0'.
  function _canonicalValue(o, v) {
    var ms = _paramOverlays(o);
    for (var i = 0; i < ms.length; i++) {
      if (_onValues(ms[i]).indexOf(v) >= 0) return _onValues(ms[i])[0];
    }
    return '0';
  }

  function _hashParts() {
    var hash = (typeof location !== 'undefined' && location.hash) || '';
    var qi = hash.indexOf('?');
    return { base: qi >= 0 ? hash.slice(0, qi) : hash, params: new URLSearchParams(qi >= 0 ? hash.slice(qi + 1) : '') };
  }

  function _rawOverlayPref(o) {
    var hv = null, stored = null;
    try { hv = _hashParts().params.get(o.hashParam); } catch (_) {}
    try { stored = window.localStorage && window.localStorage.getItem(o.storageKey); } catch (_) {}
    return { hv: hv, stored: stored };
  }

  function _readOverlayPref(o) {
    var p = _rawOverlayPref(o), known = _knownValues(o);
    var on = resolveOverlayOn(p.hv, p.stored, _defaultOn(o), _onValues(o), known);
    if (known.indexOf(p.hv) >= 0) {
      try { if (window.localStorage) window.localStorage.setItem(o.storageKey, _canonicalValue(o, p.hv)); } catch (_) {}
    }
    return on;
  }

  /* Forgets this viewer's remembered on/off of one overlay, so its default
   * applies again on the next map: the customizer calls it when it changes
   * that default (#724 M5). A hash =1/=0 still wins. */
  function forgetOverlayPref(id) {
    for (var i = 0; i < OVERLAYS.length; i++) {
      if (OVERLAYS[i].id !== id) continue;
      try { if (window.localStorage) window.localStorage.removeItem(OVERLAYS[i].storageKey); } catch (_) {}
    }
  }

  /* Whether turning o off writes =0 into the hash rather than dropping the
   * parameter: an overlay that is on by default, now (a function default
   * is asked each time, #723 M4), one marked offInHash, whose "off" is
   * written either way (the antenna sites, #724 M5), or one sharing its
   * parameter with an overlay whose #734 customizer default is on. */
  function _offInHash(o) {
    if (o.offInHash) return true;
    if (typeof o.defaultOn === 'function' ? o.defaultOn() : o.defaultOn) return true;
    return _paramOverlays(o).some(function (m) { return m.customDefault && m.customDefault(); });
  }

  // replaceState, so the SPA router does not re-run on the hash rewrite.
  function _saveOverlayPref(o, on) {
    if (!on && o.group) {
      // A hillshade turned off while the other one is the choice (the
      // control switching terrain → surface) leaves that choice alone.
      var p = _rawOverlayPref(o);
      if (!resolveOverlayOn(p.hv, p.stored, true, _onValues(o), _knownValues(o))) return;
    }
    try { if (window.localStorage) window.localStorage.setItem(o.storageKey, on ? _onValues(o)[0] : '0'); } catch (_) {}
    try {
      var h = _hashParts();
      if (on) h.params.set(o.hashParam, _onValues(o)[0]);
      else if (_offInHash(o)) h.params.set(o.hashParam, '0');
      else h.params.delete(o.hashParam);
      _writeHash(h);
    } catch (_) {}
  }

  // replaceState, so the SPA router does not re-run on the hash rewrite.
  function _writeHash(h) {
    var qs = h.params.toString();
    // An empty hash gets a bare '#', so the parameter never lands in the
    // query string (#682 review F4).
    history.replaceState(history.state, '', location.pathname + location.search + (qs ? (h.base || '#') + '?' + qs : h.base));
  }

  /* #839 finding 8 — Reset All forgets every on/off choice made in a map's
   * layer control (the overlays and the Aerial base map): the remembered
   * value, and the parameter the control wrote into the current link, so a
   * reload of this page starts from the defaults too. The map on screen
   * keeps its layers until it is next built. */
  function forgetMapLayerPrefs() {
    var prefs = OVERLAYS.concat([ORTHO_PREF]);
    prefs.forEach(function (o) {
      try { if (window.localStorage) window.localStorage.removeItem(o.storageKey); } catch (_) {}
    });
    try {
      var h = _hashParts(), changed = false;
      prefs.forEach(function (o) {
        if (h.params.has(o.hashParam)) { h.params.delete(o.hashParam); changed = true; }
      });
      if (changed) _writeHash(h);
    } catch (_) {}
  }

  /* #733 — Dataforsyningen's terms for Klimadatastyrelsen data under
   * CC BY 4.0 ("Vilkår for brug af data", v3.1,
   * https://dataforsyningen.dk/Vilkaar) require the credit to link the
   * dataset and the licence, so a DHM credit renders those two marks inside
   * its own text as anchors. The marks and their targets come from the
   * server's dhm block (cmd/server/dhm_proxy.go), which takes them from
   * internal/dhm — the one credit definition.
   *
   * appendCredit is the one walker that does it (#765): the overlay credit
   * below and the profile chart footer (public/terrain-profile.js) both go
   * through it. It appends `text` to `parent` as text nodes, with each
   * link's mark turned into an anchor, so no credit string is ever
   * concatenated into markup. Only an absolute https target becomes a link;
   * anything else leaves the text alone, so a credit never silently turns
   * into a javascript: link and a block without the fields degrades to the
   * plain credit. A mark that is not in the rest of the text is skipped.
   * `decorate(a, link)`, when given, may add a class or a test id to each
   * anchor. Returns how many anchors it made.
   */
  function appendCredit(parent, text, links, decorate) {
    var rest = typeof text === 'string' ? text : '', linked = 0;
    (links || []).forEach(function (l) {
      if (!l || typeof l.mark !== 'string' || !l.mark) return;
      if (typeof l.href !== 'string' || l.href.indexOf('https://') !== 0) return;
      var i = rest.indexOf(l.mark);
      if (i < 0) return;
      if (i > 0) parent.appendChild(document.createTextNode(rest.slice(0, i)));
      var a = document.createElement('a');
      a.setAttribute('href', l.href);
      a.setAttribute('target', '_blank');
      a.setAttribute('rel', l.rel || 'noopener noreferrer');
      a.textContent = l.mark;
      if (decorate) decorate(a, l);
      parent.appendChild(a);
      rest = rest.slice(i + l.mark.length);
      linked++;
    });
    if (rest) parent.appendChild(document.createTextNode(rest));
    return linked;
  }

  /* Leaflet's attribution control takes an HTML string, so the overlay
   * credit is walked into a detached container and handed over as its
   * innerHTML: the DOM does the escaping. The rendered TEXT stays
   * byte-identical to the plain credit — only the markup differs — which
   * matters because Leaflet's control counts, finds and removes a credit by
   * its string. With nothing linked it is the plain credit itself. */
  // #741: the registry builds the Dataforsyningen credits when it loads, so
  // a page (or a test sandbox) whose DOM cannot build one gets the plain
  // credit.
  function buildCredit(text, links) {
    var plain = typeof text === 'string' ? text : '';
    if (!plain || typeof document === 'undefined' || !document.createElement) return plain;
    try {
      var box = document.createElement('div');
      return appendCredit(box, plain, links) ? box.innerHTML : plain;
    } catch (_) { return plain; }
  }

  /* The credit links an overlay's config offers. An overlay whose owner's
   * terms ask for no link (the #723 monuments: free use under the agency's
   * own rules, textual data CC0) carries none of these fields and keeps its
   * plain credit. `key` names each link for a caller that decorates it.
   *
   * Ordering contract (#765): appendCredit consumes the marks in text
   * order. Each mark is looked for only in the text after the previous
   * anchor, so this list must follow the order the marks occur in the
   * credit — the dataset before the licence, as in "…, Danmarks
   * Højdemodel, CC BY 4.0". A mark listed after one that follows it in the
   * text is not found and stays plain text, silently. */
  function creditLinks(cfg) {
    return [
      { key: 'dataset', mark: cfg.datasetName, href: cfg.datasetUrl, rel: 'noopener noreferrer' },
      { key: 'license', mark: cfg.licenseName, href: cfg.licenseUrl, rel: 'noopener noreferrer license' }
    ];
  }

  // The marks are consumed in text order: see the ordering contract above.
  function _overlayCredit(cfg) {
    return buildCredit(cfg.attribution, creditLinks(cfg));
  }

  /* #728 N3 (#769 review) — one retry of a DHM placeholder tile.
   *
   * The DHM proxy answers a tile it cannot serve with a 1×1 transparent
   * PNG: `busy` when its admission bound of cold tiles (#744) is full,
   * `unavailable` after an upstream failure (both no-store), `outside`
   * beyond the coverage (cached for a day). With the ortho base layer and
   * the hillshade both cold, a view is about 120 tiles against 64
   * admission slots, so part of it comes back busy, and Leaflet keeps a
   * loaded tile: it stayed blank until it left the view.
   *
   * An <img> cannot read the X-CoreScope-Tile header, but every real DHM
   * tile is 256×256, so a 1×1 image is a placeholder. Each one is reloaded
   * once (same URL), RETRY_MS plus up to RETRY_JITTER_MS later, so the
   * retries of one view are spread out instead of arriving together, and
   * only if it is still loaded (not unloaded or aborted, which is also
   * what happens to every tile when its layer leaves the map) and in view
   * at the map's zoom. A no-store answer is fetched again; an `outside`
   * one is not: it is cached (Chromium HTTP cache, max-age one day), so the
   * same-URL reload is answered from the cache, is not refetched, and
   * costs no request. Bounded: one
   * retry per tile element (so at most one extra request per tile), and
   * every pending timer is cleared when the layer leaves the map.
   */
  var RETRY_MS = 3000;
  var RETRY_JITTER_MS = 2000;

  function _tileInView(map, layer, c) {
    if (c.z !== Math.round(map.getZoom())) return false;
    var ts = layer.getTileSize(), pb = map.getPixelBounds();
    return c.x * ts.x < pb.max.x && (c.x + 1) * ts.x > pb.min.x &&
           c.y * ts.y < pb.max.y && (c.y + 1) * ts.y > pb.min.y;
  }

  /* The one retry both cases below share: on `event`, a tile `needsRetry`
   * accepts is reloaded once (same URL) after the jittered delay, if it is
   * still loaded and in view on the map the layer is on then (`map`, or
   * the layer's own map for a base layer that any page may add). */
  function _retryTilesOnce(map, layer, event, needsRetry) {
    var pending = [];
    layer.on(event, function (e) {
      var img = e.tile, coords = e.coords;
      if (!img || img._mcRetried || !needsRetry(img)) return;
      img._mcRetried = true;
      var t = setTimeout(function () {
        var i = pending.indexOf(t);
        if (i >= 0) pending.splice(i, 1);
        var m = map || layer._map;
        if (img._mcGone || !m || !_tileInView(m, layer, coords)) return;
        img.src = img.src;
      }, RETRY_MS + Math.floor(Math.random() * RETRY_JITTER_MS));
      pending.push(t);
    });
    layer.on('tileunload tileabort', function (e) { if (e.tile) e.tile._mcGone = true; });
    layer.on('remove', function () {
      pending.forEach(function (t) { clearTimeout(t); });
      pending = [];
    });
    return layer;
  }

  function retryPlaceholderTiles(map, layer) {
    return _retryTilesOnce(map, layer, 'tileload', function (img) {
      return img.naturalWidth === 1 && img.naturalHeight === 1;
    });
  }

  /* #839 — one retry of a failed WMS base-map tile. Dataforsyningen's WMS
   * (Natur- og Friluftskort above all) sometimes answers a GetMap with
   * HTTP 200 and a ServiceException in XML ("msDrawMap(): Image handling
   * error"); Chrome blocks that answer (ERR_BLOCKED_BY_ORB), the tile fires
   * `tileerror` and Leaflet never asks for it again, so a 512 px block
   * stayed blank until it left the view. The same URL a few seconds later
   * is a tile. Same bounds as the DHM placeholders: one retry per tile
   * element, only while it is loaded and in view, every timer cleared when
   * the layer leaves the map. */
  function retryErroredTiles(layer) {
    return _retryTilesOnce(null, layer, 'tileerror', function () { return true; });
  }

  function _createOverlayLayer(map, o, cfg) {
    if (o.create) return o.create(map, cfg);
    if (!map.getPane(o.pane)) {
      var pane = map.createPane(o.pane);
      pane.style.zIndex = String(o.paneZ);
      pane.style.pointerEvents = 'none';
      pane.classList.add(o.paneClass);
    }
    // Leaflet turns every option it does not know into a WMS parameter, so
    // only these may be passed: the request is then exactly service,
    // request, layers, styles, format, transparent, version, width, height,
    // crs and bbox (the DHM proxy accepts nothing else). `bounds` keeps
    // Leaflet from requesting tiles outside the data at all.
    var opts = {
      layers: o.layers(cfg),
      styles: o.styles ? o.styles(cfg) : '',
      format: 'image/png',
      transparent: true,
      version: '1.3.0',
      crs: L.CRS.EPSG3857,
      tileSize: 256,
      opacity: _overlayOpacity(o),
      pane: o.pane,
      minZoom: cfg.minZoom,
      maxZoom: cfg.maxZoom,
      bounds: L.latLngBounds(cfg.bounds),
      attribution: _overlayCredit(cfg),
      className: o.className
    };
    // #792: a GridLayer option, so never a WMS parameter.
    if (cfg.maxNativeZoom) opts.maxNativeZoom = cfg.maxNativeZoom;
    var layer = L.tileLayer.wms(cfg.url, opts);
    if (o.settingsEvent) _followSettings(layer, o);
    return o.retryPlaceholders ? retryPlaceholderTiles(map, layer) : layer;
  }

  // An overlay's opacity: fixed, or this viewer's customizer value
  // (#723 M4 monuments, #792 elevation colours).
  function _overlayOpacity(o) { return typeof o.opacity === 'function' ? o.opacity() : o.opacity; }

  /* #723 M4 — a layer whose opacity the customizer sets reads it each time
   * it is added and follows o.settingsEvent while it is on the map; the
   * listener goes with the layer, so a page left behind keeps none. */
  function _followSettings(layer, o) {
    var sync = function () { layer.setOpacity(_overlayOpacity(o)); };
    layer.on('add', function () {
      sync();
      try { window.addEventListener(o.settingsEvent, sync); } catch (_) {}
    });
    layer.on('remove', function () {
      try { if (window.removeEventListener) window.removeEventListener(o.settingsEvent, sync); } catch (_) {}
    });
  }

  // The class the compact (inset) credit is styled by; public/style.css
  // owns its size, and MC_CREDIT_COMPACT_CLASS exposes the name so a test
  // or a caller never spells it a second time.
  var CREDIT_COMPACT_CLASS = 'mc-credit-compact';

  // Live runs without Leaflet's default attribution control, so it gets
  // one here, once, when its layer control is built, and keeps it: the
  // base map's credit must always show (Esri requires "Powered by Esri"),
  // with each overlay's credit next to it while that overlay is on. From
  // then on Leaflet's control follows layeradd and each layer's 'remove'
  // itself, except for the layers already on the map when it is added (the
  // Auto tile layer): it counts their credits without the removal hook it
  // gives later layers, so they get that hook here, or picking another base
  // map would leave the old credit behind. The Map page has its own control
  // and is left alone.
  //
  // #752: the inset maps (node detail, node-reach, packet-path, area-nodes,
  // rx-coverage and the Analytics subpath minimap) are built the same way
  // and render the same provider, so they call this through
  // MC_ensureAttribution with compact: true. That only adds the
  // MC_CREDIT_COMPACT_CLASS to the control's container; the sizing lives in
  // CSS, so a 200px inset is not swallowed by a long credit.
  function _ensureAttribution(map, compact) {
    if (map.attributionControl) return null;
    var control = L.control.attribution({ position: 'bottomright', prefix: false }).addTo(map);
    if (compact) {
      try {
        var el = control.getContainer && control.getContainer();
        if (el && el.classList) el.classList.add(CREDIT_COMPACT_CLASS);
      } catch (_) {}
    }
    map.eachLayer(function (layer) {
      if (!layer.getAttribution || !layer.getAttribution()) return;
      layer.once('remove', function () { control.removeAttribution(layer.getAttribution()); });
    });
    return control;
  }

  // Changes a layer's credit in place: Live's Auto layer switches provider
  // with setUrl, and the control counts credits by their text, so without
  // this it would go on showing the old provider's credit instead of the
  // new one's.
  function setLayerAttribution(map, layer, text) {
    var old = layer.options.attribution;
    layer.options.attribution = text;
    var control = map.attributionControl;
    if (old === text || !control || !map.hasLayer(layer)) return;
    control.removeAttribution(old);
    control.addAttribution(text);
  }

  // Once per map and overlay: listen for the user's toggles and apply the
  // initial choice. Runs before the layer control exists, so the initial
  // add does not make L.Control.Layers re-render its labels (that would
  // undo the label surgery in _buildControl) and is not mistaken for a
  // toggle.
  function _wireOverlay(map, o) {
    map._mcOverlayWired = map._mcOverlayWired || {};
    if (map._mcOverlayWired[o.id]) return;
    map._mcOverlayWired[o.id] = true;
    map.on('overlayadd overlayremove', function (e) {
      if (!e.layer || e.layer !== map[o.layerProp]) return;
      _saveOverlayPref(o, e.type === 'overlayadd');
    });
    if (_readOverlayPref(o)) map.addLayer(map[o.layerProp]);
  }

  // The overlay whose layer a control input toggles (L.Control.Layers sets
  // input.layerId = L.stamp(layer)), or null.
  function _overlayOfInput(map, input) {
    if (!input) return null;
    for (var i = 0; i < OVERLAYS.length; i++) {
      var l = map[OVERLAYS[i].layerProp];
      if (l && input.layerId === L.stamp(l)) return OVERLAYS[i];
    }
    return null;
  }

  /* #734 M2 — at most one overlay of a group (the hillshade pair) on. When
   * `input` has just been checked, unchecks the other checked inputs of its
   * group; returns how many. It runs in the capture phase of the click,
   * before L.Control.Layers' own handler reads every input, so Leaflet then
   * removes the old layer and adds the new one in one pass, without a
   * re-render of the control. Unchecking checks nothing: both off is Off. */
  function _uncheckGroupSiblings(map, inputs, input) {
    if (!input || input.type !== 'checkbox' || !input.checked) return 0;
    var o = _overlayOfInput(map, input);
    if (!o || !o.group) return 0;
    var n = 0;
    for (var i = 0; i < inputs.length; i++) {
      var other = inputs[i];
      if (other === input || !other.checked) continue;
      var m = _overlayOfInput(map, other);
      if (m && m !== o && m.group === o.group) { other.checked = false; n++; }
    }
    return n;
  }

  function _wireGroupInputs(map, control) {
    var el = control.getContainer && control.getContainer();
    if (!el || !el.addEventListener) return;
    el.addEventListener('click', function (e) {
      var inputs = el.querySelectorAll('.leaflet-control-layers-overlays input');
      _uncheckGroupSiblings(map, inputs, e.target);
    }, true);
  }

  // Puts every offered overlay into `overlays` (label → layer) and drops
  // the layer of any the server no longer offers; layers survive control
  // rebuilds. Returns the offered ids in insertion order, which is the
  // order L.Control.Layers lists them in.
  function _collectOverlays(map, overlays) {
    var ids = [];
    OVERLAYS.forEach(function (o) {
      var cfg = o.config();
      if (cfg) {
        if (!map[o.layerProp]) map[o.layerProp] = _createOverlayLayer(map, o, cfg);
        else _syncOverlayZoom(map[o.layerProp], o, cfg);
        _wireOverlay(map, o);
        overlays[o.label(cfg)] = map[o.layerProp];
        ids.push(o.id);
      } else if (map[o.layerProp]) {
        try { map.removeLayer(map[o.layerProp]); } catch (_) {}
        map[o.layerProp] = null;
      }
    });
    return ids;
  }

  /* #734 — a kept tile layer takes a new minZoom (the customizer's contour
   * min-zoom) in place: the same layer, so the viewer's on/off stays, and
   * GridLayer's _resetView re-checks the zoom range of the current view. */
  function _syncOverlayZoom(layer, o, cfg) {
    if (o.create || !layer || !layer.options || layer.options.minZoom === cfg.minZoom) return;
    layer.options.minZoom = cfg.minZoom;
    if (layer._map && typeof layer._resetView === 'function') {
      try { layer._resetView(); } catch (_) {}
    }
  }

  // #859 — an overlay's zoom hint when its label carries none, else ''.
  function _overlayHint(id) {
    for (var i = 0; i < OVERLAYS.length; i++) {
      var o = OVERLAYS[i];
      if (o.id !== id || !o.hint) continue;
      var cfg = o.config();
      return cfg ? o.hint(cfg) : '';
    }
    return '';
  }

  // An overlay's own addition to its label in the control (the antenna
  // sites' status line), after every control build.
  function _decorateOverlay(map, id, labelEl) {
    for (var i = 0; i < OVERLAYS.length; i++) {
      var o = OVERLAYS[i];
      if (o.id !== id || !o.decorate) continue;
      var cfg = o.config();
      if (cfg) o.decorate(map, cfg, labelEl);
    }
  }

  /* #728 M3 — "Aerial photo" (#845; "Aerial (Denmark)" before): the
   * Danish spring orthophoto as a base map, from the server's same-origin proxy (cmd/server/dhm_proxy.go,
   * /api/map/dhm/ortho/{z}/{x}/{y}), which adds the token. Offered only
   * while /api/config/client carries dhm.ortho (map.dhm.ortho.enabled).
   *
   *   - One entry for both themes: it is a photo, so it is never inverted,
   *     and it lives outside the theme-typed REGISTRY (and so outside the
   *     customizer's light/dark lists and the Auto layer).
   *   - With the Esri opt-in (map.tiles.providers.esri.enabled) it is Esri
   *     World Imagery, the M1 style, under the ortho layer, so the map is
   *     never blank outside Denmark or below the ortho minZoom. Without it
   *     the ortho layer is alone and those areas show the map background.
   *   - The ortho layer draws in its own pane, mcOrthoPane (z 201): above
   *     the base tiles (tilePane, 200), so above the Esri member however
   *     L.Control.Layers renumbers the z-index of the layers in a pane, and
   *     below the hillshade (mcTerrainPane, 250). Being outside tilePane it
   *     is also outside the dark-theme invert filter.
   *   - The ortho layer is limited to dhm.ortho.bounds, and skips the tiles
   *     dhm.ortho.foreignMask marks (Skåne, Schleswig and Rügen inside that
   *     rectangle, internal/dhm/ortho_mask.go): no request goes out for
   *     them and the Esri imagery shows.
   *   - The pick is remembered like an overlay (#680): mc-ortho-base in
   *     localStorage and ?ortho=1 in the hash, which wins and is saved.
   *     Any other pick (Auto or a style) clears it. Other explicit picks
   *     are not remembered, as before.
   *   - Its credit is the block's own, through the #733 walker.
   */
  var ORTHO_ID    = 'dhm-ortho';
  var ORTHO_GROUP_LABEL = 'Aerial photo'; // #845: its row in the Denmark group
  var ORTHO_PREF  = { storageKey: 'mc-ortho-base', hashParam: 'ortho' };
  var ORTHO_PANE  = 'mcOrthoPane';
  var ORTHO_PANE_Z = 201;

  function getOrthoConfig() {
    var dhm = getTerrainConfig();
    var c = dhm && dhm.ortho;
    if (!c || !_isSameOriginPath(c.url)) return null;
    if (c.url.indexOf('{z}') < 0 || c.url.indexOf('{x}') < 0 || c.url.indexOf('{y}') < 0) return null;
    if (!_isZoom(c.minZoom) || !_isZoom(c.maxZoom) || c.minZoom > c.maxZoom) return null;
    if (!Array.isArray(c.bounds) || c.bounds.length !== 2) return null;
    if (typeof c.attribution !== 'string' || !c.attribution) return null;
    return c;
  }

  function _isTileIndex(v) { return typeof v === 'number' && Math.floor(v) === v && v >= 0; }

  // dhm.ortho.foreignMask as internal/dhm.TileMask sends it: zoom z, a
  // window of cols×rows tiles from (x0, y0), bit r*cols+c of the base64
  // `bits`, least significant bit first. Anything malformed is null: the
  // layer then relies on `bounds` alone, as without a mask.
  function decodeTileMask(m) {
    if (!m || typeof m !== 'object' || typeof m.bits !== 'string' || typeof atob !== 'function') return null;
    if (!_isZoom(m.z) || !_isTileIndex(m.x0) || !_isTileIndex(m.y0) || !_isTileIndex(m.cols) || !_isTileIndex(m.rows)) return null;
    if (!m.cols || !m.rows) return null;
    var bits;
    try { bits = atob(m.bits); } catch (_) { return null; }
    if (bits.length !== Math.ceil(m.cols * m.rows / 8)) return null;
    return { z: m.z, x0: m.x0, y0: m.y0, cols: m.cols, rows: m.rows, bits: bits };
  }

  function _maskBit(m, c, r) {
    if (c < 0 || r < 0 || c >= m.cols || r >= m.rows) return false;
    var i = r * m.cols + c;
    return ((m.bits.charCodeAt(i >> 3) >> (i & 7)) & 1) === 1;
  }

  // Whether the ortho tile z/x/y shows nothing the mask allows: at or above
  // the mask zoom, its mask tile is marked; below it, every mask tile under
  // it inside the window is marked (those outside the window are outside
  // the coverage), and at least one is. O(1) above the mask zoom; at most
  // 4^(mask zoom - z) bits below it, and the first unmarked one ends it.
  function orthoTileSkipped(m, z, x, y) {
    if (!m) return false;
    if (z >= m.z) {
      var s = Math.pow(2, z - m.z);
      return _maskBit(m, Math.floor(x / s) - m.x0, Math.floor(y / s) - m.y0);
    }
    var n = Math.pow(2, m.z - z), c0 = x * n - m.x0, r0 = y * n - m.y0, any = false;
    for (var r = Math.max(0, r0); r < Math.min(m.rows, r0 + n); r++) {
      for (var c = Math.max(0, c0); c < Math.min(m.cols, c0 + n); c++) {
        if (!_maskBit(m, c, r)) return false;
        any = true;
      }
    }
    return any;
  }

  function _createOrthoLayer(map, cfg) {
    var mask = decodeTileMask(cfg.foreignMask);
    if (!map.getPane(ORTHO_PANE)) {
      var pane = map.createPane(ORTHO_PANE);
      pane.style.zIndex = String(ORTHO_PANE_Z);
      pane.style.pointerEvents = 'none';
      pane.classList.add('mc-ortho-pane');
    }
    // #741 M1a — the ortho layer is a base map like any other, so it is
    // built by the one factory too. Its spec carries the URL only: the
    // credit is assembled here (buildCredit) and every other option is
    // this layer's own, so they travel as the call site's `opts`.
    var ortho = createBaseLayer({ url: cfg.url }, {
      pane: ORTHO_PANE,
      minZoom: cfg.minZoom,
      maxZoom: cfg.maxZoom,
      bounds: L.latLngBounds(cfg.bounds),
      attribution: buildCredit(cfg.attribution, creditLinks(cfg)),
      className: 'mc-ortho-tiles'
    });
    // Leaflet 1.9.4 (vendored, pinned) asks GridLayer._isValidTile for every
    // tile in range, and that is where `bounds` is applied; extending it is
    // the one way to keep a tile from being requested at all.
    var inBounds = ortho._isValidTile;
    ortho._isValidTile = function (coords) {
      return inBounds.call(this, coords) && !orthoTileSkipped(mask, coords.z, coords.x, coords.y);
    };
    retryPlaceholderTiles(map, ortho);
    var esri = _hasId('esri-imagery') ? REGISTRY['esri-imagery'] : null;
    if (!esri) return ortho;
    // The layer under it is a plain registry style built by the factory (#741):
    // it resolves the lazy url() and carries the style's own credit. maxNativeZoom
    // keeps Esri to the levels it has (19 here); above it Leaflet upscales z19
    // tiles, up to the ortho layer's maxZoom, instead of a blank map at z20
    // outside Denmark (#783).
    var under = createBaseLayer(esri, { maxNativeZoom: esri.maxZoom, maxZoom: cfg.maxZoom });
    return L.layerGroup([under, ortho]);
  }

  /* #741 M1b — the base map last picked in a layer control, for the page
   * session: every control (Map, Live, and the same page again after SPA
   * navigation) starts from it, and a rebuild restores it whatever the
   * theme. In memory only, so a reload starts from Auto; across reloads the
   * customizer's choice applies, as for every non-ortho style. Auto and
   * the Aerial entry (which has its own pref) clear it. */
  var _sessionBasePick = null;

  /* #845 — the layer control's model, pure so it can be tested alone.
   *
   *   - A style with both variants is listed once, as the current theme's.
   *     A style's counterpart in the other theme is the style of the same
   *     provider and label (osm-standard / osm-dark, esri-imagery /
   *     esri-imagery-dark, ...), which is also how a pick follows a theme
   *     switch. A style with no counterpart is always listed, as it is
   *     (#859: a light-only map in dark mode), so no map is ever hidden.
   *   - The Danish base maps (Dataforsyningen, and Aerial, which lives
   *     outside the registry) go into the "Denmark" group, as do all the
   *     overlays, which are all Danish data.
   *   - Inside the group, "(Denmark)" is dropped from a label and a zoom hint
   *     ("zoom 14+") is shown apart, as small right-aligned text.
   *   - With no saved open state, the group starts open while the map
   *     centre is over Denmark. */
  var DENMARK_BOUNDS = [[54.5, 8.0], [57.8, 15.2]]; // Jutland to Bornholm
  var DK_GROUP_PREF = 'mc-layers-denmark-open';

  function counterpartId(id, reg) {
    reg = reg || REGISTRY;
    var p = Object.prototype.hasOwnProperty.call(reg, id) ? reg[id] : null;
    if (!p) return null;
    for (var k in reg) {
      if (!Object.prototype.hasOwnProperty.call(reg, k)) continue;
      var q = reg[k];
      if (q.type !== p.type && q.provider === p.provider && q.label === p.label) return k;
    }
    return null;
  }

  function isDenmarkBase(id, reg) {
    reg = reg || REGISTRY;
    if (id === ORTHO_ID) return true;
    var p = Object.prototype.hasOwnProperty.call(reg, id) ? reg[id] : null;
    return !!p && p.provider === 'dataforsyningen';
  }

  // The base-map ids the control lists for one theme, in registry order:
  // { world: [...], denmark: [...] }. #859 — nothing is hidden: a style of
  // the other theme is left out only when its counterpart is listed; one
  // with no counterpart is listed as it is (a light-only map is offered,
  // and shown, as the light map in dark mode).
  function themeBaseIds(reg, isDark) {
    reg = reg || REGISTRY;
    var type = isDark ? 'dark' : 'light';
    var out = { world: [], denmark: [] };
    Object.keys(reg).forEach(function (id) {
      if (reg[id].type !== type && counterpartId(id, reg)) return;
      out[isDenmarkBase(id, reg) ? 'denmark' : 'world'].push(id);
    });
    return out;
  }

  // The pick a theme switch leads to: its counterpart, or itself.
  function pickForTheme(id, isDark, reg) {
    reg = reg || REGISTRY;
    if (!Object.prototype.hasOwnProperty.call(reg, id)) return id;
    if (reg[id].type === (isDark ? 'dark' : 'light')) return id;
    return counterpartId(id, reg) || id;
  }

  /* #873 — the base map deep link, #/map?base=<style id> (Live shares the
   * control, so #/live?base= too). resolveBase answers the style id a link
   * stands for in this theme, or null when there is none: not a string, or
   * an id that is not in the registry (unknown, or its provider is gated
   * off). A retired CARTO id is migrated (#845); a style of the other theme
   * becomes its counterpart (#845). */
  function resolveBase(value, isDark, reg) {
    reg = reg || REGISTRY;
    if (typeof value !== 'string') return null;
    var id = migrateTileId(value.trim());
    if (!Object.prototype.hasOwnProperty.call(reg, id)) return null;
    return pickForTheme(id, isDark, reg); // always one the control lists (#859)
  }

  /* Why a link is not shown as written, or null when it is: unknown,
   * gated off, a retired CARTO id, or a style of the other theme. */
  function rejectedBaseMessage(value, isDark, reg) {
    reg = reg || REGISTRY;
    var raw = String(value).trim();
    var head = '[map tiles] ?base=' + raw + ' ';
    var shown = resolveBase(raw, isDark, reg);
    if (shown === raw) return null;
    if (shown) {
      return head + (_hasId(raw) ? 'is a style of the other theme' : 'is a retired style') +
        '; showing "' + shown + '" instead.';
    }
    var style = Object.prototype.hasOwnProperty.call(BASE_STYLES, raw) ? BASE_STYLES[raw] : null;
    return head + 'ignored: ' + (style
      ? 'its provider is not enabled (' + (GATE_HINT[style.provider] || 'set map.tiles.providers.' + style.provider + '.enabled: true') + ')'
      : 'it is not a known tile style') + '. Using Auto instead.';
  }
  // One warning per rejected id and page (the same dedupe as the defaults).
  function _warnRejectedBase(value, msg) {
    var key = 'base=' + String(value).trim();
    msg = msg || rejectedBaseMessage(value, _isDark());
    if (!msg || _warnedDefaults[key]) return;
    _warnedDefaults[key] = true;
    try { if (typeof console !== 'undefined' && console.warn) console.warn(msg); } catch (_) {}
  }

  var BASE_PARAM = 'base';
  function _readBaseParam() {
    try { return _hashParts().params.get(BASE_PARAM); } catch (_) { return null; }
  }
  // Writes (or, with null, removes) base= in the hash. replaceState, so a
  // pick is no history entry and the SPA router does not re-run.
  function _writeBaseParam(id) {
    try {
      var h = _hashParts();
      if (id) h.params.set(BASE_PARAM, id); else h.params.delete(BASE_PARAM);
      _writeHash(h);
    } catch (_) {}
  }

  var ZOOM_HINT_RE = /zoom \d+\+/;
  function layerRowParts(text) {
    var name = String(text == null ? '' : text);
    var m = name.match(ZOOM_HINT_RE);
    var hint = m ? m[0] : '';
    if (m) name = name.replace(ZOOM_HINT_RE, '');
    name = name
      .replace(/\(([^()]*)\)/g, function (_, inner) {
        var kept = inner.split(',').map(function (t) { return t.trim(); })
          .filter(function (t) { return t && t !== 'Denmark' && t !== 'DHM'; });
        return kept.length ? '(' + kept.join(', ') + ')' : '';
      })
      .replace(/\s+/g, ' ').trim();
    return { name: name, hint: hint };
  }

  function inDenmark(c) {
    return !!c && typeof c.lat === 'number' && typeof c.lng === 'number' &&
      c.lat >= DENMARK_BOUNDS[0][0] && c.lat <= DENMARK_BOUNDS[1][0] &&
      c.lng >= DENMARK_BOUNDS[0][1] && c.lng <= DENMARK_BOUNDS[1][1];
  }

  // A saved '1' / '0' wins; with none, open while the centre is in Denmark.
  function denmarkOpenDefault(stored, centre) {
    if (stored === '1') return true;
    if (stored === '0') return false;
    return inDenmark(centre);
  }

  // ── Public surface ──────────────────────────────────────────────────────
  
  window.MC_DARK_TILE_DEFAULT           = DEFAULT_ID;
  window.MC_LIGHT_TILE_DEFAULT          = DEFAULT_ID_LIGHT;
  window.MC_TILE_PROVIDERS              = REGISTRY; // initial ref; MC_initTileRegistry keeps this in sync
  window.MC_setDarkTileProvider         = function(id) { return setActive(id, 'dark'); };
  window.MC_setLightTileProvider        = function(id) { return setActive(id, 'light'); };
  window.MC_getDarkTileProvider         = getActiveId;
  window.MC_getLightTileProvider        = getActiveLightId;
  window.MC_setServerDefaultTileProvider = setServerDefault;
  window.MC_setServerDefaultLightTileProvider = setServerDefaultLight;
  window.MC_applyTileFilter             = applyTileFilter;
  window.MC_getTileSpec                 = getTileSpec;
  // #741 M1a — every base map, on every page, is built here.
  window.MC_createBaseLayer             = createBaseLayer;
  window.MC_syncBaseLayer               = syncBaseLayer;
  window.MC_getTerrainConfig            = getTerrainConfig;
  window.MC_getHeritageConfig           = getHeritageConfig;
  window.MC_getMastsConfig              = getMastsConfig;
  // #723 M4 — the monuments settings rule (keys, defaults, bounds).
  window.MC_HERITAGE_SETTINGS           = {
    KEYS: HERITAGE_SETTING_KEYS, DEFAULTS: HERITAGE_DEFAULTS, LIMITS: HERITAGE_LIMITS,
    valid: validHeritageSetting, resolve: heritageSettings
  };
  window.MC_getOrthoConfig              = getOrthoConfig;
  window.MC_ORTHO_BASE_ID               = ORTHO_ID;
  window.MC_resolveOverlayOn            = resolveOverlayOn;
  window.MC_uncheckGroupSiblings        = _uncheckGroupSiblings;
  window.MC_forgetOverlayPref           = forgetOverlayPref;
  window.MC_forgetMapLayerPrefs         = forgetMapLayerPrefs;
  // #734 — the DHM overlay defaults' one rule, shared with the customizer.
  window.MC_DHMOverlays = {
    SETTING_KEYS: DHM_SETTING_KEYS,
    HILLSHADE_VALUES: DHM_HILLSHADE_VALUES,
    validSetting: validDHMSetting,
    settings: dhmOverlaySettings,
    contourZoomRange: dhmContourZoomRange,
    offered: offeredDHMOverlays,
    // #792
    ELEVATION_OPACITY_RANGE: DHM_ELEVATION_OPACITY_RANGE,
    elevationLegendLabels: elevationLegendLabels,
    buildElevationLegend: buildElevationLegend
  };
  window.MC_setLayerAttribution         = setLayerAttribution;
  window.MC_migrateTileId               = migrateTileId;
  window.MC_LayerGroups                 = {
    DENMARK_BOUNDS: DENMARK_BOUNDS, PREF_KEY: DK_GROUP_PREF,
    counterpartId: counterpartId, isDenmarkBase: isDenmarkBase, themeBaseIds: themeBaseIds,
    pickForTheme: pickForTheme, resolveBase: resolveBase, rowParts: layerRowParts, inDenmark: inDenmark,
    openDefault: denmarkOpenDefault
  };
  window.MC_CREDIT_COMPACT_CLASS        = CREDIT_COMPACT_CLASS;
  // #733 — exposed so a caller (and the unit tests) build an overlay credit
  // the one way, rather than assembling the links a second time.
  window.MC_buildCredit                 = buildCredit;
  window.MC_appendCredit                = appendCredit;
  window.MC_creditLinks                 = creditLinks;
  // #752 — every map without Leaflet's own attribution control needs one,
  // not just Live: the inset maps call this (compact: true) right after
  // they attach the viewer's provider.
  window.MC_ensureAttribution           = function (map, opts) {
    if (!map || typeof L === 'undefined') return null;
    return _ensureAttribution(map, !!(opts && opts.compact));
  };


  /**
   * Build and attach a Leaflet layer control listing "Auto (follows theme)"
   * at the top, then every enabled provider as an individual selectable base
   * layer. Selecting Auto delegates to the existing theme-synced tile group;
   * selecting a specific provider overrides it.
   *
   * @param {L.Map} map - The Leaflet map instance
   * @param {L.LayerGroup} autoLayerGroup - Existing group managed by _syncDarkTiles
   */
  window.MC_createLayerControl = function(map, autoLayerGroup, position) {
    if (typeof L === 'undefined') return null;
    
    // Set a default position just in case it isn't provided
    var controlPosition = position || 'topleft';

    var AUTO_KEY   = '__auto__';
    var AUTO_TITLE = "Follows the app's current light/dark theme";
    // #845 — the theme the list was built for, and the Denmark group's
    // open state (null until first read: saved, else the map centre).
    var _builtDark = _isDark();
    var _dkOpen    = null;
    var _control   = null;  // current L.control.layers instance
    var _layerMap  = {};    // provider-id → L.tileLayer
    var _isAuto    = !_sessionBasePick;  // true when "Auto" is the active selection
    // #728 M3 — the "Aerial (Denmark)" entry is the active base layer; the
    // viewer's stored or linked choice is read once, when it is first offered.
    var _orthoOn         = false;
    var _orthoPrefRead   = false;
    var _baseLinkRead    = false;  // #873 — the base= link is read once per control
    var _linkRaw         = null;   // #873 — the link not yet judged (config pending)
    var _linkWatching    = false;

    _ensureAttribution(map);

    // Restore the auto tile group and kick _syncDarkTiles
    function _activateAuto() {
      _isAuto = true;
      try { 
        var pane = map.getPane('tilePane');
        if (pane) pane.removeAttribute('data-explicit-layer');
      } catch (_) {}
      try { if (autoLayerGroup && !map.hasLayer(autoLayerGroup)) map.addLayer(autoLayerGroup); } catch (_) {}
      
      try {
        var ev = (typeof CustomEvent === 'function')
          ? new CustomEvent('mc-tile-provider-changed', { detail: { auto: true } })
          : { type: 'mc-tile-provider-changed', detail: { auto: true } };
        window.dispatchEvent(ev);
      } catch (_) {}
      
      if (typeof applyTileFilter === 'function') applyTileFilter();
    }

    // #845 — the Denmark group of one control: a real button with
    // aria-expanded, then "Base maps" and "Layers", each a container with
    // Leaflet's own list class, so one radio group and every selector of
    // the rows (data-tile-id, data-overlay-id) stay as they were.
    function _denmarkGroup(ctl) {
      if (ctl._mcDk) return ctl._mcDk;
      var doc = document;
      var group = doc.createElement('div');
      group.className = 'mc-layers-dk';
      group.setAttribute('data-layer-group', 'denmark');
      var btn = doc.createElement('button');
      btn.type = 'button';
      btn.className = 'mc-layers-dk-toggle';
      var body = doc.createElement('div');
      body.className = 'mc-layers-dk-body';
      body.id = 'mc-layers-dk-body-' + L.stamp(ctl);
      btn.setAttribute('aria-controls', body.id);
      btn.textContent = 'Denmark';
      var baseHead = doc.createElement('div');
      baseHead.className = 'mc-layers-dk-head';
      baseHead.textContent = 'Base maps';
      var base = doc.createElement('div');
      base.className = 'leaflet-control-layers-base mc-layers-dk-base';
      var layersHead = doc.createElement('div');
      layersHead.className = 'mc-layers-dk-head';
      layersHead.textContent = 'Layers';
      var over = doc.createElement('div');
      over.className = 'leaflet-control-layers-overlays mc-layers-dk-overlays';
      body.appendChild(baseHead); body.appendChild(base);
      body.appendChild(layersHead); body.appendChild(over);
      group.appendChild(btn); group.appendChild(body);
      btn.addEventListener('click', function () {
        _dkOpen = !_dkOpen;
        try { if (window.localStorage) window.localStorage.setItem(DK_GROUP_PREF, _dkOpen ? '1' : '0'); } catch (_) {}
        _showDenmark(ctl);
        if (window.MapChromeFit && window.MapChromeFit.fitList) window.MapChromeFit.fitList(map, ctl);
      });
      ctl._mcDk = { group: group, button: btn, body: body, baseHead: baseHead, base: base, layersHead: layersHead, overlays: over };
      return ctl._mcDk;
    }

    function _showDenmark(ctl) {
      var dk = ctl._mcDk;
      if (!dk) return;
      dk.button.setAttribute('aria-expanded', _dkOpen ? 'true' : 'false');
      dk.body.hidden = !_dkOpen;
    }

    function _emptyEl(el) { while (el.firstChild) el.removeChild(el.firstChild); }

    // The row text: a name, and (inside the group) a zoom hint apart.
    function _setRowText(span, text, grouped) {
      _emptyEl(span);
      var parts = grouped ? layerRowParts(text) : { name: text, hint: '' };
      var name = document.createElement('span');
      name.className = 'mc-layer-name';
      name.textContent = ' ' + parts.name;
      span.appendChild(name);
      if (parts.hint) {
        var hint = document.createElement('span');
        hint.className = 'mc-layer-hint';
        hint.textContent = parts.hint;
        span.appendChild(hint);
      }
    }

    // After every render of `ctl`: the rows' ids and names, then the Danish
    // rows into the Denmark group, then the overlays' own additions.
    function _layoutRows(ctl, overlayIds) {
      var c = ctl.getContainer && ctl.getContainer();
      if (!c) return;
      var real = !!(ctl._baseLayersList && ctl._baseLayersList.parentNode);
      var dk = real ? _denmarkGroup(ctl) : null;
      if (dk) { _emptyEl(dk.base); _emptyEl(dk.overlays); }

      var overlayLabels = [];
      try { overlayLabels = c.querySelectorAll('.leaflet-control-layers-overlays label'); } catch (_) {}
      for (var oi = 0; oi < overlayLabels.length && oi < overlayIds.length; oi++) {
        overlayLabels[oi].setAttribute('data-overlay-id', overlayIds[oi]);
      }
      if (!real) {
        for (var si = 0; si < overlayLabels.length && si < overlayIds.length; si++) {
          try { _decorateOverlay(map, overlayIds[si], overlayLabels[si]); } catch (_) {}
        }
        return;
      }

      var labels = ctl._baseLayersList.querySelectorAll('label');
      for (var i = 0; i < labels.length; i++) {
        var labelEl = labels[i];
        var spans = labelEl.querySelectorAll('span');
        if (spans.length === 0) continue;
        // The last span holds the name; Leaflet's radio input stays.
        var span = spans[spans.length - 1];
        var id = span.textContent.trim();
        labelEl.setAttribute('data-tile-id', id);
        var grouped = isDenmarkBase(id);
        if (id === AUTO_KEY) {
          _setRowText(span, 'Auto', false);
          span.title = AUTO_TITLE;
        } else if (id === ORTHO_ID) {
          _setRowText(span, ORTHO_GROUP_LABEL, true);
        } else if (REGISTRY[id]) {
          _setRowText(span, REGISTRY[id].label || id, grouped);
        }
        if (grouped) dk.base.appendChild(labelEl);
      }
      for (var j = 0; j < overlayLabels.length; j++) {
        var os = overlayLabels[j].querySelectorAll('span');
        if (os.length) {
          var t = os[os.length - 1];
          var text = t.textContent.trim();
          var extra = j < overlayIds.length && !ZOOM_HINT_RE.test(text) ? _overlayHint(overlayIds[j]) : '';
          _setRowText(t, extra ? text + ' ' + extra : text, true);
        }
        dk.overlays.appendChild(overlayLabels[j]);
      }
      for (var k = 0; k < overlayLabels.length && k < overlayIds.length; k++) {
        try { _decorateOverlay(map, overlayIds[k], overlayLabels[k]); } catch (_) {}
      }

      var hasBase = !!dk.base.firstChild, hasOver = !!dk.overlays.firstChild;
      dk.baseHead.hidden = dk.base.hidden = !hasBase;
      dk.layersHead.hidden = dk.overlays.hidden = !hasOver;
      // An empty Denmark group is not shown at all.
      dk.group.hidden = !hasBase && !hasOver;
      if (dk.group.parentNode !== ctl._baseLayersList.parentNode) {
        ctl._baseLayersList.parentNode.insertBefore(dk.group, ctl._baseLayersList.nextSibling);
      }
      // Leaflet's own overlay list is empty now; so is its separator's use.
      if (ctl._separator) ctl._separator.style.display = 'none';
      _showDenmark(ctl);
    }

    function _buildControl() {
      // Tear down existing control + explicit tile layers
      if (_control) { try { _control.remove(); } catch (_) {} _control = null; }
      Object.keys(_layerMap).forEach(function (id) {
        try { map.removeLayer(_layerMap[id]); } catch (_) {}
      });
      _layerMap = {};
      // Remove stale baselayerchange listeners by cloning off event (Leaflet re-adds on new control)
      if (map._mcBaselayerchangeHandler) {
        try { map.off('baselayerchange', map._mcBaselayerchangeHandler); } catch (_) {}
      }

      // #845 — this theme's variant of each style (#859: a style with none
      // in it as its other one), the Danish ones in the Denmark group.
      _builtDark = _isDark();
      var listed = themeBaseIds(REGISTRY, _builtDark);

      function _makeLayer(id) {
        var p   = REGISTRY[id];
        // #741 M1a — through the shared factory, so a WMS style is a
        // selectable base map here too. The options stay the ones this
        // control always built.
        var layer = createBaseLayer(p, { attribution: p.attribution || '', maxZoom: p.maxZoom || 19 });
        
        // Every explicit layer enforces its own filter and locks the pane
        layer.on('add', function () {
          var pane = map.getPane('tilePane');
          if (pane) {
            pane.setAttribute('data-explicit-layer', 'true');
            pane.style.filter = p.invertFilter || ''; // Clears it if null!
          }
        });

        _layerMap[id] = layer;
        return layer;
      }

      // "Auto" entry is backed by the theme-synced autoLayerGroup
      var baseMaps = {};
      baseMaps[AUTO_KEY] = autoLayerGroup;

      listed.world.forEach(function (id) { baseMaps[id] = _makeLayer(id); });
      listed.denmark.forEach(function (id) { baseMaps[id] = _makeLayer(id); });

      // #873 — a base= link, read once per control, wins over what is
      // stored. Until /api/config/client has fed the registry it is only
      // kept (hash and all): the style it names may be one the config is
      // about to enable. It is judged on the config's rebuild, or when the
      // request has failed; a value that is then not a listed style is
      // dropped from the hash, with a warning.
      var link = null;
      if (!_baseLinkRead) {
        _baseLinkRead = true;
        _linkRaw = _readBaseParam();
      }
      if (_linkRaw != null) {
        var cfgReady = window.MeshConfigReady;
        if (_configSettled || !(cfgReady && typeof cfgReady.then === 'function')) {
          link = { raw: _linkRaw, id: _linkRaw.trim() === ORTHO_ID ? null : resolveBase(_linkRaw, _isDark()) };
          _linkRaw = null;
        } else if (!_linkWatching) {
          _linkWatching = true;
          var settle = function () {
            _configSettled = true;
            if (!map._mcGone && _linkRaw != null) _buildControl();
          };
          try { cfgReady.then(settle, settle); } catch (_) { settle(); }
        }
      }

      // #728 M3 — one entry for both themes, last in the Denmark group.
      var orthoCfg = getOrthoConfig();
      if (orthoCfg) {
        var orthoLayer = _createOrthoLayer(map, orthoCfg);
        orthoLayer.on('add', function () {
          var pane = map.getPane('tilePane');
          if (pane) {
            pane.setAttribute('data-explicit-layer', 'true');
            pane.style.filter = '';
          }
        });
        _layerMap[ORTHO_ID] = baseMaps[ORTHO_ID] = orthoLayer;
        if (!_orthoPrefRead) {
          _orthoPrefRead = true;
          if (_readOverlayPref(ORTHO_PREF)) { _orthoOn = true; _isAuto = false; }
        }
      } else if (_orthoOn) {
        _orthoOn = false; // no longer offered: back to Auto, the stored choice kept
        _isAuto = true;
      }

      // The judged link wins over a stored Aerial pick, whenever it is judged.
      if (link) {
        var linkOrtho = link.raw.trim() === ORTHO_ID;
        if (linkOrtho && _layerMap[ORTHO_ID]) {
          _orthoOn = true; _isAuto = false; _sessionBasePick = null;
          _saveOverlayPref(ORTHO_PREF, true);
          _writeBaseParam(null); // the Aerial map rides on its own parameter
        } else if (!linkOrtho && link.id && _layerMap[link.id]) {
          if (_orthoOn) { _orthoOn = false; _saveOverlayPref(ORTHO_PREF, false); }
          _sessionBasePick = link.id; _isAuto = false;
          _writeBaseParam(link.id);
          _warnRejectedBase(link.raw); // only when shown as another map
        } else {
          // An empty base= is a typo, not a rejected style: no warning.
          if (link.raw.trim() !== '') {
            _warnRejectedBase(link.raw, linkOrtho
              ? '[map tiles] ?base=' + ORTHO_ID + ' ignored: the Aerial map is not offered here. Using Auto instead.' : null);
          }
          _writeBaseParam(null); // unknown, gated or unlisted: Auto, a clean link
          // Auto for this open: a stored Aerial pick is not restored (and
          // not deleted, so the next open without a link brings it back).
          _orthoOn = false; _isAuto = true; _sessionBasePick = null;
        }
      }

      // #680/#723 — optional overlays; null keeps the overlay section out.
      var overlays = {};
      var overlayIds = _collectOverlays(map, overlays);
      if (!overlayIds.length) overlays = null;

      // A restored Aerial pick, or (#741) the session's explicit pick, goes
      // on the map before the control exists, as the overlays do
      // (_wireOverlay), so its add is not taken for a toggle.
      var restored = _orthoOn ? _layerMap[ORTHO_ID]
        : (!_isAuto && _sessionBasePick && _layerMap[_sessionBasePick]) || null;
      if (restored) {
        if (!link && _linkRaw == null && _sessionBasePick && !_orthoOn) _writeBaseParam(_sessionBasePick); // #873
        map.addLayer(restored);
        try { if (autoLayerGroup && map.hasLayer(autoLayerGroup)) map.removeLayer(autoLayerGroup); } catch (_) {}
      }

      if (_dkOpen === null) {
        var stored = null, centre = null;
        try { stored = window.localStorage && window.localStorage.getItem(DK_GROUP_PREF); } catch (_) {}
        try { centre = map.getCenter ? map.getCenter() : null; } catch (_) {}
        _dkOpen = denmarkOpenDefault(stored, centre);
      }

      // Use the dynamic controlPosition variable instead of a hardcoded string
      var ctl = L.control.layers(baseMaps, overlays, { position: controlPosition });
      // #845 — L.Control.Layers re-renders every row whenever a listed
      // layer is added or removed (_update), so the labels and the Denmark
      // group are laid out after each render, not once.
      var leafletUpdate = ctl._update;
      if (typeof leafletUpdate === 'function') {
        ctl._update = function () {
          var r = leafletUpdate.apply(this, arguments);
          _layoutRows(this, overlayIds);
          return r;
        };
      }
      _control = ctl.addTo(map);
      if (typeof leafletUpdate !== 'function') _layoutRows(_control, overlayIds);
      // #839 — the list ends above the attribution and the bottom nav.
      if (window.MapChromeFit) window.MapChromeFit.fitLayerControl(map, _control);
      if (overlayIds.length) {
        try { _wireGroupInputs(map, _control); } catch (_) {}
      }

      // Decide initial active layer: a restored pick is already on the map,
      // above; otherwise Auto (also the fallback for a pick that is no
      // longer offered).
      if (!restored) _activateAuto();

      map._mcBaselayerchangeHandler = function (e) {
        _linkRaw = null; // #873 — a pick made while config is pending beats the link
        if (e.name === ORTHO_ID) {
          _isAuto = false;
          _sessionBasePick = null;
          _writeBaseParam(null);
          if (!_orthoOn) { _orthoOn = true; _saveOverlayPref(ORTHO_PREF, true); }
          try { if (autoLayerGroup && map.hasLayer(autoLayerGroup)) map.removeLayer(autoLayerGroup); } catch (_) {}
          return;
        }
        if (_orthoOn) { _orthoOn = false; _saveOverlayPref(ORTHO_PREF, false); }

        if (e.name === AUTO_KEY) {
          _sessionBasePick = null;
          _writeBaseParam(null);
          _activateAuto();
          return;
        }

        // Explicit provider selected — the name parameter is exactly our registry ID
        var selectedId = e.name;
        if (!REGISTRY[selectedId]) return;

        _isAuto = false;
        try { 
          var pane = map.getPane('tilePane');
          // Contract: mark the pane as explicit so applyTileFilter skips auto-theme CSS filters
          if (pane) pane.setAttribute('data-explicit-layer', 'true');
        } catch (_) {}

        // Hide autoLayerGroup while an explicit layer is active
        try { if (autoLayerGroup && map.hasLayer(autoLayerGroup)) map.removeLayer(autoLayerGroup); } catch (_) {}

        _sessionBasePick = selectedId;
        _writeBaseParam(selectedId);

        // CSS invert filter is handled by the layer's own add/remove events above
      };

      // Now attach the correctly assigned handler
      map.on('baselayerchange', map._mcBaselayerchangeHandler);

      return _control;
    }

    _buildControl();

    // Re-build when server config arrives async so newly-enabled providers appear
    var onConfigProviders = function (e) {
      if (!map._mcGone && e && e.detail && e.detail.fromConfig) _buildControl();
    };
    window.addEventListener('mc-tile-provider-changed', onConfigProviders);

    // #734 — the customizer moved a DHM overlay default: rebuild, so the
    // contour label and minZoom follow at once. The on/off defaults apply
    // to the next map; layers and choices survive the rebuild. A removed
    // map stops listening — to both events (#839: the config one used to
    // stay, one per Live/Map visit).
    var onDHMDefaults = function () { if (!map._mcGone) _buildControl(); };
    window.addEventListener('dhm-defaults-changed', onDHMDefaults);
    // #859 — the customizer moved the antenna sites' zoom gate: the rows
    // are laid out again, so the hint follows (no rebuild needed).
    var onMastsDefaults = function () {
      if (!map._mcGone && _control && _control._map && typeof _control._update === 'function') _control._update();
    };
    window.addEventListener('masts-defaults-changed', onMastsDefaults);
    // #845 — the list follows the theme: rebuilt when it flips (never per
    // map move), an explicit pick moved to its counterpart in the new one.
    var onTheme = function () {
      if (map._mcGone || _isDark() === _builtDark) return;
      if (!_isAuto && _sessionBasePick) _sessionBasePick = pickForTheme(_sessionBasePick, _isDark());
      _buildControl();
    };
    var themeObs = null, themeMq = null;
    try {
      if (typeof MutationObserver === 'function' && document.documentElement) {
        themeObs = new MutationObserver(onTheme);
        themeObs.observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] });
      }
    } catch (_) { themeObs = null; }
    try {
      themeMq = window.matchMedia ? window.matchMedia('(prefers-color-scheme: dark)') : null;
      if (themeMq && themeMq.addEventListener) themeMq.addEventListener('change', onTheme);
    } catch (_) { themeMq = null; }
    map.on('unload', function () {
      map._mcGone = true;
      try { if (themeObs) themeObs.disconnect(); } catch (_) {}
      try { if (themeMq && themeMq.removeEventListener) themeMq.removeEventListener('change', onTheme); } catch (_) {}
      try {
        if (window.removeEventListener) {
          window.removeEventListener('dhm-defaults-changed', onDHMDefaults);
          window.removeEventListener('masts-defaults-changed', onMastsDefaults);
          window.removeEventListener('mc-tile-provider-changed', onConfigProviders);
        }
      } catch (_) {}
    });

    // When the site theme flips, update the Auto tile via the existing _syncDarkTiles
    // path (map.js/live.js listen for mc-tile-provider-changed and call _syncDarkTiles).
    // Nothing extra needed here — autoLayerGroup is managed externally.

    return _control;
  };


  // ── Cross-tab sync ──────────────────────────────────────────────────────
  // If another tab in the same browser changes the provider, mirror the
  // dispatch + filter-apply here so live map.js / live.js swap tiles too.
  try {
    window.addEventListener('storage', function (e) {
      if (!e || (e.key !== STORAGE_KEY && e.key !== STORAGE_KEY_LIGHT)) return;
      if (!_hasId(e.newValue)) return;
      var detail = { id: e.newValue, provider: REGISTRY[e.newValue], crossTab: true };
      try {
        var ev = (typeof CustomEvent === 'function')
          ? new CustomEvent(EVENT_NAME, { detail: detail })
          : { type: EVENT_NAME, detail: detail };
        window.dispatchEvent(ev);
      } catch (_) { /* dispatch optional */ }
      applyTileFilter();
    });
  } catch (_) { /* addEventListener may not exist in some envs */ }
})();
