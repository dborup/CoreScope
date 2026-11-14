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
 *     plus the optional DHM terrain hillshade overlay (#680).
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

  /* #680 — optional DHM terrain hillshade overlay (Denmark).
   *
   * Offered only when /api/config/client carries an enabled `dhm` block
   * (roles.js stores it as window.MC_DHM_CFG); the server sends it only
   * while map.dhm is enabled with a token. Tiles come from the server's
   * same-origin WMS proxy, which adds the token: the browser never sees it.
   *
   * The layer lives in its own pane between the base tiles (tilePane, 200)
   * and the vector overlays/markers (overlayPane 400, markerPane 600). That
   * also keeps it out of the dark-theme invert filter on the tile pane.
   *
   * On/off is remembered per viewer (localStorage) and mirrored in the hash
   * as ?terrain=1. A ?terrain=1 / ?terrain=0 in the hash wins over the
   * stored choice and is saved, like Live's ?heard= (#660).
   */
  var TERRAIN_ID          = 'dhm-terrain';
  var TERRAIN_LABEL       = 'Terrain (DHM hillshade)';
  var TERRAIN_STORAGE_KEY = 'mc-terrain-dhm';
  var TERRAIN_HASH_PARAM  = 'terrain';
  var TERRAIN_OPACITY     = 0.65;
  var TERRAIN_PANE        = 'mcTerrainPane';
  var TERRAIN_PANE_Z      = 250;

  function getTerrainConfig() {
    var c = window.MC_DHM_CFG;
    if (!c || c.enabled !== true || typeof c.url !== 'string' || c.url.charAt(0) !== '/' || c.url.charAt(1) === '/') return null;
    return c;
  }

  function resolveTerrainOn(hashValue, stored) {
    if (hashValue === '1') return true;
    if (hashValue === '0') return false;
    return stored === '1';
  }

  function _hashParts() {
    var hash = (typeof location !== 'undefined' && location.hash) || '';
    var qi = hash.indexOf('?');
    return { base: qi >= 0 ? hash.slice(0, qi) : hash, params: new URLSearchParams(qi >= 0 ? hash.slice(qi + 1) : '') };
  }

  function _readTerrainPref() {
    var hv = null, stored = null;
    try { hv = _hashParts().params.get(TERRAIN_HASH_PARAM); } catch (_) {}
    try { stored = window.localStorage && window.localStorage.getItem(TERRAIN_STORAGE_KEY); } catch (_) {}
    var on = resolveTerrainOn(hv, stored);
    if (hv === '1' || hv === '0') {
      try { if (window.localStorage) window.localStorage.setItem(TERRAIN_STORAGE_KEY, on ? '1' : '0'); } catch (_) {}
    }
    return on;
  }

  // replaceState, so the SPA router does not re-run on the hash rewrite.
  function _saveTerrainPref(on) {
    try { if (window.localStorage) window.localStorage.setItem(TERRAIN_STORAGE_KEY, on ? '1' : '0'); } catch (_) {}
    try {
      var h = _hashParts();
      if (on) h.params.set(TERRAIN_HASH_PARAM, '1');
      else h.params.delete(TERRAIN_HASH_PARAM);
      var qs = h.params.toString();
      // An empty hash gets a bare '#', so the parameter never lands in the
      // query string (#682 review F4).
      history.replaceState(history.state, '', location.pathname + location.search + (qs ? (h.base || '#') + '?' + qs : h.base));
    } catch (_) {}
  }

  function createTerrainLayer(map, cfg) {
    if (!map.getPane(TERRAIN_PANE)) {
      var pane = map.createPane(TERRAIN_PANE);
      pane.style.zIndex = String(TERRAIN_PANE_Z);
      pane.style.pointerEvents = 'none';
      pane.classList.add('mc-terrain-pane');
    }
    // Only these options become WMS parameters (layers, styles, format,
    // transparent, version + crs/width/height/bbox); the server accepts
    // exactly that shape. `bounds` keeps Leaflet from requesting tiles
    // outside DHM coverage at all.
    return L.tileLayer.wms(cfg.url, {
      layers: cfg.layer,
      styles: '',
      format: 'image/png',
      transparent: true,
      version: '1.3.0',
      crs: L.CRS.EPSG3857,
      tileSize: 256,
      opacity: TERRAIN_OPACITY,
      pane: TERRAIN_PANE,
      minZoom: cfg.minZoom,
      maxZoom: cfg.maxZoom,
      bounds: L.latLngBounds(cfg.bounds),
      attribution: cfg.attribution,
      className: 'mc-terrain-tiles'
    });
  }

  // Live runs without an attribution control; the DHM credit is still
  // required while the overlay is on, so such a map gets one for exactly
  // that time (it lists every active layer's credit, as Leaflet does).
  function _syncTerrainAttribution(map, on) {
    if (on && !map.attributionControl && !map._mcTerrainAttribution) {
      map._mcTerrainAttribution = L.control.attribution({ position: 'bottomright', prefix: false }).addTo(map);
    } else if (!on && map._mcTerrainAttribution) {
      try { map._mcTerrainAttribution.remove(); } catch (_) {}
      map._mcTerrainAttribution = null;
    }
  }

  // Once per map: listen for the user's toggles and apply the initial
  // choice. Runs before the layer control exists, so the initial add does
  // not make L.Control.Layers re-render its labels (that would undo the
  // label surgery in _buildControl) and is not mistaken for a toggle.
  function _wireTerrain(map, layer) {
    if (map._mcTerrainWired) return;
    map._mcTerrainWired = true;
    map.on('overlayadd overlayremove', function (e) {
      if (e.layer !== map._mcTerrainLayer) return;
      var on = e.type === 'overlayadd';
      _syncTerrainAttribution(map, on);
      _saveTerrainPref(on);
    });
    if (_readTerrainPref()) {
      map.addLayer(layer);
      _syncTerrainAttribution(map, true);
    }
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
  window.MC_resolveTerrainOn            = resolveTerrainOn;


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

      // #680 — the terrain overlay survives control rebuilds; it is dropped
      // only if the server stops offering it.
      var terrainCfg = getTerrainConfig();
      var overlays = null;
      if (terrainCfg) {
        if (!map._mcTerrainLayer) map._mcTerrainLayer = createTerrainLayer(map, terrainCfg);
        _wireTerrain(map, map._mcTerrainLayer);
        overlays = {};
        overlays[TERRAIN_LABEL] = map._mcTerrainLayer;
      } else if (map._mcTerrainLayer) {
        try { map.removeLayer(map._mcTerrainLayer); } catch (_) {}
        _syncTerrainAttribution(map, false);
        map._mcTerrainLayer = null;
      }

      // Use the dynamic controlPosition variable instead of a hardcoded string
      _control = L.control.layers(baseMaps, overlays, { position: controlPosition }).addTo(map);

      if (terrainCfg) {
        try {
          var overlayLabel = _control.getContainer().querySelector('.leaflet-control-layers-overlays label');
          if (overlayLabel) overlayLabel.setAttribute('data-overlay-id', TERRAIN_ID);
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
          } else if (REGISTRY[id]) {
            span.innerHTML = ' ' + (REGISTRY[id].label || id);
          }
          
          if (id === firstDarkId && lightIds.length > 0) {
            var sep = document.createElement('div');
            sep.className = 'leaflet-control-layers-separator';
            labelEl.parentNode.insertBefore(sep, labelEl);
          }
        }
      } catch (_) {}

      // Decide initial active layer
      if (_isAuto) {
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
