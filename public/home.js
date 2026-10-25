/* === CoreScope — home.js (My Mesh Dashboard) === */
'use strict';

(function () {
  let searchTimeout = null;
  let miniMap = null;
  let searchAbort = null; // AbortController for document-level listeners
  let _themeRefreshHandler = null;

  // #1648 M5: home steps + footerLinks may store either a 'ph:<name>' sprite
  // token (new defaults) or a legacy emoji/text string (operator config).
  // Return safe HTML — sprite for ph tokens, escaped text for everything
  // else. EMOJI-OK-LEGACY-RENDER below preserves operator-stored emoji.
  function _renderHomeGlyph(value) {
    if (value == null) return '';
    const s = String(value);
    const m = s.match(/^ph:([a-z][a-z0-9-]+)$/);
    if (m) return `<svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-${m[1]}"/></svg>`;
    return _escHtml(s); // EMOJI-OK-LEGACY-RENDER
  }
  function _renderHomeLabel(label) {
    if (label == null) return '';
    const s = String(label);
    const m = s.match(/^(ph:[a-z][a-z0-9-]+)\s+(.*)$/);
    if (m) return _renderHomeGlyph(m[1]) + ' ' + _escHtml(m[2]);
    return _escHtml(s); // EMOJI-OK-LEGACY-RENDER
  }
  // #429 — footerLinks come from importable Home customization, so the URL
  // gets the same scheme check as miniMarkdown links (roles.js). A rejected
  // URL renders as its label text.
  function footerLinkHtml(l) {
    const href = window.safeUrlAttr(l.url);
    const label = _renderHomeLabel(l.label);
    if (!href) return label;
    return `<a href="${href}" class="home-footer-link" target="_blank" rel="noopener">${label}</a>`;
  }
  function _escHtml(s) { return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;'); }

  const PREF_KEY = 'meshcore-user-level';
  const MY_NODES_KEY = 'meshcore-my-nodes'; // [{pubkey, name, addedAt}]

  function getMyNodes() {
    try { return JSON.parse(localStorage.getItem(MY_NODES_KEY)) || []; } catch { return []; }
  }
  function saveMyNodes(nodes) { localStorage.setItem(MY_NODES_KEY, JSON.stringify(nodes)); }
  function addMyNode(pubkey, name) {
    const nodes = getMyNodes();
    if (!nodes.find(n => n.pubkey === pubkey)) {
      nodes.push({ pubkey, name: name || pubkey.slice(0, 12), addedAt: new Date().toISOString() });
      saveMyNodes(nodes);
    }
  }
  function removeMyNode(pubkey) {
    saveMyNodes(getMyNodes().filter(n => n.pubkey !== pubkey));
  }
  function isMyNode(pubkey) { return getMyNodes().some(n => n.pubkey === pubkey); }
  // My Mesh cards opt into advert intervals on their health request. The
  // health panel reuses that URL for claimed nodes so the client cache
  // answers it; other nodes keep the plain URL and skip the advert scan.
  function healthPath(pubkey, myMesh) {
    return '/nodes/' + encodeURIComponent(pubkey) + '/health' + (myMesh ? '?include=advertIntervals' : '');
  }

  function isExperienced() { return localStorage.getItem(PREF_KEY) === 'experienced'; }
  function setLevel(level) { localStorage.setItem(PREF_KEY, level); }

  function init(container) {
    if (!localStorage.getItem(PREF_KEY)) {
      showChooser(container);
    } else {
      renderHome(container);
    }
    _themeRefreshHandler = function() {
      if (!localStorage.getItem(PREF_KEY)) showChooser(container);
      else renderHome(container);
    };
    window.addEventListener('theme-refresh', _themeRefreshHandler);
  }

  function showChooser(container) {
    container.innerHTML = `
      <section class="home-chooser">
        <h1>Welcome to ${escapeHtml(window.SITE_CONFIG?.branding?.siteName || 'CoreScope')}</h1>
        <p>How familiar are you with MeshCore?</p>
        <div class="chooser-options">
          <button class="chooser-btn new" id="chooseNew">
            <span class="chooser-icon" aria-hidden="true"><svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-plant"/></svg></span>
            <strong>I\u2019m new</strong>
            <span>Show me setup guides and tips</span>
          </button>
          <button class="chooser-btn exp" id="chooseExp">
            <span class="chooser-icon" aria-hidden="true"><svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-lightning"/></svg></span>
            <strong>I know what I\u2019m doing</strong>
            <span>Just the analyzer, skip the guides</span>
          </button>
        </div>
      </section>`;
    document.getElementById('chooseNew').addEventListener('click', () => { setLevel('new'); renderHome(container); });
    document.getElementById('chooseExp').addEventListener('click', () => { setLevel('experienced'); renderHome(container); });
  }

  function renderHome(container) {
    const exp = isExperienced();
    const myNodes = getMyNodes();
    const hasNodes = myNodes.length > 0;
    const homeCfg = window.SITE_CONFIG?.home || null;
    const siteName = window.SITE_CONFIG?.branding?.siteName || 'CoreScope';

    container.innerHTML = `
      <section class="home-hero">
        <svg class="home-hero-logo" xmlns="http://www.w3.org/2000/svg" width="1200" height="300" viewBox="0 0 1200 300" aria-hidden="true" focusable="false"><path d="M540 100 A 30 30 0 1 0 540 160" fill="none" stroke="var(--logo-accent)" stroke-width="8" opacity="1.00"/>
<path d="M540 73 A 57 57 0 1 0 540 187" fill="none" stroke="var(--logo-accent)" stroke-width="8" opacity="0.82"/>
<path d="M540 46 A 84 84 0 1 0 540 214" fill="none" stroke="var(--logo-accent)" stroke-width="8" opacity="0.64"/>
<path d="M540 19 A 111 111 0 1 0 540 241" fill="none" stroke="var(--logo-accent)" stroke-width="8" opacity="0.46"/>
<path d="M660 100 A 30 30 0 1 1 660 160" fill="none" stroke="var(--logo-accent-hi)" stroke-width="8" opacity="1.00"/>
<path d="M660 73 A 57 57 0 1 1 660 187" fill="none" stroke="var(--logo-accent-hi)" stroke-width="8" opacity="0.82"/>
<path d="M660 46 A 84 84 0 1 1 660 214" fill="none" stroke="var(--logo-accent-hi)" stroke-width="8" opacity="0.64"/>
<path d="M660 19 A 111 111 0 1 1 660 241" fill="none" stroke="var(--logo-accent-hi)" stroke-width="8" opacity="0.46"/>

<polyline points="540.00,130.00 540.30,130.17 540.60,130.35 540.90,130.53 541.20,130.71 541.50,130.89 541.80,131.07 542.10,131.26 542.40,131.45 542.70,131.64 543.00,131.83 543.30,132.02 543.60,132.21 543.90,132.41 544.20,132.61 544.50,132.80 544.80,133.00 545.10,133.20 545.40,133.40 545.70,133.60 546.00,133.81 546.30,134.01 546.60,134.21 546.90,134.42 547.20,134.62 547.50,134.82 547.80,135.03 548.10,135.23 548.40,135.44 548.70,135.64 549.00,135.84 549.30,136.04 549.60,136.24 549.90,136.44 550.20,136.64 550.50,136.83 550.80,137.02 551.10,137.22 551.40,137.41 551.70,137.59 552.00,137.78 552.30,137.96 552.60,138.14 552.90,138.31 553.20,138.49 553.50,138.66 553.80,138.82 554.10,138.98 554.40,139.14 554.70,139.29 555.00,139.44 555.30,139.58 555.60,139.71 555.90,139.84 556.20,139.97 556.50,140.09 556.80,140.20 557.10,140.31 557.40,140.41 557.70,140.50 558.00,140.59 558.30,140.67 558.60,140.74 558.90,140.80 559.20,140.85 559.50,140.90 559.80,140.94 560.10,140.97 560.40,140.99 560.70,141.00 561.00,141.00 561.30,140.99 561.60,140.97 561.90,140.95 562.20,140.91 562.50,140.86 562.80,140.80 563.10,140.73 563.40,140.65 563.70,140.56 564.00,140.46 564.30,140.35 564.60,140.23 564.90,140.09 565.20,139.95 565.50,139.79 565.80,139.62 566.10,139.44 566.40,139.25 566.70,139.05 567.00,138.84 567.30,138.61 567.60,138.38 567.90,138.13 568.20,137.87 568.50,137.60 568.80,137.33 569.10,137.04 569.40,136.74 569.70,136.43 570.00,136.11 570.30,135.78 570.60,135.45 570.90,135.10 571.20,134.75 571.50,134.38 571.80,134.01 572.10,133.64 572.40,133.25 572.70,132.86 573.00,132.46 573.30,132.06 573.60,131.65 573.90,131.24 574.20,130.82 574.50,130.40 574.80,129.98 575.10,129.56 575.40,129.13 575.70,128.71 576.00,128.28 576.30,127.85 576.60,127.43 576.90,127.00 577.20,126.58 577.50,126.17 577.80,125.75 578.10,125.35 578.40,124.94 578.70,124.55 579.00,124.16 579.30,123.78 579.60,123.41 579.90,123.05 580.20,122.70 580.50,122.36 580.80,122.03 581.10,121.71 581.40,121.41 581.70,121.13 582.00,120.85 582.30,120.60 582.60,120.36 582.90,120.14 583.20,119.93 583.50,119.75 583.80,119.58 584.10,119.43 584.40,119.31 584.70,119.20 585.00,119.12 585.30,119.06 585.60,119.02 585.90,119.00 586.20,119.01 586.50,119.04 586.80,119.09 587.10,119.17 587.40,119.27 587.70,119.39 588.00,119.54 588.30,119.71 588.60,119.91 588.90,120.13 589.20,120.37 589.50,120.64 589.80,120.92 590.10,121.24 590.40,121.57 590.70,121.92 591.00,122.30 591.30,122.69 591.60,123.11 591.90,123.54 592.20,123.99 592.50,124.46 592.80,124.94 593.10,125.44 593.40,125.95 593.70,126.48 594.00,127.01 594.30,127.56 594.60,128.11 594.90,128.67 595.20,129.24 595.50,129.81 595.80,130.38 596.10,130.96 596.40,131.53 596.70,132.10 597.00,132.67 597.30,133.24 597.60,133.79 597.90,134.34 598.20,134.87 598.50,135.40 598.80,135.91 599.10,136.40 599.40,136.88 599.70,137.34 600.00,137.78 600.30,138.19 600.60,138.59 600.90,138.96 601.20,139.30 601.50,139.61 601.80,139.90 602.10,140.15 602.40,140.37 602.70,140.56 603.00,140.72 603.30,140.84 603.60,140.93 603.90,140.98 604.20,141.00 604.50,140.98 604.80,140.92 605.10,140.83 605.40,140.70 605.70,140.53 606.00,140.32 606.30,140.08 606.60,139.80 606.90,139.49 607.20,139.14 607.50,138.75 607.80,138.34 608.10,137.89 608.40,137.42 608.70,136.91 609.00,136.38 609.30,135.82 609.60,135.24 609.90,134.64 610.20,134.01 610.50,133.37 610.80,132.72 611.10,132.05 611.40,131.37 611.70,130.69 612.00,130.00 612.30,129.31 612.60,128.62 612.90,127.93 613.20,127.25 613.50,126.58 613.80,125.91 614.10,125.27 614.40,124.64 614.70,124.03 615.00,123.45 615.30,122.89 615.60,122.36 615.90,121.86 616.20,121.39 616.50,120.96 616.80,120.57 617.10,120.21 617.40,119.90 617.70,119.64 618.00,119.41 618.30,119.24 618.60,119.11 618.90,119.03 619.20,119.00 619.50,119.02 619.80,119.09 620.10,119.21 620.40,119.39 620.70,119.61 621.00,119.88 621.30,120.20 621.60,120.57 621.90,120.98 622.20,121.44 622.50,121.94 622.80,122.48 623.10,123.06 623.40,123.68 623.70,124.33 624.00,125.01 624.30,125.71 624.60,126.44 624.90,127.19 625.20,127.96 625.50,128.73 625.80,129.52 626.10,130.31 626.40,131.10 626.70,131.89 627.00,132.67 627.30,133.44 627.60,134.19 627.90,134.93 628.20,135.63 628.50,136.31 628.80,136.96 629.10,137.57 629.40,138.14 629.70,138.67 630.00,139.15 630.30,139.58 630.60,139.95 630.90,140.28 631.20,140.54 631.50,140.75 631.80,140.90 632.10,140.98 632.40,141.00 632.70,140.96 633.00,140.85 633.30,140.68 633.60,140.44 633.90,140.14 634.20,139.78 634.50,139.36 634.80,138.89 635.10,138.36 635.40,137.78 635.70,137.14 636.00,136.47 636.30,135.75 636.60,134.99 636.90,134.20 637.20,133.38 637.50,132.54 637.80,131.68 638.10,130.81 638.40,129.93 638.70,129.05 639.00,128.17 639.30,127.31 639.60,126.45 639.90,125.62 640.20,124.82 640.50,124.05 640.80,123.31 641.10,122.62 641.40,121.98 641.70,121.39 642.00,120.85 642.30,120.38 642.60,119.97 642.90,119.63 643.20,119.36 643.50,119.17 643.80,119.05 644.10,119.00 644.40,119.03 644.70,119.14 645.00,119.33 645.30,119.59 645.60,119.93 645.90,120.34 646.20,120.83 646.50,121.38 646.80,121.99 647.10,122.67 647.40,123.40 647.70,124.18 648.00,125.01 648.30,125.87 648.60,126.77 648.90,127.69 649.20,128.64 649.50,129.60 649.80,130.56 650.10,131.52 650.40,132.47 650.70,133.40 651.00,134.31 651.30,135.19 651.60,136.02 651.90,136.82 652.20,137.56 652.50,138.24 652.80,138.86 653.10,139.41 653.40,139.88 653.70,140.27 654.00,140.59 654.30,140.81 654.60,140.95 654.90,141.00 655.20,140.96 655.50,140.82 655.80,140.60 656.10,140.29 656.40,139.89 656.70,139.40 657.00,138.84 657.30,138.19 657.60,137.48 657.90,136.70 658.20,135.86 658.50,134.97 658.80,134.03 659.10,133.06 659.40,132.06 659.70,131.03 660.00,130.00" fill="none" stroke="var(--logo-muted)" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" opacity="1.0"/>
<circle cx="540" cy="130" r="13" fill="var(--logo-accent)"/>
<circle cx="660" cy="130" r="13" fill="var(--logo-accent-hi)"/>
<text x="426.20" y="161.68" font-family="Aldrich, monospace" font-size="88" text-anchor="end" dominant-baseline="alphabetic" font-weight="700" letter-spacing="4.4" fill="var(--logo-accent)">CORE</text><text x="773.80" y="161.68" font-family="Aldrich, monospace" font-size="88" text-anchor="start" dominant-baseline="alphabetic" font-weight="700" letter-spacing="4.4" fill="var(--logo-accent-hi)">SCOPE</text><text x="604.00" y="279.20" font-family="Aldrich, monospace" font-size="20" text-anchor="middle" dominant-baseline="alphabetic" font-weight="500" letter-spacing="8" fill="var(--logo-muted)">MESH ANALYZER</text></svg>
        <h1>${hasNodes ? 'My Mesh' : escapeHtml(homeCfg?.heroTitle || siteName)}</h1>
        <p>${hasNodes ? 'Your nodes at a glance. Add more by searching below.' : escapeHtml(homeCfg?.heroSubtitle || 'Find your nodes to start monitoring them.')}</p>
        <div class="home-search-wrap">
          <input type="text" id="homeSearch" placeholder="Search by node name or public key…" autocomplete="off" aria-label="Search nodes" role="combobox" aria-expanded="false" aria-owns="homeSuggest" aria-autocomplete="list" aria-activedescendant="">
          <div class="home-suggest" id="homeSuggest" role="listbox"></div>
        </div>
      </section>

      ${hasNodes ? '<div class="my-nodes-grid" id="myNodesGrid"><div class="my-nodes-loading">Loading your nodes…</div></div>' : '<div class="my-nodes-grid" id="myNodesGrid"></div>'}
      <dialog class="mnc-observers-dialog" id="homeObserversDialog" aria-labelledby="homeObserversTitle">
        <div class="mnc-dialog-header"><h2 id="homeObserversTitle"></h2><button type="button" class="mnc-dialog-close" aria-label="Close observer list">Close</button></div>
        <ul class="mnc-dialog-list"></ul>
      </dialog>

      ${!hasNodes ? `
        <div class="onboarding-prompt">
          <div class="onboard-icon" aria-hidden="true"><svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-broadcast"/></svg></div>
          <h2>Claim your first node</h2>
          <p>Search for your node above, or paste your public key. Once claimed, you'll see live status, signal quality, and who's hearing you.</p>
        </div>
      ` : ''}

      <div class="home-detail-area">
        <div class="home-health" id="homeHealth"></div>
        <div class="home-journey" id="homeJourney"></div>
      </div>

      <div class="home-stats" id="homeStats"></div>

      ${exp ? '' : `
      <section class="home-checklist">
        <h2><svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-rocket"/></svg> Getting on the mesh${homeCfg?.steps ? '' : ' — SF Bay Area'}</h2>
        ${checklist(homeCfg)}
      </section>`}

      <section class="home-footer">
        <div class="home-footer-links">
          ${homeCfg?.footerLinks ? homeCfg.footerLinks.map(footerLinkHtml).join('') : `
          <a href="#/packets" class="home-footer-link"><svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-package"/></svg> Packets</a>
          <a href="#/map" class="home-footer-link"><svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-map-trifold"/></svg> Network Map</a>
          <a href="#/live" class="home-footer-link"><svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-broadcast"/></svg> Live</a>
          <a href="#/nodes" class="home-footer-link"><svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-broadcast"/></svg> All Nodes</a>
          <a href="#/channels" class="home-footer-link"><svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-chats"/></svg> Channels</a>`}
        </div>
        <div class="home-level-toggle">
          <small>${exp ? 'Want setup guides? ' : 'Already know MeshCore? '}
          <a href="#" id="toggleLevel" style="color:var(--link-color)">${exp ? 'Show new user tips' : 'Hide guides'}</a></small>
        </div>
      </section>
    `;

    document.getElementById('toggleLevel')?.addEventListener('click', (e) => {
      e.preventDefault();
      setLevel(exp ? 'new' : 'experienced');
      renderHome(container);
    });

    setupSearch(container);
    loadStats();
    if (hasNodes) loadMyNodes();

    // Checklist accordion
    container.querySelectorAll('.checklist-q').forEach(q => {
      const toggle = () => {
        const item = q.parentElement;
        item.classList.toggle('open');
        q.setAttribute('aria-expanded', item.classList.contains('open'));
      };
      q.addEventListener('click', toggle);
      q.addEventListener('keydown', (e) => {
        if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); toggle(); }
      });
    });
  }

  function setupSearch(container) {
    const input = document.getElementById('homeSearch');
    const suggest = document.getElementById('homeSuggest');
    if (!input || !suggest) return;

    input.addEventListener('input', () => {
      clearTimeout(searchTimeout);
      const q = input.value.trim();
      if (!q) { suggest.classList.remove('open'); input.setAttribute('aria-expanded', 'false'); input.setAttribute('aria-activedescendant', ''); return; }
      searchTimeout = setTimeout(async () => {
        try {
          const data = await api('/nodes/search?q=' + encodeURIComponent(q), { ttl: CLIENT_TTL.nodeSearch });
          const nodes = data.nodes || [];
          if (!nodes.length) {
            suggest.innerHTML = '<div class="suggest-empty">No nodes found</div>';
          } else {
            suggest.innerHTML = nodes.slice(0, 10).map((n, idx) => {
              const claimed = isMyNode(n.public_key);
              return `<div class="suggest-item" role="option" id="suggest-${idx}" data-key="${n.public_key}" data-name="${escapeAttr(n.name || '')}">
                <div class="suggest-main">
                  <span class="suggest-name">${escapeHtml(n.name || 'Unknown')}</span>
                  <small class="suggest-key">${truncate(n.public_key, 16)}</small>
                </div>
                <div class="suggest-actions">
                  <span class="suggest-role badge-${n.role || 'unknown'}">${n.role || '?'}</span>
                  <button class="suggest-claim ${claimed ? 'claimed' : ''}" data-key="${n.public_key}" data-name="${escapeAttr(n.name || '')}" title="${claimed ? 'Remove from My Mesh' : 'Add to My Mesh'}">
                    ${claimed ? '<svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-check"/></svg> Mine' : '+ Claim'}
                  </button>
                </div>
              </div>`;
            }).join('');
          }
          suggest.classList.add('open');
          input.setAttribute('aria-expanded', 'true');
          input.setAttribute('aria-activedescendant', '');

          // Claim buttons
          suggest.querySelectorAll('.suggest-claim').forEach(btn => {
            btn.addEventListener('click', (e) => {
              e.stopPropagation();
              const pk = btn.dataset.key;
              const nm = btn.dataset.name;
              if (isMyNode(pk)) {
                removeMyNode(pk);
                btn.classList.remove('claimed');
                btn.innerHTML = '+ Claim';
              } else {
                addMyNode(pk, nm);
                btn.classList.add('claimed');
                btn.innerHTML = '<svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-check"/></svg> Mine';
              }
              loadMyNodes();
            });
          });
        } catch { suggest.classList.remove('open'); input.setAttribute('aria-expanded', 'false'); }
      }, 200);
    });

    suggest.addEventListener('click', (e) => {
      const item = e.target.closest('.suggest-item');
      if (!item || !item.dataset.key || e.target.closest('.suggest-claim')) return;
      suggest.classList.remove('open');
      input.setAttribute('aria-expanded', 'false');
      input.value = '';
      loadHealth(item.dataset.key);
    });

    // Use AbortController so re-calling setupSearch won't stack listeners
    if (searchAbort) searchAbort.abort();
    searchAbort = new AbortController();
    document.addEventListener('click', handleOutsideClick, { signal: searchAbort.signal });
  }

  function handleOutsideClick(e) {
    const suggest = document.getElementById('homeSuggest');
    const input = document.getElementById('homeSearch');
    if (suggest && !e.target.closest('.home-search-wrap')) {
      suggest.classList.remove('open');
      if (input) { input.setAttribute('aria-expanded', 'false'); input.setAttribute('aria-activedescendant', ''); }
    }
  }

  function destroy() {
    clearTimeout(searchTimeout);
    if (searchAbort) { searchAbort.abort(); searchAbort = null; }
    if (miniMap) { miniMap.remove(); miniMap = null; }
    const observersDialog = document.getElementById('homeObserversDialog');
    if (observersDialog?.open) observersDialog.close();
    if (_themeRefreshHandler) { window.removeEventListener('theme-refresh', _themeRefreshHandler); _themeRefreshHandler = null; }
  }

  // ==================== MY NODES DASHBOARD ====================
  async function loadMyNodes() {
    const grid = document.getElementById('myNodesGrid');
    if (!grid) return;
    const myNodes = getMyNodes();

    // Update hero text dynamically
    const h1 = document.querySelector('.home-hero h1');
    const heroP = document.querySelector('.home-hero p');
    if (myNodes.length) {
      if (h1) h1.textContent = 'My Mesh';
      if (heroP) heroP.textContent = 'Your nodes at a glance. Add more by searching below.';
      // Hide onboarding prompt
      const onboard = document.querySelector('.onboarding-prompt');
      if (onboard) onboard.style.display = 'none';
    }

    if (!myNodes.length) {
      grid.innerHTML = '';
      return;
    }

    const observerLists = new Map();
    // #262: a node in My Mesh may ALSO be a registered observer, which is a
    // separate property from its mesh role. One bulk /observers request per
    // grid render feeds every card (O(n) index build, O(1) per card) — never
    // a request per card, never a scan of the list per card. A failed or
    // unusable response leaves the index unavailable, which the grid reports
    // explicitly instead of silently dropping the observer identity.
    const observerIndexPromise = api('/observers', { ttl: CLIENT_TTL.observers })
      .then(payload => MyMeshObservers.index(payload), () => MyMeshObservers.index(null));
    const observerRecords = new Map();

    const cards = await Promise.all(myNodes.map(async (mn) => {
      // The health request starts BEFORE the observer list is awaited: the
      // node part of the card never depends on /observers, so a slow or
      // retrying observer request must not sit in front of it. The no-op
      // catch only marks the rejection handled while we wait on the index;
      // the try below still sees the real error.
      // The 240 s nodeHealth TTL is intentional: it matches node detail's
      // cadence fetch (CLIENT_TTL.nodeDetail), and WS traffic drops it early.
      const healthPromise = api(healthPath(mn.pubkey, true), { ttl: CLIENT_TTL.nodeHealth });
      healthPromise.catch(() => {});
      const observer = MyMeshObservers.match(await observerIndexPromise, mn.pubkey);
      if (observer) observerRecords.set(mn.pubkey, observer);
      try {
        const h = await healthPromise;
        const node = h.node || {};
        const stats = h.stats || {};
        // #76: the preview names direct observers; the dialog lists both.
        const heard = HeardBy.lists(h);
        const obs = heard.direct;
        observerLists.set(mn.pubkey, heard);

        // #304 point 3: ONE evidence model drives the badge, its wording and
        // the disclosure below. It reuses the shared liveness rules (roles.js,
        // #539) and reports a badge that rests only on an attributed relay
        // hash prefix as attributed, never as a confident green.
        const ev = MyMeshActivity.statusEvidence({ node, stats });
        const status = ev.status;
        const heardAt = ev.heardAt;
        const statusDot = '<span class="' + ev.cls + '" aria-label="' + escapeAttr(ev.text) +
          '"><svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#' + ev.glyph + '"/></svg></span>';
        const name = node.name || mn.name || truncate(mn.pubkey, 12);

        // #304 points 4 and 5: every headline number states what it covers.
        // No SNR quality verdict and no verdict colour: the mean describes
        // the observer links that proved a direct reception, not this node's
        // signal quality (#76).
        const snr = MyMeshActivity.snr(stats, obs);
        const traffic = MyMeshActivity.traffic24h(h.activity24h);
        const trend = MyMeshActivity.compare24h(h.activity24h);

        const sparkHtml = buildSparkline(h.activity24h);
        const isRepeater = String(node.role || '').toLowerCase() === 'repeater';
        const preview = isRepeater ? obs.slice(0, 3) : obs;
        const cadenceHtml = isRepeater ? renderMyMeshAdvertIntervals(h.advertIntervals, h.advertRouteBackfill) : '';
        // #351 F2/R1: a name wider than the CSS max-width: 14ch clamp is
        // clipped with an ellipsis. The full name must stay recoverable on
        // EVERY card. Each span carries a title (pointer + assistive tech),
        // and the accessible dialog (keyboard + touch) is offered when the
        // preview hides names. Otherwise the button starts hidden and
        // revealClippedObserverButtons() shows it after layout if a name is
        // actually clipped — glyph width, not character count, decides that.
        const obsFullName = o => HeardBy.observerName(o);
        const heardTotal = obs.length + heard.relay.length;
        const hiddenObservers = heardTotal - preview.length;
        const observerBtnText = hiddenObservers > 0 ? `View all ${heardTotal} →` : 'Full names →';
        const directNames = obs.length
          ? preview.map(o => `<span class="mnc-observer-name" title="${escapeAttr(obsFullName(o))}">${escapeHtml(obsFullName(o))}</span>`).join(', ')
          : `<span class="text-muted" title="${escapeAttr(HeardBy.EMPTY_DIRECT_TIP)}">none proven</span>`;
        const relayCount = heard.relay.length ? ` · <span class="mnc-relay-count" title="${escapeAttr(HeardBy.RELAY_TIP)}">${heard.relay.length} via relay</span>` : '';

        return `<div class="my-node-card ${status} evidence-${ev.source}${observer ? ' is-observer' : ''}" data-key="${mn.pubkey}" tabindex="0" role="button">
          <div class="mnc-header">
            <div class="mnc-status">${statusDot}</div>
            <div class="mnc-name">${escapeHtml(name)}</div>
            ${observerBadgeHtml(observer, node.role)}
            <div class="mnc-role">${node.role || '?'}</div>
            <button class="mnc-remove" data-key="${mn.pubkey}" title="Remove from My Mesh" aria-label="Remove ${escapeAttr(name)} from My Mesh"><svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-x"/></svg></button>
          </div>
          <div class="mnc-status-text">${escapeHtml(ev.text)}${heardAt ? ' · ' + timeAgo(heardAt) : ''}</div>
          ${renderMyMeshEvidence(ev, h.activity24h, stats)}
          ${observerPanelHtml(observer)}
          ${observer ? `<div class="mnc-section-lbl">${escapeHtml(MyMeshObservers.NODE_SECTION_LABEL)}</div>` : ''}
          <div class="mnc-metrics">
            <div class="mnc-metric" title="${escapeAttr(traffic.tip)}">
              <div class="mnc-val">${escapeHtml(traffic.value)}${trend.short ? ` <span class="mnc-metric-sub" title="${escapeAttr(trend.text + ' — ' + trend.tip)}">${escapeHtml(trend.short)}</span>` : ''}</div>
              <div class="mnc-lbl">${escapeHtml(traffic.label)}</div>
            </div>
            <div class="mnc-metric" title="${escapeAttr(HeardBy.DIRECT_TIP)}">
              <div class="mnc-val">${obs.length}</div>
              <div class="mnc-lbl">Direct observers</div>
            </div>
            <div class="mnc-metric" title="${escapeAttr(snr.tip)}">
              <div class="mnc-val">${escapeHtml(snr.value)}</div>
              <div class="mnc-lbl">${escapeHtml(snr.label)}</div>
            </div>
          </div>
          ${heardTotal ? `<div class="mnc-observers"><strong title="${escapeAttr(HeardBy.DIRECT_TIP)}">Heard directly by:</strong> ${directNames}${relayCount} <button type="button" class="mnc-btn mnc-view-all" data-action="observers" data-key="${escapeAttr(mn.pubkey)}" aria-label="Show all ${heardTotal} observer names for ${escapeAttr(name)}"${hiddenObservers > 0 ? '' : ' hidden'}>${observerBtnText}</button></div>` : ''}
          ${cadenceHtml}
          <div class="mnc-spark">${sparkHtml}</div>
          <div class="mnc-actions">
            <button class="mnc-btn" data-action="node" data-key="${escapeAttr(mn.pubkey)}">Node page →</button>
            ${observerActionHtml(observer, mn.pubkey, name)}
            <button class="mnc-btn" data-action="health" data-key="${mn.pubkey}">Full health →</button>
            <button class="mnc-btn" data-action="packets" data-key="${mn.pubkey}">View packets →</button>
          </div>
        </div>`;
      } catch (err) {
        const is404 = err && err.message && err.message.includes('404');
        const statusIcon = is404 ? '<span class="status-warn"><svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-broadcast"/></svg></span>' : '<span class="status-muted"><svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-question"/></svg></span>';
        const statusMsg = is404
          ? 'Waiting for first advert — this node has been seen in channel messages but hasn\u2019t advertised yet'
          : 'Could not load data';
        // #262: the observer record comes from the bulk observer list, not
        // from node health, so a failed health request must not hide the
        // observer identity, status or reported traffic. The mesh role is
        // genuinely unknown here, so the badge carries no secondary term.
        const fallbackName = mn.name || truncate(mn.pubkey, 12);
        return `<div class="my-node-card silent${observer ? ' is-observer' : ''}" data-key="${mn.pubkey}" tabindex="0" role="button">
          <div class="mnc-header">
            <div class="mnc-status">${statusIcon}</div>
            <div class="mnc-name">${escapeHtml(fallbackName)}</div>
            ${observerBadgeHtml(observer, null)}
            <button class="mnc-remove" data-key="${mn.pubkey}" title="Remove" aria-label="Remove ${escapeAttr(fallbackName)} from My Mesh"><svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-x"/></svg></button>
          </div>
          <div class="mnc-status-text">${statusMsg}</div>
          ${observerPanelHtml(observer)}
          <div class="mnc-actions">
            <button class="mnc-btn" data-action="node" data-key="${escapeAttr(mn.pubkey)}">Node page →</button>
            ${observerActionHtml(observer, mn.pubkey, fallbackName)}
          </div>
        </div>`;
      }
    }));

    // #262: an unavailable observer list is reported once for the whole
    // grid. Without it a card would silently look like a non-observer, which
    // is a false negative the operator cannot see.
    const observerNotice = (await observerIndexPromise).available ? ''
      : `<div class="my-nodes-observer-notice" role="status"><svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-warning"/></svg> ${escapeHtml(MyMeshObservers.UNAVAILABLE_NOTICE)}</div>`;
    grid.innerHTML = observerNotice + cards.join('');
    revealClippedObserverButtons(grid);

    // Wire up remove buttons
    grid.querySelectorAll('.mnc-remove').forEach(btn => {
      btn.addEventListener('click', (e) => {
        e.stopPropagation();
        removeMyNode(btn.dataset.key);
        loadMyNodes();
        // Update title if no nodes left
        const h1 = document.querySelector('.home-hero h1');
        if (h1 && !getMyNodes().length) h1.textContent = 'CoreScope';
      });
    });

    // Wire up action buttons
    grid.querySelectorAll('.mnc-btn').forEach(btn => {
      btn.addEventListener('click', (e) => {
        e.stopPropagation();
        if (btn.dataset.action === 'node') window.location.hash = '#/nodes/' + encodeURIComponent(btn.dataset.key);
        // #262: navigate with the MATCHED observer record's own id, so the
        // observer page always opens the right device. observers rows are
        // stored uppercase and nodes rows lowercase (#1836), so the node key
        // is never re-cased by hand here.
        if (btn.dataset.action === 'observer') {
          const href = MyMeshObservers.pageHref(observerRecords.get(btn.dataset.key));
          if (href) window.location.hash = href;
        }
        if (btn.dataset.action === 'health') loadHealth(btn.dataset.key);
        if (btn.dataset.action === 'packets') window.location.hash = '#/packets/' + btn.dataset.key;
        if (btn.dataset.action === 'observers') showObservers(btn, observerLists.get(btn.dataset.key) || HeardBy.lists(null));
      });
    });

    // Card click → health
    grid.querySelectorAll('.my-node-card').forEach(card => {
      const handler = (e) => {
        // #304: the evidence disclosure is part of the card, so a click on
        // its summary (or on the text it reveals) must toggle it instead of
        // navigating to the health panel.
        if (e.target.closest('.mnc-remove') || e.target.closest('.mnc-btn') || e.target.closest('.mnc-evidence')) return;
        loadHealth(card.dataset.key);
      };
      card.addEventListener('click', handler);
      card.addEventListener('keydown', (e) => {
        // #2027 follow-up: a nested action/remove button's own Enter/Space
        // activation must not be swallowed by the card's preventDefault()
        // below. Bail out before it runs, the same target check `handler`
        // already uses for the click case, so the button still fires its
        // own click via the browser's native keyboard activation.
        if (e.target.closest('.mnc-remove') || e.target.closest('.mnc-btn') || e.target.closest('.mnc-evidence')) return;
        if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); handler(e); }
      });
    });
  }

  // ==================== #262 OBSERVER IDENTITY ON A CARD ====================
  // All three helpers take the matched observer row (or null) and render
  // nothing at all for a node that has no observer record, so an ordinary
  // repeater/room card is byte-for-byte what it was before #262.

  // OBSERVER is the primary badge; the node's mesh role keeps its own
  // .mnc-role chip right beside it and is never overwritten. The title spells
  // the pair out ("Observer · Repeater") for pointer and assistive tech.
  function observerBadgeHtml(observer, nodeRole) {
    if (!observer) return '';
    const summary = MyMeshObservers.roleSummary(nodeRole);
    return `<span class="mnc-badge-observer" title="${escapeAttr(summary)}" aria-label="${escapeAttr(summary)}">${escapeHtml(MyMeshObservers.PRIMARY_BADGE)}</span>`;
  }

  // Observer-sourced status and traffic, kept in their own labelled block so
  // nothing here can be read as the node's own activity. Every value states
  // its source; a missing one renders an em dash rather than a zero.
  function observerPanelHtml(observer) {
    if (!observer) return '';
    const st = MyMeshObservers.status(observer);
    const dot = `<span class="${st.cls}" aria-hidden="true"><svg class="ph-icon"><use href="/icons/phosphor-sprite.svg#ph-circle-fill"/></svg></span>`;
    const reported = st.reportedAt ? MyMeshObservers.STATUS_REPORTED_PREFIX + timeAgo(st.reportedAt) : '';
    const lastReport = MyMeshObservers.lastReport(observer);
    const traffic = MyMeshObservers.traffic(observer);
    return `<div class="mnc-observer-panel" data-observer-id="${escapeAttr(String(observer.id || ''))}">
      <div class="mnc-section-lbl">${escapeHtml(MyMeshObservers.OBSERVER_SECTION_LABEL)}</div>
      <div class="mnc-observer-rows">
        <div class="mnc-observer-row" title="${escapeAttr(MyMeshObservers.STATUS_TIP)}"><strong>${escapeHtml(MyMeshObservers.STATUS_ROW_LABEL)}</strong> ${dot} <span class="mnc-observer-state">${escapeHtml(st.label)}</span>${escapeHtml(reported)}</div>
        <div class="mnc-observer-row"><strong>${escapeHtml(MyMeshObservers.LAST_REPORT_ROW_LABEL)}</strong> <span class="mnc-observer-last">${lastReport ? escapeHtml(timeAgo(lastReport)) : escapeHtml(MyMeshObservers.UNKNOWN_VALUE)}</span></div>
      </div>
      <div class="mnc-metric mnc-observer-metric" title="${escapeAttr(MyMeshObservers.TRAFFIC_TIP)}">
        <div class="mnc-val">${escapeHtml(traffic.value)}</div>
        <div class="mnc-lbl">${escapeHtml(traffic.label)}</div>
      </div>
    </div>`;
  }

  // The direct Observer page action. Only rendered for a matched observer;
  // the existing Node page action is untouched and always present.
  function observerActionHtml(observer, pubkey, name) {
    if (!MyMeshObservers.pageHref(observer)) return '';
    return `<button class="mnc-btn mnc-observer-page" data-action="observer" data-key="${escapeAttr(pubkey)}" aria-label="Open the observer page for ${escapeAttr(name)}">Observer page →</button>`;
  }

  // #351 R1: measure after layout. scrollWidth > clientWidth is exactly when
  // the ellipsis is drawn. All reads happen before any write, so the whole
  // grid costs one layout pass.
  function revealClippedObserverButtons(grid) {
    const clipped = [...grid.querySelectorAll('.mnc-view-all[hidden]')].filter(btn =>
      [...btn.parentElement.querySelectorAll('.mnc-observer-name')].some(el => el.scrollWidth > el.clientWidth));
    clipped.forEach(btn => { btn.hidden = false; });
  }

  // heard is HeardBy.lists(health): direct observers first, then the relay
  // ones, each marked so the list stays truthful about what was received.
  function showObservers(trigger, heard) {
    const dialog = document.getElementById('homeObserversDialog');
    if (!dialog) return;
    const cardName = trigger.closest('.my-node-card')?.querySelector('.mnc-name')?.textContent || 'Node';
    dialog.querySelector('#homeObserversTitle').textContent = `${cardName} · ${heard.direct.length} direct, ${heard.relay.length} via relay`;
    const list = dialog.querySelector('.mnc-dialog-list');
    const fragment = document.createDocumentFragment();
    heard.direct.concat(heard.relay).forEach((observer, i) => {
      const item = document.createElement('li');
      item.textContent = HeardBy.observerName(observer) + (i >= heard.direct.length ? ' (via relay)' : '');
      fragment.appendChild(item);
    });
    list.replaceChildren(fragment);
    dialog.querySelector('.mnc-dialog-close').onclick = () => dialog.close();
    dialog.onclick = (event) => {
      if (event.target !== dialog) return;
      const rect = dialog.getBoundingClientRect();
      if (event.clientX < rect.left || event.clientX > rect.right || event.clientY < rect.top || event.clientY > rect.bottom) dialog.close();
    };
    dialog.onclose = () => { if (trigger.isConnected) trigger.focus(); };
    dialog.showModal();
  }

  // #304 point 3/5: the evidence behind the badge and the two numbers that
  // need their sample scope spelled out, in a native <details> so the card
  // stays compact and the explanation is reachable by keyboard AND touch —
  // a title tooltip is neither. Timestamps are rendered separately, never
  // merged: "last own advert" is the node's own transmission, "last
  // attributed relay activity" is other traffic whose observed path named
  // this node, and it is only reported when it is newer than that advert.
  function renderMyMeshEvidence(ev, activity, stats) {
    const hops = MyMeshActivity.hops(stats);
    const trend = MyMeshActivity.compare24h(activity);
    const row = (label, value, tip) =>
      `<div class="mnc-evidence-row"${tip ? ` title="${escapeAttr(tip)}"` : ''}><strong>${escapeHtml(label)}</strong> <span>${escapeHtml(value)}</span></div>`;
    return `<details class="mnc-evidence"><summary>${escapeHtml(MyMeshActivity.EVIDENCE_SUMMARY)}</summary>` +
      row(MyMeshActivity.ADVERT_ROW_LABEL, ev.advertText || timeAgo(ev.advertAt)) +
      row(MyMeshActivity.RELAY_ROW_LABEL, ev.relayText || timeAgo(ev.relayAt)) +
      `<div class="mnc-evidence-note">${escapeHtml(ev.explanation)}</div>` +
      row(MyMeshActivity.WINDOW_ROW_LABEL, MyMeshActivity.windowText(activity), MyMeshActivity.TRAFFIC_TIP) +
      row(MyMeshActivity.COMPARE_ROW_LABEL, trend.text, trend.tip) +
      row(hops.label, hops.value, hops.tip) +
      '</details>';
  }

  function renderMyMeshAdvertIntervals(intervals, backfill) {
    const row = (label, estimate) => {
      if (!estimate) return `<div class="mnc-advert-row"><strong>${label}</strong> unavailable</div>`;
      const samples = Number(estimate.samples);
      const seconds = Number(estimate.interval_s);
      let value = 'unavailable';
      if (estimate.status === 'estimated' && estimate.interval_s != null && Number.isFinite(seconds) && seconds > 0) {
        const hours = seconds / 3600;
        value = '≈ ' + (hours >= 1 ? Number(hours.toFixed(1)) + ' h' : Math.round(seconds / 60) + ' min');
      } else if (estimate.status === 'too_few') {
        value = 'not enough adverts';
      } else if (estimate.status === 'none_observed') {
        value = 'not observed';
      } else if (estimate.status === 'irregular') {
        value = 'irregular';
      }
      const count = Number.isInteger(samples) && samples >= 0 ? ` · ${samples} heard` : '';
      const seen = estimate.last_advert && Number.isFinite(Date.parse(estimate.last_advert))
        ? ' · last ' + timeAgo(estimate.last_advert) : '';
      return `<div class="mnc-advert-row"><strong>${label}</strong> ${value}${count}${seen}</div>`;
    };
    const provisional = intervals && backfill?.status !== 'complete'
      ? '<div class="mnc-advert-provisional" title="Route-mask backfill incomplete or unknown; flood and zero-hop classifications may change">Route classes provisional</div>' : '';
    return `<div class="mnc-advert-cadence" aria-label="Observed repeater advert intervals">` +
      row('Zero-hop', intervals?.zero_hop) + row('Flood', intervals?.flood) +
      provisional + '<div class="mnc-advert-note" title="Observed estimate from at most 20 newest adverts per type; missed receptions can skew it. Not the configured timer.">Observed estimate ≠ setting · ≤20/type · missed receptions skew</div></div>';
  }

  function buildSparkline(activity) {
    const label = '<div class="home-spark-label">Node-associated transmissions · last 24h</div>';
    // #304: one shared shape check for the rolling window (MyMeshActivity),
    // so the sparkline and the 24h traffic count can never disagree about
    // whether the hourly counts are usable.
    const buckets = MyMeshActivity.buckets(activity);
    if (!buckets || activity.complete !== true) return label + '<div class="home-spark-unavailable">Activity unavailable: 24h history incomplete</div>';
    const max = Math.max(1, ...buckets.map(b => b.count));
    const bars = buckets.map(b => {
      const height = Math.max(2, Math.round((b.count / max) * 24));
      const opacity = b.count > 0 ? 0.4 + (b.count / max) * 0.6 : 0.1;
      const range = `${new Date(b.start).toLocaleString()} to ${new Date(b.end).toLocaleString()}`;
      return `<div class="home-spark-bar" role="img" aria-label="${escapeAttr(range)}: ${b.count} node-associated transmissions" title="${escapeAttr(range)}: ${b.count} node-associated transmissions" style="height:${height}px;opacity:${opacity}"></div>`;
    }).join('');
    return `${label}<div class="home-spark-bars">${bars}</div>`;
  }

  // ==================== STATS ====================
  async function loadStats() {
    try {
      const s = await api('/stats', { ttl: CLIENT_TTL.nodeSearch });
      const el = document.getElementById('homeStats');
      if (!el) return;
      el.innerHTML = `
        <div class="home-stat"><div class="val">${s.totalTransmissions ?? s.totalPackets ?? '—'}</div><div class="lbl">Transmissions</div></div>
        <div class="home-stat"><div class="val">${s.totalNodes ?? '—'}</div><div class="lbl">Nodes</div></div>
        <div class="home-stat"><div class="val">${s.totalObservers ?? '—'}</div><div class="lbl">Observers</div></div>
        <div class="home-stat"><div class="val">${s.packetsLast24h ?? '—'}</div><div class="lbl">Last 24h</div></div>
      `;
    } catch {}
  }

  // ==================== HEALTH DETAIL ====================
  async function loadHealth(pubkey) {
    const card = document.getElementById('homeHealth');
    const journey = document.getElementById('homeJourney');
    if (!card) return;
    card.innerHTML = '<p style="color:var(--text-muted);padding:12px">Loading…</p>';
    card.classList.add('visible');
    if (journey) journey.classList.remove('visible');

    try {
      const h = await api(healthPath(pubkey, isMyNode(pubkey)), { ttl: CLIENT_TTL.nodeHealth });
      const node = h.node || {};
      const stats = h.stats || {};
      const packets = h.recentPackets || [];
      const hasLocation = node.lat != null && node.lon != null;
      const heard = HeardBy.lists(h);
      const claimed = isMyNode(pubkey);

      // #304: the panel reads the same evidence model as the cards, so the
      // badge, the SNR attribution and the traffic window cannot drift apart
      // between the two views.
      const ev = MyMeshActivity.statusEvidence({ node, stats });
      const heardAt = ev.heardAt;
      const status = ev.status;
      const color = ev.confident && status === 'healthy' ? 'green' : status === 'silent' ? 'red' : 'yellow';
      const statusMsg = (heardAt ? `Last heard ${timeAgo(heardAt)}` : 'No confirmed activity in the current window') +
        (ev.confident ? '' : ' · ' + MyMeshActivity.ATTRIBUTED_SUFFIX);

      const snr = MyMeshActivity.snr(stats, heard.direct);
      const traffic = MyMeshActivity.traffic24h(h.activity24h);
      const trend = MyMeshActivity.compare24h(h.activity24h);
      const hops = MyMeshActivity.hops(stats);

      card.innerHTML = `
        <div class="health-banner ${color}">
          <span class="${ev.cls}" aria-label="${escapeAttr(ev.text)}">${ev.confident && status === 'healthy' ? '<svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-check-circle"/></svg>' : status === 'silent' ? '<svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-x-circle"/></svg>' : '<svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-warning"/></svg>'}</span>
          <span><strong>${escapeHtml(node.name || truncate(pubkey, 16))}</strong> — ${statusMsg}</span>
          ${!claimed ? `<button class="health-claim" data-key="${pubkey}" data-name="${escapeAttr(node.name || '')}">+ Add to My Mesh</button>` : ''}
        </div>
        <div class="health-body">
          <div class="health-metrics">
            <div class="health-metric" title="${escapeAttr(traffic.tip)}"><div class="val">${escapeHtml(traffic.value)}</div><div class="lbl">${escapeHtml(traffic.label)}</div><div class="mnc-metric-sub" title="${escapeAttr(trend.tip)}">${escapeHtml(trend.text)}</div></div>
            <div class="health-metric" title="${escapeAttr(HeardBy.DIRECT_TIP)}"><div class="val">${heard.direct.length}</div><div class="lbl">Direct observers</div></div>
            <div class="health-metric"><div class="val">${heardAt ? timeAgo(heardAt) : '—'}</div><div class="lbl">Last seen</div></div>
            <div class="health-metric" title="${escapeAttr(snr.tip)}"><div class="val">${escapeHtml(snr.value)}</div><div class="lbl">${escapeHtml(snr.label)}</div></div>
            <div class="health-metric" title="${escapeAttr(hops.tip)}"><div class="val">${escapeHtml(hops.value)}</div><div class="lbl">${escapeHtml(hops.label)}</div></div>
          </div>
          ${renderMyMeshEvidence(ev, h.activity24h, stats)}
          ${heard.direct.length || heard.relay.length ? `<div class="health-observers"><strong title="${escapeAttr(HeardBy.DIRECT_TIP)}">Heard directly by:</strong> ${heard.direct.length ? heard.direct.map(o => escapeHtml(HeardBy.observerName(o))).join(', ') : `<span class="text-muted" title="${escapeAttr(HeardBy.EMPTY_DIRECT_TIP)}">none proven</span>`}</div>` : ''}
          ${heard.relay.length ? `<div class="health-observers"><strong title="${escapeAttr(HeardBy.RELAY_TIP)}">Heard via relay by:</strong> ${heard.relay.map(o => escapeHtml(HeardBy.observerName(o))).join(', ')}</div>` : ''}
          ${hasLocation ? '<div class="health-map" id="healthMap"></div>' : ''}
          <div class="health-timeline">
            <h3>Recent Activity</h3>
            ${packets.length ? packets.slice(0, 10).map(p => {
              const decoded = p.decoded_json ? JSON.parse(p.decoded_json) : {};
              const obsId = p.observer_name || p.observer_id || '?';
              return `<div class="timeline-item" tabindex="0" role="button" data-pkt='${JSON.stringify({
                from: node.name || truncate(pubkey, 12),
                observers: [obsId],
                type: p.payload_type,
                time: p.timestamp || p.created_at
              }).replace(/'/g, '&#39;')}'>
                <span class="badge" style="background:var(--type-${payloadTypeColor(p.payload_type)})">${escapeHtml(payloadTypeName(p.payload_type))}</span>
                <span>via ${escapeHtml(obsId)}</span>
                <span class="time">${timeAgo(p.timestamp || p.created_at)}</span>
                <span class="snr">${p.snr != null ? Number(p.snr).toFixed(1) + ' dB' : ''}</span>
              </div>`;
            }).join('') : '<p style="color:var(--text-muted);font-size:.85rem">No recent packets found for this node.</p>'}
          </div>
        </div>
      `;

      // Claim button in health detail
      card.querySelector('.health-claim')?.addEventListener('click', (e) => {
        e.stopPropagation();
        addMyNode(pubkey, node.name);
        e.target.remove();
        loadMyNodes();
        const h1 = document.querySelector('.home-hero h1');
        if (h1) h1.textContent = 'My Mesh';
      });

      // Mini map
      if (hasLocation && typeof L !== 'undefined') {
        if (miniMap) { miniMap.remove(); miniMap = null; }
        const mapEl = document.getElementById('healthMap');
        if (mapEl) {
          miniMap = L.map(mapEl, { zoomControl: false, attributionControl: false }).setView([node.lat, node.lon], 12);
          L.tileLayer('https://{s}.tile.openstreetmap.org/{z}/{x}/{y}.png').addTo(miniMap);
          L.marker([node.lat, node.lon]).addTo(miniMap);
          setTimeout(() => miniMap.invalidateSize(), 100);
        }
      }

      // Scroll to health card
      card.scrollIntoView({ behavior: 'smooth', block: 'start' });

      // Timeline click/keyboard → journey
      card.querySelectorAll('.timeline-item').forEach(item => {
        const activate = () => { try { showJourney(JSON.parse(item.dataset.pkt)); } catch {} };
        item.addEventListener('click', activate);
        item.addEventListener('keydown', (e) => {
          if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); activate(); }
        });
      });
    } catch (e) {
      card.innerHTML = '<p style="color:var(--status-red, #ef4444);padding:12px">Failed to load node health.</p>';
    }
  }

  function showJourney(data) {
    const el = document.getElementById('homeJourney');
    if (!el) return;
    const nodes = [];
    nodes.push({ name: data.from, meta: 'Sender' });
    if (data.observers && data.observers.length) {
      data.observers.forEach(o => nodes.push({ name: o, meta: 'Observer' }));
    }
    const flow = nodes.map((n, i) => {
      const nodeHtml = `<div class="journey-node"><div class="node-name">${escapeHtml(n.name)}</div><div class="node-meta">${n.meta}</div></div>`;
      return i < nodes.length - 1 ? nodeHtml + '<div class="journey-arrow"></div>' : nodeHtml;
    }).join('');
    el.innerHTML = `<div class="journey-title">Packet Journey — ${escapeHtml(payloadTypeName(data.type))}</div><div class="journey-flow">${flow}</div>`;
    el.classList.add('visible');
  }

  // ==================== HELPERS ====================
  function escapeHtml(s) { return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;'); }
  /* #368: one shared implementation, defined and exported by public/roles.js
     (which index.html loads before this file) so the Markdown link
     destinations in miniMarkdown() and the attribute sinks here cannot drift
     apart. Behaviour is unchanged from the local copy this replaces. */
  function escapeAttr(s) { return window.escapeAttr(s); }
  function timeSinceMs(d) { return Date.now() - d.getTime(); }

  function checklist(homeCfg) {
    var html = '';
    // Render steps (getting started guide)
    if (homeCfg?.steps?.length) {
      html += homeCfg.steps.map(s => `<div class="checklist-item"><div class="checklist-q" role="button" tabindex="0" aria-expanded="false">${_renderHomeGlyph(s.emoji || '')} ${escapeHtml(s.title)}</div><div class="checklist-a">${window.miniMarkdown ? miniMarkdown(s.description) : escapeHtml(s.description)}</div></div>`).join('');
    }
    // Render FAQ/checklist (additional Q&A)
    if (homeCfg?.checklist?.length) {
      if (html) html += '<h3 style="margin:24px 0 12px;font-size:16px"><svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-question"/></svg> FAQ</h3>';
      html += homeCfg.checklist.map(i => `<div class="checklist-item"><div class="checklist-q" role="button" tabindex="0" aria-expanded="false">${escapeHtml(i.question)}</div><div class="checklist-a">${window.miniMarkdown ? miniMarkdown(i.answer) : escapeHtml(i.answer)}</div></div>`).join('');
    }
    // Fallback: Bay Area defaults when no config at all
    if (!html) {
      const items = [
        { q: '<svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-chats"/></svg> First: Join the Bay Area MeshCore Discord',
          a: '<p>The community Discord is the best place to get help and find local mesh enthusiasts.</p><p><a href="https://discord.gg/q59JzsYTst" target="_blank" rel="noopener" style="color:var(--link-color);font-weight:600">Join the Discord ↗</a></p><p>Start with <strong>#intro-to-meshcore</strong> — it has detailed setup instructions.</p>' },
        { q: '<svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-bluetooth"/></svg> Step 1: Connect via Bluetooth',
          a: '<p>Flash <strong>BLE companion</strong> firmware from <a href="https://flasher.meshcore.io/" target="_blank" rel="noopener" style="color:var(--link-color)">MeshCore Flasher</a>.</p><ul><li>Screenless devices: default PIN <code>123456</code></li><li>Screen devices: random PIN shown on display</li><li>If pairing fails: forget device, reboot, re-pair</li></ul>' },
        { q: '<svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-radio"/></svg> Step 2: Set the right frequency preset',
          a: '<p><strong>US Recommended:</strong></p><div style="margin:8px 0;padding:8px 12px;background:var(--surface-1);border-radius:6px;font-family:var(--mono);font-size:.85rem">910.525 MHz · BW 62.5 kHz · SF 7 · CR 5</div><p>Select <strong>"US Recommended"</strong> in the app or flasher.</p>' },
        { q: '<svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-broadcast"/></svg> Step 3: Advertise yourself',
          a: '<p>Tap the signal icon → <strong>Flood</strong> to broadcast your node to the mesh. Companions only advert when you trigger it manually.</p>' },
        { q: '<svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-repeat"/></svg> Step 4: Check "Heard N repeats"',
          a: '<ul><li><strong>"Sent"</strong> = transmitted, no confirmation</li><li><strong>"Heard 0 repeats"</strong> = no repeater picked it up</li><li><strong>"Heard 1+ repeats"</strong> = you\'re on the mesh!</li></ul>' },
        { q: '<svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-map-pin"/></svg> Repeaters near you?',
          a: '<p><a href="#/map" style="color:var(--link-color)">Check the network map</a> to see active repeaters.</p>' }
      ];
      html = items.map(i => `<div class="checklist-item"><div class="checklist-q" role="button" tabindex="0" aria-expanded="false">${i.q}</div><div class="checklist-a">${i.a}</div></div>`).join('');
    }
    return html;
  }

  registerPage('home', { init, destroy });
})();
