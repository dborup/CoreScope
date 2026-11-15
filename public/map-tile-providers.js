/* map-tile-providers.js — Registry of map tile providers & runtime switcher (#1165/#1420).
 *
 * Scope:
 *   - Multiple providers: OSM (default), Carto, Stamen, OpenTopoMap, USGS,
 *     Esri. OpenTopoMap, USGS and Esri are opt-in (map.tiles.providers.*.enabled).
 *   - MC_setDarkTileProvider(id) / MC_setLightTileProvider(id) persist per-browser
 *     to localStorage and dispatch `mc-tile-provider-changed`.
 *   - Resolves localStorage → server default → 'osm-dark' / 'osm-standard' (#332).
 *   - MC_applyTileFilter() applies/clears the CSS filter on
 *     `.leaflet-tile-pane` based on current theme + selected provider.
 *   - MC_createLayerControl() builds a Leaflet control mapping providers,
 *     plus the optional DHM terrain hillshade (#680), protected ancient
 *     monuments (#723) and Mastedatabasen antenna site (#724) overlays.
 *
 * No new deps — URL-only providers.
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
    'esri-imagery-dark': { provider: 'esri', label: 'Esri World Imagery', url: function() { return ESRI_IMAGERY_URL; }, invertFilter: null, type: 'dark', attribution: ESRI_IMAGERY_ATTRIBUTION, maxZoom: 19 }
  };

  var REGISTRY = {};

  function _hasId(id) {
    return typeof id === 'string' && Object.prototype.hasOwnProperty.call(REGISTRY, id);
  }

  /* #728 — a configured default whose provider is not enabled is skipped
   * (the resolvers fall through to the legacy URL or the built-in
   * fallback), so the map still works. Say so once per page, naming the
   * switch, so an operator who upgraded past the Esri opt-in can find it. */
  var _warnedDefaults = {};
  function _warnUnavailableDefault(key, id) {
    if (typeof id !== 'string' || !id || _hasId(id)) return;
    if (_warnedDefaults[key + '=' + id]) return;
    _warnedDefaults[key + '=' + id] = true;
    var style = Object.prototype.hasOwnProperty.call(BASE_STYLES, id) ? BASE_STYLES[id] : null;
    var why = style
      ? 'its provider is not enabled (set map.tiles.providers.' + style.provider + '.enabled: true)'
      : 'it is not a known tile style';
    try {
      if (typeof console !== 'undefined' && console.warn) {
        console.warn('[map tiles] map.tiles.' + key + ' "' + id + '" is ignored: ' + why + '. Using the fallback base map instead.');
      }
    } catch (_) {}
  }

  window.MC_initTileRegistry = function(fromAsync) {
    _cfg = (typeof window !== 'undefined' && window.MC_MAP_CFG && window.MC_MAP_CFG.tiles) ? window.MC_MAP_CFG.tiles : null;

    var HAS_CARTO = !_cfg || !_cfg.providers || !_cfg.providers.carto || _cfg.providers.carto.enabled !== false;
    var HAS_OSM = _cfg && _cfg.providers && _cfg.providers.osm && _cfg.providers.osm.enabled;
    var HAS_OPENTOPOMAP = _cfg && _cfg.providers && _cfg.providers.opentopomap && _cfg.providers.opentopomap.enabled;
    var HAS_USGS = _cfg && _cfg.providers && _cfg.providers.usgs && _cfg.providers.usgs.enabled;
    var HAS_STAMEN = _cfg && _cfg.providers && _cfg.providers.stamen && _cfg.providers.stamen.enabled && !!_cfg.providers.stamen.token;
    // #728 — opt-in like OpenTopoMap and USGS. The public Esri endpoint is
    // licensed under the Esri Master License Agreement, so an operator has
    // to switch it on deliberately; only the JSON boolean true counts.
    var HAS_ESRI = !!(_cfg && _cfg.providers && _cfg.providers.esri && _cfg.providers.esri.enabled === true);

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

    if (_cfg && _cfg.darkDefault && _hasId(_cfg.darkDefault)) _serverDefault = _cfg.darkDefault;
    if (_cfg && _cfg.lightDefault && _hasId(_cfg.lightDefault)) _serverDefaultLight = _cfg.lightDefault;
    _warnUnavailableDefault('darkDefault', _cfg && _cfg.darkDefault);
    _warnUnavailableDefault('lightDefault', _cfg && _cfg.lightDefault);

    // When called after async config loads, notify maps to re-sync tiles
    if (fromAsync) {
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

  
  // #390 — mirror of getSelectedLightId(): a valid explicit choice can
  // itself be the built-in fallback id, so an implicit fallback has to stay
  // distinguishable from one the operator or user actually picked.
  function getSelectedDarkId() {
    try {
      var stored = window.localStorage && window.localStorage.getItem(STORAGE_KEY);
      if (_hasId(stored) && REGISTRY[stored].type === 'dark') return stored;
    } catch (_) {}
    if (_cfg && _cfg.darkDefault && _hasId(_cfg.darkDefault)) return _cfg.darkDefault;
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
      var stored = window.localStorage && window.localStorage.getItem(STORAGE_KEY_LIGHT);
      if (_hasId(stored) && REGISTRY[stored].type === 'light') return stored;
    } catch (_) {}
    if (_cfg && _cfg.lightDefault && _hasId(_cfg.lightDefault)) return _cfg.lightDefault;
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
   *   { id, url, attribution, refUrl, maxZoom, invertFilter }
   *
   * `url` is always a string template (or null if a style is malformed) and
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
    return {
      id: id,
      url: (typeof u === 'function' ? u() : u) || null,
      attribution: (p && p.attribution) || BASE_STYLES[fallback].attribution,
      refUrl: (p && p.refUrl) || null,
      maxZoom: (p && p.maxZoom) || 19,
      invertFilter: (p && p.invertFilter) || null
    };
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
   *     defaultOn, whose "off" is then written to the hash as =0.
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

  function getHeritageConfig() {
    var c = window.MC_HERITAGE_CFG;
    if (!c || c.enabled !== true || typeof c.url !== 'string' || c.url.indexOf('https://') !== 0) return null;
    if (typeof c.layers !== 'string' || !c.layers || !_isZoom(c.minZoom) || !_isZoom(c.maxZoom)) return null;
    if (!Array.isArray(c.bounds) || c.bounds.length !== 2) return null;
    return c;
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

  // #723 — opacity (and whether an overlay starts on) is hard-coded here;
  // exposing them in the customizer is tracked as #723 M4 (and #724 M5 for
  // the antenna sites).
  var OVERLAYS = [
    {
      id: TERRAIN_ID, storageKey: 'mc-terrain-dhm', hashParam: 'terrain', layerProp: '_mcTerrainLayer',
      pane: 'mcTerrainPane', paneZ: 250, paneClass: 'mc-terrain-pane', className: 'mc-terrain-tiles', opacity: 0.65,
      retryPlaceholders: true, // #728 N3: a DHM proxy tile
      config: getTerrainConfig,
      label: function () { return TERRAIN_LABEL; },
      layers: function (cfg) { return cfg.layer; }
    },
    {
      // Protection-zone polygons under the monument points. The WMS draws
      // nothing further out than minZoom, so the label says where it starts.
      id: HERITAGE_ID, storageKey: 'mc-heritage', hashParam: 'heritage', layerProp: '_mcHeritageLayer',
      pane: 'mcHeritagePane', paneZ: 260, paneClass: 'mc-heritage-pane', className: 'mc-heritage-tiles', opacity: 0.85,
      config: getHeritageConfig,
      label: function (cfg) { return 'Protected ancient monuments (zoom ' + cfg.minZoom + '+)'; },
      layers: function (cfg) { return cfg.layers; }
    },
    {
      // #724 option A: every site, honestly labelled, on when offered.
      id: 'masts-sites', storageKey: 'mc-masts', hashParam: 'masts', layerProp: '_mcMastsSitesLayer', defaultOn: true,
      config: getMastsConfig,
      label: function () { return window.MastsLayer.SITE_LABEL; },
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

  function resolveOverlayOn(hashValue, stored, defaultOn) {
    if (hashValue === '1') return true;
    if (hashValue === '0') return false;
    if (stored === '1' || stored === '0') return stored === '1';
    return !!defaultOn;
  }

  function _hashParts() {
    var hash = (typeof location !== 'undefined' && location.hash) || '';
    var qi = hash.indexOf('?');
    return { base: qi >= 0 ? hash.slice(0, qi) : hash, params: new URLSearchParams(qi >= 0 ? hash.slice(qi + 1) : '') };
  }

  function _readOverlayPref(o) {
    var hv = null, stored = null;
    try { hv = _hashParts().params.get(o.hashParam); } catch (_) {}
    try { stored = window.localStorage && window.localStorage.getItem(o.storageKey); } catch (_) {}
    var on = resolveOverlayOn(hv, stored, o.defaultOn);
    if (hv === '1' || hv === '0') {
      try { if (window.localStorage) window.localStorage.setItem(o.storageKey, on ? '1' : '0'); } catch (_) {}
    }
    return on;
  }

  // replaceState, so the SPA router does not re-run on the hash rewrite.
  function _saveOverlayPref(o, on) {
    try { if (window.localStorage) window.localStorage.setItem(o.storageKey, on ? '1' : '0'); } catch (_) {}
    try {
      var h = _hashParts();
      if (on) h.params.set(o.hashParam, '1');
      else if (o.defaultOn) h.params.set(o.hashParam, '0');
      else h.params.delete(o.hashParam);
      var qs = h.params.toString();
      // An empty hash gets a bare '#', so the parameter never lands in the
      // query string (#682 review F4).
      history.replaceState(history.state, '', location.pathname + location.search + (qs ? (h.base || '#') + '?' + qs : h.base));
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
  function buildCredit(text, links) {
    var plain = typeof text === 'string' ? text : '';
    if (!plain || typeof document === 'undefined' || !document.createElement) return plain;
    var box;
    try { box = document.createElement('div'); } catch (_) { return plain; }
    return appendCredit(box, plain, links) ? box.innerHTML : plain;
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

  function retryPlaceholderTiles(map, layer) {
    var pending = [];
    layer.on('tileload', function (e) {
      var img = e.tile, coords = e.coords;
      if (!img || img._mcRetried || img.naturalWidth !== 1 || img.naturalHeight !== 1) return;
      img._mcRetried = true;
      var t = setTimeout(function () {
        var i = pending.indexOf(t);
        if (i >= 0) pending.splice(i, 1);
        if (img._mcGone || !_tileInView(map, layer, coords)) return;
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
    var layer = L.tileLayer.wms(cfg.url, {
      layers: o.layers(cfg),
      styles: '',
      format: 'image/png',
      transparent: true,
      version: '1.3.0',
      crs: L.CRS.EPSG3857,
      tileSize: 256,
      opacity: o.opacity,
      pane: o.pane,
      minZoom: cfg.minZoom,
      maxZoom: cfg.maxZoom,
      bounds: L.latLngBounds(cfg.bounds),
      attribution: _overlayCredit(cfg),
      className: o.className
    });
    return o.retryPlaceholders ? retryPlaceholderTiles(map, layer) : layer;
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

  /* #728 M3 — "Aerial (Denmark)": the Danish spring orthophoto as a base
   * map, from the server's same-origin proxy (cmd/server/dhm_proxy.go,
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
  var ORTHO_LABEL = 'Aerial (Denmark)';
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
    var ortho = L.tileLayer(cfg.url, {
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
    var under = L.tileLayer(esri.url(), { attribution: esri.attribution, maxZoom: esri.maxZoom });
    return L.layerGroup([under, ortho]);
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
  window.MC_getTerrainConfig            = getTerrainConfig;
  window.MC_getHeritageConfig           = getHeritageConfig;
  window.MC_getMastsConfig              = getMastsConfig;
  window.MC_getOrthoConfig              = getOrthoConfig;
  window.MC_ORTHO_BASE_ID               = ORTHO_ID;
  window.MC_resolveOverlayOn            = resolveOverlayOn;
  window.MC_setLayerAttribution         = setLayerAttribution;
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
    var AUTO_LABEL = ' <span title="Follows the app\'s current light/dark theme">Auto</span>';
    var _control   = null;  // current L.control.layers instance
    var _layerMap  = {};    // provider-id → L.tileLayer
    var _isAuto    = true;  // true when "Auto" is the active selection
    var _overrideLightId = null;
    var _overrideDarkId  = null;
    // #728 M3 — the "Aerial (Denmark)" entry is the active base layer; the
    // viewer's stored or linked choice is read once, when it is first offered.
    var _orthoOn         = false;
    var _orthoPrefRead   = false;

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

      var isDark       = _isDarkEffective();
      var activeDarkId  = _overrideDarkId || getActiveId();
      var activeLightId = _overrideLightId || getActiveLightId();

      // Light providers first, then dark — natural grouping in the UI
      var lightIds = Object.keys(REGISTRY).filter(function (id) { return REGISTRY[id].type === 'light'; });
      var darkIds  = Object.keys(REGISTRY).filter(function (id) { return REGISTRY[id].type === 'dark';  });

      function _makeLayer(id) {
        var p   = REGISTRY[id];
        var url = typeof p.url === 'function' ? p.url() : p.url;
        var layer = L.tileLayer(url, { attribution: p.attribution || '', maxZoom: p.maxZoom || 19 });
        
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

      lightIds.forEach(function (id) { baseMaps[id] = _makeLayer(id); });
      darkIds.forEach(function  (id) { baseMaps[id] = _makeLayer(id); });

      // #728 M3 — listed last, under its own separator, on either theme.
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

      // #680/#723 — optional overlays; null keeps the overlay section out.
      var overlays = {};
      var overlayIds = _collectOverlays(map, overlays);
      if (!overlayIds.length) overlays = null;

      // A restored Aerial pick goes on the map before the control exists, as
      // the overlays do (_wireOverlay): added later, it would make
      // L.Control.Layers re-render its labels and undo the surgery below.
      if (_orthoOn) {
        map.addLayer(_layerMap[ORTHO_ID]);
        try { if (autoLayerGroup && map.hasLayer(autoLayerGroup)) map.removeLayer(autoLayerGroup); } catch (_) {}
      }

      // Use the dynamic controlPosition variable instead of a hardcoded string
      _control = L.control.layers(baseMaps, overlays, { position: controlPosition }).addTo(map);

      if (overlayIds.length) {
        try {
          var overlayLabels = _control.getContainer().querySelectorAll('.leaflet-control-layers-overlays label');
          for (var oi = 0; oi < overlayLabels.length && oi < overlayIds.length; oi++) {
            overlayLabels[oi].setAttribute('data-overlay-id', overlayIds[oi]);
            try { _decorateOverlay(map, overlayIds[oi], overlayLabels[oi]); } catch (_) {}
          }
        } catch (_) {}
      }

      // DOM surgery: map IDs to human-readable labels, inject separator, set data attributes.
      // We pass raw IDs to Leaflet as keys so e.name is the exact ID without label parsing.
      try {
        var firstDarkId = darkIds.length > 0 ? darkIds[0] : null;
        var labels = _control.getContainer().querySelectorAll('.leaflet-control-layers-base label');
        for (var i = 0; i < labels.length; i++) {
          var labelEl = labels[i];
          var spans = labelEl.querySelectorAll('span');
          if (spans.length === 0) continue;

          // Target the last span to avoid nuking Leaflet's radio <input>
          var span = spans[spans.length - 1]; 

          var id = span.textContent.trim();
          labelEl.setAttribute('data-tile-id', id);
          
          if (id === AUTO_KEY) {
            span.innerHTML = AUTO_LABEL;
          } else if (id === ORTHO_ID) {
            span.textContent = ' ' + ORTHO_LABEL;
          } else if (REGISTRY[id]) {
            span.innerHTML = ' ' + (REGISTRY[id].label || id);
          }
          
          if ((id === firstDarkId && lightIds.length > 0) || id === ORTHO_ID) {
            var sep = document.createElement('div');
            sep.className = 'leaflet-control-layers-separator';
            labelEl.parentNode.insertBefore(sep, labelEl);
          }
        }
      } catch (_) {}

      // Decide initial active layer
      if (_orthoOn) {
        // Already on the map, above.
      } else if (_isAuto) {
        // Ensure autoLayerGroup is on the map; explicit layers are off
        _activateAuto();
      } else {
        // Re-select whichever explicit provider was active before rebuild
        var prevId = isDark ? activeDarkId : activeLightId;
        var prevLayer = _layerMap[prevId];
        if (prevLayer) {
          map.addLayer(prevLayer);
          try { if (autoLayerGroup && map.hasLayer(autoLayerGroup)) map.removeLayer(autoLayerGroup); } catch (_) {}
        } else {
          _activateAuto(); // fallback
        }
      }

      map._mcBaselayerchangeHandler = function (e) {
        if (e.name === ORTHO_ID) {
          _isAuto = false;
          if (!_orthoOn) { _orthoOn = true; _saveOverlayPref(ORTHO_PREF, true); }
          try { if (autoLayerGroup && map.hasLayer(autoLayerGroup)) map.removeLayer(autoLayerGroup); } catch (_) {}
          return;
        }
        if (_orthoOn) { _orthoOn = false; _saveOverlayPref(ORTHO_PREF, false); }

        if (e.name === AUTO_KEY) {
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

        var p = REGISTRY[selectedId];
        if (p.type === 'light') _overrideLightId = selectedId;
        else                    _overrideDarkId  = selectedId;

        // CSS invert filter is handled by the layer's own add/remove events above
      };

      // Now attach the correctly assigned handler
      map.on('baselayerchange', map._mcBaselayerchangeHandler);

      return _control;
    }

    _buildControl();

    // Re-build when server config arrives async so newly-enabled providers appear
    window.addEventListener('mc-tile-provider-changed', function (e) {
      if (e && e.detail && e.detail.fromConfig) _buildControl();
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
