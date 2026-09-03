'use strict';

// Privacy — the opt-in #/privacy page.
//
// The notice is a FIXED document: DOC below is the whole of it, and it is
// the only content this page can ever show. Config decides WHETHER the page
// is published (privacy.enabled, see PrivacyConfig in cmd/server/config.go
// and window.MC_PRIVACY in public/roles.js) and nothing else. No operator
// value — current, stale or newly added — reaches the DOM, so the visible
// text cannot drift from the notice that was signed off.
//
// SECURITY: DOC is code, never config. Every text run goes through
// escapeHtml, and the single link's href is a compile-time constant that is
// re-checked against SAFE_URL_RE at render time, so only an absolute
// http(s) URL can ever become an href. The renderer emits a closed set of
// tags (h2/p/ul/li/strong/br/a) and has no path that copies input into
// markup.

(function () {
  // Inline run constructors. The tuple shapes are the ONLY thing render()
  // understands; anything else is dropped rather than emitted.
  function t(s) { return ['t', s]; }          // plain text
  function b(s) { return ['b', s]; }          // **bold**
  function a(label, href) { return ['a', label, href]; }
  var BR = ['br'];                            // markdown hard line break

  // Absolute http(s) only — belt-and-braces around a constant, so a future
  // edit to DOC cannot smuggle javascript:/data: into an href.
  var SAFE_URL_RE = /^https?:\/\/[^\s"'<>]+$/;

  // ── The notice. Authoritative text; do not reword, extend or trim. ──
  var DOC = [
    ['h', "WHO WE ARE"],
    ['p', [
      t("meshview.dk is a non-commercial community service that visualises the Danish "),
      a("MeshCore", "https://meshcore.co.uk/"),
      t(" LoRa mesh network. It runs the open-source CoreScope analyzer. The data controller is:"),
    ]],
    ['p', [
      b("The operator of meshview.dk"),
      BR,
      t("Contact: "),
      b("kontakt@meshview.dk"),
    ]],
    ['h', "WHAT THIS SITE DOES"],
    ['p', [
      t("Volunteer-run observer nodes listen to MeshCore radio traffic and forward the packets they hear to this site over MQTT. The site displays a live map and analysis of the network so that node operators and the community can see coverage, diagnose problems, and keep the mesh healthy."),
    ]],
    ['h', "WHAT DATA WE PROCESS"],
    ['p', [
      t("All data originates from radio packets that MeshCore devices broadcast themselves:"),
    ]],
    ['ul', [
      [
        b("Node adverts"),
        t(": node name, role, public key, and the GPS position the node is configured to advertise. Node names are chosen by their operators and may contain a personal handle or name; an advertised position may reveal where the operator lives."),
      ],
      [
        b("Packet metadata"),
        t(": timestamps, packet types, routing paths, hop counts, and signal measurements (SNR/RSSI) as heard by observers."),
      ],
      [
        b("Node telemetry"),
        t(": values a node chooses to broadcast, such as battery level and uptime."),
      ],
      [
        b("Public channel messages"),
        t(": messages sent on well-known public channels (whose encryption keys are community knowledge) are decoded and shown, including the sender's node name and timestamp. Direct (private) messages are end-to-end encrypted and are never decrypted or displayed."),
      ],
    ]],
    ['p', [
      b("Website visitors"),
      t(": the site uses no analytics, tracking, or advertising cookies. [Our web server keeps standard technical logs, including IP addresses, for a short period for security and abuse prevention.]"),
    ]],
    ['h', "WHY, AND ON WHAT LEGAL BASIS"],
    ['p', [
      t("We process this data under "),
      b("legitimate interest"),
      t(" (GDPR Art. 6(1)(f)): operating, mapping, and troubleshooting a community radio network — the same purpose for which node operators broadcast this information in the first place. The data shown is limited to what devices already transmit openly over the air, and an easy opt-out exists (below)."),
    ]],
    ['h', "HOW LONG WE KEEP IT"],
    ['p', [
      t("Packet data, telemetry, and decoded public-channel messages are kept "),
      b("indefinitely"),
      t(", as a historical archive used for long-term network analysis (coverage trends, node health over time). We periodically review the archive and delete data that is no longer needed for that purpose. The node directory and map reflect the "),
      b("current"),
      t(" state of the network; nodes that stop advertising disappear from the live view, though their historical packets remain in the archive."),
    ]],
    ['h', "WHO CAN SEE IT, AND WHO WE SHARE IT WITH"],
    ['p', [
      t("The site is publicly accessible, so anything displayed here can be seen by anyone. We do not sell data or share it with third parties, apart from the hosting provider that technically operates the server [hosted within the EU/EEA]."),
    ]],
    ['h', "A NOTE ON PUBLIC CHANNELS"],
    ['p', [
      t("Public MeshCore channels are receivable and readable by anyone with a radio. Please do not send personal information over them — this site, like any other listener, will pick it up and keep it in the archive."),
    ]],
  ];

  function inline(runs) {
    var out = '';
    for (var i = 0; i < runs.length; i++) {
      var r = runs[i];
      if (r[0] === 't') { out += escapeHtml(r[1]); }
      else if (r[0] === 'b') { out += '<strong>' + escapeHtml(r[1]) + '</strong>'; }
      else if (r[0] === 'br') { out += '<br>'; }
      else if (r[0] === 'a') {
        // An unsafe href is never emitted: the label degrades to plain text.
        out += SAFE_URL_RE.test(r[2])
          ? '<a href="' + escapeHtml(r[2]) + '" rel="noopener noreferrer" target="_blank">' + escapeHtml(r[1]) + '</a>'
          : escapeHtml(r[1]);
      }
    }
    return out;
  }

  function block(node) {
    if (node[0] === 'h') { return '<h2 class="privacy-h">' + escapeHtml(node[1]) + '</h2>'; }
    if (node[0] === 'p') { return '<p>' + inline(node[1]) + '</p>'; }
    if (node[0] === 'ul') {
      var items = '';
      for (var i = 0; i < node[1].length; i++) { items += '<li>' + inline(node[1][i]) + '</li>'; }
      return '<ul>' + items + '</ul>';
    }
    return '';
  }

  function renderNotice(container) {
    var html = '';
    for (var i = 0; i < DOC.length; i++) { html += block(DOC[i]); }
    container.innerHTML = '<div class="privacy-page">' + html + '</div>';
  }

  // Deployments that have not opted in show no notice at all — and, since
  // the notice is not theirs, no heading or fragment of it either.
  function renderDisabled(container) {
    container.innerHTML =
      '<div class="privacy-page">' +
      '<p class="text-muted">This deployment has not published a privacy notice.</p>' +
      '<p><a href="#/home">Back to Home</a></p>' +
      '</div>';
  }

  registerPage('privacy', {
    init: function (container) {
      container.innerHTML = '<div class="privacy-page"><p class="text-muted">Loading…</p></div>';
      // Config arrives via roles.js's /api/config/client fetch. Gate on
      // MeshConfigReady so a direct deep-link to #/privacy renders after
      // window.MC_PRIVACY is actually there.
      var ready = (window.MeshConfigReady && typeof window.MeshConfigReady.then === 'function')
        ? window.MeshConfigReady
        : Promise.resolve();
      return ready.then(function () {
        var cfg = window.MC_PRIVACY;
        if (!cfg || !cfg.enabled) { renderDisabled(container); return; }
        renderNotice(container);
      }).catch(function () { renderDisabled(container); });
    },
    destroy: function () {}
  });
})();
