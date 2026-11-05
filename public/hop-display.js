/* === CoreScope — hop-display.js === */
/* Shared hop rendering with conflict info for all pages */
'use strict';

window.HopDisplay = (function() {
  function escapeHtml(s) {
    return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;').replace(/'/g,'&#39;');
  }

  // #598 — an inferred hop below this confidence is shown as a guess: dashed
  // underline and a badge to its candidates. #598 r2 nit 8 — read from
  // HopResolver, which computes the confidence this is compared against, so
  // there is one threshold and it cannot drift. Resolved at call time:
  // index.html loads hop-resolver.js first, but a caller that renders before
  // it exists should get no threshold rather than a stale copy of one.
  function LIKELY() {
    if (typeof window === 'undefined' || !window.HopResolver) return 0;
    return window.HopResolver.LIKELY_CONFIDENCE;
  }

  // Dismiss any open conflict popover
  function dismissPopover() {
    const old = document.querySelector('.hop-conflict-popover');
    if (old) old.remove();
  }

  // Global click handler to dismiss popovers
  let _listenerAttached = false;
  function ensureGlobalListener() {
    if (_listenerAttached) return;
    _listenerAttached = true;
    document.addEventListener('click', (e) => {
      if (!e.target.closest('.hop-conflict-popover') && !e.target.closest('.hop-conflict-btn')) {
        dismissPopover();
      }
    });
  }

  function showConflictPopover(btn, h, conflicts, globalFallback, pick) {
    dismissPopover();
    ensureGlobalListener();

    // #598 — an inferred hop (pick) says what the whole path settled on, so a
    // regional narrowing of its prefix is not what its candidates mean.
    const regional = pick ? [] : conflicts.filter(c => c.regional);
    const shown = regional.length > 0 ? regional : conflicts;

    const count = `${shown.length} candidate${shown.length > 1 ? 's' : ''}`;
    const header = pick
      ? `${escapeHtml(h)} — ${pick.name ? 'likely ' + escapeHtml(pick.name) : 'unlisted node'} (${pct(pick.confidence)}) · ${count}`
      : `${escapeHtml(h)} — ${count}${regional.length > 0 ? ' in region' : ' (global fallback)'}`;
    let html = `<div class="hop-conflict-header">${header}</div>`;
    html += '<div class="hop-conflict-list">';
    for (const c of shown) {
      const name = escapeHtml(c.name || c.pubkey?.slice(0, 16) || '?');
      // An inferred candidate carries a likelihood, or the hop the path
      // already uses it at; a resolve() candidate carries its region distance.
      const metric = pick
        ? (c.located === false ? 'no GPS' : c.usedAt != null ? 'hop ' + c.usedAt : pct(c.confidence))
        : (c.distKm != null ? c.distKm + 'km' : '');
      const dist = metric ? `<span class="hop-conflict-dist">${escapeHtml(metric)}</span>` : '';
      const pk = c.pubkey ? c.pubkey.slice(0, 12) + '…' : '';
      html += `<a href="#/nodes/${encodeURIComponent(c.pubkey || '')}" class="hop-conflict-item">
        <span class="hop-conflict-name">${name}</span>
        ${dist}
        <span class="hop-conflict-pk">${pk}</span>
      </a>`;
    }
    html += '</div>';

    const popover = document.createElement('div');
    popover.className = 'hop-conflict-popover';
    popover.innerHTML = html;
    document.body.appendChild(popover);

    // Position near the button
    const rect = btn.getBoundingClientRect();
    popover.style.top = (rect.bottom + window.scrollY + 4) + 'px';
    popover.style.left = Math.max(8, Math.min(rect.left + window.scrollX - 60, window.innerWidth - 280)) + 'px';
  }

  /**
   * The number on a hop's conflict badge; the badge is drawn when it is > 1.
   * Only regional conflicts count, or every conflict when the resolver fell
   * back to the global list. #165 — the packets list's path summary counts
   * hops with this, so the list flags exactly the hops the detail badges.
   */
  function conflictBadgeCount(entry) {
    if (!entry || typeof entry !== 'object') return 0;
    const conflicts = entry.conflicts || [];
    const regional = conflicts.filter(c => c.regional).length;
    return regional > 0 ? regional : (entry.globalFallback ? conflicts.length : 0);
  }

  function pct(confidence) {
    return Math.round((confidence || 0) * 100) + '%';
  }

  // #165 — the hash-to-name match is geographically inconsistent with the path
  // around it. Shared by renderHop() and renderInferredHop(): one glyph, one
  // wording, and the "Path symbols" legend describes exactly this button.
  const UNRELIABLE_BADGE = ' <button class="hop-unreliable-btn status-warn" aria-label="Unreliable name resolution" title="Unreliable name resolution — this hash\u2192name match is geographically inconsistent with the surrounding path hops. The repeater itself may be fine; this specific hop assignment is uncertain."><svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-warning"/></svg></button>';

  // The yellow "N candidates" button. pick is the inferred answer, or null for
  // a resolve() hop, and rides along in data-conflict for the popover.
  function conflictButton(h, conflicts, globalFallback, pick, count, label) {
    const conflictData = escapeHtml(JSON.stringify({ h, conflicts, globalFallback, pick }));
    return ` <button class="hop-conflict-btn status-warn" data-conflict='${conflictData}' onclick="event.preventDefault();event.stopPropagation();HopDisplay._showFromBtn(this)" title="${escapeHtml(label)}"><svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-warning"/></svg>${count}</button>`;
  }

  /**
   * #598 — a hop from HopResolver.inferPath(): one position in ONE
   * observation's path, with a confidence. "Unlisted" means no located node
   * sharing its prefix fits that point in the path, so the hex prefix is shown
   * rather than the nearest node that happens to share it.
   */
  // #674 — the ONE rule for "this inferred hop is uncertain", shared by the
  // packet detail (renderInferredHop) and the map route view: a guess below
  // LIKELY_CONFIDENCE with alternatives to weigh, or an unlisted hop whose
  // candidates were all rejected. A canonical hop is the ingestor's own
  // answer (#1283) and never uncertain.
  function isUnsureInferredHop(entry) {
    if (!entry || typeof entry !== 'object') return false;
    const candidates = entry.candidates || [];
    if (entry.unlisted) return !entry.name && candidates.length > 0;
    const guess = !entry.canonical && entry.confidence < LIKELY();
    return guess && candidates.length > 1;
  }

  function renderInferredHop(h, entry, opts) {
    const candidates = entry.candidates || [];
    const name = entry.name || null;
    // A guess is dashed; the badge only appears when there are candidates to
    // weigh: the alternatives to a guess, or those rejected for an unlisted hop.
    // A canonical hop is the ingestor's own answer (#1283) and neither.
    const guess = !entry.canonical && !entry.unlisted && entry.confidence < LIKELY();
    const unsure = isUnsureInferredHop(entry);
    // A node the ingestor resolved that does not fit where the hop sits. #165
    // already has a warning for that, and it is not the same thing as a node
    // with no GPS to check.
    const misplaced = !!(entry.unlisted && name && entry.unreliable);

    let title;
    if (entry.canonical) {
      title = `${h} — resolved by the ingestor`;
    } else if (!entry.unlisted) {
      title = `${h} — ${pct(entry.confidence)} likely` +
        (candidates.length > 1 ? ` · ${candidates.length} nodes share this prefix` : '');
    } else if (misplaced) {
      title = `${h} — resolved by the ingestor, but this node does not fit this point in the path`;
    } else if (name) {
      title = `${h} — the only known node with this prefix, no GPS to check its place in the path`;
    } else if (candidates.length) {
      title = `${h} — no listed node with this prefix fits this point in the path`;
    } else {
      title = `${h} — no known node has this prefix`;
    }

    const display = opts.hexMode || !name ? h
      : escapeHtml(opts.truncate ? name.slice(0, opts.truncate) : name);
    const cls = [
      'hop',
      name ? 'hop-named' : '',
      guess || unsure ? 'hop-ambiguous' : '',
      // hop-unlisted mutes the hex prefix; a misplaced hop shows a NAME, and
      // .hop-unreliable (italic, #165) is the mark that belongs to it.
      entry.unlisted && !misplaced ? 'hop-unlisted' : '',
      misplaced ? 'hop-unreliable' : '',
      // #165 — callers mark the pill itself; see renderHop() below.
      opts.className || '',
    ].filter(Boolean).join(' ');
    const label = name && entry.unlisted && !misplaced
      ? `${display} <span class="hop-nogps">(no GPS)</span>` : display;
    const pill = opts.link !== false && entry.pubkey
      ? `<a class="${cls} hop-link" href="#/nodes/${encodeURIComponent(entry.pubkey)}" title="${escapeHtml(title)}" data-hop-link="true">${label}</a>`
      : `<span class="${cls}" title="${escapeHtml(title)}">${label}</span>`;

    if (opts.badge === false) return pill;
    if (misplaced) return `<span class="hop-group">${pill}${UNRELIABLE_BADGE}</span>`;
    if (!unsure) return pill;
    const pick = { name, confidence: entry.confidence };
    const btn = conflictButton(h, candidates, false, pick, candidates.length,
      candidates.length + ' candidates — click for details');
    return `<span class="hop-group">${pill}${btn}</span>`;
  }

  /**
   * Render a hop prefix as HTML with conflict info.
   */
  function renderHop(h, entry, opts) {
    opts = opts || {};
    if (!entry) entry = {};
    if (typeof entry === 'string') entry = { name: entry };
    // #598 — only inferPath() entries carry a numeric confidence.
    if (typeof entry.confidence === 'number') return renderInferredHop(h, entry, opts);

    const name = entry.name || null;
    const pubkey = entry.pubkey || h;
    const ambiguous = entry.ambiguous || false;
    const conflicts = entry.conflicts || [];
    const globalFallback = entry.globalFallback || false;
    const unreliable = entry.unreliable || false;
    const display = opts.hexMode ? h : (name ? escapeHtml(opts.truncate ? name.slice(0, opts.truncate) : name) : h);

    // Simple title for the hop link itself
    let title = h;
    if (unreliable) title += ' — unreliable';

    const badgeCount = conflictBadgeCount(entry);
    const conflictBadge = badgeCount > 1
      ? conflictButton(h, conflicts, globalFallback, null, badgeCount,
        badgeCount + ' candidates — click for details')
      : '';
    const unreliableBadge = unreliable ? UNRELIABLE_BADGE : '';
    // #165 (port of upstream 2099) — opts.badge === false renders the name without its badge, for the
    // packets list, which summarises per path instead of per hop.
    const warnBadge = opts.badge === false ? '' : (conflictBadge + unreliableBadge);

    const cls = [
      'hop',
      name ? 'hop-named' : '',
      ambiguous ? 'hop-ambiguous' : '',
      unreliable ? 'hop-unreliable' : '',
      globalFallback ? 'hop-global-fallback' : '',
      // #165 — callers mark the pill itself (e.g. nodes.js 'hop-current').
      // Patching the first class="" of the returned HTML would hit the
      // .hop-group wrapper whenever the hop carries a badge.
      opts.className || '',
    ].filter(Boolean).join(' ');

    const pill = opts.link !== false
      ? `<a class="${cls} hop-link" href="#/nodes/${encodeURIComponent(pubkey)}" title="${escapeHtml(title)}" data-hop-link="true">${display}</a>`
      : `<span class="${cls}" title="${escapeHtml(title)}">${display}</span>`;

    // #165 (port of upstream 2099) — a badge is a sibling of its pill, so in "A [8] → B [6]" nothing
    // said which name the 8 belonged to. Wrapping the pair keeps them together
    // and puts the separator clearly outside. Only when there is a badge:
    // wrapping every hop would change the layout of every path for nothing.
    if (!warnBadge) return pill;
    return `<span class="hop-group">${pill}${warnBadge}</span>`;
  }

  /**
   * Render a full path as HTML.
   */
  function renderPath(hops, cache, opts) {
    opts = opts || {};
    const sep = opts.separator || ' → ';
    if (!hops || !hops.length) return '—';
    return hops.filter(Boolean).map(h => renderHop(h, cache[h], opts)).join(sep);
  }

  // Called from inline onclick
  function _showFromBtn(btn) {
    try {
      const data = JSON.parse(btn.dataset.conflict);
      showConflictPopover(btn, data.h, data.conflicts, data.globalFallback, data.pick || null);
    } catch (e) { console.error('Conflict popover error:', e); }
  }

  // #1504 — Path symbols legend (shared by Packets + Nodes pages).
  // Tufte: integrate words and graphics — small, on-data, dismissible.
  // PATH_SYMBOLS_LEGEND mirrors what renderHop() emits (yellow warning+N
  // button, bare warning button, dashed-underline class). Glyphs are
  // pre-rendered Phosphor sprite HTML so the legend visually matches the
  // actual hop chrome (#1648 M3).
  const WARN_PH = '<svg class="ph-icon" aria-hidden="true"><use href="/icons/phosphor-sprite.svg#ph-warning"/></svg>';
  const PATH_SYMBOLS_LEGEND = [
    { glyphHtml: '<span class="status-warn">' + WARN_PH + 'N</span>',
      description: 'Yellow button next to a hop — N regional candidates share this hop\u2019s prefix. Click for the candidate list.' },
    { glyphHtml: '<span class="status-warn">' + WARN_PH + '</span>',
      description: 'Warning icon alone (no number) — unreliable name resolution: the best-guess pubkey couldn\u2019t be confirmed against surrounding path hops.' },
    { glyphHtml: 'dashed underline',
      description: 'Ambiguous or global-fallback resolution — the name matched outside the current region, or is a guess.' },
    { glyphHtml: '<span class="hop-unlisted">5E</span>',
      description: 'Hex prefix in a packet\u2019s detail — unlisted: no known node with this prefix fits this point in the path. It is likely a node missing from the node list, or one without GPS.' },
  ];

  function renderPathSymbolsLegend() {
    const items = PATH_SYMBOLS_LEGEND.map(function(e) {
      // glyphHtml is trusted (constant strings above), description is escaped.
      return '<li><span class="path-legend-glyph">' + e.glyphHtml + '</span> — ' + escapeHtml(e.description) + '</li>';
    }).join('');
    return '<details class="path-symbols-legend"><summary>Path symbols</summary>' +
           '<ul class="path-legend-list">' + items + '</ul></details>';
  }

  return { renderHop, renderPath, conflictBadgeCount, isUnsureInferredHop, _showFromBtn, _showConflictPopover: showConflictPopover, LIKELY, PATH_SYMBOLS_LEGEND, renderPathSymbolsLegend };
})();
