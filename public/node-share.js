/* === CoreScope — node-share.js ===
 * Node share links (#811 M4). Copy URL on the node page and in the side
 * pane hands out /p?node=<pk>, which previews as a node card in a chat,
 * only when the server publishes share.nodes in /api/config/client
 * (share.nodes.enabled), the node's role is one of the server's card roles,
 * the key is a full 64-hex key (the only form /p?node= accepts), and the
 * viewer has not turned "Copy link shares a preview" off in the customizer
 * (terrain.shareLinks, the #727 M4 switch, which covers both card types).
 * Otherwise it is the plain #/nodes/<pk> link, as before. */
(function (global) {
  'use strict';

  var FULL_KEY = /^[0-9a-fA-F]{64}$/;

  /** offered: the server serves /p?node= (share.nodes is true). */
  function offered(cfg) {
    return !!(cfg && cfg.nodes === true);
  }

  /** carded: role is one of the server's card roles (share.roles). */
  function carded(cfg, role) {
    return offered(cfg) && Array.isArray(cfg.roles) && typeof role === 'string' && role !== '' && cfg.roles.indexOf(role) >= 0;
  }

  /** viewerOn: the customizer's switch, on unless stored false. */
  function viewerOn(siteConfig) {
    var t = siteConfig && siteConfig.terrain;
    return !(t && typeof t === 'object' && t.shareLinks === false);
  }

  function url(loc, pk, role, cfg, viewer) {
    var key = String(pk == null ? '' : pk);
    var origin = String((loc && loc.origin) || '');
    if (viewer === true && FULL_KEY.test(key) && carded(cfg, role)) {
      return origin + '/p?node=' + key.toLowerCase();
    }
    return origin + '/#/nodes/' + encodeURIComponent(key);
  }

  /** copyURL: the link for node n, read at copy time so a late config or
   *  a customizer change needs no re-render. */
  function copyURL(n) {
    return url(global.location, n && n.public_key, n && n.role, global.MC_SHARE_CFG, viewerOn(global.SITE_CONFIG));
  }

  global.NodeShare = { offered: offered, carded: carded, viewerOn: viewerOn, url: url, copyURL: copyURL };
})(typeof window !== 'undefined' ? window : this);
