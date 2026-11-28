/* === CoreScope — page-title.js (#563) ===
 *
 * One owner for document.title. Format: "<View> · <entity> · <instance>",
 * e.g. "Live · My Mesh" or "Reach · Node A · My Mesh". The instance name comes
 * from branding/customizer (setInstance); the view comes from the router
 * (setRoute); the optional entity label is set by a page once it already holds
 * the name (setEntity), guarded by the visit token so a late response from an
 * earlier navigation cannot rename the tab. Everything is assigned as plain
 * text to document.title — never HTML.
 */
'use strict';

(function (root) {
  var SEP = ' · ';
  var MAX_LABEL = 60;

  var ROUTE_LABELS = {
    home: 'Home',
    packets: 'Packets',
    'packet-detail': 'Packet',
    map: 'Map',
    live: 'Live',
    channels: 'Channels',
    nodes: 'Nodes',
    'node-analytics': 'Node Analytics',
    'node-reach': 'Reach',
    observers: 'Observers',
    'observer-detail': 'Observer',
    analytics: 'Analytics',
    perf: 'Performance',
    'tools-landing': 'Tools',
    traces: 'Trace Viewer',
    'path-inspector': 'Path Inspector',
    'observer-neighbors-tool': 'Observer Neighbors',
    'new-nodes-tool': 'New Nodes',
    'node-changes-tool': 'Node Changes',
    'network-digest-tool': 'Network Digest',
    'position-gaps-tool': 'Position Gaps',
    'gps-sanity-tool': 'GPS Sanity',
    compare: 'Compare',
    'audio-lab': 'Audio Lab',
    privacy: 'Privacy',
    'ping-scores': 'Ping Scores',
    'reach-rank': 'Reach Rank',
    'rx-coverage': 'RX Coverage',
    regions: 'Regions'
  };
  var FALLBACK_LABEL = 'Not found';

  // One line, no control characters, bounded length.
  function clean(s) {
    s = String(s == null ? '' : s).replace(/[\u0000-\u001f\u007f]+/g, ' ').replace(/\s+/g, ' ').trim();
    return s.length > MAX_LABEL ? s.slice(0, MAX_LABEL - 1) + '…' : s;
  }

  function titleFor(view, entity, instance) {
    var parts = [];
    if (view) parts.push(view);
    if (entity) parts.push(entity);
    if (instance) parts.push(instance);
    return parts.join(SEP);
  }

  function create(doc) {
    var instance = 'CoreScope';
    var view = '';
    var entity = '';
    var visit = 0;

    function render() {
      var t = titleFor(view, entity, instance);
      if (doc && doc.title !== t) doc.title = t;
      return t;
    }

    return {
      setInstance: function (name) {
        var n = clean(name);
        if (!n) return;
        instance = n;
        render();
      },
      setRoute: function (page) {
        visit++;
        entity = '';
        view = Object.prototype.hasOwnProperty.call(ROUTE_LABELS, page) ? ROUTE_LABELS[page] : FALLBACK_LABEL;
        return render();
      },
      visit: function () { return visit; },
      setEntity: function (label, token) {
        if (token !== visit) return false;
        entity = clean(label);
        render();
        return true;
      }
    };
  }

  var api = { create: create, titleFor: titleFor, ROUTE_LABELS: ROUTE_LABELS };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  if (root && root.document) {
    root.PageTitle = create(root.document);
    // Branding/customizer entry point: sets the instance name, keeps the route.
    root.setInstanceTitle = function (name) { root.PageTitle.setInstance(name); };
  }
  if (root) root.PageTitleLib = api;
})(typeof window !== 'undefined' ? window : (typeof globalThis !== 'undefined' ? globalThis : this));
